package rbac

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gobwas/glob"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// matchDeadline is far above the few milliseconds these evaluations take, even under
// -race on a loaded machine, and far below the hours the exponential cases took before
// the fix. A late result fails the test; the stuck goroutine is abandoned.
const matchDeadline = 15 * time.Second

func withinDeadline(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	start := time.Now()
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
		t.Logf("%s took %v", what, time.Since(start))
	case <-time.After(matchDeadline):
		t.Fatalf("%s did not finish within %v: pattern matching is super-linear", what, matchDeadline)
	}
}

// adversarialAlternation is a glob whose star-bearing alternation groups make
// gobwas/glob's backtracking exponential in groups and value length.
func adversarialAlternation(groups int) string {
	return strings.Repeat("{*a,*aa}", groups) + "b*"
}

// Regression for the review finding: a project editor wrote a quoted action containing
// comma alternations. CSV parsing kept it as one field, validation passed, and every
// request on the project's apps ran an exponential glob match.
func TestProjectPolicyRejectsCommasInFields(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		policy string
		valid  bool
	}{
		{name: "quoted adversarial action", policy: `p, proj:a:r, applications, "update/` + adversarialAlternation(6) + `", a/*, allow`},
		{name: "quoted alternation action", policy: `p, proj:a:r, applications, "update/{apps,batch}/*", a/*, allow`},
		{name: "quoted alternation deny", policy: `p, proj:a:r, applications, "delete/{*a,*b}", a/*, deny`},
		{name: "quoted object with comma", policy: `p, proj:a:r, applications, get, "a/x,y", allow`},
		{name: "quoted subject with comma", policy: `p, "proj:a:r,x", applications, get, a/*, allow`},
		{name: "quoted action without comma", policy: `p, proj:a:r, applications, "update/*", a/*, allow`, valid: true},
		{name: "comma-free glob syntax", policy: `p, proj:a:r, applications, update/{apps}/[DS]*/?s/*, a/*, allow`, valid: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnforcer(t, BuiltinPolicy(), "", RoleReadOnly, "")
			err := e.SetProjectPolicies("a", []Role{{Name: "r", Policies: []string{tt.policy}, Groups: []string{"devs"}}})
			if tt.valid {
				require.NoError(t, err)
				assert.Empty(t, e.ProjectProblems("a"))
				return
			}
			require.ErrorIs(t, err, ErrInvalidPolicy)
			assert.Contains(t, err.Error(), "exactly six comma-separated fields")
			problems := e.ProjectProblems("a")
			require.True(t, HasErrors(problems))
			assert.Equal(t, 1, problems[0].Line)
		})
	}
}

// The exact scenario from the review: the role is bound to a common group and the
// default role is readonly, so any request on the project's apps reaches the rule.
func TestAdversarialProjectPolicyCannotStallEnforce(t *testing.T) {
	t.Parallel()
	e := newEnforcer(t, BuiltinPolicy(), "", RoleReadOnly, "")
	adversarial := `p, proj:a:r, applications, "update/` + adversarialAlternation(8) + `", a/*, allow`
	err := e.SetProjectPolicies("a", []Role{{Name: "r", Policies: []string{adversarial}, Groups: []string{RoleReadOnly, "devs"}}})
	require.ErrorIs(t, err, ErrInvalidPolicy)

	action := SubResourceAction(ActionUpdate, "apps", "Deployment", "ns", strings.Repeat("a", 200))
	var d Decision
	withinDeadline(t, "Enforce against the rejected adversarial project policy", func() {
		d = e.Enforce(user("anyone", "devs"), ResourceApplications, action, "a/app")
	})
	assert.False(t, d.Allowed, d.Reason)
}

