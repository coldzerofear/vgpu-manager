/*
Copyright 2024-2026 coldzerofear

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
	"fmt"
	"regexp"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func Test_CheckDeviceType(t *testing.T) {
	testCases := []struct {
		name        string
		cardType    string
		annotations map[string]string
		want        bool
	}{
		{
			name:     "example 1: match GPU type",
			cardType: "NVIDIA A10",
			annotations: map[string]string{
				PodIncludeGpuTypeAnnotation: "A10",
			},
			want: true,
		}, {
			name:     "example 2: no match GPU type",
			cardType: "NVIDIA A100-SXM4-40GB",
			annotations: map[string]string{
				PodIncludeGpuTypeAnnotation: "3080Ti",
			},
			want: false,
		}, {
			name:     "example 3: no match GPU type",
			cardType: "NVIDIA A100-SXM4-40GB",
			annotations: map[string]string{
				PodIncludeGpuTypeAnnotation: "NVIDIA A10",
				PodExcludeGpuTypeAnnotation: "NVIDIA A100",
			},
			want: false,
		}, {
			name:     "example 4: no match GPU type",
			cardType: "NVIDIA A100-SXM4-80GB",
			annotations: map[string]string{
				PodIncludeGpuTypeAnnotation: "NVIDIA A100-SXM4-40GB",
			},
			want: false,
		}, {
			name:     "example 5: no match GPU type",
			cardType: "NVIDIA A100-SXM4-80GB",
			annotations: map[string]string{
				PodExcludeGpuTypeAnnotation: "NVIDIA A100",
			},
			want: false,
		}, {
			name:     "example 6: match GPU type",
			cardType: "NVIDIA GeForce RTX 3080 Ti",
			annotations: map[string]string{
				PodIncludeGpuTypeAnnotation: "RTX 4090,RTX 3080",
			},
			want: true,
		}, {
			name:        "example 7: empty annotations",
			cardType:    "NVIDIA GeForce RTX 3080 Ti",
			annotations: nil,
			want:        true,
		}, {
			name:     "example 8: not case sensitive",
			cardType: "NVIDIA A100-SXM4-80GB",
			annotations: map[string]string{
				PodIncludeGpuTypeAnnotation: "a100",
			},
			want: true,
		}, {
			name:     "example 9: empty string",
			cardType: "NVIDIA A100-SXM4-80GB",
			annotations: map[string]string{
				PodIncludeGpuTypeAnnotation: "",
			},
			want: true,
		}, {
			name:     "example 10: space string",
			cardType: "NVIDIA A100-SXM4-80GB",
			annotations: map[string]string{
				PodExcludeGpuTypeAnnotation: "   ",
			},
			want: true,
		}, {
			name:     "example 11: trailing comma",
			cardType: "NVIDIA A100-SXM4-80GB",
			annotations: map[string]string{
				PodIncludeGpuTypeAnnotation: "V100,",
			},
			want: false,
		}, {
			name:     "example 12: prefix comma",
			cardType: "NVIDIA A100-SXM4-80GB",
			annotations: map[string]string{
				PodExcludeGpuTypeAnnotation: ",V100",
			},
			want: true,
		}, {
			name:     "example 13: trailing comma",
			cardType: "NVIDIA A100-SXM4-80GB",
			annotations: map[string]string{
				PodExcludeGpuTypeAnnotation: "V100,",
			},
			want: true,
		}, {
			// An include list with nothing usable in it means "no constraint",
			// not "reject everything" — a stray space must not make the Pod
			// unschedulable on every node.
			name:     "example 14: include with only blank entries",
			cardType: "NVIDIA A100-SXM4-80GB",
			annotations: map[string]string{
				PodIncludeGpuTypeAnnotation: "   ",
			},
			want: true,
		}, {
			name:     "example 15: include with only commas",
			cardType: "NVIDIA A100-SXM4-80GB",
			annotations: map[string]string{
				PodIncludeGpuTypeAnnotation: ",,",
			},
			want: true,
		}, {
			name:     "example 16: include matches, exclude also matches",
			cardType: "NVIDIA A100-SXM4-80GB",
			annotations: map[string]string{
				PodIncludeGpuTypeAnnotation: "A100",
				PodExcludeGpuTypeAnnotation: "A100",
			},
			want: false,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			got := CheckDeviceType(testCase.annotations, testCase.cardType)
			assert.Equal(t, testCase.want, got)
		})
	}
}

func Test_CheckDeviceUuid(t *testing.T) {
	gpu0Uuid := "GPU-" + uuid.New().String()
	testCases := []struct {
		name        string
		cardUuid    string
		annotations map[string]string
		want        bool
	}{
		{
			name:     "example 1: match GPU uuid",
			cardUuid: gpu0Uuid,
			annotations: map[string]string{
				PodIncludeGPUUUIDAnnotation: gpu0Uuid,
			},
			want: true,
		}, {
			name:     "example 2: no match GPU uuid",
			cardUuid: gpu0Uuid,
			annotations: map[string]string{
				PodIncludeGPUUUIDAnnotation: "GPU-" + uuid.New().String(),
			},
			want: false,
		}, {
			name:     "example 3: match GPU uuid",
			cardUuid: gpu0Uuid,
			annotations: map[string]string{
				PodIncludeGPUUUIDAnnotation: "GPU-" + uuid.New().String() + "," + gpu0Uuid,
			},
			want: true,
		}, {
			name:     "example 4: no match GPU uuid",
			cardUuid: gpu0Uuid,
			annotations: map[string]string{
				PodExcludeGPUUUIDAnnotation: gpu0Uuid,
			},
			want: false,
		}, {
			name:     "example 5: no match GPU uuid",
			cardUuid: gpu0Uuid,
			annotations: map[string]string{
				PodExcludeGPUUUIDAnnotation: "GPU-" + uuid.New().String() + "," + gpu0Uuid,
			},
			want: false,
		}, {
			name:        "example 6: empty annotations",
			cardUuid:    gpu0Uuid,
			annotations: nil,
			want:        true,
		}, {
			name:     "example 7: empty string",
			cardUuid: gpu0Uuid,
			annotations: map[string]string{
				PodIncludeGPUUUIDAnnotation: "",
			},
			want: true,
		}, {
			name:     "example 8: space string",
			cardUuid: gpu0Uuid,
			annotations: map[string]string{
				PodIncludeGPUUUIDAnnotation: "   ",
			},
			want: true,
		}, {
			name:     "example 9: trailing comma",
			cardUuid: gpu0Uuid,
			annotations: map[string]string{
				PodIncludeGPUUUIDAnnotation: gpu0Uuid + ",",
			},
			want: true,
		}, {
			name:     "example 10: prefix comma",
			cardUuid: gpu0Uuid,
			annotations: map[string]string{
				PodIncludeGPUUUIDAnnotation: "," + gpu0Uuid,
			},
			want: true,
		}, {
			name:     "example 11: trailing comma",
			cardUuid: gpu0Uuid,
			annotations: map[string]string{
				PodExcludeGPUUUIDAnnotation: gpu0Uuid + ",",
			},
			want: false,
		}, {
			name:     "example 12: prefix comma",
			cardUuid: gpu0Uuid,
			annotations: map[string]string{
				PodExcludeGPUUUIDAnnotation: "," + gpu0Uuid,
			},
			want: false,
		}, {
			name:     "example 13: exclude with only blank entries",
			cardUuid: gpu0Uuid,
			annotations: map[string]string{
				PodExcludeGPUUUIDAnnotation: "   ",
			},
			want: true,
		}, {
			// Both filters apply. Earlier releases returned as soon as the include
			// list was consulted, which silently ignored the exclude list.
			name:     "example 14: include matches, exclude also matches",
			cardUuid: gpu0Uuid,
			annotations: map[string]string{
				PodIncludeGPUUUIDAnnotation: gpu0Uuid,
				PodExcludeGPUUUIDAnnotation: gpu0Uuid,
			},
			want: false,
		}, {
			name:     "example 15: include matches, exclude names another device",
			cardUuid: gpu0Uuid,
			annotations: map[string]string{
				PodIncludeGPUUUIDAnnotation: gpu0Uuid,
				PodExcludeGPUUUIDAnnotation: "GPU-" + uuid.New().String(),
			},
			want: true,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			got := CheckDeviceUuid(testCase.annotations, testCase.cardUuid)
			assert.Equal(t, testCase.want, got)
		})
	}
}

func Test_MakeDeviceID(t *testing.T) {
	testCases1 := []struct {
		gpuId, i int64
	}{
		{0, 0},
		{15, 10000},
		{255, 1000000},
	}
	t.Run("encoding and decoding", func(t *testing.T) {
		for _, tc := range testCases1 {
			id := MakeDeviceID(tc.gpuId, tc.i)
			gpuId, i, err := ParseDeviceID(id)
			if err != nil {
				t.Fatal(err)
			}
			assert.Equal(t, tc.gpuId, gpuId)
			assert.Equal(t, tc.i, i)
		}
	})

	t.Run("small space exhaustive", func(t *testing.T) {
		set := make(map[string]struct{})
		for gpuId := 0; gpuId < 4; gpuId++ {
			for i := 0; i < 1000; i++ {
				id := MakeDeviceID(int64(gpuId), int64(i))
				if _, exists := set[id]; exists {
					t.Fatalf("duplicate: gpuId=%d, i=%d", gpuId, i)
				}
				set[id] = struct{}{}
			}
		}
	})

	t.Run("round-trip and uniqueness", func(t *testing.T) {
		testCases := [][2]int64{
			{0, 0}, {1, 0}, {255, 0},
			{0, 1}, {1, 1}, {255, 1},
			{0, 12345}, {128, 99999}, {255, 1048575},
		}

		seen := make(map[string]struct{})

		for _, tc := range testCases {
			gpuId, i := tc[0], tc[1]
			id := MakeDeviceID(gpuId, i)
			if _, exists := seen[id]; exists {
				t.Fatalf("Duplicate ID: %s for (%d,%d)", id, gpuId, i)
			}
			seen[id] = struct{}{}
			parsedGpuId, parsedI, err := ParseDeviceID(id)
			if err != nil {
				t.Fatalf("Parse failed: %v", err)
			}
			assert.Equal(t, gpuId, parsedGpuId)
			assert.Equal(t, i, parsedI)
		}
	})

	t.Run("boundary cases", func(t *testing.T) {
		cases := []struct{ gpuId, i int64 }{
			{0, 0},
			{255, 0},
			{0, 1},
			{255, 1},
			{0, 1048575},
			{15, 1048575},
		}
		set := make(map[string]struct{})
		for _, tc := range cases {
			id := MakeDeviceID(tc.gpuId, tc.i)
			if _, exists := set[id]; exists {
				t.Fatalf("boundary duplicate: %v", tc)
			}
			set[id] = struct{}{}
		}
	})
}

func Test_GenerateK8sSafeResourceName(t *testing.T) {
	testCases := []struct {
		inputs []string
	}{
		{inputs: []string{"default", "test1.test1"}},
		{inputs: []string{"default", "ddssaawdddddddsadwwwwwww", "--"}},
		{inputs: []string{"---", "default", "ddssaawdddddddsadwwwwwww.test1"}},
		{inputs: []string{"default", "test1.test1", ".", "----"}},
		{inputs: []string{"default", "test1.test1..test1..test1..test1", ".", "--.test1.test1"}},
		{inputs: []string{"default", uuid.NewString(), uuid.NewString()}},
		{inputs: []string{"1", "1"}},
	}
	for i, test := range testCases {
		t.Run(fmt.Sprintf("example %d", i+1), func(t *testing.T) {
			name := GenerateK8sSafeResourceName(test.inputs...)
			fmt.Println(name)
			assertDNS1123Compatibility(t, name)
		})
	}
}

func Test_MakeDNS1123Compatible(t *testing.T) {
	examples := []struct {
		name     string
		expected string
	}{
		{
			name:     "Pinco.Pallo-kubeworld.it-clientconfig",
			expected: "pincopallo-kubeworldit-clientconfig",
		},
		{
			name:     "tOk3_?ofTHE-Year",
			expected: "tok3ofthe-year",
		},
		{
			name:     "----tOk3_?ofTHE-YEAR!",
			expected: "tok3ofthe-year",
		},
		{
			name:     "tOk3_?ofTHE-YEAR--",
			expected: "tok3ofthe-year",
		},
	}

	for _, example := range examples {
		t.Run(example.name, func(t *testing.T) {
			name := MakeDNS1123Compatible(example.name)

			assert.Equal(t, example.expected, name)
			assertDNS1123Compatibility(t, name)
		})
	}
}

// Test_PodIsGangMember covers every signal PodIsGangMember consults to
// recognize a pod as part of a gang/PodGroup. Each subtest isolates one
// signal so a regression on any single detection path surfaces as a single
// failed case, not a confused composite.
//
// NOTE: the pod.Spec.SchedulingGroup path (native upstream gang scheduling
// API) is intentionally NOT exercised as a positive case here because the
// surrounding helper struct's exported name has not been pinned in this
// test environment. The negative coverage (default empty PodSpec → no
// SchedulingGroup → recognized as non-gang via the other-signal cases)
// still validates the branch isn't accidentally true. Add a positive case
// once the upstream type is locally available; the production code already
// guards with `pod.Spec.SchedulingGroup != nil &&
// pod.Spec.SchedulingGroup.PodGroupName != nil`.
func Test_PodIsGangMember(t *testing.T) {
	mkPodWithLabel := func(key, value string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{key: value},
			},
		}
	}
	mkPodWithAnnotation := func(key, value string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{key: value},
			},
		}
	}
	mkPodWithOwner := func(kind string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				OwnerReferences: []metav1.OwnerReference{{
					Kind: kind, Name: "owner", APIVersion: "v1",
				}},
			},
		}
	}

	tests := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{
			name: "nil pod is not a gang member",
			pod:  nil,
			want: false,
		},
		{
			name: "bare pod (no labels/annotations/owner) is not a gang member",
			pod:  &corev1.Pod{},
			want: false,
		},
		{
			name: "coscheduling label (scheduler-plugins v1alpha1) marks pod as gang member",
			pod:  mkPodWithLabel(CoschedulingPodGroupLabel, "my-group"),
			want: true,
		},
		{
			name: "legacy coscheduling label (lightweight-coscheduling) marks pod as gang member",
			pod:  mkPodWithLabel(CoschedulingPodGroupNameLabel, "legacy-group"),
			want: true,
		},
		{
			name: "Volcano group annotation marks pod as gang member",
			pod:  mkPodWithAnnotation(VolcanoGroupNameAnnotation, "volcano-group"),
			want: true,
		},
		{
			name: "Koordinator gang annotation marks pod as gang member",
			pod:  mkPodWithAnnotation(KoordinatorGangNameAnnotation, "koord-gang"),
			want: true,
		},
		{
			name: "ownerReference Kind=PodGroup marks pod as gang member",
			pod:  mkPodWithOwner("PodGroup"),
			want: true,
		},
		{
			name: "ownerReference Kind=ReplicaSet is NOT a gang member",
			pod:  mkPodWithOwner("ReplicaSet"),
			want: false,
		},
		{
			name: "gang label present but empty value is NOT recognized as a member",
			pod:  mkPodWithLabel(CoschedulingPodGroupLabel, ""),
			want: false,
		},
		{
			name: "gang annotation present but empty value is NOT recognized as a member",
			pod:  mkPodWithAnnotation(VolcanoGroupNameAnnotation, ""),
			want: false,
		},
		{
			name: "unrelated label is NOT a gang signal",
			pod:  mkPodWithLabel("app", "frontend"),
			want: false,
		},
		{
			name: "unrelated annotation is NOT a gang signal",
			pod:  mkPodWithAnnotation("kubernetes.io/some-other", "value"),
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ok := PodHasGangName(tt.pod)
			assert.Equal(t, tt.want, ok)
		})
	}
}

func Test_PodGangKey(t *testing.T) {
	gangPodIn := func(namespace, gang string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: namespace,
				Labels:    map[string]string{CoschedulingPodGroupLabel: gang},
			},
		}
	}
	plainPod := func() *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "team-a",
				Labels:    map[string]string{"app": "frontend"},
			},
		}
	}

	// Annotation-based dialects are free-form, so the same gang can be spelled
	// several ways. All of them must fold onto one key, otherwise a gang splits
	// in two -- the mirror image of the cross-namespace collision.
	t.Run("every spelling folds onto the same key", func(t *testing.T) {
		annoPod := func(namespace, value string) *corev1.Pod {
			return &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace:   namespace,
					Annotations: map[string]string{VolcanoGroupNameAnnotation: value},
				},
			}
		}
		for _, spelling := range []string{
			"training",
			"team-a/training",
			"  team-a/training  ",
		} {
			key, ok := PodGangKey(annoPod("team-a", spelling))
			assert.True(t, ok, spelling)
			assert.Equal(t, "team-a/training", key, spelling)
		}
	})

	t.Run("an explicit foreign namespace is honoured", func(t *testing.T) {
		key, ok := PodGangKey(&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:   "team-b",
				Annotations: map[string]string{VolcanoGroupNameAnnotation: "team-a/training"},
			},
		})
		assert.True(t, ok)
		assert.Equal(t, "team-a/training", key)
	})

	t.Run("degenerate slash forms fall back to the pod namespace", func(t *testing.T) {
		for value, want := range map[string]string{
			"/training": "team-a/training",
			"training/": "team-a/training",
		} {
			key, ok := PodGangKey(&corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace:   "team-a",
					Annotations: map[string]string{VolcanoGroupNameAnnotation: value},
				},
			})
			assert.True(t, ok, value)
			assert.Equal(t, want, key, value)
		}
	})

	t.Run("qualifies the gang name with the namespace", func(t *testing.T) {
		key, ok := PodGangKey(gangPodIn("team-a", "training"))
		assert.True(t, ok)
		assert.Equal(t, "team-a/training", key)
	})

	t.Run("same gang name in two namespaces yields different keys", func(t *testing.T) {
		// The whole point: a PodGroup is namespaced, so the bare name is not a
		// cluster-unique identity and must never be used to decide sameness.
		a, okA := PodGangKey(gangPodIn("team-a", "training"))
		b, okB := PodGangKey(gangPodIn("team-b", "training"))
		assert.True(t, okA)
		assert.True(t, okB)
		assert.NotEqual(t, a, b)
	})

	t.Run("non-gang pod reports no key", func(t *testing.T) {
		key, ok := PodGangKey(plainPod())
		assert.False(t, ok)
		assert.Empty(t, key)
	})

	t.Run("nil pod reports no key", func(t *testing.T) {
		key, ok := PodGangKey(nil)
		assert.False(t, ok)
		assert.Empty(t, key)
	})

	t.Run("punctuation-only reference reports no key", func(t *testing.T) {
		for _, value := range []string{"/", "  "} {
			key, ok := PodGangKey(&corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace:   "team-a",
					Annotations: map[string]string{VolcanoGroupNameAnnotation: value},
				},
			})
			assert.False(t, ok, value)
			assert.Empty(t, key, value)
		}
	})
}

func assertDNS1123Compatibility(t *testing.T, name string) {
	dns1123FormatRegexp := regexp.MustCompile("^[a-z0-9]([-a-z0-9]*[a-z0-9])?$")
	assert.True(t, len(name) <= DNS1123NameMaximumLength, "Name length needs to be shorter than %d", DNS1123NameMaximumLength)
	assert.Regexp(t, dns1123FormatRegexp, name, "Name needs to be in DNS-1123 allowed format")
}

func Test_CollectableContainerNames(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways
	running := corev1.ContainerStatus{State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}
	terminated := corev1.ContainerStatus{State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}}
	withName := func(s corev1.ContainerStatus, name string) corev1.ContainerStatus { s.Name = name; return s }

	tests := []struct {
		name string
		pod  *corev1.Pod
		want []string
	}{
		{name: "nil pod", pod: nil, want: nil},
		{
			name: "app only",
			pod: &corev1.Pod{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "app"}},
			}},
			want: []string{"app"},
		},
		{
			name: "completed sequential init is excluded",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{{Name: "init"}},
					Containers:     []corev1.Container{{Name: "app"}},
				},
				Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{withName(terminated, "init")}},
			},
			want: []string{"app"},
		},
		{
			name: "running sequential init is included, init-first",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{{Name: "init"}},
					Containers:     []corev1.Container{{Name: "app"}},
				},
				Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{withName(running, "init")}},
			},
			want: []string{"init", "app"},
		},
		{
			name: "sidecar always included regardless of status",
			pod: &corev1.Pod{Spec: corev1.PodSpec{
				InitContainers: []corev1.Container{{Name: "side", RestartPolicy: &always}},
				Containers:     []corev1.Container{{Name: "app"}},
			}},
			want: []string{"side", "app"},
		},
		{
			name: "running init-only pod",
			pod: &corev1.Pod{
				Spec:   corev1.PodSpec{InitContainers: []corev1.Container{{Name: "init"}}},
				Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{withName(running, "init")}},
			},
			want: []string{"init"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, CollectableContainerNames(tt.pod))
		})
	}
}

func Test_BaseTopology(t *testing.T) {
	// Regression guard: the non-strict link/numa modes MUST map to themselves,
	// not collapse to none (a default-case bug once broke all plain link/numa
	// topology scheduling while link-strict kept working).
	cases := map[TopologyMode]TopologyMode{
		NoneTopology:          NoneTopology,
		"":                    NoneTopology,
		NUMATopology:          NUMATopology,
		NUMATopologyStrict:    NUMATopology,
		LinkTopology:          LinkTopology,
		LinkTopologyStrict:    LinkTopology,
		TopologyMode("bogus"): TopologyMode("bogus"),
	}
	for mode, want := range cases {
		if got := mode.BaseTopology(); got != want {
			t.Fatalf("(%q).BaseTopology() = %q, want %q", mode, got, want)
		}
	}
	// IsStrictTopology pairs with the -strict variants only.
	for _, m := range []TopologyMode{NUMATopologyStrict, LinkTopologyStrict} {
		if !m.IsStrictTopology() {
			t.Fatalf("(%q).IsStrictTopology() = false, want true", m)
		}
	}
	for _, m := range []TopologyMode{NoneTopology, NUMATopology, LinkTopology, "bogus"} {
		if m.IsStrictTopology() {
			t.Fatalf("(%q).IsStrictTopology() = true, want false", m)
		}
	}
}
