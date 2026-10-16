package extensions_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/extensions"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/internal/miscapitest"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

func TestMain(m *testing.M) {
	os.Exit(miscapitest.Main(m, miscapitest.CRDDir(4)))
}

const (
	inCluster  = "https://kubernetes.default.svc"
	edgeServer = "https://edge.example:6443"
	farServer  = "https://far.example:6443"
	apiKey     = "s3cret-api-key-value"
)

// recorded is one request seen by a backend.
type recorded struct {
	method, path, rawPath, query, host, body string
	header                                   http.Header
}

// backend is an extension backend recording what it receives.
type backend struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []recorded
	// handle answers requests (default: 200 "ok <name>").
	handle func(w http.ResponseWriter, r *http.Request)
	name   string
}

func newBackend(t *testing.T, name string) *backend {
	t.Helper()
	b := &backend{name: name}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		b.mu.Lock()
		b.reqs = append(b.reqs, recorded{method: r.Method, path: r.URL.Path, rawPath: r.URL.RawPath, query: r.URL.RawQuery, host: r.Host, body: string(body), header: r.Header.Clone()})
		h := b.handle
		b.mu.Unlock()
		if h != nil {
			h(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprintf(w, "ok %s", name)
	}))
	t.Cleanup(b.srv.Close)
	return b
}

func (b *backend) last(t *testing.T) recorded {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	require.NotEmpty(t, b.reqs, "backend %s received no request", b.name)
	return b.reqs[len(b.reqs)-1]
}

func (b *backend) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.reqs)
}

