package gitlab_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/gitlab"
)

const testToken = "glpat-test-0123456789"

func sha(n int) string { return fmt.Sprintf("%040x", n) }

// seed is the standard fixture shared by provider and conformance tests.
type seed struct {
	f        *fakeGitLab
	svcNames []string
	branches map[string]string
	openMRs  []scm.PullRequest
	platform *fgProject
}

func newSeed(t *testing.T) *seed {
	t.Helper()
	f := newFakeGitLab(t)
	f.token = testToken
	f.addGroup("acme", 10)
	f.addGroup("acme/infra", 11)
	f.addGroup("other", 12)
	s := &seed{f: f}
	for i := range 7 {
		name := fmt.Sprintf("svc-%02d", i)
		s.svcNames = append(s.svcNames, name)
		f.addProject(&fgProject{ID: int64(100 + i), Namespace: "acme", Path: name, DefaultBranch: "main", Topics: []string{"team-a"},
			Branches: []fgBranch{{Name: "main", SHA: sha(100 + i)}}})
	}
	s.branches = map[string]string{"main": sha(1), "feature/a": sha(2), "feature-b": sha(3), "release-1": sha(4), "hotfix": sha(5)}
	mrs := []fgMR{
		{IID: 1, Title: "Add a", Source: "feature/a", Target: "main", SHA: sha(2), Author: "alice", Labels: []string{"preview"}, State: "opened"},
		{IID: 2, Title: "Add b", Source: "feature-b", Target: "main", SHA: sha(3), Author: "bob", Labels: []string{"preview", "team-a"}, State: "opened", Draft: true},
		{IID: 3, Title: "Hotfix", Source: "hotfix", Target: "release-1", SHA: sha(5), Author: "carol", State: "opened"},
		{IID: 4, Title: "Docs", Source: "docs", Target: "main", SHA: sha(6), Author: "dave", Labels: []string{"docs"}, State: "opened"},
		{IID: 5, Title: "Old", Source: "old", Target: "main", SHA: sha(7), Author: "erin", State: "merged"},
		{IID: 6, Title: "Abandoned", Source: "gone", Target: "main", SHA: sha(8), Author: "frank", State: "closed"},
	}
	for _, mr := range mrs[:4] {
		s.openMRs = append(s.openMRs, scm.PullRequest{Number: mr.IID, Title: mr.Title, Branch: mr.Source, TargetBranch: mr.Target, HeadSHA: mr.SHA, Author: mr.Author, Labels: mr.Labels})
	}
	s.platform = &fgProject{ID: 42, Namespace: "acme", Path: "platform", DefaultBranch: "main", Topics: []string{"platform"},
		Branches: []fgBranch{
			{Name: "main", SHA: sha(1), Protected: true}, {Name: "feature/a", SHA: sha(2)}, {Name: "feature-b", SHA: sha(3)},
			{Name: "release-1", SHA: sha(4)}, {Name: "hotfix", SHA: sha(5)},
		},
		Tags:  map[string]string{"v1.2.0": sha(4)},
		Files: map[string][]string{sha(1): {"apps/a/values.yaml", "apps/a/configmap.yaml", "README.md", "dir with space/f#1.yaml"}, sha(2): {"apps/b/values.yaml"}},
		MRs:   mrs}
	f.addProject(s.platform)
	f.addProject(&fgProject{ID: 7, Namespace: "acme", Path: "legacy", DefaultBranch: "main", Archived: true, Branches: []fgBranch{{Name: "main", SHA: sha(9)}}})
	f.addProject(&fgProject{ID: 8, Namespace: "acme", Path: "empty"})
	f.addProject(&fgProject{ID: 9, Namespace: "acme/infra", Path: "net", DefaultBranch: "main", TagList: []string{"legacy-tag"}, Branches: []fgBranch{{Name: "main", SHA: sha(90)}}})
	f.addProject(&fgProject{ID: 300, Namespace: "other", Path: "shared", DefaultBranch: "trunk", Visibility: "public", SharedWith: []string{"acme"}, Branches: []fgBranch{{Name: "trunk", SHA: sha(300)}}})
	return s
}

// testRetry keeps retries fast and below injected rate-limit horizons.
var testRetry = scm.RetryPolicy{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond, MaxWait: 500 * time.Millisecond}

func (s *seed) config() scm.Config {
	return scm.Config{
		Type:      gitlab.Type,
		Name:      "test",
		APIURL:    s.f.url(),
		Token:     func(context.Context) (string, error) { return testToken, nil },
		RateLimit: scm.RateLimit{RPS: -1},
		Retry:     testRetry,
		Timeout:   10 * time.Second,
	}
}

func newProvider(t testing.TB, cfg scm.Config, opts gitlab.Options) *gitlab.Provider {
	t.Helper()
	p, err := gitlab.NewProvider(context.Background(), cfg, opts)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	return p
}

func fixture(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// delivery returns GitLab-style headers for a webhook body; the token header is set
// only when secret is non-empty.
func delivery(event, id string, secret []byte) http.Header {
	h := http.Header{}
	h.Set("X-Gitlab-Event", event)
	h.Set("X-Gitlab-Event-UUID", id)
	h.Set("X-Gitlab-Webhook-UUID", "5d0e2c55-6c0b-4c8e-9a1f-1f2b3c4d5e6f")
	h.Set("X-Gitlab-Instance", "https://gitlab.example.com")
	h.Set("User-Agent", "GitLab/17.5.0")
	h.Set("Content-Type", "application/json")
	if len(secret) > 0 {
		h.Set("X-Gitlab-Token", string(secret))
	}
	return h
}
