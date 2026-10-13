package oidc

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/sync/singleflight"

	"github.com/juliosaraiva/patrol-cd/internal/auth"
)

const (
	// maxIdPResponseBytes bounds every response body read from the IdP (discovery,
	// JWKS, token, userinfo).
	maxIdPResponseBytes = 1 << 20

	defaultHTTPTimeout        = 30 * time.Second
	defaultDiscoveryAttempts  = 3
	defaultDiscoveryBackoff   = 500 * time.Millisecond
	defaultDiscoveryTimeout   = 60 * time.Second
	defaultFailureCooldown    = 5 * time.Second
	defaultRediscoverInterval = time.Minute
	defaultUserInfoCacheSize  = 10000
)

// Provider is a relying-party view of one OIDC issuer. Discovery is lazy (the bundled
// Dex may be served by the same process that consumes it) and retried with backoff;
// failures are cached briefly so a down IdP is not hammered. It is safe for concurrent
// use.
type Provider struct {
	cfg    Config
	client *http.Client
	logger *slog.Logger
	now    func() time.Time

	discoveryAttempts  int
	discoveryBackoff   time.Duration
	discoveryTimeout   time.Duration
	failureCooldown    time.Duration
	rediscoverInterval time.Duration

	mu             sync.Mutex
	prov           *gooidc.Provider
	lastErr        error
	lastFailure    time.Time
	lastRediscover time.Time
	sf             singleflight.Group

	userInfo *userInfoCache
}

// Option customizes a Provider.
type Option func(*Provider)

// WithHTTPClient uses c (its transport is wrapped with the response size limit) instead
// of a client built from Config.RootCA / InsecureSkipVerify.
func WithHTTPClient(c *http.Client) Option {
	return func(p *Provider) {
		cp := *c
		base := cp.Transport
		if base == nil {
			base = http.DefaultTransport
		}
		cp.Transport = &limitedTransport{base: base, max: maxIdPResponseBytes}
		p.client = &cp
	}
}

// WithLogger sets the logger (default slog.Default()).
func WithLogger(l *slog.Logger) Option { return func(p *Provider) { p.logger = l } }

// WithClock sets the time source used for token and state validation.
func WithClock(now func() time.Time) Option { return func(p *Provider) { p.now = now } }

// WithDiscoveryRetry sets the number of discovery attempts per call and the initial
// backoff between them (doubled after each failure).
func WithDiscoveryRetry(attempts int, backoff time.Duration) Option {
	return func(p *Provider) {
		p.discoveryAttempts = max(1, attempts)
		p.discoveryBackoff = max(0, backoff)
	}
}

// WithFailureCooldown sets how long a discovery failure is returned from cache before a
// new attempt is made.
func WithFailureCooldown(d time.Duration) Option {
	return func(p *Provider) { p.failureCooldown = max(0, d) }
}

// NewProvider validates cfg and returns a Provider. No network I/O happens until the
// provider is first used.
func NewProvider(cfg Config, opts ...Option) (*Provider, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	p := &Provider{
		cfg:                cfg,
		logger:             slog.Default(),
		now:                time.Now,
		discoveryAttempts:  defaultDiscoveryAttempts,
		discoveryBackoff:   defaultDiscoveryBackoff,
		discoveryTimeout:   defaultDiscoveryTimeout,
		failureCooldown:    defaultFailureCooldown,
		rediscoverInterval: defaultRediscoverInterval,
	}
	for _, o := range opts {
		o(p)
	}
	if p.client == nil {
		c, err := newHTTPClient(cfg)
		if err != nil {
			return nil, err
		}
		p.client = c
	}
	p.userInfo = newUserInfoCache(defaultUserInfoCacheSize, p.now)
	return p, nil
}

// Config returns the provider configuration.
func (p *Provider) Config() Config { return p.cfg }

func newHTTPClient(cfg Config) (*http.Client, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.RootCA != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(cfg.RootCA)) {
			return nil, fmt.Errorf("%w: rootCA contains no valid PEM certificates", ErrInvalidConfig)
		}
		tlsCfg.RootCAs = pool
	}
	if cfg.InsecureSkipVerify {
		tlsCfg.InsecureSkipVerify = true //nolint:gosec // explicit operator opt-in
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tlsCfg
	return &http.Client{
		Transport: &limitedTransport{base: tr, max: maxIdPResponseBytes},
		Timeout:   defaultHTTPTimeout,
	}, nil
}

