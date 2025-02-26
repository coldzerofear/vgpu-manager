package manager

import (
	"strconv"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/client"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
)

func (m *DeviceManager) registryNode() {
	registryNode := func() error {
		nodeDeviceInfos := m.GetNodeDeviceInfos()
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
		},
	}
	return retry.OnError(retry.DefaultRetry, util.ShouldRetry, func() error {
		return client.PatchNodeMetadata(m.client, m.config.NodeName(), patchData)
	})
}
