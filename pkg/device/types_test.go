package device

import (
	"fmt"
	"testing"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func Test_PodDevices(t *testing.T) {
	gpu0uuid := "GPU-" + uuid.New().String()
	gpu1uuid := "GPU-" + uuid.New().String()

	tests := []struct {
		name    string
		devices PodDeviceClaim
		want    string
	}{
		{
			name: "example 1: single container, single device",
			devices: PodDeviceClaim{
				{
					Name: "cont1",
					DeviceClaims: []DeviceClaim{
						{
							Id:     0,
							Uuid:   gpu0uuid,
							Cores:  0,
							Memory: 1024,
						},
					},
				},
			},
			want: fmt.Sprintf("cont1[0_%s_0_1024]", gpu0uuid),
		},
		{
			name: "example 2: single container, no device",
			devices: PodDeviceClaim{
				{
					Name:         "cont1",
					DeviceClaims: []DeviceClaim{},
				},
			},
			want: fmt.Sprintf("cont1[]"),
		},
		{
			name: "example 3: single container, multiple device",
			devices: PodDeviceClaim{
				{
					Name: "cont1",
					DeviceClaims: []DeviceClaim{
						{
							Id:     0,
							Uuid:   gpu0uuid,
							Cores:  0,
							Memory: 1024,
						}, {
							Id:     1,
							Uuid:   gpu1uuid,
							Cores:  10,
							Memory: 2048,
						},
					},
				},
			},
			want: fmt.Sprintf("cont1[0_%s_0_1024,1_%s_10_2048]", gpu0uuid, gpu1uuid),
		},
		{
			name: "example 4: multiple container, single device",
			devices: PodDeviceClaim{
				{
					Name: "cont1",
					DeviceClaims: []DeviceClaim{
						{
							Id:     0,
							Uuid:   gpu0uuid,
							Cores:  0,
							Memory: 1024,
						},
					},
				},
				{
					Name: "cont2",
					DeviceClaims: []DeviceClaim{
						{
							Id:     0,
							Uuid:   gpu0uuid,
							Cores:  0,
							Memory: 1024,
						},
					},
				},
			},
			want: fmt.Sprintf("cont1[0_%s_0_1024];cont2[0_%s_0_1024]", gpu0uuid, gpu0uuid),
		},
		{
			name: "example 5: multiple container, multiple device",
			devices: PodDeviceClaim{
				{
					Name: "cont1",
					DeviceClaims: []DeviceClaim{
						{
							Id:     0,
							Uuid:   gpu0uuid,
							Cores:  0,
							Memory: 1024,
						}, {
							Id:     1,
							Uuid:   gpu1uuid,
							Cores:  50,
							Memory: 10240,
						},
					},
				},
				{
					Name: "cont2",
					DeviceClaims: []DeviceClaim{
						{
							Id:     0,
							Uuid:   gpu0uuid,
							Cores:  0,
							Memory: 1024,
						}, {
							Id:     1,
							Uuid:   gpu1uuid,
							Cores:  50,
							Memory: 10240,
						},
					},
				},
			},
			want: fmt.Sprintf("cont1[0_%s_0_1024,1_%s_50_10240];cont2[0_%s_0_1024,1_%s_50_10240]",
				gpu0uuid, gpu1uuid, gpu0uuid, gpu1uuid),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			text, err := test.devices.MarshalText()
			if err != nil {
				t.Error(err)
			}
			assert.Equal(t, test.want, text)
			podDevices := PodDeviceClaim{}
			err = podDevices.UnmarshalText(text)
			if err != nil {
				t.Error(err)
			}
			assert.Equal(t, test.devices, podDevices)
		})
	}
}

