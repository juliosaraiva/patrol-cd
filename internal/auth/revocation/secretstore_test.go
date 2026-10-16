package revocation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
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

// ref returns a TokenRef issued now on c that expires after ttl.
func ref(c *clock, sub, id string, ttl time.Duration) auth.TokenRef {
	return auth.TokenRef{ID: id, Subject: sub, IssuedAt: c.Now(), ExpiresAt: c.Now().Add(ttl)}
}

// tableOf decodes the stored Secret.
func tableOf(t *testing.T, cs *fake.Clientset) *auth.RevocationTable {
	t.Helper()
	tbl, bad := auth.DecodeRevocationTable(getSecret(t, cs).Data)
	if bad != 0 {
		t.Fatalf("%d malformed entries in stored secret", bad)
	}
	return tbl
}

var secretKeyRE = regexp.MustCompile(`^[-._a-zA-Z0-9]+$`)

func TestRevokeCreatesAndUpdates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := newClock()
	cs := newClient()
	s := newStore(t, cs, c)

	r := ref(c, "alice", "8f14e45f-ceea-467f-a0e6-b6d7e0e8b3a1", time.Hour)
	if ok, err := s.IsRevoked(ctx, r); err != nil || ok {
		t.Fatalf("empty store: %v %v", ok, err)
	}
	if err := s.Revoke(ctx, r); err != nil {
		t.Fatal(err)
	}
	sec := getSecret(t, cs)
	if sec.Labels["app.kubernetes.io/part-of"] != "patrol-cd" || sec.Type != corev1.SecretTypeOpaque {
		t.Fatalf("secret meta = %+v", sec.ObjectMeta)
	}
	want := fmt.Sprintf("%d.%d", r.ExpiresAt.Unix(), r.IssuedAt.Unix())
	for k, v := range sec.Data {
		// Keys are fixed-length hashes: neither the token ID nor the subject is stored.
		if !secretKeyRE.MatchString(k) || len(k) != 41 || strings.Contains(k, "8f14e45f") || string(v) != want {
			t.Fatalf("entry %q = %q", k, v)
		}
	}
	if ok, err := s.IsRevoked(ctx, r); err != nil || !ok {
		t.Fatalf("revoked token not reported: %v %v", ok, err)
	}
	// Long or unusual IDs get keys of the same length.
	for _, id := range []string{"id/with+chars==", strings.Repeat("x", 253), strings.Repeat("y", 1024)} {
		if err := s.Revoke(ctx, ref(c, "alice", id, time.Hour)); err != nil {
			t.Fatal(err)
		}
		if ok, _ := s.IsRevoked(ctx, ref(c, "alice", id, 0)); !ok {
			t.Fatalf("%q not reported", id[:8])
		}
	}
	for k := range getSecret(t, cs).Data {
		if len(k) != 41 {
			t.Fatalf("key %q has length %d", k, len(k))
		}
	}
	// Idempotent; a later expiry extends, an earlier one does not shorten.
	ver := getSecret(t, cs).ResourceVersion
	if err := s.Revoke(ctx, ref(c, "alice", "id/with+chars==", 30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := getSecret(t, cs).ResourceVersion; got != ver {
		t.Fatal("no-op revocation wrote the secret")
	}
	if err := s.Revoke(ctx, ref(c, "alice", "id/with+chars==", 2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	c.Advance(90 * time.Minute)
	if ok, _ := s.IsRevoked(ctx, ref(c, "alice", "id/with+chars==", 0)); !ok {
		t.Fatal("expiry not extended")
	}
	// Sub-second expiries round up. The write also prunes the four expired 1h entries.
	if err := s.Revoke(ctx, ref(c, "bob", "frac", 1500*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if n := len(getSecret(t, cs).Data); n != 2 {
		t.Fatalf("entries after pruning write = %d, want 2", n)
	}
	c.Advance(1999 * time.Millisecond)
	if n, err := s.Prune(ctx); err != nil || n != 0 {
		t.Fatalf("prune before rounded expiry: %d %v", n, err)
	}
	if ok, _ := s.IsRevoked(ctx, ref(c, "bob", "frac", 0)); !ok {
		t.Fatal("fractional expiry rounded down")
	}
	c.Advance(time.Millisecond)
	if n, err := s.Prune(ctx); err != nil || n != 1 {
		t.Fatalf("prune at rounded expiry: %d %v", n, err)
	}
}

func TestRevokeValidationAndNoops(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := newClock()
	cs := newClient()
	s := newStore(t, cs, c)
	if err := s.Revoke(ctx, auth.TokenRef{ID: "x"}); !errors.Is(err, auth.ErrNotRevocable) {
		t.Fatalf("zero exp: %v", err)
	}
	if err := s.Revoke(ctx, ref(c, "s", "", time.Hour)); err == nil {
		t.Fatal("empty id accepted")
	}
	if err := s.Revoke(ctx, ref(c, "s", strings.Repeat("x", 2000), time.Hour)); err == nil {
		t.Fatal("oversized id accepted")
	}
	if err := s.Revoke(ctx, ref(c, "s", "old", -time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CoreV1().Secrets(ns).Get(ctx, DefaultSecretName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("invalid or expired revocation wrote the secret: %v", err)
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
	// Entries written an hour ago: one has expired since.
	old := &clock{t: c.Now().Add(-time.Hour)}
	tbl := auth.NewRevocationTable()
	for _, r := range []auth.TokenRef{ref(old, "u", "expired", 50*time.Minute), ref(old, "u", "live", 2*time.Hour)} {
		if _, err := tbl.Revoke(r, old.Now(), auth.RevocationLimits{}); err != nil {
			t.Fatal(err)
		}
	}
	data := tbl.Data()
	data["garbage"] = []byte("not-a-number")
	data["8f14e45f-ceea-467f-a0e6-b6d7e0e8b3a1"] = []byte(strconv.FormatInt(c.Now().Unix()+600, 10)) // old format
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: DefaultSecretName, Namespace: ns, ResourceVersion: "1"},
		Data:       data,
	}
	cs := newClient(existing)
	s := newStore(t, cs, c)
	if ok, _ := s.IsRevoked(ctx, auth.TokenRef{ID: "live", Subject: "u", IssuedAt: old.Now()}); !ok {
		t.Fatal("pre-existing revocation not reported")
	}
	if err := s.Revoke(ctx, ref(c, "u", "new", time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := len(getSecret(t, cs).Data); got != 2 {
		t.Fatalf("entries after write = %d, want 2 (live + new)", got)
	}
	tbl = tableOf(t, cs)
	if !tbl.IsRevoked(auth.TokenRef{ID: "live", Subject: "u", IssuedAt: old.Now()}, c.Now()) {
		t.Fatal("live entry dropped")
	}
	if tbl.IsRevoked(auth.TokenRef{ID: "expired", Subject: "u", IssuedAt: old.Now()}, c.Now()) {
		t.Fatal("expired entry kept")
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
		if err := s.Revoke(ctx, ref(c, "u", fmt.Sprintf("t%d", i), time.Duration(i+1)*time.Minute)); err != nil {
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
	first, second := ref(c, "u", "first", time.Hour), ref(c, "u", "second", time.Hour)
	if err := s.Revoke(ctx, first); err != nil {
		t.Fatal(err)
	}
	var conflicts atomic.Int32
	cs.PrependReactor("update", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if conflicts.Add(1) <= 2 {
			return true, nil, apierrors.NewConflict(secretsGVR.GroupResource(), DefaultSecretName, errors.New("injected"))
		}
		return false, nil, nil
	})
	if err := s.Revoke(ctx, second); err != nil {
		t.Fatal(err)
	}
	if conflicts.Load() != 3 {
		t.Fatalf("update attempts = %d, want 3", conflicts.Load())
	}
	tbl := tableOf(t, cs)
	if !tbl.IsRevoked(first, c.Now()) || !tbl.IsRevoked(second, c.Now()) {
		t.Fatal("entry lost or missing after conflict retries")
	}
}

func TestCreateRaceRetriesAsUpdate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := newClock()
	cs := newClient()
	s := newStore(t, cs, c)
	theirs, ours := ref(c, "u", "theirs", time.Hour), ref(c, "u", "ours", time.Hour)
	var once sync.Once
	cs.PrependReactor("create", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		handled := false
		once.Do(func() {
			// Another replica creates the secret between our GET and CREATE.
			tbl := auth.NewRevocationTable()
			if _, err := tbl.Revoke(theirs, c.Now(), auth.RevocationLimits{}); err != nil {
				t.Error(err)
			}
			other := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: DefaultSecretName, Namespace: ns, ResourceVersion: "50"},
				Data:       tbl.Data(),
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
	if err := s.Revoke(ctx, ours); err != nil {
		t.Fatal(err)
	}
	tbl := tableOf(t, cs)
	if !tbl.IsRevoked(theirs, c.Now()) {
		t.Fatal("concurrently created entry lost")
	}
	if !tbl.IsRevoked(ours, c.Now()) {
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
	errs := make(chan error, 60)
	for i := range 30 {
		wg.Go(func() {
			s := replicas[i%len(replicas)]
			if err := s.Revoke(ctx, ref(c, fmt.Sprint("user-", i%4), fmt.Sprintf("jti-%d", i), time.Hour)); err != nil {
				errs <- err
			}
			if _, err := s.IsRevoked(ctx, ref(c, "u", "probe", 0)); err != nil {
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
	x := ref(c, "u", "x", time.Hour)
	if ok, err := b.IsRevoked(ctx, x); err != nil || ok {
		t.Fatal("unexpected revocation")
	}
	var gets atomic.Int32
	cs.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		gets.Add(1)
		return false, nil, nil
	})
	if err := a.Revoke(ctx, x); err != nil {
		t.Fatal(err)
	}
	gets.Store(0)
	// Within the TTL replica b answers from its cache (no API call) and is stale.
	if ok, _ := b.IsRevoked(ctx, x); ok {
		t.Fatal("cache TTL not honored")
	}
	if gets.Load() != 0 {
		t.Fatalf("cache hit made %d API calls", gets.Load())
	}
	c.Advance(5 * time.Second)
	if ok, err := b.IsRevoked(ctx, x); err != nil || !ok {
		t.Fatalf("revocation not visible after TTL: %v %v", ok, err)
	}
	if gets.Load() != 1 {
		t.Fatalf("refresh made %d API calls", gets.Load())
	}
	// The writing replica sees its own revocation immediately.
	if ok, _ := a.IsRevoked(ctx, x); !ok {
		t.Fatal("writer does not see its own revocation")
	}
}

func TestStaleServingAndFailClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := newClock()
	cs := newClient()
	s := newStore(t, cs, c, WithCacheTTL(5*time.Second), WithMaxStale(30*time.Second))
	x := ref(c, "u", "x", time.Hour)

	// Never loaded + API down: fail closed.
	var down atomic.Bool
	cs.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		if down.Load() {
			return true, nil, apierrors.NewServiceUnavailable("etcd down")
		}
		return false, nil, nil
	})
	down.Store(true)
	if _, err := s.IsRevoked(ctx, x); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("cold failure: %v", err)
	}
	down.Store(false)
	if err := s.Revoke(ctx, x); err != nil {
		t.Fatal(err)
	}
	down.Store(true)
	c.Advance(20 * time.Second) // past TTL, within TTL+maxStale
	if ok, err := s.IsRevoked(ctx, x); err != nil || !ok {
		t.Fatalf("stale copy not served: %v %v", ok, err)
	}
	c.Advance(20 * time.Second) // beyond TTL+maxStale
	if _, err := s.IsRevoked(ctx, x); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("too-stale copy served: %v", err)
	}
}

func TestStoreFullOnlyWithDistinctSubjects(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := newClock()
	cs := newClient()
	s := newStore(t, cs, c, WithMaxEntries(2))
	if err := s.Revoke(ctx, ref(c, "a", "1", time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke(ctx, ref(c, "b", "1", time.Hour)); err != nil {
		t.Fatal(err)
	}
	ver := getSecret(t, cs).ResourceVersion
	if err := s.Revoke(ctx, ref(c, "c", "1", time.Hour)); !errors.Is(err, auth.ErrRevocationStoreFull) {
		t.Fatalf("full: %v", err)
	}
	if getSecret(t, cs).ResourceVersion != ver {
		t.Fatal("failed revocation wrote the secret")
	}
	// Another token of an existing subject is accepted: that subject is collapsed.
	if err := s.Revoke(ctx, ref(c, "b", "2", time.Hour)); err != nil {
		t.Fatalf("existing subject: %v", err)
	}
	c.Advance(2 * time.Minute) // "a" expires and is pruned on the next write
	if err := s.Revoke(ctx, ref(c, "c", "1", time.Hour)); err != nil {
		t.Fatalf("after expiry: %v", err)
	}
}

// Regression for the reviewed DoS: one account logging in and out repeatedly used to
// fill the Secret so that every other user's logout failed until entries expired.
func TestOneSubjectCannotBlockOtherLogouts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := newClock()
	cs := newClient()
	ks, err := auth.NewKeySet([]byte("0123456789abcdef0123456789abcdef-k"))
	if err != nil {
		t.Fatal(err)
	}
	accounts, _ := auth.AccountsFromMaps(map[string]string{"accounts.mallory": "login", "accounts.bob": "login"}, nil)
	store := newStore(t, cs, c, WithMaxEntries(20), WithMaxEntriesPerSubject(5))
	svc, err := auth.NewTokenService(auth.TokenServiceConfig{Keys: ks, Accounts: accounts, Revocations: store, Now: c.Now})
	if err != nil {
		t.Fatal(err)
	}
	logout := func(acct string) string {
		t.Helper()
		iss, err := svc.IssueLoginSession(ctx, accounts[acct], time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		p, err := svc.Verify(ctx, iss.Token)
		if err != nil {
			t.Fatal(err)
		}
		if err := svc.Revoke(ctx, p); err != nil {
			t.Fatalf("%s logout: %v", acct, err)
		}
		return iss.Token
	}
	var stormed []string
	for range 60 {
		stormed = append(stormed, logout("mallory"))
		c.Advance(time.Second)
	}
	bobTok := logout("bob")
	if _, err := svc.Verify(ctx, bobTok); !errors.Is(err, auth.ErrTokenRevoked) {
		t.Fatalf("bob's logged-out session: %v", err)
	}
	for i, tok := range stormed {
		if _, err := svc.Verify(ctx, tok); !errors.Is(err, auth.ErrTokenRevoked) {
			t.Fatalf("mallory's session %d accepted again: %v", i, err)
		}
	}
	if n := len(getSecret(t, cs).Data); n > 20 {
		t.Fatalf("secret holds %d entries", n)
	}
}

// The default limits keep the Secret far below the API server limits (1 MiB of Secret
// data, etcd's default 1.5 MiB request) in both protobuf and JSON encodings, for the
// realistic (10-digit timestamps, entry limit binds) and worst case (19-digit values,
// byte limit binds); a write to a store at its limits stays within them.
func TestSecretSizeAtDefaultLimits(t *testing.T) {
	t.Parallel()
	tag := func(prefix string, i, n int) string {
		s := fmt.Sprintf("%s%0*d", prefix, n-len(prefix), i)
		return s[len(s)-n:]
	}
	for name, tc := range map[string]struct{ value string }{
		"realistic":  {"1760317200.1760230800"},
		"worst case": {"4611686018427387904.4611686018427387903"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			data := map[string][]byte{}
			size := 0
			for i := 0; ; i++ {
				// Four tokens per subject; key layout as written by auth.RevocationTable.
				k := "t." + tag("s", i/4, 16) + "." + tag("t", i, 22)
				if len(data)+1 > auth.DefaultRevocationMaxEntries || size+len(k)+len(tc.value) > auth.DefaultRevocationMaxBytes {
					break
				}
				data[k] = []byte(tc.value)
				size += len(k) + len(tc.value)
			}
			tbl, bad := auth.DecodeRevocationTable(data)
			if bad != 0 || tbl.Size() != size || tbl.Len() != len(data) {
				t.Fatalf("decode: bad %d size %d/%d len %d/%d", bad, tbl.Size(), size, tbl.Len(), len(data))
			}
			sec := &corev1.Secret{
				TypeMeta:   metav1.TypeMeta{Kind: "Secret", APIVersion: "v1"},
				ObjectMeta: metav1.ObjectMeta{Name: DefaultSecretName, Namespace: ns, ResourceVersion: "1"},
				Type:       corev1.SecretTypeOpaque,
				Data:       data,
			}
			pb, err := sec.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			js, err := json.Marshal(sec)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%d entries: %d key+value bytes, protobuf %d bytes, JSON %d bytes", len(data), size, len(pb), len(js))
			if len(pb) > 1<<20 || len(js) > 1<<20 || size > 1<<20 {
				t.Fatalf("secret too large: protobuf %d, JSON %d, data %d", len(pb), len(js), size)
			}

			// A new principal's logout against the full Secret succeeds and stays bounded.
			c := newClock()
			cs := newClient(sec)
			s := newStore(t, cs, c)
			newcomer := ref(c, "newcomer", "jti", time.Hour)
			if err := s.Revoke(context.Background(), newcomer); err != nil {
				t.Fatalf("revoke at limits: %v", err)
			}
			after := tableOf(t, cs)
			if after.Len() > auth.DefaultRevocationMaxEntries || after.Size() > auth.DefaultRevocationMaxBytes {
				t.Fatalf("after write: %d entries, %d bytes", after.Len(), after.Size())
			}
			if !after.IsRevoked(newcomer, c.Now()) {
				t.Fatal("newcomer not revoked")
			}
		})
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
	_, err := s.IsRevoked(ctx, ref(c, "u", "x", 0))
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