// Comma-free glob syntax stays valid in project policies (upstream accepts it). None of
// it can make gobwas/glob backtrack exponentially: these near-worst-case patterns of up
// to 16 KiB against 8 KiB values finish in about a millisecond.
func TestCommaFreeProjectPatternsMatchQuickly(t *testing.T) {
	t.Parallel()
	actions := []string{
		"update/" + strings.Repeat("{*a}", 2000) + "b*",
		"update/" + strings.Repeat("*a", 4000) + "*b*",
		"update/" + strings.Repeat("*[a]", 2000) + "*b*",
		"update/" + strings.Repeat("*[!b]", 2000) + "*b*",
		"update/" + strings.Repeat("*?", 4000) + "*b*",
		"update/" + strings.Repeat("*a?", 2700) + "*b*",
	}
	var policies []string
	for _, act := range actions {
		policies = append(policies, "p, proj:a:r, applications, "+act+", a/*, allow")
	}
	e := newEnforcer(t, BuiltinPolicy(), "", "", "")
	require.NoError(t, e.SetProjectPolicies("a", []Role{{Name: "r", Policies: policies, Groups: []string{"devs"}}}))

	values := []string{
		SubResourceAction(ActionUpdate, "apps", "Deployment", "ns", strings.Repeat("a", 8000)),
		SubResourceAction(ActionUpdate, "apps", "Deployment", "ns", strings.Repeat("a", 8000)+"b"),
	}
	var got []bool
	withinDeadline(t, "Enforce against comma-free adversarial project patterns", func() {
		for _, v := range values {
			got = append(got, e.Enforce(user("u", "devs"), ResourceApplications, v, "a/app").Allowed)
		}
	})
	// "apps/Deployment/ns/" contains no 'b' before the name, so only the second value
	// satisfies the trailing "b*" or "*b*" of the patterns.
	assert.Equal(t, []bool{false, true}, got)
}

// Defense in depth for administrator policies, which may contain alternations: they are
// evaluated through an equivalent RE2 expression, never by the backtracking matcher.
func TestGlobalAlternationPatternsMatchQuickly(t *testing.T) {
	t.Parallel()
	policy := `p, role:x, applications, "update/` + adversarialAlternation(16) + `", */*, allow` + "\n" +
		`p, role:x, applications, "delete/{*a,*aa}{*a,*aa}{*a,*aa}{*a,*aa}c", */*, deny` + "\n" +
		"p, role:x, applications, delete/*, */*, allow\n" +
		"g, devs, role:x"
	e := newEnforcer(t, "", policy, "", "")

	long := strings.Repeat("a", 4000)
	tests := []struct {
		action string
		want   bool
	}{
		{action: "update/" + long, want: false},
		{action: "update/" + long + "b", want: true},
		{action: "update/" + strings.Repeat("a", 15) + "b", want: false}, // 16 groups need at least 16 'a'
		{action: "delete/" + long, want: true},
		{action: "delete/" + long + "c", want: false}, // the deny matches
	}
	got := make([]bool, len(tests))
	withinDeadline(t, "Enforce against global alternation patterns", func() {
		for i, tt := range tests {
			got[i] = e.Enforce(user("u", "devs"), ResourceApplications, tt.action, "p/app").Allowed
		}
	})
	for i, tt := range tests {
		assert.Equal(t, tt.want, got[i], "action of %d bytes ending in %q", len(tt.action), tt.action[len(tt.action)-1:])
	}
}

func TestCompilePatternChoosesSafeMatcher(t *testing.T) {
	t.Parallel()
	deep := strings.Repeat("{a,b", 1500) + strings.Repeat("}", 1500)
	tests := []struct {
		name    string
		pattern string
		kind    matchKind
		matches map[string]bool
	}{
		{name: "alternation", pattern: "{a,b}*", kind: matchGlobRegexp, matches: map[string]bool{"a": true, "bxx": true, "c": false, "": false}},
		{name: "empty branch", pattern: "x{a,}", kind: matchGlobRegexp, matches: map[string]bool{"x": true, "xa": true, "xb": false}},
		{name: "nested", pattern: "{a{b,c},d}", kind: matchGlobRegexp, matches: map[string]bool{"ab": true, "ac": true, "d": true, "a": false}},
		{name: "class and single", pattern: "{[!a-c]?,[xy]}", kind: matchGlobRegexp, matches: map[string]bool{"d\n": true, "x1": true, "a1": false, "c": false, "y": true, "": false}},
		{name: "escaped metacharacters", pattern: `{\*,\{}`, kind: matchGlobRegexp, matches: map[string]bool{"*": true, "{": true, "a": false}},
		{name: "regexp metacharacters are literal", pattern: "{.+,(a)}", kind: matchGlobRegexp, matches: map[string]bool{".+": true, "(a)": true, "aa": false}},
		{name: "group without comma", pattern: "{*a}b", kind: matchGlob, matches: map[string]bool{"xab": true, "xb": false}},
		{name: "comma outside group", pattern: "a,b*", kind: matchGlob, matches: map[string]bool{"a,bc": true}},
		{name: "escaped braces", pattern: `\{a,b\}`, kind: matchGlob, matches: map[string]bool{"{a,b}": true}},
		{name: "unclosed group", pattern: "{a,b", kind: matchNever, matches: map[string]bool{"a": false}},
		{name: "literal U+FFFD in alternation", pattern: "{a,�}", kind: matchFail},
		{name: "nesting beyond RE2 limit", pattern: deep, kind: matchFail},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := compilePattern(tt.pattern, GlobMatchMode)
			require.Equal(t, tt.kind, m.kind, "%v", m.err)
			if tt.kind == matchFail {
				require.ErrorIs(t, m.err, errGlobUntranslatable)
				ok, err := m.match("a")
				assert.False(t, ok)
				assert.ErrorIs(t, err, errPatternEvaluation)
				return
			}
			for value, want := range tt.matches {
				got, err := m.match(value)
				require.NoError(t, err)
				assert.Equal(t, want, got, "value %q", value)
			}
		})
	}
}

