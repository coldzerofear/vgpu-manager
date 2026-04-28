package claimresolve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/util/sets"
)

type PartitionDetail struct {
	Key        string
	Containers []string
	Requests   []string
}

type PartitionInfo struct {
	RequestToPartition map[string]string
	Partitions         map[string]PartitionDetail
	Fallback           bool
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

	graph := map[string]sets.Set[string]{}
	resolvedEdges := 0
	for _, pod := range reservedPods {
		for _, container := range getAllPodContainers(pod) {
			containerKey := buildContainerKey(pod, container.Kind, container.Name)
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
	}

	if len(info.RequestToPartition) == 0 {
		info.Fallback = true
	}
	return info, nil
}

func buildContainerKey(pod *corev1.Pod, kind, containerName string) string {
	podUID := string(pod.UID)
	if podUID == "" {
		podUID = pod.Namespace + "/" + pod.Name
	}
	return sanitizePartitionToken(podUID) + "/" + sanitizePartitionToken(kind) + "/" + sanitizePartitionToken(containerName)
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

	containerList := sets.List(containers)
	requestList := sets.List(requests)
	return PartitionDetail{Containers: containerList, Requests: requestList}
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

type containerRef struct {
	Name   string
	Claims []corev1.ResourceClaim
	Kind   string
}

func getAllPodContainers(pod *corev1.Pod) []containerRef {
	all := make([]containerRef, 0, len(pod.Spec.InitContainers)+len(pod.Spec.Containers))
	for _, c := range pod.Spec.InitContainers {
		all = append(all, containerRef{Name: c.Name, Claims: c.Resources.Claims, Kind: "init"})
	}
	for _, c := range pod.Spec.Containers {
		all = append(all, containerRef{Name: c.Name, Claims: c.Resources.Claims, Kind: "app"})
	}
	return all
}
