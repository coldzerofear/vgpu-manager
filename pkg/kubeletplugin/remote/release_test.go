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
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	draclient "k8s.io/dynamic-resource-allocation/client"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	pkgflags "sigs.k8s.io/dra-driver-nvidia-gpu/pkg/flags"
)

func TestClaimHasLiveConsumers(t *testing.T) {
	ctx := context.Background()
	pod := func(name, uid, node string, phase corev1.PodPhase) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", UID: types.UID(uid)},
			Spec:       corev1.PodSpec{NodeName: node},
			Status:     corev1.PodStatus{Phase: phase},
		}
	}
	ref := func(name, uid string) resourceapi.ResourceClaimConsumerReference {
		return resourceapi.ResourceClaimConsumerReference{Resource: "pods", Name: name, UID: types.UID(uid)}
	}
	d := &InjectDriver{config: InjectConfig{NodeName: "node-x"}, clients: pkgflags.ClientSets{Core: fake.NewSimpleClientset(
		pod("here", "u-here", "node-x", corev1.PodRunning),   // on this node: the kubelet said it is done
		pod("done", "u-done", "node-y", corev1.PodSucceeded), // finished elsewhere
		pod("live", "u-live", "node-y", corev1.PodRunning),   // still running elsewhere
		pod("reborn", "u-new", "node-y", corev1.PodRunning),  // same name, different UID
	)}}
	claim := func(refs ...resourceapi.ResourceClaimConsumerReference) *resourceapi.ResourceClaim {
		return &resourceapi.ResourceClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns"},
			Status:     resourceapi.ResourceClaimStatus{ReservedFor: refs},
		}
	}
	for name, tc := range map[string]struct {
		refs []resourceapi.ResourceClaimConsumerReference
		live bool
	}{
		"nobody":             {nil, false},
		"only this node":     {[]resourceapi.ResourceClaimConsumerReference{ref("here", "u-here")}, false},
		"finished elsewhere": {[]resourceapi.ResourceClaimConsumerReference{ref("done", "u-done")}, false},
		"gone":               {[]resourceapi.ResourceClaimConsumerReference{ref("missing", "u-x")}, false},
		"replaced (uid)":     {[]resourceapi.ResourceClaimConsumerReference{ref("reborn", "u-old")}, false},
		"running elsewhere":  {[]resourceapi.ResourceClaimConsumerReference{ref("here", "u-here"), ref("live", "u-live")}, true},
		"non-pod consumer":   {[]resourceapi.ResourceClaimConsumerReference{{APIGroup: "batch", Resource: "jobs", Name: "j", UID: "u"}}, true},
	} {
		live, err := d.claimHasLiveConsumers(ctx, claim(tc.refs...))
		if err != nil || live != tc.live {
			t.Errorf("%s: live=%v err=%v, want %v", name, live, err, tc.live)
		}
	}
}

func TestAssignTokensScopedToAllocation(t *testing.T) {
	ctx := context.Background()
	alloc := func(dev string) *resourceapi.AllocationResult {
		return &resourceapi.AllocationResult{Devices: resourceapi.DeviceAllocationResult{Results: []resourceapi.DeviceRequestAllocationResult{
			{Request: "r1", Driver: "d", Pool: "gpu-a", Device: dev},
		}}}
	}
	claim := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns", UID: "uid-1"},
		Status:     resourceapi.ResourceClaimStatus{Allocation: alloc("vgpu-0")},
	}
	d := &InjectDriver{clients: pkgflags.ClientSets{Core: fake.NewSimpleClientset(claim)}}
	key := SessionAnnotationKey("part-1")

	// First prepare: mints a token and records the allocation it belongs to.
	p := &partition{key: "part-1"}
	if err := d.assignTokens(ctx, claim, []*partition{p}); err != nil {
		t.Fatal(err)
	}
	first := p.token
	if first == "" || claim.Annotations[key] != first || claim.Annotations[AllocationAnnotation] != AllocationID(claim) {
		t.Fatalf("after first assign: token=%q annotations=%v", first, claim.Annotations)
	}

	// Same allocation (a kubelet retry): the token is reused, nothing patched.
	p = &partition{key: "part-1"}
	if err := d.assignTokens(ctx, claim, []*partition{p}); err != nil {
		t.Fatal(err)
	}
	if p.token != first {
		t.Fatalf("retry must reuse the token: %q != %q", p.token, first)
	}

	// The claim was deallocated and allocated again to another device before
	// the previous consumer's NodeUnprepare ran: its tokens must not carry
	// over. The stale annotation is removed in the same patch.
	claim.Status.Allocation = alloc("vgpu-1")
	if _, err := d.clients.Core.ResourceV1().ResourceClaims("ns").UpdateStatus(ctx, claim, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	p = &partition{key: "part-1"}
	q := &partition{key: "part-2"}
	if err := d.assignTokens(ctx, claim, []*partition{p, q}); err != nil {
		t.Fatal(err)
	}
	if p.token == first || p.token == "" || q.token == "" || p.token == q.token {
		t.Fatalf("new allocation must get fresh tokens: p=%q q=%q first=%q", p.token, q.token, first)
	}
	if claim.Annotations[key] != p.token || claim.Annotations[AllocationAnnotation] != AllocationID(claim) {
		t.Fatalf("annotations after re-allocation: %v", claim.Annotations)
	}
	if got := ClaimSessionTokens(claim.Annotations); !got.Equal(sets.New(p.token, q.token)) {
		t.Fatalf("stale tokens must be gone: %v", got)
	}
	// And the API object agrees with the local copy.
	stored, err := d.clients.Core.ResourceV1().ResourceClaims("ns").Get(ctx, "c", metav1.GetOptions{})
	if err != nil || !ClaimSessionTokens(stored.Annotations).Equal(sets.New(p.token, q.token)) {
		t.Fatalf("stored annotations: %v %v", stored.Annotations, err)
	}
}

