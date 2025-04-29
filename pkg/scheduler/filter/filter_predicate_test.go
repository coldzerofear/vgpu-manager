package filter

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
	typedv1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"
	extenderv1 "k8s.io/kube-scheduler/extender/v1"
)

const (
	namespace = "test-ns"
)

func Test_DeviceFilter(t *testing.T) {
	k8sClient := fake.NewSimpleClientset()
	factory := informers.NewSharedInformerFactory(k8sClient, 0)
	broadcaster := record.NewBroadcaster()
	broadcaster.StartRecordingToSink(&typedv1.EventSinkImpl{Interface: k8sClient.CoreV1().Events("")})
	recorder := broadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "test"})

	filterPredicate, err := New(k8sClient, factory, recorder)
	if err != nil {
		t.Fatalf("failed to create new filterPredicate due to %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())

	var nodeList []corev1.Node
	nodeGPUMap := make(map[string]device.NodeDeviceInfo)
	for i := 0; i < 4; i++ {
		nodeGPUInfos := device.NodeDeviceInfo{{
			Id:         0,
			Uuid:       "GPU-" + uuid.New().String(),
			Core:       util.HundredCore,
			Memory:     12288,
			Type:       "NVIDIA RTX3080Ti",
			Mig:        false,
			Number:     10,
			Numa:       0,
			Capability: 8.9,
			Healthy:    true,
		}, {
			Id:         1,
			Uuid:       "GPU-" + uuid.New().String(),
			Core:       util.HundredCore,
			Memory:     12288,
			Type:       "NVIDIA RTX3080Ti",
			Mig:        false,
			Number:     10,
			Numa:       0,
			Capability: 8.9,
			Healthy:    true,
		}, {
			Id:         2,
			Uuid:       "GPU-" + uuid.New().String(),
			Core:       util.HundredCore,
			Memory:     20480,
			Type:       "NVIDIA RTX4080Ti",
			Mig:        false,
			Number:     10,
			Numa:       1,
			Capability: 8.9,
			Healthy:    true,
		}, {
			Id:         3,
			Uuid:       "GPU-" + uuid.New().String(),
			Core:       util.HundredCore,
			Memory:     20480,
			Type:       "NVIDIA RTX4080Ti",
			Mig:        false,
			Number:     10,
			Numa:       1,
			Capability: 8.9,
			Healthy:    true,
		}}
		registerNode, _ := nodeGPUInfos.Encode()
		heartbateTime, _ := metav1.NowMicro().MarshalText()
		node := corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "testnode" + strconv.Itoa(i),
				Labels: map[string]string{},
				Annotations: map[string]string{
					util.NodeDeviceHeartbeatAnnotation: string(heartbateTime),
					util.NodeDeviceRegisterAnnotation:  registerNode,
					util.DeviceMemoryFactorAnnotation:  "1",
				},
			},
			Status: corev1.NodeStatus{
				Capacity: corev1.ResourceList{
					util.VGPUNumberResourceName: resource.MustParse("10"),
				},
				Allocatable: corev1.ResourceList{
					util.VGPUNumberResourceName: resource.MustParse("10"),
				},
			},
		}
		nodeGPUMap[node.Name] = nodeGPUInfos
		nodeList = append(nodeList, node)
	}
	podUID := k8stypes.UID(uuid.NewString())
	testCases := []struct {
		name        string
		uid         *k8stypes.UID
		annotations map[string]string
		containers  []corev1.Container
		// result
		nodeName string
		err      error
	}{
		{
			name:        "example1: single container, single device",
			annotations: map[string]string{},
			containers: []corev1.Container{
				{
					Name: "cont1",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							util.VGPUNumberResourceName: resource.MustParse(fmt.Sprintf("%d", 1)),
							util.VGPUCoreResourceName:   resource.MustParse(fmt.Sprintf("%d", 0)),
							util.VGPUMemoryResourceName: resource.MustParse(fmt.Sprintf("%d", 2048)),
						},
					},
				},
			},
			nodeName: nodeList[0].Name,
			err:      nil,
		}, {
			name:        "example2: single container, request core exceeding limits",
			annotations: map[string]string{},
			containers: []corev1.Container{
				{
					Name: "cont1",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							util.VGPUNumberResourceName: resource.MustParse(fmt.Sprintf("%d", 1)),
							util.VGPUCoreResourceName:   resource.MustParse(fmt.Sprintf("%d", 101)),
							//util.VGPUMemoryResourceName: resource.MustParse(fmt.Sprintf("%d", 2048)),
						},
					},
				},
			},
			nodeName: "",
			err:      fmt.Errorf("container cont1 requests vGPU core exceeding limit"),
		}, {
			name:        "example3: single container, request number exceeding limits",
			annotations: map[string]string{},
			containers: []corev1.Container{
				{
					Name: "cont1",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							util.VGPUNumberResourceName: resource.MustParse(fmt.Sprintf("%d", 17)),
							//util.VGPUCoreResourceName:   resource.MustParse(fmt.Sprintf("%d", 101)),
							//util.VGPUMemoryResourceName: resource.MustParse(fmt.Sprintf("%d", 2048)),
						},
					},
				},
			},
			nodeName: "",
			err:      fmt.Errorf("container cont1 requests vGPU number exceeding limit"),
		}, {
			name: "example4: single container, scheduled",
			uid:  &podUID,
			annotations: map[string]string{
				util.PodPredicateNodeAnnotation: nodeList[1].Name,
				util.PodVGPUPreAllocAnnotation: fmt.Sprintf("cont1[0_%s_10_2048]",
					nodeGPUMap[nodeList[1].Name][0].Uuid),
			},
			containers: []corev1.Container{
				{
					Name: "cont1",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							util.VGPUNumberResourceName: resource.MustParse(fmt.Sprintf("%d", 1)),
							util.VGPUCoreResourceName:   resource.MustParse(fmt.Sprintf("%d", 10)),
							util.VGPUMemoryResourceName: resource.MustParse(fmt.Sprintf("%d", 2048)),
						},
					},
				},
			},
			nodeName: "",
			err:      fmt.Errorf("pod %s had been predicated", podUID),
		}, {
			name: "example5: single container, multiple devices",
			containers: []corev1.Container{
				{
					Name: "cont1",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							util.VGPUNumberResourceName: resource.MustParse(fmt.Sprintf("%d", 2)),
							util.VGPUCoreResourceName:   resource.MustParse(fmt.Sprintf("%d", 10)),
							util.VGPUMemoryResourceName: resource.MustParse(fmt.Sprintf("%d", 2048)),
						},
					},
				},
			},
			nodeName: nodeList[0].Name,
			err:      nil,
		}, {
			name: "example6: multiple containers, multiple devices",
			containers: []corev1.Container{
				{
					Name: "cont1",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							util.VGPUNumberResourceName: resource.MustParse(fmt.Sprintf("%d", 2)),
							util.VGPUCoreResourceName:   resource.MustParse(fmt.Sprintf("%d", 10)),
							util.VGPUMemoryResourceName: resource.MustParse(fmt.Sprintf("%d", 2048)),
						},
					},
				}, {
					Name: "cont2",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							util.VGPUNumberResourceName: resource.MustParse(fmt.Sprintf("%d", 2)),
							util.VGPUCoreResourceName:   resource.MustParse(fmt.Sprintf("%d", 10)),
							util.VGPUMemoryResourceName: resource.MustParse(fmt.Sprintf("%d", 2048)),
						},
					},
				},
			},
			nodeName: nodeList[0].Name,
			err:      nil,
		}, {
			name: "example7: multiple containers, exceeds limit",
			containers: []corev1.Container{
				{
					Name: "cont1",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							util.VGPUNumberResourceName: resource.MustParse(fmt.Sprintf("%d", 2)),
							util.VGPUCoreResourceName:   resource.MustParse(fmt.Sprintf("%d", 100)),
							util.VGPUMemoryResourceName: resource.MustParse(fmt.Sprintf("%d", 12288)),
						},
					},
				}, {
					Name: "cont2",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							util.VGPUNumberResourceName: resource.MustParse(fmt.Sprintf("%d", 2)),
							util.VGPUCoreResourceName:   resource.MustParse(fmt.Sprintf("%d", 100)),
							util.VGPUMemoryResourceName: resource.MustParse(fmt.Sprintf("%d", 12288)),
						},
					},
				}, {
					Name: "cont3",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							util.VGPUNumberResourceName: resource.MustParse(fmt.Sprintf("%d", 2)),
							util.VGPUCoreResourceName:   resource.MustParse(fmt.Sprintf("%d", 100)),
							util.VGPUMemoryResourceName: resource.MustParse(fmt.Sprintf("%d", 12288)),
						},
					},
				},
			},
			nodeName: "",
			err:      nil,
		},
	}
	for i, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        fmt.Sprintf("pod-%d", i),
					Namespace:   namespace,
					UID:         k8stypes.UID(uuid.NewString()),
					Annotations: testCase.annotations,
				},
				Spec: corev1.PodSpec{
					Containers: testCase.containers,
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodPending,
				},
			}
			if testCase.uid != nil {
				pod.UID = *testCase.uid
			}
			pod, _ = k8sClient.CoreV1().Pods(namespace).Create(context.Background(), pod, metav1.CreateOptions{})

			// wait for podLister to sync
			time.Sleep(time.Second)

			nodes, _, err := filterPredicate.deviceFilter(pod, nodeList)
			assert.Equal(t, testCase.err, err)
			if err != nil {
				return
			}
			var nodeName string
			if len(nodes) > 0 {
				nodeName = nodes[0].Name
				//t.Fatalf("deviceFilter should return exact one node: %v, failedNodes: %v", nodes, failedNodes)
			}
			assert.Equal(t, testCase.nodeName, nodeName)

			// wait for podLister to sync
			time.Sleep(time.Second)

			if len(nodeName) > 0 {
				// get the latest pod and bind it to the node
				pod, _ = k8sClient.CoreV1().Pods(namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
				pod.Spec.NodeName = nodes[0].Name
				pod.Status.Phase = corev1.PodRunning
				pod, _ = k8sClient.CoreV1().Pods("test-ns").Update(context.Background(), pod, metav1.UpdateOptions{})
			}
		})
	}

}

