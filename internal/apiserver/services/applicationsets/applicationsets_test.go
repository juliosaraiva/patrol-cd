package applicationsets_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/applicationsets"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/internal/miscapitest"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

func TestCreateGetListDelete(t *testing.T) {
	t.Parallel()
	f := newFixture(t, fixtureOptions{})
	ctx := miscapitest.Ctx(t)
	alice := f.Login("alice")

	created, err := alice.CreateApplicationSet(ctx, &apiclient.ApplicationSetCreateRequest{ApplicationSet: listAppSet("", "guestbook", "team-a", "dev", "prod")})
	require.NoError(t, err)
	require.Equal(t, f.Namespace, created.Namespace, "defaults to the control-plane namespace")
	require.NotEmpty(t, created.UID)
	require.Equal(t, "ApplicationSet", created.Kind)
	require.True(t, f.exists(f.Namespace, "guestbook"))

	got, err := alice.GetApplicationSet(ctx, "guestbook", "")
	require.NoError(t, err)
	require.Equal(t, created.UID, got.UID)
	require.Equal(t, "team-a", got.Spec.Template.Spec.Project)

	list, err := alice.ListApplicationSets(ctx, &apiclient.ApplicationSetListRequest{})
	require.NoError(t, err)
	require.Equal(t, []string{f.Namespace + "/guestbook"}, setNames(list.Items))
	require.NotEmpty(t, list.ResourceVersion)

	// Audit entry and Kubernetes Event of the creation.
	entries := f.Audit.ByOperation("createApplicationSet")
	require.Len(t, entries, 1)
	e := entries[0]
	require.Equal(t, rest.AuditSucceeded, e.Result)
	require.Equal(t, "alice", e.Actor)
	require.Equal(t, []string{"applicationsets", "create", "team-a/guestbook"}, []string{e.Resource, e.Action, e.Object})
	require.NotNil(t, e.Target)
	require.Equal(t, "ApplicationSet", e.Target.Kind)
	require.Contains(t, f.Recorder.All(), "Normal ResourceCreated "+f.Namespace+"/guestbook alice created ApplicationSet")

	require.NoError(t, alice.DeleteApplicationSet(ctx, &apiclient.ApplicationSetDeleteRequest{Name: "guestbook"}))
	require.False(t, f.exists(f.Namespace, "guestbook"))
	require.Contains(t, f.Recorder.All(), "Normal ResourceDeleted "+f.Namespace+"/guestbook alice deleted ApplicationSets")
	del := f.Audit.ByOperation("deleteApplicationSet")
	require.Len(t, del, 1)
	require.Equal(t, "team-a/guestbook", del[0].Object)
}

func TestCreateValidation(t *testing.T) {
	t.Parallel()
	f := newFixture(t, fixtureOptions{})
	ctx := miscapitest.Ctx(t)
	admin := f.Login("admin")

	templated := listAppSet("", "templated", "{{project}}", "a")
	genTemplated := listAppSet("", "gen-templated", "team-a", "a")
	genTemplated.Spec.Generators[0].List.Template.Spec.Project = "{{.p}}"
	unknownGen := listAppSet("", "unknown", "team-a", "a")
	unknownGen.Spec.Generators = append(unknownGen.Spec.Generators, v1alpha1.ApplicationSetGenerator{})
	noName := listAppSet("", "", "team-a", "a")

	cases := []struct {
		name   string
		as     *v1alpha1.ApplicationSet
		status int
		detail string
	}{
		{"templated project", templated, http.StatusBadRequest, "error validating ApplicationSets: the API does not currently support creating ApplicationSets with templated `project` fields"},
		{"templated generator project", genTemplated, http.StatusBadRequest, "templated `project` fields"},
		{"unrecognized generator", unknownGen, http.StatusBadRequest, "error validating ApplicationSets: ApplicationSet unknown contains unrecognized generators"},
		{"missing project", listAppSet("", "orphan", "nope", "a"), http.StatusBadRequest, "ApplicationSet references project nope which does not exist"},
		{"namespace not enabled", listAppSet(f.disabled, "x", "team-a", "a"), http.StatusForbidden, "namespace '" + f.disabled + "' is not permitted"},
		{"project does not trust the namespace", listAppSet(f.tenant, "x", "team-b", "a"), http.StatusForbidden, "ApplicationSet namespace '" + f.tenant + "' is not permitted in project 'team-b'"},
		{"no name", noName, http.StatusBadRequest, "metadata.name is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := admin.CreateApplicationSet(ctx, &apiclient.ApplicationSetCreateRequest{ApplicationSet: tc.as})
			ae := apiErr(t, err)
			require.Equal(t, tc.status, ae.StatusCode)
			require.Contains(t, ae.Problem.Detail, tc.detail)
			if tc.as.Name != "" {
				require.False(t, f.exists(f.nsOr(tc.as.Namespace), tc.as.Name))
			}
		})
	}

	// A null body.
	_, err := admin.DoRaw(ctx, http.MethodPost, apiclient.PathApplicationSets, nil, strings.NewReader("null"), "application/json")
	ae := apiErr(t, err)
	require.Equal(t, http.StatusBadRequest, ae.StatusCode)
	require.Contains(t, ae.Problem.Detail, "ApplicationSets is nil in request")
}

