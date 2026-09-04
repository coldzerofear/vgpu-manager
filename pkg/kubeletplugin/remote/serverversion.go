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
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"time"

	"github.com/Masterminds/semver"
	endpointutil "github.com/coldzerofear/vgpu-manager/pkg/util/endpoint"
)

// ServerCUDAVersionHeader is the response header lupine-server puts on every
// reply. Its value is the CUDA version the server binary was built with
// (for example "13.3.73").
const ServerCUDAVersionHeader = "x-lupine-cuda-version"

// probeClient talks to the node IP directly: HTTP_PROXY in the pod must not
// redirect it (Proxy: nil). Keep-alive is off because lupine-server forks one
// child per connection, so there is nothing to reuse.
var probeClient = &http.Client{
	Transport: &http.Transport{
		Proxy: nil,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
		DisableKeepAlives: true,
	},
}

// ProbeServerCUDAVersion asks the lupine-server at endpoint which CUDA
// version it was built with.
//
// Since lupine #660 the RPC port also answers plain HTTP/1.1, and every reply
// carries x-lupine-cuda-version, even a 404. So one GET on "/" is enough:
// only the header matters, not the status code.
//
// Returns an error when the server is down, does not speak HTTP (a build
// older than #660), or the header is missing or unparseable.
func ProbeServerCUDAVersion(ctx context.Context, endpoint string, timeout time.Duration) (*semver.Version, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	serverEndpoint, err := endpointutil.ParseEndpoint(endpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid lupine-server endpoint %q: %w", endpoint, err)
	}
	serverEndpoint.DefaultPort(DefaultServerPort)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, serverEndpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := probeClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("lupine-server %s not answering HTTP: %w", endpoint, err)
	}
	defer resp.Body.Close()

	raw := resp.Header.Get(ServerCUDAVersionHeader)
	if raw == "" {
		return nil, fmt.Errorf("lupine-server %s answered without %s (build too old?)", endpoint, ServerCUDAVersionHeader)
	}
	v, err := semver.NewVersion(raw)
	if err != nil {
		return nil, fmt.Errorf("lupine-server %s reports unparseable CUDA version %q: %w", endpoint, raw, err)
	}
	return v, nil
}

// EffectiveCUDACeiling is the version a client artifact must not exceed: the
// lower of the node driver ceiling and the lupine-server build version. A
// client newer than the server would send RPCs the server does not know.
// A nil server version (not learned yet) leaves the driver ceiling alone.
func EffectiveCUDACeiling(driver, server *semver.Version) *semver.Version {
	if server == nil {
		return driver
	}
	if driver == nil || server.Compare(driver) < 0 {
		return server
	}
	return driver
}
