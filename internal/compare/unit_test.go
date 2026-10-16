package compare

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"pgregory.net/rapid"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/tracking"
)

func staticScope(clusterScoped ...string) scopeFunc {
	cs := map[string]bool{}
	for _, k := range clusterScoped {
		cs[k] = true
	}
	return func(gk schema.GroupKind) (bool, error) {
		if gk.Kind == "Unknown" {
			return false, errors.New("unknown kind")
		}
		return !cs[gk.Kind], nil
	}
}

func TestNormalizeTargets(t *testing.T) {
	t.Parallel()
	tr := tracking.New(tracking.Config{ControlPlaneNamespace: "patrol"})
	set := func(u *unstructured.Unstructured) error {
		return tr.SetAppInstance(u, "app", "dest", tracking.MethodAnnotation, "")
	}
	cr := obj(t, `{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"ClusterRole","metadata":{"name":"cr","namespace":"wrong"}}`)
	cm := configMap("cm", "", map[string]string{"a": "1"})
	cmOther := configMap("cm", "other", nil)
	unknown := obj(t, `{"apiVersion":"example.com/v1","kind":"Unknown","metadata":{"name":"u"}}`)
	gen := obj(t, `{"apiVersion":"batch/v1","kind":"Job","metadata":{"generateName":"job-"}}`)
	dup1 := configMap("dup", "dest", map[string]string{"v": "first"})
	dup2 := configMap("dup", "", map[string]string{"v": "second"})

	out, conds, err := normalizeTargets("dest", []*unstructured.Unstructured{cr, cm, nil, cmOther, unknown, gen, dup1, dup2}, staticScope("ClusterRole"), set)
	require.NoError(t, err)
	require.Len(t, out, 6)
	assert.Empty(t, out[0].GetNamespace(), "cluster-scoped namespace is stripped")
	assert.Equal(t, "dest", out[1].GetNamespace(), "namespaced kinds get the destination namespace")
	assert.Equal(t, "app:/ConfigMap:dest/cm", out[1].GetAnnotations()[v1alpha1.AnnotationKeyTrackingID], "tracking is re-applied")
	assert.Equal(t, "other", out[2].GetNamespace())
	assert.Empty(t, out[2].GetAnnotations(), "unchanged objects are not re-tracked")
	assert.Equal(t, "dest", out[3].GetNamespace(), "unknown scope counts as namespaced")
	assert.Equal(t, "job-", out[4].GetGenerateName())
	data, _, _ := unstructured.NestedString(out[5].Object, "data", "v")
	assert.Equal(t, "second", data, "the last duplicate wins")
	require.Len(t, conds, 1)
	assert.Equal(t, v1alpha1.ApplicationConditionRepeatedResourceWarning, conds[0].Type)
	assert.Equal(t, "Resource /ConfigMap/dest/dup appeared 2 times among application resources.", conds[0].Message)

	// Tracking failures abort normalization.
	bad := configMap("x", "", nil)
	_, _, err = normalizeTargets("dest", []*unstructured.Unstructured{bad}, staticScope(), func(*unstructured.Unstructured) error { return errors.New("boom") })
	require.ErrorContains(t, err, "failed to set app instance label on resource ConfigMap/x: boom")
}

func TestNormalizeTargetsGenerateNameKeys(t *testing.T) {
	t.Parallel()
	a := obj(t, `{"apiVersion":"batch/v1","kind":"Job","metadata":{"generateName":"job-","namespace":"ns"}}`)
	b := obj(t, `{"apiVersion":"batch/v1","kind":"Job","metadata":{"generateName":"job-","namespace":"ns"}}`)
	out, conds, err := normalizeTargets("ns", []*unstructured.Unstructured{a, b}, staticScope(), func(*unstructured.Unstructured) error { return nil })
	require.NoError(t, err)
	assert.Len(t, out, 2, "generateName objects are keyed by position")
	assert.Empty(t, conds)
}

