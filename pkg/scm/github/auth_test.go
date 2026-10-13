package github_test

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/github"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/github/internal/fakegithub"
)

func countTokenMints(s *seed) int {
	n := 0
	for _, r := range s.srv.Requests() {
		if r.Method == http.MethodPost && strings.HasSuffix(r.Path, "/access_tokens") {
			if r.Auth != "jwt" {
				panic("access token minted without App JWT: " + r.Auth)
			}
			n++
		}
	}
	return n
}

func TestAppExplicitInstallation(t *testing.T) {
	t.Parallel()
	s := newSeed(t, "/api/v3")
	p := newProvider(t, s.config(t, appExplicit))
	ctx := t.Context()
	repos, err := p.ListRepositories(ctx, scm.RepoQuery{Organization: "acme", Match: func(r scm.Repository) bool { return r.Name == "platform" }})
	if err != nil || len(repos) != 1 {
		t.Fatalf("private repository via App: %v %v", repos, err)
	}
	if _, err := p.ListPullRequests(ctx, s.platformRepo, scm.PRQuery{}); err != nil {
		t.Fatal(err)
	}
	if n := countTokenMints(s); n != 1 {
		t.Fatalf("token minted %d times, want 1 (cached)", n)
	}
	if n := s.srv.CountRequests(http.MethodPost, "/app/installations/77/access_tokens"); n != 1 {
		t.Fatalf("token request did not reach <base>/app/installations/77/access_tokens: %d", n)
	}
	for _, r := range s.srv.Requests() {
		if !strings.HasPrefix(r.Path, "/app/") && r.Auth != "installation" {
			t.Fatalf("API request not authenticated with the installation token: %+v", r)
		}
	}
	if s.srv.CountRequests(http.MethodGet, "/orgs/acme/installation") != 0 {
		t.Fatal("explicit installation ID must not trigger discovery")
	}
}

