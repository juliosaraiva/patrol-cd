package gitlab_test

import (
	"context"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/gitlab"
)

func names(repos []scm.Repository) []string {
	out := make([]string, 0, len(repos))
	for _, r := range repos {
		out = append(out, r.FullName)
	}
	slices.Sort(out)
	return out
}

func boolPtr(b bool) *bool { return &b }

func TestListRepositoriesGroupOptions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		opts      gitlab.Options
		wantIn    []string
		wantOut   []string
		wantQuery []string
	}{
		{"defaults", gitlab.Options{}, []string{"acme/platform", "other/shared"}, []string{"acme/infra/net"},
			[]string{"include_subgroups=false", "with_shared=true"}},
		{"subgroups", gitlab.Options{IncludeSubgroups: true}, []string{"acme/infra/net", "other/shared"}, nil,
			[]string{"include_subgroups=true"}},
		{"no shared", gitlab.Options{IncludeSharedProjects: boolPtr(false)}, []string{"acme/platform"}, []string{"other/shared", "acme/infra/net"},
			[]string{"with_shared=false"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newSeed(t)
			p := newProvider(t, s.config(), tc.opts)
			got, err := p.ListRepositories(t.Context(), scm.RepoQuery{Organization: "acme"})
			if err != nil {
				t.Fatal(err)
			}
			n := names(got)
			for _, w := range tc.wantIn {
				if !slices.Contains(n, w) {
					t.Errorf("missing %s in %v", w, n)
				}
			}
			for _, w := range tc.wantOut {
				if slices.Contains(n, w) {
					t.Errorf("unexpected %s in %v", w, n)
				}
			}
			log := strings.Join(s.f.requestLog(), "\n")
			for _, w := range tc.wantQuery {
				if !strings.Contains(log, w) {
					t.Errorf("request log lacks %q:\n%s", w, log)
				}
			}
		})
	}
}

func TestListRepositoriesFields(t *testing.T) {
	t.Parallel()
	s := newSeed(t)
	p := newProvider(t, s.config(), gitlab.Options{IncludeSubgroups: true})
	got, err := p.ListRepositories(t.Context(), scm.RepoQuery{Organization: "acme", CloneProtocol: scm.CloneProtocolHTTPS})
	if err != nil {
		t.Fatal(err)
	}
	var platform, net *scm.Repository
	for i := range got {
		switch got[i].FullName {
		case "acme/platform":
			platform = &got[i]
		case "acme/infra/net":
			net = &got[i]
		}
	}
	if platform == nil || net == nil {
		t.Fatalf("missing repositories: %v", names(got))
	}
	host := strings.TrimPrefix(s.f.url(), "http://")
	if platform.Organization != "acme" || platform.Name != "platform" || platform.RepositoryID != int64(42) ||
		platform.URL != s.f.url()+"/acme/platform.git" || platform.SSHURL != "git@"+host+":acme/platform.git" ||
		platform.WebURL != s.f.url()+"/acme/platform" || platform.Branch != "main" || platform.SHA != sha(1) ||
		!slices.Equal(platform.Labels, []string{"platform"}) || !platform.Private {
		t.Fatalf("platform = %+v", platform)
	}
	// Subgroup projects report the namespace full path; legacy tag list is the fallback.
	if net.Organization != "acme/infra" || net.Name != "net" || !slices.Equal(net.Labels, []string{"legacy-tag"}) {
		t.Fatalf("net = %+v", net)
	}
	if got := net.Ref(); got != (scm.RepoRef{Owner: "acme/infra", Name: "net"}) {
		t.Fatalf("Ref() = %v", got)
	}
	// The repository reference round-trips into other capabilities.
	bs, err := p.ListBranches(t.Context(), net.Ref(), true)
	if err != nil || len(bs) != 1 || bs[0].SHA != sha(90) {
		t.Fatalf("ListBranches(subgroup) = %v, %v", bs, err)
	}
}

