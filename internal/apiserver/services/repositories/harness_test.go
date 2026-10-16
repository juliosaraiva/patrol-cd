package repositories_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	kuberest "k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/repositories"
	"github.com/juliosaraiva/patrol-cd/internal/auth"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	reposerver "github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/internal/repository"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// testCfg is the shared envtest API server with the Patrol CRDs, started only when
// KUBEBUILDER_ASSETS is set (never against an existing cluster).
var testCfg *kuberest.Config

func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		return m.Run()
	}
	ctrllog.SetLogger(logr.Discard())
	crds := filepath.Join("..", "..", "..", "..", "deploy", "crds")
	var paths []string
	for _, k := range []string{"applications", "appprojects", "repositories", "repositorycredentials", "scmproviders", "clusters"} {
		paths = append(paths, filepath.Join(crds, "cd.patrol.example_"+k+".yaml"))
	}
	env := &envtest.Environment{
		UseExistingCluster: ptr.To(false),
		CRDInstallOptions:  envtest.CRDInstallOptions{Paths: paths, ErrorIfPathMissing: true},
	}
	cfg, err := env.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "start envtest control plane: %v\n", err)
		return 1
	}
	defer func() {
		if err := env.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "stop envtest control plane: %v\n", err)
		}
	}()
	testCfg = cfg
	return m.Run()
}

func envConfig(t *testing.T) *kuberest.Config {
	t.Helper()
	if testCfg == nil {
		t.Skip("KUBEBUILDER_ASSETS not set; skipping envtest integration test")
	}
	return kuberest.CopyConfig(testCfg)
}

// Test identities. Policies (testPolicy) give:
//   - admin: everything of this API (role:repo-admin);
//   - reader: built-in role:readonly (get on repositories, certificates, gpgkeys, ...);
//   - team-a / team-b: repositories and applications of their project only;
//   - scm: scmproviders get on github-* only;
//   - repo-only: repositories but not write-repositories, nor scmproviders;
//   - nobody: authenticated, no grant;
//   - project token proj:team-a:ci: repositories get in team-a.
const testPolicy = `
p, role:repo-admin, repositories, *, *, allow
p, role:repo-admin, write-repositories, *, *, allow
p, role:repo-admin, certificates, *, *, allow
p, role:repo-admin, gpgkeys, *, *, allow
p, role:repo-admin, scmproviders, *, *, allow
p, role:repo-admin, applications, *, */*, allow
g, admin, role:repo-admin
g, reader, role:readonly
p, team-a, repositories, *, team-a/*, allow
p, team-a, applications, *, team-a/*, allow
p, team-b, repositories, *, team-b/*, allow
p, team-b, applications, *, team-b/*, allow
p, scm, scmproviders, get, github-*, allow
p, repo-only, repositories, *, *, allow
`

var testUsers = []string{"admin", "reader", "team-a", "team-b", "scm", "repo-only", "nobody"}

// fakeRepoServer is an in-process repo-server: TestRepository fails for URLs containing
// "unreachable" (echoing the password, which the resolver must redact) and for the
// password "wrong"; browsing methods return fixed data. It records what it received.
type fakeRepoServer struct {
	reposerver.Client
	mu         sync.Mutex
	tested     []reposerver.Repository
	last       map[string]reposerver.Repository
	appDetails []reposerver.AppDetailsQuery
}

func (f *fakeRepoServer) record(method string, r reposerver.Repository) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.last == nil {
		f.last = map[string]reposerver.Repository{}
	}
	f.last[method] = r
	if method == reposerver.MethodTestRepository {
		f.tested = append(f.tested, r)
	}
}

func (f *fakeRepoServer) lastRepo(method string) reposerver.Repository {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last[method]
}

func (f *fakeRepoServer) testCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.tested)
}

func (f *fakeRepoServer) TestRepository(_ context.Context, req *reposerver.TestRepositoryRequest) (*reposerver.TestRepositoryResponse, error) {
	f.record(reposerver.MethodTestRepository, req.Repo)
	switch {
	case strings.Contains(req.Repo.URL, "unreachable"):
		return nil, &reposerver.Error{Code: reposerver.CodeInvalidArgument, Message: "unable to ls-remote HEAD on repository: dial failed for " + req.Repo.Password}
	case req.Repo.Password == "wrong":
		return nil, &reposerver.Error{Code: reposerver.CodeInvalidArgument, Message: "authentication required"}
	}
	return &reposerver.TestRepositoryResponse{VerifiedRepository: true}, nil
}

