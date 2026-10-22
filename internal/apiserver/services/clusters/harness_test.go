package clusters_test

import (
	"context"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/golang-jwt/jwt/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	restapi "github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/clusters"
	"github.com/juliosaraiva/patrol-cd/internal/auth"
	clusterlib "github.com/juliosaraiva/patrol-cd/internal/clusters"
	"github.com/juliosaraiva/patrol-cd/internal/envtestx"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// testCfg is the shared envtest API server (with the Patrol CRDs), started only when
// KUBEBUILDER_ASSETS is set.
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
	cfg, err := envtestx.Start(env)
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

// settingsHolder rebuilds the settings snapshot from raw ConfigMap data.
type settingsHolder struct {
	mu     sync.Mutex
	ns     string
	cm     map[string]string
	rbacCM map[string]string
	secret map[string]string
	cur    atomic.Pointer[settings.Settings]
}

func (h *settingsHolder) Current() *settings.Settings { return h.cur.Load() }

func (h *settingsHolder) rebuild() {
	toBytes := func(m map[string]string) map[string][]byte {
		out := map[string][]byte{}
		for k, v := range m {
			out[k] = []byte(v)
		}
		return out
	}
	in := settings.Input{
		Namespace:         h.ns,
		IgnorePartOfLabel: true,
		ConfigMaps: []*corev1.ConfigMap{
			{ObjectMeta: metav1.ObjectMeta{Name: settings.DefaultConfigMapName, Namespace: h.ns}, Data: maps.Clone(h.cm)},
			{ObjectMeta: metav1.ObjectMeta{Name: settings.DefaultRBACConfigMapName, Namespace: h.ns}, Data: maps.Clone(h.rbacCM)},
		},
		Secrets: []*corev1.Secret{{ObjectMeta: metav1.ObjectMeta{Name: settings.DefaultSecretName, Namespace: h.ns}, Data: toBytes(h.secret)}},
	}
	h.cur.Store(settings.Parse(in))
}

func (h *settingsHolder) setCM(key, value string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cm[key] = value
	h.rebuild()
}

// auditLog collects audit entries.
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
//   - admin: role:admin (built-in);
//   - alice: role:readonly;
//   - bob: every cluster action on clusters scoped to project team-a;
//   - frank: get and create (not update) on clusters scoped to team-a;
//   - henry: create on every cluster object and get on the in-cluster server (no update);
//   - carol: nothing.
const testPolicy = `g, alice, role:readonly
p, bob, clusters, *, team-a/*, allow
p, frank, clusters, get, team-a/*, allow
p, frank, clusters, create, team-a/*, allow
p, henry, clusters, create, *, allow
p, henry, clusters, get, https://kubernetes.default.svc, allow
`

type envOptions struct {
	cm        map[string]string
	connector func(e *testEnv) clusters.Connector
}

// testEnv is the cluster service mounted on the real route chain (token verification,
// RBAC enforcer, audit) over a dedicated namespace of the envtest API server.
type testEnv struct {
	t         *testing.T
	ns        string
	cfg       *rest.Config
	kube      client.Client
	clientset kubernetes.Interface
	holder    *settingsHolder
	tokens    *auth.TokenService
	enforcer  *rbac.Enforcer
	audit     *auditLog
	url       string
}

func newEnv(t *testing.T, o envOptions) *testEnv {
	t.Helper()
	cfg := envConfig(t)
	kube, err := client.New(cfg, client.Options{Scheme: testScheme})
	require.NoError(t, err)
	clientset, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)
	dyn, err := dynamic.NewForConfig(cfg)
	require.NoError(t, err)
	ns := createNamespace(t, clientset, "patrol")
	e := &testEnv{t: t, ns: ns, cfg: cfg, kube: kube, clientset: clientset, audit: &auditLog{}}
	cm := map[string]string{
		"accounts.alice": "login", "accounts.bob": "login", "accounts.carol": "login", "accounts.frank": "login", "accounts.henry": "login",
	}
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
	})
	require.NoError(t, err)
	e.enforcer = rbac.NewEnforcer()
	require.NoError(t, e.enforcer.ApplyConfigMap(rbac.BuiltinPolicy(), e.holder.rbacCM))

	var conn clusters.Connector
	if o.connector != nil {
		conn = o.connector(e)
	} else {
		conn = clusters.NewConnector(clusters.ConnectorOptions{Timeout: 5 * time.Second})
	}
	svc := clusters.New(clusters.Deps{
		Config:    clusters.Config{Namespace: ns, Bootstrap: &clusterlib.BootstrapOptions{TokenTimeout: 10 * time.Second, PollInterval: 50 * time.Millisecond}},
		Client:    kube,
		Settings:  e.holder,
		Connector: conn,
		Status:    clusterlib.NewStatusWriter(dyn),
		Logger:    slog.New(slog.DiscardHandler),
	})
	reg := prometheus.NewRegistry()
	m, err := restapi.NewMetrics(reg)
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

// client returns an API client with a login session of user ("" for no token).
func (e *testEnv) client(user string) *apiclient.Client {
	e.t.Helper()
	opts := &apiclient.ClientOptions{ServerAddr: e.url}
	if user != "" {
		acct, err := e.holder.Current().Accounts().Get(user)
		require.NoError(e.t, err)
		iss, err := e.tokens.IssueLoginSession(context.Background(), acct, time.Hour)
		require.NoError(e.t, err)
		opts.AuthToken = iss.Token
	}
	c, err := apiclient.NewClient(opts)
	require.NoError(e.t, err)
	return c
}