func TestListRepositoriesTopicAndArchivedWire(t *testing.T) {
	t.Parallel()
	tests := []struct {
		archived scm.ArchivedFilter
		topics   []string
		want     string
		absent   string
		repos    []string
	}{
		{scm.ArchivedDefault, nil, "", "archived=", nil},
		{scm.ArchivedInclude, nil, "archived=true", "", []string{"acme/legacy"}},
		{scm.ArchivedExclude, nil, "archived=false", "", nil},
		{scm.ArchivedDefault, []string{"team-a"}, "topic=team-a", "", []string{"acme/svc-00"}},
	}
	for _, tc := range tests {
		t.Run(string(tc.archived)+strings.Join(tc.topics, ","), func(t *testing.T) {
			t.Parallel()
			s := newSeed(t)
			p := newProvider(t, s.config(), gitlab.Options{})
			got, err := p.ListRepositories(t.Context(), scm.RepoQuery{Organization: "acme", Archived: tc.archived, Topics: tc.topics})
			if err != nil {
				t.Fatal(err)
			}
			first := s.f.requestLog()[0]
			if tc.want != "" && !strings.Contains(first, tc.want) {
				t.Errorf("request %q lacks %q", first, tc.want)
			}
			if tc.absent != "" && strings.Contains(first, tc.absent) {
				t.Errorf("request %q must not contain %q", first, tc.absent)
			}
			for _, r := range tc.repos {
				if !slices.Contains(names(got), r) {
					t.Errorf("missing %s in %v", r, names(got))
				}
			}
			if tc.archived == scm.ArchivedInclude {
				for _, r := range got {
					if !r.Archived {
						t.Errorf("archived=true is upstream's only-archived filter on GitLab; got %s", r.FullName)
					}
				}
			}
		})
	}
}

func TestListRepositoriesErrors(t *testing.T) {
	t.Parallel()
	s := newSeed(t)
	p := newProvider(t, s.config(), gitlab.Options{})
	_, err := p.ListRepositories(t.Context(), scm.RepoQuery{Organization: "acme", CloneProtocol: "gopher"})
	if scm.KindOf(err) != scm.KindInvalid || !strings.Contains(err.Error(), "unknown clone protocol for Gitlab gopher") {
		t.Fatalf("unknown protocol: %v", err)
	}
	for _, org := range []string{"", "../x", "acme/../x", "a b"} {
		if _, err := p.ListRepositories(t.Context(), scm.RepoQuery{Organization: org}); scm.KindOf(err) != scm.KindInvalid {
			t.Errorf("organization %q: %v, want Invalid", org, err)
		}
	}
	if n := len(s.f.requestLog()); n != 0 {
		t.Fatalf("invalid queries reached the API (%d requests)", n)
	}
}

func TestPageLimit(t *testing.T) {
	t.Parallel()
	s := newSeed(t)
	cfg := s.config()
	cfg.MaxPages = 1
	p := newProvider(t, cfg, gitlab.Options{})
	_, err := p.ListRepositories(t.Context(), scm.RepoQuery{Organization: "acme"})
	var se *scm.Error
	if !errors.Is(err, scm.ErrPageLimit) || !errors.As(err, &se) || se.Kind != scm.KindInvalid || se.Provider != gitlab.Type || se.Op != "ListRepositories" {
		t.Fatalf("page limit error = %#v", err)
	}
}

func TestAuthentication(t *testing.T) {
	t.Parallel()
	s := newSeed(t)
	cfg := s.config()
	cfg.Token = func(context.Context) (string, error) { return "wrong", nil }
	_, err := newProvider(t, cfg, gitlab.Options{}).ListBranches(t.Context(), scm.RepoRef{Owner: "acme", Name: "platform"}, true)
	if scm.KindOf(err) != scm.KindUnauthorized {
		t.Fatalf("wrong token: %v", err)
	}
	before := len(s.f.requestLog())
	cfg.Token = func(context.Context) (string, error) { return "", errors.New("secret gitlab-token not found") }
	_, err = newProvider(t, cfg, gitlab.Options{}).ListBranches(t.Context(), scm.RepoRef{Owner: "acme", Name: "platform"}, true)
	if scm.KindOf(err) != scm.KindUnauthorized || !strings.Contains(err.Error(), "token source failed") {
		t.Fatalf("failing token source: %v", err)
	}
	if after := len(s.f.requestLog()); after != before {
		t.Fatalf("a failing token source must not send requests (%d → %d)", before, after)
	}
}

func TestCredentialsNotSentAcrossHosts(t *testing.T) {
	t.Parallel()
	var leaked atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("PRIVATE-TOKEN") != "" || r.Header.Get("Authorization") != "" {
			leaked.Store(true)
		}
		writeJSON(w, http.StatusOK, []any{})
	}))
	t.Cleanup(other.Close)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/api/v4/groups/acme/projects", http.StatusFound)
	}))
	t.Cleanup(api.Close)
	cfg := scm.Config{APIURL: api.URL, Token: func(context.Context) (string, error) { return testToken, nil }, RateLimit: scm.RateLimit{RPS: -1}, Retry: testRetry}
	if _, err := newProvider(t, cfg, gitlab.Options{}).ListRepositories(t.Context(), scm.RepoQuery{Organization: "acme"}); err != nil {
		t.Fatal(err)
	}
	if leaked.Load() {
		t.Fatal("credentials were sent to a redirect target on another host")
	}
}

