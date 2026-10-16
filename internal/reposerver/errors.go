package reposerver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/juliosaraiva/patrol-cd/internal/git"
	"github.com/juliosaraiva/patrol-cd/internal/helm"
	"github.com/juliosaraiva/patrol-cd/internal/oci"
	"github.com/juliosaraiva/patrol-cd/internal/render"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/internal/sandbox"
)

// codedError attaches a repo-server error code to an error without changing its text.
type codedError struct {
	code string
	err  error
}

func (e *codedError) Error() string { return e.err.Error() }
func (e *codedError) Unwrap() error { return e.err }

// withCode returns err carrying code (nil stays nil).
func withCode(code string, err error) error {
	if err == nil {
		return nil
	}
	return &codedError{code: code, err: err}
}

// errorf formats an error carrying code.
func errorf(code, format string, args ...any) error {
	return withCode(code, fmt.Errorf(format, args...))
}

// ErrorCode returns the apiclient.Code* classification of a service error.
func ErrorCode(err error) string {
	if err == nil {
		return ""
	}
	var ce *codedError
	if errors.As(err, &ce) {
		return ce.code
	}
	var ae *apiclient.Error
	if errors.As(err, &ae) && ae.Code != "" {
		return ae.Code
	}
	if c := render.ErrorCode(err); c != "" {
		return c
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, sandbox.ErrTimeout):
		return apiclient.CodeDeadlineExceeded
	case errors.Is(err, context.Canceled):
		return apiclient.CodeUnavailable
	case errors.Is(err, git.ErrInvalidRevision), errors.Is(err, git.ErrInvalidURL),
		errors.Is(err, git.ErrInvalidCredentials), errors.Is(err, oci.ErrInvalidReference),
		errors.Is(err, helm.ErrInvalidArgument), errors.Is(err, helm.ErrUnsupportedVersion),
		errors.Is(err, helm.ErrPathOutsideRoot), errors.Is(err, helm.ErrURLSchemeNotAllowed):
		return apiclient.CodeInvalidArgument
	case errors.Is(err, git.ErrAuthentication), errors.Is(err, oci.ErrUnauthorized):
		return apiclient.CodePermissionDenied
	case errors.Is(err, sandbox.ErrOutputLimit), errors.Is(err, sandbox.ErrArchiveLimit),
		errors.Is(err, render.ErrTooManyManifests), errors.Is(err, render.ErrOutputTooLarge),
		errors.Is(err, render.ErrExceededMaxCombinedManifestFileSize):
		return apiclient.CodeResourceExhausted
	}
	return apiclient.CodeInternal
}

// contentError marks a failure to obtain the content of the main source: its working
// copy checkout (fetch, authentication, timeouts), its symlink scan, an OCI artifact
// pull or a Helm chart download. Upstream returns these from runRepoOperation before
// manifest generation starts, so they never enter the manifest-generation error cache
// (SRC-111): a recovered remote or a rotated credential takes effect on the next
// request. Referenced (`$ref`) checkouts, source integrity and rendering failures are
// recorded, as upstream.
type contentError struct{ err error }

func (e *contentError) Error() string { return e.err.Error() }
func (e *contentError) Unwrap() error { return e.err }

// asContentError marks err as a content failure (nil stays nil).
func asContentError(err error) error {
	if err == nil {
		return nil
	}
	return &contentError{err: err}
}

// isContentError reports whether err is a content failure.
func isContentError(err error) bool {
	var ce *contentError
	return errors.As(err, &ce)
}

// chartFetchErrorPrefix is how internal/render reports a failed Helm chart pull
// (render.pullChart). render exposes no typed error for it yet (requested); the
// prefix is pinned by TestGenerateManifestErrorCachingSkipsContentFailures.
const chartFetchErrorPrefix = "error fetching chart: "

// isChartFetchError reports whether a render error is a Helm chart pull failure.
func isChartFetchError(err error) bool {
	return err != nil && strings.HasPrefix(err.Error(), chartFetchErrorPrefix)
}

// sanitizedError replaces the message of err (cache paths removed) and keeps it
// unwrappable so codes and sentinels survive.
type sanitizedError struct {
	msg string
	err error
}

func (e *sanitizedError) Error() string { return e.msg }
func (e *sanitizedError) Unwrap() error { return e.err }

// placeholderCachedSource replaces working-copy paths in returned errors (upstream text).
const placeholderCachedSource = "<path to cached source>"

// pathSanitizer rewrites <root>/<entry> occurrences (the random per-repository and
// per-request directory names) in error messages (SRC-118).
type pathSanitizer struct {
	res []*regexp.Regexp
}

func newPathSanitizer(roots ...string) *pathSanitizer {
	s := &pathSanitizer{}
	seen := map[string]bool{}
	// Longer roots first, so "/private/tmp/x" is replaced before "/tmp/x" matches
	// inside it.
	roots = slices.Clone(roots)
	sort.Slice(roots, func(i, j int) bool { return len(roots[i]) > len(roots[j]) })
	for _, r := range roots {
		r = strings.TrimSuffix(r, string(os.PathSeparator))
		if r == "" || seen[r] {
			continue
		}
		seen[r] = true
		// The component after root holds no spaces (random names), so text following
		// a path is not swallowed (upstream getRepoSanitizerRegex).
		s.res = append(s.res, regexp.MustCompile(regexp.QuoteMeta(r)+`/[^ /]*`))
	}
	return s
}

func (s *pathSanitizer) apply(msg string) string {
	for _, re := range s.res {
		msg = re.ReplaceAllLiteralString(msg, placeholderCachedSource)
	}
	return msg
}

func (s *pathSanitizer) sanitize(err error) error {
	if err == nil || s == nil {
		return err
	}
	msg := err.Error()
	clean := s.apply(msg)
	if clean == msg {
		return err
	}
	return &sanitizedError{msg: clean, err: err}
}

// readFileLimited reads at most limit bytes of a regular file.
func readFileLimited(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s exceeds %d bytes", path, limit)
	}
	return data, nil
}

func trimSpace(s string) string { return strings.TrimSpace(s) }