func TestParseWarnsAboutUnevaluableGlob(t *testing.T) {
	t.Parallel()
	policy := "p, a, applications, *, */*, allow\np, a, applications, get, \"{a,�}/*\", deny"
	// In a user source the pattern is an error: the policy does not load.
	_, problems, err := Parse(policy, ParseOptions{})
	require.ErrorIs(t, err, ErrInvalidPolicy)
	require.Len(t, problems, 1)
	assert.Equal(t, SeverityError, problems[0].Severity)
	assert.Equal(t, 2, problems[0].Line)
	assert.Contains(t, problems[0].Message, "cannot be evaluated")

	// In the built-in source it is a warning, and evaluation fails closed.
	pol, problems, err := ParseSources([]Source{{Name: "builtin", CSV: policy, Builtin: true}}, ParseOptions{})
	require.NoError(t, err)
	require.Len(t, problems, 1)
	assert.Equal(t, SeverityWarning, problems[0].Severity)
	assert.Equal(t, 2, problems[0].Line)
	assert.Contains(t, problems[0].Message, "cannot be evaluated")
	// Fail closed: the deny cannot be evaluated, so the allow does not apply either.
	d := pol.EnforceSubject("a", ResourceApplications, ActionGet, "b/x")
	assert.False(t, d.Allowed, d.Reason)
	// Requests whose action differs never reach the unevaluable object, so they are
	// unaffected.
	assert.True(t, pol.EnforceSubject("a", ResourceApplications, ActionSync, "b/x").Allowed)
}

// The RE2 translation of alternation patterns agrees with gobwas/glob on every input,
// including newlines, invalid UTF-8 and U+FFFD in the value.
func TestPropertyGlobAlternationMatchesLibrary(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		pieces := rapid.SampledFrom([]string{
			"a", "b", "-", "*", "**", "?", "{", "}", ",", "{", ",", "}", "[", "]", "!", `\`,
			"[a-b]", "[!a]", "[ab]", "\n", ".", "�", "é",
		})
		parts := rapid.SliceOfN(pieces, 1, 10).Draw(t, "pattern")
		pattern := "{" + strings.Join(parts, "") + ",}" // always one alternation
		if rapid.Bool().Draw(t, "unwrap") {
			pattern = strings.Join(parts, "")
		}
		g, err := glob.Compile(pattern)
		if err != nil {
			return
		}
		m := compilePattern(pattern, GlobMatchMode)
		values := rapid.SliceOfN(rapid.SampledFrom([]string{"a", "b", "-", ",", "!", "\n", ".", "�", "\xff", "é", "\xc3"}), 0, 12).Draw(t, "value")
		value := strings.Join(values, "")
		got, err := m.match(value)
		switch m.kind {
		case matchFail:
			if !errors.Is(m.err, errGlobUntranslatable) || !strings.Contains(pattern, "�") {
				t.Fatalf("pattern %q unexpectedly untranslatable: %v", pattern, m.err)
			}
			return
		case matchGlobRegexp:
			if !globHasAlternation(pattern) {
				t.Fatalf("pattern %q has no alternation but uses the RE2 path", pattern)
			}
		case matchGlob, matchAny, matchEqual:
			if globHasAlternation(pattern) {
				t.Fatalf("pattern %q has an alternation but uses the backtracking matcher", pattern)
			}
		}
		if err != nil {
			t.Fatalf("pattern %q on %q: %v", pattern, value, err)
		}
		if want := g.Match(value); got != want {
			t.Fatalf("pattern %q on %q: got %v, gobwas/glob %v (kind %d)", pattern, value, got, want, m.kind)
		}
	})
}
