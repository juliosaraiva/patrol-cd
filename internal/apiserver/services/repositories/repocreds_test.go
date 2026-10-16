package repositories_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

func credBody(prefix string, creds *apiclient.RepositoryCredentials) apiclient.RepositoryCredentialWriteRequest {
	return apiclient.RepositoryCredentialWriteRequest{Spec: v1alpha1.RepositoryCredentialSpec{URLPrefix: prefix}, Credentials: creds}
}

// TestRepoCreds covers REPO-045/API-036: create, list (user name only), identical
// create, differing create, upsert, update, exact-prefix delete, URL-encoded paths, the
// repository URL quirk, RBAC per URL prefix.
func TestRepoCreds(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	ctx := t.Context()
	admin := e.client("admin")
	const prefix = "https://git.example.com/org"

	_, err := admin.CreateRepositoryCredential(ctx, &apiclient.RepositoryCredentialCreateRequest{Body: credBody("", nil)})
	ae := requireStatus(t, err, http.StatusBadRequest, apiclient.CodeInvalidArgument)
	assert.Equal(t, "must specify URL", ae.Detail)

	c, err := admin.CreateRepositoryCredential(ctx, &apiclient.RepositoryCredentialCreateRequest{Body: credBody(prefix, basicCreds("tpl-user", "tpl-secret"))})
	require.NoError(t, err)
	requireNoSecret(t, c, "tpl-secret")
	assert.True(t, c.HasCredentials)
	assert.Equal(t, "tpl-user", c.Username)
	sec := e.secret(c.Spec.CredentialsRef.Name)
	require.NotNil(t, sec)
	assert.Equal(t, v1alpha1.LabelValueSecretTypeRepoCreds, sec.Labels[v1alpha1.LabelKeySecretType])
	assert.Equal(t, "RepositoryCredential", sec.OwnerReferences[0].Kind)

	list, err := admin.ListRepositoryCredentials(ctx)
	require.NoError(t, err)
	require.Len(t, list.Items, 1)
	requireNoSecret(t, list, "tpl-secret")

	// Identical: idempotent; different: refused unless upsert.
	_, err = admin.CreateRepositoryCredential(ctx, &apiclient.RepositoryCredentialCreateRequest{Body: credBody(prefix, basicCreds("tpl-user", "tpl-secret"))})
	require.NoError(t, err)
	_, err = admin.CreateRepositoryCredential(ctx, &apiclient.RepositoryCredentialCreateRequest{Body: credBody(prefix, basicCreds("tpl-user", "other"))})
	ae = requireStatus(t, err, http.StatusBadRequest, apiclient.CodeInvalidArgument)
	assert.Equal(t, `existing repository credentials spec is different; use upsert flag to force update; difference in keys "credentials"`, ae.Detail)
	_, err = admin.CreateRepositoryCredential(ctx, &apiclient.RepositoryCredentialCreateRequest{Upsert: true, Body: credBody(prefix, basicCreds("tpl-user", "other"))})
	require.NoError(t, err)
	assert.Equal(t, "other", string(e.secret(c.Spec.CredentialsRef.Name).Data["password"]))

	// Update through the URL-encoded path.
	upd := credBody(prefix, nil)
	upd.Spec.Proxy = "http://proxy.example.com:3128"
	u, err := admin.UpdateRepositoryCredential(ctx, &apiclient.RepositoryCredentialUpdateRequest{URL: prefix, Body: upd})
	require.NoError(t, err)
	assert.Equal(t, "http://proxy.example.com:3128", u.Spec.Proxy)
	assert.Equal(t, "other", string(e.secret(c.Spec.CredentialsRef.Name).Data["password"]), "credentials kept")
	_, err = admin.UpdateRepositoryCredential(ctx, &apiclient.RepositoryCredentialUpdateRequest{URL: "https://git.example.com/nope", Body: credBody("https://git.example.com/nope", nil)})
	ae = requireStatus(t, err, http.StatusNotFound, apiclient.CodeNotFound)
	assert.Equal(t, "repository credentials 'https://git.example.com/nope' not found", ae.Detail)

	// RBAC on the URL prefix: team-a has no grant on this prefix.
	lt, err := e.client("team-a").ListRepositoryCredentials(ctx)
	require.NoError(t, err)
	assert.Empty(t, lt.Items)
	err = e.client("team-a").DeleteRepositoryCredential(ctx, prefix)
	requireStatus(t, err, http.StatusForbidden, apiclient.CodePermissionDenied)
	_, err = e.client("reader").CreateRepositoryCredential(ctx, &apiclient.RepositoryCredentialCreateRequest{Body: credBody("https://other.example.com/", nil)})
	requireStatus(t, err, http.StatusForbidden, apiclient.CodePermissionDenied)

	// Delete addresses the exact prefix (a longer URL does not delete the shorter
	// template, unlike upstream's longest-prefix lookup).
	err = admin.DeleteRepositoryCredential(ctx, prefix+"/team")
	requireStatus(t, err, http.StatusNotFound, apiclient.CodeNotFound)
	require.NoError(t, admin.DeleteRepositoryCredential(ctx, prefix))
	list, err = admin.ListRepositoryCredentials(ctx)
	require.NoError(t, err)
	assert.Empty(t, list.Items)
	assert.Nil(t, e.secret(c.Spec.CredentialsRef.Name), "managed Secret deleted")

	// Upstream quirk: a read template whose URL is an unscoped repository's is refused.
	// (A URL outside the deleted template's prefix: the informers may still hold it.)
	_, err = admin.CreateRepository(ctx, &apiclient.RepositoryCreateRequest{Body: repoBody("https://other.example.com/app", "", nil)})
	require.NoError(t, err)
	_, err = admin.CreateRepositoryCredential(ctx, &apiclient.RepositoryCredentialCreateRequest{Body: credBody("https://other.example.com/app", nil)})
	ae = requireStatus(t, err, http.StatusConflict, apiclient.CodeAlreadyExists)
	assert.Equal(t, `repository credentials "https://other.example.com/app" already exists`, ae.Detail)
}

