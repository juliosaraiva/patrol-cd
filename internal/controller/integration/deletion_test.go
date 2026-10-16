package integration

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

const blockFinalizer = "test.patrol.example/block"

func (e *env) deleteApp(a *v1alpha1.Application) {
	e.t.Helper()
	require.NoError(e.t, e.c.Delete(e.ctx, a.DeepCopy()))
}

func (e *env) waitAppGone(a *v1alpha1.Application) {
	e.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	var last *v1alpha1.Application
	for time.Now().Before(deadline) {
		cur := &v1alpha1.Application{}
		err := e.c.Get(e.ctx, keyOf(a), cur)
		if apierrors.IsNotFound(err) {
			return
		}
		last = cur
		time.Sleep(pollEvery)
	}
	e.t.Fatalf("application %s was not deleted\nlast status: %s", a.Name, describeApp(last))
}

// TestCascadeDeleteReverseWaves: deleting an Application with the background resources
// finalizer deletes its resources highest wave first, waiting while a wave terminates, and
// keeps Delete=false and helm.sh/resource-policy=keep resources (APP-059, APP-061, SYNC-070).
func TestCascadeDeleteReverseWaves(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.startController()
	w1 := withWave(configMap("cd-w1", nil, nil), 1)
	w1.SetFinalizers([]string{blockFinalizer})
	rev := e.repo.commit(e.repoURL, "cd", "main",
		withWave(configMap("cd-w0", nil, nil), 0),
		w1,
		withAnnotation(configMap("cd-keep", nil, nil), v1alpha1.AnnotationKeySyncOptions, "Delete=false"),
		withAnnotation(configMap("cd-helmkeep", nil, nil), "helm.sh/resource-policy", "keep"),
	)
	a := e.newApp("cd", func(a *v1alpha1.Application) { a.Finalizers = []string{v1alpha1.FinalizerResourcesBackground} })
	e.create(a)
	e.waitApp(a, "compared", outOfSync)
	e.waitOp(e.sync(a), "synced", phaseIs(v1alpha1.OperationSucceeded))
	e.waitApp(a, "synced", synced(rev))

	e.deleteApp(a)
	e.waitLive(gvrConfigMap, "cd-w1", "wave 1 terminating", func(u *unstructured.Unstructured, ok bool) bool {
		return ok && u.GetDeletionTimestamp() != nil
	})
	time.Sleep(2 * time.Second)
	w0, ok := e.live(gvrConfigMap, "cd-w0")
	require.True(t, ok)
	assert.Nil(t, w0.GetDeletionTimestamp(), "wave 0 waits while wave 1 is terminating")

	e.mutateLive(gvrConfigMap, "cd-w1", func(u *unstructured.Unstructured) { u.SetFinalizers(nil) })
	e.waitLive(gvrConfigMap, "cd-w0", "wave 0 deleted", gone)
	e.waitAppGone(a)
	for _, name := range []string{"cd-keep", "cd-helmkeep"} {
		_, ok := e.live(gvrConfigMap, name)
		assert.True(t, ok, "%s is kept", name)
	}
}

// TestCascadeDeleteForeground: the default resources finalizer deletes with foreground
// propagation (released here by the fake garbage collector) and then removes the
// Application; an Application without the finalizer leaves its resources (APP-057, APP-060).
func TestCascadeDeleteForeground(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.startController()
	revFG := e.repo.commit(e.repoURL, "fg", "main", configMap("fg-cm", nil, nil))
	revNC := e.repo.commit(e.repoURL, "nc", "main", configMap("nc-cm", nil, nil))
	fg := e.newApp("fg", func(a *v1alpha1.Application) { a.Finalizers = []string{v1alpha1.FinalizerResources} })
	nc := e.newApp("nc")
	e.create(fg)
	e.create(nc)
	for _, a := range []*v1alpha1.Application{fg, nc} {
		e.waitApp(a, "compared", outOfSync)
		e.waitOp(e.sync(a), "synced", phaseIs(v1alpha1.OperationSucceeded))
	}
	e.waitApp(fg, "synced", synced(revFG))
	e.waitApp(nc, "synced", synced(revNC))
	e.deleteApp(fg)
	e.deleteApp(nc)
	e.waitLive(gvrConfigMap, "fg-cm", "deleted", gone)
	e.waitAppGone(fg)
	e.waitAppGone(nc)
	_, ok := e.live(gvrConfigMap, "nc-cm")
	assert.True(t, ok, "non-cascading deletion keeps resources")
}

