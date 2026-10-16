package repository

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"

	v1alpha1 "github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// TestSCMProviderTokenAndBasicAuth covers credentials supplied by an SCMProvider token or
// basic auth Secret: used only when the object's own Secret has none, and only for
// repositories on the provider's host.
func TestSCMProviderTokenAndBasicAuth(t *testing.T) {
	t.Parallel()
	f := newFixture(t, []runtime.Object{
		secret("gl-token", v1alpha1.LabelValueSecretTypeSCMCreds, map[string]string{"token": "glpat-provider-token"}),
		secret("bb-basic", v1alpha1.LabelValueSecretTypeSCMCreds, map[string]string{"username": "bot", "password": "bb-app-password"}),
		secret("bb-nopass", v1alpha1.LabelValueSecretTypeSCMCreds, map[string]string{"username": "bot"}),
		secret("own", v1alpha1.LabelValueSecretTypeRepository, map[string]string{"username": "me", "password": "own-password"}),
		&v1alpha1.SCMProvider{ObjectMeta: metaName("gitlab"), Spec: v1alpha1.SCMProviderSpec{
			Type: v1alpha1.SCMProviderTypeGitLab,
			Auth: &v1alpha1.SCMProviderAuth{TokenSecretRef: &v1alpha1.SecretKeyReference{Name: "gl-token", Key: "token"}},
		}},
		&v1alpha1.SCMProvider{ObjectMeta: metaName("bitbucket"), Spec: v1alpha1.SCMProviderSpec{
			Type: v1alpha1.SCMProviderTypeBitbucketServer, APIURL: "https://bitbucket.example.com/rest",
			Auth: &v1alpha1.SCMProviderAuth{BasicAuthSecretRef: &v1alpha1.LocalSecretReference{Name: "bb-basic"}},
		}},
		&v1alpha1.SCMProvider{ObjectMeta: metaName("bitbucket-broken"), Spec: v1alpha1.SCMProviderSpec{
			Type: v1alpha1.SCMProviderTypeBitbucketServer, APIURL: "https://bitbucket.example.com/rest",
			Auth: &v1alpha1.SCMProviderAuth{BasicAuthSecretRef: &v1alpha1.LocalSecretReference{Name: "bb-nopass"}},
		}},
		&v1alpha1.SCMProvider{ObjectMeta: metaName("noauth"), Spec: v1alpha1.SCMProviderSpec{Type: v1alpha1.SCMProviderTypeGitHub}},
		repo("gl", "https://gitlab.com/group/app", func(r *v1alpha1.Repository) {
			r.Spec.SCMProviderRef = &v1alpha1.SCMProviderReference{Name: "gitlab"}
		}),
		repo("gl-own", "https://gitlab.com/group/own", withCreds("own"), func(r *v1alpha1.Repository) {
			r.Spec.SCMProviderRef = &v1alpha1.SCMProviderReference{Name: "gitlab"}
		}),
		repo("bb", "https://bitbucket.example.com/scm/prj/app.git", func(r *v1alpha1.Repository) {
			r.Spec.SCMProviderRef = &v1alpha1.SCMProviderReference{Name: "bitbucket"}
		}),
		repo("bb-broken", "https://bitbucket.example.com/scm/prj/broken.git", func(r *v1alpha1.Repository) {
			r.Spec.SCMProviderRef = &v1alpha1.SCMProviderReference{Name: "bitbucket-broken"}
		}),
		repo("noauth", "https://github.com/org/noauth", func(r *v1alpha1.Repository) {
			r.Spec.SCMProviderRef = &v1alpha1.SCMProviderReference{Name: "noauth"}
		}),
	})
	ctx := t.Context()
	gl, err := f.resolver.GetRepository(ctx, "https://gitlab.com/group/app", "")
	require.NoError(t, err)
	assert.Equal(t, "glpat-provider-token", gl.Password)
	own, err := f.resolver.GetRepository(ctx, "https://gitlab.com/group/own", "")
	require.NoError(t, err)
	assert.Equal(t, "own-password", own.Password, "the object's own Secret wins")
	bb, err := f.resolver.GetRepository(ctx, "https://bitbucket.example.com/scm/prj/app.git", "")
	require.NoError(t, err)
	assert.Equal(t, "bot", bb.Username)
	assert.Equal(t, "bb-app-password", bb.Password)
	_, err = f.resolver.GetRepository(ctx, "https://bitbucket.example.com/scm/prj/broken.git", "")
	assert.ErrorIs(t, err, ErrSecretKeyMissing)
	none, err := f.resolver.Resolve(ctx, Query{URL: "https://github.com/org/noauth"})
	require.NoError(t, err)
	assert.Equal(t, MethodNone, none.Method)
}

