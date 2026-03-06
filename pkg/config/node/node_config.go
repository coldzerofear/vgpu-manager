package node

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/coldzerofear/vgpu-manager/pkg/device/imex"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"gopkg.in/yaml.v3"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
)

const Version = "v1"

type ConfigTemplate struct {
	Version string       `json:"version"           yaml:"version"`
	Configs []ConfigSpec `json:"configs,omitempty" yaml:"configs,omitempty"`
}

type ConfigSpec struct {
	NodeName            string     `json:"nodeName"                      yaml:"nodeName"`
	CGroupDriver        *string    `json:"cgroupDriver,omitempty"        yaml:"cgroupDriver,omitempty"`
	DeviceListStrategy  *string    `json:"deviceListStrategy,omitempty"  yaml:"deviceListStrategy,omitempty"`
	DeviceSplitCount    *int       `json:"deviceSplitCount,omitempty"    yaml:"deviceSplitCount,omitempty"`
	DeviceMemoryScaling *float64   `json:"deviceMemoryScaling,omitempty" yaml:"deviceMemoryScaling,omitempty"`
	DeviceMemoryFactor  *int       `json:"deviceMemoryFactor,omitempty"  yaml:"deviceMemoryFactor,omitempty"`
	DeviceCoresScaling  *float64   `json:"deviceCoresScaling,omitempty"  yaml:"deviceCoresScaling,omitempty"`
	ExcludeDevices      *IDStore   `json:"excludeDevices,omitempty"      yaml:"excludeDevices,omitempty"`
	GDSEnabled          *bool      `json:"gdsEnabled,omitempty"          yaml:"gdsEnabled,omitempty"`
	MOFEDEnabled        *bool      `json:"mofedEnabled,omitempty"        yaml:"mofedEnabled,omitempty"`
	GDRCopyEnabled      *bool      `json:"gdrcopyEnabled"                yaml:"gdrcopyEnabled"`
	MigStrategy         *string    `json:"migStrategy,omitempty"         yaml:"migStrategy,omitempty"`
	OpenKernelModules   *bool      `json:"openKernelModules,omitempty"   yaml:"openKernelModules,omitempty"`
	Imex                *imex.Imex `json:"imex,omitempty"                yaml:"imex,omitempty"`
}

type NodeConfigSpec struct {
	ConfigSpec       `json:",inline" yaml:",inline"`
	devicePluginPath *string
	nodeConfigPath   string
	checkFields      bool
}

func (nc NodeConfigSpec) GetNodeName() string {
	return nc.NodeName
}

func (nc NodeConfigSpec) GetCGroupDriver() string {
	if nc.CGroupDriver == nil {
		return ""
	}
	return *nc.CGroupDriver
}

func (nc NodeConfigSpec) GetDeviceListStrategy() string {
	if nc.DeviceListStrategy == nil {
		return ""
	}
	return *nc.DeviceListStrategy
}

func (nc NodeConfigSpec) GetDevicePluginPath() string {
	if nc.devicePluginPath == nil {
		return ""
	}
	return *nc.devicePluginPath
}

func (nc NodeConfigSpec) GetDeviceSplitCount() int {
	if nc.DeviceSplitCount == nil {
		return 0
	}
	return *nc.DeviceSplitCount
}

func (nc NodeConfigSpec) GetDeviceMemoryScaling() float64 {
	if nc.DeviceMemoryScaling == nil {
		return 0
	}
	return *nc.DeviceMemoryScaling
}

func (nc NodeConfigSpec) GetDeviceMemoryFactor() int {
	if nc.DeviceMemoryFactor == nil {
		return 0
	}
	return *nc.DeviceMemoryFactor
}

func (nc NodeConfigSpec) GetDeviceCoresScaling() float64 {
	if nc.DeviceCoresScaling == nil {
		return 0
	}
	return *nc.DeviceCoresScaling
}

func (nc NodeConfigSpec) GetExcludeDevices() IDStore {
	if nc.ExcludeDevices == nil {
		return NewIDStore()
	}
	return *nc.ExcludeDevices
}

func (nc NodeConfigSpec) GetGDSEnabled() bool {
	if nc.GDSEnabled == nil {
		return false
	}
	return *nc.GDSEnabled
}

func (nc NodeConfigSpec) GetMOFEDEnabled() bool {
	if nc.MOFEDEnabled == nil {
		return false
	}
	return *nc.MOFEDEnabled
}

func (nc NodeConfigSpec) GetGDRCopyEnabled() bool {
	if nc.GDRCopyEnabled == nil {
		return false
	}
	return *nc.GDRCopyEnabled
}

func (nc NodeConfigSpec) GetOpenKernelModules() bool {
	if nc.OpenKernelModules == nil {
		return false
	}
	return *nc.OpenKernelModules
}

func (nc NodeConfigSpec) GetMigStrategy() string {
	if nc.MigStrategy == nil {
		return ""
	}
	return *nc.MigStrategy
}

func (nc NodeConfigSpec) GetIMEX() imex.Imex {
	if nc.Imex == nil {
		return imex.Imex{}
	}
	return *nc.Imex
}

