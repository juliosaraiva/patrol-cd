package project

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

func newTestProject() *v1alpha1.AppProject {
	return &v1alpha1.AppProject{
		ObjectMeta: metav1.ObjectMeta{Name: "my-proj"},
		Spec: v1alpha1.AppProjectSpec{
			Roles:        []v1alpha1.ProjectRole{{Name: "my-role"}},
			Destinations: []v1alpha1.ApplicationDestination{{}},
		},
	}
}

func requireValidation(t *testing.T, err error, code Code, msg string) {
	t.Helper()
	require.Error(t, err)
	var ve *ValidationError
	require.True(t, errors.As(err, &ve), "error %v is not a *ValidationError", err)
	assert.True(t, errors.Is(err, ErrInvalidProject))
	assert.Equal(t, code, ve.Code, msg)
	assert.Equal(t, msg, ve.Message)
}

// TEN-009: each rule with the exact message and code.
func TestValidateProjectRules(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(p *v1alpha1.AppProject)
		code   Code
		msg    string
	}{
		{"dest name !*", func(p *v1alpha1.AppProject) { p.Spec.Destinations[0].Name = "!*" }, CodeInvalidArgument, "name has an invalid format, '!*'"},
		{"dest server !*", func(p *v1alpha1.AppProject) { p.Spec.Destinations[0].Server = "!*" }, CodeInvalidArgument, "server has an invalid format, '!*'"},
		{"dest namespace !*", func(p *v1alpha1.AppProject) { p.Spec.Destinations[0].Namespace = "!*" }, CodeInvalidArgument, "namespace has an invalid format, '!*'"},
		{"dest dup server", func(p *v1alpha1.AppProject) {
			p.Spec.Destinations = []v1alpha1.ApplicationDestination{{Server: "s", Namespace: "n"}, {Server: "s", Namespace: "n"}}
		}, CodeInvalidArgument, "destination 's/n' already added"},
		{"dest dup name", func(p *v1alpha1.AppProject) {
			p.Spec.Destinations = []v1alpha1.ApplicationDestination{{Name: "c", Namespace: "n"}, {Name: "c", Namespace: "n"}}
		}, CodeInvalidArgument, "destination 'c/n' already added"},
		{"source namespace dup", func(p *v1alpha1.AppProject) { p.Spec.SourceNamespaces = []string{"argocd", "argocd"} }, CodeInvalidArgument, "source namespace 'argocd' already added"},
		{"source !*", func(p *v1alpha1.AppProject) { p.Spec.SourceRepos = []string{"!*"} }, CodeInvalidArgument, "source repository has an invalid format, '!*'"},
		{"source dup", func(p *v1alpha1.AppProject) { p.Spec.SourceRepos = []string{"foo", "foo"} }, CodeInvalidArgument, "source repository 'foo' already added"},
		{"role dup", func(p *v1alpha1.AppProject) {
			p.Spec.Roles = append(p.Spec.Roles, v1alpha1.ProjectRole{Name: "my-role"})
		}, CodeAlreadyExists, "role 'my-role' already exists"},
		{"role name", func(p *v1alpha1.AppProject) { p.Spec.Roles[0].Name = "-a" }, CodeInvalidArgument, "invalid role name '-a'. Must consist of alphanumeric characters, '-' or '_', and must start and end with an alphanumeric character"},
		{"policy dup", func(p *v1alpha1.AppProject) {
			pol := "p, proj:my-proj:my-role, applications, get, my-proj/*, allow"
			p.Spec.Roles[0].Policies = []string{pol, pol}
		}, CodeAlreadyExists, "policy 'p, proj:my-proj:my-role, applications, get, my-proj/*, allow' already exists for role 'my-role'"},
		{"group dup", func(p *v1alpha1.AppProject) { p.Spec.Roles[0].Groups = []string{"g", "g"} }, CodeAlreadyExists, "group 'g' already exists for role 'my-role'"},
		{"window dup", func(p *v1alpha1.AppProject) {
			w := v1alpha1.SyncWindow{Kind: "allow", Schedule: "* * * * *", Duration: "1h", Applications: []string{"a"}}
			w2 := w
			w2.ManualSync, w2.Description, w2.SyncOverrun = true, "differs only in metadata", true
			p.Spec.SyncWindows = v1alpha1.SyncWindows{w, w2}
		}, CodeAlreadyExists, ""},
		{"window kind", func(p *v1alpha1.AppProject) {
			p.Spec.SyncWindows = v1alpha1.SyncWindows{{Kind: "wrong", Schedule: "* * * * *", Duration: "1h", Applications: []string{"a"}}}
		}, CodeInvalidArgument, "kind 'wrong' mismatch: can only be allow or deny"},
		{"window schedule", func(p *v1alpha1.AppProject) {
			p.Spec.SyncWindows = v1alpha1.SyncWindows{{Kind: "allow", Schedule: "* * *", Duration: "1h", Applications: []string{"a"}}}
		}, CodeInvalidArgument, "cannot parse schedule '* * *': expected exactly 5 fields, found 3: [* * *]"},
		{"window duration", func(p *v1alpha1.AppProject) {
			p.Spec.SyncWindows = v1alpha1.SyncWindows{{Kind: "allow", Schedule: "* * * * *", Duration: "1000days", Applications: []string{"a"}}}
		}, CodeInvalidArgument, `cannot parse duration '1000days': time: unknown unit "days" in duration "1000days"`},
		{"window description", func(p *v1alpha1.AppProject) {
			p.Spec.SyncWindows = v1alpha1.SyncWindows{{Kind: "allow", Schedule: "* * * * *", Duration: "1h", Applications: []string{"a"}, Description: strings.Repeat("x", 256)}}
		}, CodeInvalidArgument, "description must not exceed 255 characters"},
		{"window selectors", func(p *v1alpha1.AppProject) {
			p.Spec.SyncWindows = v1alpha1.SyncWindows{{Kind: "allow", Schedule: "* * * * *", Duration: "1h", Applications: []string{}}}
		}, CodeOutOfRange, "window 'allow':'* * * * *':'1h' requires one of application, cluster or namespace"},
		{"dsa server !", func(p *v1alpha1.AppProject) {
			p.Spec.DestinationServiceAccounts = []v1alpha1.ApplicationDestinationServiceAccount{{Server: "!abc", Namespace: "n", DefaultServiceAccount: "sa"}}
		}, CodeInvalidArgument, "server has an invalid format, '!abc'"},
		{"dsa namespace !", func(p *v1alpha1.AppProject) {
			p.Spec.DestinationServiceAccounts = []v1alpha1.ApplicationDestinationServiceAccount{{Server: "s", Namespace: "!abc", DefaultServiceAccount: "sa"}}
		}, CodeInvalidArgument, "namespace has an invalid format, '!abc'"},
		{"dsa empty sa", func(p *v1alpha1.AppProject) {
			p.Spec.DestinationServiceAccounts = []v1alpha1.ApplicationDestinationServiceAccount{{Server: "s", Namespace: "n", DefaultServiceAccount: "   "}}
		}, CodeInvalidArgument, "defaultServiceAccount has an invalid format, '   '"},
		{"dsa bad sa", func(p *v1alpha1.AppProject) {
			p.Spec.DestinationServiceAccounts = []v1alpha1.ApplicationDestinationServiceAccount{{Server: "s", Namespace: "n", DefaultServiceAccount: "test\\sa"}}
		}, CodeInvalidArgument, "defaultServiceAccount has an invalid format, 'test\\sa'"},
		{"dsa server glob", func(p *v1alpha1.AppProject) {
			p.Spec.DestinationServiceAccounts = []v1alpha1.ApplicationDestinationServiceAccount{{Server: "[[ech*", Namespace: "n", DefaultServiceAccount: "sa"}}
		}, CodeInvalidArgument, "server has an invalid format, '[[ech*'"},
		{"dsa namespace glob", func(p *v1alpha1.AppProject) {
			p.Spec.DestinationServiceAccounts = []v1alpha1.ApplicationDestinationServiceAccount{{Server: "s", Namespace: "[[ech*", DefaultServiceAccount: "sa"}}
		}, CodeInvalidArgument, "namespace has an invalid format, '[[ech*'"},
		{"dsa dup", func(p *v1alpha1.AppProject) {
			p.Spec.DestinationServiceAccounts = []v1alpha1.ApplicationDestinationServiceAccount{
				{Server: "s", Namespace: "n", DefaultServiceAccount: "a"}, {Server: "s", Namespace: "n", DefaultServiceAccount: "b"},
			}
		}, CodeInvalidArgument, "destinationServiceAccount 's/n' already added"},
	}
	for _, tt := range tests {
		p := newTestProject()
		require.NoError(t, ValidateProject(p))
		tt.mutate(p)
		err := ValidateProject(p)
		if tt.msg == "" {
			require.Error(t, err, tt.name)
			var ve *ValidationError
			require.True(t, errors.As(err, &ve))
			assert.Equal(t, tt.code, ve.Code, tt.name)
			assert.Contains(t, ve.Message, "sync window with kind 'allow', schedule '* * * * *', and duration '1h' already exists (hash=", tt.name)
			assert.Contains(t, ve.Message, ", duplicate detected)", tt.name)
			continue
		}
		requireValidation(t, err, tt.code, tt.msg)
	}
	requireValidation(t, ValidateProject(nil), CodeInvalidArgument, "missing payload 'project' in request")
}

