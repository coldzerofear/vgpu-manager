package util

import (
	"strings"
	"sync"

	"k8s.io/klog/v2"
)

var (
	domainName     = "nvidia.com"
	initDomainOnce sync.Once

	// VGPUComputePolicyAnnotation none / balance / fixed(default)
	VGPUComputePolicyAnnotation = domainName + "/vgpu-compute-policy"

	NodeNvidiaDriverVersionLabel = domainName + "/node-driver-version"
	NodeNvidiaCudaVersionLabel   = domainName + "/node-cuda-version"

	MIGDeviceResourceNamePrefix = domainName + "/mig-"
	VGPUNumberResourceName      = domainName + "/vgpu-number"
	VGPUMemoryResourceName      = domainName + "/vgpu-memory"
	VGPUCoreResourceName        = domainName + "/vgpu-cores"

	// NodeDeviceHeartbeatAnnotation Node device heartbeat time
	NodeDeviceHeartbeatAnnotation = domainName + "/node-device-heartbeat"
	NodeDeviceRegisterAnnotation  = domainName + "/node-device-register"
	NodeDeviceTopologyAnnotation  = domainName + "/node-device-topology"
	NodeConfigInfoAnnotation      = domainName + "/node-config-info"

	// PodIncludeGpuTypeAnnotation Specify the GPU type to be used
	PodIncludeGpuTypeAnnotation = domainName + "/include-gpu-type"
	// PodExcludeGpuTypeAnnotation Specify the GPU type to exclude
	PodExcludeGpuTypeAnnotation = domainName + "/exclude-gpu-type"

	// Scheduling strategies at the node and device levels
	NodeSchedulerPolicyAnnotation   = domainName + "/node-scheduler-policy"
	DeviceSchedulerPolicyAnnotation = domainName + "/device-scheduler-policy"
	MemorySchedulerPolicyAnnotation = domainName + "/memory-scheduler-policy"

	// DeviceTopologyModeAnnotation Specify device topology mode
	DeviceTopologyModeAnnotation = domainName + "/device-topology-mode"

	// PodIncludeGPUUUIDAnnotation Specify the GPU UUID to be used
	PodIncludeGPUUUIDAnnotation = domainName + "/include-gpu-uuid"
	// PodExcludeGPUUUIDAnnotation Specify the GPU UUID to be excluded
	PodExcludeGPUUUIDAnnotation = domainName + "/exclude-gpu-uuid"

	PodPredicateNodeAnnotation = domainName + "/predicate-node"
	PodPredicateTimeAnnotation = domainName + "/predicate-time"
	PodAssignedPhaseLabel      = domainName + "/assigned-phase"

	// PodVGPUPreAllocAnnotation Pre allocated device information by the scheduler
	PodVGPUPreAllocAnnotation = domainName + "/pre-allocated"
	// PodVGPURealAllocAnnotation Real device information allocated by device plugins
	PodVGPURealAllocAnnotation = domainName + "/real-allocated"
)

func GetGlobalDomain() string {
	return domainName
}

func SetGlobalDomain(domain string) {
	initDomainOnce.Do(func() {
		domain = strings.TrimSpace(domain)
		if domain == "" {
			klog.Fatalf("domain name cannot be empty")
		}
		domainName = domain
		klog.Infof("Successfully set the domain name to %s", domain)
	})
}

const (
	HundredCore = 100

	// MaxContainerLimit max container num
	MaxContainerLimit = 300000
	// PodAnnotationMaxLength pod annotation max data length 1MB
	PodAnnotationMaxLength = 1024 * 1024

	AllocateCheckErrMsg           = "Allocate check failed"
	PreStartContainerCheckErrMsg  = "PreStartContainer check failed"
	PreStartContainerCheckErrType = "PreStartContainerCheckErr"

	ManagerRootPath = "/etc/vgpu-manager"
	Checkpoints     = "checkpoints"
	Watcher         = "watcher"
	SMUtilFile      = "sm_util.config"
	VMemNode        = "vmem_node"
	VMemNodeFile    = "vmem_node.config"
)

