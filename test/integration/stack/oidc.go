package stack

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	krand "k8s.io/apimachinery/pkg/util/rand"
)

// OIDCUser is the identity the fake provider authenticates on its next authorization
// request.
type OIDCUser struct {
	Subject, Email, Name string
	Groups               []string
}

// AuthorizationRequest is what the fake provider received at its authorization endpoint.
type AuthorizationRequest struct {
	ClientID, RedirectURI, Scope, State, Nonce string
	CodeChallenge, CodeChallengeMethod         string
}

// OIDCProvider is an in-process OpenID Connect provider for the authorization-code flow
// with PKCE (S256): discovery, JWKS, authorization (auto-approving the configured user),
// token (client secret and code verifier checked, codes single-use) and userinfo
// endpoints; RS256 ID tokens with email, name and groups claims. It is a stand-in for an
// external identity provider (simulated).
type OIDCProvider struct {
	// URL is the issuer URL.
	URL string
	// ClientID and ClientSecret are the registered confidential client.
	ClientID, ClientSecret string

	key *rsa.PrivateKey
	kid string

	mu       sync.Mutex
	user     OIDCUser
	codes    map[string]grant
	requests []AuthorizationRequest
	// verified counts token requests whose PKCE verifier matched; rejected counts refused
	// token requests.
	verified, rejected int
}

type grant struct {
	req  AuthorizationRequest
	user OIDCUser
}

// NewOIDCProvider starts a fake provider for one client.
func NewOIDCProvider(t testing.TB) *OIDCProvider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &OIDCProvider{ClientID: "patrol", ClientSecret: "idp-secret-" + krand.String(12), key: key, kid: krand.String(8), codes: map[string]grant{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", p.discovery)
	mux.HandleFunc("GET /keys", p.jwks)
	mux.HandleFunc("GET /auth", p.authorize)
	mux.HandleFunc("POST /token", p.token)
	mux.HandleFunc("GET /userinfo", p.userinfo)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	p.URL = srv.URL
	return p
}

// SetUser sets the identity of the next authorizations.
func (p *OIDCProvider) SetUser(u OIDCUser) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.user = u
}

// Requests returns the authorization requests received so far.
func (p *OIDCProvider) Requests() []AuthorizationRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]AuthorizationRequest(nil), p.requests...)
}

// PKCE returns how many token requests presented a matching code verifier and how many
// token requests were refused.
func (p *OIDCProvider) PKCE() (verified, rejected int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.verified, p.rejected
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (p *OIDCProvider) discovery(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer": p.URL, "authorization_endpoint": p.URL + "/auth", "token_endpoint": p.URL + "/token",
		"jwks_uri": p.URL + "/keys", "userinfo_endpoint": p.URL + "/userinfo",
		"response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"}, "code_challenge_methods_supported": []string{"S256"},
		"scopes_supported": []string{"openid", "profile", "email", "groups"}, "claims_supported": []string{"sub", "email", "name", "groups"},
	})
}

func (p *OIDCProvider) jwks(w http.ResponseWriter, _ *http.Request) {
	pub := p.key.PublicKey
	writeJSON(w, http.StatusOK, map[string]any{"keys": []any{map[string]any{
		"kty": "RSA", "alg": "RS256", "use": "sig", "kid": p.kid,
		"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}}})
}

func (p *OIDCProvider) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	req := AuthorizationRequest{
		ClientID: q.Get("client_id"), RedirectURI: q.Get("redirect_uri"), Scope: q.Get("scope"), State: q.Get("state"),
		Nonce: q.Get("nonce"), CodeChallenge: q.Get("code_challenge"), CodeChallengeMethod: q.Get("code_challenge_method"),
	}
	p.mu.Lock()
	p.requests = append(p.requests, req)
	user := p.user
	p.mu.Unlock()
	if req.ClientID != p.ClientID || q.Get("response_type") != "code" || req.RedirectURI == "" {
		http.Error(w, "invalid_request", http.StatusBadRequest)
		return
	}
	code := krand.String(24)
	p.mu.Lock()
	p.codes[code] = grant{req: req, user: user}
	p.mu.Unlock()
	u, err := url.Parse(req.RedirectURI)
	if err != nil {
		http.Error(w, "invalid redirect_uri", http.StatusBadRequest)
		return
	}
	v := u.Query()
	v.Set("code", code)
	v.Set("state", req.State)
	u.RawQuery = v.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func (p *OIDCProvider) refuse(w http.ResponseWriter, msg string) {
	p.mu.Lock()
	p.rejected++
	p.mu.Unlock()
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant", "error_description": msg})
}

func (p *OIDCProvider) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		p.refuse(w, "bad form")
		return
	}
	id, secret, ok := r.BasicAuth()
	if !ok {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if id != p.ClientID || subtle.ConstantTimeCompare([]byte(secret), []byte(p.ClientSecret)) != 1 {
		p.refuse(w, "client authentication failed")
		return
	}
	code := r.PostForm.Get("code")
	p.mu.Lock()
	g, found := p.codes[code]
	delete(p.codes, code) // single use
	p.mu.Unlock()
	if !found || r.PostForm.Get("grant_type") != "authorization_code" || r.PostForm.Get("redirect_uri") != g.req.RedirectURI {
		p.refuse(w, "unknown or reused code")
		return
	}
	if g.req.CodeChallenge != "" {
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if g.req.CodeChallengeMethod != "S256" || base64.RawURLEncoding.EncodeToString(sum[:]) != g.req.CodeChallenge {
			p.refuse(w, "PKCE verification failed")
			return
		}
		p.mu.Lock()
		p.verified++
		p.mu.Unlock()
	}
	idToken, err := p.MintIDToken(p.ClientID, g.user, g.req.Nonce, time.Hour)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"access_token": "at-" + krand.String(16), "token_type": "Bearer", "expires_in": 3600, "id_token": idToken})
}

func (p *OIDCProvider) userinfo(w http.ResponseWriter, _ *http.Request) {
	p.mu.Lock()
	u := p.user
	p.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"sub": u.Subject, "email": u.Email, "name": u.Name, "groups": u.Groups})
}

// MintIDToken signs an ID token for audience with the provider key (also for the CLI
// exchange of an ID token obtained out of band).
func (p *OIDCProvider) MintIDToken(audience string, u OIDCUser, nonce string, ttl time.Duration) (string, error) {
	now := time.Now()
	claims := jwt.MapClaims{
		"iss": p.URL, "sub": u.Subject, "aud": audience, "iat": now.Unix(), "exp": now.Add(ttl).Unix(),
		"email": u.Email, "email_verified": true, "name": u.Name, "groups": u.Groups,
	}
	if nonce != "" {
		claims["nonce"] = nonce
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = p.kid
	return tok.SignedString(p.key)
}
