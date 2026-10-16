package sandbox

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// fsEntry describes one fixture entry: a directory (trailing "/"), a regular file,
// or a symlink when link is non-empty.
type fsEntry struct {
	path string
	link string
}

func buildTree(t testing.TB, root string, entries []fsEntry) {
	t.Helper()
	for _, e := range entries {
		p := filepath.Join(root, filepath.FromSlash(e.path))
		switch {
		case e.link != "":
			require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
			require.NoError(t, os.Symlink(e.link, p))
		case strings.HasSuffix(e.path, "/"):
			require.NoError(t, os.MkdirAll(p, 0o755))
		default:
			require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
			require.NoError(t, os.WriteFile(p, []byte(e.path), 0o644))
		}
	}
}

// secureJoinFixture is shared by the table test and the fuzz test.
var secureJoinFixture = []fsEntry{
	{path: "a/b/file"},
	{path: "a/link-in", link: "b"},
	{path: "a/link-file", link: "b/file"},
	{path: "a/deep", link: "../a/b"},
	{path: "a/escape", link: "../../x"},
	{path: "up", link: ".."},
	{path: "abs", link: "/etc"},
	{path: "self", link: "."},
	{path: "chain1", link: "chain2"},
	{path: "chain2", link: "a"},
	{path: "loop1", link: "loop2"},
	{path: "loop2", link: "loop1"},
	{path: "dangling", link: "nonexistent/x"},
	{path: "p/q/b", link: "../.."},
	{path: "p/q/a", link: "b/.."},
}

