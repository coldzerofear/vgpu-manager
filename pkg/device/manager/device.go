package manager

import (
	"fmt"
	"strconv"
	"sync"

	nvdev "github.com/NVIDIA/go-nvlib/pkg/nvlib/device"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/coldzerofear/vgpu-manager/cmd/device-plugin/options"
	"github.com/coldzerofear/vgpu-manager/pkg/config/node"
	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/device/gpuallocator"
	"github.com/coldzerofear/vgpu-manager/pkg/device/gpuallocator/links"
	"github.com/coldzerofear/vgpu-manager/pkg/device/nvidia"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/component-base/featuregate"
	"k8s.io/klog/v2"
)

type GPUDevice struct {
	*nvidia.GpuInfo
	NumaNode int
	Paths    []string
	Healthy  bool
	Links    map[int][]links.P2PLinkType
}

type MIGDevice struct {
	*nvidia.MigInfo
	Paths   []string
	Healthy bool
}

type Device struct {
	GPU *GPUDevice
	MIG *MIGDevice
}

type DeviceManager struct {
	*nvidia.DeviceLib
	mut           sync.Mutex
	client        kubernetes.Interface
	config        *node.NodeConfig
	driverVersion nvidia.DriverVersion
	devices       []*Device

	unhealthy chan *Device
	notify    map[string]chan *Device
	stop      chan struct{}
}

func (m *DeviceManager) GetDriverVersion() nvidia.DriverVersion {
	return m.driverVersion
}

func (m *DeviceManager) GetNodeConfig() node.NodeConfig {
	return *m.config
}

func (m *DeviceManager) AddNotifyChannel(name string, ch chan *Device) {
	m.mut.Lock()
	m.notify[name] = ch
	m.mut.Unlock()
}

func (m *DeviceManager) RemoveNotifyChannel(name string) {
	m.mut.Lock()
	delete(m.notify, name)
	m.mut.Unlock()
}

func NewFakeDeviceManager(config *node.NodeConfig, version nvidia.DriverVersion, devices []*Device) *DeviceManager {
	return &DeviceManager{
		driverVersion: version,
		config:        config,
		devices:       devices,
		client:        fake.NewSimpleClientset(),
		stop:          make(chan struct{}),
		unhealthy:     make(chan *Device),
		notify:        make(map[string]chan *Device),
	}
}

func NewDeviceManager(config *node.NodeConfig, kubeClient *kubernetes.Clientset) (*DeviceManager, error) {
	driverlib, err := nvidia.NewDeviceLib("/")
	if err != nil {
		klog.Error("If this is a GPU node, did you configure the NVIDIA Container Toolkit?")
		klog.Error("You can check the prerequisites at: https://github.com/NVIDIA/k8s-device-plugin#prerequisites")
		klog.Error("You can learn how to set the runtime at: https://github.com/NVIDIA/k8s-device-plugin#quick-start")
		klog.Error("If this is not a GPU node, you should set up a toleration or nodeSelector to only deploy this plugin on GPU nodes")
		return nil, err
	}
	m := &DeviceManager{
		DeviceLib: driverlib,
		config:    config,
		client:    kubeClient,
		stop:      make(chan struct{}),
		unhealthy: make(chan *Device),
		notify:    make(map[string]chan *Device),
	}
	if err = m.Init(); err != nil {
		klog.Errorf("If this is a GPU node, did you set the default container runtime to `nvidia`?")
		klog.Errorf("You can check the prerequisites at: https://github.com/NVIDIA/k8s-device-plugin#prerequisites")
		klog.Errorf("You can learn how to set the runtime at: https://github.com/NVIDIA/k8s-device-plugin#quick-start")
		klog.Errorf("If this is not a GPU node, you should set up a toleration or nodeSelector to only deploy this plugin on GPU nodes")
		return nil, err
	}
	defer m.Shutdown()
	err = m.initDevices()
	return m, err
}

