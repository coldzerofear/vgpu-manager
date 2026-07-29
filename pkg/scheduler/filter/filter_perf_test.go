package filter

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/device/gpuallocator/links"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
	extenderv1 "k8s.io/kube-scheduler/extender/v1"
)

// Test_FilterPerf is a perf-tracking benchmark, NOT a correctness test.
// Skipped unless VGPU_PERF=1. Reports per-pod and aggregate filter
// latency across a matrix of (node count, pod count, policy combination)
// with serialFilterNode=true (the production-like setting).
//
// Knobs (all comma-separated, all optional):
//
//	VGPU_PERF=1              run the test
//	VGPU_PERF_NODES=...      node counts            (default 100,1000)
//	VGPU_PERF_PODS=...       pod counts             (default 100,1000)
//	VGPU_PERF_POLICIES=...   names from the policy
//	                         table below            (default: all 5)
//	VGPU_PERF_FULL=1         override the defaults with the headline
//	                         matrix nodes=100,1000,5000
//	                                pods=1000,10000,100000
//	                         Note: the 5000×100000 case can run for
//	                         hours; use smaller knobs for routine runs.
//
// Output goes to t.Logf and ends with a single tab-aligned summary
// table so future runs can be eyeballed against the baseline kept in
// the same file (the table at the bottom of this comment).
//
// Each scenario:
//  1. pre-creates `nodeCount` nodes (4 GPUs each, deviceSplit=10, so
//     40 vGPU slots per node — enough headroom for 100k 1-vGPU pods on
//     a 5k-node cluster).
//  2. pre-creates `podCount` 1-vGPU pods through the fake clientset so
//     the kube-scheduler-extender Patch operations have something to
//     patch. THIS WORK IS NOT TIMED.
//  3. starts the timer, calls f.Filter() once per pod sequentially
//     (matches kube-scheduler's serial Filter calls), stops the timer
//     after the last pod.
//  4. reports total wall-clock, per-pod average, and a count of pods
//     whose Filter() returned an error message (which on a sized-up
//     cluster typically means we ran out of capacity, NOT a bug).
//
// The lock is on (serialFilterNode=true) so the measurement reflects
// the production setting where the filter holds its in-process mutex
// while building NodeInfo + running the allocator.
func Test_FilterPerf(t *testing.T) {
	if os.Getenv("VGPU_PERF") == "" {
		t.Skip("set VGPU_PERF=1 to run filter perf benchmark (slow)")
	}

	nodeCounts := parsePerfInts("VGPU_PERF_NODES", []int{100, 1000})
	podCounts := parsePerfInts("VGPU_PERF_PODS", []int{100, 1000})
	if os.Getenv("VGPU_PERF_FULL") != "" {
		nodeCounts = []int{100, 1000, 5000}
		podCounts = []int{1000, 10000, 100000}
	}

	policies := selectPolicies(os.Getenv("VGPU_PERF_POLICIES"))

	type rec struct {
		scenario string
		nodes    int
		pods     int
		total    time.Duration
		perPod   time.Duration
		errors   int
	}
	var results []rec

	for _, nc := range nodeCounts {
		for _, pc := range podCounts {
			for _, pv := range policies {
				name := fmt.Sprintf("nodes=%d/pods=%d/%s", nc, pc, pv.name)
				t.Run(name, func(t *testing.T) {
					total, perPod, errs := runFilterPerfScenario(t, nc, pc, pv)
					results = append(results, rec{
						scenario: name, nodes: nc, pods: pc,
						total: total, perPod: perPod, errors: errs,
					})
					t.Logf("PERF %s: total=%v perPod=%v errs=%d",
						name, total.Round(time.Millisecond), perPod, errs)
				})
			}
		}
	}

	// Sort the summary by (nodes, pods, scenario name) so the printed
	// table is deterministic regardless of the iteration order above.
	sort.Slice(results, func(i, j int) bool {
		if results[i].nodes != results[j].nodes {
			return results[i].nodes < results[j].nodes
		}
		if results[i].pods != results[j].pods {
			return results[i].pods < results[j].pods
		}
		return results[i].scenario < results[j].scenario
	})

	var b strings.Builder
	b.WriteString("\n=== FILTER PERF SUMMARY (serialFilterNode=true) ===\n")
	fmt.Fprintf(&b, "%-50s  %12s  %12s  %8s\n", "scenario", "total", "per-pod", "errors")
	for _, r := range results {
		fmt.Fprintf(&b, "%-50s  %12v  %12v  %8d\n",
			r.scenario, r.total.Round(time.Millisecond), r.perPod, r.errors)
	}
	t.Log(b.String())
}

