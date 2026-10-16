package tracking

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
)

// TestSetAppInstanceRoundTrip writes tracking metadata with every method and reads it back.
func TestSetAppInstanceRoundTrip(t *testing.T) {
	t.Parallel()
	type want struct {
		trackingID string // "" means no annotation
		label      string // "" means no label
	}
	cpApp := App{Name: "guestbook", Namespace: testControlPlane}
	teamApp := App{Name: "web", Namespace: "team"}
	tests := []struct {
		name      string
		obj       func() *unstructured.Unstructured
		app       App
		defaultNS string
		want      map[Method]want
	}{
		{
			name:      "namespaced deployment, control-plane app",
			obj:       func() *unstructured.Unstructured { return newObj("apps/v1", "Deployment", "default", "my-deployment") },
			app:       cpApp,
			defaultNS: "default",
			want: map[Method]want{
				"":                       {trackingID: "guestbook:apps/Deployment:default/my-deployment"},
				MethodAnnotation:         {trackingID: "guestbook:apps/Deployment:default/my-deployment"},
				MethodLabel:              {label: "guestbook"},
				MethodAnnotationAndLabel: {trackingID: "guestbook:apps/Deployment:default/my-deployment", label: "guestbook"},
			},
		},
		{
			name: "cluster-scoped resource records the destination namespace",
			obj: func() *unstructured.Unstructured {
				return newObj("rbac.authorization.k8s.io/v1", "ClusterRole", "", "reader")
			},
			app:       cpApp,
			defaultNS: "dest",
			want: map[Method]want{
				"":                       {trackingID: "guestbook:rbac.authorization.k8s.io/ClusterRole:dest/reader"},
				MethodAnnotation:         {trackingID: "guestbook:rbac.authorization.k8s.io/ClusterRole:dest/reader"},
				MethodLabel:              {label: "guestbook"},
				MethodAnnotationAndLabel: {trackingID: "guestbook:rbac.authorization.k8s.io/ClusterRole:dest/reader", label: "guestbook"},
			},
		},
		{
			name:      "app in any namespace is qualified",
			obj:       func() *unstructured.Unstructured { return configMap("team-dest", "settings") },
			app:       teamApp,
			defaultNS: "team-dest",
			want: map[Method]want{
				"":                       {trackingID: "team_web:/ConfigMap:team-dest/settings"},
				MethodAnnotation:         {trackingID: "team_web:/ConfigMap:team-dest/settings"},
				MethodLabel:              {label: "team_web"},
				MethodAnnotationAndLabel: {trackingID: "team_web:/ConfigMap:team-dest/settings", label: "team_web"},
			},
		},
	}
	tr := newTestTracker()
	for _, tc := range tests {
		for _, m := range allMethods {
			t.Run(tc.name+"/"+string(m), func(t *testing.T) {
				t.Parallel()
				w := tc.want[m]
				obj := tc.obj()
				instance := tr.InstanceName(tc.app)
				require.NoError(t, tr.SetAppInstance(obj, instance, tc.defaultNS, m, ""))

				gotID, hasID := obj.GetAnnotations()[keyTracking]
				assert.Equal(t, w.trackingID != "", hasID, "tracking-id presence")
				assert.Equal(t, w.trackingID, gotID)
				gotLabel, hasLabel := obj.GetLabels()[keyInstance]
				assert.Equal(t, w.label != "", hasLabel, "instance label presence")
				assert.Equal(t, w.label, gotLabel)
				assert.NotContains(t, obj.GetAnnotations(), keyInstallation)

				assert.Equal(t, instance, tr.GetAppName(obj, m, ""))
				assert.True(t, tr.IsManagedBy(obj, tc.app, m, ""))
				assert.True(t, tr.IsSelfReferencedObject(obj, nil, m, ""))
				other, shared := tr.DetectSharedResource(obj, tc.app, m, "")
				assert.False(t, shared)
				assert.Empty(t, other)

				inst := tr.GetAppInstance(obj, m, "")
				if m == MethodLabel {
					assert.Nil(t, inst)
					return
				}
				require.NotNil(t, inst)
				assert.Equal(t, w.trackingID, inst.String())
				assert.Equal(t, instance, inst.ApplicationName)
			})
		}
	}
}

