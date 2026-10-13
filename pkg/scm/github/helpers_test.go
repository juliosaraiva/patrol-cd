package github_test

import (
	"context"
	"crypto/rsa"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/github"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/github/internal/fakegithub"
)

const (
	appID          = int64(1234)
	installationID = int64(77)
	testPAT        = "ghp_testtoken0123456789"
)

var (
	keyOnce sync.Once
	rsaKey  *rsa.PrivateKey
	keyPEM  []byte
	keyErr  error
)

func appKey(t testing.TB) (*rsa.PrivateKey, []byte) {
	t.Helper()
	keyOnce.Do(func() { rsaKey, keyPEM, keyErr = fakegithub.GenerateKey(2048) })
	if keyErr != nil {
		t.Fatalf("generate key: %v", keyErr)
	}
	return rsaKey, keyPEM
}

func sha(n int) string { return fmt.Sprintf("%040x", n) }

// seed describes the standard fixture shared by provider and conformance tests.
type seed struct {
	srv          *fakegithub.Server
	svcNames     []string
	branches     map[string]string
	pulls        []fakegithub.Pull
	openPRs      []scm.PullRequest
	platformRepo scm.RepoRef
	platform     fakegithub.Repo
}

func newSeed(t *testing.T, prefix string) *seed {
	t.Helper()
	k, _ := appKey(t)
	srv := fakegithub.New(prefix)
	t.Cleanup(srv.Close)
	srv.SetMaxPerPage(3)
	srv.AddOwner("acme", "Organization")
	srv.AddOwner("octo", "User")
	srv.AddPAT(testPAT)
	srv.SetApp(appID, "patrol-cd", &k.PublicKey)
	srv.AddInstallation(fakegithub.Installation{ID: installationID, Account: "acme", AccountType: "Organization"})
	srv.AddInstallation(fakegithub.Installation{ID: 88, Account: "octo", AccountType: "User"})

	s := &seed{srv: srv, platformRepo: scm.RepoRef{Owner: "acme", Name: "platform"}}
	for i := range 7 {
		name := fmt.Sprintf("svc-%02d", i)
		s.svcNames = append(s.svcNames, name)
		srv.AddRepo(fakegithub.Repo{Owner: "acme", Name: name, ID: int64(1000 + i), DefaultBranch: "main",
			Topics: []string{"team-a"}, Branches: []fakegithub.Branch{{Name: "main", SHA: sha(100 + i)}}})
	}
	s.branches = map[string]string{"main": sha(1), "feature-a": sha(2), "feature-b": sha(3), "release-1": sha(4), "hotfix": sha(5)}
	s.pulls = []fakegithub.Pull{
		{Number: 1, Title: "Add a", Head: "feature-a", HeadSHA: sha(2), Base: "main", User: "alice", Labels: []string{"preview"}},
		{Number: 2, Title: "Add b", Head: "feature-b", HeadSHA: sha(3), Base: "main", User: "bob", Labels: []string{"preview", "team-a"}, Draft: true},
		{Number: 3, Title: "Hotfix", Head: "hotfix", HeadSHA: sha(5), Base: "release-1", User: "carol"},
		{Number: 4, Title: "Docs", Head: "docs", HeadSHA: sha(6), Base: "main", User: "dave", Labels: []string{"docs"}},
		{Number: 5, Title: "Old", Head: "old", HeadSHA: sha(7), Base: "main", User: "erin", State: "closed", MergedAt: "2024-01-01T00:00:00Z"},
	}
	for _, p := range s.pulls[:4] {
		s.openPRs = append(s.openPRs, scm.PullRequest{Number: int64(p.Number), Title: p.Title, Branch: p.Head, TargetBranch: p.Base, HeadSHA: p.HeadSHA, Author: p.User, Labels: p.Labels})
	}
	s.platform = fakegithub.Repo{Owner: "acme", Name: "platform", ID: 42, DefaultBranch: "main", Private: true, Topics: []string{"platform"},
		Branches: []fakegithub.Branch{
			{Name: "main", SHA: sha(1), Protected: true}, {Name: "feature-a", SHA: sha(2)}, {Name: "feature-b", SHA: sha(3)},
			{Name: "release-1", SHA: sha(4)}, {Name: "hotfix", SHA: sha(5)},
		},
		Tags: map[string]string{"v1.2.0": sha(4)},
		Files: map[string][]string{
			sha(1): {"apps/a/values.yaml", "apps/a/configmap.yaml", "README.md", "dir with space/f#1.yaml"},
			sha(2): {"apps/b/values.yaml"},
		},
		Pulls: s.pulls}
	srv.AddRepo(s.platform)
	srv.AddRepo(fakegithub.Repo{Owner: "acme", Name: "legacy", ID: 7, DefaultBranch: "main", Archived: true, Branches: []fakegithub.Branch{{Name: "main", SHA: sha(9)}}})
	srv.AddRepo(fakegithub.Repo{Owner: "acme", Name: "empty", ID: 8, DefaultBranch: "main"})
	srv.AddRepo(fakegithub.Repo{Owner: "octo", Name: "dotfiles", ID: 500, DefaultBranch: "trunk", Branches: []fakegithub.Branch{{Name: "trunk", SHA: sha(500)}}})
	return s
}

