package node

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/coldzerofear/vgpu-manager/cmd/device-plugin/options"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
)

type NodeConfigMap struct {
	NodeName            string   `json:"nodeName"`
	CGroupDriver        *string  `json:"cgroupDriver,omitempty"`
	DeviceSplitCount    *int     `json:"deviceSplitCount,omitempty"`
	DeviceMemoryScaling *float64 `json:"deviceMemoryScaling,omitempty"`
	DeviceMemoryFactor  *int     `json:"deviceMemoryFactor,omitempty"`
	DeviceCoresScaling  *float64 `json:"deviceCoresScaling,omitempty"`
	ExcludeDevices      *string  `json:"excludeDevices,omitempty"`
}

type NodeConfig struct {
	nodeName            string
	cgroupDriver        string
	devicePluginPath    string
	deviceSplitCount    int
	deviceMemoryScaling float64
	deviceMemoryFactor  int
	deviceCoresScaling  float64
	excludeDevices      sets.Int
}

func (nc NodeConfig) NodeName() string {
	return nc.nodeName
}

func (nc NodeConfig) CGroupDriver() string {
	return nc.cgroupDriver
}

func (nc NodeConfig) DevicePluginPath() string {
	return nc.devicePluginPath
}

func (nc NodeConfig) DeviceSplitCount() int {
	return nc.deviceSplitCount
}

func (nc NodeConfig) DeviceMemoryScaling() float64 {
	return nc.deviceMemoryScaling
}

func (nc NodeConfig) DeviceMemoryFactor() int {
	return nc.deviceMemoryFactor
}

func (nc NodeConfig) DeviceCoresScaling() float64 {
	return nc.deviceCoresScaling
}

func (nc NodeConfig) ExcludeDevices() sets.Int {
	return nc.excludeDevices
}

func (nc NodeConfig) String() string {
	return fmt.Sprintf("nodeName: %s, devicePluginPath: %s, deviceSplitCount: %d, deviceMemoryScaling: %2.f, deviceMemoryFactor: %d, deviceCoresScaling: %2.f, excludeDevices: %+v",
		nc.nodeName, nc.devicePluginPath, nc.deviceSplitCount, nc.deviceMemoryScaling, nc.deviceMemoryFactor, nc.deviceCoresScaling, nc.excludeDevices.List())
}

func checkNodeConfig(nodeConfig *NodeConfig) error {
	if nodeConfig == nil {
		return fmt.Errorf("NodeConfig is empty")
	}
	if len(nodeConfig.devicePluginPath) == 0 {
		return fmt.Errorf("NodeConfig.DevicePluginPath is empty")
	}
	if nodeConfig.deviceSplitCount < 0 {
		return fmt.Errorf("NodeConfig.DeviceSplitCount must be a positive integer greater than or equal to 0")
	}
	if nodeConfig.deviceMemoryScaling < 0 {
		return fmt.Errorf("NodeConfig.DeviceMemoryScaling must be any number greater than or equal to 0 but less than or equal to 1")
	}
	if nodeConfig.deviceMemoryFactor <= 0 {
		return fmt.Errorf("NodeConfig.DeviceMemoryFactor must be a positive integer greater than 0")
	}
	if nodeConfig.deviceCoresScaling < 0 || nodeConfig.deviceCoresScaling > 1 {
		return fmt.Errorf("NodeConfig.DeviceCoresScaling must be any number greater than or equal to 0 but less than or equal to 1")
	}
	return nil
}

func NewNodeConfig(opt options.Options) (*NodeConfig, error) {
	config := &NodeConfig{
		nodeName:            opt.NodeName,
		cgroupDriver:        opt.CGroupDriver,
		deviceSplitCount:    opt.DeviceSplitCount,
		devicePluginPath:    opt.DevicePluginPath,
		deviceMemoryScaling: opt.DeviceMemoryScaling,
		deviceMemoryFactor:  opt.DeviceMemoryFactor,
		deviceCoresScaling:  opt.DeviceCoresScaling,
		excludeDevices:      ParseExcludeDevices(opt.ExcludeDevices),
	}
	if len(opt.NodeConfigPath) > 0 {
		bytes, err := os.ReadFile(opt.NodeConfigPath)
		if err != nil {
			return nil, err
		}
		var configMap []NodeConfigMap
		if err = json.Unmarshal(bytes, &configMap); err != nil {
			return nil, err
		}
		for _, nodeConfigMap := range configMap {
			if nodeConfigMap.NodeName != config.nodeName {
				continue
			}
			klog.Infoln("Matched Node ConfigMap", nodeConfigMap)
			if nodeConfigMap.CGroupDriver != nil {
				config.cgroupDriver = *nodeConfigMap.CGroupDriver
			}
			if nodeConfigMap.DeviceSplitCount != nil {
				config.deviceSplitCount = *nodeConfigMap.DeviceSplitCount
			}
			if nodeConfigMap.DeviceMemoryFactor != nil {
				config.deviceMemoryFactor = *nodeConfigMap.DeviceMemoryFactor
			}
			if nodeConfigMap.DeviceCoresScaling != nil {
				config.deviceCoresScaling = *nodeConfigMap.DeviceCoresScaling
			}
			if nodeConfigMap.DeviceMemoryScaling != nil {
				config.deviceMemoryScaling = *nodeConfigMap.DeviceMemoryScaling
			}
			if nodeConfigMap.ExcludeDevices != nil {
				config.excludeDevices = ParseExcludeDevices(*nodeConfigMap.ExcludeDevices)
			}
			break
		}
	}
	return config, checkNodeConfig(config)
}

func ParseExcludeDevices(excludeDevices string) sets.Int {
	exDevs := sets.NewInt()
	excludeDevices = strings.TrimSpace(excludeDevices)
	if len(excludeDevices) == 0 {
		return exDevs
	}
	for _, str := range strings.Split(excludeDevices, ",") {
		split := strings.Split(strings.TrimSpace(str), "-")
		switch len(split) {
		case 1:
			atoi, err := strconv.Atoi(strings.TrimSpace(split[0]))
			if err != nil {
				klog.Errorf("Call ParseExcludeDevices failed: excludeDevices: [%s], err: %v", excludeDevices, err)
				continue
			}
			exDevs.Insert(atoi)
		case 2:
			start, err := strconv.Atoi(strings.TrimSpace(split[0]))
			if err != nil {
				klog.Errorf("Call ParseExcludeDevices failed: excludeDevices: [%s], err: %v", excludeDevices, err)
				continue
			}
			end, err := strconv.Atoi(strings.TrimSpace(split[1]))
			if err != nil {
				klog.Errorf("Call ParseExcludeDevices failed: excludeDevices: [%s], err: %v", excludeDevices, err)
				continue
			}
			for ; start <= end; start++ {
				exDevs.Insert(start)
			}
		}
	}
	return exDevs
}
