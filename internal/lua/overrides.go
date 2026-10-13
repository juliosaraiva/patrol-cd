package lua

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"
)

// Settings keys (patrol-cm), identical to upstream argocd-cm keys.
const (
	// CustomizationsKey holds the legacy YAML map of group/kind → customization.
	CustomizationsKey = "resource.customizations"
	// customizationsPrefix starts every split key resource.customizations.<type>.<group_kind>.
	customizationsPrefix = CustomizationsKey
)

// ErrInvalidCustomization is wrapped by errors from ParseResourceOverrides.
var ErrInvalidCustomization = errors.New("invalid resource customization")

// KnownTypeField declares that a CRD field has a well-known Kubernetes type (for example
// "cpu" or "memory") so diffing can normalize its representation.
type KnownTypeField struct {
	Field string `json:"field,omitempty"`
	Type  string `json:"type,omitempty"`
}

// IgnoreDiff lists fields to ignore when diffing or when deciding whether a resource
// update should trigger a refresh.
type IgnoreDiff struct {
	JSONPointers          []string `json:"jsonPointers"`
	JQPathExpressions     []string `json:"jqPathExpressions"`
	ManagedFieldsManagers []string `json:"managedFieldsManagers"`
}

// ResourceOverride is the customization configured for one group/kind key.
//
// Keys of the map passed to [New] are "group/kind" ("Kind" for the core group), may
// contain glob wildcards ("*.crossplane.io/*", "*/*") and are matched as upstream does.
// Callers adapting CRD or settings types copy the fields one to one.
type ResourceOverride struct {
	// HealthLua is a Lua health script; empty means not configured.
	HealthLua string
	// UseOpenLibs opens the full Lua standard library (including io and os) for HealthLua.
	UseOpenLibs bool
	// Actions is the YAML document describing custom actions (see [ResourceActions]).
	Actions string
	// IgnoreDifferences and IgnoreResourceUpdates are YAML documents of [IgnoreDiff],
	// validated at parse time and consumed by the diff package.
	IgnoreDifferences     string
	IgnoreResourceUpdates string
	// KnownTypeFields lists CRD fields with well-known types.
	KnownTypeFields []KnownTypeField
}

// ResourceActions is the parsed form of ResourceOverride.Actions.
type ResourceActions struct {
	// ActionDiscoveryLua lists available actions for an object.
	ActionDiscoveryLua string `json:"discovery.lua,omitempty"`
	// Definitions are the custom action scripts.
	Definitions []ActionDefinition `json:"definitions,omitempty"`
	// MergeBuiltinActions also offers the built-in actions of the kind.
	MergeBuiltinActions bool `json:"mergeBuiltinActions,omitempty"`
}

// ActionDefinition is one named custom action script.
type ActionDefinition struct {
	Name      string `json:"name"`
	ActionLua string `json:"action.lua"`
}

// GetActions parses o.Actions.
func (o ResourceOverride) GetActions() (ResourceActions, error) {
	var a ResourceActions
	if err := yaml.Unmarshal([]byte(o.Actions), &a); err != nil {
		return a, fmt.Errorf("%w: actions: %w", ErrInvalidCustomization, err)
	}
	return a, nil
}

// GetIgnoreDifferences parses o.IgnoreDifferences.
func (o ResourceOverride) GetIgnoreDifferences() (IgnoreDiff, error) {
	return parseIgnoreDiff("ignoreDifferences", o.IgnoreDifferences)
}

// GetIgnoreResourceUpdates parses o.IgnoreResourceUpdates.
func (o ResourceOverride) GetIgnoreResourceUpdates() (IgnoreDiff, error) {
	return parseIgnoreDiff("ignoreResourceUpdates", o.IgnoreResourceUpdates)
}

func parseIgnoreDiff(field, doc string) (IgnoreDiff, error) {
	var d IgnoreDiff
	if err := yaml.Unmarshal([]byte(doc), &d); err != nil {
		return d, fmt.Errorf("%w: %s: %w", ErrInvalidCustomization, field, err)
	}
	return d, nil
}

// OverrideKey returns the customization key of gk: "group/kind", or "Kind" for the core
// group.
func OverrideKey(gk schema.GroupKind) string {
	if gk.Group == "" {
		return gk.Kind
	}
	return gk.Group + "/" + gk.Kind
}

// legacyOverride is the per-key shape of the legacy resource.customizations YAML.
type legacyOverride struct {
	HealthLua             string           `json:"health.lua,omitempty"`
	UseOpenLibs           bool             `json:"health.lua.useOpenLibs,omitempty"`
	Actions               string           `json:"actions,omitempty"`
	IgnoreDifferences     string           `json:"ignoreDifferences,omitempty"`
	IgnoreResourceUpdates string           `json:"ignoreResourceUpdates,omitempty"`
	KnownTypeFields       []KnownTypeField `json:"knownTypeFields,omitempty"`
}

