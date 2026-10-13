package github_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/github"
)

func TestAPIURLs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in, base, upload string
		wantErr          bool
	}{
		{"", "https://api.github.com/", "https://uploads.github.com/", false},
		{"https://api.github.com", "https://api.github.com/", "https://uploads.github.com/", false},
		{"https://api.github.com/", "https://api.github.com/", "https://uploads.github.com/", false},
		{"https://ghe.example.com", "https://ghe.example.com/api/v3/", "https://ghe.example.com/api/uploads/", false},
		{"https://ghe.example.com/", "https://ghe.example.com/api/v3/", "https://ghe.example.com/api/uploads/", false},
		{"https://ghe.example.com/api/v3", "https://ghe.example.com/api/v3/", "https://ghe.example.com/api/uploads/", false},
		{"https://ghe.example.com/api/v3/", "https://ghe.example.com/api/v3/", "https://ghe.example.com/api/uploads/", false},
		{"https://example.com/github/api/v3", "https://example.com/github/api/v3/", "https://example.com/github/api/uploads/", false},
		{"https://api.acme.ghe.com", "https://api.acme.ghe.com/", "https://uploads.acme.ghe.com/", false},
		{"https://api.acme.ghe.com/", "https://api.acme.ghe.com/", "https://uploads.acme.ghe.com/", false},
		{"https://API.example.com/github", "https://API.example.com/github/", "https://uploads.example.com/github/", false},
		{"https://gh.api.example.com", "https://gh.api.example.com/", "https://gh.api.example.com/", false},
		{"http://ghe.internal:8080", "http://ghe.internal:8080/api/v3/", "http://ghe.internal:8080/api/uploads/", false},
		{"ghe.example.com", "", "", true},
		{"ftp://ghe.example.com", "", "", true},
		{"https://user:pass@ghe.example.com", "", "", true},
		{"https://ghe.example.com/?x=1", "", "", true},
		{"https://ghe.example.com/#frag", "", "", true},
		{"::", "", "", true},
	}
	for _, tc := range tests {
		base, upload, err := github.APIURLs(tc.in)
		if (err != nil) != tc.wantErr || base != tc.base || upload != tc.upload {
			t.Errorf("APIURLs(%q) = %q, %q, %v", tc.in, base, upload, err)
		}
		if err != nil && strings.Contains(err.Error(), "pass") {
			t.Errorf("error leaks credentials: %v", err)
		}
	}
}

// TestAPIURLsProperties checks that normalization is idempotent (a derived base URL
// configured again yields itself), always ends with "/", keeps scheme and host, and that
// "/api/v3" is appended exactly for plain GitHub Enterprise Server roots.
func TestAPIURLsProperties(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		scheme := rapid.SampledFrom([]string{"http", "https"}).Draw(t, "scheme")
		host := rapid.SampledFrom([]string{"ghe.example.com", "api.example.com", "api.acme.ghe.com", "gh.api.example.com", "ghe.internal:8443", "API.Example.com"}).Draw(t, "host")
		segs := rapid.SliceOfN(rapid.SampledFrom([]string{"github", "api", "v3", "x-y", "prefix"}), 0, 3).Draw(t, "segs")
		path := ""
		if len(segs) > 0 {
			path = "/" + strings.Join(segs, "/")
		}
		in := scheme + "://" + host + path + rapid.SampledFrom([]string{"", "/"}).Draw(t, "slash")
		base, upload, err := github.APIURLs(in)
		if err != nil {
			t.Fatalf("APIURLs(%q): %v", in, err)
		}
		if !strings.HasSuffix(base, "/") || !strings.HasSuffix(upload, "/") {
			t.Fatalf("APIURLs(%q) = %q, %q: missing trailing slash", in, base, upload)
		}
		if !strings.HasPrefix(base, scheme+"://"+host+"/") {
			t.Fatalf("APIURLs(%q) base %q changed scheme or host", in, base)
		}
		again, upload2, err := github.APIURLs(base)
		if err != nil || again != base || upload2 != upload {
			t.Fatalf("not idempotent: %q -> %q/%q -> %q/%q (%v)", in, base, upload, again, upload2, err)
		}
		lower := strings.ToLower(host)
		plainGHES := !strings.HasPrefix(lower, "api.") && !strings.Contains(lower, ".api.") && !strings.HasSuffix(path, "/api/v3")
		if plainGHES != (base == scheme+"://"+host+path+"/api/v3/") {
			t.Fatalf("APIURLs(%q) = %q: /api/v3 handling wrong (plain GHES root: %v)", in, base, plainGHES)
		}
	})
}

