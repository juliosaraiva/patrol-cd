package gitlab_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/conformance"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/gitlab"
)

type glFixture struct {
	s    *seed
	data conformance.Data
}

func (f *glFixture) Data() conformance.Data { return f.data }

func (f *glFixture) WebhookDelivery(t testing.TB, typ scm.EventType, id string, secret []byte) (http.Header, []byte) {
	t.Helper()
	switch typ {
	case scm.EventPush:
		return delivery(gitlab.EventPushHook, id, secret), fixture(t, "push.json")
	case scm.EventPullRequest:
		return delivery(gitlab.EventMergeRequestHook, id, secret), fixture(t, "merge_request_open.json")
	}
	t.Fatalf("no fixture for %q", typ)
	return nil, nil
}

func (f *glFixture) SignatureHeaders() []string { return []string{gitlab.HeaderToken} }

func (f *glFixture) InjectRateLimit(d time.Duration) { f.s.f.injectRateLimit(d) }

func (f *glFixture) CommitStatuses(scm.RepoRef, string) []scm.CommitStatus { return nil }

func (f *glFixture) CheckRuns(scm.RepoRef, string) []conformance.RecordedCheckRun { return nil }

func conformanceData(s *seed) conformance.Data {
	return conformance.Data{
		Organization:         "acme",
		Repositories:         append(append([]string{}, s.svcNames...), "platform", "shared"),
		ArchivedRepositories: []string{"legacy"},
		EmptyRepository:      "empty",
		Repo:                 scm.RepoRef{Owner: "acme", Name: "platform"},
		DefaultBranch:        "main",
		Branches:             s.branches,
		ExistingPath:         "apps/a",
		MissingPath:          "apps/zzz",
		PullRequests:         s.openMRs,
		PRLabel:              "preview",
		MissingRepo:          scm.RepoRef{Owner: "nobody", Name: "nothing"},
	}
}

func TestConformance(t *testing.T) {
	t.Parallel()
	conformance.Run(t, func(t *testing.T) (scm.Provider, conformance.Fixture) {
		s := newSeed(t)
		return newProvider(t, s.config(), gitlab.Options{}), &glFixture{s: s, data: conformanceData(s)}
	}, conformance.Options{Skip: []string{
		// GitLab authenticates webhooks with a shared token header and does not sign the
		// body, so body tampering is undetectable by design (upstream behaves the same).
		// TestWebhookTokenDoesNotCoverBody documents it.
		"Webhooks/TamperedBody",
	}})
}

// TestConformanceViaRegistry builds the provider through the scm registry with a
// numeric-group fixture to cover the registry path and default options.
func TestConformanceViaRegistry(t *testing.T) {
	t.Parallel()
	conformance.Run(t, func(t *testing.T) (scm.Provider, conformance.Fixture) {
		s := newSeed(t)
		r := scm.NewRegistry()
		if err := gitlab.Register(r); err != nil {
			t.Fatal(err)
		}
		p, err := r.New(t.Context(), s.config())
		if err != nil {
			t.Fatal(err)
		}
		d := conformanceData(s)
		d.Organization = "10" // numeric group ID of acme
		return p, &glFixture{s: s, data: d}
	}, conformance.Options{Skip: []string{"Webhooks/TamperedBody"}})
}
