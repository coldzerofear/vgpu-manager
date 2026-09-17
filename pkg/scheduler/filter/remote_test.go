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
	"strings"
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/device"
	"github.com/coldzerofear/vgpu-manager/pkg/device/remotegpu"
	"github.com/coldzerofear/vgpu-manager/pkg/scheduler/reason"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
	extenderv1 "k8s.io/kube-scheduler/extender/v1"
)

// remoteServerNode is a GPU node labeled to serve remote pods.
func remoteServerNode(t *testing.T, name string) (corev1.Node, device.NodeDeviceInfo) {
	t.Helper()
	nodes, devices := buildNodeList()
	node := nodes[0]
	registered := devices[node.Name]
	node.Name = name
	node.Labels[util.NodeRemoteServerLabel] = "true"
	endpoints, err := remotegpu.ServerEndpointInfo{
		ServerEndpoint: "http://10.0.0.1:8080",
		AgentEndpoint:  "grpc://10.0.0.1:9090",
	}.Encode()
	require.NoError(t, err)
	node.Annotations[util.NodeRemoteEndpointsAnnotation] = endpoints
	return node, registered
}

// remoteConsumerNode runs remote pods and has no GPUs of its own.
func remoteConsumerNode(name string) corev1.Node {
	number := corev1.ResourceList{corev1.ResourceName(util.VGPUNumberResourceName): resource.MustParse("40")}
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{util.NodeRemoteConsumerLabel: "true"},
		},
		Status: corev1.NodeStatus{Capacity: number, Allocatable: number},
	}
}

func remotePod(name string, number, cores, memory int) *corev1.Pod {
	pod := dryRunPod(name, number, cores, memory)
	pod.Annotations = map[string]string{util.VGPUAccessModeAnnotation: util.AccessModeRemote}
	return pod
}

type remoteFixture struct {
	filter    *gpuFilter
	client    *fake.Clientset
	recorder  *record.FakeRecorder
	consumers []corev1.Node
}

// newRemoteFixture puts the servers and pods in the informer caches. The
// consumers are passed to Filter as candidates, the way kube-scheduler does.
func newRemoteFixture(t *testing.T, servers []corev1.Node, pods ...*corev1.Pod) *remoteFixture {
	t.Helper()
	k8sClient := fake.NewClientset()
	for i := range servers {
		_, err := k8sClient.CoreV1().Nodes().Create(context.Background(), &servers[i], metav1.CreateOptions{})
		require.NoError(t, err)
	}
	for _, pod := range pods {
		_, err := k8sClient.CoreV1().Pods(pod.Namespace).Create(context.Background(), pod, metav1.CreateOptions{})
		require.NoError(t, err)
	}
	factory := informers.NewSharedInformerFactory(k8sClient, 0)
	recorder := record.NewFakeRecorder(64)
	filterPredicate, err := New(k8sClient, factory, recorder, true, true)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())
	return &remoteFixture{
		filter:    filterPredicate,
		client:    k8sClient,
		recorder:  recorder,
		consumers: []corev1.Node{remoteConsumerNode("consumer-a"), remoteConsumerNode("consumer-b")},
	}
}

func (f *remoteFixture) createPod(t *testing.T, pod *corev1.Pod) *corev1.Pod {
	t.Helper()
	created, err := f.client.CoreV1().Pods(pod.Namespace).Create(context.Background(), pod, metav1.CreateOptions{})
	require.NoError(t, err)
	return created
}

