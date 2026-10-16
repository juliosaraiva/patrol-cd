// Package clitest is the end-to-end test harness of the patrol CLI: it starts the real
// patrol API server (internal/apiserver with the core routes and the project, cluster and
// repository services, real auth.TokenService, rbac.Enforcer, settings.Manager and
// core.KubeSecretStore) against a shared envtest Kubernetes API server, and runs CLI
// commands in-process through the real root command, capturing stdout, stderr and the exit
// code. Other CLI packages (app, admin) reuse it: Options.Registrars adds their command
// groups and Options.Services their API services. See docs/architecture/components/cli.md.
//
// Use it from a package's TestMain:
//
//	func TestMain(m *testing.M) { os.Exit(clitest.Main(m)) }
//
// and in tests:
//
//	e := clitest.New(t, clitest.Options{})
//	e.LoginAs("admin")
//	r := e.Run("proj", "create", "demo")
//	r.Succeeded(t)
//
// Without KUBEBUILDER_ASSETS, tests calling New are skipped (PATROL_REQUIRE_ENVTEST=1
// turns that into a failure).
package clitest

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/core"
	apirest "github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	clusterssvc "github.com/juliosaraiva/patrol-cd/internal/apiserver/services/clusters"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/projects"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/repositories"
	"github.com/juliosaraiva/patrol-cd/internal/auth"
	"github.com/juliosaraiva/patrol-cd/internal/cli"
	clusterlib "github.com/juliosaraiva/patrol-cd/internal/clusters"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	"github.com/juliosaraiva/patrol-cd/internal/repository"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// Test credentials (not real secrets).
const (
	// Password is the password of every local account of an Env.
	Password = "Patrol-Test-Pass1!"
	// ServerKey is the session signing key of every Env.
	ServerKey = "0123456789abcdef0123456789abcdef-clitest-signing-key"
)

var envCfg *rest.Config

// Main starts the shared envtest API server (with the Patrol CRDs) when KUBEBUILDER_ASSETS
// is set, runs the tests and stops it.
func Main(m *testing.M) int {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		if os.Getenv("PATROL_REQUIRE_ENVTEST") == "1" {
			fmt.Fprintln(os.Stderr, "PATROL_REQUIRE_ENVTEST=1 but KUBEBUILDER_ASSETS is not set")
			return 1
		}
		return m.Run()
	}
	ctrllog.SetLogger(logr.Discard())
	env := &envtest.Environment{CRDDirectoryPaths: []string{CRDDir()}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "start envtest: %v\n", err)
		return 1
	}
	defer func() {
		if err := env.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "stop envtest: %v\n", err)
		}
	}()
	envCfg = cfg
	return m.Run()
}

// RepoRoot returns the repository root directory.
func RepoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}

// CRDDir returns deploy/crds.
func CRDDir() string { return filepath.Join(RepoRoot(), "deploy", "crds") }

// RestConfig returns the envtest (cluster-admin) configuration, skipping the test without
// envtest.
func RestConfig(t testing.TB) *rest.Config {
	t.Helper()
	if envCfg == nil {
		t.Skip("KUBEBUILDER_ASSETS is not set: envtest end-to-end test skipped")
	}
	return rest.CopyConfig(envCfg)
}

// Scheme has the core and Patrol types.
var Scheme = func() *k8sruntime.Scheme {
	s := k8sruntime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		panic(err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		panic(err)
	}
	return s
}()

// passwordHash is computed once (bcrypt is slow).
var passwordHash = sync.OnceValue(func() string {
	h, err := auth.HashPassword(Password)
	if err != nil {
		panic(err)
	}
	return h
})

// DefaultCM are the patrol-cm entries of every Env: local accounts alice (login, apiKey),
// bob (login, apiKey), ci (apiKey) and nobody (login, no RBAC grant).
func DefaultCM() map[string]string {
	return map[string]string{
		"accounts.alice":  "login, apiKey",
		"accounts.bob":    "login, apiKey",
		"accounts.ci":     "apiKey",
		"accounts.nobody": "login",
	}
}

