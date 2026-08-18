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

	"github.com/coldzerofear/vgpu-manager/pkg/scheduler/metrics"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
	typedv1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"
	extenderv1 "k8s.io/kube-scheduler/extender/v1"
)

func bindCount(result string) float64 {
	return metrics.CounterValue("verb_total", map[string]string{
		"verb": metrics.VerbBind, "result": result,
	})
}

// The bind outcome label is the whole point of the metric: a bind that fails
// because the pre-allocation expired needs a different response from one the
// API server rejected, so the two must never collapse into a single "failed".
func Test_Bind_RecordsOutcomePerFailureMode(t *testing.T) {
	k8sClient := fake.NewSimpleClientset()
	broadcaster := record.NewBroadcaster()
	broadcaster.StartRecordingToSink(&typedv1.EventSinkImpl{Interface: k8sClient.CoreV1().Events("")})
	recorder := broadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "test"})
	defer broadcaster.Shutdown()

	binding, err := New(k8sClient, recorder, nil, true)
	assert.NoError(t, err)

	podUID := uuid.NewUUID()
	vgpuPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "metrics-pod",
			Namespace: "default",
			UID:       podUID,
			Annotations: map[string]string{
				// Predicated elsewhere: binding it to "node2" must be refused.
				util.PodPredicateNodeAnnotation: "node1",
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "cont1",
			Resources: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceName(util.VGPUNumberResourceName): resource.MustParse("1"),
				},
			},
		}}},
	}
	_, err = k8sClient.CoreV1().Pods("default").Create(context.Background(), vgpuPod, metav1.CreateOptions{})
	assert.NoError(t, err)

	testCases := []struct {
		name   string
		args   extenderv1.ExtenderBindingArgs
		result string
	}{
		{
			name:   "no target node",
			args:   extenderv1.ExtenderBindingArgs{PodName: "metrics-pod", PodNamespace: "default", PodUID: podUID},
			result: metrics.ResultBindNoNode,
		}, {
			name: "pod does not exist",
			args: extenderv1.ExtenderBindingArgs{
				PodName: "ghost", PodNamespace: "default", PodUID: podUID, Node: "node1"},
			result: metrics.ResultBindPodNotFound,
		}, {
			name: "pod was recreated under the same name",
			args: extenderv1.ExtenderBindingArgs{
				PodName: "metrics-pod", PodNamespace: "default", PodUID: uuid.NewUUID(), Node: "node1"},
			result: metrics.ResultBindUIDMismatch,
		}, {
			name: "bound node is not the predicated one",
			args: extenderv1.ExtenderBindingArgs{
				PodName: "metrics-pod", PodNamespace: "default", PodUID: podUID, Node: "node2"},
			result: metrics.ResultBindNodeMismatch,
		}, {
			name: "pre-allocation no longer current",
			args: extenderv1.ExtenderBindingArgs{
				PodName: "metrics-pod", PodNamespace: "default", PodUID: podUID, Node: "node1"},
			result: metrics.ResultBindPreAllocExpired,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			before := bindCount(testCase.result)
			bindResult := binding.Bind(context.Background(), testCase.args)
			assert.NotEmpty(t, bindResult.Error)
			assert.Equal(t, before+1, bindCount(testCase.result))
		})
	}
}

// A pod without vGPU resources skips every vGPU gate and binds normally.
func Test_Bind_RecordsSuccess(t *testing.T) {
	k8sClient := fake.NewSimpleClientset()
	broadcaster := record.NewBroadcaster()
	broadcaster.StartRecordingToSink(&typedv1.EventSinkImpl{Interface: k8sClient.CoreV1().Events("")})
	recorder := broadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "test"})
	defer broadcaster.Shutdown()

	binding, err := New(k8sClient, recorder, nil, true)
	assert.NoError(t, err)

	podUID := uuid.NewUUID()
	_, err = k8sClient.CoreV1().Pods("default").Create(context.Background(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "plain-pod", Namespace: "default", UID: podUID},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "cont1"}}},
	}, metav1.CreateOptions{})
	assert.NoError(t, err)

	before := bindCount(metrics.ResultBindSuccess)
	result := binding.Bind(context.Background(), extenderv1.ExtenderBindingArgs{
		PodName: "plain-pod", PodNamespace: "default", PodUID: podUID, Node: "node1",
	})
	assert.Empty(t, result.Error)
	assert.Equal(t, before+1, bindCount(metrics.ResultBindSuccess))
}
