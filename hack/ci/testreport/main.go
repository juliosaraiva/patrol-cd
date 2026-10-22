// Command testreport reads the event stream of `go test -json` and reports what the run did:
// package and test totals, the slowest packages and tests, every failure with its captured
// output, and every skipped test with the reason its t.Skip call gave. Tests in this repository
// skip, rather than fail, when a tool (git, gpg, sshd, git-lfs, helm, kustomize,
// git-http-backend) or the envtest assets are missing, so a green `go test` can hide large
// untested areas. testreport therefore fails when a skip is not on an allowlist, or when a test
// that must run did not pass.
//
// Usage:
//
//	go test -race -json ./... >unit.json 2>unit.stderr; go run ./hack/ci/testreport [flags] unit.json
//	go test -race -json ./... 2>&1 | go run ./hack/ci/testreport [flags] [-]
//	go run ./hack/ci/testreport -check-rules [packages.txt|-]
//
// Flags come before the input path (stdin when the path is "-" or absent):
//
//	-skips FILE        allowlist of skips (default hack/ci/allowed-skips.txt; "" = empty list)
//	-must-run FILE     tests that must pass (default hack/ci/must-run.txt; "" = none)
//	-summary FILE      append the report as Markdown to FILE (for $GITHUB_STEP_SUMMARY)
//	-json-out FILE     write a machine-readable summary (stable field order, no timestamps)
//	-stderr FILE       show the last 40 lines of FILE (go test's stderr) in the reports
//	-title TEXT        heading of the reports
//	-strict-allowlist  also exit 1 when an allowlist line matched no skip
//	-require-packages  also exit 1 when a must-run package does not occur in the input
//	-check-rules       check the rule files against the module's packages instead (see below)
//
// The plain text report goes to stdout. Its headline (also the first line of the Markdown
// report, and "result" in -json-out) gives the outcome of the whole run: "OK", or "FAILED:"
// with the failed and unfinished packages, builds and tests, then the skip policy outcome
// ("skip policy OK", or the forbidden skips and must-run violations), or "NO TEST EVENTS".
// -json-out also carries the two outcomes apart, as tests_result ("ok", "failed",
// "no-events") and policy_result ("ok", "violations").
//
// Exit status: 0 when the headline is "OK"; 1 when the run failed: a failed or unfinished test,
// package or build, a forbidden skip or a must-run violation. go test's own exit status reports
// test failures too; this one is the backstop for a TestMain that drops the code of m.Run (go
// test then exits 0 although a test failed). 2 on usage errors, unreadable or malformed rule
// files, and input without a single test event (the reports are still written). The -stderr
// file is shown, never parsed: it does not change the outcome.
//
// Input: one JSON event per line as defined by `go doc cmd/test2json` (Actions start, run,
// pause, cont, pass, bench, fail, output, skip) and `go help buildjson` (build-output,
// build-fail). Other lines, such as "go: downloading ..." when stderr is merged into the
// stream, are counted and the first 50 are printed. The stream is read line by line: per-test
// output is kept, bounded, only while a test runs and for tests that fail or skip. A truncated
// stream (a killed run) is reported as far as it goes: packages and tests that did not finish
// are failures of the run, and their must-run tests are violations.
//
// The skip reason of a test is the last "file.go:NN: message" block it printed before its
// "--- SKIP" line, which is the message of its t.Skip/t.Skipf call (without the file:line
// location, reported separately). Output events that test2json split (lines over 1024 bytes)
// or that the test printed without a newline are joined into lines first; a message that
// follows such output on the same line still counts, and a continuation line of a message
// stays part of it even when it looks like a "file.go:NN:" header. A joined line is kept up to
// 8 KiB: when a longer one has no message header in that part, the message may have been cut
// off, so the reason is "(skip message lost: ...)" (fail closed: no allowlist line is
// suggested for it, and the report says why). The reason is matched as up to 20 lines and
// 4 KiB of the message; the reports mark a longer one as truncated. A test that called
// t.SkipNow after logging gets its last log message as the reason; one that logged nothing has
// an empty reason. A log written after t.Skip, by a t.Cleanup, a deferred call or a goroutine,
// is printed before the "--- SKIP" line, so it becomes the reason: when it matches no
// allowlist line the skip fails as forbidden, and the report names an earlier message of the
// test that does match; when it matches a line, the skip is allowed whatever the t.Skip
// message was. Guard such logs with `if !t.Skipped()`. A test whose subtests all skipped (Go
// reports such a parent as passed) is reported once, with the number of skipped subtests and
// their distinct reasons. A package without test files is not a skipped test; it is counted
// separately.
//
// Allowlist format (-skips): one rule per line, four fields separated by tabs,
//
//	PACKAGE_REGEXP<TAB>TEST_REGEXP<TAB>REASON_REGEXP<TAB>comment
//
// Blank lines and lines starting with '#' are ignored. Each regexp must match the whole
// package import path, test name ("TestX/sub" as go test prints it) and skip reason; "." also
// matches a newline. The comment, saying why the skip is legitimate on CI, is mandatory. Every
// skipped test, including each subtest folded into a parent entry, must match a rule; the
// report lists each one that does not, with a line to add if the skip is legitimate. A rule
// that matched no skip, although its package pattern matched a package of the input, is
// reported as stale (a warning, or a failure with -strict-allowlist).
//
// Must-run format (-must-run): one rule per line, three fields separated by tabs,
//
//	PACKAGE<TAB>TEST_REGEXP<TAB>comment
//
// PACKAGE is a full import path. TEST_REGEXP must match whole test names (top-level tests and
// subtests). When PACKAGE occurs in the input, at least one test must match and every matching
// test must have passed: a missing, skipped, failed or unfinished test is a violation, and so
// is a passed test whose subtests all skipped. A package that does not occur in the input (a
// shard that does not run it) is not checked, unless -require-packages is set.
//
// -check-rules reads no test events. It checks the rule files against the packages of the
// module: every package named in -must-run must exist, and every package regexp of -skips must
// match at least one package, so that renaming or moving a package cannot silently disable a
// rule. The package list (one import path per line, as `go list ./...` prints it) is read from
// the input path, or from stdin for "-"; without an input path the tool runs `go list ./...`
// in the current directory. It prints each problem and exits 1 when there is one, 0 when there
// is none, and 2 when the rule files or the package list cannot be read.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