// DefaultRBAC is the patrol-rbac-cm of every Env: alice is read-only, bob may manage
// projects and repositories of project team-a only; admin is role:admin (built in).
func DefaultRBAC() map[string]string {
	return map[string]string{
		"policy.csv": strings.Join([]string{
			"g, alice, role:readonly",
			"p, bob, projects, get, team-a, allow",
			"p, bob, projects, update, team-a, allow",
			"p, bob, repositories, *, team-a/*, allow",
		}, "\n") + "\n",
	}
}

// Options configure New.
type Options struct {
	// CM, RBAC and Secret are merged over DefaultCM, DefaultRBAC and the default
	// patrol-secret (signing key, passwords of admin and the default accounts).
	CM, RBAC, Secret map[string]string
	// TLS serves HTTPS with a certificate for 127.0.0.1 signed by Env.CAPEM; otherwise the
	// server is plain HTTP and Run adds --plaintext through the stored context or flags.
	TLS bool
	// RootPath serves the API under a sub-path.
	RootPath string
	// Registrars add command groups to the CLI (e.g. app.Register).
	Registrars []cli.Registrar
	// Services adds API services; they are built after the core collaborators exist.
	Services func(e *Env) []apiserver.Service
	// RepoServer replaces the fake repo-server (FakeRepoServer).
	RepoServer RepoServerClient
	// DisableAuth runs the server with --disable-auth.
	DisableAuth bool
}

// Env is a running Patrol API server and a CLI runner.
type Env struct {
	T *testing.T
	// URL is the server base URL (with the root path); Addr its host:port.
	URL  string
	Addr string
	// Namespace is the control-plane namespace (unique per Env).
	Namespace  string
	RestConfig *rest.Config
	Kube       client.WithWatch
	Clientset  kubernetes.Interface
	Dynamic    dynamic.Interface
	Settings   *settings.Manager
	Tokens     *auth.TokenService
	Enforcer   *rbac.Enforcer
	RepoServer *FakeRepoServer
	Audit      *AuditLog
	// ServerLog captures the server's structured logs.
	ServerLog *SyncBuffer
	// CAPEM is the CA of the TLS server (Options.TLS).
	CAPEM []byte
	// Dir is a private directory (HOME and the CLI configuration of Run).
	Dir string
	// Options of the Env.
	Options Options

	ctx      context.Context
	cancel   context.CancelFunc
	runMu    sync.Mutex
	runEnv   map[string]string
	repoSvc  RepoServerClient
	services []apiserver.Service
}

// AuditLog collects audit entries.
type AuditLog struct {
	mu      sync.Mutex
	entries []apirest.AuditEntry
}

// Audit implements rest.Auditor.
func (a *AuditLog) Audit(_ context.Context, e apirest.AuditEntry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, e)
}

// Entries returns the recorded entries.
func (a *AuditLog) Entries() []apirest.AuditEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]apirest.AuditEntry(nil), a.entries...)
}

// SyncBuffer is a concurrency-safe bytes.Buffer.
type SyncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *SyncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

