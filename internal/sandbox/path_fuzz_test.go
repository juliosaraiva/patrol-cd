package sandbox

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// FuzzSecureJoin checks SecureJoin's invariants against a fixture tree full of
// in-bounds, escaping, absolute, chained and looping symlinks:
//   - absolute input always fails with ErrAbsolutePath;
//   - a successful result is root or lexically below it;
//   - a successful result, resolved by the OS (deepest existing prefix), stays
//     inside the real root;
//   - joining the result's root-relative remainder again is idempotent.
func FuzzSecureJoin(f *testing.F) {
	for _, seed := range []string{
		"", ".", "..", "a/b/file", "a/link-in/file", "../x", "a/../../x", "/etc/passwd",
		"up/x", "a/escape", "abs/passwd", "self/..", "self/self/a", "chain1/b", "loop1",
		"dangling", "p/q/a", "p/q/b/a", "a/deep/../file", "new/../../x", "a//b/./file",
		"a/b/file/..", "\x00", "a/\x00/b", strings.Repeat("self/", 300) + "a",
	} {
		f.Add(seed)
	}
	root := f.TempDir()
	buildTree(f, root, secureJoinFixture)
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		f.Fatal(err)
	}

	f.Fuzz(func(t *testing.T, rel string) {
		got, err := SecureJoin(root, rel)
		if filepath.IsAbs(rel) {
			if !errors.Is(err, ErrAbsolutePath) {
				t.Fatalf("SecureJoin(%q) = %q, %v; want ErrAbsolutePath", rel, got, err)
			}
			return
		}
		if err != nil {
			if got != "" {
				t.Fatalf("SecureJoin(%q) returned %q with error %v", rel, got, err)
			}
			return
		}
		if !within(root, got) {
			t.Fatalf("SecureJoin(%q) = %q, not within %q", rel, got, root)
		}
		existing := deepestExisting(t, got, root)
		resolved, err := filepath.EvalSymlinks(existing)
		if err != nil {
			t.Fatalf("EvalSymlinks(%q): %v", existing, err)
		}
		if !within(realRoot, resolved) {
			t.Fatalf("SecureJoin(%q) = %q resolves to %q outside %q", rel, got, resolved, realRoot)
		}
		rest, err := filepath.Rel(root, got)
		if err != nil {
			t.Fatal(err)
		}
		again, err := SecureJoin(root, rest)
		if err != nil || again != got {
			t.Fatalf("not idempotent: SecureJoin(%q) = %q, %v; want %q", rest, again, err, got)
		}
	})
}

// deepestExisting returns the longest prefix of p (not above root) that exists.
func deepestExisting(t *testing.T, p, root string) string {
	t.Helper()
	for {
		if _, err := os.Lstat(p); err == nil {
			return p
		} else if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
			t.Fatalf("lstat %q: %v", p, err)
		}
		if p == root {
			return p
		}
		p = filepath.Dir(p)
	}
}
