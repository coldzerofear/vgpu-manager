/*
 * Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package kubeletplugin

import (
	"fmt"

	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
)

const (
	VGpuDeviceType = "vgpu"
	GpuDeviceType  = "gpu"
	// For business logic, we need distinction between `MigStaticDeviceType` and
	// `MigDynamicDeviceType`. In the API, however, these types are deliberately
	// indistinguishbable (just "mig".) That is, we should define these string
	// constants separately from the types used in code.
	MigStaticDeviceType = "mig"
	// Abstract allocatable MIG device is manged by us (DynamicMIG).
	MigDynamicDeviceType = "migdyn"
	VfioDeviceType       = "vfio"
	UnknownDeviceType    = "unknown"
)

type UUIDProvider interface {
	// Both, full GPUs and MIG devices
	UUIDs() []string
	// Only full GPUs
	GpuUUIDs() []string
	// Only MIG devices
	MigDeviceUUIDs() []string
}

func ResourceClaimToString(rc *resourcev1.ResourceClaim) string {
	return fmt.Sprintf("%s/%s:%s", rc.Namespace, rc.Name, rc.UID)
}

func PreparedClaimToString(pc *PreparedClaim, uid string) string {
	return fmt.Sprintf("%s/%s:%s", pc.Namespace, pc.Name, uid)
}

func ClaimsToStrings(claims []*resourcev1.ResourceClaim) []string {
	var results []string
	for _, c := range claims {
		results = append(results, ResourceClaimToString(c))
	}
	return results
}

func ClaimRefsToStrings(claimRefs []kubeletplugin.NamespacedObject) []string {
	var results []string
	for _, r := range claimRefs {
		results = append(results, r.String())
	}
	return results
}
