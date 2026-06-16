package allocator

import "sync/atomic"

// crossPodLinkTopologyEnabled is a process-wide switch for cross-pod NVLink
// affinity (keeping same-gang pods within one connected component on a node).
// The scheduler sets it once at startup from the CrossPodLinkTopology feature
// gate; it is read on the allocator hot path. Kept as a package-level toggle —
// rather than threaded through NewAllocator / the filter / preempt call sites —
// because it is a single boolean process config with no per-node variation, so
// a startup setter keeps the cross-pod change out of every allocator caller.
// Default false: when unset (every existing deployment and unit test) the
// allocator never enters the anchor path and behaves exactly as before.
var crossPodLinkTopologyEnabled atomic.Bool

// SetCrossPodLinkTopology configures whether cross-pod NVLink anchoring is
// active. Called once at scheduler startup; tests may set it explicitly.
func SetCrossPodLinkTopology(enabled bool) {
	crossPodLinkTopologyEnabled.Store(enabled)
}

// CrossPodLinkTopologyEnabled reports the current setting.
func CrossPodLinkTopologyEnabled() bool {
	return crossPodLinkTopologyEnabled.Load()
}
