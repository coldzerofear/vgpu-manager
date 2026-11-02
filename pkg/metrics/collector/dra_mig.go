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

package collector

import (
	"fmt"
	"sort"
	"strconv"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/coldzerofear/vgpu-manager/pkg/device/nvidia"
	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin"
	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin/remote"
	"github.com/docker/go-units"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
)

// MIG metrics on the DRA path.
//
// The metric set is driven by the MIG devices NVML reports as alive, so a
// dynamically created device appears on the first scrape after creation and
// its series vanish once it is destroyed. The ResourceSlice only adds health
// and the claim allocations only add ownership; a published but not yet
// created dynamic MIG device has no metrics.
//
// A slice device is joined to a live MIG device by:
//   - static MIG: its uuid attribute;
//   - dynamic MIG: parent UUID plus the GPU instance profile ID and placement
//     start encoded in its canonical name. These are the values the kubelet
//     plugin creates the GPU instance with and matches live devices against
//     (FindMigDevBySpec), so they identify the instance. A dynamic device has
//     no uuid attribute because the UUID only exists after creation.

// migDeviceTypeAttr is the published type attribute of every MIG device. The
// kubelet plugin tells static and dynamic MIG apart internally (Type() returns
// "migdyn" for the latter) but publishes both as "mig", so a dynamic device is
// recognised by its missing uuid attribute rather than by its type.
const migDeviceTypeAttr = kubeletplugin.MigStaticDeviceType

// draMigDevice is a MIG device published in this node's ResourceSlices.
type draMigDevice struct {
	name string
	// uuid is set for static MIG devices only.
	uuid string
	spec migSpecKey
	// healthy is false when the device carries a NoSchedule taint.
	healthy bool
}

// migSpecKey identifies a GPU instance on its parent GPU.
type migSpecKey struct {
	parentUUID     string
	profileID      int
	placementStart int
}

// newDRAMigDevice reads a ResourceSlice device of type mig. It returns false
// for a device that cannot be joined to NVML.
func newDRAMigDevice(dev *v1.Device) (*draMigDevice, bool) {
	tuple, err := kubeletplugin.NewMigSpecTupleFromCanonicalName(dev.Name)
	if err != nil {
		klog.V(4).InfoS("skip MIG device with unexpected name", "device", dev.Name, "err", err)
		return nil, false
	}
	parentUUID := DeviceUUIDFromAttribute(remote.StringAttr(dev, "parentUUID"))
	if parentUUID == "" {
		klog.V(4).InfoS("skip MIG device without parentUUID attribute", "device", dev.Name)
		return nil, false
	}
	return &draMigDevice{
		name: dev.Name,
		uuid: DeviceUUIDFromAttribute(remote.StringAttr(dev, remote.AttrUUID)),
		spec: migSpecKey{
			parentUUID:     parentUUID,
			profileID:      tuple.ProfileID,
			placementStart: tuple.PlacementStart,
		},
		healthy: kubeletplugin.IsHealthy(dev.Taints),
	}, true
}

// liveMigIndex looks up the live MIG devices NVML reported in this scrape.
type liveMigIndex struct {
	byUUID map[string]*nvidia.MigInfo
	bySpec map[migSpecKey]*nvidia.MigInfo
}

func newLiveMigIndex(devMigInfosMap map[string][]*nvidia.MigInfo) liveMigIndex {
	index := liveMigIndex{
		byUUID: make(map[string]*nvidia.MigInfo),
		bySpec: make(map[migSpecKey]*nvidia.MigInfo),
	}
	FlattenMigInfosMapEach(devMigInfosMap, func(parentUUID string, mig *nvidia.MigInfo) {
		index.byUUID[mig.UUID] = mig
		if mig.GiInfo == nil || mig.Placement == nil {
			return
		}
		index.bySpec[migSpecKey{
			parentUUID:     parentUUID,
			profileID:      int(mig.GiInfo.ProfileId),
			placementStart: int(mig.Placement.Start),
		}] = mig
	})
	return index
}

// resolve returns the live MIG device behind a slice device, or nil when it
// does not exist: not yet created, already destroyed, or created elsewhere.
func (i liveMigIndex) resolve(dev *draMigDevice) *nvidia.MigInfo {
	if dev.uuid != "" {
		return i.byUUID[dev.uuid]
	}
	return i.bySpec[dev.spec]
}

// migHealthByUUID maps each live MIG device to the health of the slice device
// it was published as. Live devices absent from the slice are left out.
func migHealthByUUID(migDevices map[string]*draMigDevice, index liveMigIndex) map[string]bool {
	health := make(map[string]bool, len(migDevices))
	for _, dev := range migDevices {
		if mig := index.resolve(dev); mig != nil {
			health[mig.UUID] = dev.healthy
		}
	}
	return health
}

func migDeviceLabels(nodeName, parentUUID string, mig *nvidia.MigInfo) []string {
	var ciID, giID, ciProfileID, giProfileID string
	if mig.CiInfo != nil {
		ciID = strconv.Itoa(int(mig.CiInfo.Id))
	}
	if mig.GiInfo != nil {
		giID = strconv.Itoa(int(mig.GiInfo.Id))
	}
	if mig.CiProfileInfo != nil {
		ciProfileID = strconv.Itoa(int(mig.CiProfileInfo.Id))
	}
	if mig.GiProfileInfo != nil {
		giProfileID = strconv.Itoa(int(mig.GiProfileInfo.Id))
	}
	return []string{nodeName, strconv.Itoa(mig.Index), mig.UUID, parentUUID, ciID, giID, ciProfileID, giProfileID, mig.Profile}
}

