package allocator

import (
	"fmt"
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// vgpuContainer builds a container requesting the given vGPU number/cores/memory.
func vgpuContainer(name string, number, cores, memory int64) corev1.Container {
	limits := corev1.ResourceList{
		corev1.ResourceName(util.VGPUNumberResourceName): resource.MustParse(fmt.Sprint(number)),
	}
	if cores > 0 {
		limits[corev1.ResourceName(util.VGPUCoreResourceName)] = resource.MustParse(fmt.Sprint(cores))
	}
	if memory > 0 {
		limits[corev1.ResourceName(util.VGPUMemoryResourceName)] = resource.MustParse(fmt.Sprint(memory))
	}
	return corev1.Container{Name: name, Resources: corev1.ResourceRequirements{Limits: limits}}
}

func sidecarVGPUContainer(name string, number, cores, memory int64) corev1.Container {
	c := vgpuContainer(name, number, cores, memory)
	always := corev1.ContainerRestartPolicyAlways
	c.RestartPolicy = &always
	return c
}

// fakeNode builds a single-NUMA node with `count` identical fake GPUs.
// numa=-1 keeps NUMA topology off so allocation stays deterministic.
func fakeNode(count int, slots int, cores, memory int64) (*device.NodeInfo, []string) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "test-node"}}
	devs := make([]*device.Device, count)
	uuids := make([]string, count)
	for i := 0; i < count; i++ {
		uuids[i] = fmt.Sprintf("GPU-%d", i)
		devs[i] = device.NewFakeDeviceWithUUID(uuids[i], i, 0, slots, 0, cores, 0, memory, -1)
	}
	return device.NewFakeNodeInfo(node, false, devs...), uuids
}

func runAllocate(t *testing.T, nodeInfo *device.NodeInfo, pod *corev1.Pod) device.PodDeviceClaim {
	t.Helper()
	req := BuildAllocationRequest(pod)
	newPod, rsn, err := NewAllocator(nodeInfo, nil).Allocate(req)
	require.NoError(t, err)
	require.Nil(t, rsn, "allocation unexpectedly rejected: %v", rsn)
	pre, ok := util.HasAnnotation(newPod, util.PodVGPUPreAllocAnnotation)
	require.True(t, ok, "pre-allocated annotation missing")
	claim := device.PodDeviceClaim{}
	require.NoError(t, claim.UnmarshalText(pre))
	return claim
}

// assertNoOvercommit verifies the reduced per-GPU footprint (the actual node
// reservation) never exceeds device capacity — the core safety invariant.
func assertNoOvercommit(t *testing.T, pod *corev1.Pod, claim device.PodDeviceClaim, slots int, cores, memory int64) {
	t.Helper()
	for uuid, fp := range device.ReducePodFootprint(pod, claim) {
		assert.LessOrEqualf(t, fp.Number, slots, "GPU %s number footprint overcommits", uuid)
		assert.LessOrEqualf(t, fp.Cores, cores, "GPU %s cores footprint overcommits", uuid)
		assert.LessOrEqualf(t, fp.Memory, memory, "GPU %s memory footprint overcommits", uuid)
	}
}

// containerUUIDs returns the set of GPU UUIDs a container's claim landed on.
func containerUUIDs(claim device.PodDeviceClaim, name string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, cc := range claim {
		if cc.Name == name {
			for _, dc := range cc.DeviceClaims {
				out[dc.Uuid] = struct{}{}
			}
		}
	}
	return out
}

func Test_Allocate_InitReusesAppCard(t *testing.T) {
	// 1 card, 100 cores / 24GB, 10 slots. init bigger than app on every
	// dimension but fits the freed card → must reuse the same physical GPU.
	nodeInfo, uuids := fakeNode(1, 10, 100, 24000)
	pod := &corev1.Pod{Spec: corev1.PodSpec{
		InitContainers: []corev1.Container{vgpuContainer("init", 1, 80, 8000)},
		Containers:     []corev1.Container{vgpuContainer("app", 1, 40, 4000)},
	}}
	claim := runAllocate(t, nodeInfo, pod)

	// Reuse: init and app on the SAME card.
	assert.Equal(t, map[string]struct{}{uuids[0]: {}}, containerUUIDs(claim, "init"))
	assert.Equal(t, map[string]struct{}{uuids[0]: {}}, containerUUIDs(claim, "app"))
	// Footprint = per-dim max.
	fp := device.ReducePodFootprint(pod, claim)
	assert.Equal(t, device.PodDeviceFootprint{Id: 0, Uuid: uuids[0], Number: 1, Cores: 80, Memory: 8000}, fp[uuids[0]])
	assertNoOvercommit(t, pod, claim, 10, 100, 24000)
}

