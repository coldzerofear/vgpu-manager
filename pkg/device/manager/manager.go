package manager

import (
	"fmt"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/coldzerofear/vgpu-manager/pkg/client"
	"github.com/coldzerofear/vgpu-manager/pkg/config/node"
	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/coldzerofear/vgpu-manager/pkg/version"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

type GPUDevice struct {
	device.GPUInfo
	//Paths     []string
	MinorNumber int
	MigDevice   bool
}

type DeviceManager struct {
	mut     sync.Mutex
	client  kubernetes.Interface
	config  *node.NodeConfig
	version version.Version
	devices []*GPUDevice

	unhealthy chan *GPUDevice
	notify    map[string]chan *pluginapi.Device
	stop      chan struct{}
}

func (m *DeviceManager) GetVersion() version.Version {
	return m.version
}

func (m *DeviceManager) GetNodeConfig() node.NodeConfig {
	return *m.config
}

func (m *DeviceManager) AddNotifyChannel(name string, ch chan *pluginapi.Device) {
	m.mut.Lock()
	m.notify[name] = ch
	m.mut.Unlock()
}

func (m *DeviceManager) RemoveNotifyChannel(name string) {
	m.mut.Lock()
	delete(m.notify, name)
	m.mut.Unlock()
}

func NewFakeDeviceManager(config *node.NodeConfig, version version.Version, devices []*GPUDevice) *DeviceManager {
	return &DeviceManager{
		version:   version,
		config:    config,
		devices:   devices,
		client:    fake.NewSimpleClientset(),
		stop:      make(chan struct{}),
		unhealthy: make(chan *GPUDevice),
		notify:    make(map[string]chan *pluginapi.Device),
	}
}

func NewDeviceManager(config *node.NodeConfig, kubeClient *kubernetes.Clientset) *DeviceManager {
	m := &DeviceManager{
		version:   version.Version{},
		config:    config,
		client:    kubeClient,
		stop:      make(chan struct{}),
		unhealthy: make(chan *GPUDevice),
		notify:    make(map[string]chan *pluginapi.Device),
	}
	return m.initDevices()
}

// DeviceHandleIsMigEnabled Determine if Mig mode is enabled
func DeviceHandleIsMigEnabled(dev nvml.Device) (bool, nvml.Return) {
	cm, pm, rt := dev.GetMigMode()
	if rt == nvml.ERROR_NOT_SUPPORTED {
		return false, nvml.SUCCESS
	}
	if rt != nvml.SUCCESS {
		return false, rt
	}
	return cm == nvml.DEVICE_MIG_ENABLE && cm == pm, rt
}

func (m *DeviceManager) initDevices() *DeviceManager {
	rt := nvml.Init()
	handlerReturn(rt)
	defer nvml.Shutdown()

	count, rt := nvml.DeviceGetCount()
	handlerReturn(rt)

	m.devices = make([]*GPUDevice, count)

	driverVersion, rt := nvml.SystemGetDriverVersion()
	handlerReturn(rt)
	m.version.DriverVersion = driverVersion
	cudaVersion, rt := nvml.SystemGetCudaDriverVersion_v2()
	handlerReturn(rt)
	m.version.CudaVersion = version.CudaVersion(cudaVersion)

	// TODO Scaling Core
	totalCore := int(m.config.DeviceCoresScaling() * float64(util.HundredCore))
	var err error
	for i := 0; i < count; i++ {
		klog.V(2).Infof("nvmlDeviceGetHandleByIndex <%d>", i)

		devHandle, rt := nvml.DeviceGetHandleByIndex(i)
		handlerReturn(rt)

		uuid, rt := devHandle.GetUUID()
		handlerReturn(rt)

		minorNumber, rt := devHandle.GetMinorNumber()
		handlerReturn(rt)

		migEnabled, rt := DeviceHandleIsMigEnabled(devHandle)
		handlerReturn(rt)

		migDevice, rt := devHandle.IsMigDeviceHandle()
		handlerReturn(rt)

		memory_v2, rt := devHandle.GetMemoryInfo_v2()
		handlerReturn(rt)
		totalMemory := int(memory_v2.Total >> 20) // bytes -> mb
		// TODO Scaling Memory
		totalMemory = int(m.config.DeviceMemoryScaling() * float64(totalMemory))

		devType, rt := devHandle.GetName()
		handlerReturn(rt)

		major, minor, rt := devHandle.GetCudaComputeCapability()
		handlerReturn(rt)
		level := fmt.Sprintf("%d%d", major, minor)
		capability, _ := strconv.Atoi(level)

		minor, rt = devHandle.GetMinorNumber()
		handlerReturn(rt)

		numaNode := 0
		//var paths []string
		if migDevice {
			parentDev, rt := devHandle.GetDeviceHandleFromMigDeviceHandle()
			handlerReturn(rt)
			index, rt := parentDev.GetIndex()
			handlerReturn(rt)

			numaNode, err = util.GetNumaInformation(index)
			if err != nil {
				klog.ErrorS(err, "failed to get numa information", "device", index)
			}
			//numaNode, _ = parentDev.GetNumaNodeId()
			//handlerReturn(rt)

		} else {
			numaNode, err = util.GetNumaInformation(i)
			if err != nil {
				klog.ErrorS(err, "failed to get numa information", "device", i)
			}
			//numaNode, _ = devHandle.GetNumaNodeId()
			//handlerReturn(rt)
			//paths = append(paths, fmt.Sprintf("/dev/nvidia%d", minor))
		}
		if numaNode < 0 {
			numaNode = 0
		}

		device := &GPUDevice{
			GPUInfo: device.GPUInfo{
				Id:         i,
				Uuid:       uuid,
				Core:       totalCore,
				Memory:     totalMemory,
				Type:       devType,
				Mig:        migEnabled || migDevice,
				Number:     m.config.DeviceSplitCount(),
				Capability: capability,
				Numa:       numaNode,
				Healthy:    !m.config.ExcludeDevices().Has(i),
			},
			//Paths: paths,
			MinorNumber: minorNumber,
			MigDevice:   migDevice,
		}
		m.devices[i] = device
	}
	return m
}

func (m *DeviceManager) GetDeviceMap() map[string]GPUDevice {
	m.mut.Lock()
	defer m.mut.Unlock()
	deviceMap := make(map[string]GPUDevice)
	for i, dev := range m.devices {
		deviceMap[dev.Uuid] = *m.devices[i]
	}
	return deviceMap
}

func (m *DeviceManager) GetDevices() []GPUDevice {
	m.mut.Lock()
	defer m.mut.Unlock()
	devices := make([]GPUDevice, len(m.devices))
	for i := range m.devices {
		devices[i] = *m.devices[i]
	}
	return devices
}

func (m *DeviceManager) Start() {
	klog.V(4).Infoln("DeviceManager starting check health...")
	go m.checkHealth()
	klog.V(4).Infoln("DeviceManager starting handle notify...")
	go m.handleNotify()
	klog.V(4).Infoln("DeviceManager starting registry node...")
	go m.registryNode()
}

func (m *DeviceManager) cleanupRegistry() error {
	patchData := client.PatchMetadata{
		Annotations: map[string]string{
			util.NodeDeviceHeartbeatAnnotation: "",
			util.NodeDeviceRegisterAnnotation:  "",
		},
	}
	return retry.OnError(retry.DefaultRetry, util.ShouldRetry, func() error {
		return client.PatchNodeMetadata(m.client, m.config.NodeName(), patchData)
	})
}

func (m *DeviceManager) registryNode() {
	registryNode := func() error {
		devices := m.GetDevices()
		nodeDeviceInfos := make(device.NodeDeviceInfos, len(devices))
		for i := range devices {
			nodeDeviceInfos[i] = devices[i].GPUInfo
		}
		registryGPUs, err := nodeDeviceInfos.Encode()
		if err != nil {
			return err
		}
		heartbeatTime, err := metav1.NowMicro().MarshalText()
		if err != nil {
			return err
		}
		patchData := client.PatchMetadata{
			Annotations: map[string]string{
				util.NodeDeviceRegisterAnnotation:  registryGPUs,
				util.NodeDeviceHeartbeatAnnotation: string(heartbeatTime),
				util.DeviceMemoryFactorAnnotation:  strconv.Itoa(m.config.DeviceMemoryFactor()),
			},
			Labels: map[string]string{
				util.NodeNvidiaDriverVersionLabel: m.version.DriverVersion,
				util.NodeNvidiaCudaVersionLabel:   strconv.Itoa(int(m.version.CudaVersion)),
			},
		}
		return retry.OnError(retry.DefaultRetry, util.ShouldRetry, func() error {
			return client.PatchNodeMetadata(m.client, m.config.NodeName(), patchData)
		})
	}
	stopCh := m.stop
	for {
		select {
		case <-stopCh:
			klog.V(5).Infoln("DeviceManager Node registration has stopped")
			return
		default:
			if err := registryNode(); err != nil {
				klog.ErrorS(err, "Registry node device infos failed")
				time.Sleep(time.Second * 5)
			} else {
				time.Sleep(time.Second * 30)
			}
		}
	}
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
			device := &pluginapi.Device{
				ID:     dev.Uuid,
				Health: pluginapi.Unhealthy,
			}
			for _, ch := range m.notify {
				ch <- device
			}
			m.mut.Unlock()
		}
	}
}

