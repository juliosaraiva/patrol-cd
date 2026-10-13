package rbac

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func token(project, role string) Subjects {
	return Subjects{ProjectRole: &ProjectRole{Project: project, Role: role}}
}

func myProjRoles() []Role {
	return []Role{{
		Name: "my-role",
		Policies: []string{
			"p, proj:my-proj:my-role, applications, create, my-proj/*, allow",
			"p, proj:my-proj:my-role, logs, get, my-proj/*, allow",
			"p, proj:my-proj:my-role, exec, create, my-proj/*, allow",
		},
		Groups: []string{"my-org:my-team"},
	}}
}

// Ports the semantics of upstream TestEnforceAllPolicies: built-in, user, project-role
// token and project group bindings all grant access.
func TestEnforceAllPolicySources(t *testing.T) {
	t.Parallel()
	e := newEnforcer(t,
		"p, alice, applications, create, my-proj/*, allow\np, alice, logs, get, my-proj/*, allow\np, alice, exec, create, my-proj/*, allow",
		"p, bob, applications, create, my-proj/*, allow\np, bob, logs, get, my-proj/*, allow\np, bob, exec, create, my-proj/*, allow",
		"", "", WithProjectPolicies("my-proj", myProjRoles()))

	type req struct{ resource, action string }
	reqs := []req{{"applications", "create"}, {"logs", "get"}, {"exec", "create"}}
	allowedFor := []Subjects{
		user("alice"),
		user("bob"),
		SubjectsFromClaims(map[string]any{"sub": "qwertyuiop", "federated_claims": map[string]any{"user_id": "bob"}}, DefaultScopes()),
		token("my-proj", "my-role"),
		SubjectsFromClaims(map[string]any{"groups": []string{"my-org:my-team"}}, DefaultScopes()),
	}
	for _, s := range allowedFor {
		for _, r := range reqs {
			d := e.Enforce(s, r.resource, r.action, "my-proj/my-app")
			assert.True(t, d.Allowed, "%+v %v: %s", s, r, d.Reason)
		}
	}
	for _, r := range reqs {
		assert.False(t, e.Enforce(user("cathy"), r.resource, r.action, "my-proj/my-app").Allowed)
	}
}

func TestProjectRoleScoping(t *testing.T) {
	t.Parallel()
	e := newEnforcer(t, BuiltinPolicy(), "p, proj:a:deployer, clusters, get, *, allow", "", "")
	require.NoError(t, e.SetProjectPolicies("a", []Role{
		{Name: "deployer", Policies: []string{
			"p, proj:a:deployer, applications, sync, a/*, allow",
			"p, proj:a:deployer, applications, get, a/*, allow",
			"p, proj:a:deployer, applications, sync, a/prod, deny",
		}, Groups: []string{"team-a"}},
	}))
	require.NoError(t, e.SetProjectPolicies("b", []Role{
		{Name: "viewer", Policies: []string{"p, proj:b:viewer, applications, get, b/*, allow"}, Groups: []string{"team-b"}},
	}))

	runChecks(t, e, []check{
		// Token for project a.
		{token("a", "deployer"), "applications", "sync", "a/web", true},
		{token("a", "deployer"), "applications", "sync", "a/prod", false},
		{token("a", "deployer"), "applications", "delete", "a/web", false},
		// The implicit "projects, get, <project>" rule.
		{token("a", "deployer"), "projects", "get", "a", true},
		{token("a", "deployer"), "projects", "update", "a", false},
		// A project role cannot reach another project's applications.
		{token("a", "deployer"), "applications", "get", "b/web", false},
		{token("a", "deployer"), "projects", "get", "b", false},
		{token("a", "deployer"), "applications", "get", "a/ns/web", true},
		// Global grants to the project subject still apply outside project scope.
		{token("a", "deployer"), "clusters", "get", "https://kubernetes.default.svc", true},
		// Groups bound in project a act only on a's objects.
		{user("u", "team-a"), "applications", "sync", "a/web", true},
		{user("u", "team-a"), "applications", "sync", "a/prod", false},
		{user("u", "team-a"), "applications", "get", "b/web", false},
		{user("u", "team-b"), "applications", "get", "b/web", true},
		{user("u", "team-b"), "applications", "get", "a/web", false},
		{user("u", "team-a", "team-b"), "applications", "get", "b/x", true},
		// Global admin still reaches everything.
		{user("admin"), "applications", "delete", "a/web", true},
	})

	d := e.Enforce(token("a", "deployer"), "applications", "get", "b/web")
	assert.Contains(t, d.Reason, `may only access project "a", not "b"`)

	d = e.Enforce(token("a", "deployer"), "applications", "sync", "a/web")
	assert.Equal(t, "project/a/role/deployer:1: p, proj:a:deployer, applications, sync, a/*, allow", d.MatchedPolicy)
}

