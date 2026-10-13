package rbac

import (
	"fmt"
	"strconv"
)

// effect is the effect of a 'p' rule.
type effect uint8

const (
	// effectNone is an effect value other than allow/deny: the rule matches but never
	// contributes (Casbin "indeterminate").
	effectNone effect = iota
	effectAllow
	effectDeny
)

func parseEffect(s string) effect {
	switch s {
	case EffectAllow:
		return effectAllow
	case EffectDeny:
		return effectDeny
	default:
		return effectNone
	}
}

// location identifies the line a rule came from.
type location struct {
	source string
	line   int
	text   string
}

// String renders "<source>:<line>: <text>".
func (l location) String() string {
	if l.line > 0 {
		return fmt.Sprintf("%s:%d: %s", l.source, l.line, l.text)
	}
	return fmt.Sprintf("%s: %s", l.source, l.text)
}

// rule is one compiled 'p' line.
type rule struct {
	sub           string
	res, act, obj *matcher
	eft           effect
	loc           location
	// matched is loc rendered once for Decision.MatchedPolicy.
	matched string
}

// subjectRules indexes the rules of one subject. In glob mode, rules whose resource is a
// literal are bucketed by that literal, since they can only match an equal resource.
type subjectRules struct {
	byResource map[string][]*rule
	other      []*rule
}

// Policy is a parsed and compiled policy: an immutable value that is safe for
// concurrent use. Obtain one with Parse or ParseSources.
type Policy struct {
	mode      string
	numRules  int
	numLinks  int
	bySubject map[string]*subjectRules
	// links maps a member to the roles assigned to it by 'g' lines, in line order.
	links map[string][]string
	// roles lists the distinct role names ('g' line targets) in first-appearance order;
	// roleLoc is the first 'g' line naming each.
	roles   []string
	roleLoc map[string]location
	// subjects lists the distinct 'p' subjects.
	subjects map[string]struct{}
}

// MatchMode returns the match mode the policy was compiled for.
func (p *Policy) MatchMode() string { return p.mode }

// NumRules returns the number of 'p' rules.
func (p *Policy) NumRules() int { return p.numRules }

// NumLinks returns the number of 'g' lines.
func (p *Policy) NumLinks() int { return p.numLinks }

// Roles returns the distinct role names assigned by 'g' lines, in first-appearance order.
func (p *Policy) Roles() []string { return append([]string(nil), p.roles...) }

// EnforceSubject evaluates a single raw subject (user, group or role name) against this
// policy alone: no default role, no groups, no project policies. It is the primitive the
// Enforcer composes, exposed for tooling.
func (p *Policy) EnforceSubject(subject, resource, action, object string) Decision {
	res := evaluate(subject, []*Policy{p}, resource, action, object)
	return res.decision(subject, kindSubject)
}

// builder accumulates rules and links for one Policy.
type builder struct {
	mode  string
	cache map[string]*matcher
	p     *Policy
}

func newBuilder(mode string) *builder {
	return &builder{
		mode:  mode,
		cache: make(map[string]*matcher),
		p: &Policy{
			mode:      mode,
			bySubject: make(map[string]*subjectRules),
			links:     make(map[string][]string),
			roleLoc:   make(map[string]location),
			subjects:  make(map[string]struct{}),
		},
	}
}

// emptyPolicy returns a policy without rules (denies everything).
func emptyPolicy(mode string) *Policy { return newBuilder(mode).build() }

func (b *builder) compile(pattern string) *matcher {
	if m, ok := b.cache[pattern]; ok {
		return m
	}
	m := compilePattern(pattern, b.mode)
	b.cache[pattern] = m
	return m
}

func (b *builder) addRule(sub, res, act, obj string, eft effect, loc location) *rule {
	r := &rule{
		sub:     sub,
		res:     b.compile(res),
		act:     b.compile(act),
		obj:     b.compile(obj),
		eft:     eft,
		loc:     loc,
		matched: loc.String(),
	}
	sr := b.p.bySubject[sub]
	if sr == nil {
		sr = &subjectRules{}
		b.p.bySubject[sub] = sr
	}
	if r.res.literal() {
		if sr.byResource == nil {
			sr.byResource = make(map[string][]*rule)
		}
		sr.byResource[r.res.lit] = append(sr.byResource[r.res.lit], r)
	} else {
		sr.other = append(sr.other, r)
	}
	b.p.subjects[sub] = struct{}{}
	b.p.numRules++
	return r
}

func (b *builder) addLink(member, role string, loc location) {
	b.p.numLinks++
	existing := b.p.links[member]
	for _, r := range existing {
		if r == role {
			return
		}
	}
	b.p.links[member] = append(existing, role)
	if _, ok := b.p.roleLoc[role]; !ok {
		b.p.roleLoc[role] = loc
		b.p.roles = append(b.p.roles, role)
	}
}

func (b *builder) build() *Policy {
	p := b.p
	b.p = nil
	b.cache = nil
	return p
}

// outcome is the result of evaluating one subject.
type outcome uint8

const (
	outcomeNone outcome = iota // no allow matched
	outcomeAllow
	outcomeDeny   // an explicit deny matched
	outcomeFailed // a pattern could not be evaluated (invalid regex): treated as deny
)

