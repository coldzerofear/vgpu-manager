package options

import (
	"fmt"
	"os"

	pkgversion "github.com/coldzerofear/vgpu-manager/pkg/version"
	"github.com/spf13/pflag"
	"k8s.io/component-base/featuregate"
	baseversion "k8s.io/component-base/version"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

type Options struct {
	KubeConfigFile string
	MasterURL      string
	QPS            float64
	Burst          int

	NodeName            string
	CGroupDriver        string
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
	FeatureGate         featuregate.MutableFeatureGate
}

const (
	defaultQPS   = 20.0
	defaultBurst = 30

	defaultDeviceSplitCount    = 10
	defaultDeviceMemoryFactor  = 1
	defaultDeviceMemoryScaling = 1.0
	defaultDeviceCoresScaling  = 1.0
	defaultPprofBindPort       = 0

	Component = "devicePlugin"

	// CorePlugin feature gate will report the virtual cores of the node device to kubelet.
	CorePlugin featuregate.Feature = "CorePlugin"
	// MemoryPlugin feature gate will report the virtual memory of the node device to kubelet.
	MemoryPlugin featuregate.Feature = "MemoryPlugin"
	// Reschedule feature gate will attempt to reschedule Pods that meet the criteria.
	Reschedule featuregate.Feature = "Reschedule"
	// GPUTopology feature gate will report gpu topology information to node.
	GPUTopology featuregate.Feature = "GPUTopology"
)

var (
	version             bool
	defaultFeatureGates = map[featuregate.Feature]featuregate.FeatureSpec{
		CorePlugin:   {Default: false, PreRelease: featuregate.Alpha},
		MemoryPlugin: {Default: false, PreRelease: featuregate.Alpha},
		Reschedule:   {Default: false, PreRelease: featuregate.Alpha},
		GPUTopology:  {Default: false, PreRelease: featuregate.Alpha},
	}
)

func NewOptions() *Options {
	featureGate := featuregate.NewFeatureGate()
	if err := featureGate.Add(defaultFeatureGates); err != nil {
		panic(fmt.Sprintf("Failed to add feature gates: %v", err))
	}
	if err := featuregate.DefaultComponentGlobalsRegistry.Register(Component,
		baseversion.DefaultBuildEffectiveVersion(), featureGate); err != nil {
		panic(fmt.Sprintf("Failed to registry feature gates to global: %v", err))
	}
	gdsEnabled := os.Getenv("NVIDIA_GDS") == "enabled" || os.Getenv("NVIDIA_GDS") == "true"
	mofedEnabled := os.Getenv("NVIDIA_MOFED") == "enabled" || os.Getenv("NVIDIA_MOFED") == "true"
	return &Options{
		QPS:                 defaultQPS,
		Burst:               defaultBurst,
		NodeName:            os.Getenv("NODE_NAME"),
		CGroupDriver:        os.Getenv("CGROUP_DRIVER"),
		DeviceSplitCount:    defaultDeviceSplitCount,
		DeviceCoresScaling:  defaultDeviceCoresScaling,
		DeviceMemoryScaling: defaultDeviceMemoryScaling,
		DeviceMemoryFactor:  defaultDeviceMemoryFactor,
		DevicePluginPath:    pluginapi.DevicePluginPath,
		PprofBindPort:       defaultPprofBindPort,
		GDSEnabled:          gdsEnabled,
		MOFEDEnabled:        mofedEnabled,
		FeatureGate:         featureGate,
	}
}

func (o *Options) InitFlags(fs *pflag.FlagSet) {
	fs.StringVar(&o.KubeConfigFile, "kubeconfig", o.KubeConfigFile, "Path to a kubeconfig. Only required if out-of-cluster.")
	fs.StringVar(&o.MasterURL, "master", o.MasterURL, "The address of the Kubernetes API server. Overrides any value in kubeconfig. Only required if out-of-cluster.")
	fs.Float64Var(&o.QPS, "kube-api-qps", o.QPS, "QPS to use while talking with kubernetes apiserver.")
	fs.IntVar(&o.Burst, "kube-api-burst", o.Burst, "Burst to use while talking with kubernetes apiserver.")
	fs.StringVar(&o.NodeName, "node-name", o.NodeName, "If non-empty, will use this string as identification instead of the actual node name.")
	fs.StringVar(&o.CGroupDriver, "cgroup-driver", o.CGroupDriver, "Specify the cgroup driver used. (example: cgroupfs | systemd)")
	fs.IntVar(&o.DeviceSplitCount, "device-split-count", o.DeviceSplitCount, "The number for NVIDIA device split.")
	fs.Float64Var(&o.DeviceCoresScaling, "device-cores-scaling", o.DeviceCoresScaling, "The ratio for NVIDIA device cores scaling.")
	fs.Float64Var(&o.DeviceMemoryScaling, "device-memory-scaling", o.DeviceMemoryScaling, "The ratio for NVIDIA device memory scaling.")
	fs.IntVar(&o.DeviceMemoryFactor, "device-memory-factor", o.DeviceMemoryFactor, "The default gpu memory block size is 1MB.")
	fs.StringVar(&o.NodeConfigPath, "node-config-path", o.NodeConfigPath, "Specify the node configuration path to apply differentiated configuration to the node.")
	fs.StringVar(&o.ExcludeDevices, "exclude-devices", "", "Specify the GPU IDs that need to be excluded. (example: 0,1,2 | 0-2)")
	fs.StringVar(&o.DevicePluginPath, "device-plugin-path", o.DevicePluginPath, "The path for kubelet receive device plugin registration.")
	fs.IntVar(&o.PprofBindPort, "pprof-bind-port", o.PprofBindPort, "The port that the debugger listens. (default disable service)")
	fs.BoolVar(&version, "version", false, "Print version information and quit.")
	fs.BoolVar(&o.GDSEnabled, "gds-enabled", o.GDSEnabled, "Ensure that containers are started with NVIDIA_GDS=enabled.")
	fs.BoolVar(&o.MOFEDEnabled, "mofed-enabled", o.MOFEDEnabled, "Ensure that containers are started with NVIDIA_MOFED=enabled.")
	o.FeatureGate.AddFlag(fs)
	pflag.Parse()
}

func (o *Options) PrintAndExitIfRequested() {
	if version {
		fmt.Printf("%#v\n", pkgversion.Get())
		os.Exit(0)
	}
}