const (
	defaultSkips   = "hack/ci/allowed-skips.txt"
	defaultMustRun = "hack/ci/must-run.txt"
	defaultTitle   = "go test report"
	stderrTail     = 40 // lines of the -stderr file shown in the reports
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run is main without the process: it returns the exit status.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	status, err := report(args, stdin, stdout, stderr)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "testreport:", err)
	}
	return status
}

// report parses the flags, reads the input and writes the reports. It returns the exit status
// and, for status 2, the error to print.
func report(args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	fs := flag.NewFlagSet("testreport", flag.ContinueOnError)
	fs.SetOutput(stderr)
	skipsPath := fs.String("skips", defaultSkips, `allowlist of skips ("" for an empty list)`)
	mustRunPath := fs.String("must-run", defaultMustRun, `tests that must pass ("" for none)`)
	summaryPath := fs.String("summary", "", "append a Markdown report to `FILE`")
	jsonPath := fs.String("json-out", "", "write the machine-readable summary to `FILE`")
	stderrPath := fs.String("stderr", "", "show the last 40 lines of `FILE` (go test's stderr) in the reports")
	title := fs.String("title", defaultTitle, "heading of the reports")
	strict := fs.Bool("strict-allowlist", false, "exit 1 when an allowlist line matched no skip")
	requirePkgs := fs.Bool("require-packages", false, "exit 1 when a must-run package does not occur in the input")
	checkOnly := fs.Bool("check-rules", false, "check the rule files against the module's packages (input: go list ./... output; runs it when absent)")
	fs.Usage = func() {
		_, _ = fmt.Fprintln(fs.Output(), "usage: testreport [flags] [input.json|-]\n       testreport -check-rules [-skips FILE] [-must-run FILE] [packages.txt|-]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, nil
		}
		return 2, nil // the flag package printed the problem
	}
	if fs.NArg() > 1 {
		return 2, fmt.Errorf("one input file at most, got %d (flags go before the input)", fs.NArg())
	}

	allow, err := loadAllowlist(*skipsPath)
	if err != nil {
		return 2, err
	}
	must, err := loadMustRun(*mustRunPath)
	if err != nil {
		return 2, err
	}
	if *checkOnly {
		return checkRulesMode(allow, must, fs.Arg(0), stdin, stdout, stderr)
	}
	var tail []string
	tailTotal := 0
	if *stderrPath != "" {
		if tail, tailTotal, err = readTail(*stderrPath, stderrTail); err != nil {
			return 2, err
		}
	}

	in := stdin
	inputName := "stdin"
	if fs.NArg() == 1 && fs.Arg(0) != "-" {
		inputName = fs.Arg(0)
		f, err := os.Open(inputName)
		if err != nil {
			return 2, err
		}
		defer f.Close() //nolint:errcheck // read-only file
		in = f
	}

	c := newCollector(allow, must, *requirePkgs)
	if err := c.read(in); err != nil {
		return 2, fmt.Errorf("reading %s: %w", inputName, err)
	}
	sum := c.summary(*title, *strict)
	if tailTotal > 0 {
		sum.Stderr, sum.StderrLines, sum.StderrPath = tail, tailTotal, *stderrPath
	}

	skipsName, mustName := allow.path, must.path
	if skipsName == "" {
		skipsName = "(empty allowlist)"
	}
	if mustName == "" {
		mustName = "(no must-run list)"
	}
	if _, err := io.WriteString(stdout, renderText(sum, skipsName, mustName)); err != nil {
		return 2, err
	}
	if *summaryPath != "" {
		if err := appendFile(*summaryPath, renderMarkdown(sum, skipsName, mustName)); err != nil {
			return 2, err
		}
	}
	if *jsonPath != "" {
		if err := writeJSON(*jsonPath, sum); err != nil {
			return 2, err
		}
	}
	if sum.TestsResult == resultNoEvents {
		// Typically go test failed before running anything: its stderr (-stderr, or non-JSON
		// lines when it was merged into the input) says why.
		return 2, fmt.Errorf("%s holds no go test -json events", inputName)
	}
	if sum.Result != resultOK {
		// A test, package or build failed or did not finish, or the skip policy was violated.
		return 1, nil
	}
	return 0, nil
}

