package repository

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"

	v1alpha1 "github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// fakeGitHub is a GitHub Enterprise API (under /api/v3, TLS) answering App installation
// lookups and installation token requests. Tokens expire clock+ttl. Fixtures using it need
// withTransport(trustTransport(gh.Server)).
type fakeGitHub struct {
	*httptest.Server
	clock *fakeClock
	ttl   time.Duration
	appID int64
	pub   *rsa.PublicKey

	mu         sync.Mutex
	orgs       map[string]int64
	mints      map[int64]int
	lookups    map[string]int
	requests   int
	seq        int
	failTokens bool
	// failBody, when set, is the body of every token refusal.
	failBody string
}

func newFakeGitHub(t *testing.T, clock *fakeClock, appID int64, orgs map[string]int64) *fakeGitHub {
	t.Helper()
	key, _ := rsaKey(t)
	g := &fakeGitHub{clock: clock, ttl: time.Hour, appID: appID, pub: &key.PublicKey, orgs: orgs, mints: map[int64]int{}, lookups: map[string]int{}}
	g.Server = httptest.NewTLSServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.Close)
	return g
}

// apiURL is the enterprise base URL (with a trailing slash, which must be tolerated).
func (g *fakeGitHub) apiURL() string { return g.URL + "/api/v3/" }

func (g *fakeGitHub) authorized(r *http.Request) bool {
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}
	tok, err := jwt.Parse(raw, func(*jwt.Token) (any, error) { return g.pub, nil }, jwt.WithValidMethods([]string{"RS256"}))
	if err != nil || !tok.Valid {
		return false
	}
	iss, err := tok.Claims.GetIssuer()
	return err == nil && iss == strconv.FormatInt(g.appID, 10)
}

func (g *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.requests++
	g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if !g.authorized(r) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v3")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && len(parts) == 3 && (parts[0] == "orgs" || parts[0] == "users") && parts[2] == "installation":
		g.lookups[parts[1]]++
		id, ok := g.orgs[parts[1]]
		if !ok || parts[0] == "users" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "app_id": g.appID, "account": map[string]any{"login": parts[1], "type": "Organization"}})
	case r.Method == http.MethodPost && len(parts) == 4 && parts[0] == "app" && parts[1] == "installations" && parts[3] == "access_tokens":
		id, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil || g.failTokens {
			w.WriteHeader(http.StatusForbidden)
			body := `{"message":"forbidden"}`
			if g.failBody != "" {
				body = g.failBody
			}
			_, _ = w.Write([]byte(body))
			return
		}
		g.mints[id]++
		g.seq++
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      fmt.Sprintf("ghs_installation_%d_token_%d", id, g.seq),
			"expires_at": g.clock.Now().Add(g.ttl).UTC().Format(time.RFC3339),
		})
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}
}

func (g *fakeGitHub) mintCount(id int64) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.mints[id]
}

func (g *fakeGitHub) requestCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.requests
}

func (g *fakeGitHub) lookupCount(owner string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lookups[owner]
}

