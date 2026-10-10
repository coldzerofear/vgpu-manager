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

package remoteagent

// Sessions owned by a Pod (device-plugin path): the node snapshot comes from
// what the device plugin publishes on the node, and the quota from the devices
// the scheduler pre-allocated to one container.

import (
	"fmt"
	"sort"

	"github.com/Masterminds/semver"
	vgpuconfig "github.com/coldzerofear/vgpu-manager/pkg/config/vgpu"
	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/device/remotegpu"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// NodeDevicesFromNode builds the snapshot from what the device plugin
// publishes on the node: the device registry annotation, the node config
// (memory scaling) and the CUDA / driver version labels. Devices are keyed by
// UUID, which is how pre-allocated devices name them.
func NodeDevicesFromNode(node *corev1.Node) (*NodeDevices, error) {
	registered, _ := util.HasAnnotation(node, util.NodeDeviceRegisterAnnotation)
	if registered == "" {
		return nil, fmt.Errorf("node %s has no %s annotation", node.Name, util.NodeDeviceRegisterAnnotation)
	}
	var devices device.NodeDeviceInfo
	if err := devices.Decode(registered); err != nil {
		return nil, fmt.Errorf("decode node device registry: %w", err)
	}
	memoryRatio := int64(util.HundredCore)
	if config, _ := util.HasAnnotation(node, util.NodeConfigInfoAnnotation); config != "" {
		var nodeConfig device.NodeConfigInfo
		if err := nodeConfig.Decode(config); err != nil {
			return nil, fmt.Errorf("decode node config: %w", err)
		}
		if nodeConfig.MemoryScaling > 0 {
			memoryRatio = int64(nodeConfig.MemoryScaling * util.HundredCore)
		}
	}

	nd := &NodeDevices{Devices: map[string]NodeDevice{}}
	for _, dev := range devices {
		if dev.Mig || dev.Uuid == "" || dev.Id < 0 || dev.Id >= vgpuconfig.MaxDeviceCount {
			continue
		}
		nd.Devices[dev.Uuid] = NodeDevice{
			Name: dev.Uuid, Minor: int64(dev.Id), UUID: dev.Uuid,
			MemoryMiB: dev.Memory, Cores: dev.Core, MemoryRatio: memoryRatio,
		}
	}
	// The plugin publishes both versions as node labels next to the registry.
	if version, _ := util.HasLabel(node, util.NodeNvidiaCudaVersionLabel); version != "" {
		if v, err := semver.NewVersion(version); err == nil {
			nd.CudaVersion = v
		}
	}
	if version, _ := util.HasLabel(node, util.NodeNvidiaDriverVersionLabel); version != "" {
		if v, err := semver.NewVersion(version); err == nil {
			nd.DriverVersion = v
		}
	}
	return nd, nil
}

// PodSessionSpec is the session of one container of a remote pod: the devices
// the scheduler pre-allocated to it, which are this node's by the time the
// agent is asked (the caller checks the pod's predicate node).
func PodSessionSpec(pod *corev1.Pod, containerName string, nd *NodeDevices) (SessionSpec, error) {
	preAllocated, _ := util.HasAnnotation(pod, util.PodVGPUPreAllocAnnotation)
	var podClaims device.PodDeviceClaim
	if err := podClaims.UnmarshalText(preAllocated); err != nil {
		return SessionSpec{}, fmt.Errorf("parse pre-allocated devices of pod %s: %w", klog.KObj(pod), err)
	}
	spec := SessionSpec{
		Owner: SessionOwner{
			Kind: OwnerPod, UID: string(pod.UID), Namespace: pod.Namespace,
			Name: pod.Name, Version: objectRV(pod.ResourceVersion),
		},
		MemoryRatio: 1,
	}
	for _, container := range podClaims {
		if container.Name != containerName {
			continue
		}
		claims := append([]device.DeviceClaim(nil), container.DeviceClaims...)
		sort.Slice(claims, func(i, j int) bool { return claims[i].Id < claims[j].Id })
		for _, claim := range claims {
			dev, ok := nd.Devices[claim.Uuid]
			if !ok {
				return SessionSpec{}, fmt.Errorf("device %s of container %s is not registered on this node", claim.Uuid, containerName)
			}
			// Slot = host device index, as on the DRA path: the pre-allocated
			// id is the scheduler's view of the same index.
			slot := int(dev.Minor)
			spec.Infos = append(spec.Infos, device.DeviceClaim{Id: slot, Uuid: dev.UUID, Cores: dev.Cores, Memory: dev.MemoryMiB})
			spec.Claims = append(spec.Claims, device.DeviceClaim{Id: slot, Uuid: dev.UUID, Cores: claim.Cores, Memory: claim.Memory})
			spec.MemoryRatio = float64(dev.MemoryRatio) / float64(util.HundredCore)
		}
		return spec, nil
	}
	return SessionSpec{}, fmt.Errorf("pod %s has no pre-allocated devices for container %s", klog.KObj(pod), containerName)
}

// PodSessionTokens is the set of sessions a pod may have on this node: one per
// container the scheduler pre-allocated devices to. A sweep keeps these and
// removes the rest.
func PodSessionTokens(pod *corev1.Pod) []string {
	containers := podDeviceContainers(pod)
	tokens := make([]string, 0, len(containers))
	for _, container := range containers {
		tokens = append(tokens, remotegpu.SessionToken(string(pod.UID), container))
	}
	return tokens
}

// PodSessionContainer returns the container of the pod whose session token is
// token. This is what authorizes a session request: only a container the
// scheduler gave devices to has a token.
func PodSessionContainer(pod *corev1.Pod, token string) (string, bool) {
	for _, container := range podDeviceContainers(pod) {
		if remotegpu.SessionToken(string(pod.UID), container) == token {
			return container, true
		}
	}
	return "", false
}

// podDeviceContainers names the containers the scheduler pre-allocated devices to.
func podDeviceContainers(pod *corev1.Pod) []string {
	preAllocated, _ := util.HasAnnotation(pod, util.PodVGPUPreAllocAnnotation)
	var podClaims device.PodDeviceClaim
	if err := podClaims.UnmarshalText(preAllocated); err != nil {
		return nil
	}
	containers := make([]string, 0, len(podClaims))
	for _, container := range podClaims {
		if len(container.DeviceClaims) > 0 {
			containers = append(containers, container.Name)
		}
	}
	return containers
}
