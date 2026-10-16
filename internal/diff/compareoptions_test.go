package diff

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"pgregory.net/rapid"
)

func TestAnnotationOptions(t *testing.T) {
	t.Parallel()
	key := DefaultCompareOptionsAnnotation
	cases := []struct {
		name  string
		value *string
		want  []string
	}{
		{"absent", nil, nil},
		{"empty", ptrTo(""), nil},
		{"single", ptrTo("IgnoreExtraneous"), []string{"IgnoreExtraneous"}},
		{"trims, drops empties and duplicates", ptrTo(" ServerSideDiff=true ,, IncludeMutationWebhook=true,ServerSideDiff=true "),
			[]string{"ServerSideDiff=true", "IncludeMutationWebhook=true"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ann := map[string]string{}
			if tc.value != nil {
				ann[key] = *tc.value
			}
			assert.Equal(t, tc.want, AnnotationOptions(ann, key))
		})
	}
	assert.True(t, HasAnnotationOption(map[string]string{DefaultSyncOptionsAnnotation: "Prune=false, ServerSideApply=true"}, DefaultSyncOptionsAnnotation, "ServerSideApply=true"))
	assert.False(t, HasAnnotationOption(map[string]string{DefaultSyncOptionsAnnotation: "ServerSideApply=truex"}, DefaultSyncOptionsAnnotation, "ServerSideApply=true"))
}

func ptrTo[T any](v T) *T { return &v }

func TestParseCompareAnnotation(t *testing.T) {
	t.Parallel()
	key := DefaultCompareOptionsAnnotation
	cases := []struct {
		name          string
		value         string
		controllerSSD bool
		want          CompareAnnotation
		wantSSD       bool
	}{
		{name: "empty uses controller default off", value: "", want: CompareAnnotation{}, wantSSD: false},
		{name: "empty uses controller default on", value: "", controllerSSD: true, want: CompareAnnotation{}, wantSSD: true},
		{name: "ignore extraneous", value: "IgnoreExtraneous", want: CompareAnnotation{IgnoreExtraneous: true}},
		{name: "enable ssd", value: "ServerSideDiff=true", want: CompareAnnotation{ServerSideDiff: ptrTo(true)}, wantSSD: true},
		{name: "disable ssd overrides controller", value: "ServerSideDiff=false", controllerSSD: true, want: CompareAnnotation{ServerSideDiff: ptrTo(false)}, wantSSD: false},
		{name: "false wins over true", value: "ServerSideDiff=true,ServerSideDiff=false", want: CompareAnnotation{ServerSideDiff: ptrTo(false)}, wantSSD: false},
		{name: "include mutation webhook", value: "ServerSideDiff=true, IncludeMutationWebhook=true", want: CompareAnnotation{ServerSideDiff: ptrTo(true), IncludeMutationWebhook: true}, wantSSD: true},
		{name: "unknown options ignored", value: "Whatever=1, IgnoreExtraneous", want: CompareAnnotation{IgnoreExtraneous: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ParseCompareAnnotation(map[string]string{key: tc.value}, key)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.wantSSD, got.ServerSideDiffEnabled(tc.controllerSSD))
		})
	}
}

func TestIgnoreStatusMode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in     string
		want   IgnoreStatusMode
		wantOK bool
	}{
		{"", IgnoreStatusAll, true}, {"all", IgnoreStatusAll, true}, {"crd", IgnoreStatusCRD, true},
		{"none", IgnoreStatusNone, true}, {"off", IgnoreStatusNone, true}, {"false", IgnoreStatusNone, true},
		{"bogus", IgnoreStatusAll, false},
	}
	for _, tc := range cases {
		got, ok := ParseIgnoreStatusMode(tc.in)
		assert.Equal(t, tc.want, got, tc.in)
		assert.Equal(t, tc.wantOK, ok, tc.in)
	}
}

