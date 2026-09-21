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

// Package hostname gives a pod a hostname derived from the node it will run
// on, which is what lets one DaemonSet pod per node have a DNS name of its
// own: with spec.subdomain naming a headless service, the record
// <hostname>.<subdomain>.<namespace>.svc.<cluster domain> resolves to that
// pod, and keeps resolving to it after the pod is recreated with another IP.
//
// The remote GPU server pod needs exactly that. Its address is handed to
// consumer containers once, as LUPINE_SERVER at Allocate time, and a container
// restart does not get a new one -- so an address that survives the server
// pod's recreation is the difference between a remote pod that recovers by
// itself and one that has to be deleted. Without hostNetwork (where the node
// IP does the same job) a name is the only thing that survives.
//
// A DaemonSet cannot do this on its own: its pod template is one template for
// every node, and spec.hostname takes no field references.
package hostname

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/coldzerofear/vgpu-manager/cmd/device-webhook/options"
	"github.com/coldzerofear/vgpu-manager/pkg/util"
	"github.com/coldzerofear/vgpu-manager/pkg/webhook/resourcereader"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const (
	Path = "/pods/hostname"

	// HostnameEnv carries the hostname into the containers, which is what
	// makes it usable in the pod spec: $(HOSTNAME) is expanded from the
	// container's own environment, not from spec.hostname. It is also what the
	// remote-agent's ADVERTISE_SERVER_ENDPOINT is built from.
	HostnameEnv = "HOSTNAME"

	// hostnameHashLength is how much of the node name digest is appended to a
	// name that had to be rewritten; 8 hex characters make a collision between
	// two nodes of one cluster a non-issue while keeping the name readable.
	hostnameHashLength = 8
)

func NewMutateWebhook(
	client client.Client, options *options.Options,
	_ resourcereader.ResourceAPIReader,
	_ events.EventRecorderLogger,
) (http.Handler, error) {
	return &admission.Webhook{
		Handler: &mutateHandle{
			decoder: admission.NewDecoder(client.Scheme()),
			options: options,
		},
		RecoverPanic: ptr.To[bool](true),
	}, nil
}

type mutateHandle struct {
	decoder admission.Decoder
	options *options.Options
}

// MutateCreate gives an opted-in pod the hostname of its node.
func (h *mutateHandle) MutateCreate(ctx context.Context, pod *corev1.Pod) {
	logger := log.FromContext(ctx)
	if value, ok := pod.Labels[util.NodeHostnameLabel]; !ok || value != "true" {
		return
	}
	nodeName, field := targetNode(pod)
	if nodeName == "" {
		// Nothing says where this pod will run, so there is no name to give
		// it. Admission still passes: the pod is left as it was written, and
		// what depends on the hostname fails loudly instead (an unexpanded
		// $(HOSTNAME) does not parse as an endpoint).
		logger.Info("Pod opted into a node hostname but names no node to take it from",
			"pod", pod.Name, "namespace", pod.Namespace)
		return
	}
	if pod.Spec.Hostname == "" {
		pod.Spec.Hostname = NodeHostname(nodeName)
	}
	setHostnameEnv(pod, pod.Spec.Hostname)
	logger.V(4).Info("Set pod hostname from its node",
		"pod", pod.Name, "namespace", pod.Namespace, "node", nodeName,
		"nodeFrom", field, "hostname", pod.Spec.Hostname)
}

// NodeHostname is the node's name as a hostname: a DNS label (RFC 1123), which
// a node name is not required to be -- it is a DNS subdomain, so it may carry
// dots and run up to 253 characters, while a hostname may not and must fit in
// 63.
//
// A name that has to be rewritten gets a digest of the original appended,
// because the rewriting is not injective: without it "a.b" and "a-b" would
// both become "a-b", and two nodes publishing one name under the same headless
// service means a client can be sent to the wrong GPU server.
func NodeHostname(nodeName string) string {
	label := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '-'
		}
	}, nodeName)
	label = strings.Trim(label, "-")

	if label == nodeName && len(label) <= validation.DNS1123LabelMaxLength {
		return label
	}
	sum := sha256.Sum256([]byte(nodeName))
	suffix := "-" + hex.EncodeToString(sum[:])[:hostnameHashLength]
	if len(label)+len(suffix) > validation.DNS1123LabelMaxLength {
		label = label[:validation.DNS1123LabelMaxLength-len(suffix)]
		label = strings.TrimRight(label, "-")
	}
	return label + suffix
}

