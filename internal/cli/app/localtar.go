package app

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// Bounds of the local source archive sent for server-side generation. The server accepts
// at most 32 MiB of gzipped tar (applications.DefaultMaxLocalFilesBytes).
const (
	maxArchiveCompressedBytes   int64 = 32 << 20
	maxArchiveUncompressedBytes int64 = 256 << 20
	maxArchiveFiles                   = 20000
	maxIgnoreFileBytes                = 1 << 20
)

// defaultLocalIncludes are the file name globs sent by default (upstream --local-include).
var defaultLocalIncludes = []string{"*.yaml", "*.yml", "*.json"}

// ignoreFileNames are read in every directory of the local source; their patterns exclude
// files from the archive (gitignore syntax: "#" comments, "!" negation, trailing "/" for
// directories, leading "/" or an inner "/" anchors to the file's directory, "**" spans
// directories).
var ignoreFileNames = []string{".gitignore", ".patrolignore"}

// alwaysExcludedDirs are never archived.
var alwaysExcludedDirs = map[string]bool{".git": true, ".hg": true, ".svn": true}

// localArchive is a gzipped tar of a local source directory in a private temporary file.
type localArchive struct {
	path     string
	checksum string // hex SHA-256 of the gzipped bytes
	size     int64
	files    int
}

// Open opens the archive for reading.
func (a *localArchive) Open() (*os.File, error) { return os.Open(a.path) }

// Remove deletes the archive.
func (a *localArchive) Remove() { _ = os.Remove(a.path) }

// buildLocalArchive archives dir: regular files whose base name matches one of includes
// (all files when includes is empty), directories, and symbolic links that stay inside dir.
// VCS directories and paths ignored by .gitignore/.patrolignore files are skipped. File
// ownership is not recorded.
func buildLocalArchive(dir string, includes []string) (*localArchive, error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}
	for _, p := range includes {
		if _, err := filepath.Match(p, ""); err != nil {
			return nil, fmt.Errorf("error verifying inclusion pattern %q: %w", p, err)
		}
	}
	f, err := os.CreateTemp("", "patrol-local-*.tar.gz")
	if err != nil {
		return nil, fmt.Errorf("create archive: %w", err)
	}
	a := &localArchive{path: f.Name()}
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
			a.Remove()
		}
	}()
	h := sha256.New()
	counter := &countingWriter{w: io.MultiWriter(f, h), limit: maxArchiveCompressedBytes}
	bw := bufio.NewWriter(counter)
	gz := gzip.NewWriter(bw)
	tw := tar.NewWriter(gz)
	w := &archiveWalker{root: root, includes: includes, tw: tw, ignores: newIgnoreSet()}
	if err := filepath.WalkDir(root, w.visit); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("write archive: %w", err)
	}
	if err := gz.Close(); err != nil {
		return nil, fmt.Errorf("write archive: %w", err)
	}
	if err := bw.Flush(); err != nil {
		return nil, fmt.Errorf("write archive: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("write archive: %w", err)
	}
	if w.files == 0 {
		return nil, errors.New("no files to send")
	}
	a.checksum = hex.EncodeToString(h.Sum(nil))
	a.size = counter.n
	a.files = w.files
	ok = true
	return a, nil
}

