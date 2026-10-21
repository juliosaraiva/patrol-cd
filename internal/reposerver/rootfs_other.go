//go:build !unix

package reposerver

import (
	"io/fs"
	"os"
)

// defaultRootFS is the real file system view prepareRoot uses. Ownership is not available here,
// so a root dir whose mode cannot be changed is refused rather than adopted.
func defaultRootFS() rootFS {
	return rootFS{chmod: os.Chmod, owner: func(fs.FileInfo) (uint32, bool) { return 0, false }}
}
