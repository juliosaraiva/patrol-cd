package treeapi

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// serverMetrics are the request instruments. Labels are bounded (route names, status codes).
type serverMetrics struct {
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

func newServerMetrics(reg prometheus.Registerer) (*serverMetrics, error) {
	m := &serverMetrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patrol_controller_treeapi_requests_total",
			Help: "Controller tree API requests by route and HTTP status code.",
		}, []string{"route", "code"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "patrol_controller_treeapi_request_duration_seconds",
			Help:    "Controller tree API request duration by route.",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
		}, []string{"route"}),
	}
	if reg == nil {
		return m, nil
	}
	var err error
	if m.requests, err = register(reg, m.requests); err != nil {
		return nil, err
	}
	if m.duration, err = register(reg, m.duration); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *serverMetrics) observe(route string, status int, d time.Duration) {
	m.requests.WithLabelValues(route, strconv.Itoa(status)).Inc()
	m.duration.WithLabelValues(route).Observe(d.Seconds())
}

// register registers c, reusing an identical collector registered earlier.
func register[T prometheus.Collector](reg prometheus.Registerer, c T) (T, error) {
	if err := reg.Register(c); err != nil {
		var are prometheus.AlreadyRegisteredError
		if errors.As(err, &are) {
			if existing, ok := are.ExistingCollector.(T); ok {
				return existing, nil
			}
		}
		var zero T
		return zero, fmt.Errorf("register tree API metrics: %w", err)
	}
	return c, nil
}
