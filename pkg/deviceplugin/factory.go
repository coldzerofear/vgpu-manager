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
	"github.com/coldzerofear/vgpu-manager/pkg/deviceplugin/base"
	"github.com/coldzerofear/vgpu-manager/pkg/deviceplugin/cdi"
	"github.com/coldzerofear/vgpu-manager/pkg/deviceplugin/mig"
	"github.com/coldzerofear/vgpu-manager/pkg/deviceplugin/vgpu"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"gomodules.xyz/jsonpatch/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	ctrm "sigs.k8s.io/controller-runtime/pkg/manager"
)

func GetDevicePlugins(
	option *options.Options, devManager *manager.DeviceManager,
	clusterManager ctrm.Manager, kubeClient *kubernetes.Clientset,
) ([]base.DevicePlugin, error) {
	// Build the CDI handler (a null no-op handler is returned when no CDI
	// strategy is configured) and generate the node CDI specification so that
	// CDI device references emitted during Allocate can be resolved.
	// The NVIDIA CDI hook binary is bundled in the image and installed onto the
	// host (under HOST_MANAGER_DIR) by the install init container; the CDI spec
	// references that host path. It is executed by the host container runtime,
	// not by this plugin, so no flag is exposed for it.
	nodeConfig := devManager.GetNodeConfig()
	cdiHandler, err := cdi.New(
		devManager.DeviceLib,
		cdi.Config{
			Strategies:        nodeConfig.GetDeviceListStrategy(),
			Vendor:            util.CDIVendor,
			Class:             util.CDIClass,
			DeviceIDStrategy:  util.CDIDeviceIDStrategy,
			AnnotationPrefix:  option.CDIAnnotationPrefix,
			NvidiaCDIHookPath: filepath.Join(vgpu.HostManagerDirectoryPath, util.Tools, "nvidia-cdi-hook"),
			// The host driver/dev root is mounted into the plugin at the same path,
			// so the in-container read path equals the host path written into the
			// spec (TargetDriverRoot/TargetDevRoot default to these in cdi.New).
			DriverRoot:       option.ContainerDriverRoot,
			TargetDriverRoot: option.HostDriverRoot,
			DevRoot:          devManager.DevRoot,
		})
	if err != nil {
		klog.Errorf("Create CDI handler failed: %v", err)
		return nil, err
	}
	if err = cdiHandler.CreateSpecFile(); err != nil {
		klog.Errorf("Generate CDI spec file failed: %v", err)
		return nil, err
	}

	var plugins []base.DevicePlugin
	migStrategy := devManager.GetNodeConfig().GetMigStrategy()
	if migStrategy != util.MigStrategySingle {
		socket := filepath.Join(nodeConfig.GetDevicePluginPath(), "nvidia-vgpu.sock")
		plugin, err := vgpu.NewVNumberDevicePlugin(util.VGPUNumberResourceName,
			socket, devManager, kubeClient, clusterManager.GetCache(), cdiHandler)
		if err != nil {
			return nil, fmt.Errorf("create vnumber plugin failed: %v", err)
		}
		plugins = append(plugins, plugin)
	}

	var deleteResources []string
	if devManager.GetFeatureGate().Enabled(options.CorePlugin) {
		socket := filepath.Join(nodeConfig.GetDevicePluginPath(), "nvidia-vgpu-core.sock")
		plugins = append(plugins, vgpu.NewVCoreDevicePlugin(util.VGPUCoreResourceName, socket, devManager))
	} else {
		deleteResources = append(deleteResources, util.VGPUCoreResourceName)
	}

	if devManager.GetFeatureGate().Enabled(options.MemoryPlugin) {
		socket := filepath.Join(nodeConfig.GetDevicePluginPath(), "nvidia-vgpu-memory.sock")
		plugins = append(plugins, vgpu.NewVMemoryDevicePlugin(util.VGPUMemoryResourceName, socket, devManager))
	} else {
		deleteResources = append(deleteResources, util.VGPUMemoryResourceName)
	}

	nodeName := devManager.GetNodeConfig().GetNodeName()
	go CycleCleanupNodeResources(kubeClient, nodeName, deleteResources)

	if migStrategy != util.MigStrategyNone {
		var requireUniformMIGDevices bool
		if migStrategy == util.MigStrategySingle {
			requireUniformMIGDevices = true
		}
		if err := devManager.AssertAllMigDevicesAreValid(requireUniformMIGDevices); err != nil {
			return nil, fmt.Errorf("invalid MIG configuration: %v", err)
		}

		migDevices := devManager.GetMIGDeviceMap()
		if requireUniformMIGDevices && !devManager.AllAvailableGpuMigEnabled() && len(migDevices) != 0 {
			return nil, fmt.Errorf("all devices on the node must be configured with the same migEnabled value")
		}
		resourceSet := sets.NewString()
		for _, migDev := range migDevices {
			resource := strings.ReplaceAll(migDev.Profile, "+", ".")
			resourceSet.Insert(resource)
		}
		for resource := range resourceSet {
			resourceName := util.MIGDeviceResourceNamePrefix + resource
			socket := filepath.Join(nodeConfig.GetDevicePluginPath(), fmt.Sprintf("nvidia-mig-%s.sock", resource))
			plugins = append(plugins, mig.NewMigDevicePlugin(resourceName, socket, devManager, cdiHandler))
		}
	}

	if len(plugins) == 0 {
		return nil, fmt.Errorf("no available device plugins")
	}

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
		klog.ErrorS(err, "get node failed", "node", nodeName)
		return false
	}
	var jsonPatches []jsonpatch.Operation
	for _, resourceName := range resources {
		_, existAllocatable := util.GetAllocatableOfNode(node, resourceName)
		_, existCapacity := util.GetCapacityOfNode(node, resourceName)
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
		_, err = kubeClient.CoreV1().Nodes().Patch(ctx, nodeName, types.JSONPatchType,
			patchDataBytes, metav1.PatchOptions{}, "status")
		if err != nil {
			klog.V(3).ErrorS(err, "clear node resource failure", "node", nodeName, "resources", resources)
		}
	}
	return len(jsonPatches) == 0
}