func Test_Allocate_MultipleAppsCollectivelyCoverInit(t *testing.T) {
	// The exact case raised in review: init needs 80c/20GB on one card; two
	// app containers (40c/10GB each) both land on the single card. Neither app
	// alone covers init, but the freed card does → init reuses it.
	nodeInfo, uuids := fakeNode(1, 10, 100, 24000)
	pod := &corev1.Pod{Spec: corev1.PodSpec{
		InitContainers: []corev1.Container{vgpuContainer("init", 1, 80, 20000)},
		Containers: []corev1.Container{
			vgpuContainer("app0", 1, 40, 10000),
			vgpuContainer("app1", 1, 40, 10000),
		},
	}}
	claim := runAllocate(t, nodeInfo, pod)

	assert.Equal(t, map[string]struct{}{uuids[0]: {}}, containerUUIDs(claim, "init"))
	fp := device.ReducePodFootprint(pod, claim)
	// app phase: 2 slots / 80c / 20GB; init phase: 1 slot / 80c / 20GB.
	assert.Equal(t, device.PodDeviceFootprint{Id: 0, Uuid: uuids[0], Number: 2, Cores: 80, Memory: 20000}, fp[uuids[0]])
	assertNoOvercommit(t, pod, claim, 10, 100, 24000)
}

func Test_Allocate_InitNeedsMoreCardsThanApp(t *testing.T) {
	// init wants 2 cards, app uses 1 → init can't fit entirely on app's card,
	// falls back to the full node and reserves a second card. Still correct.
	nodeInfo, _ := fakeNode(2, 10, 100, 24000)
	pod := &corev1.Pod{Spec: corev1.PodSpec{
		InitContainers: []corev1.Container{vgpuContainer("init", 2, 50, 8000)},
		Containers:     []corev1.Container{vgpuContainer("app", 1, 40, 4000)},
	}}
	claim := runAllocate(t, nodeInfo, pod)

	assert.Len(t, containerUUIDs(claim, "init"), 2, "init must occupy 2 cards")
	assert.Len(t, containerUUIDs(claim, "app"), 1)
	assertNoOvercommit(t, pod, claim, 10, 100, 24000)
}

func Test_Allocate_InitOnlyPod(t *testing.T) {
	nodeInfo, uuids := fakeNode(1, 10, 100, 24000)
	pod := &corev1.Pod{Spec: corev1.PodSpec{
		InitContainers: []corev1.Container{vgpuContainer("init", 1, 60, 6000)},
	}}
	claim := runAllocate(t, nodeInfo, pod)
	fp := device.ReducePodFootprint(pod, claim)
	assert.Equal(t, device.PodDeviceFootprint{Id: 0, Uuid: uuids[0], Number: 1, Cores: 60, Memory: 6000}, fp[uuids[0]])
	assertNoOvercommit(t, pod, claim, 10, 100, 24000)
}

func Test_Allocate_SidecarSumsWithApp(t *testing.T) {
	// Sidecar runs concurrently with app → their claims SUM on the shared card
	// (no max). No sequential init, so this exercises the fast path too.
	nodeInfo, uuids := fakeNode(1, 10, 100, 24000)
	pod := &corev1.Pod{Spec: corev1.PodSpec{
		InitContainers: []corev1.Container{sidecarVGPUContainer("side", 1, 20, 2000)},
		Containers:     []corev1.Container{vgpuContainer("app", 1, 40, 4000)},
	}}
	claim := runAllocate(t, nodeInfo, pod)
	fp := device.ReducePodFootprint(pod, claim)
	assert.Equal(t, device.PodDeviceFootprint{Id: 0, Uuid: uuids[0], Number: 2, Cores: 60, Memory: 6000}, fp[uuids[0]])
	assertNoOvercommit(t, pod, claim, 10, 100, 24000)
}

