package auth

import (
	"os"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func TestMain(m *testing.M) {
	// Keep bcrypt fast in tests; production cost is covered by TestHashPasswordDefaultCost.
	minBcryptCost = bcrypt.MinCost
	os.Exit(m.Run())
}

// fakeClock is a manually advanced clock safe for concurrent use.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func mustHash(t testing.TB, pw string) string {
	t.Helper()
	h, err := HashPasswordWithCost(pw, bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

var (
	testSecret     = []byte("0123456789abcdef0123456789abcdef-current")
	testSecretPrev = []byte("fedcba9876543210fedcba9876543210-previous")
)
