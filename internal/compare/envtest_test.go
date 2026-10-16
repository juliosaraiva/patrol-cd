package compare

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

const repoURL = "https://git.example.com/apps.git"

func src(path string) v1alpha1.ApplicationSource {
	return v1alpha1.ApplicationSource{RepoURL: repoURL, Path: path, TargetRevision: "main"}
}

func TestEnvCompareLifecycle(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.repo.set(repoURL, "app", "rev-1",
		configMap("cm-a", "", map[string]string{"k": "v1"}),
		configMap("cm-b", "", map[string]string{"k": "v1"}),
	)
	app := h.app("lifecycle", src("app"))
	in := Input{App: app}

	// Missing: nothing exists yet.
	res := h.compare(in)
	assert.Equal(t, v1alpha1.SyncStatusCodeOutOfSync, res.SyncStatus.Status)
	assert.Equal(t, v1alpha1.HealthStatusMissing, res.Health, "all resources missing")
	assert.Equal(t, "rev-1", res.SyncStatus.Revision)
	assert.Equal(t, "rev-1", res.SyncStatus.ShortRevision)
	assert.Equal(t, []string{"rev-1"}, res.Revisions)
	require.NotNil(t, res.SyncStatus.ComparedTo.Source)
	assert.Equal(t, "app", res.SyncStatus.ComparedTo.Source.Path)
	assert.Equal(t, v1alpha1.ApplicationSourceTypeDirectory, res.SourceType)
	require.Len(t, res.Resources, 2)
	for _, r := range res.Resources {
		assert.Equal(t, v1alpha1.SyncStatusCodeOutOfSync, r.Status)
		assert.Equal(t, h.ns, r.Namespace)
		require.NotNil(t, r.Health)
		assert.Equal(t, v1alpha1.HealthStatusMissing, r.Health.Status)
	}
	require.NotNil(t, res.Tree)
	require.Len(t, res.Tree.Nodes, 2)
	assert.Equal(t, v1alpha1.HealthStatusMissing, res.Tree.Nodes[0].Health.Status)
	assert.Empty(t, res.Tree.Nodes[0].UID)
	for _, p := range []string{PhaseSettings, PhaseManifests, PhaseDedup, PhaseLive, PhaseDiff, PhaseSync, PhaseHealth, PhaseTree} {
		found := false
		for _, tm := range res.Timings {
			found = found || tm.Phase == p
		}
		assert.True(t, found, "timing %s", p)
	}
	reqs := h.repo.lastRequests()
	require.NotEmpty(t, reqs)
	assert.Equal(t, app.Name, reqs[0].AppName)
	assert.Equal(t, h.ns, reqs[0].Namespace)
	assert.NotEmpty(t, reqs[0].KubeVersion, "the destination version is sent to the repo-server")
	assert.Contains(t, reqs[0].APIVersions, "v1/ConfigMap")

	// Synced once the resources exist as desired.
	h.createTargets(res)
	res = h.eventually(in, func(r *Result) bool { return r.SyncStatus.Status == v1alpha1.SyncStatusCodeSynced }, "synced after creation")
	assert.Equal(t, v1alpha1.HealthStatusHealthy, res.Health)
	for _, r := range res.Resources {
		assert.Equal(t, v1alpha1.SyncStatusCodeSynced, r.Status, r.Name)
	}
	assert.Empty(t, res.Conditions)
	require.Len(t, res.Tree.Nodes, 2)
	assert.NotEmpty(t, res.Tree.Nodes[0].UID)
	assert.NotNil(t, res.Tree.Nodes[0].CreatedAt)

	// Out of sync when live drifts.
	h.mutate(configMap("cm-a", h.ns, nil), func(u *unstructured.Unstructured) {
		require.NoError(t, unstructured.SetNestedField(u.Object, "drifted", "data", "k"))
	})
	res = h.eventually(in, func(r *Result) bool { return r.SyncStatus.Status == v1alpha1.SyncStatusCodeOutOfSync }, "out of sync after drift")
	assert.Equal(t, v1alpha1.SyncStatusCodeOutOfSync, findResource(res, "ConfigMap", "cm-a").Status)
	assert.Equal(t, v1alpha1.SyncStatusCodeSynced, findResource(res, "ConfigMap", "cm-b").Status)
	assert.True(t, res.DiffResults[ResourceKey{Kind: "ConfigMap", Namespace: h.ns, Name: "cm-a"}])
	assert.False(t, res.DiffResults[ResourceKey{Kind: "ConfigMap", Namespace: h.ns, Name: "cm-b"}])
	mr := findManaged(res, "ConfigMap", "cm-a")
	require.NotNil(t, mr)
	assert.True(t, mr.Diff.Modified)
	assert.Contains(t, string(mr.Diff.NormalizedLive), "drifted")
	assert.Contains(t, string(mr.Diff.PredictedLive), "v1")

	// Extraneous: cm-b leaves the desired state and must be pruned.
	h.repo.set(repoURL, "app", "rev-2", configMap("cm-a", "", map[string]string{"k": "v1"}))
	h.mutate(configMap("cm-a", h.ns, nil), func(u *unstructured.Unstructured) {
		require.NoError(t, unstructured.SetNestedField(u.Object, "v1", "data", "k"))
	})
	res = h.eventually(in, func(r *Result) bool {
		a := findResource(r, "ConfigMap", "cm-a")
		return a != nil && a.Status == v1alpha1.SyncStatusCodeSynced
	}, "cm-a synced again")
	b := findResource(res, "ConfigMap", "cm-b")
	require.NotNil(t, b)
	assert.Equal(t, v1alpha1.SyncStatusCodeOutOfSync, b.Status)
	assert.True(t, b.RequiresPruning)
	assert.Equal(t, v1alpha1.SyncStatusCodeOutOfSync, res.SyncStatus.Status)
	assert.Equal(t, "rev-2", res.SyncStatus.Revision)

	// IgnoreExtraneous: the extraneous resource no longer makes the application OutOfSync.
	h.mutate(configMap("cm-b", h.ns, nil), func(u *unstructured.Unstructured) {
		a := u.GetAnnotations()
		a[v1alpha1.AnnotationKeyCompareOptions] = v1alpha1.CompareOptionIgnoreExtraneous
		u.SetAnnotations(a)
	})
	res = h.eventually(in, func(r *Result) bool { return r.SyncStatus.Status == v1alpha1.SyncStatusCodeSynced }, "IgnoreExtraneous")
	b = findResource(res, "ConfigMap", "cm-b")
	require.NotNil(t, b)
	assert.Equal(t, v1alpha1.SyncStatusCodeOutOfSync, b.Status, "the resource itself stays OutOfSync")
	assert.True(t, b.RequiresPruning)
}

