package v1alpha1

import (
	"slices"
	"strings"
)

// SyncOptions is a list of sync options of the form "Key=Value". Application.spec and Operation
// spec.sync reject entries of another form at admission; ApplicationSet templates do not, because
// their entries may be template expressions.
// +kubebuilder:validation:MaxItems=64
// +kubebuilder:validation:items:MaxLength=256
type SyncOptions []string

// Sync option keys and well-known values (spec.syncPolicy.syncOptions, Operation spec.sync.syncOptions
// and the cd.patrol.example/sync-options resource annotation).
const (
	SyncOptionKeyValidate                    = "Validate"
	SyncOptionKeyCreateNamespace             = "CreateNamespace"
	SyncOptionKeyPrune                       = "Prune"
	SyncOptionKeyDelete                      = "Delete"
	SyncOptionKeyPruneLast                   = "PruneLast"
	SyncOptionKeyPrunePropagationPolicy      = "PrunePropagationPolicy"
	SyncOptionKeyApplyOutOfSyncOnly          = "ApplyOutOfSyncOnly"
	SyncOptionKeyReplace                     = "Replace"
	SyncOptionKeyForce                       = "Force"
	SyncOptionKeyServerSideApply             = "ServerSideApply"
	SyncOptionKeyFailOnSharedResource        = "FailOnSharedResource"
	SyncOptionKeyRespectIgnoreDifferences    = "RespectIgnoreDifferences"
	SyncOptionKeySkipDryRunOnMissingResource = "SkipDryRunOnMissingResource"
	SyncOptionKeyClientSideApplyMigration    = "ClientSideApplyMigration"

	SyncOptionValueTrue       = "true"
	SyncOptionValueFalse      = "false"
	SyncOptionValueConfirm    = "confirm"
	SyncOptionValueForeground = "foreground"
	SyncOptionValueBackground = "background"
	SyncOptionValueOrphan     = "orphan"
)

// Compare option values (cd.patrol.example/compare-options resource annotation).
const (
	CompareOptionIgnoreExtraneous       = "IgnoreExtraneous"
	CompareOptionServerSideDiff         = "ServerSideDiff"
	CompareOptionIncludeMutationWebhook = "IncludeMutationWebhook"
)

// ParseSyncOption splits a "Key=Value" option. ok is false when the option has no "=".
// Surrounding whitespace of key and value is trimmed.
func ParseSyncOption(option string) (key, value string, ok bool) {
	key, value, ok = strings.Cut(option, "=")
	return strings.TrimSpace(key), strings.TrimSpace(value), ok
}

// HasOption reports whether the list contains option verbatim (e.g. "PruneLast=true").
func (o SyncOptions) HasOption(option string) bool {
	return slices.Contains(o, option)
}

// GetOptionValue returns the value of the last "key=value" option with the given key. Later entries
// win, so callers can append overrides.
func (o SyncOptions) GetOptionValue(key string) (string, bool) {
	for i := len(o) - 1; i >= 0; i-- {
		k, v, ok := ParseSyncOption(o[i])
		if ok && k == key {
			return v, true
		}
	}
	return "", false
}

// IsOptionTrue reports whether the option with the given key is set to "true" (case-insensitive).
func (o SyncOptions) IsOptionTrue(key string) bool {
	v, ok := o.GetOptionValue(key)
	return ok && strings.EqualFold(v, SyncOptionValueTrue)
}

// AddOption returns the list with option appended unless it is already present.
func (o SyncOptions) AddOption(option string) SyncOptions {
	if o.HasOption(option) {
		return o
	}
	return append(o, option)
}

// RemoveOption returns the list without any occurrence of option.
func (o SyncOptions) RemoveOption(option string) SyncOptions {
	return slices.DeleteFunc(slices.Clone(o), func(s string) bool { return s == option })
}
