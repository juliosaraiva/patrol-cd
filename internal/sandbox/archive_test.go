package sandbox

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

type tarEntry struct {
	name     string
	typ      byte
	body     string
	linkname string
	mode     int64
	pax      map[string]string
}

func reg(name, body string) tarEntry {
	return tarEntry{name: name, typ: tar.TypeReg, body: body, mode: 0o644}
}
func dir(name string) tarEntry { return tarEntry{name: name, typ: tar.TypeDir, mode: 0o755} }
func symlink(name, target string) tarEntry {
	return tarEntry{name: name, typ: tar.TypeSymlink, linkname: target, mode: 0o777}
}
func hardlink(name, target string) tarEntry {
	return tarEntry{name: name, typ: tar.TypeLink, linkname: target, mode: 0o644}
}

func makeTar(t testing.TB, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:       e.name,
			Typeflag:   e.typ,
			Linkname:   e.linkname,
			Mode:       e.mode,
			Size:       int64(len(e.body)),
			ModTime:    time.Unix(1_700_000_000, 0),
			PAXRecords: e.pax,
		}
		if e.pax != nil {
			hdr.Format = tar.FormatPAX
		}
		if e.typ != tar.TypeReg {
			hdr.Size = 0
		}
		if e.typ == tar.TypeXGlobalHeader {
			hdr = &tar.Header{Name: e.name, Typeflag: e.typ, PAXRecords: e.pax, Format: tar.FormatPAX}
		}
		require.NoError(t, tw.WriteHeader(hdr))
		if e.typ == tar.TypeReg {
			_, err := tw.Write([]byte(e.body))
			require.NoError(t, err)
		}
	}
	require.NoError(t, tw.Close())
	return buf.Bytes()
}

