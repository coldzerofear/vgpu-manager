package manager

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"k8s.io/klog/v2"
)

// fullGPUInstanceID is the GI/CI value NVML reports for events that concern a
// whole GPU rather than a specific MIG instance.
const fullGPUInstanceID uint32 = 0xFFFFFFFF

// eventErrorBackoff throttles the event loop after a non-timeout NVML error.
// eventSet.Wait() returns immediately on error, so a persistent failure mode
// (a lost GPU, or NVML left uninitialized after a driver reload) would
// otherwise spin this goroutine at full CPU.
const eventErrorBackoff = time.Second

// devicePlacementMap indexes devices by their placement 3-tuple
// <parent UUID, GI, CI>: a full GPU is stored under
// (its own UUID, fullGPUInstanceID, fullGPUInstanceID), a MIG device under
// (parent UUID, its GI, its CI).
//
// A flat parentUUID -> device map cannot be used here: m.devices holds both the
// full GPU and every MIG device carved out of it, and all of them share one
// parent UUID, so they would overwrite each other and all but the last entry
// would silently stop receiving health events.
type devicePlacementMap map[string]map[uint32]map[uint32]*Device

func (p devicePlacementMap) add(parentUUID string, gi, ci uint32, d *Device) {
	if _, ok := p[parentUUID]; !ok {
		p[parentUUID] = make(map[uint32]map[uint32]*Device)
	}
	if _, ok := p[parentUUID][gi]; !ok {
		p[parentUUID][gi] = make(map[uint32]*Device)
	}
	p[parentUUID][gi][ci] = d
}

// get resolves an event to a device. It first looks for an exact placement
// match, then falls back to the parent GPU entry: an event carrying a GI/CI we
// do not track (a MIG instance absent from our list, e.g. under
// MigStrategy=none) still concerns hardware we do advertise, so attributing it
// to the parent GPU is better than dropping it.
func (p devicePlacementMap) get(parentUUID string, gi, ci uint32) *Device {
	giMap, ok := p[parentUUID]
	if !ok {
		return nil
	}
	if d, ok := giMap[gi][ci]; ok {
		return d
	}
	return giMap[fullGPUInstanceID][fullGPUInstanceID]
}

// devices flattens every device registered under a parent GPU.
func (p devicePlacementMap) devices(parentUUID string) []*Device {
	var devices []*Device
	for _, ciMap := range p[parentUUID] {
		for _, d := range ciMap {
			devices = append(devices, d)
		}
	}
	return devices
}

const (
	// envDisableHealthChecks defines the environment variable that is checked to determine whether healthchecks
	// should be disabled. If this envvar is set to "all" or contains the string "xids", healthchecks are
	// disabled entirely. If set, the envvar is treated as a comma-separated list of Xids to ignore. Note that
	// this is in addition to the Application errors that are already ignored.
	envDisableHealthChecks = "DP_DISABLE_HEALTHCHECKS"
	// envEnableHealthChecks defines the environment variable that is checked to
	// determine which XIDs should be explicitly enabled. XIDs specified here
	// override the ones specified in the `DP_DISABLE_HEALTHCHECKS`.
	// Note that this also allows individual XIDs to be selected when ALL XIDs
	// are disabled.
	envEnableHealthChecks = "DP_ENABLE_HEALTHCHECKS"
)

