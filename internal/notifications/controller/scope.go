package controller

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/gobwas/glob"
	"k8s.io/apimachinery/pkg/util/validation"
)

// namespaceMatcher decides whether an Application namespace is in scope: the control-plane
// namespace always is; other namespaces must match an application.namespaces entry. Entries
// wrapped in "/" are regular expressions, every entry is also tried as a glob (upstream
// glob.MatchStringInList in REGEXP mode). Patterns are compiled once at startup. An entry that
// cannot be compiled is reported as a warning and never matches, like the other components
// reading application.namespaces (internal/webhook, internal/project): the start is not
// failed. Regular expressions are Go RE2 while upstream uses regexp2, so lookaround entries
// such as /^((?!not-allowed).)*$/ match upstream but never match here (different by design).
type namespaceMatcher struct {
	control  string
	patterns []nsPattern
}

type nsPattern struct {
	raw string
	re  *regexp.Regexp
	g   *glob.Pattern
}

func newNamespaceMatcher(control string, patterns []string) (m namespaceMatcher, warnings []string) {
	m = namespaceMatcher{control: control}
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		np := nsPattern{raw: p}
		if isRegexpPattern(p) {
			re, err := regexp.Compile(p[1 : len(p)-1])
			if err != nil {
				// The glob fallback of a "/.../" entry starts with a literal "/", which no
				// namespace name contains, so the entry can never match: drop it.
				warnings = append(warnings, fmt.Sprintf("application namespace expression %q is not a valid RE2 regular expression and never matches (lookaround is not supported): %v", p, err))
				continue
			}
			np.re = re
		}
		g, err := glob.Compile(p)
		if err != nil && np.re == nil {
			warnings = append(warnings, fmt.Sprintf("application namespace pattern %q is not a valid glob and never matches: %v", p, err))
			continue
		}
		np.g = g
		m.patterns = append(m.patterns, np)
	}
	return m, warnings
}

func isRegexpPattern(p string) bool {
	return len(p) >= 2 && strings.HasPrefix(p, "/") && strings.HasSuffix(p, "/")
}

// matches reports whether Applications of namespace ns are processed.
func (m namespaceMatcher) matches(ns string) bool {
	if ns == m.control {
		return true
	}
	for _, p := range m.patterns {
		if p.re != nil && p.re.MatchString(ns) {
			return true
		}
		if p.g != nil && p.g.Match(ns) {
			return true
		}
	}
	return false
}

// multiNamespace reports whether Applications outside the control-plane namespace are in scope.
func (m namespaceMatcher) multiNamespace() bool { return len(m.patterns) > 0 }

// literalNamespaces returns the sorted set of namespaces in scope when every pattern is a plain
// namespace name, so the caches can be restricted to them. ok is false when any pattern is a
// glob or a regular expression (cluster-wide watch required).
func (m namespaceMatcher) literalNamespaces() (namespaces []string, ok bool) {
	set := map[string]struct{}{m.control: {}}
	for _, p := range m.patterns {
		if p.re != nil || strings.ContainsAny(p.raw, `*?[]{}\!`) || len(validation.IsDNS1123Label(p.raw)) > 0 {
			return nil, false
		}
		set[p.raw] = struct{}{}
	}
	for ns := range set {
		namespaces = append(namespaces, ns)
	}
	sort.Strings(namespaces)
	return namespaces, true
}
