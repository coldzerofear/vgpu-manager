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
	// Endpoint of the lupine-server, verbatim as published. The injection
	// layer never interprets it beyond concatenation into LUPINE_SERVER.
	Endpoint string
	// CUDAVersion is the server node's CUDA driver ceiling; client artifact
	// selection picks max{ver : ver <= CUDAVersion} (design §4.3).
	CUDAVersion *semver.Version
}

// PublishSpec is what the server-side publisher stamps onto every device of
// the node when the RemoteGPUSupport gate is on.
type PublishSpec struct {
	Endpoint string
	Selector []corev1.NodeSelectorRequirement
}

// Decorate stamps accessMode (and, for remote, endpoint) onto the
// devices in place. spec == nil means the node is local-only.
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
		attrs[AttrEndpoint] = resourceapi.DeviceAttribute{StringValue: ptr.To(spec.Endpoint)}
	}
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
		UUID:     StringAttr(dev, AttrUUID),
		Endpoint: StringAttr(dev, AttrEndpoint),
	}
	if info.Endpoint == "" {
		return nil, true, fmt.Errorf("remote device %q has no %q attribute", dev.Name, AttrEndpoint)
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
