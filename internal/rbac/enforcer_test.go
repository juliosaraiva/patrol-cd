package rbac

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newEnforcer returns an enforcer loaded with builtin + user policy; it fails the test
// when the policy does not load.
func newEnforcer(t testing.TB, builtin, user, defaultRole, mode string, opts ...Option) *Enforcer {
	t.Helper()
	e := NewEnforcer(append([]Option{WithLogger(discardLogger())}, opts...)...)
	require.NoError(t, e.SetPolicy(builtin, user, defaultRole, mode))
	return e
}

func user(name string, groups ...string) Subjects { return Subjects{User: name, Groups: groups} }

// check is one Enforce expectation.
type check struct {
	sub      Subjects
	resource string
	action   string
	object   string
	allowed  bool
}

func runChecks(t *testing.T, e *Enforcer, checks []check) {
	t.Helper()
	for _, c := range checks {
		d := e.Enforce(c.sub, c.resource, c.action, c.object)
		assert.Equal(t, c.allowed, d.Allowed, "%+v %s %s %s: %s (%s)", c.sub, c.resource, c.action, c.object, d.Reason, d.MatchedPolicy)
	}
}

func TestNoPolicyDeniesEverything(t *testing.T) {
	t.Parallel()
	e := NewEnforcer(WithLogger(discardLogger()))
	d := e.Enforce(user("admin"), ResourceApplications, ActionDelete, "foo/bar")
	assert.False(t, d.Allowed)
	assert.Empty(t, d.MatchedPolicy)

	// Without the built-in policy the admin account has no role.
	e = newEnforcer(t, "", "", "", "")
	assert.False(t, e.Enforce(user("admin"), ResourceApplications, ActionGet, "foo/bar").Allowed)
}

func TestBuiltinRoles(t *testing.T) {
	t.Parallel()
	e := newEnforcer(t, BuiltinPolicy(), "", "", "")
	runChecks(t, e, []check{
		{user("admin"), ResourceApplications, ActionGet, "foo/bar", true},
		{user("admin"), ResourceApplications, ActionDelete, "foo/bar", true},
		{user("admin"), ResourceApplications, SubResourceAction(ActionUpdate, "apps", "Deployment", "ns", "web"), "foo/bar", true},
		{user("admin"), ResourceApplications, CustomAction("apps", "Deployment", "restart"), "foo/bar", true},
		{user("admin"), ResourceExec, ActionCreate, "foo/bar", true},
		{user("admin"), ResourceWriteRepositories, ActionCreate, "https://example.com/repo.git", true},
		{user("admin"), ResourceAccounts, ActionUpdate, "alice", true},
		// role:admin does not include extensions in the baseline.
		{user("admin"), ResourceExtensions, ActionInvoke, "httpbin", false},
		{user("role:readonly"), ResourceApplications, ActionGet, "foo/bar", true},
		{user("role:readonly"), ResourceLogs, ActionGet, "foo/bar", true},
		{user("role:readonly"), ResourceClusters, ActionGet, "https://kubernetes.default.svc", true},
		{user("role:readonly"), ResourceApplications, ActionCreate, "foo/bar", false},
		{user("role:readonly"), ResourceApplications, ActionDelete, "foo/bar", false},
		{user("role:readonly"), ResourceExec, ActionCreate, "foo/bar", false},
		{user("role:admin"), ResourceApplications, ActionGet, "foo/bar", true},
		{user("role:admin"), ResourceApplications, ActionDelete, "foo/bar", true},
		{user("someone"), ResourceApplications, ActionGet, "foo/bar", false},
	})
}

