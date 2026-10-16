package gitea_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/gitea"
)

func byName(repos []scm.Repository) map[string]scm.Repository {
	out := map[string]scm.Repository{}
	for _, r := range repos {
		out[r.Name+"@"+r.Branch] = r
	}
	return out
}

func TestListRepositoriesFields(t *testing.T) {
	t.Parallel()
	s := newSeed(t)
	p := newProvider(t, s.config())
	got, err := p.ListRepositories(t.Context(), scm.RepoQuery{Organization: "ACME", CloneProtocol: scm.CloneProtocolHTTPS})
	if err != nil {
		t.Fatal(err)
	}
	m := byName(got)
	pl, ok := m["platform@main"]
	if !ok {
		t.Fatalf("platform missing: %v", m)
	}
	// Upstream: organization is the configured owner, labels are the repository's
	// labels, https selects the HTML URL.
	if pl.Organization != "ACME" || pl.RepositoryID != int64(42) || pl.SHA != sha(1) || pl.URL != s.f.srv.URL+"/acme/platform" ||
		!slices.Equal(pl.Labels, []string{"platform", "bug"}) || !pl.Private {
		t.Fatalf("platform = %+v", pl)
	}
	if _, ok := m["legacy@main"]; !ok {
		t.Fatal("archived repositories are included by default (excludeArchivedRepos=false)")
	}
	if _, ok := m["empty@main"]; ok {
		t.Fatal("empty repositories must be omitted")
	}
	ssh, err := p.ListRepositories(t.Context(), scm.RepoQuery{Organization: "acme", Match: func(r scm.Repository) bool { return r.Name == "platform" }})
	if err != nil || len(ssh) != 1 || !strings.HasPrefix(ssh[0].URL, "git@") {
		t.Fatalf("ssh default: %+v, %v", ssh, err)
	}
	if _, err := p.ListRepositories(t.Context(), scm.RepoQuery{Organization: "acme", CloneProtocol: "gopher"}); scm.KindOf(err) != scm.KindInvalid ||
		!strings.Contains(err.Error(), "unknown clone protocol for GitHub gopher") {
		t.Fatalf("unknown protocol (upstream text): %v", err)
	}
}

func TestListRepositoriesLabelsAndTopics(t *testing.T) {
	t.Parallel()
	s := newSeed(t)
	p := newProvider(t, s.config())
	var seen []string
	_, err := p.ListRepositories(t.Context(), scm.RepoQuery{Organization: "acme", Match: func(r scm.Repository) bool {
		if slices.Contains(r.Labels, "bug") {
			seen = append(seen, r.Name)
		}
		return false
	}})
	if err != nil || !slices.Equal(seen, []string{"platform"}) {
		t.Fatalf("labelMatch pushdown saw %v, %v", seen, err)
	}
	got, err := p.ListRepositories(t.Context(), scm.RepoQuery{Organization: "acme", Topics: []string{"k8s"}})
	if err != nil || len(got) != 1 || got[0].Name != "platform" {
		t.Fatalf("topics: %+v, %v", got, err)
	}
}

func TestListRepositoriesUserFallback(t *testing.T) {
	t.Parallel()
	s := newSeed(t)
	got, err := newProvider(t, s.config()).ListRepositories(t.Context(), scm.RepoQuery{Organization: "octo"})
	if err != nil || len(got) != 1 || got[0].Name != "dotfiles" || got[0].SHA != sha(500) {
		t.Fatalf("user repos: %+v, %v", got, err)
	}
	log := strings.Join(s.f.requestLog(), "\n")
	if !strings.Contains(log, "/orgs/octo/repos") || !strings.Contains(log, "/users/octo/repos") {
		t.Fatalf("expected org then user listing:\n%s", log)
	}
}

func TestDefaultBranchMissingIsAnError(t *testing.T) {
	t.Parallel()
	s := newSeed(t)
	s.f.add(&ftRepo{ID: 9, Owner: "acme", Name: "broken", DefaultBranch: "gone", Branches: []ftBranch{{Name: "main", SHA: sha(9)}}})
	_, err := newProvider(t, s.config()).ListRepositories(t.Context(), scm.RepoQuery{Organization: "acme"})
	if scm.KindOf(err) != scm.KindNotFound || !strings.Contains(err.Error(), `got 404 while getting default branch "gone" for repo "broken" - check your repo config`) {
		t.Fatalf("err = %v", err)
	}
}

func TestPaginationWithoutLinkHeader(t *testing.T) {
	t.Parallel()
	s := newSeed(t)
	s.f.noLink = true
	s.f.maxPer = 50
	var many []string
	for i := range 55 {
		many = append(many, fmt.Sprintf("label-%02d", i))
	}
	s.f.add(&ftRepo{ID: 77, Owner: "acme", Name: "labels", DefaultBranch: "main", Labels: many, Branches: []ftBranch{{Name: "main", SHA: sha(77)}}})
	got, err := newProvider(t, s.config()).ListRepositories(t.Context(), scm.RepoQuery{Organization: "acme", Match: func(r scm.Repository) bool { return r.Name == "labels" }})
	if err != nil || len(got) != 1 || len(got[0].Labels) != 55 {
		t.Fatalf("labels across pages without Link: %v", err)
	}
}

