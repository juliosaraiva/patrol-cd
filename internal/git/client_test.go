package git

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// revisionFixture builds:
//
//	c1  tag v1.0.0 (annotated), tag light (lightweight)
//	c2  tags v1.9.0 (annotated), app1/v1.0.1, app1/v1.1.0-rc.1; branch feature
//	c3  tag v2.0.0; ref refs/pull/1/head
//	c4  main (HEAD), tag v2.1.0-rc.1
type revisionFixture struct {
	*remoteFixture
	c1, c2, c3, c4 string
}

func newRevisionFixture(t *testing.T) *revisionFixture {
	t.Helper()
	f := &revisionFixture{remoteFixture: newRemoteFixture(t)}
	f.c1 = f.commit("first", map[string]string{"app/a.yaml": "a: 1\n", "README": "one\n"})
	f.git("tag", "-a", "v1.0.0", "-m", "release 1.0.0")
	f.git("tag", "light")
	f.c2 = f.commit("second", map[string]string{"app/a.yaml": "a: 2\n", "app/b.yaml": "b: 1\n"})
	f.git("tag", "-a", "v1.9.0", "-m", "release 1.9.0")
	f.git("tag", "app1/v1.0.1")
	f.git("tag", "app1/v1.1.0-rc.1")
	f.git("branch", "feature")
	f.c3 = f.commit("third", map[string]string{"other/c.yaml": "c: 1\n"})
	f.git("tag", "v2.0.0")
	f.c4 = f.commit("fourth", map[string]string{"README": "four\n"})
	f.git("tag", "v2.1.0-rc.1")
	f.push("main", "feature", "--tags")
	f.push(f.c3 + ":refs/pull/1/head")
	return f
}

func TestResolveRevision(t *testing.T) {
	t.Parallel()
	f := newRevisionFixture(t)
	ctx := testCtx(t)

	tests := []struct {
		name      string
		revision  string
		tagPrefix string
		want      string
	}{
		{"empty is HEAD", "", "", f.c4},
		{"HEAD", "HEAD", "", f.c4},
		{"branch short", "main", "", f.c4},
		{"branch full", "refs/heads/main", "", f.c4},
		{"other branch", "feature", "", f.c2},
		{"annotated tag peeled", "v1.0.0", "", f.c1},
		{"annotated tag full name", "refs/tags/v1.9.0", "", f.c2},
		{"lightweight tag", "light", "", f.c1},
		{"semver range excludes v2", ">=1.0.0 <2.0.0", "", f.c2},
		{"semver star excludes prerelease", "*", "", f.c3},
		{"semver star -0 includes prerelease", "*-0", "", f.c4},
		{"semver wildcard minor", "1.9.*", "", f.c2},
		{"semver exact version is a literal tag", "v2.0.0", "", f.c3},
		{"tag prefix constraint", "v1.0.*", "app1/", f.c2},
		{"tag prefix prerelease", ">=v1.0.0-0", "app1/", f.c2},
		{"tag prefix with branch falls back", "main", "app1/", f.c4},
		{"non standard ref full", "refs/pull/1/head", "", f.c3},
		{"non standard ref short", "pull/1/head", "", f.c3},
		{"full sha returned as is", f.c2, "", f.c2},
		{"truncated sha expanded", f.c2[:7], "", f.c2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newTestClient(t, f.url(), nil, TagPrefix(tt.tagPrefix))
			got, err := c.ResolveRevision(ctx, tt.revision)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestResolveRevisionNotFound(t *testing.T) {
	t.Parallel()
	f := newRevisionFixture(t)
	ctx := testCtx(t)
	for _, rev := range []string{"doesnotexist", ">=5.0.0", "deadbee"} {
		t.Run(rev, func(t *testing.T) {
			t.Parallel()
			c := newTestClient(t, f.url(), nil)
			_, err := c.ResolveRevision(ctx, rev)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrRevisionNotFound)
			assert.Equal(t, "unable to resolve '"+rev+"' to a commit SHA", err.Error())
		})
	}
}

func TestResolveRevisionFullSHADoesNotContactRemote(t *testing.T) {
	t.Parallel()
	rec := &cmdRecorder{}
	c := newTestClient(t, "https://example.invalid/repo.git", nil, withRunHook(rec.hook))
	sha := "0123456789abcdef0123456789abcdef01234567"
	got, err := c.ResolveRevision(testCtx(t), sha)
	require.NoError(t, err)
	assert.Equal(t, sha, got)
	assert.Empty(t, rec.all())
}

func TestLsRemote(t *testing.T) {
	t.Parallel()
	f := newRevisionFixture(t)
	c := newTestClient(t, f.url(), nil)
	refs, err := c.LsRemote(testCtx(t))
	require.NoError(t, err)
	assert.Equal(t, []string{"feature", "main"}, refs.Branches)
	assert.Equal(t, []string{"app1/v1.0.1", "app1/v1.1.0-rc.1", "light", "v1.0.0", "v1.9.0", "v2.0.0", "v2.1.0-rc.1"}, refs.Tags)
	assert.Equal(t, f.c4, refs.HEAD)
	assert.Equal(t, "refs/heads/main", refs.HEADRef)
}

func TestLsRemoteEmptyRepository(t *testing.T) {
	t.Parallel()
	f := newRemoteFixture(t)
	c := newTestClient(t, f.url(), nil)
	refs, err := c.LsRemote(testCtx(t))
	require.NoError(t, err)
	assert.Equal(t, []string{}, refs.Branches)
	assert.Equal(t, []string{}, refs.Tags)
	assert.Empty(t, refs.HEAD)
}

func TestLsRemoteMissingRepository(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, "file://"+filepath.Join(t.TempDir(), "missing.git"), nil)
	_, err := c.LsRemote(testCtx(t))
	require.Error(t, err)
	var ce *CommandError
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, "ls-remote", ce.Op)
	assert.Contains(t, err.Error(), "failed to list refs")
}

