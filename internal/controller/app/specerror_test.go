package app

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clocktesting "k8s.io/utils/clock/testing"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/compare"
	"github.com/juliosaraiva/patrol-cd/internal/controller/core"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

// orphanComparer is a Comparer and TreeBuilder that counts two orphaned resources in a
// comparison and three in a tree build; it counts both. With noTree its comparisons build no
// tree (Result.Tree is nil, as when the live-state cache is unavailable).
type orphanComparer struct {
	compares, trees int
	noTree          bool
}

func (o *orphanComparer) Compare(_ context.Context, in compare.Input) (*compare.Result, error) {
	o.compares++
	res := &compare.Result{
		SyncStatus: v1alpha1.SyncStatus{Status: v1alpha1.SyncStatusCodeSynced, Revision: "abc",
			ComparedTo: v1alpha1.ComparedTo{Source: in.App.Spec.Source.DeepCopy(), Destination: in.App.Spec.Destination}},
		Health:       v1alpha1.HealthStatusHealthy,
		ReconciledAt: metav1.NewTime(now),
	}
	if !o.noTree {
		res.Tree = &compare.ApplicationTree{Nodes: []compare.ResourceNode{{ResourceRef: compare.ResourceRef{Version: "v1", Kind: "ConfigMap", Namespace: "web", Name: "cm"}}}}
		res.OrphanedResources = 2
	}
	return res, nil
}

func (o *orphanComparer) ResourceTree(context.Context, compare.TreeInput) (*compare.TreeResult, error) {
	o.trees++
	return &compare.TreeResult{Tree: &compare.ApplicationTree{}, OrphanedResources: 3,
		Conditions:              []compare.Condition{{Type: v1alpha1.ApplicationConditionOrphanedResourceWarning, Message: "Application has 3 orphaned resources"}},
		EvaluatedConditionTypes: []string{v1alpha1.ApplicationConditionOrphanedResourceWarning}}, nil
}

// orphanEnv is a Reconciler with an orphanComparer over a fake client holding the Application
// patrol/a of project p.
type orphanEnv struct {
	t   *testing.T
	c   client.Client
	r   *Reconciler
	reg *prometheus.Registry
	m   *core.Metrics
	cmp *orphanComparer
	app *v1alpha1.Application
	key types.NamespacedName
}

func newOrphanEnv(t *testing.T, monitor bool) *orphanEnv {
	t.Helper()
	sch := runtime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(sch))
	proj := &v1alpha1.AppProject{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "patrol"},
		Spec: v1alpha1.AppProjectSpec{SourceRepos: []string{"*"}, Destinations: []v1alpha1.ApplicationDestination{{Server: "*", Namespace: "*"}}}}
	if monitor {
		proj.Spec.OrphanedResources = &v1alpha1.OrphanedResourcesMonitorSettings{Warn: ptr.To(true)}
	}
	cluster := &v1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "k8s", Namespace: "patrol"}, Spec: v1alpha1.ClusterSpec{Server: testServer, Name: "k8s"}}
	app := &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "patrol", UID: "u-a"},
		Spec: v1alpha1.ApplicationSpec{Project: "p",
			Source:      &v1alpha1.ApplicationSource{RepoURL: "https://git.example/repo.git", Path: ".", TargetRevision: "HEAD"},
			Destination: v1alpha1.ApplicationDestination{Server: testServer, Namespace: "web"}}}
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(proj, cluster, app.DeepCopy()).
		WithStatusSubresource(&v1alpha1.Application{}).
		WithIndex(&v1alpha1.Application{}, indexDestinationNamespace, destinationNamespaceKeys).
		WithIndex(&v1alpha1.AppProject{}, indexOrphanedResources, orphanMonitoringKeys).
		Build()
	st := settings.Parse(settings.Input{Namespace: "patrol", ConfigMaps: []*corev1.ConfigMap{{
		ObjectMeta: metav1.ObjectMeta{Name: "patrol-cm", Namespace: "patrol", Labels: map[string]string{v1alpha1.LabelKeyPartOf: v1alpha1.LabelValuePartOf}},
	}}})
	reg := prometheus.NewRegistry()
	m, err := core.NewMetrics(reg)
	require.NoError(t, err)
	cmp := &orphanComparer{}
	clk := clocktesting.NewFakeClock(now)
	r := &Reconciler{
		cfg: Config{Deps: core.Deps{Client: c, APIReader: c, Namespace: "patrol", Settings: compare.StaticSettings{Settings: st},
			Comparer: cmp, Metrics: m, Clock: clk, Recorder: core.NopRecorder{}, Logger: slog.New(slog.DiscardHandler)},
			Jitter: func(time.Duration) time.Duration { return 0 }},
		c:       c,
		log:     slog.New(slog.DiscardHandler),
		refresh: newRefreshQueue(clk),
		state:   newStateStore(),
	}
	return &orphanEnv{t: t, c: c, r: r, reg: reg, m: m, cmp: cmp, app: app, key: client.ObjectKeyFromObject(app)}
}

func (e *orphanEnv) reconcile() {
	e.t.Helper()
	_, err := e.r.Reconcile(context.Background(), reconcile.Request{NamespacedName: e.key})
	require.NoError(e.t, err)
}

// series returns the number of series of the orphaned-resources gauge.
func (e *orphanEnv) series() int {
	e.t.Helper()
	n, err := testutil.GatherAndCount(e.reg, "patrol_app_orphaned_resources_count")
	require.NoError(e.t, err)
	return n
}

// value returns the gauge of the Application, which must have its one series.
func (e *orphanEnv) value() float64 {
	e.t.Helper()
	require.Equal(e.t, 1, e.series())
	return testutil.ToFloat64(e.m.OrphanedResources.WithLabelValues("patrol", "a", "p"))
}