func TestReconcile(t *testing.T) {
	t.Parallel()
	cm := configMap("cm", "ns", nil)
	cr := obj(t, `{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"ClusterRole","metadata":{"name":"cr"}}`)
	unk := obj(t, `{"apiVersion":"example.com/v1","kind":"Unknown","metadata":{"name":"u","namespace":"ns"}}`)
	hook := configMap("hook", "ns", nil)
	hook.SetAnnotations(map[string]string{v1alpha1.AnnotationKeyHook: "PreSync"})
	ignored := configMap("ignored", "ns", nil)
	ignored.SetAnnotations(map[string]string{v1alpha1.AnnotationKeyHook: "NotAHookType"})

	liveCM := configMap("cm", "ns", nil)
	liveCR := obj(t, `{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"ClusterRole","metadata":{"name":"cr","uid":"1"}}`)
	liveCRDup := obj(t, `{"apiVersion":"authorization.openshift.io/v1","kind":"ClusterRole","metadata":{"name":"cr","uid":"1"}}`)
	liveUnk := obj(t, `{"apiVersion":"example.com/v1","kind":"Unknown","metadata":{"name":"u"}}`)
	extra := configMap("extra", "ns", nil)
	live := map[ResourceKey]*unstructured.Unstructured{
		{Kind: "ConfigMap", Namespace: "ns", Name: "cm"}:                       liveCM,
		{Group: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "cr"}:  liveCR,
		{Group: "authorization.openshift.io", Kind: "ClusterRole", Name: "cr"}: liveCRDup,
		{Group: "example.com", Kind: "Unknown", Name: "u"}:                     liveUnk,
		{Kind: "ConfigMap", Namespace: "ns", Name: "extra"}:                    extra,
	}
	rec := reconcile([]*unstructured.Unstructured{cm, cr, unk, hook, ignored, nil}, live, "ns", staticScope("ClusterRole"))
	require.Len(t, rec.Hooks, 1)
	assert.Equal(t, "hook", rec.Hooks[0].GetName())
	require.Len(t, rec.Target, 4)
	assert.Same(t, liveCM, rec.Live[0])
	assert.Same(t, liveCR, rec.Live[1], "the duplicate in another group is dropped, the target's group is kept")
	assert.Same(t, liveUnk, rec.Live[2], "unknown scope checks the cluster-scoped key too")
	assert.Nil(t, rec.Target[3])
	assert.Same(t, extra, rec.Live[3])
}

func TestMergeConditions(t *testing.T) {
	t.Parallel()
	old := metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	now := metav1.NewTime(time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC))
	existing := []metav1.Condition{
		{Type: v1alpha1.ApplicationConditionSharedResourceWarning, Status: metav1.ConditionTrue, Reason: "SharedResource", Message: "old", LastTransitionTime: old},
		{Type: v1alpha1.ApplicationConditionExcludedResourceWarning, Status: metav1.ConditionTrue, Reason: "ExcludedResource", Message: "gone", LastTransitionTime: old},
		{Type: v1alpha1.ApplicationConditionSyncError, Status: metav1.ConditionTrue, Reason: "AutoSyncFailed", Message: "kept", LastTransitionTime: old},
	}
	found := []Condition{
		{Type: v1alpha1.ApplicationConditionSharedResourceWarning, Message: "a"},
		{Type: v1alpha1.ApplicationConditionSharedResourceWarning, Message: "b"},
		{Type: v1alpha1.ApplicationConditionSharedResourceWarning, Message: "a"},
		{Type: v1alpha1.ApplicationConditionComparisonError, Reason: v1alpha1.ReasonManifestGenerationFail, Message: "boom"},
	}
	out := MergeConditions(existing, found, comparisonConditionTypes, 7, now)
	require.Len(t, out, 3)
	assert.Equal(t, "a; b", out[0].Message)
	assert.Equal(t, old, out[0].LastTransitionTime, "an active condition keeps its transition time")
	assert.Equal(t, v1alpha1.ReasonSharedResource, out[0].Reason)
	assert.Equal(t, int64(7), out[0].ObservedGeneration)
	assert.Equal(t, v1alpha1.ApplicationConditionSyncError, out[1].Type, "non-evaluated types are kept")
	assert.Equal(t, v1alpha1.ApplicationConditionComparisonError, out[2].Type)
	assert.Equal(t, v1alpha1.ReasonManifestGenerationFail, out[2].Reason)
	assert.Equal(t, now, out[2].LastTransitionTime)

	var many []Condition
	for i := range 30 {
		many = append(many, Condition{Type: v1alpha1.ApplicationConditionRepeatedResourceWarning, Message: fmt.Sprintf("m%d", i)})
	}
	out = MergeConditions(nil, many, nil, 1, now)
	assert.Contains(t, out[0].Message, "; and 10 more")
}

