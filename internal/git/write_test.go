package git

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newWriteClient(t *testing.T, f *remoteFixture) *Client {
	t.Helper()
	c := newTestClient(t, f.url(), nil)
	require.NoError(t, c.SetAuthor("Patrol Hydrator", "hydrator@example.com"))
	ctx := testCtx(t)
	require.NoError(t, c.Init(ctx))
	require.NoError(t, c.Fetch(ctx, ""))
	return c
}

func TestHydratorWriteFlow(t *testing.T) {
	t.Parallel()
	f := newRevisionFixture(t)
	ctx := testCtx(t)
	c := newWriteClient(t, f.remoteFixture)

	// New branch: orphan with an empty initial commit, pushed.
	require.NoError(t, c.CheckoutOrOrphan(ctx, "env/prod"))
	initial := f.bareGit("rev-parse", "refs/heads/env/prod")
	assert.Equal(t, "Initial commit for env/prod", f.bareGit("log", "-1", "--format=%s", "env/prod"))
	assert.Equal(t, "", f.bareGit("ls-tree", "--name-only", "env/prod"), "orphan branch starts empty")
	assert.NoFileExists(t, filepath.Join(c.Root(), "README"), "files of the previous checkout are gone")

	// Write, commit with signoff, push.
	require.NoError(t, os.MkdirAll(filepath.Join(c.Root(), "app"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(c.Root(), "app", "manifest.yaml"), []byte("kind: ConfigMap\n"), 0o644))
	msg := "--hydrate from abc\n\nArgocd-reference-commit-sha: abc1234"
	sha, err := c.CommitAll(ctx, msg, true)
	require.NoError(t, err)
	require.NoError(t, c.Push(ctx, "env/prod", false))
	assert.Equal(t, sha, f.bareGit("rev-parse", "refs/heads/env/prod"))
	assert.Equal(t, initial, f.bareGit("rev-parse", "env/prod~1"))
	body := f.bareGit("log", "-1", "--format=%B", "env/prod")
	assert.True(t, strings.HasPrefix(body, "--hydrate from abc"), body)
	assert.Contains(t, body, "Signed-off-by: Patrol Hydrator <hydrator@example.com>")
	assert.Equal(t, "Patrol Hydrator <hydrator@example.com>", f.bareGit("log", "-1", "--format=%an <%ae>", "env/prod"))

	// Nothing changed: unchanged HEAD and ErrNothingToCommit.
	same, err := c.CommitAll(ctx, "noop", false)
	assert.ErrorIs(t, err, ErrNothingToCommit)
	assert.Equal(t, sha, same)

	// RemoveContents + commit deletes files.
	require.NoError(t, c.RemoveContents(ctx, []string{"app", "does-not-exist", ":(glob)*"}))
	assert.NoFileExists(t, filepath.Join(c.Root(), "app", "manifest.yaml"))
	sha2, err := c.CommitAll(ctx, "remove", false)
	require.NoError(t, err)
	assert.NotEqual(t, sha, sha2)

	// Existing remote branch: a fresh clone checks out the remote tip.
	other := newWriteClient(t, f.remoteFixture)
	require.NoError(t, c.Push(ctx, "env/prod", false))
	require.NoError(t, other.Fetch(ctx, ""))
	require.NoError(t, other.CheckoutOrOrphan(ctx, "env/prod"))
	head, err := other.CommitSHA(ctx)
	require.NoError(t, err)
	assert.Equal(t, sha2, head)

	// Diverged history needs force.
	require.NoError(t, os.WriteFile(filepath.Join(other.Root(), "x"), []byte("x"), 0o644))
	_, err = other.CommitAll(ctx, "other", false)
	require.NoError(t, err)
	require.NoError(t, other.Push(ctx, "env/prod", false))
	require.NoError(t, os.WriteFile(filepath.Join(c.Root(), "y"), []byte("y"), 0o644))
	mine, err := c.CommitAll(ctx, "mine", false)
	require.NoError(t, err)
	require.Error(t, c.Push(ctx, "env/prod", false), "non-fast-forward push is rejected")
	require.NoError(t, c.Push(ctx, "env/prod", true))
	assert.Equal(t, mine, f.bareGit("rev-parse", "refs/heads/env/prod"))
}

func TestCheckoutOrNew(t *testing.T) {
	t.Parallel()
	f := newRevisionFixture(t)
	ctx := testCtx(t)
	c := newWriteClient(t, f.remoteFixture)
	require.NoError(t, c.CheckoutOrNew(ctx, "env/staging-next", "feature"))
	head, err := c.CommitSHA(ctx)
	require.NoError(t, err)
	assert.Equal(t, f.c2, head, "new branch starts at base")
	branch, err := c.git(ctx, cmdSpec{args: []string{"symbolic-ref", "--short", "HEAD"}})
	require.NoError(t, err)
	assert.Equal(t, "env/staging-next", branch)

	require.NoError(t, c.CheckoutOrNew(ctx, "main", "feature"))
	head, err = c.CommitSHA(ctx)
	require.NoError(t, err)
	assert.Equal(t, f.c4, head, "existing branch is checked out")

	err = c.CheckoutOrNew(ctx, "new", "no-such-base")
	assert.ErrorIs(t, err, ErrRevisionNotFound)
}

func TestNotes(t *testing.T) {
	t.Parallel()
	f := newRevisionFixture(t)
	ctx := testCtx(t)
	c := newWriteClient(t, f.remoteFixture)

	_, err := c.GetNote(ctx, f.c1, "hydrator.metadata")
	require.ErrorIs(t, err, ErrNoteNotFound)

	note := `{"drySha":"` + f.c1 + `"}`
	require.NoError(t, c.AddNote(ctx, f.c1, "hydrator.metadata", note))
	assert.Equal(t, note, f.bareGit("notes", "--ref=hydrator.metadata", "show", f.c1))

	// Another working copy sees the pushed note; overwriting works.
	other := newWriteClient(t, f.remoteFixture)
	got, err := other.GetNote(ctx, f.c1, "hydrator.metadata")
	require.NoError(t, err)
	assert.Equal(t, note, got)
	require.NoError(t, other.AddNote(ctx, f.c1, "hydrator.metadata", "v2"))
	got, err = c.GetNote(ctx, f.c1, "hydrator.metadata")
	require.NoError(t, err)
	assert.Equal(t, "v2", got)

	// Default namespace.
	require.NoError(t, c.AddNote(ctx, f.c2, "", "default ns"))
	assert.Equal(t, "default ns", f.bareGit("notes", "--ref=commit", "show", f.c2))

	// Concurrent writers on different commits both land (retry on rejected push).
	var wg sync.WaitGroup
	for i, cl := range []*Client{c, other} {
		wg.Go(func() {
			assert.NoError(t, cl.AddNote(ctx, []string{f.c3, f.c4}[i], "race", "n"))
		})
	}
	wg.Wait()
	assert.Equal(t, "n", f.bareGit("notes", "--ref=race", "show", f.c3))
	assert.Equal(t, "n", f.bareGit("notes", "--ref=race", "show", f.c4))

	assert.ErrorIs(t, c.AddNote(ctx, f.c1, "../evil", "x"), ErrInvalidRevision)
	assert.ErrorIs(t, c.AddNote(ctx, "-f", "ns", "x"), ErrInvalidRevision)
}

func TestWriteValidation(t *testing.T) {
	t.Parallel()
	f := newRevisionFixture(t)
	ctx := testCtx(t)
	c := newWriteClient(t, f.remoteFixture)
	for _, b := range []string{"", "-x", "a..b", "a b", "a~1", "x.lock", ".hidden", "a//b", "a/", "@", "a@{1}", "a:b"} {
		assert.ErrorIs(t, c.CheckoutOrOrphan(ctx, b), ErrInvalidRevision, "branch %q", b)
		assert.ErrorIs(t, c.Push(ctx, b, false), ErrInvalidRevision, "branch %q", b)
	}
	assert.Error(t, c.SetAuthor("Evil\nName", "x@example.com"))
	assert.Error(t, c.SetAuthor("Name", "<x@example.com>"))
}

// TestCommitAllKeepsHashLines is a regression test: CommitAll used
// --cleanup=strip, which deletes every line starting with '#' ("#123" issue
// references, markdown headings). Upstream commits with -m, whose default cleanup
// for an unedited message is "whitespace".
func TestCommitAllKeepsHashLines(t *testing.T) {
	t.Parallel()
	f := newRevisionFixture(t)
	ctx := testCtx(t)
	c := newWriteClient(t, f.remoteFixture)
	require.NoError(t, c.CheckoutOrOrphan(ctx, "env/hash"))
	require.NoError(t, os.WriteFile(filepath.Join(c.Root(), "m.yaml"), []byte("x"), 0o644))

	msg := "\n\nHydrate   \n\n\n# Heading line\n#123 fixes issue\n\tbody\n\n"
	sha, err := c.CommitAll(ctx, msg, false)
	require.NoError(t, err)
	body, err := c.git(ctx, cmdSpec{args: []string{"log", "-1", "--format=%B", sha}})
	require.NoError(t, err)
	// Whitespace cleanup only: leading/trailing blank lines and trailing spaces
	// removed, consecutive blank lines collapsed, '#' lines kept.
	assert.Equal(t, "Hydrate\n\n# Heading line\n#123 fixes issue\n\tbody", strings.TrimRight(body, "\n"))

	// Notes keep '#' lines too.
	note := "# metadata\n#42"
	require.NoError(t, c.AddNote(ctx, sha, "hash", note))
	got, err := c.GetNote(ctx, sha, "hash")
	require.NoError(t, err)
	assert.Equal(t, note, got)
}
