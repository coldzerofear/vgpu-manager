package filter

import (
	"context"
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
	extenderv1 "k8s.io/kube-scheduler/extender/v1"
)

// driveFilter schedules podCount pods onto nodeCount perf nodes through the real
// Filter path (serial, with per-pod patch so usage accumulates), then returns the
// placements grouped by node and the node objects. Reuses buildPerfNodes/
// buildPerfPod from the perf benchmark.
func driveFilter(t *testing.T, nodeCount, podCount int, pv perfPolicy) (
	byNode map[string][]*corev1.Pod, nodeByName map[string]*corev1.Node, placed int,
) {
	t.Helper()
	k8sClient := fake.NewClientset()
	factory := informers.NewSharedInformerFactory(k8sClient, 0)
	broadcaster := record.NewBroadcaster()
	defer broadcaster.Shutdown()
	recorder := broadcaster.NewRecorder(nil, corev1.EventSource{Component: "scale-ovc"})
	fp, err := New(k8sClient, factory, recorder, true, true)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())

	nodes := buildPerfNodes(nodeCount)
	nodeList := &corev1.NodeList{Items: nodes}
	nodeByName = make(map[string]*corev1.Node, nodeCount)
	for i := range nodes {
		nodeByName[nodes[i].Name] = &nodes[i]
	}

	pods := make([]*corev1.Pod, podCount)
	for i := 0; i < podCount; i++ {
		p := buildPerfPod(i, pv)
		created, err := k8sClient.CoreV1().Pods(p.Namespace).Create(ctx, p, metav1.CreateOptions{})
		require.NoError(t, err)
		pods[i] = created
	}
	waitForPerfIndexer(t, fp.GetPodLister(), podCount)

	for _, p := range pods {
		r := fp.Filter(ctx, extenderv1.ExtenderArgs{Pod: p, Nodes: nodeList})
		if r.Error == "" && !filterResultEmpty(r) {
			placed++
		}
	}

	listerPods, err := fp.GetPodLister().List(labels.Everything())
	require.NoError(t, err)
	byNode = make(map[string][]*corev1.Pod)
	for _, p := range listerPods {
		if n, ok := p.Annotations[util.PodPredicateNodeAnnotation]; ok {
			byNode[n] = append(byNode[n], p)
		}
	}
	return byNode, nodeByName, placed
}

// assertNoDeviceOvercommit rebuilds each node's NodeInfo from its placed pods
// (NodeInfo.AddPodsUsedResources sums every pod's reduced per-GPU footprint) and
// fails if ANY device's allocatable number/cores/memory went negative — the
// definition of over-subscription.
func assertNoDeviceOvercommit(t *testing.T, byNode map[string][]*corev1.Pod, nodeByName map[string]*corev1.Node) {
	t.Helper()
	for nodeName, ps := range byNode {
		ni, err := device.NewNodeInfo(nodeByName[nodeName], device.WithNodePods(ps...))
		require.NoError(t, err)
		for id, d := range ni.GetDeviceMap() {
			require.GreaterOrEqualf(t, d.AllocatableNumber(), 0,
				"node %s GPU %d OVERCOMMIT: number allocatable=%d", nodeName, id, d.AllocatableNumber())
			require.GreaterOrEqualf(t, d.AllocatableCores(), int64(0),
				"node %s GPU %d OVERCOMMIT: cores allocatable=%d", nodeName, id, d.AllocatableCores())
			require.GreaterOrEqualf(t, d.AllocatableMemory(), int64(0),
				"node %s GPU %d OVERCOMMIT: memory allocatable=%d", nodeName, id, d.AllocatableMemory())
		}
	}
}

// Test_FilterScale_NoOvercommit drives the cluster to its cores-bound capacity
// (100 nodes × 10 four-card pods = 1000) under link+binpack and asserts that NO
// device is over-subscribed — the property the perf benchmark's errs=0 alone does
// NOT prove (over-commit is also errs=0).
func Test_FilterScale_NoOvercommit(t *testing.T) {
	const nodeCount, podCount = 100, 1000 // cores-bound saturation
	pv := perfPolicy{name: "link+binpack", nodePolicy: "binpack", devicePolicy: "binpack", topology: "link"}

	byNode, nodeByName, placed := driveFilter(t, nodeCount, podCount, pv)
	t.Logf("placed=%d/%d nodesUsed=%d", placed, podCount, len(byNode))

	require.Equal(t, podCount, placed, "all pods should fit at exact capacity (no spurious rejection)")
	assertNoDeviceOvercommit(t, byNode, nodeByName)

	// At cores-bound saturation each USED GPU should be cores-full (allocatable
	// cores == 0), i.e. packed to the limit but not beyond.
	full := 0
	for nodeName, ps := range byNode {
		ni, _ := device.NewNodeInfo(nodeByName[nodeName], device.WithNodePods(ps...))
		for _, d := range ni.GetDeviceMap() {
			if d.AllocatableCores() == 0 {
				full++
			}
		}
	}
	t.Logf("cores-full GPUs: %d", full)
}

// Test_FilterScale_NodePolicyDistribution verifies node binpack/spread actually
// behave as advertised at scale: binpack concentrates pods onto far fewer nodes
// than spread. Uses a NON-saturating load so the distribution is visible.
func Test_FilterScale_NodePolicyDistribution(t *testing.T) {
	const nodeCount, podCount = 100, 200 // 20 nodes worth of capacity

	binByNode, _, binPlaced := driveFilter(t, nodeCount, podCount,
		perfPolicy{name: "binpack", nodePolicy: "binpack"})
	sprByNode, _, sprPlaced := driveFilter(t, nodeCount, podCount,
		perfPolicy{name: "spread", nodePolicy: "spread"})

	require.Equal(t, podCount, binPlaced)
	require.Equal(t, podCount, sprPlaced)
	t.Logf("binpack nodesUsed=%d  spread nodesUsed=%d (of %d, %d pods)",
		len(binByNode), len(sprByNode), nodeCount, podCount)

	// binpack should pack onto near the minimum (200 pods / 10 per node ≈ 20);
	// spread should fan out across many more nodes.
	require.Lessf(t, len(binByNode), len(sprByNode),
		"binpack (%d nodes) must concentrate more than spread (%d nodes)",
		len(binByNode), len(sprByNode))
	require.LessOrEqualf(t, len(binByNode), 30,
		"binpack should pack ~20 nodes, got %d", len(binByNode))
}
