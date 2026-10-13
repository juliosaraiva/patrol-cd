package rbac

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPolicyCSV(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		data map[string]string
		want string
	}{
		{name: "no policy keys", data: map[string]string{}, want: ""},
		{name: "main policy only", data: map[string]string{ConfigMapPolicyCSVKey: "policy1\npolicy2", "UnrelatedKey": "x"}, want: "policy1\npolicy2"},
		{
			name: "overlays in key order",
			data: map[string]string{
				"UnrelatedKey":            "unrelated value",
				"policy.B.csv":            "policyb",
				"policy.A.csv":            "policya",
				"policy.C.csv":            "policyc",
				ConfigMapPolicyCSVKey:     "policy1",
				ConfigMapPolicyDefaultKey: "role:readonly",
				ConfigMapMatchModeKey:     "glob",
			},
			want: "policy1\npolicya\npolicyb\npolicyc",
		},
		{name: "overlay without main policy", data: map[string]string{"policy.x.csv": "px"}, want: "\npx"},
		{name: "non-overlay lookalikes", data: map[string]string{"policy.csv.bak": "no", "mypolicy.x.csv": "no", "policy.x.csv": "yes"}, want: "\nyes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, PolicyCSV(tt.data))
		})
	}
}

func TestPolicySources(t *testing.T) {
	t.Parallel()
	got := PolicySources(map[string]string{
		"policy.b.csv":        "b",
		ConfigMapPolicyCSVKey: "main",
		"policy.a.csv":        "a",
		ConfigMapScopesKey:    "[groups]",
	})
	assert.Equal(t, []Source{{ConfigMapPolicyCSVKey, "main"}, {"policy.a.csv", "a"}, {"policy.b.csv", "b"}}, got)
	assert.Empty(t, PolicySources(map[string]string{"other": "x"}))
}

func TestParseScopes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in      string
		want    []string
		wantErr bool
	}{
		{in: "", want: []string{"groups"}},
		{in: "  ", want: []string{"groups"}},
		{in: "[groups]", want: []string{"groups"}},
		{in: "[groups, email]", want: []string{"groups", "email"}},
		{in: "- groups\n- cognito:groups\n", want: []string{"groups", "cognito:groups"}},
		{in: "groups", want: []string{"groups"}},
		{in: "[]", want: []string{}},
		{in: "null", want: []string{"groups"}},
		{in: "[groups", wantErr: true},
		{in: "{a: b}", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			got, err := ParseScopes(tt.in)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
	// DefaultScopes returns a copy.
	d := DefaultScopes()
	d[0] = "mutated"
	assert.Equal(t, []string{"groups"}, DefaultScopes())
}

func TestScopeValuesAndUserIdentifier(t *testing.T) {
	t.Parallel()
	claims := map[string]any{
		"sub":    "sub-1",
		"email":  "a@example.org",
		"groups": []any{"g1", 42, "g2"},
		"roles":  []string{"r1"},
		"num":    7,
	}
	assert.Equal(t, []string{"g1", "g2", "a@example.org", "r1"}, ScopeValues(claims, []string{"groups", "email", "roles", "num", "missing"}))
	assert.Empty(t, ScopeValues(claims, nil))

	assert.Equal(t, "sub-1", UserIdentifier(claims))
	assert.Equal(t, "dex-user", UserIdentifier(map[string]any{"sub": "x", "federated_claims": map[string]any{"user_id": "dex-user"}}))
	assert.Equal(t, "x", UserIdentifier(map[string]any{"sub": "x", "federated_claims": map[string]any{"user_id": ""}}))
	assert.Equal(t, "x", UserIdentifier(map[string]any{"sub": "x", "federated_claims": "garbage"}))
	assert.Empty(t, UserIdentifier(nil))

	s := SubjectsFromClaims(map[string]any{"sub": "proj:a:r", "groups": "solo"}, DefaultScopes())
	assert.Equal(t, "proj:a:r", s.User)
	assert.Nil(t, s.ProjectRole)
	assert.Equal(t, []string{"solo"}, s.Groups)
}

func TestPolicyCSVMatchesSources(t *testing.T) {
	t.Parallel()
	data := map[string]string{
		ConfigMapPolicyCSVKey: "p, a, applications, get, */*, allow",
		"policy.z.csv":        "p, a, applications, get, x/secret, deny",
		"policy.m.csv":        "g, team, role:t\np, role:t, clusters, get, *, allow",
	}
	one, _, err := Parse(PolicyCSV(data), ParseOptions{})
	require.NoError(t, err)
	many, _, err := ParseSources(PolicySources(data), ParseOptions{})
	require.NoError(t, err)
	for _, sub := range []string{"a", "team", "role:t"} {
		for _, obj := range []string{"x/secret", "x/web", "c"} {
			for _, res := range []string{"applications", "clusters"} {
				a := one.EnforceSubject(sub, res, "get", obj)
				b := many.EnforceSubject(sub, res, "get", obj)
				assert.Equal(t, a.Allowed, b.Allowed, strings.Join([]string{sub, res, obj}, " "))
			}
		}
	}
}
