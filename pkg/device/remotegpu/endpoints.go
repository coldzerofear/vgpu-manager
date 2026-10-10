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

// Package remotegpu holds the contract shared by the scheduler, the device
// plugins and the remote agent for remote vGPU on the device-plugin path.
package remotegpu

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/coldzerofear/vgpu-manager/pkg/util"
	endpointutil "github.com/coldzerofear/vgpu-manager/pkg/util/endpoint"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/kube-scheduler/framework"
)

// UnreachableServerEndpointInfo is the annotation value a server node publishes
// while its lupine-server cannot be reached: the node keeps its server role, so
// local pods stay off its GPUs, but no remote pod is placed on it.
const UnreachableServerEndpointInfo = "{}"

// ErrServerUnreachable is returned when a node publishes UnreachableServerEndpointInfo.
var ErrServerUnreachable = errors.New("remote GPU server is not reachable")

// ServerEndpointInfo is what a remote GPU server node publishes about itself in
// util.NodeRemoteEndpointsAnnotation.
type ServerEndpointInfo struct {
	// ServerEndpoint is the lupine-server address clients connect to.
	ServerEndpoint string `json:"serverEndpoint"`
	// AgentEndpoint is the remote-agent gRPC address that prepares sessions.
	AgentEndpoint string `json:"agentEndpoint"`
	// ServerCUDAVersion is the CUDA version lupine-server was built with.
	ServerCUDAVersion string `json:"serverCudaVersion,omitempty"`
	// BundleETag identifies the client bundle lupine-server embeds.
	BundleETag string `json:"bundleEtag,omitempty"`
}

// Clone lets the scheduler keep the endpoints in its cycle state.
func (e *ServerEndpointInfo) Clone() framework.StateData {
	if e == nil {
		return e
	}
	data := *e
	return &data
}

// Encode checks the addresses and returns the annotation value.
func (e ServerEndpointInfo) Encode() (string, error) {
	if err := e.normalize(); err != nil {
		return "", err
	}
	data, err := json.Marshal(e)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// DecodeServerEndpointInfo parses the annotation value and checks the addresses.
// The returned addresses are in canonical form.
func DecodeServerEndpointInfo(value string) (*ServerEndpointInfo, error) {
	var e ServerEndpointInfo
	if err := json.Unmarshal([]byte(value), &e); err != nil {
		return nil, fmt.Errorf("invalid remote endpoints %q: %w", value, err)
	}
	if e.ServerEndpoint == "" && e.AgentEndpoint == "" {
		return nil, ErrServerUnreachable
	}
	if err := e.normalize(); err != nil {
		return nil, err
	}
	return &e, nil
}

// GetServerEndpointInfo reads the endpoints published on a remote GPU server node.
func GetServerEndpointInfo(node *corev1.Node) (*ServerEndpointInfo, error) {
	if node == nil {
		return nil, errors.New("node is nil")
	}
	value, _ := util.HasAnnotation(node, util.NodeRemoteEndpointsAnnotation)
	if value == "" {
		return nil, fmt.Errorf("node %s has no %s annotation", node.Name, util.NodeRemoteEndpointsAnnotation)
	}
	return DecodeServerEndpointInfo(value)
}

// normalize requires both addresses to be dialable from other nodes and
// rewrites them in canonical form.
func (e *ServerEndpointInfo) normalize() error {
	server, err := parseRoutable("server", e.ServerEndpoint, endpointutil.Http, endpointutil.Https)
	if err != nil {
		return err
	}
	agent, err := parseRoutable("agent", e.AgentEndpoint, endpointutil.Grpc)
	if err != nil {
		return err
	}
	e.ServerEndpoint, e.AgentEndpoint = server.String(), agent.String()
	return nil
}

// parseRoutable parses an address another node has to dial: one of the given
// schemes, a host that is not local, and an explicit port.
func parseRoutable(name, raw string, schemes ...endpointutil.Scheme) (*endpointutil.Endpoint, error) {
	ep, err := endpointutil.ParseEndpoint(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid %s endpoint %q: %w", name, raw, err)
	}
	if !slices.Contains(schemes, ep.Scheme) {
		return nil, fmt.Errorf("invalid %s endpoint %q: scheme must be one of %v", name, raw, schemes)
	}
	if ep.IsLoopback() {
		return nil, fmt.Errorf("invalid %s endpoint %q: host is not reachable from other nodes", name, raw)
	}
	if ep.Port == "" {
		return nil, fmt.Errorf("invalid %s endpoint %q: port is required", name, raw)
	}
	return ep, nil
}