func (m *DeviceManager) Stop() {
	klog.Infof("DeviceManager stopping...")
	close(m.stop)
	m.stop = make(chan struct{})
	time.Sleep(2 * time.Second)
	if err := m.cleanupRegistry(); err != nil {
		klog.ErrorS(err, "cleanup node registry failed")
	}
}

func handlerReturn(r nvml.Return) {
	if r != nvml.SUCCESS {
		klog.Infoln("Did you enable the device plugin feature gate?")
		klog.Infoln("You can check the prerequisites at: https://github.com/NVIDIA/k8s-device-plugin#prerequisites")
		klog.Infoln("You can learn how to set the runtime at: https://github.com/NVIDIA/k8s-device-plugin#quick-start")

		stack := debug.Stack()
		klog.Fatalf("Nvml call failed: %v\nStack trace:\n%s", nvml.ErrorString(r), string(stack))
	}
}

const (
	// envDisableHealthChecks defines the environment variable that is checked to determine whether healthchecks
	// should be disabled. If this envvar is set to "all" or contains the string "xids", healthchecks are
	// disabled entirely. If set, the envvar is treated as a comma-separated list of Xids to ignore. Note that
	// this is in addition to the Application errors that are already ignored.
	envDisableHealthChecks = "DP_DISABLE_HEALTHCHECKS"
	allHealthChecks        = "xids"

	// maxSuccessiveEventErrorCount sets the number of errors waiting for events before marking all devices as unhealthy.
	maxSuccessiveEventErrorCount = 3
)

