package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

const (
	// maxSymlinkHops bounds the symlinks followed in one resolution. It equals the
	// limit of filepath.EvalSymlinks (used by kustomize and other Go tools) and is
	// above every kernel limit (Linux 40, macOS 32), so any chain those resolvers
	// can follow is followed here too; longer chains are loops for all of them.
	maxSymlinkHops = 255
	// maxResolveSteps bounds the path components processed by one resolution:
	// the input path plus every expanded link target. Real paths need a few
	// dozen; the kernel's PATH_MAX keeps a single target under 2048 components.
	maxResolveSteps = 4096
	// walkInitialSteps and walkStepsPerEntry bound the total resolution work of
	// one CheckSymlinks or Tar walk to walkInitialSteps + walkStepsPerEntry per
	// walked entry, so a tree full of individually-legal but expensive links
	// fails closed instead of costing O(links × maxResolveSteps).
	walkInitialSteps  = 1 << 20
	walkStepsPerEntry = 256
	// lookupCacheMaxBytes bounds the per-resolver lstat cache (keys plus link
	// targets plus a per-entry allowance); the cache is cleared when it is full.
	lookupCacheMaxBytes = 8 << 20
)

// SecureJoin joins rel onto root, resolving symlinks the way the kernel would, and
// returns an absolute path that is guaranteed to lie within root (or be root).
//
// It fails with ErrAbsolutePath when rel is absolute, with ErrPathEscape when a
// '..' component or a symlink would leave root (absolute symlink targets always
// count as escapes, even if they point inside root), with ErrTooManySymlinks for
// loops, with ErrSymlinkBudget when resolution needs more than a bounded amount
// of work (see maxResolveSteps), and with fs.ErrInvalid for a NUL byte. Components that do not exist are joined
// lexically, so the result may name a file that is about to be created. root
// itself is made absolute but its own symlinks are not resolved.
//
// The check is not atomic: a concurrent writer inside root can swap a directory for
// a symlink after SecureJoin returns. Use os.Root for writes into trees other
// processes can modify.
func SecureJoin(root, rel string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("%w: empty root", ErrPathEscape)
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: %q", ErrAbsolutePath, rel)
	}
	if strings.IndexByte(rel, 0) >= 0 {
		// Rejected explicitly: components below a missing one are joined without
		// a system call, so the OS would not reject the NUL byte for us.
		return "", fmt.Errorf("sandbox: join %q: %w: NUL byte in path", rel, fs.ErrInvalid)
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("sandbox: resolve root: %w", err)
	}
	comps, err := newResolver(rootAbs).resolve(nil, rel)
	if err != nil {
		return "", fmt.Errorf("sandbox: join %q: %w", rel, err)
	}
	return filepath.Join(rootAbs, filepath.Join(comps...)), nil
}

// nodeKind classifies a looked-up path.
type nodeKind uint8

const (
	nodeMissing nodeKind = iota // does not exist, or a parent is not a directory
	nodeOther                   // exists and is not a symlink
	nodeLink                    // symlink; node.target holds its raw target
)

type node struct {
	kind   nodeKind
	target string
}

// resolver resolves root-relative paths physically (through symlinks) without
// ever stepping above root, with bounded work. One resolver serves one SecureJoin
// call or one walk; it is not safe for concurrent use. Its lstat cache assumes
// the tree does not change during that call or walk (a stale entry can only make
// a check describe the tree as it was when first looked at).
type resolver struct {
	root string

	// walkLimited enables the per-walk step budget in walkSteps.
	walkLimited bool
	walkSteps   int64

	cache      map[string]node
	cacheBytes int

	// lstats counts lstat system calls (for tests).
	lstats int
}

func newResolver(root string) *resolver {
	return &resolver{root: root, cache: make(map[string]node)}
}

// newWalkResolver returns a resolver whose total work is bounded by
// walkInitialSteps plus walkStepsPerEntry for every grantEntry call.
func newWalkResolver(root string) *resolver {
	r := newResolver(root)
	r.walkLimited = true
	r.walkSteps = walkInitialSteps
	return r
}

// grantEntry adds the per-entry allowance for one walked entry.
func (r *resolver) grantEntry() {
	if r.walkLimited {
		r.walkSteps += walkStepsPerEntry
	}
}

func (r *resolver) spend() bool {
	if !r.walkLimited {
		return true
	}
	if r.walkSteps <= 0 {
		return false
	}
	r.walkSteps--
	return true
}

// pathStack is a stack of root-relative components with their joined form kept
// incrementally, so each step costs O(len(component)) instead of O(len(path)).
type pathStack struct {
	comps []string
	buf   []byte
	ends  []int
}

