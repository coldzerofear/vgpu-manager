package options

import (
	"flag"
	"fmt"
	"os"

	pkgversion "github.com/coldzerofear/vgpu-manager/pkg/version"
	"github.com/spf13/pflag"
	"k8s.io/component-base/featuregate"
	baseversion "k8s.io/component-base/version"
	"k8s.io/klog/v2"
)

type Options struct {
	SchedulerName       string
	KubeConfigFile      string
	MasterURL           string
	QPS                 float64
	Burst               int
	ServerBindProt      int
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
	defaultServerBindProt      = 3456
	defaultPprofBindPort       = 0
	defaultCertRefreshInterval = 5

	Component = "scheduler"

	// SerialBindNode feature gate will binding node operation of serial execution scheduler.
	SerialBindNode featuregate.Feature = "SerialBindNode"
	// GPUTopology feature gate will consider topology structure when allocating devices.
	GPUTopology featuregate.Feature = "GPUTopology"
)

var (
	version             bool
	defaultFeatureGates = map[featuregate.Feature]featuregate.FeatureSpec{
		SerialBindNode: {Default: false, PreRelease: featuregate.Alpha},
		GPUTopology:    {Default: false, PreRelease: featuregate.Alpha},
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
	return &Options{
		SchedulerName:       defaultSchedulerName,
		QPS:                 defaultQPS,
		Burst:               defaultBurst,
		ServerBindProt:      defaultServerBindProt,
		PprofBindPort:       defaultPprofBindPort,
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
	pflag.StringVar(&o.SchedulerName, "scheduler-name", o.SchedulerName, "Specify scheduler name.")
	pflag.IntVar(&o.ServerBindProt, "server-bind-port", o.ServerBindProt, "The port on which the server listens.")
	pflag.IntVar(&o.PprofBindPort, "pprof-bind-port", o.PprofBindPort, "The port that the debugger listens. (default disable service)")
	pflag.BoolVar(&o.EnableTls, "enable-tls", false, "Open TLS encrypted communication for the server. (default: false)")
	pflag.StringVar(&o.TlsKeyFile, "tls-key-file", "", "Specify tls key file path. (need enable tls)")
	pflag.StringVar(&o.TlsCertFile, "tls-cert-file", "", "Specify tls cert file path. (need enable tls)")
	pflag.IntVar(&o.CertRefreshInterval, "cert-refresh-interval", o.CertRefreshInterval, "Certificate refresh interval in seconds.")
	o.FeatureGate.AddFlag(pflag.CommandLine)
	pflag.BoolVar(&version, "version", false, "Print version information and quit.")
	pflag.CommandLine.AddGoFlagSet(fs)
	pflag.Parse()
}

func (o *Options) PrintAndExitIfRequested() {
	if version {
		fmt.Printf("%#v\n", pkgversion.Get())
		os.Exit(0)
	}
}
