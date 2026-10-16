package git

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gitHTTPServer serves the bare repositories below root over git smart HTTP
// (git http-backend through net/http/cgi), guarded by an auth check.
type gitHTTPServer struct {
	*httptest.Server
	mu       sync.Mutex
	authSeen []string // Authorization header of every request, in order
}

func gitHTTPBackend(t testing.TB) string {
	t.Helper()
	out, err := exec.Command(gitBin(t), "--exec-path").Output()
	require.NoError(t, err)
	p := filepath.Join(strings.TrimSpace(string(out)), "git-http-backend")
	if _, err := os.Stat(p); err != nil {
		t.Skip("git-http-backend not available")
	}
	return p
}

func newGitHTTPServer(t testing.TB, root string, allow func(*http.Request) bool, tlsCfg *tls.Config) *gitHTTPServer {
	t.Helper()
	backend := &cgi.Handler{
		Path: gitHTTPBackend(t),
		Env: []string{
			"GIT_PROJECT_ROOT=" + root,
			"GIT_HTTP_EXPORT_ALL=1",
			"GIT_CONFIG_NOSYSTEM=1",
			"HOME=" + root,
			"REMOTE_USER=tester",
		},
		InheritEnv: []string{"PATH"},
	}
	s := &gitHTTPServer{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.authSeen = append(s.authSeen, r.Header.Get("Authorization"))
		s.mu.Unlock()
		if allow != nil && !allow(r) {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(w, r)
	})
	if tlsCfg != nil {
		s.Server = httptest.NewUnstartedServer(h)
		s.TLS = tlsCfg
		s.StartTLS()
	} else {
		s.Server = httptest.NewServer(h)
	}
	t.Cleanup(s.Close)
	return s
}

func (s *gitHTTPServer) headers() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.authSeen...)
}

func basicAuth(user, pass string) func(*http.Request) bool {
	return func(r *http.Request) bool {
		u, p, ok := r.BasicAuth()
		return ok && u == user && p == pass
	}
}

// assertNoSecretInArgs checks that no recorded command line contains a secret
// and that per-command credential files were removed.
func assertNoSecretInArgs(t *testing.T, rec *cmdRecorder, secrets ...string) {
	t.Helper()
	cmds := rec.all()
	require.NotEmpty(t, cmds)
	for _, c := range cmds {
		joined := c.Path + " " + strings.Join(c.Args, " ")
		for _, s := range secrets {
			assert.NotContains(t, joined, s, "secret in argv")
		}
		for _, kv := range c.Env {
			k, v, _ := strings.Cut(kv, "=")
			if k == askpassFileEnv || k == "GIT_SSL_KEY" || k == "GIT_SSL_CERT" || k == "GIT_SSL_CAINFO" {
				_, err := os.Stat(v)
				assert.ErrorIs(t, err, os.ErrNotExist, "credential file %s left behind", k)
				assert.NoDirExists(t, filepath.Dir(v), "credential directory left behind")
			}
		}
	}
}