// Ports the semantics of upstream TestResourceActionWildcards: resource/action/object
// wildcards, deny precedence regardless of line order, trailing wildcards.
func TestWildcardsAndDenyPrecedence(t *testing.T) {
	t.Parallel()
	policy := `
p, alice, *, get, foo/obj, allow
p, bob, repositories, *, foo/obj, allow
p, cathy, *, *, foo/obj, allow
p, dave, applications, get, foo/obj, allow
p, dave, applications/*, get, foo/obj, allow
p, eve, *, get, foo/obj, deny
p, mallory, repositories, *, foo/obj, deny
p, mallory, repositories, *, foo/obj, allow
p, mike, *, *, foo/obj, allow
p, mike, *, *, foo/obj, deny
p, trudy, applications, get, foo/obj, allow
p, trudy, applications/*, get, foo/obj, allow
p, trudy, applications/secrets, get, foo/obj, deny
p, danny, applications, get, */obj, allow
p, danny, applications, get, proj1/a*p1, allow
`
	e := newEnforcer(t, "", policy, "", "")
	runChecks(t, e, []check{
		{user("alice"), "applications", "get", "foo/obj", true},
		{user("alice"), "applications/resources", "get", "foo/obj", true},
		{user("alice"), "applications/resources", "delete", "foo/obj", false},
		{user("bob"), "repositories", "get", "foo/obj", true},
		{user("bob"), "repositories", "delete", "foo/obj", true},
		{user("bob"), "applications", "get", "foo/obj", false},
		{user("cathy"), "repositories", "get", "foo/obj", true},
		{user("cathy"), "repositories", "delete", "foo/obj", true},
		{user("cathy"), "applications", "get", "foo/obj", true},
		{user("cathy"), "applications/resources", "delete", "foo/obj", true},
		{user("dave"), "applications", "get", "foo/obj", true},
		{user("dave"), "applications/logs", "get", "foo/obj", true},
		{user("eve"), "applications", "get", "foo/obj", false},
		{user("eve"), "applications/resources", "get", "foo/obj", false},
		{user("mallory"), "repositories", "get", "foo/obj", false},
		{user("mallory"), "repositories", "delete", "foo/obj", false},
		{user("mallory"), "applications", "get", "foo/obj", false},
		{user("mike"), "repositories", "get", "foo/obj", false},
		{user("mike"), "applications/resources", "delete", "foo/obj", false},
		{user("trudy"), "applications", "get", "foo/obj", true},
		{user("trudy"), "applications/logs", "get", "foo/obj", true},
		{user("trudy"), "applications/secrets", "get", "foo/obj", false},
		{user("danny"), "applications", "get", "foo/obj", true},
		{user("danny"), "applications", "get", "bar/obj", true},
		{user("danny"), "applications", "get", "foo/bar", false},
		{user("danny"), "applications", "get", "proj1/app1", true},
		{user("danny"), "applications", "get", "proj1/app2", false},
	})

	d := e.Enforce(user("mallory"), "repositories", "get", "foo/obj")
	assert.Equal(t, "user:8: p, mallory, repositories, *, foo/obj, deny", d.MatchedPolicy)
	assert.Contains(t, d.Reason, "explicitly denied")
}

func TestProjectIsolationThroughRoles(t *testing.T) {
	t.Parallel()
	policy := `
p, role:foo-admin, *, *, foo/*, allow
p, role:bar-admin, *, *, bar/*, allow
p, role:foo-readonly, *, get, foo/*, allow
g, alice, role:foo-admin
g, bob, role:bar-admin
g, carol, role:foo-readonly
`
	e := newEnforcer(t, policy, "", "", "")
	runChecks(t, e, []check{
		{user("bob"), "applications", "delete", "bar/obj", true},
		{user("bob"), "applications", "delete", "foo/obj", false},
		{user("alice"), "applications", "delete", "foo/obj", true},
		{user("alice"), "applications", "delete", "bar/obj", false},
		{user("carol"), "applications", "get", "foo/obj", true},
		{user("carol"), "applications", "delete", "foo/obj", false},
		{user("carol"), "applications", "get", "bar/obj", false},
		{user("dave"), "applications", "get", "foo/obj", false},
	})
}

