package bitbucketcloud_test

import (
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
	"github.com/juliosaraiva/patrol-cd/pkg/scm/bitbucketcloud"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/conformance"
)

type bcFixture struct {
	s    *seed
	data conformance.Data
}

func (f *bcFixture) Data() conformance.Data { return f.data }

func (f *bcFixture) WebhookDelivery(t testing.TB, typ scm.EventType, id string, secret []byte) (http.Header, []byte) {
	t.Helper()
	switch typ {
	case scm.EventPush:
		return delivery("repo:push", id, secret), fixture(t, "repo_push.json")
	case scm.EventPullRequest:
		return delivery("pullrequest:created", id, secret), fixture(t, "pullrequest_created.json")
	}
	t.Fatalf("no fixture for %q", typ)
	return nil, nil
}

func (f *bcFixture) SignatureHeaders() []string { return []string{bitbucketcloud.HeaderHookUUID} }

func (f *bcFixture) InjectRateLimit(d time.Duration) { f.s.f.injectRateLimit(d) }

func (f *bcFixture) CommitStatuses(scm.RepoRef, string) []scm.CommitStatus { return nil }

func (f *bcFixture) CheckRuns(scm.RepoRef, string) []conformance.RecordedCheckRun { return nil }

func TestConformance(t *testing.T) {
	t.Parallel()
	conformance.Run(t, func(t *testing.T) (scm.Provider, conformance.Fixture) {
		s := newSeed(t)
		return basicProvider(t, s), &bcFixture{s: s, data: conformance.Data{
			Organization:    "acme",
			Repositories:    append(append([]string{}, s.svcNames...), "platform"),
			EmptyRepository: "empty",
			Repo:            scm.RepoRef{Owner: "acme", Name: "platform"},
			DefaultBranch:   "main",
			Branches:        s.branches,
			ExistingPath:    "apps/a",
			MissingPath:     "apps/zzz",
			PullRequests:    s.openPRs,
			MissingRepo:     scm.RepoRef{Owner: "nobody", Name: "nothing"},
		}}
	}, conformance.Options{Skip: []string{
		// No pull request labels on Bitbucket Cloud (none upstream either); a label filter
		// fails with KindInvalid (TestPullRequests).
		"PullRequests/Labels",
		// repo:push carries no changed-file list; upstream (and Provider.ChangedFiles)
		// compute it with the diffstat API. TestWebhookPush covers the other fields.
		"Webhooks/ValidSignature",
		// The webhook UUID authenticates the sender but does not cover the body, so body
		// tampering is undetectable (upstream behaves the same).
		"Webhooks/TamperedBody",
	}})
}

func TestListRepositories(t *testing.T) {
	t.Parallel()
	s := newSeed(t)
	p := basicProvider(t, s)
	got, err := p.ListRepositories(t.Context(), scm.RepoQuery{Organization: "acme", CloneProtocol: scm.CloneProtocolHTTPS, Match: func(r scm.Repository) bool { return r.Name == "platform" }})
	if err != nil || len(got) != 1 {
		t.Fatalf("got %+v, %v", got, err)
	}
	r := got[0]
	if r.Organization != "acme" || r.RepositoryID != "{11111111-2222-3333-4444-555555555555}" || r.URL != "https://x-token-auth@bitbucket.org/acme/platform.git" ||
		r.SSHURL != "git@bitbucket.org:acme/platform.git" || r.SHA != sha(1) || r.Branch != "main" || r.Labels == nil || len(r.Labels) != 0 || !r.Private {
		t.Fatalf("platform = %+v", r)
	}
	if !strings.Contains(s.f.requestLog()[0], "role=member") {
		t.Fatalf("repositories must be listed with role=member: %v", s.f.requestLog()[0])
	}
	if _, err := p.ListRepositories(t.Context(), scm.RepoQuery{Organization: "acme", CloneProtocol: "git"}); scm.KindOf(err) != scm.KindInvalid ||
		!strings.Contains(err.Error(), "unknown clone protocol for Bitbucket cloud git") {
		t.Fatalf("unknown protocol: %v", err)
	}
}

