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

// What one container needs right before it starts: its session on the GPU
// server, and the client shim it will load. Allocate already tried the
// session so a healthy node has nothing left to do here; this is the attempt
// that must succeed, and the one that covers a container Allocate could not
// reach the agent for.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Masterminds/semver"
	"github.com/coldzerofear/vgpu-manager/pkg/client"
	"github.com/coldzerofear/vgpu-manager/pkg/device/remotegpu"
	"github.com/coldzerofear/vgpu-manager/pkg/deviceplugin/checkpoint"
	kubeletremote "github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin/remote"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
	"k8s.io/kubelet/pkg/apis/podresources/v1alpha1"
)

// containerMatch is a container kubelet says holds a set of device ids.
type containerMatch struct {
	pod       *corev1.Pod
	container string
}

// PreStartContainer prepares every container the request can belong to.
//
// kubelet passes device ids and no container name, and it lets a sequential
// init container's ids be reused by the app container, so one id set can
// belong to two containers. Picking one of them would leave the other without
// a session -- the bug the local plugin ran into -- so every match is
// prepared. Sessions are per container and idempotent, which makes that safe.
func (m *consumerDevicePlugin) PreStartContainer(
	ctx context.Context, req *pluginapi.PreStartContainerRequest,
) (*pluginapi.PreStartContainerResponse, error) {
	resp := &pluginapi.PreStartContainerResponse{}
	klog.V(4).InfoS("PreStartContainer", "pluginName", m.Name(), "deviceIDs", req.GetDevicesIds())

	matches, err := m.lookup(ctx, req.GetDevicesIds())
	if err != nil {
		return resp, fmt.Errorf("%s: %s", util.PreStartContainerCheckErrMsg, err.Error())
	}
	for _, match := range matches {
		if err := m.prepareContainer(ctx, match.pod, match.container); err != nil {
			klog.ErrorS(err, util.PreStartContainerCheckErrMsg,
				"pod", klog.KObj(match.pod), "container", match.container)
			return resp, fmt.Errorf("%s: %s", util.PreStartContainerCheckErrMsg, err.Error())
		}
	}
	return resp, nil
}

// prepareContainer stages the client shim and makes sure the session exists.
func (m *consumerDevicePlugin) prepareContainer(ctx context.Context, pod *corev1.Pod, containerName string) error {
	server, err := m.serverEndpoints(ctx, pod)
	if err != nil {
		return err
	}
	if err = m.stageClientShim(pod, containerName, server); err != nil {
		return err
	}
	_, err = m.ensureSession(ctx, server.AgentEndpoint, m.podSession(pod, containerName))
	return err
}

// podSession is how the agent is asked for one container's session.
func (m *consumerDevicePlugin) podSession(pod *corev1.Pod, containerName string) remotegpu.PodSession {
	return remotegpu.PodSession{
		Token:           remotegpu.SessionToken(string(pod.UID), containerName),
		PodUID:          string(pod.UID),
		PodNamespace:    pod.Namespace,
		PodName:         pod.Name,
		ResourceVersion: pod.ResourceVersion,
	}
}

// stageClientShim points the container's two mount sources at the client shim
// built for this server and at its preload list. Allocate declared those
// paths; these links are what make them resolve.
func (m *consumerDevicePlugin) stageClientShim(
	pod *corev1.Pod, containerName string, server *remotegpu.ServerEndpointInfo,
) error {
	// A client must never be newer than the server it talks to, so the
	// server's build version is the ceiling. Until the server reports it,
	// nothing may be staged.
	version, err := semver.NewVersion(server.ServerCUDAVersion)
	if err != nil {
		return fmt.Errorf("remote GPU server reports no usable CUDA version (%q): %w", server.ServerCUDAVersion, err)
	}
	artifact, err := kubeletremote.StageClientArtifact(m.cfg.ArtifactsDir, m.cfg.HostArtifactsDir, version)
	if err != nil {
		return err
	}
	contDir, _ := m.containerPaths(pod.UID, containerName)
	if err = linkTo(filepath.Join(contDir, driverLinkName), artifact.HostDir); err != nil {
		return err
	}
	return linkTo(filepath.Join(contDir, ldPreloadFileName), artifact.LdPreloadHost)
}

