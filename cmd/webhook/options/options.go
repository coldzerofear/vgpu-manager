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
	ServerBindPort      int
	PprofBindPort       int
	Domain              string
	CertDir             string
	SchedulerName       string
	DefaultNodePolicy   string
	DefaultDevicePolicy string
	DefaultTopologyMode string
	DefaultRuntimeClass string
}

const (
	defaultServerBindPort = 9443
	defaultPprofBindPort  = 0
	defaultCertDir        = "/tmp/k8s-webhook-server/serving-certs"
)

func NewOptions() *Options {
	return &Options{
		ServerBindPort: defaultServerBindPort,
		PprofBindPort:  defaultPprofBindPort,
		Domain:         util.GetGlobalDomain(),
		CertDir:        defaultCertDir,
	}
}

var version bool

func (o *Options) InitFlags(fs *flag.FlagSet) {
	klog.InitFlags(fs)
	pflag.CommandLine.SortFlags = false
	pflag.StringVar(&o.SchedulerName, "scheduler-name", o.SchedulerName, "Specify scheduler name and automatically set it to vGPU pod.")
	pflag.IntVar(&o.ServerBindPort, "server-bind-port", o.ServerBindPort, "The port on which the server listens.")
	pflag.IntVar(&o.PprofBindPort, "pprof-bind-port", o.PprofBindPort, "The port that the debugger listens. (default disable service)")
	pflag.StringVar(&o.Domain, "domain", o.Domain, "Set global domain name to replace all resource and annotation domains.")
	pflag.StringVar(&o.CertDir, "cert-dir", o.CertDir, "CertDir is the directory that contains the server key and certificate.")
	pflag.StringVar(&o.DefaultNodePolicy, "default-node-policy", "", "Default node scheduling policy. (supported values: \"binpack\" | \"spread\")")
	pflag.StringVar(&o.DefaultDevicePolicy, "default-device-policy", "", "Default device scheduling policy. (supported values: \"binpack\" | \"spread\")")
	pflag.StringVar(&o.DefaultDevicePolicy, "default-topology-mode", "", "Default device list topology mode. (supported values: \"numa\" | \"link\")")
	pflag.StringVar(&o.DefaultRuntimeClass, "default-runtime-class", "", "Specify the default container runtimeClassName for the vGPU pod.")
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
