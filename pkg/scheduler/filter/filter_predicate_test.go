package filter

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	jsonpatch "github.com/evanphx/json-patch"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
	typedv1 "k8s.io/client-go/kubernetes/typed/core/v1"
	testing2 "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
	extenderv1 "k8s.io/kube-scheduler/extender/v1"
)

const (
	namespace = "test-ns"
)

func Test_Parallel_Scheduling(t *testing.T) {
	k8sClient := fake.NewClientset()
	k8sClient.PrependReactor("patch", "pods", func(a testing2.Action) (bool, runtime.Object, error) {
		action := a.(testing2.PatchAction)
		obj, err := k8sClient.Tracker().Get(action.GetResource(), action.GetNamespace(), action.GetName())
		if err != nil {
			return true, nil, err
		}
		pod, ok := obj.(*corev1.Pod)
		if !ok {
			return true, nil, fmt.Errorf("unexpected object type: %T", obj)
		}
		pod.ResourceVersion += "1"
		var patchedBytes []byte
		currentBytes, _ := json.Marshal(pod)
		switch action.GetPatchType() {
		case k8stypes.JSONPatchType:
			// JSON Patch (RFC 6902)
			patch, err := jsonpatch.DecodePatch(action.GetPatch())
			if err != nil {
				return true, nil, err
			}
			patchedBytes, err = patch.Apply(currentBytes)
			if err != nil {
				return true, nil, err
			}
		case k8stypes.MergePatchType, k8stypes.StrategicMergePatchType:
			// Merge Patch (RFC 7386)
			patchedBytes, err = jsonpatch.MergePatch(currentBytes, action.GetPatch())
			if err != nil {
				return true, nil, err
			}
		default:
			return true, nil, fmt.Errorf("unsupported patch type: %v", action.GetPatchType())
		}
		newPod := &corev1.Pod{}
		if err = json.Unmarshal(patchedBytes, newPod); err != nil {
			return true, nil, err
		}
		newPod.DeepCopyInto(pod)
		return true, pod, nil
	})
	broadcaster := record.NewBroadcaster()
	broadcaster.StartRecordingToSink(&typedv1.EventSinkImpl{Interface: k8sClient.CoreV1().Events("")})
	recorder := broadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "test"})
	factory := informers.NewSharedInformerFactory(k8sClient, 0)
	filterPredicate, err := New(k8sClient, factory, recorder, true)
	if err != nil {
		t.Fatalf("failed to create new filterPredicate due to %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())

	nodes, _ := buildNodeList()
	nodeList := corev1.NodeList{Items: nodes}

	wg := sync.WaitGroup{}
	errCh := make(chan string, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			pod := &corev1.Pod{}
			pod.Name = fmt.Sprintf("test-pod%d", i)
			pod.Namespace = namespace
			pod.Spec.Containers = []corev1.Container{
				{
					Name: "cont1",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							corev1.ResourceName(util.VGPUNumberResourceName): resource.MustParse(fmt.Sprintf("%d", 1)),
							corev1.ResourceName(util.VGPUCoreResourceName):   resource.MustParse(fmt.Sprintf("%d", 20)),
							corev1.ResourceName(util.VGPUMemoryResourceName): resource.MustParse(fmt.Sprintf("%d", 2048)),
						},
					},
				},
				{
					Name: "cont2",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							corev1.ResourceName(util.VGPUNumberResourceName): resource.MustParse(fmt.Sprintf("%d", 1)),
							corev1.ResourceName(util.VGPUCoreResourceName):   resource.MustParse(fmt.Sprintf("%d", 20)),
							corev1.ResourceName(util.VGPUMemoryResourceName): resource.MustParse(fmt.Sprintf("%d", 2048)),
						},
					},
				},
			}
			pod.UID = k8stypes.UID(uuid.NewString())
			pod.ResourceVersion = "1"
			pod, err := k8sClient.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{})
			if err != nil {
				errCh <- err.Error()
				return
			}
			result := filterPredicate.Filter(ctx, extenderv1.ExtenderArgs{
				Pod:   pod,
				Nodes: &nodeList,
			})
			if result.Error != "" {
				errCh <- result.Error
				return
			}
		}(i)
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatal(err)
	}

	list, err := filterPredicate.podLister.List(labels.Everything())
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range nodes {
		nodeInfo, err := device.NewNodeInfo(&node, list)
		if err != nil {
			t.Fatal(err)
		}

		if nodeInfo.GetUsedCores() > nodeInfo.GetTotalCores() {
			t.Fatalf("The GPU core allocation of node %s exceeds the limit", node.Name)
		}
		if nodeInfo.GetUsedNumber() > nodeInfo.GetTotalNumber() {
			t.Fatalf("The GPU number allocation of node %s exceeds the limit", node.Name)
		}
		if nodeInfo.GetUsedMemory() > nodeInfo.GetTotalMemory() {
			t.Fatalf("The GPU memory allocation of node %s exceeds the limit", node.Name)
		}

		for _, dev := range nodeInfo.GetDeviceMap() {
			if dev.GetUsedCores() > dev.GetTotalCores() {
				t.Fatalf("The GPU %d core allocation of node %s exceeds the limit", dev.GetID(), node.Name)
			}
			if dev.GetUsedNumber() > dev.GetTotalNumber() {
				t.Fatalf("The GPU %d number allocation of node %s exceeds the limit", dev.GetID(), node.Name)
			}
			if dev.GetUsedMemory() > dev.GetTotalMemory() {
				t.Fatalf("The GPU %d memory allocation of node %s exceeds the limit", dev.GetID(), node.Name)
			}
		}
	}
}

