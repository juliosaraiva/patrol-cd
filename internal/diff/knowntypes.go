package diff

//go:generate go run ./internal/gen/knowntypes -alias corev1 -pkg k8s.io/api/core/v1 -out knowntypes_corev1.go -docs testdata/known_types.txt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// knownTypes maps a knownTypeFields type name to a constructor of the Go type whose JSON
// encoding normalizes the field. The core/v1 entries are generated (knowntypes_corev1.go).
var knownTypes = map[string]func() any{
	"core/Quantity":    func() any { return &resource.Quantity{} },
	"meta/v1/Duration": func() any { return &metav1.Duration{} },
}

// KnownTypeField declares that a field of a custom resource holds a built-in Kubernetes type
// (resource.customizations.knownTypeFields), e.g. {Field: "spec.template.spec",
// Type: "core/v1/PodSpec"}. Field is a dot-separated path; lists along the path are
// traversed element-wise.
type KnownTypeField struct {
	Field string
	Type  string
}

// SupportedKnownTypes returns the sorted type names accepted in KnownTypeField.Type.
func SupportedKnownTypes() []string {
	out := make([]string, 0, len(knownTypes))
	for k := range knownTypes {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

type knownTypeField struct {
	path    []string
	newType func() any
}

// KnownTypesNormalizer re-encodes configured custom resource fields through the Go types
// of the Kubernetes API, so that CRDs embedding e.g. a PodSpec do not show drift caused by
// equivalent encodings (cpu: 0.1 vs 100m). It is safe for concurrent use.
type KnownTypesNormalizer struct {
	fields map[schema.GroupKind][]knownTypeField
}

// NewKnownTypesNormalizer builds a normalizer from per-group/kind field declarations. Like
// the baseline, argoproj.io/Rollout spec.template.spec is pre-configured as core/v1/PodSpec
// unless fields already configure that kind. Entries naming an unsupported type are skipped;
// the returned error (wrapping ErrUnsupportedKnownType) lists them, while the normalizer is
// still returned and usable.
func NewKnownTypesNormalizer(fields map[schema.GroupKind][]KnownTypeField) (*KnownTypesNormalizer, error) {
	n := &KnownTypesNormalizer{fields: map[schema.GroupKind][]knownTypeField{}}
	var errs []error
	for gk, fs := range fields {
		for _, f := range fs {
			newType, ok := knownTypes[f.Type]
			if !ok {
				errs = append(errs, fmt.Errorf("%w: %q for %s field %q", ErrUnsupportedKnownType, f.Type, gk, f.Field))
				continue
			}
			n.fields[gk] = append(n.fields[gk], knownTypeField{path: strings.Split(f.Field, "."), newType: newType})
		}
	}
	rollout := schema.GroupKind{Group: "argoproj.io", Kind: "Rollout"}
	if _, ok := n.fields[rollout]; !ok {
		n.fields[rollout] = []knownTypeField{{
			path:    []string{"spec", "template", "spec"},
			newType: func() any { return &corev1.PodSpec{} },
		}}
	}
	return n, errors.Join(errs...)
}

// Normalize implements Normalizer.
func (n *KnownTypesNormalizer) Normalize(_ context.Context, un *unstructured.Unstructured) error {
	if un == nil || un.Object == nil {
		return nil
	}
	for _, f := range n.fields[un.GroupVersionKind().GroupKind()] {
		if err := normalizeKnownField(un.Object, f, f.path); err != nil {
			return fmt.Errorf("known type field %s: %w", strings.Join(f.path, "."), err)
		}
	}
	return nil
}

// normalizeKnownField re-encodes the value at path. When a prefix of path is a list, the
// remainder of the path is applied to every object element of the list.
func normalizeKnownField(obj map[string]any, f knownTypeField, path []string) error {
	for i := range path {
		v, ok, err := unstructured.NestedFieldNoCopy(obj, path[:i+1]...)
		if err != nil || !ok {
			continue
		}
		items, isList := v.([]any)
		if !isList {
			continue
		}
		rest := path[i+1:]
		for j := range items {
			item, isMap := items[j].(map[string]any)
			if !isMap {
				continue
			}
			if len(rest) == 0 {
				re, err := reencode(item, f.newType)
				if err != nil {
					return err
				}
				items[j] = re
				continue
			}
			if err := normalizeKnownField(item, f, rest); err != nil {
				return err
			}
		}
		return unstructured.SetNestedSlice(obj, items, path[:i+1]...)
	}
	v, ok, err := unstructured.NestedFieldNoCopy(obj, path...)
	if err != nil || !ok {
		return nil //nolint:nilerr // a missing or non-traversable path is not an error (baseline)
	}
	re, err := reencode(v, f.newType)
	if err != nil {
		return err
	}
	return unstructured.SetNestedField(obj, re, path...)
}

func reencode(v any, newType func() any) (any, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}
	typedValue := newType()
	if err := json.Unmarshal(data, typedValue); err != nil {
		return nil, fmt.Errorf("decode as %T: %w", typedValue, err)
	}
	data, err = json.Marshal(typedValue)
	if err != nil {
		return nil, fmt.Errorf("encode %T: %w", typedValue, err)
	}
	return decodeJSON(data)
}

// Chain returns a Normalizer applying normalizers in order; nil entries are skipped. The
// first error stops the chain, and so does a done context (the chain then returns an error
// wrapping ctx.Err()). If any element provides trusted managers, so does the chain.
func Chain(normalizers ...Normalizer) Normalizer {
	var ns []Normalizer
	for _, n := range normalizers {
		if n != nil {
			ns = append(ns, n)
		}
	}
	return chain(ns)
}

type chain []Normalizer

func (c chain) Normalize(ctx context.Context, un *unstructured.Unstructured) error {
	for _, n := range c {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("diff: normalization of %s interrupted: %w", describe(un), err)
		}
		if err := n.Normalize(ctx, un); err != nil {
			return err
		}
	}
	return nil
}

func (c chain) TrustedManagers(gk schema.GroupKind, name, namespace string) []string {
	var out []string
	for _, n := range c {
		p, ok := n.(TrustedManagersProvider)
		if !ok {
			continue
		}
		for _, m := range p.TrustedManagers(gk, name, namespace) {
			if !slices.Contains(out, m) {
				out = append(out, m)
			}
		}
	}
	return out
}

// NormalizerConfig describes the user and system normalization settings of an application.
type NormalizerConfig struct {
	// Rules are the application ignoreDifferences plus system-level overrides (see
	// SystemIgnoreRules).
	Rules []IgnoreRule
	// KnownTypeFields are the system-level knownTypeFields per group/kind.
	KnownTypeFields map[schema.GroupKind][]KnownTypeField
	// Ignore configures jq evaluation and logging.
	Ignore IgnoreNormalizerOptions
}

// NewNormalizer builds the standard diff normalizer: ignore rules followed by known-type
// normalization (the baseline order). Unsupported known types are reported in the error
// (wrapping ErrUnsupportedKnownType) while the normalizer is still returned; an invalid
// ignore rule is fatal and returns a nil normalizer.
func NewNormalizer(cfg NormalizerConfig) (Normalizer, error) {
	ignore, err := NewIgnoreNormalizer(cfg.Rules, cfg.Ignore)
	if err != nil {
		return nil, err
	}
	known, kerr := NewKnownTypesNormalizer(cfg.KnownTypeFields)
	return Chain(ignore, known), kerr
}
