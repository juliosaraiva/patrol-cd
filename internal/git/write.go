package git

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/juliosaraiva/patrol-cd/internal/sandbox"
)

// Notes push retry bounds (upstream AddAndPushNote).
const (
	noteRetryInitial = 50 * time.Millisecond
	noteRetryMax     = time.Second
	noteRetryElapsed = 5 * time.Second
)

// DefaultNotesNamespace is used when a notes namespace is empty.
const DefaultNotesNamespace = "commit"

// SetAuthor sets the identity used for commits and notes made by this client
// (GIT_AUTHOR_* and GIT_COMMITTER_*). Empty values leave git's configuration in
// effect. Values containing control characters or angle brackets are rejected.
func (c *Client) SetAuthor(name, email string) error {
	if strings.ContainsFunc(name+email, isControl) || strings.ContainsAny(name+email, "<>") {
		return fmt.Errorf("git: invalid author identity")
	}
	c.authorMu.Lock()
	defer c.authorMu.Unlock()
	c.authorName, c.authorEmail = name, email
	return nil
}

func (c *Client) authorEnv() []string {
	c.authorMu.RLock()
	defer c.authorMu.RUnlock()
	var env []string
	if c.authorName != "" {
		env = append(env, "GIT_AUTHOR_NAME="+c.authorName, "GIT_COMMITTER_NAME="+c.authorName)
	}
	if c.authorEmail != "" {
		env = append(env, "GIT_AUTHOR_EMAIL="+c.authorEmail, "GIT_COMMITTER_EMAIL="+c.authorEmail)
	}
	return env
}

// refExists reports whether the fully qualified ref exists locally.
func (c *Client) refExists(ctx context.Context, ref string) (bool, error) {
	_, err := c.run(ctx, cmdSpec{
		args:      []string{"rev-parse", "--verify", "--quiet", "--end-of-options", ref},
		maxStdout: maxSmallOutput,
	})
	if err == nil {
		return true, nil
	}
	var exitErr *sandbox.ExitError
	if errors.As(err, &exitErr) && exitErr.Code == 1 {
		return false, nil
	}
	return false, err
}

// checkoutBranch checks out an existing branch, preferring the remote-tracking
// ref (so the local branch is reset to the fetched tip). It reports false when
// neither refs/remotes/origin/<branch> nor refs/heads/<branch> exists.
func (c *Client) checkoutBranch(ctx context.Context, branch string) (bool, error) {
	remote := "refs/remotes/origin/" + branch
	ok, err := c.refExists(ctx, remote)
	if err != nil {
		return false, err
	}
	var args []string
	switch {
	case ok:
		args = []string{"checkout", "--force", "--quiet", "-B", branch, remote, "--"}
	default:
		local, err := c.refExists(ctx, "refs/heads/"+branch)
		if err != nil || !local {
			return false, err
		}
		args = []string{"checkout", "--force", "--quiet", branch, "--"}
	}
	if _, err := c.run(ctx, cmdSpec{args: args}); err != nil {
		return false, fmt.Errorf("failed to checkout branch: %w", err)
	}
	if _, err := c.run(ctx, cmdSpec{args: []string{"clean", "-ffdx", "--quiet"}}); err != nil {
		return false, fmt.Errorf("failed to clean: %w", err)
	}
	return true, nil
}

// CheckoutOrOrphan checks out branch (resetting it to origin/<branch> when the
// remote has it). When the branch exists neither remotely nor locally it creates
// an orphan branch with an empty "Initial commit for <branch>" and pushes it.
func (c *Client) CheckoutOrOrphan(ctx context.Context, branch string) error {
	if err := c.requireRepo(); err != nil {
		return err
	}
	if err := validateRefName("branch", branch); err != nil {
		return err
	}
	if ok, err := c.checkoutBranch(ctx, branch); err != nil || ok {
		return err
	}
	if _, err := c.run(ctx, cmdSpec{args: []string{"checkout", "--force", "--quiet", "--orphan", branch}}); err != nil {
		return fmt.Errorf("failed to create orphan branch: %w", err)
	}
	if _, err := c.run(ctx, cmdSpec{args: []string{"rm", "-r", "-q", "--cached", "--ignore-unmatch", "--", "."}}); err != nil {
		return fmt.Errorf("failed to clear orphan branch index: %w", err)
	}
	if _, err := c.run(ctx, cmdSpec{args: []string{"clean", "-ffdx", "--quiet"}}); err != nil {
		return fmt.Errorf("failed to clean: %w", err)
	}
	if _, err := c.run(ctx, cmdSpec{
		args:  []string{"commit", "--quiet", "--allow-empty", "--no-verify", "-F", "-"},
		env:   c.authorEnv(),
		stdin: strings.NewReader("Initial commit for " + branch),
	}); err != nil {
		return fmt.Errorf("failed to commit initial commit: %w", err)
	}
	if err := c.Push(ctx, branch, false); err != nil {
		return fmt.Errorf("failed to push to branch: %w", err)
	}
	return nil
}

