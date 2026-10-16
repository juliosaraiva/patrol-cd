package projects_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// globalProjectsSetting makes "shared" the global project of projects labeled inherit=yes
// and "baseline" the global project of every project without the optout label.
const globalProjectsSetting = `- projectName: shared
  labelSelector:
    matchLabels:
      inherit: "yes"
- projectName: baseline
  labelSelector:
    matchExpressions:
    - key: optout
      operator: DoesNotExist
`

func assertDenied(t *testing.T, err error, prefix, msg string) {
	t.Helper()
	ae := apiErr(t, err)
	assert.Equal(t, http.StatusForbidden, ae.StatusCode, msg)
	assert.True(t, strings.HasPrefix(ae.Problem.Detail, prefix), "%s: %s", msg, ae.Problem.Detail)
}

// TestProjectEscalationWidening covers the review findings on TEN-046: destinations are
// compared whole (namespace widening, name and name-glob entries), destinationServiceAccounts
// changes are checked, and labels that add or remove global projects need projects, update
// on those global projects plus the permissions for what is inherited or escaped, on update
// and on create.
func TestProjectEscalationWidening(t *testing.T) {
	e := newEnv(t, envOptions{cm: map[string]string{"globalProjects": globalProjectsSetting}})
	ctx := context.Background()
	admin, dave, erin, ivan := e.client("admin"), e.client("dave"), e.client("erin"), e.client("ivan")
	e.createCluster("dev", "https://dev.example.com", "")

	shared := &v1alpha1.AppProject{ObjectMeta: metav1.ObjectMeta{Name: "shared"}, Spec: v1alpha1.AppProjectSpec{
		SourceRepos:              []string{"https://github.com/shared/*"},
		Destinations:             []v1alpha1.ApplicationDestination{{Server: "https://prod.example.com", Namespace: "*"}},
		ClusterResourceWhitelist: []v1alpha1.ClusterResourceRestrictionItem{{Group: "*", Kind: "*"}},
	}}
	baseline := &v1alpha1.AppProject{ObjectMeta: metav1.ObjectMeta{Name: "baseline"}, Spec: v1alpha1.AppProjectSpec{
		NamespaceResourceBlacklist: []metav1.GroupKind{{Group: "", Kind: "Secret"}},
		SyncWindows:                v1alpha1.SyncWindows{{Kind: "deny", Schedule: "0 22 * * *", Duration: "8h", Applications: []string{"*"}}},
	}}
	for _, p := range []*v1alpha1.AppProject{shared, baseline} {
		_, err := admin.CreateProject(ctx, p, false)
		require.NoError(t, err)
	}
	p := teamProject("team-a")
	p.Spec.Destinations = append(p.Spec.Destinations, v1alpha1.ApplicationDestination{Name: "dev", Namespace: "apps"})
	_, err := admin.CreateProject(ctx, p, false)
	require.NoError(t, err)
	before := e.stored("team-a")

	// dave: projects get/update on team-a, no cluster or repository rights.
	cur, err := dave.GetProject(ctx, "team-a")
	require.NoError(t, err)
	for _, tc := range []struct {
		name   string
		mutate func(p *v1alpha1.AppProject)
		detail string
	}{
		{"namespace widened on a server destination", func(p *v1alpha1.AppProject) { p.Spec.Destinations[0].Namespace = "*" },
			"permission denied: clusters, update, https://kube.example.com, sub: dave"},
		{"namespace widened on a name destination", func(p *v1alpha1.AppProject) { p.Spec.Destinations[1].Namespace = "*" },
			"permission denied: clusters, update, https://dev.example.com, sub: dave"},
		{"name glob added next to a name entry", func(p *v1alpha1.AppProject) {
			p.Spec.Destinations = append(p.Spec.Destinations, v1alpha1.ApplicationDestination{Name: "*", Namespace: "*"})
		}, "permission denied: clusters, update, *, sub: dave"},
		{"unregistered name added", func(p *v1alpha1.AppProject) {
			p.Spec.Destinations = append(p.Spec.Destinations, v1alpha1.ApplicationDestination{Name: "future-prod", Namespace: "apps"})
		}, "permission denied: clusters, update, *, sub: dave"},
		{"impersonated service account added", func(p *v1alpha1.AppProject) {
			p.Spec.DestinationServiceAccounts = []v1alpha1.ApplicationDestinationServiceAccount{{Server: "https://kube.example.com", Namespace: "apps", DefaultServiceAccount: "kube-system:admin"}}
		}, "permission denied: clusters, update, https://kube.example.com, sub: dave"},
		{"label opting into a global project", func(p *v1alpha1.AppProject) { p.Labels = map[string]string{"inherit": "yes"} },
			"permission denied: projects, update, shared, sub: dave"},
		{"label opting out of a global project (deny lists and windows)", func(p *v1alpha1.AppProject) { p.Labels = map[string]string{"optout": "true"} },
			"permission denied: projects, update, baseline, sub: dave"},
	} {
		q := cur.DeepCopy()
		tc.mutate(q)
		_, err := dave.UpdateProject(ctx, q)
		assertDenied(t, err, tc.detail, tc.name)
	}
	after := e.stored("team-a")
	assert.Equal(t, before.Spec, after.Spec, "nothing written")
	assert.Equal(t, before.Labels, after.Labels)
	assert.Equal(t, before.ResourceVersion, after.ResourceVersion)

	// ivan may update every project (shared and baseline included) but holds no cluster or
	// repository rights: what the global project grants is still checked.
	q, err := ivan.GetProject(ctx, "team-a")
	require.NoError(t, err)
	q.Labels = map[string]string{"inherit": "yes"}
	_, err = ivan.UpdateProject(ctx, q)
	assertDenied(t, err, "permission denied: clusters, update, https://prod.example.com, sub: ivan", "inherited destinations")
	// Escaping baseline only loses a deny list here: clusters, update on every destination.
	q.Labels = map[string]string{"optout": "true"}
	_, err = ivan.UpdateProject(ctx, q)
	assertDenied(t, err, "permission denied: clusters, update, https://dev.example.com, sub: ivan", "escaped deny list")

	// The admin may do both; the effective project then includes the inherited lists.
	q, err = admin.GetProject(ctx, "team-a")
	require.NoError(t, err)
	q.Labels = map[string]string{"inherit": "yes"}
	_, err = admin.UpdateProject(ctx, q)
	require.NoError(t, err)
	d, err := admin.GetDetailedProject(ctx, "team-a")
	require.NoError(t, err)
	assert.Contains(t, d.Project.Spec.Destinations, v1alpha1.ApplicationDestination{Server: "https://prod.example.com", Namespace: "*"})
	// Opted in, a change of team-a's own resource lists also applies to the inherited
	// destinations: kim, allowed on team-a's own servers, also needs the inherited one.
	kim := e.client("kim")
	q, err = kim.GetProject(ctx, "team-a")
	require.NoError(t, err)
	q.Spec.NamespaceResourceWhitelist = []metav1.GroupKind{{Group: "", Kind: "ConfigMap"}}
	_, err = kim.UpdateProject(ctx, q)
	assertDenied(t, err, "permission denied: clusters, update, https://prod.example.com, sub: kim", "own list on inherited destinations")
	// Without global projects (team-b opted out of baseline by the admin) kim may.
	b := teamProject("team-b")
	b.Labels = map[string]string{"optout": "true"}
	_, err = admin.CreateProject(ctx, b, false)
	require.NoError(t, err)
	q, err = kim.GetProject(ctx, "team-b")
	require.NoError(t, err)
	q.Spec.NamespaceResourceWhitelist = []metav1.GroupKind{{Group: "", Kind: "ConfigMap"}}
	_, err = kim.UpdateProject(ctx, q)
	require.NoError(t, err)
	// Labels that do not change the global projects stay free.
	q, err = dave.GetProject(ctx, "team-a")
	require.NoError(t, err)
	q.Labels["team"] = "a"
	q.Spec.Description = "dave was here"
	_, err = dave.UpdateProject(ctx, q)
	require.NoError(t, err)

	// erin holds clusters, update on one server: she may widen a destination there.
	q, err = erin.GetProject(ctx, "team-a")
	require.NoError(t, err)
	q.Spec.Destinations = append(q.Spec.Destinations, v1alpha1.ApplicationDestination{Server: "https://allowed.example.com", Namespace: "apps"})
	q, err = erin.UpdateProject(ctx, q)
	require.NoError(t, err)
	q.Spec.Destinations[len(q.Spec.Destinations)-1].Namespace = "*"
	_, err = erin.UpdateProject(ctx, q)
	require.NoError(t, err)

	// Creation: the bare project's global projects are the baseline.
	_, err = dave.CreateProject(ctx, &v1alpha1.AppProject{ObjectMeta: metav1.ObjectMeta{Name: "team-x", Labels: map[string]string{"inherit": "yes"}}}, false)
	assertDenied(t, err, "permission denied: projects, update, shared, sub: dave", "create opting in")
	_, err = dave.CreateProject(ctx, &v1alpha1.AppProject{ObjectMeta: metav1.ObjectMeta{Name: "team-y", Labels: map[string]string{"optout": "true"}}}, false)
	assertDenied(t, err, "permission denied: projects, update, baseline, sub: dave", "create opting out")
	_, err = dave.CreateProject(ctx, &v1alpha1.AppProject{ObjectMeta: metav1.ObjectMeta{Name: "team-z", Labels: map[string]string{"team": "z"}}}, false)
	require.NoError(t, err, "same global projects as a bare project")
	_, err = dave.CreateProject(ctx, &v1alpha1.AppProject{ObjectMeta: metav1.ObjectMeta{Name: "team-w"}, Spec: v1alpha1.AppProjectSpec{
		Destinations: []v1alpha1.ApplicationDestination{{Name: "*", Namespace: "*"}},
	}}, false)
	assertDenied(t, err, "permission denied: clusters, update, *, sub: dave", "create with a name glob")
	l, err := admin.ListProjects(ctx, &apiclient.ProjectListRequest{})
	require.NoError(t, err)
	names := make([]string, 0, len(l.Items))
	for _, it := range l.Items {
		names = append(names, it.Name)
	}
	assert.ElementsMatch(t, []string{"baseline", "shared", "team-a", "team-b", "team-z"}, names)
}
