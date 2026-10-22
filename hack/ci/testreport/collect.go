package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Bounds that keep memory and report size flat on a run of several hundred thousand events.
const (
	maxNonJSON     = 50      // non-JSON lines kept for the report
	captureHead    = 10      // first lines of a failure's output
	captureErrors  = 30      // t.Error/t.Fatal lines (OutputType error) after the head
	captureTail    = 20      // last lines; head+errors+tail = 60 lines per failure
	maxLineBytes   = 1000    // longer output lines are clipped
	maxJoinedBytes = 8 << 10 // a line split over several output events is joined up to this size
	maxReasonLines = 20      // lines of a skip message
	maxReasonBytes = 4 << 10 // bytes of a skip message; the allowlist is matched against them
	maxEarlier     = 3       // earlier messages of a skipped test kept for the hint
	slowestN       = 15      // entries of the slowest packages and tests lists
	maxListedTests = 20      // tests listed per must-run violation
)

// Test, package and failure states as they appear in the reports.
const (
	statusPass        = "pass"
	statusFail        = "fail"
	statusSkip        = "skip"
	statusUnfinish    = "incomplete"
	subtestsSkipped   = "subtests-skipped" // passed, but every subtest skipped
	pkgOK             = "ok"
	pkgFail           = "fail"
	pkgNoTestFiles    = "no-test-files"
	pkgIncomplete     = "incomplete"
	failTest          = "test"
	failIncomplete    = "incomplete"
	failPackage       = "package"
	failPkgUnfinished = "package-incomplete"
	failBuild         = "build"
)

// event is one line of a `go test -json` stream: a test2json TestEvent (go doc cmd/test2json)
// or a BuildEvent (go help buildjson), told apart by Action.
type event struct {
	Time        string
	Action      string
	Package     string
	Test        string
	Elapsed     float64
	Output      string
	OutputType  string
	FailedBuild string
	ImportPath  string
}

var knownAction = map[string]bool{
	"start": true, "run": true, "pause": true, "cont": true, "pass": true, "bench": true,
	"fail": true, "output": true, "skip": true, "build-output": true, "build-fail": true,
}

// collector aggregates the stream package by package. A package's tests are kept only until
// the package ends; then its results are folded into the summary and the tests are dropped.
type collector struct {
	allow       *allowlist
	must        *mustRunList
	requirePkgs bool

	active map[string]*pkgState
	ended  map[string]bool // packages that reached their final event
	seen   map[string]bool // packages that occurred in the input
	builds map[string]*buildState
	order  int // creation counter, keeps output deterministic for repeated packages

	sum      Summary
	topTests []Timing
	first    time.Time
	last     time.Time
}

type buildState struct {
	out      capture
	failed   bool
	reported bool
}

type pkgState struct {
	name        string
	order       int
	tests       []*testRec
	current     map[string]int // latest record of each test name
	out         capture        // package-level output (events without Test)
	status      string
	elapsed     float64
	failedBuild string
}

type testRec struct {
	name     string
	parent   int // index in pkgState.tests, -1 for a top-level test
	children []int
	status   string
	elapsed  float64
	out      *capture // own output; dropped when the test passes
	reason   *reasonTracker
	pend     pending // a line whose output events have not all arrived
	skip     message // reason and location, once skipped
	hint     string  // set when an earlier message, not the reason, matches the allowlist
}

func newCollector(allow *allowlist, must *mustRunList, requirePkgs bool) *collector {
	if allow == nil {
		allow = &allowlist{}
	}
	if must == nil {
		must = &mustRunList{byPkg: map[string][]*mustRule{}}
	}
	return &collector{
		allow:       allow,
		must:        must,
		requirePkgs: requirePkgs,
		active:      map[string]*pkgState{},
		ended:       map[string]bool{},
		seen:        map[string]bool{},
		builds:      map[string]*buildState{},
	}
}

func (c *collector) nonJSON(line string) {
	c.sum.Totals.NonJSONLines++
	if len(c.sum.NonJSON) < maxNonJSON {
		c.sum.NonJSON = append(c.sum.NonJSON, clip(line))
	}
}

