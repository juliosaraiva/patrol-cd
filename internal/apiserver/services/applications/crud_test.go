package applications_test

import (
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	reposerver "github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

func TestCreateApplication(t *testing.T) {
	e := newEnv(t, envOptions{})
	alice := e.client("alice")
	app := e.newApp("guestbook", "")
	app.Spec.Project = "proj-a"
	app.Labels = map[string]string{"team": "a"}

	created, err := alice.CreateApplication(e.ctx, &apiclient.ApplicationCreateRequest{Application: app})
	require.NoError(t, err)
	assert.Equal(t, "proj-a", created.Spec.Project)
	assert.NotEmpty(t, created.UID)
	// validate=true rendered the source through the repo-server with the project's settings.
	gen := e.repo.lastGenerate()
	require.NotNil(t, gen)
	assert.True(t, gen.NoRevisionCache)
	assert.Equal(t, "guestbook", gen.Source.Path)
	assert.Equal(t, "proj-a", gen.ProjectName)
	assert.Contains(t, e.events(), "Normal ResourceCreated alice created application")
	audits := e.audit.byOperation("createApplication")
	require.Len(t, audits, 1)
	assert.Equal(t, "succeeded", string(audits[0].Result))
	assert.Equal(t, "proj-a/guestbook", audits[0].Object)

	// Identical create is idempotent.
	again, err := alice.CreateApplication(e.ctx, &apiclient.ApplicationCreateRequest{Application: app})
	require.NoError(t, err)
	assert.Equal(t, created.ResourceVersion, again.ResourceVersion)

	// A different spec needs upsert, and upsert needs update.
	changed := app.DeepCopy()
	changed.Spec.Source.TargetRevision = "v2"
	_, err = alice.CreateApplication(e.ctx, &apiclient.ApplicationCreateRequest{Application: changed})
	requireProblem(t, err, http.StatusBadRequest, "existing application spec is different, use upsert flag to force update")
	upserted, err := alice.CreateApplication(e.ctx, &apiclient.ApplicationCreateRequest{Application: changed, Upsert: ptrBool(true)})
	require.NoError(t, err)
	assert.Equal(t, "v2", upserted.Spec.Source.TargetRevision)
	assert.Equal(t, map[string]string{"team": "a"}, upserted.Labels)
}

func TestCreateApplicationDenials(t *testing.T) {
	e := newEnv(t, envOptions{})
	app := e.newApp("guestbook", "proj-a")

	_, err := e.client("bob").CreateApplication(e.ctx, &apiclient.ApplicationCreateRequest{Application: app})
	requireProblem(t, err, http.StatusForbidden, "permission denied: applications, create, proj-a/guestbook, sub: bob")
	_, err = e.client("reader").CreateApplication(e.ctx, &apiclient.ApplicationCreateRequest{Application: app})
	requireProblem(t, err, http.StatusForbidden, "permission denied: applications, create, proj-a/guestbook")
	_, err = e.client("").CreateApplication(e.ctx, &apiclient.ApplicationCreateRequest{Application: app})
	requireProblem(t, err, http.StatusUnauthorized, "")
	denied := e.audit.byOperation("createApplication")
	require.NotEmpty(t, denied)
	assert.Equal(t, "denied", string(denied[0].Result))

	// Nonexistent project: the vague message, also for admins.
	missing := e.newApp("ghost", "nope")
	_, err = e.client("admin").CreateApplication(e.ctx, &apiclient.ApplicationCreateRequest{Application: missing})
	requireProblem(t, err, http.StatusBadRequest, `app is not allowed in project "nope", or the project does not exist`)

	// Nil application and empty name.
	_, err = e.client("admin").CreateApplication(e.ctx, &apiclient.ApplicationCreateRequest{})
	requireProblem(t, err, http.StatusBadRequest, "")
	noName := e.newApp("", "proj-a")
	_, err = e.client("admin").CreateApplication(e.ctx, &apiclient.ApplicationCreateRequest{Application: noName})
	requireProblem(t, err, http.StatusBadRequest, "resource name may not be empty")

	// Upsert by a user without update.
	e.createApp(app)
	e.updateProject("proj-a", func(p *v1alpha1.AppProject) {
		p.Spec.Roles = []v1alpha1.ProjectRole{{Name: "creator", Policies: []string{"p, proj:proj-a:creator, applications, create, proj-a/*, allow", "p, proj:proj-a:creator, applications, get, proj-a/*, allow"}}}
	})
	changed := app.DeepCopy()
	changed.Spec.Source.TargetRevision = "v9"
	_, err = e.projectClient("proj-a", "creator").CreateApplication(e.ctx, &apiclient.ApplicationCreateRequest{Application: changed, Upsert: ptrBool(true)})
	requireProblem(t, err, http.StatusForbidden, "permission denied: applications, update, proj-a/guestbook")

	// Taking the name of an Application the caller cannot see reveals nothing of it.
	other := e.createApp(e.newApp("secret-app", "proj-b"))
	steal := e.newApp("secret-app", "proj-a")
	_, err = e.client("alice").CreateApplication(e.ctx, &apiclient.ApplicationCreateRequest{Application: steal, Upsert: ptrBool(true)})
	ae := requireProblem(t, err, http.StatusConflict, `application "secret-app" already exists`)
	assert.NotContains(t, ae.Detail, "proj-b")
	assert.Equal(t, "proj-b", e.getApp(other.Namespace, other.Name).Spec.Project)
}

func TestCreateApplicationValidation(t *testing.T) {
	e := newEnv(t, envOptions{})
	admin := e.client("admin")
	e.createProject("narrow", func(p *v1alpha1.AppProject) {
		p.Spec.Destinations = []v1alpha1.ApplicationDestination{{Server: inCluster, Namespace: "allowed"}}
	})

	cases := []struct {
		name     string
		mutate   func(*v1alpha1.Application)
		validate bool
		status   int
		detail   string
	}{
		{"destination not permitted even without validation", func(a *v1alpha1.Application) { a.Spec.Project = "narrow" }, false, 400,
			"application spec for v1 is invalid: InvalidSpecError: application destination server 'https://kubernetes.default.svc' and namespace 'default' do not match any of the allowed destinations in project 'narrow'"},
		{"unknown destination cluster", func(a *v1alpha1.Application) { a.Spec.Destination.Server = "https://nowhere.example.com" }, false, 400,
			"application destination spec for v1 is invalid:"},
		{"repository not permitted", func(a *v1alpha1.Application) { a.Spec.Source.RepoURL = "https://gitlab.com/evil/repo" }, false, 400,
			"InvalidSpecError: application repo https://gitlab.com/evil/repo is not permitted in project 'proj-a'"},
		{"invalid managed-by-url", func(a *v1alpha1.Application) {
			a.Annotations = map[string]string{v1alpha1.AnnotationKeyManagedByURL: "patrol.example.com"}
		}, false, 400, "application spec for v1 is invalid: InvalidSpecError: invalid managed-by URL: URL must include http or https protocol"},
		{"duplicate source names", func(a *v1alpha1.Application) {
			a.Spec.Source = nil
			a.Spec.Sources = v1alpha1.ApplicationSources{{RepoURL: repoURL, Path: "a", Name: "x"}, {RepoURL: repoURL, Path: "b", Name: "x"}}
		}, false, 400, "application v1 has duplicate source name: x"},
		{"invalid sync option", func(a *v1alpha1.Application) {
			a.Spec.SyncPolicy = &v1alpha1.SyncPolicy{SyncOptions: v1alpha1.SyncOptions{"ServerSideApply=True"}}
		}, false, 400, `invalid value "True" for sync option ServerSideApply`},
		{"render failure with validation", func(a *v1alpha1.Application) { a.Spec.Source.Path = "missing" }, true, 400,
			"application spec for v1 is invalid: InvalidSpecError: Unable to generate manifests in missing: app path does not exist"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.validate {
				e.repo.mu.Lock()
				e.repo.genErr = &reposerver.Error{Code: reposerver.CodeInvalidArgument, Message: "app path does not exist"}
				e.repo.mu.Unlock()
				defer func() { e.repo.mu.Lock(); e.repo.genErr = nil; e.repo.mu.Unlock() }()
			}
			app := e.newApp("v1", "proj-a")
			tc.mutate(app)
			_, err := admin.CreateApplication(e.ctx, &apiclient.ApplicationCreateRequest{Application: app, Validate: ptrBool(tc.validate)})
			requireProblem(t, err, tc.status, tc.detail)
		})
	}

	// The same render failure is accepted without validation.
	app := e.newApp("v1", "proj-a")
	app.Spec.Source.Path = "missing"
	_, err := admin.CreateApplication(e.ctx, &apiclient.ApplicationCreateRequest{Application: app, Validate: ptrBool(false)})
	require.NoError(t, err)
}

