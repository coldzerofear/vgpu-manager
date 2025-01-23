package deviceplugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"time"

	"github.com/coldzerofear/vgpu-manager/cmd/device-plugin/options"
	"github.com/coldzerofear/vgpu-manager/pkg/device/manager"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"gomodules.xyz/jsonpatch/v2"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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

func InitDevicePlugins(opt *options.Options, devManager *manager.DeviceManager,
	clusterManager ctrm.Manager, kubeClient *kubernetes.Clientset) []DevicePlugin {
	socket := filepath.Join(opt.DevicePluginPath, "vgpu-number.sock")
	plugins := []DevicePlugin{
		NewNumberDevicePlugin(util.VGPUNumberResourceName,
			devManager, socket, kubeClient, clusterManager.GetCache()),
	}

	var deleteResources []string
	if opt.EnableCorePlugin {
		socket = filepath.Join(opt.DevicePluginPath, "vgpu-core.sock")
		corePlugin := NewCoreDevicePlugin(
			util.VGPUCoreResourceName, devManager, socket,
		)
		plugins = append(plugins, corePlugin)
	} else {
		deleteResources = append(deleteResources, util.VGPUCoreResourceName)
	}
	if opt.EnableMemoryPlugin {
		socket = filepath.Join(opt.DevicePluginPath, "vgpu-memory.sock")
		memoryPlugin := NewMemoryDevicePlugin(
			util.VGPUMemoryResourceName, devManager, socket,
		)
		plugins = append(plugins, memoryPlugin)
	} else {
		deleteResources = append(deleteResources, util.VGPUMemoryResourceName)
	}
	cleanupNodeResources(kubeClient, opt.NodeName, deleteResources)
	return plugins
}

// cleanupNodeResources Clean up some resource names published on node
func cleanupNodeResources(kubeClient *kubernetes.Clientset, nodeName string, resources []string) {
	node, err := kubeClient.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
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
		_, err := kubeClient.CoreV1().Nodes().Patch(context.Background(), nodeName,
			types.JSONPatchType, patchDataBytes, metav1.PatchOptions{}, "status")
		if err != nil {
			klog.V(3).Infof("Clear node <%s> resource %+v failure: %v", nodeName, resources, err)
		}
	}
}

// dial establishes the gRPC communication with the registered device plugin.
func dial(unixSocketPath string, timeout time.Duration) (*grpc.ClientConn, error) {
	if c, err := grpc.Dial(unixSocketPath, grpc.WithInsecure(), grpc.WithBlock(), grpc.WithTimeout(timeout),
		grpc.WithDialer(func(addr string, timeout time.Duration) (net.Conn, error) {
			return net.DialTimeout("unix", addr, timeout)
		}),
	); err != nil {
		return nil, err
	} else {
		return c, nil
	}
}
