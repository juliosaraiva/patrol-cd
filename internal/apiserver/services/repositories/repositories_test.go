package repositories_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// TestRepositoryLifecycle covers REPO-035..039/043 and API-037 through the Go client: the
// managed credentials Secret, sanitized responses, idempotent create, upsert, optimistic
// concurrency, forced connection refresh and deletion with the Secret.
func TestRepositoryLifecycle(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	ctx := t.Context()
	admin := e.client("admin")
	const repoURL = "https://git.example.com/org/app.git"

	created, err := admin.CreateRepository(ctx, &apiclient.RepositoryCreateRequest{Body: repoBody(repoURL, "", basicCreds("bot", "s3cret-pass"))})
	require.NoError(t, err)
	requireNoSecret(t, created, "s3cret-pass")
	assert.Equal(t, repoURL, created.Spec.URL)
	assert.Equal(t, v1alpha1.RepositoryTypeGit, created.Spec.Type)
	assert.Equal(t, v1alpha1.RepositoryPurposeRead, created.Spec.Purpose)
	assert.True(t, created.HasCredentials)
	assert.Equal(t, v1alpha1.ConnectionStatusSuccessful, created.ConnectionState.Status)
	require.NotNil(t, created.Spec.CredentialsRef)
	assert.Equal(t, "Successful", string(created.Status.ConnectionState.Status), "status recorded")
	assert.Equal(t, "s3cret-pass", e.repo.lastRepo("TestRepository").Password, "tested with the request credentials before saving")

	// The Secret is managed: labelled, annotated, owned by the Repository.
	sec := e.secret(created.Spec.CredentialsRef.Name)
	require.NotNil(t, sec)
	assert.Equal(t, v1alpha1.LabelValueSecretTypeRepository, sec.Labels[v1alpha1.LabelKeySecretType])
	assert.Equal(t, v1alpha1.AnnotationValueManagedByPatrol, sec.Annotations[v1alpha1.AnnotationKeyManagedBy])
	require.Len(t, sec.OwnerReferences, 1)
	assert.Equal(t, created.Metadata.UID, sec.OwnerReferences[0].UID)
	assert.Equal(t, "Repository", sec.OwnerReferences[0].Kind)
	assert.Equal(t, map[string][]byte{"username": []byte("bot"), "password": []byte("s3cret-pass")}, sec.Data)

	// List and get never return secrets; get accepts another spelling of the same URL.
	list, err := admin.ListRepositories(ctx, &apiclient.RepositoryListRequest{})
	require.NoError(t, err)
	require.Len(t, list.Items, 1)
	requireNoSecret(t, list, "s3cret-pass")
	got, err := admin.GetRepository(ctx, &apiclient.RepositoryQuery{Repo: "https://GIT.example.com/org/app"})
	require.NoError(t, err)
	assert.Equal(t, created.Metadata.Name, got.Metadata.Name)

	// API-009: the URL is URL-encoded in the path (UI encodeURIComponent form).
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.url+"/api/v1/repositories/"+url.QueryEscape(repoURL), nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+admin.Token())
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	assert.Contains(t, string(body), `"url":"https://git.example.com/org/app.git"`)
	bad, err := http.NewRequestWithContext(ctx, http.MethodGet, e.url, nil)
	require.NoError(t, err)
	bad.URL.Opaque = "/api/v1/repositories/https%zz"
	bad.Header.Set("Authorization", "Bearer "+admin.Token())
	resp, err = http.DefaultClient.Do(bad)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.GreaterOrEqual(t, resp.StatusCode, 400, "malformed percent-encoding is refused")

	// Identical create: idempotent. Different create: refused unless upsert.
	_, err = admin.CreateRepository(ctx, &apiclient.RepositoryCreateRequest{Body: repoBody(repoURL, "", basicCreds("bot", "s3cret-pass"))})
	require.NoError(t, err)
	diff := repoBody(repoURL, "", basicCreds("bot", "s3cret-pass"))
	diff.Spec.Proxy = "http://proxy.example.com:3128"
	ae := requireStatus(t, func() error {
		_, err := admin.CreateRepository(ctx, &apiclient.RepositoryCreateRequest{Body: diff})
		return err
	}(), http.StatusBadRequest, apiclient.CodeInvalidArgument)
	assert.Equal(t, `existing repository spec is different; use upsert flag to force update; difference in keys "proxy"`, ae.Detail)
	upserted, err := admin.CreateRepository(ctx, &apiclient.RepositoryCreateRequest{Upsert: true, Body: diff})
	require.NoError(t, err)
	assert.Equal(t, "http://proxy.example.com:3128", upserted.Spec.Proxy)
	assert.Len(t, e.repositories(), 1, "upsert updates in place")

	// A key the API does not manage survives credential updates.
	sec = e.secret(created.Spec.CredentialsRef.Name)
	sec.Data["custom"] = []byte("keep-me")
	require.NoError(t, e.kc.Update(ctx, sec))

	// Optimistic concurrency: a stale resourceVersion is a 409.
	cur, err := admin.GetRepository(ctx, &apiclient.RepositoryQuery{Repo: repoURL})
	require.NoError(t, err)
	upd := apiclient.RepositoryWriteRequest{Spec: cur.Spec}
	upd.Spec.Depth = 5
	upd.Metadata.ResourceVersion = "1"
	requireStatus(t, func() error {
		_, err := admin.UpdateRepository(ctx, &apiclient.RepositoryUpdateRequest{Repo: repoURL, Body: upd})
		return err
	}(), http.StatusConflict, apiclient.CodeConflict)

	// Update without credentials keeps them; with credentials replaces the managed keys.
	upd.Metadata.ResourceVersion = cur.Metadata.ResourceVersion
	updated, err := admin.UpdateRepository(ctx, &apiclient.RepositoryUpdateRequest{Repo: repoURL, Body: upd})
	require.NoError(t, err)
	assert.Equal(t, int64(5), updated.Spec.Depth)
	assert.Equal(t, "s3cret-pass", string(e.secret(created.Spec.CredentialsRef.Name).Data["password"]))
	upd = apiclient.RepositoryWriteRequest{Spec: updated.Spec, Credentials: &apiclient.RepositoryCredentials{Password: "rotated-pass"}}
	_, err = admin.UpdateRepository(ctx, &apiclient.RepositoryUpdateRequest{Repo: repoURL, Body: upd})
	require.NoError(t, err)
	sec = e.secret(created.Spec.CredentialsRef.Name)
	assert.Equal(t, map[string][]byte{"password": []byte("rotated-pass"), "custom": []byte("keep-me")}, sec.Data)

	// The server manages credentialsRef.
	hijack := repoBody(repoURL, "", nil)
	hijack.Spec.CredentialsRef = &v1alpha1.LocalSecretReference{Name: "someone-else"}
	requireStatus(t, func() error {
		_, err := admin.UpdateRepository(ctx, &apiclient.RepositoryUpdateRequest{Repo: repoURL, Body: hijack})
		return err
	}(), http.StatusBadRequest, apiclient.CodeInvalidArgument)

	// forceRefresh re-tests and records; a broken credential flips the state (REPO-043).
	e.waitResolved(repoURL, "", "rotated-pass")
	sec.Data["password"] = []byte("wrong")
	require.NoError(t, e.kc.Update(ctx, sec))
	e.waitResolved(repoURL, "", "wrong")
	before := e.repo.testCount()
	stale, err := admin.GetRepository(ctx, &apiclient.RepositoryQuery{Repo: repoURL})
	require.NoError(t, err)
	assert.Equal(t, v1alpha1.ConnectionStatusSuccessful, stale.ConnectionState.Status, "recorded state is served until refreshed")
	assert.Equal(t, before, e.repo.testCount())
	refreshed, err := admin.GetRepository(ctx, &apiclient.RepositoryQuery{Repo: repoURL, ForceRefresh: true})
	require.NoError(t, err)
	assert.Equal(t, v1alpha1.ConnectionStatusFailed, refreshed.ConnectionState.Status)
	assert.Equal(t, "Unable to connect to repository: authentication required", refreshed.ConnectionState.Message)
	stored := &v1alpha1.Repository{}
	require.NoError(t, e.kc.Get(ctx, client.ObjectKey{Namespace: e.ns, Name: created.Metadata.Name}, stored))
	require.NotNil(t, stored.Status.ConnectionState)
	assert.Equal(t, v1alpha1.ConnectionStatusFailed, stored.Status.ConnectionState.Status, "recorded in Repository.status")
	listed, err := admin.ListRepositories(ctx, &apiclient.RepositoryListRequest{ForceRefresh: true})
	require.NoError(t, err)
	assert.Equal(t, v1alpha1.ConnectionStatusFailed, listed.Items[0].ConnectionState.Status)

	// Delete removes the Repository and its managed Secret; afterwards the repository is
	// indistinguishable from one that never existed.
	require.NoError(t, admin.DeleteRepository(ctx, &apiclient.RepositoryQuery{Repo: repoURL}))
	assert.Empty(t, e.repositories())
	assert.Nil(t, e.secret(created.Spec.CredentialsRef.Name))
	ae = requireStatus(t, func() error {
		_, err := admin.GetRepository(ctx, &apiclient.RepositoryQuery{Repo: repoURL})
		return err
	}(), http.StatusForbidden, apiclient.CodePermissionDenied)
	assert.Equal(t, "permission denied", ae.Detail)

	// Audit records name the Repository targets, never the credentials.
	entries := e.audit.byOperation("createRepository")
	require.NotEmpty(t, entries)
	assert.Equal(t, "Repository", entries[0].Target.Kind)
	requireNoSecret(t, e.audit.entries, "s3cret-pass", "rotated-pass")
}

