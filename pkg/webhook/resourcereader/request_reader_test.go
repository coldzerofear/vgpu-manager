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
	fake2 "k8s.io/client-go/kubernetes/fake"
	kcache "k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"
)

func TestGetDeviceRequestsForPodClaim_LiveFallbackAndMutationCache(t *testing.T) {
	ctx := context.Background()

	claim := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "claim-a"},
		Spec: resourceapi.ResourceClaimSpec{
			Devices: resourceapi.DeviceClaim{Requests: []resourceapi.DeviceRequest{{Name: "req-a"}}},
		},
	}

	k8sClient := fake2.NewSimpleClientset(claim)
	reader := NewResourceAPIReader(k8sClient, newIndexer(), newIndexer(), newIndexer(), newIndexer(), time.Minute)

	podClaim := &corev1.PodResourceClaim{Name: "pc-a", ResourceClaimName: ptr.To("claim-a")}

	requests, err := reader.GetDeviceRequestsForPodClaim(ctx, "default", podClaim)
	require.NoError(t, err)
	require.Len(t, requests, 1)
	assert.Equal(t, "req-a", requests[0].Name)

	require.NoError(t, k8sClient.ResourceV1().ResourceClaims(claim.Namespace).Delete(ctx, claim.Name, metav1.DeleteOptions{}))

	requests, err = reader.GetDeviceRequestsForPodClaim(ctx, "default", podClaim)
	require.NoError(t, err)
	require.Len(t, requests, 1)
	assert.Equal(t, "req-a", requests[0].Name)
}

func TestGetDeviceRequestsForPodClaim_TemplatePath(t *testing.T) {
	ctx := context.Background()

	tpl := &resourceapi.ResourceClaimTemplate{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "tpl-a"},
		Spec: resourceapi.ResourceClaimTemplateSpec{
			Spec: resourceapi.ResourceClaimSpec{
				Devices: resourceapi.DeviceClaim{Requests: []resourceapi.DeviceRequest{{Name: "req-from-template"}}},
			},
		},
	}

	k8sClient := fake2.NewSimpleClientset(tpl)
	reader := NewResourceAPIReader(k8sClient, newIndexer(), newIndexer(), newIndexer(), newIndexer(), time.Minute)

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

func newIndexer() kcache.Indexer {
	return kcache.NewIndexer(kcache.MetaNamespaceKeyFunc, kcache.Indexers{})
}
