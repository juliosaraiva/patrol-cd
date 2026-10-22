package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunExitStatus(t *testing.T) {
	t.Parallel()
	const (
		syn     = "testdata/synthetic.json"
		synSkip = "testdata/synthetic-skips.txt"
		real    = "testdata/real-darwin.json"
		realSk  = "testdata/real-skips.txt"
	)
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	empty := write("empty.json", "")
	noEvents := write("noevents.json", "go: cannot find main module\n")
	badRules := write("bad.txt", "pkg\tTestX\treason\n") // 3 fields: an allowlist needs 4
	badMust := write("badmust.txt", "pkg\tTestX\n")      // 2 fields: a must-run list needs 3
	allowLFS := write("lfs.txt", "github\\.com/juliosaraiva/patrol-cd/internal/git\tTestLFS\tgit-lfs not installed\tdeveloper machine\n")
	allowBoth := write("both.txt", "github\\.com/juliosaraiva/patrol-cd/internal/git\tTestLFS\tgit-lfs not installed\tdeveloper machine\n"+
		"github\\.com/juliosaraiva/patrol-cd/pkg/scm/.*\tTestConformance/.*\t.*\tconformance\n"+
		"github\\.com/juliosaraiva/patrol-cd/internal/health\tTestNothing\t.*\tstale\n")
	mustAbsent := write("absent.txt", "github.com/juliosaraiva/patrol-cd/test/deploy\tTest.*\tnot in the capture\n")
	allowAll := write("all.txt", ".*\t.*\t.*\tevery skip\n")
	unbalanced := write("unbalanced.txt", "example\\.com/none)|(.*\tTest.*\t.*\tlooks like a rule for one package\n")
	pkgList := write("pkgs.txt", "example.com/fixture/alpha\nexample.com/fixture/beta\n")
	mustAlpha := write("mustalpha.txt", "example.com/fixture/alpha\tTestFail\twhy\n")
	mustGone := write("mustgone.txt", "example.com/fixture/alpha\tTestFail\twhy\nexample.com/fixture/gone\tTestX\trenamed\n")
	allowAlpha := write("allowalpha.txt", "example\\.com/fixture/alpha\tTestA\t.*\twhy\n")
	allowGone := write("allowgone.txt", "example\\.com/fixture/(alpha|beta)\tTestA\t.*\twhy\nexample\\.com/fixture/gone\tTestA\t.*\twhy\n")
	stderrFile := write("go.stderr", "go: updates to go.mod needed\n")

	tests := []struct {
		name string
		args []string
		want int
	}{
		{name: "help", args: []string{"-h"}, want: 0},
		{name: "unknown flag", args: []string{"-nope"}, want: 2},
		{name: "two inputs", args: []string{"-skips", "", "-must-run", "", syn, syn}, want: 2},
		{name: "missing input", args: []string{"-skips", "", "-must-run", "", filepath.Join(dir, "missing.json")}, want: 2},
		{name: "empty input", args: []string{"-skips", "", "-must-run", "", empty}, want: 2},
		{name: "input without events", args: []string{"-skips", "", "-must-run", "", noEvents}, want: 2},
		{name: "missing allowlist", args: []string{"-skips", filepath.Join(dir, "missing.txt"), "-must-run", "", syn}, want: 2},
		{name: "malformed allowlist", args: []string{"-skips", badRules, "-must-run", "", syn}, want: 2},
		{name: "malformed must-run", args: []string{"-skips", "", "-must-run", badMust, syn}, want: 2},
		{name: "forbidden skips", args: []string{"-skips", synSkip, "-must-run", "", syn}, want: 1},
		{name: "must-run violations", args: []string{"-skips", "", "-must-run", "testdata/real-must-run.txt", real}, want: 1},
		{name: "some skips still forbidden", args: []string{"-skips", allowLFS, "-must-run", "", real}, want: 1},
		{name: "failures alone fail the run", args: []string{"-skips", allowAll, "-must-run", "", syn}, want: 1},
		{name: "unbalanced parenthesis cannot allow every skip", args: []string{"-skips", unbalanced, "-must-run", "", real}, want: 2},
		{name: "stale line is only a warning", args: []string{"-skips", allowBoth, "-must-run", "", real}, want: 0},
		{name: "stale line fails with -strict-allowlist", args: []string{"-strict-allowlist", "-skips", allowBoth, "-must-run", "", real}, want: 1},
		{name: "absent package is fine", args: []string{"-skips", allowBoth, "-must-run", mustAbsent, real}, want: 0},
		{name: "absent package fails with -require-packages", args: []string{"-require-packages", "-skips", allowBoth, "-must-run", mustAbsent, real}, want: 1},
		{name: "must-run /dev/null", args: []string{"-skips", allowBoth, "-must-run", os.DevNull, real}, want: 0},
		{name: "-stderr file shown, not parsed", args: []string{"-stderr", stderrFile, "-skips", allowBoth, "-must-run", "", real}, want: 0},
		{name: "missing -stderr file", args: []string{"-stderr", filepath.Join(dir, "missing.stderr"), "-skips", "", "-must-run", "", syn}, want: 2},
		{name: "no events with -stderr", args: []string{"-stderr", stderrFile, "-skips", "", "-must-run", "", empty}, want: 2},
		{name: "check-rules: every package exists", args: []string{"-check-rules", "-skips", allowAlpha, "-must-run", mustAlpha, pkgList}, want: 0},
		{name: "check-rules: must-run package gone", args: []string{"-check-rules", "-skips", allowAlpha, "-must-run", mustGone, pkgList}, want: 1},
		{name: "check-rules: allowlist package regexp matches nothing", args: []string{"-check-rules", "-skips", allowGone, "-must-run", mustAlpha, pkgList}, want: 1},
		{name: "check-rules: empty package list", args: []string{"-check-rules", "-skips", "", "-must-run", "", empty}, want: 2},
		{name: "check-rules: missing package list", args: []string{"-check-rules", "-skips", "", "-must-run", "", filepath.Join(dir, "missing.txt")}, want: 2},
		{name: "check-rules: malformed rules", args: []string{"-check-rules", "-skips", badRules, "-must-run", "", pkgList}, want: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			if got := run(tt.args, strings.NewReader(""), &stdout, &stderr); got != tt.want {
				t.Fatalf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", got, tt.want, stdout.String(), stderr.String())
			}
		})
	}
}