func TestValidateProjectAcceptsValidProject(t *testing.T) {
	t.Parallel()
	p := &v1alpha1.AppProject{
		ObjectMeta: metav1.ObjectMeta{Name: "team"},
		Spec: v1alpha1.AppProjectSpec{
			SourceRepos:      []string{"https://github.com/org/*", "!https://github.com/org/secret"},
			SourceNamespaces: []string{"team-*", "/^ci-[0-9]+$/"},
			Destinations: []v1alpha1.ApplicationDestination{
				{Server: "*", Namespace: "!kube-system"},
				{Name: "cluster1", Namespace: "some-namespace"},
				{Name: "cluster2", Namespace: "some-namespace"},
				{Server: "*", Namespace: "*"},
			},
			Roles: []v1alpha1.ProjectRole{{
				Name: "deployer",
				Policies: []string{
					"p, proj:team:deployer, applications, sync, team/*, allow",
					"p, proj:team:deployer, applications, action/apps/Deployment/restart, team/ns/*, allow",
				},
				Groups: []string{"org:deployers", `"a,b"`},
			}},
			SyncWindows: v1alpha1.SyncWindows{
				{Kind: "deny", Schedule: "0 22 * * *", Duration: "1h", Namespaces: []string{"prod-*"}},
				{Kind: "deny", Schedule: "0 22 * * *", Duration: "1h", Namespaces: []string{"prod-*"}, TimeZone: "UTC"},
				{Kind: "allow", Schedule: "TZ=Europe/Paris 0 8 * * 1-5", Duration: "10h", Clusters: []string{"*"}},
			},
			DestinationServiceAccounts: []v1alpha1.ApplicationDestinationServiceAccount{
				{Server: "https://*", Namespace: "team-*", DefaultServiceAccount: "deployer"},
				{Server: "*", Namespace: "*", DefaultServiceAccount: "sys:readonly"},
			},
		},
	}
	assert.NoError(t, ValidateProject(p))
	assert.Equal(t, "", p.Spec.SyncWindows[0].TimeZone, "validation does not modify the project")
}

