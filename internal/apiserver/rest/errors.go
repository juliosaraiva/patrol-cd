package rest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/juliosaraiva/patrol-cd/internal/auth"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// Error is an API error: an HTTP status, a stable code and a detail message that is safe
// to return to the client. The optional cause is logged (for 5xx) but never returned.
type Error struct {
	Status int
	Code   apiclient.ErrorCode
	Detail string
	// Cause is the underlying error, for logs and errors.Is.
	Cause error
	// RetryAfter sets the Retry-After header (rounded up to seconds) when > 0.
	RetryAfter time.Duration
	// Allow sets the Allow header (405 responses).
	Allow []string
}

func (e *Error) Error() string {
	if e.Cause != nil && e.Detail != "" {
		return e.Detail + ": " + e.Cause.Error()
	}
	if e.Cause != nil {
		return e.Cause.Error()
	}
	return e.Detail
}

// Unwrap returns the cause.
func (e *Error) Unwrap() error { return e.Cause }

func newError(status int, code apiclient.ErrorCode, format string, args []any) *Error {
	detail := format
	if len(args) > 0 {
		detail = fmt.Sprintf(format, args...)
	}
	return &Error{Status: status, Code: code, Detail: detail}
}

// NotFound returns a 404 not_found error.
func NotFound(format string, args ...any) *Error {
	return newError(http.StatusNotFound, apiclient.CodeNotFound, format, args)
}

// InvalidArgument returns a 400 invalid_argument error.
func InvalidArgument(format string, args ...any) *Error {
	return newError(http.StatusBadRequest, apiclient.CodeInvalidArgument, format, args)
}

// PermissionDenied returns a 403 permission_denied error.
func PermissionDenied(format string, args ...any) *Error {
	return newError(http.StatusForbidden, apiclient.CodePermissionDenied, format, args)
}

// Unauthenticated returns a 401 unauthenticated error.
func Unauthenticated(format string, args ...any) *Error {
	return newError(http.StatusUnauthorized, apiclient.CodeUnauthenticated, format, args)
}

// Conflict returns a 409 conflict error.
func Conflict(format string, args ...any) *Error {
	return newError(http.StatusConflict, apiclient.CodeConflict, format, args)
}

// AlreadyExists returns a 409 already_exists error.
func AlreadyExists(format string, args ...any) *Error {
	return newError(http.StatusConflict, apiclient.CodeAlreadyExists, format, args)
}

// FailedPrecondition returns a 412 failed_precondition error.
func FailedPrecondition(format string, args ...any) *Error {
	return newError(http.StatusPreconditionFailed, apiclient.CodeFailedPrecondition, format, args)
}

// Unavailable returns a 503 unavailable error.
func Unavailable(format string, args ...any) *Error {
	return newError(http.StatusServiceUnavailable, apiclient.CodeUnavailable, format, args)
}

// ResourceExhausted returns a 429 resource_exhausted error.
func ResourceExhausted(format string, args ...any) *Error {
	return newError(http.StatusTooManyRequests, apiclient.CodeResourceExhausted, format, args)
}

// Internal returns a 500 internal error whose detail is generic; cause is logged.
func Internal(cause error) *Error {
	return &Error{Status: http.StatusInternalServerError, Code: apiclient.CodeInternal, Detail: "internal error", Cause: cause}
}

// WithCause returns a copy of e with the cause set.
func (e *Error) WithCause(cause error) *Error {
	c := *e
	c.Cause = cause
	return &c
}

