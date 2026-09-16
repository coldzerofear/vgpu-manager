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

// The consumer role: a node with no GPUs of its own that runs remote vGPU
// pods. kubelet is offered plain slots; the devices of a pod live on the GPU
// server the scheduler picked for it.
//
// Allocate does all the work: it stages the client shim the container loads,
// creates the container's session on that server, and returns the environment
// and mounts that tie the two together. PreStartContainer is deliberately not
// implemented -- kubelet passes device ids only, and a sequential init
// container's ids may be reused for the app container, so it cannot tell
// reliably which container it is called for, while Allocate always knows.

import (
	"context"
	"fmt"
	"sync"

	"github.com/coldzerofear/vgpu-manager/pkg/client"
	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/device/manager"
	"github.com/coldzerofear/vgpu-manager/pkg/device/remotegpu"
	"github.com/coldzerofear/vgpu-manager/pkg/deviceplugin/base"
	"github.com/coldzerofear/vgpu-manager/pkg/deviceplugin/nodedevice"
	"github.com/coldzerofear/vgpu-manager/pkg/deviceplugin/vgpu"
	kubeletremote "github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin/remote"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
)

// ConsumerConfig is what the consumer plugin needs to know about this node.
type ConsumerConfig struct {
	NodeName     string
	ResourceName string
	Socket       string
	// VGPUNumber is how many remote vGPUs this node runs at a time.
	VGPUNumber int
	// ArtifactsDir holds the client shims, one directory per CUDA version, as
	// this process sees them; HostArtifactsDir is the same directory as the
	// host sees it, which is what a mount must refer to.
	ArtifactsDir     string
	HostArtifactsDir string
}

type consumerDevicePlugin struct {
	pluginapi.UnimplementedDevicePluginServer
	baseServer base.PluginServer
	kubeClient kubernetes.Interface
	cfg        ConsumerConfig
	devices    []*pluginapi.Device
	mutex      sync.Mutex
	// ensureSession asks an agent for one container's session; overridden in
	// tests, where no agent is there to ask.
	ensureSession func(ctx context.Context, agentEndpoint string, session remotegpu.PodSession) (string, error)
}

var _ base.DevicePlugin = &consumerDevicePlugin{}

// NewConsumerDevicePlugin returns the device plugin of a remote consumer node.
func NewConsumerDevicePlugin(cfg ConsumerConfig, devManager *manager.DeviceManager, kubeClient kubernetes.Interface) base.DevicePlugin {
	// A node that also serves its own GPUs remotely keeps at least as many
	// slots as those GPUs offer, so its local capacity is never the smaller
	// of the two (analysis §16.6).
	localSlots := 0
	for _, dev := range devManager.GetNodeDeviceInfo() {
		if !dev.Mig {
			localSlots += dev.Number
		}
	}
	slots := max(cfg.VGPUNumber, localSlots)
	devices := make([]*pluginapi.Device, 0, slots)
	for i := 0; i < slots; i++ {
		devices = append(devices, &pluginapi.Device{
			ID:     fmt.Sprintf("%s-%d", remoteDeviceIDPrefix, i),
			Health: pluginapi.Healthy,
		})
	}
	return &consumerDevicePlugin{
		baseServer:    base.NewBasePluginServer(cfg.ResourceName, cfg.Socket, devManager),
		kubeClient:    kubeClient,
		cfg:           cfg,
		devices:       devices,
		ensureSession: remotegpu.EnsureSession,
	}
}

func (m *consumerDevicePlugin) Name() string { return consumerPluginName }

func (m *consumerDevicePlugin) Start() error {
	err := m.baseServer.Start(m.Name(), m)
	if err == nil {
		// A node that serves its own GPUs remotely as well publishes them for
		// the scheduler; a node without GPUs publishes nothing.
		nodedevice.Setup(m.Name(), m.baseServer.GetDeviceManager())
	}
	return err
}

func (m *consumerDevicePlugin) Stop() error {
	err := m.baseServer.Stop(m.Name())
	nodedevice.Remove(m.Name(), m.baseServer.GetDeviceManager())
	return err
}

// Devices are slots, not hardware: they never turn unhealthy on this node.
// Whether a remote GPU can serve a pod is the scheduler's decision, made from
// the server node's own registry.
func (m *consumerDevicePlugin) Devices() []*pluginapi.Device { return m.devices }

// GetDevicePluginOptions asks for nothing: everything a remote container needs
// is prepared in Allocate.
func (m *consumerDevicePlugin) GetDevicePluginOptions(_ context.Context, _ *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	return &pluginapi.DevicePluginOptions{}, nil
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

// Allocate prepares each container of the pod kubelet is admitting: its
// session on the remote GPU server and the client shim it loads, then returns
// the environment and mounts that connect them.
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
		if responses[i], err = m.containerResponse(ctx, currentPod, contClaim, server); err != nil {
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

// containerResponse prepares one container and describes what it gets: the
// client shim, the preload list that makes it load, and the address and
// session of the server holding its GPUs.
func (m *consumerDevicePlugin) containerResponse(
	ctx context.Context, pod *corev1.Pod,
	contClaim *device.ContainerDeviceClaim, server *remotegpu.ServerEndpointInfo,
) (*pluginapi.ContainerAllocateResponse, error) {
	artifact, err := m.stageClientShim(server)
	if err != nil {
		return nil, err
	}
	session := m.podSession(pod, contClaim.Name)
	if _, err = m.ensureSession(ctx, server.AgentEndpoint, session); err != nil {
		return nil, fmt.Errorf("prepare the remote session of container %s: %w", contClaim.Name, err)
	}

	response := &pluginapi.ContainerAllocateResponse{Envs: map[string]string{
		util.PodNameEnv:      pod.Name,
		util.PodNamespaceEnv: pod.Namespace,
		util.PodUIDEnv:       string(pod.UID),
		util.ContNameEnv:     contClaim.Name,
		// No local GPU is injected; every CUDA call goes to the server.
		"NVIDIA_VISIBLE_DEVICES":            "void",
		kubeletremote.EnvLupineDisableLocal: "1",
		kubeletremote.EnvLupineServer:       server.ServerEndpoint,
		kubeletremote.EnvLupineSession:      session.Token,
	}}
	if artifact.ETag != "" {
		// Lets the server check that this client is the build it embeds.
		response.Envs[kubeletremote.EnvLupineClientETag] = artifact.ETag
		response.Envs[kubeletremote.EnvLupineClientPlatform] = kubeletremote.LocalClientBundlePlatform()
	}
	response.Mounts = []*pluginapi.Mount{{
		// The client shim libraries.
		ContainerPath: artifact.ContainerDir,
		HostPath:      artifact.HostDir,
		ReadOnly:      true,
	}, {
		// The preload list that makes them load, leaving the image untouched.
		ContainerPath: vgpu.ContPreLoadFilePath,
		HostPath:      artifact.LdPreloadHost,
		ReadOnly:      true,
	}}
	if artifact.NvidiaSMIHost != "" {
		response.Mounts = append(response.Mounts, &pluginapi.Mount{
			ContainerPath: "/usr/bin/nvidia-smi",
			HostPath:      artifact.NvidiaSMIHost,
			ReadOnly:      true,
		})
	}
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