func TestPullRequestStates(t *testing.T) {
	t.Parallel()
	tests := []struct {
		state scm.PRState
		want  []int64
	}{
		{"", []int64{1, 2, 3, 4}},
		{scm.PRStateClosed, []int64{5, 6}},
		{scm.PRStateMerged, []int64{5}},
		{scm.PRStateAll, []int64{1, 2, 3, 4, 5, 6}},
	}
	s := newSeed(t)
	p := newProvider(t, s.config())
	for _, tc := range tests {
		got, err := p.ListPullRequests(t.Context(), scm.RepoRef{Owner: "acme", Name: "platform"}, scm.PRQuery{State: tc.state})
		if err != nil {
			t.Fatal(err)
		}
		var nums []int64
		for _, pr := range got {
			nums = append(nums, pr.Number)
		}
		slices.Sort(nums)
		if !slices.Equal(nums, tc.want) {
			t.Errorf("state %q: %v, want %v", tc.state, nums, tc.want)
		}
	}
	got, err := p.ListPullRequests(t.Context(), scm.RepoRef{Owner: "acme", Name: "platform"}, scm.PRQuery{TargetBranch: "release-1"})
	if err != nil || len(got) != 1 || got[0].Number != 3 || got[0].Author != "carol" || got[0].State != "open" {
		t.Fatalf("target branch: %+v, %v", got, err)
	}
	if _, err := p.ListPullRequests(t.Context(), scm.RepoRef{Owner: "acme", Name: "platform"}, scm.PRQuery{State: "bogus"}); scm.KindOf(err) != scm.KindInvalid {
		t.Fatalf("bogus state: %v", err)
	}
}

func TestResolveRevisionAndPaths(t *testing.T) {
	t.Parallel()
	s := newSeed(t)
	p := newProvider(t, s.config())
	ref := scm.RepoRef{Owner: "acme", Name: "platform"}
	for in, want := range map[string]string{"v1.2.0": sha(4), "refs/tags/v1.2.0": sha(4), "refs/heads/feature/a": sha(2), sha(3): sha(3), "main": sha(1)} {
		got, err := p.ResolveRevision(t.Context(), ref, in)
		if err != nil || got != want {
			t.Errorf("ResolveRevision(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := p.ResolveRevision(t.Context(), ref, "refs/heads/v1.2.0"); !scm.IsNotFound(err) {
		t.Errorf("refs/heads/ must not match tags: %v", err)
	}
	for _, bad := range []string{"", "refs/heads/", "../x"} {
		if _, err := p.ResolveRevision(t.Context(), ref, bad); scm.KindOf(err) != scm.KindInvalid {
			t.Errorf("ResolveRevision(%q) = %v, want Invalid", bad, err)
		}
	}
	paths := []struct {
		rev, path string
		want      bool
	}{
		{"main", "apps/a/values.yaml", true},
		{"main", "apps/a", true},
		{"", "README.md", true},
		{"main", "dir with space/f#1.yaml", true},
		{"main", "", true},
		{"main", "apps/b", false},
		{"feature/a", "apps/b", true},
	}
	for _, tc := range paths {
		got, err := p.PathExists(t.Context(), ref, tc.rev, tc.path)
		if err != nil || got != tc.want {
			t.Errorf("PathExists(%q, %q) = %v, %v; want %v", tc.rev, tc.path, got, err, tc.want)
		}
	}
}

func TestAuthAndErrors(t *testing.T) {
	t.Parallel()
	s := newSeed(t)
	cfg := s.config()
	cfg.Token = func(context.Context) (string, error) { return "wrong", nil }
	if _, err := newProvider(t, cfg).ListBranches(t.Context(), scm.RepoRef{Owner: "acme", Name: "platform"}, true); scm.KindOf(err) != scm.KindUnauthorized {
		t.Fatalf("wrong token: %v", err)
	}
	cfg.Token = func(context.Context) (string, error) { return "", errors.New("no secret") }
	if _, err := newProvider(t, cfg).ListBranches(t.Context(), scm.RepoRef{Owner: "acme", Name: "platform"}, true); scm.KindOf(err) != scm.KindUnauthorized {
		t.Fatalf("token source failure: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"user does not have permission","url":"x"}`))
	}))
	t.Cleanup(srv.Close)
	_, err := newProvider(t, scm.Config{APIURL: srv.URL, RateLimit: scm.RateLimit{RPS: -1}}).ListBranches(t.Context(), scm.RepoRef{Owner: "a", Name: "b"}, true)
	if scm.KindOf(err) != scm.KindForbidden || !strings.Contains(err.Error(), "user does not have permission") {
		t.Fatalf("403: %v", err)
	}
}

func TestAPIBaseURL(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"https://gitea.com":            "https://gitea.com/api/v1/",
		"https://gitea.com/":           "https://gitea.com/api/v1/",
		"https://example.com/git/":     "https://example.com/git/api/v1/",
		"http://10.0.0.1:3000/api/v1/": "http://10.0.0.1:3000/api/v1/",
	} {
		u, err := gitea.APIBaseURL(in)
		if err != nil || u.String() != want {
			t.Errorf("APIBaseURL(%q) = %v, %v; want %q", in, u, err, want)
		}
	}
	for _, bad := range []string{"", "gitea.com", "https://u:p@gitea.com"} {
		if _, err := gitea.APIBaseURL(bad); err == nil {
			t.Errorf("APIBaseURL(%q) accepted", bad)
		}
	}
	if _, err := gitea.New(t.Context(), scm.Config{}); scm.KindOf(err) != scm.KindInvalid {
		t.Fatalf("missing API URL: %v", err)
	}
	rapid.Check(t, func(t *rapid.T) {
		path := rapid.StringMatching(`(/[a-z0-9]{1,6}){0,3}/?`).Draw(t, "path")
		u, err := gitea.APIBaseURL("https://git.example.com" + path)
		if err != nil {
			t.Fatal(err)
		}
		again, err := gitea.APIBaseURL(u.String())
		if err != nil || again.String() != u.String() {
			t.Fatalf("not idempotent: %q → %q", u, again)
		}
	})
}
