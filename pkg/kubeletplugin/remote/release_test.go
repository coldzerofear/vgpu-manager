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
	"testing"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
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