// targetNode is the node this pod is going to run on, and where that was
// read from. A DaemonSet pod carries it as a node affinity on metadata.name
// (which is what the DaemonSet controller writes), a pinned pod as
// spec.nodeName, and a hand-written one may use the hostname label; the
// mutating webhook of this project rewrites spec.nodeName into a selector for
// vGPU pods, so both of the latter two occur.
func targetNode(pod *corev1.Pod) (string, string) {
	if pod.Spec.NodeName != "" {
		return pod.Spec.NodeName, "spec.nodeName"
	}
	if node, ok := pod.Spec.NodeSelector[corev1.LabelHostname]; ok && node != "" {
		return node, "spec.nodeSelector"
	}
	affinity := pod.Spec.Affinity
	if affinity == nil || affinity.NodeAffinity == nil ||
		affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return "", ""
	}
	// Only a single candidate names a node: a term matching several nodes (or
	// several terms, which are ORed) says the pod may run on any of them, and
	// a hostname must belong to one.
	var found string
	for _, term := range affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		for _, expr := range term.MatchFields {
			if expr.Key != "metadata.name" || expr.Operator != corev1.NodeSelectorOpIn {
				continue
			}
			if len(expr.Values) != 1 || (found != "" && found != expr.Values[0]) {
				return "", ""
			}
			found = expr.Values[0]
		}
	}
	if found == "" {
		return "", ""
	}
	return found, "spec.affinity.nodeAffinity"
}

// setHostnameEnv makes the hostname readable from the pod spec. A container
// that sets HOSTNAME itself keeps its own value.
func setHostnameEnv(pod *corev1.Pod, hostname string) {
	containers := make([]*corev1.Container, 0, len(pod.Spec.InitContainers)+len(pod.Spec.Containers))
	for i := range pod.Spec.InitContainers {
		containers = append(containers, &pod.Spec.InitContainers[i])
	}
	for i := range pod.Spec.Containers {
		containers = append(containers, &pod.Spec.Containers[i])
	}
	for _, container := range containers {
		if slicesContainsEnv(container.Env, HostnameEnv) {
			continue
		}
		container.Env = append([]corev1.EnvVar{{Name: HostnameEnv, Value: hostname}}, container.Env...)
	}
}

func slicesContainsEnv(envs []corev1.EnvVar, name string) bool {
	for _, env := range envs {
		if env.Name == name {
			return true
		}
	}
	return false
}

func (h *mutateHandle) Handle(ctx context.Context, req admission.Request) admission.Response {
	logger := log.FromContext(ctx).WithValues("operation", req.Operation)
	logger.V(4).Info("into pod hostname mutate handle")

	if req.Operation != admissionv1.Create {
		return admission.ValidationResponse(true, "")
	}
	pod := &corev1.Pod{}
	if err := h.decoder.Decode(req, pod); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	h.MutateCreate(log.IntoContext(ctx, logger), pod)

	marshalled, err := json.Marshal(pod)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	return admission.PatchResponseFromRaw(req.Object.Raw, marshalled)
}

// FQDN is the name the pod answers to once it has a hostname and a subdomain
// naming a headless service. Only used in messages and docs; the pod spec
// builds it from $(HOSTNAME).
func FQDN(hostname, subdomain, namespace, clusterDomain string) string {
	return fmt.Sprintf("%s.%s.%s.svc.%s", hostname, subdomain, namespace, clusterDomain)
}
