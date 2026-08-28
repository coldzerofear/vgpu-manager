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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
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
		Decorate(devices, &PublishSpec{Endpoint: "10.0.0.7:14833"})
		info, isRemote, err := ParseDevice(&devices[0])
		if err != nil || !isRemote {
			t.Fatalf("expected remote, got isRemote=%v err=%v", isRemote, err)
		}
		if info.Endpoint != "10.0.0.7:14833" || info.UUID != "GPU-a" || info.CUDAVersion.String() != "12.9.0" {
			t.Fatalf("unexpected info: %+v", info)
		}
		if *devices[0].Attributes["type"].StringValue != "vgpu" {
			t.Fatal("pre-existing type attribute was clobbered")
		}
	})

	t.Run("pool selector is exactly the operator predicate (one term, ANDed)", func(t *testing.T) {
		reqs, err := ParseNodeSelector("topology.kubernetes.io/zone=az1,gpu-fabric in (a,b),!isolated,tier!=edge")
		if err != nil {
			t.Fatal(err)
		}
		sel := PoolNodeSelector(reqs)
		if len(sel.NodeSelectorTerms) != 1 {
			t.Fatalf("expected exactly 1 term, got %d", len(sel.NodeSelectorTerms))
		}
		got := map[string]corev1.NodeSelectorRequirement{}
		for _, r := range sel.NodeSelectorTerms[0].MatchExpressions {
			got[r.Key] = r
		}
		if len(got) != 4 {
			t.Fatalf("expected 4 ANDed requirements, got %+v", got)
		}
		if r := got["topology.kubernetes.io/zone"]; r.Operator != corev1.NodeSelectorOpIn || r.Values[0] != "az1" {
			t.Fatalf("zone: %+v", r)
		}
		if r := got["gpu-fabric"]; r.Operator != corev1.NodeSelectorOpIn || len(r.Values) != 2 {
			t.Fatalf("fabric: %+v", r)
		}
		if r := got["isolated"]; r.Operator != corev1.NodeSelectorOpDoesNotExist {
			t.Fatalf("isolated: %+v", r)
		}
		if r := got["tier"]; r.Operator != corev1.NodeSelectorOpNotIn || r.Values[0] != "edge" {
			t.Fatalf("tier: %+v", r)
		}
	})

	t.Run("empty or invalid selector is rejected", func(t *testing.T) {
		for _, bad := range []string{"", "   ", "=x", "a=b=c", "zone>1"} {
			if _, err := ParseNodeSelector(bad); err == nil {
				t.Errorf("%q should be rejected", bad)
			}
		}
	})
}

