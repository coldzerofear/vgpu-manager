package vgpu

import (
	"os"
	"syscall"
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/version"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
)

func Test_WriteVGPUConfigFile(t *testing.T) {
	driverVersion := version.Version{
		DriverVersion: "",
		CudaVersion:   version.CudaVersion(12020),
	}
	tests := []struct {
		name    string
		path    string
		pod     *corev1.Pod
		devices device.ContainerDevices
	}{
		{
			name: "example 1",
			path: "/tmp/vgpu1.config",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test1",
					Namespace: "default",
					UID:       uuid.NewUUID(),
				},
			},
			devices: device.ContainerDevices{
				Name: "test",
				Devices: []device.ClaimDevice{
					{
						Id:     0,
						Uuid:   "GPU-" + string(uuid.NewUUID()),
						Core:   0,
						Memory: 1024,
					},
				},
			},
		},
		{
			name: "example 2",
			path: "/tmp/vgpu2.config",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test2",
					Namespace: "default",
					UID:       uuid.NewUUID(),
				},
			},
			devices: device.ContainerDevices{
				Name: "test",
				Devices: []device.ClaimDevice{
					{
						Id:     0,
						Uuid:   "GPU-" + string(uuid.NewUUID()),
						Core:   20,
						Memory: 1024,
					},
				},
			},
		},
		{
			name: "example 3",
			path: "/tmp/vgpu3.config",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test3",
					Namespace: "test",
					UID:       uuid.NewUUID(),
				},
			},
			devices: device.ContainerDevices{
				Name: "test",
				Devices: []device.ClaimDevice{
					{
						Id:     0,
						Uuid:   "GPU-" + string(uuid.NewUUID()),
						Core:   20,
						Memory: 1024,
					},
					{
						Id:     1,
						Uuid:   "GPU-" + string(uuid.NewUUID()),
						Core:   30,
						Memory: 2048,
					},
				},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := WriteVGPUConfigFile(test.path, driverVersion, test.pod, test.devices)
			if err != nil {
				t.Error(err)
			}
			resourceData1, data, err := MmapResourceDataT(test.path)
			if err != nil {
				t.Error(err)
			}
			defer syscall.Munmap(data)
			resourceData2 := NewResourceDataT(driverVersion, test.pod, test.devices)
			assert.Equal(t, *resourceData1, *resourceData2)
			err = os.RemoveAll(test.path)
			if err != nil {
				t.Error(err)
			}
		})
	}
}
