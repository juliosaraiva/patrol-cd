package extensions

import (
	"errors"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// metrics counts proxied requests per extension (upstream extension request counter and
// duration histogram).
type metrics struct {
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

func newMetrics(reg prometheus.Registerer, log *slog.Logger) *metrics {
	m := &metrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patrol_server_extension_requests_total",
			Help: "Proxy extension requests by extension and response status.",
		}, []string{"extension", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "patrol_server_extension_request_duration_seconds",
			Help:    "Duration of proxy extension requests.",
			Buckets: prometheus.DefBuckets,
		}, []string{"extension"}),
	}
	if reg == nil {
		return m
	}
	m.requests = register(reg, m.requests, log)
	m.duration = register(reg, m.duration, log)
	return m
}

// register registers c, reusing an identical collector registered earlier (several
// services built against one registry).
func register[C prometheus.Collector](reg prometheus.Registerer, c C, log *slog.Logger) C {
	if err := reg.Register(c); err != nil {
		var are prometheus.AlreadyRegisteredError
		if errors.As(err, &are) {
			if existing, ok := are.ExistingCollector.(C); ok {
				return existing
			}
		}
		log.Warn("proxy extension metrics not registered", slog.String("error", err.Error()))
	}
	return c
}

// observe records one request. Only registered extension names are labels, so callers
// cannot grow the label set.
func (m *metrics) observe(ext string, status int, d time.Duration) {
	if m == nil {
		return
	}
	m.requests.WithLabelValues(ext, statusText(status)).Inc()
	m.duration.WithLabelValues(ext).Observe(d.Seconds())
}
