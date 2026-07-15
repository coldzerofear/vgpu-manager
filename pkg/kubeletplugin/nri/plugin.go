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
	"strings"
	"sync"
	"time"

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
	defaultFailureGracePeriod = 3 * time.Minute
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
}

// Plugin is the in-process NRI plugin.
type Plugin struct {
	stub               stub.Stub
	cache              *Cache
	dryRun             bool
	failureGracePeriod time.Duration

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
		if !ok || podUID == "" {
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

// CreateContainer is the hook that will (in a later stage) inject the partition
// mounts + register env. In DRY-RUN it only observes: it logs the CDI-injected
// env/mounts/pid/state the runtime built (validating that CDI ran first, §12.12
// items 1-2, and that no PID exists yet, item 3), records the intended cache
// entry when the claim-uid env is present, and returns no adjustment.
func (p *Plugin) CreateContainer(_ context.Context, pod *api.PodSandbox, c *api.Container) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	p.mu.Lock()
	p.createdAt[c.GetId()] = time.Now()
	p.mu.Unlock()

	claimUID, hasClaim := lookupEnv(c.GetEnv(), util.ManagerVGpuClaimUid)
	_, hasCompat := lookupEnv(c.GetEnv(), util.ManagerCompatibilityMode)

	var configDir string
	if hasClaim {
		configDir = ConfigDirFor(claimUID, pod.GetUid(), c.GetName())
		// Record for observation even in dry-run; the register path is not yet
		// wired to this cache, so this is purely to validate the mapping.
		p.cache.Set(pod.GetUid(), c.GetName(), Entry{ClaimUID: claimUID, ConfigDir: configDir})
	}

	klog.InfoS("NRI CreateContainer",
		"pod", pod.GetName(), "podUID", pod.GetUid(), "namespace", pod.GetNamespace(),
		"container", c.GetName(), "state", c.GetState().String(), "pid", c.GetPid(),
		"isVGPUContainer", hasCompat, "hasClaimUIDEnv", hasClaim, "claimUID", claimUID,
		"intendedConfigDir", configDir,
		"envOfInterest", filterEnv(c.GetEnv()), "mountsOfInterest", filterMounts(c.GetMounts()),
		"dryRun", p.dryRun)
	return nil, nil, nil
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
