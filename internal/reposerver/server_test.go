package reposerver

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
)

func postRaw(t *testing.T, url, token, contentType, body string) (int, apiclient.Problem) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	require.NoError(t, err)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	var p apiclient.Problem
	if strings.HasPrefix(resp.Header.Get("Content-Type"), apiclient.ContentTypeProblem) {
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&p))
	}
	return resp.StatusCode, p
}

func TestHandlerAuthAndErrors(t *testing.T) {
	t.Parallel()
	s := newTestService(t, func(c *Config) { c.MaxRequestBytes = 1024 })
	h, err := s.Handler()
	require.NoError(t, err)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	method := srv.URL + apiclient.PathPrefix + apiclient.MethodListRefs

	status, p := postRaw(t, method, "", "application/json", `{}`)
	assert.Equal(t, http.StatusUnauthorized, status)
	assert.Equal(t, apiclient.CodeUnauthenticated, p.Code)
	status, _ = postRaw(t, method, "wrong-token", "application/json", `{}`)
	assert.Equal(t, http.StatusUnauthorized, status)
	status, _ = postRaw(t, method, testToken+"x", "application/json", `{}`)
	assert.Equal(t, http.StatusUnauthorized, status)

	status, p = postRaw(t, srv.URL+apiclient.PathPrefix+"DeleteEverything", testToken, "application/json", `{}`)
	assert.Equal(t, http.StatusNotFound, status)
	assert.Equal(t, apiclient.CodeNotFound, p.Code)

	status, _ = postRaw(t, method, testToken, "text/plain", `{}`)
	assert.Equal(t, http.StatusUnsupportedMediaType, status)

	status, p = postRaw(t, method, testToken, "application/json", `{"repo":`)
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Contains(t, p.Detail, "malformed request body")

	status, p = postRaw(t, method, testToken, "application/json", `{} {}`)
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Contains(t, p.Detail, "trailing data")

	status, p = postRaw(t, method, testToken, "application/json", `{"repo":{"url":"`+strings.Repeat("a", 2048)+`"}}`)
	assert.Equal(t, http.StatusRequestEntityTooLarge, status)
	assert.Equal(t, apiclient.CodeResourceExhausted, p.Code)

	status, p = postRaw(t, method, testToken, "application/json", `{}`)
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Equal(t, "must pass a valid repo", p.Detail)

	resp, err := http.Get(method)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)

	for _, path := range []string{apiclient.PathHealthz, apiclient.PathReadyz} {
		resp, err := http.Get(srv.URL + path)
		require.NoError(t, err)
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode, path)
		assert.Equal(t, "ok\n", string(body))
	}
	s.shuttingDown.Store(true)
	resp, err = http.Get(srv.URL + apiclient.PathReadyz)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)

	// Metrics count requests by method and code.
	assert.GreaterOrEqual(t, counter(s.metrics.requests, apiclient.MethodListRefs, apiclient.CodeUnauthenticated), 3.0)
	assert.Equal(t, 1.0, counter(s.metrics.requests, "unknown", apiclient.CodeNotFound))

	_, err = NewHandler(s, HandlerOptions{})
	require.Error(t, err)
}

// testPKI issues a CA and certificates signed by it.
type testPKI struct {
	caCert *x509.Certificate
	caKey  *ecdsa.PrivateKey
	caPEM  []byte
}

