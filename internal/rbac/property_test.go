package rbac

import (
	"encoding/csv"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/gobwas/glob"
	"pgregory.net/rapid"
)

// Small alphabets keep collisions (and therefore interesting interactions) frequent.
var (
	propUsers     = []string{"alice", "bob", "carol"}
	propGroups    = []string{"devs", "ops"}
	propRoles     = []string{"role:a", "role:b", "role:c"}
	propResources = []string{ResourceApplications, ResourceClusters, ResourceLogs, ResourceProjects}
	propActions   = []string{ActionGet, ActionSync, ActionDelete, "update/apps/Deployment/ns/web"}
	propObjects   = []string{"p1/a", "p1/b", "p2/a", "p2/b", "p1", "p2", "*"}
	globPatterns  = []string{"*", "p1/*", "*/a", "p1/a", "p2/?", `"{p1,p2}/b"`, "p[12]/a", "p1", "{bad"}
	regexPatterns = []string{".*", "^p1/", "a$", "p1/a", "p2/.", "^p[12]/b$", "p1", "*"}
	resPatterns   = []string{ResourceApplications, ResourceClusters, ResourceLogs, ResourceProjects, "*"}
	actPatterns   = []string{ActionGet, ActionSync, ActionDelete, "update/*", "*"}
	regexActs     = []string{ActionGet, ActionSync, ActionDelete, "^update/", ".*"}
)

func genSubjectName() *rapid.Generator[string] {
	all := append(append(append([]string{}, propUsers...), propGroups...), propRoles...)
	return rapid.SampledFrom(all)
}

func genPolicyLine(mode string, eft *rapid.Generator[string]) *rapid.Generator[string] {
	objs, acts := globPatterns, actPatterns
	if mode == RegexMatchMode {
		objs, acts = regexPatterns, regexActs
	}
	return rapid.Custom(func(t *rapid.T) string {
		return fmt.Sprintf("p, %s, %s, %s, %s, %s",
			genSubjectName().Draw(t, "sub"),
			rapid.SampledFrom(resPatterns).Draw(t, "res"),
			rapid.SampledFrom(acts).Draw(t, "act"),
			rapid.SampledFrom(objs).Draw(t, "obj"),
			eft.Draw(t, "eft"))
	})
}

func genGroupLine() *rapid.Generator[string] {
	return rapid.Custom(func(t *rapid.T) string {
		return fmt.Sprintf("g, %s, %s", genSubjectName().Draw(t, "member"), rapid.SampledFrom(propRoles).Draw(t, "role"))
	})
}

func genPolicy(mode string) *rapid.Generator[[]string] {
	line := rapid.OneOf(
		genPolicyLine(mode, rapid.SampledFrom([]string{EffectAllow, EffectAllow, EffectDeny})),
		genGroupLine(),
	)
	return rapid.SliceOfN(line, 0, 14)
}

// genProjectRoles draws roles for project p1 that pass project validation.
func genProjectRoles(mode string) *rapid.Generator[[]Role] {
	objs := []string{"p1/*", "p1/a", "p1/b"}
	if mode == RegexMatchMode {
		objs = []string{"p1/.*", "p1/a", "p1/b"}
	}
	return rapid.Custom(func(t *rapid.T) []Role {
		var policies []string
		for i := range rapid.IntRange(0, 4).Draw(t, "n") {
			policies = append(policies, fmt.Sprintf("p, proj:p1:r, %s, %s, %s, %s",
				rapid.SampledFrom([]string{ResourceApplications, ResourceLogs}).Draw(t, fmt.Sprintf("res%d", i)),
				rapid.SampledFrom([]string{ActionGet, ActionSync, ActionDelete, "*"}).Draw(t, fmt.Sprintf("act%d", i)),
				rapid.SampledFrom(objs).Draw(t, fmt.Sprintf("obj%d", i)),
				rapid.SampledFrom([]string{EffectAllow, EffectDeny}).Draw(t, fmt.Sprintf("eft%d", i))))
		}
		groups := rapid.SliceOfNDistinct(rapid.SampledFrom(append(append([]string{}, propGroups...), propRoles...)), 0, 2, rapid.ID[string]).Draw(t, "groups")
		return []Role{{Name: "r", Policies: policies, Groups: groups}}
	})
}

