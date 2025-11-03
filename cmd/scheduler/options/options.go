package options

import (
	"flag"
	"fmt"
	"os"

	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"k8s.io/apiserver/pkg/util/compatibility"
	"k8s.io/klog/v2"

	pkgversion "github.com/coldzerofear/vgpu-manager/pkg/version"
	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/component-base/featuregate"
)

type Options struct {
	KubeConfigFile string
	MasterURL      string
	QPS            float64
	Burst          int

	Domain              string
	SchedulerName       string
	ServerBindPort      int
	PprofBindPort       int
	EnableTls           bool
	TlsKeyFile          string
	TlsCertFile         string
	CertRefreshInterval int
	FeatureGate         featuregate.MutableFeatureGate
}

const (
	defaultSchedulerName       = "vgpu-scheduler"
	defaultQPS                 = 20.0
	defaultBurst               = 30
	defaultServerBindPort      = 3456
	defaultPprofBindPort       = 0
	defaultCertRefreshInterval = 5

	Component = "scheduler"

	// SerialBindNode feature gate will serially execute the binding node operations of the scheduler.
	SerialBindNode featuregate.Feature = util.SerialBindNode
	// SerialFilterNode feature gate will serially execute the filter node operations of the scheduler.
	SerialFilterNode featuregate.Feature = util.SerialFilterNode
	// GPUTopology feature gate will consider topology structure when allocating devices.
	GPUTopology featuregate.Feature = util.GPUTopology
)

var (
	version             bool
	defaultFeatureGates = map[featuregate.Feature]featuregate.FeatureSpec{
		SerialBindNode:   {Default: true, PreRelease: featuregate.Beta},
		SerialFilterNode: {Default: false, PreRelease: featuregate.Alpha},
		GPUTopology:      {Default: false, PreRelease: featuregate.Alpha},
	}
)

func NewOptions() *Options {
	featureGate := featuregate.NewFeatureGate()
	runtime.Must(featureGate.Add(defaultFeatureGates))
	runtime.Must(compatibility.DefaultComponentGlobalsRegistry.Register(
		Component, compatibility.DefaultBuildEffectiveVersion(), featureGate))
	return &Options{
		QPS:                 defaultQPS,
		Burst:               defaultBurst,
		ServerBindPort:      defaultServerBindPort,
		PprofBindPort:       defaultPprofBindPort,
		Domain:              util.GetGlobalDomain(),
		SchedulerName:       defaultSchedulerName,
		CertRefreshInterval: defaultCertRefreshInterval,
		FeatureGate:         featureGate,
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
	pflag.StringVar(&o.SchedulerName, "scheduler-name", o.SchedulerName, "Specify scheduler name.")
	pflag.IntVar(&o.ServerBindPort, "server-bind-port", o.ServerBindPort, "The port on which the server listens.")
	pflag.IntVar(&o.PprofBindPort, "pprof-bind-port", o.PprofBindPort, "The port that the debugger listens. (default disable)")
	pflag.BoolVar(&o.EnableTls, "enable-tls", false, "Open TLS encrypted communication for the server. (default: false)")
	pflag.StringVar(&o.TlsKeyFile, "tls-key-file", "", "Specify tls key file path. (need enable tls)")
	pflag.StringVar(&o.TlsCertFile, "tls-cert-file", "", "Specify tls cert file path. (need enable tls)")
	pflag.IntVar(&o.CertRefreshInterval, "cert-refresh-interval", o.CertRefreshInterval, "Certificate refresh interval in seconds.")
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
