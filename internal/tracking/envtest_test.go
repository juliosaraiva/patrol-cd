package tracking

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/utils/ptr"

	"github.com/juliosaraiva/patrol-cd/internal/diff"
)

// kubeClient is a minimal dynamic client for the kinds used by the integration tests.
type kubeClient struct{ dyn dynamic.Interface }

var testGVRs = map[string]schema.GroupVersionResource{
	"Namespace":   {Version: "v1", Resource: "namespaces"},
	"ConfigMap":   {Version: "v1", Resource: "configmaps"},
	"Secret":      {Version: "v1", Resource: "secrets"},
	"ClusterRole": {Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"},
	"Deployment":  {Group: "apps", Version: "v1", Resource: "deployments"},
	"ReplicaSet":  {Group: "apps", Version: "v1", Resource: "replicasets"},
}

func newKubeClient(t *testing.T) *kubeClient {
	t.Helper()
	dyn, err := dynamic.NewForConfig(envConfig(t))
	require.NoError(t, err)
	return &kubeClient{dyn: dyn}
}

func (k *kubeClient) resource(t *testing.T, obj *unstructured.Unstructured) dynamic.ResourceInterface {
	t.Helper()
	gvr, ok := testGVRs[obj.GetKind()]
	require.True(t, ok, "no GVR for kind %s", obj.GetKind())
	if obj.GetNamespace() != "" {
		return k.dyn.Resource(gvr).Namespace(obj.GetNamespace())
	}
	return k.dyn.Resource(gvr)
}

func (k *kubeClient) create(ctx context.Context, t *testing.T, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	t.Helper()
	return k.resource(t, obj).Create(ctx, obj, metav1.CreateOptions{})
}

func (k *kubeClient) mustCreate(ctx context.Context, t *testing.T, obj *unstructured.Unstructured) *unstructured.Unstructured {
	t.Helper()
	created, err := k.create(ctx, t, obj)
	require.NoError(t, err)
	// Read back through a GET so assertions use what the API server stored.
	live, err := k.resource(t, obj).Get(ctx, created.GetName(), metav1.GetOptions{})
	require.NoError(t, err)
	return live
}

func (k *kubeClient) namespace(ctx context.Context, t *testing.T, name string) {
	t.Helper()
	_, err := k.create(ctx, t, newObj("v1", "Namespace", "", name))
	require.NoError(t, err)
}

func withData(obj *unstructured.Unstructured, kv ...string) *unstructured.Unstructured {
	data := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		data[kv[i]] = kv[i+1]
	}
	obj.Object["data"] = data
	return obj
}

