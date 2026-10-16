package projects_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

func teamProject(name string) *v1alpha1.AppProject {
	return &v1alpha1.AppProject{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.AppProjectSpec{
			Description:  "team project",
			SourceRepos:  []string{"https://github.com/org/app"},
			Destinations: []v1alpha1.ApplicationDestination{{Server: "https://kube.example.com", Namespace: "apps"}},
			Roles: []v1alpha1.ProjectRole{
				{Name: "ci", Policies: []string{"p,proj:" + name + ":ci,applications,get," + name + "/*,allow"}, Groups: []string{"ci-admins"}},
				{Name: "deploy"},
			},
		},
	}
}

// TestProjectCRUD covers TEN-044/045/048/002/009 through pkg/apiclient: creation with
// normalization and validation, idempotent create and upsert, get/list, update, delete
// rules, and audit annotations.
func TestProjectCRUD(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	admin := e.client("admin")

	in := teamProject("team-a")
	in.Status.JWTTokensByRole = map[string]v1alpha1.JWTTokens{"ci": {Items: []v1alpha1.JWTToken{{ID: "forged", IssuedAt: 1}}}}
	in.Spec.Roles[1].JWTTokens = []v1alpha1.JWTToken{{ID: "forged-legacy", IssuedAt: 2}} //nolint:staticcheck // deprecated field: must be ignored
	in.Spec.SyncWindows = v1alpha1.SyncWindows{{Kind: "allow", Schedule: "0 10 * * *", Duration: "1h", Applications: []string{"*"}}}
	created, err := admin.CreateProject(ctx, in, false)
	require.NoError(t, err)
	assert.Equal(t, "AppProject", created.Kind)
	assert.Equal(t, []string{"p, proj:team-a:ci, applications, get, team-a/*, allow"}, created.Spec.Roles[0].Policies, "policies normalized")
	assert.Equal(t, "UTC", created.Spec.SyncWindows[0].TimeZone, "time zones normalized")
	stored := e.stored("team-a")
	assert.Empty(t, stored.Status.JWTTokensByRole, "status is not writable")
	assert.Empty(t, stored.Spec.Roles[1].JWTTokens, "token records are not writable") //nolint:staticcheck // deprecated field

	// Validation (TEN-009/017/018/021) with upstream wording and codes.
	for _, tc := range []struct {
		mutate func(p *v1alpha1.AppProject)
		status int
		detail string
	}{
		{func(p *v1alpha1.AppProject) {
			p.Spec.Destinations = append(p.Spec.Destinations, v1alpha1.ApplicationDestination{Server: "!*"})
		}, 400, "error validating project: server has an invalid format, '!*'"},
		{func(p *v1alpha1.AppProject) { p.Spec.SourceRepos = []string{"x", "x"} }, 400, "error validating project: source repository 'x' already added"},
		{func(p *v1alpha1.AppProject) { p.Spec.Roles[0].Name = "-bad" }, 400, "error validating project: invalid role name '-bad'. Must consist of alphanumeric characters, '-' or '_', and must start and end with an alphanumeric character"},
		{func(p *v1alpha1.AppProject) { p.Spec.Roles[1].Name = "ci" }, 409, "error validating project: role 'ci' already exists"},
		{func(p *v1alpha1.AppProject) {
			p.Spec.Roles[0].Policies = []string{"p, proj:team-x:ci, applications, get, team-x/*, allow"}
		}, 400, "error validating project: invalid policy rule 'p, proj:team-x:ci, applications, get, team-x/*, allow': policy subject must be: 'proj:team-v:ci', not 'proj:team-x:ci'"},
		{func(p *v1alpha1.AppProject) { p.Spec.SourceNamespaces = []string{"/(unclosed/"} }, 400, "source namespace '/(unclosed/' is not a valid regular expression"},
		{func(p *v1alpha1.AppProject) { p.Spec.SourceNamespaces = []string{"a", "a"} }, 400, "error validating project: source namespace 'a' already added"},
		{func(p *v1alpha1.AppProject) {
			p.Spec.SyncWindows = v1alpha1.SyncWindows{{Kind: "allow", Schedule: "0 10 * * *", Duration: "1h"}}
		}, 400, "error validating project: window 'allow':'0 10 * * *':'1h' requires one of application, cluster or namespace"},
	} {
		p := teamProject("team-v")
		tc.mutate(p)
		_, err := admin.CreateProject(ctx, p, false)
		ae := apiErr(t, err)
		assert.Equal(t, tc.status, ae.StatusCode, tc.detail)
		assert.True(t, strings.HasPrefix(ae.Problem.Detail, tc.detail), "got %q want prefix %q", ae.Problem.Detail, tc.detail)
	}
	_, err = admin.CreateProject(ctx, nil, false)
	assert.Equal(t, "missing payload 'project' in request", apiErr(t, err).Problem.Detail)

	// Idempotent create, spec diff, upsert (TEN-044).
	same := teamProject("team-a")
	same.Spec.SyncWindows = v1alpha1.SyncWindows{{Kind: "allow", Schedule: "0 10 * * *", Duration: "1h", Applications: []string{"*"}}}
	again, err := admin.CreateProject(ctx, same, false)
	require.NoError(t, err)
	assert.Equal(t, stored.UID, again.UID)
	changed := same.DeepCopy()
	changed.Spec.Description = "changed"
	changed.Spec.SourceRepos = append(changed.Spec.SourceRepos, "https://github.com/org/more")
	_, err = admin.CreateProject(ctx, changed, false)
	ae := apiErr(t, err)
	assert.Equal(t, http.StatusBadRequest, ae.StatusCode)
	assert.Equal(t, `existing project spec is different; use upsert flag to force update; difference in keys "SourceRepos,1,Description"`, ae.Problem.Detail)
	up, err := admin.CreateProject(ctx, changed, true)
	require.NoError(t, err)
	assert.Equal(t, "changed", up.Spec.Description)

	// Get and list (sorted, paginated).
	_, err = admin.CreateProject(ctx, teamProject("team-b"), false)
	require.NoError(t, err)
	got, err := admin.GetProject(ctx, "team-a")
	require.NoError(t, err)
	assert.Equal(t, "changed", got.Spec.Description)
	_, err = admin.GetProject(ctx, "missing")
	assert.Equal(t, http.StatusNotFound, apiErr(t, err).StatusCode)
	list, err := admin.ListProjects(ctx, nil)
	require.NoError(t, err)
	require.Len(t, list.Items, 2)
	assert.Equal(t, []string{"team-a", "team-b"}, []string{list.Items[0].Name, list.Items[1].Name})
	page, err := admin.ListProjects(ctx, &apiclient.ProjectListRequest{Limit: 1})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	page, err = admin.ListProjects(ctx, &apiclient.ProjectListRequest{Limit: 1, Continue: page.Continue})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	assert.Equal(t, "team-b", page.Items[0].Name)

	// Update: spec, labels; status and token records in the body are ignored; name and
	// resourceVersion are checked.
	got.Spec.Description = "updated"
	got.Labels = map[string]string{"tier": "gold"}
	got.Status.JWTTokensByRole = map[string]v1alpha1.JWTTokens{"ci": {Items: []v1alpha1.JWTToken{{ID: "forged", IssuedAt: 1}}}}
	updated, err := admin.UpdateProject(ctx, got)
	require.NoError(t, err)
	assert.Equal(t, "updated", updated.Spec.Description)
	assert.Equal(t, "gold", updated.Labels["tier"])
	assert.Empty(t, e.stored("team-a").Status.JWTTokensByRole)
	stale := got.DeepCopy()
	_, err = admin.UpdateProject(ctx, stale)
	assert.Equal(t, http.StatusConflict, apiErr(t, err).StatusCode, "stale resourceVersion")
	wrong := updated.DeepCopy()
	wrong.Name = "team-b"
	err = admin.Do(ctx, http.MethodPut, apiclient.PathProject, &apiclient.ProjectUpdateRequest{Name: "team-a", Project: wrong}, nil)
	assert.Contains(t, apiErr(t, err).Problem.Detail, "does not match the path name")

	// Delete rules (TEN-002, TEN-048).
	ae = apiErr(t, admin.DeleteProject(ctx, "default"))
	assert.Equal(t, http.StatusBadRequest, ae.StatusCode)
	assert.Equal(t, "name 'default' is reserved and cannot be deleted", ae.Problem.Detail)
	e.createApp("app1", "team-b", "https://github.com/org/app", "https://kube.example.com", "apps")
	ae = apiErr(t, admin.DeleteProject(ctx, "team-b"))
	assert.Equal(t, "project is referenced by 1 applications", ae.Problem.Detail)
	require.NoError(t, e.kube.Delete(ctx, &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: "app1", Namespace: e.ns}}))
	appset := &v1alpha1.ApplicationSet{ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: e.ns}}
	appset.Spec.Generators = []v1alpha1.ApplicationSetGenerator{}
	appset.Spec.Template.Spec.Project = "team-b"
	appset.Spec.Template.Spec.Destination = v1alpha1.ApplicationDestination{Server: "https://kube.example.com"}
	require.NoError(t, e.kube.Create(ctx, appset))
	ae = apiErr(t, admin.DeleteProject(ctx, "team-b"))
	assert.Equal(t, "project is referenced by 1 applicationsets", ae.Problem.Detail)
	require.NoError(t, e.kube.Delete(ctx, appset))
	require.NoError(t, admin.DeleteProject(ctx, "team-b"))
	assert.Equal(t, http.StatusNotFound, apiErr(t, admin.DeleteProject(ctx, "team-b")).StatusCode)

	// Audit annotations: target, event reason and upstream message.
	creates := e.audit.byOperation("createProject")
	require.NotEmpty(t, creates)
	assert.Equal(t, "AppProject", creates[0].Target.Kind)
	assert.Equal(t, "ResourceCreated", creates[0].EventReason)
	assert.Equal(t, "admin created project", creates[0].Message)
	deletes := e.audit.byOperation("deleteProject")
	assert.Equal(t, "admin deleted project", deletes[len(deletes)-2].Message)
}