func TestSetAppInstanceIsIdempotentAndOverwrites(t *testing.T) {
	t.Parallel()
	tr := newTestTracker()
	obj := withAnnotations(configMap("ns", "cm"), "keep", "me")
	require.NoError(t, tr.SetAppInstance(obj, "old", "ns", MethodAnnotationAndLabel, "inst"))
	first := obj.DeepCopy()
	require.NoError(t, tr.SetAppInstance(obj, "old", "ns", MethodAnnotationAndLabel, "inst"))
	assert.Equal(t, first, obj)

	// Taking ownership rewrites the tracking id, the label and the installation id.
	require.NoError(t, tr.SetAppInstance(obj, "new", "ns", MethodAnnotationAndLabel, ""))
	assert.Equal(t, map[string]string{"keep": "me", keyTracking: "new:/ConfigMap:ns/cm"}, obj.GetAnnotations())
	assert.Equal(t, map[string]string{keyInstance: "new"}, obj.GetLabels())
}

// TestLabelTrackingDoesNotTruncate: under label tracking the value is written as is, so
// instance names over 63 characters are rejected by the API server (APP-071).
func TestLabelTrackingDoesNotTruncate(t *testing.T) {
	t.Parallel()
	tr := newTestTracker()
	long := strings.Repeat("a", 70)
	obj := configMap("ns", "cm")
	require.NoError(t, tr.SetAppInstance(obj, long, "ns", MethodLabel, ""))
	assert.Equal(t, long, obj.GetLabels()[keyInstance])
	assert.Equal(t, long, tr.GetAppName(obj, MethodLabel, ""))
	assert.Empty(t, obj.GetAnnotations())
}

// TestAnnotationAndLabelTruncation mirrors the upstream truncation scenarios (APP-072).
func TestAnnotationAndLabelTruncation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, instance, wantLabel string
	}{
		{
			name:      "long name",
			instance:  "my-app-with-an-extremely-long-name-that-is-over-sixty-three-characters",
			wantLabel: "my-app-with-an-extremely-long-name-that-is-over-sixty-three-cha",
		},
		{
			name:      "trailing hyphen after truncation",
			instance:  "the-very-suspicious-name-with-precisely-sixty-three-characters-with-hyphen",
			wantLabel: "the-very-suspicious-name-with-precisely-sixty-three-characters",
		},
		{
			name:      "80 character qualified name",
			instance:  "team-" + strings.Repeat("n", 30) + "_" + strings.Repeat("x", 44),
			wantLabel: "team-" + strings.Repeat("n", 30) + "_" + strings.Repeat("x", 27),
		},
	}
	tr := newTestTracker()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			obj := newObj("v1", "Service", "default", "my-service")
			require.NoError(t, tr.SetAppInstance(obj, tc.instance, "default", MethodAnnotationAndLabel, ""))
			assert.Equal(t, tc.wantLabel, obj.GetLabels()[keyInstance])
			assert.LessOrEqual(t, len(obj.GetLabels()[keyInstance]), LabelMaxLength)
			// Ownership is read from the annotation, so the full name still matches.
			assert.Equal(t, tc.instance, tr.GetAppName(obj, MethodAnnotationAndLabel, ""))
			assert.Equal(t, tc.instance+":/Service:default/my-service", obj.GetAnnotations()[keyTracking])
		})
	}

	t.Run("cannot truncate", func(t *testing.T) {
		t.Parallel()
		obj := newObj("v1", "Service", "default", "my-service")
		before := obj.DeepCopy()
		err := tr.SetAppInstance(obj, strings.Repeat("-", 64), "default", MethodAnnotationAndLabel, "")
		require.ErrorIs(t, err, ErrLabelTruncation)
		assert.EqualError(t, err, "failed to set app instance label: unable to truncate label to not end with a special character")
		assert.Equal(t, before, obj, "object must be unchanged on error")
	})
}

func TestCustomInstanceLabelKey(t *testing.T) {
	t.Parallel()
	const custom = "cd.patrol.example/instance"
	tr := New(Config{InstanceLabelKey: custom, ControlPlaneNamespace: testControlPlane})

	obj := configMap("ns", "cm")
	require.NoError(t, tr.SetAppInstance(obj, "guestbook", "ns", MethodLabel, ""))
	assert.Equal(t, map[string]string{custom: "guestbook"}, obj.GetLabels())
	assert.Equal(t, "guestbook", tr.GetAppName(obj, MethodLabel, ""))

	// A Helm-style app.kubernetes.io/instance label is not tracking metadata with a custom key.
	helm := withLabels(configMap("ns", "cm"), keyInstance, "guestbook")
	assert.Empty(t, tr.GetAppName(helm, MethodLabel, ""))
	assert.False(t, tr.IsManagedBy(helm, App{Name: "guestbook", Namespace: testControlPlane}, MethodLabel, ""))

	obj = configMap("ns", "cm")
	require.NoError(t, tr.SetAppInstance(obj, "guestbook", "ns", MethodAnnotationAndLabel, ""))
	assert.Equal(t, map[string]string{custom: "guestbook"}, obj.GetLabels())
}

