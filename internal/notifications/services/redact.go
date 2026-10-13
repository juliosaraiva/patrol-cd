package services

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
)

const redactedMarker = "******"

// minSecretLen avoids redacting trivially short values (e.g. "1", "a") that would mangle
// unrelated error text without protecting anything meaningful.
const minSecretLen = 4

// Redactor removes secret values and URL credentials from strings. It is safe for
// concurrent use.
type Redactor struct {
	mu      sync.RWMutex
	secrets []string // longest first
}

// NewRedactor returns a redactor for the given secret values.
func NewRedactor(values ...string) *Redactor {
	r := &Redactor{}
	r.Add(values...)
	return r
}

// With returns a copy of r that additionally redacts values. Use it for call-scoped
// secrets (e.g. short-lived session tokens) so the shared redactor does not grow.
func (r *Redactor) With(values ...string) *Redactor {
	out := &Redactor{}
	if r != nil {
		r.mu.RLock()
		out.secrets = append([]string(nil), r.secrets...)
		r.mu.RUnlock()
	}
	out.Add(values...)
	return out
}

// Add registers additional secret values.
func (r *Redactor) Add(values ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := make(map[string]struct{}, len(r.secrets))
	for _, s := range r.secrets {
		seen[s] = struct{}{}
	}
	for _, v := range values {
		v = strings.TrimSpace(v)
		if len(v) < minSecretLen {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		r.secrets = append(r.secrets, v)
		// Values commonly appear URL-escaped inside request URLs.
		if esc := url.QueryEscape(v); esc != v {
			if _, ok := seen[esc]; !ok {
				seen[esc] = struct{}{}
				r.secrets = append(r.secrets, esc)
			}
		}
	}
	sort.SliceStable(r.secrets, func(i, j int) bool { return len(r.secrets[i]) > len(r.secrets[j]) })
}

var urlInText = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^\s"'<>]+`)

// Redact returns s with every registered secret replaced and every URL stripped of user
// info and query values.
func (r *Redactor) Redact(s string) string {
	if r != nil {
		r.mu.RLock()
		for _, secret := range r.secrets {
			s = strings.ReplaceAll(s, secret, redactedMarker)
		}
		r.mu.RUnlock()
	}
	return urlInText.ReplaceAllStringFunc(s, SanitizeURL)
}

// SanitizeURL removes user info and query values from a URL string. Unparseable input is
// returned unchanged.
func SanitizeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	changed := false
	if u.User != nil {
		u.User = url.User(redactedMarker)
		changed = true
	}
	if u.RawQuery != "" {
		q := u.Query()
		for k := range q {
			q[k] = []string{redactedMarker}
		}
		u.RawQuery = q.Encode()
		changed = true
	}
	if !changed {
		return raw
	}
	return u.String()
}

// redactedError carries a redacted message while preserving errors.Is/As classification
// of the original cause. It deliberately does not expose Unwrap so the unredacted message
// of the cause cannot be reached by generic unwrapping.
type redactedError struct {
	msg   string
	cause error
}

func (e *redactedError) Error() string { return e.msg }

func (e *redactedError) Is(target error) bool { return errors.Is(e.cause, target) }

func (e *redactedError) As(target any) bool {
	// Only StatusError is exposed; its body was redacted at construction.
	if t, ok := target.(**StatusError); ok {
		var se *StatusError
		if errors.As(e.cause, &se) {
			cp := *se
			cp.URL = SanitizeURL(cp.URL)
			*t = &cp
			return true
		}
	}
	return false
}

// redactErr redacts err with r, preserving classification.
func redactErr(r *Redactor, err error) error {
	if err == nil {
		return nil
	}
	return &redactedError{msg: r.Redact(err.Error()), cause: err}
}

// redactingService wraps a Service and redacts its errors.
type redactingService struct {
	typ      string
	inner    Service
	redactor *Redactor
}

func (s *redactingService) Send(ctx context.Context, n Notification, dest Destination) error {
	err := s.inner.Send(ctx, n, dest)
	if err == nil {
		return nil
	}
	return &redactedError{msg: s.redactor.Redact(err.Error()), cause: err}
}

// Unwrap exposes the wrapped service (used by tests and type assertions).
func (s *redactingService) Unwrap() Service { return s.inner }
