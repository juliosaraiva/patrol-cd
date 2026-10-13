package sandbox

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// maxSymlinkHops bounds symlink expansion. It is above every kernel limit (Linux 40,
// macOS 32), so any path the kernel can resolve is resolved here too.
const maxSymlinkHops = 255

// SecureJoin joins rel onto root, resolving symlinks the way the kernel would, and
// returns an absolute path that is guaranteed to lie within root (or be root).
//
// It fails with ErrAbsolutePath when rel is absolute, with ErrPathEscape when a
// '..' component or a symlink would leave root (absolute symlink targets always
// count as escapes, even if they point inside root), and with ErrTooManySymlinks
// for loops. Components that do not exist are joined lexically, so the result may
// name a file that is about to be created. root itself is made absolute but its own
// symlinks are not resolved.
//
// The check is not atomic: a concurrent writer inside root can swap a directory for
// a symlink after SecureJoin returns. Use os.Root for writes into trees other
// processes can modify.
func SecureJoin(root, rel string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("%w: empty root", ErrPathEscape)
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: %q", ErrAbsolutePath, rel)
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("sandbox: resolve root: %w", err)
	}
	comps, err := resolveWithin(rootAbs, nil, rel)
	if err != nil {
		return "", fmt.Errorf("sandbox: join %q: %w", rel, err)
	}
	return filepath.Join(rootAbs, filepath.Join(comps...)), nil
}

// resolveWithin resolves rel starting at the directory base (root-relative
// components that contain no symlinks) and returns the resolved root-relative
// components. It never steps above root: '..' at the root fails with ErrPathEscape.
// Absolute link targets fail with *OutOfBoundsSymlinkError.
func resolveWithin(root string, base []string, rel string) ([]string, error) {
	stack := append([]string(nil), base...)
	pending := splitComponents(rel)
	hops := 0
	for len(pending) > 0 {
		comp := pending[0]
		pending = pending[1:]
		if comp == ".." {
			if len(stack) == 0 {
				return nil, ErrPathEscape
			}
			stack = stack[:len(stack)-1]
			continue
		}
		stack = append(stack, comp)
		full := filepath.Join(root, filepath.Join(stack...))
		fi, err := os.Lstat(full)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
				continue // join the rest lexically
			}
			return nil, err
		}
		if fi.Mode()&fs.ModeSymlink == 0 {
			continue
		}
		hops++
		if hops > maxSymlinkHops {
			return nil, ErrTooManySymlinks
		}
		target, err := os.Readlink(full)
		if err != nil {
			return nil, err
		}
		linkRel := filepath.Join(stack...)
		if filepath.IsAbs(target) {
			return nil, &OutOfBoundsSymlinkError{Path: linkRel, Target: target}
		}
		// A relative target is interpreted from the link's parent directory.
		stack = stack[:len(stack)-1]
		pending = append(splitComponents(target), pending...)
	}
	return stack, nil
}

// splitComponents splits p on the separator and drops empty and "." components.
func splitComponents(p string) []string {
	parts := strings.Split(p, string(filepath.Separator))
	out := parts[:0]
	for _, c := range parts {
		if c != "" && c != "." {
			out = append(out, c)
		}
	}
	return out
}

// CheckSymlinks walks root and returns *OutOfBoundsSymlinkError for the first
// symlink (in lexical walk order) that is absolute or resolves outside root.
//
// It mirrors upstream CheckOutOfBoundsSymlinks: absolute targets are always
// rejected, a relative target is walked component by component from the link's
// directory and rejected as soon as any step leaves root (even if it would come
// back), dangling targets that stay inside root are allowed, and entries that
// vanish during the walk are ignored. In addition, each target is resolved
// physically through other symlinks, so chains such as "d -> ." plus "x -> d/.."
// (lexically inside, physically outside) are rejected too. Loops are not escapes:
// the kernel cannot resolve them either.
//
// skip lists root-relative paths (for example ".git") whose subtrees are not checked.
func CheckSymlinks(root string, skip ...string) error {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("sandbox: resolve root: %w", err)
	}
	skipSet := make(map[string]bool, len(skip))
	for _, p := range skip {
		skipSet[filepath.Clean(p)] = true
	}
	return filepath.WalkDir(rootAbs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && p != rootAbs {
				return nil // e.g. transient .git/*.lock files
			}
			return fmt.Errorf("sandbox: walk %s for symlinks: %w", root, err)
		}
		if p == rootAbs {
			return nil
		}
		rel, err := filepath.Rel(rootAbs, p)
		if err != nil {
			return fmt.Errorf("sandbox: relative path for %s: %w", p, err)
		}
		if skipSet[rel] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink == 0 {
			return nil
		}
		return checkLink(rootAbs, rel)
	})
}

// checkLink validates one symlink at root-relative path rel whose parent
// directories are real directories (as produced by a non-following walk).
func checkLink(root, rel string) error {
	target, err := os.Readlink(filepath.Join(root, rel))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("sandbox: read link %s: %w", rel, err)
	}
	if err := checkLinkTarget(root, rel, target); err != nil {
		return err
	}
	return nil
}

// checkLinkTarget applies the lexical (upstream) and physical checks to a link at
// root-relative path rel with the given target.
func checkLinkTarget(root, rel, target string) error {
	oob := &OutOfBoundsSymlinkError{Path: rel, Target: target}
	if filepath.IsAbs(target) {
		return oob
	}
	if lexicallyEscapes(filepath.Dir(rel), target) {
		return oob
	}
	_, err := resolveWithin(root, splitComponents(filepath.Dir(rel)), target)
	switch {
	case err == nil, errors.Is(err, ErrTooManySymlinks):
		return nil
	case errors.Is(err, ErrPathEscape):
		return oob
	default:
		return fmt.Errorf("sandbox: resolve link %s: %w", rel, err)
	}
}

// lexicallyEscapes walks target from the root-relative directory dir one
// component at a time and reports whether any step leaves the root.
func lexicallyEscapes(dir, target string) bool {
	depth := len(splitComponents(dir))
	for _, c := range splitComponents(target) {
		if c == ".." {
			depth--
			if depth < 0 {
				return true
			}
			continue
		}
		depth++
	}
	return false
}