// TestCreateUnreachable covers REPO-037: an unscoped repository that fails the connection
// test is not saved and the error does not echo its password; a project-scoped one is
// created and its failure recorded.
func TestCreateUnreachable(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	ctx := t.Context()
	admin := e.client("admin")
	ae := requireStatus(t, func() error {
		_, err := admin.CreateRepository(ctx, &apiclient.RepositoryCreateRequest{Body: repoBody("https://unreachable.example.com/x.git", "", basicCreds("u", "leaky-pass"))})
		return err
	}(), http.StatusBadRequest, apiclient.CodeInvalidArgument)
	assert.Contains(t, ae.Detail, "error testing repository connectivity: Unable to connect to repository: ")
	assert.NotContains(t, ae.Detail, "leaky-pass")
	assert.Empty(t, e.repositories())
	var secrets corev1.SecretList
	require.NoError(t, e.kc.List(ctx, &secrets, client.InNamespace(e.ns)))
	for _, s := range secrets.Items {
		assert.NotContains(t, s.Name, "creds", "no credentials Secret is left behind")
	}

	requireStatus(t, func() error {
		_, err := admin.CreateRepository(ctx, &apiclient.RepositoryCreateRequest{Body: repoBody("https://unreachable.example.com/x.git", "missing", nil)})
		return err
	}(), http.StatusBadRequest, apiclient.CodeInvalidArgument)
	e.createProject("team-a")
	scoped, err := admin.CreateRepository(ctx, &apiclient.RepositoryCreateRequest{Body: repoBody("https://unreachable.example.com/x.git", "team-a", basicCreds("u", "leaky-pass"))})
	require.NoError(t, err)
	assert.Equal(t, v1alpha1.ConnectionStatusFailed, scoped.ConnectionState.Status)
	assert.NotContains(t, scoped.ConnectionState.Message, "leaky-pass")
	assert.Contains(t, scoped.ConnectionState.Message, "******")
}

