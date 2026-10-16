package reposerver

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// metrics are the repo-server Prometheus metrics. Labels never carry repository URLs
// or other unbounded values.
type metrics struct {
	requests        *prometheus.CounterVec
	requestDuration *prometheus.HistogramVec
	renderDuration  *prometheus.HistogramVec
	gitOps          *prometheus.CounterVec
	gitOpDuration   *prometheus.HistogramVec
	cacheRequests   *prometheus.CounterVec
	parallelismWait prometheus.Histogram
	inflight        prometheus.Gauge
	workingCopies   prometheus.GaugeFunc
	manifestEntries prometheus.GaugeFunc
	manifestBytes   prometheus.GaugeFunc
}

func newMetrics(reg prometheus.Registerer, s *Service) (*metrics, error) {
	m := &metrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patrol_reposerver_requests_total",
			Help: "Repo-server API requests by method and result code.",
		}, []string{"method", "code"}),
		requestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "patrol_reposerver_request_duration_seconds",
			Help:    "Repo-server API request duration by method.",
			Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300},
		}, []string{"method"}),
		renderDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "patrol_reposerver_render_duration_seconds",
			Help:    "Manifest rendering duration (cache misses) by source type and outcome.",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120},
		}, []string{"source_type", "outcome"}),
		gitOps: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patrol_reposerver_git_requests_total",
			Help: "Git operations by kind (ls-remote: remote ref listings, possibly served by the ref cache; fetch; checkout) and outcome.",
		}, []string{"operation", "outcome"}),
		gitOpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "patrol_reposerver_git_request_duration_seconds",
			Help:    "Git operation duration by kind.",
			Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120},
		}, []string{"operation"}),
		cacheRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patrol_reposerver_cache_requests_total",
			Help: "Repo-server cache lookups by cache and result (hit, miss).",
		}, []string{"cache", "result"}),
		parallelismWait: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "patrol_reposerver_parallelism_wait_seconds",
			Help:    "Time spent waiting for the manifest generation parallelism limit.",
			Buckets: []float64{0.001, 0.01, 0.1, 0.5, 1, 5, 10, 30, 60},
		}),
		inflight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "patrol_reposerver_inflight_requests",
			Help: "Repo-server API requests in progress.",
		}),
	}
	m.workingCopies = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "patrol_reposerver_working_copies",
		Help: "Persistent git working copies on disk.",
	}, func() float64 { return float64(s.workdirs.count()) })
	m.manifestEntries = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "patrol_reposerver_manifest_cache_entries",
		Help: "Entries in the manifest cache.",
	}, func() float64 { n, _ := s.manifests.stats(); return float64(n) })
	m.manifestBytes = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "patrol_reposerver_manifest_cache_bytes",
		Help: "Estimated size of the manifest cache.",
	}, func() float64 { _, b := s.manifests.stats(); return float64(b) })
	if reg == nil {
		return m, nil
	}
	for _, c := range []prometheus.Collector{
		m.requests, m.requestDuration, m.renderDuration, m.gitOps, m.gitOpDuration, m.cacheRequests,
		m.parallelismWait, m.inflight, m.workingCopies, m.manifestEntries, m.manifestBytes,
	} {
		if err := reg.Register(c); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func (m *metrics) cache(name string, hit bool) {
	result := "miss"
	if hit {
		result = "hit"
	}
	m.cacheRequests.WithLabelValues(name, result).Inc()
}

// gitOp records one git operation.
func (m *metrics) gitOp(op string, start time.Time, err error) {
	outcome := "success"
	if err != nil {
		outcome = "failure"
	}
	m.gitOps.WithLabelValues(op, outcome).Inc()
	m.gitOpDuration.WithLabelValues(op).Observe(time.Since(start).Seconds())
}

func (m *metrics) render(sourceType string, start time.Time, err error) {
	outcome := "success"
	if err != nil {
		outcome = "failure"
	}
	if sourceType == "" {
		sourceType = "unknown"
	}
	m.renderDuration.WithLabelValues(sourceType, outcome).Observe(time.Since(start).Seconds())
}
