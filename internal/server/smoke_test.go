package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	krest "k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/auth"
	"github.com/juliosaraiva/patrol-cd/internal/cmp"
	"github.com/juliosaraiva/patrol-cd/internal/controller/sharding"
	notifcontroller "github.com/juliosaraiva/patrol-cd/internal/notifications/controller"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

// One envtest API server (with the generated CRDs) per test binary, started only when
// KUBEBUILDER_ASSETS is set; the smoke tests skip without it (and fail with
// PATROL_REQUIRE_ENVTEST=1). Each test uses its own namespace.
var (
	testCfg        *krest.Config
	testKubeconfig []byte
)

func TestMain(m *testing.M) {
	DispatchHelpers()
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	ctrllog.SetLogger(logr.Discard())
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		if os.Getenv("PATROL_REQUIRE_ENVTEST") == "1" {
			fmt.Fprintln(os.Stderr, "PATROL_REQUIRE_ENVTEST=1 but KUBEBUILDER_ASSETS is not set")
			return 1
		}
		return m.Run()
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "deploy", "crds")},
		ErrorIfCRDPathMissing: true,
	}
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
	testCfg, testKubeconfig = cfg, env.KubeConfig
	return m.Run()
}

func requireEnvtest(t *testing.T) {
	t.Helper()
	if testCfg == nil {
		t.Skip("KUBEBUILDER_ASSETS is not set")
	}
}

// syncBuffer collects logs; it is printed when a test fails.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	b := &syncBuffer{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("component logs:\n%s", b.String())
		}
	})
	return slog.New(slog.NewTextHandler(b, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

func newNamespace(t *testing.T, kube kubernetes.Interface) string {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "wiring-"}}
	created, err := kube.CoreV1().Namespaces().Create(context.Background(), ns, metav1.CreateOptions{})
	require.NoError(t, err)
	return created.Name
}

// running runs fn in the background and returns a stop function that cancels it and
// requires a clean return within the drain bound.
func running(t *testing.T, fn func(ctx context.Context) error) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- fn(ctx) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				require.NoError(t, err, "clean shutdown")
			case <-time.After(30 * time.Second):
				t.Fatal("component did not stop within 30s of cancellation")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

// waitAddr waits for an address published by a component.
func waitAddr[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(60 * time.Second):
		t.Fatal("component did not start listening within 60s")
	}
	var zero T
	return zero
}

var insecureClient = &http.Client{
	Timeout:   10 * time.Second,
	Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // self-signed test listeners
}

// eventually polls url until it answers want (60s).
func eventually(t *testing.T, url string, want int) string {
	t.Helper()
	var last string
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := insecureClient.Get(url)
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == want {
				return string(b)
			}
			last = fmt.Sprintf("%d %s", resp.StatusCode, b)
		} else {
			last = err.Error()
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("GET %s never answered %d; last: %s", url, want, last)
	return ""
}

func httpDo(t *testing.T, method, url string, hdr map[string]string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	require.NoError(t, err)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := insecureClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(b)
}

const smokeToken = "smoke-internal-token-0123456789"

// unusedRepoServer is an address nothing listens on: the components must start without
// reaching the repo-server.
const unusedRepoServer = "127.0.0.1:1"

