package compare

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// TestEnvResourceHealthSource: a comparison stores per-resource health in status.resources
// only with PersistResourceHealth (resourceHealthSource inline). Otherwise (the default of
// controller.resource.health.persist) status.resources has no health, resourceHealthSource
// is appTree and the tree nodes carry the health the API server fills in (DIFF-064): the
// assessed health of live objects (a Healthy Service, a Progressing Deployment that no
// controller rolls out) and Missing for an object never created.
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
			deploy := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "apps/v1", "kind": "Deployment",
				"metadata": map[string]any{"name": "web"},
				"spec": map[string]any{
					"selector": map[string]any{"matchLabels": map[string]any{"app": "web"}},
					"template": map[string]any{
						"metadata": map[string]any{"labels": map[string]any{"app": "web"}},
						"spec":     map[string]any{"containers": []any{map[string]any{"name": "c", "image": "nginx:1"}}},
					},
				},
			}}
			svc := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "v1", "kind": "Service",
				"metadata": map[string]any{"name": "web"},
				"spec":     map[string]any{"selector": map[string]any{"app": "web"}, "ports": []any{map[string]any{"port": int64(80)}}},
			}}
			h.repo.set(repoURL, "app", "rev-1", configMap("cm", "", nil), deploy, svc)
			in := Input{App: h.app("health", src("app"))}
			res := h.compare(in)
			for _, tgt := range res.Targets {
				if tgt.GetKind() != "ConfigMap" {
					h.create(tgt)
				}
			}
			live := func(r *Result, group, kind string) *ResourceNode {
				if r.Tree == nil {
					return nil
				}
				n := r.Tree.FindNode(group, kind, h.ns, "web")
				if n == nil || n.UID == "" || n.Health == nil {
					return nil
				}
				return n
			}
			res = h.eventually(in, func(r *Result) bool {
				return live(r, "", "Service") != nil && live(r, "apps", "Deployment") != nil
			}, "the live Service and Deployment in the tree")

			assert.Equal(t, tc.want, res.ResourceHealthSource)
			for _, c := range []struct {
				group, kind, name string
				want              v1alpha1.HealthStatusCode
			}{
				{"", "Service", "web", v1alpha1.HealthStatusHealthy},
				{"apps", "Deployment", "web", v1alpha1.HealthStatusProgressing},
				{"", "ConfigMap", "cm", v1alpha1.HealthStatusMissing},
			} {
				n := res.Tree.FindNode(c.group, c.kind, h.ns, c.name)
				require.NotNil(t, n, c.kind)
				require.NotNil(t, n.Health, c.kind)
				assert.Equal(t, c.want, n.Health.Status, "tree node of the %s", c.kind)
				r := findResource(res, c.kind, c.name)
				require.NotNil(t, r, c.kind)
				if tc.persist {
					require.NotNil(t, r.Health, c.kind)
					assert.Equal(t, n.Health.Status, r.Health.Status, "status.resources of the %s", c.kind)
					assert.Equal(t, n.Health.Message, r.Health.Message, "status.resources of the %s", c.kind)
				} else {
					assert.Nil(t, r.Health, c.kind)
				}
			}
		})
	}
}
