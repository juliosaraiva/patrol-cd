package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

// allowRule is one line of the skip allowlist.
type allowRule struct {
	line    int
	text    string // the line as written
	pkgExpr string // the package field as written
	pkg     *regexp.Regexp
	test    *regexp.Regexp
	reason  *regexp.Regexp
	comment string

	hits       int  // skipped tests this rule matched
	applicable bool // its package pattern matched a package of the input
}

// allowlist is the parsed -skips file. A nil or empty list allows nothing.
type allowlist struct {
	path  string
	rules []*allowRule
}

// mustRule is one line of the must-run list.
type mustRule struct {
	line    int
	pkg     string
	expr    string
	test    *regexp.Regexp
	comment string

	seen bool // the package occurred in the input
}

// mustRunList is the parsed -must-run file, indexed by package.
type mustRunList struct {
	path  string
	rules []*mustRule
	byPkg map[string][]*mustRule
}

// anchored compiles expr so that it must match a whole string; "." also matches newlines, so
// that a reason pattern can cover a multi-line skip message. expr must parse on its own first:
// an unbalanced ")" would otherwise close the wrapper's group, and "x)|(.*" would compile to
// ^(?s:x)|(.*)$, which matches every string.
func anchored(expr string) (*regexp.Regexp, error) {
	if _, err := regexp.Compile(expr); err != nil {
		return nil, err
	}
	return regexp.Compile(`^(?s:` + expr + `)$`)
}

