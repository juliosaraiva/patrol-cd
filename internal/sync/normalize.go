package sync

import (
	"context"
	"fmt"
	"reflect"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/client-go/kubernetes/scheme"

	"github.com/juliosaraiva/patrol-cd/internal/diff"
)

// normalizeTargets implements RespectIgnoreDifferences=true (SYNC-045): every target with a
// live counterpart receives the live values of the fields the ignore rules remove.
func (r *run) normalizeTargets(recs []*reconciled) error {
	for _, rec := range recs {
		if rec.target == nil || rec.live == nil {
			continue
		}
		patched, err := RespectIgnoreDifferences(r.ctx, rec.target, rec.live, r.in.Normalizer)
		if err != nil {
			return fmt.Errorf("%s: %w", rec.key, err)
		}
		rec.target = patched
	}
	return nil
}

// RespectIgnoreDifferences returns a copy of target in which the fields removed by the
// normalizer carry their live values, so a sync does not revert ignored differences. Live
// status is never copied. Built-in kinds use a strategic merge patch, other kinds a JSON
// merge patch; fields clobbered through `patchStrategy: replace` parents are restored.
func RespectIgnoreDifferences(ctx context.Context, target, live *unstructured.Unstructured, n diff.Normalizer) (*unstructured.Unstructured, error) {
	if target == nil || live == nil || n == nil {
		return target, nil
	}
	opts := diff.Options{Normalizer: n}
	normTarget := target.DeepCopy()
	if err := diff.Normalize(ctx, normTarget, opts); err != nil {
		return nil, fmt.Errorf("normalize target: %w", err)
	}
	normLive := live.DeepCopy()
	if err := diff.Normalize(ctx, normLive, opts); err != nil {
		return nil, fmt.Errorf("normalize live: %w", err)
	}

	livePatchSrc := live.DeepCopy()
	unstructured.RemoveNestedField(livePatchSrc.Object, "status")
	normLiveSrc := normLive.DeepCopy()
	unstructured.RemoveNestedField(normLiveSrc.Object, "status")

	var versioned runtime.Object
	var meta *strategicpatch.PatchMetaFromStruct
	if obj, err := scheme.Scheme.New(normTarget.GroupVersionKind()); err == nil {
		m, err := strategicpatch.NewPatchMetaFromStruct(obj)
		if err != nil {
			return nil, fmt.Errorf("patch metadata: %w", err)
		}
		versioned, meta = obj, &m
	}

	original, err := normLiveSrc.MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("marshal normalized live: %w", err)
	}
	modified, err := livePatchSrc.MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("marshal live: %w", err)
	}
	var patch []byte
	if meta != nil {
		patch, err = strategicpatch.CreateThreeWayMergePatch(modified, modified, original, meta, true)
	} else {
		patch, err = jsonpatch.CreateMergePatch(original, modified)
	}
	if err != nil {
		return nil, fmt.Errorf("compute live patch: %w", err)
	}

	targetJSON, err := normTarget.MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("marshal normalized target: %w", err)
	}
	var patchedJSON []byte
	if versioned != nil {
		patchedJSON, err = strategicpatch.StrategicMergePatch(targetJSON, patch, versioned)
	} else {
		patchedJSON, err = jsonpatch.MergePatch(targetJSON, patch)
	}
	if err != nil {
		return nil, fmt.Errorf("apply live patch: %w", err)
	}
	patched := &unstructured.Unstructured{}
	if err := patched.UnmarshalJSON(patchedJSON); err != nil {
		return nil, fmt.Errorf("decode patched target: %w", err)
	}
	restoreNonIgnoredFields(patched.Object, target.Object, normTarget.Object, normLive.Object)
	return patched, nil
}

// restoreNonIgnoredFields undoes collateral changes of a strategic merge patch on fields
// with patchStrategy "replace" (for example a PodDisruptionBudget selector): a field the
// normalizer left untouched in the target keeps its target value, and keys that only the
// live object has and the normalizer did not strip are removed again.
func restoreNonIgnoredFields(patched, original, normTarget, normLive map[string]any) {
	for key, origVal := range original {
		patchedVal, inPatched := patched[key]
		normVal, inNorm := normTarget[key]
		pm, pok := patchedVal.(map[string]any)
		om, ook := origVal.(map[string]any)
		nm, nok := normVal.(map[string]any)
		if inPatched && pok && ook && nok {
			var lm map[string]any
			if v, ok := normLive[key].(map[string]any); ok {
				lm = v
			}
			restoreNonIgnoredFields(pm, om, nm, lm)
			continue
		}
		if inNorm && reflect.DeepEqual(normVal, origVal) && (!inPatched || !reflect.DeepEqual(patchedVal, origVal)) {
			patched[key] = origVal
		}
	}
	for key := range patched {
		if _, ok := original[key]; ok {
			continue
		}
		if _, ok := normLive[key]; ok {
			delete(patched, key)
		}
	}
}
