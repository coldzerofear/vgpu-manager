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

	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	extenderv1 "k8s.io/kube-scheduler/extender/v1"
)

// A remote pod gets kube-scheduler's victims back unchanged.
func Test_Preempt_RemotePodPassthrough(t *testing.T) {
	plugin, cleanup := newPreemptPluginWithSync(t, nil, nil)
	defer cleanup()
	preemptor := newVGPUPod("remote-preemptor", "ns", 1, withPriority(100), func(pod *corev1.Pod) {
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		pod.Annotations[util.VGPUAccessModeAnnotation] = util.AccessModeRemote
	})
	args := extenderv1.ExtenderPreemptionArgs{
		Pod: preemptor,
		NodeNameToMetaVictims: map[string]*extenderv1.MetaVictims{
			"n1": {Pods: []*extenderv1.MetaPod{{UID: "u1"}}, NumPDBViolations: 0},
		},
	}

	res := plugin.Preempt(context.Background(), args)

	assert.Len(t, res.NodeNameToMetaVictims, 1)
	assert.Equal(t, []string{"u1"}, metaUIDs(res.NodeNameToMetaVictims["n1"]))
}

// A local pod never preempts pods on a remote GPU server.
func Test_Preempt_SkipsRemoteServer(t *testing.T) {
	node, devUUIDs := newTestNode("server1")
	node.Labels = map[string]string{util.NodeRemoteServerLabel: "true"}
	node.Annotations[util.NodeRemoteEndpointsAnnotation] =
		`{"serverEndpoint":"http://10.0.0.1:8080","agentEndpoint":"grpc://10.0.0.1:9090"}`
	lowA := newVGPUPod("low-a", "ns", 1, withPriority(10), withNodeName(node.Name))
	allocatePodOn(lowA, node.Name, 0, devUUIDs[0])
	lowB := newVGPUPod("low-b", "ns", 1, withPriority(10), withNodeName(node.Name))
	allocatePodOn(lowB, node.Name, 1, devUUIDs[1])
	preemptor := newVGPUPod("preemptor", "ns", 1, withPriority(100))

	plugin, cleanup := newPreemptPluginWithSync(t, []*corev1.Pod{lowA, lowB, preemptor}, []*corev1.Node{node})
	defer cleanup()

	res := plugin.Preempt(context.Background(), extenderv1.ExtenderPreemptionArgs{
		Pod: preemptor,
		NodeNameToMetaVictims: map[string]*extenderv1.MetaVictims{
			node.Name: {Pods: []*extenderv1.MetaPod{{UID: string(lowB.UID)}}},
		},
	})

	assert.NotContains(t, res.NodeNameToMetaVictims, node.Name)
}
