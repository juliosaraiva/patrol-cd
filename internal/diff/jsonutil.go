package diff

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// trimToShape returns a copy of live that only keeps the map keys present in shape, recursing
// into nested maps and lists (lists are matched by index; extra live elements are kept so
// they still show up as differences). It is used to drop defaulted/server-populated fields.
func trimToShape(shape, live map[string]any) map[string]any {
	out := make(map[string]any, len(shape))
	for k, sv := range shape {
		lv, ok := live[k]
		if !ok {
			continue
		}
		if lv != nil {
			lv = trimValue(sv, lv)
		}
		out[k] = lv
	}
	return out
}

func trimValue(shape, live any) any {
	switch s := shape.(type) {
	case map[string]any:
		if l, ok := live.(map[string]any); ok {
			return trimToShape(s, l)
		}
	case []any:
		if l, ok := live.([]any); ok {
			return trimList(s, l)
		}
	}
	return live
}

func trimList(shape, live []any) []any {
	out := make([]any, 0, len(live))
	for i, lv := range live {
		if i < len(shape) && lv != nil {
			lv = trimValue(shape[i], lv)
		}
		out = append(out, lv)
	}
	return out
}

// decodeJSON decodes JSON into the value types used by unstructured objects: integral
// numbers become int64, other numbers float64.
func decodeJSON(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("decode json: %w", err)
	}
	if dec.More() {
		return nil, errors.New("decode json: trailing data")
	}
	return fromJSONNumbers(v), nil
}

func fromJSONNumbers(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			t[k] = fromJSONNumbers(child)
		}
		return t
	case []any:
		for i, child := range t {
			t[i] = fromJSONNumbers(child)
		}
		return t
	case json.Number:
		if i, err := strconv.ParseInt(string(t), 10, 64); err == nil {
			return i
		}
		if f, err := strconv.ParseFloat(string(t), 64); err == nil {
			return f
		}
		return string(t)
	default:
		return v
	}
}

// decodeObject decodes a JSON object into an unstructured map.
func decodeObject(data []byte) (map[string]any, error) {
	v, err := decodeJSON(data)
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("decode json: expected object, got %T", v)
	}
	return m, nil
}

// canonicalJSON re-encodes JSON so that semantically equal documents produce identical bytes
// (sorted keys, normalized number formatting).
func canonicalJSON(data []byte) ([]byte, error) {
	v, err := decodeJSON(data)
	if err != nil {
		return nil, err
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode json: %w", err)
	}
	return out, nil
}

// roundTrip returns a deep copy of obj whose values have unstructured JSON types (int64,
// float64, string, bool, nil, []any, map[string]any). It strips Go type information that
// would otherwise cause false differences (e.g. int vs float64).
func roundTrip(obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	data, err := json.Marshal(obj.Object)
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", describe(obj), err)
	}
	m, err := decodeObject(data)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", describe(obj), err)
	}
	return &unstructured.Unstructured{Object: m}, nil
}

// toJQValue converts unstructured values into the value types gojq understands.
func toJQValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, child := range t {
			out[k] = toJQValue(child)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, child := range t {
			out[i] = toJQValue(child)
		}
		return out
	case nil, bool, string, int, float64:
		return v
	case int64:
		return int(t)
	case int32:
		return int(t)
	case float32:
		return float64(t)
	default:
		// Any other Go type (not produced by JSON decoding) is converted through JSON so the
		// evaluator only ever sees the value types it supports.
		data, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprint(v)
		}
		decoded, err := decodeJSON(data)
		if err != nil {
			return string(data)
		}
		return toJQValue(decoded)
	}
}

// fromJQValue converts gojq output values back into unstructured value types.
func fromJQValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, child := range t {
			out[k] = fromJQValue(child)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, child := range t {
			out[i] = fromJQValue(child)
		}
		return out
	case int:
		return int64(t)
	case float64:
		if t == math.Trunc(t) && t >= math.MinInt64 && t < math.MaxInt64 {
			return int64(t)
		}
		return t
	case *big.Int:
		if t.IsInt64() {
			return t.Int64()
		}
		f, _ := new(big.Float).SetInt(t).Float64()
		return f
	default:
		return v
	}
}

func describe(obj *unstructured.Unstructured) string {
	if obj == nil {
		return "<nil>"
	}
	gvk := obj.GroupVersionKind()
	name := obj.GetName()
	if ns := obj.GetNamespace(); ns != "" {
		name = ns + "/" + name
	}
	if gvk.Group == "" {
		return gvk.Kind + " " + name
	}
	return gvk.Kind + "." + gvk.Group + " " + name
}