func (f *fixture) nsOr(ns string) string {
	if ns == "" {
		return f.Namespace
	}
	return ns
}

func TestCreateAuthorization(t *testing.T) {
	t.Parallel()
	f := newFixture(t, fixtureOptions{})
	ctx := miscapitest.Ctx(t)

	escalation := listAppSet("", "escalate", "team-a", "a")
	escalation.Spec.Generators[0].List.Template.Spec.Project = "team-b"

	cases := []struct {
		name   string
		client *apiclient.Client
		as     *v1alpha1.ApplicationSet
		ok     bool
		status int
	}{
		{"own project", f.Login("alice"), listAppSet("", "a1", "team-a", "x"), true, 0},
		{"other project", f.Login("alice"), listAppSet("", "a2", "team-b", "x"), false, http.StatusForbidden},
		{"generator template escalates to another project", f.Login("alice"), escalation, false, http.StatusForbidden},
		{"empty project is default", f.Login("alice"), listAppSet("", "a3", "", "x"), false, http.StatusForbidden},
		{"no role", f.Login("nobody"), listAppSet("", "a4", "team-a", "x"), false, http.StatusForbidden},
		{"read-only", f.Login("reader"), listAppSet("", "a5", "team-a", "x"), false, http.StatusForbidden},
		{"project role token", f.ProjectClient("team-a", "ci"), listAppSet("", "a6", "team-a", "x"), true, 0},
		{"project role token, other project", f.ProjectClient("team-a", "ci"), listAppSet("", "a7", "team-b", "x"), false, http.StatusForbidden},
		{"tenant user in its namespace", f.Login("tenantuser"), listAppSet(f.tenant, "a8", "team-a", "x"), true, 0},
		{"tenant user in the control plane", f.Login("tenantuser"), listAppSet("", "a9", "team-a", "x"), false, http.StatusForbidden},
		{"anonymous", f.Anonymous(), listAppSet("", "a10", "team-a", "x"), false, http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.client.CreateApplicationSet(ctx, &apiclient.ApplicationSetCreateRequest{ApplicationSet: tc.as})
			if tc.ok {
				require.NoError(t, err)
				require.True(t, f.exists(f.nsOr(tc.as.Namespace), tc.as.Name))
				return
			}
			require.Equal(t, tc.status, apiErr(t, err).StatusCode)
			require.False(t, f.exists(f.nsOr(tc.as.Namespace), tc.as.Name))
		})
	}
	denials := 0
	for _, e := range f.Audit.ByOperation("createApplicationSet") {
		if e.Result == rest.AuditDenied {
			denials++
		}
	}
	require.GreaterOrEqual(t, denials, 6, "every RBAC denial is audited")
}