// collectDRAMigDeviceMetrics reports every live MIG device. A device is healthy
// when its parent is and, if published, the MIG device itself is untainted. A
// whole-GPU fault taints every MIG device on the card, so both sides agree; the
// MIG taint adds faults scoped to one instance.
func collectDRAMigDeviceMetrics(
	ch chan<- prometheus.Metric, nodeName string,
	devMigInfosMap map[string][]*nvidia.MigInfo,
	devHealthMap map[string]int, migHealth map[string]bool,
) {
	FlattenMigInfosMapEach(devMigInfosMap, func(parentUUID string, mig *nvidia.MigInfo) {
		labels := migDeviceLabels(nodeName, parentUUID, mig)
		if mig.GiProfileInfo != nil {
			ch <- prometheus.MustNewConstMetric(migDeviceGiProfileMemory, prometheus.GaugeValue,
				float64(mig.GiProfileInfo.MemorySizeMB)*float64(units.MiB), labels...)
			ch <- prometheus.MustNewConstMetric(migDeviceGiProfileSMCount, prometheus.GaugeValue,
				float64(mig.GiProfileInfo.MultiprocessorCount), labels...)
		}
		status := 0.0
		if healthy, published := migHealth[mig.UUID]; devHealthMap[parentUUID] > 0 && (!published || healthy) {
			status = 1
		}
		ch <- prometheus.MustNewConstMetric(migDeviceHealthStatus, prometheus.GaugeValue, status, labels...)
	})
}

// collectDRAContainerMigMetrics reports the MIG devices one container holds.
//
// Usage is the memory of the container's own processes on the MIG device, so
// that containers sharing one device each see their own part. When the
// per-instance process list could not be read, it falls back to the usage of
// the whole device.
func collectDRAContainerMigMetrics(
	ch chan<- prometheus.Metric, nodeName string, pod *corev1.Pod, container string,
	migNames []string, migDevices map[string]*draMigDevice, index liveMigIndex,
	migProcInfoMap map[string]procInfoList, containerPids []uint32,
) {
	names := append([]string(nil), migNames...)
	sort.Strings(names)
	reported := sets.New[string]()
	for _, name := range names {
		dev, ok := migDevices[name]
		if !ok {
			continue
		}
		mig := index.resolve(dev)
		if mig == nil || reported.Has(mig.UUID) {
			continue
		}
		reported.Insert(mig.UUID)
		parentUUID := ""
		if mig.Parent != nil {
			parentUUID = mig.Parent.UUID
		}

		usage := mig.Memory.Used
		if procs, ok := migProcInfoMap[mig.UUID]; ok {
			usage = 0
			ContainerDeviceProcInfoEach(procs, containerPids, func(process nvml.ProcessInfo_v1) {
				usage += process.UsedGpuMemory
			})
		}
		limit := mig.Memory.Total
		utilRate := int64(0)
		if limit > 0 {
			utilRate = min(int64(float64(usage)/float64(limit)*100), 100)
		}

		labels := []string{nodeName, strconv.Itoa(mig.Index), mig.UUID, parentUUID, pod.Namespace, pod.Name, container}
		ch <- prometheus.MustNewConstMetric(containerMIGMemoryLimit, prometheus.GaugeValue, float64(limit), labels...)
		ch <- prometheus.MustNewConstMetric(containerMIGMemoryUsage, prometheus.GaugeValue, float64(usage), labels...)
		ch <- prometheus.MustNewConstMetric(containerMIGMemoryUtilRate, prometheus.GaugeValue, float64(utilRate), labels...)
	}
}

// collectMigProcesses reads the processes running on each live MIG device. It
// needs the MIG device handle: the parent handle only returns aggregate
// information.
func collectMigProcesses(lib *nvidia.DeviceLib, migInfos map[string]*nvidia.MigInfo, migProcInfoMap map[string]procInfoList) {
	for uuid := range migInfos {
		hdev, ret := lib.DeviceGetHandleByUUID(uuid)
		if ret != nvml.SUCCESS {
			klog.V(4).ErrorS(fmt.Errorf("%s", nvml.ErrorString(ret)), "get MIG device handle failed", "migUUID", uuid)
			continue
		}
		procs := make(procInfoList)
		// A process with both a graphics and a compute context is listed twice
		// with the same per-process total; keying by PID counts it once.
		if list, ret := hdev.GetGraphicsRunningProcesses(); ret == nvml.SUCCESS {
			for _, p := range list {
				procs[p.Pid] = nvml.ProcessInfo_v1{Pid: p.Pid, UsedGpuMemory: p.UsedGpuMemory}
			}
		}
		list, ret := hdev.GetComputeRunningProcesses()
		if ret != nvml.SUCCESS {
			klog.V(4).ErrorS(fmt.Errorf("%s", nvml.ErrorString(ret)), "get MIG device processes failed", "migUUID", uuid)
			continue
		}
		for _, p := range list {
			procs[p.Pid] = nvml.ProcessInfo_v1{Pid: p.Pid, UsedGpuMemory: p.UsedGpuMemory}
		}
		migProcInfoMap[uuid] = procs
	}
}
