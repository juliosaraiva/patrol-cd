package rbac

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// canFixture mirrors the policy used by the upstream "rbac can" tests.
const canFixture = `
p, role:user, clusters, get, *, allow
p, role:user, clusters, get, https://kubernetes*, deny
p, role:user, projects, get, *, allow
p, role:user, applications, get, *, allow
p, role:user, applications, create, */*, allow
p, role:user, applications, delete, *, allow
p, role:user, applications, delete, */guestbook, deny
p, role:user, applicationsets, create, */*, allow
p, role:user, applicationsets, delete, */*, allow
p, role:test, certificates, get, *, allow
p, role:test, logs, get, */*, allow
p, role:test, exec, create, */*, allow
p, log-allow-user, logs, get, */*, allow
p, log-deny-user, logs, get, */*, deny
g, test, role:user
`

func TestValidateResourceAction(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		resource string
		action   string
		err      error
	}{
		{"valid", ResourceApplications, ActionCreate, nil},
		{"unknown resource", "invalid", ActionGet, ErrUnknownResource},
		{"unknown action", ResourceApplications, "invalid", ErrInvalidAction},
		{"action not valid for resource", ResourceLogs, ActionCreate, ErrInvalidAction},
		{"action with path", ResourceApplications, ActionAction + "/apps/Deployment/restart", nil},
		{"fine-grained update", ResourceApplications, SubResourceAction(ActionUpdate, "apps", "Deployment", "ns", "x"), nil},
		{"path on non-path action", ResourceApplications, ActionGet + "/apps/Deployment/restart", ErrInvalidAction},
		{"write repositories", ResourceWriteRepositories, ActionUpdate, nil},
		{"accounts get", ResourceAccounts, ActionGet, nil},
		{"extensions invoke", ResourceExtensions, ActionInvoke, nil},
		{"wildcard action", ResourceApplications, ActionAll, ErrInvalidAction},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateResourceAction(tt.resource, tt.action)
			if tt.err == nil {
				assert.NoError(t, err)
			} else {
				assert.ErrorIs(t, err, tt.err)
			}
		})
	}
}

func TestResolveResourceName(t *testing.T) {
	t.Parallel()
	assert.Equal(t, ResourceApplications, ResolveResourceName("app"))
	assert.Equal(t, ResourceCertificates, ResolveResourceName("cert"))
	assert.Equal(t, ResourceGPGKeys, ResolveResourceName("key"))
	assert.Equal(t, ResourceProjects, ResolveResourceName("proj"))
	assert.Equal(t, ResourceRepositories, ResolveResourceName("repo"))
	assert.Equal(t, ResourceWriteRepositories, ResolveResourceName(ResourceWriteRepositories))
	assert.Equal(t, "whatever", ResolveResourceName("whatever"))
}

