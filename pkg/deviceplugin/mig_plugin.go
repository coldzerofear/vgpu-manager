package deviceplugin

import (
	"context"
	"fmt"
	"strings"

	"github.com/coldzerofear/vgpu-manager/pkg/device/manager"
	"github.com/coldzerofear/vgpu-manager/pkg/device/nvidia"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"k8s.io/klog/v2"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

type migDevicePlugin struct {
	base *baseDevicePlugin
}

var _ DevicePlugin = &migDevicePlugin{}

// NewMigDevicePlugin returns an initialized migDevicePlugin.
func NewMigDevicePlugin(resourceName, socket string, manager *manager.DeviceManager) DevicePlugin {
	return &migDevicePlugin{base: newBaseDevicePlugin(resourceName, socket, manager)}
}

func (m *migDevicePlugin) Name() string {
	return "mig-plugin"
}

// Start starts the gRPC server, registers the device plugin with the Kubelet.
func (m *migDevicePlugin) Start() error {
	return m.base.Start(m.Name(), m)
}

// Stop stops the gRPC server.
func (m *migDevicePlugin) Stop() error {
	return m.base.Stop(m.Name())
}

// GetDevicePluginOptions returns options to be communicated with Device Manager.
func (m *migDevicePlugin) GetDevicePluginOptions(_ context.Context, _ *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	return &pluginapi.DevicePluginOptions{}, nil
}

func (m *migDevicePlugin) relatedParentDevice(parentUUID string) bool {
	for _, migDevice := range m.base.manager.GetMIGDeviceMap() {
		if migDevice.Parent.UUID == parentUUID && m.base.resourceName == GetMigResourceName(migDevice.MigInfo) {
			return true
		}
	}
	return false
}

// ListAndWatch returns a stream of List of Devices,
// Whenever a Device state change or a Device disappears,
// ListAndWatch returns the new list.
func (m *migDevicePlugin) ListAndWatch(_ *pluginapi.Empty, s pluginapi.DevicePlugin_ListAndWatchServer) error {
	klog.V(4).InfoS("ListAndWatch", "pluginName", m.Name(), "server", s)
	if err := s.Send(&pluginapi.ListAndWatchResponse{Devices: m.Devices()}); err != nil {
		klog.Errorf("DevicePlugin '%s' ListAndWatch send devices error: %v", m.Name(), err)
	}
	stopCh := m.base.stop
	for {
		select {
		case d := <-m.base.health:
			// If MIG devices related to resources are marked as unhealthy, resend the device list.
			if d.MIG != nil && m.base.resourceName == GetMigResourceName(d.MIG.MigInfo) {
				klog.Infof("'%s' device marked unhealthy: %s", m.base.resourceName, d.MIG.UUID)
				if err := s.Send(&pluginapi.ListAndWatchResponse{Devices: m.Devices()}); err != nil {
					klog.Errorf("DevicePlugin '%s' ListAndWatch send devices error: %v", m.Name(), err)
				}
			}
			// If the parent device of a resource related MIG device is marked as unhealthy, resend the device list.
			if d.GPU != nil && d.GPU.MigEnabled && m.relatedParentDevice(d.GPU.UUID) {
				klog.Infof("'%s' parent device marked unhealthy: %s", m.base.resourceName, d.GPU.UUID)
				if err := s.Send(&pluginapi.ListAndWatchResponse{Devices: m.Devices()}); err != nil {
					klog.Errorf("DevicePlugin '%s' ListAndWatch send devices error: %v", m.Name(), err)
				}
			}
		case <-stopCh:
			return nil
		}
	}
}

// GetPreferredAllocation returns the preferred allocation from the set of devices specified in the request.
func (m *migDevicePlugin) GetPreferredAllocation(_ context.Context, _ *pluginapi.PreferredAllocationRequest) (*pluginapi.PreferredAllocationResponse, error) {
	return &pluginapi.PreferredAllocationResponse{}, nil
}

// Allocate is called during container creation so that the Device
// Plugin can run device specific operations and instruct Kubelet
// of the steps to make the Device available in the container.
func (m *migDevicePlugin) Allocate(_ context.Context, req *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	klog.V(4).InfoS("Allocate", "pluginName", m.Name(), "request", req.GetContainerRequests())
	nodeConfig := m.base.manager.GetNodeConfig()
	deviceMap := m.base.manager.GetMIGDeviceMap()
	responses := make([]*pluginapi.ContainerAllocateResponse, len(req.ContainerRequests))
	for i, containerRequest := range req.ContainerRequests {
		deviceIds := containerRequest.GetDevicesIDs()
		responses[i] = &pluginapi.ContainerAllocateResponse{}
		err := updateResponseForNodeConfig(responses[i], nodeConfig, deviceIds...)
		if err != nil {
			klog.Errorln(err)
			return nil, err
		}
		var devices []manager.Device
		for _, uuid := range deviceIds {
			migDevice, exists := deviceMap[uuid]
			if !exists {
				err = fmt.Errorf("MIG device %s does not exist", uuid)
				klog.Errorln(err)
				return nil, err
			}
			devices = append(devices, manager.Device{MIG: &migDevice})
		}
		responses[i].Devices = append(responses[i].Devices, passDeviceSpecs(devices)...)
	}
	return &pluginapi.AllocateResponse{ContainerResponses: responses}, nil
}

// PreStartContainer is called, if indicated by Device Plugin during registeration phase,
// before each container start. Device plugin can run device specific operations
// such as resetting the device before making devices available to the container.
func (m *migDevicePlugin) PreStartContainer(_ context.Context, _ *pluginapi.PreStartContainerRequest) (*pluginapi.PreStartContainerResponse, error) {
	return &pluginapi.PreStartContainerResponse{}, nil
}

func GetMigResourceName(migInfo *nvidia.MigInfo) string {
	resource := strings.ReplaceAll(migInfo.Profile, "+", ".")
	return util.MIGDeviceResourceNamePrefix + resource
}

func (m *migDevicePlugin) Devices() []*pluginapi.Device {
	var devices []*pluginapi.Device
	deviceMap := m.base.manager.GetGPUDeviceMap()
	for uuid, migDevice := range m.base.manager.GetMIGDeviceMap() {
		gpuDevice, ok := deviceMap[migDevice.Parent.UUID]
		if !ok || !gpuDevice.Healthy { // Skip if the parent device is unhealthy
			continue
		}
		if m.base.resourceName == GetMigResourceName(migDevice.MigInfo) {
			health := pluginapi.Healthy
			if !migDevice.Healthy {
				health = pluginapi.Unhealthy
			}
			devices = append(devices, &pluginapi.Device{
				ID:     uuid,
				Health: health,
				Topology: &pluginapi.TopologyInfo{
					Nodes: []*pluginapi.NUMANode{{
						ID: int64(gpuDevice.NumaNode),
					}},
				},
			})
		}
	}
	return devices
}
