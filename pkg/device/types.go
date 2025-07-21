package device

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/coldzerofear/vgpu-manager/cmd/scheduler/options"
	"github.com/coldzerofear/vgpu-manager/pkg/device/gpuallocator"
	"github.com/coldzerofear/vgpu-manager/pkg/device/gpuallocator/links"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/component-base/featuregate"
	"k8s.io/klog/v2"
)

type NodeConfigInfo struct {
	DeviceSplit   int     `json:"deviceSplit"`
	CoresScaling  float64 `json:"coresScaling"`
	MemoryFactor  int     `json:"memoryFactor"`
	MemoryScaling float64 `json:"memoryScaling"`
}

func (n NodeConfigInfo) Encode() (string, error) {
	if marshal, err := json.Marshal(n); err != nil {
		return "", err
	} else {
		return string(marshal), nil
	}
}

func (n *NodeConfigInfo) Decode(val string) error {
	if n == nil {
		return fmt.Errorf("self is empty")
	}
	if strings.TrimSpace(val) == "" {
		return fmt.Errorf("input value is empty")
	}
	if err := json.Unmarshal([]byte(val), n); err != nil {
		return err
	}
	return nil
}

type TopologyInfo struct {
	Index int                         `json:"index"`
	Links map[int][]links.P2PLinkType `json:"links"`
}

type NodeTopologyInfo []TopologyInfo

func (n NodeTopologyInfo) Encode() (string, error) {
	if marshal, err := json.Marshal(n); err != nil {
		return "", err
	} else {
		return string(marshal), nil
	}
}

func (n *NodeTopologyInfo) Decode(val string) error {
	if n == nil {
		return fmt.Errorf("self is empty")
	}
	topology, err := ParseNodeTopology(val)
	if err != nil {
		return err
	}
	*n = topology
	return nil
}

func ParseNodeTopology(val string) (NodeTopologyInfo, error) {
	if strings.TrimSpace(val) == "" {
		return nil, fmt.Errorf("input value is empty")
	}
	var nodeTopologyInfo NodeTopologyInfo
	err := json.Unmarshal([]byte(val), &nodeTopologyInfo)
	if err != nil {
		return nil, err
	}
	return nodeTopologyInfo, nil
}

type DeviceInfo struct {
	Id         int     `json:"id"`
	Type       string  `json:"type"`
	Uuid       string  `json:"uuid"`
	Core       int     `json:"core"`
	Memory     int     `json:"memory"`
	Number     int     `json:"number"`
	Numa       int     `json:"numa"`
	Mig        bool    `json:"mig"`
	BusId      string  `json:"busId"`
	Capability float32 `json:"capability"`
	Healthy    bool    `json:"healthy"`
}

type NodeDeviceInfo []DeviceInfo

func (n NodeDeviceInfo) Encode() (string, error) {
	if marshal, err := json.Marshal(n); err != nil {
		return "", err
	} else {
		return string(marshal), nil
	}
}

func (n *NodeDeviceInfo) Decode(val string) error {
	if n == nil {
		return fmt.Errorf("self is empty")
	}
	nodeDevice, err := ParseNodeDeviceInfo(val)
	if err != nil {
		return err
	}
	*n = nodeDevice
	return nil
}

func ParseNodeDeviceInfo(val string) (NodeDeviceInfo, error) {
	if strings.TrimSpace(val) == "" {
		return nil, fmt.Errorf("input value is empty")
	}
	var nodeDevice NodeDeviceInfo
	err := json.Unmarshal([]byte(val), &nodeDevice)
	if err != nil {
		return nil, err
	}
	return nodeDevice, nil
}

type ContainerDevices struct {
	Name    string        `json:"name"`
	Devices []ClaimDevice `json:"devices"`
}

func (c *ContainerDevices) MarshalText() (string, error) {
	var devs []string
	for _, device := range c.Devices {
		text, err := device.MarshalText()
		if err != nil {
			return "", err
		}
		devs = append(devs, text)
	}
	text := fmt.Sprintf("%s[%s]", c.Name, strings.Join(devs, ","))
	return text, nil
}

