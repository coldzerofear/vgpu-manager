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
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/device/remotegpu"
	"github.com/coldzerofear/vgpu-manager/pkg/scheduler/reason"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	extenderv1 "k8s.io/kube-scheduler/extender/v1"
)

// perfServerNode is an 8-GPU server with NVLink topology: GPUs 0-3 on NUMA 0,
// 4-7 on NUMA 1, with (0,1)(2,3)(4,5)(6,7) as the strongest pairs.
func perfServerNode(t *testing.T, name string) (corev1.Node, device.NodeDeviceInfo) {
	t.Helper()
	node := buildPerfNodes(1)[0]
	node.Name = name
	node.Labels = map[string]string{util.NodeRemoteServerLabel: "true"}
	endpoints, err := remotegpu.ServerEndpointInfo{
		ServerEndpoint: "http://10.0.0.2:8080",
		AgentEndpoint:  "grpc://10.0.0.2:9090",
	}.Encode()
	require.NoError(t, err)
	node.Annotations[util.NodeRemoteEndpointsAnnotation] = endpoints
	var devs device.NodeDeviceInfo
	require.NoError(t, devs.Decode(node.Annotations[util.NodeDeviceRegisterAnnotation]))
	return node, devs
}

// boundRemotePod is a remote pod already running on consumer with devices on server.
func boundRemotePod(t *testing.T, name, server, consumer string, claims ...device.DeviceClaim) *corev1.Pod {
	t.Helper()
	pod := remotePod(name, len(claims), int(claims[0].Cores), int(claims[0].Memory))
	pod.Spec.NodeName = consumer
	text, err := device.PodDeviceClaim{{Name: "cont1", DeviceClaims: claims}}.MarshalText()
	require.NoError(t, err)
	pod.Annotations[util.PodPredicateNodeAnnotation] = server
	pod.Annotations[util.PodVGPUPreAllocAnnotation] = text
	return pod
}

func claimIDs(t *testing.T, pod *corev1.Pod) []int {
	t.Helper()
	var claims device.PodDeviceClaim
	require.NoError(t, claims.UnmarshalText(pod.Annotations[util.PodVGPUPreAllocAnnotation]))
	var ids []int
	for _, c := range claims {
		for _, d := range c.DeviceClaims {
			ids = append(ids, d.Id)
		}
	}
	sort.Ints(ids)
	return ids
}

func Test_RemoteScenario_NodePolicyAcrossServers(t *testing.T) {
	for policy, want := range map[util.SchedulerPolicy]string{
		util.BinpackPolicy: "server-b",
		util.SpreadPolicy:  "server-a",
	} {
		t.Run(string(policy), func(t *testing.T) {
			serverA, _ := remoteServerNode(t, "server-a")
			serverB, devsB := remoteServerNode(t, "server-b")
			used := boundRemotePod(t, "used", "server-b", "consumer-b",
				device.DeviceClaim{Id: devsB[0].Id, Uuid: devsB[0].Uuid, Cores: 50, Memory: 4096})
			fixture := newRemoteFixture(t, []corev1.Node{serverA, serverB}, used)
			pod := remotePod("new", 1, 20, 1024)
			pod.Annotations[util.NodeSchedulerPolicyAnnotation] = string(policy)
			pod = fixture.createPod(t, pod)

			result := fixture.run(pod, liveFilter)

			require.Empty(t, result.Error)
			assert.Equal(t, []string{"consumer-a", "consumer-b"}, NodeNamesOfResult(result))
			assert.Equal(t, want, fixture.getPod(t, pod).Annotations[util.PodPredicateNodeAnnotation])
		})
	}
}

func Test_RemoteScenario_DevicePolicyOnServer(t *testing.T) {
	server, devs := remoteServerNode(t, "server")
	usedID := devs[0].Id
	for _, policy := range []util.SchedulerPolicy{util.BinpackPolicy, util.SpreadPolicy} {
		t.Run(string(policy), func(t *testing.T) {
			used := boundRemotePod(t, "used", "server", "consumer-b",
				device.DeviceClaim{Id: usedID, Uuid: devs[0].Uuid, Cores: 50, Memory: 4096})
			fixture := newRemoteFixture(t, []corev1.Node{server}, used)
			pod := remotePod("new", 1, 20, 1024)
			pod.Annotations[util.DeviceSchedulerPolicyAnnotation] = string(policy)
			pod = fixture.createPod(t, pod)

			require.Empty(t, fixture.run(pod, liveFilter).Error)

			ids := claimIDs(t, fixture.getPod(t, pod))
			require.Len(t, ids, 1)
			if policy == util.BinpackPolicy {
				assert.Equal(t, usedID, ids[0], "binpack shares the used GPU")
			} else {
				assert.NotEqual(t, usedID, ids[0], "spread picks an idle GPU")
			}
		})
	}
}