// TestProjectAuthorization covers RBAC per route (object = project name, checked before
// existence), per-item list filtering, the privilege-escalation checks of update and
// create (TEN-046), and anonymous access.
func TestProjectAuthorization(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	admin, dave, erin, carol, alice := e.client("admin"), e.client("dave"), e.client("erin"), e.client("carol"), e.client("alice")
	_, err := admin.CreateProject(ctx, teamProject("team-a"), false)
	require.NoError(t, err)
	_, err = admin.CreateProject(ctx, teamProject("team-b"), false)
	require.NoError(t, err)

	// Lists are filtered per project.
	names := func(c *apiclient.Client) []string {
		l, err := c.ListProjects(ctx, nil)
		require.NoError(t, err)
		var out []string
		for _, p := range l.Items {
			out = append(out, p.Name)
		}
		return out
	}
	assert.Equal(t, []string{"team-a"}, names(dave))
	assert.Equal(t, []string{"team-a", "team-b"}, names(alice))
	assert.Empty(t, names(carol))

	// RBAC is checked before existence: no oracle.
	_, err = carol.GetProject(ctx, "team-a")
	existing := apiErr(t, err)
	_, err = carol.GetProject(ctx, "nope")
	missing := apiErr(t, err)
	assert.Equal(t, http.StatusForbidden, existing.StatusCode)
	assert.Equal(t, existing.StatusCode, missing.StatusCode)
	assert.True(t, strings.HasPrefix(existing.Problem.Detail, "permission denied: projects, get, team-a, sub: carol"), existing.Problem.Detail)
	for _, f := range []func(name string) error{
		func(n string) error { _, err := carol.GetDetailedProject(ctx, n); return err },
		func(n string) error { _, err := carol.GetGlobalProjects(ctx, n); return err },
		func(n string) error { _, err := carol.ListProjectEvents(ctx, n); return err },
		func(n string) error { _, err := carol.GetProjectSyncWindows(ctx, n); return err },
		func(n string) error { return carol.DeleteProject(ctx, n) },
	} {
		assert.Equal(t, http.StatusForbidden, apiErr(t, f("team-a")).StatusCode)
	}
	_, err = alice.UpdateProject(ctx, e.stored("team-a"))
	assert.Equal(t, http.StatusForbidden, apiErr(t, err).StatusCode, "read-only")
	_, err = dave.GetProject(ctx, "team-b")
	assert.Equal(t, http.StatusForbidden, apiErr(t, err).StatusCode)

	// Escalation checks on update: dave may update team-a's description and roles but not
	// add or remove destinations or sources, nor change resource lists.
	p, err := dave.GetProject(ctx, "team-a")
	require.NoError(t, err)
	p.Spec.Description = "by dave"
	p.Spec.Roles = append(p.Spec.Roles, v1alpha1.ProjectRole{Name: "viewer"})
	p, err = dave.UpdateProject(ctx, p)
	require.NoError(t, err)
	for _, tc := range []struct {
		mutate func(p *v1alpha1.AppProject)
		detail string
	}{
		{func(p *v1alpha1.AppProject) {
			p.Spec.Destinations = append(p.Spec.Destinations, v1alpha1.ApplicationDestination{Server: "https://other.example.com", Namespace: "*"})
		}, "permission denied: clusters, update, https://other.example.com, sub: dave"},
		{func(p *v1alpha1.AppProject) { p.Spec.Destinations = nil }, "permission denied: clusters, update, https://kube.example.com, sub: dave"},
		{func(p *v1alpha1.AppProject) {
			p.Spec.SourceRepos = append(p.Spec.SourceRepos, "https://github.com/evil/*")
		}, "permission denied: repositories, update, https://github.com/evil/*, sub: dave"},
		{func(p *v1alpha1.AppProject) {
			p.Spec.ClusterResourceWhitelist = []v1alpha1.ClusterResourceRestrictionItem{{Group: "*", Kind: "*"}}
		}, "permission denied: clusters, update, https://kube.example.com, sub: dave"},
	} {
		q := p.DeepCopy()
		tc.mutate(q)
		_, err := dave.UpdateProject(ctx, q)
		ae := apiErr(t, err)
		assert.Equal(t, http.StatusForbidden, ae.StatusCode)
		assert.True(t, strings.HasPrefix(ae.Problem.Detail, tc.detail), ae.Problem.Detail)
	}
	// erin holds the permissions for one server and one organization.
	q, err := erin.GetProject(ctx, "team-a")
	require.NoError(t, err)
	q.Spec.Destinations = append(q.Spec.Destinations, v1alpha1.ApplicationDestination{Server: "https://allowed.example.com", Namespace: "*"})
	q.Spec.SourceRepos = append(q.Spec.SourceRepos, "https://github.com/org/other")
	_, err = erin.UpdateProject(ctx, q)
	require.NoError(t, err)

	// Creation grants destinations and sources too (Patrol: same checks as an update).
	c := teamProject("team-c")
	_, err = dave.CreateProject(ctx, c, false)
	ae := apiErr(t, err)
	assert.True(t, strings.HasPrefix(ae.Problem.Detail, "permission denied: clusters, update, https://kube.example.com"), ae.Problem.Detail)
	// An existing project the caller may not get is neither returned nor compared.
	_, err = dave.CreateProject(ctx, teamProject("team-b"), false)
	ae = apiErr(t, err)
	assert.Equal(t, http.StatusConflict, ae.StatusCode)
	assert.Equal(t, "project 'team-b' already exists", ae.Problem.Detail)
	empty := &v1alpha1.AppProject{ObjectMeta: metav1.ObjectMeta{Name: "team-c"}}
	_, err = dave.CreateProject(ctx, empty, false)
	require.NoError(t, err)
	_, err = dave.CreateProject(ctx, &v1alpha1.AppProject{ObjectMeta: metav1.ObjectMeta{Name: "other"}}, false)
	assert.Equal(t, http.StatusForbidden, apiErr(t, err).StatusCode)
	// dave may not get team-c: an upsert only learns that it exists.
	empty.Spec.Description = "x"
	_, err = dave.CreateProject(ctx, empty, true)
	assert.Equal(t, http.StatusConflict, apiErr(t, err).StatusCode)
	// Upsert needs projects, update (gina may get and create, not update).
	_, err = e.client("gina").CreateProject(ctx, empty, true)
	assert.True(t, strings.HasPrefix(apiErr(t, err).Problem.Detail, "permission denied: projects, update, team-c"))
	require.NoError(t, dave.DeleteProject(ctx, "team-c"))

	// Anonymous callers are rejected.
	_, err = e.client("").ListProjects(ctx, nil)
	assert.Equal(t, http.StatusUnauthorized, apiErr(t, err).StatusCode)
}

