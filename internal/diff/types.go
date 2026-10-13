package diff

import (
	"context"
	"errors"
	"log/slog"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/managedfields"
)

const (
	// AnnotationLastAppliedConfig is the client-side apply annotation used as the "original"
	// state of the three-way diff.
	AnnotationLastAppliedConfig = corev1.LastAppliedConfigAnnotation

	// DefaultManager is the field manager Patrol uses for server-side apply. It must match the
	// manager used by the sync engine, otherwise structured-merge and server-side diffs cannot
	// attribute field ownership correctly.
	DefaultManager = "patrol-controller"

	// DefaultSyncOptionsAnnotation is the per-resource sync-options annotation key. A resource
	// carrying "ServerSideApply=true" in it is diffed with the structured-merge strategy. The
	// value mirrors api/v1alpha1 (AnnotationSyncOptions); it is duplicated here because this
	// package must not import the CRD types.
	DefaultSyncOptionsAnnotation = "cd.patrol.example/sync-options"

	// DefaultCompareOptionsAnnotation is the compare-options annotation key (Application and
	// resource level). Mirrors api/v1alpha1 (AnnotationCompareOptions).
	DefaultCompareOptionsAnnotation = "cd.patrol.example/compare-options"
)

// Sentinel errors. Callers can branch on them with errors.Is.
var (
	// ErrNilObjects is returned when both config and live are nil.
	ErrNilObjects = errors.New("diff: config and live are both nil")
	// ErrLengthMismatch is returned by DiffArray when the config and live slices differ in length.
	ErrLengthMismatch = errors.New("diff: config and live lists have different lengths")
	// ErrNoDryRunner is returned when server-side diff is requested without a DryRunner.
	ErrNoDryRunner = errors.New("diff: server-side diff requires a DryRunner")
	// ErrNoGVKParser is returned when an operation needs schema information and Options.GVKParser is nil.
	ErrNoGVKParser = errors.New("diff: GVK parser is required")
	// ErrNoSchema is returned when no structured schema is known for an object's GroupVersionKind.
	ErrNoSchema = errors.New("diff: no schema for GroupVersionKind")
	// ErrNoManagedFields is returned when a server-side dry run result carries no managedFields.
	ErrNoManagedFields = errors.New("diff: predicted live state has no managedFields")
	// ErrManagerNotFound is returned when the dry-run result has no managedFields entry for the
	// configured manager.
	ErrManagerNotFound = errors.New("diff: no managed fields found for manager")
	// ErrDryRun wraps failures reported by the DryRunner.
	ErrDryRun = errors.New("diff: server-side apply dry run failed")
	// ErrInvalidIgnoreRule is returned by NewIgnoreNormalizer for rules that cannot be compiled.
	ErrInvalidIgnoreRule = errors.New("diff: invalid ignore rule")
	// ErrUnsupportedKnownType is returned for knownTypeFields entries naming an unknown type.
	ErrUnsupportedKnownType = errors.New("diff: unsupported known type")
)

// Normalizer mutates an object in place before it is compared. Implementations must be safe
// for concurrent use by multiple goroutines.
type Normalizer interface {
	Normalize(ctx context.Context, un *unstructured.Unstructured) error
}

// NormalizerFunc adapts a function to the Normalizer interface.
type NormalizerFunc func(ctx context.Context, un *unstructured.Unstructured) error

// Normalize implements Normalizer.
func (f NormalizerFunc) Normalize(ctx context.Context, un *unstructured.Unstructured) error {
	return f(ctx, un)
}

// TrustedManagersProvider is optionally implemented by a Normalizer. When Options.Normalizer
// implements it, Diff runs the managedFieldsManagers normalization before anything else:
// fields that the config would change but that are owned (per the live object's
// managedFields) by one of the returned managers are removed from both sides.
type TrustedManagersProvider interface {
	TrustedManagers(gk schema.GroupKind, name, namespace string) []string
}

// DryRunner performs a server-side apply of obj in dry-run mode with the given field manager
// and returns the object the API server would persist (including managedFields).
// Implementations must use force=true semantics (conflicts are not errors), like the sync.
type DryRunner interface {
	ServerSideApplyDryRun(ctx context.Context, obj *unstructured.Unstructured, manager string) (*unstructured.Unstructured, error)
}

// Options configures a diff. The zero value performs a legacy diff without normalization.
type Options struct {
	// Normalizer applies ignore rules, known-type normalization and similar user-configured
	// normalizations to both sides. Nil means no extra normalization.
	Normalizer Normalizer
	// IgnoreAggregatedRoles ignores rules of ClusterRoles/Roles with an aggregationRule
	// (resource.compareoptions ignoreAggregatedRoles).
	IgnoreAggregatedRoles bool
	// ServerSide enables server-side diff (compare option ServerSideDiff=true).
	ServerSide bool
	// DryRunner performs server-side apply dry runs. Required when ServerSide is true.
	DryRunner DryRunner
	// Manager is the server-side apply field manager. Empty means DefaultManager.
	Manager string
	// GVKParser provides structured schemas (built from the destination cluster's OpenAPI).
	// Required for server-side diff with IgnoreMutationWebhook; optional for structured-merge
	// diff (without it lists and maps are treated atomically).
	GVKParser *managedfields.GvkParser
	// StructuredMerge selects the structured-merge strategy for every resource (the
	// application syncs with ServerSideApply=true). Individual resources can also opt in with
	// "ServerSideApply=true" in the sync-options annotation.
	StructuredMerge bool
	// IgnoreMutationWebhook removes changes made by mutating admission webhooks from the
	// server-side diff result. The baseline default is true; set it to false only when the
	// Application has the compare option IncludeMutationWebhook=true. See DefaultOptions.
	IgnoreMutationWebhook bool
	// SyncOptionsAnnotation overrides the per-resource sync-options annotation key. Empty
	// means DefaultSyncOptionsAnnotation.
	SyncOptionsAnnotation string
	// Log receives debug/error diagnostics. Nil discards them. Secret values are never logged.
	Log *slog.Logger
}

// DefaultOptions returns Options with the baseline defaults: field manager DefaultManager and
// mutation-webhook changes ignored in server-side diffs.
func DefaultOptions() Options {
	return Options{Manager: DefaultManager, IgnoreMutationWebhook: true}
}

func (o Options) withDefaults() Options {
	if o.Manager == "" {
		o.Manager = DefaultManager
	}
	if o.SyncOptionsAnnotation == "" {
		o.SyncOptionsAnnotation = DefaultSyncOptionsAnnotation
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	return o
}

// Result is the outcome of comparing one config/live pair.
type Result struct {
	// Modified reports whether live differs from the predicted live state.
	Modified bool
	// NormalizedLive is the JSON of the live object after normalization ("null" if absent).
	NormalizedLive []byte
	// PredictedLive is the JSON of the live object expected after applying config ("null"
	// if the resource would not exist).
	PredictedLive []byte
}

// ResultList is the outcome of DiffArray.
type ResultList struct {
	Diffs    []Result
	Modified bool
}
