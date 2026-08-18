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
	"fmt"
	"strings"
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/scheduler/metrics"
	"github.com/coldzerofear/vgpu-manager/pkg/scheduler/reason"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	testing2 "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
	extenderv1 "k8s.io/kube-scheduler/extender/v1"
)

// dryRunFixture wires a gpuFilter against a fake cluster with a recorder whose
// events can be counted, which is how the read-only assertions are made.
type dryRunFixture struct {
	filter   *gpuFilter
	client   *fake.Clientset
	recorder *record.FakeRecorder
	nodes    []corev1.Node
}

func newDryRunFixture(t *testing.T) *dryRunFixture {
	t.Helper()
	k8sClient := fake.NewClientset()
	factory := informers.NewSharedInformerFactory(k8sClient, 0)
	recorder := record.NewFakeRecorder(64)
	filterPredicate, err := New(k8sClient, factory, recorder, false, true)
	if err != nil {
		t.Fatalf("failed to create new filterPredicate due to %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())

	nodes, _ := buildNodeList()
	return &dryRunFixture{filter: filterPredicate, client: k8sClient, recorder: recorder, nodes: nodes}
}

// writeActions counts every call that would mutate the cluster.
func (f *dryRunFixture) writeActions() []testing2.Action {
	var writes []testing2.Action
	for _, action := range f.client.Actions() {
		switch action.GetVerb() {
		case "get", "list", "watch":
		default:
			writes = append(writes, action)
		}
	}
	return writes
}

func dryRunPod(name string, number, cores, memory int) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			UID:       k8stypes.UID(uuid.NewString()),
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "cont1",
				Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{
						corev1.ResourceName(util.VGPUNumberResourceName): resource.MustParse(fmt.Sprintf("%d", number)),
						corev1.ResourceName(util.VGPUCoreResourceName):   resource.MustParse(fmt.Sprintf("%d", cores)),
						corev1.ResourceName(util.VGPUMemoryResourceName): resource.MustParse(fmt.Sprintf("%d", memory)),
					},
				},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
}

// Dry-run must answer the feasibility question without touching the cluster:
// no pod patch, no cached-pod mutation, no event.
func Test_FilterDryRun_HasNoSideEffects(t *testing.T) {
	fixture := newDryRunFixture(t)
	pod := dryRunPod("dryrun-readonly", 1, 50, 2048)

	result := fixture.filter.FilterDryRun(context.Background(), extenderv1.ExtenderArgs{
		Pod:   pod,
		Nodes: &corev1.NodeList{Items: fixture.nodes},
	})

	assert.Empty(t, result.Error)
	assert.NotEmpty(t, NodeNamesOfResult(result), "expected at least one feasible node")
	assert.Empty(t, fixture.writeActions(), "dry-run must not write to the cluster")
	assert.Len(t, fixture.recorder.Events, 0, "dry-run must not emit events")
	assert.Empty(t, pod.Annotations, "dry-run must not stamp pre-allocation annotations")

	cachedPods, err := fixture.filter.podLister.NodeMapByIndexValue(IndexerKeyPodRequestVGPU, "true")
	assert.NoError(t, err)
	assert.Empty(t, cachedPods, "dry-run must not seed the pod cache")
}

// The live filter commits one node; dry-run reports the whole feasible set,
// because the caller — not this extender — decides where the pod would go.
func Test_FilterDryRun_ReturnsEveryFeasibleNode(t *testing.T) {
	fixture := newDryRunFixture(t)
	args := extenderv1.ExtenderArgs{
		Pod:   dryRunPod("dryrun-all-nodes", 1, 50, 2048),
		Nodes: &corev1.NodeList{Items: fixture.nodes},
	}

	dryRun := fixture.filter.FilterDryRun(context.Background(), args)
	assert.Empty(t, dryRun.Error)
	assert.Len(t, NodeNamesOfResult(dryRun), len(fixture.nodes))
	assert.Empty(t, dryRun.FailedNodes, "feasible nodes must not be reported as failed")

	// The live path patches the pod it commits, so it must really exist.
	livePod, err := fixture.client.CoreV1().Pods(namespace).Create(
		context.Background(), dryRunPod("live-one-node", 1, 50, 2048), metav1.CreateOptions{})
	assert.NoError(t, err)
	live := fixture.filter.Filter(context.Background(), extenderv1.ExtenderArgs{
		Pod:   livePod,
		Nodes: &corev1.NodeList{Items: fixture.nodes},
	})
	assert.Empty(t, live.Error)
	assert.Len(t, NodeNamesOfResult(live), 1, "the live filter still commits exactly one node")
}

