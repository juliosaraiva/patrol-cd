package generators

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

func TestListGenerator(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		goTemplate bool
		elements   []any
		yaml       string
		want       []map[string]any
		wantErr    string
	}{
		{
			name:     "fasttemplate strings and values",
			elements: []any{map[string]any{"cluster": "a", "url": "https://a", "values": map[string]any{"env": "prod"}}, map[string]any{"cluster": "b"}},
			want: []map[string]any{
				{"cluster": "a", "url": "https://a", "values.env": "prod"},
				{"cluster": "b"},
			},
		},
		{name: "fasttemplate number", elements: []any{map[string]any{"replicas": 3}}, wantErr: "error parsing value as string"},
		{name: "fasttemplate values not object", elements: []any{map[string]any{"values": "x"}}, wantErr: "error parsing values map"},
		{name: "fasttemplate values number", elements: []any{map[string]any{"values": map[string]any{"a": 1}}}, wantErr: "error parsing value as string"},
		{
			name:       "goTemplate keeps structure",
			goTemplate: true,
			elements:   []any{map[string]any{"cluster": map[string]any{"name": "a", "replicas": 3}}},
			want:       []map[string]any{{"cluster": map[string]any{"name": "a", "replicas": float64(3)}}},
		},
		{name: "empty element", elements: []any{map[string]any{}}, want: []map[string]any{{}}},
		{
			name:     "elementsYaml appended after elements",
			elements: []any{map[string]any{"cluster": "a"}},
			yaml:     "- cluster: b\n  replicas: 2\n- cluster: c\n",
			want: []map[string]any{
				{"cluster": "a"},
				{"cluster": "b", "replicas": float64(2)},
				{"cluster": "c"},
			},
		},
		{name: "elementsYaml invalid", yaml: "a: [", wantErr: "error unmarshling decoded ElementsYaml"},
		{name: "elementsYaml not a list", yaml: "a: b", wantErr: "error unmarshling decoded ElementsYaml"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gen := &v1alpha1.ApplicationSetGenerator{List: &v1alpha1.ListGenerator{ElementsYaml: tc.yaml}}
			if tc.elements != nil {
				gen.List.Elements = jsonElements(t, tc.elements...)
			}
			got, err := (&ListGenerator{}).GenerateParams(context.Background(), gen, appSet(tc.goTemplate))
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestClusterGenerator(t *testing.T) {
	t.Parallel()
	objs := []any{
		cluster("c1", "Staging_1", "https://staging", map[string]string{"env": "staging"}, map[string]string{"owner": "team"}),
		cluster("c2", "prod", "https://prod", map[string]string{"env": "prod", "tier": "1"}, nil),
		cluster("other-ns", "ignored", "https://ignored", nil, nil),
	}
	c := objs[2].(*v1alpha1.Cluster)
	c.Namespace = "elsewhere"
	cl := fakeClient(t, objs[0].(*v1alpha1.Cluster), objs[1].(*v1alpha1.Cluster), c)
	g := &ClusterGenerator{cfg: Config{Client: cl, Namespace: controlNS}}

	t.Run("fasttemplate with local cluster", func(t *testing.T) {
		gen := &v1alpha1.ApplicationSetGenerator{Clusters: &v1alpha1.ClusterGenerator{Values: map[string]string{"rev": "{{metadata.labels.env}}-branch"}}}
		got, err := g.GenerateParams(context.Background(), gen, appSet(false))
		require.NoError(t, err)
		assert.Equal(t, []map[string]any{
			{"name": "Staging_1", "nameNormalized": "staging-1", "server": "https://staging", "project": "",
				"metadata.labels.env": "staging", "metadata.annotations.owner": "team", "values.rev": "staging-branch"},
			{"name": "prod", "nameNormalized": "prod", "server": "https://prod", "project": "",
				"metadata.labels.env": "prod", "metadata.labels.tier": "1", "values.rev": "prod-branch"},
			{"name": "in-cluster", "nameNormalized": "in-cluster", "server": v1alpha1.KubernetesInternalAPIServerAddr, "project": "", "values.rev": "{{metadata.labels.env}}-branch"},
		}, got)
	})

	t.Run("goTemplate metadata maps", func(t *testing.T) {
		gen := &v1alpha1.ApplicationSetGenerator{Clusters: &v1alpha1.ClusterGenerator{
			Selector: metav1.LabelSelector{MatchLabels: map[string]string{"env": "prod"}},
			Values:   map[string]string{"tier": `{{ index .metadata.labels "tier" }}`},
		}}
		got, err := g.GenerateParams(context.Background(), gen, appSet(true))
		require.NoError(t, err)
		assert.Equal(t, []map[string]any{{
			"name": "prod", "nameNormalized": "prod", "server": "https://prod", "project": "",
			"metadata": map[string]any{"labels": map[string]string{"env": "prod", "tier": "1"}},
			"values":   map[string]string{"tier": "1"},
		}}, got, "a selector excludes the local cluster")
	})

	t.Run("selector expressions", func(t *testing.T) {
		tests := []struct {
			name string
			sel  metav1.LabelSelector
			want []string
		}{
			{name: "In", sel: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "env", Operator: metav1.LabelSelectorOpIn, Values: []string{"prod"}}}}, want: []string{"prod"}},
			{name: "NotIn excludes local", sel: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "env", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"prod"}}}}, want: []string{"Staging_1"}},
			{name: "Exists", sel: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "tier", Operator: metav1.LabelSelectorOpExists}}}, want: []string{"prod"}},
			{name: "DoesNotExist", sel: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "tier", Operator: metav1.LabelSelectorOpDoesNotExist}}}, want: []string{"Staging_1"}},
			{name: "secret-type label matches every cluster", sel: metav1.LabelSelector{MatchLabels: map[string]string{v1alpha1.LabelKeySecretType: v1alpha1.LabelValueSecretTypeCluster}}, want: []string{"Staging_1", "prod"}},
		}
		for _, tc := range tests {
			got, err := g.GenerateParams(context.Background(), &v1alpha1.ApplicationSetGenerator{Clusters: &v1alpha1.ClusterGenerator{Selector: tc.sel}}, appSet(false))
			require.NoError(t, err, tc.name)
			var names []string
			for _, p := range got {
				names = append(names, p["name"].(string))
			}
			assert.Equal(t, tc.want, names, tc.name)
		}
	})

	t.Run("flat list", func(t *testing.T) {
		gen := &v1alpha1.ApplicationSetGenerator{Clusters: &v1alpha1.ClusterGenerator{FlatList: true, Selector: metav1.LabelSelector{MatchLabels: map[string]string{"env": "none"}}}}
		got, err := g.GenerateParams(context.Background(), gen, appSet(true))
		require.NoError(t, err)
		require.Len(t, got, 1, "zero clusters still yield one parameter set")
		assert.Empty(t, got[0]["clusters"])

		gen.Clusters.Selector = metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "env", Operator: metav1.LabelSelectorOpExists}}}
		got, err = g.GenerateParams(context.Background(), gen, appSet(true))
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Len(t, got[0]["clusters"], 2)
	})

	t.Run("in-cluster registered", func(t *testing.T) {
		cl := fakeClient(t, cluster("local", "local", v1alpha1.KubernetesInternalAPIServerAddr+"/", nil, nil))
		g := &ClusterGenerator{cfg: Config{Client: cl, Namespace: controlNS}}
		got, err := g.GenerateParams(context.Background(), &v1alpha1.ApplicationSetGenerator{Clusters: &v1alpha1.ClusterGenerator{}}, appSet(false))
		require.NoError(t, err)
		require.Len(t, got, 1, "no synthetic duplicate")
		assert.Equal(t, "local", got[0]["name"])
	})

	t.Run("no clusters", func(t *testing.T) {
		g := &ClusterGenerator{cfg: Config{Client: fakeClient(t), Namespace: controlNS}}
		got, err := g.GenerateParams(context.Background(), &v1alpha1.ApplicationSetGenerator{Clusters: &v1alpha1.ClusterGenerator{}}, appSet(false))
		require.NoError(t, err)
		assert.Equal(t, []map[string]any{{"name": "in-cluster", "nameNormalized": "in-cluster", "server": v1alpha1.KubernetesInternalAPIServerAddr, "project": ""}}, got)
	})

	t.Run("invalid selector", func(t *testing.T) {
		gen := &v1alpha1.ApplicationSetGenerator{Clusters: &v1alpha1.ClusterGenerator{Selector: metav1.LabelSelector{MatchLabels: map[string]string{"env": "not valid!"}}}}
		_, err := g.GenerateParams(context.Background(), gen, appSet(false))
		require.ErrorContains(t, err, "error converting label selector")
	})
}
