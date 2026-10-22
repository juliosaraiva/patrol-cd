package sync

import (
	"fmt"
	"os"
	"testing"

	"github.com/go-logr/logr"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/juliosaraiva/patrol-cd/internal/envtestx"
)

// testEnv is a local kube-apiserver + etcd shared by the integration tests of this package.
// It is started only when KUBEBUILDER_ASSETS is set, never against an existing cluster.
// There is no controller manager: no garbage collector, no namespace or job controllers.
var (
	testEnv *envtest.Environment
	testCfg *rest.Config
)

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		return m.Run()
	}
	ctrllog.SetLogger(logr.Discard())
	testEnv = &envtest.Environment{UseExistingCluster: ptr.To(false)}
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
	cfg := rest.CopyConfig(testCfg)
	cfg.WarningHandler = rest.NoWarnings{}
	return cfg
}
