package oidc

import (
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

var (
	serverSecret     = []byte("0123456789abcdef0123456789abcdef-server")
	serverSecretPrev = []byte("fedcba9876543210fedcba9876543210-prev")
	discardLogger    = slog.New(slog.NewTextHandler(io.Discard, nil))
)

const callbackURL = "https://patrol.example.com/auth/callback"

var allowedBases = []string{"https://patrol.example.com"}

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock() *testClock { return &testClock{t: time.Now().Truncate(time.Second)} }

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestProvider(t *testing.T, f *fakeIdP, mutate func(*Config), opts ...Option) *Provider {
	t.Helper()
	cfg := f.config()
	if mutate != nil {
		mutate(&cfg)
	}
	all := append([]Option{WithHTTPClient(f.srv.Client()), WithLogger(discardLogger), WithDiscoveryRetry(1, 0), WithFailureCooldown(0)}, opts...)
	p, err := NewProvider(cfg, all...)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func newTestCodec(t *testing.T, now func() time.Time) *StateCodec {
	t.Helper()
	c, err := NewStateCodec(now, serverSecret)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