// String returns the content.
func (s *SyncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// New starts an Env (skipping the test without envtest).
func New(t *testing.T, o Options) *Env {
	t.Helper()
	cfg := RestConfig(t)
	e := &Env{T: t, RestConfig: cfg, Audit: &AuditLog{}, ServerLog: &SyncBuffer{}, Options: o, Dir: t.TempDir(), runEnv: map[string]string{}}
	e.ctx, e.cancel = context.WithCancel(context.Background())
	t.Cleanup(e.cancel)
	var err error
	e.Kube, err = client.NewWithWatch(cfg, client.Options{Scheme: Scheme})
	e.must(err)
	e.Clientset, err = kubernetes.NewForConfig(cfg)
	e.must(err)
	e.Dynamic, err = dynamic.NewForConfig(cfg)
	e.must(err)
	ns, err := e.Clientset.CoreV1().Namespaces().Create(e.ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "patrol-cli-"}}, metav1.CreateOptions{})
	e.must(err)
	e.Namespace = ns.Name

	logger := slog.New(slog.NewJSONHandler(e.ServerLog, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ln, err := (&net.ListenConfig{}).Listen(e.ctx, "tcp", "127.0.0.1:0")
	e.must(err)
	e.Addr = ln.Addr().String()
	scheme := "http"
	if o.TLS {
		scheme = "https"
	}
	e.URL = scheme + "://" + e.Addr + strings.TrimRight("/"+strings.Trim(o.RootPath, "/"), "/")

	cm := DefaultCM()
	cm["url"] = e.URL
	cm["users.session.duration"] = "1h"
	maps.Copy(cm, o.CM)
	rbacCM := DefaultRBAC()
	maps.Copy(rbacCM, o.RBAC)
	sec := map[string]string{
		auth.KeyServerSecretKey: ServerKey,
		auth.KeyAdminPassword:   passwordHash(),
	}
	for _, u := range []string{"alice", "bob", "nobody"} {
		sec["accounts."+u+".password"] = passwordHash()
	}
	var serving *tls.Certificate
	if o.TLS {
		ca, crt, key := selfSignedTLS(t)
		e.CAPEM = ca
		pair, err := tls.X509KeyPair(crt, key)
		e.must(err)
		serving = &pair
	}
	maps.Copy(sec, o.Secret)
	e.createSettingsObjects(cm, rbacCM, sec)

	e.Settings, err = settings.NewManager(e.Clientset, settings.ManagerOptions{
		Namespace: e.Namespace, Debounce: 20 * time.Millisecond, IgnorePartOfLabel: true, Logger: logger,
	})
	e.must(err)
	e.must(e.Settings.Start(e.ctx))
	e.WaitFor("settings loaded", func() bool {
		cur := e.Settings.Current()
		if cur == nil {
			return false
		}
		_, err := cur.SigningKeys()
		return err == nil
	})
	ks, err := e.Settings.Current().SigningKeys()
	e.must(err)
	e.Tokens, err = auth.NewTokenService(auth.TokenServiceConfig{
		Keys: ks, Accounts: apiserver.AccountLookup(e.Settings),
		Revocations: auth.NewMemoryRevocationStore(auth.RevocationLimits{}, nil),
		Projects: apiserver.ProjectTokenRegistry(func(ctx context.Context, name string) (*v1alpha1.AppProject, error) {
			p := &v1alpha1.AppProject{}
			return p, e.Kube.Get(ctx, client.ObjectKey{Namespace: e.Namespace, Name: name}, p)
		}),
	})
	e.must(err)
	lc := auth.DefaultLoginConfig()
	lc.DelayMin, lc.DelayMax = 0, 0
	login, err := auth.NewAuthenticator(apiserver.AccountLookup(e.Settings), lc, auth.WithLoginLogger(logger))
	e.must(err)
	e.Enforcer = rbac.NewEnforcer(rbac.WithLogger(logger))
	e.followProjects()

	e.repoSvc = o.RepoServer
	if e.repoSvc == nil {
		e.RepoServer = &FakeRepoServer{}
		e.repoSvc = e.RepoServer
	}
	e.services = e.defaultServices(logger)
	if o.Services != nil {
		e.services = append(e.services, o.Services(e)...)
	}
	sso, err := core.NewSSO(core.SSOConfig{Logger: logger})
	e.must(err)
	srv, err := apiserver.New(apiserver.Config{
		Namespace: e.Namespace, Insecure: !o.TLS, MetricsAddress: "-", RootPath: o.RootPath, DisableAuth: o.DisableAuth,
		ShutdownTimeout: 5 * time.Second, TLS: apiserver.TLSOptions{Hosts: []string{"127.0.0.1", "localhost"}, Certificate: func() *tls.Certificate { return serving }},
	}, apiserver.Deps{
		Settings: e.Settings, Tokens: e.Tokens, Login: login, Enforcer: e.Enforcer,
		Core: core.Deps{
			Secrets:    &core.KubeSecretStore{Client: e.Clientset, Namespace: e.Namespace},
			SSO:        sso,
			RepoServer: e.repoSvc,
			Version:    &core.VersionInfo{Version: "v9.9.9-clitest", GitCommit: "0123456789abcdef"},
		},
		Services: e.services, Auditor: e.Audit, Registry: prometheus.NewRegistry(), Logger: logger,
	})
	e.must(err)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := srv.Serve(e.ctx, ln, nil); err != nil {
			t.Logf("api server stopped: %v", err)
		}
	}()
	t.Cleanup(func() {
		e.cancel()
		<-done
	})
	e.waitReady()
	return e
}

func (e *Env) must(err error) {
	e.T.Helper()
	if err != nil {
		e.T.Fatal(err)
	}
}

