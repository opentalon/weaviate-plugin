package main

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Prometheus metrics for the background sync worker. Registered against a
// private registry (see metricsRegistry below) so the plugin can expose them
// on its own /metrics endpoint without colliding with the global prometheus
// default registry — which the opentalon orchestrator may already be using.
var (
	metricsRegistry = prometheus.NewRegistry()

	// --- Background sync worker metrics ---

	syncJobsEnqueued = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "weaviate_plugin",
			Subsystem: "sync",
			Name:      "jobs_enqueued_total",
			Help:      "Total background sync jobs enqueued. type=sync_actions.",
		},
		[]string{"type"},
	)

	syncJobDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "weaviate_plugin",
			Subsystem: "sync",
			Name:      "job_duration_seconds",
			Help:      "Wall-time of each background sync job by type and outcome.",
			Buckets:   prometheus.ExponentialBuckets(0.1, 2, 12), // 0.1s .. ~200s
		},
		[]string{"type", "status"},
	)

	syncQueueDepth = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: "weaviate_plugin",
			Subsystem: "sync",
			Name:      "queue_depth",
			Help:      "Current number of pending sync jobs in the background queue.",
		},
	)
)

func init() {
	metricsRegistry.MustRegister(
		syncJobsEnqueued, syncJobDuration, syncQueueDepth,
	)
}
