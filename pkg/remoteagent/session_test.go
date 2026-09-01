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

package remoteagent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	vgpuconfig "github.com/coldzerofear/vgpu-manager/pkg/config/vgpu"
	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin/remote"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
)

const (
	testDriver = "manager.nvidia.com"
	testNode   = "gpu-node-1"
)

func testSlice() *resourceapi.ResourceSlice {
	return &resourceapi.ResourceSlice{
		Spec: resourceapi.ResourceSliceSpec{
			Driver: testDriver,
			Pool:   resourceapi.ResourcePool{Name: testNode},
			Devices: []resourceapi.Device{
				{
					// minor 2: slot index must follow the host index, not the order
					// in which devices are allocated or listed.
					Name: "vgpu-2",
					Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
						remote.AttrAccessMode:        {StringValue: ptr.To(remote.AccessModeRemote)},
						remote.AttrUUID:              {StringValue: ptr.To("gpu-aaaa")}, // published lowercase
						remote.AttrMinor:             {IntValue: ptr.To[int64](2)},
						remote.AttrCUDADriverVersion: {VersionValue: ptr.To("12.9.0")},
						remote.AttrDriverVersion:     {VersionValue: ptr.To("575.57.8")},
					},
					Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
						remote.CapacityCores:  {Value: resource.MustParse("100")},
						remote.CapacityMemory: {Value: resource.MustParse("24Gi")},
					},
				},
				{
					Name: "vgpu-0",
					Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
						remote.AttrAccessMode: {StringValue: ptr.To(remote.AccessModeRemote)},
						remote.AttrUUID:       {StringValue: ptr.To("gpu-bbbb")},
						remote.AttrMinor:      {IntValue: ptr.To[int64](0)},
					},
					Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
						remote.CapacityCores:  {Value: resource.MustParse("100")},
						remote.CapacityMemory: {Value: resource.MustParse("24Gi")},
					},
				},
				{
					// local-only device (gate off elsewhere / mixed publish): skipped
					Name: "vgpu-9",
					Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
						remote.AttrAccessMode: {StringValue: ptr.To(remote.AccessModeLocal)},
						remote.AttrUUID:       {StringValue: ptr.To("gpu-cccc")},
						remote.AttrMinor:      {IntValue: ptr.To[int64](9)},
					},
				},
			},
		},
	}
}
func testClaim(uid string, results ...resourceapi.DeviceRequestAllocationResult) *resourceapi.ResourceClaim {
	return &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns", UID: types.UID(uid)},
		Status: resourceapi.ResourceClaimStatus{
			Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{Results: results},
			},
		},
	}
}

func result(pool, dev string, cores, mem string) resourceapi.DeviceRequestAllocationResult {
	r := resourceapi.DeviceRequestAllocationResult{Request: "req", Driver: testDriver, Pool: pool, Device: dev}
	if cores != "" || mem != "" {
		r.ConsumedCapacity = map[resourceapi.QualifiedName]resource.Quantity{}
		if cores != "" {
			r.ConsumedCapacity[remote.CapacityCores] = resource.MustParse(cores)
		}
		if mem != "" {
			r.ConsumedCapacity[remote.CapacityMemory] = resource.MustParse(mem)
		}
	}
	return r
}