func TestGetApplicationExistenceHiding(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.createApp(e.newApp("app-a", "proj-a"))
	alice, bob, nobody := e.client("alice"), e.client("bob"), e.client("nobody")

	got, err := alice.GetApplication(e.ctx, &apiclient.ApplicationGetRequest{Name: "app-a"})
	require.NoError(t, err)
	assert.Equal(t, "app-a", got.Name)

	// The existence-hiding matrix (TEN-081): project given/not × exists/not × allowed/not.
	cases := []struct {
		name    string
		c       *apiclient.Client
		app     string
		project []string
		status  int
	}{
		{"no project, exists, forbidden", bob, "app-a", nil, 403},
		{"no project, missing, any user", bob, "ghost", nil, 403},
		{"no project, missing, allowed user", alice, "ghost", nil, 403},
		{"no role at all", nobody, "app-a", nil, 403},
		{"project given, forbidden on it", bob, "app-a", []string{"proj-a"}, 403},
		{"project given, allowed on it, app in another project", bob, "app-a", []string{"proj-b"}, 404},
		{"project given, allowed, missing", bob, "ghost", []string{"proj-b"}, 404},
		{"project given, allowed, wrong project", alice, "app-a", []string{"proj-b"}, 403},
		{"project given, correct", alice, "app-a", []string{"proj-a"}, 200},
	}
	var forbiddenBody string
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.c.GetApplication(e.ctx, &apiclient.ApplicationGetRequest{Name: tc.app, Project: tc.project})
			if tc.status == 200 {
				require.NoError(t, err)
				return
			}
			ae := requireProblem(t, err, tc.status, "")
			if tc.status == 403 && len(tc.project) == 0 {
				// Same answer whether the Application exists or not.
				if forbiddenBody == "" {
					forbiddenBody = ae.Detail
				}
				assert.Equal(t, forbiddenBody, ae.Detail)
				assert.Equal(t, "permission denied", ae.Detail)
			}
		})
	}
	_, err = alice.GetApplication(e.ctx, &apiclient.ApplicationGetRequest{Name: "app-a", Project: []string{"proj-a", "proj-b"}})
	requireProblem(t, err, 400, "multiple projects specified - the get endpoint accepts either zero or one project")
	_, err = e.client("").GetApplication(e.ctx, &apiclient.ApplicationGetRequest{Name: "app-a"})
	requireProblem(t, err, 401, "")
}

