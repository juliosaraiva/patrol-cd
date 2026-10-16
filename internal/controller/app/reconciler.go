// Package app implements the Application reconciler (APP-0xx lifecycle features, SYNC-017..025
// automated sync and self-heal, Application deletion): refresh triggers and comparison levels,
// comparison through internal/compare, status persistence with optimistic concurrency and
// minimal writes, aggregate conditions and Events, automated sync by creating Operations,
// pre/post-delete hooks and cascading deletion.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/clusters"
	"github.com/juliosaraiva/patrol-cd/internal/controller/core"
	"github.com/juliosaraiva/patrol-cd/internal/project"
)

// ControllerName names the Application controller (metrics, logs, events).
const ControllerName = "patrol-application"

// Field indexes of Applications.
const (
	indexProject     = "spec.project"
	indexDestination = "spec.destination"
	indexRepoURL     = "spec.sources.repoURL"
)

// repoErrorRetry is the requeue delay after a manifest error deferred by the grace period.
const repoErrorRetry = 30 * time.Second

// Config configures the Application reconciler.
type Config struct {
	core.Deps
	// DeletionKube returns the client used to delete resources and create delete hooks in a
	// destination cluster (nil: sync.NewKubeClient over Deps.ClusterConfig).
	DeletionKube DeletionKubeFunc
	// Jitter returns a random delay in [0, max) added to periodic refreshes (nil: uniform).
	Jitter func(max time.Duration) time.Duration
	// DeletionPoll is the requeue interval while deletion waits (default 2s).
	DeletionPoll time.Duration
}

// Reconciler reconciles Applications.
type Reconciler struct {
	cfg     Config
	c       client.Client
	log     *slog.Logger
	refresh *refreshQueue
	state   *stateStore
}

// New returns a Reconciler.
func New(cfg Config) (*Reconciler, error) {
	if err := cfg.Complete(); err != nil {
		return nil, err
	}
	if cfg.Jitter == nil {
		cfg.Jitter = randomJitter
	}
	if cfg.DeletionPoll <= 0 {
		cfg.DeletionPoll = 2 * time.Second
	}
	if cfg.DeletionKube == nil {
		cfg.DeletionKube = defaultDeletionKube(cfg.ClusterConfig, cfg.Metrics)
	}
	return &Reconciler{
		cfg:     cfg,
		c:       cfg.Client,
		log:     cfg.Logger.With("controller", ControllerName),
		refresh: newRefreshQueue(cfg.Clock),
		state:   newStateStore(),
	}, nil
}

// Refresher returns the refresh requester the Operation reconciler uses.
func (r *Reconciler) Refresher() core.RefreshRequester { return r.refresh }

// IndexFields registers the Application field indexes used by the watches.
func IndexFields(ctx context.Context, indexer client.FieldIndexer) error {
	if err := indexer.IndexField(ctx, &v1alpha1.Application{}, indexProject, func(o client.Object) []string {
		a, ok := o.(*v1alpha1.Application)
		if !ok {
			return nil
		}
		return []string{a.Spec.GetProject()}
	}); err != nil {
		return fmt.Errorf("index applications by project: %w", err)
	}
	if err := indexer.IndexField(ctx, &v1alpha1.Application{}, indexDestination, func(o client.Object) []string {
		a, ok := o.(*v1alpha1.Application)
		if !ok {
			return nil
		}
		return destinationKeys(a.Spec.Destination.Server, a.Spec.Destination.Name)
	}); err != nil {
		return fmt.Errorf("index applications by destination: %w", err)
	}
	if err := indexer.IndexField(ctx, &v1alpha1.Application{}, indexRepoURL, func(o client.Object) []string {
		a, ok := o.(*v1alpha1.Application)
		if !ok {
			return nil
		}
		var out []string
		for _, s := range a.Spec.GetSources() {
			out = append(out, project.NormalizeGitURL(s.RepoURL))
		}
		if h := a.Spec.SourceHydrator; h != nil {
			out = append(out, project.NormalizeGitURL(h.DrySource.RepoURL))
		}
		return out
	}); err != nil {
		return fmt.Errorf("index applications by repository: %w", err)
	}
	return nil
}

func destinationKeys(server, name string) []string {
	var out []string
	if server != "" {
		out = append(out, "server:"+clusters.NormalizeServer(server))
	}
	if name != "" {
		out = append(out, "name:"+name)
	}
	return out
}

