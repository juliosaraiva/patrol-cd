package envtestx

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// missingBinaries names binaries that do not exist. Explicit paths win over KUBEBUILDER_ASSETS and
// TEST_ASSET_*, which envtest reads before the BinaryAssetsDirectory field, so these tests never
// start a control plane wherever those variables are set (in CI and in `make test` they are).
func missingBinaries(t *testing.T) envtest.ControlPlane {
	t.Helper()
	missing := filepath.Join(t.TempDir(), "missing")
	return envtest.ControlPlane{
		APIServer: &envtest.APIServer{Path: missing},
		Etcd:      &envtest.Etcd{Path: missing},
	}
}

func TestStartFailureReturnsErrorAndSetsCRDWait(t *testing.T) {
	t.Parallel()
	env := &envtest.Environment{ControlPlane: missingBinaries(t), UseExistingCluster: ptr.To(false)}
	cfg, err := Start(env)
	require.Error(t, err)
	assert.Nil(t, cfg)
	assert.Equal(t, CRDWait, env.CRDInstallOptions.MaxTime)
}

func TestStartKeepsExplicitCRDWait(t *testing.T) {
	t.Parallel()
	env := &envtest.Environment{
		ControlPlane:       missingBinaries(t),
		UseExistingCluster: ptr.To(false),
		CRDInstallOptions:  envtest.CRDInstallOptions{MaxTime: CRDWait / 3},
	}
	_, err := Start(env)
	require.Error(t, err)
	assert.Equal(t, CRDWait/3, env.CRDInstallOptions.MaxTime)
}

// TestStartFailureStopsWhatStarted is the reason the package exists: the control plane starts, the
// CRDs cannot be installed (their directory is missing), and envtest returns the error with etcd and
// the API server still running.
func TestStartFailureStopsWhatStarted(t *testing.T) {
	t.Parallel()
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		if os.Getenv("PATROL_REQUIRE_ENVTEST") == "1" {
			t.Fatal("PATROL_REQUIRE_ENVTEST=1 but KUBEBUILDER_ASSETS is not set")
		}
		t.Skip("KUBEBUILDER_ASSETS not set; skipping envtest test")
	}
	env := &envtest.Environment{
		UseExistingCluster:    ptr.To(false),
		CRDDirectoryPaths:     []string{filepath.Join(t.TempDir(), "no-such-crds")},
		ErrorIfCRDPathMissing: true,
	}
	t.Cleanup(func() { _ = env.Stop() }) // a no-op when the helper did its job

	_, err := Start(env)
	require.Error(t, err)
	require.NotNil(t, env.ControlPlane.Etcd, "the control plane was not started, so the test proves nothing")
	require.NotNil(t, env.ControlPlane.Etcd.URL, "etcd was not started, so the test proves nothing")
	conn, dialErr := net.DialTimeout("tcp", env.ControlPlane.Etcd.URL.Host, time.Second)
	if dialErr == nil {
		_ = conn.Close()
	}
	assert.Error(t, dialErr, "etcd must not be left running after a failed Start")
}
