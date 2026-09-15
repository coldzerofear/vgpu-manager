/*
Copyright 2026 coldzerofear

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package remote

// The consumer role: a node with no GPUs that runs remote vGPU pods. kubelet
// is offered plain slots; the devices of a pod live on the GPU server the
// scheduler picked. Allocate only tells the container how to reach them and
// which files it will be given, so pod admission never waits on the network:
// the session and the client shim are prepared in PreStartContainer, per
// container, right before it starts.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/coldzerofear/vgpu-manager/pkg/client"
	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/device/manager"
	"github.com/coldzerofear/vgpu-manager/pkg/device/remotegpu"
	"github.com/coldzerofear/vgpu-manager/pkg/deviceplugin/base"
	"github.com/coldzerofear/vgpu-manager/pkg/deviceplugin/vgpu"
	kubeletremote "github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin/remote"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

const (
	// consumerPluginName names this plugin in logs and in the base server.
	consumerPluginName = "remote-consumer-plugin"
	// remoteDeviceIDPrefix prefixes the slot ids offered to kubelet. They
	// stand for "one remote vGPU this node may run", not for a local device.
	remoteDeviceIDPrefix = "remote-vgpu"
	// driverLinkName is the per-container link to the client shim directory,
	// and ldPreloadFileName the per-container preload list. Both are filled in
	// by PreStartContainer and bind-mounted into the container.
	driverLinkName      = util.Driver
	ldPreloadFileName   = "ld.so.preload"
	deviceListFileName  = "devices.json"
	containerDirectory  = 0o777
	containerDirectoryF = 0o664
)

// ConsumerConfig is what the consumer plugin needs about this node.
type ConsumerConfig struct {
	NodeName     string
	ResourceName string
	Socket       string
	// VGPUNumber is how many remote vGPUs this node runs at a time.
	VGPUNumber int
	// ManagerDir is this process's view of the manager directory,
	// HostManagerDir the host's; the latter is what mounts refer to.
	ManagerDir     string
	HostManagerDir string
}

type consumerDevicePlugin struct {
	pluginapi.UnimplementedDevicePluginServer
	baseServer base.PluginServer
	kubeClient kubernetes.Interface
	cfg        ConsumerConfig
	devices    []*pluginapi.Device
	mutex      sync.Mutex
}

var _ base.DevicePlugin = &consumerDevicePlugin{}

// NewConsumerDevicePlugin returns the device plugin of a remote consumer node.
func NewConsumerDevicePlugin(cfg ConsumerConfig, devManager *manager.DeviceManager, kubeClient kubernetes.Interface) base.DevicePlugin {
	devices := make([]*pluginapi.Device, 0, cfg.VGPUNumber)
	for i := 0; i < cfg.VGPUNumber; i++ {
		devices = append(devices, &pluginapi.Device{
			ID:     fmt.Sprintf("%s-%d", remoteDeviceIDPrefix, i),
			Health: pluginapi.Healthy,
		})
	}
	return &consumerDevicePlugin{
		baseServer: base.NewBasePluginServer(cfg.ResourceName, cfg.Socket, devManager),
		kubeClient: kubeClient,
		cfg:        cfg,
		devices:    devices,
	}
}

func (m *consumerDevicePlugin) Name() string { return consumerPluginName }

func (m *consumerDevicePlugin) Start() error { return m.baseServer.Start(m.Name(), m) }

func (m *consumerDevicePlugin) Stop() error { return m.baseServer.Stop(m.Name()) }

// Devices are slots, not hardware: they never turn unhealthy on this node.
// Whether a remote GPU can serve a pod is the scheduler's decision, made from
// the server node's own registry.
func (m *consumerDevicePlugin) Devices() []*pluginapi.Device { return m.devices }

// GetDevicePluginOptions asks kubelet for PreStartContainer: the session on
// the GPU server is created there, per container.
func (m *consumerDevicePlugin) GetDevicePluginOptions(_ context.Context, _ *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	return &pluginapi.DevicePluginOptions{PreStartRequired: true}, nil
}

// ListAndWatch sends the slot list once: it never changes while the plugin runs.
func (m *consumerDevicePlugin) ListAndWatch(_ *pluginapi.Empty, s pluginapi.DevicePlugin_ListAndWatchServer) error {
	if err := s.Send(&pluginapi.ListAndWatchResponse{Devices: m.Devices()}); err != nil {
		klog.Errorf("DevicePlugin '%s' ListAndWatch send devices error: %v", m.Name(), err)
	}
	<-m.baseServer.GetStopCh()
	return nil
}

// GetPreferredAllocation has no preference: the slots are interchangeable.
func (m *consumerDevicePlugin) GetPreferredAllocation(_ context.Context, _ *pluginapi.PreferredAllocationRequest) (*pluginapi.PreferredAllocationResponse, error) {
	return &pluginapi.PreferredAllocationResponse{}, nil
}

// Allocate hands the container what it needs to reach its remote GPUs: the
// server address and its session token, plus the mounts PreStartContainer
// fills in. It talks to no one but the apiserver.
func (m *consumerDevicePlugin) Allocate(ctx context.Context, req *pluginapi.AllocateRequest) (resp *pluginapi.AllocateResponse, err error) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	klog.V(4).InfoS("Allocate", "pluginName", m.Name(), "request", req.GetContainerRequests())
	var currentPod *corev1.Pod
	resp = &pluginapi.AllocateResponse{}
	defer func() {
		if err == nil {
			return
		}
		if currentPod != nil {
			klog.V(4).ErrorS(err, util.AllocateCheckErrMsg, "pod", klog.KObj(currentPod))
			if patchErr := client.PatchPodAllocationFailed(m.kubeClient, currentPod); patchErr != nil {
				klog.ErrorS(patchErr, "Error calling PatchPodAllocationFailed", "pod", klog.KObj(currentPod))
			}
		}
		err = fmt.Errorf("%s: %s", util.AllocateCheckErrMsg, err.Error())
	}()

	if currentPod, err = m.currentPod(ctx); err != nil {
		return resp, err
	}
	server, err := m.serverEndpoints(ctx, currentPod)
	if err != nil {
		return resp, err
	}

	responses := make([]*pluginapi.ContainerAllocateResponse, len(req.ContainerRequests))
	for i, containerRequest := range req.ContainerRequests {
		var contClaim *device.ContainerDeviceClaim
		if contClaim, err = device.GetCurrentPreAllocateContainerDevice(currentPod); err != nil {
			return resp, fmt.Errorf("get pod pre-allocate device claim failed: %w", err)
		}
		if len(containerRequest.GetDevicesIds()) != len(contClaim.DeviceClaims) {
			return resp, fmt.Errorf("requested number of devices does not match")
		}
		if responses[i], err = m.containerResponse(currentPod, contClaim, server, containerRequest.GetDevicesIds()); err != nil {
			return resp, err
		}
		if err = device.UpdatePodRealContainerDeviceClaim(currentPod, *contClaim); err != nil {
			return resp, fmt.Errorf("update pod real-allocate device claim failed: %w", err)
		}
	}

	resp.ContainerResponses = responses
	if patchErr := client.PatchPodAllocationSucceed(m.kubeClient, currentPod); patchErr != nil {
		klog.ErrorS(patchErr, "Error calling PatchPodAllocationSucceed", "pod", klog.KObj(currentPod))
	}
	return resp, nil
}

// containerResponse is what one container is given: the remote environment and
// the two paths PreStartContainer fills in before the container starts.
func (m *consumerDevicePlugin) containerResponse(
	pod *corev1.Pod, contClaim *device.ContainerDeviceClaim,
	server *remotegpu.ServerEndpointInfo, deviceIDs []string,
) (*pluginapi.ContainerAllocateResponse, error) {
	contDir, hostDir := m.containerPaths(pod.UID, contClaim.Name)
	if err := util.EnsureDir(contDir, containerDirectory); err != nil {
		return nil, fmt.Errorf("prepare directory %s: %w", contDir, err)
	}
	// PreStartContainer identifies the container it is called for by these ids.
	if err := writeJSONFile(filepath.Join(contDir, deviceListFileName), deviceIDs, containerDirectoryF); err != nil {
		return nil, fmt.Errorf("write %s failed: %w", deviceListFileName, err)
	}

	response := &pluginapi.ContainerAllocateResponse{Envs: map[string]string{
		util.PodNameEnv:      pod.Name,
		util.PodNamespaceEnv: pod.Namespace,
		util.PodUIDEnv:       string(pod.UID),
		util.ContNameEnv:     contClaim.Name,
		// No local GPU is injected; every CUDA call goes to the server.
		"NVIDIA_VISIBLE_DEVICES":              "void",
		kubeletremote.EnvLupineDisableLocal:   "1",
		kubeletremote.EnvLupineServer:         server.ServerEndpoint,
		kubeletremote.EnvLupineSession:        remotegpu.SessionToken(string(pod.UID), contClaim.Name),
		kubeletremote.EnvLupineClientPlatform: kubeletremote.LocalClientBundlePlatform(),
	}}
	response.Mounts = []*pluginapi.Mount{{
		// The client shim libraries, linked to the version this server needs.
		ContainerPath: filepath.Join(util.ManagerRootPath, util.Driver),
		HostPath:      filepath.Join(hostDir, driverLinkName),
		ReadOnly:      true,
	}, {
		// The preload list that makes them load, without touching the image.
		ContainerPath: vgpu.ContPreLoadFilePath,
		HostPath:      filepath.Join(hostDir, ldPreloadFileName),
		ReadOnly:      true,
	}}
	return response, nil
}

// currentPod is the pod kubelet is admitting: the oldest pre-allocated pod on
// this node that is still waiting for its devices.
func (m *consumerDevicePlugin) currentPod(ctx context.Context) (*corev1.Pod, error) {
	pods, err := client.GetActivePodsOnNode(ctx, m.kubeClient, m.cfg.NodeName)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve the active pods of the current node: %v", err)
	}
	return util.GetCurrentPodByAllocatingPods(util.FilterAllocatingPods(pods))
}

// serverEndpoints reads the addresses of the GPU server the scheduler chose
// for this pod, which is its predicate node.
func (m *consumerDevicePlugin) serverEndpoints(ctx context.Context, pod *corev1.Pod) (*remotegpu.ServerEndpointInfo, error) {
	serverName := util.PodPlanSchedulingNode(pod)
	if serverName == "" || serverName == m.cfg.NodeName {
		return nil, fmt.Errorf("pod %s has no remote GPU server", klog.KObj(pod))
	}
	node, err := m.kubeClient.CoreV1().Nodes().Get(ctx, serverName, metav1.GetOptions{ResourceVersion: "0"})
	if err != nil {
		return nil, fmt.Errorf("get remote GPU server node %s: %w", serverName, err)
	}
	return remotegpu.GetServerEndpointInfo(node)
}

// containerPaths is the per-container directory, as this process and as the
// host see it.
func (m *consumerDevicePlugin) containerPaths(podUID k8stypes.UID, contName string) (contDir, hostDir string) {
	return util.GetPodContainerManagerPath(m.cfg.ManagerDir, podUID, contName),
		util.GetPodContainerManagerPath(m.cfg.HostManagerDir, podUID, contName)
}

// PreStartContainer creates the container's session on the GPU server and
// stages its client shim.
//
// TODO(S4.3): not wired yet. Until it is, a remote pod must not start: it
// would have no session and its CUDA calls would be refused by the server.
func (m *consumerDevicePlugin) PreStartContainer(_ context.Context, _ *pluginapi.PreStartContainerRequest) (*pluginapi.PreStartContainerResponse, error) {
	return &pluginapi.PreStartContainerResponse{},
		fmt.Errorf("%s: remote sessions are not wired yet", util.PreStartContainerCheckErrMsg)
}

func writeJSONFile(path string, value any, mode os.FileMode) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, mode)
}
