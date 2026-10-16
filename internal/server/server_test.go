package server

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/juliosaraiva/patrol-cd/internal/apiserver"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

// testCA issues certificates for TLS tests.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return &testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// issue returns a pair for 127.0.0.1 (server and client usage) and its PEM encodings.
func (ca *testCA) issue(t *testing.T, cn string) (tls.Certificate, []byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	require.NoError(t, err)
	return pair, certPEM, keyPEM
}

// tlsServer starts an HTTPS test server presenting cert.
func tlsServer(t *testing.T, cert tls.Certificate, h http.Handler) *httptest.Server {
	t.Helper()
	ts := httptest.NewUnstartedServer(h)
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return ts
}

func TestAPICipherSuites(t *testing.T) {
	t.Parallel()
	p, issues := settings.ResolveParams(settings.ComponentServer, nil, func(string) (string, bool) { return "", false })
	require.Empty(t, issues)
	require.Equal(t, DefaultTLSCiphers, p.String("server.tls.ciphers"), "DefaultTLSCiphers is the parameter default")

	// The default (upstream) list has no HTTP/2-required suite: both are added.
	suites, ignored, added, err := apiCipherSuites("1.2", DefaultTLSCiphers)
	require.NoError(t, err)
	assert.False(t, ignored)
	assert.Equal(t, http2CipherSuites, added)
	assert.Equal(t, append([]string{DefaultTLSCiphers}, http2CipherSuites...), suites)

	// A list with one of them is kept as configured.
	suites, _, added, err = apiCipherSuites("", "TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256:TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384")
	require.NoError(t, err)
	assert.Empty(t, added)
	assert.Equal(t, []string{"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256", "TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384"}, suites)

	// TLS 1.3 minimum: suites are not configurable; only a non-default list is reported.
	suites, ignored, _, err = apiCipherSuites("1.3", DefaultTLSCiphers)
	require.NoError(t, err)
	assert.Nil(t, suites)
	assert.False(t, ignored)
	suites, ignored, _, err = apiCipherSuites("1.3", "TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384")
	require.NoError(t, err)
	assert.Nil(t, suites)
	assert.True(t, ignored)

	// Empty: Go defaults. Invalid and insecure names fail.
	suites, _, added, err = apiCipherSuites("1.2", "")
	require.NoError(t, err)
	assert.Nil(t, suites)
	assert.Nil(t, added)
	_, _, _, err = apiCipherSuites("1.2", "NOPE")
	require.Error(t, err)
	_, _, _, err = apiCipherSuites("1.2", "TLS_RSA_WITH_RC4_128_SHA")
	require.Error(t, err)
	_, _, _, err = apiCipherSuites("2.0", "")
	require.Error(t, err)
}

func TestEffectiveBaseHRef(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	assert.Equal(t, "/", effectiveBaseHRef("", "/", log), "no root path: the base href")
	assert.Equal(t, "/ui/", effectiveBaseHRef("", "/ui/", log))
	assert.Empty(t, logs.String())

	assert.Equal(t, "/patrol", effectiveBaseHRef("/patrol", "/patrol/", log), "same path: no conflict")
	assert.Equal(t, "/patrol", effectiveBaseHRef("/patrol", "", log))
	assert.Empty(t, logs.String())

	assert.Equal(t, "/patrol", effectiveBaseHRef("/patrol", "/", log), "the root path wins over the default")
	assert.Contains(t, logs.String(), "--basehref and --rootpath had conflict: basehref: / rootpath: /patrol")
	logs.Reset()
	assert.Equal(t, "/patrol", effectiveBaseHRef("/patrol", "/other/", log))
	assert.Contains(t, logs.String(), "basehref: /other/ rootpath: /patrol")
}