func TestSetAppInstanceErrors(t *testing.T) {
	t.Parallel()
	tr := newTestTracker()

	require.ErrorIs(t, tr.SetAppInstance(nil, "app", "ns", MethodAnnotation, ""), ErrNilObject)
	for _, m := range allMethods {
		obj := configMap("ns", "cm")
		require.ErrorIs(t, tr.SetAppInstance(obj, "", "ns", m, ""), ErrInvalidInstanceName, "method %q", m)
		assert.Equal(t, configMap("ns", "cm"), obj)
	}
	for _, m := range []Method{MethodAnnotation, MethodAnnotationAndLabel} {
		obj := configMap("ns", "cm")
		require.ErrorIs(t, tr.SetAppInstance(obj, "a:b", "ns", m, ""), ErrInvalidInstanceName)
		assert.Equal(t, configMap("ns", "cm"), obj)
	}

	badAnnotations := func() *unstructured.Unstructured {
		o := configMap("ns", "cm")
		o.Object["metadata"].(map[string]any)["annotations"] = map[string]any{"n": int64(1)}
		return o
	}
	badLabels := func() *unstructured.Unstructured {
		o := configMap("ns", "cm")
		o.Object["metadata"].(map[string]any)["labels"] = "not-a-map"
		return o
	}
	tests := []struct {
		name    string
		obj     func() *unstructured.Unstructured
		method  Method
		wantErr string
	}{
		{name: "annotations not strings", obj: badAnnotations, method: MethodAnnotation,
			wantErr: "failed to get annotations from target object /v1, Kind=ConfigMap ns/cm"},
		{name: "labels not a map (label)", obj: badLabels, method: MethodLabel,
			wantErr: "failed to set app instance label: failed to get labels from target object /v1, Kind=ConfigMap ns/cm"},
		{name: "labels not a map (annotation+label)", obj: badLabels, method: MethodAnnotationAndLabel,
			wantErr: "failed to set app instance label: failed to get labels from target object /v1, Kind=ConfigMap ns/cm"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			obj := tc.obj()
			err := tr.SetAppInstance(obj, "app", "ns", tc.method, "inst")
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.Equal(t, tc.obj(), obj, "object must be unchanged on error")
		})
	}
}

// TestInstallationID covers APP-074: the installation-id annotation is written with
// annotation-based methods and must match exactly for a resource to be attributed.
func TestInstallationID(t *testing.T) {
	t.Parallel()
	tr := newTestTracker()
	app := App{Name: "guestbook", Namespace: testControlPlane}

	for _, m := range []Method{MethodAnnotation, MethodAnnotationAndLabel} {
		t.Run(string(m), func(t *testing.T) {
			t.Parallel()
			obj := configMap("ns", "cm")
			require.NoError(t, tr.SetAppInstance(obj, "guestbook", "ns", m, "inst-a"))
			assert.Equal(t, "inst-a", obj.GetAnnotations()[keyInstallation])

			assert.Equal(t, "guestbook", tr.GetAppName(obj, m, "inst-a"))
			assert.Empty(t, tr.GetAppName(obj, m, "inst-b"), "other installation")
			assert.Empty(t, tr.GetAppName(obj, m, ""), "installation without ID must not adopt")
			assert.Nil(t, tr.GetAppInstance(obj, m, "inst-b"))
			assert.True(t, tr.IsManagedBy(obj, app, m, "inst-a"))
			assert.False(t, tr.IsManagedBy(obj, app, m, "inst-b"))
			_, shared := tr.DetectSharedResource(obj, App{Name: "other", Namespace: testControlPlane}, m, "inst-b")
			assert.False(t, shared, "another installation's resources are invisible, not shared")

			// Unsetting the installation ID removes the annotation on the next sync.
			require.NoError(t, tr.SetAppInstance(obj, "guestbook", "ns", m, ""))
			assert.NotContains(t, obj.GetAnnotations(), keyInstallation)
			assert.Equal(t, "guestbook", tr.GetAppName(obj, m, ""))
			assert.Empty(t, tr.GetAppName(obj, m, "inst-a"), "previously synced resources look unowned after setting an ID")
		})
	}

	t.Run("label tracking ignores the installation ID", func(t *testing.T) {
		t.Parallel()
		obj := configMap("ns", "cm")
		require.NoError(t, tr.SetAppInstance(obj, "guestbook", "ns", MethodLabel, "inst-a"))
		assert.NotContains(t, obj.GetAnnotations(), keyInstallation)
		assert.Equal(t, "guestbook", tr.GetAppName(obj, MethodLabel, "inst-b"))
	})
}

