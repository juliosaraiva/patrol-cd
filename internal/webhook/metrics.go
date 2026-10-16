package webhook

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
)

// Request results (label "result" of patrol_webhook_requests_total).
const (
	resultAccepted         = "accepted"
	resultIgnored          = "ignored"
	resultDuplicate        = "duplicate"
	resultUnknownProvider  = "unknown_provider"
	resultMethodNotAllowed = "method_not_allowed"
	resultRateLimited      = "rate_limited"
	resultTooLarge         = "too_large"
	resultInvalid          = "invalid_payload"
	resultUnsupported      = "unsupported_event"
	resultUnauthorized     = "unauthorized"
	resultUnsigned         = "unsigned_rejected"
	resultQueueFull        = "queue_full"
	// resultBusy: the in-flight request body budget is exhausted (503, retriable).
	resultBusy = "busy"
	// resultSecretsUnavailable: patrol-secret not loaded yet (503, retriable).
	resultSecretsUnavailable = "secrets_unavailable"
	resultError              = "error"
)

// Refresh results (label "result" of patrol_webhook_refreshes_total).
const (
	refreshRequested = "requested"
	refreshNotFound  = "not_found"
	refreshError     = "error"

	kindApplication    = "application"
	kindApplicationSet = "applicationset"
)

type metrics struct {
	requests  *prometheus.CounterVec
	refreshes *prometheus.CounterVec
	inFlight  prometheus.Gauge
	duration  prometheus.Histogram
}

// newMetrics creates the collectors and registers them with reg when it is non-nil.
// Labels are bounded: provider names are a closed set and repository URLs (untrusted,
// unbounded) are deliberately not used as labels.
//
//	patrol_webhook_requests_total{provider,result}   counter
//	patrol_webhook_refreshes_total{kind,result}      counter
//	patrol_webhook_events_in_flight                  gauge
//	patrol_webhook_event_duration_seconds            histogram
func newMetrics(reg prometheus.Registerer) (*metrics, error) {
	m := &metrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patrol_webhook_requests_total",
			Help: "Webhook requests by provider and result.",
		}, []string{"provider", "result"}),
		refreshes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patrol_webhook_refreshes_total",
			Help: "Refresh annotations requested by webhooks, by object kind and result.",
		}, []string{"kind", "result"}),
		inFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "patrol_webhook_events_in_flight",
			Help: "Webhook events currently being matched against Applications and ApplicationSets.",
		}),
		duration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "patrol_webhook_event_duration_seconds",
			Help:    "Time spent matching one webhook event.",
			Buckets: prometheus.ExponentialBuckets(0.001, 2, 15),
		}),
	}
	if reg != nil {
		for _, c := range []prometheus.Collector{m.requests, m.refreshes, m.inFlight, m.duration} {
			if err := reg.Register(c); err != nil {
				return nil, fmt.Errorf("register webhook metrics: %w", err)
			}
		}
	}
	return m, nil
}
