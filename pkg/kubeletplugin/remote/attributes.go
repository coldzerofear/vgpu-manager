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

package remote

import (
	"fmt"

	"github.com/Masterminds/semver"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/utils/ptr"
)

// DeviceInfo is the parsed remote view of one published device. It is the
// contract between the server-side publisher (Decorate) and the inject-mode
// consumer (ParseDevice).
type DeviceInfo struct {
	// UUID of the physical GPU on the server node (session config files are
	// keyed by it).
	UUID string
	// Endpoint of the lupine-server, verbatim as published (URL form, e.g.
	// http://10.0.0.1:14833; a path prefix stays intact for future gateway
	// routing). The injection layer only concatenates it into LUPINE_SERVER.
	Endpoint string
	// AgentEndpoint of the remote-agent on the same host, verbatim as
	// published (URL form). This is where inject calls EnsureSession (gRPC).
	AgentEndpoint string
	// CUDAVersion is the ceiling a client artifact must not exceed: the lower
	// of the node driver ceiling and ServerCUDAVersion (when known). Client
	// artifact selection picks max{ver : ver <= CUDAVersion} (design §4.3).
	CUDAVersion *semver.Version
	// ServerCUDAVersion is what the lupine-server binary was built with, nil
	// until the publisher has heard from the server.
	ServerCUDAVersion *semver.Version
}

// PublishSpec is what the server-side publisher stamps onto every device of
// the node when the RemoteGPUSupport gate is on.
type PublishSpec struct {
	Endpoint      string
	AgentEndpoint string
	Selector      []corev1.NodeSelectorRequirement
	// ServerCUDAVersion is stamped as serverCudaVersion once known (nil =
	// lupine-server has not answered yet, attribute left out).
	ServerCUDAVersion *semver.Version
}

// Reachable reports whether the spec carries what a consumer node needs to
// use the devices: the agent's address (sessions) and the server's
// (clients). Both come from the remote-agent; until it has answered, the
// devices are published but tainted (see Decorate).
func (s *PublishSpec) Reachable() bool {
	return s != nil && s.Endpoint != "" && s.AgentEndpoint != ""
}

// Decorate stamps accessMode (and, for remote, the server/agent endpoints
// plus the server CUDA version once known) onto the devices in place.
// spec == nil means the node is local-only.
//
// A remote spec whose endpoints are not known yet (the agent has not
// answered, or has no routable address) publishes the devices without the
// endpoint attributes and with a NoSchedule taint, so the scheduler keeps
// them off new claims until the next republish carries the endpoints. The
// taint needs the DRADeviceTaints gate to be honoured; without it the
// inject side still refuses a claim whose devices have no endpoint the
// agent can supply (EnsureSession), so nothing is silently misrouted.
func Decorate(devices []resourceapi.Device, spec *PublishSpec) {
	for i := range devices {
		attrs := devices[i].Attributes
		if attrs == nil {
			attrs = map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{}
			devices[i].Attributes = attrs
		}
		if spec == nil {
			attrs[AttrAccessMode] = resourceapi.DeviceAttribute{StringValue: ptr.To(AccessModeLocal)}
			continue
		}
		attrs[AttrAccessMode] = resourceapi.DeviceAttribute{StringValue: ptr.To(AccessModeRemote)}
		// The devices are rebuilt from the allocatable state on every
		// publish (GetDevice / PartGetDevice clone the health taints), so
		// nothing below persists across publishes. It is still written to
		// be idempotent on one Device value: the taint is set or cleared,
		// never accumulated, and the endpoint attributes are removed when
		// the spec loses them.
		if !spec.Reachable() {
			delete(attrs, AttrServerEndpoint)
			delete(attrs, AttrAgentEndpoint)
			delete(attrs, AttrServerCUDAVersion)
			devices[i].Taints = withTaint(devices[i].Taints, resourceapi.DeviceTaint{
				Key:    TaintKeyRemoteUnavailable,
				Value:  TaintValueRemoteUnavailable,
				Effect: resourceapi.DeviceTaintEffectNoSchedule,
			})
			continue
		}
		devices[i].Taints = withoutTaint(devices[i].Taints, TaintKeyRemoteUnavailable)
		attrs[AttrServerEndpoint] = resourceapi.DeviceAttribute{StringValue: ptr.To(spec.Endpoint)}
		attrs[AttrAgentEndpoint] = resourceapi.DeviceAttribute{StringValue: ptr.To(spec.AgentEndpoint)}
		if spec.ServerCUDAVersion != nil {
			attrs[AttrServerCUDAVersion] = resourceapi.DeviceAttribute{VersionValue: ptr.To(spec.ServerCUDAVersion.String())}
		} else {
			delete(attrs, AttrServerCUDAVersion)
		}
	}
}

// withTaint returns taints with t set: replacing the entry of the same key
// in place, or appended. The input slice is never grown in place, so a
// caller sharing its backing array with someone else is not surprised.
func withTaint(taints []resourceapi.DeviceTaint, t resourceapi.DeviceTaint) []resourceapi.DeviceTaint {
	out := make([]resourceapi.DeviceTaint, 0, len(taints)+1)
	replaced := false
	for _, existing := range taints {
		if existing.Key == t.Key {
			out = append(out, t)
			replaced = true
			continue
		}
		out = append(out, existing)
	}
	if !replaced {
		out = append(out, t)
	}
	return out
}