func TestRunStdinAndOutputs(t *testing.T) {
	t.Parallel()
	in, err := os.ReadFile("testdata/synthetic.json")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	md := filepath.Join(dir, "summary.md")
	js := filepath.Join(dir, "report.json")
	args := []string{
		"-skips", "testdata/synthetic-skips.txt", "-must-run", "testdata/synthetic-must-run.txt",
		"-summary", md, "-json-out", js, "-title", "Unit tests (shard 1)", "-",
	}
	var firstJSON []byte
	for i := 0; i < 2; i++ {
		var stdout, stderr bytes.Buffer
		if got := run(args, bytes.NewReader(in), &stdout, &stderr); got != 1 {
			t.Fatalf("exit %d, want 1; stderr %s", got, stderr.String())
		}
		text := stdout.String()
		for _, want := range []string{
			"Unit tests (shard 1)\n====================\n",
			"Result:   FAILED: 3 failed packages, 1 failed build, 1 failed test, 2 tests did not finish; 2 forbidden skipped tests, 7 must-run violations\n",
			"example\\.com/fixture/alpha\tTestToolSkip\tgit not found in PATH\tTODO: why this skip is legitimate on CI\n",
			"FORBIDDEN  example.com/fixture/beta  TestMustSkip  \"gpg not available\" at verify_test.go:63\n",
			"allowed    example.com/fixture/alpha  TestAllSubSkip (3 skipped subtests)  \"capability \\\"x\\\" not declared\" at conformance.go:154 [testdata/synthetic-skips.txt:3]\n",
			"testdata/synthetic-skips.txt:6: example\\.com/fixture/beta\tTestNothing",
			"--- DID NOT FINISH example.com/fixture/hang TestHang/inner\n    | panic: test timed out after 1s\n",
			"Non-JSON output (first 1 of 1 lines)\n  | go: downloading example.com/dep v1.2.3\n",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("text report lacks %q", want)
			}
		}
		b, err := os.ReadFile(js)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			firstJSON = b
		} else if !bytes.Equal(b, firstJSON) {
			t.Error("-json-out differs between two runs over the same input")
		}
	}

	var sum Summary
	if err := json.Unmarshal(firstJSON, &sum); err != nil {
		t.Fatal(err)
	}
	if sum.Title != "Unit tests (shard 1)" || sum.Result != resultFailed || sum.TestsResult != resultFailed || sum.PolicyResult != resultViolations || sum.Totals.SkipsForbidden != 2 {
		t.Errorf("json summary %q %q %q %+v", sum.Result, sum.TestsResult, sum.PolicyResult, sum.Totals)
	}
	if bytes.Contains(firstJSON, []byte("2026-10-21")) {
		t.Error("-json-out contains a timestamp")
	}

	b, err := os.ReadFile(md)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), "## Unit tests (shard 1)\n"); n != 2 {
		t.Errorf("-summary holds %d reports after two runs, want 2 (it appends)", n)
	}
	for _, want := range []string{
		"**Result: FAILED: 3 failed packages, 1 failed build, 1 failed test, 2 tests did not finish; 2 forbidden skipped tests, 7 must-run violations**",
		"| `example.com/fixture/alpha` | `TestToolSkip` | `git not found in PATH` | `helpers_test.go:46` | 1 |",
		"<details><summary>DID NOT FINISH example.com/fixture/hang TestHang/inner</summary>",
		"| yes | `example.com/fixture/alpha` | `TestParentSkip (1 skipped subtest)` | `parent reason` (+1 line) | `alpha_test.go:41` | 5 |",
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("markdown lacks %q", want)
		}
	}
}

