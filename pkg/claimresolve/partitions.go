package claimresolve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/coldzerofear/vgpu-manager/pkg/webhook/common"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/util/sets"
)

// PartitionDetail describes one connected component of the
// container <-> mainRequest graph: the containers that share at least one
// vGPU mainRequest, and the set of vGPU mainRequests they collectively use.
type PartitionDetail struct {
	Key        string
	Containers []string
	Requests   []string
}

// PartitionCandidate names a single container that could be the caller of a
// register request keyed to a given partition. Multiple candidates may exist
// for the same key (e.g. an init+app pair sharing a request, or a stale
// fallback key still in a claim annotation from an earlier Prepare).
type PartitionCandidate struct {
	Pod           *corev1.Pod
	ContainerName string
	Kind          string // "init" or "app"
}

// PartitionInfo is the output of partition resolution.
//
// CandidatesByKey is the field the register-resolve path actually uses:
// it maps every plausible partition key — both the real key computed by
// buildPartitionKey for the CURRENT reservedFor snapshot and the per-request
// fallback key that an earlier Prepare may have baked into a claim annotation
// — to the set of containers that key could refer to. The server iterates
// candidates and picks the one with live cgroup PIDs.
//
// This makes the resolve robust to the common case where Prepare runs while
// the claim is only partially reserved (so unused requests fall back to
// per-request keys) and another pod later joins reservedFor to consume those
// requests.
//
// Known limitation (bug-2, multi-request container w/ eager Prepare):
// if a future pod's single container references multiple requests that were
// emitted as separate per-request fallback partitions at Prepare time, that
// container will receive multiple Devices whose env vars collide on
// MANAGER_CLIENT_REGISTER_UUID and whose mounts collide on the partition
// directory. The CandidatesByKey machinery here lets register succeed for
// whichever UUID happens to win the env collision, but the other partitions'
// pids files will never be written. Properly fixing this requires deferring
// UUID assignment from Prepare (eager) to first-register (lazy), which is a
// larger refactor than the current pass.
type PartitionInfo struct {
	RequestToPartition map[string]string
	Partitions         map[string]PartitionDetail
	CandidatesByKey    map[string][]PartitionCandidate
	Fallback           bool
}

// FallbackPartitionKey returns the partition key Prepare emits for a vGPU
// request when no container in the current reservedFor snapshot uses it.
// Exposed so resolver code can register the same key as an alias in
// CandidatesByKey and so any other consumer that needs the prepare-time
// fallback name can avoid drift.
func FallbackPartitionKey(request string) string {
	return sanitizePartitionToken(request)
}

func ResolveClaimVGPUPartitions(
	ctx context.Context,
	reader Reader,
	claim *resourceapi.ResourceClaim,
	driverName string,
	isVGPUDeviceRequest DeviceRequestClassifier,
	isVGPUSubRequest SubRequestClassifier,
) (*PartitionInfo, error) {
	allocatedRequests := GetAllocatedVGPURequests(ctx, claim, driverName, isVGPUDeviceRequest, isVGPUSubRequest)
	return ResolveClaimVGPUPartitionsFromAllocatedRequests(ctx, reader, claim, allocatedRequests)
}

