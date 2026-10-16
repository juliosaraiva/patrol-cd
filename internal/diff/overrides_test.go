package diff

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestParseOverrideKey(t *testing.T) {
	t.Parallel()
	for key, want := range map[string][2]string{
		"apps/Deployment": {"apps", "Deployment"},
		"Service":         {"", "Service"},
		"*/*":             {"*", "*"},
		"*.example.com/*": {"*.example.com", "*"},
	} {
		g, k, err := ParseOverrideKey(key)
		require.NoError(t, err, key)
		assert.Equal(t, want, [2]string{g, k}, key)
	}
	_, _, err := ParseOverrideKey("a/b/c")
	require.ErrorIs(t, err, ErrInvalidOverrideKey)
}

func TestSystemRule(t *testing.T) {
	t.Parallel()
	managers := []string{"kube-controller-manager"}
	cases := []struct {
		key          string
		wantManagers bool
		matches      []schema.GroupKind
		notMatches   []schema.GroupKind
	}{
		{key: "apps/Deployment", wantManagers: true, matches: []schema.GroupKind{{Group: "apps", Kind: "Deployment"}}, notMatches: []schema.GroupKind{{Group: "apps", Kind: "StatefulSet"}}},
		{key: "*/*", wantManagers: true, matches: []schema.GroupKind{{Kind: "Service"}, {Group: "apps", Kind: "Deployment"}}},
		// Core-group keys are looked up as "/Service" by the baseline and never match.
		{key: "Service", wantManagers: false, matches: []schema.GroupKind{{Kind: "Service"}}, notMatches: []schema.GroupKind{{Group: "apps", Kind: "Service"}}},
		// Glob keys apply pointers and jq expressions, never managers.
		{key: "*.example.com/*", wantManagers: false, matches: []schema.GroupKind{{Group: "db.example.com", Kind: "Cluster"}}, notMatches: []schema.GroupKind{{Group: "apps", Kind: "Deployment"}}},
		{key: "apps/*", wantManagers: false, matches: []schema.GroupKind{{Group: "apps", Kind: "DaemonSet"}}},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			t.Parallel()
			r, err := SystemRule(tc.key, []string{"/spec/replicas"}, []string{".spec.paused"}, managers)
			require.NoError(t, err)
			if tc.wantManagers {
				assert.Equal(t, managers, r.ManagedFieldsManagers)
			} else {
				assert.Empty(t, r.ManagedFieldsManagers)
			}
			n, err := NewIgnoreNormalizer([]IgnoreRule{r}, IgnoreNormalizerOptions{})
			require.NoError(t, err)
			cr := n.rules[0]
			for _, gk := range tc.matches {
				assert.True(t, cr.matches(gk, "x", "y"), "%s should match %s", tc.key, gk)
			}
			for _, gk := range tc.notMatches {
				assert.False(t, cr.matches(gk, "x", "y"), "%s should not match %s", tc.key, gk)
			}
		})
	}
	_, err := SystemRule("a/b/c", nil, nil, nil)
	require.ErrorIs(t, err, ErrInvalidOverrideKey)
}