func (f *fakeRepoServer) ListRefs(_ context.Context, req *reposerver.ListRefsRequest) (*reposerver.Refs, error) {
	f.record(reposerver.MethodListRefs, req.Repo)
	switch {
	case strings.Contains(req.Repo.URL, "unreachable"):
		return nil, &reposerver.Error{Code: reposerver.CodeInvalidArgument, Message: "failed to list refs with " + req.Repo.Password}
	case strings.Contains(req.Repo.URL, "reposerver-down"):
		return nil, &reposerver.Error{Code: reposerver.CodeUnavailable, Message: "connection refused"}
	}
	return &reposerver.Refs{Branches: []string{"main", "dev"}, Tags: []string{"v1.0.0"}}, nil
}

func (f *fakeRepoServer) ListOCITags(_ context.Context, req *reposerver.ListRefsRequest) (*reposerver.Refs, error) {
	f.record(reposerver.MethodListOCITags, req.Repo)
	return &reposerver.Refs{Tags: []string{"1.0.0", "1.1.0"}}, nil
}

func (f *fakeRepoServer) ListApps(_ context.Context, req *reposerver.ListAppsRequest) (*reposerver.AppList, error) {
	f.record(reposerver.MethodListApps, req.Repo)
	return &reposerver.AppList{Apps: map[string]string{"apps/web": "Kustomize", "apps/api": "Helm"}}, nil
}

func (f *fakeRepoServer) GetAppDetails(_ context.Context, req *reposerver.AppDetailsQuery) (*reposerver.AppDetails, error) {
	f.record(reposerver.MethodGetAppDetails, req.Repo)
	f.mu.Lock()
	f.appDetails = append(f.appDetails, *req)
	f.mu.Unlock()
	return &reposerver.AppDetails{Type: "Helm", Helm: &reposerver.HelmAppSpec{Name: "web", ValueFiles: []string{"values.yaml"}, Parameters: []v1alpha1.HelmParameter{{Name: "replicas", Value: "2"}}}}, nil
}

func (f *fakeRepoServer) GetHelmCharts(_ context.Context, req *reposerver.HelmChartsRequest) (*reposerver.HelmChartsResponse, error) {
	f.record(reposerver.MethodGetHelmCharts, req.Repo)
	return &reposerver.HelmChartsResponse{Items: []reposerver.HelmChart{{Name: "nginx", Versions: []string{"1.0.0", "1.1.0"}}}}, nil
}

// auditLog collects audit entries.
type auditLog struct {
	mu      sync.Mutex
	entries []rest.AuditEntry
}

func (a *auditLog) Audit(_ context.Context, e rest.AuditEntry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, e)
}

func (a *auditLog) byOperation(op string) []rest.AuditEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []rest.AuditEntry
	for _, e := range a.entries {
		if e.Operation == op {
			out = append(out, e)
		}
	}
	return out
}

type settingsHolder struct{ s *settings.Settings }

func (h settingsHolder) Current() *settings.Settings { return h.s }

type envOptions struct {
	hydrator  bool
	anonymous bool
	// defaultRole is policy.default.
	defaultRole string
	// ttl is the connection state TTL (default: the service default).
	ttl time.Duration
	// wrap, when set, wraps the Kubernetes client of the handlers (fault injection).
	wrap func(client.Client) repositories.KubeClient
	// policy, when set, replaces testPolicy as policy.csv.
	policy *string
}

// testEnv is a running API server with the repository routes against envtest, with the
// real rbac.Enforcer, auth.TokenService, repository Lister/Resolver/StatusWriter and a
// fake repo-server.
type testEnv struct {
	t        *testing.T
	ns       string
	kc       client.Client
	kube     kubernetes.Interface
	dyn      dynamic.Interface
	lister   *repository.Lister
	resolver *repository.Resolver
	repo     *fakeRepoServer
	tokens   *auth.TokenService
	enforcer *rbac.Enforcer
	audit    *auditLog
	url      string
}

var testScheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		panic(err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		panic(err)
	}
	return s
}()

