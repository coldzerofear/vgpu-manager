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

	"github.com/coldzerofear/vgpu-manager/pkg/claimresolve"
	client2 "github.com/coldzerofear/vgpu-manager/pkg/client"
	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin/featuregates"
	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin/health"
	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin/nri"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"
	crcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
	// ArtifactsDir is the lupine client version directory
	// (<manager-dir>/driver, one subdirectory per CUDA version, design D12)
	// as seen by this process; it is read to enumerate versions.
	ArtifactsDir string
	// HostArtifactsDir is the same directory as seen by the kubelet/runtime;
	// it is what the emitted CDI mount names as its host path. Equal to
	// ArtifactsDir when the plugin mounts the manager dir at the host path.
	HostArtifactsDir string
	// AgentPort is the remote-agent gRPC port on every server host.
	AgentPort    int
	NRIRoot      string
	NRIPluginIdx string
}

// InjectDriver is the `--plugin-mode=inject` DRA driver: no GPU, no NVML — it
// translates allocations of accessMode=remote devices into per-partition
// sessions (D8), the EnsureSession barrier (D2) and env/mount CDI injections
// (design §2.3). On GPU nodes it runs next to the publish-only server plugin.
type InjectDriver struct {
	config       InjectConfig
	clients      pkgflags.ClientSets
	helper       *kubeletplugin.Helper
	cdi          *cdiWriter
	wg           sync.WaitGroup
	sliceIndexer cache.Indexer
	healthcheck  *health.Healthcheck
	// NRI mode state: claims prepared on this node (for the CreateContainer
	// hook) and the in-process plugin.
	preparedMu sync.Mutex
	prepared   map[string]*preparedClaim
	nriPlugin  *nri.Plugin
	nriCancel  context.CancelFunc
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
		config:   config,
		clients:  clients,
		cdi:      newCDIWriter(config.CdiRoot),
		prepared: map[string]*preparedClaim{},
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
	if err := sliceInformer.SetTransform(crcache.TransformStripManagedFields()); err != nil {
		return nil, err
	}
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

	if featuregates.Enabled(featuregates.NRISupport) {
		d.restorePrepared(ctx)
		if err := d.startNRI(ctx); err != nil {
			return nil, fmt.Errorf("start NRI plugin: %w", err)
		}
	}

	klog.V(2).Infof("Remote inject driver started on node %s (registration status: %s)",
		config.NodeName, helper.RegistrationStatus())
	return d, nil
}

