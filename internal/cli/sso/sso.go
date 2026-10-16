// Package sso implements the CLI side of single sign-on (TEN-130): the OAuth 2.0
// authorization-code flow with PKCE (S256) against the identity provider named by the
// server's settings (oidc.config, or the bundled Dex at <url>/api/dex), with a temporary
// loopback HTTP server receiving the redirect, and the refresh-token grant used to renew
// an expired session.
//
// The ID token obtained here is not a Patrol session: the caller exchanges it at
// POST /api/v1/session/oidc-exchange. Tokens are never logged.
package sso

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// Defaults of the login flow.
const (
	// DefaultPort is the default --sso-port (upstream DefaultSSOLocalPort).
	DefaultPort = 8085
	// CallbackPath is the redirect path served by the loopback server.
	CallbackPath = "/auth/callback"
	// DefaultTimeout bounds the wait for the browser redirect.
	DefaultTimeout = 5 * time.Minute
	// DexAPIPath is where patrol-server proxies the bundled Dex.
	DexAPIPath = "/api/dex"
	// DexCLIClientID is the public Dex client of the CLI.
	DexCLIClientID = "patrol-cli"
	// dexFederatedScope requests Dex's federated claims.
	dexFederatedScope = "federated:id"
	// verifierCharset is the RFC 7636 unreserved character set.
	verifierCharset = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"
	// maxCallbackRequests bounds the callback requests handled in one login.
	maxCallbackRequests = 2
)

// DefaultScopes are requested when the settings name none.
var DefaultScopes = []string{"openid", "profile", "email", "groups"}

// Provider is the identity provider configuration used by the CLI.
type Provider struct {
	Issuer   string
	ClientID string
	Scopes   []string
	// IDTokenClaims are requested with the "claims" authorization parameter.
	IDTokenClaims map[string]*apiclient.OIDCClaim
	// Dex reports the bundled Dex (reached through patrol-server).
	Dex bool
}

// ProviderFromSettings selects the provider from the server settings: oidc.config when it
// names an issuer (CLI client ID, else the client ID), else the bundled Dex below the
// settings url (or serverURL when the settings carry none).
func ProviderFromSettings(serverURL string, s *apiclient.Settings) (*Provider, error) {
	switch {
	case s != nil && s.OIDCConfig != nil && s.OIDCConfig.Issuer != "":
		id := s.OIDCConfig.CLIClientID
		if id == "" {
			id = s.OIDCConfig.ClientID
		}
		scopes := slices.Clone(s.OIDCConfig.Scopes)
		if len(scopes) == 0 {
			scopes = slices.Clone(DefaultScopes)
		}
		if !slices.Contains(scopes, oidc.ScopeOpenID) {
			scopes = append([]string{oidc.ScopeOpenID}, scopes...)
		}
		return &Provider{Issuer: s.OIDCConfig.Issuer, ClientID: id, Scopes: scopes, IDTokenClaims: s.OIDCConfig.IDTokenClaims}, nil
	case s != nil && s.DexConfig != nil && len(s.DexConfig.Connectors) > 0:
		base := strings.TrimRight(s.URL, "/")
		if base == "" {
			base = strings.TrimRight(serverURL, "/")
		}
		scopes := append(slices.Clone(DefaultScopes), dexFederatedScope)
		return &Provider{Issuer: base + DexAPIPath, ClientID: DexCLIClientID, Scopes: scopes, Dex: true}, nil
	}
	return nil, fmt.Errorf("%s is not configured with SSO", serverURL)
}

// Result holds the tokens returned by the identity provider.
type Result struct {
	IDToken      string
	AccessToken  string
	RefreshToken string
}

// String redacts the tokens.
func (r Result) String() string { return "sso.Result{[redacted]}" }

// LogValue redacts the tokens.
func (r Result) LogValue() slog.Value { return slog.StringValue("[redacted]") }

// LoginOptions configure Login.
type LoginOptions struct {
	// HTTPClient calls the identity provider (discovery, keys, token endpoint).
	HTTPClient *http.Client
	// Port is the loopback port of the redirect listener (0 picks a free port).
	Port int
	// Callback overrides the redirect base URL ("http://localhost:<port>" by default);
	// the listener still binds to localhost:<port>.
	Callback string
	// LaunchBrowser opens the authorization URL with OpenBrowser; the URL is always
	// printed, and opening failures fall back to copy-and-paste instructions.
	LaunchBrowser bool
	OpenBrowser   func(url string) error
	// Out receives the instructions.
	Out io.Writer
	// Timeout bounds the wait for the redirect (default DefaultTimeout).
	Timeout time.Duration
	Logger  *slog.Logger
}