func TestPullRequestStates(t *testing.T) {
	t.Parallel()
	tests := []struct {
		state scm.PRState
		wire  string
		want  []int64
	}{
		{"", "state=opened", []int64{1, 2, 3, 4}},
		{scm.PRStateOpen, "state=opened", []int64{1, 2, 3, 4}},
		{scm.PRStateMerged, "state=merged", []int64{5}},
		{scm.PRStateClosed, "state=closed", []int64{6}},
		{scm.PRStateAll, "state=all", []int64{1, 2, 3, 4, 5, 6}},
		{gitlab.PRStateLocked, "state=locked", nil},
	}
	for _, tc := range tests {
		t.Run(string(tc.state), func(t *testing.T) {
			t.Parallel()
			s := newSeed(t)
			p := newProvider(t, s.config(), gitlab.Options{})
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
				t.Fatalf("numbers = %v, want %v", nums, tc.want)
			}
			if first := s.f.requestLog()[0]; !strings.Contains(first, tc.wire) {
				t.Fatalf("request %q lacks %q", first, tc.wire)
			}
		})
	}
	s := newSeed(t)
	p := newProvider(t, s.config(), gitlab.Options{})
	if _, err := p.ListPullRequests(t.Context(), scm.RepoRef{Owner: "acme", Name: "platform"}, scm.PRQuery{State: "bogus"}); scm.KindOf(err) != scm.KindInvalid {
		t.Fatalf("unknown state: %v", err)
	}
}