func (b *backend) setHandler(h func(w http.ResponseWriter, r *http.Request)) {
	b.mu.Lock()
	b.handle = h
	b.mu.Unlock()
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type fixture struct {
	*miscapitest.Env
	tenant, disabled  string
	metrics, in, edge *backend
	single            *backend
	logs              *syncBuffer
}

func extensionConfig(metrics, in, edge string) string {
	return fmt.Sprintf(`extensions:
- name: metrics
  backend:
    connectionTimeout: 1s
    services:
    - url: %s
      headers:
      - name: X-Api-Key
        value: $metrics-api-key
- name: multi
  backend:
    services:
    - url: %s
      cluster:
        server: %s
    - url: %s
      cluster:
        name: edge
`, metrics, in, inCluster, edge)
}

func newFixture(t *testing.T, configure func(*extensions.Config)) *fixture {
	t.Helper()
	f := &fixture{
		tenant: miscapitest.Unique("apps"), disabled: miscapitest.Unique("off"),
		metrics: newBackend(t, "metrics"), in: newBackend(t, "in"), edge: newBackend(t, "edge"), single: newBackend(t, "single"),
		logs: &syncBuffer{},
	}
	cm := miscapitest.Accounts("alice", "noinvoke", "noapp", "admin", "nobody")
	cm["extension.config"] = extensionConfig(f.metrics.srv.URL, f.in.srv.URL, f.edge.srv.URL)
	cm["extension.config.single"] = "services:\n- url: " + f.single.srv.URL + "\n"
	f.Env = miscapitest.New(t, miscapitest.Options{
		Kube:   true,
		CM:     cm,
		Secret: map[string]string{"metrics-api-key": apiKey},
		RBAC: miscapitest.Policy(
			"p, alice, applications, get, team-a/*, allow",
			"p, alice, applications, get, team-b/*, allow",
			"p, alice, extensions, invoke, metrics, allow",
			"p, alice, extensions, invoke, multi, allow",
			"p, alice, extensions, invoke, single, allow",
			"p, alice, extensions, invoke, ghost, allow",
			"p, noinvoke, applications, get, team-a/*, allow",
			"p, noapp, extensions, invoke, *, allow",
			"g, admin, role:admin",
			"g, sso-devs, role:ext-users",
			"p, role:ext-users, applications, get, team-a/*, allow",
			"p, role:ext-users, extensions, invoke, metrics, allow",
		),
	}, func(env *miscapitest.Env) []miscapitest.Registrar {
		cfg := extensions.Config{Enabled: true, Namespace: env.Namespace, ApplicationNamespaces: []string{f.tenant}}
		if configure != nil {
			configure(&cfg)
		}
		return []miscapitest.Registrar{extensions.New(extensions.Deps{
			Config: cfg, Settings: env.Settings, Reader: env.Kube, Registry: env.Registry,
			Logger: slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		})}
	})
	f.CreateNamespace(f.tenant)
	f.CreateNamespace(f.disabled)
	ctx := context.Background()
	for _, p := range []*v1alpha1.AppProject{
		{ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: f.Namespace}, Spec: v1alpha1.AppProjectSpec{
			SourceRepos:      []string{"*"},
			SourceNamespaces: []string{f.tenant},
			Destinations:     []v1alpha1.ApplicationDestination{{Server: inCluster, Namespace: "*"}, {Name: "edge", Namespace: "*"}, {Name: "far", Namespace: "*"}},
		}},
		{ObjectMeta: metav1.ObjectMeta{Name: "team-b", Namespace: f.Namespace}, Spec: v1alpha1.AppProjectSpec{
			SourceRepos:  []string{"*"},
			Destinations: []v1alpha1.ApplicationDestination{{Server: "https://nowhere.example", Namespace: "*"}},
		}},
	} {
		require.NoError(t, f.Kube.Create(ctx, p))
	}
	for _, c := range []*v1alpha1.Cluster{
		{ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: f.Namespace}, Spec: v1alpha1.ClusterSpec{Name: "edge", Server: edgeServer}},
		{ObjectMeta: metav1.ObjectMeta{Name: "far", Namespace: f.Namespace}, Spec: v1alpha1.ClusterSpec{Name: "far", Server: farServer}},
	} {
		require.NoError(t, f.Kube.Create(ctx, c))
	}
	f.app(f.Namespace, "guestbook", "team-a", v1alpha1.ApplicationDestination{Server: inCluster, Namespace: "gb"})
	f.app(f.Namespace, "edge-app", "team-a", v1alpha1.ApplicationDestination{Name: "edge", Namespace: "gb"})
	f.app(f.Namespace, "edge-by-server", "team-a", v1alpha1.ApplicationDestination{Server: edgeServer, Namespace: "gb"})
	f.app(f.Namespace, "far-app", "team-a", v1alpha1.ApplicationDestination{Name: "far", Namespace: "gb"})
	f.app(f.Namespace, "bad-dest", "team-b", v1alpha1.ApplicationDestination{Server: inCluster, Namespace: "gb"})
	f.app(f.tenant, "tenant-app", "team-a", v1alpha1.ApplicationDestination{Server: inCluster, Namespace: "gb"})
	f.app(f.tenant, "tenant-b", "team-b", v1alpha1.ApplicationDestination{Server: "https://nowhere.example", Namespace: "gb"})
	f.app(f.disabled, "hidden", "team-a", v1alpha1.ApplicationDestination{Server: inCluster, Namespace: "gb"})
	return f
}

func (f *fixture) app(ns, name, project string, dest v1alpha1.ApplicationDestination) {
	f.T.Helper()
	require.NoError(f.T, f.Kube.Create(context.Background(), &v1alpha1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v1alpha1.ApplicationSpec{
			Project:     project,
			Source:      &v1alpha1.ApplicationSource{RepoURL: "https://git.example.com/a.git", Path: "."},
			Destination: dest,
		},
	}))
}

// call is one raw proxy request.
type call struct {
	method, path, token, app, project, body string
	header                                  http.Header
}

