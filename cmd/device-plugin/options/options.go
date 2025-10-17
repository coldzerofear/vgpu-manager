package options

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/coldzerofear/vgpu-manager/pkg/util"
	pkgversion "github.com/coldzerofear/vgpu-manager/pkg/version"
	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apiserver/pkg/util/compatibility"
	"k8s.io/component-base/featuregate"
	"k8s.io/klog/v2"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

type Options struct {
	KubeConfigFile string
	MasterURL      string
	QPS            float64
	Burst          int

	Domain              string
	NodeName            string
	CGroupDriver        string
	DeviceListStrategy  string
	DeviceSplitCount    int
	DeviceMemoryScaling float64
	DeviceMemoryFactor  int
	DeviceCoresScaling  float64
	NodeConfigPath      string
	ExcludeDevices      string
	DevicePluginPath    string
	PprofBindPort       int
	GDSEnabled          bool
	MOFEDEnabled        bool
	MigStrategy         string
	ImexChannelIDs      []int
	ImexRequired        bool
	FeatureGate         featuregate.MutableFeatureGate
}

const (
	defaultQPS   = 20.0
	defaultBurst = 30

	defaultDeviceListStrategy  = util.DeviceListStrategyEnvvar
	defaultDeviceSplitCount    = 10
	defaultDeviceMemoryFactor  = 1
	defaultDeviceMemoryScaling = 1.0
	defaultDeviceCoresScaling  = 1.0
	defaultPprofBindPort       = 0
	defaultMigStrategy         = util.MigStrategyMixed

	Component = "devicePlugin"

	// CorePlugin feature gate will report the virtual cores of the node device to kubelet.
	CorePlugin featuregate.Feature = util.CorePlugin
	// MemoryPlugin feature gate will report the virtual memory of the node device to kubelet.
	MemoryPlugin featuregate.Feature = util.MemoryPlugin
	// Reschedule feature gate will attempt to reschedule Pods that meet the criteria.
	Reschedule featuregate.Feature = util.Reschedule
	// GPUTopology feature gate will report gpu topology information to node.
	GPUTopology featuregate.Feature = util.GPUTopology
	// SMWatcher feature gate will initiate an independent utilization observation thread to share the results with the vGPU Pod node, reducing driver call consumption.
	SMWatcher featuregate.Feature = util.SMWatcher
	// VMemoryNode feature gate will track the allocation of virtual memory on devices and provide more precise virtual memory limitations.
	VMemoryNode featuregate.Feature = util.VMemoryNode
	// ClientMode feature gate will vGPU container to communicate and register devices using Unix sockets and managers, providing stronger security.
	ClientMode featuregate.Feature = util.ClientMode
)

var (
	version             bool
	defaultFeatureGates = map[featuregate.Feature]featuregate.FeatureSpec{
		CorePlugin:   {Default: false, PreRelease: featuregate.Alpha},
		MemoryPlugin: {Default: false, PreRelease: featuregate.Alpha},
		Reschedule:   {Default: false, PreRelease: featuregate.Alpha},
		GPUTopology:  {Default: false, PreRelease: featuregate.Alpha},
		SMWatcher:    {Default: false, PreRelease: featuregate.Alpha},
		VMemoryNode:  {Default: false, PreRelease: featuregate.Alpha},
		ClientMode:   {Default: false, PreRelease: featuregate.Alpha},
	}
)

func NewOptions() *Options {
	featureGate := featuregate.NewFeatureGate()
	runtime.Must(featureGate.Add(defaultFeatureGates))
	runtime.Must(compatibility.DefaultComponentGlobalsRegistry.Register(
		Component, compatibility.DefaultBuildEffectiveVersion(), featureGate))
	gdsEnabled := os.Getenv("NVIDIA_GDS") == "enabled" || os.Getenv("NVIDIA_GDS") == "true"
	mofedEnabled := os.Getenv("NVIDIA_MOFED") == "enabled" || os.Getenv("NVIDIA_MOFED") == "true"
	var imexChannelIDs []int
	imexChannelStr := strings.TrimSpace(os.Getenv("IMEX_CHANNEL_IDS"))
	for _, split := range strings.Split(imexChannelStr, ",") {
		if atoi, err := strconv.Atoi(strings.TrimSpace(split)); err == nil {
			imexChannelIDs = append(imexChannelIDs, atoi)
		}
	}
	imexRequired := strings.EqualFold(strings.TrimSpace(os.Getenv("IMEX_REQUIRED")), "true")
	return &Options{
		QPS:                 defaultQPS,
		Burst:               defaultBurst,
		Domain:              util.GetGlobalDomain(),
		NodeName:            os.Getenv("NODE_NAME"),
		CGroupDriver:        os.Getenv("CGROUP_DRIVER"),
		DeviceListStrategy:  defaultDeviceListStrategy,
		DeviceSplitCount:    defaultDeviceSplitCount,
		DeviceCoresScaling:  defaultDeviceCoresScaling,
		DeviceMemoryScaling: defaultDeviceMemoryScaling,
		DeviceMemoryFactor:  defaultDeviceMemoryFactor,
		DevicePluginPath:    pluginapi.DevicePluginPath,
		PprofBindPort:       defaultPprofBindPort,
		GDSEnabled:          gdsEnabled,
		MOFEDEnabled:        mofedEnabled,
		MigStrategy:         defaultMigStrategy,
		FeatureGate:         featureGate,
		ImexChannelIDs:      imexChannelIDs,
		ImexRequired:        imexRequired,
	}
}

