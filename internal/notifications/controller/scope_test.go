package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// NOTIF-002: the control-plane namespace is always in scope; other namespaces must match an
// application.namespaces glob or /regexp/ (upstream glob.MatchStringInList, REGEXP mode).
func TestNamespaceMatcher(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		patterns []string
		in       []string
		out      []string
	}{
		{name: "no patterns", in: []string{"patrol"}, out: []string{"team-a", ""}},
		{name: "literal list", patterns: []string{"namespace1", "namespace2"}, in: []string{"patrol", "namespace1", "namespace2"}, out: []string{"namespace3"}},
		{name: "glob", patterns: []string{"team-*"}, in: []string{"team-a", "team-b-prod"}, out: []string{"teams", "other"}},
		{name: "regexp", patterns: []string{"/^team-a$/"}, in: []string{"team-a"}, out: []string{"team-ab", "xteam-a"}},
		{name: "unanchored regexp", patterns: []string{"/team/"}, in: []string{"my-team-x"}, out: []string{"other"}},
		{name: "blank entries ignored", patterns: []string{" ", ""}, in: []string{"patrol"}, out: []string{"x"}},
		{name: "wildcard", patterns: []string{"*"}, in: []string{"anything"}},
		// Upstream (regexp2) matches every namespace but not-allowed with this documented
		// example; RE2 has no lookahead, so here it is ignored (warned) and never matches,
		// while the other entries keep working.
		{name: "lookahead entry never matches", patterns: []string{"/^((?!not-allowed).)*$/", "team-*"}, in: []string{"patrol", "team-a"}, out: []string{"allowed", "not-allowed", "other"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m, _ := newNamespaceMatcher("patrol", tc.patterns)
			for _, ns := range tc.in {
				assert.True(t, m.matches(ns), "%q should be in scope", ns)
			}
			for _, ns := range tc.out {
				assert.False(t, m.matches(ns), "%q should be out of scope", ns)
			}
		})
	}
}

// NOTIF-002: an entry that cannot be compiled is reported and never matches; it does not fail
// the start (as internal/webhook and internal/project, and upstream for invalid entries).
func TestNamespaceMatcherInvalidPatterns(t *testing.T) {
	t.Parallel()
	tests := []struct {
		pattern string
		warning string
	}{
		{pattern: "/(unclosed/", warning: "is not a valid RE2 regular expression and never matches"},
		{pattern: "/^((?!not-allowed).)*$/", warning: "lookaround is not supported"},
		{pattern: "team-[a", warning: "is not a valid glob and never matches"},
	}
	for _, tc := range tests {
		t.Run(tc.pattern, func(t *testing.T) {
			t.Parallel()
			m, warnings := newNamespaceMatcher("patrol", []string{tc.pattern})
			require.Len(t, warnings, 1)
			assert.Contains(t, warnings[0], tc.pattern)
			assert.Contains(t, warnings[0], tc.warning)
			assert.False(t, m.multiNamespace(), "an entry that never matches does not widen the watch")
			assert.True(t, m.matches("patrol"))
			for _, ns := range []string{"team-a", "allowed", "not-allowed", "x"} {
				assert.False(t, m.matches(ns), "%q", ns)
			}
			lits, ok := m.literalNamespaces()
			assert.True(t, ok)
			assert.Equal(t, []string{"patrol"}, lits)
		})
	}
	_, warnings := newNamespaceMatcher("patrol", []string{"team-*", "/^a$/", "b"})
	assert.Empty(t, warnings)
}

func TestLiteralNamespaces(t *testing.T) {
	t.Parallel()
	tests := []struct {
		patterns []string
		want     []string
		ok       bool
	}{
		{patterns: nil, want: []string{"patrol"}, ok: true},
		{patterns: []string{"b", "a", "patrol"}, want: []string{"a", "b", "patrol"}, ok: true},
		{patterns: []string{"team-*"}},
		{patterns: []string{"/^a$/"}},
		{patterns: []string{"a", "Not_A_Namespace"}},
		{patterns: []string{"a?"}},
	}
	for _, tc := range tests {
		m, warnings := newNamespaceMatcher("patrol", tc.patterns)
		require.Empty(t, warnings)
		got, ok := m.literalNamespaces()
		assert.Equal(t, tc.ok, ok, "%v", tc.patterns)
		assert.Equal(t, tc.want, got, "%v", tc.patterns)
		assert.Equal(t, len(tc.patterns) > 0, m.multiNamespace())
	}
}