func TestMarkdownHelpers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, got, want string
	}{
		{"code cell", codeCell("TestA"), "`TestA`"},
		{"code cell pipe", codeCell("a|b"), "`a\\|b`"},
		{"code cell backticks", codeCell("use `x`"), "`` use `x` ``"},
		{"code cell multi-line", codeCell("one\ntwo\nthree"), "`one` (+2 lines)"},
		{"code cell empty", codeCell(""), ""},
		{"text", mdText("a_b *c* <d> | e"), "a\\_b \\*c\\* &lt;d&gt; \\| e"},
		{"text with quotes, # and &", mdText(`go test's "x" #1 & a\b`), `go test's "x" \#1 &amp; a\\b`},
		{"code block", codeBlock([]string{"x"}), "```text\nx\n```\n"},
		{"code block with fence inside", codeBlock([]string{"```go", "x"}), "````text\n```go\nx\n````\n"},
		{"reason cell", reasonCell(Reason{Text: "one"}), "`one`"},
		{"reason cell multi-line", reasonCell(Reason{Text: "one\ntwo\nthree"}), "`one` (+2 lines)"},
		{"reason cell truncated multi-line", reasonCell(Reason{Text: "one\ntwo", Truncated: true}), "`one` (+1 line, truncated)"},
		{"reason cell truncated line", reasonCell(Reason{Text: "one", Truncated: true}), "`one ... (truncated)`"},
		{"reason cell long line", reasonCell(Reason{Text: strings.Repeat("a", maxLineBytes+1)}), "`" + strings.Repeat("a", maxLineBytes) + " ... (truncated)`"},
		{"code cell carriage return", codeCell("a\r## x\nb"), "`a\\r## x` (+1 line)"},
		{"reason cell carriage return", reasonCell(Reason{Text: "a\r::error::y"}), "`a\\r::error::y`"},
		{"text carriage return", mdText("a\r## x"), `a\r\#\# x`},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, tt.got, tt.want)
		}
	}
}

// Carriage returns in test output and skip messages reach neither the job log nor the step
// summary raw: the runner ends a log line at a lone CR (a "::error" after it would be a workflow
// command), and Markdown ends a table row there. The allowlist still matches the raw message,
// and the suggested line allows it.
func TestCarriageReturns(t *testing.T) {
	t.Parallel()
	md := filepath.Join(t.TempDir(), "summary.md")
	var stdout, stderr bytes.Buffer
	if got := run([]string{"-skips", "testdata/real-edge-skips.txt", "-must-run", "", "-summary", md, "testdata/real-edge.json"}, strings.NewReader(""), &stdout, &stderr); got != 1 {
		t.Fatalf("exit %d; stderr %s", got, stderr.String())
	}
	text := stdout.String()
	for _, l := range strings.FieldsFunc(text, func(r rune) bool { return r == '\r' || r == '\n' }) {
		if strings.HasPrefix(strings.TrimSpace(l), "::") {
			t.Errorf("a line of the job log is a workflow command: %q", l)
		}
	}
	for _, want := range []string{
		`    | tool output x\r::error file=README.md,line=1::forged` + "\n",
		`    |     a_test.go:12: progress 10%\r::warning title=Forged::forged` + "\n",
		`  example.com/edge/cr  TestCRSkip  "cannot run: a\r::error::y\r## Result: OK" at a_test.go:17` + "\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text report lacks %q", want)
		}
	}
	b, err := os.ReadFile(md)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.ContainsRune(b, '\r') {
		t.Error("the markdown holds a carriage return")
	}
	if want := "| `example.com/edge/cr` | `TestCRSkip` | `cannot run: a\\r::error::y\\r## Result: OK` | `a_test.go:17` | 1 |\n"; !strings.Contains(string(b), want) {
		t.Errorf("markdown lacks %q", want)
	}

	var sug string
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(l, "example\\.com/edge/cr\tTestCRSkip\t") {
			sug = l
		}
	}
	rules, err := parseAllowlist("suggestion", strings.NewReader(sug))
	if err != nil || len(rules) != 1 || !rules[0].allows("example.com/edge/cr", "TestCRSkip", "cannot run: a\r::error::y\r## Result: OK") {
		t.Fatalf("the suggested line %q does not allow the skip (%v)", sug, err)
	}
}

