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

// Routing by session owner: which owner a request names, which owners this
// agent serves, and the device snapshot each owner's sessions are built from.

import (
	"strings"

	"github.com/coldzerofear/vgpu-manager/pkg/api/remoteagent"
	"github.com/coldzerofear/vgpu-manager/pkg/client"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

// requestOwner is the owner a session request names. The zero value on the
// wire is a claim, so a DRA-path caller that predates pod-owned sessions
// keeps its meaning.
func requestOwner(owner remoteagent.SessionOwner) OwnerKind {
	if owner == remoteagent.SessionOwner_SESSION_OWNER_POD {
		return OwnerPod
	}
	return OwnerClaim
}

// serveOwner resolves the owner a request names, or refuses it when this
// agent serves no sessions of that kind -- a pod-owned request on a
// claim-only agent, or a claim on a cluster without the DRA API.
func (a *Agent) serveOwner(owner remoteagent.SessionOwner) (OwnerKind, error) {
	kind := requestOwner(owner)
	if !a.serves(kind) {
		return kind, status.Errorf(codes.FailedPrecondition,
			"agent on node %s serves %s sessions, not %s sessions",
			a.cfg.NodeName, strings.Join(a.servedKinds(), " and "), kind)
	}
	return kind, nil
}

// serves reports whether this agent routes sessions of kind: cfg.Serves,
// minus what startup found unavailable.
func (a *Agent) serves(kind OwnerKind) bool {
	switch kind {
	case OwnerClaim:
		return a.servesClaims
	case OwnerPod:
		return a.servesPods
	default:
		return false
	}
}

// servedKinds names what this agent serves, for logs and error messages.
func (a *Agent) servedKinds() []string {
	var kinds []string
	if a.servesClaims {
		kinds = append(kinds, string(OwnerClaim))
	}
	if a.servesPods {
		kinds = append(kinds, string(OwnerPod))
	}
	if len(kinds) == 0 {
		kinds = append(kinds, "no")
	}
	return kinds
}

// devices is the device snapshot sessions of kind are built from.
func (a *Agent) devices(kind OwnerKind) *NodeDevices {
	if kind == OwnerPod {
		return a.podDevices.Load()
	}
	return a.claimDevices.Load()
}

// addReady extends the readiness check (see Check) with one informer set's
// sync functions: auto mode wires two sets, and all of them must be synced
// before the agent answers.
func (a *Agent) addReady(synced ...cache.InformerSynced) {
	a.readySynced = append(a.readySynced, synced...)
	a.hasReady = func() bool {
		for _, hasSynced := range a.readySynced {
			if !hasSynced() {
				return false
			}
		}
		return true
	}
}

// checkDRAAPI reports whether the cluster serves the API claim sessions need.
// It runs before the claim informers start, because an unavailable API would
// otherwise leave them retrying a 404 forever: that never syncs, so the ready
// file the lupine-server container waits on is never written and the whole pod
// hangs with no obvious cause. v1 is required, not merely preferred: the
// driver allocates with consumable capacity, which only exists in
// resource.k8s.io/v1 (Kubernetes 1.34+), so the beta versions a cluster might
// still serve are of no use and are refused explicitly rather than failing
// later, deeper. Pod sessions watch pods and this node only, which is why
// auto mode treats this as a reason to skip claims instead of an error.
func (a *Agent) checkDRAAPI() error {
	return client.DRAAPIRequirement{
		Subject: "remote-agent claim sessions",
		Version: "v1",
		Remedy: "Upgrade the cluster to 1.34+, or run this agent with" +
			" --session-owner=pod (device-plugin path) or auto (whatever the cluster serves).",
	}.Check(a.cfg.ClientSets.Core.Discovery())
}

// resolveOwners settles what this agent serves before its informers start:
// claim sessions need the DRA API, and a cluster that serves none is an error
// for an agent configured for claims -- but not for an auto one, which then
// serves pod sessions alone.
func (a *Agent) resolveOwners() error {
	if !a.servesClaims {
		return nil
	}
	if err := a.checkDRAAPI(); err != nil {
		if a.cfg.SessionOwnerKind != OwnerAuto {
			return err
		}
		klog.Warningf("Skipping claim sessions: %v", err)
		a.servesClaims = false
	}
	return nil
}
