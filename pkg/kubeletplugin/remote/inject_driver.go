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
	"sync"
	"time"

	"github.com/Masterminds/semver"
	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin/health"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"
	pkgflags "sigs.k8s.io/dra-driver-nvidia-gpu/pkg/flags"
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdispec "tags.cncf.io/container-device-interface/specs-go"
)

// InjectConfig carries the subset of plugin configuration the inject-mode
// driver needs. It intentionally does not reference pkg/kubeletplugin types
// (see the package comment for the dependency direction).
type InjectConfig struct {
	HealthcheckPort               int
	NodeName                      string
	CdiRoot                       string
	KubeletRegistrarDirectoryPath string
	// PluginDataDirectoryPath is the per-driver data directory
	// (<kubelet-plugins-dir>/<driver-name>), already created by the caller.
	PluginDataDirectoryPath string
	// ArtifactsDir is the node-level lupine client version directory
	// (design D12).
	ArtifactsDir string
	// AgentPort is the remote-agent gRPC port on every server host.
	AgentPort int
}

// InjectDriver is the `--mode=inject` DRA driver for consumer nodes: no GPU,
// no NVML — it only translates allocations of accessMode=remote devices into
// env/mount CDI injections (design §2.3). S1 spike scope: the session token is derived from
// the claim UID and EnsureSession is not called; both are replaced in S2
// (design D8/D2/D20).
type InjectDriver struct {
	config       InjectConfig
	helper       *kubeletplugin.Helper
	cdi          *cdiWriter
	wg           sync.WaitGroup
	sliceIndexer cache.Indexer
	healthcheck  *health.Healthcheck
}

func (d *InjectDriver) GetPoolResourceSlices(poolName string) ([]*resourceapi.ResourceSlice, error) {
	objs, err := d.sliceIndexer.ByIndex(resourceapi.ResourceSliceSelectorPoolName, poolName)
	if err != nil {
		return nil, fmt.Errorf("slice by poolName %s failed: %w", poolName, err)
	}
	slices := make([]*resourceapi.ResourceSlice, 0, len(objs))
	for _, obj := range objs {
		if slice, ok := obj.(*resourceapi.ResourceSlice); ok {
			slices = append(slices, slice)
		}
	}
	if len(slices) == 0 {
		return nil, apierrors.NewNotFound(resourceapi.Resource("resourceslices"),
			fmt.Sprintf("%s %s", resourceapi.ResourceSliceSelectorPoolName, poolName))
	}
	return slices, nil
}

func NewInjectDriver(ctx context.Context, config InjectConfig, clients pkgflags.ClientSets) (*InjectDriver, error) {
	d := &InjectDriver{
		config: config,
		cdi:    newCDIWriter(config.CdiRoot),
	}

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

	sliceInformer := cache.NewSharedIndexInformer(cache.NewListWatchFromClient(
		clients.Resource.RESTClient(), "resourceslices", corev1.NamespaceAll,
		fields.OneTermEqualSelector(resourceapi.ResourceSliceSelectorDriver, util.DRADriverName),
	), &resourceapi.ResourceSlice{}, 10*time.Hour, cache.Indexers{
		resourceapi.ResourceSliceSelectorPoolName: func(obj interface{}) ([]string, error) {
			var indexerValues []string
			if slice, ok := obj.(*resourceapi.ResourceSlice); ok {
				indexerValues = []string{slice.Spec.Pool.Name}
			}
			return indexerValues, nil
		},
	})
	d.sliceIndexer = sliceInformer.GetIndexer()

	healthConfig := &health.HealthConfig{
		HealthcheckPort:               config.HealthcheckPort,
		KubeletRegistrarDirectoryPath: config.KubeletRegistrarDirectoryPath,
		KubeletDriverPluginPath:       config.PluginDataDirectoryPath,
	}
	healthcheck, err := health.StartHealthcheck(ctx, healthConfig, helper, nil)
	if err != nil {
		return nil, fmt.Errorf("start healthcheck: %w", err)
	}
	d.healthcheck = healthcheck

	d.wg.Go(func() {
		sliceInformer.RunWithContext(ctx)
	})

	<-sliceInformer.HasSyncedChecker().Done()

	klog.V(2).Infof("Remote inject driver started on node %s (registration status: %s)",
		config.NodeName, helper.RegistrationStatus())
	return d, nil
}

func (d *InjectDriver) Shutdown() error {
	if d == nil {
		return nil
	}
	d.wg.Wait()
	d.helper.Stop()
	return nil
}

func (d *InjectDriver) PrepareResourceClaims(ctx context.Context, claims []*resourceapi.ResourceClaim) (map[types.UID]kubeletplugin.PrepareResult, error) {
	results := make(map[types.UID]kubeletplugin.PrepareResult)
	for _, claim := range claims {
		results[claim.UID] = d.prepareClaim(ctx, claim)
	}
	return results, nil
}

func (d *InjectDriver) UnprepareResourceClaims(ctx context.Context, claimRefs []kubeletplugin.NamespacedObject) (map[types.UID]error, error) {
	results := make(map[types.UID]error)
	for _, claimRef := range claimRefs {
		results[claimRef.UID] = d.cdi.DeleteClaimSpec(string(claimRef.UID))
	}
	return results, nil
}

