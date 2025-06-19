package manager

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"k8s.io/klog/v2"
)

const (
	// envDisableHealthChecks defines the environment variable that is checked to determine whether healthchecks
	// should be disabled. If this envvar is set to "all" or contains the string "xids", healthchecks are
	// disabled entirely. If set, the envvar is treated as a comma-separated list of Xids to ignore. Note that
	// this is in addition to the Application errors that are already ignored.
	envDisableHealthChecks = "DP_DISABLE_HEALTHCHECKS"
	allHealthChecks        = "xids"
)

// CheckHealth performs health checks on a set of devices, writing to the 'unhealthy' channel with any unhealthy devices
func (m *DeviceManager) checkHealth() error {
	disableHealthChecks := strings.ToLower(os.Getenv(envDisableHealthChecks))
	if disableHealthChecks == "all" {
		disableHealthChecks = allHealthChecks
	}
	if strings.Contains(disableHealthChecks, "xids") {
		return nil
	}

	if err := m.NvmlInit(); err != nil {
		return err
	}
	defer m.NvmlShutdown()

	// FIXME: formalize the full list and document it.
	// http://docs.nvidia.com/deploy/xid-errors/index.html#topic_4
	// Application errors: the GPU should still be healthy
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

	for _, additionalXid := range getAdditionalXids(disableHealthChecks) {
		skippedXids[additionalXid] = true
	}
	eventSet, ret := m.EventSetCreate()
	if ret != nvml.SUCCESS {
		return fmt.Errorf("failed to create event set: %v", ret)
	}
	defer func() {
		_ = eventSet.Free()
	}()

	parentToDeviceMap := make(map[string]*Device)
	deviceIDToGiMap := make(map[string]uint32)
	deviceIDToCiMap := make(map[string]uint32)

	eventMask := uint64(nvml.EventTypeXidCriticalError | nvml.EventTypeDoubleBitEccError | nvml.EventTypeSingleBitEccError)
	for _, d := range m.devices {
		var uuid string
		if d.GPU != nil {
			uuid = d.GPU.UUID
			deviceIDToGiMap[d.GPU.UUID] = 0xFFFFFFFF
			deviceIDToCiMap[d.GPU.UUID] = 0xFFFFFFFF
			parentToDeviceMap[d.GPU.UUID] = d
		} else if d.MIG != nil {
			uuid = d.MIG.Parent.UUID
			deviceIDToGiMap[d.MIG.UUID] = d.MIG.GiInfo.Id
			deviceIDToCiMap[d.MIG.UUID] = d.MIG.CiInfo.Id
			parentToDeviceMap[uuid] = d
		}

		devHandle, ret := nvml.DeviceGetHandleByUUID(uuid)
		if ret != nvml.SUCCESS {
			klog.Infof("unable to get device handle from %s: %v; marking it as unhealthy.", uuid, ret)
			m.unhealthy <- d
			continue
		}

		supportedEvents, ret := devHandle.GetSupportedEventTypes()
		if ret != nvml.SUCCESS {
			klog.Infof("Unable to determine the supported events for %v: %v; marking it as unhealthy.", uuid, ret)
			m.unhealthy <- d
			continue
		}

		ret = devHandle.RegisterEvents(eventMask&supportedEvents, eventSet)
		if ret == nvml.ERROR_NOT_SUPPORTED {
			klog.Warningf("Device %v is too old to support health checking.", uuid)
		}
		if ret != nvml.SUCCESS {
			klog.Infof("Marking device %v as unhealthy: %v", uuid, ret)
			m.unhealthy <- d
		}
	}

	stopCh := m.stop
	for {
		select {
		case <-stopCh:
			klog.V(5).Infoln("DeviceManager check health has stopped")
			return nil
		default:
		}

		e, ret := eventSet.Wait(5000)
		if ret == nvml.ERROR_TIMEOUT {
			continue
		}
		if ret != nvml.SUCCESS {
			klog.Infof("Error waiting for event: %v; Marking all devices as unhealthy", ret)
			for i := range m.devices {
				m.unhealthy <- m.devices[i]
			}
			continue
		}

		if e.EventType != nvml.EventTypeXidCriticalError {
			klog.Infof("Skipping non-nvmlEventTypeXidCriticalError event: %+v", e)
			continue
		}

		if skippedXids[e.EventData] {
			klog.Infof("Skipping event %+v", e)
			continue
		}

		klog.Infof("Processing event %+v", e)
		eventUUID, ret := e.Device.GetUUID()
		if ret != nvml.SUCCESS {
			// If we cannot reliably determine the device UUID, we mark all devices as unhealthy.
			klog.Infof("Failed to determine uuid for event %v: %v; Marking all devices as unhealthy.", e, ret)
			for i := range m.devices {
				m.unhealthy <- m.devices[i]
			}
			continue
		}

		d, exists := parentToDeviceMap[eventUUID]
		if !exists {
			klog.Infof("Ignoring event for unexpected device: %v", eventUUID)
			continue
		}

		if d.MIG != nil && e.GpuInstanceId != 0xFFFFFFFF && e.ComputeInstanceId != 0xFFFFFFFF {
			gi := deviceIDToGiMap[d.MIG.UUID]
			ci := deviceIDToCiMap[d.MIG.UUID]
			if !(gi == e.GpuInstanceId && ci == e.ComputeInstanceId) {
				continue
			}
			klog.Infof("Event for mig device %v (gi=%v, ci=%v)", d.MIG.UUID, gi, ci)
		}

		klog.Infof("XidCriticalError: Xid=%d on Device=%s; marking device as unhealthy.", e.EventData, d.MIG.UUID)

		m.unhealthy <- d
	}
}

// getAdditionalXids returns a list of additional Xids to skip from the specified string.
// The input is treaded as a comma-separated string and all valid uint64 values are considered as Xid values. Invalid values
// are ignored.
func getAdditionalXids(input string) []uint64 {
	if input == "" {
		return nil
	}

	var additionalXids []uint64
	for _, additionalXid := range strings.Split(input, ",") {
		trimmed := strings.TrimSpace(additionalXid)
		if trimmed == "" {
			continue
		}
		xid, err := strconv.ParseUint(trimmed, 10, 64)
		if err != nil {
			klog.Infof("Ignoring malformed Xid value %v: %v", trimmed, err)
			continue
		}
		additionalXids = append(additionalXids, xid)
	}

	return additionalXids
}
