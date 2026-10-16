package repository

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"

	v1alpha1 "github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/pkg/scm"
)

// recordingServer is a TLS server answering every request with a fixed status and body,
// counting the requests and those that carried an Authorization header.
type recordingServer struct {
	*httptest.Server
	requests atomic.Int32
	auth     atomic.Int32
}

func newRecordingServer(t *testing.T, status int, body string) *recordingServer {
	t.Helper()
	s := &recordingServer{}
	s.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		if r.Header.Get("Authorization") != "" {
			s.auth.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(s.Close)
	return s
}

// internalBody is a large response of an "internal" service that must never be reflected.
var internalBody = `{"error":"invalid_grant","message":"` + strings.Repeat("INTERNAL-SECRET-DATA ", 5000) + `"}`

func scmRef(name string) *v1alpha1.SCMProviderReference {
	return &v1alpha1.SCMProviderReference{Name: name}
}

// TestSCMProviderCannotRedirectCredentials is the regression test of the SCMProvider host
// restriction bypass: a Repository (or a template) referencing an SCMProvider cannot choose
// the GitHub API URL, the proxy or the TLS verification used with the provider's App key,
// nor send the provider's token over http, through a proxy over unverified TLS or with
// verification disabled, and an inherited App key keeps its template's endpoint.
func TestSCMProviderCannotRedirectCredentials(t *testing.T) {
	t.Parallel()
	_, keyPEM := rsaKey(t)
	clock := newFakeClock(time.Now())
	gh := newFakeGitHub(t, clock, 77, nil)
	attacker := newRecordingServer(t, http.StatusCreated, `{"token":"x","expires_at":"2099-01-01T00:00:00Z"}`)
	proxy := newForwardProxy(t)
	ghe := gh.URL + "/api/v3"
	app := func(inst int64) *v1alpha1.SCMProviderAuth {
		return &v1alpha1.SCMProviderAuth{GitHubApp: &v1alpha1.SCMProviderGitHubApp{
			AppID: 77, InstallationID: ptrTo(inst), PrivateKeySecretRef: v1alpha1.SecretKeyReference{Name: "corp-app", Key: "private-key"},
		}}
	}
	token := &v1alpha1.SCMProviderAuth{TokenSecretRef: &v1alpha1.SecretKeyReference{Name: "gl-token", Key: "token"}}
	insecure := func(r *v1alpha1.Repository) { r.Spec.Insecure = true }
	withProxy := func(p string) func(*v1alpha1.Repository) { return func(r *v1alpha1.Repository) { r.Spec.Proxy = p } }
	f := newFixture(t, []runtime.Object{
		secret("corp-app", v1alpha1.LabelValueSecretTypeSCMCreds, map[string]string{"private-key": keyPEM}),
		secret("gl-token", v1alpha1.LabelValueSecretTypeSCMCreds, map[string]string{"token": "glpat-provider-token"}),
		secret("tpl-app", v1alpha1.LabelValueSecretTypeRepoCreds, map[string]string{"githubAppPrivateKey": keyPEM}),
		&v1alpha1.SCMProvider{ObjectMeta: metaName("corp-github"), Spec: v1alpha1.SCMProviderSpec{Type: v1alpha1.SCMProviderTypeGitHub, Auth: app(5)}},
		&v1alpha1.SCMProvider{ObjectMeta: metaName("ghe"), Spec: v1alpha1.SCMProviderSpec{Type: v1alpha1.SCMProviderTypeGitHub, APIURL: ghe, Auth: app(5)}},
		&v1alpha1.SCMProvider{ObjectMeta: metaName("ghe-insecure"), Spec: v1alpha1.SCMProviderSpec{Type: v1alpha1.SCMProviderTypeGitHub, APIURL: ghe, Insecure: true, Auth: app(6)}},
		&v1alpha1.SCMProvider{ObjectMeta: metaName("gitlab"), Spec: v1alpha1.SCMProviderSpec{Type: v1alpha1.SCMProviderTypeGitLab, Auth: token}},
		&v1alpha1.SCMProvider{ObjectMeta: metaName("gitlab-insecure"), Spec: v1alpha1.SCMProviderSpec{
			Type: v1alpha1.SCMProviderTypeGitLab, WebURL: "https://gitlab.insecure.example", Insecure: true, Auth: token,
		}},
		&v1alpha1.SCMProvider{ObjectMeta: metaName("gitea-http"), Spec: v1alpha1.SCMProviderSpec{
			Type: v1alpha1.SCMProviderTypeGitea, APIURL: "http://gitea.internal/api/v1", Auth: token,
		}},
		// The reviewer's scenario: public GitHub provider, attacker-chosen enterpriseBaseURL.
		repo("redirect", "https://github.com/victim/repo", withProject("tenant-b"), func(r *v1alpha1.Repository) {
			r.Spec.SCMProviderRef = scmRef("corp-github")
			r.Spec.GitHubApp = githubApp(77, 0, attacker.URL)
		}),
		repo("same-base", "https://127.0.0.1/org/same", func(r *v1alpha1.Repository) {
			r.Spec.SCMProviderRef = scmRef("ghe")
			r.Spec.GitHubApp = githubApp(77, 0, gh.URL+"/api/v3/")
		}),
		repo("app-insecure", "https://127.0.0.1/org/app-insecure", insecure, func(r *v1alpha1.Repository) { r.Spec.SCMProviderRef = scmRef("ghe") }),
		// Its own installation, so pkg/scm/github's process-wide token cache cannot serve it.
		repo("app-proxy", "https://127.0.0.1/org/app-proxy", withProxy(proxy.URL), func(r *v1alpha1.Repository) {
			r.Spec.SCMProviderRef = scmRef("ghe")
			r.Spec.GitHubApp = githubApp(77, 8, "")
		}),
		repo("app-proxy-unverified", "https://127.0.0.1/org/x", withProxy(proxy.URL), func(r *v1alpha1.Repository) { r.Spec.SCMProviderRef = scmRef("ghe-insecure") }),
		repo("gl-proxy-insecure", "https://gitlab.com/group/a", insecure, withProxy("http://attacker.example:3128"), func(r *v1alpha1.Repository) { r.Spec.SCMProviderRef = scmRef("gitlab") }),
		repo("gl-http", "http://gitlab.com/group/b", func(r *v1alpha1.Repository) { r.Spec.SCMProviderRef = scmRef("gitlab") }),
		repo("gl-ssh", "git@gitlab.com:group/c.git", func(r *v1alpha1.Repository) { r.Spec.SCMProviderRef = scmRef("gitlab") }),
		repo("gl-insecure", "https://gitlab.com/group/d", insecure, func(r *v1alpha1.Repository) { r.Spec.SCMProviderRef = scmRef("gitlab") }),
		repo("gl-proxy", "https://gitlab.com/group/e", withProxy("http://proxy.corp.example:3128"), func(r *v1alpha1.Repository) { r.Spec.SCMProviderRef = scmRef("gitlab") }),
		repo("gli-insecure", "https://gitlab.insecure.example/g/a", insecure, func(r *v1alpha1.Repository) { r.Spec.SCMProviderRef = scmRef("gitlab-insecure") }),
		repo("gli-insecure-proxy", "https://gitlab.insecure.example/g/b", insecure, withProxy("http://proxy.corp.example:3128"), func(r *v1alpha1.Repository) {
			r.Spec.SCMProviderRef = scmRef("gitlab-insecure")
		}),
		repo("gitea-http", "http://gitea.internal/org/a", func(r *v1alpha1.Repository) { r.Spec.SCMProviderRef = scmRef("gitea-http") }),
		// Templates: an App key of the template's own Secret, and one from an SCMProvider.
		cred("app-tpl", "https://git.corp.example/", credWithCreds("tpl-app"), func(c *v1alpha1.RepositoryCredential) {
			c.Spec.GitHubApp = githubApp(77, 9, gh.apiURL())
		}),
		cred("provider-tpl", "https://127.0.0.1/tpl/", func(c *v1alpha1.RepositoryCredential) { c.Spec.SCMProviderRef = scmRef("ghe") }),
		repo("inherits-app", "https://git.corp.example/org/app", withProject("tenant-b"), insecure, withProxy(proxy.URL), func(r *v1alpha1.Repository) {
			r.Spec.GitHubApp = githubApp(77, 0, attacker.URL)
		}),
		repo("inherits-provider", "https://127.0.0.1/tpl/app", withProject("tenant-b"), func(r *v1alpha1.Repository) {
			r.Spec.GitHubApp = githubApp(77, 0, attacker.URL)
		}),
		repo("inherits-provider-insecure", "https://127.0.0.1/tpl/other", withProject("tenant-b"), insecure, withProxy(proxy.URL)),
	}, withClock(clock), withTransport(trustTransport(gh.Server, attacker.Server)))
	ctx := t.Context()
	resolve := func(url, project string) (*Resolution, error) {
		return f.resolver.Resolve(ctx, Query{URL: url, Project: project})
	}

	rejected := []struct{ url, project, contains string }{
		{"https://github.com/victim/repo", "tenant-b", "enterpriseBaseURL must be empty or the API URL"},
		{"https://127.0.0.1/org/app-insecure", "", "cannot be used with insecure"},
		{"https://127.0.0.1/org/x", "", "whose API TLS verification is disabled"},
		{"https://gitlab.com/group/a", "", "cannot be used with insecure"},
		{"http://gitlab.com/group/b", "", "only sent over https"},
		{"git@gitlab.com:group/c.git", "", "only sent over https"},
		{"https://gitlab.com/group/d", "", "cannot be used with insecure"},
		{"https://gitlab.insecure.example/g/b", "", "through a proxy only over verified https"},
		{"https://127.0.0.1/tpl/other", "tenant-b", "cannot be used with insecure"},
	}
	for _, tc := range rejected {
		_, err := resolve(tc.url, tc.project)
		require.Error(t, err, tc.url)
		assert.ErrorIs(t, err, ErrInvalidConfiguration, tc.url)
		assert.Contains(t, err.Error(), tc.contains, tc.url)
		assert.NotContains(t, err.Error(), "glpat-provider-token", tc.url)
	}
	assert.Zero(t, attacker.requests.Load(), "no request reaches the attacker's API URL")

	// The provider's own API URL (spelled differently) is accepted.
	res, err := resolve("https://127.0.0.1/org/same", "")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(res.Repository.Password, "ghs_installation_5_"))

	// A proxy is allowed over verified TLS: the JWT requests are tunneled through it.
	before := proxy.relayed.Load()
	res, err = resolve("https://127.0.0.1/org/app-proxy", "")
	require.NoError(t, err)
	assert.Equal(t, proxy.URL, res.Repository.Proxy)
	assert.Greater(t, proxy.relayed.Load(), before)

	for _, url := range []string{"https://gitlab.com/group/e", "https://gitlab.insecure.example/g/a", "http://gitea.internal/org/a"} {
		res, err := resolve(url, "")
		require.NoError(t, err, url)
		assert.Equal(t, "glpat-provider-token", res.Repository.Password, url)
	}

	// An inherited App key keeps its template's endpoint and transport: the repository's
	// enterpriseBaseURL, proxy and insecure flag are not used for the JWT requests.
	before = proxy.relayed.Load()
	res, err = resolve("https://git.corp.example/org/app", "tenant-b")
	require.NoError(t, err)
	assert.True(t, res.Inherited)
	assert.True(t, strings.HasPrefix(res.Repository.Password, "ghs_installation_9_"))
	assert.Equal(t, before, proxy.relayed.Load(), "the repository's proxy does not carry the template's JWTs")
	res, err = resolve("https://127.0.0.1/tpl/app", "tenant-b")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(res.Repository.Password, "ghs_installation_5_"))
	assert.Zero(t, attacker.requests.Load())
	assert.Zero(t, attacker.auth.Load())
}