func (c *collector) handle(ev *event) {
	c.sum.Totals.Events++
	if ev.Time != "" {
		if t, err := time.Parse(time.RFC3339Nano, ev.Time); err == nil {
			if c.first.IsZero() || t.Before(c.first) {
				c.first = t
			}
			if t.After(c.last) {
				c.last = t
			}
		}
	}
	switch ev.Action {
	case "build-output":
		c.build(ev.ImportPath).out.addText(ev.Output, false)
		return
	case "build-fail":
		c.build(ev.ImportPath).failed = true
		return
	}

	p := c.pkg(ev)
	if p == nil {
		return
	}
	if ev.Test == "" {
		switch ev.Action {
		case "output":
			if !isFrame(ev.Output, ev.OutputType) {
				p.out.addText(ev.Output, ev.OutputType == "error" || ev.OutputType == "error-continue")
			}
		case "pass", "fail", "skip":
			switch ev.Action {
			case "pass":
				p.status = pkgOK
			case "fail":
				p.status = pkgFail
			default: // a package-level skip means the package has no test files
				p.status = pkgNoTestFiles
			}
			p.elapsed = ev.Elapsed
			p.failedBuild = ev.FailedBuild
			c.endPackage(p)
		}
		return
	}

	t := p.test(ev.Test, ev.Action == "run")
	switch ev.Action {
	case "output":
		t.output(ev.Output, ev.OutputType)
	case "pass", "bench":
		t.status, t.elapsed = statusPass, ev.Elapsed
		t.out, t.reason, t.pend = nil, nil, pending{}
	case "fail":
		t.flush()
		t.status, t.elapsed = statusFail, ev.Elapsed
		t.reason = nil
	case "skip":
		t.flush()
		t.status, t.elapsed = statusSkip, ev.Elapsed
		if t.reason != nil {
			var earlier []message
			t.skip, earlier = t.reason.result()
			t.hint = c.hint(p.name, t.name, t.skip, earlier)
		}
		t.out, t.reason = nil, nil
	}
}

// hint explains a skip whose reason matches no allowlist line: its message was lost in a cut
// line (lostText), or an earlier message of the same test matches the allowlist: a log written
// after t.Skip (by a t.Cleanup or a goroutine) is printed between the t.Skip message and the
// SKIP line, so it is taken as the reason. The skip stays forbidden; the hint only names a
// possible cause (the earlier message may be an ordinary log line).
func (c *collector) hint(pkg, test string, reason message, earlier []message) string {
	if len(c.allow.find(pkg, test, reason.text)) > 0 {
		return ""
	}
	if lostMessage(reason.text, reason.location) {
		return lostHint
	}
	for _, m := range earlier {
		if lines := c.allow.find(pkg, test, m.text); len(lines) > 0 {
			return fmt.Sprintf("the earlier message %q at %s matches allowlist line %s; if it is the t.Skip message, a log written after t.Skip (by a t.Cleanup or a goroutine) took its place: guard that log with if !t.Skipped()",
				shortReason(m.text, m.truncated), orNone(m.location), lineRefs("", lines))
		}
	}
	return ""
}

func (c *collector) build(importPath string) *buildState {
	b := c.builds[importPath]
	if b == nil {
		b = &buildState{}
		c.builds[importPath] = b
	}
	return b
}

// pkg returns the open state of the event's package, opening one when needed. It returns nil
// for stray package output after the package ended.
func (c *collector) pkg(ev *event) *pkgState {
	if p := c.active[ev.Package]; p != nil {
		return p
	}
	if ev.Test == "" && ev.Action == "output" && c.ended[ev.Package] {
		return nil
	}
	if !c.seen[ev.Package] {
		c.seen[ev.Package] = true
		c.allow.notePackage(ev.Package)
	}
	c.order++
	p := &pkgState{name: ev.Package, order: c.order, current: map[string]int{}}
	c.active[ev.Package] = p
	return p
}

// test returns the record of a test, starting a new one on "run" when the previous record of
// that name already ended (-count > 1).
func (p *pkgState) test(name string, isRun bool) *testRec {
	if i, ok := p.current[name]; ok && (!isRun || p.tests[i].status == "") {
		return p.tests[i]
	}
	t := &testRec{name: name, parent: -1, out: &capture{}, reason: &reasonTracker{}}
	for i := strings.LastIndexByte(name, '/'); i > 0; i = strings.LastIndexByte(name[:i], '/') {
		if pi, ok := p.current[name[:i]]; ok {
			t.parent = pi
			break
		}
	}
	idx := len(p.tests)
	p.tests = append(p.tests, t)
	p.current[name] = idx
	if t.parent >= 0 {
		p.tests[t.parent].children = append(p.tests[t.parent].children, idx)
	}
	return t
}

