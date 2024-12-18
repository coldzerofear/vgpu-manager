package options

import (
	"flag"
	"fmt"
	"os"

	pkgversion "github.com/coldzerofear/vgpu-manager/pkg/version"
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
	EnableMemoryPlugin  bool
	EnableCorePlugin    bool
	ExcludeDevices      string
	DevicePluginPath    string
}

const (
	defaultQPS   = 20.0
	defaultBurst = 30

	defaultDeviceSplitCount    = 10
	defaultDeviceMemoryFactor  = 1
	defaultDeviceMemoryScaling = 1.0
	defaultDeviceCoresScaling  = 1.0
)

func NewOptions() *Options {
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
	}
}

var version bool

func (o *Options) InitFlags(fs *flag.FlagSet) {
	fs.StringVar(&o.KubeConfigFile, "kubeconfig", o.KubeConfigFile, "Path to a kubeconfig. Only required if out-of-cluster.")
	fs.StringVar(&o.MasterURL, "master", o.MasterURL, "The address of the Kubernetes API server. Overrides any value in kubeconfig. Only required if out-of-cluster.")
	fs.Float64Var(&o.QPS, "kube-api-qps", o.QPS, "QPS to use while talking with kubernetes apiserver. (default: 20.0)")
	fs.IntVar(&o.Burst, "kube-api-burst", o.Burst, "Burst to use while talking with kubernetes apiserver. (default: 30)")
	fs.StringVar(&o.NodeName, "node-name", o.NodeName, "If non-empty, will use this string as identification instead of the actual node name.")
	fs.StringVar(&o.CGroupDriver, "cgroup-driver", o.CGroupDriver, "Specify the cgroup driver used. (example: cgroupfs | systemd)")
	fs.IntVar(&o.DeviceSplitCount, "device-split-count", o.DeviceSplitCount, "The number for NVIDIA device split. (default: 10)")
	fs.Float64Var(&o.DeviceCoresScaling, "device-cores-scaling", o.DeviceCoresScaling, "The ratio for NVIDIA device cores scaling. (default: 1.0)")
	fs.Float64Var(&o.DeviceMemoryScaling, "device-memory-scaling", o.DeviceMemoryScaling, "The ratio for NVIDIA device memory scaling. (default: 1.0)")
	fs.IntVar(&o.DeviceMemoryFactor, "device-memory-factor", o.DeviceMemoryFactor, "The default gpu memory block size is 1MB.")
	fs.StringVar(&o.NodeConfigPath, "node-config-path", o.NodeConfigPath, "Specify the node configuration path to apply differentiated configuration to the node.")
	fs.BoolVar(&o.EnableMemoryPlugin, "enable-memory-plugin", o.EnableMemoryPlugin, "Enable memory plugin will report the virtual memory of the node device to kubelet. (default: false)")
	fs.BoolVar(&o.EnableCorePlugin, "enable-core-plugin", o.EnableCorePlugin, "Enable core plugin will report the virtual cores of the node device to kubelet. (default: false)")
	fs.StringVar(&o.ExcludeDevices, "exclude-devices", "", "Specify the GPU IDs that need to be excluded. (example: 0,1,2 | 0-2)")
	fs.StringVar(&o.DevicePluginPath, "device-plugin-path", o.DevicePluginPath, "The path for kubelet receive device plugin registration.")

	fs.BoolVar(&version, "version", false, "Print version information and quit.")
	flag.Parse()
}

func (o *Options) PrintAndExitIfRequested() {
	if version {
		fmt.Printf("%#v\n", pkgversion.Get())
		os.Exit(0)
	}
}
