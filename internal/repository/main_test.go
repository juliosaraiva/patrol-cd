package repository

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-logr/logr"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/juliosaraiva/patrol-cd/internal/envtestx"
)

// testEnv is a local kube-apiserver + etcd with the Repository, RepositoryCredential and
// SCMProvider CRDs, shared by the integration tests. It starts only when KUBEBUILDER_ASSETS
// is set.
var (
	testEnv *envtest.Environment
	testCfg *rest.Config
)

func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		if os.Getenv("PATROL_REQUIRE_ENVTEST") == "1" {
			fmt.Fprintln(os.Stderr, "PATROL_REQUIRE_ENVTEST=1 but KUBEBUILDER_ASSETS is not set")
			return 1
		}
		return m.Run()
	}
	ctrllog.SetLogger(logr.Discard())
	crds := filepath.Join("..", "..", "deploy", "crds")
	testEnv = &envtest.Environment{
		UseExistingCluster: ptr.To(false),
		CRDInstallOptions: envtest.CRDInstallOptions{
			Paths: []string{
				filepath.Join(crds, "cd.patrol.example_repositories.yaml"),
				filepath.Join(crds, "cd.patrol.example_repositorycredentials.yaml"),
				filepath.Join(crds, "cd.patrol.example_scmproviders.yaml"),
			},
			ErrorIfPathMissing: true,
		},
	}
	cfg, err := envtestx.Start(testEnv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "start envtest control plane: %v\n", err)
		return 1
	}
	defer func() {
		if err := testEnv.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "stop envtest control plane: %v\n", err)
		}
	}()
	testCfg = cfg
	return m.Run()
}

// envConfig returns an admin REST config for the shared envtest API server, skipping the
// test when envtest binaries are unavailable.
func envConfig(t *testing.T) *rest.Config {
	t.Helper()
	if testCfg == nil {
		t.Skip("KUBEBUILDER_ASSETS not set; skipping envtest integration test")
	}
	return rest.CopyConfig(testCfg)
}