// TestEnvtestAnnotationTracking writes tracking metadata with annotation tracking, stores the
// objects in a real API server and reads ownership back from the stored objects: exact id
// format (namespaced, cluster-scoped, apps in any namespace), copied resources and
// installation ID isolation (APP-069, APP-070, APP-074, APP-075).
func TestEnvtestAnnotationTracking(t *testing.T) {
	t.Parallel()
	k := newKubeClient(t)
	ctx := t.Context()
	k.namespace(ctx, t, "trk-ann-dest")
	k.namespace(ctx, t, "trk-ann-copy")

	tr := New(Config{ControlPlaneNamespace: "patrol-cd"})
	app := App{Name: "web", Namespace: "team"}
	instance := tr.InstanceName(app)
	require.Equal(t, "team_web", instance)

	// Namespaced resource rendered without a namespace gets the destination namespace first
	// (the controller's target normalization), then tracking.
	cm := withData(configMap("trk-ann-dest", "settings"), "k", "v")
	require.NoError(t, tr.SetAppInstance(cm, instance, "trk-ann-dest", MethodAnnotation, "inst-1"))
	liveCM := k.mustCreate(ctx, t, cm)
	assert.Equal(t, "team_web:/ConfigMap:trk-ann-dest/settings", liveCM.GetAnnotations()[keyTracking])
	assert.Equal(t, "inst-1", liveCM.GetAnnotations()[keyInstallation])
	assert.Equal(t, instance, tr.GetAppName(liveCM, MethodAnnotation, "inst-1"))
	assert.True(t, tr.IsManagedBy(liveCM, app, MethodAnnotation, "inst-1"))

	// Cluster-scoped resource records the destination namespace and is still self-referenced.
	cr := newObj("rbac.authorization.k8s.io/v1", "ClusterRole", "", "trk-ann-reader")
	require.NoError(t, tr.SetAppInstance(cr, instance, "trk-ann-dest", MethodAnnotation, "inst-1"))
	liveCR := k.mustCreate(ctx, t, cr)
	assert.Equal(t, "team_web:rbac.authorization.k8s.io/ClusterRole:trk-ann-dest/trk-ann-reader", liveCR.GetAnnotations()[keyTracking])
	assert.True(t, tr.IsSelfReferencedObject(liveCR, nil, MethodAnnotation, "inst-1"))
	assert.True(t, tr.IsManagedBy(liveCR, app, MethodAnnotation, "inst-1"))

	// A replication tool copies the Secret, annotations included, into another namespace.
	secret := withData(newObj("v1", "Secret", "trk-ann-dest", "creds"), "password", "c2VjcmV0")
	require.NoError(t, tr.SetAppInstance(secret, instance, "trk-ann-dest", MethodAnnotation, "inst-1"))
	liveSecret := k.mustCreate(ctx, t, secret)
	copied := withData(newObj("v1", "Secret", "trk-ann-copy", "creds"), "password", "c2VjcmV0")
	copied.SetAnnotations(liveSecret.GetAnnotations())
	liveCopy := k.mustCreate(ctx, t, copied)
	assert.Equal(t, instance, tr.GetAppName(liveCopy, MethodAnnotation, "inst-1"), "copy is attributed to the app")
	assert.False(t, tr.IsSelfReferencedObject(liveCopy, nil, MethodAnnotation, "inst-1"), "copy is not self-referenced")
	assert.False(t, tr.IsManagedBy(liveCopy, app, MethodAnnotation, "inst-1"), "copy must never be pruned")
	assert.True(t, tr.IsManagedBy(liveSecret, app, MethodAnnotation, "inst-1"))

	// A second installation with an identically named app sees none of these resources.
	for _, live := range []*unstructured.Unstructured{liveCM, liveCR, liveSecret} {
		assert.Empty(t, tr.GetAppName(live, MethodAnnotation, "inst-2"))
		assert.False(t, tr.IsManagedBy(live, app, MethodAnnotation, "inst-2"))
		_, shared := tr.DetectSharedResource(live, App{Name: "other", Namespace: "patrol-cd"}, MethodAnnotation, "inst-2")
		assert.False(t, shared)
	}

	// Another app of the same installation that renders the ConfigMap detects the conflict.
	other, shared := tr.DetectSharedResource(liveCM, App{Name: "web", Namespace: "patrol-cd"}, MethodAnnotation, "inst-1")
	require.True(t, shared)
	assert.Equal(t, "ConfigMap/settings is part of applications patrol-cd/web and team/web",
		SharedResourceMessage(liveCM, App{Name: "web", Namespace: "patrol-cd"}, other))
}

