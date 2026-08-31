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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestInjectCollector(t *testing.T) {
	artifacts := t.TempDir()
	for _, name := range []string{"12.4.1", "12.9.1", "libvgpu-control.so.1.2.3"} {
		if err := os.Mkdir(filepath.Join(artifacts, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A regular file whose name parses as a version must not count.
	if err := os.WriteFile(filepath.Join(artifacts, "11.8.0"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	c := &injectCollector{
		node:         "consumer-1",
		artifactsDir: artifacts,
		snapshot: func() []preparedClaimSnapshot {
			return []preparedClaimSnapshot{
				{namespace: "ns1", name: "claim-a", devices: []preparedDeviceSnapshot{
					{serverNode: "gpu-node-1", endpoint: "10.0.0.1:14833"},
					{serverNode: "gpu-node-1", endpoint: "10.0.0.1:14833"},
					{serverNode: "gpu-node-2", endpoint: "10.0.0.2:14833"},
				}},
				{namespace: "ns2", name: "claim-b", devices: []preparedDeviceSnapshot{
					{serverNode: "gpu-node-1", endpoint: "10.0.0.1:14833"},
				}},
			}
		},
	}

	expected := `
# HELP remote_inject_claim_devices Remote GPU devices held by one prepared ResourceClaim on this consumer node, by serving GPU node and endpoint
# TYPE remote_inject_claim_devices gauge
remote_inject_claim_devices{claim_name="claim-a",claim_namespace="ns1",endpoint="10.0.0.1:14833",node="consumer-1",server_node="gpu-node-1"} 2
remote_inject_claim_devices{claim_name="claim-a",claim_namespace="ns1",endpoint="10.0.0.2:14833",node="consumer-1",server_node="gpu-node-2"} 1
remote_inject_claim_devices{claim_name="claim-b",claim_namespace="ns2",endpoint="10.0.0.1:14833",node="consumer-1",server_node="gpu-node-1"} 1
# HELP remote_inject_client_artifacts Lupine client artifact versions materialized on this consumer node (1 per version directory)
# TYPE remote_inject_client_artifacts gauge
remote_inject_client_artifacts{cuda_version="12.4.1",node="consumer-1"} 1
remote_inject_client_artifacts{cuda_version="12.9.1",node="consumer-1"} 1
# HELP remote_inject_prepared_claims Number of remote-GPU ResourceClaims currently prepared on this consumer node
# TYPE remote_inject_prepared_claims gauge
remote_inject_prepared_claims{node="consumer-1"} 2
# HELP remote_inject_prepared_devices Number of remote GPU devices currently prepared on this consumer node, by serving GPU node and lupine-server endpoint
# TYPE remote_inject_prepared_devices gauge
remote_inject_prepared_devices{endpoint="10.0.0.1:14833",node="consumer-1",server_node="gpu-node-1"} 3
remote_inject_prepared_devices{endpoint="10.0.0.2:14833",node="consumer-1",server_node="gpu-node-2"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected)); err != nil {
		t.Fatal(err)
	}
}

func TestInjectCollectorEmpty(t *testing.T) {
	c := &injectCollector{
		node:         "consumer-1",
		artifactsDir: filepath.Join(t.TempDir(), "does-not-exist"),
		snapshot:     func() []preparedClaimSnapshot { return nil },
	}
	expected := `
# HELP remote_inject_prepared_claims Number of remote-GPU ResourceClaims currently prepared on this consumer node
# TYPE remote_inject_prepared_claims gauge
remote_inject_prepared_claims{node="consumer-1"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected)); err != nil {
		t.Fatal(err)
	}
}

func TestObserveEnsure(t *testing.T) {
	m := newInjectMetrics("consumer-1")
	m.observeEnsure("10.0.0.1:14834", 5*time.Millisecond, nil)
	m.observeEnsure("10.0.0.1:14834", 20*time.Millisecond, errors.New("boom"))

	if got := testutil.ToFloat64(m.ensureRequests.WithLabelValues("10.0.0.1:14834", "success")); got != 1 {
		t.Fatalf("success counter = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.ensureRequests.WithLabelValues("10.0.0.1:14834", "error")); got != 1 {
		t.Fatalf("error counter = %v, want 1", got)
	}
	if got := testutil.CollectAndCount(m.ensureDuration); got != 1 {
		t.Fatalf("expected 1 histogram series, got %d", got)
	}

	// nil receiver must be a no-op (metrics disabled).
	var nilMetrics *injectMetrics
	nilMetrics.observeEnsure("x", time.Millisecond, nil)

	// Registering into a fresh registry must not conflict between the two vecs.
	reg := prometheus.NewRegistry()
	reg.MustRegister(m.ensureRequests, m.ensureDuration)
}