func (m *DeviceManager) initDevices() error {
	driverVersion, ret := m.SystemGetDriverVersion()
	if ret != nvml.SUCCESS {
		return fmt.Errorf("error getting driver version: %s", nvml.ErrorString(ret))
	}
	cudaDriverVersion, ret := m.SystemGetCudaDriverVersion()
	if ret != nvml.SUCCESS {
		return fmt.Errorf("error getting CUDA driver version: %s", nvml.ErrorString(ret))
	}
	m.driverVersion = nvidia.DriverVersion{
		DriverVersion:     driverVersion,
		CudaDriverVersion: nvidia.CudaDriverVersion(cudaDriverVersion),
	}
	var (
		linksMap           map[string]map[int][]links.P2PLinkType
		featureGate        = featuregate.DefaultComponentGlobalsRegistry.FeatureGateFor(options.Component)
		gpuTopologyEnabled = featureGate != nil && featureGate.Enabled(options.GPUTopology)
		exists             = false
	)
	if gpuTopologyEnabled {
		deviceList, err := gpuallocator.NewDevices(
			gpuallocator.WithDeviceLib(m),
			gpuallocator.WithNvmlLib(m.NvmlInterface),
		)
		if err != nil {
			return fmt.Errorf("error getting gpuallocator device list: %v", err)
		}
		linksMap = make(map[string]map[int][]links.P2PLinkType)
		for _, dev := range deviceList {
			linklist := map[int][]links.P2PLinkType{}
			for index, pLinks := range dev.Links {
				var p2pLinks []links.P2PLinkType
				for _, link := range pLinks {
					p2pLinks = append(p2pLinks, link.Type)
				}
				linklist[index] = p2pLinks
			}
			linksMap[dev.UUID] = linklist
		}
	}
	err := m.VisitDevices(func(i int, d nvdev.Device) error {
		gpuInfo, err := m.GetGpuInfo(i, d)
		if err != nil {
			return fmt.Errorf("error getting info for GPU %d: %w", i, err)
		}
		numaNode, err := util.GetNumaInformation(i)
		if err != nil {
			klog.ErrorS(err, "failed to get numa information", "device", i)
		}

		healthy := !m.config.ExcludeDevices().Has(i)
		if !healthy {
			klog.Infof("exclude GPU %d from the device list and mark it as unhealthy", i)
		}
		paths, err := gpuInfo.GetPaths()
		if err != nil {
			return fmt.Errorf("error getting device paths for GPU %d: %v", i, err)
		}
		var p2pLinks map[int][]links.P2PLinkType
		if gpuTopologyEnabled {
			if p2pLinks, exists = linksMap[gpuInfo.UUID]; !exists {
				return fmt.Errorf("error getting device links for GPU %d", i)
			}
		}
		gpuDevice := &Device{GPU: &GPUDevice{
			GpuInfo:  gpuInfo,
			NumaNode: numaNode,
			Paths:    paths,
			Healthy:  healthy,
			Links:    p2pLinks,
		}}
		m.devices = append(m.devices, gpuDevice)

		migInfos, err := m.GetMigInfos(gpuInfo)
		if err != nil {
			return fmt.Errorf("error getting MIG infos for GPU %d: %w", i, err)
		}
		for _, migInfo := range migInfos {
			paths, err = migInfo.GetPaths()
			if err != nil {
				return fmt.Errorf("error getting device paths for MIG %s: %v", migInfo.UUID, err)
			}
			migDevice := &Device{MIG: &MIGDevice{
				MigInfo: migInfo,
				Paths:   paths,
				Healthy: healthy,
			}}
			m.devices = append(m.devices, migDevice)
		}
		return nil
	})
	return err
}

