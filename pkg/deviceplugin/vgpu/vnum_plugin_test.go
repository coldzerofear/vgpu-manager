package vgpu

import (
	"strings"
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

// makeThreeContainerScheduledPod returns a pod with 3 containers each
// pre-allocated by the scheduler to a distinct GPU UUID. realAlloc starts
// empty (no container has been consumed yet).
//
// Returns: pod, container names in pod.Spec order, scheduler-assigned UUIDs
// in the same order as containers.
func makeThreeContainerScheduledPod(t *testing.T) (*corev1.Pod, []string, []string) {
	t.Helper()

	contNames := []string{"cont-a", "cont-b", "cont-c"}
	uuids := []string{
		"GPU-" + uuid.New().String(),
		"GPU-" + uuid.New().String(),
		"GPU-" + uuid.New().String(),
	}

	preAlloc := device.PodDeviceClaim{
		{Name: contNames[0], DeviceClaims: []device.DeviceClaim{
			{Id: 0, Uuid: uuids[0], Memory: 1024},
		}},
		{Name: contNames[1], DeviceClaims: []device.DeviceClaim{
			{Id: 0, Uuid: uuids[1], Memory: 2048},
		}},
		{Name: contNames[2], DeviceClaims: []device.DeviceClaim{
			{Id: 0, Uuid: uuids[2], Memory: 4096},
		}},
	}
	preAllocText, err := preAlloc.MarshalText()
	require.NoError(t, err)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			UID:       "test-uid",
			Annotations: map[string]string{
				util.PodVGPUPreAllocAnnotation: preAllocText,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: contNames[0]},
				{Name: contNames[1]},
				{Name: contNames[2]},
			},
		},
	}
	return pod, contNames, uuids
}

// availableDeviceIDsForUUIDs builds the set of "<uuid>::<slot>" identifiers
// kubelet supplies as AvailableDeviceIDs. One slot per UUID is sufficient
// for these tests — each container only requests 1 device.
func availableDeviceIDsForUUIDs(uuids []string) []string {
	ids := make([]string, len(uuids))
	for i, u := range uuids {
		ids[i] = u + "::0"
	}
	return ids
}

