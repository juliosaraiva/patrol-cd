package bitbucketserver_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/bitbucketserver"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/conformance"
)

type bsFixture struct {
	s    *seed
	data conformance.Data
}

func (f *bsFixture) Data() conformance.Data { return f.data }

func (f *bsFixture) WebhookDelivery(t testing.TB, typ scm.EventType, id string, secret []byte) (http.Header, []byte) {
	t.Helper()
	switch typ {
	case scm.EventPush:
		body := fixture(t, "repo_refs_changed.json")
		return delivery("repo:refs_changed", id, body, secret), body
	case scm.EventPullRequest:
		body := fixture(t, "pr_opened.json")
		return delivery("pr:opened", id, body, secret), body
	}
	t.Fatalf("no fixture for %q", typ)
	return nil, nil
}

func (f *bsFixture) SignatureHeaders() []string { return []string{bitbucketserver.HeaderSignature} }

func (f *bsFixture) InjectRateLimit(d time.Duration) { f.s.f.injectRateLimit(d) }

func (f *bsFixture) CommitStatuses(scm.RepoRef, string) []scm.CommitStatus { return nil }

func (f *bsFixture) CheckRuns(scm.RepoRef, string) []conformance.RecordedCheckRun { return nil }

var bbsSkips = []string{
	// Bitbucket Server pull requests have no labels (upstream has no labels filter for
	// Bitbucket Server); a label filter fails with KindInvalid, see TestPullRequests.
	"PullRequests/Labels",
	// repo:refs_changed payloads carry no changed-file list (upstream refreshes every
	// matching application); TestWebhookPush covers the remaining fields.
	"Webhooks/ValidSignature",
}

func conformanceData(s *seed) conformance.Data {
	return conformance.Data{
		Organization:         "PROJ",
		Repositories:         append(append([]string{}, s.svcNames...), "platform"),
		ArchivedRepositories: []string{"legacy"},
		// Repositories without a default branch ("empty", "nodefault") are skipped before
		// repository-level filters, as upstream; so no EmptyRepository is reported.
		Repo:          scm.RepoRef{Owner: "PROJ", Name: "platform"},
		DefaultBranch: "main",
		Branches:      s.branches,
		ExistingPath:  "apps/a",
		MissingPath:   "apps/zzz",
		PullRequests:  s.openPRs,
		MissingRepo:   scm.RepoRef{Owner: "NOBODY", Name: "nothing"},
	}
}

func TestConformanceBasicAuth(t *testing.T) {
	t.Parallel()
	conformance.Run(t, func(t *testing.T) (scm.Provider, conformance.Fixture) {
		s := newSeed(t)
		return basicProvider(t, s), &bsFixture{s: s, data: conformanceData(s)}
	}, conformance.Options{Skip: bbsSkips})
}

func TestConformanceBearerViaRegistry(t *testing.T) {
	t.Parallel()
	conformance.Run(t, func(t *testing.T) (scm.Provider, conformance.Fixture) {
		s := newSeed(t)
		s.f.auth = "Bearer " + testBearer
		r := scm.NewRegistry()
		if err := bitbucketserver.Register(r); err != nil {
			t.Fatal(err)
		}
		p, err := r.New(t.Context(), s.config(testBearer))
		if err != nil {
			t.Fatal(err)
		}
		return p, &bsFixture{s: s, data: conformanceData(s)}
	}, conformance.Options{Skip: bbsSkips})
}