func TestHTTPBasicAuthViaAskpass(t *testing.T) {
	t.Parallel()
	f := newRevisionFixture(t)
	const user, pass = "alice", "s3cr3t-Pa55word"
	srv := newGitHTTPServer(t, f.dir, basicAuth(user, pass), nil)
	repoURL := srv.URL + "/remote.git"
	ctx := testCtx(t)

	t.Run("correct credentials", func(t *testing.T) {
		t.Parallel()
		rec := &cmdRecorder{}
		c := newTestClient(t, repoURL, HTTPSCreds{Username: user, Password: pass}, withRunHook(rec.hook))
		sha, err := c.ResolveRevision(ctx, "feature")
		require.NoError(t, err)
		assert.Equal(t, f.c2, sha)
		require.NoError(t, c.CheckoutRevision(ctx, "main", false))
		got, err := c.CommitSHA(ctx)
		require.NoError(t, err)
		assert.Equal(t, f.c4, got)
		assertNoSecretInArgs(t, rec, pass)

		var askpass string
		for _, kv := range rec.all()[0].Env {
			if v, ok := strings.CutPrefix(kv, "GIT_ASKPASS="); ok {
				askpass = v
			}
		}
		exe, err := os.Executable()
		require.NoError(t, err)
		assert.Equal(t, exe, askpass)
	})
	t.Run("wrong password", func(t *testing.T) {
		t.Parallel()
		const wrong = "wrong-Pa55word-xyz"
		rec := &cmdRecorder{}
		c := newTestClient(t, repoURL, HTTPSCreds{Username: user, Password: wrong}, withRunHook(rec.hook))
		_, err := c.LsRemote(ctx)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrAuthentication)
		assert.NotContains(t, err.Error(), wrong)
		assertNoSecretInArgs(t, rec, wrong)
	})
	t.Run("no credentials", func(t *testing.T) {
		t.Parallel()
		c := newTestClient(t, repoURL, nil)
		_, err := c.LsRemote(ctx)
		assert.ErrorIs(t, err, ErrAuthentication)
	})
	t.Run("password embedded in URL", func(t *testing.T) {
		t.Parallel()
		u, err := url.Parse(repoURL)
		require.NoError(t, err)
		u.User = url.UserPassword(user, pass)
		rec := &cmdRecorder{}
		c := newTestClient(t, u.String(), nil, withRunHook(rec.hook))
		assert.NotContains(t, c.RepoURL(), pass)
		require.NoError(t, c.CheckoutRevision(ctx, "main", false))
		assertNoSecretInArgs(t, rec, pass)
		cfg := readFile(t, filepath.Join(c.Root(), ".git", "config"))
		assert.NotContains(t, cfg, pass, "password must not be stored in .git/config")

		u.User = url.UserPassword(user, "Wrong-embedded-pw")
		bad := newTestClient(t, u.String(), nil)
		_, err = bad.LsRemote(ctx)
		require.ErrorIs(t, err, ErrAuthentication)
		assert.NotContains(t, err.Error(), "Wrong-embedded-pw")
	})
}

func TestHTTPDefaultUsername(t *testing.T) {
	t.Parallel()
	f := newRevisionFixture(t)
	srv := newGitHTTPServer(t, f.dir, basicAuth(DefaultHTTPSUsername, "tok"), nil)
	c := newTestClient(t, srv.URL+"/remote.git", HTTPSCreds{Password: "tok"})
	_, err := c.LsRemote(testCtx(t))
	require.NoError(t, err)
}

func TestHTTPBearerToken(t *testing.T) {
	t.Parallel()
	f := newRevisionFixture(t)
	const token = "bearer-T0ken-value"
	srv := newGitHTTPServer(t, f.dir, func(r *http.Request) bool {
		return r.Header.Get("Authorization") == "Bearer "+token
	}, nil)
	rec := &cmdRecorder{}
	c := newTestClient(t, srv.URL+"/remote.git", HTTPSCreds{BearerToken: token}, withRunHook(rec.hook))
	ctx := testCtx(t)
	require.NoError(t, c.CheckoutRevision(ctx, "main", false))
	hs := srv.headers()
	require.NotEmpty(t, hs)
	assert.Equal(t, "Bearer "+token, hs[0], "bearer header is sent preemptively")
	assertNoSecretInArgs(t, rec, token)

	// The header is scoped to the repository origin.
	var scopedKey, unscoped bool
	for _, cmd := range rec.all() {
		for _, kv := range cmd.Env {
			if strings.HasPrefix(kv, "GIT_CONFIG_KEY_") && strings.HasSuffix(kv, "=http."+srv.URL+"/.extraHeader") {
				scopedKey = true
			}
			if strings.HasPrefix(kv, "GIT_CONFIG_KEY_") && strings.HasSuffix(kv, "=http.extraHeader") {
				unscoped = true
			}
		}
	}
	assert.False(t, unscoped, "extraHeader must never be unscoped")
	assert.True(t, scopedKey, "extraHeader must be scoped to %s", srv.URL)
}

