package projects_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	restapi "github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/projects"
	"github.com/juliosaraiva/patrol-cd/internal/auth"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

var (
	testCfg    *rest.Config
	testScheme = runtime.NewScheme()
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	slog.SetDefault(slog.New(slog.DiscardHandler))
	if err := clientgoscheme.AddToScheme(testScheme); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := v1alpha1.AddToScheme(testScheme); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		return m.Run()
	}
	ctrllog.SetLogger(logr.Discard())
	env := &envtest.Environment{
		UseExistingCluster:    ptr.To(false),
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "..", "deploy", "crds")},
		ErrorIfCRDPathMissing: true,
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

func envConfig(t *testing.T) *rest.Config {
	t.Helper()
	if testCfg == nil {
		t.Skip("KUBEBUILDER_ASSETS not set; skipping envtest integration test")
	}
	return rest.CopyConfig(testCfg)
}

const serverKey = "0123456789abcdef0123456789abcdef-server-key"

type settingsHolder struct {
	ns     string
	cm     map[string]string
	rbacCM map[string]string
	secret map[string]string
	cur    atomic.Pointer[settings.Settings]
}

func (h *settingsHolder) Current() *settings.Settings { return h.cur.Load() }

func (h *settingsHolder) rebuild() {
	data := map[string][]byte{}
	for k, v := range h.secret {
		data[k] = []byte(v)
	}
	h.cur.Store(settings.Parse(settings.Input{
		Namespace:         h.ns,
		IgnorePartOfLabel: true,
		ConfigMaps: []*corev1.ConfigMap{
			{ObjectMeta: metav1.ObjectMeta{Name: settings.DefaultConfigMapName, Namespace: h.ns}, Data: maps.Clone(h.cm)},
			{ObjectMeta: metav1.ObjectMeta{Name: settings.DefaultRBACConfigMapName, Namespace: h.ns}, Data: maps.Clone(h.rbacCM)},
		},
		Secrets: []*corev1.Secret{{ObjectMeta: metav1.ObjectMeta{Name: settings.DefaultSecretName, Namespace: h.ns}, Data: data}},
	}))
}

type auditLog struct {
	mu      sync.Mutex
	entries []restapi.AuditEntry
}

func (a *auditLog) Audit(_ context.Context, e restapi.AuditEntry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, e)
}

func (a *auditLog) byOperation(op string) []restapi.AuditEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []restapi.AuditEntry
	for _, e := range a.entries {
		if e.Operation == op {
			out = append(out, e)
		}
	}
	return out
}

// testPolicy grants:
//   - admin: role:admin (built-in); alice: role:readonly;
//   - dave: get/update team-a, create/delete team-* projects (no cluster/repository rights);
//   - erin: every project action on team-*, clusters update on one server, repositories
//     update on one organization, and get on team-a's scoped repositories and clusters;
//   - gina: get and create (not update) team-* projects;
//   - ivan: every project action on every project, no cluster/repository rights;
//   - kim: every project action on team-*, clusters update on kube and dev.example.com;
//   - carol: nothing.
const testPolicy = `g, alice, role:readonly
p, gina, projects, get, team-*, allow
p, gina, projects, create, team-*, allow
p, dave, projects, get, team-a, allow
p, dave, projects, update, team-a, allow
p, dave, projects, create, team-*, allow
p, dave, projects, delete, team-*, allow
p, erin, projects, *, team-*, allow
p, erin, clusters, update, https://allowed.example.com, allow
p, erin, repositories, update, https://github.com/org/*, allow
p, erin, repositories, get, team-a/*, allow
p, erin, clusters, get, team-a/*, allow
p, ivan, projects, *, *, allow
p, kim, projects, *, team-*, allow
p, kim, clusters, update, https://kube.example.com, allow
p, kim, clusters, update, https://dev.example.com, allow
`

type testEnv struct {
	t         *testing.T
	ns        string
	kube      client.Client
	clientset kubernetes.Interface
	holder    *settingsHolder
	tokens    *auth.TokenService
	enforcer  *rbac.Enforcer
	audit     *auditLog
	url       string
}

type envOptions struct {
	cm  map[string]string
	now func() time.Time
}

