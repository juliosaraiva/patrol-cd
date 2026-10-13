package cache

import (
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

// Defaults, matching gitops-engine where it defines them.
const (
	DefaultWatchResyncTimeout      = 10 * time.Minute
	DefaultClusterResyncPeriod     = 24 * time.Hour
	DefaultSyncRetryTimeout        = 10 * time.Second
	DefaultListPageSize            = int64(500)
	DefaultListSemaphore           = 50
	DefaultListRetryAttempts       = 3
	DefaultEventProcessingInterval = 100 * time.Millisecond
)

// RespectRBAC controls how the cache reacts to APIs the cluster credentials may not access.
type RespectRBAC string

const (
	// RespectRBACNone fails the sync when an API cannot be listed (upstream default).
	RespectRBACNone RespectRBAC = "none"
	// RespectRBACNormal skips APIs whose list request is rejected with 401/403.
	RespectRBACNormal RespectRBAC = "normal"
	// RespectRBACStrict is RespectRBACNormal, but a rejected list is confirmed with a
	// SelfSubjectAccessReview; if the review says access is allowed the sync fails instead
	// of skipping the API (the rejection is treated as transient).
	RespectRBACStrict RespectRBAC = "strict"
)

// ResourceFilter decides whether a group/kind is excluded from caching on a cluster
// (identified by its API server URL).
type ResourceFilter interface {
	IsExcluded(group, kind, cluster string) bool
}

// Metrics receives cache instrumentation callbacks. Nil functions are ignored. Callbacks run
// without cache locks held and must be fast.
type Metrics struct {
	// ActiveWatches reports the number of running watch loops whenever it changes.
	ActiveWatches func(n int)
	// EventsProcessed reports each applied batch.
	EventsProcessed func(n int, d time.Duration)
	// SyncCompleted reports each full sync attempt.
	SyncCompleted func(d time.Duration, err error)
	// Relisted reports each relist of one API scope; reason is one of "expired", "resync",
	// "forbidden-poll", "error" or "initial".
	Relisted func(gk schema.GroupKind, namespace, reason string)
}

// Settings configures a cluster cache. Zero durations and sizes select the defaults above;
// negative durations disable the corresponding behavior.
type Settings struct {
	// ResourceFilter excludes group/kinds from discovery; excluded kinds are never listed or
	// watched. Nil excludes nothing.
	ResourceFilter ResourceFilter
	// Namespaces restricts namespaced resources to these namespaces (empty: all).
	Namespaces []string
	// ClusterResources includes cluster-scoped resources when Namespaces is set.
	ClusterResources bool

	// WatchResyncTimeout is the maximum time a watch runs before its API is relisted.
	WatchResyncTimeout time.Duration
	// ClusterResyncPeriod is the age after which a full resync is started (in background).
	ClusterResyncPeriod time.Duration
	// SyncRetryTimeout is the minimum time between failed full sync attempts; EnsureSynced
	// returns the previous error until it elapses.
	SyncRetryTimeout time.Duration
	// EventProcessingInterval is the batching window for watch events.
	EventProcessingInterval time.Duration

	// ListPageSize is the page size of list requests.
	ListPageSize int64
	// ListSemaphore bounds concurrent list operations for this cluster.
	ListSemaphore int
	// ListRetryAttempts bounds attempts per list page on retryable errors.
	ListRetryAttempts int

	// RespectRBAC selects the reaction to APIs the credentials cannot list.
	RespectRBAC RespectRBAC

	// PopulateResourceInfo computes Resource.Info. isRoot is true when the object has no
	// owner references. cacheManifest requests retention of the full manifest.
	PopulateResourceInfo func(un *unstructured.Unstructured, isRoot bool) (info any, cacheManifest bool)
	// IsManaged reports objects whose manifest must be retained (OR-ed with cacheManifest).
	IsManaged func(un *unstructured.Unstructured) bool
	// TrackingLabelKeys and TrackingAnnotationKeys select the label and annotation values
	// copied into Resource.Labels / Resource.Annotations.
	TrackingLabelKeys      []string
	TrackingAnnotationKeys []string
	// RetainManagedFields keeps metadata.managedFields on retained manifests (needed for
	// server-side diff and managedFieldsManagers ignore rules, at a memory cost).
	RetainManagedFields bool

	// Metrics receives instrumentation callbacks.
	Metrics Metrics
}

func (s Settings) normalized() Settings {
	if s.WatchResyncTimeout == 0 {
		s.WatchResyncTimeout = DefaultWatchResyncTimeout
	}
	if s.ClusterResyncPeriod == 0 {
		s.ClusterResyncPeriod = DefaultClusterResyncPeriod
	}
	if s.SyncRetryTimeout == 0 {
		s.SyncRetryTimeout = DefaultSyncRetryTimeout
	}
	if s.EventProcessingInterval == 0 {
		s.EventProcessingInterval = DefaultEventProcessingInterval
	}
	if s.ListPageSize <= 0 {
		s.ListPageSize = DefaultListPageSize
	}
	if s.ListSemaphore <= 0 {
		s.ListSemaphore = DefaultListSemaphore
	}
	if s.ListRetryAttempts <= 0 {
		s.ListRetryAttempts = DefaultListRetryAttempts
	}
	switch s.RespectRBAC {
	case RespectRBACNone, RespectRBACNormal, RespectRBACStrict:
	default:
		s.RespectRBAC = RespectRBACNone
	}
	s.Namespaces = normalizeStrings(s.Namespaces)
	s.TrackingLabelKeys = normalizeStrings(s.TrackingLabelKeys)
	s.TrackingAnnotationKeys = normalizeStrings(s.TrackingAnnotationKeys)
	return s
}

// normalizeStrings returns a sorted, de-duplicated copy without empty strings.
func normalizeStrings(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s != "" {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func (s *Settings) isExcluded(group, kind, server string) bool {
	return s.ResourceFilter != nil && s.ResourceFilter.IsExcluded(group, kind, server)
}

// managesNamespace reports whether objects in ns are in scope ("" is cluster scope).
func (s *Settings) managesNamespace(ns string) bool {
	if len(s.Namespaces) == 0 {
		return true
	}
	if ns == "" {
		return s.ClusterResources
	}
	_, ok := slices.BinarySearch(s.Namespaces, ns)
	return ok
}

// scopes returns the namespaces to list/watch an API in: "" means cluster-wide (or all
// namespaces). An empty result means the API is out of scope.
func (s *Settings) scopes(namespaced bool) []string {
	switch {
	case len(s.Namespaces) == 0:
		return []string{""}
	case namespaced:
		return s.Namespaces
	case s.ClusterResources:
		return []string{""}
	default:
		return nil
	}
}

// InvalidateOption modifies the cache configuration during Invalidate.
type InvalidateOption func(*invalidateConfig)

type invalidateConfig struct {
	settings *Settings
	config   *rest.Config
}

// WithSettings replaces the cache settings.
func WithSettings(s Settings) InvalidateOption {
	return func(c *invalidateConfig) { c.settings = &s }
}

// WithConfig replaces the REST config used to reach the cluster.
func WithConfig(cfg *rest.Config) InvalidateOption {
	return func(c *invalidateConfig) { c.config = cfg }
}