func TestSecureJoin(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	buildTree(t, root, secureJoinFixture)
	j := func(p string) string { return filepath.Join(root, filepath.FromSlash(p)) }

	tests := []struct {
		rel     string
		want    string
		wantErr error
	}{
		{rel: "", want: root},
		{rel: ".", want: root},
		{rel: "a/b/file", want: j("a/b/file")},
		{rel: "a//b/./file", want: j("a/b/file")},
		{rel: "a/link-in/file", want: j("a/b/file")},
		{rel: "a/link-file", want: j("a/b/file")},
		{rel: "a/deep/file", want: j("a/b/file")},
		{rel: "a/../a/b", want: j("a/b")},
		{rel: "chain1/b/file", want: j("a/b/file")},
		{rel: "self/a", want: j("a")},
		{rel: "dangling", want: j("nonexistent/x")},
		{rel: "new/dir/file", want: j("new/dir/file")},
		{rel: "a/b/file/x", want: j("a/b/file/x")},
		{rel: "p/q/b", want: root},
		{rel: "../x", wantErr: ErrPathEscape},
		{rel: "a/../../x", wantErr: ErrPathEscape},
		{rel: "..", wantErr: ErrPathEscape},
		{rel: "up/x", wantErr: ErrPathEscape},
		{rel: "a/escape", wantErr: ErrPathEscape},
		{rel: "abs", wantErr: ErrPathEscape},
		{rel: "abs/passwd", wantErr: ErrPathEscape},
		{rel: "self/..", wantErr: ErrPathEscape},
		{rel: "p/q/a", wantErr: ErrPathEscape},
		{rel: "/etc/passwd", wantErr: ErrAbsolutePath},
		{rel: "loop1", wantErr: ErrTooManySymlinks},
		{rel: "missing/\x00", wantErr: fs.ErrInvalid},
		{rel: "a/\x00/b", wantErr: fs.ErrInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.rel, func(t *testing.T) {
			t.Parallel()
			got, err := SecureJoin(root, tt.rel)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				assert.Empty(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestSecureJoinAbsoluteSymlinkReportsLink(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	buildTree(t, root, []fsEntry{{path: "d/abs", link: "/etc"}})
	_, err := SecureJoin(root, "d/abs/hosts")
	var oob *OutOfBoundsSymlinkError
	require.ErrorAs(t, err, &oob)
	assert.Equal(t, filepath.Join("d", "abs"), oob.Path)
	assert.Equal(t, "/etc", oob.Target)
}

func TestSecureJoinRelativeRoot(t *testing.T) {
	root := t.TempDir()
	buildTree(t, root, []fsEntry{{path: "x/y"}})
	t.Chdir(root)
	got, err := SecureJoin(".", "x/y")
	require.NoError(t, err)
	wd, err := os.Getwd()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(wd, "x", "y"), got)
}

func TestCheckSymlinks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		entries  []fsEntry
		skip     []string
		wantPath string // "" means no error
	}{
		{
			name: "upstream goodlink",
			entries: []fsEntry{
				{path: "file.txt"},
				{path: "goodlink", link: "./file.txt"},
				{path: "goodlink2", link: "subdir/file.txt"},
				{path: "goodlink3", link: "subdir/..file.txt"},
				{path: "subdir/..file.txt"},
				{path: "subdir/file.txt"},
				{path: "subdir/goodlink", link: "../subdir/file.txt"},
			},
		},
		{name: "upstream badlink", entries: []fsEntry{{path: "badlink", link: "../../bla"}}, wantPath: "badlink"},
		{name: "upstream badlink2", entries: []fsEntry{{path: "badlink", link: "..///../bad//../link/"}}, wantPath: "badlink"},
		{
			name:     "upstream badlink3 leaves and re-enters",
			entries:  []fsEntry{{path: "test.txt"}, {path: "badlink", link: "../ROOTNAME/test.txt"}},
			wantPath: "badlink",
		},
		{name: "upstream abslink", entries: []fsEntry{{path: "abslink", link: "/somethingbad"}}, wantPath: "abslink"},
		{name: "upstream excluded", entries: []fsEntry{{path: "badlink", link: "../../bla"}}, skip: []string{"badlink"}},
		{
			name:     "absolute target pointing inside root is still rejected",
			entries:  []fsEntry{{path: "f"}, {path: "abs", link: "ROOTABS/f"}},
			wantPath: "abs",
		},
		{
			name:     "chain: lexically inside, physically outside",
			entries:  []fsEntry{{path: "d", link: "."}, {path: "x", link: "d/.."}},
			wantPath: "x",
		},
		{
			name:     "nested chain through parent links",
			entries:  []fsEntry{{path: "p/q/b", link: "../.."}, {path: "p/q/a", link: "b/.."}},
			wantPath: filepath.Join("p", "q", "a"),
		},
		{
			name:     "chain to absolute",
			entries:  []fsEntry{{path: "l1", link: "l2"}, {path: "l2", link: "/etc"}},
			wantPath: "l1",
		},
		{name: "loop is not an escape", entries: []fsEntry{{path: "a", link: "b"}, {path: "b", link: "a"}}},
		{name: "dangling inside root", entries: []fsEntry{{path: "x", link: "nope/y"}}},
		{name: "link to root itself", entries: []fsEntry{{path: "sub/root", link: ".."}}},
		{
			name:     "deep escape",
			entries:  []fsEntry{{path: "a/b/c/"}, {path: "a/b/c/l", link: "../../../../etc"}},
			wantPath: filepath.Join("a", "b", "c", "l"),
		},
		{
			name:    "skip .git",
			entries: []fsEntry{{path: ".git/hooks/evil", link: "/etc/passwd"}, {path: "ok"}},
			skip:    []string{".git"},
		},
		{
			name:     "not skipped .git",
			entries:  []fsEntry{{path: ".git/hooks/evil", link: "/etc/passwd"}},
			wantPath: filepath.Join(".git", "hooks", "evil"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := filepath.Join(t.TempDir(), "ROOT")
			require.NoError(t, os.Mkdir(root, 0o755))
			entries := make([]fsEntry, len(tt.entries))
			for i, e := range tt.entries {
				e.link = strings.ReplaceAll(e.link, "ROOTABS", root)
				e.link = strings.ReplaceAll(e.link, "ROOTNAME", filepath.Base(root))
				entries[i] = e
			}
			buildTree(t, root, entries)
			err := CheckSymlinks(root, tt.skip...)
			if tt.wantPath == "" {
				require.NoError(t, err)
				return
			}
			var oob *OutOfBoundsSymlinkError
			require.ErrorAs(t, err, &oob)
			require.ErrorIs(t, err, ErrPathEscape)
			assert.Equal(t, tt.wantPath, oob.Path)
			assert.Equal(t, "out of bounds symlink found: "+tt.wantPath, err.Error())
		})
	}
}

func TestCheckSymlinksMissingRoot(t *testing.T) {
	t.Parallel()
	err := CheckSymlinks(filepath.Join(t.TempDir(), "missing"))
	require.ErrorIs(t, err, fs.ErrNotExist)
	var oob *OutOfBoundsSymlinkError
	require.False(t, errors.As(err, &oob))
}

// TestCheckSymlinksSoundness generates random trees with random relative symlinks
// and checks that whenever CheckSymlinks accepts a tree, the kernel-equivalent
// resolver (filepath.EvalSymlinks) never resolves a path made only of names
// (no '..') to a location outside the root.
func TestCheckSymlinksSoundness(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	names := []string{"a", "f", "l0", "l1", "l2"}
	linkNames := []string{"l0", "l1", "l2"}
	// A small vocabulary of links and ".." makes chains (a link through another
	// link, then "..") common.
	targetComps := []string{"..", ".", "a", "l0", "l1", "l2"}
	dirs := []string{"", "a", "a/a"}
	rapid.Check(t, func(rt *rapid.T) {
		root, err := os.MkdirTemp(base, "tree")
		require.NoError(rt, err)
		defer func() { _ = os.RemoveAll(root) }()

		require.NoError(rt, os.MkdirAll(filepath.Join(root, "a", "a"), 0o755))
		require.NoError(rt, os.WriteFile(filepath.Join(root, "a", "f"), nil, 0o644))
		nLinks := rapid.IntRange(1, 6).Draw(rt, "nLinks")
		for i := range nLinks {
			dir := rapid.SampledFrom(dirs).Draw(rt, "dir")
			name := rapid.SampledFrom(linkNames).Draw(rt, "name")
			comps := rapid.SliceOfN(rapid.SampledFrom(targetComps), 1, 4).Draw(rt, "target")
			target := strings.Join(comps, "/")
			p := filepath.Join(root, dir, name)
			if _, err := os.Lstat(p); err == nil {
				continue
			}
			require.NoError(rt, os.Symlink(target, p), "link %d", i)
		}

		if err := CheckSymlinks(root); err != nil {
			var oob *OutOfBoundsSymlinkError
			require.ErrorAs(rt, err, &oob)
			return
		}
		realRoot, err := filepath.EvalSymlinks(root)
		require.NoError(rt, err)
		// Every path of up to three names must stay inside root.
		var check func(prefix string, depth int)
		check = func(prefix string, depth int) {
			for _, n := range names {
				rel := filepath.Join(prefix, n)
				if resolved, err := filepath.EvalSymlinks(filepath.Join(root, rel)); err == nil {
					require.True(rt, within(realRoot, resolved), "%s resolved to %s outside %s", rel, resolved, realRoot)
				}
				if depth < 3 {
					check(rel, depth+1)
				}
			}
		}
		check("", 1)
	})
}

func within(root, p string) bool {
	return p == root || strings.HasPrefix(p, root+string(filepath.Separator))
}

// selfLoopTarget returns a ~1 KiB relative target that walks "n/.." pairs and
// ends in name, so a link named name pointing at it refers to itself after many
// cheap-looking components (the review's cost-amplification shape).
func selfLoopTarget(name string) string {
	return strings.Repeat("n/../", 200) + name
}

func TestCheckSymlinksSymlinkedRootFailsClosed(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	real := filepath.Join(base, "real")
	buildTree(t, real, []fsEntry{{path: "d", link: "."}, {path: "x", link: "d/.."}})
	link := filepath.Join(base, "root")
	require.NoError(t, os.Symlink("real", link))

	// Walking the real directory finds the chain escape ...
	var oob *OutOfBoundsSymlinkError
	require.ErrorAs(t, CheckSymlinks(real), &oob)
	assert.Equal(t, "x", oob.Path)
	// ... and a symlinked root must not hide it (WalkDir does not descend).
	err := CheckSymlinks(link)
	require.ErrorAs(t, err, &oob)
	require.ErrorIs(t, err, ErrPathEscape)
	assert.Equal(t, ".", oob.Path)
	assert.Equal(t, "real", oob.Target)
}

func TestSecureJoinSelfLoopBoundedWork(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	buildTree(t, root, []fsEntry{{path: "n/"}, {path: "l0", link: selfLoopTarget("l0")}})

	r := newResolver(root)
	_, err := r.resolve(nil, "l0")
	require.ErrorIs(t, err, ErrSymlinkBudget)
	// Every repeated component is served from the lookup cache: l0 and n only.
	// (The review estimated ~51k lstat calls before the fix.)
	assert.LessOrEqual(t, r.lstats, 4)

	_, err = SecureJoin(root, "l0/x")
	require.ErrorIs(t, err, ErrSymlinkBudget)
	require.NotErrorIs(t, err, ErrPathEscape, "budget failures are not reported as escapes by SecureJoin")
}

func TestCheckSymlinksSelfLoopsFailClosedCheaply(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	entries := []fsEntry{{path: "n/"}}
	for i := range 20 {
		name := "l" + strconv.Itoa(i)
		entries = append(entries, fsEntry{path: name, link: selfLoopTarget(name)})
	}
	buildTree(t, root, entries)

	r, err := checkSymlinks(t.Context(), root, nil)
	require.ErrorIs(t, err, ErrSymlinkBudget)
	var oob *OutOfBoundsSymlinkError
	require.ErrorAs(t, err, &oob, "budget failures fail closed as out-of-bounds links")
	assert.Equal(t, "l0", oob.Path)
	assert.LessOrEqual(t, r.lstats, 8)
}

func TestResolveStepBudget(t *testing.T) {
	t.Parallel()
	// m0 -> ... -> m10 -> f, each target ~1 KiB (401 components).
	root := t.TempDir()
	entries := []fsEntry{{path: "n/"}, {path: "f"}}
	for i := range 11 {
		next := "m" + strconv.Itoa(i+1)
		if i == 10 {
			next = "f"
		}
		entries = append(entries, fsEntry{path: "m" + strconv.Itoa(i), link: strings.Repeat("n/../", 200) + next})
	}
	buildTree(t, root, entries)

	// Nine hops (~3.6k components) fit the per-resolution budget ...
	got, err := SecureJoin(root, "m2")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(root, "f"), got)
	// ... eleven (~4.4k) do not, and fail closed.
	_, err = SecureJoin(root, "m0")
	require.ErrorIs(t, err, ErrSymlinkBudget)

	// An over-long input path fails the same way before any lstat.
	r := newResolver(root)
	_, err = r.resolve(nil, strings.Repeat("n/", maxResolveSteps+1))
	require.ErrorIs(t, err, ErrSymlinkBudget)
	assert.Zero(t, r.lstats)
}

func TestResolveSimpleLoopsStillAccepted(t *testing.T) {
	t.Parallel()
	// Loops with short targets end at the hop limit, not the budget: they stay
	// "not an escape", as upstream and the kernel treat them.
	root := t.TempDir()
	buildTree(t, root, []fsEntry{{path: "a", link: "b"}, {path: "b", link: "a"}, {path: "s", link: "s"}})
	require.NoError(t, CheckSymlinks(root))
	_, err := SecureJoin(root, "a")
	require.ErrorIs(t, err, ErrTooManySymlinks)
}

func TestCheckSymlinksWalkBudget(t *testing.T) {
	t.Parallel()
	// Every l<i> -> m0 is cheap to store but costs ~3.6k resolution steps (m0..m8
	// chain), under the per-link budget. Many of them exhaust the per-walk budget
	// (walkInitialSteps + walkStepsPerEntry per entry) and fail closed.
	build := func(t *testing.T, links int) string {
		root := t.TempDir()
		entries := []fsEntry{{path: "n/"}, {path: "f"}}
		for i := range 9 {
			next := "m" + strconv.Itoa(i+1)
			if i == 8 {
				next = "f"
			}
			entries = append(entries, fsEntry{path: "m" + strconv.Itoa(i), link: strings.Repeat("n/../", 200) + next})
		}
		for i := range links {
			entries = append(entries, fsEntry{path: "z/l" + strconv.Itoa(i), link: "../m0"})
		}
		buildTree(t, root, entries)
		return root
	}
	t.Run("within budget", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, CheckSymlinks(build(t, 150)))
	})
	t.Run("exhausted", func(t *testing.T) {
		t.Parallel()
		err := CheckSymlinks(build(t, 400))
		require.ErrorIs(t, err, ErrSymlinkBudget)
		var oob *OutOfBoundsSymlinkError
		require.ErrorAs(t, err, &oob)
		assert.True(t, strings.HasPrefix(oob.Path, filepath.Join("z", "l")), oob.Path)
	})
}

func TestCheckSymlinksContextCanceled(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	buildTree(t, root, []fsEntry{{path: "a/b"}})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, CheckSymlinksContext(ctx, root), context.Canceled)
}
