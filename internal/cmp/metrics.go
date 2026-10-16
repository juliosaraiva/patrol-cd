package cmp

import (
	"errors"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// metrics are the server's Prometheus collectors; a nil *metrics records nothing.
type metrics struct {
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	inFlight prometheus.Gauge
}

func newMetrics(reg prometheus.Registerer, plugin string) (*metrics, error) {
	if reg == nil {
		return nil, nil
	}
	labels := prometheus.Labels{"plugin": plugin}
	m := &metrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "patrol_cmp_requests_total",
			Help:        "Requests served by the config management plugin server, by operation and outcome (success or error code).",
			ConstLabels: labels,
		}, []string{"operation", "outcome"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:        "patrol_cmp_request_duration_seconds",
			Help:        "Duration of config management plugin requests (receive, extract, commands), by operation.",
			ConstLabels: labels,
			Buckets:     []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120},
		}, []string{"operation"}),
		inFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        "patrol_cmp_requests_in_flight",
			Help:        "Config management plugin requests currently being served.",
			ConstLabels: labels,
		}),
	}
	for _, c := range []prometheus.Collector{m.requests, m.duration, m.inFlight} {
		if err := reg.Register(c); err != nil {
			var are prometheus.AlreadyRegisteredError
			if errors.As(err, &are) {
				return nil, fmt.Errorf("cmp: metrics already registered: %w", err)
			}
			return nil, fmt.Errorf("cmp: register metrics: %w", err)
		}
	}
	return m, nil
}

func (m *metrics) begin() {
	if m != nil {
		m.inFlight.Inc()
	}
}

func (m *metrics) end(op, outcome string, d time.Duration) {
	if m == nil {
		return
	}
	m.inFlight.Dec()
	m.requests.WithLabelValues(op, outcome).Inc()
	m.duration.WithLabelValues(op).Observe(d.Seconds())
}
