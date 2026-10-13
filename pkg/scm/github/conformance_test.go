package github_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/conformance"
)

type ghFixture struct {
	s    *seed
	data conformance.Data
}

func (f *ghFixture) Data() conformance.Data { return f.data }

func (f *ghFixture) WebhookDelivery(t testing.TB, typ scm.EventType, id string, secret []byte) (http.Header, []byte) {
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

func (f *ghFixture) SignatureHeaders() []string {
	return []string{"X-Hub-Signature-256", "X-Hub-Signature"}
}

func (f *ghFixture) InjectRateLimit(d time.Duration) { f.s.srv.InjectRateLimit(d, 0) }

func (f *ghFixture) CommitStatuses(r scm.RepoRef, sha string) []scm.CommitStatus {
	var out []scm.CommitStatus
	for _, s := range f.s.srv.Statuses(r.Owner, r.Name, sha) {
		out = append(out, scm.CommitStatus{State: scm.CommitState(s.State), Context: s.Context, Description: s.Description, TargetURL: s.TargetURL})
	}
	return out
}

func (f *ghFixture) CheckRuns(_ scm.RepoRef, sha string) []conformance.RecordedCheckRun {
	var out []conformance.RecordedCheckRun
	for _, c := range f.s.srv.CheckRuns(sha) {
		out = append(out, conformance.RecordedCheckRun{ID: c.ID, Name: c.Name, ExternalID: c.ExternalID,
			Status: scm.CheckStatus(c.Status), Conclusion: scm.CheckConclusion(c.Conclusion)})
	}
	return out
}

func conformanceData(s *seed, visiblePrivate bool) conformance.Data {
	repos := append([]string{}, s.svcNames...)
	if visiblePrivate {
		repos = append(repos, "platform")
	}
	return conformance.Data{
		Organization:         "acme",
		Repositories:         repos,
		ArchivedRepositories: []string{"legacy"},
		EmptyRepository:      "empty",
		Repo:                 s.platformRepo,
		DefaultBranch:        "main",
		Branches:             s.branches,
		ExistingPath:         "apps/a",
		MissingPath:          "apps/zzz",
		PullRequests:         s.openPRs,
		PRLabel:              "preview",
		MissingRepo:          scm.RepoRef{Owner: "nobody", Name: "nothing"},
		StatusSHA:            sha(1),
		InstallationOwner:    "ACME",
	}
}

func TestConformance(t *testing.T) {
	t.Parallel()
	variants := []struct {
		name   string
		prefix string
		auth   authKind
		skip   []string
	}{
		{"AppDiscovery", "", appDiscover, nil},
		{"AppExplicitGHES", "/api/v3", appExplicit, nil},
		{"PersonalAccessToken", "", pat, nil},
	}
	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			t.Parallel()
			conformance.Run(t, func(t *testing.T) (scm.Provider, conformance.Fixture) {
				s := newSeed(t, v.prefix)
				p := newProvider(t, s.config(t, v.auth))
				return p, &ghFixture{s: s, data: conformanceData(s, true)}
			}, conformance.Options{Skip: v.skip})
		})
	}
}

// TestConformanceAnonymous runs the suite without credentials against a public
// repository: private repositories are invisible and write capabilities undeclared.
func TestConformanceAnonymous(t *testing.T) {
	t.Parallel()
	conformance.Run(t, func(t *testing.T) (scm.Provider, conformance.Fixture) {
		s := newSeed(t, "")
		// Make the fixture repository public for anonymous access.
		s.srv.AddRepo(publicPlatform(s))
		p := newProvider(t, s.config(t, anonymous))
		return p, &ghFixture{s: s, data: conformanceData(s, true)}
	}, conformance.Options{})
}
