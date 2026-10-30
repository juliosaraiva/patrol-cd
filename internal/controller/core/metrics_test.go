package core

import (
	"strconv"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOrphanedResourcesMetric(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	m, err := NewMetrics(reg)
	require.NoError(t, err)
	series := func() map[string]float64 { return orphanedSeries(t, reg) }

	m.SetOrphanedResources("patrol", "a", "watch", true, 2)
	m.SetOrphanedResources("patrol", "b", "watch", true, 0)
	assert.Equal(t, map[string]float64{"patrol/a/watch": 2, "patrol/b/watch": 0}, series(), "monitored Applications have a series, even at zero")

	m.SetOrphanedResources("patrol", "b", "watch", false, 0)
	assert.Equal(t, map[string]float64{"patrol/a/watch": 2}, series(), "no series once the project stops monitoring")
	m.SetOrphanedResources("patrol", "c", "plain", false, 0)
	assert.Equal(t, map[string]float64{"patrol/a/watch": 2}, series(), "never emitted for Applications whose project does not monitor")

	// A project change moves the series: one series per Application.
	m.SetOrphanedResources("patrol", "a", "watch2", true, 3)
	assert.Equal(t, map[string]float64{"patrol/a/watch2": 3}, series(), "moved to another monitoring project")
	m.SetOrphanedResources("patrol", "a", "plain", false, 3)
	assert.Equal(t, map[string]float64{}, series(), "moved to a project that does not monitor")
	m.SetOrphanedResources("patrol", "a", "watch", true, 1)
	m.SetOrphanedResources("team", "a", "watch", true, 4)
	assert.Equal(t, map[string]float64{"patrol/a/watch": 1, "team/a/watch": 4}, series(), "Applications of other namespaces are apart")

	m.ForgetApplication("patrol", "a")
	assert.Equal(t, map[string]float64{"team/a/watch": 4}, series(), "a forgotten Application's series is removed")
	m.ForgetApplication("patrol", "never")
	assert.Equal(t, map[string]float64{"team/a/watch": 4}, series(), "forgetting an Application without a series")

	var none *Metrics
	assert.NotPanics(t, func() {
		none.SetOrphanedResources("patrol", "a", "watch", true, 1)
		none.ForgetApplication("patrol", "a")
	})
}

// Concurrent updates of one Application (comparisons, tree-only refreshes and Reconcile run on
// different goroutines) leave one series, under the last project set (-race checks the rest).
func TestOrphanedResourcesMetricConcurrent(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	m, err := NewMetrics(reg)
	require.NoError(t, err)
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			for i := range 200 {
				switch (g + i) % 3 {
				case 0:
					m.SetOrphanedResources("patrol", "a", "p"+strconv.Itoa(i%4), true, i)
				case 1:
					m.SetOrphanedResources("patrol", "a", "p"+strconv.Itoa(i%4), false, 0)
				default:
					m.ForgetApplication("patrol", "a")
				}
			}
		})
	}
	wg.Wait()
	m.SetOrphanedResources("patrol", "a", "last", true, 7)
	assert.Equal(t, map[string]float64{"patrol/a/last": 7}, orphanedSeries(t, reg))
}

// orphanedSeries returns the orphaned-resources gauge as namespace/name/project -> value.
func orphanedSeries(t *testing.T, reg *prometheus.Registry) map[string]float64 {
	t.Helper()
	mfs, err := reg.Gather()
	require.NoError(t, err)
	out := map[string]float64{}
	for _, mf := range mfs {
		if mf.GetName() != "patrol_app_orphaned_resources_count" {
			continue
		}
		for _, m := range mf.GetMetric() {
			l := map[string]string{}
			for _, p := range m.GetLabel() {
				l[p.GetName()] = p.GetValue()
			}
			out[l["namespace"]+"/"+l["name"]+"/"+l["project"]] = m.GetGauge().GetValue()
		}
	}
	return out
}