// Every node rejected by the capacity pre-gate must still come back with its
// reason: "nothing fits, and here is why" is the answer a scale-up decision
// depends on.
func Test_FilterDryRun_ReportsReasonWhenNothingFits(t *testing.T) {
	fixture := newDryRunFixture(t)
	// Larger than the biggest device on any node in the fixture.
	pod := dryRunPod("dryrun-too-big", 1, 50, 65536)

	result := fixture.filter.FilterDryRun(context.Background(), extenderv1.ExtenderArgs{
		Pod:   pod,
		Nodes: &corev1.NodeList{Items: fixture.nodes},
	})

	assert.Empty(t, result.Error)
	assert.Empty(t, NodeNamesOfResult(result))
	assert.Len(t, result.FailedNodes, len(fixture.nodes), "every rejected node needs a reason")
	for _, node := range fixture.nodes {
		assert.Contains(t, result.FailedNodes, node.Name)
		assert.NotEmpty(t, result.FailedNodes[node.Name])
	}
	assert.Len(t, fixture.recorder.Events, 0)
}

// A node-group template exists only inside the request, never in the node
// cache, so dry-run must judge it from the Node object it was handed.
func Test_FilterDryRun_AcceptsTemplateNodeAbsentFromCache(t *testing.T) {
	fixture := newDryRunFixture(t)
	template := *fixture.nodes[0].DeepCopy()
	template.Name = "template-node-for-gpu-nodegroup"

	_, err := fixture.filter.nodeLister.Get(template.Name)
	assert.Error(t, err, "the template must not exist in the node cache")

	result := fixture.filter.FilterDryRun(context.Background(), extenderv1.ExtenderArgs{
		Pod:   dryRunPod("dryrun-template", 1, 50, 2048),
		Nodes: &corev1.NodeList{Items: []corev1.Node{template}},
	})

	assert.Empty(t, result.Error)
	assert.Equal(t, []string{template.Name}, NodeNamesOfResult(result))
}

// An invalid device request is still an error, but reporting it on the pod is
// a live-path side effect that dry-run must not have.
func Test_FilterDryRun_InvalidRequestEmitsNoEvent(t *testing.T) {
	fixture := newDryRunFixture(t)
	// Cores beyond one whole device: rejected by CheckDeviceRequest.
	pod := dryRunPod("dryrun-invalid", 1, int(util.HundredCore)+1, 2048)

	result := fixture.filter.FilterDryRun(context.Background(), extenderv1.ExtenderArgs{
		Pod:   pod,
		Nodes: &corev1.NodeList{Items: fixture.nodes},
	})

	assert.NotEmpty(t, result.Error)
	assert.Len(t, fixture.recorder.Events, 0, "dry-run must not report request errors on the pod")
	assert.Empty(t, fixture.writeActions())

	// The same request on the live path does warn the user.
	live := newDryRunFixture(t)
	liveResult := live.filter.Filter(context.Background(), extenderv1.ExtenderArgs{
		Pod:   dryRunPod("live-invalid", 1, int(util.HundredCore)+1, 2048),
		Nodes: &corev1.NodeList{Items: live.nodes},
	})
	assert.NotEmpty(t, liveResult.Error)
	assert.Equal(t, 1, len(live.recorder.Events))
	assert.True(t, strings.Contains(<-live.recorder.Events, reason.EventResourceInvalid))
}

// Simulation traffic is unbounded, so it must never land on the live filter's
// series — the verb label is what keeps real scheduling readable.
func Test_FilterDryRun_MetricsStaySeparateFromLive(t *testing.T) {
	fixture := newDryRunFixture(t)
	args := extenderv1.ExtenderArgs{
		Pod:   dryRunPod("dryrun-metrics", 1, 50, 2048),
		Nodes: &corev1.NodeList{Items: fixture.nodes},
	}
	verbCount := func(verb, result string) float64 {
		return metrics.CounterValue("verb_total", map[string]string{"verb": verb, "result": result})
	}

	beforeDryRun := verbCount(metrics.VerbFilterDryRun, metrics.ResultFit)
	beforeLive := verbCount(metrics.VerbFilter, metrics.ResultFit)
	assert.Empty(t, fixture.filter.FilterDryRun(context.Background(), args).Error)

	assert.Equal(t, beforeDryRun+1, verbCount(metrics.VerbFilterDryRun, metrics.ResultFit))
	assert.Equal(t, beforeLive, verbCount(metrics.VerbFilter, metrics.ResultFit),
		"a simulation must not move the live filter's counter")
}

// "Nothing fits" is the answer a scale-up decision hinges on, and the per-node
// reason codes behind it are reported under the simulation's own verb.
func Test_FilterDryRun_RecordsRejectReasonsUnderOwnVerb(t *testing.T) {
	fixture := newDryRunFixture(t)
	rejectCount := func(verb string) float64 {
		return metrics.CounterValue("node_reject_total", map[string]string{"verb": verb})
	}

	beforeDryRun := rejectCount(metrics.VerbFilterDryRun)
	beforeLive := rejectCount(metrics.VerbFilter)
	result := fixture.filter.FilterDryRun(context.Background(), extenderv1.ExtenderArgs{
		Pod:   dryRunPod("dryrun-reject-metrics", 1, 50, 65536),
		Nodes: &corev1.NodeList{Items: fixture.nodes},
	})

	assert.Empty(t, NodeNamesOfResult(result))
	assert.Equal(t, beforeDryRun+float64(len(fixture.nodes)), rejectCount(metrics.VerbFilterDryRun))
	assert.Equal(t, beforeLive, rejectCount(metrics.VerbFilter))
}
