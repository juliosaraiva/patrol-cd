package project

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
)

func TestRolesAndPolicies(t *testing.T) {
	t.Parallel()
	p := &v1alpha1.AppProject{ObjectMeta: metav1.ObjectMeta{Name: "p"}, Spec: v1alpha1.AppProjectSpec{Roles: []v1alpha1.ProjectRole{
		{Name: "r1", Policies: []string{"p,proj:p:r1,applications,get,p/*,allow", "p,  proj:p:r1 , logs ,get, p/*,allow"}, Groups: []string{"g1"}},
		{Name: "r2"},
	}}}

	r, i, err := GetRoleByName(p, "r2")
	require.NoError(t, err)
	assert.Equal(t, 1, i)
	assert.Equal(t, "r2", r.Name)
	_, _, err = GetRoleByName(p, "nope")
	assert.EqualError(t, err, "role 'nope' does not exist in project 'p'")
	assert.Equal(t, "project 'p' does not have role 'nope'", RoleNotFoundMessage("p", "nope"))

	NormalizePolicies(p)
	assert.Equal(t, []string{"p, proj:p:r1, applications, get, p/*, allow", "p, proj:p:r1, logs, get, p/*, allow"}, p.Spec.Roles[0].Policies)
	assert.Nil(t, p.Spec.Roles[1].Policies)

	assert.Equal(t, "p, proj:p:r1, projects, get, p, allow\n"+
		"p, proj:p:r1, applications, get, p/*, allow\n"+
		"p, proj:p:r1, logs, get, p/*, allow\n"+
		"g, g1, proj:p:r1\n"+
		"p, proj:p:r2, projects, get, p, allow", ProjectPoliciesString(p))
	assert.Equal(t, []rbac.Role{
		{Name: "r1", Policies: p.Spec.Roles[0].Policies, Groups: []string{"g1"}},
		{Name: "r2"},
	}, RBACRoles(p))

	added, err := AddGroupToRole(p, "r2", "team")
	require.NoError(t, err)
	assert.True(t, added)
	added, err = AddGroupToRole(p, "r2", "team")
	require.NoError(t, err)
	assert.False(t, added)
	removed, err := RemoveGroupFromRole(p, "r2", "team")
	require.NoError(t, err)
	assert.True(t, removed)
	removed, err = RemoveGroupFromRole(p, "r2", "team")
	require.NoError(t, err)
	assert.False(t, removed)
	_, err = AddGroupToRole(p, "nope", "x")
	assert.Error(t, err)
}