// A lost skip message is reported with its hint and without a suggested allowlist line.
func TestLostMessageReport(t *testing.T) {
	t.Parallel()
	s := &Summary{
		Title: "t", Result: resultFailed, TestsResult: resultOK, PolicyResult: resultViolations, Totals: Totals{SkipsForbidden: 1},
		Skips: []Skip{{Package: "p", Test: "TestA", Reasons: []Reason{{Text: lostText, Truncated: true, Tests: 1, Hint: lostHint}}}},
	}
	text := renderText(s, "skips.txt", "must.txt")
	if !strings.Contains(text, "      hint: "+lostHint+"\n") || strings.Contains(text, "TODO") || strings.Contains(text, "add a line") {
		t.Errorf("text report:\n%s", text)
	}
	md := renderMarkdown(s, "skips.txt", "must.txt")
	if !strings.Contains(md, "- `p` `TestA`: "+mdText(lostHint)+"\n\n") || strings.Contains(md, "```") {
		t.Errorf("markdown:\n%s", md)
	}
}

// The Markdown report stays under GitHub's 1 MiB step summary limit however much failed: every
// list at its worst at once, and a real run of the tool over a stream with 3,000 failing tests.
func TestMarkdownBudget(t *testing.T) {
	t.Parallel()
	line := strings.Repeat("x", maxLineBytes)
	lines := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = line
		}
		return out
	}

	t.Run("every section at its worst", func(t *testing.T) {
		t.Parallel()
		s := &Summary{Title: line, Result: resultFailed, TestsResult: resultFailed, PolicyResult: resultViolations, Stderr: lines(stderrTail), StderrLines: 1 << 20}
		s.Totals.SkipsForbidden, s.Totals.MustRunViolations, s.Totals.TestsFailed = 5000, 3000, 400
		for i := 0; i < 400; i++ {
			s.Failures = append(s.Failures, Failure{Package: line, Test: line, Kind: failTest, Output: lines(captureHead + captureErrors + captureTail)})
		}
		for i := 0; i < 5000; i++ {
			s.Skips = append(s.Skips, Skip{Package: line, Test: line, Reasons: []Reason{{Text: line, Location: line, Hint: line}}, Suggestions: []string{line}})
			s.StaleAllowlist = append(s.StaleAllowlist, RuleRef{Line: i, Text: line})
			s.NoTestsRun = append(s.NoTestsRun, line)
		}
		for i := 0; i < 3000; i++ {
			s.MustRunViolations = append(s.MustRunViolations, Violation{Package: line, Test: line, Comment: line, Problem: line, Tests: []TestStatus{{Test: line, Status: statusSkip}}})
		}
		for i := 0; i < 2*slowestN; i++ {
			s.SlowestPackages = append(s.SlowestPackages, Timing{Package: line})
			s.SlowestTests = append(s.SlowestTests, Timing{Package: line, Test: line})
		}
		s.NonJSON, s.Totals.NonJSONLines = lines(maxNonJSON), 1<<20
		md := renderMarkdown(s, line, line)
		if len(md) > stepSummaryLimit || len(md) > markdownBudget+1024 {
			t.Fatalf("markdown is %d bytes", len(md))
		}
		if !strings.HasPrefix(md, "## "+line+"\n\n**Result: FAILED: 400 failed tests; 5000 forbidden skipped tests, 3000 must-run violations**") {
			t.Fatalf("markdown starts with %q", md[:200])
		}
		if !strings.Contains(md, "_Left out to stay under the step summary size limit: ") {
			t.Fatal("no note about the sections left out")
		}
	})

	t.Run("a run with 3000 failing tests", func(t *testing.T) {
		t.Parallel()
		var b strings.Builder
		out := strings.Repeat("y", 900)
		b.WriteString(`{"Action":"start","Package":"p"}` + "\n")
		for i := 0; i < 3000; i++ {
			fmt.Fprintf(&b, `{"Action":"run","Package":"p","Test":"Test%04d"}`+"\n", i)
			for j := 0; j < 2; j++ {
				fmt.Fprintf(&b, `{"Action":"output","Package":"p","Test":"Test%04d","Output":"    a_test.go:%d: %s\n","OutputType":"error"}`+"\n", i, j, out)
			}
			fmt.Fprintf(&b, `{"Action":"fail","Package":"p","Test":"Test%04d","Elapsed":0.1}`+"\n", i)
		}
		b.WriteString(`{"Action":"fail","Package":"p","Elapsed":300}` + "\n")
		md := filepath.Join(t.TempDir(), "summary.md")
		var stdout, stderr bytes.Buffer
		if got := run([]string{"-skips", "", "-must-run", "", "-summary", md, "-"}, strings.NewReader(b.String()), &stdout, &stderr); got != 1 {
			t.Fatalf("exit %d; stderr %s", got, stderr.String())
		}
		fi, err := os.Stat(md)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Size() > stepSummaryLimit || fi.Size() < markdownBudget/2 {
			t.Fatalf("summary is %d bytes", fi.Size())
		}
		if !strings.Contains(stdout.String(), "Result:   FAILED: 1 failed package, 3000 failed tests; skip policy OK\n") {
			t.Fatalf("text report starts with %q", stdout.String()[:200])
		}
	})
}