func (c *ContainerDevices) UnmarshalText(text string) error {
	if c == nil {
		return fmt.Errorf("self is empty")
	}
	text = strings.ReplaceAll(text, " ", "")
	if len(text) == 0 {
		return fmt.Errorf("input text is empty")
	}
	startIndex := strings.Index(text, "[")
	if startIndex < 0 || startIndex == len(text)-1 {
		return fmt.Errorf("decoding format error")
	}
	endIndex := strings.Index(text, "]")
	if endIndex < 0 || endIndex != len(text)-1 {
		return fmt.Errorf("decoding format error")
	}
	cds := make([]ClaimDevice, 0)
	for _, subText := range strings.Split(text[startIndex+1:len(text)-1], ",") {
		if len(subText) == 0 {
			continue
		}
		cd := ClaimDevice{}
		if err := cd.UnmarshalText(subText); err != nil {
			return err
		}
		cds = append(cds, cd)
	}
	c.Name = text[:startIndex]
	c.Devices = cds
	return nil
}

type ClaimDevice struct {
	Id     int    `json:"id"`
	Uuid   string `json:"uuid"`
	Cores  int    `json:"cores"`
	Memory int    `json:"memory"`
}

func (c *ClaimDevice) MarshalText() (string, error) {
	if c == nil {
		return "", fmt.Errorf("self is empty")
	}
	return fmt.Sprintf("%d_%s_%d_%d",
		c.Id, c.Uuid, c.Cores, c.Memory), nil
}

func (c *ClaimDevice) UnmarshalText(text string) error {
	if c == nil {
		return fmt.Errorf("self is empty")
	}
	text = strings.ReplaceAll(text, " ", "")
	if len(text) == 0 {
		return fmt.Errorf("input text is empty")
	}
	split := strings.Split(text, "_")
	if len(split) != 4 {
		return fmt.Errorf("text format error")
	}
	id, err := strconv.Atoi(split[0])
	if err != nil {
		return err
	}
	cores, err := strconv.Atoi(split[2])
	if err != nil {
		return err
	}
	memory, err := strconv.Atoi(split[3])
	if err != nil {
		return err
	}
	c.Id = id
	c.Uuid = split[1]
	c.Cores = cores
	c.Memory = memory
	return nil
}

type PodDevices []ContainerDevices

func (p *PodDevices) MarshalText() (string, error) {
	// "cont1['%d_%s_%d_%d','%d_%s_%d_%d'];cont2[]"
	if p == nil || len(*p) == 0 {
		return "", fmt.Errorf("self is empty")
	}
	var texts []string
	for _, contDevs := range *p {
		text, err := contDevs.MarshalText()
		if err != nil {
			return "", err
		}
		texts = append(texts, text)
	}
	return strings.Join(texts, ";"), nil
}

func (p *PodDevices) UnmarshalText(text string) error {
	if p == nil {
		return fmt.Errorf("self is empty")
	}
	text = strings.ReplaceAll(text, " ", "")
	if len(text) == 0 {
		return fmt.Errorf("input text is empty")
	}
	cds := make([]ContainerDevices, 0)
	for _, subText := range strings.Split(text, ";") {
		if len(subText) == 0 {
			continue
		}
		cd := ContainerDevices{}
		if err := cd.UnmarshalText(subText); err != nil {
			return err
		}
		cds = append(cds, cd)
	}
	*p = cds
	return nil
}

// GetCurrentPreAllocateContainerDevice find the device information pre allocated to the current container.
func GetCurrentPreAllocateContainerDevice(pod *corev1.Pod) (*ContainerDevices, error) {
	preAlloc, _ := util.HasAnnotation(pod, util.PodVGPUPreAllocAnnotation)
	preAllocPodDevices := PodDevices{}
	if err := preAllocPodDevices.UnmarshalText(preAlloc); err != nil {
		return nil, fmt.Errorf("parse pre assign devices failed: %v", err)
	}
	if len(preAllocPodDevices) == 0 {
		return nil, fmt.Errorf("current pre assign devices is empty")
	}
	checkExistCont := func(contName string) error {
		exist := slices.ContainsFunc(pod.Spec.Containers, func(container corev1.Container) bool {
			return container.Name == contName
		})
		if !exist {
			return fmt.Errorf("container <%s> does not exist in pod <%s/%s>",
				contName, pod.Namespace, pod.Name)
		}
		return nil
	}
	realAlloc, ok := util.HasAnnotation(pod, util.PodVGPURealAllocAnnotation)
	if !ok {
		if err := checkExistCont(preAllocPodDevices[0].Name); err != nil {
			return nil, err
		}
		return &preAllocPodDevices[0], nil
	}
	realAllocPodDevices := PodDevices{}
	if err := realAllocPodDevices.UnmarshalText(realAlloc); err != nil {
		return nil, fmt.Errorf("parse real assign devices failed: %v", err)
	}
	for i, contDevs := range preAllocPodDevices {
		isAssigned := slices.ContainsFunc(realAllocPodDevices,
			func(cd ContainerDevices) bool {
				return cd.Name == contDevs.Name
			})
		if !isAssigned {
			if err := checkExistCont(preAllocPodDevices[i].Name); err != nil {
				return nil, err
			}
			return &preAllocPodDevices[i], nil
		}
	}
	return nil, fmt.Errorf("no current assignable devices found")
}

