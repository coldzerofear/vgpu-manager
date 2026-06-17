package allocator

import (
	"errors"
	"fmt"
	"sort"

	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/device/gpuallocator"
	"github.com/coldzerofear/vgpu-manager/pkg/scheduler/reason"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
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

func (alloc *allocator) addContainerAllocate(contDevices *device.ContainerDeviceClaim) error {
	for _, claim := range contDevices.DeviceClaims {
		if err := alloc.nodeInfo.AddUsedResources(claim); err != nil {
			return err
		}
	}
	return nil
}

// Allocate runs the per-container allocation loop and writes the result
// onto the returned pod's annotations. The request is pre-parsed (see
// BuildAllocationRequest) so this function — and everything it calls —
// reads scheduling annotations off req instead of re-parsing them per
// container iteration.
//
// Three return values, exactly one non-nil:
//
//   - (pod, nil, nil)        — success.
//   - (nil, reason, nil)     — node rejected the pod (insufficient
//     resources, strict topology unsatisfiable,
//     etc.); caller should try the next node
//     and bucket the reason into the aggregate
//     FilteringFailed event.
//   - (nil, nil, err)        — internal/programmer error (annotation
//     encoding failed, accounting bug, ...);
//     the filter loop should abort, NOT just
//     skip the node — these signal real bugs.
//
// Containers are allocated in declaration order. addContainerAllocate
// updates node-side accounting between iterations so the next
// container's filterDevices sees the live AllocatableX values — which
// is how cross-container GPU sharing works (one physical card serving
// vGPUs from multiple containers, as long as each container's per-card
// resource needs fit in what the previous containers left behind).
func (alloc *allocator) Allocate(req *AllocationRequest) (*corev1.Pod, *reason.FilterReason, error) {
	pod := req.Pod
	klog.V(4).Infof("Attempt to allocate pod <%s> on node <%s>", klog.KObj(pod), alloc.nodeInfo.GetName())
	newPod := pod.DeepCopy()

	// Does the pod have a sequential (non-restartable) init container that
	// requests vGPU? Only then do we need the lifecycle-aware two-pass; the
	// common case (and sidecar-only pods) stays on the single-pass fast path.
	hasSequentialInit := false
	for i := range req.Containers {
		if req.Containers[i].Kind == util.ContainerKindInit && !req.Containers[i].Restartable {
			hasSequentialInit = true
			break
		}
	}

	var deviceClaims device.PodDeviceClaim

	if !hasSequentialInit {
		// Fast path: every vGPU container runs concurrently (regular app +
		// optional sidecars), so allocate in req.Containers order with
		// cross-container accumulation and append directly — allocation order
		// already equals the annotation order, so no reordering buffer is
		// needed. For a pod without init containers this is byte-for-byte the
		// historical behavior (no extra allocation).
		deviceClaims = make(device.PodDeviceClaim, 0, len(req.Containers))
		for i := range req.Containers {
			claim, rsn, err := alloc.allocateAndAccumulate(req, req.Containers[i], nil)
			if rsn != nil || err != nil {
				return nil, rsn, err
			}
			deviceClaims = append(deviceClaims, *claim)
		}
	} else {
		// claimByName collects each container's claim regardless of which pass
		// produced it (the two-pass allocates out of req.Containers order); the
		// annotation is assembled back in req.Containers order afterwards.
		claimByName := make(map[string]*device.ContainerDeviceClaim, len(req.Containers))
		// Two-pass, lifecycle-aware (see the design doc):
		//   reserve(g) = sidecarSum(g) + max(regularSum(g), maxInit(g))
		// Pass 1a/1b allocate the concurrent group (sidecars then regular app)
		// with accumulation. Pass 2 releases the regular-app reservation and
		// places each sequential init container against base+sidecars — they
		// run after the app phase and so reuse its GPUs. Each init is placed
		// independently (no inter-init accumulation; sequential inits never
		// overlap), preferring the pod's already-used GPUs to minimise the
		// reserved card set; the per-GPU max is realised at accounting time
		// via device.ReducePodFootprint.
		preferred := make(map[string]struct{})
		recordPreferred := func(claim *device.ContainerDeviceClaim) {
			for _, dc := range claim.DeviceClaims {
				preferred[dc.Uuid] = struct{}{}
			}
		}
		// Pass 1a: sidecars (concurrent, accumulate).
		for i := range req.Containers {
			n := &req.Containers[i]
			if n.Kind != util.ContainerKindInit || !n.Restartable {
				continue
			}
			claim, rsn, err := alloc.allocateAndAccumulate(req, *n, nil)
			if rsn != nil || err != nil {
				return nil, rsn, err
			}
			claimByName[n.Name] = claim
			recordPreferred(claim)
		}
		// Snapshot base+sidecars; the regular-app reservation added next is
		// released before the init pass.
		viewBaseSidecar := alloc.nodeInfo.SnapshotUsage()
		// Pass 1b: regular app containers (concurrent, accumulate).
		for i := range req.Containers {
			n := &req.Containers[i]
			if n.Kind != util.ContainerKindApp {
				continue
			}
			claim, rsn, err := alloc.allocateAndAccumulate(req, *n, nil)
			if rsn != nil || err != nil {
				return nil, rsn, err
			}
			claimByName[n.Name] = claim
			recordPreferred(claim)
		}
		// Release the regular-app reservation: init containers run after the
		// app phase, so they see base+sidecars only.
		alloc.nodeInfo.RestoreUsage(viewBaseSidecar)
		// Pass 2: sequential init containers (no accumulation between them).
		for i := range req.Containers {
			n := &req.Containers[i]
			if n.Kind != util.ContainerKindInit || n.Restartable {
				continue
			}
			var claim *device.ContainerDeviceClaim
			var rsn *reason.FilterReason
			var err error
			// Attempt 1: reuse the pod's already-used GPUs (densest). Skipped
			// when there are none (e.g. an init-only pod).
			if len(preferred) > 0 {
				claim, rsn, err = alloc.allocateOne(req, *n, preferred)
				if err != nil {
					klog.V(3).ErrorS(err, "init container reuse allocation internal error",
						"node", alloc.nodeInfo.GetName(), "pod", klog.KObj(pod), "container", n.Name)
					return nil, nil, err
				}
			}
			// Attempt 2: fall back to the whole node when reuse didn't fit
			// (claim == nil ⟺ attempt 1 was skipped or rejected). Still correct,
			// just reserves more cards.
			if claim == nil {
				claim, rsn, err = alloc.allocateOne(req, *n, nil)
				if err != nil {
					klog.V(3).ErrorS(err, "init container allocation internal error",
						"node", alloc.nodeInfo.GetName(), "pod", klog.KObj(pod), "container", n.Name)
					return nil, nil, err
				}
				if rsn != nil {
					klog.V(4).InfoS("init container allocation rejected", "node", alloc.nodeInfo.GetName(),
						"pod", klog.KObj(pod), "container", n.Name, "reason", rsn.Detailed())
					return nil, rsn, nil
				}
			}
			claimByName[n.Name] = claim
		}
		// Assemble per-container claims in req.Containers order (init-first),
		// which matches kubelet's Allocate call order and the device-plugin
		// PreAlloc cursor.
		deviceClaims = make(device.PodDeviceClaim, 0, len(req.Containers))
		for i := range req.Containers {
			deviceClaims = append(deviceClaims, *claimByName[req.Containers[i].Name])
		}
	}

	preAllocated, err := deviceClaims.MarshalText()
	if err != nil {
		returnErr := errors.New("pod device claim encoding failed")
		klog.V(2).ErrorS(err, returnErr.Error(), "node", alloc.nodeInfo.GetName(), "pod", klog.KObj(pod))
		return nil, nil, returnErr
	}
	util.InsertAnnotation(newPod, util.PodVGPUPreAllocAnnotation, preAllocated)
	util.InsertAnnotation(newPod, util.PodPredicateNodeAnnotation, alloc.nodeInfo.GetName())
	return newPod, nil, nil
}

// allocateAndAccumulate places one container and folds its claim into the node
// accounting so the next concurrent container sees the reduced availability —
// this is how cross-container GPU sharing within a single phase works. Used for
// the concurrent group (regular app + sidecars); sequential init containers are
// placed without accumulation (they never overlap).
func (alloc *allocator) allocateAndAccumulate(req *AllocationRequest, need ContainerNeed, restrictUUIDs map[string]struct{}) (*device.ContainerDeviceClaim, *reason.FilterReason, error) {
	claim, rsn, err := alloc.allocateOne(req, need, restrictUUIDs)
	if err != nil {
		klog.V(3).ErrorS(err, "container allocation internal error",
			"node", alloc.nodeInfo.GetName(), "pod", klog.KObj(req.Pod), "container", need.Name)
		return nil, nil, err
	}
	if rsn != nil {
		klog.V(4).InfoS("container allocation rejected", "node", alloc.nodeInfo.GetName(),
			"pod", klog.KObj(req.Pod), "container", need.Name, "reason", rsn.Detailed())
		return nil, rsn, nil
	}
	if err = alloc.addContainerAllocate(claim); err != nil {
		klog.V(3).ErrorS(err, "adding container resource allocation failed",
			"node", alloc.nodeInfo.GetName(), "pod", klog.KObj(req.Pod), "container", need.Name)
		return nil, nil, errors.New("internal device scheduling error")
	}
	return claim, nil, nil
}

func getDeviceUUIDs(devices []*device.Device) []string {
	uuids := make([]string, len(devices))
	for i, d := range devices {
		uuids[i] = d.GetUUID()
	}
	return uuids
}

// allocateOne picks devices for a single container.
//
// Three return values, same convention as Allocate:
//   - (claim, nil, nil)     — success.
//   - (nil, reason, nil)    — this container can't be placed on this node;
//     reason carries the structured cause (with
//     per-device counts when applicable).
//   - (nil, nil, err)       — internal error (shouldn't happen).
func (alloc *allocator) allocateOne(req *AllocationRequest, need ContainerNeed, restrictUUIDs map[string]struct{}) (*device.ContainerDeviceClaim, *reason.FilterReason, error) {
	klog.V(4).Infof("Attempt to allocate container <%s> on node <%s>", need.Name, alloc.nodeInfo.GetName())
	if need.Number > alloc.nodeInfo.GetSchedulableDeviceCount() {
		return nil, reason.New(reason.InsufficientGPUCards).
			WithDetail("need %d devices, node has %d schedulable", need.Number, alloc.nodeInfo.GetSchedulableDeviceCount()), nil
	}
	needCores, needMemory := resolveContainerNeeds(need, alloc.nodeInfo.NodeConfigInfo.MemoryFactor)

	deviceStore, deviceCounts := alloc.filterDevices(req, needCores, needMemory, restrictUUIDs)
	totalDevices := alloc.nodeInfo.GetDeviceCount()
	claims, rsn := alloc.pickDeviceClaims(req, deviceStore, need.Number, needCores, needMemory)
	if rsn != nil {
		// pickDeviceClaims surfaced its own structured reason (currently
		// only strict-topology rejection). Forward as-is so the original
		// Code (LinkTopologyUnsatisfied / NUMATopologyUnsatisfied) bubbles
		// up; the per-device counts from filterDevices are NOT relevant
		// here — topology unsatisfiable means the device count was fine,
		// just the connectivity / NUMA layout wasn't.
		return nil, rsn, nil
	}
	if len(claims) != need.Number {
		// Generic insufficient-resources path. Promote the per-device
		// counts from filterDevices into a node-level reason so the
		// aggregate event can bucket this node under the dominant cause
		// (Insufficient vGPU memory vs GPU type mismatch vs ...).
		// When counts is empty (e.g. zero devices on the node) fall back
		// to the generic "Insufficient GPU resources" code so the event
		// still says something useful.
		nodeReason := reason.FromCounts(deviceCounts, totalDevices)
		if nodeReason == nil {
			nodeReason = reason.New(reason.InsufficientGPUResources).
				WithDetail("need %d devices, none qualify", need.Number)
		}
		klog.V(5).InfoS("Insufficient node resources", "node", alloc.nodeInfo.GetName(),
			"pod", klog.KObj(req.Pod), "container", need.Name, "reason", nodeReason.Detailed())
		return nil, nodeReason, nil
	}
	sort.Slice(claims, func(i, j int) bool { return claims[i].Id < claims[j].Id })
	return &device.ContainerDeviceClaim{Name: need.Name, DeviceClaims: claims}, nil, nil
}

// resolveContainerNeeds applies the two implicit-fill rules from the
// pre-allocation semantics:
//
//   - vgpu-memory > 0 multiplies by the node's memoryFactor (user typing
//     gets converted to MB, matching what filterDevices and accounting
//     downstream both expect).
//   - vgpu-cores == 0 AND vgpu-memory == 0 promotes cores to HundredCore
//     so a "give me a whole card" pod actually reserves the full slice.
//
// vgpu-memory == 0 stays 0 here; buildClaims expands it to the device's
// total memory at claim-construction time so each picked device gets the
// right per-card value (which may differ on heterogeneous nodes).
func resolveContainerNeeds(need ContainerNeed, memoryFactor int) (cores, memory int64) {
	cores, memory = need.Cores, need.Memory
	if memory > 0 && memoryFactor > 0 {
		memory *= int64(memoryFactor)
	}
	if cores == 0 && memory == 0 {
		cores = util.HundredCore
	}
	return cores, memory
}

// pickDeviceClaims walks the shortest path that satisfies the request:
//
//   - len(deviceStore) < needNumber — bail with (nil, nil); the caller
//     promotes filterDevices' per-Code counts into a FilterReason for
//     the aggregate event.
//
//   - len(deviceStore) == needNumber and the pod isn't strict-topology —
//     the chosen set is forced (only one possible selection), so skip the
//     policy sort and topology dispatch entirely. Strict-topology pods
//     intentionally fall through so allocateByTopologyMode can validate
//     the forced set against the topology constraint (numa-strict on
//     scattered cards, link-strict on disconnected ones).
//
//   - otherwise — device-policy sort followed by topology-aware
//     selection. Strict topology rejections bubble up as a non-nil
//     *reason.FilterReason; non-strict topology failures fall back
//     internally and only emit a TopologyFallback event.
//
// Return shape:
//   - (claims, nil)        — picked successfully (or insufficient, with
//     len(claims) < needNumber so caller falls to
//     the count-promotion path).
//   - (nil, reason)        — strict topology rejected this node.
func (alloc *allocator) pickDeviceClaims(
	req *AllocationRequest, deviceStore []*device.Device,
	needNumber int, needCores, needMemory int64,
) ([]device.DeviceClaim, *reason.FilterReason) {
	switch {
	case needNumber > len(deviceStore):
		return nil, nil
	case needNumber == len(deviceStore) && (!req.TopologyStrict || needNumber <= 1):
		return buildClaims(deviceStore[:needNumber], needCores, needMemory), nil
	}
	alloc.sortDeviceStore(req, deviceStore)
	return alloc.allocateByTopologyMode(req, deviceStore, needNumber, needCores, needMemory)
}

// sortDeviceStore applies the device-level binpack/spread sort and emits
// the once-per-call diagnostic (info log for recognised policies, warning
// event for unrecognised user input). The policy enum is pre-normalised
// by BuildAllocationRequest, so the unrecognised case is detected via
// the preserved raw string.
func (alloc *allocator) sortDeviceStore(req *AllocationRequest, deviceStore []*device.Device) {
	pod := req.Pod
	switch req.DevicePolicy {
	case util.BinpackPolicy:
		klog.V(4).Infof("Pod <%s/%s> use <%s> device scheduling policy", pod.Namespace, pod.Name, req.DevicePolicy)
		NewDeviceBinpackPriority().Sort(deviceStore)
	case util.SpreadPolicy:
		klog.V(4).Infof("Pod <%s/%s> use <%s> device scheduling policy", pod.Namespace, pod.Name, req.DevicePolicy)
		NewDeviceSpreadPriority().Sort(deviceStore)
	default:
		if req.rawDevicePolicy != "" && req.rawDevicePolicy != string(util.NonePolicy) {
			klog.V(4).Infof("Pod <%s/%s> not supported device scheduling policy: %s", pod.Namespace, pod.Name, req.rawDevicePolicy)
			alloc.sendEventf(pod, corev1.EventTypeWarning, reason.EventPolicyInvalid,
				"unsupported device scheduling policy %q", req.rawDevicePolicy)
		} else {
			klog.V(4).Infof("Pod <%s/%s> none device scheduling policy", pod.Namespace, pod.Name)
		}
		NewSortPriority(ByNuma, ByDeviceIdAsc).Sort(deviceStore)
	}
}

func (alloc *allocator) sendEventf(object runtime.Object, eventtype, reason, messageFmt string, args ...interface{}) {
	if alloc.recorder != nil {
		alloc.recorder.Eventf(object, eventtype, reason, messageFmt, args...)
	}
}

// allocateByTopologyMode dispatches to topology-aware allocation.
//
// Returns:
//   - (claims, nil)        — topology succeeded, or non-strict fallback
//     took the non-topology path (a TopologyFallback
//     event is emitted in that case for visibility).
//   - (nil, *FilterReason) — strict topology unsatisfiable on this node;
//     the caller should propagate the reason up so
//     the filter loop drops just this node.
//
// req carries Topology / TopologyStrict / Profile pre-parsed; the Pod
// reference is used only for events and log keys.
func (alloc *allocator) allocateByTopologyMode(
	req *AllocationRequest, deviceStore []*device.Device,
	needNumber int, needCores, needMemory int64,
) ([]device.DeviceClaim, *reason.FilterReason) {
	if needNumber <= 1 {
		return buildClaims(deviceStore[:needNumber], needCores, needMemory), nil
	}
	pod := req.Pod
	strict := req.TopologyStrict

	switch req.Topology.BaseTopology() {
	case util.LinkTopology:
		// Cross-pod anchor: when enabled and this pod belongs to a gang, find the
		// NVLink component a sibling already pre-allocated on this node so we keep
		// this pod's GPUs connected to them. -1 = no anchor (non-gang, gate off,
		// or this is the gang's first pod here) → unchanged single-pod link path.
		anchorRoot := -1
		if req.CrossPodTopology && (req.GangName != "" || req.ControllerOwner != nil) {
			if root, ok := alloc.nodeInfo.GangAnchorComponent(req.GangName, req.ControllerOwner, sets.New(req.Pod.UID)); ok {
				// Priority 1: same-node sibling → exact NVLink component (UUID-based).
				// Intra-node connectivity is a hard requirement (NVLink doesn't cross
				// hosts), so an on-node sibling pins the component directly.
				anchorRoot = root
			} else if root, ok = alloc.nodeInfo.ComponentByOrdinal(req.GangLinkOrdinal); ok {
				// Priority 2: cross-node sibling → align to the same sub-domain
				// (rail) ordinal. The ordinal was resolved by the filter on the
				// sibling's own node (UUID-based); here we map it to THIS node's
				// component. Missing on this node (different shape) → no anchor.
				anchorRoot = root
			}
		}
		klog.V(4).Infof("Pod <%s/%s> use Links topology mode (strict=%v, anchorComponent=%d)", pod.Namespace, pod.Name, strict, anchorRoot)
		if claims, ok := alloc.allocateLink(deviceStore, req.Profile, req.DevicePolicy, strict, anchorRoot, needNumber, needCores, needMemory); ok {
			return claims, nil
		}
		if rsn := alloc.handleTopologyFallback(pod, strict,
			reason.LinkTopologyUnsatisfied, "Link topology", "non-topology allocation",
			alloc.linkFallbackReason(needNumber)); rsn != nil {
			return nil, rsn
		}
	case util.NUMATopology:
		klog.V(4).Infof("Pod <%s/%s> use NUMA topology mode (strict=%v)", pod.Namespace, pod.Name, strict)
		if claims, ok := alloc.allocateNUMA(deviceStore, req.Profile, req.DevicePolicy, needNumber, needCores, needMemory); ok {
			return claims, nil
		}
		if rsn := alloc.handleTopologyFallback(pod, strict,
			reason.NUMATopologyUnsatisfied, "NUMA topology", "cross-NUMA allocation",
			alloc.numaFallbackReason(needNumber, deviceStore)); rsn != nil {
			return nil, rsn
		}
	case util.NoneTopology, "":
		klog.V(4).Infof("Pod <%s/%s> none topology mode", pod.Namespace, pod.Name)
	default:
		klog.V(4).Infof("Pod <%s/%s> not supported topology mode: %s", pod.Namespace, pod.Name, req.Topology)
		alloc.sendEventf(pod, corev1.EventTypeWarning, reason.EventPolicyInvalid,
			"unsupported device topology mode %q", req.Topology)
	}
	return buildClaims(deviceStore[:needNumber], needCores, needMemory), nil
}

// handleTopologyFallback centralises the "strict → reject node / non-strict
// → emit TopologyFallback event" tail. On strict mode it returns a
// *reason.FilterReason carrying the unsatisfied-topology code so the
// filter loop drops only this node (other candidates still tried). On
// non-strict it emits a TopologyFallback event so operators see the
// downgrade in `kubectl describe pod` and returns nil — the caller then
// continues with the non-topology fallback path.
//
// strictCode is the reason.Code that goes into FilterReason on strict
// rejection (one of LinkTopologyUnsatisfied / NUMATopologyUnsatisfied).
// attemptKind / fallbackKind are the human-readable phrases that vary
// between modes ("Link topology" / "non-topology allocation" vs
// "NUMA topology" / "cross-NUMA allocation"); they appear only in the
// non-strict TopologyFallback event message.
func (alloc *allocator) handleTopologyFallback(
	pod *corev1.Pod, strict bool, strictCode reason.Code,
	attemptKind, fallbackKind, detail string,
) *reason.FilterReason {
	if strict {
		return reason.New(strictCode).WithDetail("%s", detail)
	}
	alloc.sendEventf(pod, corev1.EventTypeWarning, reason.EventTopologyFallback,
		"%s unsatisfiable on node %q (%s); falling back to %s",
		attemptKind, alloc.nodeInfo.GetName(), detail, fallbackKind)
	return nil
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

func (alloc *allocator) allocateLink(
	deviceStore []*device.Device, profile RequestProfile, policy util.SchedulerPolicy,
	strict bool, anchorRoot int, needNumber int, needCores, needMemory int64,
) ([]device.DeviceClaim, bool) {
	if !alloc.nodeInfo.HasGPUTopology() {
		return nil, false
	}
	devices, _ := alloc.nodeInfo.GetDeviceList().Filter(getDeviceUUIDs(deviceStore))

	// Cross-pod anchor windowing: restrict candidates to the NVLink component a
	// gang sibling already occupies on this node, so this pod stays connected to
	// them. If the window holds enough cards, select within it; if not, strict
	// rejects the node (can't keep the gang connected) while non-strict falls
	// back to the full candidate set — byte-for-byte the single-pod behaviour.
	// anchorRoot < 0 (non-gang, first sibling, or gate off) skips this entirely.
	if anchorRoot >= 0 {
		if windowed := alloc.windowToComponent(devices, anchorRoot); len(windowed) >= needNumber {
			devices = windowed
		} else if strict {
			return nil, false
		}
	}

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
		return buildClaims(resolveLinkDevices(got, deviceStore), needCores, needMemory), true
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
	return buildClaims(resolveLinkDevices(chosen, deviceStore), needCores, needMemory), true
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

// windowToComponent keeps only the devices whose UUID belongs to the given
// NVLink component root (from the NodeInfo's precomputed component index).
// Returns a new slice; nil for an unknown/empty component. Used by allocateLink
// to narrow the candidate set to a gang's anchored component.
func (alloc *allocator) windowToComponent(devices gpuallocator.DeviceList, root int) gpuallocator.DeviceList {
	members := alloc.nodeInfo.ComponentUUIDs(root)
	if len(members) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(members))
	for _, u := range members {
		set[u] = struct{}{}
	}
	out := make(gpuallocator.DeviceList, 0, len(devices))
	for _, d := range devices {
		if _, ok := set[d.UUID]; ok {
			out = append(out, d)
		}
	}
	return out
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
//
// NonePolicy callers don't reach this function (the no-policy branch of
// allocateLink takes the fast path), so policy is always Binpack or
// Spread here and Score returns a meaningful directional value.
func selectLinkCandidateByDevicePolicy(
	candidates [][]*gpuallocator.Device,
	deviceStore []*device.Device,
	profile RequestProfile,
	policy util.SchedulerPolicy,
) []*gpuallocator.Device {
	if len(candidates) <= 1 {
		return candidates[0]
	}
	byUUID := make(map[string]*device.Device, len(deviceStore))
	for _, d := range deviceStore {
		byUUID[d.GetUUID()] = d
	}
	// Single-pass argmax — we only want the best candidate, not a full
	// ordering, so a sort would do O(n log n) work for an O(n) decision.
	// Ties resolve to the lower-index candidate (preserves the link-score
	// ordering that AllocateLinkTopK established).
	bestIdx := 0
	bestScore := candidateSetScore(candidates[0], byUUID, profile, policy)
	for i := 1; i < len(candidates); i++ {
		if s := candidateSetScore(candidates[i], byUUID, profile, policy); s > bestScore {
			bestIdx, bestScore = i, s
		}
	}
	return candidates[bestIdx]
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
		claims = buildClaims(devices[:needNumber], needCores, needMemory)
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
	return fmt.Sprintf("no NUMA node has %d GPUs (max single-NUMA capacity %d)",
		needNumber, NewNumaNodeDevice(deviceStore).MaxDeviceNumberForNumaNode())
}

// buildClaims turns each picked device into a DeviceClaim, applying the
// implicit-full-memory rule (needMemory == 0 → device's whole card memory).
// Single entry point for both the per-device-numbers fast path and the
// post-topology link path so the implicit-full rule lives in exactly one
// place — the link path previously had its own copy that silently
// dropped `reqMemory` and wrote `needMemory` (0), leaving link-topology
// pods that omit vgpu-memory with Memory=0 claims.
func buildClaims(picked []*device.Device, needCores, needMemory int64) []device.DeviceClaim {
	claims := make([]device.DeviceClaim, len(picked))
	for i, d := range picked {
		mem := needMemory
		if mem == 0 {
			mem = d.GetTotalMemory()
		}
		claims[i] = device.DeviceClaim{
			Id:     d.GetID(),
			Uuid:   d.GetUUID(),
			Cores:  needCores,
			Memory: mem,
		}
	}
	return claims
}

// resolveLinkDevices maps each gpuallocator.Device back to its *device.Device
// counterpart in store. The gpuallocator package operates on its own Device
// shape (Index / UUID / Links) and doesn't carry the allocatable-resource
// accounting we need at claim time; UUID is the stable join key. Missing
// UUIDs are skipped rather than panicking — should never happen in practice
// because the topology selector picks from devices we passed in, but a
// defensive skip keeps a single stale entry from poisoning a whole claim
// list.
func resolveLinkDevices(picked []*gpuallocator.Device, store []*device.Device) []*device.Device {
	byUUID := make(map[string]*device.Device, len(store))
	for _, d := range store {
		byUUID[d.GetUUID()] = d
	}
	out := make([]*device.Device, 0, len(picked))
	for _, p := range picked {
		if d, ok := byUUID[p.UUID]; ok {
			out = append(out, d)
		}
	}
	return out
}

// filterDevices walks every GPU on the node and produces:
//   - the subset that survives every per-device gate (healthy, not in
//     MIG mode, has free vGPU slot / memory / cores, passes type / UUID
//     filters), in the order GetDeviceMap returns them;
//   - a per-Code count of HOW MANY devices each gate rejected, so the
//     caller can promote the dominant cause into a *reason.FilterReason
//     when no device survives.
//
// The Code keys come straight from the centralised vocabulary in
// pkg/scheduler/reason — no parallel enum here. That keeps the counts
// directly bucketable by the FilteringFailed aggregator without any
// translation table.
func (alloc *allocator) filterDevices(req *AllocationRequest, needCores, needMemory int64, restrictUUIDs map[string]struct{}) ([]*device.Device, map[reason.Code]int) {
	nodeName := alloc.nodeInfo.GetName()
	counts := make(map[reason.Code]int)
	devices := make([]*device.Device, 0, alloc.nodeInfo.GetDeviceCount())
	for i, deviceInfo := range alloc.nodeInfo.GetDeviceMap() {
		// Restrict to a preferred device set when requested. Used by the
		// init-container reuse pass to first try placing a sequential init
		// container only on the pod's already-chosen GPUs; on failure the
		// caller retries with no restriction. Skipped silently (not counted)
		// because it is an internal preference, not a user-facing rejection.
		if restrictUUIDs != nil {
			if _, ok := restrictUUIDs[deviceInfo.GetUUID()]; !ok {
				continue
			}
		}
		// Filter unhealthy device.
		if !deviceInfo.Healthy() {
			klog.V(4).InfoS("Filter unhealthy devices on the node", "node", nodeName,
				"deviceIndex", i, "deviceUuid", deviceInfo.GetUUID())
			counts[reason.DeviceUnhealthy]++
			continue
		}
		// Filter MIG enabled device.
		if deviceInfo.IsMIG() {
			klog.V(4).InfoS("Filter devices with MIG enabled on the node", "node",
				nodeName, "deviceIndex", i, "deviceUuid", deviceInfo.GetUUID())
			counts[reason.DeviceMIGEnabled]++
			continue
		}
		// Filter for insufficient number of virtual devices.
		if deviceInfo.AllocatableNumber() == 0 {
			klog.V(4).InfoS("Filter devices with insufficient available number on the node",
				"node", nodeName, "deviceIndex", i, "deviceUuid", deviceInfo.GetUUID())
			counts[reason.InsufficientVGPUSlot]++
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
			counts[reason.InsufficientVGPUMemory]++
			continue
		}
		if needCores > deviceInfo.AllocatableCores() || deviceInfo.AllocatableCores() == 0 {
			klog.V(4).InfoS("Filter devices with insufficient available cores on the node",
				"node", nodeName, "deviceIndex", i, "deviceUuid", deviceInfo.GetUUID(),
				"availableCores", deviceInfo.AllocatableCores(), "requestedCores", needCores)
			counts[reason.InsufficientVGPUCore]++
			continue
		}
		// Filter device type.
		if req.CheckDeviceType && !util.CheckDeviceType(req.Pod.Annotations, deviceInfo.GetType()) {
			klog.V(4).InfoS("Filter devices with type mismatches on the node",
				"node", nodeName, "deviceIndex", i, "deviceType", deviceInfo.GetType(),
				"includeTypes", req.Pod.Annotations[util.PodIncludeGpuTypeAnnotation],
				"excludeTypes", req.Pod.Annotations[util.PodExcludeGpuTypeAnnotation])
			counts[reason.DeviceTypeMismatch]++
			continue
		}
		// Filter device uuid.
		if req.CheckDeviceUuid && !util.CheckDeviceUuid(req.Pod.Annotations, deviceInfo.GetUUID()) {
			klog.V(4).InfoS("Filter devices with uuid mismatches on the node",
				"node", nodeName, "deviceIndex", i, "deviceUuid", deviceInfo.GetUUID(),
				"includeUuids", req.Pod.Annotations[util.PodIncludeGPUUUIDAnnotation],
				"excludeUuids", req.Pod.Annotations[util.PodExcludeGPUUUIDAnnotation])
			counts[reason.DeviceUUIDMismatch]++
			continue
		}
		devices = append(devices, deviceInfo)
	}
	return devices, counts
}