func loadAllowlist(path string) (*allowlist, error) {
	a := &allowlist{path: path}
	if path == "" {
		return a, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only file
	a.rules, err = parseAllowlist(path, f)
	if err != nil {
		return nil, err
	}
	return a, nil
}

func parseAllowlist(name string, r io.Reader) ([]*allowRule, error) {
	var rules []*allowRule
	err := eachRuleLine(r, func(n int, line string) error {
		f := strings.SplitN(line, "\t", 4)
		if len(f) != 4 {
			return fmt.Errorf("%s:%d: want 4 tab-separated fields (package regexp, test regexp, reason regexp, comment), got %d", name, n, len(f))
		}
		rule := &allowRule{line: n, text: line, pkgExpr: f[0], comment: strings.TrimSpace(f[3])}
		if rule.comment == "" {
			return fmt.Errorf("%s:%d: the comment (why this skip is legitimate) is mandatory", name, n)
		}
		for i, dst := range []**regexp.Regexp{&rule.pkg, &rule.test, &rule.reason} {
			what := [...]string{"package", "test", "reason"}[i]
			if f[i] == "" && i < 2 {
				return fmt.Errorf("%s:%d: empty %s regexp (use .* to match any)", name, n, what)
			}
			re, err := anchored(f[i])
			if err != nil {
				return fmt.Errorf("%s:%d: %s regexp: %w", name, n, what, err)
			}
			*dst = re
		}
		rules = append(rules, rule)
		return nil
	})
	return rules, err
}

func loadMustRun(path string) (*mustRunList, error) {
	m := &mustRunList{path: path, byPkg: map[string][]*mustRule{}}
	if path == "" {
		return m, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only file
	rules, err := parseMustRun(path, f)
	if err != nil {
		return nil, err
	}
	m.rules = rules
	for _, r := range rules {
		m.byPkg[r.pkg] = append(m.byPkg[r.pkg], r)
	}
	return m, nil
}

func parseMustRun(name string, r io.Reader) ([]*mustRule, error) {
	var rules []*mustRule
	err := eachRuleLine(r, func(n int, line string) error {
		f := strings.SplitN(line, "\t", 3)
		if len(f) != 3 {
			return fmt.Errorf("%s:%d: want 3 tab-separated fields (package, test regexp, comment), got %d", name, n, len(f))
		}
		rule := &mustRule{line: n, pkg: f[0], expr: f[1], comment: strings.TrimSpace(f[2])}
		switch {
		case rule.pkg == "" || strings.ContainsAny(rule.pkg, " *?[]()^$\\"):
			return fmt.Errorf("%s:%d: the package must be a full import path, got %q", name, n, rule.pkg)
		case rule.expr == "":
			return fmt.Errorf("%s:%d: empty test regexp", name, n)
		case rule.comment == "":
			return fmt.Errorf("%s:%d: the comment (why this test must run) is mandatory", name, n)
		}
		re, err := anchored(rule.expr)
		if err != nil {
			return fmt.Errorf("%s:%d: test regexp: %w", name, n, err)
		}
		rule.test = re
		rules = append(rules, rule)
		return nil
	})
	return rules, err
}

// eachRuleLine calls fn for every line of r that is neither blank nor a '#' comment.
func eachRuleLine(r io.Reader, fn func(n int, line string) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimRight(sc.Text(), "\r")
		if t := strings.TrimSpace(line); t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if err := fn(n, line); err != nil {
			return err
		}
	}
	return sc.Err()
}

// checkRules compares the rule files with the packages of the module (`go list ./...`). A
// must-run rule whose package is not among them would never apply again (the package was
// renamed or moved), and an allowlist line whose package regexp matches none of them can never
// match a skip. It returns one problem per such line.
func checkRules(allow *allowlist, must *mustRunList, pkgs []string) []string {
	have := make(map[string]bool, len(pkgs))
	for _, p := range pkgs {
		have[p] = true
	}
	var problems []string
	for _, r := range must.rules {
		if !have[r.pkg] {
			problems = append(problems, fmt.Sprintf("%s:%d: package %s does not exist (renamed or moved?)", must.path, r.line, r.pkg))
		}
	}
	for _, r := range allow.rules {
		matched := false
		for _, p := range pkgs {
			if r.pkg.MatchString(p) {
				matched = true
				break
			}
		}
		if !matched {
			problems = append(problems, fmt.Sprintf("%s:%d: package regexp %s matches no package", allow.path, r.line, r.pkgExpr))
		}
	}
	return problems
}

// notePackage records that pkg occurs in the input, for the stale-rule check.
func (a *allowlist) notePackage(pkg string) {
	for _, r := range a.rules {
		if !r.applicable && r.pkg.MatchString(pkg) {
			r.applicable = true
		}
	}
}

// match returns the line numbers of the rules that allow a skip, counting a hit on each.
func (a *allowlist) match(pkg, test, reason string) []int {
	var lines []int
	for _, r := range a.rules {
		if r.allows(pkg, test, reason) {
			r.hits++
			lines = append(lines, r.line)
		}
	}
	return lines
}

// find returns the line numbers of the rules that allow a skip, without counting hits.
func (a *allowlist) find(pkg, test, reason string) []int {
	var lines []int
	for _, r := range a.rules {
		if r.allows(pkg, test, reason) {
			lines = append(lines, r.line)
		}
	}
	return lines
}

func (r *allowRule) allows(pkg, test, reason string) bool {
	return r.pkg.MatchString(pkg) && r.test.MatchString(test) && r.reason.MatchString(reason)
}

// suggestMaxBytes bounds the literal part of a suggested reason regexp; a longer first line
// is cut and followed by ".*".
const suggestMaxBytes = 200

// suggestRule returns an allowlist line that allows exactly this skip. test is a regexp. A tab
// or carriage return of the reason is written as the regexp escape \t or \r: raw, a tab would
// split the line's fields and a carriage return would end the line in the job log.
func suggestRule(pkg, testExpr, reason string) string {
	reasonExpr := ""
	if reason != "" {
		first, rest, multi := strings.Cut(reason, "\n")
		switch {
		case len(first) > suggestMaxBytes:
			reasonExpr = regexp.QuoteMeta(cutRunes(first, suggestMaxBytes)) + `.*`
		case multi && rest != "":
			reasonExpr = regexp.QuoteMeta(first) + `\n.*`
		default:
			reasonExpr = regexp.QuoteMeta(first)
		}
		reasonExpr = escapeCR(strings.ReplaceAll(reasonExpr, "\t", `\t`))
	}
	return strings.Join([]string{regexp.QuoteMeta(pkg), testExpr, reasonExpr, "TODO: why this skip is legitimate on CI"}, "\t")
}
