// Command parity generates docs/parity/matrix.md from the per-area inventories in
// docs/parity/areas/*.md and the implementation status overlay docs/parity/status.yaml.
//
// Usage: go run ./hack/parity [-check]
//
// With -check it exits non-zero when matrix.md is stale or status.yaml references unknown
// feature IDs.
package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

type feature struct {
	ID, Title, Area, AreaFile, Maturity, Exposure string
}

type status struct {
	Status   string   `json:"status"`
	Impl     []string `json:"impl,omitempty"`
	Tests    []string `json:"tests,omitempty"`
	Evidence string   `json:"evidence,omitempty"`
	Notes    string   `json:"notes,omitempty"`
}

var (
	headingRe  = regexp.MustCompile(`^### ([A-Z]+-[0-9]{3})\s+(.*)$`)
	maturityRe = regexp.MustCompile(`^- \*\*Maturity\*\*:\s*(.*)$`)
	exposureRe = regexp.MustCompile(`^- \*\*Exposure\*\*:\s*(.*)$`)
	areaRe     = regexp.MustCompile(`^# (.*?)\s+—`)
)

var validStatus = map[string]bool{
	"not-started": true, "in-progress": true, "implemented": true, "verified": true,
	"verified-simulated": true, "blocked": true, "different-by-design": true,
}

func main() {
	check := flag.Bool("check", false, "fail if matrix.md is stale")
	root := flag.String("root", ".", "repository root")
	flag.Parse()
	if err := run(*root, *check); err != nil {
		fmt.Fprintln(os.Stderr, "parity:", err)
		os.Exit(1)
	}
}

func run(root string, check bool) error {
	files, err := filepath.Glob(filepath.Join(root, "docs/parity/areas/*.md"))
	if err != nil {
		return err
	}
	sort.Strings(files)
	var feats []feature
	seen := map[string]string{}
	for _, f := range files {
		fs, err := parseArea(f)
		if err != nil {
			return err
		}
		for _, x := range fs {
			if prev, ok := seen[x.ID]; ok {
				return fmt.Errorf("duplicate feature ID %s in %s and %s", x.ID, prev, x.AreaFile)
			}
			seen[x.ID] = x.AreaFile
		}
		feats = append(feats, fs...)
	}
	st, err := loadStatus(filepath.Join(root, "docs/parity/status"))
	if err != nil {
		return err
	}
	var problems []string
	for id, s := range st {
		if _, ok := seen[id]; !ok {
			problems = append(problems, "status.yaml: unknown feature "+id)
		}
		if !validStatus[s.Status] {
			problems = append(problems, fmt.Sprintf("status.yaml: %s has invalid status %q", id, s.Status))
		}
		if (s.Status == "verified" || s.Status == "verified-simulated") && len(s.Tests) == 0 {
			problems = append(problems, "status.yaml: "+id+" is verified but lists no tests")
		}
	}
	sort.Strings(problems)
	out := render(feats, st)
	target := filepath.Join(root, "docs/parity/matrix.md")
	if check {
		cur, _ := os.ReadFile(target)
		if !bytes.Equal(cur, out) {
			problems = append(problems, "docs/parity/matrix.md is stale; run go run ./hack/parity")
		}
		if len(problems) > 0 {
			return fmt.Errorf("%s", strings.Join(problems, "\n"))
		}
		return nil
	}
	for _, p := range problems {
		fmt.Fprintln(os.Stderr, "warning:", p)
	}
	return os.WriteFile(target, out, 0o644)
}

var rank = map[string]int{"not-started": 0, "in-progress": 1, "implemented": 2, "verified-simulated": 3, "verified": 4}

// loadStatus merges every docs/parity/status/*.yaml file. When several workstreams report
// the same feature, implementation/test lists are concatenated and the status is the least
// advanced one (a feature is only verified when every contributing part is); any "blocked"
// entry wins, and "different-by-design" applies only when all entries agree.
func loadStatus(dir string) (map[string]status, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	out := map[string]status{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		m := map[string]status{}
		if err := yaml.Unmarshal(b, &m); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		for id, s := range m {
			prev, ok := out[id]
			if !ok {
				out[id] = s
				continue
			}
			merged := prev
			merged.Impl = append(merged.Impl, s.Impl...)
			merged.Tests = append(merged.Tests, s.Tests...)
			merged.Evidence = strings.Trim(prev.Evidence+"; "+s.Evidence, "; ")
			merged.Notes = strings.Trim(prev.Notes+"; "+s.Notes, "; ")
			switch {
			case prev.Status == "blocked" || s.Status == "blocked":
				merged.Status = "blocked"
			case prev.Status == "different-by-design" && s.Status == "different-by-design":
				merged.Status = "different-by-design"
			case prev.Status == "different-by-design":
				merged.Status = s.Status
			case s.Status == "different-by-design":
			default:
				if rank[s.Status] < rank[prev.Status] {
					merged.Status = s.Status
				}
			}
			out[id] = merged
		}
	}
	return out, nil
}

