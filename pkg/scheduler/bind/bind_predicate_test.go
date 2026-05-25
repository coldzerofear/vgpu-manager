package bind

import (
	"context"
	"fmt"
	"testing"

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

func Test_BindPredicate(t *testing.T) {
	k8sClient := fake.NewSimpleClientset()
	broadcaster := record.NewBroadcaster()
	broadcaster.StartRecordingToSink(&typedv1.EventSinkImpl{Interface: k8sClient.CoreV1().Events("")})
	recorder := broadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "test"})
	defer broadcaster.Shutdown()

	bindPredicate, err := New(k8sClient, recorder, nil, true)
	if err != nil {
		t.Fatalf("failed to create new bindPredicate due to %v", err)
	}
	poduid := uuid.NewUUID()
	testCases := []struct {
		name   string
		pod    *corev1.Pod
		args   extenderv1.ExtenderBindingArgs
		result *extenderv1.ExtenderBindingResult
	}{
		{
			name: "example1: different pod uid",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test1",
					Namespace: "default",
					UID:       poduid,
					Annotations: map[string]string{
						util.PodPredicateNodeAnnotation: "node1",
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name: "cont1",
						Resources: corev1.ResourceRequirements{
							Limits: corev1.ResourceList{
								corev1.ResourceName(util.VGPUNumberResourceName): resource.MustParse(fmt.Sprintf("%d", 1)),
							},
						},
					}},
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodPending,
				},
			},
			args: extenderv1.ExtenderBindingArgs{
				PodName:      "test1",
				PodNamespace: "default",
				PodUID:       uuid.NewUUID(),
				Node:         "node1",
			},
			result: &extenderv1.ExtenderBindingResult{
				Error: "different UID from the target pod",
			},
		}, {
			name: "example2: pod not found",
			pod:  nil,
			args: extenderv1.ExtenderBindingArgs{
				PodName:      "test2",
				PodNamespace: "default",
				PodUID:       uuid.NewUUID(),
				Node:         "node1",
			},
			result: &extenderv1.ExtenderBindingResult{
				Error: "pods \"test2\" not found",
			},
		},
		{
			name: "example3: mismatch of predicate node",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test2",
					Namespace: "default",
					UID:       poduid,
					Annotations: map[string]string{
						util.PodPredicateNodeAnnotation: "node1",
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name: "cont1",
						Resources: corev1.ResourceRequirements{
							Limits: corev1.ResourceList{
								corev1.ResourceName(util.VGPUNumberResourceName): resource.MustParse(fmt.Sprintf("%d", 1)),
							},
						},
					}},
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodPending,
				},
			},
			args: extenderv1.ExtenderBindingArgs{
				PodName:      "test2",
				PodNamespace: "default",
				PodUID:       poduid,
				Node:         "node2",
			},
			result: &extenderv1.ExtenderBindingResult{
				Error: "predicate node and binding node do not match",
			},
		},
		{
			name: "example4: devices pre allocation failure",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test3",
					Namespace: "default",
					UID:       poduid,
					Annotations: map[string]string{
						util.PodPredicateNodeAnnotation: "node1",
					},
					Labels: map[string]string{
						util.PodAssignedPhaseLabel: string(util.AssignPhaseFailed),
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name: "cont1",
						Resources: corev1.ResourceRequirements{
							Limits: corev1.ResourceList{
								corev1.ResourceName(util.VGPUNumberResourceName): resource.MustParse(fmt.Sprintf("%d", 1)),
							},
						},
					}},
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodPending,
				},
			},
			args: extenderv1.ExtenderBindingArgs{
				PodName:      "test3",
				PodNamespace: "default",
				PodUID:       poduid,
				Node:         "node1",
			},
			result: &extenderv1.ExtenderBindingResult{
				Error: fmt.Sprintf("device pre allocation failed, unable to bind to node <%s>", "node1"),
			},
		},
		{
			name: "example5: binding success",
			pod:  nil,
			args: extenderv1.ExtenderBindingArgs{
				PodName:      "test1",
				PodNamespace: "default",
				PodUID:       poduid,
				Node:         "node1",
			},
			result: &extenderv1.ExtenderBindingResult{},
		},
		{
			name: "example6: node is empty",
			pod:  nil,
			args: extenderv1.ExtenderBindingArgs{
				PodName:      "test1",
				PodNamespace: "default",
				PodUID:       poduid,
				Node:         "",
			},
			result: &extenderv1.ExtenderBindingResult{
				Error: "ExtenderBindingArgs.Node cannot be empty",
			},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			if testCase.pod != nil {
				_, _ = k8sClient.CoreV1().Pods(testCase.pod.Namespace).
					Create(context.Background(), testCase.pod, metav1.CreateOptions{})
			}
			result := bindPredicate.Bind(context.Background(), testCase.args)
			assert.Equal(t, testCase.result, result)
		})
	}
}
