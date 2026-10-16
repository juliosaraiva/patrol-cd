package diff

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"slices"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// AnnotationOptions splits a comma-separated option annotation (sync-options,
// compare-options) into trimmed, de-duplicated, non-empty items in order of appearance.
func AnnotationOptions(annotations map[string]string, key string) []string {
	raw, ok := annotations[key]
	if !ok {
		return nil
	}
	var out []string
	for item := range strings.SplitSeq(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" || slices.Contains(out, item) {
			continue
		}
		out = append(out, item)
	}
	return out
}

// HasAnnotationOption reports whether the option annotation key contains exactly value.
func HasAnnotationOption(annotations map[string]string, key, value string) bool {
	return slices.Contains(AnnotationOptions(annotations, key), value)
}

// CompareAnnotation is the parsed value of the compare-options annotation.
type CompareAnnotation struct {
	// IgnoreExtraneous (resource level): the resource does not affect the application's
	// sync status when it exists live but not in the desired state.
	IgnoreExtraneous bool
	// ServerSideDiff (application level): nil when unset, otherwise the explicit
	// ServerSideDiff=true/false choice.
	ServerSideDiff *bool
	// IncludeMutationWebhook (application level): IncludeMutationWebhook=true.
	IncludeMutationWebhook bool
}

// ParseCompareAnnotation parses the compare-options annotation (key normally
// DefaultCompareOptionsAnnotation). Unknown items are ignored, like the baseline.
func ParseCompareAnnotation(annotations map[string]string, key string) CompareAnnotation {
	var c CompareAnnotation
	opts := AnnotationOptions(annotations, key)
	c.IgnoreExtraneous = slices.Contains(opts, "IgnoreExtraneous")
	c.IncludeMutationWebhook = slices.Contains(opts, "IncludeMutationWebhook=true")
	// Explicit disable wins over enable, as in the baseline.
	if slices.Contains(opts, "ServerSideDiff=true") {
		v := true
		c.ServerSideDiff = &v
	}
	if slices.Contains(opts, "ServerSideDiff=false") {
		v := false
		c.ServerSideDiff = &v
	}
	return c
}

// ServerSideDiffEnabled combines the controller-wide setting (controller.diff.server.side)
// with the application's compare-options annotation.
func (c CompareAnnotation) ServerSideDiffEnabled(controllerDefault bool) bool {
	if c.ServerSideDiff != nil {
		return *c.ServerSideDiff
	}
	return controllerDefault
}

// IgnoreStatusMode is the resource.compareoptions ignoreResourceStatusField setting.
type IgnoreStatusMode string

const (
	// IgnoreStatusAll ignores .status for every resource (default).
	IgnoreStatusAll IgnoreStatusMode = "all"
	// IgnoreStatusCRD ignores .status of CustomResourceDefinitions only.
	IgnoreStatusCRD IgnoreStatusMode = "crd"
	// IgnoreStatusNone compares .status of every resource.
	IgnoreStatusNone IgnoreStatusMode = "none"
)

// ParseIgnoreStatusMode normalizes a configured value. "" means the default (all); "none",
// "off" and "false" disable status ignoring. Unknown values fall back to "all" and are
// reported with ok=false so the caller can warn.
func ParseIgnoreStatusMode(v string) (mode IgnoreStatusMode, ok bool) {
	switch strings.TrimSpace(v) {
	case "", string(IgnoreStatusAll):
		return IgnoreStatusAll, true
	case string(IgnoreStatusCRD):
		return IgnoreStatusCRD, true
	case string(IgnoreStatusNone), "off", "false":
		return IgnoreStatusNone, true
	default:
		return IgnoreStatusAll, false
	}
}

