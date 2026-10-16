// Package fakecloud provides in-process fakes of the cloud endpoints the cluster
// authentication helpers talk to: AWS STS (AssumeRole), the Google OAuth token endpoint with a
// service-account ADC file, and a Microsoft Entra ID tenant. They exist for tests only.
package fakecloud

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// STS is a fake AWS STS endpoint answering AssumeRole with fixed temporary credentials.
type STS struct {
	*httptest.Server

	mu       sync.Mutex
	requests []url.Values
}

// AssumedAccessKeyID is the access key returned by STS.AssumeRole.
const AssumedAccessKeyID = "ASIAFAKEASSUMEDKEY01"

// NewSTS starts a fake STS server; it is closed at test cleanup.
func NewSTS(t testing.TB) *STS {
	t.Helper()
	s := &STS{}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

func (s *STS) serve(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.requests = append(s.requests, r.Form)
	s.mu.Unlock()
	if r.Form.Get("Action") != "AssumeRole" {
		http.Error(w, "unsupported action", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/xml")
	_, _ = fmt.Fprintf(w, `<AssumeRoleResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/">
  <AssumeRoleResult>
    <Credentials>
      <AccessKeyId>%s</AccessKeyId>
      <SecretAccessKey>fakeSecretAccessKeyForAssumedRole</SecretAccessKey>
      <SessionToken>fake-session-token</SessionToken>
      <Expiration>2099-01-01T00:00:00Z</Expiration>
    </Credentials>
    <AssumedRoleUser>
      <AssumedRoleId>AROAFAKE:session</AssumedRoleId>
      <Arn>%s</Arn>
    </AssumedRoleUser>
  </AssumeRoleResult>
  <ResponseMetadata><RequestId>fake</RequestId></ResponseMetadata>
</AssumeRoleResponse>`, AssumedAccessKeyID, r.Form.Get("RoleArn"))
}

// Requests returns the form values of the requests received so far.
func (s *STS) Requests() []url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]url.Values(nil), s.requests...)
}

// IsolateAWSEnv points the AWS shared config and credential files at empty locations and
// clears AWS variables, so tests never read the developer's AWS configuration.
func IsolateAWSEnv(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	for _, k := range []string{
		"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_PROFILE", "AWS_DEFAULT_PROFILE",
		"AWS_REGION", "AWS_DEFAULT_REGION", "AWS_ROLE_ARN", "AWS_WEB_IDENTITY_TOKEN_FILE",
		"AWS_CONTAINER_CREDENTIALS_FULL_URI", "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_ENDPOINT_URL",
		"AWS_ENDPOINT_URL_STS",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials"))
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
}

// Google is a fake Google OAuth token endpoint plus a service-account ADC file pointing at it.
type Google struct {
	*httptest.Server
	// CredentialsFile is the path of the service-account JSON file.
	CredentialsFile string
	// AccessToken is the token the endpoint returns.
	AccessToken string

	mu         sync.Mutex
	assertions []string
}

// NewGoogle starts the token endpoint and writes the credentials file.
func NewGoogle(t testing.TB) *Google {
	t.Helper()
	g := &Google{AccessToken: "ya29.fake-gke-access-token"}
	g.Server = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.Close)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	creds := map[string]string{
		"type":           "service_account",
		"project_id":     "fake-project",
		"private_key_id": "fake-key-id",
		"private_key":    string(keyPEM),
		"client_email":   "patrol@fake-project.iam.gserviceaccount.com",
		"client_id":      "1234567890",
		"token_uri":      g.URL + "/token",
	}
	b, err := json.Marshal(creds)
	if err != nil {
		t.Fatalf("marshal credentials: %v", err)
	}
	g.CredentialsFile = filepath.Join(t.TempDir(), "adc.json")
	if err := os.WriteFile(g.CredentialsFile, b, 0o600); err != nil {
		t.Fatalf("write credentials: %v", err)
	}
	return g
}

func (g *Google) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/token" || r.ParseForm() != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	g.mu.Lock()
	g.assertions = append(g.assertions, r.Form.Get("assertion"))
	g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"access_token":%q,"token_type":"Bearer","expires_in":3600}`, g.AccessToken)
}

// Assertions returns the JWT assertions received by the token endpoint.
func (g *Google) Assertions() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.assertions...)
}

// Entra is a fake Microsoft Entra ID tenant served over TLS.
type Entra struct {
	*httptest.Server
	Tenant      string
	AccessToken string

	mu       sync.Mutex
	requests []url.Values
}

// NewEntra starts a fake tenant. Clients must use e.Client() as transport (self-signed TLS)
// and disable instance discovery.
func NewEntra(t testing.TB) *Entra {
	t.Helper()
	e := &Entra{Tenant: "00000000-0000-0000-0000-00000000ffff", AccessToken: "fake-aks-access-token"}
	e.Server = httptest.NewTLSServer(http.HandlerFunc(e.serve))
	t.Cleanup(e.Close)
	return e
}

func (e *Entra) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	base := e.URL + "/" + e.Tenant
	switch {
	case strings.HasSuffix(r.URL.Path, "/.well-known/openid-configuration"):
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token_endpoint":                        base + "/oauth2/v2.0/token",
			"authorization_endpoint":                base + "/oauth2/v2.0/authorize",
			"issuer":                                base + "/v2.0",
			"device_authorization_endpoint":         base + "/oauth2/v2.0/devicecode",
			"end_session_endpoint":                  base + "/oauth2/v2.0/logout",
			"jwks_uri":                              base + "/discovery/v2.0/keys",
			"response_types_supported":              []string{"code", "id_token", "code id_token", "id_token token"},
			"scopes_supported":                      []string{"openid", "profile", "email", "offline_access"},
			"subject_types_supported":               []string{"pairwise"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	case strings.HasSuffix(r.URL.Path, "/oauth2/v2.0/token"):
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		e.mu.Lock()
		e.requests = append(e.requests, r.PostForm)
		e.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token_type":     "Bearer",
			"expires_in":     3600,
			"ext_expires_in": 3600,
			"access_token":   e.AccessToken,
		})
	default:
		http.NotFound(w, r)
	}
}

// TokenRequests returns the token request forms received so far.
func (e *Entra) TokenRequests() []url.Values {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]url.Values(nil), e.requests...)
}