// TestProjectUpdateImpact covers TEN-047: an update that makes existing Applications lose a
// permitted source or destination is refused with the upstream message.
func TestProjectUpdateImpact(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	admin := e.client("admin")
	p := teamProject("team-a")
	p.Spec.SourceRepos = []string{"https://github.com/org/app", "https://github.com/org/*"}
	p.Spec.Destinations = append(p.Spec.Destinations, v1alpha1.ApplicationDestination{Server: "https://kube.example.com", Namespace: "*"})
	_, err := admin.CreateProject(ctx, p, false)
	require.NoError(t, err)
	e.createCluster("kube", "https://kube.example.com", "")
	e.createApp("web", "team-a", "https://github.com/org/app", "https://kube.example.com", "apps")
	e.createApp("other-ns", "team-a", "https://github.com/org/app", "https://kube.example.com", "web")
	e.createApp("elsewhere", "team-b", "https://github.com/x/y", "https://kube.example.com", "apps")

	cur, err := admin.GetProject(ctx, "team-a")
	require.NoError(t, err)
	// Still permitted by another entry: allowed.
	next := cur.DeepCopy()
	next.Spec.SourceRepos = []string{"https://github.com/org/*"}
	next.Spec.Destinations = next.Spec.Destinations[1:]
	cur, err = admin.UpdateProject(ctx, next)
	require.NoError(t, err)

	next = cur.DeepCopy()
	next.Spec.SourceRepos = []string{"https://github.com/other/*"}
	_, err = admin.UpdateProject(ctx, next)
	ae := apiErr(t, err)
	assert.Equal(t, http.StatusBadRequest, ae.StatusCode)
	assert.Equal(t, "as a result of project update 2 applications source became invalid", ae.Problem.Detail)

	next = cur.DeepCopy()
	next.Spec.Destinations = []v1alpha1.ApplicationDestination{{Server: "https://kube.example.com", Namespace: "apps"}}
	_, err = admin.UpdateProject(ctx, next)
	assert.Equal(t, "as a result of project update 1 applications destination became invalid", apiErr(t, err).Problem.Detail)

	next.Spec.SourceRepos = []string{"https://github.com/other/*"}
	_, err = admin.UpdateProject(ctx, next)
	assert.Equal(t, "as a result of project update 2 applications source became invalid and 1 applications destination became invalid", apiErr(t, err).Problem.Detail)

	// Upsert is held to the same check (Patrol; upstream skips it).
	_, err = admin.CreateProject(ctx, next, true)
	assert.Contains(t, apiErr(t, err).Problem.Detail, "as a result of project update")
}