func TestCan(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		builtin     string
		defaultRole string
		mode        string
		req         CanRequest
		allowed     bool
	}{
		{name: "get applications", builtin: BuiltinPolicy(), req: CanRequest{Subject: "role:user", Action: "get", Resource: "applications", Object: "*/*"}, allowed: true},
		{name: "get clusters, deny pattern does not match literal *", builtin: BuiltinPolicy(), req: CanRequest{Subject: "role:user", Action: "get", Resource: "clusters", Object: "*"}, allowed: true},
		{name: "deny for a specific cluster wins", builtin: BuiltinPolicy(), req: CanRequest{Subject: "role:user", Action: "get", Resource: "cluster", Object: "https://kubernetes.default.svc"}, allowed: false},
		{name: "deny for a specific app wins", builtin: BuiltinPolicy(), req: CanRequest{Subject: "role:user", Action: "delete", Resource: "app", Object: "default/guestbook"}, allowed: false},
		{name: "deny for a specific app does not match */*", builtin: BuiltinPolicy(), req: CanRequest{Subject: "role:user", Action: "delete", Resource: "app"}, allowed: true},
		{name: "via group mapping", builtin: BuiltinPolicy(), req: CanRequest{Subject: "test", Action: "delete", Resource: "app", Object: "default/other"}, allowed: true},
		{name: "get certificates", builtin: BuiltinPolicy(), req: CanRequest{Subject: "role:user", Action: "get", Resource: "certificates", Object: "*"}, allowed: false},
		{name: "get certificates by default role", builtin: BuiltinPolicy(), defaultRole: RoleReadOnly, req: CanRequest{Subject: "role:user", Action: "get", Resource: "certificates", Object: "*"}, allowed: true},
		{name: "default role without builtin policy", defaultRole: RoleReadOnly, req: CanRequest{Subject: "role:user", Action: "get", Resource: "certificates", Object: "*"}, allowed: false},
		{name: "regex mode with glob object", builtin: BuiltinPolicy(), defaultRole: RoleReadOnly, mode: RegexMatchMode, req: CanRequest{Subject: "role:user", Action: "get", Resource: "certificates", Object: ".*"}, allowed: false},
		{name: "get logs */*", builtin: BuiltinPolicy(), req: CanRequest{Subject: "role:test", Action: "get", Resource: "logs", Object: "*/*"}, allowed: true},
		{name: "get logs * expands to */*", builtin: BuiltinPolicy(), req: CanRequest{Subject: "role:test", Action: "get", Resource: "logs", Object: "*"}, allowed: true},
		{name: "get logs empty expands to */*", builtin: BuiltinPolicy(), req: CanRequest{Subject: "role:test", Action: "get", Resource: "logs"}, allowed: true},
		{name: "no-such-user", builtin: BuiltinPolicy(), req: CanRequest{Subject: "no-such-user", Action: "get", Resource: "logs"}, allowed: false},
		{name: "explicit deny", builtin: BuiltinPolicy(), req: CanRequest{Subject: "log-deny-user", Action: "get", Resource: "logs"}, allowed: false},
		{name: "explicit allow", builtin: BuiltinPolicy(), req: CanRequest{Subject: "log-allow-user", Action: "get", Resource: "logs"}, allowed: true},
		{name: "create exec", builtin: BuiltinPolicy(), req: CanRequest{Subject: "role:test", Action: "create", Resource: "exec"}, allowed: true},
		{name: "create applicationsets", builtin: BuiltinPolicy(), req: CanRequest{Subject: "role:user", Action: "create", Resource: "applicationsets"}, allowed: true},
		{name: "builtin admin", builtin: BuiltinPolicy(), req: CanRequest{Subject: "admin", Action: "update", Resource: "proj", Object: "default"}, allowed: true},
		{name: "empty subject", builtin: BuiltinPolicy(), req: CanRequest{Action: "get", Resource: "app"}, allowed: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnforcer(t, tt.builtin, canFixture, tt.defaultRole, tt.mode)
			tt.req.Strict = true
			d, err := e.Can(tt.req)
			require.NoError(t, err)
			assert.Equal(t, tt.allowed, d.Allowed, "%s (%s)", d.Reason, d.MatchedPolicy)
		})
	}
}

func TestCanRegexPolicy(t *testing.T) {
	t.Parallel()
	policy := `
p, role:user, clusters, get, .+, allow
p, role:user, clusters, get, https://kubernetes.*, deny
p, role:user, applications, get, .*, allow
p, role:user, applicationsets, delete, .*/.*, allow
`
	e := newEnforcer(t, "", policy, "role:unknown", RegexMatchMode)
	for _, c := range []struct {
		req     CanRequest
		allowed bool
	}{
		{CanRequest{Subject: "role:user", Action: "get", Resource: "clusters", Object: "https://example.com"}, true},
		{CanRequest{Subject: "role:user", Action: "get", Resource: "clusters", Object: "https://kubernetes.default.svc"}, false},
		{CanRequest{Subject: "role:user", Action: "get", Resource: "applications", Object: ".*/.*"}, true},
		{CanRequest{Subject: "role:user", Action: "delete", Resource: "applicationsets", Object: "a/b"}, true},
	} {
		d, err := e.Can(c.req)
		require.NoError(t, err)
		assert.Equal(t, c.allowed, d.Allowed, "%+v: %s", c.req, d.Reason)
	}
}

func TestCanStrictMode(t *testing.T) {
	t.Parallel()
	e := newEnforcer(t, BuiltinPolicy(), "p, x, foo, bar, *, allow", "", "")
	_, err := e.Can(CanRequest{Subject: "admin", Action: "get", Resource: "nonsense", Strict: true})
	require.ErrorIs(t, err, ErrUnknownResource)
	_, err = e.Can(CanRequest{Subject: "admin", Action: "sync", Resource: "logs", Strict: true})
	require.ErrorIs(t, err, ErrInvalidAction)

	d, err := e.Can(CanRequest{Subject: "x", Action: "bar", Resource: "foo"})
	require.NoError(t, err)
	assert.True(t, d.Allowed)

	e.SetEnforcementEnabled(false)
	d, err = e.Can(CanRequest{Subject: "nobody", Action: "get", Resource: "app"})
	require.NoError(t, err)
	assert.True(t, d.Allowed)
}
