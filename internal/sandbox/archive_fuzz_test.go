package sandbox

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// FuzzExtractTar feeds arbitrary bytes (seeded with hostile archives) to
// ExtractTar and checks that nothing is ever created outside the destination and
// that an accepted archive never leaves an out-of-bounds symlink behind.
func FuzzExtractTar(f *testing.F) {
	seeds := [][]tarEntry{
		{reg("a", "x"), dir("d/"), symlink("d/l", "../a"), hardlink("h", "a")},
		{reg("../evil", "x")},
		{symlink("up", "..")},
		{symlink("d", "."), symlink("x", "d/..")},
		{dir("s/"), symlink("l", "s"), reg("l/f", "x")},
		{symlink("abs", "/etc"), reg("abs/x", "y")},
		{hardlink("h", "../outside/secret")},
		{{name: "fifo", typ: tar.TypeFifo}},
	}
	for _, s := range seeds {
		f.Add(makeTar(f, s...))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		base := t.TempDir()
		dest := filepath.Join(base, "dest")
		err := ExtractTar(bytes.NewReader(data), dest, ArchiveLimits{MaxFiles: 64, MaxTotalBytes: 1 << 20})
		entries, rerr := os.ReadDir(base)
		if rerr != nil {
			t.Fatal(rerr)
		}
		if len(entries) != 1 || entries[0].Name() != "dest" {
			t.Fatalf("extraction created entries next to dest: %v", entries)
		}
		if err == nil {
			if cerr := CheckSymlinks(dest); cerr != nil {
				t.Fatalf("accepted archive left an unsafe tree: %v", cerr)
			}
		}
	})
}