func (s *pathStack) push(c string) {
	if len(s.buf) > 0 {
		s.buf = append(s.buf, filepath.Separator)
	}
	s.buf = append(s.buf, c...)
	s.comps = append(s.comps, c)
	s.ends = append(s.ends, len(s.buf))
}

func (s *pathStack) pop() {
	s.comps = s.comps[:len(s.comps)-1]
	s.ends = s.ends[:len(s.ends)-1]
	n := 0
	if len(s.ends) > 0 {
		n = s.ends[len(s.ends)-1]
	}
	s.buf = s.buf[:n]
}

// resolve resolves rel starting at the directory base (root-relative components
// that contain no symlinks) and returns the resolved root-relative components. It
// never steps above root: '..' at the root fails with ErrPathEscape. Absolute link
// targets fail with *OutOfBoundsSymlinkError. More than maxSymlinkHops links fail
// with ErrTooManySymlinks; more than maxResolveSteps components (or an exhausted
// walk budget) fail with ErrSymlinkBudget.
func (r *resolver) resolve(base []string, rel string) ([]string, error) {
	var p pathStack
	for _, c := range base {
		p.push(c)
	}
	// pending is reversed: the next component is the last element, so expanding
	// a link appends its target instead of copying the whole remainder.
	pending := splitComponents(rel)
	slices.Reverse(pending)
	steps, hops := 0, 0
	// missingAt > 0 means p.comps[missingAt-1] does not exist, so nothing below it
	// does either and no lstat is needed until a '..' climbs back above it.
	missingAt := 0
	for len(pending) > 0 {
		// Every pending component costs one step, so fail as soon as the
		// remaining work cannot fit.
		if steps+len(pending) > maxResolveSteps || !r.spend() {
			return nil, ErrSymlinkBudget
		}
		steps++
		comp := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if comp == ".." {
			if len(p.comps) == 0 {
				return nil, ErrPathEscape
			}
			p.pop()
			if len(p.comps) < missingAt {
				missingAt = 0
			}
			continue
		}
		p.push(comp)
		if missingAt > 0 {
			continue // joined lexically below a missing component
		}
		n, err := r.lookup(p.buf)
		if err != nil {
			return nil, err
		}
		switch n.kind {
		case nodeMissing:
			missingAt = len(p.comps)
			continue
		case nodeOther:
			continue
		}
		hops++
		if hops > maxSymlinkHops {
			return nil, ErrTooManySymlinks
		}
		if filepath.IsAbs(n.target) {
			return nil, &OutOfBoundsSymlinkError{Path: string(p.buf), Target: n.target}
		}
		// A relative target is interpreted from the link's parent directory.
		p.pop()
		target := splitComponents(n.target)
		for i := len(target) - 1; i >= 0; i-- {
			pending = append(pending, target[i])
		}
	}
	if missingAt > 0 && len(p.comps) > missingAt {
		// Components below a missing one were joined without system calls; one
		// lstat of the result surfaces what the OS rejects (ENAMETOOLONG,
		// EINVAL), as a component-by-component walk would.
		if _, err := r.lookup(p.buf); err != nil {
			return nil, err
		}
	}
	return p.comps, nil
}

// lookup lstats the root-relative path rel, caching the result.
func (r *resolver) lookup(rel []byte) (node, error) {
	if n, ok := r.cache[string(rel)]; ok {
		return n, nil
	}
	key := string(rel)
	full := filepath.Join(r.root, key)
	r.lstats++
	fi, err := os.Lstat(full)
	var n node
	switch {
	case err != nil:
		if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
			return node{}, err
		}
		n.kind = nodeMissing
	case fi.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(full)
		switch {
		case err == nil:
			n = node{kind: nodeLink, target: target}
		case errors.Is(err, fs.ErrNotExist):
			n.kind = nodeMissing // vanished
		default:
			return node{}, err
		}
	default:
		n.kind = nodeOther
	}
	size := len(key) + len(n.target) + 64
	if r.cacheBytes+size > lookupCacheMaxBytes {
		clear(r.cache)
		r.cacheBytes = 0
	}
	r.cache[key] = n
	r.cacheBytes += size
	return n, nil
}

// splitComponents splits p on the separator and drops empty and "." components.
func splitComponents(p string) []string {
	parts := strings.Split(p, string(filepath.Separator))
	out := parts[:0]
	for _, c := range parts {
		if c != "" && c != "." {
			out = append(out, c)
		}
	}
	return out
}

