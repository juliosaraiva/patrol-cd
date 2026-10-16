package helm

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/juliosaraiva/patrol-cd/internal/sandbox"
)

// DefaultValuesFileSchemes are the remote value-file schemes allowed when
// argocd-cm style setting helm.valuesFileSchemes is absent.
var DefaultValuesFileSchemes = []string{"https", "http"}

// ValuesFileSchemes interprets the helm.valuesFileSchemes setting: absent
// (present=false) means DefaultValuesFileSchemes; present means the
// comma-separated, trimmed list, so an empty value allows no remote file.
func ValuesFileSchemes(raw string, present bool) []string {
	if !present {
		return append([]string(nil), DefaultValuesFileSchemes...)
	}
	var out []string
	for _, s := range strings.Split(raw, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// IsURLSchemeAllowed reports whether scheme (case-insensitive) is in allowed. An
// empty scheme (a local path) is never "allowed" as a URL.
func IsURLSchemeAllowed(scheme string, allowed []string) bool {
	if scheme == "" {
		return false
	}
	for _, s := range allowed {
		if strings.EqualFold(scheme, s) {
			return true
		}
	}
	return false
}

// ResolveValueFilePathOrURL resolves a value file (or --set-file path) with the
// baseline rules and returns either an allowed remote URL (remote=true, returned
// verbatim) or an absolute local path that lies strictly inside repoRoot.
//
//   - A value that parses as a URL with a scheme is remote; its scheme must be in
//     allowedSchemes, otherwise "the URL scheme '<s>' is not allowed"
//     (ErrURLSchemeNotAllowed).
//   - A path starting with "/" is relative to repoRoot; any other path is
//     relative to appPath (which must lie inside repoRoot). ".." components are
//     applied lexically first, as upstream.
//   - Every component is then resolved with sandbox.SecureJoin, so symlinks are
//     followed only while they stay inside repoRoot; the returned path contains
//     no symlinks. Escapes fail with "file '<orig>' resolved to outside
//     repository root" (ErrPathOutsideRoot); resolving to repoRoot itself or a
//     symlink loop fails with ErrInvalidValueFile and a generic message (details
//     are logged at debug level, never returned, to avoid leaking paths).
//
// The file does not have to exist; callers implementing ignoreMissingValueFiles
// stat the result.
func ResolveValueFilePathOrURL(appPath, repoRoot, valueFile string, allowedSchemes []string) (resolved string, remote bool, err error) {
	if u, perr := url.Parse(valueFile); perr == nil && u.Scheme != "" {
		if IsURLSchemeAllowed(u.Scheme, allowedSchemes) {
			return valueFile, true, nil
		}
		return "", false, tag(ErrURLSchemeNotAllowed, fmt.Errorf("the URL scheme '%s' is not allowed", u.Scheme))
	}
	p, err := resolveInRoot(appPath, repoRoot, valueFile, false)
	if err != nil {
		return "", false, err
	}
	return p, false, nil
}

// ResolveFileOrDirectoryPath is ResolveValueFilePathOrURL for local files or
// directories that may also be repoRoot itself (no URL handling).
func ResolveFileOrDirectoryPath(appPath, repoRoot, p string) (string, error) {
	return resolveInRoot(appPath, repoRoot, p, true)
}

var errResolveInternal = errors.New("internal error: failed to resolve path. Check logs for more details")

func resolveInRoot(appPath, repoRoot, p string, allowRoot bool) (string, error) {
	root, err := filepath.Abs(repoRoot)
	if err != nil {
		return "", internalResolveError(p, err)
	}
	app, err := filepath.Abs(appPath)
	if err != nil {
		return "", internalResolveError(p, err)
	}
	appRel, err := filepath.Rel(root, app)
	if err != nil || appRel == ".." || strings.HasPrefix(appRel, ".."+string(filepath.Separator)) {
		return "", internalResolveError(p, fmt.Errorf("application path is outside the repository root"))
	}
	var rel string
	if filepath.IsAbs(p) {
		rel = filepath.Clean(strings.TrimLeft(p, string(filepath.Separator)))
		if rel == "" {
			rel = "."
		}
	} else {
		rel = filepath.Join(appRel, p)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", outsideRoot(p)
	}
	joined, err := sandbox.SecureJoin(root, rel)
	if err != nil {
		if errors.Is(err, sandbox.ErrPathEscape) {
			return "", outsideRoot(p)
		}
		return "", internalResolveError(p, err)
	}
	if joined == root && !allowRoot {
		return "", internalResolveError(p, errors.New("path resolved to repository root, which is not allowed"))
	}
	return joined, nil
}

func outsideRoot(p string) error {
	return tag(ErrPathOutsideRoot, fmt.Errorf("file '%s' resolved to outside repository root", p))
}

func internalResolveError(p string, cause error) error {
	slog.Debug("helm: failed to resolve path", "path", p, "error", cause)
	return tag(ErrInvalidValueFile, errResolveInternal)
}