func TestListApplications(t *testing.T) {
	e := newEnv(t, envOptions{})
	for i := range 5 {
		a := e.newApp(fmt.Sprintf("a-%d", i), "proj-a")
		a.Labels = map[string]string{"tier": map[bool]string{true: "web", false: "db"}[i%2 == 0]}
		e.createApp(a)
	}
	b := e.newApp("b-0", "proj-b")
	b.Spec.Source.RepoURL = "https://github.com/org/other"
	e.createApp(b)

	names := func(l *v1alpha1.ApplicationList) []string {
		var out []string
		for _, a := range l.Items {
			out = append(out, a.Name)
		}
		return out
	}
	l, err := e.client("alice").ListApplications(e.ctx, &apiclient.ApplicationQuery{})
	require.NoError(t, err)
	assert.Equal(t, []string{"a-0", "a-1", "a-2", "a-3", "a-4"}, names(l))
	assert.NotEmpty(t, l.ResourceVersion)

	l, err = e.client("bob").ListApplications(e.ctx, &apiclient.ApplicationQuery{})
	require.NoError(t, err)
	assert.Equal(t, []string{"b-0"}, names(l))

	l, err = e.client("nobody").ListApplications(e.ctx, &apiclient.ApplicationQuery{})
	require.NoError(t, err)
	assert.Empty(t, l.Items)

	admin := e.client("admin")
	for _, tc := range []struct {
		name string
		q    apiclient.ApplicationQuery
		want []string
	}{
		{"all", apiclient.ApplicationQuery{}, []string{"a-0", "a-1", "a-2", "a-3", "a-4", "b-0"}},
		{"projects", apiclient.ApplicationQuery{Projects: []string{"proj-b"}}, []string{"b-0"}},
		{"legacy project wins", apiclient.ApplicationQuery{Project: []string{"proj-a"}, Projects: []string{"proj-b"}}, []string{"a-0", "a-1", "a-2", "a-3", "a-4"}},
		{"selector", apiclient.ApplicationQuery{Selector: "tier=web"}, []string{"a-0", "a-2", "a-4"}},
		{"repo", apiclient.ApplicationQuery{Repo: "https://github.com/org/other"}, []string{"b-0"}},
		{"name", apiclient.ApplicationQuery{Name: "a-3"}, []string{"a-3"}},
		{"namespace", apiclient.ApplicationQuery{AppNamespace: e.ns, Name: "b-0"}, []string{"b-0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, err := admin.ListApplications(e.ctx, &tc.q)
			require.NoError(t, err)
			assert.Equal(t, tc.want, names(l))
		})
	}

	// Pagination.
	var all []string
	q := &apiclient.ApplicationQuery{Limit: 2}
	for range 10 {
		l, err := admin.ListApplications(e.ctx, q)
		require.NoError(t, err)
		require.LessOrEqual(t, len(l.Items), 2)
		all = append(all, names(l)...)
		if l.Continue == "" {
			break
		}
		q.Continue = l.Continue
	}
	assert.Equal(t, []string{"a-0", "a-1", "a-2", "a-3", "a-4", "b-0"}, all)

	_, err = admin.ListApplications(e.ctx, &apiclient.ApplicationQuery{Selector: "tier in (("})
	requireProblem(t, err, 400, "error parsing the selector")
	_, err = admin.ListApplications(e.ctx, &apiclient.ApplicationQuery{Continue: "%%%"})
	requireProblem(t, err, 400, "invalid continue token")
	_, err = admin.ListApplications(e.ctx, &apiclient.ApplicationQuery{Limit: 100000})
	requireProblem(t, err, 400, "limit must be between")
}