// TestEnvtestLabelLimits: label tracking writes the instance name untruncated, so the API
// server rejects names over 63 characters (APP-071); annotation+label truncates the label
// and keeps ownership through the annotation (APP-072).
func TestEnvtestLabelLimits(t *testing.T) {
	t.Parallel()
	k := newKubeClient(t)
	ctx := t.Context()
	k.namespace(ctx, t, "trk-label")
	tr := New(Config{ControlPlaneNamespace: "patrol-cd"})

	long70 := "team-" + strings.Repeat("a", 20) + "_" + strings.Repeat("b", 44)
	require.Len(t, long70, 70)
	labelOnly := configMap("trk-label", "label-only")
	require.NoError(t, tr.SetAppInstance(labelOnly, long70, "trk-label", MethodLabel, ""))
	_, err := k.create(ctx, t, labelOnly)
	require.Error(t, err)
	assert.True(t, apierrors.IsInvalid(err), "want Invalid, got %v", err)
	assert.Contains(t, err.Error(), "must be no more than 63")

	long80 := "team-" + strings.Repeat("n", 30) + "_" + strings.Repeat("x", 44)
	both := configMap("trk-label", "both")
	require.NoError(t, tr.SetAppInstance(both, long80, "trk-label", MethodAnnotationAndLabel, ""))
	live := k.mustCreate(ctx, t, both)
	label := live.GetLabels()[keyInstance]
	assert.Len(t, label, LabelMaxLength)
	assert.True(t, strings.HasPrefix(long80, label))
	assert.Equal(t, long80, tr.GetAppName(live, MethodAnnotationAndLabel, ""))

	short := configMap("trk-label", "short")
	require.NoError(t, tr.SetAppInstance(short, "guestbook", "trk-label", MethodLabel, ""))
	live = k.mustCreate(ctx, t, short)
	assert.Equal(t, "guestbook", tr.GetAppName(live, MethodLabel, ""))
	assert.True(t, tr.IsManagedBy(live, App{Name: "guestbook", Namespace: "patrol-cd"}, MethodLabel, ""))
}

// TestEnvtestTrackingMethodMigration: resources last synced with label tracking stay in sync
// after switching to annotation tracking once live objects are normalized, while a live
// tracking id of another app still shows as a difference (APP-076, DIFF-026). Live objects
// come from a real API server and the comparison uses internal/diff.
func TestEnvtestTrackingMethodMigration(t *testing.T) {
	t.Parallel()
	k := newKubeClient(t)
	ctx := t.Context()
	k.namespace(ctx, t, "trk-migrate")
	tr := New(Config{ControlPlaneNamespace: "patrol-cd"})

	render := func(name string) *unstructured.Unstructured {
		return withData(configMap("trk-migrate", name), "key", "value")
	}
	modified := func(target, live *unstructured.Unstructured, o diff.Options) bool {
		t.Helper()
		res, err := diff.Diff(ctx, target, live, o)
		require.NoError(t, err)
		if res.Modified {
			t.Logf("diff: normalized live %s, predicted live %s", res.NormalizedLive, res.PredictedLive)
		}
		return res.Modified
	}

	// Synced earlier with label tracking.
	old := render("migrated")
	require.NoError(t, tr.SetAppInstance(old, "guestbook", "trk-migrate", MethodLabel, ""))
	live := k.mustCreate(ctx, t, old)

	// The setting is now annotation tracking.
	target := render("migrated")
	require.NoError(t, tr.SetAppInstance(target, "guestbook", "trk-migrate", MethodAnnotation, ""))
	require.True(t, modified(target, live, diff.DefaultOptions()), "without normalization the missing annotation is drift")

	normalized := live.DeepCopy()
	require.NoError(t, tr.Normalize(target, normalized, MethodAnnotation))
	assert.False(t, modified(target, normalized, diff.DefaultOptions()), "normalized live must be in sync")
	assert.NotContains(t, normalized.GetLabels(), keyInstance)

	o := diff.DefaultOptions()
	lk, ak := tr.DiffTrackingKeys(MethodAnnotation)
	o.Tracking = diff.TrackingOptions{LabelKey: lk, AnnotationKey: ak}
	assert.False(t, modified(target, live, o), "diff's built-in tracking normalization with the tracker's keys")

	// A resource moved from app-a (annotation+label) to guestbook still differs.
	moved := render("moved")
	require.NoError(t, tr.SetAppInstance(moved, "app-a", "trk-migrate", MethodAnnotationAndLabel, ""))
	liveMoved := k.mustCreate(ctx, t, moved)
	targetMoved := render("moved")
	require.NoError(t, tr.SetAppInstance(targetMoved, "guestbook", "trk-migrate", MethodAnnotation, ""))
	normalizedMoved := liveMoved.DeepCopy()
	require.NoError(t, tr.Normalize(targetMoved, normalizedMoved, MethodAnnotation))
	assert.True(t, modified(targetMoved, normalizedMoved, diff.DefaultOptions()), "stale tracking id must be a diff")
	assert.Equal(t, "app-a:/ConfigMap:trk-migrate/moved", normalizedMoved.GetAnnotations()[keyTracking])

	// After the next sync the live object carries the tracking id.
	apply := target.DeepCopy()
	apply.SetResourceVersion(live.GetResourceVersion())
	updated, err := k.resource(t, apply).Update(ctx, apply, metav1.UpdateOptions{})
	require.NoError(t, err)
	assert.Equal(t, "guestbook:/ConfigMap:trk-migrate/migrated", updated.GetAnnotations()[keyTracking])
	assert.Equal(t, "guestbook", tr.GetAppName(updated, MethodAnnotation, ""))
	assert.False(t, modified(target, updated, diff.DefaultOptions()))
}