func Test_RemoteScenario_TopologyOnServer(t *testing.T) {
	server, devs := perfServerNode(t, "server")
	numaOf := map[int]int{}
	for _, d := range devs {
		numaOf[d.Id] = d.Numa
	}
	t.Run("numa-strict", func(t *testing.T) {
		fixture := newRemoteFixture(t, []corev1.Node{server})
		pod := remotePod("numa", 4, 20, 1024)
		pod.Annotations[util.DeviceTopologyModeAnnotation] = string(util.NUMATopologyStrict)
		pod = fixture.createPod(t, pod)

		require.Empty(t, fixture.run(pod, liveFilter).Error)

		ids := claimIDs(t, fixture.getPod(t, pod))
		require.Len(t, ids, 4)
		for _, id := range ids {
			assert.Equal(t, numaOf[ids[0]], numaOf[id], "all GPUs on one NUMA node: %v", ids)
		}
	})
	t.Run("link", func(t *testing.T) {
		fixture := newRemoteFixture(t, []corev1.Node{server})
		pod := remotePod("link", 2, 20, 1024)
		pod.Annotations[util.DeviceTopologyModeAnnotation] = string(util.LinkTopology)
		pod = fixture.createPod(t, pod)

		require.Empty(t, fixture.run(pod, liveFilter).Error)

		ids := claimIDs(t, fixture.getPod(t, pod))
		require.Len(t, ids, 2)
		assert.True(t, ids[0]/2 == ids[1]/2 && ids[1] == ids[0]+1, "an NVLink pair: %v", ids)
	})
}

// Cross-pod topology is turned off for remote pods, with a warning event that
// only a live Filter may send.
func Test_RemoteScenario_CrossPodTopologyDisabled(t *testing.T) {
	server, _ := perfServerNode(t, "server")
	newPod := func(name string) *corev1.Pod {
		pod := remotePod(name, 2, 20, 1024)
		pod.Annotations[util.DeviceTopologyModeAnnotation] = string(util.LinkTopology)
		pod.Annotations[util.CrossPodTopologyAnnotation] = "true"
		pod.Labels = map[string]string{util.CoschedulingPodGroupLabel: "gang"}
		return pod
	}
	t.Run("live", func(t *testing.T) {
		fixture := newRemoteFixture(t, []corev1.Node{server})
		pod := fixture.createPod(t, newPod("live"))

		result := fixture.run(pod, liveFilter)

		require.Empty(t, result.Error)
		assert.Len(t, NodeNamesOfResult(result), 2)
		assert.True(t, slices.ContainsFunc(fixture.events(), func(e string) bool {
			return strings.Contains(e, reason.EventTopologyFallback)
		}))
	})
	t.Run("dry-run sends no event", func(t *testing.T) {
		fixture := newRemoteFixture(t, []corev1.Node{server})

		result := fixture.run(newPod("dry"), dryRunFilter)

		assert.Len(t, NodeNamesOfResult(result), 2)
		assert.Empty(t, fixture.events())
	})
}

// With nodeCacheCapable, candidates come as names and the answer is names.
func Test_RemoteScenario_NodeNamesForm(t *testing.T) {
	server, _ := remoteServerNode(t, "server")
	local, _ := buildNodeList()
	consumerA, consumerB := remoteConsumerNode("consumer-a"), remoteConsumerNode("consumer-b")
	fixture := newRemoteFixture(t, []corev1.Node{server, local[1], consumerA, consumerB})
	pod := fixture.createPod(t, remotePod("names", 1, 20, 1024))
	names := []string{"consumer-a", local[1].Name, "consumer-b"}

	result := fixture.filter.Filter(context.Background(), extenderv1.ExtenderArgs{Pod: pod, NodeNames: &names})

	require.Empty(t, result.Error)
	require.NotNil(t, result.NodeNames)
	assert.Equal(t, []string{"consumer-a", "consumer-b"}, *result.NodeNames)
	assert.Equal(t, reason.Phrase(reason.NodeNotRemoteConsumer), result.FailedNodes[local[1].Name])
}