func (e *orphanEnv) conditions() []string {
	e.t.Helper()
	got := &v1alpha1.Application{}
	require.NoError(e.t, e.c.Get(context.Background(), e.key, got))
	var out []string
	for _, cd := range got.Status.Conditions {
		out = append(out, cd.Type)
	}
	return out
}

// setRepos sets the project's sourceRepos and refreshes the Application.
func (e *orphanEnv) setRepos(repos ...string) {
	e.t.Helper()
	p := &v1alpha1.AppProject{}
	require.NoError(e.t, e.c.Get(context.Background(), client.ObjectKey{Namespace: "patrol", Name: "p"}, p))
	p.Spec.SourceRepos = repos
	require.NoError(e.t, e.c.Update(context.Background(), p))
	e.r.refresh.RequestRefresh(e.key, core.CompareLatest, 0)
	e.reconcile()
}

// A refresh that finds spec errors stores an empty tree and no managed resources, as upstream,
// and removes the orphaned-resources series; a later tree-only refresh (an orphan event)
// compares again instead of rebuilding the tree from the stale comparison, so neither the
// series nor OrphanedResourceWarning comes back while the spec stays invalid.
func TestSpecErrorClearsTreeState(t *testing.T) {
	t.Parallel()
	e := newOrphanEnv(t, true)
	e.reconcile()
	require.Equal(t, 1, e.cmp.compares)
	require.Equal(t, 1, e.series(), "a monitored Application has a series")

	// The project stops permitting the repository: the spec is invalid.
	e.setRepos("https://other.example/*")
	assert.Equal(t, 1, e.cmp.compares, "an invalid spec is not compared")
	assert.Equal(t, 0, e.series(), "no series while the spec is invalid")
	assert.Contains(t, e.conditions(), v1alpha1.ApplicationConditionInvalidSpecError)
	state, ok := e.r.State(e.key)
	require.True(t, ok)
	assert.True(t, state.SpecInvalid)
	assert.Equal(t, &compare.ApplicationTree{}, state.Tree, "an empty tree, as upstream")
	assert.Nil(t, state.Managed)

	// An unmanaged object appears in the destination namespace: its tree-only refresh compares
	// again (the spec is still invalid) instead of rebuilding the tree.
	e.r.onResourceUpdated(testServer, liveRes("", "ConfigMap", "web", "stray", nil), nil, nsResources())
	assert.Equal(t, map[types.NamespacedName]requested{e.key: treeReq}, pendingRequests(e.r.refresh))
	e.reconcile()
	assert.Equal(t, map[types.NamespacedName]requested{e.key: recentReq}, pendingRequests(e.r.refresh), "a comparison instead")
	e.reconcile()
	assert.Zero(t, e.cmp.trees, "no tree built from the stale comparison")
	assert.Equal(t, 1, e.cmp.compares)
	assert.Equal(t, 0, e.series(), "still no series")
	assert.NotContains(t, e.conditions(), v1alpha1.ApplicationConditionOrphanedResourceWarning)

	// The project permits the repository again: compared, the series is back.
	e.setRepos("*")
	assert.Equal(t, 2, e.cmp.compares)
	assert.Equal(t, 1, e.series())
	state, _ = e.r.State(e.key)
	assert.False(t, state.SpecInvalid)
}

// A tree-only refresh records the orphaned resources of its tree build, only for an
// Application whose project monitors them.
func TestTreeRefreshOrphanedSeries(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		monitor bool
	}{{"monitoring project", true}, {"project without monitoring", false}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newOrphanEnv(t, tc.monitor)
			e.reconcile()
			e.r.refresh.RequestTreeRefresh(e.key)
			e.reconcile()
			assert.Equal(t, 1, e.cmp.compares)
			require.Equal(t, 1, e.cmp.trees)
			if tc.monitor {
				assert.InDelta(t, 3, e.value(), 0)
			} else {
				assert.Equal(t, 0, e.series())
			}
		})
	}
}

// A comparison that built no tree counted no orphaned resources: the gauge keeps its value.
func TestComparisonWithoutTreeKeepsOrphanedSeries(t *testing.T) {
	t.Parallel()
	e := newOrphanEnv(t, true)
	e.reconcile()
	require.InDelta(t, 2, e.value(), 0)
	e.cmp.noTree = true
	e.r.refresh.RequestRefresh(e.key, core.CompareLatest, 0)
	e.reconcile()
	require.Equal(t, 2, e.cmp.compares)
	assert.InDelta(t, 2, e.value(), 0)
}

// staleGet returns stale for every Application read: an informer cache behind the API server.
type staleGet struct {
	client.Client
	stale *v1alpha1.Application
}

func (s staleGet) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if a, ok := obj.(*v1alpha1.Application); ok {
		s.stale.DeepCopyInto(a)
		return nil
	}
	return s.Client.Get(ctx, key, obj, opts...)
}

// A cached Application behind the API server looks as if it needs a comparison; when the read
// from the API server finds it does not, a pending tree-only refresh still runs.
func TestTreeRefreshRunsAfterStaleRead(t *testing.T) {
	t.Parallel()
	e := newOrphanEnv(t, true)
	e.reconcile()
	require.Equal(t, 1, e.cmp.compares)
	e.r.c = staleGet{Client: e.c, stale: e.app}
	e.r.refresh.RequestTreeRefresh(e.key)
	e.reconcile()
	assert.Equal(t, 1, e.cmp.compares, "nothing to compare")
	assert.Equal(t, 1, e.cmp.trees, "the tree-only refresh ran")
	assert.InDelta(t, 3, e.value(), 0)
}