func TestURLAsObjectName(t *testing.T) {
	t.Parallel()
	policy := `
p, alice, repositories, *, foo/*, allow
p, bob, repositories, *, foo/https://github.com/argoproj/argo-cd.git, allow
`
	e := newEnforcer(t, "", policy, "", "")
	runChecks(t, e, []check{
		{user("alice"), "repositories", "delete", "foo/https://github.com/argoproj/argo-cd.git", true},
		{user("alice"), "repositories", "delete", "foo/https://github.com/golang/go.git", true},
		{user("bob"), "repositories", "delete", "foo/https://github.com/argoproj/argo-cd.git", true},
		{user("bob"), "repositories", "delete", "foo/https://github.com/golang/go.git", false},
	})
}

func TestGroupMappings(t *testing.T) {
	t.Parallel()
	policy := `
p, role:dev, applications, *, dev/*, allow
p, role:dev, applications, delete, dev/prod-*, deny
g, my-org:devs, role:dev
p, my-org:direct, applications, get, */*, allow
p, my-org:direct, applications, get, */*, allow
g, my-org:direct, role:nothing
p, my-org:unmapped, applications, get, */*, allow
p, alice, applications, delete, dev/prod-api, allow
p, mallory, applications, get, dev/*, deny
`
	e := newEnforcer(t, "", policy, "", "")
	runChecks(t, e, []check{
		{user("x", "my-org:devs"), "applications", "sync", "dev/web", true},
		{user("x", "my-org:devs"), "applications", "delete", "dev/prod-api", false},
		{user("x", "my-org:devs"), "applications", "get", "ops/web", false},
		// A group's own 'p' lines count only when the group is the member of a 'g' line.
		{user("x", "my-org:direct"), "applications", "get", "ops/web", true},
		{user("x", "my-org:unmapped"), "applications", "get", "ops/web", false},
		// Subjects are evaluated independently: the user's allow is not cancelled by the
		// deny the group's role carries, and a user's deny does not cancel a group allow.
		{user("alice", "my-org:devs"), "applications", "delete", "dev/prod-api", true},
		{user("mallory", "my-org:devs"), "applications", "get", "dev/web", true},
		{user("mallory"), "applications", "get", "dev/web", false},
		// Groups are compared literally.
		{user("x", "my-org:DEVS"), "applications", "sync", "dev/web", false},
	})

	d := e.Enforce(user("x", "other", "my-org:devs"), "applications", "sync", "dev/web")
	require.True(t, d.Allowed)
	assert.Contains(t, d.Reason, `group "my-org:devs"`)
}

func TestScopesSelectGroupClaims(t *testing.T) {
	t.Parallel()
	policy := `
p, role:admin-ish, applications, *, */*, allow
g, user@example.org, role:admin-ish
g, my-org:team-beta, role:admin-ish
`
	e := newEnforcer(t, "", policy, "", "")
	claims := map[string]any{
		"sub":    "Cg1hbGljZQ",
		"email":  "user@example.org",
		"groups": []any{"my-org:team-alpha"},
	}
	groupsOnly, err := ParseScopes("[groups]")
	require.NoError(t, err)
	assert.False(t, e.Enforce(SubjectsFromClaims(claims, groupsOnly), "applications", "sync", "p/a").Allowed)

	withEmail, err := ParseScopes("[groups, email]")
	require.NoError(t, err)
	assert.True(t, e.Enforce(SubjectsFromClaims(claims, withEmail), "applications", "sync", "p/a").Allowed)

	cognito := map[string]any{"sub": "x", "cognito:groups": []string{"my-org:team-beta"}}
	assert.False(t, e.Enforce(SubjectsFromClaims(cognito, DefaultScopes()), "applications", "sync", "p/a").Allowed)
	assert.True(t, e.Enforce(SubjectsFromClaims(cognito, []string{"cognito:groups"}), "applications", "sync", "p/a").Allowed)
}