func TestHTTPForceBasicAuth(t *testing.T) {
	t.Parallel()
	f := newRevisionFixture(t)
	srv := newGitHTTPServer(t, f.dir, basicAuth("bob", "pw-123456"), nil)
	c := newTestClient(t, srv.URL+"/remote.git", HTTPSCreds{Username: "bob", Password: "pw-123456", ForceBasicAuth: true})
	_, err := c.LsRemote(testCtx(t))
	require.NoError(t, err)
	hs := srv.headers()
	require.NotEmpty(t, hs)
	assert.Equal(t, "Basic "+base64.StdEncoding.EncodeToString([]byte("bob:pw-123456")), hs[0])

	// Without the flag the first request is anonymous (challenge-response).
	srv2 := newGitHTTPServer(t, f.dir, basicAuth("bob", "pw-123456"), nil)
	c2 := newTestClient(t, srv2.URL+"/remote.git", HTTPSCreds{Username: "bob", Password: "pw-123456"})
	_, err = c2.LsRemote(testCtx(t))
	require.NoError(t, err)
	assert.Empty(t, srv2.headers()[0])
}

// testPKI issues a CA, a server certificate for 127.0.0.1 and a client
// certificate.
type testPKI struct {
	caPEM         []byte
	serverCert    tls.Certificate
	clientCertPEM []byte
	clientKeyPEM  []byte
	pool          *x509.CertPool
}

func newTestPKI(t testing.TB) *testPKI {
	t.Helper()
	newKey := func() *ecdsa.PrivateKey {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		require.NoError(t, err)
		return k
	}
	caKey := newKey()
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	require.NoError(t, err)
	ca, err := x509.ParseCertificate(caDER)
	require.NoError(t, err)
	issue := func(serial int64, cn string, usage x509.ExtKeyUsage, ips []net.IP) ([]byte, *ecdsa.PrivateKey) {
		k := newKey()
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(serial),
			Subject:      pkix.Name{CommonName: cn},
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{usage},
			IPAddresses:  ips,
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &k.PublicKey, caKey)
		require.NoError(t, err)
		return der, k
	}
	encKey := func(k *ecdsa.PrivateKey) []byte {
		der, err := x509.MarshalECPrivateKey(k)
		require.NoError(t, err)
		return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	}
	srvDER, srvKey := issue(2, "127.0.0.1", x509.ExtKeyUsageServerAuth, []net.IP{net.ParseIP("127.0.0.1")})
	srvCert, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srvDER}), encKey(srvKey))
	require.NoError(t, err)
	cliDER, cliKey := issue(3, "client", x509.ExtKeyUsageClientAuth, nil)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return &testPKI{
		caPEM:         pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		serverCert:    srvCert,
		clientCertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cliDER}),
		clientKeyPEM:  encKey(cliKey),
		pool:          pool,
	}
}

func TestHTTPSCustomCAAndInsecure(t *testing.T) {
	t.Parallel()
	f := newRevisionFixture(t)
	pki := newTestPKI(t)
	srv := newGitHTTPServer(t, f.dir, nil, &tls.Config{Certificates: []tls.Certificate{pki.serverCert}, MinVersion: tls.VersionTLS12})
	repoURL := srv.URL + "/remote.git"
	ctx := testCtx(t)

	_, err := newTestClient(t, repoURL, nil).LsRemote(ctx)
	require.Error(t, err, "unknown CA must be rejected")

	_, err = newTestClient(t, repoURL, HTTPSCreds{CAData: pki.caPEM}).LsRemote(ctx)
	require.NoError(t, err, "CA bundle from credentials")

	_, err = newTestClient(t, repoURL, HTTPSCreds{Insecure: true}).LsRemote(ctx)
	require.NoError(t, err, "insecure credentials")

	_, err = newTestClient(t, repoURL, nil, Insecure(true)).LsRemote(ctx)
	require.NoError(t, err, "insecure client option")
}

