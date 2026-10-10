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

package mutate

import (
	"context"
	"testing"

	"github.com/coldzerofear/vgpu-manager/cmd/device-webhook/options"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A remote pod only runs on a node the consumer plugin claimed, so the pod is
// given that label as a node selector; everything else is left alone.
func TestMutateCreateRemoteNodeSelector(t *testing.T) {
	vgpuPod := func(accessMode string, selector map[string]string) *corev1.Pod {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pod", Namespace: "default",
				Annotations: map[string]string{},
			},
			Spec: corev1.PodSpec{
				NodeSelector: selector,
				Containers: []corev1.Container{{
					Name: "app",
					Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
						corev1.ResourceName(util.VGPUNumberResourceName): resource.MustParse("1"),
					}},
				}},
			},
		}
		if accessMode != "" {
			pod.Annotations[util.VGPUAccessModeAnnotation] = accessMode
		}
		return pod
	}
	handle := &mutateHandle{options: &options.Options{}}

	t.Run("a remote pod is pinned to the consumer nodes", func(t *testing.T) {
		pod := vgpuPod(util.AccessModeRemote, nil)

		require.NoError(t, handle.MutateCreate(context.Background(), pod, false))

		assert.Equal(t, map[string]string{util.NodeRemoteConsumerLabel: "true"}, pod.Spec.NodeSelector)
	})

	t.Run("selectors of its own are kept", func(t *testing.T) {
		pod := vgpuPod(util.AccessModeRemote, map[string]string{"zone": "a"})

		require.NoError(t, handle.MutateCreate(context.Background(), pod, false))

		assert.Equal(t, map[string]string{
			"zone": "a", util.NodeRemoteConsumerLabel: "true",
		}, pod.Spec.NodeSelector)
	})

	t.Run("a local pod is left where it is", func(t *testing.T) {
		for _, mode := range []string{"", util.AccessModeLocal} {
			pod := vgpuPod(mode, map[string]string{"zone": "a"})

			require.NoError(t, handle.MutateCreate(context.Background(), pod, false))

			assert.Equal(t, map[string]string{"zone": "a"}, pod.Spec.NodeSelector, "access mode %q", mode)
		}
	})

	// The remote switch rides on a vGPU request; a pod without one is not ours
	// to place (a DRA pod reaches its server through the claim's pool instead).
	t.Run("a pod without a vGPU request is untouched", func(t *testing.T) {
		pod := vgpuPod(util.AccessModeRemote, nil)
		pod.Spec.Containers[0].Resources.Limits = nil

		require.NoError(t, handle.MutateCreate(context.Background(), pod, false))

		assert.Empty(t, pod.Spec.NodeSelector)
	})
}
