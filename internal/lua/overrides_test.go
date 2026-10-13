package lua

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"pgregory.net/rapid"
)

func TestParseResourceOverridesLegacy(t *testing.T) {
	t.Parallel()
	data := map[string]string{
		"resource.customizations": `
cert-manager.io/Certificate:
  health.lua: |
    return {status = "Healthy"}
  health.lua.useOpenLibs: true
  actions: |
    discovery.lua: return {}
"*.aws.crossplane.io/*":
  health.lua: return nil
apps/Deployment:
  ignoreDifferences: |
    jsonPointers:
    - /spec/replicas
  ignoreResourceUpdates: |
    jqPathExpressions:
    - .status
  knownTypeFields:
  - field: spec.template.spec
    type: core/v1/PodSpec
`,
	}
	got, err := ParseResourceOverrides(data)
	require.NoError(t, err)
	assert.Equal(t, map[string]ResourceOverride{
		"cert-manager.io/Certificate": {
			HealthLua:   "return {status = \"Healthy\"}\n",
			UseOpenLibs: true,
			Actions:     "discovery.lua: return {}\n",
		},
		"*.aws.crossplane.io/*": {HealthLua: "return nil"},
		"apps/Deployment": {
			IgnoreDifferences:     "jsonPointers:\n- /spec/replicas\n",
			IgnoreResourceUpdates: "jqPathExpressions:\n- .status\n",
			KnownTypeFields:       []KnownTypeField{{Field: "spec.template.spec", Type: "core/v1/PodSpec"}},
		},
	}, got)

	diff, err := got["apps/Deployment"].GetIgnoreDifferences()
	require.NoError(t, err)
	assert.Equal(t, []string{"/spec/replicas"}, diff.JSONPointers)
	upd, err := got["apps/Deployment"].GetIgnoreResourceUpdates()
	require.NoError(t, err)
	assert.Equal(t, []string{".status"}, upd.JQPathExpressions)
	actions, err := got["cert-manager.io/Certificate"].GetActions()
	require.NoError(t, err)
	assert.Equal(t, "return {}", actions.ActionDiscoveryLua)
}

func TestParseResourceOverridesSplitKeys(t *testing.T) {
	t.Parallel()
	data := map[string]string{
		"resource.customizations": `
cert-manager.io/Certificate:
  health.lua: legacy
  actions: legacy-actions
`,
		"resource.customizations.health.cert-manager.io_Certificate":      "split",
		"resource.customizations.useOpenLibs.cert-manager.io_Certificate": "true",
		"resource.customizations.health.ServiceAccount":                   "sa",
		"resource.customizations.ignoreDifferences.all":                   "jsonPointers:\n- /status\n",
		"resource.customizations.ignoreResourceUpdates.apps_Deployment":   "jsonPointers:\n- /metadata/annotations\n",
		"resource.customizations.knownTypeFields.argoproj.io_Rollout":     "- field: spec.template.spec\n  type: core/v1/PodSpec\n",
		"resource.customizations.actions.argoproj.io_Rollout":             "mergeBuiltinActions: true\n",
		"resource.exclusions":              "ignored",
		"resource.customizationsunrelated": "too few parts",
	}
	got, err := ParseResourceOverrides(data)
	require.NoError(t, err)
	assert.Equal(t, map[string]ResourceOverride{
		"cert-manager.io/Certificate": {HealthLua: "split", UseOpenLibs: true, Actions: "legacy-actions"},
		"ServiceAccount":              {HealthLua: "sa"},
		"*/*":                         {IgnoreDifferences: "jsonPointers:\n- /status\n"},
		"apps/Deployment":             {IgnoreResourceUpdates: "jsonPointers:\n- /metadata/annotations\n"},
		"argoproj.io/Rollout": {
			KnownTypeFields: []KnownTypeField{{Field: "spec.template.spec", Type: "core/v1/PodSpec"}},
			Actions:         "mergeBuiltinActions: true\n",
		},
	}, got)
}

