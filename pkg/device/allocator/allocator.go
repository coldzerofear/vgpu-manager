package allocator

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/device/gpuallocator"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
)

type allocator struct {
	nodeInfo *device.NodeInfo
	recorder record.EventRecorder
}

func NewAllocator(nodeInfo *device.NodeInfo, recorder record.EventRecorder) *allocator {
	return &allocator{
		nodeInfo: nodeInfo,
		recorder: recorder,
	}
}

func (alloc *allocator) addAllocateOne(contDevices *device.ContainerDeviceClaim) error {
	for _, claim := range contDevices.DeviceClaims {
		if err := alloc.nodeInfo.AddUsedResources(claim, true); err != nil {
			return err
		}
	}
	return nil
}

// Allocate tries to find a suitable GPU device for containers
// and records some data in pod's annotation
func (alloc *allocator) Allocate(pod *corev1.Pod) (*corev1.Pod, error) {
	klog.V(4).Infof("Attempt to allocate pod <%s> on node <%s>", klog.KObj(pod), alloc.nodeInfo.GetName())
	newPod := pod.DeepCopy()
	// One profile per pod: weights are derived purely from the pod's own
	// container requests (see NewRequestProfile for why no node info
	// reaches this call). Filter and allocator compute identical weights
	// for the same pod, so cross-node ranking and per-node device-policy
	// tie-breaking stay coherent without threading any node-specific
	// normalisation through.
	profile := NewRequestProfile(pod)
	var podAssignDevices device.PodDeviceClaim
	for i := range newPod.Spec.Containers {
		container := &pod.Spec.Containers[i]
		// Skip containers that do not request vGPU.
		if !util.IsVGPURequiredContainer(container) {
			continue
		}
		assignDevices, err := alloc.allocateOne(newPod, container, profile)
		if err != nil {
			klog.V(4).InfoS(err.Error(), "node", alloc.nodeInfo.GetName(),
				"pod", klog.KObj(pod), "container", container.Name)
			return nil, err
		}
		if err = alloc.addAllocateOne(assignDevices); err != nil {
			klog.V(3).ErrorS(err, "Failed to add assigned resources", "node",
				alloc.nodeInfo.GetName(), "pod", klog.KObj(pod), "container", container.Name)
			return nil, fmt.Errorf("internal device scheduling error")
		}
		podAssignDevices = append(podAssignDevices, *assignDevices)
	}
	preAlloc, err := podAssignDevices.MarshalText()
	if err != nil {
		klog.V(2).ErrorS(err, "assign devices encoding failed",
			"node", alloc.nodeInfo.GetName(), "pod", klog.KObj(pod))
		return nil, fmt.Errorf("assign devices encoding failed")
	}
	util.InsertAnnotation(newPod, util.PodVGPUPreAllocAnnotation, preAlloc)
	util.InsertAnnotation(newPod, util.PodPredicateNodeAnnotation, alloc.nodeInfo.GetName())
	return newPod, nil
}

func getDeviceUUIDs(devices []*device.Device) []string {
	uuids := make([]string, len(devices))
	for i, d := range devices {
		uuids[i] = d.GetUUID()
	}
	return uuids
}

