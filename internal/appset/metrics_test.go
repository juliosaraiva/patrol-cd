package appset

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

func TestMetrics(t *testing.T) {
	t.Parallel()
	as := listAppSet(t, tenantNS, "set", elem("a", "dev"))
	as.Labels = map[string]string{"team": "payments", "app.kubernetes.io/part-of": "shop"}
	ignored := listAppSet(t, "other", "ignored", elem("b", "dev"))
	c := newFakeClient(t, as, ignored, appProject("default", func(p *v1alpha1.AppProject) { p.Spec.SourceNamespaces = []string{"*"} }))
	reg := prometheus.NewPedanticRegistry()
	r, _ := newTestReconciler(t, c, func(cfg *Config) {
		cfg.Registry = reg
		cfg.MetricsApplicationSetLabels = []string{"team", "app.kubernetes.io/part-of", "app_kubernetes_io/part-of"}
	})
	reconcileOnce(t, r, as)

	expected := `
# HELP patrol_appset_info Information about ApplicationSets (value 1).
# TYPE patrol_appset_info gauge
patrol_appset_info{name="set",namespace="team-a",resource_update_status="ApplicationSetUpToDate"} 1
# HELP patrol_appset_labels ApplicationSet labels selected by configuration (value 1).
# TYPE patrol_appset_labels gauge
patrol_appset_labels{label_app_kubernetes_io_part_of="shop",label_team="payments",name="set",namespace="team-a"} 1
# HELP patrol_appset_owned_applications Number of Applications listed in the ApplicationSet status.
# TYPE patrol_appset_owned_applications gauge
patrol_appset_owned_applications{name="set",namespace="team-a"} 1
`
	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(expected),
		MetricAppSetInfo, MetricAppSetLabels, MetricAppSetOwnedApplications))
	n, err := testutil.GatherAndCount(reg, MetricAppSetReconcile)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	// The reconcile series of a deleted ApplicationSet is dropped.
	r.metrics.forget(tenantNS, "set")
	n, err = testutil.GatherAndCount(reg, MetricAppSetReconcile)
	require.NoError(t, err)
	assert.Equal(t, 0, n)

	// Without configured labels the labels metric is not exported.
	reg2 := prometheus.NewPedanticRegistry()
	_, err = newMetrics(reg2, c, r.cfg.namespaces, nil)
	require.NoError(t, err)
	n, err = testutil.GatherAndCount(reg2, MetricAppSetLabels)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}
