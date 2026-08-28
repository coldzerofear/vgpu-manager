/*
Copyright 2026 coldzerofear

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package collector

import (
	"context"
	"fmt"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"

	nvdev "github.com/NVIDIA/go-nvlib/pkg/nvlib/device"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/coldzerofear/vgpu-manager/pkg/claimresolve"
	"github.com/coldzerofear/vgpu-manager/pkg/client"
	"github.com/coldzerofear/vgpu-manager/pkg/config/node"
	"github.com/coldzerofear/vgpu-manager/pkg/config/watcher"
	"github.com/coldzerofear/vgpu-manager/pkg/device/nvidia"
	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin"
	"github.com/coldzerofear/vgpu-manager/pkg/metrics"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/coldzerofear/vgpu-manager/pkg/util/cgroup"
	"github.com/opencontainers/cgroups"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/exp/maps"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	listerv1 "k8s.io/client-go/listers/core/v1"
	resourcev1 "k8s.io/client-go/listers/resource/v1"
	"k8s.io/component-base/featuregate"
	"k8s.io/klog/v2"
)

// draGPUCollector implements the Collector interface.
//
// It reports the same metric set as nodeGPUCollector, but sources the
// allocation view from DRA API objects instead of the device-plugin's
// annotations and per-container shared-memory files:
//
//	device inventory / ratios / health -> ResourceSlice published by our driver
//	container <-> device allocation    -> ResourceClaim allocation results
//	physical + per-process metrics     -> NVML and cgroup (identical; see CollectBasedOnNvml)
//
// Two things the device-plugin path has are structurally unavailable here:
// the per-container resource-data file (so limits come from the allocation's
// ConsumedCapacity) and the virtual-memory ledger (so containerVGPUMemoryUsage
// currently equals the physical usage; see the VirtualMemoryTracking branch below).
type draGPUCollector struct {
	*nvidia.DeviceLib
	nodeName    string
	nodeLister  listerv1.NodeLister
	podLister   client.PodLister
	sliceLister resourcev1.ResourceSliceLister
	claimLister resourcev1.ResourceClaimLister
	utilAdapter watcher.DeviceUtilInterface
	featureGate featuregate.FeatureGate
}

func NewDRAGPUCollector(
	config *node.NodeConfigSpec, nodeLister listerv1.NodeLister, podLister client.PodLister,
	sliceLister resourcev1.ResourceSliceLister, claimLister resourcev1.ResourceClaimLister,
	featureGate featuregate.FeatureGate,
) (prometheus.Collector, error) {
	driverRoot := config.GetDriverRoot()
	deviceLib, err := nvidia.DetectionDeviceLib(driverRoot)
	if err != nil {
		return nil, err
	}
	adapter := watcher.NewDeviceUtilAdapter(
		watcher.WithExtendedInterface(deviceLib.Extensions()),
	)
	return &draGPUCollector{
		DeviceLib:   deviceLib,
		nodeName:    config.GetNodeName(),
		nodeLister:  nodeLister,
		podLister:   podLister,
		sliceLister: sliceLister,
		claimLister: claimLister,
		featureGate: featureGate,
		utilAdapter: adapter,
	}, nil
}

// Describe is implemented with DescribeByCollect. That's possible because the
// Collect method will always return the same two metrics with the same two
// descriptors.
func (c draGPUCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- physicalGPUTotalMemory
	ch <- physicalGPUMemoryUsage
	ch <- physicalGPUMemoryUtilRate
	ch <- physicalGPUCoreUtilRate
	ch <- physicalGPUHealthStatus
	ch <- nodeGPUConfigInfo
	ch <- nodeGPUDriverVersionInfo
	ch <- nodeVGPUTotalMemory
	ch <- nodeVGPUTotalPhysicalMemory
	ch <- nodeVGPUAssignedMemory
	ch <- nodeVGPUAssignedPhysicalMemory
	ch <- vGPUTotalCoresNumber
	ch <- vGPUAssignedCoresNumber
	ch <- vGPUPeakSharedContainersNumber
	ch <- vGPUCurrentSharedContainersNumber
	ch <- vGPUTotalMemory
	ch <- vGPUTotalPhysicalMemory
	ch <- vGPUAssignedMemory
	ch <- vGPUAssignedPhysicalMemory
	ch <- containerVGPUMemoryUsage
	ch <- containerVGPUPhysicalMemoryUsage
	ch <- containerVGPUMemoryLimit
	ch <- containerVGPUPhysicalMemoryLimit
	ch <- containerVGPUMemoryUtilRate
	ch <- containerVGPUCoreUtilRate
	// MIG devices are not reported on the DRA path yet; see node_gpu.go for the
	// device-plugin equivalent.
	//ch <- migDeviceTotalMemory
	//ch <- migDeviceMemoryUsage
	//ch <- migDeviceMemoryUtilRate
	//ch <- containerMIGAllocationInfo
}

// listManagerResourceSlices returns every ResourceSlice this node's driver
// published. There can be more than one: generateCombinedResourceSlices emits
// one slice PER GPU, and the split variant emits G+1 (a SharedCounters slice
// plus one per GPU). Reading only the first would silently report a single GPU
// on any node using the partitionable-device layout.
func (c draGPUCollector) listManagerResourceSlices() ([]*v1.ResourceSlice, error) {
	sliceList, err := c.sliceLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}
	// Not named `slices`: that would shadow the stdlib package imported above.
	var nodeSlices []*v1.ResourceSlice
	for _, slice := range sliceList {
		if slice.Spec.Driver != util.DRADriverName {
			continue
		}
		// A node-local slice carries the node it belongs to. Slices without one
		// are not node-local and must not be attributed to us.
		if slice.Spec.NodeName == nil || *slice.Spec.NodeName != c.nodeName {
			continue
		}
		nodeSlices = append(nodeSlices, slice)
	}
	if len(nodeSlices) == 0 {
		return nil, fmt.Errorf("no resourceSlice published by driver %q for node %q", util.DRADriverName, c.nodeName)
	}
	return nodeSlices, nil
}

func CollectBasedOnNvml(
	ch chan<- prometheus.Metric, lib *nvidia.DeviceLib, nodeName string,
	devTypeMap map[string]string, devIndexMap map[string]int, devHealthMap map[string]int,
	devHealthLvs map[string][]string, devMemInfoMap map[string]nvml.Memory,
	devProcInfoMap map[string]procInfoList, devProcUtilMap map[string]procUtilList,
	devMigInfosMap map[string][]*nvidia.MigInfo,
	utilAdapter watcher.DeviceUtilInterface, featureGate featuregate.FeatureGate,
) {
	err := lib.NvmlInit()
	if err != nil {
		klog.Errorln(err)
		return
	}
	var deviceUtil *watcher.MmapDeviceUtil
	defer func() {
		lib.NvmlShutdown()
		if deviceUtil != nil {
			_ = deviceUtil.Close()
		}
	}()

	func() {
		driverVersion, ret := lib.SystemGetDriverVersion()
		if ret != nvml.SUCCESS {
			klog.Errorf("error getting driver version: %s", nvml.ErrorString(ret))
			driverVersion = "N/A"
		}
		cudaVersion := ""
		version, ret := lib.SystemGetCudaDriverVersion()
		if ret != nvml.SUCCESS {
			klog.Errorf("error getting CUDA driver version: %s", nvml.ErrorString(ret))
			cudaVersion = "N/A"
		} else {
			cudaVersion = strconv.Itoa(version)
		}
		nvmlVersion, ret := lib.SystemGetNVMLVersion()
		if ret != nvml.SUCCESS {
			klog.Errorf("error getting NVML driver version: %s", nvml.ErrorString(ret))
			nvmlVersion = "N/A"
		}
		ch <- prometheus.MustNewConstMetric(nodeGPUDriverVersionInfo,
			prometheus.GaugeValue, float64(1), nodeName, driverVersion, cudaVersion, nvmlVersion)
	}()

	if featureGate.Enabled(util.SharedSMUtilizationWatcher) {
		if deviceUtil, err = watcher.NewMmapDeviceUtil(smFilePath); err != nil && !os.IsNotExist(err) {
			klog.V(3).ErrorS(err, "Failed to read manager SM util file")
		}
	}

	err = lib.VisitDevices(func(index int, hdev nvdev.Device) error {
		gpuInfo, err := lib.GetGpuInfo(index, hdev)
		if err != nil {
			klog.Errorf("error getting info for GPU %d: %v", index, err)
			return nil
		}
		devHealthMap[gpuInfo.UUID]++
		devIndexMap[gpuInfo.UUID] = index
		devTypeMap[gpuInfo.UUID] = gpuInfo.ProductName
		devMemInfoMap[gpuInfo.UUID] = gpuInfo.Memory
		migEnabled := fmt.Sprint(gpuInfo.MigEnabled)

		var numaNode string
		if numa := gpuInfo.GetNumaNode(); numa >= 0 {
			numaNode = strconv.Itoa(int(numa))
		}
		deviceIndex := strconv.Itoa(index)
		minorNumber := strconv.Itoa(gpuInfo.Minor)
		devHealthLvs[gpuInfo.UUID] = []string{
			nodeName, deviceIndex, gpuInfo.UUID, gpuInfo.ProductName, gpuInfo.PciBusID,
			minorNumber, migEnabled, gpuInfo.CudaComputeCapability, numaNode,
		}

		ch <- prometheus.MustNewConstMetric(physicalGPUTotalMemory,
			prometheus.GaugeValue, float64(gpuInfo.Memory.Total), devHealthLvs[gpuInfo.UUID]...)

		ch <- prometheus.MustNewConstMetric(physicalGPUMemoryUsage,
			prometheus.GaugeValue, float64(gpuInfo.Memory.Used), devHealthLvs[gpuInfo.UUID]...)

		memoryUtilRate := int64(0)
		if gpuInfo.Memory.Total > 0 {
			memoryUtilRate = int64(float64(gpuInfo.Memory.Used) / float64(gpuInfo.Memory.Total) * 100)
		}
		ch <- prometheus.MustNewConstMetric(physicalGPUMemoryUtilRate,
			prometheus.GaugeValue, float64(memoryUtilRate), devHealthLvs[gpuInfo.UUID]...)

		migInfos, err := lib.GetMigInfos(gpuInfo)
		if err != nil {
			klog.Errorf("error getting MIG infos for GPU %d: %v", index, err)
		}
		if len(migInfos) > 0 {
			devMigInfosMap[gpuInfo.UUID] = maps.Values[map[string]*nvidia.MigInfo](migInfos)
		}

		// Skip unsupported operations after enabling MIG.
		if gpuInfo.MigEnabled {
			return nil
		}

		// On MIG-enabled GPUs, querying device utilization rates is not currently supported.
		deviceUtilRates, rt := hdev.GetUtilizationRates()
		if rt != nvml.SUCCESS {
			klog.Errorf("error getting utilization rates for device %d: %s", index, nvml.ErrorString(rt))
		} else {
			ch <- prometheus.MustNewConstMetric(physicalGPUCoreUtilRate,
				prometheus.GaugeValue, float64(deviceUtilRates.Gpu), devHealthLvs[gpuInfo.UUID]...)
		}

		CollectorDeviceProcesses(utilAdapter, deviceUtil, index, hdev, devProcInfoMap, devProcUtilMap)
		return nil
	})
	if err != nil {
		klog.Errorln(err.Error())
	}
}

type DRADeviceInfo struct {
	name        string
	devType     string
	uuid        string
	coreRatio   int64
	memoryRatio int64
	cores       int64
	memory      int64
	healthy     bool
}

// memoryOversubscription returns the device's memory oversubscription factor,
// i.e. announced-memory / physical-memory. Values <= 1 mean the announced
// memory is backed 1:1 (or under-committed) by real VRAM.
func (d *DRADeviceInfo) memoryOversubscription() float64 {
	return float64(d.memoryRatio) / float64(util.HundredCore)
}

// deviceUUIDFromAttribute converts a ResourceSlice `uuid` attribute back to the
// NVML spelling used everywhere else in this collector.
//
// The driver publishes strings.ToLower(nvmlUUID) (GpuDeviceInfo.Attributes), and
// NVML renders the body as lowercase hex, so re-upcasing only the prefix before
// the first '-' ("gpu-5e4b..." -> "GPU-5e4b...") is the exact inverse. Without
// this the UUID never matches devIndexMap/devHealthLvs and the device silently
// drops out of every per-device metric.
func deviceUUIDFromAttribute(value string) string {
	idx := strings.Index(value, "-")
	if idx < 0 {
		return value
	}
	return strings.ToUpper(value[:idx]) + value[idx:]
}

// consumedInt64 reads an int64 out of an allocation result's ConsumedCapacity.
//
// The presence check matters: a zero-valued resource.Quantity returns
// (0, true) from AsInt64(), so indexing the map directly turns "this device
// type has no consumable capacity" into "the container was allocated 0", which
// silently zeroes every derived limit and utilisation percentage.
func consumedInt64(result v1.DeviceRequestAllocationResult, name v1.QualifiedName) (int64, bool) {
	quantity, ok := result.ConsumedCapacity[name]
	if !ok {
		return 0, false
	}
	value, ok := quantity.AsInt64()
	if !ok || value < 0 {
		return 0, false
	}
	return value, true
}

// capacityInt64 reads an int64 out of a device's advertised Capacity, with the
// same presence semantics as consumedInt64.
func capacityInt64(capacity map[v1.QualifiedName]v1.DeviceCapacity, name v1.QualifiedName) (int64, bool) {
	entry, ok := capacity[name]
	if !ok {
		return 0, false
	}
	value, ok := entry.Value.AsInt64()
	if !ok || value < 0 {
		return 0, false
	}
	return value, true
}

// draAllocResult is one allocation result resolved against the node's
// ResourceSlice: which device it landed on, and how much of it the consuming
// container actually holds.
type draAllocResult struct {
	request string
	device  string
	devInfo *DRADeviceInfo
	// cores/memory are the amounts CONSUMED by this result. For a consumable
	// (vGPU) device they come from ConsumedCapacity; for a whole-GPU device,
	// which is allocated exclusively and advertises no request policy, they
	// fall back to the device's full capacity.
	cores  int64
	memory int64
}

// draContainerAlloc is one container's resolved allocation set, carrying the
// lifecycle classification needed to fold overlapping containers correctly.
type draContainerAlloc struct {
	name        string
	kind        util.ContainerKind
	restartable bool
	results     []draAllocResult
}

// draFootprint mirrors device.PodDeviceFootprint for the DRA path.
type draFootprint struct {
	number int
	cores  int64
	memory int64
}

func draAddFootprint(m map[string]draFootprint, uuid string, cores, memory int64) {
	f := m[uuid]
	f.number++
	f.cores += cores
	f.memory += memory
	m[uuid] = f
}

func draMaxFootprintInto(m map[string]draFootprint, uuid string, in draFootprint) {
	f := m[uuid]
	f.number = max(f.number, in.number)
	f.cores = max(f.cores, in.cores)
	f.memory = max(f.memory, in.memory)
	m[uuid] = f
}

// reduceDRAPodFootprint collapses one pod's per-container allocations into the
// per-GPU LIFECYCLE PEAK, mirroring device.ReducePodFootprint on the
// device-plugin path.
//
// Why a plain sum is wrong: the resourceclaim webhook explicitly permits an
// init container and an app container to reference the same vGPU request
// (see validateOneReservedPodAgainstAllocatedClaim). Those two never run at the
// same time, so summing them would double-count both the assigned memory and
// the assigned cores of every pod that warms up a GPU in an init container --
// the intended pattern, not an edge case.
//
// The three lifecycle classes and how they combine:
//   - sidecars (restartable init) run through the whole app phase, so they
//     overlap everything and are summed;
//   - app containers run concurrently with each other, so they are summed;
//   - sequential init containers never overlap each other or the app phase, so
//     the per-GPU MAX across them is taken and then folded against the app sum.
func reduceDRAPodFootprint(containers []draContainerAlloc) map[string]draFootprint {
	regularSum := map[string]draFootprint{}
	sidecarSum := map[string]draFootprint{}
	initMax := map[string]draFootprint{}

	for _, alloc := range containers {
		switch {
		case alloc.restartable:
			for _, r := range alloc.results {
				draAddFootprint(sidecarSum, r.devInfo.uuid, r.cores, r.memory)
			}
		case alloc.kind == util.ContainerKindInit:
			// One sequential init container's footprint is the sum of ITS OWN
			// results per GPU; take the per-GPU max across them.
			perInit := map[string]draFootprint{}
			for _, r := range alloc.results {
				draAddFootprint(perInit, r.devInfo.uuid, r.cores, r.memory)
			}
			for uuid, f := range perInit {
				draMaxFootprintInto(initMax, uuid, f)
			}
		default:
			for _, r := range alloc.results {
				draAddFootprint(regularSum, r.devInfo.uuid, r.cores, r.memory)
			}
		}
	}

	result := make(map[string]draFootprint, len(regularSum)+len(sidecarSum)+len(initMax))
	combine := func(uuid string) {
		if _, done := result[uuid]; done {
			return
		}
		sc, rg, im := sidecarSum[uuid], regularSum[uuid], initMax[uuid]
		result[uuid] = draFootprint{
			number: sc.number + max(rg.number, im.number),
			cores:  sc.cores + max(rg.cores, im.cores),
			memory: sc.memory + max(rg.memory, im.memory),
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

// claimAllocation is the per-claim view the container resolution needs: which
// main requests this driver actually got devices for, and the results behind
// each of them.
type claimAllocation struct {
	resultsByMainRequest map[string][]v1.DeviceRequestAllocationResult
	allocatedRequests    sets.Set[string]
}

// resolvePodAllocations maps every container of the pod to the allocation
// results of this driver that it actually references.
//
// The claimRef -> request resolution deliberately reuses pkg/claimresolve, the
// same code the resourceclaim webhook validates with. Re-deriving it here (for
// instance by splitting result.Request on '/') diverges on FirstAvailable
// subrequests and on claims created from a template, which would make the
// exporter disagree with the admission rules about which container owns a GPU.
func (c draGPUCollector) resolvePodAllocations(
	pod *corev1.Pod, devInfoNameMap map[string]*DRADeviceInfo,
) []draContainerAlloc {
	// Claims are resolved at most once per pod claim name; a nil entry is a
	// remembered negative result.
	claimCache := map[string]*claimAllocation{}

	loadClaim := func(podClaimName string) *claimAllocation {
		if cached, ok := claimCache[podClaimName]; ok {
			return cached
		}
		claimCache[podClaimName] = nil

		actualName, ok, err := claimresolve.ResolveActualClaimNameForPodClaim(pod, podClaimName)
		if err != nil {
			klog.V(4).ErrorS(err, "resolve actual claim name failed", "pod",
				klog.KObj(pod), "podClaim", podClaimName)
			return nil
		}
		if !ok {
			// The claim has not been created/bound yet.
			return nil
		}
		claim, err := c.claimLister.ResourceClaims(pod.Namespace).Get(actualName)
		if err != nil {
			klog.V(4).ErrorS(err, "get resourceClaim failed", "resourceClaim",
				fmt.Sprintf("%s/%s", pod.Namespace, actualName))
			return nil
		}
		if claim.Status.Allocation == nil {
			return nil
		}
		// Only count a claim this pod actually holds. A claim can outlive one
		// consumer and be reserved for another pod entirely.
		if !slices.ContainsFunc(claim.Status.ReservedFor, func(r v1.ResourceClaimConsumerReference) bool { return r.UID == pod.GetUID() }) {
			return nil
		}

		// Maps "request" and "request/subrequest" alike onto the main request.
		index := claimresolve.BuildAllocatedResultIndex(context.Background(), claim, nil, nil)
		allocation := &claimAllocation{
			resultsByMainRequest: map[string][]v1.DeviceRequestAllocationResult{},
			allocatedRequests:    sets.New[string](),
		}
		for _, result := range claim.Status.Allocation.Devices.Results {
			if result.Driver != util.DRADriverName {
				continue
			}
			meta, ok := index[result.Request]
			if !ok {
				klog.V(5).InfoS("allocation result has no matching request in claim spec",
					"resourceClaim", klog.KObj(claim), "request", result.Request)
				continue
			}
			allocation.resultsByMainRequest[meta.MainRequest] = append(allocation.resultsByMainRequest[meta.MainRequest], result)
			allocation.allocatedRequests.Insert(meta.MainRequest)
		}
		claimCache[podClaimName] = allocation
		return allocation
	}

	// appendResult resolves one allocation result against the node's slice and
	// appends it, skipping duplicates. A container can reach the same result
	// through several claimRefs, and the extended-resource path below can
	// re-surface a result the regular path already produced.
	appendResult := func(dst []draAllocResult, seen sets.Set[string], result v1.DeviceRequestAllocationResult) []draAllocResult {
		devInfo, ok := devInfoNameMap[result.Device]
		if !ok {
			// Not a device we report on (MIG, VFIO, or a slice/NVML mismatch).
			return dst
		}
		key := result.Request + "|" + result.Device
		if result.ShareID != nil {
			key += "|" + string(*result.ShareID)
		}
		if seen.Has(key) {
			return dst
		}
		seen.Insert(key)

		cores := devInfo.cores
		if value, ok := consumedInt64(result, kubeletplugin.CoresResourceName); ok {
			cores = value
		}
		memory := devInfo.memory
		if value, ok := consumedInt64(result, kubeletplugin.MemoryResourceName); ok {
			memory = value
		}
		return append(dst, draAllocResult{
			request: result.Request,
			device:  result.Device,
			devInfo: devInfo,
			cores:   cores,
			memory:  memory,
		})
	}

	containerRefs := util.GetAllPodContainers(pod)
	allocs := make([]draContainerAlloc, 0, len(containerRefs))
	seenByContainer := make(map[string]sets.Set[string], len(containerRefs))
	indexByName := make(map[string]int, len(containerRefs))

	for _, ref := range containerRefs {
		seen := sets.New[string]()
		var results []draAllocResult
		for _, claimRef := range ref.Claims {
			allocation := loadClaim(claimRef.Name)
			if allocation == nil {
				continue
			}
			for _, mainRequest := range claimresolve.ResolveActualAllocatedRequestsForClaimRef(claimRef, allocation.allocatedRequests) {
				for _, result := range allocation.resultsByMainRequest[mainRequest] {
					results = appendResult(results, seen, result)
				}
			}
		}
		indexByName[ref.Name] = len(allocs)
		seenByContainer[ref.Name] = seen
		allocs = append(allocs, draContainerAlloc{
			name:        ref.Name,
			kind:        ref.Kind,
			restartable: ref.Restartable,
			results:     results,
		})
	}

	// Extended resources backed by DRA are mapped to containers by the
	// scheduler rather than by a claimRef, so they are resolved separately --
	// through the same dedup set, so a request reachable both ways is counted
	// once.
	if status := pod.Status.ExtendedResourceClaimStatus; status != nil {
		claim, err := c.claimLister.ResourceClaims(pod.Namespace).Get(status.ResourceClaimName)
		switch {
		case err != nil:
			klog.V(4).ErrorS(err, "get extended resourceClaim failed", "resourceClaim",
				fmt.Sprintf("%s/%s", pod.Namespace, status.ResourceClaimName))
		case claim.Status.Allocation == nil:
		case !slices.ContainsFunc(claim.Status.ReservedFor, func(r v1.ResourceClaimConsumerReference) bool { return r.UID == pod.GetUID() }):
		default:
			for _, mapping := range status.RequestMappings {
				idx, ok := indexByName[mapping.ContainerName]
				if !ok {
					continue
				}
				for _, result := range claim.Status.Allocation.Devices.Results {
					if result.Driver != util.DRADriverName || result.Request != mapping.RequestName {
						continue
					}
					allocs[idx].results = appendResult(allocs[idx].results, seenByContainer[mapping.ContainerName], result)
				}
			}
		}
	}

	return allocs
}

// Collect device indicators
func (c draGPUCollector) Collect(ch chan<- prometheus.Metric) {
	klog.V(4).Infof("Starting to collect metrics for vGPU on node <%s>", c.nodeName)
	var (
		devTypeMap     = make(map[string]string)
		devIndexMap    = make(map[string]int)
		devHealthMap   = make(map[string]int)
		devHealthLvs   = make(map[string][]string)
		devMemInfoMap  = make(map[string]nvml.Memory)
		devProcInfoMap = make(map[string]procInfoList)
		devProcUtilMap = make(map[string]procUtilList)
		devMigInfosMap = make(map[string][]*nvidia.MigInfo)
	)

	CollectBasedOnNvml(ch, c.DeviceLib, c.nodeName, devTypeMap, devIndexMap, devHealthMap, devHealthLvs,
		devMemInfoMap, devProcInfoMap, devProcUtilMap, devMigInfosMap, c.utilAdapter, c.featureGate)

	// Get current node.
	node, err := c.nodeLister.Get(c.nodeName)
	if err != nil {
		klog.Errorf("node lister get node <%s> error: %v", c.nodeName, err)
		return
	}

	// Retrieve the vGPU resourceSlices of the current node.
	resourceSlices, err := c.listManagerResourceSlices()
	if err != nil {
		klog.Errorf("resourceSlice get node %q error: %v", c.nodeName, err)
		return
	}

	var (
		nodeGPUTotalMemBytes  uint64
		nodeVGPUTotalMemBytes uint64
		devInfoUuidMap        = make(map[string]*DRADeviceInfo)
		devInfoNameMap        = make(map[string]*DRADeviceInfo)
	)

	for _, resourceSlice := range resourceSlices {
		for i := range resourceSlice.Spec.Devices {
			dev := &resourceSlice.Spec.Devices[i]
			attribute, ok := dev.Attributes["type"]
			if !ok || attribute.StringValue == nil {
				continue
			}
			devType := *attribute.StringValue
			if devType != kubeletplugin.VGpuDeviceType && devType != kubeletplugin.GpuDeviceType {
				continue
			}
			devInfo := &DRADeviceInfo{
				name:        dev.Name,
				devType:     devType,
				coreRatio:   util.HundredCore,
				memoryRatio: util.HundredCore,
				cores:       util.HundredCore,
			}
			if attribute, ok = dev.Attributes["uuid"]; ok && attribute.StringValue != nil {
				devInfo.uuid = deviceUUIDFromAttribute(*attribute.StringValue)
			}
			if devInfo.uuid == "" {
				// Without a UUID the device cannot be joined to NVML data, and
				// keying maps on "" would collapse every such device into one.
				klog.V(4).InfoS("skip resourceSlice device without uuid attribute", "device", dev.Name)
				continue
			}
			if attribute, ok = dev.Attributes["coreRatio"]; ok && attribute.IntValue != nil {
				devInfo.coreRatio = *attribute.IntValue
			}
			if attribute, ok = dev.Attributes["memoryRatio"]; ok && attribute.IntValue != nil {
				devInfo.memoryRatio = *attribute.IntValue
			}
			if devInfo.healthy = kubeletplugin.IsHealthy(dev.Taints); !devInfo.healthy {
				devHealthMap[devInfo.uuid] = 0
			}
			if value, ok := capacityInt64(dev.Capacity, kubeletplugin.CoresResourceName); ok {
				devInfo.cores = value
			}
			if value, ok := capacityInt64(dev.Capacity, kubeletplugin.MemoryResourceName); ok {
				devInfo.memory = value
			}
			// Both device types contribute to the node's announced vGPU memory:
			// a whole-GPU device announces its full VRAM, a vGPU device
			// announces VRAM x memoryRatio.
			nodeVGPUTotalMemBytes += uint64(devInfo.memory)
			if memory, exists := devMemInfoMap[devInfo.uuid]; exists {
				nodeGPUTotalMemBytes += memory.Total
			} else if ratio := devInfo.memoryOversubscription(); ratio > 1 {
				nodeGPUTotalMemBytes += uint64(float64(devInfo.memory) / ratio)
			} else {
				nodeGPUTotalMemBytes += uint64(devInfo.memory)
			}
			devInfoUuidMap[devInfo.uuid] = devInfo
			devInfoNameMap[devInfo.name] = devInfo
		}
	}

	for uuid, status := range devHealthMap {
		labelValues, ok := devHealthLvs[uuid]
		if !ok {
			// A device announced in the ResourceSlice that NVML did not
			// enumerate has no label set. Emitting with a nil slice trips the
			// label-cardinality check inside MustNewConstMetric, and prometheus
			// runs Collect on its own goroutine without recovering -- so this
			// would take the whole exporter down on every scrape. Reachable
			// whenever NvmlInit fails, since CollectBasedOnNvml then returns
			// with every map still empty.
			klog.V(4).InfoS("skip health metric for device with no NVML labels", "deviceUuid", uuid)
			continue
		}
		ch <- prometheus.MustNewConstMetric(
			physicalGPUHealthStatus, prometheus.GaugeValue, float64(status), labelValues...)
	}

	ch <- prometheus.MustNewConstMetric(
		nodeVGPUTotalMemory, prometheus.GaugeValue, float64(nodeVGPUTotalMemBytes), c.nodeName)

	ch <- prometheus.MustNewConstMetric(
		nodeVGPUTotalPhysicalMemory, prometheus.GaugeValue, float64(nodeGPUTotalMemBytes), c.nodeName)

	// The ratios are per-device attributes, but the node-level config metric
	// carries a single pair. They are derived from one driver-wide config, so
	// in practice every device agrees; pick deterministically (lowest device
	// name) rather than by map order, and say so if they ever diverge.
	coreRatio, memoryRatio := int64(util.HundredCore), int64(util.HundredCore)
	if names := maps.Keys(devInfoNameMap); len(names) > 0 {
		sort.Strings(names)
		first := devInfoNameMap[names[0]]
		coreRatio, memoryRatio = first.coreRatio, first.memoryRatio
		for _, name := range names[1:] {
			if devInfoNameMap[name].coreRatio != coreRatio || devInfoNameMap[name].memoryRatio != memoryRatio {
				klog.V(4).InfoS("devices announce different core/memory ratios; "+
					"node_gpu_device_configuration_info reports the lowest-named device", "device", names[0])
				break
			}
		}
	}
	// device_split and memory_factor have no DRA equivalent: sharing is
	// expressed per-request through consumable capacity rather than by a
	// node-wide split count.
	ch <- prometheus.MustNewConstMetric(
		nodeGPUConfigInfo, prometheus.GaugeValue, float64(1), c.nodeName, "",
		strconv.FormatFloat(float64(coreRatio)/float64(util.HundredCore), 'f', 2, 64),
		strconv.FormatFloat(float64(memoryRatio)/float64(util.HundredCore), 'f', 2, 64),
		"")

	// Get all pods bound to the current node.
	pods, err := c.podLister.ListByIndexValue(metrics.IndexerKeyPodNodeName, c.nodeName)
	if err != nil {
		klog.Errorf("pod lister list error: %v", err)
		return
	}

	nodeVGpuAssignedMemBytes := uint64(0)
	vGpuAssignedMemMap := make(map[string]uint64)
	vGpuAssignedCoresMap := make(map[string]int64)
	peakSharedContainersMap := make(map[string]int)
	currentSharedContainersMap := make(map[string]int)

	util.PodsOnNodeCallback(pods, node, func(pod *corev1.Pod) {
		// Unlike the device-plugin path there is no pre-bind reservation to
		// honour: a DRA allocation only exists once the scheduler has written
		// it into the claim, and the ResourceSlice it points at is node-local.
		// So the bound node is the authority.
		if pod.Spec.NodeName != c.nodeName {
			return
		}
		// Only pods that actually go through DRA are ours to account for.
		if !util.HasDRARequests(pod) && !util.HasExtendedResource(pod) {
			return
		}

		containerAllocs := c.resolvePodAllocations(pod, devInfoNameMap)

		// Aggregate the allocated resources on the node, collapsing each pod's
		// claims to the per-GPU lifecycle peak so a sequential init container
		// reusing an app container's GPU is not double-counted.
		for uuid, footprint := range reduceDRAPodFootprint(containerAllocs) {
			vGpuAssignedCoresMap[uuid] += footprint.cores
			vGpuAssignedMemMap[uuid] += uint64(footprint.memory)
			nodeVGpuAssignedMemBytes += uint64(footprint.memory)
			// Peak (reserved) concurrent sharing: per-GPU lifecycle peak count.
			peakSharedContainersMap[uuid] += footprint.number
		}

		// Current sharing: only containers running right now, so a completed
		// sequential init container drops out (always <= the peak above). A
		// container counts once per GPU no matter how many results it holds.
		for _, alloc := range containerAllocs {
			if !util.IsContainerRunning(pod, alloc.name) {
				continue
			}
			counted := sets.New[string]()
			for _, result := range alloc.results {
				if counted.Has(result.devInfo.uuid) {
					continue
				}
				counted.Insert(result.devInfo.uuid)
				currentSharedContainersMap[result.devInfo.uuid]++
			}
		}

		// Real-time per-container usage: regular containers, sidecars, and
		// currently-running sequential init containers (a completed init
		// container is excluded so its stale usage stops being reported).
		collectable := sets.New[string](util.CollectableContainerNames(pod)...)

		var getFullPath func(string) string
		switch {
		case cgroups.IsCgroup2UnifiedMode(): // cgroupv2
			getFullPath = cgroup.GetK8sPodCGroupFullPath
		case cgroups.IsCgroup2HybridMode():
			// If the device controller does not exist, use the path of cgroupv2.
			getFullPath = cgroup.GetK8sPodDeviceCGroupFullPath
			if util.PathIsNotExist(cgroup.CGroupDevicePath) {
				getFullPath = cgroup.GetK8sPodCGroupFullPath
			}
		default: // cgroupv1
			getFullPath = cgroup.GetK8sPodDeviceCGroupFullPath
		}

		for _, alloc := range containerAllocs {
			if len(alloc.results) == 0 || !collectable.Has(alloc.name) {
				continue
			}

			klog.V(4).InfoS("Container matching: using resource claim allocation",
				"pod", klog.KObj(pod), "container", alloc.name)

			var containerPids []uint32
			_ = cgroup.GetContainerPidsFunc(pod, alloc.name, getFullPath, func(pid int) {
				containerPids = append(containerPids, uint32(pid))
			})

			// Stable vdevice_idx across scrapes: allocation results arrive in
			// claim order, which is not guaranteed to be stable, and an index
			// that reshuffles would break every per-vdevice time series.
			results := make([]draAllocResult, len(alloc.results))
			copy(results, alloc.results)
			sort.Slice(results, func(i, j int) bool {
				if results[i].device != results[j].device {
					return results[i].device < results[j].device
				}
				return results[i].request < results[j].request
			})

			for deviceCount, result := range results {
				deviceUUID := result.devInfo.uuid
				var (
					deviceMemLimit  = result.memory
					realMemBytes    = result.memory
					vDevIndex       = strconv.Itoa(deviceCount)
					deviceMemUsage  = uint64(0)
					deviceVMemUsage = uint64(0)
					deviceSMUtil    = uint32(0)
					contGPUPids     []string
				)
				// The physical limit is the container's own limit converted back
				// to real VRAM -- NOT the device total. Only oversubscription
				// (ratio > 1) makes the two differ.
				if ratio := result.devInfo.memoryOversubscription(); ratio > 1 {
					realMemBytes = int64(float64(deviceMemLimit) / ratio)
				}

				ContainerDeviceProcInfoEach(devProcInfoMap[deviceUUID], containerPids,
					func(process nvml.ProcessInfo_v1) {
						contGPUPids = append(contGPUPids, strconv.Itoa(int(process.Pid)))
						deviceMemUsage += process.UsedGpuMemory
					})
				ContainerDeviceProcUtilEach(devProcUtilMap[deviceUUID], containerPids,
					func(sample nvml.ProcessUtilizationSample) {
						smUtil := util.GetValidValue(sample.SmUtil)
						codecUtil := util.GetValidValue(sample.EncUtil) + util.GetValidValue(sample.DecUtil)
						codecUtil = util.CodecNormalize(codecUtil)
						deviceSMUtil += smUtil + codecUtil
					})

				ch <- prometheus.MustNewConstMetric(
					containerVGPUMemoryLimit, prometheus.GaugeValue, float64(deviceMemLimit),
					pod.Namespace, pod.Name, alloc.name, vDevIndex, deviceUUID, c.nodeName)
				ch <- prometheus.MustNewConstMetric(
					containerVGPUPhysicalMemoryLimit, prometheus.GaugeValue, float64(realMemBytes),
					pod.Namespace, pod.Name, alloc.name, vDevIndex, deviceUUID, c.nodeName)

				// TODO Unable to track virtual memory usage temporarily.
				// The device-plugin path reads the per-container vMemory ledger
				// through its ContainerLister; the DRA path has no equivalent
				// handle on the container's manager directory yet, so the
				// unified-memory component stays 0 and the two usage metrics
				// below coincide.
				if c.featureGate.Enabled(util.VirtualMemoryTracking) {
					// Once there is a suitable plan in the future, it will be implemented deviceVMemUsage
				}

				ch <- prometheus.MustNewConstMetric(
					containerVGPUMemoryUsage, prometheus.GaugeValue, float64(deviceMemUsage+deviceVMemUsage),
					pod.Namespace, pod.Name, alloc.name, vDevIndex, deviceUUID, c.nodeName)
				ch <- prometheus.MustNewConstMetric(
					containerVGPUPhysicalMemoryUsage, prometheus.GaugeValue, float64(deviceMemUsage),
					pod.Namespace, pod.Name, alloc.name, vDevIndex, deviceUUID, c.nodeName)

				deviceMemUsage += deviceVMemUsage
				memoryUtilRate := int64(0)
				if deviceMemLimit > 0 {
					if deviceMemUsage >= uint64(deviceMemLimit) {
						memoryUtilRate = 100
					} else {
						memoryUtilRate = int64(float64(deviceMemUsage) / float64(deviceMemLimit) * 100)
					}
				}

				ch <- prometheus.MustNewConstMetric(
					containerVGPUMemoryUtilRate, prometheus.GaugeValue, float64(memoryUtilRate),
					pod.Namespace, pod.Name, alloc.name, vDevIndex, deviceUUID, c.nodeName)
				ch <- prometheus.MustNewConstMetric(containerVGPUCoreUtilRate,
					prometheus.GaugeValue, float64(util.GetPercentageValue(deviceSMUtil)),
					pod.Namespace, pod.Name, alloc.name, vDevIndex, deviceUUID, c.nodeName)
			}
		}
	})
	nodeGpuAssignedMemoryBytes := uint64(0)
	for uuid, devInfo := range devInfoUuidMap {
		// Prefer the real VRAM size NVML reports; fall back to undoing the
		// oversubscription factor when the device is not visible to NVML.
		totalPhyMemoryBytes := devInfo.memory
		memoryRatio := devInfo.memoryOversubscription()
		if memory, exists := devMemInfoMap[uuid]; exists {
			totalPhyMemoryBytes = int64(memory.Total)
		} else if memoryRatio > 1 {
			totalPhyMemoryBytes = int64(float64(devInfo.memory) / memoryRatio)
		}

		deviceIndex := strconv.Itoa(devIndexMap[uuid])
		ch <- prometheus.MustNewConstMetric(vGPUTotalMemory,
			prometheus.GaugeValue, float64(devInfo.memory), c.nodeName, deviceIndex, uuid, devTypeMap[uuid])

		ch <- prometheus.MustNewConstMetric(vGPUTotalPhysicalMemory,
			prometheus.GaugeValue, float64(totalPhyMemoryBytes), c.nodeName, deviceIndex, uuid, devTypeMap[uuid])

		assignedPhyMemoryBytes := vGpuAssignedMemMap[uuid]
		if memoryRatio > 1 {
			assignedPhyMemoryBytes = uint64(float64(assignedPhyMemoryBytes) / memoryRatio)
		}
		nodeGpuAssignedMemoryBytes += assignedPhyMemoryBytes
		ch <- prometheus.MustNewConstMetric(vGPUAssignedMemory,
			prometheus.GaugeValue, float64(vGpuAssignedMemMap[uuid]),
			c.nodeName, deviceIndex, uuid, devTypeMap[uuid])
		ch <- prometheus.MustNewConstMetric(vGPUAssignedPhysicalMemory,
			prometheus.GaugeValue, float64(assignedPhyMemoryBytes),
			c.nodeName, deviceIndex, uuid, devTypeMap[uuid])
		ch <- prometheus.MustNewConstMetric(vGPUTotalCoresNumber,
			prometheus.GaugeValue, float64(devInfo.cores),
			c.nodeName, deviceIndex, uuid, devTypeMap[uuid])
		ch <- prometheus.MustNewConstMetric(vGPUAssignedCoresNumber,
			prometheus.GaugeValue, float64(vGpuAssignedCoresMap[uuid]),
			c.nodeName, deviceIndex, uuid, devTypeMap[uuid])
		ch <- prometheus.MustNewConstMetric(vGPUPeakSharedContainersNumber,
			prometheus.GaugeValue, float64(peakSharedContainersMap[uuid]),
			c.nodeName, deviceIndex, uuid, devTypeMap[uuid])
		ch <- prometheus.MustNewConstMetric(vGPUCurrentSharedContainersNumber,
			prometheus.GaugeValue, float64(currentSharedContainersMap[uuid]),
			c.nodeName, deviceIndex, uuid, devTypeMap[uuid])
	}

	ch <- prometheus.MustNewConstMetric(nodeVGPUAssignedMemory,
		prometheus.GaugeValue, float64(nodeVGpuAssignedMemBytes), c.nodeName)

	ch <- prometheus.MustNewConstMetric(nodeVGPUAssignedPhysicalMemory,
		prometheus.GaugeValue, float64(nodeGpuAssignedMemoryBytes), c.nodeName)
}
