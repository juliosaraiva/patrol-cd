// Package envtestx starts envtest control planes for tests and test tools.
//
// It exists for two reasons that only show on a busy or small machine, such as a 2 vCPU CI
// runner: envtest leaves its etcd and kube-apiserver processes running when Start fails, and
// it waits only 10 seconds for installed CRDs to be served. A package whose start fails
// then slows every package that runs after it, and those fail in turn.
package envtestx

import (
	"time"

	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// CRDWait is how long Start waits for installed CRDs to be served, unless the caller set
// CRDInstallOptions.MaxTime. envtest's own default is 10 seconds.
const CRDWait = 90 * time.Second

// Start starts env like env.Start, with CRDInstallOptions.MaxTime defaulting to CRDWait. When
// the start fails it stops what was started before returning the error, so no etcd or
// kube-apiserver process is left behind. The caller stops env after a successful start.
//
// The wait for the control plane itself is not set here: envtest reads it from
// KUBEBUILDER_CONTROLPLANE_START_TIMEOUT unless env.ControlPlaneStartTimeout is set.
func Start(env *envtest.Environment) (*rest.Config, error) {
	if env.CRDInstallOptions.MaxTime == 0 {
		env.CRDInstallOptions.MaxTime = CRDWait
	}
	cfg, err := env.Start()
	if err != nil {
		_ = env.Stop()
		return nil, err
	}
	return cfg, nil
}

// ci probe (throwaway): a Go-only change that touches no workflow, Makefile or module file.