func TestServerRunSmoke(t *testing.T) {
	requireEnvtest(t)
	kube := kubernetes.NewForConfigOrDie(testCfg)
	ns := newNamespace(t, kube)
	ext := t.TempDir()
	writeFile(t, filepath.Join(ext, "demo", "extension-demo.js"), "window.demo = 1")
	type addrs struct{ api, metrics net.Addr }
	ch := make(chan addrs, 1)
	cfg := Config{
		Namespace: ns, RestConfig: testCfg,
		ListenAddress: "127.0.0.1:0", MetricsAddress: "127.0.0.1:0", Insecure: true,
		InternalToken: smokeToken,
		RepoServer:    RepoServerClientOptions{Address: unusedRepoServer, TLS: TLSClientOptions{Plaintext: true}},
		TreeAPI:       TreeAPIClientOptions{URL: "http://" + unusedRepoServer, TLS: TLSClientOptions{Plaintext: true}},
		ExtensionsDir: ext, DownloadDir: t.TempDir(),
		SyncWithReplaceAllowed: true,
		ShutdownTimeout:        5 * time.Second,
		Logger:                 testLogger(t),
		OnListening:            func(api, metrics net.Addr) { ch <- addrs{api, metrics} },
	}
	stop := running(t, func(ctx context.Context) error { return Run(ctx, cfg) })
	a := waitAddr(t, ch)
	api, metrics := "http://"+a.api.String(), "http://"+a.metrics.String()

	eventually(t, api+"/healthz", http.StatusOK)
	eventually(t, metrics+"/healthz", http.StatusOK)
	assert.Contains(t, eventually(t, api+"/api/version", http.StatusOK), `"version"`)
	assert.Contains(t, eventually(t, api+"/extensions.js", http.StatusOK), "window.demo = 1")
	code, body := httpDo(t, http.MethodGet, api+"/", map[string]string{"Accept": "text/html"})
	assert.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "Patrol CD")
	code, body = httpDo(t, http.MethodPost, api+"/api/webhook", nil)
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Contains(t, body, "Unknown webhook event", "the request reached the webhook handler")
	code, _ = httpDo(t, http.MethodGet, api+"/api/v1/applications", nil)
	assert.Equal(t, http.StatusUnauthorized, code, "resource routes require authentication")
	code, body = httpDo(t, http.MethodGet, metrics+"/metrics", nil)
	assert.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "go_goroutines")
	code, body = httpDo(t, http.MethodGet, metrics+"/debug/pprof/", nil)
	assert.Equal(t, http.StatusUnauthorized, code, "the profiler is off by default")
	assert.Contains(t, body, profilerDisabledMessage)

	// Startup bootstrap: session signing key (OPS-030) and default project (TEN-002).
	sec, err := kube.CoreV1().Secrets(ns).Get(context.Background(), settings.DefaultSecretName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.NotEmpty(t, sec.Data["server.secretkey"])
	c, err := client.New(testCfg, client.Options{Scheme: mustScheme(t)})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		var p v1alpha1.AppProject
		return c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "default"}, &p) == nil &&
			len(p.Spec.SourceRepos) == 1 && p.Spec.SourceRepos[0] == "*"
	}, 30*time.Second, 200*time.Millisecond)

	stop()
	_, err = insecureClient.Get(api + "/healthz")
	assert.Error(t, err, "the listener is closed after shutdown")
}

// defaultServerParams are the compiled patrol-server parameters (no ConfigMap, flags or
// environment).
func defaultServerParams(t *testing.T) *settings.Params {
	t.Helper()
	p, issues := settings.ResolveParams(settings.ComponentServer, nil, func(string) (string, bool) { return "", false })
	require.Empty(t, issues)
	return p
}

// getProto polls url with c until it answers 200 and returns the response (body closed).
func getProto(t *testing.T, c *http.Client, url string) *http.Response {
	t.Helper()
	var last error
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := c.Get(url)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return resp
			}
			last = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			last = err
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("GET %s never answered 200; last: %v", url, last)
	return nil
}

// TestServerRunDefaultTLS starts patrol-server with TLS and the compiled TLS parameters of
// `patrolcd server` (server.tls.* defaults, generated certificate) and completes TLS 1.3
// with HTTP/2 and TLS 1.2 handshakes (OPS-097). The upstream default cipher list alone
// made net/http refuse to serve HTTP/2, so the server exited at startup.
func TestServerRunDefaultTLS(t *testing.T) {
	requireEnvtest(t)
	kube := kubernetes.NewForConfigOrDie(testCfg)
	ns := newNamespace(t, kube)
	p := defaultServerParams(t)
	require.False(t, p.Bool("server.insecure"), "TLS is the default")
	type addrs struct{ api, metrics net.Addr }
	ch := make(chan addrs, 1)
	cfg := Config{
		Namespace: ns, RestConfig: testCfg,
		ListenAddress: "127.0.0.1:0", MetricsAddress: "127.0.0.1:0",
		Insecure:      p.Bool("server.insecure"),
		TLSMinVersion: p.String("server.tls.minversion"), TLSMaxVersion: p.String("server.tls.maxversion"),
		TLSCiphers:    p.String("server.tls.ciphers"),
		InternalToken: smokeToken,
		RepoServer:    RepoServerClientOptions{Address: unusedRepoServer, TLS: TLSClientOptions{Plaintext: true}},
		TreeAPI:       TreeAPIClientOptions{URL: "http://" + unusedRepoServer, TLS: TLSClientOptions{Plaintext: true}},
		ExtensionsDir: t.TempDir(), DownloadDir: t.TempDir(),
		ShutdownTimeout: 5 * time.Second,
		Logger:          testLogger(t),
		OnListening:     func(api, metrics net.Addr) { ch <- addrs{api, metrics} },
	}
	stop := running(t, func(ctx context.Context) error { return Run(ctx, cfg) })
	a := waitAddr(t, ch)
	url := "https://" + a.api.String() + "/healthz"

	client := func(tc *tls.Config) *http.Client {
		tc.InsecureSkipVerify = true //nolint:gosec // generated self-signed certificate
		return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: tc, ForceAttemptHTTP2: true}}
	}
	resp := getProto(t, client(&tls.Config{}), url)
	assert.Equal(t, uint16(tls.VersionTLS13), resp.TLS.Version)
	assert.Equal(t, "HTTP/2.0", resp.Proto, "HTTP/2 is negotiated")

	resp = getProto(t, client(&tls.Config{MaxVersion: tls.VersionTLS12}), url)
	assert.Equal(t, uint16(tls.VersionTLS12), resp.TLS.Version, "TLS 1.2 clients can use the generated certificate")
	assert.Equal(t, "HTTP/2.0", resp.Proto)
	assert.Equal(t, "TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256", tls.CipherSuiteName(resp.TLS.CipherSuite))

	// The cipher list applies: a TLS 1.2 client offering only other suites is refused.
	_, err := client(&tls.Config{MaxVersion: tls.VersionTLS12, CipherSuites: []uint16{tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256}}).Get(url)
	require.Error(t, err)
	stop()
}

