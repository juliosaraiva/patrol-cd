package template

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

func baseTemplate() v1alpha1.ApplicationSetTemplate {
	return v1alpha1.ApplicationSetTemplate{
		ApplicationSetTemplateMeta: v1alpha1.ApplicationSetTemplateMeta{
			Name:        "{{cluster}}-guestbook",
			Namespace:   "elsewhere",
			Labels:      map[string]string{"env": "{{env}}"},
			Annotations: map[string]string{"{{annotationKey}}": "{{cluster}}"},
		},
		Spec: v1alpha1.ApplicationSpec{
			Project: "default",
			Source: &v1alpha1.ApplicationSource{
				RepoURL:        "https://github.com/example/apps.git",
				Path:           "guestbook/{{env}}",
				TargetRevision: "HEAD",
			},
			Destination: v1alpha1.ApplicationDestination{Server: "{{url}}", Namespace: "guestbook"},
		},
	}
}

func TestRenderApplicationFasttemplate(t *testing.T) {
	t.Parallel()
	tmpl := ApplicationFromTemplate(baseTemplate())
	params := map[string]any{"cluster": "prod", "env": "production", "url": "https://prod", "annotationKey": "example.com/cluster"}
	app, err := RenderApplication(tmpl, nil, params, Options{})
	require.NoError(t, err)
	assert.Equal(t, "prod-guestbook", app.Name)
	assert.Equal(t, "elsewhere", app.Namespace, "the caller overrides the namespace")
	assert.Equal(t, map[string]string{"env": "production"}, app.Labels)
	assert.Equal(t, map[string]string{"example.com/cluster": "prod"}, app.Annotations, "annotation keys are templated")
	assert.Equal(t, "guestbook/production", app.Spec.Source.Path)
	assert.Equal(t, "https://prod", app.Spec.Destination.Server)
	assert.Equal(t, []string{v1alpha1.FinalizerResources}, app.Finalizers)
	// The template is not modified.
	assert.Equal(t, "{{cluster}}-guestbook", tmpl.Name)
}

func TestRenderApplicationGoTemplate(t *testing.T) {
	t.Parallel()
	tmpl := ApplicationFromTemplate(v1alpha1.ApplicationSetTemplate{
		ApplicationSetTemplateMeta: v1alpha1.ApplicationSetTemplateMeta{
			Name:        "{{.cluster.name}}",
			Annotations: map[string]string{"{{ .key }}": "v"},
		},
		Spec: v1alpha1.ApplicationSpec{
			Project: "{{.project}}",
			Source: &v1alpha1.ApplicationSource{
				RepoURL: "https://example.com/r.git",
				Helm: &v1alpha1.ApplicationSourceHelm{
					ValuesObject: &runtime.RawExtension{Raw: []byte(`{"replicas": 3, "name": "{{.cluster.name}}", "nested": {"{{.key}}": ["{{.cluster.name}}", true]}}`)},
					Parameters:   []v1alpha1.HelmParameter{{Name: "image.tag", Value: "{{.tag}}", ForceString: true}},
				},
			},
			Destination: v1alpha1.ApplicationDestination{Name: "{{.cluster.name}}", Namespace: "ns"},
		},
	})
	params := map[string]any{"cluster": map[string]any{"name": "c1"}, "key": "k", "project": "team", "tag": "1.2.3"}
	app, err := RenderApplication(tmpl, &v1alpha1.ApplicationSetSyncPolicy{PreserveResourcesOnDeletion: true}, params, Options{GoTemplate: true})
	require.NoError(t, err)
	assert.Equal(t, "c1", app.Name)
	assert.Equal(t, map[string]string{"k": "v"}, app.Annotations)
	assert.Equal(t, "team", app.Spec.Project)
	assert.Equal(t, "c1", app.Spec.Destination.Name)
	assert.Equal(t, "1.2.3", app.Spec.Source.Helm.Parameters[0].Value)
	assert.True(t, app.Spec.Source.Helm.Parameters[0].ForceString, "non-string scalars are kept")
	var values map[string]any
	require.NoError(t, json.Unmarshal(app.Spec.Source.Helm.ValuesObject.Raw, &values))
	assert.Equal(t, map[string]any{"replicas": float64(3), "name": "c1", "nested": map[string]any{"k": []any{"c1", true}}}, values)
	assert.Empty(t, app.Finalizers, "preserveResourcesOnDeletion adds no finalizer")
}