// CheckHealth performs health checks on a set of devices, writing to the 'unhealthy' channel with any unhealthy devices
func (m *DeviceManager) checkHealth() error {
	xids := getDisabledHealthCheckXids()
	if xids.IsAllDisabled() {
		return nil
	}

	if err := m.NvmlInit(); err != nil {
		return err
	}
	defer m.NvmlShutdown()
	stopCh := m.stop

	klog.Infof("Ignoring the following XIDs for health checks: %v", xids)

	eventSet, ret := m.EventSetCreate()
	if ret != nvml.SUCCESS {
		return fmt.Errorf("failed to create event set: %v", ret)
	}
	defer func() {
		_ = eventSet.Free()
	}()

	placements := make(devicePlacementMap)
	for _, d := range m.devices {
		switch {
		case d.GPU != nil:
			placements.add(d.GPU.UUID, fullGPUInstanceID, fullGPUInstanceID, d)
		case d.MIG != nil:
			placements.add(d.MIG.Parent.UUID, d.MIG.GiInfo.Id, d.MIG.CiInfo.Id, d)
		default:
			klog.Warningf("Skipping device with neither GPU nor MIG info set")
		}
	}

	// Events are registered once per physical GPU. Registering per device entry
	// would repeat the same call for every MIG instance of a GPU, and a failure
	// has to disable monitoring for the parent and all of its MIG children
	// alike -- hence the iteration over parents rather than over m.devices.
	eventMask := uint64(nvml.EventTypeXidCriticalError | nvml.EventTypeDoubleBitEccError | nvml.EventTypeSingleBitEccError)
	for parentUUID := range placements {
		devHandle, ret := nvml.DeviceGetHandleByUUID(parentUUID)
		if ret != nvml.SUCCESS {
			klog.Infof("unable to get device handle from %s: %v; marking it as unhealthy.", parentUUID, ret)
			m.markUnhealthy(placements.devices(parentUUID)...)
			continue
		}

		supportedEvents, ret := devHandle.GetSupportedEventTypes()
		if ret != nvml.SUCCESS {
			klog.Infof("Unable to determine the supported events for %v: %v; marking it as unhealthy.", parentUUID, ret)
			m.markUnhealthy(placements.devices(parentUUID)...)
			continue
		}

		// ERROR_NOT_SUPPORTED means the GPU is too old to report events at all.
		// That is not a fault: the device stays healthy, it just goes
		// unmonitored. It must therefore not fall through to the generic
		// failure branch below (which is what a plain `if ret != SUCCESS` would
		// do, since NOT_SUPPORTED is also != SUCCESS).
		ret = devHandle.RegisterEvents(eventMask&supportedEvents, eventSet)
		switch {
		case ret == nvml.ERROR_NOT_SUPPORTED:
			klog.Warningf("Device %v is too old to support health checking.", parentUUID)
		case ret != nvml.SUCCESS:
			klog.Infof("Marking device %v as unhealthy: %v", parentUUID, ret)
			m.markUnhealthy(placements.devices(parentUUID)...)
		}
	}

	for {
		select {
		case <-stopCh:
			klog.V(3).Infoln("DeviceManager check health has stopped")
			return nil
		default:
		}

		e, ret := eventSet.Wait(5000)
		if ret == nvml.ERROR_TIMEOUT {
			continue
		}
		if ret != nvml.SUCCESS {
			if ret == nvml.ERROR_GPU_IS_LOST {
				klog.Warningf("GPU is lost error: %v; Marking all devices as unhealthy", ret)
				m.markUnhealthy(m.devices...)
			} else {
				klog.V(6).Infof("Error waiting for NVML event: %v. Retrying...", ret)
			}
			time.Sleep(eventErrorBackoff)
			continue
		}

		if e.EventType != nvml.EventTypeXidCriticalError {
			klog.Infof("Skipping non-nvmlEventTypeXidCriticalError event: %+v", e)
			continue
		}

		if xids.IsDisabled(e.EventData) {
			klog.Infof("Skipping event %+v", e)
			continue
		}

		klog.Infof("Processing event %+v", e)
		eventUUID, ret := e.Device.GetUUID()
		if ret != nvml.SUCCESS {
			// If we cannot reliably determine the device UUID, we mark all devices as unhealthy.
			klog.Infof("Failed to determine uuid for event %v: %v; Marking all devices as unhealthy.", e, ret)
			m.markUnhealthy(m.devices...)
			continue
		}

		// The event carries the parent GPU's UUID plus the GI/CI of the MIG
		// instance it originated from (both fullGPUInstanceID for a GPU-wide
		// error), which is exactly the placement 3-tuple the map is keyed on.
		d := placements.get(eventUUID, e.GpuInstanceId, e.ComputeInstanceId)
		if d == nil {
			klog.Infof("Ignoring event for unexpected device: %v (gi=%v, ci=%v)",
				eventUUID, e.GpuInstanceId, e.ComputeInstanceId)
			continue
		}
		if d.MIG != nil {
			klog.Infof("Event for mig device %v (gi=%v, ci=%v)", d.MIG.UUID, e.GpuInstanceId, e.ComputeInstanceId)
		}

		klog.Infof("XidCriticalError: Xid=%d on Device=%s; marking device as unhealthy.", e.EventData, d.getUuid())

		m.markUnhealthy(d)
	}
}

