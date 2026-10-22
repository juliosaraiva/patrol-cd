package stack

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/clusters"
	"github.com/juliosaraiva/patrol-cd/internal/envtestx"
	"github.com/juliosaraiva/patrol-cd/internal/server"
)

// Environment variables of the harness.
const (
	// EnvIntegration enables the integration tests ("1").
	EnvIntegration = "PATROL_INTEGRATION"
	// EnvAssets is envtest's binary directory (kube-apiserver, etcd).
	EnvAssets = "KUBEBUILDER_ASSETS"
	// EnvTimeoutScale multiplies every wait bound (float, default 1; 2 when CI is set).
	EnvTimeoutScale = "PATROL_INTEGRATION_TIMEOUT_SCALE"
	// EnvLogDir receives the complete component logs of every Stack (<test>.log).
	EnvLogDir = "PATROL_INTEGRATION_LOG_DIR"
	// envtestVersion is the envtest Kubernetes version installed by `make` (hack/tools).
	envtestVersion = "1.36.0"
	// stopGrace bounds the graceful stop of a dedicated destination's kube-apiserver.
	stopGrace = 5 * time.Second
)

// Main runs the tests of a package that uses Stacks. Call it as the only statement of
// TestMain: os.Exit(stack.Main(m)). It dispatches the hidden helper modes first (the
// repo-server's sandbox helper and git askpass re-execute the test binary), then runs the
// tests and stops the shared clusters.
func Main(m *testing.M) int {
	server.DispatchHelpers()
	ctrllog.SetLogger(logr.Discard())
	if !Enabled() {
		return m.Run()
	}
	code := m.Run()
	if err := stopShared(); err != nil {
		fmt.Fprintf(os.Stderr, "stop shared clusters: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

// Enabled reports whether integration tests run (PATROL_INTEGRATION=1 or KUBEBUILDER_ASSETS
// set).
func Enabled() bool {
	return os.Getenv(EnvIntegration) == "1" || os.Getenv(EnvAssets) != ""
}

// RepoRoot is the repository root (from this file's location).
func RepoRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "."
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}

// assetsDir returns envtest's binary directory: KUBEBUILDER_ASSETS, else the repository's
// bin/envtest/k8s/<version>-<os>-<arch>.
func assetsDir() (string, error) {
	if d := os.Getenv(EnvAssets); d != "" {
		return d, nil
	}
	d := filepath.Join(RepoRoot(), "bin", "envtest", "k8s", fmt.Sprintf("%s-%s-%s", envtestVersion, runtime.GOOS, runtime.GOARCH))
	if _, err := os.Stat(filepath.Join(d, "kube-apiserver")); err != nil {
		return "", fmt.Errorf("%s=1 but %s is not set and %s has no kube-apiserver (run `make envtest` or set %s): %w",
			EnvIntegration, EnvAssets, d, EnvAssets, err)
	}
	return d, nil
}

// Timeout scales d by PATROL_INTEGRATION_TIMEOUT_SCALE (default 1, 2 when CI is set).
func Timeout(d time.Duration) time.Duration {
	scale := 1.0
	if os.Getenv("CI") != "" {
		scale = 2
	}
	if v := os.Getenv(EnvTimeoutScale); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			scale = f
		}
	}
	return time.Duration(float64(d) * scale)
}

// shared are the clusters every Stack of the test binary uses.
type sharedEnv struct {
	control *Cluster
	dest    *Cluster
	tmp     string
}

var (
	sharedOnce sync.Once
	sharedVal  *sharedEnv
	sharedErr  error
)

// shared starts the control cluster and the shared destination once per test binary.
func shared() (*sharedEnv, error) {
	sharedOnce.Do(func() {
		sharedVal, sharedErr = startShared()
	})
	return sharedVal, sharedErr
}

