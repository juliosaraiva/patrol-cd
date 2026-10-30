package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	krest "k8s.io/client-go/rest"

	"github.com/juliosaraiva/patrol-cd/internal/apiserver"
)

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestRunGroup(t *testing.T) {
	t.Parallel()
	t.Run("a failing task stops the others", func(t *testing.T) {
		t.Parallel()
		var stopped atomic.Bool
		err := runGroup(context.Background(), time.Second, discard(),
			task{name: "fails", run: func(context.Context) error { return errors.New("boom") }},
			task{name: "waits", run: func(ctx context.Context) error { <-ctx.Done(); stopped.Store(true); return nil }},
		)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "fails: boom")
		assert.True(t, stopped.Load())
	})
	t.Run("cancellation is a clean stop", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		go func() { time.Sleep(50 * time.Millisecond); cancel() }()
		err := runGroup(ctx, time.Second, discard(),
			task{name: "a", run: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }},
			task{name: "finished early", run: func(context.Context) error { return nil }},
		)
		require.NoError(t, err)
	})
	t.Run("a panic is an error", func(t *testing.T) {
		t.Parallel()
		err := runGroup(context.Background(), time.Second, discard(),
			task{name: "panics", run: func(context.Context) error { panic("bad") }})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "panic: bad")
	})
	t.Run("the drain is bounded", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		block := make(chan struct{})
		defer close(block)
		start := time.Now()
		err := runGroup(ctx, -drainGrace+100*time.Millisecond, discard(),
			task{name: "stuck", run: func(context.Context) error { <-block; return nil }})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "still running: [stuck]")
		assert.Less(t, time.Since(start), 5*time.Second)
	})
	// Regression: cancelling while runGroup waits in its run phase must start the drain
	// bound; a stuck task used to keep the group (and the process) waiting forever.
	t.Run("the drain is bounded after a later cancellation", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		block := make(chan struct{})
		defer close(block)
		done := make(chan error, 1)
		go func() {
			done <- runGroup(ctx, -drainGrace+200*time.Millisecond, discard(),
				task{name: "stuck", run: func(context.Context) error { <-block; return nil }})
		}()
		time.Sleep(50 * time.Millisecond)
		cancelled := time.Now()
		cancel()
		select {
		case err := <-done:
			require.Error(t, err)
			assert.Contains(t, err.Error(), "still running: [stuck]")
			assert.Less(t, time.Since(cancelled), 3*time.Second)
		case <-time.After(5 * time.Second):
			t.Fatal("runGroup did not return within the drain bound after cancellation")
		}
	})
}

