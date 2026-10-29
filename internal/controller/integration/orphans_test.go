package integration

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

const orphanGauge = "patrol_app_orphaned_resources_count"

// TestOrphanedResourcesRefresh: in a project monitoring orphaned resources, creating an
// unmanaged ConfigMap in the destination namespace sets OrphanedResourceWarning and the
// orphaned-resources gauge through a refresh driven by the live-state event (the harness's
// periodic refresh is 1h away), and deleting it clears both (APP-085, DIFF-078, OPS-073). An
// Application of a project without monitoring targeting the same namespace gets neither.
func TestOrphanedResourcesRefresh(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctl := e.startController()
	proj := permissiveProject(e.ns, "orphans")
	proj.Spec.OrphanedResources = &v1alpha1.OrphanedResourcesMonitorSettings{Warn: ptr.To(true)}
	e.create(proj)

	monRev := e.repo.commit(e.repoURL, "orph", "main", configMap("orph-cm", nil, nil))
	plainRev := e.repo.commit(e.repoURL, "plain", "main", configMap("plain-cm", nil, nil))
	mon := e.newApp("orph", func(a *v1alpha1.Application) { a.Spec.Project = "orphans" })
	plain := e.newApp("plain")
	e.create(mon)
	e.create(plain)
	for _, x := range []struct {
		app *v1alpha1.Application
		rev string
	}{{mon, monRev}, {plain, plainRev}} {
		e.waitApp(x.app, "compared", outOfSync)
		e.waitOp(e.sync(x.app), "synced", phaseIs(v1alpha1.OperationSucceeded))
		e.waitApp(x.app, "synced", synced(x.rev))
	}
	// Let the post-operation refreshes land, then take the reference reconciledAt.
	time.Sleep(1500 * time.Millisecond)
	before := e.getApp(mon)
	require.Nil(t, hasCondition(before, v1alpha1.ApplicationConditionOrphanedResourceWarning), describeApp(before))
	assertGauge(t, ctl.registry, e.ns, mon.Name, "orphans", 0)

	_, err := e.dyn.Resource(gvrConfigMap).Namespace(e.dest).Create(e.ctx, configMap("stray", nil, nil), metav1.CreateOptions{})
	require.NoError(t, err)
	warned := e.waitApp(mon, "OrphanedResourceWarning from the unmanaged ConfigMap", func(a *v1alpha1.Application) bool {
		c := hasCondition(a, v1alpha1.ApplicationConditionOrphanedResourceWarning)
		return c != nil && c.Message == "Application has 1 orphaned resources"
	})
	assert.True(t, warned.Status.ReconciledAt.Equal(before.Status.ReconciledAt), "event-driven refresh, not a latest-level one")
	assertGauge(t, ctl.registry, e.ns, mon.Name, "orphans", 1)

	require.NoError(t, e.dyn.Resource(gvrConfigMap).Namespace(e.dest).Delete(e.ctx, "stray", metav1.DeleteOptions{}))
	e.waitApp(mon, "OrphanedResourceWarning cleared", func(a *v1alpha1.Application) bool {
		return hasCondition(a, v1alpha1.ApplicationConditionOrphanedResourceWarning) == nil
	})
	assertGauge(t, ctl.registry, e.ns, mon.Name, "orphans", 0)

	got := e.getApp(plain)
	assert.Nil(t, hasCondition(got, v1alpha1.ApplicationConditionOrphanedResourceWarning), describeApp(got))
	_, ok := gaugeValue(t, ctl.registry, e.ns, plain.Name)
	assert.False(t, ok, "no orphaned-resources series without monitoring")

	// Deleting the Application removes its series.
	require.NoError(t, e.c.Delete(e.ctx, e.getApp(mon)))
	require.Eventually(t, func() bool {
		_, ok := gaugeValue(t, ctl.registry, e.ns, mon.Name)
		return !ok
	}, waitTimeout, pollEvery, "series of the deleted Application removed")
}

// assertGauge waits until the orphaned-resources gauge of an Application has value want.
func assertGauge(t *testing.T, reg *prometheus.Registry, namespace, name, project string, want float64) {
	t.Helper()
	var last float64
	var found bool
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		last, found = gaugeValue(t, reg, namespace, name, "project", project)
		if found && last == want {
			return
		}
		time.Sleep(pollEvery)
	}
	t.Fatalf("%s{namespace=%q,name=%q,project=%q}: want %v, got %v (found %v)", orphanGauge, namespace, name, project, want, last, found)
}

// gaugeValue returns the orphaned-resources gauge of an Application (extra: more label
// name/value pairs to match).
func gaugeValue(t *testing.T, reg *prometheus.Registry, namespace, name string, extra ...string) (float64, bool) {
	t.Helper()
	want := map[string]string{"namespace": namespace, "name": name}
	for i := 0; i+1 < len(extra); i += 2 {
		want[extra[i]] = extra[i+1]
	}
	mfs, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() != orphanGauge {
			continue
		}
	metrics:
		for _, m := range mf.GetMetric() {
			got := map[string]string{}
			for _, l := range m.GetLabel() {
				got[l.GetName()] = l.GetValue()
			}
			for k, v := range want {
				if got[k] != v {
					continue metrics
				}
			}
			return m.GetGauge().GetValue(), true
		}
	}
	return 0, false
}