// Ports the semantics of upstream TestAppProject_ValidateRoleName.
func TestValidateRoleName(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"", " ", "my role", "my, role", "my,role", "my\nrole", "my\rrole", "my:role", "my-role-", "-my-role", "a.b", "a-"} {
		assert.Error(t, ValidateRoleName(bad), "%q", bad)
	}
	for _, good := range []string{"MY-ROLE", "1MY-ROLE1", "a", "a-b_c"} {
		assert.NoError(t, ValidateRoleName(good), "%q", good)
	}
}

// Ports the semantics of upstream Test_validateGroupName and TestAppProject_ValidateGroupName.
func TestValidateGroupName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		msg  string
	}{
		{"\"", "group '\"' contains invalid characters"},
		{"\"\"", "group '' is empty"},
		{"foo", ""},
		{"\"foo,bar,baz\"", ""},
		{"\"foo\"", ""},
		{"  \"foo\" ", "group 'foo' contains a leading space"},
		{"", "group '' is empty"},
		{" ", "group '' is empty"},
		{"foo,bar,baz", "group 'foo,bar,baz' must be quoted"},
		{"\"foo,bar,baz", "group '\"foo,bar,baz' must be quoted"},
		{"foo,bar,baz\"", "group 'foo,bar,baz\"' must be quoted"},
		{"foo,bar\",baz", "group 'foo,bar\",baz' must be quoted"},
		{"\"foo,\"bar,baz\"", "group 'foo,\"bar,baz' contains invalid characters"},
		{"foo\nbar", "group 'foo\nbar' contains invalid characters"},
		{"\"foo\nbar\"", "group 'foo\nbar' contains invalid characters"},
		{"\"foo", "group '\"foo' contains invalid characters"},
		{"foo\"", "group 'foo\"' contains invalid characters"},
		{"my, group", "group 'my, group' must be quoted"},
		{"my\rgroup", "group 'my\rgroup' contains invalid characters"},
		{" my:group", "group 'my:group' contains a leading space"},
		{"my:group ", "group 'my:group' contains a trailing space"},
		{"my:group", ""},
	}
	for _, tt := range tests {
		err := ValidateGroupName(tt.name)
		if tt.msg == "" {
			assert.NoError(t, err, "%q", tt.name)
			continue
		}
		requireValidation(t, err, CodeInvalidArgument, tt.msg)
	}
}