func TestResolveRemoteDevicesAndPartitions(t *testing.T) {
	slice := func(pool string, devs ...*resourceapi.Device) *resourceapi.ResourceSlice {
		s := &resourceapi.ResourceSlice{Spec: resourceapi.ResourceSliceSpec{Pool: resourceapi.ResourcePool{Name: pool}}}
		for _, d := range devs {
			s.Spec.Devices = append(s.Spec.Devices, *d)
		}
		return s
	}
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{
		resourceapi.ResourceSliceSelectorPoolName: func(obj interface{}) ([]string, error) {
			return []string{obj.(*resourceapi.ResourceSlice).Spec.Pool.Name}, nil
		},
	})
	for i, s := range []*resourceapi.ResourceSlice{
		slice("node-a", remoteDevice("vgpu-0", "GPU-a0", "server-a:14833", "12.9.0"), remoteDevice("vgpu-1", "GPU-a1", "server-a:14833", "12.9.0")),
		slice("node-b", remoteDevice("vgpu-0", "GPU-b0", "server-b:14833", "12.4.0")),
		slice("node-c", &resourceapi.Device{Name: "vgpu-0", Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			AttrAccessMode: {StringValue: strPtr(AccessModeLocal)},
		}}),
	} {
		s.Name = fmt.Sprintf("slice-%d", i)
		if err := indexer.Add(s); err != nil {
			t.Fatal(err)
		}
	}
	d := &InjectDriver{sliceIndexer: indexer}

	claim := func(results ...resourceapi.DeviceRequestAllocationResult) *resourceapi.ResourceClaim {
		return &resourceapi.ResourceClaim{
			Spec: resourceapi.ResourceClaimSpec{Devices: resourceapi.DeviceClaim{Requests: []resourceapi.DeviceRequest{
				{Name: "ctr0", Exactly: &resourceapi.ExactDeviceRequest{}},
				{Name: "ctr1", Exactly: &resourceapi.ExactDeviceRequest{}},
				{Name: "fa", FirstAvailable: []resourceapi.DeviceSubRequest{{Name: "big"}, {Name: "small"}}},
			}}},
			Status: resourceapi.ResourceClaimStatus{Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{Results: results},
			}},
		}
	}
	result := func(request, pool, device string) resourceapi.DeviceRequestAllocationResult {
		return resourceapi.DeviceRequestAllocationResult{Request: request, Driver: "manager.nvidia.com", Pool: pool, Device: device}
	}

	t.Run("resolves devices, folds subrequests, partitions by resolver key", func(t *testing.T) {
		c := claim(
			result("ctr0", "node-b", "vgpu-0"),
			result("ctr1", "node-a", "vgpu-0"),
			result("fa/small", "node-a", "vgpu-1"),
			resourceapi.DeviceRequestAllocationResult{Request: "x", Driver: "other.driver", Pool: "p", Device: "d"}, // ignored
		)
		devices, err := d.resolveRemoteDevices(c)
		if err != nil {
			t.Fatal(err)
		}
		if len(devices) != 3 || devices[2].mainRequest != "fa" || devices[2].index != 2 {
			t.Fatalf("unexpected devices: %+v", devices)
		}
		if cudaFloor(devices).String() != "12.4.0" {
			t.Fatalf("expected CUDA floor 12.4.0, got %s", cudaFloor(devices))
		}

		// ctr1 and fa share one container -> one partition; ctr0 is alone.
		parts := buildPartitions(devices, map[string]string{"ctr1": "pod-abc-c1", "fa": "pod-abc-c1"})
		if len(parts) != 2 {
			t.Fatalf("expected 2 partitions, got %d", len(parts))
		}
		if parts[0].key != "ctr0" || len(parts[0].results) != 1 || parts[0].endpoints[0] != "server-b:14833" {
			t.Fatalf("fallback partition: %+v", parts[0])
		}
		if parts[1].key != "pod-abc-c1" || len(parts[1].results) != 2 || len(parts[1].endpoints) != 1 ||
			len(parts[1].requests) != 2 || parts[1].requests[0] != "ctr1" || parts[1].requests[1] != "fa" {
			t.Fatalf("resolved partition: %+v", parts[1])
		}
	})

	t.Run("unknown pool/device and local-only devices fail", func(t *testing.T) {
		for _, c := range []*resourceapi.ResourceClaim{
			claim(result("ctr0", "node-x", "vgpu-0")),
			claim(result("ctr0", "node-a", "vgpu-9")),
			claim(result("ctr0", "node-c", "vgpu-0")),
			claim(result("nope", "node-a", "vgpu-0")), // request not in spec
		} {
			if _, err := d.resolveRemoteDevices(c); err == nil {
				t.Fatalf("expected error for %+v", c.Status.Allocation.Devices.Results[0])
			}
		}
	})
}

