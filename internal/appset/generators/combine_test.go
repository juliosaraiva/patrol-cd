package generators

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

func listChild(t *testing.T, elems ...any) v1alpha1.ApplicationSetNestedGenerator {
	t.Helper()
	return v1alpha1.ApplicationSetNestedGenerator{List: &v1alpha1.ListGenerator{Elements: jsonElements(t, elems...)}}
}

func TestMatrixGenerator(t *testing.T) {
	t.Parallel()
	set := New(Config{})
	entry := v1alpha1.ApplicationSetGenerator{Matrix: &v1alpha1.MatrixGenerator{Generators: []v1alpha1.ApplicationSetNestedGenerator{
		listChild(t, map[string]any{"cluster": "east"}, map[string]any{"cluster": "west"}),
		// The second child is interpolated with each parameter set of the first.
		listChild(t, map[string]any{"app": "{{cluster}}-api"}, map[string]any{"app": "{{cluster}}-web"}),
	}}}
	got, err := generate(t, set, entry, appSet(false))
	require.NoError(t, err)
	assert.Equal(t, []map[string]any{
		{"cluster": "east", "app": "east-api"},
		{"cluster": "east", "app": "east-web"},
		{"cluster": "west", "app": "west-api"},
		{"cluster": "west", "app": "west-web"},
	}, got)

	// fasttemplate: a key present in both children with different values fails.
	entry.Matrix.Generators[1] = listChild(t, map[string]any{"cluster": "other"})
	_, err = generate(t, set, entry, appSet(false))
	require.ErrorContains(t, err, "found duplicate key cluster with different value")

	// goTemplate: the first child's values win (deep merge).
	entry.Matrix.Generators = []v1alpha1.ApplicationSetNestedGenerator{
		listChild(t, map[string]any{"cluster": "east", "meta": map[string]any{"a": "1"}}),
		listChild(t, map[string]any{"cluster": "other", "meta": map[string]any{"b": "2"}, "app": "{{.cluster}}"}),
	}
	got, err = generate(t, set, entry, appSet(true))
	require.NoError(t, err)
	assert.Equal(t, []map[string]any{{"cluster": "east", "meta": map[string]any{"a": "1", "b": "2"}, "app": "east"}}, got)
}

func TestMatrixGeneratorErrorsAndNesting(t *testing.T) {
	t.Parallel()
	set := New(Config{})
	one := v1alpha1.ApplicationSetGenerator{Matrix: &v1alpha1.MatrixGenerator{Generators: []v1alpha1.ApplicationSetNestedGenerator{listChild(t, map[string]any{"a": "1"})}}}
	_, err := generate(t, set, one, appSet(false))
	require.ErrorIs(t, err, ErrLessThanTwoGenerators)

	three := v1alpha1.ApplicationSetGenerator{Matrix: &v1alpha1.MatrixGenerator{Generators: []v1alpha1.ApplicationSetNestedGenerator{
		listChild(t, map[string]any{"a": "1"}), listChild(t, map[string]any{"b": "1"}), listChild(t, map[string]any{"c": "1"}),
	}}}
	_, err = generate(t, set, three, appSet(false))
	require.ErrorIs(t, err, ErrMoreThanTwoGenerators)

	double := listChild(t, map[string]any{"a": "1"})
	double.Clusters = &v1alpha1.ClusterGenerator{}
	twoInOne := v1alpha1.ApplicationSetGenerator{Matrix: &v1alpha1.MatrixGenerator{Generators: []v1alpha1.ApplicationSetNestedGenerator{double, listChild(t, map[string]any{"b": "1"})}}}
	set = New(Config{Client: fakeClient(t), Namespace: controlNS})
	_, err = generate(t, set, twoInOne, appSet(false))
	require.ErrorIs(t, err, ErrMoreThenOneInnerGenerators)

	empty := v1alpha1.ApplicationSetGenerator{Matrix: &v1alpha1.MatrixGenerator{Generators: []v1alpha1.ApplicationSetNestedGenerator{{}, listChild(t, map[string]any{"b": "1"})}}}
	_, err = generate(t, set, empty, appSet(false))
	require.ErrorContains(t, err, "child generator generated no parameters")

	// A matrix nested in a matrix (raw JSON) and a merge nested in a matrix.
	nested := v1alpha1.ApplicationSetGenerator{Matrix: &v1alpha1.MatrixGenerator{Generators: []v1alpha1.ApplicationSetNestedGenerator{
		listChild(t, map[string]any{"env": "prod"}),
		{Matrix: rawJSON(t, map[string]any{"generators": []any{
			map[string]any{"list": map[string]any{"elements": []any{map[string]any{"region": "eu"}}}},
			map[string]any{"list": map[string]any{"elements": []any{map[string]any{"name": "{{env}}-{{region}}"}}}},
		}})},
	}}}
	got, err := generate(t, set, nested, appSet(false))
	require.NoError(t, err)
	assert.Equal(t, []map[string]any{{"env": "prod", "region": "eu", "name": "prod-eu"}}, got)

	// Matrix/merge children of a nested matrix are rejected by the decoder (terminal only).
	tooDeep := v1alpha1.ApplicationSetGenerator{Matrix: &v1alpha1.MatrixGenerator{Generators: []v1alpha1.ApplicationSetNestedGenerator{
		listChild(t, map[string]any{"env": "prod"}),
		{Matrix: rawJSON(t, map[string]any{"generators": []any{map[string]any{"list": map[string]any{}}}})},
	}}}
	_, err = generate(t, set, tooDeep, appSet(false))
	require.ErrorContains(t, err, "want exactly 2")
}