// The headline and -json-out carry the test outcome next to the skip policy outcome; the exit
// status follows the headline: 1 when either outcome failed, 2 for "no events".
func TestRunReportsOutcome(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	allowAll := write("all.txt", ".*\t.*\t.*\tevery skip\n")
	allowStale := write("stale.txt", ".*\t.*\t.*\tevery skip\ngithub\\.com/juliosaraiva/patrol-cd/internal/health\tTestNothing\t.*\tstale\n")
	stderrFile := write("go.stderr", "go: updates to go.mod needed; to update it:\n\tgo mod tidy\n")
	empty := write("empty.json", "")
	passing := write("pass.json", `{"Action":"start","Package":"p"}
{"Action":"run","Package":"p","Test":"TestA"}
{"Action":"pass","Package":"p","Test":"TestA","Elapsed":0.1}
{"Action":"pass","Package":"p","Elapsed":0.2}
`)
	tests := []struct {
		name                     string
		args                     []string
		want                     int
		headline                 string
		result, tests, policy    string
		textHas, mdHas, mdHasNot []string
	}{
		{
			name: "failing tests, skip policy OK", args: []string{"-skips", allowAll, "-must-run", "", "testdata/synthetic.json"}, want: 1,
			headline: "FAILED: 3 failed packages, 1 failed build, 1 failed test, 2 tests did not finish; skip policy OK",
			result:   resultFailed, tests: resultFailed, policy: resultOK,
			mdHasNot: []string{"go test stderr"},
		},
		{
			// A TestMain that drops the code of m.Run: the package passed and go test exited 0,
			// but a test failed.
			name: "failed test in a package that passed", args: []string{"-skips", "", "-must-run", "", "testdata/real-mainexit0.json"}, want: 1,
			headline: "FAILED: 1 failed test; skip policy OK",
			result:   resultFailed, tests: resultFailed, policy: resultOK,
			textHas: []string{"--- FAIL example.com/edge/mainexit0 TestFails (0.00s)\n    |     a_test.go:15: failed, but the package passes\n"},
		},
		{
			name: "failing tests with go test's stderr", args: []string{"-stderr", stderrFile, "-skips", allowAll, "-must-run", "", "testdata/synthetic.json"}, want: 1,
			headline: "FAILED: 3 failed packages, 1 failed build, 1 failed test, 2 tests did not finish; skip policy OK",
			result:   resultFailed, tests: resultFailed, policy: resultOK,
			textHas: []string{"\ngo test stderr (" + stderrFile + ", last 2 of 2 lines)\n  | go: updates to go.mod needed; to update it:\n  | \tgo mod tidy\n"},
			mdHas:   []string{"<details open><summary>go test stderr (last 2 of 2 lines)</summary>\n\n```text\ngo: updates to go.mod needed; to update it:\n\tgo mod tidy\n```"},
		},
		{
			name: "skip policy only", args: []string{"-skips", "", "-must-run", "", "testdata/real-darwin.json"}, want: 1,
			headline: "FAILED: the tests passed, but 26 forbidden skipped tests",
			result:   resultFailed, tests: resultOK, policy: resultViolations,
		},
		{
			name: "stale line with -strict-allowlist", args: []string{"-strict-allowlist", "-skips", allowStale, "-must-run", "", "testdata/real-darwin.json"}, want: 1,
			headline: "FAILED: the tests passed, but 1 stale allowlist line (-strict-allowlist)",
			result:   resultFailed, tests: resultOK, policy: resultViolations,
		},
		{
			name: "failing tests and the skip policy", args: []string{"-skips", "testdata/real-probe-skips.txt", "-must-run", "testdata/real-probe-must-run.txt", "testdata/real-probe.json"}, want: 1,
			headline: "FAILED: 1 failed package, 1 failed test; 4 forbidden skipped tests, 2 must-run violations",
			result:   resultFailed, tests: resultFailed, policy: resultViolations,
			textHas: []string{"      hint: the earlier message \"allowed reason\" at a_test.go:49 matches allowlist line 3; if it is the t.Skip message, a log written after t.Skip"},
			mdHas:   []string{"- `example.com/probe/a` `TestCleanupLogAfterSkip`: the earlier message \"allowed reason\" at a\\_test.go:49 matches allowlist line 3; if it is the t.Skip message, "},
		},
		{
			name: "no events", args: []string{"-stderr", stderrFile, "-skips", "", "-must-run", "", empty}, want: 2,
			headline: "NO TEST EVENTS: the input holds no go test -json events; go test's stderr says why",
			result:   resultNoEvents, tests: resultNoEvents, policy: resultOK,
			mdHas: []string{"<details open><summary>go test stderr (last 2 of 2 lines)</summary>"},
		},
		{
			name: "no events, must-run package required", args: []string{"-require-packages", "-skips", "", "-must-run", "testdata/real-must-run.txt", empty}, want: 2,
			headline: "NO TEST EVENTS: the input holds no go test -json events; go test's stderr says why; 4 must-run violations",
			result:   resultNoEvents, tests: resultNoEvents, policy: resultViolations,
		},
		{
			name: "everything passed", args: []string{"-stderr", stderrFile, "-skips", "", "-must-run", "", passing}, want: 0,
			headline: "OK",
			result:   resultOK, tests: resultOK, policy: resultOK,
			mdHas: []string{"<details><summary>go test stderr (last 2 of 2 lines)</summary>"},
		},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			md := filepath.Join(dir, fmt.Sprintf("s%d.md", i))
			js := filepath.Join(dir, fmt.Sprintf("r%d.json", i))
			var stdout, stderr bytes.Buffer
			args := append([]string{"-summary", md, "-json-out", js}, tt.args...)
			if got := run(args, strings.NewReader(""), &stdout, &stderr); got != tt.want {
				t.Fatalf("exit %d, want %d\n%s%s", got, tt.want, stdout.String(), stderr.String())
			}
			if want := "Result:   " + tt.headline + "\n"; !strings.Contains(stdout.String(), want) {
				t.Errorf("text report lacks %q:\n%s", want, stdout.String())
			}
			for _, want := range tt.textHas {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("text report lacks %q", want)
				}
			}
			mdOut, err := os.ReadFile(md)
			if err != nil {
				t.Fatal(err)
			}
			if want := "\n\n**Result: " + mdText(tt.headline) + "**\n\n"; !strings.Contains(string(mdOut), want) {
				t.Errorf("markdown lacks %q:\n%s", want, mdOut)
			}
			for _, want := range tt.mdHas {
				if !strings.Contains(string(mdOut), want) {
					t.Errorf("markdown lacks %q", want)
				}
			}
			for _, not := range tt.mdHasNot {
				if strings.Contains(string(mdOut), not) {
					t.Errorf("markdown has %q", not)
				}
			}
			b, err := os.ReadFile(js)
			if err != nil {
				t.Fatal(err)
			}
			var sum Summary
			if err := json.Unmarshal(b, &sum); err != nil {
				t.Fatal(err)
			}
			if sum.Result != tt.result || sum.TestsResult != tt.tests || sum.PolicyResult != tt.policy {
				t.Errorf("json result %q, tests %q, policy %q; want %q, %q, %q", sum.Result, sum.TestsResult, sum.PolicyResult, tt.result, tt.tests, tt.policy)
			}
		})
	}
}

