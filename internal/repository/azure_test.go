package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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

// fakeWorkloadIdentity is an AzureTokenProvider issuing numbered tokens per scope.
type fakeWorkloadIdentity struct {
	clock *fakeClock
	ttl   time.Duration
	err   error

	mu     sync.Mutex
	scopes []string
}

func (p *fakeWorkloadIdentity) GetToken(_ context.Context, scope string) (string, time.Time, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return "", time.Time{}, p.err
	}
	p.scopes = append(p.scopes, scope)
	return fmt.Sprintf("entra-token-%d-for-%s", len(p.scopes), scope), p.clock.Now().Add(p.ttl), nil
}

func (p *fakeWorkloadIdentity) calls() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.scopes...)
}

// TestAzureWorkloadIdentityGit covers REPO-019 (git): the Azure DevOps scope, the empty GUID
// user name, the token as password and bearer token, and caching until shortly before
// expiry. Templates always overwrite useAzureWorkloadIdentity.
func TestAzureWorkloadIdentityGit(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(time.Now())
	wi := &fakeWorkloadIdentity{clock: clock, ttl: time.Hour}
	f := newFixture(t, []runtime.Object{
		repo("ado", "https://dev.azure.com/org/project/_git/repo", func(r *v1alpha1.Repository) { r.Spec.UseAzureWorkloadIdentity = true }),
		repo("plain", "https://dev.azure.com/org/project/_git/plain"),
		cred("ado", "https://dev.azure.com/org/", func(c *v1alpha1.RepositoryCredential) { c.Spec.UseAzureWorkloadIdentity = true }),
	}, withClock(clock), withAzureWI(wi))
	ctx := t.Context()

	res, err := f.resolver.Resolve(ctx, Query{URL: "https://dev.azure.com/org/project/_git/repo"})
	require.NoError(t, err)
	assert.Equal(t, MethodAzureWorkloadIdentity, res.Method)
	assert.False(t, res.Inherited, "useAzureWorkloadIdentity counts as credentials")
	r := res.Repository
	assert.Equal(t, AzureEmptyGUID, r.Username)
	assert.Equal(t, "entra-token-1-for-"+AzureDevOpsScope, r.Password)
	assert.Equal(t, r.Password, r.BearerToken)
	require.NotNil(t, r.CredentialsExpireAt)
	assert.True(t, r.CredentialsExpireAt.Equal(clock.Now().Add(time.Hour)))

	// A stub under the template prefix inherits the flag and shares the cached token.
	inherited, err := f.resolver.Resolve(ctx, Query{URL: "https://dev.azure.com/org/project/_git/plain"})
	require.NoError(t, err)
	assert.True(t, inherited.Inherited)
	assert.Equal(t, r.Password, inherited.Repository.Password)
	assert.Equal(t, []string{AzureDevOpsScope}, wi.calls())

	clock.Advance(56 * time.Minute)
	refreshed, err := f.resolver.GetRepository(ctx, "https://dev.azure.com/org/project/_git/repo", "")
	require.NoError(t, err)
	assert.Equal(t, "entra-token-2-for-"+AzureDevOpsScope, refreshed.Password)

	wi.err = errors.New("AADSTS700016: application not found")
	clock.Advance(time.Hour)
	_, err = f.resolver.GetRepository(ctx, "https://dev.azure.com/org/project/_git/repo", "")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrTokenUnavailable)
	assert.Contains(t, err.Error(), "failed to get Azure access token")
}

// fakeEntraID is a Microsoft Entra ID authority (OpenID discovery and the client
// credentials token endpoint) for one tenant.
type fakeEntraID struct {
	*httptest.Server
	tenant, clientID, secret string

	mu         sync.Mutex
	discovery  int
	tokens     int
	lastScopes string
}

func newFakeEntraID(t *testing.T, tenant, clientID, clientSecret string) *fakeEntraID {
	t.Helper()
	e := &fakeEntraID{tenant: tenant, clientID: clientID, secret: clientSecret}
	e.Server = httptest.NewTLSServer(http.HandlerFunc(e.serve))
	t.Cleanup(e.Close)
	return e
}

