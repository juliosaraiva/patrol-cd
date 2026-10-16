package app

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeTree creates files (path → content) below dir.
func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for p, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o600))
	}
}

// archiveEntries returns the regular files of an archive with their contents, the
// symlinks with their targets, and checks the recorded checksum.
func archiveEntries(t *testing.T, a *localArchive) (files map[string]string, links map[string]string) {
	t.Helper()
	f, err := a.Open()
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(f)
	require.NoError(t, err)
	sum := sha256.Sum256(raw)
	assert.Equal(t, hex.EncodeToString(sum[:]), a.checksum)
	assert.Equal(t, int64(len(raw)), a.size)
	f2, err := a.Open()
	require.NoError(t, err)
	defer func() { _ = f2.Close() }()
	gz, err := gzip.NewReader(f2)
	require.NoError(t, err)
	tr := tar.NewReader(gz)
	files, links = map[string]string{}, map[string]string{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		assert.Empty(t, h.Uname, "owner names are not recorded")
		assert.Zero(t, h.Uid)
		switch h.Typeflag {
		case tar.TypeReg:
			b, err := io.ReadAll(tr)
			require.NoError(t, err)
			files[h.Name] = string(b)
		case tar.TypeSymlink:
			links[h.Name] = h.Linkname
		}
	}
	return files, links
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestBuildLocalArchiveIncludesAndIgnoreRules(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"deploy.yaml":            "kind: Deployment",
		"svc.yml":                "kind: Service",
		"data.json":              "{}",
		"README.md":              "docs",
		"nested/cm.yaml":         "kind: ConfigMap",
		"nested/secret.env":      "TOKEN=x",
		"generated/out.yaml":     "kind: Out",
		"keep/generated.yaml":    "kind: Keep",
		"tmp/scratch.yaml":       "kind: Scratch",
		".git/config":            "[core]",
		"sub/.gitignore":         "local.yaml\n",
		"sub/local.yaml":         "kind: Local",
		"sub/shared.yaml":        "kind: Shared",
		".gitignore":             "# comment\ngenerated/\n*.tmp.yaml\n/tmp\n",
		".patrolignore":          "README.md\nkeep.yaml\n!keep.yaml\n",
		"report.tmp.yaml":        "kind: Tmp",
		"nested/deep/keep.yaml":  "kind: DeepKeep",
		"nested/deep/other.yaml": "kind: Other",
	})
	a, err := buildLocalArchive(dir, defaultLocalIncludes)
	require.NoError(t, err)
	t.Cleanup(a.Remove)
	files, _ := archiveEntries(t, a)
	assert.Equal(t, []string{
		"data.json", "deploy.yaml", "keep/generated.yaml", "nested/cm.yaml", "nested/deep/keep.yaml", "nested/deep/other.yaml",
		"sub/shared.yaml", "svc.yml",
	}, keys(files))
	assert.Equal(t, "kind: Deployment", files["deploy.yaml"])
	assert.Equal(t, 8, a.files)

	all, err := buildLocalArchive(dir, nil)
	require.NoError(t, err)
	t.Cleanup(all.Remove)
	files, _ = archiveEntries(t, all)
	assert.Contains(t, files, "nested/secret.env", "no include patterns: every file not ignored")
	assert.Contains(t, files, ".gitignore")
	assert.NotContains(t, files, "README.md")
	assert.NotContains(t, files, ".git/config")
}

func TestBuildLocalArchiveSymlinks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"base/cm.yaml": "kind: ConfigMap"})
	require.NoError(t, os.Symlink("base/cm.yaml", filepath.Join(dir, "link.yaml")))
	a, err := buildLocalArchive(dir, defaultLocalIncludes)
	require.NoError(t, err)
	t.Cleanup(a.Remove)
	_, links := archiveEntries(t, a)
	assert.Equal(t, map[string]string{"link.yaml": "base/cm.yaml"}, links)

	outside := t.TempDir()
	writeTree(t, outside, map[string]string{"x.yaml": "kind: X"})
	require.NoError(t, os.Symlink(filepath.Join(outside, "x.yaml"), filepath.Join(dir, "abs.yaml")))
	_, err = buildLocalArchive(dir, defaultLocalIncludes)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "points outside")

	dir2 := t.TempDir()
	writeTree(t, dir2, map[string]string{"a/b.yaml": "kind: B"})
	require.NoError(t, os.Symlink("../../etc/passwd.yaml", filepath.Join(dir2, "a", "up.yaml")))
	_, err = buildLocalArchive(dir2, defaultLocalIncludes)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "points outside")
}

func TestBuildLocalArchiveErrors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"README.md": "x"})
	_, err := buildLocalArchive(dir, defaultLocalIncludes)
	require.EqualError(t, err, "no files to send")

	_, err = buildLocalArchive(filepath.Join(dir, "missing"), nil)
	require.Error(t, err)

	_, err = buildLocalArchive(filepath.Join(dir, "README.md"), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not a directory")

	_, err = buildLocalArchive(dir, []string{"[bad"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "error verifying inclusion pattern")
}

func TestIgnoreRules(t *testing.T) {
	t.Parallel()
	s := &ignoreSet{}
	s.rules = append(s.rules, parseIgnoreRules("", "build/\n/top.yaml\n**/gen/*.yaml\n*.bak\n!important.bak\n\\#literal\n")...)
	s.rules = append(s.rules, parseIgnoreRules("sub", "only-here.yaml\n")...)
	tests := []struct {
		path  string
		isDir bool
		want  bool
	}{
		{"build", true, true},
		{"build", false, false},
		{"top.yaml", false, true},
		{"x/top.yaml", false, false},
		{"a/gen/f.yaml", false, true},
		{"gen/f.yaml", false, true},
		{"x.bak", false, true},
		{"important.bak", false, false},
		{"#literal", false, true},
		{"sub/only-here.yaml", false, true},
		{"only-here.yaml", false, false},
		{"other/sub/only-here.yaml", false, false},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, s.ignored(tc.path, tc.isDir), tc.path)
	}
}
