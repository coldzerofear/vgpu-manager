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
	"testing"

	nvdev "github.com/NVIDIA/go-nvlib/pkg/nvlib/device"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/coldzerofear/vgpu-manager/pkg/device/nvidia"
	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	resourcev1 "k8s.io/client-go/listers/resource/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"
)

const (
	testParentUUID = "GPU-1d2e3f40-0000-0000-0000-000000000001"
	testMigUUID    = "MIG-5a6b7c80-0000-0000-0000-000000000001"
)

type collected struct {
	desc   *prometheus.Desc
	value  float64
	labels map[string]string
}

func drain(t *testing.T, fn func(ch chan<- prometheus.Metric)) []collected {
	t.Helper()
	ch := make(chan prometheus.Metric, 64)
	fn(ch)
	close(ch)
	var out []collected
	for m := range ch {
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))
		labels := map[string]string{}
		for _, l := range pb.GetLabel() {
			labels[l.GetName()] = l.GetValue()
		}
		out = append(out, collected{desc: m.Desc(), value: pb.GetGauge().GetValue(), labels: labels})
	}
	return out
}

func find(ms []collected, desc *prometheus.Desc) []collected {
	var out []collected
	for _, m := range ms {
		if m.desc == desc {
			out = append(out, m)
		}
	}
	return out
}

// sliceMig is a MIG device as the kubelet plugin publishes it: a dynamic one
// carries no uuid, a static one does.
func sliceMig(name, migUUID string, taints ...v1.DeviceTaint) v1.Device {
	attrs := map[v1.QualifiedName]v1.DeviceAttribute{
		"type":       {StringValue: ptr.To(kubeletplugin.MigStaticDeviceType)},
		"parentUUID": {StringValue: ptr.To("gpu-1d2e3f40-0000-0000-0000-000000000001")},
		"profile":    {StringValue: ptr.To("1g.10gb")},
	}
	if migUUID != "" {
		attrs["uuid"] = v1.DeviceAttribute{StringValue: ptr.To("mig-5a6b7c80-0000-0000-0000-000000000001")}
	}
	return v1.Device{Name: name, Attributes: attrs, Taints: taints}
}

// liveMig is a MIG device as NVML reports it: GPU instance profile ID 19,
// placement start 2.
func liveMig() map[string][]*nvidia.MigInfo {
	parent := &nvidia.GpuInfo{UUID: testParentUUID}
	return map[string][]*nvidia.MigInfo{testParentUUID: {{
		Index:         3,
		UUID:          testMigUUID,
		Profile:       "1g.10gb",
		Parent:        parent,
		Memory:        nvml.Memory{Total: 10 << 30, Used: 3 << 30},
		Placement:     &nvidia.MigDevicePlacement{GpuInstancePlacement: nvml.GpuInstancePlacement{Start: 2, Size: 1}},
		GiProfileInfo: &nvml.GpuInstanceProfileInfo{Id: 19, MemorySizeMB: 10240, MultiprocessorCount: 16},
		GiInfo:        &nvml.GpuInstanceInfo{Id: 9, ProfileId: 19, Placement: nvml.GpuInstancePlacement{Start: 2, Size: 1}},
		CiProfileInfo: &nvml.ComputeInstanceProfileInfo{Id: 0},
		CiInfo:        &nvml.ComputeInstanceInfo{Id: 0},
	}}}
}

