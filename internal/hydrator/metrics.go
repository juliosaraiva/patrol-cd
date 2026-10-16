package hydrator

import (
	"errors"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Hydration results (label result of patrol_hydrator_hydrations_total).
const (
	ResultHydrated = "hydrated"
	ResultSkipped  = "skipped"
	ResultFailed   = "failed"
)

// Evaluation outcomes (label outcome of patrol_hydrator_app_evaluations_total).
const (
	OutcomeHydrate = "hydrate"
	OutcomeSkip    = "skip"
	OutcomeError   = "error"
)

// metrics are the hydrator metrics. A nil *metrics records nothing.
type metrics struct {
	hydrations  *prometheus.CounterVec
	duration    *prometheus.HistogramVec
	groupSize   prometheus.Histogram
	evaluations *prometheus.CounterVec
}

func newMetrics(reg prometheus.Registerer) (*metrics, error) {
	m := &metrics{
		hydrations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patrol_hydrator_hydrations_total",
			Help: "Number of processed hydration keys (groups of Applications sharing a dry source and destination branch) by result.",
		}, []string{"result"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "patrol_hydrator_hydration_duration_seconds",
			Help:    "Duration of the processing of one hydration key (render, commit, status updates).",
			Buckets: []float64{0.25, 0.5, 1, 2, 5, 10, 30, 60, 120, 300},
		}, []string{"result"}),
		groupSize: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "patrol_hydrator_group_applications",
			Help:    "Number of Applications hydrated together in one commit.",
			Buckets: []float64{1, 2, 5, 10, 25, 50, 100, 250},
		}),
		evaluations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patrol_hydrator_app_evaluations_total",
			Help: "Number of hydration need evaluations of Applications by outcome.",
		}, []string{"outcome"}),
	}
	if reg == nil {
		return m, nil
	}
	var err error
	if m.hydrations, err = register(reg, m.hydrations); err != nil {
		return nil, err
	}
	if m.duration, err = register(reg, m.duration); err != nil {
		return nil, err
	}
	if m.groupSize, err = register(reg, m.groupSize); err != nil {
		return nil, err
	}
	if m.evaluations, err = register(reg, m.evaluations); err != nil {
		return nil, err
	}
	return m, nil
}

func register[C prometheus.Collector](reg prometheus.Registerer, c C) (C, error) {
	if err := reg.Register(c); err != nil {
		var are prometheus.AlreadyRegisteredError
		if errors.As(err, &are) {
			if existing, ok := are.ExistingCollector.(C); ok {
				return existing, nil
			}
		}
		var zero C
		return zero, fmt.Errorf("register hydrator metrics: %w", err)
	}
	return c, nil
}

func (m *metrics) observeHydration(result string, apps int, d time.Duration) {
	if m == nil {
		return
	}
	m.hydrations.WithLabelValues(result).Inc()
	m.duration.WithLabelValues(result).Observe(d.Seconds())
	if apps > 0 {
		m.groupSize.Observe(float64(apps))
	}
}

func (m *metrics) observeEvaluation(outcome string) {
	if m != nil {
		m.evaluations.WithLabelValues(outcome).Inc()
	}
}
