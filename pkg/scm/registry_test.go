package scm_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
)

type versioned struct{ version string }

func (v versioned) Info() scm.Info { return scm.Info{Type: "v", ContractVersion: v.version} }

func TestRegistry(t *testing.T) {
	t.Parallel()
	r := scm.NewRegistry()
	ok := func(context.Context, scm.Config) (scm.Provider, error) { return versioned{scm.ContractVersion}, nil }
	old := func(context.Context, scm.Config) (scm.Provider, error) { return versioned{"v0"}, nil }
	boom := errors.New("boom")
	failing := func(context.Context, scm.Config) (scm.Provider, error) { return nil, boom }

	tests := []struct {
		typ     string
		f       scm.Factory
		wantErr bool
	}{
		{"github", ok, false},
		{"GitHub", ok, true}, // case-insensitive duplicate
		{"", ok, true},
		{"nilfactory", nil, true},
		{"old", old, false},
		{"failing", failing, false},
	}
	for _, tc := range tests {
		if err := r.Register(tc.typ, tc.f); (err != nil) != tc.wantErr {
			t.Errorf("Register(%q) err = %v, wantErr %v", tc.typ, err, tc.wantErr)
		}
	}
	if got := r.Types(); !slices.Equal(got, []string{"failing", "github", "old"}) {
		t.Fatalf("Types = %v", got)
	}
	if _, err := r.New(t.Context(), scm.Config{Type: " GITHUB "}); err != nil {
		t.Fatalf("New(github): %v", err)
	}
	if _, err := r.New(t.Context(), scm.Config{Type: "old"}); scm.KindOf(err) != scm.KindInvalid {
		t.Fatalf("contract mismatch must be rejected: %v", err)
	}
	if _, err := r.New(t.Context(), scm.Config{Type: "failing"}); !errors.Is(err, boom) {
		t.Fatalf("factory error: %v", err)
	}
	if _, err := r.New(t.Context(), scm.Config{Type: "gitlab"}); !errors.Is(err, scm.ErrUnknownProviderType) {
		t.Fatalf("unknown type: %v", err)
	}
}

func TestRegistryConcurrent(t *testing.T) {
	t.Parallel()
	r := scm.NewRegistry()
	f := func(context.Context, scm.Config) (scm.Provider, error) { return versioned{scm.ContractVersion}, nil }
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() { errs <- r.Register("same", f) })
	}
	wg.Wait()
	close(errs)
	okCount := 0
	for err := range errs {
		if err == nil {
			okCount++
		}
	}
	if okCount != 1 {
		t.Fatalf("exactly one registration must win, got %d", okCount)
	}
}

func TestConfigDefaults(t *testing.T) {
	t.Parallel()
	c := scm.Config{}.WithDefaults()
	if c.MaxPages != scm.DefaultMaxPages || c.MaxResponseBytes != scm.DefaultMaxResponseBytes ||
		c.RateLimit.RPS != scm.DefaultRateLimitRPS || c.RateLimit.Burst != scm.DefaultRateLimitBurst ||
		c.Retry.MaxAttempts != 4 || c.Retry.MaxWait == 0 || c.Logger == nil || c.Clock == nil || c.UserAgent == "" || c.Timeout == 0 {
		t.Fatalf("defaults not applied: %+v", c)
	}
	custom := scm.Config{MaxPages: 3, RateLimit: scm.RateLimit{RPS: -1}, Retry: scm.RetryPolicy{MaxAttempts: 1}}.WithDefaults()
	if custom.MaxPages != 3 || custom.RateLimit.RPS != -1 || custom.Retry.MaxAttempts != 1 {
		t.Fatalf("explicit values overridden: %+v", custom)
	}
}

func TestRetryPolicyCallBudget(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		policy  scm.RetryPolicy
		attempt time.Duration
		want    time.Duration
	}{
		{"defaults", scm.RetryPolicy{}, scm.DefaultRequestTimeout, 4*scm.DefaultRequestTimeout + 3*time.Minute},
		{"single attempt", scm.RetryPolicy{MaxAttempts: 1}, 10 * time.Second, 10 * time.Second},
		{"MaxDelay above MaxWait", scm.RetryPolicy{MaxAttempts: 2, MaxDelay: 2 * time.Minute, MaxWait: time.Second}, time.Second, 2*time.Second + 2*time.Minute},
		{"no attempt timeout", scm.RetryPolicy{MaxAttempts: 3, MaxWait: 5 * time.Second}, 0, 2 * 10 * time.Second},
	}
	for _, tc := range tests {
		if got := tc.policy.CallBudget(tc.attempt); got != tc.want {
			t.Errorf("%s: CallBudget = %v, want %v", tc.name, got, tc.want)
		}
		p := tc.policy.WithDefaults()
		if got := tc.policy.CallBudget(tc.attempt); p.MaxAttempts > 1 && got < p.MaxWait+tc.attempt {
			t.Errorf("%s: budget %v cannot cover one advised wait plus an attempt", tc.name, got)
		}
	}
}
