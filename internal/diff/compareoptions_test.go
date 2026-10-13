package diff

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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

	_, err := ResourceUpdateHash(context.Background(), nil, nil)
	require.ErrorIs(t, err, ErrNilObjects)
	h1, err := ResourceUpdateHash(context.Background(), base, nil)
	require.NoError(t, err)
	assert.NotEmpty(t, h1)
}
