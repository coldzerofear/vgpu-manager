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

	resourceapi "k8s.io/api/resource/v1"
)

// edges builds a `connected` predicate from an explicit peer list. Pairs are
// given once; the predicate answers for i<j, which is all nvlinkPeerSets asks.
func edges(pairs ...[2]int) func(i, j int) (bool, error) {
	return func(i, j int) (bool, error) {
		return slices.Contains(pairs, [2]int{i, j}) || slices.Contains(pairs, [2]int{j, i}), nil
	}
}

func TestNvlinkPeerSets(t *testing.T) {
	t.Run("a fully connected board is one component", func(t *testing.T) {
		peers, err := nvlinkPeerSets(4, edges([2]int{0, 1}, [2]int{0, 2}, [2]int{0, 3},
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
		peers, err := nvlinkPeerSets(4, edges([2]int{0, 1}, [2]int{2, 3}))
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
		peers, err := nvlinkPeerSets(3, edges([2]int{0, 1}, [2]int{1, 2}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !slices.Equal(peers[0], []int{0, 1, 2}) {
			t.Fatalf("peers[0] = %v, want [0 1 2]", peers[0])
		}
	})

	// A GPU with no peer must report nothing rather than a component of one:
	// a one-GPU "NVLink domain" would let a link constraint succeed on
	// hardware that offers no NVLink at all.
	t.Run("a GPU with no peer has no component", func(t *testing.T) {
		peers, err := nvlinkPeerSets(3, edges([2]int{0, 1}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(peers[2]) != 0 {
			t.Fatalf("peers[2] = %v, want empty", peers[2])
		}
	})

	t.Run("no GPUs at all", func(t *testing.T) {
		peers, err := nvlinkPeerSets(0, edges())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(peers) != 0 {
			t.Fatalf("peers = %v, want empty", peers)
		}
	})

	t.Run("a probe error is propagated", func(t *testing.T) {
		want := errors.New("probe failed")
		_, err := nvlinkPeerSets(2, func(int, int) (bool, error) { return false, want })
		if !errors.Is(err, want) {
			t.Fatalf("err = %v, want %v", err, want)
		}
	})
}

func TestNvlinkComponentKey(t *testing.T) {
	uuids := []string{"GPU-a", "GPU-b", "GPU-c"}

	t.Run("member order does not change the key", func(t *testing.T) {
		if a, b := nvlinkComponentKey("node1", []int{0, 1}, uuids), nvlinkComponentKey("node1", []int{1, 0}, uuids); a != b {
			t.Fatalf("%q != %q", a, b)
		}
	})

	// Remote-GPU pools publish several nodes' devices into one scheduling
	// view, where two unrelated nodes must not produce the same key.
	t.Run("the same members on another node are a different domain", func(t *testing.T) {
		if a, b := nvlinkComponentKey("node1", []int{0, 1}, uuids), nvlinkComponentKey("node2", []int{0, 1}, uuids); a == b {
			t.Fatalf("node1 and node2 both produced %q", a)
		}
	})

	t.Run("different members are a different domain", func(t *testing.T) {
		if a, b := nvlinkComponentKey("node1", []int{0, 1}, uuids), nvlinkComponentKey("node1", []int{1, 2}, uuids); a == b {
			t.Fatalf("both member sets produced %q", a)
		}
	})

	// A DRA string attribute is capped at 64 characters, and node names run to
	// 253.
	t.Run("the key fits a DRA string attribute", func(t *testing.T) {
		key := nvlinkComponentKey(strings.Repeat("n", 253), []int{0, 1, 2}, uuids)
		if len(key) > 64 {
			t.Fatalf("key %q is %d characters, want at most 64", key, len(key))
		}
		if !strings.HasPrefix(key, nvlinkComponentPrefix) {
			t.Fatalf("key %q does not carry the component prefix", key)
		}
	})
}

func TestAddNVLinkTopologyAttributes(t *testing.T) {
	attrsFor := func(topo nvlinkTopology) map[resourceapi.QualifiedName]resourceapi.DeviceAttribute {
		attrs := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{}
		(&GpuDeviceInfo{nvlink: topo}).addNVLinkTopologyAttributes(attrs)
		return attrs
	}

	t.Run("a fabric-attached GPU publishes both attributes", func(t *testing.T) {
		attrs := attrsFor(nvlinkTopology{Clique: "clique:aa.1", Domain: "clique:aa.1"})
		if got := attrs["clique"].StringValue; got == nil || *got != "clique:aa.1" {
			t.Fatalf("clique = %v, want clique:aa.1", got)
		}
		if got := attrs["nvlinkDomain"].StringValue; got == nil || *got != "clique:aa.1" {
			t.Fatalf("nvlinkDomain = %v, want clique:aa.1", got)
		}
	})

	t.Run("a GPU with no fabric identity publishes only the domain", func(t *testing.T) {
		attrs := attrsFor(nvlinkTopology{Domain: "comp:0011223344556677"})
		if _, ok := attrs["clique"]; ok {
			t.Fatal("clique must be omitted when the fabric identity is unknown")
		}
		if got := attrs["nvlinkDomain"].StringValue; got == nil || *got != "comp:0011223344556677" {
			t.Fatalf("nvlinkDomain = %v, want comp:0011223344556677", got)
		}
	})

	// Omitting the attribute is what makes a matchAttribute constraint refuse
	// the device; publishing an empty string would make it match other
	// NVLink-less GPUs instead.
	t.Run("a GPU with no NVLink publishes nothing", func(t *testing.T) {
		if attrs := attrsFor(nvlinkTopology{}); len(attrs) != 0 {
			t.Fatalf("attrs = %v, want none", attrs)
		}
	})
}
