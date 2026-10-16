package sharding

import (
	"cmp"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/types"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/clusters"
)

// Assignment is the shard of a cluster.
type Assignment struct {
	// Shard is the shard whose owner processes the cluster's Applications (≥ 0).
	Shard int
	// Skip reports the skip-reconcile annotation: no shard processes the cluster.
	Skip bool
}

// Change reports a changed assignment. Old is nil for a new cluster and New is nil for a
// removed one.
type Change struct {
	Server string
	Old    *Assignment
	New    *Assignment
}

// AssignerOptions configure an Assigner.
type AssignerOptions struct {
	// Algorithm distributes clusters without an explicit shard (DefaultAlgorithm when empty).
	Algorithm Algorithm
	// Replicas is the initial replica count; values below 1 count as 1.
	Replicas int
	// Logger receives assignment changes and warnings (nil discards).
	Logger *slog.Logger
	// Registerer receives the assignment metrics (nil: not registered).
	Registerer prometheus.Registerer
}

// Assigner is the cluster → shard table of the controller (upstream ClusterSharding): it is
// fed with clusters (registry events) and Applications (informer events) and recomputes the
// distribution when an input that matters changes. It is safe for concurrent use.
type Assigner struct {
	alg     Algorithm
	logger  *slog.Logger
	metrics *assignerMetrics

	mu          sync.RWMutex
	replicas    int
	clusters    map[string]ClusterInfo
	apps        map[types.NamespacedName]string // Application → spec.destination.server (normalized)
	appCounts   map[string]int64
	assignments map[string]Assignment
	pending     [][]Change

	notifyMu sync.Mutex
	subsMu   sync.Mutex
	subs     map[uint64]func([]Change)
	nextSub  uint64
}

// NewAssigner returns an empty Assigner.
func NewAssigner(opts AssignerOptions) (*Assigner, error) {
	m, err := newAssignerMetrics(opts.Registerer)
	if err != nil {
		return nil, err
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	alg := opts.Algorithm
	if alg == "" {
		alg = DefaultAlgorithm
	}
	a := &Assigner{
		alg:         alg,
		logger:      logger,
		metrics:     m,
		replicas:    max(opts.Replicas, 1),
		clusters:    map[string]ClusterInfo{},
		apps:        map[types.NamespacedName]string{},
		appCounts:   map[string]int64{},
		assignments: map[string]Assignment{},
		subs:        map[uint64]func([]Change){},
	}
	m.replicas.Set(float64(a.replicas))
	return a, nil
}

// Algorithm returns the distribution algorithm.
func (a *Assigner) Algorithm() Algorithm { return a.alg }

// Replicas returns the replica count clusters are distributed over (≥ 1).
func (a *Assigner) Replicas() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.replicas
}

// SetReplicas changes the replica count (values below 1 count as 1) and redistributes. It
// reports whether the count changed.
func (a *Assigner) SetReplicas(n int) bool {
	n = max(n, 1)
	a.mu.Lock()
	if n == a.replicas {
		a.mu.Unlock()
		return false
	}
	a.logger.Info("controller replica count changed; redistributing clusters", slog.Int("from", a.replicas), slog.Int("to", n))
	a.replicas = n
	a.metrics.replicas.Set(float64(n))
	a.recomputeAndNotify()
	return true
}

// Init replaces all clusters and Applications (upstream ClusterSharding.Init).
func (a *Assigner) Init(clusterList []*v1alpha1.Cluster, apps []*v1alpha1.Application) {
	a.mu.Lock()
	a.clusters = make(map[string]ClusterInfo, len(clusterList))
	for _, c := range clusterList {
		if c == nil {
			continue
		}
		info := ClusterInfoFrom(c)
		a.clusters[info.Server] = info
	}
	a.apps = make(map[types.NamespacedName]string, len(apps))
	a.appCounts = map[string]int64{}
	for _, app := range apps {
		if app == nil {
			continue
		}
		server := appServer(app)
		a.apps[types.NamespacedName{Namespace: app.Namespace, Name: app.Name}] = server
		a.appCounts[server]++
	}
	a.recomputeAndNotify()
}

