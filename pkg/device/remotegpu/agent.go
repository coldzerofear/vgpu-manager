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

package remotegpu

// Talking to a node's remote-agent, shared by the DRA kubelet plugin and the
// device plugin. Nothing here depends on the DRA API.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/coldzerofear/vgpu-manager/pkg/api/remoteagent"
	endpointutil "github.com/coldzerofear/vgpu-manager/pkg/util/endpoint"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// The two endpoint kinds of the remote path, parsed in one place so every
// flag, published value and RPC field agrees on defaults and allowed schemes:
//
//   - lupine-server: http (port DefaultServerPort) or https (port 443), the
//     way the lupine client reads LUPINE_SERVER (docs/lupine_env_reference.md);
//   - remote-agent: grpc (port DefaultAgentPort) over TCP, or a unix socket
//     for callers on the same node.
const (
	// DefaultServerPort is lupine-server's default listen port
	// (docs/lupine_env_reference.md, LUPINE_PORT).
	DefaultServerPort = 14833
	// DefaultAgentPort is the remote-agent gRPC port.
	DefaultAgentPort = 14834
	// AgentCallTimeout bounds one short call to a remote-agent.
	AgentCallTimeout = 5 * time.Second
)

// ParseServerEndpoint parses a lupine-server endpoint; host and path are
// optional (an empty host is the caller's to fill in).
func ParseServerEndpoint(raw string) (*endpointutil.Endpoint, error) {
	e, err := endpointutil.ParseEndpoint(raw, endpointutil.WithDefaultScheme(endpointutil.Http))
	if err != nil {
		return nil, fmt.Errorf("invalid lupine-server endpoint %q: %w", raw, err)
	}
	switch e.Scheme {
	case endpointutil.Http:
		e.DefaultPort(DefaultServerPort)
	case endpointutil.Https:
		e.DefaultPort(443)
	default:
		return nil, fmt.Errorf("invalid lupine-server endpoint %q: scheme must be http or https", raw)
	}
	return e, nil
}

// ParseAgentEndpoint parses a remote-agent endpoint: grpc://host:port
// (host optional, port defaults) or unix:///abs/path. http(s) schemes are
// accepted as grpc for values published by older builds.
func ParseAgentEndpoint(raw string) (*endpointutil.Endpoint, error) {
	e, err := endpointutil.ParseEndpoint(raw,
		endpointutil.WithDefaultScheme(endpointutil.Grpc),
		endpointutil.WithDefaultPort(DefaultAgentPort))
	if err != nil {
		return nil, fmt.Errorf("invalid remote-agent endpoint %q: %w", raw, err)
	}
	switch e.Scheme {
	case endpointutil.Grpc, endpointutil.Unix:
	case endpointutil.Http, endpointutil.Https:
		e.Scheme = endpointutil.Grpc
	default:
		return nil, fmt.Errorf("invalid remote-agent endpoint %q: scheme must be grpc or unix", raw)
	}
	return e, nil
}

// ErrServerNotListening is returned (wrapped) by ServerInfo when the agent
// answers but reports that lupine-server did not pass its last probe.
var ErrServerNotListening = errors.New("lupine-server is not listening")

// ServerInfo asks the remote-agent at agentEndpoint what it knows about its
// lupine-server: reachability, the CUDA version it was built with, and the
// endpoint other nodes should use. This is how every other component learns
// about the server without having its address configured. A server that
// did not pass the agent's last probe is an ErrServerNotListening error.
func ServerInfo(ctx context.Context, agentEndpoint string) (*remoteagent.ServerInfoResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, AgentCallTimeout)
	defer cancel()

	conn, err := DialAgent(agentEndpoint)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()

	info, err := remoteagent.NewRemoteAgentClient(conn).ServerInfo(ctx, &remoteagent.ServerInfoRequest{})
	if err != nil {
		return nil, fmt.Errorf("remote-agent %s: %w", agentEndpoint, err)
	}
	if !info.Listening {
		return nil, fmt.Errorf("remote-agent %s (node %s): %w", agentEndpoint, info.NodeName, ErrServerNotListening)
	}
	return info, nil
}

// DialAgent opens a client connection to the agent. K1: plaintext;
// TLS/credentials arrive with D5 (multi-tenant gate). grpc.NewClient does not
// connect until the first RPC, so this never blocks.
func DialAgent(agentEndpoint string) (*grpc.ClientConn, error) {
	target, err := agentDialTarget(agentEndpoint)
	if err != nil {
		return nil, err
	}
	return grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
}

