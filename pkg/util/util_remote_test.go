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
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
		NodeRemoteServerLabel:   "true",
		NodeRemoteConsumerLabel: "false",
	}}}
	assert.True(t, IsRemoteServerNode(node))
	assert.False(t, IsRemoteConsumerNode(node))
	assert.False(t, IsRemoteServerNode(nil))
	assert.False(t, IsRemoteConsumerNode(nil))
}
