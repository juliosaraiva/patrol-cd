package repository

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestTokenCache(t *testing.T, clock *fakeClock) *tokenCache {
	t.Helper()
	m, err := newMetrics(prometheus.NewRegistry())
	require.NoError(t, err)
	c, err := newTokenCache(16, clock.Now, 5*time.Minute, time.Second, m)
	require.NoError(t, err)
	return c
}

func TestTokenCacheValidUntil(t *testing.T) {
	t.Parallel()
	c := newTestTokenCache(t, newFakeClock(time.Now()))
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		exp    time.Time
		maxAge time.Duration
		want   time.Time
	}{
		{"unknown expiry is not reused", time.Time{}, 0, time.Time{}},
		{"unknown expiry with max age", time.Time{}, time.Minute, t0.Add(time.Minute)},
		{"refresh window before expiry", t0.Add(time.Hour), 0, t0.Add(55 * time.Minute)},
		{"short lifetime uses half", t0.Add(4 * time.Minute), 0, t0.Add(2 * time.Minute)},
		{"max age caps reuse", t0.Add(time.Hour), 10 * time.Minute, t0.Add(10 * time.Minute)},
		{"max age beyond expiry is ignored", t0.Add(time.Hour), 2 * time.Hour, t0.Add(55 * time.Minute)},
		{"already expired", t0.Add(-time.Minute), 0, t0.Add(-6 * time.Minute)},
	}
	for _, tc := range cases {
		assert.True(t, tc.want.Equal(c.validUntil(t0, tc.exp, tc.maxAge)), tc.name)
	}
}

// TestTokenCacheValidUntilProperty: a token with a known future expiry is reused for a
// positive time and never past its expiry, nor past maxAge.
func TestTokenCacheValidUntilProperty(t *testing.T) {
	t.Parallel()
	c := newTestTokenCache(t, newFakeClock(time.Now()))
	rng := rand.New(rand.NewPCG(1, 2))
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for range 10000 {
		life := time.Duration(rng.Int64N(int64(48*time.Hour))) + time.Second
		maxAge := time.Duration(0)
		if rng.IntN(2) == 0 {
			maxAge = time.Duration(rng.Int64N(int64(2*time.Hour))) + time.Second
		}
		until := c.validUntil(t0, t0.Add(life), maxAge)
		require.True(t, until.After(t0), "life %s maxAge %s", life, maxAge)
		require.True(t, until.Before(t0.Add(life)), "life %s maxAge %s", life, maxAge)
		if maxAge > 0 {
			require.False(t, until.After(t0.Add(maxAge)), "life %s maxAge %s", life, maxAge)
		}
	}
}

func TestTokenCacheGet(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(time.Now())
	c := newTestTokenCache(t, clock)
	ctx := t.Context()
	var mints atomic.Int32
	mint := func(context.Context) (mintedToken, error) {
		n := mints.Add(1)
		return mintedToken{value: fmt.Sprintf("tok-%d", n), expiresAt: clock.Now().Add(time.Hour)}, nil
	}
	a, err := c.get(ctx, "k", "key-a", 0, mint)
	require.NoError(t, err)
	b, err := c.get(ctx, "k", "key-a", 0, mint)
	require.NoError(t, err)
	assert.Equal(t, a.value, b.value)
	other, err := c.get(ctx, "k", "key-b", 0, mint)
	require.NoError(t, err)
	assert.NotEqual(t, a.value, other.value)
	assert.Equal(t, int32(2), mints.Load())

	// Errors and empty tokens are not cached.
	boom := errors.New("issuer down")
	_, err = c.get(ctx, "k", "key-c", 0, func(context.Context) (mintedToken, error) { return mintedToken{}, boom })
	assert.ErrorIs(t, err, boom)
	_, err = c.get(ctx, "k", "key-c", 0, func(context.Context) (mintedToken, error) { return mintedToken{}, nil })
	assert.ErrorIs(t, err, ErrTokenUnavailable)
	c3, err := c.get(ctx, "k", "key-c", 0, mint)
	require.NoError(t, err)
	assert.Equal(t, "tok-3", c3.value)

	// Tokens without expiry are not reused.
	noExp := func(context.Context) (mintedToken, error) {
		return mintedToken{value: fmt.Sprintf("noexp-%d", mints.Add(1))}, nil
	}
	x, err := c.get(ctx, "k", "key-d", 0, noExp)
	require.NoError(t, err)
	y, err := c.get(ctx, "k", "key-d", 0, noExp)
	require.NoError(t, err)
	assert.NotEqual(t, x.value, y.value)

	c.purge()
	d, err := c.get(ctx, "k", "key-a", 0, mint)
	require.NoError(t, err)
	assert.NotEqual(t, a.value, d.value)
}