func TestProjectRoleWithoutRegisteredProject(t *testing.T) {
	t.Parallel()
	// Objects of unregistered projects are evaluated against the global policy only.
	e := newEnforcer(t, "", "p, proj:a:ci, repositories, get, *, allow", "", "")
	require.NoError(t, e.SetProjectPolicies("a", []Role{{Name: "ci"}}))
	assert.True(t, e.Enforce(token("a", "ci"), "repositories", "get", "https://github.com/org/repo.git").Allowed)
	assert.True(t, e.Enforce(token("a", "ci"), "repositories", "get", "unregistered/https://x").Allowed)
	// A project with no roles still confines tokens of other projects.
	require.NoError(t, e.SetProjectPolicies("b", nil))
	assert.False(t, e.Enforce(token("a", "ci"), "repositories", "get", "b/https://x").Allowed)
}

func TestProjectPoliciesAppsInAnyNamespace(t *testing.T) {
	t.Parallel()
	e := newEnforcer(t, "", "", "", "")
	require.NoError(t, e.SetProjectPolicies("team", []Role{{
		Name:     "ns-limited",
		Policies: []string{"p, proj:team:ns-limited, applications, *, team/app-ns/*, allow"},
	}}))
	runChecks(t, e, []check{
		{token("team", "ns-limited"), "applications", "get", AppObject("patrol", "team", "app-ns", "web"), true},
		{token("team", "ns-limited"), "applications", "get", AppObject("patrol", "team", "other-ns", "web"), false},
		{token("team", "ns-limited"), "applications", "get", AppObject("patrol", "team", "patrol", "web"), false},
	})
}

func TestProjectPolicyValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		roles   []Role
		message string
	}{
		{name: "invalid role name", roles: []Role{{Name: "-bad"}}, message: "invalid role name"},
		{name: "duplicate role", roles: []Role{{Name: "r"}, {Name: "r"}}, message: "defined more than once"},
		{name: "wrong subject", roles: []Role{{Name: "r", Policies: []string{"p, proj:other:r, applications, get, a/*, allow"}}}, message: "policy subject must be"},
		{name: "not project scoped", roles: []Role{{Name: "r", Policies: []string{"p, proj:a:r, projects, update, a/*, allow"}}}, message: "project resource must be"},
		{name: "invalid action", roles: []Role{{Name: "r", Policies: []string{"p, proj:a:r, applications, action, a/*, allow"}}}, message: "invalid action"},
		{name: "object outside project", roles: []Role{{Name: "r", Policies: []string{"p, proj:a:r, applications, get, b/*, allow"}}}, message: "object must be"},
		{name: "wildcard object", roles: []Role{{Name: "r", Policies: []string{"p, proj:a:r, applications, get, */*, deny"}}}, message: "object must be"},
		{name: "invalid effect", roles: []Role{{Name: "r", Policies: []string{"p, proj:a:r, applications, get, a/*, permit"}}}, message: "effect must be"},
		{name: "g line in policies", roles: []Role{{Name: "r", Policies: []string{"g, alice, role:admin"}}}, message: "must be of the form"},
		{name: "unparseable line", roles: []Role{{Name: "r", Policies: []string{"p, proj:a:r"}}}, message: "need 6 fields"},
		{name: "unquoted comma group", roles: []Role{{Name: "r", Groups: []string{"a,b"}}}, message: "must be quoted"},
		{name: "empty group", roles: []Role{{Name: "r", Groups: []string{" "}}}, message: "is empty"},
		{name: "group with tab", roles: []Role{{Name: "r", Groups: []string{"a\tb"}}}, message: "invalid characters"},
		{name: "group with leading space", roles: []Role{{Name: "r", Groups: []string{" ab"}}}, message: "leading space"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnforcer(t, "", "", "", "")
			err := e.SetProjectPolicies("a", tt.roles)
			require.ErrorIs(t, err, ErrInvalidPolicy)
			assert.Contains(t, err.Error(), tt.message)
			assert.True(t, HasErrors(e.ProjectProblems("a")))
		})
	}
}

