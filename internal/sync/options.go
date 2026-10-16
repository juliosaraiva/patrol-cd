package sync

import (
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// Exact option strings matched at the Application/Operation level. Upstream matches these
// lists verbatim (case-sensitive, no trimming); the CRD admission rule already forbids
// whitespace.
const (
	optValidateFalse                    = v1alpha1.SyncOptionKeyValidate + "=false"
	optSkipDryRunOnMissingResource      = v1alpha1.SyncOptionKeySkipDryRunOnMissingResource + "=true"
	optCreateNamespace                  = v1alpha1.SyncOptionKeyCreateNamespace + "=true"
	optPruneLast                        = v1alpha1.SyncOptionKeyPruneLast + "=true"
	optApplyOutOfSyncOnly               = v1alpha1.SyncOptionKeyApplyOutOfSyncOnly + "=true"
	optReplace                          = v1alpha1.SyncOptionKeyReplace + "=true"
	optForce                            = v1alpha1.SyncOptionKeyForce + "=true"
	optServerSideApply                  = v1alpha1.SyncOptionKeyServerSideApply + "=true"
	optServerSideApplyFalse             = v1alpha1.SyncOptionKeyServerSideApply + "=false"
	optClientSideApplyMigrationDisabled = v1alpha1.SyncOptionKeyClientSideApplyMigration + "=false"
	optRespectIgnoreDifferences         = v1alpha1.SyncOptionKeyRespectIgnoreDifferences + "=true"
	optFailOnSharedResource             = v1alpha1.SyncOptionKeyFailOnSharedResource + "=true"
	optPPPBackground                    = v1alpha1.SyncOptionKeyPrunePropagationPolicy + "=" + v1alpha1.SyncOptionValueBackground
	optPPPForeground                    = v1alpha1.SyncOptionKeyPrunePropagationPolicy + "=" + v1alpha1.SyncOptionValueForeground
	optPPPOrphan                        = v1alpha1.SyncOptionKeyPrunePropagationPolicy + "=" + v1alpha1.SyncOptionValueOrphan
)

// Options are the parsed operation-level sync options (Operation spec.sync.syncOptions,
// which the API server and auto-sync fill from the Application when the request has none).
//
// Resource-level overrides from the cd.patrol.example/sync-options annotation are applied
// per task by the engine with the upstream precedence rules (see the methods below).
type Options struct {
	// Validate is false when "Validate=false" is set (schema validation Ignore).
	Validate bool
	// SkipDryRunOnMissingResource skips the dry run of resources whose kind is not served.
	SkipDryRunOnMissingResource bool
	// CreateNamespace creates the destination namespace (and applies
	// managedNamespaceMetadata).
	CreateNamespace bool
	// PruneLast moves every prune task into an implicit final wave of the Sync phase.
	PruneLast bool
	// PrunePropagationPolicy is used for prune and hook deletions (default foreground).
	PrunePropagationPolicy metav1.DeletionPropagation
	// ApplyOutOfSyncOnly skips resources whose diff reports no modification.
	ApplyOutOfSyncOnly bool
	// Replace uses replace/create instead of apply.
	Replace bool
	// ServerSideApply uses server-side apply.
	ServerSideApply bool
	// ClientSideApplyMigration migrates client-side apply field ownership before
	// server-side apply (default true; "ClientSideApplyMigration=false" disables it).
	ClientSideApplyMigration bool
	// RespectIgnoreDifferences keeps live values of ignored fields.
	RespectIgnoreDifferences bool
	// FailOnSharedResource fails the operation when a target is owned by another
	// Application.
	FailOnSharedResource bool
	// Prune is the application-level default of the "Prune=" option ("false", "confirm",
	// ...), nil when unset. A resource annotation value overrides it.
	Prune *string
	// Delete is the application-level default of the "Delete=" option, nil when unset.
	Delete *string
}

// ParseOptions parses an operation's sync options. Unknown options are ignored.
func ParseOptions(o v1alpha1.SyncOptions) Options {
	opts := Options{
		Validate:                    !o.HasOption(optValidateFalse),
		SkipDryRunOnMissingResource: o.HasOption(optSkipDryRunOnMissingResource),
		CreateNamespace:             o.HasOption(optCreateNamespace),
		PruneLast:                   o.HasOption(optPruneLast),
		PrunePropagationPolicy:      metav1.DeletePropagationForeground,
		ApplyOutOfSyncOnly:          o.HasOption(optApplyOutOfSyncOnly),
		Replace:                     o.HasOption(optReplace),
		ServerSideApply:             o.HasOption(optServerSideApply),
		ClientSideApplyMigration:    !o.HasOption(optClientSideApplyMigrationDisabled),
		RespectIgnoreDifferences:    o.HasOption(optRespectIgnoreDifferences),
		FailOnSharedResource:        o.HasOption(optFailOnSharedResource),
		Prune:                       firstOptionValue(o, v1alpha1.SyncOptionKeyPrune),
		Delete:                      firstOptionValue(o, v1alpha1.SyncOptionKeyDelete),
	}
	// Upstream checks background, foreground, orphan in that order; any other value keeps
	// the foreground default.
	switch {
	case o.HasOption(optPPPBackground):
		opts.PrunePropagationPolicy = metav1.DeletePropagationBackground
	case o.HasOption(optPPPForeground):
		opts.PrunePropagationPolicy = metav1.DeletePropagationForeground
	case o.HasOption(optPPPOrphan):
		opts.PrunePropagationPolicy = metav1.DeletePropagationOrphan
	}
	return opts
}

// firstOptionValue returns the value of the first "key=value" entry, matched by exact
// prefix like upstream's SyncOptions.GetOptionValue (v1alpha1.SyncOptions.GetOptionValue
// returns the last entry and trims; the engine keeps upstream's first-match rule).
func firstOptionValue(o v1alpha1.SyncOptions, key string) *string {
	prefix := key + "="
	for _, s := range o {
		if v, ok := strings.CutPrefix(s, prefix); ok {
			return &v
		}
	}
	return nil
}

// AnnotationOptions returns the items of a comma-separated annotation on obj: trimmed,
// empty items dropped and duplicates removed, in first-occurrence order.
func AnnotationOptions(obj *unstructured.Unstructured, key string) []string {
	if obj == nil {
		return nil
	}
	raw, ok := obj.GetAnnotations()[key]
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, item := range strings.Split(raw, ",") {
		v := strings.TrimSpace(item)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// hasSyncOption reports whether obj's sync-options annotation contains option verbatim.
func hasSyncOption(obj *unstructured.Unstructured, option string) bool {
	for _, item := range AnnotationOptions(obj, v1alpha1.AnnotationKeySyncOptions) {
		if item == option {
			return true
		}
	}
	return false
}

// syncOptionValue returns the value of the first "key=value" item of obj's sync-options
// annotation.
func syncOptionValue(obj *unstructured.Unstructured, key string) *string {
	prefix := key + "="
	for _, item := range AnnotationOptions(obj, v1alpha1.AnnotationKeySyncOptions) {
		if v, ok := strings.CutPrefix(item, prefix); ok {
			return &v
		}
	}
	return nil
}

// resourceOrDefault returns the resource annotation value of key, falling back to def.
func resourceOrDefault(obj *unstructured.Unstructured, key string, def *string) *string {
	if v := syncOptionValue(obj, key); v != nil {
		return v
	}
	return def
}

// RequiresPruneConfirmation reports whether pruning live requires confirmation: the
// resource's "Prune=" value, or the application default when the resource has none, is
// "confirm".
func (o Options) RequiresPruneConfirmation(live *unstructured.Unstructured) bool {
	v := resourceOrDefault(live, v1alpha1.SyncOptionKeyPrune, o.Prune)
	return v != nil && *v == v1alpha1.SyncOptionValueConfirm
}

// PruneDisabled reports whether pruning live is disabled ("Prune=false" on the resource,
// or as application default when the resource has no Prune option).
func (o Options) PruneDisabled(live *unstructured.Unstructured) bool {
	v := resourceOrDefault(live, v1alpha1.SyncOptionKeyPrune, o.Prune)
	return v != nil && *v == v1alpha1.SyncOptionValueFalse
}

// DeleteDisabled reports whether cascading Application deletion must leave obj in the
// cluster ("Delete=false" on the resource, or as application default) (SYNC-032).
func (o Options) DeleteDisabled(obj *unstructured.Unstructured) bool {
	v := resourceOrDefault(obj, v1alpha1.SyncOptionKeyDelete, o.Delete)
	return v != nil && *v == v1alpha1.SyncOptionValueFalse
}

// RequiresDeletionConfirmation reports whether deleting obj requires an approval:
// "Delete=confirm" or "Prune=confirm" on obj, each falling back to the application default
// (SYNC-033, resource status requiresDeletionConfirmation).
func (o Options) RequiresDeletionConfirmation(obj *unstructured.Unstructured) bool {
	if obj == nil {
		return false
	}
	if v := resourceOrDefault(obj, v1alpha1.SyncOptionKeyDelete, o.Delete); v != nil && *v == v1alpha1.SyncOptionValueConfirm {
		return true
	}
	v := resourceOrDefault(obj, v1alpha1.SyncOptionKeyPrune, o.Prune)
	return v != nil && *v == v1alpha1.SyncOptionValueConfirm
}

// PruneLastFor reports whether a prune task for live runs in the final wave: application
// "PruneLast=true" or the live object's annotation.
func (o Options) PruneLastFor(live *unstructured.Unstructured) bool {
	return o.PruneLast || hasSyncOption(live, optPruneLast)
}

// ReplaceFor reports whether a resource is replaced instead of applied: the application
// option, or "Replace=true" on the target or the live object. "Replace=false" has no
// effect (upstream parity).
func (o Options) ReplaceFor(target, live *unstructured.Unstructured) bool {
	return o.Replace || hasSyncOption(target, optReplace) || hasSyncOption(live, optReplace)
}

// ForceFor reports whether a resource is applied or replaced with force: the strategy's
// force flag, or "Force=true" on the target or the live object. An application-level
// "Force=true" option is ignored (upstream parity).
func ForceFor(strategyForce bool, target, live *unstructured.Unstructured) bool {
	return strategyForce || hasSyncOption(target, optForce) || hasSyncOption(live, optForce)
}

// ServerSideApplyFor reports whether target is applied server-side: "ServerSideApply=false"
// on the target wins; otherwise the application option or "ServerSideApply=true" on the
// target.
func (o Options) ServerSideApplyFor(target *unstructured.Unstructured) bool {
	if hasSyncOption(target, optServerSideApplyFalse) {
		return false
	}
	return o.ServerSideApply || hasSyncOption(target, optServerSideApply)
}

// ValidateFor reports whether target is validated (schema validation Strict): neither the
// application nor the target says "Validate=false".
func (o Options) ValidateFor(target *unstructured.Unstructured) bool {
	return o.Validate && !hasSyncOption(target, optValidateFalse)
}

// SkipDryRunFor reports whether a missing kind may skip the dry run of target: the
// application option or "SkipDryRunOnMissingResource=true" on the target (a CRD of the kind
// in the same sync is checked by the engine).
func (o Options) SkipDryRunFor(target *unstructured.Unstructured) bool {
	return o.SkipDryRunOnMissingResource || hasSyncOption(target, optSkipDryRunOnMissingResource)
}

// DeletionApproved reports whether an Application's cd.patrol.example/deletion-approved
// annotation holds an RFC 3339 timestamp at or after since. Non-RFC 3339 values are ignored
// (SYNC-031, SYNC-033).
func DeletionApproved(annotations map[string]string, since time.Time) bool {
	v := annotations[v1alpha1.AnnotationKeyDeletionApproved]
	if v == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return false
	}
	return !t.Before(since)
}
