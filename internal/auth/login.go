package auth

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math"
	"math/rand/v2"
	"strconv"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Login throttling defaults (upstream ARGOCD_SESSION_* defaults; the documented cache
// size of 1000 is used).
const (
	DefaultMaxLoginFailures   = 5
	DefaultLoginFailureWindow = 300 * time.Second
	DefaultLoginCacheSize     = 1000
	DefaultMaxConcurrentLogin = 50
	DefaultLoginDelayMin      = 500 * time.Millisecond
	DefaultLoginDelayMax      = 1000 * time.Millisecond

	// Environment variables read by LoginConfigFromEnv.
	EnvLoginMaxFailCount      = "PATROL_SESSION_FAILURE_MAX_FAIL_COUNT"
	EnvLoginFailureWindowSecs = "PATROL_SESSION_FAILURE_WINDOW_SECONDS"
	EnvLoginMaxCacheSize      = "PATROL_SESSION_MAX_CACHE_SIZE"
	EnvMaxConcurrentLogins    = "PATROL_MAX_CONCURRENT_LOGIN_REQUESTS_COUNT"

	loginLockStripes = 256
)

// LoginConfig configures an Authenticator. Start from DefaultLoginConfig.
type LoginConfig struct {
	// MaxFailures is the number of consecutive failures after which further attempts
	// are rejected without checking the password, while inside FailureWindow.
	MaxFailures int
	// FailureWindow is how long a failure counts. 0 disables expiry: once MaxFailures is
	// reached the user stays locked out until the entry is evicted or the process
	// restarts (upstream semantics).
	FailureWindow time.Duration
	// MaxCacheSize bounds the number of tracked usernames.
	MaxCacheSize int
	// MaxConcurrent bounds concurrent login attempts per process; 0 disables the limit.
	MaxConcurrent int
	// DelayMin/DelayMax bound the randomized minimum response time of every attempt
	// (timing equalization). DelayMax <= 0 disables the delay.
	DelayMin time.Duration
	DelayMax time.Duration
}

// DefaultLoginConfig returns the upstream-equivalent defaults.
func DefaultLoginConfig() LoginConfig {
	return LoginConfig{
		MaxFailures:   DefaultMaxLoginFailures,
		FailureWindow: DefaultLoginFailureWindow,
		MaxCacheSize:  DefaultLoginCacheSize,
		MaxConcurrent: DefaultMaxConcurrentLogin,
		DelayMin:      DefaultLoginDelayMin,
		DelayMax:      DefaultLoginDelayMax,
	}
}

// LoginConfigFromEnv returns DefaultLoginConfig overridden by the PATROL_SESSION_* and
// PATROL_MAX_CONCURRENT_LOGIN_REQUESTS_COUNT variables. Invalid values keep the default
// and are reported in the returned error; the config is always usable. lookup is
// typically os.LookupEnv.
func LoginConfigFromEnv(lookup func(string) (string, bool)) (LoginConfig, error) {
	cfg := DefaultLoginConfig()
	var errs []error
	num := func(key string, minVal int, dst *int) {
		v, ok := lookup(key)
		if !ok || v == "" {
			return
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < minVal || n > math.MaxInt32 {
			errs = append(errs, fmt.Errorf("%s=%q: must be an integer in [%d, %d]", key, v, minVal, math.MaxInt32))
			return
		}
		*dst = n
	}
	num(EnvLoginMaxFailCount, 1, &cfg.MaxFailures)
	num(EnvLoginMaxCacheSize, 1, &cfg.MaxCacheSize)
	num(EnvMaxConcurrentLogins, 0, &cfg.MaxConcurrent)
	window := int(cfg.FailureWindow / time.Second)
	num(EnvLoginFailureWindowSecs, 0, &window)
	cfg.FailureWindow = time.Duration(window) * time.Second
	return cfg, errors.Join(errs...)
}

func (c LoginConfig) normalized() LoginConfig {
	d := DefaultLoginConfig()
	if c.MaxFailures < 1 {
		c.MaxFailures = d.MaxFailures
	}
	if c.MaxCacheSize < 1 {
		c.MaxCacheSize = d.MaxCacheSize
	}
	if c.FailureWindow < 0 {
		c.FailureWindow = 0
	}
	if c.MaxConcurrent < 0 {
		c.MaxConcurrent = 0
	}
	if c.DelayMax <= 0 {
		c.DelayMin, c.DelayMax = 0, 0
	}
	c.DelayMin = max(0, min(c.DelayMin, c.DelayMax))
	return c
}

// LoginFailureReason is the internal reason for a failed login. Only use it for logs
// and metrics; never return it to the client.
type LoginFailureReason string

const (
	ReasonUnknownUser      LoginFailureReason = "unknown_user"
	ReasonBadPassword      LoginFailureReason = "bad_password"
	ReasonLockedOut        LoginFailureReason = "locked_out"
	ReasonConcurrencyLimit LoginFailureReason = "concurrency_limit"
)

// LoginError is returned for login failures that must look identical to the client.
// It matches ErrInvalidCredentials with errors.Is.
type LoginError struct {
	Reason LoginFailureReason
}

func (e *LoginError) Error() string { return ErrInvalidCredentials.Error() }

// Is reports whether target is ErrInvalidCredentials.
func (e *LoginError) Is(target error) bool { return target == ErrInvalidCredentials }

type loginAttempts struct {
	failCount  int
	lastFailed time.Time
}

// failureTracker is a bounded per-username failure counter.
type failureTracker struct {
	mu      sync.Mutex
	entries map[string]loginAttempts
	cfg     LoginConfig
	now     func() time.Time
	logger  *slog.Logger
}

func (f *failureTracker) lockedOut(user string) (bool, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.entries[user]
	if !ok || a.failCount < f.cfg.MaxFailures {
		return false, a.failCount
	}
	inWindow := f.cfg.FailureWindow == 0 || f.now().Sub(a.lastFailed) <= f.cfg.FailureWindow
	return inWindow, a.failCount
}

func (f *failureTracker) recordFailure(user string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	f.expireLocked(now)
	if _, ok := f.entries[user]; !ok && len(f.entries) >= f.cfg.MaxCacheSize {
		f.evictLocked(user)
	}
	a := f.entries[user]
	a.failCount++
	a.lastFailed = now
	f.entries[user] = a
	return a.failCount
}

func (f *failureTracker) reset(user string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.entries, user)
}

func (f *failureTracker) count(user string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.entries[user].failCount
}

func (f *failureTracker) size() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.entries)
}