func (m *DeviceManager) modifyDeviceUnHealthy(device *GPUDevice) {
	m.mut.Lock()
	device.Healthy = false
	m.unhealthy <- device
	m.mut.Unlock()
}

// CheckHealth performs health checks on a set of devices, writing to the 'unhealthy' channel with any unhealthy devices
func (m *DeviceManager) checkHealth() {
	disableHealthChecks := strings.ToLower(os.Getenv(envDisableHealthChecks))
	if disableHealthChecks == "all" {
		disableHealthChecks = allHealthChecks
	}
	if strings.Contains(disableHealthChecks, "xids") {
		return
	}

	ret := nvml.Init()
	handlerReturn(ret)
	defer nvml.Shutdown()

	// FIXME: formalize the full list and document it.
	// http://docs.nvidia.com/deploy/xid-errors/index.html#topic_4
	// Application errors: the GPU should still be healthy
	applicationErrorXids := []uint64{
		13, // Graphics Engine Exception
		31, // GPU memory page fault
		43, // GPU stopped processing
		45, // Preemptive cleanup, due to previous errors
		68, // Video processor exception
	}

	skippedXids := make(map[uint64]bool)
	for _, id := range applicationErrorXids {
		skippedXids[id] = true
	}

	for _, additionalXid := range getAdditionalXids(disableHealthChecks) {
		skippedXids[additionalXid] = true
	}

	eventSet, ret := nvml.EventSetCreate()
	handlerReturn(ret)
	defer eventSet.Free()

	parentToDeviceMap := make(map[string]*GPUDevice)
	deviceIDToGiMap := make(map[string]int)
	deviceIDToCiMap := make(map[string]int)

	eventMask := uint64(nvml.EventTypeXidCriticalError | nvml.EventTypeDoubleBitEccError | nvml.EventTypeSingleBitEccError)
	for i, d := range m.devices {
		devHandle, ret := nvml.DeviceGetHandleByUUID(d.Uuid)
		if ret != nvml.SUCCESS {
			klog.Infof("unable to get device handle from %s: %v; marking it as unhealthy.", d.Uuid, ret)
			m.modifyDeviceUnHealthy(m.devices[i])
			continue
		}

		uuid, gi, ci, err := getDevicePlacement(d)
		if err != nil {
			klog.Warningf("Could not determine device placement for %v: %v; Marking it unhealthy.", d.Uuid, err)
			m.modifyDeviceUnHealthy(m.devices[i])
			continue
		}
		deviceIDToGiMap[d.Uuid] = gi
		deviceIDToCiMap[d.Uuid] = ci
		parentToDeviceMap[uuid] = m.devices[i]

		supportedEvents, ret := devHandle.GetSupportedEventTypes()
		if ret != nvml.SUCCESS {
			klog.Infof("Unable to determine the supported events for %v: %v; marking it as unhealthy.", d.Uuid, ret)
			m.modifyDeviceUnHealthy(m.devices[i])
			continue
		}

		ret = devHandle.RegisterEvents(eventMask&supportedEvents, eventSet)
		if ret == nvml.ERROR_NOT_SUPPORTED {
			klog.Warningf("Device %v is too old to support health checking.", d.Uuid)
		}
		if ret != nvml.SUCCESS {
			klog.Infof("Marking device %v as unhealthy: %v", d.Uuid, ret)
			m.modifyDeviceUnHealthy(m.devices[i])
		}
	}
	stopCh := m.stop
	for {
		select {
		case <-stopCh:
			klog.V(5).Infoln("DeviceManager check health has stopped")
			return
		default:
		}

		e, ret := eventSet.Wait(5000)
		if ret == nvml.ERROR_TIMEOUT {
			klog.V(5).Infoln("waiting for event timeout")
			continue
		}
		if ret != nvml.SUCCESS {
			klog.Infof("Error waiting for event: %v; Marking all devices as unhealthy", ret)
			for i := range m.devices {
				m.modifyDeviceUnHealthy(m.devices[i])
			}
			continue
		}

		if e.EventType != nvml.EventTypeXidCriticalError {
			klog.Infof("Skipping non-nvmlEventTypeXidCriticalError event: %+v", e)
			continue
		}

		if skippedXids[e.EventData] {
			klog.Infof("Skipping event %+v", e)
			continue
		}

		klog.Infof("Processing event %+v", e)
		eventUUID, ret := e.Device.GetUUID()
		if ret != nvml.SUCCESS {
			// If we cannot reliably determine the device UUID, we mark all devices as unhealthy.
			klog.Infof("Failed to determine uuid for event %v: %v; Marking all devices as unhealthy.", e, ret)
			for i := range m.devices {
				m.modifyDeviceUnHealthy(m.devices[i])
			}
			continue
		}

		d, exists := parentToDeviceMap[eventUUID]
		if !exists {
			klog.Infof("Ignoring event for unexpected device: %v", eventUUID)
			continue
		}

		if d.Mig && e.GpuInstanceId != 0xFFFFFFFF && e.ComputeInstanceId != 0xFFFFFFFF {
			gi := deviceIDToGiMap[d.Uuid]
			ci := deviceIDToCiMap[d.Uuid]
			if !(uint32(gi) == e.GpuInstanceId && uint32(ci) == e.ComputeInstanceId) {
				continue
			}
			klog.Infof("Event for mig device %v (gi=%v, ci=%v)", d.Uuid, gi, ci)
		}

		klog.Infof("XidCriticalError: Xid=%d on Device=%s; marking device as unhealthy.", e.EventData, d.Uuid)
		m.modifyDeviceUnHealthy(d)
	}
}

