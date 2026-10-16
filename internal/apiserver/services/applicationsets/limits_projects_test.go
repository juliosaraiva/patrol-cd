package applicationsets_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/applicationsets"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/internal/miscapitest"
	"github.com/juliosaraiva/patrol-cd/internal/appset"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

func listElements(t *testing.T, key string, n int) []apiextensionsv1.JSON {
	t.Helper()
	out := make([]apiextensionsv1.JSON, 0, n)
	for i := range n {
		b, err := json.Marshal(map[string]string{key: fmt.Sprintf("%s%d", key, i)})
		require.NoError(t, err)
		out = append(out, apiextensionsv1.JSON{Raw: b})
	}
	return out
}

// fanOut returns an ApplicationSet in project team-a with k generator entries, each a Matrix of
// two List generators of n elements (k*n*n Applications).
func fanOut(t *testing.T, name string, k, n int) *v1alpha1.ApplicationSet {
	t.Helper()
	as := listAppSet("", name, "team-a")
	as.Spec.Template.Name = "{{a}}-{{b}}-" + name
	as.Spec.Template.Spec.Source.Path = "{{a}}"
	as.Spec.Template.Spec.Destination.Namespace = "{{b}}"
	as.Spec.Generators = nil
	for range k {
		as.Spec.Generators = append(as.Spec.Generators, v1alpha1.ApplicationSetGenerator{Matrix: &v1alpha1.MatrixGenerator{
			Generators: []v1alpha1.ApplicationSetNestedGenerator{
				{List: &v1alpha1.ListGenerator{Elements: listElements(t, "a", n)}},
				{List: &v1alpha1.ListGenerator{Elements: listElements(t, "b", n)}},
			},
		}})
	}
	return as
}

// TestGenerateBoundsRealGenerator drives the wired generator (applicationsets.NewGenerator):
// limits and the deadline stop the generation while it runs.
func TestGenerateBoundsRealGenerator(t *testing.T) {
	t.Parallel()
	ctx := miscapitest.Ctx(t)

	capped := newFixture(t, fixtureOptions{config: func(c *applicationsets.Config) { c.MaxGeneratedApplications = 100 }})
	alice := capped.Login("alice")
	// Within the limit.
	gen, err := alice.GenerateApplicationSet(ctx, fanOut(t, "small", 1, 10))
	require.NoError(t, err)
	require.Len(t, gen.Applications, 100)
	// One more Application than the limit: 400, for generate and for dry run.
	_, err = alice.GenerateApplicationSet(ctx, fanOut(t, "over", 2, 8))
	ae := apiErr(t, err)
	require.Equal(t, http.StatusBadRequest, ae.StatusCode)
	require.Equal(t, "ApplicationSet over generates more than 100 Applications, the limit of the API", ae.Problem.Detail)
	_, err = alice.CreateApplicationSet(ctx, &apiclient.ApplicationSetCreateRequest{ApplicationSet: fanOut(t, "over", 2, 8), DryRun: true})
	require.Equal(t, http.StatusBadRequest, apiErr(t, err).StatusCode)
	require.False(t, capped.exists(capped.Namespace, "over"))

	// The reviewer's fan-out (12 entries of 141x141, 238,572 Applications unbounded) under a
	// short deadline and limits that do not stop it: 504 at the deadline.
	slow := newFixture(t, fixtureOptions{config: func(c *applicationsets.Config) {
		c.GenerateTimeout = 200 * time.Millisecond
		c.MaxGeneratedApplications = 1 << 30
		c.MaxGenerateParameterSets = 1 << 30
		c.MaxGenerateResponseBytes = 1 << 40
	}})
	start := time.Now()
	_, err = slow.Login("alice").GenerateApplicationSet(ctx, fanOut(t, "heavy", 12, 141))
	elapsed := time.Since(start)
	ae = apiErr(t, err)
	require.Equal(t, http.StatusGatewayTimeout, ae.StatusCode)
	require.Contains(t, ae.Problem.Detail, "timed out after 200ms")
	require.Less(t, elapsed, 10*time.Second, "the generation stops at the deadline")

	// Default limits: the same fan-out stops at 5,000 Applications.
	_, err = capped.Login("admin").GenerateApplicationSet(ctx, fanOut(t, "heavy", 12, 141))
	require.Equal(t, http.StatusBadRequest, apiErr(t, err).StatusCode)
}

// sharedAppSet returns an ApplicationSet whose template project is team-a and whose
// generator-level template generates Applications in team-b.
func sharedAppSet(ns, name string) *v1alpha1.ApplicationSet {
	as := listAppSet(ns, name, "team-a", "x")
	as.Spec.Generators[0].List.Template.Spec.Project = "team-b"
	return as
}

