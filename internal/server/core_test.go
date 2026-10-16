package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes"
	krest "k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/extensions"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/webhooks"
	"github.com/juliosaraiva/patrol-cd/internal/compare"
	"github.com/juliosaraiva/patrol-cd/internal/controller/sharding"
	"github.com/juliosaraiva/patrol-cd/internal/controller/treeapi"
	"github.com/juliosaraiva/patrol-cd/internal/diff"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver"
	reposerverapi "github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

func TestCoreConfigValidate(t *testing.T) {
	t.Parallel()
	_, _, err := NewCoreHandler(t.Context(), CoreConfig{})
	require.ErrorContains(t, err, "namespace is required")
	require.ErrorContains(t, err, "Kubernetes client configuration is required")
	_, _, err = NewCoreHandler(t.Context(), CoreConfig{Namespace: "Not_A_Namespace", RestConfig: &krest.Config{}})
	require.ErrorContains(t, err, "not a valid namespace name")
}

func TestCoreDialTargets(t *testing.T) {
	t.Parallel()
	a := &coreAssembly{cfg: CoreConfig{Namespace: "patrol"}, repoHostPort: "patrol-repo-server:8081", repoPort: 8081, treeService: "patrol-controller"}
	for addr, want := range map[string]CoreDialTarget{
		"patrol-repo-server:8081":                               {Component: CoreComponentRepoServer, Port: 8081},
		"patrol-controller-3.patrol-controller.patrol.svc:8090": {Component: CoreComponentController, Pod: "patrol-controller-3", Port: 8090},
	} {
		got, err := a.dialTarget(addr)
		require.NoError(t, err, addr)
		assert.Equal(t, want, got, addr)
	}
	for _, addr := range []string{
		"patrol-repo-server:8082", "evil.example.com:443", "x.patrol-controller.other.svc:8090", "x.other.patrol.svc:8090",
		".patrol-controller.patrol.svc:8090", "a.b.patrol-controller.patrol.svc:8090", "169.254.169.254:80",
	} {
		_, err := a.dialTarget(addr)
		require.ErrorContains(t, err, "no route", addr)
	}
	a.cfg.TreeAPI.URL = "http://127.0.0.1:1"
	_, err := a.dialTarget("patrol-controller-3.patrol-controller.patrol.svc:8090")
	require.Error(t, err, "with a fixed tree API URL shard addresses are not routed")

	for raw, want := range map[string]string{
		"https://patrol-repo-server:8081": "patrol-repo-server:8081",
		"http://repo.patrol.svc":          "repo.patrol.svc:80",
		"https://repo.patrol.svc":         "repo.patrol.svc:443",
	} {
		got, _, err := canonicalHostPort(raw)
		require.NoError(t, err)
		assert.Equal(t, want, got)
	}
	u, err := coreRepoURL("", RepoServerClientOptions{Address: "patrol-repo-server:8081", TLS: TLSClientOptions{Plaintext: true}})
	require.NoError(t, err)
	assert.Equal(t, "http://patrol-repo-server:8081", u)
	u, err = coreRepoURL("", RepoServerClientOptions{Address: "patrol-repo-server:8081"})
	require.NoError(t, err)
	assert.Equal(t, "https://patrol-repo-server:8081", u)
}

// pluginRepo answers ListPlugins and records the bearer token it was called with.
type pluginRepo struct{ reposerverapi.Client }

func (pluginRepo) ListPlugins(context.Context) (*reposerverapi.PluginList, error) {
	return &reposerverapi.PluginList{Items: []reposerverapi.PluginInfo{{Name: "cmp-demo"}}}, nil
}

// treeStub serves an empty tree for every Application.
type treeStub struct{}

func (treeStub) Tree(context.Context, treeapi.AppKey) (*compare.ApplicationTree, error) {
	return &compare.ApplicationTree{}, nil
}

func (treeStub) ManagedResources(context.Context, treeapi.AppKey) ([]compare.ManagedResource, error) {
	return nil, nil
}

