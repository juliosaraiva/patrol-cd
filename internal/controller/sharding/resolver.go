package sharding

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/utils/clock"
	"k8s.io/utils/ptr"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// DestinationClusterLookup returns the cluster of an Application destination
// (*clusters.Registry implements it).
type DestinationClusterLookup interface {
	DestinationCluster(dest v1alpha1.ApplicationDestination) (*v1alpha1.Cluster, error)
}

// StatusShardResolver resolves the shard serving an Application for components that do not
// run an Assigner (patrol-server routing tree API calls): the destination cluster's
// status.assignedShard written by the controller, else its spec.shard, else 0. Unresolvable
// destinations map to shard 0, like Ownership. A stale answer costs one redirect: the tree API
// answers 421 with the right shard.
type StatusShardResolver struct {
	Clusters DestinationClusterLookup
}

// ShardForApplication implements the tree API client's ShardResolver.
func (r StatusShardResolver) ShardForApplication(_ context.Context, app *v1alpha1.Application) (int, error) {
	if r.Clusters == nil {
		return 0, errors.New("sharding: no cluster lookup configured")
	}
	c, err := r.Clusters.DestinationCluster(app.Spec.Destination)
	if err != nil || c == nil {
		return 0, nil //nolint:nilerr // unresolvable destinations are served by shard 0
	}
	switch {
	case c.Status.AssignedShard != nil && *c.Status.AssignedShard >= 0:
		return int(*c.Status.AssignedShard), nil
	case c.Spec.Shard != nil && *c.Spec.Shard >= 0:
		return int(*c.Spec.Shard), nil
	}
	return 0, nil
}

// DefaultHolderCacheTTL is how long LeaseHolders caches a holder.
const DefaultHolderCacheTTL = 5 * time.Second

// LeaseHolders returns the current holder of a shard Lease (the controller pod serving the
// shard), with a short cache.
type LeaseHolders struct {
	client LeaseClient
	ttl    time.Duration
	clock  clock.Clock

	mu    sync.Mutex
	cache map[int]holderEntry
}

type holderEntry struct {
	holder string
	until  time.Time
}

// NewLeaseHolders returns a holder lookup; ttl ≤ 0 uses DefaultHolderCacheTTL and a nil clk the
// real clock.
func NewLeaseHolders(client LeaseClient, ttl time.Duration, clk clock.Clock) *LeaseHolders {
	if ttl <= 0 {
		ttl = DefaultHolderCacheTTL
	}
	if clk == nil {
		clk = clock.RealClock{}
	}
	return &LeaseHolders{client: client, ttl: ttl, clock: clk, cache: map[int]holderEntry{}}
}

// ShardHolder returns the identity holding the Lease of shard, or "" when the Lease is
// missing, free or not renewed within its duration (judged with this process's clock).
func (h *LeaseHolders) ShardHolder(ctx context.Context, shard int) (string, error) {
	now := h.clock.Now()
	h.mu.Lock()
	if e, ok := h.cache[shard]; ok && now.Before(e.until) {
		h.mu.Unlock()
		return e.holder, nil
	}
	h.mu.Unlock()
	l, err := h.client.Get(ctx, LeaseName(shard))
	holder := ""
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return "", fmt.Errorf("get shard lease %d: %w", shard, err)
	default:
		holder = holderOf(l)
		if holder != "" && l.Spec.RenewTime != nil {
			d := time.Duration(ptr.Deref(l.Spec.LeaseDurationSeconds, 0)) * time.Second
			if d > 0 && now.After(l.Spec.RenewTime.Add(d)) {
				holder = ""
			}
		}
	}
	h.mu.Lock()
	h.cache[shard] = holderEntry{holder: holder, until: now.Add(h.ttl)}
	h.mu.Unlock()
	return holder, nil
}

// Invalidate drops the cached holder of shard (after a misdirected request).
func (h *LeaseHolders) Invalidate(shard int) {
	h.mu.Lock()
	delete(h.cache, shard)
	h.mu.Unlock()
}