func TestParseResourceOverridesErrors(t *testing.T) {
	t.Parallel()
	tests := map[string]map[string]string{
		"unknown type":            {"resource.customizations.bogus.apps_Deployment": "x"},
		"bad bool":                {"resource.customizations.useOpenLibs.apps_Deployment": "maybe"},
		"bad group kind":          {"resource.customizations.health.a_b_c": "x"},
		"empty group kind":        {"resource.customizations.health.": "x"},
		"bad ignoreDifferences":   {"resource.customizations.ignoreDifferences.apps_Deployment": "jsonPointers: {"},
		"bad ignoreUpdates":       {"resource.customizations.ignoreResourceUpdates.apps_Deployment": "jsonPointers: ["},
		"bad knownTypeFields":     {"resource.customizations.knownTypeFields.apps_Deployment": "field: x"},
		"bad legacy yaml":         {"resource.customizations": "a: [b"},
		"legacy diff not string":  {"resource.customizations": "apps/Deployment:\n  ignoreDifferences:\n    jsonPointers: [/a]\n"},
		"legacy diff bad content": {"resource.customizations": "apps/Deployment:\n  ignoreDifferences: 'jsonPointers: {'\n"},
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseResourceOverrides(data)
			require.Error(t, err)
			if name != "legacy diff not string" {
				assert.ErrorIs(t, err, ErrInvalidCustomization)
			}
		})
	}
}

func TestOverrideKey(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "Pod", OverrideKey(schema.GroupKind{Kind: "Pod"}))
	assert.Equal(t, "apps/Deployment", OverrideKey(schema.GroupKind{Group: "apps", Kind: "Deployment"}))
}

// Split-key group_kind conversion is the inverse of OverrideKey for every group without
// "_" and every kind without "_" and "." (Kubernetes kinds are CamelCase identifiers).
func TestSplitKeyGroupKindProperty(t *testing.T) {
	t.Parallel()
	label := rapid.StringMatching(`[a-z][a-z0-9-]{0,10}`)
	rapid.Check(t, func(t *rapid.T) {
		kind := rapid.StringMatching(`[A-Z][A-Za-z0-9]{0,15}`).Draw(t, "kind")
		parts := rapid.SliceOfN(label, 0, 4).Draw(t, "group")
		group := strings.Join(parts, ".")
		gk := schema.GroupKind{Group: group, Kind: kind}
		suffix := kind
		if group != "" {
			suffix = group + "_" + kind
		}
		got, err := splitKeyGroupKind(suffix)
		if err != nil {
			t.Fatalf("splitKeyGroupKind(%q): %v", suffix, err)
		}
		if got != OverrideKey(gk) {
			t.Fatalf("splitKeyGroupKind(%q) = %q, want %q", suffix, got, OverrideKey(gk))
		}
		parsed, err := ParseResourceOverrides(map[string]string{"resource.customizations.health." + suffix: "return nil"})
		if err != nil {
			t.Fatal(err)
		}
		if parsed[OverrideKey(gk)].HealthLua != "return nil" {
			t.Fatalf("split key not mapped to %q: %v", OverrideKey(gk), parsed)
		}
	})
}

// MarshalLegacy and the legacy parser round-trip arbitrary customizations.
func TestLegacyRoundTripProperty(t *testing.T) {
	t.Parallel()
	text := rapid.StringMatching(`[ -~\n]{0,40}`)
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(0, 4).Draw(t, "n")
		in := map[string]ResourceOverride{}
		for range n {
			key := rapid.StringMatching(`(\*|[a-z]{1,8})(\.[a-z]{1,5}){0,2}/(\*|[A-Z][a-z]{0,8})`).Draw(t, "key")
			o := ResourceOverride{
				HealthLua:   text.Draw(t, "health"),
				UseOpenLibs: rapid.Bool().Draw(t, "open"),
				Actions:     text.Draw(t, "actions"),
			}
			if rapid.Bool().Draw(t, "diff") {
				o.IgnoreDifferences = "jsonPointers:\n- /spec/" + rapid.StringMatching(`[a-z]{1,8}`).Draw(t, "ptr") + "\n"
			}
			if rapid.Bool().Draw(t, "ktf") {
				o.KnownTypeFields = []KnownTypeField{{Field: "spec.x", Type: "cpu"}}
			}
			in[key] = o
		}
		doc, err := MarshalLegacy(in)
		if err != nil {
			t.Fatal(err)
		}
		out, err := ParseResourceOverrides(map[string]string{CustomizationsKey: doc})
		if err != nil {
			t.Fatalf("parse %q: %v", doc, err)
		}
		if len(in) == 0 {
			if len(out) != 0 {
				t.Fatalf("expected no overrides, got %v", out)
			}
			return
		}
		assert.Equal(t, in, out)
	})
}
