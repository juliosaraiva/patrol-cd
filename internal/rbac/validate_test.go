package rbac

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func danglingRoles(problems []ValidationError) []string {
	var out []string
	for _, p := range problems {
		if p.Severity == SeverityWarning && strings.Contains(p.Message, "is assigned but no policy grants it") {
			out = append(out, p.Text)
		}
	}
	return out
}

func TestValidateReferentialIntegrity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		policy   string
		builtin  string
		dangling []string
	}{
		{
			name:   "every role has permissions",
			policy: "p, role:depA, *, get, foo/obj, allow\np, role:depB, *, get, foo/obj, deny\ng, depA, role:depA\ng, depB, role:depB",
		},
		{
			name:   "role inherits permissions transitively",
			policy: "p, role:depA, *, get, foo/obj, allow\ng, depC, role:depC\ng, role:depC, role:depA",
		},
		{
			name:     "role without permissions",
			policy:   "p, role:depA, *, get, foo/obj, allow\ng, depC, role:depC",
			dangling: []string{"g, depC, role:depC"},
		},
		{
			name:     "built-in role referenced without the built-in policy",
			policy:   "g, alice, role:admin",
			dangling: []string{"g, alice, role:admin"},
		},
		{
			name:    "built-in role referenced with the built-in policy",
			policy:  "g, alice, role:admin",
			builtin: BuiltinPolicy(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			problems, err := Validate([]Source{{Name: "policy.csv", CSV: tt.policy}}, ValidateOptions{Builtin: tt.builtin})
			require.NoError(t, err)
			assert.Equal(t, tt.dangling, danglingRoles(problems))
			for _, p := range problems {
				if p.Line > 0 {
					assert.Equal(t, "policy.csv", p.Source)
				}
			}
		})
	}
}

func TestValidateSyntaxErrors(t *testing.T) {
	t.Parallel()
	problems, err := Validate([]Source{{Name: "policy.csv", CSV: "p, a, applications, get, */*, allow\nthis\ttoo"}}, ValidateOptions{MatchMode: RegexMatchMode})
	require.ErrorIs(t, err, ErrInvalidPolicy)
	require.True(t, HasErrors(problems))
	assert.Equal(t, 2, problems[len(problems)-1].Line)

	problems, err = Validate(nil, ValidateOptions{})
	require.NoError(t, err)
	assert.Empty(t, problems)
}
