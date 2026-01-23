/*
 * Copyright (c) 2023, NVIDIA CORPORATION.  All rights reserved.
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

package kubeletplugin

import (
	"fmt"
	"slices"

	configapi "github.com/NVIDIA/k8s-dra-driver-gpu/api/nvidia.com/resource/v1beta1"
)

type TimeSlicingManager struct {
	nvdevlib *deviceLib
}

type MpsManager struct {
	config           *Config
	controlFilesRoot string
	hostDriverRoot   string
	templatePath     string

	nvdevlib *deviceLib
}

type MpsControlDaemon struct {
	id        string
	nodeName  string
	namespace string
	name      string
	rootDir   string
	pipeDir   string
	shmDir    string
	logDir    string
	devices   UUIDProvider
	manager   *MpsManager
}

func NewTimeSlicingManager(deviceLib *deviceLib) *TimeSlicingManager {
	return &TimeSlicingManager{
		nvdevlib: deviceLib,
	}
}

func (t *TimeSlicingManager) SetTimeSlice(devices UUIDProvider, config *configapi.TimeSlicingConfig) error {
	// Ensure all devices are full devices
	if !slices.Equal(devices.UUIDs(), devices.GpuUUIDs()) {
		return fmt.Errorf("can only set the time-slice interval on full GPUs")
	}

	// Set the compute mode of the GPU to DEFAULT.
	err := t.nvdevlib.SetComputeMode(devices.UUIDs(), "DEFAULT")
	if err != nil {
		return fmt.Errorf("error setting compute mode: %w", err)
	}

	// Set the time slice based on the config provided.
	err = t.nvdevlib.SetTimeSlice(devices.UUIDs(), config.Interval.Int())
	if err != nil {
		return fmt.Errorf("error setting time slice: %w", err)
	}

	return nil
}