// conflictOnce makes the first patch of a resourceclaim fail with a Conflict
// and records the resourceVersion every patch body carried.
func conflictOnce(cs *fake.Clientset) (patchedRVs *[]string) {
	var rvs []string
	fired := false
	cs.PrependReactor("patch", "resourceclaims", func(action k8stesting.Action) (bool, runtime.Object, error) {
		body := string(action.(k8stesting.PatchAction).GetPatch())
		rv := ""
		if i := strings.Index(body, `"resourceVersion":"`); i >= 0 {
			rest := body[i+len(`"resourceVersion":"`):]
			rv = rest[:strings.Index(rest, `"`)]
		}
		rvs = append(rvs, rv)
		if !fired {
			fired = true
			return true, nil, apierrors.NewConflict(resourceapi.Resource("resourceclaims"), action.(k8stesting.PatchAction).GetName(), errors.New("the object has been modified"))
		}
		return false, nil, nil
	})
	return &rvs
}

func TestAssignTokensRetriesOnConflict(t *testing.T) {
	ctx := context.Background()
	claim := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns", UID: "uid-1", ResourceVersion: "7"},
		Status: resourceapi.ResourceClaimStatus{Allocation: &resourceapi.AllocationResult{Devices: resourceapi.DeviceAllocationResult{Results: []resourceapi.DeviceRequestAllocationResult{
			{Request: "r1", Driver: "d", Pool: "gpu-a", Device: "vgpu-0"},
		}}}},
	}
	cs := fake.NewSimpleClientset(claim.DeepCopy())
	rvs := conflictOnce(cs)
	d := &InjectDriver{clients: pkgflags.ClientSets{Core: cs, Resource: draclient.New(cs)}}

	p := &partition{key: "part-1"}
	if err := d.assignTokens(ctx, claim, []*partition{p}); err != nil {
		t.Fatal(err)
	}
	if len(*rvs) != 2 || (*rvs)[0] != "7" || (*rvs)[1] == "" {
		t.Fatalf("patches must carry the claim version and be retried after a conflict: %v", *rvs)
	}
	if p.token == "" || claim.Annotations[SessionAnnotationKey("part-1")] != p.token {
		t.Fatalf("token must be recorded after the retry: %q %v", p.token, claim.Annotations)
	}
}

func TestReleaseClaimReevaluatesOnConflict(t *testing.T) {
	ctx := context.Background()
	claim := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns", UID: "uid-1", ResourceVersion: "7",
			Annotations: map[string]string{SessionAnnotationKey("p"): "tok-old", AllocationAnnotation: "x"}},
		Status: resourceapi.ResourceClaimStatus{Allocation: &resourceapi.AllocationResult{}},
	}
	cs := fake.NewSimpleClientset(claim)
	rvs := conflictOnce(cs)
	d := &InjectDriver{config: InjectConfig{NodeName: "node-x"}, clients: pkgflags.ClientSets{Core: cs, Resource: draclient.New(cs)}}

	// Nobody holds the claim: the tokens go, conditionally, and a conflict
	// makes the decision be taken again on the fresh object.
	if err := d.releaseClaim(ctx, kubeletplugin.NamespacedObject{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "c"}, UID: "uid-1"}); err != nil {
		t.Fatal(err)
	}
	if len(*rvs) != 2 || (*rvs)[0] != "7" {
		t.Fatalf("clean patches must carry the claim version and be retried: %v", *rvs)
	}
	stored, err := cs.ResourceV1().ResourceClaims("ns").Get(ctx, "c", metav1.GetOptions{})
	if err != nil || len(ClaimSessionTokens(stored.Annotations)) != 0 || stored.Annotations[AllocationAnnotation] != "" {
		t.Fatalf("tokens must be gone: %v %v", stored.Annotations, err)
	}
}
