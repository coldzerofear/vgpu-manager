/*
Copyright 2026 coldzerofear

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package remoteagent

import (
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/client-go/tools/cache"
)

// trimClaim is the claim informer's transform: ResourceClaims have no field
// selector for "allocates a device of pool X", so the agent must watch every
// claim in the cluster. The watch itself cannot be narrowed; the cache can.
// Only what EnsureSession / GC read survives:
//
//   - identity (name, namespace, UID, resourceVersion) and deletionTimestamp;
//   - spec request names (to fold "<request>/<subrequest>" back to the main
//     request), with their selectors/config/capacity dropped;
//   - allocation results of this driver on this pool; other results are
//     dropped, an empty allocation stays non-nil so "allocated" is preserved.
//
// Claims that never touch this node shrink to a few hundred bytes.
func trimClaim(driverName, poolName string) cache.TransformFunc {
	return func(obj interface{}) (interface{}, error) {
		claim, ok := obj.(*resourceapi.ResourceClaim)
		if !ok {
			// DeletedFinalStateUnknown etc. pass through.
			return obj, nil
		}
		trimmed := &resourceapi.ResourceClaim{
			TypeMeta: claim.TypeMeta,
		}
		trimmed.Name = claim.Name
		trimmed.Namespace = claim.Namespace
		trimmed.UID = claim.UID
		trimmed.ResourceVersion = claim.ResourceVersion
		trimmed.DeletionTimestamp = claim.DeletionTimestamp

		for _, req := range claim.Spec.Devices.Requests {
			r := resourceapi.DeviceRequest{Name: req.Name}
			if req.Exactly != nil {
				r.Exactly = &resourceapi.ExactDeviceRequest{}
			}
			for _, sub := range req.FirstAvailable {
				r.FirstAvailable = append(r.FirstAvailable, resourceapi.DeviceSubRequest{Name: sub.Name})
			}
			trimmed.Spec.Devices.Requests = append(trimmed.Spec.Devices.Requests, r)
		}

		if claim.Status.Allocation != nil {
			alloc := &resourceapi.AllocationResult{}
			for _, result := range claim.Status.Allocation.Devices.Results {
				if result.Driver != driverName || result.Pool != poolName {
					continue
				}
				alloc.Devices.Results = append(alloc.Devices.Results, resourceapi.DeviceRequestAllocationResult{
					Request:          result.Request,
					Driver:           result.Driver,
					Pool:             result.Pool,
					Device:           result.Device,
					ShareID:          result.ShareID,
					ConsumedCapacity: result.ConsumedCapacity,
				})
			}
			trimmed.Status.Allocation = alloc
		}
		return trimmed, nil
	}
}
