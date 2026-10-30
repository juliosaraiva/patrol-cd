package integration

import (
	"fmt"
	"slices"
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
// Application of a project without monitoring targeting the same namespace gets neither. The
// gauge has one series per Application, under its current project, and none while its spec is
// invalid.
func TestOrphanedResourcesRefresh(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctl := e.startController()
	proj := permissiveProject(e.ns, "orphans")
	proj.Spec.OrphanedResources = &v1alpha1.OrphanedResourcesMonitorSettings{Warn: ptr.To(true)}
	e.create(proj)
	other := permissiveProject(e.ns, "orphans2")
	other.Spec.OrphanedResources = &v1alpha1.OrphanedResourcesMonitorSettings{}
	e.create(other)

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

	// A project change moves the series, a project that does not exist (a spec error: no
	// comparison) removes it, and a monitoring project brings it back.
	for _, step := range []struct {
		project string
		want    []string
	}{
		{"orphans2", []string{"orphans2"}},
		{"absent", nil},
		{"orphans", []string{"orphans"}},
	} {
		e.updateApp(mon, func(a *v1alpha1.Application) { a.Spec.Project = step.project })
		assertGaugeProjects(t, ctl.registry, e.ns, mon.Name, step.want)
	}

	// An object tracked by an Application that does not exist is an orphan, unlike the objects
	// of plain. Its event is attributed to the missing Application, so nothing refreshes the
	// monitors (as upstream), and the live-state cache sees it asynchronously: refresh until a
	// comparison has it. The second Application is in a namespace this cache does not hold.
	for i, instance := range []string{"gone-app", "elsewhere_gone-app"} {
		name := fmt.Sprintf("ghost-%d", i)
		ghost := configMap(name, nil, map[string]string{v1alpha1.AnnotationKeyTrackingID: instance + ":/ConfigMap:" + e.dest + "/" + name})
		_, err = e.dyn.Resource(gvrConfigMap).Namespace(e.dest).Create(e.ctx, ghost, metav1.CreateOptions{})
		require.NoError(t, err)
		for deadline := time.Now().Add(waitTimeout); ; time.Sleep(5 * pollEvery) {
			e.refresh(mon, v1alpha1.RefreshTypeNormal)
			if v, ok := gaugeValue(t, ctl.registry, e.ns, mon.Name); ok && v == float64(i+1) {
				break
			}
			require.True(t, time.Now().Before(deadline), "the object of %s never counted as an orphan", instance)
		}
	}

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

// assertGaugeProjects waits until the projects of an Application's orphaned-resources series
// are want (sorted; nil: no series).
func assertGaugeProjects(t *testing.T, reg *prometheus.Registry, namespace, name string, want []string) {
	t.Helper()
	var last []string
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		last = gaugeProjects(t, reg, namespace, name)
		if slices.Equal(last, want) {
			return
		}
		time.Sleep(pollEvery)
	}
	t.Fatalf("%s{namespace=%q,name=%q}: want projects %v, got %v", orphanGauge, namespace, name, want, last)
}

// gaugeProjects returns the sorted project labels of an Application's orphaned-resources
// series.
func gaugeProjects(t *testing.T, reg *prometheus.Registry, namespace, name string) []string {
	t.Helper()
	mfs, err := reg.Gather()
	require.NoError(t, err)
	var out []string
	for _, mf := range mfs {
		if mf.GetName() != orphanGauge {
			continue
		}
		for _, m := range mf.GetMetric() {
			got := map[string]string{}
			for _, l := range m.GetLabel() {
				got[l.GetName()] = l.GetValue()
			}
			if got["namespace"] == namespace && got["name"] == name {
				out = append(out, got["project"])
			}
		}
	}
	slices.Sort(out)
	return out
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