// Context returns the Env's context (canceled at cleanup).
func (e *Env) Context() context.Context { return e.ctx }

func (e *Env) defaultServices(logger *slog.Logger) []apiserver.Service {
	lister, err := repository.NewLister(e.Dynamic, e.Clientset, repository.ListerOptions{Namespace: e.Namespace, ResyncPeriod: time.Minute})
	e.must(err)
	e.must(lister.Start(e.ctx))
	resolver, err := repository.NewResolver(lister, repository.Options{RepoServer: e.repoSvc, Registerer: prometheus.NewRegistry(), Logger: logger})
	e.must(err)
	return []apiserver.Service{
		projects.New(projects.Deps{
			Config: projects.Config{Namespace: e.Namespace}, Client: e.Kube, Settings: e.Settings, Tokens: e.Tokens,
			Events: e.Clientset.CoreV1().Events(e.Namespace), Policies: e.Enforcer, Logger: logger,
		}),
		clusterssvc.New(clusterssvc.Deps{
			Config:    clusterssvc.Config{Namespace: e.Namespace, Bootstrap: &clusterlib.BootstrapOptions{TokenTimeout: 20 * time.Second, PollInterval: 50 * time.Millisecond}},
			Client:    e.Kube,
			Settings:  e.Settings,
			Connector: clusterssvc.NewConnector(clusterssvc.ConnectorOptions{Timeout: 10 * time.Second}),
			Status:    clusterlib.NewStatusWriter(e.Dynamic),
			Logger:    logger,
		}),
		repositories.New(repositories.Deps{
			Config: repositories.Config{Namespace: e.Namespace, ConnectionTimeout: 10 * time.Second}, Client: e.Kube,
			Resolver: resolver, StatusWriter: repository.NewStatusWriter(e.Dynamic), RepoServer: e.repoSvc,
			Settings: e.Settings, Logger: logger,
		}),
	}
}

func (e *Env) createSettingsObjects(cm, rbacCM, sec map[string]string) {
	ctx := e.ctx
	cms := e.Clientset.CoreV1().ConfigMaps(e.Namespace)
	_, err := cms.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: settings.DefaultConfigMapName}, Data: cm}, metav1.CreateOptions{})
	e.must(err)
	_, err = cms.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: settings.DefaultRBACConfigMapName}, Data: rbacCM}, metav1.CreateOptions{})
	e.must(err)
	data := map[string][]byte{}
	for k, v := range sec {
		data[k] = []byte(v)
	}
	_, err = e.Clientset.CoreV1().Secrets(e.Namespace).Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: settings.DefaultSecretName}, Data: data}, metav1.CreateOptions{})
	e.must(err)
}

// followProjects keeps the enforcer's project role policies in sync with the AppProjects
// of the namespace (the server does this from an informer in production).
func (e *Env) followProjects() {
	w, err := e.Kube.Watch(e.ctx, &v1alpha1.AppProjectList{}, client.InNamespace(e.Namespace))
	e.must(err)
	go func() {
		defer w.Stop()
		for {
			select {
			case <-e.ctx.Done():
				return
			case ev, ok := <-w.ResultChan():
				if !ok {
					return
				}
				p, ok := ev.Object.(*v1alpha1.AppProject)
				if !ok {
					continue
				}
				if ev.Type == watch.Deleted {
					e.Enforcer.DeleteProjectPolicies(p.Name)
					continue
				}
				roles := make([]rbac.Role, 0, len(p.Spec.Roles))
				for _, r := range p.Spec.Roles {
					roles = append(roles, rbac.Role{Name: r.Name, Policies: r.Policies, Groups: r.Groups})
				}
				_ = e.Enforcer.SetProjectPolicies(p.Name, roles)
			}
		}
	}()
}

