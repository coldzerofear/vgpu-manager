package manager

import (
	"fmt"
	"strconv"
	"sync"

	nvdev "github.com/NVIDIA/go-nvlib/pkg/nvlib/device"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/coldzerofear/vgpu-manager/pkg/client"
	"github.com/coldzerofear/vgpu-manager/pkg/config/node"
	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/device/gpuallocator"
	"github.com/coldzerofear/vgpu-manager/pkg/device/gpuallocator/links"
	"github.com/coldzerofear/vgpu-manager/pkg/device/imex"
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

type RegistryFunc func(featuregate.FeatureGate) (*client.PatchMetadata, error)

type DeviceManager struct {
	*nvidia.DeviceLib
	mut           sync.Mutex
	client        kubernetes.Interface
	config        *node.NodeConfigSpec
	driverVersion nvidia.DriverVersion
	devices       []*Device
	imexChannels  imex.Channels
	featureGate   featuregate.FeatureGate

	unhealthy            chan *Device
	notify               map[string]chan *Device
	registryFuncs        map[string]RegistryFunc
	cleanupRegistryFuncs map[string]RegistryFunc
	stop                 chan struct{}
	wait                 sync.WaitGroup
}

func (m *DeviceManager) GetDriverVersion() nvidia.DriverVersion {
	return m.driverVersion
}

func (m *DeviceManager) GetNodeConfig() node.NodeConfigSpec {
	return *m.config
}

func (m *DeviceManager) GetImexChannels() imex.Channels {
	return m.imexChannels
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

func (m *DeviceManager) AddRegistryFunc(name string, fn RegistryFunc) {
	m.mut.Lock()
	m.registryFuncs[name] = fn
	m.mut.Unlock()
}

func (m *DeviceManager) RemoveRegistryFunc(name string) {
	m.mut.Lock()
	delete(m.registryFuncs, name)
	m.mut.Unlock()
}

func (m *DeviceManager) AddCleanupRegistryFunc(name string, fn RegistryFunc) {
	m.mut.Lock()
	m.cleanupRegistryFuncs[name] = fn
	m.mut.Unlock()
}

func (m *DeviceManager) RemoveCleanupRegistryFunc(name string) {
	m.mut.Lock()
	delete(m.cleanupRegistryFuncs, name)
	m.mut.Unlock()
}

func NewFakeDeviceManager(opts ...OptionFunc) *DeviceManager {
	m := &DeviceManager{
		client:               fake.NewSimpleClientset(),
		featureGate:          featuregate.NewFeatureGate(),
		unhealthy:            make(chan *Device),
		notify:               make(map[string]chan *Device),
		registryFuncs:        make(map[string]RegistryFunc),
		cleanupRegistryFuncs: make(map[string]RegistryFunc),
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

type OptionFunc func(*DeviceManager)

func WithDevices(devices []*Device) OptionFunc {
	return func(m *DeviceManager) {
		m.devices = devices
	}
}

func WithNvidiaVersion(version nvidia.DriverVersion) OptionFunc {
	return func(m *DeviceManager) {
		m.driverVersion = version
	}
}

func WithNodeConfigSpec(config *node.NodeConfigSpec) OptionFunc {
	return func(d *DeviceManager) {
		d.config = config
	}
}

func WithFeatureGate(featureGate featuregate.FeatureGate) OptionFunc {
	return func(d *DeviceManager) {
		d.featureGate = featureGate
	}
}

func WithKubeClient(kubeClient *kubernetes.Clientset) OptionFunc {
	return func(d *DeviceManager) {
		d.client = kubeClient
	}
}

func WithDeviceLib(lib *nvidia.DeviceLib) OptionFunc {
	return func(d *DeviceManager) {
		d.DeviceLib = lib
	}
}

func NewDeviceManager(config *node.NodeConfigSpec, opts ...OptionFunc) (*DeviceManager, error) {
	m := &DeviceManager{
		config:               config,
		unhealthy:            make(chan *Device),
		notify:               make(map[string]chan *Device),
		registryFuncs:        make(map[string]RegistryFunc),
		cleanupRegistryFuncs: make(map[string]RegistryFunc),
	}
	for _, opt := range opts {
		opt(m)
	}
	if m.client == nil {
		m.client = fake.NewSimpleClientset()
	}
	if m.DeviceLib == nil {
		driverlib, err := nvidia.NewDeviceLib("/")
		if err != nil {
			klog.Error("If this is a GPU node, did you configure the NVIDIA Container Toolkit?")
			klog.Error("You can check the prerequisites at: https://github.com/NVIDIA/k8s-device-plugin#prerequisites")
			klog.Error("You can learn how to set the runtime at: https://github.com/NVIDIA/k8s-device-plugin#quick-start")
			klog.Error("If this is not a GPU node, you should set up a toleration or nodeSelector to only deploy this plugin on GPU nodes")
			return nil, err
		}
		m.DeviceLib = driverlib
	}
	if m.featureGate == nil {
		m.featureGate = featuregate.NewFeatureGate()
	}
	if err := m.NvmlInit(); err != nil {
		klog.Errorf("If this is a GPU node, did you set the default container runtime to `nvidia`?")
		klog.Errorf("You can check the prerequisites at: https://github.com/NVIDIA/k8s-device-plugin#prerequisites")
		klog.Errorf("You can learn how to set the runtime at: https://github.com/NVIDIA/k8s-device-plugin#quick-start")
		klog.Errorf("If this is not a GPU node, you should set up a toleration or nodeSelector to only deploy this plugin on GPU nodes")
		return nil, err
	}
	defer m.NvmlShutdown()
	err := m.initDevices()
	return m, err
}

func (m *DeviceManager) initDevices() (err error) {
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
		gpuTopologyEnabled = m.featureGate.Enabled(util.GPUTopology)
		exists             = false
	)
	if gpuTopologyEnabled {
		deviceList, err := gpuallocator.NewDevices(
			gpuallocator.WithDeviceLib(m),
			gpuallocator.WithNvmlLib(m),
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
	m.imexChannels, err = imex.GetChannels(m.config.GetIMEX(), "/")
	if err != nil {
		return fmt.Errorf("error querying IMEX channels: %w", err)
	}

	excludeDevices := m.config.GetExcludeDevices()
	err = m.VisitDevices(func(i int, d nvdev.Device) error {
		gpuInfo, err := m.GetGpuInfo(i, d)
		if err != nil {
			return fmt.Errorf("error getting info for GPU %d: %w", i, err)
		}
		numaNode, err := util.GetNumaInformation(m.NvidiaSMIPath, i)
		if err != nil {
			klog.ErrorS(err, "failed to get numa information", "nvidia-smi", m.NvidiaSMIPath, "device", i)
		}

		healthy := true
		if excludeDevices.HasIntID(i) {
			klog.Infof("exclude GPU ID <%d> from the device list and mark it as unhealthy", i)
			healthy = false
		}
		if healthy && excludeDevices.HasStringID(gpuInfo.UUID) {
			klog.Infof("exclude GPU UUID <%s> from the device list and mark it as unhealthy", gpuInfo.UUID)
			healthy = false
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
	if err := m.NvmlInit(); err != nil {
		return err
	}
	defer m.NvmlShutdown()
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

func (m *DeviceManager) GetFeatureGate() featuregate.FeatureGate {
	return m.featureGate
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

func (m *DeviceManager) initialize() {
	m.stop = make(chan struct{})
}

func (m *DeviceManager) Start() {
	m.Stop()
	m.initialize()
	klog.V(4).Infoln("DeviceManager starting handle notify...")
	go m.handleNotify()
	klog.V(4).Infoln("DeviceManager starting registry devices...")
	go m.registryDevices()
	klog.V(4).Infoln("DeviceManager starting check health...")
	go func() {
		if err := m.checkHealth(); err != nil {
			klog.Errorf("Failed to start health check: %v; continuing with health checks disabled", err)
		}
	}()
	go m.doWatcher()
}

func (m *DeviceManager) GetNodeDeviceInfo() device.NodeDeviceInfo {
	// Scaling Cores.
	totalCores := int(m.config.GetDeviceCoresScaling() * float64(util.HundredCore))
	deviceInfos := make(device.NodeDeviceInfo, 0, len(m.devices))
	for _, gpuDevice := range m.GetGPUDeviceMap() {
		// Scaling Memory.
		totalMemory := int(gpuDevice.Memory.Total >> 20) // bytes -> mb
		totalMemory = int(m.config.GetDeviceMemoryScaling() * float64(totalMemory))
		capability, _ := strconv.ParseFloat(gpuDevice.CudaComputeCapability, 32)
		deviceInfos = append(deviceInfos, device.DeviceInfo{
			Id:         gpuDevice.Index,
			Type:       gpuDevice.ProductName,
			Uuid:       gpuDevice.UUID,
			Core:       totalCores,
			Memory:     totalMemory,
			Number:     m.config.GetDeviceSplitCount(),
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
	m.wait.Add(1)
	defer m.wait.Done()
	for {
		select {
		case <-stopCh:
			klog.V(5).Infoln("DeviceManager handle notify has stopped")
			return
		case dev := <-m.unhealthy:
			m.mut.Lock()
			if dev.GPU != nil {
				dev.GPU.Healthy = false
			}
			if dev.MIG != nil {
				dev.MIG.Healthy = false
			}
			for _, ch := range m.notify {
				ch <- dev
			}
			m.mut.Unlock()
		}
	}
}

func (m *DeviceManager) Stop() {
	if m.stop != nil {
		klog.Infof("DeviceManager stopping...")
		close(m.stop)
		m.wait.Wait()
		m.stop = nil
	}
}
