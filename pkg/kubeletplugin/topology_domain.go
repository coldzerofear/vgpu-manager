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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	nvdev "github.com/NVIDIA/go-nvlib/pkg/nvlib/device"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/coldzerofear/vgpu-manager/pkg/device/gpuallocator/links"
	"k8s.io/klog/v2"
)

// deviceTopology is what one GPU publishes about its interconnect.
//
// The three fields answer different questions and are deliberately kept apart.
// Clique is the hardware's own fabric identity and is only ever set when the
// driver reports one, so a claim matching on it knows exactly what it got. The
// two domains are the keys topology constraints match on, and each is equal
// exactly for the GPUs that can reach each other at that level.
type deviceTopology struct {
	// Clique identifies the NVLink fabric ("clique:<clusterUUID>.<cliqueID>"),
	// empty when this GPU is not fabric-attached or the driver cannot say.
	// Equal values across NODES are meaningful: on MNNVL systems one clique
	// spans several hosts.
	Clique string
	// NVLinkDomain is equal exactly for GPUs that can reach each other over
	// NVLink. Empty when the GPU has no NVLink peer on this node.
	NVLinkDomain string
	// PCIeDomain is equal exactly for GPUs that can reach each other by
	// peer-to-peer DMA without traversing a PCIe host bridge. Empty when the
	// GPU has no such peer on this node.
	//
	// It is NOT the PCIe root complex: two GPUs under one root can still have
	// to cross the host bridge to talk, and the standard pcieRoot attribute
	// would call those a pair. NVLink peers are always included, since the
	// tiers nest (NVLink links outrank switch links), so a GPU normally
	// carries both domains.
	PCIeDomain string
}

// Domain key prefixes. The prefix records which evidence produced the value,
// so two keys can never collide across derivations.
const (
	topologyCliquePrefix = "clique:"
	topologyNVLinkPrefix = "nvlink:"
	topologyPCIePrefix   = "pcie:"
)

// Connectivity thresholds, mirroring the scheduler's link tier table
// (device.LinkTierThreshold): a pair counts as connected at a level when its
// STRONGEST link type reaches the threshold. Keeping the two in step is what
// makes a `link` or `pcie` pod mean the same thing on the DRA path as on the
// extender path; TestTopologyThresholdsMatchSchedulerTiers asserts it.
const (
	nvlinkConnectedThreshold = links.SingleNVLINKLink
	pcieConnectedThreshold   = links.P2PLinkMultiSwitch
)

// discoverTopologyDomains resolves the interconnect topology of every GPU on
// the node, keyed by GPU UUID.
//
// GPUs are grouped by walking the links between them and taking the connected
// components at each level, which is the grouping that matters to a workload:
// two GPUs in one component can reach each other, directly or across switches.
// Fabric identities feed into the NVLink walk (see links.GetNVLinkWithCliques)
// so that two independent NVSwitch fabrics in one chassis stay separate.
//
// nodeName is mixed into the component keys. Component membership is only
// meaningful within the node that computed it, and remote-GPU pools publish
// several nodes' devices into one scheduling view, where two unrelated nodes
// would otherwise both produce the key for "GPUs 0-3".
func (l *deviceLib) discoverTopologyDomains(nodeName string) (map[string]deviceTopology, error) {
	shutdown, ret := l.ensureNVML()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("ensureNVML failed: %w", ret)
	}
	defer shutdown()

	var (
		devs  []nvdev.Device
		uuids []string
	)
	if err := l.VisitDevices(func(_ int, d nvdev.Device) error {
		uuid, ret := d.GetUUID()
		if ret != nvml.SUCCESS {
			return fmt.Errorf("error getting GPU uuid: %w", ret)
		}
		devs = append(devs, d)
		uuids = append(uuids, uuid)
		return nil
	}); err != nil {
		return nil, err
	}

	cliques := links.FabricCliques(l.DeviceLib, devs)

	strongest, err := strongestLinks(devs, cliques)
	if err != nil {
		return nil, err
	}

	nvlinkPeers, err := peerSets(len(devs), func(i, j int) (bool, error) {
		return strongest[i][j] >= nvlinkConnectedThreshold, nil
	})
	if err != nil {
		return nil, err
	}
	pciePeers, err := peerSets(len(devs), func(i, j int) (bool, error) {
		return strongest[i][j] >= pcieConnectedThreshold, nil
	})
	if err != nil {
		return nil, err
	}

	topologies := make(map[string]deviceTopology, len(devs))
	for i, uuid := range uuids {
		topo := deviceTopology{}
		if cliques[i] != "" {
			topo.Clique = topologyCliquePrefix + cliques[i]
		}
		switch {
		case topo.Clique != "":
			// The fabric already names the reachable set, and names it in a
			// way that stays valid across nodes. Prefer it over the local
			// component, which cannot see peers on other hosts.
			topo.NVLinkDomain = topo.Clique
		case len(nvlinkPeers[i]) > 0:
			topo.NVLinkDomain = componentKey(topologyNVLinkPrefix, nodeName, nvlinkPeers[i], uuids)
		}
		if len(pciePeers[i]) > 0 {
			// Always node-local: PCIe peer-to-peer never crosses a host, so
			// unlike a clique this key must never compare equal across nodes.
			topo.PCIeDomain = componentKey(topologyPCIePrefix, nodeName, pciePeers[i], uuids)
		}
		topologies[uuid] = topo
		klog.V(4).Infof("Topology for GPU %s: clique=%q nvlinkDomain=%q pcieDomain=%q",
			uuid, topo.Clique, topo.NVLinkDomain, topo.PCIeDomain)
	}
	return topologies, nil
}