func TestUpdateApplication(t *testing.T) {
	e := newEnv(t, envOptions{})
	app := e.newApp("guestbook", "proj-a")
	app.Labels = map[string]string{"keep": "1", "drop": "1"}
	stored := e.createApp(app)
	alice := e.client("alice")

	upd := stored.DeepCopy()
	upd.Labels = map[string]string{"keep": "1"}
	upd.Spec.Source.TargetRevision = "v2"
	upd.Status.Sync.Status = v1alpha1.SyncStatusCodeSynced // ignored
	upd.ResourceVersion = ""
	got, err := alice.UpdateApplication(e.ctx, &apiclient.ApplicationUpdateRequest{Application: upd, Validate: ptrBool(false)})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"keep": "1"}, got.Labels)
	assert.Equal(t, "v2", got.Spec.Source.TargetRevision)
	assert.Empty(t, e.getApp(e.ns, "guestbook").Status.Sync.Status)
	assert.Contains(t, e.events(), "Normal ResourceUpdated alice updated application spec")

	// A stale resourceVersion is a conflict.
	stale := stored.DeepCopy()
	stale.Spec.Source.TargetRevision = "v3"
	_, err = alice.UpdateApplication(e.ctx, &apiclient.ApplicationUpdateRequest{Application: stale, Validate: ptrBool(false)})
	requireProblem(t, err, 409, "")
	// The current one succeeds.
	cur := e.getApp(e.ns, "guestbook")
	cur.Spec.Source.TargetRevision = "v3"
	_, err = alice.UpdateApplication(e.ctx, &apiclient.ApplicationUpdateRequest{Application: cur, Validate: ptrBool(false)})
	require.NoError(t, err)

	// Authorization.
	cur = e.getApp(e.ns, "guestbook")
	_, err = e.client("reader").UpdateApplication(e.ctx, &apiclient.ApplicationUpdateRequest{Application: cur})
	requireProblem(t, err, 403, "permission denied: applications, update, proj-a/guestbook")
	_, err = e.client("bob").UpdateApplication(e.ctx, &apiclient.ApplicationUpdateRequest{Application: cur})
	requireProblem(t, err, 403, "")

	// Path and body names must agree.
	_, err = alice.UpdateApplication(e.ctx, &apiclient.ApplicationUpdateRequest{Name: "other", Application: cur})
	requireProblem(t, err, 400, "does not match the path name")
}

func TestUpdateApplicationProjectChange(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.createApp(e.newApp("mv", "proj-a"))
	move := func(c *apiclient.Client) error {
		a := e.getApp(e.ns, "mv")
		a.Spec.Project = "proj-b"
		a.ResourceVersion = ""
		_, err := c.UpdateApplication(e.ctx, &apiclient.ApplicationUpdateRequest{Application: a, Validate: ptrBool(false)})
		return err
	}
	// alice: update in proj-a but no create in proj-b.
	requireProblem(t, move(e.client("alice")), 403, "permission denied: applications, update, proj-b/mv")
	// bob: update in proj-b but nothing in proj-a: the old Application is hidden.
	requireProblem(t, move(e.client("bob")), 403, "permission denied")
	// mover: update in proj-a and create in proj-b, but update on the new RBAC object is
	// required first (upstream Update checks the new object).
	requireProblem(t, move(e.client("mover")), 403, "permission denied: applications, update, proj-b/mv")
	// admin may move it.
	require.NoError(t, move(e.client("admin")))
	assert.Equal(t, "proj-b", e.getApp(e.ns, "mv").Spec.Project)

	// UpdateSpec: needs create on the new project and update on the old.
	e.createApp(e.newApp("mv2", "proj-a"))
	spec := e.getApp(e.ns, "mv2").Spec
	spec.Project = "proj-b"
	_, err := e.client("alice").UpdateApplicationSpec(e.ctx, &apiclient.ApplicationUpdateSpecRequest{Name: "mv2", Spec: &spec, Validate: ptrBool(false)})
	requireProblem(t, err, 403, "permission denied: applications, create, proj-b/mv2")
	got, err := e.client("mover").UpdateApplicationSpec(e.ctx, &apiclient.ApplicationUpdateSpecRequest{Name: "mv2", Spec: &spec, Validate: ptrBool(false)})
	require.NoError(t, err)
	assert.Equal(t, "proj-b", got.Project)

	// Moving to a project that does not admit the namespace fails with the vague error.
	e.createApp(e.newApp("mv3", "proj-a"))
	spec = e.getApp(e.ns, "mv3").Spec
	spec.Project = "missing"
	_, err = e.client("admin").UpdateApplicationSpec(e.ctx, &apiclient.ApplicationUpdateSpecRequest{Name: "mv3", Spec: &spec, Validate: ptrBool(false)})
	requireProblem(t, err, 400, `app is not allowed in project "missing", or the project does not exist`)
}

