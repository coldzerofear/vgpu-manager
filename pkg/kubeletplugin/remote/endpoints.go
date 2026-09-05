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

	endpointutil "github.com/coldzerofear/vgpu-manager/pkg/util/endpoint"
)

// The two endpoint kinds of the remote path, parsed in one place so every
// flag, attribute and RPC field agrees on defaults and allowed schemes:
//
//   - lupine-server: http (port DefaultServerPort) or https (port 443), the
//     way the lupine client reads LUPINE_SERVER (docs/lupine_env_reference.md);
//   - remote-agent: grpc (port DefaultAgentPort) over TCP, or a unix socket
//     for callers on the same node.

// DefaultAgentPort is the remote-agent gRPC port.
const DefaultAgentPort = 14834

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
// accepted as grpc for attributes published by older builds.
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
