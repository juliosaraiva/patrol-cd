package examples_test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/go-logr/logr"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/juliosaraiva/patrol-cd/internal/envtestx"
)

// envtestK8sVersion is the kube-apiserver version installed by `make envtest`.
const envtestK8sVersion = "1.36.0"

var (
	// repoRoot is the module root; examplesDir is <repoRoot>/examples.
	repoRoot    string
	examplesDir string

	// restCfg is the admin config of the envtest API server, nil when it is unavailable
	// (envUnavailable says why).
	restCfg        *rest.Config
	envUnavailable error
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	repoRoot = filepath.Clean(filepath.Join(wd, "..", ".."))
	if _, err := os.Stat(filepath.Join(repoRoot, "go.mod")); err != nil {
		fmt.Fprintf(os.Stderr, "cannot locate the repository root from %s: %v\n", wd, err)
		return 1
	}
	examplesDir = filepath.Join(repoRoot, "examples")

	// Default to the binaries installed by `make envtest` so `go test ./test/examples` works
	// without exporting KUBEBUILDER_ASSETS.
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		dir := filepath.Join(repoRoot, "bin", "envtest", "k8s", envtestK8sVersion+"-"+runtime.GOOS+"-"+runtime.GOARCH)
		if _, err := os.Stat(filepath.Join(dir, "kube-apiserver")); err == nil {
			_ = os.Setenv("KUBEBUILDER_ASSETS", dir)
		}
	}
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		envUnavailable = errors.New("KUBEBUILDER_ASSETS is not set and bin/envtest has no kube-apiserver (run `make envtest`)")
		return m.Run()
	}

	ctrllog.SetLogger(logr.Discard())
	env := &envtest.Environment{
		// The Patrol CD CRDs and the CRD of the Lua customization example.
		CRDDirectoryPaths:     []string{filepath.Join(repoRoot, "deploy", "crds"), filepath.Join(examplesDir, "plugins", "lua", "crd")},
		ErrorIfCRDPathMissing: true,
		UseExistingCluster:    ptr.To(false),
	}
	cfg, err := envtestx.Start(env)
	if err != nil {
		fmt.Fprintf(os.Stderr, "start envtest control plane: %v\n", err)
		return 1
	}
	defer func() {
		if err := env.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "stop envtest control plane: %v\n", err)
		}
	}()
	restCfg = cfg
	return m.Run()
}

// requireAPIServer returns a config for the envtest API server. Without one the test is
// skipped, or fails when PATROL_REQUIRE_ENVTEST is set (as `make test` does).
func requireAPIServer(t *testing.T) *rest.Config {
	t.Helper()
	if restCfg == nil {
		if os.Getenv("PATROL_REQUIRE_ENVTEST") != "" {
			t.Fatalf("envtest is required (PATROL_REQUIRE_ENVTEST is set): %v", envUnavailable)
		}
		t.Skipf("SKIPPED: no envtest API server: %v", envUnavailable)
	}
	return rest.CopyConfig(restCfg)
}

// tools are the external binaries the source renderers run.
type tools struct {
	helm      string
	kustomize string
}

// findTools locates helm and kustomize: $HELM / $KUSTOMIZE, then bin/kustomize (installed
// by `make tools`), then PATH. The error lists every missing tool.
func findTools() (tools, error) {
	find := func(envVar string, candidates ...string) (string, error) {
		if p := os.Getenv(envVar); p != "" {
			return exec.LookPath(p)
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
	var missing []error
	helm, err := find("HELM", "helm")
	if err != nil {
		missing = append(missing, fmt.Errorf("helm: %w", err))
	}
	kustomize, err := find("KUSTOMIZE", filepath.Join(repoRoot, "bin", "kustomize"), "kustomize")
	if err != nil {
		missing = append(missing, fmt.Errorf("kustomize: %w", err))
	}
	return tools{helm: helm, kustomize: kustomize}, errors.Join(missing...)
}

// requireTools is called by the (sub)tests that render sources, with the error of
// findTools. Missing tools skip the test, or fail it when PATROL_REQUIRE_EXAMPLE_TOOLS or
// PATROL_REQUIRE_ENVTEST is set: `make test` sets the latter and runs without -v, where a
// skip would be invisible.
func requireTools(t *testing.T, missing error) {
	t.Helper()
	if missing == nil {
		return
	}
	for _, v := range []string{"PATROL_REQUIRE_EXAMPLE_TOOLS", "PATROL_REQUIRE_ENVTEST"} {
		if os.Getenv(v) != "" {
			t.Fatalf("rendering tools are required (%s is set): %v", v, missing)
		}
	}
	t.Skipf("SKIPPED: rendering tools missing: %v", missing)
}
