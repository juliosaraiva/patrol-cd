package cache

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

func TestNewResource(t *testing.T) {
	t.Parallel()
	const tracking = "cd.patrol.example/tracking-id"
	withManaged := func(u *unstructured.Unstructured) {
		u.SetManagedFields([]metav1.ManagedFieldsEntry{{Manager: "kubectl", Operation: metav1.ManagedFieldsOperationApply}})
	}
	tests := []struct {
		name         string
		obj          *unstructured.Unstructured
		settings     Settings
		wantManifest bool
		wantLabels   map[string]string
		wantAnn      map[string]string
		wantLast     string
		wantRoot     bool
	}{
		{
			name: "unmanaged keeps no manifest and only tracking metadata",
			obj: mkObj("v1", "ConfigMap", "ns", "a", "u1",
				withLabels(map[string]string{"app.kubernetes.io/instance": "app", "other": "x"}),
				withAnnotations(map[string]string{tracking: "app:/ConfigMap:ns/a", AnnotationLastApplied: "{}", "noise": "y"}), withManaged),
			settings:   Settings{TrackingLabelKeys: []string{"app.kubernetes.io/instance"}, TrackingAnnotationKeys: []string{tracking}},
			wantLabels: map[string]string{"app.kubernetes.io/instance": "app"},
			wantAnn:    map[string]string{tracking: "app:/ConfigMap:ns/a"},
			wantRoot:   true,
		},
		{
			name: "managed manifest is stripped of managedFields and last-applied",
			obj: mkObj("v1", "ConfigMap", "ns", "a", "u1",
				withAnnotations(map[string]string{AnnotationLastApplied: `{"kind":"ConfigMap"}`, "keep": "me"}), withManaged),
			settings:     Settings{IsManaged: func(*unstructured.Unstructured) bool { return true }},
			wantManifest: true,
			wantLast:     `{"kind":"ConfigMap"}`,
			wantRoot:     true,
		},
		{
			name: "populate handler can request manifest retention",
			obj:  mkObj("apps/v1", "ReplicaSet", "ns", "rs", "u2", withOwner("apps/v1", "Deployment", "d", "u3"), withManaged),
			settings: Settings{PopulateResourceInfo: func(_ *unstructured.Unstructured, isRoot bool) (any, bool) {
				return isRoot, true
			}},
			wantManifest: true,
			wantRoot:     false,
		},
		{
			name:         "CRD manifests are always retained",
			obj:          mkObj("apiextensions.k8s.io/v1", "CustomResourceDefinition", "", "widgets.example.com", "u5", withManaged),
			wantManifest: true,
			wantRoot:     true,
		},
		{
			name:         "retain managed fields when requested",
			obj:          mkObj("v1", "ConfigMap", "ns", "a", "u1", withManaged),
			settings:     Settings{RetainManagedFields: true, IsManaged: func(*unstructured.Unstructured) bool { return true }},
			wantManifest: true,
			wantRoot:     true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := tc.settings.normalized()
			var gotRoot *bool
			if s.PopulateResourceInfo == nil {
				s.PopulateResourceInfo = func(_ *unstructured.Unstructured, isRoot bool) (any, bool) {
					gotRoot = &isRoot
					return "info", false
				}
			}
			r := newResource(tc.obj, &s)
			assert.Equal(t, "1", r.ResourceVersion)
			assert.Equal(t, types.UID(tc.obj.GetUID()), r.Ref.UID)
			assert.Equal(t, tc.wantLabels, r.Labels)
			assert.Equal(t, tc.wantAnn, r.Annotations)
			assert.Equal(t, tc.wantLast, r.LastAppliedConfiguration)
			if gotRoot != nil {
				assert.Equal(t, tc.wantRoot, *gotRoot)
				assert.Equal(t, "info", r.Info)
			} else if b, ok := r.Info.(bool); ok {
				assert.Equal(t, tc.wantRoot, b)
			}
			if !tc.wantManifest {
				assert.Nil(t, r.Resource)
				return
			}
			require.NotNil(t, r.Resource)
			_, hasLast := r.Resource.GetAnnotations()[AnnotationLastApplied]
			assert.False(t, hasLast, "last-applied must be stripped from the retained manifest")
			if tc.settings.RetainManagedFields {
				assert.NotEmpty(t, r.Resource.GetManagedFields())
			} else {
				assert.Empty(t, r.Resource.GetManagedFields())
			}
			live := r.liveCopy()
			assert.Equal(t, tc.wantLast, live.GetAnnotations()[AnnotationLastApplied])
			// liveCopy must not alias the shared manifest.
			live.SetName("mutated")
			assert.Equal(t, tc.obj.GetName(), r.Resource.GetName())
		})
	}
}

func TestWithInferredOwner(t *testing.T) {
	t.Parallel()
	pvc := mkRes("v1", "PersistentVolumeClaim", "ns", "data-web-0", "p1")
	owner := metav1.OwnerReference{APIVersion: "apps/v1", Kind: "StatefulSet", Name: "web", UID: "s1"}

	added := pvc.withInferredOwner(owner, true)
	require.NotSame(t, pvc, added)
	assert.Empty(t, pvc.OwnerRefs, "original must not be mutated")
	assert.Equal(t, []metav1.OwnerReference{owner}, added.OwnerRefs)
	assert.Same(t, added, added.withInferredOwner(owner, true), "adding twice is a no-op")

	removed := added.withInferredOwner(owner, false)
	assert.Empty(t, removed.OwnerRefs)
	assert.Len(t, added.OwnerRefs, 1, "copy-on-write")

	// A real owner reference is never removed by inference.
	real := mkRes("v1", "PersistentVolumeClaim", "ns", "data-web-0", "p1", withOwner("apps/v1", "StatefulSet", "web", "s1"))
	assert.Same(t, real, real.withInferredOwner(owner, true))
	assert.Same(t, real, real.withInferredOwner(owner, false))
}
