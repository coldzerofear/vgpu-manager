/*
 * Copyright (c) 2022-2023 NVIDIA CORPORATION.  All rights reserved.
 * Copyright 2025-2026 coldzerofear
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"

	pkgkubeletplugin "github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin"
	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin/common"
	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin/featuregates"
	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin/nri"
	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin/remote"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/coldzerofear/vgpu-manager/pkg/version"
	"github.com/urfave/cli/v2"
	"k8s.io/component-base/logs"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"
	pkgflags "sigs.k8s.io/dra-driver-nvidia-gpu/pkg/flags"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/metrics"
)

func main() {
	if err := newApp().Run(os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func newApp() *cli.App {
	// init FeatureGates
	_ = featuregates.FeatureGates()
	loggingConfig := pkgflags.NewLoggingConfig()
	featureGateConfig := pkgflags.NewFeatureGateConfig()
	flags := &pkgkubeletplugin.Flags{}

	cliFlags := []cli.Flag{
		&cli.StringFlag{
			Name:        "node-name",
			Usage:       "The name of the node to be worked on.",
			Required:    true,
			Destination: &flags.NodeName,
			EnvVars:     []string{"NODE_NAME"},
		},
		&cli.StringFlag{
			Name:        "cdi-root",
			Usage:       "Absolute path to the directory where CDI files will be generated.",
			Value:       "/etc/cdi",
			Destination: &flags.CdiRoot,
			EnvVars:     []string{"CDI_ROOT"},
		},
		&cli.StringFlag{
			Name:        "nvidia-driver-root",
			Aliases:     []string{"host_driver-root"},
			Value:       "/",
			Usage:       "the root path for the NVIDIA driver installation on the host (typical values are '/' or '/run/nvidia/driver').",
			Destination: &flags.HostDriverRoot,
			EnvVars:     []string{"NVIDIA_DRIVER_ROOT", "HOST_DRIVER_ROOT"},
		},
		&cli.StringFlag{
			Name:        "container-driver-root",
			Value:       "/driver-root",
			Usage:       "the path where the NVIDIA driver root is mounted in the container; used for generating CDI specifications.",
			Destination: &flags.ContainerDriverRoot,
			EnvVars:     []string{"DRIVER_ROOT_CTR_PATH"},
		},
		&cli.StringFlag{
			Name:        "host-root",
			Value:       "/host-root",
			Destination: &flags.HostRoot,
			EnvVars:     []string{"HOST_ROOT"},
			Usage:       "the path where the root path of the host file system is mounted in the container (required when PassthroughSupport feature gate is enabled)",
		},
		&cli.StringFlag{
			Name:        "nvidia-cdi-hook-path",
			Usage:       "Absolute path to the nvidia-cdi-hook executable in the host file system. Used in the generated CDI specification.",
			Destination: &flags.NvidiaCDIHookPath,
			EnvVars:     []string{"NVIDIA_CDI_HOOK_PATH"},
		},
		&cli.StringFlag{
			Name:        "kubelet-registrar-directory-path",
			Usage:       "Absolute path to the directory where kubelet stores plugin registrations.",
			Value:       kubeletplugin.KubeletRegistryDir,
			Destination: &flags.KubeletRegistrarDirectoryPath,
			EnvVars:     []string{"KUBELET_REGISTRAR_DIRECTORY_PATH"},
		},
		&cli.StringFlag{
			Name:        "kubelet-plugins-directory-path",
			Usage:       "Absolute path to the directory where kubelet stores plugin data.",
			Value:       kubeletplugin.KubeletPluginsDir,
			Destination: &flags.KubeletPluginsDirectoryPath,
			EnvVars:     []string{"KUBELET_PLUGINS_DIRECTORY_PATH"},
		},
		&cli.IntFlag{
			Name:        "healthcheck-port",
			Usage:       "Port to start a gRPC healthcheck service. When positive, a literal port number. When zero, a random port is allocated. When negative, the healthcheck service is disabled.",
			Value:       -1,
			Destination: &flags.HealthcheckPort,
			EnvVars:     []string{"HEALTHCHECK_PORT"},
		},
		// TODO: change to StringSliceFlag.
		&cli.StringFlag{
			Name:        "additional-xids-to-ignore",
			Usage:       "A comma-separated list of additional XIDs to ignore.",
			Value:       "",
			Destination: &flags.AdditionalXidsToIgnore,
			EnvVars:     []string{"ADDITIONAL_XIDS_TO_IGNORE"},
		},
		&cli.StringFlag{
			Name:        "http-endpoint",
			Usage:       "The TCP network `address` where the metrics HTTP server will listen (example: `:8080`). The default is the empty string, which means the server is disabled.",
			Destination: &flags.HttpEndpoint,
			EnvVars:     []string{"HTTP_ENDPOINT"},
		},
		&cli.StringFlag{
			Name:        "metrics-path",
			Usage:       "The HTTP `path` where Prometheus metrics are exposed, disabled if empty.",
			Value:       "/metrics",
			Destination: &flags.MetricsPath,
			EnvVars:     []string{"METRICS_PATH"},
		},
		&cli.StringFlag{
			Name:        "consumable-shares",
			Usage:       "Configure consumable shares support ('disabled', 'memory', 'unlimited', or a positive integer).",
			Value:       "disabled",
			Destination: &flags.ConsumableShares,
			EnvVars:     []string{"CONSUMABLE_SHARES"},
		},
		&cli.StringFlag{
			Name:        "host-manager-dir",
			Usage:       "Configure the host path used by vgpu-manager.",
			Value:       util.ManagerRootPath,
			Required:    true,
			Destination: &flags.HostManagerDir,
			EnvVars:     []string{"HOST_MANAGER_DIR"},
		},
		&cli.StringFlag{
			Name:        "container-manager-dir",
			Usage:       "Configure the container mount path used by vgpu-manager.",
			Value:       util.ManagerRootPath,
			Required:    true,
			Destination: &flags.ContainerManagerDir,
			EnvVars:     []string{"CONTAINER_MANAGER_DIR"},
		},
		&cli.StringFlag{
			Name:        "cgroup-driver",
			Usage:       "Configure the cgroup driver for the current container environment.",
			Value:       "",
			Destination: &flags.CGroupDriver,
			EnvVars:     []string{"CGROUP_DRIVER"},
		},
		&cli.UintFlag{
			Name:        "device-cores-ratio",
			Usage:       "Nvidia vGPU device cores oversold ratio",
			Value:       100,
			Destination: &flags.DeviceCoresRatio,
			EnvVars:     []string{"DEVICE_CORES_RATIO"},
		},
		&cli.UintFlag{
			Name:        "device-memory-ratio",
			Usage:       "Nvidia vGPU device memory oversold ratio",
			Value:       100,
			Destination: &flags.DeviceMemoryRatio,
			EnvVars:     []string{"DEVICE_MEMORY_RATIO"},
		},
		&cli.StringFlag{
			Name:        "nri-root",
			Usage:       "Directory (mounted from the host) holding the runtime NRI socket; the in-process NRI plugin dials <nri-root>/nri.sock. Only used when the NRISupport feature gate is enabled.",
			Value:       "/var/run/nri",
			Destination: &flags.NRIRoot,
			EnvVars:     []string{"NRI_ROOT"},
		},
		&cli.StringFlag{
			Name:        "nri-plugin-idx",
			Usage:       "Configure nri plugin idx to ensure plugin execution order.",
			Value:       "00",
			Destination: &flags.NRIPluginIdx,
			EnvVars:     []string{"NRI_PLUGIN_IDX"},
		},
		&cli.StringFlag{
			Name:        "plugin-mode",
			Usage:       "Plugin role: 'server' (GPU node) or 'inject' (consumer node, remote GPU injection only; requires the RemoteGPUSupport feature gate).",
			Value:       pkgkubeletplugin.ModeServer,
			Destination: &flags.PluginMode,
			EnvVars:     []string{"PLUGIN_MODE"},
		},
		&cli.IntFlag{
			Name:        "remote-agent-port",
			Usage:       "remote-agent gRPC port on server hosts; inject mode calls EnsureSession on <endpoint host>:<port>.",
			Value:       remote.DefaultAgentPort,
			Destination: &flags.RemoteAgentPort,
			EnvVars:     []string{"REMOTE_AGENT_PORT"},
		},
		&cli.StringFlag{
			Name:        "remote-endpoint",
			Usage:       "lupine-server endpoint published on this node's devices when RemoteGPUSupport is enabled (server mode). Empty derives <node InternalIP>:14833.",
			Destination: &flags.RemoteEndpoint,
			EnvVars:     []string{"REMOTE_ENDPOINT"},
		},
		&cli.StringFlag{
			Name:        "remote-node-selector",
			Usage:       "Label selector over nodes that can reach this node's lupine-server, e.g. 'topology.kubernetes.io/zone=az1,gpu-fabric in (a,b)'. The pool becomes schedulable on this node OR matching nodes (server mode, RemoteGPUSupport).",
			Destination: &flags.RemoteNodeSelector,
			EnvVars:     []string{"REMOTE_NODE_SELECTOR"},
		},
	}
	cliFlags = append(cliFlags, flags.KubeClientConfig.Flags()...)
	cliFlags = append(cliFlags, featureGateConfig.Flags()...)
	cliFlags = append(cliFlags, loggingConfig.Flags()...)

	app := &cli.App{
		Name:            "vgpu-manager-kubelet-plugin",
		Usage:           "vgpu-manager-kubelet-plugin implements a DRA driver plugin for NVIDIA GPUs.",
		ArgsUsage:       " ",
		HideHelpCommand: true,
		Flags:           cliFlags,
		Before: func(c *cli.Context) error {
			if c.Args().Len() > 0 {
				return fmt.Errorf("arguments not supported: %v", c.Args().Slice())
			}
			// `loggingConfig` must be applied before doing any logging
			err := loggingConfig.Apply()

			// Store klog's log verbosity setting in this program's config for
			// later runtime inspection (it's otherwise not accessible anymore
			// because we do not expose the raw `cliFlags`.
			flags.KlogVerbosity = int(loggingConfig.Config.Verbosity)
			pkgflags.LogStartupConfig(flags, loggingConfig)
			return err
		},
		Action: func(c *cli.Context) error {
			if err := featuregates.ValidateFeatureGates(); err != nil {
				return fmt.Errorf("feature gate validation failed: %w", err)
			}

			if err := validateCLIFlags(flags); err != nil {
				return fmt.Errorf("invalid CLI flags: %w", err)
			}

			clientSets, err := flags.KubeClientConfig.NewClientSets()
			if err != nil {
				return fmt.Errorf("create client: %w", err)
			}

			config := &pkgkubeletplugin.Config{
				Flags:      flags,
				ClientSets: clientSets,
			}

			if flags.PluginMode == pkgkubeletplugin.ModeInject {
				return RunInjectPlugin(c.Context, config)
			}
			return RunPlugin(c.Context, config)
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

	// We remove the -v alias for the version flag so as to not conflict with the -v flag used for klog.
	f, ok := cli.VersionFlag.(*cli.BoolFlag)
	if ok {
		f.Aliases = nil
	}

	return app
}

// Input validation of CLI flags.
func validateCLIFlags(flags *pkgkubeletplugin.Flags) error {
	switch flags.PluginMode {
	// Empty means the caller did not go through the CLI (e.g. tests); treat
	// it as the server default.
	case "", pkgkubeletplugin.ModeServer:
		if featuregates.Enabled(featuregates.RemoteGPUSupport) {
			if flags.RemoteNodeSelector == "" {
				return fmt.Errorf("--remote-node-selector is required in server mode when feature gate %s is enabled",
					featuregates.RemoteGPUSupport)
			}
			if _, err := remote.ParseNodeSelector(flags.RemoteNodeSelector); err != nil {
				return err
			}
			if flags.HttpEndpoint != "" {
				return fmt.Errorf("when the feature gate %s is enabled and the --plugin-mode=%s, --http-endpoint cannot be set",
					featuregates.RemoteGPUSupport, pkgkubeletplugin.ModeServer)
			}
			if flags.HealthcheckPort >= 0 {
				return fmt.Errorf("when the feature gate %s is enabled and the --plugin-mode=%s, --healthcheck-port cannot be set",
					featuregates.RemoteGPUSupport, pkgkubeletplugin.ModeServer)
			}
		}
	case pkgkubeletplugin.ModeInject:
		if !featuregates.Enabled(featuregates.RemoteGPUSupport) {
			return fmt.Errorf("--plugin-mode=%s requires feature gate %s to be enabled",
				pkgkubeletplugin.ModeInject, featuregates.RemoteGPUSupport)
		} else {
			if featuregates.Enabled(featuregates.NVMLDeviceHealthCheck) {
				return fmt.Errorf("--plugin-mode=%s requires feature gate %s to be disabled",
					pkgkubeletplugin.ModeInject, featuregates.NVMLDeviceHealthCheck)
			}
		}
	default:
		return fmt.Errorf("invalid --plugin-mode %q: must be %q or %q", flags.PluginMode,
			pkgkubeletplugin.ModeServer, pkgkubeletplugin.ModeInject)
	}

	// Validate the NRI plugin index format early (only when set): containerd
	// otherwise rejects a bad index at registration time, which surfaces as an
	// obscure reconnect-loop failure rather than a clear startup error.
	if featuregates.Enabled(featuregates.NRISupport) {
		if flags.NRIPluginIdx != "" {
			if err := nri.ValidatePluginIdx(flags.NRIPluginIdx); err != nil {
				return fmt.Errorf("invalid --nri-plugin-idx %q: %w", flags.NRIPluginIdx, err)
			}
		}
	}

	if featuregates.Enabled(featuregates.VGPUSupport) {
		if flags.DeviceCoresRatio <= 0 {
			return fmt.Errorf("invalid --device-cores-ratio %d: must be greater than or equal to 0", flags.DeviceCoresRatio)
		}
		if flags.DeviceMemoryRatio <= 0 {
			return fmt.Errorf("invalid --device-memory-ratio %d: must be greater than or equal to 0", flags.DeviceCoresRatio)
		}
	}

	if featuregates.Enabled(featuregates.PassthroughSupport) {
		if flags.HostRoot == "" {
			return fmt.Errorf("host root is required when PassthroughSupport feature gate is enabled")
		}
		// Host root FS must be mounted in the container for passthrough support to work.
		// vsekar: This requirement is for being able to run `modprobe` to load the vfio driver.
		// This mount would also be a duplicate if the nvidia driver is installed to the host rootFS.
		// TODO: Reduce scope of the host mounts for least access.
		if _, err := os.Stat(flags.HostRoot); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("host root is not mounted at %q", flags.HostRoot)
			}
			return fmt.Errorf("error checking if host root is mounted at %q: %w", flags.HostRoot, err)
		}
	}

	if flags.ConsumableShares != "disabled" && !featuregates.Enabled(featuregates.ConsumableShares) {
		return fmt.Errorf("--consumable-shares requires feature gate %s to be enabled", featuregates.ConsumableShares)
	}

	if flags.ConsumableShares != "disabled" && flags.ConsumableShares != "memory" && flags.ConsumableShares != "unlimited" {
		val, err := strconv.Atoi(flags.ConsumableShares)
		if err != nil || val <= 0 {
			return fmt.Errorf("invalid value for --consumable-shares: %q (must be 'disabled', 'memory', 'unlimited', or a positive integer)", flags.ConsumableShares)
		}
	}

	return nil
}

// RunInjectPlugin initializes and runs the consumer-node (no GPU) inject
// plugin for remote GPU claims (--plugin-mode=inject, design D21/§2.3). It shares
// the driver name and kubelet plugin directories with server mode, but skips
// everything that needs a GPU or the NVIDIA driver (NVML enumeration, health
// checks, nvidia-cdi-hook, NRI).
func RunInjectPlugin(ctx context.Context, config *pkgkubeletplugin.Config) error {
	common.StartDebugSignalHandlers()

	if err := os.MkdirAll(config.DriverPluginPath(), 0750); err != nil {
		return err
	}

	// Initialize CDI root directory
	info, err := os.Stat(config.Flags.CdiRoot)
	switch {
	case err != nil && os.IsNotExist(err):
		err := os.MkdirAll(config.Flags.CdiRoot, 0750)
		if err != nil {
			return err
		}
	case err != nil:
		return err
	case !info.IsDir():
		return fmt.Errorf("path for cdi file generation is not a directory: '%v'", config.Flags.CdiRoot)
	}

	ctx, cancel := signal.NotifyContext(ctx, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	defer cancel()

	metrics.InitializeDRARequestMetrics(util.DRADriverName)
	if config.Flags.HttpEndpoint != "" {
		if err := metrics.RunPrometheusMetricsServer(ctx, config.Flags.HttpEndpoint, config.Flags.MetricsPath); err != nil {
			return fmt.Errorf("setup metrics endpoint: %w", err)
		}
	}

	// Client artifacts live under <manager-dir>/driver/<cuda-ver>/ — the same
	// driver directory the local path uses for libvgpu-control.so.<ver>. The
	// plugin reads it through its own mount (container path) but the CDI
	// mount it emits must name the host path.
	driver, err := remote.NewInjectDriver(ctx, remote.InjectConfig{
		HealthcheckPort:               config.Flags.HealthcheckPort,
		NodeName:                      config.Flags.NodeName,
		CdiRoot:                       config.Flags.CdiRoot,
		KubeletRegistrarDirectoryPath: config.Flags.KubeletRegistrarDirectoryPath,
		PluginDataDirectoryPath:       config.DriverPluginPath(),
		ArtifactsDir:                  filepath.Join(config.Flags.ContainerManagerDir, util.Driver),
		HostArtifactsDir:              filepath.Join(config.Flags.HostManagerDir, util.Driver),
		AgentPort:                     config.Flags.RemoteAgentPort,
		NRIRoot:                       config.Flags.NRIRoot,
		NRIPluginIdx:                  config.Flags.NRIPluginIdx,
	}, config.ClientSets)
	if err != nil {
		return fmt.Errorf("error creating inject driver: %w", err)
	}

	<-ctx.Done()
	if err := ctx.Err(); err != nil && !errors.Is(err, context.Canceled) {
		klog.Errorf("error from context: %v", err)
	}

	if err := driver.Shutdown(); err != nil {
		klog.Errorf("unable to cleanly shutdown inject driver: %v", err)
	}
	return nil
}

// RunPlugin initializes and runs the GPU kubelet plugin.
func RunPlugin(ctx context.Context, config *pkgkubeletplugin.Config) error {
	common.StartDebugSignalHandlers()

	// Create the plugin directory
	if err := os.MkdirAll(config.DriverPluginPath(), 0750); err != nil {
		return err
	}

	// Setup nvidia-cdi-hook binary
	if err := config.SetNvidiaCDIHookPath(); err != nil {
		return fmt.Errorf("error setting up nvidia-cdi-hook: %w", err)
	}

	// Initialize CDI root directory
	info, err := os.Stat(config.Flags.CdiRoot)
	switch {
	case err != nil && os.IsNotExist(err):
		if err = os.MkdirAll(config.Flags.CdiRoot, 0750); err != nil {
			return err
		}
	case err != nil:
		return err
	case !info.IsDir():
		return fmt.Errorf("path for cdi file generation is not a directory: '%v'", config.Flags.CdiRoot)
	}

	ctx, cancel := signal.NotifyContext(ctx, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	defer cancel()

	metrics.InitializeDRARequestMetrics(util.DRADriverName)

	if config.Flags.HttpEndpoint != "" {
		if err := metrics.RunPrometheusMetricsServer(ctx, config.Flags.HttpEndpoint, config.Flags.MetricsPath); err != nil {
			return fmt.Errorf("setup metrics endpoint: %w", err)
		}
	}

	// Create and start the driver
	driver, err := pkgkubeletplugin.NewDriver(ctx, config)
	if err != nil {
		return fmt.Errorf("error creating driver: %w", err)
	}

	<-ctx.Done()
	if err := ctx.Err(); err != nil && !errors.Is(err, context.Canceled) {
		// A canceled context is the normal case here when the process receives
		// a signal. Only log the error for more interesting cases.
		klog.Errorf("error from context: %v", err)
	}

	if err = driver.Shutdown(); err != nil {
		klog.Errorf("unable to cleanly shutdown driver: %v", err)
	}

	return nil
}