// TestGitHubAppTokenCaching covers REPO-015, REPO-017 and REPO-055: an installation token is
// minted at <base>/app/installations/<id>/access_tokens (trailing slash tolerated), reused
// until five minutes before expiry, then refreshed; CredentialsExpireAt is set.
func TestGitHubAppTokenCaching(t *testing.T) {
	t.Parallel()
	_, keyPEM := rsaKey(t)
	clock := newFakeClock(time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC))
	gh := newFakeGitHub(t, clock, 42, nil)
	f := newFixture(t, []runtime.Object{
		secret("app", v1alpha1.LabelValueSecretTypeRepository, map[string]string{"githubAppPrivateKey": keyPEM}),
		repo("app", "https://github.com/org/repo", withCreds("app"), func(r *v1alpha1.Repository) {
			r.Spec.GitHubApp = githubApp(42, 7, gh.apiURL())
		}),
	}, withClock(clock), withTransport(trustTransport(gh.Server)))
	ctx := context.Background()

	res, err := f.resolver.Resolve(ctx, Query{URL: "https://github.com/org/repo"})
	require.NoError(t, err)
	assert.Equal(t, MethodGitHubApp, res.Method)
	first := res.Repository
	assert.Equal(t, "x-access-token", first.Username)
	assert.True(t, strings.HasPrefix(first.Password, "ghs_installation_7_"))
	require.NotNil(t, first.CredentialsExpireAt)
	assert.True(t, first.CredentialsExpireAt.Equal(clock.Now().Add(time.Hour)))
	assert.Equal(t, 1, gh.mintCount(7))

	clock.Advance(54 * time.Minute)
	again, err := f.resolver.ResolveForURL(ctx, "https://github.com/org/repo", "", read)
	require.NoError(t, err)
	assert.Equal(t, first.Password, again.Password, "reused before the refresh window")
	assert.Equal(t, 1, gh.mintCount(7))

	clock.Advance(2 * time.Minute)
	refreshed, err := f.resolver.ResolveForURL(ctx, "https://github.com/org/repo", "", read)
	require.NoError(t, err)
	assert.NotEqual(t, first.Password, refreshed.Password, "refreshed within five minutes of expiry")
	assert.Equal(t, 2, gh.mintCount(7))
	assert.True(t, refreshed.CredentialsExpireAt.After(*first.CredentialsExpireAt))

	assert.InDelta(t, 2, f.counterValue(t, "patrol_repository_token_requests_total", map[string]string{"kind": tokenKindGitHubApp, "result": "minted"}), 0)
	assert.InDelta(t, 1, f.counterValue(t, "patrol_repository_token_requests_total", map[string]string{"kind": tokenKindGitHubApp, "result": "hit"}), 0)
	assert.NotContains(t, f.logs.String(), refreshed.Password)
	assert.NotContains(t, f.logs.String(), "PRIVATE KEY")
}

// TestGitHubAppConcurrentMint checks that concurrent resolutions share one mint.
func TestGitHubAppConcurrentMint(t *testing.T) {
	t.Parallel()
	_, keyPEM := rsaKey(t)
	clock := newFakeClock(time.Now())
	gh := newFakeGitHub(t, clock, 43, nil)
	f := newFixture(t, []runtime.Object{
		secret("app", v1alpha1.LabelValueSecretTypeRepoCreds, map[string]string{"githubAppPrivateKey": keyPEM}),
		cred("app", "https://github.com/", credWithCreds("app"), func(c *v1alpha1.RepositoryCredential) {
			c.Spec.GitHubApp = githubApp(43, 9, gh.apiURL())
		}),
	}, withClock(clock), withTransport(trustTransport(gh.Server)))
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Go(func() {
			_, err := f.resolver.ResolveForURL(context.Background(), "https://github.com/org/repo", "", read)
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	assert.Equal(t, 1, gh.mintCount(9))
}

// TestGitHubAppInstallationDiscovery covers REPO-016: the installation is discovered for the
// organization in the URL (case-insensitive, https and scp forms), cached, and a missing
// installation produces upstream's message.
func TestGitHubAppInstallationDiscovery(t *testing.T) {
	t.Parallel()
	_, keyPEM := rsaKey(t)
	clock := newFakeClock(time.Now())
	gh := newFakeGitHub(t, clock, 44, map[string]int64{"orga": 11, "orgb": 22})
	f := newFixture(t, []runtime.Object{
		secret("app", v1alpha1.LabelValueSecretTypeRepoCreds, map[string]string{"githubAppPrivateKey": keyPEM}),
		cred("github", "https://github.com/", credWithCreds("app"), func(c *v1alpha1.RepositoryCredential) {
			c.Spec.GitHubApp = githubApp(44, 0, gh.apiURL())
		}),
		cred("github-ssh", "git@github.com:", credWithCreds("app"), func(c *v1alpha1.RepositoryCredential) {
			c.Spec.GitHubApp = githubApp(44, 0, gh.apiURL())
		}),
	}, withClock(clock), withTransport(trustTransport(gh.Server)))
	ctx := context.Background()
	a, err := f.resolver.ResolveForURL(ctx, "https://github.com/OrgA/svc", "", read)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(a.Password, "ghs_installation_11_"))
	b, err := f.resolver.ResolveForURL(ctx, "git@github.com:orgb/svc.git", "", read)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(b.Password, "ghs_installation_22_"))
	a2, err := f.resolver.ResolveForURL(ctx, "https://github.com/orga/other", "", read)
	require.NoError(t, err)
	assert.Equal(t, a.Password, a2.Password, "same organization shares the cached token")
	assert.Equal(t, 1, gh.lookupCount("orga"))
	assert.Equal(t, 1, gh.mintCount(11))

	_, err = f.resolver.ResolveForURL(ctx, "https://github.com/missing/x", "", read)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrTokenUnavailable)
	assert.Contains(t, err.Error(), "installation not found for org: missing")
	assert.Contains(t, err.Error(), "ensure the GitHub App (ID: 44) is installed")

	_, err = f.resolver.ResolveForURL(ctx, "https://github.com/onlyorg", "", read)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidConfiguration)
	assert.Contains(t, err.Error(), "does not contain org/repo format")
}