func (d *InjectDriver) Shutdown() error {
	if d == nil {
		return nil
	}
	if d.nriCancel != nil {
		d.nriCancel()
	}
	if d.nriPlugin != nil {
		d.nriPlugin.Stop()
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
		d.forgetPrepared(string(claimRef.UID))
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

	// 1. Resolve every allocation result of ours to a published remote
	// device (accessMode=remote, endpoint, CUDA ceiling).
	devices, err := d.resolveRemoteDevices(claim)
	if err != nil {
		return fail(err)
	}
	if len(devices) == 0 {
		return fail(fmt.Errorf("claim %s has no devices allocated by %s", klog.KObj(claim), util.DRADriverName))
	}

	// 2. Client artifact: one mount for the claim, chosen against the CUDA
	// floor across every server it touches (§4.3).
	artifact, err := selectArtifact(d.config.ArtifactsDir, d.config.HostArtifactsDir, cudaFloor(devices))
	if err != nil {
		return fail(err)
	}
	baseEnv := []string{
		// Mandatory: prevents client-local routing (§4.3.2 lesson).
		fmt.Sprintf("%s=1", EnvLupineDisableLocal),
		// The artifact dir contains only libcuda.so.1/libnvidia-ml.so.1
		// (self-contained static client, D11), so shadowing is limited to
		// those two names. OCI/CDI env values are literal (no shell
		// expansion), so an image-defined LD_LIBRARY_PATH is replaced, not
		// extended (known K1 limitation).
		fmt.Sprintf("%s=%s", util.LdLibraryPathEnv, artifact.LibDir),
	}
	mounts := []*cdispec.Mount{{
		HostPath:      artifact.HostDir,
		ContainerPath: artifact.ContainerDir,
		Options:       []string{"ro", "nosuid", "nodev", "bind"},
	}}

	// 3. Session assignment.
	edits := map[string]*cdiapi.ContainerEdits{}
	idOf := map[int]string{} // index into devices -> CDI device id
	if featuregates.Enabled(featuregates.NRISupport) {
		// NRI mode: the per-container session (server list + token) is
		// injected at CreateContainer; CDI only carries the claim
		// correlation env. See nri.go.
		d.recordPrepared(claim, devices)
		containerEdits := &cdispec.ContainerEdits{Env: append(baseEnv, nriClaimEnv(claim)), Mounts: mounts}
		for i, rd := range devices {
			id := cdiDeviceID(rd, i)
			edits[id] = &cdiapi.ContainerEdits{ContainerEdits: containerEdits}
			idOf[rd.index] = id
		}
		klog.V(2).Infof("Remote claim %s prepared for NRI per-container sessions (%d device(s), artifact %s)",
			klog.KObj(claim), len(devices), artifact.Name)
	} else {
		// Partition mode: one session per connected component of the
		// container<->request graph over the reserved pods, resolved here
		// the same way the local path does (design D8 v2.2).
		allocatedRequests := sets.New[string]()
		for _, rd := range devices {
			allocatedRequests.Insert(rd.mainRequest)
		}
		info, err := claimresolve.ResolveClaimVGPUPartitionsFromAllocatedRequests(ctx, &apiReader{clients: d.clients}, claim, allocatedRequests)
		if err != nil {
			return fail(fmt.Errorf("resolve partitions of claim %s: %w", klog.KObj(claim), err))
		}
		partitions := buildPartitions(devices, info.RequestToPartition)

		// Tokens: reuse the annotation-recorded token for a partition
		// (retries / plugin restarts), mint the rest and persist them before
		// any server learns about them.
		if err := d.assignTokens(ctx, claim, partitions); err != nil {
			return fail(err)
		}

		// D2 barrier per partition: every server it spans must have the
		// session quota on disk before any container of the pod starts. A
		// failure makes the kubelet retry NodePrepare with backoff.
		ordinal := 0
		for _, p := range partitions {
			if err := EnsureSessions(ctx, p.endpoints, d.config.AgentPort, claim, p.token, p.key, p.requests); err != nil {
				return fail(err)
			}
			klog.V(2).Infof("Remote claim %s partition %s: requests=%v endpoints=%v artifact=%s, session ensured",
				klog.KObj(claim), p.key, p.requests, p.endpoints, artifact.Name)

			// One CDI device per allocation result, carrying its partition's
			// env. The kubelet hands a container only the devices of the
			// requests it references; all of them belong to one partition,
			// so the env never collides within a container.
			partitionEdits := &cdispec.ContainerEdits{
				Env: append([]string{
					fmt.Sprintf("%s=%s", EnvLupineServer, strings.Join(p.endpoints, ",")),
					fmt.Sprintf("%s=%s", EnvLupineSession, p.token),
				}, baseEnv...),
				Mounts: mounts,
			}
			for _, rd := range p.results {
				id := cdiDeviceID(rd, ordinal)
				ordinal++
				edits[id] = &cdiapi.ContainerEdits{ContainerEdits: partitionEdits}
				idOf[rd.index] = id
			}
		}
	}

	names, err := d.cdi.WriteClaimSpec(string(claim.UID), edits)
	if err != nil {
		return fail(fmt.Errorf("failed to write CDI spec for claim %s: %w", klog.KObj(claim), err))
	}

	out := make([]kubeletplugin.Device, 0, len(devices))
	for _, rd := range devices {
		out = append(out, kubeletplugin.Device{
			Requests:     []string{rd.result.Request},
			PoolName:     rd.result.Pool,
			DeviceName:   rd.result.Device,
			CDIDeviceIDs: []string{names[idOf[rd.index]]},
		})
	}
	return kubeletplugin.PrepareResult{Devices: out}
}

// resolveRemoteDevices maps each of our allocation results to its published
// device. A result that resolves to a non-remote device (accessMode=local)
// fails the claim: a claim mixing local-only and remote devices cannot be
// served by one injection path.
func (d *InjectDriver) resolveRemoteDevices(claim *resourceapi.ResourceClaim) ([]resultDevice, error) {
	var out []resultDevice
	for i, result := range claim.Status.Allocation.Devices.Results {
		if result.Driver != util.DRADriverName {
			continue
		}
		slices, err := d.GetPoolResourceSlices(result.Pool)
		if err != nil {
			return nil, fmt.Errorf("allocated device pool %s not found in any published ResourceSlice of %s",
				result.Pool, util.DRADriverName)
		}
		dev, ok := slicesDeviceMap(slices)[result.Device]
		if !ok {
			return nil, fmt.Errorf("allocated device %s/%s not found in any published ResourceSlice of %s",
				result.Pool, result.Device, util.DRADriverName)
		}
		info, isRemote, err := ParseDevice(dev)
		if err != nil {
			return nil, err
		}
		if !isRemote {
			return nil, fmt.Errorf("device %s/%s is not %s=%s; inject mode cannot prepare it",
				result.Pool, result.Device, AttrAccessMode, AccessModeRemote)
		}
		mainRequest := MainRequestName(claim, result.Request)
		if mainRequest == "" {
			return nil, fmt.Errorf("allocation result request %q is not a request of claim %s", result.Request, klog.KObj(claim))
		}
		out = append(out, resultDevice{index: i, result: result, info: info, mainRequest: mainRequest})
	}
	return out, nil
}

// assignTokens fills partition tokens from the claim annotations, minting and
// persisting new ones in a single merge patch.
func (d *InjectDriver) assignTokens(ctx context.Context, claim *resourceapi.ResourceClaim, partitions []*partition) error {
	fresh := map[string]*string{}
	for _, p := range partitions {
		key := SessionAnnotationKey(p.key)
		if tok := claim.Annotations[key]; tok != "" {
			p.token = tok
			continue
		}
		if tok, err := NewSessionToken(); err != nil {
			return err
		} else {
			p.token = tok
			fresh[key] = &tok
		}
	}
	if len(fresh) == 0 {
		return nil
	}
	metadata := client2.PatchMetadata{Annotations: fresh}
	patch, err := metadata.JSONBytes()
	if err != nil {
		return err
	}
	newClaim, err := d.clients.Resource.ResourceClaims(claim.Namespace).Patch(ctx, claim.Name, metadata.PatchType(), patch, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("record session tokens on claim %s: %w", klog.KObj(claim), err)
	}

	newClaim.DeepCopyInto(claim)

	return nil
}

// apiReader satisfies claimresolve.Reader with direct API reads, like the
// local path's kubeClaimResolveReader: NodePrepare is rare and the reserved
// pods may live on other nodes (shared claims), outside any node-scoped
// informer.
type apiReader struct {
	clients pkgflags.ClientSets
}

func (r *apiReader) GetPod(ctx context.Context, key client.ObjectKey, obj *corev1.Pod) error {
	pod, err := r.clients.Core.CoreV1().Pods(key.Namespace).Get(ctx, key.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	pod.DeepCopyInto(obj)
	return nil
}

func (r *apiReader) GetResourceClaim(ctx context.Context, key client.ObjectKey, obj *resourceapi.ResourceClaim) error {
	claim, err := r.clients.Resource.ResourceClaims(key.Namespace).Get(ctx, key.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	claim.DeepCopyInto(obj)
	return nil
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
