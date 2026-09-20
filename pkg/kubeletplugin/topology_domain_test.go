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

package kubeletplugin

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/device/gpuallocator/links"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	resourceapi "k8s.io/api/resource/v1"
)

// TestTopologyThresholdsMatchSchedulerTiers is the guard that keeps a `link` or
// `pcie` pod meaning the same thing on the DRA path as on the extender path.
// The two compute connectivity independently — the plugin from NVML at
// discovery, the scheduler from the published topology annotation — so the only
// thing tying them together is that both use these thresholds.
//
// pkg/device is imported by this TEST only: the plugin must not depend on the
// scheduler's device model at runtime.
func TestTopologyThresholdsMatchSchedulerTiers(t *testing.T) {
	if got, want := int(nvlinkConnectedThreshold), device.LinkTierThreshold(device.TierNVLink); got != want {
		t.Errorf("nvlink threshold = %d, scheduler TierNVLink = %d", got, want)
	}
	if got, want := int(pcieConnectedThreshold), device.LinkTierThreshold(device.TierSwitch); got != want {
		t.Errorf("pcie threshold = %d, scheduler TierSwitch = %d", got, want)
	}
	// The tiers nest, so any NVLink pair is also a PCIe-domain pair. A GPU is
	// therefore never in an NVLink domain without also being in a PCIe one.
	if nvlinkConnectedThreshold < pcieConnectedThreshold {
		t.Errorf("nvlink threshold %d must not be below the pcie threshold %d",
			nvlinkConnectedThreshold, pcieConnectedThreshold)
	}

	// strongestLinks takes max(GetP2PLink, GetNVLinkWithCliques), and only the
	// NVLink side applies the fabric-clique check. If a PCIe topology level
	// could ever reach the NVLink threshold, that max would route around the
	// check and two GPUs on different NVSwitch fabrics would land in one
	// nvlinkDomain again. P2PLinkSameBoard is the strongest thing GetP2PLink
	// returns.
	if links.P2PLinkSameBoard >= nvlinkConnectedThreshold {
		t.Errorf("P2PLinkSameBoard (%d) reaches the nvlink threshold (%d): a PCIe "+
			"topology level can now bypass the fabric-clique check",
			links.P2PLinkSameBoard, nvlinkConnectedThreshold)
	}
}

// edges builds a `connected` predicate from an explicit peer list. Pairs are
// given once; the predicate answers for i<j, which is all peerSets asks.
func edges(pairs ...[2]int) func(i, j int) (bool, error) {
	return func(i, j int) (bool, error) {
		return slices.Contains(pairs, [2]int{i, j}) || slices.Contains(pairs, [2]int{j, i}), nil
	}
}