// TestInstallationIsolation: two installations with identically named Applications share a
// cluster and never see each other's resources.
func TestInstallationIsolation(t *testing.T) {
	t.Parallel()
	a, b := newTestTracker(), newTestTracker()
	app := App{Name: "guestbook", Namespace: testControlPlane}

	fromA := configMap("default", "cm-a")
	require.NoError(t, a.SetAppInstance(fromA, a.InstanceName(app), "default", MethodAnnotation, "inst-a"))
	fromB := configMap("default", "cm-b")
	require.NoError(t, b.SetAppInstance(fromB, b.InstanceName(app), "default", MethodAnnotation, "inst-b"))

	assert.True(t, a.IsManagedBy(fromA, app, MethodAnnotation, "inst-a"))
	assert.False(t, a.IsManagedBy(fromB, app, MethodAnnotation, "inst-a"))
	assert.True(t, b.IsManagedBy(fromB, app, MethodAnnotation, "inst-b"))
	assert.False(t, b.IsManagedBy(fromA, app, MethodAnnotation, "inst-b"))
	assert.Empty(t, a.GetAppName(fromB, MethodAnnotation, "inst-a"))
	assert.Empty(t, b.GetAppName(fromA, MethodAnnotation, "inst-b"))
}

func TestGetAppNameUntrackedOrMalformed(t *testing.T) {
	t.Parallel()
	tr := newTestTracker()
	malformedAnnotations := configMap("ns", "cm")
	malformedAnnotations.Object["metadata"].(map[string]any)["annotations"] = map[string]any{keyTracking: int64(3)}
	malformedLabels := configMap("ns", "cm")
	malformedLabels.Object["metadata"].(map[string]any)["labels"] = []any{"x"}
	badMetadata := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": "x"}}

	tests := []struct {
		name   string
		obj    *unstructured.Unstructured
		method Method
	}{
		{name: "nil object", obj: nil, method: MethodAnnotation},
		{name: "nil object (label)", obj: nil, method: MethodLabel},
		{name: "no metadata", obj: configMap("ns", "cm"), method: MethodAnnotation},
		{name: "no metadata (label)", obj: configMap("ns", "cm"), method: MethodLabel},
		{name: "legacy label value as tracking id", obj: withAnnotations(configMap("ns", "cm"), keyTracking, "guestbook"), method: MethodAnnotation},
		{name: "wrong separators", obj: withAnnotations(configMap("ns", "cm"), keyTracking, "guestbook;/ConfigMap/ns/cm"), method: MethodAnnotation},
		{name: "missing name", obj: withAnnotations(configMap("ns", "cm"), keyTracking, "guestbook:/ConfigMap:ns"), method: MethodAnnotationAndLabel},
		{name: "empty tracking id", obj: withAnnotations(configMap("ns", "cm"), keyTracking, ""), method: MethodAnnotation},
		{name: "annotation values not strings", obj: malformedAnnotations, method: MethodAnnotation},
		{name: "labels not a map", obj: malformedLabels, method: MethodLabel},
		{name: "metadata not a map", obj: badMetadata, method: MethodAnnotation},
		{name: "metadata not a map (label)", obj: badMetadata, method: MethodLabel},
		// APP-070 edge case: label-only metadata grants no ownership under annotation tracking.
		{name: "label only under annotation tracking", obj: withLabels(configMap("ns", "cm"), keyInstance, "guestbook"), method: MethodAnnotation},
		{name: "label only under annotation+label tracking", obj: withLabels(configMap("ns", "cm"), keyInstance, "guestbook"), method: MethodAnnotationAndLabel},
		// And the tracking annotation grants none under label tracking.
		{name: "annotation only under label tracking", obj: withAnnotations(configMap("ns", "cm"), keyTracking, "guestbook:/ConfigMap:ns/cm"), method: MethodLabel},
	}
	app := App{Name: "guestbook", Namespace: testControlPlane}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Empty(t, tr.GetAppName(tc.obj, tc.method, ""))
			assert.Nil(t, tr.GetAppInstance(tc.obj, tc.method, ""))
			assert.False(t, tr.IsManagedBy(tc.obj, app, tc.method, ""))
			_, shared := tr.DetectSharedResource(tc.obj, app, tc.method, "")
			assert.False(t, shared)
		})
	}
}