// TestSecretReferencesAreChecked is the regression test of credentialsRef borrowing: the
// referenced Secret must carry the secret-type of the referencing kind and purpose, and a
// project-scoped Repository may only use Secrets bound to its project.
func TestSecretReferencesAreChecked(t *testing.T) {
	t.Parallel()
	wrongOwner := secret("wrong-owner", v1alpha1.LabelValueSecretTypeRepository, map[string]string{"password": "tenant-a-pass"})
	wrongOwner.OwnerReferences = []metav1.OwnerReference{{APIVersion: v1alpha1.GroupVersion.String(), Kind: "Repository", Name: "a-repo", UID: "recreated"}}
	foreignKind := secret("foreign-kind", v1alpha1.LabelValueSecretTypeRepository, map[string]string{"password": "tenant-a-pass"})
	foreignKind.OwnerReferences = []metav1.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Repository", Name: "b-owner", UID: repoUID("b-owner")}}
	f := newFixture(t, []runtime.Object{
		secret("bb-basic", v1alpha1.LabelValueSecretTypeSCMCreds, map[string]string{"username": "bot", "password": "bb-app-password"}),
		secret("corp-write", v1alpha1.LabelValueSecretTypeRepoCredsWrite, map[string]string{"password": "push-secret"}),
		secret("global-read", v1alpha1.LabelValueSecretTypeRepository, map[string]string{"password": "global-pass"}),
		ownedBy(secret("tenant-a", v1alpha1.LabelValueSecretTypeRepository, map[string]string{"password": "tenant-a-pass"}), "a-repo"),
		ownedBy(secret("shared-b", v1alpha1.LabelValueSecretTypeRepository, map[string]string{"password": "tenant-b-pass"}), "b-owner"),
		wrongOwner, foreignKind,
		&v1alpha1.SCMProvider{ObjectMeta: metaName("gl"), Spec: v1alpha1.SCMProviderSpec{
			Type: v1alpha1.SCMProviderTypeGitLab, Auth: &v1alpha1.SCMProviderAuth{TokenSecretRef: &v1alpha1.SecretKeyReference{Name: "global-read", Key: "password"}},
		}},
		repo("steal-scm", "https://attacker.example/a", withProject("tenant-b"), withCreds("bb-basic")),
		repo("steal-write", "https://attacker.example/b", withProject("tenant-b"), withCreds("corp-write")),
		repo("global-steal-write", "https://attacker.example/c", withCreds("corp-write")),
		repo("steal-tenant", "https://attacker.example/d", withProject("tenant-b"), withCreds("tenant-a")),
		repo("steal-global", "https://attacker.example/e", withProject("tenant-b"), withCreds("global-read")),
		repo("a2", "https://attacker.example/f", withProject("tenant-a"), withCreds("wrong-owner")),
		repo("b-foreign", "https://attacker.example/g", withProject("tenant-b"), withCreds("foreign-kind")),
		repo("write-with-read", "https://git.example/w", withPurpose(write), withCreds("global-read")),
		repo("uses-gl", "https://gitlab.com/x/y", func(r *v1alpha1.Repository) { r.Spec.SCMProviderRef = scmRef("gl") }),
		cred("tpl-wrong", "https://tpl.example/", credWithCreds("global-read")),
		repo("a-repo", "https://git.example/a", withProject("tenant-a"), withCreds("tenant-a")),
		repo("b-owner", "https://git.example/b-owner", withProject("tenant-b"), withCreds("shared-b")),
		repo("b-other", "https://git.example/b-other", withProject("tenant-b"), withCreds("shared-b")),
		repo("global-ok", "https://git.example/g", withCreds("global-read")),
	})
	ctx := t.Context()
	rejected := []struct {
		url, project string
		purpose      v1alpha1.RepositoryPurpose
		contains     string
	}{
		{"https://attacker.example/a", "tenant-b", read, `secret "bb-basic" has cd.patrol.example/secret-type="scm-creds" but this reference requires "repository"`},
		{"https://attacker.example/b", "tenant-b", read, `requires "repository"`},
		{"https://attacker.example/c", "", read, `secret "corp-write" has cd.patrol.example/secret-type="repo-write-creds"`},
		{"https://attacker.example/d", "tenant-b", read, `secret "tenant-a" is not bound to project "tenant-b"`},
		{"https://attacker.example/e", "tenant-b", read, `secret "global-read" is not bound to project "tenant-b"`},
		{"https://attacker.example/f", "tenant-a", read, `secret "wrong-owner" is not bound to project "tenant-a"`},
		{"https://attacker.example/g", "tenant-b", read, `secret "foreign-kind" is not bound`},
		{"https://git.example/w", "", write, `requires "repository-write"`},
		{"https://gitlab.com/x/y", "", read, `SCMProvider "gl": secret "global-read" has cd.patrol.example/secret-type="repository" but this reference requires "scm-creds"`},
		{"https://tpl.example/stub", "", read, `RepositoryCredential "tpl-wrong": secret "global-read"`},
	}
	for _, tc := range rejected {
		_, err := f.resolver.ResolveForURL(ctx, tc.url, tc.project, tc.purpose)
		require.Error(t, err, tc.url)
		assert.ErrorIs(t, err, ErrInvalidConfiguration, tc.url)
		assert.Equal(t, v1alpha1.ReasonInvalidConfiguration, ConditionReason(err), tc.url)
		assert.Contains(t, err.Error(), tc.contains, tc.url)
		for _, v := range []string{"bb-app-password", "push-secret", "tenant-a-pass", "global-pass"} {
			assert.NotContains(t, err.Error(), v, tc.url)
		}
	}
	allowed := []struct{ url, project, password string }{
		{"https://git.example/a", "tenant-a", "tenant-a-pass"},
		{"https://git.example/b-owner", "tenant-b", "tenant-b-pass"},
		{"https://git.example/b-other", "tenant-b", "tenant-b-pass"},
		{"https://git.example/g", "", "global-pass"},
	}
	for _, tc := range allowed {
		got, err := f.resolver.ResolveForURL(ctx, tc.url, tc.project, read)
		require.NoError(t, err, tc.url)
		assert.Equal(t, tc.password, got.Password, tc.url)
	}
	// Unsaved input supplies its own data: no Secret is dereferenced.
	rs := &fakeRepoServer{}
	g := newFixture(t, nil, withRepoServer(rs))
	_, err := g.resolver.ValidateWithSecretData(ctx, repo("new", "https://git.example/new", withProject("p"), withCreds("anything")), map[string][]byte{"password": []byte("typed")})
	require.NoError(t, err)
	assert.Equal(t, "typed", rs.lastCall(t).Password)
}

