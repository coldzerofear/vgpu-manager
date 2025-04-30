package node

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	dpoptions "github.com/coldzerofear/vgpu-manager/cmd/device-plugin/options"
	monitoroptions "github.com/coldzerofear/vgpu-manager/cmd/monitor/options"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
)

type NodeConfigMap struct {
	NodeName            string   `json:"nodeName"`
	CGroupDriver        *string  `json:"cgroupDriver,omitempty"`
	DeviceListStrategy  *string  `json:"deviceListStrategy,omitempty"`
	DeviceSplitCount    *int     `json:"deviceSplitCount,omitempty"`
	DeviceMemoryScaling *float64 `json:"deviceMemoryScaling,omitempty"`
	DeviceMemoryFactor  *int     `json:"deviceMemoryFactor,omitempty"`
	DeviceCoresScaling  *float64 `json:"deviceCoresScaling,omitempty"`
	ExcludeDevices      *string  `json:"excludeDevices,omitempty"`
	GDSEnabled          *bool    `json:"gdsEnabled,omitempty"`
	MOFEDEnabled        *bool    `json:"mofedEnabled,omitempty"`
	MigStrategy         *string  `json:"migStrategy,omitempty"`
	OpenKernelModules   *bool    `json:"openKernelModules,omitempty"`
}

type NodeConfig struct {
	nodeName            string
	nodeConfigPath      string
	cgroupDriver        string
	deviceListStrategy  string
	devicePluginPath    string
	deviceSplitCount    int
	deviceMemoryScaling float64
	deviceMemoryFactor  int
	deviceCoresScaling  float64
	excludeDevices      sets.Int
	gdsEnabled          bool
	mofedEnabled        bool
	migStrategy         string
	openKernelModules   bool
	checkFields         bool
}

func (nc NodeConfig) NodeName() string {
	return nc.nodeName
}

func (nc NodeConfig) CGroupDriver() string {
	return nc.cgroupDriver
}