func TestNestedRoles(t *testing.T) {
	t.Parallel()
	// Chain: alice -> r1 -> r2 -> ... -> r11. role r_i's permission is at depth i.
	var b strings.Builder
	b.WriteString("g, alice, r1\n")
	for i := 1; i <= 11; i++ {
		fmt.Fprintf(&b, "g, r%d, r%d\n", i, i+1)
		fmt.Fprintf(&b, "p, r%d, applications, get, p/app%d, allow\n", i, i)
	}
	// A cycle must not loop forever.
	b.WriteString("g, r3, alice\n")
	e := newEnforcer(t, "", b.String(), "", "")
	for i := 1; i <= 12; i++ {
		d := e.Enforce(user("alice"), "applications", "get", fmt.Sprintf("p/app%d", i))
		// Depth of r_i from alice is i; at most 10 hops are followed.
		assert.Equal(t, i <= 10, d.Allowed, "depth %d", i)
	}

	// Doc example: role:admin inherits role:readonly, users inherit role:admin.
	e = newEnforcer(t, BuiltinPolicy(), "g, my-org:team-beta, role:admin\ng, user@example.org, role:admin", "", "")
	runChecks(t, e, []check{
		{user("user@example.org"), "certificates", "get", "*", true},
		{user("x", "my-org:team-beta"), "projects", "delete", "default", true},
	})
}

func TestDefaultRole(t *testing.T) {
	t.Parallel()
	e := newEnforcer(t, BuiltinPolicy(), "", "", "")
	assert.False(t, e.Enforce(user("bob"), "applications", "get", "foo/bar").Allowed)

	e = newEnforcer(t, BuiltinPolicy(), "", RoleReadOnly, "")
	d := e.Enforce(user("bob"), "applications", "get", "foo/bar")
	assert.True(t, d.Allowed)
	assert.Contains(t, d.Reason, `default role "role:readonly"`)
	assert.Equal(t, "builtin:12: p, role:readonly, applications, get, */*, allow", d.MatchedPolicy)
	assert.Equal(t, RoleReadOnly, e.DefaultRole())
	// Anonymous requests (no subject at all) get the default role too.
	assert.True(t, e.Enforce(Subjects{}, "applications", "get", "foo/bar").Allowed)
	assert.False(t, e.Enforce(user("bob"), "applications", "delete", "foo/bar").Allowed)

	// A deny for the user cannot take away what the default role grants.
	e = newEnforcer(t, BuiltinPolicy(), "p, bob, applications, get, */*, deny", RoleReadOnly, "")
	assert.True(t, e.Enforce(user("bob"), "applications", "get", "foo/bar").Allowed)

	// A deny inside the default role's own closure applies to the default role.
	e = newEnforcer(t, BuiltinPolicy(), "p, role:readonly, applications, get, secret/*, deny", RoleReadOnly, "")
	assert.False(t, e.Enforce(user("bob"), "applications", "get", "secret/x").Allowed)

	// An unknown default role grants nothing.
	e = newEnforcer(t, BuiltinPolicy(), "", "role:unknown", "")
	assert.False(t, e.Enforce(user("bob"), "applications", "get", "foo/bar").Allowed)
}