// TestIsSelfReferencedObject covers APP-075/SYNC-080: copied resources whose tracking id
// names another object are not part of the Application.
func TestIsSelfReferencedObject(t *testing.T) {
	t.Parallel()
	tr := newTestTracker()
	tracked := func(apiVersion, kind, ns, name, id string) *unstructured.Unstructured {
		return withAnnotations(newObj(apiVersion, kind, ns, name), keyTracking, id)
	}
	tests := []struct {
		name    string
		live    *unstructured.Unstructured
		target  *unstructured.Unstructured
		method  Method
		install string
		want    bool
	}{
		{name: "nil live", live: nil, method: MethodAnnotation, want: true},
		{name: "own tracking id", live: tracked("v1", "Secret", "ns-a", "s", "app:/Secret:ns-a/s"), method: MethodAnnotation, want: true},
		{name: "copied to another namespace", live: tracked("v1", "Secret", "ns-b", "s", "app:/Secret:ns-a/s"), method: MethodAnnotation, want: false},
		{name: "copied to another namespace (annotation+label)", live: tracked("v1", "Secret", "ns-b", "s", "app:/Secret:ns-a/s"), method: MethodAnnotationAndLabel, want: false},
		{name: "copied to another name", live: tracked("v1", "Secret", "ns-a", "s-copy", "app:/Secret:ns-a/s"), method: MethodAnnotation, want: false},
		{name: "copied to another kind", live: tracked("v1", "ConfigMap", "ns-a", "s", "app:/Secret:ns-a/s"), method: MethodAnnotation, want: false},
		{name: "other group", live: tracked("networking.k8s.io/v1", "Ingress", "default", "ing", "app:extensions/Ingress:default/ing"), method: MethodAnnotation, want: false},
		{
			name:   "group migration resolved by the desired object",
			live:   tracked("networking.k8s.io/v1", "Ingress", "default", "ing", "app:extensions/Ingress:default/ing"),
			target: newObj("networking.k8s.io/v1", "Ingress", "default", "ing"),
			method: MethodAnnotation, want: true,
		},
		{
			name:   "desired object takes precedence over the live id",
			live:   tracked("v1", "Secret", "ns-a", "s", "app:/Secret:elsewhere/other"),
			target: newObj("v1", "Secret", "ns-a", "s"),
			method: MethodAnnotation, want: true,
		},
		{name: "cluster-scoped ignores the namespace in the id", live: tracked("rbac.authorization.k8s.io/v1", "ClusterRole", "", "r", "app:rbac.authorization.k8s.io/ClusterRole:dest/r"), method: MethodAnnotation, want: true},
		{name: "label tracking cannot tell", live: tracked("v1", "Secret", "ns-b", "s", "app:/Secret:ns-a/s"), method: MethodLabel, want: true},
		{name: "no tracking id is assumed self-referenced", live: newObj("v1", "Secret", "ns-a", "s"), method: MethodAnnotation, want: true},
		{name: "malformed tracking id is assumed self-referenced", live: tracked("v1", "Secret", "ns-b", "s", "garbage"), method: MethodAnnotation, want: true},
		{name: "other installation's id is not read", live: tracked("v1", "Secret", "ns-b", "s", "app:/Secret:ns-a/s"), method: MethodAnnotation, install: "inst", want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, tr.IsSelfReferencedObject(tc.live, tc.target, tc.method, tc.install))
		})
	}
}