func (m *DeviceManager) AssertAllMigDevicesAreValid(uniform bool) error {
	if err := m.Init(); err != nil {
		return err
	}
	defer m.Shutdown()
	err := m.VisitDevices(func(i int, d nvdev.Device) error {
		isMigEnabled, err := d.IsMigEnabled()
		if err != nil {
			return err
		}
		if !isMigEnabled {
			return nil
		}
		migDevices, err := d.GetMigDevices()
		if err != nil {
			return err
		}
		if uniform && len(migDevices) == 0 {
			return fmt.Errorf("device %v has no MIG devices configured", i)
		}
		if !uniform && len(migDevices) == 0 {
			klog.Warningf("device %v has no MIG devices configured", i)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("at least one device with migEnabled=true was not configured correctly: %v", err)
	}

	if !uniform {
		return nil
	}

	var previousAttributes *nvml.DeviceAttributes
	return m.VisitMigDevices(func(i int, d nvdev.Device, j int, m nvdev.MigDevice) error {
		attrs, ret := m.GetAttributes()
		if ret != nvml.SUCCESS {
			return fmt.Errorf("error getting device attributes: %v", ret)
		}
		if previousAttributes == nil {
			previousAttributes = &attrs
		} else if attrs != *previousAttributes {
			return fmt.Errorf("more than one MIG device type present on node")
		}

		return nil
	})
}

func (m *DeviceManager) AllAvailableGpuMigEnabled() bool {
	allMigEnabled := true
	for _, gpuDevice := range m.GetGPUDeviceMap() {
		if !gpuDevice.Healthy { // Skip excluded devices
			continue
		}
		if !gpuDevice.MigEnabled {
			allMigEnabled = false
			break
		}
	}
	return allMigEnabled
}

func (m *DeviceManager) GetGPUDeviceMap() map[string]GPUDevice {
	m.mut.Lock()
	defer m.mut.Unlock()
	deviceMap := make(map[string]GPUDevice)
	for i, dev := range m.devices {
		if dev.GPU != nil {
			deviceMap[dev.GPU.UUID] = *m.devices[i].GPU
		}
	}
	return deviceMap
}

func (m *DeviceManager) GetMIGDeviceMap() map[string]MIGDevice {
	m.mut.Lock()
	defer m.mut.Unlock()
	deviceMap := make(map[string]MIGDevice)
	for i, dev := range m.devices {
		if dev.MIG != nil {
			deviceMap[dev.MIG.UUID] = *m.devices[i].MIG
		}
	}
	return deviceMap
}

func (m *DeviceManager) Start() {
	klog.V(4).Infoln("DeviceManager starting handle notify...")
	go m.handleNotify()
	klog.V(4).Infoln("DeviceManager starting registry node...")
	go m.registryNode()
	klog.V(4).Infoln("DeviceManager starting check health...")
	go func() {
		if err := m.checkHealth(); err != nil {
			klog.Errorf("Failed to start health check: %v; continuing with health checks disabled", err)
		}
	}()
}

func (m *DeviceManager) GetDeviceInfoMap() map[string]device.DeviceInfo {
	nodeDeviceInfo := m.GetNodeDeviceInfo()
	deviceInfoMap := make(map[string]device.DeviceInfo, len(nodeDeviceInfo))
	for i := range nodeDeviceInfo {
		deviceInfoMap[nodeDeviceInfo[i].Uuid] = nodeDeviceInfo[i]
	}
	return deviceInfoMap
}

func (m *DeviceManager) GetNodeDeviceInfo() device.NodeDeviceInfo {
	// Scaling Cores.
	totalCores := int(m.config.DeviceCoresScaling() * float64(util.HundredCore))
	deviceInfos := make(device.NodeDeviceInfo, 0, len(m.devices))
	for _, gpuDevice := range m.GetGPUDeviceMap() {
		// Scaling Memory.
		totalMemory := int(gpuDevice.Memory.Total >> 20) // bytes -> mb
		totalMemory = int(m.config.DeviceMemoryScaling() * float64(totalMemory))
		capability, _ := strconv.ParseFloat(gpuDevice.CudaComputeCapability, 32)
		deviceInfos = append(deviceInfos, device.DeviceInfo{
			Id:         gpuDevice.Index,
			Type:       gpuDevice.ProductName,
			Uuid:       gpuDevice.UUID,
			Core:       totalCores,
			Memory:     totalMemory,
			Number:     m.config.DeviceSplitCount(),
			Numa:       gpuDevice.NumaNode,
			Mig:        gpuDevice.MigEnabled,
			BusId:      links.PciInfo(gpuDevice.PciInfo).BusID(),
			Capability: float32(capability),
			Healthy:    gpuDevice.Healthy,
		})
	}
	return deviceInfos
}

func (m *DeviceManager) handleNotify() {
	stopCh := m.stop
	for {
		select {
		case <-stopCh:
			klog.V(5).Infoln("DeviceManager handle notify has stopped")
			return
		case dev := <-m.unhealthy:
			m.mut.Lock()
			for _, ch := range m.notify {
				ch <- dev
			}
			m.mut.Unlock()
		}
	}
}

func (m *DeviceManager) Stop() {
	klog.Infof("DeviceManager stopping...")
	close(m.stop)
	m.stop = make(chan struct{})
	if err := m.cleanupRegistry(); err != nil {
		klog.ErrorS(err, "cleanup node registry failed")
	}
}

func (m *DeviceManager) modifyDeviceUnHealthy(device *Device) {
	m.mut.Lock()
	defer m.mut.Unlock()
	if device.GPU != nil {
		device.GPU.Healthy = false
	}
	if device.MIG != nil {
		device.MIG.Healthy = false
	}
	m.unhealthy <- device
}