// ParseResourceOverrides extracts resource customizations from settings ConfigMap data
// (patrol-cm), supporting both formats upstream accepts:
//
//   - the legacy key resource.customizations holding a YAML map keyed by group/kind
//     (wildcards allowed) with fields health.lua, health.lua.useOpenLibs, actions,
//     ignoreDifferences, ignoreResourceUpdates, knownTypeFields;
//   - split keys resource.customizations.<type>.<group_kind>, where <type> is one of
//     health, useOpenLibs, actions, ignoreDifferences, ignoreResourceUpdates,
//     knownTypeFields and <group_kind> is "group_Kind", "Kind" or "all" (→ "*/*").
//
// Split keys are applied after the legacy map and replace individual fields. Status-field
// ignore rules derived from resource.compareoptions are not added here; that is the diff
// settings' responsibility.
func ParseResourceOverrides(data map[string]string) (map[string]ResourceOverride, error) {
	out := map[string]ResourceOverride{}
	if v := data[CustomizationsKey]; v != "" {
		legacy := map[string]legacyOverride{}
		if err := yaml.Unmarshal([]byte(v), &legacy); err != nil {
			return nil, fmt.Errorf("%w: %s: %w", ErrInvalidCustomization, CustomizationsKey, err)
		}
		for key, l := range legacy {
			o := ResourceOverride(l)
			if _, err := o.GetIgnoreDifferences(); err != nil {
				return nil, fmt.Errorf("%s[%q]: %w", CustomizationsKey, key, err)
			}
			if _, err := o.GetIgnoreResourceUpdates(); err != nil {
				return nil, fmt.Errorf("%s[%q]: %w", CustomizationsKey, key, err)
			}
			out[key] = o
		}
	}
	for k, v := range data {
		if !strings.HasPrefix(k, customizationsPrefix) {
			continue
		}
		parts := strings.SplitN(k, ".", 4)
		if len(parts) < 4 {
			continue
		}
		key, err := splitKeyGroupKind(parts[3])
		if err != nil {
			return nil, fmt.Errorf("%w: key %q: %w", ErrInvalidCustomization, k, err)
		}
		o := out[key]
		switch typ := parts[2]; typ {
		case "health":
			o.HealthLua = v
		case "useOpenLibs":
			b, err := strconv.ParseBool(v)
			if err != nil {
				return nil, fmt.Errorf("%w: key %q: %w", ErrInvalidCustomization, k, err)
			}
			o.UseOpenLibs = b
		case "actions":
			o.Actions = v
		case "ignoreDifferences":
			if _, err := parseIgnoreDiff(typ, v); err != nil {
				return nil, fmt.Errorf("key %q: %w", k, err)
			}
			o.IgnoreDifferences = v
		case "ignoreResourceUpdates":
			if _, err := parseIgnoreDiff(typ, v); err != nil {
				return nil, fmt.Errorf("key %q: %w", k, err)
			}
			o.IgnoreResourceUpdates = v
		case "knownTypeFields":
			var fields []KnownTypeField
			if err := yaml.Unmarshal([]byte(v), &fields); err != nil {
				return nil, fmt.Errorf("%w: key %q: %w", ErrInvalidCustomization, k, err)
			}
			o.KnownTypeFields = fields
		default:
			return nil, fmt.Errorf("%w: resource customization type %s not supported", ErrInvalidCustomization, typ)
		}
		out[key] = o
	}
	return out, nil
}

// splitKeyGroupKind converts "group_Kind" → "group/Kind", "Kind" → "Kind", "all" → "*/*".
func splitKeyGroupKind(groupKind string) (string, error) {
	parts := strings.Split(groupKind, "_")
	switch {
	case len(parts) == 2:
		return parts[0] + "/" + parts[1], nil
	case len(parts) == 1 && groupKind == "all":
		return "*/*", nil
	case len(parts) == 1 && groupKind != "":
		return groupKind, nil
	}
	return "", fmt.Errorf("group kind should be in format `resource.customizations.<type>.<group_kind>` or `resource.customizations.<type>.<kind>`, got group kind: %q", groupKind)
}

// MarshalLegacy renders overrides in the legacy resource.customizations YAML format. It is
// the inverse of the legacy part of ParseResourceOverrides (used by tooling and tests).
func MarshalLegacy(overrides map[string]ResourceOverride) (string, error) {
	legacy := make(map[string]legacyOverride, len(overrides))
	for k, o := range overrides {
		legacy[k] = legacyOverride(o)
	}
	b, err := json.Marshal(legacy)
	if err != nil {
		return "", fmt.Errorf("marshal resource customizations: %w", err)
	}
	y, err := yaml.JSONToYAML(b)
	if err != nil {
		return "", fmt.Errorf("marshal resource customizations: %w", err)
	}
	return string(y), nil
}
