package vgpu

import (
	"os"
	"syscall"
	"testing"

	dpoptions "github.com/coldzerofear/vgpu-manager/cmd/device-plugin/options"
	"github.com/coldzerofear/vgpu-manager/pkg/config/node"
	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/device/manager"
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
	option := node.MutationDPOptions(dpoptions.Options{
		NodeName:            "testNode",
		DeviceMemoryScaling: float64(1),
	})
	config, _ := node.NewNodeConfig("", option)
	gpuUUID0 := "GPU-" + string(uuid.NewUUID())
	gpuUUID1 := "GPU-" + string(uuid.NewUUID())
	devices := []*manager.GPUDevice{
		{
			GPUInfo: device.GPUInfo{
				Id:      0,
				Uuid:    gpuUUID0,
				Core:    100,
				Memory:  12288,
				Type:    "Nvidia RTX 3080Ti",
				Number:  10,
				Numa:    0,
				Healthy: true,
			},
			MinorNumber: 0,
		}, {
			GPUInfo: device.GPUInfo{
				Id:      1,
				Uuid:    gpuUUID1,
				Core:    100,
				Memory:  12288,
				Type:    "Nvidia RTX 3080Ti",
				Number:  10,
				Numa:    0,
				Healthy: true,
			},
			MinorNumber: 1,
		},
	}
	devManager := manager.NewFakeDeviceManager(config, driverVersion, devices)
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
						Uuid:   gpuUUID0,
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
						Uuid:   gpuUUID0,
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
						Uuid:   gpuUUID0,
						Core:   20,
						Memory: 1024,
					},
					{
						Id:     1,
						Uuid:   gpuUUID1,
						Core:   30,
						Memory: 2048,
					},
				},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := WriteVGPUConfigFile(test.path, devManager, test.pod, test.devices)
			if err != nil {
				t.Error(err)
			}
			resourceData1, data, err := MmapResourceDataT(test.path)
			if err != nil {
				t.Error(err)
			}
			defer syscall.Munmap(data)
			resourceData2 := NewResourceDataT(devManager, test.pod, test.devices)
			assert.Equal(t, *resourceData1, *resourceData2)
			if err = os.RemoveAll(test.path); err != nil {
				t.Error(err)
			}
		})
	}
}
