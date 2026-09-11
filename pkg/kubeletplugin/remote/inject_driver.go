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
	"sync/atomic"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/claimresolve"
	client2 "github.com/coldzerofear/vgpu-manager/pkg/client"
	"github.com/coldzerofear/vgpu-manager/pkg/deviceplugin/vgpu"
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
	"k8s.io/client-go/util/retry"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"
	crcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	pkgflags "sigs.k8s.io/dra-driver-nvidia-gpu/pkg/flags"
	drametrics "sigs.k8s.io/dra-driver-nvidia-gpu/pkg/metrics"
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
	NRIRoot          string
	NRISocket        string
	NRIPluginIdx     string
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
	// Claims prepared on this node (their devices, hence the agents behind
	// them): read by the NRI CreateContainer hook and by NodeUnprepare when
	// the claim object is already gone. Plus the in-process NRI plugin.
	preparedMu sync.Mutex
	prepared   map[string]*preparedClaim
	// nriPlugin is written by startNRI during startup and read by the
	// healthcheck goroutine, which StartHealthcheck spawns earlier — hence the
	// atomic rather than a plain field.
	nriPlugin atomic.Pointer[nri.Plugin]
	nriCancel context.CancelFunc
}

// nriHealthy is the health accessor the healthcheck consults (design §12.13.6).
// It mirrors the local driver's: healthy unless the NRI plugin has been
// disconnected past its grace period, in which case liveness fails and kubelet
// restarts the pod cleanly. Safe to call before the plugin exists (healthy
// while nil), which it always is — StartHealthcheck runs before startNRI.
func (d *InjectDriver) nriHealthy() bool {
	if plugin := d.nriPlugin.Load(); plugin != nil {
		return plugin.Healthy()
	}
	return true
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
		// One Prepare/Unprepare at a time on this node. Each call is short
		// (a few RPCs, bounded by ensureSessionTimeout per agent), and a
		// predictable order is worth more here than concurrency: the NRI
		// hook and the prepared-claim cache then never see two calls
		// interleave.
		kubeletplugin.Serialize(true),
		kubeletplugin.RegistrarDirectoryPath(config.KubeletRegistrarDirectoryPath),
		kubeletplugin.PluginDataDirectoryPath(config.PluginDataDirectoryPath),
		// This plugin does not report device health (KEP-4680), so don't
		// advertise the DRAResourceHealth service to the kubelet.
		kubeletplugin.HealthService(false),
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
	healthcheck, err := health.StartHealthcheck(ctx, healthConfig, helper, d.nriHealthy)
	if err != nil {
		return nil, fmt.Errorf("start healthcheck: %w", err)
	}
	d.healthcheck = healthcheck

	d.wg.Go(func() {
		sliceInformer.RunWithContext(ctx)
	})

	<-sliceInformer.HasSyncedChecker().Done()

	// Prepared claims are remembered in every mode: NRI needs them at
	// CreateContainer, and NodeUnprepare needs the agents of a claim whose
	// object is already gone.
	d.restorePrepared(ctx)
	if featuregates.Enabled(featuregates.NRISupport) {
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
	if plugin := d.nriPlugin.Load(); plugin != nil {
		plugin.Stop()
	}
	d.wg.Wait()
	d.helper.Stop()
	return nil
}

func (d *InjectDriver) PrepareResourceClaims(ctx context.Context, claims []*resourceapi.ResourceClaim) (map[types.UID]kubeletplugin.PrepareResult, error) {
	results := make(map[types.UID]kubeletplugin.PrepareResult)
	for _, claim := range claims {
		results[claim.UID] = d.nodePrepareResource(ctx, claim)
	}
	return results, nil
}

// nodePrepareResource wraps prepareClaim with the same DRA request metrics
// the server-mode driver records (driver.go): in-flight tracking, error
// counters and request duration, under the shared driver name and the same
// reason values so dashboards work across both modes.
func (d *InjectDriver) nodePrepareResource(ctx context.Context, claim *resourceapi.ResourceClaim) kubeletplugin.PrepareResult {
	t0 := time.Now()
	doneInFlight := drametrics.TrackInFlight(util.DRADriverName, "prepare")
	defer doneInFlight()

	result := d.prepareClaim(ctx, claim)
	if result.Err != nil {
		drametrics.IncNodePrepareError(util.DRADriverName, "prepare_devices")
		return result
	}

	drametrics.ObserveRequest(util.DRADriverName, "prepare", time.Since(t0))
	return result
}

func (d *InjectDriver) UnprepareResourceClaims(ctx context.Context, claimRefs []kubeletplugin.NamespacedObject) (map[types.UID]error, error) {
	results := make(map[types.UID]error)
	for _, claimRef := range claimRefs {
		results[claimRef.UID] = d.nodeUnprepareResource(claimRef)
	}
	return results, nil
}

// nodeUnprepareResource mirrors the server-mode unprepare metrics.
func (d *InjectDriver) nodeUnprepareResource(claimRef kubeletplugin.NamespacedObject) error {
	t0 := time.Now()
	doneInFlight := drametrics.TrackInFlight(util.DRADriverName, "unprepare")
	defer doneInFlight()

	if err := d.cdi.DeleteClaimSpec(string(claimRef.UID)); err != nil {
		drametrics.IncNodeUnprepareError(util.DRADriverName, "unprepare_devices")
		return err
	}

	if err := d.releaseClaim(context.Background(), claimRef); err != nil {
		drametrics.IncNodeUnprepareError(util.DRADriverName, "unprepare_devices")
		return err
	}

	d.forgetPrepared(string(claimRef.UID))
	drametrics.ObserveRequest(util.DRADriverName, "unprepare", time.Since(t0))
	return nil
}

// releaseClaim ends the claim's sessions when this node was its last
// consumer anywhere. The kubelet only tells us that no pod on *this* node
// references the claim; a standalone claim may still be reserved by pods on
// other nodes, which share the same tokens and sessions, so the check is
// against the claim's ReservedFor. When nobody is left, the tokens come
// off the claim first (the source of truth every sweep compares against),
// then each agent is asked to release the sessions right away -- best
// effort, the agents' own sweep finishes the job if one is unreachable.
//
// The decision and the annotation removal are one optimistic transaction:
// a Conflict on the patch means the claim changed since it was read (a new
// consumer on another node recorded its tokens), so the whole thing is
// re-evaluated against the fresh claim rather than removing on stale grounds.
func (d *InjectDriver) releaseClaim(ctx context.Context, claimRef kubeletplugin.NamespacedObject) error {
	uid := string(claimRef.UID)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		claim, err := d.clients.Resource.ResourceClaims(claimRef.Namespace).Get(ctx, claimRef.Name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err) || (err == nil && claim.UID != claimRef.UID):
			// The claim object is gone (or replaced): nothing to patch, and
			// the agents' claim watch has already swept. Release anyway, in
			// case that event was missed, with the tokens the prepared copy
			// of the claim recorded (the agents require them).
			if pc := d.lookupPrepared(uid); pc != nil {
				if tokens := ClaimSessionTokens(pc.claim.Annotations).UnsortedList(); len(tokens) > 0 {
					d.releaseSessions(ctx, uid, d.agentsOfClaim(nil, uid), tokens)
				}
			}
			return nil
		case err != nil:
			return err
		case !claim.DeletionTimestamp.IsZero():
			d.releaseSessions(ctx, uid, d.agentsOfClaim(claim, uid), ClaimSessionTokens(claim.Annotations).UnsortedList())
			return nil
		}

		live, err := d.claimHasLiveConsumers(ctx, claim)
		if err != nil {
			return err
		}
		if live {
			klog.V(2).Infof("Claim %s is still reserved by pods on other nodes; keeping its sessions", klog.KObj(claim))
			return nil
		}
		tokens := ClaimSessionTokens(claim.Annotations).UnsortedList()
		if err := d.cleanTokens(ctx, claim); err != nil {
			return err
		}
		d.releaseSessions(ctx, uid, d.agentsOfClaim(claim, uid), tokens)
		return nil
	})
}