func Test_Allocate_AppOnlyUnchanged(t *testing.T) {
	// Regression: a pod with no init containers must behave exactly as before
	// (single-pass accumulation; footprint == plain sum).
	nodeInfo, uuids := fakeNode(1, 10, 100, 24000)
	pod := &corev1.Pod{Spec: corev1.PodSpec{
		Containers: []corev1.Container{
			vgpuContainer("app0", 1, 30, 3000),
			vgpuContainer("app1", 1, 25, 2000),
		},
	}}
	claim := runAllocate(t, nodeInfo, pod)
	fp := device.ReducePodFootprint(pod, claim)
	assert.Equal(t, device.PodDeviceFootprint{Id: 0, Uuid: uuids[0], Number: 2, Cores: 55, Memory: 5000}, fp[uuids[0]])
	assertNoOvercommit(t, pod, claim, 10, 100, 24000)
}

func Test_Allocate_SidecarAndSequentialInit(t *testing.T) {
	// Most complex path: a sidecar (concurrent throughout) + a sequential init
	// + an app container, all able to share one card. Validates the full
	// reserve = sidecarSum + max(regularSum, initMax) formula end-to-end.
	nodeInfo, uuids := fakeNode(1, 10, 100, 24000)
	pod := &corev1.Pod{Spec: corev1.PodSpec{
		InitContainers: []corev1.Container{
			sidecarVGPUContainer("side", 1, 20, 2000),
			vgpuContainer("init", 1, 50, 5000),
		},
		Containers: []corev1.Container{vgpuContainer("app", 1, 40, 4000)},
	}}
	claim := runAllocate(t, nodeInfo, pod)

	// All three reuse the single card.
	assert.Equal(t, map[string]struct{}{uuids[0]: {}}, containerUUIDs(claim, "init"))
	fp := device.ReducePodFootprint(pod, claim)
	// app phase = sidecar(20/2000)+app(40/4000); init phase = sidecar+init(50/5000).
	// number = 1(sidecar) + max(1 app, 1 init) = 2
	// cores  = 20 + max(40, 50) = 70 ; memory = 2000 + max(4000, 5000) = 7000
	assert.Equal(t, device.PodDeviceFootprint{Id: 0, Uuid: uuids[0], Number: 2, Cores: 70, Memory: 7000}, fp[uuids[0]])
	assertNoOvercommit(t, pod, claim, 10, 100, 24000)
}

func Test_Allocate_TwoSequentialInitsReuseSameCard(t *testing.T) {
	// Two sequential (non-restartable) init containers never overlap, so the
	// allocator must reuse ONE physical card for both (footprint = per-dim max),
	// leaving the second card free — packing GPU utilization tighter. They are
	// allocated against the same app-released view, so the deterministic device
	// sort lands both on the same card.
	nodeInfo, uuids := fakeNode(2, 10, 100, 24000)
	pod := &corev1.Pod{Spec: corev1.PodSpec{
		InitContainers: []corev1.Container{
			vgpuContainer("init1", 1, 50, 5000),
			vgpuContainer("init2", 1, 60, 6000),
		},
	}}
	claim := runAllocate(t, nodeInfo, pod)

	// Both inits on the same single card; the other card stays unused.
	assert.Equal(t, containerUUIDs(claim, "init1"), containerUUIDs(claim, "init2"))
	fp := device.ReducePodFootprint(pod, claim)
	assert.Len(t, fp, 1, "two sequential inits must reuse one card, not span two")
	for _, f := range fp { // the single shared card
		assert.Equal(t, device.PodDeviceFootprint{Id: f.Id, Uuid: f.Uuid, Number: 1, Cores: 60, Memory: 6000}, f)
	}
	_ = uuids
	assertNoOvercommit(t, pod, claim, 10, 100, 24000)
}
