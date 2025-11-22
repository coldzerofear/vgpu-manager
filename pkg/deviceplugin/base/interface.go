package base

import (
	"github.com/coldzerofear/vgpu-manager/pkg/device/manager"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
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

type PluginServer interface {
	GetDeviceManager() *manager.DeviceManager
	GetStopCh() chan struct{}
	GetDeviceCh() chan *manager.Device
	GetResourceName() string
	Start(name string, server pluginapi.DevicePluginServer) error
	Stop(name string) error
}