func TestObservability(t *testing.T) {
	t.Parallel()
	flag := filepath.Join(t.TempDir(), "profiler.enabled")
	ready := atomic.Bool{}
	ctx, cancel := context.WithCancel(context.Background())
	addrs := make(chan [2]net.Addr, 1)
	done := make(chan error, 1)
	go func() {
		done <- serveObservability(ctx, ObservabilityOptions{
			MetricsAddr: "127.0.0.1:0", HealthzAddr: "127.0.0.1:0", Gatherer: newRegistry(), PprofFile: flag,
			Ready: func(context.Context) error {
				if !ready.Load() {
					return errors.New("not yet")
				}
				return nil
			},
			Logger: discard(),
		}, func(m, h net.Addr) { addrs <- [2]net.Addr{m, h} })
	}()
	a := <-addrs
	m, h := "http://"+a[0].String(), "http://"+a[1].String()
	code := func(url string) int {
		resp, err := http.Get(url)
		require.NoError(t, err)
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	assert.Equal(t, http.StatusServiceUnavailable, code(h+"/healthz"))
	assert.Equal(t, http.StatusOK, code(h+"/livez"))
	ready.Store(true)
	assert.Equal(t, http.StatusOK, code(h+"/readyz"))
	assert.Equal(t, http.StatusOK, code(m+"/healthz"))
	assert.Equal(t, http.StatusOK, code(m+"/metrics"))
	assert.Equal(t, http.StatusUnauthorized, code(m+"/debug/pprof/"))
	require.NoError(t, os.WriteFile(flag, []byte("true\n"), 0o644))
	assert.Equal(t, http.StatusOK, code(m+"/debug/pprof/"), "toggled at request time")
	assert.Equal(t, http.StatusNotFound, code(h+"/metrics"), "the health listener serves probes only")
	cancel()
	require.NoError(t, <-done)
}

func TestInternalToken(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	file := filepath.Join(dir, "token")
	require.NoError(t, os.WriteFile(file, []byte(" from-file \n"), 0o600))
	tok, err := loadInternalToken(ctx, file, true, nil, "")
	require.NoError(t, err)
	assert.Equal(t, "from-file", tok)

	_, err = loadInternalToken(ctx, filepath.Join(dir, "missing"), true, fake.NewClientset(), "patrol")
	require.ErrorIs(t, err, errNoToken, "an explicit file never falls back to the Secret")

	cs := fake.NewClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: InternalTokenSecretName, Namespace: "patrol"},
		Data:       map[string][]byte{InternalTokenSecretKey: []byte("from-secret")},
	})
	tok, err = loadInternalToken(ctx, filepath.Join(dir, "missing"), false, cs, "patrol")
	require.NoError(t, err)
	assert.Equal(t, "from-secret", tok)

	_, err = loadInternalToken(ctx, filepath.Join(dir, "missing"), false, fake.NewClientset(), "patrol")
	require.ErrorIs(t, err, errNoToken)

	empty := filepath.Join(dir, "empty")
	require.NoError(t, os.WriteFile(empty, []byte("\n"), 0o600))
	_, err = loadInternalToken(ctx, empty, true, nil, "")
	require.Error(t, err)
}

func writePair(t *testing.T, dir, host string) (string, string) {
	t.Helper()
	cert, err := apiserver.GenerateSelfSigned([]string{host}, time.Hour)
	require.NoError(t, err)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})
	keyDER, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	require.NoError(t, err)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	crt, key := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	require.NoError(t, os.WriteFile(crt, certPEM, 0o600))
	require.NoError(t, os.WriteFile(key, keyPEM, 0o600))
	return crt, key
}

func TestTLSConfigs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	crt, key := writePair(t, dir, "first.example")

	cfg, err := serverTLSConfig(TLSServerOptions{CertFile: crt, KeyFile: key, MinVersion: "1.3", Ciphers: ""})
	require.NoError(t, err)
	assert.Equal(t, uint16(tls.VersionTLS13), cfg.MinVersion)
	c1, err := cfg.GetCertificate(nil)
	require.NoError(t, err)
	leaf, err := x509.ParseCertificate(c1.Certificate[0])
	require.NoError(t, err)
	assert.Equal(t, []string{"first.example"}, leaf.DNSNames)

	_, err = serverTLSConfig(TLSServerOptions{CertFile: crt})
	require.Error(t, err, "pair must be complete")
	_, err = serverTLSConfig(TLSServerOptions{CertFile: crt, KeyFile: key, MinVersion: "1.3", MaxVersion: "1.2"})
	require.Error(t, err)
	_, err = serverTLSConfig(TLSServerOptions{CertFile: crt, KeyFile: key, Ciphers: "TLS_RSA_WITH_RC4_128_SHA"})
	require.Error(t, err, "insecure suites are refused")

	cfg, err = serverTLSConfig(TLSServerOptions{CertFile: crt, KeyFile: key, ClientCAFile: crt})
	require.NoError(t, err)
	assert.Equal(t, tls.RequireAndVerifyClientCert, cfg.ClientAuth)
	cfg, err = serverTLSConfig(TLSServerOptions{CertFile: crt, KeyFile: key, ClientCAFile: filepath.Join(dir, "absent.crt")})
	require.NoError(t, err)
	assert.Equal(t, tls.NoClientCert, cfg.ClientAuth, "an absent client CA disables mutual TLS")

	cc, err := clientTLSConfig(TLSClientOptions{Plaintext: true})
	require.NoError(t, err)
	assert.Nil(t, cc)
	cc, err = clientTLSConfig(TLSClientOptions{})
	require.NoError(t, err)
	assert.True(t, cc.InsecureSkipVerify, "upstream default without a CA")
	cc, err = clientTLSConfig(TLSClientOptions{StrictTLS: true})
	require.NoError(t, err)
	assert.False(t, cc.InsecureSkipVerify)
	cc, err = clientTLSConfig(TLSClientOptions{CAFile: crt, ClientCertFile: crt, ClientKeyFile: key})
	require.NoError(t, err)
	assert.NotNil(t, cc.RootCAs)
	assert.NotNil(t, cc.GetClientCertificate)
	cc, err = clientTLSConfig(TLSClientOptions{ClientCertFile: filepath.Join(dir, "none"), ClientKeyFile: key})
	require.NoError(t, err)
	assert.Nil(t, cc.GetClientCertificate, "missing client certificate files mean no client certificate")
}

