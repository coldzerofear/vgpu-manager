package manager

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"time"

	"github.com/NVIDIA/go-nvlib/pkg/nvlib/device"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/coldzerofear/vgpu-manager/pkg/config/watcher"
	"github.com/coldzerofear/vgpu-manager/pkg/device/nvidia"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
)

const (
	WatcherDir   = util.ManagerRootPath + "/" + util.Watcher
	SMUtilFile   = util.SMUtilFile
	MaxBatchSize = 4
)

func WrapChannelWithContext[T any](ch <-chan T) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		defer cancel()
		for {
			select {
			case _, ok := <-ch:
				if !ok {
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	return ctx, cancel
}

func (m *DeviceManager) doWatcher() {
	ctx, cancelFunc := WrapChannelWithContext(m.stop)
	defer cancelFunc()
	filePath := filepath.Join(WatcherDir, SMUtilFile)
	SMUtilWatcherStart(ctx, m.DeviceLib, m.getGPUDeviceMap(), filePath)
	klog.V(3).Infoln("DeviceManager sm watcher stopped")
}

func SMUtilWatcherStart(ctx context.Context, deviceLib *nvidia.DeviceLib, gpuDeviceMap map[string]*GPUDevice, filePath string) {
	wait.UntilWithContext(ctx, func(ctx context.Context) {
		if err := watcher.PrepareDeviceUtilFile(filePath); err != nil {
			klog.ErrorS(err, "PrepareDeviceUtilFile failed")
			return
		}
		deviceUtil, err := watcher.NewMmapDeviceUtil(filePath)
		if err != nil {
			klog.ErrorS(err, "WatchDeviceUtilFile failed")
			return
		}
		defer func() {
			if err := deviceUtil.Sync(); err != nil {
				klog.V(3).ErrorS(err, "failed to sync mmap", "filepath", filePath)
			}
			if err := deviceUtil.Close(); err != nil {
				klog.V(3).ErrorS(err, "failed to close mmap", "filepath", filePath)
			}
		}()

		if err = deviceLib.NvmlInit(); err != nil {
			klog.ErrorS(err, "nvmlInit failed")
			return
		}
		defer deviceLib.NvmlShutdown()

		gpuDevices := make([]*GPUDevice, 0, len(gpuDeviceMap))
		deviceHandlers := make([]device.Device, 0, len(gpuDeviceMap))
		for _, dev := range gpuDeviceMap {
			handle, ret := deviceLib.DeviceGetHandleByUUID(dev.UUID)
			if ret != nvml.SUCCESS {
				klog.Errorf("error getting device handle for uuid '%v': %v", dev.UUID, ret)
				return
			}
			gpuDevices = append(gpuDevices, dev)
			devHandle, _ := deviceLib.NewDevice(handle)
			deviceHandlers = append(deviceHandlers, devHandle)
		}
		if len(deviceHandlers) == 0 {
			klog.V(3).Infoln("no gpu device handle, will exit retry")
			return
		}

		subCtx, subCancelFunc := context.WithCancel(ctx)
		defer subCancelFunc()

		wg := sync.WaitGroup{}
		batches := watcher.BalanceBatches(len(deviceHandlers), MaxBatchSize)
		utilAdapter := watcher.NewDeviceUtilAdapter(
			watcher.WithExtendedInterface(deviceLib.Extensions()),
		)

		for _, batch := range batches {
			wg.Add(1)
			go func(config watcher.BatchConfig, devices []*GPUDevice, handles []device.Device) {
				defer wg.Done()
				err := smWatcherBatchWithContext(subCtx, utilAdapter, deviceUtil, config, devices, handles)
				if err != nil {
					subCancelFunc()
				}
			}(batch, gpuDevices, deviceHandlers)
		}
		wg.Wait()
	}, time.Second)
}

func smWatcherBatchWithContext(
	ctx context.Context, utilAdapter watcher.DeviceUtilInterface, mmapUtil *watcher.MmapDeviceUtil,
	batch watcher.BatchConfig, devices []*GPUDevice, handles []device.Device,
) error {
	interval := 80 * time.Millisecond / time.Duration(batch.Count)
	for {
		for i := batch.StartIndex; i <= batch.EndIndex; i++ {
			select {
			case <-ctx.Done():
				return nil
			default:
			}

			gpuDevice := devices[i]
			gpuHandle := handles[i]

			if err := smWatcherSingleDevice(utilAdapter, mmapUtil, gpuDevice, gpuHandle); err != nil {
				klog.ErrorS(err, "sm watcher single device failed")
				return err
			}
			time.Sleep(interval)
		}
	}
}

func smWatcherSingleDevice(
	utilAdapter watcher.DeviceUtilInterface,
	mmapUtil *watcher.MmapDeviceUtil,
	info *GPUDevice, d device.Device,
) error {
	if !info.Healthy || info.MigEnabled {
		return nil
	}
	if enabled, _ := d.IsMigEnabled(); enabled {
		return nil
	}
	i := info.Index

	computeProcesses, rt := utilAdapter.DeviceGetComputeRunningProcessesByCount(d, watcher.MaxPids)
	if rt != nvml.SUCCESS {
		klog.ErrorS(errors.New(rt.Error()), "GetComputeRunningProcesses failed", "device", i)
		return nil
	}

	graphicsProcesses, rt := utilAdapter.DeviceGetGraphicsRunningProcessesByCount(d, watcher.MaxPids)
	if rt != nvml.SUCCESS {
		klog.ErrorS(errors.New(rt.Error()), "GetGraphicsRunningProcesses failed", "device", i)
		return nil
	}

	procUtilSamples, lastTs, rt := utilAdapter.DeviceGetEnhanceCompatibilityProcessUtilSamplesByCount(d, watcher.MaxPids)
	if rt != nvml.SUCCESS && rt != nvml.ERROR_NOT_FOUND {
		// NOT_FOUND just means the driver holds no samples newer than lastTs. At
		// this poll rate that is the common answer even for a busy GPU, so it stays
		// silent; anything else is worth a line.
		klog.V(3).ErrorS(errors.New(rt.Error()), "DeviceGetProcessUtilSamples failed", "device", i)
	}

	unlock, err := mmapUtil.WLock(i)
	if err != nil {
		klog.V(3).ErrorS(err, "DeviceUtil WLock failed", "device", i)
		return err
	}
	defer func() { _ = unlock() }()

	devUtil, err := mmapUtil.GetDeviceUtil(i)
	if err != nil {
		klog.V(3).ErrorS(err, "get device util failed", "device", i)
		return err
	}

	computeProcessesSize := min(len(computeProcesses), watcher.MaxPids)
	devUtil.ComputeProcessesSize = uint32(computeProcessesSize)
	for index, process := range computeProcesses[:computeProcessesSize] {
		devUtil.ComputeProcesses[index] = process
	}

	graphicsProcessesSize := min(len(graphicsProcesses), watcher.MaxPids)
	devUtil.GraphicsProcessesSize = uint32(graphicsProcessesSize)
	for index, process := range graphicsProcesses[:graphicsProcessesSize] {
		devUtil.GraphicsProcesses[index] = process
	}

	// Refreshed on every tick, including the NOT_FOUND ones. Readers take this as
	// the cutoff to filter the cached samples against, so advancing it is what
	// ages stale samples out (~1s, the sample window); it also proves the watcher
	// is alive, which is what their 5s expiry check is really looking for. The
	// samples themselves are only replaced when the driver actually produced a
	// fresh batch -- clearing them on NOT_FOUND would collapse utilization to zero
	// on the majority of ticks.
	devUtil.LastSeenTimeStamp = lastTs
	if rt == nvml.SUCCESS {
		processUtilSamplesSize := min(len(procUtilSamples), watcher.MaxPids)
		devUtil.ProcessUtilSamplesSize = uint32(processUtilSamplesSize)
		for index, sample := range procUtilSamples[:processUtilSamplesSize] {
			devUtil.ProcessUtilSamples[index] = sample
		}
	}
	return nil
}