func (f *failureTracker) expireLocked(now time.Time) {
	if f.cfg.FailureWindow == 0 {
		return
	}
	for k, a := range f.entries {
		if now.Sub(a.lastFailed) > f.cfg.FailureWindow {
			delete(f.entries, k)
		}
	}
}

// evictLocked removes one random entry other than the admin account and the user being
// recorded, so that flooding the cache with fake usernames cannot reset the admin's
// failure count.
func (f *failureTracker) evictLocked(current string) {
	candidates := make([]string, 0, len(f.entries))
	for k := range f.entries {
		if k != AdminUsername && k != current {
			candidates = append(candidates, k)
		}
	}
	if len(candidates) == 0 {
		return
	}
	victim := candidates[rand.IntN(len(candidates))] //nolint:gosec // eviction choice, not security sensitive
	delete(f.entries, victim)
	f.logger.Warn("login failure cache full, evicted entry", "maxCacheSize", f.cfg.MaxCacheSize)
}

// Authenticator verifies local username/password logins with upstream-equivalent
// throttling: per-username failure counting with a window and lockout, a bounded failure
// cache that protects the admin entry, a global concurrency limit, per-username
// serialization, and timing equalization (a dummy bcrypt comparison for unknown users or
// accounts without a password, plus a randomized minimum response time).
type Authenticator struct {
	accounts  AccountLookup
	cfg       LoginConfig
	failures  *failureTracker
	stripes   [loginLockStripes]sync.Mutex
	sem       chan struct{}
	dummyHash []byte
	logger    *slog.Logger
	now       func() time.Time
	sleep     func(ctx context.Context, d time.Duration)
}

// AuthenticatorOption customizes an Authenticator.
type AuthenticatorOption func(*Authenticator)

// WithLoginLogger sets the logger (default slog.Default()).
func WithLoginLogger(l *slog.Logger) AuthenticatorOption {
	return func(a *Authenticator) { a.logger = l }
}

// WithLoginClock sets the time source.
func WithLoginClock(now func() time.Time) AuthenticatorOption {
	return func(a *Authenticator) { a.now = now }
}

// WithLoginSleeper replaces the delay function used for timing equalization.
func WithLoginSleeper(sleep func(ctx context.Context, d time.Duration)) AuthenticatorOption {
	return func(a *Authenticator) { a.sleep = sleep }
}