func TestRefCacheCoalescesAndExpires(t *testing.T) {
	t.Parallel()
	f := newRevisionFixture(t)
	ctx := testCtx(t)
	cache := NewRefCache(time.Minute, 10)
	now := time.Now()
	var clockMu sync.Mutex
	cache.now = func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return now }
	rec := &cmdRecorder{}

	c := newTestClient(t, f.url(), nil, WithRefCache(cache, true), withRunHook(rec.hook))
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			sha, err := c.ResolveRevision(ctx, "main")
			assert.NoError(t, err)
			assert.Equal(t, f.c4, sha)
		})
	}
	wg.Wait()
	assert.Equal(t, 1, rec.count("ls-remote"), "concurrent resolutions share one listing")

	// Another client with the same URL (different spelling) and credentials hits the cache.
	other := newTestClient(t, f.url()+"/", nil, WithRefCache(cache, true), withRunHook(rec.hook))
	_, err := other.LsRemote(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, rec.count("ls-remote"))

	// Different credentials never share a listing.
	creds := newTestClient(t, f.url(), HTTPSCreds{Username: "u", Password: "p"}, WithRefCache(cache, true), withRunHook(rec.hook))
	_, err = creds.LsRemote(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, rec.count("ls-remote"))

	// noRevisionCache: bypass reads, still refresh the entry.
	fresh := newTestClient(t, f.url(), nil, WithRefCache(cache, false), withRunHook(rec.hook))
	_, err = fresh.LsRemote(ctx)
	require.NoError(t, err)
	assert.Equal(t, 3, rec.count("ls-remote"))

	// Expiry.
	clockMu.Lock()
	now = now.Add(2 * time.Minute)
	clockMu.Unlock()
	_, err = c.LsRemote(ctx)
	require.NoError(t, err)
	assert.Equal(t, 4, rec.count("ls-remote"))

	cache.Invalidate(f.url())
	assert.Equal(t, 0, cache.Len())
}

func TestRefCacheBounded(t *testing.T) {
	t.Parallel()
	cache := NewRefCache(time.Minute, 3)
	for i := range 10 {
		cache.set(string(rune('a'+i)), "g", &refList{})
	}
	assert.LessOrEqual(t, cache.Len(), 3)
}

