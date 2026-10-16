package rbac

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requireNoUnevaluableProjectRules fails when a compiled rule of the project has a
// pattern that does not compile (it could fail or silently never match at evaluation).
func requireNoUnevaluableProjectRules(t *testing.T, e *Enforcer, project string) {
	t.Helper()
	entry := e.load().projects[project]
	require.NotNil(t, entry, "project %q is not registered", project)
	assert.Empty(t, unevaluableRules(entry.policy))
}

// findProblem returns the first error-severity problem of source at line.
func findProblem(problems []ValidationError, source string, line int) (ValidationError, bool) {
	for _, p := range problems {
		if p.Severity == SeverityError && p.Source == source && p.Line == line {
			return p, true
		}
	}
	return ValidationError{}, false
}

// Regression for the rapid fail file of TestPropertyAddingDenyNeverWidensAccess: in
// regex mode, a project role line whose action is '*' (not a valid regular expression)
// was accepted, then failed at evaluation and denied every member of the role's groups
// every 'applications' request in the project, overriding their global grants. Adding
// an unrelated invalid line reduced the role to its denies and "restored" the grant.
func TestRegressionProjectRegexLineDoesNotPoisonGlobalGrant(t *testing.T) {
	t.Parallel()
	global := "g, bob, role:b\np, bob, applications, get, .*, allow"
	e := newEnforcer(t, "", global, "", RegexMatchMode)
	roles := []Role{{Name: "r", Policies: []string{
		"p, proj:p1:r, applications, get, p1/.*, allow",
		"p, proj:p1:r, applications, get, p1/.*, allow",
		"p, proj:p1:r, applications, get, p1/.*, allow",
		"p, proj:p1:r, applications, *, p1/.*, allow",
	}, Groups: []string{"role:b"}}}

	err := e.SetProjectPolicies("p1", roles)
	require.ErrorIs(t, err, ErrInvalidPolicy)
	bad, ok := findProblem(e.ProjectProblems("p1"), "project/p1/role/r", 4)
	require.True(t, ok, "line 4 must be reported: %v", e.ProjectProblems("p1"))
	assert.Equal(t, "p, proj:p1:r, applications, *, p1/.*, allow", bad.Text)
	assert.Contains(t, bad.Message, `action "*" is not a valid regular expression`)
	assert.Contains(t, bad.Message, "the line is ignored")
	assert.Contains(t, err.Error(), "project/p1/role/r:4")
	requireNoUnevaluableProjectRules(t, e, "p1")

	check := func(t *testing.T) {
		t.Helper()
		d := e.Enforce(user("bob"), ResourceApplications, ActionGet, "p1/a")
		assert.True(t, d.Allowed, d.Reason)
		assert.Equal(t, "user:2: p, bob, applications, get, .*, allow", d.MatchedPolicy)
		for _, act := range []string{ActionGet, ActionSync, ActionDelete, "update/apps/Deployment/ns/web"} {
			d := e.Enforce(user("bob"), ResourceApplications, act, "p1/a")
			assert.NotContains(t, d.Reason, "evaluation failed", act)
		}
		// The role is reduced to its denies (none): its allows and implicit get are gone.
		assert.False(t, e.Enforce(token("p1", "r"), ResourceApplications, ActionGet, "p1/a").Allowed)
		assert.False(t, e.Enforce(token("p1", "r"), ResourceProjects, ActionGet, "p1").Allowed)
	}
	check(t)

	// The fail file's step: adding an unrelated invalid deny changes nothing now.
	roles[0].Policies = append(roles[0].Policies, "p, proj:p1:r, applications, *, p2/a, deny")
	require.ErrorIs(t, e.SetProjectPolicies("p1", roles), ErrInvalidPolicy)
	_, ok = findProblem(e.ProjectProblems("p1"), "project/p1/role/r", 5)
	assert.True(t, ok)
	check(t)

	// In glob mode the same role is valid: '*' and 'p1/.*' are valid globs.
	roles[0].Policies = roles[0].Policies[:4]
	require.NoError(t, e.SetPolicy("", "g, bob, role:b\np, bob, applications, get, *, allow", "", GlobMatchMode))
	require.NoError(t, e.SetProjectPolicies("p1", roles))
	assert.True(t, e.Enforce(user("bob"), ResourceApplications, ActionGet, "p1/a").Allowed)
	assert.True(t, e.Enforce(token("p1", "r"), ResourceApplications, ActionSync, "p1/.x").Allowed)
}