// agentsOfClaim lists the agents the claim's sessions live on: resolved
// from the claim's allocation while the object exists, else from the
// NRI-mode prepared cache; empty when neither knows (the agents' sweep is
// then the only cleanup, which is the documented backstop).
func (d *InjectDriver) agentsOfClaim(claim *resourceapi.ResourceClaim, uid string) []string {
	var devices []resultDevice
	if claim != nil && claim.Status.Allocation != nil {
		if resolved, err := d.resolveRemoteDevices(claim); err == nil {
			devices = resolved
		}
	}
	if devices == nil {
		if pc := d.lookupPrepared(uid); pc != nil {
			devices = pc.devices
		}
	}
	agents := make([]string, 0, len(devices))
	for _, info := range endpointInfosOf(devices) {
		agents = append(agents, info.agentEndpoint)
	}
	return agents
}

// claimHasLiveConsumers reports whether any pod in the claim's ReservedFor
// may still be running. Pods on this node are known to be done (the
// kubelet unprepares only after the last one here stopped), pods that no
// longer exist or have terminated are done, anything else -- including a
// non-pod consumer -- counts as live.
func (d *InjectDriver) claimHasLiveConsumers(ctx context.Context, claim *resourceapi.ResourceClaim) (bool, error) {
	for _, ref := range claim.Status.ReservedFor {
		if ref.APIGroup != "" || ref.Resource != "pods" {
			return true, nil
		}
		pod, err := d.clients.Core.CoreV1().Pods(claim.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("get consumer pod %s/%s of claim %s: %w", claim.Namespace, ref.Name, klog.KObj(claim), err)
		}
		if pod.UID != ref.UID || pod.Spec.NodeName == d.config.NodeName ||
			pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		return true, nil
	}
	return false, nil
}