// TestProjectDetails covers TEN-037/038/039/045/049/050/051: the detailed project with
// global projects and the scoped repositories and clusters the caller may get, global
// projects, events, active sync windows and project links.
func TestProjectDetails(t *testing.T) {
	now := time.Date(2026, 10, 16, 10, 30, 0, 0, time.UTC)
	e := newEnv(t, envOptions{now: func() time.Time { return now }, cm: map[string]string{
		"globalProjects": "- projectName: global\n  labelSelector:\n    matchLabels:\n      inherit: \"yes\"\n",
		"project.links": `- url: https://dash.example.com/{{.project.metadata.name}}?tier={{ default "none" (index .project.metadata.labels "tier") | upper }}
  title: Dashboard
  description: team dashboard
- url: https://status.example.com/{{.project.status}}
  title: Status
- url: https://cond.example.com/{{.project.metadata.name}}
  title: Conditional
  if: project.metadata.name == "team-b"
- url: javascript:alert(1)
  title: Evil
- url: https://env.example.com/{{ env "HOME" }}
  title: Env
`,
	}})
	ctx := context.Background()
	admin, dave, erin, carol := e.client("admin"), e.client("dave"), e.client("erin"), e.client("carol")

	global := &v1alpha1.AppProject{ObjectMeta: metav1.ObjectMeta{Name: "global"}, Spec: v1alpha1.AppProjectSpec{
		SourceRepos:              []string{"https://github.com/shared/*"},
		ClusterResourceWhitelist: []v1alpha1.ClusterResourceRestrictionItem{{Group: "*", Kind: "Namespace"}},
		Roles:                    []v1alpha1.ProjectRole{{Name: "r"}},
	}}
	_, err := admin.CreateProject(ctx, global, false)
	require.NoError(t, err)
	p := teamProject("team-a")
	p.Labels = map[string]string{"inherit": "yes", "tier": "gold"}
	p.Spec.SyncWindows = v1alpha1.SyncWindows{
		{Kind: "allow", Schedule: "0 10 * * *", Duration: "1h", Applications: []string{"*"}},
		{Kind: "deny", Schedule: "0 22 * * *", Duration: "1h", Namespaces: []string{"*"}},
	}
	_, err = admin.CreateProject(ctx, p, false)
	require.NoError(t, err)

	// Scoped resources of team-a and team-b.
	for _, r := range []struct{ name, url, project string }{{"scoped-a", "https://github.com/org/scoped", "team-a"}, {"scoped-b", "https://github.com/org/b", "team-b"}, {"global-repo", "https://github.com/org/global", ""}} {
		require.NoError(t, e.kube.Create(ctx, &v1alpha1.Repository{ObjectMeta: metav1.ObjectMeta{Name: r.name, Namespace: e.ns}, Spec: v1alpha1.RepositorySpec{URL: r.url, Project: r.project}}))
	}
	sc := e.createCluster("scoped-cluster", "https://scoped.example.com", "team-a")
	require.NoError(t, e.kube.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "scoped-creds", Namespace: e.ns, Labels: map[string]string{v1alpha1.LabelKeySecretType: v1alpha1.LabelValueSecretTypeCluster}},
		Data: map[string][]byte{v1alpha1.ClusterSecretKeyBearerToken: []byte("TOP-SECRET")}}))
	sc.Spec.Config.CredentialsRef = &v1alpha1.LocalSecretReference{Name: "scoped-creds"}
	require.NoError(t, e.kube.Update(ctx, sc))
	e.createCluster("other-cluster", "https://other.example.com", "team-b")

	d, err := erin.GetDetailedProject(ctx, "team-a")
	require.NoError(t, err)
	assert.Contains(t, d.Project.Spec.SourceRepos, "https://github.com/shared/*", "global project merged")
	assert.Equal(t, []v1alpha1.ClusterResourceRestrictionItem{{Group: "*", Kind: "Namespace"}}, d.Project.Spec.ClusterResourceWhitelist)
	assert.Len(t, d.Project.Spec.Roles, 2, "roles are not inherited")
	require.Len(t, d.GlobalProjects, 1)
	assert.Equal(t, "global", d.GlobalProjects[0].Name)
	require.Len(t, d.Repositories, 1)
	assert.Equal(t, "https://github.com/org/scoped", d.Repositories[0].Spec.URL)
	require.Len(t, d.Clusters, 1)
	assert.Equal(t, "https://scoped.example.com", d.Clusters[0].Spec.Server)
	assert.True(t, d.Clusters[0].Credentials.HasBearerToken)
	resp, err := erin.DoRaw(ctx, http.MethodGet, apiclient.PathProjectDetailed, &apiclient.ProjectRequest{Name: "team-a"}, nil, "")
	require.NoError(t, err)
	raw, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "TOP-SECRET")
	// dave may get the project but not its scoped repositories or clusters.
	d, err = dave.GetDetailedProject(ctx, "team-a")
	require.NoError(t, err)
	assert.Empty(t, d.Repositories)
	assert.Empty(t, d.Clusters)
	assert.Len(t, d.GlobalProjects, 1)

	gp, err := dave.GetGlobalProjects(ctx, "team-a")
	require.NoError(t, err)
	require.Len(t, gp.Items, 1)
	assert.Empty(t, gp.Items[0].Status.JWTTokensByRole)
	gp, err = admin.GetGlobalProjects(ctx, "global")
	require.NoError(t, err)
	assert.Empty(t, gp.Items, "a global project is not its own global project")

	// Events of the project only.
	stored := e.stored("team-a")
	other := e.stored("global")
	for i, obj := range []*v1alpha1.AppProject{stored, other} {
		require.NoError(t, e.kube.Create(ctx, &corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: "ev-" + obj.Name, Namespace: e.ns},
			InvolvedObject: corev1.ObjectReference{Kind: "AppProject", APIVersion: v1alpha1.GroupVersion.String(), Name: obj.Name, Namespace: e.ns, UID: obj.UID},
			Reason:         "ResourceUpdated", Message: []string{"admin updated project", "other"}[i], Type: corev1.EventTypeNormal,
		}))
	}
	evs, err := dave.ListProjectEvents(ctx, "team-a")
	require.NoError(t, err)
	require.Len(t, evs.Items, 1)
	assert.Equal(t, "admin updated project", evs.Items[0].Message)
	_, err = admin.ListProjectEvents(ctx, "missing")
	assert.Equal(t, http.StatusNotFound, apiErr(t, err).StatusCode)

	// Active sync windows at 10:30 UTC: the allow window only.
	sw, err := dave.GetProjectSyncWindows(ctx, "team-a")
	require.NoError(t, err)
	require.Len(t, sw.Windows, 1)
	assert.Equal(t, "allow", sw.Windows[0].Kind)
	sw, err = admin.GetProjectSyncWindows(ctx, "global")
	require.NoError(t, err)
	assert.Empty(t, sw.Windows)
	assert.NotNil(t, sw.Windows)

	// Project links: rendered with sprig, status data unavailable, condition false
	// skipped, non-http(s) and env-reading links skipped.
	links, err := dave.ListProjectLinks(ctx, "team-a")
	require.NoError(t, err)
	require.Len(t, links.Items, 2)
	assert.Equal(t, "Dashboard", links.Items[0].Title)
	assert.Equal(t, "https://dash.example.com/team-a?tier=GOLD", links.Items[0].URL)
	assert.Equal(t, "team dashboard", *links.Items[0].Description)
	assert.Equal(t, "https://status.example.com/map[]", links.Items[1].URL, "status emptied (upstream)")
	_, err = carol.ListProjectLinks(ctx, "team-a")
	ae := apiErr(t, err)
	assert.Equal(t, http.StatusForbidden, ae.StatusCode)
	assert.Equal(t, "unauthorized access to project team-a", ae.Problem.Detail)
}
