package deviceplugin

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/coldzerofear/vgpu-manager/cmd/device-plugin/options"
	"github.com/coldzerofear/vgpu-manager/pkg/device/manager"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"gomodules.xyz/jsonpatch/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
	ctrm "sigs.k8s.io/controller-runtime/pkg/manager"
)

type DevicePlugin interface {
	pluginapi.DevicePluginServer
	// Name return device plugin name.
	Name() string
	// Start the plugin.
	Start() error
	// Stop the plugin.
	Stop() error
	// Devices return device list.
	Devices() []*pluginapi.Device
}

func GetDevicePlugins(opt *options.Options, devManager *manager.DeviceManager,
	clusterManager ctrm.Manager, kubeClient *kubernetes.Clientset) ([]DevicePlugin, error) {
	socket := filepath.Join(opt.DevicePluginPath, "vgpu-number.sock")
	plugins := []DevicePlugin{
		NewVNumberDevicePlugin(util.VGPUNumberResourceName,
			socket, devManager, kubeClient, clusterManager.GetCache()),
	}

	var deleteResources []string

	if opt.FeatureGate.Enabled(options.CorePlugin) {
		socket = filepath.Join(opt.DevicePluginPath, "vgpu-core.sock")
		vcorePlugin := NewVCoreDevicePlugin(util.VGPUCoreResourceName, socket, devManager)
		plugins = append(plugins, vcorePlugin)
	} else {
		deleteResources = append(deleteResources, util.VGPUCoreResourceName)
	}

	if opt.FeatureGate.Enabled(options.MemoryPlugin) {
		socket = filepath.Join(opt.DevicePluginPath, "vgpu-memory.sock")
		vmemoryPlugin := NewVMemoryDevicePlugin(util.VGPUMemoryResourceName, socket, devManager)
		plugins = append(plugins, vmemoryPlugin)
	} else {
		deleteResources = append(deleteResources, util.VGPUMemoryResourceName)
	}

	migStrategy := devManager.GetNodeConfig().MigStrategy()
	if migStrategy != util.MigStrategyNone {
		var requireUniformMIGDevices bool
		if migStrategy == util.MigStrategySingle {
			requireUniformMIGDevices = true
		}
		if err := devManager.AssertAllMigDevicesAreValid(requireUniformMIGDevices); err != nil {
			return nil, fmt.Errorf("invalid MIG configuration: %v", err)
		}
		allMigEnabled := true
		for _, gpuDevice := range devManager.GetGPUDeviceMap() {
			if !gpuDevice.Healthy { //Skip excluded devices
				continue
			}
			if !gpuDevice.MigEnabled {
				allMigEnabled = false
				break
			}
		}
		migDevices := devManager.GetMIGDeviceMap()
		if requireUniformMIGDevices && !allMigEnabled && len(migDevices) != 0 {
			return nil, fmt.Errorf("all devices on the node must be configured with the same migEnabled value")
		}
		resourceSet := sets.NewString()
		for _, mig := range migDevices {
			resource := strings.ReplaceAll(mig.Profile, "+", ".")
			resourceSet.Insert(resource)
		}
		for resource := range resourceSet {
			resourceName := util.MIGDeviceResourceNamePrefix + resource
			socket = filepath.Join(opt.DevicePluginPath, fmt.Sprintf("nvidia-mig-%s.sock", resource))
			plugins = append(plugins, NewMigDevicePlugin(resourceName, socket, devManager))
		}
	}
	go CycleCleanupNodeResources(kubeClient, opt.NodeName, deleteResources)
	return plugins, nil
}

const defaultDoneCount = 6

// CycleCleanupNodeResources Loop cleaning until the resource names on the node are completely deleted.
func CycleCleanupNodeResources(kubeClient *kubernetes.Clientset, nodeName string, resources []string) {
	ctx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()
	count := 0
	_ = wait.PollUntilContextCancel(ctx, 5*time.Second, true, func(ctx context.Context) (bool, error) {
		done := cleanupNodeResources(ctx, kubeClient, nodeName, resources)
		if done {
			count++
			if count < defaultDoneCount {
				done = false
			}
		} else {
			count = 0
		}
		return done, nil
	})
}

// cleanupNodeResources Clean up some resource names published on node
func cleanupNodeResources(ctx context.Context, kubeClient *kubernetes.Clientset, nodeName string, resources []string) bool {
	node, err := kubeClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		klog.Fatalf("get node %s failed: %v", nodeName, err)
	}
	var jsonPatches []jsonpatch.Operation
	for _, resourceName := range resources {
		_, existAllocatable := node.Status.Allocatable[corev1.ResourceName(resourceName)]
		_, existCapacity := node.Status.Capacity[corev1.ResourceName(resourceName)]
		resourceName = strings.ReplaceAll(resourceName, "/", "~1")
		if existCapacity {
			jsonPatches = append(jsonPatches, jsonpatch.NewOperation("remove",
				fmt.Sprintf("/status/capacity/%s", resourceName), nil))
		}
		if existAllocatable {
			jsonPatches = append(jsonPatches, jsonpatch.NewOperation("remove",
				fmt.Sprintf("/status/allocatable/%s", resourceName), nil))
		}
	}
	if len(jsonPatches) > 0 {
		patchDataBytes, _ := json.Marshal(jsonPatches)
		_, err := kubeClient.CoreV1().Nodes().Patch(ctx, nodeName, types.JSONPatchType,
			patchDataBytes, metav1.PatchOptions{}, "status")
		if err != nil {
			klog.V(3).Infof("Clear node <%s> resource %+v failure: %v", nodeName, resources, err)
		}
		return false
	}
	return true
}