// TestStatusWriterRejectsStaleResults is the regression test of the lost-update race: a
// check of an older generation, of a recreated object, or older than the recorded one is
// not written.
func TestStatusWriterRejectsStaleResults(t *testing.T) {
	t.Parallel()
	f := newFixture(t, []runtime.Object{repo("r", "https://git.example/r", func(r *v1alpha1.Repository) { r.Generation = 1 })})
	var updates atomic.Int32
	f.dyn.PrependReactor("update", "repositories", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() == "status" {
			updates.Add(1)
		}
		return false, nil, nil
	})
	ctx := t.Context()
	w := NewStatusWriter(f.dyn)
	validated, err := f.lister.Repository("r")
	require.NoError(t, err)
	t0 := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *metav1.Time { v := metav1.NewTime(t0.Add(d)); return &v }
	ok := v1alpha1.ConnectionState{Status: v1alpha1.ConnectionStatusSuccessful, AttemptedAt: at(0)}

	// The spec changes while the check runs.
	u, err := f.dyn.Resource(RepositoryGVR).Namespace(testNS).Get(ctx, "r", metav1.GetOptions{})
	require.NoError(t, err)
	u.SetGeneration(2)
	require.NoError(t, unstructured.SetNestedField(u.Object, int64(5), "spec", "depth"))
	_, err = f.dyn.Resource(RepositoryGVR).Namespace(testNS).Update(ctx, u, metav1.UpdateOptions{})
	require.NoError(t, err)
	out, err := w.WriteConnectionState(ctx, validated, ok, nil)
	require.ErrorIs(t, err, ErrStaleConnectionState)
	assert.Contains(t, err.Error(), "generation 1 was validated but the repository is at generation 2")
	assert.Nil(t, out.Status.ConnectionState)
	assert.Zero(t, updates.Load())

	// A check of the current generation is written with that generation.
	current := validated.DeepCopy()
	current.Generation = 2
	failed := v1alpha1.ConnectionState{Status: v1alpha1.ConnectionStatusFailed, Message: "x", AttemptedAt: at(10 * time.Second)}
	out, err = w.WriteConnectionState(ctx, current, failed, errors.New("x"))
	require.NoError(t, err)
	assert.Equal(t, int64(2), out.Status.ObservedGeneration)
	assert.Equal(t, int32(1), updates.Load())

	// An older check finishing later does not overwrite it.
	_, err = w.WriteConnectionState(ctx, current, ok, nil)
	require.ErrorIs(t, err, ErrStaleConnectionState)
	assert.Contains(t, err.Error(), "a later connection check is already recorded")
	stored, err := f.dyn.Resource(RepositoryGVR).Namespace(testNS).Get(ctx, "r", metav1.GetOptions{})
	require.NoError(t, err)
	status, _, _ := unstructured.NestedString(stored.Object, "status", "connectionState", "status")
	assert.Equal(t, string(v1alpha1.ConnectionStatusFailed), status)

	// Checks in the same second cannot be ordered at the stored precision and are written.
	sameSecond := v1alpha1.ConnectionState{Status: v1alpha1.ConnectionStatusSuccessful, AttemptedAt: at(10*time.Second + 300*time.Millisecond)}
	out, err = w.WriteConnectionState(ctx, current, sameSecond, nil)
	require.NoError(t, err)
	assert.Equal(t, v1alpha1.ConnectionStatusSuccessful, out.Status.ConnectionState.Status)

	// A recreated object (same name, new UID) is not the validated one.
	recreated := current.DeepCopy()
	recreated.UID = "another-uid"
	_, err = w.WriteConnectionState(ctx, recreated, v1alpha1.ConnectionState{Status: v1alpha1.ConnectionStatusFailed, AttemptedAt: at(time.Minute)}, nil)
	require.ErrorIs(t, err, ErrStaleConnectionState)
	_, err = w.WriteConnectionState(ctx, nil, ok, nil)
	assert.Error(t, err)
}

