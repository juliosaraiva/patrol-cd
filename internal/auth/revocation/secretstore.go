// Package revocation provides a Kubernetes-Secret-backed auth.RevocationStore. Revoked
// token IDs are stored in Secret "patrol-session-revocations" as "<key>: <exp unix>"
// entries and pruned once expired, so logout state is shared by all patrol-server
// replicas without Redis (ADR-0004).
package revocation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"strconv"
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
	// DefaultMaxEntries keeps the Secret well below the 1 MiB object size limit.
	DefaultMaxEntries = 15000

	maxTokenIDLength = 1024
	refreshTimeout   = 10 * time.Second
)

// ErrUnavailable wraps failures to read the revocation Secret when no usable cached
// copy exists.
var ErrUnavailable = errors.New("revocation store unavailable")

var validKeyRE = regexp.MustCompile(`^[-._a-zA-Z0-9]{1,253}$`)

// SecretStore implements auth.RevocationStore on a Secret. Writes use the Secret's
// resourceVersion for optimistic concurrency and retry on conflict; reads are served from
// a short-TTL in-memory copy refreshed with a single GET (no informer).
type SecretStore struct {
	client     kubernetes.Interface
	namespace  string
	name       string
	cacheTTL   time.Duration
	maxStale   time.Duration
	maxEntries int
	backoff    wait.Backoff
	now        func() time.Time
	logger     *slog.Logger

	mu       sync.RWMutex
	entries  map[string]int64
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

// WithMaxEntries overrides DefaultMaxEntries.
func WithMaxEntries(n int) Option { return func(s *SecretStore) { s.maxEntries = n } }

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
		client:     client,
		namespace:  namespace,
		name:       DefaultSecretName,
		cacheTTL:   DefaultCacheTTL,
		maxStale:   DefaultMaxStale,
		maxEntries: DefaultMaxEntries,
		backoff:    retry.DefaultBackoff,
		now:        time.Now,
		logger:     slog.Default(),
	}
	for _, o := range opts {
		o(s)
	}
	if s.maxEntries <= 0 {
		s.maxEntries = DefaultMaxEntries
	}
	return s, nil
}

// EncodeKey maps a token ID to a valid Secret data key: IDs made of [-._a-zA-Z0-9]
// (UUIDs) are used verbatim, anything else as "sha256-<hex>".
func EncodeKey(jti string) string {
	if validKeyRE.MatchString(jti) {
		return jti
	}
	sum := sha256.Sum256([]byte(jti))
	return "sha256-" + hex.EncodeToString(sum[:])
}

// Revoke implements auth.RevocationStore. It is idempotent; a later expiry for an
// already-revoked ID extends the entry. Expired entries are pruned in the same write.
func (s *SecretStore) Revoke(ctx context.Context, jti string, exp time.Time) error {
	if jti == "" || len(jti) > maxTokenIDLength {
		return errors.New("revoke: invalid token id")
	}
	if exp.IsZero() {
		return auth.ErrNotRevocable
	}
	if !exp.After(s.now()) {
		return nil // already expired: nothing to revoke
	}
	key, expUnix := EncodeKey(jti), exp.Unix()
	if exp.Truncate(time.Second).Before(exp) {
		expUnix++ // round up so the entry never lapses before the token
	}
	isRetriable := func(err error) bool { return apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) }
	var final map[string]int64
	err := retry.OnError(s.backoff, isRetriable, func() error {
		secrets := s.client.CoreV1().Secrets(s.namespace)
		sec, err := secrets.Get(ctx, s.name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			entries := map[string]int64{key: expUnix}
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
				Data: encode(entries),
			}
			if _, err := secrets.Create(ctx, obj, metav1.CreateOptions{}); err != nil {
				return err
			}
			final = entries
			return nil
		}
		if err != nil {
			return err
		}
		entries := s.decode(sec.Data)
		s.prune(entries)
		if cur, ok := entries[key]; ok && cur >= expUnix {
			final = entries
			return nil
		}
		if _, ok := entries[key]; !ok && len(entries) >= s.maxEntries {
			return fmt.Errorf("%w: %d entries", auth.ErrRevocationStoreFull, len(entries))
		}
		entries[key] = expUnix
		sec = sec.DeepCopy()
		sec.Data = encode(entries)
		if _, err := secrets.Update(ctx, sec, metav1.UpdateOptions{}); err != nil {
			return err
		}
		final = entries
		return nil
	})
	if err != nil {
		return fmt.Errorf("revoke token: %w", err)
	}
	s.store(final)
	return nil
}

