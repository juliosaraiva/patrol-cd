package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseCommit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		subject, body          string
		typ, scope, desc, note string
		breaking               bool
		pr                     int
	}{
		{subject: "feat(api): add a field", typ: "feat", scope: "api", desc: "add a field"},
		{subject: "fix: plain fix", typ: "fix", desc: "plain fix"},
		{subject: "feat(ui)!: drop the old page", typ: "feat", scope: "ui", desc: "drop the old page", breaking: true},
		{subject: "refactor(x): y", body: "Details.\n\nBREAKING CHANGE: the flag\n  --foo is gone\nRefs: #3", typ: "refactor", scope: "x", desc: "y", breaking: true, note: "the flag --foo is gone"},
		{subject: "fix(git): squash merged (#42)", typ: "fix", scope: "git", desc: "squash merged", pr: 42},
		{subject: "Update README"},
		{subject: "feat:missing space"},
		{subject: "Feat: upper case"},
	}
	for _, tc := range tests {
		c := commit{Subject: tc.subject, Body: tc.body}
		c.parse()
		if c.Type != tc.typ || c.Scope != tc.scope || c.Desc != tc.desc || c.Breaking != tc.breaking || c.BreakingNote != tc.note || c.PR != tc.pr {
			t.Errorf("%q: got type=%q scope=%q desc=%q breaking=%t note=%q pr=%d", tc.subject, c.Type, c.Scope, c.Desc, c.Breaking, c.BreakingNote, c.PR)
		}
	}
}

func TestBump(t *testing.T) {
	t.Parallel()
	c := func(typ string, breaking bool) commit { return commit{Type: typ, Breaking: breaking} }
	tests := []struct {
		from    string
		commits []commit
		want    string
	}{
		{"0.1.0", []commit{c("docs", false), c("chore", false)}, "0.1.0"},
		{"0.1.0", []commit{c("fix", false)}, "0.1.1"},
		{"0.1.3", []commit{c("fix", false), c("feat", false)}, "0.2.0"},
		{"0.1.3", []commit{c("refactor", true)}, "0.2.0"},
		{"1.4.2", []commit{c("feat", true), c("fix", false)}, "2.0.0"},
		{"1.4.2", []commit{c("perf", false)}, "1.4.3"},
		{"1.4.2", []commit{{Breaking: true}}, "1.4.2"}, // not a conventional commit
	}
	for _, tc := range tests {
		v, err := parseVersion(tc.from)
		if err != nil {
			t.Fatal(err)
		}
		if got := v.apply(bumpFor(tc.commits)).String(); got != tc.want {
			t.Errorf("%s + %v = %s, want %s", tc.from, tc.commits, got, tc.want)
		}
	}
	for _, bad := range []string{"1.2", "01.2.3", "1.2.3-rc.1", "v1.2.3.4", ""} {
		if _, err := parseVersion(bad); err == nil {
			t.Errorf("parseVersion(%q) succeeded", bad)
		}
	}
}