// CheckoutOrNew checks out branch, or creates it from base (checked out first)
// when it does not exist remotely or locally. The new branch is not pushed.
func (c *Client) CheckoutOrNew(ctx context.Context, branch, base string) error {
	if err := c.requireRepo(); err != nil {
		return err
	}
	if err := validateRefName("branch", branch); err != nil {
		return err
	}
	if err := validateRefName("branch", base); err != nil {
		return err
	}
	if ok, err := c.checkoutBranch(ctx, branch); err != nil || ok {
		return err
	}
	ok, err := c.checkoutBranch(ctx, base)
	if err != nil {
		return fmt.Errorf("failed to checkout sync branch: %w", err)
	}
	if !ok {
		return fmt.Errorf("failed to checkout sync branch: %w", &RevisionNotFoundError{Revision: base})
	}
	if _, err := c.run(ctx, cmdSpec{args: []string{"checkout", "--quiet", "-b", branch}}); err != nil {
		return fmt.Errorf("failed to create branch: %w", err)
	}
	return nil
}

// RemoveContents removes the given paths (relative to root, literal, recursive)
// from the index and the working tree. Missing paths are ignored.
func (c *Client) RemoveContents(ctx context.Context, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	if err := c.requireRepo(); err != nil {
		return err
	}
	args := append([]string{"rm", "-r", "-q", "--ignore-unmatch", "--"}, paths...)
	if _, err := c.run(ctx, cmdSpec{args: args, env: []string{"GIT_LITERAL_PATHSPECS=1"}}); err != nil {
		return fmt.Errorf("failed to clear paths: %w", err)
	}
	return nil
}

// CommitAll stages every change in the working tree (additions, modifications,
// deletions) and commits it with message (read from stdin, never argv; only
// whitespace is cleaned up, '#' lines are kept), optionally adding a
// Signed-off-by trailer. It returns the new HEAD SHA, or the
// unchanged HEAD SHA together with ErrNothingToCommit when there is nothing to
// commit. Repository hooks are not run.
func (c *Client) CommitAll(ctx context.Context, message string, signoff bool) (string, error) {
	if err := c.requireRepo(); err != nil {
		return "", err
	}
	if _, err := c.run(ctx, cmdSpec{args: []string{"add", "--all", "--", "."}}); err != nil {
		return "", fmt.Errorf("failed to add files: %w", err)
	}
	_, err := c.run(ctx, cmdSpec{args: []string{"diff", "--cached", "--quiet"}})
	if err == nil {
		sha, shaErr := c.CommitSHA(ctx)
		if shaErr != nil {
			return "", shaErr
		}
		return sha, ErrNothingToCommit
	}
	var exitErr *sandbox.ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 1 {
		return "", fmt.Errorf("failed to inspect staged changes: %w", err)
	}
	// Upstream commits with -m, whose cleanup for an unedited message is
	// "whitespace": lines starting with '#' ("#123" references, markdown
	// headings) are kept.
	args := []string{"commit", "--quiet", "--no-verify", "--cleanup=whitespace", "-F", "-"}
	if signoff {
		args = append(args, "--signoff")
	}
	if _, err := c.run(ctx, cmdSpec{args: args, env: c.authorEnv(), stdin: strings.NewReader(message)}); err != nil {
		return "", fmt.Errorf("failed to commit: %w", err)
	}
	return c.CommitSHA(ctx)
}

