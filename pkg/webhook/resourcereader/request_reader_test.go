package resourcereader

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	kcache "k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestGetDeviceRequestsForPodClaim_LiveFallbackAndMutationCache(t *testing.T) {
	ctx := context.Background()
	testScheme := newTestScheme(t)

	claim := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "claim-a"},
		Spec: resourceapi.ResourceClaimSpec{
			Devices: resourceapi.DeviceClaim{Requests: []resourceapi.DeviceRequest{{Name: "req-a"}}},
		},
	}

	liveClient := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(claim).Build()
	reader := NewResourceAPIReader(liveClient, newIndexer(), newIndexer(), newIndexer(), newIndexer(), time.Minute)

	podClaim := &corev1.PodResourceClaim{Name: "pc-a", ResourceClaimName: ptr.To("claim-a")}

	requests, err := reader.GetDeviceRequestsForPodClaim(ctx, "default", podClaim)
	require.NoError(t, err)
	require.Len(t, requests, 1)
	assert.Equal(t, "req-a", requests[0].Name)

	require.NoError(t, liveClient.Delete(ctx, claim))

	requests, err = reader.GetDeviceRequestsForPodClaim(ctx, "default", podClaim)
	require.NoError(t, err)
	require.Len(t, requests, 1)
	assert.Equal(t, "req-a", requests[0].Name)
}

func TestGetDeviceRequestsForPodClaim_TemplatePath(t *testing.T) {
	ctx := context.Background()
	testScheme := newTestScheme(t)

	tpl := &resourceapi.ResourceClaimTemplate{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "tpl-a"},
		Spec: resourceapi.ResourceClaimTemplateSpec{
			Spec: resourceapi.ResourceClaimSpec{
				Devices: resourceapi.DeviceClaim{Requests: []resourceapi.DeviceRequest{{Name: "req-from-template"}}},
			},
		},
	}

	cachedClient := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(tpl).Build()
	reader := NewResourceAPIReader(cachedClient, newIndexer(), newIndexer(), newIndexer(), newIndexer(), time.Minute)

	podClaim := &corev1.PodResourceClaim{Name: "pc-a", ResourceClaimTemplateName: ptr.To("tpl-a")}
	requests, err := reader.GetDeviceRequestsForPodClaim(ctx, "default", podClaim)
	require.NoError(t, err)
	require.Len(t, requests, 1)
	assert.Equal(t, "req-from-template", requests[0].Name)
}

func TestGetDeviceRequestsForPodClaim_NotFoundWhenNoClients(t *testing.T) {
	ctx := context.Background()
	reader := NewResourceAPIReader(nil, newIndexer(), newIndexer(), newIndexer(), newIndexer(), time.Minute)

	podClaim := &corev1.PodResourceClaim{Name: "pc-a", ResourceClaimName: ptr.To("missing")}
	_, err := reader.GetDeviceRequestsForPodClaim(ctx, "default", podClaim)
	require.Error(t, err)
	assert.True(t, apierrors.IsNotFound(err))
}

func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	testScheme := runtime.NewScheme()
	require.NoError(t, scheme.AddToScheme(testScheme))
	require.NoError(t, resourceapi.AddToScheme(testScheme))
	return testScheme
}

func newIndexer() kcache.Indexer {
	return kcache.NewIndexer(kcache.MetaNamespaceKeyFunc, kcache.Indexers{})
}
