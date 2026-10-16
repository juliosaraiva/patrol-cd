package applicationsets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/appset"
	"github.com/juliosaraiva/patrol-cd/internal/appset/generators"
)

func elements(t testing.TB, key string, n int) []apiextensionsv1.JSON {
	t.Helper()
	out := make([]apiextensionsv1.JSON, 0, n)
	for i := range n {
		b, err := json.Marshal(map[string]string{key: fmt.Sprintf("%s%d", key, i)})
		require.NoError(t, err)
		out = append(out, apiextensionsv1.JSON{Raw: b})
	}
	return out
}

func baseAppSet(name string) *v1alpha1.ApplicationSet {
	return &v1alpha1.ApplicationSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "patrol"},
		Spec: v1alpha1.ApplicationSetSpec{
			Template: v1alpha1.ApplicationSetTemplate{
				ApplicationSetTemplateMeta: v1alpha1.ApplicationSetTemplateMeta{Name: "{{a}}-{{b}}"},
				Spec: v1alpha1.ApplicationSpec{
					Project:     "team-a",
					Source:      &v1alpha1.ApplicationSource{RepoURL: "https://git.example.com/apps.git", Path: "{{a}}"},
					Destination: v1alpha1.ApplicationDestination{Server: "https://kubernetes.default.svc", Namespace: "{{b}}"},
				},
			},
		},
	}
}

// matrixAppSet returns an ApplicationSet with k generator entries, each a Matrix of two List
// generators of n elements: k*n*n Applications.
func matrixAppSet(t testing.TB, k, n int) *v1alpha1.ApplicationSet {
	t.Helper()
	as := baseAppSet("fanout")
	for range k {
		as.Spec.Generators = append(as.Spec.Generators, v1alpha1.ApplicationSetGenerator{Matrix: &v1alpha1.MatrixGenerator{
			Generators: []v1alpha1.ApplicationSetNestedGenerator{
				{List: &v1alpha1.ListGenerator{Elements: elements(t, "a", n)}},
				{List: &v1alpha1.ListGenerator{Elements: elements(t, "b", n)}},
			},
		}})
	}
	return as
}

var unlimited = GenerateLimits{MaxParameterSets: 1 << 40, MaxApplications: 1 << 40, MaxBytes: 1 << 50}

func defaultLimits() GenerateLimits {
	return GenerateLimits{MaxParameterSets: DefaultMaxGenerateParameterSets, MaxApplications: DefaultMaxGeneratedApplications, MaxBytes: DefaultMaxGenerateResponseBytes}
}