func TestEnvIgnoreExtraneousDoesNotHideModifications(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	desired := configMap("cm", "", map[string]string{"k": "git"})
	desired.SetAnnotations(map[string]string{v1alpha1.AnnotationKeyCompareOptions: v1alpha1.CompareOptionIgnoreExtraneous})
	h.repo.set(repoURL, "app", "rev-1", desired)
	app := h.app("modified-extraneous", src("app"))
	in := Input{App: app}
	res := h.compare(in)
	live := res.Targets[0].DeepCopy()
	require.NoError(t, unstructured.SetNestedField(live.Object, "cluster", "data", "k"))
	h.create(live)
	res = h.eventually(in, func(r *Result) bool {
		c := findResource(r, "ConfigMap", "cm")
		return c != nil && c.Health == nil && r.SyncStatus.Status == v1alpha1.SyncStatusCodeOutOfSync && findManaged(r, "ConfigMap", "cm").Live != nil
	}, "modified resource stays OutOfSync")
	assert.Equal(t, v1alpha1.SyncStatusCodeOutOfSync, findResource(res, "ConfigMap", "cm").Status)
}

func TestEnvMultiSourceLastWins(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.repo.set(repoURL, "first", "sha-first",
		configMap("dup", "", map[string]string{"from": "first"}),
		configMap("only-first", "", nil),
	)
	h.repo.set(repoURL, "second", "sha-second",
		configMap("dup", "", map[string]string{"from": "second"}),
	)
	h.repo.set("https://git.example.com/values.git", "", "sha-values")
	values := v1alpha1.ApplicationSource{RepoURL: "https://git.example.com/values.git", TargetRevision: "main", Ref: "values"}
	app := h.app("multi", src("first"), src("second"), values)

	res := h.compare(Input{App: app})
	assert.Contains(t, conditionMessages(res, v1alpha1.ApplicationConditionRepeatedResourceWarning),
		fmt.Sprintf("Resource /ConfigMap/%s/dup appeared 2 times among application resources.", h.ns))
	dup := findManaged(res, "ConfigMap", "dup")
	require.NotNil(t, dup)
	from, _, _ := unstructured.NestedString(dup.Target.Object, "data", "from")
	assert.Equal(t, "second", from, "the last source wins")
	assert.NotNil(t, findManaged(res, "ConfigMap", "only-first"))
	assert.Equal(t, []string{"sha-first", "sha-second", "sha-values"}, res.SyncStatus.Revisions)
	assert.Empty(t, res.SyncStatus.Revision)
	assert.Len(t, res.SyncStatus.ComparedTo.Sources, 3)
	assert.Nil(t, res.SyncStatus.ComparedTo.Source)
	assert.Len(t, res.SourceTypes, 3)
	assert.Equal(t, "sha-first +2", res.SyncStatus.ShortRevision)

	reqs := h.repo.lastRequests()
	require.Len(t, reqs, 3)
	for _, r := range reqs {
		assert.True(t, r.HasMultipleSources)
		require.Contains(t, r.RefSources, "$values")
		assert.Equal(t, "main", r.RefSources["$values"].TargetRevision)
	}

	// Revision overrides (sync/rollback) reach the repo-server and the ref sources.
	res = h.compare(Input{App: app, Revisions: []string{"", "v2", "v3"}, RollbackOrSyncOverride: true})
	reqs = h.repo.lastRequests()[3:]
	require.Len(t, reqs, 3)
	assert.Equal(t, "main", reqs[0].Revision)
	assert.Equal(t, "v2", reqs[1].Revision)
	assert.Equal(t, "v3", reqs[0].RefSources["$values"].TargetRevision)
	assert.Equal(t, v1alpha1.SyncStatusCodeOutOfSync, res.SyncStatus.Status)
}

