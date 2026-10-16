package project

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// memProjects is an in-memory ProjectGetter returning Kubernetes NotFound errors like a
// lister.
type memProjects map[string]*v1alpha1.AppProject

func (m memProjects) GetProject(_ context.Context, name string) (*v1alpha1.AppProject, error) {
	p, ok := m[name]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: v1alpha1.GroupName, Resource: "appprojects"}, name)
	}
	return p.DeepCopy(), nil
}

type memRepos map[string][]string

func (m memRepos) GetProjectRepositoryURLs(_ context.Context, project string) ([]string, error) {
	return m[project], nil
}

func TestParseGlobalProjectSettings(t *testing.T) {
	t.Parallel()
	settings, err := ParseGlobalProjectSettings(`
- projectName: global-a
  labelSelector:
    matchExpressions:
    - key: opt
      operator: In
      values: [a]
- projectName: global-b
  labelSelector:
    matchLabels:
      team: b
`)
	require.NoError(t, err)
	require.Len(t, settings, 2)
	assert.Equal(t, "global-a", settings[0].ProjectName)
	assert.Equal(t, map[string]string{"team": "b"}, settings[1].LabelSelector.MatchLabels)

	empty, err := ParseGlobalProjectSettings("")
	require.NoError(t, err)
	assert.Empty(t, empty)

	_, err = ParseGlobalProjectSettings("- projectName: [")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "error unmarshalling global project settings: ")
}

func labeled(name string, labels map[string]string, spec v1alpha1.AppProjectSpec) *v1alpha1.AppProject {
	return &v1alpha1.AppProject{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "patrol", Labels: labels}, Spec: spec}
}

func selector(expr ...metav1.LabelSelectorRequirement) metav1.LabelSelector {
	return metav1.LabelSelector{MatchExpressions: expr}
}

