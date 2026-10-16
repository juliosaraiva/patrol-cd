package clusters

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1alpha1 "github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/clusters/internal/fakecloud"
)

func lookupMap(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

// TestGCPTokenSource covers REPO-112: application default credentials from a service-account
// file, with the cloud-platform and userinfo.email scopes.
func TestGCPTokenSource(t *testing.T) {
	t.Parallel()
	g := fakecloud.NewGoogle(t)
	src := GCPTokenSource(GCPOptions{Lookup: lookupMap(map[string]string{"GOOGLE_APPLICATION_CREDENTIALS": g.CredentialsFile})})
	tok, err := src.Token(context.Background())
	require.NoError(t, err)
	assert.Equal(t, g.AccessToken, tok.Value)
	assert.WithinDuration(t, time.Now().Add(time.Hour), tok.Expiry, 5*time.Minute)

	// The credentials are discovered once; the oauth2 source caches the access token.
	_, err = src.Token(context.Background())
	require.NoError(t, err)
	assertions := g.Assertions()
	require.Len(t, assertions, 1)
	claims := jwt.MapClaims{}
	_, _, err = jwt.NewParser().ParseUnverified(assertions[0], claims)
	require.NoError(t, err)
	assert.Equal(t, "https://www.googleapis.com/auth/cloud-platform https://www.googleapis.com/auth/userinfo.email", claims["scope"])
	assert.Equal(t, "patrol@fake-project.iam.gserviceaccount.com", claims["iss"])
}

func TestGCPTokenSourceErrors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	require.NoError(t, os.WriteFile(bad, []byte(`{"type":"unknown_kind"}`), 0o600))
	_, err := GCPTokenSource(GCPOptions{Lookup: lookupMap(map[string]string{"GOOGLE_APPLICATION_CREDENTIALS": bad})}).Token(context.Background())
	require.ErrorContains(t, err, `unsupported credentials type "unknown_kind"`)
	_, err = GCPTokenSource(GCPOptions{Lookup: lookupMap(map[string]string{"GOOGLE_APPLICATION_CREDENTIALS": filepath.Join(dir, "missing.json")})}).Token(context.Background())
	require.ErrorContains(t, err, "read GOOGLE_APPLICATION_CREDENTIALS")
}

// TestGCPNativeExec authenticates a GKE-style exec configuration in-process: the credentials
// file comes from execProviderConfig.env.
func TestGCPNativeExec(t *testing.T) {
	t.Parallel()
	g := fakecloud.NewGoogle(t)
	srv := newTLSAPIServer(t)
	c := cluster("gke", srv.URL, func(c *v1alpha1.Cluster) {
		c.Spec.Config.ExecProviderConfig = &v1alpha1.ExecProviderConfig{
			Command: "argocd-k8s-auth", Args: []string{"gcp"}, APIVersion: ExecCredentialAPIVersion,
			Env: map[string]string{"GOOGLE_APPLICATION_CREDENTIALS": g.CredentialsFile},
		}
	})
	_, err := probeWith(t, c, map[string][]byte{"caData": srv.caPEM()}, Options{Providers: mustProviderCache(t)})
	require.NoError(t, err)
	assert.Equal(t, "Bearer "+g.AccessToken, srv.lastHeader(t).Get("Authorization"))
}

func entraOptions(e *fakecloud.Entra, env map[string]string) AzureOptions {
	env[EnvAzureAuthorityHost] = e.URL
	env[EnvAzureTenantID] = e.Tenant
	return AzureOptions{
		Lookup:                   lookupMap(env),
		ClientOptions:            azcore.ClientOptions{Transport: e.Client()},
		DisableInstanceDiscovery: true,
	}
}

// TestAzureServicePrincipal covers REPO-113 with the spn login method against a mock tenant.
func TestAzureServicePrincipal(t *testing.T) {
	t.Parallel()
	e := fakecloud.NewEntra(t)
	src, err := AzureTokenSource(entraOptions(e, map[string]string{
		EnvAzureLoginMethod:  "spn",
		EnvAzureClientID:     "client-123",
		EnvAzureClientSecret: "client-secret",
	}))
	require.NoError(t, err)
	tok, err := src.Token(context.Background())
	require.NoError(t, err)
	assert.Equal(t, e.AccessToken, tok.Value)
	assert.WithinDuration(t, time.Now().Add(time.Hour), tok.Expiry, 5*time.Minute)
	reqs := e.TokenRequests()
	require.NotEmpty(t, reqs)
	last := reqs[len(reqs)-1]
	assert.Equal(t, "client_credentials", last.Get("grant_type"))
	assert.Equal(t, "client-123", last.Get("client_id"))
	assert.Equal(t, "client-secret", last.Get("client_secret"))
	assert.Contains(t, strings.Fields(last.Get("scope")), AzureDefaultServerApplicationID+"/.default")
}

