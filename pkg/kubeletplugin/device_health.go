/*
 * Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"k8s.io/klog/v2"
)

const (
	FullGPUInstanceID uint32 = 0xFFFFFFFF
)

// For a MIG device the placement is defined by the 3-tuple <parent UUID, GI, CI>.
// For a full device the returned 3-tuple is the device's uuid and (FullGPUInstanceID) 0xFFFFFFFF for the other two elements.
type devicePlacementMap map[string]map[uint32]map[uint32]*AllocatableDevice

type nvmlDeviceHealthMonitor struct {
	nvmllib           nvml.Interface
	eventSet          nvml.EventSet
	unhealthy         chan *AllocatableDevice
	deviceByPlacement devicePlacementMap
	skippedXids       map[uint64]bool
	wg                sync.WaitGroup
}

func newNvmlDeviceHealthMonitor(config *Config, allocatable AllocatableDevices, nvdevlib *deviceLib) (*nvmlDeviceHealthMonitor, error) {
	if nvdevlib == nil {
		return nil, fmt.Errorf("nvml library is nil")
	}
	if ret := nvdevlib.NvmlInit(); ret != nvml.SUCCESS {
		return nil, fmt.Errorf("failed to initialize NVML: %v", ret)
	}
	defer nvdevlib.NvmlShutdown()

	m := &nvmlDeviceHealthMonitor{
		nvmllib:           nvdevlib,
		unhealthy:         make(chan *AllocatableDevice, len(allocatable)),
		deviceByPlacement: getDevicePlacementMap(allocatable),
		skippedXids:       xidsToSkip(config.Flags.AdditionalXidsToIgnore),
	}
	return m, nil
}

func (m *nvmlDeviceHealthMonitor) Start(ctx context.Context) (rerr error) {
	if ret := m.nvmllib.Init(); ret != nvml.SUCCESS {
		return fmt.Errorf("failed to initialize NVML: %v", ret)
	}

	defer func() {
		if rerr != nil {
			_ = m.nvmllib.Shutdown()
		}
	}()

	klog.V(4).Info("creating NVML events for device health monitor")
	eventSet, ret := m.nvmllib.EventSetCreate()
	if ret != nvml.SUCCESS {
		return fmt.Errorf("failed to create event set: %w", ret)
	}

	m.eventSet = eventSet

	klog.V(4).Info("registering NVML events for device health monitor")
	m.registerEventsForDevices()

	m.wg.Go(func() {
		m.run(ctx)
	})

	klog.V(4).Info("started device health monitoring")
	return nil
}

func (m *nvmlDeviceHealthMonitor) registerEventsForDevices() {
	eventMask := uint64(nvml.EventTypeXidCriticalError | nvml.EventTypeDoubleBitEccError | nvml.EventTypeSingleBitEccError)

	for parentUUID, giMap := range m.deviceByPlacement {
		gpu, ret := m.nvmllib.DeviceGetHandleByUUID(parentUUID)
		if ret != nvml.SUCCESS {
			klog.Warningf("Unable to get device handle from UUID[%s]: %v; marking it as unhealthy", parentUUID, ret)
			m.markAllMigDevicesUnhealthy(giMap)
			continue
		}

		supportedEvents, ret := gpu.GetSupportedEventTypes()
		if ret != nvml.SUCCESS {
			klog.Warningf("unable to determine the supported events for %s: %v; marking it as unhealthy", parentUUID, ret)
			m.markAllMigDevicesUnhealthy(giMap)
			continue
		}

		ret = gpu.RegisterEvents(eventMask&supportedEvents, m.eventSet)
		if ret == nvml.ERROR_NOT_SUPPORTED {
			klog.Warningf("Device %v is too old to support healthchecking.", parentUUID)
		}
		if ret != nvml.SUCCESS {
			klog.Warningf("unable to register events for %s: %v; marking it as unhealthy", parentUUID, ret)
			m.markAllMigDevicesUnhealthy(giMap)
		}
	}
}

func (m *nvmlDeviceHealthMonitor) Stop() {
	if m == nil {
		return
	}
	klog.V(6).Info("stopping health monitor")

	m.wg.Wait()

	if ret := m.eventSet.Free(); ret != nvml.SUCCESS {
		klog.Warningf("failed to unset events: %v", ret)
	}

	if ret := m.nvmllib.Shutdown(); ret != nvml.SUCCESS {
		klog.Warningf("failed to shutdown NVML: %v", ret)
	}
	close(m.unhealthy)
}

func (m *nvmlDeviceHealthMonitor) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			klog.V(6).Info("Stopping event-driven GPU health monitor...")
			return
		default:
			event, ret := m.eventSet.Wait(5000) // timeout in 5000 ms.
			if ret == nvml.ERROR_TIMEOUT {
				continue
			}
			// not all return errors are handled as currently there is no proper way to process these errors other than marking all devices healthy.
			// Ref doc: [https://docs.nvidia.com/deploy/nvml-api/group__nvmlEvents.html#group__nvmlEvents_1g9714b0ca9a34c7a7780f87fee16b205c].
			if ret != nvml.SUCCESS {
				if ret == nvml.ERROR_GPU_IS_LOST {
					klog.Warningf("GPU is lost error: %v; Marking all devices as unhealthy", ret)
					m.markAllDevicesUnhealthy()
					continue
				}
				klog.V(6).Infof("Error waiting for NVML event: %v. Retrying...", ret)
				continue
			}

			// TODO: check why other supported types are not considered?
			eType := event.EventType
			xid := event.EventData
			gi := event.GpuInstanceId
			ci := event.ComputeInstanceId
			if eType != nvml.EventTypeXidCriticalError {
				klog.V(6).Infof("Skipping non-nvmlEventTypeXidCriticalError event: Data=%d, Type=%d, GI=%d, CI=%d", xid, eType, gi, ci)
				continue
			}

			if m.skippedXids[xid] {
				klog.V(6).Infof("Skipping XID event: Data=%d, Type=%d, GI=%d, CI=%d", xid, eType, gi, ci)
				continue
			}

			klog.V(4).Infof("Processing event XID=%d event", xid)
			// this seems an extreme action.
			// should we just log the error and proceed anyway.
			// TODO: look into how to properly handle this error.
			eventUUID, ret := event.Device.GetUUID()
			if ret != nvml.SUCCESS {
				klog.Warningf("Failed to determine uuid for event %v: %v; Marking all devices as unhealthy.", event, ret)
				m.markAllDevicesUnhealthy()
				continue
			}
			affectedDevice := m.deviceByPlacement.get(eventUUID, gi, ci)
			if affectedDevice == nil {
				klog.V(6).Infof("Ignoring event for unexpected device (UUID:%s, GI:%d, CI:%d)", eventUUID, gi, ci)
				continue
			}

			klog.V(4).Infof("Sending unhealthy notification for device %s due to event type:%v and event data:%d", affectedDevice.UUID(), eType, xid)
			m.unhealthy <- affectedDevice
		}
	}
}

func (m *nvmlDeviceHealthMonitor) Unhealthy() <-chan *AllocatableDevice {
	return m.unhealthy
}

func (m *nvmlDeviceHealthMonitor) markAllDevicesUnhealthy() {
	for _, giMap := range m.deviceByPlacement {
		m.markAllMigDevicesUnhealthy(giMap)
	}
}

// markAllMigDevicesUnhealthy is a helper function to mark every mig device under a parent as unhealthy.
func (m *nvmlDeviceHealthMonitor) markAllMigDevicesUnhealthy(giMap map[uint32]map[uint32]*AllocatableDevice) {
	for _, ciMap := range giMap {
		for _, dev := range ciMap {
			// Non-blocking send to avoid deadlocks if channel is full.
			select {
			case m.unhealthy <- dev:
				klog.V(6).Infof("Marked device %s as unhealthy", dev.UUID())
			// TODO: The non-blocking send protects the health-monitor goroutine from deadlocks,
			// but dropping an unhealthy notification means the device's health transition may
			// never reach the consumer. Consider follow-up improvements:
			//   - increase the channel buffer beyond len(allocatable) to reduce backpressure;
			//   - introduce a special "all devices unhealthy" message when bulk updates occur;
			//   - or revisit whether blocking briefly here is acceptable.
			default:
				klog.Errorf("Unhealthy channel full. Dropping unhealthy notification for device %s", dev.UUID())
			}
		}
	}
}

func getDevicePlacementMap(allocatable AllocatableDevices) devicePlacementMap {
	placementMap := make(devicePlacementMap)

	for _, d := range allocatable {
		var parentUUID string
		var giID, ciID uint32

		switch d.Type() {
		case GpuDeviceType, VGpuDeviceType:
			parentUUID = d.UUID()
			if parentUUID == "" {
				continue
			}
			giID = FullGPUInstanceID
			ciID = FullGPUInstanceID

		case MigDeviceType:
			parentUUID = d.Mig.Parent.UUID
			if parentUUID == "" {
				continue
			}
			giID = d.Mig.GiInfo.Id
			ciID = d.Mig.CiInfo.Id

		default:
			klog.V(6).Infof("Skipping device with unknown type: %s", d.Type())
			continue
		}
		placementMap.addDevice(parentUUID, giID, ciID, d)
	}
	return placementMap
}

func (p devicePlacementMap) addDevice(parentUUID string, giID uint32, ciID uint32, d *AllocatableDevice) {
	if _, ok := p[parentUUID]; !ok {
		p[parentUUID] = make(map[uint32]map[uint32]*AllocatableDevice)
	}
	if _, ok := p[parentUUID][giID]; !ok {
		p[parentUUID][giID] = make(map[uint32]*AllocatableDevice)
	}
	p[parentUUID][giID][ciID] = d
}

func (p devicePlacementMap) get(uuid string, gi, ci uint32) *AllocatableDevice {
	giMap, ok := p[uuid]
	if !ok {
		return nil
	}

	ciMap, ok := giMap[gi]
	if !ok {
		return nil
	}
	return ciMap[ci]
}

// getAdditionalXids returns a list of additional Xids to skip from the specified string.
// The input is treaded as a comma-separated string and all valid uint64 values are considered as Xid values.
// Invalid values nare ignored.
// TODO: add list of EXPLICIT XIDs from [https://github.com/NVIDIA/k8s-device-plugin/pull/1443].
func getAdditionalXids(input string) []uint64 {
	if input == "" {
		return nil
	}

	var additionalXids []uint64
	klog.V(6).Infof("Creating a list of additional xids to ignore: [%s]", input)
	for _, additionalXid := range strings.Split(input, ",") {
		trimmed := strings.TrimSpace(additionalXid)
		if trimmed == "" {
			continue
		}
		xid, err := strconv.ParseUint(trimmed, 10, 64)
		if err != nil {
			klog.V(6).Infof("Ignoring malformed Xid value %v: %v", trimmed, err)
			continue
		}
		additionalXids = append(additionalXids, xid)
	}

	return additionalXids
}

func xidsToSkip(additionalXids string) map[uint64]bool {
	// Add the list of hardcoded disabled (ignored) XIDs:
	// http://docs.nvidia.com/deploy/xid-errors/index.html#topic_4
	// Application errors: the GPU should still be healthy.
	ignoredXids := []uint64{
		13,  // Graphics Engine Exception
		31,  // GPU memory page fault
		43,  // GPU stopped processing
		45,  // Preemptive cleanup, due to previous errors
		68,  // Video processor exception
		109, // Context Switch Timeout Error
	}

	skippedXids := make(map[uint64]bool)
	for _, id := range ignoredXids {
		skippedXids[id] = true
	}

	for _, additionalXid := range getAdditionalXids(additionalXids) {
		skippedXids[additionalXid] = true
	}
	return skippedXids
}