func TestMergeConditionsMessageBound(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(1, 30).Draw(t, "n")
		var found []Condition
		for i := range n {
			found = append(found, Condition{Type: "X", Message: rapid.StringN(0, 4000, -1).Draw(t, fmt.Sprintf("m%d", i))})
		}
		out := MergeConditions(nil, found, nil, 1, metav1.Now())
		if len(out) != 1 {
			t.Fatalf("want one condition, got %d", len(out))
		}
		if len(out[0].Message) > maxConditionMessage {
			t.Fatalf("message of %d bytes exceeds the limit", len(out[0].Message))
		}
		if out[0].Reason == "" {
			t.Fatal("empty reason")
		}
	})
}

func TestBoundResources(t *testing.T) {
	t.Parallel()
	var in []v1alpha1.ResourceStatus
	for i := range 10 {
		in = append(in, v1alpha1.ResourceStatus{Kind: "ConfigMap", Namespace: "ns", Name: fmt.Sprintf("cm-%02d", 9-i), Status: v1alpha1.SyncStatusCodeSynced})
	}
	in[0].Status = v1alpha1.SyncStatusCodeOutOfSync // cm-09
	out, dropped := boundResources(in, -1)
	assert.Zero(t, dropped)
	assert.Equal(t, "cm-00", out[0].Name)
	out, dropped = boundResources(in, 3)
	assert.Equal(t, 7, dropped)
	require.Len(t, out, 3)
	assert.Equal(t, []string{"cm-00", "cm-01", "cm-09"}, []string{out[0].Name, out[1].Name, out[2].Name}, "drifted resources survive truncation, order is kept")
}

func TestApplicationHealth(t *testing.T) {
	t.Parallel()
	app := &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "patrol", UID: "uid-app"}}
	degradedPod := obj(t, `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"p","namespace":"ns"},"status":{"phase":"Failed"}}`)
	hookPod := degradedPod.DeepCopy()
	hookPod.SetAnnotations(map[string]string{v1alpha1.AnnotationKeyHook: "PreSync"})
	ignoredPod := degradedPod.DeepCopy()
	ignoredPod.SetAnnotations(map[string]string{v1alpha1.AnnotationKeyIgnoreHealthCheck: "true"})
	self := obj(t, `{"apiVersion":"cd.patrol.example/v1alpha1","kind":"Application","metadata":{"name":"app","namespace":"patrol","uid":"uid-app"},"status":{"health":{"status":"Degraded"}}}`)
	cm := configMap("cm", "ns", nil)
	skipTarget := configMap("skip", "ns", nil)
	skipTarget.SetAnnotations(map[string]string{v1alpha1.AnnotationKeyHook: "Skip"})

	mr := func(target, live *unstructured.Unstructured) ManagedResource {
		o := live
		if o == nil {
			o = target
		}
		return ManagedResource{Key: ResourceKey{Group: o.GroupVersionKind().Group, Kind: o.GetKind(), Namespace: o.GetNamespace(), Name: o.GetName()}, Target: target, Live: live}
	}
	tests := []struct {
		name    string
		managed []ManagedResource
		want    v1alpha1.HealthStatusCode
	}{
		{"no resources", nil, v1alpha1.HealthStatusHealthy},
		{"hooks only", []ManagedResource{mr(nil, hookPod)}, v1alpha1.HealthStatusHealthy},
		{"hook excluded", []ManagedResource{mr(cm, cm), mr(nil, hookPod)}, v1alpha1.HealthStatusHealthy},
		{"skip target excluded", []ManagedResource{mr(skipTarget, nil), mr(cm, cm)}, v1alpha1.HealthStatusHealthy},
		{"ignore healthcheck", []ManagedResource{mr(cm, cm), mr(ignoredPod, ignoredPod)}, v1alpha1.HealthStatusHealthy},
		{"self reference", []ManagedResource{mr(cm, cm), mr(nil, self)}, v1alpha1.HealthStatusHealthy},
		{"degraded", []ManagedResource{mr(cm, cm), mr(degradedPod, degradedPod)}, v1alpha1.HealthStatusDegraded},
		{"one missing", []ManagedResource{mr(cm, nil), mr(cm, cm)}, v1alpha1.HealthStatusHealthy},
		{"all missing", []ManagedResource{mr(cm, nil)}, v1alpha1.HealthStatusMissing},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			statuses := make([]v1alpha1.ResourceStatus, len(tt.managed))
			got, err := applicationHealth(app, tt.managed, statuses, nil, true)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}

	// Persisted vs tree health.
	managed := []ManagedResource{mr(cm, nil), mr(degradedPod, degradedPod), mr(ignoredPod, ignoredPod)}
	statuses := make([]v1alpha1.ResourceStatus, len(managed))
	_, err := applicationHealth(app, managed, statuses, nil, false)
	require.NoError(t, err)
	assert.Nil(t, statuses[1].Health, "health is not persisted in appTree mode")
	require.NotNil(t, managed[0].Health)
	assert.Equal(t, v1alpha1.HealthStatusMissing, managed[0].Health.Status)
	assert.Equal(t, v1alpha1.HealthStatusDegraded, managed[1].Health.Status)
	assert.Nil(t, managed[2].Health, "ignored resources have no recorded health")

	// Assessment errors are reported, the status is still aggregated.
	badDeploy := obj(t, `{"apiVersion":"apps/v1beta2","kind":"Deployment","metadata":{"name":"d","namespace":"ns"}}`)
	got, err := applicationHealth(app, []ManagedResource{mr(badDeploy, badDeploy)}, make([]v1alpha1.ResourceStatus, 1), nil, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `failed to get resource health for "Deployment" with name "d" in namespace "ns"`)
	assert.Equal(t, v1alpha1.HealthStatusUnknown, got)
}