func gzipBytes(t testing.TB, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, err := gz.Write(data)
	require.NoError(t, err)
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

// extractEnv returns a destination directory and a sibling "outside" directory
// whose contents must never change.
func extractEnv(t *testing.T) (dest, outside string) {
	t.Helper()
	base := t.TempDir()
	dest = filepath.Join(base, "dest", "inner")
	outside = filepath.Join(base, "outside")
	require.NoError(t, os.MkdirAll(outside, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0o600))
	return dest, outside
}

func requireOutsideUntouched(t *testing.T, dest, outside string) {
	t.Helper()
	entries, err := os.ReadDir(outside)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	data, err := os.ReadFile(filepath.Join(outside, "secret"))
	require.NoError(t, err)
	require.Equal(t, "secret", string(data))
	// Nothing may appear next to dest either.
	parent := filepath.Dir(dest)
	siblings, err := os.ReadDir(parent)
	require.NoError(t, err)
	for _, s := range siblings {
		require.Equal(t, "inner", s.Name())
	}
	grand, err := os.ReadDir(filepath.Dir(parent))
	require.NoError(t, err)
	for _, s := range grand {
		require.Contains(t, []string{"dest", "outside"}, s.Name())
	}
}

func TestExtractTarHappyPath(t *testing.T) {
	t.Parallel()
	dest, outside := extractEnv(t)
	data := makeTar(t,
		tarEntry{name: "pax_global_header", typ: tar.TypeXGlobalHeader, pax: map[string]string{"comment": "abc"}},
		dir("./"),
		dir("chart/"),
		reg("chart/Chart.yaml", "name: demo\n"),
		reg("chart/templates/deploy.yaml", "kind: Deployment\n"), // parent created implicitly
		tarEntry{name: "chart/run.sh", typ: tar.TypeReg, body: "#!/bin/sh\n", mode: 0o4777},
		tarEntry{name: "chart/private", typ: tar.TypeReg, body: "p", mode: 0o600},
		symlink("chart/values.yaml", "templates/../Chart.yaml"),
		symlink("chart/link-dir", "templates"),
		hardlink("chart/Chart-copy.yaml", "chart/Chart.yaml"),
		reg("chart/dup", "first"),
		reg("chart/dup", "second"),
		dir("chart/"), // repeated directory is fine
	)
	require.NoError(t, ExtractTar(bytes.NewReader(data), dest, ArchiveLimits{}))
	requireOutsideUntouched(t, dest, outside)

	read := func(p string) string {
		b, err := os.ReadFile(filepath.Join(dest, p))
		require.NoError(t, err)
		return string(b)
	}
	assert.Equal(t, "name: demo\n", read("chart/Chart.yaml"))
	assert.Equal(t, "kind: Deployment\n", read("chart/templates/deploy.yaml"))
	assert.Equal(t, "name: demo\n", read("chart/values.yaml"))
	assert.Equal(t, "name: demo\n", read("chart/Chart-copy.yaml"))
	assert.Equal(t, "second", read("chart/dup"))
	assert.Equal(t, "kind: Deployment\n", read("chart/link-dir/deploy.yaml"))

	mode := func(p string) fs.FileMode {
		fi, err := os.Lstat(filepath.Join(dest, p))
		require.NoError(t, err)
		return fi.Mode()
	}
	assert.Equal(t, fs.FileMode(0o755), mode("chart/run.sh"), "setuid dropped, exec kept")
	assert.Equal(t, fs.FileMode(0o644), mode("chart/private"))
	assert.Equal(t, fs.FileMode(0o644), mode("chart/Chart.yaml"))
	assert.Equal(t, fs.ModeDir|0o755, mode("chart/templates"))
	assert.Equal(t, fs.ModeSymlink, mode("chart/values.yaml")&fs.ModeType)
	_, err := os.Lstat(filepath.Join(dest, "pax_global_header"))
	assert.ErrorIs(t, err, fs.ErrNotExist)
}

func TestExtractTarRejects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		entries []tarEntry
		wantErr error
		wantOOB string // expected OutOfBoundsSymlinkError.Path, if any
	}{
		{name: "tar-slip ../", entries: []tarEntry{reg("../evil", "x")}, wantErr: ErrUnsafeArchiveEntry},
		{name: "tar-slip deep", entries: []tarEntry{reg("../../outside/secret", "x")}, wantErr: ErrUnsafeArchiveEntry},
		{name: "dotdot in the middle", entries: []tarEntry{reg("a/../../evil", "x")}, wantErr: ErrUnsafeArchiveEntry},
		{name: "dotdot that stays inside", entries: []tarEntry{reg("a/../b", "x")}, wantErr: ErrUnsafeArchiveEntry},
		{name: "dot-slash dotdot", entries: []tarEntry{reg("./../evil", "x")}, wantErr: ErrUnsafeArchiveEntry},
		{name: "zip-slip backslashes", entries: []tarEntry{reg(`..\..\evil`, "x")}, wantErr: ErrUnsafeArchiveEntry},
		{name: "zip-slip mixed separators", entries: []tarEntry{reg(`a\../../evil`, "x")}, wantErr: ErrUnsafeArchiveEntry},
		{name: "absolute file", entries: []tarEntry{reg("/tmp/evil", "x")}, wantErr: ErrAbsolutePath},
		{name: "absolute dir", entries: []tarEntry{dir("/evil/")}, wantErr: ErrAbsolutePath},
		{name: "symlink escaping", entries: []tarEntry{symlink("evil", "../outside")}, wantOOB: "evil"},
		{name: "symlink to parent", entries: []tarEntry{symlink("up", "..")}, wantOOB: "up"},
		{name: "symlink absolute", entries: []tarEntry{symlink("passwd", "/etc/passwd")}, wantOOB: "passwd"},
		{
			name:    "symlink leaves and re-enters",
			entries: []tarEntry{dir("a/"), symlink("a/l", "../../inner/a")},
			wantOOB: filepath.Join("a", "l"),
		},
		{
			name:    "symlink chain resolved after extraction",
			entries: []tarEntry{symlink("d", "."), symlink("x", "d/..")},
			wantOOB: "x",
		},
		{
			name:    "write through symlinked parent",
			entries: []tarEntry{dir("sub/"), symlink("link", "sub"), reg("link/evil", "x")},
			wantErr: ErrUnsafeArchiveEntry,
		},
		{
			name:    "write through in-root symlink to dir chain",
			entries: []tarEntry{dir("a/"), symlink("a/up", "."), reg("a/up/file", "x")},
			wantErr: ErrUnsafeArchiveEntry,
		},
		{
			name:    "file replacing symlink",
			entries: []tarEntry{reg("target", "t"), symlink("f", "target"), reg("f", "overwrite")},
			wantErr: ErrUnsafeArchiveEntry,
		},
		{name: "symlink replacing file", entries: []tarEntry{reg("f", "x"), symlink("f", "g")}, wantErr: ErrUnsafeArchiveEntry},
		{name: "file replacing dir", entries: []tarEntry{dir("d/"), reg("d", "x")}, wantErr: ErrUnsafeArchiveEntry},
		{name: "dir replacing file", entries: []tarEntry{reg("d", "x"), dir("d/")}, wantErr: ErrUnsafeArchiveEntry},
		{name: "file under a file", entries: []tarEntry{reg("f", "x"), reg("f/g", "y")}, wantErr: ErrUnsafeArchiveEntry},
		{name: "empty symlink target", entries: []tarEntry{symlink("e", "")}, wantErr: ErrUnsafeArchiveEntry},
		{name: "hardlink escaping", entries: []tarEntry{hardlink("h", "../outside/secret")}, wantErr: ErrUnsafeArchiveEntry},
		{name: "hardlink absolute", entries: []tarEntry{hardlink("h", "/etc/passwd")}, wantErr: ErrAbsolutePath},
		{name: "hardlink to missing", entries: []tarEntry{hardlink("h", "nope")}, wantErr: ErrUnsafeArchiveEntry},
		{name: "hardlink to dir", entries: []tarEntry{dir("d/"), hardlink("h", "d")}, wantErr: ErrUnsafeArchiveEntry},
		{name: "hardlink to root", entries: []tarEntry{hardlink("h", "./")}, wantErr: ErrUnsafeArchiveEntry},
		{
			name:    "hardlink to symlink",
			entries: []tarEntry{reg("f", "x"), symlink("s", "f"), hardlink("h", "s")},
			wantErr: ErrUnsafeArchiveEntry,
		},
		{
			name:    "hardlink through symlinked parent",
			entries: []tarEntry{dir("d/"), reg("d/f", "x"), symlink("l", "d"), hardlink("h", "l/f")},
			wantErr: ErrUnsafeArchiveEntry,
		},
		{name: "char device", entries: []tarEntry{{name: "tty", typ: tar.TypeChar}}, wantErr: ErrUnsafeArchiveEntry},
		{name: "block device", entries: []tarEntry{{name: "sda", typ: tar.TypeBlock}}, wantErr: ErrUnsafeArchiveEntry},
		{name: "fifo", entries: []tarEntry{{name: "pipe", typ: tar.TypeFifo}}, wantErr: ErrUnsafeArchiveEntry},
		{name: "unsupported type", entries: []tarEntry{{name: "c", typ: tar.TypeCont}}, wantErr: ErrUnsafeArchiveEntry},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dest, outside := extractEnv(t)
			err := ExtractTar(bytes.NewReader(makeTar(t, tt.entries...)), dest, ArchiveLimits{})
			require.Error(t, err)
			if tt.wantOOB != "" {
				var oob *OutOfBoundsSymlinkError
				require.ErrorAs(t, err, &oob)
				assert.Equal(t, tt.wantOOB, oob.Path)
				require.ErrorIs(t, err, ErrPathEscape)
			} else {
				require.ErrorIs(t, err, tt.wantErr)
			}
			requireOutsideUntouched(t, dest, outside)
		})
	}
}

