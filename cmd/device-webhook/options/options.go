package options

import (
	"flag"
	"fmt"
	"os"

	"github.com/coldzerofear/vgpu-manager/pkg/util"
	pkgversion "github.com/coldzerofear/vgpu-manager/pkg/version"
	"github.com/spf13/pflag"
	"k8s.io/klog/v2"
)

type Options struct {
	KubeConfigFile string
	MasterURL      string
	QPS            float32
	Burst          int

	ServerBindPort        int
	PprofBindPort         int
	Domain                string
	CertDir               string
	TlsCertName           string
	TlsKeyName            string
	SchedulerName         string
	DefaultNodePolicy     string
	DefaultDevicePolicy   string
	DefaultTopologyMode   string
	DefaultRuntimeClass   string
	VGPUDeviceClassName   string
	DRAAdmissionEnabled   bool
	DefaultConvertToDRA   bool
	CombinedResourceClaim bool
}

const (
	defaultQPS             = 20.0
	defaultBurst           = 30
	defaultServerBindPort  = 9443
	defaultPprofBindPort   = 0
	defaultCertDir         = "/tmp/k8s-webhook-server/serving-certs"
	defaultTlsCertName     = "tls.crt"
	defaultTlsKeyName      = "tls.key"
	defaultVGPUDeviceClass = util.VGPUDeviceClassName
)

func NewOptions() *Options {
	return &Options{
		QPS:                 defaultQPS,
		Burst:               defaultBurst,
		ServerBindPort:      defaultServerBindPort,
		PprofBindPort:       defaultPprofBindPort,
		Domain:              util.GetGlobalDomain(),
		CertDir:             defaultCertDir,
		TlsCertName:         defaultTlsCertName,
		TlsKeyName:          defaultTlsKeyName,
		VGPUDeviceClassName: defaultVGPUDeviceClass,
	}
}

var version bool

func (o *Options) InitFlags(fs *flag.FlagSet) {
	klog.InitFlags(fs)
	// Opt into the new klog behavior so that -stderrthreshold is honored even
	// when -logtostderr=true (the default).
	// Ref: kubernetes/klog#212, kubernetes/klog#432
	_ = fs.Set("legacy_stderr_threshold_behavior", "false")
	_ = fs.Set("stderrthreshold", "INFO")
	pflag.CommandLine.SortFlags = false
	pflag.StringVar(&o.KubeConfigFile, "kubeconfig", o.KubeConfigFile, "Path to a kubeconfig. Only required if out-of-cluster.")
	pflag.StringVar(&o.MasterURL, "master", o.MasterURL, "The address of the Kubernetes API server. Overrides any value in kubeconfig. Only required if out-of-cluster.")
	pflag.Float32Var(&o.QPS, "kube-api-qps", o.QPS, "QPS to use while talking with kubernetes apiserver.")
	pflag.IntVar(&o.Burst, "kube-api-burst", o.Burst, "Burst to use while talking with kubernetes apiserver.")
	pflag.StringVar(&o.SchedulerName, "scheduler-name", o.SchedulerName, "Specify scheduler name and automatically set it to vGPU pod.")
	pflag.IntVar(&o.ServerBindPort, "server-bind-port", o.ServerBindPort, "The port on which the server listens.")
	pflag.IntVar(&o.PprofBindPort, "pprof-bind-port", o.PprofBindPort, "The port that the debugger listens. (default disable)")
	pflag.StringVar(&o.Domain, "domain", o.Domain, "Set global domain name to replace all resource and annotation domains.")
	pflag.StringVar(&o.CertDir, "cert-dir", o.CertDir, "CertDir is the directory that contains the server key and certificate.")
	pflag.StringVar(&o.TlsCertName, "tls-cert-name", o.TlsCertName, "Specify the tls cert file name in the certificate directory.")
	pflag.StringVar(&o.TlsKeyName, "tls-key-name", o.TlsKeyName, "Specify the tls key file name in the certificate directory.")
	pflag.StringVar(&o.DefaultNodePolicy, "default-node-policy", "", "Default node scheduling policy. (supported values: \"binpack\" | \"spread\")")
	pflag.StringVar(&o.DefaultDevicePolicy, "default-device-policy", "", "Default device scheduling policy. (supported values: \"binpack\" | \"spread\")")
	pflag.StringVar(&o.DefaultTopologyMode, "default-topology-mode", "", "Default device list topology mode. (supported values: \"numa\" | \"link\")")
	pflag.StringVar(&o.DefaultRuntimeClass, "default-runtime-class", "", "Specify the default container runtimeClassName for the vGPU pod.")
	pflag.StringVar(&o.VGPUDeviceClassName, "vgpu-device-class-name", o.VGPUDeviceClassName, "Specify the name of the vGPU device class for DRA conversion.")
	pflag.BoolVar(&o.DRAAdmissionEnabled, "dra-admission-enabled", false, "Enable access verification for requests related to DRA resources.")
	pflag.BoolVar(&o.DefaultConvertToDRA, "default-convert2-dra", false, "Enable the conversion of vGPU extended resources into DRA requests.")
	pflag.BoolVar(&o.CombinedResourceClaim, "combined-resource-claim", false, "combine multiple claim requests into one resource claim. (need to enable `default-convert2-dra`)")
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