// TestWriteRepoCreds covers REPO-046: write templates use write-repositories RBAC, are
// separate from read templates and are not gated by the hydrator flag.
func TestWriteRepoCreds(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	ctx := t.Context()
	admin := e.client("admin")
	const prefix = "https://git.example.com/deploy/"
	_, err := e.client("repo-only").CreateWriteRepositoryCredential(ctx, &apiclient.RepositoryCredentialCreateRequest{Body: credBody(prefix, basicCreds("u", "push"))})
	requireStatus(t, err, http.StatusForbidden, apiclient.CodePermissionDenied)
	w, err := admin.CreateWriteRepositoryCredential(ctx, &apiclient.RepositoryCredentialCreateRequest{Body: credBody(prefix, basicCreds("u", "push-secret"))})
	require.NoError(t, err)
	assert.Equal(t, v1alpha1.RepositoryPurposeWrite, w.Spec.Purpose)
	assert.Equal(t, v1alpha1.LabelValueSecretTypeRepoCredsWrite, e.secret(w.Spec.CredentialsRef.Name).Labels[v1alpha1.LabelKeySecretType])
	_, err = admin.CreateWriteRepositoryCredential(ctx, &apiclient.RepositoryCredentialCreateRequest{Body: credBody(prefix, basicCreds("u", "different"))})
	requireStatus(t, err, http.StatusBadRequest, apiclient.CodeInvalidArgument)
	reads, err := admin.ListRepositoryCredentials(ctx)
	require.NoError(t, err)
	assert.Empty(t, reads.Items)
	writes, err := admin.ListWriteRepositoryCredentials(ctx)
	require.NoError(t, err)
	require.Len(t, writes.Items, 1)
	requireNoSecret(t, writes, "push-secret")
	ro, err := e.client("repo-only").ListWriteRepositoryCredentials(ctx)
	require.NoError(t, err)
	assert.Empty(t, ro.Items)
	_, err = admin.UpdateWriteRepositoryCredential(ctx, &apiclient.RepositoryCredentialUpdateRequest{URL: prefix, Body: credBody(prefix, &apiclient.RepositoryCredentials{})})
	require.NoError(t, err)
	got, err := admin.ListWriteRepositoryCredentials(ctx)
	require.NoError(t, err)
	assert.False(t, got.Items[0].HasCredentials, "empty credentials remove the stored ones")
	assert.Nil(t, e.secret(w.Spec.CredentialsRef.Name))
	require.NoError(t, admin.DeleteWriteRepositoryCredential(ctx, prefix))
}