func TestStatusIgnoreRules(t *testing.T) {
	t.Parallel()
	crd := `{"apiVersion":"apiextensions.k8s.io/v1","kind":"CustomResourceDefinition","metadata":{"name":"x"},"spec":{"group":"g"},"status":{"acceptedNames":{"kind":"X"}}}`
	cm := `{"apiVersion":"example.com/v1","kind":"Widget","metadata":{"name":"w"},"status":{"phase":"Ready"}}`
	hasStatus := func(mode IgnoreStatusMode, manifest string) bool {
		n, err := NewIgnoreNormalizer(SystemIgnoreRules(nil, CompareOptions{IgnoreResourceStatusField: mode}), IgnoreNormalizerOptions{})
		require.NoError(t, err)
		u := obj(t, manifest)
		require.NoError(t, n.Normalize(context.Background(), u))
		_, ok := u.Object["status"]
		return ok
	}
	assert.False(t, hasStatus(IgnoreStatusAll, crd))
	assert.False(t, hasStatus(IgnoreStatusAll, cm))
	assert.False(t, hasStatus(IgnoreStatusCRD, crd))
	assert.True(t, hasStatus(IgnoreStatusCRD, cm))
	assert.True(t, hasStatus(IgnoreStatusNone, crd))
	assert.True(t, hasStatus(IgnoreStatusNone, cm))
	assert.Equal(t, DefaultCompareOptions().IgnoreResourceStatusField, IgnoreStatusAll)

	// With the default options a status difference is not drift; with "none" it is.
	live := obj(t, cm)
	config := obj(t, cm)
	require.NoError(t, unstructured.SetNestedField(config.Object, "Pending", "status", "phase"))
	for mode, want := range map[IgnoreStatusMode]bool{IgnoreStatusAll: false, IgnoreStatusNone: true} {
		n, err := NewNormalizer(NormalizerConfig{Rules: SystemIgnoreRules(nil, CompareOptions{IgnoreResourceStatusField: mode})})
		require.NoError(t, err)
		assert.Equal(t, want, mustDiff(t, config, live, Options{Normalizer: n}).Modified, mode)
	}
}

func TestResourceUpdateRulesAndHash(t *testing.T) {
	t.Parallel()
	diffRules := []IgnoreRule{{Group: "apps", Kind: "Deployment", JSONPointers: []string{"/spec/replicas"}}}
	updateRules := []IgnoreRule{{Group: "*", Kind: "*", JSONPointers: []string{"/status"}}}

	base := obj(t, deployLive)
	scaled := base.DeepCopy()
	require.NoError(t, unstructured.SetNestedField(scaled.Object, int64(5), "spec", "replicas"))
	require.NoError(t, unstructured.SetNestedField(scaled.Object, "999", "metadata", "resourceVersion"))
	require.NoError(t, unstructured.SetNestedField(scaled.Object, int64(9), "status", "replicas"))

	hash := func(rules []IgnoreRule, u *unstructured.Unstructured) string {
		n, err := NewIgnoreNormalizer(rules, IgnoreNormalizerOptions{})
		require.NoError(t, err)
		before := u.DeepCopy()
		h, err := ResourceUpdateHash(context.Background(), u, n)
		require.NoError(t, err)
		assert.Equal(t, before, u, "input must not be modified")
		return h
	}

	with := ResourceUpdateIgnoreRules(updateRules, diffRules, CompareOptions{IgnoreDifferencesOnResourceUpdates: true})
	assert.Equal(t, hash(with, base), hash(with, scaled), "replicas, status and resourceVersion are all ignored")

	without := ResourceUpdateIgnoreRules(updateRules, diffRules, CompareOptions{IgnoreDifferencesOnResourceUpdates: false})
	assert.NotEqual(t, hash(without, base), hash(without, scaled), "replicas change is relevant when diff rules are not reused")

	onlyRV := base.DeepCopy()
	require.NoError(t, unstructured.SetNestedField(onlyRV.Object, "1000", "metadata", "resourceVersion"))
	require.NoError(t, unstructured.SetNestedField(onlyRV.Object, int64(4), "metadata", "generation"))
	assert.Equal(t, hash(without, base), hash(without, onlyRV), "metadata noise is always ignored")

	// The /status rule implied by ignoreResourceStatusField is part of the update rules when
	// ignoreDifferences are reused, whether or not the caller passed it in diffRules.
	statusOnly := base.DeepCopy()
	require.NoError(t, unstructured.SetNestedField(statusOnly.Object, int64(7), "status", "readyReplicas"))
	for _, rules := range [][]IgnoreRule{diffRules, SystemIgnoreRules(diffRules, DefaultCompareOptions())} {
		r := ResourceUpdateIgnoreRules(nil, rules, DefaultCompareOptions())
		assert.Equal(t, hash(r, base), hash(r, statusOnly), "status-only change is not relevant by default")
	}
	crdOnly := DefaultCompareOptions()
	crdOnly.IgnoreResourceStatusField = IgnoreStatusCRD
	assert.NotEqual(t, hash(ResourceUpdateIgnoreRules(nil, diffRules, crdOnly), base),
		hash(ResourceUpdateIgnoreRules(nil, diffRules, crdOnly), statusOnly), "status of a Deployment is relevant with mode crd")
	noReuse := DefaultCompareOptions()
	noReuse.IgnoreDifferencesOnResourceUpdates = false
	assert.NotEqual(t, hash(ResourceUpdateIgnoreRules(nil, diffRules, noReuse), base),
		hash(ResourceUpdateIgnoreRules(nil, diffRules, noReuse), statusOnly), "status is relevant when ignoreDifferences are not reused")

	_, err := ResourceUpdateHash(context.Background(), nil, nil)
	require.ErrorIs(t, err, ErrNilObjects)
	h1, err := ResourceUpdateHash(context.Background(), base, nil)
	require.NoError(t, err)
	assert.NotEmpty(t, h1)
}