func TestEnvIgnoreDifferences(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.repo.set(repoURL, "app", "rev-1", configMap("cm", "", map[string]string{"k": "git", "other": "x"}))
	app := h.app("ignore", src("app"))
	in := Input{App: app}
	res := h.compare(in)
	live := res.Targets[0].DeepCopy()
	require.NoError(t, unstructured.SetNestedField(live.Object, "cluster", "data", "k"))
	h.create(live)
	h.eventually(in, func(r *Result) bool {
		return r.SyncStatus.Status == v1alpha1.SyncStatusCodeOutOfSync && findManaged(r, "ConfigMap", "cm").Live != nil
	}, "drift without ignore rule")

	app.Spec.IgnoreDifferences = v1alpha1.IgnoreDifferences{{Kind: "ConfigMap", JSONPointers: []string{"/data/k"}}}
	res = h.compare(Input{App: app})
	assert.Equal(t, v1alpha1.SyncStatusCodeSynced, res.SyncStatus.Status)
	assert.Equal(t, app.Spec.IgnoreDifferences, res.SyncStatus.ComparedTo.IgnoreDifferences)
	require.NotNil(t, res.Normalizer, "the normalizer is exposed for RespectIgnoreDifferences")

	// jq rules and name scoping.
	app.Spec.IgnoreDifferences = v1alpha1.IgnoreDifferences{{Kind: "ConfigMap", Name: "other", JQPathExpressions: []string{".data.k"}}}
	res = h.compare(Input{App: app})
	assert.Equal(t, v1alpha1.SyncStatusCodeOutOfSync, res.SyncStatus.Status, "a rule for another name does not apply")
	app.Spec.IgnoreDifferences = v1alpha1.IgnoreDifferences{{Kind: "ConfigMap", Name: "cm", JQPathExpressions: []string{".data.k"}}}
	res = h.compare(Input{App: app})
	assert.Equal(t, v1alpha1.SyncStatusCodeSynced, res.SyncStatus.Status)

	// An invalid jq rule fails the comparison.
	app.Spec.IgnoreDifferences = v1alpha1.IgnoreDifferences{{Kind: "ConfigMap", JQPathExpressions: []string{".data[["}}}
	res = h.compare(Input{App: app})
	assert.Equal(t, v1alpha1.SyncStatusCodeUnknown, res.SyncStatus.Status)
	assert.Contains(t, conditionMessages(res, v1alpha1.ApplicationConditionComparisonError), "Failed to compare desired state to live state")
}

func TestEnvSharedResourceWarning(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.repo.set(repoURL, "a", "rev-a", configMap("shared", "", map[string]string{"k": "a"}))
	h.repo.set(repoURL, "b", "rev-b", configMap("shared", "", map[string]string{"k": "a"}))
	appA := h.app("app-a", src("a"))
	appB := h.app("app-b", src("b"))
	res := h.compare(Input{App: appA})
	h.createTargets(res)
	h.eventually(Input{App: appA}, func(r *Result) bool { return r.SyncStatus.Status == v1alpha1.SyncStatusCodeSynced }, "app-a synced")

	res = h.eventually(Input{App: appB}, func(r *Result) bool {
		return r.HasCondition(v1alpha1.ApplicationConditionSharedResourceWarning)
	}, "shared resource warning for app-b")
	assert.Equal(t, fmt.Sprintf("ConfigMap/shared is part of applications patrol/%s and %s", appB.Name, appA.Name),
		conditionMessages(res, v1alpha1.ApplicationConditionSharedResourceWarning))
	resA := h.compare(Input{App: appA})
	assert.False(t, resA.HasCondition(v1alpha1.ApplicationConditionSharedResourceWarning))
	assert.Contains(t, res.EvaluatedConditionTypes, v1alpha1.ApplicationConditionSharedResourceWarning)
}

func TestEnvCopiedTrackingIsNotManaged(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.repo.set(repoURL, "app", "rev-1", configMap("original", "", nil))
	app := h.app("copied", src("app"))
	in := Input{App: app}
	res := h.compare(in)
	h.createTargets(res)
	// A ConfigMap carrying the tracking id copied from "original".
	copied := res.Targets[0].DeepCopy()
	copied.SetName("copy")
	h.create(copied)
	res = h.eventually(in, func(r *Result) bool {
		return findManaged(r, "ConfigMap", "copy") != nil && r.SyncStatus.Status == v1alpha1.SyncStatusCodeSynced
	}, "copy observed")
	cp := findResource(res, "ConfigMap", "copy")
	require.NotNil(t, cp)
	assert.Empty(t, cp.Status, "copied tracking ids get no status")
	assert.False(t, cp.RequiresPruning, "copies are never pruned")
}

