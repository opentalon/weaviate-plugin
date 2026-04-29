package main

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Prometheus metrics for the translator pre-processor. Registered against
// a private registry (see metricsRegistry below) so the plugin can expose
// them on its own /metrics endpoint without colliding with the global
// prometheus default registry — which the opentalon orchestrator may
// already be using.
var (
	metricsRegistry = prometheus.NewRegistry()

	// translatorCallsTotal counts every translator entry by outcome. The
	// callsite label tracks which plugin action triggered the call —
	// useful to spot e.g. "every prepare translates but searches don't".
	translatorCallsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "weaviate_plugin",
			Subsystem: "translator",
			Name:      "calls_total",
			Help:      "Translator outcomes by callsite. result=translated|skipped_target_lang|skipped_disabled|failed.",
		},
		[]string{"callsite", "result"},
	)

	// translatorDurationSeconds measures the wall-time of the full translator
	// step (detect + translate or just detect for skip cases). Buckets cover
	// 1ms..2s — 80–100ms is the steady-state target inside the cluster.
	translatorDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "weaviate_plugin",
			Subsystem: "translator",
			Name:      "duration_seconds",
			Help:      "Wall-time of the translator step (detect + translate) by callsite and outcome.",
			Buckets:   []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2},
		},
		[]string{"callsite", "result"},
	)

	// translatorDetectedLangsTotal tracks the source-language distribution
	// reported by the /detect short-circuit. Only populated when the
	// detect-first path runs — disabled means this stays at 0.
	translatorDetectedLangsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "weaviate_plugin",
			Subsystem: "translator",
			Name:      "detected_lang_total",
			Help:      "Source language distribution reported by the /detect short-circuit.",
		},
		[]string{"lang"},
	)
)

func init() {
	metricsRegistry.MustRegister(translatorCallsTotal, translatorDurationSeconds, translatorDetectedLangsTotal)
}