func TestUpdateApplicationSpec(t *testing.T) {
	e := newEnv(t, envOptions{})
	app := e.newApp("guestbook", "proj-a")
	app.Annotations = map[string]string{"note": "kept"}
	e.createApp(app)
	spec := app.Spec.DeepCopy()
	spec.Source.TargetRevision = "v2"
	spec.SyncPolicy = &v1alpha1.SyncPolicy{} // normalized away
	got, err := e.client("alice").UpdateApplicationSpec(e.ctx, &apiclient.ApplicationUpdateSpecRequest{Name: "guestbook", Spec: spec, Validate: ptrBool(false)})
	require.NoError(t, err)
	assert.Equal(t, "v2", got.Source.TargetRevision)
	assert.Nil(t, got.SyncPolicy)
	stored := e.getApp(e.ns, "guestbook")
	assert.Equal(t, "kept", stored.Annotations["note"])

	bad := spec.DeepCopy()
	bad.Destination.Namespace = ""
	bad.Destination.Server = ""
	_, err = e.client("alice").UpdateApplicationSpec(e.ctx, &apiclient.ApplicationUpdateSpecRequest{Name: "guestbook", Spec: bad, Validate: ptrBool(false)})
	requireProblem(t, err, 400, "application destination spec for guestbook is invalid: Destination server missing from app spec")
	_, err = e.client("alice").UpdateApplicationSpec(e.ctx, &apiclient.ApplicationUpdateSpecRequest{Name: "guestbook"})
	requireProblem(t, err, 400, "error updating application spec: spec is nil in request")
	_, err = e.client("reader").UpdateApplicationSpec(e.ctx, &apiclient.ApplicationUpdateSpecRequest{Name: "guestbook", Spec: spec})
	requireProblem(t, err, 403, "permission denied")
}

func TestPatchApplication(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.createApp(e.newApp("guestbook", "proj-a"))
	alice := e.client("alice")

	got, err := alice.PatchApplication(e.ctx, &apiclient.ApplicationPatchRequest{Name: "guestbook",
		Patch: `[{"op":"replace","path":"/spec/source/targetRevision","value":"v7"}]`})
	require.NoError(t, err)
	assert.Equal(t, "v7", got.Spec.Source.TargetRevision)

	got, err = alice.PatchApplication(e.ctx, &apiclient.ApplicationPatchRequest{Name: "guestbook", PatchType: "merge",
		Patch: `{"metadata":{"annotations":{"team":"a"}}}`})
	require.NoError(t, err)
	assert.Equal(t, "a", got.Annotations["team"])

	// Patching status has no effect.
	_, err = alice.PatchApplication(e.ctx, &apiclient.ApplicationPatchRequest{Name: "guestbook", PatchType: "merge",
		Patch: `{"status":{"sync":{"status":"Synced"}}}`})
	require.NoError(t, err)
	assert.Empty(t, e.getApp(e.ns, "guestbook").Status.Sync.Status)

	// A patch asserting a stale resourceVersion conflicts.
	_, err = alice.PatchApplication(e.ctx, &apiclient.ApplicationPatchRequest{Name: "guestbook", PatchType: "merge",
		Patch: `{"metadata":{"resourceVersion":"1"},"spec":{"source":{"targetRevision":"v8"}}}`})
	requireProblem(t, err, 409, "")

	for _, tc := range []struct{ typ, patch, detail string }{
		{"strategic", `{}`, "Patch type 'strategic' is not supported"},
		{"json", `{"not":"a patch"}`, "error decoding json patch"},
		{"json", `[{"op":"remove","path":"/spec/nothing"}]`, "error applying patch"},
		{"merge", `{"metadata":{"name":"renamed"}}`, "the patch must not change metadata.name or metadata.namespace"},
		{"merge", `{"spec":{"destination":{"server":"https://nowhere.example.com"}}}`, "application destination spec for guestbook is invalid"},
	} {
		_, err := alice.PatchApplication(e.ctx, &apiclient.ApplicationPatchRequest{Name: "guestbook", PatchType: tc.typ, Patch: tc.patch})
		requireProblem(t, err, 400, tc.detail)
	}
	// get but no update.
	_, err = e.client("reader").PatchApplication(e.ctx, &apiclient.ApplicationPatchRequest{Name: "guestbook", PatchType: "merge", Patch: `{}`})
	requireProblem(t, err, 403, "permission denied: applications, update, proj-a/guestbook")
	// Body fields from Argo CD clients must agree with the path.
	_, err = alice.PatchApplication(e.ctx, &apiclient.ApplicationPatchRequest{Name: "guestbook", BodyName: "x", PatchType: "merge", Patch: `{}`})
	requireProblem(t, err, 400, "does not match the path name")
}