// TEN-037.
func TestGlobalProjects(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	globalSpec := v1alpha1.AppProjectSpec{
		SourceRepos:                []string{"https://github.com/global/*"},
		Destinations:               []v1alpha1.ApplicationDestination{{Server: "https://global", Namespace: "*"}},
		ClusterResourceWhitelist:   []v1alpha1.ClusterResourceRestrictionItem{{Group: "", Kind: "Namespace"}},
		ClusterResourceBlacklist:   []v1alpha1.ClusterResourceRestrictionItem{{Group: "*", Kind: "ClusterRole"}},
		NamespaceResourceWhitelist: []metav1.GroupKind{{Group: "apps", Kind: "Deployment"}},
		NamespaceResourceBlacklist: []metav1.GroupKind{{Group: "", Kind: "Secret"}},
		SyncWindows:                v1alpha1.SyncWindows{{Kind: "deny", Schedule: "0 22 * * *", Duration: "1h", Applications: []string{"*"}}},
		DestinationServiceAccounts: []v1alpha1.ApplicationDestinationServiceAccount{{Server: "*", Namespace: "*", DefaultServiceAccount: "global"}},
		// Not inherited:
		SourceNamespaces:                []string{"global-ns"},
		Roles:                           []v1alpha1.ProjectRole{{Name: "global-role"}},
		OrphanedResources:               &v1alpha1.OrphanedResourcesMonitorSettings{},
		SignatureKeys:                   []v1alpha1.SignatureKey{{KeyID: "ABCD"}}, //nolint:staticcheck // deprecated field exercised for migration
		PermitOnlyProjectScopedClusters: true,
		Description:                     "global",
	}
	projects := memProjects{
		"global-a": labeled("global-a", nil, globalSpec),
		"global-b": labeled("global-b", nil, v1alpha1.AppProjectSpec{SourceRepos: []string{"https://github.com/b/*"}}),
	}
	proj := labeled("team", map[string]string{"opt": "a", "team": "b"}, v1alpha1.AppProjectSpec{SourceRepos: []string{"https://github.com/team/*"}})
	settings := []GlobalProjectSetting{
		{ProjectName: "global-a", LabelSelector: selector(metav1.LabelSelectorRequirement{Key: "opt", Operator: metav1.LabelSelectorOpIn, Values: []string{"a"}})},
		{ProjectName: "global-b", LabelSelector: metav1.LabelSelector{MatchLabels: map[string]string{"team": "b"}}},
	}

	globals, err := GlobalProjects(ctx, proj, settings, projects)
	require.NoError(t, err)
	require.Len(t, globals, 2)

	virtual, err := VirtualProject(ctx, proj, settings, projects)
	require.NoError(t, err)
	assert.Equal(t, []string{"https://github.com/team/*", "https://github.com/global/*", "https://github.com/b/*"}, virtual.Spec.SourceRepos)
	assert.Equal(t, globalSpec.Destinations, virtual.Spec.Destinations)
	assert.Equal(t, globalSpec.ClusterResourceWhitelist, virtual.Spec.ClusterResourceWhitelist)
	assert.Equal(t, globalSpec.ClusterResourceBlacklist, virtual.Spec.ClusterResourceBlacklist)
	assert.Equal(t, globalSpec.NamespaceResourceWhitelist, virtual.Spec.NamespaceResourceWhitelist)
	assert.Equal(t, globalSpec.NamespaceResourceBlacklist, virtual.Spec.NamespaceResourceBlacklist)
	assert.Equal(t, globalSpec.SyncWindows, virtual.Spec.SyncWindows)
	assert.Equal(t, globalSpec.DestinationServiceAccounts, virtual.Spec.DestinationServiceAccounts)
	assert.Empty(t, virtual.Spec.SourceNamespaces)
	assert.Empty(t, virtual.Spec.Roles)
	assert.Nil(t, virtual.Spec.OrphanedResources)
	assert.Empty(t, virtual.Spec.SignatureKeys) //nolint:staticcheck // deprecated field exercised for migration
	assert.False(t, virtual.Spec.PermitOnlyProjectScopedClusters)
	assert.Empty(t, virtual.Spec.Description)
	assert.Equal(t, []string{"https://github.com/team/*"}, proj.Spec.SourceRepos, "input not modified")
	// Inheriting a namespace whitelist turns "all kinds" into "only the inherited ones".
	assert.False(t, IsGroupKindPermitted(virtual, schema.GroupKind{Kind: "ConfigMap"}, true))
	assert.True(t, IsGroupKindPermitted(proj, schema.GroupKind{Kind: "ConfigMap"}, true))

	// matchExpressions operators.
	for _, tt := range []struct {
		req  metav1.LabelSelectorRequirement
		want bool
	}{
		{metav1.LabelSelectorRequirement{Key: "opt", Operator: metav1.LabelSelectorOpIn, Values: []string{"a"}}, true},
		{metav1.LabelSelectorRequirement{Key: "opt", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"a"}}, false},
		{metav1.LabelSelectorRequirement{Key: "opt", Operator: metav1.LabelSelectorOpExists}, true},
		{metav1.LabelSelectorRequirement{Key: "opt", Operator: metav1.LabelSelectorOpDoesNotExist}, false},
		{metav1.LabelSelectorRequirement{Key: "missing", Operator: metav1.LabelSelectorOpDoesNotExist}, true},
	} {
		globals, err := GlobalProjects(ctx, proj, []GlobalProjectSetting{{ProjectName: "global-b", LabelSelector: selector(tt.req)}}, projects)
		require.NoError(t, err)
		assert.Equal(t, tt.want, len(globals) == 1, "%+v", tt.req)
	}

	// A global project is not its own global project.
	self, err := GlobalProjects(ctx, projects["global-a"], []GlobalProjectSetting{{ProjectName: "global-a"}}, projects)
	require.NoError(t, err)
	assert.Empty(t, self)

	// A missing global project stops the later entries (parity), and is reported.
	broken := []GlobalProjectSetting{{ProjectName: "global-b"}, {ProjectName: "missing"}, {ProjectName: "global-a"}}
	globals, err = GlobalProjects(ctx, proj, broken, projects)
	require.Error(t, err)
	assert.True(t, apierrors.IsNotFound(errors.Unwrap(err)))
	require.Len(t, globals, 1)
	assert.Equal(t, "global-b", globals[0].Name)
	virtual, err = VirtualProject(ctx, proj, broken, projects)
	require.Error(t, err)
	assert.Contains(t, virtual.Spec.SourceRepos, "https://github.com/b/*")
	assert.NotContains(t, virtual.Spec.SourceRepos, "https://github.com/global/*")

	// An invalid selector stops processing too.
	invalid := []GlobalProjectSetting{{ProjectName: "global-a", LabelSelector: selector(metav1.LabelSelectorRequirement{Key: "opt", Operator: "Bogus"})}, {ProjectName: "global-b"}}
	globals, err = GlobalProjects(ctx, proj, invalid, projects)
	require.Error(t, err)
	assert.Empty(t, globals)
}