func TestRenderAndInsertSection(t *testing.T) {
	t.Parallel()
	info := releaseInfo{
		Version: version{0, 2, 0}, Previous: "v0.1.0", Date: "2026-10-22", RepoURL: "https://github.com/o/r",
		Commits: []commit{
			{SHA: "aaaaaaaaaa", Type: "feat", Scope: "ui", Desc: "new <b>page</b>", PR: 7},
			{SHA: "bbbbbbbbbb", Type: "fix", Desc: "a fix"},
			{SHA: "cccccccccc", Type: "docs", Desc: "hidden"},
			{SHA: "dddddddddd", Type: "feat", Scope: "api", Desc: "rename", Breaking: true, BreakingNote: "old name removed"},
		},
	}
	got := renderSection(info)
	want := `## [0.2.0](https://github.com/o/r/compare/v0.1.0...v0.2.0) - 2026-10-22

### Breaking changes

- **api:** rename ([ddddddd](https://github.com/o/r/commit/dddddddddd))
  old name removed

### Features

- **ui:** new &lt;b&gt;page&lt;/b&gt; ([#7](https://github.com/o/r/pull/7)) ([aaaaaaa](https://github.com/o/r/commit/aaaaaaaaaa))
- **api:** rename ([ddddddd](https://github.com/o/r/commit/dddddddddd))

### Bug fixes

- a fix ([bbbbbbb](https://github.com/o/r/commit/bbbbbbbbbb))
`
	if got != want {
		t.Fatalf("renderSection:\n%s\nwant:\n%s", got, want)
	}

	// First section into an empty file, then a newer one goes on top, then a re-run replaces it.
	cl := insertSection("", "## [0.1.0] - 2026-10-01\n\n- first\n", version{0, 1, 0})
	if !strings.HasPrefix(cl, "# Changelog\n") || !strings.HasSuffix(cl, "## [0.1.0] - 2026-10-01\n\n- first\n") {
		t.Fatalf("first insert:\n%s", cl)
	}
	cl = insertSection(cl, got, version{0, 2, 0})
	cl = insertSection(cl, strings.Replace(got, "a fix", "a better fix", 1), version{0, 2, 0})
	if strings.Count(cl, "## [0.2.0]") != 1 || !strings.Contains(cl, "a better fix") {
		t.Fatalf("re-insert did not replace:\n%s", cl)
	}
	if strings.Index(cl, "## [0.2.0]") > strings.Index(cl, "## [0.1.0]") {
		t.Fatalf("newest section not first:\n%s", cl)
	}
	notes, ok := sectionNotes(cl, version{0, 1, 0})
	if !ok || notes != "- first\n" {
		t.Fatalf("sectionNotes(0.1.0) = %q, %t", notes, ok)
	}
	if _, ok := sectionNotes(cl, version{0, 3, 0}); ok {
		t.Fatal("sectionNotes found a missing version")
	}
}

func TestChartAndValues(t *testing.T) {
	t.Parallel()
	chart := "apiVersion: v2\nname: patrol-cd\nversion: 0.1.0\nappVersion: \"0.1.0\"\nkubeVersion: \">=1.30.0-0\"\n"
	out, err := setChartVersion(chart, version{0, 2, 0})
	if err != nil {
		t.Fatal(err)
	}
	if want := "apiVersion: v2\nname: patrol-cd\nversion: 0.2.0\nappVersion: \"0.2.0\"\nkubeVersion: \">=1.30.0-0\"\n"; out != want {
		t.Fatalf("setChartVersion:\n%s", out)
	}
	if v, err := chartVersion(out); err != nil || v != (version{0, 2, 0}) {
		t.Fatalf("chartVersion = %v, %v", v, err)
	}

	values := "global:\n  imageRegistry: \"\"\nimage:\n  # -- Registry.\n  registry: \"\"\n  repository: patrol-cd/patrol-cd\ndex:\n  image:\n    registry: \"\"\n"
	got, err := setImageRegistry(values, "ghcr.io/o")
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.Replace(values, "  registry: \"\"\n  repository", "  registry: \"ghcr.io/o\"\n  repository", 1); got != want {
		t.Fatalf("setImageRegistry:\n%s", got)
	}
	if _, err := setImageRegistry("image:\n  repository: x\n", "r"); err == nil {
		t.Fatal("setImageRegistry without the key succeeded")
	}
}

func TestRetagManifest(t *testing.T) {
	t.Parallel()
	m := "containers:\n  - name: a\n    image: patrol-cd/patrol-cd:0.2.0\n  - image: patrol-cd/patrol-cd:0.2.0\n    name: b\n  - image: other/patrol-cd/patrol-cd:0.2.0\n"
	out, n, err := retagManifest(m, imageRepository, "ghcr.io/o/patrol-cd/patrol-cd", version{0, 2, 0})
	if err != nil || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if strings.Count(out, "image: ghcr.io/o/patrol-cd/patrol-cd:0.2.0") != 2 || !strings.Contains(out, "image: other/patrol-cd/patrol-cd:0.2.0") {
		t.Fatalf("retagManifest:\n%s", out)
	}
	if _, _, err := retagManifest(m, imageRepository, "x", version{0, 3, 0}); err == nil {
		t.Fatal("stale tag accepted")
	}
}

