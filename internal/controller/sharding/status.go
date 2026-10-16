package sharding

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/utils/ptr"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// ClusterStatusUpdater writes Cluster status (*clusters.StatusWriter implements it: it re-reads
// the object, applies mutate and writes the status subresource only when it changed).
type ClusterStatusUpdater interface {
	Update(ctx context.Context, namespace, name string, mutate func(*v1alpha1.Cluster) error) (*v1alpha1.Cluster, error)
}

// DefaultAssignedShardResync is how often AssignedShardWriter.Run re-checks every owned
// cluster even without changes.
const DefaultAssignedShardResync = 10 * time.Minute

// AssignedShardWriter keeps Cluster.status.assignedShard up to date. Each replica writes only
// the clusters assigned to shards it owns, so there is one writer per cluster; the synthetic
// in-cluster cluster has no object and is skipped.
type AssignedShardWriter struct {
	assigner *Assigner
	owner    ShardOwner
	updater  ClusterStatusUpdater
	logger   *slog.Logger

	mu      sync.Mutex
	written map[string]int // server → shard last written (or confirmed) by this replica
	trigger chan struct{}
}

// NewAssignedShardWriter returns a writer.
func NewAssignedShardWriter(assigner *Assigner, owner ShardOwner, updater ClusterStatusUpdater, logger *slog.Logger) (*AssignedShardWriter, error) {
	if assigner == nil || owner == nil || updater == nil {
		return nil, errors.New("sharding: assigner, shard owner and status updater are required")
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &AssignedShardWriter{
		assigner: assigner,
		owner:    owner,
		updater:  updater,
		logger:   logger,
		written:  map[string]int{},
		trigger:  make(chan struct{}, 1),
	}, nil
}

// Trigger requests a Sync from Run (coalesced, non-blocking). Call it when shard ownership
// changes (LeaseManager callbacks); assignment changes trigger it automatically.
func (w *AssignedShardWriter) Trigger() {
	w.mu.Lock()
	clear(w.written)
	w.mu.Unlock()
	select {
	case w.trigger <- struct{}{}:
	default:
	}
}

// Sync writes status.assignedShard of every cluster whose shard this replica owns and whose
// value was not written yet. It returns the joined write errors (missing clusters are
// ignored).
func (w *AssignedShardWriter) Sync(ctx context.Context) error {
	var errs []error
	for _, c := range w.assigner.Clusters() {
		if c.ObjectName == "" {
			continue
		}
		as, ok := w.assigner.Lookup(c.Server)
		if !ok || !w.owner.OwnsShard(as.Shard) {
			continue
		}
		w.mu.Lock()
		done, seen := w.written[c.Server]
		w.mu.Unlock()
		if seen && done == as.Shard {
			continue
		}
		shard := int64(as.Shard)
		_, err := w.updater.Update(ctx, c.Namespace, c.ObjectName, func(cl *v1alpha1.Cluster) error {
			cl.Status.AssignedShard = ptr.To(shard)
			return nil
		})
		switch {
		case apierrors.IsNotFound(err):
			continue
		case err != nil:
			errs = append(errs, fmt.Errorf("write assigned shard of cluster %s/%s: %w", c.Namespace, c.ObjectName, err))
			continue
		}
		w.mu.Lock()
		w.written[c.Server] = as.Shard
		w.mu.Unlock()
		w.logger.Debug("recorded assigned shard", slog.String("cluster", c.ObjectName), slog.Int("shard", as.Shard))
	}
	return errors.Join(errs...)
}

// Run syncs on assignment changes, on Trigger and every resync (DefaultAssignedShardResync
// when ≤ 0) until ctx ends. Failed writes are retried on the next round.
func (w *AssignedShardWriter) Run(ctx context.Context, resync time.Duration) {
	if resync <= 0 {
		resync = DefaultAssignedShardResync
	}
	cancel := w.assigner.Subscribe(func([]Change) {
		select {
		case w.trigger <- struct{}{}:
		default:
		}
	})
	defer cancel()
	timer := time.NewTimer(0)
	defer timer.Stop()
	retry := time.Duration(0)
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.trigger:
		case <-timer.C:
			if retry == 0 {
				w.mu.Lock()
				clear(w.written)
				w.mu.Unlock()
			}
		}
		if err := w.Sync(ctx); err != nil && ctx.Err() == nil {
			w.logger.Warn("cannot record assigned shards; retrying", slog.Any("error", err))
			retry = min(max(2*retry, time.Second), time.Minute)
			timer.Reset(retry)
			continue
		}
		retry = 0
		timer.Reset(resync)
	}
}