// ProblemFor maps any error to an *Error:
//   - *Error values (anywhere in the chain) are returned as is;
//   - Kubernetes API errors map to their HTTP equivalents (NotFound 404, AlreadyExists and
//     Conflict 409, Forbidden 403, Invalid 422, BadRequest 400, TooManyRequests 429,
//     timeouts and unavailability 503, Gone/Expired 410, too large 413);
//   - rbac.ErrPermissionDenied is 403, auth.ErrInvalidToken 401;
//   - request body limits are 413, context cancellation and deadlines 503/504;
//   - everything else is a 500 with a generic detail.
func ProblemFor(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	if pe, ok := errors.AsType[*rbac.PermissionDeniedError](err); ok {
		return &Error{Status: http.StatusForbidden, Code: apiclient.CodePermissionDenied, Detail: pe.Error(), Cause: err}
	}
	if errors.Is(err, rbac.ErrPermissionDenied) {
		return &Error{Status: http.StatusForbidden, Code: apiclient.CodePermissionDenied, Detail: "permission denied", Cause: err}
	}
	if errors.Is(err, auth.ErrInvalidToken) {
		return &Error{Status: http.StatusUnauthorized, Code: apiclient.CodeUnauthenticated, Detail: "invalid session", Cause: err}
	}
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return &Error{Status: http.StatusRequestEntityTooLarge, Code: apiclient.CodeResourceExhausted, Detail: "request body too large", Cause: err}
	}
	if k := kubeProblem(err); k != nil {
		return k
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{Status: http.StatusGatewayTimeout, Code: apiclient.CodeUnavailable, Detail: "deadline exceeded", Cause: err}
	}
	if errors.Is(err, context.Canceled) {
		return &Error{Status: http.StatusServiceUnavailable, Code: apiclient.CodeUnavailable, Detail: "request canceled", Cause: err}
	}
	return Internal(err)
}

// kubeProblem maps Kubernetes API status errors. Messages of NotFound, AlreadyExists,
// Conflict, Invalid and BadRequest errors describe the request and are returned; the
// messages of Forbidden and Unauthorized errors describe patrol-server's own identity
// and are not.
func kubeProblem(err error) *Error {
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		return nil
	}
	msg := status.Status().Message
	mk := func(code int, c apiclient.ErrorCode, detail string) *Error {
		return &Error{Status: code, Code: c, Detail: detail, Cause: err}
	}
	switch {
	case apierrors.IsNotFound(err):
		return mk(http.StatusNotFound, apiclient.CodeNotFound, msg)
	case apierrors.IsAlreadyExists(err):
		return mk(http.StatusConflict, apiclient.CodeAlreadyExists, msg)
	case apierrors.IsConflict(err):
		return mk(http.StatusConflict, apiclient.CodeConflict, msg)
	case apierrors.IsForbidden(err):
		return mk(http.StatusForbidden, apiclient.CodePermissionDenied, "permission denied")
	case apierrors.IsInvalid(err):
		return mk(http.StatusUnprocessableEntity, apiclient.CodeInvalidArgument, msg)
	case apierrors.IsBadRequest(err):
		return mk(http.StatusBadRequest, apiclient.CodeInvalidArgument, msg)
	case apierrors.IsTooManyRequests(err):
		e := mk(http.StatusTooManyRequests, apiclient.CodeResourceExhausted, "too many requests")
		if s, ok := apierrors.SuggestsClientDelay(err); ok {
			e.RetryAfter = time.Duration(s) * time.Second
		}
		return e
	case apierrors.IsResourceExpired(err), apierrors.IsGone(err):
		return mk(http.StatusGone, apiclient.CodeFailedPrecondition, msg)
	case apierrors.IsRequestEntityTooLargeError(err):
		return mk(http.StatusRequestEntityTooLarge, apiclient.CodeResourceExhausted, "request entity too large")
	case apierrors.IsServiceUnavailable(err), apierrors.IsTimeout(err), apierrors.IsServerTimeout(err):
		return mk(http.StatusServiceUnavailable, apiclient.CodeUnavailable, "the Kubernetes API is unavailable")
	case apierrors.IsMethodNotSupported(err):
		return mk(http.StatusMethodNotAllowed, apiclient.CodeInvalidArgument, msg)
	}
	return Internal(err)
}
