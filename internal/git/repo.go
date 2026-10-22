package git

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/juliosaraiva/patrol-cd/internal/sandbox"
)

// Init makes root a repository whose "origin" remote is the client's URL. An
// existing repository is kept when its origin is the client's location (RepoKey);
// a different spelling of that location only updates remote.origin.url. A
// repository whose origin is another location (or missing) is discarded, so
// refs, objects and files of one repository are never served for another. Any
// other content of root is removed first. The repository is created without
// templates (no sample hooks).
func (c *Client) Init(ctx context.Context) error {
	if c.root == "" {
		return fmt.Errorf("%w: client has no working copy root", ErrNotInitialized)
	}
	if c.isRepo() {
		keep, err := c.reconcileOrigin(ctx)
		if err != nil || keep {
			return err
		}
		c.log.Info("git: working copy belongs to another repository location, re-initializing",
			slog.String("repo", c.RepoURL()), slog.String("dir", filepath.Base(c.root)))
	}
	c.log.Info("git: initializing working copy", slog.String("repo", c.RepoURL()), slog.String("dir", filepath.Base(c.root)))
	if err := os.RemoveAll(c.root); err != nil {
		return fmt.Errorf("git: unable to clean repo dir: %w", err)
	}
	if err := os.MkdirAll(c.root, 0o755); err != nil {
		return fmt.Errorf("git: create repo dir: %w", err)
	}
	steps := [][]string{
		{"init", "--quiet", "--template="},
		{"config", "remote.origin.url", c.remoteURL},
		{"config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*"},
	}
	for _, args := range steps {
		if _, err := c.git(ctx, cmdSpec{args: args, maxStdout: maxSmallOutput}); err != nil {
			// A half-initialized repository would be taken as valid next time.
			_ = os.RemoveAll(filepath.Join(c.root, ".git"))
			return err
		}
	}
	return nil
}

