package filter

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/client"
	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/scheduler/predicate"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	listerv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	extenderv1 "k8s.io/kube-scheduler/extender/v1"
)

type gpuFilter struct {
	kubeClient kubernetes.Interface
	nodeLister listerv1.NodeLister
	podLister  listerv1.PodLister
	recorder   record.EventRecorder
}

const (
	Name = "FilterPredicate"
)

var _ predicate.FilterPredicate = &gpuFilter{}

func New(client kubernetes.Interface, factory informers.SharedInformerFactory, recorder record.EventRecorder) (*gpuFilter, error) {
	podInformer := factory.Core().V1().Pods().Informer()
	nodeInformer := factory.Core().V1().Nodes().Informer()
	podLister := listerv1.NewPodLister(podInformer.GetIndexer())
	nodeLister := listerv1.NewNodeLister(nodeInformer.GetIndexer())

	return &gpuFilter{
		kubeClient: client,
		nodeLister: nodeLister,
		podLister:  podLister,
		recorder:   recorder,
	}, nil
}

func (f *gpuFilter) Name() string {
	return Name
}

type filterFunc func(*corev1.Pod, []corev1.Node) ([]corev1.Node, extenderv1.FailedNodesMap, error)

func (f *gpuFilter) Filter(_ context.Context, args extenderv1.ExtenderArgs) *extenderv1.ExtenderFilterResult {
	klog.V(4).InfoS("FilterNode", "args", args)
	if args.Pod == nil {
		return &extenderv1.ExtenderFilterResult{
			Error: "Pod is empty",
		}
	}
	if !util.IsVGPUResourcePod(args.Pod) {
		return &extenderv1.ExtenderFilterResult{
			Nodes:     args.Nodes,
			NodeNames: args.NodeNames,
		}
	}

	var (
		nodeCache      bool
		filteredNodes  []corev1.Node
		failedNodesMap extenderv1.FailedNodesMap
	)
	switch {
	case args.NodeNames != nil && len(*args.NodeNames) > 0:
		nodeCache = true
		filteredNodes, failedNodesMap = f.getNodesOnCache(*args.NodeNames...)
	case args.Nodes != nil && len(args.Nodes.Items) > 0:
		filteredNodes = args.Nodes.Items
		failedNodesMap = make(extenderv1.FailedNodesMap)
	default:
		return &extenderv1.ExtenderFilterResult{
			Nodes:     args.Nodes,
			NodeNames: args.NodeNames,
			Error:     "No schedulable nodes",
		}
	}

	filters := []filterFunc{
		f.heartbeatFilter,
		f.deviceFilter,
	}

	for i, filter := range filters {
		passedNodes, failedNodes, err := filter(args.Pod, filteredNodes)
		if err != nil {
			klog.Errorf("Filter %d (%T) call failed: %v", i, filter, err)
			return &extenderv1.ExtenderFilterResult{Error: err.Error()}
		}
		// Change the latest node filtering list for the next round of filtering.
		filteredNodes = passedNodes
		for name, reason := range failedNodes {
			failedNodesMap[name] = reason
		}
	}
	var (
		nodes     *corev1.NodeList
		nodeNames *[]string
	)
	if nodeCache {
		temp := make([]string, len(filteredNodes))
		for i, node := range filteredNodes {
			temp[i] = node.Name
		}
		nodeNames = &temp
	} else {
		nodes = &corev1.NodeList{Items: filteredNodes}
	}

	return &extenderv1.ExtenderFilterResult{
		Nodes:       nodes,
		NodeNames:   nodeNames,
		FailedNodes: failedNodesMap,
	}
}

func (f *gpuFilter) getNodesOnCache(nodeNames ...string) ([]corev1.Node, extenderv1.FailedNodesMap) {
	failedNodesMap := make(extenderv1.FailedNodesMap)
	filteredNodes := make([]corev1.Node, 0, len(nodeNames))
	for _, nodeName := range nodeNames {
		if node, err := f.nodeLister.Get(nodeName); err != nil {
			failedNodesMap[nodeName] = fmt.Sprintf("node %s cache failed: %v", nodeName, err)
		} else {
			filteredNodes = append(filteredNodes, *node)
		}
	}
	return filteredNodes, failedNodesMap
}