func TestNodeRemoteDevicesFromSlices(t *testing.T) {
	nd := NodeRemoteDevicesFromSlices([]*resourceapi.ResourceSlice{testSlice()})
	if len(nd.Devices) != 2 {
		t.Fatalf("expected 2 remote devices (local one skipped), got %d", len(nd.Devices))
	}
	if d := nd.Devices["vgpu-2"]; d.UUID != "GPU-aaaa" || d.Minor != 2 || d.MemoryMiB != 24*1024 || d.Cores != 100 {
		t.Fatalf("unexpected device: %+v", d)
	}
	if nd.CudaVersionString() != "12.9.0" || nd.DriverVersion == nil || nd.DriverVersion.Original() != "575.57.8" {
		t.Fatalf("unexpected versions: cuda=%q driver=%v", nd.CudaVersionString(), nd.DriverVersion)
	}
	if empty := NodeRemoteDevicesFromSlices(nil); empty.CudaVersionString() != "" || len(empty.Devices) != 0 {
		t.Fatal("empty snapshot must be empty and nil-safe")
	}
}
func TestValidateToken(t *testing.T) {
	for _, ok := range []string{"a", "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0", "tok.en_1"} {
		if err := validateToken(ok); err != nil {
			t.Errorf("%q should be valid: %v", ok, err)
		}
	}
	for _, bad := range []string{"", ".", "..", "../x", "a/b", ".hidden", "x y", string(make([]byte, 200))} {
		if err := validateToken(bad); err == nil {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

func TestMaterialize(t *testing.T) {
	base := t.TempDir()
	store := NewSessionStore(Config{SessionBase: base, NodeName: testNode, DriverName: testDriver})
	if err := store.Prepare(); err != nil {
		t.Fatal(err)
	}
	nd := NodeRemoteDevicesFromSlices([]*resourceapi.ResourceSlice{testSlice()})

	claim := testClaim("uid-1",
		result("other-node", "vgpu-0", "", ""), // foreign pool, ignored
		result(testNode, "vgpu-2", "50", "4Gi"),
		result(testNode, "vgpu-0", "", ""), // whole device: no limits
	)
	if err := store.Materialize("uid-1", claim, nd, nil); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(base, "uid-1")
	for _, p := range []string{"config/vgpu.config", "pids.config", ".vgpu_lock", ".vmem_node", ".sm_node", ".claim-uid"} {
		if _, err := os.Stat(filepath.Join(root, p)); err != nil {
			t.Fatalf("missing %s: %v", p, err)
		}
	}

	data, err := vgpuconfig.NewMmapResourceData(filepath.Join(root, "config", "vgpu.config"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = data.Close() }()
	cfg := data.GetResource()
	if cfg.CompatibilityMode != int32(util.SessionMode) {
		t.Fatalf("compatibility mode = %d, want %d", cfg.CompatibilityMode, util.SessionMode)
	}
	if cfg.CudaVersion.Major != 12 || cfg.CudaVersion.Minor != 9 {
		t.Fatalf("cuda version = %+v", cfg.CudaVersion)
	}
	// Slot = host index (minor): vgpu-2 (limited) lands in slot 2, vgpu-0
	// (whole device) in slot 0, slot 1 stays inactive.
	d2 := cfg.Devices[2]
	if d2.Activate != 1 || string(trimZero(d2.UUID[:])) != "GPU-aaaa" {
		t.Fatalf("slot 2 = %+v", d2)
	}
	if d2.MemoryLimit != 1 || d2.TotalMemory != 4<<30 || d2.CoreLimit != 1 || d2.HardLimit != 1 || d2.HardCore != 50 {
		t.Fatalf("slot 2 limits = mem(%d,%d) core(%d,%d,%d)", d2.MemoryLimit, d2.TotalMemory, d2.CoreLimit, d2.HardLimit, d2.HardCore)
	}
	d0 := cfg.Devices[0]
	if d0.Activate != 1 || string(trimZero(d0.UUID[:])) != "GPU-bbbb" || d0.MemoryLimit != 0 || d0.CoreLimit != 0 {
		t.Fatalf("slot 0 = %+v", d0)
	}
	if cfg.Devices[1].Activate != 0 {
		t.Fatal("slot 1 must be inactive")
	}
	// Idempotent for the same claim; refused for a different claim.
	if err := store.Materialize("uid-1", claim, nd, nil); err != nil {
		t.Fatalf("second materialize must be a no-op: %v", err)
	}
	if err := store.Materialize("uid-1", testClaim("uid-2", result(testNode, "vgpu-0", "", "")), nd, nil); err == nil {
		t.Fatal("token reuse by another claim must be refused")
	}

	// Errors: unknown device, nothing on this pool, bad token.
	if err := store.Materialize("t2", testClaim("u", result(testNode, "vgpu-9", "", "")), nd, nil); err == nil {
		t.Fatal("unknown device must fail")
	}
	if err := store.Materialize("t3", testClaim("u", result("elsewhere", "vgpu-0", "", "")), nd, nil); err == nil {
		t.Fatal("claim with nothing on this pool must fail")
	}
	if err := store.Materialize("../t", claim, nd, nil); err == nil {
		t.Fatal("bad token must fail")
	}

	entries, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Token != "uid-1" || entries[0].ClaimUID != "uid-1" {
		t.Fatalf("unexpected entries: %+v", entries)
	}
	if err := store.Remove("uid-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("session dir should be gone")
	}
	if err := store.Remove("uid-1"); err != nil {
		t.Fatalf("remove must be idempotent: %v", err)
	}
}

func trimZero(b []byte) []byte {
	for i, c := range b {
		if c == 0 {
			return b[:i]
		}
	}
	return b
}

func TestMaterializePartitionFilterAndMerge(t *testing.T) {
	base := t.TempDir()
	store := NewSessionStore(Config{SessionBase: base, NodeName: testNode, DriverName: testDriver})
	if err := store.Prepare(); err != nil {
		t.Fatal(err)
	}
	nd := NodeRemoteDevicesFromSlices([]*resourceapi.ResourceSlice{testSlice()})

	// Two requests: "c0" -> vgpu-2 (50 cores/4Gi), "c1" -> vgpu-0 and, via a
	// duplicate share, vgpu-0 again with a bigger slice.
	claim := testClaim("uid-p",
		result(testNode, "vgpu-2", "50", "4Gi"),
		result(testNode, "vgpu-0", "20", "2Gi"),
		result(testNode, "vgpu-0", "60", "8Gi"),
	)
	claim.Spec.Devices.Requests = []resourceapi.DeviceRequest{
		{Name: "c0", Exactly: &resourceapi.ExactDeviceRequest{}},
		{Name: "c1", Exactly: &resourceapi.ExactDeviceRequest{}},
	}
	claim.Status.Allocation.Devices.Results[0].Request = "c0"
	claim.Status.Allocation.Devices.Results[1].Request = "c1"
	claim.Status.Allocation.Devices.Results[2].Request = "c1"

	if err := store.Materialize("tok-c1", claim, nd, []string{"c1"}); err != nil {
		t.Fatal(err)
	}
	data, err := vgpuconfig.NewMmapResourceData(filepath.Join(base, "tok-c1", "config", "vgpu.config"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = data.Close() }()
	cfg := data.GetResource()
	if cfg.Devices[2].Activate != 0 {
		t.Fatal("request c0's device must not be in partition c1's session")
	}
	d0 := cfg.Devices[0]
	if d0.Activate != 1 || d0.HardCore != 60 || d0.TotalMemory != 8<<30 {
		t.Fatalf("duplicate shares must merge to the larger one: %+v", d0)
	}

	// A partition whose requests have nothing on this node is an error.
	if err := store.Materialize("tok-none", claim, nd, []string{"nope"}); err == nil {
		t.Fatal("expected error for a partition with no devices on this node")
	}
}

func TestSessionIndexAndRestore(t *testing.T) {
	base := t.TempDir()
	cfg := Config{SessionBase: base, NodeName: testNode, DriverName: testDriver}
	store := NewSessionStore(cfg)
	if err := store.Prepare(); err != nil {
		t.Fatal(err)
	}
	nd := NodeRemoteDevicesFromSlices([]*resourceapi.ResourceSlice{testSlice()})
	for _, tok := range []string{"t1", "t2"} {
		if err := store.Materialize(tok, testClaim("uid-x", result(testNode, "vgpu-0", "", "")), nd, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Materialize("t3", testClaim("uid-y", result(testNode, "vgpu-2", "", "")), nd, nil); err != nil {
		t.Fatal(err)
	}
	if got := store.TokensOfClaim("uid-x"); len(got) != 2 || got[0] != "t1" || got[1] != "t2" {
		t.Fatalf("index for uid-x: %v", got)
	}

	// A fresh store over the same directory rebuilds the index from markers.
	again := NewSessionStore(cfg)
	if err := again.Prepare(); err != nil {
		t.Fatal(err)
	}
	if got := again.TokensOfClaim("uid-y"); len(got) != 1 || got[0] != "t3" {
		t.Fatalf("restored index for uid-y: %v", got)
	}
	if err := again.Remove("t3"); err != nil {
		t.Fatal(err)
	}
	if got := again.TokensOfClaim("uid-y"); len(got) != 0 {
		t.Fatalf("index must drop removed sessions: %v", got)
	}
}

func TestTrimClaim(t *testing.T) {
	full := testClaim("uid-t",
		result(testNode, "vgpu-0", "50", "4Gi"),
		result("other-node", "vgpu-0", "", ""),
	)
	full.Annotations = map[string]string{"big": strings.Repeat("x", 4096)}
	full.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "kubectl"}}
	full.Spec.Devices.Requests = []resourceapi.DeviceRequest{
		{Name: "a", Exactly: &resourceapi.ExactDeviceRequest{DeviceClassName: "vgpu-manager"}},
		{Name: "fa", FirstAvailable: []resourceapi.DeviceSubRequest{{Name: "x", DeviceClassName: "vgpu-manager"}}},
	}
	full.Status.ReservedFor = []resourceapi.ResourceClaimConsumerReference{{Resource: "pods", Name: "p", UID: "u"}}

	out, err := trimClaim(testDriver, testNode)(full)
	if err != nil {
		t.Fatal(err)
	}
	trimmed := out.(*resourceapi.ResourceClaim)
	if trimmed.UID != "uid-t" || trimmed.Annotations != nil || trimmed.ManagedFields != nil || trimmed.Status.ReservedFor != nil {
		t.Fatalf("unexpected leftovers: %+v", trimmed.ObjectMeta)
	}
	if len(trimmed.Status.Allocation.Devices.Results) != 1 || trimmed.Status.Allocation.Devices.Results[0].Pool != testNode {
		t.Fatalf("results must be narrowed to this pool: %+v", trimmed.Status.Allocation.Devices.Results)
	}
	if trimmed.Spec.Devices.Requests[0].Exactly == nil || trimmed.Spec.Devices.Requests[0].Exactly.DeviceClassName != "" ||
		trimmed.Spec.Devices.Requests[1].FirstAvailable[0].Name != "x" {
		t.Fatalf("request names must survive without payload: %+v", trimmed.Spec.Devices.Requests)
	}
	if remote.MainRequestName(trimmed, "fa/x") != "fa" {
		t.Fatal("subrequest folding must still work on the trimmed claim")
	}

	// Unallocated stays unallocated; non-claims pass through.
	un, _ := trimClaim(testDriver, testNode)(&resourceapi.ResourceClaim{})
	if un.(*resourceapi.ResourceClaim).Status.Allocation != nil {
		t.Fatal("nil allocation must stay nil")
	}
	if v, _ := trimClaim(testDriver, testNode)("not a claim"); v != "not a claim" {
		t.Fatal("foreign objects must pass through")
	}
}

// The library resolves the shared SM watcher cache to <base>/watcher/...;
// Prepare must bridge that to the manager dir's watcher directory (where the
// dra-server plugin writes the file) with a relative symlink.
func TestPrepareWatcherSymlink(t *testing.T) {
	parent := t.TempDir() // stands in for the manager dir
	base := filepath.Join(parent, "remote-sessions")
	store := NewSessionStore(Config{SessionBase: base, NodeName: testNode, DriverName: testDriver})
	if err := store.Prepare(); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(base, util.Watcher)
	target, err := os.Readlink(link)
	if err != nil || target != filepath.Join("..", util.Watcher) {
		t.Fatalf("expected %s -> ../watcher symlink, got %q, %v", link, target, err)
	}
	// A file the plugin writes into <manager>/watcher must be visible through
	// the library's <base>/watcher path.
	if err := os.WriteFile(filepath.Join(parent, util.Watcher, util.SMUtilFile), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, util.Watcher, util.SMUtilFile)); err != nil {
		t.Fatalf("cache not visible through the session base: %v", err)
	}

	// Prepare is idempotent, and an empty directory left by an older agent
	// is migrated to the symlink.
	if err := store.Prepare(); err != nil {
		t.Fatal(err)
	}
	base2 := filepath.Join(t.TempDir(), "remote-sessions")
	if err := os.MkdirAll(filepath.Join(base2, util.Watcher), 0o755); err != nil {
		t.Fatal(err)
	}
	store2 := NewSessionStore(Config{SessionBase: base2, NodeName: testNode, DriverName: testDriver})
	if err := store2.Prepare(); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(filepath.Join(base2, util.Watcher)); err != nil || target != filepath.Join("..", util.Watcher) {
		t.Fatalf("empty legacy dir not migrated: %q, %v", target, err)
	}
}
