package apiclient_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// TestRepositoryClientRequests checks the HTTP requests of the repository area methods:
// URL-encoded repository URLs as single path segments, query parameters, write variants,
// bodies carrying write-only credentials.
func TestRepositoryClientRequests(t *testing.T) {
	t.Parallel()
	srv, reqs := recorder(t, okJSON(map[string]any{}))
	c, err := apiclient.NewClient(&apiclient.ClientOptions{ServerAddr: srv.URL})
	require.NoError(t, err)
	ctx := context.Background()
	const repo = "https://github.com/org/repo.git"
	const esc = "https:%2F%2Fgithub.com%2Forg%2Frepo.git"

	_, err = c.GetRepository(ctx, &apiclient.RepositoryQuery{Repo: repo, AppProject: "team", ForceRefresh: true})
	require.NoError(t, err)
	_, err = c.GetWriteRepository(ctx, &apiclient.RepositoryQuery{Repo: repo})
	require.NoError(t, err)
	_, err = c.CreateRepository(ctx, &apiclient.RepositoryCreateRequest{Upsert: true, Body: apiclient.RepositoryWriteRequest{
		Spec: v1alpha1.RepositorySpec{URL: repo}, Credentials: &apiclient.RepositoryCredentials{Username: "u", Password: "p"},
	}})
	require.NoError(t, err)
	_, err = c.ListRepositories(ctx, &apiclient.RepositoryListRequest{ForceRefresh: true, Limit: 10})
	require.NoError(t, err)
	require.NoError(t, c.DeleteRepository(ctx, &apiclient.RepositoryQuery{Repo: repo, AppProject: "team"}))
	_, err = c.ListRepositoryApps(ctx, &apiclient.RepositoryAppsRequest{Repo: repo, Revision: "main", AppName: "ns/app", AppProject: "p"})
	require.NoError(t, err)
	_, err = c.GetRepositoryAppDetails(ctx, &apiclient.RepositoryAppDetailsRequest{Repo: repo, Body: apiclient.RepositoryAppDetailsQuery{
		Source: &v1alpha1.ApplicationSource{RepoURL: repo, Path: "x"}, AppName: "app", AppProject: "p", VersionID: 3,
	}})
	require.NoError(t, err)
	_, err = c.ValidateWriteRepositoryAccess(ctx, &apiclient.RepositoryValidateRequest{Repo: repo})
	require.NoError(t, err)
	require.NoError(t, c.DeleteWriteRepositoryCredential(ctx, "https://github.com/org/"))
	_, err = c.DeleteCertificates(ctx, &apiclient.RepositoryCertificateQuery{HostNamePattern: "[git.example.com]:2222", CertType: "ssh"})
	require.NoError(t, err)
	require.NoError(t, c.DeleteGPGKey(ctx, "0123456789ABCDEF"))
	_, err = c.UpdateSCMProvider(ctx, "gh", &apiclient.SCMProviderWriteRequest{Spec: v1alpha1.SCMProviderSpec{Type: "github"}})
	require.NoError(t, err)

	r := reqs()
	require.Len(t, r, 12)
	want := []struct{ method, rawPath, query string }{
		{http.MethodGet, "/api/v1/repositories/" + esc, "appProject=team&forceRefresh=true"},
		{http.MethodGet, "/api/v1/write-repositories/" + esc, ""},
		{http.MethodPost, "/api/v1/repositories", "upsert=true"},
		{http.MethodGet, "/api/v1/repositories", "forceRefresh=true&limit=10"},
		{http.MethodDelete, "/api/v1/repositories/" + esc, "appProject=team"},
		{http.MethodGet, "/api/v1/repositories/" + esc + "/apps", "appName=ns%2Fapp&appProject=p&revision=main"},
		{http.MethodPost, "/api/v1/repositories/" + esc + "/appdetails", ""},
		{http.MethodPost, "/api/v1/write-repositories/" + esc + "/validate", ""},
		{http.MethodDelete, "/api/v1/write-repocreds/https:%2F%2Fgithub.com%2Forg%2F", ""},
		{http.MethodDelete, "/api/v1/certificates", "certType=ssh&hostNamePattern=%5Bgit.example.com%5D%3A2222"},
		{http.MethodDelete, "/api/v1/gpgkeys/0123456789ABCDEF", ""},
		{http.MethodPut, "/api/v1/scmproviders/gh", ""},
	}
	for i, w := range want {
		assert.Equal(t, w.method, r[i].Method, "request %d", i)
		assert.Equal(t, w.rawPath, r[i].RawPath, "request %d", i)
		assert.Equal(t, w.query, r[i].Query, "request %d", i)
	}
	assert.Equal(t, repo, r[0].Path[len("/api/v1/repositories/"):], "decoded path value is the URL")

	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(r[2].Body), &body))
	assert.Equal(t, map[string]any{"url": repo}, body["spec"])
	assert.Equal(t, map[string]any{"username": "u", "password": "p"}, body["credentials"])
	require.NoError(t, json.Unmarshal([]byte(r[6].Body), &body))
	assert.Equal(t, float64(3), body["versionId"])
	assert.Equal(t, "p", body["appProject"])
	assert.Equal(t, "", r[4].Body, "DELETE has no body")
}
