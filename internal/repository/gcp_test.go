package repository

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"

	v1alpha1 "github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// fakeGoogleTokenEndpoint is an OAuth 2.0 token endpoint answering the JWT bearer grant of
// a service account key.
type fakeGoogleTokenEndpoint struct {
	*httptest.Server
	mu    sync.Mutex
	calls int
	fail  bool
}

func newFakeGoogleTokenEndpoint(t *testing.T) *fakeGoogleTokenEndpoint {
	t.Helper()
	g := &fakeGoogleTokenEndpoint{}
	g.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.Method != http.MethodPost || r.URL.Path != "/token" ||
			r.PostForm.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" || r.PostForm.Get("assertion") == "" {
			http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
			return
		}
		g.mu.Lock()
		g.calls++
		n, fail := g.calls, g.fail
		g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if fail {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"Invalid JWT Signature."}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": fmt.Sprintf("ya29.fake-access-token-%d", n),
			"token_type":   "Bearer",
			"expires_in":   3600,
		})
	}))
	t.Cleanup(g.Close)
	return g
}

func (g *fakeGoogleTokenEndpoint) callCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

func serviceAccountKey(t *testing.T, tokenURI, email string) string {
	t.Helper()
	_, keyPEM := rsaKey(t)
	b, err := json.Marshal(map[string]string{
		"type":           "service_account",
		"project_id":     "patrol-test",
		"private_key_id": "0123456789abcdef",
		"private_key":    keyPEM,
		"client_email":   email,
		"client_id":      "1234567890",
		"token_uri":      tokenURI,
	})
	require.NoError(t, err)
	return string(b)
}

// TestGCPServiceAccountToken covers REPO-018: the service account key is exchanged at its
// token_uri, the git user name is client_email, the token is reused until shortly before it
// expires and then minted again.
func TestGCPServiceAccountToken(t *testing.T) {
	t.Parallel()
	endpoint := newFakeGoogleTokenEndpoint(t)
	clock := newFakeClock(time.Now())
	key := serviceAccountKey(t, endpoint.URL+"/token", "deployer@patrol-test.iam.gserviceaccount.com")
	f := newFixture(t, []runtime.Object{
		secret("gcp", v1alpha1.LabelValueSecretTypeRepository, map[string]string{"gcpServiceAccountKey": key}),
		repo("csr", "https://source.developers.google.com/p/patrol-test/r/app", withCreds("gcp")),
	}, withClock(clock), withTransport(trustTransport(endpoint.Server)), withLoopbackIssuers())
	ctx := t.Context()

	res, err := f.resolver.Resolve(ctx, Query{URL: "https://source.developers.google.com/p/patrol-test/r/app"})
	require.NoError(t, err)
	assert.Equal(t, MethodGCP, res.Method)
	first := res.Repository
	assert.Equal(t, "deployer@patrol-test.iam.gserviceaccount.com", first.Username)
	assert.Equal(t, "ya29.fake-access-token-1", first.Password)
	assert.Empty(t, first.BearerToken)
	require.NotNil(t, first.CredentialsExpireAt)
	assert.WithinDuration(t, time.Now().Add(time.Hour), *first.CredentialsExpireAt, time.Minute)
	assert.Equal(t, 1, endpoint.callCount())

	clock.Advance(50 * time.Minute)
	again, err := f.resolver.GetRepository(ctx, "https://source.developers.google.com/p/patrol-test/r/app", "")
	require.NoError(t, err)
	assert.Equal(t, first.Password, again.Password)
	assert.Equal(t, 1, endpoint.callCount(), "cached until five minutes before expiry")

	clock.Advance(6 * time.Minute)
	refreshed, err := f.resolver.GetRepository(ctx, "https://source.developers.google.com/p/patrol-test/r/app", "")
	require.NoError(t, err)
	assert.Equal(t, "ya29.fake-access-token-2", refreshed.Password)
	assert.Equal(t, 2, endpoint.callCount())
	assert.NotContains(t, f.logs.String(), "ya29.")
	assert.NotContains(t, f.logs.String(), "PRIVATE KEY")
}

// TestGCPServiceAccountErrors covers invalid keys (REPO-018), rejected external_account
// credentials and token endpoint refusals.
func TestGCPServiceAccountErrors(t *testing.T) {
	t.Parallel()
	endpoint := newFakeGoogleTokenEndpoint(t)
	endpoint.fail = true
	external := `{"type":"external_account","audience":"//iam.googleapis.com/x","subject_token_type":"urn:ietf:params:oauth:token-type:jwt","token_url":"https://sts.googleapis.com/v1/token","credential_source":{"file":"/etc/passwd"}}`
	impersonatedBad := `{"type":"impersonated_service_account","service_account_impersonation_url":"https://x","source_credentials":{"type":"external_account"}}`
	f := newFixture(t, []runtime.Object{
		secret("garbage", v1alpha1.LabelValueSecretTypeRepository, map[string]string{"gcpServiceAccountKey": "{not json"}),
		secret("external", v1alpha1.LabelValueSecretTypeRepository, map[string]string{"gcpServiceAccountKey": external}),
		secret("impersonated", v1alpha1.LabelValueSecretTypeRepository, map[string]string{"gcpServiceAccountKey": impersonatedBad}),
		secret("nokey", v1alpha1.LabelValueSecretTypeRepository, map[string]string{"gcpServiceAccountKey": `{"type":"service_account","client_email":"a@b"}`}),
		secret("refused", v1alpha1.LabelValueSecretTypeRepository, map[string]string{"gcpServiceAccountKey": serviceAccountKey(t, endpoint.URL+"/token", "x@y")}),
		repo("garbage", "https://source.developers.google.com/p/x/r/garbage", withCreds("garbage")),
		repo("external", "https://source.developers.google.com/p/x/r/external", withCreds("external")),
		repo("impersonated", "https://source.developers.google.com/p/x/r/impersonated", withCreds("impersonated")),
		repo("nokey", "https://source.developers.google.com/p/x/r/nokey", withCreds("nokey")),
		repo("refused", "https://source.developers.google.com/p/x/r/refused", withCreds("refused")),
	}, withTransport(trustTransport(endpoint.Server)), withLoopbackIssuers())
	cases := []struct {
		url      string
		sentinel error
		contains string
	}{
		{"https://source.developers.google.com/p/x/r/garbage", ErrInvalidCredentials, errGCPInvalid},
		{"https://source.developers.google.com/p/x/r/external", ErrInvalidCredentials, `unsupported credentials type "external_account"`},
		{"https://source.developers.google.com/p/x/r/impersonated", ErrInvalidCredentials, "source credentials must be"},
		{"https://source.developers.google.com/p/x/r/nokey", ErrInvalidCredentials, errGCPInvalid},
		{"https://source.developers.google.com/p/x/r/refused", ErrTokenUnavailable, "failed to get access token from Google credentials"},
	}
	for _, tc := range cases {
		_, err := f.resolver.ResolveForURL(t.Context(), tc.url, "", read)
		require.Error(t, err, tc.url)
		assert.ErrorIs(t, err, tc.sentinel, tc.url)
		assert.Contains(t, err.Error(), tc.contains, tc.url)
		assert.NotContains(t, err.Error(), "BEGIN RSA PRIVATE KEY", tc.url)
	}
	assert.Equal(t, 1, endpoint.callCount())
}
