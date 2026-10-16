package apiclient

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// ProblemContentType is the media type of error responses (RFC 9457).
const ProblemContentType = "application/problem+json"

// ErrorCode is the stable machine-readable error identifier carried in Problem.Code.
type ErrorCode string

// Error codes (api-contract.md, "Errors").
const (
	CodeNotFound           ErrorCode = "not_found"
	CodePermissionDenied   ErrorCode = "permission_denied"
	CodeInvalidArgument    ErrorCode = "invalid_argument"
	CodeConflict           ErrorCode = "conflict"
	CodeAlreadyExists      ErrorCode = "already_exists"
	CodeFailedPrecondition ErrorCode = "failed_precondition"
	CodeUnauthenticated    ErrorCode = "unauthenticated"
	CodeUnavailable        ErrorCode = "unavailable"
	CodeInternal           ErrorCode = "internal"
	CodeResourceExhausted  ErrorCode = "resource_exhausted"
)

// ErrorCodes returns every defined error code.
func ErrorCodes() []ErrorCode {
	return []ErrorCode{
		CodeNotFound, CodePermissionDenied, CodeInvalidArgument, CodeConflict, CodeAlreadyExists,
		CodeFailedPrecondition, CodeUnauthenticated, CodeUnavailable, CodeInternal, CodeResourceExhausted,
	}
}

// JSONSchemaEnum lists the values of ErrorCode for schema generation.
func (ErrorCode) JSONSchemaEnum() []any {
	codes := ErrorCodes()
	out := make([]any, len(codes))
	for i, c := range codes {
		out[i] = string(c)
	}
	return out
}

// ProblemTypePrefix prefixes Problem.Type; the suffix is the error code.
const ProblemTypePrefix = "urn:patrol-cd:problem:"

// ProblemType returns the Problem.Type URI of code.
func ProblemType(code ErrorCode) string { return ProblemTypePrefix + string(code) }

// Problem is an RFC 9457 problem details object (application/problem+json).
type Problem struct {
	// Type is "urn:patrol-cd:problem:<code>".
	Type string `json:"type,omitempty"`
	// Title is the reason phrase of Status.
	Title string `json:"title"`
	// Status is the HTTP status code.
	Status int `json:"status"`
	// Detail is a human-readable explanation specific to this occurrence.
	Detail string `json:"detail,omitempty"`
	// Instance identifies the request (the request path).
	Instance string `json:"instance,omitempty"`
	// Code is the stable error identifier.
	Code ErrorCode `json:"code,omitempty"`
	// RequestID correlates the response with server logs and audit records.
	RequestID string `json:"requestId,omitempty"`
}

// APIError is returned by the client for non-2xx responses.
type APIError struct {
	// StatusCode is the HTTP status.
	StatusCode int
	// Problem is the decoded problem document. When the server did not send
	// application/problem+json, Code is derived from the status and Detail holds the
	// (truncated) body text.
	Problem Problem
	// RetryAfter is the Retry-After header value in seconds (0 when absent).
	RetryAfter int
}

func (e *APIError) Error() string {
	msg := e.Problem.Detail
	if msg == "" {
		msg = e.Problem.Title
	}
	if msg == "" {
		msg = http.StatusText(e.StatusCode)
	}
	return fmt.Sprintf("%s (HTTP %d, %s)", strings.TrimSpace(msg), e.StatusCode, e.Code())
}

// Code returns the problem code, derived from the status when the server sent none.
func (e *APIError) Code() ErrorCode {
	if e.Problem.Code != "" {
		return e.Problem.Code
	}
	return CodeForStatus(e.StatusCode)
}

// CodeForStatus maps an HTTP status to the closest error code.
func CodeForStatus(status int) ErrorCode {
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity, http.StatusUnsupportedMediaType,
		http.StatusMethodNotAllowed:
		return CodeInvalidArgument
	case http.StatusUnauthorized:
		return CodeUnauthenticated
	case http.StatusForbidden:
		return CodePermissionDenied
	case http.StatusNotFound:
		return CodeNotFound
	case http.StatusConflict:
		return CodeConflict
	case http.StatusPreconditionFailed:
		return CodeFailedPrecondition
	case http.StatusTooManyRequests, http.StatusRequestEntityTooLarge:
		return CodeResourceExhausted
	case http.StatusServiceUnavailable, http.StatusBadGateway, http.StatusGatewayTimeout:
		return CodeUnavailable
	}
	if status >= 500 {
		return CodeInternal
	}
	return CodeInvalidArgument
}

func hasCode(err error, code ErrorCode) bool {
	var e *APIError
	return errors.As(err, &e) && e.Code() == code
}

// IsNotFound reports whether err is an APIError with code not_found.
func IsNotFound(err error) bool { return hasCode(err, CodeNotFound) }

// IsPermissionDenied reports whether err is an APIError with code permission_denied.
func IsPermissionDenied(err error) bool { return hasCode(err, CodePermissionDenied) }

// IsUnauthenticated reports whether err is an APIError with code unauthenticated.
func IsUnauthenticated(err error) bool { return hasCode(err, CodeUnauthenticated) }

// IsInvalidArgument reports whether err is an APIError with code invalid_argument.
func IsInvalidArgument(err error) bool { return hasCode(err, CodeInvalidArgument) }

// IsConflict reports whether err is an APIError with code conflict.
func IsConflict(err error) bool { return hasCode(err, CodeConflict) }

// IsAlreadyExists reports whether err is an APIError with code already_exists.
func IsAlreadyExists(err error) bool { return hasCode(err, CodeAlreadyExists) }

// IsResourceExhausted reports whether err is an APIError with code resource_exhausted.
func IsResourceExhausted(err error) bool { return hasCode(err, CodeResourceExhausted) }

// IsUnavailable reports whether err is an APIError with code unavailable.
func IsUnavailable(err error) bool { return hasCode(err, CodeUnavailable) }