func TestUpsertAndIdempotentCreate(t *testing.T) {
	t.Parallel()
	f := newFixture(t, fixtureOptions{})
	ctx := miscapitest.Ctx(t)
	alice := f.Login("alice")
	first, err := alice.CreateApplicationSet(ctx, &apiclient.ApplicationSetCreateRequest{ApplicationSet: listAppSet("", "web", "team-a", "a")})
	require.NoError(t, err)

	// Identical spec: the existing object, unchanged.
	again, err := alice.CreateApplicationSet(ctx, &apiclient.ApplicationSetCreateRequest{ApplicationSet: listAppSet("", "web", "team-a", "a")})
	require.NoError(t, err)
	require.Equal(t, first.ResourceVersion, again.ResourceVersion)

	changed := listAppSet("", "web", "team-a", "a", "b")
	changed.Labels = map[string]string{"extra": "yes"}
	_, err = alice.CreateApplicationSet(ctx, &apiclient.ApplicationSetCreateRequest{ApplicationSet: changed})
	ae := apiErr(t, err)
	require.Equal(t, http.StatusBadRequest, ae.StatusCode)
	require.Equal(t, "existing ApplicationSet spec is different, use upsert flag to force update", ae.Problem.Detail)

	// create without update permission.
	_, err = f.Login("creator").CreateApplicationSet(ctx, &apiclient.ApplicationSetCreateRequest{ApplicationSet: changed, Upsert: true})
	require.Equal(t, http.StatusForbidden, apiErr(t, err).StatusCode)

	updated, err := alice.CreateApplicationSet(ctx, &apiclient.ApplicationSetCreateRequest{ApplicationSet: changed, Upsert: true})
	require.NoError(t, err)
	require.Len(t, updated.Spec.Generators[0].List.Elements, 2)
	require.Equal(t, map[string]string{"team": "team-a", "extra": "yes"}, updated.Labels, "labels are merged")
	require.Contains(t, f.Recorder.All(), "Normal ResourceUpdated "+f.Namespace+"/web alice updated ApplicationSets spec")

	// Conditional upsert with a stale resourceVersion: 409.
	stale := listAppSet("", "web", "team-a", "c")
	stale.ResourceVersion = first.ResourceVersion
	_, err = alice.CreateApplicationSet(ctx, &apiclient.ApplicationSetCreateRequest{ApplicationSet: stale, Upsert: true})
	ae = apiErr(t, err)
	require.Equal(t, http.StatusConflict, ae.StatusCode)
	require.Equal(t, apiclient.CodeConflict, ae.Code())
	// With the current resourceVersion it applies.
	fresh := listAppSet("", "web", "team-a", "c")
	fresh.ResourceVersion = updated.ResourceVersion
	_, err = alice.CreateApplicationSet(ctx, &apiclient.ApplicationSetCreateRequest{ApplicationSet: fresh, Upsert: true})
	require.NoError(t, err)

	// The controller's finalizer survives an upsert that omits it.
	cur := &v1alpha1.ApplicationSet{}
	require.NoError(t, f.Kube.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: "web"}, cur))
	cur.Finalizers = []string{v1alpha1.FinalizerApplicationSet}
	require.NoError(t, f.Kube.Update(ctx, cur))
	_, err = alice.CreateApplicationSet(ctx, &apiclient.ApplicationSetCreateRequest{ApplicationSet: listAppSet("", "web", "team-a", "d"), Upsert: true})
	require.NoError(t, err)
	require.NoError(t, f.Kube.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: "web"}, cur))
	require.Equal(t, []string{v1alpha1.FinalizerApplicationSet}, cur.Finalizers)
	cur.Finalizers = nil
	require.NoError(t, f.Kube.Update(ctx, cur))
}

func TestUpsertProjectChange(t *testing.T) {
	t.Parallel()
	f := newFixture(t, fixtureOptions{})
	ctx := miscapitest.Ctx(t)
	_, err := f.Login("admin").CreateApplicationSet(ctx, &apiclient.ApplicationSetCreateRequest{ApplicationSet: listAppSet("", "move", "team-a", "a")})
	require.NoError(t, err)
	toB := listAppSet("", "move", "team-b", "a")

	// mover2 may create and update in team-b but not update in team-a.
	_, err = f.Login("mover2").CreateApplicationSet(ctx, &apiclient.ApplicationSetCreateRequest{ApplicationSet: toB, Upsert: true})
	require.Equal(t, http.StatusForbidden, apiErr(t, err).StatusCode)
	cur := &v1alpha1.ApplicationSet{}
	require.NoError(t, f.Kube.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: "move"}, cur))
	require.Equal(t, "team-a", cur.Spec.Template.Spec.Project)

	// mover may update in both.
	moved, err := f.Login("mover").CreateApplicationSet(ctx, &apiclient.ApplicationSetCreateRequest{ApplicationSet: toB, Upsert: true})
	require.NoError(t, err)
	require.Equal(t, "team-b", moved.Spec.Template.Spec.Project)
}

func TestDryRunAndGenerate(t *testing.T) {
	t.Parallel()
	f := newFixture(t, fixtureOptions{})
	ctx := miscapitest.Ctx(t)
	alice := f.Login("alice")

	dry, err := alice.CreateApplicationSet(ctx, &apiclient.ApplicationSetCreateRequest{ApplicationSet: listAppSet("", "preview", "team-a", "prod", "dev"), DryRun: true})
	require.NoError(t, err)
	require.False(t, f.exists(f.Namespace, "preview"), "dry run persists nothing")
	require.Len(t, dry.Status.Resources, 2)
	require.Equal(t, "dev-preview", dry.Status.Resources[0].Name)
	require.Equal(t, "prod-preview", dry.Status.Resources[1].Name)
	require.Equal(t, "Application", dry.Status.Resources[0].Kind)
	require.Equal(t, int64(2), dry.Status.ResourcesCount)

	gen, err := alice.GenerateApplicationSet(ctx, listAppSet(f.tenant, "tenant-set", "team-a", "x", "y"))
	require.NoError(t, err)
	require.Equal(t, []string{"x-tenant-set", "y-tenant-set"}, appNames(gen.Applications))
	for _, a := range gen.Applications {
		require.Equal(t, f.tenant, a.Namespace, "generated Applications live in the ApplicationSet namespace")
		require.Equal(t, "team-a", a.Spec.Project)
		require.Equal(t, "Application", a.Kind)
	}
	require.False(t, f.exists(f.tenant, "tenant-set"))

	// Generate needs create on the project.
	_, err = f.Login("bob").GenerateApplicationSet(ctx, listAppSet("", "b", "team-b", "x"))
	require.Equal(t, http.StatusForbidden, apiErr(t, err).StatusCode)
	_, err = f.Login("reader").CreateApplicationSet(ctx, &apiclient.ApplicationSetCreateRequest{ApplicationSet: listAppSet("", "r", "team-a", "x"), DryRun: true})
	require.Equal(t, http.StatusForbidden, apiErr(t, err).StatusCode)
	_, err = alice.GenerateApplicationSet(ctx, listAppSet("", "t", "{{p}}", "x"))
	require.Contains(t, apiErr(t, err).Problem.Detail, "templated `project` fields")
	_, err = alice.GenerateApplicationSet(ctx, nil)
	require.Contains(t, apiErr(t, err).Problem.Detail, "ApplicationSets is nil in request")
	_, err = alice.GenerateApplicationSet(ctx, listAppSet(f.disabled, "t", "team-a", "x"))
	require.Contains(t, apiErr(t, err).Problem.Detail, "is not permitted")

	// Generator failures carry the generator error (400).
	broken := listAppSet("", "broken", "team-a", "x")
	broken.Spec.GoTemplate = true
	broken.Spec.GoTemplateOptions = []string{"missingkey=error"}
	broken.Spec.Template.Name = "{{ .nope }}"
	_, err = alice.GenerateApplicationSet(ctx, broken)
	ae := apiErr(t, err)
	require.Equal(t, http.StatusBadRequest, ae.StatusCode)
	require.True(t, strings.HasPrefix(ae.Problem.Detail, "unable to generate Applications of ApplicationSet"), ae.Problem.Detail)
}

