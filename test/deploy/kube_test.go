package deploy_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// auditPolicy logs every request at Metadata level (no bodies), except Operation creations,
// logged with their request body so the harness can tell whether the "impersonate" verb was
// needed (spec.initiatedBy set to another identity). Secret bodies are never logged.
const auditPolicy = `apiVersion: audit.k8s.io/v1
kind: Policy
omitStages: [RequestReceived]
rules:
  - level: Request
    verbs: [create]
    resources:
      - group: cd.patrol.example
        resources: [operations]
  - level: Metadata
`

// kubeEnv is one envtest control plane (etcd + kube-apiserver with RBAC, the default
// admission plugins but ServiceAccount, and optionally an audit log).
type kubeEnv struct {
	name string
	env  *envtest.Environment
	// cfg is the admin configuration (system:masters).
	cfg    *rest.Config
	kube   kubernetes.Interface
	dyn    dynamic.Interface
	mapper *restmapper.DeferredDiscoveryRESTMapper
	// kubeconfig is an admin kubeconfig file (for helm and kubectl-like clients).
	kubeconfig string
	// auditLog is the audit log file ("" without audit).
	auditLog string
	dir      string
}

type envOptions struct {
	name string
	// audit enables the JSON audit log (auditPolicy).
	audit bool
	// crds installs deploy/crds and the Operation admission policy.
	crds bool
}

// startKubeEnv starts an envtest control plane.
func startKubeEnv(assets string, o envOptions) (*kubeEnv, error) {
	dir, err := os.MkdirTemp("", "patrol-deploy-"+o.name+"-")
	if err != nil {
		return nil, err
	}
	k := &kubeEnv{name: o.name, dir: dir}
	apiLog, err := os.Create(filepath.Join(dir, "kube-apiserver.log"))
	if err != nil {
		return nil, err
	}
	api := &envtest.APIServer{Out: apiLog, Err: apiLog}
	if o.audit {
		policy := filepath.Join(dir, "audit-policy.yaml")
		if err := os.WriteFile(policy, []byte(auditPolicy), 0o600); err != nil {
			return nil, err
		}
		k.auditLog = filepath.Join(dir, "audit.log")
		api.Configure().
			Set("audit-policy-file", policy).
			Set("audit-log-path", k.auditLog).
			Set("audit-log-format", "json").
			Set("audit-log-mode", "blocking").
			Set("audit-log-maxsize", "0")
	}
	k.env = &envtest.Environment{
		BinaryAssetsDirectory:    assets,
		ControlPlaneStartTimeout: 2 * time.Minute,
		ControlPlaneStopTimeout:  time.Minute,
	}
	k.env.ControlPlane.APIServer = api
	if o.crds {
		k.env.CRDDirectoryPaths = []string{crdDir()}
		k.env.ErrorIfCRDPathMissing = true
	}
	cfg, err := k.env.Start()
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("start envtest %s: %w", o.name, err)
	}
	cfg = rest.CopyConfig(cfg)
	cfg.QPS, cfg.Burst = 200, 400
	cfg.WarningHandler = rest.NoWarnings{}
	k.cfg = cfg
	if k.kube, err = kubernetes.NewForConfig(cfg); err != nil {
		return nil, errors.Join(err, k.stop())
	}
	if k.dyn, err = dynamic.NewForConfig(cfg); err != nil {
		return nil, errors.Join(err, k.stop())
	}
	k.mapper = restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(k.kube.Discovery()))
	k.kubeconfig = filepath.Join(dir, "admin.kubeconfig")
	if err := os.WriteFile(k.kubeconfig, k.env.KubeConfig, 0o600); err != nil {
		return nil, errors.Join(err, k.stop())
	}
	if o.crds {
		objs, err := readObjects(filepath.Join(crdDir(), "operations-admission-policy.yaml"))
		if err != nil {
			return nil, errors.Join(err, k.stop())
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, obj := range objs {
			if err := k.apply(ctx, k.kube.Discovery().RESTClient(), obj, "", applyOptions{fieldManager: "deploy-test", force: true}); err != nil {
				return nil, errors.Join(fmt.Errorf("install the admission policy: %w", err), k.stop())
			}
		}
	}
	return k, nil
}

