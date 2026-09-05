/*
Copyright 2026 coldzerofear

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
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin/remote"
	"github.com/coldzerofear/vgpu-manager/pkg/remoteagent"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/coldzerofear/vgpu-manager/pkg/version"
	"github.com/spf13/pflag"
	"github.com/urfave/cli/v2"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apiserver/pkg/util/compatibility"
	"k8s.io/component-base/featuregate"
	"k8s.io/component-base/logs"
	"k8s.io/klog/v2"
	pkgflags "sigs.k8s.io/dra-driver-nvidia-gpu/pkg/flags"
)

const (
	Component = "remoteAgent"
	// SharedSMUtilizationWatcher: sessions are written expecting the node-wide
	// SM sampling cache (published by the dra-server plugin's watcher thread).
	SharedSMUtilizationWatcher featuregate.Feature = util.SharedSMUtilizationWatcher
	// VirtualMemoryTracking: sessions are written with the virtual-memory ledger enabled.
	VirtualMemoryTracking featuregate.Feature = util.VirtualMemoryTracking
)

// FeatureGateFlags returns the CLI flags for the unified feature gate configuration.
func FeatureGateFlags(featureGates featuregate.MutableVersionedFeatureGate) []cli.Flag {
	var fs pflag.FlagSet

	// Add the unified feature gates flag containing both project and logging features
	fs.AddFlag(&pflag.Flag{
		Name: "feature-gates",
		Usage: "A set of key=value pairs that describe feature gates for alpha/experimental features. " +
			"Options are:\n     " + strings.Join(featureGates.KnownFeatures(), "\n     "),
		Value: featureGates.(pflag.Value), //nolint:forcetypeassert // No need for type check: FeatureGates is a *featuregate.featureGate, which implements pflag.Value.
	})

	var flags []cli.Flag
	fs.VisitAll(func(flag *pflag.Flag) {
		flags = append(flags, &cli.GenericFlag{
			Name:        flag.Name,
			Category:    "Feature Gates:",
			Usage:       flag.Usage,
			Value:       flag.Value,
			Destination: flag.Value,
			EnvVars:     []string{strings.ToUpper(strings.ReplaceAll(flag.Name, "-", "_"))},
		})
	})
	return flags
}

func main() {
	var (
		kube            pkgflags.KubeClientConfig
		cfg             remoteagent.Config
		listenEndpoints string
		featureGate     = featuregate.NewFeatureGate()
		// klog flags (-v etc.), same wiring as cmd/kubelet-plugin.
		loggingConfig = pkgflags.NewLoggingConfig()
	)

	runtime.Must(featureGate.Add(map[featuregate.Feature]featuregate.FeatureSpec{
		SharedSMUtilizationWatcher: {Default: false, PreRelease: featuregate.Alpha},
		VirtualMemoryTracking:      {Default: false, PreRelease: featuregate.Alpha},
	}))
	runtime.Must(compatibility.DefaultComponentGlobalsRegistry.Register(
		Component, compatibility.DefaultBuildEffectiveVersion(), featureGate,
	))

	flags := append([]cli.Flag{
		&cli.StringFlag{Name: "node-name", Usage: "Node this agent runs on (= the driver's pool name).", Destination: &cfg.NodeName, EnvVars: []string{"NODE_NAME"}, Required: true},
		&cli.StringFlag{Name: "driver-name", Usage: "DRA driver name whose slices/claims are consumed.", Value: util.DRADriverName, Destination: &cfg.DriverName, EnvVars: []string{"DRIVER_NAME"}},
		&cli.StringFlag{Name: "ready-file", Usage: "File written after preflight; the server container waits for it. Defaults to <session-base>/.agent-ready.", Destination: &cfg.ReadyFile, EnvVars: []string{"READY_FILE"}},
		&cli.StringFlag{Name: "container-manager-dir", Usage: "Configure the container mount path used by vgpu-manager.", Value: util.ManagerRootPath, Destination: &cfg.ContainerManagerDir, EnvVars: []string{"CONTAINER_MANAGER_DIR"}},
		&cli.StringFlag{Name: "config-session-base", Usage: "Session directory root shared with lupine-server (VGPU_CONFIG_SESSION_BASE).", Value: util.RemoteSessionBasePath, Destination: &cfg.SessionBase, EnvVars: []string{"VGPU_CONFIG_SESSION_BASE"}},
		&cli.StringFlag{Name: "remote-server-endpoint", Usage: "lupine-server endpoint to probe (URL form, http/https; host defaults to 127.0.0.1 = same pod). When the host is a loopback, the agent discovers the address other nodes can reach the server at and reports that from ServerInfo.", Value: fmt.Sprintf("127.0.0.1:%d", remote.DefaultServerPort), Destination: &cfg.ServerEndpoint, EnvVars: []string{"REMOTE_SERVER_ENDPOINT"}},
		&cli.StringFlag{Name: "advertise-server-endpoint", Usage: "lupine-server endpoint reported to other components verbatim (URL form, e.g. https://gpu-a.corp/pool-a), instead of the probed/discovered one. For DNS names or gateways this host cannot reach itself.", Destination: &cfg.AdvertiseEndpoint, EnvVars: []string{"ADVERTISE_SERVER_ENDPOINT"}},
		&cli.StringFlag{Name: "listen-server-endpoint", Usage: "Agent gRPC listen endpoints, comma separated: grpc://host:port (empty host = all interfaces) and/or unix:///path.sock for same-node callers.", Value: fmt.Sprintf("0.0.0.0:%d", remote.DefaultAgentPort), Destination: &listenEndpoints, EnvVars: []string{"LISTEN_SERVER_ENDPOINT"}},
		&cli.DurationFlag{Name: "gc-interval", Usage: "Orphaned session sweep interval.", Value: time.Minute, Destination: &cfg.GCInterval, EnvVars: []string{"GC_INTERVAL"}},
	}, kube.Flags()...)
	flags = append(flags, FeatureGateFlags(featureGate)...)
	flags = append(flags, loggingConfig.Flags()...)

	app := &cli.App{
		Name:  "remote-agent",
		Usage: "GPU-node agent for remote GPU sessions (runs alongside lupine-server)",
		Flags: flags,
		Before: func(c *cli.Context) error {
			// Apply the logging config before anything logs.
			return loggingConfig.Apply()
		},
		Action: func(c *cli.Context) error {
			if util.PathIsNotExist(cfg.ContainerManagerDir) {
				return fmt.Errorf("container-manager-dir %q does not exist", cfg.ContainerManagerDir)
			}
			endpoint, err := remote.ParseServerEndpoint(cfg.ServerEndpoint)
			if err != nil {
				return fmt.Errorf("invalid --remote-server-endpoint: %w", err)
			}
			// The probed lupine-server runs in the same pod by default.
			if endpoint.Host == "" {
				endpoint.Host = "127.0.0.1"
			}
			cfg.ServerEndpoint = endpoint.String()

			if cfg.AdvertiseEndpoint != "" {
				advertise, err := remote.ParseServerEndpoint(cfg.AdvertiseEndpoint)
				if err != nil {
					return fmt.Errorf("invalid --advertise-server-endpoint: %w", err)
				}
				if advertise.IsLoopback() {
					return fmt.Errorf("invalid --advertise-server-endpoint %q: the host must be one other nodes can reach", cfg.AdvertiseEndpoint)
				}
				cfg.AdvertiseEndpoint = advertise.String()
			}

			cfg.ListenEndpoints = nil
			for _, raw := range strings.Split(listenEndpoints, ",") {
				if strings.TrimSpace(raw) == "" {
					continue
				}
				listen, err := remote.ParseAgentEndpoint(raw)
				if err != nil {
					return fmt.Errorf("invalid --listen-server-endpoint: %w", err)
				}
				cfg.ListenEndpoints = append(cfg.ListenEndpoints, listen.String())
			}
			if len(cfg.ListenEndpoints) == 0 {
				return fmt.Errorf("invalid --listen-server-endpoint %q: empty", listenEndpoints)
			}

			cfg.FeatureGate = featureGate
			if cfg.ReadyFile == "" {
				cfg.ReadyFile = cfg.SessionBase + "/.agent-ready"
			}
			clientSets, err := kube.NewClientSets()
			if err != nil {
				return fmt.Errorf("create client sets: %w", err)
			}
			cfg.ClientSets = clientSets

			ctx, cancel := signal.NotifyContext(c.Context, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
			defer cancel()

			return remoteagent.New(cfg).Run(ctx)
		},
		After: func(c *cli.Context) error {
			// Runs after `Action` (regardless of success/error). In urfave cli
			// v2, the final error reported will be from either Action, Before,
			// or After (whichever is non-nil and last executed).
			klog.Infof("shutdown")
			logs.FlushLogs()
			return nil
		},
		Version: version.Get().String(),
	}

	// Remove the -v alias of the version flag: -v belongs to klog verbosity
	// (same as cmd/kubelet-plugin).
	if f, ok := cli.VersionFlag.(*cli.BoolFlag); ok {
		f.Aliases = nil
	}

	if err := app.RunContext(context.Background(), os.Args); err != nil {
		klog.Errorf("%v", err)
		os.Exit(1)
	}
}