// markUnhealthy hands devices to the notification loop, which flips their
// health flag and republishes the device list.
func (m *DeviceManager) markUnhealthy(devices ...*Device) {
	for _, d := range devices {
		m.unhealthy <- d
	}
}

const allXIDs = 0

// disabledXIDs stores a map of explicitly disabled XIDs.
// The special XID `allXIDs` indicates that all XIDs are disabled, but does
// allow for specific XIDs to be enabled even if this is the case.
type disabledXIDs map[uint64]bool

// Disabled returns whether XID-based health checks are disabled.
// These are considered if all XIDs have been disabled AND no other XIDs have
// been explcitly enabled.
func (h disabledXIDs) IsAllDisabled() bool {
	if allDisabled, ok := h[allXIDs]; ok {
		return allDisabled
	}
	// At this point we wither have explicitly disabled XIDs or explicitly
	// enabled XIDs. Since ANY XID that's not specified is assumed enabled, we
	// return here.
	return false
}

// IsDisabled checks whether the specified XID has been explicitly disalbled.
// An XID is considered disabled if it has been explicitly disabled, or all XIDs
// have been disabled.
func (h disabledXIDs) IsDisabled(xid uint64) bool {
	// Handle the case where enabled=all.
	if explicitAll, ok := h[allXIDs]; ok && !explicitAll {
		return false
	}
	// Handle the case where the XID has been specifically enabled (or disabled)
	if disabled, ok := h[xid]; ok {
		return disabled
	}
	return h.IsAllDisabled()
}

// getDisabledHealthCheckXids returns the XIDs that should be ignored.
// Here we combine the following (in order of precedence):
// * A list of explicitly disabled XIDs (including all XIDs)
// * A list of hardcoded disabled XIDs
// * A list of explicitly enabled XIDs (including all XIDs)
//
// Note that if an XID is explicitly enabled, this takes precedence over it
// having been disabled either explicitly or implicitly.
func getDisabledHealthCheckXids() disabledXIDs {
	disabled := newHealthCheckXIDs(
		// TODO: We should not read the envvar here directly, but instead
		// "upgrade" this to a top-level config option.
		strings.Split(strings.ToLower(os.Getenv(envDisableHealthChecks)), ",")...,
	)
	enabled := newHealthCheckXIDs(
		// TODO: We should not read the envvar here directly, but instead
		// "upgrade" this to a top-level config option.
		strings.Split(strings.ToLower(os.Getenv(envEnableHealthChecks)), ",")...,
	)

	// Add the list of hardcoded disabled (ignored) XIDs:
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
	for _, ignored := range ignoredXids {
		disabled[ignored] = true
	}

	// Explicitly ENABLE specific XIDs,
	for enabled := range enabled {
		disabled[enabled] = false
	}
	return disabled
}

// newHealthCheckXIDs converts a list of Xids to a healthCheckXIDs map.
// Special xid values 'all' and 'xids' return a special map that matches all
// xids.
// For other xids, these are converted to a uint64 values with invalid values
// being ignored.
func newHealthCheckXIDs(xids ...string) disabledXIDs {
	output := make(disabledXIDs)
	for _, xid := range xids {
		trimmed := strings.TrimSpace(xid)
		if trimmed == "all" || trimmed == "xids" {
			// TODO: We should have a different type for "all" and "all-except"
			return disabledXIDs{allXIDs: true}
		}
		if trimmed == "" {
			continue
		}
		id, err := strconv.ParseUint(trimmed, 10, 64)
		if err != nil {
			klog.Infof("Ignoring malformed Xid value %v: %v", trimmed, err)
			continue
		}

		output[id] = true
	}
	return output
}
