package device

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/device/gpuallocator"
	"github.com/coldzerofear/vgpu-manager/pkg/device/gpuallocator/links"
	"github.com/coldzerofear/vgpu-manager/pkg/scheduler/reason"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"golang.org/x/exp/maps"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
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
		return fmt.Errorf("receiver is nil")
	}
	if strings.TrimSpace(val) == "" {
		return fmt.Errorf("input value is empty")
	}
	return json.Unmarshal([]byte(val), nci)
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
		return fmt.Errorf("receiver is nil")
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
		return fmt.Errorf("receiver is nil")
	}
	parsed, err := ParseNodeDeviceInfo(val)
	if err != nil {
		return err
	}
	*n = parsed
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
	if len(nodeDevices) > 1 {
		sort.Slice(nodeDevices, func(i, j int) bool {
			return nodeDevices[i].Id < nodeDevices[j].Id
		})
	}
	return nodeDevices, nil
}

// Clone returns a shallow copy of the slice.
// NOTE: DeviceInfo contains only value types and strings,
// so this is effectively a deep copy. If pointer/slice/map
// fields are added to DeviceInfo in the future, this method
// must be updated to perform a true deep copy.
func (n *NodeDeviceInfo) Clone() framework.StateData {
	device := slices.Clone(*n)
	return &device
}

type ContainerDeviceClaim struct {
	Name         string        `json:"name"`
	DeviceClaims []DeviceClaim `json:"deviceClaims"`
}

