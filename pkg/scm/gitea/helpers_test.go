package gitea_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/gitea"
)

const testToken = "gitea-test-token-0123456789"

func sha(n int) string { return fmt.Sprintf("%040x", n) }

type seed struct {
	f        *fakeGitea
	svcNames []string
	branches map[string]string
	openPRs  []scm.PullRequest
}

func newSeed(t *testing.T) *seed {
	t.Helper()
	f := newFakeGitea(t)
	f.token = testToken
	f.orgs["acme"] = true
	f.users["octo"] = true
	s := &seed{f: f}
	for i := range 7 {
		name := fmt.Sprintf("svc-%02d", i)
		s.svcNames = append(s.svcNames, name)
		f.add(&ftRepo{ID: int64(100 + i), Owner: "acme", Name: name, DefaultBranch: "main", Labels: []string{"team-a"},
			Branches: []ftBranch{{Name: "main", SHA: sha(100 + i)}}})
	}
	s.branches = map[string]string{"main": sha(1), "feature/a": sha(2), "feature-b": sha(3), "release-1": sha(4), "hotfix": sha(5)}
	pulls := []ftPull{
		{Number: 1, Title: "Add a", Head: "feature/a", Base: "main", SHA: sha(2), User: "alice", Labels: []string{"preview"}, State: "open"},
		{Number: 2, Title: "Add b", Head: "feature-b", Base: "main", SHA: sha(3), User: "bob", Labels: []string{"preview", "team-a"}, State: "open", Draft: true},
		{Number: 3, Title: "Hotfix", Head: "hotfix", Base: "release-1", SHA: sha(5), User: "carol", State: "open"},
		{Number: 4, Title: "Docs", Head: "docs", Base: "main", SHA: sha(6), User: "dave", Labels: []string{"docs"}, State: "open"},
		{Number: 5, Title: "Old", Head: "old", Base: "main", SHA: sha(7), User: "erin", State: "closed", Merged: true},
		{Number: 6, Title: "Abandoned", Head: "gone", Base: "main", SHA: sha(8), User: "frank", State: "closed"},
	}
	for _, p := range pulls[:4] {
		s.openPRs = append(s.openPRs, scm.PullRequest{Number: p.Number, Title: p.Title, Branch: p.Head, TargetBranch: p.Base, HeadSHA: p.SHA, Author: p.User, Labels: p.Labels})
	}
	f.add(&ftRepo{ID: 42, Owner: "acme", Name: "platform", DefaultBranch: "main", Private: true, Labels: []string{"platform", "bug"}, Topics: []string{"k8s"},
		Branches: []ftBranch{{Name: "main", SHA: sha(1)}, {Name: "feature/a", SHA: sha(2)}, {Name: "feature-b", SHA: sha(3)}, {Name: "release-1", SHA: sha(4)}, {Name: "hotfix", SHA: sha(5)}},
		Tags:     map[string]string{"v1.2.0": sha(4)},
		Files:    map[string][]string{sha(1): {"apps/a/values.yaml", "apps/a/configmap.yaml", "README.md", "dir with space/f#1.yaml"}, sha(2): {"apps/b/values.yaml"}},
		Pulls:    pulls})
	f.add(&ftRepo{ID: 7, Owner: "acme", Name: "legacy", DefaultBranch: "main", Archived: true, Branches: []ftBranch{{Name: "main", SHA: sha(9)}}})
	f.add(&ftRepo{ID: 8, Owner: "acme", Name: "empty", DefaultBranch: "main", Empty: true})
	f.add(&ftRepo{ID: 500, Owner: "octo", Name: "dotfiles", DefaultBranch: "trunk", Branches: []ftBranch{{Name: "trunk", SHA: sha(500)}}})
	return s
}

var testRetry = scm.RetryPolicy{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond, MaxWait: 500 * time.Millisecond}

func (s *seed) config() scm.Config {
	return scm.Config{
		Type:      gitea.Type,
		Name:      "test",
		APIURL:    s.f.srv.URL,
		Token:     func(context.Context) (string, error) { return testToken, nil },
		RateLimit: scm.RateLimit{RPS: -1},
		Retry:     testRetry,
		Timeout:   10 * time.Second,
	}
}

func newProvider(t testing.TB, cfg scm.Config) *gitea.Provider {
	t.Helper()
	p, err := gitea.NewProvider(context.Background(), cfg)
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

// delivery returns the headers Gitea sends (including its Gogs and GitHub compatibility
// headers), signed with secret when non-empty.
func delivery(event, id string, body, secret []byte) http.Header {
	h := http.Header{}
	h.Set("X-Gitea-Event", event)
	h.Set("X-Gitea-Event-Type", event)
	h.Set("X-Gitea-Delivery", id)
	h.Set("X-Gogs-Event", event)
	h.Set("X-Gogs-Delivery", id)
	h.Set("X-GitHub-Event", event)
	h.Set("X-GitHub-Delivery", id)
	h.Set("User-Agent", "Go-http-client/1.1")
	h.Set("Content-Type", "application/json")
	if len(secret) > 0 {
		sig := scm.SignHMACSHA256(secret, body)
		h.Set("X-Gitea-Signature", sig)
		h.Set("X-Gogs-Signature", sig)
		h.Set("X-Hub-Signature-256", "sha256="+sig)
	}
	return h
}
