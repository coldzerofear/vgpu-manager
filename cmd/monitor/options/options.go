package options

import (
	"fmt"
	"os"

	pkgversion "github.com/coldzerofear/vgpu-manager/pkg/version"
	"github.com/spf13/pflag"
)

type Options struct {
	KubeConfigFile string
	MasterURL      string
	QPS            float64
	Burst          int

	NodeName       string
	CGroupDriver   string
	NodeConfigPath string
	ServerBindProt int
	PprofBindPort  int
}

const (
	defaultQPS            = 20.0
	defaultBurst          = 30
	defaultServerBindProt = 3456
	defaultPprofBindPort  = 0
)

func NewOptions() *Options {
	return &Options{
		QPS:            defaultQPS,
		Burst:          defaultBurst,
		NodeName:       os.Getenv("NODE_NAME"),
		CGroupDriver:   os.Getenv("CGROUP_DRIVER"),
		ServerBindProt: defaultServerBindProt,
		PprofBindPort:  defaultPprofBindPort,
	}
}

var version bool

func (o *Options) InitFlags(fs *pflag.FlagSet) {
	fs.StringVar(&o.KubeConfigFile, "kubeconfig", o.KubeConfigFile, "Path to a kubeconfig. Only required if out-of-cluster.")
	fs.StringVar(&o.MasterURL, "master", o.MasterURL, "The address of the Kubernetes API server. Overrides any value in kubeconfig. Only required if out-of-cluster.")
	fs.Float64Var(&o.QPS, "kube-api-qps", o.QPS, "QPS to use while talking with kubernetes apiserver.")
	fs.IntVar(&o.Burst, "kube-api-burst", o.Burst, "Burst to use while talking with kubernetes apiserver.")
	fs.StringVar(&o.NodeName, "node-name", o.NodeName, "If non-empty, will use this string as identification instead of the actual node name.")
	fs.StringVar(&o.CGroupDriver, "cgroup-driver", o.CGroupDriver, "Specify the cgroup driver used. (example: cgroupfs | systemd)")
	fs.StringVar(&o.NodeConfigPath, "node-config-path", o.NodeConfigPath, "Specify the node configuration path to apply differentiated configuration to the node.")
	fs.IntVar(&o.ServerBindProt, "server-bind-port", o.ServerBindProt, "The port on which the server listens.")
	fs.IntVar(&o.PprofBindPort, "pprof-bind-port", o.PprofBindPort, "The port that the debugger listens. (default disable service)")
	fs.BoolVar(&version, "version", false, "Print version information and quit.")
	pflag.Parse()
}

func (o *Options) PrintAndExitIfRequested() {
	if version {
		fmt.Printf("%#v\n", pkgversion.Get())
		os.Exit(0)
	}
}