// provider returns the discovered go-oidc provider, discovering it if needed.
func (p *Provider) provider(ctx context.Context) (*gooidc.Provider, error) {
	p.mu.Lock()
	if p.prov != nil {
		prov := p.prov
		p.mu.Unlock()
		return prov, nil
	}
	if p.lastErr != nil && p.now().Sub(p.lastFailure) < p.failureCooldown {
		err := p.lastErr
		p.mu.Unlock()
		return nil, err
	}
	p.mu.Unlock()
	return p.discover(ctx)
}

// discover runs (or joins) a discovery. The discovery itself runs on a context detached
// from ctx's cancellation so that one canceled caller does not fail the others.
func (p *Provider) discover(ctx context.Context) (*gooidc.Provider, error) {
	ch := p.sf.DoChan("discover", func() (any, error) {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.discoveryTimeout)
		defer cancel()
		prov, err := p.discoverWithRetry(dctx)
		p.mu.Lock()
		defer p.mu.Unlock()
		if err != nil {
			// A failed re-discovery keeps the working provider (and its cached keys).
			if p.prov == nil {
				p.lastErr, p.lastFailure = err, p.now()
			}
			return nil, err
		}
		p.prov, p.lastErr = prov, nil
		return prov, nil
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		if r.Err != nil {
			return nil, r.Err
		}
		return r.Val.(*gooidc.Provider), nil
	}
}

func (p *Provider) discoverWithRetry(ctx context.Context) (*gooidc.Provider, error) {
	backoff := p.discoveryBackoff
	var lastErr error
	for attempt := 1; ; attempt++ {
		prov, err := gooidc.NewProvider(gooidc.ClientContext(ctx, p.client), p.cfg.Issuer)
		if err == nil {
			p.logger.Info("OIDC provider discovered", "issuer", p.cfg.Issuer, "attempt", attempt)
			return prov, nil
		}
		lastErr = err
		p.logger.Warn("OIDC provider discovery failed", "issuer", p.cfg.Issuer, "attempt", attempt, "error", err)
		if attempt >= p.discoveryAttempts {
			break
		}
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, fmt.Errorf("%w (issuer %q): %w", ErrDiscovery, p.cfg.Issuer, ctx.Err())
		case <-t.C:
		}
		backoff *= 2
	}
	return nil, fmt.Errorf("%w (issuer %q): %w", ErrDiscovery, p.cfg.Issuer, lastErr)
}

// forceRediscover discovers again, at most once per rediscoverInterval, and swaps in the
// new provider only on success. This handles IdPs (e.g. Dex without persistent storage)
// whose discovery document or keys change across restarts.
func (p *Provider) forceRediscover(ctx context.Context) (*gooidc.Provider, bool) {
	p.mu.Lock()
	if !p.lastRediscover.IsZero() && p.now().Sub(p.lastRediscover) < p.rediscoverInterval {
		p.mu.Unlock()
		return nil, false
	}
	p.lastRediscover = p.now()
	p.mu.Unlock()
	prov, err := p.discover(ctx)
	if err != nil {
		return nil, false
	}
	return prov, true
}

// VerifyIDToken verifies an ID token's signature (algorithms advertised by the IdP,
// RS256 by default; never HMAC), issuer, expiry and audience. A token with an "aud"
// claim must name one of Config.AllowedAudiences; a token without "aud" is rejected
// unless SkipAudienceCheckWhenTokenHasNoAudience is set. Expiry failures keep the
// *gooidc.TokenExpiredError in the chain. The nonce is not checked here.
func (p *Provider) VerifyIDToken(ctx context.Context, raw string) (*gooidc.IDToken, error) {
	return p.verifySigned(ctx, raw, false)
}

