package node

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	dpoptions "github.com/coldzerofear/vgpu-manager/cmd/device-plugin/options"
	monitoroptions "github.com/coldzerofear/vgpu-manager/cmd/monitor/options"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
)

const Version = "v1"

type ConfigYaml struct {
	Version string   `yaml:"version"`
	Config  []Config `yaml:"config,omitempty"`
}

type Config struct {
	NodeName            string   `json:"nodeName" yaml:"nodeName"`
	CGroupDriver        *string  `json:"cgroupDriver,omitempty" yaml:"cgroupDriver,omitempty"`
	DeviceListStrategy  *string  `json:"deviceListStrategy,omitempty" yaml:"deviceListStrategy,omitempty"`
	DeviceSplitCount    *int     `json:"deviceSplitCount,omitempty" yaml:"deviceSplitCount,omitempty"`
	DeviceMemoryScaling *float64 `json:"deviceMemoryScaling,omitempty" yaml:"deviceMemoryScaling,omitempty"`
	DeviceMemoryFactor  *int     `json:"deviceMemoryFactor,omitempty" yaml:"deviceMemoryFactor,omitempty"`
	DeviceCoresScaling  *float64 `json:"deviceCoresScaling,omitempty" yaml:"deviceCoresScaling,omitempty"`
	ExcludeDevices      *string  `json:"excludeDevices,omitempty" yaml:"excludeDevices,omitempty"`
	GDSEnabled          *bool    `json:"gdsEnabled,omitempty" yaml:"gdsEnabled,omitempty"`
	MOFEDEnabled        *bool    `json:"mofedEnabled,omitempty" yaml:"mofedEnabled,omitempty"`
	MigStrategy         *string  `json:"migStrategy,omitempty" yaml:"migStrategy,omitempty"`
	OpenKernelModules   *bool    `json:"openKernelModules,omitempty" yaml:"openKernelModules,omitempty"`
}

type NodeConfig struct {
	nodeName            string
	nodeConfigPath      string
	devicePluginPath    *string
	cgroupDriver        *string
	deviceListStrategy  *string
	deviceSplitCount    *int
	deviceMemoryScaling *float64
	deviceMemoryFactor  *int
	deviceCoresScaling  *float64
	excludeDevices      sets.Int
	gdsEnabled          *bool
	mofedEnabled        *bool
	migStrategy         *string
	openKernelModules   *bool
	checkFields         bool
}

func (nc NodeConfig) NodeName() string {
	return nc.nodeName
}

func (nc NodeConfig) CGroupDriver() string {
	if nc.cgroupDriver == nil {
		return ""
	}
	return *nc.cgroupDriver
}

func (nc NodeConfig) DeviceListStrategy() string {
	if nc.deviceListStrategy == nil {
		return ""
	}
	return *nc.deviceListStrategy
}

func (nc NodeConfig) DevicePluginPath() string {
	if nc.devicePluginPath == nil {
		return ""
	}
	return *nc.devicePluginPath
}

func (nc NodeConfig) DeviceSplitCount() int {
	if nc.deviceSplitCount == nil {
		return 0
	}
	return *nc.deviceSplitCount
}

func (nc NodeConfig) DeviceMemoryScaling() float64 {
	if nc.deviceMemoryScaling == nil {
		return 0
	}
	return *nc.deviceMemoryScaling
}

func (nc NodeConfig) DeviceMemoryFactor() int {
	if nc.deviceMemoryFactor == nil {
		return 0
	}
	return *nc.deviceMemoryFactor
}

func (nc NodeConfig) DeviceCoresScaling() float64 {
	if nc.deviceCoresScaling == nil {
		return 0
	}
	return *nc.deviceCoresScaling
}

func (nc NodeConfig) ExcludeDevices() sets.Int {
	return nc.excludeDevices
}

func (nc NodeConfig) GDSEnabled() bool {
	if nc.gdsEnabled == nil {
		return false
	}
	return *nc.gdsEnabled
}

func (nc NodeConfig) MOFEDEnabled() bool {
	if nc.mofedEnabled == nil {
		return false
	}
	return *nc.mofedEnabled
}

