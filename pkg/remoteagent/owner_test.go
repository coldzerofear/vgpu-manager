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

package remoteagent

import (
	"context"
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/api/remoteagent"
	"github.com/coldzerofear/vgpu-manager/pkg/device/remotegpu"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/client-go/kubernetes/fake"
	pkgflags "sigs.k8s.io/dra-driver-nvidia-gpu/pkg/flags"
)

// What each configured owner kind serves, and what it refuses.
func TestServedOwners(t *testing.T) {
	for _, test := range []struct {
		configured OwnerKind
		claims     bool
		pods       bool
		served     []string
	}{
		{configured: OwnerClaim, claims: true, served: []string{"claim"}},
		{configured: OwnerPod, pods: true, served: []string{"pod"}},
		{configured: OwnerAuto, claims: true, pods: true, served: []string{"claim", "pod"}},
		// Unset is the DRA path the agent started as.
		{configured: "", claims: true, served: []string{"claim"}},
	} {
		t.Run(string(test.configured), func(t *testing.T) {
			a := New(Config{NodeName: testNode, SessionOwnerKind: test.configured})
			assert.Equal(t, test.claims, a.serves(OwnerClaim))
			assert.Equal(t, test.pods, a.serves(OwnerPod))
			assert.Equal(t, test.served, a.servedKinds())

			// A request naming an owner this agent does not serve is refused,
			// not silently routed to the other path.
			for kind, owner := range map[OwnerKind]remoteagent.SessionOwner{
				OwnerClaim: remoteagent.SessionOwner_SESSION_OWNER_CLAIM,
				OwnerPod:   remoteagent.SessionOwner_SESSION_OWNER_POD,
			} {
				got, err := a.serveOwner(owner)
				assert.Equal(t, kind, got)
				if a.serves(kind) {
					assert.NoError(t, err)
					continue
				}
				assert.Equal(t, codes.FailedPrecondition, status.Code(err))
			}
		})
	}
}

// Auto mode on a cluster that serves no DRA API keeps serving pod sessions.
func TestResolveOwnersWithoutDRAAPI(t *testing.T) {
	for _, test := range []struct {
		configured OwnerKind
		wantErr    bool
		wantServed []string
	}{
		{configured: OwnerAuto, wantServed: []string{"pod"}},
		{configured: OwnerPod, wantServed: []string{"pod"}},
		{configured: OwnerClaim, wantErr: true, wantServed: []string{"claim"}},
	} {
		t.Run(string(test.configured), func(t *testing.T) {
			a := New(Config{
				NodeName:         testNode,
				SessionOwnerKind: test.configured,
				ClientSets:       pkgflags.ClientSets{Core: fake.NewClientset()},
			})
			err := a.resolveOwners()
			if test.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, test.wantServed, a.servedKinds())
		})
	}
}

// The owner in the request decides the path, so a claim-owned request on an
// agent serving pod sessions is refused instead of being taken for a pod.
func TestEnsureSessionRoutesByRequestOwner(t *testing.T) {
	pod := testRemotePod(t, podClaims())
	a := newPodModeAgent(t, pod)
	req := &remoteagent.EnsureSessionRequest{
		Session:        remotegpu.SessionToken(string(pod.UID), "app"),
		ClaimUid:       string(pod.UID),
		ClaimNamespace: pod.Namespace,
		ClaimName:      pod.Name,
	}

	// No owner named = claim (older DRA-path callers).
	_, err := a.EnsureSession(context.Background(), req)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))

	req.Owner = remoteagent.SessionOwner_SESSION_OWNER_POD
	_, err = a.EnsureSession(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, []string{req.Session}, a.store.TokensOfOwner(string(pod.UID)))
}
