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
			cardType: "NVIDIA-NVIDIA GeForce RTX 3080 Ti",
			annotations: map[string]string{
				PodIncludeGpuTypeAnnotation: "RTX 4090,RTX 3080",
			},
			want: true,
		}, {
			name:        "example 7: empty annotations",
			cardType:    "NVIDIA-NVIDIA GeForce RTX 3080 Ti",
			annotations: nil,
			want:        true,
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
			assert.Equal(t, tt.want, PodIsGangMember(tt.pod))
		})
	}
}

func assertDNS1123Compatibility(t *testing.T, name string) {
	dns1123FormatRegexp := regexp.MustCompile("^[a-z0-9]([-a-z0-9]*[a-z0-9])?$")
	assert.True(t, len(name) <= DNS1123NameMaximumLength, "Name length needs to be shorter than %d", DNS1123NameMaximumLength)
	assert.Regexp(t, dns1123FormatRegexp, name, "Name needs to be in DNS-1123 allowed format")
}
