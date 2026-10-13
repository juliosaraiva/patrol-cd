package auth

import (
	"context"
	"errors"
	"sync"
	"time"
)

// RevocationStore records revoked token IDs until the token would have expired anyway.
// Implementations must be safe for concurrent use. IsRevoked errors are treated as
// infrastructure failures by TokenService (the token is not accepted).
type RevocationStore interface {
	// Revoke marks jti as revoked until exp. Revoking an already-expired token is a no-op.
	Revoke(ctx context.Context, jti string, exp time.Time) error
	// IsRevoked reports whether jti has been revoked.
	IsRevoked(ctx context.Context, jti string) (bool, error)
}

// TokenRegistry reports whether a project-role token is still recorded (upstream:
// AppProject.status.jwtTokensByRole). subject is "proj:<project>:<role>", jti the token
// ID and issuedAt its "iat" claim (older tokens may be recorded by issue time only).
// Removing the record revokes the token.
type TokenRegistry interface {
	IsTokenKnown(ctx context.Context, subject, jti string, issuedAt int64) (bool, error)
}

// TokenRegistryFunc adapts a function to TokenRegistry.
type TokenRegistryFunc func(ctx context.Context, subject, jti string, issuedAt int64) (bool, error)

// IsTokenKnown calls f.
func (f TokenRegistryFunc) IsTokenKnown(ctx context.Context, subject, jti string, issuedAt int64) (bool, error) {
	return f(ctx, subject, jti, issuedAt)
}

// ErrRevocationStoreFull is returned when a bounded revocation store cannot accept more
// entries even after pruning expired ones.
var ErrRevocationStoreFull = errors.New("revocation store is full")

// MemoryRevocationStore is an in-process RevocationStore for tests and single-replica
// development setups. It is bounded by maxEntries and prunes expired entries on write.
type MemoryRevocationStore struct {
	mu         sync.Mutex
	entries    map[string]time.Time
	maxEntries int
	now        func() time.Time
}

// NewMemoryRevocationStore returns a store holding at most maxEntries unexpired entries
// (<= 0 selects 100000). now may be nil (time.Now).
func NewMemoryRevocationStore(maxEntries int, now func() time.Time) *MemoryRevocationStore {
	if maxEntries <= 0 {
		maxEntries = 100000
	}
	if now == nil {
		now = time.Now
	}
	return &MemoryRevocationStore{entries: map[string]time.Time{}, maxEntries: maxEntries, now: now}
}

// Revoke implements RevocationStore.
func (m *MemoryRevocationStore) Revoke(_ context.Context, jti string, exp time.Time) error {
	if jti == "" {
		return errors.New("revoke: empty token id")
	}
	if exp.IsZero() {
		return ErrNotRevocable
	}
	now := m.now()
	if !exp.After(now) {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.entries[jti]; ok {
		if exp.After(cur) {
			m.entries[jti] = exp
		}
		return nil
	}
	if len(m.entries) >= m.maxEntries {
		for k, e := range m.entries {
			if !e.After(now) {
				delete(m.entries, k)
			}
		}
		if len(m.entries) >= m.maxEntries {
			return ErrRevocationStoreFull
		}
	}
	m.entries[jti] = exp
	return nil
}

// IsRevoked implements RevocationStore.
func (m *MemoryRevocationStore) IsRevoked(_ context.Context, jti string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.entries[jti]
	return ok, nil
}