func TestEnvOrphanedResources(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	exists := map[string]bool{}
	h := newHarness(t, withDeps(func(d *Deps) {
		d.ApplicationExists = func(name string) bool {
			mu.Lock()
			defer mu.Unlock()
			return exists[name]
		}
	}))
	h.repo.set(repoURL, "app", "rev-1", configMap("managed", "", nil))
	app := h.app("orphans", src("app"))
	proj := permissiveProject()
	proj.Spec.OrphanedResources = &v1alpha1.OrphanedResourcesMonitorSettings{
		Warn:   ptrTo(true),
		Ignore: []v1alpha1.OrphanedResourceKey{{Kind: "Secret"}},
	}
	in := Input{App: app, Project: proj}
	res := h.compare(in)
	h.createTargets(res)
	h.create(configMap("orphan", h.ns, nil))
	h.create(&unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "ignored", "namespace": h.ns}}})
	otherApp := h.app("other-app", src("other"))
	mu.Lock()
	exists[otherApp.Name] = true
	mu.Unlock()
	h.create(h.track(configMap("belongs-to-other", h.ns, nil), otherApp))
	h.waitCached("", "ConfigMap", h.ns, "belongs-to-other")
	h.waitCached("", "Secret", h.ns, "ignored")

	res = h.eventually(in, func(r *Result) bool {
		return r.Tree != nil && len(r.Tree.OrphanedNodes) == 1 && r.SyncStatus.Status == v1alpha1.SyncStatusCodeSynced
	}, "one orphan")
	assert.Equal(t, "orphan", res.Tree.OrphanedNodes[0].Name)
	assert.Equal(t, 1, res.OrphanedResources)
	assert.Equal(t, "Application has 1 orphaned resources", conditionMessages(res, v1alpha1.ApplicationConditionOrphanedResourceWarning))
	assert.Contains(t, res.EvaluatedConditionTypes, v1alpha1.ApplicationConditionOrphanedResourceWarning)

	// Resources of unknown Applications are orphans too.
	mu.Lock()
	delete(exists, otherApp.Name)
	mu.Unlock()
	h.eventually(in, func(r *Result) bool { return r.OrphanedResources == 2 }, "orphans of an unknown application")

	// warn=false keeps the orphan list without the condition.
	proj.Spec.OrphanedResources.Warn = nil
	res = h.compare(in)
	assert.Equal(t, 2, res.OrphanedResources)
	assert.False(t, res.HasCondition(v1alpha1.ApplicationConditionOrphanedResourceWarning))

	// Monitoring off: no orphans.
	proj.Spec.OrphanedResources = nil
	res = h.compare(in)
	assert.Zero(t, res.OrphanedResources)
	assert.Empty(t, res.Tree.OrphanedNodes)
}

func TestEnvExcludedResources(t *testing.T) {
	t.Parallel()
	h := newHarness(t, withSettings(map[string]string{
		"resource.exclusions": "- apiGroups: [\"\"]\n  kinds: [Secret]\n  clusters: [\"*\"]\n",
	}))
	secret := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "creds"}}}
	h.repo.set(repoURL, "app", "rev-1", configMap("cm", "", nil), secret)
	app := h.app("excluded", src("app"))
	in := Input{App: app}
	res := h.compare(in)
	assert.Equal(t, "Resource /Secret creds is excluded in the settings",
		conditionMessages(res, v1alpha1.ApplicationConditionExcludedResourceWarning))
	assert.Nil(t, findManaged(res, "Secret", "creds"))
	require.NotNil(t, findManaged(res, "ConfigMap", "cm"))
	h.createTargets(res)
	res = h.eventually(in, func(r *Result) bool { return r.SyncStatus.Status == v1alpha1.SyncStatusCodeSynced }, "synced with the excluded secret ignored")
	assert.True(t, res.HasCondition(v1alpha1.ApplicationConditionExcludedResourceWarning))
}

func TestEnvNamespaceDefaulting(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	role := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole",
		"metadata": map[string]any{"name": h.ns + "-reader", "namespace": "should-be-stripped"},
	}}
	h.repo.set(repoURL, "app", "rev-1", configMap("cm", "", nil), configMap("elsewhere", "default", nil), role)
	app := h.app("namespaces", src("app"))
	res := h.compare(Input{App: app})
	cm := findManaged(res, "ConfigMap", "cm")
	require.NotNil(t, cm)
	assert.Equal(t, h.ns, cm.Key.Namespace)
	assert.Equal(t, h.ns, cm.Target.GetNamespace())
	assert.Equal(t, "default", findManaged(res, "ConfigMap", "elsewhere").Key.Namespace)
	cr := findManaged(res, "ClusterRole", h.ns+"-reader")
	require.NotNil(t, cr)
	assert.Empty(t, cr.Key.Namespace)
	assert.Empty(t, cr.Target.GetNamespace())

	// Namespaced resources without a destination namespace are an invalid spec.
	app2 := h.app("no-namespace", src("app"))
	app2.Spec.Destination.Namespace = ""
	res = h.compare(Input{App: app2})
	assert.Contains(t, conditionMessages(res, v1alpha1.ApplicationConditionInvalidSpecError), "Namespace for cm /v1, Kind=ConfigMap is missing.")
}