// TestDeleteWithUnresolvableDestination: an Application whose destination cannot be
// resolved loses its deletion finalizers and is removed without touching resources (APP-063).
func TestDeleteWithUnresolvableDestination(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.startController()
	e.repo.commit(e.repoURL, "nowhere", "main", configMap("nowhere-cm", nil, nil))
	a := e.newApp("nowhere", func(a *v1alpha1.Application) {
		a.Spec.Destination = v1alpha1.ApplicationDestination{Name: "no-such-cluster", Namespace: e.dest}
		a.Finalizers = []string{v1alpha1.FinalizerResources, v1alpha1.FinalizerPostDelete}
	})
	e.create(a)
	got := e.waitApp(a, "InvalidSpecError", func(a *v1alpha1.Application) bool {
		return hasCondition(a, v1alpha1.ApplicationConditionInvalidSpecError) != nil
	})
	assert.Equal(t, v1alpha1.SyncStatusCodeUnknown, got.Status.Sync.Status)
	assert.Contains(t, hasCondition(got, v1alpha1.ApplicationConditionInvalidSpecError).Message, "no-such-cluster")
	e.deleteApp(a)
	e.waitAppGone(a)
}

// TestPreDeleteHook: an Application rendering a PreDelete hook gets the pre-delete
// finalizers; on deletion the hook runs before resources are deleted, and the Application is
// removed once it completed (APP-064, APP-066).
func TestPreDeleteHook(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.startController()
	rev := e.repo.commit(e.repoURL, "pre", "main", configMap("pre-cm", nil, nil), namedHookPod("pre-hook", v1alpha1.HookTypePreDelete))
	a := e.newApp("pre", func(a *v1alpha1.Application) { a.Finalizers = []string{v1alpha1.FinalizerResourcesBackground} })
	e.create(a)
	got := e.waitApp(a, "pre-delete finalizers added", func(a *v1alpha1.Application) bool {
		return slices.Contains(a.Finalizers, v1alpha1.FinalizerPreDelete) && slices.Contains(a.Finalizers, v1alpha1.FinalizerPreDeleteCleanup)
	})
	assert.NotContains(t, got.Finalizers, v1alpha1.FinalizerPostDelete)
	e.waitOp(e.sync(a), "synced", phaseIs(v1alpha1.OperationSucceeded))
	e.waitApp(a, "synced", synced(rev))
	_, ok := e.live(gvrPod, "pre-hook")
	assert.False(t, ok, "PreDelete hooks are not part of syncs")

	e.deleteApp(a)
	e.waitLive(gvrPod, "pre-hook", "PreDelete hook created", exists)
	time.Sleep(time.Second)
	cm, ok := e.live(gvrConfigMap, "pre-cm")
	require.True(t, ok)
	assert.Nil(t, cm.GetDeletionTimestamp(), "resources are kept while the PreDelete hook runs")
	e.setPodPhase("pre-hook", corev1.PodSucceeded)
	e.waitLive(gvrConfigMap, "pre-cm", "deleted after the hook", gone)
	e.waitAppGone(a)
}

// TestPreDeleteHookFailure: a failing PreDelete hook blocks deletion with DeletionError and
// is deleted so it can be retried.
func TestPreDeleteHookFailure(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.startController()
	e.repo.commit(e.repoURL, "prefail", "main", configMap("prefail-cm", nil, nil), namedHookPod("prefail-hook", v1alpha1.HookTypePreDelete))
	a := e.newApp("prefail", func(a *v1alpha1.Application) { a.Finalizers = []string{v1alpha1.FinalizerResourcesBackground} })
	e.create(a)
	e.waitApp(a, "pre-delete finalizer", func(a *v1alpha1.Application) bool { return slices.Contains(a.Finalizers, v1alpha1.FinalizerPreDelete) })
	e.waitOp(e.sync(a), "synced", phaseIs(v1alpha1.OperationSucceeded))
	e.deleteApp(a)
	e.waitLive(gvrPod, "prefail-hook", "hook created", exists)
	e.setPodPhase("prefail-hook", corev1.PodFailed)
	got := e.waitApp(a, "DeletionError", func(a *v1alpha1.Application) bool {
		c := hasCondition(a, v1alpha1.ApplicationConditionDeletionError)
		return c != nil && strings.Contains(c.Message, "PreDelete hook(s) failed: "+e.dest+"/prefail-hook")
	})
	assert.NotNil(t, got.DeletionTimestamp)
	_, ok := e.live(gvrConfigMap, "prefail-cm")
	assert.True(t, ok, "resources untouched after a failed PreDelete hook")
	// Unblock: remove the hook from Git and the finalizer is no longer needed by the app,
	// but deletion keeps the stage-less finalizer; remove it by hand as an operator would.
	cur := e.getApp(a)
	cur.Finalizers = slices.DeleteFunc(cur.Finalizers, func(f string) bool { return f == v1alpha1.FinalizerPreDelete })
	require.NoError(t, e.c.Update(e.ctx, cur))
	e.waitAppGone(a)
}