// TestTokenEndpointPolicy is the regression test of token-request SSRF with response
// reflection: endpoints named by credentials are restricted (Google and Microsoft Entra
// hosts, https GitHub API URLs, no loopback or link-local destinations), and issuer
// response bodies never reach messages.
func TestTokenEndpointPolicy(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	internal := newRecordingServer(t, http.StatusBadRequest, internalBody)
	rs := &fakeRepoServer{}
	validate := func(t *testing.T, f *fixture, r *v1alpha1.Repository, data map[string]string) (v1alpha1.ConnectionState, error) {
		t.Helper()
		in := map[string][]byte{}
		for k, v := range data {
			in[k] = []byte(v)
		}
		return f.resolver.ValidateWithSecretData(ctx, r, in)
	}
	assertNotReflected := func(t *testing.T, state v1alpha1.ConnectionState, err error) {
		t.Helper()
		require.Error(t, err)
		assert.NotContains(t, state.Message, "INTERNAL-SECRET-DATA")
		assert.NotContains(t, err.Error(), "INTERNAL-SECRET-DATA")
		assert.Less(t, len(state.Message), 1024)
	}

	t.Run("google endpoints are pinned", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, nil, withRepoServer(rs), withTransport(trustTransport(internal.Server)))
		csr := repo("csr", "https://source.developers.google.com/p/x/r/y", withCreds("unsaved"))
		state, err := validate(t, f, csr, map[string]string{"gcpServiceAccountKey": serviceAccountKey(t, internal.URL+"/token", "a@b")})
		require.ErrorIs(t, err, ErrInvalidConfiguration)
		assert.Equal(t, MessageConfigurationError, state.Message)
		assert.Contains(t, err.Error(), `token_uri host "127.0.0.1" is not allowed`)
		impersonated := fmt.Sprintf(`{"type":"impersonated_service_account","service_account_impersonation_url":%q,"source_credentials":{"type":"authorized_user","client_id":"c","client_secret":"s","refresh_token":"r"}}`, internal.URL+"/x")
		_, err = validate(t, f, csr, map[string]string{"gcpServiceAccountKey": impersonated})
		require.ErrorIs(t, err, ErrInvalidConfiguration)
		assert.Contains(t, err.Error(), "service_account_impersonation_url")
		plain := serviceAccountKey(t, "http://oauth2.googleapis.com/token", "a@b")
		_, err = validate(t, f, csr, map[string]string{"gcpServiceAccountKey": plain})
		require.ErrorIs(t, err, ErrInvalidConfiguration)
		assert.Contains(t, err.Error(), "must use https")
		assert.Zero(t, internal.requests.Load())
	})

	t.Run("google error bodies are not reflected", func(t *testing.T) {
		t.Parallel()
		endpoint := newRecordingServer(t, http.StatusBadRequest, internalBody)
		f := newFixture(t, nil, withRepoServer(rs), withTransport(trustTransport(endpoint.Server)), withLoopbackIssuers())
		state, err := validate(t, f, repo("csr", "https://source.developers.google.com/p/x/r/y", withCreds("unsaved")),
			map[string]string{"gcpServiceAccountKey": serviceAccountKey(t, endpoint.URL+"/token", "a@b")})
		assertNotReflected(t, state, err)
		assert.ErrorIs(t, err, ErrTokenUnavailable)
		assert.Contains(t, state.Message, `the token endpoint returned HTTP 400 (error "invalid_grant")`)
		assert.Equal(t, int32(1), endpoint.requests.Load())
	})

	t.Run("azure authority hosts are pinned and bodies not reflected", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, nil, withRepoServer(rs), withTransport(trustTransport(internal.Server)))
		sp := func(endpoint string) *v1alpha1.Repository {
			return repo("ado", "https://dev.azure.com/o/p/_git/r", withCreds("unsaved"), func(r *v1alpha1.Repository) {
				r.Spec.AzureServicePrincipal = &v1alpha1.AzureServicePrincipal{ClientID: "c", TenantID: "t", ActiveDirectoryEndpoint: endpoint}
			})
		}
		data := map[string]string{"azureServicePrincipalClientSecret": "sp-secret-value"}
		for _, ep := range []string{internal.URL, "http://login.microsoftonline.com", "https://169.254.169.254"} {
			state, err := validate(t, f, sp(ep), data)
			require.ErrorIs(t, err, ErrInvalidConfiguration, ep)
			assert.Equal(t, MessageConfigurationError, state.Message, ep)
		}
		assert.Zero(t, internal.requests.Load())

		entra := newRecordingServer(t, http.StatusUnauthorized, internalBody)
		g := newFixture(t, nil, withRepoServer(rs), withTransport(trustTransport(entra.Server)), withLoopbackIssuers())
		state, err := validate(t, g, sp(entra.URL), data)
		assertNotReflected(t, state, err)
		assert.ErrorIs(t, err, ErrTokenUnavailable)
		assert.NotContains(t, state.Message, "sp-secret-value")
		assert.Positive(t, entra.requests.Load())
	})

	t.Run("github API URLs must be https and bodies are not reflected", func(t *testing.T) {
		t.Parallel()
		_, keyPEM := rsaKey(t)
		clock := newFakeClock(time.Now())
		gh := newFakeGitHub(t, clock, 61, nil)
		gh.failTokens = true
		gh.failBody = `{"message":"` + strings.Repeat("INTERNAL-SECRET-DATA ", 100) + `"}`
		f := newFixture(t, nil, withRepoServer(rs), withTransport(trustTransport(gh.Server)), withClock(clock))
		app := func(base string) *v1alpha1.Repository {
			return repo("app", "https://github.com/org/app", withCreds("unsaved"), func(r *v1alpha1.Repository) { r.Spec.GitHubApp = githubApp(61, 1, base) })
		}
		key := map[string]string{"githubAppPrivateKey": keyPEM}
		state, err := validate(t, f, app(strings.Replace(gh.apiURL(), "https://", "http://", 1)), key)
		require.ErrorIs(t, err, ErrInvalidConfiguration)
		assert.Equal(t, MessageConfigurationError, state.Message)
		assert.Zero(t, gh.requestCount())

		state, err = validate(t, f, app(gh.apiURL()), key)
		assertNotReflected(t, state, err)
		assert.ErrorIs(t, err, ErrTokenUnavailable)
		assert.Contains(t, state.Message, "(HTTP 403)")
		assert.Positive(t, gh.requestCount())
	})

	t.Run("loopback and link-local destinations are refused", func(t *testing.T) {
		t.Parallel()
		_, keyPEM := rsaKey(t)
		clock := newFakeClock(time.Now())
		gh := newFakeGitHub(t, clock, 62, nil)
		f := newFixture(t, nil, withRepoServer(rs), withTransport(trustTransport(gh.Server)), withClock(clock), withStrictEgress())
		r := repo("app", "https://github.com/org/app", withCreds("unsaved"), func(r *v1alpha1.Repository) { r.Spec.GitHubApp = githubApp(62, 1, gh.apiURL()) })
		state, err := validate(t, f, r, map[string]string{"githubAppPrivateKey": keyPEM})
		require.ErrorIs(t, err, ErrInvalidConfiguration)
		assert.Equal(t, MessageConfigurationError, state.Message)
		assert.Contains(t, err.Error(), "connection to 127.0.0.1 refused")
		assert.Zero(t, gh.requestCount())
	})
}

