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

package client

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/fake"
	k8stesting "k8s.io/client-go/testing"
)

// fakeDiscovery answers ServerGroups with the given groups, optionally
// alongside an error (the way a cluster with an unreachable aggregated API
// server answers: a partial list AND an error).
type fakeDiscovery struct {
	*fake.FakeDiscovery
	groups *metav1.APIGroupList
	err    error
}

func (f *fakeDiscovery) ServerGroups() (*metav1.APIGroupList, error) {
	return f.groups, f.err
}

func newDiscovery(err error, groupVersions ...string) discovery.DiscoveryInterface {
	byGroup := map[string]*metav1.APIGroup{}
	list := &metav1.APIGroupList{}
	for _, groupVersion := range groupVersions {
		gv, parseErr := schema.ParseGroupVersion(groupVersion)
		if parseErr != nil {
			panic(parseErr)
		}
		group, ok := byGroup[gv.Group]
		if !ok {
			list.Groups = append(list.Groups, metav1.APIGroup{Name: gv.Group})
			group = &list.Groups[len(list.Groups)-1]
			byGroup[gv.Group] = group
		}
		group.Versions = append(group.Versions, metav1.GroupVersionForDiscovery{
			GroupVersion: gv.String(), Version: gv.Version,
		})
	}
	return &fakeDiscovery{
		FakeDiscovery: &fake.FakeDiscovery{Fake: &k8stesting.Fake{}, FakedServerVersion: nil},
		groups:        list,
		err:           err,
	}
}

func TestDRAServedVersions(t *testing.T) {
	tests := []struct {
		name    string
		client  discovery.DiscoveryInterface
		want    []string
		wantErr bool
	}{{
		name:   "GA cluster",
		client: newDiscovery(nil, "resource.k8s.io/v1", "apps/v1"),
		want:   []string{"v1"},
	}, {
		// Preference order is the API server's; it must be preserved so the
		// error message names versions the way the cluster does.
		name:   "beta-only cluster keeps the server's order",
		client: newDiscovery(nil, "resource.k8s.io/v1beta2", "resource.k8s.io/v1beta1"),
		want:   []string{"v1beta2", "v1beta1"},
	}, {
		name:   "group absent is a definite empty answer",
		client: newDiscovery(nil, "apps/v1"),
		want:   []string{},
	}, {
		// An unrelated aggregated API being down must not turn into "DRA
		// state unknown": our group is plainly in the list.
		name: "partial discovery failure elsewhere does not hide the answer",
		client: newDiscovery(&discovery.ErrGroupDiscoveryFailed{Groups: map[schema.GroupVersion]error{
			{Group: "metrics.k8s.io", Version: "v1beta1"}: errors.New("unreachable"),
		}}, "resource.k8s.io/v1"),
		want: []string{"v1"},
	}, {
		name: "partial discovery failure of our own group is an error",
		client: newDiscovery(&discovery.ErrGroupDiscoveryFailed{Groups: map[schema.GroupVersion]error{
			{Group: DRAAPIGroup, Version: "v1"}: errors.New("unreachable"),
		}}, "apps/v1"),
		wantErr: true,
	}, {
		name:    "hard discovery failure is an error",
		client:  newDiscovery(errors.New("connection refused"), "apps/v1"),
		wantErr: true,
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DRAServedVersions(tt.client)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if len(got) == 0 && len(tt.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("versions = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDRAAPIRequirementCheck(t *testing.T) {
	v1Only := DRAAPIRequirement{Subject: "remote-agent", Version: "v1", Remedy: "Upgrade the cluster."}
	anyVersion := DRAAPIRequirement{Subject: "--enable-dra-monitor", Remedy: "Drop the flag."}

	// Case A/B: the group is not served (old cluster, or the apiserver gate
	// is off). Both requirements must refuse, and say why and what to do.
	noDRA := newDiscovery(nil, "apps/v1")
	for _, req := range []DRAAPIRequirement{v1Only, anyVersion} {
		err := req.Check(noDRA)
		if err == nil {
			t.Fatalf("%s: a cluster without %s must be refused", req.Subject, DRAAPIGroup)
		}
		for _, want := range []string{req.Subject, "DynamicResourceAllocation", req.Remedy} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: error %q does not mention %q", req.Subject, err, want)
			}
		}
	}

	// Case C: served, but only as beta. The v1-only component refuses and
	// names what the cluster does serve; the negotiating one accepts.
	betaOnly := newDiscovery(nil, "resource.k8s.io/v1beta1")
	err := v1Only.Check(betaOnly)
	if err == nil {
		t.Fatal("a beta-only cluster must be refused by a v1-only component")
	}
	if !strings.Contains(err.Error(), "v1beta1") || !strings.Contains(err.Error(), v1Only.Remedy) {
		t.Errorf("error %q should name the served version and the remedy", err)
	}
	if err := anyVersion.Check(betaOnly); err != nil {
		t.Errorf("a version-negotiating component must accept a beta-only cluster: %v", err)
	}

	// GA cluster: both accept.
	ga := newDiscovery(nil, "resource.k8s.io/v1", "resource.k8s.io/v1beta1")
	for _, req := range []DRAAPIRequirement{v1Only, anyVersion} {
		if err := req.Check(ga); err != nil {
			t.Errorf("%s must accept a GA cluster: %v", req.Subject, err)
		}
	}

	// A discovery failure is reported as such, never silently treated as
	// "available" (which would put us back to hanging informers).
	if err := v1Only.Check(newDiscovery(errors.New("boom"), "apps/v1")); err == nil {
		t.Fatal("a discovery failure must be surfaced")
	}
}
