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
	"os"
	"path/filepath"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
)

func strPtr(s string) *string { return &s }

func remoteDevice(name, uuid, endpoint, cudaVersion string) *resourceapi.Device {
	dev := &resourceapi.Device{
		Name: name,
		Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			AttrAccessMode: {StringValue: strPtr(AccessModeRemote)},
			AttrUUID:       {StringValue: strPtr(uuid)},
			AttrEndpoint:   {StringValue: strPtr(endpoint)},
		},
	}
	if cudaVersion != "" {
		dev.Attributes[AttrCUDADriverVersion] = resourceapi.DeviceAttribute{VersionValue: strPtr(cudaVersion)}
	}
	return dev
}

func TestParseDevice(t *testing.T) {
	t.Run("local-only device is not remote", func(t *testing.T) {
		dev := &resourceapi.Device{
			Name: "vgpu-0",
			Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				"type":         {StringValue: strPtr("vgpu")},
				AttrAccessMode: {StringValue: strPtr(AccessModeLocal)},
			},
		}
		_, isRemote, err := ParseDevice(dev)
		if err != nil || isRemote {
			t.Fatalf("expected (not remote, nil), got isRemote=%v err=%v", isRemote, err)
		}
	})

	t.Run("device without accessMode is not remote", func(t *testing.T) {
		_, isRemote, err := ParseDevice(&resourceapi.Device{Name: "gpu-0"})
		if err != nil || isRemote {
			t.Fatalf("expected (not remote, nil), got isRemote=%v err=%v", isRemote, err)
		}
	})

	t.Run("well-formed remote device", func(t *testing.T) {
		info, isRemote, err := ParseDevice(remoteDevice("vgpu-0", "GPU-abc", "10.0.0.1:14833", "12.9.0"))
		if err != nil || !isRemote {
			t.Fatalf("expected remote, got isRemote=%v err=%v", isRemote, err)
		}
		if info.UUID != "GPU-abc" || info.Endpoint != "10.0.0.1:14833" || info.CUDAVersion.String() != "12.9.0" {
			t.Fatalf("unexpected info: %+v", info)
		}
	})

	t.Run("remote device missing endpoint fails", func(t *testing.T) {
		_, isRemote, err := ParseDevice(remoteDevice("vgpu-0", "GPU-abc", "", "12.9.0"))
		if !isRemote || err == nil {
			t.Fatalf("expected (remote, error), got isRemote=%v err=%v", isRemote, err)
		}
	})

	t.Run("remote device missing cudaDriverVersion fails", func(t *testing.T) {
		_, isRemote, err := ParseDevice(remoteDevice("vgpu-0", "GPU-abc", "10.0.0.1:14833", ""))
		if !isRemote || err == nil {
			t.Fatalf("expected (remote, error), got isRemote=%v err=%v", isRemote, err)
		}
	})
}

func TestDecorateAndSelector(t *testing.T) {
	newDevices := func() []resourceapi.Device {
		return []resourceapi.Device{
			{Name: "vgpu-0", Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				"type":                {StringValue: strPtr("vgpu")},
				AttrUUID:              {StringValue: strPtr("GPU-a")},
				AttrCUDADriverVersion: {VersionValue: strPtr("12.9.0")},
			}},
			{Name: "gpu-1"}, // nil attribute map must be tolerated
		}
	}

	t.Run("local-only stamps accessMode=local and nothing else", func(t *testing.T) {
		devices := newDevices()
		Decorate(devices, nil)
		for _, d := range devices {
			if got := *d.Attributes[AttrAccessMode].StringValue; got != AccessModeLocal {
				t.Fatalf("%s: accessMode=%q", d.Name, got)
			}
			if _, ok := d.Attributes[AttrEndpoint]; ok {
				t.Fatalf("%s: local device must not carry an endpoint", d.Name)
			}
		}
	})

	t.Run("remote decorated device round-trips through ParseDevice", func(t *testing.T) {
		devices := newDevices()
		Decorate(devices, &PublishSpec{Endpoint: "10.0.0.7:14833", NetZone: "zone-a"})
		info, isRemote, err := ParseDevice(&devices[0])
		if err != nil || !isRemote {
			t.Fatalf("expected remote, got isRemote=%v err=%v", isRemote, err)
		}
		if info.Endpoint != "10.0.0.7:14833" || info.NetZone != "zone-a" || info.UUID != "GPU-a" || info.CUDAVersion.String() != "12.9.0" {
			t.Fatalf("unexpected info: %+v", info)
		}
		if *devices[0].Attributes["type"].StringValue != "vgpu" {
			t.Fatal("pre-existing type attribute was clobbered")
		}
	})

	t.Run("pool selector covers the GPU node itself OR reachable nodes", func(t *testing.T) {
		sel := PoolNodeSelector("gpu-node-1", "zone-a")
		if len(sel.NodeSelectorTerms) != 2 {
			t.Fatalf("expected 2 OR-terms, got %d", len(sel.NodeSelectorTerms))
		}
		first := sel.NodeSelectorTerms[0].MatchExpressions[0]
		if first.Key != LabelHostname || first.Values[0] != "gpu-node-1" {
			t.Fatalf("first term must pin the GPU node: %+v", first)
		}
		second := sel.NodeSelectorTerms[1].MatchExpressions[0]
		if second.Key != "vgpu-manager.io/net-zone.zone-a" || second.Values[0] != LabelValueReachable {
			t.Fatalf("second term must match the reachability label: %+v", second)
		}
	})
}