func TestDeleteApplication(t *testing.T) {
	e := newEnv(t, envOptions{})
	alice := e.client("alice")

	e.createApp(e.newApp("fg", "proj-a"))
	require.NoError(t, alice.DeleteApplication(e.ctx, &apiclient.ApplicationDeleteRequest{Name: "fg"}))
	fg := e.getApp(e.ns, "fg") // kept by the finalizer until the controller cascades
	assert.Equal(t, []string{v1alpha1.FinalizerResources}, fg.Finalizers)
	assert.NotNil(t, fg.DeletionTimestamp)
	assert.Contains(t, e.events(), "Normal ResourceDeleted alice deleted application")
	// Deleting again while terminating is accepted (idempotent retries).
	require.NoError(t, alice.DeleteApplication(e.ctx, &apiclient.ApplicationDeleteRequest{Name: "fg", PropagationPolicy: "Background"}))
	assert.Equal(t, []string{v1alpha1.FinalizerResources}, e.getApp(e.ns, "fg").Finalizers)
	// Non-cascading removes the propagation finalizers: the Application disappears.
	require.NoError(t, alice.DeleteApplication(e.ctx, &apiclient.ApplicationDeleteRequest{Name: "fg", Cascade: ptrBool(false)}))
	err := e.kube.Get(e.ctx, client.ObjectKey{Namespace: e.ns, Name: "fg"}, &v1alpha1.Application{})
	assert.True(t, client.IgnoreNotFound(err) == nil && err != nil, "application should be gone: %v", err)

	e.createApp(e.newApp("bg", "proj-a"))
	require.NoError(t, alice.DeleteApplication(e.ctx, &apiclient.ApplicationDeleteRequest{Name: "bg", PropagationPolicy: "background"}))
	assert.Equal(t, []string{v1alpha1.FinalizerResourcesBackground}, e.getApp(e.ns, "bg").Finalizers)

	keep := e.newApp("keep", "proj-a")
	keep.Finalizers = []string{v1alpha1.FinalizerResourcesForeground, "example.com/other"}
	e.createApp(keep)
	require.NoError(t, alice.DeleteApplication(e.ctx, &apiclient.ApplicationDeleteRequest{Name: "keep", Cascade: ptrBool(false)}))
	assert.Equal(t, []string{"example.com/other"}, e.getApp(e.ns, "keep").Finalizers)

	e.createApp(e.newApp("x", "proj-a"))
	err = alice.DeleteApplication(e.ctx, &apiclient.ApplicationDeleteRequest{Name: "x", Cascade: ptrBool(false), PropagationPolicy: "foreground"})
	requireProblem(t, err, 400, "cannot set propagation policy when cascading is disabled")
	err = alice.DeleteApplication(e.ctx, &apiclient.ApplicationDeleteRequest{Name: "x", PropagationPolicy: "orphan"})
	requireProblem(t, err, 400, "invalid propagation policy: orphan")
	err = e.client("reader").DeleteApplication(e.ctx, &apiclient.ApplicationDeleteRequest{Name: "x"})
	requireProblem(t, err, 403, "permission denied: applications, delete, proj-a/x")
	err = e.client("bob").DeleteApplication(e.ctx, &apiclient.ApplicationDeleteRequest{Name: "x"})
	requireProblem(t, err, 403, "permission denied")
	assert.Nil(t, e.getApp(e.ns, "x").DeletionTimestamp)
}