func startShared() (*sharedEnv, error) {
	assets, err := assetsDir()
	if err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp("", "patrol-integration-")
	if err != nil {
		return nil, err
	}
	s := &sharedEnv{tmp: tmp}
	control, err := startEnvtest(assets, envtestOptions{crds: true, name: "in-cluster"})
	if err != nil {
		_ = os.RemoveAll(tmp)
		return nil, fmt.Errorf("start the control cluster: %w", err)
	}
	s.control = control
	if err := installAdmissionPolicy(context.Background(), control); err != nil {
		_ = stopSharedEnv(s)
		return nil, err
	}
	// The control cluster is also the destination "in-cluster": the components serve
	// https://kubernetes.default.svc from the local kubeconfig (clusters.FakeInCluster).
	kc := filepath.Join(tmp, "control.kubeconfig")
	if err := os.WriteFile(kc, control.env.KubeConfig, 0o600); err != nil {
		_ = stopSharedEnv(s)
		return nil, err
	}
	if err := os.Setenv("KUBECONFIG", kc); err != nil {
		_ = stopSharedEnv(s)
		return nil, err
	}
	if err := os.Setenv(clusters.EnvFakeInCluster, "true"); err != nil {
		_ = stopSharedEnv(s)
		return nil, err
	}
	control.Server = v1alpha1.KubernetesInternalAPIServerAddr
	control.Auth = AuthInCluster

	dest, err := startEnvtest(assets, envtestOptions{name: "dest-1", auth: AuthClientCert})
	if err != nil {
		_ = stopSharedEnv(s)
		return nil, fmt.Errorf("start the shared destination cluster: %w", err)
	}
	s.dest = dest
	return s, nil
}

func stopShared() error {
	if sharedVal == nil {
		return nil
	}
	return stopSharedEnv(sharedVal)
}

func stopSharedEnv(s *sharedEnv) error {
	var errs []error
	for _, c := range []*Cluster{s.dest, s.control} {
		if c != nil {
			errs = append(errs, c.stopEnv())
		}
	}
	errs = append(errs, os.RemoveAll(s.tmp))
	return errors.Join(errs...)
}

// AuthMode is how Patrol authenticates to a destination cluster.
type AuthMode string

const (
	// AuthInCluster is the synthetic in-cluster destination (the control cluster).
	AuthInCluster AuthMode = "in-cluster"
	// AuthClientCert uses a client certificate issued by envtest (AddUser, system:masters).
	AuthClientCert AuthMode = "client-certificate"
	// AuthBearerToken uses a ServiceAccount token (TokenRequest) bound to cluster-admin.
	AuthBearerToken AuthMode = "bearer-token"
)

type envtestOptions struct {
	name string
	crds bool
	auth AuthMode
	// certDir, when set, keeps the API server's serving certificates and service account keys
	// in this directory so the API server can be restarted with the same identity. The API
	// server's output is written to certDir/kube-apiserver.log.
	certDir string
}

