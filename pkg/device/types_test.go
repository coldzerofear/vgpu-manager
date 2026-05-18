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

func Test_ShouldCountPodDeviceAllocation(t *testing.T) {
	// Anchor time-sensitive cases to wall clock; the function consults
	// time.Since(predicateTime) for the stuck-vs-bind-window distinction, so
	// tests must position predicateTime relative to "now".
	now := time.Now()
	// recent = inside StuckGracePeriod (treat as bind window)
	// aged   = outside StuckGracePeriod (treat as stuck)
	recent := now.Add(-1 * time.Second)
	aged := now.Add(-2 * StuckGracePeriod)

	type podOpts struct {
		nodeName      string
		condition     *corev1.PodCondition
		predicateTime string // empty = annotation absent
		assignedPhase string // empty = label absent
	}
	makePod := func(o podOpts) *corev1.Pod {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{},
				Labels:      map[string]string{},
			},
			Spec: corev1.PodSpec{NodeName: o.nodeName},
		}
		if o.condition != nil {
			pod.Status.Conditions = []corev1.PodCondition{*o.condition}
		}
		if o.predicateTime != "" {
			pod.Annotations[util.PodPredicateTimeAnnotation] = o.predicateTime
		}
		if o.assignedPhase != "" {
			pod.Labels[util.PodAssignedPhaseLabel] = o.assignedPhase
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
			name: "already bound to a node: count regardless of condition",
			pod: makePod(podOpts{
				nodeName:      "node-1",
				condition:     unschedulableCond(aged),
				predicateTime: nanos(aged),
			}),
			want: true,
		},
		{
			name: "bind failed last cycle (phase=failed): do not count",
			pod: makePod(podOpts{
				assignedPhase: string(util.AssignPhaseFailed),
				// MaxUint64 sentinel written by PatchPodAllocationFailed;
				// the phase=failed check must fire before the timestamp branch.
				predicateTime: fmt.Sprintf("%d", uint64(^uint64(0))),
				condition:     unschedulableCond(aged),
			}),
			want: false,
		},
		{
			name: "phase=failed without PodScheduled condition: do not count",
			pod: makePod(podOpts{
				assignedPhase: string(util.AssignPhaseFailed),
			}),
			want: false,
		},
		{
			name: "phase=allocating within bind grace: count",
			pod: makePod(podOpts{
				assignedPhase: string(util.AssignPhaseAllocating),
				condition:     unschedulableCond(recent.Add(-1 * time.Second)),
				predicateTime: nanos(recent),
			}),
			want: true,
		},
		{
			name: "no PodScheduled condition (first scheduling attempt): count",
			pod:  makePod(podOpts{}),
			want: true,
		},
		{
			name: "condition False but reason not Unschedulable: count",
			pod: makePod(podOpts{
				condition: &corev1.PodCondition{
					Type:               corev1.PodScheduled,
					Status:             corev1.ConditionFalse,
					Reason:             "SchedulerError",
					LastTransitionTime: metav1.NewTime(aged),
				},
			}),
			want: true,
		},
		{
			name: "condition Status True: count",
			pod: makePod(podOpts{
				condition: &corev1.PodCondition{
					Type:               corev1.PodScheduled,
					Status:             corev1.ConditionTrue,
					LastTransitionTime: metav1.NewTime(aged),
				},
			}),
			want: true,
		},
		{
			name: "Unschedulable condition without predicate-time: do not count",
			pod: makePod(podOpts{
				condition: unschedulableCond(aged),
			}),
			want: false,
		},
		{
			name: "predicate-time recent and newer than condition: bind window, count",
			pod: makePod(podOpts{
				condition:     unschedulableCond(recent.Add(-1 * time.Second)),
				predicateTime: nanos(recent),
			}),
			want: true,
		},
		{
			name: "predicate-time older than condition: cycle rejected after Filter, do not count",
			pod: makePod(podOpts{
				condition:     unschedulableCond(recent),
				predicateTime: nanos(aged),
			}),
			want: false,
		},
		{
			name: "same-second boundary conservatively treated as condition newer: do not count",
			pod: makePod(podOpts{
				condition:     unschedulableCond(recent),
				predicateTime: nanos(recent.Add(500 * time.Millisecond)),
			}),
			want: false,
		},
		{
			name: "malformed predicate-time: do not count",
			pod: makePod(podOpts{
				condition:     unschedulableCond(aged),
				predicateTime: "not-a-number",
			}),
			want: false,
		},
		{
			name: "predicate-time newer than LTT but older than bind grace: stuck, do not count",
			pod: makePod(podOpts{
				// predicateTime > LTT (Filter ran after the condition was set),
				// yet StuckGracePeriod has elapsed without LTT advancing — bind
				// would have completed by now, so the pre-allocation is stale.
				condition:     unschedulableCond(aged.Add(-1 * time.Second)),
				predicateTime: nanos(aged),
			}),
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ShouldCountPodDeviceAllocation(tt.pod))
		})
	}
}
