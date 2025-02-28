package manager

import (
	"strconv"
	"time"

	"github.com/coldzerofear/vgpu-manager/cmd/device-plugin/options"
	"github.com/coldzerofear/vgpu-manager/pkg/client"
	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/component-base/featuregate"
	"k8s.io/klog/v2"
)

func (m *DeviceManager) getNodeTopologyInfo() device.NodeTopologyInfo {
	nodeTopo := device.NodeTopologyInfo{}
	for _, dev := range m.devices {
		if dev.MIG != nil {
			continue
		}
		nodeTopo = append(nodeTopo, device.TopologyInfo{
			Index: dev.GPU.Index,
			Links: dev.GPU.Links,
		})
	}
	return nodeTopo
}

func (m *DeviceManager) registryNode() {
	featureGate := featuregate.DefaultComponentGlobalsRegistry.FeatureGateFor(options.Component)
	gpuTopologyEnabled := featureGate != nil && featureGate.Enabled(options.GPUTopology)
	registryNode := func() error {
		nodeDeviceInfos := m.GetNodeDeviceInfo()
		registryGPUs, err := nodeDeviceInfos.Encode()
		if err != nil {
			return err
		}
		heartbeatTime, err := metav1.NowMicro().MarshalText()
		if err != nil {
			return err
		}
		var registryGPUTopology string
		if gpuTopologyEnabled {
			nodeTopo := m.getNodeTopologyInfo()
			registryGPUTopology, err = nodeTopo.Encode()
			if err != nil {
				return err
			}
		}
		patchData := client.PatchMetadata{
			Annotations: map[string]string{
				util.NodeDeviceRegisterAnnotation:  registryGPUs,
				util.NodeDeviceHeartbeatAnnotation: string(heartbeatTime),
				util.NodeDeviceTopologyAnnotation:  registryGPUTopology,
				util.DeviceMemoryFactorAnnotation:  strconv.Itoa(m.config.DeviceMemoryFactor()),
			},
			Labels: map[string]string{
				util.NodeNvidiaDriverVersionLabel: m.driverVersion.DriverVersion,
				util.NodeNvidiaCudaVersionLabel:   strconv.Itoa(int(m.driverVersion.CudaDriverVersion)),
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

func (m *DeviceManager) cleanupRegistry() error {
	patchData := client.PatchMetadata{
		Annotations: map[string]string{
			util.NodeDeviceHeartbeatAnnotation: "",
			util.NodeDeviceRegisterAnnotation:  "",
			util.NodeDeviceTopologyAnnotation:  "",
		},
	}
	return retry.OnError(retry.DefaultRetry, util.ShouldRetry, func() error {
		return client.PatchNodeMetadata(m.client, m.config.NodeName(), patchData)
	})
}