const (
	// CUDA_MEM_LIMIT_<index> gpu memory limit
	CudaMemoryLimitEnv = "CUDA_MEM_LIMIT"
	// CUDA_MEM_RATIO_<index> gpu memory ratio
	CudaMemoryRatioEnv = "CUDA_MEM_RATIO"
	// CUDA_CORE_LIMIT_<index> gpu core limit
	CudaCoreLimitEnv = "CUDA_CORE_LIMIT"
	// CUDA_CORE_SOFT_LIMIT_<index> gpu core soft limit
	CudaSoftCoreLimitEnv = "CUDA_CORE_SOFT_LIMIT"
	// CUDA_CORE_SOFT_LIMIT_<index> gpu memory oversold switch
	CudaMemoryOversoldEnv = "CUDA_MEM_OVERSOLD"
	// GPU_DEVICES_UUID gpu uuid list
	GPUDevicesUuidEnv = "GPU_DEVICES_UUID"
	// CompatibilityModeEnv Indicate the compatibility mode of the environment
	CompatibilityModeEnv = "ENV_COMPATIBILITY_MODE"

	PodNameEnv      = "VGPU_POD_NAME"
	PodNamespaceEnv = "VGPU_POD_NAMESPACE"
	PodUIDEnv       = "VGPU_POD_UID"
	ContNameEnv     = "VGPU_CONTAINER_NAME"
)

type ComputePolicy string

const (
	// NoneComputePolicy There are no computing power limitations, tasks compete for GPUs on their own.
	NoneComputePolicy ComputePolicy = "none"
	// BalanceComputePolicy Automatically balance GPU load, maximize GPU utilization,
	// allocate idle computing power to ongoing tasks,
	// and roll back excess computing power when new tasks require it.
	BalanceComputePolicy ComputePolicy = "balance"
	// FixedComputePolicy Run tasks with a fixed computing power quota,
	// and the utilization rate will not exceed the quota.
	// Default strategy
	FixedComputePolicy ComputePolicy = "fixed" // default
)

type SchedulerPolicy string

const (
	NonePolicy SchedulerPolicy = "none"
	// BinpackPolicy means the lower device memory remained after this allocation, the better
	BinpackPolicy SchedulerPolicy = "binpack"
	// SpreadPolicy means better put this task into an idle GPU card than a shared GPU card
	SpreadPolicy SchedulerPolicy = "spread"
)

type AssignedPhase string

const (
	AssignPhaseSucceed    AssignedPhase = "succeed"
	AssignPhaseAllocating AssignedPhase = "allocating"
	AssignPhaseFailed     AssignedPhase = "failed"
)

type TopologyMode string

const (
	// NoneTopology Do not use any topology mode to allocate devices.
	NoneTopology TopologyMode = "none"
	// NUMATopology aligns the allocated devices according to numa nodes.
	NUMATopology TopologyMode = "numa"
	// LinkTopology find the best device set based on link topology.
	LinkTopology TopologyMode = "link"
)

// Constants representing the various MIG strategies
const (
	MigStrategyNone   = "none"
	MigStrategySingle = "single"
	MigStrategyMixed  = "mixed"
)

// Constants to represent the various device list strategies
const (
	DeviceListStrategyEnvvar         = "envvar"
	DeviceListStrategyVolumeMounts   = "volume-mounts"
	DeviceListStrategyCDIAnnotations = "cdi-annotations"
	DeviceListStrategyCDICRI         = "cdi-cri"
)

type MemorySchedulerPolicy string

const (
	// VirtualMemoryPolicy means selecting nodes with GPU virtual memory enabled
	VirtualMemoryPolicy MemorySchedulerPolicy = "virtual"
	// PhysicalMemoryPolicy Means selecting nodes with GPU physical memory
	PhysicalMemoryPolicy MemorySchedulerPolicy = "physical"
)

// FeatureGates
const (
	CorePlugin       = "CorePlugin"
	MemoryPlugin     = "MemoryPlugin"
	Reschedule       = "Reschedule"
	GPUTopology      = "GPUTopology"
	SMWatcher        = "SMWatcher"
	SerialBindNode   = "SerialBindNode"
	SerialFilterNode = "SerialFilterNode"
	VMemoryNode      = "VMemoryNode"
)
