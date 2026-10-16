package webhook

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/gobwas/glob"
)

// namespaceMatcher decides whether objects in a namespace are considered: the
// control-plane namespace always, plus namespaces matching a list of patterns. An entry
// wrapped in '/' is first tried as an (unanchored, RE2) regular expression, then every
// entry is tried as a glob, like upstream's glob.MatchStringInList in REGEXP mode. An
// entry that compiles neither way never matches (as upstream), and is reported once.
type namespaceMatcher struct {
	controlPlane string
	entries      []nsPattern
}

type nsPattern struct {
	re *regexp.Regexp // nil unless the entry is "/.../" and compiles
	g  *glob.Pattern  // nil when the entry is not a valid glob
}

func newNamespaceMatcher(controlPlane string, patterns []string) (m *namespaceMatcher, warnings []string) {
	m = &namespaceMatcher{controlPlane: controlPlane}
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		var e nsPattern
		if len(p) >= 2 && strings.HasPrefix(p, "/") && strings.HasSuffix(p, "/") {
			// Upstream uses .NET-style regexp2; RE2 has no lookaround.
			re, err := regexp.Compile(p[1 : len(p)-1])
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("namespace pattern %q is not a valid RE2 expression: %v", p, err))
			} else {
				e.re = re
			}
		}
		if g, err := glob.Compile(p); err == nil {
			e.g = g
		} else if e.re == nil {
			warnings = append(warnings, fmt.Sprintf("namespace pattern %q never matches: %v", p, err))
			continue
		}
		m.entries = append(m.entries, e)
	}
	return m, warnings
}

// clusterWide reports whether objects must be listed in every namespace.
func (m *namespaceMatcher) clusterWide() bool { return len(m.entries) > 0 }

// allowed reports whether objects in ns are considered.
func (m *namespaceMatcher) allowed(ns string) bool {
	if ns == m.controlPlane {
		return true
	}
	for _, e := range m.entries {
		if e.re != nil && e.re.MatchString(ns) {
			return true
		}
		if e.g != nil && e.g.Match(ns) {
			return true
		}
	}
	return false
}
