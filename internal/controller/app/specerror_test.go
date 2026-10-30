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

// orphanComparer is a Comparer and TreeBuilder whose results always count two orphaned
// resources; it counts its comparisons and tree builds.
type orphanComparer struct {
	compares, trees int
}

func (o *orphanComparer) Compare(_ context.Context, in compare.Input) (*compare.Result, error) {
	o.compares++
	return &compare.Result{
		SyncStatus: v1alpha1.SyncStatus{Status: v1alpha1.SyncStatusCodeSynced, Revision: "abc",
			ComparedTo: v1alpha1.ComparedTo{Source: in.App.Spec.Source.DeepCopy(), Destination: in.App.Spec.Destination}},
		Health:            v1alpha1.HealthStatusHealthy,
		Tree:              &compare.ApplicationTree{Nodes: []compare.ResourceNode{{ResourceRef: compare.ResourceRef{Version: "v1", Kind: "ConfigMap", Namespace: "web", Name: "cm"}}}},
		OrphanedResources: 2,
		ReconciledAt:      metav1.NewTime(now),
	}, nil
}

func (o *orphanComparer) ResourceTree(context.Context, compare.TreeInput) (*compare.TreeResult, error) {
	o.trees++
	return &compare.TreeResult{Tree: &compare.ApplicationTree{}, OrphanedResources: 2,
		Conditions:              []compare.Condition{{Type: v1alpha1.ApplicationConditionOrphanedResourceWarning, Message: "Application has 2 orphaned resources"}},
		EvaluatedConditionTypes: []string{v1alpha1.ApplicationConditionOrphanedResourceWarning}}, nil
}

// A refresh that finds spec errors stores an empty tree and no managed resources, as upstream,
// and removes the orphaned-resources series; a later tree-only refresh (an orphan event)
// compares again instead of rebuilding the tree from the stale comparison, so neither the
// series nor OrphanedResourceWarning comes back while the spec stays invalid.
func TestSpecErrorClearsTreeState(t *testing.T) {
	t.Parallel()
	sch := runtime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(sch))
	proj := &v1alpha1.AppProject{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "patrol"},
		Spec: v1alpha1.AppProjectSpec{SourceRepos: []string{"*"},
			Destinations:      []v1alpha1.ApplicationDestination{{Server: "*", Namespace: "*"}},
			OrphanedResources: &v1alpha1.OrphanedResourcesMonitorSettings{Warn: ptr.To(true)}}}
	cluster := &v1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "k8s", Namespace: "patrol"}, Spec: v1alpha1.ClusterSpec{Server: testServer, Name: "k8s"}}
	app := &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "patrol", UID: "u-a"},
		Spec: v1alpha1.ApplicationSpec{Project: "p",
			Source:      &v1alpha1.ApplicationSource{RepoURL: "https://git.example/repo.git", Path: ".", TargetRevision: "HEAD"},
			Destination: v1alpha1.ApplicationDestination{Server: testServer, Namespace: "web"}}}
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(proj, cluster, app).
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
	ctx := context.Background()
	key := client.ObjectKeyFromObject(app)
	reconcileApp := func() {
		t.Helper()
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		require.NoError(t, err)
	}
	series := func() int {
		t.Helper()
		n, err := testutil.GatherAndCount(reg, "patrol_app_orphaned_resources_count")
		require.NoError(t, err)
		return n
	}
	conditions := func() []string {
		t.Helper()
		got := &v1alpha1.Application{}
		require.NoError(t, c.Get(ctx, key, got))
		var out []string
		for _, cd := range got.Status.Conditions {
			out = append(out, cd.Type)
		}
		return out
	}
	setRepos := func(repos ...string) {
		t.Helper()
		p := &v1alpha1.AppProject{}
		require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(proj), p))
		p.Spec.SourceRepos = repos
		require.NoError(t, c.Update(ctx, p))
		r.refresh.RequestRefresh(key, core.CompareLatest, 0)
		reconcileApp()
	}

	reconcileApp()
	require.Equal(t, 1, cmp.compares)
	require.Equal(t, 1, series(), "a monitored Application has a series")

	// The project stops permitting the repository: the spec is invalid.
	setRepos("https://other.example/*")
	assert.Equal(t, 1, cmp.compares, "an invalid spec is not compared")
	assert.Equal(t, 0, series(), "no series while the spec is invalid")
	assert.Contains(t, conditions(), v1alpha1.ApplicationConditionInvalidSpecError)
	state, ok := r.State(key)
	require.True(t, ok)
	assert.True(t, state.SpecInvalid)
	assert.Equal(t, &compare.ApplicationTree{}, state.Tree, "an empty tree, as upstream")
	assert.Nil(t, state.Managed)

	// An unmanaged object appears in the destination namespace: its tree-only refresh compares
	// again (the spec is still invalid) instead of rebuilding the tree.
	r.onResourceUpdated(testServer, liveRes("", "ConfigMap", "web", "stray", nil), nil, nsResources())
	assert.Equal(t, map[types.NamespacedName]requested{key: treeReq}, pendingRequests(r.refresh))
	reconcileApp()
	assert.Equal(t, map[types.NamespacedName]requested{key: recentReq}, pendingRequests(r.refresh), "a comparison instead")
	reconcileApp()
	assert.Zero(t, cmp.trees, "no tree built from the stale comparison")
	assert.Equal(t, 1, cmp.compares)
	assert.Equal(t, 0, series(), "still no series")
	assert.NotContains(t, conditions(), v1alpha1.ApplicationConditionOrphanedResourceWarning)

	// The project permits the repository again: compared, the series is back.
	setRepos("*")
	assert.Equal(t, 2, cmp.compares)
	assert.Equal(t, 1, series())
	state, _ = r.State(key)
	assert.False(t, state.SpecInvalid)
}