func createNamespace(t *testing.T, kube kubernetes.Interface, prefix string) string {
	t.Helper()
	name := prefix + "-" + utilrand.String(6)
	_, err := kube.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{})
	require.NoError(t, err)
	return name
}

func (e *testEnv) createProject(name string) {
	e.t.Helper()
	require.NoError(e.t, e.kube.Create(context.Background(), &v1alpha1.AppProject{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: e.ns}}))
}

func apiErr(t *testing.T, err error) *apiclient.APIError {
	t.Helper()
	require.Error(t, err)
	var ae *apiclient.APIError
	require.True(t, errors.As(err, &ae), "want *apiclient.APIError, got %T: %v", err, err)
	return ae
}

// fakeCluster is a destination cluster: a TLS front end of the envtest API server that
// authenticates callers itself (static bearer tokens, basic credentials, or the tokens of
// the service-account token Secrets of tokenNS) and forwards authenticated requests with
// admin credentials.
type fakeCluster struct {
	t       *testing.T
	srv     *httptest.Server
	caPEM   []byte
	kube    kubernetes.Interface
	tokenNS string

	mu       sync.Mutex
	tokens   map[string]bool
	user     string
	password string
	requests atomic.Int64
}

func newFakeCluster(t *testing.T, cfg *rest.Config, kube kubernetes.Interface, tokenNS string) *fakeCluster {
	t.Helper()
	target, err := url.Parse(cfg.Host)
	require.NoError(t, err)
	rt, err := rest.TransportFor(cfg)
	require.NoError(t, err)
	f := &fakeCluster{t: t, kube: kube, tokenNS: tokenNS, tokens: map[string]bool{}}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.Out.Header.Del("Authorization")
		},
		Transport: rt,
	}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		if !f.authenticated(r) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"Unauthorized","reason":"Unauthorized","code":401}`)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	f.caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw})
	return f
}

func (f *fakeCluster) addToken(tok string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens[tok] = true
}

func (f *fakeCluster) setBasic(user, password string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.user, f.password = user, password
}

func (f *fakeCluster) authenticated(r *http.Request) bool {
	if u, p, ok := r.BasicAuth(); ok {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.user != "" && u == f.user && p == f.password
	}
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || tok == "" {
		return false
	}
	f.mu.Lock()
	static := f.tokens[tok]
	f.mu.Unlock()
	if static {
		return true
	}
	if f.tokenNS == "" {
		return false
	}
	list, err := f.kube.CoreV1().Secrets(f.tokenNS).List(r.Context(), metav1.ListOptions{FieldSelector: "type=" + string(corev1.SecretTypeServiceAccountToken)})
	if err != nil {
		return false
	}
	for _, s := range list.Items {
		if string(s.Data[corev1.ServiceAccountTokenKey]) == tok {
			return true
		}
	}
	return false
}

// legacyToken builds a JWT shaped like a legacy service-account token.
func legacyToken(key []byte, namespace, secret, sa string) (string, error) {
	return jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"iss":                                    "kubernetes/serviceaccount",
		"sub":                                    clusterlib.ServiceAccountUsername(namespace, sa),
		"kubernetes.io/serviceaccount/namespace": namespace,
		"kubernetes.io/serviceaccount/secret.name":          secret,
		"kubernetes.io/serviceaccount/service-account.name": sa,
		"kubernetes.io/serviceaccount/service-account.uid":  "uid-" + sa,
	}).SignedString(key)
}

// startTokenController emulates the token controller of kube-controller-manager (envtest
// runs none): service-account token Secrets of namespace get a legacy token.
func startTokenController(t *testing.T, kube kubernetes.Interface, namespace string) {
	t.Helper()
	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() {
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			list, err := kube.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{FieldSelector: "type=" + string(corev1.SecretTypeServiceAccountToken)})
			if err != nil {
				continue
			}
			for i := range list.Items {
				s := &list.Items[i]
				if len(s.Data[corev1.ServiceAccountTokenKey]) > 0 {
					continue
				}
				tok, err := legacyToken(key, namespace, s.Name, s.Annotations[corev1.ServiceAccountNameKey])
				if err != nil {
					continue
				}
				if s.Data == nil {
					s.Data = map[string][]byte{}
				}
				s.Data[corev1.ServiceAccountTokenKey] = []byte(tok)
				s.Data["namespace"] = []byte(namespace)
				_, _ = kube.CoreV1().Secrets(namespace).Update(ctx, s, metav1.UpdateOptions{})
			}
		}
	})
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
}

// clusterObject reads the Cluster object serving server.
func (e *testEnv) clusterObject(server string) *v1alpha1.Cluster {
	e.t.Helper()
	var list v1alpha1.ClusterList
	require.NoError(e.t, e.kube.List(context.Background(), &list, client.InNamespace(e.ns)))
	for i := range list.Items {
		if list.Items[i].Spec.Server == server {
			return &list.Items[i]
		}
	}
	return nil
}

func (e *testEnv) secret(name string) *corev1.Secret {
	e.t.Helper()
	var s corev1.Secret
	err := e.kube.Get(context.Background(), client.ObjectKey{Namespace: e.ns, Name: name}, &s)
	if err != nil {
		return nil
	}
	return &s
}