// TestBoundedGeneratorMatchesAppset checks that the API's bounded generator renders exactly
// what internal/appset renders for the controller and the unbounded API generator.
func TestBoundedGeneratorMatchesAppset(t *testing.T) {
	t.Parallel()
	patch := `metadata:
  annotations:
    patched: "{{a}}"
`
	withPatch := baseAppSet("patched")
	withPatch.Spec.Generators = []v1alpha1.ApplicationSetGenerator{{List: &v1alpha1.ListGenerator{Elements: elements(t, "a", 3)}}}
	withPatch.Spec.Template.Name = "{{a}}"
	withPatch.Spec.Template.Spec.Destination.Namespace = "x"
	withPatch.Spec.TemplatePatch = &patch

	goTmpl := baseAppSet("gotemplate")
	goTmpl.Spec.GoTemplate = true
	goTmpl.Spec.GoTemplateOptions = []string{"missingkey=error"}
	goTmpl.Spec.Template.Name = "{{ .a }}-{{ .b }}"
	goTmpl.Spec.Template.Spec.Source.Path = "{{ .a }}"
	goTmpl.Spec.Template.Spec.Destination.Namespace = "{{ .b | upper }}"
	goTmpl.Spec.SyncPolicy = &v1alpha1.ApplicationSetSyncPolicy{PreserveResourcesOnDeletion: true}
	goTmpl.Spec.Generators = matrixAppSet(t, 1, 3).Spec.Generators

	selected := matrixAppSet(t, 2, 4)
	selected.Name = "selected"
	selected.Spec.Generators[1].Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"a": "a1"}}
	selected.Spec.Generators[1].Matrix.Template.Spec.Project = "team-b"

	merge := baseAppSet("merge")
	merge.Spec.Generators = []v1alpha1.ApplicationSetGenerator{{Merge: &v1alpha1.MergeGenerator{
		MergeKeys: []string{"a"},
		Generators: []v1alpha1.ApplicationSetNestedGenerator{
			{List: &v1alpha1.ListGenerator{Elements: elements(t, "a", 3)}},
			{List: &v1alpha1.ListGenerator{Elements: []apiextensionsv1.JSON{{Raw: []byte(`{"a":"a1","b":"override"}`)}}}},
		},
	}}}
	merge.Spec.Template.Name = "{{a}}"

	broken := baseAppSet("broken")
	broken.Spec.Generators = []v1alpha1.ApplicationSetGenerator{
		{List: &v1alpha1.ListGenerator{Elements: elements(t, "a", 1)}},
		{Matrix: &v1alpha1.MatrixGenerator{Generators: []v1alpha1.ApplicationSetNestedGenerator{{List: &v1alpha1.ListGenerator{Elements: elements(t, "a", 1)}}}}},
	}

	cfg := generators.Config{Namespace: "patrol"}
	for _, as := range []*v1alpha1.ApplicationSet{withPatch, goTmpl, selected, merge, matrixAppSet(t, 3, 5), broken} {
		t.Run(as.Name, func(t *testing.T) {
			t.Parallel()
			want, wantErr := appset.NewAPIGenerator(cfg).Generate(context.Background(), as.DeepCopy())
			got, gotErr := NewGenerator(cfg).Generate(context.Background(), as.DeepCopy(), defaultLimits())
			if wantErr != nil {
				require.Error(t, gotErr)
				first := func(err error) string { return strings.SplitN(err.Error(), "\n", 2)[0] }
				require.Equal(t, first(wantErr), first(gotErr))
				return
			}
			require.NoError(t, gotErr)
			require.NotEmpty(t, got)
			require.Equal(t, want, got)
		})
	}
}

// heavy is the reviewer's fan-out: k Matrix(List x 141, List x 141) entries, 19,881
// Applications each. Unbounded, k=12 renders 238,572 Applications (several GB allocated).
const heavyEntries, heavyWidth = 12, 141

func measure(f func()) (time.Duration, uint64) {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	f()
	elapsed := time.Since(start)
	runtime.ReadMemStats(&after)
	return elapsed, after.TotalAlloc - before.TotalAlloc
}

// Not parallel: the allocation measurement must not include other tests.
func TestBoundedGeneratorStopsAtApplicationLimit(t *testing.T) {
	as := matrixAppSet(t, heavyEntries, heavyWidth)
	var (
		apps []v1alpha1.Application
		err  error
	)
	elapsed, alloc := measure(func() {
		apps, err = NewGenerator(generators.Config{}).Generate(context.Background(), as, defaultLimits())
	})
	t.Logf("application limit: %s, %d MB allocated", elapsed, alloc>>20)
	var le *LimitError
	require.ErrorAs(t, err, &le)
	require.Equal(t, "ApplicationSet fanout generates more than 5000 Applications, the limit of the API", err.Error())
	require.Nil(t, apps)
	// 5,001 Applications are rendered, not 238,572.
	require.Less(t, alloc, uint64(1<<30), "allocated %d MB", alloc>>20)
}

// Not parallel: the allocation measurement must not include other tests.
func TestBoundedGeneratorHonorsDeadline(t *testing.T) {
	as := matrixAppSet(t, heavyEntries, heavyWidth)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	var err error
	elapsed, alloc := measure(func() {
		_, err = NewGenerator(generators.Config{}).Generate(ctx, as, unlimited)
	})
	t.Logf("deadline: %s, %d MB allocated", elapsed, alloc>>20)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	// Stops at the deadline (plus at most one generator call or one rendering), long before
	// the 238,572 Applications are rendered.
	require.Less(t, elapsed, 5*time.Second)
	require.Less(t, alloc, uint64(2<<30), "allocated %d MB", alloc>>20)
}