func TestMatchModes(t *testing.T) {
	t.Parallel()
	t.Run("default is glob", func(t *testing.T) {
		t.Parallel()
		e := newEnforcer(t, "", `p, alice, clusters, get, "https://github.com/*/*.git", allow`, "", "")
		assert.Equal(t, GlobMatchMode, e.MatchMode())
		assert.True(t, e.Enforce(user("alice"), "clusters", "get", "https://github.com/argoproj/argo-cd.git").Allowed)
		assert.False(t, e.Enforce(user("alice"), "repositories", "get", "https://github.com/argoproj/argo-cd.git").Allowed)
		// '*' also matches '/' (no separators).
		assert.True(t, e.Enforce(user("alice"), "clusters", "get", "https://github.com/a/b/c.git").Allowed)
		assert.False(t, e.Enforce(user("alice"), "clusters", "get", "https://github.com/argo-cd.git").Allowed)
	})
	t.Run("glob syntax", func(t *testing.T) {
		t.Parallel()
		e := newEnforcer(t, "", `
p, alice, applications, get, "{dev,staging}/*", allow
p, alice, applications, sync, team-[a-c]/app?, allow
p, alice, applications, delete, \*/literal, allow
`, "", GlobMatchMode)
		runChecks(t, e, []check{
			{user("alice"), "applications", "get", "dev/x", true},
			{user("alice"), "applications", "get", "staging/x", true},
			{user("alice"), "applications", "get", "prod/x", false},
			{user("alice"), "applications", "sync", "team-b/app1", true},
			{user("alice"), "applications", "sync", "team-d/app1", false},
			{user("alice"), "applications", "sync", "team-b/app12", false},
			{user("alice"), "applications", "delete", "*/literal", true},
			{user("alice"), "applications", "delete", "x/literal", false},
		})
	})
	t.Run("regex", func(t *testing.T) {
		t.Parallel()
		e := newEnforcer(t, "", `p, alice, clusters, get, "https://github.com/argo[a-z]{4}/argo-[a-z]+.git", allow`, "", RegexMatchMode)
		assert.Equal(t, RegexMatchMode, e.MatchMode())
		assert.True(t, e.Enforce(user("alice"), "clusters", "get", "https://github.com/argoproj/argo-cd.git").Allowed)
		assert.False(t, e.Enforce(user("alice"), "clusters", "get", "https://github.com/argoproj/1argo-cd.git").Allowed)
	})
	t.Run("regex is unanchored", func(t *testing.T) {
		t.Parallel()
		e := newEnforcer(t, "", "p, alice, applications, get, team, allow\np, bob, applications, get, ^team/.*$, allow", "", RegexMatchMode)
		runChecks(t, e, []check{
			{user("alice"), "applications", "get", "my-team/x", true},
			{user("alice"), "applications", "get", "other/x", false},
			{user("bob"), "applications", "get", "team/x", true},
			{user("bob"), "applications", "get", "my-team/x", false},
		})
	})
	t.Run("glob star is an invalid regex and the user policy fails closed", func(t *testing.T) {
		t.Parallel()
		e := NewEnforcer(WithLogger(discardLogger()))
		err := e.SetPolicy("", "p, alice, applications, get, *, allow\np, alice, clusters, get, .*, allow", "", RegexMatchMode)
		require.ErrorIs(t, err, ErrInvalidPolicy)
		assert.Contains(t, err.Error(), `user:1: error: object "*" is not a valid regular expression`)
		require.Error(t, e.LoadError())
		// The whole policy is rejected, so even the valid rule does not apply, and no
		// evaluation can fail on the broken pattern.
		for _, r := range [][3]string{{"applications", "get", "p/a"}, {"clusters", "get", "https://x"}} {
			d := e.Enforce(user("alice"), r[0], r[1], r[2])
			assert.False(t, d.Allowed)
			assert.Contains(t, d.Reason, "RBAC policy failed to load")
		}
		// The next valid policy restores service.
		require.NoError(t, e.SetPolicy("", "p, alice, applications, get, .*, allow", "", RegexMatchMode))
		assert.True(t, e.Enforce(user("alice"), "applications", "get", "p/a").Allowed)
	})
	t.Run("invalid glob never matches", func(t *testing.T) {
		t.Parallel()
		e := newEnforcer(t, "", "p, alice, applications, get, */*, allow\np, alice, applications, get, {p/a, deny", "", "")
		// The broken deny is ignored, like upstream.
		assert.True(t, e.Enforce(user("alice"), "applications", "get", "p/a").Allowed)
	})
	t.Run("same policy, different meaning", func(t *testing.T) {
		t.Parallel()
		policy := "p, role:user, certificates, get, .+, allow\np, role:user, logs, get, .*/.*, allow"
		glob := newEnforcer(t, "", policy, "", GlobMatchMode)
		regex := newEnforcer(t, "", policy, "", RegexMatchMode)
		assert.False(t, glob.Enforce(user("role:user"), "certificates", "get", "x").Allowed)
		assert.True(t, glob.Enforce(user("role:user"), "certificates", "get", ".+").Allowed)
		assert.True(t, regex.Enforce(user("role:user"), "certificates", "get", "x").Allowed)
		assert.True(t, glob.Enforce(user("role:user"), "logs", "get", ".*/.*").Allowed)
		assert.True(t, regex.Enforce(user("role:user"), "logs", "get", "a/b").Allowed)
	})
}

