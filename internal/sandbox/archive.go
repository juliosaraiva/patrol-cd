package sandbox

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// Defaults applied when the corresponding ArchiveLimits field is zero. The byte
// defaults match upstream's 1G helm/OCI max-extracted-size defaults.
const (
	DefaultArchiveMaxFiles              = 500_000
	DefaultArchiveMaxTotalBytes   int64 = 1 << 30
	DefaultArchiveMaxFileBytes    int64 = 1 << 30
	archiveEntryOverhead          int64 = 64 << 10 // header + PAX records + padding allowance per entry
	archiveInitialStreamAllowance int64 = 1 << 20
	archiveMaxCarry               int64 = 2 << 20 // unused allowance kept across entries
)

// ArchiveLimits bounds archive extraction. Zero numeric fields take the package
// defaults; negative ones disable that limit.
type ArchiveLimits struct {
	// MaxFiles caps the number of archive entries of any type.
	MaxFiles int
	// MaxTotalBytes caps the sum of regular file sizes.
	MaxTotalBytes int64
	// MaxFileBytes caps the size of one regular file.
	MaxFileBytes int64
	// StripExecBits makes every extracted file 0644, matching upstream CMP
	// extraction with preserveFileMode: false. By default the execute bit is kept
	// (0755/0644).
	StripExecBits bool
}

func (l ArchiveLimits) normalized() ArchiveLimits {
	if l.MaxFiles == 0 {
		l.MaxFiles = DefaultArchiveMaxFiles
	}
	if l.MaxTotalBytes == 0 {
		l.MaxTotalBytes = DefaultArchiveMaxTotalBytes
	}
	if l.MaxFileBytes == 0 {
		l.MaxFileBytes = DefaultArchiveMaxFileBytes
	}
	return l
}

// ExtractTarGz extracts a gzip-compressed tar stream into dest. See ExtractTar.
func ExtractTarGz(r io.Reader, dest string, lim ArchiveLimits) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("%w: gzip: %w", ErrInvalidArchive, err)
	}
	defer func() { _ = gz.Close() }()
	return ExtractTar(gz, dest, lim)
}

// ExtractTar extracts a tar stream into dest, creating dest if needed.
//
// Entries are rejected (ErrAbsolutePath, ErrUnsafeArchiveEntry, ErrPathEscape) when
// their name is absolute or contains a '..' component, when a parent directory on
// disk is a symlink, when a symlink target is absolute or leaves dest at any step,
// when a hard link names anything but an already extracted regular file, when the
// entry is a device, fifo or other unsupported type, or when it conflicts with an
// existing entry of a different type. Limits are enforced on entry count, per-file
// and total size, and on the decompressed bytes consumed (so oversized headers or
// skipped payloads cannot be used to burn CPU). Regular files get mode 0755 when
// any execute bit is set and 0644 otherwise; directories get 0755. After the last
// entry the whole tree is re-checked with CheckSymlinks to catch symlink chains.
//
// All writes go through os.Root, so even a missed check cannot write outside dest.
// On error dest may hold partial content; callers should use a fresh directory and
// remove it on failure.
func ExtractTar(r io.Reader, dest string, lim ArchiveLimits) error {
	lim = lim.normalized()
	destAbs, err := filepath.Abs(dest)
	if err != nil {
		return fmt.Errorf("sandbox: resolve extraction dir: %w", err)
	}
	if err := os.MkdirAll(destAbs, 0o755); err != nil {
		return fmt.Errorf("sandbox: create extraction dir: %w", err)
	}
	root, err := os.OpenRoot(destAbs)
	if err != nil {
		return fmt.Errorf("sandbox: open extraction dir: %w", err)
	}
	defer func() { _ = root.Close() }()

	stream := &budgetReader{r: r, budget: archiveInitialStreamAllowance}
	x := &extractor{root: root, tr: tar.NewReader(stream), stream: stream, lim: lim}
	if err := x.run(); err != nil {
		return err
	}
	if err := CheckSymlinks(destAbs); err != nil {
		return fmt.Errorf("sandbox: extracted archive: %w", err)
	}
	return nil
}