// agentDialTarget turns an agent endpoint (URL form; grpc://host:port[/path]
// as published, http(s):// accepted for older publishers, or unix:///path
// for a same-node socket) into a gRPC dial target: bare host:port, or the
// unix:// URL grpc-go resolves itself. A future gateway path prefix needs a
// gRPC-aware route, not this dial.
func agentDialTarget(agentEndpoint string) (string, error) {
	endpoint, err := ParseAgentEndpoint(agentEndpoint)
	if err != nil {
		return "", err
	}
	if endpoint.Scheme != endpointutil.Unix && (endpoint.Host == "" || endpoint.Port == "0") {
		return "", fmt.Errorf("invalid remote-agent endpoint %q: a host and a non-zero port are required", agentEndpoint)
	}
	return endpoint.DialTarget(), nil
}

// ResolveAgentDial turns a configured agent endpoint into the address a
// component on nodeName dials its agent at. A unix socket is used as is (it
// is bind-mounted from the host). A grpc endpoint without a host gets the
// node's InternalIP: the agent listens there under hostNetwork, while the
// caller may run in the pod network, where a loopback reaches only itself.
func ResolveAgentDial(ctx context.Context, kubeClient kubernetes.Interface, nodeName, raw string) (string, error) {
	agentDial, err := ParseAgentEndpoint(raw)
	if err != nil {
		return "", err
	}
	if agentDial.Scheme != endpointutil.Unix && agentDial.Host == "" {
		ip, err := nodeInternalIP(ctx, kubeClient, nodeName)
		if err != nil {
			return "", fmt.Errorf("derive agent endpoint: %w", err)
		}
		agentDial.Host = ip
	}
	return agentDial.String(), nil
}

func nodeInternalIP(ctx context.Context, kubeClient kubernetes.Interface, nodeName string) (string, error) {
	node, err := kubeClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{ResourceVersion: "0"})
	if err != nil {
		return "", fmt.Errorf("get node %s: %w", nodeName, err)
	}
	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP && addr.Address != "" {
			return addr.Address, nil
		}
	}
	return "", fmt.Errorf("node %s has no InternalIP address; set the host in --remote-agent-endpoint explicitly", nodeName)
}

// PublishableEndpoints validates what the agent reported before it is
// published for other nodes to dial: both must be present, in URL form, with
// a host that is not this machine's loopback. Returns the canonical forms.
func PublishableEndpoints(server, agent string) (string, string, error) {
	if server == "" || agent == "" {
		return "", "", fmt.Errorf("no routable endpoint reported yet (server %q, agent %q)", server, agent)
	}
	s, err := ParseServerEndpoint(server)
	if err != nil || s.IsLoopback() {
		return "", "", fmt.Errorf("reported lupine-server endpoint %q is not publishable: %v", server, err)
	}
	a, err := ParseAgentEndpoint(agent)
	if err != nil || a.Scheme != endpointutil.Grpc || a.IsLoopback() {
		// A unix-scheme endpoint works for this node's own dial but must
		// never be advertised: IsLoopback() is unconditionally true for it
		// (see its doc comment), so the explicit Scheme check here is
		// belt-and-suspenders, not redundant with it.
		return "", "", fmt.Errorf("reported remote-agent endpoint %q is not publishable: %v", agent, err)
	}
	return s.String(), a.String(), nil
}

// ProbeServer asks the agent at agentDial about its lupine-server and returns
// what may be published for other nodes: both endpoints in canonical form,
// the server's CUDA version and its client bundle etag. Any error means the
// server must not be offered to other nodes right now.
func ProbeServer(ctx context.Context, agentDial string) (*ServerEndpointInfo, error) {
	info, err := ServerInfo(ctx, agentDial)
	if err != nil {
		return nil, err
	}
	agent := info.AgentEndpoint
	if agent == "" {
		// The agent has not found its own routable host yet. The address this
		// call just reached it at will do; PublishableEndpoints still rejects
		// it when other nodes cannot use it (a unix socket, for one).
		agent = agentDial
	}
	server, agent, err := PublishableEndpoints(info.Endpoint, agent)
	if err != nil {
		return nil, fmt.Errorf("remote-agent %s: %w", agentDial, err)
	}
	return &ServerEndpointInfo{
		ServerEndpoint:    server,
		AgentEndpoint:     agent,
		ServerCUDAVersion: info.CudaDriverVersion,
		BundleETag:        info.ClientBundleEtag,
	}, nil
}
