package scm_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
)

func TestErrorKinds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		kind      scm.ErrorKind
		sentinel  error
		retryable bool
	}{
		{scm.KindNotFound, scm.ErrNotFound, false},
		{scm.KindUnauthorized, scm.ErrUnauthorized, false},
		{scm.KindForbidden, scm.ErrForbidden, false},
		{scm.KindRateLimited, scm.ErrRateLimited, true},
		{scm.KindUnavailable, scm.ErrUnavailable, true},
		{scm.KindInvalid, scm.ErrInvalid, false},
		{scm.KindConflict, scm.ErrConflict, true},
	}
	cause := errors.New("cause")
	all := []error{scm.ErrNotFound, scm.ErrUnauthorized, scm.ErrForbidden, scm.ErrRateLimited, scm.ErrUnavailable, scm.ErrInvalid, scm.ErrConflict}
	for _, tc := range tests {
		t.Run(string(tc.kind), func(t *testing.T) {
			t.Parallel()
			err := fmt.Errorf("wrapped: %w", scm.NewError(tc.kind, "github", "Op", cause))
			if !errors.Is(err, tc.sentinel) {
				t.Fatalf("errors.Is(%v, %v) = false", err, tc.sentinel)
			}
			for _, other := range all {
				if other != tc.sentinel && errors.Is(err, other) {
					t.Fatalf("%v unexpectedly matches %v", err, other)
				}
			}
			if !errors.Is(err, cause) {
				t.Fatal("cause not reachable through Unwrap")
			}
			if scm.KindOf(err) != tc.kind {
				t.Fatalf("KindOf = %q", scm.KindOf(err))
			}
			if scm.IsRetryable(err) != tc.retryable {
				t.Fatalf("IsRetryable = %v", scm.IsRetryable(err))
			}
		})
	}
	if scm.KindOf(errors.New("plain")) != scm.KindUnknown || scm.IsNotFound(nil) {
		t.Fatal("plain errors must be KindUnknown")
	}
}

func TestErrorMessageAndRetryAfter(t *testing.T) {
	t.Parallel()
	e := &scm.Error{Kind: scm.KindRateLimited, Provider: "github", Op: "ListRepositories", RetryAfter: 90 * time.Second, Err: errors.New("API rate limit exceeded")}
	msg := e.Error()
	for _, want := range []string{"github", "ListRepositories", "RateLimited", "1m30s", "API rate limit exceeded"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q lacks %q", msg, want)
		}
	}
	if d, ok := scm.RetryAfter(fmt.Errorf("x: %w", e)); !ok || d != 90*time.Second {
		t.Fatalf("RetryAfter = %v %v", d, ok)
	}
	if _, ok := scm.RetryAfter(errors.New("x")); ok {
		t.Fatal("RetryAfter on plain error")
	}
	if !scm.IsRateLimited(e) {
		t.Fatal("IsRateLimited")
	}
	if (&scm.Error{}).Error() == "" {
		t.Fatal("empty error message")
	}
}