func (alloc *allocator) allocateOne(pod *corev1.Pod, container *corev1.Container, profile RequestProfile) (*device.ContainerDeviceClaim, error) {
	klog.V(4).Infof("Attempt to allocate container <%s> on node <%s>", container.Name, alloc.nodeInfo.GetName())
	node := alloc.nodeInfo.GetNode()
	needNumber := int(util.GetResourceOfContainer(container, util.VGPUNumberResourceName))
	needCores := util.GetResourceOfContainer(container, util.VGPUCoreResourceName)
	needMemory := util.GetResourceOfContainer(container, util.VGPUMemoryResourceName)
	if needNumber > alloc.nodeInfo.GetDeviceCount() {
		return nil, errors.New("insufficient GPU cards")
	}
	// Calculate the actual requested memory size based on the node memory factor.
	if needMemory > 0 {
		if memoryFactor := alloc.nodeInfo.NodeConfigInfo.MemoryFactor; memoryFactor > 0 {
			needMemory *= int64(memoryFactor)
		}
	}
	if needCores == 0 && needMemory == 0 {
		needCores = util.HundredCore
	}
	var (
		deviceClaims []device.DeviceClaim
		topoErr      error
		devicePolicy string
	)
	deviceStore, reasonStore := alloc.filterDevices(pod, needCores, needMemory)
	if needNumber > len(deviceStore) {
		goto DONE
	} else if needNumber == len(deviceStore) {
		// When the request happens to consume every filterable device on this
		// node there is no per-device choice to make. The fast path skips the
		// policy sort and topology dispatch, but ONLY when no strict-topology
		// constraint is in effect — a numa-strict (or link-strict) pod still
		// needs us to verify the forced "take all" set actually satisfies the
		// constraint, otherwise A1's no-fallback semantics break silently
		// (e.g. 4 GPUs across two NUMA nodes would falsely succeed for
		// numa-strict). Non-strict requests keep the fast path because their
		// result would be identical either way.
		if _, strict := parseTopologyMode(pod); !strict || needNumber <= 1 {
			deviceClaims = allocateByNumbers(deviceStore, needNumber, needCores, needMemory)
			goto DONE
		}
		// Fall through to the policy switch so allocateByTopologyMode can run
		// the topology validation and surface TopologyUnsatisfiedError on
		// violation. The downstream sort is harmless: with needNumber == len
		// there is only one possible set, so order does not change the result.
	}
	// Sort the devices according to the device scheduling strategy.
	devicePolicy, _ = util.HasAnnotation(pod, util.DeviceSchedulerPolicyAnnotation)
	switch policy := strings.ToLower(devicePolicy); policy {
	case string(util.BinpackPolicy):
		klog.V(4).Infof("Pod <%s/%s> use <%s> node scheduling policy", pod.Namespace, pod.Name, policy)
		NewDeviceBinpackPriority().Sort(deviceStore)
		deviceClaims, topoErr = alloc.allocateByTopologyMode(pod, deviceStore, profile, util.BinpackPolicy, needNumber, needCores, needMemory)
	case string(util.SpreadPolicy):
		klog.V(4).Infof("Pod <%s/%s> use <%s> node scheduling policy", pod.Namespace, pod.Name, policy)
		NewDeviceSpreadPriority().Sort(deviceStore)
		deviceClaims, topoErr = alloc.allocateByTopologyMode(pod, deviceStore, profile, util.SpreadPolicy, needNumber, needCores, needMemory)
	default:
		if policy == "" || policy == string(util.NonePolicy) {
			klog.V(4).Infof("Pod <%s/%s> none device scheduling policy", pod.Namespace, pod.Name)
		} else {
			klog.V(4).Infof("Pod <%s/%s> not supported device scheduling policy: %s", pod.Namespace, pod.Name, devicePolicy)
			alloc.sendEventf(pod, corev1.EventTypeWarning, "DevicePolicy",
				"Unsupported device scheduling policy '%s'", devicePolicy)
		}
		NewSortPriority(ByNuma, ByDeviceIdAsc).Sort(deviceStore)
		deviceClaims, topoErr = alloc.allocateByTopologyMode(pod, deviceStore, profile, util.NonePolicy, needNumber, needCores, needMemory)
	}
	// Strict topology rejection propagates as-is so the Filter loop can
	// drop just this node (instead of treating it as a generic resource
	// failure that would burn the entire scheduling cycle).
	if topoErr != nil {
		return nil, topoErr
	}
DONE:
	if len(deviceClaims) != needNumber {
		klog.V(5).InfoS("Insufficient node resources", "node", node.GetName(),
			"pod", klog.KObj(pod), "container", container.Name, "reason", reasonStore)
		return nil, errors.New("insufficient GPU resources")
	}
	containerClaim := &device.ContainerDeviceClaim{
		Name:         container.Name,
		DeviceClaims: deviceClaims,
	}
	sort.Slice(containerClaim.DeviceClaims, func(i, j int) bool {
		return containerClaim.DeviceClaims[i].Id < containerClaim.DeviceClaims[j].Id
	})
	return containerClaim, nil
}

func (alloc *allocator) sendEventf(object runtime.Object, eventtype, reason, messageFmt string, args ...interface{}) {
	if alloc.recorder != nil {
		alloc.recorder.Eventf(object, eventtype, reason, messageFmt, args...)
	}
}

// TopologyUnsatisfiedError signals that a topology-strict pod request could
// not be satisfied on the current node and the node must therefore be
// rejected (no silent fallback). The Filter loop treats this as a per-node
// rejection — other candidate nodes will still be tried; only when ALL
// strict-eligible nodes return this error does the pod stay Pending.
type TopologyUnsatisfiedError struct {
	Mode util.TopologyMode
	Node string
	// Reason is a short human-readable phrase used in events and logs,
	// e.g. "no NUMA node has 4 GPUs" or "no link-topology satisfying set".
	Reason string
}