// checkRulesMode is -check-rules: it reads the package list from path ("-" for stdin), or runs
// `go list ./...` when path is empty, and reports the rules that name no existing package.
func checkRulesMode(allow *allowlist, must *mustRunList, path string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	var list []byte
	var err error
	switch path {
	case "":
		cmd := exec.Command("go", "list", "./...")
		cmd.Stderr = stderr
		if list, err = cmd.Output(); err != nil {
			return 2, fmt.Errorf("go list ./...: %w", err)
		}
	case "-":
		list, err = io.ReadAll(stdin)
	default:
		list, err = os.ReadFile(path)
	}
	if err != nil {
		return 2, err
	}
	var pkgs []string
	for _, l := range strings.Split(string(list), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			pkgs = append(pkgs, l)
		}
	}
	if len(pkgs) == 0 {
		return 2, errors.New("the package list is empty")
	}
	problems := checkRules(allow, must, pkgs)
	for _, p := range problems {
		if _, err := fmt.Fprintln(stdout, p); err != nil {
			return 2, err
		}
	}
	if len(problems) > 0 {
		return 1, nil
	}
	_, err = fmt.Fprintf(stdout, "rules OK: %s and %s name existing packages (%s)\n",
		plural(len(must.rules), "must-run rule"), plural(len(allow.rules), "allowlist line"), plural(len(pkgs), "package"))
	if err != nil {
		return 2, err
	}
	return 0, nil
}

// readTail returns the last n lines of a file, each clipped to maxLineBytes, and how many lines
// it has. Memory stays bounded however long the file or its lines are.
func readTail(path string, n int) ([]string, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close() //nolint:errcheck // read-only file
	br := bufio.NewReaderSize(f, 1<<16)
	ring := make([]string, 0, n)
	next, total := 0, 0
	for {
		line, dropped, err := readBoundedLine(br, maxLineBytes+4)
		if len(line) > 0 || dropped > 0 || err == nil {
			l := clipN(line, dropped)
			if len(ring) < n {
				ring = append(ring, l)
			} else {
				ring[next] = l
				next = (next + 1) % n
			}
			total++
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, 0, err
		}
	}
	return append(ring[next:], ring[:next]...), total, nil
}

// readBoundedLine reads one line without its newline, keeping at most keep bytes; dropped
// counts the bytes left out.
func readBoundedLine(br *bufio.Reader, keep int) (line string, dropped int, err error) {
	var b []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if err == nil {
			chunk = chunk[:len(chunk)-1]
		}
		room := keep - len(b)
		if len(chunk) > room {
			dropped += len(chunk) - room
			chunk = chunk[:room]
		}
		b = append(b, chunk...)
		if !errors.Is(err, bufio.ErrBufferFull) {
			return string(b), dropped, err
		}
	}
}

// read feeds every line of r to the collector, then closes the packages left open.
func (c *collector) read(r io.Reader) error {
	br := bufio.NewReaderSize(r, 1<<16)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			c.line(line)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	c.finish()
	return nil
}

// line handles one input line: a test2json or build event, or anything else.
func (c *collector) line(b []byte) {
	b = bytes.TrimRight(b, "\r\n")
	if len(bytes.TrimSpace(b)) == 0 {
		return
	}
	var ev event
	if b[0] != '{' || json.Unmarshal(b, &ev) != nil || !knownAction[ev.Action] {
		c.nonJSON(string(b))
		return
	}
	c.handle(&ev)
}

func appendFile(path, text string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(text); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func writeJSON(path string, sum *Summary) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(sum); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}
