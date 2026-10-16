package appset

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// Metric names.
const (
	MetricAppSetInfo              = "patrol_appset_info"
	MetricAppSetOwnedApplications = "patrol_appset_owned_applications"
	MetricAppSetReconcile         = "patrol_appset_reconcile"
	MetricAppSetLabels            = "patrol_appset_labels"
)

// metricsListTimeout bounds the ApplicationSet listing done at scrape time.
const metricsListTimeout = 10 * time.Second

// metrics are the controller metrics. A nil *metrics records nothing.
type metrics struct {
	reconcile *prometheus.HistogramVec
}

// newMetrics registers the reconcile histogram and a collector that reports the info,
// owned-applications and labels metrics of every ApplicationSet in an allowed namespace at
// scrape time (from the cache, so deleted ApplicationSets leave no stale series).
func newMetrics(reg prometheus.Registerer, reader client.Reader, namespaces *NamespaceMatcher, labelKeys []string) (*metrics, error) {
	if reg == nil {
		return nil, nil
	}
	m := &metrics{
		reconcile: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    MetricAppSetReconcile,
			Help:    "ApplicationSet reconcile duration in seconds.",
			Buckets: prometheus.DefBuckets,
		}, []string{"namespace", "name"}),
	}
	if err := reg.Register(m.reconcile); err != nil {
		return nil, fmt.Errorf("registering %s: %w", MetricAppSetReconcile, err)
	}
	if err := reg.Register(newAppSetCollector(reader, namespaces, labelKeys)); err != nil {
		return nil, fmt.Errorf("registering ApplicationSet collector: %w", err)
	}
	return m, nil
}

func (m *metrics) observeReconcile(appSet *v1alpha1.ApplicationSet, d time.Duration) {
	if m == nil {
		return
	}
	m.reconcile.WithLabelValues(appSet.Namespace, appSet.Name).Observe(d.Seconds())
}

// forget drops the reconcile series of a deleted ApplicationSet.
func (m *metrics) forget(namespace, name string) {
	if m == nil {
		return
	}
	m.reconcile.DeleteLabelValues(namespace, name)
}

// appSetCollector reports per-ApplicationSet gauges at scrape time.
type appSetCollector struct {
	reader     client.Reader
	namespaces *NamespaceMatcher
	labelKeys  []string
	info       *prometheus.Desc
	owned      *prometheus.Desc
	labels     *prometheus.Desc
}

var invalidLabelChars = regexp.MustCompile(`[^a-zA-Z0-9_]`)

// normalizeLabel turns an ApplicationSet label key into a Prometheus label name: "label_" plus
// the key with every character outside [a-zA-Z0-9_] replaced by '_'.
func normalizeLabel(key string) string {
	return "label_" + invalidLabelChars.ReplaceAllString(key, "_")
}

func newAppSetCollector(reader client.Reader, namespaces *NamespaceMatcher, labelKeys []string) *appSetCollector {
	// Keys normalizing to the same label name are reported once (first wins).
	seen := map[string]bool{}
	var keys []string
	for _, k := range labelKeys {
		if n := normalizeLabel(k); !seen[n] {
			seen[n] = true
			keys = append(keys, k)
		}
	}
	c := &appSetCollector{
		reader:     reader,
		namespaces: namespaces,
		labelKeys:  keys,
		info: prometheus.NewDesc(MetricAppSetInfo, "Information about ApplicationSets (value 1).",
			[]string{"namespace", "name", "resource_update_status"}, nil),
		owned: prometheus.NewDesc(MetricAppSetOwnedApplications, "Number of Applications listed in the ApplicationSet status.",
			[]string{"namespace", "name"}, nil),
	}
	if len(keys) > 0 {
		names := []string{"namespace", "name"}
		for _, k := range keys {
			names = append(names, normalizeLabel(k))
		}
		c.labels = prometheus.NewDesc(MetricAppSetLabels, "ApplicationSet labels selected by configuration (value 1).", names, nil)
	}
	return c
}

// Describe implements prometheus.Collector.
func (c *appSetCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.info
	ch <- c.owned
	if c.labels != nil {
		ch <- c.labels
	}
}

// Collect implements prometheus.Collector.
func (c *appSetCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), metricsListTimeout)
	defer cancel()
	var list v1alpha1.ApplicationSetList
	if err := c.reader.List(ctx, &list); err != nil {
		ch <- prometheus.NewInvalidMetric(c.info, fmt.Errorf("listing ApplicationSets: %w", err))
		return
	}
	for i := range list.Items {
		as := &list.Items[i]
		if !c.namespaces.Allowed(as.Namespace) {
			continue
		}
		status := "Unknown"
		for _, cond := range as.Status.Conditions {
			if cond.Type == v1alpha1.ApplicationSetConditionResourcesUpToDate {
				status = cond.Reason
			}
		}
		send(ch, c.info, 1, as.Namespace, as.Name, status)
		send(ch, c.owned, float64(len(as.Status.Resources)), as.Namespace, as.Name)
		if c.labels != nil {
			values := []string{as.Namespace, as.Name}
			for _, k := range c.labelKeys {
				values = append(values, as.Labels[k])
			}
			send(ch, c.labels, 1, values...)
		}
	}
}

// send emits a gauge sample, or an invalid metric describing why it could not be built.
func send(ch chan<- prometheus.Metric, desc *prometheus.Desc, value float64, labels ...string) {
	m, err := prometheus.NewConstMetric(desc, prometheus.GaugeValue, value, labels...)
	if err != nil {
		ch <- prometheus.NewInvalidMetric(desc, err)
		return
	}
	ch <- m
}
