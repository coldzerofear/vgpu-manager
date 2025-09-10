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

	filterPredicate, err := New(k8sClient, factory, recorder, false)
	if err != nil {
		t.Fatalf("failed to create new filterPredicate due to %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())

	var nodeList []corev1.Node
	nodeGPUMap := make(map[string]device.NodeDeviceInfo)
	nodeConfig := device.NodeConfigInfo{
		DeviceSplit:   10,
		CoresScaling:  1,
		MemoryFactor:  1,
		MemoryScaling: 1,
	}
	encodeNodeConfig, _ := nodeConfig.Encode()
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
					util.NodeConfigInfoAnnotation:      encodeNodeConfig,
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

func Test_NodeFilter(t *testing.T) {
	filterPredicate := gpuFilter{}
	oldTimestamp, err := metav1.Time{Time: time.Unix(0, 0)}.MarshalText()
	if err != nil {
		t.Fatal(err)
	}
	timestamp, err := metav1.NowMicro().MarshalText()
	if err != nil {
		t.Fatal(err)
	}
	testCases := []struct {
		name  string
		pod   *corev1.Pod
		nodes []corev1.Node
		// result
		filterNodes    []corev1.Node
		failedNodesMap extenderv1.FailedNodesMap
	}{
		{
			name: "example1, no heartbeat timestamp",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{},
					Name:        "testpod",
					Namespace:   namespace,
				},
			},
			nodes: []corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "testnode",
						Annotations: map[string]string{
							util.NodeDeviceHeartbeatAnnotation: "",
						},
					},
				},
			},
			filterNodes: []corev1.Node{},
			failedNodesMap: map[string]string{
				"testnode": "node without device heartbeat",
			},
		},
		{
			name: "example2, wrong heartbeat timestamp",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{},
					Name:        "testpod",
					Namespace:   namespace,
				},
			},
			nodes: []corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "testnode",
						Annotations: map[string]string{
							util.NodeDeviceHeartbeatAnnotation: "xxxxx",
						},
					},
				},
			},
			filterNodes: []corev1.Node{},
			failedNodesMap: map[string]string{
				"testnode": "node with incorrect heartbeat timestamp",
			},
		},
		{
			name: "example3, timeout heartbeat timestamp",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{},
					Name:        "testpod",
					Namespace:   namespace,
				},
			},
			nodes: []corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "testnode",
						Annotations: map[string]string{
							util.NodeDeviceHeartbeatAnnotation: string(oldTimestamp),
						},
					},
				},
			},
			filterNodes: []corev1.Node{},
			failedNodesMap: map[string]string{
				"testnode": "node with heartbeat timeout",
			},
		},
		{
			name: "example4, no node config info",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{},
					Name:        "testpod",
					Namespace:   namespace,
				},
			},
			nodes: []corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "testnode",
						Annotations: map[string]string{
							util.NodeDeviceHeartbeatAnnotation: string(timestamp),
							util.NodeConfigInfoAnnotation:      "",
						},
					},
				},
			},
			filterNodes: []corev1.Node{},
			failedNodesMap: map[string]string{
				"testnode": "node with empty configuration information",
			},
		},
		{
			name: "example5, incorrect node config info",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{},
					Name:        "testpod",
					Namespace:   namespace,
				},
			},
			nodes: []corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "testnode",
						Annotations: map[string]string{
							util.NodeDeviceHeartbeatAnnotation: string(timestamp),
							util.NodeConfigInfoAnnotation:      "xxxxxxxxx",
						},
					},
				},
			},
			filterNodes: []corev1.Node{},
			failedNodesMap: map[string]string{
				"testnode": "node with incorrect configuration information",
			},
		},
		{
			name: "example6, no GPU device",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{},
					Name:        "testpod",
					Namespace:   namespace,
				},
			},
			nodes: []corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "testnode",
						Annotations: map[string]string{
							util.NodeDeviceHeartbeatAnnotation: string(timestamp),
							util.NodeConfigInfoAnnotation: func() string {
								config := device.NodeConfigInfo{}
								encode, _ := config.Encode()
								return encode
							}(),
						},
					},
				},
			},
			filterNodes: []corev1.Node{},
			failedNodesMap: map[string]string{
				"testnode": "node without GPU device",
			},
		},
		{
			name: "example7, incorrect GPU memory factor",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{},
					Name:        "testpod",
					Namespace:   namespace,
				},
			},
			nodes: []corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "testnode",
						Annotations: map[string]string{
							util.NodeDeviceHeartbeatAnnotation: string(timestamp),
							util.NodeConfigInfoAnnotation: func() string {
								config := device.NodeConfigInfo{DeviceSplit: 1}
								encode, _ := config.Encode()
								return encode
							}(),
						},
					},
				},
			},
			filterNodes: []corev1.Node{},
			failedNodesMap: map[string]string{
				"testnode": "node with incorrect GPU memory factor",
			},
		},
		{
			name: "example8, no virtual memory nodes",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						util.MemorySchedulerPolicyAnnotation: string(util.VirtualMemoryPolicy),
					},
					Name:      "testpod",
					Namespace: namespace,
				},
			},
			nodes: []corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "testnode",
						Annotations: map[string]string{
							util.NodeDeviceHeartbeatAnnotation: string(timestamp),
							util.NodeConfigInfoAnnotation: func() string {
								config := device.NodeConfigInfo{
									DeviceSplit:   1,
									MemoryFactor:  1,
									MemoryScaling: 1,
								}
								encode, _ := config.Encode()
								return encode
							}(),
						},
					},
				},
			},
			filterNodes: []corev1.Node{},
			failedNodesMap: map[string]string{
				"testnode": "node GPU use physical memory",
			},
		},
		{
			name: "example9, no physical memory nodes",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						util.MemorySchedulerPolicyAnnotation: string(util.PhysicalMemoryPolicy),
					},
					Name:      "testpod",
					Namespace: namespace,
				},
			},
			nodes: []corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "testnode",
						Annotations: map[string]string{
							util.NodeDeviceHeartbeatAnnotation: string(timestamp),
							util.NodeConfigInfoAnnotation: func() string {
								config := device.NodeConfigInfo{
									DeviceSplit:   1,
									MemoryFactor:  1,
									MemoryScaling: 2,
								}
								encode, _ := config.Encode()
								return encode
							}(),
						},
					},
				},
			},
			filterNodes: []corev1.Node{},
			failedNodesMap: map[string]string{
				"testnode": "node GPU use virtual memory",
			},
		},
		{
			name: "example10, success",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{},
					Name:        "testpod",
					Namespace:   namespace,
				},
			},
			nodes: []corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "testnode",
						Annotations: map[string]string{
							util.NodeDeviceHeartbeatAnnotation: string(timestamp),
							util.NodeConfigInfoAnnotation: func() string {
								config := device.NodeConfigInfo{
									DeviceSplit:   1,
									CoresScaling:  1,
									MemoryFactor:  1,
									MemoryScaling: 1,
								}
								encode, _ := config.Encode()
								return encode
							}(),
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
							util.NodeConfigInfoAnnotation: func() string {
								config := device.NodeConfigInfo{
									DeviceSplit:   1,
									CoresScaling:  1,
									MemoryFactor:  1,
									MemoryScaling: 1,
								}
								encode, _ := config.Encode()
								return encode
							}(),
						},
					},
				},
			},
			failedNodesMap: map[string]string{},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			filterNodes, failedNodesMap, _ := filterPredicate.nodeFilter(testCase.pod, testCase.nodes)
			assert.Equal(t, testCase.filterNodes, filterNodes)
			assert.Equal(t, testCase.failedNodesMap, failedNodesMap)
		})
	}
}
