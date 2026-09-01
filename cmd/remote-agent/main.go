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
	endpointutil "github.com/coldzerofear/vgpu-manager/pkg/util/endpoint"
	"github.com/spf13/pflag"
	"github.com/urfave/cli/v2"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apiserver/pkg/util/compatibility"
	"k8s.io/component-base/featuregate"
	"k8s.io/klog/v2"
	pkgflags "sigs.k8s.io/dra-driver-nvidia-gpu/pkg/flags"
)

const (
	Component = "remoteAgent"
	// GPUCoreResourcePlugin feature gate will report the virtual cores of the node device to kubelet.
	GPUCoreResourcePlugin featuregate.Feature = util.GPUCoreResourcePlugin
	// GPUMemoryResourcePlugin feature gate will report the virtual memory of the node device to kubelet.
	GPUMemoryResourcePlugin featuregate.Feature = util.GPUMemoryResourcePlugin
	// AllocationFailureReschedule feature gate will attempt to reschedule Pods that meet the criteria.
	AllocationFailureReschedule featuregate.Feature = util.AllocationFailureReschedule
	// TopologyAwareGPUAllocation feature gate will report gpu topology information to node.
	TopologyAwareGPUAllocation featuregate.Feature = util.TopologyAwareGPUAllocation
	// SharedSMUtilizationWatcher feature gate will initiate an independent utilization observation thread to share the results with the vGPU Pod node, reducing driver call consumption.
	SharedSMUtilizationWatcher featuregate.Feature = util.SharedSMUtilizationWatcher
	// VirtualMemoryTracking feature gate will track the allocation of virtual memory on devices and provide more precise virtual memory limitations.
	VirtualMemoryTracking featuregate.Feature = util.VirtualMemoryTracking
	// DevicePluginClientMode feature gate will vGPU container to communicate and register devices using Unix sockets and managers, providing stronger security.
	DevicePluginClientMode featuregate.Feature = util.DevicePluginClientMode
	// HonorPreAllocatedDeviceIDs makes preferred allocation follow pre-allocated device IDs whenever possible.
	HonorPreAllocatedDeviceIDs featuregate.Feature = util.HonorPreAllocatedDeviceIDs
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
		kube        pkgflags.KubeClientConfig
		cfg         remoteagent.Config
		featureGate = featuregate.NewFeatureGate()
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
		&cli.StringFlag{Name: "config-session-base", Usage: "Session directory root shared with lupine-server (VGPU_CONFIG_SESSION_BASE).", Value: util.RemoteSessionBasePath, Destination: &cfg.SessionBase, EnvVars: []string{"VGPU_CONFIG_SESSION_BASE"}},
		&cli.StringFlag{Name: "remote-server-endpoint", Usage: "Lupine remote service endpoint.", Value: fmt.Sprintf("127.0.0.1:%d", remote.DefaultServerPort), Destination: &cfg.ServerEndpoint, EnvVars: []string{"REMOTE_SERVER_ENDPOINT"}},
		&cli.StringFlag{Name: "listen-server-endpoint", Usage: "Agent grpc service listening endpoint.", Value: fmt.Sprintf("0.0.0.0:%d", remote.DefaultAgentPort), Destination: &cfg.ListenEndpoint, EnvVars: []string{"LISTEN_SERVER_ENDPOINT"}},
		&cli.DurationFlag{Name: "gc-interval", Usage: "Orphaned session sweep interval.", Value: time.Minute, Destination: &cfg.GCInterval, EnvVars: []string{"GC_INTERVAL"}},
	}, kube.Flags()...)
	flags = append(flags, FeatureGateFlags(featureGate)...)

	app := &cli.App{
		Name:  "remote-agent",
		Usage: "GPU-node agent for remote GPU sessions (runs alongside lupine-server)",
		Flags: flags,
		Action: func(c *cli.Context) error {
			endpoint, err := endpointutil.ParseEndpoint(cfg.ServerEndpoint)
			if err != nil {
				return fmt.Errorf("parse remote endpoint failed: %w", err)
			}
			if endpoint.Host == "" {
				endpoint.Host = "127.0.0.1"
			}
			cfg.ServerEndpoint = endpoint.String()

			endpoint, err = endpointutil.ParseEndpoint(cfg.ListenEndpoint)
			if err != nil {
				return fmt.Errorf("parse listen endpoint failed: %w", err)
			}
			if endpoint.Host == "" {
				endpoint.Host = "0.0.0.0"
			}
			cfg.ListenEndpoint = endpoint.String()

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
	}

	if err := app.RunContext(context.Background(), os.Args); err != nil {
		klog.Errorf("%v", err)
		os.Exit(1)
	}
}