func TestCapabilitiesByAuthentication(t *testing.T) {
	t.Parallel()
	s := newSeed(t, "")
	read := []scm.Capability{scm.CapabilityRepositories, scm.CapabilityBranches, scm.CapabilityRevisions, scm.CapabilityPaths, scm.CapabilityPullRequests, scm.CapabilityWebhooks}
	tests := []struct {
		auth authKind
		want []scm.Capability
	}{
		{anonymous, read},
		{pat, append(slices.Clone(read), scm.CapabilityCommitStatuses, scm.CapabilityCredentials)},
		{appExplicit, append(slices.Clone(read), scm.CapabilityCommitStatuses, scm.CapabilityCredentials, scm.CapabilityCheckRuns, scm.CapabilityInstallations)},
	}
	for _, tc := range tests {
		p := newProvider(t, s.config(t, tc.auth))
		info := p.Info()
		if !slices.Equal(info.Capabilities, tc.want) {
			t.Errorf("auth %d: capabilities %v, want %v", tc.auth, info.Capabilities, tc.want)
		}
		if info.Type != github.Type || info.Name != "test" || info.ContractVersion != scm.ContractVersion || info.APIURL != strings.TrimSuffix(s.apiURL(), "/") {
			t.Errorf("info = %+v", info)
		}
	}
	// Direct method calls on an undeclared capability fail loudly too.
	anon := newProvider(t, s.config(t, anonymous))
	if _, err := anon.Token(t.Context()); !errors.Is(err, scm.ErrUnsupported) {
		t.Fatalf("anonymous Token: %v", err)
	}
	before := len(s.srv.Requests())
	if err := anon.SetCommitStatus(t.Context(), s.platformRepo, sha(1), scm.CommitStatus{State: scm.CommitStateSuccess, Context: "c"}); !errors.Is(err, scm.ErrUnsupported) {
		t.Fatalf("anonymous SetCommitStatus: %v", err)
	}
	if _, err := anon.FindInstallation(t.Context(), "acme"); !errors.Is(err, scm.ErrUnsupported) {
		t.Fatalf("anonymous FindInstallation: %v", err)
	}
	if after := len(s.srv.Requests()); after != before {
		t.Fatalf("undeclared capability reached the API (%d requests)", after-before)
	}
	patP := newProvider(t, s.config(t, pat))
	if _, err := patP.UpsertCheckRun(t.Context(), s.platformRepo, sha(1), scm.CheckRun{Name: "n", ExternalID: "e", Status: scm.CheckStatusQueued}); !errors.Is(err, scm.ErrUnsupported) {
		t.Fatalf("PAT UpsertCheckRun: %v", err)
	}
	if _, err := patP.ListInstallations(t.Context()); !errors.Is(err, scm.ErrUnsupported) {
		t.Fatalf("PAT ListInstallations: %v", err)
	}
}

func TestNewProviderValidation(t *testing.T) {
	t.Parallel()
	_, pemKey := appKey(t)
	tests := []struct {
		name string
		cfg  scm.Config
	}{
		{"bad url", scm.Config{APIURL: "ftp://x"}},
		{"app without id", scm.Config{GitHubApp: &scm.GitHubAppConfig{PrivateKey: pemKey}}},
		{"app without key", scm.Config{GitHubApp: &scm.GitHubAppConfig{AppID: 1}}},
		{"app with garbage key", scm.Config{GitHubApp: &scm.GitHubAppConfig{AppID: 1, PrivateKey: []byte("-----BEGIN RSA PRIVATE KEY-----\nc2VjcmV0LWtleS1ieXRlcw==\n-----END RSA PRIVATE KEY-----\n")}}},
		{"negative installation", scm.Config{GitHubApp: &scm.GitHubAppConfig{AppID: 1, InstallationID: -1, PrivateKey: pemKey}}},
		{"bad CA", scm.Config{CAData: []byte("nope")}},
	}
	for _, tc := range tests {
		_, err := github.NewProvider(t.Context(), tc.cfg)
		if scm.KindOf(err) != scm.KindInvalid {
			t.Errorf("%s: err = %v, want KindInvalid", tc.name, err)
		}
		if err != nil && strings.Contains(err.Error(), "c2VjcmV0") {
			t.Errorf("%s: error leaks key material: %v", tc.name, err)
		}
	}
}