func TestBoundedGeneratorLimits(t *testing.T) {
	t.Parallel()
	big := baseAppSet("big")
	big.Spec.Template.Annotations = map[string]string{"pad": strings.Repeat("x", 1024)}
	big.Spec.Generators = []v1alpha1.ApplicationSetGenerator{{List: &v1alpha1.ListGenerator{Elements: elements(t, "a", 10)}}}
	big.Spec.Template.Name = "{{a}}"

	// Two entries of 2,500 parameter sets each that the post-selector filters out entirely:
	// no Application is rendered, the parameter sets still count.
	filtered := matrixAppSet(t, 2, 50)
	filtered.Name = "filtered"
	for i := range filtered.Spec.Generators {
		filtered.Spec.Generators[i].Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"a": "none"}}
	}

	cases := []struct {
		name   string
		as     *v1alpha1.ApplicationSet
		limits GenerateLimits
		want   string
	}{
		{"bytes", big, GenerateLimits{MaxBytes: 4096}, "the Applications generated by ApplicationSet big exceed the response limit of 4096 bytes"},
		{"applications", big, GenerateLimits{MaxApplications: 3}, "ApplicationSet big generates more than 3 Applications, the limit of the API"},
		{"parameter sets across entries", filtered, GenerateLimits{MaxParameterSets: 4000}, "the generators of ApplicationSet filtered produce more than 4000 parameter sets, the limit of the API"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewGenerator(generators.Config{}).Generate(context.Background(), tc.as.DeepCopy(), tc.limits)
			var le *LimitError
			require.ErrorAs(t, err, &le)
			require.Equal(t, tc.want, err.Error())
		})
	}

	// Within the limits, the filtered ApplicationSet renders nothing.
	apps, err := NewGenerator(generators.Config{}).Generate(context.Background(), filtered.DeepCopy(), GenerateLimits{MaxParameterSets: 5000})
	require.NoError(t, err)
	require.Empty(t, apps)

	// The per-generator bound follows the limit: one Matrix product larger than it fails while
	// it is built (generator error, not a limit error).
	_, err = NewGenerator(generators.Config{}).Generate(context.Background(), matrixAppSet(t, 1, 50), GenerateLimits{MaxParameterSets: 1000})
	require.ErrorIs(t, err, generators.ErrTooManyParameterSets)
	require.False(t, errors.As(err, new(*LimitError)))
	require.True(t, strings.HasPrefix(err.Error(), "unable to generate Applications of ApplicationSet: "), err.Error())

	_, err = NewGenerator(generators.Config{}).Generate(context.Background(), nil, defaultLimits())
	require.EqualError(t, err, "error creating ApplicationSets: ApplicationSets is nil in request")
}

func TestBoundedBuffer(t *testing.T) {
	t.Parallel()
	b := &boundedBuffer{max: 5}
	n, err := b.Write([]byte("abc"))
	require.NoError(t, err)
	require.Equal(t, 3, n)
	n, _ = b.Write([]byte("defgh"))
	require.Equal(t, 5, n, "reports the whole write")
	require.Equal(t, "abcde", b.String())
}

func TestRBACObjects(t *testing.T) {
	t.Parallel()
	s := New(Deps{Config: Config{Namespace: "patrol"}})
	as := baseAppSet("set")
	require.Equal(t, []string{"team-a/set"}, s.objects(as))

	multi := matrixAppSet(t, 2, 1)
	multi.Name = "set"
	multi.Spec.Generators[1].Matrix.Template.Spec.Project = "team-b"
	require.Equal(t, []string{"team-a/set", "team-b/set"}, s.objects(multi))
	multi.Namespace = "tenant"
	require.Equal(t, []string{"team-a/tenant/set", "team-b/tenant/set"}, s.objects(multi))

	// A templated project (kubectl) fails closed: only a grant on every project matches.
	templated := baseAppSet("set")
	templated.Spec.Template.Spec.Project = "{{.p}}"
	require.Equal(t, []string{"*/set"}, s.objects(templated))
	genTemplated := matrixAppSet(t, 1, 1)
	genTemplated.Name = "set"
	genTemplated.Spec.Generators[0].Matrix.Template.Spec.Project = "{{p}}"
	require.Equal(t, []string{"*/set"}, s.objects(genTemplated))
}
