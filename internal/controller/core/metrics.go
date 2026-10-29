package core

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/client-go/rest"
)

// Metrics are the controller's Prometheus collectors (upstream argocd_app_* families with the
// patrol_ prefix).
type Metrics struct {
	// SyncTotal counts completed sync operations by phase (argocd_app_sync_total).
	SyncTotal *prometheus.CounterVec
	// SyncDuration accumulates the duration of completed operations
	// (argocd_app_sync_duration_seconds_total).
	SyncDuration *prometheus.CounterVec
	// ReconcileDuration observes Application reconciliations (argocd_app_reconcile).
	ReconcileDuration *prometheus.HistogramVec
	// OperationStepDuration observes Operation reconcile steps by resulting phase.
	OperationStepDuration *prometheus.HistogramVec
	// K8sRequests counts requests to destination clusters (argocd_app_k8s_request_total).
	K8sRequests *prometheus.CounterVec
	// AutoSyncTotal counts automated sync decisions by outcome.
	AutoSyncTotal *prometheus.CounterVec
	// DeletionTotal counts cascade-deletion passes by outcome.
	DeletionTotal *prometheus.CounterVec
	// OrphanedResources is the number of orphaned resources per Application whose project
	// monitors them (argocd_app_orphaned_resources_count).
	OrphanedResources *prometheus.GaugeVec
}

// NewMetrics creates the collectors and registers them with reg (nil: not registered).
func NewMetrics(reg prometheus.Registerer) (*Metrics, error) {
	m := &Metrics{
		SyncTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patrol_app_sync_total",
			Help: "Number of completed application sync operations.",
		}, []string{"namespace", "name", "project", "dest_server", "phase", "dry_run"}),
		SyncDuration: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patrol_app_sync_duration_seconds_total",
			Help: "Total duration of completed application sync operations, in seconds.",
		}, []string{"namespace", "name", "project", "dest_server"}),
		ReconcileDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "patrol_app_reconcile_duration_seconds",
			Help:    "Duration of Application reconciliations, in seconds.",
			Buckets: []float64{0.25, 0.5, 1, 2, 4, 8, 16, 32},
		}, []string{"namespace", "dest_server"}),
		OperationStepDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "patrol_app_operation_step_duration_seconds",
			Help:    "Duration of Operation reconcile steps by resulting phase, in seconds.",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 4, 8, 16, 32},
		}, []string{"phase"}),
		K8sRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patrol_app_k8s_request_total",
			Help: "Number of Kubernetes API requests sent to destination clusters by the controller.",
		}, []string{"server", "verb", "response_code"}),
		AutoSyncTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patrol_app_auto_sync_total",
			Help: "Automated sync decisions by outcome.",
		}, []string{"outcome"}),
		DeletionTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patrol_app_deletion_passes_total",
			Help: "Application deletion (finalizer) passes by outcome.",
		}, []string{"outcome"}),
		OrphanedResources: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "patrol_app_orphaned_resources_count",
			Help: "Number of orphaned resources per Application.",
		}, []string{"namespace", "name", "project"}),
	}
	if reg == nil {
		return m, nil
	}
	var errs []error
	for _, c := range []prometheus.Collector{m.SyncTotal, m.SyncDuration, m.ReconcileDuration, m.OperationStepDuration, m.K8sRequests, m.AutoSyncTotal, m.DeletionTotal, m.OrphanedResources} {
		if err := reg.Register(c); err != nil {
			var are prometheus.AlreadyRegisteredError
			if errors.As(err, &are) {
				continue
			}
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("register controller metrics: %w", err)
	}
	return m, nil
}

// ObserveSync records a completed operation.
func (m *Metrics) ObserveSync(namespace, name, project, server, phase string, dryRun bool, d time.Duration) {
	if m == nil {
		return
	}
	m.SyncTotal.WithLabelValues(namespace, name, project, server, phase, strconv.FormatBool(dryRun)).Inc()
	if d > 0 {
		m.SyncDuration.WithLabelValues(namespace, name, project, server).Add(d.Seconds())
	}
}

// SetOrphanedResources records the orphaned resources found by an Application's latest tree
// build. The series exists only while the project monitors orphaned resources (monitored).
func (m *Metrics) SetOrphanedResources(namespace, name, project string, monitored bool, n int) {
	if m == nil {
		return
	}
	if !monitored {
		m.OrphanedResources.DeleteLabelValues(namespace, name, project)
		return
	}
	m.OrphanedResources.WithLabelValues(namespace, name, project).Set(float64(n))
}

// ForgetApplication removes the per-Application series of a deleted Application.
func (m *Metrics) ForgetApplication(namespace, name string) {
	if m == nil {
		return
	}
	m.OrphanedResources.DeletePartialMatch(prometheus.Labels{"namespace": namespace, "name": name})
}

// WrapConfig returns a copy of cfg whose requests are counted in K8sRequests under server.
func (m *Metrics) WrapConfig(cfg *rest.Config, server string) *rest.Config {
	out := rest.CopyConfig(cfg)
	if m == nil {
		return out
	}
	out.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		return &countingTransport{next: rt, server: server, counter: m.K8sRequests}
	})
	return out
}

type countingTransport struct {
	next    http.RoundTripper
	server  string
	counter *prometheus.CounterVec
}

func (t *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	code := "error"
	if err == nil && resp != nil {
		code = strconv.Itoa(resp.StatusCode)
	}
	t.counter.WithLabelValues(t.server, strings.ToLower(req.Method), code).Inc()
	return resp, err
}