func TestExtractTarStripExecBits(t *testing.T) {
	t.Parallel()
	dest, _ := extractEnv(t)
	data := makeTar(t, tarEntry{name: "run.sh", typ: tar.TypeReg, body: "x", mode: 0o755}, dir("d/"))
	require.NoError(t, ExtractTar(bytes.NewReader(data), dest, ArchiveLimits{StripExecBits: true}))
	fi, err := os.Stat(filepath.Join(dest, "run.sh"))
	require.NoError(t, err)
	assert.Equal(t, fs.FileMode(0o644), fi.Mode())
	fi, err = os.Stat(filepath.Join(dest, "d"))
	require.NoError(t, err)
	assert.Equal(t, fs.ModeDir|0o755, fi.Mode(), "directories stay traversable")
}

func TestExtractTarLimits(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("z", 10)
	tests := []struct {
		name    string
		entries []tarEntry
		lim     ArchiveLimits
		wantErr bool
	}{
		{"max files", []tarEntry{reg("a", "1"), reg("b", "2"), reg("c", "3")}, ArchiveLimits{MaxFiles: 2}, true},
		{"max files counts dirs", []tarEntry{dir("a/"), dir("b/"), reg("c", "3")}, ArchiveLimits{MaxFiles: 2}, true},
		{"max files ok", []tarEntry{reg("a", "1"), reg("b", "2")}, ArchiveLimits{MaxFiles: 2}, false},
		{"max file bytes", []tarEntry{reg("a", big+"!")}, ArchiveLimits{MaxFileBytes: 10}, true},
		{"max file bytes ok", []tarEntry{reg("a", big)}, ArchiveLimits{MaxFileBytes: 10}, false},
		{"max total bytes", []tarEntry{reg("a", big), reg("b", big)}, ArchiveLimits{MaxTotalBytes: 15}, true},
		{"max total bytes ok", []tarEntry{reg("a", big), reg("b", big)}, ArchiveLimits{MaxTotalBytes: 20}, false},
		{"negative disables", []tarEntry{reg("a", big), reg("b", big)}, ArchiveLimits{MaxFiles: -1, MaxTotalBytes: -1, MaxFileBytes: -1}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dest, outside := extractEnv(t)
			err := ExtractTar(bytes.NewReader(makeTar(t, tt.entries...)), dest, tt.lim)
			if tt.wantErr {
				require.ErrorIs(t, err, ErrArchiveLimit)
			} else {
				require.NoError(t, err)
			}
			requireOutsideUntouched(t, dest, outside)
		})
	}
}