// perfPolicy is the combination of annotation values exercised by one
// row of the perf matrix. Each field maps to a real pod annotation;
// leaving a field empty means the annotation is OMITTED from the pod
// (NOT set to ""), which routes the pod through the no-policy code
// path instead of the "unsupported policy ”" event branch.
//
// The matrix is bigger than it looks because three independent axes
// interact with three different code paths:
//
//   - nodePolicy   drives the node ranking comparator
//     (WeightedNodeLess) in deviceFilter.
//   - devicePolicy drives the device-level sort
//     (NewDeviceBinpackPriority / NewDeviceSpreadPriority)
//     AND the topology compose-vs-fast-path branch.
//   - topology     drives allocateLink / allocateNUMA dispatch.
//
// The topology × devicePolicy interaction is the one that's easy to
// miss: with devicePolicy=none, allocateLink takes the FAST PATH
// (one bestEffort call), and allocateNUMA uses DefaultCallback (no
// per-NUMA scoring). With devicePolicy!=none those become
// AllocateLinkTopK + selectLinkCandidateByDevicePolicy and
// Binpack/SpreadCallback respectively — meaningfully more expensive.
// We need both rows in the matrix to expose the cost.
type perfPolicy struct {
	name         string
	nodePolicy   string // util.NodeSchedulerPolicyAnnotation;   "" = unset
	devicePolicy string // util.DeviceSchedulerPolicyAnnotation; "" = unset
	topology     string // util.DeviceTopologyModeAnnotation;    "" = unset
}

// allPerfPolicies is the default matrix. Picks were guided by "what
// distinct code paths actually run?" — combinations that hit the same
// path twice with slightly different params don't appear.
var allPerfPolicies = []perfPolicy{
	// Baseline: every gate off.
	{name: "none"},

	// Node-only: WeightedNodeLess fires (binpack vs spread direction).
	{name: "node=binpack", nodePolicy: "binpack"},
	{name: "node=spread", nodePolicy: "spread"},

	// Device-only: NewDeviceBinpackPriority / SpreadPriority fires; no
	// topology dispatch (allocateByTopologyMode falls into the
	// NoneTopology branch).
	{name: "dev=binpack", devicePolicy: "binpack"},
	{name: "dev=spread", devicePolicy: "spread"},

	// Both: both sorts fire, no topology.
	{name: "node=binpack/dev=binpack", nodePolicy: "binpack", devicePolicy: "binpack"},

	// Link topology, FAST path: devicePolicy=none → allocateLink calls
	// gpuallocator.AllocateLink (single bestEffort) and returns directly.
	{name: "link", topology: "link"},

	// Link topology, COMPOSE path: devicePolicy=binpack →
	// AllocateLinkTopK + selectLinkCandidateByDevicePolicy. Real cost
	// difference vs the fast path comes from the top-K enumeration.
	{name: "link/dev=binpack", topology: "link", devicePolicy: "binpack"},
	{name: "link/dev=spread", topology: "link", devicePolicy: "spread"},

	// Link strict: same selector as link/none but with the
	// AreDevicesLinked validation pass. Cheap vs the compose path —
	// useful as a separate row only because the validation cost is
	// what we'd measure first if a regression here.
	{name: "link-strict", topology: "link-strict"},

	// NUMA topology, FAST path: devicePolicy=none →
	// SchedulerPolicyCallback → DefaultCallback (no scoring sort).
	{name: "numa", topology: "numa"},

	// NUMA topology, COMPOSE path: devicePolicy=binpack →
	// Binpack/SpreadCallback (per-NUMA Score sort).
	{name: "numa/dev=binpack", topology: "numa", devicePolicy: "binpack"},

	// NUMA strict: same callback as numa/none but the strict failure
	// path is exercised on nodes where CanNotCrossNumaNode returns
	// false. With our 2-vGPU pods all 2 land on the same NUMA (the
	// mesh is 2+2), so this exercises the success path.
	{name: "numa-strict", topology: "numa-strict"},

	// Full combo: node + device + link topology compose path.
	// Closest to what a real "binpack everything please" pod looks
	// like and the worst case any single pod will see.
	{name: "all=binpack/link", nodePolicy: "binpack", devicePolicy: "binpack", topology: "link"},
}