// TestStaticAssets serves the static assets directory through the UI handler apiserver uses
// (OPS-111).
func TestStaticAssets(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "custom.css"), "body { color: red }")
	writeFile(t, filepath.Join(dir, "index.html"), "<html><head></head>operator index</html>")
	// A ConfigMap volume: every key links through ..data to a timestamped directory.
	writeFile(t, filepath.Join(dir, "..2026_10_16", "logo.svg"), "<svg/>")
	require.NoError(t, os.Symlink("..2026_10_16", filepath.Join(dir, "..data")))
	require.NoError(t, os.Symlink(filepath.Join("..data", "logo.svg"), filepath.Join(dir, "logo.svg")))
	// Links leading out of the directory.
	outsideDir := t.TempDir()
	secret := filepath.Join(outsideDir, "token")
	writeFile(t, secret, "outside-secret")
	require.NoError(t, os.Symlink(secret, filepath.Join(dir, "leak.txt")))
	require.NoError(t, os.Symlink(filepath.Join("..", filepath.Base(outsideDir), "token"), filepath.Join(dir, "relative.txt")))
	require.NoError(t, os.Symlink(outsideDir, filepath.Join(dir, "linkdir")))
	if _, err := os.Stat("/etc/passwd"); err == nil {
		require.NoError(t, os.Symlink("/etc/passwd", filepath.Join(dir, "passwd.css")))
	}

	embedded := fstest.MapFS{
		"index.html":    {Data: []byte("<html><head></head>embedded index</html>")},
		"assets/app.js": {Data: []byte("app()")},
	}
	fsys, root, err := withStaticAssets(embedded, dir, discard())
	require.NoError(t, err)
	require.NotNil(t, root)
	t.Cleanup(func() { _ = root.Close() })
	h, err := apiserver.NewUIHandler(fsys, "/", nil)
	require.NoError(t, err)

	resp := get(t, h, "/custom.css")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.True(t, strings.HasPrefix(resp.Header.Get("Content-Type"), "text/css"))
	assert.Equal(t, "body { color: red }", body(t, resp))
	resp = get(t, h, "/logo.svg")
	require.Equal(t, http.StatusOK, resp.StatusCode, "links inside the directory work")
	assert.Equal(t, "<svg/>", body(t, resp))
	resp = get(t, h, "/assets/app.js")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "app()", body(t, resp))
	resp = get(t, h, "/", "Accept", "text/html")
	assert.Contains(t, body(t, resp), "embedded index", "the embedded UI is looked up first (upstream order)")

	for _, p := range []string{"/leak.txt", "/relative.txt", "/linkdir/token", "/passwd.css"} {
		for _, accept := range []string{"", "text/html"} {
			resp := get(t, h, p, "Accept", accept)
			b := body(t, resp)
			assert.NotContains(t, b, "outside-secret", p)
			assert.NotContains(t, b, "root:", p)
			if accept == "" {
				assert.NotEqual(t, http.StatusOK, resp.StatusCode, p)
			}
		}
	}
	// The escape is reported as such (not as a missing file), so the handler can never
	// mistake it for a servable one.
	_, err = fsys.Open("leak.txt")
	require.Error(t, err)
	assert.False(t, errors.Is(err, fs.ErrNotExist), "an escaping link is an error, not a missing file: %v", err)
	_, err = fsys.Open("missing.css")
	assert.ErrorIs(t, err, fs.ErrNotExist)

	// A missing directory only warns (upstream message); another open error is fatal.
	var logs bytes.Buffer
	got, root2, err := withStaticAssets(embedded, filepath.Join(dir, "absent"), slog.New(slog.NewTextHandler(&logs, nil)))
	require.NoError(t, err)
	assert.Nil(t, root2)
	assert.Equal(t, fs.FS(embedded), got)
	assert.Contains(t, logs.String(), `Static assets directory \"`+filepath.Join(dir, "absent")+`\" does not exist, using only embedded assets`)
	_, _, err = withStaticAssets(embedded, filepath.Join(dir, "custom.css"), discard())
	require.Error(t, err, "a file is not a directory")
	got, root3, err := withStaticAssets(embedded, "", discard())
	require.NoError(t, err)
	assert.Nil(t, root3)
	assert.Equal(t, fs.FS(embedded), got)
}

