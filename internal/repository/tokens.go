package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
	"golang.org/x/sync/singleflight"
)

// Credential kinds of minted tokens (metric label values).
const (
	tokenKindGitHubApp             = "github_app"
	tokenKindGCP                   = "gcp"
	tokenKindAzureWorkloadIdentity = "azure_workload_identity"
	tokenKindAzureServicePrincipal = "azure_service_principal"
	tokenKindACR                   = "acr"
)

// mintedToken is a short-lived credential. Value is secret; String never prints it.
type mintedToken struct {
	value    string
	username string
	// expiresAt is the issuer's expiry (zero when unknown).
	expiresAt time.Time
	// validUntil is when the cache stops reusing the token (before expiresAt).
	validUntil time.Time
}

func (t mintedToken) String() string {
	return fmt.Sprintf("mintedToken{username:%q, value:<redacted>, expiresAt:%s}", t.username, t.expiresAt.Format(time.RFC3339))
}

// GoString prevents %#v from printing the value.
func (t mintedToken) GoString() string { return t.String() }

// tokenCache caches minted tokens until shortly before they expire and coalesces concurrent
// mints of the same key. Keys are SHA-256 digests of the credential identity (never the
// secret itself).
type tokenCache struct {
	entries       *lru.Cache[string, mintedToken]
	group         singleflight.Group
	now           func() time.Time
	refreshBefore time.Duration
	timeout       time.Duration
	metrics       *metrics
}

func newTokenCache(size int, now func() time.Time, refreshBefore, timeout time.Duration, m *metrics) (*tokenCache, error) {
	entries, err := lru.New[string, mintedToken](size)
	if err != nil {
		return nil, fmt.Errorf("token cache: %w", err)
	}
	return &tokenCache{entries: entries, now: now, refreshBefore: refreshBefore, timeout: timeout, metrics: m}, nil
}

// cacheKey hashes the parts of a credential identity.
func cacheKey(kind string, parts ...string) string {
	h := sha256.New()
	h.Write([]byte(kind))
	for _, p := range parts {
		var n [8]byte
		l := uint64(len(p))
		for i := range n {
			n[i] = byte(l >> (8 * i))
		}
		h.Write(n[:])
		h.Write([]byte(p))
	}
	return kind + ":" + hex.EncodeToString(h.Sum(nil))
}

// validUntil computes how long a token may be reused: until refreshBefore ahead of its
// expiry (at most half of its lifetime), capped by maxAge when positive. Tokens without a
// known expiry are not reused unless maxAge is set.
func (c *tokenCache) validUntil(minted, expiresAt time.Time, maxAge time.Duration) time.Time {
	var until time.Time
	if !expiresAt.IsZero() {
		margin := c.refreshBefore
		if life := expiresAt.Sub(minted); life > 0 && life/2 < margin {
			margin = life / 2
		}
		until = expiresAt.Add(-margin)
	}
	if maxAge > 0 {
		capAt := minted.Add(maxAge)
		if until.IsZero() || capAt.Before(until) {
			until = capAt
		}
	}
	return until
}

// get returns a cached token for key or mints one. The mint runs detached from the caller's
// cancellation (other waiters share it) and is bounded by the cache timeout; the caller
// stops waiting when ctx ends. maxAge caps reuse (0 = until shortly before expiry).
func (c *tokenCache) get(ctx context.Context, kind, key string, maxAge time.Duration, mint func(context.Context) (mintedToken, error)) (mintedToken, error) {
	if t, ok := c.entries.Get(key); ok && c.now().Before(t.validUntil) {
		c.metrics.tokenRequest(kind, "hit")
		return t, nil
	}
	ch := c.group.DoChan(key, func() (any, error) {
		if t, ok := c.entries.Get(key); ok && c.now().Before(t.validUntil) {
			return t, nil
		}
		mctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.timeout)
		defer cancel()
		start := time.Now()
		t, err := mint(mctx)
		c.metrics.observeMint(kind, time.Since(start))
		if err != nil {
			c.metrics.tokenRequest(kind, "error")
			return nil, err
		}
		if t.value == "" {
			c.metrics.tokenRequest(kind, "error")
			return nil, fmt.Errorf("%w: the issuer returned an empty token", ErrTokenUnavailable)
		}
		c.metrics.tokenRequest(kind, "minted")
		t.validUntil = c.validUntil(c.now(), t.expiresAt, maxAge)
		if c.now().Before(t.validUntil) {
			c.entries.Add(key, t)
		}
		return t, nil
	})
	select {
	case <-ctx.Done():
		return mintedToken{}, context.Cause(ctx)
	case res := <-ch:
		if res.Err != nil {
			return mintedToken{}, res.Err
		}
		t, ok := res.Val.(mintedToken)
		if !ok {
			return mintedToken{}, errors.New("token cache: unexpected value")
		}
		return t, nil
	}
}

// purge drops every cached token (tests, credential rotation hooks).
func (c *tokenCache) purge() { c.entries.Purge() }