// TestEnvtestOwnedChildIsNotManaged (DIFF-076 negative case): under label tracking a
// ReplicaSet created for a Deployment carries the pod-template instance label and an
// ownerReference, as the deployment controller writes it. Both objects are stored in a real
// API server; the ReplicaSet is attributed to the app but is not a managed root, so it is
// never pruned, while the Deployment is.
func TestEnvtestOwnedChildIsNotManaged(t *testing.T) {
	t.Parallel()
	k := newKubeClient(t)
	ctx := t.Context()
	const ns = "trk-owned"
	k.namespace(ctx, t, ns)
	tr := New(Config{ControlPlaneNamespace: "patrol-cd"})
	app := App{Name: "guestbook", Namespace: "patrol-cd"}

	podTemplate := func(labels map[string]any) map[string]any {
		return map[string]any{
			"metadata": map[string]any{"labels": labels},
			"spec": map[string]any{"containers": []any{
				map[string]any{"name": "web", "image": "registry.example/web:1"},
			}},
		}
	}
	templateLabels := map[string]any{"app": "web", keyInstance: "guestbook"}

	dep := newObj("apps/v1", "Deployment", ns, "web")
	dep.Object["spec"] = map[string]any{
		"selector": map[string]any{"matchLabels": map[string]any{"app": "web"}},
		"template": podTemplate(templateLabels),
	}
	require.NoError(t, tr.SetAppInstance(dep, tr.InstanceName(app), ns, MethodLabel, ""))
	liveDep := k.mustCreate(ctx, t, dep)

	rs := newObj("apps/v1", "ReplicaSet", ns, "web-7d9f")
	rs.SetLabels(map[string]string{"app": "web", keyInstance: "guestbook", "pod-template-hash": "7d9f"})
	rs.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: "apps/v1", Kind: "Deployment", Name: liveDep.GetName(), UID: liveDep.GetUID(),
		Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true),
	}})
	rs.Object["spec"] = map[string]any{
		"selector": map[string]any{"matchLabels": map[string]any{"app": "web", "pod-template-hash": "7d9f"}},
		"template": podTemplate(map[string]any{"app": "web", keyInstance: "guestbook", "pod-template-hash": "7d9f"}),
	}
	liveRS := k.mustCreate(ctx, t, rs)
	require.Len(t, liveRS.GetOwnerReferences(), 1)

	assert.True(t, tr.IsManagedBy(liveDep, app, MethodLabel, ""))
	assert.Equal(t, "guestbook", tr.GetAppName(liveRS, MethodLabel, ""), "child inherits the instance label")
	assert.True(t, tr.IsSelfReferencedObject(liveRS, nil, MethodLabel, ""))
	assert.False(t, tr.IsManagedBy(liveRS, app, MethodLabel, ""), "owned child must never be pruned")
}