func TestPullRequestFields(t *testing.T) {
	t.Parallel()
	s := newSeed(t)
	p := newProvider(t, s.config(), gitlab.Options{})
	got, err := p.ListPullRequests(t.Context(), scm.RepoRef{Owner: "acme", Name: "platform"}, scm.PRQuery{Labels: []string{"preview", "team-a"}, TargetBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
	pr := got[0]
	if pr.Number != 2 || pr.Branch != "feature-b" || pr.TargetBranch != "main" || pr.HeadSHA != sha(3) || pr.Author != "bob" ||
		!pr.Draft || pr.State != "opened" || pr.Merged || !strings.HasSuffix(pr.URL, "/acme/platform/-/merge_requests/2") {
		t.Fatalf("pr = %+v", pr)
	}
	if first := s.f.requestLog()[0]; !strings.Contains(first, "labels=preview%2Cteam-a") || !strings.Contains(first, "target_branch=main") {
		t.Fatalf("labels/target branch not sent to the API: %q", first)
	}
	// Numeric project IDs are accepted as RepoRef{Name: "<id>"}.
	byID, err := p.ListPullRequests(t.Context(), scm.RepoRef{Name: "42"}, scm.PRQuery{})
	if err != nil || len(byID) != 4 {
		t.Fatalf("by numeric ID: %d PRs, %v", len(byID), err)
	}
	if _, err := p.ListPullRequests(t.Context(), scm.RepoRef{Name: "platform"}, scm.PRQuery{}); scm.KindOf(err) != scm.KindInvalid {
		t.Fatalf("non-numeric name without owner: %v", err)
	}
}

// TestPullRequestLabelKeywords pins upstream parity: labels are a server-side filter
// only, so GitLab's predefined None/Any keywords keep working.
func TestPullRequestLabelKeywords(t *testing.T) {
	t.Parallel()
	tests := []struct {
		labels []string
		wire   string
		want   []int64
	}{
		{[]string{"None"}, "labels=None", []int64{3}},
		{[]string{"none"}, "labels=none", []int64{3}},
		{[]string{"Any"}, "labels=Any", []int64{1, 2, 4}},
		{[]string{"ANY"}, "labels=ANY", []int64{1, 2, 4}},
		{[]string{"preview"}, "labels=preview", []int64{1, 2}},
		{[]string{"preview", "team-a"}, "labels=preview%2Cteam-a", []int64{2}},
		{[]string{"preview", "missing"}, "labels=preview%2Cmissing", nil},
	}
	for _, tc := range tests {
		t.Run(strings.Join(tc.labels, ","), func(t *testing.T) {
			t.Parallel()
			s := newSeed(t)
			p := newProvider(t, s.config(), gitlab.Options{})
			got, err := p.ListPullRequests(t.Context(), scm.RepoRef{Owner: "acme", Name: "platform"}, scm.PRQuery{Labels: tc.labels})
			if err != nil {
				t.Fatal(err)
			}
			var nums []int64
			for _, pr := range got {
				nums = append(nums, pr.Number)
			}
			slices.Sort(nums)
			if !slices.Equal(nums, tc.want) {
				t.Fatalf("numbers = %v, want %v", nums, tc.want)
			}
			if first := s.f.requestLog()[0]; !strings.Contains(first, tc.wire) {
				t.Fatalf("request %q lacks %q", first, tc.wire)
			}
		})
	}
}

// TestPullRequestLabelsNotRecheckedClientSide asserts that every merge request the API
// returns for a label query is kept, whatever its labels (server-side semantics win).
func TestPullRequestLabelsNotRecheckedClientSide(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("labels") != "scoped::*" {
			writeJSON(w, http.StatusOK, []any{})
			return
		}
		w.Header().Set("X-Next-Page", "")
		writeJSON(w, http.StatusOK, []any{map[string]any{
			"iid": 9, "title": "t", "source_branch": "f", "target_branch": "main", "sha": sha(9),
			"labels": []string{"scoped::a"}, "state": "opened", "author": map[string]any{"username": "u"},
		}})
	}))
	t.Cleanup(srv.Close)
	cfg := scm.Config{Type: gitlab.Type, APIURL: srv.URL, RateLimit: scm.RateLimit{RPS: -1}, Retry: scm.RetryPolicy{MaxAttempts: 1}}
	got, err := newProvider(t, cfg, gitlab.Options{}).ListPullRequests(t.Context(), scm.RepoRef{Owner: "acme", Name: "platform"}, scm.PRQuery{Labels: []string{"scoped::*"}})
	if err != nil || len(got) != 1 || got[0].Number != 9 {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestResolveRevision(t *testing.T) {
	t.Parallel()
	s := newSeed(t)
	p := newProvider(t, s.config(), gitlab.Options{})
	ref := scm.RepoRef{Owner: "acme", Name: "platform"}
	for in, want := range map[string]string{"v1.2.0": sha(4), "refs/tags/v1.2.0": sha(4), "refs/heads/feature/a": sha(2), sha(3): sha(3)} {
		got, err := p.ResolveRevision(t.Context(), ref, in)
		if err != nil || got != want {
			t.Errorf("ResolveRevision(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "  ", "a/../b"} {
		if _, err := p.ResolveRevision(t.Context(), ref, bad); scm.KindOf(err) != scm.KindInvalid {
			t.Errorf("ResolveRevision(%q): %v, want Invalid", bad, err)
		}
	}
}

func TestPathExists(t *testing.T) {
	t.Parallel()
	s := newSeed(t)
	p := newProvider(t, s.config(), gitlab.Options{})
	ref := scm.RepoRef{Owner: "acme", Name: "platform"}
	tests := []struct {
		rev, path string
		want      bool
	}{
		{"main", "apps/a/values.yaml", true},
		{"main", "/apps/a/values.yaml", true},
		{"main", "apps/a", true},
		{"main", "", true},
		{"", "README.md", true},
		{"main", "dir with space/f#1.yaml", true},
		{"main", "apps/b", false},
		{"feature/a", "apps/b", true},
		{"no-such-branch", "apps/a", false},
	}
	for _, tc := range tests {
		got, err := p.PathExists(t.Context(), ref, tc.rev, tc.path)
		if err != nil || got != tc.want {
			t.Errorf("PathExists(%q, %q) = %v, %v; want %v", tc.rev, tc.path, got, err, tc.want)
		}
	}
	if ok, err := p.PathExists(t.Context(), scm.RepoRef{Owner: "acme", Name: "missing"}, "main", "a"); ok || err != nil {
		t.Errorf("missing project: %v, %v", ok, err)
	}
	// Files are probed with HEAD so file contents are never downloaded.
	for _, r := range s.f.requestLog() {
		if strings.Contains(r, "/repository/files/") && !strings.HasPrefix(r, "HEAD ") {
			t.Errorf("file probe used %q", r)
		}
	}
	if !slices.ContainsFunc(s.f.requestLog(), func(r string) bool { return strings.Contains(r, "dir%20with%20space%2Ff%231.yaml") }) {
		t.Errorf("path not escaped as one segment: %v", s.f.requestLog())
	}
}

func TestErrorMapping(t *testing.T) {
	t.Parallel()
	tests := []struct {
		status int
		header map[string]string
		body   string
		kind   scm.ErrorKind
		retry  time.Duration
	}{
		{400, nil, `{"message":{"name":["is invalid"]}}`, scm.KindInvalid, 0},
		{401, nil, `{"message":"401 Unauthorized"}`, scm.KindUnauthorized, 0},
		{403, nil, `{"message":"403 Forbidden"}`, scm.KindForbidden, 0},
		{404, nil, `{"message":"404 Group Not Found"}`, scm.KindNotFound, 0},
		{409, nil, `{"message":"conflict"}`, scm.KindConflict, 0},
		{422, nil, `{"error":"invalid"}`, scm.KindInvalid, 0},
		{429, nil, `{"message":"Retry later"}`, scm.KindRateLimited, time.Minute},
		{429, map[string]string{"Retry-After": "120"}, ``, scm.KindRateLimited, 120 * time.Second},
		{403, map[string]string{"RateLimit-Remaining": "0", "Retry-After": "30"}, ``, scm.KindRateLimited, 30 * time.Second},
		{500, nil, `<html>oops</html>`, scm.KindUnavailable, 0},
		{503, map[string]string{"Retry-After": "7"}, ``, scm.KindUnavailable, 7 * time.Second},
	}
	for _, tc := range tests {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				for k, v := range tc.header {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)
			cfg := scm.Config{APIURL: srv.URL, RateLimit: scm.RateLimit{RPS: -1}, Retry: scm.RetryPolicy{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, MaxWait: time.Millisecond}}
			_, err := newProvider(t, cfg, gitlab.Options{}).ListBranches(t.Context(), scm.RepoRef{Owner: "acme", Name: "platform"}, true)
			var se *scm.Error
			if !errors.As(err, &se) || se.Kind != tc.kind || se.StatusCode != tc.status || se.Provider != gitlab.Type || se.Op != "ListBranches" {
				t.Fatalf("err = %#v, want kind %s", err, tc.kind)
			}
			if se.RetryAfter != tc.retry {
				t.Fatalf("RetryAfter = %v, want %v", se.RetryAfter, tc.retry)
			}
			if strings.Contains(err.Error(), "<html>") {
				t.Fatalf("HTML body leaked into the error: %v", err)
			}
			if tc.status == http.StatusInternalServerError && calls.Load() != 2 {
				t.Fatalf("5xx must be retried: %d calls", calls.Load())
			}
		})
	}
}

func TestResponseSizeCapAndCancellation(t *testing.T) {
	t.Parallel()
	s := newSeed(t)
	cfg := s.config()
	cfg.MaxResponseBytes = 64
	_, err := newProvider(t, cfg, gitlab.Options{}).ListBranches(t.Context(), scm.RepoRef{Owner: "acme", Name: "platform"}, true)
	if scm.KindOf(err) != scm.KindInvalid || !errors.Is(err, scm.ErrResponseTooLarge) {
		t.Fatalf("oversize response: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = newProvider(t, s.config(), gitlab.Options{}).ListBranches(ctx, scm.RepoRef{Owner: "acme", Name: "platform"}, true)
	if !errors.Is(err, context.Canceled) || scm.IsRetryable(err) {
		t.Fatalf("canceled context: %v", err)
	}
}

func TestTLSOptions(t *testing.T) {
	t.Parallel()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, []any{})
	}))
	t.Cleanup(srv.Close)
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	base := scm.Config{APIURL: srv.URL, RateLimit: scm.RateLimit{RPS: -1}, Retry: scm.RetryPolicy{MaxAttempts: 1}}
	ref := scm.RepoRef{Owner: "acme", Name: "platform"}

	if _, err := newProvider(t, base, gitlab.Options{}).ListBranches(t.Context(), ref, true); scm.KindOf(err) != scm.KindUnavailable {
		t.Fatalf("self-signed server without CA must fail: %v", err)
	}
	withCA := base
	withCA.CAData = caPEM
	if _, err := newProvider(t, withCA, gitlab.Options{}).ListBranches(t.Context(), ref, true); err != nil {
		t.Fatalf("with CA: %v", err)
	}
	insecure := base
	insecure.Insecure = true
	if _, err := newProvider(t, insecure, gitlab.Options{}).ListBranches(t.Context(), ref, true); err != nil {
		t.Fatalf("insecure: %v", err)
	}
}

