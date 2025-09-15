/*
 * Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"slices"

	"github.com/coldzerofear/vgpu-manager/pkg/device/nvidia"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
)

type PreparedDeviceList []PreparedDevice
type PreparedDevices []*PreparedDeviceGroup

type PreparedDevice struct {
	Gpu *PreparedGpu       `json:"gpu"`
	Mig *PreparedMigDevice `json:"mig"`
}

type PreparedGpu struct {
	Info   *nvidia.GpuInfo       `json:"info"`
	Device *kubeletplugin.Device `json:"device"`
}

type PreparedMigDevice struct {
	Info   *nvidia.MigInfo       `json:"info"`
	Device *kubeletplugin.Device `json:"device"`
}

type PreparedDeviceGroup struct {
	Devices     PreparedDeviceList `json:"devices"`
	ConfigState DeviceConfigState  `json:"configState"`
}

func (d PreparedDevice) Type() string {
	if d.Gpu != nil {
		return nvidia.GpuDeviceType
	}
	if d.Mig != nil {
		return nvidia.MigDeviceType
	}
	return nvidia.UnknownDeviceType
}

func (d *PreparedDevice) CanonicalName() string {
	switch d.Type() {
	case nvidia.GpuDeviceType:
		return d.Gpu.Info.CanonicalName()
	case nvidia.MigDeviceType:
		return d.Mig.Info.CanonicalName()
	}
	panic("unexpected type for AllocatableDevice")
}

func (d *PreparedDevice) CanonicalIndex() string {
	switch d.Type() {
	case nvidia.GpuDeviceType:
		return d.Gpu.Info.CanonicalIndex()
	case nvidia.MigDeviceType:
		return d.Mig.Info.CanonicalIndex()
	}
	panic("unexpected type for AllocatableDevice")
}

func (l PreparedDeviceList) Gpus() PreparedDeviceList {
	var devices PreparedDeviceList
	for _, device := range l {
		if device.Type() == nvidia.GpuDeviceType {
			devices = append(devices, device)
		}
	}
	return devices
}

func (l PreparedDeviceList) MigDevices() PreparedDeviceList {
	var devices PreparedDeviceList
	for _, device := range l {
		if device.Type() == nvidia.MigDeviceType {
			devices = append(devices, device)
		}
	}
	return devices
}

func (d PreparedDevices) GetDevices() []kubeletplugin.Device {
	var devices []kubeletplugin.Device
	for _, group := range d {
		devices = append(devices, group.GetDevices()...)
	}
	return devices
}

func (g *PreparedDeviceGroup) GetDevices() []kubeletplugin.Device {
	var devices []kubeletplugin.Device
	for _, device := range g.Devices {
		switch device.Type() {
		case nvidia.GpuDeviceType:
			devices = append(devices, *device.Gpu.Device)
		case nvidia.MigDeviceType:
			devices = append(devices, *device.Mig.Device)
		}
	}
	return devices
}

func (l PreparedDeviceList) UUIDs() []string {
	uuids := append(l.GpuUUIDs(), l.MigUUIDs()...)
	slices.Sort(uuids)
	return uuids
}

func (g *PreparedDeviceGroup) UUIDs() []string {
	uuids := append(g.GpuUUIDs(), g.MigUUIDs()...)
	slices.Sort(uuids)
	return uuids
}

func (d PreparedDevices) UUIDs() []string {
	uuids := append(d.GpuUUIDs(), d.MigUUIDs()...)
	slices.Sort(uuids)
	return uuids
}

func (l PreparedDeviceList) GpuUUIDs() []string {
	var uuids []string
	for _, device := range l.Gpus() {
		uuids = append(uuids, device.Gpu.Info.UUID)
	}
	slices.Sort(uuids)
	return uuids
}

func (g *PreparedDeviceGroup) GpuUUIDs() []string {
	return g.Devices.Gpus().UUIDs()
}

func (d PreparedDevices) GpuUUIDs() []string {
	var uuids []string
	for _, group := range d {
		uuids = append(uuids, group.GpuUUIDs()...)
	}
	slices.Sort(uuids)
	return uuids
}

func (l PreparedDeviceList) MigUUIDs() []string {
	var uuids []string
	for _, device := range l.MigDevices() {
		uuids = append(uuids, device.Mig.Info.UUID)
	}
	slices.Sort(uuids)
	return uuids
}

func (g *PreparedDeviceGroup) MigUUIDs() []string {
	return g.Devices.MigDevices().UUIDs()
}

func (d PreparedDevices) MigUUIDs() []string {
	var uuids []string
	for _, group := range d {
		uuids = append(uuids, group.MigUUIDs()...)
	}
	slices.Sort(uuids)
	return uuids
}