// Every count that makes a run fail shows up in the outcome and in the headline.
func TestTestsResult(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		set      func(*Totals)
		tests    string
		headline string
	}{
		{name: "nothing failed", set: func(*Totals) {}, tests: resultOK, headline: "OK"},
		{name: "no events", set: func(t *Totals) { t.Events = 0 }, tests: resultNoEvents, headline: "NO TEST EVENTS: the input holds no go test -json events; go test's stderr says why"},
		{name: "failed package", set: func(t *Totals) { t.PackagesFailed = 2 }, tests: resultFailed, headline: "FAILED: 2 failed packages; skip policy OK"},
		{name: "unfinished package", set: func(t *Totals) { t.PackagesIncomplete = 1 }, tests: resultFailed, headline: "FAILED: 1 package did not finish; skip policy OK"},
		{name: "failed build", set: func(t *Totals) { t.BuildFailures = 1 }, tests: resultFailed, headline: "FAILED: 1 failed build; skip policy OK"},
		{name: "failed test", set: func(t *Totals) { t.TestsFailed = 3 }, tests: resultFailed, headline: "FAILED: 3 failed tests; skip policy OK"},
		{name: "unfinished test", set: func(t *Totals) { t.TestsIncomplete = 40 }, tests: resultFailed, headline: "FAILED: 40 tests did not finish; skip policy OK"},
		{name: "forbidden skip", set: func(t *Totals) { t.SkipsForbidden = 1 }, tests: resultOK, headline: "FAILED: the tests passed, but 1 forbidden skipped test"},
		{name: "stale line without -strict-allowlist", set: func(t *Totals) {}, tests: resultOK, headline: "OK"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newCollector(nil, nil, false)
			c.sum.Totals.Events = 1
			tt.set(&c.sum.Totals)
			s := c.summary("t", false)
			if s.TestsResult != tt.tests || resultLine(s) != tt.headline {
				t.Fatalf("tests %q, headline %q; want %q, %q", s.TestsResult, resultLine(s), tt.tests, tt.headline)
			}
		})
	}
}