// forwardProxy is an HTTP forward proxy (absolute-URL requests and CONNECT tunnels) that
// counts the requests it relays.
type forwardProxy struct {
	*httptest.Server
	relayed atomic.Int32
}

func newForwardProxy(t *testing.T) *forwardProxy {
	t.Helper()
	p := &forwardProxy{}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			p.relayed.Add(1)
			tunnel(w, r)
			return
		}
		if !r.URL.IsAbs() {
			http.Error(w, "not a proxy request", http.StatusBadRequest)
			return
		}
		p.relayed.Add(1)
		out := r.Clone(r.Context())
		out.RequestURI = ""
		resp, err := http.DefaultTransport.RoundTrip(out)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
		buf := make([]byte, 32<<10)
		for {
			n, rerr := resp.Body.Read(buf)
			if n > 0 {
				_, _ = w.Write(buf[:n])
			}
			if rerr != nil {
				return
			}
		}
	}))
	t.Cleanup(p.Close)
	return p
}

// tunnel serves a CONNECT request by splicing the client and the target.
func tunnel(w http.ResponseWriter, r *http.Request) {
	dst, err := net.DialTimeout("tcp", r.Host, 5*time.Second)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = dst.Close()
		http.Error(w, "hijacking unsupported", http.StatusInternalServerError)
		return
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		_ = dst.Close()
		return
	}
	_, _ = conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	go func() {
		defer func() { _ = dst.Close() }()
		_, _ = io.Copy(dst, rw)
	}()
	defer func() { _ = conn.Close() }()
	_, _ = io.Copy(conn, dst)
}

// TestGitHubAppProxy covers REPO-015/REPO-021/REPO-022 for token requests: the repository
// proxy carries GitHub API calls, noProxy bypasses it, and an unsafe proxy URL is dropped
// with a security log instead of being used.
func TestGitHubAppProxy(t *testing.T) {
	t.Parallel()
	_, keyPEM := rsaKey(t)
	clock := newFakeClock(time.Now())
	gh := newFakeGitHub(t, clock, 50, nil)
	proxy := newForwardProxy(t)
	app := func(r *v1alpha1.Repository) { r.Spec.GitHubApp = githubApp(50, 3, gh.apiURL()) }
	f := newFixture(t, []runtime.Object{
		secret("app", v1alpha1.LabelValueSecretTypeRepository, map[string]string{"githubAppPrivateKey": keyPEM}),
		repo("proxied", "https://github.com/org/proxied", withCreds("app"), app, func(r *v1alpha1.Repository) { r.Spec.Proxy = proxy.URL }),
		repo("bypassed", "https://github.com/org/bypassed", withCreds("app"), app, func(r *v1alpha1.Repository) {
			r.Spec.Proxy = proxy.URL
			r.Spec.NoProxy = "127.0.0.1"
			// A different installation so the token is not shared with "proxied".
			r.Spec.GitHubApp = githubApp(50, 4, gh.apiURL())
		}),
		repo("unsafe", "https://github.com/org/unsafe", withCreds("app"), func(r *v1alpha1.Repository) {
			r.Spec.Proxy = "http://bad;host:3128"
			r.Spec.GitHubApp = githubApp(50, 5, gh.apiURL())
		}),
	}, withClock(clock), withTransport(trustTransport(gh.Server)))
	ctx := t.Context()

	res, err := f.resolver.GetRepository(ctx, "https://github.com/org/proxied", "")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(res.Password, "ghs_installation_3_"))
	assert.Equal(t, proxy.URL, res.Proxy, "the proxy is forwarded to the repo-server too")
	assert.Positive(t, proxy.relayed.Load())

	before := proxy.relayed.Load()
	res, err = f.resolver.GetRepository(ctx, "https://github.com/org/bypassed", "")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(res.Password, "ghs_installation_4_"))
	assert.Equal(t, before, proxy.relayed.Load(), "noProxy bypasses the proxy")

	res, err = f.resolver.GetRepository(ctx, "https://github.com/org/unsafe", "")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(res.Password, "ghs_installation_5_"))
	assert.Contains(t, f.logs.String(), "rejecting unsafe proxy URL for token requests")
	assert.Contains(t, f.logs.String(), `"CWE":78`)
}
