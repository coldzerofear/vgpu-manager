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

	nvdev "github.com/NVIDIA/go-nvlib/pkg/nvlib/device"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
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
			NVLinkDomain: "nvlink:8899aabbccddeeff",
			PCIeDomain:   "pcie:0011223344556677",
		})
		if got := value(attrs, util.CliqueDeviceAttribute); got != "clique:aa.1" {
			t.Fatalf("clique = %q, want clique:aa.1", got)
		}
		if got := value(attrs, util.NVLinkDomainDeviceAttribute); got != "nvlink:8899aabbccddeeff" {
			t.Fatalf("nvlinkDomain = %q, want nvlink:8899aabbccddeeff", got)
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

func TestAssembleTopologies(t *testing.T) {
	uuids := []string{"GPU-0", "GPU-1", "GPU-2", "GPU-3"}
	// GPUs 0-1 are NVLink peers, as are 2-3; the two pairs share a PCIe switch
	// only within a pair, so each pair is also its own PCIe domain.
	matrix := func() [][]links.P2PLinkType {
		m := make([][]links.P2PLinkType, len(uuids))
		for i := range m {
			m[i] = make([]links.P2PLinkType, len(uuids))
		}
		link := func(i, j int, t links.P2PLinkType) { m[i][j], m[j][i] = t, t }
		link(0, 1, links.SingleNVLINKLink)
		link(2, 3, links.SingleNVLINKLink)
		link(0, 2, links.P2PLinkCrossCPU)
		link(0, 3, links.P2PLinkCrossCPU)
		link(1, 2, links.P2PLinkCrossCPU)
		link(1, 3, links.P2PLinkCrossCPU)
		return m
	}
	assemble := func(t *testing.T, nodeName string, cliques []string) map[string]deviceTopology {
		t.Helper()
		topos, err := assembleTopologies(nodeName, uuids, cliques, matrix())
		if err != nil {
			t.Fatalf("assembleTopologies: %v", err)
		}
		return topos
	}
	noCliques := make([]string, len(uuids))

	t.Run("the clique is published but does not become the NVLink domain", func(t *testing.T) {
		cliques := []string{"aa.1", "aa.1", "aa.1", "aa.1"}
		topos := assemble(t, "node-a", cliques)
		for _, uuid := range uuids {
			if got := topos[uuid].Clique; got != "clique:aa.1" {
				t.Fatalf("%s clique = %q, want clique:aa.1", uuid, got)
			}
			if !strings.HasPrefix(topos[uuid].NVLinkDomain, topologyNVLinkPrefix) {
				t.Fatalf("%s nvlinkDomain = %q, want a node-local component key", uuid, topos[uuid].NVLinkDomain)
			}
		}
		if topos["GPU-0"].NVLinkDomain != topos["GPU-1"].NVLinkDomain {
			t.Fatal("NVLink peers must share a domain")
		}
		if topos["GPU-0"].NVLinkDomain == topos["GPU-2"].NVLinkDomain {
			t.Fatal("two NVLink islands must not share a domain just because they share a clique")
		}
	})

	// A claim can draw from several nodes (remote-GPU pools), and a plain
	// claim cannot use a cross-node NVLink path.
	t.Run("one clique on two nodes is two NVLink domains", func(t *testing.T) {
		cliques := []string{"aa.1", "aa.1", "aa.1", "aa.1"}
		a, b := assemble(t, "node-a", cliques), assemble(t, "node-b", cliques)
		if a["GPU-0"].Clique != b["GPU-0"].Clique {
			t.Fatal("the clique is the cross-node identity and must stay equal")
		}
		if a["GPU-0"].NVLinkDomain == b["GPU-0"].NVLinkDomain {
			t.Fatal("nvlinkDomain must never compare equal across nodes")
		}
	})

	t.Run("a partially reported clique does not split a component", func(t *testing.T) {
		topos := assemble(t, "node-a", []string{"aa.1", "", "", ""})
		if topos["GPU-0"].NVLinkDomain != topos["GPU-1"].NVLinkDomain {
			t.Fatalf("NVLink peers split by a missing clique: %q vs %q",
				topos["GPU-0"].NVLinkDomain, topos["GPU-1"].NVLinkDomain)
		}
		if topos["GPU-1"].Clique != "" {
			t.Fatal("an unreported clique must not be invented")
		}
	})

	t.Run("without cliques the domains are the components", func(t *testing.T) {
		topos := assemble(t, "node-a", noCliques)
		if topos["GPU-0"].NVLinkDomain == "" || topos["GPU-0"].NVLinkDomain != topos["GPU-1"].NVLinkDomain {
			t.Fatal("NVLink peers must share a non-empty domain")
		}
		if topos["GPU-0"].PCIeDomain != topos["GPU-1"].PCIeDomain || topos["GPU-0"].PCIeDomain == topos["GPU-2"].PCIeDomain {
			t.Fatal("PCIe domains must follow the components, NVLink edges included")
		}
	})

	t.Run("a clique with no local NVLink peer gives no NVLink domain", func(t *testing.T) {
		topos, err := assembleTopologies("node-a", []string{"GPU-0"}, []string{"aa.1"}, [][]links.P2PLinkType{{0}})
		if err != nil {
			t.Fatalf("assembleTopologies: %v", err)
		}
		if got := topos["GPU-0"]; got.Clique != "clique:aa.1" || got.NVLinkDomain != "" || got.PCIeDomain != "" {
			t.Fatalf("topology = %+v, want only the clique", got)
		}
	})
}

// linkProbeDevice is a GPU with no NVLink whose common ancestor with each
// peer (keyed by the peer's bus id) is canned; a missing peer makes the probe
// fail. Every other nvdev.Device method is inherited from the nil embedded
// interface and must not be called.
type linkProbeDevice struct {
	nvdev.Device
	busID     string
	ancestors map[string]nvml.GpuTopologyLevel
}

func (d *linkProbeDevice) GetTopologyCommonAncestor(peer nvml.Device) (nvml.GpuTopologyLevel, nvml.Return) {
	level, ok := d.ancestors[peer.(*linkProbeDevice).busID]
	if !ok {
		return 0, nvml.ERROR_NOT_SUPPORTED
	}
	return level, nvml.SUCCESS
}

func (d *linkProbeDevice) GetNvLinkState(int) (nvml.EnableState, nvml.Return) {
	return 0, nvml.ERROR_NOT_SUPPORTED
}

func (d *linkProbeDevice) GetPciInfo() (nvml.PciInfo, nvml.Return) {
	var info nvml.PciInfo
	for i, c := range []byte(d.busID) {
		info.BusId[i] = int8(c)
	}
	return info, nvml.SUCCESS
}

// strongestLinks must never fail the walk: TopologyDeviceAttributes is not
// allowed to stop the plugin from starting.
func TestStrongestLinksNeverFails(t *testing.T) {
	probe := func(busID string, ancestors map[string]nvml.GpuTopologyLevel) *linkProbeDevice {
		return &linkProbeDevice{busID: busID, ancestors: ancestors}
	}

	t.Run("a single GPU is never probed", func(t *testing.T) {
		got := strongestLinks([]nvdev.Device{probe("0000:01:00.0", nil)}, []string{""})
		if len(got) != 1 || got[0][0] != 0 {
			t.Fatalf("matrix = %v, want one unlinked GPU", got)
		}
	})

	t.Run("PCIe-only GPUs are linked by their P2P level", func(t *testing.T) {
		a := probe("0000:01:00.0", map[string]nvml.GpuTopologyLevel{"0000:02:00.0": nvml.TOPOLOGY_SINGLE})
		b := probe("0000:02:00.0", nil)
		got := strongestLinks([]nvdev.Device{a, b}, []string{"", ""})
		if got[0][1] != links.P2PLinkSingleSwitch || got[1][0] != links.P2PLinkSingleSwitch {
			t.Fatalf("matrix = %v, want a single-switch link both ways", got)
		}
	})

	t.Run("an unreadable pair is unconnected and the rest still resolve", func(t *testing.T) {
		// 0-1 share a switch; 0-2 and 1-2 cannot be read.
		a := probe("0000:01:00.0", map[string]nvml.GpuTopologyLevel{"0000:02:00.0": nvml.TOPOLOGY_SINGLE})
		b := probe("0000:02:00.0", nil)
		c := probe("0000:03:00.0", nil)
		got := strongestLinks([]nvdev.Device{a, b, c}, []string{"", "", ""})
		if got[0][1] != links.P2PLinkSingleSwitch {
			t.Fatalf("readable pair = %v, want single switch", got[0][1])
		}
		if got[0][2] != 0 || got[1][2] != 0 {
			t.Fatalf("unreadable pairs = %v / %v, want unconnected", got[0][2], got[1][2])
		}

		topos, err := assembleTopologies("node-a", []string{"GPU-0", "GPU-1", "GPU-2"}, []string{"", "", ""}, got)
		if err != nil {
			t.Fatalf("assembleTopologies: %v", err)
		}
		if topos["GPU-0"].PCIeDomain == "" || topos["GPU-0"].PCIeDomain != topos["GPU-1"].PCIeDomain {
			t.Fatal("the readable pair must still share a PCIe domain")
		}
		if topos["GPU-2"].PCIeDomain != "" {
			t.Fatal("a GPU whose links could not be read must publish no PCIe domain")
		}
	})
}
