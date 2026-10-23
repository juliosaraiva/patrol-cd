package main

import (
	"bufio"
	"fmt"
	"os"
	"slices"
	"strings"
)

type acceptance struct {
	Rule, Path, Fingerprint, Reason string
	Line                            int // line in the baseline file
}

func readBaseline(path string) ([]acceptance, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // read only
	var out []acceptance
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		cols := strings.Split(line, "\t")
		if len(cols) != 4 || slices.Contains(cols, "") {
			return nil, fmt.Errorf("%w: %s:%d: want 4 non-empty tab-separated columns (rule, path, fingerprint, reason)", errBaseline, path, n)
		}
		out = append(out, acceptance{Rule: cols[0], Path: cols[1], Fingerprint: cols[2], Reason: cols[3], Line: n})
	}
	return out, sc.Err()
}

type verdict struct {
	result
	State  string // fail, accepted, suppressed, info
	Reason string
}

type report struct {
	Verdicts []verdict
	Unused   []acceptance
}

// blocking reports whether a result of this level stops a push.
func blocking(level string) bool { return level == "error" || level == "warning" }

func evaluate(results []result, accepted []acceptance) report {
	used := make([]bool, len(accepted))
	var rep report
	for _, r := range results {
		v := verdict{result: r, State: "info"}
		switch {
		case r.Suppressed:
			v.State = "suppressed"
		case blocking(r.Level):
			v.State = "fail"
			for i, a := range accepted {
				if a.Rule == r.Rule && a.Path == r.Path && a.Fingerprint == r.Fingerprint {
					v.State, v.Reason, used[i] = "accepted", a.Reason, true
					break
				}
			}
		}
		rep.Verdicts = append(rep.Verdicts, v)
	}
	for i, a := range accepted {
		if !used[i] {
			rep.Unused = append(rep.Unused, a)
		}
	}
	order := map[string]int{"fail": 0, "accepted": 1, "suppressed": 2, "info": 3}
	slices.SortStableFunc(rep.Verdicts, func(a, b verdict) int {
		if d := order[a.State] - order[b.State]; d != 0 {
			return d
		}
		if c := strings.Compare(a.Path, b.Path); c != 0 {
			return c
		}
		return a.Line - b.Line
	})
	return rep
}

func (r report) failed() bool {
	return slices.ContainsFunc(r.Verdicts, func(v verdict) bool { return v.State == "fail" })
}

func (r report) count(state string) int {
	n := 0
	for _, v := range r.Verdicts {
		if v.State == state {
			n++
		}
	}
	return n
}

func (r report) String() string {
	var b strings.Builder
	head := "OK"
	if r.failed() {
		head = "FAILED"
	}
	fmt.Fprintf(&b, "CodeQL gate: %s: %d results, %d blocking, %d accepted in the baseline, %d suppressed in the source, %d notes\n",
		head, len(r.Verdicts), r.count("fail"), r.count("accepted"), r.count("suppressed"), r.count("info"))
	for _, v := range r.Verdicts {
		sev := ""
		if v.Severity > 0 {
			sev = fmt.Sprintf(" (security-severity %.1f)", v.Severity)
		}
		fmt.Fprintf(&b, "%-10s %-7s %s %s:%d%s\n           %s\n", v.State, v.Level, v.Rule, v.Path, v.Line, sev, firstLine(v.Message))
		if v.State == "fail" {
			fmt.Fprintf(&b, "           fingerprint %s\n", v.Fingerprint)
		}
		if v.Reason != "" {
			fmt.Fprintf(&b, "           accepted: %s\n", v.Reason)
		}
	}
	for _, a := range r.Unused {
		fmt.Fprintf(&b, "unused baseline line %d: %s %s %s\n", a.Line, a.Rule, a.Path, a.Fingerprint)
	}
	return b.String()
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return s
}
