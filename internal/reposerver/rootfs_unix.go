//go:build unix

package reposerver

import (
	"io/fs"
	"os"
	"syscall"
)

// defaultRootFS is the real file system view prepareRoot uses.
func defaultRootFS() rootFS {
	return rootFS{chmod: os.Chmod, owner: fileOwner, euid: uint32(os.Geteuid())} //nolint:gosec // uids are non-negative
}

// fileOwner returns the uid that owns the file; false when the platform does not say.
func fileOwner(fi fs.FileInfo) (uint32, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Uid, true
}