func TestProxy(t *testing.T) {
	t.Parallel()
	var proxied atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.IsAbs() { // absolute-form request line: we are being used as a proxy
			proxied.Add(1)
		}
		writeJSON(w, http.StatusOK, []any{})
	}))
	t.Cleanup(proxy.Close)
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, []any{}) }))
	t.Cleanup(direct.Close)
	ref := scm.RepoRef{Owner: "acme", Name: "platform"}
	cfg := scm.Config{APIURL: "http://gitlab.proxied.test", ProxyURL: proxy.URL, RateLimit: scm.RateLimit{RPS: -1}, Retry: scm.RetryPolicy{MaxAttempts: 1}}
	if _, err := newProvider(t, cfg, gitlab.Options{}).ListBranches(t.Context(), ref, true); err != nil {
		t.Fatal(err)
	}
	if proxied.Load() != 1 {
		t.Fatalf("request did not traverse the proxy (%d)", proxied.Load())
	}
	u, _ := url.Parse(direct.URL)
	cfg.APIURL, cfg.NoProxy = direct.URL, u.Hostname()
	if _, err := newProvider(t, cfg, gitlab.Options{}).ListBranches(t.Context(), ref, true); err != nil {
		t.Fatal(err)
	}
	if proxied.Load() != 1 {
		t.Fatal("no-proxy host was proxied")
	}
}

