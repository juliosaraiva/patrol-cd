package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The release workflow publishes exactly one commit: the release commit, the newest commit on
// main's first-parent history that changed CHANGELOG.md (the merge of the release pull request,
// or the squashed or rebased release commit). Whichever event started the workflow (a CI run of
// some later commit, or a manual run on main's tip), the commit is chosen the same way, and it is
// released only after a CI run of a push of that very commit passed every job of the full tier.
// Releasing the commit that triggered the event instead would tag a commit the changelog does not
// describe (commits after the release commit would land in the tag but in no section, and the next
// release starts from the tag) and could rely on a CI run that skipped the full tier (a
// documentation-only push runs the static checks only).

// requiredJobs are the CI jobs (names as GitHub reports them, .github/workflows/ci.yml) that
// must have succeeded on the release commit; a skipped job (a reduced tier) does not count.
var requiredJobs = []string{
	"Choose tiers",
	"Static checks",
	"Unit tests",
	"Web UI",
	"Image (linux/amd64)",
	"Integration suite",
	"Deploy suite",
	"Kind (install, verify, lifecycle)",
	"CI",
}

// releaseCommit returns the release commit of the release pending at HEAD.
func releaseCommit(ctx context.Context, dir string) (string, error) {
	out, err := git(ctx, dir, "log", "--first-parent", "-1", "--format=%H", "HEAD", "--", changelogFile)
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(out)
	if sha == "" {
		return "", fmt.Errorf("no commit on the first-parent history of HEAD changes %s", changelogFile)
	}
	return sha, nil
}

// ciState is the outcome of the CI check of the release commit.
type ciState string

const (
	ciPassed  ciState = "passed"  // a completed run passed every required job
	ciPending ciState = "pending" // no run yet, or the newest run is not finished
	ciFailed  ciState = "failed"  // the newest run failed, or skipped a required job
)

// ciChecker reads workflow runs and jobs from the GitHub REST API.
type ciChecker struct {
	client   *http.Client
	api      string // https://api.github.com
	repo     string // owner/name
	token    string
	workflow string // workflow file name, ci.yml
	branch   string // main
}

type workflowRun struct {
	ID         int64     `json:"id"`
	HeadSHA    string    `json:"head_sha"`
	HeadBranch string    `json:"head_branch"`
	Event      string    `json:"event"`
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion"`
	CreatedAt  time.Time `json:"created_at"`
	HTMLURL    string    `json:"html_url"`
}

type workflowJob struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

// check reports whether CI passed on a push of sha to the branch. Only the newest run counts: a
// re-run (a new attempt of the same run) replaces the result of the attempt before it.
func (c *ciChecker) check(ctx context.Context, sha string) (ciState, string, error) {
	q := url.Values{"head_sha": {sha}, "event": {"push"}, "branch": {c.branch}, "per_page": {"100"}}
	var runs struct {
		WorkflowRuns []workflowRun `json:"workflow_runs"`
	}
	if err := c.get(ctx, "/repos/"+c.repo+"/actions/workflows/"+url.PathEscape(c.workflow)+"/runs?"+q.Encode(), &runs); err != nil {
		return "", "", err
	}
	var newest *workflowRun
	for i := range runs.WorkflowRuns {
		r := &runs.WorkflowRuns[i]
		// The query filters already; checked again so that a misbehaving filter cannot pick
		// another commit's run.
		if r.HeadSHA != sha || r.Event != "push" || r.HeadBranch != c.branch {
			continue
		}
		if newest == nil || r.CreatedAt.After(newest.CreatedAt) || (r.CreatedAt.Equal(newest.CreatedAt) && r.ID > newest.ID) {
			newest = r
		}
	}
	short := sha
	if len(short) > 7 {
		short = short[:7]
	}
	if newest == nil {
		return ciPending, fmt.Sprintf("no %s run for the push of %s to %s yet", c.workflow, short, c.branch), nil
	}
	if newest.Status != "completed" {
		return ciPending, fmt.Sprintf("%s run %d for %s is %s", c.workflow, newest.ID, short, newest.Status), nil
	}
	if newest.Conclusion != "success" {
		return ciFailed, fmt.Sprintf("%s run %d for %s concluded %q (%s)", c.workflow, newest.ID, short, newest.Conclusion, newest.HTMLURL), nil
	}
	var jobs struct {
		Jobs []workflowJob `json:"jobs"`
	}
	if err := c.get(ctx, fmt.Sprintf("/repos/%s/actions/runs/%d/jobs?filter=latest&per_page=100", c.repo, newest.ID), &jobs); err != nil {
		return "", "", err
	}
	got := map[string]workflowJob{}
	for _, j := range jobs.Jobs {
		got[j.Name] = j
	}
	var bad []string
	for _, name := range requiredJobs {
		j, ok := got[name]
		switch {
		case !ok:
			bad = append(bad, name+": missing")
		case j.Conclusion != "success":
			bad = append(bad, fmt.Sprintf("%s: %s", name, orDefault(j.Conclusion, j.Status)))
		}
	}
	if len(bad) > 0 {
		return ciFailed, fmt.Sprintf("%s run %d for %s did not pass the full tier (%s; %s)", c.workflow, newest.ID, short, strings.Join(bad, ", "), newest.HTMLURL), nil
	}
	return ciPassed, fmt.Sprintf("%s run %d for %s passed every required job (%s)", c.workflow, newest.ID, short, newest.HTMLURL), nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func (c *ciChecker) get(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(c.api, "/")+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d: %.200s", path, resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	return nil
}

// selection is what the release workflow's plan job acts on.
type selection struct {
	status   releaseStatus
	sha      string // the release commit ("" when nothing is pending)
	ci       ciState
	ciReason string
}

func (s selection) release() bool { return s.status.pending() && s.ci == ciPassed }

// selectRelease works out whether to publish and from which commit. HEAD is main's tip.
func (r *repo) selectRelease(ctx context.Context, c *ciChecker) (selection, error) {
	var sel selection
	st, err := r.status(ctx)
	if err != nil {
		return sel, err
	}
	sel.status = st
	if !st.pending() {
		return sel, nil
	}
	if sel.sha, err = releaseCommit(ctx, r.root); err != nil {
		return sel, err
	}
	// The release commit must itself carry the release: the chart version and the section.
	chart, err := git(ctx, r.root, "show", sel.sha+":"+chartDir+"/Chart.yaml")
	if err != nil {
		return sel, err
	}
	if v, err := chartVersion(chart); err != nil || v != st.current {
		return sel, errors.Join(err, fmt.Errorf("release commit %.7s has chart version %s, not %s", sel.sha, v, st.current))
	}
	changelog, err := git(ctx, r.root, "show", sel.sha+":"+changelogFile)
	if err != nil {
		return sel, err
	}
	if _, _, ok := findSection(changelog, st.current); !ok {
		return sel, fmt.Errorf("release commit %.7s has no %s section for %s", sel.sha, changelogFile, st.current)
	}
	if c == nil {
		return sel, errors.New("no GitHub API access configured")
	}
	sel.ci, sel.ciReason, err = c.check(ctx, sel.sha)
	return sel, err
}