func TestExtractTarGzBomb(t *testing.T) {
	t.Parallel()
	// 2 MiB of zeros compresses to a few KiB; the size check must reject it from
	// the header without inflating the payload.
	const size = 2 << 20
	var gzBuf bytes.Buffer
	gzw, err := gzip.NewWriterLevel(&gzBuf, gzip.BestSpeed)
	require.NoError(t, err)
	tw := tar.NewWriter(gzw)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "bomb", Typeflag: tar.TypeReg, Mode: 0o644, Size: size}))
	_, err = io.CopyN(tw, zeroReader{}, size)
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gzw.Close())
	gz := gzBuf.Bytes()

	dest, _ := extractEnv(t)
	err = ExtractTarGz(bytes.NewReader(gz), dest, ArchiveLimits{MaxFileBytes: 64 << 10})
	require.ErrorIs(t, err, ErrArchiveLimit)
	entries, err := os.ReadDir(dest)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestExtractTarRejectsOversizedFileFromHeader(t *testing.T) {
	t.Parallel()
	// A header announcing 1 TiB followed by an endless payload: extraction must stop
	// after reading the header.
	var hdr bytes.Buffer
	tw := tar.NewWriter(&hdr)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "huge", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1 << 40}))
	counter := &countingReader{r: io.MultiReader(bytes.NewReader(hdr.Bytes()), zeroReader{})}
	dest, _ := extractEnv(t)
	err := ExtractTar(counter, dest, ArchiveLimits{})
	require.ErrorIs(t, err, ErrArchiveLimit)
	assert.LessOrEqual(t, counter.n, int64(64<<10))
}