func ResolveClaimVGPUPartitionsFromAllocatedRequests(
	ctx context.Context,
	reader Reader,
	claim *resourceapi.ResourceClaim,
	allocatedRequests sets.Set[string],
) (*PartitionInfo, error) {
	info := &PartitionInfo{
		RequestToPartition: map[string]string{},
		Partitions:         map[string]PartitionDetail{},
		CandidatesByKey:    map[string][]PartitionCandidate{},
	}
	if claim == nil {
		info.Fallback = true
		return info, nil
	}
	if allocatedRequests.Len() == 0 {
		return info, nil
	}

	reservedPods, err := GetReservedPods(ctx, reader, claim)
	if err != nil {
		return nil, err
	}
	if len(reservedPods) == 0 {
		info.Fallback = true
		return info, nil
	}

	// containerCands maps the synthetic containerKey used by the graph back
	// to the (pod, container) pair that produced it. Used after the graph
	// walk to attach candidates to real partition keys.
	containerCands := map[string]PartitionCandidate{}

	graph := map[string]sets.Set[string]{}
	resolvedEdges := 0
	for _, pod := range reservedPods {
		for _, container := range common.GetAllPodContainers(pod) {
			containerKey := buildContainerKey(pod, string(container.Kind), container.Name)
			cand := PartitionCandidate{
				Pod:           pod,
				ContainerName: container.Name,
				Kind:          string(container.Kind),
			}
			for _, claimRef := range container.Claims {
				actualClaimName, ok, err := ResolveActualClaimNameForPodClaim(pod, claimRef.Name)
				if err != nil {
					return nil, err
				}
				if !ok || actualClaimName != claim.Name {
					continue
				}

				actualRequests := ResolveActualAllocatedRequestsForClaimRef(claimRef, allocatedRequests)
				for _, request := range actualRequests {
					requestNode := buildRequestNode(request)
					linkGraph(graph, containerKey, requestNode)
					resolvedEdges++

					// Fallback alias: a previous Prepare that ran before this
					// pod joined reservedFor may have baked the sanitized
					// request name into the claim annotation as the
					// partitionKey. Register that name as an alias here so the
					// resolver still finds this candidate.
					fallbackKey := FallbackPartitionKey(request)
					info.CandidatesByKey[fallbackKey] = appendUniqueCandidate(info.CandidatesByKey[fallbackKey], cand)

					containerCands[containerKey] = cand
				}
			}
		}
	}

	if resolvedEdges == 0 {
		info.Fallback = true
		return info, nil
	}

	visited := sets.New[string]()
	for node := range graph {
		if visited.Has(node) {
			continue
		}
		partition := walkPartition(graph, node, visited)
		if len(partition.Requests) == 0 {
			continue
		}
		partition.Key = buildPartitionKey(partition.Containers, partition.Requests)
		info.Partitions[partition.Key] = partition
		for _, request := range partition.Requests {
			info.RequestToPartition[request] = partition.Key
		}
		// Real-key aliases. The same candidate may already be present under
		// the fallback key; appendUniqueCandidate dedupes by identity.
		for _, ck := range partition.Containers {
			if cand, ok := containerCands[ck]; ok {
				info.CandidatesByKey[partition.Key] = appendUniqueCandidate(info.CandidatesByKey[partition.Key], cand)
			}
		}
	}

	if len(info.RequestToPartition) == 0 {
		info.Fallback = true
	}
	return info, nil
}

// appendUniqueCandidate appends cand to the slice unless an entry referring to
// the same (pod uid, container name) is already present.
func appendUniqueCandidate(list []PartitionCandidate, cand PartitionCandidate) []PartitionCandidate {
	for _, c := range list {
		if c.Pod.UID == cand.Pod.UID && c.ContainerName == cand.ContainerName {
			return list
		}
	}
	return append(list, cand)
}

func buildContainerKey(pod *corev1.Pod, kind, containerName string) string {
	return string(pod.UID) + "/" + kind + "/" + containerName
}

func buildRequestNode(request string) string {
	return "r:" + request
}

func linkGraph(graph map[string]sets.Set[string], left, right string) {
	if graph[left] == nil {
		graph[left] = sets.New[string]()
	}
	if graph[right] == nil {
		graph[right] = sets.New[string]()
	}
	graph[left].Insert(right)
	graph[right].Insert(left)
}

func walkPartition(graph map[string]sets.Set[string], start string, visited sets.Set[string]) PartitionDetail {
	stack := []string{start}
	containers := sets.New[string]()
	requests := sets.New[string]()

	for len(stack) > 0 {
		last := len(stack) - 1
		node := stack[last]
		stack = stack[:last]
		if visited.Has(node) {
			continue
		}
		visited.Insert(node)

		switch {
		case strings.HasPrefix(node, "r:"):
			requests.Insert(strings.TrimPrefix(node, "r:"))
		default:
			containers.Insert(node)
		}

		for _, next := range sets.List(graph[node]) {
			if !visited.Has(next) {
				stack = append(stack, next)
			}
		}
	}

	return PartitionDetail{
		Containers: sets.List(containers),
		Requests:   sets.List(requests),
	}
}

func buildPartitionKey(containers, requests []string) string {
	if len(containers) == 1 {
		parts := strings.SplitN(containers[0], "/", 2)
		if len(parts) == 2 {
			return fmt.Sprintf("pod-%s-%s", shortHash(parts[0]), parts[1])
		}
		return fmt.Sprintf("pod-%s", shortHash(containers[0]))
	}
	payload := strings.Join(containers, ",") + "|" + strings.Join(requests, ",")
	return "cg-" + shortHash(payload)
}

func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:12]
}

// sanitizePartitionToken normalizes a string so it's safe to use as a path
// component / partition key. Lowercase letters, digits, '-', '_', '.' are
// kept; everything else collapses to '-'.
func sanitizePartitionToken(value string) string {
	if value == "" {
		return "unknown"
	}
	var b strings.Builder
	b.Grow(len(value))
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}