// slowGenerator blocks until the context ends.
type slowGenerator struct{}

func (slowGenerator) Generate(ctx context.Context, _ *v1alpha1.ApplicationSet, _ applicationsets.GenerateLimits) ([]v1alpha1.Application, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// manyGenerator returns n Applications.
type manyGenerator struct{ n int }

func (g manyGenerator) Generate(context.Context, *v1alpha1.ApplicationSet, applicationsets.GenerateLimits) ([]v1alpha1.Application, error) {
	out := make([]v1alpha1.Application, g.n)
	for i := range out {
		out[i].Name = "app-" + strings.Repeat("x", i+1)
	}
	return out, nil
}

func TestGenerateBounds(t *testing.T) {
	t.Parallel()
	slow := newFixture(t, fixtureOptions{generator: slowGenerator{}, config: func(c *applicationsets.Config) { c.GenerateTimeout = 200 * time.Millisecond }})
	ctx := miscapitest.Ctx(t)
	_, err := slow.Login("alice").GenerateApplicationSet(ctx, listAppSet("", "slow", "team-a", "x"))
	ae := apiErr(t, err)
	require.Equal(t, http.StatusGatewayTimeout, ae.StatusCode)
	require.Contains(t, ae.Problem.Detail, "timed out")

	many := newFixture(t, fixtureOptions{generator: manyGenerator{n: 5}, config: func(c *applicationsets.Config) { c.MaxGeneratedApplications = 4 }})
	_, err = many.Login("alice").GenerateApplicationSet(ctx, listAppSet("", "many", "team-a", "x"))
	ae = apiErr(t, err)
	require.Equal(t, http.StatusBadRequest, ae.StatusCode)
	require.Contains(t, ae.Problem.Detail, "generates 5 Applications, more than the limit of 4")

	big := newFixture(t, fixtureOptions{generator: manyGenerator{n: 50}, config: func(c *applicationsets.Config) { c.MaxGenerateResponseBytes = 1024 }})
	_, err = big.Login("alice").GenerateApplicationSet(ctx, listAppSet("", "big", "team-a", "x"))
	require.Contains(t, apiErr(t, err).Problem.Detail, "exceed the response limit of 1024 bytes")
}

func TestGetNoExistenceOracle(t *testing.T) {
	t.Parallel()
	f := newFixture(t, fixtureOptions{})
	ctx := miscapitest.Ctx(t)
	f.seed(listAppSet(f.Namespace, "secret-a", "team-a", "x"))
	f.seed(listAppSet(f.tenant, "tenant-a", "team-a", "x"))

	bob := f.Login("bob")
	_, errForbidden := bob.GetApplicationSet(ctx, "secret-a", "")
	_, errMissing := bob.GetApplicationSet(ctx, "does-not-exist", "")
	a, b := apiErr(t, errForbidden), apiErr(t, errMissing)
	require.Equal(t, http.StatusForbidden, a.StatusCode)
	require.Equal(t, a.StatusCode, b.StatusCode)
	require.Equal(t, a.Problem.Detail, b.Problem.Detail, "forbidden and missing are indistinguishable")
	require.Equal(t, "permission denied", a.Problem.Detail)

	// The same holds for the tree, events and delete routes.
	for _, call := range []func(name string) error{
		func(n string) error { _, err := bob.GetApplicationSetResourceTree(ctx, n, ""); return err },
		func(n string) error { _, err := bob.ListApplicationSetEvents(ctx, n, ""); return err },
		func(n string) error {
			return bob.DeleteApplicationSet(ctx, &apiclient.ApplicationSetDeleteRequest{Name: n})
		},
	} {
		x, y := apiErr(t, call("secret-a")), apiErr(t, call("does-not-exist"))
		require.Equal(t, x.StatusCode, y.StatusCode)
		require.Equal(t, x.Problem.Detail, y.Problem.Detail)
	}
	require.True(t, f.exists(f.Namespace, "secret-a"))

	// Allowed reader, namespaced object, and a disabled namespace.
	got, err := f.Login("tenantuser").GetApplicationSet(ctx, "tenant-a", f.tenant)
	require.NoError(t, err)
	require.Equal(t, f.tenant, got.Namespace)
	_, err = f.Login("tenantuser").GetApplicationSet(ctx, "secret-a", "")
	require.Equal(t, http.StatusForbidden, apiErr(t, err).StatusCode, "tenant isolation")
	_, err = f.Login("admin").GetApplicationSet(ctx, "x", f.disabled)
	ae := apiErr(t, err)
	require.Equal(t, http.StatusForbidden, ae.StatusCode)
	require.Equal(t, "namespace '"+f.disabled+"' is not permitted", ae.Problem.Detail)
	_, err = f.Anonymous().GetApplicationSet(ctx, "secret-a", "")
	require.True(t, apiclient.IsUnauthenticated(err))
}

func TestListFiltering(t *testing.T) {
	t.Parallel()
	f := newFixture(t, fixtureOptions{})
	ctx := miscapitest.Ctx(t)
	f.seed(listAppSet(f.Namespace, "c-a", "team-a", "x"))
	f.seed(listAppSet(f.Namespace, "b-b", "team-b", "x"))
	f.seed(listAppSet(f.tenant, "a-a", "team-a", "x"))
	f.seed(listAppSet(f.disabled, "z-hidden", "team-a", "x"))
	noProj := listAppSet(f.Namespace, "d-default", "", "x")
	noProj.Labels = map[string]string{"team": "none"}
	f.seed(noProj)

	cases := []struct {
		name   string
		client *apiclient.Client
		req    apiclient.ApplicationSetListRequest
		want   []string
	}{
		{"reader sees enabled namespaces", f.Login("reader"), apiclient.ApplicationSetListRequest{},
			[]string{f.tenant + "/a-a", f.Namespace + "/b-b", f.Namespace + "/c-a", f.Namespace + "/d-default"}},
		{"alice sees team-a", f.Login("alice"), apiclient.ApplicationSetListRequest{}, []string{f.tenant + "/a-a", f.Namespace + "/c-a"}},
		{"bob sees team-b", f.Login("bob"), apiclient.ApplicationSetListRequest{}, []string{f.Namespace + "/b-b"}},
		{"tenant user sees its namespace", f.Login("tenantuser"), apiclient.ApplicationSetListRequest{}, []string{f.tenant + "/a-a"}},
		{"nobody sees nothing", f.Login("nobody"), apiclient.ApplicationSetListRequest{}, []string{}},
		{"project filter", f.Login("reader"), apiclient.ApplicationSetListRequest{Projects: []string{"team-b", "default"}}, []string{f.Namespace + "/b-b", f.Namespace + "/d-default"}},
		{"namespace filter", f.Login("reader"), apiclient.ApplicationSetListRequest{AppsetNamespace: f.tenant}, []string{f.tenant + "/a-a"}},
		{"disabled namespace filter", f.Login("admin"), apiclient.ApplicationSetListRequest{AppsetNamespace: f.disabled}, []string{}},
		{"selector", f.Login("reader"), apiclient.ApplicationSetListRequest{Selector: "team in (team-b, none)"}, []string{f.Namespace + "/b-b", f.Namespace + "/d-default"}},
		{"sso group", f.ClientWithToken(f.SSOToken("u1", "u1@example.com", "sso-devs")), apiclient.ApplicationSetListRequest{AppsetNamespace: f.tenant}, []string{f.tenant + "/a-a"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			list, err := tc.client.ListApplicationSets(ctx, &tc.req)
			require.NoError(t, err)
			require.Equal(t, tc.want, setNames(list.Items))
		})
	}

	_, err := f.Login("reader").ListApplicationSets(ctx, &apiclient.ApplicationSetListRequest{Selector: "team in ("})
	ae := apiErr(t, err)
	require.Equal(t, http.StatusBadRequest, ae.StatusCode)
	require.Contains(t, ae.Problem.Detail, "error parsing the selector")

	// Pagination.
	var all []string
	req := &apiclient.ApplicationSetListRequest{Limit: 3}
	for range 5 {
		page, err := f.Login("reader").ListApplicationSets(ctx, req)
		require.NoError(t, err)
		require.LessOrEqual(t, len(page.Items), 3)
		all = append(all, setNames(page.Items)...)
		if page.Continue == "" {
			break
		}
		req.Continue = page.Continue
	}
	require.Equal(t, []string{f.tenant + "/a-a", f.Namespace + "/b-b", f.Namespace + "/c-a", f.Namespace + "/d-default"}, all)
	_, err = f.Login("reader").ListApplicationSets(ctx, &apiclient.ApplicationSetListRequest{Continue: "!!"})
	require.Equal(t, http.StatusBadRequest, apiErr(t, err).StatusCode)
	_, err = f.Login("reader").ListApplicationSets(ctx, &apiclient.ApplicationSetListRequest{Limit: -1})
	require.Equal(t, http.StatusBadRequest, apiErr(t, err).StatusCode)
}

func TestDeletePropagation(t *testing.T) {
	t.Parallel()
	f := newFixture(t, fixtureOptions{})
	ctx := miscapitest.Ctx(t)
	admin := f.Login("admin")
	for _, n := range []string{"bg", "fg", "orphan"} {
		f.seed(listAppSet(f.Namespace, n, "team-b", "x"))
	}
	// envtest runs no garbage collector: foreground and orphan deletions leave the object
	// terminating with the propagation finalizer, which shows the policy that was sent.
	require.NoError(t, admin.DeleteApplicationSet(ctx, &apiclient.ApplicationSetDeleteRequest{Name: "bg"}))
	require.False(t, f.exists(f.Namespace, "bg"))
	require.NoError(t, admin.DeleteApplicationSet(ctx, &apiclient.ApplicationSetDeleteRequest{Name: "fg", PropagationPolicy: "foreground"}))
	require.NoError(t, admin.DeleteApplicationSet(ctx, &apiclient.ApplicationSetDeleteRequest{Name: "orphan", Cascade: ptr.To(false)}))
	for name, fin := range map[string]string{"fg": metav1.FinalizerDeleteDependents, "orphan": metav1.FinalizerOrphanDependents} {
		as := &v1alpha1.ApplicationSet{}
		require.NoError(t, f.Kube.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: name}, as))
		require.NotNil(t, as.DeletionTimestamp, name)
		require.Contains(t, as.Finalizers, fin, name)
		as.Finalizers = nil
		require.NoError(t, f.Kube.Update(ctx, as))
	}

	f.seed(listAppSet(f.Namespace, "keep", "team-b", "x"))
	for _, req := range []apiclient.ApplicationSetDeleteRequest{
		{Name: "keep", PropagationPolicy: "sideways"},
		{Name: "keep", Cascade: ptr.To(false), PropagationPolicy: "foreground"},
	} {
		err := admin.DeleteApplicationSet(ctx, &req)
		require.Equal(t, http.StatusBadRequest, apiErr(t, err).StatusCode)
	}
	// bob may delete team-b; alice may not; reader may not.
	require.Equal(t, http.StatusForbidden, apiErr(t, f.Login("alice").DeleteApplicationSet(ctx, &apiclient.ApplicationSetDeleteRequest{Name: "keep"})).StatusCode)
	require.Equal(t, http.StatusForbidden, apiErr(t, f.Login("reader").DeleteApplicationSet(ctx, &apiclient.ApplicationSetDeleteRequest{Name: "keep"})).StatusCode)
	require.True(t, f.exists(f.Namespace, "keep"))
	require.NoError(t, f.Login("bob").DeleteApplicationSet(ctx, &apiclient.ApplicationSetDeleteRequest{Name: "keep"}))
	require.False(t, f.exists(f.Namespace, "keep"))
}

