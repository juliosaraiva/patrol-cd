package core_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	corev1 "k8s.io/api/core/v1"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/cli/clitest"
	"github.com/juliosaraiva/patrol-cd/internal/envtestx"
)

// One envtest kube-apiserver per test binary with the Patrol CRDs, the Operation admission
// policy of deploy/crds and an audit log (Metadata level), so tests can check which
// identity every request of the in-process server used.
var (
	testEnv      *envtest.Environment
	adminCfg     *rest.Config
	auditLogPath string
)

// auditPolicy records every request at Metadata level (user, impersonation, verb, object).
const auditPolicy = `apiVersion: audit.k8s.io/v1
kind: Policy
omitStages: ["RequestReceived"]
rules:
- level: Metadata
`

func TestMain(m *testing.M) { os.Exit(runTests(m)) }

func runTests(m *testing.M) int {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		if os.Getenv("PATROL_REQUIRE_ENVTEST") == "1" {
			fmt.Fprintln(os.Stderr, "PATROL_REQUIRE_ENVTEST=1 but KUBEBUILDER_ASSETS is not set")
			return 1
		}
		return m.Run()
	}
	ctrllog.SetLogger(logr.Discard())
	dir, err := os.MkdirTemp("", "patrol-core-envtest-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()
	policy := filepath.Join(dir, "audit-policy.yaml")
	if err := os.WriteFile(policy, []byte(auditPolicy), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	auditLogPath = filepath.Join(dir, "audit.log")
	env := &envtest.Environment{CRDDirectoryPaths: []string{clitest.CRDDir()}, ErrorIfCRDPathMissing: true}
	env.ControlPlane.GetAPIServer().Configure().
		Set("audit-policy-file", policy).
		Set("audit-log-path", auditLogPath)
	cfg, err := envtestx.Start(env)
	if err != nil {
		fmt.Fprintf(os.Stderr, "start envtest: %v\n", err)
		return 1
	}
	defer func() {
		if err := env.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "stop envtest: %v\n", err)
		}
	}()
	testEnv, adminCfg = env, cfg
	if err := installAdmissionPolicy(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "install the Operation admission policy: %v\n", err)
		return 1
	}
	return m.Run()
}

// installAdmissionPolicy applies deploy/crds/operations-admission-policy.yaml and waits
// until the API server enforces it.
func installAdmissionPolicy(cfg *rest.Config) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c, err := client.New(cfg, client.Options{Scheme: clitest.Scheme})
	if err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(clitest.CRDDir(), "operations-admission-policy.yaml"))
	if err != nil {
		return err
	}
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	for {
		obj := &unstructured.Unstructured{}
		if err := dec.Decode(&obj.Object); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}
		if len(obj.Object) == 0 {
			continue
		}
		if err := c.Create(ctx, obj); err != nil {
			return err
		}
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "policy-probe-"}}
	if err := c.Create(ctx, ns); err != nil {
		return err
	}
	probe := &v1alpha1.Operation{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns.Name, Name: "probe", Labels: map[string]string{v1alpha1.LabelKeyOperationApplication: "other"}},
		Spec:       v1alpha1.OperationSpec{Application: "web", Sync: &v1alpha1.SyncOperation{}},
	}
	for {
		err := c.Create(ctx, probe.DeepCopy(), client.DryRunAll)
		if err != nil && strings.Contains(err.Error(), "must equal spec.application") {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("policy not enforced; last probe result: %v", err)
		case <-time.After(200 * time.Millisecond):
		}
	}
}
