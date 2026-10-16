package project

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

const controlNS = "patrol"

func testLookup() *ClusterIndex {
	return NewClusterIndex([]v1alpha1.Cluster{
		cluster("prod", "https://prod.example.com"),
		cluster("dup-a", "https://dup-a", func(c *v1alpha1.Cluster) { c.Spec.Name = "dup" }),
		cluster("dup-b", "https://dup-b", func(c *v1alpha1.Cluster) { c.Spec.Name = "dup" }),
	}, true)
}

func testProject() *v1alpha1.AppProject {
	return &v1alpha1.AppProject{
		ObjectMeta: metav1.ObjectMeta{Name: "team", Namespace: controlNS},
		Spec: v1alpha1.AppProjectSpec{
			SourceRepos:      []string{"https://github.com/team/*"},
			SourceNamespaces: []string{"team-*"},
			Destinations:     []v1alpha1.ApplicationDestination{{Server: "https://prod.example.com", Namespace: "apps"}, {Name: "in-cluster", Namespace: "dev"}},
		},
	}
}

func withSources(p *v1alpha1.AppProject, sources ...string) *v1alpha1.AppProject {
	p.Spec.SourceRepos = sources
	return p
}

func testApp(mutate ...func(*v1alpha1.Application)) *v1alpha1.Application {
	app := &v1alpha1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: controlNS},
		Spec: v1alpha1.ApplicationSpec{
			Project:     "team",
			Source:      &v1alpha1.ApplicationSource{RepoURL: "https://github.com/team/web.git", Path: "deploy", TargetRevision: "main"},
			Destination: v1alpha1.ApplicationDestination{Server: "https://prod.example.com", Namespace: "apps"},
		},
	}
	for _, m := range mutate {
		m(app)
	}
	return app
}

func messages(issues []Issue) []string {
	out := make([]string, 0, len(issues))
	for _, i := range issues {
		out = append(out, i.Message)
	}
	return out
}