// SetCluster adds or updates a cluster.
func (a *Assigner) SetCluster(c *v1alpha1.Cluster) {
	if c == nil {
		return
	}
	info := ClusterInfoFrom(c)
	a.mu.Lock()
	if old, ok := a.clusters[info.Server]; ok && clusterInfoEqual(old, info) {
		a.mu.Unlock()
		return
	}
	a.clusters[info.Server] = info
	a.recomputeAndNotify()
}

// DeleteCluster removes the cluster serving server.
func (a *Assigner) DeleteCluster(server string) {
	server = clusters.NormalizeServer(server)
	a.mu.Lock()
	if _, ok := a.clusters[server]; !ok {
		a.mu.Unlock()
		return
	}
	delete(a.clusters, server)
	a.recomputeAndNotify()
}

// HandleClusterEvent applies a registry event (subscribe with clusters.Registry.Subscribe).
func (a *Assigner) HandleClusterEvent(ev clusters.Event) {
	switch ev.Type {
	case clusters.EventDeleted:
		a.DeleteCluster(ev.Server)
	default:
		if ev.New != nil {
			a.SetCluster(ev.New)
		}
	}
}

// SetApplication records the destination of an Application. Only consistent hashing uses
// Application counts; other algorithms are not recomputed.
func (a *Assigner) SetApplication(app *v1alpha1.Application) {
	if app == nil {
		return
	}
	key := types.NamespacedName{Namespace: app.Namespace, Name: app.Name}
	server := appServer(app)
	a.mu.Lock()
	old, ok := a.apps[key]
	if ok && old == server {
		a.mu.Unlock()
		return
	}
	if ok {
		a.decAppLocked(old)
	}
	a.apps[key] = server
	a.appCounts[server]++
	if !a.appsMatterLocked() {
		a.mu.Unlock()
		return
	}
	a.recomputeAndNotify()
}

// DeleteApplication forgets an Application.
func (a *Assigner) DeleteApplication(namespace, name string) {
	key := types.NamespacedName{Namespace: namespace, Name: name}
	a.mu.Lock()
	old, ok := a.apps[key]
	if !ok {
		a.mu.Unlock()
		return
	}
	delete(a.apps, key)
	a.decAppLocked(old)
	if !a.appsMatterLocked() {
		a.mu.Unlock()
		return
	}
	a.recomputeAndNotify()
}

func (a *Assigner) decAppLocked(server string) {
	if a.appCounts[server] <= 1 {
		delete(a.appCounts, server)
		return
	}
	a.appCounts[server]--
}

func (a *Assigner) appsMatterLocked() bool {
	return a.alg == AlgorithmConsistentHashing && a.replicas > 1
}

// Lookup returns the assignment of the cluster serving server; ok is false for unknown
// servers.
func (a *Assigner) Lookup(server string) (Assignment, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	as, ok := a.assignments[clusters.NormalizeServer(server)]
	return as, ok
}

// ShardForServer returns the shard processing Applications that target server (upstream
// IsManagedCluster): managed is false for skip-reconcile clusters; an unknown server maps to
// shard 0.
func (a *Assigner) ShardForServer(server string) (shard int, managed bool) {
	as, ok := a.Lookup(server)
	if !ok {
		a.logger.Debug(fmt.Sprintf("The cluster %s has no assigned shard.", server), slog.String("server", server))
		return 0, true
	}
	if as.Skip {
		return as.Shard, false
	}
	return as.Shard, true
}

// Distribution returns server → shard for every known cluster (upstream GetDistribution).
func (a *Assigner) Distribution() map[string]int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make(map[string]int, len(a.assignments))
	for s, as := range a.assignments {
		out[s] = as.Shard
	}
	return out
}

// AppDistribution returns the number of Applications per destination server (upstream
// GetAppDistribution; Applications addressing clusters by name count under "").
func (a *Assigner) AppDistribution() map[string]int64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return maps.Clone(a.appCounts)
}

// Clusters returns the known clusters sorted by server.
func (a *Assigner) Clusters() []ClusterInfo {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := slices.Collect(maps.Values(a.clusters))
	slices.SortFunc(out, func(x, y ClusterInfo) int { return cmp.Compare(x.Server, y.Server) })
	return out
}