func selectPolicies(env string) []perfPolicy {
	if env == "" {
		return allPerfPolicies
	}
	wanted := map[string]bool{}
	for _, s := range strings.Split(env, ",") {
		wanted[strings.TrimSpace(s)] = true
	}
	var out []perfPolicy
	for _, p := range allPerfPolicies {
		if wanted[p.name] {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return allPerfPolicies
	}
	return out
}

func parsePerfInts(envVar string, defaults []int) []int {
	raw := os.Getenv(envVar)
	if raw == "" {
		return defaults
	}
	var out []int
	for _, s := range strings.Split(raw, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err == nil && n > 0 {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		return defaults
	}
	return out
}

// runFilterPerfScenario drives one cell of the matrix. Returns total
// wall-clock for the Filter loop, the per-pod average, and a count of
// Filter() responses that came back with an Error string set (treated
// as "capacity exhausted" which is expected past a certain pod count
// for any given node footprint).
func runFilterPerfScenario(t *testing.T, nodeCount, podCount int, pv perfPolicy) (total, perPod time.Duration, errs int) {
	t.Helper()

	k8sClient := fake.NewClientset()
	factory := informers.NewSharedInformerFactory(k8sClient, 0)
	broadcaster := record.NewBroadcaster()
	recorder := broadcaster.NewRecorder(nil, corev1.EventSource{Component: "perf"})
	defer broadcaster.Shutdown()

	// serialFilterNode=true to match the production-like setting the
	// user wanted us to measure under.
	fp, err := New(k8sClient, factory, recorder, true, true)
	if err != nil {
		t.Fatalf("filter New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())

	nodes := buildPerfNodes(nodeCount)
	nodeList := &corev1.NodeList{Items: nodes}

	// Pre-create pods in the fake clientset so the per-pod Patch
	// inside Filter() has a target. Pod creation is intentionally
	// outside the timing window.
	pods := make([]*corev1.Pod, podCount)
	for i := 0; i < podCount; i++ {
		p := buildPerfPod(i, pv)
		created, err := k8sClient.CoreV1().Pods(p.Namespace).Create(ctx, p, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("pre-create pod %d: %v", i, err)
		}
		pods[i] = created
	}

	// Drive the informer to a known state BEFORE timing — otherwise
	// the pod indexer is racing with the fake clientset's watch and
	// PodLister.ListByIndexValue (consulted on every Filter call to
	// subtract pre-allocated resources from each NodeInfo) sees an
	// empty set for the early iterations. That would silently produce
	// fake numbers: every Filter would believe the cluster were
	// pristine and resource pressure across the run would never grow.
	waitForPerfIndexer(t, fp.GetPodLister(), podCount)

	// Warm-up: one filter call against a throwaway pod so any first-
	// use caches (informer, GC tuning) are primed and the actual
	// measurement starts on a steady state.
	_ = fp.Filter(ctx, extenderv1.ExtenderArgs{Pod: pods[0], Nodes: nodeList})

	runtime.GC()
	start := time.Now()
	for i, p := range pods {
		r := fp.Filter(ctx, extenderv1.ExtenderArgs{Pod: p, Nodes: nodeList})
		// Filter signals "no node matched" in TWO different ways:
		//   - r.Error != ""        for internal errors (annotation
		//                           encoding bug, etc.)
		//   - empty r.Nodes/Names  for "all candidates rejected"
		// kube-scheduler treats both as unschedulable; we count both
		// so the perf summary reflects what an operator would see on
		// a saturated cluster.
		if r.Error != "" || filterResultEmpty(r) {
			errs++
		}
		_ = i // keep loop counter visible for debuggers
	}
	total = time.Since(start)
	perPod = total / time.Duration(len(pods))

	// Post-loop SANITY CHECK: confirm the per-pod patches actually
	// landed in the fake clientset AND are visible to the lister. If
	// either is broken, every Filter call after the first saw a
	// pristine cluster and the perf numbers describe an unrealistic
	// scenario. We don't fail the test on a small shortfall (events
	// from the last few patches may still be in flight through the
	// watch channel), but a near-zero count means the wiring is
	// broken and the numbers are bogus.
	verifyPerfResourceVisibility(t, k8sClient, fp.GetPodLister(), podCount-errs)
	return total, perPod, errs
}

// filterResultEmpty reports whether Filter returned no candidate
// node — equivalent to "this pod is unschedulable in this pass."
// The extender API uses two parallel result-shapes (Nodes vs.
// NodeNames) depending on which input shape was supplied; we handle
// both so this works regardless of which path the test took.
func filterResultEmpty(r *extenderv1.ExtenderFilterResult) bool {
	if r.Nodes != nil && len(r.Nodes.Items) > 0 {
		return false
	}
	if r.NodeNames != nil && len(*r.NodeNames) > 0 {
		return false
	}
	return true
}

// waitForPerfIndexer blocks until the pod informer's indexer has
// surfaced all `want` pre-created pods, or fails the test after a
// generous timeout. With a fake clientset the Create→watch→indexer
// path is fast but still async; running Filter against an unpopulated
// indexer would skip resource accounting entirely.
func waitForPerfIndexer(t *testing.T, podLister interface {
	List(labels.Selector) ([]*corev1.Pod, error)
}, want int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		got, err := podLister.List(labels.Everything())
		if err == nil && len(got) >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pod informer never reached %d pods (last err=%v, got=%d)", want, err, len(got))
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// verifyPerfResourceVisibility cross-checks two things after the
// timed Filter loop:
//
//  1. The fake clientset's tracker holds at least `expectAllocated`
//     pods carrying the PodVGPUPreAllocAnnotation. If this is far
//     below expectAllocated, PatchPodPreAllocatedMetadata never
//     actually wrote to the tracker — every Filter call saw a
//     pristine cluster and the perf numbers describe an unrealistic
//     scenario where nodes never fill up.
//
//  2. The pod lister surfaces the same set. Coverage on the
//     Mutation+MutationCache path used by neighbouring Filter calls
//     within the same run.
//
// We allow a small shortfall (a couple of last-iteration patches may
// still be queued through the watch channel by the time we sample),
// but anything beyond a tiny rounding error is a real bug in test
// wiring and the run's numbers must be discarded.
func verifyPerfResourceVisibility(t *testing.T, k8sClient *fake.Clientset, podLister interface {
	List(labels.Selector) ([]*corev1.Pod, error)
}, expectAllocated int) {
	t.Helper()
	if expectAllocated <= 0 {
		return
	}

	clientPods, err := k8sClient.CoreV1().Pods(namespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list pods from fake client: %v", err)
	}
	clientPatched := 0
	for i := range clientPods.Items {
		if _, ok := clientPods.Items[i].Annotations[util.PodVGPUPreAllocAnnotation]; ok {
			clientPatched++
		}
	}

	listerPods, err := podLister.List(labels.Everything())
	if err != nil {
		t.Fatalf("list pods from podLister: %v", err)
	}
	listerPatched := 0
	for _, p := range listerPods {
		if _, ok := p.Annotations[util.PodPredicateNodeAnnotation]; ok {
			listerPatched++
		}
	}

	// Tolerate up to 5% drift to absorb late watch events; anything
	// more is a wiring failure that invalidates this scenario.
	tolerance := expectAllocated / 20
	if tolerance < 2 {
		tolerance = 2
	}
	if clientPatched < expectAllocated-tolerance {
		t.Fatalf("patched-pod count in fake clientset = %d, want ~%d "+
			"(PatchPodPreAllocatedMetadata didn't land — perf numbers are bogus)",
			clientPatched, expectAllocated)
	}
	if listerPatched < expectAllocated-tolerance {
		t.Fatalf("patched-pod count via podLister = %d, want ~%d "+
			"(MutationCache overlay not visible — neighbour Filter calls saw stale view)",
			listerPatched, expectAllocated)
	}
	t.Logf("resource-view check ok: clientPatched=%d listerPatched=%d expected≈%d",
		clientPatched, listerPatched, expectAllocated)
}

// buildPerfNodes mints `count` synthetic vGPU nodes, all identical in
// shape: 8 GPUs (four 12GB on NUMA 0, four 20GB on NUMA 1),
// deviceSplit=10 ⇒ 80 vGPU slots per node. This matches the standard
// 8-GPU server footprint (DGX/HGX-style) — giving the allocator a
// realistic combinatorial space for 4-vGPU pods.
//
// Each node also carries the GPU topology annotation so the
// link-topology code path actually engages — without it allocateLink
// short-circuits at `if !HasGPUTopology() { return false }` and the
// topology rows of the perf matrix degenerate into the no-topology
// fallback.
//
// Topology mesh (intra-NUMA full NVLink mesh, cross-NUMA via PCIe
// single-switch):
//
//	NUMA 0:                       NUMA 1:
//	      ┌─2NVL─┐                       ┌─2NVL─┐
//	   GPU0 ─── GPU1               GPU4 ─── GPU5
//	    │ ╲    ╱ │                  │ ╲    ╱ │
//	   1NVL 1NVL 1NVL              1NVL 1NVL 1NVL
//	    │ ╱    ╲ │                  │ ╱    ╲ │
//	   GPU2 ─── GPU3               GPU6 ─── GPU7
//	      └─2NVL─┘                       └─2NVL─┘
//
//	          ── all cross-NUMA pairs: P2PLinkSingleSwitch ──
//
// Pairs (0,1), (2,3), (4,5), (6,7) have TwoNVLINKLinks — these are
// the "strong" pairs link-strict will prefer. Every other intra-NUMA
// pair has one NVLink. Cross-NUMA edges are PCIe switch — no NVLink,
// so link-strict cannot span NUMA.
//
// For a 4-vGPU request, link-strict / link succeed within a single
// NUMA (4 fully-connected cards). numa-strict also succeeds (4 cards
// per NUMA). With 80 slots/node a 500-node cluster fits 20×500 = 10k
// 4-vGPU pods before slot saturation.
//
// Names are "perfnode-<i>" zero-padded so lexicographic sort matches
// numeric order — handy when the ByNodeNameAsc tie-breaker fires.
func buildPerfNodes(count int) []corev1.Node {
	nodeConfig := device.NodeConfigInfo{
		DeviceSplit:   10,
		CoresScaling:  1,
		MemoryFactor:  1,
		MemoryScaling: 1,
	}
	encodedCfg, _ := nodeConfig.Encode()
	encodedTopo := buildPerfTopologyAnnotation()

	nodes := make([]corev1.Node, count)
	for i := 0; i < count; i++ {
		devs := device.NodeDeviceInfo{
			{Id: 0, Uuid: "GPU-" + uuid.NewString(), Core: util.HundredCore, Memory: 12288,
				Type: "NVIDIA RTX3080Ti", Number: 10, Numa: 0, Capability: 8.9, Healthy: true},
			{Id: 1, Uuid: "GPU-" + uuid.NewString(), Core: util.HundredCore, Memory: 12288,
				Type: "NVIDIA RTX3080Ti", Number: 10, Numa: 0, Capability: 8.9, Healthy: true},
			{Id: 2, Uuid: "GPU-" + uuid.NewString(), Core: util.HundredCore, Memory: 12288,
				Type: "NVIDIA RTX3080Ti", Number: 10, Numa: 0, Capability: 8.9, Healthy: true},
			{Id: 3, Uuid: "GPU-" + uuid.NewString(), Core: util.HundredCore, Memory: 12288,
				Type: "NVIDIA RTX3080Ti", Number: 10, Numa: 0, Capability: 8.9, Healthy: true},
			{Id: 4, Uuid: "GPU-" + uuid.NewString(), Core: util.HundredCore, Memory: 20480,
				Type: "NVIDIA RTX4080Ti", Number: 10, Numa: 1, Capability: 8.9, Healthy: true},
			{Id: 5, Uuid: "GPU-" + uuid.NewString(), Core: util.HundredCore, Memory: 20480,
				Type: "NVIDIA RTX4080Ti", Number: 10, Numa: 1, Capability: 8.9, Healthy: true},
			{Id: 6, Uuid: "GPU-" + uuid.NewString(), Core: util.HundredCore, Memory: 20480,
				Type: "NVIDIA RTX4080Ti", Number: 10, Numa: 1, Capability: 8.9, Healthy: true},
			{Id: 7, Uuid: "GPU-" + uuid.NewString(), Core: util.HundredCore, Memory: 20480,
				Type: "NVIDIA RTX4080Ti", Number: 10, Numa: 1, Capability: 8.9, Healthy: true},
		}
		registerNode, _ := devs.Encode()
		nodes[i] = corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf("perfnode-%06d", i),
				Annotations: map[string]string{
					util.NodeDeviceRegisterAnnotation: registerNode,
					util.NodeConfigInfoAnnotation:     encodedCfg,
					util.NodeDeviceTopologyAnnotation: encodedTopo,
				},
			},
			Status: corev1.NodeStatus{
				Capacity: corev1.ResourceList{
					corev1.ResourceName(util.VGPUNumberResourceName): resource.MustParse("80"),
				},
				Allocatable: corev1.ResourceList{
					corev1.ResourceName(util.VGPUNumberResourceName): resource.MustParse("80"),
				},
			},
		}
	}
	return nodes
}

// buildPerfTopologyAnnotation hand-builds the 8-GPU mesh described on
// buildPerfNodes. NodeTopologyInfo is the raw shape
// NewNodeDeviceGatherInfo parses; we encode it once at startup since
// every test node is identical.
//
// Encoding goes through NodeTopologyInfo.Encode (JSON); the
// links.P2PLinkType enum serialises as its uint value, which is what
// the parser expects on the read side.
func buildPerfTopologyAnnotation() string {
	strong := []links.P2PLinkType{links.TwoNVLINKLinks}
	weak := []links.P2PLinkType{links.SingleNVLINKLink}
	sw := []links.P2PLinkType{links.P2PLinkSingleSwitch}
	// Within each NUMA group, pairs (0,1)(2,3) and (4,5)(6,7) get
	// TwoNVLINKLinks; remaining intra-NUMA edges get one NVLink. All
	// cross-NUMA edges go via P2PLinkSingleSwitch.
	topo := device.NodeTopologyInfo{
		// NUMA 0
		{Index: 0, Links: map[int][]links.P2PLinkType{
			1: strong, 2: weak, 3: weak,
			4: sw, 5: sw, 6: sw, 7: sw,
		}},
		{Index: 1, Links: map[int][]links.P2PLinkType{
			0: strong, 2: weak, 3: weak,
			4: sw, 5: sw, 6: sw, 7: sw,
		}},
		{Index: 2, Links: map[int][]links.P2PLinkType{
			0: weak, 1: weak, 3: strong,
			4: sw, 5: sw, 6: sw, 7: sw,
		}},
		{Index: 3, Links: map[int][]links.P2PLinkType{
			0: weak, 1: weak, 2: strong,
			4: sw, 5: sw, 6: sw, 7: sw,
		}},
		// NUMA 1
		{Index: 4, Links: map[int][]links.P2PLinkType{
			0: sw, 1: sw, 2: sw, 3: sw,
			5: strong, 6: weak, 7: weak,
		}},
		{Index: 5, Links: map[int][]links.P2PLinkType{
			0: sw, 1: sw, 2: sw, 3: sw,
			4: strong, 6: weak, 7: weak,
		}},
		{Index: 6, Links: map[int][]links.P2PLinkType{
			0: sw, 1: sw, 2: sw, 3: sw,
			4: weak, 5: weak, 7: strong,
		}},
		{Index: 7, Links: map[int][]links.P2PLinkType{
			0: sw, 1: sw, 2: sw, 3: sw,
			4: weak, 5: weak, 6: strong,
		}},
	}
	encoded, err := topo.Encode()
	if err != nil {
		panic(fmt.Sprintf("encode perf topology annotation: %v", err))
	}
	return encoded
}

// buildPerfPod mints a multi-vGPU pod: one container, 4 vGPUs,
// 20 cores / 2048 MB per vGPU. Four vGPUs is the sweet spot for
// exercising the topology code path on 8-GPU nodes:
//
//   - allocateByTopologyMode bypasses the dispatch when needNumber <= 1
//     and just returns buildClaims(deviceStore[:needNumber], ...).
//     One-vGPU pods never see allocateLink / allocateNUMA at all.
//   - CanNotCrossNumaNode similarly requires gpuNumber > 1.
//   - With 4 vGPUs out of 8 cards, link allocation has real combinatorial
//     work: C(8,4)=70 candidate sets, each with a topology score derived
//     from the per-edge link-type table. The compose-path (AllocateLinkTopK
//   - per-K device-policy sort) cost ratio over the fast-path
//     (single bestEffort call) is visible at this size, where it wasn't
//     at 2-vGPU/4-GPU.
//   - 4 vGPUs also matches the worst-case numa-strict ask: must fit all 4
//     on one NUMA. With 4 cards per NUMA, that succeeds — exercising the
//     CanNotCrossNumaNode check on a real boundary rather than trivially.
//
// Resource budget: 4 vGPUs × 20 cores = 80 core-units demanded across
// 4 different cards (20% per card). A node has 8 cards × 100 cores =
// 800 total, so core is not the binding constraint. Slot count (80 per
// node) is: 80/4 = 20 pods/node. A 500-node cluster fits 10k 4-vGPU
// pods before slot saturation.
//
// pv.nodePolicy / pv.devicePolicy / pv.topology are written onto the
// pod annotations when non-empty; an empty value means "leave the
// annotation off" (NOT "set it to empty string"), so the pod hits the
// no-policy code path rather than the "unsupported policy ” " event.
func buildPerfPod(i int, pv perfPolicy) *corev1.Pod {
	annos := map[string]string{}
	if pv.nodePolicy != "" {
		annos[util.NodeSchedulerPolicyAnnotation] = pv.nodePolicy
	}
	if pv.devicePolicy != "" {
		annos[util.DeviceSchedulerPolicyAnnotation] = pv.devicePolicy
	}
	if pv.topology != "" {
		annos[util.DeviceTopologyModeAnnotation] = pv.topology
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            fmt.Sprintf("perfpod-%07d", i),
			Namespace:       namespace,
			UID:             k8stypes.UID(uuid.NewString()),
			Annotations:     annos,
			ResourceVersion: "1",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "c0",
				Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{
						corev1.ResourceName(util.VGPUNumberResourceName): resource.MustParse("4"),
						corev1.ResourceName(util.VGPUCoreResourceName):   resource.MustParse("20"),
						corev1.ResourceName(util.VGPUMemoryResourceName): resource.MustParse("2048"),
					},
				},
			}},
		},
	}
}