// APP-002..005, APP-014, APP-026, APP-033, APP-036, APP-068, TEN-040.
func TestValidateApplication(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tests := []struct {
		name   string
		app    *v1alpha1.Application
		proj   *v1alpha1.AppProject
		issues []Issue
	}{
		{"valid", testApp(), testProject(), nil},
		{"valid by name", testApp(func(a *v1alpha1.Application) {
			a.Spec.Destination = v1alpha1.ApplicationDestination{Name: "prod", Namespace: "apps"}
		}), testProject(), nil},
		{"valid in-cluster by server", testApp(func(a *v1alpha1.Application) {
			a.Spec.Destination = v1alpha1.ApplicationDestination{Server: v1alpha1.KubernetesInternalAPIServerAddr, Namespace: "dev"}
		}), testProject(), nil},
		{"project missing", testApp(), nil, []Issue{{v1alpha1.ApplicationConditionInvalidSpecError, v1alpha1.ReasonProjectNotFound, "Application referencing project team which does not exist"}}},
		{"default project missing", testApp(func(a *v1alpha1.Application) { a.Spec.Project = "" }), nil, []Issue{{v1alpha1.ApplicationConditionInvalidSpecError, v1alpha1.ReasonProjectNotFound, "Application referencing project default which does not exist"}}},
		{"namespace not permitted", testApp(func(a *v1alpha1.Application) { a.Namespace = "ns-b" }), testProject(), []Issue{{v1alpha1.ApplicationConditionUnknownError, v1alpha1.ReasonProjectNotPermitted, "application 'web' in namespace 'ns-b' is not permitted to use project 'team'"}}},
		{"namespace permitted", testApp(func(a *v1alpha1.Application) { a.Namespace = "team-a" }), testProject(), nil},
		{"repo not permitted", testApp(func(a *v1alpha1.Application) { a.Spec.Source.RepoURL = "https://github.com/other/web" }), testProject(),
			[]Issue{{v1alpha1.ApplicationConditionInvalidSpecError, v1alpha1.ReasonRepositoryNotPermitted, "application repo https://github.com/other/web is not permitted in project 'team'"}}},
		{"repo denied", testApp(func(a *v1alpha1.Application) { a.Spec.Source.RepoURL = "https://github.com/team/forbidden" }), withSources(testProject(), "*", "!https://github.com/team/forbidden"),
			[]Issue{{v1alpha1.ApplicationConditionInvalidSpecError, v1alpha1.ReasonRepositoryNotPermitted, "application repo https://github.com/team/forbidden is not permitted in project 'team'"}}},
		{"repo with credentials redacted", testApp(func(a *v1alpha1.Application) { a.Spec.Source.RepoURL = "https://u:secret@github.com/other/web" }), testProject(),
			[]Issue{{v1alpha1.ApplicationConditionInvalidSpecError, v1alpha1.ReasonRepositoryNotPermitted, "application repo https://u:xxxxx@github.com/other/web is not permitted in project 'team'"}}},
		{"missing source", testApp(func(a *v1alpha1.Application) { a.Spec.Source = nil }), testProject(),
			[]Issue{{v1alpha1.ApplicationConditionInvalidSpecError, v1alpha1.ReasonInvalidSpec, "spec.source.repoURL and either spec.source.path or spec.source.chart are required"}}},
		{"missing path", testApp(func(a *v1alpha1.Application) { a.Spec.Source.Path = "" }), testProject(),
			[]Issue{{v1alpha1.ApplicationConditionInvalidSpecError, v1alpha1.ReasonInvalidSpec, "spec.source.repoURL and either spec.source.path or spec.source.chart are required"}}},
		{"chart without revision", testApp(func(a *v1alpha1.Application) {
			a.Spec.Source = &v1alpha1.ApplicationSource{RepoURL: "https://github.com/team/charts", Chart: "web"}
		}), testProject(),
			[]Issue{{v1alpha1.ApplicationConditionInvalidSpecError, v1alpha1.ReasonInvalidSpec, "spec.source.targetRevision is required if the manifest source is a helm chart"}}},
		{"both server and name", testApp(func(a *v1alpha1.Application) { a.Spec.Destination.Name = "prod" }), testProject(),
			[]Issue{{v1alpha1.ApplicationConditionInvalidSpecError, v1alpha1.ReasonDestinationNotFound, "application destination can't have both name and server defined: prod https://prod.example.com"}}},
		{"no destination", testApp(func(a *v1alpha1.Application) { a.Spec.Destination = v1alpha1.ApplicationDestination{Namespace: "apps"} }), testProject(),
			[]Issue{{v1alpha1.ApplicationConditionInvalidSpecError, v1alpha1.ReasonDestinationNotFound, "Destination server missing from app spec"}}},
		{"unknown name", testApp(func(a *v1alpha1.Application) {
			a.Spec.Destination = v1alpha1.ApplicationDestination{Name: "nope", Namespace: "apps"}
		}), testProject(),
			[]Issue{{v1alpha1.ApplicationConditionInvalidSpecError, v1alpha1.ReasonDestinationNotFound, "there are no clusters with this name: nope"}}},
		{"ambiguous name", testApp(func(a *v1alpha1.Application) {
			a.Spec.Destination = v1alpha1.ApplicationDestination{Name: "dup", Namespace: "apps"}
		}), testProject(),
			[]Issue{{v1alpha1.ApplicationConditionInvalidSpecError, v1alpha1.ReasonDestinationNotFound, "there are 2 clusters with the same name: [https://dup-a https://dup-b]"}}},
		{"unknown server", testApp(func(a *v1alpha1.Application) { a.Spec.Destination.Server = "https://unknown" }), testProject(),
			[]Issue{{v1alpha1.ApplicationConditionInvalidSpecError, v1alpha1.ReasonDestinationNotFound, `error getting cluster by server "https://unknown": cluster "https://unknown" not found: cluster not found`}}},
		{"source and destination both wrong", testApp(func(a *v1alpha1.Application) {
			a.Spec.Source.RepoURL = "https://github.com/other/web"
			a.Spec.Destination.Server = "https://unknown"
		}), testProject(), []Issue{
			{v1alpha1.ApplicationConditionInvalidSpecError, v1alpha1.ReasonRepositoryNotPermitted, "application repo https://github.com/other/web is not permitted in project 'team'"},
			{v1alpha1.ApplicationConditionInvalidSpecError, v1alpha1.ReasonDestinationNotFound, `error getting cluster by server "https://unknown": cluster "https://unknown" not found: cluster not found`},
		}},
		{"destination not permitted", testApp(func(a *v1alpha1.Application) { a.Spec.Destination.Namespace = "kube-system" }), testProject(),
			[]Issue{{v1alpha1.ApplicationConditionInvalidSpecError, v1alpha1.ReasonInvalidSpec, "application destination server 'https://prod.example.com' and namespace 'kube-system' do not match any of the allowed destinations in project 'team'"}}},
		{"destination by name not permitted prints the name", testApp(func(a *v1alpha1.Application) {
			a.Spec.Destination = v1alpha1.ApplicationDestination{Name: "prod", Namespace: "kube-system"}
		}), testProject(),
			[]Issue{{v1alpha1.ApplicationConditionInvalidSpecError, v1alpha1.ReasonInvalidSpec, "application destination server 'prod' and namespace 'kube-system' do not match any of the allowed destinations in project 'team'"}}},
		{"multi-source one per disallowed source", testApp(func(a *v1alpha1.Application) {
			a.Spec.Source = nil
			a.Spec.Sources = v1alpha1.ApplicationSources{
				{RepoURL: "https://github.com/a/x", Path: "x"},
				{RepoURL: "https://github.com/team/ok", Path: "y"},
				{RepoURL: "https://github.com/b/z", Ref: "values"},
			}
		}), testProject(), []Issue{
			{v1alpha1.ApplicationConditionInvalidSpecError, v1alpha1.ReasonRepositoryNotPermitted, "application repo https://github.com/a/x is not permitted in project 'team'"},
			{v1alpha1.ApplicationConditionInvalidSpecError, v1alpha1.ReasonRepositoryNotPermitted, "application repo https://github.com/b/z is not permitted in project 'team'"},
		}},
		{"multi-source missing fields", testApp(func(a *v1alpha1.Application) {
			a.Spec.Source = nil
			a.Spec.Sources = v1alpha1.ApplicationSources{{RepoURL: "https://github.com/a/x", Path: "x"}, {RepoURL: "https://github.com/team/y", TargetRevision: "v1"}}
		}), testProject(), []Issue{
			{v1alpha1.ApplicationConditionInvalidSpecError, v1alpha1.ReasonRepositoryNotPermitted, "application repo https://github.com/a/x is not permitted in project 'team'"},
			{v1alpha1.ApplicationConditionInvalidSpecError, v1alpha1.ReasonInvalidSpec, "spec.source.repoURL and either source.path, source.chart, or source.ref are required for source &ApplicationSource{RepoURL:https://github.com/team/y,Path:,TargetRevision:v1,Helm:nil,Kustomize:nil,Directory:nil,Plugin:nil,Chart:,Ref:,Name:,TagPrefix:,}"},
		}},
		{"hydrator missing fields", testApp(func(a *v1alpha1.Application) {
			a.Spec.Source = nil
			a.Spec.SourceHydrator = &v1alpha1.SourceHydrator{HydrateTo: &v1alpha1.HydrateTo{}}
		}), testProject(), []Issue{
			{v1alpha1.ApplicationConditionInvalidSpecError, v1alpha1.ReasonInvalidSpec, "spec.sourceHydrator.drySource.repoURL is required"},
			{v1alpha1.ApplicationConditionInvalidSpecError, v1alpha1.ReasonInvalidSpec, "spec.sourceHydrator.syncSource.targetBranch is required"},
			{v1alpha1.ApplicationConditionInvalidSpecError, v1alpha1.ReasonInvalidSpec, "when spec.sourceHydrator.hydrateTo is set, spec.sourceHydrator.hydrateTo.targetBranch is required"},
		}},
		{"hydrator dry repo not permitted", testApp(func(a *v1alpha1.Application) {
			a.Spec.Source = nil
			a.Spec.SourceHydrator = &v1alpha1.SourceHydrator{
				DrySource:  v1alpha1.DrySource{RepoURL: "https://github.com/other/dry", TargetRevision: "main", Path: "app"},
				SyncSource: v1alpha1.SyncSource{TargetBranch: "env/prod", Path: "app"},
			}
		}), testProject(), []Issue{
			{v1alpha1.ApplicationConditionInvalidSpecError, v1alpha1.ReasonRepositoryNotPermitted, "application repo https://github.com/other/dry is not permitted in project 'team'"},
		}},
		{"hydrator sync repo not permitted", testApp(func(a *v1alpha1.Application) {
			a.Spec.Source = nil
			a.Spec.SourceHydrator = &v1alpha1.SourceHydrator{
				DrySource:  v1alpha1.DrySource{RepoURL: "https://github.com/team/dry", TargetRevision: "main", Path: "app"},
				SyncSource: v1alpha1.SyncSource{TargetBranch: "env/prod", Path: "app", RepoURL: "https://github.com/other/hydrated"},
			}
		}), testProject(), []Issue{
			{v1alpha1.ApplicationConditionInvalidSpecError, v1alpha1.ReasonRepositoryNotPermitted, "sync source repo https://github.com/other/hydrated is not permitted in project 'team'"},
		}},
		{"sync options", testApp(func(a *v1alpha1.Application) {
			a.Spec.SyncPolicy = &v1alpha1.SyncPolicy{SyncOptions: v1alpha1.SyncOptions{"CreateNamespace=true", "CreateNamespaces=true", "Prune=sometimes", "bad option", "PrunePropagationPolicy=orphan"}}
		}), testProject(), []Issue{
			{v1alpha1.ApplicationConditionInvalidSpecError, v1alpha1.ReasonInvalidSpec, `unknown sync option "CreateNamespaces"`},
			{v1alpha1.ApplicationConditionInvalidSpecError, v1alpha1.ReasonInvalidSpec, `invalid value "sometimes" for sync option Prune: must be one of true, false, confirm`},
			{v1alpha1.ApplicationConditionInvalidSpecError, v1alpha1.ReasonInvalidSpec, `sync option "bad option" must have the form Key=Value`},
		}},
		{"ref sources", testApp(func(a *v1alpha1.Application) {
			a.Spec.Source = nil
			a.Spec.Sources = v1alpha1.ApplicationSources{
				{RepoURL: "https://github.com/team/charts", Chart: "web", TargetRevision: "1.0.0", Helm: &v1alpha1.ApplicationSourceHelm{ValueFiles: []string{"$vals/values.yaml"}}},
				{RepoURL: "https://github.com/team/values", Ref: "values"},
			}
		}), testProject(), []Issue{
			{v1alpha1.ApplicationConditionInvalidSpecError, v1alpha1.ReasonInvalidSpec, `source referenced "$vals", which is not one of the available sources ($values)`},
		}},
	}
	for _, tt := range tests {
		got := ValidateApplication(ctx, tt.app, tt.proj, testLookup(), ValidateOptions{ControlPlaneNamespace: controlNS})
		if len(tt.issues) == 0 {
			assert.Empty(t, got, tt.name)
			continue
		}
		assert.Equal(t, tt.issues, got, tt.name)
	}

	opts := ValidateOptions{ControlPlaneNamespace: controlNS, SkipSyncOptions: true, SkipRefSources: true}
	app := testApp(func(a *v1alpha1.Application) {
		a.Spec.SyncPolicy = &v1alpha1.SyncPolicy{SyncOptions: v1alpha1.SyncOptions{"Bogus=1"}}
	})
	assert.Empty(t, ValidateApplication(ctx, app, testProject(), testLookup(), opts))
	assert.NotEmpty(t, ValidateApplication(ctx, nil, testProject(), testLookup(), opts))
}

func TestValidatePermissionsLookupFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	proj := testProject()
	proj.Spec.PermitOnlyProjectScopedClusters = true
	lookup := scopedFailure{testLookup()}
	issues := ValidateApplication(ctx, testApp(func(a *v1alpha1.Application) { a.Spec.Source.RepoURL = "https://github.com/other/x" }), proj, lookup, ValidateOptions{ControlPlaneNamespace: controlNS})
	assert.Equal(t, []Issue{{v1alpha1.ApplicationConditionUnknownError, v1alpha1.ReasonUnknownError, "could not retrieve project clusters: boom"}}, issues,
		"an internal error replaces the other conditions, as upstream refreshAppConditions")
}

type scopedFailure struct{ *ClusterIndex }

func (scopedFailure) GetProjectClusters(context.Context, string) ([]*v1alpha1.Cluster, error) {
	return nil, errors.New("boom")
}

// APP-015 ref validation messages.
func TestValidateRefSources(t *testing.T) {
	t.Parallel()
	helm := func(files ...string) *v1alpha1.ApplicationSourceHelm {
		return &v1alpha1.ApplicationSourceHelm{ValueFiles: files}
	}
	tests := []struct {
		name    string
		sources v1alpha1.ApplicationSources
		err     string
	}{
		{"ok", v1alpha1.ApplicationSources{{RepoURL: "r", Chart: "c", Helm: helm("$values/a.yaml", "plain.yaml")}, {RepoURL: "https://g/v", Ref: "values"}}, ""},
		{"bad ref", v1alpha1.ApplicationSources{{RepoURL: "r", Ref: "a.b"}}, "sources.ref a.b cannot contain any special characters except '_' and '-'"},
		{"dup ref", v1alpha1.ApplicationSources{{RepoURL: "r", Ref: "a"}, {RepoURL: "s", Ref: "a"}}, "invalid sources: multiple sources had the same `ref` key"},
		{"no refs", v1alpha1.ApplicationSources{{RepoURL: "r", Chart: "c", Helm: helm("$values/a.yaml")}}, `source referenced "$values", but no source has a 'ref' field defined`},
		{"unknown ref", v1alpha1.ApplicationSources{{RepoURL: "r", Chart: "c", Helm: helm("$x/a.yaml")}, {RepoURL: "s", Ref: "b"}, {RepoURL: "t", Ref: "a"}}, `source referenced "$x", which is not one of the available sources ($a, $b)`},
		{"file parameter", v1alpha1.ApplicationSources{{RepoURL: "r", Chart: "c", Helm: &v1alpha1.ApplicationSourceHelm{FileParameters: []v1alpha1.HelmFileParameter{{Name: "f", Path: "$x/f"}}}}, {RepoURL: "s", Ref: "a"}}, `source referenced "$x", which is not one of the available sources ($a)`},
		{"chart ref", v1alpha1.ApplicationSources{{RepoURL: "r", Chart: "c", Helm: helm("$a/v.yaml")}, {RepoURL: "s", Chart: "d", Ref: "a"}}, "source has a 'chart' field defined, but Helm charts are not yet not supported for 'ref' sources"},
		{"two revisions same repo", v1alpha1.ApplicationSources{
			{RepoURL: "r", Chart: "c", Helm: helm("$a/v.yaml", "$b/v.yaml")},
			{RepoURL: "https://github.com/org/values.git", TargetRevision: "v1", Ref: "a"},
			{RepoURL: "https://github.com/Org/values", TargetRevision: "v2", Ref: "b"},
		}, `cannot reference multiple revisions for the same repository ($b references "v2" while $a references "v1")`},
		{"same repo same revision", v1alpha1.ApplicationSources{
			{RepoURL: "r", Chart: "c", Helm: helm("$a/v.yaml", "$b/v.yaml")},
			{RepoURL: "https://github.com/org/values.git", TargetRevision: "v1", Ref: "a"},
			{RepoURL: "https://github.com/org/values", TargetRevision: "v1", Ref: "b"},
		}, ""},
	}
	for _, tt := range tests {
		err := ValidateRefSources(tt.sources)
		if tt.err == "" {
			assert.NoError(t, err, tt.name)
			continue
		}
		assert.EqualError(t, err, tt.err, tt.name)
	}
}