func TestApplicationActionActions(t *testing.T) {
	t.Parallel()
	e := newEnforcer(t, `
p, alice, applications, action/*, my-proj/*, allow
p, bob, applications, action/argoproj.io/Rollout/*, my-proj/*, allow
p, cam, applications, action/argoproj.io/Rollout/resume, my-proj/*, allow
p, dan, applications, action//Pod/maintenance-off, default/*, allow
`, "", "", "")
	runChecks(t, e, []check{
		{user("alice"), "applications", CustomAction("argoproj.io", "Rollout", "resume"), "my-proj/my-app", true},
		{user("alice"), "applications", CustomAction("argoproj.io", "NewCrd", "abort"), "my-proj/my-app", true},
		{user("bob"), "applications", CustomAction("argoproj.io", "Rollout", "resume"), "my-proj/my-app", true},
		{user("bob"), "applications", CustomAction("argoproj.io", "NewCrd", "abort"), "my-proj/my-app", false},
		{user("cam"), "applications", CustomAction("argoproj.io", "Rollout", "resume"), "my-proj/my-app", true},
		{user("cam"), "applications", CustomAction("argoproj.io", "Rollout", "abort"), "my-proj/my-app", false},
		{user("dan"), "applications", CustomAction("", "Pod", "maintenance-off"), "default/x", true},
		{user("eve"), "applications", CustomAction("argoproj.io", "Rollout", "resume"), "my-proj/my-app", false},
	})
}

func TestSetPolicyReplacesAtomically(t *testing.T) {
	t.Parallel()
	e := newEnforcer(t, "", "p, alice, *, get, foo/obj, allow", "", "")
	assert.True(t, e.Enforce(user("alice"), "applications", "get", "foo/obj").Allowed)
	assert.False(t, e.Enforce(user("bob"), "applications", "get", "foo/obj").Allowed)

	require.NoError(t, e.SetPolicy("", "p, bob, *, get, foo/obj, allow", "", ""))
	assert.False(t, e.Enforce(user("alice"), "applications", "get", "foo/obj").Allowed)
	assert.True(t, e.Enforce(user("bob"), "applications", "get", "foo/obj").Allowed)

	require.NoError(t, e.SetPolicy("p, alice, *, get, foo/obj, allow", "", "", ""))
	assert.True(t, e.Enforce(user("alice"), "applications", "get", "foo/obj").Allowed)
	assert.False(t, e.Enforce(user("bob"), "applications", "get", "foo/obj").Allowed)

	require.NoError(t, e.SetPolicy("", "", "", ""))
	assert.False(t, e.Enforce(user("alice"), "applications", "get", "foo/obj").Allowed)
}

func TestInvalidPolicyFailsClosed(t *testing.T) {
	t.Parallel()
	e := newEnforcer(t, BuiltinPolicy(), "", RoleReadOnly, "")
	require.True(t, e.Enforce(user("admin"), "applications", "update", "foo/bar").Allowed)

	err := e.SetPolicy(BuiltinPolicy(), "p, alice, applications, get, */*, allow\nthis, is, not, a, good, policy", RoleReadOnly, "")
	require.ErrorIs(t, err, ErrInvalidPolicy)
	var pe *PolicyError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, "user", pe.Problems[0].Source)
	assert.Equal(t, 2, pe.Problems[0].Line)
	require.ErrorIs(t, e.LoadError(), ErrInvalidPolicy)

	d := e.Enforce(user("admin"), "applications", "update", "foo/bar")
	assert.False(t, d.Allowed)
	assert.Contains(t, d.Reason, "failed to load")
	assert.False(t, e.Enforce(user("bob"), "applications", "get", "foo/bar").Allowed, "default role is not applied either")
	cd, err := e.Can(CanRequest{Subject: "admin", Action: "get", Resource: "app"})
	require.NoError(t, err)
	assert.False(t, cd.Allowed)

	require.NoError(t, e.SetPolicy(BuiltinPolicy(), "", "", ""))
	require.NoError(t, e.LoadError())
	assert.True(t, e.Enforce(user("admin"), "applications", "update", "foo/bar").Allowed)
}