// releaseSessions asks each agent to drop the named sessions of the claim.
// Best effort by design: a
// failure is logged, never returned -- the agent's own sweep removes the
// same sessions once the tokens are off the claim.
func (d *InjectDriver) releaseSessions(ctx context.Context, uid string, agents, tokens []string) {
	for _, agent := range agents {
		released, err := ReleaseSessions(ctx, agent, uid, tokens)
		if err != nil {
			klog.Warningf("Release sessions of claim %s on %s: %v (the agent's sweep will finish it)", uid, agent, err)
			continue
		}
		klog.V(2).Infof("Released %d session(s) of claim %s on %s", released, uid, agent)
	}
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

	d.recordPrepared(claim, devices)

	// 2. Sessions. Partition mode: one session per connected component of
	// the container<->request graph over the reserved pods, resolved here
	// the same way the local path does (design D8 v2.2), ensured on every
	// server the partition spans before any container starts (D2). Their
	// answers also say which client build each server embeds, which step 3
	// checks the artifact against. NRI mode: sessions come later, at
	// CreateContainer (see nri.go); CDI only carries the claim correlation
	// env, and the artifact check asks the agent directly.
	type partitionSession struct {
		env     []string
		results []resultDevice
	}
	var sessions []partitionSession
	etagOf := map[string]string{}
	nriMode := featuregates.Enabled(featuregates.NRISupport)
	token := func() (string, error) { return d.claimPrepareToken(ctx, claim) }
	if !nriMode {
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
		d.updatePreparedClaim(claim) // the release path reads the tokens from this copy
		if len(partitions) > 0 {
			// Any session token of the claim authorizes a bundle download.
			token = func() (string, error) { return partitions[0].token, nil }
		}

		// D2 barrier per partition: every server it spans must have the
		// session quota on disk before any container of the pod starts. A
		// failure makes the kubelet retry NodePrepare with backoff.
		for _, p := range partitions {
			endpoints, etags, err := EnsureSessions(ctx, p.endpoints, claim, p.token, p.key, p.requests)
			if err != nil {
				return fail(err)
			}
			for agent, etag := range etags {
				etagOf[agent] = etag
			}
			klog.V(2).Infof("Remote claim %s partition %s: requests=%v agents=%v servers=%v, session ensured",
				klog.KObj(claim), p.key, p.requests, p.endpoints, endpoints)
			sessions = append(sessions, partitionSession{
				env: []string{
					fmt.Sprintf("%s=%s", EnvLupineServer, strings.Join(endpoints, ",")),
					fmt.Sprintf("%s=%s", EnvLupineSession, p.token),
				},
				results: p.results,
			})
		}
	}

	// 3. Client artifact: one mount for the claim, chosen against the CUDA
	// floor across every server it touches (§4.3) and verified against (or
	// fetched as) the build those servers embed.
	for _, rd := range devices {
		if rd.info.ServerCUDAVersion == nil {
			// Selection then only knows the driver ceiling; a server built for
			// an older CUDA would get a client that is too new. dra-server
			// republishes with serverCudaVersion as soon as the server answers.
			klog.Warningf("Claim %s: device %s (agent %s) has no %s yet, choosing the client artifact by the driver ceiling %s only",
				klog.KObj(claim), rd.result.Device, rd.info.AgentEndpoint, AttrServerCUDAVersion, rd.info.CUDAVersion)
		}
	}
	artifact, err := d.ensureArtifact(ctx, claim, devices, etagOf, token)
	if err != nil {
		return fail(err)
	}

	// How the container finds the shims (why /etc/ld.so.preload):
	//   - ld.so.conf.d is only read by ldconfig, never by the loader at
	//     runtime, so a mounted conf snippet alone changes nothing;
	//   - regenerating ld.so.cache would need a hook binary on every
	//     consumer node and a working ldconfig in every image;
	//   - LD_LIBRARY_PATH via CDI env would replace an image-defined value.
	// The loader reads /etc/ld.so.preload directly for every process, and a
	// later dlopen("libcuda.so.1") resolves to the already-loaded shim by
	// SONAME. Same single-file mount the local path uses. Boundaries: an
	// ld.so.preload the image ships is shadowed, and setuid binaries skip
	// preload entries containing "/" (harmless — they need no CUDA).
	ldPreloadHost, err := ensureLdPreloadFile(d.config.ArtifactsDir, artifact)
	if err != nil {
		return fail(err)
	}

	// Mandatory: prevents client-local routing (§4.3.2 lesson).
	baseEnv := []string{
		// Remote GPU without injecting any real Nvidia devices or drivers
		"NVIDIA_VISIBLE_DEVICES=void",
		// TODO In the future, this failed environment variable will be removed
		fmt.Sprintf("%s=1", EnvLupineDisableLocal),
	}
	if artifact.ETag != "" {
		// Known build: let the server refuse the pod (426) rather than fail
		// on an unknown RPC if the two ever diverge.
		baseEnv = append(baseEnv,
			fmt.Sprintf("%s=%s", EnvLupineClientETag, artifact.ETag),
			fmt.Sprintf("%s=%s", EnvLupineClientPlatform, LocalClientBundlePlatform()),
		)
	}

	mounts := []*cdispec.Mount{{ // the client shim libraries
		HostPath:      artifact.HostDir,
		ContainerPath: artifact.ContainerDir,
		Options:       []string{"ro", "nosuid", "nodev", "bind"},
	}, { // the preload list that makes them loadable, env untouched
		HostPath:      ldPreloadHost,
		ContainerPath: vgpu.ContPreLoadFilePath,
		Options:       []string{"ro", "nosuid", "nodev", "bind"},
	}}

	if artifact.NvidiaSMIHost != "" {
		// Single-file bind so the pod keeps its own /usr/bin (gpu-go lesson:
		// never overlay a system directory). nvidia-smi dlopens
		// libnvidia-ml.so.1 at runtime, which the preload list above resolves
		// to the lupine shim - so it reports the remote session's view.
		mounts = append(mounts, &cdispec.Mount{
			HostPath:      artifact.NvidiaSMIHost,
			ContainerPath: "/usr/bin/nvidia-smi",
			Options:       []string{"ro", "nosuid", "nodev", "bind"},
		})
	}

	// 4. CDI edits: one CDI device per allocation result.
	edits := map[string]*cdiapi.ContainerEdits{}
	idOf := map[int]string{} // index into devices -> CDI device id
	if nriMode {
		containerEdits := &cdispec.ContainerEdits{Env: append(baseEnv, nriClaimEnv(claim)), Mounts: mounts}
		for i, rd := range devices {
			id := cdiDeviceID(rd, i)
			edits[id] = &cdiapi.ContainerEdits{ContainerEdits: containerEdits}
			idOf[rd.index] = id
		}
		klog.V(2).Infof("Remote claim %s prepared for NRI per-container sessions (%d device(s), artifact %s)",
			klog.KObj(claim), len(devices), artifact.Name)
	} else {
		// Each result carries its partition's env. The kubelet hands a
		// container only the devices of the requests it references; all of
		// them belong to one partition, so the env never collides within a
		// container.
		ordinal := 0
		for _, s := range sessions {
			partitionEdits := &cdispec.ContainerEdits{Env: append(s.env, baseEnv...), Mounts: mounts}
			for _, rd := range s.results {
				id := cdiDeviceID(rd, ordinal)
				ordinal++
				edits[id] = &cdiapi.ContainerEdits{ContainerEdits: partitionEdits}
				idOf[rd.index] = id
			}
		}
		klog.V(2).Infof("Remote claim %s prepared: %d partition(s), artifact %s", klog.KObj(claim), len(sessions), artifact.Name)
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
			return nil, fmt.Errorf("allocation result request %q is not a request of claim %s",
				result.Request, klog.KObj(claim))
		}
		out = append(out, resultDevice{index: i, result: result, info: info, mainRequest: mainRequest})
	}
	return out, nil
}

