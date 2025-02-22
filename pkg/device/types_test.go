package device

import (
	"fmt"
	"testing"

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
		devices PodDevices
		want    string
	}{
		{
			name: "example 1: single container, single device",
			devices: PodDevices{
				{
					Name: "cont1",
					Devices: []ClaimDevice{
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
			devices: PodDevices{
				{
					Name:    "cont1",
					Devices: []ClaimDevice{},
				},
			},
			want: fmt.Sprintf("cont1[]"),
		},
		{
			name: "example 3: single container, multiple device",
			devices: PodDevices{
				{
					Name: "cont1",
					Devices: []ClaimDevice{
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
			devices: PodDevices{
				{
					Name: "cont1",
					Devices: []ClaimDevice{
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
					Devices: []ClaimDevice{
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
			devices: PodDevices{
				{
					Name: "cont1",
					Devices: []ClaimDevice{
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
					Devices: []ClaimDevice{
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
			podDevices := PodDevices{}
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
		want *ContainerDevices
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
			want: &ContainerDevices{
				Name: "cont1",
				Devices: []ClaimDevice{
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
			want: &ContainerDevices{
				Name: "cont2",
				Devices: []ClaimDevice{
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
			want: &ContainerDevices{
				Name: "cont2",
				Devices: []ClaimDevice{
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