// TEN-038 / TEN-039 / REPO-004 / REPO-081.
func TestEffectiveProjectScopedResources(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	proj := labeled("team", nil, v1alpha1.AppProjectSpec{SourceRepos: []string{"https://github.com/team/*"}})
	idx := NewClusterIndex([]v1alpha1.Cluster{
		cluster("all-ns", "https://all/", func(c *v1alpha1.Cluster) { c.Spec.Project = "team" }),
		cluster("two-ns", "https://two", func(c *v1alpha1.Cluster) { c.Spec.Project = "team"; c.Spec.Namespaces = []string{"a", "b"} }),
		cluster("other", "https://other", func(c *v1alpha1.Cluster) { c.Spec.Project = "other" }),
		cluster("global", "https://global"),
	}, true)
	l := Lookups{Repositories: memRepos{"team": {"https://gitlab.com/scoped/repo.git"}}, Clusters: idx}

	eff, gerr, err := EffectiveProject(ctx, proj, l)
	require.NoError(t, err)
	require.NoError(t, gerr)
	assert.Equal(t, []string{"https://github.com/team/*", "https://gitlab.com/scoped/repo.git"}, eff.Spec.SourceRepos)
	assert.Equal(t, []v1alpha1.ApplicationDestination{
		{Server: "https://all", Namespace: "*"},
		{Server: "https://two", Namespace: "a"},
		{Server: "https://two", Namespace: "b"},
	}, eff.Spec.Destinations)
	assert.True(t, IsSourcePermitted(eff, "https://gitlab.com/scoped/repo"), "scoped repo permitted without being listed")
	assert.False(t, IsSourcePermitted(proj, "https://gitlab.com/scoped/repo"))

	ok, err := IsDestinationPermitted(ctx, eff, Destination{Server: "https://two", Name: "two-ns", Namespace: "b"}, idx)
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = IsDestinationPermitted(ctx, eff, Destination{Server: "https://two", Name: "two-ns", Namespace: "c"}, idx)
	require.NoError(t, err)
	assert.False(t, ok)

	// permitOnlyProjectScopedClusters rejects a global cluster even when listed explicitly.
	strict := eff.DeepCopy()
	strict.Spec.PermitOnlyProjectScopedClusters = true
	strict.Spec.Destinations = append(strict.Spec.Destinations, v1alpha1.ApplicationDestination{Server: "https://global", Namespace: "*"})
	ok, err = IsDestinationPermitted(ctx, strict, Destination{Server: "https://global", Name: "global", Namespace: "x"}, idx)
	require.NoError(t, err)
	assert.False(t, ok)
	ok, err = IsDestinationPermitted(ctx, strict, Destination{Server: "https://all", Name: "all-ns", Namespace: "x"}, idx)
	require.NoError(t, err)
	assert.True(t, ok)

	failing := Lookups{Repositories: failingRepos{}}
	_, _, err = EffectiveProject(ctx, proj, failing)
	assert.ErrorContains(t, err, "error getting project repositories: boom")
	failingC := Lookups{Clusters: failingClusters{}}
	_, _, err = EffectiveProject(ctx, proj, failingC)
	assert.ErrorContains(t, err, "error getting project clusters: boom")
}