func (treeStub) ServerSideDiff(context.Context, treeapi.AppKey, []*unstructured.Unstructured, []*unstructured.Unstructured) ([]diff.Result, error) {
	return nil, nil
}

func (treeStub) LiveResource(context.Context, treeapi.AppKey, compare.ResourceRef) (*unstructured.Unstructured, error) {
	return nil, treeapi.ErrNotFound
}

func (treeStub) SensitiveAnnotations() []string { return nil }

// dialRecorder is CoreConfig.Dial: it records the targets and connects them to test
// servers.
type dialRecorder struct {
	mu      sync.Mutex
	targets []CoreDialTarget
	repo    string
	tree    string
}

func (d *dialRecorder) dial(ctx context.Context, t CoreDialTarget) (net.Conn, error) {
	d.mu.Lock()
	d.targets = append(d.targets, t)
	d.mu.Unlock()
	addr := d.tree
	if t.Component == CoreComponentRepoServer {
		addr = d.repo
	}
	return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
}

func (d *dialRecorder) list() []CoreDialTarget {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]CoreDialTarget(nil), d.targets...)
}

func serve(t *testing.T, h http.Handler, method, path string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), method, path, nil))
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

// TestCoreHandlerEnvtest assembles the core handler against envtest: no listener, tree API
// calls routed to the controller pod of the Application's shard (Cluster
// status.assignedShard, shard Lease holder, else the ordinal pod), repo-server calls with
// the internal token read from its Secret, session routes refused, the caller's identity
// as principal, and Kubernetes denials appended to errors.
func TestCoreHandlerEnvtest(t *testing.T) {
	requireEnvtest(t)
	cfg := krest.CopyConfig(testCfg)
	ctx := t.Context()
	c, err := client.New(cfg, client.Options{Scheme: mustScheme(t)})
	require.NoError(t, err)
	kube, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)
	ns := newNamespace(t, kube)

	const token = "core-handler-test-token"
	_, err = kube.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: InternalTokenSecretName}, Data: map[string][]byte{InternalTokenSecretKey: []byte(token)}}, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.Create(ctx, &v1alpha1.AppProject{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "default"},
		Spec:       v1alpha1.AppProjectSpec{SourceRepos: []string{"*"}, Destinations: []v1alpha1.ApplicationDestination{{Server: "*", Namespace: "*"}}},
	}))
	remote := &v1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "remote"}, Spec: v1alpha1.ClusterSpec{Server: "https://remote.example.com", Name: "remote"}}
	require.NoError(t, c.Create(ctx, remote))
	remote.Status.AssignedShard = ptr.To[int64](2)
	require.NoError(t, c.Status().Update(ctx, remote))
	_, err = kube.CoordinationV1().Leases(ns).Create(ctx, &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: sharding.LeaseName(2)},
		Spec: coordinationv1.LeaseSpec{HolderIdentity: ptr.To("patrol-controller-7"), LeaseDurationSeconds: ptr.To[int32](300),
			RenewTime: &metav1.MicroTime{Time: time.Now()}},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	for name, server := range map[string]string{"on-remote": "https://remote.example.com", "on-local": v1alpha1.KubernetesInternalAPIServerAddr} {
		require.NoError(t, c.Create(ctx, &v1alpha1.Application{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
			Spec: v1alpha1.ApplicationSpec{Project: "default", Source: &v1alpha1.ApplicationSource{RepoURL: "https://example.com/r", Path: "p"},
				Destination: v1alpha1.ApplicationDestination{Server: server, Namespace: "default"}},
		}))
	}

	var seenToken sync.Map
	rh, err := reposerver.NewHandler(pluginRepo{}, reposerver.HandlerOptions{Token: token, Logger: slog.New(slog.DiscardHandler)})
	require.NoError(t, err)
	repoSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenToken.Store(r.Header.Get("Authorization"), true)
		rh.ServeHTTP(w, r)
	}))
	t.Cleanup(repoSrv.Close)
	th, err := treeapi.NewHandler(treeStub{}, treeapi.HandlerOptions{Token: token})
	require.NoError(t, err)
	treeSrv := httptest.NewServer(th)
	t.Cleanup(treeSrv.Close)
	d := &dialRecorder{repo: repoSrv.Listener.Addr().String(), tree: treeSrv.Listener.Addr().String()}

	h, stop, err := NewCoreHandler(ctx, CoreConfig{Namespace: ns, RestConfig: cfg, Dial: d.dial,
		TreeAPI: TreeAPIClientOptions{TLS: TLSClientOptions{Plaintext: true}}})
	require.NoError(t, err)
	defer stop()

	code, body := serve(t, h, http.MethodGet, "/api/v1/version")
	require.Equal(t, http.StatusOK, code, body)
	extra, _ := body["extraBuildInfo"].(map[string]any)
	assert.Contains(t, extra["mode"], "core")

	code, body = serve(t, h, http.MethodGet, "/api/v1/applications/on-remote/resource-tree")
	require.Equal(t, http.StatusOK, code, body)
	code, body = serve(t, h, http.MethodGet, "/api/v1/applications/on-local/resource-tree")
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, []CoreDialTarget{
		{Component: CoreComponentController, Pod: "patrol-controller-7", Port: treeapi.DefaultPort},
		{Component: CoreComponentController, Pod: "patrol-controller-0", Port: treeapi.DefaultPort},
	}, d.list(), "shard 2 → Lease holder; shard 0 without a Lease → ordinal pod")

	code, body = serve(t, h, http.MethodGet, apiclient.PathSettingsPlugins)
	require.Equal(t, http.StatusOK, code, body)
	assert.Contains(t, d.list(), CoreDialTarget{Component: CoreComponentRepoServer, Port: 8081})
	_, ok := seenToken.Load("Bearer " + token)
	assert.True(t, ok, "the repo-server received the internal token read from the Secret")

	for _, r := range []struct{ method, path string }{
		{http.MethodPost, apiclient.PathSession}, {http.MethodDelete, apiclient.PathSession},
		{http.MethodPut, apiclient.PathAccountPassword}, {http.MethodPost, "/api/v1/account/admin/token"},
		{http.MethodDelete, "/api/v1/account/admin/token/x"}, {http.MethodGet, apiclient.PathAuthLogin},
	} {
		code, body = serve(t, h, r.method, r.path)
		assert.Equal(t, http.StatusPreconditionFailed, code, r.path)
		assert.Contains(t, body["detail"], "not available in core mode", r.path)
	}

	// The caller's identity (SelfSubjectReview) is the principal.
	ssr, err := kube.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
	require.NoError(t, err)
	code, body = serve(t, h, http.MethodGet, apiclient.PathSessionUserInfo)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, true, body["loggedIn"])
	assert.Equal(t, ssr.Status.UserInfo.Username, body["username"])
	// A client-sent token is never used.
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, apiclient.PathSessionUserInfo, nil)
	req.Header.Set("Authorization", "Bearer forged")
	h.ServeHTTP(rec, req)
	assert.Contains(t, rec.Body.String(), ssr.Status.UserInfo.Username)

	// Project tokens need the real signing key, absent here.
	require.NoError(t, c.Create(ctx, &v1alpha1.AppProject{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "p"},
		Spec:       v1alpha1.AppProjectSpec{Roles: []v1alpha1.ProjectRole{{Name: "ci"}}},
	}))
	rec = httptest.NewRecorder()
	req = httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/projects/p/roles/ci/token", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	assert.GreaterOrEqual(t, rec.Code, 400, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "project tokens cannot be issued in core mode without the session signing key")
}