func (o *Options) InitFlags(fs *flag.FlagSet) {
	klog.InitFlags(fs)
	pflag.CommandLine.SortFlags = false
	pflag.StringVar(&o.KubeConfigFile, "kubeconfig", o.KubeConfigFile, "Path to a kubeconfig. Only required if out-of-cluster.")
	pflag.StringVar(&o.MasterURL, "master", o.MasterURL, "The address of the Kubernetes API server. Overrides any value in kubeconfig. Only required if out-of-cluster.")
	pflag.Float64Var(&o.QPS, "kube-api-qps", o.QPS, "QPS to use while talking with kubernetes apiserver.")
	pflag.IntVar(&o.Burst, "kube-api-burst", o.Burst, "Burst to use while talking with kubernetes apiserver.")
	pflag.StringVar(&o.Domain, "domain", o.Domain, "Set global domain name to replace all resource and annotation domains.")
	pflag.StringVar(&o.NodeName, "node-name", o.NodeName, "If non-empty, will use this string as identification instead of the actual node name.")
	pflag.StringVar(&o.CGroupDriver, "cgroup-driver", o.CGroupDriver, "Specify the cgroup driver used. (supported values: \"cgroupfs\" | \"system\")")
	pflag.StringVar(&o.DeviceListStrategy, "device-list-strategy", o.DeviceListStrategy, "The desired strategy for passing the device list to the underlying runtime. (supported values: \"envvar\" | \"volume-mounts\")")
	pflag.IntVar(&o.DeviceSplitCount, "device-split-count", o.DeviceSplitCount, "The maximum number of vGPU that can be split per physical GPU.")
	pflag.Float64Var(&o.DeviceCoresScaling, "device-cores-scaling", o.DeviceCoresScaling, "The ratio for NVIDIA device cores scaling.")
	pflag.Float64Var(&o.DeviceMemoryScaling, "device-memory-scaling", o.DeviceMemoryScaling, "The ratio for NVIDIA device memory scaling.")
	pflag.IntVar(&o.DeviceMemoryFactor, "device-memory-factor", o.DeviceMemoryFactor, "The default gpu memory block size is 1MB.")
	pflag.StringVar(&o.NodeConfigPath, "node-config-path", o.NodeConfigPath, "Specify the node configuration path to apply differentiated configuration to the node.")
	pflag.StringVar(&o.ExcludeDevices, "exclude-devices", "", "Specify the GPU IDs that need to be excluded. (example: \"0,1,2\" | \"0-2\")")
	pflag.StringVar(&o.DevicePluginPath, "device-plugin-path", o.DevicePluginPath, "The path for kubelet receive device plugin registration.")
	pflag.IntVar(&o.PprofBindPort, "pprof-bind-port", o.PprofBindPort, "The port that the debugger listens. (default disable service)")
	pflag.BoolVar(&o.GDSEnabled, "gds-enabled", o.GDSEnabled, "Ensure that containers are started with NVIDIA_GDS=enabled.")
	pflag.BoolVar(&o.MOFEDEnabled, "mofed-enabled", o.MOFEDEnabled, "Ensure that containers are started with NVIDIA_MOFED=enabled.")
	pflag.StringVar(&o.MigStrategy, "mig-strategy", o.MigStrategy, "Strategy for starting MIG device plugin service. (supported values: \"none\" | \"single\" | \"mixed\")")
	pflag.IntSliceVar(&o.ImexChannelIDs, "imex-channel-ids", o.ImexChannelIDs, "A list of IMEX channels to inject.")
	pflag.BoolVar(&o.ImexRequired, "imex-required", o.ImexRequired, "The specified IMEX channels are required.")
	o.FeatureGate.AddFlag(pflag.CommandLine)
	pflag.BoolVar(&version, "version", false, "Print version information and quit.")
	pflag.CommandLine.AddGoFlagSet(fs)
}

func (o *Options) FlagParse() {
	pflag.Parse()
}

func (o *Options) PrintAndExitIfRequested() {
	o.FlagParse()
	if version {
		fmt.Printf("%#v\n", pkgversion.Get())
		os.Exit(0)
	}
}
