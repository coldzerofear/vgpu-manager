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

package remote

import (
	"context"
	"fmt"
	"strings"

	"github.com/Masterminds/semver"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"
	pkgflags "sigs.k8s.io/dra-driver-nvidia-gpu/pkg/flags"
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdispec "tags.cncf.io/container-device-interface/specs-go"
)

// InjectConfig carries the subset of plugin configuration the remote
// injection path needs. It intentionally does not reference
// pkg/kubeletplugin types (see the package comment for the dependency
// direction).
type InjectConfig struct {
	NodeName                      string
	CdiRoot                       string
	KubeletRegistrarDirectoryPath string
	// PluginDataDirectoryPath is the per-driver data directory
	// (<kubelet-plugins-dir>/<driver-name>), already created by the caller.
	PluginDataDirectoryPath string
	// ArtifactsDir is the node-level lupine client version directory
	// (design D12).
	ArtifactsDir string
	// ClientMountPath is the in-container mount root for artifacts.
	ClientMountPath string
	// AgentPort is the remote-agent gRPC port on server hosts.
	AgentPort int
}

// Injector turns allocations of accessMode=remote devices into env/mount CDI
// injections plus the EnsureSession barrier (design §2.3, D2). It is the
// single remote prepare implementation, used by both roles:
//
//   - `--mode=inject` on consumer nodes (InjectDriver below);
//   - `--mode=server` on GPU nodes with RemoteGPUSupport on, where the local
//     DRA driver delegates every prepare to it — a GPU node's own devices are
//     consumed through lupine even by pods scheduled onto that node (design
//     v2.1, D23): a claim may mix this node's devices with another node's,
//     and the local (LD_PRELOAD real driver) and remote (lupine shim,
//     LUPINE_DISABLE_LOCAL) paths cannot coexist in one container.
//
// K1 scope: the session token is derived from the claim UID (D8 random token
// pending).
type Injector struct {
	config  InjectConfig
	clients pkgflags.ClientSets
	cdi     *cdiWriter
}

func NewInjector(config InjectConfig, clients pkgflags.ClientSets) *Injector {
	return &Injector{config: config, clients: clients, cdi: newCDIWriter(config.CdiRoot)}
}

// Prepare resolves, ensures sessions and writes the CDI spec for one claim.
func (in *Injector) Prepare(ctx context.Context, claim *resourceapi.ResourceClaim) kubeletplugin.PrepareResult {
	fail := func(err error) kubeletplugin.PrepareResult {
		return kubeletplugin.PrepareResult{Err: err}
	}

	if claim.Status.Allocation == nil {
		return fail(fmt.Errorf("claim %s/%s has no allocation", claim.Namespace, claim.Name))
	}
	var allocated []resourceapi.DeviceRequestAllocationResult
	for _, result := range claim.Status.Allocation.Devices.Results {
		if result.Driver == util.DRADriverName {
			allocated = append(allocated, result)
		}
	}
	if len(allocated) == 0 {
		return fail(fmt.Errorf("claim %s/%s has no devices allocated by %s", claim.Namespace, claim.Name, util.DRADriverName))
	}

	deviceIndex, err := in.indexSlices(ctx)
	if err != nil {
		return fail(err)
	}
	endpoints, minCUDA, err := resolveRemoteAllocation(allocated, deviceIndex)
	if err != nil {
		return fail(err)
	}

	// Multiple servers with differing CUDA versions: the client artifact must
	// satisfy client <= server for every server, hence the minimum (§4.3).
	artifact, err := selectArtifact(in.config.ArtifactsDir, in.config.ClientMountPath, minCUDA)
	if err != nil {
		return fail(err)
	}

	// K1: the session token is the claim UID.
	// TODO(D8): mint a random token and write it to
	// claim.status.devices[].data.
	token := string(claim.UID)

	// D2 barrier: every server must have the session on disk before any
	// container of the pod starts. Failure -> kubelet retries with backoff.
	if err := EnsureSessions(ctx, endpoints, in.config.AgentPort, token, claim); err != nil {
		return fail(err)
	}
	klog.V(2).Infof("Remote claim %s/%s: endpoints=%v artifact=%s (server CUDA floor %s)",
		claim.Namespace, claim.Name, endpoints, artifact.Name, minCUDA)

	edits := &cdiapi.ContainerEdits{
		ContainerEdits: &cdispec.ContainerEdits{
			Env: []string{
				fmt.Sprintf("%s=%s", EnvLupineServer, strings.Join(endpoints, ",")),
				fmt.Sprintf("%s=%s", EnvLupineSession, token),
				// Mandatory: prevents client-local routing (§4.3.2 lesson).
				fmt.Sprintf("%s=1", EnvLupineDisableLocal),
				// The artifact dir contains only libcuda.so.1/libnvidia-ml.so.1
				// (self-contained static client, D11), so shadowing is limited
				// to those two names. Note: CDI env entries replace, not
				// extend, an LD_LIBRARY_PATH set by the image.
				fmt.Sprintf("LD_LIBRARY_PATH=%s", artifact.LibDir),
			},
			Mounts: []*cdispec.Mount{
				{
					HostPath:      artifact.HostDir,
					ContainerPath: artifact.ContainerDir,
					Options:       []string{"ro", "nosuid", "nodev", "bind"},
				},
			},
		},
	}

	qualifiedName, err := in.cdi.WriteClaimSpec(string(claim.UID), edits)
	if err != nil {
		return fail(fmt.Errorf("failed to write CDI spec for claim %s/%s: %w", claim.Namespace, claim.Name, err))
	}

	var devices []kubeletplugin.Device
	for _, result := range allocated {
		devices = append(devices, kubeletplugin.Device{
			Requests:     []string{result.Request},
			PoolName:     result.Pool,
			DeviceName:   result.Device,
			CDIDeviceIDs: []string{qualifiedName},
		})
	}
	return kubeletplugin.PrepareResult{Devices: devices}
}