func TestExtractTarOversizedPAXHeaders(t *testing.T) {
	t.Parallel()
	// Every entry carries ~900 KiB of PAX records: legal per entry, but the stream
	// budget (1 MiB + 64 KiB per entry + file sizes) stops a stream of them.
	huge := map[string]string{"comment": strings.Repeat("p", 900<<10)}
	var entries []tarEntry
	for _, n := range []string{"a", "b", "c", "d"} {
		e := reg(n, "x")
		e.pax = huge
		entries = append(entries, e)
	}
	dest, outside := extractEnv(t)
	err := ExtractTar(bytes.NewReader(makeTar(t, entries...)), dest, ArchiveLimits{})
	require.ErrorIs(t, err, ErrArchiveLimit)
	requireOutsideUntouched(t, dest, outside)
}

func TestExtractTarInvalidStreams(t *testing.T) {
	t.Parallel()
	valid := makeTar(t, reg("a", strings.Repeat("a", 2000)))
	tests := []struct {
		name string
		fn   func(dest string) error
	}{
		{"not gzip", func(dest string) error {
			return ExtractTarGz(bytes.NewReader([]byte("not gzip at all")), dest, ArchiveLimits{})
		}},
		{"truncated tar", func(dest string) error {
			return ExtractTar(bytes.NewReader(valid[:700]), dest, ArchiveLimits{})
		}},
		{"truncated gzip", func(dest string) error {
			gz := gzipBytes(t, valid)
			return ExtractTarGz(bytes.NewReader(gz[:len(gz)/2]), dest, ArchiveLimits{})
		}},
		{"garbage header", func(dest string) error {
			return ExtractTar(bytes.NewReader(bytes.Repeat([]byte{0xff}, 1024)), dest, ArchiveLimits{})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dest, _ := extractEnv(t)
			require.ErrorIs(t, tt.fn(dest), ErrInvalidArchive)
		})
	}
}

func TestExtractTarEmptyArchive(t *testing.T) {
	t.Parallel()
	dest, _ := extractEnv(t)
	require.NoError(t, ExtractTar(bytes.NewReader(makeTar(t)), dest, ArchiveLimits{}))
	entries, err := os.ReadDir(dest)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestTarRoundTripAndExcludes(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	buildTree(t, src, []fsEntry{
		{path: ".git/config"},
		{path: ".git/objects/ab/cd"},
		{path: "app/kustomization.yaml"},
		{path: "app/base/deploy.yaml"},
		{path: "app/values.yaml", link: "base/deploy.yaml"},
		{path: "app/basedir", link: "base"},
		{path: "empty/"},
		{path: "artifact.zip"},
		{path: "app/nested.zip"},
	})
	require.NoError(t, os.Chmod(filepath.Join(src, "app", "kustomization.yaml"), 0o750))

	var buf bytes.Buffer
	n, err := TarCount(src, &buf, []string{".git", "*.zip"})
	require.NoError(t, err)
	assert.Equal(t, 3, n, "regular files only; symlinks and dirs are not counted")

	dest := filepath.Join(t.TempDir(), "out")
	require.NoError(t, ExtractTarGz(bytes.NewReader(buf.Bytes()), dest, ArchiveLimits{}))

	got := snapshotTree(t, dest)
	want := map[string]string{
		"app/":                   "dir 0755",
		"app/base/":              "dir 0755",
		"app/base/deploy.yaml":   "file 0644 app/base/deploy.yaml",
		"app/basedir":            "link base",
		"app/kustomization.yaml": "file 0755 app/kustomization.yaml",
		"app/nested.zip":         "file 0644 app/nested.zip", // "*.zip" matches only top level
		"app/values.yaml":        "link base/deploy.yaml",
		"empty/":                 "dir 0755",
	}
	assert.Equal(t, want, got)
}

func TestTarRejectsOutOfBoundsSymlink(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		entries []fsEntry
		want    string
	}{
		{"relative escape", []fsEntry{{path: "a/evil", link: "../../etc"}}, filepath.Join("a", "evil")},
		{"absolute", []fsEntry{{path: "evil", link: "/etc/passwd"}}, "evil"},
		{"chain", []fsEntry{{path: "d", link: "."}, {path: "x", link: "d/.."}}, "x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			src := t.TempDir()
			buildTree(t, src, tt.entries)
			err := Tar(src, io.Discard, nil)
			var oob *OutOfBoundsSymlinkError
			require.ErrorAs(t, err, &oob)
			assert.Equal(t, tt.want, oob.Path)
		})
	}
	t.Run("excluded escape is not sent", func(t *testing.T) {
		t.Parallel()
		src := t.TempDir()
		buildTree(t, src, []fsEntry{{path: ".git/evil", link: "/etc"}, {path: "ok"}})
		require.NoError(t, Tar(src, io.Discard, []string{".git"}))
	})
}