// Test_PreferredAllocation_BatchPattern exercises the protocol-level batch
// case: a single GetPreferredAllocation RPC carries 3 ContainerRequests.
// Verifies the in-memory advance inside buildPreAllocContext correctly
// returns claim A, B, C for requests [0], [1], [2] and that
// buildPreferredAllocationResponsesFromClaims emits 3 responses each
// pointing at the matching container's pre-allocated UUID.
//
// kubelet 1.x never sends batch (always exactly 1 ContainerRequest per
// RPC — see pkg/kubelet/cm/devicemanager/endpoint.go) but the device
// plugin protocol allows multi-container batches and our implementation
// must remain correct in that hypothetical case.
func Test_PreferredAllocation_BatchPattern(t *testing.T) {
	pod, contNames, uuids := makeThreeContainerScheduledPod(t)
	available := availableDeviceIDsForUUIDs(uuids)

	requests := []*pluginapi.ContainerPreferredAllocationRequest{
		{AvailableDeviceIDs: available, AllocationSize: 1},
		{AvailableDeviceIDs: available, AllocationSize: 1},
		{AvailableDeviceIDs: available, AllocationSize: 1},
	}

	// Mirror buildPreAllocContext's loop without going through
	// m.getCurrentPod (which has a DeviceManager dependency we do not
	// want to mock here). The semantic under test IS the loop body's
	// in-memory advance: for each request we walk
	//   GetCurrentPreAllocateContainerDevice -> Update...DeviceClaim
	// and the second iteration must skip the container the first
	// iteration consumed.
	claims := make([]*device.ContainerDeviceClaim, len(requests))
	availableMap := make([]map[string][]string, len(requests))
	for i := range requests {
		claim, err := device.GetCurrentPreAllocateContainerDevice(pod)
		require.NoError(t, err, "batch step %d: get current claim", i)
		require.Equal(t, contNames[i], claim.Name,
			"batch step %d: expected next-unconsumed container %q, got %q",
			i, contNames[i], claim.Name)

		require.NoError(t,
			device.UpdatePodRealContainerDeviceClaim(pod, *claim),
			"batch step %d: advance realAlloc", i)

		claims[i] = claim
		availableMap[i] = buildAvailableDeviceMap(requests[i].GetAvailableDeviceIDs())
	}

	preCtx := &preAllocContext{
		pod:          pod,
		claims:       claims,
		availableMap: availableMap,
	}

	resps, err := buildPreferredAllocationResponsesFromClaims(requests, preCtx)
	require.NoError(t, err)
	require.Len(t, resps, 3)

	// Each response's device IDs must point at the matching container's
	// pre-allocated UUID — no cross-container leakage.
	for i := range resps {
		wantPrefix := uuids[i] + "::"
		require.Len(t, resps[i].DeviceIDs, 1, "step %d: response should have 1 device id", i)
		require.True(t,
			strings.HasPrefix(resps[i].DeviceIDs[0], wantPrefix),
			"step %d: expected device id starting with %q, got %q",
			i, wantPrefix, resps[i].DeviceIDs[0])
	}

	// realAlloc should now hold all 3 containers (in-memory only — no
	// PatchPodAllocationSucceed runs in this code path; the protocol
	// caller would persist after the RPC if needed).
	realText, ok := util.HasAnnotation(pod, util.PodVGPURealAllocAnnotation)
	require.True(t, ok)
	var realAlloc device.PodDeviceClaim
	require.NoError(t, realAlloc.UnmarshalText(realText))
	require.Len(t, realAlloc, 3)
	require.Equal(t, contNames[0], realAlloc[0].Name)
	require.Equal(t, contNames[1], realAlloc[1].Name)
	require.Equal(t, contNames[2], realAlloc[2].Name)
}