func TestInitFetchCheckoutAndClean(t *testing.T) {
	t.Parallel()
	f := newRevisionFixture(t)
	ctx := testCtx(t)
	c := newTestClient(t, f.url(), nil)

	require.ErrorIs(t, c.Fetch(ctx, ""), ErrNotInitialized)
	require.NoError(t, c.Init(ctx))
	require.NoError(t, c.Init(ctx), "Init is idempotent")
	assert.False(t, c.IsRevisionPresent(ctx, f.c1))
	require.NoError(t, c.Fetch(ctx, ""))
	assert.True(t, c.IsRevisionPresent(ctx, f.c1))
	assert.False(t, c.IsRevisionPresent(ctx, "v1.0.0"), "annotated tag object is not a commit")
	assert.False(t, c.IsRevisionPresent(ctx, "-v"))

	require.NoError(t, c.Checkout(ctx, f.c1, false))
	sha, err := c.CommitSHA(ctx)
	require.NoError(t, err)
	assert.Equal(t, f.c1, sha)
	assert.Equal(t, "a: 1\n", readFile(t, filepath.Join(c.Root(), "app/a.yaml")))

	// Dirty the working copy: modified tracked file, untracked file, ignored-like
	// nested directory, nested repository.
	require.NoError(t, os.WriteFile(filepath.Join(c.Root(), "app/a.yaml"), []byte("tampered"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(c.Root(), "untracked.txt"), []byte("x"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(c.Root(), "charts/dep/.git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(c.Root(), "charts/dep/.git/HEAD"), []byte("ref: refs/heads/main\n"), 0o644))

	require.NoError(t, c.Checkout(ctx, f.c1, false))
	assert.Equal(t, "a: 1\n", readFile(t, filepath.Join(c.Root(), "app/a.yaml")))
	assert.NoFileExists(t, filepath.Join(c.Root(), "untracked.txt"))
	assert.NoDirExists(t, filepath.Join(c.Root(), "charts"))

	require.NoError(t, c.Checkout(ctx, "", false))
	sha, err = c.CommitSHA(ctx)
	require.NoError(t, err)
	assert.Equal(t, f.c4, sha, "empty revision checks out origin/HEAD")

	err = c.Checkout(ctx, "nosuchrev", false)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRevisionNotFound)
	assert.ErrorIs(t, c.Checkout(ctx, "--orphan", false), ErrInvalidRevision)
}

func TestInitReplacesNonRepositoryContent(t *testing.T) {
	t.Parallel()
	f := newRevisionFixture(t)
	root := filepath.Join(t.TempDir(), "wc")
	require.NoError(t, os.MkdirAll(root, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "junk"), []byte("x"), 0o644))
	c, err := New(f.url(), root, nil, AllowLocalRepositories(true))
	require.NoError(t, err)
	require.NoError(t, c.CheckoutRevision(testCtx(t), "main", false))
	assert.NoFileExists(t, filepath.Join(root, "junk"))
}

func TestCheckoutRevisionStrategy(t *testing.T) {
	t.Parallel()
	f := newRevisionFixture(t)
	ctx := testCtx(t)
	rec := &cmdRecorder{}
	c := newTestClient(t, f.url(), nil, withRunHook(rec.hook))

	require.NoError(t, c.CheckoutRevision(ctx, f.c2, false))
	assert.Equal(t, 1, rec.count("fetch"))
	sha, err := c.CommitSHA(ctx)
	require.NoError(t, err)
	assert.Equal(t, f.c2, sha)

	// Present revision: no fetch.
	require.NoError(t, c.CheckoutRevision(ctx, f.c1, false))
	assert.Equal(t, 1, rec.count("fetch"))

	// A ref outside the default refspec needs the explicit fetch + FETCH_HEAD fallback.
	require.NoError(t, c.CheckoutRevision(ctx, "refs/pull/1/head", false))
	sha, err = c.CommitSHA(ctx)
	require.NoError(t, err)
	assert.Equal(t, f.c3, sha)
	assert.Equal(t, 3, rec.count("fetch"))
}

func TestShallowFetch(t *testing.T) {
	t.Parallel()
	f := newRevisionFixture(t)
	ctx := testCtx(t)
	rec := &cmdRecorder{}
	c := newTestClient(t, f.url(), nil, Depth(1), withRunHook(rec.hook))
	require.NoError(t, c.CheckoutRevision(ctx, f.c3, false))
	sha, err := c.CommitSHA(ctx)
	require.NoError(t, err)
	assert.Equal(t, f.c3, sha)

	shallow, err := c.git(ctx, cmdSpec{args: []string{"rev-parse", "--is-shallow-repository"}})
	require.NoError(t, err)
	assert.Equal(t, "true", shallow)
	count, err := c.git(ctx, cmdSpec{args: []string{"rev-list", "--count", "HEAD"}})
	require.NoError(t, err)
	assert.Equal(t, "1", count)

	for _, cmd := range rec.all() {
		if cmd.Args[0] == "fetch" {
			assert.Contains(t, cmd.Args, "--depth")
			assert.NotContains(t, cmd.Args, "--tags", "shallow fetches do not fetch tags")
		}
	}
}

func TestSubmodules(t *testing.T) {
	t.Parallel()
	sub := newRemoteFixture(t)
	sub.commit("sub", map[string]string{"manifests/cm.yaml": "kind: ConfigMap\n"})
	sub.push("main")

	parent := newRemoteFixture(t)
	parent.commit("init", map[string]string{"README": "parent\n"})
	parent.git("-c", "protocol.file.allow=always", "submodule", "add", "--quiet", sub.url(), "deps/sub")
	parent.git("commit", "--quiet", "-m", "add submodule")
	parent.push("main")

	ctx := testCtx(t)
	t.Run("enabled", func(t *testing.T) {
		t.Parallel()
		c := newTestClient(t, parent.url(), nil, withGitConfig("protocol.file.allow", "always"))
		require.NoError(t, c.CheckoutRevision(ctx, "main", true))
		assert.Equal(t, "kind: ConfigMap\n", readFile(t, filepath.Join(c.Root(), "deps/sub/manifests/cm.yaml")))
		// A second checkout keeps the submodule content and stays clean.
		require.NoError(t, c.Checkout(ctx, "main", true))
		assert.FileExists(t, filepath.Join(c.Root(), "deps/sub/manifests/cm.yaml"))
	})
	t.Run("disabled per call", func(t *testing.T) {
		t.Parallel()
		c := newTestClient(t, parent.url(), nil, withGitConfig("protocol.file.allow", "always"))
		require.NoError(t, c.CheckoutRevision(ctx, "main", false))
		assert.NoFileExists(t, filepath.Join(c.Root(), "deps/sub/manifests/cm.yaml"))
	})
	t.Run("disabled server wide", func(t *testing.T) {
		t.Parallel()
		c := newTestClient(t, parent.url(), nil, SubmodulesEnabled(false), withGitConfig("protocol.file.allow", "always"))
		require.NoError(t, c.CheckoutRevision(ctx, "main", true))
		assert.NoFileExists(t, filepath.Join(c.Root(), "deps/sub/manifests/cm.yaml"))
	})
	t.Run("default protocol policy refuses local submodules", func(t *testing.T) {
		t.Parallel()
		c := newTestClient(t, parent.url(), nil)
		err := c.CheckoutRevision(ctx, "main", true)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to update submodules")
	})
}

func TestChangedFiles(t *testing.T) {
	t.Parallel()
	f := newRevisionFixture(t)
	c5 := f.commit("rename", map[string]string{"other/c.yaml": "", "other/renamed.yaml": "c: 1\n"})
	f.push("main")
	ctx := testCtx(t)
	c := newTestClient(t, f.url(), nil)
	require.NoError(t, c.CheckoutRevision(ctx, "main", false))

	files, err := c.ChangedFiles(ctx, f.c1, f.c2)
	require.NoError(t, err)
	assert.Equal(t, []string{"app/a.yaml", "app/b.yaml"}, files)

	files, err = c.ChangedFiles(ctx, f.c4, c5)
	require.NoError(t, err)
	assert.Equal(t, []string{"other/c.yaml", "other/renamed.yaml"}, files, "renames list both paths")

	files, err = c.ChangedFiles(ctx, f.c2, f.c2)
	require.NoError(t, err)
	assert.Empty(t, files)

	_, err = c.ChangedFiles(ctx, "main", f.c2)
	assert.ErrorIs(t, err, ErrInvalidRevision)
}

func TestRevisionMetadata(t *testing.T) {
	t.Parallel()
	f := newRemoteFixture(t)
	msg := "Hydrate manifests\n\nSome body text.\n\n" +
		"Argocd-reference-commit-author: Dry Author <dry@example.com>\n" +
		"Argocd-reference-commit-sha: abc1234\n" +
		"Argocd-reference-commit-date: 2024-01-02T03:04:05Z\n" +
		"Argocd-reference-commit-subject: Original subject\n" +
		"Argocd-reference-commit-body: \"line1\\nline2\"\n" +
		"Argocd-reference-commit-repourl: https://example.com/org/dry.git\n" +
		"Signed-off-by: Someone <someone@example.com>"
	sha := f.commit(msg, map[string]string{"x": "y\n"})
	f.git("tag", "v9.9.9")
	f.git("tag", "-a", "annotated", "-m", "x")
	f.push("main", "--tags")

	ctx := testCtx(t)
	c := newTestClient(t, f.url(), nil)
	require.NoError(t, c.CheckoutRevision(ctx, sha, false))
	m, err := c.RevisionMetadata(ctx, sha)
	require.NoError(t, err)
	assert.Equal(t, "Fixture Author <author@example.com>", m.Author)
	assert.WithinDuration(t, time.Now(), m.Date, 10*time.Minute)
	assert.Equal(t, []string{"annotated", "v9.9.9"}, m.Tags)
	assert.Equal(t, msg, m.Message)
	require.Len(t, m.References, 1)
	assert.Equal(t, CommitMetadata{
		Author:  `"Dry Author" <dry@example.com>`,
		Date:    "2024-01-02T03:04:05Z",
		Subject: "Original subject",
		Body:    "line1\nline2",
		SHA:     "abc1234",
		RepoURL: "https://example.com/org/dry.git",
	}, *m.References[0].Commit)
	assert.Nil(t, m.Signature)

	_, err = c.RevisionMetadata(ctx, "--help")
	assert.ErrorIs(t, err, ErrInvalidRevision)
}

func TestParseReferences(t *testing.T) {
	t.Parallel()
	refs, rest := ParseReferences(nil, "Argocd-reference-commit-sha: NOTHEX\n"+
		"Argocd-reference-commit-date: yesterday\n"+
		"Argocd-reference-commit-body: not-json\n"+
		"Argocd-reference-commit-author: not an address\n"+
		"Argocd-reference-commit-unknown: x\n"+
		"Other: y\n")
	assert.Nil(t, refs, "invalid trailers produce no reference")
	assert.Equal(t, "Argocd-reference-commit-sha: NOTHEX\nArgocd-reference-commit-date: yesterday\n"+
		"Argocd-reference-commit-body: not-json\nArgocd-reference-commit-author: not an address\n"+
		"Argocd-reference-commit-unknown: x\nOther: y\n", rest)

	refs, rest = ParseReferences(nil, "Argocd-reference-commit-subject: s\nKeep: me\n")
	require.Len(t, refs, 1)
	assert.Equal(t, "s", refs[0].Commit.Subject)
	assert.Equal(t, "Keep: me\n", rest)
}

func TestNewRejectsUnsafeInput(t *testing.T) {
	t.Parallel()
	for _, u := range []string{"", "-uhelp", "ext::sh -c touch% /tmp/pwned", "fd::17", "ftp://example.com/x", "https://", "https://exa mple.com/x", "https://example.com/\nx"} {
		_, err := New(u, t.TempDir(), nil)
		assert.ErrorIs(t, err, ErrInvalidURL, "url %q", u)
	}
	for _, u := range []string{"https://example.com/org/repo.git", "git@github.com:org/repo.git", "ssh://git@host:2222/x", "github.com:org/repo", "git://host/x"} {
		_, err := New(u, t.TempDir(), nil)
		assert.NoError(t, err, "url %q", u)
	}
	for _, u := range []string{"file:///srv/repo.git", "FILE:///srv/repo.git", "/srv/repo.git"} {
		_, err := New(u, t.TempDir(), nil)
		assert.ErrorIs(t, err, ErrInvalidURL, "local repositories are refused by default: %q", u)
		_, err = New(u, t.TempDir(), nil, AllowLocalRepositories(true))
		assert.NoError(t, err, "url %q", u)
	}
	_, err := New("https://example.com/x", "", nil, GitPath("bin/git"))
	assert.Error(t, err)
}

func TestWorkingCopyOperationsRequireRoot(t *testing.T) {
	t.Parallel()
	c, err := New("https://example.com/x", "", nil)
	require.NoError(t, err)
	ctx := testCtx(t)
	assert.ErrorIs(t, c.Init(ctx), ErrNotInitialized)
	_, err = c.CommitSHA(ctx)
	assert.ErrorIs(t, err, ErrNotInitialized)
	_, err = c.expandShortSHA(ctx, "abcdef1")
	assert.ErrorIs(t, err, ErrNotInitialized)
}

func TestCommandsNeverDiscoverParentRepository(t *testing.T) {
	t.Parallel()
	// The working copy lives inside another repository; an uninitialized root
	// must not make git operate on the enclosing one.
	outer := newRemoteFixture(t)
	outer.commit("outer", map[string]string{"f": "x\n"})
	root := filepath.Join(outer.work, "nested", "wc")
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".git"), 0o755)) // no HEAD: not a repo
	c, err := New("file:///nonexistent.git", root, nil, AllowLocalRepositories(true))
	require.NoError(t, err)
	_, err = c.git(testCtx(t), cmdSpec{args: []string{"rev-parse", "--show-toplevel"}})
	require.Error(t, err, "git must not find the enclosing repository")
	assert.False(t, errors.Is(err, ErrAuthentication))
}

