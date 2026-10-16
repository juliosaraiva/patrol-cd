package azuredevops_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/azuredevops"
)

const (
	testPAT = "azdo-test-pat-0123456789"
	project = "Platform Team"
)

func sha(n int) string { return fmt.Sprintf("%040x", n) }

type seed struct {
	f        *fakeAzure
	svcNames []string
	branches map[string]string
	openPRs  []scm.PullRequest
}

func newSeed(t *testing.T) *seed {
	t.Helper()
	f := newFakeAzure(t, "acme")
	f.pat = testPAT
	s := &seed{f: f}
	var repos []*faRepo
	for i := range 7 {
		name := fmt.Sprintf("svc-%02d", i)
		s.svcNames = append(s.svcNames, name)
		repos = append(repos, &faRepo{ID: fmt.Sprintf("00000000-0000-0000-0000-%012d", 100+i), Name: name, DefaultBranch: "main", Branches: []faBranch{{Name: "main", SHA: sha(100 + i)}}})
	}
	s.branches = map[string]string{"main": sha(1), "feature/a": sha(2), "feature-b": sha(3), "release-1": sha(4), "hotfix": sha(5)}
	repos = append(repos,
		&faRepo{ID: "11111111-2222-3333-4444-555555555555", Name: "platform", DefaultBranch: "main",
			Branches: []faBranch{{Name: "main", SHA: sha(1)}, {Name: "feature/a", SHA: sha(2)}, {Name: "feature-b", SHA: sha(3)}, {Name: "release-1", SHA: sha(4)}, {Name: "hotfix", SHA: sha(5)}},
			Tags:     map[string]string{"v1.2.0": sha(4), "v1.2.0-rc": sha(3)},
			Files:    map[string][]string{sha(1): {"apps/a/values.yaml", "apps/a/configmap.yaml", "README.md", "dir with space/f#1.yaml"}, sha(2): {"apps/b/values.yaml"}}},
		&faRepo{ID: "dddddddd-0000-0000-0000-000000000001", Name: "disabled", DefaultBranch: "main", Disabled: true, Branches: []faBranch{{Name: "main", SHA: sha(9)}}},
		&faRepo{ID: "eeeeeeee-0000-0000-0000-000000000001", Name: "empty"},
	)
	f.projects[project] = repos
	pulls := []faPull{
		{ID: 1, Repo: "platform", Title: "Add a", From: "feature/a", To: "main", SHA: sha(2), Author: "alice", Status: "active", Labels: []string{"preview"}},
		{ID: 2, Repo: "platform", Title: "Add b", From: "feature-b", To: "main", SHA: sha(3), Author: "bob", Status: "active", Labels: []string{"preview", "team-a"}},
		{ID: 3, Repo: "platform", Title: "Hotfix", From: "hotfix", To: "release-1", SHA: sha(5), Author: "carol", Status: "active"},
		{ID: 4, Repo: "platform", Title: "Docs", From: "docs", To: "main", SHA: sha(6), Author: "dave", Status: "active", Labels: []string{"docs"}},
		{ID: 5, Repo: "platform", Title: "Old", From: "old", To: "main", SHA: sha(7), Author: "erin", Status: "completed"},
		{ID: 6, Repo: "platform", Title: "Nope", From: "nope", To: "main", SHA: sha(8), Author: "frank", Status: "abandoned"},
	}
	for _, p := range pulls[:4] {
		s.openPRs = append(s.openPRs, scm.PullRequest{Number: p.ID, Title: p.Title, Branch: p.From, TargetBranch: p.To, HeadSHA: p.SHA, Author: p.Author, Labels: p.Labels})
	}
	// Pull requests of another repository in the same project push the listing past one
	// $top page; they must be filtered out by repository name.
	for i := range 120 {
		pulls = append(pulls, faPull{ID: int64(1000 + i), Repo: "svc-00", Title: "noise", From: fmt.Sprintf("n-%d", i), To: "main", SHA: sha(2000 + i), Author: "bot", Status: "active"})
	}
	f.pulls[project] = pulls
	return s
}

var testRetry = scm.RetryPolicy{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond, MaxWait: 500 * time.Millisecond}

func (s *seed) config() scm.Config {
	return scm.Config{
		Type:      azuredevops.Type,
		Name:      "test",
		APIURL:    s.f.srv.URL,
		Token:     func(context.Context) (string, error) { return testPAT, nil },
		RateLimit: scm.RateLimit{RPS: -1},
		Retry:     testRetry,
		Timeout:   10 * time.Second,
	}
}

func newProvider(t testing.TB, cfg scm.Config, opts azuredevops.Options) *azuredevops.Provider {
	t.Helper()
	p, err := azuredevops.NewProvider(context.Background(), cfg, opts)
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

// delivery returns Azure DevOps service hook headers; basic auth is set from the
// "user:password" secret when non-empty.
func delivery(secret []byte) http.Header {
	h := http.Header{}
	h.Set("X-Vss-Activityid", "8ed2f0f2-4f15-4e4b-9b7e-2c1c3a5d7e9f")
	h.Set("X-Vss-Subscriptionid", "7e2a6d26-0000-0000-0000-000000000000")
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("User-Agent", "VSServices/19.255.0")
	if len(secret) > 0 {
		user, pass := azuredevops.SplitSecret(secret)
		h.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(string(user)+":"+string(pass))))
	}
	return h
}

// withID replaces the event id of a fixture (the delivery ID).
func withID(t testing.TB, name, id string) []byte {
	t.Helper()
	b := fixture(t, name)
	return []byte(strings.Replace(string(b), `"id": "EVENT-ID"`, `"id": "`+id+`"`, 1))
}
