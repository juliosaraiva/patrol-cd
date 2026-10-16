package treeapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
)

// Sentinel errors. Providers wrap them; Client errors match them with errors.Is.
var (
	ErrNotFound         = errors.New("not found")
	ErrUnavailable      = errors.New("unavailable")
	ErrInvalidArgument  = errors.New("invalid argument")
	ErrPermissionDenied = errors.New("permission denied")
	ErrUnauthenticated  = errors.New("unauthenticated")
	ErrTooLarge         = errors.New("too large")
	ErrDeadlineExceeded = errors.New("deadline exceeded")
	ErrInternal         = errors.New("internal error")
	// ErrNotOwned: the replica does not own the Application's shard (see NotOwnedError).
	ErrNotOwned = errors.New("application is served by another controller replica")
)

// Problem codes.
const (
	CodeNotFound         = "not_found"
	CodeUnavailable      = "unavailable"
	CodeInvalidArgument  = "invalid_argument"
	CodePermissionDenied = "permission_denied"
	CodeUnauthenticated  = "unauthenticated"
	CodeTooLarge         = "resource_exhausted"
	CodeDeadlineExceeded = "deadline_exceeded"
	CodeInternal         = "internal"
	CodeMisdirected      = "misdirected"
)

// NotOwnedError reports that another replica serves the Application: Shard is the owning
// shard, or -1 when unknown.
type NotOwnedError struct {
	Shard int
}

func (e *NotOwnedError) Error() string {
	if e.Shard < 0 {
		return ErrNotOwned.Error()
	}
	return fmt.Sprintf("%s (shard %d)", ErrNotOwned, e.Shard)
}

// Is matches ErrNotOwned.
func (e *NotOwnedError) Is(target error) bool { return target == ErrNotOwned }

// Error is a tree API error response received by the Client.
type Error struct {
	Status  int    `json:"status"`
	Code    string `json:"code"`
	Message string `json:"detail"`
	// Shard is the owning shard of a misdirected request.
	Shard *int `json:"shard,omitempty"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("controller tree API: %s (%d %s)", e.Message, e.Status, e.Code)
}

// Is matches the sentinel of the error's code.
func (e *Error) Is(target error) bool {
	return target == sentinelFor(e.Code)
}

func sentinelFor(code string) error {
	switch code {
	case CodeNotFound:
		return ErrNotFound
	case CodeUnavailable:
		return ErrUnavailable
	case CodeInvalidArgument:
		return ErrInvalidArgument
	case CodePermissionDenied:
		return ErrPermissionDenied
	case CodeUnauthenticated:
		return ErrUnauthenticated
	case CodeTooLarge:
		return ErrTooLarge
	case CodeDeadlineExceeded:
		return ErrDeadlineExceeded
	case CodeMisdirected:
		return ErrNotOwned
	default:
		return ErrInternal
	}
}

// classify maps a provider or validation error to an HTTP status and code.
func classify(err error) (status int, code string, shard *int) {
	var no *NotOwnedError
	switch {
	case errors.As(err, &no):
		if no.Shard >= 0 {
			s := no.Shard
			shard = &s
		}
		return http.StatusMisdirectedRequest, CodeMisdirected, shard
	case errors.Is(err, ErrNotOwned):
		return http.StatusMisdirectedRequest, CodeMisdirected, nil
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound, CodeNotFound, nil
	case errors.Is(err, ErrInvalidArgument):
		return http.StatusBadRequest, CodeInvalidArgument, nil
	case errors.Is(err, ErrPermissionDenied):
		return http.StatusForbidden, CodePermissionDenied, nil
	case errors.Is(err, ErrUnavailable):
		return http.StatusServiceUnavailable, CodeUnavailable, nil
	case errors.Is(err, ErrTooLarge):
		return http.StatusRequestEntityTooLarge, CodeTooLarge, nil
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, ErrDeadlineExceeded):
		return http.StatusGatewayTimeout, CodeDeadlineExceeded, nil
	case errors.Is(err, context.Canceled):
		return http.StatusServiceUnavailable, CodeUnavailable, nil
	default:
		return http.StatusInternalServerError, CodeInternal, nil
	}
}

// invalidf returns an ErrInvalidArgument error with message format.
func invalidf(format string, args ...any) error {
	return &codedError{msg: fmt.Sprintf(format, args...), sentinel: ErrInvalidArgument}
}

// deniedf returns an ErrPermissionDenied error with message format.
func deniedf(format string, args ...any) error {
	return &codedError{msg: fmt.Sprintf(format, args...), sentinel: ErrPermissionDenied}
}

// codedError carries an exact message (upstream wording) and a sentinel.
type codedError struct {
	msg      string
	sentinel error
}

func (e *codedError) Error() string        { return e.msg }
func (e *codedError) Is(target error) bool { return target == e.sentinel }

func shardHeader(shard *int) string {
	if shard == nil {
		return ""
	}
	return strconv.Itoa(*shard)
}