type Device struct {
	id          int
	uuid        string
	deviceType  string
	usedNumber  int
	totalNumber int
	usedCores   int
	totalCores  int
	usedMemory  int
	totalMemory int
	capability  float32
	numa        int
	busId       string
	mig         bool
	healthy     bool
}

func NewFakeDevice(id, usedNum, totalNum, usedCore, totalCore, usedMem, totalMem, numa int) *Device {
	return &Device{
		id:          id,
		usedNumber:  usedNum,
		totalNumber: totalNum,
		usedCores:   usedCore,
		totalCores:  totalCore,
		usedMemory:  usedMem,
		totalMemory: totalMem,
		numa:        numa,
		healthy:     true,
	}
}

func NewDevice(dev DeviceInfo) *Device {
	return &Device{
		id:          dev.Id,
		uuid:        dev.Uuid,
		deviceType:  dev.Type,
		totalCores:  dev.Core,
		totalMemory: dev.Memory,
		mig:         dev.Mig,
		capability:  dev.Capability,
		totalNumber: dev.Number,
		numa:        dev.Numa,
		healthy:     dev.Healthy,
		busId:       dev.BusId,
	}
}

var (
	gpuTopoEnabledOnce sync.Once
	gpuTopologyEnabled bool
)

func IsGPUTopologyEnabled() bool {
	gpuTopoEnabledOnce.Do(func() {
		featureGate := featuregate.DefaultComponentGlobalsRegistry.FeatureGateFor(options.Component)
		gpuTopologyEnabled = featureGate != nil && featureGate.Enabled(options.GPUTopology)
		klog.InfoS("Feature Gates[GPUTopology]", "enabled", gpuTopologyEnabled)
	})
	return gpuTopologyEnabled
}

func NewDeviceMapAndDeviceList(node *corev1.Node) (map[int]*Device, gpuallocator.DeviceList, bool, error) {
	deviceRegister, _ := util.HasAnnotation(node, util.NodeDeviceRegisterAnnotation)
	nodeDeviceInfo, err := ParseNodeDeviceInfo(deviceRegister)
	if err != nil {
		return nil, nil, false, fmt.Errorf("parse node device information failed: %v", err)
	}
	deviceMap := make(map[int]*Device, len(nodeDeviceInfo))
	deviceList := make(gpuallocator.DeviceList, len(nodeDeviceInfo))
	for _, deviceInfo := range nodeDeviceInfo {
		deviceMap[deviceInfo.Id] = NewDevice(deviceInfo)
		deviceList[deviceInfo.Id] = gpuallocator.NewDevice(
			deviceInfo.Id, deviceInfo.Uuid, deviceInfo.BusId)
	}
	hasGPUTopology := false
	if IsGPUTopologyEnabled() {
		topoStr, ok := util.HasAnnotation(node, util.NodeDeviceTopologyAnnotation)
		if !ok || len(topoStr) == 0 {
			klog.V(3).InfoS("node does not have device topology information", "node", node.Name)
			return deviceMap, deviceList, false, nil
		}
		nodeTopology, err := ParseNodeTopology(topoStr)
		if err != nil {
			klog.V(3).ErrorS(err, "parse node device topology failed", "node", node.Name)
		}
		for _, deviceTopoInfo := range nodeTopology {
			for toIdx, p2pLinks := range deviceTopoInfo.Links {
				for _, p2pLinkType := range p2pLinks {
					hasGPUTopology = true
					deviceList.AddLink(deviceTopoInfo.Index, toIdx, p2pLinkType)
				}
			}
		}
	}
	return deviceMap, deviceList, hasGPUTopology, nil
}

// GetID returns the idx of this device
func (dev *Device) GetID() int {
	return dev.id
}

// GetUUID returns the uuid of this device
func (dev *Device) GetUUID() string {
	return dev.uuid
}

// GetNUMA returns the numa of this device
func (dev *Device) GetNUMA() int {
	return dev.numa
}

func (dev *Device) IsMIG() bool {
	return dev.mig
}

// Healthy return whether the device is healthy
func (dev *Device) Healthy() bool {
	return dev.healthy
}

// GetComputeCapability returns the capability of this device
func (dev *Device) GetComputeCapability() float32 {
	return dev.capability
}

