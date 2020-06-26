/*
Copyright 2020 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package endpointslicemirroring

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1beta1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/controller/endpointslicemirroring/metrics"
	endpointutil "k8s.io/kubernetes/pkg/controller/util/endpoint"
)

// reconciler is responsible for transforming current EndpointSlice state into
// desired state
type reconciler struct {
	client                clientset.Interface
	maxEndpointsPerSubset int32
	endpointSliceTracker  *endpointSliceTracker
	metricsCache          *metrics.Cache
	eventRecorder         record.EventRecorder
}

// reconcile takes an Endpoints resource and ensures that corresponding
// EndpointSlices exist. It creates, updates, or deletes EndpointSlices to
// ensure the desired set of addresses are represented by EndpointSlices.
func (r *reconciler) reconcile(endpoints *corev1.Endpoints, existingSlices []*discovery.EndpointSlice) error {
	// Calculate desired state.
	d := newDesiredCalc()

	for _, subset := range endpoints.Subsets {
		multiKey := d.initPorts(subset.Ports)

		totalAddresses := 0
		numInvalidAddresses := 0

		for _, address := range subset.Addresses {
			totalAddresses++
			if totalAddresses > int(r.maxEndpointsPerSubset) {
				break
			}
			if ok := d.addAddress(address, multiKey, true); !ok {
				numInvalidAddresses++
				klog.Warningf("Address in %s/%s Endpoints is not a valid IP, it will not be mirrored to an EndpointSlice: %s", endpoints.Namespace, endpoints.Name, address.IP)
			}
		}

		for _, address := range subset.NotReadyAddresses {
			totalAddresses++
			if totalAddresses > int(r.maxEndpointsPerSubset) {
				break
			}
			if ok := d.addAddress(address, multiKey, false); !ok {
				numInvalidAddresses++
				klog.Warningf("Address in %s/%s Endpoints is not a valid IP, it will not be mirrored to an EndpointSlice: %s", endpoints.Namespace, endpoints.Name, address.IP)
			}
		}

		if numInvalidAddresses > 0 {
			r.eventRecorder.Eventf(endpoints, corev1.EventTypeWarning, InvalidIPAddress,
				"Skipped %d invalid IP addresses when mirroring to EndpointSlices", numInvalidAddresses)
		}
	}

	// Build data structures for existing state.
	existingSlicesByKey := endpointSlicesByKey(existingSlices)

	// Determine changes necessary for each group of slices by port map.
	epMetrics := metrics.NewEndpointPortCache()
	totals := totalsByAction{}
	slices := slicesByAction{}

	for portKey, desiredEndpoints := range d.endpointsByKey {
		numEndpoints := len(desiredEndpoints)
		pmSlices, pmTotals := r.reconcileByPortMapping(
			endpoints, existingSlicesByKey[portKey], desiredEndpoints, d.portsByKey[portKey], portKey.addressType())

		slices.append(pmSlices)
		totals.add(pmTotals)

		epMetrics.Set(endpointutil.PortMapKey(portKey), metrics.EfficiencyInfo{
			Endpoints: numEndpoints,
			Slices:    len(existingSlicesByKey[portKey]) + len(pmSlices.toCreate) - len(pmSlices.toDelete),
		})
	}

	// If there are unique sets of ports that are no longer desired, mark
	// the corresponding endpoint slices for deletion.
	for portKey, existingSlices := range existingSlicesByKey {
		if _, ok := d.endpointsByKey[portKey]; !ok {
			for _, existingSlice := range existingSlices {
				slices.toDelete = append(slices.toDelete, existingSlice)
			}
		}
	}

	metrics.EndpointsAddedPerSync.WithLabelValues().Observe(float64(totals.added))
	metrics.EndpointsUpdatedPerSync.WithLabelValues().Observe(float64(totals.updated))
	metrics.EndpointsRemovedPerSync.WithLabelValues().Observe(float64(totals.removed))

	endpointsNN := types.NamespacedName{Name: endpoints.Name, Namespace: endpoints.Namespace}
	r.metricsCache.UpdateEndpointPortCache(endpointsNN, epMetrics)

	return r.finalize(endpoints, slices)
}

// reconcileByPortMapping compares the endpoints found in existing slices with
// the list of desired endpoints and returns lists of slices to create, update,
// and delete.
func (r *reconciler) reconcileByPortMapping(
	endpoints *corev1.Endpoints,
	existingSlices []*discovery.EndpointSlice,
	desiredSet endpointSet,
	endpointPorts []discovery.EndpointPort,
	addressType discovery.AddressType,
) (slicesByAction, totalsByAction) {
	slices := slicesByAction{}
	totals := totalsByAction{}

	// If no endpoints are desired, mark existing slices for deletion and
	// return.
	if desiredSet.Len() == 0 {
		slices.toDelete = existingSlices
		for _, epSlice := range existingSlices {
			totals.removed += len(epSlice.Endpoints)
		}
		return slices, totals
	}

	if len(existingSlices) == 0 {
		// if no existing slices, all desired endpoints will be added.
		totals.added = desiredSet.Len()
	} else {
		// if >0 existing slices, mark all but 1 for deletion.
		slices.toDelete = existingSlices[1:]

		// Return early if first slice matches desired endpoints.
		totals = totalChanges(existingSlices[0], desiredSet)
		if totals.added == 0 && totals.updated == 0 && totals.removed == 0 {
			return slices, totals
		}
	}

	// generate a new slice with the desired endpoints.
	var sliceName string
	if len(existingSlices) > 0 {
		sliceName = existingSlices[0].Name
	}
	newSlice := newEndpointSlice(endpoints, endpointPorts, addressType, sliceName)
	for desiredSet.Len() > 0 && len(newSlice.Endpoints) < int(r.maxEndpointsPerSubset) {
		endpoint, _ := desiredSet.PopAny()
		newSlice.Endpoints = append(newSlice.Endpoints, *endpoint)
	}

	if newSlice.Name != "" {
		slices.toUpdate = []*discovery.EndpointSlice{newSlice}
	} else { // Slices to be created set GenerateName instead of Name.
		slices.toCreate = []*discovery.EndpointSlice{newSlice}
	}

	return slices, totals
}

// finalize creates, updates, and deletes slices as specified
func (r *reconciler) finalize(endpoints *corev1.Endpoints, slices slicesByAction) error {
	// If there are slices to create and delete, recycle the slices marked for
	// deletion by replacing creates with updates of slices that would otherwise
	// be deleted.
	recycleSlices(&slices)

	var errs []error
	epsClient := r.client.DiscoveryV1beta1().EndpointSlices(endpoints.Namespace)

	// Don't create more EndpointSlices if corresponding Endpoints resource is
	// being deleted.
	if endpoints.DeletionTimestamp == nil {
		for _, endpointSlice := range slices.toCreate {
			createdSlice, err := epsClient.Create(context.TODO(), endpointSlice, metav1.CreateOptions{})
			if err != nil {
				// If the namespace is terminating, creates will continue to fail. Simply drop the item.
				if errors.HasStatusCause(err, corev1.NamespaceTerminatingCause) {
					return nil
				}
				errs = append(errs, fmt.Errorf("Error creating EndpointSlice for Endpoints %s/%s: %v", endpoints.Namespace, endpoints.Name, err))
			} else {
				r.endpointSliceTracker.update(createdSlice)
				metrics.EndpointSliceChanges.WithLabelValues("create").Inc()
			}
		}
	}

	for _, endpointSlice := range slices.toUpdate {
		updatedSlice, err := epsClient.Update(context.TODO(), endpointSlice, metav1.UpdateOptions{})
		if err != nil {
			errs = append(errs, fmt.Errorf("Error updating %s EndpointSlice for Endpoints %s/%s: %v", endpointSlice.Name, endpoints.Namespace, endpoints.Name, err))
		} else {
			r.endpointSliceTracker.update(updatedSlice)
			metrics.EndpointSliceChanges.WithLabelValues("update").Inc()
		}
	}

	for _, endpointSlice := range slices.toDelete {
		err := epsClient.Delete(context.TODO(), endpointSlice.Name, metav1.DeleteOptions{})
		if err != nil {
			errs = append(errs, fmt.Errorf("Error deleting %s EndpointSlice for Endpoints %s/%s: %v", endpointSlice.Name, endpoints.Namespace, endpoints.Name, err))
		} else {
			r.endpointSliceTracker.delete(endpointSlice)
			metrics.EndpointSliceChanges.WithLabelValues("delete").Inc()
		}
	}

	return utilerrors.NewAggregate(errs)
}

// deleteEndpoints deletes any associated EndpointSlices and cleans up any
// Endpoints references from the metricsCache.
func (r *reconciler) deleteEndpoints(namespace, name string, endpointSlices []*discovery.EndpointSlice) error {
	r.metricsCache.DeleteEndpoints(types.NamespacedName{Namespace: namespace, Name: name})
	var errs []error
	for _, endpointSlice := range endpointSlices {
		err := r.client.DiscoveryV1beta1().EndpointSlices(namespace).Delete(context.TODO(), endpointSlice.Name, metav1.DeleteOptions{})
		if err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("Error(s) deleting %d/%d EndpointSlices for %s/%s Endpoints, including: %s", len(errs), len(endpointSlices), namespace, name, errs[0])
	}
	return nil
}

// endpointSlicesByKey returns a map that groups EndpointSlices by unique
// addrTypePortMapKey values.
func endpointSlicesByKey(existingSlices []*discovery.EndpointSlice) map[addrTypePortMapKey][]*discovery.EndpointSlice {
	slicesByKey := map[addrTypePortMapKey][]*discovery.EndpointSlice{}
	for _, existingSlice := range existingSlices {
		epKey := newAddrTypePortMapKey(existingSlice.Ports, existingSlice.AddressType)
		slicesByKey[epKey] = append(slicesByKey[epKey], existingSlice)
	}
	return slicesByKey
}

// totalChanges returns the total changes that will be required for an
// EndpointSlice to match a desired set of endpoints.
func totalChanges(existingSlice *discovery.EndpointSlice, desiredSet endpointSet) totalsByAction {
	totals := totalsByAction{}
	existingMatches := 0

	for _, endpoint := range existingSlice.Endpoints {
		got := desiredSet.Get(&endpoint)
		if got == nil {
			// If not desired, increment number of endpoints to be deleted.
			totals.removed++
		} else {
			existingMatches++

			// If existing version of endpoint doesn't match desired version
			// increment number of endpoints to be updated.
			if !endpointsEqualBeyondHash(got, &endpoint) {
				totals.updated++
			}
		}
	}

	// Any desired endpoints that have not been found in the existing slice will
	// be added.
	totals.added = desiredSet.Len() - existingMatches
	return totals
}
