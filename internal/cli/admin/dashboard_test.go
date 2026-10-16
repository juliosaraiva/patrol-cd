package admin

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

// syncBuffer is a goroutine-safe output buffer.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// testCert returns a self-signed certificate for localhost and its PEM encoding.
func testCert(t *testing.T) (tls.Certificate, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "patrol-server"},
		DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// fakePortForward serves the pods/portforward WebSocket protocol for pod "ns/pod" port
// 8080, relaying each stream to backend (mode "relay"), or misbehaving.
type fakePortForward struct {
	t       *testing.T
	backend string
	mode    string
	mu      sync.Mutex
	streams int
}

func (p *fakePortForward) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/v1/namespaces/ns/pods/pod/portforward" || r.URL.Query().Get("ports") != "8080" {
		http.Error(w, "unexpected request "+r.URL.String(), http.StatusNotFound)
		return
	}
	if p.mode == "forbidden" {
		http.Error(w, "pods \"pod\" is forbidden", http.StatusForbidden)
		return
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{portForwardProtocol}})
	if err != nil {
		return
	}
	defer func() { _ = ws.CloseNow() }()
	if ws.Subprotocol() != portForwardProtocol {
		_ = ws.Close(websocket.StatusPolicyViolation, "subprotocol")
		return
	}
	p.mu.Lock()
	p.streams++
	p.mu.Unlock()
	ctx := r.Context()
	header := func(ch byte) []byte {
		b := []byte{ch, 0, 0}
		binary.LittleEndian.PutUint16(b[1:], 8080)
		return b
	}
	if p.mode == "badheader" {
		_ = ws.Write(ctx, websocket.MessageBinary, []byte{7, 1})
		return
	}
	_ = ws.Write(ctx, websocket.MessageBinary, header(pfDataChannel))
	_ = ws.Write(ctx, websocket.MessageBinary, header(pfErrorChannel))
	if p.mode == "error" {
		_ = ws.Write(ctx, websocket.MessageBinary, append([]byte{pfErrorChannel}, "error forwarding port 8080: connection refused"...))
		_ = ws.Close(websocket.StatusNormalClosure, "")
		return
	}
	conn, err := net.Dial("tcp", p.backend)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 16<<10)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				if werr := ws.Write(ctx, websocket.MessageBinary, append([]byte{pfDataChannel}, buf[:n]...)); werr != nil {
					return
				}
			}
			if err != nil {
				_ = ws.Close(websocket.StatusNormalClosure, "")
				return
			}
		}
	}()
	for {
		_, msg, err := ws.Read(ctx)
		if err != nil {
			_ = conn.Close()
			break
		}
		if len(msg) > 1 && msg[0] == pfDataChannel {
			if _, err := conn.Write(msg[1:]); err != nil {
				break
			}
		}
	}
	<-done
}

func startFakePortForward(t *testing.T, backend, mode string) (*fakePortForward, *rest.Config) {
	t.Helper()
	pf := &fakePortForward{t: t, backend: backend, mode: mode}
	srv := httptest.NewServer(pf)
	t.Cleanup(srv.Close)
	return pf, &rest.Config{Host: srv.URL}
}

func echoServer(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	return l.Addr().String()
}

func TestPortForwardProtocol(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pf, cfg := startFakePortForward(t, echoServer(t), "relay")
	d, err := newPodDialer(cfg, "ns", "pod", 8080)
	require.NoError(t, err)
	conn, err := d.DialContext(ctx, "tcp", "ignored:1")
	require.NoError(t, err)
	// More than one 32 KiB frame each way.
	payload := make([]byte, 100<<10)
	_, _ = rand.Read(payload)
	go func() { _, _ = conn.Write(payload) }()
	got := make([]byte, len(payload))
	_, err = io.ReadFull(conn, got)
	require.NoError(t, err)
	assert.Equal(t, payload, got)
	require.NoError(t, conn.Close())
	require.NoError(t, conn.Close(), "Close is idempotent")
	assert.Equal(t, 1, pf.streams)

	// Errors on the error channel are returned by Read.
	_, cfg = startFakePortForward(t, "", "error")
	d, err = newPodDialer(cfg, "ns", "pod", 8080)
	require.NoError(t, err)
	conn, err = d.DialContext(ctx, "tcp", "")
	require.NoError(t, err)
	_, err = conn.Read(make([]byte, 10))
	require.ErrorContains(t, err, "port forwarding: error forwarding port 8080: connection refused")
	_ = conn.Close()

	_, cfg = startFakePortForward(t, "", "badheader")
	d, err = newPodDialer(cfg, "ns", "pod", 8080)
	require.NoError(t, err)
	_, err = d.DialContext(ctx, "tcp", "")
	require.ErrorContains(t, err, "port forwarding: unexpected stream header")

	_, cfg = startFakePortForward(t, "", "forbidden")
	d, err = newPodDialer(cfg, "ns", "pod", 8080)
	require.NoError(t, err)
	_, err = d.DialContext(ctx, "tcp", "")
	require.ErrorContains(t, err, "port forwarding: 403 Forbidden")

	_, err = newPodDialer(&rest.Config{Host: "ftp://example.com"}, "ns", "pod", 8080)
	require.ErrorContains(t, err, `unsupported API server scheme "ftp"`)
}

