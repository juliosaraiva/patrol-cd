package bitbucketserver_test

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
	"github.com/juliosaraiva/patrol-cd/pkg/scm/bitbucketserver"
)

const (
	testUser     = "svc-patrol"
	testPassword = "bbs-test-password"
	testBearer   = "BBDC-test-token"
)

func sha(n int) string { return fmt.Sprintf("%040x", n) }

type seed struct {
	f        *fakeBBS
	svcNames []string
	branches map[string]string
	openPRs  []scm.PullRequest
}

func basic(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

func newSeed(t *testing.T) *seed {
	t.Helper()
	f := newFakeBBS(t)
	f.auth = basic(testUser, testPassword)
	s := &seed{f: f}
	for i := range 7 {
		name := fmt.Sprintf("svc-%02d", i)
		s.svcNames = append(s.svcNames, name)
		f.add(&fbRepo{ID: int64(100 + i), Project: "PROJ", Slug: name, DefaultBranch: "main", Branches: []fbBranch{{Name: "main", SHA: sha(100 + i)}}})
	}
	s.branches = map[string]string{"main": sha(1), "feature/a": sha(2), "feature-b": sha(3), "release-1": sha(4), "hotfix": sha(5)}
	pulls := []fbPull{
		{ID: 1, Title: "Add a", From: "feature/a", To: "main", SHA: sha(2), Author: "alice", State: "OPEN"},
		{ID: 2, Title: "Add b", From: "feature-b", To: "main", SHA: sha(3), Author: "bob", State: "OPEN"},
		{ID: 3, Title: "Hotfix", From: "hotfix", To: "release-1", SHA: sha(5), Author: "carol", State: "OPEN"},
		{ID: 4, Title: "Docs", From: "docs", To: "main", SHA: sha(6), Author: "dave", State: "OPEN"},
		{ID: 5, Title: "Old", From: "old", To: "main", SHA: sha(7), Author: "erin", State: "MERGED"},
		{ID: 6, Title: "Nope", From: "nope", To: "main", SHA: sha(8), Author: "frank", State: "DECLINED"},
	}
	for _, p := range pulls[:4] {
		s.openPRs = append(s.openPRs, scm.PullRequest{Number: p.ID, Title: p.Title, Branch: p.From, TargetBranch: p.To, HeadSHA: p.SHA, Author: p.Author})
	}
	f.add(&fbRepo{ID: 42, Project: "PROJ", Slug: "platform", Name: "Platform", DefaultBranch: "main",
		Branches: []fbBranch{{Name: "main", SHA: sha(1)}, {Name: "feature/a", SHA: sha(2)}, {Name: "feature-b", SHA: sha(3)}, {Name: "release-1", SHA: sha(4)}, {Name: "hotfix", SHA: sha(5)}},
		Tags:     map[string]string{"v1.2.0": sha(4)},
		Files:    map[string][]string{sha(1): {"apps/a/values.yaml", "apps/a/configmap.yaml", "README.md", "dir with space/f#1.yaml"}, sha(2): {"apps/b/values.yaml"}},
		Pulls:    pulls})
	f.add(&fbRepo{ID: 7, Project: "PROJ", Slug: "legacy", DefaultBranch: "main", Archived: true, Branches: []fbBranch{{Name: "main", SHA: sha(9)}}})
	f.add(&fbRepo{ID: 8, Project: "PROJ", Slug: "empty"})
	f.add(&fbRepo{ID: 9, Project: "PROJ", Slug: "nodefault", DefaultBranch: "gone", Branches: []fbBranch{{Name: "other", SHA: sha(10)}}})
	return s
}

var testRetry = scm.RetryPolicy{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond, MaxWait: 500 * time.Millisecond}

func (s *seed) config(token string) scm.Config {
	return scm.Config{
		Type:      bitbucketserver.Type,
		Name:      "test",
		APIURL:    s.f.srv.URL,
		Token:     func(context.Context) (string, error) { return token, nil },
		RateLimit: scm.RateLimit{RPS: -1},
		Retry:     testRetry,
		Timeout:   10 * time.Second,
	}
}

func newProvider(t testing.TB, cfg scm.Config, opts bitbucketserver.Options) *bitbucketserver.Provider {
	t.Helper()
	p, err := bitbucketserver.NewProvider(context.Background(), cfg, opts)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	return p
}

func basicProvider(t testing.TB, s *seed) *bitbucketserver.Provider {
	return newProvider(t, s.config(testPassword), bitbucketserver.Options{Username: testUser})
}

func fixture(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func delivery(key, id string, body, secret []byte) http.Header {
	h := http.Header{}
	h.Set("X-Event-Key", key)
	h.Set("X-Request-Id", id)
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("User-Agent", "Atlassian HttpClient 4.2.0 / Bitbucket-9.4.0 (9004000) / Default")
	if len(secret) > 0 {
		h.Set("X-Hub-Signature", "sha256="+scm.SignHMACSHA256(secret, body))
	}
	return h
}