func (nc NodeConfigSpec) YamlString() string {
	ct := ConfigTemplate{
		Version: Version,
		Configs: []ConfigSpec{
			nc.ConfigSpec,
		},
	}
	marshal, err := yaml.Marshal(ct)
	if err != nil {
		klog.Warningf("node config yaml.Marshal failed: %v", err)
		return ""
	}
	return string(marshal)
}

func (nc NodeConfigSpec) JsonString() string {
	marshal, err := json.MarshalIndent(nc, "", "  ")
	if err != nil {
		klog.Warningf("node config json.Marshal failed: %v", err)
		return ""
	}
	return string(marshal)
}

func (nc NodeConfigSpec) String() string {
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

func (nc NodeConfigSpec) checkNodeConfig() (errs []error) {
	switch nc.GetDeviceListStrategy() {
	case util.DeviceListStrategyEnvvar:
	case util.DeviceListStrategyVolumeMounts:
	default:
		errs = append(errs, fmt.Errorf("unknown deviceListStrategy value: \"%s\"", nc.GetDeviceListStrategy()))
	}
	switch nc.GetMigStrategy() {
	case util.MigStrategyNone:
	case util.MigStrategySingle:
	case util.MigStrategyMixed:
	default:
		errs = append(errs, fmt.Errorf("unknown migStrategy value: \"%s\"", nc.GetMigStrategy()))
	}
	if nc.GetDevicePluginPath() == "" {
		errs = append(errs, fmt.Errorf("devicePluginPath cannot be empty"))
	}
	if nc.GetDeviceSplitCount() < 0 {
		errs = append(errs, fmt.Errorf("deviceSplitCount must be a positive integer greater than or equal to 0"))
	}
	if nc.GetDeviceMemoryScaling() < 0 {
		errs = append(errs, fmt.Errorf("deviceMemoryScaling must be any number greater than or equal to 0"))
	}
	if nc.GetDeviceMemoryFactor() <= 0 {
		errs = append(errs, fmt.Errorf("deviceMemoryFactor must be a positive integer greater than 0"))
	}
	if nc.GetDeviceCoresScaling() < 0 {
		errs = append(errs, fmt.Errorf("deviceCoresScaling must be any number greater than or equal to 0"))
	}
	if err := imex.AssertChannelIDsValid(nc.GetIMEX().ChannelIDs); err != nil {
		errs = append(errs, fmt.Errorf("invalid IMEX channel IDs: %w", err))
	}
	return errs
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

func parseConfigTemplate(configFile string) (*ConfigTemplate, error) {
	configFile = strings.TrimSpace(configFile)
	configBytes, err := os.ReadFile(configFile)
	if err != nil {
		return nil, fmt.Errorf("error read config file: %v", err)
	}
	var configTemp ConfigTemplate
	fileName := strings.ToLower(filepath.Base(configFile))
	switch {
	case strings.HasSuffix(fileName, ".yaml"), strings.HasSuffix(fileName, ".yml"):
		if err = yaml.Unmarshal(configBytes, &configTemp); err != nil {
			return nil, fmt.Errorf("yaml unmarshal error: %v", err)
		}
		if configTemp.Version == "" {
			configTemp.Version = Version
		}
	case strings.HasSuffix(fileName, ".json"):
		var configs []ConfigSpec
		if err = json.Unmarshal(configBytes, &configs); err != nil {
			return nil, fmt.Errorf("json unmarshal error: %v", err)
		}
		configTemp = ConfigTemplate{
			Version: Version,
			Configs: configs,
		}
	default:
		return nil, fmt.Errorf("unsupported config file format: %s", fileName)
	}
	if configTemp.Version != Version {
		return nil, fmt.Errorf("unknown config version: %v", configTemp.Version)
	}
	return &configTemp, nil
}

func loadConfigSpec(nodeConfig *NodeConfigSpec) error {
	configTemp, err := parseConfigTemplate(nodeConfig.nodeConfigPath)
	if err != nil {
		klog.Errorf("parse node config file failed: %v", err)
		return err
	}
	for _, config := range configTemp.Configs {
		if !matchNodeName(config.NodeName, nodeConfig.NodeName) {
			continue
		}
		klog.InfoS("Matched node config", "nodeConfig.nodeName",
			config.NodeName, "current.nodeName", nodeConfig.NodeName)
		if config.CGroupDriver != nil {
			nodeConfig.CGroupDriver = config.CGroupDriver
		}
		if config.DeviceListStrategy != nil {
			nodeConfig.DeviceListStrategy = config.DeviceListStrategy
		}
		if config.DeviceSplitCount != nil {
			nodeConfig.DeviceSplitCount = config.DeviceSplitCount
		}
		if config.DeviceMemoryFactor != nil {
			nodeConfig.DeviceMemoryFactor = config.DeviceMemoryFactor
		}
		if config.DeviceCoresScaling != nil {
			nodeConfig.DeviceCoresScaling = config.DeviceCoresScaling
		}
		if config.DeviceMemoryScaling != nil {
			nodeConfig.DeviceMemoryScaling = config.DeviceMemoryScaling
		}
		if config.ExcludeDevices != nil {
			nodeConfig.ExcludeDevices = config.ExcludeDevices
		}
		if config.GDSEnabled != nil {
			nodeConfig.GDSEnabled = config.GDSEnabled
		}
		if config.MOFEDEnabled != nil {
			nodeConfig.MOFEDEnabled = config.MOFEDEnabled
		}
		if config.MigStrategy != nil {
			nodeConfig.MigStrategy = config.MigStrategy
		}
		if config.OpenKernelModules != nil {
			nodeConfig.OpenKernelModules = config.OpenKernelModules
		}
		if config.Imex != nil {
			nodeConfig.Imex = config.Imex
		}
		break
	}
	return nil
}

type Option func(*NodeConfigSpec)

func WithIMEXOption(imexChannelIDs []int, imexRequired bool) Option {
	return func(spec *NodeConfigSpec) {
		if len(imexChannelIDs) > 0 {
			spec.Imex = &imex.Imex{
				ChannelIDs: imexChannelIDs,
				Required:   imexRequired,
			}
		}
	}
}

func WithGDSEnabledOption(gdsEnabled bool) Option {
	return func(spec *NodeConfigSpec) {
		spec.GDSEnabled = ptr.To[bool](gdsEnabled)
	}
}

func WithMOFEDEnabledOption(mofedEnabled bool) Option {
	return func(spec *NodeConfigSpec) {
		spec.MOFEDEnabled = ptr.To[bool](mofedEnabled)
	}
}

func WithGDRCopyEnabledOption(gdrcopyEnabled bool) Option {
	return func(spec *NodeConfigSpec) {
		spec.GDRCopyEnabled = ptr.To[bool](gdrcopyEnabled)
	}
}

func WithOpenKernelModulesOption(openKernelModules bool) Option {
	return func(spec *NodeConfigSpec) {
		spec.OpenKernelModules = ptr.To[bool](openKernelModules)
	}
}

func WithExcludeDevicesOption(excludeDevices string) Option {
	return func(spec *NodeConfigSpec) {
		spec.ExcludeDevices = ptr.To[IDStore](parseDeviceIDs(excludeDevices))
	}
}

func WithDeviceCoresScalingOption(deviceCoresScaling float64) Option {
	return func(spec *NodeConfigSpec) {
		spec.DeviceCoresScaling = ptr.To[float64](deviceCoresScaling)
	}
}

func WithDeviceMemoryFactorOption(deviceMemoryFactor int) Option {
	return func(spec *NodeConfigSpec) {
		spec.DeviceMemoryFactor = ptr.To[int](deviceMemoryFactor)
	}
}

func WithDeviceMemoryScalingOption(deviceMemoryScaling float64) Option {
	return func(spec *NodeConfigSpec) {
		spec.DeviceMemoryScaling = ptr.To[float64](deviceMemoryScaling)
	}
}

func WithDevicePluginPathOption(devicePluginPath string) Option {
	return func(spec *NodeConfigSpec) {
		spec.devicePluginPath = ptr.To[string](devicePluginPath)
	}
}

func WithDeviceSplitCountOption(deviceSplitCount int) Option {
	return func(spec *NodeConfigSpec) {
		spec.DeviceSplitCount = ptr.To[int](deviceSplitCount)
	}
}

func WithDeviceListStrategyOption(deviceListStrategy string) Option {
	return func(spec *NodeConfigSpec) {
		spec.DeviceListStrategy = ptr.To[string](deviceListStrategy)
	}
}

func WithMigStrategyOption(migStrategy string) Option {
	return func(spec *NodeConfigSpec) {
		spec.MigStrategy = ptr.To[string](migStrategy)
	}
}

func WithCGroupDriverOption(cgroupDriver string) Option {
	return func(spec *NodeConfigSpec) {
		spec.CGroupDriver = ptr.To[string](cgroupDriver)
	}
}

func WithNodeNameOption(nodeName string) Option {
	return func(spec *NodeConfigSpec) {
		spec.NodeName = nodeName
	}
}

func WithConfigPathOption(configPath string) Option {
	return func(spec *NodeConfigSpec) {
		spec.nodeConfigPath = configPath
	}
}

func WithCheckFieldsOption(checkFields bool) Option {
	return func(spec *NodeConfigSpec) {
		spec.checkFields = checkFields
	}
}

func NewNodeConfig(options ...Option) (config *NodeConfigSpec, err error) {
	if len(options) == 0 {
		return nil, fmt.Errorf("node config options cannot is empty")
	}
	config = &NodeConfigSpec{}
	for _, option := range options {
		option(config)
	}

	if len(config.nodeConfigPath) > 0 {
		if err = loadConfigSpec(config); err != nil {
			return nil, err
		}
	}
	if config.checkFields {
		errs := config.checkNodeConfig()
		err = MergeError(errs)
	}
	return config, err
}

func MergeError(errs []error) error {
	errMsgs := make([]string, len(errs))
	for i, err := range errs {
		errMsgs[i] = err.Error()
	}
	if len(errMsgs) > 0 {
		return fmt.Errorf("%s", strings.Join(errMsgs, "; "))
	}
	return nil
}