// TestFetchRejectsRefspecRevisions is a regression test: a revision shaped like a
// refspec ("+src:dst", ":dst", globs, negative refspecs) reached `git fetch` and
// could force-update any local ref of the shared working copy.
func TestFetchRejectsRefspecRevisions(t *testing.T) {
	t.Parallel()
	f := newRevisionFixture(t)
	ctx := testCtx(t)
	rec := &cmdRecorder{}
	c := newTestClient(t, f.url(), nil, withRunHook(rec.hook))
	require.NoError(t, c.CheckoutRevision(ctx, "main", false))
	refs := func() string {
		out, err := c.git(ctx, cmdSpec{args: []string{"for-each-ref", "--format=%(refname) %(objectname)"}})
		require.NoError(t, err)
		return out
	}
	before := refs()
	fetches := rec.count("fetch")

	for _, rev := range []string{
		"+refs/heads/feature:refs/remotes/origin/main",
		"refs/heads/feature:refs/heads/x",
		"feature:refs/tags/v1.0.0",
		":refs/remotes/origin/main",
		":",
		"refs/heads/*:refs/remotes/x/*",
		"refs/heads/*",
		"refs/heads/fe?ture",
		"refs/heads/[f]eature",
		"^refs/heads/main",
		"+feature",
		"main~1",
		"main^",
		"main@{1}",
		"a..b",
	} {
		assert.ErrorIs(t, c.Fetch(ctx, rev), ErrInvalidRevision, "Fetch(%q)", rev)
		assert.ErrorIs(t, c.CheckoutRevision(ctx, rev, false), ErrInvalidRevision, "CheckoutRevision(%q)", rev)
	}
	assert.Equal(t, fetches, rec.count("fetch"), "no refspec-shaped revision reached git fetch")
	assert.Equal(t, before, refs(), "no local ref was rewritten")

	// Names and object names are still fetched.
	for _, rev := range []string{"feature", "refs/heads/feature", "refs/pull/1/head", "v1.0.0", "HEAD", f.c2} {
		require.NoError(t, c.Fetch(ctx, rev), "Fetch(%q)", rev)
	}
	require.NoError(t, c.CheckoutRevision(ctx, "refs/pull/1/head", false))
	sha, err := c.CommitSHA(ctx)
	require.NoError(t, err)
	assert.Equal(t, f.c3, sha)
}