func TestAPIBaseURL(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"":                                   "https://gitlab.com/api/v4/",
		"https://gitlab.com":                 "https://gitlab.com/api/v4/",
		"https://gitlab.example.com/":        "https://gitlab.example.com/api/v4/",
		"https://example.com/gitlab":         "https://example.com/gitlab/api/v4/",
		"https://example.com/gitlab/api/v4/": "https://example.com/gitlab/api/v4/",
		"http://10.0.0.1:8080/api/v4":        "http://10.0.0.1:8080/api/v4/",
	}
	for in, want := range tests {
		u, err := gitlab.APIBaseURL(in)
		if err != nil || u.String() != want {
			t.Errorf("APIBaseURL(%q) = %v, %v; want %q", in, u, err, want)
		}
	}
	for _, bad := range []string{"ftp://x", "https://user:pw@gitlab.com", "https://gitlab.com/?private_token=x", "gitlab.com", "https://"} {
		if _, err := gitlab.APIBaseURL(bad); err == nil {
			t.Errorf("APIBaseURL(%q) accepted", bad)
		} else if strings.Contains(err.Error(), "pw@") || strings.Contains(err.Error(), "private_token=x") {
			t.Errorf("APIBaseURL(%q) error leaks credentials: %v", bad, err)
		}
	}
}

func TestAPIBaseURLIdempotent(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		host := rapid.StringMatching(`[a-z]{1,10}(\.[a-z]{2,5}){0,2}`).Draw(t, "host")
		path := rapid.StringMatching(`(/[a-z0-9]{1,6}){0,3}/?`).Draw(t, "path")
		u, err := gitlab.APIBaseURL("https://" + host + path)
		if err != nil {
			t.Fatalf("APIBaseURL: %v", err)
		}
		again, err := gitlab.APIBaseURL(u.String())
		if err != nil || again.String() != u.String() {
			t.Fatalf("not idempotent: %q → %q (%v)", u, again, err)
		}
		if !strings.HasSuffix(u.Path, "/api/v4/") {
			t.Fatalf("path %q lacks /api/v4/", u.Path)
		}
	})
}

func TestInfo(t *testing.T) {
	t.Parallel()
	p, err := gitlab.New(t.Context(), scm.Config{Name: "corp", APIURL: "https://gitlab.example.com/"})
	if err != nil {
		t.Fatal(err)
	}
	info := p.Info()
	if info.Type != "gitlab" || info.Name != "corp" || info.APIURL != "https://gitlab.example.com/api/v4" || info.Has(scm.CapabilityCommitStatuses) {
		t.Fatalf("info = %+v", info)
	}
	info.Capabilities[0] = "mutated"
	if p.Info().Capabilities[0] == "mutated" {
		t.Fatal("Info must return a copy of the capabilities")
	}
	if _, err := gitlab.New(t.Context(), scm.Config{APIURL: "ftp://x"}); scm.KindOf(err) != scm.KindInvalid {
		t.Fatalf("bad URL: %v", err)
	}
}
