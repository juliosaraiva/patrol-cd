package rbac

import (
	"fmt"
	"strings"
	"testing"
)

// benchPolicy builds a policy of exactly n lines: per team a role with four 'p' lines and
// one 'g' line binding an SSO group to it.
func benchPolicy(n int, mode string) string {
	var b strings.Builder
	any := "*"
	if mode == RegexMatchMode {
		any = ".*"
	}
	lines := 0
	for team := 0; lines < n; team++ {
		role := fmt.Sprintf("role:team-%d", team)
		for _, l := range []string{
			fmt.Sprintf("p, %s, applications, get, team-%d/%s, allow", role, team, any),
			fmt.Sprintf("p, %s, applications, sync, team-%d/%s, allow", role, team, any),
			fmt.Sprintf("p, %s, applications, delete, team-%d/prod-%s, deny", role, team, any),
			fmt.Sprintf("p, %s, logs, get, team-%d/%s, allow", role, team, any),
			fmt.Sprintf("g, org:team-%d, %s", team, role),
		} {
			if lines == n {
				break
			}
			b.WriteString(l)
			b.WriteByte('\n')
			lines++
		}
	}
	return b.String()
}

func BenchmarkParse10kLines(b *testing.B) {
	policy := benchPolicy(10_000, GlobMatchMode)
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := Parse(policy, ParseOptions{}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEnforce10kLines(b *testing.B) {
	for _, mode := range []string{GlobMatchMode, RegexMatchMode} {
		e := NewEnforcer(WithLogger(discardLogger()))
		if err := e.SetPolicy(BuiltinPolicy(), benchPolicy(10_000, mode), "", mode); err != nil {
			b.Fatal(err)
		}
		if err := e.SetProjectPolicies("team-7", []Role{{Name: "ci", Policies: []string{"p, proj:team-7:ci, applications, sync, team-7/*, allow"}, Groups: []string{"org:ci"}}}); err != nil {
			b.Fatal(err)
		}
		cases := []struct {
			name    string
			s       Subjects
			act     string
			obj     string
			allowed bool
		}{
			{"group-allow", Subjects{User: "u", Groups: []string{"org:other", "org:team-1500"}}, ActionSync, "team-1500/web", true},
			{"group-deny", Subjects{User: "u", Groups: []string{"org:team-1500"}}, ActionDelete, "team-1500/prod-web", false},
			{"no-match", Subjects{User: "u", Groups: []string{"org:team-1"}}, ActionGet, "team-2/web", false},
			// In regex mode the glob-style built-in patterns ("*/*") are invalid regular
			// expressions, so built-in roles deny, as upstream.
			{"builtin-admin", Subjects{User: "admin"}, ActionDelete, "team-9/web", mode == GlobMatchMode},
			{"project-token", Subjects{ProjectRole: &ProjectRole{Project: "team-7", Role: "ci"}}, ActionSync, "team-7/web", true},
		}
		for _, c := range cases {
			b.Run(mode+"/"+c.name, func(b *testing.B) {
				if got := e.Enforce(c.s, ResourceApplications, c.act, c.obj).Allowed; got != c.allowed {
					b.Fatalf("allowed=%v, want %v", got, c.allowed)
				}
				b.ReportAllocs()
				for b.Loop() {
					e.Enforce(c.s, ResourceApplications, c.act, c.obj)
				}
			})
		}
	}
}

func BenchmarkEnforceParallel(b *testing.B) {
	e := NewEnforcer(WithLogger(discardLogger()))
	if err := e.SetPolicy(BuiltinPolicy(), benchPolicy(10_000, GlobMatchMode), RoleReadOnly, ""); err != nil {
		b.Fatal(err)
	}
	s := Subjects{User: "u", Groups: []string{"org:team-42"}}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			e.Enforce(s, ResourceApplications, ActionSync, "team-42/web")
		}
	})
}