func TestRefreshApplication(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.createApp(e.newApp("guestbook", "proj-a"))

	// A stand-in controller: removes the refresh annotation and records reconciledAt.
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
			}
			a := &v1alpha1.Application{}
			if err := e.kube.Get(e.ctx, client.ObjectKey{Namespace: e.ns, Name: "guestbook"}, a); err != nil {
				continue
			}
			if _, ok := a.Annotations[v1alpha1.AnnotationKeyRefresh]; !ok {
				continue
			}
			now := metav1.Now()
			a.Status.ReconciledAt = &now
			if err := e.kube.Status().Update(e.ctx, a); err != nil {
				continue
			}
			patch := []byte(`{"metadata":{"annotations":{"` + v1alpha1.AnnotationKeyRefresh + `":null}}}`)
			_ = e.kube.Patch(e.ctx, a, client.RawPatch("application/merge-patch+json", patch))
		}
	}()
	before := e.getApp(e.ns, "guestbook")
	got, err := e.client("reader").GetApplication(e.ctx, &apiclient.ApplicationGetRequest{Name: "guestbook", Refresh: "normal"})
	require.NoError(t, err)
	assert.NotContains(t, got.Annotations, v1alpha1.AnnotationKeyRefresh)
	require.NotNil(t, got.Status.ReconciledAt)
	assert.NotEqual(t, before.ResourceVersion, got.ResourceVersion)
	// refresh is audited even though it is a GET.
	audits := e.audit.byOperation("getApplication")
	require.NotEmpty(t, audits)
	assert.Equal(t, "succeeded", string(audits[len(audits)-1].Result))

	got, err = e.client("reader").GetApplication(e.ctx, &apiclient.ApplicationGetRequest{Name: "guestbook", Refresh: "hard"})
	require.NoError(t, err)
	assert.NotContains(t, got.Annotations, v1alpha1.AnnotationKeyRefresh)
	e.repo.mu.Lock()
	require.NotEmpty(t, e.repo.details)
	assert.True(t, e.repo.details[len(e.repo.details)-1].NoCache)
	e.repo.mu.Unlock()
	close(stop)
	<-done
}

func TestRefreshApplicationTimeout(t *testing.T) {
	for _, noEvents := range []bool{false, true} {
		t.Run(fmt.Sprintf("noEvents=%v", noEvents), func(t *testing.T) {
			e := newEnv(t, envOptions{noEvents: noEvents, config: func(_ *testEnv, c *applicationsConfig) { c.RefreshTimeout = 500 * time.Millisecond }})
			h := e.newApp("hyd", "proj-a")
			h.Spec.Source = nil
			h.Spec.SourceHydrator = &v1alpha1.SourceHydrator{
				DrySource:  v1alpha1.DrySource{RepoURL: repoURL, TargetRevision: "main", Path: "dry"},
				SyncSource: v1alpha1.SyncSource{TargetBranch: "env/prod", Path: "prod"},
			}
			e.createApp(h)
			start := time.Now()
			_, err := e.client("alice").GetApplication(e.ctx, &apiclient.ApplicationGetRequest{Name: "hyd", Refresh: "hard"})
			requireProblem(t, err, 504, "application refresh deadline exceeded")
			assert.Less(t, time.Since(start), 5*time.Second)
			stored := e.getApp(e.ns, "hyd")
			assert.Equal(t, "hard", stored.Annotations[v1alpha1.AnnotationKeyRefresh])
			assert.Equal(t, "hard", stored.Annotations[v1alpha1.AnnotationKeyHydrate])
		})
	}
	e := newEnv(t, envOptions{})
	e.createApp(e.newApp("plain", "proj-a"))
	_, err := e.client("bob").GetApplication(e.ctx, &apiclient.ApplicationGetRequest{Name: "plain", Refresh: "normal"})
	requireProblem(t, err, 403, "permission denied")
	assert.NotContains(t, e.getApp(e.ns, "plain").Annotations, v1alpha1.AnnotationKeyRefresh, "a denied refresh must not write")
}