func TestRequiresDeletionConfirmation(t *testing.T) {
	t.Parallel()
	app := &v1alpha1.Application{}
	cm := configMap("cm", "ns", nil)
	assert.False(t, requiresDeletionConfirmation(cm, app))
	assert.False(t, requiresDeletionConfirmation(nil, app))
	cm.SetAnnotations(map[string]string{v1alpha1.AnnotationKeySyncOptions: "Prune=confirm"})
	assert.True(t, requiresDeletionConfirmation(cm, app))
	cm.SetAnnotations(map[string]string{v1alpha1.AnnotationKeySyncOptions: "Delete=false"})
	app.Spec.SyncPolicy = &v1alpha1.SyncPolicy{SyncOptions: v1alpha1.SyncOptions{"Delete=confirm"}}
	assert.False(t, requiresDeletionConfirmation(cm, app), "the resource annotation wins over the app option")
	cm.SetAnnotations(nil)
	assert.True(t, requiresDeletionConfirmation(cm, app))
}

func TestManagedNamespaceHelpers(t *testing.T) {
	t.Parallel()
	app := &v1alpha1.Application{Spec: v1alpha1.ApplicationSpec{
		Destination: v1alpha1.ApplicationDestination{Namespace: "dest"},
		SyncPolicy:  &v1alpha1.SyncPolicy{ManagedNamespaceMetadata: &v1alpha1.ManagedNamespaceMetadata{Labels: map[string]string{"team": "a"}}},
	}}
	ns, err := managedNamespaceTarget("dest", app.Spec.SyncPolicy.ManagedNamespaceMetadata)
	require.NoError(t, err)
	assert.True(t, isManagedNamespace(ns, app))
	assert.Equal(t, map[string]string{"team": "a"}, ns.GetLabels())
	assert.Equal(t, "ServerSideApply=true", ns.GetAnnotations()[v1alpha1.AnnotationKeySyncOptions])
	assert.False(t, changedManagedNamespaceMetadata(app, nil))
	assert.True(t, changedManagedNamespaceMetadata(app, &v1alpha1.SyncOperationResult{}))
	assert.False(t, changedManagedNamespaceMetadata(app, &v1alpha1.SyncOperationResult{ManagedNamespaceMetadata: &v1alpha1.ManagedNamespaceMetadata{Labels: map[string]string{"team": "a"}}}))
}