// CheckSymlinks is CheckSymlinksContext with context.Background().
func CheckSymlinks(root string, skip ...string) error {
	return CheckSymlinksContext(context.Background(), root, skip...)
}

// CheckSymlinksContext walks root and returns *OutOfBoundsSymlinkError for the
// first symlink (in lexical walk order) that is absolute or resolves outside root.
//
// It mirrors upstream CheckOutOfBoundsSymlinks: absolute targets are always
// rejected, a relative target is walked component by component from the link's
// directory and rejected as soon as any step leaves root (even if it would come
// back), dangling targets that stay inside root are allowed, entries that vanish
// during the walk are ignored, and a root that is itself a symlink is reported as
// an out-of-bounds symlink with Path ".". In addition, each target is resolved
// physically through other symlinks, so chains such as "d -> ." plus "x -> d/.."
// (lexically inside, physically outside) are rejected too. Loops are not escapes:
// no resolver can follow them either.
//
// Resolution work is bounded per link (maxResolveSteps) and per walk
// (walkInitialSteps + walkStepsPerEntry per entry). A link that exceeds a budget
// fails closed with an error matching both ErrSymlinkBudget and
// *OutOfBoundsSymlinkError. The context is checked before every entry.
//
// skip lists root-relative paths (for example ".git") whose subtrees are not checked.
func CheckSymlinksContext(ctx context.Context, root string, skip ...string) error {
	_, err := checkSymlinks(ctx, root, skip)
	return err
}

// checkSymlinks implements CheckSymlinksContext and returns the resolver for tests.
func checkSymlinks(ctx context.Context, root string, skip []string) (*resolver, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("sandbox: resolve root: %w", err)
	}
	skipSet := make(map[string]bool, len(skip))
	for _, p := range skip {
		skipSet[filepath.Clean(p)] = true
	}
	r := newWalkResolver(rootAbs)
	err = filepath.WalkDir(rootAbs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && p != rootAbs {
				return nil // e.g. transient .git/*.lock files
			}
			return fmt.Errorf("sandbox: walk %s for symlinks: %w", root, err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if p == rootAbs {
			// WalkDir does not descend into a symlinked root, so nothing below it
			// would be checked: fail closed, as upstream does.
			return rootSymlinkError(rootAbs, d)
		}
		r.grantEntry()
		rel, err := filepath.Rel(rootAbs, p)
		if err != nil {
			return fmt.Errorf("sandbox: relative path for %s: %w", p, err)
		}
		if skipSet[rel] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink == 0 {
			return nil
		}
		n, err := r.lookup([]byte(rel))
		if err != nil {
			return fmt.Errorf("sandbox: read link %s: %w", rel, err)
		}
		if n.kind != nodeLink {
			return nil // vanished or replaced during the walk
		}
		return r.checkLink(rel, n.target)
	})
	return r, err
}

// rootSymlinkError returns *OutOfBoundsSymlinkError{Path: "."} when the walked
// root entry d is a symlink, and nil otherwise.
func rootSymlinkError(rootAbs string, d fs.DirEntry) error {
	if d == nil || d.Type()&fs.ModeSymlink == 0 {
		return nil
	}
	target, _ := os.Readlink(rootAbs) // best effort: only used in the error value
	return &OutOfBoundsSymlinkError{Path: ".", Target: target}
}

// checkLink applies the lexical (upstream) and physical checks to a link at the
// root-relative path rel (whose parents are real directories, as produced by a
// non-following walk) with the given target.
func (r *resolver) checkLink(rel, target string) error {
	oob := &OutOfBoundsSymlinkError{Path: rel, Target: target}
	if filepath.IsAbs(target) {
		return oob
	}
	if lexicallyEscapes(filepath.Dir(rel), target) {
		return oob
	}
	_, err := r.resolve(splitComponents(filepath.Dir(rel)), target)
	switch {
	case err == nil, errors.Is(err, ErrTooManySymlinks):
		return nil
	case errors.Is(err, ErrSymlinkBudget):
		return fmt.Errorf("%w: %w", ErrSymlinkBudget, oob)
	case errors.Is(err, ErrPathEscape):
		return oob
	default:
		return fmt.Errorf("sandbox: resolve link %s: %w", rel, err)
	}
}

// lexicallyEscapes walks target from the root-relative directory dir one
// component at a time and reports whether any step leaves the root.
func lexicallyEscapes(dir, target string) bool {
	depth := len(splitComponents(dir))
	for _, c := range splitComponents(target) {
		if c == ".." {
			depth--
			if depth < 0 {
				return true
			}
			continue
		}
		depth++
	}
	return false
}
