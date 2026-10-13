package revocation

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/juliosaraiva/patrol-cd/internal/auth"
)

const ns = "patrol"

var secretsGVR = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newClient returns a fake clientset that enforces resourceVersion-based optimistic
// concurrency on Secrets like the real API server (the stock fake tracker does not).
func newClient(objs ...runtime.Object) *fake.Clientset {
	cs := fake.NewClientset(objs...)
	var rv atomic.Int64
	rv.Store(100)
	cs.PrependReactor("create", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		obj := a.(k8stesting.CreateAction).GetObject().(*corev1.Secret)
		obj.ResourceVersion = strconv.FormatInt(rv.Add(1), 10)
		return false, nil, nil
	})
	cs.PrependReactor("update", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		obj := a.(k8stesting.UpdateAction).GetObject().(*corev1.Secret)
		cur, err := cs.Tracker().Get(secretsGVR, obj.Namespace, obj.Name)
		if err != nil {
			return false, nil, nil
		}
		if cur.(*corev1.Secret).ResourceVersion != obj.ResourceVersion {
			return true, nil, apierrors.NewConflict(secretsGVR.GroupResource(), obj.Name, errors.New("resourceVersion mismatch"))
		}
		obj.ResourceVersion = strconv.FormatInt(rv.Add(1), 10)
		return false, nil, nil
	})
	return cs
}