// newDistinctRepos creates two different bare repositories whose URLs differ
// only by a ".git" suffix ("<dir>/remote.git" and "<dir>/remote"): upstream-style
// URL comparison treats them as the same repository.
func newDistinctRepos(t *testing.T) (a, b *remoteFixture, shaA, shaB string) {
	t.Helper()
	a = newRemoteFixture(t)
	shaA = a.commit("repo A", map[string]string{"who": "A\n"})
	a.push("main")
	b = &remoteFixture{t: t, dir: a.dir, bare: filepath.Join(a.dir, "remote"), work: filepath.Join(a.dir, "work-b"), env: a.env}
	runGit(t, a.dir, a.env, "init", "--quiet", "--bare", "--initial-branch=main", b.bare)
	runGit(t, a.dir, a.env, "init", "--quiet", "--initial-branch=main", b.work)
	b.git("remote", "add", "origin", b.bare)
	shaB = b.commit("repo B", map[string]string{"who": "B\n"})
	b.push("main")
	return a, b, shaA, shaB
}

// TestDistinctRepositoriesNeverShareState is a regression test: RepoKey folded
// path case and ".git" suffixes, and Init kept any existing repository without
// checking its origin, so a client could fetch from (and serve the content of)
// another repository through a shared working copy or ref cache entry.
func TestDistinctRepositoriesNeverShareState(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	a, b, shaA, shaB := newDistinctRepos(t)
	urlA, urlB := a.url(), b.url()
	require.True(t, SameURL(urlA, urlB), "upstream comparison treats them as one repository")
	assert.NotEqual(t, RepoKey(urlA), RepoKey(urlB), "distinct locations must not share a working copy key")

	t.Run("shared working copy", func(t *testing.T) {
		t.Parallel()
		root := filepath.Join(t.TempDir(), "wc")
		ca, err := New(urlA, root, nil, AllowLocalRepositories(true))
		require.NoError(t, err)
		cb, err := New(urlB, root, nil, AllowLocalRepositories(true))
		require.NoError(t, err)
		origin := func(c *Client) string {
			out, err := c.git(ctx, cmdSpec{args: []string{"config", "--get-all", "remote.origin.url"}})
			require.NoError(t, err)
			return out
		}

		require.NoError(t, ca.CheckoutRevision(ctx, "main", false))
		assert.Equal(t, "A\n", readFile(t, filepath.Join(root, "who")))
		require.NoError(t, cb.CheckoutRevision(ctx, "main", false))
		assert.Equal(t, "B\n", readFile(t, filepath.Join(root, "who")), "client B must see repository B")
		assert.Equal(t, urlB, origin(cb))
		assert.False(t, cb.IsRevisionPresent(ctx, shaA), "repository A's objects were discarded")
		require.NoError(t, ca.CheckoutRevision(ctx, "main", false))
		assert.Equal(t, "A\n", readFile(t, filepath.Join(root, "who")))
		assert.Equal(t, urlA, origin(ca))

		// Another spelling of the same location keeps the working copy and only
		// updates the configured URL.
		spelled, err := New(urlA+"/", root, nil, AllowLocalRepositories(true))
		require.NoError(t, err)
		require.Equal(t, RepoKey(urlA), RepoKey(urlA+"/"))
		require.NoError(t, spelled.Init(ctx))
		assert.True(t, spelled.IsRevisionPresent(ctx, shaA), "working copy kept")
		assert.Equal(t, urlA+"/", origin(spelled))
	})

	t.Run("working copy without origin is re-initialized", func(t *testing.T) {
		t.Parallel()
		root := filepath.Join(t.TempDir(), "wc")
		ca, err := New(urlA, root, nil, AllowLocalRepositories(true))
		require.NoError(t, err)
		require.NoError(t, ca.CheckoutRevision(ctx, "main", false))
		_, err = ca.git(ctx, cmdSpec{args: []string{"config", "--unset-all", "remote.origin.url"}})
		require.NoError(t, err)
		require.NoError(t, ca.Init(ctx))
		assert.False(t, ca.IsRevisionPresent(ctx, shaA))
		require.NoError(t, ca.CheckoutRevision(ctx, "main", false))
		assert.Equal(t, "A\n", readFile(t, filepath.Join(root, "who")))
	})

	t.Run("ref cache", func(t *testing.T) {
		t.Parallel()
		cache := NewRefCache(time.Minute, 10)
		ca := newTestClient(t, urlA, nil, WithRefCache(cache, true))
		cb := newTestClient(t, urlB, nil, WithRefCache(cache, true))
		got, err := ca.ResolveRevision(ctx, "main")
		require.NoError(t, err)
		assert.Equal(t, shaA, got)
		got, err = cb.ResolveRevision(ctx, "main")
		require.NoError(t, err)
		assert.Equal(t, shaB, got, "repository B must not be served repository A's listing")

		// Invalidation still covers every spelling upstream treats as the same URL.
		cache.Invalidate(strings.ToUpper(urlA[:7]) + urlA[7:])
		assert.Equal(t, 0, cache.Len())
	})
}
