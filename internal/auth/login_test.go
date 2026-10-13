package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

type loginEnv struct {
	clock    *fakeClock
	accounts Accounts
	auth     *Authenticator
	mu       sync.Mutex
	sleeps   []time.Duration
}

func newLoginEnv(t *testing.T, cfg LoginConfig) *loginEnv {
	t.Helper()
	pw := mustHash(t, "correct-password")
	cm := map[string]string{
		"accounts.alice":       "login, apiKey",
		"accounts.ci":          "apiKey",
		"accounts.bob":         "login",
		"accounts.bob.enabled": "false",
		"accounts.nopass":      "login",
	}
	secret := map[string]string{
		KeyAdminPassword:          pw,
		"accounts.alice.password": pw,
		"accounts.ci.password":    pw,
		"accounts.bob.password":   pw,
	}
	accounts, err := AccountsFromMaps(cm, secret)
	if err != nil {
		t.Fatal(err)
	}
	e := &loginEnv{clock: newFakeClock(), accounts: accounts}
	e.auth, err = NewAuthenticator(accounts, cfg,
		WithLoginClock(e.clock.Now),
		WithLoginLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		WithLoginSleeper(func(_ context.Context, d time.Duration) {
			e.mu.Lock()
			e.sleeps = append(e.sleeps, d)
			e.mu.Unlock()
		}))
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func noDelay() LoginConfig {
	c := DefaultLoginConfig()
	c.DelayMin, c.DelayMax = 0, 0
	return c
}

func loginReason(err error) LoginFailureReason {
	var le *LoginError
	if errors.As(err, &le) {
		return le.Reason
	}
	return ""
}

func TestAuthenticate(t *testing.T) {
	t.Parallel()
	e := newLoginEnv(t, noDelay())
	ctx := context.Background()
	tests := []struct {
		name       string
		user, pass string
		wantErr    error
		wantReason LoginFailureReason
	}{
		{name: "admin ok", user: "admin", pass: "correct-password"},
		{name: "local ok", user: "alice", pass: "correct-password"},
		{name: "wrong password", user: "alice", pass: "nope", wantErr: ErrInvalidCredentials, wantReason: ReasonBadPassword},
		{name: "unknown user", user: "mallory", pass: "correct-password", wantErr: ErrInvalidCredentials, wantReason: ReasonUnknownUser},
		{name: "no password set", user: "nopass", pass: "correct-password", wantErr: ErrInvalidCredentials, wantReason: ReasonBadPassword},
		{name: "blank password", user: "alice", pass: "", wantErr: ErrNoCredentials},
		{name: "blank user", user: "", pass: "x", wantErr: ErrNoCredentials},
		{name: "username too long", user: strings.Repeat("u", MaxUsernameLength+1), pass: "x", wantErr: ErrUsernameTooLong},
		{name: "disabled account", user: "bob", pass: "correct-password", wantErr: ErrAccountDisabled},
		{name: "login capability missing", user: "ci", pass: "correct-password", wantErr: ErrMissingCapability},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			acct, err := e.auth.Authenticate(ctx, tc.user, tc.pass)
			if tc.wantErr == nil {
				if err != nil || acct.Name != tc.user {
					t.Fatalf("Authenticate = %+v, %v", acct, err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if got := loginReason(err); got != tc.wantReason {
				t.Fatalf("reason = %q, want %q", got, tc.wantReason)
			}
			if errors.Is(err, ErrInvalidCredentials) && err.Error() != ErrInvalidCredentials.Error() {
				t.Fatalf("client-visible message leaks detail: %q", err.Error())
			}
		})
	}
}

func TestLockoutAndReset(t *testing.T) {
	t.Parallel()
	cfg := noDelay()
	cfg.MaxFailures = 3
	cfg.FailureWindow = 300 * time.Second
	e := newLoginEnv(t, cfg)
	ctx := context.Background()

	for i := 1; i <= 3; i++ {
		if _, err := e.auth.Authenticate(ctx, "alice", "bad"); loginReason(err) != ReasonBadPassword {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if n := e.auth.FailureCount("alice"); n != 3 {
		t.Fatalf("failures = %d", n)
	}
	// Locked: even the right password is rejected, and the count does not grow.
	_, err := e.auth.Authenticate(ctx, "alice", "correct-password")
	if loginReason(err) != ReasonLockedOut || !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("locked: %v", err)
	}
	if n := e.auth.FailureCount("alice"); n != 3 {
		t.Fatalf("failures while locked = %d", n)
	}
	// Other users are unaffected.
	if _, err := e.auth.Authenticate(ctx, "admin", "correct-password"); err != nil {
		t.Fatalf("admin: %v", err)
	}
	// Still locked at the edge of the window, unlocked after it.
	e.clock.Advance(300 * time.Second)
	if _, err := e.auth.Authenticate(ctx, "alice", "correct-password"); loginReason(err) != ReasonLockedOut {
		t.Fatalf("at window edge: %v", err)
	}
	e.clock.Advance(time.Second)
	if _, err := e.auth.Authenticate(ctx, "alice", "correct-password"); err != nil {
		t.Fatalf("after window: %v", err)
	}
	if n := e.auth.FailureCount("alice"); n != 0 {
		t.Fatalf("success did not reset: %d", n)
	}

	// A success below the threshold resets the counter.
	_, _ = e.auth.Authenticate(ctx, "alice", "bad")
	_, _ = e.auth.Authenticate(ctx, "alice", "bad")
	if _, err := e.auth.Authenticate(ctx, "alice", "correct-password"); err != nil {
		t.Fatal(err)
	}
	if n := e.auth.FailureCount("alice"); n != 0 {
		t.Fatalf("failures after success = %d", n)
	}
	// Unknown users are tracked too (no user enumeration through lockout behavior).
	for range 3 {
		_, _ = e.auth.Authenticate(ctx, "ghost", "x")
	}
	if _, err := e.auth.Authenticate(ctx, "ghost", "x"); loginReason(err) != ReasonLockedOut {
		t.Fatalf("unknown user lockout: %v", err)
	}
}

func TestLockoutWithoutWindow(t *testing.T) {
	t.Parallel()
	cfg := noDelay()
	cfg.MaxFailures = 2
	cfg.FailureWindow = 0
	e := newLoginEnv(t, cfg)
	ctx := context.Background()
	_, _ = e.auth.Authenticate(ctx, "alice", "bad")
	_, _ = e.auth.Authenticate(ctx, "alice", "bad")
	e.clock.Advance(365 * 24 * time.Hour)
	if _, err := e.auth.Authenticate(ctx, "alice", "correct-password"); loginReason(err) != ReasonLockedOut {
		t.Fatalf("window 0 should lock until eviction/restart: %v", err)
	}
}

func TestFailureCacheBoundProtectsAdmin(t *testing.T) {
	t.Parallel()
	cfg := noDelay()
	cfg.MaxCacheSize = 5
	cfg.MaxFailures = 3
	e := newLoginEnv(t, cfg)
	ctx := context.Background()
	_, _ = e.auth.Authenticate(ctx, "admin", "bad")
	_, _ = e.auth.Authenticate(ctx, "admin", "bad")
	for i := range 200 {
		_, _ = e.auth.Authenticate(ctx, fmt.Sprintf("flood-%d", i), "x")
	}
	if n := e.auth.TrackedUsers(); n > 5 {
		t.Fatalf("cache grew to %d entries", n)
	}
	if n := e.auth.FailureCount("admin"); n != 2 {
		t.Fatalf("admin failure count reset by flooding: %d", n)
	}
	// Expired entries are dropped when recording.
	e.clock.Advance(DefaultLoginFailureWindow + time.Second)
	_, _ = e.auth.Authenticate(ctx, "late", "x")
	if n := e.auth.TrackedUsers(); n != 1 {
		t.Fatalf("expired entries not pruned: %d", n)
	}
}

func TestConcurrencyLimit(t *testing.T) {
	t.Parallel()
	cfg := DefaultLoginConfig()
	cfg.MaxConcurrent = 1
	cfg.DelayMin, cfg.DelayMax = time.Second, time.Second
	release := make(chan struct{})
	entered := make(chan struct{})
	accounts := Accounts{}
	a, err := NewAuthenticator(accounts, cfg,
		WithLoginLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		WithLoginSleeper(func(context.Context, time.Duration) {
			close(entered)
			<-release
		}))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error)
	go func() {
		_, err := a.Authenticate(context.Background(), "u1", "p")
		done <- err
	}()
	<-entered
	_, err = a.Authenticate(context.Background(), "u2", "p")
	if loginReason(err) != ReasonConcurrencyLimit || !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("second concurrent login: %v", err)
	}
	close(release)
	if err := <-done; loginReason(err) != ReasonUnknownUser {
		t.Fatalf("first login: %v", err)
	}
}

func TestTimingEqualizationDelay(t *testing.T) {
	t.Parallel()
	cfg := noDelay()
	cfg.DelayMin, cfg.DelayMax = 500*time.Millisecond, 500*time.Millisecond
	e := newLoginEnv(t, cfg)
	ctx := context.Background()
	_, _ = e.auth.Authenticate(ctx, "alice", "correct-password") // success
	_, _ = e.auth.Authenticate(ctx, "alice", "wrong")            // bad password
	_, _ = e.auth.Authenticate(ctx, "nobody", "wrong")           // unknown user
	_, _ = e.auth.Authenticate(ctx, "nopass", "wrong")           // no password
	e.mu.Lock()
	defer e.mu.Unlock()
	// The clock is frozen, so every outcome requests the same full delay.
	want := []time.Duration{500 * time.Millisecond, 500 * time.Millisecond, 500 * time.Millisecond, 500 * time.Millisecond}
	if diff := cmp.Diff(want, e.sleeps); diff != "" {
		t.Fatalf("delays (-want +got):\n%s", diff)
	}
}

func TestTimingEqualizationRandomRange(t *testing.T) {
	t.Parallel()
	e := newLoginEnv(t, DefaultLoginConfig())
	for range 20 {
		_, _ = e.auth.Authenticate(context.Background(), "nobody", "x")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, d := range e.sleeps {
		if d < DefaultLoginDelayMin || d >= DefaultLoginDelayMax {
			t.Fatalf("delay %v outside [%v,%v)", d, DefaultLoginDelayMin, DefaultLoginDelayMax)
		}
	}
}

func TestConcurrentFailuresAreSerialized(t *testing.T) {
	t.Parallel()
	cfg := noDelay()
	cfg.MaxFailures = 5
	e := newLoginEnv(t, cfg)
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() { _, _ = e.auth.Authenticate(context.Background(), "alice", "bad") })
	}
	wg.Wait()
	// Per-user serialization means attempts after the 5th see the lockout and do not
	// increment the counter.
	if n := e.auth.FailureCount("alice"); n != 5 {
		t.Fatalf("failures = %d, want exactly 5", n)
	}
}