// TestInputValidation covers the input checks (URL scheme allow-list, embedded
// passwords, credential material, unknown fields).
func TestInputValidation(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	ctx := t.Context()
	admin := e.client("admin")
	cases := []struct {
		name   string
		body   apiclient.RepositoryWriteRequest
		detail string
	}{
		{"file scheme", repoBody("file:///etc/passwd", "", nil), "unsupported URL scheme"},
		{"local path", repoBody("/srv/git/repo", "", nil), "not a supported repository URL"},
		{"ext transport", repoBody("ext::sh -c id", "", nil), "whitespace"},
		{"ext transport no space", repoBody("ext::id", "", nil), "not a supported repository URL"},
		{"option injection", repoBody("--upload-pack=touch:x", "", nil), "not a supported repository URL"},
		{"password in URL", repoBody("https://u:p@git.example.com/x", "", nil), "must not contain a password"},
		{"bad proxy", func() apiclient.RepositoryWriteRequest {
			b := repoBody("https://git.example.com/x", "", nil)
			b.Spec.Proxy = "http://user:pw@proxy:3128"
			return b
		}(), "spec.proxy must not contain user information"},
		{"half TLS pair", repoBody("https://git.example.com/x", "", &apiclient.RepositoryCredentials{TLSClientCertData: "x"}), "must be set together"},
		{"bad ssh key", repoBody("git@git.example.com:org/x", "", &apiclient.RepositoryCredentials{SSHPrivateKey: "not a key"}), "not a valid private key"},
		{"github key without app", repoBody("https://git.example.com/x", "", &apiclient.RepositoryCredentials{GitHubAppPrivateKey: "x"}), "requires spec.githubApp"},
		{"gcp external account", repoBody("https://source.developers.google.com/p/x", "", &apiclient.RepositoryCredentials{GCPServiceAccountKey: `{"type":"external_account"}`}), "service account key"},
		{"write purpose on read route", func() apiclient.RepositoryWriteRequest {
			b := repoBody("https://git.example.com/x", "", nil)
			b.Spec.Purpose = v1alpha1.RepositoryPurposeWrite
			return b
		}(), "spec.purpose must be"},
		{"lfs on helm", func() apiclient.RepositoryWriteRequest {
			b := repoBody("https://charts.example.com", "", nil)
			b.Spec.Type, b.Spec.EnableLFS = v1alpha1.RepositoryTypeHelm, true
			return b
		}(), "enableLFS"},
		{"wrong kind", func() apiclient.RepositoryWriteRequest {
			b := repoBody("https://git.example.com/x", "", nil)
			b.Kind = "Secret"
			return b
		}(), "kind must be Repository"},
		{"enterprise URL over http", func() apiclient.RepositoryWriteRequest {
			b := repoBody("https://git.example.com/x", "", nil)
			b.Spec.GitHubApp = &v1alpha1.RepositoryGitHubApp{AppID: 1, EnterpriseBaseURL: "http://ghe.example.com/api/v3"}
			return b
		}(), "must be an https URL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ae := requireStatus(t, func() error {
				_, err := admin.CreateRepository(ctx, &apiclient.RepositoryCreateRequest{Body: tc.body})
				return err
			}(), http.StatusBadRequest, apiclient.CodeInvalidArgument)
			assert.Contains(t, ae.Detail, tc.detail)
		})
	}
	assert.Empty(t, e.repositories())

	// Unknown body fields are refused (no smuggled fields).
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url+"/api/v1/repositories", strings.NewReader(`{"spec":{"url":"https://git.example.com/x"},"password":"p"}`))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+admin.Token())
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// TestProjectScopedRepositories covers REPO-004/036/038/039 and the tenant isolation of
// the API: per-project visibility, 403 without existence oracle, appProject selection,
// moving between projects, project token confinement.
func TestProjectScopedRepositories(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	ctx := t.Context()
	e.createProject("team-a")
	e.createProject("team-b")
	admin, teamA, teamB := e.client("admin"), e.client("team-a"), e.client("team-b")
	const repoURL = "https://git.example.com/shared/app"

	// team-a may create in its project only.
	_, err := teamA.CreateRepository(ctx, &apiclient.RepositoryCreateRequest{Body: repoBody(repoURL, "team-b", nil)})
	requireStatus(t, err, http.StatusForbidden, apiclient.CodePermissionDenied)
	_, err = teamA.CreateRepository(ctx, &apiclient.RepositoryCreateRequest{Body: repoBody(repoURL, "", nil)})
	requireStatus(t, err, http.StatusForbidden, apiclient.CodePermissionDenied)
	a, err := teamA.CreateRepository(ctx, &apiclient.RepositoryCreateRequest{Body: repoBody(repoURL, "team-a", basicCreds("a", "team-a-pass"))})
	require.NoError(t, err)
	assert.Equal(t, "team-a", a.Spec.Project)
	b, err := admin.CreateRepository(ctx, &apiclient.RepositoryCreateRequest{Body: repoBody(repoURL, "team-b", nil)})
	require.NoError(t, err)
	assert.NotEqual(t, a.Metadata.Name, b.Metadata.Name, "same URL in two projects: distinct objects (REPO-028)")

	// Lists are filtered per item.
	la, err := teamA.ListRepositories(ctx, &apiclient.RepositoryListRequest{})
	require.NoError(t, err)
	require.Len(t, la.Items, 1)
	assert.Equal(t, "team-a", la.Items[0].Spec.Project)
	ln, err := e.client("nobody").ListRepositories(ctx, &apiclient.RepositoryListRequest{})
	require.NoError(t, err)
	assert.Empty(t, ln.Items, "no permission: empty list, not 403")
	all, err := admin.ListRepositories(ctx, &apiclient.RepositoryListRequest{})
	require.NoError(t, err)
	require.Len(t, all.Items, 2)
	assert.Equal(t, "team-a", all.Items[0].Spec.Project, "sorted by project/url")
	only, err := admin.ListRepositories(ctx, &apiclient.RepositoryListRequest{AppProject: "team-b"})
	require.NoError(t, err)
	require.Len(t, only.Items, 1)

	// Ambiguous URL without appProject; appProject selects.
	_, err = admin.GetRepository(ctx, &apiclient.RepositoryQuery{Repo: repoURL})
	ae := requireStatus(t, err, http.StatusNotFound, apiclient.CodeNotFound)
	assert.Equal(t, `repository not found for url "https://git.example.com/shared/app" and project ""`, ae.Detail)
	gb, err := admin.GetRepository(ctx, &apiclient.RepositoryQuery{Repo: repoURL, AppProject: "team-b"})
	require.NoError(t, err)
	assert.Equal(t, b.Metadata.Name, gb.Metadata.Name)
	ga, err := teamA.GetRepository(ctx, &apiclient.RepositoryQuery{Repo: repoURL})
	require.NoError(t, err, "one visible repository: no project needed")
	assert.Equal(t, a.Metadata.Name, ga.Metadata.Name)

	// No existence oracle: a forbidden repository and a missing one answer the same.
	const onlyA = "https://git.example.com/only-a/app"
	_, err = admin.CreateRepository(ctx, &apiclient.RepositoryCreateRequest{Body: repoBody(onlyA, "team-a", nil)})
	require.NoError(t, err)
	_, errForbidden := teamB.GetRepository(ctx, &apiclient.RepositoryQuery{Repo: onlyA, AppProject: "team-a"})
	_, errMissing := teamB.GetRepository(ctx, &apiclient.RepositoryQuery{Repo: "https://git.example.com/nothing", AppProject: "team-a"})
	f, m := requireStatus(t, errForbidden, http.StatusForbidden, apiclient.CodePermissionDenied), requireStatus(t, errMissing, http.StatusForbidden, apiclient.CodePermissionDenied)
	assert.Equal(t, f.Detail, m.Detail)
	assert.Equal(t, "permission denied", f.Detail)
	_, err = teamB.UpdateRepository(ctx, &apiclient.RepositoryUpdateRequest{Repo: onlyA, AppProject: "team-a", Body: repoBody(onlyA, "team-a", nil)})
	requireStatus(t, err, http.StatusForbidden, apiclient.CodePermissionDenied)
	err = teamB.DeleteRepository(ctx, &apiclient.RepositoryQuery{Repo: onlyA, AppProject: "team-a"})
	requireStatus(t, err, http.StatusForbidden, apiclient.CodePermissionDenied)
	require.Len(t, e.repositories(), 3)
	require.NoError(t, admin.DeleteRepository(ctx, &apiclient.RepositoryQuery{Repo: onlyA}))

	// Project token: confined to its project, read only.
	ci := e.projectClient("team-a", "ci")
	lc, err := ci.ListRepositories(ctx, &apiclient.RepositoryListRequest{})
	require.NoError(t, err)
	require.Len(t, lc.Items, 1)
	assert.Equal(t, "team-a", lc.Items[0].Spec.Project)
	_, err = ci.CreateRepository(ctx, &apiclient.RepositoryCreateRequest{Body: repoBody("https://git.example.com/other", "team-a", nil)})
	requireStatus(t, err, http.StatusForbidden, apiclient.CodePermissionDenied)
	// team-b's repository of the same URL is not visible: only team-a's is a candidate.
	_, err = ci.GetRepository(ctx, &apiclient.RepositoryQuery{Repo: repoURL, AppProject: "team-b"})
	requireStatus(t, err, http.StatusNotFound, apiclient.CodeNotFound)
	_, err = ci.GetRepository(ctx, &apiclient.RepositoryQuery{Repo: onlyA, AppProject: "team-a"})
	requireStatus(t, err, http.StatusForbidden, apiclient.CodePermissionDenied)

	// The scoped repository's Secret is bound to it, so the library resolves it for
	// team-a only.
	e.waitResolved(repoURL, "team-a", "team-a-pass")

	// Moving a repository needs update in both projects (REPO-038), and the target project
	// must exist.
	move := repoBody(repoURL, "team-c", nil)
	_, err = admin.UpdateRepository(ctx, &apiclient.RepositoryUpdateRequest{Repo: repoURL, AppProject: "team-a", Body: move})
	requireStatus(t, err, http.StatusBadRequest, apiclient.CodeInvalidArgument)
	move = repoBody(repoURL, "team-b", nil)
	_, err = teamA.UpdateRepository(ctx, &apiclient.RepositoryUpdateRequest{Repo: repoURL, AppProject: "team-a", Body: move})
	requireStatus(t, err, http.StatusForbidden, apiclient.CodePermissionDenied)
	_, err = admin.UpdateRepository(ctx, &apiclient.RepositoryUpdateRequest{Repo: repoURL, AppProject: "team-a", Body: move})
	requireStatus(t, err, http.StatusConflict, apiclient.CodeAlreadyExists)
	require.NoError(t, admin.DeleteRepository(ctx, &apiclient.RepositoryQuery{Repo: repoURL, AppProject: "team-b"}))
	moved, err := admin.UpdateRepository(ctx, &apiclient.RepositoryUpdateRequest{Repo: repoURL, AppProject: "team-a", Body: move})
	require.NoError(t, err)
	assert.Equal(t, "team-b", moved.Spec.Project)
	assert.Equal(t, a.Metadata.Name, moved.Metadata.Name)
	_, err = teamA.GetRepository(ctx, &apiclient.RepositoryQuery{Repo: repoURL})
	requireStatus(t, err, http.StatusForbidden, apiclient.CodePermissionDenied)
}