// Every pattern position of a project role line, in both modes: a value that cannot be
// compiled (or that the structural AppProject rules reject first) makes the line
// invalid, is reported with project, role and line number, never reaches the compiled
// policy, and never fails a decision of another rule or subject.
func TestProjectRolePatternValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mode    string
		project string
		line    string // %s is the role subject
		message string
	}{
		// Subject: not a pattern (exact name), so it must be the role's own subject.
		{name: "regex subject star", mode: RegexMatchMode, project: "p1", line: "p, proj:p1:r*, applications, get, p1/a, allow", message: "policy subject must be"},
		{name: "regex subject paren", mode: RegexMatchMode, project: "p1", line: "p, (, applications, get, p1/a, allow", message: "policy subject must be"},
		{name: "glob subject bracket", mode: GlobMatchMode, project: "p1", line: "p, proj:p1:r[, applications, get, p1/a, allow", message: "policy subject must be"},
		// Resource: must be a literal project-scoped resource.
		{name: "regex resource star", mode: RegexMatchMode, project: "p1", line: "p, %s, *, get, p1/a, allow", message: "project resource must be"},
		{name: "regex resource bracket", mode: RegexMatchMode, project: "p1", line: "p, %s, [, get, p1/a, allow", message: "project resource must be"},
		{name: "glob resource bracket", mode: GlobMatchMode, project: "p1", line: "p, %s, applications[, get, p1/a, allow", message: "project resource must be"},
		// Action: compiled with the evaluator's compiler.
		{name: "regex action star", mode: RegexMatchMode, project: "p1", line: "p, %s, applications, *, p1/a, allow", message: `action "*" is not a valid regular expression`},
		{name: "regex action bracket", mode: RegexMatchMode, project: "p1", line: "p, %s, applications, action/[, p1/a, deny", message: `action "action/[" is not a valid regular expression`},
		{name: "regex action paren", mode: RegexMatchMode, project: "p1", line: "p, %s, applications, update/(, p1/a, allow", message: `action "update/(" is not a valid regular expression`},
		{name: "glob action bracket", mode: GlobMatchMode, project: "p1", line: "p, %s, applications, action/[, p1/a, allow", message: `action "action/[" is not a valid glob pattern`},
		{name: "glob action brace", mode: GlobMatchMode, project: "p1", line: "p, %s, applications, action/{x, p1/a, deny", message: `action "action/{x" is not a valid glob pattern`},
		// Object: the project's object rule only allows '*', word characters, '-' and
		// '.', so a broken object needs '**' or metacharacters in the project name.
		{name: "regex object double star", mode: RegexMatchMode, project: "p1", line: "p, %s, applications, get, p1/**, allow", message: `object "p1/**" is not a valid regular expression`},
		{name: "regex object paren project", mode: RegexMatchMode, project: "p(1", line: "p, %s, applications, get, p(1/a, deny", message: `object "p(1/a" is not a valid regular expression`},
		{name: "regex object bracket project", mode: RegexMatchMode, project: "p[1", line: "p, %s, applications, get, p[1/*, allow", message: `object "p[1/*" is not a valid regular expression`},
		{name: "glob object bracket project", mode: GlobMatchMode, project: "p[1", line: "p, %s, applications, get, p[1/a, allow", message: `object "p[1/a" is not a valid glob pattern`},
		{name: "glob object brace project", mode: GlobMatchMode, project: "p{1", line: "p, %s, logs, get, p{1/*, deny", message: `object "p{1/*" is not a valid glob pattern`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			all := "*"
			if tt.mode == RegexMatchMode {
				all = ".*"
			}
			// bob holds a global grant and is a member of the role through role:b.
			global := strings.ReplaceAll("g, bob, role:b\np, bob, applications, ALL, ALL, allow\np, bob, logs, ALL, ALL, allow\np, bob, projects, get, ALL, allow", "ALL", all)
			e := newEnforcer(t, "", global, "", tt.mode)
			subject := ProjectRole{Project: tt.project, Role: "r"}.Subject()
			line := strings.ReplaceAll(tt.line, "%s", subject)
			err := e.SetProjectPolicies(tt.project, []Role{{Name: "r", Policies: []string{line}, Groups: []string{"role:b"}}})
			require.ErrorIs(t, err, ErrInvalidPolicy)
			source := "project/" + tt.project + "/role/r"
			p, ok := findProblem(e.ProjectProblems(tt.project), source, 1)
			require.True(t, ok, "%v", e.ProjectProblems(tt.project))
			assert.Equal(t, line, p.Text)
			assert.Contains(t, p.Message, tt.message)
			assert.Contains(t, p.Message, "the line is ignored")
			requireNoUnevaluableProjectRules(t, e, tt.project)

			app := tt.project + "/a"
			for _, r := range [][3]string{
				{ResourceApplications, ActionGet, app},
				{ResourceApplications, ActionSync, app},
				{ResourceApplications, "action/x", app},
				{ResourceApplications, "update/(", app},
				{ResourceLogs, ActionGet, app},
				{ResourceProjects, ActionGet, tt.project},
			} {
				d := e.Enforce(user("bob"), r[0], r[1], r[2])
				assert.True(t, d.Allowed, "%v: %s", r, d.Reason)
				assert.NotContains(t, d.Reason, "evaluation failed")
				// The invalid role grants its token nothing.
				d = e.Enforce(Subjects{ProjectRole: &ProjectRole{Project: tt.project, Role: "r"}}, r[0], r[1], r[2])
				assert.False(t, d.Allowed, "%v: %s", r, d.Reason)
				assert.NotContains(t, d.Reason, "evaluation failed")
			}
		})
	}
}

