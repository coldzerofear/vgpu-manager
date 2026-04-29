package claimresolve

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestResolveClaimVGPUPartitions_SingleContainerMultipleRequests(t *testing.T) {
	ctx := context.Background()
	claim := newAllocatedClaim("claim-a", []string{"req-a", "req-b"})
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "pod-a", UID: types.UID("pod-a-uid")},
		Spec: corev1.PodSpec{
			ResourceClaims: []corev1.PodResourceClaim{{Name: "gpu", ResourceClaimName: strPtr("claim-a")}},
			Containers: []corev1.Container{{
				Name:      "app",
				Resources: corev1.ResourceRequirements{Claims: []corev1.ResourceClaim{{Name: "gpu"}}},
			}},
		},
	}

	reader := &fakePartitionReader{pods: map[client.ObjectKey]*corev1.Pod{{Namespace: "default", Name: "pod-a"}: pod}}
	info, err := ResolveClaimVGPUPartitions(ctx, reader, claim, "test-driver", alwaysVGPURequest, alwaysVGPUSubRequest)
	require.NoError(t, err)
	assert.False(t, info.Fallback)
	require.Len(t, info.Partitions, 1)
	assert.Equal(t, info.RequestToPartition["req-a"], info.RequestToPartition["req-b"])
	partition := info.Partitions[info.RequestToPartition["req-a"]]
	assert.Equal(t, []string{"pod-a-uid/app/app"}, partition.Containers)
	assert.Equal(t, []string{"req-a", "req-b"}, partition.Requests)
}

func TestResolveClaimVGPUPartitions_TwoContainersSeparateRequests(t *testing.T) {
	ctx := context.Background()
	claim := newAllocatedClaim("claim-a", []string{"req-a", "req-b"})
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "pod-a", UID: types.UID("pod-a-uid")},
		Spec: corev1.PodSpec{
			ResourceClaims: []corev1.PodResourceClaim{{Name: "gpu", ResourceClaimName: strPtr("claim-a")}},
			Containers: []corev1.Container{
				{Name: "app-a", Resources: corev1.ResourceRequirements{Claims: []corev1.ResourceClaim{{Name: "gpu", Request: "req-a"}}}},
				{Name: "app-b", Resources: corev1.ResourceRequirements{Claims: []corev1.ResourceClaim{{Name: "gpu", Request: "req-b"}}}},
			},
		},
	}

	reader := &fakePartitionReader{pods: map[client.ObjectKey]*corev1.Pod{{Namespace: "default", Name: "pod-a"}: pod}}
	info, err := ResolveClaimVGPUPartitions(ctx, reader, claim, "test-driver", alwaysVGPURequest, alwaysVGPUSubRequest)
	require.NoError(t, err)
	assert.False(t, info.Fallback)
	require.Len(t, info.Partitions, 2)
	assert.NotEqual(t, info.RequestToPartition["req-a"], info.RequestToPartition["req-b"])
}

func TestResolveClaimVGPUPartitions_InitAppShareAndAppUsesExtraRequest(t *testing.T) {
	ctx := context.Background()
	claim := newAllocatedClaim("claim-a", []string{"req-a", "req-b"})
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "pod-a", UID: types.UID("pod-a-uid")},
		Spec: corev1.PodSpec{
			ResourceClaims: []corev1.PodResourceClaim{{Name: "gpu", ResourceClaimName: strPtr("claim-a")}},
			InitContainers: []corev1.Container{{
				Name:      "init-a",
				Resources: corev1.ResourceRequirements{Claims: []corev1.ResourceClaim{{Name: "gpu", Request: "req-a"}}},
			}},
			Containers: []corev1.Container{{
				Name:      "app-a",
				Resources: corev1.ResourceRequirements{Claims: []corev1.ResourceClaim{{Name: "gpu"}}},
			}},
		},
	}

	reader := &fakePartitionReader{pods: map[client.ObjectKey]*corev1.Pod{{Namespace: "default", Name: "pod-a"}: pod}}
	info, err := ResolveClaimVGPUPartitions(ctx, reader, claim, "test-driver", alwaysVGPURequest, alwaysVGPUSubRequest)
	require.NoError(t, err)
	assert.False(t, info.Fallback)
	require.Len(t, info.Partitions, 1)
	assert.Equal(t, info.RequestToPartition["req-a"], info.RequestToPartition["req-b"])
	partition := info.Partitions[info.RequestToPartition["req-a"]]
	assert.Equal(t, []string{"pod-a-uid/app/app-a", "pod-a-uid/init/init-a"}, partition.Containers)
	assert.Equal(t, []string{"req-a", "req-b"}, partition.Requests)
}

