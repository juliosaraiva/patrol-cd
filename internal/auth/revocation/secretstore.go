// Package revocation provides a Kubernetes-Secret-backed auth.RevocationStore. The
// revoked tokens live in Secret "patrol-session-revocations" in the serialized form of
// auth.RevocationTable (fixed-length hashed keys, per-subject watermarks, bounded by
// entry count and bytes) and are pruned once expired, so logout state is shared by all
// patrol-server replicas without Redis (ADR-0004).
package revocation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"golang.org/x/sync/singleflight"

	"github.com/juliosaraiva/patrol-cd/internal/auth"
)

const (
	// DefaultSecretName is the Secret holding revocations.
	DefaultSecretName = "patrol-session-revocations"
	// DefaultCacheTTL is how long a read of the Secret is reused by IsRevoked. A
	// revocation made on another replica becomes visible within this delay.
	DefaultCacheTTL = 5 * time.Second
	// DefaultMaxStale is how long past the TTL a cached copy may still be served when
	// the API server cannot be reached. Afterwards IsRevoked fails (closed).
	DefaultMaxStale = 30 * time.Second

	refreshTimeout = 10 * time.Second
)

// ErrUnavailable wraps failures to read the revocation Secret when no usable cached
// copy exists.
var ErrUnavailable = errors.New("revocation store unavailable")

// SecretStore implements auth.RevocationStore on a Secret. Writes use the Secret's
// resourceVersion for optimistic concurrency and retry on conflict; reads are served from
// a short-TTL in-memory copy refreshed with a single GET (no informer). Every write
// rewrites the whole Secret, which auth.RevocationLimits keeps at or below MaxBytes of
// keys and values (512 KiB by default).
type SecretStore struct {
	client    kubernetes.Interface
	namespace string
	name      string
	cacheTTL  time.Duration
	maxStale  time.Duration
	limits    auth.RevocationLimits
	backoff   wait.Backoff
	now       func() time.Time
	logger    *slog.Logger

	mu       sync.RWMutex
	table    *auth.RevocationTable
	loadedAt time.Time
	loaded   bool
	sf       singleflight.Group
}

var _ auth.RevocationStore = (*SecretStore)(nil)

// Option customizes a SecretStore.
type Option func(*SecretStore)

// WithSecretName overrides DefaultSecretName.
func WithSecretName(name string) Option { return func(s *SecretStore) { s.name = name } }

// WithCacheTTL overrides DefaultCacheTTL (0 disables caching of reads).
func WithCacheTTL(d time.Duration) Option { return func(s *SecretStore) { s.cacheTTL = max(0, d) } }

// WithMaxStale overrides DefaultMaxStale.
func WithMaxStale(d time.Duration) Option { return func(s *SecretStore) { s.maxStale = max(0, d) } }

// WithMaxEntries overrides auth.DefaultRevocationMaxEntries (entries plus watermarks).
func WithMaxEntries(n int) Option { return func(s *SecretStore) { s.limits.MaxEntries = n } }

// WithMaxBytes overrides auth.DefaultRevocationMaxBytes (summed key and value bytes).
func WithMaxBytes(n int) Option { return func(s *SecretStore) { s.limits.MaxBytes = n } }

// WithMaxEntriesPerSubject overrides auth.DefaultRevocationMaxEntriesPerSubject.
func WithMaxEntriesPerSubject(n int) Option {
	return func(s *SecretStore) { s.limits.MaxEntriesPerSubject = n }
}

// WithClock sets the time source.
func WithClock(now func() time.Time) Option { return func(s *SecretStore) { s.now = now } }

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) Option { return func(s *SecretStore) { s.logger = l } }

// WithBackoff sets the conflict retry backoff (default retry.DefaultBackoff).
func WithBackoff(b wait.Backoff) Option { return func(s *SecretStore) { s.backoff = b } }