func Test_GetCurrentContainerDevice(t *testing.T) {
	gpu0uuid := "GPU-" + uuid.New().String()
	gpu1uuid := "GPU-" + uuid.New().String()

	tests := []struct {
		name string
		pod  *corev1.Pod
		want *ContainerDeviceClaim
	}{
		{
			name: "example 1",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						util.PodVGPUPreAllocAnnotation: fmt.Sprintf("cont1[0_%s_10_1024]", gpu0uuid),
					},
					Name:      "test1",
					Namespace: "default",
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "cont1",
						},
					},
				},
			},
			want: &ContainerDeviceClaim{
				Name: "cont1",
				DeviceClaims: []DeviceClaim{
					{
						Id:     0,
						Uuid:   gpu0uuid,
						Cores:  10,
						Memory: 1024,
					},
				},
			},
		},
		{
			name: "example 2",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						util.PodVGPUPreAllocAnnotation: fmt.Sprintf("cont2[0_%s_10_1024,1_%s_10_1024]", gpu0uuid, gpu1uuid),
					},
					Name:      "test2",
					Namespace: "default",
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "cont1",
						}, {
							Name: "cont2",
						},
					},
				},
			},
			want: &ContainerDeviceClaim{
				Name: "cont2",
				DeviceClaims: []DeviceClaim{
					{
						Id:     0,
						Uuid:   gpu0uuid,
						Cores:  10,
						Memory: 1024,
					}, {
						Id:     1,
						Uuid:   gpu1uuid,
						Cores:  10,
						Memory: 1024,
					},
				},
			},
		},
		{
			name: "example 3",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						util.PodVGPUPreAllocAnnotation:  fmt.Sprintf("cont1[0_%s_10_1024,1_%s_10_1024];cont2[0_%s_20_2048,1_%s_20_2048]", gpu0uuid, gpu1uuid, gpu0uuid, gpu1uuid),
						util.PodVGPURealAllocAnnotation: fmt.Sprintf("cont1[0_%s_10_1024,1_%s_10_1024]", gpu0uuid, gpu1uuid),
					},
					Name:      "test3",
					Namespace: "default",
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "cont1",
						}, {
							Name: "cont2",
						},
					},
				},
			},
			want: &ContainerDeviceClaim{
				Name: "cont2",
				DeviceClaims: []DeviceClaim{
					{
						Id:     0,
						Uuid:   gpu0uuid,
						Cores:  20,
						Memory: 2048,
					}, {
						Id:     1,
						Uuid:   gpu1uuid,
						Cores:  20,
						Memory: 2048,
					},
				},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			contDevices, err := GetCurrentPreAllocateContainerDevice(test.pod)
			if err != nil {
				t.Error(err)
			}
			assert.Equal(t, *test.want, *contDevices)
		})
	}
}

func Test_PodStatusUnschedulable(t *testing.T) {
	base := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)

	type podOpts struct {
		nodeName      string
		condition     *corev1.PodCondition
		predicateTime string // empty = annotation absent
	}
	makePod := func(o podOpts) *corev1.Pod {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{}},
			Spec:       corev1.PodSpec{NodeName: o.nodeName},
		}
		if o.condition != nil {
			pod.Status.Conditions = []corev1.PodCondition{*o.condition}
		}
		if o.predicateTime != "" {
			pod.Annotations[util.PodPredicateTimeAnnotation] = o.predicateTime
		}
		return pod
	}
	unschedulableCond := func(at time.Time) *corev1.PodCondition {
		return &corev1.PodCondition{
			Type:               corev1.PodScheduled,
			Status:             corev1.ConditionFalse,
			Reason:             corev1.PodReasonUnschedulable,
			LastTransitionTime: metav1.NewTime(at),
		}
	}
	nanos := func(t time.Time) string {
		return fmt.Sprintf("%d", t.UnixNano())
	}

	tests := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{
			name: "already bound to a node returns false regardless of condition",
			pod: makePod(podOpts{
				nodeName:      "node-1",
				condition:     unschedulableCond(base),
				predicateTime: nanos(base),
			}),
			want: false,
		},
		{
			name: "no PodScheduled condition (first scheduling attempt)",
			pod:  makePod(podOpts{}),
			want: false,
		},
		{
			name: "condition is False but reason is not Unschedulable",
			pod: makePod(podOpts{
				condition: &corev1.PodCondition{
					Type:               corev1.PodScheduled,
					Status:             corev1.ConditionFalse,
					Reason:             "SchedulerError",
					LastTransitionTime: metav1.NewTime(base),
				},
			}),
			want: false,
		},
		{
			name: "condition Status True is never Unschedulable",
			pod: makePod(podOpts{
				condition: &corev1.PodCondition{
					Type:               corev1.PodScheduled,
					Status:             corev1.ConditionTrue,
					LastTransitionTime: metav1.NewTime(base),
				},
			}),
			want: false,
		},
		{
			name: "Unschedulable condition with no predicate-time annotation falls back to condition-only logic",
			pod: makePod(podOpts{
				condition: unschedulableCond(base),
			}),
			want: true,
		},
		{
			name: "predicate-time strictly newer than condition: Filter passed in current cycle",
			pod: makePod(podOpts{
				condition:     unschedulableCond(base),
				predicateTime: nanos(base.Add(2 * time.Second)),
			}),
			want: false,
		},
		{
			name: "predicate-time older than condition: current cycle rejected after Filter",
			pod: makePod(podOpts{
				condition:     unschedulableCond(base.Add(2 * time.Second)),
				predicateTime: nanos(base),
			}),
			want: true,
		},
		{
			name: "same-second boundary is conservatively treated as condition newer",
			pod: makePod(podOpts{
				condition:     unschedulableCond(base),
				predicateTime: nanos(base.Add(500 * time.Millisecond)),
			}),
			want: true,
		},
		{
			name: "malformed predicate-time falls back to condition-only logic",
			pod: makePod(podOpts{
				condition:     unschedulableCond(base),
				predicateTime: "not-a-number",
			}),
			want: true,
		},
		{
			name: "empty predicate-time string falls back to condition-only logic",
			pod: makePod(podOpts{
				condition:     unschedulableCond(base),
				predicateTime: "",
			}),
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, PodStatusUnschedulable(tt.pod))
		})
	}
}
