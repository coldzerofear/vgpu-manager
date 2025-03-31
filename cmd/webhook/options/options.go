package options

import (
	"flag"
	"fmt"
	"os"

	pkgversion "github.com/coldzerofear/vgpu-manager/pkg/version"
	"github.com/spf13/pflag"
	"k8s.io/klog/v2"
)

type Options struct {
	ServerBindProt      int
	PprofBindPort       int
	SchedulerName       string
	CertDir             string
	DefaultNodePolicy   string
	DefaultDevicePolicy string
	DefaultTopologyMode string
}

const (
	defaultServerBindProt = 9443
	defaultPprofBindPort  = 0
	defaultCertDir        = "/tmp/k8s-webhook-server/serving-certs"
)

func NewOptions() *Options {
	return &Options{
		ServerBindProt: defaultServerBindProt,
		PprofBindPort:  defaultPprofBindPort,
		CertDir:        defaultCertDir,
	}
}

var version bool

func (o *Options) InitFlags(fs *flag.FlagSet) {
	klog.InitFlags(fs)
	pflag.CommandLine.SortFlags = false
	pflag.StringVar(&o.SchedulerName, "scheduler-name", o.SchedulerName, "Specify scheduler name and automatically set it to vGPU pod.")
	pflag.IntVar(&o.ServerBindProt, "server-bind-port", o.ServerBindProt, "The port on which the server listens.")
	pflag.IntVar(&o.PprofBindPort, "pprof-bind-port", o.PprofBindPort, "The port that the debugger listens. (default disable service)")
	pflag.StringVar(&o.CertDir, "cert-dir", o.CertDir, "CertDir is the directory that contains the server key and certificate.")
	pflag.StringVar(&o.DefaultNodePolicy, "default-node-policy", "", "Default node scheduling policy. (supported values: binpack | spread)")
	pflag.StringVar(&o.DefaultDevicePolicy, "default-device-policy", "", "Default device scheduling policy. (supported values: binpack | spread)")
	pflag.StringVar(&o.DefaultDevicePolicy, "default-topology-mode", "", "Default device list topology mode. (supported values: numa | link)")
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