// TestAuthentication covers anonymous and unauthenticated callers.
func TestAuthentication(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	ctx := t.Context()
	_, err := e.client("").ListRepositories(ctx, &apiclient.RepositoryListRequest{})
	requireStatus(t, err, http.StatusUnauthorized, apiclient.CodeUnauthenticated)
	_, err = e.clientWithToken("garbage.token.value").GetRepository(ctx, &apiclient.RepositoryQuery{Repo: "https://x.example.com/r"})
	requireStatus(t, err, http.StatusUnauthorized, apiclient.CodeUnauthenticated)

	anon := newEnv(t, envOptions{anonymous: true})
	_, err = anon.client("admin").CreateRepository(ctx, &apiclient.RepositoryCreateRequest{Body: repoBody("https://git.example.com/pub", "", nil)})
	require.NoError(t, err)
	l, err := anon.client("").ListRepositories(ctx, &apiclient.RepositoryListRequest{})
	require.NoError(t, err)
	assert.Empty(t, l.Items, "anonymous without policy.default sees nothing")
	_, err = anon.client("").CreateRepository(ctx, &apiclient.RepositoryCreateRequest{Body: repoBody("https://git.example.com/other", "", nil)})
	requireStatus(t, err, http.StatusForbidden, apiclient.CodePermissionDenied)

	ro := newEnv(t, envOptions{anonymous: true, defaultRole: "role:readonly"})
	_, err = ro.client("admin").CreateRepository(ctx, &apiclient.RepositoryCreateRequest{Body: repoBody("https://git.example.com/pub", "", nil)})
	require.NoError(t, err)
	l, err = ro.client("").ListRepositories(ctx, &apiclient.RepositoryListRequest{})
	require.NoError(t, err)
	assert.Len(t, l.Items, 1, "policy.default applies to anonymous callers")
	err = ro.client("").DeleteRepository(ctx, &apiclient.RepositoryQuery{Repo: "https://git.example.com/pub"})
	requireStatus(t, err, http.StatusForbidden, apiclient.CodePermissionDenied)
}

