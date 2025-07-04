package options

import (
	"flag"
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
	ServerBindPort int
	PprofBindPort  int
}

const (
	defaultQPS            = 20.0
	defaultBurst          = 30
	defaultServerBindPort = 3456
	defaultPprofBindPort  = 0
)

func NewOptions() *Options {
	return &Options{
		QPS:            defaultQPS,
		Burst:          defaultBurst,
		NodeName:       os.Getenv("NODE_NAME"),
		CGroupDriver:   os.Getenv("CGROUP_DRIVER"),
		ServerBindPort: defaultServerBindPort,
		PprofBindPort:  defaultPprofBindPort,
	}
}

var version bool

func (o *Options) InitFlags(fs *flag.FlagSet) {
	pflag.CommandLine.SortFlags = false
	pflag.StringVar(&o.KubeConfigFile, "kubeconfig", o.KubeConfigFile, "Path to a kubeconfig. Only required if out-of-cluster.")
	pflag.StringVar(&o.MasterURL, "master", o.MasterURL, "The address of the Kubernetes API server. Overrides any value in kubeconfig. Only required if out-of-cluster.")
	pflag.Float64Var(&o.QPS, "kube-api-qps", o.QPS, "QPS to use while talking with kubernetes apiserver.")
	pflag.IntVar(&o.Burst, "kube-api-burst", o.Burst, "Burst to use while talking with kubernetes apiserver.")
	pflag.StringVar(&o.NodeName, "node-name", o.NodeName, "If non-empty, will use this string as identification instead of the actual node name.")
	pflag.StringVar(&o.CGroupDriver, "cgroup-driver", o.CGroupDriver, "Specify the cgroup driver used. (supported values: \"cgroupfs\" | \"systemd\")")
	pflag.StringVar(&o.NodeConfigPath, "node-config-path", o.NodeConfigPath, "Specify the node configuration path to apply differentiated configuration to the node.")
	pflag.IntVar(&o.ServerBindPort, "server-bind-port", o.ServerBindPort, "The port on which the server listens.")
	pflag.IntVar(&o.PprofBindPort, "pprof-bind-port", o.PprofBindPort, "The port that the debugger listens. (default disable service)")
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