// TestParseCompareAnnotationProperties: parsing is insensitive to surrounding whitespace,
// empty items, duplicates and item order, and only exact (case-sensitive) items count.
func TestParseCompareAnnotationProperties(t *testing.T) {
	t.Parallel()
	vocabulary := []string{"IgnoreExtraneous", "ServerSideDiff=true", "ServerSideDiff=false", "IncludeMutationWebhook=true",
		"serverSideDiff=true", "IgnoreExtraneous=true", "Prune=false", ""}
	rapid.Check(t, func(rt *rapid.T) {
		items := rapid.SliceOfN(rapid.SampledFrom(vocabulary), 0, 8).Draw(rt, "items")
		canonical := strings.Join(items, ",")
		noisy := make([]string, 0, 2*len(items))
		for _, it := range rapid.Permutation(items).Draw(rt, "order") {
			pad := strings.Repeat(" ", rapid.IntRange(0, 2).Draw(rt, "pad"))
			noisy = append(noisy, pad+it+pad)
			if rapid.Bool().Draw(rt, "dup") {
				noisy = append(noisy, it)
			}
		}
		const key = DefaultCompareOptionsAnnotation
		want := ParseCompareAnnotation(map[string]string{key: canonical}, key)
		got := ParseCompareAnnotation(map[string]string{key: strings.Join(noisy, ",")}, key)
		assert.Equal(rt, want, got)

		assert.Equal(rt, slices.Contains(items, "IgnoreExtraneous"), got.IgnoreExtraneous)
		assert.Equal(rt, slices.Contains(items, "IncludeMutationWebhook=true"), got.IncludeMutationWebhook)
		switch {
		case slices.Contains(items, "ServerSideDiff=false"):
			require.NotNil(rt, got.ServerSideDiff)
			assert.False(rt, *got.ServerSideDiff, "an explicit disable wins")
		case slices.Contains(items, "ServerSideDiff=true"):
			require.NotNil(rt, got.ServerSideDiff)
			assert.True(rt, *got.ServerSideDiff)
		default:
			assert.Nil(rt, got.ServerSideDiff)
		}
		for _, controllerDefault := range []bool{false, true} {
			if got.ServerSideDiff == nil {
				assert.Equal(rt, controllerDefault, got.ServerSideDiffEnabled(controllerDefault))
			}
		}
	})
}
