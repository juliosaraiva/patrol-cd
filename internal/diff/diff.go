package diff

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/jsonmergepatch"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/client-go/kubernetes/scheme"
)

// nullJSON is the representation of an absent object in Result.
var nullJSON = []byte("null")

// Diff compares the desired config with the live object. Either may be nil (resource to be
// created, or resource not in the desired state); both nil is an error. The inputs are never
// modified.
//
// Strategy selection follows the baseline:
//  1. Options.ServerSide: server-side diff (dry run) when both objects exist; create/delete
//     falls back to a local comparison without calling the API server.
//  2. Options.StructuredMerge, or ServerSideApply=true in the config's sync-options
//     annotation: structured-merge diff.
//  3. Otherwise a three-way diff against the live last-applied-configuration annotation; if
//     the annotation is missing, unparsable, or the three-way diff fails, a two-way diff that
//     uses config as the original state.
func Diff(ctx context.Context, config, live *unstructured.Unstructured, o Options) (*Result, error) {
	if config == nil && live == nil {
		return nil, ErrNilObjects
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("diff: %w", err)
	}
	o = o.withDefaults()

	var err error
	// managedFieldsManagers: drop fields owned by trusted managers before anything else.
	if config != nil && live != nil {
		if config, live, err = normalizeTrustedManagers(config, live, o); err != nil {
			return nil, err
		}
	}
	if config != nil {
		if config, err = remarshal(config, o.Log); err != nil {
			return nil, err
		}
	}
	if live != nil {
		if live, err = remarshal(live, o.Log); err != nil {
			return nil, err
		}
	}

	// Server-side diff applies the user normalizer only after the dry run, so the config sent
	// to the API server is complete.
	normalizeObject(ctx, config, o, o.ServerSide)
	normalizeObject(ctx, live, o, o.ServerSide)

	if o.ServerSide {
		if config == nil || live == nil {
			return createOrDeleteResult(config, live)
		}
		res, err := serverSideDiff(ctx, config, live, o)
		if err != nil {
			return nil, fmt.Errorf("server-side diff of %s: %w", describe(config), err)
		}
		return res, nil
	}

	if o.StructuredMerge || (config != nil && HasAnnotationOption(config.GetAnnotations(), o.SyncOptionsAnnotation, "ServerSideApply=true")) {
		res, err := StructuredMergeDiff(config, live, o.GVKParser, o.Manager)
		if err != nil {
			return nil, fmt.Errorf("structured merge diff of %s: %w", describe(config), err)
		}
		return res, nil
	}

	orig, err := LastAppliedConfig(live)
	switch {
	case err != nil:
		o.Log.Debug("ignoring unparsable last-applied-configuration", "object", describe(live))
	case orig != nil && config != nil:
		normalizeObject(ctx, orig, o, false)
		res, err := ThreeWayDiff(orig, config, live)
		if err == nil {
			return res, nil
		}
		o.Log.Debug("three-way diff failed; falling back to two-way diff", "object", describe(live), "error", sanitizeError(live, err))
	}
	return TwoWayDiff(config, live)
}

// DiffArray diffs configs[i] against lives[i] for every i. The slices must have equal length.
func DiffArray(ctx context.Context, configs, lives []*unstructured.Unstructured, o Options) (*ResultList, error) {
	if len(configs) != len(lives) {
		return nil, ErrLengthMismatch
	}
	list := &ResultList{Diffs: make([]Result, len(configs))}
	for i := range configs {
		res, err := Diff(ctx, configs[i], lives[i], o)
		if err != nil {
			return nil, fmt.Errorf("resource %d: %w", i, err)
		}
		list.Diffs[i] = *res
		if res.Modified {
			list.Modified = true
		}
	}
	return list, nil
}

// TwoWayDiff is a three-way diff that uses config as the last-applied state.
func TwoWayDiff(config, live *unstructured.Unstructured) (*Result, error) {
	if config != nil && live != nil {
		return ThreeWayDiff(config, config.DeepCopy(), live)
	}
	return createOrDeleteResult(config, live)
}

// ThreeWayDiff computes the predicted live state by applying the three-way merge patch
// (orig → config, relative to live) to live. Fields that exist in live but in neither orig
// nor config are preserved, so server-populated and defaulted fields are not drift. Built-in
// kinds use strategic-merge semantics (merge keys, retainKeys) and API defaults; other kinds
// use JSON merge patch semantics.
func ThreeWayDiff(orig, config, live *unstructured.Unstructured) (*Result, error) {
	if orig == nil || config == nil || live == nil {
		return nil, errors.New("diff: three-way diff needs orig, config and live")
	}
	orig = withoutNamespaceAndEmptyAnnotations(orig)
	config = withoutNamespaceAndEmptyAnnotations(config)

	patch, newTyped, err := threeWayMergePatch(orig, config, live)
	if err != nil {
		return nil, err
	}
	liveBytes, err := json.Marshal(live.Object)
	if err != nil {
		return nil, fmt.Errorf("encode live state: %w", err)
	}
	var predicted []byte
	if newTyped != nil {
		liveBytes, predicted, err = applyPatchWithDefaults(liveBytes, patch, newTyped)
		if err != nil {
			return nil, err
		}
	} else {
		predicted, err = jsonpatch.MergePatch(liveBytes, patch)
		if err != nil {
			return nil, fmt.Errorf("apply merge patch to live state: %w", err)
		}
		if predicted, err = canonicalJSON(predicted); err != nil {
			return nil, err
		}
		if liveBytes, err = canonicalJSON(liveBytes); err != nil {
			return nil, err
		}
	}
	return newResult(predicted, liveBytes), nil
}

