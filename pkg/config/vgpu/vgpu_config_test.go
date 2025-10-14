package vgpu

import (
	"os"
	"syscall"
	"testing"

	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/component-base/featuregate"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/coldzerofear/vgpu-manager/pkg/config/node"
	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/device/manager"
	"github.com/coldzerofear/vgpu-manager/pkg/device/nvidia"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/utils/ptr"
)

func Test_WriDriverConfigFile(t *testing.T) {
	driverVersion := nvidia.DriverVersion{
		DriverVersion:     "",
		CudaDriverVersion: nvidia.CudaDriverVersion(12020),
	}
	config, err := node.NewNodeConfig(func(spec *node.NodeConfigSpec) {
		spec.NodeName = "testNode"
		spec.DeviceCoresScaling = ptr.To(float64(1))
		spec.DeviceMemoryScaling = ptr.To(float64(1))
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	gpuUUID0 := "GPU-" + string(uuid.NewUUID())
	gpuUUID1 := "GPU-" + string(uuid.NewUUID())
	devices := []*manager.Device{
		{
			GPU: &manager.GPUDevice{
				GpuInfo: &nvidia.GpuInfo{
					Index: 0,
					UUID:  gpuUUID0,
					Minor: 0,
					Memory: nvml.Memory{
						Total: 12288 << 20,
					},
					ProductName: "Nvidia RTX 3080Ti",
				},
				NumaNode: 0,
				Healthy:  true,
			},
		}, {
			GPU: &manager.GPUDevice{
				GpuInfo: &nvidia.GpuInfo{
					Index: 1,
					UUID:  gpuUUID1,
					Minor: 1,
					Memory: nvml.Memory{
						Total: 12288 << 20,
					},
					ProductName: "Nvidia RTX 3080Ti",
				},
				NumaNode: 0,
				Healthy:  true,
			},
		},
	}
	nodes := []*corev1.Node{
		{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{util.VGPUComputePolicyAnnotation: string(util.NoneComputePolicy)},
			},
		}, {
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{util.VGPUComputePolicyAnnotation: string(util.BalanceComputePolicy)},
			},
		}, {
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{util.VGPUComputePolicyAnnotation: string(util.FixedComputePolicy)},
			},
		},
	}
	featureGate := featuregate.NewFeatureGate()
	runtime.Must(featureGate.Add(map[featuregate.Feature]featuregate.FeatureSpec{
		util.SMWatcher:   {Default: true, PreRelease: featuregate.Alpha},
		util.VMemoryNode: {Default: true, PreRelease: featuregate.Alpha},
		util.ClientMode:  {Default: true, PreRelease: featuregate.Alpha},
	}))

	devManager := manager.NewFakeDeviceManager(
		manager.WithNodeConfigSpec(config),
		manager.WithDevices(devices),
		manager.WithNvidiaVersion(driverVersion),
		manager.WithFeatureGate(featureGate))
	tests := []struct {
		name    string
		path    string
		nodes   []*corev1.Node
		pod     *corev1.Pod
		devices device.ContainerDevices
	}{
		{
			name:  "example 1",
			path:  "/tmp/vgpu1.config",
			nodes: nodes,
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
						Cores:  0,
						Memory: 1024,
					},
				},
			},
		},
		{
			name:  "example 2",
			path:  "/tmp/vgpu2.config",
			nodes: nodes,
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
						Cores:  20,
						Memory: 1024,
					},
				},
			},
		},
		{
			name:  "example 3",
			path:  "/tmp/vgpu3.config",
			nodes: nodes,
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
						Cores:  20,
						Memory: 1024,
					}, {
						Id:     1,
						Uuid:   gpuUUID1,
						Cores:  30,
						Memory: 2048,
					},
				},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, node := range test.nodes {
				func() {
					err = WriteVGPUConfigFile(test.path, devManager, test.pod, test.devices, false, node)
					if err != nil {
						t.Fatal(err)
					}
					resourceData1, data, err := MmapResourceDataT(test.path)
					if err != nil {
						t.Fatal(err)
					}
					defer syscall.Munmap(data)
					resourceData2 := NewResourceDataT(devManager, test.pod, test.devices, false, node)
					assert.Equal(t, *resourceData1, *resourceData2)
					if err = os.RemoveAll(test.path); err != nil {
						t.Error(err)
					}
				}()
			}
		})
	}
}