func TestValidateSourceNames(t *testing.T) {
	t.Parallel()
	app := testApp(func(a *v1alpha1.Application) {
		a.Spec.Source = nil
		a.Spec.Sources = v1alpha1.ApplicationSources{{RepoURL: "a", Name: "x"}, {RepoURL: "b"}, {RepoURL: "c"}, {RepoURL: "d", Name: "x"}}
	})
	assert.EqualError(t, ValidateSourceNames(app), "application web has duplicate source name: x")
	app.Spec.Sources[3].Name = "y"
	assert.NoError(t, ValidateSourceNames(app))
	assert.NoError(t, ValidateSourceNames(testApp()))
}

func TestValidateSyncOptions(t *testing.T) {
	t.Parallel()
	assert.Empty(t, ValidateSyncOptions(v1alpha1.SyncOptions{
		"Validate=false", "CreateNamespace=true", "Prune=confirm", "Delete=false", "PruneLast=true",
		"PrunePropagationPolicy=background", "ApplyOutOfSyncOnly=true", "Replace=true", "Force=true",
		"ServerSideApply=false", "FailOnSharedResource=true", "RespectIgnoreDifferences=true",
		"SkipDryRunOnMissingResource=true", "ClientSideApplyMigration=false", "PrunePropagationPolicy=orphan",
		"PrunePropagationPolicy=foreground", "Prune=false", "Delete=confirm",
	}))
	assert.Equal(t, []string{
		`sync option "=true" must have the form Key=Value`,
		`sync option "Prune" must have the form Key=Value`,
		`sync option "Prune= false" must have the form Key=Value`,
		`invalid value "yes" for sync option Validate: must be one of true, false`,
		`unknown sync option "CreateNamespaces"`,
	}, ValidateSyncOptions(v1alpha1.SyncOptions{"=true", "Prune", "Prune= false", "Validate=yes", "CreateNamespaces=true"}))

	// Keys and values are case-sensitive, like the sync engine's exact option matching
	// (upstream HasOption): a wrong-case option would be silently ignored.
	assert.Equal(t, []string{
		`invalid value "True" for sync option ServerSideApply: must be one of true, false (values are case-sensitive: did you mean "true"?)`,
		`invalid value "False" for sync option Validate: must be one of true, false (values are case-sensitive: did you mean "false"?)`,
		`invalid value "Background" for sync option PrunePropagationPolicy: must be one of foreground, background, orphan (values are case-sensitive: did you mean "background"?)`,
		`invalid value "CONFIRM" for sync option Prune: must be one of true, false, confirm (values are case-sensitive: did you mean "confirm"?)`,
		`unknown sync option "createNamespace" (keys are case-sensitive: did you mean "CreateNamespace"?)`,
	}, ValidateSyncOptions(v1alpha1.SyncOptions{"ServerSideApply=True", "Validate=False", "PrunePropagationPolicy=Background", "Prune=CONFIRM", "createNamespace=true"}))
}

