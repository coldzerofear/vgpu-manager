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

package nri

import (
	"context"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"testing"
	"time"

	"github.com/containerd/nri/pkg/api"
)

const claimUIDEnv = "MANAGER_VGPU_CLAIM_UID"

func testPlugin(prepared map[string]bool, resolve func(ctx context.Context, claimUID, podName, podNamespace, podUID, cn string) (*Injection, error)) *Plugin {
	return &Plugin{
		cache:           NewCache(),
		createdAt:       make(map[string]time.Time),
		isClaimPrepared: func(_ context.Context, uid string) bool { return prepared[uid] },
		resolveMounts:   resolve,
	}
}

func vgpuInjection(_ context.Context, claimUID, podName, podNamespace, podUID, cn string) (*Injection, error) {
	return &Injection{
		ConfigDir: ConfigDirFor(util.ManagerRootPath, claimUID, podUID, cn),
		Env: []string{
			"VGPU_POD_NAME=" + podName,
			"VGPU_POD_NAMESPACE=" + podNamespace,
			"VGPU_POD_UID=" + podUID,
			"VGPU_CONTAINER_NAME=" + cn,
		},
		Mounts: []Mount{
			{ContainerPath: "/etc/vgpu-manager/config", HostPath: "/host/config", Options: []string{"rw", "bind"}},
		},
	}, nil
}