func TestEnvHooksExcludedFromHealth(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	job := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]any{"name": "migrate", "annotations": map[string]any{v1alpha1.AnnotationKeyHook: "PreSync"}},
		"spec":     map[string]any{"template": map[string]any{"spec": map[string]any{"restartPolicy": "Never", "containers": []any{map[string]any{"name": "m", "image": "migrate:1"}}}}},
	}}
	h.repo.set(repoURL, "app", "rev-1", configMap("cm", "", nil), job)
	app := h.app("hooks", src("app"))
	in := Input{App: app}
	res := h.compare(in)
	assert.Nil(t, findManaged(res, "Job", "migrate"), "hook targets are not compared")
	found := false
	for _, tgt := range res.Targets {
		found = found || tgt.GetName() == "migrate"
	}
	assert.True(t, found, "hooks are passed to the sync engine")
	h.createTargets(res)

	// A failed hook Pod left by an earlier sync.
	pod := h.track(failedPod("hook-pod", h.ns, map[string]string{v1alpha1.AnnotationKeyHook: "PostSync", v1alpha1.AnnotationKeySyncWave: "5"}), app)
	h.failPod(h.create(pod))
	res = h.eventually(in, func(r *Result) bool {
		mr := findManaged(r, "Pod", "hook-pod")
		return mr != nil && mr.Live != nil && r.SyncStatus.Status == v1alpha1.SyncStatusCodeSynced
	}, "hook pod observed")
	assert.Equal(t, v1alpha1.HealthStatusHealthy, res.Health, "hooks do not affect health")
	hook := findResource(res, "Pod", "hook-pod")
	require.NotNil(t, hook)
	assert.True(t, hook.Hook)
	assert.Empty(t, hook.Status)
	assert.Nil(t, hook.Health)

	// The same failure on a regular resource degrades the application.
	regular := h.track(failedPod("app-pod", h.ns, nil), app)
	h.failPod(h.create(regular))
	h.eventually(in, func(r *Result) bool { return r.Health == v1alpha1.HealthStatusDegraded }, "degraded by a regular pod")

	// ignore-healthcheck removes it again.
	h.mutate(regular, func(u *unstructured.Unstructured) {
		a := u.GetAnnotations()
		a[v1alpha1.AnnotationKeyIgnoreHealthCheck] = "true"
		u.SetAnnotations(a)
	})
	h.eventually(in, func(r *Result) bool { return r.Health == v1alpha1.HealthStatusHealthy }, "ignore-healthcheck")
}

func failedPod(name, namespace string, annotations map[string]string) *unstructured.Unstructured {
	pod := &corev1.Pod{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Annotations: annotations},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "example.invalid/img"}}},
	}
	u, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(pod)
	return &unstructured.Unstructured{Object: u}
}

func (h *harness) failPod(pod *unstructured.Unstructured) {
	h.t.Helper()
	cur, err := h.kube.CoreV1().Pods(pod.GetNamespace()).Get(h.ctx, pod.GetName(), metav1.GetOptions{})
	require.NoError(h.t, err)
	cur.Status.Phase = corev1.PodFailed
	cur.Status.Message = "boom"
	_, err = h.kube.CoreV1().Pods(pod.GetNamespace()).UpdateStatus(h.ctx, cur, metav1.UpdateOptions{})
	require.NoError(h.t, err)
}

func TestEnvRepoErrorComparisonError(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.repo.set(repoURL, "app", "rev-1", configMap("cm", "", nil))
	app := h.app("repo-error", src("app"))
	in := Input{App: app}
	res := h.compare(in)
	h.createTargets(res)
	h.eventually(in, func(r *Result) bool { return r.SyncStatus.Status == v1alpha1.SyncStatusCodeSynced }, "synced")

	h.repo.fail(repoURL, "app", errBoom)
	res = h.compare(in)
	assert.Equal(t, v1alpha1.SyncStatusCodeUnknown, res.SyncStatus.Status)
	assert.True(t, res.FailedToLoad)
	assert.Equal(t, "Failed to load target state: failed to generate manifest for source 1 of 1: "+errBoom.Error(),
		conditionMessages(res, v1alpha1.ApplicationConditionComparisonError))
	for _, c := range res.Conditions {
		if c.Type == v1alpha1.ApplicationConditionComparisonError {
			assert.Equal(t, v1alpha1.ReasonManifestGenerationFail, c.Reason)
		}
	}
	cm := findResource(res, "ConfigMap", "cm")
	require.NotNil(t, cm, "live resources are still listed")
	assert.Equal(t, v1alpha1.SyncStatusCodeUnknown, cm.Status)
	assert.Equal(t, "main", res.SyncStatus.Revision, "the requested revision is kept")

	// Recovery.
	h.repo.set(repoURL, "app", "rev-1", configMap("cm", "", nil))
	res = h.compare(in)
	assert.Equal(t, v1alpha1.SyncStatusCodeSynced, res.SyncStatus.Status)
	assert.False(t, res.HasCondition(v1alpha1.ApplicationConditionComparisonError))
}

func TestEnvRepoErrorGracePeriod(t *testing.T) {
	t.Parallel()
	h := newHarness(t, withDeps(func(d *Deps) { d.RepoErrorGracePeriod = time.Hour }))
	h.repo.fail(repoURL, "app", errBoom)
	app := h.app("grace", src("app"))
	_, err := h.mgr.Compare(h.ctx, Input{App: app, Project: permissiveProject()})
	require.ErrorIs(t, err, ErrRepoErrorDeferred)
	_, err = h.mgr.Compare(h.ctx, Input{App: app, Project: permissiveProject()})
	require.ErrorIs(t, err, ErrRepoErrorDeferred, "still within the grace period")
	res := h.compare(Input{App: app, NoRevisionCache: true})
	assert.Equal(t, v1alpha1.SyncStatusCodeUnknown, res.SyncStatus.Status, "hard refreshes report the error")
	assert.True(t, res.HasCondition(v1alpha1.ApplicationConditionComparisonError))
}

