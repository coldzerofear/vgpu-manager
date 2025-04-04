package util

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
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
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			got := CheckDeviceUuid(testCase.annotations, testCase.cardUuid)
			assert.Equal(t, testCase.want, got)
		})
	}
}

func Test_parseNvidiaNumaInfo(t *testing.T) {
	tests := []struct {
		name          string
		idx           int
		nvidiaTopoStr string
		want          int
		wantErr       bool
	}{
		{
			name: "single Tesla P4 NUMA",
			idx:  0,
			nvidiaTopoStr: `GPU0    CPU Affinity    NUMA Affinity ...
                            ...`,
			want:    0,
			wantErr: false,
		}, {
			name: "two Tesla P4 NUMA topo with index 0",
			idx:  0,
			nvidiaTopoStr: `GPU0    GPU1    CPU Affinity    NUMA Affinity ...
                            ...`,
			want:    0,
			wantErr: false,
		}, {
			name: "two Tesla P4 NUMA topo with index 1",
			idx:  1,
			nvidiaTopoStr: `GPU0    GPU1    CPU Affinity    NUMA Affinity ...
                            ...`,
			want:    0,
			wantErr: false,
		}, {
			name: "NUMA Affinity is empty",
			idx:  0,
			nvidiaTopoStr: `GPU0	CPU Affinity	NUMA Affinity	GPU NUMA ID
GPU0	X`,
			want:    0,
			wantErr: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseNvidiaNumaInfo(test.idx, test.nvidiaTopoStr)
			if (err != nil) != test.wantErr {
				t.Errorf("parseNvidiaNumaInfo() error = %v, wantErr %v", err, test.wantErr)
				return
			}
			if got != test.want {
				t.Errorf("parseNvidiaNumaInfo() got = %v, want %v", got, test.want)
			}
		})
	}
}