func parseArea(path string) ([]feature, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	area := strings.TrimSuffix(filepath.Base(path), ".md")
	var out []feature
	var cur *feature
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if m := areaRe.FindStringSubmatch(line); m != nil && len(out) == 0 && cur == nil {
			area = m[1]
		}
		if m := headingRe.FindStringSubmatch(line); m != nil {
			if cur != nil {
				out = append(out, *cur)
			}
			cur = &feature{ID: m[1], Title: strings.TrimSpace(m[2]), Area: area, AreaFile: filepath.Base(path)}
			continue
		}
		if cur == nil {
			continue
		}
		if m := maturityRe.FindStringSubmatch(line); m != nil && cur.Maturity == "" {
			cur.Maturity = firstWord(m[1])
		}
		if m := exposureRe.FindStringSubmatch(line); m != nil && cur.Exposure == "" {
			cur.Exposure = exposureFlags(m[1])
		}
		if strings.HasPrefix(line, "## ") {
			out = append(out, *cur)
			cur = nil
		}
	}
	if cur != nil {
		out = append(out, *cur)
	}
	return out, sc.Err()
}

func firstWord(s string) string {
	s = strings.Trim(strings.TrimSpace(s), "*`")
	if i := strings.IndexAny(s, " (,;."); i > 0 {
		s = s[:i]
	}
	if s == "" {
		return "GA"
	}
	return s
}

// exposureFlags condenses the free-text exposure line into C/A/L/U flags (CRD, API, CLI, UI).
func exposureFlags(s string) string {
	l := strings.ToLower(s)
	flag := func(key, f string) string {
		i := strings.Index(l, key)
		if i < 0 {
			return "-"
		}
		rest := strings.TrimLeft(l[i+len(key):], " *:")
		for _, neg := range []string{"none", "n/a", "no ", "not exposed", "—", "-"} {
			if strings.HasPrefix(rest, neg) {
				return "-"
			}
		}
		return f
	}
	return flag("crd", "C") + flag("api", "A") + flag("cli", "L") + flag("ui", "U")
}

func render(feats []feature, st map[string]status) []byte {
	var b bytes.Buffer
	b.WriteString("# Parity matrix — Argo CD v3.5.5 → Patrol CD\n\n")
	b.WriteString("Generated by `go run ./hack/parity` from `docs/parity/areas/*.md` (feature inventory) and\n")
	b.WriteString("`docs/parity/status.yaml` (Patrol implementation status). Do not edit by hand.\n\n")
	b.WriteString("Status values: `not-started`, `in-progress`, `implemented` (code present, evidence incomplete),\n")
	b.WriteString("`verified` (tests passed against real components), `verified-simulated` (tests passed against fakes/contract\n")
	b.WriteString("servers only), `blocked`, `different-by-design` (intentional difference, documented).\n")
	b.WriteString("Exposure flags: C=CRD, A=API, L=CLI, U=UI.\n\n")

	type agg struct {
		total int
		by    map[string]int
	}
	areas := map[string]*agg{}
	var order []string
	for _, f := range feats {
		a, ok := areas[f.AreaFile]
		if !ok {
			a = &agg{by: map[string]int{}}
			areas[f.AreaFile] = a
			order = append(order, f.AreaFile)
		}
		a.total++
		a.by[statusOf(st, f.ID)]++
	}
	cols := []string{"verified", "verified-simulated", "implemented", "in-progress", "not-started", "blocked", "different-by-design"}
	b.WriteString("## Summary\n\n| Area | Features |")
	for _, c := range cols {
		b.WriteString(" " + c + " |")
	}
	b.WriteString("\n|---|---|" + strings.Repeat("---|", len(cols)) + "\n")
	tot := &agg{by: map[string]int{}}
	for _, k := range order {
		a := areas[k]
		fmt.Fprintf(&b, "| [%s](areas/%s) | %d |", strings.TrimSuffix(k, ".md"), k, a.total)
		for _, c := range cols {
			fmt.Fprintf(&b, " %d |", a.by[c])
			tot.by[c] += a.by[c]
		}
		tot.total += a.total
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "| **Total** | **%d** |", tot.total)
	for _, c := range cols {
		fmt.Fprintf(&b, " **%d** |", tot.by[c])
	}
	b.WriteString("\n")

	cur := ""
	for _, f := range feats {
		if f.AreaFile != cur {
			cur = f.AreaFile
			fmt.Fprintf(&b, "\n## %s\n\n| ID | Feature | Maturity | Exp | Status | Implementation | Tests / evidence |\n|---|---|---|---|---|---|---|\n", f.Area)
		}
		s := st[f.ID]
		impl := strings.Join(s.Impl, "<br>")
		ev := strings.Join(s.Tests, "<br>")
		if s.Evidence != "" {
			if ev != "" {
				ev += "<br>"
			}
			ev += s.Evidence
		}
		if s.Notes != "" {
			if ev != "" {
				ev += "<br>"
			}
			ev += "_" + s.Notes + "_"
		}
		fmt.Fprintf(&b, "| [%s](areas/%s#%s) | %s | %s | %s | %s | %s | %s |\n",
			f.ID, f.AreaFile, anchor(f.ID+" "+f.Title), esc(f.Title), f.Maturity, f.Exposure, statusOf(st, f.ID), esc(impl), esc(ev))
	}
	return b.Bytes()
}

func statusOf(st map[string]status, id string) string {
	if s, ok := st[id]; ok && s.Status != "" {
		return s.Status
	}
	return "not-started"
}

func esc(s string) string { return strings.ReplaceAll(s, "|", "\\|") }

// anchor approximates GitHub's heading anchor algorithm.
func anchor(h string) string {
	h = strings.ToLower(h)
	var b strings.Builder
	for _, r := range h {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('-')
		}
	}
	return b.String()
}
