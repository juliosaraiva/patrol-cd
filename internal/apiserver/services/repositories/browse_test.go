package repositories_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// TestBrowse covers REPO-042/API-038: refs, OCI tags and Helm charts with credentials
// resolved server-side from the stored repository, RBAC repositories get, and repo-server
// errors without secrets.
func TestBrowse(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	ctx := t.Context()
	admin := e.client("admin")
	e.createProject("team-a", "*")
	const repoURL = "https://git.example.com/org/browse"
	_, err := admin.CreateRepository(ctx, &apiclient.RepositoryCreateRequest{Body: repoBody(repoURL, "team-a", basicCreds("u", "browse-pass"))})
	require.NoError(t, err)
	e.waitResolved(repoURL, "team-a", "browse-pass")

	refs, err := e.client("team-a").ListRepositoryRefs(ctx, &apiclient.RepositoryQuery{Repo: repoURL, AppProject: "team-a"})
	require.NoError(t, err)
	assert.Equal(t, []string{"main", "dev"}, refs.Branches)
	assert.Equal(t, []string{"v1.0.0"}, refs.Tags)
	assert.Equal(t, "browse-pass", e.repo.lastRepo("ListRefs").Password, "credentials resolved server-side")

	// Other tenants: denied, whether or not they name the project.
	_, err = e.client("team-b").ListRepositoryRefs(ctx, &apiclient.RepositoryQuery{Repo: repoURL, AppProject: "team-a"})
	requireStatus(t, err, http.StatusForbidden, apiclient.CodePermissionDenied)
	// Without appProject the scoped repository is not used: the URL is an unregistered
	// one (no credentials), which team-b may not read either.
	_, err = e.client("team-b").ListRepositoryRefs(ctx, &apiclient.RepositoryQuery{Repo: repoURL})
	requireStatus(t, err, http.StatusForbidden, apiclient.CodePermissionDenied)
	_, err = e.client("nobody").ListRepositoryRefs(ctx, &apiclient.RepositoryQuery{Repo: repoURL, AppProject: "team-a"})
	requireStatus(t, err, http.StatusForbidden, apiclient.CodePermissionDenied)
	_, err = e.projectClient("team-a", "ci").ListRepositoryRefs(ctx, &apiclient.RepositoryQuery{Repo: repoURL, AppProject: "team-a"})
	require.NoError(t, err)

	tags, err := admin.ListRepositoryOCITags(ctx, &apiclient.RepositoryQuery{Repo: "oci://registry.example.com/charts/web"})
	require.NoError(t, err)
	assert.Equal(t, []string{"1.0.0", "1.1.0"}, tags.Tags)
	assert.Equal(t, v1alpha1.RepositoryTypeOCI, e.repo.lastRepo("ListOCITags").Type)
	charts, err := admin.ListRepositoryHelmCharts(ctx, &apiclient.RepositoryQuery{Repo: "https://charts.example.com"})
	require.NoError(t, err)
	require.Len(t, charts.Items, 1)
	assert.Equal(t, "nginx", charts.Items[0].Name)
	assert.Equal(t, v1alpha1.RepositoryTypeHelm, e.repo.lastRepo("GetHelmCharts").Type)

	// Unsupported URLs are refused before anything is resolved.
	_, err = admin.ListRepositoryRefs(ctx, &apiclient.RepositoryQuery{Repo: "file:///etc"})
	requireStatus(t, err, http.StatusBadRequest, apiclient.CodeInvalidArgument)

	// Repo-server errors are returned without the credentials.
	const bad = "https://unreachable.example.com/org/x"
	_, err = admin.CreateRepository(ctx, &apiclient.RepositoryCreateRequest{Body: repoBody(bad, "team-a", basicCreds("u", "hidden-pass"))})
	require.NoError(t, err)
	e.waitResolved(bad, "team-a", "hidden-pass")
	_, err = admin.ListRepositoryRefs(ctx, &apiclient.RepositoryQuery{Repo: bad, AppProject: "team-a"})
	ae := requireStatus(t, err, http.StatusBadRequest, apiclient.CodeInvalidArgument)
	assert.NotContains(t, ae.Detail, "hidden-pass")
	assert.Contains(t, ae.Detail, "******")
}

