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

package nri

import (
	"sync"
	"time"

	"k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
)

// Metrics for the NRI path. They register into legacyregistry, which is what
// metrics.RunPrometheusMetricsServer already gathers and serves, so the metrics
// endpoint itself needs no change.
//
// These exist because the failure they describe is otherwise invisible: a
// container the runtime never routed to us looks, from every other vantage
// point, exactly like a container we processed and had nothing to do for.
// nri_ready is the one to alert on — a node whose plugin is disconnected is a
// node accumulating containers with no vGPU isolation.

// CreateContainer outcomes. Every return path of the hook records exactly one.
const (
	// resultInjected: mounts and env were added to the container.
	resultInjected = "injected"
	// resultNotOurs: no claim-UID env, i.e. not a vGPU container of ours. The
	// overwhelming majority on a normal node.
	resultNotOurs = "not_ours"
	// resultNoOp: ours, but with nothing to inject (see Config.ResolveMounts).
	// A legitimate outcome, kept distinct from the rejections below.
	resultNoOp = "no_op"
	// resultRejectedUnprepared: the claim UID is not prepared on this node;
	// container creation was aborted.
	resultRejectedUnprepared = "rejected_unprepared"
	// resultRejectedResolve: resolving the injection failed; container creation
	// was aborted.
	resultRejectedResolve = "rejected_resolve"
	// resultObserveOnly: dry-run / no ResolveMounts wired (test-only).
	resultObserveOnly = "observe_only"
)

var (
	metricsOnce sync.Once

	nriReady = metrics.NewGauge(&metrics.GaugeOpts{
		Namespace: "vgpu_manager",
		Subsystem: "nri",
		Name:      "ready",
		Help: "Whether the in-process NRI plugin is registered with the container runtime " +
			"and has completed its Synchronize (1) or not (0). While 0, containers can be " +
			"created without their vGPU isolation.",
		StabilityLevel: metrics.ALPHA,
	})

	nriCreateContainerTotal = metrics.NewCounterVec(&metrics.CounterOpts{
		Namespace:      "vgpu_manager",
		Subsystem:      "nri",
		Name:           "create_container_total",
		Help:           "CreateContainer hook invocations by outcome.",
		StabilityLevel: metrics.ALPHA,
	}, []string{"result"})

	nriCreateContainerDuration = metrics.NewHistogramVec(&metrics.HistogramOpts{
		Namespace: "vgpu_manager",
		Subsystem: "nri",
		Name:      "create_container_duration_seconds",
		Help: "CreateContainer hook latency by outcome. Compare against the runtime's NRI " +
			"request timeout (2s by default): overrunning it detaches the plugin.",
		// Sub-millisecond through several seconds: the interesting region is
		// near the runtime's request budget.
		Buckets:        []float64{0.0005, 0.001, 0.005, 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5},
		StabilityLevel: metrics.ALPHA,
	}, []string{"result"})
)

// InitializeMetrics registers the NRI metrics. Safe to call more than once and
// from either plugin mode; call it before starting the metrics server.
func InitializeMetrics() {
	metricsOnce.Do(func() {
		legacyregistry.MustRegister(nriReady)
		legacyregistry.MustRegister(nriCreateContainerTotal)
		legacyregistry.MustRegister(nriCreateContainerDuration)
	})
}

func setReadyMetric(ready bool) {
	if ready {
		nriReady.Set(1)
		return
	}
	nriReady.Set(0)
}

func observeCreateContainer(result string, start time.Time) {
	nriCreateContainerTotal.WithLabelValues(result).Inc()
	nriCreateContainerDuration.WithLabelValues(result).Observe(time.Since(start).Seconds())
}