// evalResult is the outcome of evaluating one subject, with the deciding rule.
type evalResult struct {
	outcome outcome
	rule    *rule
	err     error
}

// subjectKind names the identity an evaluation was for, in decision reasons.
type subjectKind uint8

const (
	kindSubject subjectKind = iota
	kindGroup
	kindDefaultRole
)

func (k subjectKind) String() string {
	switch k {
	case kindGroup:
		return "group"
	case kindDefaultRole:
		return "default role"
	default:
		return "subject"
	}
}

// decision converts an evaluation result into a Decision for subject.
func (r evalResult) decision(subject string, kind subjectKind) Decision {
	switch r.outcome {
	case outcomeAllow:
		return Decision{Allowed: true, MatchedPolicy: r.rule.matched, Reason: "allowed for " + kind.String() + " " + strconv.Quote(subject)}
	case outcomeDeny:
		return Decision{MatchedPolicy: r.rule.matched, Reason: "explicitly denied for " + kind.String() + " " + strconv.Quote(subject)}
	case outcomeFailed:
		return Decision{MatchedPolicy: r.rule.matched, Reason: fmt.Sprintf("policy evaluation failed for %s %q: %v", kind, subject, r.err)}
	default:
		return Decision{Reason: "no policy grants access to " + kind.String() + " " + strconv.Quote(subject)}
	}
}

// visitedLinearLimit is the closure size up to which visited uses a linear array.
const visitedLinearLimit = 16

// visited is a set of names: an inline array for small role closures (no allocation),
// a map beyond.
type visited struct {
	n     int
	small [visitedLinearLimit]string
	big   map[string]struct{}
}

// add inserts name and reports whether it was absent.
func (v *visited) add(name string) bool {
	if v.big != nil {
		if _, ok := v.big[name]; ok {
			return false
		}
		v.big[name] = struct{}{}
		return true
	}
	for i := range v.n {
		if v.small[i] == name {
			return false
		}
	}
	if v.n < visitedLinearLimit {
		v.small[v.n] = name
		v.n++
		return true
	}
	v.big = make(map[string]struct{}, 2*visitedLinearLimit)
	for _, s := range v.small {
		v.big[s] = struct{}{}
	}
	v.big[name] = struct{}{}
	return true
}

// evaluate evaluates one subject against the union of pols. The subject's roles are
// discovered breadth-first through 'g' links of every policy, at most maxHierarchyLevel
// hops away. The subject is allowed when some allow rule of the subject or one of its
// roles matches and no deny rule does; an invalid regular expression reached during
// matching fails the evaluation.
func evaluate(subject string, pols []*Policy, resource, action, object string) evalResult {
	var (
		seen        visited
		frontierBuf [8]string
		nextBuf     [8]string
		allow       *rule
	)
	seen.add(subject)
	frontierBuf[0] = subject
	frontier := frontierBuf[:1]
	next := nextBuf[:0]
	for level := 0; ; level++ {
		for _, name := range frontier {
			for _, p := range pols {
				sr := p.bySubject[name]
				if sr == nil {
					continue
				}
				if res, done := scanRules(sr.byResource[resource], true, resource, action, object, &allow); done {
					return res
				}
				if res, done := scanRules(sr.other, false, resource, action, object, &allow); done {
					return res
				}
			}
		}
		if level == maxHierarchyLevel {
			break
		}
		next = next[:0]
		for _, name := range frontier {
			for _, p := range pols {
				for _, role := range p.links[name] {
					if seen.add(role) {
						next = append(next, role)
					}
				}
			}
		}
		if len(next) == 0 {
			break
		}
		frontier, next = next, frontier
	}
	if allow != nil {
		return evalResult{outcome: outcomeAllow, rule: allow}
	}
	return evalResult{outcome: outcomeNone}
}

// scanRules matches rules in order. It returns done=true on an explicit deny or an
// evaluation failure; the first matching allow is stored in *allow. resourceMatched
// skips the resource comparison for rules bucketed by an equal literal resource.
func scanRules(rules []*rule, resourceMatched bool, resource, action, object string, allow **rule) (evalResult, bool) {
	for _, r := range rules {
		ok, err := matchRule(r, resourceMatched, resource, action, object)
		if err != nil {
			return evalResult{outcome: outcomeFailed, rule: r, err: err}, true
		}
		if !ok {
			continue
		}
		switch r.eft {
		case effectDeny:
			return evalResult{outcome: outcomeDeny, rule: r}, true
		case effectAllow:
			if *allow == nil {
				*allow = r
			}
		}
	}
	return evalResult{}, false
}

// matchRule compares resource, action and object in that order, stopping at the first
// mismatch (the upstream matcher short-circuits the same way, which decides whether an
// invalid regex in a later field is ever evaluated).
func matchRule(r *rule, resourceMatched bool, resource, action, object string) (bool, error) {
	if !resourceMatched {
		if ok, err := r.res.match(resource); !ok || err != nil {
			return false, err
		}
	}
	if ok, err := r.act.match(action); !ok || err != nil {
		return false, err
	}
	return r.obj.match(object)
}

// hasMember reports whether name is the member of a 'g' line in any of pols. Upstream
// only evaluates a token group as a subject when it is such a member.
func hasMember(name string, pols []*Policy) bool {
	for _, p := range pols {
		if _, ok := p.links[name]; ok {
			return true
		}
	}
	return false
}