// withoutNamespaceAndEmptyAnnotations drops metadata.namespace (present in live objects but
// often absent from manifests) and a null/empty annotations map.
func withoutNamespaceAndEmptyAnnotations(obj *unstructured.Unstructured) *unstructured.Unstructured {
	obj = obj.DeepCopy()
	md, ok := obj.Object["metadata"].(map[string]any)
	if !ok {
		return obj
	}
	delete(md, "namespace")
	if a, ok := md["annotations"]; ok {
		if a == nil {
			delete(md, "annotations")
		} else if m, isMap := a.(map[string]any); isMap && len(m) == 0 {
			delete(md, "annotations")
		}
	}
	return obj
}

func threeWayMergePatch(orig, config, live *unstructured.Unstructured) ([]byte, func() (runtime.Object, error), error) {
	origBytes, err := json.Marshal(orig.Object)
	if err != nil {
		return nil, nil, fmt.Errorf("encode original state: %w", err)
	}
	configBytes, err := json.Marshal(config.Object)
	if err != nil {
		return nil, nil, fmt.Errorf("encode config: %w", err)
	}
	gvk := orig.GroupVersionKind()
	if typedObj, err := scheme.Scheme.New(gvk); err == nil {
		if (gvk.Group == "apps" || gvk.Group == "extensions") && gvk.Kind == "StatefulSet" {
			live = trimVolumeClaimTemplates(orig, live)
		}
		liveBytes, err := json.Marshal(live.Object)
		if err != nil {
			return nil, nil, fmt.Errorf("encode live state: %w", err)
		}
		meta, err := strategicpatch.NewPatchMetaFromStruct(typedObj)
		if err != nil {
			return nil, nil, fmt.Errorf("build patch metadata for %s: %w", gvk, err)
		}
		patch, err := strategicpatch.CreateThreeWayMergePatch(origBytes, configBytes, liveBytes, meta, true)
		if err != nil {
			return nil, nil, fmt.Errorf("create three-way strategic merge patch: %w", err)
		}
		return patch, func() (runtime.Object, error) { return scheme.Scheme.New(gvk) }, nil
	}
	// Unknown kind: drop live fields that the original never had (defaults, controller
	// fields), then compute a JSON merge patch.
	trimmed := trimToShape(orig.Object, live.Object)
	liveBytes, err := json.Marshal(trimmed)
	if err != nil {
		return nil, nil, fmt.Errorf("encode live state: %w", err)
	}
	patch, err := jsonmergepatch.CreateThreeWayJSONMergePatch(origBytes, configBytes, liveBytes)
	if err != nil {
		return nil, nil, fmt.Errorf("create three-way JSON merge patch: %w", err)
	}
	return patch, nil, nil
}

// trimVolumeClaimTemplates removes API-server-defaulted fields from the live StatefulSet's
// volumeClaimTemplates: the embedded PVCs receive defaults that cannot be reproduced locally.
func trimVolumeClaimTemplates(orig, live *unstructured.Unstructured) *unstructured.Unstructured {
	origTemplates, ok, err := unstructured.NestedSlice(orig.Object, "spec", "volumeClaimTemplates")
	if !ok || err != nil {
		return live
	}
	liveTemplates, ok, err := unstructured.NestedSlice(live.Object, "spec", "volumeClaimTemplates")
	if !ok || err != nil {
		return live
	}
	live = live.DeepCopy()
	_ = unstructured.SetNestedField(live.Object, trimList(origTemplates, liveTemplates), "spec", "volumeClaimTemplates")
	return live
}

// applyPatchWithDefaults applies the strategic merge patch to live and then applies the API
// defaults of the typed object to both predicted and live, the way the API server would.
// Defaults are computed on the typed object and applied as a patch to the untyped JSON so
// that fields unknown to the Go type are preserved.
func applyPatchWithDefaults(liveBytes, patch []byte, newTyped func() (runtime.Object, error)) ([]byte, []byte, error) {
	schemaObj, err := newTyped()
	if err != nil {
		return nil, nil, fmt.Errorf("new typed object: %w", err)
	}
	predicted, err := strategicpatch.StrategicMergePatch(liveBytes, patch, schemaObj)
	if err != nil {
		return nil, nil, fmt.Errorf("apply strategic merge patch to live state: %w", err)
	}
	if predicted, err = withTypedDefaults(predicted, newTyped); err != nil {
		return nil, nil, fmt.Errorf("default predicted state: %w", err)
	}
	if liveBytes, err = withTypedDefaults(liveBytes, newTyped); err != nil {
		return nil, nil, fmt.Errorf("default live state: %w", err)
	}
	return liveBytes, predicted, nil
}