// Ports the semantics of upstream TestAppProject_InvalidPolicyRules / ValidPolicyRules,
// Test_isValidPolicyObject, Test_validatePolicy_* and TestIsValidAction (TEN-018).
func TestValidateRolePolicy(t *testing.T) {
	t.Parallel()
	bad := []struct{ policy, msg string }{
		{"g, proj:my-proj:my-role, applications, get, my-proj/*, allow", "invalid policy rule 'g, proj:my-proj:my-role, applications, get, my-proj/*, allow': must be of the form: 'p, sub, res, act, obj, eft'"},
		{"p, not, enough, parts", "must be of the form: 'p, sub, res, act, obj, eft'"},
		{"p, has, too, many, parts, to, split", "must be of the form: 'p, sub, res, act, obj, eft'"},
		{"p, , applications, get, my-proj/*, allow", "policy subject must be: 'proj:my-proj:my-role', not ''"},
		{"p, proj:my-proj, applications, get, my-proj/*, allow", "policy subject must be: 'proj:my-proj:my-role'"},
		{"p, proj:my-proj:, applications, get, my-proj/*, allow", "policy subject must be: 'proj:my-proj:my-role'"},
		{"p, ::, applications, get, my-proj/*, allow", "policy subject must be: 'proj:my-proj:my-role'"},
		{"p, proj:different-my-proj:my-role, applications, get, my-proj/*, allow", "policy subject must be: 'proj:my-proj:my-role', not 'proj:different-my-proj:my-role'"},
		{"p, proj:my-proj:my-role, , get, my-proj/*, allow", "project resource must be: 'applications', 'applicationsets', 'repositories', 'exec', 'logs' or 'clusters', not ''"},
		{"p, proj:my-proj:my-role, applicationz, get, my-proj/*, allow", "resource must be: 'applications'"},
		{"p, proj:my-proj:my-role, projects, get, my-proj, allow", "resource must be: 'applications'"},
		{"p, proj:my-proj:my-role, applications, , my-proj/*, allow", "invalid action ''"},
		{"p, proj:my-proj:my-role, applications, foo, my-proj/*, allow", "invalid action 'foo'"},
		{"p, proj:my-proj:my-role, applications, actionfoo, my-proj/*, allow", "invalid action 'actionfoo'"},
		{"p, proj:my-proj:my-role, applications, get, my-proj/, allow", "object must be of form 'my-proj/*', 'my-proj[/<NAMESPACE>]/<APPNAME>' or 'my-proj/<APPNAME>', not 'my-proj/'"},
		{"p, proj:my-proj:my-role, applications, get, /, allow", "object must be of form"},
		{"p, proj:my-proj:my-role, applications, get, different-my-proj/*, allow", "object must be of form"},
		{"p, proj:my-proj:my-role, applications, get, my-proj/https://x, allow", "object must be of form"},
		{"p, proj:my-proj:my-role, applications, get, my-proj/some~namespace/app, allow", "object must be of form"},
		{"p, proj:my-proj:my-role, applications, get, my-proj/ns/some^app, allow", "object must be of form"},
		{"p, proj:my-proj:my-role, applications, get, my-proj/*, ", "effect must be: 'allow' or 'deny'"},
		{"p, proj:my-proj:my-role, applications, get, my-proj/*, foo", "effect must be: 'allow' or 'deny'"},
	}
	for _, tt := range bad {
		err := ValidateRolePolicy("my-proj", "my-role", tt.policy)
		require.Error(t, err, tt.policy)
		assert.Contains(t, err.Error(), tt.msg, tt.policy)
		assert.True(t, strings.HasPrefix(err.Error(), "invalid policy rule '"+tt.policy+"': "), err.Error())
	}
	good := []string{
		"p,proj:my-proj:my-role,applications,get,my-proj/*,allow",
		"p, proj:my-proj:my-role, applications, get, my-proj/*, allow",
		"p, proj:my-proj:my-role, applications, get, my-proj/*, deny",
		"p, proj:my-proj:my-role, applications, get, my-proj/foo, allow",
		"p, proj:my-proj:my-role, applications, get, my-proj/*-foo, allow",
		"p, proj:my-proj:my-role, applications, get, my-proj/foo-*, allow",
		"p, proj:my-proj:my-role, applications, get, my-proj/*-*, allow",
		"p, proj:my-proj:my-role, applications, get, my-proj/*.*, allow",
		"p, proj:my-proj:my-role, applications, *, my-proj/foo, allow",
		"p, proj:my-proj:my-role, applications, create, my-proj/foo, allow",
		"p, proj:my-proj:my-role, applications, update, my-proj/foo, allow",
		"p, proj:my-proj:my-role, applications, update/*, my-proj/foo, allow",
		"p, proj:my-proj:my-role, applications, update/*/Pod/*, my-proj/foo, allow",
		"p, proj:my-proj:my-role, applications, sync, my-proj/foo, allow",
		"p, proj:my-proj:my-role, applications, override, my-proj/foo, allow",
		"p, proj:my-proj:my-role, applications, delete, my-proj/foo, allow",
		"p, proj:my-proj:my-role, applications, delete/*, my-proj/foo, allow",
		"p, proj:my-proj:my-role, applications, delete/*/Pod/*, my-proj/foo, allow",
		"p, proj:my-proj:my-role, applications, action/*, my-proj/foo, allow",
		"p, proj:my-proj:my-role, applications, action/apps/Deployment/restart, my-proj/foo, allow",
		"p, proj:my-proj:my-role, applications, get, my-proj/*/*, allow",
		"p, proj:my-proj:my-role, applications, get, my-proj/*/some-application, allow",
		"p, proj:my-proj:my-role, applications, get, my-proj/some-name*/some-application, allow",
		"p, proj:my-proj:my-role, applicationsets, *, my-proj/*, allow",
		"p, proj:my-proj:my-role, repositories, *, my-proj/*, allow",
		"p, proj:my-proj:my-role, clusters, *, my-proj/*, allow",
		"p, proj:my-proj:my-role, exec, *, my-proj/*, allow",
		"p, proj:my-proj:my-role, logs, *, my-proj/*, allow",
	}
	for _, policy := range good {
		assert.NoError(t, ValidateRolePolicy("my-proj", "my-role", policy), policy)
	}
	// The project name is quoted in the object regexp.
	require.Error(t, ValidateRolePolicy("some.project", "org-admin", "p, proj:some.project:org-admin, applications, *, some-project/*, allow"))
	require.NoError(t, ValidateRolePolicy("some.project", "org-admin", "p, proj:some.project:org-admin, applications, *, some.project/*, allow"))
}