func Test_RemoteScenario_ConsumerRoles(t *testing.T) {
	server, _ := remoteServerNode(t, "server")
	serverConsumer := server
	serverConsumer.Labels = map[string]string{
		util.NodeRemoteServerLabel:   "true",
		util.NodeRemoteConsumerLabel: "true",
	}
	noNumber := remoteConsumerNode("no-number")
	noNumber.Status.Allocatable = corev1.ResourceList{}
	fixture := newRemoteFixture(t, []corev1.Node{serverConsumer})
	pod := fixture.createPod(t, remotePod("roles", 1, 20, 1024))

	result := fixture.run(pod, liveFilter, serverConsumer, noNumber, fixture.consumers[0])

	require.Empty(t, result.Error)
	assert.Equal(t, []string{"server", "consumer-a"}, NodeNamesOfResult(result), "a server may also run remote pods")
	assert.Equal(t, reason.Phrase(reason.NodeNotRemoteConsumer), result.FailedNodes["no-number"])
	assert.Equal(t, "server", fixture.getPod(t, pod).Annotations[util.PodPredicateNodeAnnotation])
}

func Test_RemoteScenario_MemoryPolicyAndGPUType(t *testing.T) {
	server, devs := remoteServerNode(t, "server")
	t.Run("virtual memory on a physical server", func(t *testing.T) {
		fixture := newRemoteFixture(t, []corev1.Node{server})
		pod := remotePod("virtual", 1, 20, 1024)
		pod.Annotations[util.MemorySchedulerPolicyAnnotation] = util.VirtualMemoryPolicy.String()

		result := fixture.run(pod, dryRunFilter)

		assert.Empty(t, NodeNamesOfResult(result))
		assert.Contains(t, result.FailedNodes["consumer-a"], reason.Phrase(reason.RemoteServerUnfit))
		assert.Contains(t, result.FailedNodes["consumer-a"], reason.Phrase(reason.NodeMemoryTypeMismatch))
	})
	t.Run("include GPU type", func(t *testing.T) {
		fixture := newRemoteFixture(t, []corev1.Node{server})
		pod := remotePod("type", 1, 20, 1024)
		pod.Annotations[util.PodIncludeGpuTypeAnnotation] = devs[3].Type
		pod = fixture.createPod(t, pod)

		require.Empty(t, fixture.run(pod, liveFilter).Error)

		for _, id := range claimIDs(t, fixture.getPod(t, pod)) {
			assert.Equal(t, devs[3].Type, devs[id].Type)
		}
	})
}

// Local and remote pods never use each other's capacity, and only a remote
// pod gets more than one node back.
func Test_RemoteScenario_LocalAndRemoteIsolation(t *testing.T) {
	server, _ := remoteServerNode(t, "server")
	local, _ := buildNodeList()
	fixture := newRemoteFixture(t, []corev1.Node{server})

	fill := fixture.createPod(t, remotePod("fill-server", 4, 100, 1024))
	assert.Equal(t, []string{"consumer-a", "consumer-b"}, NodeNamesOfResult(fixture.run(fill, liveFilter)))

	localPod := fixture.createPod(t, dryRunPod("local", 1, 50, 1024))
	localResult := fixture.run(localPod, liveFilter, server, local[0], local[1])
	require.Empty(t, localResult.Error)
	require.Len(t, NodeNamesOfResult(localResult), 1, "a local pod still gets exactly one node")
	assert.NotEqual(t, "server", NodeNamesOfResult(localResult)[0])
	assert.Equal(t, reason.Phrase(reason.NodeIsRemoteServer), localResult.FailedNodes["server"])

	more := fixture.createPod(t, remotePod("more", 1, 10, 1024))
	moreResult := fixture.run(more, liveFilter)
	assert.Empty(t, NodeNamesOfResult(moreResult), "the full server is not refilled from local nodes")
	assert.Equal(t, reason.Phrase(reason.RemoteServerUnfit), moreResult.FailedNodes["consumer-a"])
}