func (cdc ContainerDeviceClaim) MarshalText() (string, error) {
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
		return fmt.Errorf("receiver is nil")
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
	split := strings.Split(text[startIndex+1:endIndex], ",")
	dcs := make([]DeviceClaim, 0, len(split))
	for _, subText := range split {
		if len(subText) == 0 {
			continue
		}
		var dc DeviceClaim
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

func (dc DeviceClaim) MarshalText() (string, error) {
	return fmt.Sprintf("%d_%s_%d_%d", dc.Id, dc.Uuid, dc.Cores, dc.Memory), nil
}

func (dc *DeviceClaim) UnmarshalText(text string) error {
	if dc == nil {
		return fmt.Errorf("receiver is nil")
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

func (pdc PodDeviceClaim) MarshalText() (string, error) {
	// "cont1['%d_%s_%d_%d','%d_%s_%d_%d'];cont2[]"
	texts := make([]string, len(pdc))
	for i, contClaim := range pdc {
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
		return fmt.Errorf("receiver is nil")
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
	var pdc PodDeviceClaim
	if val, _ := util.HasAnnotation(pod, util.PodVGPURealAllocAnnotation); len(val) > 0 {
		if err := pdc.UnmarshalText(val); err != nil {
			klog.Warningf("decoding pod real container device claim failed: %v", err)
		}
	}
	pdc = append(pdc, cdc)
	if val, err := pdc.MarshalText(); err != nil {
		return fmt.Errorf("encoding pod real container device claim failed: %v", err)
	} else {
		util.InsertAnnotation(pod, util.PodVGPURealAllocAnnotation, val)
		return nil
	}
}

// GetCurrentPreAllocateContainerDevice find the device information pre allocated to the current container.
func GetCurrentPreAllocateContainerDevice(pod *corev1.Pod) (*ContainerDeviceClaim, error) {
	preAlloc, _ := util.HasAnnotation(pod, util.PodVGPUPreAllocAnnotation)
	var preAllocPodDevices PodDeviceClaim
	if err := preAllocPodDevices.UnmarshalText(preAlloc); err != nil {
		return nil, fmt.Errorf("parse pre assign devices failed: %v", err)
	}
	if len(preAllocPodDevices) == 0 {
		return nil, errors.New("current pre assign devices is empty")
	}
	checkExistCont := func(contName string) error {
		matchName := func(container corev1.Container) bool {
			return container.Name == contName
		}
		// Container names are unique across init and regular containers, so a
		// pre-allocated entry may legitimately reference an init container.
		// Searching both lists keeps validation correct once the allocator
		// emits init-container claims; for app-only pre-alloc it is a no-op.
		exist := slices.ContainsFunc(pod.Spec.InitContainers, matchName) ||
			slices.ContainsFunc(pod.Spec.Containers, matchName)
		if !exist {
			return fmt.Errorf("container %q does not exist in pod %s", contName, klog.KObj(pod))
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
	var realAllocPodDevices PodDeviceClaim
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

type DeviceGatherInfo struct {
	DeviceMap           map[int]*Device
	DeviceList          gpuallocator.DeviceList
	DeviceIndexMap      map[string]int
	EnabledGPUTopology  bool
	EnabledNumaAffinity bool
	NodeConfigInfo
}

func NewNodeDeviceGatherInfo(node *corev1.Node, option *NodeInfoOption) (*DeviceGatherInfo, error) {
	var nodeDeviceInfo NodeDeviceInfo
	if option != nil && option.nodeDevice != nil {
		nodeDeviceInfo = *option.nodeDevice
	} else {
		deviceRegister, ok := util.HasAnnotation(node, util.NodeDeviceRegisterAnnotation)
		if !ok || len(deviceRegister) == 0 {
			return nil, errors.New(reason.New(reason.NodeNoVGPURegister).Short())
		}
		if err := nodeDeviceInfo.Decode(deviceRegister); err != nil {
			klog.V(2).ErrorS(err, "parse node device registry failed", "node", node.Name, "value", deviceRegister)
			return nil, errors.New(reason.New(reason.NodeBadVGPURegister).Short())
		}
	}
	var nodeConfigInfo NodeConfigInfo
	if option != nil && option.nodeConfig != nil {
		nodeConfigInfo = *option.nodeConfig
	} else {
		nodeConfig, ok := util.HasAnnotation(node, util.NodeConfigInfoAnnotation)
		if !ok || len(nodeConfig) == 0 {
			return nil, errors.New(reason.New(reason.NodeNoVGPUConfig).Short())
		}
		if err := nodeConfigInfo.Decode(nodeConfig); err != nil {
			klog.V(2).ErrorS(err, "parse node config information failed", "node", node.Name, "value", nodeConfig)
			return nil, errors.New(reason.New(reason.NodeBadVGPUConfig).Short())
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
	if option != nil && option.gpuTopologyEnabled {
		topoValue, ok := util.HasAnnotation(node, util.NodeDeviceTopologyAnnotation)
		if !ok || len(topoValue) == 0 {
			klog.V(3).InfoS("node does not have device topology information", "node", node.Name)
			return &deviceGatherInfo, nil
		}
		var nodeTopology NodeTopologyInfo
		if err := nodeTopology.Decode(topoValue); err != nil {
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
	dev.addUsedResourcesN(1, usedCores, usedMemory)
}

// addUsedResourcesN records an already-aggregated occupancy on this device.
// addUsedResources is the number==1 (single vGPU claim) special case.
func (dev *Device) addUsedResourcesN(usedNumber int, usedCores, usedMemory int64) {
	dev.usedNumber += usedNumber
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
			msg := fmt.Sprintf("pod annotation[%q] parsing failed", util.PodVGPURealAllocAnnotation)
			klog.V(3).ErrorS(err, msg, "pod", klog.KObj(pod), "annoValue", realAlloc)
		}
	}
	preAlloc, _ := util.HasAnnotation(pod, util.PodVGPUPreAllocAnnotation)
	if len(preAlloc) > 0 {
		if err := prePodDeviceClaim.UnmarshalText(preAlloc); err != nil {
			msg := fmt.Sprintf("pod annotation[%q] parsing failed", util.PodVGPUPreAllocAnnotation)
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
		ret.maxLinkComponentSize, ret.linkComponentByUUID = computeLinkComponents(ret.deviceList, anyLinkEdge)
		ret.maxNVLinkComponentSize, ret.nvlinkComponentByUUID = computeLinkComponents(ret.deviceList, nvlinkEdge)
		ret.nvlinkComponentToUUIDs = buildComponentIndex(ret.nvlinkComponentByUUID)
		ret.nvlinkRootByOrdinal, ret.nvlinkComponentOrdinal = buildComponentOrdinals(ret.nvlinkComponentToUUIDs, ret.deviceIndexMap)
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
	// linkComponentByUUID maps each GPU's UUID to its ANY-P2P (reachability)
	// component root — two UUIDs share a value iff reachable via any P2P link
	// (PCIe included). Used only by AreDevicesLinked (strict-link reachability
	// safety). Coarse on purpose; NVLink-quality grouping is nvlinkComponentByUUID
	// below. Nil/empty when gpuTopology is false.
	linkComponentByUUID map[string]int
	// maxNUMAGroupSize is the largest count of GPUs sharing a single NUMA
	// node. Equivalent role to maxLinkComponentSize for NUMA-mode sorting.
	// Zero when numaTopology is false.
	maxNUMAGroupSize int
	// nvlink* describe the NVLink-FABRIC components (union over NVLink edges only,
	// see nvlinkEdge), which is the grouping cross-pod affinity needs: on a full
	// NVSwitch node all GPUs are one component (no narrowing needed); on a 2x4
	// island node the two islands are distinct (so a sibling's GPUs pin the island
	// its peers stay in). nvlinkComponentByUUID: UUID → NVLink-component root;
	// nvlinkComponentToUUIDs: root → member UUIDs; nvlinkRootByOrdinal /
	// nvlinkComponentOrdinal: the cross-node-stable ordinal maps (ranked by min
	// Device.Index — ordinal-k denotes the same NVLink sub-domain on homogeneous
	// nodes). Nil/empty when gpuTopology is false.
	nvlinkComponentByUUID  map[string]int
	nvlinkComponentToUUIDs map[int][]string
	nvlinkRootByOrdinal    map[int]int
	nvlinkComponentOrdinal map[int]int
	// maxNVLinkComponentSize is the largest NVLink-fabric component size (the
	// most GPUs mutually reachable over NVLink only). Node fitness uses it to
	// rank a node that can host the request fully NVLink-connected above one that
	// could only do so by spanning NVLink islands over PCIe. Zero when gpuTopology
	// is false.
	maxNVLinkComponentSize int
	// nodePods is the set of pods scheduled (or pre-allocated) to this node,
	// as injected via WithNodePods. Retained — not just consumed for usage
	// accounting in AddPodsUsedResources — so cross-pod allocation can resolve
	// a gang's already-chosen NVLink component on this node. Read-only after
	// construction.
	nodePods []*corev1.Pod
	NodeConfigInfo
}

type NodeInfoOption struct {
	excludedUidSet     sets.Set[types.UID]
	nodePods           []*corev1.Pod
	nodeConfig         *NodeConfigInfo
	nodeDevice         *NodeDeviceInfo
	resetUsed          bool
	resetPods          bool
	gpuTopologyEnabled bool
}

type NodeInfoOptionFn func(*NodeInfoOption)

func WithGPUTopologyEnabled(b bool) NodeInfoOptionFn {
	return func(o *NodeInfoOption) {
		o.gpuTopologyEnabled = b
	}
}

func WithResetUsed(b bool) NodeInfoOptionFn {
	return func(o *NodeInfoOption) {
		o.resetUsed = b
	}
}

func WithResetPods(b bool) NodeInfoOptionFn {
	return func(o *NodeInfoOption) {
		o.resetPods = b
	}
}

func WithNodeDevice(info *NodeDeviceInfo) NodeInfoOptionFn {
	return func(o *NodeInfoOption) {
		o.nodeDevice = info
	}
}

func WithNodeConfig(info *NodeConfigInfo) NodeInfoOptionFn {
	return func(o *NodeInfoOption) {
		o.nodeConfig = info
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
		nodePods:       make([]*corev1.Pod, 0, len(infoOption.nodePods)),
	}
	// Precompute topology fitness signals so the node-level sort can ask
	// "can this node fit a group of N GPUs?" in O(1). These are constants for
	// the lifetime of the NodeInfo snapshot.
	if ret.gpuTopology {
		ret.maxLinkComponentSize, ret.linkComponentByUUID = computeLinkComponents(gatherInfo.DeviceList, anyLinkEdge)
		ret.maxNVLinkComponentSize, ret.nvlinkComponentByUUID = computeLinkComponents(gatherInfo.DeviceList, nvlinkEdge)
		ret.nvlinkComponentToUUIDs = buildComponentIndex(ret.nvlinkComponentByUUID)
		ret.nvlinkRootByOrdinal, ret.nvlinkComponentOrdinal = buildComponentOrdinals(ret.nvlinkComponentToUUIDs, ret.deviceIndexMap)
	}
	if ret.numaTopology {
		ret.maxNUMAGroupSize = computeMaxNUMAGroupSize(gatherInfo.DeviceMap)
	}
	ret.AddPodsUsedResources(infoOption.nodePods, opts...)
	ret.RefreshResourcesData()
	return ret, nil
}

// anyLinkEdge treats any P2P link entry (PCIe-cross-CPU included) as an edge —
// the coarse "are these GPUs reachable at all" relation used for the strict-link
// reachability check (AreDevicesLinked) and node fitness (MaxLinkComponentSize).
func anyLinkEdge(ls []gpuallocator.P2PLink) bool { return len(ls) > 0 }

// nvlinkEdge treats only NVLink links (>= SingleNVLINKLink) as an edge, so the
// resulting components are the NVLink fabrics (NVSwitch domain / NVLink island)
// — the grouping NCCL actually cares about for fast intra-node P2P. This is what
// cross-pod affinity needs: on a full-NVSwitch node all GPUs form one NVLink
// component (any subset is connected → no narrowing needed, correctly), while on
// a 2x4-island node the two islands are distinct components (so a sibling's GPUs
// pin the island its peers must stay in). PCIe-only pairs are NOT edges here.
func nvlinkEdge(ls []gpuallocator.P2PLink) bool {
	for _, l := range ls {
		if l.Type >= links.SingleNVLINKLink {
			return true
		}
	}
	return false
}

// computeLinkComponents runs union-find (Weighted Quick Union with path
// compression) over the GPU link graph and returns both the largest connected
// component size AND a per-UUID component-root map. Two GPUs are unioned when
// isEdge reports an edge between them — pass anyLinkEdge for coarse reachability
// (strict-link / fitness) or nvlinkEdge for NVLink-fabric grouping (cross-pod
// affinity). Link QUALITY ranking still happens inside the bestEffort policy.
//
// Complexity O((V + E)·α(V)) ≈ O(V + E) which is negligible compared to the
// existing topology parsing cost.
func computeLinkComponents(devices gpuallocator.DeviceList, isEdge func([]gpuallocator.P2PLink) bool) (max int, byUUID map[string]int) {
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
		for j, edges := range d.Links {
			if i == j || !isEdge(edges) {
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

// buildComponentIndex inverts linkComponentByUUID (UUID → component root) into
// componentToUUIDs (component root → member UUIDs). Built once at NodeInfo
// construction so cross-pod anchor logic can enumerate a component's cards in
// O(component size) instead of scanning the whole node. Returns nil for an
// empty/nil input so the field stays nil on non-topology nodes.
func buildComponentIndex(byUUID map[string]int) map[int][]string {
	byComponent := make(map[int][]string, len(byUUID))
	if len(byUUID) == 0 {
		return byComponent
	}
	for uuid, root := range byUUID {
		byComponent[root] = append(byComponent[root], uuid)
	}
	return byComponent
}

// buildComponentOrdinals assigns each NVLink component a cross-node-STABLE
// ordinal by ranking components by their minimum Device.Index, then returns
// (rootByOrdinal, componentOrdinal) — the two inverse maps (ordinal→root and
// root→ordinal). On homogeneous nodes (identical GPU↔rail layout) ordinal-k
// denotes the same physical sub-domain on every node, which is what lets
// cross-node gang alignment compare sub-domains across nodes even though
// union-find roots are node-local. Returns empty maps for an empty input.
func buildComponentOrdinals(componentToUUIDs map[int][]string, deviceIndexMap map[string]int) (rootByOrdinal, componentOrdinal map[int]int) {
	rootByOrdinal = make(map[int]int, len(componentToUUIDs))
	componentOrdinal = make(map[int]int, len(componentToUUIDs))
	if len(componentToUUIDs) == 0 {
		return rootByOrdinal, componentOrdinal
	}
	// minIndex(component) = smallest Device.Index among its members. A component
	// whose UUIDs are all absent from deviceIndexMap (defensive — both maps derive
	// from the same node device set, so this should not happen) keeps minIdx at
	// MaxInt so it sorts LAST and never steals a low ordinal from a real component.
	type rootMin struct{ root, min int }
	mins := make([]rootMin, 0, len(componentToUUIDs))
	for root, uuids := range componentToUUIDs {
		minIdx := math.MaxInt
		for _, uuid := range uuids {
			if id, ok := deviceIndexMap[uuid]; ok && id < minIdx {
				minIdx = id
			}
		}
		mins = append(mins, rootMin{root: root, min: minIdx})
	}
	// Rank by min index ascending; break ties by root so ordinals are fully
	// deterministic. (Disjoint components can't share a device, so equal min is
	// impossible in practice — the root tiebreak only guards the degenerate case.)
	sort.Slice(mins, func(i, j int) bool {
		if mins[i].min != mins[j].min {
			return mins[i].min < mins[j].min
		}
		return mins[i].root < mins[j].root
	})
	for ordinal, rm := range mins {
		rootByOrdinal[ordinal] = rm.root
		componentOrdinal[rm.root] = ordinal
	}
	return rootByOrdinal, componentOrdinal
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
	// Component/ordinal maps and nodePods are read-only snapshots; clone the
	// top-level containers while sharing the immutable member slices / pod ptrs.
	nodeInfo.linkComponentByUUID = maps.Clone(n.linkComponentByUUID)
	nodeInfo.nvlinkComponentByUUID = maps.Clone(n.nvlinkComponentByUUID)
	nodeInfo.nvlinkComponentToUUIDs = maps.Clone(n.nvlinkComponentToUUIDs)
	nodeInfo.nvlinkRootByOrdinal = maps.Clone(n.nvlinkRootByOrdinal)
	nodeInfo.nvlinkComponentOrdinal = maps.Clone(n.nvlinkComponentOrdinal)
	nodeInfo.nodePods = slices.Clone(n.nodePods)
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
	if err != nil || predicateTimeNanos <= 0 || predicateTimeNanos >= math.MaxInt64 {
		return false
	}
	// LastTransitionTime is persisted at second precision (RFC3339), so
	// compare in seconds. Same-second is conservatively treated as
	// "condition newer" to avoid double-allocation when ordering is ambiguous.
	if predicateTimeNanos/int64(time.Second) <= condition.LastTransitionTime.Unix() {
		return false
	}
	stuckGracePeriod := StuckGracePeriod
	stuckDuration := time.Since(time.Unix(0, predicateTimeNanos))
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
	infoOption := &NodeInfoOption{}
	for _, opt := range opts {
		opt(infoOption)
	}
	if infoOption.excludedUidSet == nil {
		infoOption.excludedUidSet = sets.New[types.UID]()
	}
	if infoOption.resetPods {
		n.nodePods = make([]*corev1.Pod, 0, len(pods))
	}
	if infoOption.resetUsed {
		n.resetResourceUsage()
	}
	util.PodsOnNodeCallback(pods, n.node, func(pod *corev1.Pod) {
		if !infoOption.excludedUidSet.Has(pod.UID) {
			n.nodePods = append(n.nodePods, pod)
			n.AddPodUsedResources(pod)
		}
	})
}

// PodDeviceFootprint is the peak occupancy a pod imposes on a SINGLE physical
// GPU across its whole lifecycle. See ReducePodFootprint for how container
// lifecycles combine into it.
type PodDeviceFootprint struct {
	Id     int
	Uuid   string
	Number int
	Cores  int64
	Memory int64
}

// ReducePodFootprint collapses a pod's per-container device claims into the
// per-physical-GPU peak occupancy, keyed by GPU UUID, honoring the
// non-overlapping lifecycle of init containers.
//
// Containers fall into three groups by lifecycle:
//   - regular (app) containers: run concurrently in the main phase.
//   - sidecars (restartable init containers): start during the init sequence
//     and keep running for the rest of the pod's life, so they overlap BOTH
//     the main phase and the sequential-init phases.
//   - sequential init containers (non-restartable): run one at a time, before
//     the main phase, never overlapping each other or the app containers.
//
// Per GPU g, with regularSum(g)/sidecarSum(g) the plain per-claim sums of the
// regular and sidecar groups, and initMax(g) the per-dimension max over each
// sequential init container's OWN per-GPU sum, the reserved peak is:
//
//	reserve(g) = sidecarSum(g) + max(regularSum(g), initMax(g))   // per dimension
//
// Sidecars run throughout, so they are a constant addend; the variable part is
// whichever peaks higher — the app phase or the heaviest single sequential
// init phase. This never under-reserves a GPU. (Conservative simplification:
// sidecars are treated as overlapping EVERY sequential-init phase even though
// only those started earlier in the init order truly do; this can slightly
// over-reserve in the unusual "sidecar declared after a vGPU init container"
// ordering, which is safe.) For a pod without a sequential init container this
// collapses to the historical plain per-GPU sum.
func ReducePodFootprint(pod *corev1.Pod, podDeviceClaim PodDeviceClaim) map[string]PodDeviceFootprint {
	// Classify init containers. Sidecars overlap the app phase; the remaining
	// init containers are sequential. Regular containers are everything not in
	// either set.
	var sequentialInit, sidecar map[string]struct{}
	for i := range pod.Spec.InitContainers {
		ic := &pod.Spec.InitContainers[i]
		if util.IsRestartableInitContainer(ic) {
			if sidecar == nil {
				sidecar = make(map[string]struct{})
			}
			sidecar[ic.Name] = struct{}{}
			continue
		}
		if sequentialInit == nil {
			sequentialInit = make(map[string]struct{}, len(pod.Spec.InitContainers))
		}
		sequentialInit[ic.Name] = struct{}{}
	}

	// Fast path: without a sequential init container every claim is concurrent
	// (regular containers and sidecars all run together), so the footprint is
	// the plain per-GPU sum — identical to the historical per-claim accounting.
	if len(sequentialInit) == 0 {
		footprint := make(map[string]PodDeviceFootprint, len(podDeviceClaim))
		for _, containerClaim := range podDeviceClaim {
			for _, claim := range containerClaim.DeviceClaims {
				sumClaimFootprint(footprint, claim)
			}
		}
		return footprint
	}

	// General path: bucket claims into the three groups, then combine.
	regularSum := map[string]PodDeviceFootprint{}
	sidecarSum := map[string]PodDeviceFootprint{}
	initMax := map[string]PodDeviceFootprint{}
	for _, containerClaim := range podDeviceClaim {
		switch {
		case inNameSet(sequentialInit, containerClaim.Name):
			// One sequential init container's footprint is the SUM of ITS OWN
			// claims per GPU (a single running container); take the per-GPU max
			// across sequential init containers since they never overlap.
			perInit := map[string]PodDeviceFootprint{}
			for _, claim := range containerClaim.DeviceClaims {
				sumClaimFootprint(perInit, claim)
			}
			for _, pf := range perInit {
				maxFootprintInto(initMax, pf)
			}
		case inNameSet(sidecar, containerClaim.Name):
			for _, claim := range containerClaim.DeviceClaims {
				sumClaimFootprint(sidecarSum, claim)
			}
		default:
			for _, claim := range containerClaim.DeviceClaims {
				sumClaimFootprint(regularSum, claim)
			}
		}
	}

	result := make(map[string]PodDeviceFootprint, len(regularSum)+len(sidecarSum)+len(initMax))
	combine := func(uuid string) {
		if _, done := result[uuid]; done {
			return
		}
		sc, rg, im := sidecarSum[uuid], regularSum[uuid], initMax[uuid]
		result[uuid] = PodDeviceFootprint{
			Id:     footprintId(rg, im, sc),
			Uuid:   uuid,
			Number: sc.Number + max(rg.Number, im.Number),
			Cores:  sc.Cores + max(rg.Cores, im.Cores),
			Memory: sc.Memory + max(rg.Memory, im.Memory),
		}
	}
	for uuid := range regularSum {
		combine(uuid)
	}
	for uuid := range sidecarSum {
		combine(uuid)
	}
	for uuid := range initMax {
		combine(uuid)
	}
	return result
}

// CurrentSharedContainers counts, per physical GPU UUID, the containers that
// currently hold a device claim on it AND are running right now. Unlike the
// peak count derived from ReducePodFootprint, a terminated container (e.g. a
// completed sequential init container) is excluded, so this reflects the
// instantaneous sharing and is always <= the peak. A container is counted at
// most once per GPU.
func CurrentSharedContainers(pod *corev1.Pod, podDeviceClaim PodDeviceClaim) map[string]int {
	counts := map[string]int{}
	for _, containerClaim := range podDeviceClaim {
		if !util.IsContainerRunning(pod, containerClaim.Name) {
			continue
		}
		var seen map[string]struct{}
		for _, claim := range containerClaim.DeviceClaims {
			if _, ok := seen[claim.Uuid]; ok {
				continue
			}
			if seen == nil {
				seen = make(map[string]struct{}, len(containerClaim.DeviceClaims))
			}
			seen[claim.Uuid] = struct{}{}
			counts[claim.Uuid]++
		}
	}
	return counts
}

func inNameSet(m map[string]struct{}, name string) bool {
	_, ok := m[name]
	return ok
}

// sumClaimFootprint adds one device claim onto the running per-GPU sum.
func sumClaimFootprint(m map[string]PodDeviceFootprint, claim DeviceClaim) {
	f := m[claim.Uuid]
	f.Id, f.Uuid = claim.Id, claim.Uuid
	f.Number++
	f.Cores += claim.Cores
	f.Memory += claim.Memory
	m[claim.Uuid] = f
}

// maxFootprintInto folds one per-GPU footprint into the running per-GPU max.
func maxFootprintInto(m map[string]PodDeviceFootprint, pf PodDeviceFootprint) {
	f, ok := m[pf.Uuid]
	if !ok {
		f.Id, f.Uuid = pf.Id, pf.Uuid
	}
	f.Number = max(f.Number, pf.Number)
	f.Cores = max(f.Cores, pf.Cores)
	f.Memory = max(f.Memory, pf.Memory)
	m[pf.Uuid] = f
}

// footprintId returns the GPU index from the first present footprint (all
// claims on the same UUID carry the same Id).
func footprintId(fs ...PodDeviceFootprint) int {
	for _, f := range fs {
		if f.Uuid != "" {
			return f.Id
		}
	}
	return 0
}

// podHasVGPUInitContainer report whether there are any non sidecar type init containers requesting vGPU.
// It gates the init-aware reduce path so the common pod (no vGPU init
// container) keeps the historical per-claim accounting with no extra cost.
func podHasVGPUInitContainer(pod *corev1.Pod) bool {
	for i := range pod.Spec.InitContainers {
		if util.IsVGPURequiredContainer(&pod.Spec.InitContainers[i]) &&
			!util.IsRestartableInitContainer(&pod.Spec.InitContainers[i]) {
			return true
		}
	}
	return false
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
	if !podHasVGPUInitContainer(pod) {
		// Fast path (historical): without a vGPU init container the per-GPU
		// peak equals the plain per-claim sum, so account claim by claim.
		for _, containerClaim := range podDeviceClaim {
			for _, claim := range containerClaim.DeviceClaims {
				if err := n.AddUsedResources(claim); err != nil {
					klog.Warningf("failed to update used resource for node %s dev %d due to %v", n.name, claim.Id, err)
				}
			}
		}
		return
	}
	// Init-aware path: collapse to the per-GPU lifecycle peak so a sequential
	// init container reusing a regular container's GPU is not double-counted.
	for _, fp := range ReducePodFootprint(pod, podDeviceClaim) {
		if err := n.addFootprintResources(fp); err != nil {
			klog.Warningf("failed to update used resource for node %s dev %d due to %v", n.name, fp.Id, err)
		}
	}
}

func (n *NodeInfo) resetResourceUsage() {
	n.usedNumber, n.usedCores, n.usedMemory = 0, 0, 0
	for _, deviceInfo := range n.deviceMap {
		deviceInfo.ResetUsed()
	}
}

type deviceUsage struct {
	number int
	cores  int64
	memory int64
}

// UsageSnapshot captures a NodeInfo's mutable used-resource counters (node
// aggregate + per-device) so a transient allocation pass can be rolled back.
// Opaque to callers. Used by the allocator's init-container pass to release
// the app-phase reservation before placing sequential init containers (which
// run after the app phase and may reuse its GPUs).
type UsageSnapshot struct {
	nodeNumber int
	nodeCores  int64
	nodeMemory int64
	devices    map[int]deviceUsage
}

// SnapshotUsage captures the current used-resource counters for a later
// RestoreUsage. It does not copy the device list itself (capacities/health
// are immutable here), only the mutable usage counters.
func (n *NodeInfo) SnapshotUsage() *UsageSnapshot {
	snap := &UsageSnapshot{
		nodeNumber: n.usedNumber,
		nodeCores:  n.usedCores,
		nodeMemory: n.usedMemory,
		devices:    make(map[int]deviceUsage, len(n.deviceMap)),
	}
	for id, deviceInfo := range n.deviceMap {
		snap.devices[id] = deviceUsage{
			number: deviceInfo.usedNumber,
			cores:  deviceInfo.usedCores,
			memory: deviceInfo.usedMemory,
		}
	}
	return snap
}

// RestoreUsage rolls the used-resource counters back to a SnapshotUsage value.
// Devices absent from the snapshot are left untouched (the device set is
// stable within a single allocation, so this never happens in practice).
func (n *NodeInfo) RestoreUsage(snap *UsageSnapshot) {
	if snap == nil {
		return
	}
	n.usedNumber, n.usedCores, n.usedMemory = snap.nodeNumber, snap.nodeCores, snap.nodeMemory
	for id, deviceInfo := range n.deviceMap {
		if u, ok := snap.devices[id]; ok {
			deviceInfo.usedNumber, deviceInfo.usedCores, deviceInfo.usedMemory = u.number, u.cores, u.memory
		}
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

// addFootprintResources records an already-reduced per-GPU peak footprint.
// It mirrors AddUsedResources exactly (same UUID lookup, same MIG/healthy
// gate on the node-level aggregate) but adds the aggregated number/cores/
// memory in one shot instead of a single vGPU claim.
func (n *NodeInfo) addFootprintResources(fp PodDeviceFootprint) error {
	deviceId, ok := n.deviceIndexMap[fp.Uuid]
	if !ok {
		return fmt.Errorf("device UUID <%s> does not exist in the NodeInfo <%s>", fp.Uuid, n.name)
	}
	device, ok := n.deviceMap[deviceId]
	if !ok {
		return fmt.Errorf("device ID <%d> does not exist in the NodeInfo <%s>", deviceId, n.name)
	}
	device.addUsedResourcesN(fp.Number, fp.Cores, fp.Memory)
	if !device.IsMIG() && device.Healthy() {
		n.usedNumber += fp.Number
		n.usedCores += fp.Cores
		n.usedMemory += fp.Memory
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

// MaxNVLinkComponentSize returns the largest number of GPUs mutually reachable
// over NVLink ONLY (the biggest NVLink fabric / island). Node fitness ranks a
// node that can host the requested group entirely within one NVLink fabric
// (MaxNVLinkComponentSize >= N) above one that can only reach N over PCIe
// (MaxLinkComponentSize >= N > MaxNVLinkComponentSize). Equals
// MaxLinkComponentSize on a fully NVSwitch-connected node.
func (n *NodeInfo) MaxNVLinkComponentSize() int {
	return n.maxNVLinkComponentSize
}

// LinkTopologyFitness scores how well this node can host a link-topology group
// of needNumber GPUs, higher = better NCCL performance:
//
//	3 = fits within ONE NVLink fabric (MaxNVLinkComponentSize >= N) — best
//	2 = fits within a P2P-reachable group but spans NVLink islands over PCIe
//	1 = has topology but can't fit even a P2P group (allocator will fall back)
//	0 = no GPU topology info
//
// Tiers 0/1/2 preserve the prior ranking (topology-capable above non-topology);
// tier 3 is the finer split that pulls fully-NVLink-connectable nodes to the
// front. On a homogeneous NVSwitch cluster every candidate is tier 3 (== the old
// uniform "fits" tier), so downstream binpack/spread ordering is unchanged.
func (n *NodeInfo) LinkTopologyFitness(needNumber int) int {
	if !n.gpuTopology {
		return 0
	}
	switch {
	case n.maxNVLinkComponentSize >= needNumber:
		return 3
	case n.maxLinkComponentSize >= needNumber:
		return 2
	default:
		return 1
	}
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

// ComponentUUIDs returns the UUIDs of every GPU in the given NVLink-fabric
// component (root index from nvlinkComponentByUUID), or nil for an unknown root.
// Used by cross-pod allocation to window the candidate set to an anchored
// NVLink island.
func (n *NodeInfo) ComponentUUIDs(root int) []string {
	return n.nvlinkComponentToUUIDs[root]
}

// ComponentByOrdinal returns the NVLink-component root that has the given
// cross-node ordinal on this node, or (-1, false) when this node has no such
// ordinal (e.g. fewer NVLink sub-domains than the requested ordinal).
func (n *NodeInfo) ComponentByOrdinal(ordinal int) (int, bool) {
	if ordinal < 0 {
		return -1, false
	}
	root, ok := n.nvlinkRootByOrdinal[ordinal]
	if !ok {
		return -1, false
	}
	return root, true
}

// OrdinalOfUUIDs resolves the cross-node-stable ordinal (rail) of a set of GPU
// UUIDs **on this node**, i.e. it must be called on the NodeInfo of the node the
// UUIDs actually live on (the sibling's own node). Each UUID maps to its
// NVLink-fabric component root via nvlinkComponentByUUID and then to that root's
// ordinal; the majority ordinal is returned. This is identity-based (UUID), so
// it does NOT depend on the possibly-stale Device.Index recorded in a pre-alloc
// annotation. Duplicate UUIDs (a pod's multiple containers sharing one card) are
// collapsed. Returns (-1, false) when no UUID is known here or no NVLink topology.
func (n *NodeInfo) OrdinalOfUUIDs(uuids []string) (int, bool) {
	if len(uuids) == 0 || len(n.nvlinkComponentByUUID) == 0 {
		return -1, false
	}
	seen := make(map[string]struct{}, len(uuids))
	votes := make(map[int]int)
	for _, uuid := range uuids {
		if _, dup := seen[uuid]; dup {
			continue
		}
		seen[uuid] = struct{}{}
		root, ok := n.nvlinkComponentByUUID[uuid]
		if !ok {
			continue
		}
		if ord, ok := n.nvlinkComponentOrdinal[root]; ok {
			votes[ord]++
		}
	}
	if len(votes) == 0 {
		return -1, false
	}
	bestOrd, bestVotes := -1, 0
	for ord, v := range votes {
		if v > bestVotes || (v == bestVotes && (bestOrd == -1 || ord < bestOrd)) {
			bestOrd, bestVotes = ord, v
		}
	}
	return bestOrd, true
}

// GangAnchorComponent resolves the NVLink component that this gang's sibling
// pods have already pre-allocated on this node, so a later sibling can keep its
// GPUs in the same connected component. Returns (root, true) when at least one
// live sibling pre-allocation maps to a known component; (-1, false) for the
// gang's first pod on the node, for non-gang pods, or on non-topology nodes —
// in which case the caller takes the unchanged, non-anchored path.
//
// Only siblings whose pre-allocation still counts (ShouldCountPodDeviceAllocation)
// vote, matching the usage accounting that built this NodeInfo. If siblings span
// more than one component (connectivity already degraded — e.g. an earlier
// sibling fell back across components), the majority component is chosen and the
// split is logged at V(3) rather than failing here.
func (n *NodeInfo) GangAnchorComponent(gangName string, excludedSet sets.Set[types.UID]) (int, bool) {
	if gangName == "" || len(n.nvlinkComponentByUUID) == 0 || len(n.nodePods) == 0 {
		return -1, false
	}
	votes := make(map[int]int)
	for _, p := range n.nodePods {
		if p == nil || excludedSet.Has(p.UID) {
			continue
		}
		if name, ok := util.PodHasGangName(p); !ok || name != gangName {
			continue
		}
		if !ShouldCountPodDeviceAllocation(p) {
			continue
		}
		for _, uuid := range PodPreAllocatedUUIDs(p) {
			if root, ok := n.nvlinkComponentByUUID[uuid]; ok {
				votes[root]++
			}
		}
	}
	if len(votes) == 0 {
		return -1, false
	}
	bestRoot, bestVotes := -1, 0
	for root, v := range votes {
		if v > bestVotes || (v == bestVotes && (bestRoot == -1 || root < bestRoot)) {
			bestRoot, bestVotes = root, v
		}
	}
	if len(votes) > 1 {
		klog.V(3).Infof("gang %q anchor split across %d NVLink components on node %s; using majority root %d",
			gangName, len(votes), n.name, bestRoot)
	}
	return bestRoot, true
}

// PodPreAllocatedUUIDs extracts the DEDUPLICATED GPU UUIDs from a pod's
// pre-allocated device annotation. UUID (not the possibly-stale Device.Index) is
// the stable device identity; duplicates appear when a pod's multiple containers
// share one card, so they are collapsed. Returns nil when the annotation is
// absent or unparsable (a malformed sibling simply doesn't vote — never an error
// that blocks the pod being scheduled).
func PodPreAllocatedUUIDs(pod *corev1.Pod) []string {
	val, ok := util.HasAnnotation(pod, util.PodVGPUPreAllocAnnotation)
	if !ok || len(val) == 0 {
		return nil
	}
	var pdc PodDeviceClaim
	if err := pdc.UnmarshalText(val); err != nil {
		return nil
	}
	seen := make(map[string]struct{})
	var uuids []string
	for _, cdc := range pdc {
		for _, dc := range cdc.DeviceClaims {
			if dc.Uuid == "" {
				continue
			}
			if _, dup := seen[dc.Uuid]; dup {
				continue
			}
			seen[dc.Uuid] = struct{}{}
			uuids = append(uuids, dc.Uuid)
		}
	}
	return uuids
}

// MaxNUMAGroupSize returns the largest number of GPUs sharing a single NUMA
// node. NUMA-mode sorting checks this against the pod's request so we avoid
// ranking nodes high that "have NUMA topology" but couldn't actually fit a
// same-NUMA group.
func (n *NodeInfo) MaxNUMAGroupSize() int {
	return n.maxNUMAGroupSize
}
