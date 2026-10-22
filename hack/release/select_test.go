package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGitHub serves the two Actions endpoints the CI check reads: workflow runs by head SHA
// and the jobs of a run.
type fakeGitHub struct {
	mu   sync.Mutex
	runs []workflowRun
	jobs map[int64][]workflowJob
	// queries records the query strings of the runs listing.
	queries []string
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer test-token" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.URL.Path == "/repos/o/r/actions/workflows/ci.yml/runs" {
		f.queries = append(f.queries, r.URL.RawQuery)
		// Like GitHub: filter by head_sha only, so that the client's own checks of event and
		// branch are exercised.
		var out []workflowRun
		for _, run := range f.runs {
			if run.HeadSHA == r.URL.Query().Get("head_sha") {
				out = append(out, run)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(out), "workflow_runs": out})
		return
	}
	var id int64
	if _, err := fmt.Sscanf(r.URL.Path, "/repos/o/r/actions/runs/%d/jobs", &id); err == nil {
		_ = json.NewEncoder(w).Encode(map[string]any{"jobs": f.jobs[id]})
		return
	}
	http.NotFound(w, r)
}

func allJobs(conclusion string) []workflowJob {
	var jobs []workflowJob
	for _, n := range requiredJobs {
		jobs = append(jobs, workflowJob{Name: n, Status: "completed", Conclusion: conclusion})
	}
	return jobs
}

// staticOnly is a documentation-only push: the full tier is skipped and the run still succeeds.
func staticOnly() []workflowJob {
	jobs := allJobs("skipped")
	for i := range jobs {
		switch jobs[i].Name {
		case "Choose tiers", "Static checks", "CI":
			jobs[i].Conclusion = "success"
		}
	}
	return jobs
}

func TestCICheck(t *testing.T) {
	t.Parallel()
	const sha = "1111111111111111111111111111111111111111"
	t0 := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	run := func(id int64, at time.Time, status, conclusion string) workflowRun {
		return workflowRun{ID: id, HeadSHA: sha, HeadBranch: "main", Event: "push", Status: status, Conclusion: conclusion, CreatedAt: at}
	}
	tests := []struct {
		name string
		runs []workflowRun
		jobs map[int64][]workflowJob
		want ciState
		why  string
	}{
		{name: "no run yet", want: ciPending, why: "no ci.yml run"},
		{name: "in progress", runs: []workflowRun{run(1, t0, "in_progress", "")}, want: ciPending, why: "in_progress"},
		{name: "failed", runs: []workflowRun{run(1, t0, "completed", "failure")}, want: ciFailed, why: `"failure"`},
		{name: "full tier passed", runs: []workflowRun{run(1, t0, "completed", "success")}, jobs: map[int64][]workflowJob{1: allJobs("success")}, want: ciPassed},
		{name: "static only (documentation push) is not enough", runs: []workflowRun{run(1, t0, "completed", "success")},
			jobs: map[int64][]workflowJob{1: staticOnly()}, want: ciFailed, why: "Integration suite: skipped"},
		{name: "a required job is missing", runs: []workflowRun{run(1, t0, "completed", "success")},
			jobs: map[int64][]workflowJob{1: allJobs("success")[1:]}, want: ciFailed, why: "Choose tiers: missing"},
		{name: "the newest run counts, not an older success", runs: []workflowRun{run(1, t0, "completed", "success"), run(2, t0.Add(time.Hour), "completed", "failure")},
			jobs: map[int64][]workflowJob{1: allJobs("success")}, want: ciFailed, why: "run 2"},
		{name: "a newer success replaces an older failure", runs: []workflowRun{run(1, t0, "completed", "failure"), run(2, t0.Add(time.Hour), "completed", "success")},
			jobs: map[int64][]workflowJob{2: allJobs("success")}, want: ciPassed, why: "run 2"},
		{name: "pull request and other-branch runs of the same commit are ignored", runs: []workflowRun{
			{ID: 3, HeadSHA: sha, HeadBranch: "release/next", Event: "pull_request", Status: "completed", Conclusion: "success", CreatedAt: t0},
			{ID: 4, HeadSHA: sha, HeadBranch: "other", Event: "push", Status: "completed", Conclusion: "success", CreatedAt: t0},
		}, jobs: map[int64][]workflowJob{3: allJobs("success"), 4: allJobs("success")}, want: ciPending, why: "no ci.yml run"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gh := &fakeGitHub{runs: tc.runs, jobs: tc.jobs}
			srv := httptest.NewServer(gh)
			defer srv.Close()
			c := &ciChecker{client: srv.Client(), api: srv.URL, repo: "o/r", token: "test-token", workflow: "ci.yml", branch: "main"}
			got, why, err := c.check(context.Background(), sha)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want || !strings.Contains(why, tc.why) {
				t.Fatalf("check = %s (%s), want %s containing %q", got, why, tc.want, tc.why)
			}
			if q := gh.queries[0]; !strings.Contains(q, "head_sha="+sha) || !strings.Contains(q, "event=push") || !strings.Contains(q, "branch=main") {
				t.Fatalf("runs query %q", q)
			}
		})
	}

	srv := httptest.NewServer(&fakeGitHub{})
	defer srv.Close()
	c := &ciChecker{client: srv.Client(), api: srv.URL, repo: "o/r", token: "wrong", workflow: "ci.yml", branch: "main"}
	if _, _, err := c.check(context.Background(), sha); err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("API error not reported: %v", err)
	}
}