// reconcileOrigin checks remote.origin.url of the existing working copy. It
// reports true when the working copy can be kept (the origin is the client's URL,
// or another spelling of the same location, which is then replaced by the
// client's URL) and false when it must be re-initialized (no origin, or another
// location). Timeouts and cancellation are returned as errors.
func (c *Client) reconcileOrigin(ctx context.Context) (bool, error) {
	out, err := c.git(ctx, cmdSpec{args: []string{"config", "--get-all", "remote.origin.url"}, maxStdout: maxSmallOutput})
	if err != nil {
		if exitedWithStatus(err) {
			return false, nil // missing key or unreadable configuration
		}
		return false, err
	}
	urls := lines(out)
	if len(urls) == 1 && urls[0] == c.remoteURL {
		return true, nil
	}
	if len(urls) == 0 {
		return false, nil
	}
	key := RepoKey(c.remoteURL)
	for _, u := range urls {
		if RepoKey(u) != key {
			return false, nil
		}
	}
	_, err = c.git(ctx, cmdSpec{args: []string{"config", "--replace-all", "remote.origin.url", c.remoteURL}, maxStdout: maxSmallOutput})
	if err != nil {
		if exitedWithStatus(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// exitedWithStatus reports whether err is a git command that ran and exited with
// a non-zero status (as opposed to a timeout, cancellation or start failure).
func exitedWithStatus(err error) bool {
	var exitErr *sandbox.ExitError
	return errors.As(err, &exitErr) && exitErr.Signal == 0 && !errors.Is(err, sandbox.ErrTimeout) &&
		!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

// Fetch fetches from origin: the given revision, or the default refspec plus all
// tags when revision is empty. The revision must be HEAD, an object name or a ref
// name; refspecs ("src:dst", "+src", globs) and revision expressions return
// ErrInvalidRevision. Fetches are forced and pruned; with a configured Depth they
// are shallow and do not fetch tags (upstream behavior). With LFS enabled, large
// files referenced by the current checkout are fetched too.
func (c *Client) Fetch(ctx context.Context, revision string) error {
	if err := c.requireRepo(); err != nil {
		return err
	}
	args := []string{"fetch", "--force", "--prune"}
	if c.opts.depth > 0 {
		args = append(args, "--depth", strconv.FormatInt(c.opts.depth, 10))
	} else {
		args = append(args, "--tags")
	}
	args = append(args, "--", "origin")
	if revision != "" {
		if err := validateFetchRevision(revision); err != nil {
			return err
		}
		args = append(args, revision)
	}
	err := c.withRetry(ctx, func() error {
		_, err := c.run(ctx, cmdSpec{args: args, credentialed: true, maxStdout: maxMetadataOutput})
		return err
	})
	if err != nil {
		c.cleanupOrphanedTempPackfiles()
		return err
	}
	if revision == "" {
		c.ensureRemoteHEAD(ctx)
	}
	if c.opts.enableLFS {
		files, err := c.lsLargeFiles(ctx)
		if err == nil && len(files) > 0 {
			if _, err := c.run(ctx, cmdSpec{args: []string{"lfs", "fetch", "--all"}, credentialed: true}); err != nil {
				return err
			}
		}
	}
	return nil
}

// ensureRemoteHEAD makes origin/HEAD resolve after a default fetch, because Checkout("")
// checks it out. Git 2.48 and later create it as part of fetch (remote.origin.followRemoteHEAD);
// earlier versions only set it on clone, and the working copy is created by init. A failure is
// logged, not returned: Checkout reports the missing ref, and CheckoutRevision falls back to
// fetching HEAD explicitly.
func (c *Client) ensureRemoteHEAD(ctx context.Context) {
	if _, err := c.git(ctx, cmdSpec{args: []string{"rev-parse", "--verify", "--quiet", "refs/remotes/origin/HEAD"}, maxStdout: maxSmallOutput}); err == nil {
		return
	}
	err := c.withRetry(ctx, func() error {
		_, err := c.run(ctx, cmdSpec{args: []string{"remote", "set-head", "origin", "--auto"}, credentialed: true, maxStdout: maxSmallOutput})
		return err
	})
	if err != nil && ctx.Err() == nil {
		c.log.Warn("git: failed to set origin/HEAD", slog.String("repo", c.RepoURL()), slog.Any("error", err))
	}
}

// cleanupOrphanedTempPackfiles removes temporary pack files left by a fetch that
// was killed (git never prunes them). Only files older than twice the command
// timeout are removed, so a concurrent fetch is never disturbed.
func (c *Client) cleanupOrphanedTempPackfiles() {
	packDir := filepath.Join(c.root, ".git", "objects", "pack")
	entries, err := os.ReadDir(packDir)
	if err != nil {
		return
	}
	timeout := c.opts.timeout
	if timeout <= 0 {
		timeout = sandbox.DefaultTimeout
	}
	grace := 2 * timeout
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !isGitTempFile(name) {
			continue
		}
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < grace {
			continue
		}
		if err := os.Remove(filepath.Join(packDir, name)); err != nil {
			c.log.Warn("git: failed to remove orphaned temp pack", slog.String("file", name), slog.Any("error", err))
		}
	}
}

// lsLargeFiles lists LFS-tracked files of the current checkout.
func (c *Client) lsLargeFiles(ctx context.Context) ([]string, error) {
	out, err := c.git(ctx, cmdSpec{args: []string{"lfs", "ls-files", "-n"}, maxStdout: maxMetadataOutput})
	if err != nil {
		return nil, err
	}
	return lines(out), nil
}

// IsRevisionPresent reports whether revision names a commit present in the
// working copy (upstream: `git cat-file -t` prints "commit").
func (c *Client) IsRevisionPresent(ctx context.Context, revision string) bool {
	if revision == "" || validateRevisionArg(revision) != nil || !c.isRepo() {
		return false
	}
	out, err := c.git(ctx, cmdSpec{args: []string{"cat-file", "-t", revision}, maxStdout: maxSmallOutput})
	return err == nil && strings.TrimSpace(out) == "commit"
}

// CommitSHA returns the commit checked out in the working copy.
func (c *Client) CommitSHA(ctx context.Context) (string, error) {
	if err := c.requireRepo(); err != nil {
		return "", err
	}
	out, err := c.git(ctx, cmdSpec{args: []string{"rev-parse", "HEAD"}, maxStdout: maxSmallOutput})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// Checkout checks out revision ("" and "HEAD" mean origin/HEAD) with --force,
// resets the index and working tree, populates LFS files (when enabled),
// synchronizes and updates submodules recursively (when submodules is true and
// the client's SubmodulesEnabled switch is on and .gitmodules exists), and finally
// removes every untracked and ignored file, including nested repositories
// (clean -ffdx). The revision must already be present (see CheckoutRevision).
func (c *Client) Checkout(ctx context.Context, revision string, submodules bool) error {
	if err := c.requireRepo(); err != nil {
		return err
	}
	if revision == "" || revision == "HEAD" {
		revision = "origin/HEAD"
	}
	if err := validateRevisionArg(revision); err != nil {
		return err
	}
	if _, err := c.run(ctx, cmdSpec{args: []string{"checkout", "--force", "--quiet", revision, "--"}}); err != nil {
		return fmt.Errorf("failed to checkout %s: %w", truncateForMessage(revision), err)
	}
	if _, err := c.run(ctx, cmdSpec{args: []string{"reset", "--hard", "--quiet", "HEAD", "--"}}); err != nil {
		return fmt.Errorf("failed to reset working tree: %w", err)
	}
	if c.opts.enableLFS {
		files, err := c.lsLargeFiles(ctx)
		if err != nil {
			return fmt.Errorf("failed to list LFS files: %w", err)
		}
		if len(files) > 0 {
			if _, err := c.run(ctx, cmdSpec{args: []string{"lfs", "pull"}, credentialed: true}); err != nil {
				return fmt.Errorf("failed to checkout LFS files: %w", err)
			}
		}
	}
	if submodules && c.opts.submodules {
		if _, err := os.Lstat(filepath.Join(c.root, ".gitmodules")); err == nil {
			if err := c.Submodule(ctx); err != nil {
				return fmt.Errorf("failed to update submodules: %w", err)
			}
		}
	}
	if _, err := c.run(ctx, cmdSpec{args: []string{"clean", "-ffdx", "--quiet"}}); err != nil {
		return fmt.Errorf("failed to clean: %w", err)
	}
	return nil
}

// Submodule synchronizes and updates submodules recursively with the client's
// credentials (upstream: submodules reuse the parent repository's credentials;
// here HTTPS credentials are only offered to the parent's origin).
func (c *Client) Submodule(ctx context.Context) error {
	if _, err := c.run(ctx, cmdSpec{args: []string{"submodule", "--quiet", "sync", "--recursive"}, credentialed: true}); err != nil {
		return err
	}
	_, err := c.run(ctx, cmdSpec{args: []string{"submodule", "--quiet", "update", "--init", "--recursive"}, credentialed: true})
	return err
}

// CheckoutRevision is upstream's repo-server checkout strategy: initialize the
// working copy, fetch only when the revision is not present (the revision itself
// at the configured depth, otherwise the default refspec to avoid bloat), check it
// out, and when that fails fetch the revision explicitly and check out FETCH_HEAD
// (refs outside the default refspec such as refs/pull/N/head). The revision is
// "" (origin/HEAD), HEAD, a full or abbreviated commit SHA, or a branch, tag or
// ref name; anything else (refspecs, revision expressions such as "main~1")
// returns ErrInvalidRevision before any command runs, because it may be passed
// to fetch.
func (c *Client) CheckoutRevision(ctx context.Context, revision string, submodules bool) error {
	if revision != "" {
		if err := validateFetchRevision(revision); err != nil {
			return err
		}
	}
	if err := c.Init(ctx); err != nil {
		return fmt.Errorf("failed to initialize git repo: %w", err)
	}
	if !c.IsRevisionPresent(ctx, revision) {
		var err error
		if c.opts.depth > 0 && revision != "" {
			err = c.Fetch(ctx, revision)
		} else {
			err = c.Fetch(ctx, "")
		}
		if err != nil {
			return fmt.Errorf("failed to fetch default: %w", err)
		}
	}
	err := c.Checkout(ctx, revision, submodules)
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrInvalidRevision) || ctx.Err() != nil {
		return err
	}
	c.log.Info("git: checkout failed, fetching revision explicitly", slog.String("repo", c.RepoURL()))
	fetchRev := revision
	if fetchRev == "" {
		fetchRev = "HEAD"
	}
	if err := c.Fetch(ctx, fetchRev); err != nil {
		return fmt.Errorf("failed to checkout revision %s: %w", truncateForMessage(revision), err)
	}
	if err := c.Checkout(ctx, "FETCH_HEAD", submodules); err != nil {
		return fmt.Errorf("failed to checkout FETCH_HEAD: %w", err)
	}
	return nil
}

// ChangedFiles returns the paths that differ between two commits (both full
// SHAs present locally). Renames are reported as deletion plus addition, so both
// the old and the new path are listed. Equal revisions yield an empty list.
func (c *Client) ChangedFiles(ctx context.Context, from, to string) ([]string, error) {
	if from == to {
		return []string{}, nil
	}
	if !IsCommitSHA(from) || !IsCommitSHA(to) {
		return []string{}, fmt.Errorf("%w: invalid revision provided, must be SHA", ErrInvalidRevision)
	}
	if err := c.requireRepo(); err != nil {
		return nil, err
	}
	res, err := c.run(ctx, cmdSpec{
		args:      []string{"diff", "--name-only", "--no-renames", "-z", from + ".." + to, "--"},
		maxStdout: maxMetadataOutput,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to diff %s..%s: %w", from, to, err)
	}
	return splitNUL(res.Stdout), nil
}

// isGitTempFile reports whether name is a temporary file git leaves in objects/pack when
// an operation is interrupted.
func isGitTempFile(name string) bool {
	for _, prefix := range []string{"tmp_pack_", "tmp_idx_", "tmp_rev_", "tmp_mtimes_"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}
