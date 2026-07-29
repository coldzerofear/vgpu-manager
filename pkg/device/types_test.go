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
	// tests must position predicateTime relative to "now". Truncate to a whole
	// second so adding sub-second offsets stays in a deterministic Unix-second
	// bucket — the same-second-boundary case below depends on it.
	now := time.Now().Truncate(time.Second)
	// recent = inside StuckGracePeriod (treat as bind window)
	// aged   = outside StuckGracePeriod (treat as stuck)
	recent := now.Add(-1 * time.Second)
	aged := now.Add(-2 * StuckGracePeriod)

	type podOpts struct {
		nodeName       string
		condition      *corev1.PodCondition
		predicateTime  string // empty = annotation absent
		assignedPhase  string // empty = label absent
		stuckGraceAnno string // empty = annotation absent (per-pod override)
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
		if o.stuckGraceAnno != "" {
			pod.Annotations[util.SchedulerStuckGracePeriodAnnotation] = o.stuckGraceAnno
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
		// Per-pod stuck-grace-period annotation override.
		// The gang-scheduling motivation: a gang pod can carry an annotation
		// whose duration exceeds the gang's Permit timeout so legitimate
		// Permit-wait does not get misclassified as stuck.
		{
			name: "annotation extends grace beyond default: aged but still within custom grace → count",
			pod: makePod(podOpts{
				// Same shape as the "stuck" case above (predicateTime ~ 2*default
				// grace ago) but with a per-pod annotation of 10× the default.
				// time.Since(aged) ≈ 60s; 10×default = 300s; 60 < 300 → counted.
				condition:      unschedulableCond(aged.Add(-1 * time.Second)),
				predicateTime:  nanos(aged),
				stuckGraceAnno: (10 * StuckGracePeriod).String(),
			}),
			want: true,
		},
		{
			name: "annotation below 1s minimum is rejected, default grace applies → recent allocation counted",
			pod: makePod(podOpts{
				// recent ≈ 1s ago. With default 30s grace → counted.
				// Annotation 500ms is below the helper's 1-second minimum
				// (enforced in ShouldCountPodDeviceAllocation), so the
				// override is rejected and default 30s applies → counted.
				condition:      unschedulableCond(recent.Add(-1 * time.Second)),
				predicateTime:  nanos(recent),
				stuckGraceAnno: "500ms",
			}),
			want: true,
		},
		{
			name: "annotation override exactly at 1s minimum is honored",
			pod: makePod(podOpts{
				// predicateTime aged ~60s ago, override = 1s → 60s > 1s → stuck.
				condition:      unschedulableCond(aged.Add(-1 * time.Second)),
				predicateTime:  nanos(aged),
				stuckGraceAnno: "1s",
			}),
			want: false,
		},
		{
			name: "malformed annotation falls back to default grace",
			pod: makePod(podOpts{
				// Same as "stuck" baseline (aged → would be stuck under default).
				// Bad annotation must NOT silently extend grace — fall back to
				// default → stuck classification preserved.
				condition:      unschedulableCond(aged.Add(-1 * time.Second)),
				predicateTime:  nanos(aged),
				stuckGraceAnno: "not-a-duration",
			}),
			want: false,
		},
		{
			name: "annotation set but pod is bound: NodeName check short-circuits, override never consulted",
			pod: makePod(podOpts{
				nodeName:       "node-1",
				condition:      unschedulableCond(aged),
				predicateTime:  nanos(aged),
				stuckGraceAnno: "1ms", // would normally force stuck, but NodeName wins
			}),
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ShouldCountPodDeviceAllocation(tt.pod))
		})
	}
}

// naivePerClaimSum is the historical accounting: every container claim sums
// onto its GPU regardless of container lifecycle. ReducePodFootprint must
// equal this for any pod without a vGPU init container.
func naivePerClaimSum(claim PodDeviceClaim) map[string]PodDeviceFootprint {
	m := make(map[string]PodDeviceFootprint)
	for _, cc := range claim {
		for _, dc := range cc.DeviceClaims {
			f := m[dc.Uuid]
			f.Id, f.Uuid = dc.Id, dc.Uuid
			f.Number++
			f.Cores += dc.Cores
			f.Memory += dc.Memory
			m[dc.Uuid] = f
		}
	}
	return m
}

