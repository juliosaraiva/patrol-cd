package compare

import (
	"errors"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/cache"
	"github.com/juliosaraiva/patrol-cd/internal/diff"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
)

// ResourceKey identifies a resource independently of its API version. It is the key type of
// the live-state cache and of the sync engine, so comparison results can be passed on
// unchanged.
type ResourceKey = cache.ResourceKey

// Phase names reported in Result.Timings, in execution order (upstream timing checkpoints
// settings_ms, git_ms, dedup_ms, live_ms, diff_ms, sync_ms, health_ms, plus the tree).
const (
	PhaseSettings  = "settings"
	PhaseManifests = "git"
	PhaseDedup     = "dedup"
	PhaseLive      = "live"
	PhaseDiff      = "diff"
	PhaseSync      = "sync"
	PhaseHealth    = "health"
	PhaseTree      = "tree"
)

// ConditionResourcesTruncated is reported when status.resources was cut to Config.MaxResources
// entries. It is a Patrol addition (Argo CD stores an unbounded list) and is not yet declared
// in api/v1alpha1.
const (
	ConditionResourcesTruncated = "ResourcesTruncatedWarning"
	ReasonResourcesTruncated    = "ResourcesTruncated"
)

// DefaultMaxResources bounds status.resources (about 1 MiB of status at ~200 bytes per entry).
const DefaultMaxResources = 5000

// ErrRepoErrorDeferred is returned by Compare, without a Result, when manifest generation
// failed for the first time within the repo error grace period: the caller keeps the previous
// status instead of flapping to Unknown (upstream ErrCompareStateRepo).
var ErrRepoErrorDeferred = errors.New("failed to get repo objects")

// ErrInvalidInput is returned for a nil Application or AppProject.
var ErrInvalidInput = errors.New("compare: invalid input")

// Input is one comparison request (upstream CompareAppState arguments).
type Input struct {
	// App is the Application. It is not modified.
	App *v1alpha1.Application
	// Project is the effective AppProject (global projects merged, scoped clusters and
	// repositories resolved by the caller).
	Project *v1alpha1.AppProject
	// Revisions optionally overrides the revision of each source (sync and rollback). Entries
	// may be empty; a list whose length differs from the sources is ignored.
	Revisions []string
	// Sources optionally overrides the sources (sync with sources, rollback). Empty means
	// app.Spec.GetSources(): the hydrated sync source for source-hydrator Applications.
	Sources []v1alpha1.ApplicationSource
	// NoCache bypasses the repo-server manifest cache (hard refresh).
	NoCache bool
	// NoRevisionCache bypasses the repo-server revision cache; it also disables the repo
	// error grace period (upstream level-3 comparison).
	NoRevisionCache bool
	// LocalManifests replaces manifest generation with these JSON or YAML documents (sync
	// with local manifests). Refused when the project enforces source integrity.
	LocalManifests []string
	// HasMultipleSources reports a multi-source comparison (OR-ed with
	// app.Spec.HasMultipleSources(); set it for rollbacks to a multi-source history entry).
	HasMultipleSources bool
	// RollbackOrSyncOverride marks comparisons made for a sync with overridden sources or
	// revisions, or a rollback: the repo error grace period does not apply and
	// RevisionsMayHaveChanges is not evaluated.
	RollbackOrSyncOverride bool
	// LastSyncResult is the syncResult of the latest Operation of the Application (nil when
	// unknown). Its managedNamespaceMetadata detects changed managed namespace metadata
	// (upstream HasChangedManagedNamespaceMetadata).
	LastSyncResult *v1alpha1.SyncOperationResult
	// SkipTree skips building the resource tree (and the orphaned-resource condition).
	SkipTree bool
}

// Condition is an application condition found by a comparison. Several conditions can share
// a type, as Argo CD's ApplicationCondition list allows; MergeConditions folds them into the
// one-per-type list of Application.status.
type Condition struct {
	Type    string
	Reason  string
	Message string
}

// Timing is the duration of one comparison phase.
type Timing struct {
	Phase    string
	Duration time.Duration
}

// ManagedResource is one paired desired/live resource of the comparison (upstream
// managedResource plus the per-resource status fields).
type ManagedResource struct {
	// Key identifies the resource (namespace of the live object when present).
	Key ResourceKey
	// Version is the API version of the live object, else of the target.
	Version string
	// Target is the desired object (nil when the resource is extraneous).
	Target *unstructured.Unstructured
	// Live is the live object (nil when the resource is missing).
	Live *unstructured.Unstructured
	// Diff is the diff result; Diff.Modified decides OutOfSync.
	Diff diff.Result
	// Hook reports a sync hook (status left empty, excluded from sync status and health).
	Hook bool
	// SelfReferenced is false for live objects carrying a tracking id copied from another
	// object (status left empty, never pruned).
	SelfReferenced bool
	// RequiresPruning reports a managed live object without desired state.
	RequiresPruning bool
	// RequiresDeletionConfirmation reports Delete=confirm or Prune=confirm.
	RequiresDeletionConfirmation bool
	// SyncWave is the sync wave of the target (or of the matching hook target).
	SyncWave int
	// Status is the resource sync status ("" for hooks and non-self-referenced objects).
	Status v1alpha1.SyncStatusCode
	// Health is the assessed health of the resource (nil for kinds without health, hooks and
	// ignored resources).
	Health *v1alpha1.HealthStatus
	// ResourceVersion is the live object's resourceVersion.
	ResourceVersion string
}

