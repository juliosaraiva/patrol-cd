package render

import (
	"errors"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// metrics are the renderer's Prometheus collectors; a nil *metrics records nothing.
type metrics struct {
	duration *prometheus.HistogramVec
	jsonnet  *prometheus.CounterVec
	cmp      *prometheus.CounterVec
}

func newMetrics(reg prometheus.Registerer) (*metrics, error) {
	if reg == nil {
		return nil, nil
	}
	m := &metrics{
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "patrol_render_duration_seconds",
			Help:    "Duration of manifest generation for one source, by source type and outcome.",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120},
		}, []string{"source_type", "outcome"}),
		jsonnet: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patrol_render_jsonnet_evaluations_total",
			Help: "Jsonnet evaluation batches run in the sandboxed evaluator, by outcome.",
		}, []string{"outcome"}),
		cmp: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patrol_render_cmp_requests_total",
			Help: "Requests to config management plugin sidecars, by operation and outcome.",
		}, []string{"operation", "outcome"}),
	}
	for _, c := range []prometheus.Collector{m.duration, m.jsonnet, m.cmp} {
		if err := reg.Register(c); err != nil {
			var are prometheus.AlreadyRegisteredError
			if errors.As(err, &are) {
				return nil, fmt.Errorf("render: metrics already registered: %w", err)
			}
			return nil, fmt.Errorf("render: register metrics: %w", err)
		}
	}
	return m, nil
}

func outcome(err error) string {
	if err != nil {
		return "error"
	}
	return "success"
}

func (m *metrics) observeRender(sourceType string, err error, d time.Duration) {
	if m == nil {
		return
	}
	if sourceType == "" {
		sourceType = "unknown"
	}
	m.duration.WithLabelValues(sourceType, outcome(err)).Observe(d.Seconds())
}

func (m *metrics) observeJsonnet(_ int, ok bool) {
	if m == nil {
		return
	}
	o := "success"
	if !ok {
		o = "error"
	}
	m.jsonnet.WithLabelValues(o).Inc()
}

func (m *metrics) observeCMP(op string, err error) {
	if m == nil {
		return
	}
	m.cmp.WithLabelValues(op, outcome(err)).Inc()
}
