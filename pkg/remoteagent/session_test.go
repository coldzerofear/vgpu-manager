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
					Name: "vgpu-0",
					Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
						remote.AttrUUID:              {StringValue: ptr.To("GPU-aaaa")},
						remote.AttrCUDADriverVersion: {VersionValue: ptr.To("12.9.0")},
						"driverVersion":              {VersionValue: ptr.To("575.57.8")},
					},
					Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
						remote.CapacityCores:  {Value: resource.MustParse("100")},
						remote.CapacityMemory: {Value: resource.MustParse("24Gi")},
					},
				},
				{
					Name: "vgpu-1",
					Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
						remote.AttrUUID: {StringValue: ptr.To("GPU-bbbb")},
					},
					Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
						remote.CapacityCores:  {Value: resource.MustParse("100")},
						remote.CapacityMemory: {Value: resource.MustParse("24Gi")},
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

func TestNodeDevicesFromSlices(t *testing.T) {
	nd := NodeDevicesFromSlices([]*resourceapi.ResourceSlice{testSlice()}, testNode)
	if len(nd.Devices) != 2 {
		t.Fatalf("expected 2 devices, got %d", len(nd.Devices))
	}
	if d := nd.Devices["vgpu-0"]; d.UUID != "GPU-aaaa" || d.MemoryMiB != 24*1024 || d.Cores != 100 {
		t.Fatalf("unexpected device: %+v", d)
	}
	if nd.CudaVersionString != "12.9.0" || nd.CudaDriverVersion != 12090 || nd.DriverVersion != "575.57.8" {
		t.Fatalf("unexpected versions: %+v", nd)
	}
	if other := NodeDevicesFromSlices([]*resourceapi.ResourceSlice{testSlice()}, "other-node"); len(other.Devices) != 0 {
		t.Fatal("devices of another pool must be ignored")
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
	store := NewSessionStore(base, false)
	if err := store.Prepare(); err != nil {
		t.Fatal(err)
	}
	nd := NodeDevicesFromSlices([]*resourceapi.ResourceSlice{testSlice()}, testNode)

	claim := testClaim("uid-1",
		result("other-node", "vgpu-0", "", ""), // foreign pool, ignored
		result(testNode, "vgpu-1", "50", "4Gi"),
		result(testNode, "vgpu-0", "", ""), // whole device: no limits
	)
	if err := store.Materialize("uid-1", claim, nd, testNode, testDriver); err != nil {
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
	// Slot 0 = first allocation result of this pool (vgpu-1, limited).
	d0 := cfg.Devices[0]
	if d0.Activate != 1 || string(trimZero(d0.UUID[:])) != "GPU-bbbb" {
		t.Fatalf("slot 0 = %+v", d0)
	}
	if d0.MemoryLimit != 1 || d0.TotalMemory != 4<<30 || d0.CoreLimit != 1 || d0.HardLimit != 1 || d0.HardCore != 50 {
		t.Fatalf("slot 0 limits = mem(%d,%d) core(%d,%d,%d)", d0.MemoryLimit, d0.TotalMemory, d0.CoreLimit, d0.HardLimit, d0.HardCore)
	}
	// Slot 1 = whole device, limits off.
	d1 := cfg.Devices[1]
	if d1.Activate != 1 || string(trimZero(d1.UUID[:])) != "GPU-aaaa" || d1.MemoryLimit != 0 || d1.CoreLimit != 0 {
		t.Fatalf("slot 1 = %+v", d1)
	}
	if cfg.Devices[2].Activate != 0 {
		t.Fatal("slot 2 must be inactive")
	}

	// Idempotent for the same claim; refused for a different claim.
	if err := store.Materialize("uid-1", claim, nd, testNode, testDriver); err != nil {
		t.Fatalf("second materialize must be a no-op: %v", err)
	}
	if err := store.Materialize("uid-1", testClaim("uid-2", result(testNode, "vgpu-0", "", "")), nd, testNode, testDriver); err == nil {
		t.Fatal("token reuse by another claim must be refused")
	}

	// Errors: unknown device, nothing on this pool, bad token.
	if err := store.Materialize("t2", testClaim("u", result(testNode, "vgpu-9", "", "")), nd, testNode, testDriver); err == nil {
		t.Fatal("unknown device must fail")
	}
	if err := store.Materialize("t3", testClaim("u", result("elsewhere", "vgpu-0", "", "")), nd, testNode, testDriver); err == nil {
		t.Fatal("claim with nothing on this pool must fail")
	}
	if err := store.Materialize("../t", claim, nd, testNode, testDriver); err == nil {
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
