package options

import (
	"fmt"
	"os"

	pkgversion "github.com/coldzerofear/vgpu-manager/pkg/version"
	"github.com/spf13/pflag"
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
}

const (
	defaultSchedulerName       = "vgpu-scheduler"
	defaultQPS                 = 20.0
	defaultBurst               = 30
	defaultServerBindProt      = 3456
	defaultPprofBindPort       = 0
	defaultCertRefreshInterval = 5
)

func NewOptions() *Options {
	return &Options{
		SchedulerName:       defaultSchedulerName,
		QPS:                 defaultQPS,
		Burst:               defaultBurst,
		ServerBindProt:      defaultServerBindProt,
		PprofBindPort:       defaultPprofBindPort,
		CertRefreshInterval: defaultCertRefreshInterval,
	}
}

var version bool

func (o *Options) InitFlags(fs *pflag.FlagSet) {
	fs.StringVar(&o.SchedulerName, "scheduler-name", o.SchedulerName, "Specify scheduler name. (default: vgpu-scheduler)")
	fs.StringVar(&o.KubeConfigFile, "kubeconfig", o.KubeConfigFile, "Path to a kubeconfig. Only required if out-of-cluster.")
	fs.StringVar(&o.MasterURL, "master", o.MasterURL, "The address of the Kubernetes API server. Overrides any value in kubeconfig. Only required if out-of-cluster.")
	fs.Float64Var(&o.QPS, "kube-api-qps", o.QPS, "QPS to use while talking with kubernetes apiserver. (default: 20.0)")
	fs.IntVar(&o.Burst, "kube-api-burst", o.Burst, "Burst to use while talking with kubernetes apiserver. (default: 30)")
	fs.IntVar(&o.ServerBindProt, "server-bind-port", o.ServerBindProt, "The port on which the server listens. (default: 3456)")
	fs.IntVar(&o.PprofBindPort, "pprof-bind-port", o.PprofBindPort, "The port that the debugger listens. (default disable service)")
	fs.BoolVar(&o.EnableTls, "enable-tls", false, "Open TLS encrypted communication for the server. (default: false)")
	fs.StringVar(&o.TlsKeyFile, "tls-key-file", "", "Specify tls key file path. (need enable tls)")
	fs.StringVar(&o.TlsCertFile, "tls-cert-file", "", "Specify tls cert file path. (need enable tls)")
	fs.IntVar(&o.CertRefreshInterval, "cert-refresh-interval", o.CertRefreshInterval, "Certificate refresh interval in seconds. (default: 5)")
	fs.BoolVar(&version, "version", false, "Print version information and quit.")
	pflag.Parse()
}

func (o *Options) PrintAndExitIfRequested() {
	if version {
		fmt.Printf("%#v\n", pkgversion.Get())
		os.Exit(0)
	}
}
