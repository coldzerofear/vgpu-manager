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

// Sessions owned by a ResourceClaim (DRA path): the node snapshot comes from
// this node's ResourceSlices and the quota from the claim's allocation.

import (
	"fmt"

	"github.com/Masterminds/semver"
	vgpuconfig "github.com/coldzerofear/vgpu-manager/pkg/config/vgpu"
	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin/remote"
	"github.com/coldzerofear/vgpu-manager/pkg/metrics/collector"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
)

// NodeRemoteDevicesFromSlices builds the snapshot from this node's slices.
// Only accessMode=remote devices that carry a uuid and a minor are kept:
// the minor is the host device index, which is also the session config slot
// (library config_allowed_devices treats slot index as host index). The map
// is keyed by device name because allocation results reference devices by
// name.
func NodeRemoteDevicesFromSlices(slices []*resourceapi.ResourceSlice) *NodeDevices {
	nd := &NodeDevices{Devices: map[string]NodeDevice{}}
	for _, slice := range slices {
		for _, dev := range slice.Spec.Devices {
			mode := remote.StringAttr(&dev, remote.AttrAccessMode)
			if mode != remote.AccessModeRemote {
				continue
			}
			uuid := collector.DeviceUUIDFromAttribute(remote.StringAttr(&dev, remote.AttrUUID))
			if uuid == "" {
				continue
			}
			minor := remote.IntAttr(&dev, remote.AttrMinor)
			if minor < 0 || minor >= vgpuconfig.MaxDeviceCount {
				continue
			}
			d := NodeDevice{Name: dev.Name, Minor: minor, UUID: uuid, MemoryRatio: util.HundredCore}
			if ratio := remote.IntAttr(&dev, remote.AttrMemoryRatio); ratio >= 0 {
				d.MemoryRatio = ratio
			}
			if q, ok := dev.Capacity[remote.CapacityCores]; ok {
				d.Cores = q.Value.Value()
			}
			if q, ok := dev.Capacity[remote.CapacityMemory]; ok {
				d.MemoryMiB = q.Value.Value() >> 20
			}
			nd.Devices[d.Name] = d

			if nd.CudaVersion == nil {
				if version := remote.VersionAttr(&dev, remote.AttrCUDADriverVersion); version != "" {
					if v, err := semver.NewVersion(version); err == nil {
						nd.CudaVersion = v
					}
				}
			}
			if nd.DriverVersion == nil {
				if version := remote.VersionAttr(&dev, remote.AttrDriverVersion); version != "" {
					if v, err := semver.NewVersion(version); err == nil {
						nd.DriverVersion = v
					}
				}
			}
		}
	}
	return nd
}

// claimRV is the claim's resourceVersion as the sweep compares it.
func claimRV(claim *resourceapi.ResourceClaim) int64 {
	return objectRV(claim.ResourceVersion)
}

// MaterializeClaim writes the session for the partition of `claim` named by
// `requests` (main request names; empty = every request) on this node's pool.
// When several results of the partition land on the same physical device (the
// webhook normally prevents this), the largest share wins — the config has one
// slot per device.
func (s *SessionStore) MaterializeClaim(token string, claim *resourceapi.ResourceClaim, nd *NodeDevices, requests []string) error {
	spec, err := s.claimSessionSpec(token, claim, nd, requests)
	if err != nil {
		return err
	}
	policy := vgpuconfig.GetDefaultComputePolicy(claim, s.nodeObject())
	return s.Materialize(token, spec, nd, policy)
}

func (s *SessionStore) claimSessionSpec(token string, claim *resourceapi.ResourceClaim, nd *NodeDevices, requests []string) (SessionSpec, error) {
	if claim.Status.Allocation == nil {
		return SessionSpec{}, fmt.Errorf("claim %s has no allocation", klog.KObj(claim))
	}
	poolName := s.cfg.NodeName
	memoryRatio := int64(util.HundredCore)
	results := remote.FilterResultsByRequests(claim, claim.Status.Allocation.Devices.Results, requests)
	infoBySlot := map[int]device.DeviceClaim{}
	claimBySlot := map[int]device.DeviceClaim{}
	for _, result := range results {
		if result.Driver != s.cfg.DriverName || result.Pool != poolName {
			continue
		}
		dev, ok := nd.Devices[result.Device]
		if !ok {
			return SessionSpec{}, fmt.Errorf("allocated device %s is not published by this node (pool %s)", result.Device, poolName)
		}
		if memoryRatio == util.HundredCore && dev.MemoryRatio != memoryRatio {
			memoryRatio = dev.MemoryRatio
		}
		// Slot = host device index (minor), exactly as the local path lays
		// out the config: the library reads slot index as host index
		// (config_allowed_devices) and translates to the container-visible
		// ordinal itself.
		slot := int(dev.Minor)
		infoBySlot[slot] = device.DeviceClaim{Id: slot, Uuid: dev.UUID, Cores: dev.Cores, Memory: dev.MemoryMiB}

		cores, memoryMiB := dev.Cores, dev.MemoryMiB
		if q, ok := result.ConsumedCapacity[remote.CapacityCores]; ok {
			cores = q.Value()
		}
		if q, ok := result.ConsumedCapacity[remote.CapacityMemory]; ok {
			memoryMiB = q.Value() >> 20
		}
		if prev, dup := claimBySlot[slot]; dup {
			klog.Warningf("session %s: device %s allocated more than once in one partition; taking the larger share", token, result.Device)
			cores = max(cores, prev.Cores)
			memoryMiB = max(memoryMiB, prev.Memory)
		}
		claimBySlot[slot] = device.DeviceClaim{Id: slot, Uuid: dev.UUID, Cores: cores, Memory: memoryMiB}
	}

	spec := SessionSpec{
		Owner: SessionOwner{
			Kind: OwnerClaim, UID: string(claim.UID), Namespace: claim.Namespace,
			Name: claim.Name, Version: claimRV(claim),
		},
		MemoryRatio: float64(memoryRatio) / float64(util.HundredCore),
	}
	for _, slot := range sets.List(sets.KeySet(claimBySlot)) {
		spec.Infos = append(spec.Infos, infoBySlot[slot])
		spec.Claims = append(spec.Claims, claimBySlot[slot])
	}
	return spec, nil
}