func TestRenderApplicationValuesObjectYAML(t *testing.T) {
	t.Parallel()
	tmpl := ApplicationFromTemplate(v1alpha1.ApplicationSetTemplate{
		ApplicationSetTemplateMeta: v1alpha1.ApplicationSetTemplateMeta{Name: "a"},
		Spec: v1alpha1.ApplicationSpec{Source: &v1alpha1.ApplicationSource{Helm: &v1alpha1.ApplicationSourceHelm{
			ValuesObject: &runtime.RawExtension{Raw: []byte("name: '{{x}}'\nlist:\n- '{{x}}'\n")},
		}}},
	})
	app, err := RenderApplication(tmpl, nil, map[string]any{"x": "y"}, Options{})
	require.NoError(t, err)
	assert.JSONEq(t, `{"name":"y","list":["y"]}`, string(app.Spec.Source.Helm.ValuesObject.Raw))
}

func TestRenderApplicationFinalizers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		finalizers []string
		policy     *v1alpha1.ApplicationSetSyncPolicy
		params     map[string]any
		want       []string
	}{
		{name: "default adds resources finalizer", params: map[string]any{"a": "b"}, want: []string{v1alpha1.FinalizerResources}},
		{name: "sync policy without preserve", policy: &v1alpha1.ApplicationSetSyncPolicy{}, params: map[string]any{"a": "b"}, want: []string{v1alpha1.FinalizerResources}},
		{name: "preserve adds nothing", policy: &v1alpha1.ApplicationSetSyncPolicy{PreserveResourcesOnDeletion: true}, params: map[string]any{"a": "b"}},
		{name: "custom finalizers verbatim", finalizers: []string{"example.com/x"}, params: map[string]any{"a": "b"}, want: []string{"example.com/x"}},
		{name: "empty params render nothing", params: map[string]any{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tmpl := &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: "x", Finalizers: tc.finalizers}}
			app, err := RenderApplication(tmpl, tc.policy, tc.params, Options{})
			require.NoError(t, err)
			assert.Equal(t, tc.want, app.Finalizers)
		})
	}
}

func TestRenderApplicationErrors(t *testing.T) {
	t.Parallel()
	_, err := RenderApplication(nil, nil, map[string]any{"a": "b"}, Options{})
	require.ErrorContains(t, err, "application template is empty")

	tmpl := &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: "{{.missing}}"}}
	_, err = RenderApplication(tmpl, nil, map[string]any{"a": "b"}, Options{GoTemplate: true, GoTemplateOptions: []string{"missingkey=error"}})
	require.ErrorContains(t, err, "map has no entry for key")

	tmpl = &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: "{{a}} {{b}"}}
	_, err = RenderApplication(tmpl, nil, map[string]any{"a": "b"}, Options{})
	require.ErrorContains(t, err, "invalid template")
}