func TestEnvLocalManifests(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	app := h.app("local", src("app"))
	cm := configMap("local-cm", "", map[string]string{"k": "v"})
	res := h.compare(Input{App: app, LocalManifests: []string{toJSON(t, cm)}})
	require.NotNil(t, findManaged(res, "ConfigMap", "local-cm"))
	assert.Empty(t, h.repo.lastRequests(), "local manifests are not rendered")
	assert.Equal(t, v1alpha1.SyncStatusCodeOutOfSync, res.SyncStatus.Status)

	proj := permissiveProject()
	proj.Spec.SourceIntegrity = &v1alpha1.SourceIntegrity{Git: &v1alpha1.SourceIntegrityGit{Policies: []v1alpha1.SourceIntegrityGitPolicy{{
		Repos: []v1alpha1.SourceIntegrityGitPolicyRepo{{URL: "*"}},
		GPG:   &v1alpha1.SourceIntegrityGitPolicyGPG{Mode: v1alpha1.SourceIntegrityGitPolicyGPGModeHead, Keys: []string{"ABCDEF0123456789"}},
	}}}}
	h.mgr.d.GPGEnabled = true
	res = h.compare(Input{App: app, Project: proj, LocalManifests: []string{toJSON(t, cm)}})
	assert.Equal(t, v1alpha1.SyncStatusCodeUnknown, res.SyncStatus.Status)
	assert.Equal(t, "Cannot use local manifests when source integrity is enforced", conditionMessages(res, v1alpha1.ApplicationConditionComparisonError))
}

func TestEnvProjectRestrictions(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	role := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole",
		"metadata": map[string]any{"name": h.ns + "-denied"},
	}}
	h.repo.set(repoURL, "app", "rev-1", configMap("cm", "", nil), role)
	app := h.app("restricted", src("app"))
	proj := permissiveProject()
	proj.Spec.ClusterResourceWhitelist = nil
	res := h.compare(Input{App: app, Project: proj})
	cr := findResource(res, "ClusterRole", h.ns+"-denied")
	require.NotNil(t, cr)
	assert.Equal(t, v1alpha1.SyncStatusCodeUnknown, cr.Status, "kinds the project denies are Unknown")
	assert.Equal(t, v1alpha1.SyncStatusCodeOutOfSync, findResource(res, "ConfigMap", "cm").Status)
}

func TestEnvManagedNamespaceMetadata(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.repo.set(repoURL, "app", "rev-1", configMap("cm", "", nil))
	app := h.app("managed-ns", src("app"))
	app.Spec.SyncPolicy = &v1alpha1.SyncPolicy{
		SyncOptions:              v1alpha1.SyncOptions{"CreateNamespace=true"},
		ManagedNamespaceMetadata: &v1alpha1.ManagedNamespaceMetadata{Labels: map[string]string{"team": "a"}},
	}
	in := Input{App: app}
	res := h.compare(in)
	h.createTargets(res)
	// The destination namespace carries tracking metadata of the app (it was created by a
	// sync), so it is in the managed live set without being in the manifests.
	h.mutate(&unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": h.ns}}},
		func(u *unstructured.Unstructured) {
			// As applied by the sync: tracking plus the managed metadata (server-side apply).
			tracked := h.track(u, app)
			a := tracked.GetAnnotations()
			a[v1alpha1.AnnotationKeySyncOptions] = "ServerSideApply=true"
			u.SetAnnotations(a)
			l := u.GetLabels()
			l["team"] = "a"
			u.SetLabels(l)
		})
	res = h.eventually(in, func(r *Result) bool {
		mr := findManaged(r, "Namespace", h.ns)
		return mr != nil && mr.Live != nil && r.SyncStatus.Status == v1alpha1.SyncStatusCodeSynced
	}, "managed namespace compared against its synthesized target")
	ns := findManaged(res, "Namespace", h.ns)
	require.NotNil(t, ns.Target, "a target is synthesized so the namespace is not pruned")
	assert.False(t, ns.RequiresPruning)

	// A metadata change since the last sync makes the application OutOfSync.
	res = h.compare(Input{App: app, LastSyncResult: &v1alpha1.SyncOperationResult{ManagedNamespaceMetadata: &v1alpha1.ManagedNamespaceMetadata{Labels: map[string]string{"team": "b"}}}})
	assert.Equal(t, v1alpha1.SyncStatusCodeOutOfSync, res.SyncStatus.Status)
}

