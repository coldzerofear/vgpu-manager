package manager

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/NVIDIA/go-nvlib/pkg/nvlib/device"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/coldzerofear/vgpu-manager/pkg/config/watcher"
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
	m.wait.Add(1)
	defer func() {
		cancelFunc()
		m.wait.Done()
	}()
	filePath := filepath.Join(WatcherDir, SMUtilFile)

	wait.UntilWithContext(ctx, func(ctx context.Context) {
		if err := watcher.PrepareDeviceUtilFile(filePath); err != nil {
			klog.ErrorS(err, "PrepareDeviceUtilFile failed")
			return
		}
		deviceUtil, err := watcher.NewDeviceUtil(filePath)
		if err != nil {
			klog.ErrorS(err, "WatchDeviceUtilFile failed")
			return
		}
		defer func() {
			_ = deviceUtil.Munmap(true)
		}()

		if err = m.NvmlInit(); err != nil {
			klog.ErrorS(err, "nvmlInit failed")
			return
		}
		defer m.NvmlShutdown()

		count, ret := m.DeviceGetCount()
		if ret != nvml.SUCCESS {
			klog.Errorf("error getting device count: %s", ret.Error())
			return
		} else if count <= 0 {
			return
		}

		subCtx, subCancelFunc := context.WithCancel(ctx)
		defer subCancelFunc()

		wg := sync.WaitGroup{}
		batches := watcher.BalanceBatches(count, MaxBatchSize)
		for _, batch := range batches {
			wg.Add(1)
			go func(config watcher.BatchConfig) {
				defer wg.Done()
				err := m.smWatcherBatchWithContext(deviceUtil, config, subCtx)
				if err != nil {
					subCancelFunc()
				}
			}(batch)
		}
		wg.Wait()
	}, time.Second)

	klog.V(3).Infoln("DeviceManager sm watcher stopped")
}

func (m *DeviceManager) smWatcherBatchWithContext(deviceUtil *watcher.DeviceUtil, batch watcher.BatchConfig, ctx context.Context) error {
	interval := 80 * time.Millisecond / time.Duration(batch.Count)
	for {
		for i := batch.StartIndex; i <= batch.EndIndex; i++ {
			select {
			case <-ctx.Done():
				return nil
			default:
			}

			dev, ret := m.DeviceGetHandleByIndex(i)
			if ret != nvml.SUCCESS {
				return fmt.Errorf("error getting device handle for index '%v': %v", i, ret)
			}
			newDevice, _ := m.NewDevice(dev)
			if err := m.smWatcherSingleDevice(deviceUtil.GetWrap(), i, newDevice); err != nil {
				klog.ErrorS(err, "sm watcher single device failed")
				return err
			}
			time.Sleep(interval)
		}
	}
}

func (m *DeviceManager) smWatcherSingleDevice(deviceUtil *watcher.DeviceUtilWrap, i int, d device.Device) error {
	if enabled, _ := d.IsMigEnabled(); enabled {
		return nil
	}
	computeProcesses, rt := d.GetComputeRunningProcessesBySize(watcher.MaxPids)
	if rt != nvml.SUCCESS {
		klog.ErrorS(errors.New(rt.Error()), "GetComputeRunningProcesses failed", "device", i)
		return nil
	}
	graphicsProcesses, rt := d.GetGraphicsRunningProcessesBySize(watcher.MaxPids)
	if rt != nvml.SUCCESS {
		klog.ErrorS(errors.New(rt.Error()), "GetGraphicsRunningProcesses failed", "device", i)
		return nil
	}

	lastTs := time.Now().Add(-1 * time.Second).UnixMicro()
	procUtilSamples, rt := d.GetProcessUtilizationBySize(uint64(lastTs), watcher.MaxPids)

	if err := deviceUtil.WLock(i); err != nil {
		klog.V(3).ErrorS(err, "DeviceUtilWLock failed", "device", i)
		return err
	}
	defer func() {
		_ = deviceUtil.Unlock(i)
	}()

	computeProcessesSize := min(len(computeProcesses), watcher.MaxPids)
	deviceUtil.GetUtil().Devices[i].ComputeProcessesSize = uint32(computeProcessesSize)
	for index, process := range computeProcesses[:computeProcessesSize] {
		deviceUtil.GetUtil().Devices[i].ComputeProcesses[index] = process
	}

	graphicsProcessesSize := min(len(graphicsProcesses), watcher.MaxPids)
	deviceUtil.GetUtil().Devices[i].GraphicsProcessesSize = uint32(graphicsProcessesSize)
	for index, process := range graphicsProcesses[:graphicsProcessesSize] {
		deviceUtil.GetUtil().Devices[i].GraphicsProcesses[index] = process
	}

	deviceUtil.GetUtil().Devices[i].LastSeenTimeStamp = uint64(lastTs)
	if rt == nvml.SUCCESS {
		processUtilSamplesSize := min(len(procUtilSamples), watcher.MaxPids)
		deviceUtil.GetUtil().Devices[i].ProcessUtilSamplesSize = uint32(processUtilSamplesSize)
		for index, sample := range procUtilSamples[:processUtilSamplesSize] {
			deviceUtil.GetUtil().Devices[i].ProcessUtilSamples[index] = sample
		}
	}
	return nil
}
