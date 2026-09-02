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

// Package remote implements the k8s control-plane side of the remote GPU
// (lupine-backed) data path: the `--mode=inject` consumer-node driver, and the
// shared vocabulary (device attributes, node labels, env names, artifact
// layout) that the server-side publisher must agree on. Design reference:
// docs/remote_gpu_k8s_integration_design.md (v2.0 model: no separate remote
// pool — the node's existing devices carry an accessMode attribute and the
// pool's node scope is widened to the reachability zone).
//
// This package must not import pkg/kubeletplugin: the server-side publisher
// imports this package from there, and a reverse edge would be a cycle.
package remote

import "github.com/coldzerofear/vgpu-manager/pkg/util"

const (
	// Device attributes are published unqualified, i.e. under the driver's
	// own domain, like every other attribute of this driver; CEL reads them
	// as device.attributes["manager.nvidia.com"].<name>.

	// AttrAccessMode is always published: AccessModeLocal on nodes without
	// the RemoteGPUSupport gate (pool scoped to the node, prepared by the
	// local path — today's behaviour); AccessModeRemote on nodes with it
	// (pool scoped to the reachability selector, and EVERY consumer of the
	// device goes through lupine — including pods scheduled onto the GPU
	// node itself, design v2.1 D23). One device carries exactly one value.
	AttrAccessMode   = util.AccessModeAttribute
	AccessModeLocal  = util.AccessModeLocal
	AccessModeRemote = util.AccessModeRemote

	// AttrServerEndpoint is the lupine-server endpoint, verbatim (IP or domain,
	// optional scheme/port, design D3/§6.1). Published only for remote.
	AttrServerEndpoint = "serverEndpoint"
	// AttrAgentEndpoint is the remote agent endpoint, verbatim (IP or domain,
	// optional scheme/port, design D3/§6.1). Published only for remote.
	AttrAgentEndpoint = "agentEndpoint"

	// Capacity names, aligned with the local vgpu share semantics
	// (pkg/kubeletplugin/vgpu.go).
	CapacityCores  = "cores"
	CapacityMemory = "memory"

	// Existing attributes reused by the inject side (pkg/kubeletplugin/deviceinfo.go).
	AttrUUID              = "uuid"
	AttrMinor             = "minor"
	AttrMemoryRatio       = "memoryRatio"
	AttrCUDADriverVersion = "cudaDriverVersion"
	AttrDriverVersion     = "driverVersion"

	// AttrServerCUDAVersion is the CUDA version the node's lupine-server binary
	// was built with, read from its x-lupine-cuda-version header. Published only
	// after the server has answered. The driver ceiling above can be higher than
	// this, and the client artifact must stay below the lower of the two.
	AttrServerCUDAVersion = "serverCudaVersion"

	// DefaultServerPort is lupine-server's default listen port
	// (docs/lupine_env_reference.md, LUPINE_PORT).
	DefaultServerPort = 14833

	// The lupine client injection triplet (docs/lupine_env_reference.md §5).
	// LUPINE_DISABLE_LOCAL is mandatory: without it a client with a local GPU
	// routes device 0 locally and never reaches the server.
	EnvLupineServer       = "LUPINE_SERVER"
	EnvLupineSession      = "LUPINE_SESSION"
	EnvLupineDisableLocal = "LUPINE_DISABLE_LOCAL"

	// CDI vendor/class must match the values used by the local plugin
	// (pkg/kubeletplugin/cdi.go) so that all claim devices of this driver live
	// under one naming convention.
	cdiVendor     = "k8s." + util.DRADriverName
	cdiClaimClass = "claim"

	RemoteClientConf = "remote-client-library.conf"
)
