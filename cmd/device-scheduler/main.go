/*
Copyright 2024-2026 coldzerofear

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin/featuregates"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/component-base/logs"
	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"github.com/coldzerofear/vgpu-manager/cmd/device-scheduler/options"
	"github.com/coldzerofear/vgpu-manager/pkg/client"
	"github.com/coldzerofear/vgpu-manager/pkg/route"
	"github.com/coldzerofear/vgpu-manager/pkg/scheduler/bind"
	"github.com/coldzerofear/vgpu-manager/pkg/scheduler/filter"
	"github.com/coldzerofear/vgpu-manager/pkg/scheduler/metrics"
	"github.com/coldzerofear/vgpu-manager/pkg/scheduler/preempt"
	tlsconfig "github.com/grepplabs/cert-source/config"
	tlsserver "github.com/grepplabs/cert-source/tls/server"
	tlsserverconfig "github.com/grepplabs/cert-source/tls/server/config"
	"github.com/julienschmidt/httprouter"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/scheme"
	typedv1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"
	_ "k8s.io/component-base/metrics/prometheus/clientgo"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/cache"
)

var Scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(scheme.AddToScheme(Scheme))
}

func runApp(opt *options.Options) (exitCode int) {
	exitCode = 1

	klog.Infof("Feature Gates: %#v", featuregates.ToMap(opt.FeatureGate))
	util.MustInitGlobalDomain(opt.Domain)
	device.MustInitGlobalStuckGracePeriod(opt.StuckGracePeriod)

	kubeClient, err := client.NewClientSet(
		client.WithConfigMasterURL(opt.MasterURL),
		client.WithKubeConfigPath(opt.KubeConfigFile),
		client.WithQPSBurst(opt.QPS, opt.Burst),
		client.WithDefaultUserAgent())
	if err != nil {
		klog.Errorf("Create kubeClient failed: %v", err)
		return exitCode
	}

	var tlsConfig *tls.Config
	if opt.EnableTls {
		if len(opt.TlsKeyFile) == 0 || len(opt.TlsCertFile) == 0 {
			klog.Errorf("Enable Tls but did not specify a certificate file: "+
				"tlsKeyFile: %q, tlsCertFile: %q", opt.TlsKeyFile, opt.TlsCertFile)
			return exitCode
		}
		if opt.CertRefreshInterval <= 0 {
			klog.Warningf("Certificate refresh interval is less than or equal to 0, " +
				"and the automatic certificate rotation function will be turned off")
		}

		tlsConfig, err = tlsserverconfig.GetServerTLSConfig(slog.Default(), &tlsconfig.TLSServerConfig{
			Enable:  opt.EnableTls,
			Refresh: opt.CertRefreshInterval,
			File: tlsconfig.TLSServerFiles{
				Key:  opt.TlsKeyFile,
				Cert: opt.TlsCertFile,
			},
			// Using http/1.1 will prevent from being vulnerable to the HTTP/2 Stream Cancellation and Rapid Reset CVEs.
			// For more information see:
			// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
			// - https://github.com/advisories/GHSA-4374-p667-p6c8
		}, tlsserver.WithTLSServerNextProtos([]string{"http/1.1"}))
		if err != nil {
			klog.Errorf("GetServerTLSConfig failed: %v", err)
			return exitCode
		}
	}

	broadcaster := record.NewBroadcaster()
	defer broadcaster.Shutdown()
	broadcaster.StartRecordingToSink(&typedv1.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})
	recorder := broadcaster.NewRecorder(Scheme, corev1.EventSource{Component: opt.SchedulerName})

	// trim managedFields to reduce cache memory usage.
	option := informers.WithTransform(cache.TransformStripManagedFields())
	factory := informers.NewSharedInformerFactoryWithOptions(kubeClient, 10*time.Hour, option)
	filterPlugin, err := filter.New(
		kubeClient, factory, recorder,
		opt.FeatureGate.Enabled(options.SerializedNodeFilter),
		opt.FeatureGate.Enabled(options.TopologyAwareGPUAllocation))
	if err != nil {
		klog.Errorf("Initialization of scheduler FilterPlugin failed: %v", err)
		return exitCode
	}

	bindPlugin, err := bind.New(
		kubeClient, recorder, filterPlugin.GetPodLister(),
		opt.FeatureGate.Enabled(options.SerializedNodeBind))
	if err != nil {
		klog.Errorf("Initialization of scheduler BindPlugin failed: %v", err)
		return exitCode
	}

	preemptPlugin, err := preempt.New(
		kubeClient, factory, recorder, filterPlugin.GetPodLister(),
		opt.FeatureGate.Enabled(options.TopologyAwareGPUAllocation))
	if err != nil {
		klog.Errorf("Initialization of scheduler PreemptPlugin failed: %v", err)
		return exitCode
	}

	if opt.WatchLease && opt.LeaderElect {
		klog.Errorln("The watch-lease and leader-elect functions are mutually exclusive and cannot be enabled simultaneously")
		return exitCode
	}
	podName := strings.TrimSpace(os.Getenv("POD_NAME"))
	podNamespace := strings.TrimSpace(os.Getenv("POD_NAMESPACE"))
	leaseName := strings.TrimSpace(opt.LeaderElectResourceName)
	leaseNamespace := strings.TrimSpace(opt.LeaderElectResourceNamespace)
	if opt.WatchLease || opt.LeaderElect {
		if leaseName == "" {
			klog.Errorln("Enabling leader-elect or watch-lease requires specifying leader-elect-resource-name")
			return exitCode
		}
		if leaseNamespace == "" {
			klog.Errorln("Enabling leader-elect or watch-lease requires specifying leader-elect-resource-namespace")
			return exitCode
		}
		if podName == "" || podNamespace == "" {
			klog.Errorln("Enabling leader-elect or watch-lease requires specifying environment variable 'POD_NAME' and 'POD_NAMESPACE'")
			return exitCode
		}
	}

	ctx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()

	isLeaderFunc := func() bool { return true }
	if opt.WatchLease {
		klog.Infoln("Watch lease enabled: Initialize lease detector")
		leaderIdentityPrefix := strings.TrimSpace(opt.LeaderIdentityPrefix)
		if leaderIdentityPrefix == "" {
			klog.Errorln("Enabling watch-lease requires specifying leader-identity-prefix")
			return exitCode
		}
		leaseDetector, err := NewLeaseDetector(factory,
			leaseNamespace, leaseName, leaderIdentityPrefix,
			WithStartCallback(func() {
				patchPodRoleLabel(kubeClient, podName, podNamespace, util.SchedulerRoleValueFollower)
			}),
			WithLeaderCallback(func() {
				patchPodRoleLabel(kubeClient, podName, podNamespace, util.SchedulerRoleValueLeader)
			}),
			WithReleaseCallback(func() {
				patchPodRoleLabel(kubeClient, podName, podNamespace, util.SchedulerRoleValueFollower)
			}),
		)
		if err != nil {
			klog.Errorf("Initialization of LeaseDetector failed: %v", err)
			return exitCode
		}
		isLeaderFunc = leaseDetector.IsLeader
	}

	if opt.LeaderElect {
		klog.Infoln("Leader elect enabled: Initialize leader elect")
		leaderIdentity := uuid.NewString()
		if leaderIdentityPrefix := strings.TrimSpace(opt.LeaderIdentityPrefix); leaderIdentityPrefix != "" {
			leaderIdentity = fmt.Sprintf("%s_%s", leaderIdentityPrefix, leaderIdentity)
		}
		leaderElector, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
			Lock: &resourcelock.LeaseLock{
				LeaseMeta: metav1.ObjectMeta{
					Name:      leaseName,
					Namespace: leaseNamespace,
				},
				Client: kubeClient.CoordinationV1(),
				LockConfig: resourcelock.ResourceLockConfig{
					Identity:      leaderIdentity,
					EventRecorder: recorder,
				},
			},
			// NewLeaderElector rejects a config that leaves these zero
			// ("leaseDuration must be greater than renewDeadline"), which
			// would abort startup, so they are not optional. Values are the
			// kube-scheduler defaults and satisfy its two constraints:
			// LeaseDuration > RenewDeadline > RetryPeriod * JitterFactor(1.2).
			LeaseDuration: 15 * time.Second,
			RenewDeadline: 10 * time.Second,
			RetryPeriod:   2 * time.Second,
			// Hands the lease back on shutdown so a standby can take over
			// without waiting it out. Needs the process to outlive the
			// release call -- main() sleeps after runApp returns, and the
			// HTTP server is stopped before cancelFunc, so nothing is served
			// between giving up the lease and exiting.
			ReleaseOnCancel: true,
			Callbacks: leaderelection.LeaderCallbacks{
				OnStartedLeading: func(ctx context.Context) {
					klog.Infof("started leader identity: %s", leaderIdentity)
					patchPodRoleLabel(kubeClient, podName, podNamespace, util.SchedulerRoleValueLeader)
				},
				OnStoppedLeading: func() {
					klog.Infoln("stopped leader elect")
					patchPodRoleLabel(kubeClient, podName, podNamespace, util.SchedulerRoleValueFollower)
				},
				OnNewLeader: func(identity string) {
					if leaderIdentity == identity {
						patchPodRoleLabel(kubeClient, podName, podNamespace, util.SchedulerRoleValueLeader)
					} else {
						klog.Infof("new leader elected: %s", identity)
						patchPodRoleLabel(kubeClient, podName, podNamespace, util.SchedulerRoleValueFollower)
					}
				},
			},
		})
		if err != nil {
			klog.Errorf("Initialization of LeaderElector failed: %v", err)
			return exitCode
		}
		go leaderElector.Run(ctx)
		isLeaderFunc = leaderElector.IsLeader
	}

	handler := httprouter.New()
	route.AddVersion(handler)
	route.AddHealthProbe(handler)
	route.AddReadyHandler(handler, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !util.InformerFactoryHasSynced(factory, r.Context()) {
			http.Error(w, "internal server error: not synchronized yet completed", http.StatusInternalServerError)
			return
		} else if !isLeaderFunc() {
			klog.V(4).Infoln("internal server unavailable: instance is not a leader")
		}
		http.Error(w, "ok", http.StatusOK)
	}))
	route.AddFilterPredicate(handler, filterPlugin)
	route.AddFilterDryRunPredicate(handler, filterPlugin)
	route.AddBindPredicate(handler, bindPlugin)
	route.AddPreemptPredicate(handler, preemptPlugin)
	// Served on the extender's existing port: the endpoint inherits its TLS
	// setting and needs no extra chart plumbing (port, probe, NetworkPolicy).
	route.AddMetricsHandle(handler, metrics.Handler())

	factory.StartWithContext(ctx)
	if klog.V(4).Enabled() {
		go func() {
			klog.Infoln("Waiting for InformerFactory cache synchronization...")
			if util.InformerFactoryHasSynced(factory, ctx) {
				klog.Infoln("InformerFactory cache synchronization successful")
			}
		}()
	}

	// Start pprof debug debugging service.
	route.StartDebugServer(opt.PprofBindPort)
	server := http.Server{
		Addr:              "0.0.0.0:" + strconv.Itoa(opt.ServerBindPort),
		Handler:           handler,
		TLSConfig:         tlsConfig,
		ReadHeaderTimeout: 15 * time.Second,
		ReadTimeout:       60 * time.Second,
	}
	go func() {
		if opt.EnableTls {
			klog.Infof("Tls Server starting on <0.0.0.0:%d>", opt.ServerBindPort)
			err = server.ListenAndServeTLS("", "")
		} else {
			klog.Infof("Server starting on <0.0.0.0:%d>", opt.ServerBindPort)
			err = server.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			klog.Errorf("Server error occurred: %v", err)
			cancelFunc()
		}
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	select {
	case s := <-sigChan:
		klog.Infof("Received signal %v, shutting down...", s)
		if err = server.Shutdown(context.Background()); err != nil {
			klog.Errorf("Error while stopping extender service: %s", err.Error())
		}
		cancelFunc()
		exitCode = 0
	case <-ctx.Done():
		klog.Errorln("Internal error, service abnormal stop")
		exitCode = 1
	}

	return exitCode
}

func patchPodRoleLabel(kubeClient kubernetes.Interface, podName, podNamespace, roleValue string) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: podName, Namespace: podNamespace,
	}}
	if err := client.PatchPodMetadata(kubeClient, pod, client.PatchMetadata{
		Labels: map[string]*string{util.SchedulerRoleLabel: &roleValue},
	}); err != nil && !apierrors.IsNotFound(err) {
		klog.ErrorS(err, "patch pod leader labels failed", "pod", klog.KObj(pod), "role", roleValue)
	}
}

func main() {
	opt := options.NewOptions()
	opt.InitFlags(flag.CommandLine)
	opt.PrintAndExitIfRequested()
	logs.InitLogs()
	defer logs.FlushLogs()

	exitCode := runApp(opt)
	time.Sleep(5 * time.Second)
	if exitCode != 0 {
		klog.FlushAndExit(klog.ExitFlushTimeout, exitCode)
	}
}
