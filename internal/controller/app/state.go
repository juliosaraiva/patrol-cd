package app

import (
	"context"
	gosync "sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/compare"
	"github.com/juliosaraiva/patrol-cd/internal/controller/core"
	"github.com/juliosaraiva/patrol-cd/internal/project"
)

// AppState is the in-memory result of an Application's latest comparison: the resource tree
// and the managed resources (Secret data masked). It backs the controller's tree API and
// tree-only refreshes. It is never persisted: after a restart it is rebuilt by the next
// comparison (Kubernetes stays the only authoritative store, ADR-0004).
type AppState struct {
	// Tree is the resource tree (nil when it could not be built).
	Tree *compare.ApplicationTree
	// Managed are the managed resources with Secret data masked.
	Managed []compare.ManagedResource
	// ComparedAt is when the comparison or tree refresh completed.
	ComparedAt time.Time
}

// TreeBuilder rebuilds an Application's tree from the managed resources of its last
// comparison; *compare.Manager implements it.
type TreeBuilder interface {
	ResourceTree(ctx context.Context, in compare.TreeInput) (*compare.TreeResult, error)
}

type stateStore struct {
	mu gosync.RWMutex
	m  map[types.NamespacedName]AppState
}

func newStateStore() *stateStore { return &stateStore{m: map[types.NamespacedName]AppState{}} }

func (s *stateStore) get(key types.NamespacedName) (AppState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st, ok := s.m[key]
	return st, ok
}

func (s *stateStore) set(key types.NamespacedName, st AppState) {
	s.mu.Lock()
	s.m[key] = st
	s.mu.Unlock()
}

func (s *stateStore) forget(key types.NamespacedName) {
	s.mu.Lock()
	delete(s.m, key)
	s.mu.Unlock()
}

// State returns the latest comparison state of an Application reconciled by this
// controller.
func (r *Reconciler) State(key types.NamespacedName) (AppState, bool) { return r.state.get(key) }

// storeComparison records the tree and masked managed resources of a comparison.
func (a *appRun) storeComparison(ctx context.Context, res *compare.Result) {
	managed, err := res.MaskSecrets(ctx, a.st.SensitiveMaskAnnotations())
	if err != nil {
		a.log.Warn("cannot mask secret data of managed resources; not keeping them", "error", err)
		managed = nil
	}
	tree := res.Tree
	if tree == nil {
		if prev, ok := a.r.state.get(client.ObjectKeyFromObject(a.app)); ok {
			tree = prev.Tree
		}
	}
	a.r.state.set(client.ObjectKeyFromObject(a.app), AppState{Tree: tree, Managed: managed, ComparedAt: a.now()})
}

// refreshTree rebuilds the resource tree from the managed resources of the last comparison
// (upstream comparison level "nothing": a descendant of a managed resource changed) and
// persists the summary and the orphaned-resource condition. Without a stored comparison
// (after a restart) it falls back to a comparison at level recent.
func (a *appRun) refreshTree(ctx context.Context) (reconcile.Result, error) {
	key := client.ObjectKeyFromObject(a.app)
	prev, ok := a.r.state.get(key)
	tb, isBuilder := a.r.cfg.Comparer.(TreeBuilder)
	if !ok || !isBuilder || a.destErr != nil {
		a.r.refresh.RequestRefresh(key, core.CompareRecent, 0)
		return reconcile.Result{}, nil
	}
	proj, err := a.r.cfg.ResolveProject(ctx, a.app, a.st)
	if err != nil {
		return reconcile.Result{}, nil
	}
	tr, err := tb.ResourceTree(ctx, compare.TreeInput{App: a.app, Project: proj, Cluster: a.cluster, Managed: prev.Managed})
	if err != nil {
		a.log.Debug("tree-only refresh failed", "error", err)
		return reconcile.Result{}, nil
	}
	a.r.state.set(key, AppState{Tree: tr.Tree, Managed: prev.Managed, ComparedAt: a.now()})
	a.r.cfg.Metrics.SetOrphanedResources(a.app.Namespace, a.app.Name, a.app.Spec.GetProject(), project.OrphanedResourcesEnabled(proj), tr.OrphanedResources)
	now := metav1.NewTime(a.now())
	_, err = core.PatchStatus(ctx, a.r.c, a.r.cfg.APIReader, a.app, func(app *v1alpha1.Application) error {
		app.Status.Summary = tr.Summary
		app.Status.Conditions = compare.MergeConditions(app.Status.Conditions, tr.Conditions, tr.EvaluatedConditionTypes, app.Generation, now)
		return nil
	})
	return reconcile.Result{}, err
}