func TestMergeGenerator(t *testing.T) {
	t.Parallel()
	set := New(Config{})
	entry := v1alpha1.ApplicationSetGenerator{Merge: &v1alpha1.MergeGenerator{
		MergeKeys: []string{"cluster"},
		Generators: []v1alpha1.ApplicationSetNestedGenerator{
			listChild(t, map[string]any{"cluster": "east", "size": "small"}, map[string]any{"cluster": "west", "size": "small"}),
			listChild(t, map[string]any{"cluster": "west", "size": "large"}, map[string]any{"cluster": "north", "size": "huge"}),
			listChild(t, map[string]any{"cluster": "east", "extra": "x"}),
		},
	}}
	got, err := generate(t, set, entry, appSet(false))
	require.NoError(t, err)
	assert.Equal(t, []map[string]any{
		{"cluster": "east", "size": "small", "extra": "x"},
		{"cluster": "west", "size": "large"},
	}, got, "base order kept, later children override, unmatched later sets dropped")

	// goTemplate deep merge.
	entry.Merge.Generators = []v1alpha1.ApplicationSetNestedGenerator{
		listChild(t, map[string]any{"cluster": "east", "values": map[string]any{"a": "1", "b": "1"}}),
		listChild(t, map[string]any{"cluster": "east", "values": map[string]any{"b": "2"}}),
	}
	got, err = generate(t, set, entry, appSet(true))
	require.NoError(t, err)
	assert.Equal(t, []map[string]any{{"cluster": "east", "values": map[string]any{"a": "1", "b": "2"}}}, got)

	entry.Merge.MergeKeys = nil
	_, err = generate(t, set, entry, appSet(true))
	require.ErrorIs(t, err, ErrNoMergeKeys)

	entry.Merge.MergeKeys = []string{"cluster"}
	entry.Merge.Generators[0] = listChild(t, map[string]any{"cluster": "east"}, map[string]any{"cluster": "east"})
	_, err = generate(t, set, entry, appSet(true))
	require.ErrorIs(t, err, ErrNonUniqueParamSets)

	entry.Merge.Generators = entry.Merge.Generators[:1]
	_, err = generate(t, set, entry, appSet(true))
	require.ErrorIs(t, err, ErrLessThanTwoGeneratorsInMerge)
}

func TestSelectorPostFilter(t *testing.T) {
	t.Parallel()
	set := New(Config{})
	entry := v1alpha1.ApplicationSetGenerator{
		List: &v1alpha1.ListGenerator{Elements: jsonElements(t,
			map[string]any{"env": "prod", "url": "https://x.example/a"},
			map[string]any{"env": "dev", "url": "https://x.example/b"},
		)},
		Selector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "url", Operator: metav1.LabelSelectorOpIn, Values: []string{"https://x.example/a"}}}},
	}
	got, err := generate(t, set, entry, appSet(false))
	require.NoError(t, err)
	require.Len(t, got, 1, "values are not validated as label values, so URLs can be matched")
	assert.Equal(t, "prod", got[0]["env"])

	// goTemplate: nested maps are flattened for the selector.
	entry.List.Elements = jsonElements(t, map[string]any{"meta": map[string]any{"env": "prod"}}, map[string]any{"meta": map[string]any{"env": "dev"}})
	entry.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"meta.env": "dev"}}
	got, err = generate(t, set, entry, appSet(true))
	require.NoError(t, err)
	require.Len(t, got, 1)

	// A selector on a matrix child filters that child.
	child := listChild(t, map[string]any{"b": "1"}, map[string]any{"b": "2"})
	child.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"b": "2"}}
	matrix := v1alpha1.ApplicationSetGenerator{Matrix: &v1alpha1.MatrixGenerator{Generators: []v1alpha1.ApplicationSetNestedGenerator{listChild(t, map[string]any{"a": "1"}), child}}}
	got, err = generate(t, set, matrix, appSet(false))
	require.NoError(t, err)
	assert.Equal(t, []map[string]any{{"a": "1", "b": "2"}}, got)

	entry.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"bad key!": "x"}}
	_, err = generate(t, set, entry, appSet(true))
	require.ErrorContains(t, err, "error parsing label selector")
}