// TestCopiedResourceIsNotManaged: a Secret copied (e.g. by a replication controller) with its
// tracking annotation into another namespace is tracked by the app but neither managed nor
// prunable.
func TestCopiedResourceIsNotManaged(t *testing.T) {
	t.Parallel()
	tr := newTestTracker()
	app := App{Name: "guestbook", Namespace: testControlPlane}
	orig := newObj("v1", "Secret", "ns-a", "creds")
	require.NoError(t, tr.SetAppInstance(orig, tr.InstanceName(app), "ns-a", MethodAnnotation, ""))

	cp := orig.DeepCopy()
	cp.SetNamespace("ns-b")
	assert.Equal(t, "guestbook", tr.GetAppName(cp, MethodAnnotation, ""), "still attributed to the app")
	assert.False(t, tr.IsSelfReferencedObject(cp, nil, MethodAnnotation, ""))
	assert.False(t, tr.IsManagedBy(cp, app, MethodAnnotation, ""))
	assert.True(t, tr.IsManagedBy(orig, app, MethodAnnotation, ""))
}

func TestIsManagedBy(t *testing.T) {
	t.Parallel()
	tr := newTestTracker()
	team := App{Name: "web", Namespace: "team"}
	cp := App{Name: "web", Namespace: testControlPlane}

	obj := configMap("ns", "cm")
	require.NoError(t, tr.SetAppInstance(obj, tr.InstanceName(team), "ns", MethodAnnotation, ""))
	assert.True(t, tr.IsManagedBy(obj, team, MethodAnnotation, ""))
	assert.False(t, tr.IsManagedBy(obj, cp, MethodAnnotation, ""), "same name in the control-plane namespace is another app")
	assert.False(t, tr.IsManagedBy(obj, App{Name: "web", Namespace: "team2"}, MethodAnnotation, ""))
	assert.False(t, tr.IsManagedBy(obj, App{}, MethodAnnotation, ""))
	assert.False(t, tr.IsManagedBy(nil, team, MethodAnnotation, ""))
}

func withOwner(obj *unstructured.Unstructured, apiVersion, kind, name, uid string) *unstructured.Unstructured {
	obj.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: apiVersion, Kind: kind, Name: name, UID: types.UID(uid), Controller: ptr.To(true),
	}})
	return obj
}

// TestIsManagedByOwnedChild (DIFF-076 negative case): a child carrying its owner's tracking
// metadata (a Pod inheriting the pod-template instance label, or any tracked object with an
// ownerReference) is attributed to the app and self-referenced, but it is not a managed root,
// so it must never be pruned.
func TestIsManagedByOwnedChild(t *testing.T) {
	t.Parallel()
	tr := newTestTracker()
	app := App{Name: "guestbook", Namespace: testControlPlane}

	t.Run("label inherited from the pod template", func(t *testing.T) {
		t.Parallel()
		pod := withLabels(newObj("v1", "Pod", "default", "web-7d9f-abcde"), keyInstance, "guestbook", "pod-template-hash", "7d9f")
		withOwner(pod, "apps/v1", "ReplicaSet", "web-7d9f", "rs-uid")
		assert.Equal(t, "guestbook", tr.GetAppName(pod, MethodLabel, ""))
		assert.True(t, tr.IsSelfReferencedObject(pod, nil, MethodLabel, ""))
		assert.False(t, tr.IsManagedBy(pod, app, MethodLabel, ""))

		root := withLabels(newObj("apps/v1", "Deployment", "default", "web"), keyInstance, "guestbook")
		assert.True(t, tr.IsManagedBy(root, app, MethodLabel, ""))
	})

	for _, m := range allMethods {
		t.Run("method="+string(m), func(t *testing.T) {
			t.Parallel()
			obj := newObj("apps/v1", "ReplicaSet", "default", "web-7d9f")
			require.NoError(t, tr.SetAppInstance(obj, tr.InstanceName(app), "default", m, "inst"))
			require.True(t, tr.IsManagedBy(obj, app, m, "inst"))

			owned := withOwner(obj.DeepCopy(), "apps/v1", "Deployment", "web", "dep-uid")
			assert.Equal(t, "guestbook", tr.GetAppName(owned, m, "inst"), "still attributed to the app")
			assert.True(t, tr.IsSelfReferencedObject(owned, nil, m, "inst"))
			assert.False(t, tr.IsManagedBy(owned, app, m, "inst"))
		})
	}
}