func Test_ReducePodFootprint(t *testing.T) {
	const g0, g1 = "GPU-0000", "GPU-1111"
	restartAlways := corev1.ContainerRestartPolicyAlways

	// initContainers builds a pod whose InitContainers carry only the names
	// (and optional restart policy) ReducePodFootprint needs for classifying
	// claims; regular containers are implicit (any claim whose container name
	// is not a sequential init container counts as concurrent).
	type initSpec struct {
		name        string
		restartable bool
	}
	makePodWithInit := func(inits ...initSpec) *corev1.Pod {
		pod := &corev1.Pod{}
		for _, is := range inits {
			c := corev1.Container{Name: is.name}
			if is.restartable {
				c.RestartPolicy = &restartAlways
			}
			pod.Spec.InitContainers = append(pod.Spec.InitContainers, c)
		}
		return pod
	}

	tests := []struct {
		name      string
		pod       *corev1.Pod
		claim     PodDeviceClaim
		want      map[string]PodDeviceFootprint
		appOnlyEq bool // also assert equality with naive per-claim sum
	}{
		{
			name: "app-only single container single device == sum",
			pod:  &corev1.Pod{},
			claim: PodDeviceClaim{
				{Name: "app", DeviceClaims: []DeviceClaim{{Id: 0, Uuid: g0, Cores: 50, Memory: 1000}}},
			},
			want: map[string]PodDeviceFootprint{
				g0: {Id: 0, Uuid: g0, Number: 1, Cores: 50, Memory: 1000},
			},
			appOnlyEq: true,
		},
		{
			name: "app-only two containers share one GPU == sum",
			pod:  &corev1.Pod{},
			claim: PodDeviceClaim{
				{Name: "app1", DeviceClaims: []DeviceClaim{{Id: 0, Uuid: g0, Cores: 30, Memory: 1000}}},
				{Name: "app2", DeviceClaims: []DeviceClaim{{Id: 0, Uuid: g0, Cores: 40, Memory: 2000}}},
			},
			want: map[string]PodDeviceFootprint{
				g0: {Id: 0, Uuid: g0, Number: 2, Cores: 70, Memory: 3000},
			},
			appOnlyEq: true,
		},
		{
			name: "sequential init reuses app GPU, init smaller -> app sum wins",
			pod:  makePodWithInit(initSpec{name: "init"}),
			claim: PodDeviceClaim{
				{Name: "init", DeviceClaims: []DeviceClaim{{Id: 0, Uuid: g0, Cores: 20, Memory: 500}}},
				{Name: "app", DeviceClaims: []DeviceClaim{{Id: 0, Uuid: g0, Cores: 60, Memory: 4000}}},
			},
			want: map[string]PodDeviceFootprint{
				g0: {Id: 0, Uuid: g0, Number: 1, Cores: 60, Memory: 4000},
			},
		},
		{
			name: "sequential init reuses app GPU, init bigger memory -> per-dim max",
			pod:  makePodWithInit(initSpec{name: "init"}),
			claim: PodDeviceClaim{
				{Name: "init", DeviceClaims: []DeviceClaim{{Id: 0, Uuid: g0, Cores: 20, Memory: 8000}}},
				{Name: "app", DeviceClaims: []DeviceClaim{{Id: 0, Uuid: g0, Cores: 60, Memory: 4000}}},
			},
			// number=max(1,1)=1, cores=max(60,20)=60, memory=max(4000,8000)=8000
			want: map[string]PodDeviceFootprint{
				g0: {Id: 0, Uuid: g0, Number: 1, Cores: 60, Memory: 8000},
			},
		},
		{
			name: "sequential init on a different GPU than app -> both reserved",
			pod:  makePodWithInit(initSpec{name: "init"}),
			claim: PodDeviceClaim{
				{Name: "init", DeviceClaims: []DeviceClaim{{Id: 1, Uuid: g1, Cores: 100, Memory: 16000}}},
				{Name: "app", DeviceClaims: []DeviceClaim{{Id: 0, Uuid: g0, Cores: 50, Memory: 4000}}},
			},
			want: map[string]PodDeviceFootprint{
				g0: {Id: 0, Uuid: g0, Number: 1, Cores: 50, Memory: 4000},
				g1: {Id: 1, Uuid: g1, Number: 1, Cores: 100, Memory: 16000},
			},
		},
		{
			name: "init-only pod -> footprint is the init claim",
			pod:  makePodWithInit(initSpec{name: "init"}),
			claim: PodDeviceClaim{
				{Name: "init", DeviceClaims: []DeviceClaim{{Id: 0, Uuid: g0, Cores: 30, Memory: 2000}}},
			},
			want: map[string]PodDeviceFootprint{
				g0: {Id: 0, Uuid: g0, Number: 1, Cores: 30, Memory: 2000},
			},
		},
		{
			name: "two sequential init containers share one GPU -> max, slot saturates at 1",
			pod:  makePodWithInit(initSpec{name: "init1"}, initSpec{name: "init2"}),
			claim: PodDeviceClaim{
				{Name: "init1", DeviceClaims: []DeviceClaim{{Id: 0, Uuid: g0, Cores: 30, Memory: 2000}}},
				{Name: "init2", DeviceClaims: []DeviceClaim{{Id: 0, Uuid: g0, Cores: 70, Memory: 1000}}},
			},
			// sequential inits never overlap: number=1, cores=max(30,70)=70, memory=max(2000,1000)=2000
			want: map[string]PodDeviceFootprint{
				g0: {Id: 0, Uuid: g0, Number: 1, Cores: 70, Memory: 2000},
			},
		},
		{
			name: "restartable init (sidecar) counts as concurrent -> sums with app",
			pod:  makePodWithInit(initSpec{name: "sidecar", restartable: true}),
			claim: PodDeviceClaim{
				{Name: "sidecar", DeviceClaims: []DeviceClaim{{Id: 0, Uuid: g0, Cores: 20, Memory: 1000}}},
				{Name: "app", DeviceClaims: []DeviceClaim{{Id: 0, Uuid: g0, Cores: 50, Memory: 4000}}},
			},
			// sidecar is concurrent: number=2, cores=70, memory=5000
			want: map[string]PodDeviceFootprint{
				g0: {Id: 0, Uuid: g0, Number: 2, Cores: 70, Memory: 5000},
			},
		},
		{
			name: "sidecar + sequential init: sidecar sums, init maxes",
			pod:  makePodWithInit(initSpec{name: "sidecar", restartable: true}, initSpec{name: "init"}),
			claim: PodDeviceClaim{
				{Name: "sidecar", DeviceClaims: []DeviceClaim{{Id: 0, Uuid: g0, Cores: 20, Memory: 1000}}},
				{Name: "init", DeviceClaims: []DeviceClaim{{Id: 0, Uuid: g0, Cores: 90, Memory: 3000}}},
				{Name: "app", DeviceClaims: []DeviceClaim{{Id: 0, Uuid: g0, Cores: 50, Memory: 4000}}},
			},
			// reserve = sidecarSum + max(regularSum, initMax), per dimension.
			// sidecarSum={1,20,1000} regularSum(app)={1,50,4000} initMax={1,90,3000}
			// number = 1 + max(1,1) = 2
			// cores  = 20 + max(50,90) = 110  (init phase sidecar+init=110 > app phase 70)
			// memory = 1000 + max(4000,3000) = 5000
			want: map[string]PodDeviceFootprint{
				g0: {Id: 0, Uuid: g0, Number: 2, Cores: 110, Memory: 5000},
			},
		},
		{
			// Robustness: a single sequential init container's own claims SUM
			// per GPU (within one running container), they are not max'd. The
			// allocator's distinct-card rule makes two same-UUID claims from one
			// container not happen today, but the reducer must not silently
			// undercount if it ever does (the old per-claim cap-at-1 would).
			name: "one sequential init container, two claims on same GPU -> within-container sum",
			pod:  makePodWithInit(initSpec{name: "init"}),
			claim: PodDeviceClaim{
				{Name: "init", DeviceClaims: []DeviceClaim{
					{Id: 0, Uuid: g0, Cores: 30, Memory: 1000},
					{Id: 0, Uuid: g0, Cores: 40, Memory: 2000},
				}},
			},
			want: map[string]PodDeviceFootprint{
				g0: {Id: 0, Uuid: g0, Number: 2, Cores: 70, Memory: 3000},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ReducePodFootprint(tt.pod, tt.claim)
			assert.Equal(t, tt.want, got)
			if tt.appOnlyEq {
				assert.Equal(t, naivePerClaimSum(tt.claim), got,
					"app-only footprint must equal the historical per-claim sum")
			}
		})
	}
}