// StatusIgnoreRules returns the ignore rules implied by ignoreResourceStatusField.
func StatusIgnoreRules(mode IgnoreStatusMode) []IgnoreRule {
	switch mode {
	case IgnoreStatusNone:
		return nil
	case IgnoreStatusCRD:
		return []IgnoreRule{{Group: "apiextensions.k8s.io", Kind: "CustomResourceDefinition", JSONPointers: []string{"/status"}}}
	default:
		return []IgnoreRule{{Group: "*", Kind: "*", JSONPointers: []string{"/status"}}}
	}
}

// CompareOptions are the system-wide resource.compareoptions settings.
type CompareOptions struct {
	IgnoreAggregatedRoles              bool
	IgnoreResourceStatusField          IgnoreStatusMode
	IgnoreDifferencesOnResourceUpdates bool
}

// DefaultCompareOptions returns the baseline defaults.
func DefaultCompareOptions() CompareOptions {
	return CompareOptions{IgnoreResourceStatusField: IgnoreStatusAll, IgnoreDifferencesOnResourceUpdates: true}
}

// SystemIgnoreRules returns the rules the diff must apply on top of application rules:
// the system-level ignoreDifferences overrides plus the status rule.
func SystemIgnoreRules(overrides []IgnoreRule, opts CompareOptions) []IgnoreRule {
	out := slices.Clone(overrides)
	return append(out, StatusIgnoreRules(opts.IgnoreResourceStatusField)...)
}

// metadataNoisePointers are always ignored when deciding whether a live update is relevant.
var metadataNoisePointers = []string{"/metadata/resourceVersion", "/metadata/generation", "/metadata/managedFields"}

// ResourceUpdateIgnoreRules returns the rules used to decide whether a watch event for a
// live resource is relevant (ignoreResourceUpdates). updateRules are the system-level
// ignoreResourceUpdates overrides. When IgnoreDifferencesOnResourceUpdates is set, diffRules
// (the system-level ignoreDifferences overrides) and the /status rule implied by
// IgnoreResourceStatusField are added, as the baseline merges its status override into the
// ignoreDifferences overrides before reusing them; passing SystemIgnoreRules output as
// diffRules is equivalent (a repeated /status removal is a no-op). Application-level
// ignoreDifferences never take part. resourceVersion, generation and managedFields changes
// are always ignored. Managed fields managers have no effect on hashing (only pointers and jq
// expressions are applied), as in the baseline.
func ResourceUpdateIgnoreRules(updateRules, diffRules []IgnoreRule, opts CompareOptions) []IgnoreRule {
	out := slices.Clone(updateRules)
	if opts.IgnoreDifferencesOnResourceUpdates {
		out = append(out, diffRules...)
		out = append(out, StatusIgnoreRules(opts.IgnoreResourceStatusField)...)
	}
	return append(out, IgnoreRule{Group: "*", Kind: "*", JSONPointers: slices.Clone(metadataNoisePointers)})
}

// ResourceUpdateHash returns a hash of un after normalization with n (typically an
// IgnoreNormalizer built from ResourceUpdateIgnoreRules). Two versions of a live object with
// the same hash differ only in ignored fields, so the update need not trigger a refresh.
// un is not modified. If ctx is done once n returns, the error wraps ctx.Err().
func ResourceUpdateHash(ctx context.Context, un *unstructured.Unstructured, n Normalizer) (string, error) {
	if un == nil {
		return "", ErrNilObjects
	}
	cp, err := roundTrip(un)
	if err != nil {
		return "", err
	}
	if n != nil {
		err := n.Normalize(ctx, cp)
		// A normalizer may skip rules because ctx is done while reporting success.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("normalize %s: %w", describe(un), ctxErr)
		}
		if err != nil {
			return "", fmt.Errorf("normalize %s: %w", describe(un), err)
		}
	}
	data, err := json.Marshal(cp.Object)
	if err != nil {
		return "", fmt.Errorf("encode %s: %w", describe(un), err)
	}
	h := fnv.New64a()
	_, _ = h.Write(data)
	return strconv.FormatUint(h.Sum64(), 16), nil
}
