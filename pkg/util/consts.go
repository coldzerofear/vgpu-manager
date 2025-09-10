package util

const (
	DomainPrefix = "nvidia.com"

	// VGPUComputePolicyAnnotation none / balance / fixed(default)
	VGPUComputePolicyAnnotation = DomainPrefix + "/vgpu-compute-policy"

	NodeNvidiaDriverVersionLabel = DomainPrefix + "/node-driver-version"
	NodeNvidiaCudaVersionLabel   = DomainPrefix + "/node-cuda-version"

	MIGDeviceResourceNamePrefix = DomainPrefix + "/mig-"
	VGPUNumberResourceName      = DomainPrefix + "/vgpu-number"
	VGPUMemoryResourceName      = DomainPrefix + "/vgpu-memory"
	VGPUCoreResourceName        = DomainPrefix + "/vgpu-cores"

	// NodeDeviceHeartbeatAnnotation Node device heartbeat time
	NodeDeviceHeartbeatAnnotation = DomainPrefix + "/node-device-heartbeat"
	NodeDeviceRegisterAnnotation  = DomainPrefix + "/node-device-register"
	NodeDeviceTopologyAnnotation  = DomainPrefix + "/node-device-topology"
	NodeConfigInfoAnnotation      = DomainPrefix + "/node-config-info"

	// PodIncludeGpuTypeAnnotation Specify the GPU type to be used
	PodIncludeGpuTypeAnnotation = DomainPrefix + "/include-gpu-type"
	// PodExcludeGpuTypeAnnotation Specify the GPU type to exclude
	PodExcludeGpuTypeAnnotation = DomainPrefix + "/exclude-gpu-type"

	// Scheduling strategies at the node and device levels
	NodeSchedulerPolicyAnnotation   = DomainPrefix + "/node-scheduler-policy"
	DeviceSchedulerPolicyAnnotation = DomainPrefix + "/device-scheduler-policy"
	MemorySchedulerPolicyAnnotation = DomainPrefix + "/memory-scheduler-policy"

	// DeviceTopologyModeAnnotation Specify device topology mode
	DeviceTopologyModeAnnotation = DomainPrefix + "/device-topology-mode"

	// PodIncludeGPUUUIDAnnotation Specify the GPU UUID to be used
	PodIncludeGPUUUIDAnnotation = DomainPrefix + "/include-gpu-uuid"
	// PodExcludeGPUUUIDAnnotation Specify the GPU UUID to be excluded
	PodExcludeGPUUUIDAnnotation = DomainPrefix + "/exclude-gpu-uuid"

	PodPredicateNodeAnnotation = DomainPrefix + "/predicate-node"
	PodPredicateTimeAnnotation = DomainPrefix + "/predicate-time"
	PodAssignedPhaseLabel      = DomainPrefix + "/assigned-phase"

	// PodVGPUPreAllocAnnotation Pre allocated device information by the scheduler
	PodVGPUPreAllocAnnotation = DomainPrefix + "/pre-allocated"
	// PodVGPURealAllocAnnotation Real device information allocated by device plugins
	PodVGPURealAllocAnnotation = DomainPrefix + "/real-allocated"

	HundredCore     = 100
	MaxDeviceNumber = 16

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
