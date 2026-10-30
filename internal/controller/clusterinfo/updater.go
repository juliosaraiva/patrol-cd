package clusterinfo

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/clusters"
	"github.com/juliosaraiva/patrol-cd/internal/controller/sharding"
	"github.com/juliosaraiva/patrol-cd/internal/project"
)

// DefaultCacheInfoWriteInterval limits writes of a Cluster whose cache statistics alone changed.
const DefaultCacheInfoWriteInterval = time.Minute

// Assigner is the cluster table of the controller (*sharding.Assigner implements it).
type Assigner interface {
	Clusters() []sharding.ClusterInfo
	Lookup(server string) (sharding.Assignment, bool)
}

// Recorder writes what the updater decided (*clusters.StatusWriter implements it).
type Recorder interface {
	RecordClusterInfo(ctx context.Context, c *v1alpha1.Cluster, u clusters.StatusUpdate) (*v1alpha1.Cluster, error)
	RecordVersionLabel(ctx context.Context, c *v1alpha1.Cluster, version string) (bool, error)
}

// ApplicationCounter counts Applications per normalized server (*clusters.Registry implements it).
type ApplicationCounter interface {
	ApplicationCounts(apps []v1alpha1.Application, permitted func(*v1alpha1.Application) bool) map[string]int64
}

// Options configures an Updater.
type Options struct {
	// Assigner lists the clusters and their shards. Required.
	Assigner Assigner
	// Owner tells which shards this replica owns. Required.
	Owner sharding.ShardOwner
	// Reader reads Clusters, Applications and AppProjects. It should be the manager's cached
	// client: every round reads all of them. Required.
	Reader client.Reader
	// Namespace is the control-plane namespace, which holds the AppProjects. Required.
	Namespace string
	// NamespaceAllowed tells whether this controller serves the Applications of a namespace: the
	// Reader can hold more than that. Required.
	NamespaceAllowed func(namespace string) bool
	// Counter counts the Applications of each cluster. Required.
	Counter ApplicationCounter
	// CacheState returns the live-state cache information of a normalized server, nil when this
	// replica has no cache for it. Required.
	CacheState func(server string) *clusters.CacheState
	// Recorder writes the results. Required.
	Recorder Recorder
	// Interval is the time between rounds (clusters.DefaultClusterInfoInterval when ≤ 0). It also
	// bounds a round: what a round did not reach waits for the next one.
	Interval time.Duration
	// CacheInfoWriteInterval is DefaultCacheInfoWriteInterval when ≤ 0.
	CacheInfoWriteInterval time.Duration
	// Now replaces time.Now.
	Now func() time.Time
	// Logger receives the start and failed rounds (nil discards).
	Logger *slog.Logger
}

// Updater keeps Cluster status up to date. A replica writes the clusters of the shards it owns
// only, so a cluster has one writer; the synthetic in-cluster cluster has no object and is skipped.
type Updater struct {
	opts Options
	// lastWrite is when this replica last wrote a server's status. Only Sync touches it.
	lastWrite map[string]time.Time
	// resume is the server a cut round stopped at, where the next one starts, so that the head of
	// a long list does not use every round. Only Sync touches it.
	resume string
}