func TestPeerSets(t *testing.T) {
	t.Run("a fully connected board is one component", func(t *testing.T) {
		peers, err := peerSets(4, edges([2]int{0, 1}, [2]int{0, 2}, [2]int{0, 3},
			[2]int{1, 2}, [2]int{1, 3}, [2]int{2, 3}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		for i := range peers {
			if len(peers[i]) != 4 {
				t.Fatalf("peers[%d] = %v, want all four GPUs", i, peers[i])
			}
		}
	})

	// The case the whole feature exists for: two islands must not be merged.
	t.Run("two islands stay separate", func(t *testing.T) {
		peers, err := peerSets(4, edges([2]int{0, 1}, [2]int{2, 3}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !slices.Equal(peers[0], []int{0, 1}) || !slices.Equal(peers[1], []int{0, 1}) {
			t.Fatalf("first island = %v / %v, want [0 1]", peers[0], peers[1])
		}
		if !slices.Equal(peers[2], []int{2, 3}) || !slices.Equal(peers[3], []int{2, 3}) {
			t.Fatalf("second island = %v / %v, want [2 3]", peers[2], peers[3])
		}
	})

	t.Run("transitive links form one component", func(t *testing.T) {
		peers, err := peerSets(3, edges([2]int{0, 1}, [2]int{1, 2}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !slices.Equal(peers[0], []int{0, 1, 2}) {
			t.Fatalf("peers[0] = %v, want [0 1 2]", peers[0])
		}
	})

	// A GPU with no peer must report nothing rather than a component of one: a
	// one-GPU domain would let a topology constraint succeed on hardware that
	// offers no such connectivity at all.
	t.Run("a GPU with no peer has no component", func(t *testing.T) {
		peers, err := peerSets(3, edges([2]int{0, 1}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(peers[2]) != 0 {
			t.Fatalf("peers[2] = %v, want empty", peers[2])
		}
	})

	t.Run("no GPUs at all", func(t *testing.T) {
		peers, err := peerSets(0, edges())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(peers) != 0 {
			t.Fatalf("peers = %v, want empty", peers)
		}
	})

	t.Run("a probe error is propagated", func(t *testing.T) {
		want := errors.New("probe failed")
		_, err := peerSets(2, func(int, int) (bool, error) { return false, want })
		if !errors.Is(err, want) {
			t.Fatalf("err = %v, want %v", err, want)
		}
	})
}

func TestComponentKey(t *testing.T) {
	uuids := []string{"GPU-a", "GPU-b", "GPU-c"}

	t.Run("member order does not change the key", func(t *testing.T) {
		a := componentKey(topologyNVLinkPrefix, "node1", []int{0, 1}, uuids)
		b := componentKey(topologyNVLinkPrefix, "node1", []int{1, 0}, uuids)
		if a != b {
			t.Fatalf("%q != %q", a, b)
		}
	})

	// Remote-GPU pools publish several nodes' devices into one scheduling view,
	// where two unrelated nodes must not produce the same key.
	t.Run("the same members on another node are a different domain", func(t *testing.T) {
		a := componentKey(topologyNVLinkPrefix, "node1", []int{0, 1}, uuids)
		b := componentKey(topologyNVLinkPrefix, "node2", []int{0, 1}, uuids)
		if a == b {
			t.Fatalf("node1 and node2 both produced %q", a)
		}
	})

	t.Run("different members are a different domain", func(t *testing.T) {
		a := componentKey(topologyNVLinkPrefix, "node1", []int{0, 1}, uuids)
		b := componentKey(topologyNVLinkPrefix, "node1", []int{1, 2}, uuids)
		if a == b {
			t.Fatalf("both member sets produced %q", a)
		}
	})

	// The NVLink and PCIe components of a node frequently have the SAME
	// members, and the two keys must still not collide: a pod asking for pcie
	// would otherwise be indistinguishable from one asking for link.
	t.Run("the same members at another level are a different domain", func(t *testing.T) {
		a := componentKey(topologyNVLinkPrefix, "node1", []int{0, 1}, uuids)
		b := componentKey(topologyPCIePrefix, "node1", []int{0, 1}, uuids)
		if a == b {
			t.Fatalf("both levels produced %q", a)
		}
	})

	// A DRA string attribute is capped at 64 characters, and node names run to
	// 253.
	t.Run("the key fits a DRA string attribute", func(t *testing.T) {
		for _, prefix := range []string{topologyNVLinkPrefix, topologyPCIePrefix} {
			key := componentKey(prefix, strings.Repeat("n", 253), []int{0, 1, 2}, uuids)
			if len(key) > 64 {
				t.Fatalf("key %q is %d characters, want at most 64", key, len(key))
			}
			if !strings.HasPrefix(key, prefix) {
				t.Fatalf("key %q does not carry the %q prefix", key, prefix)
			}
		}
	})
}

func TestAddTopologyDeviceAttributes(t *testing.T) {
	attrsFor := func(topo deviceTopology) map[resourceapi.QualifiedName]resourceapi.DeviceAttribute {
		attrs := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{}
		(&GpuDeviceInfo{topology: topo}).addTopologyDeviceAttributes(attrs)
		return attrs
	}
	value := func(attrs map[resourceapi.QualifiedName]resourceapi.DeviceAttribute, name resourceapi.QualifiedName) string {
		attr, ok := attrs[name]
		if !ok || attr.StringValue == nil {
			return ""
		}
		return *attr.StringValue
	}

	t.Run("a fabric-attached GPU publishes all three attributes", func(t *testing.T) {
		attrs := attrsFor(deviceTopology{
			Clique:       "clique:aa.1",
			NVLinkDomain: "clique:aa.1",
			PCIeDomain:   "pcie:0011223344556677",
		})
		if got := value(attrs, util.CliqueDeviceAttribute); got != "clique:aa.1" {
			t.Fatalf("clique = %q, want clique:aa.1", got)
		}
		if got := value(attrs, util.NVLinkDomainDeviceAttribute); got != "clique:aa.1" {
			t.Fatalf("nvlinkDomain = %q, want clique:aa.1", got)
		}
		if got := value(attrs, util.PCIeDomainDeviceAttribute); got != "pcie:0011223344556677" {
			t.Fatalf("pcieDomain = %q, want pcie:0011223344556677", got)
		}
	})

	// A PCIe-only server: no NVLink anywhere, but `pcie` must still work.
	t.Run("a GPU with no NVLink publishes only the PCIe domain", func(t *testing.T) {
		attrs := attrsFor(deviceTopology{PCIeDomain: "pcie:0011223344556677"})
		for _, absent := range []resourceapi.QualifiedName{
			util.CliqueDeviceAttribute, util.NVLinkDomainDeviceAttribute,
		} {
			if _, ok := attrs[absent]; ok {
				t.Fatalf("%s must be omitted when there is no NVLink", absent)
			}
		}
		if got := value(attrs, util.PCIeDomainDeviceAttribute); got != "pcie:0011223344556677" {
			t.Fatalf("pcieDomain = %q, want pcie:0011223344556677", got)
		}
	})

	t.Run("a GPU with no fabric identity publishes a derived NVLink domain", func(t *testing.T) {
		attrs := attrsFor(deviceTopology{NVLinkDomain: "nvlink:aabb", PCIeDomain: "pcie:ccdd"})
		if _, ok := attrs[util.CliqueDeviceAttribute]; ok {
			t.Fatal("clique must be omitted when the fabric identity is unknown")
		}
		if got := value(attrs, util.NVLinkDomainDeviceAttribute); got != "nvlink:aabb" {
			t.Fatalf("nvlinkDomain = %q, want nvlink:aabb", got)
		}
	})

	// Omitting the attributes is what makes a matchAttribute constraint refuse
	// the device; publishing empty strings would make it match other
	// connectivity-less GPUs instead.
	t.Run("a GPU with no peer at any level publishes nothing", func(t *testing.T) {
		if attrs := attrsFor(deviceTopology{}); len(attrs) != 0 {
			t.Fatalf("attrs = %v, want none", attrs)
		}
	})
}