// TestGitHubAppFromSCMProvider covers a GitHub App supplied by an SCMProvider: the App ID,
// installation and key come from the provider, and its credentials are restricted to the
// provider's host.
func TestGitHubAppFromSCMProvider(t *testing.T) {
	t.Parallel()
	_, keyPEM := rsaKey(t)
	clock := newFakeClock(time.Now())
	gh := newFakeGitHub(t, clock, 45, nil)
	provider := &v1alpha1.SCMProvider{
		ObjectMeta: metaName("ghe"),
		Spec: v1alpha1.SCMProviderSpec{
			Type:   v1alpha1.SCMProviderTypeGitHub,
			APIURL: gh.URL + "/api/v3",
			Auth: &v1alpha1.SCMProviderAuth{GitHubApp: &v1alpha1.SCMProviderGitHubApp{
				AppID: 45, InstallationID: ptrTo(int64(5)),
				PrivateKeySecretRef: v1alpha1.SecretKeyReference{Name: "ghe-app", Key: "private-key"},
			}},
		},
	}
	f := newFixture(t, []runtime.Object{
		secret("ghe-app", v1alpha1.LabelValueSecretTypeSCMCreds, map[string]string{"private-key": keyPEM}),
		provider,
		cred("ghe", "https://127.0.0.1/", func(c *v1alpha1.RepositoryCredential) {
			c.Spec.SCMProviderRef = &v1alpha1.SCMProviderReference{Name: "ghe"}
		}),
		repo("pinned", "https://127.0.0.1/org/pinned", func(r *v1alpha1.Repository) {
			r.Spec.SCMProviderRef = &v1alpha1.SCMProviderReference{Name: "ghe"}
			r.Spec.GitHubApp = githubApp(45, 6, "")
		}),
		repo("mismatch", "https://127.0.0.1/org/mismatch", func(r *v1alpha1.Repository) {
			r.Spec.SCMProviderRef = &v1alpha1.SCMProviderReference{Name: "ghe"}
			r.Spec.GitHubApp = githubApp(99, 0, "")
		}),
	}, withClock(clock), withTransport(trustTransport(gh.Server)))
	ctx := context.Background()
	res, err := f.resolver.Resolve(ctx, Query{URL: "https://127.0.0.1/org/repo"})
	require.NoError(t, err)
	assert.True(t, res.Inherited)
	assert.Equal(t, MethodGitHubApp, res.Method)
	assert.True(t, strings.HasPrefix(res.Repository.Password, "ghs_installation_5_"))

	pinned, err := f.resolver.ResolveForURL(ctx, "https://127.0.0.1/org/pinned", "", read)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(pinned.Password, "ghs_installation_6_"), "the repository's installation ID wins")

	_, err = f.resolver.ResolveForURL(ctx, "https://127.0.0.1/org/mismatch", "", read)
	assert.ErrorIs(t, err, ErrInvalidConfiguration)
	assert.Contains(t, err.Error(), "does not match the App 45")
}

