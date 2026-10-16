package tracking

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func service() *unstructured.Unstructured {
	o := newObj("v1", "Service", "default", "my-service")
	o.Object["spec"] = map[string]any{"selector": map[string]any{"app": "MyApp"}}
	return o
}

func crd() *unstructured.Unstructured {
	o := newObj("apiextensions.k8s.io/v1", "CustomResourceDefinition", "", "crontabs.stable.example.com")
	o.Object["spec"] = map[string]any{"group": "stable.example.com", "scope": "Namespaced"}
	return withLabels(o, keyInstance, "my-app")
}

// TestNormalize ports the upstream ResourceIdNormalizer scenarios (APP-076, DIFF-026).
func TestNormalize(t *testing.T) {
	t.Parallel()
	tr := newTestTracker()
	set := func(o *unstructured.Unstructured, instance string, m Method) *unstructured.Unstructured {
		require.NoError(t, tr.SetAppInstance(o, instance, "", m, ""))
		return o
	}
	tests := []struct {
		name            string
		target, live    *unstructured.Unstructured
		method          Method
		wantAnnotations map[string]string
		wantLabels      map[string]string
	}{
		{
			name:            "label to annotation migration backfills the id and drops the label",
			live:            set(service(), "my-app", MethodLabel),
			target:          set(service(), "my-app2", MethodAnnotation),
			method:          MethodAnnotation,
			wantAnnotations: map[string]string{keyTracking: "my-app2:/Service:default/my-service"},
		},
		{
			name:            "empty method is the annotation default",
			live:            set(service(), "my-app", MethodLabel),
			target:          set(service(), "my-app", MethodAnnotation),
			method:          "",
			wantAnnotations: map[string]string{keyTracking: "my-app:/Service:default/my-service"},
		},
		{
			name:            "label kept when the desired object has it",
			live:            set(service(), "my-app", MethodLabel),
			target:          set(set(service(), "my-app2", MethodAnnotation), "my-app", MethodLabel),
			method:          MethodAnnotation,
			wantAnnotations: map[string]string{keyTracking: "my-app2:/Service:default/my-service"},
			wantLabels:      map[string]string{keyInstance: "my-app"},
		},
		{
			name:            "resource moved between apps (annotation+label) keeps the stale id",
			live:            set(configMap("default", "my-cm"), "app-a", MethodAnnotationAndLabel),
			target:          set(configMap("default", "my-cm"), "app-b", MethodAnnotationAndLabel),
			method:          MethodAnnotationAndLabel,
			wantAnnotations: map[string]string{keyTracking: "app-a:/ConfigMap:default/my-cm"},
			wantLabels:      map[string]string{keyInstance: "app-a"},
		},
		{
			name:            "resource moved to an annotation-tracked app keeps the id, drops the label",
			live:            set(configMap("default", "my-cm"), "app-a", MethodAnnotationAndLabel),
			target:          set(configMap("default", "my-cm"), "app-b", MethodAnnotation),
			method:          MethodAnnotation,
			wantAnnotations: map[string]string{keyTracking: "app-a:/ConfigMap:default/my-cm"},
		},
		{
			name:            "chart-rendered instance label does not hide a stale id",
			live:            set(withLabels(configMap("default", "my-cm"), keyInstance, "my-release"), "standalone-app", MethodAnnotation),
			target:          set(withLabels(configMap("default", "my-cm"), keyInstance, "my-release"), "appset-app", MethodAnnotation),
			method:          MethodAnnotation,
			wantAnnotations: map[string]string{keyTracking: "standalone-app:/ConfigMap:default/my-cm"},
			wantLabels:      map[string]string{keyInstance: "my-release"},
		},
		{
			name:       "CRDs are left alone",
			live:       crd(),
			target:     crd(),
			method:     MethodAnnotation,
			wantLabels: map[string]string{keyInstance: "my-app"},
		},
		{
			name:       "label tracking is a no-op",
			live:       set(service(), "my-app", MethodLabel),
			target:     set(service(), "my-app2", MethodAnnotation),
			method:     MethodLabel,
			wantLabels: map[string]string{keyInstance: "my-app"},
		},
		{
			name:   "live without instance label is a no-op",
			live:   service(),
			target: set(service(), "my-app", MethodAnnotation),
			method: MethodAnnotation,
		},
		{
			// Upstream copies the (missing) desired id as an empty value; reproduced for parity.
			name:            "desired object without tracking id",
			live:            set(service(), "my-app", MethodLabel),
			target:          service(),
			method:          MethodAnnotation,
			wantAnnotations: map[string]string{keyTracking: ""},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			target := tc.target.DeepCopy()
			require.NoError(t, tr.Normalize(target, tc.live, tc.method))
			assert.Equal(t, tc.target, target, "target must not be modified")
			assert.Equal(t, tc.wantAnnotations, nilIfEmpty(tc.live.GetAnnotations()))
			assert.Equal(t, tc.wantLabels, nilIfEmpty(tc.live.GetLabels()))
		})
	}
}

func nilIfEmpty(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	return m
}

func TestNormalizeNilAndErrors(t *testing.T) {
	t.Parallel()
	tr := newTestTracker()
	require.NoError(t, tr.Normalize(nil, service(), MethodAnnotation))
	require.NoError(t, tr.Normalize(service(), nil, MethodAnnotation))
	require.NoError(t, tr.Normalize(nil, nil, MethodAnnotation))

	badLabels := service()
	badLabels.Object["metadata"].(map[string]any)["labels"] = "x"
	require.ErrorContains(t, tr.Normalize(service(), badLabels, MethodAnnotation), "failed to get app instance label: failed to get labels for /v1, Kind=Service default/my-service")

	live := withLabels(service(), keyInstance, "my-app")
	badTarget := service()
	badTarget.Object["metadata"].(map[string]any)["annotations"] = map[string]any{keyTracking: true}
	require.ErrorContains(t, tr.Normalize(badTarget, live, MethodAnnotation), "failed to get annotations from target object")

	badTargetLabels := withAnnotations(service(), keyTracking, "my-app:/Service:default/my-service")
	badTargetLabels.Object["metadata"].(map[string]any)["labels"] = "x"
	require.ErrorContains(t, tr.Normalize(badTargetLabels, withLabels(service(), keyInstance, "my-app"), MethodAnnotation), "failed to get app instance label")
}

func TestNormalizeCustomLabelKey(t *testing.T) {
	t.Parallel()
	const custom = "cd.patrol.example/instance"
	tr := New(Config{InstanceLabelKey: custom})
	live := withLabels(service(), custom, "my-app", keyInstance, "chart")
	target := withLabels(service(), keyInstance, "chart")
	require.NoError(t, tr.SetAppInstance(target, "my-app", "", MethodAnnotation, ""))
	require.NoError(t, tr.Normalize(target, live, MethodAnnotation))
	assert.Equal(t, map[string]string{keyTracking: "my-app:/Service:default/my-service"}, live.GetAnnotations())
	assert.Equal(t, map[string]string{keyInstance: "chart"}, live.GetLabels())
}