func (f *fixture) do(t *testing.T, c call) (*http.Response, string) {
	t.Helper()
	method := c.method
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader
	if c.body != "" {
		body = strings.NewReader(c.body)
	}
	req, err := http.NewRequest(method, f.URL+c.path, body)
	require.NoError(t, err)
	for k, vs := range c.header {
		req.Header[k] = vs
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.app != "" {
		req.Header.Set(apiclient.HeaderExtensionApplication, c.app)
	}
	if c.project != "" {
		req.Header.Set(apiclient.HeaderExtensionProject, c.project)
	}
	hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := hc.Do(req)
	require.NoError(t, err)
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp, string(b)
}

func (f *fixture) guestbook(token string) call {
	return call{path: "/extensions/metrics/api/v1/query", token: token, app: f.Namespace + ":guestbook", project: "team-a"}
}

func TestProxyForwardsWithHeaderContract(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	tok := f.LoginToken("alice")
	f.metrics.setHandler(func(w http.ResponseWriter, _ *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "patrol.token", Value: "planted"})
		w.Header().Set("X-Backend", "yes")
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"result":"ok"}`)
	})
	c := f.guestbook(tok)
	c.method = http.MethodPost
	c.path = "/extensions/metrics/api/v1/query%2Fraw/x?q=up&step=1"
	c.body = `{"query":"up"}`
	c.header = http.Header{
		"Content-Type":              {"application/json"},
		"Accept":                    {"application/json"},
		"Cookie":                    {"patrol.token=" + tok + "; other=1"},
		"X-Forwarded-For":           {"6.6.6.6"},
		"X-Custom":                  {"not-allow-listed"},
		"Patrol-Username":           {"mallory"},
		"Patrol-User-Groups":        {"role:admin"},
		"Patrol-Target-Cluster-Url": {"https://evil.example"},
		"Proxy-Authorization":       {"Basic Zm9vOmJhcg=="},
		"X-Requested-With":          {"patrol"},
		"Connection":                {"Accept-Language"},
		"Accept-Language":           {"declared-hop-by-hop"},
	}
	resp, body := f.do(t, c)
	require.Equal(t, http.StatusAccepted, resp.StatusCode, body)
	require.JSONEq(t, `{"result":"ok"}`, body)
	require.Equal(t, "yes", resp.Header.Get("X-Backend"))
	require.Empty(t, resp.Header.Values("Set-Cookie"), "backends cannot plant cookies")

	got := f.metrics.last(t)
	require.Equal(t, http.MethodPost, got.method)
	require.Equal(t, "/api/v1/query/raw/x", got.path)
	require.Equal(t, "/api/v1/query%2Fraw/x", got.rawPath, "escaping preserved")
	require.Equal(t, "q=up&step=1", got.query)
	require.Equal(t, `{"query":"up"}`, got.body)
	require.Equal(t, strings.TrimPrefix(f.metrics.srv.URL, "http://"), got.host)
	h := got.header
	require.Equal(t, apiKey, h.Get("X-Api-Key"), "configured header resolved from patrol-secret")
	require.Equal(t, f.Namespace, h.Get(apiclient.HeaderExtensionNamespace))
	require.Equal(t, inCluster, h.Get(apiclient.HeaderExtensionTargetClusterURL))
	require.Empty(t, h.Get(apiclient.HeaderExtensionTargetClusterName))
	require.Equal(t, "alice", h.Get(apiclient.HeaderExtensionUserID))
	require.Equal(t, "alice", h.Get(apiclient.HeaderExtensionUsername))
	require.Empty(t, h.Get(apiclient.HeaderExtensionUserGroups))
	require.Equal(t, "application/json", h.Get("Content-Type"))
	require.Equal(t, "application/json", h.Get("Accept"))
	require.Equal(t, f.Namespace+":guestbook", h.Get(apiclient.HeaderExtensionApplication))
	require.NotEmpty(t, h.Get(rest.RequestIDHeader))
	for _, gone := range []string{"Authorization", "Cookie", "X-Forwarded-For", "X-Custom", "Proxy-Authorization", "X-Requested-With", "Accept-Language"} {
		require.Empty(t, h.Values(gone), gone)
	}

	// Audit entry, metrics, and no secret in the logs.
	entries := f.Audit.ByOperation("invokeExtensionPost")
	require.Len(t, entries, 1)
	require.Equal(t, rest.AuditSucceeded, entries[0].Result)
	require.Equal(t, []string{"extensions", "invoke", "metrics"}, []string{entries[0].Resource, entries[0].Action, entries[0].Object})
	require.Equal(t, 1.0, f.requests(t, "metrics", "202"))
	require.NotContains(t, f.logs.String(), apiKey)
	require.NotContains(t, f.logs.String(), tok)
}

func (f *fixture) requests(t *testing.T, ext, status string) float64 {
	t.Helper()
	mfs, err := f.Registry.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() != "patrol_server_extension_requests_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			l := map[string]string{}
			for _, lp := range m.GetLabel() {
				l[lp.GetName()] = lp.GetValue()
			}
			if l["extension"] == ext && l["status"] == status {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func TestProxyGoClientAndSSOIdentity(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	tok := f.SSOToken("user-123", "jane@example.com", "sso-devs", "other")
	resp, err := f.ClientWithToken(tok).InvokeExtension(context.Background(), &apiclient.ExtensionCall{
		Extension: "metrics", Path: "/healthz", AppNamespace: f.Namespace, AppName: "guestbook", Project: "team-a",
	})
	require.NoError(t, err)
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "ok metrics", string(b))
	h := f.metrics.last(t).header
	require.Equal(t, "user-123", h.Get(apiclient.HeaderExtensionUserID))
	require.Equal(t, "jane@example.com", h.Get(apiclient.HeaderExtensionUsername))
	require.Equal(t, "sso-devs,other", h.Get(apiclient.HeaderExtensionUserGroups))
	require.Equal(t, "/healthz", f.metrics.last(t).path)
}

func TestProxyRoutesByDestination(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	tok := f.LoginToken("alice")
	cases := []struct {
		app          string
		want         *backend
		server, name string
	}{
		{"guestbook", f.in, inCluster, ""},
		{"edge-app", f.edge, "", "edge"},
		{"edge-by-server", f.edge, edgeServer, ""}, // resolved cluster name selects the service (Patrol addition)
	}
	for _, tc := range cases {
		t.Run(tc.app, func(t *testing.T) {
			before := tc.want.count()
			resp, body := f.do(t, call{path: "/extensions/multi/x", token: tok, app: f.Namespace + ":" + tc.app, project: "team-a"})
			require.Equal(t, http.StatusOK, resp.StatusCode, body)
			require.Equal(t, "ok "+tc.want.name, body)
			require.Equal(t, before+1, tc.want.count())
			h := tc.want.last(t).header
			require.Equal(t, tc.server, h.Get(apiclient.HeaderExtensionTargetClusterURL))
			require.Equal(t, tc.name, h.Get(apiclient.HeaderExtensionTargetClusterName))
		})
	}
	// A destination no service is configured for.
	resp, body := f.do(t, call{path: "/extensions/multi/x", token: tok, app: f.Namespace + ":far-app", project: "team-a"})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Contains(t, body, "invalid extension")
	// Single-service extension (extension.config.<name>) serves every destination.
	resp, body = f.do(t, call{path: "/extensions/single/", token: tok, app: f.Namespace + ":far-app", project: "team-a"})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.Equal(t, "ok single", body)
	require.Equal(t, "/", f.single.last(t).path)
	// Tenant namespace Application.
	resp, body = f.do(t, call{path: "/extensions/metrics/q", token: tok, app: f.tenant + ":tenant-app", project: "team-a"})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
}

func TestProxyAuthorizationIsUniform(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	alice := f.LoginToken("alice")
	cases := []struct {
		name string
		c    call
	}{
		{"no extensions invoke", call{token: f.LoginToken("noinvoke"), app: f.Namespace + ":guestbook", project: "team-a"}},
		{"no applications get", call{token: f.LoginToken("noapp"), app: f.Namespace + ":guestbook", project: "team-a"}},
		{"no role at all", call{token: f.LoginToken("nobody"), app: f.Namespace + ":guestbook", project: "team-a"}},
		{"missing application", call{token: alice, app: f.Namespace + ":nope", project: "team-a"}},
		{"project mismatch", call{token: alice, app: f.Namespace + ":bad-dest", project: "team-a"}},
		{"header project differs from the application", call{token: alice, app: f.Namespace + ":guestbook", project: "team-b"}},
		{"destination not permitted", call{token: alice, app: f.Namespace + ":bad-dest", project: "team-b"}},
		{"namespace not permitted by the project", call{token: alice, app: f.tenant + ":tenant-b", project: "team-b"}},
		{"namespace not enabled", call{token: alice, app: f.disabled + ":hidden", project: "team-a"}},
		{"project token cannot invoke", call{token: projectToken(t, f), app: f.Namespace + ":guestbook", project: "team-a"}},
	}
	before := f.metrics.count()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.c.path = "/extensions/metrics/x"
			resp, body := f.do(t, tc.c)
			require.Equal(t, http.StatusForbidden, resp.StatusCode, body)
			require.Contains(t, body, `"detail":"Unauthorized extension request"`)
			require.Contains(t, body, `"code":"permission_denied"`)
		})
	}
	require.Equal(t, before, f.metrics.count(), "nothing reaches the backend")
	// Unauthenticated.
	resp, _ := f.do(t, call{path: "/extensions/metrics/x", app: f.Namespace + ":guestbook", project: "team-a"})
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	// Denials and failures are audited.
	var denied, failed int
	for _, e := range f.Audit.ByOperation("invokeExtensionGet") {
		switch e.Result {
		case rest.AuditDenied:
			denied++
		case rest.AuditFailed:
			failed++
		}
	}
	require.GreaterOrEqual(t, denied, 3)
	require.GreaterOrEqual(t, failed, 6)
}

func projectToken(t *testing.T, f *fixture) string {
	t.Helper()
	c := f.ProjectClient("team-a", "ci")
	return c.Token()
}

func TestProxyHeaderValidationAndUnknownExtension(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	tok := f.LoginToken("alice")
	cases := []struct {
		name, app, project, detail string
	}{
		{"missing application header", "", "team-a", `header "Patrol-Application-Name" must be provided`},
		{"no namespace separator", "guestbook", "team-a", "expected format: <namespace>:<app-name>"},
		{"too many separators", "a:b:c", "team-a", "expected format"},
		{"invalid namespace", "Bad_NS:guestbook", "team-a", "invalid value for namespace"},
		{"empty namespace", ":guestbook", "team-a", "invalid value for namespace"},
		{"invalid name", f.Namespace + ":Bad Name", "team-a", "invalid value for application name"},
		{"missing project", f.Namespace + ":guestbook", "", `header "Patrol-Project-Name" must be provided`},
		{"invalid project", f.Namespace + ":guestbook", "a/b", "invalid value for project name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := f.do(t, call{path: "/extensions/metrics/x", token: tok, app: tc.app, project: tc.project})
			require.Equal(t, http.StatusBadRequest, resp.StatusCode, body)
			var p apiclient.Problem
			require.NoError(t, json.Unmarshal([]byte(body), &p))
			require.True(t, strings.HasPrefix(p.Detail, "Invalid headers: "), p.Detail)
			require.Contains(t, p.Detail, tc.detail)
		})
	}
	resp, body := f.do(t, call{path: "/extensions/bad.name/x", token: tok, app: f.Namespace + ":guestbook", project: "team-a"})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, body)
	// An unregistered extension the caller may invoke: 404; one it may not: the same 403.
	resp, body = f.do(t, call{path: "/extensions/ghost/x", token: tok, app: f.Namespace + ":guestbook", project: "team-a"})
	require.Equal(t, http.StatusNotFound, resp.StatusCode, body)
	require.Contains(t, body, "Extension not found")
	resp, _ = f.do(t, call{path: "/extensions/phantom/x", token: tok, app: f.Namespace + ":guestbook", project: "team-a"})
	require.Equal(t, http.StatusForbidden, resp.StatusCode, "existence of extensions is not revealed")
}

func TestProxyDisabled(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(c *extensions.Config) { c.Enabled = false })
	resp, body := f.do(t, f.guestbook(f.LoginToken("admin")))
	require.Equal(t, http.StatusNotFound, resp.StatusCode, body)
	require.Contains(t, body, "proxy extensions are not enabled")
	require.Zero(t, f.metrics.count())
}

func TestProxyConfigReloadWithoutRestart(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	tok := f.LoginToken("alice")
	resp, body := f.do(t, f.guestbook(tok))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "ok metrics", body)

	other := newBackend(t, "other")
	f.Settings.UpdateCM(func(cm map[string]string) {
		cm["extension.config"] = extensionConfig(other.srv.URL, f.in.srv.URL, f.edge.srv.URL)
	})
	resp, body = f.do(t, f.guestbook(tok))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "ok other", body)
	require.Equal(t, apiKey, other.last(t).header.Get("X-Api-Key"))

	// A rotated Secret value is applied too.
	f.Settings.UpdateSecret(func(sec map[string]string) { sec["metrics-api-key"] = "rotated-key" })
	_, _ = f.do(t, f.guestbook(tok))
	require.Equal(t, "rotated-key", other.last(t).header.Get("X-Api-Key"))

	// A backend URL that is not http(s): the extension is not registered.
	f.Settings.UpdateCM(func(cm map[string]string) {
		cm["extension.config"] = "extensions:\n- name: metrics\n  backend:\n    services:\n    - url: file:///etc/passwd\n"
	})
	resp, body = f.do(t, f.guestbook(tok))
	require.Equal(t, http.StatusNotFound, resp.StatusCode, body)
	resp, body = f.do(t, call{path: "/extensions/single/x", token: tok, app: f.Namespace + ":guestbook", project: "team-a"})
	require.Equal(t, http.StatusOK, resp.StatusCode, "other extensions keep working: %s", body)
}

func TestProxyLimitsAndFailures(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(c *extensions.Config) {
		c.MaxRequestBytes = 1024
		c.MaxResponseBytes = 1024
		c.ResponseHeaderTimeout = 300 * time.Millisecond
	})
	tok := f.LoginToken("alice")

	// Request body over the cap.
	c := f.guestbook(tok)
	c.method, c.body = http.MethodPost, strings.Repeat("x", 4096)
	resp, body := f.do(t, c)
	require.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode, body)

	// Response with a Content-Length over the cap.
	f.metrics.setHandler(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "4096")
		_, _ = w.Write(bytes.Repeat([]byte("y"), 4096))
	})
	resp, body = f.do(t, f.guestbook(tok))
	require.Equal(t, http.StatusBadGateway, resp.StatusCode, body)
	require.Contains(t, body, "exceeds the size limit")

	// Streamed response over the cap: the connection is aborted, never completed.
	f.metrics.setHandler(func(w http.ResponseWriter, _ *http.Request) {
		for range 8 {
			_, _ = w.Write(bytes.Repeat([]byte("z"), 512))
			_ = http.NewResponseController(w).Flush()
		}
	})
	req, err := http.NewRequest(http.MethodGet, f.URL+"/extensions/metrics/stream", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set(apiclient.HeaderExtensionApplication, f.Namespace+":guestbook")
	req.Header.Set(apiclient.HeaderExtensionProject, "team-a")
	sresp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	got, rerr := io.ReadAll(sresp.Body)
	_ = sresp.Body.Close()
	require.True(t, rerr != nil || len(got) < 4096, "truncated response must not look complete")

	// Backend too slow to answer.
	f.metrics.setHandler(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(3 * time.Second):
		case <-r.Context().Done():
		}
	})
	resp, body = f.do(t, f.guestbook(tok))
	require.Equal(t, http.StatusGatewayTimeout, resp.StatusCode, body)
	require.Contains(t, body, "did not respond in time")

	// Redirects are returned to the caller, never followed by the server.
	f.metrics.setHandler(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data", http.StatusFound)
	})
	before := f.metrics.count()
	resp, _ = f.do(t, f.guestbook(tok))
	require.Equal(t, http.StatusFound, resp.StatusCode)
	require.Equal(t, before+1, f.metrics.count())

	// Unreachable backend.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	dead := "http://" + l.Addr().String()
	require.NoError(t, l.Close())
	f.Settings.UpdateCM(func(cm map[string]string) {
		cm["extension.config"] = extensionConfig(dead, f.in.srv.URL, f.edge.srv.URL)
	})
	resp, body = f.do(t, f.guestbook(tok))
	require.Equal(t, http.StatusBadGateway, resp.StatusCode, body)
	require.Contains(t, body, "the extension backend is unavailable")
	require.NotContains(t, body, l.Addr().String(), "backend addresses are not revealed")
	require.NotContains(t, f.logs.String(), apiKey)
}

func TestProxyCookieRequiresCSRFHeader(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	tok := f.LoginToken("alice")
	c := call{method: http.MethodPost, path: "/extensions/metrics/x", app: f.Namespace + ":guestbook", project: "team-a", body: "{}",
		header: http.Header{"Cookie": {"patrol.token=" + tok}, "Content-Type": {"application/json"}}}
	resp, body := f.do(t, c)
	require.Equal(t, http.StatusForbidden, resp.StatusCode, body)
	require.Contains(t, body, "X-Requested-With")
	c.header.Set("X-Requested-With", "patrol")
	resp, body = f.do(t, c)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.Empty(t, f.metrics.last(t).header.Get("Cookie"), "the session cookie is never forwarded")
}

func TestProxyWebSocket(t *testing.T) {
	t.Parallel()
	echo := func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		typ, msg, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		_ = conn.Write(r.Context(), typ, append([]byte("echo: "), msg...))
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}
	dial := func(f *fixture) (*websocket.Conn, *http.Response, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		h := http.Header{}
		h.Set("Authorization", "Bearer "+f.LoginToken("alice"))
		h.Set(apiclient.HeaderExtensionApplication, f.Namespace+":guestbook")
		h.Set(apiclient.HeaderExtensionProject, "team-a")
		return websocket.Dial(ctx, "ws"+strings.TrimPrefix(f.URL, "http")+"/extensions/metrics/ws", &websocket.DialOptions{HTTPHeader: h})
	}

	off := newFixture(t, nil)
	off.metrics.setHandler(echo)
	_, resp, err := dial(off)
	require.Error(t, err)
	require.NotNil(t, resp)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, "WebSockets are off by default")
	require.Zero(t, off.metrics.count())

	on := newFixture(t, func(c *extensions.Config) { c.EnableWebSockets = true })
	on.metrics.setHandler(echo)
	conn, _, err := dial(on)
	require.NoError(t, err)
	defer func() { _ = conn.CloseNow() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte("hi")))
	_, msg, err := conn.Read(ctx)
	require.NoError(t, err)
	require.Equal(t, "echo: hi", string(msg))
	require.Empty(t, on.metrics.last(t).header.Get("Authorization"))
}

func TestUnavailableWithoutDependencies(t *testing.T) {
	t.Parallel()
	env := miscapitest.New(t, miscapitest.Options{CM: miscapitest.Accounts("admin"), RBAC: miscapitest.Policy("g, admin, role:admin")},
		func(env *miscapitest.Env) []miscapitest.Registrar {
			return []miscapitest.Registrar{extensions.New(extensions.Deps{Config: extensions.Config{Enabled: true, Namespace: env.Namespace}})}
		})
	req, err := http.NewRequest(http.MethodGet, env.URL+"/extensions/x/y", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+env.LoginToken("admin"))
	req.Header.Set(apiclient.HeaderExtensionApplication, "patrol:app")
	req.Header.Set(apiclient.HeaderExtensionProject, "p")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}