func newEnv(t *testing.T, o envOptions) *testEnv {
	t.Helper()
	cfg := envConfig(t)
	ctx := t.Context()
	kc, err := client.New(cfg, client.Options{Scheme: testScheme})
	require.NoError(t, err)
	kube, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)
	dyn, err := dynamic.NewForConfig(cfg)
	require.NoError(t, err)
	ns, err := kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "patrol-api-repo-"}}, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = kube.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{}) })

	e := &testEnv{t: t, ns: ns.Name, kc: kc, kube: kube, dyn: dyn, repo: &fakeRepoServer{}, audit: &auditLog{}}
	e.lister, err = repository.NewLister(dyn, kube, repository.ListerOptions{Namespace: ns.Name, ResyncPeriod: time.Minute})
	require.NoError(t, err)
	lctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, e.lister.Start(lctx))
	e.resolver, err = repository.NewResolver(e.lister, repository.Options{
		RepoServer: e.repo, Registerer: prometheus.NewRegistry(), Logger: slog.New(slog.DiscardHandler),
	})
	require.NoError(t, err)

	st := settings.Parse(settings.Input{
		Namespace: ns.Name, IgnorePartOfLabel: true,
		ConfigMaps: []*corev1.ConfigMap{{ObjectMeta: metav1.ObjectMeta{Name: settings.DefaultConfigMapName, Namespace: ns.Name},
			Data: map[string]string{"kustomize.buildOptions": "--enable-helm", "helm.valuesFileSchemes": "https"}}},
	})
	var handlerClient repositories.KubeClient = kc
	if o.wrap != nil {
		handlerClient = o.wrap(kc)
	}
	svc := repositories.New(repositories.Deps{
		Config:       repositories.Config{Namespace: ns.Name, HydratorEnabled: o.hydrator, ConnectionTimeout: 10 * time.Second, ConnectionStateTTL: o.ttl},
		Client:       handlerClient,
		Resolver:     e.resolver,
		StatusWriter: repository.NewStatusWriter(dyn),
		RepoServer:   e.repo,
		Settings:     settingsHolder{st},
		Logger:       slog.New(slog.DiscardHandler),
	})
	tbl := rest.NewTable()
	svc.Register(tbl)
	require.NoError(t, tbl.Err())

	e.enforcer = rbac.NewEnforcer(rbac.WithLogger(slog.New(slog.DiscardHandler)))
	policy := testPolicy
	if o.policy != nil {
		policy = *o.policy
	}
	require.NoError(t, e.enforcer.ApplyConfigMap(rbac.BuiltinPolicy(), map[string]string{"policy.csv": policy, "policy.default": o.defaultRole}))
	require.NoError(t, e.enforcer.SetProjectPolicies("team-a", []rbac.Role{{Name: "ci", Policies: []string{"p, proj:team-a:ci, repositories, get, team-a/*, allow"}}}))
	require.NoError(t, e.enforcer.SetProjectPolicies("team-b", nil))

	ks, err := auth.NewKeySet([]byte("0123456789abcdef0123456789abcdef-test-key"))
	require.NoError(t, err)
	accounts := map[string]auth.Account{}
	for _, u := range testUsers {
		accounts[u] = auth.Account{Name: u, Enabled: true, Capabilities: []auth.Capability{auth.CapabilityLogin}}
	}
	e.tokens, err = auth.NewTokenService(auth.TokenServiceConfig{
		Keys: ks,
		Accounts: auth.AccountLookupFunc(func(_ context.Context, name string) (auth.Account, error) {
			if a, ok := accounts[name]; ok {
				return a, nil
			}
			return auth.Account{}, auth.ErrAccountNotFound
		}),
		Revocations: auth.NewMemoryRevocationStore(auth.RevocationLimits{}, nil),
		Projects:    auth.TokenRegistryFunc(func(context.Context, string, string, int64) (bool, error) { return true, nil }),
	})
	require.NoError(t, err)
	m, err := rest.NewMetrics(prometheus.NewRegistry())
	require.NoError(t, err)
	env := &rest.Env{
		Authenticator: &rest.Authenticator{
			Verifier: e.tokens,
			Policy: func() rest.AuthPolicy {
				return rest.AuthPolicy{AnonymousEnabled: o.anonymous, Scopes: rbac.DefaultScopes()}
			},
		},
		Enforcer: e.enforcer,
		Auditor:  e.audit,
		Metrics:  m,
		Logger:   slog.New(slog.DiscardHandler),
	}
	handler, err := tbl.Handler(env)
	require.NoError(t, err)
	srv := httptest.NewServer(rest.Chain(handler, rest.Recover(slog.New(slog.DiscardHandler), m), rest.RequestIDs()))
	t.Cleanup(srv.Close)
	e.url = srv.URL
	return e
}