// strongestLinks returns the symmetric matrix of the strongest link type
// between every pair of devices, which is the quantity both connectivity
// levels are thresholds on. The diagonal is left at zero: a device is not its
// own peer, and treating it as one would give an isolated GPU a domain of one.
//
// Only i<j is probed and the result mirrored: the relation is symmetric and
// each probe costs a full NVML link scan of both devices.
func strongestLinks(devs []nvdev.Device, cliques []string) ([][]links.P2PLinkType, error) {
	matrix := make([][]links.P2PLinkType, len(devs))
	for i := range matrix {
		matrix[i] = make([]links.P2PLinkType, len(devs))
	}
	for i := 0; i < len(devs); i++ {
		for j := i + 1; j < len(devs); j++ {
			p2p, err := links.GetP2PLink(devs[i], devs[j])
			if err != nil {
				return nil, fmt.Errorf("error getting P2P link between GPU %d and %d: %w", i, j, err)
			}
			nvlink, err := links.GetNVLinkWithCliques(devs[i], devs[j], cliques[i], cliques[j])
			if err != nil {
				return nil, fmt.Errorf("error getting NVLink between GPU %d and %d: %w", i, j, err)
			}
			matrix[i][j] = max(p2p, nvlink)
			matrix[j][i] = matrix[i][j]
		}
	}
	return matrix, nil
}

// peerSets returns, per device index, the indices of every device in the same
// connected component — including the device itself, but only when it actually
// has a peer. A GPU with no peer gets an empty set, which is what keeps it from
// claiming a domain it has no way to use.
//
// connected reports whether two devices are peers; it is only ever asked about
// i<j, since the relation is symmetric.
func peerSets(n int, connected func(i, j int) (bool, error)) ([][]int, error) {
	// Union-find over the connectivity edges. parent[i] is i's representative.
	parent := make([]int, n)
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}

	linked := make([]bool, n)
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			isPeer, err := connected(i, j)
			if err != nil {
				return nil, err
			}
			if !isPeer {
				continue
			}
			linked[i], linked[j] = true, true
			if ri, rj := find(i), find(j); ri != rj {
				parent[ri] = rj
			}
		}
	}

	members := make(map[int][]int, n)
	for i := 0; i < n; i++ {
		if linked[i] {
			root := find(i)
			members[root] = append(members[root], i)
		}
	}
	peers := make([][]int, n)
	for i := 0; i < n; i++ {
		if linked[i] {
			peers[i] = members[find(i)]
		}
	}
	return peers, nil
}

// componentKey renders a component as a fixed-width key. The member UUIDs are
// hashed rather than listed because a DRA string attribute is capped at 64
// characters, which a node name plus eight UUIDs would blow past; the value is
// only ever compared for equality, so it does not need to be legible.
func componentKey(prefix, nodeName string, member []int, uuids []string) string {
	names := make([]string, 0, len(member))
	for _, i := range member {
		names = append(names, uuids[i])
	}
	slices.Sort(names)
	sum := sha256.Sum256([]byte(prefix + nodeName + "\x00" + strings.Join(names, "\x00")))
	return prefix + hex.EncodeToString(sum[:8])
}
