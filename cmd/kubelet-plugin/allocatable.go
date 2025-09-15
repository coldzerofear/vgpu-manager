/*
 * Copyright (c) 2022, NVIDIA CORPORATION.  All rights reserved.
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
	"fmt"
	"slices"

	"github.com/coldzerofear/vgpu-manager/pkg/device/nvidia"
	resourceapi "k8s.io/api/resource/v1"
)

type AllocatableDevices map[string]*AllocatableDevice

type AllocatableDevice struct {
	GpuInfo *nvidia.GpuInfo
	MigInfo *nvidia.MigInfo
}

func (d AllocatableDevice) Type() string {
	if d.GpuInfo != nil {
		return nvidia.GpuDeviceType
	}
	if d.MigInfo != nil {
		return nvidia.MigDeviceType
	}
	return nvidia.UnknownDeviceType
}

func (d *AllocatableDevice) CanonicalName() string {
	switch d.Type() {
	case nvidia.GpuDeviceType:
		return fmt.Sprintf("gpu-%d", d.GpuInfo.Minor)
	case nvidia.VGpuDeviceType:
		return fmt.Sprintf("vgpu-%d", d.GpuInfo.Minor)
	case nvidia.MigDeviceType:
		return fmt.Sprintf("gpu-%d-mig-%d-%d-%d",
			d.MigInfo.Parent.Minor, d.MigInfo.GiInfo.ProfileId,
			d.MigInfo.Placement.Start, d.MigInfo.Placement.Size)
	}
	panic("unexpected type for AllocatableDevice")
}

func (d *AllocatableDevice) CanonicalIndex() string {
	switch d.Type() {
	case nvidia.GpuDeviceType:
		return d.GpuInfo.CanonicalIndex()
	case nvidia.VGpuDeviceType:

	case nvidia.MigDeviceType:
		return d.MigInfo.CanonicalIndex()
	}
	panic("unexpected type for AllocatableDevice")
}

func (d *AllocatableDevice) GetDevice() resourceapi.Device {
	switch d.Type() {
	case nvidia.GpuDeviceType:
		return d.GpuInfo.GetDevice()
	case nvidia.MigDeviceType:
		return d.MigInfo.GetDevice()
	}
	panic("unexpected type for AllocatableDevice")
}

func (d AllocatableDevices) GpuUUIDs() []string {
	var uuids []string
	for _, device := range d {
		if device.Type() == nvidia.GpuDeviceType {
			uuids = append(uuids, device.GpuInfo.UUID)
		}
	}
	slices.Sort(uuids)
	return uuids
}

func (d AllocatableDevices) MigUUIDs() []string {
	var uuids []string
	for _, device := range d {
		if device.Type() == nvidia.MigDeviceType {
			uuids = append(uuids, device.MigInfo.UUID)
		}
	}
	slices.Sort(uuids)
	return uuids
}

func (d AllocatableDevices) UUIDs() []string {
	uuids := append(d.GpuUUIDs(), d.MigUUIDs()...)
	slices.Sort(uuids)
	return uuids
}