// output handles one output event of a test. Complete lines go to the skip reason tracker and,
// except test framing, to the captured output; a part without its newline waits in pend for
// the rest of its line.
func (t *testRec) output(text, outputType string) {
	if t.out == nil && t.reason == nil {
		return // the test passed: its output is no longer kept
	}
	isErr := outputType == "error" || outputType == "error-continue"
	frame := isFrame(text, outputType)
	if frame {
		t.flush() // framing starts a line of its own (test2json marks it even after a partial line)
	}
	for text != "" {
		part, rest, complete := strings.Cut(text, "\n")
		text = rest
		t.pend.add(part, isErr)
		if !complete {
			return
		}
		line, lineErr, dropped := t.pend.take()
		t.line(line, lineErr, dropped, frame)
	}
}

// flush hands on the part of a line that is still waiting, at the end of a test or before
// framing output.
func (t *testRec) flush() {
	if t.pend.open {
		line, isErr, dropped := t.pend.take()
		t.line(line, isErr, dropped, false)
	}
}

func (t *testRec) line(text string, isErr bool, dropped int, frame bool) {
	if t.reason != nil {
		t.reason.add(text, dropped > 0)
	}
	if t.out != nil && !frame {
		t.out.add(text, isErr, dropped)
	}
}

// pending joins the output events of one line. test2json splits a line longer than its 1024-byte
// buffer into several events, and output printed without a newline is an event of its own. The
// joined line is bounded by maxJoinedBytes; dropped counts the bytes cut off.
type pending struct {
	buf     []byte
	isErr   bool
	dropped int
	open    bool
}

func (p *pending) add(s string, isErr bool) {
	p.open = true
	p.isErr = p.isErr || isErr
	room := maxJoinedBytes - len(p.buf)
	if len(s) > room {
		p.dropped += len(s) - room
		s = s[:room]
	}
	p.buf = append(p.buf, s...)
}

func (p *pending) take() (line string, isErr bool, dropped int) {
	line, isErr, dropped = string(p.buf), p.isErr, p.dropped
	*p = pending{}
	return line, isErr, dropped
}

// endPackage folds a finished (or, at the end of input, unfinished) package into the summary.
func (c *collector) endPackage(p *pkgState) {
	delete(c.active, p.name)
	c.ended[p.name] = true
	if p.status == "" {
		p.status = pkgIncomplete
	}
	tot := &c.sum.Totals
	res := PackageResult{Package: p.name, Status: p.status, Elapsed: round3(p.elapsed), FailedBuild: p.failedBuild}

	for _, t := range p.tests {
		if t.status == "" {
			t.flush()
			t.status = statusUnfinish
		}
		switch t.status {
		case statusPass:
			res.Passed++
		case statusFail:
			res.Failed++
		case statusSkip:
			res.Skipped++
		default:
			res.Incomplete++
		}
		if t.parent < 0 && t.status != statusSkip {
			c.noteSlowTest(Timing{Package: p.name, Test: t.name, Elapsed: round3(t.elapsed)})
		}
	}
	tot.Packages++
	tot.Tests += len(p.tests)
	tot.TestsPassed += res.Passed
	tot.TestsFailed += res.Failed
	tot.TestsSkipped += res.Skipped
	tot.TestsIncomplete += res.Incomplete
	tot.PackageSeconds += p.elapsed
	switch p.status {
	case pkgOK:
		tot.PackagesOK++
		if len(p.tests) == 0 {
			tot.PackagesNoTestsRun++
			c.sum.NoTestsRun = append(c.sum.NoTestsRun, p.name)
		}
	case pkgFail:
		tot.PackagesFailed++
	case pkgNoTestFiles:
		tot.PackagesNoTestFiles++
	default:
		tot.PackagesIncomplete++
	}
	c.sum.Packages = append(c.sum.Packages, res)

	c.collectFailures(p)
	c.collectSkips(p)
	c.checkMustRun(p)
	p.tests, p.current = nil, nil
}

func (c *collector) noteSlowTest(t Timing) {
	c.topTests = append(c.topTests, t)
	if len(c.topTests) > 4*slowestN {
		sortTimings(c.topTests)
		c.topTests = c.topTests[:slowestN]
	}
}

