package commitserver

import (
	"errors"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Git request types (label request_type) and commit response types (label response_type).
const (
	GitRequestTypeFetch = "fetch"
	GitRequestTypePush  = "push"

	ResponseTypeSuccess = "success"
	ResponseTypeFailure = "failure"
)

// Metrics are the commit-server metrics (upstream argocd_commitserver_* with the patrol_
// prefix). A nil *Metrics records nothing.
type Metrics struct {
	pending        *prometheus.GaugeVec
	gitRequests    *prometheus.CounterVec
	gitDuration    *prometheus.HistogramVec
	commitDuration *prometheus.HistogramVec
	commitRequests *prometheus.CounterVec
}

var durationBuckets = []float64{0.1, 0.25, .5, 1, 2, 4, 10, 20}

// NewMetrics creates the metrics and registers them with reg (nil: not registered). A
// collector already registered with reg is reused.
func NewMetrics(reg prometheus.Registerer) (*Metrics, error) {
	m := &Metrics{
		pending: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "patrol_commitserver_commit_pending_request_total",
			Help: "Number of pending commit requests.",
		}, []string{"repo"}),
		gitRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patrol_commitserver_git_request_total",
			Help: "Number of git requests performed by the commit server.",
		}, []string{"repo", "request_type"}),
		gitDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "patrol_commitserver_git_request_duration_seconds",
			Help:    "Git request duration in seconds.",
			Buckets: durationBuckets,
		}, []string{"repo", "request_type"}),
		commitDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "patrol_commitserver_commit_request_duration_seconds",
			Help:    "Commit request duration in seconds.",
			Buckets: durationBuckets,
		}, []string{"repo", "response_type"}),
		commitRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patrol_commitserver_commit_request_total",
			Help: "Number of commit requests handled.",
		}, []string{"repo", "response_type"}),
	}
	if reg == nil {
		return m, nil
	}
	var err error
	if m.pending, err = register(reg, m.pending); err != nil {
		return nil, err
	}
	if m.gitRequests, err = register(reg, m.gitRequests); err != nil {
		return nil, err
	}
	if m.gitDuration, err = register(reg, m.gitDuration); err != nil {
		return nil, err
	}
	if m.commitDuration, err = register(reg, m.commitDuration); err != nil {
		return nil, err
	}
	if m.commitRequests, err = register(reg, m.commitRequests); err != nil {
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
		return zero, fmt.Errorf("register commit-server metrics: %w", err)
	}
	return c, nil
}

func (m *Metrics) incPending(repo string) {
	if m != nil {
		m.pending.WithLabelValues(repo).Inc()
	}
}

func (m *Metrics) decPending(repo string) {
	if m != nil {
		m.pending.WithLabelValues(repo).Dec()
	}
}

func (m *Metrics) observeGit(repo, requestType string, d time.Duration) {
	if m != nil {
		m.gitRequests.WithLabelValues(repo, requestType).Inc()
		m.gitDuration.WithLabelValues(repo, requestType).Observe(d.Seconds())
	}
}

func (m *Metrics) observeCommit(repo, responseType string, d time.Duration) {
	if m != nil {
		m.commitRequests.WithLabelValues(repo, responseType).Inc()
		m.commitDuration.WithLabelValues(repo, responseType).Observe(d.Seconds())
	}
}
