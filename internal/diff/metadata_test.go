package diff

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"pgregory.net/rapid"
)

var testTracking = TrackingOptions{LabelKey: "app.kubernetes.io/instance", AnnotationKey: "cd.patrol.example/tracking-id"}

func TestNormalizeTracking(t *testing.T) {
	t.Parallel()
	const trackingID = "shop:apps/Deployment:shop/web"
	meta := func(labels, annotations map[string]any) *unstructured.Unstructured {
		md := map[string]any{"name": "web", "namespace": "shop"}
		if labels != nil {
			md["labels"] = labels
		}
		if annotations != nil {
			md["annotations"] = annotations
		}
		return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": md}}
	}
	instance := func() map[string]any { return map[string]any{"app.kubernetes.io/instance": "shop"} }
	tracked := func() map[string]any { return map[string]any{"cd.patrol.example/tracking-id": trackingID} }

	cases := []struct {
		name            string
		config, live    *unstructured.Unstructured
		opts            TrackingOptions
		wantLabels      map[string]string
		wantAnnotations map[string]string
		wantErr         bool
	}{
		{
			name:   "label-tracked live gets the desired annotation",
			config: meta(nil, tracked()), live: meta(instance(), nil), opts: testTracking,
			wantAnnotations: map[string]string{"cd.patrol.example/tracking-id": trackingID},
		},
		{
			name:   "instance label not desired is removed from live",
			config: meta(map[string]any{"tier": "web"}, tracked()), live: meta(map[string]any{"app.kubernetes.io/instance": "shop", "tier": "web"}, tracked()), opts: testTracking,
			wantLabels: map[string]string{"tier": "web"}, wantAnnotations: map[string]string{"cd.patrol.example/tracking-id": trackingID},
		},
		{
			name:   "desired instance label is kept",
			config: meta(instance(), tracked()), live: meta(instance(), tracked()), opts: testTracking,
			wantLabels: map[string]string{"app.kubernetes.io/instance": "shop"}, wantAnnotations: map[string]string{"cd.patrol.example/tracking-id": trackingID},
		},
		{
			name:   "a different live tracking annotation is a real difference and kept",
			config: meta(instance(), tracked()), live: meta(instance(), map[string]any{"cd.patrol.example/tracking-id": "other:apps/Deployment:shop/web"}), opts: testTracking,
			wantLabels: map[string]string{"app.kubernetes.io/instance": "shop"}, wantAnnotations: map[string]string{"cd.patrol.example/tracking-id": "other:apps/Deployment:shop/web"},
		},
		{
			name:   "live without instance label is untouched",
			config: meta(nil, tracked()), live: meta(map[string]any{"tier": "web"}, nil), opts: testTracking,
			wantLabels: map[string]string{"tier": "web"},
		},
		{
			name:   "label tracking (disabled) is a no-op",
			config: meta(nil, tracked()), live: meta(instance(), nil), opts: TrackingOptions{},
			wantLabels: map[string]string{"app.kubernetes.io/instance": "shop"},
		},
		{
			name:   "malformed live labels are an error and leave live unchanged",
			config: meta(nil, tracked()), live: meta(map[string]any{"app.kubernetes.io/instance": int64(1)}, nil), opts: testTracking,
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			before := tc.live.DeepCopy()
			err := NormalizeTracking(tc.config, tc.live, tc.opts)
			if tc.wantErr {
				require.Error(t, err)
				assert.Equal(t, before, tc.live)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantLabels, tc.live.GetLabels())
			assert.Equal(t, tc.wantAnnotations, tc.live.GetAnnotations())
		})
	}

	t.Run("CRDs never get tracking annotations", func(t *testing.T) {
		t.Parallel()
		crd := func() *unstructured.Unstructured {
			return &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
				"metadata": map[string]any{"name": "widgets.example.com", "labels": map[string]any{"app.kubernetes.io/instance": "shop"}},
			}}
		}
		live := crd()
		require.NoError(t, NormalizeTracking(crd(), live, testTracking))
		assert.Nil(t, live.GetAnnotations())
	})
}

func TestDiffTrackingMigration(t *testing.T) {
	t.Parallel()
	config := obj(t, widgetConfig)
	config.SetAnnotations(map[string]string{"cd.patrol.example/tracking-id": "shop:example.com/Widget:shop/w1"})
	live := obj(t, widgetLive)
	live.SetLabels(map[string]string{"app.kubernetes.io/instance": "shop"})
	live = withLastApplied(t, live, obj(t, widgetConfig))
	liveBefore := live.DeepCopy()

	assert.True(t, mustDiff(t, config, live, Options{}).Modified, "without tracking normalization the annotation is drift")
	res := mustDiff(t, config, live, Options{Tracking: testTracking})
	assert.False(t, res.Modified, "live: %s\npredicted: %s", res.NormalizedLive, res.PredictedLive)
	_, found := nested(t, res.NormalizedLive, "metadata", "labels", "app.kubernetes.io/instance")
	assert.False(t, found, "instance label not in the desired state is dropped from live")
	assert.Equal(t, liveBefore, live, "Diff must not modify its input")

	moved := live.DeepCopy()
	ann := moved.GetAnnotations()
	ann["cd.patrol.example/tracking-id"] = "other:example.com/Widget:shop/w1"
	moved.SetAnnotations(ann)
	assert.True(t, mustDiff(t, config, moved, Options{Tracking: testTracking}).Modified, "resource owned by another application")
}

