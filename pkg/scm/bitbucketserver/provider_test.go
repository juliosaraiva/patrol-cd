package bitbucketserver_test

import (
	"context"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"pgregory.net/rapid"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/bitbucketserver"
)

func TestListRepositories(t *testing.T) {
	t.Parallel()
	s := newSeed(t)
	p := basicProvider(t, s)
	got, err := p.ListRepositories(t.Context(), scm.RepoQuery{Organization: "PROJ", CloneProtocol: scm.CloneProtocolHTTPS})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	var platform scm.Repository
	for _, r := range got {
		names = append(names, r.Name)
		if r.Name == "platform" {
			platform = r
		}
	}
	for _, skipped := range []string{"empty", "nodefault"} {
		if slices.Contains(names, skipped) {
			t.Errorf("repository %q without default branch must be skipped", skipped)
		}
	}
	if !slices.Contains(names, "legacy") {
		t.Error("archived repositories are listed unless excluded")
	}
	if platform.Organization != "PROJ" || platform.RepositoryID != int64(42) || platform.Branch != "main" || platform.SHA != sha(1) ||
		platform.URL != s.f.srv.URL+"/scm/proj/platform.git" || !strings.HasPrefix(platform.SSHURL, "ssh://git@") ||
		platform.WebURL != s.f.srv.URL+"/projects/PROJ/repos/platform/browse" || len(platform.Labels) != 0 || platform.Labels == nil || !platform.Private {
		t.Fatalf("platform = %+v", platform)
	}
	all, err := p.ListRepositories(t.Context(), scm.RepoQuery{Organization: "PROJ", AllBranches: true, Match: func(r scm.Repository) bool {
		return r.Name == "nodefault" || r.Name == "platform"
	}})
	if err != nil || len(all) != 5 {
		t.Fatalf("all branches: %d entries, %v", len(all), err)
	}
	_, err = p.ListRepositories(t.Context(), scm.RepoQuery{Organization: "PROJ", CloneProtocol: "https-fips"})
	if scm.KindOf(err) != scm.KindInvalid || !strings.Contains(err.Error(), "unknown clone protocol for Bitbucket Server https-fips") {
		t.Fatalf("unknown protocol: %v", err)
	}
	if got, err := p.ListRepositories(t.Context(), scm.RepoQuery{Organization: "PROJ", Topics: []string{"x"}}); err != nil || len(got) != 0 {
		t.Fatalf("topics filter (unsupported) must match nothing: %v, %v", got, err)
	}
}

// TestPersonalProject covers personal projects, whose key is "~" + user slug in the
// REST API (upstream passes the configured project key through unchanged).
func TestPersonalProject(t *testing.T) {
	t.Parallel()
	s := newSeed(t)
	s.f.add(&fbRepo{ID: 77, Project: "~jdoe", Slug: "dotfiles", DefaultBranch: "main",
		Branches: []fbBranch{{Name: "main", SHA: sha(70)}, {Name: "wip", SHA: sha(71)}},
		Files:    map[string][]string{sha(70): {"apps/x/kustomization.yaml"}},
		Pulls:    []fbPull{{ID: 1, Title: "WIP", From: "wip", To: "main", SHA: sha(71), Author: "jdoe", State: "OPEN"}}})
	p := basicProvider(t, s)

	for _, key := range []string{"~jdoe", "~JDOE"} {
		repos, err := p.ListRepositories(t.Context(), scm.RepoQuery{Organization: key})
		if err != nil || len(repos) != 1 {
			t.Fatalf("ListRepositories(%q) = %+v, %v", key, repos, err)
		}
		r := repos[0]
		if r.Organization != "~JDOE" || r.Name != "dotfiles" || r.FullName != "~JDOE/dotfiles" || r.Branch != "main" || r.SHA != sha(70) ||
			r.URL != "ssh://git@"+strings.TrimPrefix(s.f.srv.URL, "http://")+"/~jdoe/dotfiles.git" {
			t.Fatalf("personal repository = %+v", r)
		}
		// The listed repository is addressable through every RepoRef operation.
		ref := scm.RepoRef{Owner: r.Organization, Name: r.Name}
		if bs, err := p.ListBranches(t.Context(), ref, true); err != nil || len(bs) != 2 {
			t.Fatalf("ListBranches(%v) = %v, %v", ref, bs, err)
		}
	}
	ref := scm.RepoRef{Owner: "~jdoe", Name: "dotfiles"}
	prs, err := p.ListPullRequests(t.Context(), ref, scm.PRQuery{})
	if err != nil || len(prs) != 1 || prs[0].Number != 1 || prs[0].HeadSHA != sha(71) {
		t.Fatalf("ListPullRequests = %+v, %v", prs, err)
	}
	if got, err := p.ResolveRevision(t.Context(), ref, "wip"); err != nil || got != sha(71) {
		t.Fatalf("ResolveRevision = %q, %v", got, err)
	}
	if ok, err := p.PathExists(t.Context(), ref, "main", "apps/x"); err != nil || !ok {
		t.Fatalf("PathExists = %v, %v", ok, err)
	}
	reqs, _ := s.f.log()
	for _, want := range []string{"GET /rest/api/1.0/projects/~jdoe/repos?", "GET /rest/api/1.0/projects/~jdoe/repos/dotfiles/pull-requests?"} {
		if !slices.ContainsFunc(reqs, func(r string) bool { return strings.HasPrefix(r, want) }) {
			t.Errorf("no request starting with %q in %v", want, reqs)
		}
	}
}

