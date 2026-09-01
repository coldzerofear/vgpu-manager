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

package kubeletplugin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/kubeletplugin/remote"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/dynamic-resource-allocation/resourceslice"
)

// A fake lupine-server: answers 404 like the real one, with the version header
// unless told to go silent.
func fakeLupineServer(t *testing.T) (*httptest.Server, *atomic.Value) {
	t.Helper()
	var version atomic.Value
	version.Store("13.3.73")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v := version.Load().(string); v != "" {
			w.Header().Set(remote.ServerCUDAVersionHeader, v)
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv, &version
}

func TestRemotePublisherServerVersion(t *testing.T) {
	ctx := context.Background()
	srv, version := fakeLupineServer(t)
	rp := &remotePublisher{
		nodeName: "gpu-node",
		spec:     &remote.PublishSpec{Endpoint: strings.TrimPrefix(srv.URL, "http://")},
	}
	pool := func() resourceslice.Pool {
		return rp.apply(resourceslice.Pool{Slices: []resourceslice.Slice{{
			Devices: []resourceapi.Device{{Name: "vgpu-0"}},
		}}})
	}
	published := func(p resourceslice.Pool) string {
		attr, ok := p.Slices[0].Devices[0].Attributes[remote.AttrServerCUDAVersion]
		if !ok {
			return ""
		}
		return *attr.VersionValue
	}

	if got := published(pool()); got != "" {
		t.Fatalf("nothing learned yet, but serverCudaVersion=%q was published", got)
	}

	changed, err := rp.refreshServerVersion(ctx)
	if err != nil || !changed {
		t.Fatalf("first answer must count as a change: changed=%v err=%v", changed, err)
	}
	if got := published(pool()); got != "13.3.73" {
		t.Fatalf("published serverCudaVersion=%q, want 13.3.73", got)
	}

	changed, err = rp.refreshServerVersion(ctx)
	if err != nil || changed {
		t.Fatalf("same answer must not count as a change: changed=%v err=%v", changed, err)
	}

	// The server comes back built from another image.
	version.Store("12.9.1")
	changed, err = rp.refreshServerVersion(ctx)
	if err != nil || !changed {
		t.Fatalf("new build must count as a change: changed=%v err=%v", changed, err)
	}
	if got := published(pool()); got != "12.9.1" {
		t.Fatalf("published serverCudaVersion=%q, want 12.9.1", got)
	}

	// A silent server keeps the last known value.
	version.Store("")
	if changed, err = rp.refreshServerVersion(ctx); err == nil || changed {
		t.Fatalf("silent server must be an error without a change: changed=%v err=%v", changed, err)
	}
	if got := published(pool()); got != "12.9.1" {
		t.Fatalf("last known version must survive a failed probe, got %q", got)
	}
}