// propRequests enumerates the request universe.
func propRequests() []struct {
	s                        Subjects
	resource, action, object string
} {
	subjects := []Subjects{{}, {ProjectRole: &ProjectRole{Project: "p1", Role: "r"}}, {ProjectRole: &ProjectRole{Project: "p2", Role: "r"}}}
	for _, u := range propUsers {
		subjects = append(subjects, Subjects{User: u}, Subjects{User: u, Groups: []string{"devs"}}, Subjects{User: u, Groups: []string{"ops", "devs"}})
	}
	subjects = append(subjects, Subjects{Groups: []string{"role:a"}})
	var out []struct {
		s                        Subjects
		resource, action, object string
	}
	for _, s := range subjects {
		for _, r := range propResources {
			for _, a := range propActions {
				for _, o := range propObjects {
					out = append(out, struct {
						s                        Subjects
						resource, action, object string
					}{s, r, a, o})
				}
			}
		}
	}
	return out
}

type enforcerSetup struct {
	mode, defaultRole string
	lines             []string
	roles             []Role
}

func (s enforcerSetup) build(t *rapid.T) *Enforcer {
	e := NewEnforcer(WithLogger(discardLogger()))
	_ = e.SetPolicy("", strings.Join(s.lines, "\n"), s.defaultRole, s.mode)
	if s.roles != nil {
		_ = e.SetProjectPolicies("p1", s.roles)
		_ = e.SetProjectPolicies("p2", nil)
	}
	return e
}

func allowedSet(e *Enforcer) []bool {
	reqs := propRequests()
	out := make([]bool, len(reqs))
	for i, r := range reqs {
		out[i] = e.Enforce(r.s, r.resource, r.action, r.object).Allowed
	}
	return out
}

func drawSetup(t *rapid.T) enforcerSetup {
	mode := rapid.SampledFrom([]string{GlobMatchMode, RegexMatchMode}).Draw(t, "mode")
	return enforcerSetup{
		mode:        mode,
		defaultRole: rapid.SampledFrom([]string{"", "role:a", "role:none"}).Draw(t, "defaultRole"),
		lines:       genPolicy(mode).Draw(t, "policy"),
		roles:       genProjectRoles(mode).Draw(t, "roles"),
	}
}

// Adding a deny line, anywhere, never grants a request that was denied before. This
// holds for the global policy (including syntactically broken lines, which fail the
// whole policy closed) and for project role policies (including invalid lines, which
// reduce the role to its denies).
func TestPropertyAddingDenyNeverWidensAccess(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		base := drawSetup(t)
		before := allowedSet(base.build(t))

		widened := base
		widened.lines = append([]string(nil), base.lines...)
		widened.roles = cloneRoles(base.roles)
		switch rapid.IntRange(0, 2).Draw(t, "where") {
		case 0: // a well-formed deny in the global policy
			deny := genPolicyLine(base.mode, rapid.Just(EffectDeny)).Draw(t, "deny")
			pos := rapid.IntRange(0, len(widened.lines)).Draw(t, "pos")
			widened.lines = append(widened.lines[:pos], append([]string{deny}, widened.lines[pos:]...)...)
		case 1: // a broken line in the global policy
			widened.lines = append(widened.lines, rapid.SampledFrom([]string{"p, x, deny", `p, "x, a, b, c, deny`, "deny"}).Draw(t, "broken"))
		default: // a deny, valid or not, in the project role
			deny := rapid.SampledFrom([]string{
				"p, proj:p1:r, applications, *, p1/a, deny",
				"p, proj:p1:r, logs, get, p1/b, deny",
				"p, proj:p1:r, applications, *, p2/a, deny", // out of scope: invalid
				"p, proj:p1:r, applications, get, p1/a",     // malformed: invalid
			}).Draw(t, "projectDeny")
			widened.roles[0].Policies = append(widened.roles[0].Policies, deny)
		}
		after := allowedSet(widened.build(t))

		reqs := propRequests()
		for i := range reqs {
			if after[i] && !before[i] {
				t.Fatalf("deny widened access for %+v %s %s %s", reqs[i].s, reqs[i].resource, reqs[i].action, reqs[i].object)
			}
		}
	})
}