func (o *LoginOptions) logger() *slog.Logger {
	if o.Logger != nil {
		return o.Logger
	}
	return slog.New(slog.DiscardHandler)
}

// discover fetches the provider metadata.
func discover(ctx context.Context, p *Provider, hc *http.Client) (*oidc.Provider, *oauth2.Config, []string, error) {
	if hc != nil {
		ctx = oidc.ClientContext(ctx, hc)
	}
	op, err := oidc.NewProvider(ctx, p.Issuer)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("discover the identity provider %s: %w", p.Issuer, err)
	}
	var meta struct {
		ResponseTypes []string `json:"response_types_supported"`
	}
	_ = op.Claims(&meta)
	conf := &oauth2.Config{ClientID: p.ClientID, Scopes: p.Scopes, Endpoint: op.Endpoint()}
	return op, conf, meta.ResponseTypes, nil
}

// Login runs the authorization-code flow with PKCE and returns the provider's tokens.
func Login(ctx context.Context, p *Provider, o LoginOptions) (*Result, error) {
	op, conf, responseTypes, err := discover(ctx, p, o.HTTPClient)
	if err != nil {
		return nil, err
	}
	if len(responseTypes) > 0 && !slices.ContainsFunc(responseTypes, func(t string) bool { return slices.Contains(strings.Fields(t), "code") }) {
		return nil, errors.New("the identity provider does not support the authorization code flow (the implicit flow is not supported by the Patrol CLI)")
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", net.JoinHostPort("localhost", strconv.Itoa(o.Port)))
	if err != nil {
		return nil, fmt.Errorf("start the local SSO callback listener on port %d: %w", o.Port, err)
	}
	defer func() { _ = ln.Close() }()
	base := strings.TrimRight(o.Callback, "/")
	if base == "" {
		base = "http://localhost:" + strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	}
	conf.RedirectURL = base + CallbackPath

	state, err := randomString(32)
	if err != nil {
		return nil, err
	}
	nonce, err := randomString(32)
	if err != nil {
		return nil, err
	}
	verifier, err := randomString(43)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	opts := []oauth2.AuthCodeOption{
		oauth2.AccessTypeOffline,
		oauth2.SetAuthURLParam("code_challenge", challenge),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
		oidc.Nonce(nonce),
	}
	if len(p.IDTokenClaims) > 0 {
		claims, err := json.Marshal(map[string]any{"id_token": p.IDTokenClaims})
		if err != nil {
			return nil, fmt.Errorf("encode requested claims: %w", err)
		}
		opts = append(opts, oauth2.SetAuthURLParam("claims", string(claims)))
	}
	authURL := conf.AuthCodeURL(state, opts...)

	tokenCtx := ctx
	if o.HTTPClient != nil {
		tokenCtx = context.WithValue(ctx, oauth2.HTTPClient, o.HTTPClient)
	}
	cb := &callback{state: state, done: make(chan callbackResult, 1)}
	cb.exchange = func(code string) (*Result, error) {
		tok, err := conf.Exchange(tokenCtx, code, oauth2.VerifierOption(verifier))
		if err != nil {
			return nil, fmt.Errorf("exchange the authorization code: %w", err)
		}
		res, err := resultFrom(tok)
		if err != nil {
			return nil, err
		}
		if err := verifyIDToken(tokenCtx, op, res.IDToken, nonce); err != nil {
			return nil, err
		}
		return res, nil
	}
	srv := &http.Server{Handler: cb, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()

	out := o.Out
	if out == nil {
		out = io.Discard
	}
	_, _ = fmt.Fprintf(out, "Performing authorization_code flow login: %s\n", authURL)
	opened := false
	if o.LaunchBrowser && o.OpenBrowser != nil {
		_, _ = fmt.Fprintln(out, "Opening system default browser for authentication")
		if err := o.OpenBrowser(authURL); err != nil {
			o.logger().Warn("cannot open a browser", slog.Any("error", err))
		} else {
			opened = true
		}
	}
	if !opened {
		_, _ = fmt.Fprintf(out, "To authenticate, copy-and-paste the following URL into your preferred browser: %s\n", authURL)
	}

	wait, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	select {
	case r := <-cb.done:
		if r.err != nil {
			return nil, r.err
		}
		_, _ = fmt.Fprintln(out, "Authentication successful")
		return r.res, nil
	case <-wait.Done():
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("timed out after %s waiting for the SSO login to complete", timeout)
	}
}

// Refresh redeems a refresh token at the provider (the session renewal of an SSO
// context). The new refresh token is the old one when the provider returns none.
func Refresh(ctx context.Context, p *Provider, hc *http.Client, refreshToken string) (*Result, error) {
	if refreshToken == "" {
		return nil, errors.New("no refresh token")
	}
	op, conf, _, err := discover(ctx, p, hc)
	if err != nil {
		return nil, err
	}
	if hc != nil {
		ctx = context.WithValue(ctx, oauth2.HTTPClient, hc)
	}
	tok, err := conf.TokenSource(ctx, &oauth2.Token{RefreshToken: refreshToken, Expiry: time.Unix(1, 0)}).Token()
	if err != nil {
		return nil, fmt.Errorf("refresh the SSO session: %w", err)
	}
	res, err := resultFrom(tok)
	if err != nil {
		return nil, err
	}
	if res.RefreshToken == "" {
		res.RefreshToken = refreshToken
	}
	if err := verifyIDToken(ctx, op, res.IDToken, ""); err != nil {
		return nil, err
	}
	return res, nil
}

func resultFrom(tok *oauth2.Token) (*Result, error) {
	id, _ := tok.Extra("id_token").(string)
	if id == "" {
		return nil, errors.New("no id_token in token response")
	}
	return &Result{IDToken: id, AccessToken: tok.AccessToken, RefreshToken: tok.RefreshToken}, nil
}

// verifyIDToken checks the signature, issuer and expiry of the ID token and, for a
// login, its nonce. The audience is checked by patrol-server at the exchange.
func verifyIDToken(ctx context.Context, op *oidc.Provider, raw, nonce string) error {
	tok, err := op.Verifier(&oidc.Config{SkipClientIDCheck: true}).Verify(ctx, raw)
	if err != nil {
		return fmt.Errorf("invalid ID token from the identity provider: %w", err)
	}
	if nonce != "" && tok.Nonce != nonce {
		return errors.New("invalid ID token from the identity provider: nonce mismatch")
	}
	return nil
}

type callbackResult struct {
	res *Result
	err error
}

// callback serves the redirect from the identity provider.
type callback struct {
	state    string
	exchange func(code string) (*Result, error)
	done     chan callbackResult

	mu       sync.Mutex
	handled  int
	finished bool
}

func (c *callback) finish(r callbackResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.finished {
		return
	}
	c.finished = true
	c.done <- r
}

func (c *callback) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != CallbackPath {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	q := r.URL.Query()
	fail := func(msg string) {
		http.Error(w, html.EscapeString(msg), http.StatusBadRequest)
		c.finish(callbackResult{err: errors.New(msg)})
	}
	c.mu.Lock()
	c.handled++
	n, finished := c.handled, c.finished
	c.mu.Unlock()
	if finished {
		http.Error(w, "the login flow already completed", http.StatusBadRequest)
		return
	}
	if e := q.Get("error"); e != "" {
		fail(fmt.Sprintf("%s: %s", e, q.Get("error_description")))
		return
	}
	if n > maxCallbackRequests {
		fail("Unable to complete login flow: too many redirects")
		return
	}
	if q.Get("state") != c.state {
		fail("Unknown state nonce")
		return
	}
	code := q.Get("code")
	if code == "" {
		keys := make([]string, 0, len(q))
		for k := range q {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		fail(fmt.Sprintf("no code in request: parameters %q", keys))
		return
	}
	res, err := c.exchange(code)
	if err != nil {
		fail(err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, successPage)
	c.finish(callbackResult{res: res})
}

const successPage = `<!doctype html><html><head><meta charset="utf-8"><title>Patrol CD</title></head>
<body style="font-family: sans-serif; margin: 3em"><h1>Authentication successful</h1>
<p>You can close this window and return to the CLI.</p></body></html>`

// randomString returns n characters of the RFC 7636 unreserved set from crypto/rand.
func randomString(n int) (string, error) {
	var b strings.Builder
	limit := big.NewInt(int64(len(verifierCharset)))
	for range n {
		i, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return "", fmt.Errorf("generate random value: %w", err)
		}
		b.WriteByte(verifierCharset[i.Int64()])
	}
	return b.String(), nil
}

// SameHost reports whether rawURL designates host (the issuer is served by the Patrol
// server, e.g. the bundled Dex), so that the server's TLS settings apply to it.
func SameHost(rawURL, host string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, host)
}
