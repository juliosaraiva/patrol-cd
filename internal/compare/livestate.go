package compare

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sync"

	"k8s.io/client-go/rest"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/cache"
	"github.com/juliosaraiva/patrol-cd/internal/clusters"
	"github.com/juliosaraiva/patrol-cd/internal/diff"
	"github.com/juliosaraiva/patrol-cd/internal/lua"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	"github.com/juliosaraiva/patrol-cd/internal/tracking"
)

// ClusterConfigFunc returns the REST config used to reach a destination cluster
// (*clusters.Registry RestConfig, bound to its options).
type ClusterConfigFunc func(ctx context.Context, cluster *v1alpha1.Cluster) (*rest.Config, error)

// ClusterKey returns the live-state cache key of a cluster: its normalized server URL.
func ClusterKey(cluster *v1alpha1.Cluster) string {
	return clusters.NormalizeServer(cluster.Spec.Server)
}

// CacheSettings returns the live-state cache settings of cluster under the settings snapshot
// st: base (controller tuning knobs such as resync periods) completed with the resource
// filter, the cluster's namespaces, respectRBAC, the tracking keys and the ResourceInfo
// populator (attribution, health with Lua customizations, info items, update hashes).
func CacheSettings(st *settings.Settings, cluster *v1alpha1.Cluster, controlPlaneNamespace string, base cache.Settings, logger *slog.Logger) cache.Settings {
	if logger == nil {
		logger = slog.Default()
	}
	t := tracking.New(tracking.Config{InstanceLabelKey: st.InstanceLabelKey(), ControlPlaneNamespace: controlPlaneNamespace})
	var updates diff.Normalizer
	if st.IgnoreResourceUpdatesEnabled() {
		n, err := diff.NewIgnoreNormalizer(st.ResourceUpdateIgnoreRules(), diff.IgnoreNormalizerOptions{Log: logger})
		if err != nil {
			logger.Warn("ignoreResourceUpdates rules are invalid; resource updates are not filtered", "error", err)
		} else {
			updates = n
		}
	}
	s := base
	s.ResourceFilter = st.ResourceFilter()
	s.Namespaces = clusters.NormalizeNamespaces(cluster.Spec.Namespaces)
	s.ClusterResources = cluster.Spec.ClusterResources
	s.RespectRBAC = st.CacheRespectRBAC()
	s.TrackingLabelKeys = t.TrackingLabelKeys()
	s.TrackingAnnotationKeys = t.TrackingAnnotationKeys()
	s.PopulateResourceInfo = PopulateResourceInfo(InfoConfig{
		Tracker:          t,
		Method:           st.TrackingMethod(),
		InstallationID:   st.InstallationID(),
		Health:           lua.New(st.ResourceOverrides(), lua.WithLogger(logger)),
		CustomLabels:     st.CustomLabels(),
		UpdateNormalizer: updates,
	})
	return s
}