func (nc NodeConfig) OpenKernelModules() bool {
	if nc.openKernelModules == nil {
		return false
	}
	return *nc.openKernelModules
}

func (nc NodeConfig) MigStrategy() string {
	if nc.migStrategy == nil {
		return ""
	}
	return *nc.migStrategy
}

func (nc NodeConfig) YamlString() string {
	var result strings.Builder
	result.WriteString("version: v1\n")
	result.WriteString("config:\n")
	result.WriteString(fmt.Sprintf(" - nodeName: %s\n", nc.nodeName))
	if nc.cgroupDriver != nil {
		result.WriteString(fmt.Sprintf("   cgroupDriver: %s\n", *nc.cgroupDriver))
	}
	if nc.deviceListStrategy != nil {
		result.WriteString(fmt.Sprintf("   deviceListStrategy: %s\n", *nc.deviceListStrategy))
	}
	if nc.deviceSplitCount != nil {
		result.WriteString(fmt.Sprintf("   deviceSplitCount: %d\n", *nc.deviceSplitCount))
	}
	if nc.deviceMemoryScaling != nil {
		result.WriteString(fmt.Sprintf("   deviceMemoryScaling: %.2f\n", *nc.deviceMemoryScaling))
	}
	if nc.deviceMemoryFactor != nil {
		result.WriteString(fmt.Sprintf("   deviceMemoryFactor: %d\n", *nc.deviceMemoryFactor))
	}
	if nc.deviceCoresScaling != nil {
		result.WriteString(fmt.Sprintf("   deviceCoresScaling: %.2f\n", *nc.deviceCoresScaling))
	}
	if len(nc.excludeDevices) > 0 {
		result.WriteString(fmt.Sprintf("   excludeDevices: %+v\n", nc.excludeDevices.List()))
	}
	if nc.gdsEnabled != nil {
		result.WriteString(fmt.Sprintf("   gdsEnabled: %t\n", *nc.gdsEnabled))
	}
	if nc.mofedEnabled != nil {
		result.WriteString(fmt.Sprintf("   mofedEnabled: %t\n", *nc.mofedEnabled))
	}
	if nc.migStrategy != nil {
		result.WriteString(fmt.Sprintf("   migStrategy: %s\n", *nc.migStrategy))
	}
	if nc.openKernelModules != nil {
		result.WriteString(fmt.Sprintf("   openKernelModules: %t\n", *nc.openKernelModules))
	}
	return result.String()
}

func (nc NodeConfig) JsonString() string {
	var result strings.Builder
	result.WriteString("{\n")
	result.WriteString(fmt.Sprintf("  \"nodeName\": \"%s\"\n", nc.nodeName))
	if nc.cgroupDriver != nil {
		result.WriteString(fmt.Sprintf("  \"cgroupDriver\": \"%s\"\n", *nc.cgroupDriver))
	}
	if nc.deviceListStrategy != nil {
		result.WriteString(fmt.Sprintf("  \"deviceListStrategy\": \"%s\"\n", *nc.deviceListStrategy))
	}
	if nc.deviceSplitCount != nil {
		result.WriteString(fmt.Sprintf("  \"deviceSplitCount\": %d\n", *nc.deviceSplitCount))
	}
	if nc.deviceMemoryScaling != nil {
		result.WriteString(fmt.Sprintf("  \"deviceMemoryScaling\": %.2f\n", *nc.deviceMemoryScaling))
	}
	if nc.deviceMemoryFactor != nil {
		result.WriteString(fmt.Sprintf("  \"deviceMemoryFactor\": %d\n", *nc.deviceMemoryFactor))
	}
	if nc.deviceCoresScaling != nil {
		result.WriteString(fmt.Sprintf("  \"deviceCoresScaling\": %.2f\n", *nc.deviceCoresScaling))
	}
	if len(nc.excludeDevices) > 0 {
		result.WriteString(fmt.Sprintf("  \"excludeDevices\": %+v\n", nc.excludeDevices.List()))
	}
	if nc.gdsEnabled != nil {
		result.WriteString(fmt.Sprintf("  \"gdsEnabled\": %t\n", *nc.gdsEnabled))
	}
	if nc.mofedEnabled != nil {
		result.WriteString(fmt.Sprintf("  \"mofedEnabled\": %t\n", *nc.mofedEnabled))
	}
	if nc.migStrategy != nil {
		result.WriteString(fmt.Sprintf("  \"migStrategy\": \"%s\"\n", *nc.migStrategy))
	}
	if nc.openKernelModules != nil {
		result.WriteString(fmt.Sprintf("  \"openKernelModules\": %t\n", *nc.openKernelModules))
	}
	result.WriteString("}")
	return result.String()
}

