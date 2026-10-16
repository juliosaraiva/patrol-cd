package diff

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/managedfields"
	"k8s.io/client-go/applyconfigurations"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"
	"sigs.k8s.io/structured-merge-diff/v6/merge"
	"sigs.k8s.io/structured-merge-diff/v6/typed"
)

// objectTyper converts an unstructured object of one GroupVersionKind into a typed value.
type objectTyper interface {
	fromUnstructured(obj map[string]any) (*typed.TypedValue, error)
}

type parseableTyper struct{ pt *typed.ParseableType }

func (p parseableTyper) fromUnstructured(obj map[string]any) (*typed.TypedValue, error) {
	tv, err := p.pt.FromUnstructured(obj)
	if err != nil {
		return nil, fmt.Errorf("build typed value: %w", err)
	}
	return tv, nil
}

// staticTyper uses the schemas compiled into client-go for built-in kinds, falling back to
// the cluster schema when the static schema cannot type the object.
type staticTyper struct{ fallback *typed.ParseableType }

var staticTypeConverter = sync.OnceValue(func() managedfields.TypeConverter {
	return applyconfigurations.NewTypeConverter(scheme.Scheme)
})

func (s staticTyper) fromUnstructured(obj map[string]any) (*typed.TypedValue, error) {
	tv, err := staticTypeConverter().ObjectToTyped(&unstructured.Unstructured{Object: obj})
	if err == nil {
		return tv, nil
	}
	if s.fallback == nil {
		return nil, fmt.Errorf("build typed value: %w", err)
	}
	return parseableTyper{s.fallback}.fromUnstructured(obj)
}

// resolveTyper returns the typer for gvk. Without a parser every object is typed with the
// deduced schema (maps are granular, lists atomic). With a parser, kinds the parser does not
// know yield nil; built-in kinds prefer the client-go static schema, like the baseline.
func resolveTyper(gvk schema.GroupVersionKind, parser *managedfields.GvkParser) objectTyper {
	if parser == nil {
		return parseableTyper{&typed.DeducedParseableType}
	}
	pt := parser.Type(gvk)
	if pt == nil {
		return nil
	}
	if _, err := scheme.Scheme.New(gvk); err == nil {
		return staticTyper{fallback: pt}
	}
	return parseableTyper{pt}
}

// StructuredMergeDiff predicts the result of a server-side apply of config onto live with
// the given field manager using sigs.k8s.io/structured-merge-diff, honoring the live
// managedFields: fields previously applied by manager and no longer in config are removed
// unless another manager owns them. Without a parser lists and maps are merged atomically.
func StructuredMergeDiff(config, live *unstructured.Unstructured, parser *managedfields.GvkParser, manager string) (*Result, error) {
	config, live, err := jsonCopies(config, live)
	if err != nil {
		return nil, err
	}
	return structuredMergeDiff(config, live, parser, manager)
}

// structuredMergeDiff implements StructuredMergeDiff for JSON-typed inputs.
func structuredMergeDiff(config, live *unstructured.Unstructured, parser *managedfields.GvkParser, manager string) (*Result, error) {
	if config == nil || live == nil {
		return createOrDeleteResult(config, live)
	}
	if manager == "" {
		manager = DefaultManager
	}
	gvk := config.GroupVersionKind()
	ty := resolveTyper(gvk, parser)
	if ty == nil {
		return nil, fmt.Errorf("%w: %s", ErrNoSchema, gvk)
	}
	tvLive, err := ty.fromUnstructured(live.Object)
	if err != nil {
		return nil, fmt.Errorf("live state: %w", err)
	}
	tvConfig, err := ty.fromUnstructured(config.Object)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	managed, err := decodeManagedFields(live.GetManagedFields())
	if err != nil {
		return nil, err
	}
	managerKey, err := managerIdentifier(metav1.ManagedFieldsEntry{Manager: manager, Operation: metav1.ManagedFieldsOperationApply})
	if err != nil {
		return nil, err
	}
	updater := (&merge.UpdaterBuilder{Converter: versionConverter{parser: parser}}).BuildUpdater()
	merged, _, err := updater.Apply(tvLive, tvConfig, fieldpath.APIVersion(config.GetAPIVersion()), managed, managerKey, true)
	if err != nil {
		return nil, fmt.Errorf("apply config: %w", err)
	}
	if merged == nil {
		// Applying config is a no-op.
		data, err := json.Marshal(live.Object)
		if err != nil {
			return nil, fmt.Errorf("encode live state: %w", err)
		}
		return newResult(data, data), nil
	}
	predicted, err := typedToNormalizedJSON(merged)
	if err != nil {
		return nil, fmt.Errorf("predicted state: %w", err)
	}
	normalizedLive, err := typedToNormalizedJSON(tvLive)
	if err != nil {
		return nil, fmt.Errorf("live state: %w", err)
	}
	return newResult(predicted, normalizedLive), nil
}