// cacheSignature digests everything CacheSettings derives from st and cluster, so a change
// can invalidate the cache (upstream invalidates on any change of the cache settings).
func cacheSignature(st *settings.Settings, cluster *v1alpha1.Cluster) string {
	payload := struct {
		Server           string
		Namespaces       []string
		ClusterResources bool
		InstanceLabelKey string
		TrackingMethod   v1alpha1.TrackingMethod
		InstallationID   string
		Filter           *settings.ResourcesFilter
		Overrides        map[string]lua.ResourceOverride
		CustomLabels     []string
		RespectRBAC      string
		UpdatesEnabled   bool
		UpdateRules      []diff.IgnoreRule
	}{
		Server:           ClusterKey(cluster),
		Namespaces:       clusters.NormalizeNamespaces(cluster.Spec.Namespaces),
		ClusterResources: cluster.Spec.ClusterResources,
		InstanceLabelKey: st.InstanceLabelKey(),
		TrackingMethod:   st.TrackingMethod(),
		InstallationID:   st.InstallationID(),
		Filter:           st.ResourceFilter(),
		Overrides:        st.ResourceOverrides(),
		CustomLabels:     st.CustomLabels(),
		RespectRBAC:      st.RespectRBAC(),
		UpdatesEnabled:   st.IgnoreResourceUpdatesEnabled(),
		UpdateRules:      st.ResourceUpdateIgnoreRules(),
	}
	b, err := json.Marshal(payload)
	if err != nil {
		// Unreachable for these types; fall back to the whole settings fingerprint.
		return "fp:" + st.Fingerprint()
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// CacheManager owns the per-cluster live-state caches; *cache.Manager implements it.
// Implementations return pointer-like (comparable) ClusterCache values, so the cache returned
// for a key can be recognized across calls.
type CacheManager interface {
	// Get returns the synced cache of clusterKey, creating it from cfg and settings when
	// absent. An existing cache keeps its configuration.
	Get(ctx context.Context, clusterKey string, cfg *rest.Config, settings cache.Settings) (cache.ClusterCache, error)
	// Invalidate closes and forgets the cache of clusterKey.
	Invalidate(clusterKey string)
}

var _ CacheManager = (*cache.Manager)(nil)

// LiveStateCacheOptions configures a LiveStateCache.
type LiveStateCacheOptions struct {
	// Manager owns the per-cluster caches (a *cache.Manager). Required. Register update
	// handlers through its OnCacheCreated option.
	Manager CacheManager
	// Config returns cluster REST configs. Required.
	Config ClusterConfigFunc
	// Settings provides patrol-cm settings. Required.
	Settings SettingsSource
	// ControlPlaneNamespace is the namespace Patrol CD runs in. Required.
	ControlPlaneNamespace string
	// Base holds the controller tuning knobs applied to every cache (resync periods, list
	// page size, RetainManagedFields for server-side diff, metrics).
	Base cache.Settings
	// Logger (default slog.Default()).
	Logger *slog.Logger
}

// LiveStateCache is the LiveStateProvider over a cache.Manager. It creates each cluster's
// cache with CacheSettings and reconfigures it when the cache-relevant settings or the
// cluster's namespaces change. Changes of the cluster's credentials or server must be handled
// by the caller (cache.Manager.Invalidate, then Forget). It is safe for concurrent use.
//
// A settings change is applied to the cache in place (ClusterCache.Invalidate with the new
// settings) before the cache is synced, and recorded only once applied: it neither depends
// on a sync with the old settings succeeding nor is lost when the call that first observes it
// fails. Callers observing the new settings never receive the cache before the change was
// applied; they wait (bounded by their context) while another caller applies it.
type LiveStateCache struct {
	o LiveStateCacheOptions

	mu      sync.Mutex // guards entries and every clusterEntry's fields
	entries map[string]*clusterEntry
}

// clusterEntry tracks the cache of one cluster key. Entries are never removed (Forget resets
// one), so callers of a key always serialize on the same sem; an entry is a few words.
type clusterEntry struct {
	// sem (capacity 1) serializes the transitions of the key: first build, settings
	// changes and the verification of a cache the Manager rebuilt.
	sem chan struct{}
	// gen is bumped by Forget; a transition records its outcome only if gen is unchanged.
	gen uint64
	// cc is the cache known to run with settings (signature sig); nil until verified.
	cc       cache.ClusterCache
	sig      string
	settings cache.Settings
	// attempt is the signature of the settings passed to Manager.Get while cc is nil, so a
	// cache the Manager built during a failed attempt is known to run with them. Empty:
	// unknown (first use or after Forget).
	attempt string
}

// NewLiveStateCache returns a LiveStateCache.
func NewLiveStateCache(o LiveStateCacheOptions) (*LiveStateCache, error) {
	var errs []error
	if o.Manager == nil {
		errs = append(errs, errors.New("compare: LiveStateCacheOptions.Manager is required"))
	}
	if o.Config == nil {
		errs = append(errs, errors.New("compare: LiveStateCacheOptions.Config is required"))
	}
	if o.Settings == nil {
		errs = append(errs, errors.New("compare: LiveStateCacheOptions.Settings is required"))
	}
	if o.ControlPlaneNamespace == "" {
		errs = append(errs, errors.New("compare: LiveStateCacheOptions.ControlPlaneNamespace is required"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return &LiveStateCache{o: o, entries: map[string]*clusterEntry{}}, nil
}

// ClusterCache implements LiveStateProvider.
func (l *LiveStateCache) ClusterCache(ctx context.Context, cluster *v1alpha1.Cluster) (cache.ClusterCache, error) {
	if cluster == nil {
		return nil, errors.New("compare: nil cluster")
	}
	key := ClusterKey(cluster)
	cfg, err := l.o.Config(ctx, cluster)
	if err != nil {
		return nil, fmt.Errorf("failed to get REST config of cluster %q: %w", cluster.Spec.Server, err)
	}
	st := l.o.Settings.Current()
	if st == nil {
		return nil, errors.New("compare: settings are not loaded")
	}
	sig := cacheSignature(st, cluster)

	l.mu.Lock()
	e := l.entry(key)
	verified, cs := e.cc, e.settings
	current := verified != nil && e.sig == sig
	l.mu.Unlock()
	if current {
		cc, err := l.o.Manager.Get(ctx, key, cfg, cs)
		if err != nil {
			return nil, err
		}
		if sameCache(cc, verified) {
			return cc, nil
		}
		// The Manager rebuilt the cache since it was verified: verify the replacement.
	}
	return l.transition(ctx, e, key, cluster, cfg)
}

// transition brings the cache of key in line with the current settings and returns it. It
// runs serialized per key.
func (l *LiveStateCache) transition(ctx context.Context, e *clusterEntry, key string, cluster *v1alpha1.Cluster, cfg *rest.Config) (cache.ClusterCache, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("wait for cluster %q cache: %w", key, err)
	}
	select {
	case e.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, fmt.Errorf("wait for cluster %q cache: %w", key, ctx.Err())
	}
	defer func() { <-e.sem }()

	// Read the settings again: a caller that waited must not apply an older snapshot over
	// the one a previous transition applied.
	st := l.o.Settings.Current()
	if st == nil {
		return nil, errors.New("compare: settings are not loaded")
	}
	sig := cacheSignature(st, cluster)
	l.mu.Lock()
	gen, verified, verifiedSig, cs, attempt := e.gen, e.cc, e.sig, e.settings, e.attempt
	l.mu.Unlock()
	if verified == nil || verifiedSig != sig {
		cs = CacheSettings(st, cluster, l.o.ControlPlaneNamespace, l.o.Base, l.o.Logger)
	}

	switch {
	case verified != nil && verifiedSig != sig:
		// Reconfigure the verified cache before syncing it, so applying the change neither
		// waits for nor depends on a sync with the old settings.
		l.o.Logger.Info("cache settings changed; invalidating cluster cache", "server", key)
		verified.Invalidate(cache.WithSettings(cs))
		l.record(e, gen, verified, sig, cs)
	case verified == nil && attempt != sig:
		// The Manager may hold a cache built with other settings by an earlier failed
		// attempt or before Forget: discard it so that Get builds one with cs.
		l.o.Manager.Invalidate(key)
		l.mu.Lock()
		if e.gen == gen {
			e.attempt = sig
		}
		l.mu.Unlock()
	}

	cc, err := l.o.Manager.Get(ctx, key, cfg, cs)
	if err != nil {
		return nil, err
	}
	if verified == nil || sameCache(cc, verified) {
		// Either the verified cache (now running cs), or one built under attempt == sig.
		l.record(e, gen, cc, sig, cs)
		return cc, nil
	}
	// The Manager replaced the verified cache (cache.Manager.Invalidate without Forget): the
	// replacement's settings are unknown, so apply cs to it.
	l.o.Logger.Info("cluster cache was rebuilt; applying cache settings", "server", key)
	cc.Invalidate(cache.WithSettings(cs))
	l.record(e, gen, cc, sig, cs)
	if err := cc.EnsureSynced(ctx); err != nil {
		return nil, fmt.Errorf("sync cluster %q: %w", key, err)
	}
	return cc, nil
}

// entry returns the entry of key, creating it; call with l.mu held.
func (l *LiveStateCache) entry(key string) *clusterEntry {
	e := l.entries[key]
	if e == nil {
		e = &clusterEntry{sem: make(chan struct{}, 1)}
		l.entries[key] = e
	}
	return e
}

// record notes that cc runs with cs (signature sig), unless Forget ran since gen.
func (l *LiveStateCache) record(e *clusterEntry, gen uint64, cc cache.ClusterCache, sig string, cs cache.Settings) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e.gen != gen {
		return
	}
	e.cc, e.sig, e.settings, e.attempt = cc, sig, cs, sig
}

// sameCache reports whether a and b are the same cache, without panicking on implementations
// whose dynamic type is not comparable.
func sameCache(a, b cache.ClusterCache) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	ta, tb := reflect.TypeOf(a), reflect.TypeOf(b)
	return ta == tb && ta.Comparable() && a == b
}

// Forget drops what is known about a cluster's cache. Call it after the cache was invalidated
// through the cache.Manager (for example when the cluster's credentials or server changed, or
// the cluster was removed); the next ClusterCache call builds a new cache.
func (l *LiveStateCache) Forget(cluster *v1alpha1.Cluster) {
	if cluster == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := l.entries[ClusterKey(cluster)]; e != nil {
		e.gen++
		e.cc, e.sig, e.settings, e.attempt = nil, "", cache.Settings{}, ""
	}
}