// TestRepository runs the commands against a scratch git repository: first release from the
// chart version, pending state, the next bump with a pull request merge, lint and stage.
func TestRepository(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	ctx := context.Background()
	sh := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
			"GIT_AUTHOR_DATE=2026-10-20T10:00:00Z", "GIT_COMMITTER_DATE=2026-10-20T10:00:00Z")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(path, content string) {
		t.Helper()
		p := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	commit := func(msg string) {
		t.Helper()
		sh("add", "-A")
		sh("commit", "-q", "--allow-empty", "-m", msg)
	}
	call := func(args ...string) (string, error) {
		var b bytes.Buffer
		err := run(ctx, append(args[:1:1], append([]string{"-root", dir, "-repo-url", "https://github.com/o/r"}, args[1:]...)...), &b)
		return b.String(), err
	}
	must := func(args ...string) string {
		t.Helper()
		out, err := call(args...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return out
	}

	sh("init", "-q", "-b", "main")
	write(chartDir+"/Chart.yaml", "apiVersion: v2\nname: patrol-cd\nversion: 0.1.0\nappVersion: \"0.1.0\"\n")
	write(chartDir+"/values.yaml", "image:\n  registry: \"\"\n  repository: patrol-cd/patrol-cd\n")
	write(manifestsDir+"/install.yaml", "image: patrol-cd/patrol-cd:0.1.0\n")
	write(manifestsDir+"/ha/install.yaml", "image: patrol-cd/patrol-cd:0.1.0\n")
	write(manifestsDir+"/kustomization.yaml", "resources: []\n")
	commit("chore: scaffold")
	if got := must("next"); got != "" {
		t.Fatalf("next with only chore = %q", got)
	}
	commit("feat(api): first feature")
	if got := must("next"); got != "0.1.0\n" {
		t.Fatalf("first release = %q, want the chart version", got)
	}
	if got := must("prepare"); got != "0.1.0\n" {
		t.Fatalf("prepare = %q", got)
	}
	commit("chore(release): v0.1.0")
	if got := must("status"); !strings.Contains(got, "pending=true") {
		t.Fatalf("status after the release commit:\n%s", got)
	}
	if got := must("next"); got != "" {
		t.Fatalf("next while pending = %q", got)
	}
	if got := must("notes", "-version", "0.1.0"); !strings.Contains(got, "**api:** first feature") {
		t.Fatalf("notes:\n%s", got)
	}

	out := filepath.Join(t.TempDir(), "stage")
	must("stage", "-version", "0.1.0", "-registry", "ghcr.io/o", "-out", out)
	for name, want := range map[string]string{
		"install.yaml":      "image: ghcr.io/o/patrol-cd/patrol-cd:0.1.0\n",
		"ha-install.yaml":   "image: ghcr.io/o/patrol-cd/patrol-cd:0.1.0\n",
		"chart/values.yaml": "image:\n  registry: \"ghcr.io/o\"\n  repository: patrol-cd/patrol-cd\n",
	} {
		data, err := os.ReadFile(filepath.Join(out, name))
		if err != nil || string(data) != want {
			t.Errorf("%s = %q, %v", name, data, err)
		}
	}
	if _, err := os.Stat(filepath.Join(out, "kustomization.yaml")); err == nil {
		t.Error("kustomization.yaml staged")
	}
	if _, err := call("stage", "-version", "0.2.0", "-registry", "ghcr.io/o", "-out", out); err == nil {
		t.Error("stage of a version the chart does not carry succeeded")
	}

	sh("tag", "v0.1.0")
	sh("checkout", "-q", "-b", "topic")
	commit("fix(ui): a fix")
	commit("feat(cli)!: rename a flag")
	sh("checkout", "-q", "main")
	sh("merge", "-q", "--no-ff", "-m", "Merge pull request #9 from o/topic", "topic")
	if _, err := call("lint", "v0.1.0..HEAD"); err != nil {
		t.Fatalf("lint: %v", err)
	}
	if got := must("next"); got != "0.2.0\n" {
		t.Fatalf("next after a breaking change before 1.0 = %q", got)
	}
	preview := must("preview")
	for _, want := range []string{"## [0.2.0](https://github.com/o/r/compare/v0.1.0...v0.2.0) - 2026-10-20", "### Breaking changes", "([#9](https://github.com/o/r/pull/9))"} {
		if !strings.Contains(preview, want) {
			t.Errorf("preview lacks %q:\n%s", want, preview)
		}
	}
	if _, err := call("prepare", "-version", "0.1.0"); err == nil {
		t.Error("prepare of an existing version succeeded")
	}

	commit("Update things")
	if _, err := call("lint", "v0.1.0..HEAD"); err == nil || !strings.Contains(err.Error(), "Update things") {
		t.Fatalf("lint of a non-conventional subject: %v", err)
	}
}
