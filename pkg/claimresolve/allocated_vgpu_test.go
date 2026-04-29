package claimresolve

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestGetAllocatedVGPURequests(t *testing.T) {
	ctx := context.Background()
	claim := &resourceapi.ResourceClaim{
		Spec: resourceapi.ResourceClaimSpec{
			Devices: resourceapi.DeviceClaim{Requests: []resourceapi.DeviceRequest{
				{Name: "vgpu-main", Exactly: &resourceapi.ExactDeviceRequest{}},
				{Name: "mixed-main", FirstAvailable: []resourceapi.DeviceSubRequest{{Name: "sub-vgpu"}, {Name: "sub-other"}}},
			}},
		},
		Status: resourceapi.ResourceClaimStatus{
			Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{
					Results: []resourceapi.DeviceRequestAllocationResult{{Request: "vgpu-main"}, {Request: "mixed-main/sub-vgpu"}},
				},
			},
		},
	}

	isVGPURequest := func(_ context.Context, req resourceapi.DeviceRequest) bool {
		return req.Name == "vgpu-main"
	}
	isVGPUSubRequest := func(_ context.Context, sub resourceapi.DeviceSubRequest) bool {
		return sub.Name == "sub-vgpu"
	}

	assert.Equal(t, sets.New("mixed-main", "vgpu-main"), GetAllocatedVGPURequests(ctx, claim, "", isVGPURequest, isVGPUSubRequest))
}

func TestResolveActualAllocatedRequestsForClaimRef(t *testing.T) {
	allocated := sets.New("req-a", "req-b")

	assert.Equal(t, []string{"req-a"}, ResolveActualAllocatedRequestsForClaimRef(corev1.ResourceClaim{Name: "c", Request: "req-a"}, allocated))
	assert.Nil(t, ResolveActualAllocatedRequestsForClaimRef(corev1.ResourceClaim{Name: "c", Request: "req-missing"}, allocated))
	assert.Equal(t, []string{"req-a", "req-b"}, ResolveActualAllocatedRequestsForClaimRef(corev1.ResourceClaim{Name: "c"}, allocated))
}

func TestResolveActualClaimNameForPodClaim(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "pod-a"},
		Spec:       corev1.PodSpec{ResourceClaims: []corev1.PodResourceClaim{{Name: "direct", ResourceClaimName: strPtr("claim-a")}, {Name: "templ", ResourceClaimTemplateName: strPtr("tpl-a")}}},
		Status:     corev1.PodStatus{ResourceClaimStatuses: []corev1.PodResourceClaimStatus{{Name: "templ", ResourceClaimName: strPtr("claim-from-status")}}},
	}

	name, ok, err := ResolveActualClaimNameForPodClaim(pod, "direct")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "claim-a", name)

	name, ok, err = ResolveActualClaimNameForPodClaim(pod, "templ")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "claim-from-status", name)
}

func TestGetReservedPodsSkipsNotFoundAndUIDMismatch(t *testing.T) {
	ctx := context.Background()
	claim := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "claim-a"},
		Status: resourceapi.ResourceClaimStatus{ReservedFor: []resourceapi.ResourceClaimConsumerReference{
			{Name: "pod-a", Resource: "pods", UID: types.UID("uid-a")},
			{Name: "pod-b", Resource: "pods", UID: types.UID("uid-b")},
			{Name: "pod-missing", Resource: "pods", UID: types.UID("uid-missing")},
		}},
	}

	reader := &fakeReader{pods: map[client.ObjectKey]*corev1.Pod{
		{Namespace: "default", Name: "pod-a"}: {ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "pod-a", UID: types.UID("uid-a")}},
		{Namespace: "default", Name: "pod-b"}: {ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "pod-b", UID: types.UID("wrong")}},
	}}

	pods, err := GetReservedPods(ctx, reader, claim)
	require.NoError(t, err)
	require.Len(t, pods, 1)
	assert.Equal(t, "pod-a", pods[0].Name)
}

type fakeReader struct {
	pods map[client.ObjectKey]*corev1.Pod
}

func (r *fakeReader) GetPod(_ context.Context, key client.ObjectKey, obj *corev1.Pod) error {
	pod, ok := r.pods[key]
	if !ok {
		return notFoundErr("pods", key.String())
	}
	pod.DeepCopyInto(obj)
	return nil
}

func (r *fakeReader) GetResourceClaim(_ context.Context, _ client.ObjectKey, _ *resourceapi.ResourceClaim) error {
	return nil
}

func strPtr(s string) *string {
	return &s
}

func notFoundErr(resource, name string) error {
	return apierrors.NewNotFound(schema.GroupResource{Resource: resource}, name)
}
