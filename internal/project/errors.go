package project

import (
	"errors"
	"fmt"
)

// Code classifies a validation error the way upstream classifies it with gRPC status codes,
// so the API server can map it to an HTTP status (InvalidArgument → 400, AlreadyExists →
// 409, OutOfRange → 400, NotFound → 404, Internal → 500).
type Code string

const (
	CodeInvalidArgument Code = "InvalidArgument"
	CodeAlreadyExists   Code = "AlreadyExists"
	CodeOutOfRange      Code = "OutOfRange"
	CodeNotFound        Code = "NotFound"
	CodeInternal        Code = "Internal"
)

// ErrInvalidProject is matched (errors.Is) by every *ValidationError returned by project
// validation.
var ErrInvalidProject = errors.New("invalid project")

// ValidationError is a project validation failure. Message carries the upstream wording.
type ValidationError struct {
	Code    Code
	Message string
}

// Error implements error.
func (e *ValidationError) Error() string { return e.Message }

// Unwrap makes errors.Is(err, ErrInvalidProject) true.
func (e *ValidationError) Unwrap() error { return ErrInvalidProject }

func invalidf(code Code, format string, args ...any) *ValidationError {
	return &ValidationError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// ErrProjectNotFound is matched by errors returned when an AppProject does not exist.
var ErrProjectNotFound = errors.New("project not found")

// ErrClusterNotFound is matched by errors returned by the cluster lookups of this package
// when no registered cluster has the requested server URL (or the in-cluster destination is
// disabled).
var ErrClusterNotFound = errors.New("cluster not found")

// ErrDestinationMissing is returned when an Application destination has neither a server nor
// a name. The text is the upstream constant.
var ErrDestinationMissing = errors.New("Destination server missing from app spec") //nolint:staticcheck,revive // upstream wording

// NotPermittedError reports an Application whose namespace may not use its project
// (spec.sourceNamespaces). Error() returns the controller wording; APIMessage the API
// server wording.
type NotPermittedError struct {
	Application string
	Namespace   string
	Project     string
}

// Error returns "application '<a>' in namespace '<ns>' is not permitted to use project '<p>'".
func (e *NotPermittedError) Error() string {
	return fmt.Sprintf("application '%s' in namespace '%s' is not permitted to use project '%s'", e.Application, e.Namespace, e.Project)
}

// APIMessage returns the internal API server wording
// "application '<a>' in namespace '<ns>' is not allowed to use project <p>". The API server
// must not return it to clients (it returns ErrAppNotAllowedMessage instead, to avoid
// project enumeration).
func (e *NotPermittedError) APIMessage() string {
	return fmt.Sprintf("application '%s' in namespace '%s' is not allowed to use project %s", e.Application, e.Namespace, e.Project)
}

// AppNotAllowedMessage is the deliberately vague message the API server returns for both
// "project does not exist" and "namespace not permitted" (APP-002, APP-068).
func AppNotAllowedMessage(project string) string {
	return fmt.Sprintf("app is not allowed in project %q, or the project does not exist", project)
}

// NoServiceAccountError is returned by SelectServiceAccount when impersonation is enabled
// and enforced and no destination service account matches.
type NoServiceAccountError struct {
	Server    string
	Namespace string
}

// Error returns the upstream wording.
func (e *NoServiceAccountError) Error() string {
	return fmt.Sprintf("no matching service account found for destination server %s and namespace %s", e.Server, e.Namespace)
}