func (e *fakeEntraID) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	base := e.URL + "/" + e.tenant
	switch r.URL.Path {
	case "/" + e.tenant + "/v2.0/.well-known/openid-configuration":
		e.mu.Lock()
		e.discovery++
		e.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"authorization_endpoint": base + "/oauth2/v2.0/authorize",
			"token_endpoint":         base + "/oauth2/v2.0/token",
			"issuer":                 base + "/v2.0",
		})
	case "/" + e.tenant + "/oauth2/v2.0/token":
		if err := r.ParseForm(); err != nil || r.PostForm.Get("client_id") != e.clientID ||
			r.PostForm.Get("client_secret") != e.secret || r.PostForm.Get("grant_type") != "client_credentials" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_client","error_description":"AADSTS7000215: Invalid client secret provided."}`))
			return
		}
		e.mu.Lock()
		e.tokens++
		n := e.tokens
		e.lastScopes = r.PostForm.Get("scope")
		e.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token_type":     "Bearer",
			"expires_in":     3600,
			"ext_expires_in": 3600,
			"access_token":   fmt.Sprintf("sp-access-token-%d", n),
		})
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not_found"}`))
	}
}

func (e *fakeEntraID) counts() (discovery, tokens int, scopes string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.discovery, e.tokens, e.lastScopes
}

// TestAzureServicePrincipal covers REPO-020: a client secret credential against a custom
// Active Directory endpoint (instance discovery disabled), the Azure DevOps scope, the
// empty GUID user name, the cache duration cap and refusal handling.
func TestAzureServicePrincipal(t *testing.T) {
	t.Parallel()
	entra := newFakeEntraID(t, "tenant-1", "client-1", "sp-client-secret-value")
	clock := newFakeClock(time.Now())
	spec := func(r *v1alpha1.Repository) {
		r.Spec.AzureServicePrincipal = &v1alpha1.AzureServicePrincipal{ClientID: "client-1", TenantID: "tenant-1", ActiveDirectoryEndpoint: entra.URL}
	}
	f := newFixture(t, []runtime.Object{
		secret("sp", v1alpha1.LabelValueSecretTypeRepository, map[string]string{"azureServicePrincipalClientSecret": "sp-client-secret-value"}),
		secret("sp-wrong", v1alpha1.LabelValueSecretTypeRepository, map[string]string{"azureServicePrincipalClientSecret": "wrong-secret-value"}),
		repo("ado", "https://dev.azure.com/org/p/_git/r", withCreds("sp"), spec),
		repo("wrong", "https://dev.azure.com/org/p/_git/wrong", withCreds("sp-wrong"), spec),
	}, withClock(clock), withTransport(trustTransport(entra.Server)), withLoopbackIssuers(), withOptions(func(o *Options) {
		o.AzureServicePrincipalCacheTTL = 10 * time.Minute
	}))
	ctx := t.Context()

	res, err := f.resolver.Resolve(ctx, Query{URL: "https://dev.azure.com/org/p/_git/r"})
	require.NoError(t, err)
	assert.Equal(t, MethodAzureServicePrincipal, res.Method)
	r := res.Repository
	assert.Equal(t, AzureEmptyGUID, r.Username)
	assert.Equal(t, "sp-access-token-1", r.Password)
	assert.Equal(t, r.Password, r.BearerToken)
	require.NotNil(t, r.CredentialsExpireAt)
	discovery, tokens, scopes := entra.counts()
	assert.Equal(t, 1, discovery)
	assert.Equal(t, 1, tokens)
	assert.Contains(t, scopes, AzureDevOpsScope)

	clock.Advance(9 * time.Minute)
	again, err := f.resolver.GetRepository(ctx, "https://dev.azure.com/org/p/_git/r", "")
	require.NoError(t, err)
	assert.Equal(t, "sp-access-token-1", again.Password)
	clock.Advance(2 * time.Minute)
	again, err = f.resolver.GetRepository(ctx, "https://dev.azure.com/org/p/_git/r", "")
	require.NoError(t, err)
	assert.Equal(t, "sp-access-token-2", again.Password, "reuse is capped by the cache duration")

	_, err = f.resolver.GetRepository(ctx, "https://dev.azure.com/org/p/_git/wrong", "")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrTokenUnavailable)
	assert.NotContains(t, err.Error(), "wrong-secret-value")
	assert.NotContains(t, f.logs.String(), "sp-client-secret-value")
	assert.NotContains(t, f.logs.String(), "sp-access-token")
}

