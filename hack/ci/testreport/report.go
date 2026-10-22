package main

import (
	"fmt"
	"html"
	"strconv"
	"strings"
	"time"
)

const (
	resultOK         = "ok"
	resultFailed     = "failed"
	resultNoEvents   = "no-events"
	resultViolations = "violations"
	// markdownBudget keeps the summary under GitHub's 1 MiB step summary limit.
	markdownBudget = 900 << 10
	// stepSummaryLimit is GitHub's limit for the step summary of one step.
	stepSummaryLimit = 1 << 20
)

// Summary is the report data; -json-out writes it as is. Field order is the output order, and
// every list is sorted, so two runs over the same input produce the same bytes.
//
// Result is the outcome of the whole run: "ok", "failed" (a test, package or build failed or
// did not finish, or the skip policy was violated) or "no-events". TestsResult is the test
// outcome alone ("ok", "failed" or "no-events") and PolicyResult the skip policy alone ("ok"
// or "violations"). Result decides the exit status: 0 for "ok", 1 for "failed", 2 for
// "no-events".
type Summary struct {
	Title             string          `json:"title"`
	Result            string          `json:"result"`
	TestsResult       string          `json:"tests_result"`
	PolicyResult      string          `json:"policy_result"`
	StrictAllowlist   bool            `json:"strict_allowlist"`
	Totals            Totals          `json:"totals"`
	MustRunViolations []Violation     `json:"must_run_violations"`
	StaleAllowlist    []RuleRef       `json:"stale_allowlist_lines"`
	Failures          []Failure       `json:"failures"`
	Skips             []Skip          `json:"skips"`
	SlowestPackages   []Timing        `json:"slowest_packages"`
	SlowestTests      []Timing        `json:"slowest_tests"`
	NoTestsRun        []string        `json:"packages_no_tests_run"`
	Packages          []PackageResult `json:"packages"`
	NonJSON           []string        `json:"non_json_lines"`
	// Stderr is the tail of the -stderr file (go test's own error output), shown, not parsed.
	Stderr      []string `json:"stderr,omitempty"`
	StderrLines int      `json:"stderr_lines,omitempty"`
	StderrPath  string   `json:"stderr_path,omitempty"`
}

// Totals counts tests at every level (a subtest is a test).
type Totals struct {
	Events                 int     `json:"events"`
	NonJSONLines           int     `json:"non_json_lines"`
	Packages               int     `json:"packages"`
	PackagesOK             int     `json:"packages_ok"`
	PackagesFailed         int     `json:"packages_failed"`
	PackagesNoTestFiles    int     `json:"packages_no_test_files"`
	PackagesNoTestsRun     int     `json:"packages_no_tests_run"`
	PackagesIncomplete     int     `json:"packages_incomplete"`
	BuildFailures          int     `json:"build_failures"`
	Tests                  int     `json:"tests"`
	TestsPassed            int     `json:"tests_passed"`
	TestsFailed            int     `json:"tests_failed"`
	TestsSkipped           int     `json:"tests_skipped"`
	TestsIncomplete        int     `json:"tests_incomplete"`
	SkipsAllowed           int     `json:"skips_allowed"`
	SkipsForbidden         int     `json:"skips_forbidden"`
	MustRunViolations      int     `json:"must_run_violations"`
	MustRunNotApplicable   int     `json:"must_run_not_applicable"`
	StaleAllowlistLines    int     `json:"stale_allowlist_lines"`
	AllowlistNotApplicable int     `json:"allowlist_not_applicable"`
	WallSeconds            float64 `json:"wall_seconds"`
	PackageSeconds         float64 `json:"package_seconds"`
}

// PackageResult is one package run. Status is ok, fail, no-test-files or incomplete.
type PackageResult struct {
	Package     string  `json:"package"`
	Status      string  `json:"status"`
	Elapsed     float64 `json:"elapsed"`
	Passed      int     `json:"passed"`
	Failed      int     `json:"failed"`
	Skipped     int     `json:"skipped"`
	Incomplete  int     `json:"incomplete"`
	FailedBuild string  `json:"failed_build,omitempty"`
}

// Timing is a package (Test empty) or top-level test and its elapsed seconds.
type Timing struct {
	Package string  `json:"package"`
	Test    string  `json:"test,omitempty"`
	Elapsed float64 `json:"elapsed"`
}