func TestTarDeterministic(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	buildTree(t, src, []fsEntry{{path: "b/z"}, {path: "a/y"}, {path: "c", link: "a/y"}, {path: "d/"}})
	var first, second bytes.Buffer
	require.NoError(t, Tar(src, &first, nil))
	// Different timestamps must not change the stream.
	past := time.Unix(1_000_000_000, 0)
	require.NoError(t, os.Chtimes(filepath.Join(src, "a", "y"), past, past))
	require.NoError(t, os.Chtimes(filepath.Join(src, "b"), past, past))
	require.NoError(t, Tar(src, &second, nil))
	assert.Equal(t, first.Bytes(), second.Bytes())

	// Entries come out in lexical order.
	gz, err := gzip.NewReader(bytes.NewReader(first.Bytes()))
	require.NoError(t, err)
	tr := tar.NewReader(gz)
	var names []string
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		names = append(names, hdr.Name)
		assert.True(t, hdr.ModTime.Equal(time.Unix(0, 0)))
		assert.Zero(t, hdr.Uid)
		assert.Zero(t, hdr.Gid)
	}
	assert.Equal(t, []string{"a/", "a/y", "b/", "b/z", "c", "d/"}, names)
}

func TestTarErrors(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	n, err := TarCount(src, io.Discard, nil)
	require.NoError(t, err)
	assert.Zero(t, n, "empty tree: callers report upstream's 'no files to send'")
	err = Tar(src, io.Discard, []string{"["})
	require.ErrorIs(t, err, filepath.ErrBadPattern)
	assert.Contains(t, err.Error(), `error verifying exclusion pattern "["`)
	file := filepath.Join(src, "f")
	require.NoError(t, os.WriteFile(file, nil, 0o644))
	require.Error(t, Tar(file, io.Discard, nil))
	require.ErrorIs(t, Tar(filepath.Join(src, "missing"), io.Discard, nil), fs.ErrNotExist)
}

// TestTarExtractRoundTripProperty: for random trees, Tar followed by ExtractTarGz
// reproduces the tree (with normalized modes), and Tar is deterministic.
func TestTarExtractRoundTripProperty(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	rapid.Check(t, func(rt *rapid.T) {
		src, err := os.MkdirTemp(base, "src")
		require.NoError(rt, err)
		defer func() { _ = os.RemoveAll(src) }()

		names := rapid.SliceOfNDistinct(rapid.StringMatching(`[a-z]{1,3}(/[a-z]{1,3}){0,2}`), 1, 8, rapid.ID[string]).Draw(rt, "paths")
		var created []string
		for _, n := range names {
			p := filepath.Join(src, filepath.FromSlash(n))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				continue // a prefix is already a file
			}
			if _, err := os.Lstat(p); err == nil {
				continue
			}
			switch rapid.IntRange(0, 3).Draw(rt, "kind") {
			case 0:
				require.NoError(rt, os.Mkdir(p, 0o755))
			case 1:
				body := rapid.SliceOfN(rapid.Byte(), 0, 512).Draw(rt, "body")
				perm := rapid.SampledFrom([]fs.FileMode{0o600, 0o644, 0o700, 0o755}).Draw(rt, "perm")
				require.NoError(rt, os.WriteFile(p, body, perm))
				require.NoError(rt, os.Chmod(p, perm))
			case 2:
				if len(created) > 0 {
					target := rapid.SampledFrom(created).Draw(rt, "target")
					rel, err := filepath.Rel(filepath.Dir(p), filepath.Join(src, target))
					require.NoError(rt, err)
					require.NoError(rt, os.Symlink(rel, p))
				}
			default:
				require.NoError(rt, os.WriteFile(p, []byte(n), 0o644))
			}
			created = append(created, filepath.FromSlash(n))
		}

		var a, b bytes.Buffer
		require.NoError(rt, Tar(src, &a, nil))
		require.NoError(rt, Tar(src, &b, nil))
		require.Equal(rt, a.Bytes(), b.Bytes(), "Tar must be deterministic")

		dest, err := os.MkdirTemp(base, "dest")
		require.NoError(rt, err)
		defer func() { _ = os.RemoveAll(dest) }()
		require.NoError(rt, ExtractTarGz(bytes.NewReader(a.Bytes()), dest, ArchiveLimits{}))
		require.Equal(rt, snapshotTree(rt, src), snapshotTree(rt, dest))
	})
}