// -check-rules names each rule that refers to a package the module does not have.
func TestCheckRulesOutput(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	pkgs := "example.com/m/a\nexample.com/m/b\n"
	must := write("must.txt", "# c\nexample.com/m/a\tTestA\twhy\nexample.com/m/gone\tTestB\twhy\nexample.com/m/b\tTestC\twhy\n")
	allow := write("allow.txt", "example\\.com/m/[ab]\tTestA\t.*\twhy\nexample\\.com/m/gone\tTestA\t.*\twhy\nexample\\.com/m/a.+\tTestA\t.*\twhy\n")
	tests := []struct {
		name  string
		args  []string
		stdin string
		want  int
		out   string
	}{
		{
			name: "problems", args: []string{"-check-rules", "-skips", allow, "-must-run", must, "-"}, stdin: pkgs, want: 1,
			out: must + ":3: package example.com/m/gone does not exist (renamed or moved?)\n" +
				allow + ":2: package regexp example\\.com/m/gone matches no package\n" +
				allow + ":3: package regexp example\\.com/m/a.+ matches no package\n",
		},
		{
			name: "no problems", args: []string{"-check-rules", "-skips", "", "-must-run", "", "-"}, stdin: pkgs, want: 0,
			out: "rules OK: 0 must-run rules and 0 allowlist lines name existing packages (2 packages)\n",
		},
		{
			name: "go list ./... in the current directory", args: []string{"-check-rules", "-skips", "", "-must-run", ""}, want: 0,
			out: "rules OK: 0 must-run rules and 0 allowlist lines name existing packages (1 package)\n",
		},
		{
			name: "go list ./... and the real must-run fixture", args: []string{"-check-rules", "-skips", "", "-must-run", "testdata/real-must-run.txt"}, want: 1,
			out: "testdata/real-must-run.txt:2: package github.com/juliosaraiva/patrol-cd/internal/git does not exist (renamed or moved?)\n" +
				"testdata/real-must-run.txt:3: package github.com/juliosaraiva/patrol-cd/internal/git does not exist (renamed or moved?)\n" +
				"testdata/real-must-run.txt:4: package github.com/juliosaraiva/patrol-cd/internal/health does not exist (renamed or moved?)\n" +
				"testdata/real-must-run.txt:5: package github.com/juliosaraiva/patrol-cd/test/deploy does not exist (renamed or moved?)\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			if got := run(tt.args, strings.NewReader(tt.stdin), &stdout, &stderr); got != tt.want {
				t.Fatalf("exit %d, want %d\n%s%s", got, tt.want, stdout.String(), stderr.String())
			}
			if stdout.String() != tt.out {
				t.Fatalf("output\n%s\nwant\n%s", stdout.String(), tt.out)
			}
		})
	}
}

