package render

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/juliosaraiva/patrol-cd/internal/sandbox"
)

// ErrScratchCopyLimit reports a repository too large for the Kustomize scratch copy.
var ErrScratchCopyLimit = errors.New("repository exceeds the scratch copy limit")

// copyTree copies the tree at src into the new directory dst for Kustomize overrides,
// so `kustomize edit` and Helm chart inflation never modify the shared checkout.
// ".git" entries are skipped; directories, regular files (permission bits kept) and
// symlinks (targets kept verbatim) are copied; other file types are skipped. Writes
// go through an os.Root on dst. The copied tree is then checked for out-of-bounds
// symlinks, so links that leave the copy (absolute, or relative chains escaping) fail
// the call. maxBytes and maxFiles bound the copy (negative: unlimited).
func copyTree(ctx context.Context, src, dst string, maxBytes int64, maxFiles int) error {
	if err := os.Mkdir(dst, 0o700); err != nil {
		return fmt.Errorf("create scratch copy: %w", err)
	}
	root, err := os.OpenRoot(dst)
	if err != nil {
		return fmt.Errorf("open scratch copy: %w", err)
	}
	defer func() { _ = root.Close() }()
	var total int64
	files := 0
	err = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if p == src {
			return nil
		}
		if d.Name() == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		files++
		if maxFiles > 0 && files > maxFiles {
			return fmt.Errorf("%w: more than %d entries", ErrScratchCopyLimit, maxFiles)
		}
		switch t := d.Type(); {
		case t.IsDir():
			info, err := d.Info()
			if err != nil {
				return err
			}
			return root.Mkdir(rel, info.Mode().Perm()|0o700)
		case t&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			if filepath.IsAbs(target) {
				return &sandbox.OutOfBoundsSymlinkError{Path: rel, Target: target}
			}
			return root.Symlink(target, rel)
		case t.IsRegular():
			info, err := d.Info()
			if err != nil {
				return err
			}
			total += info.Size()
			if maxBytes > 0 && total > maxBytes {
				return fmt.Errorf("%w: more than %d bytes", ErrScratchCopyLimit, maxBytes)
			}
			return copyFileInto(root, p, rel, info.Mode().Perm()|0o600, info.Size())
		default:
			return nil
		}
	})
	if err != nil {
		return err
	}
	return sandbox.CheckSymlinksContext(ctx, dst)
}

func copyFileInto(root *os.Root, src, rel string, perm fs.FileMode, size int64) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	// Copy at most the size seen during the walk (plus one byte to detect growth).
	n, err := io.Copy(out, io.LimitReader(in, size+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n > size {
		return fmt.Errorf("%s changed while copying", rel)
	}
	return nil
}