// client returns an API client for user ("" for no token).
func (e *testEnv) client(user string) *apiclient.Client {
	e.t.Helper()
	tok := ""
	if user != "" {
		iss, err := e.tokens.IssueLoginSession(context.Background(), auth.Account{Name: user, Enabled: true, Capabilities: []auth.Capability{auth.CapabilityLogin}}, time.Hour)
		require.NoError(e.t, err)
		tok = iss.Token
	}
	return e.clientWithToken(tok)
}

func (e *testEnv) clientWithToken(tok string) *apiclient.Client {
	e.t.Helper()
	c, err := apiclient.NewClient(&apiclient.ClientOptions{ServerAddr: e.url, AuthToken: tok})
	require.NoError(e.t, err)
	return c
}

// projectClient returns a client using a project role token.
func (e *testEnv) projectClient(proj, role string) *apiclient.Client {
	e.t.Helper()
	iss, err := e.tokens.IssueProjectToken(context.Background(), proj, role, "tok-"+role, time.Hour)
	require.NoError(e.t, err)
	return e.clientWithToken(iss.Token)
}

func (e *testEnv) createProject(name string, sourceRepos ...string) {
	e.t.Helper()
	p := &v1alpha1.AppProject{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: e.ns}, Spec: v1alpha1.AppProjectSpec{SourceRepos: sourceRepos}}
	require.NoError(e.t, e.kc.Create(e.t.Context(), p))
}

func (e *testEnv) secret(name string) *corev1.Secret {
	e.t.Helper()
	s := &corev1.Secret{}
	err := e.kc.Get(e.t.Context(), client.ObjectKey{Namespace: e.ns, Name: name}, s)
	if err != nil {
		return nil
	}
	return s
}

func (e *testEnv) repositories() []v1alpha1.Repository {
	e.t.Helper()
	var l v1alpha1.RepositoryList
	require.NoError(e.t, e.kc.List(e.t.Context(), &l, client.InNamespace(e.ns)))
	return l.Items
}

// waitResolved waits until the repository library (informers) resolves url for project
// with the given password.
func (e *testEnv) waitResolved(url, project, password string) {
	e.t.Helper()
	require.Eventually(e.t, func() bool {
		res, err := e.resolver.Resolve(context.Background(), repository.Query{URL: url, Project: project})
		return err == nil && res.Repository.Password == password
	}, 10*time.Second, 20*time.Millisecond, "resolver never saw %s with the expected credentials", url)
}

func apiErr(t *testing.T, err error) *apiclient.APIError {
	t.Helper()
	require.Error(t, err)
	var ae *apiclient.APIError
	require.True(t, errors.As(err, &ae), "want *apiclient.APIError, got %T: %v", err, err)
	return ae
}

// requireStatus asserts an API error status (and code when given) and returns its
// problem document.
func requireStatus(t *testing.T, err error, status int, code apiclient.ErrorCode) *apiclient.Problem {
	t.Helper()
	ae := apiErr(t, err)
	require.Equal(t, status, ae.StatusCode, "detail: %s", ae.Problem.Detail)
	if code != "" {
		require.Equal(t, code, ae.Problem.Code)
	}
	return &ae.Problem
}

// requireNoSecret asserts that v's JSON form contains none of the secrets.
func requireNoSecret(t *testing.T, v any, secrets ...string) {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	for _, s := range secrets {
		require.NotContains(t, string(b), s)
	}
}

func basicCreds(user, pass string) *apiclient.RepositoryCredentials {
	return &apiclient.RepositoryCredentials{Username: user, Password: pass}
}

func repoBody(url, project string, creds *apiclient.RepositoryCredentials) apiclient.RepositoryWriteRequest {
	return apiclient.RepositoryWriteRequest{Spec: v1alpha1.RepositorySpec{URL: url, Project: project}, Credentials: creds}
}
