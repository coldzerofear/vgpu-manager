/*
 * Copyright 2024 NVIDIA CORPORATION.
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

// Package nri hosts the in-process NRI plugin for vgpu-manager's DRA driver.
//
// It is started by the driver only when the NRISupport feature gate is enabled
// (design doc dra_nri_integration_design.md §12.13). It dials the runtime NRI
// socket over ttrpc, and on every (re)connect rebuilds a per-container cache
// from container env via Synchronize.
//
// This file is the SKELETON + DRY-RUN stage (§12.6 item 1): every hook only
// observes and logs; CreateContainer returns no adjustment. Actual mount/env
// injection is a later stage, gated on the §12.12 validation this dry-run mode
// produces.
package nri

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/device/registry"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/containerd/nri/pkg/api"
	"github.com/containerd/nri/pkg/stub"
	"k8s.io/klog/v2"
)

const (
	// reconnectBaseDelay / reconnectMaxDelay bound the exponential backoff of
	// the reconnect loop. Unlike dranet / dra-driver-cpu (5 attempts, no
	// backoff, then give up), we retry indefinitely with backoff and never
	// crash the process — the DRA path shares this process (design §12.13.3).
	reconnectBaseDelay = 1 * time.Second
	reconnectMaxDelay  = 30 * time.Second

	// healthyRunThreshold: a Run that lasted at least this long is treated as a
	// healthy session, so the backoff resets on the next disconnect.
	healthyRunThreshold = reconnectMaxDelay

	// defaultFailureGracePeriod bounds the recovery tier: if the plugin stays
	// continuously disconnected longer than this (i.e. reconnect retries within
	// the window are exhausted), it flips to an unhealthy state that the
	// healthcheck surfaces to kubelet (design §12.13.3/§12.13.6). The reconnect
	// loop keeps trying, so a later recovery clears it.
	defaultFailureGracePeriod = 2 * time.Minute
)

// envPrefixesOfInterest are the env keys the plugin highlights in logs; the vGPU
// CDI edits inject env starting with one of these (see pkg/kubeletplugin/vgpu.go).
var envPrefixesOfInterest = []string{"MANAGER_", "VGPU_", "CUDA_", "LD_PRELOAD", "NVIDIA_"}

// mountDestsOfInterest are the container paths the vGPU mounts target.
var mountDestsOfInterest = []string{
	"/etc/vgpu-manager/config", "/etc/vgpu-manager/registry", "/etc/vgpu-manager/driver",
	"/etc/ld.so.preload", "/tmp/.vgpu_lock", "/tmp/.vmem_node",
}

// Config configures the in-process NRI plugin.
type Config struct {
	// SocketPath is the runtime NRI socket to dial. Empty uses the NRI library
	// default (/var/run/nri/nri.sock).
	SocketPath string
	// PluginName / PluginIdx register the plugin with the runtime. Idx orders
	// this plugin relative to OTHER NRI plugins; it does not affect CDI-vs-NRI
	// ordering (CDI is applied by the runtime before any NRI CreateContainer).
	PluginName string
	PluginIdx  string
	// DryRun, when true, makes every hook observe-and-log only, injecting
	// nothing. This is the §12.12 validation mode.
	DryRun bool
	// Cache is the shared (podUID, containerName) -> Entry store the register
	// server's pod-uid resolver reads.
	Cache *Cache
	// FailureGracePeriod bounds the recovery tier before the plugin reports
	// unhealthy. Zero uses defaultFailureGracePeriod.
	FailureGracePeriod time.Duration
	// IsClaimPrepared validates that a claim UID read from (attacker-controllable)
	// container env was actually prepared on this node (§12.12.1). nil disables
	// the check. Injected by the driver (backed by the DRA checkpoint) to avoid
	// an import cycle.
	IsClaimPrepared func(claimUID string) bool
	// ResolveMounts ensures the per-container partition directories exist and
	// returns the mounts + env to inject. Returning (nil, nil) skips injection.
	// When nil, the plugin runs observe-only regardless of DryRun. Injected by
	// the driver.
	ResolveMounts func(claimUID, podName, podNamespace, podUID, containerName string) (*Injection, error)
}

// Mount is one partition bind mount the plugin injects at CreateContainer.
type Mount struct {
	ContainerPath string
	HostPath      string
	Options       []string
}

// Injection is what ResolveMounts returns: the mounts + env to add to a vGPU
// container, plus the ConfigDir (in the plugin's filesystem view) recorded in
// the cache for the register server's pod-uid path.
type Injection struct {
	Mounts    []Mount
	Env       []string // "KEY=VALUE"
	ConfigDir string
}

// Plugin is the in-process NRI plugin.
type Plugin struct {
	stub               stub.Stub
	cache              *Cache
	dryRun             bool
	failureGracePeriod time.Duration
	isClaimPrepared    func(claimUID string) bool
	resolveMounts      func(claimUID, podName, podNamespace, podUID, containerName string) (*Injection, error)

	// createdAt tracks CreateContainer timestamps so StartContainer can report
	// the create→start delta (validates ordering; §12.12 item 3).
	mu        sync.Mutex
	createdAt map[string]time.Time

	// healthMu guards the recovery/escalation state (§12.13.3). failed is set
	// when the plugin has been continuously disconnected past the grace period;
	// the reconnect loop keeps running so a later healthy session clears it.
	healthMu       sync.Mutex
	firstFailureAt time.Time
	failed         bool
}

// ValidatePluginIdx reports whether idx is a valid NRI plugin index (exactly two
// digits). containerd only rejects a bad index at registration time, which
// surfaces as an obscure reconnect-loop failure — validate it early instead.
func ValidatePluginIdx(idx string) error {
	return api.CheckPluginIndex(idx)
}

// NewPlugin builds the plugin and its NRI stub (does not connect yet).
func NewPlugin(cfg Config) (*Plugin, error) {
	if cfg.Cache == nil {
		cfg.Cache = NewCache()
	}
	name := cfg.PluginName
	if name == "" {
		name = util.DRADriverName
	}
	idx := cfg.PluginIdx
	if idx == "" {
		idx = "00"
	}
	grace := cfg.FailureGracePeriod
	if grace <= 0 {
		grace = defaultFailureGracePeriod
	}
	p := &Plugin{
		cache:              cfg.Cache,
		dryRun:             cfg.DryRun,
		failureGracePeriod: grace,
		isClaimPrepared:    cfg.IsClaimPrepared,
		resolveMounts:      cfg.ResolveMounts,
		createdAt:          make(map[string]time.Time),
	}
	opts := []stub.Option{
		stub.WithPluginName(name),
		stub.WithPluginIdx(idx),
		stub.WithOnClose(p.onClose),
	}
	if cfg.SocketPath != "" {
		opts = append(opts, stub.WithSocketPath(cfg.SocketPath))
	}
	s, err := stub.New(p, opts...)
	if err != nil {
		return nil, err
	}
	p.stub = s
	return p, nil
}

// Run drives the reconnect loop until ctx is canceled. It is meant to be called
// in its own goroutine. It NEVER crashes the process on failure — on disconnect
// it backs off and reconnects, so the DRA path in this same process keeps
// serving even while the NRI runtime is down (design §12.13.3). If it stays
// disconnected past the grace period it reports unhealthy via Healthy() (so the
// healthcheck can surface it to kubelet, §12.13.6), but keeps reconnecting so a
// later recovery clears the state.
func (p *Plugin) Run(ctx context.Context) {
	delay := reconnectBaseDelay
	for {
		start := time.Now()
		err := p.stub.Run(ctx)
		if ctx.Err() != nil {
			klog.V(4).InfoS("NRI plugin stopping (context canceled)")
			return
		}
		healthySession := time.Since(start) >= healthyRunThreshold
		p.recordDisconnect(healthySession)
		if err != nil {
			klog.ErrorS(err, "NRI plugin connection ended; will reconnect", "healthy", p.Healthy())
		} else {
			klog.InfoS("NRI plugin connection closed; will reconnect", "healthy", p.Healthy())
		}
		// A long-lived session is considered healthy: reset the backoff so a
		// later transient blip reconnects promptly.
		if healthySession {
			delay = reconnectBaseDelay
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, reconnectMaxDelay)
	}
}

// recordDisconnect updates the recovery/escalation state after a stub.Run
// returns. A session that lasted long enough to be healthy resets the failure
// window; otherwise, once the plugin has been continuously disconnected past
// the grace period, it flips to failed (surfaced via Healthy()).
func (p *Plugin) recordDisconnect(healthySession bool) {
	p.healthMu.Lock()
	defer p.healthMu.Unlock()
	now := time.Now()
	if healthySession {
		// Was up long enough to be healthy; this disconnect starts a fresh
		// failure window rather than counting against the previous one.
		p.firstFailureAt = now
		p.failed = false
		return
	}
	if p.firstFailureAt.IsZero() {
		p.firstFailureAt = now
	}
	if now.Sub(p.firstFailureAt) > p.failureGracePeriod {
		p.failed = true
	}
}

// Healthy reports whether the plugin is within its recovery tier. It returns
// false only after the plugin has been continuously disconnected past the grace
// period. The healthcheck consumes this (only when NRISupport is enabled) so a
// persistent NRI failure fails the pod's liveness probe, yielding a clean,
// checkpoint-safe restart (design §12.13.6).
func (p *Plugin) Healthy() bool {
	p.healthMu.Lock()
	defer p.healthMu.Unlock()
	return !p.failed
}

// Stop tears down the stub for graceful shutdown.
func (p *Plugin) Stop() {
	if p.stub != nil {
		p.stub.Stop()
	}
}

func (p *Plugin) onClose() {
	klog.InfoS("NRI plugin ttrpc connection closed")
}

// Configure logs the runtime + NRI version we are talking to. Returning 0 keeps
// the default event subscription derived from the interfaces we implement.
func (p *Plugin) Configure(_ context.Context, config, runtime, version string) (api.EventMask, error) {
	klog.InfoS("NRI Configure", "runtime", runtime, "runtimeVersion", version, "dryRun", p.dryRun)
	return 0, nil
}

// Synchronize is the (re)connect replay: rebuild the whole cache from the
// current container set's env (design §12.9). It never mutates running
// containers, so it returns no updates.
func (p *Plugin) Synchronize(_ context.Context, pods []*api.PodSandbox, containers []*api.Container) ([]*api.ContainerUpdate, error) {
	podUIDByID := make(map[string]string, len(pods))
	for _, pod := range pods {
		podUIDByID[pod.GetId()] = pod.GetUid()
	}

	rebuilt := make(map[string]Entry)
	vgpuCount := 0
	for _, c := range containers {
		podUID := podUIDByID[c.GetPodSandboxId()]
		claimUID, ok := lookupEnv(c.GetEnv(), util.ManagerVGpuClaimUid)
		if !ok || podUID == "" || claimUID == "" {
			continue
		}
		// Validate against node prepared state — env is attacker-controllable
		// (§12.12.1), so never rebuild a cache entry for an unprepared claim.
		if p.isClaimPrepared != nil && !p.isClaimPrepared(claimUID) {
			continue
		}
		vgpuCount++
		entry := Entry{ClaimUID: claimUID, ConfigDir: ConfigDirFor(claimUID, podUID, c.GetName())}
		rebuilt[Key(podUID, c.GetName())] = entry
		klog.V(4).InfoS("NRI Synchronize vGPU container",
			"container", c.GetName(), "podUID", podUID, "claimUID", claimUID,
			"configDir", entry.ConfigDir, "state", c.GetState().String())
	}

	p.cache.Replace(rebuilt)
	klog.InfoS("NRI Synchronize complete",
		"pods", len(pods), "containers", len(containers),
		"vgpuContainers", vgpuCount, "cacheEntries", p.cache.Len(), "dryRun", p.dryRun)
	return nil, nil
}

// CreateContainer injects the per-container partition mounts + register env for
// a vGPU container. A container is ours iff it carries MANAGER_VGPU_CLAIM_UID
// (injected by DRA Prepare via CDI). The claim UID is validated against the
// node's prepared claims (§12.12.1) before anything is injected — env is
// attacker-controllable, so it is only a correlation key, never trusted.
//
// In observe-only mode (DryRun or no ResolveMounts wired) it logs and injects
// nothing (the §12.12 validation mode).
func (p *Plugin) CreateContainer(_ context.Context, pod *api.PodSandbox, c *api.Container) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	p.mu.Lock()
	p.createdAt[c.GetId()] = time.Now()
	p.mu.Unlock()

	claimUID, hasClaim := lookupEnv(c.GetEnv(), util.ManagerVGpuClaimUid)

	// Observe-only: log what the runtime built (validates CDI-before-NRI + no
	// PID yet), inject nothing.
	if p.dryRun || p.resolveMounts == nil {
		_, hasCompat := lookupEnv(c.GetEnv(), util.ManagerCompatibilityMode)
		klog.InfoS("NRI CreateContainer (observe-only)",
			"pod", pod.GetName(), "podUID", pod.GetUid(), "namespace", pod.GetNamespace(),
			"container", c.GetName(), "state", c.GetState().String(), "pid", c.GetPid(),
			"isVGPUContainer", hasCompat, "hasClaimUIDEnv", hasClaim, "claimUID", claimUID,
			"envOfInterest", filterEnv(c.GetEnv()), "mountsOfInterest", filterMounts(c.GetMounts()),
			"dryRun", p.dryRun)
		return nil, nil, nil
	}

	// Not a vGPU container of ours: leave it untouched.
	if !hasClaim || claimUID == "" {
		return nil, nil, nil
	}

	// Validate the (attacker-controllable) claim UID against node prepared state.
	if p.isClaimPrepared != nil && !p.isClaimPrepared(claimUID) {
		klog.InfoS("NRI CreateContainer: claim not prepared on this node; skipping injection (possible spoofed env)",
			"pod", pod.GetName(), "podUID", pod.GetUid(), "container", c.GetName(), "claimUID", claimUID)
		return nil, nil, nil
	}

	inj, err := p.resolveMounts(claimUID, pod.GetName(), pod.GetNamespace(), pod.GetUid(), c.GetName())
	if err != nil {
		// Fail-closed: a container starting without its vGPU isolation is worse
		// than not starting. containerd aborts container creation on this error.
		return nil, nil, fmt.Errorf("NRI resolve partition mounts (claim %s, pod %s, container %s): %w",
			claimUID, pod.GetUid(), c.GetName(), err)
	}
	if inj == nil {
		return nil, nil, nil
	}

	adjust := &api.ContainerAdjustment{}
	for _, m := range inj.Mounts {
		adjust.AddMount(&api.Mount{
			Destination: m.ContainerPath,
			Source:      m.HostPath,
			Type:        "none",
			Options:     m.Options,
		})
	}
	for _, e := range inj.Env {
		if k, v, ok := splitEnv(e); ok {
			adjust.AddEnv(k, v)
		}
	}

	// Clean up old cache files (if any)
	pidsConfigPath := filepath.Join(inj.ConfigDir, registry.PidsConfig)
	basePath := strings.TrimSuffix(inj.ConfigDir, util.Config)
	vmemNodeConfigPath := filepath.Join(basePath, util.VMemNode, util.VMemNodeFile)
	_ = os.RemoveAll(pidsConfigPath)
	_ = os.RemoveAll(vmemNodeConfigPath)

	p.cache.Set(pod.GetUid(), c.GetName(), Entry{ClaimUID: claimUID, ConfigDir: inj.ConfigDir})

	klog.InfoS("NRI CreateContainer injected",
		"pod", pod.GetName(), "podUID", pod.GetUid(), "container", c.GetName(),
		"claimUID", claimUID, "configDir", inj.ConfigDir, "mounts", len(inj.Mounts))
	return adjust, nil, nil
}

// StartContainer fires after the process starts; logging pid (now non-zero) and
// the create→start delta confirms CreateContainer precedes StartContainer and
// that PIDs only exist from Start onward (§12.12 item 3).
func (p *Plugin) StartContainer(_ context.Context, pod *api.PodSandbox, c *api.Container) error {
	var sinceCreate time.Duration
	p.mu.Lock()
	if t, ok := p.createdAt[c.GetId()]; ok {
		sinceCreate = time.Since(t)
	}
	p.mu.Unlock()
	klog.V(4).InfoS("NRI StartContainer",
		"pod", pod.GetName(), "container", c.GetName(),
		"state", c.GetState().String(), "pid", c.GetPid(),
		"sinceCreateMillis", sinceCreate.Milliseconds())
	return nil
}

// RemoveContainer drops the container's cache entry and create-timestamp.
func (p *Plugin) RemoveContainer(_ context.Context, pod *api.PodSandbox, c *api.Container) error {
	p.mu.Lock()
	delete(p.createdAt, c.GetId())
	p.mu.Unlock()
	p.cache.Delete(pod.GetUid(), c.GetName())
	klog.V(4).InfoS("NRI RemoveContainer", "pod", pod.GetName(), "container", c.GetName())
	return nil
}

// StopPodSandbox logs pod teardown for lifecycle visibility.
func (p *Plugin) StopPodSandbox(_ context.Context, pod *api.PodSandbox) error {
	klog.V(4).InfoS("NRI StopPodSandbox", "pod", pod.GetName(), "podUID", pod.GetUid())
	return nil
}

// splitEnv splits a "KEY=VALUE" entry. ok is false if there is no '='.
func splitEnv(e string) (key, value string, ok bool) {
	i := strings.IndexByte(e, '=')
	if i < 0 {
		return "", "", false
	}
	return e[:i], e[i+1:], true
}

// lookupEnv finds KEY in a []string of "KEY=VALUE" entries.
func lookupEnv(env []string, key string) (string, bool) {
	prefix := key + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return strings.TrimPrefix(e, prefix), true
		}
	}
	return "", false
}

func filterEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, e := range env {
		for _, p := range envPrefixesOfInterest {
			if strings.HasPrefix(e, p) {
				out = append(out, e)
				break
			}
		}
	}
	return out
}

func filterMounts(mounts []*api.Mount) []string {
	out := make([]string, 0)
	for _, m := range mounts {
		for _, d := range mountDestsOfInterest {
			if strings.HasPrefix(m.GetDestination(), d) {
				out = append(out, m.GetDestination()+" <- "+m.GetSource())
				break
			}
		}
	}
	return out
}
