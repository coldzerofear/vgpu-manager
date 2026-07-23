package options

import (
	"flag"
	"fmt"
	"os"
	"time"

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
	QPS            float32
	Burst          int

	Domain              string
	SchedulerName       string
	ServerBindPort      int
	PprofBindPort       int
	EnableTls           bool
	TlsKeyFile          string
	TlsCertFile         string
	CertRefreshInterval time.Duration
	StuckGracePeriod    time.Duration
	BestEffortMaxGPUs   int
	FeatureGate         featuregate.MutableFeatureGate

	WatchLease                   bool
	LeaderIdentityPrefix         string
	LeaderElect                  bool
	LeaderElectResourceName      string
	LeaderElectResourceNamespace string
}

const (
	defaultSchedulerName       = "vgpu-scheduler"
	defaultQPS                 = 20.0
	defaultBurst               = 30
	defaultServerBindPort      = 3456
	defaultPprofBindPort       = 0
	defaultCertRefreshInterval = 5 * time.Second
	defaultStuckGracePeriod    = 30 * time.Second
	// defaultBestEffortMaxGPUs caps the exhaustive bestEffort link-allocation
	// search. Beyond this candidate count we fall back to a greedy O(n²·k)
	// allocator to keep filter latency bounded on dense GPU nodes (16+ cards).
	// Empirically 12 leaves the exhaustive search around the ~100k partitions
	// range; 16 partitioning into 8 sets is already ~2M and noticeably slow.
	defaultBestEffortMaxGPUs = 12

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
		SerialFilterNode: {Default: true, PreRelease: featuregate.Beta},
		GPUTopology:      {Default: false, PreRelease: featuregate.Alpha},
	}
)

func NewOptions() *Options {
	featureGate := featuregate.NewFeatureGate()
	runtime.Must(featureGate.Add(defaultFeatureGates))
	runtime.Must(compatibility.DefaultComponentGlobalsRegistry.Register(
		Component,
		compatibility.DefaultBuildEffectiveVersion(),
		featureGate,
	))
	identityPrefix := util.GetEnvDefault("POD_NAME", os.Getenv("HOSTNAME"))
	if identityPrefix == "" {
		identityPrefix, _ = os.Hostname()
	}
	return &Options{
		QPS:                  defaultQPS,
		Burst:                defaultBurst,
		ServerBindPort:       defaultServerBindPort,
		PprofBindPort:        defaultPprofBindPort,
		Domain:               util.GetGlobalDomain(),
		SchedulerName:        defaultSchedulerName,
		CertRefreshInterval:  defaultCertRefreshInterval,
		StuckGracePeriod:     defaultStuckGracePeriod,
		BestEffortMaxGPUs:    defaultBestEffortMaxGPUs,
		LeaderIdentityPrefix: identityPrefix,
		FeatureGate:          featureGate,
	}
}

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
	pflag.BoolVar(&o.WatchLease, "watch-lease", false, "Watch and share a lease created by other components.")
	pflag.BoolVar(&o.LeaderElect, "leader-elect", false, "Enable leader election function, Ensure that there is only one instance that remains active.")
	pflag.StringVar(&o.LeaderIdentityPrefix, "leader-identity-prefix", o.LeaderIdentityPrefix, "Watch and share a lease created by other components.")
	pflag.StringVar(&o.LeaderElectResourceName, "leader-elect-resource-name", "", "Enabling the leader-election or watch-lease feature requires specifying the leader lease resource name.")
	pflag.StringVar(&o.LeaderElectResourceNamespace, "leader-elect-resource-namespace", "", "Enabling the leader-election or watch-lease feature requires specifying the leader lease resource namespace.")
	pflag.StringVar(&o.Domain, "domain", o.Domain, "Set global domain name to replace all resource and annotation domains.")
	pflag.StringVar(&o.SchedulerName, "scheduler-name", o.SchedulerName, "Specify scheduler name.")
	pflag.IntVar(&o.ServerBindPort, "server-bind-port", o.ServerBindPort, "The port on which the server listens.")
	pflag.IntVar(&o.PprofBindPort, "pprof-bind-port", o.PprofBindPort, "The port that the debugger listens. (default disable)")
	pflag.BoolVar(&o.EnableTls, "enable-tls", false, "Open TLS encrypted communication for the server. (default: false)")
	pflag.StringVar(&o.TlsKeyFile, "tls-key-file", "", "Specify tls key file path. (need --enable-tls)")
	pflag.StringVar(&o.TlsCertFile, "tls-cert-file", "", "Specify tls cert file path. (need --enable-tls)")
	pflag.DurationVar(&o.CertRefreshInterval, "cert-refresh-interval", o.CertRefreshInterval, "Certificate refresh interval duration.")
	pflag.DurationVar(&o.StuckGracePeriod, "stuck-grace-period", o.StuckGracePeriod, "Scheduling stuck grace period, filtering the maximum delay time to the binding stage.")
	pflag.IntVar(&o.BestEffortMaxGPUs, "best-effort-max-gpus", o.BestEffortMaxGPUs,
		"When the candidate GPU count on a node exceeds this threshold, the link-topology "+
			"allocator falls back to an O(n²·k) greedy algorithm instead of the exhaustive "+
			"bestEffort partition search, keeping filter latency bounded on dense nodes.")
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
