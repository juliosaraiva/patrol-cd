package render

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/juliosaraiva/patrol-cd/internal/sandbox"
)

// ErrMessageAppPathDoesNotExist is the upstream message for a missing application
// path (used by hydrator messaging).
const ErrMessageAppPathDoesNotExist = "app path does not exist"

// resolveAppPath returns the absolute application directory for the relative path p
// inside root (SRC-004). Symlinks inside root are followed; a path (or a symlink)
// leaving root is rejected.
func resolveAppPath(root, p string) (string, error) {
	if filepath.IsAbs(p) {
		return "", fmt.Errorf("%s: app path is absolute", p)
	}
	rel := filepath.Clean(p)
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s: app path outside root", p)
	}
	root = filepath.Clean(root)
	resolved, err := sandbox.SecureJoin(root, rel)
	if err != nil {
		if errors.Is(err, sandbox.ErrPathEscape) {
			return "", fmt.Errorf("%s: app path outside root", p)
		}
		return "", fmt.Errorf("%s: invalid app path: %w", p, err)
	}
	info, err := os.Stat(resolved)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("%s: %s", p, ErrMessageAppPathDoesNotExist)
	}
	if err != nil {
		return "", fmt.Errorf("%s: %w", p, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s: app path is not a directory", p)
	}
	return resolved, nil
}

// relToRoot returns p relative to root using forward-compatible OS separators, or an
// error when p is outside root.
func relToRoot(root, p string) (string, error) {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%q is outside %q", p, root)
	}
	return rel, nil
}
