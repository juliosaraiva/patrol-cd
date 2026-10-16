package azuredevops_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/azuredevops"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/conformance"
)

var whSecret = azuredevops.JoinSecret("argocd", "azdo-hook-password")

type azFixture struct {
	s    *seed
	data conformance.Data
}

func (f *azFixture) Data() conformance.Data { return f.data }

func (f *azFixture) WebhookDelivery(t testing.TB, typ scm.EventType, id string, secret []byte) (http.Header, []byte) {
	t.Helper()
	switch typ {
	case scm.EventPush:
		return delivery(secret), withID(t, "git_push.json", id)
	case scm.EventPullRequest:
		return delivery(secret), withID(t, "pr_created.json", id)
	}
	t.Fatalf("no fixture for %q", typ)
	return nil, nil
}

func (f *azFixture) SignatureHeaders() []string { return []string{"Authorization"} }

func (f *azFixture) InjectRateLimit(d time.Duration) { f.s.f.injectRateLimit(d) }

func (f *azFixture) CommitStatuses(scm.RepoRef, string) []scm.CommitStatus { return nil }

func (f *azFixture) CheckRuns(scm.RepoRef, string) []conformance.RecordedCheckRun { return nil }

func TestConformance(t *testing.T) {
	t.Parallel()
	conformance.Run(t, func(t *testing.T) (scm.Provider, conformance.Fixture) {
		s := newSeed(t)
		r := scm.NewRegistry()
		if err := azuredevops.Register(r); err != nil {
			t.Fatal(err)
		}
		cfg := s.config()
		cfg.APIURL = s.f.orgURL() // registry path: organization URL
		p, err := r.New(t.Context(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		return p, &azFixture{s: s, data: conformance.Data{
			Organization: project,
			Repositories: append(append([]string{}, s.svcNames...), "platform"),
			// Repositories without a default branch are skipped before filters (upstream).
			Repo:          scm.RepoRef{Owner: project, Name: "platform"},
			DefaultBranch: "main",
			Branches:      s.branches,
			ExistingPath:  "apps/a",
			MissingPath:   "apps/zzz",
			PullRequests:  s.openPRs,
			PRLabel:       "preview",
			MissingRepo:   scm.RepoRef{Owner: "Nope", Name: "nothing"},
		}}
	}, conformance.Options{Skip: []string{
		// Upstream ignores cloneProtocol for Azure DevOps: URL is always the remote
		// (HTTPS) URL. TestListRepositories covers it.
		"Repositories/CloneProtocol",
		// git.push payloads carry no changed-file list (upstream refreshes every matching
		// application); TestWebhookPush covers the other fields.
		"Webhooks/ValidSignature",
		// Basic authentication covers the sender, not the body (upstream identical).
		"Webhooks/TamperedBody",
	}})
}

func TestListRepositories(t *testing.T) {
	t.Parallel()
	s := newSeed(t)
	p := newProvider(t, s.config(), azuredevops.Options{Organization: "acme"})
	if p.Organization() != "acme" || p.Info().APIURL != s.f.orgURL() {
		t.Fatalf("organization %q, API URL %q", p.Organization(), p.Info().APIURL)
	}
	for _, proto := range []scm.CloneProtocol{scm.CloneProtocolDefault, scm.CloneProtocolSSH, scm.CloneProtocolHTTPS} {
		got, err := p.ListRepositories(t.Context(), scm.RepoQuery{Organization: project, CloneProtocol: proto})
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, r := range got {
			names = append(names, r.Name)
			if r.URL != r.HTTPSURL || !strings.Contains(r.URL, "/_git/") {
				t.Errorf("%s: URL %q must be the remote URL whatever the protocol (%q)", r.Name, r.URL, proto)
			}
		}
		if slices.Contains(names, "disabled") || slices.Contains(names, "empty") || len(names) != 8 {
			t.Fatalf("repositories = %v", names)
		}
	}
	got, err := p.ListRepositories(t.Context(), scm.RepoQuery{Organization: project, Match: func(r scm.Repository) bool { return r.Name == "platform" }})
	if err != nil || len(got) != 1 {
		t.Fatal(err)
	}
	r := got[0]
	if r.Organization != project || r.RepositoryID != "11111111-2222-3333-4444-555555555555" || r.Branch != "main" || r.SHA != sha(1) ||
		!strings.HasPrefix(r.SSHURL, "git@ssh.") || r.Labels == nil || len(r.Labels) != 0 || r.Ref() != (scm.RepoRef{Owner: project, Name: "platform"}) {
		t.Fatalf("platform = %+v", r)
	}
	if _, err := p.ListRepositories(t.Context(), scm.RepoQuery{Organization: project, CloneProtocol: "gopher"}); scm.KindOf(err) != scm.KindInvalid {
		t.Fatalf("unknown protocol: %v", err)
	}
	if !slices.ContainsFunc(s.f.requestLog(), func(r string) bool {
		return strings.Contains(r, "/acme/Platform%20Team/_apis/git/repositories?api-version=7.1")
	}) {
		t.Fatalf("project name not escaped: %v", s.f.requestLog())
	}
}

func TestAuthentication(t *testing.T) {
	t.Parallel()
	s := newSeed(t)
	ref := scm.RepoRef{Owner: project, Name: "platform"}
	cfg := s.config()
	cfg.Token = func(context.Context) (string, error) { return "expired", nil }
	_, err := newProvider(t, cfg, azuredevops.Options{Organization: "acme"}).ListBranches(t.Context(), ref, true)
	if scm.KindOf(err) != scm.KindUnauthorized || !strings.Contains(err.Error(), "sign-in page") || strings.Contains(err.Error(), "<html>") {
		t.Fatalf("203 sign-in page: %v", err)
	}
	cfg.Token = nil
	if _, err := newProvider(t, cfg, azuredevops.Options{Organization: "acme"}).ListBranches(t.Context(), ref, true); scm.KindOf(err) != scm.KindUnauthorized {
		t.Fatalf("anonymous on a private organization: %v", err)
	}
}

func TestPullRequests(t *testing.T) {
	t.Parallel()
	s := newSeed(t)
	p := newProvider(t, s.config(), azuredevops.Options{Organization: "acme"})
	ref := scm.RepoRef{Owner: project, Name: "platform"}
	for state, want := range map[scm.PRState][]int64{"": {1, 2, 3, 4}, scm.PRStateMerged: {5}, scm.PRStateClosed: {5, 6}, scm.PRStateAll: {1, 2, 3, 4, 5, 6}} {
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
	got, err := p.ListPullRequests(t.Context(), ref, scm.PRQuery{Labels: []string{"preview", "team-a"}, TargetBranch: "main"})
	if err != nil || len(got) != 1 {
		t.Fatalf("labels: %+v, %v", got, err)
	}
	pr := got[0]
	if pr.Number != 2 || pr.Branch != "feature-b" || pr.TargetBranch != "main" || pr.HeadSHA != sha(3) || pr.Author != "bob" || pr.State != "active" ||
		pr.URL != s.f.orgURL()+"/"+project+"/_git/platform/pullrequest/2" {
		t.Fatalf("pr = %+v", pr)
	}
	pages := 0
	for _, r := range s.f.requestLog() {
		if strings.Contains(r, "/pullrequests?") && strings.Contains(r, "%24skip=100") {
			pages++
		}
	}
	if pages == 0 {
		t.Fatal("pull requests must be paged with $top/$skip")
	}
	// A missing repository in an existing project yields no pull requests (upstream).
	if got, err := p.ListPullRequests(t.Context(), scm.RepoRef{Owner: project, Name: "missing"}, scm.PRQuery{}); err != nil || len(got) != 0 {
		t.Fatalf("missing repository: %v, %v", got, err)
	}
	_, err = p.ListPullRequests(t.Context(), scm.RepoRef{Owner: "Nope", Name: "x"}, scm.PRQuery{})
	if !scm.IsNotFound(err) || !strings.Contains(err.Error(), "The following project does not exist") {
		t.Fatalf("missing project: %v", err)
	}
}

func TestResolveAndPaths(t *testing.T) {
	t.Parallel()
	s := newSeed(t)
	s.f.refPage = 1 // force continuation tokens
	p := newProvider(t, s.config(), azuredevops.Options{Organization: "acme"})
	ref := scm.RepoRef{Owner: project, Name: "platform"}
	for in, want := range map[string]string{"main": sha(1), "feature/a": sha(2), "v1.2.0": sha(4), "refs/tags/v1.2.0": sha(4), "v1.2.0-rc": sha(3), sha(5): sha(5)} {
		if got, err := p.ResolveRevision(t.Context(), ref, in); err != nil || got != want {
			t.Errorf("ResolveRevision(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := p.ResolveRevision(t.Context(), ref, "feature"); !scm.IsNotFound(err) {
		t.Errorf("prefix filter must match exactly: %v", err)
	}
	for _, tc := range []struct {
		rev, path string
		want      bool
	}{
		{"main", "apps/a", true}, {"main", "apps/a/values.yaml", true}, {"", "README.md", true}, {sha(1), "dir with space/f#1.yaml", true},
		{"main", "", true}, {"main", "apps/b", false}, {"feature/a", "apps/b", true}, {"refs/heads/feature/a", "apps/b", true}, {"nope", "apps/a", false},
	} {
		if got, err := p.PathExists(t.Context(), ref, tc.rev, tc.path); err != nil || got != tc.want {
			t.Errorf("PathExists(%q, %q) = %v, %v; want %v", tc.rev, tc.path, got, err, tc.want)
		}
	}
	empty, err := p.ListBranches(t.Context(), scm.RepoRef{Owner: project, Name: "empty"}, false)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty repository: %v, %v", empty, err)
	}
}

func TestOrganizationURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		api, org, want, wantOrg string
	}{
		{"", "acme", "https://dev.azure.com/acme/", "acme"},
		{"https://dev.azure.com/", "acme", "https://dev.azure.com/acme/", "acme"},
		{"https://dev.azure.com/acme", "", "https://dev.azure.com/acme/", "acme"},
		{"https://tfs.example.com/tfs", "Default Collection", "https://tfs.example.com/tfs/Default%20Collection/", "Default Collection"},
		{"https://tfs.example.com/tfs/Default%20Collection/", "", "https://tfs.example.com/tfs/Default%20Collection/", "Default Collection"},
	}
	for _, tc := range tests {
		u, org, err := azuredevops.OrganizationURL(tc.api, tc.org)
		if err != nil || u.String() != tc.want || org != tc.wantOrg {
			t.Errorf("OrganizationURL(%q, %q) = %v, %q, %v; want %q, %q", tc.api, tc.org, u, org, err, tc.want, tc.wantOrg)
		}
	}
	for _, bad := range [][2]string{{"", ""}, {"https://dev.azure.com", ""}, {"ftp://x", "a"}, {"", "../x"}, {"", "a/b"}} {
		if _, _, err := azuredevops.OrganizationURL(bad[0], bad[1]); err == nil {
			t.Errorf("OrganizationURL(%q, %q) accepted", bad[0], bad[1])
		}
	}
}

func TestFixturesAreValidJSON(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		var v any
		if err := json.Unmarshal(fixture(t, e.Name()), &v); err != nil {
			t.Errorf("%s: %v", e.Name(), err)
		}
	}
}

func TestWebhookPush(t *testing.T) {
	t.Parallel()
	ev, err := azuredevops.WebhookVerifier{}.VerifyAndParse(delivery(whSecret), withID(t, "git_push.json", "evt-1"), whSecret)
	if err != nil {
		t.Fatal(err)
	}
	if !ev.Verified || ev.Provider != "azuredevops" || ev.DeliveryID != "evt-1" || ev.Type != scm.EventPush || ev.Ref != "refs/heads/main" ||
		ev.Revision != "main" || ev.Before != sha(99) || ev.After != sha(1) || !ev.TouchedDefaultBranch || !ev.ChangedFilesTruncated || ev.Sender != "alice" {
		t.Fatalf("event = %+v", ev)
	}
	r := ev.Repository
	if r.Owner != project || r.Name != "platform" || r.ID != "11111111-2222-3333-4444-555555555555" ||
		r.HTMLURL != "https://acme@dev.azure.com/acme/Platform%20Team/_git/platform" || r.DefaultBranch != "main" || !r.Private {
		t.Fatalf("repository = %+v", r)
	}
	// Without an event id the activity ID is the delivery ID.
	body := []byte(strings.Replace(string(withID(t, "git_push.json", "")), `"id": "",`, ``, 1))
	ev, err = azuredevops.WebhookVerifier{}.VerifyAndParse(delivery(whSecret), body, whSecret)
	if err != nil || ev.DeliveryID != "8ed2f0f2-4f15-4e4b-9b7e-2c1c3a5d7e9f" {
		t.Fatalf("activity ID fallback: %+v, %v", ev, err)
	}
}

func TestWebhookPullRequests(t *testing.T) {
	t.Parallel()
	v := azuredevops.WebhookVerifier{}
	ev, err := v.VerifyAndParse(delivery(whSecret), withID(t, "pr_created.json", "e1"), whSecret)
	if err != nil {
		t.Fatal(err)
	}
	pr := ev.PullRequest
	if ev.Action != scm.PRActionOpened || pr.Number != 1 || pr.Branch != "feature/a" || pr.TargetBranch != "main" || pr.HeadSHA != sha(2) ||
		pr.Author != "alice" || !slices.Equal(pr.Labels, []string{"preview"}) || ev.Repository.Owner != project {
		t.Fatalf("created = %+v / %+v", ev, pr)
	}
	ev, err = v.VerifyAndParse(delivery(whSecret), withID(t, "pr_updated_completed.json", "e2"), whSecret)
	if err != nil || ev.Action != scm.PRActionClosed || !ev.PullRequest.Merged || !ev.PullRequest.Draft || ev.Repository.Private {
		t.Fatalf("completed = %+v, %v", ev, err)
	}
}

func TestWebhookAuthentication(t *testing.T) {
	t.Parallel()
	v := azuredevops.WebhookVerifier{}
	body := withID(t, "git_push.json", "e")
	tests := []struct {
		name     string
		h        http.Header
		sentinel error
	}{
		{"wrong password", delivery(azuredevops.JoinSecret("argocd", "nope")), scm.ErrSignatureMismatch},
		{"wrong user", delivery(azuredevops.JoinSecret("root", "azdo-hook-password")), scm.ErrSignatureMismatch},
		{"no auth", delivery(nil), scm.ErrSignatureMissing},
		{"bearer", http.Header{"X-Vss-Activityid": {"a"}, "Authorization": {"Bearer x"}}, scm.ErrSignatureMissing},
	}
	for _, tc := range tests {
		if _, err := v.VerifyAndParse(tc.h, body, whSecret); scm.KindOf(err) != scm.KindUnauthorized || !errors.Is(err, tc.sentinel) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	// Password only (upstream with an empty username).
	pw := []byte("only-password")
	if ev, err := v.VerifyAndParse(delivery(pw), body, pw); err != nil || !ev.Verified {
		t.Fatalf("password-only secret: %v", err)
	}
	if _, err := v.VerifyAndParse(delivery(whSecret), []byte(`{"eventType":"workitem.created","resource":{}}`), whSecret); !errors.Is(err, scm.ErrUnsupportedEvent) {
		t.Fatalf("unsupported: %v", err)
	}
	if _, err := v.VerifyAndParse(delivery(whSecret), []byte(`{"eventType":"git.push"}`), whSecret); !errors.Is(err, scm.ErrMalformedPayload) {
		t.Fatalf("missing resource: %v", err)
	}
	if _, err := v.VerifyAndParse(delivery(whSecret), withID(t, "git_push.json", strings.Repeat("x", 200)), whSecret); !errors.Is(err, scm.ErrInvalidDeliveryID) {
		t.Fatalf("oversized delivery ID: %v", err)
	}
	if !v.MatchesWebhook(delivery(nil)) || v.MatchesWebhook(http.Header{"X-Github-Event": {"push"}}) {
		t.Fatal("MatchesWebhook must key on X-Vss-Activityid")
	}
}

func TestSecretSplitProperty(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		user := rapid.StringMatching(`[A-Za-z0-9._-]{0,16}`).Draw(t, "user")
		pass := rapid.String().Draw(t, "pass")
		if user == "" && pass == "" {
			if azuredevops.JoinSecret(user, pass) != nil {
				t.Fatal("empty credentials must yield no secret")
			}
			return
		}
		u, p := azuredevops.SplitSecret(azuredevops.JoinSecret(user, pass))
		if string(u) != user || string(p) != pass {
			t.Fatalf("split(join(%q, %q)) = %q, %q", user, pass, u, p)
		}
	})
}
