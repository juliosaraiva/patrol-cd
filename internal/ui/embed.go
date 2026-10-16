// Package ui embeds the web UI build (ui/dist, copied into internal/ui/dist by
// `make ui-build`) into the patrolcd binary.
//
// The repository tracks only dist/.keep, so `go build` works without a UI build; the
// server then serves a built-in page explaining how to build the UI.
package ui

import (
	"embed"
	"io/fs"
)

// dist holds the copied ui/dist tree (all: keeps dot-files such as .keep, which the UI
// handler never serves).
//
//go:embed all:dist
var dist embed.FS

// notBuilt is served when dist has no index.html.
//
//go:embed notbuilt
var notBuilt embed.FS

// FS returns the embedded UI and whether it is a real build. When the UI was not built
// it returns a minimal file system whose index.html explains how to build it.
func FS() (fs.FS, bool) {
	sub, err := fs.Sub(dist, "dist")
	if err == nil {
		if st, err := fs.Stat(sub, "index.html"); err == nil && st.Mode().IsRegular() {
			return sub, true
		}
	}
	fallback, err := fs.Sub(notBuilt, "notbuilt")
	if err != nil {
		// The directive above guarantees the directory exists; an error here is a build
		// defect, reported by serving nothing.
		return emptyFS{}, false
	}
	return fallback, false
}

// emptyFS has no files.
type emptyFS struct{}

func (emptyFS) Open(name string) (fs.File, error) {
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}
