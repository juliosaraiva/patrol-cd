package deploy_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// Environment variables of the harness.
const (
	// envAssets is envtest's binary directory (kube-apiserver, etcd).
	envAssets = "KUBEBUILDER_ASSETS"
	// envRequireEnvtest makes a missing envtest a failure instead of a skip.
	envRequireEnvtest = "PATROL_REQUIRE_ENVTEST"
	// envRequireTools makes a missing helm or kustomize a failure instead of a skip.
	envRequireTools = "PATROL_REQUIRE_DEPLOY_TOOLS"
	// envKeepLogs names a directory receiving the component and audit logs of the RBAC runs.
	envKeepLogs = "PATROL_DEPLOY_LOG_DIR"
)

// Fixed rendering inputs: the release name and namespace of the static renders, and the
// Kubernetes version used for .Capabilities (envtest's version, the newest the baseline is
// tested on).
const (
	releaseName  = "patrol"
	releaseNS    = "patrol"
	kubeVersion  = "1.36.0"
	envtestMinor = "1.36"
)

// repoRoot is the module root, from this file's location.
func repoRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("cannot locate the source file") // static: runtime.Caller(0) only fails without debug info
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func chartDir() string { return filepath.Join(repoRoot(), "deploy", "helm", "patrol-cd") }
func crdDir() string   { return filepath.Join(repoRoot(), "deploy", "crds") }

// skipOrFail skips the test with a "SKIPPED:" reason, or fails it when the environment
// variable require is set to a non-empty value.
func skipOrFail(t testing.TB, require, format string, args ...any) {
	t.Helper()
	msg := fmt.Sprintf(format, args...)
	if os.Getenv(require) != "" {
		t.Fatalf("%s is set but: %s", require, msg)
	}
	t.Skipf("SKIPPED: %s (set %s=1 to make this a failure)", msg, require)
}

// lookTool returns the first executable among $envVar and the candidates (absolute paths or
// names looked up in PATH).
func lookTool(envVar string, candidates ...string) (string, error) {
	if v := os.Getenv(envVar); v != "" {
		return exec.LookPath(v)
	}
	var last error
	for _, c := range candidates {
		p, err := exec.LookPath(c)
		if err == nil {
			return p, nil
		}
		last = err
	}
	return "", last
}

var (
	helmOnce    sync.Once
	helmPath    string
	helmErr     error
	helmVersion string
)

// helmBin returns the helm binary, skipping (or failing) the test when it is missing.
func helmBin(t testing.TB) string {
	t.Helper()
	helmOnce.Do(func() {
		helmPath, helmErr = lookTool("HELM", "helm")
		if helmErr == nil {
			out, err := exec.Command(helmPath, "version", "--short").Output()
			if err != nil {
				helmErr = fmt.Errorf("%s version: %w", helmPath, err)
				return
			}
			helmVersion = strings.TrimSpace(string(out))
		}
	})
	if helmErr != nil {
		skipOrFail(t, envRequireTools, "helm is not available: %v", helmErr)
	}
	return helmPath
}

// kustomizeBin returns kustomize (bin/kustomize installed by `make tools`, $KUSTOMIZE or PATH).
func kustomizeBin(t testing.TB) string {
	t.Helper()
	p, err := lookTool("KUSTOMIZE", filepath.Join(repoRoot(), "bin", "kustomize"), "kustomize")
	if err != nil {
		skipOrFail(t, envRequireTools, "kustomize is not available: %v", err)
	}
	return p
}

// envtestAssets returns the envtest binary directory. The heavy tests run only when
// KUBEBUILDER_ASSETS is set or PATROL_REQUIRE_ENVTEST is set (then bin/envtest is used when
// KUBEBUILDER_ASSETS is not); -short skips them.
func envtestAssets(t testing.TB) string {
	t.Helper()
	if testing.Short() {
		t.Skip("SKIPPED: envtest-backed deployment test in -short mode")
	}
	dir := os.Getenv(envAssets)
	if dir == "" && os.Getenv(envRequireEnvtest) != "" {
		dir = filepath.Join(repoRoot(), "bin", "envtest", "k8s", kubeVersion+"-"+runtime.GOOS+"-"+runtime.GOARCH)
	}
	if dir == "" {
		skipOrFail(t, envRequireEnvtest, "%s is not set (run `make envtest` and export its output)", envAssets)
	}
	if _, err := os.Stat(filepath.Join(dir, "kube-apiserver")); err != nil {
		skipOrFail(t, envRequireEnvtest, "no kube-apiserver in %s: %v", dir, err)
	}
	return dir
}

// cmdResult is the outcome of an external command.
type cmdResult struct {
	stdout, stderr []byte
	err            error
}

func (r cmdResult) combined() string { return string(r.stdout) + string(r.stderr) }

// runCmd runs an external command with a deadline; env replaces the environment when not nil.
func runCmd(t testing.TB, timeout time.Duration, env []string, name string, args ...string) cmdResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	if env != nil {
		cmd.Env = env
	}
	cmd.Dir = repoRoot()
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	if ctx.Err() != nil {
		err = fmt.Errorf("%s %s: timed out after %s", name, strings.Join(args, " "), timeout)
	}
	return cmdResult{stdout: out.Bytes(), stderr: errOut.Bytes(), err: err}
}

// helmEnv is an isolated helm environment: no user repositories, registries, plugins or cache.
func helmEnv(t testing.TB, extra ...string) []string {
	t.Helper()
	home := t.TempDir()
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"HELM_CACHE_HOME=" + filepath.Join(home, "cache"),
		"HELM_CONFIG_HOME=" + filepath.Join(home, "config"),
		"HELM_DATA_HOME=" + filepath.Join(home, "data"),
		"HELM_NO_PLUGINS=1",
	}
	return append(env, extra...)
}