func TestProjectKeyValidation(t *testing.T) {
	t.Parallel()
	s := newSeed(t)
	p := basicProvider(t, s)
	for _, key := range []string{"", "~", "~~jdoe", "~.", "~..", ".", "..", "a~b", "jdoe~", "~a/b", "~a b", "~a%2Fb", "~a?x", "~a#x", "~" + strings.Repeat("a", 257)} {
		if _, err := p.ListRepositories(t.Context(), scm.RepoQuery{Organization: key}); scm.KindOf(err) != scm.KindInvalid {
			t.Errorf("ListRepositories(%q): %v, want KindInvalid", key, err)
		}
		if _, err := p.ListPullRequests(t.Context(), scm.RepoRef{Owner: key, Name: "platform"}, scm.PRQuery{}); scm.KindOf(err) != scm.KindInvalid {
			t.Errorf("ListPullRequests(%q): %v, want KindInvalid", key, err)
		}
	}
	if reqs, _ := s.f.log(); len(reqs) != 0 {
		t.Fatalf("invalid project keys reached the API: %v", reqs)
	}
}

// TestProjectKeyPathProperty checks that any accepted project key reaches the API as
// exactly one path segment equal to the key, and that rejected keys send nothing.
func TestProjectKeyPathProperty(t *testing.T) {
	t.Parallel()
	var (
		mu   sync.Mutex
		segs []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.EscapedPath(), "/rest/api/1.0/projects/")
		seg, _, _ := strings.Cut(rest, "/")
		mu.Lock()
		segs = append(segs, seg)
		mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"isLastPage": true, "values": []any{}})
	}))
	t.Cleanup(srv.Close)
	p := newProvider(t, scm.Config{APIURL: srv.URL, RateLimit: scm.RateLimit{RPS: -1}, Retry: scm.RetryPolicy{MaxAttempts: 1}}, bitbucketserver.Options{})
	valid := regexp.MustCompile(`^~?[A-Za-z0-9_.-]+$`)
	rapid.Check(t, func(rt *rapid.T) {
		key := rapid.StringMatching(`~{0,2}[A-Za-z0-9_.~/%?# -]{0,8}`).Draw(rt, "key")
		mu.Lock()
		segs = nil
		mu.Unlock()
		_, err := p.ListRepositories(t.Context(), scm.RepoQuery{Organization: key})
		mu.Lock()
		got := slices.Clone(segs)
		mu.Unlock()
		trimmed := strings.TrimSpace(key)
		bare := strings.TrimPrefix(trimmed, "~")
		accept := valid.MatchString(trimmed) && bare != "." && bare != ".."
		if !accept {
			if scm.KindOf(err) != scm.KindInvalid || len(got) != 0 {
				rt.Fatalf("key %q: err %v, requests %v; want KindInvalid and no request", key, err, got)
			}
			return
		}
		if err != nil || len(got) != 1 || got[0] != trimmed {
			rt.Fatalf("key %q: err %v, path segments %v; want exactly [%q]", key, err, got, trimmed)
		}
	})
}

