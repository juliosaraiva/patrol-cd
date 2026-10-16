package core

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/auth"
	authoidc "github.com/juliosaraiva/patrol-cd/internal/auth/oidc"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// SSO defaults.
const (
	// DefaultCallbackPath is the OIDC redirect path appended to the external URL.
	DefaultCallbackPath = apiclient.PathAuthCallback
	// DefaultExchangeMaxAge bounds the age (now - iat) of ID tokens accepted by POST
	// /session/oidc-exchange; the CLI exchanges a token it just obtained.
	DefaultExchangeMaxAge = 5 * time.Minute
	// SSOTokenMaxAgeForPasswordChange is how recent an SSO session must be to change a
	// local password (upstream ChangePasswordSSOTokenMaxAge).
	SSOTokenMaxAgeForPasswordChange = 5 * time.Minute
	// ssoTimeout bounds IdP calls made while serving one request.
	ssoTimeout = 30 * time.Second
	// maxDexErrorBody bounds the Dex error body logged by the proxy.
	maxDexErrorBody = 4 << 10
)

// SSOConfig configures SSO.
type SSOConfig struct {
	// HTTPClient is used for calls to the identity provider (tests, proxies). Nil builds
	// one from oidc.config (rootCA, oidc.tls.insecure.skip.verify).
	HTTPClient *http.Client
	// DexServerURL is the bundled Dex service ("https://patrol-dex-server:5556"); requests
	// to /api/dex are reverse-proxied to it and server-side calls to the Dex issuer are
	// sent to it directly. Empty disables the Dex proxy.
	DexServerURL string
	// DexTransport is the transport towards Dex (TLS settings: plaintext, CA pinning or
	// verification skipped). Nil uses http.DefaultTransport.
	DexTransport http.RoundTripper
	Logger       *slog.Logger
	Now          func() time.Time
}

// SSO runs the OIDC relying party for patrol-server. The provider is rebuilt when the
// SSO settings change; discovery is lazy and retried by internal/auth/oidc.
type SSO struct {
	cfg    SSOConfig
	dexURL *url.URL

	mu       sync.Mutex
	provKey  string
	prov     *authoidc.Provider
	codecKey string
	codec    *authoidc.StateCodec
}

// NewSSO returns an SSO helper.
func NewSSO(cfg SSOConfig) (*SSO, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	s := &SSO{cfg: cfg}
	if cfg.DexServerURL != "" {
		u, err := url.Parse(cfg.DexServerURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("invalid Dex server URL %q", cfg.DexServerURL)
		}
		s.dexURL = u
	}
	return s, nil
}

var errSSONotConfigured = errors.New("SSO is not configured")