func TestResolveRemoteAllocation(t *testing.T) {
	index := map[string]map[string]*resourceapi.Device{
		"node-a": {
			"vgpu-0": remoteDevice("vgpu-0", "GPU-a0", "server-a:14833", "12.9.0"),
			"vgpu-1": remoteDevice("vgpu-1", "GPU-a1", "server-a:14833", "12.9.0"),
		},
		"node-b": {
			"vgpu-0": remoteDevice("vgpu-0", "GPU-b0", "server-b:14833", "12.4.0"),
		},
		"node-c": {
			"vgpu-0": {Name: "vgpu-0", Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				AttrAccessMode: {StringValue: strPtr(AccessModeLocal)},
			}},
		},
	}
	result := func(pool, device string) resourceapi.DeviceRequestAllocationResult {
		return resourceapi.DeviceRequestAllocationResult{
			Request: "req", Driver: "manager.nvidia.com", Pool: pool, Device: device,
		}
	}

	t.Run("dedup preserves first-appearance order and floors CUDA", func(t *testing.T) {
		endpoints, minCUDA, err := resolveRemoteAllocation([]resourceapi.DeviceRequestAllocationResult{
			result("node-b", "vgpu-0"),
			result("node-a", "vgpu-0"),
			result("node-a", "vgpu-1"),
		}, index)
		if err != nil {
			t.Fatal(err)
		}
		if len(endpoints) != 2 || endpoints[0] != "server-b:14833" || endpoints[1] != "server-a:14833" {
			t.Fatalf("unexpected endpoint order: %v", endpoints)
		}
		if minCUDA.String() != "12.4.0" {
			t.Fatalf("expected CUDA floor 12.4.0, got %s", minCUDA)
		}
	})

	t.Run("unknown device errors", func(t *testing.T) {
		if _, _, err := resolveRemoteAllocation([]resourceapi.DeviceRequestAllocationResult{result("node-x", "vgpu-9")}, index); err == nil {
			t.Fatal("expected error for unpublished device")
		}
	})

	t.Run("local-only device errors in inject mode", func(t *testing.T) {
		if _, _, err := resolveRemoteAllocation([]resourceapi.DeviceRequestAllocationResult{result("node-c", "vgpu-0")}, index); err == nil {
			t.Fatal("expected error for accessMode=local device")
		}
	})
}

func TestSelectArtifact(t *testing.T) {
	dir := t.TempDir()
	for _, v := range []string{"11.8", "12.4.1", "12.9.1", "13.1.0", "not-a-version"} {
		if err := os.Mkdir(filepath.Join(dir, v), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustVer := func(s string) *DeviceInfo {
		info, _, err := ParseDevice(remoteDevice("d", "GPU-x", "e:1", s))
		if err != nil {
			t.Fatal(err)
		}
		return info
	}

	t.Run("picks highest version at or below server ceiling", func(t *testing.T) {
		sel, err := selectArtifact(dir, "/opt/vgpu/lupine", mustVer("12.9.0").CUDAVersion)
		if err != nil {
			t.Fatal(err)
		}
		// 12.9.1 > 12.9.0, so 12.4.1 is the best admissible artifact.
		if sel.Name != "12.4.1" {
			t.Fatalf("expected 12.4.1, got %s", sel.Name)
		}
		if sel.ContainerDir != "/opt/vgpu/lupine/12.4.1" || sel.LibDir != sel.ContainerDir {
			t.Fatalf("unexpected paths: %+v", sel)
		}
	})

	t.Run("prefers lib subdirectory when present", func(t *testing.T) {
		if err := os.Mkdir(filepath.Join(dir, "12.4.1", "lib"), 0o755); err != nil {
			t.Fatal(err)
		}
		sel, err := selectArtifact(dir, "/opt/vgpu/lupine", mustVer("12.4.1").CUDAVersion)
		if err != nil {
			t.Fatal(err)
		}
		if sel.LibDir != "/opt/vgpu/lupine/12.4.1/lib" {
			t.Fatalf("expected lib subdir, got %s", sel.LibDir)
		}
	})

	t.Run("no admissible version errors", func(t *testing.T) {
		if _, err := selectArtifact(dir, "/opt/vgpu/lupine", mustVer("11.7.0").CUDAVersion); err == nil {
			t.Fatal("expected error when every artifact is newer than the server")
		}
	})
}

func TestAgentAddr(t *testing.T) {
	cases := map[string]string{
		"10.0.0.7":                   "10.0.0.7:14834",
		"10.0.0.7:14833":             "10.0.0.7:14834",
		"http://gpu-a:14833":         "gpu-a:14834",
		"https://gpu-a.example.com":  "gpu-a.example.com:14834",
		"gpu-a.zone.vgpu.internal:1": "gpu-a.zone.vgpu.internal:14834",
	}
	for in, want := range cases {
		if got := AgentAddr(in, 14834); got != want {
			t.Errorf("AgentAddr(%q) = %q, want %q", in, got, want)
		}
	}
}