type authKind int

const (
	anonymous authKind = iota
	pat
	appExplicit
	appDiscover
)

// testRetry keeps retries fast and below the injected rate-limit horizons.
var testRetry = scm.RetryPolicy{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond, MaxWait: 500 * time.Millisecond}

func (s *seed) config(t testing.TB, a authKind) scm.Config {
	_, pemKey := appKey(t)
	cfg := scm.Config{
		Type:      github.Type,
		Name:      "test",
		APIURL:    s.apiURL(),
		RateLimit: scm.RateLimit{RPS: -1},
		Retry:     testRetry,
		Timeout:   10 * time.Second,
	}
	if s.srv.Prefix == "" {
		cfg.HTTPClient = s.dialingClient()
	}
	switch a {
	case pat:
		cfg.Token = func(context.Context) (string, error) { return testPAT, nil }
	case appExplicit:
		cfg.GitHubApp = &scm.GitHubAppConfig{AppID: appID, InstallationID: installationID, PrivateKey: pemKey}
	case appDiscover:
		cfg.GitHubApp = &scm.GitHubAppConfig{AppID: appID, PrivateKey: pemKey, Owner: "ACME"}
	}
	return cfg
}

// apiURL returns the URL providers are configured with. Without an enterprise prefix the
// fake server is addressed as "http://api.p<port>.github.test" (github.com-style root
// paths, unique per server so process-wide token caches never mix servers) via a dialer
// that connects to the local listener; with a prefix the GHES form is used.
func (s *seed) apiURL() string {
	if s.srv.Prefix == "" {
		_, port, _ := net.SplitHostPort(s.srv.Listener.Addr().String())
		return "http://api.p" + port + ".github.test"
	}
	return s.srv.APIURL()
}

func (s *seed) dialingClient() *http.Client {
	addr := s.srv.Listener.Addr().String()
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}}
}

func newProvider(t testing.TB, cfg scm.Config) *github.Provider {
	t.Helper()
	p, err := github.NewProvider(context.Background(), cfg)
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

// delivery returns GitHub-style headers for a fixture body signed with secret.
func delivery(event, id string, body, secret []byte) http.Header {
	h := http.Header{}
	h.Set("X-GitHub-Event", event)
	h.Set("X-GitHub-Delivery", id)
	h.Set("X-GitHub-Hook-ID", "123456")
	h.Set("User-Agent", "GitHub-Hookshot/abc123")
	h.Set("Content-Type", "application/json")
	if len(secret) > 0 {
		h.Set("X-Hub-Signature-256", "sha256="+scm.SignHMACSHA256(secret, body))
	}
	return h
}

// clock is a controllable time source.
type clock struct {
	mu  sync.Mutex
	off time.Duration
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return time.Now().Add(c.off) }

func (c *clock) Advance(d time.Duration) { c.mu.Lock(); c.off += d; c.mu.Unlock() }

func publicPlatform(s *seed) fakegithub.Repo {
	r := s.platform
	r.Private = false
	return r
}
