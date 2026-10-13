package rbac

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTokenizeLine(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		line    string
		kind    lineKind
		tokens  []string
		wantErr string
	}{
		{name: "policy line", line: "p, role:Myrole, applications, *, myproj/*, allow", kind: linePolicy, tokens: []string{"p", "role:Myrole", "applications", "*", "myproj/*", "allow"}},
		{name: "grant line", line: "g, your-github-org:your-team, role:org-admin", kind: lineGroup, tokens: []string{"g", "your-github-org:your-team", "role:org-admin"}},
		{name: "empty line", line: "", kind: lineSkip},
		{name: "whitespace line", line: " \t ", kind: lineSkip},
		{name: "comment", line: "# Some comment", kind: lineSkip},
		{name: "bare hash", line: "#", kind: lineSkip},
		{name: "indented comment", line: "   # p, a, b, c, d, allow", kind: lineSkip},
		{name: "quoted field with comma", line: `p, "role,admin", projects, delete, *, allow`, kind: linePolicy, tokens: []string{"p", "role,admin", "projects", "delete", "*", "allow"}},
		{name: "surrounding whitespace", line: " p, role:admin, projects, delete, *, allow \r", kind: linePolicy, tokens: []string{"p", "role:admin", "projects", "delete", "*", "allow"}},
		{name: "trailing field space kept", line: "p, alice , projects, get, *, allow", kind: linePolicy, tokens: []string{"p", "alice ", "projects", "get", "*", "allow"}},
		{name: "request definition is a no-op", line: "r, a, b", kind: lineNoop},
		{name: "effect definition is a no-op", line: "e, x", kind: lineNoop},
		{name: "matcher definition is a no-op", line: "m", kind: lineNoop},
		{name: "single p token", line: "p", wantErr: "need 6 fields"},
		{name: "plain text", line: "Some comment", wantErr: "unknown line type"},
		{name: "unknown type", line: "agh, foo, bar", wantErr: "unknown line type"},
		{name: "tab separated", line: "this\ttoo", wantErr: "unknown line type"},
		{name: "six bad fields", line: "this, is, not, a, good, policy", wantErr: "unknown line type"},
		{name: "missing comma", line: "p, role:Myrole, applications, *, myproj/* allow", wantErr: "need 6 fields"},
		{name: "missing policy type", line: ", role:Myrole, applications, *, myproj/*, allow", wantErr: "missing line type"},
		{name: "g with 4 fields", line: "g, a, b, c", wantErr: "need 3 fields"},
		{name: "p with 7 fields", line: "p, a, b, c, d, allow, extra", wantErr: "need 6 fields"},
		{name: "type with trailing space", line: "p , a, b, c, d, allow", wantErr: "unknown line type"},
		{name: "bare quote", line: `p, a"b, c, d, e, allow`, wantErr: "bare"},
		{name: "unterminated quote", line: `p, "abc, c, d, e, allow`, wantErr: "cannot parse CSV"},
		{name: "two-letter type", line: "pp, a, b, c, d, allow", wantErr: "unknown line type"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pl, err := tokenizeLine(tt.line, DefaultMaxLineBytes)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.kind, pl.kind)
			if tt.tokens != nil {
				assert.Equal(t, tt.tokens, pl.tokens)
			}
		})
	}
}

func TestParseReportsInvalidLinesWithLineNumbers(t *testing.T) {
	t.Parallel()
	policy := strings.Join([]string{
		"# header", // 1
		"p, alice, applications, get, */*, allow", // 2
		"p, bob, applications, get, */*",          // 3 wrong arity
		"",                                        // 4
		"x, y, z",                                 // 5 unknown type
		"g, alice, role:admin",                    // 6
		`p, "carol, applications, get, */*, allow`, // 7 unterminated quote
	}, "\n")
	pol, problems, err := Parse(policy, ParseOptions{Source: "policy.csv"})
	require.Error(t, err)
	assert.Nil(t, pol)
	assert.ErrorIs(t, err, ErrInvalidPolicy)
	var pe *PolicyError
	require.ErrorAs(t, err, &pe)

	var lines []int
	for _, p := range pe.Problems {
		assert.Equal(t, SeverityError, p.Severity)
		assert.Equal(t, "policy.csv", p.Source)
		lines = append(lines, p.Line)
	}
	assert.Equal(t, []int{3, 5, 7}, lines)
	assert.Equal(t, "p, bob, applications, get, */*", pe.Problems[0].Text)
	assert.Contains(t, pe.Problems[0].Error(), "policy.csv:3: error:")
	assert.Contains(t, err.Error(), "and 2 more errors")
	assert.True(t, HasErrors(problems))
}