func TestResolveClaimVGPUPartitions_FallbackWhenNoResolvedEdges(t *testing.T) {
	ctx := context.Background()
	claim := newAllocatedClaim("claim-a", []string{"req-a"})
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "pod-a", UID: types.UID("pod-a-uid")},
		Spec: corev1.PodSpec{
			ResourceClaims: []corev1.PodResourceClaim{{Name: "gpu", ResourceClaimName: strPtr("other-claim")}},
			Containers: []corev1.Container{{
				Name:      "app",
				Resources: corev1.ResourceRequirements{Claims: []corev1.ResourceClaim{{Name: "gpu"}}},
			}},
		},
	}

	reader := &fakePartitionReader{pods: map[client.ObjectKey]*corev1.Pod{{Namespace: "default", Name: "pod-a"}: pod}}
	info, err := ResolveClaimVGPUPartitions(ctx, reader, claim, "test-driver", alwaysVGPURequest, alwaysVGPUSubRequest)
	require.NoError(t, err)
	assert.True(t, info.Fallback)
	assert.Empty(t, info.RequestToPartition)
}

func TestResolveClaimVGPUPartitions_FallbackWhenNoReservedPods(t *testing.T) {
	ctx := context.Background()
	claim := newAllocatedClaim("claim-a", []string{"req-a"})
	claim.Status.ReservedFor = nil

	info, err := ResolveClaimVGPUPartitions(ctx, &fakePartitionReader{}, claim, "test-driver", alwaysVGPURequest, alwaysVGPUSubRequest)
	require.NoError(t, err)
	assert.True(t, info.Fallback)
	assert.Empty(t, info.Partitions)
}

type fakePartitionReader struct {
	pods   map[client.ObjectKey]*corev1.Pod
	claims map[client.ObjectKey]*resourceapi.ResourceClaim
}

func (r *fakePartitionReader) GetPod(_ context.Context, key client.ObjectKey, obj *corev1.Pod) error {
	pod, ok := r.pods[key]
	if !ok {
		return notFoundErr("pods", key.String())
	}
	pod.DeepCopyInto(obj)
	return nil
}

func (r *fakePartitionReader) GetResourceClaim(_ context.Context, key client.ObjectKey, obj *resourceapi.ResourceClaim) error {
	claim, ok := r.claims[key]
	if !ok {
		return notFoundErr("resourceclaims", key.String())
	}
	claim.DeepCopyInto(obj)
	return nil
}

func newAllocatedClaim(name string, requests []string) *resourceapi.ResourceClaim {
	deviceRequests := make([]resourceapi.DeviceRequest, 0, len(requests))
	results := make([]resourceapi.DeviceRequestAllocationResult, 0, len(requests))
	for _, request := range requests {
		deviceRequests = append(deviceRequests, resourceapi.DeviceRequest{Name: request, Exactly: &resourceapi.ExactDeviceRequest{}})
		results = append(results, resourceapi.DeviceRequestAllocationResult{Request: request, Driver: "test-driver"})
	}
	return &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name, UID: types.UID(name + "-uid")},
		Spec:       resourceapi.ResourceClaimSpec{Devices: resourceapi.DeviceClaim{Requests: deviceRequests}},
		Status: resourceapi.ResourceClaimStatus{
			ReservedFor: []resourceapi.ResourceClaimConsumerReference{{Name: "pod-a", Resource: "pods", UID: types.UID("pod-a-uid")}},
			Allocation:  &resourceapi.AllocationResult{Devices: resourceapi.DeviceAllocationResult{Results: results}},
		},
	}
}

func alwaysVGPURequest(context.Context, resourceapi.DeviceRequest) bool { return true }

func alwaysVGPUSubRequest(context.Context, resourceapi.DeviceSubRequest) bool { return true }