// withTypedDefaults decodes data into a fresh typed object; when that succeeds the API
// defaults are applied through a two-way strategic merge patch. The result is canonical JSON.
func withTypedDefaults(data []byte, newTyped func() (runtime.Object, error)) ([]byte, error) {
	obj, err := newTyped()
	if err != nil {
		return nil, fmt.Errorf("new typed object: %w", err)
	}
	if err := json.Unmarshal(data, obj); err == nil {
		patch, err := defaultsPatch(obj)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(patch, []byte("{}")) {
			if data, err = strategicPatch(data, patch, obj); err != nil {
				return nil, err
			}
		}
	}
	return canonicalJSON(data)
}

// strategicPatch applies a defaults patch. When the patch introduces a whole structure that
// data does not have, the strategic merge copies the patch value verbatim, including
// directives such as $retainKeys; they are removed so they never surface as fields. Built-in
// Kubernetes types have no field names starting with "$".
func strategicPatch(data, patch []byte, schemaObj runtime.Object) ([]byte, error) {
	out, err := strategicpatch.StrategicMergePatch(data, patch, schemaObj.DeepCopyObject())
	if err != nil {
		return nil, fmt.Errorf("apply defaults patch: %w", err)
	}
	if !bytes.Contains(out, []byte(`"$`)) {
		return out, nil
	}
	m, err := decodeObject(out)
	if err != nil {
		return nil, err
	}
	stripPatchDirectives(m)
	out, err = json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("encode defaulted object: %w", err)
	}
	return out, nil
}

// stripPatchDirectives removes strategic-merge-patch directive keys ($retainKeys, $patch,
// $setElementOrder/..., $deleteFromPrimitiveList/...) from v in place.
func stripPatchDirectives(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if strings.HasPrefix(k, "$") {
				delete(t, k)
				continue
			}
			stripPatchDirectives(child)
		}
	case []any:
		for _, child := range t {
			stripPatchDirectives(child)
		}
	}
}

// defaultsPatch returns the strategic merge patch that turns obj into its defaulted form.
func defaultsPatch(obj runtime.Object) ([]byte, error) {
	defaulted := obj.DeepCopyObject()
	applyDefaults(defaulted)
	before, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("encode object: %w", err)
	}
	after, err := json.Marshal(defaulted)
	if err != nil {
		return nil, fmt.Errorf("encode defaulted object: %w", err)
	}
	patch, err := strategicpatch.CreateTwoWayMergePatch(before, after, obj.DeepCopyObject())
	if err != nil {
		return nil, fmt.Errorf("create defaults patch: %w", err)
	}
	return patch, nil
}

// LastAppliedConfig parses the last-applied-configuration annotation of live. It returns
// (nil, nil) when live is nil or has no annotation.
func LastAppliedConfig(live *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if live == nil {
		return nil, nil
	}
	s, ok := live.GetAnnotations()[AnnotationLastAppliedConfig]
	if !ok {
		return nil, nil
	}
	m, err := decodeObject([]byte(s))
	if err != nil {
		// The error text can quote the annotation (and therefore Secret values): do not wrap it.
		return nil, fmt.Errorf("parse %s of %s: invalid JSON", AnnotationLastAppliedConfig, describe(live))
	}
	return &unstructured.Unstructured{Object: m}, nil
}

// createOrDeleteResult handles the case where exactly one of config and live exists.
func createOrDeleteResult(config, live *unstructured.Unstructured) (*Result, error) {
	switch {
	case config != nil && live != nil:
		return nil, errors.New("diff: expected config or live to be nil")
	case live != nil:
		data, err := json.Marshal(live.Object)
		if err != nil {
			return nil, fmt.Errorf("encode live state: %w", err)
		}
		// A live object without config is not a difference by itself; the caller decides
		// (pruning, IgnoreExtraneous).
		return &Result{Modified: false, NormalizedLive: data, PredictedLive: nullJSON}, nil
	case config != nil:
		data, err := json.Marshal(config.Object)
		if err != nil {
			return nil, fmt.Errorf("encode config: %w", err)
		}
		return &Result{Modified: true, NormalizedLive: nullJSON, PredictedLive: data}, nil
	default:
		return nil, ErrNilObjects
	}
}

func newResult(predicted, live []byte) *Result {
	return &Result{
		Modified:       !bytes.Equal(live, predicted),
		NormalizedLive: live,
		PredictedLive:  predicted,
	}
}
