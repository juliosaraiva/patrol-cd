package diff

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const canaryCR = `
apiVersion: some.io/v1alpha1
kind: TestCRD
metadata: {name: canary}
spec:
  template:
    metadata: {labels: {app: canary}}
    spec:
      containers:
      - name: canary
        image: something:latest
        volumeMounts:
        - {name: config, mountPath: /etc/config, readOnly: false}
        resources:
          requests: {cpu: 2000m, memory: 32Mi}
  templates:
  - spec:
      containers:
      - name: a
        image: a:1
        resources: {requests: {cpu: 1500m}}
`

var testCRD = schema.GroupKind{Group: "some.io", Kind: "TestCRD"}

func knownNormalized(t *testing.T, fields map[schema.GroupKind][]KnownTypeField, manifest string) *unstructured.Unstructured {
	t.Helper()
	n, err := NewKnownTypesNormalizer(fields)
	require.NoError(t, err)
	u := obj(t, manifest)
	require.NoError(t, n.Normalize(context.Background(), u))
	return u
}

func firstContainer(t *testing.T, obj map[string]any, path ...string) map[string]any {
	t.Helper()
	items, ok, err := unstructured.NestedSlice(obj, path...)
	require.NoError(t, err)
	require.True(t, ok)
	return items[0].(map[string]any)
}

func TestKnownTypesNormalizer(t *testing.T) {
	t.Parallel()
	t.Run("map field typed as PodSpec", func(t *testing.T) {
		t.Parallel()
		u := knownNormalized(t, map[schema.GroupKind][]KnownTypeField{testCRD: {{Field: "spec.template.spec", Type: "core/v1/PodSpec"}}}, canaryCR)
		c := firstContainer(t, u.Object, "spec", "template", "spec", "containers")
		cpu, _, _ := unstructured.NestedFieldNoCopy(c, "resources", "requests", "cpu")
		assert.Equal(t, "2", cpu)
		vm := c["volumeMounts"].([]any)[0].(map[string]any)
		assert.NotContains(t, vm, "readOnly", "false booleans with omitempty are dropped")
	})
	t.Run("list field typed as Container", func(t *testing.T) {
		t.Parallel()
		u := knownNormalized(t, map[schema.GroupKind][]KnownTypeField{testCRD: {{Field: "spec.template.spec.containers", Type: "core/v1/Container"}}}, canaryCR)
		cpu, _, _ := unstructured.NestedFieldNoCopy(firstContainer(t, u.Object, "spec", "template", "spec", "containers"), "resources", "requests", "cpu")
		assert.Equal(t, "2", cpu)
	})
	t.Run("field inside nested lists", func(t *testing.T) {
		t.Parallel()
		u := knownNormalized(t, map[schema.GroupKind][]KnownTypeField{testCRD: {{Field: "spec.templates.spec.containers", Type: "core/v1/Container"}}}, canaryCR)
		tmpl := firstContainer(t, u.Object, "spec", "templates")
		cpu, _, _ := unstructured.NestedFieldNoCopy(firstContainer(t, tmpl, "spec", "containers"), "resources", "requests", "cpu")
		assert.Equal(t, "1500m", cpu)
	})
	t.Run("quantity", func(t *testing.T) {
		t.Parallel()
		u := knownNormalized(t, map[schema.GroupKind][]KnownTypeField{testCRD: {{Field: "spec.ram", Type: "core/Quantity"}}},
			`{"apiVersion":"some.io/v1alpha1","kind":"TestCRD","metadata":{"name":"x"},"spec":{"ram":"1.25G"}}`)
		v, _, _ := unstructured.NestedFieldNoCopy(u.Object, "spec", "ram")
		assert.Equal(t, "1250M", v)
	})
	t.Run("duration", func(t *testing.T) {
		t.Parallel()
		gk := schema.GroupKind{Group: "cert-manager.io", Kind: "Certificate"}
		u := knownNormalized(t, map[schema.GroupKind][]KnownTypeField{gk: {{Field: "spec.duration", Type: "meta/v1/Duration"}}},
			`{"apiVersion":"cert-manager.io/v1","kind":"Certificate","metadata":{"name":"c"},"spec":{"duration":"8760h"}}`)
		v, _, _ := unstructured.NestedFieldNoCopy(u.Object, "spec", "duration")
		assert.Equal(t, "8760h0m0s", v)
	})
	t.Run("missing field is ignored", func(t *testing.T) {
		t.Parallel()
		u := knownNormalized(t, map[schema.GroupKind][]KnownTypeField{testCRD: {{Field: "fieldDoesNotExist", Type: "core/v1/PodSpec"}}}, canaryCR)
		assert.Equal(t, obj(t, canaryCR), u)
	})
	t.Run("other kinds are untouched", func(t *testing.T) {
		t.Parallel()
		u := knownNormalized(t, map[schema.GroupKind][]KnownTypeField{{Group: "other.io", Kind: "TestCRD"}: {{Field: "spec.template.spec", Type: "core/v1/PodSpec"}}}, canaryCR)
		assert.Equal(t, obj(t, canaryCR), u)
	})
	t.Run("invalid value is an error", func(t *testing.T) {
		t.Parallel()
		n, err := NewKnownTypesNormalizer(map[schema.GroupKind][]KnownTypeField{testCRD: {{Field: "spec.ram", Type: "core/Quantity"}}})
		require.NoError(t, err)
		u := obj(t, `{"apiVersion":"some.io/v1alpha1","kind":"TestCRD","metadata":{"name":"x"},"spec":{"ram":"lots"}}`)
		require.Error(t, n.Normalize(context.Background(), u))
	})
	t.Run("rollout is preconfigured", func(t *testing.T) {
		t.Parallel()
		u := knownNormalized(t, nil, strings.Replace(strings.Replace(canaryCR, "some.io/v1alpha1", "argoproj.io/v1alpha1", 1), "TestCRD", "Rollout", 1))
		cpu, _, _ := unstructured.NestedFieldNoCopy(firstContainer(t, u.Object, "spec", "template", "spec", "containers"), "resources", "requests", "cpu")
		assert.Equal(t, "2", cpu)
	})
	t.Run("core group key", func(t *testing.T) {
		t.Parallel()
		n, err := NewKnownTypesNormalizer(map[schema.GroupKind][]KnownTypeField{{Kind: "ConfigMap"}: {{Field: "data", Type: "core/v1/PodSpec"}}})
		require.NoError(t, err)
		assert.Contains(t, n.fields, schema.GroupKind{Kind: "ConfigMap"})
	})
	t.Run("unsupported type is reported and skipped", func(t *testing.T) {
		t.Parallel()
		n, err := NewKnownTypesNormalizer(map[schema.GroupKind][]KnownTypeField{testCRD: {
			{Field: "spec.template.spec", Type: "core/v1/NoSuchType"},
			{Field: "spec.template.spec", Type: "core/v1/PodSpec"},
		}})
		require.ErrorIs(t, err, ErrUnsupportedKnownType)
		require.NotNil(t, n)
		assert.Len(t, n.fields[testCRD], 1)
	})
}

