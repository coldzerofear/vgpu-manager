package checkpoint

import (
	"encoding/json"
	"os"
	"path/filepath"

	"k8s.io/klog/v2"
)

const (
	CheckPointFileName = "kubelet_internal_checkpoint"

	ContainerNameLabelKey = "io.kubernetes.container.name"
	PodNamespaceLabelKey  = "io.kubernetes.pod.namespace"
	PodNameLabelKey       = "io.kubernetes.pod.name"
	PodUIDLabelKey        = "io.kubernetes.pod.uid"
)

type DevicesPerNUMA map[int64][]string

type PodDevicesEntry struct {
	PodUID        string
	ContainerName string
	ResourceName  string
	DeviceIDs     []string
	AllocResp     []byte
}

type PodDevicesEntryNUMA struct {
	PodUID        string
	ContainerName string
	ResourceName  string
	DeviceIDs     DevicesPerNUMA
	AllocResp     []byte
}

type CheckpointNUMA struct {
	PodDeviceEntries  []PodDevicesEntryNUMA
	RegisteredDevices map[string][]string
}

type Checkpoint struct {
	PodDeviceEntries  []PodDevicesEntry
	RegisteredDevices map[string][]string
}

type CheckpointDataNUMA struct {
	Data *CheckpointNUMA `json:"Data"`
}

type CheckpointData struct {
	Data *Checkpoint `json:"Data"`
}

func GetDevicePluginCheckpointData(devicePluginPath string) (*Checkpoint, error) {
	cpFile := filepath.Join(devicePluginPath, CheckPointFileName)
	data, err := os.ReadFile(cpFile)
	if err != nil {
		return nil, err
	}
	klog.V(4).Infof("Try NUMA checkpoint data format")
	cpNUMAData := &CheckpointDataNUMA{}
	if err = json.Unmarshal(data, cpNUMAData); err == nil {
		podDevicesEntryies := make([]PodDevicesEntry, len(cpNUMAData.Data.PodDeviceEntries))
		for i, v := range cpNUMAData.Data.PodDeviceEntries {
			podDevicesEntry := PodDevicesEntry{
				PodUID:        v.PodUID,
				ContainerName: v.ContainerName,
				ResourceName:  v.ResourceName,
				DeviceIDs:     make([]string, 0),
				AllocResp:     v.AllocResp,
			}
			for _, devices := range v.DeviceIDs {
				podDevicesEntry.DeviceIDs = append(podDevicesEntry.DeviceIDs, devices...)
			}
			podDevicesEntryies[i] = podDevicesEntry
		}
		return &Checkpoint{
			RegisteredDevices: cpNUMAData.Data.RegisteredDevices,
			PodDeviceEntries:  podDevicesEntryies,
		}, nil
	}
	klog.V(4).Infof("Failed NUMA checkpoint data format: %v", err)
	klog.V(4).Infof("Try use checkpoint v2 data format")
	cpV2Data := &CheckpointData{}
	if err = json.Unmarshal(data, cpV2Data); err != nil {
		return nil, err
	}
	if cpV2Data.Data != nil {
		return cpV2Data.Data, nil
	}
	klog.V(4).Infof("Try use checkpoint v1 data format")
	cpV1Data := &Checkpoint{}
	if err = json.Unmarshal(data, cpV1Data); err != nil {
		return nil, err
	}
	return cpV1Data, nil
}