// TestServerRunRootPath serves patrol-server under --rootpath with the default --basehref
// (OPS-105) and a static assets directory (OPS-111).
func TestServerRunRootPath(t *testing.T) {
	requireEnvtest(t)
	kube := kubernetes.NewForConfigOrDie(testCfg)
	ns := newNamespace(t, kube)
	const password = "smoke-admin-password"
	partOf := map[string]string{v1alpha1.LabelKeyPartOf: v1alpha1.LabelValuePartOf}
	hash, err := auth.HashPasswordWithCost(password, bcrypt.MinCost)
	require.NoError(t, err)
	_, err = kube.CoreV1().Secrets(ns).Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: settings.DefaultSecretName, Labels: partOf},
		Data: map[string][]byte{
			settings.SecretKeyAdminPassword:      []byte(hash),
			settings.SecretKeyAdminPasswordMtime: []byte(time.Now().UTC().Format(time.RFC3339)),
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	// Local accounts (admin) need patrol-cm to exist.
	_, err = kube.CoreV1().ConfigMaps(ns).Create(context.Background(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: settings.DefaultConfigMapName, Labels: partOf},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	static := t.TempDir()
	writeFile(t, filepath.Join(static, "custom.css"), "body { color: red }")
	secret := filepath.Join(t.TempDir(), "token")
	writeFile(t, secret, "outside-secret")
	require.NoError(t, os.Symlink(secret, filepath.Join(static, "leak.txt")))

	logs := &syncBuffer{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("server logs:\n%s", logs.String())
		}
	})
	p := defaultServerParams(t)
	type addrs struct{ api, metrics net.Addr }
	ch := make(chan addrs, 1)
	cfg := Config{
		Namespace: ns, RestConfig: testCfg,
		ListenAddress: "127.0.0.1:0", MetricsAddress: "127.0.0.1:0", Insecure: true,
		RootPath: "/patrol", BaseHRef: p.String("server.basehref"), StaticAssetsDir: static,
		InternalToken: smokeToken,
		RepoServer:    RepoServerClientOptions{Address: unusedRepoServer, TLS: TLSClientOptions{Plaintext: true}},
		TreeAPI:       TreeAPIClientOptions{URL: "http://" + unusedRepoServer, TLS: TLSClientOptions{Plaintext: true}},
		ExtensionsDir: t.TempDir(), DownloadDir: t.TempDir(),
		ShutdownTimeout: 5 * time.Second,
		Logger:          slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo})),
		OnListening:     func(api, metrics net.Addr) { ch <- addrs{api, metrics} },
	}
	require.Equal(t, "/", cfg.BaseHRef, "the compiled --basehref default")
	stop := running(t, func(ctx context.Context) error { return Run(ctx, cfg) })
	a := waitAddr(t, ch)
	api := "http://" + a.api.String()

	eventually(t, api+"/healthz", http.StatusOK)
	eventually(t, api+"/patrol/api/version", http.StatusOK)
	assert.Contains(t, logs.String(), "--basehref and --rootpath had conflict: basehref: / rootpath: /patrol")

	code, page := httpDo(t, http.MethodGet, api+"/patrol/", map[string]string{"Accept": "text/html"})
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, page, `<base href="/patrol/" />`)
	code, _ = httpDo(t, http.MethodGet, api+"/api/version", nil)
	assert.Equal(t, http.StatusNotFound, code, "outside the root path")

	// The session cookie is scoped to the root path.
	req, err := http.NewRequest(http.MethodPost, api+"/patrol/api/v1/session?cookie=true",
		strings.NewReader(`{"username":"admin","password":"`+password+`"}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := insecureClient.Do(req)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var session *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == auth.SessionCookieName {
			session = c
		}
	}
	require.NotNil(t, session, "session cookie set")
	assert.Equal(t, "/patrol", session.Path)

	// Static assets: a file of the directory is served, a link out of it is not.
	code, css := httpDo(t, http.MethodGet, api+"/patrol/custom.css", nil)
	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, "body { color: red }", css)
	for _, accept := range []string{"", "text/html"} {
		code, leaked := httpDo(t, http.MethodGet, api+"/patrol/leak.txt", map[string]string{"Accept": accept})
		assert.NotContains(t, leaked, "outside-secret")
		if accept == "" {
			assert.NotEqual(t, http.StatusOK, code)
		}
	}
	stop()
}

func mustScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s, err := newScheme()
	require.NoError(t, err)
	return s
}

func TestControllerRunSmoke(t *testing.T) {
	requireEnvtest(t)
	kube := kubernetes.NewForConfigOrDie(testCfg)
	ns := newNamespace(t, kube)
	type addrs struct{ metrics, tree net.Addr }
	ch := make(chan addrs, 1)
	const identity = "patrol-controller-0"
	cfg := ControllerConfig{
		Namespace: ns, RestConfig: testCfg, InternalToken: smokeToken,
		RepoServer:     RepoServerClientOptions{Address: unusedRepoServer, TLS: TLSClientOptions{Plaintext: true}},
		MetricsAddress: "127.0.0.1:0", TreeAPIAddress: "127.0.0.1:0",
		Sharding:        sharding.ConfigFromEnv(func(string) (string, bool) { return "", false }, identity, nil),
		Identity:        identity,
		ShutdownTimeout: 5 * time.Second,
		Logger:          testLogger(t),
		OnListening:     func(m, tr net.Addr) { ch <- addrs{m, tr} },
	}
	stop := running(t, func(ctx context.Context) error { return RunController(ctx, cfg) })
	a := waitAddr(t, ch)
	metrics, tree := "http://"+a.metrics.String(), "http://"+a.tree.String()

	eventually(t, metrics+"/healthz", http.StatusOK)
	eventually(t, tree+"/healthz", http.StatusOK)
	code, _ := httpDo(t, http.MethodGet, tree+"/internal/v1/tree?namespace="+ns+"&name=x", nil)
	assert.Equal(t, http.StatusUnauthorized, code, "the tree API requires the internal token")
	code, _ = httpDo(t, http.MethodGet, tree+"/internal/v1/tree?namespace="+ns+"&name=missing", map[string]string{"Authorization": "Bearer " + smokeToken})
	assert.Equal(t, http.StatusNotFound, code)

	// Shard 0 is owned through its Lease (ADR-0008) and released on shutdown.
	require.Eventually(t, func() bool {
		l, err := kube.CoordinationV1().Leases(ns).Get(context.Background(), sharding.LeaseName(0), metav1.GetOptions{})
		return err == nil && l.Spec.HolderIdentity != nil && *l.Spec.HolderIdentity == identity
	}, 30*time.Second, 200*time.Millisecond)
	stop()
	l, err := kube.CoordinationV1().Leases(ns).Get(context.Background(), sharding.LeaseName(0), metav1.GetOptions{})
	require.NoError(t, err)
	assert.True(t, l.Spec.HolderIdentity == nil || *l.Spec.HolderIdentity == "", "the shard Lease is released on shutdown")
}

func TestAppSetControllerRunSmoke(t *testing.T) {
	requireEnvtest(t)
	kube := kubernetes.NewForConfigOrDie(testCfg)
	ns := newNamespace(t, kube)
	ch := make(chan net.Addr, 1)
	rc, err := appSetRunConfigFromParams(nil, ns, testCfg)
	require.NoError(t, err)
	rc.LeaderElection = true
	rc.LeaderElectionID = "wiring-smoke-appset"
	cfg := AppSetConfig{
		Run: rc, InternalToken: smokeToken,
		RepoServer:     RepoServerClientOptions{Address: unusedRepoServer, TLS: TLSClientOptions{Plaintext: true}},
		MetricsAddress: "127.0.0.1:0", ShutdownTimeout: 5 * time.Second, Logger: testLogger(t),
		OnListening: func(m net.Addr) { ch <- m },
	}
	stop := running(t, func(ctx context.Context) error { return RunAppSetController(ctx, cfg) })
	metrics := "http://" + waitAddr(t, ch).String()
	eventually(t, metrics+"/healthz", http.StatusOK)
	require.Eventually(t, func() bool {
		l, err := kube.CoordinationV1().Leases(ns).Get(context.Background(), "wiring-smoke-appset", metav1.GetOptions{})
		return err == nil && l.Spec.HolderIdentity != nil && *l.Spec.HolderIdentity != ""
	}, 30*time.Second, 200*time.Millisecond, "leader election Lease")
	stop()
}

func TestNotificationsControllerRunSmoke(t *testing.T) {
	requireEnvtest(t)
	kube := kubernetes.NewForConfigOrDie(testCfg)
	ns := newNamespace(t, kube)
	ch := make(chan net.Addr, 1)
	cfg := NotificationsConfig{
		Controller:     notifcontroller.Config{RestConfig: testCfg, Namespace: ns, LeaderElection: true, LeaderElectionID: "wiring-smoke-notifications"},
		InternalToken:  smokeToken,
		RepoServer:     RepoServerClientOptions{Address: unusedRepoServer, TLS: TLSClientOptions{Plaintext: true}},
		MetricsAddress: "127.0.0.1:0", ShutdownTimeout: 5 * time.Second, Logger: testLogger(t),
		OnListening: func(m net.Addr) { ch <- m },
	}
	stop := running(t, func(ctx context.Context) error { return RunNotificationsController(ctx, cfg) })
	metrics := "http://" + waitAddr(t, ch).String()
	eventually(t, metrics+"/healthz", http.StatusOK)
	require.Eventually(t, func() bool {
		l, err := kube.CoordinationV1().Leases(ns).Get(context.Background(), "wiring-smoke-notifications", metav1.GetOptions{})
		return err == nil && l.Spec.HolderIdentity != nil && *l.Spec.HolderIdentity != ""
	}, 30*time.Second, 200*time.Millisecond, "leader election Lease")
	stop()
}

// freeAddr returns a loopback address that was free a moment ago (for components whose
// library entry point listens itself).
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	return addr
}

func TestRepoServerRunSmoke(t *testing.T) {
	t.Parallel()
	api, probes := freeAddr(t), freeAddr(t)
	root := filepath.Join(t.TempDir(), "root")
	// The repo-server leaves its root traversable but not listable (0300).
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) })
	cfg := RepoServerConfig{Server: reposerver.Config{
		ListenAddress: api, MetricsAddress: probes, Token: smokeToken, RootDir: root,
		SSHKnownHostsPath: reposerver.TrustMaterialDisabled, TLSCertsDir: reposerver.TrustMaterialDisabled,
		GPGKeysDir: reposerver.TrustMaterialDisabled, Registerer: newRegistry(), Logger: testLogger(t),
	}, TLS: TLSServerOptions{MinVersion: "1.2", MaxVersion: "1.3"}}
	reg := newRegistry()
	cfg.Server.Registerer, cfg.Server.Gatherer = reg, reg
	stop := running(t, func(ctx context.Context) error { return RunRepoServer(ctx, cfg) })
	eventually(t, "http://"+probes+"/healthz", http.StatusOK)
	eventually(t, "http://"+probes+"/healthz?full=true", http.StatusOK)
	eventually(t, "https://"+api+"/healthz", http.StatusOK)
	code, body := httpDo(t, http.MethodGet, "http://"+probes+"/metrics", nil)
	assert.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "go_goroutines")
	stop()
}

func TestCommitServerRunSmoke(t *testing.T) {
	t.Parallel()
	type addrs struct{ api, metrics net.Addr }
	ch := make(chan addrs, 1)
	cfg := CommitServerConfig{
		ListenAddress: "127.0.0.1:0", MetricsAddress: "127.0.0.1:0", Token: smokeToken,
		ShutdownTimeout: 5 * time.Second, Logger: testLogger(t),
		OnListening: func(a, m net.Addr) { ch <- addrs{a, m} },
	}
	stop := running(t, func(ctx context.Context) error { return RunCommitServer(ctx, cfg) })
	a := waitAddr(t, ch)
	eventually(t, "http://"+a.api.String()+"/healthz", http.StatusOK)
	eventually(t, "http://"+a.metrics.String()+"/healthz", http.StatusOK)
	code, body := httpDo(t, http.MethodGet, "http://"+a.metrics.String()+"/metrics", nil)
	assert.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "go_goroutines")
	stop()
}

func TestCMPServerRunSmoke(t *testing.T) {
	t.Parallel()
	configDir := t.TempDir()
	writeFile(t, filepath.Join(configDir, "plugin.yaml"), "apiVersion: argoproj.io/v1alpha1\nkind: ConfigManagementPlugin\nmetadata:\n  name: smoke\nspec:\n  generate:\n    command: [sh, -c, 'echo {}']\n")
	// Unix socket paths are short; t.TempDir is too long on macOS.
	socketDir, err := os.MkdirTemp("/tmp", "pcmp")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	o := cmp.DefaultOptions()
	o.ConfigDir, o.SocketDir, o.WorkDir = configDir, socketDir, t.TempDir()
	stop := running(t, func(ctx context.Context) error {
		return RunCMPServer(ctx, CMPServerConfig{Options: o, MetricsAddress: "-", ShutdownTimeout: 5 * time.Second, Logger: testLogger(t)})
	})
	var sock string
	require.Eventually(t, func() bool {
		entries, _ := os.ReadDir(socketDir)
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".sock") {
				sock = filepath.Join(socketDir, e.Name())
				return true
			}
		}
		return false
	}, 30*time.Second, 100*time.Millisecond)
	stop()
	_, err = os.Stat(sock)
	assert.True(t, errors.Is(err, os.ErrNotExist), "the socket is removed on shutdown")
}

// TestBootstrapReadsCmdParams runs the command bootstrap against envtest: the parameters of
// patrol-cmd-params-cm apply unless the environment or a flag overrides them.
func TestBootstrapReadsCmdParams(t *testing.T) {
	requireEnvtest(t)
	kube := kubernetes.NewForConfigOrDie(testCfg)
	ns := newNamespace(t, kube)
	_, err := kube.CoreV1().ConfigMaps(ns).Create(context.Background(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: settings.DefaultCmdParamsConfigMapName},
		Data: map[string]string{
			"server.insecure": "true", "server.rootpath": "/from-cm", "server.basehref": "/from-cm/",
			"server.k8s.client.qps": "7", "server.log.level": "debug",
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	kc := filepath.Join(t.TempDir(), "kubeconfig")
	require.NoError(t, os.WriteFile(kc, testKubeconfig, 0o600))

	// The flags of `patrolcd server`: common options and the server parameters.
	fs := pflag.NewFlagSet("server", pflag.ContinueOnError)
	var common CommonOptions
	addCommonFlags(fs, &common, commonFlags{kube: true})
	pf := BindParamFlags(fs, settings.ComponentServer)
	require.NoError(t, fs.Parse([]string{"--kubeconfig", kc, "-n", ns, "--rootpath=/from-flag"}))

	rt, err := bootstrap(context.Background(), bootstrapInput{
		name: "server", component: settings.ComponentServer, pf: pf, common: common, kube: true, stderr: io.Discard,
		lookupEnv: envMap(map[string]string{"PATROL_SERVER_BASEHREF": "/from-env/"}),
	})
	require.NoError(t, err)
	assert.Equal(t, ns, rt.Namespace)
	assert.True(t, rt.Params.Bool("server.insecure"), "ConfigMap value")
	assert.Equal(t, "/from-env/", rt.Params.String("server.basehref"), "environment over ConfigMap")
	assert.Equal(t, "/from-flag", rt.Params.String("server.rootpath"), "flag over ConfigMap")
	assert.InDelta(t, 7, rt.Rest.QPS, 0.001, "client tuning from the ConfigMap")
	assert.True(t, rt.Log.Enabled(context.Background(), slog.LevelDebug), "log level from the ConfigMap")
}
