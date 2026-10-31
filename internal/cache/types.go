package cache

import (
	"context"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

// ResourceKey identifies an object in a cluster independently of its API version.
type ResourceKey struct {
	Group     string
	Kind      string
	Namespace string
	Name      string
}

// NewResourceKey returns a ResourceKey.
func NewResourceKey(group, kind, namespace, name string) ResourceKey {
	return ResourceKey{Group: group, Kind: kind, Namespace: namespace, Name: name}
}

// KeyOf returns the key of an unstructured object.
func KeyOf(un *unstructured.Unstructured) ResourceKey {
	gvk := un.GroupVersionKind()
	return ResourceKey{Group: gvk.Group, Kind: gvk.Kind, Namespace: un.GetNamespace(), Name: un.GetName()}
}

// String renders the key as "group/kind/namespace/name" (core group and cluster-scoped
// namespace are empty segments), the format used by upstream Argo CD.
func (k ResourceKey) String() string {
	return k.Group + "/" + k.Kind + "/" + k.Namespace + "/" + k.Name
}

// GroupKind returns the key's group and kind.
func (k ResourceKey) GroupKind() schema.GroupKind {
	return schema.GroupKind{Group: k.Group, Kind: k.Kind}
}

// ParseResourceKey parses the output of [ResourceKey.String]. Kind and name must be non-empty
// and no segment may contain '/'.
func ParseResourceKey(s string) (ResourceKey, error) {
	parts := strings.Split(s, "/")
	if len(parts) != 4 {
		return ResourceKey{}, fmt.Errorf("%w: %q: expected group/kind/namespace/name", ErrInvalidResourceKey, s)
	}
	k := ResourceKey{Group: parts[0], Kind: parts[1], Namespace: parts[2], Name: parts[3]}
	if k.Kind == "" || k.Name == "" {
		return ResourceKey{}, fmt.Errorf("%w: %q: kind and name are required", ErrInvalidResourceKey, s)
	}
	return k, nil
}

// ResourceRef identifies a concrete object: its key plus UID and the API version it was
// observed with.
type ResourceRef struct {
	ResourceKey
	UID        types.UID
	APIVersion string
}

// Resource is the cache's compact record of a live object. Values returned by the cache are
// shared and must be treated as read-only; the cache replaces (never mutates) a Resource when
// the object changes.
type Resource struct {
	// Ref identifies the object.
	Ref ResourceRef
	// OwnerRefs are the object's owner references plus references inferred by the cache
	// (Endpoints→Service, service-account token Secret→ServiceAccount,
	// ClusterServiceVersion→OperatorGroup, StatefulSet→PersistentVolumeClaim).
	OwnerRefs []metav1.OwnerReference
	// ResourceVersion is the most recently observed resourceVersion.
	ResourceVersion string
	// CreationTimestamp is nil when the object has none.
	CreationTimestamp *metav1.Time
	// Labels holds only the label keys listed in Settings.TrackingLabelKeys.
	Labels map[string]string
	// Annotations holds only the annotation keys listed in Settings.TrackingAnnotationKeys.
	Annotations map[string]string
	// Info is the value returned by Settings.PopulateResourceInfo.
	Info any
	// Resource is the full manifest, retained only for objects the caller marks as managed
	// (Settings.IsManaged or the cacheManifest result of Settings.PopulateResourceInfo).
	// metadata.managedFields (unless Settings.RetainManagedFields) and the
	// kubectl.kubernetes.io/last-applied-configuration annotation are removed; see
	// LastAppliedConfiguration. It is shared: never mutate it.
	Resource *unstructured.Unstructured
	// LastAppliedConfiguration holds the kubectl last-applied annotation value of a retained
	// manifest. GetManagedLiveObjs restores it on the copies it returns.
	LastAppliedConfiguration string

	// inferredChildren reports whether this object is the inferred parent of key (StatefulSet
	// volumeClaimTemplates → PersistentVolumeClaims).
	inferredChildren func(key ResourceKey) bool
	// inferredUIDs lists owner UIDs in OwnerRefs that were added by StatefulSet inference.
	inferredUIDs []types.UID
}

// ResourceKey returns the key of the resource.
func (r *Resource) ResourceKey() ResourceKey { return r.Ref.ResourceKey }

// TopLevelResource is a FindResources predicate matching resources without owners.
func TopLevelResource(r *Resource) bool { return len(r.OwnerRefs) == 0 }

// ResourceOfGroupKind returns a FindResources predicate matching the given group and kind.
func ResourceOfGroupKind(group, kind string) func(*Resource) bool {
	return func(r *Resource) bool { return r.Ref.Group == group && r.Ref.Kind == kind }
}

// APIResourceInfo describes one served API resource.
type APIResourceInfo struct {
	GroupKind            schema.GroupKind
	GroupVersionResource schema.GroupVersionResource
	Meta                 metav1.APIResource
}

// GroupVersionKind returns the GVK served by the resource.
func (a APIResourceInfo) GroupVersionKind() schema.GroupVersionKind {
	return a.GroupVersionResource.GroupVersion().WithKind(a.GroupKind.Kind)
}

// UnavailableReason explains why an API is not (fully) cached.
type UnavailableReason string

const (
	// ReasonListForbidden: the credentials may not list the API; it is not cached.
	ReasonListForbidden UnavailableReason = "ListForbidden"
	// ReasonWatchForbidden: the API was listed but cannot be watched; the cache relists it
	// every WatchResyncTimeout instead.
	ReasonWatchForbidden UnavailableReason = "WatchForbidden"
	// ReasonDiscoveryFailed: discovery of the group version failed (for example an
	// unavailable aggregated API server).
	ReasonDiscoveryFailed UnavailableReason = "DiscoveryFailed"
)

// UnavailableAPI records an API that the cache could not fully observe.
type UnavailableAPI struct {
	// GroupKind of the API. Kind is empty for ReasonDiscoveryFailed entries.
	GroupKind schema.GroupKind
	// Version is set for ReasonDiscoveryFailed entries.
	Version string
	// Namespace is set when the failure is specific to one namespace (namespaced mode).
	Namespace string
	Reason    UnavailableReason
	Message   string
}

// ClusterInfo holds cache statistics for one cluster.
type ClusterInfo struct {
	// Server is the API server URL.
	Server string
	// K8SVersion is the normalized server version ("v<major>.<minor>.<patch>").
	K8SVersion string
	// APIsCount is the number of watched API resources.
	APIsCount int
	// ResourcesCount is the number of cached objects.
	ResourcesCount int
	// LastCacheSyncTime is the time of the most recent full sync attempt.
	LastCacheSyncTime *time.Time
	// SyncError is the error of the most recent full sync attempt, nil from an invalidation to
	// the next attempt: the state that attempt failed on is gone.
	SyncError error
	// Synced reports whether the cache currently holds a valid, watched state.
	Synced bool
	// ActiveWatches is the number of running watch loops.
	ActiveWatches int
	// UnavailableAPIs lists APIs that could not be listed, watched or discovered.
	UnavailableAPIs []UnavailableAPI
}

// ResourceUpdatedHandler is called after an object is added, updated (newRes and oldRes set)
// or removed (newRes nil). namespaceResources is an immutable snapshot of the object's
// namespace (cluster-scoped objects share the "" namespace) taken after the batch containing
// the change was applied, so it may already reflect later changes of the same batch.
type ResourceUpdatedHandler func(newRes, oldRes *Resource, namespaceResources map[ResourceKey]*Resource)

// EventHandler is called for every watch event before it is applied. It may be invoked
// concurrently from several watch goroutines and must neither retain nor mutate un.
type EventHandler func(eventType watch.EventType, un *unstructured.Unstructured)

// ProcessEventsHandler is called after every batch of queued changes has been applied.
type ProcessEventsHandler func(duration time.Duration, processed int)

// Unsubscribe removes a previously registered handler.
type Unsubscribe func()

// ClusterCache is the read and subscription interface of a per-cluster cache.
type ClusterCache interface {
	// EnsureSynced performs a full sync if the cache was never synced, was invalidated, or
	// its last sync failed more than SyncRetryTimeout ago. A due periodic resync is started in
	// the background. Connectivity failures are reported as ErrClusterUnreachable. The sync
	// runs under the cache's own context and is shared by concurrent callers: ctx bounds only
	// the wait, so a caller that gives up does not cancel it, and its outcome is recorded for
	// later calls.
	EnsureSynced(ctx context.Context) error
	// Invalidate stops all watches and marks the cache stale; the next EnsureSynced performs
	// a full sync. Options may replace settings or the REST config.
	Invalidate(opts ...InvalidateOption)
	// GetServerVersion returns the normalized server version observed at the last sync.
	GetServerVersion() string
	// GetAPIResources returns every served (group, version, resource) supporting list and
	// watch, regardless of exclusions. The slice is a copy.
	GetAPIResources() []APIResourceInfo
	// IsNamespaced reports whether gk is namespaced; ErrUnknownGroupKind if not served.
	IsNamespaced(gk schema.GroupKind) (bool, error)
	// FindResources returns resources of namespace (all namespaces when empty) matching all
	// predicates. Predicates run without cache locks held.
	FindResources(namespace string, predicates ...func(*Resource) bool) map[ResourceKey]*Resource
	// IterateHierarchy visits each start key present in the cache and, depth first, its
	// descendants (owner references, including cluster-scoped owners of namespaced objects,
	// and inferred relations). fn returns false to skip a resource's children. Every resource
	// is passed to fn at most once; fn runs without cache locks held.
	IterateHierarchy(keys []ResourceKey, fn func(r *Resource, namespaceResources map[ResourceKey]*Resource) bool)
	// GetManagedLiveObjs returns deep copies of the live objects for which isManaged returns
	// true (top-level, manifest retained), plus the live state of each target, fetched with
	// GET when it is not available from the cache or is cached in a different API version.
	GetManagedLiveObjs(ctx context.Context, targets []*unstructured.Unstructured, isManaged func(*Resource) bool) (map[ResourceKey]*unstructured.Unstructured, error)
	// OnResourceUpdated registers a handler called after cached objects change.
	OnResourceUpdated(h ResourceUpdatedHandler) Unsubscribe
	// OnEvent registers a handler called for every raw watch event.
	OnEvent(h EventHandler) Unsubscribe
	// OnProcessEvents registers a handler called after each applied batch.
	OnProcessEvents(h ProcessEventsHandler) Unsubscribe
	// GetClusterInfo returns cache statistics.
	GetClusterInfo() ClusterInfo
	// GetOpenAPISchema returns the cluster's OpenAPI v3 component schemas, loaded lazily and
	// reloaded after API changes. The returned map is shared and must not be mutated.
	GetOpenAPISchema(ctx context.Context) (map[string]*spec.Schema, error)
	// GetGVKParser returns a structured-merge-diff type resolver built from the OpenAPI v3
	// schemas, loaded lazily and reloaded after API changes.
	GetGVKParser(ctx context.Context) (*GVKParser, error)
}