// TestRequiredJobsMatchCIWorkflow fails when a CI job is renamed or added without updating
// requiredJobs: a renamed job would read as "missing" and block every release, an added one
// would not be required.
func TestRequiredJobsMatchCIWorkflow(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../../.github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	// Job names: "    name: X" right after a two-space job key.
	re := regexp.MustCompile(`(?m)^  [a-z][a-z0-9_-]*:\n    name: (.+)$`)
	var names []string
	for _, m := range re.FindAllStringSubmatch(string(data), -1) {
		names = append(names, strings.TrimSpace(m[1]))
	}
	if strings.Join(names, "|") != strings.Join(sortedLike(requiredJobs, names), "|") || len(names) != len(requiredJobs) {
		t.Fatalf("ci.yml jobs %q, requiredJobs %q", names, requiredJobs)
	}
}

// sortedLike returns want in the order of have (for a set comparison that prints both lists).
func sortedLike(want, have []string) []string {
	in := map[string]bool{}
	for _, w := range want {
		in[w] = true
	}
	var out []string
	for _, h := range have {
		if in[h] {
			out = append(out, h)
		}
	}
	return out
}

// TestSelectRelease covers both events of release.yml through the one selection they share: a
// CI run of a later commit and a manual run on main's tip both resolve to the release commit,
// and only a full-tier pass on that commit releases it.
func TestSelectRelease(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	dir := t.TempDir()
	r := newScratchRepo(t, dir)
	r.write(chartDir+"/Chart.yaml", "apiVersion: v2\nname: patrol-cd\nversion: 0.1.0\nappVersion: \"0.1.0\"\n")
	r.commit("feat: first")
	r.git("tag", "v0.1.0")
	r.commit("feat(ui): second")

	// The release pull request: prepared on a branch, merged with a merge commit.
	r.git("checkout", "-q", "-b", "release/next")
	var out bytes.Buffer
	if err := run(ctx, []string{"prepare", "-root", dir, "-repo-url", "https://github.com/o/r"}, &out); err != nil || out.String() != "0.2.0\n" {
		t.Fatalf("prepare = %q, %v", out.String(), err)
	}
	r.commit("chore(release): v0.2.0")
	r.git("checkout", "-q", "main")
	r.git("merge", "-q", "--no-ff", "-m", "Merge pull request #5 from o/release/next", "release/next")
	releaseSHA := r.head()
	// Pushed to main before CI of the release commit finished: a docs change and a feature.
	// Neither is in the 0.2.0 section, so neither may be tagged v0.2.0.
	r.write("docs/x.md", "x\n")
	r.commit("docs: a note")
	r.commit("feat(cli): sneaked in")
	tip := r.head()

	gh := &fakeGitHub{jobs: map[int64][]workflowJob{}}
	srv := httptest.NewServer(gh)
	defer srv.Close()
	c := &ciChecker{client: srv.Client(), api: srv.URL, repo: "o/r", token: "test-token", workflow: "ci.yml", branch: "main"}
	at := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	addRun := func(id int64, sha string, jobs []workflowJob) {
		gh.mu.Lock()
		defer gh.mu.Unlock()
		gh.runs = append(gh.runs, workflowRun{ID: id, HeadSHA: sha, HeadBranch: "main", Event: "push", Status: "completed", Conclusion: "success", CreatedAt: at.Add(time.Duration(id) * time.Minute)})
		gh.jobs[id] = jobs
	}
	repoAt := &repo{root: dir}
	sel := func() selection {
		t.Helper()
		s, err := repoAt.selectRelease(ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}

	// The later commits' CI passed (the docs push only ran the static tier); the release
	// commit's has not run: nothing is released, and the release commit is the one checked.
	addRun(1, tip, allJobs("success"))
	if s := sel(); s.sha != releaseSHA || s.ci != ciPending || s.release() {
		t.Fatalf("before the release commit's CI: sha=%.7s (want %.7s) ci=%s release=%t", s.sha, releaseSHA, s.ci, s.release())
	}
	// A static-only pass on the release commit is not enough.
	addRun(2, releaseSHA, staticOnly())
	if s := sel(); s.ci != ciFailed || s.release() {
		t.Fatalf("static-only run: ci=%s release=%t (%s)", s.ci, s.release(), s.ciReason)
	}
	// A full-tier pass releases the release commit, not main's tip.
	addRun(3, releaseSHA, allJobs("success"))
	if s := sel(); !s.release() || s.sha != releaseSHA {
		t.Fatalf("full pass: sha=%.7s release=%t (%s)", s.sha, s.release(), s.ciReason)
	}

	// Tagged: nothing pending, no API call needed.
	r.git("tag", "v0.2.0", releaseSHA)
	if s, err := repoAt.selectRelease(ctx, nil); err != nil || s.status.pending() || s.release() || s.sha != "" {
		t.Fatalf("after the tag: %+v, %v", s, err)
	}
}

// TestSelectReleaseSquashAndReprepare: a squash-merged release pull request is its own release
// commit; and when it fails CI for a real defect, the fix plus an explicit re-prepare of the
// same version moves the release commit to the new merge.
func TestSelectReleaseSquashAndReprepare(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	dir := t.TempDir()
	r := newScratchRepo(t, dir)
	r.write(chartDir+"/Chart.yaml", "apiVersion: v2\nname: patrol-cd\nversion: 0.1.0\nappVersion: \"0.1.0\"\n")
	r.commit("feat: first")
	call := func(args ...string) string {
		t.Helper()
		var b bytes.Buffer
		if err := run(ctx, append(args[:1:1], append([]string{"-root", dir, "-repo-url", "https://github.com/o/r"}, args[1:]...)...), &b); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return b.String()
	}
	call("prepare")
	r.commit("chore(release): v0.1.0 (#6)") // squash merge
	first := r.head()
	if sha, err := releaseCommit(ctx, dir); err != nil || sha != first {
		t.Fatalf("releaseCommit = %.7s, %v; want %.7s", sha, err, first)
	}
	if got := call("next"); got != "" {
		t.Fatalf("next while pending = %q", got)
	}

	// CI failed on it for a real defect; the fix lands, then the same version is prepared again.
	r.commit("fix(api): the defect")
	if got := call("next", "-version", "0.1.0"); got != "0.1.0\n" {
		t.Fatalf("explicit re-prepare of the pending version = %q", got)
	}
	call("prepare", "-version", "0.1.0")
	data, err := os.ReadFile(filepath.Join(dir, changelogFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "## [0.1.0]") != 1 || !strings.Contains(string(data), "the defect") {
		t.Fatalf("re-prepared changelog:\n%s", data)
	}
	r.commit("chore(release): v0.1.0 (#7)")
	if sha, err := releaseCommit(ctx, dir); err != nil || sha != r.head() || sha == first {
		t.Fatalf("releaseCommit after re-prepare = %.7s, %v; want %.7s", sha, err, r.head())
	}
}

type scratchRepo struct {
	t   *testing.T
	dir string
}

func newScratchRepo(t *testing.T, dir string) *scratchRepo {
	r := &scratchRepo{t: t, dir: dir}
	r.git("init", "-q", "-b", "main")
	return r
}

func (r *scratchRepo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_AUTHOR_DATE=2026-10-20T10:00:00Z", "GIT_COMMITTER_DATE=2026-10-20T10:00:00Z")
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *scratchRepo) write(path, content string) {
	r.t.Helper()
	p := filepath.Join(r.dir, path)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *scratchRepo) commit(msg string) {
	r.t.Helper()
	r.git("add", "-A")
	r.git("commit", "-q", "--allow-empty", "-m", msg)
}

func (r *scratchRepo) head() string { return r.git("rev-parse", "HEAD") }