// Unprepare removes the claim's CDI spec (idempotent). Session teardown on
// the GPU node is driven by the agent's claim watch, not by this call.
func (in *Injector) Unprepare(claimUID string) error {
	return in.cdi.DeleteClaimSpec(claimUID)
}

// resolveRemoteAllocation maps every allocation result to a published remote
// device and derives the LUPINE_SERVER endpoint list plus the CUDA-version
// floor across servers. The order of `allocated` comes from the recorded
// allocation and is stable across plugin restarts — the endpoint order
// (= client device ordinals, design §6.8) derives from it deterministically,
// deduplicated to first appearance.
func resolveRemoteAllocation(allocated []resourceapi.DeviceRequestAllocationResult, deviceIndex map[string]map[string]*resourceapi.Device) ([]string, *semver.Version, error) {
	var endpoints []string
	seenEndpoint := map[string]bool{}
	var minCUDA *semver.Version
	for _, result := range allocated {
		dev, ok := deviceIndex[result.Pool][result.Device]
		if !ok {
			return nil, nil, fmt.Errorf("allocated device %s/%s not found in any published ResourceSlice of %s",
				result.Pool, result.Device, util.DRADriverName)
		}
		info, isRemote, err := ParseDevice(dev)
		if err != nil {
			return nil, nil, err
		}
		if !isRemote {
			// A local-only device (accessMode=local) reaching the remote path
			// means the claim mixed devices of a local-only node with remote
			// ones, or the pool publishing is wrong. Fail loudly rather than
			// prepare half a claim.
			return nil, nil, fmt.Errorf("device %s/%s is not %s=%s; the remote path cannot prepare it",
				result.Pool, result.Device, AttrAccessMode, AccessModeRemote)
		}
		if !seenEndpoint[info.Endpoint] {
			seenEndpoint[info.Endpoint] = true
			endpoints = append(endpoints, info.Endpoint)
		}
		if minCUDA == nil || info.CUDAVersion.Compare(minCUDA) < 0 {
			minCUDA = info.CUDAVersion
		}
	}
	return endpoints, minCUDA, nil
}

// indexSlices builds pool -> device -> *Device over all ResourceSlices
// published under our driver name. A plain List per prepare call is deliberate
// for K1 (prepare is rare and the slice set is small); switch to an informer
// if it ever shows up in latency.
func (in *Injector) indexSlices(ctx context.Context) (map[string]map[string]*resourceapi.Device, error) {
	sliceList, err := in.clients.Resource.ResourceSlices().List(ctx, metav1.ListOptions{
		FieldSelector: resourceapi.ResourceSliceSelectorDriver + "=" + util.DRADriverName,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list ResourceSlices: %w", err)
	}
	index := make(map[string]map[string]*resourceapi.Device)
	for i := range sliceList.Items {
		slice := &sliceList.Items[i]
		pool := index[slice.Spec.Pool.Name]
		if pool == nil {
			pool = make(map[string]*resourceapi.Device)
			index[slice.Spec.Pool.Name] = pool
		}
		for j := range slice.Spec.Devices {
			pool[slice.Spec.Devices[j].Name] = &slice.Spec.Devices[j]
		}
	}
	return index, nil
}

// InjectDriver is the `--mode=inject` DRA driver for consumer nodes: no GPU,
// no NVML — it registers with the kubelet and hands every claim to Injector.
type InjectDriver struct {
	*Injector
	helper *kubeletplugin.Helper
}

func NewInjectDriver(ctx context.Context, config InjectConfig, clients pkgflags.ClientSets) (*InjectDriver, error) {
	d := &InjectDriver{Injector: NewInjector(config, clients)}

	helper, err := kubeletplugin.Start(ctx, d,
		kubeletplugin.KubeClient(clients.Core),
		kubeletplugin.NodeName(config.NodeName),
		kubeletplugin.DriverName(util.DRADriverName),
		kubeletplugin.Serialize(false),
		kubeletplugin.RegistrarDirectoryPath(config.KubeletRegistrarDirectoryPath),
		kubeletplugin.PluginDataDirectoryPath(config.PluginDataDirectoryPath),
	)
	if err != nil {
		return nil, err
	}
	d.helper = helper

	klog.V(2).Infof("Remote inject driver started on node %s (registration status: %s)",
		config.NodeName, helper.RegistrationStatus())
	return d, nil
}

func (d *InjectDriver) Shutdown() error {
	if d == nil {
		return nil
	}
	d.helper.Stop()
	return nil
}

func (d *InjectDriver) PrepareResourceClaims(ctx context.Context, claims []*resourceapi.ResourceClaim) (map[types.UID]kubeletplugin.PrepareResult, error) {
	results := make(map[types.UID]kubeletplugin.PrepareResult)
	for _, claim := range claims {
		results[claim.UID] = d.Prepare(ctx, claim)
	}
	return results, nil
}

func (d *InjectDriver) UnprepareResourceClaims(ctx context.Context, claimRefs []kubeletplugin.NamespacedObject) (map[types.UID]error, error) {
	results := make(map[types.UID]error)
	for _, claimRef := range claimRefs {
		results[claimRef.UID] = d.Unprepare(string(claimRef.UID))
	}
	return results, nil
}

func (d *InjectDriver) HandleError(ctx context.Context, err error, msg string) {
	runtime.HandleErrorWithContext(ctx, err, msg)
}

func (d *InjectDriver) WatchHealthStatus(context.Context, chan<- kubeletplugin.DeviceHealthReport) error {
	return kubeletplugin.ErrHealthNotSupported
}
