package manager

import (
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"sync"

	nvdev "github.com/NVIDIA/go-nvlib/pkg/nvlib/device"
	"github.com/NVIDIA/go-nvlib/pkg/nvlib/info"
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

func (d Device) getUuid() string {
	if d.GPU != nil {
		return d.GPU.UUID
	}
	if d.MIG != nil {
		return d.MIG.UUID
	}
	return ""
}

func (d Device) isHealthy() bool {
	if d.GPU != nil {
		return d.GPU.Healthy
	}
	if d.MIG != nil {
		return d.MIG.Healthy
	}
	return false
}

func (d Device) setUnhealthy() {
	if d.GPU != nil {
		d.GPU.Healthy = false
	}
	if d.MIG != nil {
		d.MIG.Healthy = false
	}
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
	reRegister           chan struct{}
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
	manager := &DeviceManager{
		client:               fake.NewSimpleClientset(),
		featureGate:          featuregate.NewFeatureGate(),
		unhealthy:            make(chan *Device),
		reRegister:           make(chan struct{}),
		notify:               make(map[string]chan *Device),
		registryFuncs:        make(map[string]RegistryFunc),
		cleanupRegistryFuncs: make(map[string]RegistryFunc),
	}
	for _, opt := range opts {
		opt(manager)
	}
	return manager
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
	manager := &DeviceManager{
		config:               config,
		reRegister:           make(chan struct{}, 1),
		notify:               make(map[string]chan *Device),
		registryFuncs:        make(map[string]RegistryFunc),
		cleanupRegistryFuncs: make(map[string]RegistryFunc),
	}
	for _, opt := range opts {
		opt(manager)
	}
	if manager.client == nil {
		manager.client = fake.NewSimpleClientset()
	}
	if manager.featureGate == nil {
		manager.featureGate = featuregate.NewFeatureGate()
	}
	if manager.DeviceLib == nil {
		deviceLib, err := nvidia.InitDeviceLib("/")
		if err != nil {
			return nil, err
		}
		manager.DeviceLib = deviceLib
	}
	if err := manager.initDevices(); err != nil {
		return nil, err
	}
	manager.unhealthy = make(chan *Device, len(manager.devices))
	return manager, nil
}

func (m *DeviceManager) initDevices() (err error) {
	if err = m.NvmlInit(); err != nil {
		return err
	}
	defer m.NvmlShutdown()

	m.driverVersion, err = m.DeviceLib.GetDriverVersion()
	if err != nil {
		return err
	}
	var (
		devLinksMap        map[string]map[int][]links.P2PLinkType
		gpuTopologyEnabled = m.featureGate.Enabled(util.GPUTopology)
		exists             = false
	)
	if gpuTopologyEnabled {
		deviceList, err := gpuallocator.NewDevices(
			gpuallocator.WithNvmlLib(m),
			gpuallocator.WithDeviceLib(m),
		)
		if err != nil {
			return fmt.Errorf("error getting gpuallocator device list: %v", err)
		}
		devLinksMap = make(map[string]map[int][]links.P2PLinkType, len(deviceList))
		for _, dev := range deviceList {
			devLinklist := make(map[int][]links.P2PLinkType, len(dev.Links))
			for index, pLinks := range dev.Links {
				p2pLinks := make([]links.P2PLinkType, len(pLinks))
				for i, link := range pLinks {
					p2pLinks[i] = link.Type
				}
				devLinklist[index] = p2pLinks
			}
			devLinksMap[dev.UUID] = devLinklist
		}
	}
	m.imexChannels, err = imex.GetChannels(m.config.GetIMEX(), "/")
	if err != nil {
		return fmt.Errorf("error querying IMEX channels: %w", err)
	}

	if klog.V(5).Enabled() {
		cmd := exec.Command(m.NvidiaSMIPath, "topo", "-m")
		if out, err := cmd.CombinedOutput(); err != nil {
			klog.V(5).ErrorS(err, "failed to execute nvidia-smi", "nvidia-smi", m.NvidiaSMIPath)
		} else {
			klog.V(5).InfoS("nvidia-smi topo -m output", "result", string(out))
		}
	}
	platform := m.ResolvePlatform()

	excludeDevices := m.config.GetExcludeDevices()
	err = m.VisitDevices(func(i int, d nvdev.Device) error {
		gpuInfo, err := m.GetGpuInfo(i, d)
		if err != nil {
			return fmt.Errorf("error getting info for GPU %d: %w", i, err)
		}
		numaNode, _ := gpuInfo.GetNumaNode()

		healthy := true
		if excludeDevices.HasIntID(i) {
			klog.Infof("exclude GPU ID <%d> from the device list and mark it as unhealthy", i)
			healthy = false
		}
		if healthy && excludeDevices.HasStringID(gpuInfo.UUID) {
			klog.Infof("exclude GPU UUID <%s> from the device list and mark it as unhealthy", gpuInfo.UUID)
			healthy = false
		}

		var paths []string
		if platform == info.PlatformWSL {
			wslGpuInfo := nvidia.WslGpuInfo{GpuInfo: gpuInfo}
			paths, _ = wslGpuInfo.GetPaths()
		} else {
			paths, _ = gpuInfo.GetPaths()
		}
		var p2pLinks map[int][]links.P2PLinkType
		if gpuTopologyEnabled {
			if p2pLinks, exists = devLinksMap[gpuInfo.UUID]; !exists {
				return fmt.Errorf("error getting device links for GPU %d", i)
			}
		}
		gpuDevice := &Device{GPU: &GPUDevice{
			GpuInfo:  gpuInfo,
			NumaNode: int(numaNode),
			Paths:    paths,
			Healthy:  healthy,
			Links:    p2pLinks,
		}}
		m.devices = append(m.devices, gpuDevice)

		if m.GetNodeConfig().GetMigStrategy() == util.MigStrategyNone {
			return nil
		}

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
	m.wait.Go(func() {
		klog.Infoln("DeviceManager starting handle notify...")
		m.handleNotify()
	})
	m.wait.Go(func() {
		klog.Infoln("DeviceManager starting registry node devices...")
		m.registryDevices()
	})
	m.wait.Go(func() {
		klog.Infoln("DeviceManager starting check devices health...")
		if err := m.checkHealth(); err != nil {
			klog.ErrorS(err, "Failed to initiate device health check")
		}
	})
	if m.featureGate.Enabled(util.SMWatcher) {
		m.wait.Go(func() {
			klog.Infoln("DeviceManager starting sm watcher...")
			m.doWatcher()
		})
	}
}

func (m *DeviceManager) GetNodeDeviceInfo() device.NodeDeviceInfo {
	// Scaling Cores.
	totalCores := int64(m.config.GetDeviceCoresScaling() * float64(util.HundredCore))
	deviceInfos := make(device.NodeDeviceInfo, 0, len(m.devices))
	for _, gpuDevice := range m.GetGPUDeviceMap() {
		// Scaling Memory.
		totalMemory := int64(gpuDevice.Memory.Total >> 20) // bytes -> mb
		totalMemory = int64(m.config.GetDeviceMemoryScaling() * float64(totalMemory))
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
	sort.Slice(deviceInfos, func(i, j int) bool {
		return deviceInfos[i].Id < deviceInfos[j].Id
	})
	return deviceInfos
}

func (m *DeviceManager) handleNotify() {
	stopCh := m.stop
	for {
		select {
		case <-stopCh:
			klog.V(3).Infoln("DeviceManager handle notify has stopped")
			return
		case dev := <-m.unhealthy:
			if !dev.isHealthy() {
				klog.V(4).Infof("Device: %s is aleady marked unhealthy. Skip notifications", dev.getUuid())
				continue
			}

			m.mut.Lock()
			dev.setUnhealthy()
			select {
			case m.reRegister <- struct{}{}:
			default:
				klog.Errorf("Re registration channel is full, stop sending requests")
			}
			for name, ch := range m.notify {
				select {
				case ch <- dev:
				default:
					klog.Errorf("Notification channel is full. Stop sending device %s unhealthy notifications to %s", dev.getUuid(), name)
				}
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