// cleanTokens removes every session annotation from the claim. Only called
// once the claim has no live consumer (see releaseClaim): the annotations
// are what the agents keep sessions for. The patch is conditional on the
// claim version that decision was made against; a Conflict means someone
// changed the claim meanwhile (a new consumer recording its tokens) and the
// caller must look again.
func (d *InjectDriver) cleanTokens(ctx context.Context, claim *resourceapi.ResourceClaim) error {
	metadata := client2.PatchMetadata{Annotations: map[string]*string{}, ResourceVersion: claim.ResourceVersion}
	for key := range claim.GetAnnotations() {
		if strings.HasPrefix(key, SessionAnnotationPrefix) || key == AllocationAnnotation {
			metadata.Annotations[key] = nil
		}
	}
	if len(metadata.Annotations) > 0 {
		data, err := metadata.JSONBytes()
		if err != nil {
			return err
		}
		_, err = d.clients.Core.ResourceV1().ResourceClaims(claim.Namespace).
			Patch(ctx, claim.Name, metadata.PatchType(), data, metav1.PatchOptions{})
		if err != nil {
			return client.IgnoreNotFound(err)
		}
	}
	return nil
}

// assignTokens fills partition tokens from the claim annotations, minting and
// persisting new ones in a single merge patch.
//
// The patch is conditional on the claim version the tokens were decided
// against: another node may be recording or removing tokens on the same
// claim at the same time (a consumer starting here while the previous one
// is unprepared elsewhere). On a conflict the claim is re-read and the
// decision redone, so a token is never reused or dropped on stale grounds.
func (d *InjectDriver) assignTokens(ctx context.Context, claim *resourceapi.ResourceClaim, partitions []*partition) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		annotations, err := sessionTokenPatch(claim, partitions)
		if err != nil || len(annotations) == 0 {
			return err
		}
		metadata := client2.PatchMetadata{Annotations: annotations, ResourceVersion: claim.ResourceVersion}
		patch, err := metadata.JSONBytes()
		if err != nil {
			return err
		}
		newClaim, err := d.clients.Core.ResourceV1().ResourceClaims(claim.Namespace).
			Patch(ctx, claim.Name, metadata.PatchType(), patch, metav1.PatchOptions{})
		if apierrors.IsConflict(err) {
			if rerr := d.refreshClaim(ctx, claim); rerr != nil {
				return rerr
			}
			return err
		}
		if err != nil {
			return fmt.Errorf("record session tokens on claim %s: %w", klog.KObj(claim), err)
		}
		newClaim.DeepCopyInto(claim)
		return nil
	})
}