// GetType returns the type of this device
func (dev *Device) GetType() string {
	return dev.deviceType
}

// GetTotalMemory returns the totalMemory of this device
func (dev *Device) GetTotalMemory() int {
	return dev.totalMemory
}

// GetTotalCores returns the totalCore of this device
func (dev *Device) GetTotalCores() int {
	return dev.totalCores
}

// GetTotalNumber returns the totalNum of this device
func (dev *Device) GetTotalNumber() int {
	return dev.totalNumber
}

// addUsedResources records the used GPU core and memory
func (dev *Device) addUsedResources(usedCores int, usedMemory int) {
	dev.usedNumber++
	dev.usedCores += usedCores
	dev.usedMemory += usedMemory
}

// GetBusID returns the busId of this device
func (dev *Device) GetBusID() string {
	return dev.busId
}

// AllocatableCores returns the remaining cores of this GPU device
func (dev *Device) AllocatableCores() int {
	allocatableCores := dev.totalCores - dev.usedCores
	if allocatableCores >= 0 {
		return allocatableCores
	}
	return 0
}

// AllocatableMemory returns the remaining memory of this GPU device
func (dev *Device) AllocatableMemory() int {
	allocatableMemory := dev.totalMemory - dev.usedMemory
	if allocatableMemory >= 0 {
		return allocatableMemory
	}
	return 0
}

// AllocatableNumber returns the remaining number of this GPU device
func (dev *Device) AllocatableNumber() int {
	allocatableNum := dev.totalNumber - dev.usedNumber
	if allocatableNum >= 0 {
		return allocatableNum
	}
	return 0
}

func GetPodAssignDevices(pod *corev1.Pod) PodDevices {
	var (
		podAssignDevices  PodDevices
		realAssignDevices = PodDevices{}
		preAssignDevices  = PodDevices{}
	)
	realAlloc, _ := util.HasAnnotation(pod, util.PodVGPURealAllocAnnotation)
	if len(realAlloc) > 0 {
		if err := realAssignDevices.UnmarshalText(realAlloc); err != nil {
			klog.Warningf("Pod <%s/%s> real allocation device annotation parse failed: %v",
				pod.Namespace, pod.Name, err)
		}
	}
	preAlloc, _ := util.HasAnnotation(pod, util.PodVGPUPreAllocAnnotation)
	if len(preAlloc) > 0 {
		if err := preAssignDevices.UnmarshalText(preAlloc); err != nil {
			klog.Warningf("Pod <%s/%s> pre allocation device annotation parse failed: %v",
				pod.Namespace, pod.Name, err)
		}
	}
	if len(realAssignDevices) >= len(preAssignDevices) {
		podAssignDevices = realAssignDevices
	} else {
		podAssignDevices = preAssignDevices
	}
	return podAssignDevices
}

func NewFakeNodeInfo(node *corev1.Node, gpuTopology bool, devices []*Device) *NodeInfo {
	ret := &NodeInfo{
		name:        node.Name,
		node:        node,
		gpuTopology: gpuTopology,
		deviceMap:   make(map[int]*Device, len(devices)),
		deviceList:  make(gpuallocator.DeviceList, len(devices)),
	}
	for _, device := range devices {
		ret.deviceMap[device.GetID()] = device
		ret.deviceList[device.GetID()] = gpuallocator.NewDevice(
			device.GetID(), device.GetUUID(), device.GetBusID())
	}
	ret.initResourceStatistics()
	return ret
}

type NodeInfo struct {
	name          string
	node          *corev1.Node
	deviceMap     map[int]*Device
	deviceList    gpuallocator.DeviceList
	totalNumber   int
	usedNumber    int
	totalMemory   int
	usedMemory    int
	totalCores    int
	usedCores     int
	maxCapability float32
	gpuTopology   bool
}

func NewNodeInfo(node *corev1.Node, pods []*corev1.Pod) (*NodeInfo, error) {
	klog.V(4).Infof("create nodeInfo for %s", node.Name)
	deviceMap, deviceList, gpuTopology, err := NewDeviceMapAndDeviceList(node)
	if err != nil {
		return nil, err
	}
	ret := &NodeInfo{
		node:        node,
		name:        node.Name,
		deviceMap:   deviceMap,
		deviceList:  deviceList,
		gpuTopology: gpuTopology,
	}
	ret.addPodDevResources(pods)
	ret.initResourceStatistics()
	return ret, nil
}

