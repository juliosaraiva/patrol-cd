package diff

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// ErrInvalidOverrideKey is returned for resource override keys that are neither "<kind>" nor
// "<group>/<kind>".
var ErrInvalidOverrideKey = errors.New("diff: override key must be <group>/<kind> or <kind>")

// ParseOverrideKey splits a resource override key (the keys of the resource.customizations
// map, e.g. "apps/Deployment", "Service", "*/*") into group and kind. A key without "/" names
// a kind of the core group.
func ParseOverrideKey(key string) (group, kind string, err error) {
	parts := strings.Split(key, "/")
	switch len(parts) {
	case 1:
		return "", parts[0], nil
	case 2:
		return parts[0], parts[1], nil
	default:
		return "", "", fmt.Errorf("%w, got %q", ErrInvalidOverrideKey, key)
	}
}

// SystemRule converts one system-level ignoreDifferences override into an IgnoreRule with the
// baseline's semantics:
//   - JSON pointers and jq expressions apply to every resource whose group and kind match the
//     key's group and kind as glob patterns;
//   - managedFieldsManagers are looked up by the exact key "<group>/<kind>" of the resource or
//     by "*/*", so they are kept only for keys of that form: never for core-group keys
//     ("Service"), nor for keys containing glob characters other than "*/*".
func SystemRule(key string, jsonPointers, jqPathExpressions, managedFieldsManagers []string) (IgnoreRule, error) {
	group, kind, err := ParseOverrideKey(key)
	if err != nil {
		return IgnoreRule{}, err
	}
	rule := IgnoreRule{
		Group:             group,
		Kind:              kind,
		JSONPointers:      slices.Clone(jsonPointers),
		JQPathExpressions: slices.Clone(jqPathExpressions),
	}
	exactLookup := key == "*/*" || (strings.Contains(key, "/") && !strings.ContainsAny(key, `*?[]{}\!`))
	if exactLookup {
		rule.ManagedFieldsManagers = slices.Clone(managedFieldsManagers)
	}
	return rule, nil
}