func TestEnforcementDisabled(t *testing.T) {
	t.Parallel()
	e := newEnforcer(t, "", "p, mike, *, get, foo/obj, deny", "", "")
	assert.False(t, e.Enforce(user("mike"), "applications", "get", "foo/obj").Allowed)
	e.SetEnforcementEnabled(false)
	assert.True(t, e.Enforce(user("mike"), "applications", "get", "foo/obj").Allowed)
	assert.True(t, e.Enforce(Subjects{}, "applications/resources", "delete", "foo/obj").Allowed)
	e.SetEnforcementEnabled(true)
	assert.False(t, e.Enforce(user("mike"), "applications", "get", "foo/obj").Allowed)
}

func TestEnforceErr(t *testing.T) {
	t.Parallel()
	e := newEnforcer(t, "", "", "", "")
	err := e.EnforceErr(user("admin"), "applications", "get", "foo/bar")
	require.ErrorIs(t, err, ErrPermissionDenied)
	assert.EqualError(t, err, "permission denied: applications, get, foo/bar, sub: admin")

	err = e.EnforceErr(Subjects{ProjectRole: &ProjectRole{Project: "default", Role: "admin"}}, "projects", "get", "default")
	assert.EqualError(t, err, "permission denied: projects, get, default, sub: proj:default:admin")

	err = e.EnforceErr(Subjects{}, "projects", "get", "default")
	assert.EqualError(t, err, "permission denied: projects, get, default")
	var pde *PermissionDeniedError
	require.True(t, errors.As(err, &pde))
	assert.False(t, pde.Decision.Allowed)

	e = newEnforcer(t, BuiltinPolicy(), "", "", "")
	assert.NoError(t, e.EnforceErr(user("admin"), "applications", "get", "foo/bar"))
}

func TestApplyConfigMap(t *testing.T) {
	t.Parallel()
	e := NewEnforcer(WithLogger(discardLogger()))
	err := e.ApplyConfigMap(BuiltinPolicy(), map[string]string{
		ConfigMapPolicyCSVKey:     "p, role:user, applications, get, */*, allow\ng, test, role:user",
		"policy.overlay.csv":      "p, role:tester, applications, *, */*, allow\ng, my-org:team-qa, role:tester",
		ConfigMapPolicyDefaultKey: "role:unknown",
		ConfigMapMatchModeKey:     GlobMatchMode,
		ConfigMapScopesKey:        "[groups, email]",
	})
	require.NoError(t, err)
	assert.Equal(t, "role:unknown", e.DefaultRole())
	runChecks(t, e, []check{
		{user("test"), "applications", "get", "a/b", true},
		{user("x", "my-org:team-qa"), "applications", "sync", "a/b", true},
		{user("x"), "applications", "get", "a/b", false},
	})

	err = e.ApplyConfigMap(BuiltinPolicy(), map[string]string{"policy.bad.csv": "p, x"})
	var pe *PolicyError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, "policy.bad.csv", pe.Problems[0].Source)
	assert.Equal(t, 1, pe.Problems[0].Line)
}