// Test_PreferredAllocation_SerialKubeletPattern simulates kubelet 1.x's
// actual call sequence: 3 separate (GetPreferredAllocation + Allocate)
// RPC pairs, one per container, strictly serial. Each RPC reads the pod
// fresh from the apiserver; only Allocate's PatchPodAllocationSucceed
// persists realAlloc back, so the next pair sees the advance.
//
// Verified invariants per RPC pair i:
//  1. GetPreferredAllocation returns the claim for contNames[i]
//     (the i-th unconsumed container in apiserver state).
//  2. Allocate, even though it re-reads the pod fresh, returns the SAME
//     claim — both RPCs in the pair must agree on which container they
//     are processing.
//  3. The in-memory mutation made by GetPreferredAllocation does NOT
//     leak to the next RPC pair (no patch happens between
//     GetPreferredAllocation and Allocate). Only Allocate's
//     PatchPodAllocationSucceed advances persistent state.
//
// After 3 RPC pairs, the persisted realAlloc contains all 3 containers
// in pod.Spec order.
func Test_PreferredAllocation_SerialKubeletPattern(t *testing.T) {
	pod, contNames, _ := makeThreeContainerScheduledPod(t)

	for i := 0; i < 3; i++ {
		// ---- RPC i.A: GetPreferredAllocation ----
		// kubelet re-fetches the pod each RPC. Mirror that by working
		// on a clone — any in-memory mutations are discarded after the
		// RPC returns, since GetPreferredAllocation does not patch.
		gpaPod := pod.DeepCopy()
		gpaClaim, err := device.GetCurrentPreAllocateContainerDevice(gpaPod)
		require.NoError(t, err, "RPC %d GetPreferredAllocation: get current claim", i)
		require.Equal(t, contNames[i], gpaClaim.Name,
			"RPC %d GetPreferredAllocation: expected claim for %q, got %q",
			i, contNames[i], gpaClaim.Name)

		// Exercise the in-memory advance (dead in single-container case
		// since gpaPod is discarded, but the code path is exercised so a
		// future regression that makes it depend on persistence is
		// caught — see buildPreAllocContext loop body).
		require.NoError(t,
			device.UpdatePodRealContainerDeviceClaim(gpaPod, *gpaClaim),
			"RPC %d GetPreferredAllocation: in-memory advance", i)
		// gpaPod goes out of scope here — no patch back.

		// ---- RPC i.B: Allocate ----
		// kubelet re-fetches again. allocPod sees the same persisted
		// state as gpaPod did (the GetPreferredAllocation in-memory
		// mutation did not leak).
		allocPod := pod.DeepCopy()
		allocClaim, err := device.GetCurrentPreAllocateContainerDevice(allocPod)
		require.NoError(t, err, "RPC %d Allocate: get current claim", i)
		require.Equal(t, contNames[i], allocClaim.Name,
			"RPC %d Allocate: expected SAME container %q as GetPreferredAllocation, got %q",
			i, contNames[i], allocClaim.Name)
		require.Equal(t, gpaClaim.DeviceClaims, allocClaim.DeviceClaims,
			"RPC %d: GetPreferredAllocation and Allocate must agree on the same DeviceClaims", i)

		require.NoError(t,
			device.UpdatePodRealContainerDeviceClaim(allocPod, *allocClaim),
			"RPC %d Allocate: in-memory advance", i)

		// Simulate PatchPodAllocationSucceed: persist allocPod's
		// realAlloc back to "apiserver" (= our long-lived `pod` object).
		// In production this round-trips through kubeClient.Patch and
		// the informer cache; here we copy the annotation directly.
		pod.Annotations[util.PodVGPURealAllocAnnotation] =
			allocPod.Annotations[util.PodVGPURealAllocAnnotation]
	}

	// After 3 RPC pairs, persisted realAlloc must hold all 3 containers
	// in pod.Spec order.
	realText, ok := util.HasAnnotation(pod, util.PodVGPURealAllocAnnotation)
	require.True(t, ok)
	var realAlloc device.PodDeviceClaim
	require.NoError(t, realAlloc.UnmarshalText(realText))
	require.Len(t, realAlloc, 3, "all 3 containers must be in realAlloc after 3 RPC pairs")
	for i, cname := range contNames {
		require.Equal(t, cname, realAlloc[i].Name,
			"realAlloc[%d]: expected %q, got %q", i, cname, realAlloc[i].Name)
	}
}

// Test_PreferredAllocation_SerialPattern_NoLeakBetweenPairs nails down a
// specific invariant from the serial test: if kubelet ever (e.g. due to
// a retry) re-issues GetPreferredAllocation for the SAME container
// before Allocate has run, both calls must return the same claim. The
// in-memory mutation from the first GetPreferredAllocation must not be
// observable in the second.
func Test_PreferredAllocation_SerialPattern_NoLeakBetweenPairs(t *testing.T) {
	pod, contNames, _ := makeThreeContainerScheduledPod(t)

	// First GetPreferredAllocation: fresh pod, returns container A.
	clone1 := pod.DeepCopy()
	claim1, err := device.GetCurrentPreAllocateContainerDevice(clone1)
	require.NoError(t, err)
	require.Equal(t, contNames[0], claim1.Name)
	require.NoError(t, device.UpdatePodRealContainerDeviceClaim(clone1, *claim1))
	// clone1 discarded — kubelet didn't see Allocate yet.

	// Second GetPreferredAllocation (e.g. retry): MUST still return A.
	// If the in-memory mutation in clone1 had leaked to the persistent
	// pod, this call would return B and the eventual Allocate would
	// produce a misalignment between pre-allocation response and
	// actual allocation.
	clone2 := pod.DeepCopy()
	claim2, err := device.GetCurrentPreAllocateContainerDevice(clone2)
	require.NoError(t, err)
	require.Equal(t, contNames[0], claim2.Name,
		"retry of GetPreferredAllocation before Allocate must return SAME claim — "+
			"in-memory mutation from prior call must not persist")
	require.Equal(t, claim1.DeviceClaims, claim2.DeviceClaims)
}
