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

package client

import (
	"context"
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/util"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// The metrics-node label follows the node whose devices the pod uses.
func TestPatchPodAllocationSucceedMetricsNode(t *testing.T) {
	tests := []struct {
		name        string
		accessMode  string
		wantMetrics string
	}{
		{"local pod", "", "consumer"},
		{"remote pod", util.AccessModeRemote, "gpu-server"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			annotations := map[string]string{
				util.PodPredicateNodeAnnotation: "gpu-server",
				util.PodVGPUPreAllocAnnotation:  "cont1[0_GPU-0_50_1024]",
				util.PodVGPURealAllocAnnotation: "cont1[0_GPU-0_50_1024]",
			}
			if tt.accessMode != "" {
				annotations[util.VGPUAccessModeAnnotation] = tt.accessMode
			}
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "ns", Annotations: annotations},
				Spec:       corev1.PodSpec{NodeName: "consumer"},
			}
			kubeClient := fake.NewClientset(pod.DeepCopy())

			if err := PatchPodAllocationSucceed(kubeClient, pod); err != nil {
				t.Fatal(err)
			}

			got, err := kubeClient.CoreV1().Pods("ns").Get(context.Background(), "pod", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if node := got.Labels[util.PodMetricsNodeLabel]; node != tt.wantMetrics {
				t.Errorf("metrics-node = %q, want %q", node, tt.wantMetrics)
			}
			if phase := got.Labels[util.PodAssignedPhaseLabel]; phase != string(util.AssignPhaseSucceed) {
				t.Errorf("assigned-phase = %q, want %q", phase, util.AssignPhaseSucceed)
			}
		})
	}
}
