package allocator

import (
	"fmt"
	"testing"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// makeRequestPod builds a pod whose single container requests `vgpuNumber`
// vGPUs. The pod is the input ApplyTopologyMode reads (via the explicit
// needNumber arg the caller passes in alongside the request) to size the
// fitness comparator's needNumber; for NoneTopology tests pass 0
// (needNumber isn't consulted in that branch, but a benign pod keeps the
// AllocationRequest well-formed).
func makeRequestPod(vgpuNumber int64) *corev1.Pod {
	limits := corev1.ResourceList{}
	if vgpuNumber > 0 {
		limits[corev1.ResourceName(util.VGPUNumberResourceName)] = *resource.NewQuantity(vgpuNumber, resource.DecimalSI)
	}
	return &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:      "c0",
				Resources: corev1.ResourceRequirements{Limits: limits},
			}},
		},
	}
}

// makeSortRequest assembles the AllocationRequest the priority constructors
// now consume. Profile is fixed to UniformProfile so the assertions match
// the pre-Phase-B math; policy / topology / vGPU count vary per test row.
func makeSortRequest(policy util.SchedulerPolicy, topology util.TopologyMode, vgpuNumber int64) AllocationRequest {
	return AllocationRequest{
		Pod:        makeRequestPod(vgpuNumber),
		NodePolicy: policy,
		Topology:   topology,
		Profile:    UniformProfile,
		Max: ContainerNeed{
			Number: int(vgpuNumber),
		},
	}
}

func Test_NodePriority(t *testing.T) {
	t.Skip("Just trying, no need to test")
	nodeInfoList := make([]*device.NodeInfo, 500000)
	for i := 0; i < 500000; i++ {
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf("testNode%d", i),
			},
		}
		nodeInfoList[i] = device.NewFakeNodeInfo(node, false,
			device.NewFakeDevice(0, 1, 10, 80, 100, 40000, 80000, 0),
			device.NewFakeDevice(1, 2, 10, 70, 100, 40000, 80000, 0),
			device.NewFakeDevice(2, 3, 10, 60, 100, 40000, 80000, 0),
			device.NewFakeDevice(3, 4, 10, 50, 100, 40000, 80000, 0),
			device.NewFakeDevice(4, 5, 10, 40, 100, 40000, 80000, 1),
			device.NewFakeDevice(5, 6, 10, 30, 100, 40000, 80000, 1),
			device.NewFakeDevice(6, 7, 10, 20, 100, 40000, 80000, 1),
			device.NewFakeDevice(7, 8, 10, 10, 100, 40000, 80000, 1),
		)
	}
	start := time.Now()
	NewNodePolicyPriority(makeSortRequest(util.BinpackPolicy, util.NoneTopology, 0)).Sort(nodeInfoList)
	since := time.Since(start)
	fmt.Printf("call NewNodePolicyPriority(binpack) took %d Milliseconds\n", since.Milliseconds())
	start = time.Now()
	NewNodePolicyPriority(makeSortRequest(util.SpreadPolicy, util.NoneTopology, 0)).Sort(nodeInfoList)
	since = time.Since(start)
	fmt.Printf("call NewNodePolicyPriority(spread) took %d Milliseconds\n", since.Milliseconds())
}

func Test_NodeSorting(t *testing.T) {
	nodeA := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "nodeA"}}
	nodeB := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "nodeB"}}
	nodeC := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "nodeC"}}
	nodeD := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "nodeD"}}
	nodes := []*device.NodeInfo{
		device.NewFakeNodeInfo(nodeA, false,
			device.NewFakeDevice(0, 1, 0, 0, 100, 0, 80000, 0),
			device.NewFakeDevice(1, 2, 0, 0, 100, 0, 80000, 0),
			device.NewFakeDevice(2, 3, 0, 0, 100, 0, 80000, 0),
			device.NewFakeDevice(3, 4, 0, 0, 100, 0, 80000, 0),
		),
		device.NewFakeNodeInfo(nodeB, true,
			device.NewFakeDevice(0, 1, 10, 50, 100, 40000, 80000, 0),
			device.NewFakeDevice(1, 2, 10, 50, 100, 40000, 80000, 0),
			device.NewFakeDevice(2, 3, 10, 50, 100, 40000, 80000, 0),
			device.NewFakeDevice(3, 4, 10, 50, 100, 40000, 80000, 0),
		),
		device.NewFakeNodeInfo(nodeC, true,
			device.NewFakeDevice(0, 1, 10, 80, 100, 50000, 80000, 0),
			device.NewFakeDevice(1, 2, 10, 80, 100, 50000, 80000, 0),
			device.NewFakeDevice(2, 3, 10, 80, 100, 50000, 80000, 0),
			device.NewFakeDevice(3, 4, 10, 80, 100, 50000, 80000, 0),
		),
		device.NewFakeNodeInfo(nodeD, true,
			device.NewFakeDevice(0, 1, 10, 80, 100, 50000, 80000, 0),
			device.NewFakeDevice(1, 2, 10, 80, 100, 50000, 80000, 0),
			device.NewFakeDevice(2, 3, 10, 80, 100, 50000, 80000, 0),
			device.NewFakeDevice(3, 4, 10, 80, 100, 50000, 80000, 0),
		),
	}

	// Link-topology + binpack: 2-vGPU request, so needNumber=2 →
	// fitness comparator prefers nodes whose max link component is ≥ 2.
	// nodeA has no topology → fitness 0 (last). Binpack score then orders
	// the topology-capable nodes by highest utilisation first.
	NewNodePolicyPriority(makeSortRequest(util.BinpackPolicy, util.LinkTopology, 2)).Sort(nodes)
	wantNodeNames := []string{nodeC.Name, nodeD.Name, nodeB.Name, nodeA.Name}
	binpackNodeNames := make([]string, len(nodes))
	for i, node := range nodes {
		binpackNodeNames[i] = node.GetName()
	}
	assert.Equal(t, wantNodeNames, binpackNodeNames)

	NewNodePolicyPriority(makeSortRequest(util.SpreadPolicy, util.LinkTopology, 2)).Sort(nodes)
	wantNodeNames = []string{nodeB.Name, nodeC.Name, nodeD.Name, nodeA.Name}
	spreadNodeNames := make([]string, len(nodes))
	for i, node := range nodes {
		spreadNodeNames[i] = node.GetName()
	}
	assert.Equal(t, wantNodeNames, spreadNodeNames)

	// NoneTopology spread: fitness comparator is a no-op, falls back to
	// pure spread ordering — least-used (nodeA, all zero) first.
	NewNodePolicyPriority(makeSortRequest(util.SpreadPolicy, util.NoneTopology, 0)).Sort(nodes)
	wantNodeNames = []string{nodeA.Name, nodeB.Name, nodeC.Name, nodeD.Name}
	spreadNodeNames = make([]string, len(nodes))
	for i, node := range nodes {
		spreadNodeNames[i] = node.GetName()
	}
	assert.Equal(t, wantNodeNames, spreadNodeNames)
}