func TestNewDRAMigDevice(t *testing.T) {
	dyn := sliceMig("gpu-0-mig-1g10gb-19-2", "")
	dev, ok := newDRAMigDevice(&dyn)
	require.True(t, ok)
	require.Equal(t, migSpecKey{parentUUID: testParentUUID, profileID: 19, placementStart: 2}, dev.spec)
	require.Empty(t, dev.uuid)
	require.True(t, dev.healthy)

	// A profile name with a dash must not shift the trailing numbers.
	me := sliceMig("gpu-0-mig-1g10gb-me-20-4", "")
	dev, ok = newDRAMigDevice(&me)
	require.True(t, ok)
	require.Equal(t, 20, dev.spec.profileID)
	require.Equal(t, 4, dev.spec.placementStart)

	static := sliceMig("gpu-0-mig-1g10gb-19-2", testMigUUID,
		v1.DeviceTaint{Key: kubeletplugin.TaintKeyXID, Effect: v1.DeviceTaintEffectNoSchedule})
	dev, ok = newDRAMigDevice(&static)
	require.True(t, ok)
	require.Equal(t, testMigUUID, dev.uuid)
	require.False(t, dev.healthy)

	bad := sliceMig("not-a-mig-name", "")
	_, ok = newDRAMigDevice(&bad)
	require.False(t, ok)
}

// The slice device here comes from the kubelet plugin's own publishing code,
// so a change in how it names or labels dynamic MIG devices breaks this test.
func TestDynamicMigDevicePublishedByPluginIsRecognised(t *testing.T) {
	parent := &kubeletplugin.GpuDeviceInfo{GpuInfo: &nvidia.GpuInfo{
		UUID: testParentUUID, Minor: 0, ProductName: "NVIDIA H100", Brand: "NVIDIA", Architecture: "Hopper",
		CudaComputeCapability: "9.0",
		DriverVersion:         nvidia.DriverVersion{DriverVersion: "580.0.0", CudaDriverVersion: 13000},
	}}
	allocatable := &kubeletplugin.AllocatableDevice{MigDynamic: &kubeletplugin.MigSpec{
		Parent:            parent,
		CandidateProfiles: []nvdev.MigProfile{&nvdev.MigProfileInfo{C: 1, G: 1, GB: 10, GIProfileID: 0}},
		GIProfileInfo:     nvml.GpuInstanceProfileInfo{Id: 19, MemorySizeMB: 10240, MultiprocessorCount: 16},
		Placement:         nvml.GpuInstancePlacement{Start: 2, Size: 1},
	}}
	require.Equal(t, kubeletplugin.MigDynamicDeviceType, allocatable.Type(), "internal type differs from the published one")
	allocatable.AddOrUpdateTaint(&v1.DeviceTaint{Key: kubeletplugin.TaintKeyXID, Value: "31", Effect: v1.DeviceTaintEffectNoSchedule})
	published := allocatable.PartGetDevice(&kubeletplugin.Config{})

	require.Equal(t, migDeviceTypeAttr, *published.Attributes["type"].StringValue)
	require.Equal(t, "gpu-0-mig-1g10gb-19-2", published.Name)
	dev, ok := newDRAMigDevice(&published)
	require.True(t, ok)
	require.Empty(t, dev.uuid, "a dynamic device has no uuid before it is created")
	require.False(t, dev.healthy)

	mig := newLiveMigIndex(liveMig()).resolve(dev)
	require.NotNil(t, mig, "the published device matches the instance created for it")
	require.Equal(t, testMigUUID, mig.UUID)
}

func TestLiveMigIndexResolve(t *testing.T) {
	index := newLiveMigIndex(liveMig())

	for _, tc := range []struct {
		name  string
		dev   v1.Device
		found bool
	}{
		{"dynamic, created", sliceMig("gpu-0-mig-1g10gb-19-2", ""), true},
		{"dynamic, other placement not created", sliceMig("gpu-0-mig-1g10gb-19-3", ""), false},
		{"dynamic, other profile at same start", sliceMig("gpu-0-mig-2g20gb-14-2", ""), false},
		{"static, by uuid", sliceMig("gpu-0-mig-1g10gb-19-2", testMigUUID), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dev, ok := newDRAMigDevice(&tc.dev)
			require.True(t, ok)
			require.Equal(t, tc.found, index.resolve(dev) != nil)
		})
	}
}