func TestShortRevision(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "0123456", shortRevision(v1alpha1.SyncStatus{Revision: "0123456789abcdef"}))
	assert.Equal(t, "1.2.3", shortRevision(v1alpha1.SyncStatus{Revision: "1.2.3"}))
	assert.Equal(t, "abcdef0 +1", shortRevision(v1alpha1.SyncStatus{Revisions: []string{"abcdef0123456789abcdef0123456789abcdef01", "2.0.0"}}))
	assert.Empty(t, shortRevision(v1alpha1.SyncStatus{}))
}

func TestUnmarshalManifests(t *testing.T) {
	t.Parallel()
	objs, err := unmarshalManifests([]string{`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"a"}}`, "null", "", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: b\n"})
	require.NoError(t, err)
	require.Len(t, objs, 4)
	assert.Equal(t, "a", objs[0].GetName())
	assert.Nil(t, objs[1])
	assert.Nil(t, objs[2])
	assert.Equal(t, "b", objs[3].GetName())
	_, err = unmarshalManifests([]string{"{not json"})
	require.Error(t, err)
}

func TestSourceRefreshPaths(t *testing.T) {
	t.Parallel()
	app := &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
		v1alpha1.AnnotationKeyManifestGeneratePaths: " .; /shared ;; ../common",
	}}}
	assert.Equal(t, []string{"apps/web", "shared", "apps/common"}, sourceRefreshPaths(app, v1alpha1.ApplicationSource{Path: "apps/web"}))
	hyd := &v1alpha1.Application{Spec: v1alpha1.ApplicationSpec{SourceHydrator: &v1alpha1.SourceHydrator{
		DrySource:  v1alpha1.DrySource{RepoURL: "https://git/repo"},
		SyncSource: v1alpha1.SyncSource{TargetBranch: "env", Path: "out"},
	}}}
	assert.Equal(t, []string{"out"}, sourceRefreshPaths(hyd, hyd.Spec.SourceHydrator.GetSyncSource()))
}

func TestRepoErrorGracePeriod(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m := &Manager{d: Deps{RepoErrorGracePeriod: time.Minute, Now: func() time.Time { return now }}, repoErrs: map[string]time.Time{}}
	assert.True(t, m.repoErrorDeferred("a", false), "the first error is deferred")
	now = now.Add(30 * time.Second)
	assert.True(t, m.repoErrorDeferred("a", false), "still within the grace period")
	now = now.Add(time.Minute)
	assert.False(t, m.repoErrorDeferred("a", false), "reported after the grace period")
	assert.False(t, m.repoErrorDeferred("b", true), "hard refreshes report at once")
	m.clearRepoError("a")
	assert.True(t, m.repoErrorDeferred("a", false), "a success resets the grace period")

	off := &Manager{d: Deps{Now: time.Now}, repoErrs: map[string]time.Time{}}
	assert.False(t, off.repoErrorDeferred("a", false))
}

func TestTreeSummary(t *testing.T) {
	t.Parallel()
	app := &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
		v1alpha1.AnnotationKeyLinkPrefix + "home": "Home|https://home.example.com",
		"unrelated": "https://ignored.example.com",
	}}}
	tree := &ApplicationTree{
		Nodes: []ResourceNode{
			{ResourceRef: ResourceRef{Group: v1alpha1.GroupName, Kind: "Application", Name: "child"}},
			{ResourceRef: ResourceRef{Kind: "Pod", Name: "a"}, Images: []string{"b:1", "a:1"}},
			{ResourceRef: ResourceRef{Kind: "Pod", Name: "b"}, Images: []string{"a:1"}},
			{ResourceRef: ResourceRef{Group: "networking.k8s.io", Kind: "Ingress", Name: "in"}, NetworkingInfo: &NetworkingInfo{ExternalURLs: []string{"https://z.example.com"}}},
		},
		OrphanedNodes: []ResourceNode{{ResourceRef: ResourceRef{Kind: "Pod", Name: "o"}, Images: []string{"orphan:1"}}},
	}
	s := treeSummary(tree, app)
	assert.True(t, s.IsAppOfApps)
	assert.Equal(t, []string{"a:1", "b:1"}, s.Images, "orphans are not summarized")
	assert.Equal(t, []string{"Home|https://home.example.com", "https://z.example.com"}, s.ExternalURLs)
	assert.False(t, treeSummary(&ApplicationTree{}, &v1alpha1.Application{}).IsAppOfApps)
}
