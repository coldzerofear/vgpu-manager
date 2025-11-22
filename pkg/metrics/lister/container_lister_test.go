package lister

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/component-base/featuregate"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/coldzerofear/vgpu-manager/pkg/config/node"
	"github.com/coldzerofear/vgpu-manager/pkg/config/vgpu"
	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/device/manager"
	"github.com/coldzerofear/vgpu-manager/pkg/device/nvidia"
	dpvgpu "github.com/coldzerofear/vgpu-manager/pkg/deviceplugin/vgpu"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
)

func Test_ContainerLister(t *testing.T) {
	nodeName := "testNode"
	k8sClient := fake.NewSimpleClientset()
	factory := informers.NewSharedInformerFactory(k8sClient, 0)
	podLister := factory.Core().V1().Pods().Lister()
	basePath := "/tmp/vgpu-manager"
	_ = os.MkdirAll(basePath, 0777)
	contLister := NewContainerLister(basePath, nodeName, podLister)
	ctx, cancelFunc := context.WithCancel(context.Background())
	defer func() {
		cancelFunc()
		_ = os.RemoveAll(basePath)
	}()

	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())

	containerName1 := "default1"
	containerName2 := "default2"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID:       uuid.NewUUID(),
			Namespace: "default",
			Name:      "test-pod",
		},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
			Containers: []corev1.Container{{
				Name: containerName1,
			}, {
				Name: containerName2,
			}},
		},
	}
	_, _ = k8sClient.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{})

	driverVersion := nvidia.DriverVersion{
		DriverVersion:     "",
		CudaDriverVersion: nvidia.CudaDriverVersion(12020),
	}
	config, err := node.NewNodeConfig(func(spec *node.NodeConfigSpec) {
		spec.NodeName = nodeName
		spec.DeviceSplitCount = ptr.To(10)
		spec.DeviceMemoryFactor = ptr.To(1)
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
	contDeviceMap := map[string][]device.ClaimDevice{
		containerName1: {{
			Id:     0,
			Uuid:   gpuUUID0,
			Cores:  20,
			Memory: 1024,
		}, {
			Id:     1,
			Uuid:   gpuUUID1,
			Cores:  30,
			Memory: 2048,
		}},
		containerName2: {{
			Id:     0,
			Uuid:   gpuUUID0,
			Cores:  30,
			Memory: 2048,
		}, {
			Id:     1,
			Uuid:   gpuUUID1,
			Cores:  20,
			Memory: 1024,
		}},
	}
	node := &corev1.Node{}
	contResDataMap := map[string]*vgpu.ResourceDataT{}
	for _, container := range pod.Spec.Containers {
		key := GetContainerKey(pod.UID, container.Name)
		path := filepath.Join(basePath, string(key))
		_ = os.MkdirAll(path, 0777)
		path = filepath.Join(path, dpvgpu.VGPUConfigFileName)
		claimDevices := contDeviceMap[container.Name]
		assignDevices := device.ContainerDevices{Name: container.Name, Devices: claimDevices}
		err = vgpu.WriteVGPUConfigFile(path, devManager, pod, assignDevices, false, node)
		if err != nil {
			t.Error(err)
		}
		resData := vgpu.NewResourceDataT(devManager, pod, assignDevices, false, node)
		contResDataMap[container.Name] = resData
	}

	contLister.Start(time.Second, ctx.Done())
	time.Sleep(5 * time.Second)

	for _, container := range pod.Spec.Containers {
		key := GetContainerKey(pod.UID, container.Name)
		if data, ok := contLister.GetResourceDataT(key); !ok {
			t.Errorf("Unable to find container resource configuration file")
		} else {
			assert.Equal(t, contResDataMap[container.Name], data)
		}
	}

	if err := os.Setenv("UNIT_TESTING", "true"); err != nil {
		t.Error(err)
	}
	_ = k8sClient.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{})
	time.Sleep(2 * time.Second)
	for _, container := range pod.Spec.Containers {
		key := GetContainerKey(pod.UID, container.Name)
		if _, ok := contLister.GetResourceDataT(key); ok {
			t.Errorf("The container resource configuration file should have been deleted by now")
		}
	}

}
