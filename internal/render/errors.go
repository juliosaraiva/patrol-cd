package render

import (
	"errors"
	"fmt"
)

// codedError attaches a repo-server error code (apiclient.Code*) to an error without
// changing its message. Upstream returns these cases as gRPC FailedPrecondition,
// NotFound or PermissionDenied.
type codedError struct {
	code string
	err  error
}

func (e *codedError) Error() string { return e.err.Error() }
func (e *codedError) Unwrap() error { return e.err }

// withCode wraps err with code.
func withCode(code string, err error) error {
	if err == nil {
		return nil
	}
	return &codedError{code: code, err: err}
}

// codef formats an error with a code.
func codef(code, format string, args ...any) error {
	return withCode(code, fmt.Errorf(format, args...))
}

// ErrorCode returns the repo-server error code (apiclient.Code*) attached to err (the
// outermost one), or "" when none is attached.
func ErrorCode(err error) string {
	var ce *codedError
	if errors.As(err, &ce) {
		return ce.code
	}
	return ""
}

// GlobNoMatchError reports a value-file glob that matched no file. It is a runtime
// condition (the files may be added later), mapped to apiclient.CodeNotFound.
type GlobNoMatchError struct {
	Pattern string
}

func (e *GlobNoMatchError) Error() string {
	return fmt.Sprintf("values file glob %q matched no files", e.Pattern)
}

// ErrExceededMaxCombinedManifestFileSize is upstream's error for a Directory source
// over the combined size limit.
var ErrExceededMaxCombinedManifestFileSize = errors.New("exceeded max combined manifest file size")