// typedToNormalizedJSON converts tv to canonical JSON without the last-applied annotation and
// with API defaults applied for built-in kinds.
func typedToNormalizedJSON(tv *typed.TypedValue) ([]byte, error) {
	m, ok := tv.AsValue().Unstructured().(map[string]any)
	if !ok {
		return nil, fmt.Errorf("expected an object, got %T", tv.AsValue().Unstructured())
	}
	m = deepCopyMap(m)
	unstructured.RemoveNestedField(m, "metadata", "annotations", AnnotationLastAppliedConfig)
	data, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}
	gvk := (&unstructured.Unstructured{Object: m}).GroupVersionKind()
	obj, err := scheme.Scheme.New(gvk)
	if err != nil {
		return canonicalJSON(data)
	}
	if err := json.Unmarshal(data, obj); err != nil {
		return nil, fmt.Errorf("decode into %s: %w", gvk, err)
	}
	patch, err := defaultsPatch(obj)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(patch, []byte("{}")) {
		if data, err = strategicPatch(data, patch, obj); err != nil {
			return nil, err
		}
	}
	return canonicalJSON(data)
}

// managerIdentifier encodes a managedFields entry into the manager key used by
// structured-merge-diff, with the same rules as the API server: fieldsType, fields and time
// are never part of the key, and the API version is omitted for appliers.
func managerIdentifier(e metav1.ManagedFieldsEntry) (string, error) {
	e.FieldsType = ""
	e.FieldsV1 = nil
	e.Time = nil
	if e.Operation == metav1.ManagedFieldsOperationApply {
		e.APIVersion = ""
	}
	b, err := json.Marshal(&e)
	if err != nil {
		return "", fmt.Errorf("encode manager identifier: %w", err)
	}
	return string(b), nil
}

// decodeManagedFields converts the wire managedFields into structured-merge-diff form.
func decodeManagedFields(entries []metav1.ManagedFieldsEntry) (fieldpath.ManagedFields, error) {
	out := make(fieldpath.ManagedFields, len(entries))
	for i, e := range entries {
		switch e.Operation {
		case metav1.ManagedFieldsOperationApply, metav1.ManagedFieldsOperationUpdate:
		default:
			return nil, fmt.Errorf("managedFields[%d]: operation must be Apply or Update", i)
		}
		if e.APIVersion == "" {
			return nil, fmt.Errorf("managedFields[%d]: apiVersion must not be empty", i)
		}
		if e.FieldsType != "FieldsV1" {
			return nil, fmt.Errorf("managedFields[%d]: unsupported fieldsType %q", i, e.FieldsType)
		}
		id, err := managerIdentifier(e)
		if err != nil {
			return nil, err
		}
		set, err := fieldSetOf(e)
		if err != nil {
			return nil, fmt.Errorf("managedFields[%d] (%s): %w", i, e.Manager, err)
		}
		out[id] = fieldpath.NewVersionedSet(set, fieldpath.APIVersion(e.APIVersion), e.Operation == metav1.ManagedFieldsOperationApply)
	}
	return out, nil
}

func fieldSetOf(e metav1.ManagedFieldsEntry) (*fieldpath.Set, error) {
	set := &fieldpath.Set{}
	if e.FieldsV1 == nil {
		return set, nil
	}
	raw := e.FieldsV1.GetRawBytes()
	if len(raw) == 0 {
		return set, nil
	}
	if err := set.FromJSON(bytes.NewReader(raw)); err != nil {
		return nil, fmt.Errorf("decode fieldsV1: %w", err)
	}
	return set, nil
}

// errMissingVersion marks conversions that cannot be performed; structured-merge-diff then
// ignores managers recorded at that version, as the API server does for removed versions.
var errMissingVersion = errors.New("no schema for requested version")

// versionConverter converts typed values between API versions of the same group and kind.
// The baseline converts through the internal Kubernetes types; Patrol re-types the object
// with the target version's schema, which is exact whenever the versions share field
// names. When the target schema rejects the object the version is reported as missing.
type versionConverter struct{ parser *managedfields.GvkParser }

func (c versionConverter) Convert(obj *typed.TypedValue, version fieldpath.APIVersion) (*typed.TypedValue, error) {
	m, ok := obj.AsValue().Unstructured().(map[string]any)
	if !ok {
		return obj, errMissingVersion
	}
	apiVersion, _ := m["apiVersion"].(string)
	if apiVersion == string(version) {
		return obj, nil
	}
	if c.parser == nil {
		return obj, errMissingVersion
	}
	from, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return obj, errMissingVersion
	}
	to, err := schema.ParseGroupVersion(string(version))
	if err != nil || from.Group != to.Group {
		return obj, errMissingVersion
	}
	kind, _ := m["kind"].(string)
	pt := c.parser.Type(to.WithKind(kind))
	if pt == nil {
		return obj, errMissingVersion
	}
	cp := deepCopyMap(m)
	cp["apiVersion"] = string(version)
	tv, err := pt.FromUnstructured(cp)
	if err != nil {
		return obj, fmt.Errorf("%w: %w", errMissingVersion, err)
	}
	return tv, nil
}

func (versionConverter) IsMissingVersionError(err error) bool {
	return errors.Is(err, errMissingVersion)
}

func deepCopyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = deepCopyValue(v)
	}
	return out
}

func deepCopyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return deepCopyMap(t)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = deepCopyValue(e)
		}
		return out
	default:
		return v
	}
}
