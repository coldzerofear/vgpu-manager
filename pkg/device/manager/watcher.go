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
	if !m.featureGate.Enabled(util.SMWatcher) {
		return
	}

	klog.V(4).Infoln("DeviceManager starting sm watcher...")

	ctx, cancelFunc := WrapChannelWithContext(m.stop)
	defer cancelFunc()
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
			klog.ErrorS(err, "NvmlInit failed")
			return
		}
		defer m.NvmlShutdown()

		count, ret := m.DeviceGetCount()
		if ret != nvml.SUCCESS {
			klog.Errorf("error getting device count: %s", ret.Error())
			return
		}
		batches := watcher.BalanceBatches(count, MaxBatchSize)
		wg := sync.WaitGroup{}
		_ = wait.PollUntilContextCancel(ctx, 120*time.Millisecond, true, func(ctx context.Context) (bool, error) {
			var watcherErr error
			for _, batch := range batches {
				wg.Add(1)
				go func(config watcher.BatchConfig) {
					defer wg.Done()
					if err := m.smWatcher(deviceUtil, config); err != nil {
						watcherErr = err
					}
				}(batch)
			}
			wg.Wait()
			return false, watcherErr
		})
	}, time.Second)

	klog.V(3).Infoln("DeviceManager sm watcher stopped")
	m.waitCleanup <- struct{}{}
}

func (m *DeviceManager) smWatcher(deviceUtil *watcher.DeviceUtil, batch watcher.BatchConfig) error {
	watcherFunc := func(i int, d device.Device) error {
		if enabled, _ := d.IsMigEnabled(); enabled {
			return nil
		}
		computeProcesses, rt := d.GetComputeRunningProcesses()
		if rt != nvml.SUCCESS {
			klog.ErrorS(errors.New(rt.Error()), "GetComputeRunningProcesses failed", "device", i)
			return nil
		}
		graphicsProcesses, rt := d.GetGraphicsRunningProcesses()
		if rt != nvml.SUCCESS {
			klog.ErrorS(errors.New(rt.Error()), "GetGraphicsRunningProcesses failed", "device", i)
			return nil
		}

		lastTs := time.Now().Add(-1 * time.Second).UnixMicro()
		procUtilSamples, rt := d.GetProcessUtilization(uint64(lastTs))

		if err := deviceUtil.WLock(i); err != nil {
			klog.V(3).ErrorS(err, "DeviceUtilWLock failed", "device", i)
			return err
		}
		defer func() {
			_ = deviceUtil.Unlock(i)
		}()

		computeProcessesSize := min(len(computeProcesses), watcher.MAX_PIDS)
		deviceUtil.GetUtil().Devices[i].ComputeProcessesSize = uint32(computeProcessesSize)
		for index, process := range computeProcesses[:computeProcessesSize] {
			deviceUtil.GetUtil().Devices[i].ComputeProcesses[index] = process
		}

		graphicsProcessesSize := min(len(graphicsProcesses), watcher.MAX_PIDS)
		deviceUtil.GetUtil().Devices[i].GraphicsProcessesSize = uint32(graphicsProcessesSize)
		for index, process := range graphicsProcesses[:graphicsProcessesSize] {
			deviceUtil.GetUtil().Devices[i].GraphicsProcesses[index] = process
		}

		deviceUtil.GetUtil().Devices[i].LastSeenTimeStamp = uint64(lastTs)
		if rt == nvml.SUCCESS {
			processUtilSamplesSize := min(len(procUtilSamples), watcher.MAX_PIDS)
			deviceUtil.GetUtil().Devices[i].ProcessUtilSamplesSize = uint32(processUtilSamplesSize)
			for index, sample := range procUtilSamples[:processUtilSamplesSize] {
				deviceUtil.GetUtil().Devices[i].ProcessUtilSamples[index] = sample
			}
		}
		return nil
	}
	for i := batch.StartIndex; i <= batch.EndIndex; i++ {
		dev, ret := m.DeviceGetHandleByIndex(i)
		if ret != nvml.SUCCESS {
			return fmt.Errorf("error getting device handle for index '%v': %v", i, ret)
		}
		newDevice, _ := m.NewDevice(dev)
		if err := watcherFunc(i, newDevice); err != nil {
			klog.ErrorS(err, "SM Watcher failed")
			return err
		}
	}
	return nil
}
