/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package kubeletplugin

import (
	"testing"

	"github.com/stretchr/testify/assert"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
)

func TestResourceClaimToString(t *testing.T) {
	rc := &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "claim-a", UID: "uid-1"},
	}
	assert.Equal(t, "ns/claim-a:uid-1", ResourceClaimToString(rc))
}

func TestPreparedClaimToString(t *testing.T) {
	pc := &PreparedClaim{Name: "claim-b", Namespace: "ns"}
	assert.Equal(t, "ns/claim-b:uid-9", PreparedClaimToString(pc, "uid-9"))
}

func TestClaimsToStrings(t *testing.T) {
	claims := []*resourcev1.ResourceClaim{
		{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "a", UID: "1"}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "b", UID: "2"}},
	}
	assert.Equal(t, []string{"ns/a:1", "ns/b:2"}, ClaimsToStrings(claims))
	assert.Empty(t, ClaimsToStrings(nil))
}

func TestClaimRefsToStrings(t *testing.T) {
	refs := []kubeletplugin.NamespacedObject{
		{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "r1"}, UID: "u1"},
		{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "r2"}}, // no UID
	}
	assert.Equal(t, []string{"ns/r1:u1", "ns/r2"}, ClaimRefsToStrings(refs))

	assert.Empty(t, ClaimRefsToStrings(nil))
}