// NewAuthenticator returns an Authenticator using accounts for lookups.
func NewAuthenticator(accounts AccountLookup, cfg LoginConfig, opts ...AuthenticatorOption) (*Authenticator, error) {
	if accounts == nil {
		return nil, errors.New("authenticator: account lookup is required")
	}
	a := &Authenticator{
		accounts: accounts,
		cfg:      cfg.normalized(),
		logger:   slog.Default(),
		now:      time.Now,
		sleep:    sleepCtx,
	}
	for _, o := range opts {
		o(a)
	}
	// A real bcrypt hash at the production cost so that comparing against it takes as
	// long as comparing against a stored hash.
	h, err := bcrypt.GenerateFromPassword([]byte("patrol-timing-equalization"), minBcryptCost)
	if err != nil {
		return nil, fmt.Errorf("authenticator: %w", err)
	}
	a.dummyHash = h
	if a.cfg.MaxConcurrent > 0 {
		a.sem = make(chan struct{}, a.cfg.MaxConcurrent)
	}
	a.failures = &failureTracker{entries: map[string]loginAttempts{}, cfg: a.cfg, now: a.now, logger: a.logger}
	return a, nil
}

// Authenticate verifies username/password and returns the account. Failures that must not
// reveal whether the account exists return *LoginError (errors.Is ErrInvalidCredentials).
// After a correct password, ErrAccountDisabled and ErrMissingCapability may be returned.
// Errors from the AccountLookup other than ErrAccountNotFound are returned wrapped.
func (a *Authenticator) Authenticate(ctx context.Context, username, password string) (Account, error) {
	if a.sem != nil {
		select {
		case a.sem <- struct{}{}:
			defer func() { <-a.sem }()
		default:
			a.logger.Warn("exceeded number of concurrent login requests")
			return Account{}, &LoginError{Reason: ReasonConcurrencyLimit}
		}
	}
	if username == "" || password == "" {
		return Account{}, ErrNoCredentials
	}
	if len(username) > MaxUsernameLength {
		return Account{}, fmt.Errorf("%w (%d bytes max)", ErrUsernameTooLong, MaxUsernameLength)
	}

	unlock := a.lockUser(username)
	defer unlock()

	start := a.now()
	if a.cfg.DelayMax > 0 {
		defer func() {
			d := a.cfg.DelayMin
			if span := a.cfg.DelayMax - a.cfg.DelayMin; span > 0 {
				d += rand.N(span) //nolint:gosec // jitter, not security sensitive
			}
			if rem := d - a.now().Sub(start); rem > 0 {
				a.sleep(ctx, rem)
			}
		}()
	}

	if locked, n := a.failures.lockedOut(username); locked {
		// The username is not logged here: it may not be an account name (users
		// sometimes type their password into the username field).
		a.logger.Warn("login rejected: too many failed attempts", "failures", n)
		return Account{}, &LoginError{Reason: ReasonLockedOut}
	}

	acct, err := a.accounts.LookupAccount(ctx, username)
	if err != nil {
		_ = bcrypt.CompareHashAndPassword(a.dummyHash, []byte(password))
		if errors.Is(err, ErrAccountNotFound) {
			n := a.failures.recordFailure(username)
			a.logger.Warn("login failed: unknown user", "failures", n)
			return Account{}, &LoginError{Reason: ReasonUnknownUser}
		}
		return Account{}, fmt.Errorf("look up account: %w", err)
	}

	hash := []byte(acct.PasswordHash)
	if _, costErr := bcrypt.Cost(hash); costErr != nil {
		// No (valid) password set: still spend the time of a real comparison.
		_ = bcrypt.CompareHashAndPassword(a.dummyHash, []byte(password))
		hash = nil
	}
	if hash == nil || bcrypt.CompareHashAndPassword(hash, []byte(password)) != nil {
		n := a.failures.recordFailure(username)
		a.logger.Warn("login failed", "username", username, "failures", n)
		return Account{}, &LoginError{Reason: ReasonBadPassword}
	}
	if err := checkAccount(acct, CapabilityLogin); err != nil {
		return Account{}, err
	}
	a.failures.reset(username)
	return acct, nil
}

// FailureCount returns the current failure count for username.
func (a *Authenticator) FailureCount(username string) int { return a.failures.count(username) }

// TrackedUsers returns the number of usernames in the failure cache.
func (a *Authenticator) TrackedUsers() int { return a.failures.size() }

func (a *Authenticator) lockUser(username string) func() {
	h := fnv.New32a()
	_, _ = h.Write([]byte(username))
	mu := &a.stripes[h.Sum32()%loginLockStripes]
	mu.Lock()
	return mu.Unlock
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