func TestValidateSyncWindowAndEdits(t *testing.T) {
	t.Parallel()
	w := v1alpha1.SyncWindow{Kind: "allow", Schedule: "* * * * *", Duration: "1h"}
	require.NoError(t, ValidateSyncWindow(w))

	spec := &v1alpha1.AppProjectSpec{}
	assert.ErrorIs(t, AddSyncWindow(spec, v1alpha1.SyncWindow{Schedule: "* * * * *", Duration: "11", Applications: []string{"app1"}}), ErrWindowRequired)
	assert.ErrorIs(t, AddSyncWindow(spec, v1alpha1.SyncWindow{Kind: "allow", Applications: []string{"app1"}}), ErrWindowRequired)
	assert.Error(t, AddSyncWindow(spec, v1alpha1.SyncWindow{Kind: "allow", Schedule: "* * *", Duration: "1h", Applications: []string{"app1"}}))
	assert.Error(t, AddSyncWindow(spec, v1alpha1.SyncWindow{Kind: "deny", Schedule: "* * * * *", Duration: "33mm", Applications: []string{"app1"}}))
	require.NoError(t, AddSyncWindow(spec, v1alpha1.SyncWindow{Kind: "allow", Schedule: "1 * * * *", Duration: "1h", Applications: []string{"app1"}, Namespaces: []string{}, TimeZone: "error"}))
	require.Len(t, spec.SyncWindows, 1)
	assert.Equal(t, "UTC", spec.SyncWindows[0].TimeZone, "unknown zone rewritten")
	assert.Nil(t, spec.SyncWindows[0].Namespaces)
	require.NoError(t, AddSyncWindow(spec, v1alpha1.SyncWindow{Kind: "deny", Schedule: "3 * * * *", Duration: "1h", Clusters: []string{"cluster"}, Description: "Ticket AAAAA"}))
	assert.Equal(t, "Ticket AAAAA", spec.SyncWindows[1].Description)

	require.NoError(t, UpdateSyncWindow(&spec.SyncWindows[1], SyncWindowUpdate{Description: "Ticket BBBBB"}))
	assert.Equal(t, "Ticket BBBBB", spec.SyncWindows[1].Description)
	assert.EqualError(t, UpdateSyncWindow(&spec.SyncWindows[1], SyncWindowUpdate{TimeZone: "Europe/Paris"}),
		"cannot update: require one or more of schedule, duration, application, namespace, cluster or description")
	require.NoError(t, UpdateSyncWindow(&spec.SyncWindows[1], SyncWindowUpdate{Duration: "10h", Applications: []string{"a1", "a2"}}))
	assert.Equal(t, "10h", spec.SyncWindows[1].Duration)
	assert.Equal(t, []string{"a1", "a2"}, spec.SyncWindows[1].Applications)
	assert.Equal(t, "UTC", spec.SyncWindows[1].TimeZone)

	assert.EqualError(t, DeleteSyncWindow(spec, 3), "window with id '3' not found")
	require.NoError(t, DeleteSyncWindow(spec, 0))
	assert.Len(t, spec.SyncWindows, 1)

	spec2 := &v1alpha1.AppProjectSpec{SyncWindows: v1alpha1.SyncWindows{{TimeZone: ""}, {TimeZone: "Bad/Zone"}, {TimeZone: "UTC"}, {TimeZone: "Asia/Dhaka"}}}
	assert.True(t, NormalizeSyncWindowTimeZones(spec2))
	assert.Equal(t, []string{"UTC", "UTC", "UTC", "Asia/Dhaka"}, []string{spec2.SyncWindows[0].TimeZone, spec2.SyncWindows[1].TimeZone, spec2.SyncWindows[2].TimeZone, spec2.SyncWindows[3].TimeZone})
	assert.False(t, NormalizeSyncWindowTimeZones(spec2))
}