// Result is the outcome of a comparison (upstream comparisonResult).
type Result struct {
	// SyncStatus is the sync status to write to status.sync (comparedTo, revision(s)).
	SyncStatus v1alpha1.SyncStatus
	// Health is the aggregated application health.
	Health v1alpha1.HealthStatusCode
	// ResourceHealthSource is where per-resource health is stored (inline when health is
	// persisted in status.resources, appTree otherwise).
	ResourceHealthSource v1alpha1.ResourceHealthLocation
	// Resources are the status.resources entries, sorted by group, kind, namespace and name,
	// and bounded by Config.MaxResources.
	Resources []v1alpha1.ResourceStatus
	// ResourcesTruncated is the number of entries dropped from Resources.
	ResourcesTruncated int
	// Managed are all paired resources in reconciliation order (targets first, then
	// extraneous live objects), unbounded and unmasked: never serve them without
	// MaskSecrets.
	Managed []ManagedResource
	// Conditions are the conditions found by the comparison and the tree.
	Conditions []Condition
	// EvaluatedConditionTypes are the condition types this comparison is authoritative for:
	// existing conditions of these types that are not in Conditions are resolved.
	EvaluatedConditionTypes []string
	// SourceType is the rendering tool of a single-source Application.
	SourceType v1alpha1.ApplicationSourceType
	// SourceTypes are the rendering tools of each source of a multi-source Application.
	SourceTypes []v1alpha1.ApplicationSourceType
	// Revisions are the revisions resolved by the repo-server, one per source (empty with
	// local manifests or when manifest generation failed).
	Revisions []string
	// RevisionsMayHaveChanges reports that some source revision may contain changes since
	// the synced revision (used by automated sync).
	RevisionsMayHaveChanges bool
	// HasPreDeleteHooks and HasPostDeleteHooks drive the Application deletion finalizers.
	HasPreDeleteHooks  bool
	HasPostDeleteHooks bool
	// ReconciledAt is when the comparison completed.
	ReconciledAt metav1.Time
	// Timings are the phase durations, in execution order.
	Timings []Timing
	// Summary is status.summary, computed from the resource tree (zero when no tree).
	Summary v1alpha1.ApplicationSummary
	// Tree is the resource tree (nil when skipped or when it could not be built).
	Tree *ApplicationTree
	// TreeError is the error that prevented building the tree (the comparison is still valid;
	// upstream logs it and keeps the previous summary).
	TreeError error
	// OrphanedResources is the number of orphaned resources found (0 when monitoring is off).
	OrphanedResources int

	// Cluster is the resolved destination cluster.
	Cluster *v1alpha1.Cluster
	// Targets are the normalized desired objects passed to the sync engine (PreDelete and
	// PostDelete-only hooks removed; sync hooks included).
	Targets []*unstructured.Unstructured
	// Live are the managed live objects by key, including live hooks, after project
	// filtering.
	Live map[ResourceKey]*unstructured.Unstructured
	// DiffResults maps the key of each paired resource to Diff.Modified (sync.Input).
	DiffResults map[ResourceKey]bool
	// Normalizer applies the Application's ignoreDifferences and the system overrides; pass
	// it to sync.Input.Normalizer for RespectIgnoreDifferences=true. Nil when it could not be
	// built.
	Normalizer diff.Normalizer
	// ServerSideDiff reports that server-side diff was used.
	ServerSideDiff bool
	// ManifestResponses are the repo-server responses, one per source.
	ManifestResponses []*apiclient.ManifestResponse
	// FailedToLoad reports that desired or live state could not be loaded or compared, so
	// every resource status is Unknown.
	FailedToLoad bool

	// diffOpts are the options the diffs were computed with (for MaskSecrets).
	diffOpts diff.Options
}

// Timing returns the duration of phase, or 0.
func (r *Result) Timing(phase string) time.Duration {
	for _, t := range r.Timings {
		if t.Phase == phase {
			return t.Duration
		}
	}
	return 0
}

// HasCondition reports whether the result holds a condition of type t.
func (r *Result) HasCondition(t string) bool {
	for _, c := range r.Conditions {
		if c.Type == t {
			return true
		}
	}
	return false
}

// ConditionMessages returns the messages of the conditions of type t, in order.
func (r *Result) ConditionMessages(t string) []string {
	var out []string
	for _, c := range r.Conditions {
		if c.Type == t {
			out = append(out, c.Message)
		}
	}
	return out
}