// TestAzureServicePrincipalCacheCap checks that the cache duration is capped at 59 minutes
// with a warning (upstream ARGOCD_AZURE_SERVICE_PRINCIPAL_CREDS_EXPIRATION_DURATION).
func TestAzureServicePrincipalCacheCap(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil, withOptions(func(o *Options) { o.AzureServicePrincipalCacheTTL = 2 * time.Hour }))
	assert.Equal(t, MaxAzureServicePrincipalCacheTTL, f.resolver.azureSPCacheTTL)
	assert.Contains(t, f.logs.String(), "Azure service principal token cache duration capped")
	g := newFixture(t, nil)
	assert.Equal(t, MaxAzureServicePrincipalCacheTTL, g.resolver.azureSPCacheTTL)
}

// fakeACR is an Azure Container Registry: /v2/ issues a Bearer challenge and
// /oauth2/exchange trades an ARM token for a refresh token (a JWT with an exp claim).
type fakeACR struct {
	*httptest.Server
	clock *fakeClock
	// challenge overrides the Www-Authenticate header; "-" sends none with 200.
	challenge string

	mu        sync.Mutex
	exchanges int
	armTokens []string
	services  []string
}

func newFakeACR(t *testing.T, clock *fakeClock, challenge string) *fakeACR {
	t.Helper()
	a := &fakeACR{clock: clock, challenge: challenge}
	a.Server = httptest.NewTLSServer(http.HandlerFunc(a.serve))
	t.Cleanup(a.Close)
	return a
}

func (a *fakeACR) host() string { return strings.TrimPrefix(a.URL, "https://") }

func (a *fakeACR) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v2/":
		switch a.challenge {
		case "-":
			w.WriteHeader(http.StatusOK)
		case "":
			w.Header().Set("Www-Authenticate", fmt.Sprintf(`Bearer realm="https://%s/oauth2/token",service="%s"`, a.host(), a.host()))
			w.WriteHeader(http.StatusUnauthorized)
		default:
			w.Header().Set("Www-Authenticate", a.challenge)
			w.WriteHeader(http.StatusUnauthorized)
		}
	case "/oauth2/exchange":
		if err := r.ParseForm(); err != nil || r.PostForm.Get("grant_type") != "access_token" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		a.mu.Lock()
		a.exchanges++
		a.armTokens = append(a.armTokens, r.PostForm.Get("access_token"))
		a.services = append(a.services, r.PostForm.Get("service"))
		n := a.exchanges
		a.mu.Unlock()
		tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
			"exp": a.clock.Now().Add(3 * time.Hour).Unix(),
			"jti": fmt.Sprintf("refresh-%d", n),
		})
		signed, err := tok.SignedString([]byte("registry-signing-key"))
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"refresh_token": signed})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// TestAzureWorkloadIdentityACR covers REPO-019 (Helm): the challenge, the ARM token for
// AZURE_ARM_TOKEN_RESOURCE, the exchange, the refresh token as Helm password with the JWT
// expiry, and caching per registry.
func TestAzureWorkloadIdentityACR(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(time.Now().Truncate(time.Second))
	acr := newFakeACR(t, clock, "")
	wi := &fakeWorkloadIdentity{clock: clock, ttl: time.Hour}
	chartURL := acr.host() + "/helm"
	f := newFixture(t, []runtime.Object{
		repo("acr", chartURL, withType(v1alpha1.RepositoryTypeHelm), func(r *v1alpha1.Repository) {
			r.Spec.Name = "acr"
			r.Spec.UseAzureWorkloadIdentity = true
			// "127.0.0.1:port/path" does not parse as a URL, so upstream IsHelmOciRepo does
			// not recognize it; enableOCI is explicit here.
			r.Spec.EnableOCI = true
		}),
	}, withClock(clock), withAzureWI(wi), withTransport(trustTransport(acr.Server)), withLoopbackIssuers(), withOptions(func(o *Options) {
		o.LookupEnv = func(k string) (string, bool) {
			if k == EnvAzureARMTokenResource {
				return "https://containerregistry.azure.net", true
			}
			return "", false
		}
	}))
	ctx := t.Context()

	res, err := f.resolver.Resolve(ctx, Query{URL: chartURL, Type: v1alpha1.RepositoryTypeHelm})
	require.NoError(t, err)
	assert.Equal(t, MethodAzureWorkloadIdentity, res.Method)
	r := res.Repository
	assert.True(t, r.EnableOCI)
	assert.Equal(t, AzureEmptyGUID, r.Username)
	exp, err := jwtExpiry(r.Password)
	require.NoError(t, err)
	require.NotNil(t, r.CredentialsExpireAt)
	assert.True(t, r.CredentialsExpireAt.Equal(exp))
	assert.WithinDuration(t, clock.Now().Add(3*time.Hour), exp, 0)
	assert.Equal(t, []string{"https://containerregistry.azure.net/.default"}, wi.calls())
	acr.mu.Lock()
	assert.Equal(t, []string{"entra-token-1-for-https://containerregistry.azure.net/.default"}, acr.armTokens)
	assert.Equal(t, []string{acr.host()}, acr.services)
	acr.mu.Unlock()

	clock.Advance(2 * time.Hour)
	again, err := f.resolver.Resolve(ctx, Query{URL: chartURL, Type: v1alpha1.RepositoryTypeHelm})
	require.NoError(t, err)
	assert.Equal(t, r.Password, again.Repository.Password, "cached until shortly before the JWT expiry")
	clock.Advance(56 * time.Minute)
	again, err = f.resolver.Resolve(ctx, Query{URL: chartURL, Type: v1alpha1.RepositoryTypeHelm})
	require.NoError(t, err)
	assert.NotEqual(t, r.Password, again.Repository.Password)
	assert.NotContains(t, f.logs.String(), r.Password)
}

