package repositories_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/repositories"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// faultyClient injects failures into the handlers' writes: conflicts on Repository and
// RepositoryCredential updates (a concurrent writer won), errors on Secret writes.
type faultyClient struct {
	client.Client
	conflictUpdates atomic.Bool
	failSecrets     atomic.Bool
}

var errInjected = errors.New("injected Secret write failure")

func (f *faultyClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	switch obj.(type) {
	case *v1alpha1.Repository, *v1alpha1.RepositoryCredential:
		if f.conflictUpdates.Load() {
			return apierrors.NewConflict(schema.GroupResource{Group: v1alpha1.GroupName, Resource: "repositories"}, obj.GetName(), errors.New("the object has been modified"))
		}
	case *corev1.Secret:
		if f.failSecrets.Load() {
			return errInjected
		}
	}
	return f.Client.Update(ctx, obj, opts...)
}

func (f *faultyClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if _, ok := obj.(*corev1.Secret); ok && f.failSecrets.Load() {
		return errInjected
	}
	return f.Client.Create(ctx, obj, opts...)
}

func (f *faultyClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if _, ok := obj.(*corev1.Secret); ok && f.failSecrets.Load() {
		return errInjected
	}
	return f.Client.Delete(ctx, obj, opts...)
}

// TestCredentialsFollowObjectUpdate covers REPO-038 optimistic concurrency for
// credentials: an update refused by the API server (409) changes no stored credential, for
// repositories, upserts and templates alike, and a Secret that cannot be written after the
// object update puts the previous spec back.
func TestCredentialsFollowObjectUpdate(t *testing.T) {
	t.Parallel()
	var fc *faultyClient
	e := newEnv(t, envOptions{wrap: func(c client.Client) repositories.KubeClient {
		fc = &faultyClient{Client: c}
		return fc
	}})
	ctx := t.Context()
	admin := e.client("admin")
	stored := func(name string) *v1alpha1.Repository {
		t.Helper()
		r := &v1alpha1.Repository{}
		require.NoError(t, e.kc.Get(ctx, client.ObjectKey{Namespace: e.ns, Name: name}, r))
		return r
	}
	password := func(secret string) string {
		t.Helper()
		s := e.secret(secret)
		if s == nil {
			return "<no Secret>"
		}
		return string(s.Data[v1alpha1.RepositorySecretKeyPassword])
	}

	const repoURL = "https://git.example.com/org/consistent"
	created, err := admin.CreateRepository(ctx, &apiclient.RepositoryCreateRequest{Body: repoBody(repoURL, "", basicCreds("bot", "old-pass"))})
	require.NoError(t, err)
	secret := created.Spec.CredentialsRef.Name
	const bareURL = "https://git.example.com/org/bare"
	bare, err := admin.CreateRepository(ctx, &apiclient.RepositoryCreateRequest{Body: repoBody(bareURL, "", nil)})
	require.NoError(t, err)
	require.Nil(t, bare.Spec.CredentialsRef)
	tpl, err := admin.CreateRepositoryCredential(ctx, &apiclient.RepositoryCredentialCreateRequest{Body: apiclient.RepositoryCredentialWriteRequest{
		Spec: v1alpha1.RepositoryCredentialSpec{URLPrefix: "https://git.example.com/tpl/"}, Credentials: basicCreds("t", "tpl-old"),
	}})
	require.NoError(t, err)
	tplSecret := tpl.Spec.CredentialsRef.Name

	t.Run("refused update keeps credentials", func(t *testing.T) {
		fc.conflictUpdates.Store(true)
		defer fc.conflictUpdates.Store(false)
		for _, tc := range []struct {
			name  string
			creds *apiclient.RepositoryCredentials
		}{
			{"rotation", basicCreds("bot", "new-pass")},
			{"removal", &apiclient.RepositoryCredentials{}},
		} {
			upd := apiclient.RepositoryWriteRequest{Spec: created.Spec, Credentials: tc.creds}
			upd.Metadata.ResourceVersion = stored(created.Metadata.Name).ResourceVersion
			_, err := admin.UpdateRepository(ctx, &apiclient.RepositoryUpdateRequest{Repo: repoURL, Body: upd})
			requireStatus(t, err, http.StatusConflict, apiclient.CodeConflict)
			assert.Equal(t, "old-pass", password(secret), tc.name)
			assert.Equal(t, secret, stored(created.Metadata.Name).Spec.CredentialsRef.Name, tc.name)
		}
		_, err := admin.CreateRepository(ctx, &apiclient.RepositoryCreateRequest{Upsert: true, Body: repoBody(repoURL, "", basicCreds("bot", "upsert-pass"))})
		requireStatus(t, err, http.StatusConflict, apiclient.CodeConflict)
		assert.Equal(t, "old-pass", password(secret), "upsert")

		// A repository without credentials gets no Secret from a refused update.
		upd := apiclient.RepositoryWriteRequest{Spec: bare.Spec, Credentials: basicCreds("u", "never-stored")}
		_, err = admin.UpdateRepository(ctx, &apiclient.RepositoryUpdateRequest{Repo: bareURL, Body: upd})
		requireStatus(t, err, http.StatusConflict, apiclient.CodeConflict)
		assert.Nil(t, e.secret(bare.Metadata.Name+"-creds"))
		assert.Nil(t, stored(bare.Metadata.Name).Spec.CredentialsRef)

		// Templates follow the same order.
		tu := apiclient.RepositoryCredentialWriteRequest{Spec: tpl.Spec, Credentials: basicCreds("t", "tpl-new")}
		_, err = admin.UpdateRepositoryCredential(ctx, &apiclient.RepositoryCredentialUpdateRequest{URL: tpl.Spec.URLPrefix, Body: tu})
		requireStatus(t, err, http.StatusConflict, apiclient.CodeConflict)
		assert.Equal(t, "tpl-old", password(tplSecret))
		tu.Credentials = &apiclient.RepositoryCredentials{}
		_, err = admin.UpdateRepositoryCredential(ctx, &apiclient.RepositoryCredentialUpdateRequest{URL: tpl.Spec.URLPrefix, Body: tu})
		requireStatus(t, err, http.StatusConflict, apiclient.CodeConflict)
		assert.Equal(t, "tpl-old", password(tplSecret))
	})

	t.Run("Secret failure restores the spec", func(t *testing.T) {
		fc.failSecrets.Store(true)
		defer fc.failSecrets.Store(false)
		upd := apiclient.RepositoryWriteRequest{Spec: bare.Spec, Credentials: basicCreds("u", "p")}
		upd.Spec.Depth = 3
		_, err := admin.UpdateRepository(ctx, &apiclient.RepositoryUpdateRequest{Repo: bareURL, Body: upd})
		requireStatus(t, err, http.StatusInternalServerError, "")
		r := stored(bare.Metadata.Name)
		assert.Nil(t, r.Spec.CredentialsRef, "no reference to a Secret that was not written")
		assert.Zero(t, r.Spec.Depth, "previous spec restored")

		rot := apiclient.RepositoryWriteRequest{Spec: created.Spec, Credentials: basicCreds("bot", "lost-pass")}
		rot.Spec.Depth = 7
		_, err = admin.UpdateRepository(ctx, &apiclient.RepositoryUpdateRequest{Repo: repoURL, Body: rot})
		requireStatus(t, err, http.StatusInternalServerError, "")
		assert.Equal(t, "old-pass", password(secret))
		assert.Zero(t, stored(created.Metadata.Name).Spec.Depth)

		tu := apiclient.RepositoryCredentialWriteRequest{Spec: tpl.Spec, Credentials: &apiclient.RepositoryCredentials{}}
		_, err = admin.UpdateRepositoryCredential(ctx, &apiclient.RepositoryCredentialUpdateRequest{URL: tpl.Spec.URLPrefix, Body: tu})
		requireStatus(t, err, http.StatusInternalServerError, "")
		got := &v1alpha1.RepositoryCredential{}
		require.NoError(t, e.kc.Get(ctx, client.ObjectKey{Namespace: e.ns, Name: tpl.Metadata.Name}, got))
		require.NotNil(t, got.Spec.CredentialsRef, "the template still references its Secret")
		assert.Equal(t, "tpl-old", password(tplSecret))
	})

	t.Run("successful updates", func(t *testing.T) {
		upd := apiclient.RepositoryWriteRequest{Spec: created.Spec, Credentials: basicCreds("bot", "new-pass")}
		upd.Metadata.ResourceVersion = stored(created.Metadata.Name).ResourceVersion
		_, err := admin.UpdateRepository(ctx, &apiclient.RepositoryUpdateRequest{Repo: repoURL, Body: upd})
		require.NoError(t, err)
		assert.Equal(t, "new-pass", password(secret))

		upd = apiclient.RepositoryWriteRequest{Spec: bare.Spec, Credentials: basicCreds("u", "first-pass")}
		got, err := admin.UpdateRepository(ctx, &apiclient.RepositoryUpdateRequest{Repo: bareURL, Body: upd})
		require.NoError(t, err)
		require.NotNil(t, got.Spec.CredentialsRef)
		assert.Equal(t, "first-pass", password(got.Spec.CredentialsRef.Name))

		// Removing every credential deletes the managed Secret and clears the reference.
		upd = apiclient.RepositoryWriteRequest{Spec: got.Spec, Credentials: &apiclient.RepositoryCredentials{}}
		got, err = admin.UpdateRepository(ctx, &apiclient.RepositoryUpdateRequest{Repo: bareURL, Body: upd})
		require.NoError(t, err)
		assert.Nil(t, got.Spec.CredentialsRef)
		assert.Nil(t, e.secret(bare.Metadata.Name+"-creds"))
	})
}