func (e *TopologyUnsatisfiedError) Error() string {
	return fmt.Sprintf("%s topology unsatisfiable on node %s: %s", e.Mode, e.Node, e.Reason)
}

// IsTopologyUnsatisfied reports whether err is a TopologyUnsatisfiedError.
// Callers (e.g. Filter) use this to distinguish per-node strict-topology
// rejection from generic "out of resources" failures.
func IsTopologyUnsatisfied(err error) bool {
	_, ok := err.(*TopologyUnsatisfiedError)
	return ok
}

// parseTopologyMode reads DeviceTopologyModeAnnotation and returns the base
// mode (numa / link / none) together with the strict flag derived from the
// "-strict" suffix variants.
func parseTopologyMode(pod *corev1.Pod) (mode util.TopologyMode, strict bool) {
	raw, _ := util.HasAnnotation(pod, util.DeviceTopologyModeAnnotation)
	tm := util.TopologyMode(strings.ToLower(raw))
	return tm.BaseTopology(), tm.IsStrictTopology()
}

// allocateByTopologyMode dispatches to topology-aware allocation. Strict
// failures return a *TopologyUnsatisfiedError; best-effort failures fall
// back to allocateByNumbers (and emit an event so operators can see the
// downgrade in `kubectl describe pod`).
func (alloc *allocator) allocateByTopologyMode(pod *corev1.Pod, deviceStore []*device.Device,
	profile RequestProfile, policy util.SchedulerPolicy, needNumber int, needCores, needMemory int64,
) ([]device.DeviceClaim, error) {
	if needNumber <= 1 {
		return allocateByNumbers(deviceStore, needNumber, needCores, needMemory), nil
	}
	mode, strict := parseTopologyMode(pod)

	switch mode {
	case util.LinkTopology:
		klog.V(4).Infof("Pod <%s/%s> use Links topology mode (strict=%v)",
			pod.Namespace, pod.Name, strict)
		if claims, ok := alloc.allocateLink(deviceStore, profile, policy, strict, needNumber, needCores, needMemory); ok {
			return claims, nil
		}
		reason := alloc.linkFallbackReason(needNumber)
		if strict {
			return nil, &TopologyUnsatisfiedError{
				Mode: util.LinkTopologyStrict, Node: alloc.nodeInfo.GetName(), Reason: reason,
			}
		}
		alloc.sendEventf(pod, corev1.EventTypeWarning, "TopologyFallback",
			"link topology unsatisfiable on %s (%s); falling back to non-topology allocation",
			alloc.nodeInfo.GetName(), reason)

	case util.NUMATopology:
		klog.V(4).Infof("Pod <%s/%s> use NUMA topology mode (strict=%v)",
			pod.Namespace, pod.Name, strict)
		if claims, ok := alloc.allocateNUMA(deviceStore, profile, policy, needNumber, needCores, needMemory); ok {
			return claims, nil
		}
		reason := alloc.numaFallbackReason(needNumber, deviceStore)
		if strict {
			return nil, &TopologyUnsatisfiedError{
				Mode: util.NUMATopologyStrict, Node: alloc.nodeInfo.GetName(), Reason: reason,
			}
		}
		alloc.sendEventf(pod, corev1.EventTypeWarning, "TopologyFallback",
			"NUMA topology unsatisfiable on %s (%s); falling back to cross-NUMA allocation",
			alloc.nodeInfo.GetName(), reason)

	case util.NoneTopology, "":
		klog.V(4).Infof("Pod <%s/%s> none topology mode", pod.Namespace, pod.Name)

	default:
		klog.V(4).Infof("Pod <%s/%s> not supported topology mode: %s",
			pod.Namespace, pod.Name, mode)
		alloc.sendEventf(pod, corev1.EventTypeWarning, "DeviceTopologyMode",
			"Unsupported device topology mode '%s'", mode)
	}
	return allocateByNumbers(deviceStore, needNumber, needCores, needMemory), nil
}

// allocateLink runs the NVIDIA bestEffort algorithm over the link-aware
// device list. Returns (claims, true) on success; (nil, false) means the
// caller should either fall back (non-strict) or surface a
// TopologyUnsatisfiedError (strict).
// linkTopKCandidates controls how many topology-equivalent candidate sets we
// keep when the caller has a non-None device policy. Empirically 5 covers
// most "two NVLink bridges, three NUMA branches" diversity without blowing
// up the partition enumeration cost. Increase only if you observe binpack/
// spread picking the link-best set even when other equally-good sets would
// satisfy the policy better.
const linkTopKCandidates = 5