type extractor struct {
	root   *os.Root
	tr     *tar.Reader
	stream *budgetReader
	lim    ArchiveLimits

	entries int
	total   int64
}

func (x *extractor) run() error {
	for {
		x.stream.grantEntry()
		hdr, err := x.tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		// ErrInsecurePath (GODEBUG tarinsecurepath=0) comes with a usable header;
		// our own name checks below decide about it.
		insecureName := errors.Is(err, tar.ErrInsecurePath) && hdr != nil
		if err != nil && !insecureName {
			if x.stream.exhausted {
				return fmt.Errorf("%w: archive stream larger than its entries allow", ErrArchiveLimit)
			}
			return fmt.Errorf("%w: %w", ErrInvalidArchive, err)
		}
		x.entries++
		if x.lim.MaxFiles > 0 && x.entries > x.lim.MaxFiles {
			return fmt.Errorf("%w: more than %d entries", ErrArchiveLimit, x.lim.MaxFiles)
		}
		if err := x.entry(hdr); err != nil {
			if x.stream.exhausted {
				return fmt.Errorf("%w: archive stream larger than its entries allow", ErrArchiveLimit)
			}
			return err
		}
	}
}

func (x *extractor) entry(hdr *tar.Header) error {
	if hdr.Typeflag == tar.TypeXGlobalHeader {
		return nil // PAX global header (e.g. git archive commit id); no file
	}
	name, err := entryName(hdr.Name)
	if err != nil {
		return err
	}
	if name == "" {
		return nil // "." or "./": the destination itself
	}
	switch hdr.Typeflag {
	case tar.TypeDir:
		return x.dir(name)
	case tar.TypeReg:
		return x.file(name, hdr)
	case tar.TypeSymlink:
		return x.symlink(name, hdr.Linkname)
	case tar.TypeLink:
		return x.hardlink(name, hdr.Linkname)
	case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		return fmt.Errorf("%w: %q is a device or fifo", ErrUnsafeArchiveEntry, hdr.Name)
	default:
		return fmt.Errorf("%w: %q has unsupported type %q", ErrUnsafeArchiveEntry, hdr.Name, hdr.Typeflag)
	}
}