func TestParseSourcesKeepsPerSourceLineNumbers(t *testing.T) {
	t.Parallel()
	_, _, err := ParseSources([]Source{
		{Name: "policy.csv", CSV: "p, a, applications, get, */*, allow\ng, a, role:x"},
		{Name: "policy.overlay.csv", CSV: "p, b, applications, get, */*, allow\nbroken line"},
	}, ParseOptions{})
	var pe *PolicyError
	require.ErrorAs(t, err, &pe)
	require.Len(t, pe.Problems, 1)
	assert.Equal(t, "policy.overlay.csv", pe.Problems[0].Source)
	assert.Equal(t, 2, pe.Problems[0].Line)
}

func TestParseAcceptsUpstreamValidPolicies(t *testing.T) {
	t.Parallel()
	for _, good := range []string{
		"p, role:admin, projects, delete, *, allow",
		"",
		"#",
		`p, "role,admin", projects, delete, *, allow`,
		` p, role:admin, projects, delete, *, allow `,
		"r, sub, res, act, obj",
		BuiltinPolicy(),
	} {
		pol, _, err := Parse(good, ParseOptions{})
		require.NoError(t, err, good)
		require.NotNil(t, pol)
	}
}

func TestParseWarnings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		line    string
		mode    string
		message string
	}{
		{name: "unknown effect", line: "p, a, applications, get, */*, Allow", message: "never takes effect"},
		{name: "trailing whitespace", line: `p, "alice ", applications, get, */*, allow`, message: "trailing whitespace"},
		{name: "unknown resource", line: "p, a, application, get, */*, allow", message: "not a known RBAC resource"},
		{name: "unknown action", line: "p, a, applications, read, */*, allow", message: "not a known RBAC action"},
		{name: "invalid glob", line: "p, a, applications, get, {a, allow", message: "not a valid glob pattern"},
		{name: "invalid regex", line: "p, a, applications, get, *, allow", mode: RegexMatchMode, message: "not a valid regular expression"},
		{name: "empty subject", line: "p, , applications, get, */*, allow", message: "empty subject"},
		{name: "empty role", line: "g, alice, ", message: "empty member or role"},
		{name: "noop line", line: "e, some(where (p.eft == allow))", message: "no effect"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pol, problems, err := Parse(tt.line, ParseOptions{MatchMode: tt.mode})
			require.NoError(t, err)
			require.NotNil(t, pol)
			require.NotEmpty(t, problems)
			found := false
			for _, p := range problems {
				assert.Equal(t, SeverityWarning, p.Severity)
				assert.Equal(t, 1, p.Line)
				if strings.Contains(p.Message, tt.message) {
					found = true
				}
			}
			assert.True(t, found, "want a warning containing %q, got %v", tt.message, problems)
		})
	}
}

func TestParseUnknownMatchModeFallsBackToGlob(t *testing.T) {
	t.Parallel()
	pol, problems, err := Parse("p, a, applications, get, */*, allow", ParseOptions{MatchMode: "fuzzy"})
	require.NoError(t, err)
	assert.Equal(t, GlobMatchMode, pol.MatchMode())
	require.Len(t, problems, 1)
	assert.Equal(t, 0, problems[0].Line)
	assert.Contains(t, problems[0].Message, `unknown match mode "fuzzy"`)
}

func TestParseLimits(t *testing.T) {
	t.Parallel()
	long := "p, a, applications, get, " + strings.Repeat("x", 100) + ", allow"
	_, _, err := Parse(long, ParseOptions{MaxLineBytes: 50})
	require.ErrorIs(t, err, ErrInvalidPolicy)
	assert.Contains(t, err.Error(), "exceeding the limit")

	_, problems, err := Parse(long+"\n"+long, ParseOptions{MaxBytes: 64})
	require.ErrorIs(t, err, ErrInvalidPolicy)
	require.Len(t, problems, 1)
	assert.Equal(t, 0, problems[0].Line)
}

func TestPolicyAccessors(t *testing.T) {
	t.Parallel()
	pol, _, err := Parse("p, role:a, applications, get, */*, allow\ng, alice, role:a\ng, bob, role:a\ng, role:a, role:b", ParseOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, pol.NumRules())
	assert.Equal(t, 3, pol.NumLinks())
	assert.Equal(t, []string{"role:a", "role:b"}, pol.Roles())

	d := pol.EnforceSubject("alice", ResourceApplications, ActionGet, "p/a")
	assert.True(t, d.Allowed)
	assert.Equal(t, "policy:1: p, role:a, applications, get, */*, allow", d.MatchedPolicy)
}

func TestValidationErrorFormatting(t *testing.T) {
	t.Parallel()
	v := ValidationError{Source: "policy.csv", Line: 4, Text: "x", Severity: SeverityError, Message: "boom"}
	assert.Equal(t, `policy.csv:4: error: boom (line "x")`, v.Error())
	v = ValidationError{Severity: SeverityWarning, Message: "global"}
	assert.Equal(t, "warning: global", v.Error())
	assert.True(t, errors.Is(&PolicyError{}, ErrInvalidPolicy))
}