func (k *kubeEnv) stop() error {
	if k == nil || k.env == nil {
		return nil
	}
	err := k.env.Stop()
	if os.Getenv(envKeepLogs) == "" {
		err = errors.Join(err, os.RemoveAll(k.dir))
	}
	return err
}

// readObjects parses a YAML file into objects.
func readObjects(path string) ([]object, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	objs, err := parseObjects(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return objs, nil
}

var shared struct {
	once sync.Once
	env  *kubeEnv
	err  error
}

// sharedEnv returns the envtest control plane shared by the API validation, Pod Security and
// RBAC tests: deploy/crds and the admission policy installed, audit log enabled. Every test
// works in its own namespaces. It is stopped by TestMain.
func sharedEnv(t testing.TB) *kubeEnv {
	t.Helper()
	assets := envtestAssets(t)
	shared.once.Do(func() {
		shared.env, shared.err = startKubeEnv(assets, envOptions{name: "shared", audit: true, crds: true})
	})
	if shared.err != nil {
		t.Fatalf("shared envtest: %v", shared.err)
	}
	return shared.env
}

// freshEnv starts a dedicated control plane for one test (stopped at cleanup).
func freshEnv(t testing.TB, o envOptions) *kubeEnv {
	t.Helper()
	assets := envtestAssets(t)
	k, err := startKubeEnv(assets, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := k.stop(); err != nil {
			t.Errorf("stop envtest %s: %v", o.name, err)
		}
	})
	return k
}

// warningCollector records the warning headers of API responses.
type warningCollector struct {
	mu   sync.Mutex
	list []string
}

func (w *warningCollector) HandleWarningHeader(_ int, _ string, text string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.list = append(w.list, text)
}

// take returns and clears the recorded warnings.
func (w *warningCollector) take() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := w.list
	w.list = nil
	return out
}

// warningClient returns admin clients whose response warnings go to a new collector. Use one
// per goroutine so warnings can be attributed to the request just made.
func (k *kubeEnv) warningClient(t testing.TB) (kubernetes.Interface, *warningCollector) {
	t.Helper()
	w := &warningCollector{}
	cfg := rest.CopyConfig(k.cfg)
	cfg.WarningHandler = w
	c, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c, w
}

// mapping resolves an object's resource, refreshing discovery once on a miss (CRDs installed
// meanwhile).
func (k *kubeEnv) mapping(gvk schema.GroupVersionKind) (*meta.RESTMapping, error) {
	m, err := k.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil && meta.IsNoMatchError(err) {
		k.mapper.Reset()
		m, err = k.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	}
	return m, err
}

// resourcePath is the REST path of an object (or of its collection when name is empty).
func resourcePath(m *meta.RESTMapping, namespace, name string) string {
	gv := m.Resource.GroupVersion()
	p := "/apis/" + gv.Group + "/" + gv.Version
	if gv.Group == "" {
		p = "/api/" + gv.Version
	}
	if m.Scope.Name() == meta.RESTScopeNameNamespace {
		p += "/namespaces/" + namespace
	}
	p += "/" + m.Resource.Resource
	if name != "" {
		p += "/" + name
	}
	return p
}

type applyOptions struct {
	fieldManager string
	dryRun       bool
	force        bool
	// strict sets fieldValidation=Strict: unknown and duplicate fields are errors.
	strict bool
}