func TestRegisterAndFactory(t *testing.T) {
	t.Parallel()
	s := newSeed(t, "/api/v3")
	r := scm.NewRegistry()
	if err := github.Register(r); err != nil {
		t.Fatal(err)
	}
	p, err := r.New(t.Context(), s.config(t, pat))
	if err != nil {
		t.Fatal(err)
	}
	bl, err := scm.As[scm.BranchLister](p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bl.ListBranches(t.Context(), s.platformRepo, true); err != nil {
		t.Fatal(err)
	}
}

func repoNames(rs []scm.Repository) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Name)
	}
	slices.Sort(out)
	return out
}

func TestListRepositories(t *testing.T) {
	t.Parallel()
	s := newSeed(t, "/api/v3")
	p := newProvider(t, s.config(t, pat))
	ctx := t.Context()

	all, err := p.ListRepositories(ctx, scm.RepoQuery{Organization: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	want := append(slices.Clone(s.svcNames), "legacy", "platform") // archived included by default, empty omitted
	slices.Sort(want)
	if got := repoNames(all); !slices.Equal(got, want) {
		t.Fatalf("repos = %v, want %v", got, want)
	}
	// 10 repositories at 3 per page → 4 listing requests.
	if n := s.srv.CountRequests(http.MethodGet, "/orgs/acme/repos"); n != 4 {
		t.Fatalf("listing requests = %d, want 4", n)
	}
	for _, r := range all {
		if r.Name != "platform" {
			continue
		}
		if r.Organization != "acme" || r.FullName != "acme/platform" || r.SHA != sha(1) || r.Branch != "main" ||
			r.URL != "git@github.example:acme/platform.git" || r.HTTPSURL != "https://github.example/acme/platform.git" ||
			r.RepositoryID != int64(42) || !r.Private || !slices.Equal(r.Labels, []string{"platform"}) {
			t.Fatalf("platform = %+v", r)
		}
	}

	https, err := p.ListRepositories(ctx, scm.RepoQuery{Organization: "acme", CloneProtocol: scm.CloneProtocolHTTPS, Topics: []string{"team-a"}, Archived: scm.ArchivedExclude})
	if err != nil {
		t.Fatal(err)
	}
	if got := repoNames(https); !slices.Equal(got, s.svcNames) {
		t.Fatalf("topic filter = %v", got)
	}
	if !strings.HasPrefix(https[0].URL, "https://") {
		t.Fatalf("https clone URL = %q", https[0].URL)
	}

	_, err = p.ListRepositories(ctx, scm.RepoQuery{Organization: "acme", CloneProtocol: "gopher"})
	if scm.KindOf(err) != scm.KindInvalid || !strings.Contains(err.Error(), "unknown clone protocol for GitHub gopher") {
		t.Fatalf("clone protocol error = %v", err)
	}

	// User accounts fall back to /users/{user}/repos.
	user, err := p.ListRepositories(ctx, scm.RepoQuery{Organization: "octo"})
	if err != nil || len(user) != 1 || user[0].Name != "dotfiles" || user[0].Branch != "trunk" || user[0].SHA != sha(500) {
		t.Fatalf("user repos = %+v, %v", user, err)
	}

	branches, err := p.ListRepositories(ctx, scm.RepoQuery{Organization: "acme", AllBranches: true, Match: func(r scm.Repository) bool { return r.Name == "platform" }})
	if err != nil || len(branches) != len(s.branches) {
		t.Fatalf("all branches = %d, %v", len(branches), err)
	}
	for _, b := range branches {
		if s.branches[b.Branch] != b.SHA {
			t.Fatalf("branch %s sha %s", b.Branch, b.SHA)
		}
	}

	if _, err := p.ListRepositories(ctx, scm.RepoQuery{Organization: "bad/org"}); scm.KindOf(err) != scm.KindInvalid {
		t.Fatalf("invalid organization: %v", err)
	}
}

func TestPrivateRepositoriesNeedCredentials(t *testing.T) {
	t.Parallel()
	s := newSeed(t, "")
	anon := newProvider(t, s.config(t, anonymous))
	repos, err := anon.ListRepositories(t.Context(), scm.RepoQuery{Organization: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(repoNames(repos), "platform") {
		t.Fatal("private repository visible anonymously")
	}
	if _, err := anon.ListPullRequests(t.Context(), s.platformRepo, scm.PRQuery{}); !scm.IsNotFound(err) {
		t.Fatalf("private PRs anonymously: %v", err)
	}
	for _, r := range s.srv.Requests() {
		if r.Auth != "none" {
			t.Fatalf("anonymous provider sent credentials: %+v", r)
		}
	}
}

func TestListBranchesAndRevisions(t *testing.T) {
	t.Parallel()
	s := newSeed(t, "")
	p := newProvider(t, s.config(t, pat))
	ctx := t.Context()
	def, err := p.ListBranches(ctx, s.platformRepo, false)
	if err != nil || len(def) != 1 || def[0].Name != "main" || !def[0].Protected {
		t.Fatalf("default branch = %+v, %v", def, err)
	}
	empty, err := p.ListBranches(ctx, scm.RepoRef{Owner: "acme", Name: "empty"}, false)
	if err != nil || len(empty) != 0 || empty == nil {
		t.Fatalf("empty repository = %#v, %v", empty, err)
	}
	if _, err := p.ListBranches(ctx, scm.RepoRef{Owner: "acme", Name: "../x"}, true); scm.KindOf(err) != scm.KindInvalid {
		t.Fatalf("traversal: %v", err)
	}

	tests := []struct {
		ref     string
		want    string
		errKind scm.ErrorKind
	}{
		{"main", sha(1), ""},
		{"refs/heads/feature-a", sha(2), ""},
		{"v1.2.0", sha(4), ""},
		{"refs/tags/v1.2.0", sha(4), ""},
		{sha(5), sha(5), ""},
		{"nope", "", scm.KindNotFound},
		{"", "", scm.KindInvalid},
		{"../../orgs", "", scm.KindInvalid},
	}
	for _, tc := range tests {
		got, err := p.ResolveRevision(ctx, s.platformRepo, tc.ref)
		if tc.errKind != "" {
			if scm.KindOf(err) != tc.errKind {
				t.Errorf("ResolveRevision(%q) err = %v, want %s", tc.ref, err, tc.errKind)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("ResolveRevision(%q) = %q, %v", tc.ref, got, err)
		}
	}
}

func TestPathExists(t *testing.T) {
	t.Parallel()
	s := newSeed(t, "/api/v3")
	p := newProvider(t, s.config(t, pat))
	ctx := t.Context()
	tests := []struct {
		rev, path string
		want      bool
		errKind   scm.ErrorKind
	}{
		{"main", "apps/a/values.yaml", true, ""},
		{"main", "apps/a", true, ""},
		{"main", "/apps/a/", true, ""},
		{"main", "./apps//a", true, ""},
		{"main", "", true, ""},
		{"main", "dir with space/f#1.yaml", true, ""},
		{"main", "apps/b", false, ""},
		{"feature-a", "apps/b", true, ""},
		{sha(2), "apps/b/values.yaml", true, ""},
		{"no-such-ref", "apps/a", false, ""},
		{"main", "apps/../../secrets", false, scm.KindInvalid},
	}
	for _, tc := range tests {
		got, err := p.PathExists(ctx, s.platformRepo, tc.rev, tc.path)
		if tc.errKind != "" {
			if scm.KindOf(err) != tc.errKind {
				t.Errorf("PathExists(%q,%q) err = %v", tc.rev, tc.path, err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("PathExists(%q,%q) = %v, %v; want %v", tc.rev, tc.path, got, err, tc.want)
		}
	}
	if n := s.srv.CountRequests(http.MethodGet, "/repos/acme/platform/contents"); n != 0 {
		t.Fatalf("PathExists should use HEAD, saw %d GETs", n)
	}
	// A server that rejects HEAD (405) triggers a GET fallback.
	s.srv.FailNext(1, http.StatusMethodNotAllowed)
	if ok, err := p.PathExists(ctx, s.platformRepo, "main", "README.md"); err != nil || !ok {
		t.Fatalf("HEAD fallback: %v %v", ok, err)
	}
	if n := s.srv.CountRequests(http.MethodGet, "/repos/acme/platform/contents/README.md"); n != 1 {
		t.Fatalf("fallback GETs = %d", n)
	}
	// A missing repository is reported as a missing path, as on GitHub.
	if ok, err := p.PathExists(ctx, scm.RepoRef{Owner: "acme", Name: "nope"}, "main", "x"); err != nil || ok {
		t.Fatalf("missing repo: %v %v", ok, err)
	}
}

func prNums(prs []scm.PullRequest) []int64 {
	var out []int64
	for _, p := range prs {
		out = append(out, p.Number)
	}
	return out
}

func TestListPullRequests(t *testing.T) {
	t.Parallel()
	s := newSeed(t, "")
	p := newProvider(t, s.config(t, pat))
	ctx := t.Context()
	tests := []struct {
		name string
		q    scm.PRQuery
		want []int64
	}{
		{"open", scm.PRQuery{}, []int64{1, 2, 3, 4}},
		{"one label", scm.PRQuery{Labels: []string{"preview"}}, []int64{1, 2}},
		{"all labels required", scm.PRQuery{Labels: []string{"preview", "team-a"}}, []int64{2}},
		{"labels are case sensitive", scm.PRQuery{Labels: []string{"Preview"}}, nil},
		{"target branch", scm.PRQuery{TargetBranch: "release-1"}, []int64{3}},
		{"closed", scm.PRQuery{State: scm.PRStateClosed}, []int64{5}},
		{"merged", scm.PRQuery{State: scm.PRStateMerged}, []int64{5}},
		{"all", scm.PRQuery{State: scm.PRStateAll}, []int64{1, 2, 3, 4, 5}},
	}
	for _, tc := range tests {
		got, err := p.ListPullRequests(ctx, s.platformRepo, tc.q)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !slices.Equal(prNums(got), tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, prNums(got), tc.want)
		}
	}
	got, _ := p.ListPullRequests(ctx, s.platformRepo, scm.PRQuery{Labels: []string{"team-a"}})
	if len(got) != 1 {
		t.Fatalf("got %v", got)
	}
	pr := got[0]
	if pr.Title != "Add b" || pr.Branch != "feature-b" || pr.TargetBranch != "main" || pr.HeadSHA != sha(3) || pr.Author != "bob" ||
		!pr.Draft || pr.State != "open" || pr.URL != "https://github.example/acme/platform/pull/2" {
		t.Fatalf("pr = %+v", pr)
	}
	// 4 open PRs at 3 per page → 2 requests for the first query (Link pagination).
	if _, err := p.ListPullRequests(ctx, s.platformRepo, scm.PRQuery{State: "bogus"}); scm.KindOf(err) != scm.KindInvalid {
		t.Fatalf("bogus state: %v", err)
	}
	_, err := p.ListPullRequests(ctx, scm.RepoRef{Owner: "acme", Name: "missing"}, scm.PRQuery{})
	if !scm.IsNotFound(err) {
		t.Fatalf("missing repository: %v", err)
	}
	var se *scm.Error
	if !errors.As(err, &se) || se.StatusCode != http.StatusNotFound || se.Op != "ListPullRequests" || se.Provider != github.Type {
		t.Fatalf("error detail = %+v", se)
	}
}

func TestSetCommitStatus(t *testing.T) {
	t.Parallel()
	s := newSeed(t, "/api/v3")
	p := newProvider(t, s.config(t, pat))
	ctx := t.Context()
	long := strings.Repeat("é", 200)
	err := p.SetCommitStatus(ctx, s.platformRepo, sha(1), scm.CommitStatus{State: scm.CommitStatePending, Context: "continuous-delivery/app", Description: long, TargetURL: "https://patrol.example/applications/app"})
	if err != nil {
		t.Fatal(err)
	}
	got := s.srv.Statuses("acme", "platform", sha(1))
	if len(got) != 1 || got[0].State != "pending" || got[0].Context != "continuous-delivery/app" || got[0].Description != strings.Repeat("é", 140) {
		t.Fatalf("statuses = %+v", got)
	}
	before := len(s.srv.Requests())
	for _, bad := range []string{"main", "abc", sha(1) + "x"} {
		if err := p.SetCommitStatus(ctx, s.platformRepo, bad, scm.CommitStatus{State: scm.CommitStateSuccess, Context: "c"}); scm.KindOf(err) != scm.KindInvalid {
			t.Fatalf("sha %q: %v", bad, err)
		}
	}
	if err := p.SetCommitStatus(ctx, s.platformRepo, sha(1), scm.CommitStatus{State: "great", Context: "c"}); scm.KindOf(err) != scm.KindInvalid {
		t.Fatal(err)
	}
	if after := len(s.srv.Requests()); after != before {
		t.Fatalf("invalid input reached the API (%d requests)", after-before)
	}
	err = p.SetCommitStatus(ctx, s.platformRepo, sha(77), scm.CommitStatus{State: scm.CommitStateSuccess, Context: "c"})
	if scm.KindOf(err) != scm.KindInvalid {
		t.Fatalf("unknown commit: %v", err)
	}
}

func TestUpsertCheckRun(t *testing.T) {
	t.Parallel()
	s := newSeed(t, "")
	p := newProvider(t, s.config(t, appExplicit))
	ctx := t.Context()
	run := scm.CheckRun{Name: "continuous-delivery/app", ExternalID: "app-uid-1", Status: scm.CheckStatusInProgress, Title: "Syncing app", DetailsURL: "https://patrol.example/app"}
	id, err := p.UpsertCheckRun(ctx, s.platformRepo, sha(1), run)
	if err != nil {
		t.Fatal(err)
	}
	run.Status, run.Conclusion, run.Summary, run.Text = scm.CheckStatusCompleted, scm.CheckConclusionFailure, "Sync failed", strings.Repeat("x", 70000)
	id2, err := p.UpsertCheckRun(ctx, s.platformRepo, sha(1), run)
	if err != nil || id2 != id {
		t.Fatalf("update: id %d vs %d, %v", id2, id, err)
	}
	runs := s.srv.CheckRuns(sha(1))
	if len(runs) != 1 {
		t.Fatalf("runs = %+v", runs)
	}
	r := runs[0]
	if r.Status != "completed" || r.Conclusion != "failure" || r.CompletedAt == "" || r.Summary != "Sync failed" || r.Title != "Syncing app" || r.AppID != appID {
		t.Fatalf("run = %+v", r)
	}
	if s.srv.CountRequests(http.MethodPost, "/repos/acme/platform/check-runs") != 1 || s.srv.CountRequests(http.MethodPatch, "/repos/acme/platform/check-runs/") != 1 {
		t.Fatalf("requests = %+v", s.srv.Requests())
	}
	for _, rq := range s.srv.Requests() {
		if strings.Contains(rq.Path, "/check-runs") && rq.Method == http.MethodGet && !strings.Contains(rq.RawQuery, "app_id=1234") {
			t.Fatalf("lookup not scoped to the App: %+v", rq)
		}
	}
}

func TestResponseSizeAndPageCaps(t *testing.T) {
	t.Parallel()
	s := newSeed(t, "")
	cfg := s.config(t, pat)
	cfg.MaxResponseBytes = 512
	p := newProvider(t, cfg)
	_, err := p.ListRepositories(t.Context(), scm.RepoQuery{Organization: "acme"})
	if !errors.Is(err, scm.ErrResponseTooLarge) || scm.KindOf(err) != scm.KindInvalid {
		t.Fatalf("size cap: %v", err)
	}
	cfg = s.config(t, pat)
	cfg.MaxPages = 2
	p = newProvider(t, cfg)
	_, err = p.ListRepositories(t.Context(), scm.RepoQuery{Organization: "acme"})
	if !errors.Is(err, scm.ErrPageLimit) || scm.KindOf(err) != scm.KindInvalid {
		t.Fatalf("page cap: %v", err)
	}
}

func TestErrorMapping(t *testing.T) {
	t.Parallel()
	tests := []struct {
		status int
		kind   scm.ErrorKind
		calls  int // requests made (5xx retried once with testRetry)
	}{
		{http.StatusBadRequest, scm.KindInvalid, 1},
		{http.StatusUnauthorized, scm.KindUnauthorized, 1},
		{http.StatusForbidden, scm.KindForbidden, 1},
		{http.StatusNotFound, scm.KindNotFound, 1},
		{http.StatusConflict, scm.KindConflict, 1},
		{http.StatusGone, scm.KindNotFound, 1},
		{http.StatusUnprocessableEntity, scm.KindInvalid, 1},
		{http.StatusInternalServerError, scm.KindUnavailable, 2},
		{http.StatusBadGateway, scm.KindUnavailable, 2},
		{http.StatusServiceUnavailable, scm.KindUnavailable, 2},
	}
	for _, tc := range tests {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			t.Parallel()
			s := newSeed(t, "")
			p := newProvider(t, s.config(t, pat))
			s.srv.FailNext(10, tc.status)
			_, err := p.ListBranches(t.Context(), s.platformRepo, true)
			var se *scm.Error
			if !errors.As(err, &se) || se.Kind != tc.kind || se.StatusCode != tc.status || se.Op != "ListBranches" {
				t.Fatalf("err = %#v", err)
			}
			if n := len(s.srv.Requests()); n != tc.calls {
				t.Fatalf("requests = %d, want %d", n, tc.calls)
			}
		})
	}
}

func TestTransientErrorsRetried(t *testing.T) {
	t.Parallel()
	s := newSeed(t, "")
	p := newProvider(t, s.config(t, pat))
	s.srv.FailNext(1, http.StatusBadGateway)
	got, err := p.ListBranches(t.Context(), s.platformRepo, false)
	if err != nil || len(got) != 1 {
		t.Fatalf("retry: %v %v", got, err)
	}
}

func TestRateLimits(t *testing.T) {
	t.Parallel()
	t.Run("primary", func(t *testing.T) {
		t.Parallel()
		s := newSeed(t, "")
		p := newProvider(t, s.config(t, pat))
		s.srv.InjectRateLimit(time.Hour, 0)
		_, err := p.ListBranches(t.Context(), s.platformRepo, true)
		var se *scm.Error
		if !errors.As(err, &se) || se.Kind != scm.KindRateLimited || se.StatusCode != http.StatusForbidden {
			t.Fatalf("err = %v", err)
		}
		if se.RetryAfter < 55*time.Minute || se.RetryAfter > time.Hour+time.Minute {
			t.Fatalf("RetryAfter = %v", se.RetryAfter)
		}
		n := len(s.srv.Requests())
		// The client remembers the exhausted quota and does not hammer the API.
		if _, err := p.ListBranches(t.Context(), s.platformRepo, true); !scm.IsRateLimited(err) {
			t.Fatalf("second call: %v", err)
		}
		if len(s.srv.Requests()) != n {
			t.Fatalf("request sent while the rate limit is known to be exhausted")
		}
	})
	t.Run("secondary", func(t *testing.T) {
		t.Parallel()
		s := newSeed(t, "")
		p := newProvider(t, s.config(t, pat))
		s.srv.InjectSecondaryRateLimit(2*time.Second, 0)
		_, err := p.ResolveRevision(t.Context(), s.platformRepo, "main")
		ra, ok := scm.RetryAfter(err)
		if !scm.IsRateLimited(err) || !ok || ra != 2*time.Second {
			t.Fatalf("err = %v (retry after %v)", err, ra)
		}
	})
	t.Run("short reset is waited out", func(t *testing.T) {
		t.Parallel()
		s := newSeed(t, "")
		p := newProvider(t, s.config(t, pat))
		s.srv.InjectRateLimit(0, 1)
		got, err := p.ListBranches(t.Context(), s.platformRepo, false)
		if err != nil || len(got) != 1 {
			t.Fatalf("expected transparent retry: %v %v", got, err)
		}
		if n := len(s.srv.Requests()); n != 3 { // 403, then GET repo + GET branch
			t.Fatalf("requests = %d", n)
		}
	})
}

func TestCredentialsNeverSentCrossHost(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var otherAuth []string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		otherAuth = append(otherAuth, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "platform", "default_branch": ""})
	}))
	defer other.Close()
	var apiAuth string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		apiAuth = r.Header.Get("Authorization")
		mu.Unlock()
		http.Redirect(w, r, other.URL+"/moved", http.StatusTemporaryRedirect)
	}))
	defer api.Close()
	p := newProvider(t, scm.Config{APIURL: api.URL + "/api/v3", Token: func(context.Context) (string, error) { return testPAT, nil }, RateLimit: scm.RateLimit{RPS: -1}, Retry: testRetry})
	if _, err := p.ListBranches(t.Context(), scm.RepoRef{Owner: "acme", Name: "platform"}, false); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if apiAuth != "Bearer "+testPAT {
		t.Fatalf("API host did not receive the token: %q", apiAuth)
	}
	if len(otherAuth) != 1 || otherAuth[0] != "" {
		t.Fatalf("redirect target received credentials: %q", otherAuth)
	}
}