// The implicit "projects, get, <project>" rule matches the project name exactly, so a
// name with pattern metacharacters can neither break evaluation (an invalid regular
// expression would fail every evaluation reaching the rule) nor silently never match.
func TestImplicitProjectRuleMatchesNameExactly(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ mode, project string }{
		{RegexMatchMode, "p(1"},
		{RegexMatchMode, "p[1"},
		{RegexMatchMode, "^p1"},
		{RegexMatchMode, "p.1"},
		{GlobMatchMode, "p[1"},
		{GlobMatchMode, "p{1"},
		{GlobMatchMode, `p\1`},
		{GlobMatchMode, "p*1"},
	} {
		t.Run(tt.mode+"/"+tt.project, func(t *testing.T) {
			t.Parallel()
			e := newEnforcer(t, "", "", "", tt.mode)
			require.NoError(t, e.SetProjectPolicies(tt.project, []Role{{Name: "r", Groups: []string{"team"}}}))
			requireNoUnevaluableProjectRules(t, e, tt.project)
			for _, s := range []Subjects{token(tt.project, "r"), user("u", "team")} {
				d := e.Enforce(s, ResourceProjects, ActionGet, tt.project)
				assert.True(t, d.Allowed, "%+v: %s", s, d.Reason)
			}
			assert.False(t, e.Enforce(token(tt.project, "r"), ResourceProjects, ActionUpdate, tt.project).Allowed)
		})
	}
}

// Changing the match mode revalidates every registered project: a line that is a valid
// glob but an invalid regular expression becomes invalid (logged, readable through
// ProjectProblems), and becomes valid again when the mode changes back.
func TestProjectLinesRevalidatedOnMatchModeChange(t *testing.T) {
	t.Parallel()
	e := newEnforcer(t, "", "", "", GlobMatchMode)
	require.NoError(t, e.SetProjectPolicies("a", []Role{{Name: "r", Policies: []string{"p, proj:a:r, applications, *, a/*, allow"}}}))
	assert.True(t, e.Enforce(token("a", "r"), ResourceApplications, ActionSync, "a/x").Allowed)

	require.NoError(t, e.SetPolicy("", "", "", RegexMatchMode))
	p, ok := findProblem(e.ProjectProblems("a"), "project/a/role/r", 1)
	require.True(t, ok, "%v", e.ProjectProblems("a"))
	assert.Contains(t, p.Message, `action "*" is not a valid regular expression`)
	requireNoUnevaluableProjectRules(t, e, "a")
	d := e.Enforce(token("a", "r"), ResourceApplications, ActionSync, "a/x")
	assert.False(t, d.Allowed)
	assert.NotContains(t, d.Reason, "evaluation failed")

	require.NoError(t, e.SetPolicy("", "", "", GlobMatchMode))
	assert.Empty(t, e.ProjectProblems("a"))
	assert.True(t, e.Enforce(token("a", "r"), ResourceApplications, ActionSync, "a/x").Allowed)
}

