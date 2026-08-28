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
	"path/filepath"
	"strings"

	"github.com/coldzerofear/vgpu-manager/pkg/claimresolve"
	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin/nri"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
)

// NRI mode (NRISupport gate on the inject side): sessions are assigned per
// (pod, container) at the NRI CreateContainer hook instead of per partition
// at NodePrepare. CreateContainer knows the exact container, so even
// containers that share one claim request get their own session and their
// own accounting — the same granularity the local NRI path gives its
// per-container partition directories.
//
// Division of labour:
//   - NodePrepare (CDI): LUPINE_DISABLE_LOCAL, LD_LIBRARY_PATH, the artifact
//     mount, and MANAGER_VGPU_CLAIM_UID (the correlation env the NRI plugin
//     keys on, exactly like the local path). No session, no server list.
//   - CreateContainer (NRI): resolve the requests this container references,
//     mint/reuse the token for partition key <podUID>_<containerName>, run
//     the EnsureSession barrier, and inject LUPINE_SERVER + LUPINE_SESSION.
//     CreateContainer precedes the container's process start, so the D2
//     barrier holds.

// NRIPartitionKey is the per-container partition key used in NRI mode,
// shaped like the local path's NRI partition directory name.
func NRIPartitionKey(podUID, containerName string) string {
	return fmt.Sprintf("%s_%s", podUID, containerName)
}

// preparedClaim is what NodePrepare records for the NRI hook.
type preparedClaim struct {
	claim   *resourceapi.ResourceClaim
	devices []resultDevice
}

func (d *InjectDriver) recordPrepared(claim *resourceapi.ResourceClaim, devices []resultDevice) {
	d.preparedMu.Lock()
	defer d.preparedMu.Unlock()
	d.prepared[string(claim.UID)] = &preparedClaim{claim: claim.DeepCopy(), devices: devices}
}

func (d *InjectDriver) forgetPrepared(claimUID string) {
	d.preparedMu.Lock()
	defer d.preparedMu.Unlock()
	delete(d.prepared, claimUID)
}

func (d *InjectDriver) lookupPrepared(claimUID string) *preparedClaim {
	d.preparedMu.Lock()
	defer d.preparedMu.Unlock()
	return d.prepared[claimUID]
}

// isClaimPrepared is the NRI plugin's authorization check for the
// attacker-controllable MANAGER_VGPU_CLAIM_UID env.
func (d *InjectDriver) isClaimPrepared(claimUID string) bool {
	return d.lookupPrepared(claimUID) != nil
}

func (d *InjectDriver) startNRI(ctx context.Context) error {
	var socketPath string
	if d.config.NRIRoot != "" {
		socketPath = filepath.Join(d.config.NRIRoot, "nri.sock")
	}
	plugin, err := nri.NewPlugin(nri.Config{
		SocketPath:      socketPath,
		PluginName:      util.DRADriverName,
		PluginIdx:       d.config.NRIPluginIdx,
		Cache:           nri.NewCache(),
		IsClaimPrepared: d.isClaimPrepared,
		ResolveMounts:   d.nriInjection,
	})
	if err != nil {
		return err
	}
	d.nriPlugin = plugin
	nriCtx, cancel := context.WithCancel(ctx)
	d.nriCancel = cancel
	d.wg.Go(func() {
		klog.V(4).InfoS("Starting in-process NRI plugin (remote inject mode)", "socket", socketPath)
		plugin.Run(nriCtx)
	})
	return nil
}

// nriInjection is the NRI ResolveMounts callback: per-container session.
func (d *InjectDriver) nriInjection(claimUID, podName, podNamespace, podUID, containerName string) (*nri.Injection, error) {
	ctx := context.Background()
	pc := d.lookupPrepared(claimUID)
	if pc == nil {
		return nil, fmt.Errorf("claim %s is not prepared on this node", claimUID)
	}
	claim := pc.claim

	// Which of the claim's requests does this container reference? Read the
	// live pod: the NRI sandbox carries identity only, not the spec.
	pod, err := d.clients.Core.CoreV1().Pods(podNamespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get pod %s/%s: %w", podNamespace, podName, err)
	}
	if string(pod.UID) != podUID {
		return nil, fmt.Errorf("pod %s/%s UID mismatch (%s != %s)", podNamespace, podName, pod.UID, podUID)
	}
	allocated := sets.New[string]()
	for _, rd := range pc.devices {
		allocated.Insert(rd.mainRequest)
	}
	requests := sets.New[string]()
	for _, container := range util.GetAllPodContainers(pod) {
		if container.Name != containerName {
			continue
		}
		for _, claimRef := range container.Claims {
			actual, ok, err := claimresolve.ResolveActualClaimNameForPodClaim(pod, claimRef.Name)
			if err != nil {
				return nil, err
			}
			if !ok || actual != claim.Name {
				continue
			}
			requests.Insert(claimresolve.ResolveActualAllocatedRequestsForClaimRef(claimRef, allocated)...)
		}
	}
	if requests.Len() == 0 {
		// Not a consumer of this claim (or references nothing allocated): the
		// CDI env still made the plugin call us. Inject nothing.
		klog.V(4).InfoS("NRI: container references no allocated request of the claim; skipping session",
			"pod", klog.KObj(pod), "container", containerName, "claim", klog.KObj(claim))
		return nil, nil
	}

	p := &partition{key: NRIPartitionKey(podUID, containerName), requests: sets.List(requests)}
	endpoints := sets.New[string]()
	for _, rd := range pc.devices {
		if requests.Has(rd.mainRequest) {
			endpoints.Insert(rd.info.Endpoint)
			p.results = append(p.results, rd)
		}
	}
	p.endpoints = sets.List(endpoints)

	if err := d.assignTokens(ctx, claim, []*partition{p}); err != nil {
		return nil, err
	}
	if err := EnsureSessions(ctx, p.endpoints, d.config.AgentPort, claim, p.token, p.key, p.requests); err != nil {
		return nil, err
	}
	klog.V(2).Infof("NRI: container %s/%s/%s session %s ensured on %v (requests %v)",
		podNamespace, podName, containerName, p.key, p.endpoints, p.requests)

	return &nri.Injection{
		Env: []string{
			fmt.Sprintf("%s=%s", EnvLupineServer, strings.Join(p.endpoints, ",")),
			fmt.Sprintf("%s=%s", EnvLupineSession, p.token),
		},
	}, nil
}

// nriClaimEnv is the CDI env NodePrepare injects in NRI mode so the plugin
// can correlate the container back to its claim.
func nriClaimEnv(claim *resourceapi.ResourceClaim) string {
	return fmt.Sprintf("%s=%s", util.ManagerVGpuClaimUid, claim.UID)
}