func newStore(t *testing.T, cs *fake.Clientset, c *clock, opts ...Option) *SecretStore {
	t.Helper()
	all := append([]Option{
		WithClock(c.Now),
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		WithBackoff(wait.Backoff{Steps: 100, Duration: time.Millisecond, Factor: 1, Jitter: 1}),
	}, opts...)
	s, err := NewSecretStore(cs, ns, all...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func newClock() *clock { return &clock{t: time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)} }

func getSecret(t *testing.T, cs *fake.Clientset) *corev1.Secret {
	t.Helper()
	s, err := cs.CoreV1().Secrets(ns).Get(context.Background(), DefaultSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRevokeCreatesAndUpdates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := newClock()
	cs := newClient()
	s := newStore(t, cs, c)

	if ok, err := s.IsRevoked(ctx, "a"); err != nil || ok {
		t.Fatalf("empty store: %v %v", ok, err)
	}
	exp := c.Now().Add(time.Hour)
	if err := s.Revoke(ctx, "8f14e45f-ceea-467f-a0e6-b6d7e0e8b3a1", exp); err != nil {
		t.Fatal(err)
	}
	sec := getSecret(t, cs)
	if got := string(sec.Data["8f14e45f-ceea-467f-a0e6-b6d7e0e8b3a1"]); got != strconv.FormatInt(exp.Unix(), 10) {
		t.Fatalf("stored exp = %q", got)
	}
	if sec.Labels["app.kubernetes.io/part-of"] != "patrol-cd" || sec.Type != corev1.SecretTypeOpaque {
		t.Fatalf("secret meta = %+v", sec.ObjectMeta)
	}
	if ok, err := s.IsRevoked(ctx, "8f14e45f-ceea-467f-a0e6-b6d7e0e8b3a1"); err != nil || !ok {
		t.Fatalf("revoked token not reported: %v %v", ok, err)
	}
	// Non key-safe IDs are hashed.
	weird := "id/with+chars=="
	if err := s.Revoke(ctx, weird, exp); err != nil {
		t.Fatal(err)
	}
	if _, ok := getSecret(t, cs).Data[EncodeKey(weird)]; !ok || !strings.HasPrefix(EncodeKey(weird), "sha256-") {
		t.Fatalf("hashed key missing: %v", getSecret(t, cs).Data)
	}
	if ok, _ := s.IsRevoked(ctx, weird); !ok {
		t.Fatal("hashed id not reported")
	}
	// Idempotent; a later expiry extends, an earlier one does not shorten.
	if err := s.Revoke(ctx, weird, exp.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke(ctx, weird, exp.Add(-30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := string(getSecret(t, cs).Data[EncodeKey(weird)]); got != strconv.FormatInt(exp.Add(time.Hour).Unix(), 10) {
		t.Fatalf("exp not extended: %s", got)
	}
	// Sub-second expiries round up.
	if err := s.Revoke(ctx, "frac", c.Now().Add(1500*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if got := string(getSecret(t, cs).Data["frac"]); got != strconv.FormatInt(c.Now().Unix()+2, 10) {
		t.Fatalf("fractional exp = %s", got)
	}
}

func TestRevokeValidationAndNoops(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := newClock()
	cs := newClient()
	s := newStore(t, cs, c)
	if err := s.Revoke(ctx, "x", time.Time{}); !errors.Is(err, auth.ErrNotRevocable) {
		t.Fatalf("zero exp: %v", err)
	}
	if err := s.Revoke(ctx, "", c.Now().Add(time.Hour)); err == nil {
		t.Fatal("empty id accepted")
	}
	if err := s.Revoke(ctx, strings.Repeat("x", 2000), c.Now().Add(time.Hour)); err == nil {
		t.Fatal("oversized id accepted")
	}
	if err := s.Revoke(ctx, "old", c.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CoreV1().Secrets(ns).Get(ctx, DefaultSecretName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expired revocation wrote the secret: %v", err)
	}
	if _, err := NewSecretStore(nil, ns); err == nil {
		t.Fatal("nil client accepted")
	}
	if _, err := NewSecretStore(cs, ""); err == nil {
		t.Fatal("empty namespace accepted")
	}
}

func TestRevokePrunesExpiredAndMalformed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := newClock()
	now := c.Now().Unix()
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: DefaultSecretName, Namespace: ns, ResourceVersion: "1"},
		Data: map[string][]byte{
			"expired": []byte(strconv.FormatInt(now-10, 10)),
			"live":    []byte(strconv.FormatInt(now+600, 10)),
			"garbage": []byte("not-a-number"),
		},
	}
	cs := newClient(existing)
	s := newStore(t, cs, c)
	if err := s.Revoke(ctx, "new", c.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	data := getSecret(t, cs).Data
	if _, ok := data["expired"]; ok {
		t.Fatal("expired entry kept")
	}
	if _, ok := data["garbage"]; ok {
		t.Fatal("malformed entry kept")
	}
	if _, ok := data["live"]; !ok {
		t.Fatal("live entry dropped")
	}
	if ok, _ := s.IsRevoked(ctx, "live"); !ok {
		t.Fatal("pre-existing revocation not reported")
	}
}

func TestPrune(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := newClock()
	cs := newClient()
	s := newStore(t, cs, c)
	if n, err := s.Prune(ctx); err != nil || n != 0 {
		t.Fatalf("prune without secret: %d %v", n, err)
	}
	for i := range 5 {
		if err := s.Revoke(ctx, fmt.Sprintf("t%d", i), c.Now().Add(time.Duration(i+1)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	c.Advance(3*time.Minute + time.Second)
	n, err := s.Prune(ctx)
	if err != nil || n != 3 {
		t.Fatalf("pruned %d, %v; want 3", n, err)
	}
	if got := len(getSecret(t, cs).Data); got != 2 {
		t.Fatalf("remaining entries = %d", got)
	}
	if n, err := s.Prune(ctx); err != nil || n != 0 {
		t.Fatalf("second prune: %d %v", n, err)
	}
}

func TestConflictRetry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := newClock()
	cs := newClient()
	s := newStore(t, cs, c)
	if err := s.Revoke(ctx, "first", c.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var conflicts atomic.Int32
	cs.PrependReactor("update", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if conflicts.Add(1) <= 2 {
			return true, nil, apierrors.NewConflict(secretsGVR.GroupResource(), DefaultSecretName, errors.New("injected"))
		}
		return false, nil, nil
	})
	if err := s.Revoke(ctx, "second", c.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if conflicts.Load() != 3 {
		t.Fatalf("update attempts = %d, want 3", conflicts.Load())
	}
	data := getSecret(t, cs).Data
	if _, ok := data["first"]; !ok {
		t.Fatal("first entry lost")
	}
	if _, ok := data["second"]; !ok {
		t.Fatal("second entry missing")
	}
}

func TestCreateRaceRetriesAsUpdate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := newClock()
	cs := newClient()
	s := newStore(t, cs, c)
	var once sync.Once
	cs.PrependReactor("create", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		handled := false
		once.Do(func() {
			// Another replica creates the secret between our GET and CREATE.
			other := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: DefaultSecretName, Namespace: ns, ResourceVersion: "50"},
				Data:       map[string][]byte{"theirs": []byte(strconv.FormatInt(c.Now().Add(time.Hour).Unix(), 10))},
			}
			if err := cs.Tracker().Add(other); err != nil {
				t.Error(err)
			}
			handled = true
		})
		if handled {
			return true, nil, apierrors.NewAlreadyExists(secretsGVR.GroupResource(), DefaultSecretName)
		}
		return false, nil, nil
	})
	if err := s.Revoke(ctx, "ours", c.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	data := getSecret(t, cs).Data
	if _, ok := data["theirs"]; !ok {
		t.Fatal("concurrently created entry lost")
	}
	if _, ok := data["ours"]; !ok {
		t.Fatal("our entry missing")
	}
}

func TestConcurrentRevocationsAcrossReplicas(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := newClock()
	cs := newClient()
	replicas := []*SecretStore{newStore(t, cs, c), newStore(t, cs, c), newStore(t, cs, c)}
	var wg sync.WaitGroup
	errs := make(chan error, 30)
	for i := range 30 {
		wg.Go(func() {
			s := replicas[i%len(replicas)]
			if err := s.Revoke(ctx, fmt.Sprintf("jti-%d", i), c.Now().Add(time.Hour)); err != nil {
				errs <- err
			}
			if _, err := s.IsRevoked(ctx, "probe"); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if got := len(getSecret(t, cs).Data); got != 30 {
		t.Fatalf("entries = %d, want 30 (lost updates)", got)
	}
}

func TestCrossReplicaVisibilityAndCacheTTL(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := newClock()
	cs := newClient()
	a := newStore(t, cs, c)
	b := newStore(t, cs, c, WithCacheTTL(5*time.Second))
	if ok, err := b.IsRevoked(ctx, "x"); err != nil || ok {
		t.Fatal("unexpected revocation")
	}
	var gets atomic.Int32
	cs.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		gets.Add(1)
		return false, nil, nil
	})
	if err := a.Revoke(ctx, "x", c.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	gets.Store(0)
	// Within the TTL replica b answers from its cache (no API call) and is stale.
	if ok, _ := b.IsRevoked(ctx, "x"); ok {
		t.Fatal("cache TTL not honored")
	}
	if gets.Load() != 0 {
		t.Fatalf("cache hit made %d API calls", gets.Load())
	}
	c.Advance(5 * time.Second)
	if ok, err := b.IsRevoked(ctx, "x"); err != nil || !ok {
		t.Fatalf("revocation not visible after TTL: %v %v", ok, err)
	}
	if gets.Load() != 1 {
		t.Fatalf("refresh made %d API calls", gets.Load())
	}
	// The writing replica sees its own revocation immediately.
	if ok, _ := a.IsRevoked(ctx, "x"); !ok {
		t.Fatal("writer does not see its own revocation")
	}
}

func TestStaleServingAndFailClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := newClock()
	cs := newClient()
	s := newStore(t, cs, c, WithCacheTTL(5*time.Second), WithMaxStale(30*time.Second))

	// Never loaded + API down: fail closed.
	var down atomic.Bool
	cs.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		if down.Load() {
			return true, nil, apierrors.NewServiceUnavailable("etcd down")
		}
		return false, nil, nil
	})
	down.Store(true)
	if _, err := s.IsRevoked(ctx, "x"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("cold failure: %v", err)
	}
	down.Store(false)
	if err := s.Revoke(ctx, "x", c.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	down.Store(true)
	c.Advance(20 * time.Second) // past TTL, within TTL+maxStale
	if ok, err := s.IsRevoked(ctx, "x"); err != nil || !ok {
		t.Fatalf("stale copy not served: %v %v", ok, err)
	}
	c.Advance(20 * time.Second) // beyond TTL+maxStale
	if _, err := s.IsRevoked(ctx, "x"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("too-stale copy served: %v", err)
	}
}

func TestStoreFull(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := newClock()
	cs := newClient()
	s := newStore(t, cs, c, WithMaxEntries(2))
	for _, id := range []string{"a", "b"} {
		if err := s.Revoke(ctx, id, c.Now().Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Revoke(ctx, "c", c.Now().Add(time.Hour)); !errors.Is(err, auth.ErrRevocationStoreFull) {
		t.Fatalf("full: %v", err)
	}
	// Re-revoking an existing id is still allowed.
	if err := s.Revoke(ctx, "a", c.Now().Add(time.Hour)); err != nil {
		t.Fatalf("existing id: %v", err)
	}
	c.Advance(2 * time.Minute) // "b" expires and is pruned on the next write
	if err := s.Revoke(ctx, "c", c.Now().Add(time.Hour)); err != nil {
		t.Fatalf("after expiry: %v", err)
	}
}

func TestContextCancellation(t *testing.T) {
	t.Parallel()
	c := newClock()
	cs := newClient()
	release := make(chan struct{})
	cs.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		<-release
		return false, nil, nil
	})
	s := newStore(t, cs, c)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := s.IsRevoked(ctx, "x")
	close(release)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

// The store plugs into auth.TokenService: logout on one replica invalidates the session
// on another after the cache TTL.
func TestTokenServiceIntegration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := newClock()
	cs := newClient()
	ks, err := auth.NewKeySet([]byte("0123456789abcdef0123456789abcdef-k"))
	if err != nil {
		t.Fatal(err)
	}
	accounts, _ := auth.AccountsFromMaps(map[string]string{"accounts.alice": "login"}, nil)
	mk := func(store auth.RevocationStore) *auth.TokenService {
		svc, err := auth.NewTokenService(auth.TokenServiceConfig{Keys: ks, Accounts: accounts, Revocations: store, Now: c.Now})
		if err != nil {
			t.Fatal(err)
		}
		return svc
	}
	r1, r2 := mk(newStore(t, cs, c)), mk(newStore(t, cs, c))
	iss, err := r1.IssueLoginSession(ctx, accounts["alice"], time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	p, err := r2.Verify(ctx, iss.Token)
	if err != nil {
		t.Fatal(err)
	}
	if err := r1.Revoke(ctx, p); err != nil {
		t.Fatal(err)
	}
	c.Advance(DefaultCacheTTL)
	if _, err := r2.Verify(ctx, iss.Token); !errors.Is(err, auth.ErrTokenRevoked) {
		t.Fatalf("replica 2 after logout: %v", err)
	}
}