// TestValidateAccess covers REPO-040: a connection test without saving, template
// credentials when none are given, RBAC create.
func TestValidateAccess(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	ctx := t.Context()
	admin := e.client("admin")
	const repoURL = "https://git.example.com/org/validate"
	st, err := admin.ValidateRepositoryAccess(ctx, &apiclient.RepositoryValidateRequest{Repo: repoURL, Body: repoBody("", "", basicCreds("u", "good-pass"))})
	require.NoError(t, err)
	assert.Equal(t, v1alpha1.ConnectionStatusSuccessful, st.Status)
	assert.Equal(t, "good-pass", e.repo.lastRepo("TestRepository").Password)
	_, err = admin.ValidateRepositoryAccess(ctx, &apiclient.RepositoryValidateRequest{Repo: repoURL, Body: repoBody("", "", basicCreds("u", "wrong"))})
	ae := requireStatus(t, err, http.StatusBadRequest, apiclient.CodeInvalidArgument)
	assert.Equal(t, "error testing repository connectivity: Unable to connect to repository: authentication required", ae.Detail)
	_, err = admin.ValidateRepositoryAccess(ctx, &apiclient.RepositoryValidateRequest{Repo: repoURL, Body: repoBody("https://other.example.com/x", "", nil)})
	requireStatus(t, err, http.StatusBadRequest, apiclient.CodeInvalidArgument)

	// Nothing was saved.
	assert.Empty(t, e.repositories())
	var secrets corev1.SecretList
	require.NoError(t, e.kc.List(ctx, &secrets, client.InNamespace(e.ns)))
	assert.Empty(t, secrets.Items)

	// Without credentials, a matching template's are used.
	_, err = admin.CreateRepositoryCredential(ctx, &apiclient.RepositoryCredentialCreateRequest{Body: apiclient.RepositoryCredentialWriteRequest{
		Spec: v1alpha1.RepositoryCredentialSpec{URLPrefix: "https://git.example.com/org/"}, Credentials: basicCreds("t", "template-pass"),
	}})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		_, err := admin.ValidateRepositoryAccess(ctx, &apiclient.RepositoryValidateRequest{Repo: repoURL, Body: repoBody("", "", nil)})
		return err == nil && e.repo.lastRepo("TestRepository").Password == "template-pass"
	}, 10*time.Second, 20*time.Millisecond)

	// RBAC create is required.
	_, err = e.client("reader").ValidateRepositoryAccess(ctx, &apiclient.RepositoryValidateRequest{Repo: repoURL, Body: repoBody("", "", nil)})
	requireStatus(t, err, http.StatusForbidden, apiclient.CodePermissionDenied)
}