func TestNextLinkMustStayOnAPIHost(t *testing.T) {
	t.Parallel()
	s := newSeed(t)
	s.f.foreignNxt = "http://evil.example"
	_, err := basicProvider(t, s).ListRepositories(t.Context(), scm.RepoQuery{Organization: "acme"})
	if scm.KindOf(err) != scm.KindInvalid || !strings.Contains(err.Error(), "outside the configured API host") {
		t.Fatalf("foreign next link: %v", err)
	}
}

func TestAuthentication(t *testing.T) {
	t.Parallel()
	ref := scm.RepoRef{Owner: "acme", Name: "platform"}
	for name, tc := range map[string]struct {
		user, token, want string
	}{
		"basic":        {testUser, testAppPassword, basic(testUser, testAppPassword)},
		"bearer":       {"", "repo-access-token", "Bearer repo-access-token"},
		"x-token-auth": {bitbucketcloud.TokenAuthUsername, "repo-access-token", "Bearer repo-access-token"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newSeed(t)
			s.f.auth = tc.want
			if _, err := newProvider(t, s.config(tc.token), bitbucketcloud.Options{Username: tc.user}).ListBranches(t.Context(), ref, true); err != nil {
				t.Fatal(err)
			}
		})
	}
	s := newSeed(t)
	if _, err := newProvider(t, s.config("wrong"), bitbucketcloud.Options{Username: testUser}).ListBranches(t.Context(), ref, true); scm.KindOf(err) != scm.KindUnauthorized {
		t.Fatalf("wrong password: %v", err)
	}
}

func TestPullRequests(t *testing.T) {
	t.Parallel()
	s := newSeed(t)
	p := basicProvider(t, s)
	ref := scm.RepoRef{Owner: "acme", Name: "platform"}
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
	got, err := p.ListPullRequests(t.Context(), ref, scm.PRQuery{TargetBranch: "release-1"})
	if err != nil || len(got) != 1 || got[0].Author != "carol" || got[0].HeadSHA != sha(5)[:12] || got[0].URL != "https://bitbucket.org/acme/platform/pull-requests/3" {
		t.Fatalf("target branch: %+v, %v", got, err)
	}
	if _, err := p.ListPullRequests(t.Context(), ref, scm.PRQuery{Labels: []string{"x"}}); !errors.Is(err, bitbucketcloud.ErrLabelsUnsupported) || scm.KindOf(err) != scm.KindInvalid {
		t.Fatalf("labels: %v", err)
	}
	if _, err := p.ListPullRequests(t.Context(), scm.RepoRef{Owner: "acme", Name: "missing"}, scm.PRQuery{}); !scm.IsNotFound(err) {
		t.Fatalf("missing repository: %v", err)
	}
}

