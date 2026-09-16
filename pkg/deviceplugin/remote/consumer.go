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

// The consumer role: this node runs remote vGPU pods. kubelet is offered
// plain slots; the devices of a pod live on the GPU server the scheduler
// picked for it.
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
	"github.com/coldzerofear/vgpu-manager/pkg/device/remotegpu"
	"github.com/coldzerofear/vgpu-manager/pkg/deviceplugin/vgpu"
	kubeletremote "github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin/remote"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

// ConsumerOptions is what the consumer role needs to know.
type ConsumerOptions struct {
	// VGPUNumber is how many remote vGPUs this node runs at a time.
	VGPUNumber int
	// ArtifactsDir holds the client shims, one directory per CUDA version, as
	// this process sees them; HostArtifactsDir is the same directory as the
	// host sees it, which is what a mount must refer to.
	ArtifactsDir         string
	HostArtifactsDir     string
	IgnoreClientShimEtag bool
}

// consumerRole answers Allocate for the remote pods this node runs.
type consumerRole struct {
	nodeName   string
	kubeClient kubernetes.Interface
	opts       ConsumerOptions
	mutex      sync.Mutex
	// ensureSession asks an agent for one container's session; overridden in
	// tests, where no agent is there to ask.
	ensureSession func(ctx context.Context, agentEndpoint string, session remotegpu.PodSession) (string, error)
}

func newConsumerRole(nodeName string, kubeClient kubernetes.Interface, opts ConsumerOptions) *consumerRole {
	return &consumerRole{
		nodeName:      nodeName,
		kubeClient:    kubeClient,
		opts:          opts,
		ensureSession: remotegpu.EnsureSession,
	}
}

// allocate prepares each container of the pod kubelet is admitting: its
// session on the remote GPU server and the client shim it loads, then returns
// the environment and mounts that connect them.
func (m *consumerRole) allocate(ctx context.Context, req *pluginapi.AllocateRequest) (resp *pluginapi.AllocateResponse, err error) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	klog.V(4).InfoS("Allocate", "node", m.nodeName, "request", req.GetContainerRequests())
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
func (m *consumerRole) containerResponse(
	ctx context.Context, pod *corev1.Pod,
	contClaim *device.ContainerDeviceClaim,
	server *remotegpu.ServerEndpointInfo,
) (*pluginapi.ContainerAllocateResponse, error) {
	artifact, err := m.stageClientShim(ctx, pod, contClaim.Name, server)
	if err != nil {
		return nil, err
	}
	// The node annotation is how the agent is found; the agent is what says
	// where lupine-server is right now. Publishing lags a server that moved
	// or restarted on another port, so the container is given the address the
	// agent reports and the published one only as a fallback.
	session := m.podSession(pod, contClaim.Name)
	serverEndpoint, err := m.ensureSession(ctx, server.AgentEndpoint, session)
	if err != nil {
		return nil, fmt.Errorf("prepare the remote session of container %s: %w", contClaim.Name, err)
	}
	switch {
	case serverEndpoint == "":
		serverEndpoint = server.ServerEndpoint
	case server.ServerEndpoint != "" && serverEndpoint != server.ServerEndpoint:
		klog.V(2).InfoS("remote-agent reports another lupine-server address than the node publishes; using the agent's",
			"pod", klog.KObj(pod), "container", contClaim.Name, "agent", server.AgentEndpoint,
			"reported", serverEndpoint, "published", server.ServerEndpoint)
	}
	if serverEndpoint == "" {
		return nil, fmt.Errorf("container %s: remote-agent %s reports no lupine-server endpoint and none is published for node %s",
			contClaim.Name, server.AgentEndpoint, util.PodPlanSchedulingNode(pod))
	}

	response := &pluginapi.ContainerAllocateResponse{Envs: map[string]string{
		util.PodNameEnv:      pod.Name,
		util.PodNamespaceEnv: pod.Namespace,
		util.PodUIDEnv:       string(pod.UID),
		util.ContNameEnv:     contClaim.Name,
		// No local GPU is injected; every CUDA call goes to the server.
		"NVIDIA_VISIBLE_DEVICES":            "void",
		kubeletremote.EnvLupineDisableLocal: "1",
		kubeletremote.EnvLupineServer:       serverEndpoint,
		kubeletremote.EnvLupineSession:      session.Token,
	}}
	if artifact.ETag != "" && !m.opts.IgnoreClientShimEtag {
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
func (m *consumerRole) currentPod(ctx context.Context) (*corev1.Pod, error) {
	pods, err := client.GetActivePodsOnNode(ctx, m.kubeClient, m.nodeName)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve the active pods of the current node: %v", err)
	}
	return util.GetCurrentPodByAllocatingPods(util.FilterAllocatingPods(pods))
}

// serverEndpoints reads the addresses of the GPU server the scheduler chose
// for this pod, which is its predicate node.
func (m *consumerRole) serverEndpoints(ctx context.Context, pod *corev1.Pod) (*remotegpu.ServerEndpointInfo, error) {
	serverName := util.PodPlanSchedulingNode(pod)
	if serverName == "" || serverName == m.nodeName {
		return nil, fmt.Errorf("pod %s has no remote GPU server", klog.KObj(pod))
	}
	node, err := m.kubeClient.CoreV1().Nodes().Get(ctx, serverName, metav1.GetOptions{ResourceVersion: "0"})
	if err != nil {
		return nil, fmt.Errorf("get remote GPU server node %s: %w", serverName, err)
	}
	return remotegpu.GetServerEndpointInfo(node)
}