// collectFailures adds the failed and unfinished tests of a package, its failed build, and
// what the package printed outside its tests when that matters. Unfinished tests that printed
// nothing (paused parallel tests, parents of the test that hung) are listed by name in the
// package entry instead of one entry each.
func (c *collector) collectFailures(p *pkgState) {
	failedTests := 0
	var silent []string
	for _, t := range p.tests {
		if t.status != statusFail && t.status != statusUnfinish {
			continue
		}
		failedTests++
		f := Failure{Package: p.name, Test: t.name, Kind: failTest, Elapsed: round3(t.elapsed)}
		if t.out != nil {
			f.Output, f.OmittedLines = t.out.lines()
		}
		if t.status == statusUnfinish {
			if len(f.Output) == 0 {
				silent = append(silent, t.name)
				continue
			}
			f.Kind = failIncomplete
		}
		if len(f.Output) == 0 && len(t.children) > 0 {
			f.Output = []string{"(no output of its own; see its subtests)"}
		}
		c.sum.Failures = append(c.sum.Failures, f)
	}
	if p.failedBuild != "" {
		f := Failure{Package: p.name, Kind: failBuild}
		b := c.build(p.failedBuild)
		b.failed, b.reported = true, true
		f.Output, f.OmittedLines = b.out.lines()
		f.Output = append([]string{"build of " + p.failedBuild + " failed"}, f.Output...)
		c.sum.Failures = append(c.sum.Failures, f)
		return
	}
	// A failed package gets an entry of its own when something outside its tests failed (or
	// printed), or when tests did not finish; one that never finished always does, since its
	// stream was cut.
	if p.status != pkgFail && p.status != pkgIncomplete && len(silent) == 0 {
		return
	}
	out, omitted := p.out.lines()
	if len(out) == 0 && len(silent) == 0 && failedTests > 0 && p.status == pkgFail {
		return // the failed tests' own entries say it all
	}
	if len(silent) > 0 {
		out = append(out, plural(len(silent), "test")+" did not finish and printed nothing:")
		for i, name := range silent {
			if i == maxListedTests {
				out = append(out, fmt.Sprintf("  ... and %d more", len(silent)-i))
				break
			}
			out = append(out, "  "+name)
		}
	}
	if len(out) == 0 {
		out = []string{"(no output outside tests)"}
	}
	kind := failPackage
	if p.status == pkgIncomplete {
		kind = failPkgUnfinished
	}
	c.sum.Failures = append(c.sum.Failures, Failure{Package: p.name, Kind: kind, Elapsed: round3(p.elapsed), Output: out, OmittedLines: omitted})
}

// collectSkips turns the skipped tests of a package into report entries. A test whose
// subtests all skipped (a skipped test, or a passed one whose children are all such tests)
// becomes one entry for the whole group; every member is still matched against the allowlist.
func (c *collector) collectSkips(p *pkgState) {
	full := fullySkipped(p.tests)
	for i, t := range p.tests {
		switch {
		case full[i] && (t.parent < 0 || !full[t.parent]):
			c.addSkip(p, i, groupMembers(p.tests, i))
		case t.status == statusSkip && !full[i]:
			c.addSkip(p, i, []int{i})
		}
	}
}

// fullySkipped reports, per test, whether it and every test below it skipped. Children are
// always recorded after their parent, so one backward pass suffices.
func fullySkipped(tests []*testRec) []bool {
	full := make([]bool, len(tests))
	for i := len(tests) - 1; i >= 0; i-- {
		t := tests[i]
		ok := t.status == statusSkip || (t.status == statusPass && len(t.children) > 0)
		for _, ch := range t.children {
			ok = ok && full[ch]
		}
		full[i] = ok
	}
	return full
}

// groupMembers returns the skipped tests of the group rooted at root, root first.
func groupMembers(tests []*testRec, root int) []int {
	var out []int
	stack := []int{root}
	for len(stack) > 0 {
		i := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if tests[i].status == statusSkip {
			out = append(out, i)
		}
		for j := len(tests[i].children) - 1; j >= 0; j-- {
			stack = append(stack, tests[i].children[j])
		}
	}
	return out
}