func newEnv(t *testing.T, o envOptions) *testEnv {
	t.Helper()
	cfg := envConfig(t)
	kube, err := client.New(cfg, client.Options{Scheme: testScheme})
	require.NoError(t, err)
	clientset, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)
	ns := "patrol-" + utilrand.String(6)
	_, err = clientset.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{})
	require.NoError(t, err)
	e := &testEnv{t: t, ns: ns, kube: kube, clientset: clientset, audit: &auditLog{}}
	cm := map[string]string{"accounts.alice": "login", "accounts.carol": "login", "accounts.dave": "login", "accounts.erin": "login", "accounts.gina": "login", "accounts.ivan": "login", "accounts.kim": "login"}
	maps.Copy(cm, o.cm)
	e.holder = &settingsHolder{ns: ns, cm: cm, rbacCM: map[string]string{"policy.csv": testPolicy}, secret: map[string]string{auth.KeyServerSecretKey: serverKey}}
	e.holder.rebuild()
	ks, err := e.holder.Current().SigningKeys()
	require.NoError(t, err)
	e.tokens, err = auth.NewTokenService(auth.TokenServiceConfig{
		Keys: ks,
		Accounts: auth.AccountLookupFunc(func(_ context.Context, name string) (auth.Account, error) {
			return e.holder.Current().Accounts().Get(name)
		}),
		Revocations: auth.NewMemoryRevocationStore(auth.RevocationLimits{}, time.Now),
		Projects:    e.tokenRegistry(),
	})
	require.NoError(t, err)
	e.enforcer = rbac.NewEnforcer()
	require.NoError(t, e.enforcer.ApplyConfigMap(rbac.BuiltinPolicy(), e.holder.rbacCM))

	svc := projects.New(projects.Deps{
		Config:   projects.Config{Namespace: ns},
		Client:   kube,
		Settings: e.holder,
		Tokens:   e.tokens,
		Events:   clientset.CoreV1().Events(ns),
		Policies: e.enforcer,
		Logger:   slog.New(slog.DiscardHandler),
		Now:      o.now,
	})
	m, err := restapi.NewMetrics(prometheus.NewRegistry())
	require.NoError(t, err)
	tbl := restapi.NewTable()
	svc.Register(tbl)
	require.NoError(t, tbl.Err())
	handler, err := tbl.Handler(&restapi.Env{
		Authenticator: &restapi.Authenticator{
			Verifier: e.tokens,
			Policy: func() restapi.AuthPolicy {
				return restapi.AuthPolicy{AnonymousEnabled: e.holder.Current().AnonymousUserEnabled(), Scopes: rbac.DefaultScopes()}
			},
		},
		Enforcer: e.enforcer,
		Auditor:  e.audit,
		Metrics:  m,
	})
	require.NoError(t, err)
	srv := httptest.NewServer(restapi.Chain(handler, restapi.Recover(nil, m), restapi.RequestIDs()))
	t.Cleanup(srv.Close)
	e.url = srv.URL
	return e
}

// tokenRegistry mirrors apiserver.ProjectTokenRegistry (the production adapter) over the
// test namespace: a project token is known while its project and role exist and
// status.jwtTokensByRole[role] lists its jti (or, for tokens without jti, its iat).
func (e *testEnv) tokenRegistry() auth.TokenRegistry {
	return auth.TokenRegistryFunc(func(ctx context.Context, subject, jti string, issuedAt int64) (bool, error) {
		proj, role, ok := auth.ParseProjectSubject(subject)
		if !ok {
			return false, nil
		}
		var p v1alpha1.AppProject
		if err := e.kube.Get(ctx, client.ObjectKey{Namespace: e.ns, Name: proj}, &p); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		if !slices.ContainsFunc(p.Spec.Roles, func(r v1alpha1.ProjectRole) bool { return r.Name == role }) {
			return false, nil
		}
		for _, t := range p.Status.JWTTokensByRole[role].Items {
			if (jti != "" && t.ID == jti) || (t.ID == "" && issuedAt != 0 && t.IssuedAt == issuedAt) {
				return true, nil
			}
		}
		return false, nil
	})
}

func (e *testEnv) newClient(token string) *apiclient.Client {
	e.t.Helper()
	c, err := apiclient.NewClient(&apiclient.ClientOptions{ServerAddr: e.url, AuthToken: token})
	require.NoError(e.t, err)
	return c
}

// client returns an API client with a login session of user ("" for no token).
func (e *testEnv) client(user string) *apiclient.Client {
	e.t.Helper()
	if user == "" {
		return e.newClient("")
	}
	acct, err := e.holder.Current().Accounts().Get(user)
	require.NoError(e.t, err)
	iss, err := e.tokens.IssueLoginSession(context.Background(), acct, time.Hour)
	require.NoError(e.t, err)
	return e.newClient(iss.Token)
}

// ssoClient returns a client with an SSO session carrying groups.
func (e *testEnv) ssoClient(subject string, groups ...string) *apiclient.Client {
	e.t.Helper()
	iss, err := e.tokens.IssueSSOSession(context.Background(), auth.SSOIdentity{Subject: subject, Email: subject + "@example.com", Groups: groups}, time.Hour)
	require.NoError(e.t, err)
	return e.newClient(iss.Token)
}

func (e *testEnv) stored(name string) *v1alpha1.AppProject {
	e.t.Helper()
	var p v1alpha1.AppProject
	require.NoError(e.t, e.kube.Get(context.Background(), client.ObjectKey{Namespace: e.ns, Name: name}, &p))
	return &p
}

func (e *testEnv) createApp(name, project, repo, server, namespace string) {
	e.t.Helper()
	app := &v1alpha1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: e.ns},
		Spec: v1alpha1.ApplicationSpec{
			Project:     project,
			Source:      &v1alpha1.ApplicationSource{RepoURL: repo, Path: "."},
			Destination: v1alpha1.ApplicationDestination{Server: server, Namespace: namespace},
		},
	}
	require.NoError(e.t, e.kube.Create(context.Background(), app))
}

func (e *testEnv) createCluster(name, server, project string) *v1alpha1.Cluster {
	e.t.Helper()
	c := &v1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: e.ns}, Spec: v1alpha1.ClusterSpec{Server: server, Name: name, Project: project}}
	require.NoError(e.t, e.kube.Create(context.Background(), c))
	return c
}

func apiErr(t *testing.T, err error) *apiclient.APIError {
	t.Helper()
	require.Error(t, err)
	var ae *apiclient.APIError
	require.True(t, errors.As(err, &ae), "want *apiclient.APIError, got %T: %v", err, err)
	return ae
}