func TestMultipleGeneratorsInOneEntry(t *testing.T) {
	t.Parallel()
	set := New(Config{Client: fakeClient(t, cluster("c1", "c1", "https://c1", nil, nil)), Namespace: controlNS})
	entry := v1alpha1.ApplicationSetGenerator{
		List:     &v1alpha1.ListGenerator{Elements: jsonElements(t, map[string]any{"name": "from-list"})},
		Clusters: &v1alpha1.ClusterGenerator{},
	}
	res, err := set.Transform(context.Background(), entry, appSet(false).Spec.Template, appSet(false), nil)
	require.NoError(t, err)
	require.Len(t, res, 2, "each generator of a top-level entry yields its own result")
	assert.Equal(t, "from-list", res[0].Params[0]["name"])
	assert.Len(t, res[1].Params, 2, "c1 and the local cluster")
}

func TestMinRequeueAfter(t *testing.T) {
	t.Parallel()
	set := New(Config{DefaultRequeueAfter: time.Minute})
	git := &v1alpha1.GitGenerator{RepoURL: "x", Revision: "HEAD"}
	plugin := &v1alpha1.PluginGenerator{}
	tests := []struct {
		name    string
		entries []v1alpha1.ApplicationSetGenerator
		want    time.Duration
	}{
		{"list only", []v1alpha1.ApplicationSetGenerator{{List: &v1alpha1.ListGenerator{}}}, NoRequeueAfter},
		{"git default from config", []v1alpha1.ApplicationSetGenerator{{Git: git}}, time.Minute},
		{"plugin default", []v1alpha1.ApplicationSetGenerator{{Plugin: plugin}}, DefaultPluginRequeueAfter},
		{"minimum", []v1alpha1.ApplicationSetGenerator{{Plugin: plugin}, {Git: git}}, time.Minute},
		{"zero disables", []v1alpha1.ApplicationSetGenerator{{Git: &v1alpha1.GitGenerator{RequeueAfterSeconds: ptr.To[int64](0)}}}, NoRequeueAfter},
		{"matrix minimum ignores zero", []v1alpha1.ApplicationSetGenerator{{Matrix: &v1alpha1.MatrixGenerator{Generators: []v1alpha1.ApplicationSetNestedGenerator{
			{Git: &v1alpha1.GitGenerator{RequeueAfterSeconds: ptr.To[int64](0)}},
			{Plugin: &v1alpha1.PluginGenerator{RequeueAfterSeconds: ptr.To[int64](90)}},
		}}}}, 90 * time.Second},
		{"merge", []v1alpha1.ApplicationSetGenerator{{Merge: &v1alpha1.MergeGenerator{Generators: []v1alpha1.ApplicationSetNestedGenerator{
			{List: &v1alpha1.ListGenerator{}}, {PullRequest: &v1alpha1.PullRequestGenerator{RequeueAfterSeconds: ptr.To[int64](5)}},
		}}}}, 5 * time.Second},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, set.MinRequeueAfter(tt.entries), tt.name)
	}
	assert.Equal(t, DefaultRequeueAfter, New(Config{}).MinRequeueAfter([]v1alpha1.ApplicationSetGenerator{{Git: git}}))
}

func TestParameterSetBound(t *testing.T) {
	t.Parallel()
	elems := func(key string, n int) v1alpha1.ApplicationSetNestedGenerator {
		list := make([]any, n)
		for i := range list {
			list[i] = map[string]any{key: fmt.Sprint(i)}
		}
		return listChild(t, list...)
	}
	matrix := func(a, b int) v1alpha1.ApplicationSetGenerator {
		return v1alpha1.ApplicationSetGenerator{Matrix: &v1alpha1.MatrixGenerator{
			Generators: []v1alpha1.ApplicationSetNestedGenerator{elems("a", a), elems("b", b)},
		}}
	}

	small := New(Config{MaxParameterSets: 4})
	got, err := generate(t, small, matrix(2, 2), appSet(false))
	require.NoError(t, err)
	assert.Len(t, got, 4)
	_, err = generate(t, small, matrix(2, 3), appSet(false))
	require.ErrorIs(t, err, ErrTooManyParameterSets, "the product is checked before it is built")
	list := v1alpha1.ApplicationSetGenerator{List: elems("a", 5).List}
	_, err = generate(t, small, list, appSet(false))
	require.ErrorIs(t, err, ErrTooManyParameterSets, "every generator is bounded")

	// Default: DefaultMaxParameterSets.
	_, err = generate(t, New(Config{}), matrix(200, 101), appSet(false))
	require.ErrorIs(t, err, ErrTooManyParameterSets)
	assert.ErrorContains(t, err, fmt.Sprintf("more than %d parameter sets", DefaultMaxParameterSets))

	// Negative: unbounded.
	got, err = generate(t, New(Config{MaxParameterSets: -1}), matrix(3, 3), appSet(false))
	require.NoError(t, err)
	assert.Len(t, got, 9)
}