func (c *collector) addSkip(p *pkgState, root int, members []int) {
	rootRec := p.tests[root]
	entry := Skip{Package: p.name, Test: rootRec.name, Allowed: true}
	for _, m := range members {
		if m != root {
			entry.Subtests++
		}
	}
	testExpr := regexp.QuoteMeta(rootRec.name)
	if entry.Subtests > 0 {
		testExpr += "(/.*)?"
	}
	byReason := map[message]int{}
	for _, m := range members {
		t := p.tests[m]
		lines := c.allow.match(p.name, t.name, t.skip.text)
		if len(lines) > 0 {
			c.sum.Totals.SkipsAllowed++
		} else {
			c.sum.Totals.SkipsForbidden++
			entry.Allowed = false
		}
		ri, ok := byReason[t.skip]
		if !ok {
			ri = len(entry.Reasons)
			byReason[t.skip] = ri
			entry.Reasons = append(entry.Reasons, Reason{Text: t.skip.text, Truncated: t.skip.truncated, Location: t.skip.location, Allowed: true})
		}
		r := &entry.Reasons[ri]
		r.Tests++
		if len(lines) == 0 {
			r.Allowed = false
		}
		if r.Hint == "" {
			r.Hint = t.hint
		}
		r.AllowlistLines = mergeLines(r.AllowlistLines, lines)
	}
	for _, r := range entry.Reasons {
		// A lost message gets no suggestion: a line for it would allow every skip of the test
		// whose message is lost. Its hint says what happened.
		if !r.Allowed && !lostMessage(r.Text, r.Location) {
			entry.Suggestions = append(entry.Suggestions, suggestRule(p.name, testExpr, r.Text))
		}
	}
	c.sum.Skips = append(c.sum.Skips, entry)
}

func mergeLines(have, add []int) []int {
	for _, l := range add {
		i := sort.SearchInts(have, l)
		if i == len(have) || have[i] != l {
			have = append(have, 0)
			copy(have[i+1:], have[i:])
			have[i] = l
		}
	}
	return have
}

// checkMustRun applies the must-run rules of a package that occurred in the input.
func (c *collector) checkMustRun(p *pkgState) {
	rules := c.must.byPkg[p.name]
	if len(rules) == 0 {
		return
	}
	full := fullySkipped(p.tests)
	for _, r := range rules {
		r.seen = true
		matched := 0
		var bad []TestStatus
		for i, t := range p.tests {
			if !r.test.MatchString(t.name) {
				continue
			}
			matched++
			st := t.status
			if st == statusPass && full[i] {
				st = subtestsSkipped
			}
			if st != statusPass {
				bad = append(bad, TestStatus{Test: t.name, Status: st})
			}
		}
		v := Violation{Line: r.line, Package: r.pkg, Test: r.expr, Comment: r.comment}
		switch {
		case matched == 0:
			v.Problem = fmt.Sprintf("no test matched (package status %s)", p.status)
		case len(bad) > 0:
			v.Problem = fmt.Sprintf("%d of %d matching tests did not pass", len(bad), matched)
			if len(bad) > maxListedTests {
				bad = bad[:maxListedTests]
			}
			v.Tests = bad
		default:
			continue
		}
		c.sum.MustRunViolations = append(c.sum.MustRunViolations, v)
	}
}

