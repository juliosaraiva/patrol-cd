package commitserver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

func openTestRoot(t testing.TB, dir string) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = root.Close() })
	return root
}

// replaceFile replaces regular files and symbolic links (never their targets), creates
// missing files, and refuses directories without touching them.
func TestReplaceFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "victim")
	require.NoError(t, os.WriteFile(outside, []byte("victim"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "regular"), []byte("old"), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "link")))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "tree", "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tree", "sub", "keep"), []byte("keep"), 0o644))
	root := openTestRoot(t, dir)

	for _, name := range []string{"regular", "link", "missing"} {
		require.NoError(t, replaceFile(root, name, []byte("new "+name)), name)
		fi, err := os.Lstat(filepath.Join(dir, name))
		require.NoError(t, err)
		assert.True(t, fi.Mode().IsRegular(), "%s is a regular file", name)
		assert.Equal(t, os.FileMode(0o644), fi.Mode().Perm(), name)
		data, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		assert.Equal(t, "new "+name, string(data))
	}
	data, err := os.ReadFile(outside)
	require.NoError(t, err)
	assert.Equal(t, "victim", string(data), "symbolic link target untouched")

	err = replaceFile(root, "tree", []byte("x"))
	require.ErrorContains(t, err, `refusing to replace "tree": it is a directory`)
	data, err = os.ReadFile(filepath.Join(dir, "tree", "sub", "keep"))
	require.NoError(t, err)
	assert.Equal(t, "keep", string(data), "directory content untouched")
}

// Property: for any request prepare accepts, writing it into an empty tree succeeds and
// every path ends up holding its own manifests (no path's output replaces another's). The
// component alphabet includes the owned file names; it has no names differing only by case,
// so the property also holds on case-insensitive file systems.
func TestWriteTreeKeepsEveryPathProperty(t *testing.T) {
	t.Parallel()
	components := []string{"a", "b", ManifestFileName, ReadmeFileName, MetadataFileName}
	base := t.TempDir()
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(1, 4).Draw(rt, "paths")
		req := newRequest("https://git.example.com/org/hydrated.git", "main", drySHA1)
		req.ReadmeTemplate = settings.DefaultReadmeMessageTemplate
		for i := range n {
			parts := rapid.SliceOfN(rapid.SampledFrom(components), 1, 3).Draw(rt, "parts")
			req.Paths = append(req.Paths, pathOf(strings.Join(parts, "/"), configMap("cm", strings.Repeat("v", i+1))))
		}
		p, err := prepare(req)
		if err != nil {
			require.ErrorIs(rt, err, ErrInvalidRequest)
			return
		}
		dir, err := os.MkdirTemp(base, "tree-")
		require.NoError(rt, err)
		root, err := os.OpenRoot(dir)
		require.NoError(rt, err)
		defer func() { _ = root.Close() }()
		changed, err := writeTree(root, discard, p, HydratorCommitMetadata{RepoURL: req.Repo.URL, DrySHA: drySHA1})
		require.NoError(rt, err)
		require.True(rt, changed)
		for _, pp := range p.paths {
			data, err := root.ReadFile(pp.dir + "/" + ManifestFileName)
			require.NoError(rt, err, pp.dir)
			require.Equal(rt, string(pp.manifest), string(data), "manifests of %s", pp.dir)
			var md HydratorCommitMetadata
			raw, err := root.ReadFile(pp.dir + "/" + MetadataFileName)
			require.NoError(rt, err, pp.dir)
			require.NoError(rt, json.Unmarshal(raw, &md), pp.dir)
			require.Equal(rt, drySHA1, md.DrySHA)
		}
	})
}