// patrolServerBackend is a TLS server standing in for patrol-server.
func patrolServerBackend(t *testing.T, cert tls.Certificate) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	mux.HandleFunc("/api/v1/session", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"host": r.Host, "proto": r.Header.Get("X-Forwarded-Proto"), "forwardedHost": r.Header.Get("X-Forwarded-Host"),
			"cookie": r.Header.Get("Cookie"),
		})
	})
	mux.HandleFunc("/api/v1/stream", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := range 2 {
			_, _ = fmt.Fprintf(w, "data: event-%d\n\n", i)
			w.(http.Flusher).Flush()
			time.Sleep(50 * time.Millisecond)
		}
	})
	srv := httptest.NewUnstartedServer(mux)
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // expected handshake failures
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

// startDashboard runs serveDashboard on a free loopback port and returns its URL.
func startDashboard(t *testing.T, up dashboardUpstream) (string, *syncBuffer, func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	out, errOut := &syncBuffer{}, &syncBuffer{}
	f := &Factory{IOStreams: IOStreams{In: strings.NewReader(""), Out: out, ErrOut: errOut}}
	done := make(chan error, 1)
	go func() { done <- serveDashboard(ctx, f, &dashboardOptions{port: 0, address: "127.0.0.1"}, up) }()
	var url string
	require.Eventually(t, func() bool {
		select {
		case err := <-done:
			done <- err
			return true
		default:
		}
		s := out.String()
		if i := strings.Index(s, "available at "); i >= 0 {
			url = strings.TrimSpace(s[i+len("available at "):])
			return true
		}
		return false
	}, 10*time.Second, 10*time.Millisecond)
	var once sync.Once
	var stopErr error
	stop := func() error {
		once.Do(func() {
			cancel()
			select {
			case stopErr = <-done:
			case <-time.After(10 * time.Second):
				stopErr = fmt.Errorf("dashboard did not stop")
			}
		})
		return stopErr
	}
	t.Cleanup(func() { _ = stop() })
	return url, errOut, stop
}

func TestDashboardProxiesThroughPortForward(t *testing.T) {
	t.Parallel()
	cert, certPEM := testCert(t)
	_, cfg := startFakePortForward(t, patrolServerBackend(t, cert), "relay")
	d, err := newPodDialer(cfg, "ns", "pod", 8080)
	require.NoError(t, err)
	pinned, err := pinnedTLS(certPEM)
	require.NoError(t, err)
	url, errOut, stop := startDashboard(t, dashboardUpstream{dial: d.DialContext, scheme: "https", tls: pinned})
	require.NotEmpty(t, url, errOut.String())
	require.True(t, strings.HasPrefix(url, "http://127.0.0.1:"), url)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url+"/api/v1/session", nil)
	require.NoError(t, err)
	req.Header.Set("Cookie", "patrol.token=session-cookie")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	var got map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	_ = resp.Body.Close()
	assert.Equal(t, strings.TrimPrefix(url, "http://"), got["host"], "the browser's Host is kept for the server's origin checks")
	assert.Equal(t, "http", got["proto"])
	assert.Equal(t, "patrol.token=session-cookie", got["cookie"])

	// Server-Sent Events are streamed.
	resp, err = http.Get(url + "/api/v1/stream") //nolint:noctx // test
	require.NoError(t, err)
	sc := bufio.NewScanner(resp.Body)
	var events []string
	for sc.Scan() {
		if line := sc.Text(); strings.HasPrefix(line, "data: ") {
			events = append(events, strings.TrimPrefix(line, "data: "))
		}
	}
	_ = resp.Body.Close()
	assert.Equal(t, []string{"event-0", "event-1"}, events)

	require.NoError(t, stop())
	assert.Contains(t, errOut.String(), "signal received, shutting down dashboard\nclean shutdown\n")
}