func (alloc *allocator) allocateLink(deviceStore []*device.Device,
	profile RequestProfile, policy util.SchedulerPolicy, strict bool, needNumber int, needCores, needMemory int64,
) ([]device.DeviceClaim, bool) {
	if !alloc.nodeInfo.HasGPUTopology() {
		return nil, false
	}
	devices, _ := alloc.nodeInfo.GetDeviceList().Filter(getDeviceUUIDs(deviceStore))

	// Fast path: no device policy → take the link-best set (cheapest path,
	// matches pre-Phase-A behaviour). Uses the threshold-aware AllocateLink
	// which transparently falls back to greedy on dense nodes.
	if policy == util.NonePolicy || policy == "" {
		got := gpuallocator.AllocateLink(devices, needNumber)
		if len(got) != needNumber {
			return nil, false
		}
		// bestEffort returns the highest-scoring partition but does NOT reject
		// score-zero results, so on nodes with partial NVLink (some pairs
		// connected, others not) we can get back a set whose chosen GPUs sit
		// in disjoint connectivity islands. For strict-link the caller's
		// contract is "fail rather than admit a disconnected set"; verify via
		// the precomputed per-UUID component map.
		if strict && !alloc.nodeInfo.AreDevicesLinked(gpuallocatorUUIDs(got)) {
			return nil, false
		}
		return allocateByDevices(deviceStore, got, needCores, needMemory), true
	}

	// Compose path: keep top-K topology-equivalent candidates, then apply
	// binpack/spread to choose among them. This is what makes "link + binpack"
	// actually compose instead of the binpack sort being silently ignored.
	candidates := gpuallocator.AllocateLinkTopK(devices, needNumber, linkTopKCandidates)
	if len(candidates) == 0 {
		return nil, false
	}
	if strict {
		// Drop disconnected candidates BEFORE the binpack/spread tie-break so
		// a perfectly utilised-but-disconnected set never beats a worse-
		// utilised-but-connected one. If every top-K candidate is
		// disconnected, treat the node as strict-unsatisfiable.
		filtered := candidates[:0]
		for _, c := range candidates {
			if alloc.nodeInfo.AreDevicesLinked(gpuallocatorUUIDs(c)) {
				filtered = append(filtered, c)
			}
		}
		if len(filtered) == 0 {
			return nil, false
		}
		candidates = filtered
	}
	chosen := selectLinkCandidateByDevicePolicy(candidates, deviceStore, profile, policy)
	if len(chosen) != needNumber {
		return nil, false
	}
	return allocateByDevices(deviceStore, chosen, needCores, needMemory), true
}

// gpuallocatorUUIDs extracts UUIDs from a gpuallocator.Device slice. The
// existing getDeviceUUIDs takes []*device.Device, so this is a sibling
// helper for the post-AllocateLink validation path.
func gpuallocatorUUIDs(devices []*gpuallocator.Device) []string {
	uuids := make([]string, len(devices))
	for i, d := range devices {
		uuids[i] = d.UUID
	}
	return uuids
}

// selectLinkCandidateByDevicePolicy picks among link-equivalent candidate
// sets using the device-level binpack/spread policy. Candidates arrive
// already sorted by link score (highest first); the secondary key is the
// average per-device Score under the pod's RequestProfile + policy mode,
// which encodes the binpack-vs-spread direction directly (higher score is
// always the more-preferred candidate, regardless of mode). This replaces
// the legacy dimension-averaged deviceUsedRatio + per-mode sort comparator
// pair — same intent, request-aware, and one fewer place to read the
// policy direction off of.
//
// Note that link score still dominates: a strictly-worse link score will
// not be picked unless the better candidate falls outside the top-K window.
func selectLinkCandidateByDevicePolicy(
	candidates [][]*gpuallocator.Device,
	deviceStore []*device.Device,
	profile RequestProfile,
	policy util.SchedulerPolicy,
) []*gpuallocator.Device {
	if len(candidates) <= 1 {
		return candidates[0]
	}
	// NonePolicy callers don't reach this function (the no-policy branch of
	// allocateLink takes the fast path), so policy is always Binpack or
	// Spread here and Score returns a meaningful directional value.
	byUUID := make(map[string]*device.Device, len(deviceStore))
	for _, d := range deviceStore {
		byUUID[d.GetUUID()] = d
	}
	type scored struct {
		idx   int
		score float64
	}
	scoredCands := make([]scored, len(candidates))
	for i, set := range candidates {
		scoredCands[i] = scored{i, candidateSetScore(set, byUUID, profile, policy)}
	}
	sort.SliceStable(scoredCands, func(i, j int) bool {
		return scoredCands[i].score > scoredCands[j].score
	})
	return candidates[scoredCands[0].idx]
}