func (p *Provider) verifySigned(ctx context.Context, raw string, skipExpiry bool) (*gooidc.IDToken, error) {
	if raw == "" || len(raw) > auth.MaxTokenLength {
		return nil, fmt.Errorf("%w: empty or oversized token", ErrIDTokenInvalid)
	}
	prov, err := p.provider(ctx)
	if err != nil {
		return nil, err
	}
	cfg := &gooidc.Config{SkipClientIDCheck: true, SkipExpiryCheck: skipExpiry, Now: p.now}
	idt, err := prov.Verifier(cfg).Verify(ctx, raw)
	if err != nil && strings.Contains(err.Error(), "failed to verify signature") {
		if prov2, ok := p.forceRediscover(ctx); ok {
			idt, err = prov2.Verifier(cfg).Verify(ctx, raw)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrIDTokenInvalid, err)
	}
	if len(idt.Audience) == 0 {
		if !p.cfg.SkipAudienceCheckWhenTokenHasNoAudience {
			return nil, fmt.Errorf("%w: %w: token has no aud claim", ErrIDTokenInvalid, ErrAudience)
		}
		return idt, nil
	}
	allowed := p.cfg.AllowedAudiences()
	if !slices.ContainsFunc(idt.Audience, func(a string) bool { return slices.Contains(allowed, a) }) {
		return nil, fmt.Errorf("%w: %w", ErrIDTokenInvalid, ErrAudience)
	}
	return idt, nil
}

// VerifyExternal implements auth.ExternalVerifier for IdP-issued ID tokens presented as
// bearer tokens (e.g. by the CLI). When EnableUserInfoGroups is set, groups come from the
// userinfo cache populated at login; without a cache entry the token is rejected (the
// access token needed to query userinfo is not available here).
func (p *Provider) VerifyExternal(ctx context.Context, raw string) (*auth.Principal, error) {
	idt, err := p.VerifyIDToken(ctx, raw)
	if err != nil {
		if _, ok := errors.AsType[*gooidc.TokenExpiredError](err); ok {
			return nil, fmt.Errorf("%w: %w: %w", auth.ErrInvalidToken, auth.ErrTokenExpired, err)
		}
		if errors.Is(err, ErrIDTokenInvalid) {
			return nil, fmt.Errorf("%w: %w", auth.ErrInvalidToken, err)
		}
		return nil, err
	}
	rawClaims := map[string]any{}
	if err := idt.Claims(&rawClaims); err != nil {
		return nil, fmt.Errorf("%w: %w", auth.ErrInvalidToken, err)
	}
	claims, err := claimsFromRaw(rawClaims, p.cfg.groupsClaim())
	if err != nil {
		return nil, fmt.Errorf("%w: %w", auth.ErrInvalidToken, err)
	}
	if p.cfg.EnableUserInfoGroups {
		groups, ok := p.userInfo.get(claims.Sub)
		if !ok {
			return nil, fmt.Errorf("%w: %w", auth.ErrInvalidToken, ErrUserInfoUnavailable)
		}
		claims.Groups = groups
		claims.Raw[p.cfg.groupsClaim()] = slices.Clone(groups)
	}
	return &auth.Principal{
		Subject:   claims.UserID(),
		Kind:      auth.KindExternal,
		Issuer:    idt.Issuer,
		TokenID:   tokenUniqueID(rawClaims, idt),
		IssuedAt:  idt.IssuedAt,
		ExpiresAt: idt.Expiry,
		Groups:    claims.Groups,
		Email:     claims.Email,
		Name:      claims.Name,
		Claims:    claims.Raw,
	}, nil
}

// tokenUniqueID returns the identifier used for revocation of IdP tokens: "jti", then
// Entra ID's "uti", then the at_hash (Dex emits no jti).
func tokenUniqueID(raw map[string]any, idt *gooidc.IDToken) string {
	for _, k := range []string{"jti", "uti"} {
		if s, ok := raw[k].(string); ok && s != "" {
			return s
		}
	}
	return idt.AccessTokenHash
}

// limitedTransport caps response body size.
type limitedTransport struct {
	base http.RoundTripper
	max  int64
}

var errResponseTooLarge = errors.New("identity provider response exceeds size limit")

func (t *limitedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	resp.Body = &limitedBody{rc: resp.Body, remaining: t.max}
	return resp, nil
}

type limitedBody struct {
	rc        io.ReadCloser
	remaining int64
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		var one [1]byte
		n, err := b.rc.Read(one[:])
		if n > 0 {
			return 0, errResponseTooLarge
		}
		return 0, err
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.rc.Read(p)
	b.remaining -= int64(n)
	return n, err
}

func (b *limitedBody) Close() error { return b.rc.Close() }
