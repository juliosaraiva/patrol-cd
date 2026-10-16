package compare

import (
	"errors"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
)

// metrics are the comparison instruments. Label values are bounded (phases and fixed
// failure kinds); Application names are not used as labels.
type metrics struct {
	duration *prometheus.HistogramVec
	phase    *prometheus.HistogramVec
	failures *prometheus.CounterVec
	results  *prometheus.CounterVec
}

// Failure kinds counted by patrol_compare_failures_total.
const (
	failureManifests = "manifests"
	failureDeferred  = "deferred"
	failureLive      = "live"
	failureNormalize = "normalize"
	failureDiff      = "diff"
	failureProject   = "project"
	failureHealth    = "health"
	failureTree      = "tree"
)

func newMetrics(reg prometheus.Registerer) (*metrics, error) {
	m := &metrics{
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "patrol_compare_duration_seconds",
			Help:    "Duration of application state comparisons.",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 4, 8, 16, 32, 64},
		}, []string{"local"}),
		phase: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "patrol_compare_phase_duration_seconds",
			Help:    "Duration of application state comparison phases.",
			Buckets: []float64{0.005, 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 4, 8, 16},
		}, []string{"phase"}),
		failures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patrol_compare_failures_total",
			Help: "Comparison failures by kind (manifests, deferred, live, normalize, diff, project, health, tree).",
		}, []string{"kind"}),
		results: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patrol_compare_results_total",
			Help: "Completed comparisons by resulting sync status.",
		}, []string{"sync_status"}),
	}
	if reg == nil {
		return m, nil
	}
	var err error
	if m.duration, err = register(reg, m.duration); err != nil {
		return nil, err
	}
	if m.phase, err = register(reg, m.phase); err != nil {
		return nil, err
	}
	if m.failures, err = register(reg, m.failures); err != nil {
		return nil, err
	}
	if m.results, err = register(reg, m.results); err != nil {
		return nil, err
	}
	return m, nil
}

// register registers c, reusing an identical collector registered earlier (several
// Managers may share one registry).
func register[T prometheus.Collector](reg prometheus.Registerer, c T) (T, error) {
	if err := reg.Register(c); err != nil {
		var are prometheus.AlreadyRegisteredError
		if errors.As(err, &are) {
			if existing, ok := are.ExistingCollector.(T); ok {
				return existing, nil
			}
		}
		var zero T
		return zero, fmt.Errorf("register compare metrics: %w", err)
	}
	return c, nil
}

func (m *metrics) failure(kind string) { m.failures.WithLabelValues(kind).Inc() }
