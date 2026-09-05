/*
Copyright The Kubernetes Authors
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

package kubeletplugin

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/coldzerofear/vgpu-manager/pkg/util"
	pkgflags "sigs.k8s.io/dra-driver-nvidia-gpu/pkg/flags"
)

type Flags struct {
	KubeClientConfig              pkgflags.KubeClientConfig
	HttpEndpoint                  string
	MetricsPath                   string
	NodeName                      string
	CdiRoot                       string
	ContainerDriverRoot           string
	HostDriverRoot                string
	HostRoot                      string
	NvidiaCDIHookPath             string
	KubeletRegistrarDirectoryPath string
	KubeletPluginsDirectoryPath   string
	HealthcheckPort               int
	KlogVerbosity                 int
	AdditionalXidsToIgnore        string
	ConsumableShares              string
	HostManagerDir                string
	ContainerManagerDir           string
	CGroupDriver                  string
	DeviceCoresRatio              uint
	DeviceMemoryRatio             uint
	// NRIRoot is the directory (mounted from the host) that holds the runtime
	// NRI socket. The in-process NRI plugin dials <NRIRoot>/nri.sock. Only used
	// when the NRISupport feature gate is enabled.
	NRIRoot      string
	NRIPluginIdx string
	// PluginMode selects the plugin role (design D21, v1.7): "server" (GPU node;
	// local DRA duties, plus remote-pool duties when RemoteGPUSupport is
	// enabled) or "inject" (consumer node; remote env/CDI injection only, no
	// GPU dependency).
	PluginMode string
	// RemoteAgentEndpoint is how this (server-mode) plugin reaches the
	// remote-agent on its own node: grpc://host:port (empty host = the node's
	// InternalIP -- the agent is on hostNetwork, this plugin is not)
	// or unix:///path. Everything published about the remote path -- the
	// server's and the agent's routable endpoints, the server CUDA version
	// -- is what the agent reports (design D26).
	RemoteAgentEndpoint string
	// RemoteNodeSelector is a label-selector expression over nodes that can
	// reach this GPU node's lupine-server (e.g.
	// "topology.kubernetes.io/zone=az1,gpu-fabric=rdma-a"). The pool becomes
	// schedulable on the GPU node itself OR any node matching it. Required in
	// server mode when RemoteGPUSupport is on.
	RemoteNodeSelector string
}

const (
	ModeServer = "server"
	ModeInject = "inject"
)

type Config struct {
	*Flags
	pkgflags.ClientSets
}

func (c Config) DriverPluginPath() string {
	return filepath.Join(c.Flags.KubeletPluginsDirectoryPath, util.DRADriverName)
}

// change to config
// If 'f.nvidiaCDIHookPath' is already set (from the command line), do nothing.
// If 'f.nvidiaCDIHookPath' is empty, it copies the nvidia-cdi-hook binary from
// /usr/bin/nvidia-cdi-hook to DriverPluginPath and sets 'f.nvidiaCDIHookPath'
// to this path. The /usr/bin/nvidia-cdi-hook is present in the current
// container image because it is copied from the toolkit image into this
// container at build time.
func (c Config) SetNvidiaCDIHookPath() error {
	if c.Flags.NvidiaCDIHookPath != "" {
		return nil
	}

	sourcePath := "/usr/bin/nvidia-cdi-hook"
	targetPath := filepath.Join(c.DriverPluginPath(), "nvidia-cdi-hook")

	input, err := os.ReadFile(sourcePath)
	if err != nil {
		return fmt.Errorf("error reading nvidia-cdi-hook: %w", err)
	}

	if err := os.WriteFile(targetPath, input, 0755); err != nil {
		return fmt.Errorf("error copying nvidia-cdi-hook: %w", err)
	}

	c.Flags.NvidiaCDIHookPath = targetPath

	return nil
}
