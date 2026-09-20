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

// nvlinkTopology is what one GPU publishes about its NVLink connectivity.
//
// The two fields answer different questions and are deliberately kept apart.
// Clique is the hardware's own fabric identity and is only ever set when the
// driver reports one, so a claim can match on it and know exactly what it got.
// Domain is the key link-topology constraints match on, and falls back to a
// locally derived grouping when no fabric identity exists — which is the only
// way `link` can mean anything on NVLink hardware without NVSwitch.
type nvlinkTopology struct {
	// Clique identifies the NVLink fabric ("clique:<clusterUUID>.<cliqueID>"),
	// empty when this GPU is not fabric-attached or the driver cannot say.
	// Equal values across NODES are meaningful: on MNNVL systems one clique
	// spans several hosts.
	Clique string
	// Domain is equal exactly for GPUs that can reach each other over NVLink.
	// Empty when the GPU has no NVLink peer on this node, which leaves it
	// unable to satisfy a link constraint at all.
	Domain string
}

// nvlinkDomainPrefixes. The prefix records which evidence produced the value,
// so two keys can never collide across derivations.
const (
	nvlinkCliquePrefix    = "clique:"
	nvlinkComponentPrefix = "comp:"
)

// discoverNVLinkDomains resolves the NVLink topology of every GPU on the node,
// keyed by GPU UUID.
//
// GPUs are grouped by walking the NVLink edges between them and taking the
// connected components, which is the grouping that matters to a workload: two
// GPUs in one component can reach each other, directly or across NVSwitches.
// Fabric identities feed into that walk (see links.GetNVLinkWithCliques) so
// that two independent NVSwitch fabrics in one chassis stay separate
// components.
//
// nodeName is mixed into the component key. Component membership is only
// meaningful within the node that computed it, and remote-GPU pools publish
// several nodes' devices into one scheduling view, where two unrelated nodes
// would otherwise both produce the key for "GPUs 0-3".
func (l *deviceLib) discoverNVLinkDomains(nodeName string) (map[string]nvlinkTopology, error) {
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

	peers, err := nvlinkPeerSets(len(devs), func(i, j int) (bool, error) {
		link, err := links.GetNVLinkWithCliques(devs[i], devs[j], cliques[i], cliques[j])
		if err != nil {
			return false, fmt.Errorf("error getting NVLink between GPU %d and %d: %w", i, j, err)
		}
		return link != links.P2PLinkUnknown, nil
	})
	if err != nil {
		return nil, err
	}

	topologies := make(map[string]nvlinkTopology, len(devs))
	for i, uuid := range uuids {
		topo := nvlinkTopology{}
		if cliques[i] != "" {
			topo.Clique = nvlinkCliquePrefix + cliques[i]
		}
		switch {
		case topo.Clique != "":
			// The fabric already names the reachable set, and names it in a
			// way that stays valid across nodes. Prefer it over the local
			// component, which cannot see peers on other hosts.
			topo.Domain = topo.Clique
		case len(peers[i]) > 0:
			topo.Domain = nvlinkComponentKey(nodeName, peers[i], uuids)
		}
		topologies[uuid] = topo
		klog.V(4).Infof("NVLink topology for GPU %s: clique=%q domain=%q", uuid, topo.Clique, topo.Domain)
	}
	return topologies, nil
}

// nvlinkPeerSets returns, per device index, the indices of every device in the
// same NVLink connected component — including the device itself, but only when
// it actually has a peer. A GPU with no NVLink peer gets an empty set, which is
// what keeps it from claiming an NVLink domain it has no way to use.
//
// connected reports whether two devices are NVLink peers; it is only ever
// asked about i<j, since the relation is symmetric and probing both directions
// would double the NVML work for the same answer.
func nvlinkPeerSets(n int, connected func(i, j int) (bool, error)) ([][]int, error) {
	// Union-find over NVLink edges. parent[i] is i's representative.
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

// nvlinkComponentKey renders a component as a fixed-width key. The member
// UUIDs are hashed rather than listed because a DRA string attribute is capped
// at 64 characters, which a node name plus eight UUIDs would blow past; the
// value is only ever compared for equality, so it does not need to be legible.
func nvlinkComponentKey(nodeName string, member []int, uuids []string) string {
	names := make([]string, 0, len(member))
	for _, i := range member {
		names = append(names, uuids[i])
	}
	slices.Sort(names)
	sum := sha256.Sum256([]byte(nodeName + "\x00" + strings.Join(names, "\x00")))
	return nvlinkComponentPrefix + hex.EncodeToString(sum[:8])
}