// TestEgressGuard covers the address rules of token requests.
func TestEgressGuard(t *testing.T) {
	t.Parallel()
	strict := &egressGuard{denied: []netip.Prefix{netip.MustParsePrefix("10.96.0.0/12")}}
	loose := &egressGuard{allowLoopback: true}
	for addr, want := range map[string][2]bool{
		"169.254.169.254":        {false, false},
		"::ffff:169.254.169.254": {false, false},
		"fe80::1":                {false, false},
		"0.0.0.0":                {false, false},
		"::":                     {false, false},
		"224.0.0.1":              {false, false},
		"127.0.0.1":              {false, true},
		"::1":                    {false, true},
		"10.96.0.10":             {false, true},
		"10.0.0.5":               {true, true},
		"140.82.112.3":           {true, true},
	} {
		ip := netip.MustParseAddr(addr)
		assert.Equal(t, want[0], strict.allowed(ip), "strict %s", addr)
		assert.Equal(t, want[1], loose.allowed(ip), "loose %s", addr)
	}
	require.Error(t, strict.control("tcp", "169.254.169.254:80", nil))
	require.ErrorIs(t, strict.control("tcp", "not-an-ip:80", nil), errEndpointNotAllowed)
	require.NoError(t, strict.control("tcp", "10.0.0.5:443", nil))

	_, err := NewResolver(&Lister{}, Options{DeniedTokenNetworks: []netip.Prefix{{}}})
	assert.Error(t, err)
	_, err = NewResolver(&Lister{}, Options{AzureDevOpsHosts: []string{"https://dev.azure.com"}})
	assert.Error(t, err)
}