// TestListApps covers REPO-042: discovery needs applications create or update on the
// target Application and a repository permitted by the project.
func TestListApps(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	ctx := t.Context()
	e.createProject("team-a", "https://git.example.com/allowed/*")
	const repoURL = "https://git.example.com/allowed/apps"
	// team-a reads repositories of its project only: register the repository there.
	_, err := e.client("admin").CreateRepository(ctx, &apiclient.RepositoryCreateRequest{Body: repoBody(repoURL, "team-a", nil)})
	require.NoError(t, err)
	req := &apiclient.RepositoryAppsRequest{Repo: repoURL, AppProject: "team-a", AppName: "web", Revision: "main"}
	apps, err := e.client("team-a").ListRepositoryApps(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, []apiclient.RepositoryApp{{Type: "Helm", Path: "apps/api"}, {Type: "Kustomize", Path: "apps/web"}}, apps.Items, "sorted by path")

	// repositories get without applications create/update: denied.
	_, err = e.client("reader").ListRepositoryApps(ctx, req)
	ae := requireStatus(t, err, http.StatusForbidden, apiclient.CodePermissionDenied)
	assert.Equal(t, "permission denied", ae.Detail)
	_, err = e.client("team-b").ListRepositoryApps(ctx, req)
	requireStatus(t, err, http.StatusForbidden, apiclient.CodePermissionDenied)

	// Not permitted by sourceRepos; a missing project answers the same.
	_, err = e.client("admin").ListRepositoryApps(ctx, &apiclient.RepositoryAppsRequest{Repo: "https://git.example.com/other/apps", AppProject: "team-a", AppName: "web"})
	ae = requireStatus(t, err, http.StatusForbidden, apiclient.CodePermissionDenied)
	assert.Equal(t, "repository 'https://git.example.com/other/apps' not permitted in project 'team-a'", ae.Detail)
	_, err = e.client("admin").ListRepositoryApps(ctx, &apiclient.RepositoryAppsRequest{Repo: repoURL, AppProject: "ghost", AppName: "web"})
	ae = requireStatus(t, err, http.StatusForbidden, apiclient.CodePermissionDenied)
	assert.Equal(t, "repository 'https://git.example.com/allowed/apps' not permitted in project 'ghost'", ae.Detail)
	_, err = e.client("admin").ListRepositoryApps(ctx, &apiclient.RepositoryAppsRequest{Repo: repoURL, AppName: "web"})
	requireStatus(t, err, http.StatusBadRequest, apiclient.CodeInvalidArgument)

	// A repository scoped to the project is permitted even when sourceRepos does not list it.
	e.createProject("team-b")
	_, err = e.client("admin").CreateRepository(ctx, &apiclient.RepositoryCreateRequest{Body: repoBody("https://git.example.com/scoped/b", "team-b", nil)})
	require.NoError(t, err)
	_, err = e.client("team-b").ListRepositoryApps(ctx, &apiclient.RepositoryAppsRequest{Repo: "https://git.example.com/scoped/b", AppProject: "team-b", AppName: "x"})
	require.NoError(t, err)
}