// The order of policy lines does not change any decision (upstream's effect is
// "some allow and no deny", which is order-independent).
func TestPropertyLineOrderIsIrrelevant(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		base := drawSetup(t)
		shuffled := base
		shuffled.lines = rapid.Permutation(base.lines).Draw(t, "perm")
		a, b := allowedSet(base.build(t)), allowedSet(shuffled.build(t))
		for i := range a {
			if a[i] != b[i] {
				t.Fatalf("decision %d differs after reordering", i)
			}
		}
	})
}

// naiveAllowed is an independent oracle for one raw subject: every rule is checked in
// order with the upstream matcher shape g(sub, p.sub) && match(res) && match(act) &&
// match(obj), using the glob and regexp libraries directly.
func naiveAllowed(lines []string, mode, subject, resource, action, object string) bool {
	type pRule struct{ sub, res, act, obj, eft string }
	var rules []pRule
	links := map[string][]string{}
	for _, l := range lines {
		pl, err := tokenizeLine(l, DefaultMaxLineBytes)
		if err != nil {
			return false // the policy does not load
		}
		switch pl.kind {
		case linePolicy:
			rules = append(rules, pRule{pl.tokens[1], pl.tokens[2], pl.tokens[3], pl.tokens[4], pl.tokens[5]})
		case lineGroup:
			links[pl.tokens[1]] = append(links[pl.tokens[1]], pl.tokens[2])
		}
	}
	hasLink := func(a, b string) bool {
		level := map[string]bool{a: true}
		for range maxHierarchyLevel + 1 {
			if level[b] {
				return true
			}
			next := map[string]bool{}
			for n := range level {
				for _, r := range links[n] {
					next[r] = true
				}
			}
			level = next
		}
		return false
	}
	match := func(pattern, value string) (bool, error) {
		if mode == RegexMatchMode {
			return regexp.MatchString(pattern, value)
		}
		g, err := glob.Compile(pattern)
		if err != nil {
			return false, nil
		}
		return g.Match(value), nil
	}
	allow := false
	for _, r := range rules {
		if !hasLink(subject, r.sub) {
			continue
		}
		ok := true
		for _, pv := range [][2]string{{r.res, resource}, {r.act, action}, {r.obj, object}} {
			m, err := match(pv[0], pv[1])
			if err != nil {
				return false
			}
			if !m {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		switch r.eft {
		case EffectDeny:
			return false
		case EffectAllow:
			allow = true
		}
	}
	return allow
}

// The indexed, compiled evaluator agrees with the naive oracle for raw subjects.
func TestPropertyEvaluatorMatchesNaiveOracle(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		mode := rapid.SampledFrom([]string{GlobMatchMode, RegexMatchMode}).Draw(t, "mode")
		lines := genPolicy(mode).Draw(t, "policy")
		pol, _, err := Parse(strings.Join(lines, "\n"), ParseOptions{MatchMode: mode})
		if err != nil {
			t.Fatalf("generated policy failed to parse: %v", err)
		}
		subjects := append(append(append([]string{}, propUsers...), propGroups...), propRoles...)
		for _, sub := range subjects {
			for _, res := range propResources {
				for _, act := range propActions {
					for _, obj := range propObjects {
						got := pol.EnforceSubject(sub, res, act, obj).Allowed
						want := naiveAllowed(lines, mode, sub, res, act, obj)
						if got != want {
							t.Fatalf("%s %s %s %s: got %v, oracle %v", sub, res, act, obj, got, want)
						}
					}
				}
			}
		}
	})
}