// entryName validates an archive name and returns it as a cleaned, root-relative
// OS path ("" for the archive root). Backslashes are treated as separators for the
// '..' check so Windows-style traversal names are rejected everywhere.
func entryName(name string) (string, error) {
	if name == "" || strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("%w: invalid entry name %q", ErrUnsafeArchiveEntry, name)
	}
	if strings.HasPrefix(name, "/") || strings.HasPrefix(name, `\`) || filepath.IsAbs(name) || filepath.VolumeName(name) != "" {
		return "", fmt.Errorf("%w: entry %q", ErrAbsolutePath, name)
	}
	for _, c := range strings.FieldsFunc(name, func(r rune) bool { return r == '/' || r == '\\' }) {
		if c == ".." {
			return "", fmt.Errorf("%w: entry %q contains '..'", ErrUnsafeArchiveEntry, name)
		}
	}
	cleaned := path.Clean(name)
	if cleaned == "." {
		return "", nil
	}
	return filepath.FromSlash(cleaned), nil
}

// ensureParents rejects symlinked or non-directory parents of name and creates
// the missing ones.
func (x *extractor) ensureParents(name string) error {
	parent := filepath.Dir(name)
	if parent == "." {
		return nil
	}
	cur := ""
	for _, c := range splitComponents(parent) {
		cur = filepath.Join(cur, c)
		fi, err := x.root.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			break // nothing deeper exists either
		}
		if err != nil {
			return fmt.Errorf("sandbox: inspect %q: %w", cur, err)
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%w: parent %q of %q is a symlink", ErrUnsafeArchiveEntry, cur, name)
		}
		if !fi.IsDir() {
			return fmt.Errorf("%w: parent %q of %q is not a directory", ErrUnsafeArchiveEntry, cur, name)
		}
	}
	if err := x.root.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("sandbox: create %q: %w", parent, err)
	}
	return nil
}

// existing returns the Lstat of name inside the root, or nil if it does not exist.
func (x *extractor) existing(name string) (fs.FileInfo, error) {
	fi, err := x.root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sandbox: inspect %q: %w", name, err)
	}
	return fi, nil
}

func (x *extractor) dir(name string) error {
	if err := x.ensureParents(name); err != nil {
		return err
	}
	fi, err := x.existing(name)
	if err != nil {
		return err
	}
	if fi != nil {
		if fi.IsDir() && fi.Mode()&fs.ModeSymlink == 0 {
			return nil
		}
		return fmt.Errorf("%w: directory %q conflicts with an existing entry", ErrUnsafeArchiveEntry, name)
	}
	if err := x.root.Mkdir(name, 0o755); err != nil {
		return fmt.Errorf("sandbox: create directory %q: %w", name, err)
	}
	return nil
}

func (x *extractor) file(name string, hdr *tar.Header) error {
	size := hdr.Size
	if size < 0 {
		return fmt.Errorf("%w: %q has negative size", ErrInvalidArchive, name)
	}
	if x.lim.MaxFileBytes > 0 && size > x.lim.MaxFileBytes {
		return fmt.Errorf("%w: %q is %d bytes, limit %d", ErrArchiveLimit, name, size, x.lim.MaxFileBytes)
	}
	if x.lim.MaxTotalBytes > 0 && size > x.lim.MaxTotalBytes-x.total {
		return fmt.Errorf("%w: extracted size exceeds %d bytes", ErrArchiveLimit, x.lim.MaxTotalBytes)
	}
	if err := x.ensureParents(name); err != nil {
		return err
	}
	fi, err := x.existing(name)
	if err != nil {
		return err
	}
	if fi != nil && !fi.Mode().IsRegular() {
		return fmt.Errorf("%w: file %q conflicts with an existing entry", ErrUnsafeArchiveEntry, name)
	}
	mode := normalizeFileMode(fs.FileMode(hdr.Mode))
	if x.lim.StripExecBits {
		mode = 0o644
	}
	f, err := x.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("sandbox: create %q: %w", name, err)
	}
	x.stream.grant(size)
	n, copyErr := io.CopyN(f, x.tr, size)
	x.total += n
	chmodErr := f.Chmod(mode)
	closeErr := f.Close()
	switch {
	case copyErr != nil:
		if x.stream.exhausted {
			return fmt.Errorf("%w: archive stream larger than its entries allow", ErrArchiveLimit)
		}
		return fmt.Errorf("%w: write %q: %w", ErrInvalidArchive, name, copyErr)
	case chmodErr != nil:
		return fmt.Errorf("sandbox: chmod %q: %w", name, chmodErr)
	case closeErr != nil:
		return fmt.Errorf("sandbox: close %q: %w", name, closeErr)
	}
	return nil
}

func (x *extractor) symlink(name, target string) error {
	if target == "" {
		return fmt.Errorf("%w: symlink %q has an empty target", ErrUnsafeArchiveEntry, name)
	}
	if filepath.IsAbs(target) || strings.HasPrefix(target, "/") ||
		lexicallyEscapes(filepath.Dir(name), filepath.FromSlash(target)) {
		return &OutOfBoundsSymlinkError{Path: name, Target: target}
	}
	if err := x.ensureParents(name); err != nil {
		return err
	}
	fi, err := x.existing(name)
	if err != nil {
		return err
	}
	if fi != nil {
		return fmt.Errorf("%w: symlink %q conflicts with an existing entry", ErrUnsafeArchiveEntry, name)
	}
	if err := x.root.Symlink(target, name); err != nil {
		return fmt.Errorf("sandbox: create symlink %q: %w", name, err)
	}
	return nil
}

func (x *extractor) hardlink(name, linkname string) error {
	target, err := entryName(linkname)
	if err != nil {
		return fmt.Errorf("hard link %q: %w", name, err)
	}
	if target == "" {
		return fmt.Errorf("%w: hard link %q targets the archive root", ErrUnsafeArchiveEntry, name)
	}
	// The target's parents must be real directories and the target a regular file
	// already extracted from this archive.
	if err := x.checkNoSymlinkParents(target); err != nil {
		return err
	}
	tfi, err := x.existing(target)
	if err != nil {
		return err
	}
	if tfi == nil || !tfi.Mode().IsRegular() {
		return fmt.Errorf("%w: hard link %q must target an extracted regular file", ErrUnsafeArchiveEntry, name)
	}
	if err := x.ensureParents(name); err != nil {
		return err
	}
	fi, err := x.existing(name)
	if err != nil {
		return err
	}
	if fi != nil {
		return fmt.Errorf("%w: hard link %q conflicts with an existing entry", ErrUnsafeArchiveEntry, name)
	}
	if err := x.root.Link(target, name); err != nil {
		return fmt.Errorf("sandbox: create hard link %q: %w", name, err)
	}
	return nil
}

func (x *extractor) checkNoSymlinkParents(name string) error {
	cur := ""
	for _, c := range splitComponents(filepath.Dir(name)) {
		cur = filepath.Join(cur, c)
		fi, err := x.existing(cur)
		if err != nil {
			return err
		}
		if fi == nil {
			return nil
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%w: parent %q of %q is a symlink", ErrUnsafeArchiveEntry, cur, name)
		}
	}
	return nil
}

// normalizeFileMode keeps only "executable or not".
func normalizeFileMode(m fs.FileMode) fs.FileMode {
	if m&0o111 != 0 {
		return 0o755
	}
	return 0o644
}

// budgetReader fails once more bytes are read than have been granted. The
// extractor grants a fixed allowance per entry (unused allowance carries over up to
// archiveMaxCarry, which still fits one maximal 1 MiB PAX header) plus each regular
// file's size, so the decompressed stream is bounded by what the limits allow and
// chained or oversized headers cannot burn CPU.
type budgetReader struct {
	r         io.Reader
	budget    int64
	exhausted bool
}

var errStreamBudget = errors.New("sandbox: archive stream budget exhausted")

func (b *budgetReader) grantEntry() {
	b.budget = min(b.budget, archiveMaxCarry) + archiveEntryOverhead
}

func (b *budgetReader) grant(n int64) {
	switch {
	case n <= 0:
	case n > math.MaxInt64-b.budget:
		b.budget = math.MaxInt64
	default:
		b.budget += n
	}
}

func (b *budgetReader) Read(p []byte) (int, error) {
	if b.budget <= 0 {
		b.exhausted = true
		return 0, errStreamBudget
	}
	if int64(len(p)) > b.budget {
		p = p[:b.budget]
	}
	n, err := b.r.Read(p)
	b.budget -= int64(n)
	return n, err
}

// Tar writes dir as a gzip-compressed tar stream to w, for example to send a
// source tree to a CMP sidecar. See TarCount.
func Tar(dir string, w io.Writer, excludes []string) error {
	_, err := TarCount(dir, w, excludes)
	return err
}

// TarCount writes dir as a gzip-compressed tar stream to w and returns the number
// of regular files written (upstream refuses to stream a tree with none: "no files
// to send").
//
// The output is deterministic for identical trees: entries are written in lexical
// walk order, timestamps are zeroed, ownership is dropped, file modes are
// normalized to 0755/0644 and directories to 0755. Directories, regular files and
// symlinks are included; sockets, devices and fifos are skipped. A symlink that is
// absolute or resolves outside dir fails the call with *OutOfBoundsSymlinkError.
//
// excludes are filepath.Match patterns matched against each entry's dir-relative
// path (upstream --plugin-tar-exclude semantics); an excluded directory is pruned.
func TarCount(dir string, w io.Writer, excludes []string) (int, error) {
	rootAbs, err := filepath.Abs(dir)
	if err != nil {
		return 0, fmt.Errorf("sandbox: resolve tar dir: %w", err)
	}
	fi, err := os.Stat(rootAbs)
	if err != nil {
		return 0, fmt.Errorf("sandbox: tar dir: %w", err)
	}
	if !fi.IsDir() {
		return 0, fmt.Errorf("sandbox: tar dir %s is not a directory", dir)
	}
	for _, p := range excludes {
		if _, err := filepath.Match(p, ""); err != nil {
			return 0, fmt.Errorf("error verifying exclusion pattern %q: %w", p, err)
		}
	}
	root, err := os.OpenRoot(rootAbs)
	if err != nil {
		return 0, fmt.Errorf("sandbox: open tar dir: %w", err)
	}
	defer func() { _ = root.Close() }()

	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	files := 0
	walkErr := filepath.WalkDir(rootAbs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("sandbox: walk for tar: %w", err)
		}
		if p == rootAbs {
			return nil
		}
		rel, err := filepath.Rel(rootAbs, p)
		if err != nil {
			return fmt.Errorf("sandbox: relative path for %s: %w", p, err)
		}
		for _, pat := range excludes {
			if ok, _ := filepath.Match(pat, rel); ok {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		regular, err := writeTarEntry(tw, root, rootAbs, rel, d)
		if regular {
			files++
		}
		return err
	})
	twErr := tw.Close()
	gzErr := gz.Close()
	switch {
	case walkErr != nil:
		return files, walkErr
	case twErr != nil:
		return files, fmt.Errorf("sandbox: finish tar: %w", twErr)
	case gzErr != nil:
		return files, fmt.Errorf("sandbox: finish gzip: %w", gzErr)
	}
	return files, nil
}

var tarEpoch = time.Unix(0, 0)

// writeTarEntry writes one walked entry and reports whether it was a regular file.
func writeTarEntry(tw *tar.Writer, root *os.Root, rootAbs, rel string, d fs.DirEntry) (bool, error) {
	name := filepath.ToSlash(rel)
	hdr := &tar.Header{Name: name, ModTime: tarEpoch, Format: tar.FormatPAX}
	switch {
	case d.Type()&fs.ModeSymlink != 0:
		target, err := root.Readlink(rel)
		if err != nil {
			return false, fmt.Errorf("sandbox: read link %s: %w", rel, err)
		}
		if err := checkLinkTarget(rootAbs, rel, target); err != nil {
			return false, err
		}
		hdr.Typeflag = tar.TypeSymlink
		hdr.Linkname = filepath.ToSlash(target)
		hdr.Mode = 0o777
		return false, writeTarHeader(tw, hdr)
	case d.IsDir():
		hdr.Typeflag = tar.TypeDir
		hdr.Name = name + "/"
		hdr.Mode = 0o755
		return false, writeTarHeader(tw, hdr)
	case d.Type().IsRegular():
		return true, writeTarFile(tw, root, rel, hdr)
	default:
		return false, nil // sockets, devices, fifos are not sent
	}
}

func writeTarFile(tw *tar.Writer, root *os.Root, rel string, hdr *tar.Header) error {
	f, err := root.Open(rel)
	if err != nil {
		return fmt.Errorf("sandbox: open %s: %w", rel, err)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("sandbox: stat %s: %w", rel, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%w: %s changed type during tar", ErrUnsafeArchiveEntry, rel)
	}
	hdr.Typeflag = tar.TypeReg
	hdr.Size = fi.Size()
	hdr.Mode = int64(normalizeFileMode(fi.Mode()))
	if err := writeTarHeader(tw, hdr); err != nil {
		return err
	}
	if _, err := io.CopyN(tw, f, hdr.Size); err != nil {
		return fmt.Errorf("sandbox: tar %s: %w", rel, err)
	}
	return nil
}

func writeTarHeader(tw *tar.Writer, hdr *tar.Header) error {
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("sandbox: tar header %s: %w", hdr.Name, err)
	}
	return nil
}
