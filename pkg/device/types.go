package device

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coldzerofear/vgpu-manager/cmd/device-scheduler/options"
	"github.com/coldzerofear/vgpu-manager/pkg/device/gpuallocator"
	"github.com/coldzerofear/vgpu-manager/pkg/device/gpuallocator/links"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"golang.org/x/exp/maps"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/util/compatibility"
	"k8s.io/klog/v2"
	"k8s.io/kube-scheduler/framework"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
)

type NodeConfigInfo struct {
	DeviceSplit   int     `json:"deviceSplit"`
	CoresScaling  float64 `json:"coresScaling"`
	MemoryFactor  int     `json:"memoryFactor"`
	MemoryScaling float64 `json:"memoryScaling"`
}

func (nci NodeConfigInfo) Encode() (string, error) {
	if marshal, err := json.Marshal(nci); err != nil {
		return "", err
	} else {
		return string(marshal), nil
	}
}

func (nci *NodeConfigInfo) Decode(val string) error {
	if nci == nil {
		return fmt.Errorf("self is empty")
	}
	if strings.TrimSpace(val) == "" {
		return fmt.Errorf("input value is empty")
	}
	if err := json.Unmarshal([]byte(val), nci); err != nil {
		return err
	}
	return nil
}

func (nci *NodeConfigInfo) Clone() framework.StateData {
	config := *nci
	return &config
}

type TopologyInfo struct {
	Index int                         `json:"index"`
	Links map[int][]links.P2PLinkType `json:"links"`
}

type NodeTopologyInfo []TopologyInfo

func (nti NodeTopologyInfo) Encode() (string, error) {
	if marshal, err := json.Marshal(nti); err != nil {
		return "", err
	} else {
		return string(marshal), nil
	}
}

func (nti *NodeTopologyInfo) Decode(val string) error {
	if nti == nil {
		return fmt.Errorf("self is empty")
	}
	nodeTopoInfo, err := ParseNodeTopology(val)
	if err != nil {
		return err
	}
	*nti = nodeTopoInfo
	return nil
}

func ParseNodeTopology(val string) (NodeTopologyInfo, error) {
	if val = strings.TrimSpace(val); val == "" {
		return nil, fmt.Errorf("input value is empty")
	}
	var nodeTopoInfo NodeTopologyInfo
	if err := json.Unmarshal([]byte(val), &nodeTopoInfo); err != nil {
		return nil, err
	}
	return nodeTopoInfo, nil
}

type DeviceInfo struct {
	Id         int     `json:"id"`
	Type       string  `json:"type"`
	Uuid       string  `json:"uuid"`
	Core       int64   `json:"core"`
	Memory     int64   `json:"memory"`
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
	var nodeDevices NodeDeviceInfo
	err := json.Unmarshal([]byte(val), &nodeDevices)
	if err != nil {
		return nil, err
	}
	if len(nodeDevices) > 0 {
		sort.Slice(nodeDevices, func(i, j int) bool {
			return nodeDevices[i].Id < nodeDevices[j].Id
		})
	}
	return nodeDevices, nil
}

type ContainerDeviceClaim struct {
	Name         string        `json:"name"`
	DeviceClaims []DeviceClaim `json:"deviceClaims"`
}

func (cdc *ContainerDeviceClaim) MarshalText() (string, error) {
	var devs []string
	for _, deviceClaim := range cdc.DeviceClaims {
		text, err := deviceClaim.MarshalText()
		if err != nil {
			return "", err
		}
		devs = append(devs, text)
	}
	text := fmt.Sprintf("%s[%s]", cdc.Name, strings.Join(devs, ","))
	return text, nil
}