func TestRequestLogsAreRedacted(t *testing.T) {
	t.Parallel()
	s := newSeed(t, "")
	var buf bytes.Buffer
	var mu sync.Mutex
	cfg := s.config(t, appExplicit)
	cfg.Logger = slog.New(slog.NewJSONHandler(writerFunc(func(b []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(b)
	}), &slog.HandlerOptions{Level: slog.LevelDebug}))
	p := newProvider(t, cfg)
	tok, err := p.Token(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.ListBranches(t.Context(), s.platformRepo, true); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	out := buf.String()
	mu.Unlock()
	if !strings.Contains(out, `"msg":"scm request"`) {
		t.Fatalf("no request logs: %s", out)
	}
	_, pemKey := appKey(t)
	for _, secret := range []string{tok.Value, string(pemKey[40:80]), "Bearer"} {
		if strings.Contains(out, secret) {
			t.Fatalf("log leaks %q", secret)
		}
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }

func TestClientExposesSameTransport(t *testing.T) {
	t.Parallel()
	s := newSeed(t, "")
	p := newProvider(t, s.config(t, pat))
	repo, _, err := p.Client().Repositories.Get(t.Context(), "acme", "platform")
	if err != nil || repo.GetName() != "platform" {
		t.Fatalf("Client(): %v", err)
	}
}

func TestObserverSeesAPIAndTokenRequests(t *testing.T) {
	t.Parallel()
	s := newSeed(t, "/api/v3")
	var mu sync.Mutex
	var seen []scm.RequestInfo
	cfg := s.config(t, appDiscover)
	cfg.Observer = func(_ context.Context, info scm.RequestInfo) {
		mu.Lock()
		seen = append(seen, info)
		mu.Unlock()
	}
	p := newProvider(t, cfg)
	if _, err := p.ListBranches(t.Context(), s.platformRepo, true); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	type key struct{ method, path string }
	got := map[key]scm.RequestInfo{}
	for _, info := range seen {
		if info.Provider != github.Type || info.Duration < 0 {
			t.Errorf("observed %+v", info)
		}
		got[key{info.Method, info.Path}] = info
	}
	for _, want := range []key{
		{http.MethodGet, "/api/v3/orgs/acme/installation"},
		{http.MethodPost, "/api/v3/app/installations/77/access_tokens"},
		{http.MethodGet, "/api/v3/repos/acme/platform/branches"},
	} {
		info, ok := got[want]
		if !ok {
			t.Fatalf("request %v not observed; saw %d requests", want, len(seen))
		}
		if info.StatusCode < 200 || info.StatusCode > 299 || !info.RateLimit.Present || info.RateLimit.Resource != "core" || info.RateLimit.Limit != 5000 {
			t.Errorf("%v: %+v", want, info)
		}
	}
}

func TestCommitStatusLimit(t *testing.T) {
	t.Parallel()
	s := newSeed(t, "")
	s.srv.SetStatusLimit(1)
	p := newProvider(t, s.config(t, pat))
	st := scm.CommitStatus{State: scm.CommitStateSuccess, Context: "continuous-delivery/app"}
	if err := p.SetCommitStatus(t.Context(), s.platformRepo, sha(1), st); err != nil {
		t.Fatal(err)
	}
	err := p.SetCommitStatus(t.Context(), s.platformRepo, sha(1), st)
	if !errors.Is(err, scm.ErrStatusLimit) || scm.KindOf(err) != scm.KindInvalid {
		t.Fatalf("limit: %v", err)
	}
	var se *scm.Error
	if !errors.As(err, &se) || se.StatusCode != http.StatusUnprocessableEntity || se.Op != "SetCommitStatus" {
		t.Fatalf("error details: %+v", se)
	}
	// Other validation failures are not mistaken for the limit.
	err = p.SetCommitStatus(t.Context(), s.platformRepo, sha(77), scm.CommitStatus{State: scm.CommitStateSuccess, Context: "other"})
	if scm.KindOf(err) != scm.KindInvalid || errors.Is(err, scm.ErrStatusLimit) {
		t.Fatalf("unknown commit: %v", err)
	}
}