// TestWriteRepositories covers REPO-041/API-039: hydrator gating, mandatory credentials,
// write-repositories RBAC separate from repositories, separate listings.
func TestWriteRepositories(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	off := newEnv(t, envOptions{})
	admin := off.client("admin")
	const repoURL = "https://git.example.com/org/deploy"
	check501 := func(err error) {
		t.Helper()
		ae := requireStatus(t, err, http.StatusNotImplemented, "")
		assert.Equal(t, "hydrator is disabled", ae.Detail)
	}
	_, err := admin.ListWriteRepositories(ctx, &apiclient.RepositoryListRequest{})
	check501(err)
	_, err = admin.CreateWriteRepository(ctx, &apiclient.RepositoryCreateRequest{Body: repoBody(repoURL, "", basicCreds("u", "p"))})
	check501(err)
	_, err = admin.GetWriteRepository(ctx, &apiclient.RepositoryQuery{Repo: repoURL})
	check501(err)
	_, err = admin.UpdateWriteRepository(ctx, &apiclient.RepositoryUpdateRequest{Repo: repoURL, Body: repoBody(repoURL, "", nil)})
	check501(err)
	check501(admin.DeleteWriteRepository(ctx, &apiclient.RepositoryQuery{Repo: repoURL}))
	_, err = admin.ValidateWriteRepositoryAccess(ctx, &apiclient.RepositoryValidateRequest{Repo: repoURL, Body: repoBody("", "", basicCreds("u", "p"))})
	check501(err)

	e := newEnv(t, envOptions{hydrator: true})
	admin = e.client("admin")
	_, err = admin.CreateWriteRepository(ctx, &apiclient.RepositoryCreateRequest{Body: repoBody(repoURL, "", nil)})
	ae := requireStatus(t, err, http.StatusBadRequest, apiclient.CodeInvalidArgument)
	assert.Equal(t, "missing credentials in request", ae.Detail)
	_, err = e.client("repo-only").CreateWriteRepository(ctx, &apiclient.RepositoryCreateRequest{Body: repoBody(repoURL, "", basicCreds("u", "push-pass"))})
	requireStatus(t, err, http.StatusForbidden, apiclient.CodePermissionDenied)
	w, err := admin.CreateWriteRepository(ctx, &apiclient.RepositoryCreateRequest{Body: repoBody(repoURL, "", basicCreds("u", "push-pass"))})
	require.NoError(t, err)
	assert.Equal(t, v1alpha1.RepositoryPurposeWrite, w.Spec.Purpose)
	assert.Equal(t, v1alpha1.LabelValueSecretTypeRepositoryWrite, e.secret(w.Spec.CredentialsRef.Name).Labels[v1alpha1.LabelKeySecretType])
	reads, err := admin.ListRepositories(ctx, &apiclient.RepositoryListRequest{})
	require.NoError(t, err)
	assert.Empty(t, reads.Items, "write repositories are not read repositories")
	writes, err := admin.ListWriteRepositories(ctx, &apiclient.RepositoryListRequest{})
	require.NoError(t, err)
	require.Len(t, writes.Items, 1)
	writesRO, err := e.client("repo-only").ListWriteRepositories(ctx, &apiclient.RepositoryListRequest{})
	require.NoError(t, err)
	assert.Empty(t, writesRO.Items)
	_, err = e.client("repo-only").GetWriteRepository(ctx, &apiclient.RepositoryQuery{Repo: repoURL})
	requireStatus(t, err, http.StatusForbidden, apiclient.CodePermissionDenied)
	st, err := admin.ValidateWriteRepositoryAccess(ctx, &apiclient.RepositoryValidateRequest{Repo: repoURL, Body: repoBody("", "", basicCreds("u", "push-pass"))})
	require.NoError(t, err)
	assert.Equal(t, v1alpha1.ConnectionStatusSuccessful, st.Status)
	require.NoError(t, admin.DeleteWriteRepository(ctx, &apiclient.RepositoryQuery{Repo: repoURL}))
	assert.Empty(t, e.repositories())
}