func (n *NodeInfo) addPodDevResources(pods []*corev1.Pod) {
	util.PodsOnNode(pods, n.node, func(pod *corev1.Pod) {
		if !util.IsVGPUResourcePod(pod) {
			return
		}
		// According to the pods' annotations, construct the node allocation state
		podAssignDevices := GetPodAssignDevices(pod)
		if len(podAssignDevices) == 0 {
			klog.Warningf("Discovered that pod <%s/%s> device annotations may be damaged", pod.Namespace, pod.Name)
			return
		}
		for _, container := range podAssignDevices {
			for _, device := range container.Devices {
				if err := n.addUsedResources(device.Id, device.Cores, device.Memory); err != nil {
					klog.Warningf("failed to update used resource for node %s dev %d due to %v", n.name, device.Id, err)
				}
			}
		}
	})
}

func (n *NodeInfo) initResourceStatistics() {
	n.totalNumber, n.usedNumber, n.totalMemory = 0, 0, 0
	n.usedMemory, n.totalCores, n.usedCores, n.maxCapability = 0, 0, 0, 0
	for _, deviceInfo := range n.deviceMap {
		// Do not include MIG enabled devices and unhealthy devices in the assignable resources.
		if deviceInfo.IsMIG() || !deviceInfo.Healthy() {
			continue
		}
		n.totalNumber += deviceInfo.GetTotalNumber()
		n.usedNumber += deviceInfo.GetTotalNumber() - deviceInfo.AllocatableNumber()
		n.totalMemory += deviceInfo.GetTotalMemory()
		n.usedMemory += deviceInfo.GetTotalMemory() - deviceInfo.AllocatableMemory()
		n.totalCores += deviceInfo.GetTotalCores()
		n.usedCores += deviceInfo.GetTotalCores() - deviceInfo.AllocatableCores()
		n.maxCapability = max(n.maxCapability, deviceInfo.capability)
	}
}

// AddUsedResources records the used GPU core and memory
func (n *NodeInfo) AddUsedResources(id int, core int, memory int) error {
	if err := n.addUsedResources(id, core, memory); err != nil {
		return err
	}
	if device := n.deviceMap[id]; !device.IsMIG() && device.Healthy() {
		n.usedNumber++
		n.usedCores += core
		n.usedMemory += memory
	}
	return nil
}

// addUsedResources records the used GPU core and memory
func (n *NodeInfo) addUsedResources(id int, core int, memory int) error {
	device, ok := n.deviceMap[id]
	if !ok {
		return fmt.Errorf("device ID <%d> does not exist in the NodeInfo <%s>", id, n.name)
	}
	device.addUsedResources(core, memory)
	return nil
}

// GetName returns node name
func (n *NodeInfo) GetName() string {
	return n.name
}

// GetDeviceCount returns the number of GPU devices
func (n *NodeInfo) GetDeviceCount() int {
	return len(n.deviceMap)
}

// GetDeviceMap returns each GPU device map structure
func (n *NodeInfo) GetDeviceMap() map[int]*Device {
	return n.deviceMap
}

// GetDeviceList returns each GPU device list structure
func (n *NodeInfo) GetDeviceList() gpuallocator.DeviceList {
	return n.deviceList
}

// GetNode returns the original node structure of kubernetes
func (n *NodeInfo) GetNode() *corev1.Node {
	return n.node
}

// GetMaxCapability returns the maxCapability of GPU devices
func (n *NodeInfo) GetMaxCapability() float32 {
	return n.maxCapability
}

// GetTotalCores returns the total cores of this node
func (n *NodeInfo) GetTotalCores() int {
	return n.totalCores
}

// GetAvailableCores returns the remaining cores of this node
func (n *NodeInfo) GetAvailableCores() int {
	availableCore := n.totalCores - n.usedCores
	if availableCore >= 0 {
		return availableCore
	}
	return 0
}

// GetTotalMemory returns the total memory of this node
func (n *NodeInfo) GetTotalMemory() int {
	return n.totalMemory
}

// GetAvailableMemory returns the remaining memory of this node
func (n *NodeInfo) GetAvailableMemory() int {
	availableMem := n.totalMemory - n.usedMemory
	if availableMem >= 0 {
		return availableMem
	}
	return 0
}

// GetTotalNumber returns the total number of this node
func (n *NodeInfo) GetTotalNumber() int {
	return n.totalNumber
}

// GetAvailableNumber returns the remaining number of this node
func (n *NodeInfo) GetAvailableNumber() int {
	availableNum := n.totalNumber - n.usedNumber
	if availableNum >= 0 {
		return availableNum
	}
	return 0
}

// HasGPUTopology Return whether there is GPU topology information
func (n *NodeInfo) HasGPUTopology() bool {
	return n.gpuTopology
}