// TestHostPatterns covers the host allowlists.
func TestHostPatterns(t *testing.T) {
	t.Parallel()
	p, err := compileHostPatterns("x", []string{"*.AzureCR.io", "dev.azure.com.", "127.0.0.1", "::1"})
	require.NoError(t, err)
	for host, want := range map[string]bool{
		"myacr.azurecr.io":              true,
		"MyACR.azurecr.io.":             true,
		"azurecr.io":                    false,
		"myacr.azurecr.io.attacker.com": false,
		"evilazurecr.io":                false,
		"dev.azure.com":                 true,
		"org.dev.azure.com":             false,
		"127.0.0.1":                     true,
		"::1":                           true,
		"":                              false,
	} {
		assert.Equal(t, want, p.match(host), host)
	}
	for _, bad := range []string{"", "*", "*.", "a/b", "host:443", "*.*.x", "user@host", ".x", "a..b"} {
		_, err := compileHostPatterns("x", []string{bad})
		assert.Error(t, err, bad)
	}
	empty, err := compileHostPatterns("x", []string{})
	require.NoError(t, err)
	assert.NotNil(t, empty)
	assert.False(t, empty.match("anything"))
}

// TestIssuerError covers the removal of server text from token errors.
func TestIssuerError(t *testing.T) {
	t.Parallel()
	body := []byte(`{"error":"invalid_grant","error_description":"INTERNAL-SECRET-DATA"}`)
	re := &oauth2.RetrieveError{Response: &http.Response{StatusCode: http.StatusBadRequest}, Body: body}
	got := issuerError(fmt.Errorf("oauth2: %w", re))
	assert.Equal(t, `the token endpoint returned HTTP 400 (error "invalid_grant")`, got.Error())
	var target *oauth2.RetrieveError
	assert.ErrorAs(t, got, &target, "the original stays reachable for errors.As")

	weird := &oauth2.RetrieveError{Response: &http.Response{StatusCode: 500}, Body: []byte(`{"error":"<script>alert(1)</script>"}`)}
	assert.Equal(t, "the token endpoint returned HTTP 500", issuerError(weird).Error())

	ghErr := &scm.Error{Kind: scm.KindForbidden, Provider: "github", Op: "CreateInstallationToken", StatusCode: 403, Err: errors.New("POST https://x: 403 INTERNAL-SECRET-DATA")}
	assert.Equal(t, "github: CreateInstallationToken: Forbidden (HTTP 403)", issuerError(ghErr).Error())
	notFound := &scm.Error{Kind: scm.KindNotFound, Provider: "github", Op: "FindInstallation", StatusCode: 404, Err: errors.New("installation not found for org: x")}
	assert.Equal(t, notFound.Error(), issuerError(notFound).Error())
	network := &scm.Error{Kind: scm.KindUnavailable, Provider: "github", Op: "CreateInstallationToken", Err: errors.New("dial tcp: connection refused")}
	assert.Equal(t, network.Error(), issuerError(network).Error())

	long := errors.New(strings.Repeat("y", 4*maxIssuerMessage))
	assert.Len(t, issuerError(long).Error(), maxIssuerMessage+3)
	assert.NoError(t, issuerError(nil))

	cfg := tokenFailure("mint", fmt.Errorf("dial: %w", errEndpointNotAllowed))
	assert.ErrorIs(t, cfg, ErrInvalidConfiguration)
	assert.NotErrorIs(t, cfg, ErrTokenUnavailable)
	unavailable := tokenFailure("mint", re)
	assert.ErrorIs(t, unavailable, ErrTokenUnavailable)
	assert.NotContains(t, unavailable.Error(), "INTERNAL-SECRET-DATA")
}

