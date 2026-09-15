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

package util

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPodPlanSchedulingNode(t *testing.T) {
	remote := map[string]string{
		VGPUAccessModeAnnotation:   AccessModeRemote,
		PodPredicateNodeAnnotation: "server",
	}
	local := map[string]string{PodPredicateNodeAnnotation: "node"}
	pod := func(nodeName string, annotations map[string]string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Annotations: annotations},
			Spec:       corev1.PodSpec{NodeName: nodeName},
		}
	}
	tests := []struct {
		name string
		pod  *corev1.Pod
		want string
	}{
		{"local pending", pod("", local), "node"},
		{"local bound", pod("bound", local), "bound"},
		{"remote pending", pod("", remote), "server"},
		{"remote bound to a consumer", pod("consumer", remote), "server"},
		{"nil pod", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, PodPlanSchedulingNode(tt.pod))
		})
	}
}

func TestRemoteNodeRoles(t *testing.T) {
	node := func(labels, annotations map[string]string, number string) *corev1.Node {
		n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: annotations}}
		if number != "" {
			n.Status.Allocatable = corev1.ResourceList{corev1.ResourceName(VGPUNumberResourceName): resource.MustParse(number)}
		}
		return n
	}
	server := map[string]string{NodeRemoteServerLabel: "true"}
	consumer := map[string]string{NodeRemoteConsumerLabel: "true"}
	endpoints := map[string]string{NodeRemoteEndpointsAnnotation: "{}"}

	assert.True(t, IsRemoteServerNode(node(server, endpoints, "")))
	assert.False(t, IsRemoteServerNode(node(server, nil, "")), "server label without endpoints")
	assert.False(t, IsRemoteServerNode(node(nil, endpoints, "")), "endpoints without server label")
	assert.False(t, IsRemoteServerNode(nil))

	assert.True(t, IsRemoteConsumerNode(node(consumer, nil, "10")))
	assert.False(t, IsRemoteConsumerNode(node(consumer, nil, "")), "consumer label without vgpu-number")
	assert.False(t, IsRemoteConsumerNode(node(consumer, nil, "0")), "consumer label with zero vgpu-number")
	assert.False(t, IsRemoteConsumerNode(node(nil, nil, "10")), "vgpu-number without consumer label")
	assert.False(t, IsRemoteConsumerNode(nil))
}
