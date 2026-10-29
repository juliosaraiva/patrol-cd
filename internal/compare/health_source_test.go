package compare

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// TestEnvResourceHealthSource: a comparison stores per-resource health in status.resources
// only with PersistResourceHealth (resourceHealthSource inline). Otherwise (the default of
// controller.resource.health.persist) status.resources has no health, resourceHealthSource
// is appTree and the tree nodes carry the health the API server fills in (DIFF-064).
func TestEnvResourceHealthSource(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		persist bool
		want    v1alpha1.ResourceHealthLocation
	}{
		{name: "persisted", persist: true, want: v1alpha1.ResourceHealthLocationInline},
		{name: "tree", persist: false, want: v1alpha1.ResourceHealthLocationAppTree},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, withDeps(func(d *Deps) { d.PersistResourceHealth = tc.persist }))
			h.repo.set(repoURL, "app", "rev-1", configMap("cm", "", nil))
			res := h.compare(Input{App: h.app("health", src("app"))})

			assert.Equal(t, tc.want, res.ResourceHealthSource)
			r := findResource(res, "ConfigMap", "cm")
			require.NotNil(t, r)
			if tc.persist {
				require.NotNil(t, r.Health)
				assert.Equal(t, v1alpha1.HealthStatusMissing, r.Health.Status)
			} else {
				assert.Nil(t, r.Health)
			}
			require.NotNil(t, res.Tree)
			n := res.Tree.FindNode("", "ConfigMap", h.ns, "cm")
			require.NotNil(t, n)
			require.NotNil(t, n.Health)
			assert.Equal(t, v1alpha1.HealthStatusMissing, n.Health.Status)
		})
	}
}