func TestReadTail(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	numbered := func(n int) string {
		var b strings.Builder
		for i := 1; i <= n; i++ {
			fmt.Fprintf(&b, "line %d\n", i)
		}
		return b.String()
	}
	huge := strings.Repeat("z", 200<<10) // longer than the reader's buffer
	tests := []struct {
		name      string
		content   string
		wantFirst string
		wantLast  string
		wantN     int
		wantTotal int
	}{
		{name: "empty", content: ""},
		{name: "no final newline", content: "a\nb\nc", wantFirst: "a", wantLast: "c", wantN: 3, wantTotal: 3},
		{name: "blank lines count", content: "a\n\n\n", wantFirst: "a", wantLast: "", wantN: 3, wantTotal: 3},
		{name: "CRLF", content: "a\r\nb\r\n", wantFirst: "a", wantLast: "b", wantN: 2, wantTotal: 2},
		{name: "exactly the tail", content: numbered(stderrTail), wantFirst: "line 1", wantLast: fmt.Sprintf("line %d", stderrTail), wantN: stderrTail, wantTotal: stderrTail},
		{name: "longer than the tail", content: numbered(1010), wantFirst: "line 971", wantLast: "line 1010", wantN: stderrTail, wantTotal: 1010},
		{
			name: "a huge line is clipped", content: "first\n" + huge + "\nlast\n", wantN: 3, wantTotal: 3,
			wantFirst: "first", wantLast: "last",
		},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := filepath.Join(dir, fmt.Sprintf("f%d", i))
			if err := os.WriteFile(p, []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}
			got, total, err := readTail(p, stderrTail)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != tt.wantN || total != tt.wantTotal {
				t.Fatalf("got %d lines of %d, want %d of %d: %q", len(got), total, tt.wantN, tt.wantTotal, got)
			}
			if tt.wantN > 0 && (got[0] != tt.wantFirst || got[len(got)-1] != tt.wantLast) {
				t.Fatalf("first %q, last %q", got[0], got[len(got)-1])
			}
			if strings.Contains(tt.content, huge) {
				want := strings.Repeat("z", maxLineBytes) + fmt.Sprintf(" ... (%d more bytes)", len(huge)-maxLineBytes)
				if got[1] != want {
					t.Fatalf("huge line clipped to %d bytes ending %q", len(got[1]), got[1][len(got[1])-30:])
				}
			}
		})
	}
	if _, _, err := readTail(filepath.Join(dir, "missing"), stderrTail); err == nil {
		t.Fatal("no error for a missing file")
	}
}

// A line longer than the reader's buffer is kept up to the bound; the rest is counted.
func TestReadBoundedLine(t *testing.T) {
	t.Parallel()
	in := strings.Repeat("a", 100<<10) + "\nshort\nlast"
	br := bufio.NewReaderSize(strings.NewReader(in), 16)
	want := []struct {
		line    string
		dropped int
		err     error
	}{
		{strings.Repeat("a", 10), 100<<10 - 10, nil},
		{"short", 0, nil},
		{"last", 0, io.EOF},
	}
	for _, w := range want {
		line, dropped, err := readBoundedLine(br, 10)
		if line != w.line || dropped != w.dropped || !errors.Is(err, w.err) {
			t.Fatalf("got (%.20q, %d, %v), want (%.20q, %d, %v)", line, dropped, err, w.line, w.dropped, w.err)
		}
	}
}