func TestAppsInAnyNamespace(t *testing.T) {
	e := newEnv(t, envOptions{config: func(e *testEnv, c *applicationsConfig) { c.ApplicationNamespaces = []string{e.nsA} },
		policy: defaultPolicy + "p, nsuser, applications, *, proj-a/*-team-a/*, allow\n"})
	e.updateProject("proj-a", func(p *v1alpha1.AppProject) { p.Spec.SourceNamespaces = []string{e.nsA} })
	alice, admin := e.client("alice"), e.client("admin")

	inA := e.newApp("tenant", "proj-a")
	inA.Namespace = e.nsA
	created, err := alice.CreateApplication(e.ctx, &apiclient.ApplicationCreateRequest{Application: inA, Validate: ptrBool(false)})
	require.NoError(t, err)
	assert.Equal(t, e.nsA, created.Namespace)
	audits := e.audit.byOperation("createApplication")
	assert.Equal(t, "proj-a/"+e.nsA+"/tenant", audits[len(audits)-1].Object)

	// Same name in the control-plane namespace is another Application.
	e.createApp(e.newApp("tenant", "proj-a"))
	got, err := alice.GetApplication(e.ctx, &apiclient.ApplicationGetRequest{Name: "tenant", AppNamespace: e.nsA})
	require.NoError(t, err)
	assert.Equal(t, created.UID, got.UID)

	// The namespace-scoped policy grants only Applications in team-a (TEN-069).
	nsUser := e.client("nsuser")
	_, err = nsUser.GetApplication(e.ctx, &apiclient.ApplicationGetRequest{Name: "tenant", AppNamespace: e.nsA})
	require.NoError(t, err)
	_, err = nsUser.GetApplication(e.ctx, &apiclient.ApplicationGetRequest{Name: "tenant"})
	requireProblem(t, err, 403, "permission denied")

	// A namespace outside application.namespaces.
	inB := e.newApp("tenant", "proj-a")
	inB.Namespace = e.nsB
	_, err = admin.CreateApplication(e.ctx, &apiclient.ApplicationCreateRequest{Application: inB, Validate: ptrBool(false)})
	requireProblem(t, err, 403, fmt.Sprintf("namespace '%s' is not permitted", e.nsB))
	e.createApp(inB) // applied with kubectl: invisible through the API
	_, err = admin.GetApplication(e.ctx, &apiclient.ApplicationGetRequest{Name: "tenant", AppNamespace: e.nsB})
	requireProblem(t, err, 403, "permission denied")
	l, err := admin.ListApplications(e.ctx, &apiclient.ApplicationQuery{})
	require.NoError(t, err)
	var nss []string
	for _, a := range l.Items {
		nss = append(nss, a.Namespace)
	}
	slices.Sort(nss)
	assert.Equal(t, []string{e.ns, e.nsA}, nss)

	// An enabled namespace the project does not admit: the vague error.
	inA2 := e.newApp("other", "proj-b")
	inA2.Namespace = e.nsA
	_, err = admin.CreateApplication(e.ctx, &apiclient.ApplicationCreateRequest{Application: inA2, Validate: ptrBool(false)})
	requireProblem(t, err, 400, `app is not allowed in project "proj-b", or the project does not exist`)

	// Query and body namespaces must agree.
	_, err = admin.CreateApplication(e.ctx, &apiclient.ApplicationCreateRequest{Application: inA, AppNamespace: e.ns})
	requireProblem(t, err, 400, "does not match")
}

func TestProjectTokenIsolation(t *testing.T) {
	// The global policy grants the project role everything everywhere.
	e := newEnv(t, envOptions{policy: defaultPolicy + "p, proj:proj-a:ci, applications, *, */*, allow\n"})
	e.updateProject("proj-a", func(p *v1alpha1.AppProject) {
		p.Spec.Roles = []v1alpha1.ProjectRole{{Name: "ci", Policies: []string{"p, proj:proj-a:ci, applications, get, proj-a/*, allow"}}}
	})
	e.createApp(e.newApp("a1", "proj-a"))
	e.createApp(e.newApp("b1", "proj-b"))
	ci := e.projectClient("proj-a", "ci")
	_, err := ci.GetApplication(e.ctx, &apiclient.ApplicationGetRequest{Name: "a1"})
	require.NoError(t, err)
	// A project token never reaches another project, whatever its policy says (TEN-036).
	_, err = ci.GetApplication(e.ctx, &apiclient.ApplicationGetRequest{Name: "b1"})
	requireProblem(t, err, 403, "permission denied")
	l, err := ci.ListApplications(e.ctx, &apiclient.ApplicationQuery{})
	require.NoError(t, err)
	require.Len(t, l.Items, 1)
	assert.Equal(t, "a1", l.Items[0].Name)
	b1 := e.newApp("b2", "proj-b")
	_, err = ci.CreateApplication(e.ctx, &apiclient.ApplicationCreateRequest{Application: b1})
	requireProblem(t, err, 403, "permission denied: applications, create, proj-b/b2, sub: proj:proj-a:ci")
}

// With reads served by the informer cache (production), writes are visible to the next list.
func TestCachedReader(t *testing.T) {
	e := newEnv(t, envOptions{cachedReader: true})
	alice := e.client("alice")
	for i := range 3 {
		app := e.newApp(fmt.Sprintf("c%d", i), "proj-a")
		_, err := alice.CreateApplication(e.ctx, &apiclient.ApplicationCreateRequest{Application: app, Validate: ptrBool(false)})
		require.NoError(t, err)
		l, err := alice.ListApplications(e.ctx, &apiclient.ApplicationQuery{Name: app.Name})
		require.NoError(t, err)
		require.Len(t, l.Items, 1, "created application listed at once")
	}
	spec := e.getApp(e.ns, "c0").Spec
	spec.Source.TargetRevision = "v2"
	_, err := alice.UpdateApplicationSpec(e.ctx, &apiclient.ApplicationUpdateSpecRequest{Name: "c0", Spec: &spec, Validate: ptrBool(false)})
	require.NoError(t, err)
	l, err := alice.ListApplications(e.ctx, &apiclient.ApplicationQuery{Name: "c0"})
	require.NoError(t, err)
	require.Len(t, l.Items, 1)
	assert.Equal(t, "v2", l.Items[0].Spec.Source.TargetRevision)
	_, err = e.client("bob").GetApplication(e.ctx, &apiclient.ApplicationGetRequest{Name: "c0"})
	requireProblem(t, err, 403, "permission denied")
}
