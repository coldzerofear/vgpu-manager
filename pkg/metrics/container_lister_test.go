package metrics

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	dpoptions "github.com/coldzerofear/vgpu-manager/cmd/device-plugin/options"
	"github.com/coldzerofear/vgpu-manager/pkg/config/node"
	"github.com/coldzerofear/vgpu-manager/pkg/config/vgpu"
	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/device/manager"
	"github.com/coldzerofear/vgpu-manager/pkg/deviceplugin"
	"github.com/coldzerofear/vgpu-manager/pkg/version"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
)

func Test_ContainerLister(t *testing.T) {
	nodeName := "testNode"
	k8sClient := fake.NewSimpleClientset()
	factory := informers.NewSharedInformerFactory(k8sClient, 0)
	podLister := factory.Core().V1().Pods().Lister()
	basePath := "/tmp/vgpu-manager"
	_ = os.MkdirAll(basePath, 07777)
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
	k8sClient.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{})

	driverVersion := version.Version{
		DriverVersion: "",
		CudaVersion:   version.CudaVersion(12020),
	}
	option := node.MutationDPOptions(dpoptions.Options{
		NodeName:            nodeName,
		DeviceMemoryScaling: float64(1),
	})
	config, _ := node.NewNodeConfig("", option)
	gpuUUID0 := "GPU-" + string(uuid.NewUUID())
	gpuUUID1 := "GPU-" + string(uuid.NewUUID())
	devices := []*manager.GPUDevice{{
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
	}}

	devManager := manager.NewFakeDeviceManager(config, driverVersion, devices)
	contDeviceMap := map[string][]device.ClaimDevice{
		containerName1: {{
			Id:     0,
			Uuid:   gpuUUID0,
			Core:   20,
			Memory: 1024,
		}, {
			Id:     1,
			Uuid:   gpuUUID1,
			Core:   30,
			Memory: 2048,
		}},
		containerName2: {{
			Id:     0,
			Uuid:   gpuUUID0,
			Core:   30,
			Memory: 2048,
		}, {
			Id:     1,
			Uuid:   gpuUUID1,
			Core:   20,
			Memory: 1024,
		}},
	}
	node := &corev1.Node{}
	contResDataMap := map[string]*vgpu.ResourceDataT{}
	for _, container := range pod.Spec.Containers {
		key := GetContainerKey(pod.UID, container.Name)
		path := filepath.Join(basePath, string(key))
		_ = os.Mkdir(path, 07777)
		path = filepath.Join(path, deviceplugin.VGPUConfigFileName)
		claimDevices := contDeviceMap[container.Name]
		assignDevices := device.ContainerDevices{Name: container.Name, Devices: claimDevices}
		err := vgpu.WriteVGPUConfigFile(path, devManager, pod, assignDevices, false, node)
		if err != nil {
			t.Error(err)
		}
		resData := vgpu.NewResourceDataT(devManager, pod, assignDevices, false, node)
		contResDataMap[container.Name] = resData
	}
	go contLister.Start(ctx.Done())
	time.Sleep(5 * time.Second)

	for _, container := range pod.Spec.Containers {
		key := GetContainerKey(pod.UID, container.Name)
		if data, ok := contLister.GetResourceData(key); !ok {
			t.Errorf("Unable to find container resource configuration file")
		} else {
			assert.Equal(t, contResDataMap[container.Name], data)
		}
	}

	//k8sClient.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{})
	//time.Sleep(5 * time.Second)
	//for _, container := range pod.Spec.Containers {
	//	key := GetContainerKey(pod.UID, container.Name)
	//	if _, ok := contLister.GetResourceData(key); ok {
	//		t.Errorf("The container resource configuration file should have been deleted by now")
	//	}
	//}

}
