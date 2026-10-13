package oidc

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/juliosaraiva/patrol-cd/internal/auth"
)

func TestExtractGroups(t *testing.T) {
	t.Parallel()
	many := make([]any, auth.MaxGroups+1)
	for i := range many {
		many[i] = fmt.Sprint(i)
	}
	tests := []struct {
		name    string
		raw     map[string]any
		claim   string
		want    []string
		wantErr error
	}{
		{name: "array", raw: map[string]any{"groups": []any{"a", "b"}}, want: []string{"a", "b"}},
		{name: "string slice", raw: map[string]any{"groups": []string{"x"}}, want: []string{"x"}},
		{name: "single string", raw: map[string]any{"groups": "solo"}, want: []string{"solo"}},
		{name: "mixed types, duplicates, empties", raw: map[string]any{"groups": []any{"a", 1.0, nil, "b", "a", "", map[string]any{}}}, want: []string{"a", "b"}},
		{name: "missing", raw: map[string]any{}, want: nil},
		{name: "null", raw: map[string]any{"groups": nil}, want: nil},
		{name: "object is ignored", raw: map[string]any{"groups": map[string]any{"a": true}}, want: nil},
		{name: "number is ignored", raw: map[string]any{"groups": 3.0}, want: nil},
		{name: "custom claim", raw: map[string]any{"roles": []any{"r"}, "groups": []any{"g"}}, claim: "roles", want: []string{"r"}},
		{name: "nested claim", raw: map[string]any{"realm_access": map[string]any{"roles": []any{"r1", "r2"}}}, claim: "realm_access.roles", want: []string{"r1", "r2"}},
		{name: "nested path missing", raw: map[string]any{"realm_access": "flat"}, claim: "realm_access.roles", want: nil},
		{name: "namespaced top-level claim wins", raw: map[string]any{"https://example.com/groups": []any{"ns"}, "https://example": map[string]any{"com/groups": []any{"nested"}}}, claim: "https://example.com/groups", want: []string{"ns"}},
		{name: "too many", raw: map[string]any{"groups": many}, wantErr: ErrTooManyGroups},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ExtractGroups(tc.raw, tc.claim)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("groups = %q, want %q", got, tc.want)
			}
		})
	}
}

// Property: output is duplicate-free, has no empty names, keeps first-seen order, and
// contains only strings present in the input.
func TestExtractGroupsProperty(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		elems := rapid.SliceOfN(rapid.OneOf(
			rapid.Map(rapid.StringMatching(`[a-c]{0,2}`), func(s string) any { return s }),
			rapid.Map(rapid.Float64(), func(f float64) any { return f }),
			rapid.Just[any](nil),
		), 0, 30).Draw(rt, "elems")
		got, err := ExtractGroups(map[string]any{"groups": elems}, "")
		if err != nil {
			rt.Fatal(err)
		}
		var want []string
		for _, e := range elems {
			if s, ok := e.(string); ok && s != "" && !slices.Contains(want, s) {
				want = append(want, s)
			}
		}
		if (len(got) != 0 || len(want) != 0) && !slices.Equal(got, want) {
			rt.Fatalf("got %q want %q", got, want)
		}
	})
}

func TestClaimsHelpers(t *testing.T) {
	t.Parallel()
	c, err := claimsFromRaw(map[string]any{
		"iss": "https://idp", "sub": "s", "email": "e@x", "name": "N", "exp": 1.7e9,
		"groups":           "g",
		"federated_claims": map[string]any{"user_id": "fed"},
		"roles":            []any{"r"},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if c.UserID() != "fed" || c.Sub != "s" || c.Email != "e@x" || c.Name != "N" || c.Expiry.Unix() != 1.7e9 || !slices.Equal(c.Groups, []string{"g"}) {
		t.Fatalf("claims = %+v", c)
	}
	c.Raw["federated_claims"] = map[string]any{"user_id": ""}
	if c.UserID() != "s" {
		t.Fatal("empty federated user_id should fall back to sub")
	}
	id := c.SSOIdentity("roles", "sub", "kind", "absent", "")
	if id.Subject != "s" || len(id.Extra) != 1 || id.Extra["roles"] == nil {
		t.Fatalf("identity = %+v", id)
	}
	if _, err := claimsFromRaw(map[string]any{"groups": strings.Split(strings.Repeat("x,", auth.MaxGroups+1), ",")}, ""); !errors.Is(err, ErrTooManyGroups) {
		t.Fatalf("too many groups: %v", err)
	}
}