type failingRepos struct{}

func (failingRepos) GetProjectRepositoryURLs(context.Context, string) ([]string, error) {
	return nil, errors.New("boom")
}

type failingClusters struct{ ClusterLookup }

func (failingClusters) GetProjectClusters(context.Context, string) ([]*v1alpha1.Cluster, error) {
	return nil, errors.New("boom")
}

func TestResolveAppProject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	projects := memProjects{
		"default": labeled("default", nil, v1alpha1.AppProjectSpec{SourceRepos: []string{"*"}}),
		"team":    labeled("team", nil, v1alpha1.AppProjectSpec{SourceNamespaces: []string{"team-*"}}),
	}
	l := Lookups{Projects: projects}
	app := &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "patrol"}}

	p, _, err := ResolveAppProject(ctx, app, "patrol", l)
	require.NoError(t, err)
	assert.Equal(t, "default", p.Name, "empty spec.project means default")

	app.Spec.Project = "missing"
	_, _, err = ResolveAppProject(ctx, app, "patrol", l)
	require.Error(t, err)
	assert.True(t, IsNotFound(err))
	issue := ProjectErrorIssue(err, app)
	assert.Equal(t, Issue{Type: v1alpha1.ApplicationConditionInvalidSpecError, Reason: v1alpha1.ReasonProjectNotFound, Message: "Application referencing project missing which does not exist"}, issue)

	app.Spec.Project = "team"
	app.Namespace = "other"
	_, _, err = ResolveAppProject(ctx, app, "patrol", l)
	require.Error(t, err)
	issue = ProjectErrorIssue(err, app)
	assert.Equal(t, Issue{Type: v1alpha1.ApplicationConditionUnknownError, Reason: v1alpha1.ReasonProjectNotPermitted, Message: "application 'app' in namespace 'other' is not permitted to use project 'team'"}, issue)

	app.Namespace = "team-a"
	p, _, err = ResolveAppProject(ctx, app, "patrol", l)
	require.NoError(t, err)
	assert.Equal(t, "team", p.Name)

	_, _, err = ResolveAppProject(ctx, app, "patrol", Lookups{Projects: erroringProjects{}})
	require.Error(t, err)
	assert.Equal(t, "could not retrieve AppProject 'team': unavailable", err.Error())
	issue = ProjectErrorIssue(err, app)
	assert.Equal(t, v1alpha1.ApplicationConditionUnknownError, issue.Type)

	_, _, err = ResolveAppProject(ctx, app, "patrol", Lookups{})
	require.Error(t, err)
}

type erroringProjects struct{}

func (erroringProjects) GetProject(context.Context, string) (*v1alpha1.AppProject, error) {
	return nil, fmt.Errorf("unavailable")
}

func scopedTo(project string, namespaces ...string) func(*v1alpha1.Cluster) {
	return func(c *v1alpha1.Cluster) {
		c.Spec.Project = project
		c.Spec.Namespaces = namespaces
	}
}