func TestResolveAndPaths(t *testing.T) {
	t.Parallel()
	s := newSeed(t)
	p := basicProvider(t, s)
	ref := scm.RepoRef{Owner: "acme", Name: "platform"}
	for in, want := range map[string]string{"main": sha(1), "feature/a": sha(2), "refs/tags/v1.2.0": sha(4), "v1.2.0": sha(4), sha(3): sha(3)} {
		if got, err := p.ResolveRevision(t.Context(), ref, in); err != nil || got != want {
			t.Errorf("ResolveRevision(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := p.ResolveRevision(t.Context(), ref, "refs/heads/v1.2.0"); !scm.IsNotFound(err) {
		t.Errorf("refs/heads/ must not match tags: %v", err)
	}
	for _, tc := range []struct {
		rev, path string
		want      bool
	}{
		{"main", "apps/a", true}, {"main", "apps/a/values.yaml", true}, {"", "README.md", true}, {sha(1), "dir with space/f#1.yaml", true},
		{"main", "", true}, {"main", "apps/b", false}, {"feature/a", "apps/b", true}, {"nope", "apps/a", false},
	} {
		if got, err := p.PathExists(t.Context(), ref, tc.rev, tc.path); err != nil || got != tc.want {
			t.Errorf("PathExists(%q, %q) = %v, %v", tc.rev, tc.path, got, err)
		}
	}
	if ok, err := p.PathExists(t.Context(), scm.RepoRef{Owner: "acme", Name: "missing"}, "", "a"); ok || err != nil {
		t.Errorf("missing repository: %v, %v", ok, err)
	}
}

func TestChangedFilesAndMainBranch(t *testing.T) {
	t.Parallel()
	s := newSeed(t)
	s.f.diffstats[sha(99)+".."+sha(1)] = []map[string]any{
		{"type": "diffstat", "status": "modified", "old": map[string]any{"path": "apps/a/values.yaml"}, "new": map[string]any{"path": "apps/a/values.yaml"}},
		{"type": "diffstat", "status": "added", "old": nil, "new": map[string]any{"path": "apps/a/configmap.yaml"}},
		{"type": "diffstat", "status": "removed", "old": map[string]any{"path": "apps/old/deployment.yaml"}, "new": nil},
		{"type": "diffstat", "status": "renamed", "old": map[string]any{"path": "a/x.yaml"}, "new": map[string]any{"path": "b/x.yaml"}},
	}
	p := basicProvider(t, s)
	ref := scm.RepoRef{Owner: "acme", Name: "platform"}
	files, truncated, err := p.ChangedFiles(t.Context(), ref, sha(99), sha(1))
	want := []string{"apps/a/values.yaml", "apps/a/configmap.yaml", "apps/old/deployment.yaml", "b/x.yaml", "a/x.yaml"}
	if err != nil || truncated || !slices.Equal(files, want) {
		t.Fatalf("ChangedFiles = %v, %v, %v; want %v", files, truncated, err, want)
	}
	if _, truncated, err := p.ChangedFiles(t.Context(), ref, strings.Repeat("0", 40), sha(1)); err != nil || !truncated {
		t.Fatalf("branch creation must report truncated: %v, %v", truncated, err)
	}
	if _, _, err := p.ChangedFiles(t.Context(), ref, "main", sha(1)); scm.KindOf(err) != scm.KindInvalid {
		t.Fatalf("non-SHA spec: %v", err)
	}
	for rev, want := range map[string]bool{"main": true, "hotfix": false} {
		if got, err := p.TouchesMainBranch(t.Context(), ref, rev); err != nil || got != want {
			t.Errorf("TouchesMainBranch(%q) = %v, %v", rev, got, err)
		}
	}
}

func TestAPIBaseURLFromRepositoryURL(t *testing.T) {
	t.Parallel()
	got, err := bitbucketcloud.APIBaseURLFromRepositoryURL("https://api.bitbucket.org/2.0/repositories/acme/platform", "acme/platform")
	if err != nil || got != "https://api.bitbucket.org/2.0" {
		t.Fatalf("got %q, %v", got, err)
	}
	for _, bad := range [][2]string{
		{"https://api.bitbucket.org/2.0/repositories/acme/other", "acme/platform"},
		{"javascript:alert(1)/repositories/acme/platform", "acme/platform"},
		{"https://api.bitbucket.org/2.0/repositories/acme/platform", ""},
	} {
		if _, err := bitbucketcloud.APIBaseURLFromRepositoryURL(bad[0], bad[1]); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
	if u, err := bitbucketcloud.APIBaseURL(""); err != nil || u.String() != "https://api.bitbucket.org/2.0/" {
		t.Fatalf("default API URL: %v, %v", u, err)
	}
}

var whUUID = []byte(hookUUID)

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
	ev, err := bitbucketcloud.WebhookVerifier{}.VerifyAndParse(delivery("repo:push", "req-1", whUUID), fixture(t, "repo_push.json"), whUUID)
	if err != nil {
		t.Fatal(err)
	}
	if !ev.Verified || ev.Provider != "bitbucketcloud" || ev.DeliveryID != "req-1" || ev.Type != scm.EventPush || ev.Ref != "refs/heads/main" ||
		ev.Revision != "main" || ev.Before != sha(99) || ev.After != sha(1) || !ev.TouchedDefaultBranch || !ev.ChangedFilesTruncated || ev.Sender != "alice" {
		t.Fatalf("event = %+v", ev)
	}
	r := ev.Repository
	if r.Owner != "acme" || r.Name != "platform" || r.ID != "{11111111-2222-3333-4444-555555555555}" || r.HTMLURL != "https://bitbucket.org/acme/platform" ||
		r.APIURL != "https://api.bitbucket.org/2.0/repositories/acme/platform" || !r.Private {
		t.Fatalf("repository = %+v", r)
	}
	base, err := bitbucketcloud.APIBaseURLFromRepositoryURL(r.APIURL, r.FullName())
	if err != nil || base != "https://api.bitbucket.org/2.0" {
		t.Fatalf("API base from event: %q, %v", base, err)
	}
	deleted := []byte(`{"push":{"changes":[{"old":{"type":"tag","name":"v1","target":{"hash":"` + sha(4) + `"}},"new":null,"closed":true}]},
		"repository":{"full_name":"acme/platform"}}`)
	ev, err = bitbucketcloud.WebhookVerifier{}.VerifyAndParse(delivery("repo:push", "req-2", whUUID), deleted, whUUID)
	if err != nil || ev.Ref != "refs/tags/v1" || ev.Revision != "" || !ev.Deleted || ev.Before != sha(4) || ev.After != "" {
		t.Fatalf("deleted tag: %+v, %v", ev, err)
	}
}

func TestWebhookPullRequests(t *testing.T) {
	t.Parallel()
	v := bitbucketcloud.WebhookVerifier{}
	ev, err := v.VerifyAndParse(delivery("pullrequest:created", "r", whUUID), fixture(t, "pullrequest_created.json"), whUUID)
	if err != nil || ev.Action != scm.PRActionOpened || ev.PullRequest.Number != 1 || ev.PullRequest.Branch != "feature/a" || ev.PullRequest.Author != "alice" ||
		ev.PullRequest.URL != "https://bitbucket.org/acme/platform/pull-requests/1" {
		t.Fatalf("created: %+v, %v", ev, err)
	}
	ev, err = v.VerifyAndParse(delivery("pullrequest:fulfilled", "r", whUUID), fixture(t, "pullrequest_fulfilled.json"), whUUID)
	if err != nil || ev.Action != scm.PRActionClosed || !ev.PullRequest.Merged || ev.Repository.Private {
		t.Fatalf("fulfilled: %+v, %v", ev, err)
	}
}

func TestWebhookRejections(t *testing.T) {
	t.Parallel()
	v := bitbucketcloud.WebhookVerifier{}
	push := fixture(t, "repo_push.json")
	for name, tc := range map[string]struct {
		h        http.Header
		body     []byte
		sentinel error
	}{
		"wrong uuid":    {delivery("repo:push", "1", []byte("{00000000-0000-0000-0000-000000000000}")), push, scm.ErrSignatureMismatch},
		"missing uuid":  {delivery("repo:push", "1", nil), push, scm.ErrSignatureMissing},
		"unsupported":   {delivery("issue:created", "1", whUUID), []byte(`{}`), scm.ErrUnsupportedEvent},
		"malformed":     {delivery("repo:push", "1", whUUID), []byte(`[`), scm.ErrMalformedPayload},
		"no repository": {delivery("repo:push", "1", whUUID), []byte(`{"push":{}}`), scm.ErrMalformedPayload},
	} {
		if _, err := v.VerifyAndParse(tc.h, tc.body, whUUID); !errors.Is(err, tc.sentinel) {
			t.Errorf("%s: %v, want %v", name, err, tc.sentinel)
		}
	}
	if !v.MatchesWebhook(delivery("repo:push", "1", whUUID)) || v.MatchesWebhook(http.Header{"X-Event-Key": {"repo:refs_changed"}}) {
		t.Fatal("MatchesWebhook must key on X-Hook-UUID")
	}
}

func TestUUIDComparisonProperty(t *testing.T) {
	t.Parallel()
	v := bitbucketcloud.WebhookVerifier{}
	push := fixture(t, "repo_push.json")
	rapid.Check(t, func(t *rapid.T) {
		secret := rapid.StringMatching(`\{[0-9a-f-]{1,36}\}`).Draw(t, "secret")
		presented := rapid.OneOf(rapid.Just(secret), rapid.StringMatching(`\{[0-9a-f-]{1,36}\}`)).Draw(t, "presented")
		_, err := v.VerifyAndParse(delivery("repo:push", "1", []byte(presented)), push, []byte(secret))
		if (err == nil) != (presented == secret) {
			t.Fatalf("presented %q for secret %q: %v", presented, secret, err)
		}
	})
}
