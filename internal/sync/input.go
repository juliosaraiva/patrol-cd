package sync

import (
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/diff"
	"github.com/juliosaraiva/patrol-cd/internal/health"
)

// AppRef identifies the Application an operation belongs to.
type AppRef struct {
	// Name and Namespace of the Application.
	Name      string
	Namespace string
	// Annotations of the Application, read for cd.patrol.example/deletion-approved (prune
	// confirmation) and cd.patrol.example/client-side-apply-migration-manager.
	Annotations map[string]string
	// Destination is the resolved destination; Namespace is the default namespace of
	// namespaced resources and the namespace created by CreateNamespace=true.
	Destination v1alpha1.ApplicationDestination
}

// QualifiedName returns "<namespace>/<name>".
func (a AppRef) QualifiedName() string {
	if a.Namespace == "" {
		return a.Name
	}
	return a.Namespace + "/" + a.Name
}

// HealthFunc assesses the health of a live object; (nil, nil) means the kind has no health
// check. internal/health.Assess with internal/lua overrides satisfies it.
type HealthFunc func(obj *unstructured.Unstructured) (*health.HealthStatus, error)

// PermissionFunc checks a task's object against the AppProject (group/kind allow and deny
// lists, destination namespace for namespaced resources). namespaced is the scope reported
// by API discovery (or by the CRD manifest of the same sync). A non-nil error fails the
// task; its message becomes the task's result message (SYNC-073).
type PermissionFunc func(obj *unstructured.Unstructured, namespaced bool) error

// Input is everything one Plan, Step or Terminate call needs. The reconciler rebuilds it on
// every call from the Operation, the Application, rendered manifests and the live cache.
type Input struct {
	// OperationUID is the UID of the Operation; it seeds deterministic hook names and the
	// hook adoption marker. Required.
	OperationUID string
	// Attempt is the retry attempt (status.retryCount). Hooks left by an earlier attempt are
	// not adopted after a restart, so a retry re-runs them like upstream.
	Attempt int64
	// App is the Application.
	App AppRef
	// Revision is the resolved revision (informational, logged).
	Revision string

	// Targets are the rendered manifests at the resolved revisions, including hooks, in
	// render order (hook names depend on the position of a hook among the hooks).
	// PreDelete/PostDelete-only hooks are ignored (see PartitionTargets).
	Targets []*unstructured.Unstructured
	// Live are the live objects managed by the Application (tracking matches), including
	// hooks created by earlier syncs.
	Live map[ResourceKey]*unstructured.Unstructured
	// DiffResults maps the key of a managed resource (live namespace for cluster-scoped
	// objects) to whether its diff reported a modification; used by ApplyOutOfSyncOnly and
	// the preview. Resources without an entry count as modified.
	DiffResults map[ResourceKey]bool

	// SyncOptions are the operation's sync options.
	SyncOptions v1alpha1.SyncOptions
	// Prune enables deletion of resources that are no longer desired.
	Prune bool
	// DryRun runs every mutation as a server-side dry run.
	DryRun bool
	// Strategy is the sync strategy; nil means hook. apply skips hooks.
	Strategy *v1alpha1.SyncStrategy
	// Resources restricts the sync to these resources (selective sync; hooks are skipped).
	Resources []v1alpha1.SyncOperationResource
	// ManagedNamespaceMetadata is applied to the destination namespace with CreateNamespace.
	ManagedNamespaceMetadata *v1alpha1.ManagedNamespaceMetadata
	// TrackManagedNamespace also stamps tracking metadata (SetTracking) on the namespace
	// created by CreateNamespace. Upstream does not; enable it only when the comparison
	// synthesizes a target for the managed namespace, otherwise the namespace would be pruned.
	TrackManagedNamespace bool

	// StartedAt is when the operation started (status.startedAt); prune confirmations must
	// not be older.
	StartedAt time.Time
	// Previous are the results persisted by earlier steps (status.syncResult.resources).
	Previous []v1alpha1.ResourceResult
	// Progress is the persisted progress (status.progress).
	Progress *v1alpha1.OperationProgress

	// Permission checks every task against the project. Nil allows everything.
	Permission PermissionFunc
	// IsNamespaced reports the scope of a kind (internal/cache.Cluster.IsNamespaced). Nil
	// falls back to API discovery through Kube.
	IsNamespaced func(gk schema.GroupKind) (bool, error)
	// SetTracking stamps tracking metadata on a copy of every object before it is applied.
	// Nil leaves objects unchanged.
	SetTracking func(obj *unstructured.Unstructured) error
	// SharedWith reports the Application instance (display form, "namespace/name") that a
	// live object is tracked by when it is not this Application, for FailOnSharedResource.
	SharedWith func(live *unstructured.Unstructured) (owner string, shared bool)
	// IsSelfReferenced reports whether a live object really is the object its tracking
	// annotation names (SYNC-080). Nil treats every object as self-referenced.
	IsSelfReferenced func(live, target *unstructured.Unstructured) bool
	// Normalizer applies ignore rules (internal/diff.NewNormalizer) for
	// RespectIgnoreDifferences=true. Nil disables the target normalization.
	Normalizer diff.Normalizer
}

func (in *Input) validate() error {
	if in == nil {
		return fmt.Errorf("%w: nil input", ErrInvalidInput)
	}
	if in.OperationUID == "" {
		return fmt.Errorf("%w: OperationUID is required", ErrInvalidInput)
	}
	if in.App.Name == "" {
		return fmt.Errorf("%w: App.Name is required", ErrInvalidInput)
	}
	return nil
}

func (in *Input) strategyForce() bool {
	if in.Strategy == nil {
		return false
	}
	if in.Strategy.Apply != nil {
		return in.Strategy.Apply.Force
	}
	if in.Strategy.Hook != nil {
		return in.Strategy.Hook.Force
	}
	return false
}

func (in *Input) isApplyStrategy() bool {
	return in.Strategy != nil && in.Strategy.Apply != nil
}

// skipHooks: the apply strategy and selective syncs do not run hooks (SYNC-008, SYNC-010).
func (in *Input) skipHooks() bool {
	return in.isApplyStrategy() || len(in.Resources) > 0
}

// matchesResources reports whether key is selected by a selective sync. Group, kind and
// name must match ("*" matches any value, as documented on SyncOperationResource); an
// empty or "*" namespace matches any namespace.
func matchesResources(key ResourceKey, resources []v1alpha1.SyncOperationResource) bool {
	if len(resources) == 0 {
		return true
	}
	eq := func(pattern, v string) bool { return pattern == "*" || pattern == v }
	for _, r := range resources {
		if eq(r.Name, key.Name) && eq(r.Kind, key.Kind) && eq(r.Group, key.Group) &&
			(r.Namespace == "" || eq(r.Namespace, key.Namespace)) {
			return true
		}
	}
	return false
}