// linkTo points path at target, replacing what is there. The target is a host
// path: the container runtime resolves it when it binds the mount.
func linkTo(path, target string) error {
	if current, err := os.Readlink(path); err == nil && current == target {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	if err := os.Symlink(target, path); err != nil {
		return fmt.Errorf("link %s -> %s: %w", path, target, err)
	}
	return nil
}

// lookupByDeviceIDs asks kubelet which containers hold these device ids: the
// pod-resources API first, the device-plugin checkpoint when it is not
// answering. Both can report several containers for one id set.
func (m *consumerDevicePlugin) lookupByDeviceIDs(ctx context.Context, deviceIDs []string) ([]containerMatch, error) {
	if len(deviceIDs) == 0 {
		return nil, fmt.Errorf("deviceIDs cannot be empty")
	}
	wanted := sets.New(deviceIDs...)
	pods, err := client.GetActivePodsOnNode(ctx, m.kubeClient, m.cfg.NodeName)
	if err != nil {
		return nil, fmt.Errorf("list the active pods of this node: %w", err)
	}

	matches, err := m.matchPodResources(ctx, wanted, pods)
	if err != nil {
		klog.ErrorS(err, "ListPodResource failed, fallback to checkpoint")
		matches, err = m.matchCheckpoint(wanted, pods)
		if err != nil {
			return nil, err
		}
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("no container of this node holds devices %v", deviceIDs)
	}
	return matches, nil
}

// matchPodResources reads kubelet's own view of who holds which devices.
func (m *consumerDevicePlugin) matchPodResources(
	ctx context.Context, wanted sets.Set[string], pods []corev1.Pod,
) ([]containerMatch, error) {
	resp, err := m.podResource.ListPodResource(ctx)
	if err != nil {
		return nil, err
	}
	var matches []containerMatch
	for _, podResources := range resp.GetPodResources() {
		pod := findPod(pods, podResources.GetNamespace(), podResources.GetName())
		if pod == nil {
			continue
		}
		for _, container := range podResources.GetContainers() {
			if holdsDevices(container.GetDevices(), wanted) {
				matches = append(matches, containerMatch{pod: pod, container: container.GetName()})
			}
		}
	}
	return matches, nil
}

// matchCheckpoint reads the same from kubelet's device-plugin checkpoint,
// which survives a pod-resources API that is not answering.
func (m *consumerDevicePlugin) matchCheckpoint(wanted sets.Set[string], pods []corev1.Pod) ([]containerMatch, error) {
	data, err := checkpoint.GetDevicePluginCheckpointData(m.cfg.DevicePluginPath)
	if err != nil {
		return nil, fmt.Errorf("read device plugin checkpoint: %w", err)
	}
	var matches []containerMatch
	for _, entry := range data.PodDeviceEntries {
		if entry.ResourceName != m.cfg.ResourceName ||
			wanted.Len() != len(entry.DeviceIDs) || !wanted.HasAll(entry.DeviceIDs...) {
			continue
		}
		for i := range pods {
			if string(pods[i].UID) == entry.PodUID {
				matches = append(matches, containerMatch{pod: &pods[i], container: entry.ContainerName})
				break
			}
		}
	}
	return matches, nil
}

// holdsDevices reports whether a container was given exactly this device set
// of the plugin's resource.
func holdsDevices(devices []*v1alpha1.ContainerDevices, wanted sets.Set[string]) bool {
	for _, allocated := range devices {
		if allocated.GetResourceName() != util.VGPUNumberResourceName {
			continue
		}
		if wanted.Len() == len(allocated.GetDeviceIds()) && wanted.HasAll(allocated.GetDeviceIds()...) {
			return true
		}
	}
	return false
}

func findPod(pods []corev1.Pod, namespace, name string) *corev1.Pod {
	for i := range pods {
		if pods[i].Namespace == namespace && pods[i].Name == name {
			return &pods[i]
		}
	}
	return nil
}