func TestResourceTreeAndEvents(t *testing.T) {
	t.Parallel()
	f := newFixture(t, fixtureOptions{})
	ctx := miscapitest.Ctx(t)
	as := f.seed(listAppSet(f.Namespace, "tree", "team-a", "x"))
	as.Status.Resources = []v1alpha1.ResourceStatus{
		{Group: "cd.patrol.example", Version: "v1alpha1", Kind: "Application", Name: "z-app", Status: v1alpha1.SyncStatusCodeOutOfSync, Health: &v1alpha1.HealthStatus{Status: v1alpha1.HealthStatusDegraded}},
		{Name: "a-app", Status: v1alpha1.SyncStatusCodeSynced, Health: &v1alpha1.HealthStatus{Status: v1alpha1.HealthStatusHealthy}},
	}
	require.NoError(t, f.Kube.Status().Update(ctx, as))

	tree, err := f.Login("alice").GetApplicationSetResourceTree(ctx, "tree", "")
	require.NoError(t, err)
	require.Len(t, tree.Nodes, 2)
	n0, n1 := tree.Nodes[0], tree.Nodes[1]
	require.Equal(t, "a-app", n0.Name)
	require.Equal(t, "z-app", n1.Name)
	require.Equal(t, "Application", n0.Kind)
	require.Equal(t, f.Namespace, n0.Namespace)
	require.Equal(t, v1alpha1.HealthStatusHealthy, n0.Health.Status)
	require.Equal(t, []apiclient.ApplicationSetTreeInfo{{Name: "Sync Status", Value: "Synced"}}, n0.Info)
	require.Equal(t, v1alpha1.HealthStatusDegraded, n1.Health.Status)
	require.Equal(t, []apiclient.ApplicationSetTreeRef{{Group: "cd.patrol.example", Version: "v1alpha1", Kind: "ApplicationSet", Namespace: f.Namespace, Name: "tree", UID: string(as.UID)}}, n0.ParentRefs)

	// Events: same name+namespace+uid only (not a deleted-and-recreated namesake).
	mk := func(name string, uid types.UID, reason string) {
		ev := &corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: name, Namespace: f.Namespace},
			InvolvedObject: corev1.ObjectReference{APIVersion: "cd.patrol.example/v1alpha1", Kind: "ApplicationSet", Name: "tree", Namespace: f.Namespace, UID: uid},
			Reason:         reason, Message: reason, Type: corev1.EventTypeNormal,
			FirstTimestamp: metav1.Now(), LastTimestamp: metav1.Now(),
		}
		require.NoError(t, f.Kube.Create(ctx, ev))
	}
	mk("tree.1", as.UID, "created")
	mk("tree.2", "00000000-0000-0000-0000-000000000000", "stale")
	evs, err := f.Login("alice").ListApplicationSetEvents(ctx, "tree", "")
	require.NoError(t, err)
	require.Len(t, evs.Items, 1)
	require.Equal(t, "created", evs.Items[0].Reason)

	_, err = f.Login("bob").ListApplicationSetEvents(ctx, "tree", "")
	require.Equal(t, http.StatusForbidden, apiErr(t, err).StatusCode)
	_, err = f.Login("bob").GetApplicationSetResourceTree(ctx, "tree", "")
	require.Equal(t, http.StatusForbidden, apiErr(t, err).StatusCode)
}

