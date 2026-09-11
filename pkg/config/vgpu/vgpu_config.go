/*
Copyright 2024-2026 coldzerofear

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

package vgpu

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/device/manager"
	"github.com/coldzerofear/vgpu-manager/pkg/device/nvidia"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/opencontainers/cgroups"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// These sizes are the shared config-file ABI mirrored from the C side
// (library resource_data_t / device_t). MaxDeviceCount is the shared
// MAX_DEVICE_COUNT; the struct layout is asserted in vgpu_config_test.go.
const (
	MaxDeviceCount = util.MaxDeviceCount
	NameBufferSize = 64
	UuidBufferSize = 48

	// Frozen-region ABI, mirrored from library/include/hook.h. Bump
	// ConfigLayoutVersion in lockstep with CONFIG_LAYOUT_VERSION on any change to
	// field type/order/offset; the layout is pinned by vgpu_config_test.go.
	CachelineSize           = 128
	ConfigMagic             = 0x56474346 // "VGCF"
	ConfigLayoutVersion     = 1
	ConfigFileSize          = 8192 // fixed; decoupled from sizeof (see design doc)
	DriverVersionBufferSize = 32
	DeviceReservedI32       = 7
)

type VersionT struct {
	Major int32
	Minor int32
}

// DeviceT mirrors C device_t: exactly one cache line (128B), Seq at offset 0.
// Seq is the per-device seqlock version (even = stable, odd = write in
// progress); ModifyDevice bumps it, the C get_device_snapshot() reads it.
type DeviceT struct {
	Seq            uint32
	_              uint32 // keep TotalMemory 8-byte aligned (matches C _seq_pad)
	UUID           [UuidBufferSize]byte
	TotalMemory    uint64
	RealMemory     uint64
	HardCore       int32
	SoftCore       int32
	CoreLimit      int32
	HardLimit      int32
	MemoryLimit    int32
	MemoryOversold int32
	Activate       int32
	Reserved       [DeviceReservedI32]int32
}

// ResourceDataT mirrors C resource_data_t: a 128B frozen header, an immutable
// pod-identity block, then the per-device config. Field order/offsets are
// byte-for-byte identical to the C struct.
type ResourceDataT struct {
	// ---- frozen header: 128 bytes ----
	Magic         uint32
	LayoutVersion uint32
	RegionSize    uint32
	DeviceCount   uint32
	CudaVersion   VersionT // CUDA major.minor (was DriverVersion)
	DriverVersion [DriverVersionBufferSize]byte
	_             [72]byte // pad header to one cache line (128 - 56)
	// ---- pod identity + flags (written once, never mutated) ----
	PodUID            [UuidBufferSize]byte
	PodName           [NameBufferSize]byte
	PodNamespace      [NameBufferSize]byte
	ContainerName     [NameBufferSize]byte
	RegisterUUID      [UuidBufferSize]byte
	CompatibilityMode int32
	SMWatcher         int32
	VMemoryNode       int32
	_                 [84]byte // pad Devices onto a cache line (offset 512)
	// ---- per-device config, seqlock-protected ----
	Devices [MaxDeviceCount]DeviceT
}

type MmapResourceData struct {
	resource *ResourceDataT
	mmapFile *util.MmapFile
	mutex    sync.Mutex
	// closed guards against use-after-munmap: the lister Close()s and munmaps an
	// entry (removeResourceData/removeContainer) while a metrics scrape may still
	// hold this *MmapResourceData. Any accessor that dereferences r.resource must
	// bail when closed, under r.mutex, so it never touches unmapped memory.
	closed bool
}

func (r *MmapResourceData) GetResource() *ResourceDataT {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	if r.closed {
		return nil
	}
	return r.resource
}

func (r *MmapResourceData) Close() error {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	return r.mmapFile.Close()
}

func (r *MmapResourceData) NeedsReload() (reload bool, err error) {
	r.mutex.Lock()
	reload, err = r.mmapFile.NeedsReload()
	r.mutex.Unlock()
	return reload, err
}

func (r *MmapResourceData) Reload() error {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	data, err := NewMmapResourceData(r.mmapFile.Path)
	if err != nil {
		return fmt.Errorf("reload %q failed: %w", r.mmapFile.Path, err)
	}
	_ = r.mmapFile.Close()
	r.resource = data.resource
	r.mmapFile = data.mmapFile
	return nil
}

func CheckResourceDataSize(filePath string) error {
	if fileInfo, err := os.Stat(filePath); err != nil {
		return err
	} else if fileInfo.Size() != ConfigFileSize {
		return fmt.Errorf("vGPU config file size mismatch, expected: %d, actual: %d", ConfigFileSize, fileInfo.Size())
	}
	return nil
}

// validateHeader rejects a config whose frozen header does not match this
// build -- a mismatched layout_version (rolling upgrade) is refused cleanly
// rather than misread, the same contract as vmem_node / sm_node.
func validateHeader(r *ResourceDataT) error {
	wantSize := uint32(unsafe.Sizeof(ResourceDataT{}))
	if r.Magic != ConfigMagic || r.LayoutVersion != ConfigLayoutVersion ||
		r.RegionSize != wantSize || r.DeviceCount != MaxDeviceCount {
		return fmt.Errorf("vGPU config header mismatch: magic=%#x ver=%d size=%d count=%d (want %#x/%d/%d/%d)",
			r.Magic, r.LayoutVersion, r.RegionSize, r.DeviceCount,
			ConfigMagic, ConfigLayoutVersion, wantSize, MaxDeviceCount)
	}
	return nil
}

func NewMmapResourceData(filePath string) (*MmapResourceData, error) {
	mmapFile, err := util.OpenMmap(filePath, util.DefaultReadWriteMmap)
	if err != nil {
		return nil, err
	}
	if mmapFile.FileInfo.Size() != ConfigFileSize {
		klog.Errorf("File size mismatch, expected: %d, actual: %d", ConfigFileSize, mmapFile.FileInfo.Size())
		_ = mmapFile.Close()
		return nil, fmt.Errorf("vGPU config file size mismatch")
	}
	data := (*ResourceDataT)(unsafe.Pointer(&mmapFile.Data[0]))
	if err := validateHeader(data); err != nil {
		_ = mmapFile.Close()
		return nil, err
	}
	return &MmapResourceData{
		resource: data,
		mmapFile: mmapFile,
	}, nil
}

func GetCompatibilityMode(devManager *manager.DeviceManager) util.CompatibilityMode {
	mode := util.HostMode
	switch {
	case devManager.GetFeatureGate().Enabled(util.DevicePluginClientMode):
		mode |= util.ClientRegMode
	case cgroups.IsCgroup2UnifiedMode():
		mode |= util.CGroupv2Mode
	case cgroups.IsCgroup2HybridMode():
		mode |= util.CGroupv2Mode
	default:
		mode |= util.CGroupv1Mode
	}
	if devManager.GetNodeConfig().GetOpenKernelModules() {
		mode |= util.OpenKernelMode
	}
	return mode
}

type ResourceOption struct {
	CudaVersion        nvidia.CudaDriverVersion
	DriverVersion      string
	PodUID             string
	PodName            string
	PodNamespace       string
	ContainerName      string
	RegisterUUID       string
	MemoryRatio        float64
	MemoryOversold     bool
	SMWatcherEnabled   bool
	VMemoryNodeEnabled bool
	ComputePolicy      util.ComputePolicy
	CompatibilityMode  util.CompatibilityMode
	DeviceInfos        []device.DeviceClaim
	DeviceClaims       []device.DeviceClaim
}

type OptionFunc func(r *ResourceOption)

func convert32Bytes(val string) [DriverVersionBufferSize]byte {
	var byteArray [DriverVersionBufferSize]byte
	copy(byteArray[:DriverVersionBufferSize-1], val)
	return byteArray
}

func convert48Bytes(val string) [UuidBufferSize]byte {
	var byteArray [UuidBufferSize]byte
	copy(byteArray[:UuidBufferSize-1], val)
	return byteArray
}

func convert64Bytes(val string) [NameBufferSize]byte {
	var byteArray [NameBufferSize]byte
	copy(byteArray[:NameBufferSize-1], val)
	return byteArray
}

func WithDeviceInfos(infos []device.DeviceClaim) OptionFunc {
	return func(r *ResourceOption) {
		r.DeviceInfos = infos
	}
}
func WithDeviceClaims(claims []device.DeviceClaim) OptionFunc {
	return func(r *ResourceOption) {
		r.DeviceClaims = claims
	}
}
func WithContainerName(containerName string) OptionFunc {
	return func(r *ResourceOption) {
		r.ContainerName = containerName
	}
}
func WithComputePolicy(policy util.ComputePolicy) OptionFunc {
	return func(r *ResourceOption) {
		r.ComputePolicy = policy
	}
}
func WithPodInfo(pod *corev1.Pod) OptionFunc {
	return func(r *ResourceOption) {
		if pod != nil {
			r.PodUID = string(pod.UID)
			r.PodName = pod.Name
			r.PodNamespace = pod.Namespace
		}
	}
}
func WithCompatibilityMode(mode util.CompatibilityMode) OptionFunc {
	return func(r *ResourceOption) {
		r.CompatibilityMode = mode
	}
}
func WithSMWatcherEnabled(enabled bool) OptionFunc {
	return func(r *ResourceOption) {
		r.SMWatcherEnabled = enabled
	}
}
func WithVMemoryNodeEnabled(enabled bool) OptionFunc {
	return func(r *ResourceOption) {
		r.VMemoryNodeEnabled = enabled
	}
}
func WithRegisterUUID(uuid string) OptionFunc {
	return func(r *ResourceOption) {
		r.RegisterUUID = uuid
	}
}
func WithMemoryRatio(ratio float64) OptionFunc {
	return func(r *ResourceOption) {
		r.MemoryRatio = ratio
	}
}
func WithMemoryOversold(oversold bool) OptionFunc {
	return func(r *ResourceOption) {
		r.MemoryOversold = oversold
	}
}
func WithDriverVersion(version nvidia.DriverVersion) OptionFunc {
	return func(r *ResourceOption) {
		r.CudaVersion = version.CudaDriverVersion
		r.DriverVersion = version.DriverVersion
	}
}

func NewResourceDataWithOptions(o ResourceOption, opts ...OptionFunc) *ResourceDataT {
	for _, opt := range opts {
		opt(&o)
	}
	deviceInfoMap := make(map[string]*device.DeviceClaim, len(o.DeviceInfos))
	for i, info := range o.DeviceInfos {
		deviceInfoMap[info.Uuid] = &o.DeviceInfos[i]
	}
	deviceConfigs := [MaxDeviceCount]DeviceT{}
	for i, claim := range o.DeviceClaims {
		if i >= MaxDeviceCount {
			break
		}
		deviceInfo, exists := deviceInfoMap[claim.Uuid]
		if !exists {
			continue
		}
		// deviceInfo.Id is the host device index; the shared-memory layout only
		// has MaxDeviceCount slots. Guard the write so a node with more GPUs than
		// that cannot index out of range (the old cgo path did a silent OOB
		// memcpy here instead).
		if deviceInfo.Id < 0 || deviceInfo.Id >= MaxDeviceCount {
			klog.Warningf("Device host index %d out of range [0, %d), skip", deviceInfo.Id, MaxDeviceCount)
			continue
		}
		totalMemoryBytes := uint64(claim.Memory) << 20
		realMemoryBytes := totalMemoryBytes
		if o.MemoryRatio > 1 {
			o.MemoryOversold = true
			realMemoryBytes = uint64(float64(realMemoryBytes) / o.MemoryRatio)
		}
		deviceConfig := DeviceT{
			UUID:        convert48Bytes(claim.Uuid),
			TotalMemory: totalMemoryBytes,
			RealMemory:  realMemoryBytes,
			HardCore:    int32(claim.Cores),
			SoftCore:    int32(claim.Cores),
			CoreLimit:   int32(0),
			HardLimit:   int32(0),
			Activate:    int32(1),
		}
		// need limit core
		switch o.ComputePolicy {
		case util.BalanceComputePolicy:
			//  int soft_core;
			deviceConfig.SoftCore = int32(deviceInfo.Cores)
			// need limit core
			if claim.Cores > 0 && claim.Cores < util.HundredCore {
				deviceConfig.CoreLimit = int32(1) //  int core_limit;
				if claim.Cores >= deviceInfo.Cores {
					deviceConfig.HardLimit = int32(1) //  int hard_limit;
				}
			}
		case util.FixedComputePolicy: // need limit core
			if claim.Cores > 0 && claim.Cores < util.HundredCore {
				deviceConfig.CoreLimit = int32(1) //  int core_limit;
				deviceConfig.HardLimit = int32(1) //  int hard_limit;
			}
		case util.NoneComputePolicy:
		}
		//  int memory_limit
		if claim.Memory == deviceInfo.Memory && o.MemoryRatio == 1 {
			deviceConfig.MemoryLimit = int32(0)
		} else {
			deviceConfig.MemoryLimit = int32(1)
		}
		//  int memory_oversold
		if o.MemoryOversold {
			deviceConfig.MemoryOversold = int32(1)
		} else {
			deviceConfig.MemoryOversold = int32(0)
		}
		deviceConfigs[deviceInfo.Id] = deviceConfig
	}
	smWatcher := 0
	if o.SMWatcherEnabled {
		smWatcher = 1
	}
	vMemoryNode := 0
	if o.VMemoryNodeEnabled {
		vMemoryNode = 1
	}
	major, minor := o.CudaVersion.MajorAndMinor()
	return &ResourceDataT{
		Magic:         ConfigMagic,
		LayoutVersion: ConfigLayoutVersion,
		RegionSize:    uint32(unsafe.Sizeof(ResourceDataT{})),
		DeviceCount:   MaxDeviceCount,
		CudaVersion: VersionT{
			Major: int32(major),
			Minor: int32(minor),
		},
		DriverVersion:     convert32Bytes(o.DriverVersion),
		PodUID:            convert48Bytes(o.PodUID),
		PodName:           convert64Bytes(o.PodName),
		PodNamespace:      convert64Bytes(o.PodNamespace),
		ContainerName:     convert64Bytes(o.ContainerName),
		RegisterUUID:      convert48Bytes(o.RegisterUUID),
		CompatibilityMode: int32(o.CompatibilityMode),
		SMWatcher:         int32(smWatcher),
		VMemoryNode:       int32(vMemoryNode),
		Devices:           deviceConfigs,
	}
}

func WithDeviceManager(devManager *manager.DeviceManager) OptionFunc {
	return func(r *ResourceOption) {
		WithDriverVersion(devManager.GetDriverVersion())(r)
		WithMemoryRatio(devManager.GetNodeConfig().GetDeviceMemoryScaling())(r)
		WithSMWatcherEnabled(devManager.GetFeatureGate().Enabled(util.SharedSMUtilizationWatcher))(r)
		WithVMemoryNodeEnabled(devManager.GetFeatureGate().Enabled(util.VirtualMemoryTracking))(r)
		WithCompatibilityMode(GetCompatibilityMode(devManager))(r)
		devices := devManager.GetNodeDeviceInfo()
		length := min(MaxDeviceCount, len(devices))
		deviceInfos := make([]device.DeviceClaim, length)
		for i, dev := range devices[:length] {
			deviceInfos[i] = device.DeviceClaim{
				Id:     dev.Id,
				Uuid:   dev.Uuid,
				Cores:  dev.Core,
				Memory: dev.Memory,
			}
		}
		WithDeviceInfos(deviceInfos)(r)
	}
}

func GetDefaultComputePolicy(pod *corev1.Pod, node *corev1.Node) util.ComputePolicy {
	computePolicy, ok := util.HasAnnotation(pod, util.VGPUComputePolicyAnnotation)
	if !ok || len(computePolicy) == 0 {
		computePolicy, _ = util.HasAnnotation(node, util.VGPUComputePolicyAnnotation)
	}
	return GetComputePolicy(computePolicy)
}

func GetComputePolicy(policy string) util.ComputePolicy {
	switch strings.ToLower(policy) {
	case string(util.BalanceComputePolicy):
		return util.BalanceComputePolicy
	case string(util.FixedComputePolicy):
		return util.FixedComputePolicy
	case string(util.NoneComputePolicy):
		return util.NoneComputePolicy
	default:
		return util.FixedComputePolicy
	}
}

// WriteResourceDataToDisk writes the fixed-size ResourceDataT to filePath as a
// raw byte image, matching the C setting_to_disk (O_CREAT|O_TRUNC|O_WRONLY,
// mode 0777). The Go struct layout is byte-for-byte identical to the C
// resource_data_t (asserted by CheckResourceDataSize and the mmap round-trip
// test), so the bytes are interchangeable with the C reader.
func WriteResourceDataToDisk(filePath string, data *ResourceDataT) error {
	// Stamp the frozen header unconditionally so any writer path produces a file
	// the C validator (mmap_file_to_config_path) accepts.
	data.Magic = ConfigMagic
	data.LayoutVersion = ConfigLayoutVersion
	data.RegionSize = uint32(unsafe.Sizeof(ResourceDataT{}))
	data.DeviceCount = MaxDeviceCount

	size := int(unsafe.Sizeof(ResourceDataT{}))
	buf := unsafe.Slice((*byte)(unsafe.Pointer(data)), size)
	file, err := os.OpenFile(filePath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0777)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	if n, err := file.Write(buf); err != nil {
		return err
	} else if n != size {
		return fmt.Errorf("short write for config %s: wrote %d of %d bytes", filePath, n, size)
	}
	// Pad to the permanently reserved size so the map length is fixed and a later
	// larger struct never has to resize the file (which would SIGBUS old maps).
	if err := file.Truncate(int64(ConfigFileSize)); err != nil {
		return fmt.Errorf("can't size config %s to %d: %w", filePath, ConfigFileSize, err)
	}
	return nil
}

// getConfigLockOffset mirrors C GET_CONFIG_LOCK_OFFSET (library/include/hook.h):
// offsetof(devices) + deviceIndex*sizeof(device_t) + offsetof(device_t.seq).
// unsafe.Offsetof(Devices) ignores the array index, so the per-device stride is
// added explicitly, exactly like the watcher's getDeviceLockOffset.
func getConfigLockOffset(deviceIndex int) int64 {
	base := int64(unsafe.Offsetof(ResourceDataT{}.Devices))
	stride := int64(unsafe.Sizeof(DeviceT{}))
	seqOff := int64(unsafe.Offsetof(DeviceT{}.Seq))
	return base + int64(deviceIndex)*stride + seqOff
}

// ModifyDevice applies mutation to devices[deviceIndex] under the per-device
// seqlock, so a concurrent C reader (get_device_snapshot) sees either the whole
// update or none of it -- never a torn mix.
//
// The receiver is *MmapResourceData because the mutation must land in the
// MAP_SHARED mapping the C side observes (that is why it is here and not on
// *ResourceDataT, which could be a heap copy). It writes in place -- it must
// never go back through writeResourceDataToDisk, whose O_TRUNC would break the
// reader's view.
//
// It takes the per-device OFD F_WRLCK (GET_CONFIG_LOCK_OFFSET) around the
// seqlock write for two reasons: it serialises concurrent writers, and it gives
// the C reader's F_RDLCK slow-path fallback something to block on so that path
// is robust even if a writer is descheduled or dies mid-update (a dead writer's
// OFD lock releases on fd close). The C fast reader path takes no lock, so this
// adds no per-read cost. r.mutex is held so a concurrent Reload() cannot munmap
// the mapping out from under the in-place write.
func (r *MmapResourceData) ModifyDevice(deviceIndex int, mutation func(*DeviceT)) error {
	if deviceIndex < 0 || deviceIndex >= MaxDeviceCount {
		return fmt.Errorf("device index %d out of range [0, %d)", deviceIndex, MaxDeviceCount)
	}
	r.mutex.Lock()
	defer r.mutex.Unlock()
	if r.closed {
		return fmt.Errorf("resource mapping already closed")
	}

	f, err := os.OpenFile(r.mmapFile.Path, os.O_RDWR, 0644)
	if err != nil {
		return fmt.Errorf("open %q for device %d lock: %w", r.mmapFile.Path, deviceIndex, err)
	}
	defer func() { _ = f.Close() }()
	offset := getConfigLockOffset(deviceIndex)
	if err = util.FcntlRecordLock(f.Fd(), syscall.F_WRLCK, true, offset); err != nil {
		return fmt.Errorf("fcntl wlock device %d at offset %d: %w", deviceIndex, offset, err)
	}
	defer func() { _ = util.FcntlRecordLock(f.Fd(), syscall.F_UNLCK, false, offset) }()

	d := &r.resource.Devices[deviceIndex]
	atomic.AddUint32(&d.Seq, 1) // even -> odd: write in progress
	mutation(d)
	atomic.AddUint32(&d.Seq, 1) // odd -> even: publish
	return nil
}

// configSeqSpinLimit bounds the seqlock reader's retry loop; past it we assume a
// writer died mid-update (seq stuck odd) and give up rather than spin forever.
const configSeqSpinLimit = 1024

// GetDeviceSnapshot returns a tear-free copy of devices[deviceIndex], read via
// the per-device seqlock -- the lock-free reader path mirroring the C
// get_device_snapshot(). Unlike ModifyDevice it is a READER: it takes NO fcntl
// lock (no per-read syscalls) and never bumps Seq. Advancing the version on a
// read would be wrong twice over -- it would make every read look like a write
// to concurrent C readers and force them to retry, and it is simply not a
// writer. A copy taken while a writer is mid-update is detected by the seq
// change and retried. r.mutex is held only so a concurrent Reload() cannot
// munmap the mapping out from under the copy.
//
// Returns nil when the index is out of range, or (astronomically rare) a writer
// left the seqlock odd past the spin cap; callers treat nil as "no fresh data".
func (r *MmapResourceData) GetDeviceSnapshot(deviceIndex int) *DeviceT {
	if deviceIndex < 0 || deviceIndex >= MaxDeviceCount {
		return nil
	}
	r.mutex.Lock()
	defer r.mutex.Unlock()
	if r.closed {
		return nil // mapping already munmapped by the lister
	}
	d := &r.resource.Devices[deviceIndex]
	spins := 0
	for ; spins < configSeqSpinLimit; spins++ {
		s1 := atomic.LoadUint32(&d.Seq)
		if s1&1 != 0 { // odd: a writer is mid-update, retry
			runtime.Gosched()
			continue
		}
		snap := *d
		// Re-read the sequence after the copy. On amd64 the atomic load is a
		// plain mov, loads are not reordered with loads, and the Go compiler does
		// not move memory accesses across an atomic op -- so the copy completes
		// before this second load, the same ordering the C reader's ACQUIRE fence
		// encodes. A changed seq means the copy may be torn: retry.
		if atomic.LoadUint32(&d.Seq) == s1 {
			return &snap
		}
		runtime.Gosched()
	}
	if spins == configSeqSpinLimit {
		f, err := os.OpenFile(r.mmapFile.Path, os.O_RDWR, 0644)
		if err != nil {
			klog.Errorf("open %q for device %d lock: %v", r.mmapFile.Path, deviceIndex, err)
			return nil
		}
		defer func() { _ = f.Close() }()

		offset := getConfigLockOffset(deviceIndex)
		if err = util.FcntlRecordLock(f.Fd(), syscall.F_RDLCK, true, offset); err != nil {
			klog.Errorf("fcntl wlock device %d at offset %d: %v", deviceIndex, offset, err)
			return nil
		}
		defer func() { _ = util.FcntlRecordLock(f.Fd(), syscall.F_UNLCK, false, offset) }()
		snap := *d
		return &snap
	}
	return nil
}

func WriteVGPUConfigFile(
	filePath string, devManager *manager.DeviceManager, pod *corev1.Pod,
	contClaim device.ContainerDeviceClaim, memoryOversold bool, node *corev1.Node,
) error {
	if _, err := os.Stat(filePath); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		data := NewResourceDataWithOptions(ResourceOption{},
			WithPodInfo(pod),
			WithDeviceManager(devManager),
			WithContainerName(contClaim.Name),
			WithDeviceClaims(contClaim.DeviceClaims),
			WithMemoryOversold(memoryOversold),
			WithComputePolicy(GetDefaultComputePolicy(pod, node)),
		)
		if err = WriteResourceDataToDisk(filePath, data); err != nil {
			return fmt.Errorf("can't sink config %s: %w", filePath, err)
		}
	}
	return nil
}