func TestAuthenticationModes(t *testing.T) {
	t.Parallel()
	ref := scm.RepoRef{Owner: "PROJ", Name: "platform"}
	tests := []struct {
		name string
		opts bitbucketserver.Options
		tok  string
		want string
	}{
		{"basic", bitbucketserver.Options{Username: testUser}, testPassword, basic(testUser, testPassword)},
		{"bearer", bitbucketserver.Options{}, testBearer, "Bearer " + testBearer},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newSeed(t)
			s.f.auth = tc.want
			if _, err := newProvider(t, s.config(tc.tok), tc.opts).ListBranches(t.Context(), ref, true); err != nil {
				t.Fatal(err)
			}
			_, headers := s.f.log()
			h := headers[0]
			if h.Get("X-Atlassian-Token") != "no-check" || h.Get("X-Requested-With") != "XMLHttpRequest" {
				t.Fatalf("XSRF headers missing: %v", h)
			}
		})
	}
	t.Run("anonymous", func(t *testing.T) {
		t.Parallel()
		s := newSeed(t)
		s.f.auth = ""
		cfg := s.config("")
		cfg.Token = nil
		if _, err := newProvider(t, cfg, bitbucketserver.Options{}).ListBranches(t.Context(), ref, true); err != nil {
			t.Fatal(err)
		}
		_, headers := s.f.log()
		if headers[0].Get("Authorization") != "" || headers[0].Get("X-Atlassian-Token") != "" {
			t.Fatalf("anonymous requests must not carry auth or XSRF headers: %v", headers[0])
		}
	})
	t.Run("wrong password", func(t *testing.T) {
		t.Parallel()
		s := newSeed(t)
		_, err := newProvider(t, s.config("nope"), bitbucketserver.Options{Username: testUser}).ListBranches(t.Context(), ref, true)
		if scm.KindOf(err) != scm.KindUnauthorized || !strings.Contains(err.Error(), "Authentication failed") {
			t.Fatalf("err = %v", err)
		}
		if strings.Contains(err.Error(), "nope") {
			t.Fatal("error leaks the password")
		}
	})
	if _, err := bitbucketserver.NewProvider(t.Context(), scm.Config{APIURL: "https://bbs.example.com"}, bitbucketserver.Options{Username: "a:b"}); scm.KindOf(err) != scm.KindInvalid {
		t.Fatalf("username with colon: %v", err)
	}
}

func TestPullRequests(t *testing.T) {
	t.Parallel()
	s := newSeed(t)
	p := basicProvider(t, s)
	ref := scm.RepoRef{Owner: "PROJ", Name: "platform"}
	for state, want := range map[scm.PRState][]int64{
		"":                {1, 2, 3, 4},
		scm.PRStateMerged: {5},
		scm.PRStateClosed: {5, 6},
		scm.PRStateAll:    {1, 2, 3, 4, 5, 6},
	} {
		got, err := p.ListPullRequests(t.Context(), ref, scm.PRQuery{State: state})
		if err != nil {
			t.Fatal(err)
		}
		var nums []int64
		for _, pr := range got {
			nums = append(nums, pr.Number)
		}
		slices.Sort(nums)
		if !slices.Equal(nums, want) {
			t.Errorf("state %q: %v, want %v", state, nums, want)
		}
	}
	got, err := p.ListPullRequests(t.Context(), ref, scm.PRQuery{TargetBranch: "release-1"})
	if err != nil || len(got) != 1 || got[0].Number != 3 || got[0].Branch != "hotfix" || got[0].HeadSHA != sha(5) || got[0].Author != "carol" ||
		got[0].State != "OPEN" || !strings.HasSuffix(got[0].URL, "/pull-requests/3") {
		t.Fatalf("target branch: %+v, %v", got, err)
	}
	reqs, _ := s.f.log()
	if !strings.Contains(reqs[len(reqs)-1], "at=refs%2Fheads%2Frelease-1") {
		t.Fatalf("target branch not sent: %s", reqs[len(reqs)-1])
	}
	_, err = p.ListPullRequests(t.Context(), ref, scm.PRQuery{Labels: []string{"x"}})
	if scm.KindOf(err) != scm.KindInvalid || !errors.Is(err, bitbucketserver.ErrLabelsUnsupported) {
		t.Fatalf("labels: %v", err)
	}
}