// Failure is a failed or unfinished test, a failed package, or a failed build. Output holds at
// most 60 lines of the captured output, plus markers where lines were left out.
type Failure struct {
	Package      string   `json:"package"`
	Test         string   `json:"test,omitempty"`
	Kind         string   `json:"kind"` // test, incomplete, package, package-incomplete or build
	Elapsed      float64  `json:"elapsed"`
	Output       []string `json:"output"`
	OmittedLines int      `json:"omitted_lines,omitempty"`
}

// Skip is a skipped test, or a test whose Subtests skipped subtests were all that ran in it.
type Skip struct {
	Package     string   `json:"package"`
	Test        string   `json:"test"`
	Subtests    int      `json:"subtests,omitempty"`
	Allowed     bool     `json:"allowed"`
	Reasons     []Reason `json:"reasons"`
	Suggestions []string `json:"suggested_allowlist_lines,omitempty"`
}

// Reason is one distinct skip message within a Skip entry. Text is the message the allowlist
// was matched against (at most maxReasonLines lines and maxReasonBytes bytes; Truncated when
// the test printed more; lostText when the message was lost in a cut line). Hint is set for a
// forbidden reason when the message was lost or an earlier message of the test matches the
// allowlist.
type Reason struct {
	Text           string `json:"text"`
	Truncated      bool   `json:"truncated,omitempty"`
	Location       string `json:"location,omitempty"`
	Tests          int    `json:"tests"`
	Allowed        bool   `json:"allowed"`
	AllowlistLines []int  `json:"allowlist_lines,omitempty"`
	Hint           string `json:"hint,omitempty"`
}

// Violation is a must-run rule that this input does not satisfy.
type Violation struct {
	Line    int          `json:"line"`
	Package string       `json:"package"`
	Test    string       `json:"test"`
	Comment string       `json:"comment"`
	Problem string       `json:"problem"`
	Tests   []TestStatus `json:"tests,omitempty"`
}

// TestStatus is a matching test that did not pass: fail, skip, incomplete or subtests-skipped.
type TestStatus struct {
	Test   string `json:"test"`
	Status string `json:"status"`
}

// RuleRef is an allowlist line, as written.
type RuleRef struct {
	Line int    `json:"line"`
	Text string `json:"text"`
}

func (s *Summary) forbidden() []Skip {
	var out []Skip
	for _, sk := range s.Skips {
		if !sk.Allowed {
			out = append(out, sk)
		}
	}
	return out
}

// resultLine is the headline of the reports: the test outcome, then the skip policy outcome.
func resultLine(s *Summary) string {
	if s.Result == resultOK {
		return "OK"
	}
	t := s.Totals
	policy := "skip policy OK"
	if s.PolicyResult != resultOK {
		var why []string
		if t.SkipsForbidden > 0 {
			why = append(why, plural(t.SkipsForbidden, "forbidden skipped test"))
		}
		if t.MustRunViolations > 0 {
			why = append(why, plural(t.MustRunViolations, "must-run violation"))
		}
		if t.StaleAllowlistLines > 0 && s.StrictAllowlist {
			why = append(why, plural(t.StaleAllowlistLines, "stale allowlist line")+" (-strict-allowlist)")
		}
		policy = strings.Join(why, ", ")
	}
	switch s.TestsResult {
	case resultNoEvents:
		head := "NO TEST EVENTS: the input holds no go test -json events; go test's stderr says why"
		if s.PolicyResult != resultOK {
			head += "; " + policy
		}
		return head
	case resultFailed:
		var why []string
		for _, c := range []struct {
			n    int
			what string
		}{
			{t.PackagesFailed, plural(t.PackagesFailed, "failed package")},
			{t.PackagesIncomplete, plural(t.PackagesIncomplete, "package") + " did not finish"},
			{t.BuildFailures, plural(t.BuildFailures, "failed build")},
			{t.TestsFailed, plural(t.TestsFailed, "failed test")},
			{t.TestsIncomplete, plural(t.TestsIncomplete, "test") + " did not finish"},
		} {
			if c.n > 0 {
				why = append(why, c.what)
			}
		}
		return "FAILED: " + strings.Join(why, ", ") + "; " + policy
	default:
		return "FAILED: the tests passed, but " + policy
	}
}

func plural(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return strconv.Itoa(n) + " " + what + "s"
}