func TestDashboardVerifiesPatrolServer(t *testing.T) {
	t.Parallel()
	cert, certPEM := testCert(t)
	_, otherPEM := testCert(t)
	backend := patrolServerBackend(t, cert)
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", backend)
	}
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.crt")
	require.NoError(t, os.WriteFile(caPath, certPEM, 0o600))
	badPath := filepath.Join(dir, "bad.crt")
	require.NoError(t, os.WriteFile(badPath, []byte("not a certificate"), 0o600))

	otherPinned, err := pinnedTLS(otherPEM)
	require.NoError(t, err)
	err = checkUpstream(t.Context(), dashboardUpstream{dial: dial, scheme: "https", tls: otherPinned})
	require.ErrorContains(t, err, "patrol-server certificate does not match tls.crt of patrol-secret")

	// Default: verified against the system roots, which do not trust the server.
	def, err := upstreamTLS(t.Context(), nil, &dashboardOptions{})
	require.NoError(t, err)
	err = checkUpstream(t.Context(), dashboardUpstream{dial: dial, scheme: "https", tls: def})
	require.ErrorContains(t, err, "pass --server-crt, or --insecure to skip verification")

	withCA, err := upstreamTLS(t.Context(), nil, &dashboardOptions{serverCrt: caPath})
	require.NoError(t, err)
	require.NoError(t, checkUpstream(t.Context(), dashboardUpstream{dial: dial, scheme: "https", tls: withCA}))

	insecure, err := upstreamTLS(t.Context(), nil, &dashboardOptions{insecure: true})
	require.NoError(t, err)
	assert.True(t, insecure.InsecureSkipVerify)
	require.NoError(t, checkUpstream(t.Context(), dashboardUpstream{dial: dial, scheme: "https", tls: insecure}))

	_, err = upstreamTLS(t.Context(), nil, &dashboardOptions{serverCrt: badPath})
	require.ErrorContains(t, err, "contains no PEM certificate")
	_, err = pinnedTLS([]byte("garbage"))
	require.ErrorContains(t, err, "patrol-secret tls.crt is not a PEM certificate")

	// The dashboard refuses to start when the upstream is not verified, and when its port
	// is taken.
	_, errOut, stop := startDashboard(t, dashboardUpstream{dial: dial, scheme: "https", tls: otherPinned})
	require.ErrorContains(t, stop(), "could not start dashboard")
	_ = errOut
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
	f := &Factory{IOStreams: IOStreams{Out: io.Discard, ErrOut: io.Discard}}
	err = serveDashboard(t.Context(), f, &dashboardOptions{address: "127.0.0.1", port: l.Addr().(*net.TCPAddr).Port}, dashboardUpstream{dial: dial, scheme: "https", tls: insecure})
	require.ErrorContains(t, err, "could not start dashboard: listen tcp")
}

func TestDashboardClusterLookups(t *testing.T) {
	ctx := requireEnv(t)
	ns := newNamespace(t, ctx, "dashboard")
	kc, err := newKubeClients(rest.CopyConfig(envCfg), ns)
	require.NoError(t, err)
	selector := v1alpha1.LabelKeyAppName + "=" + defaultServerName

	_, err = readyPod(ctx, kc.kube, ns, selector)
	require.ErrorContains(t, err, `cannot find a running and ready pod with selector "app.kubernetes.io/name=patrol-server" in namespace `+ns)
	pod, err := envKube.CoreV1().Pods(ns).Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "patrol-server-0", Labels: map[string]string{v1alpha1.LabelKeyAppName: defaultServerName}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "server", Image: "patrol-cd/patrol:dev"}}},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = readyPod(ctx, kc.kube, ns, selector)
	require.Error(t, err, "a pending pod is not used")
	pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}
	_, err = envKube.CoreV1().Pods(ns).UpdateStatus(ctx, pod, metav1.UpdateOptions{})
	require.NoError(t, err)
	got, err := readyPod(ctx, kc.kube, ns, selector)
	require.NoError(t, err)
	assert.Equal(t, "patrol-server-0", got.Name)

	// Without tls.crt in patrol-secret the server certificate is verified normally.
	cfg, err := upstreamTLS(ctx, kc, &dashboardOptions{})
	require.NoError(t, err)
	assert.Nil(t, cfg.VerifyPeerCertificate)
	assert.Equal(t, upstreamHost, cfg.ServerName)
	// With it, exactly that certificate is accepted.
	cert, certPEM := testCert(t)
	other, _ := testCert(t)
	_, err = envKube.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: settings.DefaultSecretName},
		Data:       map[string][]byte{"tls.crt": certPEM, "tls.key": []byte("unused-in-this-test")},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	cfg, err = upstreamTLS(ctx, kc, &dashboardOptions{})
	require.NoError(t, err)
	require.NotNil(t, cfg.VerifyPeerCertificate)
	require.NoError(t, cfg.VerifyPeerCertificate(cert.Certificate, nil))
	require.Error(t, cfg.VerifyPeerCertificate(other.Certificate, nil))

	// The command reports the missing pod and ignores the Redis flags with a warning.
	require.NoError(t, envKube.CoreV1().Pods(ns).Delete(ctx, "patrol-server-0", metav1.DeleteOptions{GracePeriodSeconds: new(int64)}))
	require.Eventually(t, func() bool {
		_, err := readyPod(ctx, kc.kube, ns, selector)
		return err != nil
	}, 30*time.Second, 100*time.Millisecond)
	tio := newTestIO(ns)
	err = tio.run(ctx, "admin", "dashboard", "--port", "0", "--redis", "argocd-redis:6379", "--redisdb", "2")
	require.ErrorContains(t, err, "could not start dashboard: cannot find a running and ready pod")
	assert.Contains(t, tio.errOut.String(), "warning: --redis: Patrol CD has no Redis; the dashboard proxies to patrol-server")
	assert.Contains(t, tio.errOut.String(), "warning: --redisdb:")
}