// TestCoreHandlerDegrades: without a SelfSubjectReview answer requests are anonymous;
// an unreadable internal token is reported in the error of the request that needed it.
func TestCoreHandlerDegrades(t *testing.T) {
	requireEnvtest(t)
	cfg := krest.CopyConfig(testCfg)
	ctx := t.Context()
	kube, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)
	ns := newNamespace(t, kube)
	denied := krest.CopyConfig(cfg)
	denied.Wrap(func(rt http.RoundTripper) http.RoundTripper { return denySelfSubjectReviews{next: rt} })
	h, stop, err := NewCoreHandler(ctx, CoreConfig{
		Namespace: ns, RestConfig: denied, RepoServerURL: "http://127.0.0.1:1", TreeAPI: TreeAPIClientOptions{URL: "http://127.0.0.1:1"},
		InternalToken: func(context.Context) (string, error) {
			return "", errors.New("internal token unavailable for this test")
		},
	})
	require.NoError(t, err)
	defer stop()
	code, body := serve(t, h, http.MethodGet, apiclient.PathSessionUserInfo)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, false, body["loggedIn"])
	code, body = serve(t, h, http.MethodGet, apiclient.PathSettingsPlugins)
	assert.GreaterOrEqual(t, code, 500)
	assert.Contains(t, body["detail"], "internal token unavailable for this test")

	// Without an identity, a sync creates its Operation with an empty initiator (the shape
	// the admission policy admits for anyone).
	c, err := client.New(cfg, client.Options{Scheme: mustScheme(t)})
	require.NoError(t, err)
	require.NoError(t, c.Create(ctx, &v1alpha1.AppProject{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "default"},
		Spec:       v1alpha1.AppProjectSpec{SourceRepos: []string{"*"}, Destinations: []v1alpha1.ApplicationDestination{{Server: "*", Namespace: "*"}}},
	}))
	require.NoError(t, c.Create(ctx, &v1alpha1.Application{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "web"},
		Spec: v1alpha1.ApplicationSpec{Project: "default", Source: &v1alpha1.ApplicationSource{RepoURL: "https://example.com/r", Path: "p"},
			Destination: v1alpha1.ApplicationDestination{Server: v1alpha1.KubernetesInternalAPIServerAddr, Namespace: "default"}},
	}))
	rh, err := reposerver.NewHandler(revisionRepo{}, reposerver.HandlerOptions{Token: "tok", Logger: slog.New(slog.DiscardHandler)})
	require.NoError(t, err)
	repoSrv := httptest.NewServer(rh)
	t.Cleanup(repoSrv.Close)
	h2, stop2, err := NewCoreHandler(ctx, CoreConfig{
		Namespace: ns, RestConfig: denied, RepoServerURL: repoSrv.URL, TreeAPI: TreeAPIClientOptions{URL: "http://127.0.0.1:1"},
		InternalToken: func(context.Context) (string, error) { return "tok", nil },
	})
	require.NoError(t, err)
	defer stop2()
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/applications/web/sync", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	h2.ServeHTTP(rec, req)
	require.Less(t, rec.Code, 300, rec.Body.String())
	var ops v1alpha1.OperationList
	require.NoError(t, c.List(ctx, &ops, client.InNamespace(ns)))
	require.Len(t, ops.Items, 1)
	assert.Empty(t, ops.Items[0].Spec.InitiatedBy.Username)
	assert.False(t, ops.Items[0].Spec.InitiatedBy.Automated)
}