func (nc NodeConfig) String() string {
	configPath := strings.TrimSpace(nc.nodeConfigPath)
	if configPath == "" {
		return nc.YamlString()
	}
	fileName := strings.ToLower(filepath.Base(configPath))
	switch {
	case strings.HasSuffix(fileName, ".json"):
		return nc.JsonString()
	case strings.HasSuffix(fileName, ".yaml"), strings.HasSuffix(fileName, ".yml"):
		return nc.YamlString()
	default:
		return nc.YamlString()
	}
}

func (nc NodeConfig) checkNodeConfig() error {
	if !nc.checkFields {
		return nil
	}

	switch nc.DeviceListStrategy() {
	case util.DeviceListStrategyEnvvar:
	case util.DeviceListStrategyVolumeMounts:
	default:
		return fmt.Errorf("unknown deviceListStrategy value: %s", nc.DeviceListStrategy())
	}
	switch nc.MigStrategy() {
	case util.MigStrategyNone:
	case util.MigStrategySingle:
	case util.MigStrategyMixed:
	default:
		return fmt.Errorf("unknown migStrategy value: %s", nc.MigStrategy())
	}
	if nc.DevicePluginPath() == "" {
		return fmt.Errorf("devicePluginPath is empty")
	}
	if nc.DeviceSplitCount() < 0 {
		return fmt.Errorf("deviceSplitCount must be a positive integer greater than or equal to 0")
	}
	if nc.DeviceMemoryScaling() < 0 {
		return fmt.Errorf("deviceMemoryScaling must be any number greater than or equal to 0")
	}
	if nc.DeviceMemoryFactor() <= 0 {
		return fmt.Errorf("deviceMemoryFactor must be a positive integer greater than 0")
	}
	if nc.DeviceCoresScaling() < 0 || nc.DeviceCoresScaling() > 1 {
		return fmt.Errorf("deviceCoresScaling must be any number greater than or equal to 0 but less than or equal to 1")
	}
	return nil
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

func parseConfig(configFile string) ([]Config, error) {
	configFile = strings.TrimSpace(configFile)
	configBytes, err := os.ReadFile(configFile)
	if err != nil {
		return nil, fmt.Errorf("error read config file: %v", err)
	}
	var config ConfigYaml
	fileName := strings.ToLower(filepath.Base(configFile))
	switch {
	case strings.HasSuffix(fileName, ".yaml"), strings.HasSuffix(fileName, ".yml"):
		if err = yaml.Unmarshal(configBytes, &config); err != nil {
			return nil, fmt.Errorf("yaml unmarshal error: %v", err)
		}
		if config.Version == "" {
			config.Version = Version
		}
	case strings.HasSuffix(fileName, ".json"):
		var configs []Config
		if err = json.Unmarshal(configBytes, &configs); err != nil {
			return nil, fmt.Errorf("json unmarshal error: %v", err)
		}
		config = ConfigYaml{
			Version: Version,
			Config:  configs,
		}
	default:
		return nil, fmt.Errorf("unsupported config file format")
	}
	if config.Version != Version {
		return nil, fmt.Errorf("unknown config version: %v", config.Version)
	}
	return config.Config, nil
}

type Option func(*NodeConfig)

func WithDevicePluginOptions(opt dpoptions.Options) Option {
	return func(nodeConfig *NodeConfig) {
		nodeConfig.nodeName = opt.NodeName
		nodeConfig.nodeConfigPath = opt.NodeConfigPath
		nodeConfig.cgroupDriver = ptr.To[string](opt.CGroupDriver)
		nodeConfig.migStrategy = ptr.To[string](opt.MigStrategy)
		nodeConfig.deviceListStrategy = ptr.To[string](opt.DeviceListStrategy)
		nodeConfig.deviceSplitCount = ptr.To[int](opt.DeviceSplitCount)
		nodeConfig.devicePluginPath = ptr.To[string](opt.DevicePluginPath)
		nodeConfig.deviceMemoryScaling = ptr.To[float64](opt.DeviceMemoryScaling)
		nodeConfig.deviceMemoryFactor = ptr.To[int](opt.DeviceMemoryFactor)
		nodeConfig.deviceCoresScaling = ptr.To[float64](opt.DeviceCoresScaling)
		nodeConfig.excludeDevices = ParseExcludeDevices(opt.ExcludeDevices)
		nodeConfig.gdsEnabled = ptr.To[bool](opt.GDSEnabled)
		nodeConfig.mofedEnabled = ptr.To[bool](opt.MOFEDEnabled)
		nodeConfig.openKernelModules = ptr.To[bool](opt.OpenKernelModules)
		nodeConfig.checkFields = true
	}
}

func WithMonitorOptions(opt monitoroptions.Options) func(*NodeConfig) {
	return func(nodeConfig *NodeConfig) {
		nodeConfig.nodeName = opt.NodeName
		nodeConfig.nodeConfigPath = opt.NodeConfigPath
		nodeConfig.cgroupDriver = ptr.To[string](opt.CGroupDriver)
		nodeConfig.excludeDevices = sets.NewInt()
		nodeConfig.checkFields = false
	}
}

func NewNodeConfig(option Option) (*NodeConfig, error) {
	if option == nil {
		return nil, fmt.Errorf("option is empty")
	}
	nodeConfig := &NodeConfig{}
	option(nodeConfig)
	if len(nodeConfig.nodeConfigPath) > 0 {
		configs, err := parseConfig(nodeConfig.nodeConfigPath)
		if err != nil {
			klog.Errorf("parse node config file failed: %v", err)
			return nil, err
		}
		for _, config := range configs {
			if !matchNodeName(config.NodeName, nodeConfig.nodeName) {
				continue
			}
			klog.InfoS("Matched node config", "nodeConfig.nodeName",
				config.NodeName, "current.nodeName", nodeConfig.nodeName)

			if config.CGroupDriver != nil {
				nodeConfig.cgroupDriver = config.CGroupDriver
			}
			if config.DeviceListStrategy != nil {
				nodeConfig.deviceListStrategy = config.DeviceListStrategy
			}
			if config.DeviceSplitCount != nil {
				nodeConfig.deviceSplitCount = config.DeviceSplitCount
			}
			if config.DeviceMemoryFactor != nil {
				nodeConfig.deviceMemoryFactor = config.DeviceMemoryFactor
			}
			if config.DeviceCoresScaling != nil {
				nodeConfig.deviceCoresScaling = config.DeviceCoresScaling
			}
			if config.DeviceMemoryScaling != nil {
				nodeConfig.deviceMemoryScaling = config.DeviceMemoryScaling
			}
			if config.ExcludeDevices != nil {
				nodeConfig.excludeDevices = ParseExcludeDevices(*config.ExcludeDevices)
			}
			if config.GDSEnabled != nil {
				nodeConfig.gdsEnabled = config.GDSEnabled
			}
			if config.MOFEDEnabled != nil {
				nodeConfig.mofedEnabled = config.MOFEDEnabled
			}
			if config.MigStrategy != nil {
				nodeConfig.migStrategy = config.MigStrategy
			}
			if config.OpenKernelModules != nil {
				nodeConfig.openKernelModules = config.OpenKernelModules
			}
			break
		}
	}
	return nodeConfig, nodeConfig.checkNodeConfig()
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