// withoutTaint returns taints without the entry of the given key; nil when
// nothing is left, so an untainted device stays untainted (not empty).
func withoutTaint(taints []resourceapi.DeviceTaint, key string) []resourceapi.DeviceTaint {
	var out []resourceapi.DeviceTaint
	for _, existing := range taints {
		if existing.Key != key {
			out = append(out, existing)
		}
	}
	return out
}

// ParseNodeSelector parses a standard label-selector expression
// ("zone=a,rack in (r1,r2),!isolated") into NodeSelectorRequirements. All
// requirements are ANDed inside one term; this is the operator-supplied
// reachability predicate for --remote-node-selector.
func ParseNodeSelector(expr string) ([]corev1.NodeSelectorRequirement, error) {
	sel, err := labels.Parse(expr)
	if err != nil {
		return nil, fmt.Errorf("invalid node selector %q: %w", expr, err)
	}
	reqs, _ := sel.Requirements()
	if len(reqs) == 0 {
		return nil, fmt.Errorf("node selector %q selects every node; refusing (use an explicit predicate)", expr)
	}
	var out []corev1.NodeSelectorRequirement
	for _, r := range reqs {
		nr := corev1.NodeSelectorRequirement{Key: r.Key(), Values: r.Values().List()}
		switch r.Operator() {
		case selection.Equals, selection.DoubleEquals, selection.In:
			nr.Operator = corev1.NodeSelectorOpIn
		case selection.NotEquals, selection.NotIn:
			nr.Operator = corev1.NodeSelectorOpNotIn
		case selection.Exists:
			nr.Operator = corev1.NodeSelectorOpExists
		case selection.DoesNotExist:
			nr.Operator = corev1.NodeSelectorOpDoesNotExist
		default:
			return nil, fmt.Errorf("node selector %q: operator %q is not supported for node selection", expr, r.Operator())
		}
		out = append(out, nr)
	}
	return out, nil
}

// PoolNodeSelector is the node scope of a remote-capable pool: exactly the
// nodes matching the operator's --remote-node-selector (one term, all
// requirements ANDed). The GPU node itself is NOT implicitly included; add it
// to the selector if pods should be schedulable there (they still consume
// the GPU through lupine, design v2.1 D23).
func PoolNodeSelector(reachable []corev1.NodeSelectorRequirement) *corev1.NodeSelector {
	return &corev1.NodeSelector{
		NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: reachable}},
	}
}

// ParseDevice inspects a published device. It returns (nil, false, nil) when
// the device is not remote-capable (accessMode absent or local), and an error
// when it claims to be remote but is malformed.
func ParseDevice(dev *resourceapi.Device) (*DeviceInfo, bool, error) {
	if StringAttr(dev, AttrAccessMode) != AccessModeRemote {
		return nil, false, nil
	}

	info := &DeviceInfo{
		UUID: StringAttr(dev, AttrUUID),
		// The server endpoint may be absent (published before the agent
		// answered, or a scheduler that ignores the taint): EnsureSession
		// supplies it at prepare time. The agent endpoint cannot be: it is
		// the only way to reach that node at all.
		Endpoint:      StringAttr(dev, AttrServerEndpoint),
		AgentEndpoint: StringAttr(dev, AttrAgentEndpoint),
	}
	if info.AgentEndpoint == "" {
		return nil, true, fmt.Errorf("remote device %q has no %q attribute (its remote-agent has not reported a routable address yet)", dev.Name, AttrAgentEndpoint)
	}
	if info.UUID == "" {
		return nil, true, fmt.Errorf("remote device %q has no %q attribute", dev.Name, AttrUUID)
	}

	if versionVal := VersionAttr(dev, AttrCUDADriverVersion); versionVal != "" {
		v, err := semver.NewVersion(versionVal)
		if err != nil {
			return nil, true, fmt.Errorf("remote device %q has unparseable %s %q: %w",
				dev.Name, AttrCUDADriverVersion, versionVal, err)
		}
		info.CUDAVersion = v
	}

	if info.CUDAVersion == nil {
		return nil, true, fmt.Errorf("remote device %q has no %s attribute", dev.Name, AttrCUDADriverVersion)
	}

	// The server build version is optional (absent until the server answered).
	// When present, the lower of the two is what the client artifact must obey.
	if serverVal := VersionAttr(dev, AttrServerCUDAVersion); serverVal != "" {
		v, err := semver.NewVersion(serverVal)
		if err != nil {
			return nil, true, fmt.Errorf("remote device %q has unparseable %s %q: %w",
				dev.Name, AttrServerCUDAVersion, serverVal, err)
		}
		info.ServerCUDAVersion = v
		info.CUDAVersion = EffectiveCUDACeiling(info.CUDAVersion, v)
	}

	return info, true, nil
}

func StringAttr(dev *resourceapi.Device, name resourceapi.QualifiedName) string {
	if attr, ok := dev.Attributes[name]; ok && attr.StringValue != nil {
		return *attr.StringValue
	}
	return ""
}

func IntAttr(dev *resourceapi.Device, name resourceapi.QualifiedName) int64 {
	if attr, ok := dev.Attributes[name]; ok && attr.IntValue != nil {
		return *attr.IntValue
	}
	return -1
}

func VersionAttr(dev *resourceapi.Device, name resourceapi.QualifiedName) string {
	if attr, ok := dev.Attributes[name]; ok && attr.VersionValue != nil {
		return *attr.VersionValue
	}
	return ""
}