func TestEnvResourceTreeAndHosts(t *testing.T) {
	t.Parallel()
	h := newHarness(t, withSettings(map[string]string{"application.allowedNodeLabels": "topology.kubernetes.io/zone"}))
	labels := map[string]string{"app": "web"}
	deploy := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "web"},
		"spec": map[string]any{
			"selector": map[string]any{"matchLabels": map[string]any{"app": "web"}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app": "web"}},
				"spec":     map[string]any{"containers": []any{map[string]any{"name": "c", "image": "nginx:1"}}},
			},
		},
	}}
	svc := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]any{"name": "web", "annotations": map[string]any{v1alpha1.AnnotationKeyLinkPrefix + "docs": "https://docs.example.com"}},
		"spec":     map[string]any{"selector": map[string]any{"app": "web"}, "ports": []any{map[string]any{"port": int64(80)}}},
	}}
	h.repo.set(repoURL, "app", "rev-1", deploy, svc)
	app := h.app("tree", src("app"))
	app.Annotations = map[string]string{v1alpha1.AnnotationKeyLinkPrefix + "home": "https://home.example.com"}
	in := Input{App: app}
	res := h.compare(in)
	h.createTargets(res)

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: h.ns + "-node", Labels: map[string]string{"topology.kubernetes.io/zone": "z1", "secret-label": "x"}},
	}
	created, err := h.kube.CoreV1().Nodes().Create(h.ctx, node, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = h.kube.CoreV1().Nodes().Delete(context.Background(), created.Name, metav1.DeleteOptions{}) })
	created.Status.Capacity = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}
	_, err = h.kube.CoreV1().Nodes().UpdateStatus(h.ctx, created, metav1.UpdateOptions{})
	require.NoError(t, err)

	// Simulate the Deployment controller: ReplicaSet and Pod owned by the Deployment.
	liveDeploy, err := h.kube.AppsV1().Deployments(h.ns).Get(h.ctx, "web", metav1.GetOptions{})
	require.NoError(t, err)
	rs, err := h.kube.AppsV1().ReplicaSets(h.ns).Create(h.ctx, &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: "web-1", Labels: labels, OwnerReferences: []metav1.OwnerReference{
			{APIVersion: "apps/v1", Kind: "Deployment", Name: liveDeploy.Name, UID: liveDeploy.UID, Controller: ptrTo(true)}}},
		Spec: appsv1.ReplicaSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx:1"}}}},
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = h.kube.CoreV1().Pods(h.ns).Create(h.ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-1-a", Labels: labels, OwnerReferences: []metav1.OwnerReference{
			{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID}}},
		Spec: corev1.PodSpec{NodeName: node.Name, Containers: []corev1.Container{{
			Name: "c", Image: "nginx:1",
			Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")}},
		}}},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	res = h.eventually(in, func(r *Result) bool {
		return r.Tree != nil && r.Tree.FindNode("", "Pod", h.ns, "web-1-a") != nil && len(r.Tree.Hosts) == 1
	}, "pod and host in the tree")
	podNode := res.Tree.FindNode("", "Pod", h.ns, "web-1-a")
	require.Len(t, podNode.ParentRefs, 1)
	assert.Equal(t, "ReplicaSet", podNode.ParentRefs[0].Kind)
	assert.Equal(t, h.ns, podNode.ParentRefs[0].Namespace)
	assert.Equal(t, []string{"nginx:1"}, podNode.Images)
	nodeName, _ := infoValue(podNode.Info, InfoNode)
	assert.Equal(t, node.Name, nodeName)
	rsNode := res.Tree.FindNode("apps", "ReplicaSet", h.ns, "web-1")
	require.NotNil(t, rsNode)
	assert.Equal(t, "Deployment", rsNode.ParentRefs[0].Kind)
	svcNode := res.Tree.FindNode("", "Service", h.ns, "web")
	require.NotNil(t, svcNode)
	assert.Equal(t, labels, svcNode.NetworkingInfo.TargetLabels)

	host := res.Tree.Hosts[0]
	assert.Equal(t, node.Name, host.Name)
	assert.Equal(t, map[string]string{"topology.kubernetes.io/zone": "z1"}, host.Labels, "only allowed node labels")
	require.Len(t, host.ResourcesInfo, 2)
	assert.Equal(t, corev1.ResourceCPU, host.ResourcesInfo[0].ResourceName)
	assert.Equal(t, int64(4000), host.ResourcesInfo[0].Capacity)
	assert.Equal(t, int64(250), host.ResourcesInfo[0].RequestedByApp)

	assert.Equal(t, []string{"https://docs.example.com", "https://home.example.com"}, res.Summary.ExternalURLs)
	assert.Equal(t, []string{"nginx:1"}, res.Summary.Images)
	assert.False(t, res.Summary.IsAppOfApps)

	// The tree can be rebuilt without a comparison (tree-only refresh).
	tr, err := h.mgr.ResourceTree(h.ctx, TreeInput{App: app, Project: permissiveProject(), Managed: res.Managed})
	require.NoError(t, err)
	assert.Len(t, tr.Tree.Nodes, len(res.Tree.Nodes))
}

func TestEnvServerSideDiff(t *testing.T) {
	t.Parallel()
	h := newHarness(t, withServerSideDiff())
	h.repo.set(repoURL, "app", "rev-1", configMap("cm", "", map[string]string{"k": "v"}))
	app := h.app("ssd", src("app"))
	app.Annotations = map[string]string{v1alpha1.AnnotationKeyCompareOptions: "ServerSideDiff=true"}
	in := Input{App: app}
	res := h.compare(in)
	assert.True(t, res.ServerSideDiff)
	h.createTargets(res)
	res = h.eventually(in, func(r *Result) bool { return r.SyncStatus.Status == v1alpha1.SyncStatusCodeSynced }, "synced with server-side diff")
	assert.False(t, res.HasCondition(v1alpha1.ApplicationConditionUnknownError), "%v", res.Conditions)
	h.mutate(configMap("cm", h.ns, nil), func(u *unstructured.Unstructured) {
		require.NoError(t, unstructured.SetNestedField(u.Object, "changed", "data", "k"))
	})
	h.eventually(in, func(r *Result) bool { return r.SyncStatus.Status == v1alpha1.SyncStatusCodeOutOfSync }, "drift with server-side diff")

	// Without a provider, server-side diff reports an UnknownError and fails.
	h2 := newHarness(t)
	h2.repo.set(repoURL, "app", "rev-1", configMap("cm", "", nil))
	res = h2.compare(Input{App: app})
	assert.Contains(t, conditionMessages(res, v1alpha1.ApplicationConditionUnknownError), "server-side diff is not available")
}

