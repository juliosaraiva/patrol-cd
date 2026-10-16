package server

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

func TestEnsureDefaultProject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme, err := newScheme()
	require.NoError(t, err)
	c := crfake.NewClientBuilder().WithScheme(scheme).Build()
	require.NoError(t, ensureDefaultProject(ctx, c, "patrol"))
	var p v1alpha1.AppProject
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "patrol", Name: "default"}, &p))
	assert.Equal(t, []string{"*"}, p.Spec.SourceRepos)
	assert.Equal(t, []v1alpha1.ApplicationDestination{{Server: "*", Namespace: "*"}}, p.Spec.Destinations)
	assert.Equal(t, []v1alpha1.ClusterResourceRestrictionItem{{Group: "*", Kind: "*"}}, p.Spec.ClusterResourceWhitelist)

	// A modified default project is never overwritten (TEN-002).
	p.Spec.SourceRepos = []string{"https://git.example.com/*"}
	require.NoError(t, c.Update(ctx, &p))
	require.NoError(t, ensureDefaultProject(ctx, c, "patrol"))
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "patrol", Name: "default"}, &p))
	assert.Equal(t, []string{"https://git.example.com/*"}, p.Spec.SourceRepos)
}

func TestNormalizeProjectTokens(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme, err := newScheme()
	require.NoError(t, err)
	legacy := &v1alpha1.AppProject{
		ObjectMeta: metav1.ObjectMeta{Name: "team", Namespace: "patrol"},
		Spec: v1alpha1.AppProjectSpec{Roles: []v1alpha1.ProjectRole{{
			Name: "ci", JWTTokens: []v1alpha1.JWTToken{{IssuedAt: 1700000000}},
		}}},
		Status: v1alpha1.AppProjectStatus{JWTTokensByRole: map[string]v1alpha1.JWTTokens{
			"removed-role": {Items: []v1alpha1.JWTToken{{IssuedAt: 1600000000, ID: "old"}}},
		}},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(legacy).WithStatusSubresource(&v1alpha1.AppProject{}).Build()
	require.NoError(t, normalizeProjectTokens(ctx, c, "patrol"))
	var p v1alpha1.AppProject
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "patrol", Name: "team"}, &p))
	// TEN-034: spec-only legacy tokens migrate to status with id = iat; status entries of
	// removed roles are dropped.
	legacyTokens := p.Spec.Roles[0].JWTTokens //nolint:staticcheck // the deprecated field is the migration's input and mirror
	require.Len(t, legacyTokens, 1)
	assert.Equal(t, "1700000000", legacyTokens[0].ID)
	assert.Equal(t, []v1alpha1.JWTToken{{IssuedAt: 1700000000, ID: "1700000000"}}, p.Status.JWTTokensByRole["ci"].Items)
	assert.NotContains(t, p.Status.JWTTokensByRole, "removed-role")
}