// finish closes the packages that never reached their final event (a killed or truncated run).
func (c *collector) finish() {
	open := make([]*pkgState, 0, len(c.active))
	for _, p := range c.active {
		open = append(open, p)
	}
	sort.Slice(open, func(i, j int) bool { return open[i].order < open[j].order })
	for _, p := range open {
		c.endPackage(p)
	}
	paths := make([]string, 0, len(c.builds))
	for path, b := range c.builds {
		if b.failed && !b.reported {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	for _, path := range paths {
		out, omitted := c.builds[path].out.lines()
		c.sum.Failures = append(c.sum.Failures, Failure{Package: path, Kind: failBuild, Output: out, OmittedLines: omitted})
	}
	for _, b := range c.builds {
		if b.failed {
			c.sum.Totals.BuildFailures++
		}
	}
}

// summary completes and returns the report data.
func (c *collector) summary(title string, strict bool) *Summary {
	s := &c.sum
	s.Title = title
	tot := &s.Totals
	if !c.first.IsZero() {
		tot.WallSeconds = round3(c.last.Sub(c.first).Seconds())
	}
	tot.PackageSeconds = round3(tot.PackageSeconds)

	sort.SliceStable(s.Packages, func(i, j int) bool { return s.Packages[i].Package < s.Packages[j].Package })
	s.SlowestPackages = nil
	for _, p := range s.Packages {
		if p.Status == pkgOK || p.Status == pkgFail {
			s.SlowestPackages = append(s.SlowestPackages, Timing{Package: p.Package, Elapsed: p.Elapsed})
		}
	}
	sortTimings(s.SlowestPackages)
	if len(s.SlowestPackages) > slowestN {
		s.SlowestPackages = s.SlowestPackages[:slowestN]
	}
	sortTimings(c.topTests)
	if len(c.topTests) > slowestN {
		c.topTests = c.topTests[:slowestN]
	}
	s.SlowestTests = c.topTests

	sort.SliceStable(s.Failures, func(i, j int) bool {
		a, b := s.Failures[i], s.Failures[j]
		if a.Package != b.Package {
			return a.Package < b.Package
		}
		return a.Test < b.Test
	})
	sort.SliceStable(s.Skips, func(i, j int) bool {
		a, b := s.Skips[i], s.Skips[j]
		if a.Package != b.Package {
			return a.Package < b.Package
		}
		return a.Test < b.Test
	})
	sort.Strings(s.NoTestsRun)

	for _, r := range c.must.rules {
		if r.seen {
			continue
		}
		if c.requirePkgs {
			s.MustRunViolations = append(s.MustRunViolations, Violation{Line: r.line, Package: r.pkg, Test: r.expr, Comment: r.comment, Problem: "package does not occur in the input"})
		} else {
			tot.MustRunNotApplicable++
		}
	}
	sort.SliceStable(s.MustRunViolations, func(i, j int) bool { return s.MustRunViolations[i].Line < s.MustRunViolations[j].Line })
	tot.MustRunViolations = len(s.MustRunViolations)

	for _, r := range c.allow.rules {
		switch {
		case !r.applicable:
			tot.AllowlistNotApplicable++
		case r.hits == 0:
			s.StaleAllowlist = append(s.StaleAllowlist, RuleRef{Line: r.line, Text: r.text})
		}
	}
	tot.StaleAllowlistLines = len(s.StaleAllowlist)

	s.TestsResult = resultOK
	switch {
	case tot.Events == 0:
		s.TestsResult = resultNoEvents
	case tot.PackagesFailed+tot.PackagesIncomplete+tot.BuildFailures+tot.TestsFailed+tot.TestsIncomplete > 0:
		s.TestsResult = resultFailed
	}
	s.PolicyResult = resultOK
	if tot.SkipsForbidden > 0 || tot.MustRunViolations > 0 || (strict && tot.StaleAllowlistLines > 0) {
		s.PolicyResult = resultViolations
	}
	switch {
	case s.TestsResult == resultNoEvents:
		s.Result = resultNoEvents
	case s.TestsResult != resultOK || s.PolicyResult != resultOK:
		s.Result = resultFailed
	default:
		s.Result = resultOK
	}
	s.StrictAllowlist = strict
	return s
}

func sortTimings(ts []Timing) {
	sort.SliceStable(ts, func(i, j int) bool {
		if ts[i].Elapsed != ts[j].Elapsed {
			return ts[i].Elapsed > ts[j].Elapsed
		}
		if ts[i].Package != ts[j].Package {
			return ts[i].Package < ts[j].Package
		}
		return ts[i].Test < ts[j].Test
	})
}

func round3(f float64) float64 {
	if f < 0 {
		return 0
	}
	return float64(int64(f*1000+0.5)) / 1000
}

// framePrefixes are the lines go test prints around tests ("test framing" in test2json terms).
// test2json marks them with OutputType "frame"; the prefixes cover streams without that field.
var framePrefixes = []string{
	"=== RUN ", "=== PAUSE ", "=== CONT ", "=== NAME ",
	"--- PASS: ", "--- FAIL: ", "--- SKIP: ", "--- BENCH: ",
	"PASS", "FAIL", "ok  \t", "?   \t",
}

func isFrame(text, outputType string) bool {
	if outputType == "frame" {
		return true
	}
	l := strings.TrimLeft(strings.TrimRight(text, "\r\n"), " ")
	for _, p := range framePrefixes {
		if strings.HasPrefix(l, p) {
			return p != "PASS" && p != "FAIL" || l == p || strings.HasPrefix(l, p+"\t")
		}
	}
	return false
}

// splitLines splits one output event into lines; an event usually holds exactly one.
func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}

func clip(s string) string { return clipN(s, 0) }

// clipN clips a line to maxLineBytes; dropped counts bytes of it that were cut off earlier.
// Carriage returns inside the line are escaped (escapeCR).
func clipN(s string, dropped int) string {
	s = strings.TrimRight(s, "\r")
	if len(s) <= maxLineBytes && dropped == 0 {
		return escapeCR(s)
	}
	cut := cutRunes(s, maxLineBytes)
	return fmt.Sprintf("%s ... (%d more bytes)", escapeCR(cut), len(s)-len(cut)+dropped)
}

// escapeCR shows each carriage return of s as the two characters \r. The runner reads a step's
// output line by line and ends a line at a lone CR, so a raw one in test output would start a
// new log line (and "::error ..." after it would be a workflow command); in the step summary it
// ends a table row. It does not neutralise `##[error]`-style commands, which the runner finds
// anywhere in a line: hack/ci.mk stops workflow commands around the report instead.
func escapeCR(s string) string { return strings.ReplaceAll(s, "\r", `\r`) }

// cutRunes returns the longest prefix of s of at most n bytes that ends on a rune boundary.
func cutRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// shortReason is a skip message as the reports show it: at most maxLineBytes, marked when it
// was cut here or when the input held more than maxReasonBytes of it.
func shortReason(text string, truncated bool) string {
	if len(text) > maxLineBytes {
		return cutRunes(text, maxLineBytes) + " ... (truncated)"
	}
	if truncated {
		return text + " ... (truncated)"
	}
	return text
}

// capture keeps a bounded sample of output lines: the first captureHead lines, up to
// captureErrors lines that test2json marks as t.Error/t.Fatal output, and the last captureTail
// lines. Lines are numbered so that the sample prints in order with the gaps marked.
type capture struct {
	head  []numbered
	errs  []numbered
	tail  []numbered // ring buffer once full
	next  int
	total int
}

type numbered struct {
	n    int
	text string
}

func (c *capture) addText(text string, isErr bool) {
	for _, l := range splitLines(text) {
		c.add(l, isErr, 0)
	}
}

func (c *capture) add(text string, isErr bool, dropped int) {
	l := numbered{n: c.total, text: clipN(text, dropped)}
	c.total++
	switch {
	case len(c.head) < captureHead:
		c.head = append(c.head, l)
	case isErr && len(c.errs) < captureErrors:
		c.errs = append(c.errs, l)
	case len(c.tail) < captureTail:
		c.tail = append(c.tail, l)
	default:
		c.tail[c.next] = l
		c.next = (c.next + 1) % captureTail
	}
}

// lines returns the kept lines in order, with a marker where lines were left out, and the
// number of lines left out.
func (c *capture) lines() ([]string, int) {
	all := make([]numbered, 0, len(c.head)+len(c.errs)+len(c.tail))
	all = append(all, c.head...)
	all = append(all, c.errs...)
	all = append(all, c.tail...)
	sort.Slice(all, func(i, j int) bool { return all[i].n < all[j].n })
	var out []string
	omitted, prev := 0, -1
	for _, l := range all {
		if gap := l.n - prev - 1; gap > 0 {
			out = append(out, fmt.Sprintf("... (%d lines omitted)", gap))
			omitted += gap
		}
		out = append(out, l.text)
		prev = l.n
	}
	if gap := c.total - prev - 1; gap > 0 {
		out = append(out, fmt.Sprintf("... (%d lines omitted)", gap))
		omitted += gap
	}
	return out, omitted
}

// message is a skip reason: the text of a t.Skip call and where it was made. text holds at
// most maxReasonLines lines and maxReasonBytes bytes; truncated says the message was longer.
type message struct {
	text      string
	location  string
	truncated bool
}

// reasonTracker finds the skip message in a test's output. t.Log, t.Skip and friends print
// "    file.go:NN: text", with continuation lines indented four more spaces. The reason is the
// last such block before the "--- SKIP" line; streams that print the message after that line
// (older go versions, non-verbose mode) are handled too.
//
// A line that continues the current block stays part of it even when it looks like a header
// (a message whose second line is "    gen.go:12: ..."). A header also counts after output the
// test printed without a newline, which go puts on the same line ("progress: 3/3    x.go:9:
// msg"). When such a line was cut (pending keeps maxJoinedBytes of it) and the kept part holds
// no header, the message may be in the part cut off: a lostText block takes its place, so that
// an earlier block cannot become the reason. A log written after t.Skip (by a t.Cleanup, a
// deferred call or a goroutine) is printed before the "--- SKIP" line and becomes the reason;
// nothing in the stream tells the two apart, so the last block stays the reason. Such a log
// fails the skip when it matches no allowlist line (the up to maxEarlier blocks before it are
// kept for the hint), and passes it when it matches one, whatever the t.Skip message was.
type reasonTracker struct {
	cur        *block   // block being extended by continuation lines
	recent     []*block // the last maxEarlier+1 blocks, oldest first
	atFrame    []*block // recent, when the SKIP line was seen
	afterFrame *block   // first block after the SKIP line
	sawFrame   bool
}

type block struct {
	indent    string // continuation prefix
	location  string
	lines     []string
	size      int // bytes of lines joined by newlines
	truncated bool
}

var (
	logHeader = regexp.MustCompile(`^( +)(\S+\.go:\d+): ?(.*)$`)
	// fragmentHeader finds a header after output printed without a trailing newline: anything
	// but spaces alone (logHeader's case), down to a lone tab or carriage return. go 1.27
	// indents every t.Log and t.Skip header by four spaces, so the continuation prefix is eight.
	// The first such header of the line counts; the rest of the line is the message.
	fragmentHeader = regexp.MustCompile(`^.*?[^ ] *( {4})(\S+\.go:\d+): ?(.*)$`)
)

// lostText is the reason of a skip whose message may be in the part of a line that was cut
// off; lostHint explains it in the reports.
const (
	lostText = "(skip message lost: an output line was cut at 8 KiB)"
	lostHint = "the test printed a line of more than 8 KiB without a newline before its skip message, which go puts on that line; the part that was cut off may hold the message: end that output with a newline before t.Skip"
)

// lostMessage reports whether a skip reason is the lostText marker; a message of the test always
// has a location.
func lostMessage(text, location string) bool { return location == "" && text == lostText }

// add takes one line of the test's output; cut says that part of the line was dropped.
func (r *reasonTracker) add(line string, cut bool) {
	line = strings.TrimRight(line, "\r")
	if strings.HasPrefix(strings.TrimLeft(line, " "), "--- SKIP: ") {
		r.sawFrame, r.cur = true, nil
		r.atFrame = append([]*block(nil), r.recent...)
		return
	}
	if r.cur != nil && strings.HasPrefix(line, r.cur.indent) {
		r.cur.add(strings.TrimPrefix(line, r.cur.indent), cut)
		return
	}
	m := logHeader.FindStringSubmatch(line)
	if m == nil {
		m = fragmentHeader.FindStringSubmatch(line)
	}
	switch {
	case m != nil:
		b := &block{indent: m[1] + "    ", location: m[2]}
		b.add(m[3], cut)
		r.push(b)
	case cut:
		// The header may be in the part cut off. The marker is complete (truncated), and takes
		// the continuation lines of a lost go 1.27 message, which could look like headers.
		r.push(&block{indent: "        ", lines: []string{lostText}, size: len(lostText), truncated: true})
	default:
		r.cur = nil
	}
}

// push makes b the current block and the most recent one.
func (r *reasonTracker) push(b *block) {
	r.cur = b
	if len(r.recent) > maxEarlier {
		r.recent = append(r.recent[:0], r.recent[1:]...)
	}
	r.recent = append(r.recent, b)
	if r.sawFrame && r.afterFrame == nil {
		r.afterFrame = b
	}
}

func (b *block) add(s string, cut bool) {
	if b.truncated {
		return
	}
	if len(b.lines) == maxReasonLines {
		b.truncated = true
		return
	}
	room := maxReasonBytes - b.size
	if len(b.lines) > 0 {
		room-- // the newline before s
	}
	if len(s) > room {
		b.truncated = true
		if room <= 0 {
			return
		}
		s = cutRunes(s, room)
	}
	if len(b.lines) > 0 {
		b.size++
	}
	b.lines = append(b.lines, s)
	b.size += len(s)
	b.truncated = b.truncated || cut
}

func (b *block) message() message {
	return message{text: strings.Join(b.lines, "\n"), location: b.location, truncated: b.truncated}
}

// result returns the skip message and the messages printed before it, most recent first.
func (r *reasonTracker) result() (message, []message) {
	blocks := r.recent
	if r.sawFrame {
		switch {
		case len(r.atFrame) > 0:
			blocks = r.atFrame
		case r.afterFrame != nil:
			blocks = []*block{r.afterFrame}
		}
	}
	if len(blocks) == 0 {
		return message{}, nil
	}
	var earlier []message
	for i := len(blocks) - 2; i >= 0; i-- {
		earlier = append(earlier, blocks[i].message())
	}
	return blocks[len(blocks)-1].message(), earlier
}