// TestIsManagedByOwnerReferenceShapes: IsManagedBy fails closed on ownerReferences it cannot
// read as a list, while null and an empty list mean no owners.
func TestIsManagedByOwnerReferenceShapes(t *testing.T) {
	t.Parallel()
	tr := newTestTracker()
	app := App{Name: "guestbook", Namespace: testControlPlane}
	tests := []struct {
		name  string
		refs  any
		unset bool
		want  bool
	}{
		{name: "absent", unset: true, want: true},
		{name: "null", refs: nil, want: true},
		{name: "empty list", refs: []any{}, want: true},
		{name: "one owner", refs: []any{map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "name": "p", "uid": "u"}}, want: false},
		{name: "list of non-maps", refs: []any{"owner"}, want: false},
		{name: "string", refs: "owner", want: false},
		{name: "map", refs: map[string]any{"kind": "ConfigMap"}, want: false},
		{name: "empty map", refs: map[string]any{}, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, m := range allMethods {
				obj := configMap("ns", "cm")
				require.NoError(t, tr.SetAppInstance(obj, "guestbook", "ns", m, ""))
				if !tc.unset {
					obj.Object["metadata"].(map[string]any)["ownerReferences"] = tc.refs
				}
				assert.Equal(t, tc.want, tr.IsManagedBy(obj, app, m, ""), "method %q", m)
			}
		})
	}
}

// TestIsManagedByAcceptsSelfConsistentForgedID pins the documented limit of IsManagedBy: it
// reads only the object's own metadata, so whoever can write an object can make it look
// managed by any Application of the installation. The installation ID is not a secret either.
// AppProject restrictions, not tracking, keep an Application from pruning foreign resources.
func TestIsManagedByAcceptsSelfConsistentForgedID(t *testing.T) {
	t.Parallel()
	tr := newTestTracker()
	victim := App{Name: "victim", Namespace: testControlPlane}
	forged := withAnnotations(configMap("tenant-ns", "not-from-git"),
		keyTracking, "victim:/ConfigMap:tenant-ns/not-from-git", keyInstallation, "inst")
	assert.True(t, tr.IsManagedBy(forged, victim, MethodAnnotation, "inst"))

	// What is rejected: an id naming another object, or another installation ID.
	assert.False(t, tr.IsManagedBy(withAnnotations(forged.DeepCopy(), keyTracking, "victim:/ConfigMap:tenant-ns/other"), victim, MethodAnnotation, "inst"))
	assert.False(t, tr.IsManagedBy(forged, victim, MethodAnnotation, "other-inst"))
}

// TestDetectSharedResource covers APP-038/APP-077/SYNC-082 detection and message wording.
func TestDetectSharedResource(t *testing.T) {
	t.Parallel()
	tr := newTestTracker()
	app1 := App{Name: "app1", Namespace: testControlPlane}
	app2 := App{Name: "app2", Namespace: testControlPlane}
	teamApp := App{Name: "app1", Namespace: "team"}

	t.Run("two apps render the same ConfigMap", func(t *testing.T) {
		t.Parallel()
		live := configMap("default", "shared")
		require.NoError(t, tr.SetAppInstance(live, tr.InstanceName(app1), "default", MethodAnnotation, ""))
		other, shared := tr.DetectSharedResource(live, app2, MethodAnnotation, "")
		require.True(t, shared)
		assert.Equal(t, "app1", other)
		assert.Equal(t, "ConfigMap/shared is part of applications patrol-cd/app2 and app1", SharedResourceMessage(live, app2, other))
		_, shared = tr.DetectSharedResource(live, app1, MethodAnnotation, "")
		assert.False(t, shared, "the owner sees no conflict")
	})
	t.Run("owner in another namespace is displayed qualified", func(t *testing.T) {
		t.Parallel()
		live := newObj("apps/v1", "Deployment", "default", "web")
		require.NoError(t, tr.SetAppInstance(live, tr.InstanceName(teamApp), "default", MethodAnnotationAndLabel, ""))
		other, shared := tr.DetectSharedResource(live, app2, MethodAnnotationAndLabel, "")
		require.True(t, shared)
		assert.Equal(t, "team_app1", other)
		assert.Equal(t, "Deployment/web is part of applications patrol-cd/app2 and team/app1", SharedResourceMessage(live, app2, other))
	})
	t.Run("same name in another namespace is a different app", func(t *testing.T) {
		t.Parallel()
		live := configMap("default", "cm")
		require.NoError(t, tr.SetAppInstance(live, tr.InstanceName(teamApp), "default", MethodAnnotation, ""))
		other, shared := tr.DetectSharedResource(live, app1, MethodAnnotation, "")
		assert.True(t, shared)
		assert.Equal(t, "team_app1", other)
	})
	t.Run("label tracking: any foreign instance label value", func(t *testing.T) {
		t.Parallel()
		live := withLabels(configMap("default", "cm"), keyInstance, "my-helm-release")
		other, shared := tr.DetectSharedResource(live, app1, MethodLabel, "")
		assert.True(t, shared)
		assert.Equal(t, "ConfigMap/cm is part of applications patrol-cd/app1 and my-helm-release", SharedResourceMessage(live, app1, other))
	})
	t.Run("untracked", func(t *testing.T) {
		t.Parallel()
		_, shared := tr.DetectSharedResource(configMap("default", "cm"), app1, MethodAnnotation, "")
		assert.False(t, shared)
		_, shared = tr.DetectSharedResource(nil, app1, MethodAnnotation, "")
		assert.False(t, shared)
	})
	t.Run("app without namespace", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, "ConfigMap/cm is part of applications app2 and app1",
			SharedResourceMessage(configMap("default", "cm"), App{Name: "app2"}, "app1"))
		assert.Equal(t, "/ is part of applications app2 and app1", SharedResourceMessage(nil, App{Name: "app2"}, "app1"))
	})
}