func TestWatch(t *testing.T) {
	t.Parallel()
	f := newFixture(t, fixtureOptions{})
	ctx := miscapitest.Ctx(t)
	f.seed(listAppSet(f.Namespace, "watched", "team-a", "x"))

	// By name: the current state first, then changes.
	s, err := f.Login("alice").WatchApplicationSets(ctx, &apiclient.ApplicationSetWatchRequest{Name: "watched"}, nil)
	require.NoError(t, err)
	events := make(chan apiclient.WatchEvent[v1alpha1.ApplicationSet], 16)
	go func() {
		_ = apiclient.Watch(s, func(ev apiclient.WatchEvent[v1alpha1.ApplicationSet]) error {
			events <- ev
			return nil
		})
		close(events)
	}()
	next := func() apiclient.WatchEvent[v1alpha1.ApplicationSet] {
		t.Helper()
		select {
		case ev, ok := <-events:
			require.True(t, ok, "stream ended")
			return ev
		case <-time.After(20 * time.Second):
			t.Fatal("no event")
		}
		return apiclient.WatchEvent[v1alpha1.ApplicationSet]{}
	}
	ev := next()
	require.Equal(t, apiclient.EventAdded, ev.Type)
	require.Equal(t, "watched", ev.Object.Name)

	// An unrelated ApplicationSet does not appear on a by-name stream.
	f.seed(listAppSet(f.Namespace, "other", "team-a", "x"))
	cur := &v1alpha1.ApplicationSet{}
	require.NoError(t, f.Kube.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: "watched"}, cur))
	cur.Status.ResourcesCount = 7
	require.NoError(t, f.Kube.Status().Update(ctx, cur))
	ev = next()
	require.Equal(t, apiclient.EventModified, ev.Type)
	require.Equal(t, int64(7), ev.Object.Status.ResourcesCount)
	require.NoError(t, f.Kube.Delete(ctx, cur))
	ev = next()
	require.Equal(t, apiclient.EventDeleted, ev.Type)
	require.Equal(t, "watched", ev.Object.Name)
}

