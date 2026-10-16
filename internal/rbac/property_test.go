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

// Project role fields. The first entries of each list pass upstream's structural
// AppProject validation in both modes. The later ones exercise pattern validation: they
// are appended (never inserted) so that saved rapid fail files keep decoding to the same
// values.
var (
	// '*' is not a project-scoped resource: rejected in both modes.
	propRoleResources = []string{ResourceApplications, ResourceLogs, "*"}
	// '*' is a valid glob and an invalid regular expression; "action/[" is invalid in
	// both modes; "update/(" is a valid glob literal and an invalid regular expression.
	propRoleActions = []string{ActionGet, ActionSync, ActionDelete, "*", "action/[", "update/(", "action/*"}
	// "p1/**" is a valid glob and an invalid regular expression (nested repetition).
	propRoleGlobObjects  = []string{"p1/*", "p1/a", "p1/b", "p1/**"}
	propRoleRegexObjects = []string{"p1/.*", "p1/a", "p1/b", "p1/**", "p1/a**"}
)

// genProjectRoles draws roles for project p1. Lines pass upstream's structural
// AppProject validation most of the time; some fields do not compile in the drawn mode
// (regex: '*', '[', '(' and '**'; glob: '['), or fail the structural rules.
func genProjectRoles(mode string) *rapid.Generator[[]Role] {
	objs := propRoleGlobObjects
	if mode == RegexMatchMode {
		objs = propRoleRegexObjects
	}
	return rapid.Custom(func(t *rapid.T) []Role {
		var policies []string
		for i := range rapid.IntRange(0, 4).Draw(t, "n") {
			policies = append(policies, fmt.Sprintf("p, proj:p1:r, %s, %s, %s, %s",
				rapid.SampledFrom(propRoleResources).Draw(t, fmt.Sprintf("res%d", i)),
				rapid.SampledFrom(propRoleActions).Draw(t, fmt.Sprintf("act%d", i)),
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
				"p, proj:p1:r, applications, *, p1/a, deny", // regex mode: invalid pattern
				"p, proj:p1:r, logs, get, p1/b, deny",
				"p, proj:p1:r, applications, *, p2/a, deny", // out of scope: invalid
				"p, proj:p1:r, applications, get, p1/a",     // malformed: invalid
				// Appended: patterns that do not compile in one or both modes.
				"p, proj:p1:r, applications, action/[, p1/a, deny",
				"p, proj:p1:r, logs, get, p1/**, deny",
				"p, proj:p1:r, applications, update/(, p1/b, deny",
				"p, proj:p1:r, applications, get, p1/(, deny", // invalid object in both modes
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

// The order of policy lines, global or project role, does not change any decision
// (upstream's effect is "some allow and no deny", which is order-independent, and an
// invalid project line reduces its role whatever its position).
func TestPropertyLineOrderIsIrrelevant(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		base := drawSetup(t)
		shuffled := base
		shuffled.lines = rapid.Permutation(base.lines).Draw(t, "perm")
		shuffled.roles = cloneRoles(base.roles)
		shuffled.roles[0].Policies = rapid.Permutation(base.roles[0].Policies).Draw(t, "rolePerm")
		a, b := allowedSet(base.build(t)), allowedSet(shuffled.build(t))
		for i := range a {
			if a[i] != b[i] {
				t.Fatalf("decision %d differs after reordering", i)
			}
		}
	})
}

// unevaluableRules returns the rules of pol with a pattern that does not compile (an
// invalid glob or regular expression, or an untranslatable glob alternation).
func unevaluableRules(pol *Policy) []string {
	var out []string
	if pol == nil {
		return nil
	}
	for _, sr := range pol.bySubject {
		rules := sr.other
		for _, rs := range sr.byResource {
			rules = append(rules, rs...)
		}
		for _, r := range rules {
			for _, m := range []*matcher{r.res, r.act, r.obj} {
				if m.kind == matchFail || m.kind == matchNever {
					out = append(out, r.matched)
				}
			}
		}
	}
	return out
}

// Project-role confinement, with invalid patterns in the project's lines:
//   - p1's roles never change a decision outside p1;
//   - a p1 token gets nothing in the registered project p2 beyond what the default role
//     alone grants;
//   - no compiled p1 rule contains a pattern that does not compile, and no decision is
//     an evaluation failure (no built-in policy is loaded here, and user policies with
//     such patterns do not load).
func TestPropertyProjectRoleConfinement(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		withRoles := drawSetup(t)
		withoutRoles := withRoles
		withoutRoles.roles = []Role{}
		e1, e0 := withRoles.build(t), withoutRoles.build(t)
		if bad := unevaluableRules(e1.load().projects["p1"].policy); len(bad) > 0 {
			t.Fatalf("project rules with patterns that do not compile: %q", bad)
		}
		anonymous := Subjects{}
		for _, r := range propRequests() {
			d1 := e1.Enforce(r.s, r.resource, r.action, r.object)
			if strings.Contains(d1.Reason, "evaluation failed") {
				t.Fatalf("%+v %s %s %s: %s", r.s, r.resource, r.action, r.object, d1.Reason)
			}
			project, _ := ProjectFromObject(r.resource, r.object)
			if project != "p1" {
				if d0 := e0.Enforce(r.s, r.resource, r.action, r.object); d0.Allowed != d1.Allowed {
					t.Fatalf("p1's roles changed a decision outside p1 for %+v %s %s %s: %v -> %v (%s)", r.s, r.resource, r.action, r.object, d0.Allowed, d1.Allowed, d1.Reason)
				}
			}
			if r.s.ProjectRole != nil && r.s.ProjectRole.Project == "p1" && project == "p2" && d1.Allowed &&
				!e1.Enforce(anonymous, r.resource, r.action, r.object).Allowed {
				t.Fatalf("p1 token allowed in p2 beyond the default role: %s %s %s (%s)", r.resource, r.action, r.object, d1.Reason)
			}
		}
	})
}

// A project role made only of allow lines, valid or not (including patterns that do
// not compile in the mode, such as '*' in regex mode), never denies anything: it can
// only add access. This is the property the evaluation-time regex error broke: an allow
// line with action '*' used to deny the role's members every request in the project.
func TestPropertyProjectAllowLinesNeverDeny(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		withRoles := drawSetup(t)
		withRoles.roles = cloneRoles(withRoles.roles)
		for i, l := range withRoles.roles[0].Policies {
			if rest, ok := strings.CutSuffix(l, EffectDeny); ok {
				withRoles.roles[0].Policies[i] = rest + EffectAllow
			}
		}
		withRoles.roles[0].Policies = append(withRoles.roles[0].Policies, rapid.SliceOfN(rapid.SampledFrom([]string{
			"p, proj:p1:r, applications, *, p1/.*, allow",
			"p, proj:p1:r, applications, action/[, p1/a, allow",
			"p, proj:p1:r, logs, update/(, p1/b, allow",
			"p, proj:p1:r, applications, get, p1/**, allow",
			"p, proj:p1:r, applications, get, p2/a, allow",
		}), 0, 2).Draw(t, "extraAllows")...)
		// Make the role's members hold global grants the role could wrongly take away:
		// at least one bound group, and a global allow on everything for a drawn subset
		// of the bound groups.
		if len(withRoles.roles[0].Groups) == 0 {
			withRoles.roles[0].Groups = []string{genSubjectName().Draw(t, "group")}
		}
		withRoles.lines = append([]string(nil), withRoles.lines...)
		all := "*"
		if withRoles.mode == RegexMatchMode {
			all = ".*"
		}
		for _, g := range withRoles.roles[0].Groups {
			if rapid.Bool().Draw(t, "grant-"+g) {
				for _, res := range []string{ResourceApplications, ResourceLogs} {
					withRoles.lines = append(withRoles.lines, fmt.Sprintf("p, %s, %s, %s, %s, allow", g, res, all, all))
				}
			}
		}
		withoutRoles := withRoles
		withoutRoles.roles = []Role{}
		before, after := allowedSet(withoutRoles.build(t)), allowedSet(withRoles.build(t))
		reqs := propRequests()
		for i := range reqs {
			if before[i] && !after[i] {
				t.Fatalf("project allow lines denied %+v %s %s %s", reqs[i].s, reqs[i].resource, reqs[i].action, reqs[i].object)
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

// oracleHasInvalidRegex reports whether a 'p' line of lines has a resource, action or
// object that regexp.Compile rejects.
func oracleHasInvalidRegex(lines []string) bool {
	for _, l := range lines {
		pl, err := tokenizeLine(l, DefaultMaxLineBytes)
		if err != nil || pl.kind != linePolicy {
			continue
		}
		for _, f := range pl.tokens[2:5] {
			if _, err := regexp.Compile(f); err != nil {
				return true
			}
		}
	}
	return false
}

// The indexed, compiled evaluator agrees with the naive oracle for raw subjects. A user
// source with an invalid regular expression does not load; the same lines in the
// built-in source load, and their evaluation fails closed exactly where the oracle (and
// upstream Casbin) errors.
func TestPropertyEvaluatorMatchesNaiveOracle(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		mode := rapid.SampledFrom([]string{GlobMatchMode, RegexMatchMode}).Draw(t, "mode")
		lines := genPolicy(mode).Draw(t, "policy")
		text := strings.Join(lines, "\n")
		_, _, userErr := Parse(text, ParseOptions{MatchMode: mode})
		if wantErr := mode == RegexMatchMode && oracleHasInvalidRegex(lines); (userErr != nil) != wantErr {
			t.Fatalf("user policy load error %v, want error: %v", userErr, wantErr)
		}
		pol, _, err := ParseSources([]Source{{Name: "builtin", CSV: text, Builtin: true}}, ParseOptions{MatchMode: mode})
		if err != nil {
			t.Fatalf("generated policy failed to parse as built-in: %v", err)
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