func TestResolveAndPaths(t *testing.T) {
	t.Parallel()
	s := newSeed(t)
	p := basicProvider(t, s)
	ref := scm.RepoRef{Owner: "PROJ", Name: "platform"}
	for in, want := range map[string]string{"main": sha(1), "refs/heads/feature/a": sha(2), "v1.2.0": sha(4), sha(3): sha(3)} {
		if got, err := p.ResolveRevision(t.Context(), ref, in); err != nil || got != want {
			t.Errorf("ResolveRevision(%q) = %q, %v", in, got, err)
		}
	}
	for _, tc := range []struct {
		rev, path string
		want      bool
	}{
		{"main", "apps/a", true}, {"main", "apps/a/values.yaml", true}, {"", "README.md", true}, {"main", "dir with space/f#1.yaml", true},
		{"main", "", true}, {"main", "apps/b", false}, {"feature/a", "apps/b", true}, {"nope", "apps/a", false},
	} {
		if got, err := p.PathExists(t.Context(), ref, tc.rev, tc.path); err != nil || got != tc.want {
			t.Errorf("PathExists(%q, %q) = %v, %v", tc.rev, tc.path, got, err)
		}
	}
	empty, err := p.ListBranches(t.Context(), scm.RepoRef{Owner: "PROJ", Name: "empty"}, false)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty repository default branch: %v, %v", empty, err)
	}
	if _, err := p.ListBranches(t.Context(), scm.RepoRef{Owner: "PROJ", Name: "missing"}, false); !scm.IsNotFound(err) {
		t.Fatalf("missing repository: %v", err)
	}
}

func TestTLS(t *testing.T) {
	t.Parallel()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"isLastPage": true, "values": []any{}})
	}))
	t.Cleanup(srv.Close)
	ref := scm.RepoRef{Owner: "PROJ", Name: "platform"}
	cfg := scm.Config{APIURL: srv.URL, RateLimit: scm.RateLimit{RPS: -1}, Retry: scm.RetryPolicy{MaxAttempts: 1}}
	if _, err := newProvider(t, cfg, bitbucketserver.Options{}).ListBranches(t.Context(), ref, true); scm.KindOf(err) != scm.KindUnavailable {
		t.Fatalf("untrusted certificate: %v", err)
	}
	cfg.CAData = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if _, err := newProvider(t, cfg, bitbucketserver.Options{}).ListBranches(t.Context(), ref, true); err != nil {
		t.Fatalf("with CA: %v", err)
	}
}

func TestAPIBaseURL(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"https://bbs.example.com":                    "https://bbs.example.com/rest/api/1.0/",
		"https://bbs.example.com/":                   "https://bbs.example.com/rest/api/1.0/",
		"https://bbs.example.com/rest":               "https://bbs.example.com/rest/api/1.0/",
		"https://bbs.example.com/rest/":              "https://bbs.example.com/rest/api/1.0/",
		"https://example.com/bitbucket/rest/api/1.0": "https://example.com/bitbucket/rest/api/1.0/",
	} {
		u, err := bitbucketserver.APIBaseURL(in)
		if err != nil || u.String() != want {
			t.Errorf("APIBaseURL(%q) = %v, %v; want %q", in, u, err, want)
		}
	}
	if _, err := bitbucketserver.New(t.Context(), scm.Config{}); scm.KindOf(err) != scm.KindInvalid {
		t.Fatalf("missing API URL: %v", err)
	}
	rapid.Check(t, func(t *rapid.T) {
		path := rapid.StringMatching(`(/[a-z]{1,6}){0,2}(/rest)?/?`).Draw(t, "path")
		u, err := bitbucketserver.APIBaseURL("https://bbs.example.com" + path)
		if err != nil {
			t.Fatal(err)
		}
		again, err := bitbucketserver.APIBaseURL(u.String())
		if err != nil || again.String() != u.String() || !strings.HasSuffix(u.Path, "/rest/api/1.0/") {
			t.Fatalf("APIBaseURL not idempotent: %q → %q", u, again)
		}
	})
}

func TestTokenSourceFailure(t *testing.T) {
	t.Parallel()
	s := newSeed(t)
	cfg := s.config("")
	cfg.Token = func(context.Context) (string, error) { return "", errors.New("secret missing") }
	_, err := newProvider(t, cfg, bitbucketserver.Options{Username: testUser}).ListBranches(t.Context(), scm.RepoRef{Owner: "PROJ", Name: "platform"}, true)
	if scm.KindOf(err) != scm.KindUnauthorized {
		t.Fatalf("err = %v", err)
	}
	if reqs, _ := s.f.log(); len(reqs) != 0 {
		t.Fatalf("requests sent despite token failure: %v", reqs)
	}
}
