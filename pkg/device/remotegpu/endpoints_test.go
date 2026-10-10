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

import (
	"errors"
	"testing"

	"github.com/coldzerofear/vgpu-manager/pkg/util"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestServerEndpointInfoRoundTrip(t *testing.T) {
	in := ServerEndpointInfo{
		ServerEndpoint:    "10.0.0.1:14833", // no scheme: http is assumed
		AgentEndpoint:     "grpc://10.0.0.1:14834",
		ServerCUDAVersion: "13.3.73",
		BundleETag:        `"sha256:abc"`,
	}
	value, err := in.Encode()
	if err != nil {
		t.Fatal(err)
	}
	out, err := DecodeServerEndpointInfo(value)
	if err != nil {
		t.Fatal(err)
	}
	want := ServerEndpointInfo{
		ServerEndpoint:    "http://10.0.0.1:14833",
		AgentEndpoint:     "grpc://10.0.0.1:14834",
		ServerCUDAVersion: "13.3.73",
		BundleETag:        `"sha256:abc"`,
	}
	if *out != want {
		t.Fatalf("got %+v, want %+v", *out, want)
	}
}

func TestServerEndpointInfoRejectsUnusableAddresses(t *testing.T) {
	const server, agent = "http://10.0.0.1:14833", "grpc://10.0.0.1:14834"
	cases := map[string]ServerEndpointInfo{
		"loopback server":      {ServerEndpoint: "http://127.0.0.1:14833", AgentEndpoint: agent},
		"wildcard server":      {ServerEndpoint: "http://:14833", AgentEndpoint: agent},
		"server without port":  {ServerEndpoint: "http://10.0.0.1", AgentEndpoint: agent},
		"server wrong scheme":  {ServerEndpoint: "grpc://10.0.0.1:14833", AgentEndpoint: agent},
		"server missing":       {AgentEndpoint: agent},
		"agent without scheme": {ServerEndpoint: server, AgentEndpoint: "10.0.0.1:14834"},
		"agent unix socket":    {ServerEndpoint: server, AgentEndpoint: "unix:///run/agent.sock"},
		"agent missing":        {ServerEndpoint: server},
	}
	for name, in := range cases {
		if _, err := in.Encode(); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if _, err := DecodeServerEndpointInfo("not json"); err == nil {
		t.Error("invalid JSON must be rejected")
	}
}

func TestServerEndpointInfoClone(t *testing.T) {
	var none *ServerEndpointInfo
	if got := none.Clone().(*ServerEndpointInfo); got != nil {
		t.Fatalf("nil clone = %+v", got)
	}
	in := &ServerEndpointInfo{ServerEndpoint: "http://10.0.0.1:14833", AgentEndpoint: "grpc://10.0.0.1:14834"}
	out := in.Clone().(*ServerEndpointInfo)
	if out == in || *out != *in {
		t.Fatalf("clone = %p %+v, want a copy of %p %+v", out, *out, in, *in)
	}
}

func TestDecodeUnreachableServerEndpointInfo(t *testing.T) {
	if _, err := DecodeServerEndpointInfo(UnreachableServerEndpointInfo); !errors.Is(err, ErrServerUnreachable) {
		t.Fatalf("the unreachable marker must decode to ErrServerUnreachable, got %v", err)
	}
	if _, err := DecodeServerEndpointInfo(`{"serverEndpoint":"http://10.0.0.1:14833"}`); err == nil || errors.Is(err, ErrServerUnreachable) {
		t.Fatalf("a half-published value is invalid, not unreachable: %v", err)
	}
}

func TestGetServerEndpointInfo(t *testing.T) {
	if _, err := GetServerEndpointInfo(nil); err == nil {
		t.Fatal("nil node must be an error")
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu-1"}}
	if _, err := GetServerEndpointInfo(node); err == nil {
		t.Fatal("a node without the annotation must be an error")
	}
	value, err := ServerEndpointInfo{ServerEndpoint: "http://10.0.0.1:14833", AgentEndpoint: "grpc://10.0.0.1:14834"}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	node.Annotations = map[string]string{util.NodeRemoteEndpointsAnnotation: value}
	got, err := GetServerEndpointInfo(node)
	if err != nil || got.ServerEndpoint != "http://10.0.0.1:14833" || got.AgentEndpoint != "grpc://10.0.0.1:14834" {
		t.Fatalf("got %+v, %v", got, err)
	}
}
