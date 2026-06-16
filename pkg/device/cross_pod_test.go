package device

import (
	"fmt"
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/util"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
)

// gangPod builds a pod that PodHasGangName recognises (via the coscheduling
// pod-group label), with NodeName set so ShouldCountPodDeviceAllocation counts
// its pre-allocation, and a PodVGPUPreAllocAnnotation referencing the given
// device IDs/UUIDs.
func gangPod(uid, gang, node string, claim string) *corev1.Pod {
	labels := map[string]string{}
	if gang != "" {
		labels[util.CoschedulingPodGroupLabel] = gang
	}
	anns := map[string]string{}
	if claim != "" {
		anns[util.PodVGPUPreAllocAnnotation] = claim
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID:         types.UID(uid),
			Name:        uid,
			Labels:      labels,
			Annotations: anns,
		},
		Spec: corev1.PodSpec{NodeName: node},
	}
}

// claimText renders a one-container pre-alloc annotation for the given UUIDs.
func claimText(uuids ...string) string {
	out := "cont1["
	for i, u := range uuids {
		if i > 0 {
			out += ","
		}
		out += fmt.Sprintf("%d_%s_10_1024", i, u)
	}
	return out + "]"
}

// twoComponentNode is a NodeInfo with cards gpu0/gpu1 in component 0 and
// gpu2/gpu3 in component 2 — directly constructed so the test controls the
// link components precisely (NewFakeNodeInfo rebuilds the device list without
// links, which would make every card its own singleton component).
func twoComponentNode(pods ...*corev1.Pod) *NodeInfo {
	return &NodeInfo{
		name: "node1",
		linkComponentByUUID: map[string]int{
			"gpu0": 0, "gpu1": 0,
			"gpu2": 2, "gpu3": 2,
		},
		componentToUUIDs: map[int][]string{
			0: {"gpu0", "gpu1"},
			2: {"gpu2", "gpu3"},
		},
		nodePods: pods,
	}
}

func Test_GangAnchorComponent(t *testing.T) {
	self := types.UID("self")
	tests := []struct {
		name     string
		gang     string
		pods     []*corev1.Pod
		wantRoot int
		wantOK   bool
	}{
		{
			name:     "sibling in component 2 anchors there",
			gang:     "gangA",
			pods:     []*corev1.Pod{gangPod("sib", "gangA", "node1", claimText("gpu2"))},
			wantRoot: 2,
			wantOK:   true,
		},
		{
			name:     "self pod is ignored",
			gang:     "gangA",
			pods:     []*corev1.Pod{gangPod("self", "gangA", "node1", claimText("gpu2"))},
			wantRoot: -1,
			wantOK:   false,
		},
		{
			name:     "different gang is ignored",
			gang:     "gangA",
			pods:     []*corev1.Pod{gangPod("sib", "gangB", "node1", claimText("gpu2"))},
			wantRoot: -1,
			wantOK:   false,
		},
		{
			name:     "non-gang pod is ignored",
			gang:     "gangA",
			pods:     []*corev1.Pod{gangPod("sib", "", "node1", claimText("gpu2"))},
			wantRoot: -1,
			wantOK:   false,
		},
		{
			name:     "empty gang name never anchors",
			gang:     "",
			pods:     []*corev1.Pod{gangPod("sib", "gangA", "node1", claimText("gpu2"))},
			wantRoot: -1,
			wantOK:   false,
		},
		{
			name:     "no pods, no anchor",
			gang:     "gangA",
			pods:     nil,
			wantRoot: -1,
			wantOK:   false,
		},
		{
			name: "split anchor picks majority component",
			gang: "gangA",
			pods: []*corev1.Pod{
				gangPod("sibA", "gangA", "node1", claimText("gpu0")),         // 1 vote → comp 0
				gangPod("sibB", "gangA", "node1", claimText("gpu2", "gpu3")), // 2 votes → comp 2
			},
			wantRoot: 2,
			wantOK:   true,
		},
		{
			name:     "stale sibling (no NodeName, no condition) still counts when scheduled elsewhere is false",
			gang:     "gangA",
			pods:     []*corev1.Pod{gangPod("sib", "gangA", "node1", claimText("gpu0"))},
			wantRoot: 0,
			wantOK:   true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n := twoComponentNode(tc.pods...)
			root, ok := n.GangAnchorComponent(tc.gang, sets.New(self))
			if ok != tc.wantOK || root != tc.wantRoot {
				t.Fatalf("GangAnchorComponent(%q) = (%d, %v), want (%d, %v)",
					tc.gang, root, ok, tc.wantRoot, tc.wantOK)
			}
		})
	}
}

func Test_GangAnchorComponent_UnknownUUIDIgnored(t *testing.T) {
	// A sibling pre-allocated a card the node doesn't know about (UUID not in
	// linkComponentByUUID) contributes no vote and must not anchor.
	n := twoComponentNode(gangPod("sib", "gangA", "node1", claimText("ghost")))
	if root, ok := n.GangAnchorComponent("gangA", sets.New(types.UID("self"))); ok || root != -1 {
		t.Fatalf("unknown-uuid sibling: got (%d, %v), want (-1, false)", root, ok)
	}
}

func Test_ComponentUUIDs(t *testing.T) {
	n := twoComponentNode()
	if got := n.ComponentUUIDs(2); len(got) != 2 {
		t.Fatalf("ComponentUUIDs(2) = %v, want 2 members", got)
	}
	if got := n.ComponentUUIDs(99); got != nil {
		t.Fatalf("ComponentUUIDs(unknown) = %v, want nil", got)
	}
}

func Test_buildComponentIndex(t *testing.T) {
	if got := buildComponentIndex(nil); got != nil {
		t.Fatalf("buildComponentIndex(nil) = %v, want nil", got)
	}
	idx := buildComponentIndex(map[string]int{"a": 1, "b": 1, "c": 3})
	if len(idx[1]) != 2 || len(idx[3]) != 1 {
		t.Fatalf("buildComponentIndex produced %v", idx)
	}
}