func (e *Env) waitReady() {
	e.T.Helper()
	hc := &http.Client{Timeout: 2 * time.Second}
	if e.Options.TLS {
		c, err := HTTPClientWithCA(e.CAPEM)
		e.must(err)
		hc = c
	}
	base := strings.TrimSuffix(e.URL, strings.TrimRight("/"+strings.Trim(e.Options.RootPath, "/"), "/"))
	e.WaitFor("api server ready", func() bool {
		resp, err := hc.Get(base + "/healthz")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
}

// WaitFor polls cond every 25 ms for up to 20 s.
func (e *Env) WaitFor(what string, cond func() bool) {
	e.T.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	e.T.Fatalf("timed out waiting for %s", what)
}

// UpdateConfigMap changes patrol-cm or patrol-rbac-cm and waits until the settings
// manager published the change (check reports it).
func (e *Env) UpdateConfigMap(name string, mutate func(map[string]string), check func(*settings.Settings) bool) {
	e.T.Helper()
	cms := e.Clientset.CoreV1().ConfigMaps(e.Namespace)
	cm, err := cms.Get(e.ctx, name, metav1.GetOptions{})
	e.must(err)
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	mutate(cm.Data)
	_, err = cms.Update(e.ctx, cm, metav1.UpdateOptions{})
	e.must(err)
	if check != nil {
		e.WaitFor("settings change of "+name, func() bool { return check(e.Settings.Current()) })
	}
}

// SetPolicy replaces policy.csv and waits until subject's can-i answer for the probe
// (resource, action, object) is want.
func (e *Env) SetPolicy(policy string, probe func() bool) {
	e.T.Helper()
	e.UpdateConfigMap(settings.DefaultRBACConfigMapName, func(d map[string]string) { d["policy.csv"] = policy }, func(s *settings.Settings) bool {
		return s.RBAC().Data["policy.csv"] == policy
	})
	if probe != nil {
		e.WaitFor("RBAC policy applied", probe)
	}
}

// LoginToken issues a login session for a local account (bypassing the login route).
func (e *Env) LoginToken(user string) string {
	e.T.Helper()
	acct, err := e.Settings.Current().Accounts().Get(user)
	e.must(err)
	iss, err := e.Tokens.IssueLoginSession(e.ctx, acct, time.Hour)
	e.must(err)
	return iss.Token
}

// APIClient returns a pkg/apiclient client for the server using token.
func (e *Env) APIClient(token string) *apiclient.Client {
	e.T.Helper()
	opts := &apiclient.ClientOptions{ServerAddr: e.URL, AuthToken: token, Timeout: time.Minute}
	if e.Options.TLS {
		hc, err := HTTPClientWithCA(e.CAPEM)
		e.must(err)
		opts.HTTPClient = hc
	}
	c, err := apiclient.NewClient(opts)
	e.must(err)
	return c
}

// CreateProject creates an AppProject directly in Kubernetes.
func (e *Env) CreateProject(p *v1alpha1.AppProject) *v1alpha1.AppProject {
	e.T.Helper()
	p = p.DeepCopy()
	p.Namespace = e.Namespace
	e.must(e.Kube.Create(e.ctx, p))
	return p
}

// GetProject reads an AppProject from Kubernetes.
func (e *Env) GetProject(name string) *v1alpha1.AppProject {
	e.T.Helper()
	p := &v1alpha1.AppProject{}
	e.must(e.Kube.Get(e.ctx, client.ObjectKey{Namespace: e.Namespace, Name: name}, p))
	return p
}

// ProjectExists reports whether the AppProject exists.
func (e *Env) ProjectExists(name string) bool {
	err := e.Kube.Get(e.ctx, client.ObjectKey{Namespace: e.Namespace, Name: name}, &v1alpha1.AppProject{})
	return err == nil || !apierrors.IsNotFound(err)
}

// Repositories lists the Repository objects of the namespace.
func (e *Env) Repositories() []v1alpha1.Repository {
	e.T.Helper()
	var l v1alpha1.RepositoryList
	e.must(e.Kube.List(e.ctx, &l, client.InNamespace(e.Namespace)))
	return l.Items
}

// Clusters lists the Cluster objects of the namespace.
func (e *Env) Clusters() []v1alpha1.Cluster {
	e.T.Helper()
	var l v1alpha1.ClusterList
	e.must(e.Kube.List(e.ctx, &l, client.InNamespace(e.Namespace)))
	return l.Items
}

// Secret returns a Secret of the namespace (nil when missing).
func (e *Env) Secret(name string) *corev1.Secret {
	s, err := e.Clientset.CoreV1().Secrets(e.Namespace).Get(e.ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil
	}
	return s
}

// GVR of Patrol kinds (dynamic client).
func GVR(resource string) schema.GroupVersionResource {
	return v1alpha1.GroupVersion.WithResource(resource)
}