func TestWatchPerEventAuthorization(t *testing.T) {
	t.Parallel()
	f := newFixture(t, fixtureOptions{})
	ctx := miscapitest.Ctx(t)

	// List, then watch from the list's resourceVersion: no change is missed.
	list, err := f.Login("bob").ListApplicationSets(ctx, &apiclient.ApplicationSetListRequest{})
	require.NoError(t, err)
	s, err := f.Login("bob").WatchApplicationSets(ctx, &apiclient.ApplicationSetWatchRequest{ResourceVersion: list.ResourceVersion}, nil)
	require.NoError(t, err)
	defer func() { _ = s.Close() }()
	// Events for team-a (forbidden to bob), a disabled namespace, a team-b ApplicationSet that
	// also generates team-a Applications (forbidden), then team-b (allowed).
	f.seed(listAppSet(f.Namespace, "forbidden", "team-a", "x"))
	f.seed(listAppSet(f.disabled, "hidden", "team-b", "x"))
	shared := listAppSet(f.Namespace, "shared", "team-b", "x")
	shared.Spec.Generators[0].List.Template.Spec.Project = "team-a"
	f.seed(shared)
	f.seed(listAppSet(f.Namespace, "allowed", "team-b", "x"))
	got := make(chan apiclient.ApplicationSetWatchEvent, 1)
	go func() {
		for {
			e, err := s.Next()
			if err != nil {
				close(got)
				return
			}
			var we apiclient.ApplicationSetWatchEvent
			if e.Decode(&we) == nil {
				got <- we
				return
			}
		}
	}()
	select {
	case we, ok := <-got:
		require.True(t, ok)
		require.Equal(t, "allowed", we.Object.Name, "forbidden, hidden and shared events are skipped")
		require.Equal(t, apiclient.EventAdded, we.Type)
	case <-time.After(20 * time.Second):
		t.Fatal("no event")
	}

	// Invalid selector: 400 before the stream starts. Anonymous: 401.
	_, err = f.Login("bob").WatchApplicationSets(ctx, &apiclient.ApplicationSetWatchRequest{Selector: "a in ("}, nil)
	ae := apiErr(t, err)
	require.Equal(t, http.StatusBadRequest, ae.StatusCode)
	require.Contains(t, ae.Problem.Detail, "error parsing labels with selectors")
	_, err = f.Anonymous().WatchApplicationSets(ctx, &apiclient.ApplicationSetWatchRequest{}, nil)
	require.True(t, apiclient.IsUnauthenticated(err))
}

