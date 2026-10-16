package clusters

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-logr/logr"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
)

// envExecPlugin turns the test binary into an exec credential plugin (see runExecPlugin).
const envExecPlugin = "PATROL_CLUSTERS_TEST_EXEC_PLUGIN"

// testEnv is a local kube-apiserver + etcd with the Patrol Cluster CRD, shared by the
// integration tests. It is started only when KUBEBUILDER_ASSETS is set.
var (
	testEnv *envtest.Environment
	testCfg *rest.Config
)

func TestMain(m *testing.M) {
	if os.Getenv(envExecPlugin) == "1" {
		os.Exit(runExecPlugin())
	}
	os.Exit(run(m))
}

func run(m *testing.M) int {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		if os.Getenv("PATROL_REQUIRE_ENVTEST") == "1" {
			fmt.Fprintln(os.Stderr, "PATROL_REQUIRE_ENVTEST=1 but KUBEBUILDER_ASSETS is not set")
			return 1
		}
		return m.Run()
	}
	ctrllog.SetLogger(logr.Discard())
	testEnv = &envtest.Environment{
		UseExistingCluster: ptr.To(false),
		CRDInstallOptions: envtest.CRDInstallOptions{
			Paths:              []string{filepath.Join("..", "..", "deploy", "crds", "cd.patrol.example_clusters.yaml")},
			ErrorIfPathMissing: true,
		},
	}
	cfg, err := testEnv.Start()
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

// envConfig returns an admin REST config for the shared envtest API server, skipping the test
// when envtest binaries are unavailable.
func envConfig(t *testing.T) *rest.Config {
	t.Helper()
	if testCfg == nil {
		t.Skip("KUBEBUILDER_ASSETS not set; skipping envtest integration test")
	}
	return rest.CopyConfig(testCfg)
}

// pluginReport is what the fake exec plugin encodes into its token.
type pluginReport struct {
	Args          []string        `json:"args"`
	Foo           string          `json:"foo"`
	ClusterServer string          `json:"clusterServer,omitempty"`
	ClusterConfig json.RawMessage `json:"clusterConfig,omitempty"`
}

// runExecPlugin prints an ExecCredential whose token is the base64url JSON of the plugin's
// arguments, the FOO variable and the cluster information from KUBERNETES_EXEC_INFO.
func runExecPlugin() int {
	rep := pluginReport{Args: os.Args[1:], Foo: os.Getenv("FOO")}
	if info := os.Getenv("KUBERNETES_EXEC_INFO"); info != "" {
		var ec struct {
			Spec struct {
				Cluster *struct {
					Server string          `json:"server"`
					Config json.RawMessage `json:"config"`
				} `json:"cluster"`
			} `json:"spec"`
		}
		if err := json.Unmarshal([]byte(info), &ec); err == nil && ec.Spec.Cluster != nil {
			rep.ClusterServer = ec.Spec.Cluster.Server
			rep.ClusterConfig = ec.Spec.Cluster.Config
		}
	}
	b, err := json.Marshal(rep)
	if err != nil {
		return 1
	}
	out, err := ExecCredentialJSON(Token{Value: base64.RawURLEncoding.EncodeToString(b)})
	if err != nil {
		return 1
	}
	_, _ = os.Stdout.Write(out)
	return 0
}
