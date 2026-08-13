/*
Copyright 2024-2026 coldzerofear

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
	Start(name string, server DevicePlugin) error
	Stop(name string) error
}