func secs(f float64) string { return strconv.FormatFloat(f, 'f', 2, 64) + "s" }

func dur(f float64) string {
	return time.Duration(f * float64(time.Second)).Round(100 * time.Millisecond).String()
}

func failureHeading(f Failure) string {
	what := map[string]string{
		failTest: "FAIL", failIncomplete: "DID NOT FINISH", failPackage: "PACKAGE FAILED",
		failPkgUnfinished: "PACKAGE DID NOT FINISH", failBuild: "BUILD FAILED",
	}[f.Kind]
	h := what + " " + f.Package
	if f.Test != "" {
		h += " " + f.Test
	}
	if f.Kind == failTest || f.Kind == failPackage {
		h += " (" + secs(f.Elapsed) + ")"
	}
	return h
}

func skipName(sk Skip) string {
	if sk.Subtests > 0 {
		return fmt.Sprintf("%s (%s)", sk.Test, plural(sk.Subtests, "skipped subtest"))
	}
	return sk.Test
}

func lineRefs(path string, lines []int) string {
	if len(lines) == 0 {
		return ""
	}
	parts := make([]string, len(lines))
	for i, l := range lines {
		parts[i] = strconv.Itoa(l)
	}
	if path == "" {
		return strings.Join(parts, ",")
	}
	return path + ":" + strings.Join(parts, ",")
}

// renderText is the plain text report printed to stdout.
func renderText(s *Summary, skipsPath, mustPath string) string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	t := s.Totals
	w("%s\n%s\n", s.Title, strings.Repeat("=", len(s.Title)))
	w("Result:   %s\n", resultLine(s))
	w("Packages: %d (%d ok, %d failed, %d without test files, %d ran no tests, %d did not finish)\n",
		t.Packages, t.PackagesOK, t.PackagesFailed, t.PackagesNoTestFiles, t.PackagesNoTestsRun, t.PackagesIncomplete)
	w("Tests:    %d counting subtests (%d passed, %d failed, %d skipped, %d did not finish)\n",
		t.Tests, t.TestsPassed, t.TestsFailed, t.TestsSkipped, t.TestsIncomplete)
	w("Skips:    %d allowed, %d forbidden\n", t.SkipsAllowed, t.SkipsForbidden)
	w("Must-run: %s, %s for packages not in this input\n", plural(t.MustRunViolations, "violation"), plural(t.MustRunNotApplicable, "rule"))
	if t.BuildFailures > 0 {
		w("Builds:   %d failed\n", t.BuildFailures)
	}
	w("Time:     wall %s, sum of package times %s\n", dur(t.WallSeconds), dur(t.PackageSeconds))
	w("Input:    %s, %s\n", plural(t.Events, "event"), plural(t.NonJSONLines, "non-JSON line"))

	if len(s.Stderr) > 0 {
		w("\ngo test stderr (%s, last %d of %d lines)\n", s.StderrPath, len(s.Stderr), s.StderrLines)
		for _, l := range s.Stderr {
			w("  | %s\n", l)
		}
	}
	if len(s.Failures) > 0 {
		w("\nFailures (%d)\n", len(s.Failures))
		for _, f := range s.Failures {
			w("--- %s\n", failureHeading(f))
			for _, l := range f.Output {
				w("    | %s\n", l)
			}
		}
	}
	if fs := s.forbidden(); len(fs) > 0 {
		w("\nForbidden skips (%s): no line of %s matches them\n", plural(t.SkipsForbidden, "skipped test"), orNone(skipsPath))
		var sugs []string
		for _, sk := range fs {
			for _, r := range sk.Reasons {
				if !r.Allowed {
					w("  %s  %s  %q at %s\n", sk.Package, skipName(sk), shortReason(r.Text, r.Truncated), orNone(r.Location))
					if r.Hint != "" {
						w("      hint: %s\n", r.Hint)
					}
				}
			}
			sugs = append(sugs, sk.Suggestions...)
		}
		if len(sugs) > 0 { // none for a lost message
			w("Make these tests run on CI, or, for each skip that is legitimate there, add a line to %s\n", orNone(skipsPath))
			w("(tab-separated; replace the TODO with the reason):\n")
			for _, sug := range sugs {
				w("%s\n", sug)
			}
		}
	}
	if len(s.MustRunViolations) > 0 {
		w("\nMust-run violations (%d)\n", len(s.MustRunViolations))
		for _, v := range s.MustRunViolations {
			w("  %s:%d  %s  %s: %s\n", mustPath, v.Line, v.Package, v.Test, v.Problem)
			for _, ts := range v.Tests {
				w("      %s: %s\n", ts.Test, ts.Status)
			}
		}
	}
	if len(s.StaleAllowlist) > 0 {
		label := "warning"
		if s.StrictAllowlist {
			label = "failure with -strict-allowlist"
		}
		w("\nStale allowlist lines (%d, %s): matched no skip although their package occurs\n", len(s.StaleAllowlist), label)
		for _, r := range s.StaleAllowlist {
			w("  %s:%d: %s\n", skipsPath, r.Line, r.Text)
		}
	}
	if len(s.Skips) > 0 {
		w("\nSkips (%d entries, %s)\n", len(s.Skips), plural(t.SkipsAllowed+t.SkipsForbidden, "skipped test"))
		for _, sk := range s.Skips {
			for _, r := range sk.Reasons {
				state, ref := "allowed  ", " ["+lineRefs(skipsPath, r.AllowlistLines)+"]"
				if !r.Allowed {
					state, ref = "FORBIDDEN", ""
				}
				w("  %s  %s  %s  %q at %s%s\n", state, sk.Package, skipName(sk), shortReason(r.Text, r.Truncated), orNone(r.Location), ref)
			}
		}
	}
	if len(s.SlowestPackages) > 0 {
		w("\nSlowest packages\n")
		for _, p := range s.SlowestPackages {
			w("  %9s  %s\n", secs(p.Elapsed), p.Package)
		}
	}
	if len(s.SlowestTests) > 0 {
		w("\nSlowest top-level tests\n")
		for _, p := range s.SlowestTests {
			w("  %9s  %s %s\n", secs(p.Elapsed), p.Package, p.Test)
		}
	}
	if len(s.NoTestsRun) > 0 {
		w("\nPackages that passed without running a test (%d)\n", len(s.NoTestsRun))
		for _, p := range s.NoTestsRun {
			w("  %s\n", p)
		}
	}
	if t.NonJSONLines > 0 {
		w("\nNon-JSON output (first %d of %d lines)\n", len(s.NonJSON), t.NonJSONLines)
		for _, l := range s.NonJSON {
			w("  | %s\n", l)
		}
	}
	return b.String()
}