func TestAuthenticatorLookupError(t *testing.T) {
	t.Parallel()
	boom := errors.New("settings unavailable")
	a, err := NewAuthenticator(AccountLookupFunc(func(context.Context, string) (Account, error) {
		return Account{}, boom
	}), noDelay(), WithLoginLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.Authenticate(context.Background(), "alice", "pw")
	if !errors.Is(err, boom) || errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("err = %v", err)
	}
	if a.FailureCount("alice") != 0 {
		t.Fatal("infrastructure error counted as login failure")
	}
	if _, err := NewAuthenticator(nil, noDelay()); err == nil {
		t.Fatal("nil lookup accepted")
	}
}

func TestLoginConfigFromEnv(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		env     map[string]string
		want    LoginConfig
		wantErr bool
	}{
		{name: "defaults", want: DefaultLoginConfig()},
		{
			name: "overrides",
			env: map[string]string{
				EnvLoginMaxFailCount: "10", EnvLoginFailureWindowSecs: "0",
				EnvLoginMaxCacheSize: "50", EnvMaxConcurrentLogins: "0",
			},
			want: func() LoginConfig {
				c := DefaultLoginConfig()
				c.MaxFailures, c.FailureWindow, c.MaxCacheSize, c.MaxConcurrent = 10, 0, 50, 0
				return c
			}(),
		},
		{
			name:    "invalid values keep defaults",
			env:     map[string]string{EnvLoginMaxFailCount: "0", EnvLoginFailureWindowSecs: "-1", EnvLoginMaxCacheSize: "lots", EnvMaxConcurrentLogins: "9999999999"},
			want:    DefaultLoginConfig(),
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := LoginConfigFromEnv(func(k string) (string, bool) { v, ok := tc.env[k]; return v, ok })
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}