func (f *remoteFixture) getPod(t *testing.T, pod *corev1.Pod) *corev1.Pod {
	t.Helper()
	got, err := f.client.CoreV1().Pods(pod.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
	require.NoError(t, err)
	return got
}

func (f *remoteFixture) run(pod *corev1.Pod, mode filterMode, nodes ...corev1.Node) *extenderv1.ExtenderFilterResult {
	if len(nodes) == 0 {
		nodes = f.consumers
	}
	return f.filter.filter(context.Background(), extenderv1.ExtenderArgs{
		Pod:   pod,
		Nodes: &corev1.NodeList{Items: nodes},
	}, mode)
}

func (f *remoteFixture) events() []string {
	var events []string
	for len(f.recorder.Events) > 0 {
		events = append(events, <-f.recorder.Events)
	}
	return events
}

func Test_RemoteFilter_PlacesPodOnServer(t *testing.T) {
	server, registered := remoteServerNode(t, "gpu-server")
	fixture := newRemoteFixture(t, []corev1.Node{server})
	pod := fixture.createPod(t, remotePod("remote-pod", 1, 50, 2048))

	result := fixture.run(pod, liveFilter)

	require.Empty(t, result.Error)
	assert.Equal(t, []string{"consumer-a", "consumer-b"}, NodeNamesOfResult(result))
	assert.Empty(t, result.FailedNodes)

	got := fixture.getPod(t, pod)
	assert.Equal(t, "gpu-server", got.Annotations[util.PodPredicateNodeAnnotation])
	assert.Equal(t, "gpu-server", got.Labels[util.PodMetricsNodeLabel])
	preAlloc := got.Annotations[util.PodVGPUPreAllocAnnotation]
	fromServer := false
	for _, dev := range registered {
		fromServer = fromServer || strings.Contains(preAlloc, dev.Uuid)
	}
	assert.True(t, fromServer, "pre-allocated devices %q must come from the server", preAlloc)

	// kube-scheduler may call Filter again for the placed pod: it keeps its devices.
	again := fixture.run(got, liveFilter)
	assert.Equal(t, []string{"consumer-a", "consumer-b"}, NodeNamesOfResult(again))
	assert.Equal(t, preAlloc, fixture.getPod(t, pod).Annotations[util.PodVGPUPreAllocAnnotation])
}

// A remote pod bound to a consumer still uses its server's devices.
func Test_RemoteFilter_CountsBoundRemotePodsOnServer(t *testing.T) {
	server, registered := remoteServerNode(t, "gpu-server")
	claims := make([]device.DeviceClaim, 0, len(registered))
	for _, dev := range registered {
		claims = append(claims, device.DeviceClaim{Id: dev.Id, Uuid: dev.Uuid, Cores: util.HundredCore, Memory: 1024})
	}
	claim := device.PodDeviceClaim{{Name: "cont1", DeviceClaims: claims}}
	preAlloc, err := claim.MarshalText()
	require.NoError(t, err)
	bound := remotePod("bound", len(registered), 100, 1024)
	bound.Spec.NodeName = "consumer-a"
	bound.Annotations[util.PodPredicateNodeAnnotation] = "gpu-server"
	bound.Annotations[util.PodVGPUPreAllocAnnotation] = preAlloc
	fixture := newRemoteFixture(t, []corev1.Node{server}, bound)

	pod := fixture.createPod(t, remotePod("no-room", 1, 50, 1024))
	result := fixture.run(pod, liveFilter)

	require.Empty(t, result.Error)
	assert.Empty(t, NodeNamesOfResult(result))
	for _, consumer := range fixture.consumers {
		assert.Equal(t, reason.Phrase(reason.RemoteServerUnfit), result.FailedNodes[consumer.Name])
	}
	events := fixture.events()
	require.Len(t, events, 1)
	assert.Contains(t, events[0], "Remote GPU servers: 0/1 nodes are available")
	assert.Contains(t, events[0], "(gpu-server)")
	assert.Empty(t, fixture.getPod(t, pod).Annotations[util.PodPredicateNodeAnnotation])
}

func Test_RemoteFilter_DryRun(t *testing.T) {
	server, _ := remoteServerNode(t, "gpu-server")
	fixture := newRemoteFixture(t, []corev1.Node{server})
	pod := remotePod("dryrun-remote", 1, 50, 2048)

	result := fixture.run(pod, dryRunFilter)

	assert.Empty(t, result.Error)
	assert.Equal(t, []string{"consumer-a", "consumer-b"}, NodeNamesOfResult(result))
	for _, action := range fixture.client.Actions() {
		if action.GetResource().Resource == "pods" {
			assert.Contains(t, []string{"list", "watch"}, action.GetVerb(), "dry-run must not write pods")
		}
	}
	assert.Empty(t, fixture.events())
	assert.Len(t, pod.Annotations, 1, "dry-run must not stamp the pod")
}

func Test_RemoteFilter_Rejections(t *testing.T) {
	local, _ := buildNodeList()
	badEndpoints, _ := remoteServerNode(t, "bad-endpoints")
	badEndpoints.Annotations[util.NodeRemoteEndpointsAnnotation] = "not json"
	noEndpoints, _ := remoteServerNode(t, "no-endpoints")
	delete(noEndpoints.Annotations, util.NodeRemoteEndpointsAnnotation)

	t.Run("no server", func(t *testing.T) {
		fixture := newRemoteFixture(t, nil)
		result := fixture.run(remotePod("no-server", 1, 50, 2048), dryRunFilter, fixture.consumers[0], local[0])

		assert.Empty(t, NodeNamesOfResult(result))
		assert.Contains(t, result.FailedNodes["consumer-a"], reason.Phrase(reason.NoRemoteServer))
		assert.Contains(t, result.FailedNodes["consumer-a"], util.NodeRemoteServerLabel)
		assert.Equal(t, reason.Phrase(reason.NodeNotRemoteConsumer), result.FailedNodes[local[0].Name])
	})
	t.Run("invalid server endpoints", func(t *testing.T) {
		fixture := newRemoteFixture(t, []corev1.Node{badEndpoints})
		result := fixture.run(remotePod("bad-endpoint", 1, 50, 2048), dryRunFilter)

		assert.Empty(t, NodeNamesOfResult(result))
		assert.Contains(t, result.FailedNodes["consumer-a"], reason.Phrase(reason.RemoteServerUnfit))
		assert.Contains(t, result.FailedNodes["consumer-a"], reason.Phrase(reason.NodeBadRemoteEndpoint)+" (bad-endpoints)")
	})
	t.Run("unreachable server", func(t *testing.T) {
		unreachable, _ := remoteServerNode(t, "unreachable")
		unreachable.Annotations[util.NodeRemoteEndpointsAnnotation] = remotegpu.UnreachableServerEndpointInfo
		fixture := newRemoteFixture(t, []corev1.Node{unreachable})
		result := fixture.run(remotePod("unreachable", 1, 50, 2048), dryRunFilter)

		assert.Empty(t, NodeNamesOfResult(result))
		assert.Contains(t, result.FailedNodes["consumer-a"], reason.Phrase(reason.NodeRemoteServerUnreachable)+" (unreachable)")
	})
	// A server label left on a node without endpoints does not make it a server.
	t.Run("server label without endpoints", func(t *testing.T) {
		fixture := newRemoteFixture(t, []corev1.Node{noEndpoints})
		pod := fixture.createPod(t, remotePod("no-endpoint", 1, 50, 2048))
		result := fixture.run(pod, liveFilter)

		assert.Empty(t, NodeNamesOfResult(result))
		assert.Empty(t, fixture.getPod(t, pod).Annotations[util.PodPredicateNodeAnnotation])
	})
}

// A local pod never gets devices on a remote GPU server.
func Test_Filter_LocalPodSkipsRemoteServer(t *testing.T) {
	server, _ := remoteServerNode(t, "gpu-server")
	local, _ := buildNodeList()
	fixture := newRemoteFixture(t, []corev1.Node{server})
	pod := fixture.createPod(t, dryRunPod("local-pod", 1, 50, 2048))

	result := fixture.run(pod, liveFilter, server, local[1])

	assert.Empty(t, result.Error)
	assert.Equal(t, []string{local[1].Name}, NodeNamesOfResult(result))
	assert.Equal(t, reason.Phrase(reason.NodeIsRemoteServer), result.FailedNodes["gpu-server"])
}

// Which taints take a GPU server out of the rotation: the ones the cluster
// itself sets (a cordon, a node that went not-ready), and only as hard
// effects. An operator taint that keeps non-GPU workloads off a dedicated GPU
// node says nothing about serving remote pods -- they never run there.
func Test_RemoteFilter_ServerTaints(t *testing.T) {
	for _, test := range []struct {
		name        string
		taint       corev1.Taint
		tolerations []corev1.Toleration
		served      bool
	}{
		{
			name:   "operator isolation taint",
			taint:  corev1.Taint{Key: "nvidia.com/gpu", Value: "true", Effect: corev1.TaintEffectNoSchedule},
			served: true,
		},
		{
			name:   "soft cluster taint",
			taint:  corev1.Taint{Key: corev1.TaintNodeMemoryPressure, Effect: corev1.TaintEffectPreferNoSchedule},
			served: true,
		},
		{
			name:   "cordoned",
			taint:  corev1.Taint{Key: corev1.TaintNodeUnschedulable, Effect: corev1.TaintEffectNoSchedule},
			served: false,
		},
		{
			name:   "not ready",
			taint:  corev1.Taint{Key: corev1.TaintNodeNotReady, Effect: corev1.TaintEffectNoExecute},
			served: false,
		},
		{
			name:  "cordoned but tolerated",
			taint: corev1.Taint{Key: corev1.TaintNodeUnschedulable, Effect: corev1.TaintEffectNoSchedule},
			tolerations: []corev1.Toleration{{
				Key: corev1.TaintNodeUnschedulable, Operator: corev1.TolerationOpExists,
			}},
			served: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, _ := remoteServerNode(t, "gpu-server")
			server.Spec.Taints = []corev1.Taint{test.taint}
			fixture := newRemoteFixture(t, []corev1.Node{server})
			pod := remotePod("tainted-server", 1, 50, 2048)
			pod.Spec.Tolerations = test.tolerations

			result := fixture.run(pod, dryRunFilter, fixture.consumers[0])

			if test.served {
				assert.Equal(t, []string{"consumer-a"}, NodeNamesOfResult(result))
				return
			}
			assert.Empty(t, NodeNamesOfResult(result))
			assert.Contains(t, result.FailedNodes["consumer-a"], reason.Phrase(reason.NoRemoteServer))
		})
	}
}