// snapshotTree describes a tree with normalized modes: "dir 0755", "file <mode>
// <content>", "link <target>".
func snapshotTree(t require.TestingT, root string) map[string]string {
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			out[rel] = "link " + filepath.ToSlash(target)
		case d.IsDir():
			out[rel+"/"] = "dir 0755"
		default:
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			out[rel] = "file " + modeString(normalizeFileMode(info.Mode())) + " " + string(b)
		}
		return nil
	})
	require.NoError(t, err)
	return out
}

func modeString(m fs.FileMode) string {
	if m == 0o755 {
		return "0755"
	}
	return "0644"
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func TestExtractTarRejectsOverlongNamesBeforeTouchingDisk(t *testing.T) {
	t.Parallel()
	deep := strings.Repeat("a/", maxEntryNameComponents) + "f" // 257 components
	tests := []struct {
		name  string
		entry tarEntry
	}{
		{"name over 4096 bytes", reg(strings.Repeat("abcdefgh/", 460)+"f", "x")},
		{"too many components", reg(deep, "x")},
		{"component over 255 bytes", reg("d/"+strings.Repeat("c", 256), "x")},
		{"deep directory", dir(deep + "/")},
		{"deep symlink", symlink(deep, "x")},
		{"symlink target over 4096 bytes", symlink("l", strings.Repeat("n/", 2049))},
		{"hard link to deep name", hardlink("h", deep)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dest := filepath.Join(t.TempDir(), "dest")
			err := ExtractTar(bytes.NewReader(makeTar(t, tt.entry)), dest, ArchiveLimits{})
			require.ErrorIs(t, err, ErrUnsafeArchiveEntry)
			entries, rerr := os.ReadDir(dest)
			require.NoError(t, rerr)
			assert.Empty(t, entries, "rejected before any filesystem work")
		})
	}
}

