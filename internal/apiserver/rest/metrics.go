package rest

import (
	"errors"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics are the API server's Prometheus metrics. A nil *Metrics records nothing.
type Metrics struct {
	requests    *prometheus.CounterVec
	duration    *prometheus.HistogramVec
	inflight    prometheus.Gauge
	logins      *prometheus.CounterVec
	streams     *prometheus.GaugeVec
	slowStreams *prometheus.CounterVec
	auditEvents *prometheus.CounterVec
	denials     *prometheus.CounterVec
	rateLimited *prometheus.CounterVec
	panics      prometheus.Counter
}

// NewMetrics creates and registers the metrics on reg.
func NewMetrics(reg prometheus.Registerer) (*Metrics, error) {
	m := &Metrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patrol_server_http_requests_total",
			Help: "API requests by operation, method and status code.",
		}, []string{"operation", "method", "code"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "patrol_server_http_request_duration_seconds",
			Help:    "API request latency by operation (streams: time until the stream ended).",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30},
		}, []string{"operation", "method"}),
		inflight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "patrol_server_http_inflight_requests",
			Help: "API requests being served.",
		}),
		logins: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patrol_server_login_requests_total",
			Help: "Local and SSO login attempts by result (success, failure).",
		}, []string{"method", "result"}),
		streams: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "patrol_server_sse_streams",
			Help: "Open Server-Sent Event streams by operation.",
		}, []string{"operation"}),
		slowStreams: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patrol_server_sse_slow_consumers_total",
			Help: "Streams closed because the client did not keep up.",
		}, []string{"operation"}),
		auditEvents: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patrol_server_audit_events_total",
			Help: "Audit entries by result.",
		}, []string{"result"}),
		denials: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patrol_server_authorization_denials_total",
			Help: "RBAC denials by operation.",
		}, []string{"operation"}),
		rateLimited: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patrol_server_rate_limited_requests_total",
			Help: "Requests rejected by the login rate limiter.",
		}, []string{"operation"}),
		panics: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "patrol_server_handler_panics_total",
			Help: "Handler panics recovered.",
		}),
	}
	if reg == nil {
		return m, nil
	}
	var errs []error
	for _, c := range []prometheus.Collector{m.requests, m.duration, m.inflight, m.logins, m.streams, m.slowStreams, m.auditEvents, m.denials, m.rateLimited, m.panics} {
		if err := reg.Register(c); err != nil {
			errs = append(errs, err)
		}
	}
	return m, errors.Join(errs...)
}

func (m *Metrics) observe(operation, method string, status int, d time.Duration) {
	if m == nil {
		return
	}
	m.requests.WithLabelValues(operation, method, strconv.Itoa(status)).Inc()
	m.duration.WithLabelValues(operation, method).Observe(d.Seconds())
}

func (m *Metrics) inflightAdd(d float64) {
	if m == nil {
		return
	}
	m.inflight.Add(d)
}

// Login records a login attempt (method "local" or "sso"; result "success" or "failure").
func (m *Metrics) Login(method, result string) {
	if m == nil {
		return
	}
	m.logins.WithLabelValues(method, result).Inc()
}

func (m *Metrics) streamOpened(op string, d float64) {
	if m == nil {
		return
	}
	m.streams.WithLabelValues(op).Add(d)
}

func (m *Metrics) slowConsumer(op string) {
	if m == nil {
		return
	}
	m.slowStreams.WithLabelValues(op).Inc()
}

func (m *Metrics) audited(r AuditResult) {
	if m == nil {
		return
	}
	m.auditEvents.WithLabelValues(string(r)).Inc()
}

func (m *Metrics) denied(op string) {
	if m == nil {
		return
	}
	m.denials.WithLabelValues(op).Inc()
}

func (m *Metrics) limited(op string) {
	if m == nil {
		return
	}
	m.rateLimited.WithLabelValues(op).Inc()
}

func (m *Metrics) panicked() {
	if m == nil {
		return
	}
	m.panics.Inc()
}