// TestSCMProviderReference covers the extra check on spec.scmProviderRef: the provider
// must exist and the caller must be allowed to get it.
func TestSCMProviderReference(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	ctx := t.Context()
	admin := e.client("admin")
	body := repoBody("https://github.com/org/app", "", nil)
	body.Spec.SCMProviderRef = &v1alpha1.SCMProviderReference{Name: "github-org"}
	_, err := admin.CreateRepository(ctx, &apiclient.RepositoryCreateRequest{Body: body})
	ae := requireStatus(t, err, http.StatusBadRequest, apiclient.CodeInvalidArgument)
	assert.Equal(t, `SCM provider "github-org" does not exist`, ae.Detail)
	_, err = admin.CreateSCMProvider(ctx, &apiclient.SCMProviderWriteRequest{
		Metadata: metaName("github-org"), Spec: v1alpha1.SCMProviderSpec{Type: v1alpha1.SCMProviderTypeGitHub},
	})
	require.NoError(t, err)
	// The pre-save test resolves the provider through the informers.
	require.Eventually(t, func() bool { _, err := e.lister.SCMProvider("github-org"); return err == nil }, 10*time.Second, 20*time.Millisecond)
	_, err = e.client("repo-only").CreateRepository(ctx, &apiclient.RepositoryCreateRequest{Body: body})
	requireStatus(t, err, http.StatusForbidden, apiclient.CodePermissionDenied)
	created, err := admin.CreateRepository(ctx, &apiclient.RepositoryCreateRequest{Body: body})
	require.NoError(t, err)
	assert.True(t, created.HasCredentials)
}

func TestRepositoryCredsOnly(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	ctx := t.Context()
	admin := e.client("admin")
	out, err := admin.CreateRepository(ctx, &apiclient.RepositoryCreateRequest{CredsOnly: true, Body: repoBody("https://git.example.com/org/", "", basicCreds("u", "tpl-pass"))})
	require.NoError(t, err)
	assert.Equal(t, "RepositoryCredential", out.Kind)
	assert.Equal(t, "https://git.example.com/org/", out.Spec.URL)
	requireNoSecret(t, out, "tpl-pass")
	assert.Empty(t, e.repositories())
	creds, err := admin.ListRepositoryCredentials(ctx)
	require.NoError(t, err)
	require.Len(t, creds.Items, 1)
	e.createProject("team-a")
	_, err = admin.CreateRepository(ctx, &apiclient.RepositoryCreateRequest{CredsOnly: true, Body: repoBody("https://git.example.com/org2/", "team-a", nil)})
	ae := requireStatus(t, err, http.StatusBadRequest, apiclient.CodeInvalidArgument)
	assert.Contains(t, ae.Detail, "cannot be scoped to a project")
	_, err = e.client("team-a").CreateRepository(ctx, &apiclient.RepositoryCreateRequest{CredsOnly: true, Body: repoBody("https://git.example.com/org3/", "", nil)})
	requireStatus(t, err, http.StatusForbidden, apiclient.CodePermissionDenied)
}

