package cache

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestResolveOwnerRefs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   []objOpt
		kind [2]string // apiVersion, kind
		want []metav1.OwnerReference
	}{
		{
			name: "endpoints without owner point at service",
			kind: [2]string{"v1", "Endpoints"},
			want: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Service", Name: "obj"}},
		},
		{
			name: "endpoints with owner keep it",
			kind: [2]string{"v1", "Endpoints"},
			in:   []objOpt{withOwner("v1", "Service", "other", "u9")},
			want: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Service", Name: "other", UID: "u9"}},
		},
		{
			name: "service account token secret",
			kind: [2]string{"v1", "Secret"},
			in: []objOpt{withField("kubernetes.io/service-account-token", "type"),
				withAnnotations(map[string]string{annotationSAName: "sa", annotationSAUID: "sa-uid"})},
			want: []metav1.OwnerReference{{APIVersion: "v1", Kind: "ServiceAccount", Name: "sa", UID: "sa-uid"}},
		},
		{
			name: "opaque secret has no inferred owner",
			kind: [2]string{"v1", "Secret"},
			in:   []objOpt{withAnnotations(map[string]string{annotationSAName: "sa", annotationSAUID: "sa-uid"})},
		},
		{
			name: "token secret missing uid",
			kind: [2]string{"v1", "Secret"},
			in: []objOpt{withField("kubernetes.io/service-account-token", "type"),
				withAnnotations(map[string]string{annotationSAName: "sa"})},
		},
		{
			name: "olm csv",
			kind: [2]string{"operators.coreos.com/v1alpha1", "ClusterServiceVersion"},
			in:   []objOpt{withAnnotations(map[string]string{annotationOLMOperatorGroup: "og"})},
			want: []metav1.OwnerReference{{APIVersion: "operators.coreos.com/v1", Kind: "OperatorGroup", Name: "og"}},
		},
		{
			name: "plain configmap",
			kind: [2]string{"v1", "ConfigMap"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			u := mkObj(tc.kind[0], tc.kind[1], "ns", "obj", "uid", tc.in...)
			refs, inferred := resolveOwnerRefs(u)
			assert.Equal(t, tc.want, refs)
			assert.Nil(t, inferred)
		})
	}
}

func TestStatefulSetClaims(t *testing.T) {
	t.Parallel()
	sts := mkObj("apps/v1", "StatefulSet", "ns", "web", "s1",
		withField([]any{
			map[string]any{"metadata": map[string]any{"name": "data"}},
			map[string]any{"metadata": map[string]any{"name": "logs.v1"}},
		}, "spec", "volumeClaimTemplates"))
	_, pred := resolveOwnerRefs(sts)
	require.NotNil(t, pred)
	tests := []struct {
		key  ResourceKey
		want bool
	}{
		{NewResourceKey("", "PersistentVolumeClaim", "ns", "data-web-0"), true},
		{NewResourceKey("", "PersistentVolumeClaim", "ns", "data-web-12"), true},
		{NewResourceKey("", "PersistentVolumeClaim", "ns", "logs.v1-web-3"), true},
		{NewResourceKey("", "PersistentVolumeClaim", "ns", "logsXv1-web-3"), false}, // dots are literal
		{NewResourceKey("", "PersistentVolumeClaim", "ns", "x-data-web-0"), false},  // anchored at start
		{NewResourceKey("", "PersistentVolumeClaim", "ns", "data-web-"), false},
		{NewResourceKey("", "PersistentVolumeClaim", "ns", "data-web-0a"), false},
		{NewResourceKey("", "PersistentVolumeClaim", "other", "data-web-0"), false},
		{NewResourceKey("", "ConfigMap", "ns", "data-web-0"), false},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, pred(tc.key), tc.key.String())
	}

	noTemplates := mkObj("apps/v1", "StatefulSet", "ns", "web", "s1")
	_, pred = resolveOwnerRefs(noTemplates)
	assert.Nil(t, pred)
}