func TestReservedProjectSubjectNames(t *testing.T) {
	t.Parallel()
	// A user or group whose name looks like a project-role subject must not acquire the
	// role's permissions: only verified tokens (Subjects.ProjectRole) can.
	e := newEnforcer(t, "", "p, proj:a:r, clusters, get, *, allow\ng, proj:a:r, role:x\np, role:x, clusters, get, *, allow", "", "")
	assert.False(t, e.Enforce(user("proj:a:r"), "clusters", "get", "c").Allowed)
	assert.False(t, e.Enforce(user("x", "proj:a:r"), "clusters", "get", "c").Allowed)
	assert.True(t, e.Enforce(Subjects{ProjectRole: &ProjectRole{Project: "a", Role: "r"}}, "clusters", "get", "c").Allowed)
	assert.False(t, e.Enforce(Subjects{ProjectRole: &ProjectRole{Project: "", Role: "r"}}, "clusters", "get", "c").Allowed)
}

func TestConcurrentEnforceAndUpdates(t *testing.T) {
	t.Parallel()
	e := newEnforcer(t, BuiltinPolicy(), "", "", "")
	done := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-done:
					return
				default:
				}
				_ = e.Enforce(user("bob", "devs"), "applications", "get", "proj/app")
				_ = e.Enforce(Subjects{ProjectRole: &ProjectRole{Project: "proj", Role: "ci"}}, "applications", "sync", "proj/app")
				_, _ = e.Can(CanRequest{Subject: "bob", Action: "get", Resource: "app"})
			}
		})
	}
	for i := range 200 {
		mode := GlobMatchMode
		if i%2 == 0 {
			mode = RegexMatchMode
		}
		require.NoError(t, e.SetPolicy(BuiltinPolicy(), "g, devs, role:readonly", RoleReadOnly, mode))
		require.NoError(t, e.SetProjectPolicies("proj", []Role{{Name: "ci", Policies: []string{"p, proj:proj:ci, applications, sync, proj/*, allow"}}}))
		e.SetEnforcementEnabled(i%3 != 0)
		if i%5 == 0 {
			e.DeleteProjectPolicies("proj")
		}
	}
	close(done)
	wg.Wait()
	e.SetEnforcementEnabled(true)
	assert.True(t, e.Enforce(user("bob", "devs"), "applications", "get", "proj/app").Allowed)
}

// TEN-074 (parity security behavior, documented upstream): an SSO group named like a
// local account inherits that account's bindings, because groups are evaluated as
// subjects when they are the member of a 'g' line.
func TestGroupNamedLikeLocalAccountInheritsBindings(t *testing.T) {
	t.Parallel()
	e := newEnforcer(t, BuiltinPolicy(), "", "", "")
	assert.True(t, e.Enforce(user("mallory", "admin"), "projects", "delete", "default").Allowed)
	assert.False(t, e.Enforce(user("mallory", "not-admin"), "projects", "delete", "default").Allowed)
}

func TestUnknownMatchModeBehavesAsGlob(t *testing.T) {
	t.Parallel()
	e := newEnforcer(t, "", "p, alice, applications, get, p/*, allow", "", "fuzzy")
	assert.Equal(t, GlobMatchMode, e.MatchMode())
	assert.True(t, e.Enforce(user("alice"), "applications", "get", "p/x").Allowed)
}

// In regex mode the glob-style built-in patterns ("*", "*/*") are not valid regular
// expressions, so built-in grants fail closed (the evaluation errors are denials), as in
// upstream where Casbin's regex matcher errors on them.
func TestRegexModeWithGlobStyleBuiltinPolicy(t *testing.T) {
	t.Parallel()
	e := NewEnforcer(WithLogger(discardLogger()))
	require.NoError(t, e.SetPolicy(BuiltinPolicy(), "", RoleReadOnly, RegexMatchMode))
	d := e.Enforce(user("admin"), "applications", "get", "p/a")
	assert.False(t, d.Allowed)
	assert.Contains(t, d.Reason, "policy evaluation failed")
	problems, err := Validate(nil, ValidateOptions{MatchMode: RegexMatchMode, Builtin: BuiltinPolicy()})
	require.NoError(t, err)
	assert.NotEmpty(t, problems, "invalid regex patterns are reported as warnings")
}