// TestAzureWorkloadIdentityHostPolicy is the regression test of the process identity
// leaking to arbitrary hosts: the ACR exchange runs only for allowed registry hosts (also
// when a plain-prefix template matches a look-alike host) and the Azure DevOps token is sent
// only to https Azure DevOps hosts.
func TestAzureWorkloadIdentityHostPolicy(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(time.Now())
	wi := &fakeWorkloadIdentity{clock: clock, ttl: time.Hour}
	useWI := func(r *v1alpha1.Repository) { r.Spec.UseAzureWorkloadIdentity = true }
	f := newFixture(t, []runtime.Object{
		repo("evil-registry", "registry.attacker.example/charts", withType(v1alpha1.RepositoryTypeHelm), useWI),
		cred("acr-tpl", "myacr.azurecr.io", credWithType(v1alpha1.RepositoryTypeHelm), func(c *v1alpha1.RepositoryCredential) {
			c.Spec.UseAzureWorkloadIdentity = true
		}),
		repo("evil-git", "https://git.attacker.example/org/x", useWI),
		repo("http-ado", "http://dev.azure.com/org/p/_git/r", useWI),
		repo("ssh-ado", "git@ssh.dev.azure.com:v3/org/p/r", useWI),
		repo("vs", "https://org.visualstudio.com/p/_git/r", useWI),
		repo("ado", "https://dev.azure.com/org/p/_git/r", useWI),
	}, withClock(clock), withAzureWI(wi))
	ctx := t.Context()
	rejected := []Query{
		{URL: "registry.attacker.example/charts", Type: v1alpha1.RepositoryTypeHelm},
		{URL: "myacr.azurecr.io.attacker.com/charts", Type: v1alpha1.RepositoryTypeHelm},
		{URL: "https://git.attacker.example/org/x"},
		{URL: "http://dev.azure.com/org/p/_git/r"},
		{URL: "git@ssh.dev.azure.com:v3/org/p/r"},
	}
	for _, q := range rejected {
		_, err := f.resolver.Resolve(ctx, q)
		require.Error(t, err, q.URL)
		assert.ErrorIs(t, err, ErrInvalidConfiguration, q.URL)
	}
	assert.Empty(t, wi.calls(), "the workload identity is never asked for a token for these hosts")

	res, err := f.resolver.Resolve(ctx, Query{URL: "myacr.azurecr.io.attacker.com/charts", Type: v1alpha1.RepositoryTypeHelm})
	require.Error(t, err)
	assert.Nil(t, res)
	assert.Contains(t, err.Error(), `not "myacr.azurecr.io.attacker.com"`)

	for _, url := range []string{"https://org.visualstudio.com/p/_git/r", "https://dev.azure.com/org/p/_git/r"} {
		res, err := f.resolver.Resolve(ctx, Query{URL: url})
		require.NoError(t, err, url)
		assert.Equal(t, MethodAzureWorkloadIdentity, res.Method, url)
	}

	// An empty list disables workload identity for git.
	g := newFixture(t, []runtime.Object{repo("ado", "https://dev.azure.com/org/p/_git/r", useWI)},
		withAzureWI(wi), withOptions(func(o *Options) { o.AzureDevOpsHosts = []string{} }))
	_, err = g.resolver.GetRepository(ctx, "https://dev.azure.com/org/p/_git/r", "")
	assert.ErrorIs(t, err, ErrInvalidConfiguration)
}