// Every value ValidateSyncOptions accepts is one the sync engine recognizes verbatim: the
// accepted spellings are exactly "<Key>=<value>" with the constant key and value.
func TestValidateSyncOptionsAcceptsOnlyExactSpellings(t *testing.T) {
	t.Parallel()
	for key, values := range knownSyncOptions {
		for _, v := range values {
			exact := key + "=" + v
			assert.Empty(t, ValidateSyncOptions(v1alpha1.SyncOptions{exact}), exact)
			for _, variant := range []string{key + "=" + strings.ToUpper(v), key + "=" + strings.ToUpper(v[:1]) + v[1:], strings.ToLower(key) + "=" + v} {
				if variant != exact {
					assert.Len(t, ValidateSyncOptions(v1alpha1.SyncOptions{variant}), 1, variant)
				}
			}
		}
	}
}

func TestIssueCondition(t *testing.T) {
	t.Parallel()
	now := metav1.NewTime(time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC))
	c := Issue{Type: v1alpha1.ApplicationConditionInvalidSpecError, Reason: v1alpha1.ReasonInvalidSpec, Message: "m"}.Condition(3, now)
	assert.Equal(t, metav1.Condition{Type: "InvalidSpecError", Status: metav1.ConditionTrue, Reason: "InvalidSpec", Message: "m", ObservedGeneration: 3, LastTransitionTime: now}, c)
	require.Equal(t, []string{"m"}, messages([]Issue{{Message: "m"}}))
}