// NewSecretStore returns a store for Secret DefaultSecretName in namespace.
func NewSecretStore(client kubernetes.Interface, namespace string, opts ...Option) (*SecretStore, error) {
	if client == nil || namespace == "" {
		return nil, errors.New("revocation store: client and namespace are required")
	}
	s := &SecretStore{
		client:    client,
		namespace: namespace,
		name:      DefaultSecretName,
		cacheTTL:  DefaultCacheTTL,
		maxStale:  DefaultMaxStale,
		backoff:   retry.DefaultBackoff,
		now:       time.Now,
		logger:    slog.Default(),
	}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

// Revoke implements auth.RevocationStore. It is idempotent; a later expiry for an
// already-revoked token extends the entry. Expired entries are pruned and over-limit
// subjects collapsed in the same write (see auth.RevocationTable).
func (s *SecretStore) Revoke(ctx context.Context, ref auth.TokenRef) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	if !ref.ExpiresAt.After(s.now()) {
		return nil // already expired: nothing to revoke
	}
	isRetriable := func(err error) bool { return apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) }
	var final *auth.RevocationTable
	err := retry.OnError(s.backoff, isRetriable, func() error {
		secrets := s.client.CoreV1().Secrets(s.namespace)
		sec, err := secrets.Get(ctx, s.name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			table := auth.NewRevocationTable()
			if _, err := table.Revoke(ref, s.now(), s.limits); err != nil {
				return err
			}
			obj := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      s.name,
					Namespace: s.namespace,
					Labels: map[string]string{
						"app.kubernetes.io/part-of":   "patrol-cd",
						"app.kubernetes.io/component": "session-revocations",
					},
				},
				Type: corev1.SecretTypeOpaque,
				Data: table.Data(),
			}
			if _, err := secrets.Create(ctx, obj, metav1.CreateOptions{}); err != nil {
				return err
			}
			final = table
			return nil
		}
		if err != nil {
			return err
		}
		table := s.decode(sec.Data)
		changed, err := table.Revoke(ref, s.now(), s.limits)
		if err != nil {
			return err
		}
		if changed {
			sec = sec.DeepCopy()
			sec.Data = table.Data()
			if _, err := secrets.Update(ctx, sec, metav1.UpdateOptions{}); err != nil {
				return err
			}
		}
		final = table
		return nil
	})
	if err != nil {
		return fmt.Errorf("revoke token: %w", err)
	}
	s.store(final)
	return nil
}

// IsRevoked implements auth.RevocationStore.
func (s *SecretStore) IsRevoked(ctx context.Context, ref auth.TokenRef) (bool, error) {
	table, err := s.snapshot(ctx)
	if err != nil {
		return false, err
	}
	return table.IsRevoked(ref, s.now()), nil
}

// Prune removes expired and malformed entries from the Secret and returns how many were
// removed. Revoke prunes opportunistically; call Prune periodically to shrink the Secret
// when no revocations happen.
func (s *SecretStore) Prune(ctx context.Context) (int, error) {
	removed := 0
	var final *auth.RevocationTable
	err := retry.RetryOnConflict(s.backoff, func() error {
		secrets := s.client.CoreV1().Secrets(s.namespace)
		sec, err := secrets.Get(ctx, s.name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			removed, final = 0, auth.NewRevocationTable()
			return nil
		}
		if err != nil {
			return err
		}
		table := s.decode(sec.Data)
		table.Prune(s.now())
		// Expired and undecodable entries are both dropped.
		removed = len(sec.Data) - table.Len()
		if removed > 0 {
			sec = sec.DeepCopy()
			sec.Data = table.Data()
			if _, err := secrets.Update(ctx, sec, metav1.UpdateOptions{}); err != nil {
				return err
			}
		}
		final = table
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("prune revocations: %w", err)
	}
	s.store(final)
	return removed, nil
}

func (s *SecretStore) snapshot(ctx context.Context) (*auth.RevocationTable, error) {
	s.mu.RLock()
	table, loadedAt, loaded := s.table, s.loadedAt, s.loaded
	s.mu.RUnlock()
	age := s.now().Sub(loadedAt)
	if loaded && age < s.cacheTTL {
		return table, nil
	}
	ch := s.sf.DoChan("refresh", func() (any, error) {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
		defer cancel()
		sec, err := s.client.CoreV1().Secrets(s.namespace).Get(rctx, s.name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			fresh := auth.NewRevocationTable()
			s.store(fresh)
			return fresh, nil
		}
		if err != nil {
			return nil, err
		}
		fresh := s.decode(sec.Data)
		s.store(fresh)
		return fresh, nil
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		if r.Err == nil {
			return r.Val.(*auth.RevocationTable), nil
		}
		if loaded && age < s.cacheTTL+s.maxStale {
			s.logger.Warn("serving stale session revocations", "age", age, "error", r.Err)
			return table, nil
		}
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, r.Err)
	}
}

// store replaces the cached copy. The table is never mutated afterwards.
func (s *SecretStore) store(table *auth.RevocationTable) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.table, s.loadedAt, s.loaded = table, s.now(), true
}

func (s *SecretStore) decode(data map[string][]byte) *auth.RevocationTable {
	table, bad := auth.DecodeRevocationTable(data)
	if bad > 0 {
		s.logger.Warn("ignoring malformed session revocation entries", "secret", s.name, "count", bad)
	}
	return table
}
