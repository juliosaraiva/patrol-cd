package appset

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/appset/generators"
	"github.com/juliosaraiva/patrol-cd/internal/webhook"
)

var _ webhook.GeneratorEvaluator = (*Evaluator)(nil)

func TestGenerateApplications(t *testing.T) {
	t.Parallel()
	as := listAppSet(t, tenantNS, "set", elem("a", "dev"), elem("b", "prod"))
	as.Spec.Template.Namespace = "elsewhere"
	as.Spec.Template.Annotations = map[string]string{"note": "{{name}}"}
	// A second generator entry: Applications are concatenated in generator order.
	as.Spec.Generators = append(as.Spec.Generators, v1alpha1.ApplicationSetGenerator{List: &v1alpha1.ListGenerator{
		Elements: jsonList(t, elem("c", "dev")),
		Template: v1alpha1.ApplicationSetTemplate{Spec: v1alpha1.ApplicationSpec{Source: &v1alpha1.ApplicationSource{Path: "override/{{name}}"}}},
	}})
	set := generators.New(generators.Config{})
	apps, reason, err := GenerateApplications(context.Background(), nil, as, set)
	require.NoError(t, err)
	assert.Empty(t, reason)
	require.Len(t, apps, 3)
	names := []string{apps[0].Name, apps[1].Name, apps[2].Name}
	assert.Equal(t, []string{"a", "b", "c"}, names)
	for _, a := range apps {
		assert.Equal(t, tenantNS, a.Namespace, "the namespace is always the ApplicationSet's")
		assert.Equal(t, []string{v1alpha1.FinalizerResources}, a.Finalizers)
		assert.Equal(t, a.Name, a.Annotations["note"])
	}
	assert.Equal(t, "dev", apps[0].Labels["env"])
	assert.Equal(t, "apps/a", apps[0].Spec.Source.Path)
	assert.Equal(t, "override/c", apps[2].Spec.Source.Path, "generator-level template overrides the path")
	assert.Equal(t, "https://git.example/repo.git", apps[2].Spec.Source.RepoURL, "other fields come from spec.template")

	// preserveResourcesOnDeletion: no finalizer.
	as.Spec.SyncPolicy = &v1alpha1.ApplicationSetSyncPolicy{PreserveResourcesOnDeletion: true}
	apps, _, err = GenerateApplications(context.Background(), nil, as, set)
	require.NoError(t, err)
	assert.Empty(t, apps[0].Finalizers)
}

func TestGenerateApplicationsTemplatePatch(t *testing.T) {
	t.Parallel()
	as := listAppSet(t, controlNS, "set", elem("a", "dev"), elem("b", "prod"))
	as.Spec.GoTemplate = true
	as.Spec.Template.Name = "{{.name}}"
	as.Spec.Template.Labels = map[string]string{"env": "{{.env}}"}
	as.Spec.Template.Spec.Source.Path = "apps/{{.name}}"
	as.Spec.Template.Spec.Destination.Namespace = "{{.name}}"
	as.Spec.TemplatePatch = ptr.To(`{{- if eq .env "prod" }}
spec:
  project: escalated
  syncPolicy:
    automated:
      prune: true
{{- end }}`)
	apps, _, err := GenerateApplications(context.Background(), nil, as, generators.New(generators.Config{}))
	require.NoError(t, err)
	require.Len(t, apps, 2)
	assert.Nil(t, apps[0].Spec.SyncPolicy)
	require.NotNil(t, apps[1].Spec.SyncPolicy)
	assert.True(t, apps[1].Spec.SyncPolicy.Automated.GetPrune())
	assert.Equal(t, "default", apps[1].Spec.Project, "templatePatch cannot change the project")
}