func TestFileCertificateReload(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	crt, key := writePair(t, dir, "first.example")
	fc, err := newFileCertificate(crt, key)
	require.NoError(t, err)
	first := fc.get()
	require.NotNil(t, first)

	writePair(t, dir, "second.example")
	// Pretend the recheck interval elapsed.
	fc.mu.Lock()
	fc.checked = time.Time{}
	fc.mu.Unlock()
	second := fc.get()
	leaf, err := x509.ParseCertificate(second.Certificate[0])
	require.NoError(t, err)
	assert.Equal(t, []string{"second.example"}, leaf.DNSNames)

	require.NoError(t, os.WriteFile(crt, []byte("garbage"), 0o600))
	fc.mu.Lock()
	fc.checked = time.Time{}
	fc.mu.Unlock()
	assert.Same(t, second, fc.get(), "an invalid update keeps the last good pair")
}

func TestResolveArgs(t *testing.T) {
	t.Parallel()
	none := func(string) (string, bool) { return "", false }
	assert.Equal(t, []string{"repo-server", "--port=1"}, ResolveArgs("/usr/local/bin/patrol-repo-server", []string{"--port=1"}, none))
	assert.Equal(t, []string{"server"}, ResolveArgs("patrolcd", []string{"server"}, none))
	assert.Equal(t, []string{"controller"}, ResolveArgs("patrolcd", nil, envMap(map[string]string{EnvBinaryName: "patrol-application-controller"})))
	assert.Equal(t, []string{"--help"}, ResolveArgs("patrol-unknown", []string{"--help"}, none))
}

func TestConfigValidation(t *testing.T) {
	t.Parallel()
	err := (&Config{}).Validate()
	require.Error(t, err)
	for _, want := range []string{"namespace is required", "listen address is required", "internal token", "repo-server address"} {
		assert.Contains(t, err.Error(), want)
	}
	err = (&Config{Namespace: "n", RestConfig: testCfgOrEmpty(), ListenAddress: "nohost", InternalToken: "t",
		RepoServer: RepoServerClientOptions{Address: "r:1"}, TLSCiphers: "NOPE", TLSCertFile: "x"}).Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "listen address")
	assert.Contains(t, err.Error(), "TLS ciphers")
	assert.Contains(t, err.Error(), "--tls-cert and --tls-key")

	cerr := (&ControllerConfig{HydratorEnabled: true, TreeAPIAddress: "bad", ClusterInfoInterval: -time.Second}).Validate()
	require.Error(t, cerr)
	assert.Contains(t, cerr.Error(), "commit-server address")
	assert.Contains(t, cerr.Error(), "tree API address")
	assert.Contains(t, cerr.Error(), "cluster info interval")
	assert.True(t, strings.Contains((&AppSetConfig{}).Validate().Error(), "namespace is required"))
	assert.True(t, strings.Contains((&NotificationsConfig{}).Validate().Error(), "namespace is required"))
}

func testCfgOrEmpty() *krest.Config { return &krest.Config{Host: "https://127.0.0.1:1"} }
