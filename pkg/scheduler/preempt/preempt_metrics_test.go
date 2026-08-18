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

package preempt

import (
	"context"
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/scheduler/metrics"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	extenderv1 "k8s.io/kube-scheduler/extender/v1"
)

func preemptCount(result string) float64 {
	return metrics.CounterValue("verb_total", map[string]string{
		"verb": metrics.VerbPreempt, "result": result,
	})
}

func protectedCount(reason string) float64 {
	return metrics.CounterValue("preempt_protected_total", map[string]string{"reason": reason})
}

// The three preempt outcomes must stay distinguishable: "we found victims",
// "we vetoed every candidate", and "this was never our decision to make".
func Test_Preempt_RecordsOutcome(t *testing.T) {
	t.Run("passthrough for a non-vGPU pod", func(t *testing.T) {
		plugin, cleanup := newPreemptPluginWithSync(t, nil, nil)
		defer cleanup()
		before := preemptCount(metrics.ResultPreemptPassthrough)
		plugin.Preempt(context.Background(), extenderv1.ExtenderPreemptionArgs{
			Pod: newPlainPod("preemptor", "ns"),
			NodeNameToMetaVictims: map[string]*extenderv1.MetaVictims{
				"n1": {Pods: []*extenderv1.MetaPod{{UID: "u1"}}},
			},
		})
		assert.Equal(t, before+1, preemptCount(metrics.ResultPreemptPassthrough))
	})

	t.Run("victims found", func(t *testing.T) {
		node, devUUIDs := newTestNode("node1")
		lowA := newVGPUPod("low-a", "ns", 1, withPriority(10), withNodeName(node.Name))
		allocatePodOn(lowA, node.Name, 0, devUUIDs[0])
		lowB := newVGPUPod("low-b", "ns", 1, withPriority(10), withNodeName(node.Name))
		allocatePodOn(lowB, node.Name, 1, devUUIDs[1])
		preemptor := newVGPUPod("preemptor", "ns", 1, withPriority(100))

		plugin, cleanup := newPreemptPluginWithSync(t,
			[]*corev1.Pod{lowA, lowB, preemptor}, []*corev1.Node{node})
		defer cleanup()

		before := preemptCount(metrics.ResultPreemptVictims)
		res := plugin.Preempt(context.Background(), extenderv1.ExtenderPreemptionArgs{
			Pod: preemptor,
			NodeNameToMetaVictims: map[string]*extenderv1.MetaVictims{
				node.Name: {Pods: []*extenderv1.MetaPod{{UID: string(lowB.UID)}}},
			},
		})
		assert.Contains(t, res.NodeNameToMetaVictims, node.Name)
		assert.Equal(t, before+1, preemptCount(metrics.ResultPreemptVictims))
	})

	t.Run("every candidate vetoed", func(t *testing.T) {
		node, devUUIDs := newTestNode("node1")
		dsA := newVGPUPod("ds-a", "ns", 1, withPriority(10), withNodeName(node.Name), withOwner("DaemonSet"))
		allocatePodOn(dsA, node.Name, 0, devUUIDs[0])
		dsB := newVGPUPod("ds-b", "ns", 1, withPriority(10), withNodeName(node.Name), withOwner("DaemonSet"))
		allocatePodOn(dsB, node.Name, 1, devUUIDs[1])
		preemptor := newVGPUPod("preemptor", "ns", 1, withPriority(100))

		plugin, cleanup := newPreemptPluginWithSync(t,
			[]*corev1.Pod{dsA, dsB, preemptor}, []*corev1.Node{node})
		defer cleanup()

		beforeResult := preemptCount(metrics.ResultPreemptNoVictims)
		// The proposed victim is a DaemonSet pod we refuse to evict — the
		// counter is what explains the empty result.
		beforeProtected := protectedCount(metrics.ProtectedDaemonSet)
		res := plugin.Preempt(context.Background(), extenderv1.ExtenderPreemptionArgs{
			Pod: preemptor,
			NodeNameToMetaVictims: map[string]*extenderv1.MetaVictims{
				node.Name: {Pods: []*extenderv1.MetaPod{{UID: string(dsB.UID)}}},
			},
		})
		assert.Empty(t, res.NodeNameToMetaVictims)
		assert.Equal(t, beforeResult+1, preemptCount(metrics.ResultPreemptNoVictims))
		assert.Equal(t, beforeProtected+1, protectedCount(metrics.ProtectedDaemonSet))
	})
}