func TestGenerateApplicationsErrors(t *testing.T) {
	t.Parallel()
	as := listAppSet(t, controlNS, "set", elem("a", "dev"))
	as.Spec.GoTemplate = true
	as.Spec.GoTemplateOptions = []string{"missingkey=error"}
	as.Spec.Template.Name = "{{.missing}}"
	apps, reason, err := GenerateApplications(context.Background(), nil, as, generators.New(generators.Config{}))
	require.Error(t, err)
	assert.Empty(t, apps)
	assert.Equal(t, v1alpha1.ApplicationSetReasonRenderTemplateParamsError, reason)

	// A failing generator does not stop the others; the first error and its reason are kept.
	as = listAppSet(t, controlNS, "set", elem("a", "dev"))
	as.Spec.Generators = append([]v1alpha1.ApplicationSetGenerator{{Git: &v1alpha1.GitGenerator{RepoURL: "https://x", Revision: "HEAD"}}}, as.Spec.Generators...)
	apps, reason, err = GenerateApplications(context.Background(), nil, as, generators.New(generators.Config{}))
	require.Error(t, err)
	assert.Equal(t, v1alpha1.ApplicationSetReasonApplicationParamsGenerationError, reason)
	require.Len(t, apps, 1)
	assert.Equal(t, "a", apps[0].Name)

	// An empty parameter set renders the template unchanged and adds no finalizer.
	as = listAppSet(t, controlNS, "set", map[string]any{})
	as.Spec.Template.Name = "static"
	apps, _, err = GenerateApplications(context.Background(), nil, as, generators.New(generators.Config{}))
	require.NoError(t, err)
	require.Len(t, apps, 1)
	assert.Equal(t, "static", apps[0].Name)
	assert.Empty(t, apps[0].Finalizers)
}

func TestCheckInvalidGenerators(t *testing.T) {
	t.Parallel()
	as := listAppSet(t, controlNS, "set", elem("a", "dev"))
	require.NoError(t, CheckInvalidGenerators(as))

	as.Spec.Generators = append(as.Spec.Generators, v1alpha1.ApplicationSetGenerator{})
	require.EqualError(t, CheckInvalidGenerators(as), "ApplicationSet set contains unrecognized generators")

	as.Annotations = map[string]string{LastAppliedConfigAnnotation: `{"spec":{"generators":[{"list":{}},{"lisst":{"elements":[]}}]}}`}
	require.EqualError(t, CheckInvalidGenerators(as), "ApplicationSet set contains unrecognized generators: lisst")

	as.Annotations[LastAppliedConfigAnnotation] = "{not json"
	require.EqualError(t, CheckInvalidGenerators(as), "ApplicationSet set contains unrecognized generators")

	// A selector-only entry is not flagged.
	as.Spec.Generators[1] = v1alpha1.ApplicationSetGenerator{Selector: &metav1.LabelSelector{}}
	require.NoError(t, CheckInvalidGenerators(as))
}

func TestValidateForAPI(t *testing.T) {
	t.Parallel()
	require.Error(t, ValidateForAPI(nil))
	as := listAppSet(t, controlNS, "set", elem("a", "dev"))
	require.NoError(t, ValidateForAPI(as))
	as.Spec.Template.Spec.Project = "{{project}}"
	require.ErrorIs(t, ValidateForAPI(as), ErrTemplatedProject)
	as.Spec.Generators = append(as.Spec.Generators, v1alpha1.ApplicationSetGenerator{})
	require.ErrorContains(t, ValidateForAPI(as), "unrecognized generators")
}

