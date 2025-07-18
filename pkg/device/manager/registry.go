package manager

import (
	"time"

	"github.com/coldzerofear/vgpu-manager/cmd/device-plugin/options"
	"github.com/coldzerofear/vgpu-manager/pkg/client"
	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"golang.org/x/exp/maps"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"k8s.io/component-base/featuregate"
	"k8s.io/klog/v2"
)

func (m *DeviceManager) GetNodeTopologyInfo() device.NodeTopologyInfo {
	nodeTopo := device.NodeTopologyInfo{}
	for _, dev := range m.devices {
		if dev.GPU == nil {
			continue
		}
		nodeTopo = append(nodeTopo, device.TopologyInfo{
			Index: dev.GPU.Index,
			Links: dev.GPU.Links,
		})
	}
	return nodeTopo
}

func patchNodeMetadata(cli kubernetes.Interface, nodeName string, patchMetadata client.PatchMetadata) error {
	if len(patchMetadata.Annotations) > 0 || len(patchMetadata.Labels) > 0 {
		metadata := client.PatchMetadata{}
		if len(patchMetadata.Annotations) > 0 {
			metadata.Annotations = patchMetadata.Annotations
		}
		if len(patchMetadata.Labels) > 0 {
			metadata.Labels = patchMetadata.Labels
		}
		return retry.OnError(retry.DefaultRetry, util.ShouldRetry, func() error {
			return client.PatchNodeMetadata(cli, nodeName, metadata)
		})
	}
	return nil
}

func (m *DeviceManager) registryDevices() {
	patchMetadata := client.PatchMetadata{
		Annotations: map[string]string{},
		Labels:      map[string]string{},
	}
	featureGate := featuregate.DefaultComponentGlobalsRegistry.FeatureGateFor(options.Component)
	if featureGate == nil {
		featureGate = featuregate.NewFeatureGate()
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	stopCh := m.stop
	cleanCh := m.waitCleanup
	for {
		select {
		case <-stopCh:
			klog.V(1).Infoln("DeviceManager Node registration has stopped")
			m.mut.Lock()
			funcs := maps.Clone(m.cleanupRegistryFuncs)
			m.mut.Unlock()

			maps.Clear(patchMetadata.Labels)
			maps.Clear(patchMetadata.Annotations)
			for name, fn := range funcs {
				metadata, err := fn(featureGate)
				if err != nil {
					klog.ErrorS(err, "Preparing to clean device infos metadata failed", "pluginName", name)
					continue
				}
				if metadata != nil {
					maps.Copy(patchMetadata.Labels, metadata.Labels)
					maps.Copy(patchMetadata.Annotations, metadata.Annotations)
				}
			}
			if err := patchNodeMetadata(m.client, m.config.GetNodeName(), patchMetadata); err != nil {
				klog.ErrorS(err, "Cleanup node device registry infos failed")
			}
			close(cleanCh)
			return
		case <-ticker.C:
			m.mut.Lock()
			funcs := maps.Clone(m.registryFuncs)
			m.mut.Unlock()

			// Reset trigger to detect per second when there are no registered functions.
			if len(funcs) == 0 {
				ticker.Reset(time.Second)
				continue
			}
			maps.Clear(patchMetadata.Labels)
			maps.Clear(patchMetadata.Annotations)
			for name, fn := range funcs {
				metadata, err := fn(featureGate)
				if err != nil {
					klog.ErrorS(err, "Failed to prepare devices metadata", "pluginName", name)
					continue
				}
				if metadata != nil {
					maps.Copy(patchMetadata.Labels, metadata.Labels)
					maps.Copy(patchMetadata.Annotations, metadata.Annotations)
				}
			}
			if err := patchNodeMetadata(m.client, m.config.GetNodeName(), patchMetadata); err != nil {
				klog.ErrorS(err, "Registry node devices metadata failed")
				ticker.Reset(10 * time.Second)
			} else {
				ticker.Reset(30 * time.Second)
			}
		}
	}
}