func TestSyncWindowIdentityHash(t *testing.T) {
	t.Parallel()
	base := v1alpha1.SyncWindow{Kind: "allow", Schedule: "* * * * *", Duration: "1h", Applications: []string{"a"}}
	h, err := SyncWindowIdentityHash(base)
	require.NoError(t, err)

	same := base
	same.ManualSync, same.Description, same.SyncOverrun = true, "x", true
	h2, err := SyncWindowIdentityHash(same)
	require.NoError(t, err)
	assert.Equal(t, h, h2, "manualSync, description and syncOverrun are not part of the identity")

	emptyVsNil := base
	emptyVsNil.Namespaces = []string{}
	h3, err := SyncWindowIdentityHash(emptyVsNil)
	require.NoError(t, err)
	assert.Equal(t, h, h3)

	for _, mutate := range []func(*v1alpha1.SyncWindow){
		func(w *v1alpha1.SyncWindow) { w.Kind = "deny" },
		func(w *v1alpha1.SyncWindow) { w.Schedule = "0 * * * *" },
		func(w *v1alpha1.SyncWindow) { w.Duration = "2h" },
		func(w *v1alpha1.SyncWindow) { w.Applications = []string{"b"} },
		func(w *v1alpha1.SyncWindow) { w.Namespaces = []string{"n"} },
		func(w *v1alpha1.SyncWindow) { w.Clusters = []string{"c"} },
		func(w *v1alpha1.SyncWindow) { w.TimeZone = "UTC" },
		func(w *v1alpha1.SyncWindow) { w.UseAndOperator = true },
	} {
		w := base
		w.Applications = []string{"a"}
		mutate(&w)
		hx, err := SyncWindowIdentityHash(w)
		require.NoError(t, err)
		assert.NotEqual(t, h, hx, "%+v", w)
	}
}

func TestValidateProjectPolicySyntax(t *testing.T) {
	t.Parallel()
	// A group containing a newline passes neither ValidateGroupName nor the policy parser;
	// the spec rule reports it first.
	p := newTestProject()
	p.Spec.Roles[0].Groups = []string{"a\nb"}
	requireValidation(t, ValidateProject(p), CodeInvalidArgument, "group 'a\nb' contains invalid characters")
}
