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

package bind

import (
	"context"
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
	extenderv1 "k8s.io/kube-scheduler/extender/v1"
)

// A remote pod is bound to a consumer node while its predicate node is the GPU server.
func Test_Bind_RemotePod(t *testing.T) {
	tests := []struct {
		name          string
		predicateNode string
		wantError     bool
	}{
		{"bound to a consumer", "gpu-server", false},
		{"not placed on a server", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k8sClient := fake.NewClientset()
			bindPredicate, err := New(k8sClient, record.NewFakeRecorder(16), nil, true)
			assert.NoError(t, err)
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "remote",
					Namespace: "default",
					UID:       uuid.NewUUID(),
					Annotations: map[string]string{
						util.VGPUAccessModeAnnotation:   util.AccessModeRemote,
						util.PodPredicateNodeAnnotation: tt.predicateNode,
					},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name: "cont1",
					Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
						corev1.ResourceName(util.VGPUNumberResourceName): resource.MustParse("1"),
					}},
				}}},
				Status: corev1.PodStatus{Phase: corev1.PodPending},
			}
			_, err = k8sClient.CoreV1().Pods(pod.Namespace).Create(context.Background(), pod, metav1.CreateOptions{})
			assert.NoError(t, err)

			result := bindPredicate.Bind(context.Background(), extenderv1.ExtenderBindingArgs{
				PodName:      pod.Name,
				PodNamespace: pod.Namespace,
				PodUID:       pod.UID,
				Node:         "consumer",
			})

			assert.Equal(t, tt.wantError, result.Error != "", result.Error)
		})
	}
}