// getAdditionalXids returns a list of additional Xids to skip from the specified string.
// The input is treaded as a comma-separated string and all valid uint64 values are considered as Xid values. Invalid values
// are ignored.
func getAdditionalXids(input string) []uint64 {
	if input == "" {
		return nil
	}

	var additionalXids []uint64
	for _, additionalXid := range strings.Split(input, ",") {
		trimmed := strings.TrimSpace(additionalXid)
		if trimmed == "" {
			continue
		}
		xid, err := strconv.ParseUint(trimmed, 10, 64)
		if err != nil {
			klog.Infof("Ignoring malformed Xid value %v: %v", trimmed, err)
			continue
		}
		additionalXids = append(additionalXids, xid)
	}

	return additionalXids
}

// getDevicePlacement returns the placement of the specified device.
// For a MIG device the placement is defined by the 3-tuple <parent UUID, GI, CI>
// For a full device the returned 3-tuple is the device's uuid and 0xFFFFFFFF for the other two elements.
func getDevicePlacement(d *GPUDevice) (string, int, int, error) {
	if !d.MigDevice {
		return d.Uuid, 0xFFFFFFFF, 0xFFFFFFFF, nil
	}
	return getMigDeviceParts(d)
}

// getMigDeviceParts returns the parent GI and CI ids of the MIG device.
func getMigDeviceParts(d *GPUDevice) (string, int, int, error) {
	if !d.MigDevice {
		return d.Uuid, 0xFFFFFFFF, 0xFFFFFFFF, nil
	}
	// For older driver versions, the call to DeviceGetHandleByUUID will fail for MIG devices.
	mig, ret := nvml.DeviceGetHandleByUUID(d.Uuid)
	if ret == nvml.SUCCESS {
		parentHandle, ret := mig.GetDeviceHandleFromMigDeviceHandle()
		if ret != nvml.SUCCESS {
			return "", 0, 0, fmt.Errorf("failed to get parent device handle: %v", ret)
		}

		parentUUID, ret := parentHandle.GetUUID()
		if ret != nvml.SUCCESS {
			return "", 0, 0, fmt.Errorf("failed to get parent uuid: %v", ret)
		}
		gi, ret := mig.GetGpuInstanceId()
		if ret != nvml.SUCCESS {
			return "", 0, 0, fmt.Errorf("failed to get GPU Instance ID: %v", ret)
		}

		ci, ret := mig.GetComputeInstanceId()
		if ret != nvml.SUCCESS {
			return "", 0, 0, fmt.Errorf("failed to get Compute Instance ID: %v", ret)
		}
		return parentUUID, gi, ci, nil
	}
	return parseMigDeviceUUID(d.Uuid)
}

// parseMigDeviceUUID splits the MIG device UUID into the parent device UUID and ci and gi
func parseMigDeviceUUID(mig string) (string, int, int, error) {
	tokens := strings.SplitN(mig, "-", 2)
	if len(tokens) != 2 || tokens[0] != "MIG" {
		return "", 0, 0, fmt.Errorf("unable to parse UUID as MIG device")
	}

	tokens = strings.SplitN(tokens[1], "/", 3)
	if len(tokens) != 3 || !strings.HasPrefix(tokens[0], "GPU-") {
		return "", 0, 0, fmt.Errorf("unable to parse UUID as MIG device")
	}

	gi, err := strconv.Atoi(tokens[1])
	if err != nil {
		return "", 0, 0, fmt.Errorf("unable to parse UUID as MIG device")
	}

	ci, err := strconv.Atoi(tokens[2])
	if err != nil {
		return "", 0, 0, fmt.Errorf("unable to parse UUID as MIG device")
	}

	return tokens[0], gi, ci, nil
}