// TestGitHubAppFailures covers an invalid private key and an API refusal.
func TestGitHubAppFailures(t *testing.T) {
	t.Parallel()
	_, keyPEM := rsaKey(t)
	clock := newFakeClock(time.Now())
	gh := newFakeGitHub(t, clock, 46, nil)
	gh.failTokens = true
	f := newFixture(t, []runtime.Object{
		secret("bad", v1alpha1.LabelValueSecretTypeRepository, map[string]string{"githubAppPrivateKey": "not-a-private-key-material"}),
		secret("good", v1alpha1.LabelValueSecretTypeRepository, map[string]string{"githubAppPrivateKey": keyPEM}),
		repo("bad", "https://github.com/org/bad", withCreds("bad"), func(r *v1alpha1.Repository) { r.Spec.GitHubApp = githubApp(46, 1, gh.apiURL()) }),
		repo("refused", "https://github.com/org/refused", withCreds("good"), func(r *v1alpha1.Repository) { r.Spec.GitHubApp = githubApp(46, 1, gh.apiURL()) }),
	}, withClock(clock), withTransport(trustTransport(gh.Server)))
	_, err := f.resolver.ResolveForURL(context.Background(), "https://github.com/org/bad", "", read)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidCredentials)
	assert.Contains(t, err.Error(), "failed to initialize GitHub installation transport")
	assert.NotContains(t, err.Error(), "not-a-private-key-material")

	_, err = f.resolver.ResolveForURL(context.Background(), "https://github.com/org/refused", "", read)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrTokenUnavailable)
	assert.Equal(t, v1alpha1.ReasonConnectionFailed, ConditionReason(err))
	assert.NotContains(t, f.logs.String(), "BEGIN RSA PRIVATE KEY")
}

// TestOwnerFromURL mirrors upstream's ExtractOrgFromRepoURL cases (REPO-016).
func TestOwnerFromURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		url, want string
		wantErr   bool
	}{
		{"https://github.com/argoproj/argo-cd", "argoproj", false},
		{"https://github.com/argoproj/argo-cd.git", "argoproj", false},
		{"https://github.com:443/argoproj/argo-cd.git", "argoproj", false},
		{"git@github.com:argoproj/argo-cd.git", "argoproj", false},
		{"git@github.com:argoproj/argo-cd", "argoproj", false},
		{"ssh://git@github.com:argoproj/argo-cd.git", "argoproj", false},
		{"ssh://git@github.com:22/argoproj/argo-cd.git", "argoproj", false},
		{"git@github.com:22/argoproj/argo-cd.git", "argoproj", false},
		{"https://github.example.com/myorg/myrepo.git", "myorg", false},
		{"git@github.example.com:myorg/myrepo.git", "myorg", false},
		{"https://github.com/ArgoPROJ/argo-cd", "argoproj", false},
		{"https://user:secret@github.com/org/repo", "org", false},
		{"not-a-url", "", true},
		{"", "", true},
		{"https://github.com", "", true},
		{"https://github.com/argoproj", "", true},
	}
	for _, tc := range cases {
		got, err := ownerFromURL(tc.url)
		if tc.wantErr {
			assert.Error(t, err, tc.url)
			continue
		}
		require.NoError(t, err, tc.url)
		assert.Equal(t, tc.want, got, tc.url)
	}
	_, err := ownerFromURL("https://user:topsecret@github.com/onlyorg")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "topsecret")
}

func ptrTo[T any](v T) *T { return &v }