// TestAzureDefaultsToWorkloadIdentity: without AAD_LOGIN_METHOD the federated token file is
// exchanged (workload identity), and AAD_SERVER_APPLICATION_ID overrides the audience.
func TestAzureDefaultsToWorkloadIdentity(t *testing.T) {
	t.Parallel()
	e := fakecloud.NewEntra(t)
	fed := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(fed, []byte("federated-sa-token"), 0o600))
	src, err := AzureTokenSource(entraOptions(e, map[string]string{
		EnvAzureClientID:            "wi-client",
		EnvAzureFederatedTokenFile:  fed,
		EnvAzureServerApplicationID: "custom-server-app",
	}))
	require.NoError(t, err)
	tok, err := src.Token(context.Background())
	require.NoError(t, err)
	assert.Equal(t, e.AccessToken, tok.Value)
	reqs := e.TokenRequests()
	require.NotEmpty(t, reqs)
	last := reqs[len(reqs)-1]
	assert.Equal(t, "federated-sa-token", last.Get("client_assertion"))
	assert.Equal(t, "urn:ietf:params:oauth:client-assertion-type:jwt-bearer", last.Get("client_assertion_type"))
	assert.Contains(t, strings.Fields(last.Get("scope")), "custom-server-app/.default")
}

func TestAzureErrors(t *testing.T) {
	t.Parallel()
	_, err := AzureTokenSource(AzureOptions{Lookup: lookupMap(map[string]string{EnvAzureLoginMethod: "devicecode"})})
	require.EqualError(t, err, `azure: unsupported login method "devicecode"`)
	_, err = AzureTokenSource(AzureOptions{Lookup: lookupMap(map[string]string{EnvAzureEnvironmentName: "Mars"})})
	require.EqualError(t, err, `azure: unknown environment "Mars"`)

	// An incomplete environment fails on use, not on construction.
	src, err := AzureTokenSource(AzureOptions{Lookup: lookupMap(map[string]string{})})
	require.NoError(t, err)
	_, err = src.Token(context.Background())
	require.ErrorContains(t, err, "azure: create workloadidentity credential")

	_, err = AzureTokenSource(AzureOptions{Lookup: lookupMap(map[string]string{EnvAzureLoginMethod: "spn", EnvAzureClientID: "c", EnvAzureTenantID: "t"})})
	require.NoError(t, err)
}

// TestAzureNativeExec authenticates an AKS-style exec configuration in-process with the
// variables from execProviderConfig.env.
func TestAzureNativeExec(t *testing.T) {
	t.Parallel()
	e := fakecloud.NewEntra(t)
	srv := newTLSAPIServer(t)
	c := cluster("aks", srv.URL, func(c *v1alpha1.Cluster) {
		c.Spec.Config.ExecProviderConfig = &v1alpha1.ExecProviderConfig{
			Command: "patrolcd", Args: []string{"k8s-auth", "azure"}, APIVersion: ExecCredentialAPIVersion,
			Env: map[string]string{
				EnvAzureLoginMethod: "spn", EnvAzureClientID: "aks-client", EnvAzureClientSecret: "aks-secret",
				EnvAzureTenantID: e.Tenant, EnvAzureAuthorityHost: e.URL,
			},
		}
	})
	opts := Options{
		Providers: mustProviderCache(t),
		Azure:     AzureOptions{ClientOptions: azcore.ClientOptions{Transport: e.Client()}, DisableInstanceDiscovery: true},
	}
	_, err := probeWith(t, c, map[string][]byte{"caData": srv.caPEM()}, opts)
	require.NoError(t, err)
	assert.Equal(t, "Bearer "+e.AccessToken, srv.lastHeader(t).Get("Authorization"))
}