// Push pushes the local branch to origin's branch of the same name (forced when
// force is true).
func (c *Client) Push(ctx context.Context, branch string, force bool) error {
	if err := c.requireRepo(); err != nil {
		return err
	}
	if err := validateRefName("branch", branch); err != nil {
		return err
	}
	args := []string{"push", "--quiet", "--no-verify"}
	if force {
		args = append(args, "--force")
	}
	ref := "refs/heads/" + branch
	args = append(args, "--", "origin", ref+":"+ref)
	if _, err := c.run(ctx, cmdSpec{args: args, credentialed: true}); err != nil {
		return fmt.Errorf("failed to push: %w", err)
	}
	return nil
}

func notesNamespace(ns string) (string, error) {
	if strings.TrimSpace(ns) == "" {
		ns = DefaultNotesNamespace
	}
	if err := validateRefName("notes namespace", ns); err != nil {
		return "", err
	}
	return ns, nil
}

// AddNote attaches note to commit sha in refs/notes/<namespace> (default
// "commit") and pushes the notes ref, overwriting an existing note for that
// commit. Like upstream it fetches the remote notes first and retries with
// exponential backoff (50ms..1s, at most 5s) when a concurrent writer updated the
// notes ref.
func (c *Client) AddNote(ctx context.Context, sha, namespace, note string) error {
	if err := c.requireRepo(); err != nil {
		return err
	}
	if err := validateRevisionArg(sha); err != nil {
		return err
	}
	ns, err := notesNamespace(namespace)
	if err != nil {
		return err
	}
	ref := "refs/notes/" + ns
	deadline := time.Now().Add(noteRetryElapsed)
	delay := noteRetryInitial
	for attempt := 1; ; attempt++ {
		if _, err := c.run(ctx, cmdSpec{args: []string{"fetch", "--quiet", "--", "origin", "+" + ref + ":" + ref}, credentialed: true}); err != nil &&
			!strings.Contains(err.Error(), "couldn't find remote ref") {
			c.log.Debug("git: failed to fetch notes (continuing)", slog.Any("error", err))
		}
		if _, err := c.run(ctx, cmdSpec{
			args:  []string{"notes", "--ref=" + ns, "add", "-f", "-F", "-", sha},
			env:   c.authorEnv(),
			stdin: strings.NewReader(note),
		}); err != nil {
			return fmt.Errorf("failed to add note: %w", err)
		}
		_, err := c.run(ctx, cmdSpec{args: []string{"push", "--quiet", "--no-verify", "--", "origin", ref}, credentialed: true})
		if err == nil {
			return nil
		}
		if !isRetryableNotePushError(err.Error()) || time.Now().Add(delay).After(deadline) {
			return fmt.Errorf("failed to push note after %d attempt(s): %w", attempt, err)
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("failed to push note: %w", errors.Join(err, ctx.Err()))
		case <-t.C:
		}
		delay = min(2*delay, noteRetryMax)
	}
}

func isRetryableNotePushError(msg string) bool {
	for _, m := range []string{"fetch first", "reference already exists", "incorrect old value", "failed to update ref", "cannot lock ref", "non-fast-forward"} {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}

// GetNote returns the note of commit sha in refs/notes/<namespace> (default
// "commit"), after a best-effort fetch of the remote notes ref. A commit without
// a note yields ErrNoteNotFound.
func (c *Client) GetNote(ctx context.Context, sha, namespace string) (string, error) {
	if err := c.requireRepo(); err != nil {
		return "", err
	}
	if err := validateRevisionArg(sha); err != nil {
		return "", err
	}
	ns, err := notesNamespace(namespace)
	if err != nil {
		return "", err
	}
	ref := "refs/notes/" + ns
	_, _ = c.run(ctx, cmdSpec{args: []string{"fetch", "--quiet", "--", "origin", "+" + ref + ":" + ref}, credentialed: true})
	out, err := c.git(ctx, cmdSpec{args: []string{"notes", "--ref=" + ns, "show", sha}, maxStdout: maxMetadataOutput})
	if err != nil {
		if strings.Contains(err.Error(), "no note found") {
			return "", fmt.Errorf("failed to get commit note: %w", ErrNoteNotFound)
		}
		return "", fmt.Errorf("failed to get commit note: %w", err)
	}
	return strings.TrimSpace(out), nil
}