// provider returns the relying party for the current settings.
func (s *SSO) provider(st *settings.Settings) (*authoidc.Provider, error) {
	cfg, ok, err := st.OIDCProviderConfig()
	if err != nil {
		return nil, rest.Unavailable("the SSO configuration is invalid").WithCause(err)
	}
	if !ok {
		return nil, rest.NotFound("SSO is not configured").WithCause(errSSONotConfigured)
	}
	dex := st.OIDCConfig() == nil && st.IsDexConfigured()
	key, err := configKey(cfg, dex)
	if err != nil {
		return nil, rest.Internal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.prov != nil && s.provKey == key {
		return s.prov, nil
	}
	opts := []authoidc.Option{authoidc.WithLogger(s.cfg.Logger), authoidc.WithClock(s.cfg.Now)}
	switch {
	case dex && s.dexURL != nil:
		issuer, err := url.Parse(cfg.Issuer)
		if err != nil {
			return nil, rest.Unavailable("the Dex issuer URL is invalid").WithCause(err)
		}
		opts = append(opts, authoidc.WithHTTPClient(&http.Client{Transport: &dexRewrite{issuer: issuer, dex: s.dexURL, next: s.dexTransport()}}))
	case s.cfg.HTTPClient != nil:
		opts = append(opts, authoidc.WithHTTPClient(s.cfg.HTTPClient))
	}
	p, err := authoidc.NewProvider(cfg, opts...)
	if err != nil {
		return nil, rest.Unavailable("the SSO configuration is invalid").WithCause(err)
	}
	s.prov, s.provKey = p, key
	return p, nil
}

// configKey identifies a provider configuration without retaining its secret.
func configKey(cfg authoidc.Config, dex bool) (string, error) {
	b, err := json.Marshal(struct {
		C   authoidc.Config
		Dex bool
	}{cfg, dex})
	if err != nil {
		return "", fmt.Errorf("encode SSO configuration: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// stateCodec returns the login state codec keyed by server.secretkey.
func (s *SSO) stateCodec(st *settings.Settings) (*authoidc.StateCodec, error) {
	key := st.ServerSecretKey()
	if key.IsZero() {
		return nil, rest.Unavailable("the server secret key is not configured")
	}
	sum := sha256.Sum256(key.Bytes())
	k := hex.EncodeToString(sum[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.codec != nil && s.codecKey == k {
		return s.codec, nil
	}
	c, err := authoidc.NewStateCodec(s.cfg.Now, key.Bytes())
	if err != nil {
		return nil, rest.Internal(err)
	}
	s.codec, s.codecKey = c, k
	return c, nil
}

// DexTransport returns the transport towards the bundled Dex. With strict set (upstream
// --dex-server-strict-tls) Dex's certificate must verify against roots (its CA bundle);
// otherwise certificate verification is skipped, as upstream does for the in-cluster
// self-signed Dex. Plain-HTTP Dex (--dex-server-plaintext) needs no TLS configuration:
// use an http:// DexServerURL.
func DexTransport(strict bool, roots *x509.CertPool) (http.RoundTripper, error) {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("unexpected default transport")
	}
	tr := base.Clone()
	tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	if strict {
		if roots == nil {
			return nil, errors.New("strict Dex TLS needs the Dex CA certificates")
		}
		tr.TLSClientConfig.RootCAs = roots
	} else {
		tr.TLSClientConfig.InsecureSkipVerify = true //nolint:gosec // upstream default for the in-cluster Dex; strict mode verifies
	}
	return tr, nil
}

func (s *SSO) dexTransport() http.RoundTripper {
	if s.cfg.DexTransport != nil {
		return s.cfg.DexTransport
	}
	return http.DefaultTransport
}

// dexRewrite sends server-side requests for the Dex issuer (the external URL) to the
// internal Dex service.
type dexRewrite struct {
	issuer *url.URL
	dex    *url.URL
	next   http.RoundTripper
}

func (d *dexRewrite) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.EqualFold(r.URL.Host, d.issuer.Host) && strings.HasPrefix(r.URL.Path, strings.TrimRight(d.issuer.Path, "/")) {
		r = r.Clone(r.Context())
		r.URL.Scheme = d.dex.Scheme
		r.URL.Host = d.dex.Host
		r.Host = d.issuer.Host
	}
	return d.next.RoundTrip(r)
}

// ExternalVerifier returns an auth.ExternalVerifier for ID tokens of the configured
// identity provider presented directly as bearer tokens (upstream behavior; the CLI
// normally exchanges its ID token for a Patrol session instead). The provider follows
// the settings of src; without SSO every external token is invalid.
func (s *SSO) ExternalVerifier(src SettingsSource) auth.ExternalVerifier {
	return externalVerifierFunc(func(ctx context.Context, raw string) (*auth.Principal, error) {
		st := src.Current()
		if st == nil || !st.IsSSOConfigured() {
			return nil, fmt.Errorf("%w: SSO is not configured", auth.ErrInvalidToken)
		}
		p, err := s.provider(st)
		if err != nil {
			return nil, fmt.Errorf("SSO provider: %w", err)
		}
		return p.VerifyExternal(ctx, raw)
	})
}

type externalVerifierFunc func(ctx context.Context, raw string) (*auth.Principal, error)

func (f externalVerifierFunc) VerifyExternal(ctx context.Context, raw string) (*auth.Principal, error) {
	return f(ctx, raw)
}

// requireSSO returns the SSO helper and settings, or the error to return.
func (h *Handlers) requireSSO() (*SSO, *settings.Settings, error) {
	s, err := h.settings()
	if err != nil {
		return nil, nil, err
	}
	if !s.IsSSOConfigured() {
		return nil, nil, rest.NotFound("SSO is not configured")
	}
	if h.d.SSO == nil {
		return nil, nil, rest.Unavailable("SSO is not available on this server").WithCause(errNotConfigured)
	}
	return h.d.SSO, s, nil
}

// allowedBaseURLs returns url plus additionalUrls.
func allowedBaseURLs(s *settings.Settings) []string {
	var out []string
	if u := s.URL(); u != "" {
		out = append(out, u)
	}
	return append(out, s.AdditionalURLs()...)
}

// startSSOLogin serves GET /api/v1/auth/login: it validates ?return_url= against url and
// additionalUrls, stores state, nonce and PKCE verifier in the encrypted state cookie,
// and redirects (303) to the identity provider.
func (h *Handlers) startSSOLogin(w http.ResponseWriter, r *http.Request) {
	sso, s, err := h.requireSSO()
	if err != nil {
		rest.WriteError(w, r, err)
		return
	}
	base := h.externalBaseURL(r, s)
	if base == "" {
		rest.WriteError(w, r, rest.FailedPrecondition("the external URL (url) must be configured for SSO"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), ssoTimeout)
	defer cancel()
	p, err := sso.provider(s)
	if err != nil {
		rest.WriteError(w, r, err)
		return
	}
	codec, err := sso.stateCodec(s)
	if err != nil {
		rest.WriteError(w, r, err)
		return
	}
	returnURL := r.URL.Query().Get("return_url")
	authURL, state, err := p.StartLogin(ctx, codec, authoidc.LoginRequest{
		RedirectURL:           base + h.d.Config.CallbackPath,
		ReturnURL:             returnURL,
		AllowedReturnBaseURLs: allowedBaseURLs(s),
	})
	if err != nil {
		rest.WriteError(w, r, ssoError(err))
		return
	}
	http.SetCookie(w, authoidc.StateCookie(state, h.cookiePath(), h.cookieOptions(r, s).Secure))
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, authURL, http.StatusSeeOther)
}

// ssoCallback serves GET /api/v1/auth/callback: it completes the authorization-code
// flow, mints a Patrol SSO session (ADR-0009), sets the session cookie and redirects to
// the validated return URL. The state cookie is cleared whatever the outcome.
func (h *Handlers) ssoCallback(w http.ResponseWriter, r *http.Request) {
	sso, s, err := h.requireSSO()
	if err != nil {
		rest.WriteError(w, r, err)
		return
	}
	http.SetCookie(w, authoidc.ClearStateCookie(h.cookiePath(), h.cookieOptions(r, s).Secure))
	w.Header().Set("Cache-Control", "no-store")
	ctx, cancel := context.WithTimeout(r.Context(), ssoTimeout)
	defer cancel()
	audit := rest.AuditFrom(ctx)
	p, err := sso.provider(s)
	if err != nil {
		rest.WriteError(w, r, err)
		return
	}
	codec, err := sso.stateCodec(s)
	if err != nil {
		rest.WriteError(w, r, err)
		return
	}
	res, err := p.HandleCallback(ctx, authoidc.CallbackFromRequest(r), authoidc.StateCookieValue(r), codec)
	if err != nil {
		h.d.Metrics.Login(loginSSO, loginFailure)
		audit.SetReason("SSO login failed")
		h.log().LogAttrs(ctx, slog.LevelWarn, "SSO login failed",
			slog.String("requestId", rest.RequestID(ctx)), slog.Any("error", err))
		rest.WriteError(w, r, ssoError(err))
		return
	}
	token, subject, err := h.issueSSOSession(ctx, s, res.Claims)
	if err != nil {
		h.d.Metrics.Login(loginSSO, loginFailure)
		rest.WriteError(w, r, err)
		return
	}
	if err := auth.SetSessionCookie(w, r, token, h.cookieOptions(r, s)); err != nil {
		rest.WriteError(w, r, rest.InvalidArgument("%s", err.Error()).WithCause(err))
		return
	}
	h.d.Metrics.Login(loginSSO, loginSuccess)
	audit.SetActor(subject)
	audit.SetMessage("%s logged in with SSO", subject)
	target := res.ReturnURL
	if target == "" {
		target = h.d.Config.BaseHRef
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// issueSSOSession mints a Patrol session for verified IdP claims; the claims named by the
// RBAC scopes travel in the session so group evaluation keeps working.
func (h *Handlers) issueSSOSession(ctx context.Context, s *settings.Settings, c authoidc.Claims) (string, string, error) {
	if h.d.Tokens == nil {
		return "", "", rest.Unavailable("sessions are not available").WithCause(errNotConfigured)
	}
	ident := c.SSOIdentity(scopes(s)...)
	if ident.Subject == "" {
		return "", "", rest.Unauthenticated("the ID token has no subject")
	}
	iss, err := h.d.Tokens.IssueSSOSession(ctx, ident, s.UserSessionDuration())
	if err != nil {
		return "", "", rest.Internal(err)
	}
	h.log().LogAttrs(ctx, slog.LevelInfo, "user logged in",
		slog.String("requestId", rest.RequestID(ctx)), slog.String("user", ident.Subject), slog.String("method", loginSSO))
	return iss.Token, ident.Subject, nil
}

// exchangeOIDCToken serves POST /api/v1/session/oidc-exchange (CLI SSO): an ID token the
// CLI obtained with the authorization-code + PKCE flow against the CLI client is verified
// (signature, issuer, audience, expiry, issued within ExchangeMaxAge) and exchanged for a
// Patrol SSO session. With enableUserInfoGroups the access token is required and groups
// come from the userinfo endpoint.
func (h *Handlers) exchangeOIDCToken(ctx context.Context, req *apiclient.OIDCExchangeRequest) (*apiclient.SessionResponse, error) {
	sso, s, err := h.requireSSO()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.IDToken) == "" {
		return nil, rest.InvalidArgument("idToken is required")
	}
	if len(req.IDToken) > auth.MaxTokenLength || len(req.AccessToken) > auth.MaxTokenLength {
		return nil, rest.InvalidArgument("token too large")
	}
	ctx, cancel := context.WithTimeout(ctx, ssoTimeout)
	defer cancel()
	p, err := sso.provider(s)
	if err != nil {
		return nil, err
	}
	idt, err := p.VerifyIDToken(ctx, req.IDToken)
	if err != nil {
		h.d.Metrics.Login(loginSSO, loginFailure)
		return nil, ssoError(err)
	}
	now := h.now()
	if idt.IssuedAt.IsZero() || now.Sub(idt.IssuedAt) > h.d.Config.ExchangeMaxAge {
		h.d.Metrics.Login(loginSSO, loginFailure)
		return nil, rest.Unauthenticated("the ID token is too old to exchange; log in again")
	}
	raw := map[string]any{}
	if err := idt.Claims(&raw); err != nil {
		return nil, rest.Unauthenticated("invalid ID token claims").WithCause(err)
	}
	cfg := p.Config()
	groups, err := authoidc.ExtractGroups(raw, cfg.GroupsClaim)
	if err != nil {
		return nil, rest.Unauthenticated("invalid group claims").WithCause(err)
	}
	if cfg.EnableUserInfoGroups {
		if req.AccessToken == "" {
			return nil, rest.InvalidArgument("accessToken is required when userinfo groups are enabled")
		}
		ui, err := p.FetchUserInfo(ctx, req.AccessToken, idt.Subject)
		if err != nil {
			return nil, ssoError(err)
		}
		if groups, err = authoidc.ExtractGroups(ui, cfg.GroupsClaim); err != nil {
			return nil, rest.Unauthenticated("invalid userinfo group claims").WithCause(err)
		}
		raw["groups"] = groups
	}
	claims := authoidc.Claims{Issuer: idt.Issuer, Sub: idt.Subject, Groups: groups, Expiry: idt.Expiry, Raw: raw}
	claims.Email, _ = raw["email"].(string)
	claims.Name, _ = raw["name"].(string)
	token, subject, err := h.issueSSOSession(ctx, s, claims)
	if err != nil {
		h.d.Metrics.Login(loginSSO, loginFailure)
		return nil, err
	}
	h.d.Metrics.Login(loginSSO, loginSuccess)
	audit := rest.AuditFrom(ctx)
	audit.SetActor(subject)
	audit.SetMessage("%s logged in with SSO (CLI)", subject)
	return &apiclient.SessionResponse{Token: token}, nil
}

// ssoError maps internal/auth/oidc errors: problems caused by the user or the IdP's
// answer are 4xx, IdP outages 502/503. Details never include tokens.
func ssoError(err error) error {
	var pe *authoidc.ProviderError
	switch {
	case errors.As(err, &pe):
		return rest.Unauthenticated("identity provider returned an error: %s", truncateText(pe.Code, 64)).WithCause(err)
	case errors.Is(err, auth.ErrInvalidRedirectURL):
		return rest.InvalidArgument("Invalid redirect URL: the protocol and host (including port) must match and the path must be within allowed URLs if provided").WithCause(err)
	case errors.Is(err, authoidc.ErrInvalidState), errors.Is(err, authoidc.ErrStateMismatch):
		return rest.InvalidArgument("invalid or expired login state; start the login again").WithCause(err)
	case errors.Is(err, authoidc.ErrMissingCode):
		return rest.InvalidArgument("authorization code missing from callback").WithCause(err)
	case errors.Is(err, authoidc.ErrNonceMismatch), errors.Is(err, authoidc.ErrIDTokenInvalid),
		errors.Is(err, authoidc.ErrAudience), errors.Is(err, authoidc.ErrTooManyGroups):
		return rest.Unauthenticated("ID token verification failed").WithCause(err)
	case errors.Is(err, authoidc.ErrUserInfoUnavailable):
		return rest.Unauthenticated("userinfo groups are required but not available").WithCause(err)
	case errors.Is(err, authoidc.ErrExchange), errors.Is(err, authoidc.ErrMissingIDToken),
		errors.Is(err, authoidc.ErrUserInfo):
		return &rest.Error{Status: http.StatusBadGateway, Code: apiclient.CodeUnavailable, Detail: "the identity provider request failed", Cause: err}
	case errors.Is(err, authoidc.ErrDiscovery):
		return rest.Unavailable("the identity provider is unavailable").WithCause(err)
	case errors.Is(err, authoidc.ErrInvalidConfig):
		return rest.Unavailable("the SSO configuration is invalid").WithCause(err)
	case errors.Is(err, context.DeadlineExceeded):
		return rest.Unavailable("the identity provider did not answer in time").WithCause(err)
	}
	return rest.Internal(err)
}

func truncateText(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// cookiePath is the path of the session and state cookies.
func (h *Handlers) cookiePath() string { return auth.CookiePath(h.d.Config.BaseHRef) }

// dexProxy serves /api/dex/... by reverse-proxying to the bundled Dex when dex.config is
// set. Request headers are forwarded as upstream does, including Authorization, which
// carries Dex's own credentials (client_secret_basic on /token, Dex access tokens on
// /userinfo). Only Patrol session credentials are removed: patrol.token cookies and
// Authorization values that are Patrol-issued bearer tokens (iss "patrol"), which can
// never be Dex credentials. A 500 from Dex becomes a redirect to the login page with
// has_sso_error=true (upstream), after logging a bounded excerpt of the body.
func (h *Handlers) dexProxy(w http.ResponseWriter, r *http.Request) {
	s, err := h.settings()
	if err != nil {
		rest.WriteError(w, r, err)
		return
	}
	if !s.IsDexConfigured() {
		rest.WriteError(w, r, rest.NotFound("Dex is not configured"))
		return
	}
	if h.d.SSO == nil || h.d.SSO.dexURL == nil {
		rest.WriteError(w, r, rest.Unavailable("the Dex server address is not configured").WithCause(errNotConfigured))
		return
	}
	ext, err := url.Parse(s.URL())
	if err != nil {
		rest.WriteError(w, r, rest.Unavailable("the external URL is invalid").WithCause(err))
		return
	}
	extPath := strings.TrimRight(ext.Path, "/")
	dex := h.d.SSO.dexURL
	loginURL := strings.TrimRight(h.d.Config.BaseHRef, "/") + "/login?has_sso_error=true"
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = dex.Scheme
			pr.Out.URL.Host = dex.Host
			pr.Out.URL.Path = extPath + pr.In.URL.Path
			pr.Out.URL.RawPath = ""
			pr.Out.Host = ext.Host
			stripPatrolBearer(pr.Out)
			stripSessionCookies(pr.Out)
			pr.SetXForwarded()
		},
		Transport: h.d.SSO.dexTransport(),
		ModifyResponse: func(resp *http.Response) error {
			if resp.StatusCode != http.StatusInternalServerError {
				return nil
			}
			body, _ := io.ReadAll(io.LimitReader(resp.Body, maxDexErrorBody))
			_ = resp.Body.Close()
			h.log().LogAttrs(resp.Request.Context(), slog.LevelError, "Dex returned an error",
				slog.String("requestId", rest.RequestID(r.Context())), slog.String("body", string(body)))
			resp.StatusCode = http.StatusSeeOther
			resp.Status = http.StatusText(http.StatusSeeOther)
			resp.Header = http.Header{"Location": {loginURL}, "Content-Length": {"0"}}
			resp.Body = http.NoBody
			resp.ContentLength = 0
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			h.log().LogAttrs(r.Context(), slog.LevelError, "Dex proxy failed",
				slog.String("requestId", rest.RequestID(r.Context())), slog.Any("error", err))
			rest.WriteError(w, r, &rest.Error{Status: http.StatusBadGateway, Code: apiclient.CodeUnavailable, Detail: "the Dex server is unavailable", Cause: err})
		},
	}
	proxy.ServeHTTP(w, r)
}

// stripPatrolBearer removes Authorization values that carry a Patrol-issued bearer token
// from an outgoing request; every other value (Basic client credentials, Dex-issued
// bearer tokens) is kept.
func stripPatrolBearer(r *http.Request) {
	vals := r.Header.Values("Authorization")
	if len(vals) == 0 {
		return
	}
	kept := make([]string, 0, len(vals))
	for _, v := range vals {
		if !isPatrolBearer(v) {
			kept = append(kept, v)
		}
	}
	if len(kept) == len(vals) {
		return
	}
	r.Header.Del("Authorization")
	for _, v := range kept {
		r.Header.Add("Authorization", v)
	}
}

// isPatrolBearer reports whether an Authorization value is "Bearer <jwt>" with the
// Patrol issuer. The signature is not checked: the claim only decides whether the value
// is a Patrol credential that must not leave the server, never whether it is trusted.
func isPatrolBearer(v string) bool {
	scheme, tok, ok := strings.Cut(strings.TrimSpace(v), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	tok = strings.TrimSpace(tok)
	if tok == "" || len(tok) > auth.MaxTokenLength || strings.Count(tok, ".") != 2 {
		return false
	}
	claims := jwt.MapClaims{}
	if _, _, err := jwt.NewParser().ParseUnverified(tok, claims); err != nil {
		return false
	}
	iss, _ := claims["iss"].(string)
	return iss == auth.Issuer
}

// stripSessionCookies removes patrol.token cookies from an outgoing request.
func stripSessionCookies(r *http.Request) {
	cookies := r.Cookies()
	r.Header.Del("Cookie")
	for _, c := range cookies {
		if c.Name == auth.SessionCookieName || strings.HasPrefix(c.Name, auth.SessionCookieName+".") {
			continue
		}
		r.AddCookie(c)
	}
}