// TestListAndRecovery covers list options (selector, pagination), the inherited
// credentials flag, an SSH repository with a key, a declarative repository whose
// unmanaged Secret is replaced by a managed one, connection state expiry (REPO-043) and a
// repo-server outage.
func TestListAndRecovery(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{ttl: time.Millisecond})
	ctx := t.Context()
	admin := e.client("admin")

	_, err := admin.CreateRepositoryCredential(ctx, &apiclient.RepositoryCredentialCreateRequest{Body: credBody("https://git.example.com/tpl/", basicCreds("t", "tpl-pass"))})
	require.NoError(t, err)
	// The pre-save test of an inheriting repository resolves the template through the
	// informers: wait until they have the template and its Secret.
	e.waitResolved("https://git.example.com/tpl/app", "", "tpl-pass")
	_, err = admin.CreateRepository(ctx, &apiclient.RepositoryCreateRequest{Body: repoBody("https://git.example.com/tpl/app", "", nil)})
	require.NoError(t, err)
	_, err = admin.CreateRepository(ctx, &apiclient.RepositoryCreateRequest{Body: repoBody("https://git.example.com/own/app", "", basicCreds("o", "own-pass"))})
	require.NoError(t, err)
	l, err := admin.ListRepositories(ctx, &apiclient.RepositoryListRequest{})
	require.NoError(t, err)
	require.Len(t, l.Items, 2)
	byURL := map[string]apiclient.RepositoryInfo{}
	for _, it := range l.Items {
		byURL[it.Spec.URL] = it
	}
	assert.True(t, byURL["https://git.example.com/tpl/app"].InheritedCredentials)
	assert.True(t, byURL["https://git.example.com/own/app"].HasCredentials)

	page, err := admin.ListRepositories(ctx, &apiclient.RepositoryListRequest{Limit: 1})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.NotEmpty(t, page.Metadata.Continue)
	next, err := admin.ListRepositories(ctx, &apiclient.RepositoryListRequest{Limit: 1, Continue: page.Metadata.Continue})
	require.NoError(t, err)
	require.Len(t, next.Items, 1)
	assert.NotEqual(t, page.Items[0].Metadata.Name, next.Items[0].Metadata.Name)
	_, err = admin.ListRepositories(ctx, &apiclient.RepositoryListRequest{Selector: "a in (b"})
	requireStatus(t, err, http.StatusBadRequest, apiclient.CodeInvalidArgument)
	_, err = admin.ListRepositories(ctx, &apiclient.RepositoryListRequest{Limit: 100000})
	requireStatus(t, err, http.StatusBadRequest, apiclient.CodeInvalidArgument)

	// With an expired state, reads check again and record (upstream cache expiry).
	before := e.repo.testCount()
	time.Sleep(5 * time.Millisecond)
	_, err = admin.GetRepository(ctx, &apiclient.RepositoryQuery{Repo: "https://git.example.com/own/app"})
	require.NoError(t, err)
	assert.Greater(t, e.repo.testCount(), before)

	// SSH repository with a private key: refs resolved with the key.
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	block, err := ssh.MarshalPrivateKey(priv, "")
	require.NoError(t, err)
	sshKey := string(pem.EncodeToMemory(block))
	const sshURL = "git@git.example.com:org/ssh-app.git"
	_, err = admin.CreateRepository(ctx, &apiclient.RepositoryCreateRequest{Body: repoBody(sshURL, "", &apiclient.RepositoryCredentials{SSHPrivateKey: sshKey})})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		_, err := admin.ListRepositoryRefs(ctx, &apiclient.RepositoryQuery{Repo: sshURL})
		return err == nil && e.repo.lastRepo("ListRefs").SSHPrivateKey == sshKey
	}, 10*time.Second, 20*time.Millisecond)

	// A declarative Repository referencing an unmanaged Secret: credentials sent through
	// the API go to a managed Secret; the declarative one is left untouched.
	decl := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "declarative", Namespace: e.ns, Labels: map[string]string{v1alpha1.LabelKeySecretType: v1alpha1.LabelValueSecretTypeRepository}},
		Data: map[string][]byte{"username": []byte("d"), "password": []byte("decl-pass"), "extra": []byte("x")}}
	require.NoError(t, e.kc.Create(ctx, decl))
	declRepo := &v1alpha1.Repository{ObjectMeta: metav1.ObjectMeta{Name: "declarative", Namespace: e.ns},
		Spec: v1alpha1.RepositorySpec{URL: "https://git.example.com/decl/app", RepositoryAccess: v1alpha1.RepositoryAccess{CredentialsRef: &v1alpha1.LocalSecretReference{Name: "declarative"}}}}
	require.NoError(t, e.kc.Create(ctx, declRepo))
	got, err := admin.GetRepository(ctx, &apiclient.RepositoryQuery{Repo: "https://git.example.com/decl/app"})
	require.NoError(t, err)
	body := apiclient.RepositoryWriteRequest{Spec: got.Spec, Credentials: &apiclient.RepositoryCredentials{Username: "d", Password: "new-pass"}}
	upd, err := admin.UpdateRepository(ctx, &apiclient.RepositoryUpdateRequest{Repo: "https://git.example.com/decl/app", Body: body})
	require.NoError(t, err)
	assert.Equal(t, "declarative-creds", upd.Spec.CredentialsRef.Name)
	assert.Equal(t, map[string][]byte{"username": []byte("d"), "password": []byte("new-pass"), "extra": []byte("x")}, e.secret("declarative-creds").Data)
	assert.Equal(t, "decl-pass", string(e.secret("declarative").Data["password"]), "declarative Secret untouched")
	require.NoError(t, admin.DeleteRepository(ctx, &apiclient.RepositoryQuery{Repo: "https://git.example.com/decl/app"}))
	assert.Nil(t, e.secret("declarative-creds"))
	assert.NotNil(t, e.secret("declarative"), "Secrets not managed by the API are kept")

	// Repo-server outage: 503, retryable.
	_, err = admin.ListRepositoryRefs(ctx, &apiclient.RepositoryQuery{Repo: "https://reposerver-down.example.com/x"})
	requireStatus(t, err, http.StatusServiceUnavailable, apiclient.CodeUnavailable)
}

// TestMoveIntoProject covers moving an unscoped repository into a project (update falls
// back to the unscoped repository, as upstream getRepo does) with update permission on
// both objects.
func TestMoveIntoProject(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	ctx := t.Context()
	e.createProject("team-a")
	admin := e.client("admin")
	const repoURL = "https://git.example.com/move/me"
	_, err := admin.CreateRepository(ctx, &apiclient.RepositoryCreateRequest{Body: repoBody(repoURL, "", basicCreds("u", "move-pass"))})
	require.NoError(t, err)
	_, err = e.client("team-a").UpdateRepository(ctx, &apiclient.RepositoryUpdateRequest{Repo: repoURL, Body: repoBody(repoURL, "team-a", nil)})
	requireStatus(t, err, http.StatusForbidden, apiclient.CodePermissionDenied)
	moved, err := admin.UpdateRepository(ctx, &apiclient.RepositoryUpdateRequest{Repo: repoURL, Body: repoBody(repoURL, "team-a", nil)})
	require.NoError(t, err)
	assert.Equal(t, "team-a", moved.Spec.Project)
	e.waitResolved(repoURL, "team-a", "move-pass")
	got, err := e.client("team-a").GetRepository(ctx, &apiclient.RepositoryQuery{Repo: repoURL})
	require.NoError(t, err)
	assert.Equal(t, moved.Metadata.UID, got.Metadata.UID)
}
