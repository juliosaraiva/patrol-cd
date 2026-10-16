package repository

import (
	"errors"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// metrics are the Prometheus collectors of a Resolver. Labels never carry URLs, object
// names or credential identities (bounded cardinality, nothing sensitive).
type metrics struct {
	resolutions *prometheus.CounterVec
	errors      *prometheus.CounterVec
	tokens      *prometheus.CounterVec
	mintSeconds *prometheus.HistogramVec
	validations *prometheus.CounterVec
}

func newMetrics(reg prometheus.Registerer) (*metrics, error) {
	m := &metrics{
		resolutions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patrol_repository_resolutions_total",
			Help: "Repository access resolutions by purpose, matched source (project, global, template, anonymous) and credential method.",
		}, []string{"purpose", "source", "method"}),
		errors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patrol_repository_resolution_errors_total",
			Help: "Failed repository access resolutions by purpose and condition reason.",
		}, []string{"purpose", "reason"}),
		tokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patrol_repository_token_requests_total",
			Help: "Short-lived repository token requests by kind and result (hit, minted, error).",
		}, []string{"kind", "result"}),
		mintSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "patrol_repository_token_mint_duration_seconds",
			Help:    "Duration of short-lived repository token mints by kind.",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
		}, []string{"kind"}),
		validations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patrol_repository_validations_total",
			Help: "Repository connection checks by result (Successful, Failed).",
		}, []string{"result"}),
	}
	if reg == nil {
		return m, nil
	}
	for _, c := range []prometheus.Collector{m.resolutions, m.errors, m.tokens, m.mintSeconds, m.validations} {
		if err := reg.Register(c); err != nil {
			var are prometheus.AlreadyRegisteredError
			if errors.As(err, &are) {
				return nil, fmt.Errorf("repository metrics already registered: %w", err)
			}
			return nil, fmt.Errorf("register repository metrics: %w", err)
		}
	}
	return m, nil
}

func (m *metrics) resolution(purpose, source, method string) {
	m.resolutions.WithLabelValues(purpose, source, method).Inc()
}

func (m *metrics) resolutionError(purpose string, err error) {
	m.errors.WithLabelValues(purpose, ConditionReason(err)).Inc()
}

func (m *metrics) tokenRequest(kind, result string) { m.tokens.WithLabelValues(kind, result).Inc() }

func (m *metrics) observeMint(kind string, d time.Duration) {
	m.mintSeconds.WithLabelValues(kind).Observe(d.Seconds())
}

func (m *metrics) validation(result string) { m.validations.WithLabelValues(result).Inc() }
