package allocator

import (
	"fmt"
	"testing"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/device"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func Test_NodePriority(t *testing.T) {
	t.Skip("Just trying, no need to test")
	nodeInfoList := make([]*device.NodeInfo, 500000)
	for i := 0; i < 500000; i++ {
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("testNode%d", i),
				Namespace: "default",
			},
		}
		nodeInfoList[i] = device.NewFakeNodeInfo(node, []*device.Device{
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
	NewNodeBinpackPriority().Sort(nodeInfoList)
	since := time.Since(start)
	fmt.Printf("call NewNodeBinpackPriority took %d Milliseconds\n", since.Milliseconds())
	start = time.Now()
	NewNodeSpreadPriority().Sort(nodeInfoList)
	since = time.Since(start)
	fmt.Printf("call NewNodeSpreadPriority took %d Milliseconds\n", since.Milliseconds())
}
