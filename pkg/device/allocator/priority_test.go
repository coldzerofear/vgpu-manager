package allocator

import (
	"fmt"
	"testing"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func Test_NodePriority(t *testing.T) {
	t.Skip("Just trying, no need to test")
	nodeInfoList := make([]*device.NodeInfo, 500000)
	for i := 0; i < 500000; i++ {
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf("testNode%d", i),
			},
		}
		nodeInfoList[i] = device.NewFakeNodeInfo(node, false, []*device.Device{
			device.NewFakeDevice(0, 1, 10, 80, 100, 40000, 80000, 0),
			device.NewFakeDevice(1, 2, 10, 70, 100, 40000, 80000, 0),
			device.NewFakeDevice(2, 3, 10, 60, 100, 40000, 80000, 0),
			device.NewFakeDevice(3, 4, 10, 50, 100, 40000, 80000, 0),
			device.NewFakeDevice(4, 5, 10, 40, 100, 40000, 80000, 1),
			device.NewFakeDevice(5, 6, 10, 30, 100, 40000, 80000, 1),
			device.NewFakeDevice(6, 7, 10, 20, 100, 40000, 80000, 1),
			device.NewFakeDevice(7, 8, 10, 10, 100, 40000, 80000, 1),
		})
	}
	start := time.Now()
	NewNodeBinpackPriority(false).Sort(nodeInfoList)
	since := time.Since(start)
	fmt.Printf("call NewNodeBinpackPriority took %d Milliseconds\n", since.Milliseconds())
	start = time.Now()
	NewNodeSpreadPriority(false).Sort(nodeInfoList)
	since = time.Since(start)
	fmt.Printf("call NewNodeSpreadPriority took %d Milliseconds\n", since.Milliseconds())
}

func Test_NodeSorting(t *testing.T) {
	nodeA := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "nodeA",
		},
	}
	nodeB := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "nodeB",
		},
	}
	nodeC := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "nodeC",
		},
	}
	nodeD := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "nodeD",
		},
	}
	nodes := []*device.NodeInfo{
		device.NewFakeNodeInfo(nodeA, false, []*device.Device{
			device.NewFakeDevice(0, 1, 0, 0, 100, 0, 80000, 0),
			device.NewFakeDevice(1, 2, 0, 0, 100, 0, 80000, 0),
			device.NewFakeDevice(2, 3, 0, 0, 100, 0, 80000, 0),
			device.NewFakeDevice(3, 4, 0, 0, 100, 0, 80000, 0),
		}),
		device.NewFakeNodeInfo(nodeB, true, []*device.Device{
			device.NewFakeDevice(0, 1, 10, 50, 100, 40000, 80000, 0),
			device.NewFakeDevice(1, 2, 10, 50, 100, 40000, 80000, 0),
			device.NewFakeDevice(2, 3, 10, 50, 100, 40000, 80000, 0),
			device.NewFakeDevice(3, 4, 10, 50, 100, 40000, 80000, 0),
		}),
		device.NewFakeNodeInfo(nodeC, true, []*device.Device{
			device.NewFakeDevice(0, 1, 10, 80, 100, 50000, 80000, 0),
			device.NewFakeDevice(1, 2, 10, 80, 100, 50000, 80000, 0),
			device.NewFakeDevice(2, 3, 10, 80, 100, 50000, 80000, 0),
			device.NewFakeDevice(3, 4, 10, 80, 100, 50000, 80000, 0),
		}),
		device.NewFakeNodeInfo(nodeD, true, []*device.Device{
			device.NewFakeDevice(0, 1, 10, 80, 100, 50000, 80000, 0),
			device.NewFakeDevice(1, 2, 10, 80, 100, 50000, 80000, 0),
			device.NewFakeDevice(2, 3, 10, 80, 100, 50000, 80000, 0),
			device.NewFakeDevice(3, 4, 10, 80, 100, 50000, 80000, 0),
		}),
	}

	NewNodeBinpackPriority(true).Sort(nodes)
	wantNodeNames := []string{nodeC.Name, nodeD.Name, nodeB.Name, nodeA.Name}
	binpackNodeNames := make([]string, len(nodes))
	for i, node := range nodes {
		binpackNodeNames[i] = node.GetName()
	}
	assert.Equal(t, wantNodeNames, binpackNodeNames)

	NewNodeSpreadPriority(true).Sort(nodes)
	wantNodeNames = []string{nodeB.Name, nodeC.Name, nodeD.Name, nodeA.Name}
	spreadNodeNames := make([]string, len(nodes))
	for i, node := range nodes {
		spreadNodeNames[i] = node.GetName()
	}
	assert.Equal(t, wantNodeNames, spreadNodeNames)

	NewNodeSpreadPriority(false).Sort(nodes)
	wantNodeNames = []string{nodeA.Name, nodeB.Name, nodeC.Name, nodeD.Name}
	spreadNodeNames = make([]string, len(nodes))
	for i, node := range nodes {
		spreadNodeNames[i] = node.GetName()
	}
	assert.Equal(t, wantNodeNames, spreadNodeNames)
}