func TestHTTPSClientCertificate(t *testing.T) {
	t.Parallel()
	f := newRevisionFixture(t)
	pki := newTestPKI(t)
	srv := newGitHTTPServer(t, f.dir, nil, &tls.Config{
		Certificates: []tls.Certificate{pki.serverCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pki.pool,
		MinVersion:   tls.VersionTLS12,
	})
	repoURL := srv.URL + "/remote.git"
	ctx := testCtx(t)

	_, err := newTestClient(t, repoURL, HTTPSCreds{CAData: pki.caPEM}).LsRemote(ctx)
	require.Error(t, err, "client certificate required")

	rec := &cmdRecorder{}
	c := newTestClient(t, repoURL, HTTPSCreds{
		CAData:         pki.caPEM,
		ClientCertData: string(pki.clientCertPEM),
		ClientCertKey:  string(pki.clientKeyPEM),
	}, withRunHook(rec.hook))
	require.NoError(t, c.CheckoutRevision(ctx, "main", false))
	assertNoSecretInArgs(t, rec, string(pki.clientKeyPEM))
}

func TestHTTPProxy(t *testing.T) {
	t.Parallel()
	f := newRevisionFixture(t)
	srv := newGitHTTPServer(t, f.dir, nil, nil)
	target, err := url.Parse(srv.URL)
	require.NoError(t, err)

	var mu sync.Mutex
	var proxied []string
	proxy := httptest.NewServer(&httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			mu.Lock()
			proxied = append(proxied, r.In.URL.Host)
			mu.Unlock()
			r.SetURL(target)
			r.Out.URL.Path = r.In.URL.Path
			r.Out.URL.RawQuery = r.In.URL.RawQuery
		},
	})
	t.Cleanup(proxy.Close)

	// The repository host does not resolve: only the proxy can reach it.
	repoURL := "http://git.example.test/remote.git"
	ctx := testCtx(t)
	c := newTestClient(t, repoURL, nil, Proxy(proxy.URL))
	sha, err := c.ResolveRevision(ctx, "main")
	require.NoError(t, err)
	assert.Equal(t, f.c4, sha)
	require.NoError(t, c.CheckoutRevision(ctx, "main", false))
	mu.Lock()
	assert.NotEmpty(t, proxied)
	for _, h := range proxied {
		assert.Equal(t, "git.example.test", h)
	}
	mu.Unlock()

	bypass := newTestClient(t, repoURL, nil, Proxy(proxy.URL), NoProxy("example.test"), Timeout(30*time.Second))
	_, err = bypass.LsRemote(ctx)
	require.Error(t, err, "noProxy host must be contacted directly (and does not resolve)")
}

func TestHTTPPush(t *testing.T) {
	t.Parallel()
	f := newRevisionFixture(t)
	f.bareGit("config", "http.receivepack", "true")
	const user, pass = "pusher", "push-Secret-1"
	srv := newGitHTTPServer(t, f.dir, basicAuth(user, pass), nil)
	ctx := testCtx(t)
	rec := &cmdRecorder{}
	c := newTestClient(t, srv.URL+"/remote.git", HTTPSCreds{Username: user, Password: pass}, withRunHook(rec.hook))
	require.NoError(t, c.SetAuthor("Hydrator", "hydrator@example.com"))
	require.NoError(t, c.Init(ctx))
	require.NoError(t, c.Fetch(ctx, ""))
	require.NoError(t, c.CheckoutOrOrphan(ctx, "hydrated"))
	require.NoError(t, os.WriteFile(filepath.Join(c.Root(), "manifest.yaml"), []byte("kind: X\n"), 0o644))
	sha, err := c.CommitAll(ctx, "hydrate", false)
	require.NoError(t, err)
	require.NoError(t, c.Push(ctx, "hydrated", false))
	assert.Equal(t, sha, f.bareGit("rev-parse", "refs/heads/hydrated"))
	assertNoSecretInArgs(t, rec, pass)
}