// TODO heartbeatFilter filter nodes with heartbeat timeout
func (f *gpuFilter) heartbeatFilter(_ *corev1.Pod, nodes []corev1.Node) ([]corev1.Node, extenderv1.FailedNodesMap, error) {
	var (
		filteredNodes  = make([]corev1.Node, 0)          // Successful nodes
		failedNodesMap = make(extenderv1.FailedNodesMap) // Failed nodes
	)
	for _, node := range nodes {
		val, _ := util.HasAnnotation(&node, util.NodeDeviceHeartbeatAnnotation)
		if len(val) == 0 {
			failedNodesMap[node.Name] = "node has no heartbeat"
			continue
		}
		heartbeatTime := metav1.MicroTime{}
		err := heartbeatTime.UnmarshalText([]byte(val))
		if err != nil {
			failedNodesMap[node.Name] = "node heartbeat time is not a standard timestamp"
			continue
		}
		if time.Since(heartbeatTime.Local()) > time.Minute {
			failedNodesMap[node.Name] = "node heartbeat timeout"
			continue
		}
		memFactor, ok := util.HasAnnotation(&node, util.DeviceMemoryFactorAnnotation)
		if !ok || len(memFactor) == 0 {
			failedNodesMap[node.Name] = "node device memory factor is empty"
			continue
		}
		if factor, err := strconv.Atoi(memFactor); err != nil || factor <= 0 {
			failedNodesMap[node.Name] = "node device memory factor error"
			continue
		}
		filteredNodes = append(filteredNodes, node)
	}
	return filteredNodes, failedNodesMap, nil
}

func (f *gpuFilter) CheckDeviceRequest(pod *corev1.Pod) error {
	if err := checkCoreRequest(pod); err != nil {
		f.recorder.Event(pod, corev1.EventTypeWarning, "ResourceError", err.Error())
		return err
	}
	if err := checkNumberRequest(pod); err != nil {
		f.recorder.Event(pod, corev1.EventTypeWarning, "ResourceError", err.Error())
		return err
	}
	return nil
}

func checkNumberRequest(pod *corev1.Pod) error {
	for i, c := range pod.Spec.Containers {
		number := util.GetResourceOfContainer(&pod.Spec.Containers[i], util.VGPUNumberResourceName)
		if number > util.MaxDeviceNumber {
			return fmt.Errorf("container %s requests vGPU number exceeding limit", c.Name)
		}
	}
	return nil
}

func checkCoreRequest(pod *corev1.Pod) error {
	for i, c := range pod.Spec.Containers {
		core := util.GetResourceOfContainer(&pod.Spec.Containers[i], util.VGPUCoreResourceName)
		if core > util.HundredCore {
			return fmt.Errorf("container %s requests vGPU core exceeding limit", c.Name)
		}
	}
	return nil
}

func IsScheduled(pod *corev1.Pod) bool {
	nodeName, _ := util.HasAnnotation(pod, util.PodPredicateNodeAnnotation)
	if len(nodeName) == 0 {
		return false
	}
	preAlloc, ok := util.HasAnnotation(pod, util.PodVGPUPreAllocAnnotation)
	if !ok {
		return false
	}
	podDevices := device.PodDevices{}
	if err := podDevices.UnmarshalText(preAlloc); err != nil {
		return false
	}
	return true
}