func newTestPKI(t *testing.T) *testPKI {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return &testPKI{caCert: cert, caKey: key, caPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

func (p *testPKI) issue(t *testing.T, cn string, usage x509.ExtKeyUsage) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage},
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.caCert, &key.PublicKey, p.caKey)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func TestServeMutualTLS(t *testing.T) {
	t.Parallel()
	gitBin(t)
	r := newRemote(t)
	r.commit("one", nil)
	pki := newTestPKI(t)
	caFile := filepath.Join(t.TempDir(), "ca.crt")
	require.NoError(t, os.WriteFile(caFile, pki.caPEM, 0o600))
	serverCert := pki.issue(t, "repo-server", x509.ExtKeyUsageServerAuth)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	probeLn, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(bg())
	reg := prometheus.NewRegistry()
	cfg := Config{
		Token: testToken, RootDir: filepath.Join(t.TempDir(), "root"), AllowLocalRepositories: true,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{serverCert}}, ClientCAFile: caFile,
		ToolPATH: toolPATH(t), PluginSocketDir: filepath.Join(t.TempDir(), "none"),
		SSHKnownHostsPath: TrustMaterialDisabled, TLSCertsDir: TrustMaterialDisabled, GPGKeysDir: TrustMaterialDisabled,
		Registerer: reg, Gatherer: reg,
	}
	done := make(chan error, 1)
	go func() { done <- serve(ctx, cfg, ln, probeLn) }()
	stop := sync.OnceValue(func() error { cancel(); return <-done })
	t.Cleanup(func() {
		_ = stop()
		_ = removeTree(cfg.RootDir)
	})

	roots := x509.NewCertPool()
	roots.AddCert(pki.caCert)
	addr := "https://" + ln.Addr().String()
	waitReady := func(c *http.Client) {
		require.Eventually(t, func() bool {
			resp, err := c.Get(addr + apiclient.PathHealthz)
			if err != nil {
				return false
			}
			_ = resp.Body.Close()
			return resp.StatusCode == http.StatusOK
		}, 10*time.Second, 50*time.Millisecond)
	}

	clientCert := pki.issue(t, "controller", x509.ExtKeyUsageClientAuth)
	good, err := apiclient.NewHTTPClient(ln.Addr().String(), testToken, &tls.Config{RootCAs: roots, Certificates: []tls.Certificate{clientCert}})
	require.NoError(t, err)
	waitReady(&http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, Certificates: []tls.Certificate{clientCert}}}})
	refs, err := good.ListRefs(bg(), &apiclient.ListRefsRequest{Repo: r.repo()})
	require.NoError(t, err)
	assert.Equal(t, []string{"main"}, refs.Branches)

	// Without a client certificate the TLS handshake fails (mTLS); retried, then
	// reported as unavailable.
	noCert, err := apiclient.NewHTTPClient(ln.Addr().String(), testToken, &tls.Config{RootCAs: roots},
		apiclient.WithRetry(2, time.Millisecond, time.Millisecond))
	require.NoError(t, err)
	_, err = noCert.ListRefs(bg(), &apiclient.ListRefsRequest{Repo: r.repo()})
	require.Error(t, err)
	assert.True(t, apiclient.IsCode(err, apiclient.CodeUnavailable), "%v", err)
	_, err = (&http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}}).Get(addr + apiclient.PathHealthz)
	require.Error(t, err, "the API listener requires a client certificate, even for /healthz")

	// Kubelet probes reach the probe listener without any client certificate; the full
	// liveness check reaches the mTLS API listener with the ephemeral health-check
	// certificate.
	probe := "http://" + probeLn.Addr().String()
	for _, path := range []string{apiclient.PathHealthz, apiclient.PathHealthz + "?full=true", apiclient.PathReadyz, "/metrics"} {
		resp, err := http.Get(probe + path)
		require.NoError(t, err, path)
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode, "%s: %s", path, body)
		if path == "/metrics" {
			assert.Contains(t, string(body), "patrol_reposerver_requests_total")
		}
	}

	require.NoError(t, stop())
}

func TestProbeHandler(t *testing.T) {
	t.Parallel()
	s := newTestService(t)
	failing := httptest.NewServer(s.probeHandler(func(context.Context) error { return errors.New("API listener down") }))
	t.Cleanup(failing.Close)
	resp, err := http.Get(failing.URL + apiclient.PathHealthz)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode, "the shallow check does not run the self check")
	resp, err = http.Get(failing.URL + apiclient.PathHealthz + "?full=true")
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	resp, err = http.Get(failing.URL + "/metrics")
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "no Gatherer, no /metrics")

	s.shuttingDown.Store(true)
	resp, err = http.Get(failing.URL + apiclient.PathReadyz)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)

	// The self check against a closed listener fails; against a plain API it passes.
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := dead.Addr()
	require.NoError(t, dead.Close())
	require.Error(t, newSelfCheck(addr, false, nil)(bg()))
	h, err := s.Handler()
	require.NoError(t, err)
	plain := httptest.NewServer(h)
	t.Cleanup(plain.Close)
	_, port, err := net.SplitHostPort(plain.Listener.Addr().String())
	require.NoError(t, err)
	wildcard := &net.TCPAddr{IP: net.IPv6unspecified, Port: mustAtoi(t, port)}
	require.NoError(t, newSelfCheck(wildcard, false, nil)(bg()), "wildcard addresses are checked over loopback")
}

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	require.NoError(t, err)
	return n
}