// TestTokenCacheCancellation: a caller whose context ends stops waiting, while the shared
// mint completes for the others and is cached.
func TestTokenCacheCancellation(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(time.Now())
	c := newTestTokenCache(t, clock)
	release := make(chan struct{})
	started := make(chan struct{})
	mint := func(ctx context.Context) (mintedToken, error) {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return mintedToken{}, ctx.Err()
		}
		return mintedToken{value: "shared", expiresAt: clock.Now().Add(time.Hour)}, nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	errc := make(chan error, 1)
	go func() {
		_, err := c.get(ctx, "k", "key", 0, mint)
		errc <- err
	}()
	<-started
	waiter := make(chan mintedToken, 1)
	go func() {
		tok, err := c.get(t.Context(), "k", "key", 0, mint)
		if err == nil {
			waiter <- tok
		}
		close(waiter)
	}()
	cancel()
	assert.ErrorIs(t, <-errc, context.Canceled)
	close(release)
	tok, ok := <-waiter
	require.True(t, ok)
	assert.Equal(t, "shared", tok.value)
	cached, err := c.get(t.Context(), "k", "key", 0, func(context.Context) (mintedToken, error) {
		return mintedToken{}, errors.New("must not mint again")
	})
	require.NoError(t, err)
	assert.Equal(t, "shared", cached.value)

	// The mint timeout bounds a hung issuer.
	hung := func(ctx context.Context) (mintedToken, error) {
		<-ctx.Done()
		return mintedToken{}, ctx.Err()
	}
	_, err = c.get(t.Context(), "k", "hung", 0, hung)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestCacheKeyAndTokenFormatting(t *testing.T) {
	t.Parallel()
	assert.NotEqual(t, cacheKey("k", "ab", "c"), cacheKey("k", "a", "bc"), "parts are length-prefixed")
	assert.Equal(t, cacheKey("k", "a"), cacheKey("k", "a"))
	assert.NotContains(t, cacheKey("k", "super-secret"), "super-secret")
	tok := mintedToken{value: "super-secret-token", username: "u", expiresAt: time.Unix(0, 0).UTC()}
	for _, s := range []string{tok.String(), fmt.Sprintf("%v", tok), fmt.Sprintf("%+v", tok), fmt.Sprintf("%#v", tok)} {
		assert.NotContains(t, s, "super-secret-token")
	}
}

func TestRedact(t *testing.T) {
	t.Parallel()
	base := fmt.Errorf("auth failed for pass-1234 and pass-12345: %w", ErrTokenUnavailable)
	err := redact(base, []string{"pass-1234", "pass-12345", "abc", ""})
	assert.Equal(t, "auth failed for ****** and ******: failed to obtain repository access token", err.Error())
	assert.ErrorIs(t, err, ErrTokenUnavailable)
	assert.Same(t, base, redact(base, []string{"absent"}))
	assert.NoError(t, redact(nil, []string{"x"}))
	short := errors.New("value abc")
	assert.Equal(t, "value abc", redact(short, []string{"abc"}).Error(), "short values are not redacted")
}

func TestConditionReasons(t *testing.T) {
	t.Parallel()
	cases := map[error]string{
		nil:                                      "ConnectionSuccessful",
		fmt.Errorf("x: %w", ErrSecretNotFound):   "SecretNotFound",
		fmt.Errorf("x: %w", ErrSecretKeyMissing): "SecretKeyMissing",
		ErrSCMProviderNotFound:                   "SCMProviderNotFound",
		ErrInvalidCredentials:                    "InvalidCredentials",
		ErrInvalidConfiguration:                  "InvalidConfiguration",
		ErrTokenUnavailable:                      "ConnectionFailed",
		errors.New("network"):                    "ConnectionFailed",
	}
	for err, want := range cases {
		assert.Equal(t, want, ConditionReason(err), "%v", err)
	}
	assert.False(t, IsConfigurationError(ErrTokenUnavailable))
	assert.True(t, IsConfigurationError(fmt.Errorf("wrapped: %w", ErrSecretKeyMissing)))
}