func TestDRAMigDeviceMetricsFollowLiveDevices(t *testing.T) {
	healthy := map[string]int{testParentUUID: 1}

	// Published but not created: nothing to report.
	ms := drain(t, func(ch chan<- prometheus.Metric) {
		collectDRAMigDeviceMetrics(ch, "node-1", map[string][]*nvidia.MigInfo{}, healthy, nil)
	})
	require.Empty(t, ms)

	// Created: all three device metrics appear.
	dyn := sliceMig("gpu-0-mig-1g10gb-19-2", "")
	dev, _ := newDRAMigDevice(&dyn)
	live := liveMig()
	health := migHealthByUUID(map[string]*draMigDevice{dev.name: dev}, newLiveMigIndex(live))
	ms = drain(t, func(ch chan<- prometheus.Metric) {
		collectDRAMigDeviceMetrics(ch, "node-1", live, healthy, health)
	})
	require.Len(t, ms, 3)
	mem := find(ms, migDeviceGiProfileMemory)
	require.Len(t, mem, 1)
	require.Equal(t, float64(10<<30), mem[0].value)
	require.Equal(t, map[string]string{
		"node": "node-1", "mig_idx": "3", "mig_uuid": testMigUUID, "device_uuid": testParentUUID,
		"ci_id": "0", "gi_id": "9", "ci_profile_id": "0", "gi_profile_id": "19", "profile": "1g.10gb",
	}, mem[0].labels)
	require.Equal(t, float64(16), find(ms, migDeviceGiProfileSMCount)[0].value)
	require.Equal(t, float64(1), find(ms, migDeviceHealthStatus)[0].value)

	// A taint on the MIG device makes it unhealthy.
	tainted := sliceMig("gpu-0-mig-1g10gb-19-2", "",
		v1.DeviceTaint{Key: kubeletplugin.TaintKeyXID, Value: "31", Effect: v1.DeviceTaintEffectNoSchedule})
	dev, _ = newDRAMigDevice(&tainted)
	health = migHealthByUUID(map[string]*draMigDevice{dev.name: dev}, newLiveMigIndex(live))
	ms = drain(t, func(ch chan<- prometheus.Metric) {
		collectDRAMigDeviceMetrics(ch, "node-1", live, healthy, health)
	})
	require.Equal(t, float64(0), find(ms, migDeviceHealthStatus)[0].value)

	// So does an unhealthy parent, even with no slice device behind the MIG device.
	ms = drain(t, func(ch chan<- prometheus.Metric) {
		collectDRAMigDeviceMetrics(ch, "node-1", live, map[string]int{testParentUUID: 0}, nil)
	})
	require.Equal(t, float64(0), find(ms, migDeviceHealthStatus)[0].value)

	// Destroyed: the series are gone.
	ms = drain(t, func(ch chan<- prometheus.Metric) {
		collectDRAMigDeviceMetrics(ch, "node-1", map[string][]*nvidia.MigInfo{}, healthy, health)
	})
	require.Empty(t, ms)
}

