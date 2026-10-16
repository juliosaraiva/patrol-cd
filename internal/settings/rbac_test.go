package settings

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/juliosaraiva/patrol-cd/internal/rbac"
)

func TestRBACConfigMap(t *testing.T) {
	t.Parallel()
	rbacCM := newCM(DefaultRBACConfigMapName, map[string]string{
		RBACKeyPolicyCSV:     "p, my-org:team-alpha, applications, sync, my-project/*, allow\ng, my-org:team-beta, role:admin\n",
		"policy.z.csv":       "g, my-org:team-z, role:tester\n",
		"policy.a.csv":       "p, role:tester, applications, *, */*, allow\n",
		RBACKeyPolicyDefault: "role:readonly",
		RBACKeyScopes:        "[cognito:groups, email]",
		RBACKeyMatchMode:     "glob",
	})
	rbacCM.Labels = nil // read by name; the part-of label is not required (TEN-078)
	s := Parse(in(map[string]string{}, rbacCM))
	requireNoIssues(t, s, DefaultRBACConfigMapName)
	r := s.RBAC()
	assert.True(t, r.Present)
	assert.Equal(t, "role:readonly", r.DefaultRole)
	assert.Equal(t, "glob", r.MatchMode)
	assert.Equal(t, []string{"cognito:groups", "email"}, r.Scopes)
	require.Len(t, r.Sources, 3)
	assert.Equal(t, []string{RBACKeyPolicyCSV, "policy.a.csv", "policy.z.csv"}, []string{r.Sources[0].Name, r.Sources[1].Name, r.Sources[2].Name}, "overlays in sorted key order (TEN-077)")
	assert.Equal(t, rbac.PolicyCSV(rbacCM.Data), r.PolicyCSV)

	enf := rbac.NewEnforcer()
	require.NoError(t, enf.SetPolicySources(rbac.BuiltinPolicy(), r.Sources, r.DefaultRole, r.MatchMode))
	assert.True(t, enf.Enforce(rbac.Subjects{Groups: []string{"my-org:team-z"}}, "applications", "delete", "p/app").Allowed)
	assert.True(t, enf.Enforce(rbac.Subjects{User: "nobody"}, "applications", "get", "p/app").Allowed, "default role")
}

func TestRBACDefaultsWhenAbsent(t *testing.T) {
	t.Parallel()
	s := Parse(in(map[string]string{}))
	r := s.RBAC()
	assert.False(t, r.Present)
	assert.Equal(t, rbac.DefaultScopes(), r.Scopes)
	assert.Equal(t, rbac.GlobMatchMode, r.MatchMode)
	assert.Empty(t, r.PolicyCSV)
}

func TestRBACInvalidPolicyFailsClosed(t *testing.T) {
	t.Parallel()
	good := Parse(in(map[string]string{}, newCM(DefaultRBACConfigMapName, map[string]string{RBACKeyPolicyCSV: "g, alice, role:admin\n"})))
	bad := Parse(in(map[string]string{}, newCM(DefaultRBACConfigMapName, map[string]string{RBACKeyPolicyCSV: "p, too, few\n"})), WithPrevious(good))
	requireIssue(t, bad, DefaultRBACConfigMapName, RBACKeyPolicyCSV, SeverityError, "every request is denied")
	assert.Equal(t, "p, too, few\n", bad.RBAC().PolicyCSV, "an invalid policy is passed through, never replaced by the last valid one")
	enf := rbac.NewEnforcer()
	err := enf.SetPolicySources(rbac.BuiltinPolicy(), bad.RBAC().Sources, "", "")
	assert.Error(t, err)
	assert.False(t, enf.Enforce(rbac.Subjects{User: "alice"}, "applications", "get", "p/a").Allowed)
}

func TestRBACScopesIsolationAndWarnings(t *testing.T) {
	t.Parallel()
	good := Parse(in(map[string]string{}, newCM(DefaultRBACConfigMapName, map[string]string{RBACKeyScopes: "[groups, email]"})))
	bad := Parse(in(map[string]string{}, newCM(DefaultRBACConfigMapName, map[string]string{
		RBACKeyScopes:    "[unterminated",
		RBACKeyMatchMode: "fuzzy",
		"policy.extra":   "x",
		RBACKeyPolicyCSV: "g, alice, role:nothing\n",
	})), WithPrevious(good))
	requireIssue(t, bad, DefaultRBACConfigMapName, RBACKeyScopes, SeverityError, "error unmarshalling scopes:")
	requireIssue(t, bad, DefaultRBACConfigMapName, RBACKeyScopes, SeverityError, "keeping the last valid value")
	assert.Equal(t, []string{"groups", "email"}, bad.RBAC().Scopes)
	requireIssue(t, bad, DefaultRBACConfigMapName, RBACKeyMatchMode, SeverityWarning, `unknown match mode "fuzzy"`)
	requireIssue(t, bad, DefaultRBACConfigMapName, "policy.extra", SeverityWarning, "unknown key")
	requireIssue(t, bad, DefaultRBACConfigMapName, RBACKeyPolicyCSV, SeverityWarning, "no policy grants it")
}
