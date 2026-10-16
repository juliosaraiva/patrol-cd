package bitbucketcloud_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/bitbucketcloud"
)

const (
	testUser        = "patrol-bot"
	testAppPassword = "ATBB-test-app-password"
	hookUUID        = "{4f1c2a7e-9b3d-4c55-8e21-6a7b8c9d0e1f}"
)

func sha(n int) string { return fmt.Sprintf("%040x", n) }

type seed struct {
	f        *fakeBitbucket
	svcNames []string
	branches map[string]string
	openPRs  []scm.PullRequest
}

func basic(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

func newSeed(t *testing.T) *seed {
	t.Helper()
	f := newFakeBitbucket(t)
	f.auth = basic(testUser, testAppPassword)
	s := &seed{f: f}
	for i := range 7 {
		name := fmt.Sprintf("svc-%02d", i)
		s.svcNames = append(s.svcNames, name)
		f.add(&fcRepo{Workspace: "acme", Slug: name, UUID: fmt.Sprintf("{00000000-0000-0000-0000-%012d}", 100+i), MainBranch: "main",
			Branches: []fcBranch{{Name: "main", SHA: sha(100 + i)}}})
	}
	s.branches = map[string]string{"main": sha(1), "feature/a": sha(2), "feature-b": sha(3), "release-1": sha(4), "hotfix": sha(5)}
	pulls := []fcPull{
		{ID: 1, Title: "Add a", From: "feature/a", To: "main", SHA: sha(2), Author: "alice", State: "OPEN"},
		{ID: 2, Title: "Add b", From: "feature-b", To: "main", SHA: sha(3), Author: "bob", State: "OPEN"},
		{ID: 3, Title: "Hotfix", From: "hotfix", To: "release-1", SHA: sha(5), Author: "carol", State: "OPEN"},
		{ID: 4, Title: "Docs", From: "docs", To: "main", SHA: sha(6), Author: "dave", State: "OPEN"},
		{ID: 5, Title: "Old", From: "old", To: "main", SHA: sha(7), Author: "erin", State: "MERGED"},
		{ID: 6, Title: "Nope", From: "nope", To: "main", SHA: sha(8), Author: "frank", State: "DECLINED"},
	}
	for _, p := range pulls[:4] {
		s.openPRs = append(s.openPRs, scm.PullRequest{Number: p.ID, Title: p.Title, Branch: p.From, TargetBranch: p.To, HeadSHA: p.SHA[:12], Author: p.Author})
	}
	f.add(&fcRepo{Workspace: "acme", Slug: "platform", UUID: "{11111111-2222-3333-4444-555555555555}", MainBranch: "main", Private: true,
		Branches: []fcBranch{{Name: "main", SHA: sha(1)}, {Name: "feature/a", SHA: sha(2)}, {Name: "feature-b", SHA: sha(3)}, {Name: "release-1", SHA: sha(4)}, {Name: "hotfix", SHA: sha(5)}},
		Tags:     map[string]string{"v1.2.0": sha(4)},
		Files:    map[string][]string{sha(1): {"apps/a/values.yaml", "apps/a/configmap.yaml", "README.md", "dir with space/f#1.yaml"}, sha(2): {"apps/b/values.yaml"}},
		Pulls:    pulls})
	f.add(&fcRepo{Workspace: "acme", Slug: "empty", UUID: "{00000000-0000-0000-0000-000000000008}"})
	return s
}

var testRetry = scm.RetryPolicy{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond, MaxWait: 500 * time.Millisecond}

func (s *seed) config(token string) scm.Config {
	return scm.Config{
		Type:      bitbucketcloud.Type,
		Name:      "test",
		APIURL:    s.f.apiURL(),
		Token:     func(context.Context) (string, error) { return token, nil },
		RateLimit: scm.RateLimit{RPS: -1},
		Retry:     testRetry,
		Timeout:   10 * time.Second,
	}
}

func newProvider(t testing.TB, cfg scm.Config, opts bitbucketcloud.Options) *bitbucketcloud.Provider {
	t.Helper()
	p, err := bitbucketcloud.NewProvider(context.Background(), cfg, opts)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	return p
}

func basicProvider(t testing.TB, s *seed) *bitbucketcloud.Provider {
	return newProvider(t, s.config(testAppPassword), bitbucketcloud.Options{Username: testUser})
}

func fixture(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// delivery returns Bitbucket Cloud headers. The X-Hook-UUID header is set to uuid when
// non-empty (Bitbucket always sends it; tests omit it to model unsigned deliveries).
func delivery(key, id string, uuid []byte) http.Header {
	h := http.Header{}
	h.Set("X-Event-Key", key)
	h.Set("X-Request-UUID", id)
	h.Set("X-Attempt-Number", "1")
	h.Set("User-Agent", "Bitbucket-Webhooks/2.0")
	h.Set("Content-Type", "application/json")
	if len(uuid) > 0 {
		h.Set("X-Hook-UUID", string(uuid))
	}
	return h
}