// candidateSetScore returns the average per-device Score across a candidate
// set under the given profile + policy mode. Devices not found in byUUID
// (shouldn't happen — candidates are built from deviceStore) are skipped
// rather than scored as zero, so a stale candidate can't artificially
// depress an otherwise-good set's average.
func candidateSetScore(set []*gpuallocator.Device, byUUID map[string]*device.Device,
	profile RequestProfile, policy util.SchedulerPolicy,
) float64 {
	if len(set) == 0 {
		return 0
	}
	sum := 0.0
	count := 0
	for _, d := range set {
		dev, ok := byUUID[d.UUID]
		if !ok {
			continue
		}
		sum += Score(DeviceUtilization(dev), profile, policy)
		count++
	}
	if count == 0 {
		return 0
	}
	return sum / float64(count)
}

// allocateNUMA attempts to satisfy the request within a single NUMA node,
// applying the binpack/spread policy to choose which NUMA node to consume.
// Returns (claims, false) when no NUMA node alone can hold needNumber devices.
func (alloc *allocator) allocateNUMA(deviceStore []*device.Device,
	profile RequestProfile, policy util.SchedulerPolicy, needNumber int, needCores, needMemory int64,
) ([]device.DeviceClaim, bool) {
	if !alloc.nodeInfo.HasNUMATopology() {
		return nil, false
	}
	numaNode, ok := CanNotCrossNumaNode(needNumber, deviceStore)
	if !ok {
		return nil, false
	}
	var claims []device.DeviceClaim
	numaNode.SchedulerPolicyCallback(profile, policy, func(_ int, devices []*device.Device) bool {
		if needNumber > len(devices) {
			return false
		}
		claims = allocateByNumbers(devices, needNumber, needCores, needMemory)
		return true
	})
	if len(claims) != needNumber {
		return nil, false
	}
	return claims, true
}

func (alloc *allocator) linkFallbackReason(needNumber int) string {
	if !alloc.nodeInfo.HasGPUTopology() {
		return "node has no GPU link topology"
	}
	// HasGPUTopology was true → fall-through cause is connectivity: bestEffort
	// returned a candidate set but no candidate had all N GPUs in a single
	// NVLink component. Report the largest component so operators can see how
	// far short the node fell.
	return fmt.Sprintf("no NVLink-connected set of %d GPUs (largest component %d)",
		needNumber, alloc.nodeInfo.MaxLinkComponentSize())
}

func (alloc *allocator) numaFallbackReason(needNumber int, deviceStore []*device.Device) string {
	if !alloc.nodeInfo.HasNUMATopology() {
		return "node has no NUMA topology"
	}
	max := 0
	for _, devs := range NewNumaNodeDevice(deviceStore) {
		if len(devs) > max {
			max = len(devs)
		}
	}
	return fmt.Sprintf("no NUMA node has %d GPUs (max single-NUMA capacity %d)", needNumber, max)
}

func allocateByDevices(deviceStore []*device.Device, devices []*gpuallocator.Device, needCores, needMemory int64) []device.DeviceClaim {
	claimDevices := make([]device.DeviceClaim, len(devices))
	for i, dev := range devices {
		reqMemory := needMemory
		// When there is no defined request for memory,
		// it occupies the entire card memory.
		if reqMemory == 0 {
			index := slices.IndexFunc(deviceStore, func(d *device.Device) bool {
				return d.GetUUID() == dev.UUID
			})
			reqMemory = deviceStore[index].GetTotalMemory()
		}
		claimDevices[i] = device.DeviceClaim{
			Id:     dev.Index,
			Uuid:   dev.UUID,
			Cores:  needCores,
			Memory: needMemory,
		}
	}
	return claimDevices
}

func allocateByNumbers(deviceStore []*device.Device, needNumber int, needCores, needMemory int64) []device.DeviceClaim {
	claims := make([]device.DeviceClaim, needNumber)
	for i, deviceInfo := range deviceStore[0:needNumber] {
		reqMemory := needMemory
		// When there is no defined request for memory,
		// it occupies the entire card memory.
		if reqMemory == 0 {
			reqMemory = deviceInfo.GetTotalMemory()
		}
		claims[i] = device.DeviceClaim{
			Id:     deviceInfo.GetID(),
			Uuid:   deviceInfo.GetUUID(),
			Cores:  needCores,
			Memory: reqMemory,
		}
	}
	return claims
}

