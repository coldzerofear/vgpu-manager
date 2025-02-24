package deviceplugin

import (
	"context"
	"fmt"

	"github.com/coldzerofear/vgpu-manager/pkg/device/manager"
	"k8s.io/klog/v2"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

type vmemoryDevicePlugin struct {
	base *baseDevicePlugin
}

var _ DevicePlugin = &vmemoryDevicePlugin{}

// NewVMemoryDevicePlugin returns an initialized vmemoryDevicePlugin
func NewVMemoryDevicePlugin(resourceName, socket string, manager *manager.DeviceManager) DevicePlugin {
	return &vmemoryDevicePlugin{base: newBaseDevicePlugin(resourceName, socket, manager)}
}

func (m *vmemoryDevicePlugin) Name() string {
	return "vmemory-plugin"
}

// Start starts the gRPC server, registers the device plugin with the Kubelet.
func (m *vmemoryDevicePlugin) Start() error {
	return m.base.Start(m.Name(), m)
}

// Stop stops the gRPC server.
func (m *vmemoryDevicePlugin) Stop() error {
	return m.base.Stop(m.Name())
}

// GetDevicePluginOptions returns options to be communicated with Device Manager
func (m *vmemoryDevicePlugin) GetDevicePluginOptions(_ context.Context, _ *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	return &pluginapi.DevicePluginOptions{}, nil
}

// ListAndWatch returns a stream of List of Devices
// Whenever a Device state change or a Device disappears, ListAndWatch
// returns the new list
func (m *vmemoryDevicePlugin) ListAndWatch(_ *pluginapi.Empty, s pluginapi.DevicePlugin_ListAndWatchServer) error {
	if err := s.Send(&pluginapi.ListAndWatchResponse{Devices: m.Devices()}); err != nil {
		klog.Errorf("DevicePlugin '%s' ListAndWatch send devices error: %v", m.Name(), err)
	}
	stopCh := m.base.stop
	for {
		select {
		case d := <-m.base.health:
			if d.GPU != nil {
				klog.Infof("'%s' device marked unhealthy: %s", m.base.resourceName, d.GPU.UUID)
				if err := s.Send(&pluginapi.ListAndWatchResponse{Devices: m.Devices()}); err != nil {
					klog.Errorf("DevicePlugin '%s' ListAndWatch send devices error: %v", m.Name(), err)
				}
			}
		case <-stopCh:
			return nil
		}
	}
}

// GetPreferredAllocation returns the preferred allocation from the set of devices specified in the request
func (m *vmemoryDevicePlugin) GetPreferredAllocation(_ context.Context, _ *pluginapi.PreferredAllocationRequest) (*pluginapi.PreferredAllocationResponse, error) {
	return &pluginapi.PreferredAllocationResponse{}, nil
}

// Allocate is called during container creation so that the Device
// Plugin can run device specific operations and instruct Kubelet
// of the steps to make the Device available in the container
func (m *vmemoryDevicePlugin) Allocate(_ context.Context, req *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	responses := make([]*pluginapi.ContainerAllocateResponse, len(req.ContainerRequests))
	for i := range req.ContainerRequests {
		responses[i] = &pluginapi.ContainerAllocateResponse{}
	}
	return &pluginapi.AllocateResponse{ContainerResponses: responses}, nil
}

// PreStartContainer is called, if indicated by Device Plugin during registeration phase,
// before each container start. Device plugin can run device specific operations
// such as resetting the device before making devices available to the container
func (m *vmemoryDevicePlugin) PreStartContainer(_ context.Context, _ *pluginapi.PreStartContainerRequest) (*pluginapi.PreStartContainerResponse, error) {
	return &pluginapi.PreStartContainerResponse{}, nil
}

func (m *vmemoryDevicePlugin) Devices() []*pluginapi.Device {
	var devices []*pluginapi.Device
	for _, gpuDevice := range m.base.manager.GetNodeDeviceInfos() {
		if gpuDevice.Mig { // skip mig device
			continue
		}
		config := m.base.manager.GetNodeConfig()
		for i := 0; i < gpuDevice.Memory/config.DeviceMemoryFactor(); i++ {
			devId := fmt.Sprintf("vmemory-%d-%d", gpuDevice.Id, i)
			health := pluginapi.Healthy
			if !gpuDevice.Healthy {
				health = pluginapi.Unhealthy
			}
			devices = append(devices, &pluginapi.Device{
				ID:       devId,
				Health:   health,
				Topology: nil,
			})
		}
	}
	return devices
}