// SetupWithManager registers the controller: Applications of allowed namespaces, their
// Operations, AppProjects, Clusters and Repositories (dependent Applications are refreshed
// at level latest), and the channel of controller-requested refreshes. IndexFields and
// core.IndexOperations must have been called.
func (r *Reconciler) SetupWithManager(mgr manager.Manager) error {
	allowed := predicate.NewPredicateFuncs(func(o client.Object) bool { return r.cfg.Namespaces().Allowed(o.GetNamespace()) })
	err := ctrl.NewControllerManagedBy(mgr).
		Named(ControllerName).
		For(&v1alpha1.Application{}, builder.WithPredicates(allowed)).
		Watches(&v1alpha1.Operation{}, handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
			op, ok := o.(*v1alpha1.Operation)
			if !ok || op.Spec.Application == "" {
				return nil
			}
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: op.Namespace, Name: op.Spec.Application}}}
		}), builder.WithPredicates(allowed)).
		Watches(&v1alpha1.AppProject{}, handler.EnqueueRequestsFromMapFunc(r.mapIndexed(indexProject, func(o client.Object) []string {
			if o.GetNamespace() != r.cfg.Namespace {
				return nil
			}
			return []string{o.GetName()}
		}))).
		Watches(&v1alpha1.Cluster{}, handler.EnqueueRequestsFromMapFunc(r.mapIndexed(indexDestination, func(o client.Object) []string {
			c, ok := o.(*v1alpha1.Cluster)
			if !ok || c.Namespace != r.cfg.Namespace {
				return nil
			}
			return destinationKeys(c.Spec.Server, c.Spec.Name)
		}))).
		Watches(&v1alpha1.Repository{}, handler.EnqueueRequestsFromMapFunc(r.mapIndexed(indexRepoURL, func(o client.Object) []string {
			repo, ok := o.(*v1alpha1.Repository)
			if !ok || repo.Namespace != r.cfg.Namespace {
				return nil
			}
			return []string{project.NormalizeGitURL(repo.Spec.URL)}
		}))).
		WatchesRawSource(source.Channel(r.refresh.events, &handler.TypedEnqueueRequestForObject[*v1alpha1.Application]{})).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: r.cfg.Params.StatusProcessors,
			SkipNameValidation:      ptr.To(true),
		}).
		Complete(r)
	if err != nil {
		return fmt.Errorf("create application controller: %w", err)
	}
	return nil
}

// mapIndexed maps a dependency object to the Applications indexed under the keys it yields,
// requesting a refresh at level latest for each.
func (r *Reconciler) mapIndexed(index string, keys func(client.Object) []string) handler.MapFunc {
	return func(ctx context.Context, o client.Object) []reconcile.Request {
		var out []reconcile.Request
		seen := map[types.NamespacedName]bool{}
		for _, k := range keys(o) {
			var list v1alpha1.ApplicationList
			if err := r.c.List(ctx, &list, client.MatchingFields{index: k}); err != nil {
				r.log.Warn("cannot list applications for a dependency change", "index", index, "error", err)
				continue
			}
			for i := range list.Items {
				key := client.ObjectKeyFromObject(&list.Items[i])
				if seen[key] || !r.cfg.Namespaces().Allowed(key.Namespace) {
					continue
				}
				seen[key] = true
				r.refresh.RequestRefresh(key, core.CompareLatest, 0)
				out = append(out, reconcile.Request{NamespacedName: key})
			}
		}
		return out
	}
}

// RefreshAll requests a latest-level refresh of every reconciled Application (settings
// changes).
func (r *Reconciler) RefreshAll(ctx context.Context) error {
	var list v1alpha1.ApplicationList
	if err := r.c.List(ctx, &list); err != nil {
		return fmt.Errorf("list applications: %w", err)
	}
	for i := range list.Items {
		if key := client.ObjectKeyFromObject(&list.Items[i]); r.cfg.Namespaces().Allowed(key.Namespace) {
			r.refresh.RequestRefresh(key, core.CompareLatest, 0)
		}
	}
	return nil
}

// Reconcile refreshes, auto-syncs or finalizes one Application.
func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (res reconcile.Result, err error) {
	// Any reconcile consumes the outstanding refresh notification of the key; requests made
	// from now on notify again (the pending request itself is taken by refreshApp).
	r.refresh.received(req.NamespacedName)
	app := &v1alpha1.Application{}
	if err := r.c.Get(ctx, req.NamespacedName, app); err != nil {
		if client.IgnoreNotFound(err) == nil {
			r.refresh.forget(req.NamespacedName)
			r.state.forget(req.NamespacedName)
		}
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if !r.cfg.Namespaces().Allowed(app.Namespace) {
		return reconcile.Result{}, nil
	}
	if core.SkipReconcile(app.Annotations) {
		r.log.Debug("skipping application with skip-reconcile", "application", app.QualifiedName())
		return reconcile.Result{}, nil
	}
	st, err := r.cfg.CurrentSettings()
	if err != nil {
		return reconcile.Result{}, err
	}
	cluster, destErr := r.cfg.DestinationCluster(ctx, app.Spec.Destination, st)
	if destErr != nil {
		cluster = nil
	} else if core.SkipReconcile(cluster.Annotations) {
		r.log.Debug("skipping application of a cluster with skip-reconcile", "application", app.QualifiedName(), "server", cluster.Spec.Server)
		return reconcile.Result{}, nil
	}
	// Sharding (ADR-0008): only the shard owning the destination processes the Application;
	// an unresolvable destination (nil cluster) belongs to shard 0.
	if r.cfg.OwnsCluster != nil && !r.cfg.OwnsCluster(cluster) {
		return reconcile.Result{}, nil
	}
	log := r.log.With("application", app.QualifiedName())
	defer func() {
		if errors.Is(err, core.ErrGone) {
			res, err = reconcile.Result{}, nil
		}
	}()
	run := &appRun{r: r, app: app, st: st, cluster: cluster, destErr: destErr, log: log}
	if app.DeletionTimestamp != nil {
		return run.finalize(ctx)
	}
	return run.refreshApp(ctx)
}