// TestAzureWorkloadIdentityACRChallengeErrors covers the documented challenge failures
// (REPO-019) and Patrol's realm host restriction.
func TestAzureWorkloadIdentityACRChallengeErrors(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(time.Now())
	cases := []struct {
		name, challenge, want string
	}{
		{"no challenge", "-", "did not issue a challenge"},
		{"basic", `Basic realm="registry"`, "registry does not allow 'Bearer' authentication, got 'Basic'"},
		{"no realm", `Bearer service="svc"`, "realm parameter not found in challenge"},
		{"no service", `Bearer realm="https://example.invalid/oauth2/token"`, "service parameter not found in challenge"},
		{"foreign realm", `Bearer realm="https://attacker.example/oauth2/token",service="svc"`, "is not an https URL on the registry host"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			acr := newFakeACR(t, clock, tc.challenge)
			wi := &fakeWorkloadIdentity{clock: clock, ttl: time.Hour}
			f := newFixture(t, []runtime.Object{
				repo("acr", acr.host()+"/helm", withType(v1alpha1.RepositoryTypeHelm), func(r *v1alpha1.Repository) { r.Spec.UseAzureWorkloadIdentity = true }),
			}, withClock(clock), withAzureWI(wi), withTransport(trustTransport(acr.Server)), withLoopbackIssuers())
			_, err := f.resolver.GetRepository(t.Context(), acr.host()+"/helm", "")
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrTokenUnavailable)
			assert.Contains(t, err.Error(), tc.want)
			if tc.name == "foreign realm" {
				assert.Empty(t, wi.calls(), "no ARM token is requested for a foreign realm")
			}
		})
	}
}

// TestChallengeAndRegistryParsing covers the parsing helpers of the ACR flow.
func TestChallengeAndRegistryParsing(t *testing.T) {
	t.Parallel()
	assert.Equal(t, map[string]string{"realm": "https://r.example/oauth2/token", "service": "r.example", "scope": "a,b"},
		parseChallengeParams(`realm="https://r.example/oauth2/token", service=r.example,scope="a,b"`))
	assert.Equal(t, map[string]string{"realm": "x"}, parseChallengeParams(`Realm="x"`))
	assert.Empty(t, parseChallengeParams(`garbage`))
	assert.Equal(t, map[string]string{"realm": "unterminated"}, parseChallengeParams(`realm="unterminated`))

	for in, want := range map[string]string{
		"myacr.azurecr.io/helm":         "myacr.azurecr.io",
		"oci://MyACR.azurecr.io/charts": "myacr.azurecr.io",
		"https://reg.example:5000/x":    "reg.example:5000",
		"user@host/x":                   "",
		"":                              "",
		":5000/x":                       "",
	} {
		assert.Equal(t, want, registryHost(in), in)
	}
	for in, want := range map[string]bool{
		"myacr.azurecr.io/helm":    true,
		"registry.example:5000/x":  true,  // parsed as scheme "registry.example", no host
		"127.0.0.1:5000/x":         false, // upstream quirk: not parseable as a URL
		"https://charts.example":   false,
		"oci://registry.example/x": false,
		"":                         false,
	} {
		assert.Equal(t, want, isHelmOCIRepo(in), in)
	}
	_, err := jwtExpiry("not-a-jwt")
	assert.Error(t, err)
	noExp, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"sub": "x"}).SignedString([]byte("k"))
	require.NoError(t, err)
	_, err = jwtExpiry(noExp)
	assert.Error(t, err)
}