func TestInvalidProjectRoleKeepsOnlyDenies(t *testing.T) {
	t.Parallel()
	e := newEnforcer(t, "", "p, role:dev, applications, *, a/*, allow\ng, devs, role:dev", "", "")
	roles := []Role{{
		Name: "r",
		Policies: []string{
			"p, proj:a:r, applications, get, a/*, allow",
			"p, proj:a:r, applications, delete, a/prod, deny",
			"g, devs, role:admin", // invalid: escalation attempt
		},
		Groups: []string{"devs", `"quoted,group"`},
	}}
	err := e.SetProjectPolicies("a", roles)
	require.ErrorIs(t, err, ErrInvalidPolicy)
	assert.Contains(t, err.Error(), "role:admin")

	runChecks(t, e, []check{
		// The role's allow and implicit project get are dropped...
		{token("a", "r"), "applications", "get", "a/web", false},
		{token("a", "r"), "projects", "get", "a", false},
		// ...its valid deny still binds the role's groups...
		{user("u", "devs"), "applications", "delete", "a/prod", false},
		{user("u", "devs"), "applications", "delete", "a/web", true},
		// ...and the injected 'g' line grants nothing.
		{user("u", "devs"), "projects", "delete", "a", false},
	})
	// Quoted group names are unquoted like CSV.
	require.NoError(t, e.SetProjectPolicies("a", []Role{{Name: "r", Policies: []string{"p, proj:a:r, applications, get, a/*, allow"}, Groups: []string{`"quoted,group"`}}}))
	assert.True(t, e.Enforce(user("u", "quoted,group"), "applications", "get", "a/x").Allowed)
	assert.Empty(t, e.ProjectProblems("a"))
}

func TestInvalidProjectName(t *testing.T) {
	t.Parallel()
	e := newEnforcer(t, "", "", "", "")
	err := e.SetProjectPolicies("a:b", []Role{{Name: "r", Policies: []string{"p, proj:a:b:r, applications, get, a:b/*, allow"}}})
	require.ErrorIs(t, err, ErrInvalidPolicy)
	assert.Contains(t, err.Error(), "invalid project name")
}

func TestDeleteProjectPolicies(t *testing.T) {
	t.Parallel()
	e := newEnforcer(t, "", "", "", "")
	require.NoError(t, e.SetProjectPolicies("a", []Role{{Name: "r", Policies: []string{"p, proj:a:r, applications, get, a/*, allow"}}}))
	assert.True(t, e.Enforce(token("a", "r"), "applications", "get", "a/x").Allowed)
	e.DeleteProjectPolicies("a")
	e.DeleteProjectPolicies("never-registered")
	assert.False(t, e.Enforce(token("a", "r"), "applications", "get", "a/x").Allowed)
	assert.Nil(t, e.ProjectProblems("a"))
}

func TestProjectPoliciesFollowMatchMode(t *testing.T) {
	t.Parallel()
	e := newEnforcer(t, "", "", "", GlobMatchMode)
	require.NoError(t, e.SetProjectPolicies("a", []Role{{Name: "r", Policies: []string{"p, proj:a:r, applications, get, a/web-.*, allow"}}}))
	assert.False(t, e.Enforce(token("a", "r"), "applications", "get", "a/web-1").Allowed)
	require.NoError(t, e.SetPolicy("", "", "", RegexMatchMode))
	assert.True(t, e.Enforce(token("a", "r"), "applications", "get", "a/web-1").Allowed)
}