// TestCloudAuthWithClientCertUsesMutualTLS covers REPO-091 for cloud authentication combined
// with a client certificate: client-go never runs an exec plugin when cert auth is configured,
// so neither the exec form nor the in-process token sources may send a bearer token, and no
// cloud credentials are needed.
func TestCloudAuthWithClientCertUsesMutualTLS(t *testing.T) {
	fakecloud.IsolateAWSEnv(t)
	sts := fakecloud.NewSTS(t)
	g := fakecloud.NewGoogle(t)
	pki := newTestPKI(t)
	srv := newMTLSAPIServer(t, pki)
	certPEM, keyPEM := pki.issue(t, "patrol", true, nil, nil)
	data := map[string][]byte{"caData": pki.caPEM, "certData": certPEM, "keyData": keyPEM}
	missing := filepath.Join(t.TempDir(), "missing-patrolcd")

	eks := cluster("eks", srv.URL, func(c *v1alpha1.Cluster) {
		c.Spec.Config.AWSAuthConfig = &v1alpha1.AWSAuthConfig{ClusterName: "eks-mtls", RoleARN: "arn:aws:iam::123:role/r"}
	})
	execCluster := func(command string, args []string, env map[string]string) *v1alpha1.Cluster {
		return cluster("exec", srv.URL, func(c *v1alpha1.Cluster) {
			c.Spec.Config.ExecProviderConfig = &v1alpha1.ExecProviderConfig{
				Command: command, Args: args, Env: env, APIVersion: ExecCredentialAPIVersion,
			}
		})
	}
	gcpEnv := map[string]string{"GOOGLE_APPLICATION_CREDENTIALS": g.CredentialsFile}
	tests := []struct {
		name string
		c    *v1alpha1.Cluster
		opts Options
	}{
		{
			name: "AWS in-process",
			c:    eks,
			opts: Options{Providers: mustProviderCache(t), AWS: AWSTokenRequest{
				Region: "us-west-2", STSEndpoint: sts.URL, LoadOptions: staticAWS("AKIDSTATIC0000000004"),
			}},
		},
		{name: "AWS in-process without AWS credentials", c: eks, opts: Options{Providers: mustProviderCache(t)}},
		{name: "AWS exec form", c: eks, opts: Options{AWSAuthExec: true, K8sAuthCommand: missing}},
		{name: "GCP helper in-process", c: execCluster("argocd-k8s-auth", []string{"gcp"}, gcpEnv), opts: Options{Providers: mustProviderCache(t)}},
		{name: "patrolcd helper in-process", c: execCluster("patrolcd", []string{"k8s-auth", "gcp"}, gcpEnv), opts: Options{Providers: mustProviderCache(t)}},
		{name: "other exec plugin", c: execCluster(missing, []string{"token"}, nil)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := srv.requestCount()
			_, err := probeWith(t, tc.c, data, tc.opts)
			require.NoError(t, err)
			require.Greater(t, srv.requestCount(), before)
			assert.Empty(t, srv.lastHeader(t).Get("Authorization"))
		})
	}
	assert.Empty(t, g.Assertions(), "no GCP token was requested")
	assert.Empty(t, sts.Requests(), "no role was assumed")
}

// TestBearerTransportRefreshesOn401 checks that a rejected token is dropped from the cache.
func TestBearerTransportRefreshesOn401(t *testing.T) {
	t.Parallel()
	srv := newPlainAPIServer(t)
	srv.mu.Lock()
	srv.statuses = []int{http.StatusUnauthorized}
	srv.mu.Unlock()
	n := 0
	src := newCachingTokenSource(TokenSourceFunc(func(context.Context) (Token, error) {
		n++
		return Token{Value: "t" + strings.Repeat("x", n), Expiry: time.Now().Add(time.Hour)}, nil
	}), nil)
	client := &http.Client{Transport: &bearerTransport{base: http.DefaultTransport, src: src}}
	for i := 0; i < 2; i++ {
		resp, err := client.Get(srv.URL + "/version")
		require.NoError(t, err)
		_ = resp.Body.Close()
	}
	assert.Equal(t, "Bearer txx", srv.lastHeader(t).Get("Authorization"), "the token was refreshed after the 401")
}

// TestCachingTokenSourceRefreshesBeforeExpiry checks the one-minute refresh margin.
func TestCachingTokenSourceRefreshesBeforeExpiry(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	calls := 0
	src := newCachingTokenSource(TokenSourceFunc(func(context.Context) (Token, error) {
		calls++
		return Token{Value: "tok", Expiry: now.Add(10 * time.Minute)}, nil
	}), func() time.Time { return now })
	ctx := context.Background()
	_, err := src.Token(ctx)
	require.NoError(t, err)
	now = now.Add(8 * time.Minute)
	_, err = src.Token(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, calls, "token still valid for more than a minute")
	now = now.Add(90 * time.Second)
	_, err = src.Token(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, calls, "token refreshed within the margin")
}
