package allocator

import (
	"errors"
	"fmt"
	"sort"

	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/scheduler/metrics"
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
	// simulate suppresses every observable side effect (events, metrics).
	// See NewSimulationAllocator.
	simulate bool
}

func NewAllocator(nodeInfo *device.NodeInfo, recorder record.EventRecorder) *allocator {
	return &allocator{
		nodeInfo: nodeInfo,
		recorder: recorder,
	}
}

// NewSimulationAllocator builds an allocator for DRY RUNS — currently
// preemption, which re-runs allocation once per victim set it tests to ask
// "would the pod fit if these pods were gone?".
//
// Simulations must not be observable. They place nothing, and a single
// preemption can run several of them, so counting their searches and outcomes would
// inflate the per-search metrics by an unbounded factor and make the numbers
// unreadable. Excluding them here, at the source, is more reliable than trying
// to subtract them from a dashboard later.
func NewSimulationAllocator(nodeInfo *device.NodeInfo) *allocator {
	return &allocator{
		nodeInfo: nodeInfo,
		simulate: true,
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
	var newPod = pod.DeepCopy()
	var deviceClaims device.PodDeviceClaim

	// Does the pod have a sequential (non-restartable) init container that
	// requests vGPU? Only then do we need the lifecycle-aware two-pass; the
	// common case (and sidecar-only pods) stays on the single-pass fast path.
	if !req.HasSequentialInit {
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
		for _, need := range req.Containers {
			if need.Kind != util.ContainerKindInit || !need.Restartable {
				continue
			}
			claim, rsn, err := alloc.allocateAndAccumulate(req, need, nil)
			if rsn != nil || err != nil {
				return nil, rsn, err
			}
			claimByName[need.Name] = claim
			recordPreferred(claim)
		}
		// Snapshot base+sidecars; the regular-app reservation added next is
		// released before the init pass.
		viewBaseSidecar := alloc.nodeInfo.SnapshotUsage()
		// Pass 1b: regular app containers (concurrent, accumulate).
		for _, need := range req.Containers {
			if need.Kind != util.ContainerKindApp {
				continue
			}
			claim, rsn, err := alloc.allocateAndAccumulate(req, need, nil)
			if rsn != nil || err != nil {
				return nil, rsn, err
			}
			claimByName[need.Name] = claim
			recordPreferred(claim)
		}
		// Release the regular-app reservation: init containers run after the
		// app phase, so they see base+sidecars only.
		alloc.nodeInfo.RestoreUsage(viewBaseSidecar)
		// Pass 2: sequential init containers (no accumulation between them).
		for _, need := range req.Containers {
			if need.Kind != util.ContainerKindInit || need.Restartable {
				continue
			}
			var claim *device.ContainerDeviceClaim
			var rsn *reason.FilterReason
			var err error
			// Attempt 1: reuse the pod's already-used GPUs (densest). Skipped
			// when there are none (e.g. an init-only pod).
			if len(preferred) > 0 {
				claim, rsn, err = alloc.allocateOne(req, need, preferred)
				if err != nil {
					klog.V(3).ErrorS(err, "init container reuse allocation internal error",
						"node", alloc.nodeInfo.GetName(), "pod", klog.KObj(pod), "container", need.Name)
					return nil, nil, err
				}
			}
			// Attempt 2: fall back to the whole node when reuse didn't fit
			// (claim == nil ⟺ attempt 1 was skipped or rejected). Still correct,
			// just reserves more cards.
			if claim == nil {
				claim, rsn, err = alloc.allocateOne(req, need, nil)
				if err != nil {
					klog.V(3).ErrorS(err, "init container allocation internal error",
						"node", alloc.nodeInfo.GetName(), "pod", klog.KObj(pod), "container", need.Name)
					return nil, nil, err
				}
				if rsn != nil {
					klog.V(4).InfoS("init container allocation rejected", "node",
						alloc.nodeInfo.GetName(), "pod", klog.KObj(pod), "container", need.Name, "reason", rsn.Detailed())
					return nil, rsn, nil
				}
			}
			claimByName[need.Name] = claim
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
	needCores, needMemory := resolveContainerNeeds(need, alloc.nodeInfo.MemoryFactor, alloc.nodeInfo.HasSameCapacity(), alloc.nodeInfo.GetMaxDeviceMemory())

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
func resolveContainerNeeds(
	need ContainerNeed, memoryFactor int,
	allSameCapacity bool, memoryCapacity int64,
) (cores, memory int64) {
	cores, memory = need.Cores, need.Memory
	if memory > 0 && memoryFactor > 0 {
		memory *= int64(memoryFactor)
	}
	if cores == 0 && memory == 0 {
		cores = util.HundredCore
	}
	if memory == 0 && allSameCapacity {
		memory = memoryCapacity
	}
	return cores, memory
}

// pickDeviceClaims walks the shortest path that satisfies the request:
//
//   - len(deviceStore) < needNumber — bail with (nil, nil); the caller
//     promotes filterDevices' per-Code counts into a FilterReason for
//     the aggregate event.
//
//   - otherwise — device-policy sort followed by topology-aware
//     selection. Strict topology rejections bubble up as a non-nil
//     *reason.FilterReason; non-strict topology failures fall back
//     internally and only emit a TopologyFallback event.
//
// There is deliberately NO fast path for a forced set (needNumber ==
// len(deviceStore)). Skipping the dispatch there would save a sort over
// needNumber elements and a tier walk that has exactly one possible answer —
// and would cost the two things the topology path exists to produce besides the
// answer itself:
//
//   - the OUTCOME METRIC. topology_placement_total{mode,result} is documented as
//     "what connectivity did pods asking for link/numa actually get". A forced
//     set that skips the dispatch records nothing, so those placements vanish
//     from that metric while still being counted in pod_policy_total — the two
//     stop reconciling, and the gap is invisible rather than reported as a
//     degradation. Since a forced set is precisely the case where the node had
//     no room to choose well, silently dropping it biases the metric toward
//     looking healthier than the cluster is.
//   - the DOWNGRADE EVENT. A pod that asked for NVLink and got cards with no
//     interconnect deserves the same TopologyFallback signal whether or not the
//     node happened to have exactly enough devices.
//
// Strict correctness depended on this too: the previous condition carried an
// `|| needNumber <= 1` escape, so a 1-GPU request landing on a node with a
// single candidate device bypassed strict validation entirely and was accepted
// on nodes with no NVLink and no NUMA at all.
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
	if needNumber > len(deviceStore) {
		return nil, nil
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
	case util.BinpackPolicy, util.SpreadPolicy, util.NonePolicy:
		klog.V(4).Infof("Pod <%s> use <%s> device scheduling policy", klog.KObj(pod), req.DevicePolicy)
	default:
		klog.V(4).Infof("Pod <%s> not supported device scheduling policy: %q", klog.KObj(pod), req.DevicePolicy)
		alloc.sendEventf(pod, corev1.EventTypeWarning, reason.EventPolicyInvalid, "unsupported device scheduling policy %q", req.DevicePolicy)
	}
	// TODO The device score weight used here is the average value, which may be adjusted in the future
	NewDevicePolicyPriority(*req).Sort(deviceStore)
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
	req *AllocationRequest, deviceStore []*device.Device, needNumber int, needCores, needMemory int64,
) ([]device.DeviceClaim, *reason.FilterReason) {
	pod := req.Pod

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
			} else if root, ok = alloc.nodeInfo.ComponentByDomain(req.GangDomainKey); ok {
				// Priority 2: cross-node sibling → align to the same sub-domain
				// (rail) signature. The domain key was resolved by the filter on the
				// sibling's own node (UUID-based); here we map it to THIS node's
				// component. Missing on this node (rail-set absent) → no anchor.
				anchorRoot = root
			}
		}
		klog.V(4).Infof("Pod <%s> use Links topology mode (strict=%v, anchorComponent=%d)", klog.KObj(pod), req.TopologyStrict, anchorRoot)
		if plan, ok := alloc.allocateLink(deviceStore, req, anchorRoot, needNumber); ok {
			// Placed — but link mode promises NVLink, and the tier walk may have
			// had to settle for less. Report that, because otherwise a pod that
			// asked for NVLink and got a PCIe-switch group (or, on a link-less
			// node, cards with no connectivity at all) is indistinguishable in
			// the logs from one that got exactly what it wanted.
			//
			// The DEGRADED SET IS STILL USED. It is chosen from the tightest
			// tier that could host the request, so it is never worse than the
			// non-topology fallback and usually better — discarding it just to
			// signal the downgrade would trade real placement quality for a
			// message. Strict never reaches here: allocateLink refuses anything
			// below NVLink for it.
			if plan.Tier != device.TierNVLink || plan.Spanned {
				alloc.reportLinkDowngrade(pod, plan, needNumber)
			}
			alloc.recordOutcome(req, linkResult(plan), alignmentOf(req, anchorRoot))
			return buildClaims(plan.Devices, needCores, needMemory), nil
		}
		if rsn := alloc.handleTopologyFallback(
			pod, req.TopologyStrict,
			reason.LinkTopologyUnsatisfied, util.LinkTopology,
			"Link topology",
			"non-topology allocation",
			alloc.linkFallbackReason(needNumber)); rsn != nil {
			return nil, rsn
		}
		// Non-strict fell all the way through to resource-ordered allocation.
		alloc.recordOutcome(req, metrics.ResultNone, alignmentOf(req, anchorRoot))
	case util.NUMATopology:
		klog.V(4).Infof("Pod <%s> use NUMA topology mode (strict=%v)", klog.KObj(pod), req.TopologyStrict)
		// TODO RequestProfile uses average value, maintain semantic consistency with sortDeviceStore.
		if claims, ok := alloc.allocateNUMA(deviceStore, UniformProfile, req.DevicePolicy, needNumber, needCores, needMemory); ok {
			alloc.recordOutcome(req, metrics.ResultNUMA, "")
			return claims, nil
		}
		if rsn := alloc.handleTopologyFallback(
			pod, req.TopologyStrict,
			reason.NUMATopologyUnsatisfied, util.NUMATopology,
			"NUMA topology",
			"cross-NUMA allocation",
			alloc.numaFallbackReason(needNumber, deviceStore)); rsn != nil {
			return nil, rsn
		}
		// Non-strict: placed, but spanning NUMA nodes.
		alloc.recordOutcome(req, metrics.ResultCrossNUMA, "")
	case util.NoneTopology:
		klog.V(4).Infof("Pod <%s> none topology mode", klog.KObj(pod))
	default:
		klog.V(4).Infof("Pod <%s> not supported topology mode: %q", klog.KObj(pod), req.Topology)
		alloc.sendEventf(pod, corev1.EventTypeWarning, reason.EventPolicyInvalid, "unsupported device topology mode %q", req.Topology)
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
	mode util.TopologyMode, attemptKind, fallbackKind, detail string,
) *reason.FilterReason {
	if strict {
		// PER NODE EVALUATION: this counts nodes refused, not pods. One pod can
		// be refused by every node in the cluster.
		if !alloc.simulate {
			metrics.TopologyStrictRejectTotal.WithLabelValues(metrics.TopologyLabel(mode)).Inc()
		}
		return reason.New(strictCode).WithDetail("%s", detail)
	}
	alloc.sendEventf(pod, corev1.EventTypeWarning, reason.EventTopologyFallback,
		"%s unsatisfiable on node %q (%s); falling back to %s",
		attemptKind, alloc.nodeInfo.GetName(), detail, fallbackKind)
	return nil
}

// allocateLink selects devices via the node's tiered connectivity view.
//
// Returns (plan, true) on success; (nil, false) means the caller should fall
// back (non-strict) or reject the node (strict). The plan carries the tier
// actually achieved so the caller can report a downgrade — a non-strict request
// can legitimately be placed BELOW NVLink, and that needs to be visible rather
// than silently indistinguishable from a full-connectivity placement.
//
// strict is satisfied only when the chosen set is connected at the NVLink tier.
// That check is a direct property of the selection — the tier the walk landed
// on — rather than a post-hoc validation of an opaque search result, which is
// what previously made it possible to receive a disconnected set and have to
// re-verify it.
func (alloc *allocator) allocateLink(
	deviceStore []*device.Device, req *AllocationRequest, anchorRoot int, needNumber int,
) (*linkPlan, bool) {
	if !alloc.nodeInfo.HasGPUTopology() {
		return nil, false
	}

	// Cross-pod alignment windows, tried finest-first and DEGRADING rather than
	// failing. anchorRoot < 0 (non-gang, first sibling on this node, or gate
	// off) leaves L2 unset, and an absent GangRailKey leaves L1 unset:
	//
	//   L1  same rail set as the sibling  — the only key that works for
	//                                       single-card gang members, because a
	//                                       fully connected node has exactly one
	//                                       NVLink component and its signature is
	//                                       therefore identical on every node
	//   L2  same NVLink component         — the historical alignment
	//   L3  no window
	//
	// Degrading matters: an exact rail match is precise but brittle (those cards
	// may be busy here), and over-constraining would make a pod unschedulable in
	// exchange for an optimisation.
	var (
		railWindow      sets.Set[string]
		componentWindow sets.Set[string]
		hasAnchor       = anchorRoot >= 0
	)
	if req.GangRailKey != "" {
		if w := sets.New[string](alloc.nodeInfo.UUIDsMatchingRailSignature(req.GangRailKey)...); w.Len() >= needNumber {
			railWindow = w
		}
	}
	if hasAnchor {
		if w := sets.New[string](alloc.nodeInfo.ComponentUUIDs(anchorRoot)...); w.Len() >= needNumber {
			componentWindow = w
		} else if req.TopologyStrict {
			// strict promised the gang stays connected and this node's anchored
			// component is too small, so widening would break the contract.
			return nil, false
		}
	}

	// acceptable decides whether a plan honours the caller's contract. strict
	// demands a single NVLink-connected set; non-strict takes whatever the tier
	// walk produced.
	acceptable := func(p *linkPlan) bool {
		return p != nil && (!req.TopologyStrict || (p.Tier == device.TierNVLink && !p.Spanned))
	}

	// Build the attempt list explicitly. A nil entry means "no window", so the
	// windows that were not resolved must be OMITTED rather than left as nil
	// holes — otherwise an absent rail window would silently become an
	// unwindowed attempt and skip the anchor entirely.
	attempts := make([]sets.Set[string], 0, 3)
	if railWindow != nil {
		attempts = append(attempts, railWindow)
	}
	if componentWindow != nil {
		attempts = append(attempts, componentWindow)
	}
	// The unwindowed attempt drops gang affinity. strict promised to keep the
	// gang connected, so it is only offered when there is no anchor to honour.
	if !(req.TopologyStrict && hasAnchor) {
		attempts = append(attempts, nil)
	}

	// Keep trying while the plan does not satisfy strict. A rail window can
	// legitimately produce a LOOSER plan than the component window would — on a
	// heterogeneous cluster the rail-matched cards may straddle two NVLink
	// components — so stopping at the first non-nil plan would reject a node the
	// component window could still have satisfied.
	var plan *linkPlan
	for _, window := range attempts {
		got := alloc.allocateTiered(req, deviceStore, needNumber, window)
		if acceptable(got) {
			plan = got
			break
		}
		if plan == nil {
			plan = got // best non-strict candidate seen so far
		}
	}
	if !acceptable(plan) {
		return nil, false
	}
	klog.V(5).InfoS("Link topology selection", "node", alloc.nodeInfo.GetName(), "pod",
		klog.KObj(req.Pod), "tier", plan.Tier, "spanned", plan.Spanned, "devices", getDeviceUUIDs(plan.Devices))
	return plan, true
}

// recordOutcome stores what this node's topology decision achieved, so the
// filter can report the pod's real outcome once it knows which node won.
//
// Recorded on the request rather than counted here because THIS runs per node
// evaluation: the filter may examine several nodes before one accepts the pod,
// and counting each attempt would report a single pod as several placements.
// Simulations record nothing at all — they place nothing.
func (alloc *allocator) recordOutcome(req *AllocationRequest, result, alignment string) {
	if alloc.simulate {
		return
	}
	req.recordTopologyOutcome(result, alignment)
}

// linkResult maps a plan to the metric vocabulary. Spanning outranks the tier:
// a set split across components has no single connectivity level to report, and
// "no interconnect" is the fact worth surfacing.
func linkResult(plan *linkPlan) string {
	if plan.Spanned {
		return metrics.ResultSpanned
	}
	switch plan.Tier {
	case device.TierNVLink:
		return metrics.ResultNVLink
	case device.TierSwitch:
		return metrics.ResultSwitch
	case device.TierNUMA:
		return metrics.ResultNUMA
	default:
		return metrics.ResultAny
	}
}

// alignmentOf reports which cross-pod alignment key steered this placement.
// Empty for pods that did not opt in, so they contribute no series at all.
func alignmentOf(req *AllocationRequest, anchorRoot int) string {
	if !req.CrossPodTopology {
		return ""
	}
	switch {
	case req.GangRailKey != "":
		return metrics.AlignRail
	case anchorRoot >= 0:
		return metrics.AlignComponent
	default:
		return metrics.AlignNone
	}
}

// reportLinkDowngrade records that a non-strict link request was placed BELOW
// the NVLink connectivity the mode promises.
//
// Before the tier walk there was no way to say this: the old search returned a
// device set with no indication of how well connected it was, and only the
// strict path re-checked connectivity, in a separate pass after the fact. So a
// pod that asked for link topology and received cards with no interconnect at
// all looked exactly like one that got a full NVLink group. Operators had no
// signal that the cluster could not honour what they requested.
//
// Emitted at most once per placement — the filter stops at the first node that
// accepts the pod, and a downgrade is still an acceptance.
func (alloc *allocator) reportLinkDowngrade(pod *corev1.Pod, plan *linkPlan, needNumber int) {
	achieved := plan.Tier.String()
	if plan.Spanned {
		// Spanning means the set could not be contained in ONE component even
		// at this tier, i.e. parts of it have no direct path to each other.
		// Worth calling out separately: it is the difference between "slower
		// interconnect" and "no interconnect".
		achieved += " (spanning multiple components)"
	}
	klog.V(3).InfoS("Link topology downgraded", "node", alloc.nodeInfo.GetName(),
		"pod", klog.KObj(pod), "want", device.TierNVLink.String(), "got", achieved,
		"devices", getDeviceUUIDs(plan.Devices))
	alloc.sendEventf(pod, corev1.EventTypeWarning, reason.EventTopologyFallback,
		"Link topology downgraded on node %q: %d GPUs connected at %q, not NVLink; "+
			"use link-strict to reject such nodes instead",
		alloc.nodeInfo.GetName(), needNumber, achieved)
}

// allocateNUMA attempts to satisfy the request within a single NUMA node,
// applying the binpack/spread policy to choose which NUMA node to consume.
// Returns (claims, false) when no NUMA node alone can hold needNumber devices.
func (alloc *allocator) allocateNUMA(
	deviceStore []*device.Device, profile RequestProfile,
	policy util.SchedulerPolicy, needNumber int, needCores, needMemory int64,
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
	// HasGPUTopology was true → the cause is connectivity, in one of two shapes:
	// strict refused every plan because none reached TierNVLink, or an anchor /
	// rail window left too few candidates to form a group at all.
	//
	// Report the largest NVLink component, NOT the largest any-P2P one. The
	// latter is the number the node-wide component map would give and it is
	// almost always the full card count (every GPU is PCIe-reachable), which
	// would render as the nonsense "no NVLink set of 4 (largest component 8)".
	return fmt.Sprintf("no NVLink-connected set of %d GPUs (largest NVLink component %d)",
		needNumber, alloc.nodeInfo.LinkTierMaxComponentSize(device.TierNVLink))
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