type countingWriter struct {
	w     io.Writer
	n     int64
	limit int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	if c.n+int64(len(p)) > c.limit {
		return 0, fmt.Errorf("local source archive exceeds %d bytes", c.limit)
	}
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

type archiveWalker struct {
	root     string
	includes []string
	tw       *tar.Writer
	ignores  *ignoreSet
	files    int
	bytes    int64
}

func (w *archiveWalker) visit(p string, d fs.DirEntry, walkErr error) error {
	if walkErr != nil {
		return fmt.Errorf("error walking in %q: %w", w.root, walkErr)
	}
	rel, err := filepath.Rel(w.root, p)
	if err != nil {
		return fmt.Errorf("relative path error: %w", err)
	}
	rel = filepath.ToSlash(rel)
	if rel == "." {
		return w.ignores.load(w.root, "")
	}
	isDir := d.IsDir()
	if isDir && alwaysExcludedDirs[d.Name()] {
		return filepath.SkipDir
	}
	if w.ignores.ignored(rel, isDir) {
		if isDir {
			return filepath.SkipDir
		}
		return nil
	}
	info, err := d.Info()
	if err != nil {
		return fmt.Errorf("stat %s: %w", rel, err)
	}
	mode := info.Mode()
	switch {
	case isDir:
		if err := w.ignores.load(p, rel); err != nil {
			return err
		}
		return w.writeHeader(info, rel+"/", "")
	case mode&fs.ModeSymlink != 0:
		if !w.included(d.Name()) {
			return nil
		}
		target, err := os.Readlink(p)
		if err != nil {
			return fmt.Errorf("read link %s: %w", rel, err)
		}
		if !linkInside(rel, target) {
			return fmt.Errorf("symbolic link %s points outside of %s", rel, w.root)
		}
		return w.writeHeader(info, rel, target)
	case mode.IsRegular():
		if !w.included(d.Name()) {
			return nil
		}
		w.files++
		w.bytes += info.Size()
		if w.files > maxArchiveFiles {
			return fmt.Errorf("local source has more than %d files", maxArchiveFiles)
		}
		if w.bytes > maxArchiveUncompressedBytes {
			return fmt.Errorf("local source exceeds %d bytes", maxArchiveUncompressedBytes)
		}
		if err := w.writeHeader(info, rel, ""); err != nil {
			return err
		}
		f, err := os.Open(p) //nolint:gosec // a file below the user's local source directory
		if err != nil {
			return fmt.Errorf("open %s: %w", rel, err)
		}
		defer func() { _ = f.Close() }()
		if _, err := io.CopyN(w.tw, f, info.Size()); err != nil {
			return fmt.Errorf("archive %s: %w", rel, err)
		}
		return nil
	}
	// Devices, sockets and pipes are not archived.
	return nil
}

func (w *archiveWalker) included(base string) bool {
	if len(w.includes) == 0 {
		return true
	}
	for _, p := range w.includes {
		if ok, _ := filepath.Match(p, base); ok {
			return true
		}
	}
	return false
}

func (w *archiveWalker) writeHeader(info fs.FileInfo, name, link string) error {
	hdr, err := tar.FileInfoHeader(info, link)
	if err != nil {
		return fmt.Errorf("error creating a tar file header: %w", err)
	}
	hdr.Name = name
	hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
	hdr.Format = tar.FormatPAX
	if err := w.tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("error writing header: %w", err)
	}
	return nil
}

// linkInside reports whether a relative link target stays inside the archive root.
func linkInside(linkRel, target string) bool {
	if path.IsAbs(target) || filepath.IsAbs(target) {
		return false
	}
	resolved := path.Clean(path.Join(path.Dir(linkRel), filepath.ToSlash(target)))
	return resolved != ".." && !strings.HasPrefix(resolved, "../")
}

// ignoreRule is one pattern of an ignore file.
type ignoreRule struct {
	base     string // directory of the ignore file, relative to the root ("" for the root)
	pattern  string
	negate   bool
	dirOnly  bool
	anchored bool
}

// ignoreSet holds the rules of every ignore file read so far, in walk order.
type ignoreSet struct {
	rules []ignoreRule
}

func newIgnoreSet() *ignoreSet { return &ignoreSet{} }

// load reads the ignore files of directory dir (relative path rel).
func (s *ignoreSet) load(dir, rel string) error {
	for _, name := range ignoreFileNames {
		p := filepath.Join(dir, name)
		st, err := os.Lstat(p)
		if err != nil || !st.Mode().IsRegular() {
			continue
		}
		if st.Size() > maxIgnoreFileBytes {
			return fmt.Errorf("%s exceeds %d bytes", p, maxIgnoreFileBytes)
		}
		data, err := os.ReadFile(p) //nolint:gosec // ignore file of the user's local source
		if err != nil {
			return fmt.Errorf("read %s: %w", p, err)
		}
		s.rules = append(s.rules, parseIgnoreRules(rel, string(data))...)
	}
	return nil
}

// parseIgnoreRules parses gitignore-style lines.
func parseIgnoreRules(base, content string) []ignoreRule {
	var out []ignoreRule
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimRight(line, " ")
		r := ignoreRule{base: base}
		if strings.HasPrefix(line, "!") {
			r.negate = true
			line = line[1:]
		} else if strings.HasPrefix(line, `\`) {
			line = line[1:]
		}
		if strings.HasSuffix(line, "/") {
			r.dirOnly = true
			line = strings.TrimSuffix(line, "/")
		}
		if strings.Contains(line, "/") {
			r.anchored = true
			line = strings.TrimPrefix(line, "/")
		}
		if line == "" {
			continue
		}
		r.pattern = line
		out = append(out, r)
	}
	return out
}

// ignored reports whether rel (slash-separated, relative to the root) is ignored: the last
// matching rule wins.
func (s *ignoreSet) ignored(rel string, isDir bool) bool {
	ignored := false
	for _, r := range s.rules {
		if r.dirOnly && !isDir {
			continue
		}
		sub := rel
		if r.base != "" {
			if !strings.HasPrefix(rel, r.base+"/") {
				continue
			}
			sub = strings.TrimPrefix(rel, r.base+"/")
		}
		var match bool
		if r.anchored {
			match, _ = doublestar.Match(r.pattern, sub)
		} else {
			match, _ = doublestar.Match(r.pattern, path.Base(sub))
		}
		if match {
			ignored = !r.negate
		}
	}
	return ignored
}