func TestRunShutsDownOnCancel(t *testing.T) {
	t.Parallel()
	gitBin(t)
	ctx, cancel := context.WithCancel(bg())
	cfg := Config{
		Token: testToken, RootDir: filepath.Join(t.TempDir(), "root"), ListenAddress: "127.0.0.1:0",
		DisableTLS: true, ToolPATH: toolPATH(t), PluginSocketDir: filepath.Join(t.TempDir(), "none"),
	}
	t.Cleanup(func() { _ = removeTree(cfg.RootDir) })
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg) }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	// A configuration without a token is refused.
	root2 := filepath.Join(t.TempDir(), "root2")
	t.Cleanup(func() { _ = removeTree(root2) })
	err := Run(bg(), Config{RootDir: root2, ListenAddress: "127.0.0.1:0", DisableTLS: true, PluginSocketDir: "none"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "internal token is required")
}

func TestServerTLSConfig(t *testing.T) {
	t.Parallel()
	cfg, hc, err := serverTLSConfig(Config{DisableTLS: true})
	require.NoError(t, err)
	assert.Nil(t, cfg)
	assert.Nil(t, hc)
	cfg, hc, err = serverTLSConfig(Config{})
	require.NoError(t, err)
	require.Len(t, cfg.Certificates, 1, "self-signed certificate when none is configured")
	assert.Equal(t, uint16(tls.VersionTLS12), cfg.MinVersion)
	assert.Equal(t, tls.NoClientCert, cfg.ClientAuth)
	assert.Nil(t, hc, "no health-check certificate without mTLS")

	// A missing client CA file leaves mTLS off (upstream).
	cfg, hc, err = serverTLSConfig(Config{ClientCAFile: filepath.Join(t.TempDir(), "missing.crt")})
	require.NoError(t, err)
	assert.Equal(t, tls.NoClientCert, cfg.ClientAuth)
	assert.Nil(t, hc)

	bad := filepath.Join(t.TempDir(), "ca.crt")
	require.NoError(t, os.WriteFile(bad, []byte("not a certificate"), 0o600))
	_, _, err = serverTLSConfig(Config{ClientCAFile: bad})
	require.Error(t, err)

	// mTLS: client certificates of the CA are required, plus the ephemeral health-check
	// certificate, which only serves client authentication.
	pki := newTestPKI(t)
	caFile := filepath.Join(t.TempDir(), "ca.crt")
	require.NoError(t, os.WriteFile(caFile, pki.caPEM, 0o600))
	cfg, hc, err = serverTLSConfig(Config{ClientCAFile: caFile})
	require.NoError(t, err)
	assert.Equal(t, tls.RequireAndVerifyClientCert, cfg.ClientAuth)
	require.NotNil(t, hc)
	require.NotNil(t, hc.Leaf)
	assert.Equal(t, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, hc.Leaf.ExtKeyUsage)
	_, err = hc.Leaf.Verify(x509.VerifyOptions{Roots: cfg.ClientCAs, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	require.NoError(t, err)
	client := pki.issue(t, "controller", x509.ExtKeyUsageClientAuth)
	leaf, err := x509.ParseCertificate(client.Certificate[0])
	require.NoError(t, err)
	_, err = leaf.Verify(x509.VerifyOptions{Roots: cfg.ClientCAs, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	require.NoError(t, err, "the configured CA still verifies clients")

	// A caller-built TLSConfig that verifies clients gets the health-check certificate
	// too, without its pool being modified.
	own := x509.NewCertPool()
	own.AddCert(pki.caCert)
	cfg, hc, err = serverTLSConfig(Config{TLSConfig: &tls.Config{ClientCAs: own, ClientAuth: tls.RequireAndVerifyClientCert}})
	require.NoError(t, err)
	require.NotNil(t, hc)
	_, err = hc.Leaf.Verify(x509.VerifyOptions{Roots: cfg.ClientCAs, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	require.NoError(t, err)
	_, err = hc.Leaf.Verify(x509.VerifyOptions{Roots: own, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	require.Error(t, err, "the caller's pool is not modified")
}
