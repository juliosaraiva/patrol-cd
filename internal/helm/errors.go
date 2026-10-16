package helm

import (
	"errors"

	"github.com/juliosaraiva/patrol-cd/internal/oci"
)

// Sentinel errors. Callers branch on them with errors.Is. Where upstream has a
// user-visible message, the message is kept and the sentinel is attached without
// changing the text.
var (
	// ErrUnsupportedVersion reports a helm.version other than "", "v3" or "v4".
	ErrUnsupportedVersion = errors.New("helm: unsupported helm version")
	// ErrChartNotFound reports a chart that is not listed in the repository index.
	ErrChartNotFound = errors.New("helm: chart not found in index")
	// ErrVersionNotFound reports a chart version that is not listed in the index.
	ErrVersionNotFound = errors.New("helm: chart version not found")
	// ErrIndexFetch reports a non-200 answer to the index request.
	ErrIndexFetch = errors.New("helm: failed to get index")
	// ErrIndexTooLarge reports an index above Client.MaxIndexBytes.
	ErrIndexTooLarge = errors.New("helm: index too large")
	// ErrDownload reports a non-200 answer to a chart download.
	ErrDownload = errors.New("helm: chart download failed")
	// ErrChartTooLarge reports a chart archive above Client.MaxChartBytes.
	ErrChartTooLarge = errors.New("helm: chart archive too large")
	// ErrInvalidChart reports a chart archive without the expected chart directory.
	ErrInvalidChart = errors.New("helm: invalid chart archive")
	// ErrPathOutsideRoot reports a value file or file parameter that resolves
	// outside the repository root.
	ErrPathOutsideRoot = errors.New("helm: path resolves outside repository root")
	// ErrURLSchemeNotAllowed reports a remote value file whose scheme is not in
	// helm.valuesFileSchemes.
	ErrURLSchemeNotAllowed = errors.New("helm: URL scheme not allowed")
	// ErrInvalidValueFile reports a value file that cannot be used (resolves to
	// the repository root, symlink loop, relative path passed to Template).
	ErrInvalidValueFile = errors.New("helm: invalid value file")
	// ErrInvalidRepoName reports a dependency repository name that cannot be used
	// as a Helm repository name.
	ErrInvalidRepoName = errors.New("helm: invalid repository name")
	// ErrInvalidArgument reports a malformed call argument.
	ErrInvalidArgument = errors.New("helm: invalid argument")
	// ErrValuesTooLarge reports a values document above Client.MaxValuesBytes.
	ErrValuesTooLarge = errors.New("helm: values file too large")

	// ErrNoMatchingVersion and ErrInvalidConstraint are shared with internal/oci.
	ErrNoMatchingVersion = oci.ErrNoMatchingVersion
	ErrInvalidConstraint = oci.ErrInvalidConstraint
)

// taggedError attaches a sentinel to an error without changing its message.
type taggedError struct {
	kind error
	err  error
}

func (e *taggedError) Error() string   { return e.err.Error() }
func (e *taggedError) Unwrap() []error { return []error{e.kind, e.err} }

func tag(kind, err error) error {
	if err == nil {
		return nil
	}
	return &taggedError{kind: kind, err: err}
}
