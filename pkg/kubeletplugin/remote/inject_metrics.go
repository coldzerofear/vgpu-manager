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
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/component-base/metrics/legacyregistry"
)

// Consumer-node metrics. The device-monitor cannot run on an inject node (no
// GPU, no NVML), and remote *usage* accounting (memory/utilization per
// container) is already exported from the GPU node's device-monitor with
// pod_node = <consumer node>. What only the inject plugin can report — and
// what these metrics cover — is this node's own remote-GPU view:
//
//   - which remote claims are prepared here and which servers they span
//     (remote_inject_prepared_claims / remote_inject_prepared_devices /
//     remote_inject_claim_devices);
//   - the health of the EnsureSession control path to the remote agents,
//     which no other component observes
//     (remote_inject_ensure_session_requests_total / _duration_seconds);
//   - which client artifact versions are materialized on this node
//     (remote_inject_client_artifacts), the D12 inventory that gates
//     NodePrepare.
//
// They are served on the inject plugin's --http-endpoint alongside the DRA
// request metrics (both live in the component-base legacyregistry).

var (
	injectPreparedClaims = prometheus.NewDesc(
		"remote_inject_prepared_claims",
		"Number of remote-GPU ResourceClaims currently prepared on this consumer node",
		[]string{"node"}, nil,
	)
	injectPreparedDevices = prometheus.NewDesc(
		"remote_inject_prepared_devices",
		"Number of remote GPU devices currently prepared on this consumer node, by serving GPU node and lupine-server endpoint",
		[]string{"node", "server_node", "endpoint"}, nil,
	)
	injectClaimDevices = prometheus.NewDesc(
		"remote_inject_claim_devices",
		"Remote GPU devices held by one prepared ResourceClaim on this consumer node, by serving GPU node and endpoint",
		[]string{"node", "claim_namespace", "claim_name", "server_node", "endpoint"}, nil,
	)
	injectClientArtifacts = prometheus.NewDesc(
		"remote_inject_client_artifacts",
		"Lupine client artifact versions materialized on this consumer node (1 per version directory)",
		[]string{"node", "cuda_version"}, nil,
	)
)

// injectMetrics holds the counter-style metrics the driver updates inline.
type injectMetrics struct {
	ensureRequests *prometheus.CounterVec
	ensureDuration *prometheus.HistogramVec
}

func newInjectMetrics(nodeName string) *injectMetrics {
	constLabels := prometheus.Labels{"node": nodeName}
	return &injectMetrics{
		ensureRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "remote_inject_ensure_session_requests_total",
			Help:        "EnsureSession calls from this consumer node to remote agents, by agent address and result",
			ConstLabels: constLabels,
		}, []string{"agent", "result"}),
		ensureDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:        "remote_inject_ensure_session_duration_seconds",
			Help:        "EnsureSession round-trip latency from this consumer node, by agent address",
			ConstLabels: constLabels,
			// The client-side deadline is ensureSessionTimeout (15s); the top
			// bucket sits just above it so timeouts stay distinguishable.
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 16},
		}, []string{"agent"}),
	}
}

func (m *injectMetrics) observeEnsure(agentAddr string, elapsed time.Duration, err error) {
	if m == nil {
		return
	}
	result := "success"
	if err != nil {
		result = "error"
	}
	m.ensureRequests.WithLabelValues(agentAddr, result).Inc()
	m.ensureDuration.WithLabelValues(agentAddr).Observe(elapsed.Seconds())
}

// preparedDeviceSnapshot is one prepared remote device as seen by the
// gauges: the pool name is the serving GPU node (pool name == node name).
type preparedDeviceSnapshot struct {
	serverNode string
	endpoint   string
}

type preparedClaimSnapshot struct {
	namespace string
	name      string
	devices   []preparedDeviceSnapshot
}

// injectCollector computes the gauge metrics at scrape time. It reads
// through narrow accessors instead of the driver directly so the gauge
// logic is testable without a running kubelet plugin.
type injectCollector struct {
	node         string
	artifactsDir string
	snapshot     func() []preparedClaimSnapshot
}

func (c *injectCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- injectPreparedClaims
	ch <- injectPreparedDevices
	ch <- injectClaimDevices
	ch <- injectClientArtifacts
}

func (c *injectCollector) Collect(ch chan<- prometheus.Metric) {
	claims := c.snapshot()

	type serverKey struct{ serverNode, endpoint string }
	perServer := map[serverKey]int{}
	for _, claim := range claims {
		perClaim := map[serverKey]int{}
		for _, dev := range claim.devices {
			key := serverKey{dev.serverNode, dev.endpoint}
			perServer[key]++
			perClaim[key]++
		}
		for key, n := range perClaim {
			ch <- prometheus.MustNewConstMetric(injectClaimDevices, prometheus.GaugeValue,
				float64(n), c.node, claim.namespace, claim.name, key.serverNode, key.endpoint)
		}
	}
	ch <- prometheus.MustNewConstMetric(injectPreparedClaims, prometheus.GaugeValue,
		float64(len(claims)), c.node)
	for key, n := range perServer {
		ch <- prometheus.MustNewConstMetric(injectPreparedDevices, prometheus.GaugeValue,
			float64(n), c.node, key.serverNode, key.endpoint)
	}

	for _, version := range listArtifactVersions(c.artifactsDir) {
		ch <- prometheus.MustNewConstMetric(injectClientArtifacts, prometheus.GaugeValue,
			1, c.node, version)
	}
}

// snapshotPrepared exports the prepared set for the collector.
func (d *InjectDriver) snapshotPrepared() []preparedClaimSnapshot {
	d.preparedMu.Lock()
	defer d.preparedMu.Unlock()
	out := make([]preparedClaimSnapshot, 0, len(d.prepared))
	for _, pc := range d.prepared {
		snap := preparedClaimSnapshot{namespace: pc.claim.Namespace, name: pc.claim.Name}
		for _, rd := range pc.devices {
			snap.devices = append(snap.devices, preparedDeviceSnapshot{
				serverNode: rd.result.Pool,
				endpoint:   rd.info.Endpoint,
			})
		}
		out = append(out, snap)
	}
	return out
}

// RegisterInjectMetrics registers the inject-side metrics into the
// component-base legacyregistry — the registry the plugin's --http-endpoint
// serves (sigs.k8s.io/dra-driver-nvidia-gpu/pkg/metrics). Call at most once,
// after the driver is constructed.
func RegisterInjectMetrics(d *InjectDriver) {
	legacyregistry.RawMustRegister(
		d.metrics.ensureRequests,
		d.metrics.ensureDuration,
		&injectCollector{
			node:         d.config.NodeName,
			artifactsDir: d.config.ArtifactsDir,
			snapshot:     d.snapshotPrepared,
		},
	)
}