func TestApplyTemplatePatch(t *testing.T) {
	t.Parallel()
	app, err := RenderApplication(ApplicationFromTemplate(baseTemplate()), nil,
		map[string]any{"cluster": "c", "env": "e", "url": "https://u", "annotationKey": "k"}, Options{})
	require.NoError(t, err)

	// Upstream Template.md templatePatch example.
	patch := `spec:
  project: escalated
  source:
    helm:
      valueFiles:
      {{- range $valueFile := .valueFiles }}
        - {{ $valueFile }}
      {{- end }}
{{- if .autoSync }}
  syncPolicy:
    automated:
      prune: {{ .prune }}
{{- end }}
`
	params := map[string]any{"valueFiles": []any{"values.yaml", "prod.yaml"}, "autoSync": true, "prune": true}
	patched, err := ApplyTemplatePatch(app, patch, params, Options{GoTemplate: true})
	require.NoError(t, err)
	assert.Equal(t, []string{"values.yaml", "prod.yaml"}, patched.Spec.Source.Helm.ValueFiles)
	require.NotNil(t, patched.Spec.SyncPolicy)
	assert.Equal(t, ptr.To(true), patched.Spec.SyncPolicy.Automated.Prune)
	assert.Equal(t, "default", patched.Spec.Project, "the project cannot be patched")
	assert.Equal(t, "c-guestbook", patched.Name)
	assert.Equal(t, "guestbook/e", patched.Spec.Source.Path, "unpatched fields are kept")

	// JSON patches are accepted as is.
	patched, err = ApplyTemplatePatch(app, `{"metadata":{"labels":{"extra":"{{cluster}}"}}}`, map[string]any{"cluster": "c"}, Options{})
	require.NoError(t, err)
	assert.Equal(t, "c", patched.Labels["extra"])
	assert.Equal(t, "e", patched.Labels["env"])

	// A patch with an empty spec clears it (upstream regression #17040 behavior).
	patched, err = ApplyTemplatePatch(app, "spec:\n", nil, Options{GoTemplate: true})
	require.NoError(t, err)
	assert.Nil(t, patched.Spec.Source)
	assert.Equal(t, "default", patched.Spec.Project)
}

func TestApplyTemplatePatchErrors(t *testing.T) {
	t.Parallel()
	app := &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: "a"}}
	tests := []struct {
		name  string
		patch string
		opts  Options
		want  string
	}{
		{name: "render", patch: "{{ .x ", opts: Options{GoTemplate: true}, want: "error replacing values in templatePatch"},
		{name: "yaml", patch: "a: [", want: "error while converting template to json"},
		{name: "type mismatch", patch: "spec:\n  project: [1]\n", want: "invalid templatePatch"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := ApplyTemplatePatch(app, tc.patch, map[string]any{"a": "b"}, tc.opts)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestInterpolateGenerator(t *testing.T) {
	t.Parallel()
	gen := &v1alpha1.ApplicationSetGenerator{
		Clusters: &v1alpha1.ClusterGenerator{
			Selector: metav1.LabelSelector{MatchLabels: map[string]string{"env": "{{.path.basename}}", "{{.labelKey}}": "x"}},
			Values: map[string]string{
				"fromFirst":  "{{.path.basename}}",
				"fromSecond": "{{.name}}",
				"mixed":      "{{.path.basename}}-{{.name}}",
			},
		},
		Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"x": "{{.path.basename}}"}},
	}
	params := map[string]any{"path": map[string]any{"basename": "dev"}, "labelKey": "team"}
	out, err := InterpolateGenerator(gen, params, Options{GoTemplate: true, GoTemplateOptions: []string{"missingkey=default"}})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"env": "dev", "team": "x"}, out.Clusters.Selector.MatchLabels)
	assert.Equal(t, map[string]string{
		"fromFirst":  "dev",
		"fromSecond": "{{.name}}",
		"mixed":      "{{.path.basename}}-{{.name}}",
	}, out.Clusters.Values, "values referencing missing keys are kept for later resolution")
	assert.Equal(t, "dev", out.Selector.MatchLabels["x"])
	assert.Equal(t, "{{.path.basename}}", gen.Clusters.Values["fromFirst"], "the input is not modified")
}