// IsRevoked implements auth.RevocationStore.
func (s *SecretStore) IsRevoked(ctx context.Context, jti string) (bool, error) {
	entries, err := s.snapshot(ctx)
	if err != nil {
		return false, err
	}
	_, ok := entries[EncodeKey(jti)]
	return ok, nil
}

// Prune removes expired entries from the Secret and returns how many were removed.
// Revoke prunes opportunistically; call Prune periodically to bound the Secret when no
// revocations happen.
func (s *SecretStore) Prune(ctx context.Context) (int, error) {
	removed := 0
	var final map[string]int64
	err := retry.RetryOnConflict(s.backoff, func() error {
		secrets := s.client.CoreV1().Secrets(s.namespace)
		sec, err := secrets.Get(ctx, s.name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			removed, final = 0, map[string]int64{}
			return nil
		}
		if err != nil {
			return err
		}
		entries := s.decode(sec.Data)
		s.prune(entries)
		// Expired and undecodable entries are both dropped.
		removed = len(sec.Data) - len(entries)
		if removed == 0 {
			final = entries
			return nil
		}
		sec = sec.DeepCopy()
		sec.Data = encode(entries)
		if _, err := secrets.Update(ctx, sec, metav1.UpdateOptions{}); err != nil {
			return err
		}
		final = entries
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("prune revocations: %w", err)
	}
	s.store(final)
	return removed, nil
}

func (s *SecretStore) snapshot(ctx context.Context) (map[string]int64, error) {
	s.mu.RLock()
	entries, loadedAt, loaded := s.entries, s.loadedAt, s.loaded
	s.mu.RUnlock()
	age := s.now().Sub(loadedAt)
	if loaded && age < s.cacheTTL {
		return entries, nil
	}
	ch := s.sf.DoChan("refresh", func() (any, error) {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
		defer cancel()
		sec, err := s.client.CoreV1().Secrets(s.namespace).Get(rctx, s.name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			fresh := map[string]int64{}
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
			return r.Val.(map[string]int64), nil
		}
		if loaded && age < s.cacheTTL+s.maxStale {
			s.logger.Warn("serving stale session revocations", "age", age, "error", r.Err)
			return entries, nil
		}
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, r.Err)
	}
}

// store replaces the cached copy. The map is never mutated afterwards.
func (s *SecretStore) store(entries map[string]int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries, s.loadedAt, s.loaded = entries, s.now(), true
}

func (s *SecretStore) decode(data map[string][]byte) map[string]int64 {
	out := make(map[string]int64, len(data))
	bad := 0
	for k, v := range data {
		n, err := strconv.ParseInt(string(v), 10, 64)
		if err != nil {
			bad++
			continue
		}
		out[k] = n
	}
	if bad > 0 {
		s.logger.Warn("ignoring malformed session revocation entries", "secret", s.name, "count", bad)
	}
	return out
}

func (s *SecretStore) prune(entries map[string]int64) int {
	now := s.now().Unix()
	n := len(entries)
	maps.DeleteFunc(entries, func(_ string, exp int64) bool { return exp <= now })
	return n - len(entries)
}

func encode(entries map[string]int64) map[string][]byte {
	out := make(map[string][]byte, len(entries))
	for k, v := range entries {
		out[k] = []byte(strconv.FormatInt(v, 10))
	}
	return out
}