// TestActionsOnExistingNeedEveryProject: reads, upserts and deletes of an ApplicationSet are
// authorized on every project it generates Applications in, so a team-a user cannot see,
// rewrite or delete an ApplicationSet that generates team-b Applications.
func TestActionsOnExistingNeedEveryProject(t *testing.T) {
	t.Parallel()
	f := newFixture(t, fixtureOptions{})
	ctx := miscapitest.Ctx(t)
	projects := func(name string) []string {
		cur := &v1alpha1.ApplicationSet{}
		require.NoError(t, f.Kube.Get(context.Background(), client.ObjectKey{Namespace: f.Namespace, Name: name}, cur))
		p, err := appset.Projects(cur)
		require.NoError(t, err)
		return p
	}
	f.seed(sharedAppSet(f.Namespace, "fleet"))
	f.seed(sharedAppSet(f.Namespace, "fleet2"))
	require.Equal(t, []string{"team-a", "team-b"}, projects("fleet"))

	alice := f.Login("alice") // applicationsets, *, team-a/*
	denied := func(err error) {
		t.Helper()
		ae := apiErr(t, err)
		require.Equal(t, http.StatusForbidden, ae.StatusCode)
		require.Equal(t, "permission denied", ae.Problem.Detail, "same answer as for a missing ApplicationSet")
	}
	_, err := alice.GetApplicationSet(ctx, "fleet", "")
	denied(err)
	_, err = alice.GetApplicationSetResourceTree(ctx, "fleet", "")
	denied(err)
	_, err = alice.ListApplicationSetEvents(ctx, "fleet", "")
	denied(err)
	list, err := alice.ListApplicationSets(ctx, &apiclient.ApplicationSetListRequest{})
	require.NoError(t, err)
	require.Empty(t, list.Items)

	// Upsert with a team-a-only spec: 403, the object keeps generating team-b Applications.
	_, err = alice.CreateApplicationSet(ctx, &apiclient.ApplicationSetCreateRequest{ApplicationSet: listAppSet("", "fleet", "team-a", "x"), Upsert: true})
	require.Equal(t, http.StatusForbidden, apiErr(t, err).StatusCode)
	require.Equal(t, []string{"team-a", "team-b"}, projects("fleet"))

	// Delete: 403, the object (and its team-b Applications) stay.
	denied(alice.DeleteApplicationSet(ctx, &apiclient.ApplicationSetDeleteRequest{Name: "fleet2"}))
	require.True(t, f.exists(f.Namespace, "fleet2"))

	// bob (get and delete on team-b only) is refused as well.
	_, err = f.Login("bob").GetApplicationSet(ctx, "fleet", "")
	denied(err)
	denied(f.Login("bob").DeleteApplicationSet(ctx, &apiclient.ApplicationSetDeleteRequest{Name: "fleet2"}))

	// mover has update on team-a and team-b: the upsert applies.
	moved, err := f.Login("mover").CreateApplicationSet(ctx, &apiclient.ApplicationSetCreateRequest{ApplicationSet: listAppSet("", "fleet", "team-a", "x"), Upsert: true})
	require.NoError(t, err)
	require.Equal(t, "team-a", moved.Spec.Template.Spec.Project)
	require.Equal(t, []string{"team-a"}, projects("fleet"))
	// Now a team-a-only ApplicationSet: alice may read it.
	_, err = alice.GetApplicationSet(ctx, "fleet", "")
	require.NoError(t, err)

	// The read-only role (*/*) sees the shared one; admin deletes it.
	got, err := f.Login("reader").GetApplicationSet(ctx, "fleet2", "")
	require.NoError(t, err)
	require.Equal(t, "fleet2", got.Name)
	require.NoError(t, f.Login("admin").DeleteApplicationSet(ctx, &apiclient.ApplicationSetDeleteRequest{Name: "fleet2"}))
	require.False(t, f.exists(f.Namespace, "fleet2"))

	// A templated project (created with kubectl) fails closed: only grants on every project.
	tmpl := listAppSet(f.Namespace, "templated", "{{cluster}}", "team-a")
	tmpl.Labels = nil
	f.seed(tmpl)
	_, err = alice.GetApplicationSet(ctx, "templated", "")
	denied(err)
	denied(alice.DeleteApplicationSet(ctx, &apiclient.ApplicationSetDeleteRequest{Name: "templated"}))
	require.True(t, f.exists(f.Namespace, "templated"))
	_, err = f.Login("reader").GetApplicationSet(ctx, "templated", "")
	require.NoError(t, err)
	require.NoError(t, f.Login("admin").DeleteApplicationSet(ctx, &apiclient.ApplicationSetDeleteRequest{Name: "templated"}))

	// Every denial is audited with the RBAC object that failed.
	var objects []string
	for _, e := range f.Audit.ByOperation("deleteApplicationSet") {
		objects = append(objects, e.Object)
	}
	require.Contains(t, objects, "team-b/fleet2")
	require.Contains(t, objects, "*/templated")
}