func TestProjectGroupCanBindDefaultRole(t *testing.T) {
	t.Parallel()
	// Upstream evaluates the default role against the project-augmented policy as well.
	e := newEnforcer(t, "", "", "role:everyone", "")
	require.NoError(t, e.SetProjectPolicies("a", []Role{{Name: "r", Policies: []string{"p, proj:a:r, applications, get, a/*, allow"}, Groups: []string{"role:everyone"}}}))
	d := e.Enforce(user("anyone"), "applications", "get", "a/x")
	assert.True(t, d.Allowed)
	assert.Contains(t, d.Reason, "default role")
	assert.False(t, e.Enforce(user("anyone"), "applications", "get", "b/x").Allowed)
}

func TestHelpers(t *testing.T) {
	t.Parallel()
	pr, ok := ParseProjectSubject("proj:a:b")
	assert.True(t, ok)
	assert.Equal(t, ProjectRole{Project: "a", Role: "b"}, pr)
	assert.Equal(t, "proj:a:b", pr.Subject())
	for _, s := range []string{"proj:a", "proj:a:b:c", "prj:a:b", "alice"} {
		_, ok := ParseProjectSubject(s)
		assert.False(t, ok, s)
	}

	tests := []struct {
		resource, object, project string
		ok                        bool
	}{
		{"applications", "p/app", "p", true},
		{"applications", "p/ns/app", "p", true},
		{"applicationsets", "p/https://github.com/x", "p", true},
		{"repositories", "https://github.com/x", "https:", true},
		{"logs", "app", "", false},
		{"projects", "p", "p", true},
		{"projects", "", "", false},
		{"certificates", "p/x", "", false},
	}
	for _, tt := range tests {
		p, ok := ProjectFromObject(tt.resource, tt.object)
		assert.Equal(t, tt.ok, ok, "%s %s", tt.resource, tt.object)
		assert.Equal(t, tt.project, p, "%s %s", tt.resource, tt.object)
	}
	assert.True(t, IsProjectScoped(ResourceExec))
	assert.False(t, IsProjectScoped(ResourceProjects))
	assert.Len(t, Resources(), 12)
	assert.Len(t, Actions(), 8)
	assert.True(t, strings.HasPrefix(BuiltinPolicy(), "#"))
}

// TEN-036: the default role applies to project tokens (before project confinement), and
// a global deny for the project subject overrides the role's allow.
func TestProjectTokenDefaultRoleAndGlobalDeny(t *testing.T) {
	t.Parallel()
	e := newEnforcer(t, BuiltinPolicy(), "p, proj:a:r, applications, sync, a/prod, deny", RoleReadOnly, "")
	require.NoError(t, e.SetProjectPolicies("a", []Role{{Name: "r", Policies: []string{"p, proj:a:r, applications, sync, a/*, allow"}}}))
	require.NoError(t, e.SetProjectPolicies("b", nil))
	runChecks(t, e, []check{
		{token("a", "r"), "applications", "get", "b/web", true},
		{token("a", "r"), "applications", "sync", "b/web", false},
		{token("a", "r"), "clusters", "get", "https://kubernetes.default.svc", true},
		{token("a", "r"), "applications", "sync", "a/web", true},
		{token("a", "r"), "applications", "sync", "a/prod", false},
	})
}

// TEN-079: a broken project policy does not affect global grants (upstream
// TestInvalidRuntimePolicy).
func TestBrokenProjectPolicyKeepsGlobalPolicy(t *testing.T) {
	t.Parallel()
	e := newEnforcer(t, BuiltinPolicy(), "", "", "")
	err := e.SetProjectPolicies("foo", []Role{{Name: "r", Policies: []string{"this, is, not, a, good, policy"}}})
	require.ErrorIs(t, err, ErrInvalidPolicy)
	runChecks(t, e, []check{
		{user("admin"), "applications", "update", "foo/bar", true},
		{user("role:readonly"), "applications", "update", "foo/bar", false},
		{user("role:readonly"), "applications", "get", "foo/bar", true},
	})
}
