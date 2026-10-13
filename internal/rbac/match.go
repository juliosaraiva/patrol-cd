package rbac

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/gobwas/glob"
)

// matchKind selects the comparison a compiled pattern performs.
type matchKind uint8

const (
	// matchAny matches every value (glob patterns made only of '*').
	matchAny matchKind = iota
	// matchEqual is a glob pattern without metacharacters: plain equality.
	matchEqual
	// matchContains is a regex without metacharacters: unanchored, so substring search.
	matchContains
	// matchGlob is a compiled glob pattern.
	matchGlob
	// matchRegex is a compiled regular expression (unanchored, like regexp.MatchString).
	matchRegex
	// matchNever is a glob pattern that failed to compile. Upstream treats it as a
	// non-match, so the rule containing it never applies.
	matchNever
	// matchFail is a regular expression that failed to compile. Upstream evaluation
	// errors (and therefore denies) when it reaches such a pattern.
	matchFail
)

// matcher is a precompiled resource, action or object pattern. It is immutable.
type matcher struct {
	kind matchKind
	lit  string
	g    *glob.Pattern
	re   *regexp.Regexp
	// err is the compilation error of an invalid pattern; evalErr is the error returned
	// when an invalid regular expression is evaluated.
	err     error
	evalErr error
}

// globMeta lists the characters with special meaning in gobwas/glob patterns.
const globMeta = `*?[]{}\`

// normalizeMatchMode maps a policy.matchMode value to a supported mode. Like upstream,
// anything other than "regex" selects glob; known is false for values that are neither
// empty, "glob" nor "regex".
func normalizeMatchMode(mode string) (normalized string, known bool) {
	switch mode {
	case RegexMatchMode:
		return RegexMatchMode, true
	case GlobMatchMode, "":
		return GlobMatchMode, true
	default:
		return GlobMatchMode, false
	}
}

// compilePattern compiles pattern for the given (normalized) match mode. Compilation
// problems are recorded in the returned matcher, never returned as errors, because
// upstream accepts such policies and only fails at match time.
func compilePattern(pattern, mode string) *matcher {
	if mode == RegexMatchMode {
		if regexp.QuoteMeta(pattern) == pattern {
			return &matcher{kind: matchContains, lit: pattern}
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return &matcher{kind: matchFail, lit: pattern, err: err,
				evalErr: fmt.Errorf("%w: invalid regular expression %q: %w", errPatternEvaluation, pattern, err)}
		}
		return &matcher{kind: matchRegex, lit: pattern, re: re}
	}
	if pattern != "" && strings.Trim(pattern, "*") == "" {
		return &matcher{kind: matchAny, lit: pattern}
	}
	if !strings.ContainsAny(pattern, globMeta) {
		return &matcher{kind: matchEqual, lit: pattern}
	}
	g, err := glob.Compile(pattern)
	if err != nil {
		return &matcher{kind: matchNever, lit: pattern, err: err}
	}
	return &matcher{kind: matchGlob, lit: pattern, g: g}
}

// errPatternEvaluation is wrapped by match errors caused by invalid regular expressions.
var errPatternEvaluation = errors.New("pattern cannot be evaluated")

// match reports whether value matches the pattern. An error is returned only for
// regular expressions that failed to compile.
func (m *matcher) match(value string) (bool, error) {
	switch m.kind {
	case matchAny:
		return true, nil
	case matchEqual:
		return value == m.lit, nil
	case matchContains:
		return strings.Contains(value, m.lit), nil
	case matchGlob:
		return m.g.Match(value), nil
	case matchRegex:
		return m.re.MatchString(value), nil
	case matchFail:
		return false, m.evalErr
	default: // matchNever
		return false, nil
	}
}

// literal reports whether the pattern can only match the exact string m.lit. Used to
// index rules by resource.
func (m *matcher) literal() bool { return m.kind == matchEqual }

// isLiteralToken reports whether s contains no pattern metacharacters for mode, so it is
// a candidate for "unknown resource/action" validation warnings.
func isLiteralToken(s, mode string) bool {
	if mode == RegexMatchMode {
		return regexp.QuoteMeta(s) == s
	}
	return !strings.ContainsAny(s, globMeta)
}