func (cdc *ContainerDeviceClaim) UnmarshalText(text string) error {
	if cdc == nil {
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
	dcs := make([]DeviceClaim, 0)
	for _, subText := range strings.Split(text[startIndex+1:len(text)-1], ",") {
		if len(subText) == 0 {
			continue
		}
		dc := DeviceClaim{}
		if err := dc.UnmarshalText(subText); err != nil {
			return err
		}
		dcs = append(dcs, dc)
	}
	cdc.Name = text[:startIndex]
	cdc.DeviceClaims = dcs
	return nil
}

type DeviceClaim struct {
	Id     int    `json:"id"`
	Uuid   string `json:"uuid"`
	Cores  int64  `json:"cores"`
	Memory int64  `json:"memory"`
}

func (dc *DeviceClaim) MarshalText() (string, error) {
	if dc == nil {
		return "", fmt.Errorf("self is empty")
	}
	return fmt.Sprintf("%d_%s_%d_%d",
		dc.Id, dc.Uuid, dc.Cores, dc.Memory), nil
}

func (dc *DeviceClaim) UnmarshalText(text string) error {
	if dc == nil {
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
	cores, err := strconv.ParseInt(split[2], 10, 64)
	if err != nil {
		return err
	}
	memory, err := strconv.ParseInt(split[3], 10, 64)
	if err != nil {
		return err
	}
	dc.Id = id
	dc.Uuid = split[1]
	dc.Cores = cores
	dc.Memory = memory
	return nil
}

type PodDeviceClaim []ContainerDeviceClaim

func (pdc *PodDeviceClaim) MarshalText() (string, error) {
	// "cont1['%d_%s_%d_%d','%d_%s_%d_%d'];cont2[]"
	if pdc == nil || len(*pdc) == 0 {
		return "", fmt.Errorf("self is empty")
	}
	texts := make([]string, len(*pdc))
	for i, contClaim := range *pdc {
		text, err := contClaim.MarshalText()
		if err != nil {
			return "", err
		}
		texts[i] = text
	}
	return strings.Join(texts, ";"), nil
}

func (pdc *PodDeviceClaim) UnmarshalText(text string) error {
	if pdc == nil {
		return fmt.Errorf("self is empty")
	}
	text = strings.ReplaceAll(text, " ", "")
	if len(text) == 0 {
		return fmt.Errorf("input text is empty")
	}
	split := strings.Split(text, ";")
	cdcs := make([]ContainerDeviceClaim, 0, len(split))
	for _, subText := range split {
		if len(subText) == 0 {
			continue
		}
		cdc := ContainerDeviceClaim{}
		if err := cdc.UnmarshalText(subText); err != nil {
			return err
		}
		cdcs = append(cdcs, cdc)
	}
	*pdc = cdcs
	return nil
}

func UpdatePodRealContainerDeviceClaim(pod *corev1.Pod, cdc ContainerDeviceClaim) error {
	pdc := PodDeviceClaim{}
	val, _ := util.HasAnnotation(pod, util.PodVGPURealAllocAnnotation)
	if val != "" {
		if err := pdc.UnmarshalText(val); err != nil {
			klog.Warningf("decoding pod real container device claim failed: %v", err)
		}
	}
	pdc = append(pdc, cdc)
	val, err := pdc.MarshalText()
	if err != nil {
		return fmt.Errorf("encoding pod real container device claim failed: %v", err)
	}
	util.InsertAnnotation(pod, util.PodVGPURealAllocAnnotation, val)
	return nil
}

// GetCurrentPreAllocateContainerDevice find the device information pre allocated to the current container.
func GetCurrentPreAllocateContainerDevice(pod *corev1.Pod) (*ContainerDeviceClaim, error) {
	preAlloc, _ := util.HasAnnotation(pod, util.PodVGPUPreAllocAnnotation)
	preAllocPodDevices := PodDeviceClaim{}
	if err := preAllocPodDevices.UnmarshalText(preAlloc); err != nil {
		return nil, fmt.Errorf("parse pre assign devices failed: %v", err)
	}
	if len(preAllocPodDevices) == 0 {
		return nil, errors.New("current pre assign devices is empty")
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
	realAlloc, _ := util.HasAnnotation(pod, util.PodVGPURealAllocAnnotation)
	if len(realAlloc) == 0 {
		if err := checkExistCont(preAllocPodDevices[0].Name); err != nil {
			return nil, err
		}
		return &preAllocPodDevices[0], nil
	}
	realAllocPodDevices := PodDeviceClaim{}
	if err := realAllocPodDevices.UnmarshalText(realAlloc); err != nil {
		return nil, fmt.Errorf("parse real assign devices failed: %v", err)
	}
	for i, contDevs := range preAllocPodDevices {
		isAssigned := slices.ContainsFunc(realAllocPodDevices, func(cd ContainerDeviceClaim) bool {
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
	usedCores   int64
	totalCores  int64
	usedMemory  int64
	totalMemory int64
	capability  float32
	numa        int
	busId       string
	mig         bool
	healthy     bool
}

func NewFakeDevice(id, usedNum, totalNum int, usedCore, totalCore, usedMem, totalMem int64, numa int) *Device {
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

// NewFakeDeviceWithUUID is NewFakeDevice with an explicit UUID. Use this
// in tests that need to round-trip devices through any UUID-keyed map
// (the link-topology allocation path, AreDevicesLinked, etc.) — plain
// NewFakeDevice leaves uuid as "" which collapses every fake device to
// the same map entry.
func NewFakeDeviceWithUUID(uuid string, id, usedNum, totalNum int, usedCore, totalCore, usedMem, totalMem int64, numa int) *Device {
	d := NewFakeDevice(id, usedNum, totalNum, usedCore, totalCore, usedMem, totalMem, numa)
	d.uuid = uuid
	return d
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

func (dev *Device) DeepCopy() *Device {
	device := *dev
	return &device
}

var (
	gpuTopoEnabledOnce sync.Once
	gpuTopologyEnabled bool
)

func SetGPUTopologyEnabled(flag bool) {
	gpuTopoEnabledOnce.Do(func() {
		gpuTopologyEnabled = flag
		klog.InfoS(fmt.Sprintf("Feature Gates[%s]", util.GPUTopology), "enabled", flag)
	})
}

func IsGPUTopologyEnabled() bool {
	gpuTopoEnabledOnce.Do(func() {
		featureGate := compatibility.DefaultComponentGlobalsRegistry.FeatureGateFor(options.Component)
		gpuTopologyEnabled = featureGate != nil && featureGate.Enabled(util.GPUTopology)
		klog.InfoS(fmt.Sprintf("Feature Gates[%s]", util.GPUTopology), "enabled", gpuTopologyEnabled)
	})
	return gpuTopologyEnabled
}

type DeviceGatherInfo struct {
	DeviceMap           map[int]*Device
	DeviceList          gpuallocator.DeviceList
	DeviceIndexMap      map[string]int
	EnabledGPUTopology  bool
	EnabledNumaAffinity bool
	NodeConfigInfo
}

func NewNodeDeviceGatherInfo(node *corev1.Node, option *NodeInfoOption) (*DeviceGatherInfo, error) {
	deviceRegister, _ := util.HasAnnotation(node, util.NodeDeviceRegisterAnnotation)
	nodeDeviceInfo, err := ParseNodeDeviceInfo(deviceRegister)
	if err != nil || len(nodeDeviceInfo) == 0 {
		klog.V(2).ErrorS(err, "parse node device registry failed", "node", node.Name, "value", deviceRegister)
		return nil, errors.New("incorrect GPU registry")
	}
	var nodeConfigInfo NodeConfigInfo
	if option != nil && option.nodeConfig != nil {
		nodeConfigInfo = *option.nodeConfig
	} else {
		nodeConfig, _ := util.HasAnnotation(node, util.NodeConfigInfoAnnotation)
		if err = nodeConfigInfo.Decode(nodeConfig); err != nil {
			klog.V(2).ErrorS(err, "parse node config information failed", "node", node.Name, "value", nodeConfig)
			return nil, errors.New("incorrect GPU configuration")
		}
	}
	deviceGatherInfo := DeviceGatherInfo{
		DeviceMap:      make(map[int]*Device, len(nodeDeviceInfo)),
		DeviceList:     make(gpuallocator.DeviceList, len(nodeDeviceInfo)),
		DeviceIndexMap: make(map[string]int, len(nodeDeviceInfo)),
		NodeConfigInfo: nodeConfigInfo,
	}
	numaSet := sets.NewInt()
	for _, device := range nodeDeviceInfo {
		if device.Numa >= 0 {
			numaSet.Insert(device.Numa)
		}
		deviceGatherInfo.DeviceIndexMap[device.Uuid] = device.Id
		deviceGatherInfo.DeviceMap[device.Id] = NewDevice(device)
		deviceGatherInfo.DeviceList[device.Id] = gpuallocator.NewDevice(device.Id, device.Uuid, device.BusId)
	}
	deviceGatherInfo.EnabledNumaAffinity = numaSet.Len() > 0
	if IsGPUTopologyEnabled() {
		topoValue, ok := util.HasAnnotation(node, util.NodeDeviceTopologyAnnotation)
		if !ok || len(topoValue) == 0 {
			klog.V(3).InfoS("node does not have device topology information", "node", node.Name)
			return &deviceGatherInfo, nil
		}
		nodeTopology, err := ParseNodeTopology(topoValue)
		if err != nil {
			klog.V(3).ErrorS(err, "parse node device topology info failed", "node", node.Name, "topologyVal", topoValue)
		}
		for _, deviceTopoInfo := range nodeTopology {
			for toIdx, p2pLinks := range deviceTopoInfo.Links {
				for _, p2pLinkType := range p2pLinks {
					deviceGatherInfo.EnabledGPUTopology = true
					deviceGatherInfo.DeviceList.AddLink(deviceTopoInfo.Index, toIdx, p2pLinkType)
				}
			}
		}
	}
	return &deviceGatherInfo, nil
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
func (dev *Device) GetTotalMemory() int64 {
	return dev.totalMemory
}

// GetUsedMemory returns the usedMemory of this device
func (dev *Device) GetUsedMemory() int64 {
	return dev.usedMemory
}

// GetTotalCores returns the totalCores of this device
func (dev *Device) GetTotalCores() int64 {
	return dev.totalCores
}

// GetUsedCores returns the usedCores of this device
func (dev *Device) GetUsedCores() int64 {
	return dev.usedCores
}

// GetTotalNumber returns the totalNum of this device
func (dev *Device) GetTotalNumber() int {
	return dev.totalNumber
}

// GetUsedNumber returns the usedNumber of this device
func (dev *Device) GetUsedNumber() int {
	return dev.usedNumber
}

// addUsedResources records the used GPU core and memory
func (dev *Device) addUsedResources(usedCores, usedMemory int64) {
	dev.usedNumber++
	dev.usedCores += usedCores
	dev.usedMemory += usedMemory
}

// GetBusID returns the busId of this device
func (dev *Device) GetBusID() string {
	return dev.busId
}

// AllocatableCores returns the remaining cores of this GPU device
func (dev *Device) AllocatableCores() int64 {
	allocatableCores := dev.totalCores - dev.usedCores
	if allocatableCores >= 0 {
		return allocatableCores
	}
	return 0
}

// AllocatableMemory returns the remaining memory of this GPU device
func (dev *Device) AllocatableMemory() int64 {
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

// ResetUsed Data reset has been used
func (dev *Device) ResetUsed() {
	dev.usedNumber = 0
	dev.usedCores = 0
	dev.usedMemory = 0
}

// GetPodDeviceClaim Retrieve device claim information for a pod,
// return it if there is actual allocated device claim,
// otherwise revert back to the device claims pre allocated by the scheduler.
func GetPodDeviceClaim(pod *corev1.Pod) PodDeviceClaim {
	var (
		realPodDeviceClaim = PodDeviceClaim{}
		prePodDeviceClaim  = PodDeviceClaim{}
	)
	realAlloc, _ := util.HasAnnotation(pod, util.PodVGPURealAllocAnnotation)
	if len(realAlloc) > 0 {
		if err := realPodDeviceClaim.UnmarshalText(realAlloc); err != nil {
			msg := fmt.Sprintf("pod annotation['%s'] parsing failed", util.PodVGPURealAllocAnnotation)
			klog.V(3).ErrorS(err, msg, "pod", klog.KObj(pod), "annoValue", realAlloc)
		}
	}
	preAlloc, _ := util.HasAnnotation(pod, util.PodVGPUPreAllocAnnotation)
	if len(preAlloc) > 0 {
		if err := prePodDeviceClaim.UnmarshalText(preAlloc); err != nil {
			msg := fmt.Sprintf("pod annotation['%s'] parsing failed", util.PodVGPUPreAllocAnnotation)
			klog.V(3).ErrorS(err, msg, "pod", klog.KObj(pod), "annoValue", preAlloc)
		}
	}
	if len(realPodDeviceClaim) >= len(prePodDeviceClaim) {
		return realPodDeviceClaim
	}
	return prePodDeviceClaim
}

func NewFakeNodeInfo(node *corev1.Node, gpuTopology bool, devices ...*Device) *NodeInfo {
	ret := &NodeInfo{
		name:           node.Name,
		node:           node,
		gpuTopology:    gpuTopology,
		deviceMap:      make(map[int]*Device, len(devices)),
		deviceList:     make(gpuallocator.DeviceList, len(devices)),
		deviceIndexMap: make(map[string]int, len(devices)),
	}
	for _, device := range devices {
		ret.deviceMap[device.GetID()] = device
		ret.deviceIndexMap[device.GetUUID()] = device.GetID()
		ret.deviceList[device.GetID()] = gpuallocator.NewDevice(
			device.GetID(), device.GetUUID(), device.GetBusID())
	}
	// Recompute topology fitness for the fake NodeInfo — tests that exercise
	// ByNodeGPUTopologyFitness etc. need these populated to make meaningful
	// assertions. computeMax* are no-ops when gpuTopology/numaTopology is
	// false, so the existing zero-value path still works for non-topology
	// tests.
	if ret.gpuTopology {
		ret.maxLinkComponentSize, ret.linkComponentByUUID = computeLinkComponents(ret.deviceList)
	}
	ret.numaTopology = false
	for _, d := range devices {
		if d != nil && d.GetNUMA() >= 0 {
			ret.numaTopology = true
			break
		}
	}
	if ret.numaTopology {
		ret.maxNUMAGroupSize = computeMaxNUMAGroupSize(ret.deviceMap)
	}
	ret.RefreshResourcesData()
	return ret
}

type NodeInfo struct {
	name           string
	node           *corev1.Node
	deviceMap      map[int]*Device
	deviceIndexMap map[string]int
	deviceList     gpuallocator.DeviceList
	totalNumber    int
	usedNumber     int
	totalMemory    int64
	usedMemory     int64
	totalCores     int64
	usedCores      int64
	maxCapability  float32
	// schedulableDevices is the count of healthy, non-MIG devices on the
	// node — i.e. cards that COULD host a vGPU, regardless of how many
	// slots are currently free. NOT a measure of current availability.
	schedulableDevices int
	// maxDeviceCores / maxDeviceMemory are the largest single-device TOTAL
	// core / memory CAPACITY across the node's schedulable devices (again
	// capacity, not current remaining). Used by the deviceFilter structural
	// pre-check "can any single card on this node ever hold the largest
	// container's per-vGPU request?".
	maxDeviceCores  int64
	maxDeviceMemory int64
	gpuTopology     bool
	numaTopology    bool
	// maxLinkComponentSize is the largest set of GPUs connected to each
	// other via at least one P2P link (computed once at NodeInfo construction
	// time via union-find over deviceList). Used by ByNodeGPUTopologyFitness
	// so the node-level sort knows whether this node CAN actually satisfy a
	// requested link-topology group, not just whether it has topology info.
	// Zero when gpuTopology is false.
	maxLinkComponentSize int
	// linkComponentByUUID maps each GPU's UUID to the component-root index it
	// belongs to in the same union-find that produced maxLinkComponentSize.
	// Two UUIDs with the same value are reachable via a chain of P2P links;
	// different values mean they sit in disjoint connectivity islands. Used
	// by AreDevicesLinked for strict-link allocation validation. Nil/empty
	// when gpuTopology is false.
	linkComponentByUUID map[string]int
	// maxNUMAGroupSize is the largest count of GPUs sharing a single NUMA
	// node. Equivalent role to maxLinkComponentSize for NUMA-mode sorting.
	// Zero when numaTopology is false.
	maxNUMAGroupSize int
	NodeConfigInfo
}

type NodeInfoOption struct {
	excludedUidSet sets.Set[types.UID]
	nodePods       []*corev1.Pod
	nodeConfig     *NodeConfigInfo
}

type NodeInfoOptionFn func(*NodeInfoOption)

func WithNodeConfig(config *NodeConfigInfo) NodeInfoOptionFn {
	return func(o *NodeInfoOption) {
		o.nodeConfig = config
	}
}

func WithExcludedUidSet(uidSet sets.Set[types.UID]) NodeInfoOptionFn {
	return func(o *NodeInfoOption) {
		if o.excludedUidSet == nil {
			o.excludedUidSet = uidSet
		} else if uidSet != nil {
			o.excludedUidSet.Insert(uidSet.UnsortedList()...)
		}
	}
}

func WithExcludedPods(uids ...types.UID) NodeInfoOptionFn {
	return func(o *NodeInfoOption) {
		if o.excludedUidSet == nil {
			o.excludedUidSet = sets.New[types.UID](uids...)
		} else {
			o.excludedUidSet.Insert(uids...)
		}
	}
}

func WithNodePods(pods ...*corev1.Pod) NodeInfoOptionFn {
	return func(o *NodeInfoOption) {
		if o.nodePods == nil {
			o.nodePods = pods
		} else {
			o.nodePods = append(o.nodePods, pods...)
		}
	}
}

func NewNodeInfo(node *corev1.Node, opts ...NodeInfoOptionFn) (*NodeInfo, error) {
	klog.V(4).Infof("new nodeInfo for %s", node.Name)
	infoOption := &NodeInfoOption{}
	for _, opt := range opts {
		opt(infoOption)
	}
	gatherInfo, err := NewNodeDeviceGatherInfo(node, infoOption)
	if err != nil {
		return nil, err
	}
	ret := &NodeInfo{
		node:           node,
		name:           node.Name,
		deviceMap:      gatherInfo.DeviceMap,
		deviceList:     gatherInfo.DeviceList,
		deviceIndexMap: gatherInfo.DeviceIndexMap,
		gpuTopology:    gatherInfo.EnabledGPUTopology,
		numaTopology:   gatherInfo.EnabledNumaAffinity,
		NodeConfigInfo: gatherInfo.NodeConfigInfo,
	}
	// Precompute topology fitness signals so the node-level sort can ask
	// "can this node fit a group of N GPUs?" in O(1). These are constants for
	// the lifetime of the NodeInfo snapshot.
	if ret.gpuTopology {
		ret.maxLinkComponentSize, ret.linkComponentByUUID = computeLinkComponents(gatherInfo.DeviceList)
	}
	if ret.numaTopology {
		ret.maxNUMAGroupSize = computeMaxNUMAGroupSize(gatherInfo.DeviceMap)
	}
	ret.AddPodsUsedResources(infoOption.nodePods, opts...)
	ret.RefreshResourcesData()
	return ret, nil
}

// computeLinkComponents runs union-find (Weighted Quick Union with path
// compression) over the GPU link graph and returns both the largest
// connected component size AND a per-UUID component-root map. Two GPUs are
// connected if their Links slice has at least one P2P link entry, regardless
// of link type — even a PCIe-cross-CPU edge counts, because membership here
// only answers "are these GPUs reachable from each other"; link QUALITY
// ranking happens later inside the bestEffort policy.
//
// The byUUID map lets allocateLink verify that the bestEffort-chosen set
// actually forms a single connected component (bestEffort returns the
// highest-scoring partition without rejecting score-zero results, so the
// strict-link contract needs this independent check).
//
// Complexity O((V + E)·α(V)) ≈ O(V + E) which is negligible compared to the
// existing topology parsing cost.
func computeLinkComponents(devices gpuallocator.DeviceList) (max int, byUUID map[string]int) {
	n := len(devices)
	byUUID = make(map[string]int, n)
	if n == 0 {
		return 0, byUUID
	}
	parent := make([]int, n)
	rank := make([]int, n)
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(x int) int {
		if parent[x] != x {
			parent[x] = find(parent[x])
		}
		return parent[x]
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra == rb {
			return
		}
		if rank[ra] < rank[rb] {
			ra, rb = rb, ra
		}
		parent[rb] = ra
		if rank[ra] == rank[rb] {
			rank[ra]++
		}
	}
	for i, d := range devices {
		if d == nil {
			continue
		}
		for j, links := range d.Links {
			if i == j || len(links) == 0 {
				continue
			}
			if j < 0 || j >= n {
				continue
			}
			union(i, j)
		}
	}
	counts := make(map[int]int, n)
	for i, d := range devices {
		if d == nil {
			continue
		}
		root := find(i)
		counts[root]++
		byUUID[d.UUID] = root
	}
	for _, c := range counts {
		if c > max {
			max = c
		}
	}
	return max, byUUID
}

// computeMaxNUMAGroupSize returns the largest count of GPUs sharing one NUMA
// node. Devices with NUMA index < 0 (unknown) are not grouped — they don't
// contribute to any NUMA-locality guarantee.
func computeMaxNUMAGroupSize(deviceMap map[int]*Device) int {
	groups := make(map[int]int)
	for _, d := range deviceMap {
		if d == nil {
			continue
		}
		numa := d.GetNUMA()
		if numa < 0 {
			continue
		}
		groups[numa]++
	}
	max := 0
	for _, c := range groups {
		if c > max {
			max = c
		}
	}
	return max
}

func (n *NodeInfo) DeepCopy() *NodeInfo {
	nodeInfo := *n
	nodeInfo.node = n.node.DeepCopy()
	deviceMap := make(map[int]*Device, len(n.deviceMap))
	for index, device := range n.deviceMap {
		deviceMap[index] = device.DeepCopy()
	}
	nodeInfo.deviceMap = deviceMap
	nodeInfo.deviceIndexMap = maps.Clone(n.deviceIndexMap)
	nodeInfo.deviceList = slices.Clone(n.deviceList)
	nodeInfo.linkComponentByUUID = maps.Clone(n.linkComponentByUUID)
	return &nodeInfo
}

func (n *NodeInfo) Clone() framework.StateData {
	return n.DeepCopy()
}

// StuckGracePeriod bounds how long after Filter writes the pre-allocation
// the pod's GPU is still considered "current" without further evidence.
//
// The grace must distinguish two states that look identical in the pod's
// annotations: "just pre-allocated, bind in progress" vs "stuck for many
// cycles with LastTransitionTime frozen". Kubernetes does NOT advance LTT on
// repeated same-status condition writes, so the annotation alone cannot tell
// them apart; wall-clock elapsed time since Filter is the one signal that is
// consistent across leader-elected scheduler replicas (annotation + clock,
// no in-memory state to lose on leader change).
//
// The grace must comfortably exceed the cluster's worst-case bind latency.
// Typical Kubernetes binding completes in seconds; 30s gives ~6x margin.
// Operators in pathologically slow clusters may need to raise this.
var (
	StuckGracePeriod = 30 * time.Second
	initOnce         sync.Once
)

func MustInitGlobalStuckGracePeriod(stuckGracePeriod string) {
	initOnce.Do(func() {
		if stuckGracePeriod != "" {
			duration, err := time.ParseDuration(stuckGracePeriod)
			if err != nil {
				klog.Fatalf("parse stuck-grace-period failed: %v", err)
			}
			if duration < time.Second {
				klog.Fatalf("stuck-grace-period not less than 1 second, current: %s", stuckGracePeriod)
			}
			StuckGracePeriod = duration
		}
	})
}

// ShouldCountPodDeviceAllocation reports whether the pod's GPU pre-allocation
// should be included in NodeInfo resource accounting.
// Returns true  = the allocation is current and should be counted as used.
// Returns false = the allocation is stale or void and must be skipped.
//
// We skip (return false) in three situations:
//  1. The previous bind cycle failed (AssignPhaseFailed label): the prior
//     allocation is void; skip it so NodeInfo shows true free capacity and
//     Filter can re-trigger a fresh pre-allocation.
//  2. The PodScheduled condition flipped to Unschedulable on or after Filter
//     last ran (predicateTime <= LastTransitionTime): if LTT advanced past
//     our predicateTime the current cycle was rejected; if it equals (same
//     second) we conservatively assume the condition is newer to avoid the
//     ordering ambiguity.
//  3. Stuck cycle: predicateTime > LTT but more than StuckGracePeriod has
//     elapsed since Filter wrote it. Bind should have completed long ago;
//     the absence of a transition (LTT did not advance) means repeated
//     same-status failures, so the allocation is stale and must be freed.
func ShouldCountPodDeviceAllocation(pod *corev1.Pod) bool {
	if pod.Spec.NodeName != "" {
		return true
	}
	// The previous bind cycle failed; the prior allocation is void.
	if phase, ok := util.HasLabel(pod, util.PodAssignedPhaseLabel); ok &&
		phase == string(util.AssignPhaseFailed) {
		return false
	}
	_, condition := podutil.GetPodCondition(&pod.Status, corev1.PodScheduled)
	if condition == nil {
		return true
	}
	if condition.Status != corev1.ConditionFalse ||
		condition.Reason != corev1.PodReasonUnschedulable {
		return true
	}
	predicateTimeStr, ok := util.HasAnnotation(pod, util.PodPredicateTimeAnnotation)
	if !ok || predicateTimeStr == "" {
		return false
	}
	predicateTimeNanos, err := strconv.ParseInt(predicateTimeStr, 10, 64)
	if err != nil || predicateTimeNanos <= 0 {
		return false
	}
	// LastTransitionTime is persisted at second precision (RFC3339), so
	// compare in seconds. Same-second is conservatively treated as
	// "condition newer" to avoid double-allocation when ordering is ambiguous.
	if predicateTimeNanos/int64(time.Second) <= condition.LastTransitionTime.Unix() {
		return false
	}
	stuckGracePeriod := StuckGracePeriod
	stuckDuration := time.Since(time.Unix(0, int64(predicateTimeNanos)))
	// support custom stuck grace period annotations
	if val, ok := util.HasAnnotation(pod, util.SchedulerStuckGracePeriodAnnotation); ok && val != "" {
		if gracePeriod, err := time.ParseDuration(val); err != nil {
			klog.V(5).ErrorS(err, "parse stuck grace period annotation failed, fallback to default values",
				"pod", klog.KObj(pod), "annotationValue", val, "defaultValue", stuckGracePeriod.String())
		} else if gracePeriod < time.Second {
			klog.V(5).ErrorS(nil, "custom stuck grace period not less than 1 second, fallback to default values",
				"pod", klog.KObj(pod), "annotationValue", val, "defaultValue", stuckGracePeriod.String())
		} else {
			stuckGracePeriod = gracePeriod
		}
	}
	// predicateTime > LTT: filter ran after the condition was set. The
	// allocation is current iff it is still within the bind grace; otherwise
	// LTT did not advance despite enough time having passed for bind to
	// complete — the pod is genuinely stuck and its GPU must be released.
	// predicateTime in nanoseconds fits in int64 for any realistic wall-clock
	// time, so the cast is safe.
	return stuckDuration <= stuckGracePeriod
}

func (n *NodeInfo) AddPodsUsedResources(pods []*corev1.Pod, opts ...NodeInfoOptionFn) {
	if len(pods) > 0 {
		infoOption := &NodeInfoOption{}
		for _, opt := range opts {
			opt(infoOption)
		}
		if infoOption.excludedUidSet == nil {
			infoOption.excludedUidSet = sets.New[types.UID]()
		}
		util.PodsOnNodeCallback(pods, n.node, func(pod *corev1.Pod) {
			if !infoOption.excludedUidSet.Has(pod.UID) {
				n.AddPodUsedResources(pod)
			}
		})
	}
}

func (n *NodeInfo) AddPodUsedResources(pod *corev1.Pod) {
	//if !util.IsVGPUResourcePod(pod) {
	//	return
	//}

	// Skip pods whose GPU pre-allocation should not be counted.
	if !ShouldCountPodDeviceAllocation(pod) {
		return
	}
	// According to the pods' annotations, construct the node allocation state
	podDeviceClaim := GetPodDeviceClaim(pod)
	if len(podDeviceClaim) == 0 {
		//klog.InfoS("discovery of possible damage to pod device metadata", "pod", klog.KObj(pod))
		return
	}
	for _, containerClaim := range podDeviceClaim {
		for _, claim := range containerClaim.DeviceClaims {
			if err := n.AddUsedResources(claim); err != nil {
				klog.Warningf("failed to update used resource for node %s dev %d due to %v", n.name, claim.Id, err)
			}
		}
	}
}

func (n *NodeInfo) ResetResourceUsage() {
	n.usedNumber, n.usedCores, n.usedMemory = 0, 0, 0
	for _, deviceInfo := range n.deviceMap {
		deviceInfo.ResetUsed()
	}
}

func (n *NodeInfo) RefreshResourcesData() {
	n.totalNumber, n.totalCores, n.totalMemory = 0, 0, 0
	n.usedNumber, n.usedCores, n.usedMemory, n.maxCapability = 0, 0, 0, 0
	n.schedulableDevices, n.maxDeviceCores, n.maxDeviceMemory = 0, 0, 0
	for _, deviceInfo := range n.deviceMap {
		// Do not include MIG enabled devices and unhealthy devices in the assignable resources.
		if !deviceInfo.IsMIG() && deviceInfo.Healthy() {
			n.schedulableDevices++
			n.totalNumber += deviceInfo.GetTotalNumber()
			n.usedNumber += deviceInfo.GetTotalNumber() - deviceInfo.AllocatableNumber()
			n.totalMemory += deviceInfo.GetTotalMemory()
			n.usedMemory += deviceInfo.GetTotalMemory() - deviceInfo.AllocatableMemory()
			n.totalCores += deviceInfo.GetTotalCores()
			n.usedCores += deviceInfo.GetTotalCores() - deviceInfo.AllocatableCores()
			n.maxCapability = max(n.maxCapability, deviceInfo.GetComputeCapability())
			n.maxDeviceCores = max(n.maxDeviceCores, deviceInfo.GetTotalCores())
			n.maxDeviceMemory = max(n.maxDeviceMemory, deviceInfo.GetTotalMemory())
		}
	}
}

// AddUsedResources records the used GPU core and memory
func (n *NodeInfo) AddUsedResources(claim DeviceClaim) error {
	deviceId, ok := n.deviceIndexMap[claim.Uuid]
	if !ok {
		return fmt.Errorf("device UUID <%s> does not exist in the NodeInfo <%s>", claim.Uuid, n.name)
	}
	device, ok := n.deviceMap[deviceId]
	if !ok {
		return fmt.Errorf("device ID <%d> does not exist in the NodeInfo <%s>", deviceId, n.name)
	}
	device.addUsedResources(claim.Cores, claim.Memory)
	if !device.IsMIG() && device.Healthy() {
		n.usedNumber++
		n.usedCores += claim.Cores
		n.usedMemory += claim.Memory
	}
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

// GetDeviceIndexMap returns the uuid and index mapping of the devices
func (n *NodeInfo) GetDeviceIndexMap() map[string]int {
	return n.deviceIndexMap
}

// GetSchedulableDeviceCount returns the count of healthy, non-MIG devices
// on the node — cards that COULD host a vGPU. This is capacity, not current
// availability: a fully-allocated card still counts.
func (n *NodeInfo) GetSchedulableDeviceCount() int {
	return n.schedulableDevices
}

// GetMaxDeviceMemory returns the largest single-device TOTAL memory CAPACITY
// across the node's schedulable devices (not the remaining/free memory).
func (n *NodeInfo) GetMaxDeviceMemory() int64 {
	return n.maxDeviceMemory
}

// GetMaxDeviceCores returns the largest single-device TOTAL core CAPACITY
// across the node's schedulable devices (not the remaining/free cores).
func (n *NodeInfo) GetMaxDeviceCores() int64 {
	return n.maxDeviceCores
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
func (n *NodeInfo) GetTotalCores() int64 {
	return n.totalCores
}

// GetUsedCores returns the used cores of this node
func (n *NodeInfo) GetUsedCores() int64 {
	return n.usedCores
}

// GetAvailableCores returns the remaining cores of this node
func (n *NodeInfo) GetAvailableCores() int64 {
	availableCore := n.totalCores - n.usedCores
	if availableCore >= 0 {
		return availableCore
	}
	return 0
}

// GetTotalMemory returns the total memory of this node
func (n *NodeInfo) GetTotalMemory() int64 {
	return n.totalMemory
}

// GetUsedMemory returns the used memory of this node
func (n *NodeInfo) GetUsedMemory() int64 {
	return n.usedMemory
}

// GetAvailableMemory returns the remaining memory of this node
func (n *NodeInfo) GetAvailableMemory() int64 {
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

// GetUsedNumber returns the used number of this node
func (n *NodeInfo) GetUsedNumber() int {
	return n.usedNumber
}

// GetAvailableNumber returns the remaining number of this node
func (n *NodeInfo) GetAvailableNumber() int {
	availableNum := n.totalNumber - n.usedNumber
	if availableNum >= 0 {
		return availableNum
	}
	return 0
}

// HasGPUTopology Return whether there is GPU link topology information
func (n *NodeInfo) HasGPUTopology() bool {
	return n.gpuTopology
}

// HasNUMATopology Return whether there is GPU numa topology information
func (n *NodeInfo) HasNUMATopology() bool {
	return n.numaTopology
}

// MaxLinkComponentSize returns the largest number of GPUs mutually reachable
// via P2P links on this node. A node-level sort can use this to determine
// whether the node CAN host a topology-aware group of size N (component
// size >= N) — strictly stronger than HasGPUTopology() which only confirms
// link metadata was reported.
func (n *NodeInfo) MaxLinkComponentSize() int {
	return n.maxLinkComponentSize
}

// AreDevicesLinked reports whether every UUID in the set belongs to the same
// NVLink-connected component. Used by strict-link allocation to reject sets
// that bestEffort returned as "highest-scoring" even though their actual link
// score is zero (i.e. the chosen GPUs sit in disjoint connectivity islands).
//
// Sets of size <= 1 are trivially connected. An unknown UUID counts as a
// failure (we cannot certify what we don't know about). Returns true on
// nodes without GPU topology only for the trivial-size case — any multi-GPU
// set on a non-topology node correctly returns false, since "no link
// metadata" means we cannot prove connectivity.
func (n *NodeInfo) AreDevicesLinked(uuids []string) bool {
	if len(uuids) <= 1 {
		return true
	}
	first, ok := n.linkComponentByUUID[uuids[0]]
	if !ok {
		return false
	}
	for _, u := range uuids[1:] {
		comp, ok := n.linkComponentByUUID[u]
		if !ok || comp != first {
			return false
		}
	}
	return true
}

// MaxNUMAGroupSize returns the largest number of GPUs sharing a single NUMA
// node. NUMA-mode sorting checks this against the pod's request so we avoid
// ranking nodes high that "have NUMA topology" but couldn't actually fit a
// same-NUMA group.
func (n *NodeInfo) MaxNUMAGroupSize() int {
	return n.maxNUMAGroupSize
}