func TestEnvMaskSecrets(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	secret := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "creds"},
		"stringData": map[string]any{"password": "git-value"},
	}}
	h.repo.set(repoURL, "app", "rev-1", secret)
	app := h.app("secrets", src("app"))
	in := Input{App: app}
	res := h.compare(in)
	live := res.Targets[0].DeepCopy()
	unstructured.RemoveNestedField(live.Object, "stringData")
	require.NoError(t, unstructured.SetNestedField(live.Object, map[string]any{"password": "Y2x1c3Rlci12YWx1ZQ=="}, "data"))
	h.create(live)
	res = h.eventually(in, func(r *Result) bool {
		mr := findManaged(r, "Secret", "creds")
		return mr != nil && mr.Live != nil
	}, "secret observed")
	masked, err := res.MaskSecrets(h.ctx, nil)
	require.NoError(t, err)
	mr := findManagedIn(masked, "Secret", "creds")
	require.NotNil(t, mr)
	assert.True(t, mr.Diff.Modified, "a changed value stays visible")
	for _, doc := range []string{string(mr.Diff.NormalizedLive), string(mr.Diff.PredictedLive), toJSON(t, mr.Live), toJSON(t, mr.Target)} {
		assert.NotContains(t, doc, "git-value")
		assert.NotContains(t, doc, "Y2x1c3Rlci12YWx1ZQ==")
		assert.NotContains(t, doc, "Z2l0LXZhbHVl")
	}
	orig := findManaged(res, "Secret", "creds")
	assert.Contains(t, string(orig.Diff.NormalizedLive), "Y2x1c3Rlci12YWx1ZQ==", "the original result is not modified")
}

func findManagedIn(list []ManagedResource, kind, name string) *ManagedResource {
	for i := range list {
		if list[i].Key.Kind == kind && list[i].Key.Name == name {
			return &list[i]
		}
	}
	return nil
}

func TestEnvSettingsChangeInvalidatesCache(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := h.ctx
	cc1, err := h.live.ClusterCache(ctx, h.cluster)
	require.NoError(t, err)
	before := cc1.GetClusterInfo().LastCacheSyncTime
	require.NotNil(t, before)
	// Same settings: same cache, no resync.
	cc2, err := h.live.ClusterCache(ctx, h.cluster)
	require.NoError(t, err)
	assert.Equal(t, before, cc2.GetClusterInfo().LastCacheSyncTime)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "patrol-cm", Namespace: controlPlaneNS, Labels: map[string]string{v1alpha1.LabelKeyPartOf: v1alpha1.LabelValuePartOf}},
		Data:       map[string]string{"resource.customLabels": "team"},
	}
	h.live.o.Settings = StaticSettings{Settings: parseSettings(cm)}
	time.Sleep(10 * time.Millisecond)
	cc3, err := h.live.ClusterCache(ctx, h.cluster)
	require.NoError(t, err)
	after := cc3.GetClusterInfo().LastCacheSyncTime
	require.NotNil(t, after)
	assert.True(t, after.After(*before), "a cache-relevant settings change resyncs the cache")
}

func ptrTo[T any](v T) *T { return &v }

func parseSettings(cms ...*corev1.ConfigMap) *settings.Settings {
	return settings.Parse(settings.Input{Namespace: controlPlaneNS, ConfigMaps: cms})
}

func TestEnvHydratorComparesSyncSource(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	app := h.app("hydrated", src("unused"))
	app.Spec.Source = nil
	app.Spec.SourceHydrator = &v1alpha1.SourceHydrator{
		DrySource:  v1alpha1.DrySource{RepoURL: repoURL, TargetRevision: "main", Path: "dry"},
		SyncSource: v1alpha1.SyncSource{TargetBranch: "env/prod", Path: "hydrated/prod"},
		HydrateTo:  &v1alpha1.HydrateTo{TargetBranch: "env/prod-next"},
	}
	h.repo.set(repoURL, "hydrated/prod", "sha-hydrated", configMap("cm", "", nil))
	res := h.compare(Input{App: app})
	require.NotNil(t, res.SyncStatus.ComparedTo.Source)
	assert.Equal(t, "hydrated/prod", res.SyncStatus.ComparedTo.Source.Path)
	assert.Equal(t, "env/prod", res.SyncStatus.ComparedTo.Source.TargetRevision)
	assert.Equal(t, "sha-hydrated", res.SyncStatus.Revision)
	require.NotNil(t, findManaged(res, "ConfigMap", "cm"))

	h.repo.fail(repoURL, "hydrated/prod", fmt.Errorf("hydrated/prod: %s", errMessageAppPathDoesNotExist))
	res = h.compare(Input{App: app})
	assert.Contains(t, conditionMessages(res, v1alpha1.ApplicationConditionComparisonError),
		"app path does not exist - waiting for an external process to update env/prod from env/prod-next")
}

func TestEnvResourcesTruncated(t *testing.T) {
	t.Parallel()
	h := newHarness(t, withDeps(func(d *Deps) { d.MaxResources = 2 }))
	var objs []*unstructured.Unstructured
	for i := range 5 {
		objs = append(objs, configMap(fmt.Sprintf("cm-%d", i), "", nil))
	}
	h.repo.set(repoURL, "app", "rev-1", objs...)
	res := h.compare(Input{App: h.app("truncated", src("app"))})
	assert.Len(t, res.Resources, 2)
	assert.Equal(t, 3, res.ResourcesTruncated)
	assert.Len(t, res.Managed, 5, "managed resources are not truncated")
	assert.Equal(t, "status.resources lists 2 of 5 resources", conditionMessages(res, ConditionResourcesTruncated))
}