func TestDRAContainerMigMetrics(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "p"}}
	dyn := sliceMig("gpu-0-mig-1g10gb-19-2", "")
	dev, _ := newDRAMigDevice(&dyn)
	devices := map[string]*draMigDevice{dev.name: dev}
	index := newLiveMigIndex(liveMig())
	procs := map[string]procInfoList{testMigUUID: {
		100: {Pid: 100, UsedGpuMemory: 1 << 30},
		200: {Pid: 200, UsedGpuMemory: 2 << 30},
	}}

	// Usage is the container's own processes on the device.
	ms := drain(t, func(ch chan<- prometheus.Metric) {
		collectDRAContainerMigMetrics(ch, "node-1", pod, "c0", []string{dev.name, dev.name}, devices, index, procs, []uint32{100})
	})
	require.Len(t, ms, 3, "a device reached twice is reported once")
	usage := find(ms, containerMIGMemoryUsage)[0]
	require.Equal(t, float64(1<<30), usage.value)
	require.Equal(t, map[string]string{
		"node": "node-1", "mig_idx": "3", "mig_uuid": testMigUUID, "device_uuid": testParentUUID,
		"pod_namespace": "ns", "pod_name": "p", "container_name": "c0",
	}, usage.labels)
	require.Equal(t, float64(10<<30), find(ms, containerMIGMemoryLimit)[0].value)
	require.Equal(t, float64(10), find(ms, containerMIGMemoryUtilRate)[0].value)

	// Without a per-instance process list, the whole device is reported.
	ms = drain(t, func(ch chan<- prometheus.Metric) {
		collectDRAContainerMigMetrics(ch, "node-1", pod, "c0", []string{dev.name}, devices, index, nil, nil)
	})
	require.Equal(t, float64(3<<30), find(ms, containerMIGMemoryUsage)[0].value)

	// Allocated but not created yet, or destroyed: nothing.
	ms = drain(t, func(ch chan<- prometheus.Metric) {
		collectDRAContainerMigMetrics(ch, "node-1", pod, "c0", []string{dev.name}, devices, newLiveMigIndex(nil), procs, nil)
	})
	require.Empty(t, ms)
}

func TestResolvePodAllocationsCollectsMigDevices(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "p", UID: types.UID("pod-uid")},
		Spec: corev1.PodSpec{
			NodeName: "node-1",
			Containers: []corev1.Container{
				{Name: "c0", Resources: corev1.ResourceRequirements{Claims: []corev1.ResourceClaim{{Name: "gpus"}}}},
				{Name: "c1"},
			},
			ResourceClaims: []corev1.PodResourceClaim{{Name: "gpus", ResourceClaimName: ptr.To("claim")}},
		},
	}
	claim := &v1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "claim"},
		Spec: v1.ResourceClaimSpec{Devices: v1.DeviceClaim{Requests: []v1.DeviceRequest{
			{Name: "mig", Exactly: &v1.ExactDeviceRequest{DeviceClassName: "mig-manager"}},
			{Name: "vgpu", Exactly: &v1.ExactDeviceRequest{DeviceClassName: "vgpu-manager"}},
		}}},
		Status: v1.ResourceClaimStatus{
			Allocation: &v1.AllocationResult{Devices: v1.DeviceAllocationResult{Results: []v1.DeviceRequestAllocationResult{
				{Request: "mig", Driver: util.DRADriverName, Pool: "node-1", Device: "gpu-0-mig-1g10gb-19-2"},
				{Request: "vgpu", Driver: util.DRADriverName, Pool: "node-1", Device: "vgpu-1"},
			}}},
			ReservedFor: []v1.ResourceClaimConsumerReference{{Resource: "pods", Name: "p", UID: pod.UID}},
		},
	}
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	require.NoError(t, indexer.Add(claim))
	c := draGPUCollector{claimLister: resourcev1.NewResourceClaimLister(indexer)}

	dyn := sliceMig("gpu-0-mig-1g10gb-19-2", "")
	dev, _ := newDRAMigDevice(&dyn)
	vgpu := &DRADeviceInfo{name: "vgpu-1", uuid: "GPU-other", cores: 100, memory: 1 << 30}

	allocs := c.resolvePodAllocations(pod, map[string]*DRADeviceInfo{"vgpu-1": vgpu}, map[string]*draMigDevice{dev.name: dev})
	require.Len(t, allocs, 2)
	require.Equal(t, "c0", allocs[0].name)
	require.Equal(t, []string{"gpu-0-mig-1g10gb-19-2"}, allocs[0].migDevices)
	require.Len(t, allocs[0].results, 1, "vGPU results are unaffected")
	require.Equal(t, "vgpu-1", allocs[0].results[0].device)
	require.Empty(t, allocs[1].migDevices, "a container without the claim holds nothing")
}
