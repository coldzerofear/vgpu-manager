package util

const (
	DomainPrefix = "nvidia.com"

	NodeVGPUComputeLabel         = DomainPrefix + "/vgpu-compute-policy" // none / balance / fixed(default)
	NodeNvidiaDriverVersionLabel = DomainPrefix + "/node-driver-version"
	NodeNvidiaCudaVersionLabel   = DomainPrefix + "/node-cuda-version"

	VGPUNumberResourceName = DomainPrefix + "/vgpu-number"
	VGPUMemoryResourceName = DomainPrefix + "/vgpu-memory"
	VGPUCoreResourceName   = DomainPrefix + "/vgpu-core"

	// NodeDeviceHeartbeatAnnotation Node device heartbeat time
	NodeDeviceHeartbeatAnnotation = DomainPrefix + "/node-device-heartbeat"
	NodeDeviceRegisterAnnotation  = DomainPrefix + "/node-device-register"
	DeviceMemoryFactorAnnotation  = DomainPrefix + "/device-memory-factor"

	// PodIncludeGpuTypeAnnotation Specify the GPU type to be used
	PodIncludeGpuTypeAnnotation = DomainPrefix + "/include-gpu-type"
	// PodExcludeGpuTypeAnnotation Specify the GPU type to exclude
	PodExcludeGpuTypeAnnotation = DomainPrefix + "/exclude-gpu-type"

	// Scheduling strategies at the node and device levels
	NodeSchedulerPolicyAnnotation   = DomainPrefix + "/node-scheduler-policy"
	DeviceSchedulerPolicyAnnotation = DomainPrefix + "/device-scheduler-policy"

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
)

// * CUDA_MEM_LIMIT_<index>： 内存限制
// * CUDA_CORE_LIMIT： 算力核心限制
// * CUDA_CORE_SOFT_LIMIT： 算力核心软限制（允许超过硬限制）
// * GPU_DEVICES_UUID： 设备uuid
// * CUDA_MEMORY_OVERSUBSCRIBE： 设备内存超卖（虚拟内存）
const (
	NvidiaVisibleDevicesEnv = "NVIDIA_VISIBLE_DEVICES"
	CudaMemoryLimitEnv      = "CUDA_MEM_LIMIT"
	CudaCoreLimitEnv        = "CUDA_CORE_LIMIT"
	GPUDeviceUuidEnv        = "GPU_DEVICES_UUID"

	PodNameEnv      = "VGPU_POD_NAME"
	PodNamespaceEnv = "VGPU_POD_NAMESPACE"
	PodUIDEnv       = "VGPU_POD_UID"
	ContNameEnv     = "VGPU_CONTAINER_NAME"
)

type ComputePolicy string

const (
	NoneComputePolicy    ComputePolicy = "none"
	BalanceComputePolicy ComputePolicy = "balance"
	FixedComputePolicy   ComputePolicy = "fixed" // default
)

type SchedulerPolicy string

const (
	NonePolicy SchedulerPolicy = ""
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