func orNone(s string) string {
	if s == "" {
		return "(unknown)"
	}
	return s
}

// mdWriter appends Markdown chunks while they fit in the size budget.
type mdWriter struct {
	b       strings.Builder
	dropped []string
}

func (m *mdWriter) add(section, chunk string) {
	if m.b.Len()+len(chunk) > markdownBudget {
		if len(m.dropped) == 0 || m.dropped[len(m.dropped)-1] != section {
			m.dropped = append(m.dropped, section)
		}
		return
	}
	m.b.WriteString(chunk)
}

// renderMarkdown is the report for $GITHUB_STEP_SUMMARY: totals and the actionable parts in
// full, the long lists folded into <details>.
func renderMarkdown(s *Summary, skipsPath, mustPath string) string {
	var m mdWriter
	t := s.Totals
	var h strings.Builder
	fmt.Fprintf(&h, "## %s\n\n**Result: %s**\n\n", mdText(s.Title), mdText(resultLine(s)))
	h.WriteString("| Packages | ok | failed | no test files | ran no tests | did not finish |\n|---:|---:|---:|---:|---:|---:|\n")
	fmt.Fprintf(&h, "| %d | %d | %d | %d | %d | %d |\n\n", t.Packages, t.PackagesOK, t.PackagesFailed, t.PackagesNoTestFiles, t.PackagesNoTestsRun, t.PackagesIncomplete)
	h.WriteString("| Tests (with subtests) | passed | failed | skipped | did not finish |\n|---:|---:|---:|---:|---:|\n")
	fmt.Fprintf(&h, "| %d | %d | %d | %d | %d |\n\n", t.Tests, t.TestsPassed, t.TestsFailed, t.TestsSkipped, t.TestsIncomplete)
	fmt.Fprintf(&h, "Skipped tests: %d allowed, %d forbidden. Must-run violations: %d. Wall time %s, sum of package times %s. %s, %s",
		t.SkipsAllowed, t.SkipsForbidden, t.MustRunViolations, dur(t.WallSeconds), dur(t.PackageSeconds), plural(t.Events, "event"), plural(t.NonJSONLines, "non-JSON line"))
	if t.BuildFailures > 0 {
		fmt.Fprintf(&h, ", %s", plural(t.BuildFailures, "failed build"))
	}
	h.WriteString(".\n\n")
	m.add("totals", h.String())

	if len(s.Stderr) > 0 {
		open := ""
		if s.TestsResult != resultOK {
			open = " open"
		}
		m.add("go test stderr", fmt.Sprintf("<details%s><summary>go test stderr (last %d of %s)</summary>\n\n%s\n</details>\n\n",
			open, len(s.Stderr), plural(s.StderrLines, "line"), codeBlock(s.Stderr)))
	}

	if fs := s.forbidden(); len(fs) > 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "### Forbidden skips (%s)\n\nThese skips match no line of %s. Make the tests run on CI, or, for each skip that is legitimate there, add the line below (tab-separated) and replace the TODO with the reason.\n\n",
			plural(t.SkipsForbidden, "skipped test"), codeCell(skipsPath))
		b.WriteString("| Package | Test | Reason | Location | Tests |\n|---|---|---|---|---:|\n")
		var sugs, hints []string
		for _, sk := range fs {
			for _, r := range sk.Reasons {
				if r.Allowed {
					continue
				}
				fmt.Fprintf(&b, "| %s | %s | %s | %s | %d |\n", codeCell(sk.Package), codeCell(sk.Test), reasonCell(r), codeCell(r.Location), r.Tests)
				if r.Hint != "" {
					hints = append(hints, fmt.Sprintf("- %s %s: %s\n", codeCell(sk.Package), codeCell(sk.Test), mdText(r.Hint)))
				}
			}
			sugs = append(sugs, sk.Suggestions...)
		}
		if len(hints) > 0 {
			b.WriteString("\n" + strings.Join(hints, ""))
		}
		if len(sugs) > 0 { // none for a lost message
			b.WriteString("\n" + codeBlock(sugs))
		}
		b.WriteString("\n")
		m.add("forbidden skips", b.String())
	}

	if len(s.MustRunViolations) > 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "### Must-run violations (%d)\n\n| Rule | Package | Test | Problem | Tests that did not pass |\n|---|---|---|---|---|\n", len(s.MustRunViolations))
		for _, v := range s.MustRunViolations {
			var tests []string
			for _, ts := range v.Tests {
				tests = append(tests, codeCell(ts.Test)+" "+ts.Status)
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", codeCell(fmt.Sprintf("%s:%d", mustPath, v.Line)), codeCell(v.Package), codeCell(v.Test), mdText(v.Problem), strings.Join(tests, "<br>"))
		}
		m.add("must-run violations", b.String()+"\n")
	}

	if len(s.Failures) > 0 {
		m.add("failures", fmt.Sprintf("### Failures (%d)\n\n", len(s.Failures)))
		for _, f := range s.Failures {
			m.add("failures", fmt.Sprintf("<details><summary>%s</summary>\n\n%s\n</details>\n\n", html.EscapeString(failureHeading(f)), codeBlock(f.Output)))
		}
	}

	if len(s.StaleAllowlist) > 0 {
		var b strings.Builder
		label := "warning"
		if s.StrictAllowlist {
			label = "failure with -strict-allowlist"
		}
		fmt.Fprintf(&b, "### Stale allowlist lines (%d, %s)\n\nThese lines of %s matched no skip although their package pattern matched a package of this run.\n\n",
			len(s.StaleAllowlist), label, codeCell(skipsPath))
		var lines []string
		for _, r := range s.StaleAllowlist {
			lines = append(lines, fmt.Sprintf("%d: %s", r.Line, r.Text))
		}
		b.WriteString(codeBlock(lines) + "\n")
		m.add("stale allowlist lines", b.String())
	}

	if len(s.Skips) > 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "<details><summary>All skips (%d entries, %s)</summary>\n\n| Allowed | Package | Test | Reason | Location | Allowlist lines |\n|---|---|---|---|---|---|\n",
			len(s.Skips), plural(t.SkipsAllowed+t.SkipsForbidden, "skipped test"))
		for _, sk := range s.Skips {
			for _, r := range sk.Reasons {
				allowed := "yes"
				if !r.Allowed {
					allowed = "**no**"
				}
				name := sk.Test
				if sk.Subtests > 0 {
					name = skipName(sk)
				}
				fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s |\n", allowed, codeCell(sk.Package), codeCell(name), reasonCell(r), codeCell(r.Location), mdText(lineRefs("", r.AllowlistLines)))
			}
		}
		b.WriteString("\n</details>\n\n")
		m.add("all skips", b.String())
	}

	if len(s.SlowestPackages) > 0 || len(s.SlowestTests) > 0 {
		var b strings.Builder
		b.WriteString("<details><summary>Slowest packages and tests</summary>\n\n| Package | Elapsed |\n|---|---:|\n")
		for _, p := range s.SlowestPackages {
			fmt.Fprintf(&b, "| %s | %s |\n", codeCell(p.Package), secs(p.Elapsed))
		}
		b.WriteString("\n| Package | Top-level test | Elapsed |\n|---|---|---:|\n")
		for _, p := range s.SlowestTests {
			fmt.Fprintf(&b, "| %s | %s | %s |\n", codeCell(p.Package), codeCell(p.Test), secs(p.Elapsed))
		}
		b.WriteString("\n</details>\n\n")
		m.add("slowest", b.String())
	}

	if len(s.NoTestsRun) > 0 {
		m.add("packages without tests run", fmt.Sprintf("<details><summary>Packages that passed without running a test (%d)</summary>\n\n%s\n</details>\n\n", len(s.NoTestsRun), codeBlock(s.NoTestsRun)))
	}
	if t.NonJSONLines > 0 {
		m.add("non-JSON output", fmt.Sprintf("<details><summary>Non-JSON output (first %d of %d lines)</summary>\n\n%s\n</details>\n\n", len(s.NonJSON), t.NonJSONLines, codeBlock(s.NonJSON)))
	}
	out := m.b.String()
	if len(m.dropped) > 0 {
		out += fmt.Sprintf("_Left out to stay under the step summary size limit: %s. The job log has the full report._\n\n", strings.Join(m.dropped, ", "))
	}
	return out
}