// Global rules keep failing closed: only the built-in source may contain a pattern that
// cannot be evaluated, and reaching it denies that one subject for that request. The
// failure is deliberately not isolated to the rule: here the failing rule is a deny, and
// treating it as a non-match would let the allow next to it through.
func TestBuiltinUnevaluablePatternFailsClosedForTheSubject(t *testing.T) {
	t.Parallel()
	builtin := "p, role:x, applications, get, .*, allow\np, role:x, applications, get, *, deny\ng, alice, role:x"
	userSources := []Source{{Name: "policy.csv", CSV: "p, bob, applications, get, .*, allow\ng, devs, role:x"}}
	e := NewEnforcer(WithLogger(discardLogger()))
	require.NoError(t, e.SetPolicySources(builtin, userSources, "", RegexMatchMode))

	d := e.Enforce(Subjects{User: "alice"}, ResourceApplications, ActionGet, "p/a")
	assert.False(t, d.Allowed, "a failing deny must not be skipped")
	assert.Contains(t, d.Reason, `policy evaluation failed for subject "alice"`)
	assert.Equal(t, "builtin:2: p, role:x, applications, get, *, deny", d.MatchedPolicy)
	// The failing object is only reached when resource and action match first.
	assert.False(t, e.Enforce(Subjects{User: "alice"}, ResourceApplications, ActionSync, "p/a").Allowed)
	assert.NotContains(t, e.Enforce(Subjects{User: "alice"}, ResourceApplications, ActionSync, "p/a").Reason, "evaluation failed")
	// Other subjects, including other subjects of the same request, are unaffected.
	assert.True(t, e.Enforce(Subjects{User: "bob"}, ResourceApplications, ActionGet, "p/a").Allowed)
	assert.True(t, e.Enforce(Subjects{User: "bob", Groups: []string{"devs"}}, ResourceApplications, ActionGet, "p/a").Allowed)
	assert.False(t, e.Enforce(Subjects{User: "carol", Groups: []string{"devs"}}, ResourceApplications, ActionGet, "p/a").Allowed)

	// The same lines in a user source do not load at all, whatever Builtin says.
	userSources = []Source{{Name: "policy.csv", CSV: builtin, Builtin: true}}
	err := e.SetPolicySources("", userSources, "", RegexMatchMode)
	require.ErrorIs(t, err, ErrInvalidPolicy)
	assert.Contains(t, err.Error(), "policy.csv:2: error: object \"*\" is not a valid regular expression")
	assert.Contains(t, e.Enforce(Subjects{User: "bob"}, ResourceApplications, ActionGet, "p/a").Reason, "RBAC policy failed to load")
}

// Validate reports patterns that cannot be evaluated as errors in the user sources and
// as warnings in the built-in policy, matching what SetPolicy loads.
func TestValidateUnevaluablePatterns(t *testing.T) {
	t.Parallel()
	problems, err := Validate([]Source{{Name: "policy.csv", CSV: "p, a, applications, get, .*, allow\np, a, applications, *, .*, allow", Builtin: true}}, ValidateOptions{MatchMode: RegexMatchMode, Builtin: BuiltinPolicy()})
	require.ErrorIs(t, err, ErrInvalidPolicy)
	p, ok := findProblem(problems, "policy.csv", 2)
	require.True(t, ok, "%v", problems)
	assert.Contains(t, p.Message, `action "*" is not a valid regular expression`)
	for _, pr := range problems {
		if pr.Source == "builtin" {
			assert.Equal(t, SeverityWarning, pr.Severity, pr.Error())
		}
	}

	problems, err = Validate([]Source{{Name: "policy.csv", CSV: "p, a, applications, *, .*, allow"}}, ValidateOptions{MatchMode: GlobMatchMode})
	require.NoError(t, err)
	assert.False(t, HasErrors(problems))
}