func TestRemoveAppInstance(t *testing.T) {
	t.Parallel()
	const custom = "cd.patrol.example/instance"
	tracked := func() *unstructured.Unstructured {
		o := configMap("ns", "cm")
		withAnnotations(o, keyTracking, "app:/ConfigMap:ns/cm", keyInstallation, "inst", "other", "x")
		withLabels(o, keyInstance, "app", custom, "app", "keep", "y")
		return o
	}
	tests := []struct {
		method          Method
		wantAnnotations map[string]string
		wantLabels      map[string]string
	}{
		{method: "", wantAnnotations: map[string]string{"other": "x"}, wantLabels: map[string]string{keyInstance: "app", custom: "app", "keep": "y"}},
		{method: MethodAnnotation, wantAnnotations: map[string]string{"other": "x"}, wantLabels: map[string]string{keyInstance: "app", custom: "app", "keep": "y"}},
		// The default instance label is removed even when a custom key is configured (upstream).
		{method: MethodLabel, wantAnnotations: map[string]string{keyTracking: "app:/ConfigMap:ns/cm", keyInstallation: "inst", "other": "x"}, wantLabels: map[string]string{custom: "app", "keep": "y"}},
		{method: MethodAnnotationAndLabel, wantAnnotations: map[string]string{"other": "x"}, wantLabels: map[string]string{custom: "app", "keep": "y"}},
	}
	tr := New(Config{InstanceLabelKey: custom})
	for _, tc := range tests {
		t.Run(string(tc.method), func(t *testing.T) {
			t.Parallel()
			obj := tracked()
			require.NoError(t, tr.RemoveAppInstance(obj, tc.method))
			assert.Equal(t, tc.wantAnnotations, obj.GetAnnotations())
			assert.Equal(t, tc.wantLabels, obj.GetLabels())
		})
	}

	t.Run("empty maps are dropped", func(t *testing.T) {
		t.Parallel()
		obj := configMap("ns", "cm")
		require.NoError(t, newTestTracker().SetAppInstance(obj, "app", "ns", MethodAnnotationAndLabel, "inst"))
		require.NoError(t, newTestTracker().RemoveAppInstance(obj, MethodAnnotationAndLabel))
		assert.Equal(t, configMap("ns", "cm"), obj)
	})
	t.Run("untracked object", func(t *testing.T) {
		t.Parallel()
		obj := configMap("ns", "cm")
		require.NoError(t, tr.RemoveAppInstance(obj, MethodAnnotationAndLabel))
		assert.Equal(t, configMap("ns", "cm"), obj)
	})
	t.Run("errors", func(t *testing.T) {
		t.Parallel()
		require.ErrorIs(t, tr.RemoveAppInstance(nil, MethodAnnotation), ErrNilObject)
		bad := configMap("ns", "cm")
		bad.Object["metadata"].(map[string]any)["annotations"] = "x"
		require.ErrorContains(t, tr.RemoveAppInstance(bad, MethodAnnotation), "failed to get annotations for /v1, Kind=ConfigMap ns/cm")
		bad = configMap("ns", "cm")
		bad.Object["metadata"].(map[string]any)["labels"] = "x"
		require.ErrorContains(t, tr.RemoveAppInstance(bad, MethodLabel), "failed to get labels for /v1, Kind=ConfigMap ns/cm")
	})
}
