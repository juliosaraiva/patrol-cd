package settings

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
)

// NewPrometheusMetrics registers the settings metrics with reg and returns the Manager hook:
//
//	patrol_settings_reloads_total{result="published|unchanged"}  counter
//	patrol_settings_reload_duration_seconds                      histogram
//	patrol_settings_issues{severity="error|warning"}             gauge (current snapshot)
//	patrol_settings_last_published_timestamp_seconds             gauge
func NewPrometheusMetrics(reg prometheus.Registerer) (Metrics, error) {
	reloads := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "patrol_settings_reloads_total",
		Help: "Settings reloads by result.",
	}, []string{"result"})
	duration := prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "patrol_settings_reload_duration_seconds",
		Help:    "Time spent parsing the settings objects.",
		Buckets: prometheus.ExponentialBuckets(0.0005, 2, 14),
	})
	issues := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "patrol_settings_issues",
		Help: "Issues of the current settings snapshot by severity.",
	}, []string{"severity"})
	published := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "patrol_settings_last_published_timestamp_seconds",
		Help: "Unix time at which the current settings snapshot was published.",
	})
	for _, c := range []prometheus.Collector{reloads, duration, issues, published} {
		if err := reg.Register(c); err != nil {
			return Metrics{}, fmt.Errorf("register settings metrics: %w", err)
		}
	}
	return Metrics{Reloaded: func(r ReloadResult) {
		result := "unchanged"
		if r.Published {
			result = "published"
			issues.WithLabelValues(string(SeverityError)).Set(float64(r.Errors))
			issues.WithLabelValues(string(SeverityWarning)).Set(float64(r.Warnings))
			published.SetToCurrentTime()
		}
		reloads.WithLabelValues(result).Inc()
		duration.Observe(r.Duration.Seconds())
	}}, nil
}
