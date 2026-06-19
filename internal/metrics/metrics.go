package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	RequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "keystone_requests_total",
		Help: "Total requests processed",
	}, []string{"provider", "tier", "model", "status"})

	RequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "keystone_request_duration_seconds",
		Help:    "Request duration in seconds",
		Buckets: prometheus.DefBuckets,
	}, []string{"provider", "tier"})

	KeyState = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "keystone_key_state",
		Help: "Key health state (1=healthy, 0.5=cooling, 0=dead)",
	}, []string{"provider", "key_id", "state"})

	SessionCount = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "keystone_session_count",
		Help: "Active session count",
	}, []string{"tier"})

	CacheHitsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "keystone_cache_hits_total",
		Help: "Estimated cache hits",
	}, []string{"session_id"})

	FallbackTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "keystone_fallback_total",
		Help: "Provider fallback events",
	}, []string{"from_provider", "to_provider", "tier"})

	ClassifierDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "keystone_classification_duration_seconds",
		Help:    "Classification duration",
		Buckets: []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5},
	})

	StickyDecisions = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "keystone_sticky_decisions_total",
		Help: "Sticky vs new binding decisions",
	}, []string{"decision"})
)