// startEnvtest starts one envtest control plane and prepares the credentials Patrol uses.
func startEnvtest(assets string, o envtestOptions) (*Cluster, error) {
	env := &envtest.Environment{
		BinaryAssetsDirectory:    assets,
		ControlPlaneStartTimeout: Timeout(60 * time.Second),
		ControlPlaneStopTimeout:  Timeout(30 * time.Second),
	}
	if o.crds {
		env.CRDDirectoryPaths = []string{filepath.Join(RepoRoot(), "deploy", "crds")}
		env.ErrorIfCRDPathMissing = true
	}
	if o.certDir != "" {
		logf, err := os.OpenFile(filepath.Join(o.certDir, "kube-apiserver.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return nil, err
		}
		env.ControlPlane.APIServer = &envtest.APIServer{CertDir: o.certDir, Out: logf, Err: logf}
		// A kube-apiserver with open client connections (the controller's watches) waits up to
		// a minute for them to drain on SIGTERM; Cluster.Stop simulates an outage, so it is
		// killed after a short grace period instead.
		env.ControlPlaneStopTimeout = stopGrace
	}
	cfg, err := envtestx.Start(env)
	if err != nil {
		return nil, err
	}
	c, err := newCluster(o.name, cfg)
	if err != nil {
		_ = env.Stop()
		return nil, err
	}
	c.env = env
	if o.certDir != "" {
		c.apiserverLog = filepath.Join(o.certDir, "kube-apiserver.log")
	}
	c.Server = strings.TrimSuffix(cfg.Host, "/")
	c.Auth = o.auth
	ctx, cancel := context.WithTimeout(context.Background(), Timeout(time.Minute))
	defer cancel()
	switch o.auth {
	case AuthClientCert:
		user, err := env.AddUser(envtest.User{Name: "patrol-" + o.name, Groups: []string{"system:masters"}}, nil)
		if err != nil {
			_ = env.Stop()
			return nil, fmt.Errorf("add the patrol user: %w", err)
		}
		uc := user.Config()
		c.credentials = map[string][]byte{
			v1alpha1.ClusterSecretKeyCAData:   uc.CAData,
			v1alpha1.ClusterSecretKeyCertData: uc.CertData,
			v1alpha1.ClusterSecretKeyKeyData:  uc.KeyData,
		}
	case AuthBearerToken:
		token, err := serviceAccountToken(ctx, c.Kube)
		if err != nil {
			_ = env.Stop()
			return nil, err
		}
		c.credentials = map[string][]byte{
			v1alpha1.ClusterSecretKeyCAData:      cfg.CAData,
			v1alpha1.ClusterSecretKeyBearerToken: []byte(token),
		}
	}
	return c, nil
}

// serviceAccountToken creates kube-system/patrol-manager bound to cluster-admin and returns a
// token for it (TokenRequest API; envtest has no token controller for legacy Secrets).
func serviceAccountToken(ctx context.Context, kube kubernetes.Interface) (string, error) {
	const ns, name = "kube-system", "patrol-manager"
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	if _, err := kube.CoreV1().ServiceAccounts(ns).Create(ctx, sa, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return "", fmt.Errorf("create service account: %w", err)
	}
	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-admin"},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: name, Namespace: ns}},
	}
	if _, err := kube.RbacV1().ClusterRoleBindings().Create(ctx, crb, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return "", fmt.Errorf("create cluster role binding: %w", err)
	}
	tr, err := kube.CoreV1().ServiceAccounts(ns).CreateToken(ctx, name, &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: ptr.To(int64(24 * 3600))},
	}, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("request a service account token: %w", err)
	}
	return tr.Status.Token, nil
}

// installAdmissionPolicy applies deploy/crds/operations-admission-policy.yaml (envtest only
// installs the CRDs of the directory).
func installAdmissionPolicy(ctx context.Context, c *Cluster) error {
	path := filepath.Join(RepoRoot(), "deploy", "crds", "operations-admission-policy.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read the admission policy: %w", err)
	}
	objs, err := decodeYAML(string(data))
	if err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	for _, o := range objs {
		if _, err := c.apply(ctx, o); err != nil {
			return err
		}
	}
	return nil
}

// decodeYAML splits a multi-document YAML or JSON stream into objects (empty documents
// skipped).
func decodeYAML(s string) ([]*unstructured.Unstructured, error) {
	dec := yaml.NewYAMLOrJSONDecoder(strings.NewReader(s), 4096)
	var out []*unstructured.Unstructured
	for {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			return nil, err
		}
		if len(m) == 0 {
			continue
		}
		out = append(out, &unstructured.Unstructured{Object: m})
	}
}

// newCluster builds the clients of a cluster from an admin configuration.
func newCluster(name string, cfg *rest.Config) (*Cluster, error) {
	cfg = rest.CopyConfig(cfg)
	cfg.QPS, cfg.Burst = 200, 400
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	c, err := client.New(cfg, client.Options{Scheme: Scheme})
	if err != nil {
		return nil, err
	}
	return &Cluster{Name: name, Config: cfg, Kube: kube, Dynamic: dyn, Client: c}, nil
}