// Subscribe registers fn for assignment changes. Batches are delivered in recomputation
// order, synchronously by the goroutine that changed the table (or a concurrent one), outside
// the table lock; fn may read the Assigner but must not modify it. cancel unregisters fn.
func (a *Assigner) Subscribe(fn func([]Change)) (cancel func()) {
	a.subsMu.Lock()
	defer a.subsMu.Unlock()
	id := a.nextSub
	a.nextSub++
	a.subs[id] = fn
	return func() {
		a.subsMu.Lock()
		defer a.subsMu.Unlock()
		delete(a.subs, id)
	}
}

// recomputeAndNotify recomputes the table; it must be called with a.mu held and releases it.
// Change batches are queued under a.mu (so they keep the recomputation order) and delivered
// outside it by whichever caller drains the queue first.
func (a *Assigner) recomputeAndNotify() {
	if changes := a.recomputeLocked(); len(changes) > 0 {
		a.pending = append(a.pending, changes)
	}
	a.mu.Unlock()
	a.drain()
}

// drain delivers queued change batches in order. Lock order: notifyMu, then mu (briefly).
func (a *Assigner) drain() {
	a.notifyMu.Lock()
	defer a.notifyMu.Unlock()
	for {
		a.mu.Lock()
		if len(a.pending) == 0 {
			a.mu.Unlock()
			return
		}
		batch := a.pending[0]
		a.pending = a.pending[1:]
		a.mu.Unlock()
		a.subsMu.Lock()
		subs := slices.Collect(maps.Values(a.subs))
		a.subsMu.Unlock()
		for _, fn := range subs {
			fn(slices.Clone(batch))
		}
	}
}

func (a *Assigner) recomputeLocked() []Change {
	list := slices.Collect(maps.Values(a.clusters))
	slices.SortFunc(list, func(x, y ClusterInfo) int { return cmp.Compare(x.Server, y.Server) })
	shards := Distribute(a.alg, list, a.appCounts, a.replicas, a.logger)

	next := make(map[string]Assignment, len(list))
	var changes []Change
	for _, c := range list {
		as := Assignment{Shard: shards[c.Server], Skip: c.SkipReconcile}
		next[c.Server] = as
		old, ok := a.assignments[c.Server]
		switch {
		case !ok:
			a.logger.Info(fmt.Sprintf("Cluster %s has been assigned to shard %d", c.Server, as.Shard), slog.String("server", c.Server), slog.Int("shard", as.Shard))
			changes = append(changes, Change{Server: c.Server, New: &as})
		case old != as:
			if old.Shard != as.Shard {
				a.logger.Info(fmt.Sprintf("Cluster %s has changed shard from %d to %d", c.Server, old.Shard, as.Shard), slog.String("server", c.Server), slog.Int("from", old.Shard), slog.Int("to", as.Shard))
			}
			o := old
			changes = append(changes, Change{Server: c.Server, Old: &o, New: &as})
		}
	}
	removed := slices.Sorted(maps.Keys(a.assignments))
	for _, s := range removed {
		if _, ok := next[s]; ok {
			continue
		}
		o := a.assignments[s]
		changes = append(changes, Change{Server: s, Old: &o})
	}
	a.metrics.clusterShard.Reset()
	perShard := make([]int, a.replicas)
	for _, c := range list {
		as := next[c.Server]
		a.metrics.clusterShard.WithLabelValues(c.Server, c.Name).Set(float64(as.Shard))
		if as.Shard >= 0 && as.Shard < len(perShard) {
			perShard[as.Shard]++
		}
	}
	a.metrics.shardClusters.Reset()
	for s, n := range perShard {
		a.metrics.shardClusters.WithLabelValues(strconv.Itoa(s)).Set(float64(n))
	}
	a.assignments = next
	return changes
}

// appServer is the destination server an Application counts under (upstream counts
// spec.destination.server as is; Patrol normalizes trailing slashes).
func appServer(app *v1alpha1.Application) string {
	return clusters.NormalizeServer(app.Spec.Destination.Server)
}

func clusterInfoEqual(a, b ClusterInfo) bool {
	if (a.Shard == nil) != (b.Shard == nil) || (a.Shard != nil && *a.Shard != *b.Shard) {
		return false
	}
	a.Shard, b.Shard = nil, nil
	return a == b
}