// Ports the semantics of upstream TestProjectNormalize (TEN-034).
func TestNormalizeJWTTokens(t *testing.T) {
	t.Parallel()
	tokens := []v1alpha1.JWTToken{{ID: "1", IssuedAt: 1}, {ID: "2", IssuedAt: 2}}
	byRole := func() map[string]v1alpha1.JWTTokens {
		return map[string]v1alpha1.JWTTokens{"test-role": {Items: append([]v1alpha1.JWTToken(nil), tokens...)}}
	}
	legacy := []v1alpha1.JWTToken{{IssuedAt: 1}, {IssuedAt: 3}, {IssuedAt: 4}}

	t.Run("EmptyRolesToken", func(t *testing.T) {
		p := &v1alpha1.AppProject{}
		assert.False(t, NormalizeJWTTokens(p))
		assert.Nil(t, p.Spec.Roles)
		assert.Nil(t, p.Status.JWTTokensByRole)
	})
	t.Run("HasRoles_NoTokens", func(t *testing.T) {
		p := &v1alpha1.AppProject{Spec: v1alpha1.AppProjectSpec{Roles: []v1alpha1.ProjectRole{{Name: "test-role"}}}}
		assert.False(t, NormalizeJWTTokens(p))
	})
	t.Run("SpecOnly", func(t *testing.T) {
		p := &v1alpha1.AppProject{Spec: v1alpha1.AppProjectSpec{Roles: []v1alpha1.ProjectRole{{Name: "test-role", JWTTokens: append([]v1alpha1.JWTToken(nil), tokens...)}}}}
		assert.True(t, NormalizeJWTTokens(p))
		assert.ElementsMatch(t, p.Spec.Roles[0].JWTTokens, p.Status.JWTTokensByRole["test-role"].Items) //nolint:staticcheck // deprecated field exercised for migration
		assert.Len(t, p.Status.JWTTokensByRole["test-role"].Items, 2)
	})
	t.Run("StatusOnly", func(t *testing.T) {
		p := &v1alpha1.AppProject{Spec: v1alpha1.AppProjectSpec{Roles: []v1alpha1.ProjectRole{{Name: "test-role"}}}, Status: v1alpha1.AppProjectStatus{JWTTokensByRole: byRole()}}
		assert.True(t, NormalizeJWTTokens(p))
		assert.ElementsMatch(t, p.Spec.Roles[0].JWTTokens, p.Status.JWTTokensByRole["test-role"].Items) //nolint:staticcheck // deprecated field exercised for migration
	})
	t.Run("Same", func(t *testing.T) {
		p := &v1alpha1.AppProject{Spec: v1alpha1.AppProjectSpec{Roles: []v1alpha1.ProjectRole{{Name: "test-role", JWTTokens: append([]v1alpha1.JWTToken(nil), tokens...)}}}, Status: v1alpha1.AppProjectStatus{JWTTokensByRole: byRole()}}
		assert.False(t, NormalizeJWTTokens(p))
		assert.ElementsMatch(t, p.Spec.Roles[0].JWTTokens, p.Status.JWTTokensByRole["test-role"].Items) //nolint:staticcheck // deprecated field exercised for migration
	})
	t.Run("DifferentTokens", func(t *testing.T) {
		p := &v1alpha1.AppProject{Spec: v1alpha1.AppProjectSpec{Roles: []v1alpha1.ProjectRole{{Name: "test-role", JWTTokens: append([]v1alpha1.JWTToken(nil), legacy...)}}}, Status: v1alpha1.AppProjectStatus{JWTTokensByRole: byRole()}}
		assert.True(t, NormalizeJWTTokens(p))
		assert.ElementsMatch(t, p.Spec.Roles[0].JWTTokens, p.Status.JWTTokensByRole["test-role"].Items) //nolint:staticcheck // deprecated field exercised for migration
		// Legacy tokens got iat-based ids; "1" from spec and status merged into one.
		assert.Equal(t, []v1alpha1.JWTToken{{IssuedAt: 4, ID: "4"}, {IssuedAt: 3, ID: "3"}, {IssuedAt: 2, ID: "2"}, {IssuedAt: 1, ID: "1"}}, p.Spec.Roles[0].JWTTokens) //nolint:staticcheck // deprecated field exercised for migration
	})
	t.Run("DifferentRole", func(t *testing.T) {
		p := &v1alpha1.AppProject{
			Spec: v1alpha1.AppProjectSpec{Roles: []v1alpha1.ProjectRole{
				{Name: "test-role", JWTTokens: []v1alpha1.JWTToken{{IssuedAt: 1}}},
				{Name: "test-role1", JWTTokens: []v1alpha1.JWTToken{{IssuedAt: 1}, {IssuedAt: 2}}},
				{Name: "test-role2"},
			}},
			Status: v1alpha1.AppProjectStatus{JWTTokensByRole: byRole()},
		}
		assert.True(t, NormalizeJWTTokens(p))
		for i, r := range p.Spec.Roles {
			assert.ElementsMatch(t, p.Spec.Roles[i].JWTTokens, p.Status.JWTTokensByRole[r.Name].Items) //nolint:staticcheck // deprecated field exercised for migration
		}
	})
	t.Run("RemovedRoleDropsStatus", func(t *testing.T) {
		p := &v1alpha1.AppProject{Status: v1alpha1.AppProjectStatus{JWTTokensByRole: byRole()}}
		assert.True(t, NormalizeJWTTokens(p))
		assert.NotContains(t, p.Status.JWTTokensByRole, "test-role")
	})
}