func TestWatchRevokedPermissionStopsEvents(t *testing.T) {
	t.Parallel()
	f := newFixture(t, fixtureOptions{})
	ctx := miscapitest.Ctx(t)
	list, err := f.Login("alice").ListApplicationSets(ctx, &apiclient.ApplicationSetListRequest{})
	require.NoError(t, err)
	s, err := f.Login("alice").WatchApplicationSets(ctx, &apiclient.ApplicationSetWatchRequest{ResourceVersion: list.ResourceVersion}, nil)
	require.NoError(t, err)
	defer func() { _ = s.Close() }()
	got := make(chan string, 8)
	go func() {
		for {
			e, err := s.Next()
			if err != nil {
				return
			}
			var we apiclient.ApplicationSetWatchEvent
			if e.Decode(&we) == nil {
				got <- we.Object.Name
			}
		}
	}()
	f.seed(listAppSet(f.Namespace, "before", "team-a", "x"))
	select {
	case n := <-got:
		require.Equal(t, "before", n)
	case <-time.After(20 * time.Second):
		t.Fatal("no event")
	}
	// Revoke alice's permission: later events are no longer delivered; a sentinel for a
	// user still allowed (via a second stream) proves the event was produced.
	f.SetRBAC(miscapitest.Policy("g, admin, role:admin"))
	ss, err := f.Login("admin").WatchApplicationSets(ctx, &apiclient.ApplicationSetWatchRequest{Name: "after"}, nil)
	require.NoError(t, err)
	defer func() { _ = ss.Close() }()
	f.seed(listAppSet(f.Namespace, "after", "team-a", "x"))
	ev, err := ss.Next()
	require.NoError(t, err)
	var we apiclient.ApplicationSetWatchEvent
	require.NoError(t, ev.Decode(&we))
	require.Equal(t, "after", we.Object.Name)
	select {
	case n := <-got:
		t.Fatalf("revoked caller received %q", n)
	case <-time.After(500 * time.Millisecond):
	}
}

func TestUnavailableWithoutDependencies(t *testing.T) {
	t.Parallel()
	env := miscapitest.New(t, miscapitest.Options{CM: miscapitest.Accounts("admin"), RBAC: miscapitest.Policy("g, admin, role:admin")},
		func(env *miscapitest.Env) []miscapitest.Registrar {
			return []miscapitest.Registrar{applicationsets.New(applicationsets.Deps{Config: applicationsets.Config{Namespace: env.Namespace}})}
		})
	ctx := context.Background()
	c := env.Login("admin")
	_, err := c.GetApplicationSet(ctx, "x", "")
	require.True(t, apiclient.IsUnavailable(err), "%v", err)
	_, err = c.ListApplicationSets(ctx, &apiclient.ApplicationSetListRequest{})
	require.True(t, apiclient.IsUnavailable(err), "%v", err)
	_, err = c.WatchApplicationSets(ctx, &apiclient.ApplicationSetWatchRequest{}, nil)
	require.True(t, apiclient.IsUnavailable(err), "%v", err)
	_, err = c.GenerateApplicationSet(ctx, listAppSet("", "x", "default", "a"))
	require.True(t, apiclient.IsUnavailable(err) || errors.Is(err, context.Canceled), "%v", err)
}