func TestDexConnection(t *testing.T) {
	t.Parallel()
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "dex") })
	call := func(t *testing.T, rt http.RoundTripper, url string) error {
		t.Helper()
		resp, err := (&http.Client{Transport: rt, Timeout: 10 * time.Second}).Get(url)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		require.Equal(t, "dex", string(b))
		return nil
	}

	t.Run("no Dex", func(t *testing.T) {
		t.Parallel()
		addr, rt, err := dexConnection(Config{})
		require.NoError(t, err)
		assert.Empty(t, addr)
		assert.Nil(t, rt)
	})
	t.Run("plaintext", func(t *testing.T) {
		t.Parallel()
		ts := httptest.NewServer(ok)
		t.Cleanup(ts.Close)
		addr, rt, err := dexConnection(Config{DexServer: strings.TrimPrefix(ts.URL, "http://"), DexPlaintext: true, DexStrictTLS: true})
		require.NoError(t, err, "strict TLS has no effect with plaintext")
		assert.Equal(t, ts.URL, addr)
		assert.Nil(t, rt)
		require.NoError(t, call(t, rt, addr))
	})
	t.Run("default: TLS without verification", func(t *testing.T) {
		t.Parallel()
		ca := newTestCA(t)
		pair, _, _ := ca.issue(t, "dex")
		ts := tlsServer(t, pair, ok)
		addr, rt, err := dexConnection(Config{DexServer: strings.TrimPrefix(ts.URL, "https://")})
		require.NoError(t, err)
		assert.Equal(t, ts.URL, addr)
		require.NoError(t, call(t, rt, addr), "an unknown CA is accepted")
	})
	t.Run("strict", func(t *testing.T) {
		t.Parallel()
		ca := newTestCA(t)
		dexPair, dexPEM, _ := ca.issue(t, "dex")
		otherPair, _, _ := ca.issue(t, "other")
		dir := t.TempDir()
		caFile, certFile := filepath.Join(dir, "ca.crt"), filepath.Join(dir, "tls.crt")
		require.NoError(t, os.WriteFile(caFile, ca.pem, 0o600))
		require.NoError(t, os.WriteFile(certFile, dexPEM, 0o600))

		// Missing mounted files: a startup error naming the file.
		_, _, err := dexConnection(Config{DexServer: "dex:5556", DexStrictTLS: true,
			DexCAFile: filepath.Join(dir, "absent-ca.crt"), DexCertFile: certFile})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "the Dex CA certificate is needed")
		assert.Contains(t, err.Error(), "absent-ca.crt")
		_, _, err = dexConnection(Config{DexServer: "dex:5556", DexStrictTLS: true,
			DexCAFile: caFile, DexCertFile: filepath.Join(dir, "absent-tls.crt")})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "the pinned Dex certificate is needed")
		_, _, err = dexConnection(Config{DexServer: "dex:5556", DexStrictTLS: true})
		require.Error(t, err, "default paths are not mounted in tests")
		assert.Contains(t, err.Error(), filepath.Join(DefaultDexTLSDir, "ca.crt"))

		// The pinned certificate, signed by the CA: accepted.
		good := tlsServer(t, dexPair, ok)
		addr, rt, err := dexConnection(Config{DexServer: strings.TrimPrefix(good.URL, "https://"), DexStrictTLS: true,
			DexCAFile: caFile, DexCertFile: certFile})
		require.NoError(t, err)
		require.NoError(t, call(t, rt, addr))

		// Another certificate of the same CA: valid chain, rejected by the pin.
		other := tlsServer(t, otherPair, ok)
		_, rt, err = dexConnection(Config{DexServer: other.URL, DexStrictTLS: true, DexCAFile: caFile, DexCertFile: certFile})
		require.NoError(t, err)
		err = call(t, rt, other.URL)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "dex server certificate does not match")

		// A certificate of another CA: the chain does not verify.
		foreign, _, _ := newTestCA(t).issue(t, "dex")
		bad := tlsServer(t, foreign, ok)
		err = call(t, rt, bad.URL)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "certificate")
		assert.NotContains(t, err.Error(), "does not match", "the chain check comes first")
	})
}