func TestAppInstallationDiscovery(t *testing.T) {
	t.Parallel()
	s := newSeed(t, "")
	cfg := s.config(t, appDiscover) // Owner "ACME": discovery is case-insensitive
	p := newProvider(t, cfg)
	tok, err := p.Token(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if tok.Username != github.GitUsername || tok.Value == "" || tok.ExpiresAt.IsZero() {
		t.Fatalf("token = %v", tok)
	}
	if n := s.srv.CountRequests(http.MethodGet, "/orgs/acme/installation"); n != 1 {
		t.Fatalf("discovery requests = %d", n)
	}
	// A second provider for the same App and server reuses the cached discovery and token.
	p2 := newProvider(t, cfg)
	tok2, err := p2.Token(t.Context())
	if err != nil || tok2.Value != tok.Value {
		t.Fatalf("second provider: %v (same token: %v)", err, tok2.Value == tok.Value)
	}
	if n := s.srv.CountRequests(http.MethodGet, "/orgs/acme/installation"); n != 1 {
		t.Fatalf("discovery not cached: %d requests", n)
	}
	if n := countTokenMints(s); n != 1 {
		t.Fatalf("token not shared: %d mints", n)
	}

	// User accounts: the organization lookup 404s, the user lookup succeeds.
	userCfg := s.config(t, appDiscover)
	userCfg.GitHubApp.Owner = "octo"
	inst, err := newProvider(t, userCfg).FindInstallation(t.Context(), "octo")
	if err != nil || inst.ID != 88 || inst.AccountType != "User" {
		t.Fatalf("user installation = %+v, %v", inst, err)
	}
	if s.srv.CountRequests(http.MethodGet, "/users/octo/installation") != 1 {
		t.Fatal("user installation endpoint not used")
	}

	// Missing installation: documented error, NotFound.
	missing := s.config(t, appDiscover)
	missing.GitHubApp.Owner = "nobody"
	_, err = newProvider(t, missing).Token(t.Context())
	const wantMsg = "failed to discover GitHub App installation ID for organization nobody: ensure the GitHub App (ID: 1234) is installed for this organization: installation not found for org: nobody"
	if !scm.IsNotFound(err) || !strings.Contains(err.Error(), wantMsg) {
		t.Fatalf("missing installation: %v", err)
	}
}

func TestAppDiscoveryRequiresOwner(t *testing.T) {
	t.Parallel()
	s := newSeed(t, "")
	cfg := s.config(t, appDiscover)
	cfg.GitHubApp.Owner = ""
	p := newProvider(t, cfg)
	_, err := p.Token(t.Context())
	if scm.KindOf(err) != scm.KindInvalid || !strings.Contains(err.Error(), "installation ID is required for github") {
		t.Fatalf("err = %v", err)
	}
	if _, err := p.ListBranches(t.Context(), s.platformRepo, true); scm.KindOf(err) != scm.KindInvalid {
		t.Fatalf("API call without installation: %v", err)
	}
}

func TestAppTokenRefreshBeforeExpiry(t *testing.T) {
	t.Parallel()
	s := newSeed(t, "")
	clk := &clock{}
	cfg := s.config(t, appExplicit)
	cfg.Clock = clk.Now
	p := newProvider(t, cfg)
	ctx := t.Context()
	first, err := p.Token(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Until(first.ExpiresAt); d < 55*time.Minute || d > 61*time.Minute {
		t.Fatalf("expiry from API not honored: %v", d)
	}
	clk.Advance(50 * time.Minute) // 10 minutes left: still valid
	again, err := p.Token(ctx)
	if err != nil || again.Value != first.Value {
		t.Fatalf("token refreshed too early: %v", err)
	}
	clk.Advance(6 * time.Minute) // 4 minutes left: inside the 5-minute refresh window
	refreshed, err := p.Token(ctx)
	if err != nil || refreshed.Value == first.Value {
		t.Fatalf("token not refreshed 5 minutes before expiry: %v", err)
	}
	if n := countTokenMints(s); n != 2 {
		t.Fatalf("mints = %d, want 2", n)
	}
	// API calls use the refreshed token.
	if _, err := p.ListBranches(ctx, s.platformRepo, true); err != nil {
		t.Fatal(err)
	}
}

func TestAppTokenRevokedIsReplaced(t *testing.T) {
	t.Parallel()
	for _, mode := range []authKind{appExplicit, appDiscover} {
		t.Run(map[authKind]string{appExplicit: "explicit", appDiscover: "discovered"}[mode], func(t *testing.T) {
			t.Parallel()
			s := newSeed(t, "")
			p := newProvider(t, s.config(t, mode))
			ctx := t.Context()
			if _, err := p.ListBranches(ctx, s.platformRepo, true); err != nil {
				t.Fatal(err)
			}
			s.srv.RevokeTokens()
			// The 401 invalidates the cached token and the request is replayed once with a
			// freshly minted token, so the caller does not see the revocation.
			if _, err := p.ListBranches(ctx, s.platformRepo, true); err != nil {
				t.Fatalf("revoked token was not replaced transparently: %v", err)
			}
			if n := countTokenMints(s); n != 2 {
				t.Fatalf("mints = %d, want 2", n)
			}
			if _, err := p.ListBranches(ctx, s.platformRepo, true); err != nil {
				t.Fatalf("after replacement: %v", err)
			}
			if n := countTokenMints(s); n != 2 {
				t.Fatalf("replacement token not cached: %d mints", n)
			}
		})
	}
}

func TestAppTokenRejectedTwiceIsUnauthorized(t *testing.T) {
	t.Parallel()
	s := newSeed(t, "")
	p := newProvider(t, s.config(t, appExplicit))
	s.srv.RejectInstallationTokens(true)
	_, err := p.ListBranches(t.Context(), s.platformRepo, true)
	if scm.KindOf(err) != scm.KindUnauthorized {
		t.Fatalf("rejected tokens: %v, want Unauthorized", err)
	}
	// One mint for the first attempt and exactly one replacement: no retry loop.
	if n := countTokenMints(s); n != 2 {
		t.Fatalf("mints = %d, want 2", n)
	}
	if n := s.srv.CountRequests(http.MethodGet, "/repos/acme/platform/branches"); n != 2 {
		t.Fatalf("branch requests = %d, want 2 (original + one replay)", n)
	}
	s.srv.RejectInstallationTokens(false)
	if _, err := p.ListBranches(t.Context(), s.platformRepo, true); err != nil {
		t.Fatalf("after recovery: %v", err)
	}
}

func TestPATUnauthorizedIsNotReplayed(t *testing.T) {
	t.Parallel()
	s := newSeed(t, "")
	cfg := s.config(t, pat)
	cfg.Token = func(context.Context) (string, error) { return "ghp_revoked", nil }
	_, err := newProvider(t, cfg).ListBranches(t.Context(), s.platformRepo, true)
	if scm.KindOf(err) != scm.KindUnauthorized {
		t.Fatalf("bad PAT: %v", err)
	}
	if n := s.srv.CountRequests(http.MethodGet, "/repos/acme/platform/branches"); n != 1 {
		t.Fatalf("PAT 401 replayed: %d requests", n)
	}
}

func TestAppConcurrentTokenRequestsShareOneMint(t *testing.T) {
	t.Parallel()
	s := newSeed(t, "")
	p := newProvider(t, s.config(t, appDiscover))
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for range 20 {
		wg.Go(func() {
			_, err := p.ListBranches(t.Context(), s.platformRepo, false)
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := countTokenMints(s); n != 1 {
		t.Fatalf("mints = %d, want 1", n)
	}
	if n := s.srv.CountRequests(http.MethodGet, "/orgs/acme/installation"); n != 1 {
		t.Fatalf("discoveries = %d, want 1", n)
	}
}

func TestAppPKCS8Key(t *testing.T) {
	t.Parallel()
	k, _ := appKey(t)
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	s := newSeed(t, "")
	cfg := s.config(t, appExplicit)
	cfg.GitHubApp.PrivateKey = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if _, err := newProvider(t, cfg).Token(t.Context()); err != nil {
		t.Fatalf("PKCS#8 key: %v", err)
	}
}

func TestAppWrongKeyRejected(t *testing.T) {
	t.Parallel()
	s := newSeed(t, "")
	_, otherPEM, err := fakegithub.GenerateKey(2048)
	if err != nil {
		t.Fatal(err)
	}
	cfg := s.config(t, appExplicit)
	cfg.GitHubApp.PrivateKey = otherPEM
	_, err = newProvider(t, cfg).Token(t.Context())
	if scm.KindOf(err) != scm.KindUnauthorized {
		t.Fatalf("JWT signed with the wrong key: %v", err)
	}
	if strings.Contains(err.Error(), string(otherPEM[40:80])) {
		t.Fatal("error leaks key material")
	}
}

func TestInstallations(t *testing.T) {
	t.Parallel()
	s := newSeed(t, "/api/v3")
	s.srv.SetMaxPerPage(1) // force Link pagination of /app/installations
	p := newProvider(t, s.config(t, appExplicit))
	all, err := p.ListInstallations(t.Context())
	if err != nil || len(all) != 2 || all[0].ID != installationID || all[0].Account != "acme" || all[0].AccountType != "Organization" || all[0].RepositorySelection != "all" {
		t.Fatalf("installations = %+v, %v", all, err)
	}
	if _, err := p.FindInstallation(t.Context(), "bad/owner"); scm.KindOf(err) != scm.KindInvalid {
		t.Fatalf("invalid owner: %v", err)
	}
}

func TestPATCredential(t *testing.T) {
	t.Parallel()
	s := newSeed(t, "")
	p := newProvider(t, s.config(t, pat))
	tok, err := p.Token(t.Context())
	if err != nil || tok.Value != testPAT || tok.Username != "x-access-token" || !tok.ExpiresAt.IsZero() {
		t.Fatalf("PAT token = %v, %v", tok, err)
	}
	if strings.Contains(tok.String(), testPAT) {
		t.Fatal("Token.String leaks the PAT")
	}
	cfg := s.config(t, pat)
	cfg.Token = func(context.Context) (string, error) { return "", errors.New("secret not found") }
	broken := newProvider(t, cfg)
	if _, err := broken.Token(t.Context()); scm.KindOf(err) != scm.KindUnauthorized {
		t.Fatalf("token source error: %v", err)
	}
	// API calls report the same kind and never reach the server.
	before := len(s.srv.Requests())
	_, err = broken.ListBranches(t.Context(), s.platformRepo, true)
	if scm.KindOf(err) != scm.KindUnauthorized || !strings.Contains(err.Error(), "secret not found") {
		t.Fatalf("API call with failing token source: %v", err)
	}
	if after := len(s.srv.Requests()); after != before {
		t.Fatalf("request sent without a token (%d)", after-before)
	}
	// Context errors from the token source stay context errors.
	cfg.Token = func(ctx context.Context) (string, error) { return "", context.Canceled }
	if _, err := newProvider(t, cfg).Token(t.Context()); !errors.Is(err, context.Canceled) || scm.KindOf(err) == scm.KindUnauthorized {
		t.Fatalf("context error: %v", err)
	}
}

func TestBotIdentity(t *testing.T) {
	t.Parallel()
	s := newSeed(t, "")
	name, email, err := newProvider(t, s.config(t, appExplicit)).BotIdentity(t.Context())
	if err != nil || name != "patrol-cd[bot]" || email != "4242+patrol-cd[bot]@users.noreply.github.com" {
		t.Fatalf("identity = %q %q %v", name, email, err)
	}
	if _, _, err := newProvider(t, s.config(t, pat)).BotIdentity(t.Context()); !errors.Is(err, scm.ErrUnsupported) {
		t.Fatalf("PAT BotIdentity: %v", err)
	}
}
