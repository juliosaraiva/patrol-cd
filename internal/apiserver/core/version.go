package core

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"os/exec"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// ToolVersions are the versions of the rendering tools reported by GET /version.
type ToolVersions struct {
	Kustomize string
	Helm      string
	Kubectl   string
	Jsonnet   string
}

// VersionInfo is the build information of the server plus lazily computed tool versions.
type VersionInfo struct {
	Version        string
	BuildDate      string
	GitCommit      string
	GitTag         string
	GitTreeState   string
	ExtraBuildInfo map[string]string
	// Tools computes the tool versions; it is called once, on the first authenticated
	// version request, and its result is cached. Nil reports no tool versions.
	Tools func(ctx context.Context) ToolVersions

	once  sync.Once
	tools ToolVersions
}

// BuildVersionInfo returns a VersionInfo from the Go build information (module
// version, VCS revision, time and modification state). Fields set at link time by the
// caller take precedence.
func BuildVersionInfo() *VersionInfo {
	v := &VersionInfo{Version: "devel", GitTreeState: ""}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return v
	}
	if bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		v.Version = bi.Main.Version
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			v.GitCommit = s.Value
		case "vcs.time":
			v.BuildDate = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				v.GitTreeState = "dirty"
			} else {
				v.GitTreeState = "clean"
			}
		}
	}
	return v
}

// ModuleVersion returns the version of a dependency module from the build information
// ("" when absent).
func ModuleVersion(path string) string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, d := range bi.Deps {
		if d.Path == path {
			if d.Replace != nil && d.Replace.Version != "" {
				return d.Replace.Version
			}
			return d.Version
		}
	}
	return ""
}

// toolTimeout bounds each tool version command.
const toolTimeout = 10 * time.Second

// maxToolOutput bounds the output read from a tool version command.
const maxToolOutput = 4 << 10

// ExecToolVersions returns a VersionInfo.Tools function that runs "helm version" and
// "kustomize version" (empty paths look the binaries up on PATH) and reports the
// client-go and go-jsonnet library versions linked into the server for kubectl and
// jsonnet. A failing tool reports its error text as the version, as upstream.
func ExecToolVersions(helmPath, kustomizePath string) func(ctx context.Context) ToolVersions {
	if helmPath == "" {
		helmPath = "helm"
	}
	if kustomizePath == "" {
		kustomizePath = "kustomize"
	}
	return func(ctx context.Context) ToolVersions {
		tv := ToolVersions{
			Helm:      runTool(ctx, helmPath, "version", "--short"),
			Kustomize: runTool(ctx, kustomizePath, "version"),
			Kubectl:   ModuleVersion("k8s.io/client-go"),
			Jsonnet:   ModuleVersion("github.com/google/go-jsonnet"),
		}
		return tv
	}
}

func runTool(ctx context.Context, bin string, args ...string) string {
	ctx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // fixed tool paths from server configuration
	var out limitedBuffer
	out.limit = maxToolOutput
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Sprintf("%s: %v", bin, err)
	}
	return strings.TrimSpace(out.String())
}

type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if room := b.limit - b.Len(); room < len(p) {
		p = p[:max(room, 0)]
	}
	b.Buffer.Write(p)
	return n, nil
}

func (v *VersionInfo) toolVersions(ctx context.Context) ToolVersions {
	if v.Tools == nil {
		return ToolVersions{}
	}
	v.once.Do(func() {
		// Detached from the request: the result is cached for every later caller.
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*toolTimeout)
		defer cancel()
		v.tools = v.Tools(cctx)
	})
	return v.tools
}

// getVersion serves GET /api/v1/version and /api/version: callers that are not logged
// in only learn the version, unless anonymous access is enabled or authentication is
// disabled (upstream VersionService).
func (h *Handlers) getVersion(ctx context.Context, _ *apiclient.Empty) (*apiclient.Version, error) {
	vi := h.d.Version
	if vi == nil {
		vi = BuildVersionInfo()
	}
	out := &apiclient.Version{Version: vi.Version}
	id := rest.IdentityFrom(ctx)
	if !id.LoggedIn() && (id == nil || (!id.Anonymous && !id.AuthDisabled)) {
		return out, nil
	}
	tv := vi.toolVersions(ctx)
	out.BuildDate = vi.BuildDate
	out.GitCommit = vi.GitCommit
	out.GitTag = vi.GitTag
	out.GitTreeState = vi.GitTreeState
	out.GoVersion = runtime.Version()
	out.Compiler = runtime.Compiler
	out.Platform = runtime.GOOS + "/" + runtime.GOARCH
	out.KustomizeVersion = tv.Kustomize
	out.HelmVersion = tv.Helm
	out.KubectlVersion = tv.Kubectl
	out.JsonnetVersion = tv.Jsonnet
	out.ExtraBuildInfo = maps.Clone(vi.ExtraBuildInfo)
	return out, nil
}