// TestDeclarativeURLPasswordsMasked: objects created declaratively (kubectl, GitOps) may
// carry a password in a URL; responses, denials and audit records mask it.
func TestDeclarativeURLPasswordsMasked(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	ctx := t.Context()
	const repoURL = "https://bot:hunter2-secret@git.example.com/org/x"
	require.NoError(t, e.kc.Create(ctx, &v1alpha1.Repository{
		ObjectMeta: metav1.ObjectMeta{Name: "decl", Namespace: e.ns},
		Spec: v1alpha1.RepositorySpec{URL: repoURL, RepositoryAccess: v1alpha1.RepositoryAccess{
			Proxy:     "http://pu:proxy-secret@proxy.example.com:3128",
			GitHubApp: &v1alpha1.RepositoryGitHubApp{AppID: 1, EnterpriseBaseURL: "https://ghe:app-url-secret@ghe.example.com/api/v3"},
		}},
	}))
	require.NoError(t, e.kc.Create(ctx, &v1alpha1.RepositoryCredential{
		ObjectMeta: metav1.ObjectMeta{Name: "decl-tpl", Namespace: e.ns},
		Spec:       v1alpha1.RepositoryCredentialSpec{URLPrefix: "https://bot:tpl-secret@git.example.com/tpl/"},
	}))
	require.NoError(t, e.kc.Create(ctx, &v1alpha1.SCMProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "decl-scm", Namespace: e.ns},
		Spec:       v1alpha1.SCMProviderSpec{Type: v1alpha1.SCMProviderTypeGitHub, APIURL: "https://u:scm-secret@api.github.com", WebURL: "https://u:web-secret@github.com"},
	}))
	secrets := []string{"hunter2-secret", "proxy-secret", "app-url-secret", "tpl-secret", "scm-secret", "web-secret"}

	reader := e.client("reader")
	list, err := reader.ListRepositories(ctx, &apiclient.RepositoryListRequest{})
	require.NoError(t, err)
	require.Len(t, list.Items, 1)
	requireNoSecret(t, list, secrets...)
	assert.Equal(t, "https://bot:******@git.example.com/org/x", list.Items[0].Spec.URL)
	assert.Equal(t, "http://pu:******@proxy.example.com:3128", list.Items[0].Spec.Proxy)
	assert.Equal(t, "https://ghe:******@ghe.example.com/api/v3", list.Items[0].Spec.GitHubApp.EnterpriseBaseURL)
	got, err := reader.GetRepository(ctx, &apiclient.RepositoryQuery{Repo: repoURL})
	require.NoError(t, err)
	requireNoSecret(t, got, secrets...)

	// A denial on the stored object (reader may get, not delete) does not echo it.
	err = reader.DeleteRepository(ctx, &apiclient.RepositoryQuery{Repo: repoURL})
	pd := requireStatus(t, err, http.StatusForbidden, apiclient.CodePermissionDenied)
	assert.NotContains(t, pd.Detail, "hunter2-secret")
	assert.Contains(t, pd.Detail, "https://bot:******@git.example.com/org/x")

	admin := e.client("admin")
	creds, err := admin.ListRepositoryCredentials(ctx)
	require.NoError(t, err)
	require.Len(t, creds.Items, 1)
	assert.Equal(t, "https://bot:******@git.example.com/tpl/", creds.Items[0].Spec.URLPrefix)
	requireNoSecret(t, creds, secrets...)
	providers, err := admin.ListSCMProviders(ctx)
	require.NoError(t, err)
	require.Len(t, providers.Items, 1)
	requireNoSecret(t, providers, secrets...)
	one, err := admin.GetSCMProvider(ctx, "decl-scm")
	require.NoError(t, err)
	assert.Equal(t, "https://u:******@api.github.com", one.Spec.APIURL)
	requireNoSecret(t, one, secrets...)

	e.audit.mu.Lock()
	entries := slices.Clone(e.audit.entries)
	e.audit.mu.Unlock()
	requireNoSecret(t, entries, secrets...)
}