// mdText escapes text for a Markdown paragraph or table cell.
// Markdown punctuation is escaped first, so that the "#" of an HTML entity such as "&#34;" is
// never escaped (GitHub would print "&#34;" instead of a quote); quotes are left as they are.
// A carriage return, a line end in Markdown, is shown as \r.
func mdText(s string) string {
	for _, c := range []string{`\`, "|", "*", "_", "`", "[", "]", "#"} {
		s = strings.ReplaceAll(s, c, `\`+c)
	}
	s = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\r", `\r`).Replace(s)
	return strings.ReplaceAll(s, "\n", "<br>")
}

// reasonCell renders a skip message for a table cell: its first line, clipped, and how many
// lines follow.
func reasonCell(r Reason) string {
	first, rest, multi := strings.Cut(r.Text, "\n")
	cell := codeCell(shortReason(first, r.Truncated && !multi))
	if multi {
		more := plural(strings.Count(rest, "\n")+1, "line")
		if r.Truncated {
			more += ", truncated"
		}
		cell += " (+" + more + ")"
	}
	return cell
}

// codeCell renders s as a code span that is safe in a table cell. Only the first line of a
// multi-line value is shown, followed by the number of lines left out; a carriage return, which
// would end the table row, is shown as \r.
func codeCell(s string) string {
	if s == "" {
		return ""
	}
	first, rest, multi := strings.Cut(s, "\n")
	first = escapeCR(first)
	suffix := ""
	if multi {
		suffix = fmt.Sprintf(" (+%s)", plural(strings.Count(rest, "\n")+1, "line"))
	}
	fence := "`"
	for strings.Contains(first, fence) {
		fence += "`"
	}
	pad := ""
	if strings.HasPrefix(first, "`") || strings.HasSuffix(first, "`") {
		pad = " "
	}
	return fence + pad + strings.ReplaceAll(first, "|", `\|`) + pad + fence + suffix
}

// codeBlock renders lines as a fenced block whose fence no line can close.
func codeBlock(lines []string) string {
	fence := "```"
	for _, l := range lines {
		for strings.Contains(l, fence) {
			fence += "`"
		}
	}
	return fence + "text\n" + strings.Join(lines, "\n") + "\n" + fence + "\n"
}