// TestExtractTarDeepEntriesLinearWork checks that per-entry filesystem work is
// linear in the entry's depth. The review measured the old per-prefix Lstat walk
// (each Root.Lstat re-walking from the root) at O(depth²) per entry: 10 files at
// depth 250 took 26 s on a loaded host.
func TestExtractTarDeepEntriesLinearWork(t *testing.T) {
	t.Parallel()
	const depth = 250
	const files = 40
	prefix := strings.Repeat("a/", depth-2)
	var entries []tarEntry
	for i := range files {
		// A distinct parent per pair of entries: half the lookups miss the
		// parent cache and walk all existing components.
		entries = append(entries, reg(prefix+"b"+strconv.Itoa(i/2)+"/f"+strconv.Itoa(i), "x"))
	}
	entries = append(entries, hardlink(prefix+"b0/h", prefix+"b0/f0"), symlink(prefix+"b1/s", "f2"))
	dest := filepath.Join(t.TempDir(), "dest")
	require.NoError(t, os.MkdirAll(dest, 0o755))
	root, err := os.OpenRoot(dest)
	require.NoError(t, err)
	defer func() { _ = root.Close() }()
	data := makeTar(t, entries...)
	stream := &budgetReader{r: bytes.NewReader(data), budget: archiveInitialStreamAllowance}
	x := &extractor{ctx: t.Context(), root: root, tr: tar.NewReader(stream), stream: stream, lim: ArchiveLimits{}.normalized()}
	defer x.close()
	start := time.Now()
	require.NoError(t, x.run())
	elapsed := time.Since(start)

	// Linear: a small constant number of component traversals per level and
	// entry. Quadratic work would be about depth/2 = 125 per level and entry.
	limit := 8 * depth * len(entries)
	assert.LessOrEqual(t, x.walked, limit, "walked %d components for %d entries at depth %d", x.walked, len(entries), depth)
	t.Logf("walked %d components (limit %d) in %s", x.walked, limit, elapsed)

	got, err := os.ReadFile(filepath.Join(dest, filepath.FromSlash(prefix), "b1", "s"))
	require.NoError(t, err)
	assert.Equal(t, "x", string(got))
	require.NoError(t, CheckSymlinks(dest))
}

func TestExtractTarImplicitDirectoriesCountAsEntries(t *testing.T) {
	t.Parallel()
	data := makeTar(t, reg("a/b/c/d/e/f/g/h/i/file", "x")) // 9 implicit dirs + 1 file
	err := ExtractTar(bytes.NewReader(data), filepath.Join(t.TempDir(), "d"), ArchiveLimits{MaxFiles: 9})
	require.ErrorIs(t, err, ErrArchiveLimit)
	require.NoError(t, ExtractTar(bytes.NewReader(data), filepath.Join(t.TempDir(), "d"), ArchiveLimits{MaxFiles: 10}))
}

func TestExtractTarIntoSymlinkedDestFailsClosed(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	real := filepath.Join(base, "real")
	require.NoError(t, os.Mkdir(real, 0o755))
	dest := filepath.Join(base, "dest")
	require.NoError(t, os.Symlink("real", dest))

	// Before the fix this archive was accepted: entries went to real/ through the
	// symlink, and the final CheckSymlinks(dest) did not descend into it.
	data := makeTar(t, symlink("d", "."), symlink("x", "d/.."))
	err := ExtractTar(bytes.NewReader(data), dest, ArchiveLimits{})
	require.ErrorIs(t, err, ErrPathEscape)
	var oob *OutOfBoundsSymlinkError
	require.ErrorAs(t, err, &oob)
	assert.Equal(t, ".", oob.Path)
	entries, err := os.ReadDir(real)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestTarSymlinkedRootFailsClosed(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	real := filepath.Join(base, "real")
	buildTree(t, real, []fsEntry{{path: "file.yaml"}})
	link := filepath.Join(base, "link")
	require.NoError(t, os.Symlink("real", link))

	// Before the fix this returned files=0, err=nil and an empty archive.
	var buf bytes.Buffer
	n, err := TarCount(link, &buf, nil)
	require.ErrorIs(t, err, ErrPathEscape)
	assert.Zero(t, n)
	assert.Zero(t, buf.Len(), "nothing is streamed")

	n, err = TarCount(real, &buf, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

func TestArchiveContextCanceled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	dest := filepath.Join(t.TempDir(), "dest")
	err := ExtractTarContext(ctx, bytes.NewReader(makeTar(t, reg("a", "x"))), dest, ArchiveLimits{})
	require.ErrorIs(t, err, context.Canceled)
	entries, rerr := os.ReadDir(dest)
	require.NoError(t, rerr)
	assert.Empty(t, entries)

	src := t.TempDir()
	buildTree(t, src, []fsEntry{{path: "a"}})
	_, err = TarCountContext(ctx, src, io.Discard, nil)
	require.ErrorIs(t, err, context.Canceled)
}