func TestStripMetadataNoise(t *testing.T) {
	t.Parallel()
	u := obj(t, `
apiVersion: v1
kind: ConfigMap
metadata:
  name: c
  namespace: shop
  uid: u
  resourceVersion: "7"
  generation: 2
  creationTimestamp: "2026-01-02T03:04:05Z"
  selfLink: /api/v1/namespaces/shop/configmaps/c
  labels: {a: b}
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: "{}"
    cd.patrol.example/tracking-id: x
  managedFields: [{manager: m, operation: Apply}]
data: {k: v}`)
	StripMetadataNoise(u, "cd.patrol.example/tracking-id")
	assert.Equal(t, map[string]any{"name": "c", "namespace": "shop", "labels": map[string]any{"a": "b"}}, u.Object["metadata"])
	assert.Equal(t, map[string]any{"k": "v"}, u.Object["data"])

	keep := obj(t, `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"c","annotations":{"owner":"team"}}}`)
	StripMetadataNoise(keep)
	assert.Equal(t, map[string]string{"owner": "team"}, keep.GetAnnotations())

	StripMetadataNoise(nil)
	StripMetadataNoise(&unstructured.Unstructured{Object: map[string]any{"metadata": "bogus"}})
}

// TestGoTypedValues: objects built in Go code may hold int, int32 or typed maps, which
// unstructured deep copies reject with a panic. Every entry point must accept them.
func TestGoTypedValues(t *testing.T) {
	t.Parallel()
	deploy := func() *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "apps/v1", "kind": "Deployment",
			"metadata": map[string]any{"name": "d", "labels": map[string]any{"app.kubernetes.io/instance": "x"}},
			"spec": map[string]any{
				"replicas": 2, "minReadySeconds": int32(5), "paused": false,
				"template": map[string]any{"metadata": map[string]any{"labels": map[string]string{"a": "b"}}},
			},
		}}
	}
	n, err := NewIgnoreNormalizer([]IgnoreRule{{Group: "*", Kind: "*", ManagedFieldsManagers: []string{"m"}, JSONPointers: []string{"/spec/paused"}}}, IgnoreNormalizerOptions{})
	require.NoError(t, err)
	for name, o := range map[string]Options{
		"legacy":           {},
		"tracking":         {Tracking: testTracking},
		"trusted managers": {Normalizer: n},
		"structured merge": {StructuredMerge: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			res, err := Diff(context.Background(), deploy(), deploy(), o)
			require.NoError(t, err)
			assert.False(t, res.Modified)
		})
	}
	t.Run("exported helpers", func(t *testing.T) {
		t.Parallel()
		res, err := TwoWayDiff(deploy(), deploy())
		require.NoError(t, err)
		assert.False(t, res.Modified)
		res, err = ThreeWayDiff(deploy(), deploy(), deploy())
		require.NoError(t, err)
		assert.False(t, res.Modified)
		res, err = StructuredMergeDiff(deploy(), deploy(), nil, "")
		require.NoError(t, err)
		assert.False(t, res.Modified)
		_, _, err = NormalizeManagedFields(deploy(), deploy(), []string{"m"}, nil)
		require.NoError(t, err)
		h1, err := ResourceUpdateHash(context.Background(), deploy(), n)
		require.NoError(t, err)
		h2, err := ResourceUpdateHash(context.Background(), obj(t, string(mustJSON(t, deploy()))), n)
		require.NoError(t, err)
		assert.Equal(t, h1, h2, "Go-typed and JSON-decoded objects hash identically")
		secret := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "s"}, "data": map[string]any{"a": 1}}}
		target, live, err := HideSecretData(secret, secret, nil)
		require.NoError(t, err)
		assert.Equal(t, maskUnit, target.Object["data"].(map[string]any)["a"])
		assert.Equal(t, maskUnit, live.Object["data"].(map[string]any)["a"])
	})
}

// TestNormalizeTrackingIdempotent: tracking normalization applied twice equals once.
func TestNormalizeTrackingIdempotent(t *testing.T) {
	t.Parallel()
	labelValues := []any{nil, "shop", "other", ""}
	rapid.Check(t, func(rt *rapid.T) {
		gen := func(prefix string) *unstructured.Unstructured {
			md := map[string]any{"name": "w"}
			labels, annotations := map[string]any{}, map[string]any{}
			if v := rapid.SampledFrom(labelValues).Draw(rt, prefix+"label"); v != nil {
				labels[testTracking.LabelKey] = v
			}
			if v := rapid.SampledFrom(labelValues).Draw(rt, prefix+"annotation"); v != nil {
				annotations[testTracking.AnnotationKey] = v
			}
			if rapid.Bool().Draw(rt, prefix+"extra") {
				labels["tier"] = "web"
			}
			if len(labels) > 0 {
				md["labels"] = labels
			}
			if len(annotations) > 0 {
				md["annotations"] = annotations
			}
			kind := rapid.SampledFrom([]string{"ConfigMap", "CustomResourceDefinition"}).Draw(rt, prefix+"kind")
			apiVersion := "v1"
			if kind == "CustomResourceDefinition" {
				apiVersion = "apiextensions.k8s.io/v1"
			}
			return &unstructured.Unstructured{Object: map[string]any{"apiVersion": apiVersion, "kind": kind, "metadata": md}}
		}
		config, live := gen("config"), gen("live")
		live.SetKind(config.GetKind())
		live.SetAPIVersion(config.GetAPIVersion())
		require.NoError(rt, NormalizeTracking(config, live, testTracking))
		once := live.DeepCopy()
		require.NoError(rt, NormalizeTracking(config, live, testTracking))
		assert.Equal(rt, once.Object, live.Object)
	})
}
