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
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
	extenderv1 "k8s.io/kube-scheduler/extender/v1"
)

// init force-enables the GPUTopology feature for this test package so
// link-topology rows actually exercise the topology path. SetGPU... is
// guarded by a sync.Once shared with IsGPUTopologyEnabled(); call sites
// elsewhere (and ESPECIALLY the lazy default-false IsGPUTopologyEnabled
// invocation from NewNodeInfo) MUST NOT fire before this init runs, or
// the Once gets consumed with false. Package init() ordering ensures
// that's the case here — test functions can't run before init.
//
// Other tests in this package don't assert on topology behaviour, so
// flipping the global to true for the whole package is safe.
func init() {
	device.SetGPUTopologyEnabled(true)
}

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

type perfPolicy struct {
	name       string
	nodePolicy string // value for util.NodeSchedulerPolicyAnnotation; "" = unset
	topology   string // value for util.DeviceTopologyModeAnnotation;   "" = unset
}

var allPerfPolicies = []perfPolicy{
	{name: "none"},
	{name: "binpack", nodePolicy: "binpack"},
	{name: "spread", nodePolicy: "spread"},
	{name: "binpack+link", nodePolicy: "binpack", topology: "link"},
	{name: "binpack+numa", nodePolicy: "binpack", topology: "numa"},
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
	fp, err := New(k8sClient, factory, recorder, true)
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

	// Warm-up: one filter call against a throwaway pod so any first-
	// use caches (informer, GC tuning) are primed and the actual
	// measurement starts on a steady state.
	_ = fp.Filter(ctx, extenderv1.ExtenderArgs{Pod: pods[0], Nodes: nodeList})

	runtime.GC()
	start := time.Now()
	for i, p := range pods {
		r := fp.Filter(ctx, extenderv1.ExtenderArgs{Pod: p, Nodes: nodeList})
		if r.Error != "" {
			errs++
		}
		_ = i // keep loop counter visible for debuggers
	}
	total = time.Since(start)
	perPod = total / time.Duration(len(pods))
	return total, perPod, errs
}

// buildPerfNodes mints `count` synthetic vGPU nodes, all identical in
// shape (4 GPUs, two 12GB + two 20GB, deviceSplit=10 ⇒ 40 vGPU slots,
// NUMA={0,0,1,1}). Each node also carries the GPU topology annotation
// so the link-topology code path actually engages — without it
// allocateLink short-circuits at `if !HasGPUTopology() { return false }`
// and the topology rows of the perf matrix degenerate into the
// no-topology fallback.
//
// Topology mesh:
//
//	   GPU0 ─NVL─ GPU1       (same NUMA 0, same board pair)
//	    │    ╲   ╱  │
//	   PCIe   ╲ ╱  PCIe       (cross-NUMA via single PCIe switch)
//	    │    ╱ ╲   │
//	   GPU2 ─NVL─ GPU3       (same NUMA 1, same board pair)
//
// That gives binpack/spread a real choice (two NVLink-connected pairs
// vs. four single-switch-only options) and lets link-strict succeed
// for a 2-vGPU request (the connected pair) while numa-strict succeeds
// for the same request (any same-NUMA pair).
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
			{Id: 2, Uuid: "GPU-" + uuid.NewString(), Core: util.HundredCore, Memory: 20480,
				Type: "NVIDIA RTX4080Ti", Number: 10, Numa: 1, Capability: 8.9, Healthy: true},
			{Id: 3, Uuid: "GPU-" + uuid.NewString(), Core: util.HundredCore, Memory: 20480,
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
					corev1.ResourceName(util.VGPUNumberResourceName): resource.MustParse("40"),
				},
				Allocatable: corev1.ResourceList{
					corev1.ResourceName(util.VGPUNumberResourceName): resource.MustParse("40"),
				},
			},
		}
	}
	return nodes
}

// buildPerfTopologyAnnotation hand-builds the 4-GPU mesh described on
// buildPerfNodes. NodeTopologyInfo is the raw shape NewNodeDeviceGatherInfo
// parses; we encode it once at startup since every test node is
// identical.
//
// Encoding goes through NodeTopologyInfo.Encode (JSON); the
// links.P2PLinkType enum serialises as its uint value, which is what
// the parser expects on the read side.
func buildPerfTopologyAnnotation() string {
	pair := []links.P2PLinkType{links.SingleNVLINKLink}
	sw := []links.P2PLinkType{links.P2PLinkSingleSwitch}
	topo := device.NodeTopologyInfo{
		{Index: 0, Links: map[int][]links.P2PLinkType{1: pair, 2: sw, 3: sw}},
		{Index: 1, Links: map[int][]links.P2PLinkType{0: pair, 2: sw, 3: sw}},
		{Index: 2, Links: map[int][]links.P2PLinkType{0: sw, 1: sw, 3: pair}},
		{Index: 3, Links: map[int][]links.P2PLinkType{0: sw, 1: sw, 2: pair}},
	}
	encoded, err := topo.Encode()
	if err != nil {
		panic(fmt.Sprintf("encode perf topology annotation: %v", err))
	}
	return encoded
}

// buildPerfPod mints a small multi-vGPU pod: one container, 2 vGPUs,
// 20 cores / 2048 MB per vGPU. Two vGPUs is the SMALLEST footprint
// that still engages the topology code path:
//
//   - allocateByTopologyMode bypasses the dispatch when needNumber <= 1
//     and just returns buildClaims(deviceStore[:needNumber], ...).
//     One-vGPU pods never see allocateLink / allocateNUMA at all, so
//     measuring "link" or "numa" mode with a 1-vGPU pod is identical
//     to measuring "none".
//   - CanNotCrossNumaNode similarly requires gpuNumber > 1.
//
// Two vGPUs × 20 cores = 40 cores per pod (each vGPU consumes 20 of
// its host card's 100). On a 4-GPU node with 100 cores each, a single
// node can host up to 5 such pods before running out of cores; on a
// 40-vGPU-slot node it can host 20 before running out of slots. Slot
// pressure is the binding constraint, so a 100-node cluster fits ~2000
// pods before nodes start refusing new ones.
//
// pv.nodePolicy / pv.topology are written onto the pod annotations
// when non-empty; an empty value means "leave the annotation off"
// (NOT "set it to empty string"), so the pod hits the no-policy code
// path rather than the "unsupported policy '' " event.
func buildPerfPod(i int, pv perfPolicy) *corev1.Pod {
	annos := map[string]string{}
	if pv.nodePolicy != "" {
		annos[util.NodeSchedulerPolicyAnnotation] = pv.nodePolicy
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
						corev1.ResourceName(util.VGPUNumberResourceName): resource.MustParse("2"),
						corev1.ResourceName(util.VGPUCoreResourceName):   resource.MustParse("20"),
						corev1.ResourceName(util.VGPUMemoryResourceName): resource.MustParse("2048"),
					},
				},
			}},
		},
	}
}
