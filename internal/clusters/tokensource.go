package clusters

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
)

// Token is a bearer token with its expiry. A zero Expiry means the token does not expire.
type Token struct {
	Value  string
	Expiry time.Time
}

// String hides the token value.
func (t Token) String() string {
	return fmt.Sprintf("Token{expiry:%s}", t.Expiry.Format(time.RFC3339))
}

// GoString implements fmt.GoStringer so %#v does not print the token either.
func (t Token) GoString() string { return t.String() }

// TokenSource produces bearer tokens for a cluster.
type TokenSource interface {
	Token(ctx context.Context) (Token, error)
}

// TokenSourceFunc adapts a function to TokenSource.
type TokenSourceFunc func(ctx context.Context) (Token, error)

// Token calls f.
func (f TokenSourceFunc) Token(ctx context.Context) (Token, error) { return f(ctx) }

// tokenRefreshMargin is how long before expiry a cached token is replaced.
const tokenRefreshMargin = time.Minute

// cachingTokenSource returns the cached token until it is within tokenRefreshMargin of its
// expiry or was invalidated after a 401 response. Concurrent callers share one refresh.
type cachingTokenSource struct {
	src TokenSource
	now func() time.Time

	mu      sync.Mutex
	tok     Token
	valid   bool
	pending *tokenFetch
}

type tokenFetch struct {
	done chan struct{}
	tok  Token
	err  error
}

func newCachingTokenSource(src TokenSource, now func() time.Time) *cachingTokenSource {
	if now == nil {
		now = time.Now
	}
	return &cachingTokenSource{src: src, now: now}
}

func (c *cachingTokenSource) Token(ctx context.Context) (Token, error) {
	c.mu.Lock()
	if c.valid && (c.tok.Expiry.IsZero() || c.now().Add(tokenRefreshMargin).Before(c.tok.Expiry)) {
		tok := c.tok
		c.mu.Unlock()
		return tok, nil
	}
	f := c.pending
	if f == nil {
		f = &tokenFetch{done: make(chan struct{})}
		c.pending = f
		// The fetch is detached from the first caller's context so that one cancelled
		// request does not fail the others waiting for the same token; it is bounded instead.
		go func() {
			fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
			defer cancel()
			tok, err := c.src.Token(fctx)
			c.mu.Lock()
			f.tok, f.err = tok, err
			if err == nil {
				c.tok, c.valid = tok, true
			}
			c.pending = nil
			c.mu.Unlock()
			close(f.done)
		}()
	}
	c.mu.Unlock()
	select {
	case <-f.done:
		return f.tok, f.err
	case <-ctx.Done():
		return Token{}, ctx.Err()
	}
}

// invalidate drops tok if it is still the cached token.
func (c *cachingTokenSource) invalidate(tok Token) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.valid && c.tok.Value == tok.Value {
		c.valid = false
	}
}

// bearerTransport sets "Authorization: Bearer <token>" from a cachingTokenSource and drops the
// cached token when the server answers 401, so the next request fetches a new one.
type bearerTransport struct {
	base http.RoundTripper
	src  *cachingTokenSource
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tok, err := t.src.Token(req.Context())
	if err != nil {
		return nil, fmt.Errorf("get cluster bearer token: %w", err)
	}
	r := req.Clone(req.Context())
	r.Header.Set("Authorization", "Bearer "+tok.Value)
	resp, err := t.base.RoundTrip(r)
	if err == nil && resp.StatusCode == http.StatusUnauthorized {
		t.src.invalidate(tok)
	}
	return resp, err
}

// WrappedRoundTripper exposes the inner transport to client-go utilities.
func (t *bearerTransport) WrappedRoundTripper() http.RoundTripper { return t.base }

// wrapWithTokenSource returns a rest.Config WrapTransport function injecting tokens from src.
func wrapWithTokenSource(src *cachingTokenSource) func(http.RoundTripper) http.RoundTripper {
	return func(rt http.RoundTripper) http.RoundTripper {
		return &bearerTransport{base: rt, src: src}
	}
}

// ProviderCache shares token sources between rest.Configs built for the same provider settings,
// so tokens are reused across RestConfig calls (as client-go caches exec authenticators). It is
// safe for concurrent use. The zero value is not usable; use NewProviderCache.
type ProviderCache struct {
	entries *lru.Cache[string, *cachingTokenSource]
	mu      sync.Mutex
}

// NewProviderCache returns a cache holding at most size token sources (LRU eviction).
func NewProviderCache(size int) (*ProviderCache, error) {
	if size <= 0 {
		return nil, errors.New("provider cache size must be positive")
	}
	c, err := lru.New[string, *cachingTokenSource](size)
	if err != nil {
		return nil, fmt.Errorf("create provider cache: %w", err)
	}
	return &ProviderCache{entries: c}, nil
}

var (
	defaultProviderCacheOnce sync.Once
	defaultProviderCache     *ProviderCache
)

func sharedProviderCache() *ProviderCache {
	defaultProviderCacheOnce.Do(func() {
		c, err := NewProviderCache(1024)
		if err != nil {
			// Unreachable: the size is a positive constant.
			c = &ProviderCache{}
		}
		defaultProviderCache = c
	})
	return defaultProviderCache
}

// get returns the cached source for key, creating it with mk when absent.
func (p *ProviderCache) get(key string, now func() time.Time, mk func() (TokenSource, error)) (*cachingTokenSource, error) {
	if p == nil || p.entries == nil {
		src, err := mk()
		if err != nil {
			return nil, err
		}
		return newCachingTokenSource(src, now), nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.entries.Get(key); ok {
		return c, nil
	}
	src, err := mk()
	if err != nil {
		return nil, err
	}
	c := newCachingTokenSource(src, now)
	p.entries.Add(key, c)
	return c, nil
}

// Len returns the number of cached token sources.
func (p *ProviderCache) Len() int {
	if p == nil || p.entries == nil {
		return 0
	}
	return p.entries.Len()
}
