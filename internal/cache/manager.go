package cache

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"

	"k8s.io/client-go/rest"
)

// ErrManagerClosed is returned by Manager.Get after Close.
var ErrManagerClosed = errors.New("cluster cache manager closed")

// ManagerOptions configures a Manager.
type ManagerOptions struct {
	// Logger is passed to every cache (default slog.Default()).
	Logger *slog.Logger
	// OnCacheCreated is called once for every newly created cache, before any Get returns it
	// and before its first sync, so the caller can register handlers. It must not call the
	// Manager.
	OnCacheCreated func(clusterKey string, c ClusterCache)
	// CacheOptions are applied to every cache.
	CacheOptions []Option
}

// Manager owns one Cluster per destination cluster key (for example the Cluster CRD UID or
// server URL). It is safe for concurrent use.
type Manager struct {
	opts ManagerOptions

	mu      sync.Mutex
	entries map[string]*managerEntry
	closed  bool
}

type managerEntry struct {
	c     *Cluster
	ready chan struct{} // closed once OnCacheCreated returned
}

// NewManager returns an empty Manager.
func NewManager(opts ManagerOptions) *Manager {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Manager{opts: opts, entries: map[string]*managerEntry{}}
}

// Get returns the synced cache for clusterKey, creating it from cfg and settings when it does
// not exist. An existing cache keeps its configuration: call Invalidate when the cluster's
// REST config or cache settings change. A failed sync returns the error but keeps the cache
// registered, so the next Get retries (subject to Settings.SyncRetryTimeout). ctx bounds the
// wait only: a sync that outlives it keeps running and serves later calls.
func (m *Manager) Get(ctx context.Context, clusterKey string, cfg *rest.Config, settings Settings) (ClusterCache, error) {
	for attempt := 0; ; attempt++ {
		c, err := m.getOrCreate(ctx, clusterKey, cfg, settings)
		if err != nil {
			return nil, err
		}
		err = c.EnsureSynced(ctx)
		switch {
		case err == nil:
			return c, nil
		case errors.Is(err, ErrClosed) && attempt == 0:
			continue // invalidated concurrently: use the replacement
		case errors.Is(err, ErrClosed):
			return nil, fmt.Errorf("cluster %q: %w", clusterKey, ErrManagerClosed)
		default:
			return nil, fmt.Errorf("sync cluster %q: %w", clusterKey, err)
		}
	}
}

func (m *Manager) getOrCreate(ctx context.Context, clusterKey string, cfg *rest.Config, settings Settings) (*Cluster, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, ErrManagerClosed
	}
	if e, ok := m.entries[clusterKey]; ok {
		m.mu.Unlock()
		select {
		case <-e.ready:
			return e.c, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	opts := append([]Option{WithLogger(m.opts.Logger.With("cluster", clusterKey))}, m.opts.CacheOptions...)
	c, err := New(cfg, settings, opts...)
	if err != nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("create cache for cluster %q: %w", clusterKey, err)
	}
	e := &managerEntry{c: c, ready: make(chan struct{})}
	m.entries[clusterKey] = e
	m.mu.Unlock()
	defer close(e.ready)
	if m.opts.OnCacheCreated != nil {
		m.opts.OnCacheCreated(clusterKey, c)
	}
	return c, nil
}

// Invalidate closes and forgets the cache of clusterKey; the next Get rebuilds it. It must
// not be called from a handler registered on that cache.
func (m *Manager) Invalidate(clusterKey string) {
	m.mu.Lock()
	e := m.entries[clusterKey]
	delete(m.entries, clusterKey)
	m.mu.Unlock()
	if e != nil {
		e.c.Close()
	}
}

// ClustersInfo returns the statistics of every cache, ordered by cluster key.
func (m *Manager) ClustersInfo() []ClusterInfo {
	m.mu.Lock()
	keys := slices.Sorted(maps.Keys(m.entries))
	caches := make([]*Cluster, 0, len(keys))
	for _, k := range keys {
		caches = append(caches, m.entries[k].c)
	}
	m.mu.Unlock()
	out := make([]ClusterInfo, 0, len(caches))
	for _, c := range caches {
		out = append(out, c.GetClusterInfo())
	}
	return out
}

// Close closes every cache. Subsequent Get calls fail with ErrManagerClosed.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	entries := m.entries
	m.entries = map[string]*managerEntry{}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, e := range entries {
		wg.Go(e.c.Close)
	}
	wg.Wait()
}