// apply server-side applies the object's raw YAML document (duplicate keys reach the server).
// namespace is used for namespaced objects without one.
func (k *kubeEnv) apply(ctx context.Context, c rest.Interface, o object, namespace string, opts applyOptions) error {
	gvk := o.u.GroupVersionKind()
	m, err := k.mapping(gvk)
	if err != nil {
		return fmt.Errorf("%s: %w", o.id(), err)
	}
	ns := o.u.GetNamespace()
	if ns == "" {
		ns = namespace
	}
	req := c.Patch(types.ApplyYAMLPatchType).AbsPath(resourcePath(m, ns, o.u.GetName())).
		Param("fieldManager", opts.fieldManager).Body(o.raw)
	if opts.force {
		req = req.Param("force", "true")
	}
	if opts.dryRun {
		req = req.Param("dryRun", metav1.DryRunAll)
	}
	if opts.strict {
		req = req.Param("fieldValidation", metav1.FieldValidationStrict)
	}
	if err := req.Do(ctx).Error(); err != nil {
		return fmt.Errorf("apply %s: %w", o.id(), err)
	}
	return nil
}

// ensureNamespace creates a namespace (labels merged into an existing one).
func (k *kubeEnv) ensureNamespace(ctx context.Context, name string, labels map[string]string) error {
	ns, err := k.kube.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		_, err = k.kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}, metav1.CreateOptions{})
		return err
	case err != nil:
		return err
	}
	if len(labels) == 0 {
		return nil
	}
	if ns.Labels == nil {
		ns.Labels = map[string]string{}
	}
	for key, v := range labels {
		ns.Labels[key] = v
	}
	_, err = k.kube.CoreV1().Namespaces().Update(ctx, ns, metav1.UpdateOptions{})
	return err
}

// serviceAccountKubeconfig writes a kubeconfig authenticating as namespace/sa with a token
// from the TokenRequest API (the kubeconfig context namespace is namespace, like the pod's
// service account namespace).
func (k *kubeEnv) serviceAccountKubeconfig(ctx context.Context, dir, namespace, sa string) (string, *rest.Config, error) {
	tr, err := k.kube.CoreV1().ServiceAccounts(namespace).CreateToken(ctx, sa, &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: ptr.To(int64(6 * 3600))},
	}, metav1.CreateOptions{})
	if err != nil {
		return "", nil, fmt.Errorf("token for %s/%s: %w", namespace, sa, err)
	}
	cfg := &rest.Config{Host: k.cfg.Host, BearerToken: tr.Status.Token, TLSClientConfig: rest.TLSClientConfig{CAData: k.cfg.CAData}}
	kc := clientcmdapi.NewConfig()
	kc.Clusters["envtest"] = &clientcmdapi.Cluster{Server: k.cfg.Host, CertificateAuthorityData: k.cfg.CAData}
	kc.AuthInfos[sa] = &clientcmdapi.AuthInfo{Token: tr.Status.Token}
	kc.Contexts["sa"] = &clientcmdapi.Context{Cluster: "envtest", AuthInfo: sa, Namespace: namespace}
	kc.CurrentContext = "sa"
	path := filepath.Join(dir, sa+".kubeconfig")
	if err := clientcmd.WriteToFile(*kc, path); err != nil {
		return "", nil, err
	}
	return path, cfg, nil
}

// isNamespaced reports whether the object's kind is namespaced in this API server.
func (k *kubeEnv) isNamespaced(o object) (bool, error) {
	m, err := k.mapping(o.u.GroupVersionKind())
	if err != nil {
		return false, err
	}
	return m.Scope.Name() == meta.RESTScopeNameNamespace, nil
}

// joinLines formats a list for failure messages.
func joinLines(items []string) string {
	if len(items) == 0 {
		return "(none)"
	}
	return "  " + strings.Join(items, "\n  ")
}

// deployScheme knows the built-in kinds and the Patrol CD kinds.
var deployScheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, v1alpha1.AddToScheme} {
		if err := add(s); err != nil {
			panic(fmt.Sprintf("build scheme: %v", err)) // static registration; cannot fail at run time
		}
	}
	return s
}()