// TestKnownTypesList checks that every documented type name is constructible.
func TestKnownTypesList(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/known_types.txt")
	require.NoError(t, err)
	names := strings.Fields(string(data))
	require.Greater(t, len(names), 200)
	for _, name := range names {
		fn, ok := knownTypes[name]
		if assert.True(t, ok, name) {
			assert.NotNil(t, fn())
		}
	}
	supported := SupportedKnownTypes()
	assert.Contains(t, supported, "core/Quantity")
	assert.Contains(t, supported, "meta/v1/Duration")
	assert.Contains(t, supported, "core/v1/PodSpec")
	assert.Len(t, supported, len(names)+2)
}

func TestNewNormalizerOrder(t *testing.T) {
	t.Parallel()
	// The ignore rule runs first, then known types: the removed field is not re-encoded.
	n, err := NewNormalizer(NormalizerConfig{
		Rules:           []IgnoreRule{{Group: "some.io", Kind: "TestCRD", JSONPointers: []string{"/spec/templates"}}},
		KnownTypeFields: map[schema.GroupKind][]KnownTypeField{testCRD: {{Field: "spec.template.spec", Type: "core/v1/PodSpec"}}},
	})
	require.NoError(t, err)
	u := obj(t, canaryCR)
	require.NoError(t, n.Normalize(context.Background(), u))
	_, ok, _ := unstructured.NestedFieldNoCopy(u.Object, "spec", "templates")
	assert.False(t, ok)
	cpu, _, _ := unstructured.NestedFieldNoCopy(firstContainer(t, u.Object, "spec", "template", "spec", "containers"), "resources", "requests", "cpu")
	assert.Equal(t, "2", cpu)

	_, err = NewNormalizer(NormalizerConfig{Rules: []IgnoreRule{{Kind: "*", JQPathExpressions: []string{"(("}}}})
	require.ErrorIs(t, err, ErrInvalidIgnoreRule)

	n, err = NewNormalizer(NormalizerConfig{KnownTypeFields: map[schema.GroupKind][]KnownTypeField{testCRD: {{Field: "x", Type: "nope"}}}})
	require.ErrorIs(t, err, ErrUnsupportedKnownType)
	require.NotNil(t, n, "the normalizer is usable despite unsupported known types")
}
