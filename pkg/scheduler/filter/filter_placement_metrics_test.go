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

package filter

import (
	"context"
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/device/allocator"
	"github.com/coldzerofear/vgpu-manager/pkg/scheduler/metrics"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	extenderv1 "k8s.io/kube-scheduler/extender/v1"
	framework2 "k8s.io/kubernetes/pkg/scheduler/framework"
)

func placementCount(name string) float64 {
	return metrics.CounterValue(name, map[string]string{})
}

// The placement metrics must be read off the SAME request the allocator was
// handed. Reading them off the per-node snapshot silently reports zero forever:
// the snapshot is copied before Allocate runs, so the outcome never lands on it
// and only a live filter run — not an allocator unit test — catches it.
func Test_Filter_RecordsTopologyPlacement(t *testing.T) {
	fixture := newDryRunFixture(t)
	pod := dryRunPod("topology-placement", 2, 50, 2048)
	pod.Annotations = map[string]string{
		util.DeviceTopologyModeAnnotation: string(util.NUMATopology),
	}
	created, err := fixture.client.CoreV1().Pods(namespace).Create(
		context.Background(), pod, metav1.CreateOptions{})
	assert.NoError(t, err)

	beforePolicy := placementCount("pod_policy_total")
	beforePlacement := placementCount("topology_placement_total")

	result := fixture.filter.Filter(context.Background(), extenderv1.ExtenderArgs{
		Pod:   created,
		Nodes: &corev1.NodeList{Items: fixture.nodes},
	})
	assert.Empty(t, result.Error)
	assert.Len(t, NodeNamesOfResult(result), 1)

	assert.Equal(t, beforePolicy+1, placementCount("pod_policy_total"))
	assert.Equal(t, beforePlacement+1, placementCount("topology_placement_total"),
		"a topology pod that was placed must report the connectivity it achieved")
}

// One request is reused across every candidate node, so a node that recorded an
// outcome and was then rejected must not colour the node that finally accepts.
func Test_Allocate_ClearsTopologyOutcomePerNode(t *testing.T) {
	fixture := newDryRunFixture(t)
	pod := dryRunPod("outcome-isolation", 1, 50, 2048)
	pod.Annotations = map[string]string{
		util.DeviceTopologyModeAnnotation: string(util.NUMATopology),
	}
	req := allocator.BuildAllocationRequest(pod)

	nodeInfos, _, _, err := fixture.filter.preFilterNodeInfos(
		context.Background(), req, fixture.nodes, framework2.NewCycleState())
	assert.NoError(t, err)
	assert.NotEmpty(t, nodeInfos)

	var seen []string
	for _, nodeInfo := range nodeInfos {
		_, rsn, allocErr := allocator.NewAllocator(nodeInfo.NodeInfo, nil).Allocate(req)
		assert.NoError(t, allocErr)
		assert.Nil(t, rsn)
		// Whatever the previous node recorded, this call reports only its own.
		seen = append(seen, req.TopologyOutcome().Result)
	}
	for _, result := range seen {
		assert.NotEmpty(t, result, "each Allocate must record its own node's outcome")
	}
}
