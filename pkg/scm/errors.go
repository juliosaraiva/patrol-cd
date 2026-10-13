package scm

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrorKind classifies provider errors so callers can branch without knowing the
// provider.
type ErrorKind string

// Error kinds.
const (
	KindUnknown      ErrorKind = ""
	KindNotFound     ErrorKind = "NotFound"
	KindUnauthorized ErrorKind = "Unauthorized"
	KindForbidden    ErrorKind = "Forbidden"
	KindRateLimited  ErrorKind = "RateLimited"
	KindUnavailable  ErrorKind = "Unavailable"
	KindInvalid      ErrorKind = "Invalid"
	KindConflict     ErrorKind = "Conflict"
)

// Sentinel errors matched by errors.Is against an *Error of the same kind.
var (
	ErrNotFound     = errors.New("scm: not found")
	ErrUnauthorized = errors.New("scm: unauthorized")
	ErrForbidden    = errors.New("scm: forbidden")
	ErrRateLimited  = errors.New("scm: rate limited")
	ErrUnavailable  = errors.New("scm: unavailable")
	ErrInvalid      = errors.New("scm: invalid request")
	ErrConflict     = errors.New("scm: conflict")
)

// ErrUnsupported is matched by *UnsupportedError.
var ErrUnsupported = errors.New("scm: capability not supported")

// Additional sentinels that appear as the Err of an *Error.
var (
	// ErrPageLimit means a listing needed more pages than the configured cap.
	ErrPageLimit = errors.New("scm: pagination page limit exceeded")
	// ErrPaginationLoop means the provider returned a cursor that was already visited.
	ErrPaginationLoop = errors.New("scm: pagination cursor repeated")
	// ErrResponseTooLarge means a response body exceeded the configured size cap.
	ErrResponseTooLarge = errors.New("scm: response body exceeds size limit")
	// ErrUnknownProviderType is returned by Registry.New for unregistered types.
	ErrUnknownProviderType = errors.New("scm: unknown provider type")
)

// Error is the normalized provider error.
type Error struct {
	Kind ErrorKind
	// Provider is the provider type ("github").
	Provider string
	// Op is the contract operation ("ListRepositories").
	Op string
	// StatusCode is the HTTP status that caused the error, when known.
	StatusCode int
	// RetryAfter is the provider-advised wait before retrying (RateLimited, sometimes
	// Unavailable). Zero when unknown.
	RetryAfter time.Duration
	// Err is the underlying cause.
	Err error
}

func (e *Error) Error() string {
	var b strings.Builder
	if e.Provider != "" {
		b.WriteString(e.Provider)
		b.WriteString(": ")
	}
	if e.Op != "" {
		b.WriteString(e.Op)
		b.WriteString(": ")
	}
	if e.Kind != KindUnknown {
		b.WriteString(string(e.Kind))
	} else {
		b.WriteString("error")
	}
	if e.RetryAfter > 0 {
		fmt.Fprintf(&b, " (retry after %s)", e.RetryAfter.Round(time.Second))
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Err }

// Is matches the sentinel corresponding to e.Kind.
func (e *Error) Is(target error) bool {
	s := sentinelFor(e.Kind)
	return s != nil && target == s
}

func sentinelFor(k ErrorKind) error {
	switch k {
	case KindNotFound:
		return ErrNotFound
	case KindUnauthorized:
		return ErrUnauthorized
	case KindForbidden:
		return ErrForbidden
	case KindRateLimited:
		return ErrRateLimited
	case KindUnavailable:
		return ErrUnavailable
	case KindInvalid:
		return ErrInvalid
	case KindConflict:
		return ErrConflict
	}
	return nil
}

// NewError builds an *Error.
func NewError(kind ErrorKind, provider, op string, err error) *Error {
	return &Error{Kind: kind, Provider: provider, Op: op, Err: err}
}

// KindOf returns the kind of the first *Error in err's chain, or KindUnknown.
func KindOf(err error) ErrorKind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return KindUnknown
}

// IsNotFound reports whether err is a NotFound error.
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

// IsRateLimited reports whether err is a RateLimited error.
func IsRateLimited(err error) bool { return errors.Is(err, ErrRateLimited) }

// IsUnsupported reports whether err is an unsupported-capability error.
func IsUnsupported(err error) bool { return errors.Is(err, ErrUnsupported) }

// IsRetryable reports whether retrying the same request later may succeed
// (RateLimited, Unavailable, Conflict).
func IsRetryable(err error) bool {
	switch KindOf(err) {
	case KindRateLimited, KindUnavailable, KindConflict:
		return true
	}
	return false
}

// RetryAfter returns the provider-advised retry delay carried by err, if any.
func RetryAfter(err error) (time.Duration, bool) {
	var e *Error
	if errors.As(err, &e) && e.RetryAfter > 0 {
		return e.RetryAfter, true
	}
	return 0, false
}

// UnsupportedError reports that a provider does not declare a capability.
type UnsupportedError struct {
	Provider   string
	Name       string
	Capability Capability
}

func (e *UnsupportedError) Error() string {
	p := e.Provider
	if p == "" {
		p = "provider"
	}
	if e.Name != "" {
		p += " " + e.Name
	}
	return fmt.Sprintf("scm: %s does not support capability %q", p, e.Capability)
}

// Is matches ErrUnsupported.
func (e *UnsupportedError) Is(target error) bool { return target == ErrUnsupported }