func buildNodeList() ([]corev1.Node, map[string]device.NodeDeviceInfo) {
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
		node := corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "testnode" + strconv.Itoa(i),
				Labels: map[string]string{},
				Annotations: map[string]string{
					util.NodeDeviceRegisterAnnotation: registerNode,
					util.NodeConfigInfoAnnotation:     encodeNodeConfig,
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
		nodeGPUMap[node.Name] = nodeGPUInfos
		nodeList = append(nodeList, node)
	}
	return nodeList, nodeGPUMap
}

func Test_DeviceFilter(t *testing.T) {
	k8sClient := fake.NewClientset()
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

	nodeList, nodeGPUMap := buildNodeList()
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
							corev1.ResourceName(util.VGPUNumberResourceName): resource.MustParse(fmt.Sprintf("%d", 1)),
							corev1.ResourceName(util.VGPUCoreResourceName):   resource.MustParse(fmt.Sprintf("%d", 0)),
							corev1.ResourceName(util.VGPUMemoryResourceName): resource.MustParse(fmt.Sprintf("%d", 2048)),
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
							corev1.ResourceName(util.VGPUNumberResourceName): resource.MustParse(fmt.Sprintf("%d", 1)),
							corev1.ResourceName(util.VGPUCoreResourceName):   resource.MustParse(fmt.Sprintf("%d", 101)),
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
							corev1.ResourceName(util.VGPUNumberResourceName): resource.MustParse(fmt.Sprintf("%d", 17)),
							//util.VGPUCoreResourceName:   resource.MustParse(fmt.Sprintf("%d", 101)),
							//util.VGPUMemoryResourceName: resource.MustParse(fmt.Sprintf("%d", 2048)),
						},
					},
				},
			},
			nodeName: "",
			err:      fmt.Errorf("container cont1 requests vGPU number exceeding limit"),
		}, {
			name: "example4: single container, scheduled and with node",
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
							corev1.ResourceName(util.VGPUNumberResourceName): resource.MustParse(fmt.Sprintf("%d", 1)),
							corev1.ResourceName(util.VGPUCoreResourceName):   resource.MustParse(fmt.Sprintf("%d", 10)),
							corev1.ResourceName(util.VGPUMemoryResourceName): resource.MustParse(fmt.Sprintf("%d", 2048)),
						},
					},
				},
			},
			nodeName: nodeList[1].Name,
			err:      nil,
		}, {
			name: "example5: single container, scheduled, no nodes",
			uid:  &podUID,
			annotations: map[string]string{
				util.PodPredicateNodeAnnotation: "noNode",
				util.PodVGPUPreAllocAnnotation: fmt.Sprintf("cont1[0_%s_10_2048]",
					nodeGPUMap[nodeList[1].Name][0].Uuid),
			},
			containers: []corev1.Container{
				{
					Name: "cont1",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							corev1.ResourceName(util.VGPUNumberResourceName): resource.MustParse(fmt.Sprintf("%d", 1)),
							corev1.ResourceName(util.VGPUCoreResourceName):   resource.MustParse(fmt.Sprintf("%d", 10)),
							corev1.ResourceName(util.VGPUMemoryResourceName): resource.MustParse(fmt.Sprintf("%d", 2048)),
						},
					},
				},
			},
			nodeName: "",
			err:      fmt.Errorf("pod %s had been predicated", podUID),
		}, {
			name: "example6: single container, multiple devices",
			containers: []corev1.Container{
				{
					Name: "cont1",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							corev1.ResourceName(util.VGPUNumberResourceName): resource.MustParse(fmt.Sprintf("%d", 2)),
							corev1.ResourceName(util.VGPUCoreResourceName):   resource.MustParse(fmt.Sprintf("%d", 10)),
							corev1.ResourceName(util.VGPUMemoryResourceName): resource.MustParse(fmt.Sprintf("%d", 2048)),
						},
					},
				},
			},
			nodeName: nodeList[0].Name,
			err:      nil,
		}, {
			name: "example7: multiple containers, multiple devices",
			containers: []corev1.Container{
				{
					Name: "cont1",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							corev1.ResourceName(util.VGPUNumberResourceName): resource.MustParse(fmt.Sprintf("%d", 2)),
							corev1.ResourceName(util.VGPUCoreResourceName):   resource.MustParse(fmt.Sprintf("%d", 10)),
							corev1.ResourceName(util.VGPUMemoryResourceName): resource.MustParse(fmt.Sprintf("%d", 2048)),
						},
					},
				}, {
					Name: "cont2",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							corev1.ResourceName(util.VGPUNumberResourceName): resource.MustParse(fmt.Sprintf("%d", 2)),
							corev1.ResourceName(util.VGPUCoreResourceName):   resource.MustParse(fmt.Sprintf("%d", 10)),
							corev1.ResourceName(util.VGPUMemoryResourceName): resource.MustParse(fmt.Sprintf("%d", 2048)),
						},
					},
				},
			},
			nodeName: nodeList[0].Name,
			err:      nil,
		}, {
			name: "example8: multiple containers, exceeds limit",
			containers: []corev1.Container{
				{
					Name: "cont1",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							corev1.ResourceName(util.VGPUNumberResourceName): resource.MustParse(fmt.Sprintf("%d", 2)),
							corev1.ResourceName(util.VGPUCoreResourceName):   resource.MustParse(fmt.Sprintf("%d", 100)),
							corev1.ResourceName(util.VGPUMemoryResourceName): resource.MustParse(fmt.Sprintf("%d", 12288)),
						},
					},
				}, {
					Name: "cont2",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							corev1.ResourceName(util.VGPUNumberResourceName): resource.MustParse(fmt.Sprintf("%d", 2)),
							corev1.ResourceName(util.VGPUCoreResourceName):   resource.MustParse(fmt.Sprintf("%d", 100)),
							corev1.ResourceName(util.VGPUMemoryResourceName): resource.MustParse(fmt.Sprintf("%d", 12288)),
						},
					},
				}, {
					Name: "cont3",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							corev1.ResourceName(util.VGPUNumberResourceName): resource.MustParse(fmt.Sprintf("%d", 2)),
							corev1.ResourceName(util.VGPUCoreResourceName):   resource.MustParse(fmt.Sprintf("%d", 100)),
							corev1.ResourceName(util.VGPUMemoryResourceName): resource.MustParse(fmt.Sprintf("%d", 12288)),
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
					Name:            fmt.Sprintf("pod-%d", i),
					Namespace:       namespace,
					UID:             k8stypes.UID(uuid.NewString()),
					Annotations:     testCase.annotations,
					ResourceVersion: "1",
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
			//time.Sleep(time.Second)

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
			//time.Sleep(time.Second)

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
	nodeStatus := corev1.NodeStatus{
		Allocatable: corev1.ResourceList{
			corev1.ResourceName(util.VGPUNumberResourceName): resource.MustParse("1"),
		},
		Capacity: corev1.ResourceList{
			corev1.ResourceName(util.VGPUNumberResourceName): resource.MustParse("1"),
		},
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
			name: "example1, no node config info",
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
							util.NodeConfigInfoAnnotation:     "",
							util.NodeDeviceRegisterAnnotation: "[]",
						},
					},
					Status: nodeStatus,
				},
			},
			filterNodes: []corev1.Node{},
			failedNodesMap: map[string]string{
				"testnode": NoGPUConfigInfo,
			},
		},
		{
			name: "example2, incorrect node config info",
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
							util.NodeConfigInfoAnnotation:     "xxxxxxxxx",
							util.NodeDeviceRegisterAnnotation: "[]",
						},
					},
					Status: nodeStatus,
				},
			},
			filterNodes: []corev1.Node{},
			failedNodesMap: map[string]string{
				"testnode": IncorrectGPUConfigInfo,
			},
		},
		{
			name: "example3, no GPU device",
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
							util.NodeDeviceRegisterAnnotation: "[]",
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
				"testnode": NoGPUDevice,
			},
		},
		{
			name: "example4, no GPU device registered",
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
							util.NodeConfigInfoAnnotation: func() string {
								config := device.NodeConfigInfo{}
								encode, _ := config.Encode()
								return encode
							}(),
						},
					},
					Status: nodeStatus,
				},
			},
			filterNodes: []corev1.Node{},
			failedNodesMap: map[string]string{
				"testnode": NoGPURegister,
			},
		},
		{
			name: "example5, incorrect GPU memory factor",
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
							util.NodeDeviceRegisterAnnotation: "[]",
							util.NodeConfigInfoAnnotation: func() string {
								config := device.NodeConfigInfo{DeviceSplit: 1}
								encode, _ := config.Encode()
								return encode
							}(),
						},
					},
					Status: nodeStatus,
				},
			},
			filterNodes: []corev1.Node{},
			failedNodesMap: map[string]string{
				"testnode": IncorrectGPUMemFactory,
			},
		},
		{
			name: "example6, no gpu virtual memory nodes",
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
							util.NodeDeviceRegisterAnnotation: "[]",
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
					Status: nodeStatus,
				},
			},
			filterNodes: []corev1.Node{},
			failedNodesMap: map[string]string{
				"testnode": GPUMemTypeMismatch,
			},
		},
		{
			name: "example7, no gpu physical memory nodes",
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
							util.NodeDeviceRegisterAnnotation: "[]",
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
					Status: nodeStatus,
				},
			},
			filterNodes: []corev1.Node{},
			failedNodesMap: map[string]string{
				"testnode": GPUMemTypeMismatch,
			},
		},
		{
			name: "example8, success",
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
							util.NodeDeviceRegisterAnnotation: "[]",
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
					Status: nodeStatus,
				},
			},
			filterNodes: []corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "testnode",
						Annotations: map[string]string{
							util.NodeDeviceRegisterAnnotation: "[]",
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
					Status: nodeStatus,
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