// TestProjectsCoverGeneratorTemplates: a generator-level template's project overrides the
// top-level one, so API admission must see (and RBAC-check) it too.
func TestProjectsCoverGeneratorTemplates(t *testing.T) {
	t.Parallel()
	as := listAppSet(t, controlNS, "set", elem("a", "dev"))
	as.Spec.Template.Spec.Project = "low"
	as.Spec.Generators[0].List.Template.Spec.Project = "admin"
	require.NoError(t, ValidateForAPI(as))
	projects, err := Projects(as)
	require.NoError(t, err)
	assert.Equal(t, []string{"admin", "low"}, projects)

	// Every generated Application lands in one of the reported projects.
	apps, err := NewAPIGenerator(generators.Config{Client: newFakeClient(t), Namespace: controlNS}).Generate(context.Background(), as)
	require.NoError(t, err)
	require.Len(t, apps, 1)
	assert.Equal(t, "admin", apps[0].Spec.Project)
	assert.Contains(t, projects, apps[0].Spec.Project)

	// A templated generator-level project is rejected like a templated top-level one.
	as.Spec.Generators[0].List.Template.Spec.Project = "{{env}}"
	require.ErrorIs(t, ValidateForAPI(as), ErrTemplatedProject)
	_, err = Projects(as)
	require.ErrorIs(t, err, ErrTemplatedProject)

	// Matrix and Merge templates apply; the templates of their children never do.
	child := &v1alpha1.ListGenerator{Elements: jsonList(t, map[string]any{"b": "1"})}
	child.Template.Spec.Project = "{{ignored}}"
	matrix := v1alpha1.ApplicationSetGenerator{Matrix: &v1alpha1.MatrixGenerator{Generators: []v1alpha1.ApplicationSetNestedGenerator{
		{List: &v1alpha1.ListGenerator{Elements: jsonList(t, map[string]any{"a": "1"})}},
		{List: child},
	}}}
	matrix.Matrix.Template.Spec.Project = "matrix-proj"
	merge := v1alpha1.ApplicationSetGenerator{Merge: &v1alpha1.MergeGenerator{MergeKeys: []string{"a"}}}
	merge.Merge.Template.Spec.Project = "merge-proj"
	as.Spec.Generators = []v1alpha1.ApplicationSetGenerator{matrix, merge}
	as.Spec.Template.Spec.Project = ""
	projects, err = Projects(as)
	require.NoError(t, err)
	assert.Equal(t, []string{"default", "matrix-proj", "merge-proj"}, projects)
	matrix.Matrix.Template.Spec.Project = "{{p}}"
	as.Spec.Generators[0] = matrix
	require.ErrorIs(t, ValidateForAPI(as), ErrTemplatedProject)
}

func TestAPIGenerator(t *testing.T) {
	t.Parallel()
	g := NewAPIGenerator(generators.Config{Client: newFakeClient(t), Namespace: controlNS})
	as := listAppSet(t, tenantNS, "set", elem("a", "dev"))
	apps, err := g.Generate(context.Background(), as)
	require.NoError(t, err)
	require.Len(t, apps, 1)
	assert.Equal(t, tenantNS, apps[0].Namespace)

	// SCM generators always run in tokenRef strict mode in the API.
	assert.True(t, g.cfg.SCM.TokenRefStrictMode)

	// Failures carry the captured generator logs.
	as.Spec.Generators = []v1alpha1.ApplicationSetGenerator{{Git: &v1alpha1.GitGenerator{RepoURL: "https://x", Revision: "HEAD"}}}
	_, err = g.Generate(context.Background(), as)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unable to generate Applications of ApplicationSet: ")
	assert.Contains(t, err.Error(), "error generating application from params")

	empty := listAppSet(t, tenantNS, "set")
	apps, err = g.Generate(context.Background(), empty)
	require.NoError(t, err)
	assert.NotNil(t, apps)
	assert.Empty(t, apps)
	_, err = g.Generate(context.Background(), nil)
	require.Error(t, err)
}

func TestEvaluator(t *testing.T) {
	t.Parallel()
	e := NewEvaluator(generators.Config{})
	as := listAppSet(t, controlNS, "set", elem("a", "dev"), elem("b", "prod"))
	params, err := e.GenerateParams(context.Background(), as, &as.Spec.Generators[0])
	require.NoError(t, err)
	require.Len(t, params, 2)
	gen := &v1alpha1.ApplicationSetGenerator{Git: &v1alpha1.GitGenerator{RepoURL: "https://git.example/{{name}}.git", Revision: "HEAD"}}
	out, err := e.InterpolateGenerator(as, gen, params[0])
	require.NoError(t, err)
	assert.Equal(t, "https://git.example/a.git", out.Git.RepoURL)
	_, err = e.GenerateParams(context.Background(), as, nil)
	require.Error(t, err)
}