// TestPostDeleteHook: a PostDelete hook runs after the cascade deleted the resources; the
// cleanup stage deletes it (no delete policy) and the Application is removed (APP-065/066).
func TestPostDeleteHook(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.startController()
	rev := e.repo.commit(e.repoURL, "post", "main", configMap("post-cm", nil, nil), namedHookPod("post-hook", v1alpha1.HookTypePostDelete))
	a := e.newApp("post", func(a *v1alpha1.Application) { a.Finalizers = []string{v1alpha1.FinalizerResourcesBackground} })
	e.create(a)
	e.waitApp(a, "post-delete finalizers added", func(a *v1alpha1.Application) bool {
		return slices.Contains(a.Finalizers, v1alpha1.FinalizerPostDelete) && slices.Contains(a.Finalizers, v1alpha1.FinalizerPostDeleteCleanup)
	})
	e.waitOp(e.sync(a), "synced", phaseIs(v1alpha1.OperationSucceeded))
	e.waitApp(a, "synced", synced(rev))

	e.deleteApp(a)
	e.waitLive(gvrConfigMap, "post-cm", "resources deleted first", gone)
	e.waitLive(gvrPod, "post-hook", "PostDelete hook created after the cascade", exists)
	got := e.getApp(a)
	assert.NotContains(t, got.Finalizers, v1alpha1.FinalizerResourcesBackground)
	assert.Contains(t, got.Finalizers, v1alpha1.FinalizerPostDelete)
	e.setPodPhase("post-hook", corev1.PodSucceeded)
	e.waitLive(gvrPod, "post-hook", "hook deleted by the cleanup stage", gone)
	e.waitAppGone(a)
}

// TestPreDeleteHookNameTaken: a PreDelete hook whose name belongs to an object the
// Application does not track is skipped (upstream executeHooks), with a Warning Event, and
// deletion proceeds; the foreign object is left alone.
func TestPreDeleteHookNameTaken(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctl := e.startController()
	rev := e.repo.commit(e.repoURL, "taken", "main", configMap("taken-cm", nil, nil), namedHookPod("taken-hook", v1alpha1.HookTypePreDelete))
	a := e.newApp("taken", func(a *v1alpha1.Application) { a.Finalizers = []string{v1alpha1.FinalizerResourcesBackground} })
	e.create(a)
	e.waitApp(a, "pre-delete finalizer", func(a *v1alpha1.Application) bool { return slices.Contains(a.Finalizers, v1alpha1.FinalizerPreDelete) })
	e.waitOp(e.sync(a), "synced", phaseIs(v1alpha1.OperationSucceeded))
	e.waitApp(a, "synced", synced(rev))

	foreign := namedHookPod("taken-hook", v1alpha1.HookTypePreDelete)
	foreign.SetAnnotations(nil)
	foreign.SetLabels(map[string]string{"owner": "someone-else"})
	foreign.SetNamespace(e.dest)
	_, err := e.dyn.Resource(gvrPod).Namespace(e.dest).Create(e.ctx, foreign, metav1.CreateOptions{})
	require.NoError(t, err)

	e.deleteApp(a)
	e.waitLive(gvrConfigMap, "taken-cm", "deleted after the skipped hook", gone)
	e.waitAppGone(a)
	pod, ok := e.live(gvrPod, "taken-hook")
	require.True(t, ok, "the foreign object is kept")
	assert.Equal(t, map[string]string{"owner": "someone-else"}, pod.GetLabels())
	assert.Nil(t, pod.GetDeletionTimestamp())
	assert.True(t, ctl.events.has("DeleteHookSkipped: PreDelete hook "), "a Warning Event names the skipped hook")
}

// TestPreDeleteHookExcludedKind: when the hook's kind became excluded from the live-state
// cache after the pre-delete finalizer was added, the cache never observes the hook; its
// health is read from the object itself, so deletion waits for it and then proceeds instead
// of hanging.
func TestPreDeleteHookExcludedKind(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctl := e.startController()
	rev := e.repo.commit(e.repoURL, "excl", "main", configMap("excl-cm", nil, nil), namedHookPod("excl-hook", v1alpha1.HookTypePreDelete))
	a := e.newApp("excl", func(a *v1alpha1.Application) { a.Finalizers = []string{v1alpha1.FinalizerResourcesBackground} })
	e.create(a)
	e.waitApp(a, "pre-delete finalizer", func(a *v1alpha1.Application) bool { return slices.Contains(a.Finalizers, v1alpha1.FinalizerPreDelete) })
	e.waitOp(e.sync(a), "synced", phaseIs(v1alpha1.OperationSucceeded))
	e.waitApp(a, "synced", synced(rev))

	// Pods become excluded while no controller runs; the Application is deleted meanwhile.
	ctl.stop()
	e.deleteApp(a)
	e.startController(withSettings(map[string]string{
		"resource.exclusions": "- apiGroups: [\"\"]\n  kinds: [Pod]\n  clusters: [\"*\"]\n",
	}))
	e.waitLive(gvrPod, "excl-hook", "PreDelete hook created", exists)
	time.Sleep(2 * time.Second)
	cm, ok := e.live(gvrConfigMap, "excl-cm")
	require.True(t, ok)
	assert.Nil(t, cm.GetDeletionTimestamp(), "resources are kept while the hook runs")
	e.setPodPhase("excl-hook", corev1.PodSucceeded)
	e.waitLive(gvrConfigMap, "excl-cm", "deleted after the hook", gone)
	e.waitAppGone(a)
}
