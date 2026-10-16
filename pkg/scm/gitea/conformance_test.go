package gitea_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/conformance"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/gitea"
)

type gtFixture struct {
	s    *seed
	data conformance.Data
}

func (f *gtFixture) Data() conformance.Data { return f.data }

func (f *gtFixture) WebhookDelivery(t testing.TB, typ scm.EventType, id string, secret []byte) (http.Header, []byte) {
	t.Helper()
	switch typ {
	case scm.EventPush:
		body := fixture(t, "push.json")
		return delivery("push", id, body, secret), body
	case scm.EventPullRequest:
		body := fixture(t, "pull_request_opened.json")
		return delivery("pull_request", id, body, secret), body
	}
	t.Fatalf("no fixture for %q", typ)
	return nil, nil
}

func (f *gtFixture) SignatureHeaders() []string {
	return []string{"X-Gitea-Signature", "X-Gogs-Signature", "X-Hub-Signature-256"}
}

func (f *gtFixture) InjectRateLimit(d time.Duration) { f.s.f.injectRateLimit(d) }

func (f *gtFixture) CommitStatuses(scm.RepoRef, string) []scm.CommitStatus { return nil }

func (f *gtFixture) CheckRuns(scm.RepoRef, string) []conformance.RecordedCheckRun { return nil }

func TestConformance(t *testing.T) {
	t.Parallel()
	conformance.Run(t, func(t *testing.T) (scm.Provider, conformance.Fixture) {
		s := newSeed(t)
		r := scm.NewRegistry()
		if err := gitea.Register(r); err != nil {
			t.Fatal(err)
		}
		p, err := r.New(t.Context(), s.config())
		if err != nil {
			t.Fatal(err)
		}
		return p, &gtFixture{s: s, data: conformance.Data{
			Organization:         "acme",
			Repositories:         append(append([]string{}, s.svcNames...), "platform"),
			ArchivedRepositories: []string{"legacy"},
			EmptyRepository:      "empty",
			Repo:                 scm.RepoRef{Owner: "acme", Name: "platform"},
			DefaultBranch:        "main",
			Branches:             s.branches,
			ExistingPath:         "apps/a",
			MissingPath:          "apps/zzz",
			PullRequests:         s.openPRs,
			PRLabel:              "preview",
			MissingRepo:          scm.RepoRef{Owner: "nobody", Name: "nothing"},
		}}
	}, conformance.Options{})
}
