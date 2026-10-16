package reposerver

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/juliosaraiva/patrol-cd/internal/git"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
)

// Directory name prefixes under the root. Names after the prefix are random so they
// reveal nothing and never collide across processes.
const (
	prefixWorkingCopy = "r-"
	prefixStreamed    = "s-"
	prefixOCI         = "o-"
	prefixScratch     = "t-"
)

// workdirs manages persistent working copies: one per (repository location,
// credential fingerprint), so different credentials never share a checkout (a commit
// fetched with one credential cannot be read with another). Directories are created
// 0700, kept 0000 while idle (upstream directoryPermissionInitializer) and evicted
// least recently used when idle and over the bound.
//
// Each working copy owns its git.RepoLock, so the lock state goes away with the entry:
// rotating credentials (every rotation is a new fingerprint and a new working copy)
// or evicting copies never accumulates lock state for the life of the process.
type workdirs struct {
	root string
	max  int
	now  func() time.Time
	log  *slog.Logger

	mu      sync.Mutex
	entries map[string]*workdir
}

type workdir struct {
	key       string
	path      string
	refs      int
	lastUsed  time.Time
	expiresAt *time.Time
	// lock serializes use of path; it is only used while refs > 0, and entries are
	// only removed at refs == 0.
	lock git.RepoLock
}

func newWorkdirs(root string, maxEntries int, now func() time.Time, log *slog.Logger) *workdirs {
	return &workdirs{root: root, max: maxEntries, now: now, log: log, entries: map[string]*workdir{}}
}

func randomName(prefix string) (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b[:]), nil
}

// acquire returns the working copy directory of repo for credential fingerprint fp, its
// repository lock and a release function; the lock may be used until release. The
// directory exists (possibly empty) when acquire returns.
func (w *workdirs) acquire(repo apiclient.Repository, fp string) (string, *git.RepoLock, func(), error) {
	key := git.RepoKey(repo.URL) + "|" + fp
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	e, ok := w.entries[key]
	if ok && e.refs == 0 && e.expiresAt != nil && now.After(*e.expiresAt) {
		// Short-lived credentials expired: start over with a fresh directory.
		w.removeLocked(e)
		ok = false
	}
	if !ok {
		name, err := randomName(prefixWorkingCopy)
		if err != nil {
			return "", nil, nil, fmt.Errorf("create working copy name: %w", err)
		}
		e = &workdir{key: key, path: filepath.Join(w.root, name)}
		if err := os.Mkdir(e.path, 0o700); err != nil {
			return "", nil, nil, fmt.Errorf("create working copy: %w", err)
		}
		// Idle until the repository lock initializes it.
		if err := os.Chmod(e.path, 0o000); err != nil {
			w.log.Warn("reposerver: cannot restrict working copy permissions", slog.Any("error", err))
		}
		w.entries[key] = e
	}
	e.refs++
	e.lastUsed = now
	if repo.CredentialsExpireAt != nil {
		t := *repo.CredentialsExpireAt
		e.expiresAt = &t
	}
	w.evictLocked()
	var once sync.Once
	return e.path, &e.lock, func() {
		once.Do(func() {
			w.mu.Lock()
			defer w.mu.Unlock()
			e.refs--
			e.lastUsed = w.now()
			w.evictLocked()
		})
	}, nil
}

// evictLocked removes idle working copies beyond the bound (oldest first) and idle
// ones whose short-lived credentials expired.
func (w *workdirs) evictLocked() {
	now := w.now()
	for _, e := range w.entries {
		if e.refs == 0 && e.expiresAt != nil && now.After(*e.expiresAt) {
			w.removeLocked(e)
		}
	}
	if len(w.entries) <= w.max {
		return
	}
	idle := make([]*workdir, 0, len(w.entries))
	for _, e := range w.entries {
		if e.refs == 0 {
			idle = append(idle, e)
		}
	}
	sort.Slice(idle, func(i, j int) bool { return idle[i].lastUsed.Before(idle[j].lastUsed) })
	for _, e := range idle {
		if len(w.entries) <= w.max {
			return
		}
		w.removeLocked(e)
	}
}

func (w *workdirs) removeLocked(e *workdir) {
	delete(w.entries, e.key)
	if err := removeTree(e.path); err != nil {
		w.log.Warn("reposerver: cannot remove working copy", slog.String("dir", filepath.Base(e.path)), slog.Any("error", err))
	}
}

// count returns the number of tracked working copies.
func (w *workdirs) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.entries)
}

// paths returns the tracked working copy paths (tests).
func (w *workdirs) paths() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]string, 0, len(w.entries))
	for _, e := range w.entries {
		out = append(out, e.path)
	}
	sort.Strings(out)
	return out
}

// removeTree removes dir after restoring owner permissions on every directory (idle
// working copies are mode 0000).
func removeTree(dir string) error {
	if err := os.Chmod(dir, 0o700); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && d.Type()&fs.ModeSymlink == 0 {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	return os.RemoveAll(dir)
}

// permissionInit makes dir usable for the duration of a repository lock hold and
// locks it down again when the last holder releases (SRC-118).
func (w *workdirs) permissionInit(dir string) io.Closer {
	if err := os.Chmod(dir, 0o700); err != nil {
		w.log.Warn("reposerver: cannot restore working copy permissions", slog.String("dir", filepath.Base(dir)), slog.Any("error", err))
	}
	return closerFunc(func() error {
		if err := os.Chmod(dir, 0o000); err != nil && !os.IsNotExist(err) {
			w.log.Warn("reposerver: cannot restrict working copy permissions", slog.String("dir", filepath.Base(dir)), slog.Any("error", err))
		}
		return nil
	})
}

// cleanStale removes directories left under root by a previous process (only names
// this package creates are touched).
func cleanStale(root string, log *slog.Logger) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() {
			if strings.HasPrefix(name, prefixScratch) {
				_ = os.Remove(filepath.Join(root, name))
			}
			continue
		}
		for _, p := range []string{prefixWorkingCopy, prefixStreamed, prefixOCI, prefixScratch} {
			if strings.HasPrefix(name, p) {
				if err := removeTree(filepath.Join(root, name)); err != nil {
					log.Warn("reposerver: cannot remove stale directory", slog.String("dir", name), slog.Any("error", err))
				}
				break
			}
		}
	}
}

// tempDir creates a fresh private directory under root.
func tempDir(root, prefix string) (string, error) {
	name, err := randomName(prefix)
	if err != nil {
		return "", err
	}
	p := filepath.Join(root, name)
	if err := os.Mkdir(p, 0o700); err != nil {
		return "", err
	}
	return p, nil
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }
