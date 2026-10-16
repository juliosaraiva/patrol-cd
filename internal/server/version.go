package server

import (
	"encoding/json"
	"fmt"
	"io"
	"runtime"

	"github.com/juliosaraiva/patrol-cd/internal/apiserver/core"
)

// Build information set at link time (hack/build.mk):
//
//	-X github.com/juliosaraiva/patrol-cd/internal/server.version=v0.1.0
//	-X github.com/juliosaraiva/patrol-cd/internal/server.gitCommit=<sha>
//	-X github.com/juliosaraiva/patrol-cd/internal/server.gitTag=<tag>
//	-X github.com/juliosaraiva/patrol-cd/internal/server.gitTreeState=clean|dirty
//	-X github.com/juliosaraiva/patrol-cd/internal/server.buildDate=<RFC 3339>
//
// Unset values fall back to the Go build information (module version, VCS stamp).
var (
	version      string
	gitCommit    string
	gitTag       string
	gitTreeState string
	buildDate    string
)

// VersionInfo returns the build information of this binary, without tool versions.
func VersionInfo() *core.VersionInfo {
	v := core.BuildVersionInfo()
	if version != "" {
		v.Version = version
	}
	if gitCommit != "" {
		v.GitCommit = gitCommit
	}
	if gitTag != "" {
		v.GitTag = gitTag
	}
	if gitTreeState != "" {
		v.GitTreeState = gitTreeState
	}
	if buildDate != "" {
		v.BuildDate = buildDate
	}
	return v
}

// versionDoc is the JSON form of `patrolcd version -o json`.
type versionDoc struct {
	Version      string `json:"version"`
	BuildDate    string `json:"buildDate,omitempty"`
	GitCommit    string `json:"gitCommit,omitempty"`
	GitTag       string `json:"gitTag,omitempty"`
	GitTreeState string `json:"gitTreeState,omitempty"`
	GoVersion    string `json:"goVersion"`
	Compiler     string `json:"compiler"`
	Platform     string `json:"platform"`
}

// writeVersion prints the build information: the version only (short), JSON, or one
// "key: value" line per field.
func writeVersion(w io.Writer, short bool, output string) error {
	v := VersionInfo()
	doc := versionDoc{
		Version: v.Version, BuildDate: v.BuildDate, GitCommit: v.GitCommit, GitTag: v.GitTag,
		GitTreeState: v.GitTreeState, GoVersion: runtime.Version(), Compiler: runtime.Compiler,
		Platform: runtime.GOOS + "/" + runtime.GOARCH,
	}
	switch {
	case short:
		_, err := fmt.Fprintf(w, "patrolcd: %s\n", doc.Version)
		return err
	case output == "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(doc)
	case output != "" && output != "wide":
		return fmt.Errorf("unknown output format %q (json or wide)", output)
	}
	_, err := fmt.Fprintf(w, "patrolcd: %s\n  BuildDate: %s\n  GitCommit: %s\n  GitTag: %s\n  GitTreeState: %s\n  GoVersion: %s\n  Compiler: %s\n  Platform: %s\n",
		doc.Version, doc.BuildDate, doc.GitCommit, doc.GitTag, doc.GitTreeState, doc.GoVersion, doc.Compiler, doc.Platform)
	return err
}