type FailedReason string

const (
	DeviceUnhealthy    FailedReason = "DeviceUnhealthy"
	DeviceEnableMig    FailedReason = "DeviceEnableMig"
	InsufficientNumber FailedReason = "InsufficientNumber"
	InsufficientMemory FailedReason = "InsufficientMemory"
	InsufficientSMCore FailedReason = "InsufficientSMCore"
	DeviceTypeMismatch FailedReason = "DeviceTypeMismatch"
	DeviceUuidMismatch FailedReason = "DeviceUuidMismatch"
)

func (alloc *allocator) filterDevices(pod *corev1.Pod, needCores, needMemory int64) ([]*device.Device, map[FailedReason]int) {
	nodeName := alloc.nodeInfo.GetName()
	reasonMap := make(map[FailedReason]int)
	devices := make([]*device.Device, 0, alloc.nodeInfo.GetDeviceCount())
	for i, deviceInfo := range alloc.nodeInfo.GetDeviceMap() {
		// Filter unhealthy device.
		if !deviceInfo.Healthy() {
			klog.V(4).InfoS("Filter unhealthy devices on the node", "node", nodeName,
				"deviceIndex", i, "deviceUuid", deviceInfo.GetUUID())
			reasonMap[DeviceUnhealthy]++
			continue
		}
		// Filter MIG enabled device.
		if deviceInfo.IsMIG() {
			klog.V(4).InfoS("Filter devices with MIG enabled on the node", "node",
				nodeName, "deviceIndex", i, "deviceUuid", deviceInfo.GetUUID())
			reasonMap[DeviceEnableMig]++
			continue
		}
		// Filter for insufficient number of virtual devices.
		if deviceInfo.AllocatableNumber() == 0 {
			klog.V(4).InfoS("Filter devices with insufficient available number on the node",
				"node", nodeName, "deviceIndex", i, "deviceUuid", deviceInfo.GetUUID())
			reasonMap[InsufficientNumber]++
			continue
		}
		reqMemory := needMemory
		// When there is no defined request for memory,
		// it occupies the entire card memory.
		if reqMemory == 0 {
			reqMemory = deviceInfo.GetTotalMemory()
		}
		if reqMemory > deviceInfo.AllocatableMemory() {
			klog.V(4).InfoS("Filter devices with insufficient available memory on the node",
				"node", nodeName, "deviceIndex", i, "deviceUuid", deviceInfo.GetUUID(),
				"availableMemory", deviceInfo.AllocatableMemory(), "requestedMemory", reqMemory)
			reasonMap[InsufficientMemory]++
			continue
		}
		if needCores > deviceInfo.AllocatableCores() || deviceInfo.AllocatableCores() == 0 {
			klog.V(4).InfoS("Filter devices with insufficient available cores on the node",
				"node", nodeName, "deviceIndex", i, "deviceUuid", deviceInfo.GetUUID(),
				"availableCores", deviceInfo.AllocatableCores(), "requestedCores", needCores)
			reasonMap[InsufficientSMCore]++
			continue
		}
		// Filter device type.
		if !util.CheckDeviceType(pod.Annotations, deviceInfo.GetType()) {
			klog.V(4).InfoS("Filter devices with type mismatches on the node",
				"node", nodeName, "deviceIndex", i, "deviceType", deviceInfo.GetType(),
				"includeTypes", pod.Annotations[util.PodIncludeGpuTypeAnnotation],
				"excludeTypes", pod.Annotations[util.PodExcludeGpuTypeAnnotation])
			reasonMap[DeviceTypeMismatch]++
			continue
		}
		// Filter device uuid.
		if !util.CheckDeviceUuid(pod.Annotations, deviceInfo.GetUUID()) {
			klog.V(4).InfoS("Filter devices with uuid mismatches on the node",
				"node", nodeName, "deviceIndex", i, "deviceUuid", deviceInfo.GetUUID(),
				"includeUuids", pod.Annotations[util.PodIncludeGPUUUIDAnnotation],
				"excludeUuids", pod.Annotations[util.PodExcludeGPUUUIDAnnotation])
			reasonMap[DeviceUuidMismatch]++
			continue
		}
		devices = append(devices, deviceInfo)
	}
	return devices, reasonMap
}