// revisionRepo resolves every revision to a fixed commit.
type revisionRepo struct{ reposerverapi.Client }

func (revisionRepo) ResolveRevision(context.Context, *reposerverapi.ResolveRevisionRequest) (*reposerverapi.ResolveRevisionResponse, error) {
	return &reposerverapi.ResolveRevisionResponse{Revision: strings.Repeat("a", 40)}, nil
}

// denySelfSubjectReviews answers SelfSubjectReview requests with 403.
type denySelfSubjectReviews struct{ next http.RoundTripper }

func (d denySelfSubjectReviews) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Path, "/selfsubjectreviews") {
		return &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{"Content-Type": {"application/json"}}, Request: r,
			Body: io.NopCloser(strings.NewReader(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","code":403,"message":"selfsubjectreviews is forbidden"}`))}, nil
	}
	return d.next.RoundTrip(r)
}

func (d denySelfSubjectReviews) WrappedRoundTripper() http.RoundTripper { return d.next }

// TestCoreRoutesMatchTheServer: the in-process server registers every documented route of
// patrol-server except the proxy extension routes (disabled in core mode, as upstream) and
// the SCM webhook receivers.
func TestCoreRoutesMatchTheServer(t *testing.T) {
	requireEnvtest(t)
	kube, err := kubernetes.NewForConfig(testCfg)
	require.NoError(t, err)
	ns := newNamespace(t, kube)
	a, err := newCoreAssembly(t.Context(), CoreConfig{Namespace: ns, RestConfig: krest.CopyConfig(testCfg), RepoServerURL: "http://127.0.0.1:1", TreeAPI: TreeAPIClientOptions{URL: "http://127.0.0.1:1"}})
	require.NoError(t, err)
	defer a.close()
	key := func(r rest.Route) string { return r.Method + " " + r.Path }
	keys := func(services ...apiserver.Service) map[string]bool {
		tbl, err := apiserver.NewRouteTable(services...)
		require.NoError(t, err)
		out := map[string]bool{}
		for _, r := range tbl.Routes() {
			out[key(r)] = true
		}
		return out
	}
	excluded := keys(extensions.New(extensions.Deps{}), webhooks.New())
	for k := range keys() {
		delete(excluded, k) // core routes
	}
	got := map[string]bool{}
	for _, r := range a.api.Routes() {
		got[key(r)] = true
	}
	require.NotEmpty(t, excluded)
	require.Greater(t, len(got), 100)
	for k := range keys(apiserver.DocumentedServices()...) {
		if excluded[k] {
			assert.False(t, got[k], "core mode must not serve %s", k)
			continue
		}
		assert.True(t, got[k], "core mode lacks %s", k)
	}
}

func TestDiagnosticWriter(t *testing.T) {
	t.Parallel()
	problem := func(w http.ResponseWriter, status int, detail string) {
		w.Header().Set("Content-Type", apiclient.ProblemContentType)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"status":` + strconv.Itoa(status) + `,"detail":"` + detail + `"}`))
	}
	run := func(diags []string, h func(w http.ResponseWriter)) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		d := &coreDiagnostics{}
		for _, m := range diags {
			d.add(m)
		}
		w := &diagnosticWriter{ResponseWriter: rec, diag: d}
		h(w)
		w.finish()
		return rec
	}
	rec := run([]string{`secrets "x" is forbidden: User "u"`, `secrets "x" is forbidden: User "u"`}, func(w http.ResponseWriter) { problem(w, 403, "permission denied") })
	assert.Equal(t, 403, rec.Code)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &doc))
	assert.Equal(t, `permission denied: secrets "x" is forbidden: User "u"`, doc["detail"], "deduplicated")
	assert.Equal(t, strconv.Itoa(rec.Body.Len()), rec.Header().Get("Content-Length"))

	rec = run(nil, func(w http.ResponseWriter) { problem(w, 404, "missing") })
	assert.Contains(t, rec.Body.String(), `"detail":"missing"`, "no diagnostics: unchanged")
	rec = run([]string{"x"}, func(w http.ResponseWriter) { _, _ = w.Write([]byte("ok")) })
	assert.Equal(t, "ok", rec.Body.String(), "success responses are never held")

	big := strings.Repeat("a", maxCoreProblemBytes)
	rec = run([]string{"x"}, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", apiclient.ProblemContentType)
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"detail":"`))
		_, _ = w.Write([]byte(big))
		_, _ = w.Write([]byte(`"}`))
	})
	assert.Equal(t, 500, rec.Code)
	assert.Equal(t, len(big)+len(`{"detail":""}`), rec.Body.Len(), "an oversized document streams through unchanged")

	d := &coreDiagnostics{}
	for i := range 10 {
		d.add(strings.Repeat(strconv.Itoa(i), 2*maxCoreDiagnosticBytes))
	}
	assert.Len(t, d.list(), maxCoreDiagnostics)
	assert.LessOrEqual(t, len(d.list()[0]), maxCoreDiagnosticBytes+len("…"))
}