func TestInterpolateGeneratorNested(t *testing.T) {
	t.Parallel()
	nested := apiextensionsv1.JSON{Raw: []byte(`{"generators":[{"list":{"elements":[{"x":"{{a}}"}]}},{"git":{"repoURL":"{{a}}","revision":"HEAD","values":{"v":"{{a}}-{{b}}"}}}]}`)}
	gen := &v1alpha1.ApplicationSetGenerator{Matrix: &v1alpha1.MatrixGenerator{Generators: []v1alpha1.ApplicationSetNestedGenerator{
		{List: &v1alpha1.ListGenerator{Elements: []apiextensionsv1.JSON{{Raw: []byte(`{"cluster":"{{a}}","n":null}`)}}}},
		{Matrix: &nested},
	}}}
	out, err := InterpolateGenerator(gen, map[string]any{"a": "A"}, Options{})
	require.NoError(t, err)
	assert.JSONEq(t, `{"cluster":"A"}`, string(out.Matrix.Generators[0].List.Elements[0].Raw), "null values are dropped as upstream")
	m, err := v1alpha1.ToNestedMatrixGenerator(out.Matrix.Generators[1].Matrix)
	require.NoError(t, err)
	assert.JSONEq(t, `{"x":"A"}`, string(m.Generators[0].List.Elements[0].Raw))
	assert.Equal(t, "A", m.Generators[1].Git.RepoURL)
	assert.Equal(t, map[string]string{"v": "A-{{b}}"}, m.Generators[1].Git.Values)

	same, err := InterpolateGenerator(gen, nil, Options{})
	require.NoError(t, err)
	assert.Equal(t, gen, same)

	_, err = InterpolateGenerator(&v1alpha1.ApplicationSetGenerator{Git: &v1alpha1.GitGenerator{RepoURL: "{{ .a ", Values: map[string]string{"x": "{{ fail }}"}}},
		map[string]any{"a": "b"}, Options{GoTemplate: true})
	require.ErrorContains(t, err, "failed to replace parameters in generator")
}

func TestMergeTemplate(t *testing.T) {
	t.Parallel()
	base := baseTemplate()
	base.Labels["shared"] = "base"
	base.Spec.SyncPolicy = &v1alpha1.SyncPolicy{SyncOptions: v1alpha1.SyncOptions{"CreateNamespace=true"}, Automated: &v1alpha1.SyncPolicyAutomated{Prune: ptr.To(true)}}
	gen := &v1alpha1.ApplicationSetTemplate{
		ApplicationSetTemplateMeta: v1alpha1.ApplicationSetTemplateMeta{
			Labels: map[string]string{"shared": "generator", "own": "x"},
		},
		Spec: v1alpha1.ApplicationSpec{
			Source:     &v1alpha1.ApplicationSource{Path: "override"},
			SyncPolicy: &v1alpha1.SyncPolicy{Automated: &v1alpha1.SyncPolicyAutomated{SelfHeal: ptr.To(true), Prune: ptr.To(false)}},
		},
	}
	merged := MergeTemplate(gen, base)
	assert.Equal(t, "override", merged.Spec.Source.Path, "generator fields win")
	assert.Equal(t, base.Spec.Source.RepoURL, merged.Spec.Source.RepoURL, "empty fields come from the ApplicationSet template")
	assert.Equal(t, base.Name, merged.Name)
	assert.Equal(t, map[string]string{"env": "{{env}}", "shared": "generator", "own": "x"}, merged.Labels)
	assert.Equal(t, v1alpha1.SyncOptions{"CreateNamespace=true"}, merged.Spec.SyncPolicy.SyncOptions)
	assert.Equal(t, ptr.To(true), merged.Spec.SyncPolicy.Automated.SelfHeal)
	assert.Equal(t, ptr.To(true), merged.Spec.SyncPolicy.Automated.Prune, "a zero value cannot unset the base value (mergo semantics)")
	assert.Equal(t, "override", gen.Spec.Source.Path)
	assert.Empty(t, gen.Spec.Source.RepoURL, "inputs are not modified")

	empty := MergeTemplate(nil, base)
	assert.Equal(t, base, empty)
}
