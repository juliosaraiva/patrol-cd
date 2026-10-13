package diff

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// hpaScaledLive is a Deployment whose replicas were changed by an autoscaler (manager
// hpa-controller), and whose container env got a var from another manager.
const hpaScaledLive = `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
  namespace: shop
  managedFields:
  - manager: patrol-controller
    operation: Apply
    apiVersion: apps/v1
    fieldsType: FieldsV1
    fieldsV1: {"f:spec":{"f:selector":{},"f:template":{"f:metadata":{"f:labels":{"f:app":{}}},"f:spec":{"f:containers":{"k:{\"name\":\"api\"}":{".":{},"f:image":{},"f:name":{}}}}}}}
  - manager: hpa-controller
    operation: Update
    apiVersion: apps/v1
    fieldsType: FieldsV1
    fieldsV1: {"f:spec":{"f:replicas":{}}}
  - manager: sidecar-injector
    operation: Update
    apiVersion: apps/v1
    fieldsType: FieldsV1
    fieldsV1: {"f:spec":{"f:template":{"f:spec":{"f:containers":{"k:{\"name\":\"api\"}":{"f:image":{}}}}}}}
spec:
  replicas: 7
  selector: {matchLabels: {app: api}}
  template:
    metadata: {labels: {app: api}}
    spec:
      containers:
      - {name: api, image: "api:2-patched"}
`

const hpaScaledConfig = `
apiVersion: apps/v1
kind: Deployment
metadata: {name: api, namespace: shop}
spec:
  replicas: 2
  selector: {matchLabels: {app: api}}
  template:
    metadata: {labels: {app: api}}
    spec:
      containers:
      - {name: api, image: "api:2"}
`

func TestNormalizeManagedFields(t *testing.T) {
	t.Parallel()
	p := testParser(t)
	for _, tc := range []struct {
		name         string
		managers     []string
		wantReplicas bool
		wantImage    bool
	}{
		{"no trusted managers", nil, true, true},
		{"trusted autoscaler", []string{"hpa-controller"}, false, true},
		{"trusted injector", []string{"sidecar-injector"}, true, false},
		{"both", []string{"hpa-controller", "sidecar-injector"}, false, false},
		{"unrelated manager", []string{"someone"}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			live, config := obj(t, hpaScaledLive), obj(t, hpaScaledConfig)
			liveBefore, configBefore := live.DeepCopy(), config.DeepCopy()
			nl, nc, err := NormalizeManagedFields(live, config, tc.managers, p)
			require.NoError(t, err)
			assert.Equal(t, liveBefore, live)
			assert.Equal(t, configBefore, config)
			for _, u := range []*unstructured.Unstructured{nl, nc} {
				_, has, _ := unstructured.NestedFieldNoCopy(u.Object, "spec", "replicas")
				assert.Equal(t, tc.wantReplicas, has, "replicas")
				cs, _, _ := unstructured.NestedSlice(u.Object, "spec", "template", "spec", "containers")
				require.Len(t, cs, 1, "containers are associative: only the owned field is removed")
				_, has = cs[0].(map[string]any)["image"]
				assert.Equal(t, tc.wantImage, has, "image")
			}
		})
	}
	t.Run("nil inputs", func(t *testing.T) {
		t.Parallel()
		nl, nc, err := NormalizeManagedFields(nil, obj(t, hpaScaledConfig), []string{"x"}, p)
		require.NoError(t, err)
		assert.Nil(t, nl)
		assert.NotNil(t, nc)
	})
	t.Run("invalid fieldsV1 of a trusted manager", func(t *testing.T) {
		t.Parallel()
		live := obj(t, hpaScaledLive)
		entries := live.Object["metadata"].(map[string]any)["managedFields"].([]any)
		entries[1].(map[string]any)["fieldsV1"] = map[string]any{"zz:not-a-path-element": map[string]any{}}
		_, _, err := NormalizeManagedFields(live, obj(t, hpaScaledConfig), []string{"hpa-controller"}, p)
		require.Error(t, err)
	})
	t.Run("objects violating the schema are returned unchanged", func(t *testing.T) {
		t.Parallel()
		config := obj(t, hpaScaledConfig)
		require.NoError(t, unstructured.SetNestedField(config.Object, "not-a-number", "spec", "replicas"))
		nl, nc, err := NormalizeManagedFields(obj(t, hpaScaledLive), config, []string{"hpa-controller"}, p)
		require.NoError(t, err)
		assert.Equal(t, obj(t, hpaScaledLive), nl)
		assert.Equal(t, config, nc)
	})
	t.Run("without a parser lists are atomic", func(t *testing.T) {
		t.Parallel()
		nl, _, err := NormalizeManagedFields(obj(t, hpaScaledLive), obj(t, hpaScaledConfig), []string{"hpa-controller"}, nil)
		require.NoError(t, err)
		_, has, _ := unstructured.NestedFieldNoCopy(nl.Object, "spec", "replicas")
		assert.False(t, has)
	})
}

func TestDiffWithTrustedManagers(t *testing.T) {
	t.Parallel()
	p := testParser(t)
	diffWith := func(rules []IgnoreRule) *Result {
		n, err := NewNormalizer(NormalizerConfig{Rules: rules})
		require.NoError(t, err)
		res, err := Diff(context.Background(), obj(t, hpaScaledConfig), obj(t, hpaScaledLive), Options{Normalizer: n, GVKParser: p})
		require.NoError(t, err)
		return res
	}
	assert.True(t, diffWith(nil).Modified)
	assert.True(t, diffWith([]IgnoreRule{{Group: "apps", Kind: "Deployment", ManagedFieldsManagers: []string{"hpa-controller"}}}).Modified, "image still differs")

	res := diffWith([]IgnoreRule{{Group: "*", Kind: "*", ManagedFieldsManagers: []string{"hpa-controller", "sidecar-injector"}}})
	assert.False(t, res.Modified, "live: %s\npredicted: %s", res.NormalizedLive, res.PredictedLive)
	// The removed replicas field is re-defaulted on both sides, as the API server would.
	r1, _ := nested(t, res.NormalizedLive, "spec", "replicas")
	r2, _ := nested(t, res.PredictedLive, "spec", "replicas")
	assert.EqualValues(t, 1, r1)
	assert.EqualValues(t, 1, r2)

	other := diffWith([]IgnoreRule{{Group: "apps", Kind: "Deployment", Name: "not-this-one", ManagedFieldsManagers: []string{"hpa-controller", "sidecar-injector"}}})
	assert.True(t, other.Modified, "rules for other objects do not apply")
}