// sessionTokenPatch decides the token of every partition against the claim
// as it is now, filling p.token, and returns the annotation changes that
// record the decision (nil = nothing to write).
//
// Tokens are scoped to the allocation they were issued for. Ones recorded
// for an earlier allocation of this claim (a standalone claim deallocated
// and allocated again before the previous consumer's NodeUnprepare removed
// them) are dropped in the same patch that records the new ones; the
// agents sweep their sessions on that update.
func sessionTokenPatch(claim *resourceapi.ResourceClaim, partitions []*partition) (map[string]*string, error) {
	allocationID := AllocationID(claim)
	reusable := claim.Annotations[AllocationAnnotation] == allocationID
	annotations := map[string]*string{}
	if !reusable {
		for key := range claim.Annotations {
			if strings.HasPrefix(key, SessionAnnotationPrefix) {
				annotations[key] = nil
			}
		}
	}
	for _, p := range partitions {
		key := SessionAnnotationKey(p.key)
		if tok := claim.Annotations[key]; reusable && tok != "" {
			p.token = tok
			continue
		}
		tok, err := NewSessionToken()
		if err != nil {
			return nil, err
		}
		p.token = tok
		annotations[key] = &tok
	}
	if len(annotations) == 0 {
		return nil, nil
	}
	annotations[AllocationAnnotation] = &allocationID
	return annotations, nil
}

// refreshClaim replaces claim with its current API state; the same object
// (by UID) is required, a replaced claim ends the retry.
func (d *InjectDriver) refreshClaim(ctx context.Context, claim *resourceapi.ResourceClaim) error {
	fresh, err := d.clients.Resource.ResourceClaims(claim.Namespace).Get(ctx, claim.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if fresh.UID != claim.UID {
		return fmt.Errorf("claim %s was replaced (uid %s -> %s)", klog.KObj(claim), claim.UID, fresh.UID)
	}
	fresh.DeepCopyInto(claim)
	return nil
}

// apiReader satisfies claimresolve.Reader with direct API reads, like the
// local path's kubeClaimResolveReader: NodePrepare is rare and the reserved
// pods may live on other nodes (shared claims), outside any node-scoped informer.
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