func TestCreateContainer_InjectsForPreparedClaim(t *testing.T) {
	p := testPlugin(map[string]bool{"claim-1": true}, vgpuInjection)
	pod := &api.PodSandbox{Uid: "pod-1", Name: "pod"}
	ctr := &api.Container{Id: "c1", Name: "app", Env: []string{claimUIDEnv + "=claim-1"}}

	adjust, _, err := p.CreateContainer(context.Background(), pod, ctr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if adjust == nil {
		t.Fatal("expected an adjustment, got nil")
	}
	if len(adjust.Mounts) != 1 {
		t.Fatalf("expected 1 mount, got %d", len(adjust.Mounts))
	}
	if len(adjust.Env) != 4 {
		t.Fatalf("expected 2 env, got %d", len(adjust.Env))
	}
	if _, ok := p.cache.Get("pod-1", "app"); !ok {
		t.Fatal("expected cache entry for injected container")
	}
}

func TestCreateContainer_RejectsUnpreparedClaim(t *testing.T) {
	// Spoofed env: the claim UID is not prepared on this node (§12.12.1).
	p := testPlugin(map[string]bool{"real-claim": true}, vgpuInjection)
	pod := &api.PodSandbox{Uid: "attacker", Name: "pod"}
	ctr := &api.Container{Id: "c1", Name: "app", Env: []string{claimUIDEnv + "=someone-elses-claim"}}

	// Strict enforcement: the hook fails the container rather than starting it
	// without isolation, so containerd reports a CreateContainerError naming us.
	adjust, _, err := p.CreateContainer(context.Background(), pod, ctr)
	if err == nil {
		t.Fatal("expected an error for an unprepared (spoofed) claim")
	}
	if adjust != nil {
		t.Fatal("expected no adjustment for unprepared claim")
	}
	if _, ok := p.cache.Get("attacker", "app"); ok {
		t.Fatal("must not cache an unprepared (spoofed) claim")
	}
}

func TestCreateContainer_NoOpInjectionIsNotAnError(t *testing.T) {
	// A container that references the claim but has nothing to inject (see
	// Config.ResolveMounts) must start normally — this is the one nil-injection
	// case that is a legitimate outcome rather than a failure.
	noop := func(context.Context, string, string, string, string, string) (*Injection, error) {
		return nil, nil
	}
	p := testPlugin(map[string]bool{"claim-1": true}, noop)
	pod := &api.PodSandbox{Uid: "pod-1", Name: "pod"}
	ctr := &api.Container{Id: "c1", Name: "app", Env: []string{claimUIDEnv + "=claim-1"}}

	adjust, _, err := p.CreateContainer(context.Background(), pod, ctr)
	if err != nil {
		t.Fatalf("a no-op injection must not fail the container: %v", err)
	}
	if adjust != nil {
		t.Fatal("expected no adjustment")
	}
}

func TestReadinessTracksSessionLifecycle(t *testing.T) {
	p := testPlugin(nil, vgpuInjection)
	if p.Ready() {
		t.Fatal("a fresh plugin must not report ready")
	}
	if err := p.WaitReady(context.Background(), 10*time.Millisecond); err == nil {
		t.Fatal("WaitReady must time out while the plugin is not ready")
	}

	if _, err := p.Synchronize(context.Background(), nil, nil); err != nil {
		t.Fatalf("Synchronize: %v", err)
	}
	if !p.Ready() {
		t.Fatal("Synchronize must make the plugin ready")
	}
	if !p.cache.Synced() {
		t.Fatal("Synchronize must mark the cache synced")
	}
	if err := p.WaitReady(context.Background(), time.Second); err != nil {
		t.Fatalf("WaitReady on a ready plugin: %v", err)
	}

	// Disconnect: readiness drops and the cache stops claiming to be synced, so
	// the register resolver retries instead of answering from a stale cache.
	p.setReady(false)
	if p.Ready() {
		t.Fatal("a disconnected plugin must not report ready")
	}
	if p.cache.Synced() {
		t.Fatal("a disconnect must unsync the cache")
	}
	if err := p.WaitReady(context.Background(), 10*time.Millisecond); err == nil {
		t.Fatal("WaitReady must block again after a disconnect")
	}

	// Reconnect: the next Synchronize makes it ready again.
	if _, err := p.Synchronize(context.Background(), nil, nil); err != nil {
		t.Fatalf("Synchronize after reconnect: %v", err)
	}
	if !p.Ready() || !p.cache.Synced() {
		t.Fatal("reconnect must restore readiness and cache sync")
	}
}

func TestCreateContainer_SkipsNonVGPUContainer(t *testing.T) {
	p := testPlugin(map[string]bool{"claim-1": true}, vgpuInjection)
	pod := &api.PodSandbox{Uid: "pod-1", Name: "pod"}
	ctr := &api.Container{Id: "c1", Name: "plain", Env: []string{"PATH=/usr/bin"}}

	adjust, _, err := p.CreateContainer(context.Background(), pod, ctr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if adjust != nil {
		t.Fatal("expected no adjustment for a non-vGPU container")
	}
}

func TestCreateContainer_ObserveOnlyWhenNoResolver(t *testing.T) {
	// No resolveMounts wired => observe-only, never injects.
	p := &Plugin{cache: NewCache(), createdAt: make(map[string]time.Time)}
	pod := &api.PodSandbox{Uid: "pod-1", Name: "pod"}
	ctr := &api.Container{Id: "c1", Name: "app", Env: []string{claimUIDEnv + "=claim-1"}}

	adjust, _, err := p.CreateContainer(context.Background(), pod, ctr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if adjust != nil {
		t.Fatal("observe-only mode must not inject")
	}
	if _, ok := p.cache.Get("pod-1", "app"); ok {
		t.Fatal("observe-only mode must not populate the cache")
	}
}

func TestCacheReadinessGate(t *testing.T) {
	c := NewCache()
	if c.Synced() {
		t.Fatal("a fresh cache must not report synced")
	}
	c.Replace(map[string]Entry{Key("pod-1", "app"): {ClaimUID: "claim-1", ConfigDir: "/d"}})
	if !c.Synced() {
		t.Fatal("cache must report synced after Replace")
	}
	if e, ok := c.Get("pod-1", "app"); !ok || e.ClaimUID != "claim-1" {
		t.Fatalf("unexpected entry after Replace: %+v ok=%v", e, ok)
	}
}

func TestConfigDirFor(t *testing.T) {
	got := ConfigDirFor(util.ManagerRootPath, "claim-1", "pod-1", "app")
	want := "/etc/vgpu-manager/claims/claim-1/pod-1_app/config"
	if got != want {
		t.Fatalf("ConfigDirFor = %q, want %q", got, want)
	}
}

func TestValidatePluginIdx(t *testing.T) {
	for _, idx := range []string{"00", "05", "99"} {
		if err := ValidatePluginIdx(idx); err != nil {
			t.Errorf("ValidatePluginIdx(%q) = %v, want nil", idx, err)
		}
	}
	for _, idx := range []string{"", "0", "5", "100", "0a", "ab"} {
		if err := ValidatePluginIdx(idx); err == nil {
			t.Errorf("ValidatePluginIdx(%q) = nil, want error", idx)
		}
	}
}

func TestSplitEnv(t *testing.T) {
	if k, v, ok := splitEnv("A=b=c"); !ok || k != "A" || v != "b=c" {
		t.Fatalf("splitEnv(A=b=c) = %q,%q,%v", k, v, ok)
	}
	if _, _, ok := splitEnv("noequals"); ok {
		t.Fatal("splitEnv without '=' must return ok=false")
	}
}
