package diff

import (
	"encoding/json"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// maskUnit is the mask of the first distinct value of a key; each further distinct value
// gets four more characters. "+" is used because it is not valid in base64-decoded
// comparisons and cannot be confused with "*" glob syntax in the UI.
const maskUnit = "++++++++"

// HideSecretData returns copies of target and live (either may be nil) in which Secret
// data values, and the values of the annotations named in hideAnnotations, are replaced by
// masks. The same masking is applied inside each object's last-applied-configuration
// annotation. For every key, equal plaintext values receive equal masks and different
// values receive different masks (8, 12, 16, ... "+" characters, assigned in the order
// target, live, target last-applied, live last-applied), so a difference remains visible
// without revealing any value. stringData is folded into data first.
//
// A last-applied-configuration annotation that is not valid JSON, or that is itself listed in
// hideAnnotations, is replaced entirely by "++++++++" (fail closed).
func HideSecretData(target, live *unstructured.Unstructured, hideAnnotations map[string]bool) (*unstructured.Unstructured, *unstructured.Unstructured, error) {
	var targetLA, liveLA *unstructured.Unstructured
	var targetLAInvalid, liveLAInvalid bool
	if live != nil {
		var err error
		liveLA, err = LastAppliedConfig(live)
		liveLAInvalid = err != nil
		live = live.DeepCopy()
	}
	if target != nil {
		var err error
		targetLA, err = LastAppliedConfig(target)
		targetLAInvalid = err != nil
		target = target.DeepCopy()
	}
	objs := []*unstructured.Unstructured{target, live, targetLA, liveLA}

	dataKeys := map[string]bool{}
	for _, obj := range objs {
		if obj == nil {
			continue
		}
		NormalizeSecret(obj)
		if data, found, err := unstructured.NestedMap(obj.Object, "data"); found && err == nil {
			for k := range data {
				dataKeys[k] = true
			}
		}
	}
	if err := maskFields(objs, dataKeys, "data"); err != nil {
		return nil, nil, err
	}
	if err := maskFields(objs, hideAnnotations, "metadata", "annotations"); err != nil {
		return nil, nil, err
	}

	_, hideLastApplied := hideAnnotations[AnnotationLastAppliedConfig]
	if err := restoreLastApplied(live, liveLA, liveLAInvalid, hideLastApplied); err != nil {
		return nil, nil, err
	}
	if err := restoreLastApplied(target, targetLA, targetLAInvalid, hideLastApplied); err != nil {
		return nil, nil, err
	}
	return target, live, nil
}

// maskFields replaces, in every object, the values of keys inside the map at fields.
func maskFields(objs []*unstructured.Unstructured, keys map[string]bool, fields ...string) error {
	for k := range keys {
		next := maskUnit
		masks := map[string]string{}
		for _, obj := range objs {
			if obj == nil {
				continue
			}
			v, found, _ := unstructured.NestedFieldNoCopy(obj.Object, fields...)
			if found && v == nil {
				continue
			}
			data, _, err := unstructured.NestedMap(obj.Object, fields...)
			if err != nil {
				return fmt.Errorf("mask %s: field is not a map", strings.Join(fields, "."))
			}
			raw, ok := data[k]
			if !ok {
				continue
			}
			plain := maskKey(raw)
			mask, seen := masks[plain]
			if !seen {
				mask = next
				next += "++++"
				masks[plain] = mask
			}
			data[k] = mask
			if err := unstructured.SetNestedField(obj.Object, data, fields...); err != nil {
				return fmt.Errorf("mask %s: %w", strings.Join(fields, "."), err)
			}
		}
	}
	return nil
}

// maskKey is the identity of a value for mask assignment. Strings and non-strings never
// collide; null is treated as the empty string (as in the baseline).
func maskKey(v any) string {
	switch t := v.(type) {
	case nil:
		return "s:"
	case string:
		return "s:" + t
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("v:%#v", v)
	}
	return "j:" + string(b)
}

func restoreLastApplied(obj, lastApplied *unstructured.Unstructured, invalid, hide bool) error {
	if obj == nil || (lastApplied == nil && !invalid) {
		return nil
	}
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	if hide || invalid {
		annotations[AnnotationLastAppliedConfig] = maskUnit
	} else {
		data, err := json.Marshal(lastApplied.Object)
		if err != nil {
			return fmt.Errorf("encode masked last-applied-configuration: %w", err)
		}
		annotations[AnnotationLastAppliedConfig] = string(data)
	}
	obj.SetAnnotations(annotations)
	return nil
}

// MaskSecretResult masks Secret values in a diff result (both sides as a pair, so that
// differences stay visible). It is needed for server-side diff results, which carry the API
// server's view of the Secret. Results for other kinds are returned unchanged.
func MaskSecretResult(res *Result, hideAnnotations map[string]bool) (*Result, error) {
	if res == nil {
		return nil, nil
	}
	predicted, err := decodeResultSide(res.PredictedLive)
	if err != nil {
		return nil, fmt.Errorf("predicted state: %w", err)
	}
	live, err := decodeResultSide(res.NormalizedLive)
	if err != nil {
		return nil, fmt.Errorf("live state: %w", err)
	}
	if !isSecret(predicted) && !isSecret(live) {
		return res, nil
	}
	predicted, live, err = HideSecretData(predicted, live, hideAnnotations)
	if err != nil {
		return nil, err
	}
	out := &Result{Modified: res.Modified}
	if out.PredictedLive, err = encodeResultSide(predicted); err != nil {
		return nil, err
	}
	if out.NormalizedLive, err = encodeResultSide(live); err != nil {
		return nil, err
	}
	return out, nil
}

func isSecret(obj *unstructured.Unstructured) bool {
	return obj != nil && obj.GroupVersionKind().Group == "" && obj.GetKind() == "Secret"
}

func decodeResultSide(data []byte) (*unstructured.Unstructured, error) {
	if len(data) == 0 || string(data) == "null" {
		return nil, nil
	}
	m, err := decodeObject(data)
	if err != nil {
		return nil, err
	}
	return &unstructured.Unstructured{Object: m}, nil
}

func encodeResultSide(obj *unstructured.Unstructured) ([]byte, error) {
	if obj == nil {
		return nullJSON, nil
	}
	data, err := json.Marshal(obj.Object)
	if err != nil {
		return nil, fmt.Errorf("encode masked object: %w", err)
	}
	return data, nil
}