func (d *InjectDriver) HandleError(ctx context.Context, err error, msg string) {
	runtime.HandleErrorWithContext(ctx, err, msg)
}

func (d *InjectDriver) WatchHealthStatus(context.Context, chan<- kubeletplugin.DeviceHealthReport) error {
	return kubeletplugin.ErrHealthNotSupported
}

func (d *InjectDriver) prepareClaim(ctx context.Context, claim *resourceapi.ResourceClaim) kubeletplugin.PrepareResult {
	fail := func(err error) kubeletplugin.PrepareResult {
		return kubeletplugin.PrepareResult{Err: err}
	}

	if claim.Status.Allocation == nil {
		return fail(fmt.Errorf("claim %s has no allocation", klog.KObj(claim)))
	}
	var allocated []resourceapi.DeviceRequestAllocationResult
	for _, result := range claim.Status.Allocation.Devices.Results {
		if result.Driver == util.DRADriverName {
			allocated = append(allocated, result)
		}
	}
	if len(allocated) == 0 {
		return fail(fmt.Errorf("claim %s has no devices allocated by %s", klog.KObj(claim), util.DRADriverName))
	}

	endpoints, minCUDA, err := d.resolveRemoteAllocation(allocated)
	if err != nil {
		return fail(err)
	}

	// Multiple servers with differing CUDA versions: the client artifact must
	// satisfy client <= server for every server, hence the minimum (§4.3).
	artifact, err := selectArtifact(d.config.ArtifactsDir, minCUDA)
	if err != nil {
		return fail(err)
	}

	// K1: the session token is the claim UID (also lets a spike materialize
	// the session by hand with vgpu-session-config --session <uid>).
	// TODO(D8): mint a random token and record it in
	// claim.status.devices[].data so it is unpredictable.
	token := string(claim.UID)

	// D2 barrier: every server must have the session quota on disk before
	// any container of the pod starts. A failure here makes the kubelet
	// retry NodePrepare with backoff.
	if err := EnsureSessions(ctx, endpoints, d.config.AgentPort, token, claim); err != nil {
		return fail(err)
	}
	klog.V(2).Infof("Remote claim %s: endpoints=%v artifact=%s (server CUDA floor %s), sessions ensured",
		klog.KObj(claim), endpoints, artifact.Name, minCUDA)

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
				fmt.Sprintf("%s=%s:$%s", util.LdLibraryPathEnv, artifact.LibDir, util.LdLibraryPathEnv),
				//fmt.Sprintf("%s=%s", util.LdPreloadEnv, artifact.LibDir),
			},
			Mounts: []*cdispec.Mount{{
				HostPath:      artifact.HostDir,
				ContainerPath: artifact.ContainerDir,
				Options:       []string{"ro", "nosuid", "nodev", "bind"},
			}},
		},
	}

	qualifiedName, err := d.cdi.WriteClaimSpec(string(claim.UID), edits)
	if err != nil {
		return fail(fmt.Errorf("failed to write CDI spec for claim %s: %w", klog.KObj(claim), err))
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

// resolveRemoteAllocation maps every allocation result to a published remote
// device and derives the LUPINE_SERVER endpoint list plus the CUDA-version
// floor across servers. The order of `allocated` comes from the recorded
// allocation and is stable across plugin restarts — the endpoint order
// (= client device ordinals, design §6.8) derives from it deterministically,
// deduplicated to first appearance.
func (d *InjectDriver) resolveRemoteAllocation(allocated []resourceapi.DeviceRequestAllocationResult) ([]string, *semver.Version, error) {
	endpointSet := sets.NewString()
	var minCUDA *semver.Version
	for _, result := range allocated {
		slices, err := d.GetPoolResourceSlices(result.Pool)
		if err != nil {
			return nil, nil, fmt.Errorf("allocated device pool %s not found in any published ResourceSlice of %s",
				result.Pool, util.DRADriverName)
		}
		dev, ok := slicesDeviceMap(slices)[result.Device]
		if !ok {
			return nil, nil, fmt.Errorf("allocated device %s/%s not found in any published ResourceSlice of %s",
				result.Pool, result.Device, util.DRADriverName)
		}
		info, isRemote, err := ParseDevice(dev)
		if err != nil {
			return nil, nil, err
		}
		if !isRemote {
			// The inject-mode plugin exists only on nodes without GPUs; a
			// non-remote allocation reaching it means the claim mixed local
			// and remote devices or the pool publishing is wrong. Fail
			// loudly rather than prepare half a claim.
			return nil, nil, fmt.Errorf("device %s/%s is not %s=%s; inject mode cannot prepare it",
				result.Pool, result.Device, AttrAccessMode, AccessModeRemote)
		}
		endpointSet.Insert(info.Endpoint)
		if minCUDA == nil || info.CUDAVersion.Compare(minCUDA) < 0 {
			minCUDA = info.CUDAVersion
		}
	}
	return endpointSet.List(), minCUDA, nil
}

func slicesDeviceMap(slices []*resourceapi.ResourceSlice) map[string]*resourceapi.Device {
	deviceMap := make(map[string]*resourceapi.Device)
	for _, slice := range slices {
		for j, dev := range slice.Spec.Devices {
			deviceMap[dev.Name] = &slice.Spec.Devices[j]
		}
	}
	return deviceMap
}
