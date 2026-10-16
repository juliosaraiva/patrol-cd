package generators

import (
	"fmt"
	"maps"
	"slices"
	"strconv"

	"github.com/juliosaraiva/patrol-cd/internal/appset/template"
)

// FlattenParameters flattens a parameter set into dot-separated string keys for the
// post-selector: nested map[string]any and map[string]string values are recursed, list items
// become "<key>.<index>", and leaves are formatted with %v.
func FlattenParameters(in map[string]any) (map[string]string, error) {
	flat := map[string]any{}
	flattenInto(flat, "", true, normalizeForFlatten(in))
	out := make(map[string]string, len(flat))
	for k, v := range flat {
		out[k] = fmt.Sprintf("%v", v)
	}
	return out, nil
}

// flattenObject flattens a decoded YAML/JSON object with dot keys and %v-formatted leaves
// (fasttemplate parameters of the Git files and Plugin generators).
func flattenObject(in map[string]any) map[string]any {
	flat := map[string]any{}
	flattenInto(flat, "", true, in)
	out := make(map[string]any, len(flat))
	for k, v := range flat {
		out[k] = fmt.Sprintf("%v", v)
	}
	return out
}

// flattenInto mirrors github.com/jeremywohl/flatten (DotStyle) as pinned upstream: maps and
// lists are recursed, and empty nested maps and lists are kept as leaves.
func flattenInto(out map[string]any, prefix string, top bool, v any) {
	key := func(k string) string {
		if top {
			return k
		}
		return prefix + "." + k
	}
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 0 && !top {
			out[prefix] = t
			return
		}
		for k, val := range t {
			assignFlat(out, key(k), val)
		}
	case []any:
		if len(t) == 0 && !top {
			out[prefix] = t
			return
		}
		for i, val := range t {
			assignFlat(out, key(strconv.Itoa(i)), val)
		}
	}
}

func assignFlat(out map[string]any, k string, v any) {
	switch v.(type) {
	case map[string]any, []any:
		flattenInto(out, k, false, v)
	default:
		out[k] = v
	}
}

// normalizeForFlatten converts nested map[string]string values into map[string]any so they are
// flattened (goTemplate parameters such as metadata.labels). Like upstream, typed lists
// ([]string, e.g. path.segments or PR labels) are leaves formatted with %v.
func normalizeForFlatten(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = normalizeValue(v)
	}
	return out
}

func normalizeValue(v any) any {
	switch t := v.(type) {
	case map[string]string:
		m := make(map[string]any, len(t))
		for k, s := range t {
			m[k] = s
		}
		return m
	case map[string]any:
		return normalizeForFlatten(t)
	case []any:
		l := make([]any, len(t))
		for i := range t {
			l[i] = normalizeValue(t[i])
		}
		return l
	default:
		return v
	}
}

// appendTemplatedValues renders the generator `values` against params and adds them as
// values.<key> (fasttemplate) or a "values" map[string]string (goTemplate). Values are rendered
// into a temporary map first, so they cannot reference each other.
func appendTemplatedValues(values map[string]string, params map[string]any, o template.Options) error {
	if len(values) == 0 {
		return nil
	}
	tmp := map[string]any{}
	for _, k := range sortedKeys(values) {
		result, err := template.Replace(values[k], params, o)
		if err != nil {
			return fmt.Errorf("failed to replace templated string: failed to replace templated string with rendered values: %w", err)
		}
		if o.GoTemplate {
			if tmp["values"] == nil {
				tmp["values"] = map[string]string{}
			}
			tmp["values"].(map[string]string)[k] = result
		} else {
			tmp["values."+k] = result
		}
	}
	maps.Copy(params, tmp)
	return nil
}

// combineStringMaps unions two fasttemplate parameter sets (values formatted with %v); a key
// present in both with different values is an error.
func combineStringMaps(a, b map[string]any) (map[string]any, error) {
	res := make(map[string]any, len(a)+len(b))
	for k, v := range a {
		res[k] = fmt.Sprintf("%v", v)
	}
	for k, v := range b {
		s := fmt.Sprintf("%v", v)
		if cur, ok := res[k]; ok && cur != s {
			return nil, fmt.Errorf("found duplicate key %s with different value, a: %s ,b: %s", k, cur, s)
		}
		res[k] = s
	}
	return res, nil
}

// mergeOverride deep-merges src into dst (upstream mergo.Merge with WithOverride): nested maps
// are merged key by key, every other src value replaces the dst value. Values copied from src
// are deep-copied so parameter sets never share nested maps.
func mergeOverride(dst, src map[string]any) {
	for k, sv := range src {
		dv, exists := dst[k]
		if exists {
			if merged, ok := mergeMaps(dv, sv); ok {
				dst[k] = merged
				continue
			}
		}
		dst[k] = deepCopyValue(sv)
	}
}

// mergeMaps merges two map values (map[string]any or map[string]string). It reports false when
// either value is not a map.
func mergeMaps(dst, src any) (any, bool) {
	switch d := dst.(type) {
	case map[string]string:
		switch s := src.(type) {
		case map[string]string:
			out := maps.Clone(d)
			maps.Copy(out, s)
			return out, true
		case map[string]any:
			out := normalizeValue(d).(map[string]any)
			mergeOverride(out, s)
			return out, true
		}
	case map[string]any:
		switch s := src.(type) {
		case map[string]string:
			out := deepCopyValue(d).(map[string]any)
			mergeOverride(out, normalizeValue(s).(map[string]any))
			return out, true
		case map[string]any:
			out := deepCopyValue(d).(map[string]any)
			mergeOverride(out, s)
			return out, true
		}
	}
	return nil, false
}

func deepCopyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = deepCopyValue(val)
		}
		return out
	case map[string]string:
		return maps.Clone(t)
	case []any:
		out := make([]any, len(t))
		for i := range t {
			out[i] = deepCopyValue(t[i])
		}
		return out
	case []string:
		return append([]string(nil), t...)
	default:
		return v
	}
}

func copyParams(in map[string]any) map[string]any {
	return deepCopyValue(in).(map[string]any)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
