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

package hostname

import (
	"context"
	"strings"
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// A node name is a DNS subdomain; a hostname is a DNS label. Everything a node
// name may carry and a hostname may not has to go, and the result must stay
// unique per node.
func TestNodeHostname(t *testing.T) {
	for _, test := range []struct {
		name     string
		nodeName string
		// want is the whole hostname when the node name needs no rewriting,
		// and the part before the digest when it does.
		want     string
		rewrites bool
	}{
		{name: "already a label", nodeName: "gpu-node-1", want: "gpu-node-1"},
		{name: "dotted", nodeName: "gpu-a.corp.example.com", want: "gpu-a-corp-example-com", rewrites: true},
		{name: "upper case", nodeName: "GPU-Node", want: "gpu-node", rewrites: true},
		{name: "underscores", nodeName: "gpu_node_1", want: "gpu-node-1", rewrites: true},
		{name: "trailing dot", nodeName: "gpu-node-1.", want: "gpu-node-1", rewrites: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := NodeHostname(test.nodeName)
			assert.Empty(t, validation.IsDNS1123Label(got), "%q must be a DNS label", got)
			assert.Equal(t, got, NodeHostname(test.nodeName), "the same node must always get the same hostname")
			if !test.rewrites {
				assert.Equal(t, test.want, got)
				return
			}
			// A rewritten name carries a digest of the original, so that two
			// node names cannot end up as one hostname.
			assert.Equal(t, test.want+"-", got[:len(test.want)+1], "got %q", got)
			assert.Len(t, got, len(test.want)+1+hostnameHashLength)
		})
	}
}

// Two node names that sanitize to the same label must not get the same
// hostname: they would publish one DNS name between them, and a client could
// be sent to the wrong GPU server.
func TestNodeHostnameCollision(t *testing.T) {
	assert.NotEqual(t, NodeHostname("a.b"), NodeHostname("a-b"))
	long := strings.Repeat("gpu-node-", 12) // > 63 characters
	other := long + "2"
	for _, name := range []string{long, other} {
		got := NodeHostname(name)
		assert.Empty(t, validation.IsDNS1123Label(got), "%q must be a DNS label", got)
		assert.LessOrEqual(t, len(got), validation.DNS1123LabelMaxLength)
	}
	assert.NotEqual(t, NodeHostname(long), NodeHostname(other), "a truncated name still has to be unique")
}

func optedInPod(node string) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "vgpu-manager-remote-gpu-server-abcde",
			Namespace: "kube-system",
			Labels:    map[string]string{util.NodeHostnameLabel: "true"},
		},
		Spec: corev1.PodSpec{
			Subdomain:      "vgpu-manager-remote-gpu-server-headless",
			InitContainers: []corev1.Container{{Name: "init-install"}},
			Containers:     []corev1.Container{{Name: "remote-agent"}, {Name: "lupine-server"}},
		},
	}
	// What the DaemonSet controller writes: the node as a field selector.
	pod.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchFields: []corev1.NodeSelectorRequirement{{
					Key:      "metadata.name",
					Operator: corev1.NodeSelectorOpIn,
					Values:   []string{node},
				}},
			}},
		},
	}}
	return pod
}

func TestMutateCreate(t *testing.T) {
	h := &mutateHandle{}
	ctx := context.Background()

	pod := optedInPod("gpu-node-1")
	h.MutateCreate(ctx, pod)
	assert.Equal(t, "gpu-node-1", pod.Spec.Hostname)
	// The env is what makes it usable from the pod spec, in every container.
	for _, container := range append(pod.Spec.InitContainers, pod.Spec.Containers...) {
		require.Len(t, container.Env, 1, "container %s", container.Name)
		assert.Equal(t, corev1.EnvVar{Name: HostnameEnv, Value: "gpu-node-1"}, container.Env[0])
	}

	// A pod that did not ask for it is left alone.
	pod = optedInPod("gpu-node-1")
	delete(pod.Labels, util.NodeHostnameLabel)
	h.MutateCreate(ctx, pod)
	assert.Empty(t, pod.Spec.Hostname)
	assert.Empty(t, pod.Spec.Containers[0].Env)

	// A hostname of its own is respected, and still exported.
	pod = optedInPod("gpu-node-1")
	pod.Spec.Hostname = "written-by-hand"
	h.MutateCreate(ctx, pod)
	assert.Equal(t, "written-by-hand", pod.Spec.Hostname)
	assert.Equal(t, "written-by-hand", pod.Spec.Containers[0].Env[0].Value)

	// So is a HOSTNAME the author set.
	pod = optedInPod("gpu-node-1")
	pod.Spec.Containers[0].Env = []corev1.EnvVar{{Name: HostnameEnv, Value: "mine"}}
	h.MutateCreate(ctx, pod)
	assert.Equal(t, "mine", pod.Spec.Containers[0].Env[0].Value)
	assert.Len(t, pod.Spec.Containers[0].Env, 1)
}

// Where the node name is read from, and when there is none to read.
func TestTargetNode(t *testing.T) {
	pod := optedInPod("gpu-node-1")
	node, from := targetNode(pod)
	assert.Equal(t, "gpu-node-1", node)
	assert.Equal(t, "spec.affinity.nodeAffinity", from)

	// A bound or pinned pod says it outright.
	pod = optedInPod("gpu-node-1")
	pod.Spec.NodeName = "gpu-node-2"
	node, from = targetNode(pod)
	assert.Equal(t, "gpu-node-2", node)
	assert.Equal(t, "spec.nodeName", from)

	// The mutating webhook of this project rewrites nodeName into a selector.
	pod = optedInPod("gpu-node-1")
	pod.Spec.Affinity = nil
	pod.Spec.NodeSelector = map[string]string{corev1.LabelHostname: "gpu-node-3"}
	node, from = targetNode(pod)
	assert.Equal(t, "gpu-node-3", node)
	assert.Equal(t, "spec.nodeSelector", from)

	// Several candidates name no node: a hostname belongs to one of them.
	pod = optedInPod("gpu-node-1")
	pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.
		NodeSelectorTerms[0].MatchFields[0].Values = []string{"gpu-node-1", "gpu-node-2"}
	node, _ = targetNode(pod)
	assert.Empty(t, node)

	// Nothing to go on: the pod passes through untouched rather than being
	// refused; what needs the hostname fails loudly on its own.
	pod = optedInPod("gpu-node-1")
	pod.Spec.Affinity = nil
	node, _ = targetNode(pod)
	assert.Empty(t, node)
	h := &mutateHandle{}
	h.MutateCreate(context.Background(), pod)
	assert.Empty(t, pod.Spec.Hostname)
}