// deviceFilter will choose one and only one node fullfil the request,
// so it should always be the last filter of gpuFilter
func (f *gpuFilter) deviceFilter(pod *corev1.Pod, nodes []corev1.Node) ([]corev1.Node, extenderv1.FailedNodesMap, error) {
	var (
		filteredNodes  = make([]corev1.Node, 0, 1)       // Successful nodes
		failedNodesMap = make(extenderv1.FailedNodesMap) // Failed nodes
		nodeInfoList   []*device.NodeInfo
		success        bool
	)
	// Skip pods that have already been scheduled.
	if IsScheduled(pod) {
		return filteredNodes, failedNodesMap, fmt.Errorf("pod %s had been predicated", pod.Name)
	}

	if err := f.CheckDeviceRequest(pod); err != nil {
		klog.Error(err)
		return filteredNodes, failedNodesMap, err
	}

	pods, err := f.podLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("PodLister list all pod error: %v", err)
		return filteredNodes, failedNodesMap, err
	}
	for i := range nodes {
		node := &nodes[i]
		_, ok := util.HasAnnotation(node, util.NodeDeviceRegisterAnnotation)
		if !ok {
			klog.V(3).Infof("Current node <%s> has not registered any GPU devices, skipping it", node.Name)
		}
		if !ok || !util.IsVGPUEnabledNode(node) {
			failedNodesMap[node.Name] = "no GPU device"
			continue
		}
		// Pods on aggregation nodes.
		nodePods := CollectPodsOnNode(pods, node)
		nodeInfo, err := device.NewNodeInfo(node, nodePods)
		if err != nil {
			klog.Warningf("NewNodeInfo error, skipping node: %s, err: %v", node.Name, err)
			failedNodesMap[node.Name] = err.Error()
			continue
		}
		nodeInfoList = append(nodeInfoList, nodeInfo)
	}
	// Sort nodes according to node scheduling strategy.
	nodePolicy, _ := util.HasAnnotation(pod, util.NodeSchedulerPolicyAnnotation)
	switch strings.ToLower(nodePolicy) {
	case string(util.BinpackPolicy):
		klog.V(4).Infof("Pod <%s/%s> use <%s> node scheduling strategy", pod.Namespace, pod.Name, nodePolicy)
		device.NewNodeBinpackPriority().Sort(nodeInfoList)
	case string(util.SpreadPolicy):
		klog.V(4).Infof("Pod <%s/%s> use <%s> node scheduling strategy", pod.Namespace, pod.Name, nodePolicy)
		device.NewNodeSpreadPriority().Sort(nodeInfoList)
	default:
		klog.V(4).Infof("Pod <%s/%s> no node scheduling strategy", pod.Namespace, pod.Name)
	}

	for _, nodeInfo := range nodeInfoList {
		node := nodeInfo.GetNode()
		if success {
			failedNodesMap[node.Name] = fmt.Sprintf("pod %s has already been matched to another node", pod.UID)
			continue
		}
		// Attempt to allocate devices for pods on this node.
		newPod, err := device.NewAllocator(nodeInfo).Allocate(pod)
		if err != nil {
			klog.Errorln(err.Error())
			failedNodesMap[node.Name] = err.Error()
			continue
		}
		err = client.PatchPodVGPUAnnotation(f.kubeClient, newPod)
		if err != nil {
			errMsg := fmt.Sprintf("patch pod vgpu metadata failed: %v", err)
			klog.Errorln(errMsg)
			failedNodesMap[node.Name] = errMsg
			continue
		}
		filteredNodes = append(filteredNodes, *node)
		success = true
	}

	return filteredNodes, failedNodesMap, nil
}

func CollectPodsOnNode(pods []*corev1.Pod, node *corev1.Node) []*corev1.Pod {
	klog.V(5).Infof("Collect pods on node <%s>", node.Name)
	var ret []*corev1.Pod
	for _, pod := range pods {
		var predicateNode string
		if pod.Spec.NodeName == "" {
			predicateNode, _ = util.HasAnnotation(pod, util.PodPredicateNodeAnnotation)
		}
		if (pod.Spec.NodeName == node.Name || predicateNode == node.Name) &&
			pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
			ret = append(ret, pod)
			klog.V(5).Infof("Append Pod <%s/%s> on node %s", pod.Namespace, pod.Name, node.Name)
		}
	}
	return ret
}