func TestJWTTokenLifecycle(t *testing.T) {
	t.Parallel()
	p := &v1alpha1.AppProject{ObjectMeta: metav1.ObjectMeta{Name: "p"}, Spec: v1alpha1.AppProjectSpec{Roles: []v1alpha1.ProjectRole{{Name: "ci"}}}}

	require.NoError(t, ValidateJWTTokenID(p, "ci", ""))
	require.NoError(t, AddJWTToken(p, "ci", v1alpha1.JWTToken{IssuedAt: 100, ExpiresAt: 200, ID: "a"}))
	require.NoError(t, AddJWTToken(p, "ci", v1alpha1.JWTToken{IssuedAt: 100, ID: "b"}))
	require.NoError(t, AddJWTToken(p, "ci", v1alpha1.JWTToken{IssuedAt: 50, ID: "c"}))
	assert.Equal(t, []v1alpha1.JWTToken{{IssuedAt: 100, ExpiresAt: 200, ID: "a"}, {IssuedAt: 100, ID: "b"}, {IssuedAt: 50, ID: "c"}}, p.Status.JWTTokensByRole["ci"].Items)
	assert.Equal(t, p.Status.JWTTokensByRole["ci"].Items, p.Spec.Roles[0].JWTTokens, "mirrored to the legacy spec field") //nolint:staticcheck // deprecated field exercised for migration

	requireValidation(t, ValidateJWTTokenID(p, "ci", "a"), CodeInvalidArgument, "Token id 'a' has been used. ")
	assert.Error(t, ValidateJWTTokenID(p, "nope", "a"))
	err := AddJWTToken(p, "nope", v1alpha1.JWTToken{IssuedAt: 1})
	requireValidation(t, err, CodeNotFound, "project 'p' does not have role 'nope'")

	tok, _, err := GetJWTToken(p, "ci", -1, "b")
	require.NoError(t, err)
	assert.Equal(t, "b", tok.ID)
	// An unknown id does not fall back to the issued-at time (deliberate difference from
	// upstream, see TestJWTTokenRevocationWithSameIssuedAt); an empty id does.
	_, _, err = GetJWTToken(p, "ci", 100, "deleted")
	require.EqualError(t, err, "JWT token for role 'ci' issued at '100' does not exist in project 'p'")
	tok, _, err = GetJWTToken(p, "ci", 100, "")
	require.NoError(t, err)
	assert.Equal(t, "a", tok.ID)
	_, _, err = GetJWTToken(p, "ci", 7, "nope")
	assert.EqualError(t, err, "JWT token for role 'ci' issued at '7' does not exist in project 'p'")
	_, _, err = GetJWTTokenFromSpec(p, "ci", 50, "")
	require.NoError(t, err)

	found, err := RemoveJWTToken(p, "ci", -1, "b")
	require.NoError(t, err)
	assert.True(t, found)
	_, _, err = GetJWTToken(p, "ci", -1, "b")
	require.Error(t, err)
	found, err = RemoveJWTToken(p, "ci", 50, "")
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, []v1alpha1.JWTToken{{IssuedAt: 100, ExpiresAt: 200, ID: "a"}}, p.Status.JWTTokensByRole["ci"].Items)
	assert.Equal(t, []v1alpha1.JWTToken{{IssuedAt: 100, ExpiresAt: 200, ID: "a"}}, p.Spec.Roles[0].JWTTokens) //nolint:staticcheck // deprecated field exercised for migration

	found, err = RemoveJWTToken(p, "ci", 999, "zzz")
	require.Error(t, err)
	assert.False(t, found)
	_, err = RemoveJWTToken(p, "nope", 1, "")
	require.Error(t, err)
}

// Security (deliberate difference from upstream TEN-035): a token with a jti is matched by
// its jti only. The issued-at fallback applies to legacy tokens without a jti, so revoking a
// token is effective even when another token of the role was issued in the same second, and
// deleting by an unknown id never removes another token.
func TestJWTTokenRevocationWithSameIssuedAt(t *testing.T) {
	t.Parallel()
	p := &v1alpha1.AppProject{ObjectMeta: metav1.ObjectMeta{Name: "p"}, Spec: v1alpha1.AppProjectSpec{Roles: []v1alpha1.ProjectRole{{Name: "ci"}}}}
	require.NoError(t, AddJWTToken(p, "ci", v1alpha1.JWTToken{IssuedAt: 100, ID: "keep"}))
	require.NoError(t, AddJWTToken(p, "ci", v1alpha1.JWTToken{IssuedAt: 100, ID: "revoked"}))

	found, err := RemoveJWTToken(p, "ci", 100, "revoked")
	require.NoError(t, err)
	require.True(t, found)
	_, _, err = GetJWTToken(p, "ci", 100, "revoked")
	require.EqualError(t, err, "JWT token for role 'ci' issued at '100' does not exist in project 'p'", "a revoked jti must not authenticate through the iat of another token")
	_, _, err = GetJWTTokenFromSpec(p, "ci", 100, "revoked")
	require.Error(t, err)
	tok, _, err := GetJWTToken(p, "ci", 100, "keep")
	require.NoError(t, err)
	assert.Equal(t, "keep", tok.ID)

	found, err = RemoveJWTToken(p, "ci", 100, "unknown")
	require.Error(t, err)
	assert.False(t, found, "an unknown id must not remove the token with the same iat")
	assert.Equal(t, []v1alpha1.JWTToken{{IssuedAt: 100, ID: "keep"}}, p.Status.JWTTokensByRole["ci"].Items)
	assert.Equal(t, []v1alpha1.JWTToken{{IssuedAt: 100, ID: "keep"}}, p.Spec.Roles[0].JWTTokens) //nolint:staticcheck // deprecated field exercised for migration

	// Legacy tokens (no jti claim) are still found, and deleted, by issued-at time.
	tok, _, err = GetJWTToken(p, "ci", 100, "")
	require.NoError(t, err)
	assert.Equal(t, "keep", tok.ID)
	_, _, err = GetJWTToken(p, "ci", -1, "")
	require.Error(t, err)
	found, err = RemoveJWTToken(p, "ci", 100, "")
	require.NoError(t, err)
	assert.True(t, found)
	assert.Empty(t, p.Status.JWTTokensByRole["ci"].Items)
}
