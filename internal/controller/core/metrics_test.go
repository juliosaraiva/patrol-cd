package core

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOrphanedResourcesMetric(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	m, err := NewMetrics(reg)
	require.NoError(t, err)
	count := func() int {
		n, err := testutil.GatherAndCount(reg, "patrol_app_orphaned_resources_count")
		require.NoError(t, err)
		return n
	}

	m.SetOrphanedResources("patrol", "a", "watch", true, 2)
	m.SetOrphanedResources("patrol", "b", "watch", true, 0)
	assert.InDelta(t, 2, testutil.ToFloat64(m.OrphanedResources.WithLabelValues("patrol", "a", "watch")), 0)
	assert.Equal(t, 2, count(), "monitored Applications have a series, even at zero")

	m.SetOrphanedResources("patrol", "b", "watch", false, 0)
	assert.Equal(t, 1, count(), "no series once the project stops monitoring")
	m.SetOrphanedResources("patrol", "c", "plain", false, 0)
	assert.Equal(t, 1, count(), "never emitted for Applications whose project does not monitor")

	m.ForgetApplication("patrol", "a")
	assert.Equal(t, 0, count(), "a deleted Application's series is removed")

	var none *Metrics
	assert.NotPanics(t, func() {
		none.SetOrphanedResources("patrol", "a", "watch", true, 1)
		none.ForgetApplication("patrol", "a")
	})
}
