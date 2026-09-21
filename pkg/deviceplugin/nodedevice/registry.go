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

// Package nodedevice publishes what a node's GPUs are, for the scheduler to
// read: the device registry, the node's vGPU configuration and the driver
// version labels. Every plugin that serves GPUs of this node needs it -- the
// local vGPU plugin and, on a node that also serves remote pods, the remote
// plugin -- so it lives here rather than in one of them.
package nodedevice

import (
	"fmt"
	"strconv"
	"sync"

	"github.com/coldzerofear/vgpu-manager/pkg/client"
	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/device/manager"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"k8s.io/component-base/featuregate"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
)

// Setup publishes this node's devices through the device manager's node
// registration under name, and has the metadata removed when the manager
// stops. A manager without devices publishes nothing: a node that only runs
// remote pods has no GPUs of its own to offer.
func Setup(name string, devManager *manager.DeviceManager) {
	if len(devManager.GetNodeDeviceInfo()) == 0 {
		klog.V(3).InfoS("Node has no local GPU devices to register", "registrar", name)
		return
	}
	publisher := &publisher{manager: devManager}
	devManager.AddRegistryFunc(name, publisher.registry)
	devManager.AddCleanupRegistryFunc(name, Cleanup)
}

// Remove stops publishing under name. It leaves the metadata on the node: the
// plugin may just be restarting, and Cleanup runs when the manager stops.
func Remove(name string, devManager *manager.DeviceManager) {
	devManager.RemoveRegistryFunc(name)
	devManager.RemoveCleanupRegistryFunc(name)
}

// publisher encodes the node's device metadata. The configuration and the
// topology never change while the process runs, so both are encoded once.
type publisher struct {
	manager *manager.DeviceManager

	configOnce   sync.Once
	config       string
	configErr    error
	topologyOnce sync.Once
	topology     string
	topologyErr  error
}

// registry is the node metadata this node's devices amount to.
func (p *publisher) registry(featureGate featuregate.FeatureGate) (*client.PatchMetadata, error) {
	registryGPUs, err := p.manager.GetNodeDeviceInfo().Encode()
	if err != nil {
		return nil, fmt.Errorf("encoding node device information failed: %v", err)
	}
	var registryGPUTopology *string
	if featureGate.Enabled(util.TopologyAwareGPUAllocation) {
		gpuTopology, err := p.encodeTopology()
		if err != nil {
			return nil, err
		}
		registryGPUTopology = &gpuTopology
	}
	nodeConfigEncode, err := p.encodeConfig()
	if err != nil {
		return nil, err
	}
	driverVersion := p.manager.GetDriverVersion()
	major, minor := driverVersion.CudaDriverVersion.MajorAndMinor()
	return &client.PatchMetadata{
		Annotations: map[string]*string{
			util.NodeConfigInfoAnnotation:     ptr.To(nodeConfigEncode),
			util.NodeDeviceRegisterAnnotation: ptr.To(registryGPUs),
			util.NodeDeviceTopologyAnnotation: registryGPUTopology,
		},
		Labels: map[string]*string{
			util.NodeNvidiaDriverVersionLabel: ptr.To(driverVersion.DriverVersion),
			util.NodeNvidiaCudaVersionLabel:   ptr.To(driverVersion.CudaDriverVersion.String()),
			util.NodeNvidiaCudaMajorLabel:     ptr.To(strconv.Itoa(int(major))),
			util.NodeNvidiaCudaMinorLabel:     ptr.To(strconv.Itoa(int(minor))),
		},
	}, nil
}

// Cleanup removes everything registry publishes.
func Cleanup(featuregate.FeatureGate) (*client.PatchMetadata, error) {
	return &client.PatchMetadata{
		Annotations: map[string]*string{
			// TODO Reserved for cleaning up after upgrading
			util.NodeDeviceHeartbeatAnnotation: nil,
			util.NodeDeviceRegisterAnnotation:  nil,
			util.NodeDeviceTopologyAnnotation:  nil,
			util.NodeConfigInfoAnnotation:      nil,
		},
		Labels: map[string]*string{
			util.NodeNvidiaDriverVersionLabel: nil,
			util.NodeNvidiaCudaVersionLabel:   nil,
			util.NodeNvidiaCudaMajorLabel:     nil,
			util.NodeNvidiaCudaMinorLabel:     nil,
		},
	}, nil
}

func (p *publisher) encodeTopology() (string, error) {
	p.topologyOnce.Do(func() {
		p.topology, p.topologyErr = p.manager.GetNodeTopologyInfo().Encode()
		if p.topologyErr != nil {
			p.topologyErr = fmt.Errorf("encoding node topology information failed: %v", p.topologyErr)
			return
		}
		klog.V(3).Infof("node GPU topology information: %s", p.topology)
	})
	return p.topology, p.topologyErr
}

func (p *publisher) encodeConfig() (string, error) {
	p.configOnce.Do(func() {
		nodeConfig := p.manager.GetNodeConfig()
		p.config, p.configErr = device.NodeConfigInfo{
			DeviceSplit:   nodeConfig.GetDeviceSplitCount(),
			CoresScaling:  nodeConfig.GetDeviceCoresScaling(),
			MemoryFactor:  nodeConfig.GetDeviceMemoryFactor(),
			MemoryScaling: nodeConfig.GetDeviceMemoryScaling(),
		}.Encode()
		if p.configErr != nil {
			p.configErr = fmt.Errorf("encoding node configuration information failed: %v", p.configErr)
			return
		}
		klog.V(3).Infof("node GPU configuration information: %s", p.config)
	})
	return p.config, p.configErr
}
