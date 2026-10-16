package cmdutil

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
)

// Build information set at link time, e.g.
// -ldflags "-X github.com/juliosaraiva/patrol-cd/internal/cli/cmdutil.version=v1.2.3".
var (
	version        = ""
	buildDate      = ""
	gitCommit      = ""
	gitTag         = ""
	gitTreeState   = ""
	extraBuildInfo = ""
)

// ClientVersion is the build information of the CLI.
type ClientVersion struct {
	Version        string `json:"Version"`
	BuildDate      string `json:"BuildDate"`
	GitCommit      string `json:"GitCommit"`
	GitTag         string `json:"GitTag"`
	GitTreeState   string `json:"GitTreeState"`
	GoVersion      string `json:"GoVersion"`
	Compiler       string `json:"Compiler"`
	Platform       string `json:"Platform"`
	ExtraBuildInfo string `json:"ExtraBuildInfo,omitempty"`
}

// String returns the version followed by the short commit when known.
func (v ClientVersion) String() string {
	if v.GitCommit == "" || v.GitTag != "" {
		return v.Version
	}
	c := v.GitCommit
	if len(c) > 7 {
		c = c[:7]
	}
	if strings.Contains(v.Version, c) {
		// Module pseudo-versions already carry the commit.
		return v.Version
	}
	return fmt.Sprintf("%s+%s", v.Version, c)
}

// BuildVersion returns the link-time build information, completed from the Go build
// information (module version, VCS revision, time and modification state).
func BuildVersion() ClientVersion {
	v := ClientVersion{
		Version: version, BuildDate: buildDate, GitCommit: gitCommit, GitTag: gitTag,
		GitTreeState: gitTreeState, ExtraBuildInfo: extraBuildInfo,
		GoVersion: runtime.Version(), Compiler: runtime.Compiler, Platform: runtime.GOOS + "/" + runtime.GOARCH,
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		if v.Version == "" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			v.Version = bi.Main.Version
		}
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				if v.GitCommit == "" {
					v.GitCommit = s.Value
				}
			case "vcs.time":
				if v.BuildDate == "" {
					v.BuildDate = s.Value
				}
			case "vcs.modified":
				if v.GitTreeState == "" {
					v.GitTreeState = map[bool]string{true: "dirty", false: "clean"}[s.Value == "true"]
				}
			}
		}
	}
	if v.Version == "" {
		v.Version = "devel"
	}
	return v
}