// newRedirectServer answers every request with a 302 to the same request URI on
// target (a different origin), optionally only after allow accepted the request.
func newRedirectServer(t testing.TB, target string, allow func(*http.Request) bool) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if allow != nil && !allow(r) {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, target+r.URL.RequestURI(), http.StatusFound)
	}))
	t.Cleanup(s.Close)
	return s
}

// TestHTTPCredentialsNeverFollowCrossOriginRedirect is a regression test: git's
// default http.followRedirects=initial rebases later requests onto the redirect
// target but keeps sending the http.<origin>/.extraHeader loaded for the original
// URL, so a preemptive Authorization header reached the other origin.
func TestHTTPCredentialsNeverFollowCrossOriginRedirect(t *testing.T) {
	t.Parallel()
	f := newRevisionFixture(t)
	ctx := testCtx(t)

	headerCreds := map[string]struct {
		creds  HTTPSCreds
		secret string
	}{
		"bearer":       {HTTPSCreds{BearerToken: "redirect-Bearer-T0ken"}, "redirect-Bearer-T0ken"},
		"forced basic": {HTTPSCreds{Username: "u", Password: "redirect-Basic-Pw", ForceBasicAuth: true}, "redirect-Basic-Pw"},
	}
	for name, tc := range headerCreds {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			target := newGitHTTPServer(t, f.dir, nil, nil)
			redirector := newRedirectServer(t, target.URL, nil)
			rec := &cmdRecorder{}
			c := newTestClient(t, redirector.URL+"/remote.git", tc.creds, withRunHook(rec.hook))

			_, lsErr := c.LsRemote(ctx)
			coErr := c.CheckoutRevision(ctx, "main", false)
			for _, h := range target.headers() {
				assert.Empty(t, h, "redirect target received an Authorization header")
			}
			require.Error(t, lsErr, "a redirect must not be followed while an Authorization header is configured")
			assert.Contains(t, lsErr.Error(), "302", "the refused redirect is visible in the error")
			assert.NotContains(t, lsErr.Error(), tc.secret)
			require.Error(t, coErr)
			assert.NotContains(t, coErr.Error(), tc.secret)
			assertNoSecretInArgs(t, rec, tc.secret)
		})
	}

	t.Run("askpass credentials still follow redirects", func(t *testing.T) {
		t.Parallel()
		// Upstream parity: without a preemptive header redirects are followed; the
		// askpass helper never answers for the new origin.
		target := newGitHTTPServer(t, f.dir, nil, nil)
		redirector := newRedirectServer(t, target.URL, nil)
		c := newTestClient(t, redirector.URL+"/remote.git", HTTPSCreds{Username: "u", Password: "askpass-Pw-1"})
		sha, err := c.ResolveRevision(ctx, "main")
		require.NoError(t, err)
		assert.Equal(t, f.c4, sha)
		require.NoError(t, c.CheckoutRevision(ctx, "main", false))
		require.NotEmpty(t, target.headers())
		for _, h := range target.headers() {
			assert.Empty(t, h)
		}
	})

	t.Run("askpass refuses the redirect origin", func(t *testing.T) {
		t.Parallel()
		const user, pass = "carol", "askpass-Pw-2"
		target := newGitHTTPServer(t, f.dir, basicAuth(user, pass), nil)
		redirector := newRedirectServer(t, target.URL, basicAuth(user, pass))
		c := newTestClient(t, redirector.URL+"/remote.git", HTTPSCreds{Username: user, Password: pass})
		_, err := c.LsRemote(ctx)
		require.ErrorIs(t, err, ErrAuthentication)
		for _, h := range target.headers() {
			assert.Empty(t, h, "redirect target received credentials")
		}
	})
}
