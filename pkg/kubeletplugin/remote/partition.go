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
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/Masterminds/semver"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/util/sets"
)

// Session identity (design D8, v2.2): a lupine session is one accounting
// unit on the server, so it must map to the set of processes that share one
// quota. That set is a *partition* — the connected component of the
// "container <-> claim request" graph over the claim's reserved pods,
// exactly what the local path uses to carve per-container config
// directories (pkg/claimresolve). A claim consumed by one container is the
// degenerate single-partition case; a multi-request claim spread over
// several containers yields one session per partition.
//
// Each partition gets a random, unpredictable token. It is persisted as a
// claim annotation so that NodePrepare retries and plugin restarts reuse
// the same session (the agent keys the session directory by it):
//
//	<driver>/session-<shortHash(partitionKey)>: <token>
//
// The annotation key hashes the partition key because partition keys embed
// pod UIDs / container names and would exceed the 63-char name limit.

const (
	sessionAnnotationPrefix = util.DRADriverName + "/session-"
	tokenBytes              = 16
)

// SessionAnnotationKey returns the claim annotation key holding the token
// of `partitionKey`.
func SessionAnnotationKey(partitionKey string) string {
	sum := sha256.Sum256([]byte(partitionKey))
	return sessionAnnotationPrefix + hex.EncodeToString(sum[:])[:16]
}

// NewSessionToken mints a random session token (32 hex chars). It satisfies
// the agent's token grammar and the lupine header constraints.
func NewSessionToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("mint session token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// MainRequestName maps an allocation result's request name (which may be
// "<request>/<subrequest>" for firstAvailable requests) back to the claim's
// main request name; "" when the claim spec has no such request.
func MainRequestName(claim *resourceapi.ResourceClaim, requestName string) string {
	if claim == nil {
		return ""
	}
	for _, req := range claim.Spec.Devices.Requests {
		if req.Exactly != nil && req.Name == requestName {
			return req.Name
		}
		for _, sub := range req.FirstAvailable {
			if req.Name+"/"+sub.Name == requestName {
				return req.Name
			}
		}
	}
	return ""
}

// FilterResultsByRequests keeps the allocation results whose main request is
// in `requests` (all results when requests is empty — the legacy 1:1 form).
func FilterResultsByRequests(claim *resourceapi.ResourceClaim, results []resourceapi.DeviceRequestAllocationResult, requests []string) []resourceapi.DeviceRequestAllocationResult {
	if len(requests) == 0 {
		return results
	}
	want := sets.New(requests...)
	var out []resourceapi.DeviceRequestAllocationResult
	for _, r := range results {
		if want.Has(MainRequestName(claim, r.Request)) {
			out = append(out, r)
		}
	}
	return out
}

// resultDevice pairs one allocation result with the published device it
// resolved to.
type resultDevice struct {
	// index is the position in claim.Status.Allocation.Devices.Results.
	index  int
	result resourceapi.DeviceRequestAllocationResult
	info   *DeviceInfo
	// mainRequest is the claim-level request name (subrequests folded).
	mainRequest string
}

// endpointInfo is one GPU node a partition spans, identified by its agent
// (the address sessions are established at). serverEndpoint is the
// published lupine-server address, which may be empty until the agent has
// reported one; EnsureSessions resolves the final value.
type endpointInfo struct {
	serverEndpoint string
	agentEndpoint  string
}

// endpointInfosOf collects the distinct nodes behind devices, keyed by agent
// endpoint and sorted by it, so the LUPINE_SERVER order (= virtual device
// numbering) is the same however the devices were listed. When the same
// agent shows up with and without a published server endpoint, the
// non-empty one is kept.
func endpointInfosOf(devices []resultDevice) []endpointInfo {
	byAgent := map[string]endpointInfo{}
	for _, rd := range devices {
		info, ok := byAgent[rd.info.AgentEndpoint]
		if !ok {
			info = endpointInfo{agentEndpoint: rd.info.AgentEndpoint}
		}
		if info.serverEndpoint == "" {
			info.serverEndpoint = rd.info.Endpoint
		}
		byAgent[rd.info.AgentEndpoint] = info
	}
	out := make([]endpointInfo, 0, len(byAgent))
	for _, info := range byAgent {
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].agentEndpoint < out[j].agentEndpoint
	})
	return out
}

// partition is one session: the results (devices) it spans, the servers to
// dial, and the token that names it on those servers.
type partition struct {
	key       string
	requests  []string
	results   []resultDevice
	endpoints []endpointInfo // sorted, deduplicated
	token     string
}

// buildPartitions groups resolved results by partition key. requestToKey is
// the resolver output (request -> partition key); requests not present fall
// back to a per-request partition, mirroring the local path.
func buildPartitions(devices []resultDevice, requestToKey map[string]string) []*partition {
	byKey := map[string]*partition{}
	var order []string
	for _, rd := range devices {
		key := requestToKey[rd.mainRequest]
		if key == "" {
			key = PartitionFallbackKey(rd.mainRequest)
		}
		p := byKey[key]
		if p == nil {
			p = &partition{key: key}
			byKey[key] = p
			order = append(order, key)
		}
		p.results = append(p.results, rd)
	}
	out := make([]*partition, 0, len(order))
	for _, key := range order {
		p := byKey[key]
		reqs := sets.New[string]()
		for _, rd := range p.results {
			reqs.Insert(rd.mainRequest)
		}
		p.requests = sets.List(reqs)
		p.endpoints = endpointInfosOf(p.results)
		out = append(out, p)
	}
	return out
}

// cudaFloor returns the lowest server CUDA version across all results.
func cudaFloor(devices []resultDevice) *semver.Version {
	var floor *semver.Version
	for _, rd := range devices {
		if floor == nil || rd.info.CUDAVersion.Compare(floor) < 0 {
			floor = rd.info.CUDAVersion
		}
	}
	return floor
}

// cdiDeviceID names one allocation result inside the claim's CDI spec, in
// the same shape the local path uses (<request>-<device>-share-<shareID>).
func cdiDeviceID(rd resultDevice, ordinal int) string {
	req := sanitizeToken(rd.result.Request)
	dev := sanitizeToken(rd.result.Device)
	if rd.result.ShareID != nil && *rd.result.ShareID != "" {
		return fmt.Sprintf("%s-%s-share-%s", req, dev, sanitizeToken(string(*rd.result.ShareID)))
	}
	return fmt.Sprintf("%s-%s-%d", req, dev, ordinal)
}

func sanitizeToken(value string) string {
	if value == "" {
		return "unknown"
	}
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}

// PartitionFallbackKey is the partition key used for a request when the
// resolver could not place it (no reserved pods yet, or a request no
// container references): one partition per request, mirroring the local
// path. Shared with the monitor so both sides derive the same token key.
func PartitionFallbackKey(mainRequest string) string {
	return sanitizeToken(mainRequest)
}