// New returns an Updater.
func New(opts Options) (*Updater, error) {
	if opts.Assigner == nil || opts.Owner == nil || opts.Reader == nil || opts.Namespace == "" || opts.NamespaceAllowed == nil || opts.Counter == nil || opts.CacheState == nil || opts.Recorder == nil {
		return nil, errors.New("clusterinfo: assigner, shard owner, reader, namespace, namespace filter, application counter, cache state and recorder are required")
	}
	if opts.Interval <= 0 {
		opts.Interval = clusters.DefaultClusterInfoInterval
	}
	if opts.CacheInfoWriteInterval <= 0 {
		opts.CacheInfoWriteInterval = DefaultCacheInfoWriteInterval
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	return &Updater{opts: opts, lastWrite: map[string]time.Time{}}, nil
}

// Run does a round at once and one per interval until ctx ends. A failed round is logged and the
// next one tries again.
func (u *Updater) Run(ctx context.Context) {
	u.opts.Logger.Info("cluster info updater started", slog.Duration("interval", u.opts.Interval))
	ticker := time.NewTicker(u.opts.Interval)
	defer ticker.Stop()
	for {
		round, cancel := context.WithTimeout(ctx, u.opts.Interval)
		err := u.Sync(round)
		cancel()
		if err != nil && ctx.Err() == nil {
			u.opts.Logger.Warn("cannot record cluster info; retrying on the next round", slog.Any("error", err))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Sync does one round. It returns the joined errors; one cluster's failure does not stop the rest,
// but the end of ctx does.
func (u *Updater) Sync(ctx context.Context) error {
	counts, err := u.applicationCounts(ctx)
	if err != nil {
		return err
	}
	now := u.opts.Now()
	var errs []error
	owned := map[string]bool{}
	infos := u.opts.Assigner.Clusters()
	if i := slices.IndexFunc(infos, func(info sharding.ClusterInfo) bool { return info.Server >= u.resume }); i > 0 {
		infos = slices.Concat(infos[i:], infos[:i])
	}
	for _, info := range infos {
		if err := ctx.Err(); err != nil {
			u.resume = info.Server
			return errors.Join(append(errs, err)...)
		}
		if info.ObjectName == "" {
			continue
		}
		as, ok := u.opts.Assigner.Lookup(info.Server)
		if !ok || !u.opts.Owner.OwnsShard(as.Shard) {
			continue
		}
		owned[info.Server] = true
		if err := u.syncCluster(ctx, info, as, counts[info.Server], now); err != nil {
			errs = append(errs, fmt.Errorf("cluster %s/%s: %w", info.Namespace, info.ObjectName, err))
		}
	}
	u.resume = ""
	maps.DeleteFunc(u.lastWrite, func(server string, _ time.Time) bool { return !owned[server] })
	return errors.Join(errs...)
}

// applicationCounts counts, per server, the Applications of the namespaces this controller serves
// whose project exists and permits their namespace, as upstream.
func (u *Updater) applicationCounts(ctx context.Context) (map[string]int64, error) {
	// Shared with the informer cache: read only.
	var apps v1alpha1.ApplicationList
	if err := u.opts.Reader.List(ctx, &apps, client.UnsafeDisableDeepCopy); err != nil {
		return nil, fmt.Errorf("list applications: %w", err)
	}
	var projects v1alpha1.AppProjectList
	if err := u.opts.Reader.List(ctx, &projects, client.InNamespace(u.opts.Namespace), client.UnsafeDisableDeepCopy); err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	byName := make(map[string]*v1alpha1.AppProject, len(projects.Items))
	for i := range projects.Items {
		byName[projects.Items[i].Name] = &projects.Items[i]
	}
	return u.opts.Counter.ApplicationCounts(apps.Items, func(app *v1alpha1.Application) bool {
		proj, ok := byName[app.Spec.GetProject()]
		return ok && u.opts.NamespaceAllowed(app.Namespace) && project.IsAppNamespacePermitted(proj, app.Namespace, u.opts.Namespace)
	}), nil
}

func (u *Updater) syncCluster(ctx context.Context, info sharding.ClusterInfo, as sharding.Assignment, apps int64, now time.Time) error {
	// Shared with the informer cache: read only.
	c := &v1alpha1.Cluster{}
	switch err := u.opts.Reader.Get(ctx, client.ObjectKey{Namespace: info.Namespace, Name: info.ObjectName}, c, client.UnsafeDisableDeepCopy); {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return err
	}
	// No shard processes a skip-reconcile cluster, so it has no fresh state: Unknown rather than
	// the connection state from before the annotation.
	var state *clusters.CacheState
	if !as.Skip {
		state = u.opts.CacheState(info.Server)
	}
	update := clusters.ComputeClusterInfo(state, apps, now)
	if u.needsWrite(c, update, info.Server, now) {
		switch _, err := u.opts.Recorder.RecordClusterInfo(ctx, c, update); {
		case apierrors.IsNotFound(err):
			return nil
		case err != nil:
			return err
		}
		u.lastWrite[info.Server] = now
	}
	if state != nil && clusters.WantsVersionLabel(c, state.ServerVersion) {
		if _, err := u.opts.Recorder.RecordVersionLabel(ctx, c, state.ServerVersion); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// needsWrite reports whether update differs from the status of c enough to write it now.
func (u *Updater) needsWrite(c *v1alpha1.Cluster, update clusters.StatusUpdate, server string, now time.Time) bool {
	next := &v1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Generation: c.Generation}, Status: *c.Status.DeepCopy()}
	clusters.ApplyClusterInfo(next, update)
	if equality.Semantic.DeepEqual(&c.Status, &next.Status) {
		return false
	}
	next.Status.Info.CacheInfo = c.Status.Info.CacheInfo
	if !equality.Semantic.DeepEqual(&c.Status, &next.Status) {
		return true
	}
	return now.Sub(u.lastWrite[server]) >= u.opts.CacheInfoWriteInterval
}
