package repositories_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// TestInheritedTemplateTransport: a caller allowed to create repositories in its project
// cannot make the server send an organization-wide template's credentials through a proxy
// it chose or with TLS/host key verification off (validate, create, update), while own
// credentials, URLs outside every template, the template's own settings and callers
// allowed to update the template are unaffected.
func TestInheritedTemplateTransport(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	ctx := t.Context()
	admin := e.client("admin")
	teamA := e.client("team-a")
	e.createProject("team-a")
	const (
		prefix   = "https://git.example.com/org/"
		private  = "https://git.example.com/org/private"
		attacker = "http://attacker.example:3128"
		orgPass  = "org-template-pass"
	)
	_, err := admin.CreateRepositoryCredential(ctx, &apiclient.RepositoryCredentialCreateRequest{Body: apiclient.RepositoryCredentialWriteRequest{
		Spec: v1alpha1.RepositoryCredentialSpec{URLPrefix: prefix, RepositoryAccess: v1alpha1.RepositoryAccess{Proxy: "http://egress.example.com:3128"}}, Credentials: basicCreds("org-bot", orgPass),
	}})
	require.NoError(t, err)
	sentWithOverride := func() bool {
		e.repo.mu.Lock()
		defer e.repo.mu.Unlock()
		for _, r := range e.repo.tested {
			if r.Password == orgPass && ((r.Proxy != "" && r.Proxy != "http://egress.example.com:3128") || r.Insecure || r.InsecureIgnoreHostKey || r.InsecureOCIForceHTTP) {
				return true
			}
		}
		return false
	}

	for _, tc := range []struct {
		name   string
		mutate func(*v1alpha1.RepositorySpec)
		detail string
	}{
		{"proxy", func(s *v1alpha1.RepositorySpec) { s.Proxy = attacker }, "own spec.proxy;"},
		{"insecure", func(s *v1alpha1.RepositorySpec) { s.Insecure = true }, "own spec.insecure;"},
		{"insecureIgnoreHostKey", func(s *v1alpha1.RepositorySpec) { s.InsecureIgnoreHostKey = true }, "own spec.insecureIgnoreHostKey;"}, //nolint:staticcheck // deprecated upstream but honored by the library
		{"insecureOCIForceHTTP", func(s *v1alpha1.RepositorySpec) { s.InsecureOCIForceHTTP = true }, "own spec.insecureOCIForceHTTP;"},
		{"proxy and insecure", func(s *v1alpha1.RepositorySpec) { s.Proxy, s.Insecure = attacker, true }, "own spec.proxy, spec.insecure;"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := repoBody("", "team-a", nil)
			tc.mutate(&body.Spec)
			_, err := teamA.ValidateRepositoryAccess(ctx, &apiclient.RepositoryValidateRequest{Repo: private, Body: body})
			pd := requireStatus(t, err, http.StatusForbidden, apiclient.CodePermissionDenied)
			assert.Contains(t, pd.Detail, tc.detail)
			assert.NotContains(t, pd.Detail, prefix, "the template is not disclosed")
			body.Spec.URL = private
			_, err = teamA.CreateRepository(ctx, &apiclient.RepositoryCreateRequest{Body: body})
			requireStatus(t, err, http.StatusForbidden, apiclient.CodePermissionDenied)
		})
	}
	assert.False(t, sentWithOverride(), "template credentials never left with the caller's transport")
	assert.Empty(t, e.repositories())
	audited := false
	for _, a := range e.audit.byOperation("validateRepositoryAccess") {
		audited = audited || (a.Action == "update" && a.Object == prefix && a.Status == http.StatusForbidden)
	}
	assert.True(t, audited, "the denial is audited with the template's update check")

	// The template's own proxy may be named; own credentials make the transport the
	// caller's business; URLs outside every template are not affected.
	same := repoBody("", "team-a", nil)
	same.Spec.Proxy = "http://egress.example.com:3128"
	_, err = teamA.ValidateRepositoryAccess(ctx, &apiclient.RepositoryValidateRequest{Repo: private, Body: same})
	require.NoError(t, err)
	own := repoBody("", "team-a", basicCreds("me", "team-a-pass"))
	own.Spec.Proxy, own.Spec.Insecure = "http://proxy.team-a.example:3128", true
	_, err = teamA.ValidateRepositoryAccess(ctx, &apiclient.RepositoryValidateRequest{Repo: private, Body: own})
	require.NoError(t, err)
	assert.Equal(t, "team-a-pass", e.repo.lastRepo("TestRepository").Password)
	userOnly := repoBody("", "team-a", &apiclient.RepositoryCredentials{Username: "me"})
	userOnly.Spec.Insecure = true
	_, err = teamA.ValidateRepositoryAccess(ctx, &apiclient.RepositoryValidateRequest{Repo: private, Body: userOnly})
	require.NoError(t, err, "a username is a credential of its own (no template is inherited)")
	assert.Empty(t, e.repo.lastRepo("TestRepository").Password)
	outside := repoBody("", "team-a", nil)
	outside.Spec.Insecure = true
	_, err = teamA.ValidateRepositoryAccess(ctx, &apiclient.RepositoryValidateRequest{Repo: "https://git.other.example/x", Body: outside})
	require.NoError(t, err)

	// A repository that inherits without overrides is accepted; adding a proxy later is not.
	created, err := teamA.CreateRepository(ctx, &apiclient.RepositoryCreateRequest{Body: repoBody(private, "team-a", nil)})
	require.NoError(t, err)
	assert.True(t, created.InheritedCredentials)
	upd := apiclient.RepositoryWriteRequest{Spec: created.Spec}
	upd.Spec.Proxy = attacker
	_, err = teamA.UpdateRepository(ctx, &apiclient.RepositoryUpdateRequest{Repo: private, AppProject: "team-a", Body: upd})
	requireStatus(t, err, http.StatusForbidden, apiclient.CodePermissionDenied)
	stored := &v1alpha1.Repository{}
	require.NoError(t, e.kc.Get(ctx, client.ObjectKey{Namespace: e.ns, Name: created.Metadata.Name}, stored))
	assert.Empty(t, stored.Spec.Proxy)

	// Own credentials with a proxy are kept on updates without credentials, but removing
	// them while keeping the proxy (inheritance would start) is refused.
	const ownURL = "https://git.example.com/org/own"
	body := repoBody(ownURL, "team-a", basicCreds("me", "own-pass"))
	body.Spec.Proxy = "http://proxy.team-a.example:3128"
	withOwn, err := teamA.CreateRepository(ctx, &apiclient.RepositoryCreateRequest{Body: body})
	require.NoError(t, err)
	keep := apiclient.RepositoryWriteRequest{Spec: withOwn.Spec}
	keep.Spec.Depth = 1
	_, err = teamA.UpdateRepository(ctx, &apiclient.RepositoryUpdateRequest{Repo: ownURL, AppProject: "team-a", Body: keep})
	require.NoError(t, err)
	drop := apiclient.RepositoryWriteRequest{Spec: withOwn.Spec, Credentials: &apiclient.RepositoryCredentials{}}
	_, err = teamA.UpdateRepository(ctx, &apiclient.RepositoryUpdateRequest{Repo: ownURL, AppProject: "team-a", Body: drop})
	requireStatus(t, err, http.StatusForbidden, apiclient.CodePermissionDenied)
	require.NotNil(t, e.secret(withOwn.Spec.CredentialsRef.Name), "credentials kept")
	assert.False(t, sentWithOverride())

	// A caller allowed to update the template may override its transport (it could change
	// the template itself).
	adminBody := repoBody("", "", nil)
	adminBody.Spec.Proxy, adminBody.Spec.Insecure = "http://proxy.example.com:3128", true
	_, err = admin.ValidateRepositoryAccess(ctx, &apiclient.RepositoryValidateRequest{Repo: private, Body: adminBody})
	require.NoError(t, err)
}