// TestAppDetails covers REPO-042: details are revealed for Applications the caller may
// get, of their own project and for current or historical sources only; for a new
// Application, create is required.
func TestAppDetails(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	ctx := t.Context()
	e.createProject("team-a", "*")
	e.createProject("team-b", "*")
	const repoURL = "https://git.example.com/org/web"
	current := v1alpha1.ApplicationSource{RepoURL: repoURL, Path: "charts/web", TargetRevision: "HEAD"}
	app := &v1alpha1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: e.ns},
		Spec: v1alpha1.ApplicationSpec{
			Project:     "team-a",
			Source:      &current,
			Destination: v1alpha1.ApplicationDestination{Server: "https://kubernetes.default.svc", Namespace: "web"},
		},
	}
	require.NoError(t, e.kc.Create(ctx, app))
	app.Status.History = v1alpha1.RevisionHistories{{ID: 1, Revision: "abc123", DeployedAt: metav1.NewTime(time.Now()), Source: &v1alpha1.ApplicationSource{RepoURL: repoURL, Path: "charts/old", TargetRevision: "HEAD"}}}
	require.NoError(t, e.kc.Status().Update(ctx, app))
	_, err := e.client("admin").CreateRepository(ctx, &apiclient.RepositoryCreateRequest{Body: repoBody(repoURL, "team-a", nil)})
	require.NoError(t, err)

	teamA := e.client("team-a")
	details := func(c *apiclient.Client, q apiclient.RepositoryAppDetailsQuery) (*apiclient.RepositoryAppDetails, error) {
		return c.GetRepositoryAppDetails(ctx, &apiclient.RepositoryAppDetailsRequest{Repo: repoURL, Body: q})
	}
	d, err := details(teamA, apiclient.RepositoryAppDetailsQuery{Source: &current, AppName: "web", AppProject: "team-a"})
	require.NoError(t, err)
	assert.Equal(t, "Helm", d.Type)
	require.NotNil(t, d.Helm)
	assert.Equal(t, []string{"values.yaml"}, d.Helm.ValueFiles)

	// A historical source compares with the synced revision.
	old := v1alpha1.ApplicationSource{RepoURL: repoURL, Path: "charts/old", TargetRevision: "abc123"}
	_, err = details(teamA, apiclient.RepositoryAppDetailsQuery{Source: &old, AppName: "web", AppProject: "team-a"})
	require.NoError(t, err)

	// Arbitrary source values of an existing Application: denied.
	arbitrary := v1alpha1.ApplicationSource{RepoURL: repoURL, Path: "../../secrets", TargetRevision: "HEAD"}
	_, err = details(teamA, apiclient.RepositoryAppDetailsQuery{Source: &arbitrary, AppName: "web", AppProject: "team-a"})
	requireStatus(t, err, http.StatusForbidden, apiclient.CodePermissionDenied)
	// The Application belongs to team-a: naming another project is denied.
	_, err = details(e.client("admin"), apiclient.RepositoryAppDetailsQuery{Source: &current, AppName: "web", AppProject: "team-b"})
	requireStatus(t, err, http.StatusForbidden, apiclient.CodePermissionDenied)
	// team-b may not get team-a's Application.
	_, err = details(e.client("team-b"), apiclient.RepositoryAppDetailsQuery{Source: &current, AppName: "web", AppProject: "team-a"})
	requireStatus(t, err, http.StatusForbidden, apiclient.CodePermissionDenied)
	// A new Application needs create: team-a may, reader may not.
	_, err = details(teamA, apiclient.RepositoryAppDetailsQuery{Source: &arbitrary, AppName: "new-app", AppProject: "team-a"})
	require.NoError(t, err)
	_, err = details(e.client("reader"), apiclient.RepositoryAppDetailsQuery{Source: &arbitrary, AppName: "new-app", AppProject: "team-a"})
	requireStatus(t, err, http.StatusForbidden, apiclient.CodePermissionDenied)

	// Missing source; source of another repository.
	_, err = details(teamA, apiclient.RepositoryAppDetailsQuery{AppName: "web", AppProject: "team-a"})
	ae := requireStatus(t, err, http.StatusBadRequest, apiclient.CodeInvalidArgument)
	assert.Equal(t, "missing payload in request", ae.Detail)
	other := v1alpha1.ApplicationSource{RepoURL: "https://git.example.com/org/other"}
	_, err = details(teamA, apiclient.RepositoryAppDetailsQuery{Source: &other, AppName: "web", AppProject: "team-a"})
	requireStatus(t, err, http.StatusBadRequest, apiclient.CodeInvalidArgument)

	// Settings reach the repo-server.
	e.repo.mu.Lock()
	last := e.repo.appDetails[len(e.repo.appDetails)-1]
	e.repo.mu.Unlock()
	assert.Equal(t, "--enable-helm", last.KustomizeOptions.BuildOptions)
	assert.Equal(t, []string{"https"}, last.HelmOptions.ValuesFileSchemes)
}