func Test_HeartbeatFilter(t *testing.T) {
	filterPredicate := gpuFilter{}
	timestamp, err := metav1.NowMicro().MarshalText()
	if err != nil {
		t.Fatal(err)
	}
	testCases := []struct {
		name  string
		nodes []corev1.Node
		// result
		filterNodes    []corev1.Node
		failedNodesMap extenderv1.FailedNodesMap
	}{
		{
			name: "example1",
			nodes: []corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "testnode",
						Annotations: map[string]string{
							util.NodeDeviceHeartbeatAnnotation: "",
							util.DeviceMemoryFactorAnnotation:  "1",
						},
					},
				},
			},
			filterNodes: []corev1.Node{},
			failedNodesMap: map[string]string{
				"testnode": "node has no heartbeat",
			},
		},
		{
			name: "example2",
			nodes: []corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "testnode",
						Annotations: map[string]string{
							util.NodeDeviceHeartbeatAnnotation: "xxxxx",
							util.DeviceMemoryFactorAnnotation:  "1",
						},
					},
				},
			},
			filterNodes: []corev1.Node{},
			failedNodesMap: map[string]string{
				"testnode": "node heartbeat time is not a standard timestamp",
			},
		},
		{
			name: "example3",
			nodes: []corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "testnode",
						Annotations: map[string]string{
							util.NodeDeviceHeartbeatAnnotation: string(timestamp),
							util.DeviceMemoryFactorAnnotation:  "",
						},
					},
				},
			},
			filterNodes: []corev1.Node{},
			failedNodesMap: map[string]string{
				"testnode": "node device memory factor is empty",
			},
		},
		{
			name: "example4",
			nodes: []corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "testnode",
						Annotations: map[string]string{
							util.NodeDeviceHeartbeatAnnotation: string(timestamp),
							util.DeviceMemoryFactorAnnotation:  "-1",
						},
					},
				},
			},
			filterNodes: []corev1.Node{},
			failedNodesMap: map[string]string{
				"testnode": "node device memory factor error",
			},
		},
		{
			name: "example5",
			nodes: []corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "testnode",
						Annotations: map[string]string{
							util.NodeDeviceHeartbeatAnnotation: "2001-01-01T00:00:00.503158522+08:00",
							util.DeviceMemoryFactorAnnotation:  "-1",
						},
					},
				},
			},
			filterNodes: []corev1.Node{},
			failedNodesMap: map[string]string{
				"testnode": "node heartbeat timeout",
			},
		},
		{
			name: "example6",
			nodes: []corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "testnode",
						Annotations: map[string]string{
							util.NodeDeviceHeartbeatAnnotation: string(timestamp),
							util.DeviceMemoryFactorAnnotation:  "10",
						},
					},
				},
			},
			filterNodes: []corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "testnode",
						Annotations: map[string]string{
							util.NodeDeviceHeartbeatAnnotation: string(timestamp),
							util.DeviceMemoryFactorAnnotation:  "10",
						},
					},
				},
			},
			failedNodesMap: map[string]string{},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			filterNodes, failedNodesMap, _ := filterPredicate.heartbeatFilter(nil, testCase.nodes)
			assert.Equal(t, testCase.filterNodes, filterNodes)
			assert.Equal(t, testCase.failedNodesMap, failedNodesMap)
		})
	}
}