// Fast paths (match-all, literal equality, literal substring) agree with the libraries.
func TestPropertyPatternFastPaths(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		alphabet := rapid.SampledFrom([]rune("ab/*?.-,[]{}\\^$+"))
		pattern := string(rapid.SliceOfN(alphabet, 0, 8).Draw(t, "pattern"))
		value := string(rapid.SliceOfN(rapid.SampledFrom([]rune("ab/.-,*?")), 0, 10).Draw(t, "value"))

		gm := compilePattern(pattern, GlobMatchMode)
		g, err := glob.Compile(pattern)
		got, _ := gm.match(value)
		if err != nil {
			if got || gm.kind != matchNever {
				t.Fatalf("glob %q: invalid pattern must never match", pattern)
			}
		} else if want := g.Match(value); got != want {
			t.Fatalf("glob %q on %q: got %v want %v (kind %d)", pattern, value, got, want, gm.kind)
		}

		rm := compilePattern(pattern, RegexMatchMode)
		rgot, rerr := rm.match(value)
		rwant, werr := regexp.MatchString(pattern, value)
		if (rerr != nil) != (werr != nil) || rgot != rwant {
			t.Fatalf("regex %q on %q: got %v/%v want %v/%v", pattern, value, rgot, rerr, rwant, werr)
		}
	})
}

// Composing ConfigMap keys as one CSV or as named sources yields the same decisions.
func TestPropertyConfigMapComposition(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		keys := []string{ConfigMapPolicyCSVKey, "policy.a.csv", "policy.b.csv", "policy.zz.csv", ConfigMapPolicyDefaultKey, "other"}
		data := map[string]string{}
		for _, k := range keys {
			if rapid.Bool().Draw(t, "has-"+k) {
				data[k] = strings.Join(genPolicy(GlobMatchMode).Draw(t, k), "\n")
			}
		}
		one, _, err1 := Parse(PolicyCSV(data), ParseOptions{})
		many, _, err2 := ParseSources(PolicySources(data), ParseOptions{})
		if (err1 != nil) != (err2 != nil) {
			t.Fatalf("composition disagrees on validity: %v vs %v", err1, err2)
		}
		if err1 != nil {
			return
		}
		for _, sub := range append(append([]string{}, propUsers...), propRoles...) {
			for _, obj := range propObjects {
				if one.EnforceSubject(sub, ResourceApplications, ActionGet, obj).Allowed != many.EnforceSubject(sub, ResourceApplications, ActionGet, obj).Allowed {
					t.Fatalf("decision differs for %s %s", sub, obj)
				}
			}
		}
	})
}

// The quote-free fast path of splitCSVLine agrees with encoding/csv.
func TestPropertyCSVFastPath(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		runes := rapid.SliceOfN(rapid.SampledFrom([]rune("ab ,\t\r\u00a0\u2003:*/\x00\xff")), 1, 24).Draw(t, "line")
		text := strings.TrimSpace(string(runes))
		if text == "" {
			return
		}
		got, err := splitCSVLine(text)
		if err != nil {
			t.Fatalf("fast path failed on %q: %v", text, err)
		}
		r := csv.NewReader(strings.NewReader(text))
		r.TrimLeadingSpace = true
		want, err := r.Read()
		if err != nil {
			t.Fatalf("encoding/csv failed on %q: %v", text, err)
		}
		if strings.Join(got, "\x1f") != strings.Join(want, "\x1f") || len(got) != len(want) {
			t.Fatalf("%q: fast path %q, encoding/csv %q", text, got, want)
		}
	})
}