func TestSessionHelpers(t *testing.T) {
	tok, err := NewSessionToken()
	if err != nil || len(tok) != 32 {
		t.Fatalf("token: %q %v", tok, err)
	}
	k1, k2 := SessionAnnotationKey("pod-abc-ctr0"), SessionAnnotationKey("pod-abc-ctr1")
	if k1 == k2 || len(k1) > 63+len("manager.nvidia.com/") || !strings.HasPrefix(k1, "manager.nvidia.com/session-") {
		t.Fatalf("annotation keys: %s %s", k1, k2)
	}
	if got := cdiDeviceID(resultDevice{result: resourceapi.DeviceRequestAllocationResult{Request: "fa/small", Device: "vgpu-1", ShareID: func() *types.UID { u := types.UID("s1"); return &u }()}}, 0); got != "fa-small-vgpu-1-share-s1" {
		t.Fatalf("cdi id: %s", got)
	}
	c := &resourceapi.ResourceClaim{Spec: resourceapi.ResourceClaimSpec{Devices: resourceapi.DeviceClaim{Requests: []resourceapi.DeviceRequest{
		{Name: "a", Exactly: &resourceapi.ExactDeviceRequest{}}, {Name: "b", Exactly: &resourceapi.ExactDeviceRequest{}},
	}}}}
	results := []resourceapi.DeviceRequestAllocationResult{{Request: "a"}, {Request: "b"}}
	if got := FilterResultsByRequests(c, results, []string{"b"}); len(got) != 1 || got[0].Request != "b" {
		t.Fatalf("filter: %+v", got)
	}
	if got := FilterResultsByRequests(c, results, nil); len(got) != 2 {
		t.Fatalf("empty filter must keep everything: %+v", got)
	}
}

func TestSelectArtifact(t *testing.T) {
	dir := t.TempDir()
	for _, v := range []string{"11.8", "12.4.1", "12.9.1", "13.1.0", "not-a-version"} {
		if err := os.Mkdir(filepath.Join(dir, v), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A control-library file next to the version dirs must be ignored.
	if err := os.WriteFile(filepath.Join(dir, "libvgpu-control.so.1.0.0"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	mustVer := func(s string) *DeviceInfo {
		info, _, err := ParseDevice(remoteDevice("d", "GPU-x", "e:1", s))
		if err != nil {
			t.Fatal(err)
		}
		return info
	}

	t.Run("picks highest version at or below server ceiling, host path for the mount", func(t *testing.T) {
		sel, err := selectArtifact(dir, "/host"+dir, mustVer("12.9.0").CUDAVersion)
		if err != nil {
			t.Fatal(err)
		}
		// 12.9.1 > 12.9.0, so 12.4.1 is the best admissible artifact.
		if sel.Name != "12.4.1" {
			t.Fatalf("expected 12.4.1, got %s", sel.Name)
		}
		if sel.HostDir != filepath.Join("/host"+dir, "12.4.1") {
			t.Fatalf("host dir must derive from the host artifacts dir: %s", sel.HostDir)
		}
		if sel.ContainerDir != "/etc/vgpu-manager/driver" || sel.LibDir != sel.ContainerDir {
			t.Fatalf("unexpected container paths: %+v", sel)
		}
	})

	t.Run("no admissible version errors", func(t *testing.T) {
		if _, err := selectArtifact(dir, dir, mustVer("11.7.0").CUDAVersion); err == nil {
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

func TestPreparedCheckpointRoundTrip(t *testing.T) {
	dir := t.TempDir()
	d := &InjectDriver{config: InjectConfig{PluginDataDirectoryPath: dir}, prepared: map[string]*preparedClaim{}}
	claim := &resourceapi.ResourceClaim{}
	claim.Name, claim.Namespace, claim.UID = "c", "ns", "uid-1"
	d.recordPrepared(claim, nil)
	if !d.isClaimPrepared("uid-1") {
		t.Fatal("recorded claim must be prepared")
	}
	data, err := os.ReadFile(filepath.Join(dir, preparedCheckpointFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"uid-1"`) || !strings.Contains(string(data), `"name":"c"`) {
		t.Fatalf("checkpoint content: %s", data)
	}
	d.forgetPrepared("uid-1")
	if d.isClaimPrepared("uid-1") {
		t.Fatal("forgotten claim must not be prepared")
	}
	if data, _ := os.ReadFile(filepath.Join(dir, preparedCheckpointFile)); strings.Contains(string(data), "uid-1") {
		t.Fatalf("checkpoint must drop forgotten claims: %s", data)
	}
}