func (nc NodeConfig) DeviceListStrategy() string {
	return nc.deviceListStrategy
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

func (nc NodeConfig) GDSEnabled() bool {
	return nc.gdsEnabled
}

func (nc NodeConfig) MOFEDEnabled() bool {
	return nc.mofedEnabled
}

func (nc NodeConfig) OpenKernelModules() bool {
	return nc.openKernelModules
}

func (nc NodeConfig) MigStrategy() string {
	return nc.migStrategy
}

func (nc NodeConfig) String() string {
	format := `{
  "nodeName": "%s", 
  "devicePluginPath": "%s", 
  "cgroupDriver": "%s", 
  "migStrategy": "%s",
  "deviceListStrategy": "%s",
  "deviceSplitCount": %d, 
  "deviceCoresScaling": %.2f, 
  "deviceMemoryScaling": %.2f,
  "deviceMemoryFactor": %d,
  "excludeDevices": %+v, 
  "gdsEnabled": %t, 
  "mofedEnabled": %t,
  "openKernelModules": %t
}`
	return fmt.Sprintf(format, nc.nodeName, nc.devicePluginPath, nc.cgroupDriver, nc.migStrategy,
		nc.deviceListStrategy, nc.deviceSplitCount, nc.deviceCoresScaling, nc.deviceMemoryScaling,
		nc.deviceMemoryFactor, nc.excludeDevices.List(), nc.gdsEnabled, nc.mofedEnabled, nc.openKernelModules)
}

func checkNodeConfig(nodeConfig *NodeConfig) error {
	if nodeConfig == nil {
		return fmt.Errorf("NodeConfig is empty")
	}
	switch nodeConfig.deviceListStrategy {
	case util.DeviceListStrategyEnvvar:
	case util.DeviceListStrategyVolumeMounts:
	default:
		return fmt.Errorf("unknown deviceListStrategy value: %s", nodeConfig.deviceListStrategy)
	}
	switch nodeConfig.migStrategy {
	case util.MigStrategyNone:
	case util.MigStrategySingle:
	case util.MigStrategyMixed:
	default:
		return fmt.Errorf("unknown migStrategy value: %s", nodeConfig.deviceListStrategy)
	}
	if len(nodeConfig.devicePluginPath) == 0 {
		return fmt.Errorf("NodeConfig.DevicePluginPath is empty")
	}
	if nodeConfig.deviceSplitCount < 0 {
		return fmt.Errorf("NodeConfig.DeviceSplitCount must be a positive integer greater than or equal to 0")
	}
	if nodeConfig.deviceMemoryScaling < 0 {
		return fmt.Errorf("NodeConfig.DeviceMemoryScaling must be any number greater than or equal to 0")
	}
	if nodeConfig.deviceMemoryFactor <= 0 {
		return fmt.Errorf("NodeConfig.DeviceMemoryFactor must be a positive integer greater than 0")
	}
	if nodeConfig.deviceCoresScaling < 0 || nodeConfig.deviceCoresScaling > 1 {
		return fmt.Errorf("NodeConfig.DeviceCoresScaling must be any number greater than or equal to 0 but less than or equal to 1")
	}
	return nil
}

func MutationDPOptions(opt dpoptions.Options) func(*NodeConfig) {
	return func(nodeConfig *NodeConfig) {
		nodeConfig.nodeName = opt.NodeName
		nodeConfig.nodeConfigPath = opt.NodeConfigPath
		nodeConfig.cgroupDriver = opt.CGroupDriver
		nodeConfig.migStrategy = opt.MigStrategy
		nodeConfig.deviceListStrategy = opt.DeviceListStrategy
		nodeConfig.deviceSplitCount = opt.DeviceSplitCount
		nodeConfig.devicePluginPath = opt.DevicePluginPath
		nodeConfig.deviceMemoryScaling = opt.DeviceMemoryScaling
		nodeConfig.deviceMemoryFactor = opt.DeviceMemoryFactor
		nodeConfig.deviceCoresScaling = opt.DeviceCoresScaling
		nodeConfig.excludeDevices = ParseExcludeDevices(opt.ExcludeDevices)
		nodeConfig.gdsEnabled = opt.GDSEnabled
		nodeConfig.mofedEnabled = opt.MOFEDEnabled
		nodeConfig.openKernelModules = opt.OpenKernelModules
		nodeConfig.checkFields = true
	}
}

func MutationMonitorOptions(opt monitoroptions.Options) func(*NodeConfig) {
	return func(nodeConfig *NodeConfig) {
		nodeConfig.nodeName = opt.NodeName
		nodeConfig.nodeConfigPath = opt.NodeConfigPath
		nodeConfig.cgroupDriver = opt.CGroupDriver
		nodeConfig.excludeDevices = sets.NewInt()
		nodeConfig.checkFields = false
	}
}

func regexpMatch(expr, target string) bool {
	compile, err := regexp.Compile(expr)
	if err != nil {
		return false
	}
	return compile.MatchString(target)
}

func matchNodeName(cmNodeName, cuNodeName string) bool {
	cmNodeName = strings.TrimSpace(cmNodeName)
	if len(cmNodeName) == 0 {
		return false
	}
	if cmNodeName == cuNodeName {
		return true
	}
	if cmNodeName[0] != '^' && cmNodeName[len(cmNodeName)-1] != '$' {
		cmNodeName = "^" + cmNodeName + "$"
	}
	return regexpMatch(cmNodeName, cuNodeName)
}

func NewNodeConfig(mutations ...func(*NodeConfig)) (*NodeConfig, error) {
	config := &NodeConfig{}
	for _, mutation := range mutations {
		mutation(config)
	}
	if len(config.nodeConfigPath) > 0 {
		configBytes, err := os.ReadFile(config.nodeConfigPath)
		if err != nil {
			return nil, err
		}
		var nodeConfigs []NodeConfigMap
		if err = json.Unmarshal(configBytes, &nodeConfigs); err != nil {
			return nil, err
		}
		for _, nodeConfig := range nodeConfigs {
			if !matchNodeName(nodeConfig.NodeName, config.nodeName) {
				continue
			}
			klog.InfoS("Matched node config", "nodeConfig.nodeName",
				nodeConfig.NodeName, "current.nodeName", config.nodeName)
			if nodeConfig.CGroupDriver != nil {
				config.cgroupDriver = *nodeConfig.CGroupDriver
			}
			if nodeConfig.DeviceListStrategy != nil {
				config.deviceListStrategy = *nodeConfig.DeviceListStrategy
			}
			if nodeConfig.DeviceSplitCount != nil {
				config.deviceSplitCount = *nodeConfig.DeviceSplitCount
			}
			if nodeConfig.DeviceMemoryFactor != nil {
				config.deviceMemoryFactor = *nodeConfig.DeviceMemoryFactor
			}
			if nodeConfig.DeviceCoresScaling != nil {
				config.deviceCoresScaling = *nodeConfig.DeviceCoresScaling
			}
			if nodeConfig.DeviceMemoryScaling != nil {
				config.deviceMemoryScaling = *nodeConfig.DeviceMemoryScaling
			}
			if nodeConfig.ExcludeDevices != nil {
				config.excludeDevices = ParseExcludeDevices(*nodeConfig.ExcludeDevices)
			}
			if nodeConfig.GDSEnabled != nil {
				config.gdsEnabled = *nodeConfig.GDSEnabled
			}
			if nodeConfig.MOFEDEnabled != nil {
				config.mofedEnabled = *nodeConfig.MOFEDEnabled
			}
			if nodeConfig.MigStrategy != nil {
				config.migStrategy = *nodeConfig.MigStrategy
			}
			if nodeConfig.OpenKernelModules != nil {
				config.openKernelModules = *nodeConfig.OpenKernelModules
			}
			break
		}
	}
	var err error
	if config.checkFields {
		err = checkNodeConfig(config)
	}
	return config, err
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