func Test_CurrentSharedContainers(t *testing.T) {
	const g0, g1 = "GPU-0000", "GPU-1111"
	running := func(name string) corev1.ContainerStatus {
		return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}
	}
	terminated := func(name string) corev1.ContainerStatus {
		return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}}
	}
	// podStatus builds a pod carrying the given init/app container statuses.
	podStatus := func(inits, apps []corev1.ContainerStatus) *corev1.Pod {
		return &corev1.Pod{Status: corev1.PodStatus{
			InitContainerStatuses: inits,
			ContainerStatuses:     apps,
		}}
	}

	tests := []struct {
		name  string
		pod   *corev1.Pod
		claim PodDeviceClaim
		want  map[string]int
	}{
		{
			name: "single running app container",
			pod:  podStatus(nil, []corev1.ContainerStatus{running("app")}),
			claim: PodDeviceClaim{
				{Name: "app", DeviceClaims: []DeviceClaim{{Uuid: g0}}},
			},
			want: map[string]int{g0: 1},
		},
		{
			name: "completed sequential init is excluded; only running app counts",
			pod:  podStatus([]corev1.ContainerStatus{terminated("init")}, []corev1.ContainerStatus{running("app")}),
			claim: PodDeviceClaim{
				{Name: "init", DeviceClaims: []DeviceClaim{{Uuid: g0}}},
				{Name: "app", DeviceClaims: []DeviceClaim{{Uuid: g0}}},
			},
			want: map[string]int{g0: 1},
		},
		{
			name: "init phase: init running, app not started yet",
			pod:  podStatus([]corev1.ContainerStatus{running("init")}, []corev1.ContainerStatus{terminated("app")}),
			claim: PodDeviceClaim{
				{Name: "init", DeviceClaims: []DeviceClaim{{Uuid: g0}}},
				{Name: "app", DeviceClaims: []DeviceClaim{{Uuid: g0}}},
			},
			want: map[string]int{g0: 1},
		},
		{
			name: "running sidecar + running app share a GPU -> 2",
			pod:  podStatus([]corev1.ContainerStatus{running("side")}, []corev1.ContainerStatus{running("app")}),
			claim: PodDeviceClaim{
				{Name: "side", DeviceClaims: []DeviceClaim{{Uuid: g0}}},
				{Name: "app", DeviceClaims: []DeviceClaim{{Uuid: g0}}},
			},
			want: map[string]int{g0: 2},
		},
		{
			name: "container with no status is not counted",
			pod:  podStatus(nil, nil),
			claim: PodDeviceClaim{
				{Name: "app", DeviceClaims: []DeviceClaim{{Uuid: g0}}},
			},
			want: map[string]int{},
		},
		{
			name: "two running apps on different GPUs",
			pod:  podStatus(nil, []corev1.ContainerStatus{running("app1"), running("app2")}),
			claim: PodDeviceClaim{
				{Name: "app1", DeviceClaims: []DeviceClaim{{Uuid: g0}}},
				{Name: "app2", DeviceClaims: []DeviceClaim{{Uuid: g1}}},
			},
			want: map[string]int{g0: 1, g1: 1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CurrentSharedContainers(tt.pod, tt.claim)
			assert.Equal(t, tt.want, got)
			// Invariant: current sharing never exceeds the peak footprint.
			peak := ReducePodFootprint(tt.pod, tt.claim)
			for uuid, cur := range got {
				assert.LessOrEqualf(t, cur, peak[uuid].Number,
					"current shared containers (%d) on %s must be <= peak (%d)", cur, uuid, peak[uuid].Number)
			}
		})
	}
}