// Security (deliberate difference from upstream, which appends the raw values as patterns):
// a project-scoped Repository or Cluster widens its project by exactly itself. URLs and
// servers that read as globs or negations are never interpreted as patterns.
func TestEffectiveProjectScopedResourcesAreExact(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	proj := labeled("team", nil, v1alpha1.AppProjectSpec{
		SourceRepos:  []string{"https://github.com/team/*"},
		Destinations: []v1alpha1.ApplicationDestination{{Server: "https://listed", Namespace: "web"}},
	})
	idx := NewClusterIndex([]v1alpha1.Cluster{
		cluster("star", "https://*", scopedTo("team")),
		cluster("negated", "!https://x", scopedTo("team")),
		cluster("ipv6", "https://[fd00::1]:6443/", scopedTo("team", "web", "!kube-system", "*")),
		cluster("prod", "https://prod.example.com"),
		cluster("listed", "https://listed"),
	}, true)
	repos := memRepos{"team": {
		"*", "!x", " !https://github.com/evil/repo", "https://github.com/*/*", "https://github.com/evil/rep?",
		"https://gitlab.com/Scoped/Repo.git", "https://gitlab.com/denied/repo", "not a url %zz",
	}}
	eff, gerr, err := EffectiveProject(ctx, proj, Lookups{Repositories: repos, Clusters: idx})
	require.NoError(t, err)
	require.NoError(t, gerr)

	for _, src := range []string{"https://github.com/evil/repo", "https://github.com/evil/rep?", "https://github.com/evil/repx", "x", "", "%zz"} {
		require.False(t, IsSourcePermitted(proj, src))
		assert.False(t, IsSourcePermitted(eff, src), "pattern-like scoped repository widened the project to %q", src)
	}
	assert.Equal(t, []string{"https://github.com/team/*", "https://gitlab.com/Scoped/Repo.git", "https://gitlab.com/denied/repo"}, eff.Spec.SourceRepos)
	assert.True(t, IsSourcePermitted(eff, "https://gitlab.com/scoped/repo"), "a regular scoped repository is permitted")
	assert.True(t, IsSourcePermitted(eff, "https://github.com/team/web"), "listed entries still apply")

	// The project's deny entries still win over scoped repositories.
	denying := labeled("team", nil, v1alpha1.AppProjectSpec{SourceRepos: []string{"https://github.com/team/*", "!https://gitlab.com/denied/*"}})
	effDenying, _, err := EffectiveProject(ctx, denying, Lookups{Repositories: repos})
	require.NoError(t, err)
	assert.False(t, IsSourcePermitted(effDenying, "https://gitlab.com/denied/repo"))
	assert.True(t, IsSourcePermitted(effDenying, "https://gitlab.com/scoped/repo"))

	dest := func(name, namespace string) bool {
		t.Helper()
		ok, _, err := IsAppDestinationPermitted(ctx, eff, v1alpha1.ApplicationDestination{Name: name, Namespace: namespace}, idx)
		require.NoError(t, err)
		return ok
	}
	assert.False(t, dest("prod", "kube-system"), "server https://* widened the project to an unscoped cluster")
	assert.False(t, dest("prod", "web"))
	assert.True(t, dest("star", "kube-system"), "the scoped cluster itself is permitted")
	assert.True(t, dest("negated", "default"), "the scoped cluster itself is permitted")
	assert.True(t, dest("ipv6", "web"), "IPv6 servers match themselves (upstream reads [..] as a character class)")
	assert.False(t, dest("ipv6", "api"), "scoped namespaces are exact: '*' and '!kube-system' are not patterns")
	assert.True(t, dest("ipv6", "*"))
	assert.False(t, dest("ipv6", "kube-system"))
	assert.True(t, dest("listed", "web"), "listed destinations still apply")
	assert.False(t, dest("listed", "api"))
	assert.ElementsMatch(t, []v1alpha1.ApplicationDestination{
		{Server: `https://\*`, Namespace: "*"},
		{Server: `\!https://x`, Namespace: "*"},
		{Server: `https://\[fd00::1\]:6443`, Namespace: "web"},
		{Server: `https://\[fd00::1\]:6443`, Namespace: `\!kube-system`},
		{Server: `https://\[fd00::1\]:6443`, Namespace: `\*`},
	}, eff.Spec.Destinations[1:])
}
