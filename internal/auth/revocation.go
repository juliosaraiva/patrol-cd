package auth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// TokenRef identifies a token for revocation.
type TokenRef struct {
	// ID is the token's unique ID (jti, or the ID an ExternalVerifier derived).
	ID string
	// Subject groups the revocations of one principal. Stores bound the entries a single
	// subject may hold by collapsing them into a watermark ("every token of this subject
	// issued at or before T is revoked"), so one principal cannot exhaust a shared store.
	// TokenService sets it from the token kind, issuer and subject, which keeps for
	// example an account's login sessions and its API tokens apart.
	Subject string
	// IssuedAt is the token's iat. Zero means unknown; such a token is covered by any
	// watermark of its subject.
	IssuedAt time.Time
	// ExpiresAt is the last instant the token can verify, including clock-skew leeway.
	// Revoke requires it (ErrNotRevocable otherwise).
	ExpiresAt time.Time
}

// Validate reports whether r can be passed to RevocationStore.Revoke.
func (r TokenRef) Validate() error {
	if r.ID == "" || len(r.ID) > maxRevocationIDBytes {
		return fmt.Errorf("revoke: token id must be 1..%d bytes", maxRevocationIDBytes)
	}
	if len(r.Subject) > maxRevocationSubjectBytes {
		return fmt.Errorf("revoke: subject must be at most %d bytes", maxRevocationSubjectBytes)
	}
	if r.ExpiresAt.IsZero() {
		return ErrNotRevocable
	}
	return nil
}

// RevocationStore records revoked tokens until they would have expired anyway.
// Implementations must be safe for concurrent use. IsRevoked errors are treated as
// infrastructure failures by TokenService (the token is not accepted).
//
// A store may over-approximate: once a subject holds too many revocations they are
// collapsed into a watermark that also revokes that subject's other tokens issued at or
// before the newest collapsed one (see RevocationTable). A store must never report a
// revoked, unexpired token as not revoked.
type RevocationStore interface {
	// Revoke marks the token as revoked until ref.ExpiresAt. Revoking an already-expired
	// token is a no-op; ref.ExpiresAt must be set (ErrNotRevocable).
	Revoke(ctx context.Context, ref TokenRef) error
	// IsRevoked reports whether the token has been revoked.
	IsRevoked(ctx context.Context, ref TokenRef) (bool, error)
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

// ErrRevocationStoreFull is returned when a bounded revocation store cannot accept a
// revocation even after pruning expired entries and collapsing every subject that holds
// more than one entry. It takes more distinct subjects with live revocations than the
// store's entry limit.
var ErrRevocationStoreFull = errors.New("revocation store is full")

// MemoryRevocationStore is an in-process RevocationStore for tests and single-replica
// development setups. It applies the same RevocationTable rules (limits, per-subject
// collapse) as the Secret-backed store.
type MemoryRevocationStore struct {
	mu     sync.Mutex
	table  *RevocationTable
	limits RevocationLimits
	now    func() time.Time
}

// NewMemoryRevocationStore returns a store bounded by limits (zero fields select the
// defaults). now may be nil (time.Now).
func NewMemoryRevocationStore(limits RevocationLimits, now func() time.Time) *MemoryRevocationStore {
	if now == nil {
		now = time.Now
	}
	return &MemoryRevocationStore{table: NewRevocationTable(), limits: limits, now: now}
}

// Revoke implements RevocationStore.
func (m *MemoryRevocationStore) Revoke(_ context.Context, ref TokenRef) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, err := m.table.Revoke(ref, m.now(), m.limits)
	return err
}

// IsRevoked implements RevocationStore.
func (m *MemoryRevocationStore) IsRevoked(_ context.Context, ref TokenRef) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.table.IsRevoked(ref, m.now()), nil
}
