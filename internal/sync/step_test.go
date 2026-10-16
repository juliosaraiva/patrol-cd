package sync

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/health"
)

func TestStepSingleWaveSucceedsAfterApply(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cm("a"), cm("b"))
	h.kube.put(cm("b"), cm("extra"))
	res := h.step()
	assert.Equal(t, v1alpha1.OperationSucceeded, res.Phase)
	assert.Equal(t, "successfully synced (all tasks run)", res.Message)
	require.Len(t, res.Results, 3)
	extra := findResult(res.Results, "extra", "")
	assert.Equal(t, v1alpha1.ResultCodePruned, extra.Status)
	assert.Equal(t, "pruned", extra.Message)
	assert.Equal(t, v1alpha1.OperationSucceeded, extra.HookPhase)
	a := findResult(res.Results, "a", "")
	assert.Equal(t, v1alpha1.ResultCodeSynced, a.Status)
	assert.Equal(t, v1alpha1.OperationRunning, a.HookPhase)
	assert.Equal(t, "configmap/a created", a.Message)
	assert.Equal(t, "configmap/b configured", findResult(res.Results, "b", "").Message)
	// Prune runs before apply in the same wave, the dry run before anything.
	calls := h.kube.callsWith("")
	require.GreaterOrEqual(t, len(calls), 4)
	assert.True(t, strings.HasPrefix(calls[0], "dryrun "), calls)
	assert.Contains(t, calls, "delete dest-ns/extra Foreground")
	assert.Zero(t, res.RequeueAfter)
	assert.Equal(t, int64(3), res.Progress.TotalTasks)
}

func TestStepMultiWaveHealthGating(t *testing.T) {
	t.Parallel()
	h := newHarness(t, wave(pod("p-1"), -1), pod("p0"), wave(pod("p1"), 1))
	h.health.set("p-1", health.StatusProgressing)

	res := h.step()
	assert.Equal(t, v1alpha1.OperationRunning, res.Phase)
	assert.Equal(t, "waiting for healthy state of /Pod/p-1", res.Message)
	assert.Equal(t, []string{"p-1"}, names(res.Results))
	assert.Equal(t, v1alpha1.SyncPhaseSync, res.Progress.SyncPhase)
	assert.Equal(t, int64(-1), res.Progress.Wave)

	res = h.step()
	assert.Equal(t, "waiting for healthy state of /Pod/p-1", res.Message, "still progressing")
	assert.Equal(t, time.Second, res.RequeueAfter)

	h.health.set("p-1", health.StatusHealthy)
	h.health.set("p0", health.StatusDegraded)
	res = h.step()
	assert.Equal(t, []string{"p-1", "p0"}, names(res.Results))
	assert.Equal(t, v1alpha1.OperationSucceeded, findResult(res.Results, "p-1", "").HookPhase)
	assert.Equal(t, "Healthy message", findResult(res.Results, "p-1", "").Message)

	res = h.step()
	assert.Equal(t, v1alpha1.OperationFailed, res.Phase)
	assert.Equal(t, "one or more synchronization tasks completed unsuccessfully", res.Message)
	assert.Nil(t, findResult(res.Results, "p1", ""), "later waves never run")
}

func TestStepDegradedSingleStepStillSucceeds(t *testing.T) {
	t.Parallel()
	h := newHarness(t, pod("p"))
	h.health.set("p", health.StatusDegraded)
	res := h.step()
	assert.Equal(t, v1alpha1.OperationSucceeded, res.Phase)
	assert.Equal(t, "successfully synced (all tasks run)", res.Message)
}

func TestStepWaveDelay(t *testing.T) {
	t.Parallel()
	h := newHarness(t, pod("a"), wave(pod("b"), 1))
	e, err := NewEngine(h.kube, h.health.fn, Config{WaveDelay: 2 * time.Second, PollInterval: time.Second, Clock: h.clock})
	require.NoError(t, err)
	h.engine = e
	res := h.step()
	assert.Equal(t, 2*time.Second, res.RequeueAfter)
	require.NotNil(t, res.Progress.StartedAt)

	// Too early: health is not assessed yet.
	h.clock.Step(500 * time.Millisecond)
	res = h.step()
	assert.Equal(t, v1alpha1.OperationRunning, res.Phase)
	assert.Equal(t, "waiting for healthy state of /Pod/a", res.Message)
	assert.Equal(t, 1500*time.Millisecond, res.RequeueAfter)
	assert.Equal(t, v1alpha1.OperationRunning, findResult(res.Results, "a", "").HookPhase)

	h.clock.Step(1500 * time.Millisecond)
	res = h.step()
	assert.Equal(t, v1alpha1.OperationSucceeded, findResult(res.Results, "a", "").HookPhase)
	assert.Equal(t, v1alpha1.OperationSucceeded, res.Phase, "no delay after the final wave")
	assert.Equal(t, "successfully synced (all tasks run)", res.Message)
}

func TestStepDryRunFailureStopsEverything(t *testing.T) {
	t.Parallel()
	h := newHarness(t, pod("first"), wave(pod("bad"), 2))
	h.kube.dryRunErr["bad"] = errors.New("invalid object failing dry-run")
	res := h.step()
	assert.Equal(t, v1alpha1.OperationFailed, res.Phase)
	assert.Equal(t, "one or more objects failed to apply (dry run)", res.Message)
	require.Len(t, res.Results, 1)
	assert.Equal(t, "invalid object failing dry-run", res.Results[0].Message)
	assert.Equal(t, v1alpha1.ResultCodeSyncFailed, res.Results[0].Status)
	assert.Empty(t, h.kube.callsWith("apply"), "nothing applied")
}

func TestStepInvalidTasks(t *testing.T) {
	t.Parallel()
	h := newHarness(t, pod("ok"), newObj("example.com/v1", "Widget", testNS, "w"))
	res := h.step()
	assert.Equal(t, v1alpha1.OperationFailed, res.Phase)
	assert.Equal(t, "one or more synchronization tasks are not valid: failed to discover server resources for group version example.com/v1: the server could not find the requested resource", res.Message)
	require.Len(t, res.Results, 1)
	assert.Equal(t, `The Kubernetes API could not find example.com/Widget for requested resource dest-ns/w. Make sure the "Widget" CRD is installed on the destination cluster.`, res.Results[0].Message)
	assert.Empty(t, h.kube.callsWith("dryrun"))
}

func TestStepCRDAndCustomResource(t *testing.T) {
	t.Parallel()
	h := newHarness(t, crd("example.com", "Widget", "widgets", "Namespaced"), newObj("example.com/v1", "Widget", "", "w"))
	res := h.run(10)
	assert.Equal(t, v1alpha1.OperationSucceeded, res.Phase, res.Message)
	assert.Equal(t, "widget.example.com/w created", findResult(res.Results, "w", "").Message)
	assert.NotContains(t, h.kube.callsWith("dryrun"), "dryrun apply w", "CR dry run is skipped")
}

func TestStepPruneOptions(t *testing.T) {
	t.Parallel()
	t.Run("prune disabled for the operation", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.in.Prune = false
		h.kube.put(cm("extra"))
		res := h.step()
		assert.Equal(t, v1alpha1.OperationSucceeded, res.Phase)
		assert.Equal(t, v1alpha1.ResultCodePruneSkipped, res.Results[0].Status)
		assert.Equal(t, "ignored (requires pruning)", res.Results[0].Message)
		assert.Empty(t, h.kube.callsWith("delete"))
	})
	for _, tc := range []struct{ name, ann, app string }{
		{"resource Prune=false", "Prune=false", ""},
		{"app Prune=false", "", "Prune=false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			obj := cm("extra")
			if tc.ann != "" {
				annotate(obj, v1alpha1.AnnotationKeySyncOptions, tc.ann)
			}
			if tc.app != "" {
				h.in.SyncOptions = v1alpha1.SyncOptions{tc.app}
			}
			h.kube.put(obj)
			res := h.step()
			assert.Equal(t, v1alpha1.OperationSucceeded, res.Phase)
			assert.Equal(t, "ignored (no prune)", res.Results[0].Message)
			assert.Empty(t, h.kube.callsWith("delete"))
		})
	}
	t.Run("propagation policy", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.in.SyncOptions = v1alpha1.SyncOptions{"PrunePropagationPolicy=orphan"}
		h.kube.put(cm("extra"))
		h.step()
		assert.Equal(t, []string{"delete dest-ns/extra Orphan"}, h.kube.callsWith("delete"))
	})
	t.Run("prune failure fails the operation", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.kube.put(cm("extra"))
		h.kube.deleteErr["extra"] = errors.New("foo")
		res := h.step()
		assert.Equal(t, v1alpha1.OperationFailed, res.Phase)
		assert.Equal(t, "one or more objects failed to apply, reason: foo", res.Message)
		assert.Equal(t, v1alpha1.ResultCodeSyncFailed, res.Results[0].Status)
	})
	t.Run("dry run operation", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, cm("new"))
		h.in.DryRun = true
		h.kube.put(cm("extra"))
		res := h.step()
		assert.Equal(t, v1alpha1.OperationSucceeded, res.Phase)
		assert.Equal(t, "pruned (dry run)", findResult(res.Results, "extra", "").Message)
		assert.Equal(t, v1alpha1.OperationSucceeded, findResult(res.Results, "new", "").HookPhase)
		assert.Empty(t, h.kube.callsWith("delete"))
		assert.Empty(t, h.kube.callsWith("apply"))
	})
}

func TestStepPruneConfirmation(t *testing.T) {
	t.Parallel()
	for _, appDefault := range []string{"", "Prune=true", "Prune=false", "Prune=confirm"} {
		t.Run("default "+appDefault, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			if appDefault != "" {
				h.in.SyncOptions = v1alpha1.SyncOptions{appDefault}
			}
			h.kube.put(annotate(pod("my-pod"), v1alpha1.AnnotationKeySyncOptions, "Prune=confirm"))
			res := h.step()
			assert.Equal(t, v1alpha1.OperationRunning, res.Phase)
			assert.Equal(t, "waiting for pruning confirmation of /Pod/my-pod", res.Message)
			assert.Empty(t, res.Results)

			// An approval older than the operation start does not count.
			h.in.App.Annotations = map[string]string{v1alpha1.AnnotationKeyDeletionApproved: h.in.StartedAt.Add(-time.Minute).Format(time.RFC3339)}
			assert.Equal(t, "waiting for pruning confirmation of /Pod/my-pod", h.step().Message)

			h.in.App.Annotations = map[string]string{v1alpha1.AnnotationKeyDeletionApproved: h.in.StartedAt.Format(time.RFC3339)}
			res = h.step()
			assert.Equal(t, v1alpha1.OperationSucceeded, res.Phase)
			require.Len(t, res.Results, 1)
			assert.Equal(t, v1alpha1.ResultCodePruned, res.Results[0].Status)
			assert.Equal(t, "pruned", res.Results[0].Message)
		})
	}
}

func TestStepPruneWaitsForDeletionBeforeNextWave(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.kube.put(wave(pod("pod-1"), 1), wave(pod("pod-2"), 2))
	// Simulate a finalizer that keeps pod-2 terminating.
	h.kube.mu.Lock()
	h.kube.objs[ResourceKey{Kind: "Pod", Namespace: testNS, Name: "pod-2"}].SetFinalizers([]string{"example.com/slow"})
	h.kube.mu.Unlock()

	res := h.step()
	assert.Equal(t, []string{"pod-2"}, names(res.Results))
	res = h.step()
	assert.Equal(t, "waiting for deletion of /Pod/pod-2", res.Message)
	assert.Len(t, res.Results, 1)

	h.kube.mu.Lock()
	delete(h.kube.objs, ResourceKey{Kind: "Pod", Namespace: testNS, Name: "pod-2"})
	h.kube.mu.Unlock()
	res = h.step()
	assert.Equal(t, v1alpha1.OperationSucceeded, res.Phase)
	assert.Equal(t, []string{"pod-2", "pod-1"}, names(res.Results))
}

func TestStepSyncFailHooks(t *testing.T) {
	t.Parallel()
	h := newHarness(t, pod("app"), hookObj(pod("successful-sync-fail-hook"), "SyncFail"), hookObj(pod("failed-sync-fail-hook"), "SyncFail"))
	h.kube.applyErr["app"] = errors.New("fake pod failure")
	h.kube.applyErr["failed-sync-fail-hook"] = errors.New("fake hook failure")
	h.health.set("successful-sync-fail-hook", health.StatusProgressing)

	res := h.step()
	assert.Equal(t, v1alpha1.OperationRunning, res.Phase)
	assert.Equal(t, "waiting for completion of hook /Pod/failed-sync-fail-hook and 1 more hooks", res.Message)
	assert.Equal(t, v1alpha1.OperationFailed, findResult(res.Results, "app", "").HookPhase)
	assert.Equal(t, v1alpha1.ResultCodeSyncFailed, findResult(res.Results, "failed-sync-fail-hook", "").Status)
	assert.Equal(t, v1alpha1.OperationRunning, findResult(res.Results, "successful-sync-fail-hook", "").HookPhase)

	h.health.set("successful-sync-fail-hook", health.StatusHealthy)
	res = h.run(5)
	assert.Equal(t, v1alpha1.OperationFailed, res.Phase)
	assert.Equal(t, "one or more synchronization tasks completed unsuccessfully, reason: fake pod failure\none or more SyncFail hooks failed, reason: fake hook failure", res.Message)
	assert.Equal(t, v1alpha1.OperationSucceeded, findResult(res.Results, "successful-sync-fail-hook", "").HookPhase)
}

func TestStepSyncFailHooksNotRunOnSuccess(t *testing.T) {
	t.Parallel()
	h := newHarness(t, pod("app"), hookObj(pod("on-fail"), "SyncFail"))
	res := h.run(5)
	assert.Equal(t, v1alpha1.OperationSucceeded, res.Phase)
	assert.Nil(t, findResult(res.Results, "on-fail", ""))
}

func TestStepHookLifecycleAndDeletePolicies(t *testing.T) {
	t.Parallel()
	h := newHarness(t,
		hookObj(pod("pre-succeeded"), "PreSync", "HookSucceeded"),
		hookObj(pod("pre-failed-policy"), "PreSync", "HookFailed"),
		hookObj(pod("pre-default"), "PreSync"),
		pod("app"),
		hookObj(pod("post"), "PostSync"),
	)
	for _, n := range []string{"pre-succeeded", "pre-failed-policy", "pre-default", "post"} {
		h.health.set(n, health.StatusProgressing)
	}
	res := h.step()
	assert.Equal(t, "waiting for completion of hook /Pod/pre-default and 2 more hooks", res.Message)
	hook := findResult(res.Results, "pre-default", "")
	assert.Equal(t, v1alpha1.HookTypePreSync, hook.HookType)
	assert.Equal(t, v1alpha1.SyncPhasePreSync, hook.SyncPhase)
	assert.Nil(t, findResult(res.Results, "app", ""), "Sync phase waits for PreSync hooks")
	live, err := h.kube.Get(context.Background(), schema.GroupVersionKind{Version: "v1", Kind: "Pod"}, testNS, "pre-default")
	require.NoError(t, err)
	assert.Contains(t, live.GetFinalizers(), v1alpha1.FinalizerHook)
	assert.Equal(t, "op-uid-1/0/PreSync", live.GetAnnotations()[AnnotationKeyOperation])

	for _, n := range []string{"pre-succeeded", "pre-failed-policy", "pre-default"} {
		h.health.set(n, health.StatusHealthy)
	}
	res = h.step()
	assert.Equal(t, "one or more tasks are running", res.Message, "completion is persisted before cleanup")
	assert.Zero(t, res.RequeueAfter)
	assert.Empty(t, h.kube.callsWith("update"), "finalizers are kept until the completion is persisted")

	res = h.step() // finalizers removed, Sync wave applied
	assert.NotNil(t, findResult(res.Results, "app", ""))
	assert.Len(t, h.kube.callsWith("update"), 3)
	live, err = h.kube.Get(context.Background(), schema.GroupVersionKind{Version: "v1", Kind: "Pod"}, testNS, "pre-default")
	require.NoError(t, err)
	assert.NotContains(t, live.GetFinalizers(), v1alpha1.FinalizerHook)

	h.health.set("post", health.StatusHealthy)
	res = h.run(10)
	assert.Equal(t, v1alpha1.OperationSucceeded, res.Phase)
	assert.Equal(t, "successfully synced (no more tasks)", res.Message)
	deletes := h.kube.callsWith("delete")
	assert.Equal(t, []string{"delete dest-ns/pre-succeeded Foreground"}, deletes, "only HookSucceeded hooks are deleted on success")
}

func TestStepHookDeletedOnFailure(t *testing.T) {
	t.Parallel()
	h := newHarness(t,
		hookObj(pod("keep-on-failure"), "PreSync", "HookSucceeded"),
		hookObj(pod("delete-on-failure"), "PreSync", "HookFailed"),
		hookObj(pod("broken"), "PreSync"),
	)
	h.health.set("broken", health.StatusDegraded)
	res := h.run(10)
	assert.Equal(t, v1alpha1.OperationFailed, res.Phase)
	assert.Equal(t, "one or more synchronization tasks completed unsuccessfully", res.Message)
	assert.Equal(t, []string{"delete dest-ns/delete-on-failure Foreground"}, h.kube.callsWith("delete"))
}

func TestStepBeforeHookCreation(t *testing.T) {
	t.Parallel()
	hook := hookObj(pod("my-pod"), "Sync", "BeforeHookCreation")
	h := newHarness(t, hook)
	old := hook.DeepCopy()
	old.SetFinalizers([]string{v1alpha1.FinalizerHook})
	annotate(old, AnnotationKeyOperation, "previous-op/0/Sync")
	h.kube.put(old)
	h.health.set("my-pod", health.StatusProgressing)

	res := h.step()
	assert.Equal(t, v1alpha1.OperationRunning, res.Phase)
	assert.Equal(t, "waiting for deletion of hook /Pod/my-pod", res.Message)
	assert.Len(t, h.kube.callsWith("update"), 1, "leftover hook finalizer removed")
	assert.Len(t, h.kube.callsWith("delete"), 1)

	res = h.step()
	assert.Equal(t, "waiting for completion of hook /Pod/my-pod", res.Message)
	require.Len(t, res.Results, 1)
	assert.Equal(t, v1alpha1.OperationRunning, res.Results[0].HookPhase)
}

func TestStepRestartAdoptsHookCreatedBeforeCrash(t *testing.T) {
	t.Parallel()
	gen := hookObj(pod(""), "PreSync")
	gen.SetGenerateName("migrate-")
	h := newHarness(t, gen, pod("app"))
	name := HookName("migrate-", "op-uid-1", "PreSync", 0, 0)
	h.health.set(name, health.StatusProgressing)

	// The step creates the hook, but its result is never persisted (crash).
	in := h.in
	in.Live = h.kube.live(in.Targets)
	res, err := h.engine.Step(context.Background(), &in)
	require.NoError(t, err)
	require.Equal(t, "waiting for completion of hook /Pod/"+name, res.Message)

	// A new engine continues from the old persisted state (no results at all).
	h.rebuildEngine()
	res = h.step()
	assert.Equal(t, "waiting for completion of hook /Pod/"+name, res.Message)
	adopted := findResult(res.Results, name, "")
	assert.Equal(t, v1alpha1.ResultCodeSynced, adopted.Status)
	assert.Equal(t, v1alpha1.OperationRunning, adopted.HookPhase)
	assert.Empty(t, h.kube.callsWith("delete"), "the hook of this operation is adopted, not re-created")
	assert.Empty(t, h.kube.callsWith("update"))

	h.health.set(name, health.StatusHealthy)
	res = h.run(10)
	assert.Equal(t, v1alpha1.OperationSucceeded, res.Phase)
	creates := 0
	for _, c := range h.kube.callsWith("apply") {
		if strings.HasSuffix(c, "/"+name) {
			creates++
		}
	}
	assert.Equal(t, 1, creates, "the hook was applied exactly once")
}

func TestStepRetryAttemptReRunsHooks(t *testing.T) {
	t.Parallel()
	gen := hookObj(pod(""), "PreSync")
	gen.SetGenerateName("migrate-")
	h := newHarness(t, gen)
	name := HookName("migrate-", "op-uid-1", "PreSync", 0, 0)
	h.health.set(name, health.StatusDegraded)
	res := h.run(10)
	require.Equal(t, v1alpha1.OperationFailed, res.Phase)

	// Retry attempt 1: the reconciler resets results and progress; the failed hook of attempt
	// 0 has the same deterministic name but must run again, not be adopted.
	h.in.Attempt = 1
	h.in.Previous, h.in.Progress = nil, nil
	h.health.set(name, health.StatusProgressing)
	res = h.step()
	assert.Equal(t, "waiting for deletion of hook /Pod/"+name, res.Message)
	res = h.step()
	assert.Equal(t, "waiting for completion of hook /Pod/"+name, res.Message)
	live, err := h.kube.Get(context.Background(), schema.GroupVersionKind{Version: "v1", Kind: "Pod"}, testNS, name)
	require.NoError(t, err)
	assert.Equal(t, "op-uid-1/1/PreSync", live.GetAnnotations()[AnnotationKeyOperation])
}

func TestStepRunningResourceMissing(t *testing.T) {
	t.Parallel()
	h := newHarness(t, wave(pod("pod-1"), 1), wave(pod("pod-2"), 2))
	h.health.set("pod-1", health.StatusProgressing)
	res := h.step()
	assert.Equal(t, []string{"pod-1"}, names(res.Results))
	h.kube.mu.Lock()
	delete(h.kube.objs, ResourceKey{Kind: "Pod", Namespace: testNS, Name: "pod-1"})
	h.kube.mu.Unlock()
	res = h.step()
	assert.Equal(t, v1alpha1.OperationRunning, res.Phase)
	r := findResult(res.Results, "pod-1", "")
	assert.Equal(t, v1alpha1.ResultCodeSyncFailed, r.Status)
	assert.Equal(t, v1alpha1.OperationError, r.HookPhase)
	assert.Equal(t, "Resource /Pod/pod-1 is missing, it might have been deleted", r.Message)
	assert.Equal(t, v1alpha1.OperationFailed, h.step().Phase)
}

func TestStepTerminate(t *testing.T) {
	t.Parallel()
	h := newHarness(t,
		hookObj(pod("hook-1"), "Sync", "BeforeHookCreation"),
		hookObj(pod("hook-2"), "Sync", "HookFailed"),
		hookObj(pod("hook-3"), "Sync", "HookSucceeded"),
		pod("obj"),
	)
	for _, n := range []string{"hook-1", "hook-2", "hook-3"} {
		h.health.set(n, health.StatusProgressing)
	}
	res := h.step()
	require.Equal(t, v1alpha1.OperationRunning, res.Phase)

	in := h.in
	in.Live = h.kube.live(in.Targets)
	res, err := h.engine.Terminate(context.Background(), &in)
	require.NoError(t, err)
	assert.Equal(t, v1alpha1.OperationCancelled, res.Phase)
	assert.Equal(t, "Operation terminated", res.Message)
	for _, n := range []string{"hook-1", "hook-2", "hook-3"} {
		r := findResult(res.Results, n, "")
		assert.Equal(t, v1alpha1.OperationFailed, r.HookPhase, n)
		assert.Equal(t, "Terminated", r.Message, n)
		_, err := h.kube.Get(context.Background(), schema.GroupVersionKind{Version: "v1", Kind: "Pod"}, testNS, n)
		assert.True(t, apierrors.IsNotFound(err), "%s deleted", n)
	}
	assert.Len(t, h.kube.callsWith("update"), 3, "finalizers removed")
}

func TestStepTerminateCompletedHooks(t *testing.T) {
	t.Parallel()
	h := newHarness(t, hookObj(pod("done"), "PreSync", "HookFailed"), pod("obj"))
	h.health.set("done", health.StatusProgressing)
	h.step()
	h.health.set("done", health.StatusHealthy)
	in := h.in
	in.Live = h.kube.live(in.Targets)
	res, err := h.engine.Terminate(context.Background(), &in)
	require.NoError(t, err)
	assert.Equal(t, v1alpha1.OperationCancelled, res.Phase)
	r := findResult(res.Results, "done", "")
	assert.Equal(t, v1alpha1.OperationSucceeded, r.HookPhase, "real terminal state is recorded")
	assert.Equal(t, []string{"delete dest-ns/done Foreground"}, h.kube.callsWith("delete"), "HookFailed hooks are deleted on termination")
}

func TestStepTransientErrors(t *testing.T) {
	t.Parallel()
	h := newHarness(t, pod("a"), pod("b"))
	h.kube.applyErr["b"] = apierrors.NewServiceUnavailable("etcd leader changed")
	in := h.in
	in.Live = h.kube.live(in.Targets)
	res, err := h.engine.Step(context.Background(), &in)
	require.ErrorIs(t, err, ErrTransient)
	require.NotNil(t, res)
	assert.Equal(t, v1alpha1.OperationRunning, res.Phase)
	assert.Contains(t, res.Message, "etcd leader changed")
	assert.Nil(t, findResult(res.Results, "b", ""), "the failed task stays pending")
	h.persist(res)

	delete(h.kube.applyErr, "b")
	res = h.run(5)
	assert.Equal(t, v1alpha1.OperationSucceeded, res.Phase)
	assert.Equal(t, v1alpha1.ResultCodeSynced, findResult(res.Results, "b", "").Status)
}

func TestStepNamespaceCreation(t *testing.T) {
	t.Parallel()
	t.Run("created before the dry run", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, pod("p"))
		h.in.SyncOptions = v1alpha1.SyncOptions{"CreateNamespace=true"}
		h.kube.dryRunErr["p"] = errors.New("invalid object failing dry-run")
		res := h.step()
		assert.Equal(t, v1alpha1.OperationFailed, res.Phase)
		assert.Equal(t, "one or more objects failed to apply (dry run)", res.Message)
		require.Len(t, res.Results, 2)
		assert.Equal(t, "Namespace", res.Results[0].Kind)
		assert.Equal(t, v1alpha1.ResultCodeSynced, res.Results[0].Status)
		assert.Equal(t, "namespace/dest-ns created", res.Results[0].Message)
		assert.Equal(t, v1alpha1.SyncPhasePreSync, res.Results[0].SyncPhase)
		assert.Equal(t, "Pod", res.Results[1].Kind)
		_, err := h.kube.Get(context.Background(), schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}, "", testNS)
		assert.NoError(t, err, "namespace exists although the dry run failed")
	})
	t.Run("failure", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, pod("p"))
		h.in.SyncOptions = v1alpha1.SyncOptions{"CreateNamespace=true"}
		h.kube.applyErr[testNS] = errors.New("forbidden")
		res := h.step()
		assert.Equal(t, v1alpha1.OperationFailed, res.Phase)
		assert.Equal(t, "the namespace failed to apply, reason: forbidden", res.Message)
	})
	t.Run("get error", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, pod("p"))
		h.in.SyncOptions = v1alpha1.SyncOptions{"CreateNamespace=true"}
		h.kube.getErr[testNS] = apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, testNS, errors.New("nope"))
		res := h.step()
		assert.Equal(t, v1alpha1.OperationFailed, res.Phase)
		assert.Equal(t, "one or more synchronization tasks completed unsuccessfully, reason: namespace auto creation failed: namespaces \"dest-ns\" is forbidden: nope", res.Message)
	})
	t.Run("dry-run operation does not fail resources in the missing namespace", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, pod("p"))
		h.in.SyncOptions = v1alpha1.SyncOptions{"CreateNamespace=true"}
		h.in.DryRun = true
		h.kube.dryRunErr["p"] = errors.New(`namespaces "dest-ns" not found`)
		res := h.run(5)
		assert.Equal(t, v1alpha1.OperationSucceeded, res.Phase, res.Message)
		assert.Equal(t, "pod/p created (dry run)", findResult(res.Results, "p", "").Message)
	})
}

func TestStepReplaceForceSSAAndMigration(t *testing.T) {
	t.Parallel()
	live := cm("csa")
	live.SetManagedFields([]metav1.ManagedFieldsEntry{{Manager: DefaultClientSideApplyMigrationManager, Operation: metav1.ManagedFieldsOperationUpdate}})
	h := newHarness(t,
		annotate(cm("replace-me"), v1alpha1.AnnotationKeySyncOptions, "Replace=true"),
		annotate(cm("force-replace"), v1alpha1.AnnotationKeySyncOptions, "Replace=true,Force=true"),
		annotate(cm("server-side"), v1alpha1.AnnotationKeySyncOptions, "ServerSideApply=true"),
		annotate(cm("csa"), v1alpha1.AnnotationKeySyncOptions, "ServerSideApply=true"),
		annotate(cm("create-replace"), v1alpha1.AnnotationKeySyncOptions, "Replace=true"),
		annotate(namespace("ns-replace"), v1alpha1.AnnotationKeySyncOptions, "Replace=true"),
	)
	h.kube.put(cm("replace-me"), cm("force-replace"), live, namespace("ns-replace"))
	res := h.run(5)
	assert.Equal(t, v1alpha1.OperationSucceeded, res.Phase, res.Message)
	calls := h.kube.callsWith("")
	assert.Contains(t, calls, "replace dest-ns/replace-me")
	assert.Contains(t, calls, "force-replace dest-ns/force-replace")
	assert.Contains(t, calls, "ssa dest-ns/server-side")
	assert.Contains(t, calls, "create dest-ns/create-replace")
	assert.Contains(t, calls, "update /ns-replace")
	assert.Contains(t, calls, "migrate csa kubectl-client-side-apply")
	assert.Equal(t, "Namespace/ns-replace updated", findResult(res.Results, "ns-replace", "").Message)

	h2 := newHarness(t, annotate(cm("csa"), v1alpha1.AnnotationKeySyncOptions, "ServerSideApply=true"))
	h2.kube.put(live)
	h2.in.SyncOptions = v1alpha1.SyncOptions{"ClientSideApplyMigration=false"}
	h2.run(5)
	assert.Empty(t, h2.kube.callsWith("migrate"))

	h3 := newHarness(t, annotate(cm("csa"), v1alpha1.AnnotationKeySyncOptions, "ServerSideApply=true"))
	live3 := live.DeepCopy()
	live3.SetManagedFields([]metav1.ManagedFieldsEntry{{Manager: "my-kubectl", Operation: metav1.ManagedFieldsOperationUpdate}})
	h3.kube.put(live3)
	h3.in.App.Annotations = map[string]string{v1alpha1.AnnotationKeyClientSideApplyMigrationManager: "my-kubectl"}
	h3.run(5)
	assert.Equal(t, []string{"migrate csa my-kubectl"}, h3.kube.callsWith("migrate"))
}

func TestStepApplyOutOfSyncOnly(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cm("same"), cm("changed"), cm("new"))
	h.kube.put(cm("same"), cm("changed"))
	h.in.SyncOptions = v1alpha1.SyncOptions{"ApplyOutOfSyncOnly=true"}
	h.in.DiffResults = map[ResourceKey]bool{
		{Kind: "ConfigMap", Namespace: testNS, Name: "same"}:    false,
		{Kind: "ConfigMap", Namespace: testNS, Name: "changed"}: true,
	}
	res := h.run(5)
	assert.Equal(t, v1alpha1.OperationSucceeded, res.Phase)
	assert.ElementsMatch(t, []string{"changed", "new"}, names(res.Results))
	assert.Empty(t, h.kube.callsWith("dryrun apply same"))
}

func TestStepSelectiveSyncSkipsHooks(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cm("a"), cm("b"), hookObj(pod("hook"), "PreSync"))
	h.in.Resources = []v1alpha1.SyncOperationResource{{Kind: "ConfigMap", Name: "a"}}
	res := h.run(5)
	assert.Equal(t, v1alpha1.OperationSucceeded, res.Phase)
	assert.Equal(t, []string{"a"}, names(res.Results))
}

func TestStepValidateFalse(t *testing.T) {
	t.Parallel()
	k := &validateRecorder{fakeKube: newFakeKube()}
	e, err := NewEngine(k, nil, Config{})
	require.NoError(t, err)
	in := &Input{OperationUID: "u", App: AppRef{Name: "app", Destination: v1alpha1.ApplicationDestination{Namespace: testNS}},
		Targets: []*unstructured.Unstructured{cm("strict"), annotate(cm("lenient"), v1alpha1.AnnotationKeySyncOptions, "Validate=false")}}
	_, err = e.Step(context.Background(), in)
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"strict": true, "lenient": false}, k.validate)

	in.SyncOptions = v1alpha1.SyncOptions{"Validate=false"}
	k.validate = nil
	_, err = e.Step(context.Background(), in)
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"strict": false, "lenient": false}, k.validate)
}

type validateRecorder struct {
	*fakeKube
	validate map[string]bool
}

func (v *validateRecorder) ClientSideApply(ctx context.Context, obj *unstructured.Unstructured, o ApplyOptions) (string, error) {
	v.mu.Lock()
	if v.validate == nil {
		v.validate = map[string]bool{}
	}
	v.validate[obj.GetName()] = o.Validate
	v.mu.Unlock()
	return v.fakeKube.ClientSideApply(ctx, obj, o)
}

func TestRunningMessage(t *testing.T) {
	t.Parallel()
	podTask := func(name string) *task { return &task{target: pod(name)} }
	hookTask := func(name string) *task { return &task{target: hookObj(pod(name), "Sync"), hook: true} }
	opts := ParseOptions(nil)
	tests := []struct {
		name     string
		tasks    syncTasks
		deletion bool
		want     string
	}{
		{"empty", nil, false, ""},
		{"single resource", syncTasks{podTask("my-pod")}, false, "waiting for healthy state of /Pod/my-pod"},
		{"multiple resources", syncTasks{podTask("pod-1"), podTask("pod-2"), podTask("pod-3")}, false, "waiting for healthy state of /Pod/pod-1 and 2 more resources"},
		{"single hook", syncTasks{hookTask("hook-1")}, false, "waiting for completion of hook /Pod/hook-1"},
		{"multiple hooks", syncTasks{hookTask("hook-1"), hookTask("hook-2")}, false, "waiting for completion of hook /Pod/hook-1 and 1 more hooks"},
		{"hooks first", syncTasks{podTask("pod-1"), hookTask("hook-1"), podTask("pod-2")}, false, "waiting for completion of hook /Pod/hook-1 and 2 more resources"},
		{"resource deletion", syncTasks{podTask("my-pod")}, true, "waiting for deletion of /Pod/my-pod"},
		{"resources deletion", syncTasks{podTask("pod-1"), podTask("pod-2"), podTask("pod-3")}, true, "waiting for deletion of /Pod/pod-1 and 2 more resources"},
		{"hook deletion", syncTasks{hookTask("hook-1")}, true, "waiting for deletion of hook /Pod/hook-1"},
		{"hooks deletion", syncTasks{hookTask("hook-1"), hookTask("hook-2")}, true, "waiting for deletion of hook /Pod/hook-1 and 1 more hooks"},
		{"mixed deletion", syncTasks{podTask("pod-1"), hookTask("hook-1"), podTask("pod-2")}, true, "waiting for deletion of hook /Pod/hook-1 and 2 more resources"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, runningMessage(tt.tasks, tt.deletion, true, opts))
		})
	}
}

func TestExecuteSyncFailPhaseMessages(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.engine.newRun(context.Background(), &h.in)
	assert.True(t, r.executeSyncFailPhase(nil, syncTasks{{message: "namespace not found"}}, "one or more objects failed to apply"))
	assert.Equal(t, "one or more objects failed to apply, reason: namespace not found", r.message)

	assert.True(t, r.executeSyncFailPhase(nil, syncTasks{{message: "b"}, {message: "a"}, {message: "b"}}, "x"))
	assert.Equal(t, "x, reason: a,b", r.message, "messages are de-duplicated")

	assert.True(t, r.executeSyncFailPhase(nil, nil, "x"))
	assert.Equal(t, "x", r.message)
}

func TestStepDryRunSkipsStateReplacedBySync(t *testing.T) {
	t.Parallel()
	t.Run("hook re-created by BeforeHookCreation", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, hookObj(pod("migrate"), "PreSync"), hookObj(pod("patched"), "PreSync", "HookSucceeded"), pod("app"))
		h.kube.put(annotate(hookObj(pod("migrate"), "PreSync"), AnnotationKeyOperation, "old-op/0/PreSync"),
			annotate(hookObj(pod("patched"), "PreSync", "HookSucceeded"), AnnotationKeyOperation, "old-op/0/PreSync"))
		h.kube.dryRunErr["migrate"] = errors.New(`Pod "migrate" is invalid: spec: Forbidden: pod updates may not change fields`)
		res := h.step()
		assert.Equal(t, v1alpha1.OperationRunning, res.Phase, res.Message)
		assert.Equal(t, "waiting for deletion of hook /Pod/migrate", res.Message)
		assert.ElementsMatch(t, []string{"dryrun apply patched", "dryrun apply app"}, h.kube.callsWith("dryrun"),
			"the hook deleted before its creation is not dry-run against the old object")

		h.kube.dryRunErr["patched"] = errors.New("invalid patch")
		h.in.Previous, h.in.Progress = nil, nil
		h.kube.put(annotate(hookObj(pod("migrate"), "PreSync"), AnnotationKeyOperation, "old-op/0/PreSync"))
		res = h.step()
		assert.Equal(t, v1alpha1.OperationFailed, res.Phase)
		assert.Equal(t, "one or more objects failed to apply (dry run)", res.Message)
	})
	t.Run("dry-run operation", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, hookObj(pod("migrate"), "PreSync"))
		h.kube.put(hookObj(pod("migrate"), "PreSync"))
		h.in.DryRun = true
		h.kube.dryRunErr["migrate"] = errors.New("immutable")
		res := h.run(5)
		assert.Equal(t, v1alpha1.OperationSucceeded, res.Phase, res.Message)
		assert.Equal(t, "pod/migrate created (dry run)", findResult(res.Results, "migrate", "").Message)
		assert.Empty(t, h.kube.callsWith("delete"))
	})
	t.Run("custom resource of a CRD changed by the sync", func(t *testing.T) {
		t.Parallel()
		widgets := crd("example.com", "Widget", "widgets", "Namespaced")
		h := newHarness(t, widgets, newObj("example.com/v1", "Widget", "", "w"))
		h.kube.put(widgets)
		h.kube.dryRunErr["w"] = errors.New(`strict decoding error: unknown field "spec.color"`)
		p, err := h.engine.Plan(context.Background(), withLive(h))
		require.NoError(t, err)
		assert.True(t, plannedTask(p, "w").SkipDryRun)
		res := h.run(5)
		assert.Equal(t, v1alpha1.OperationSucceeded, res.Phase, res.Message)
		assert.NotContains(t, h.kube.callsWith("dryrun"), "dryrun apply w")
	})
	t.Run("custom resource of an unchanged CRD", func(t *testing.T) {
		t.Parallel()
		widgets := crd("example.com", "Widget", "widgets", "Namespaced")
		h := newHarness(t, widgets, newObj("example.com/v1", "Widget", "", "w"))
		h.kube.put(widgets)
		h.in.DiffResults = map[ResourceKey]bool{{Group: groupCRD, Kind: kindCRD, Name: "widgets.example.com"}: false}
		h.kube.dryRunErr["w"] = errors.New(`strict decoding error: unknown field "spec.color"`)
		res := h.run(5)
		assert.Equal(t, v1alpha1.OperationFailed, res.Phase)
		assert.Equal(t, "one or more objects failed to apply (dry run)", res.Message)
		assert.Empty(t, h.kube.callsWith("apply"))
	})
}

func withLive(h *harness) *Input {
	in := h.in
	in.Live = h.kube.live(in.Targets)
	return &in
}

func plannedTask(p *Plan, name string) PlannedTask {
	for _, t := range p.Tasks {
		if t.Key.Name == name {
			return t
		}
	}
	return PlannedTask{}
}

func TestStepClientSideApplyMigrationWithoutManagedFields(t *testing.T) {
	t.Parallel()
	live := cm("csa")
	live.SetManagedFields([]metav1.ManagedFieldsEntry{{Manager: DefaultClientSideApplyMigrationManager, Operation: metav1.ManagedFieldsOperationUpdate}})
	h := newHarness(t, cm("csa"))
	h.kube.put(live)
	h.in.SyncOptions = v1alpha1.SyncOptions{"ServerSideApply=true"}
	// Live state from the cluster cache without managedFields (RetainManagedFields=false).
	in := withLive(h)
	for _, obj := range in.Live {
		obj.SetManagedFields(nil)
	}
	res, err := h.engine.Step(context.Background(), in)
	require.NoError(t, err)
	assert.Equal(t, v1alpha1.OperationSucceeded, res.Phase, res.Message)
	assert.Equal(t, []string{"migrate csa kubectl-client-side-apply"}, h.kube.callsWith("migrate"))

	// Live state with managedFields that need no migration: no call.
	h2 := newHarness(t, cm("ssa"))
	ssaLive := cm("ssa")
	ssaLive.SetManagedFields([]metav1.ManagedFieldsEntry{{Manager: DefaultManager, Operation: metav1.ManagedFieldsOperationApply}})
	h2.kube.put(ssaLive)
	h2.in.SyncOptions = v1alpha1.SyncOptions{"ServerSideApply=true"}
	h2.run(5)
	assert.Empty(t, h2.kube.callsWith("migrate"))
}

func TestStepTransientErrorDuringDryRunRepeatsDryRun(t *testing.T) {
	t.Parallel()
	h := newHarness(t, pod("good"), wave(pod("bad"), 1))
	h.in.SyncOptions = v1alpha1.SyncOptions{"CreateNamespace=true"}
	h.kube.dryRunErr["bad"] = apierrors.NewServiceUnavailable("etcd leader changed")
	res, err := h.engine.Step(context.Background(), withLive(h))
	require.ErrorIs(t, err, ErrTransient)
	assert.Equal(t, v1alpha1.OperationRunning, res.Phase)
	assert.Empty(t, res.Results, "nothing of an unfinished dry run is reported")
	h.persist(res) // the reconciler may persist the partial result

	h.kube.dryRunErr["bad"] = errors.New("invalid object")
	h.kube.applyErr["bad"] = errors.New("invalid object")
	res = h.run(5)
	assert.Equal(t, v1alpha1.OperationFailed, res.Phase)
	assert.Equal(t, "one or more objects failed to apply (dry run)", res.Message)
	assert.Empty(t, h.kube.callsWith("apply dest-ns/"), "nothing applied")
}

func TestStepPendingWaveRequeuesWithPollInterval(t *testing.T) {
	t.Parallel()
	t.Run("prune confirmation", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, cm("a"), wave(cm("b"), 1))
		h.kube.put(annotate(cm("old"), v1alpha1.AnnotationKeySyncOptions, "Prune=confirm"))
		for range 3 {
			res := h.step()
			require.Equal(t, v1alpha1.OperationRunning, res.Phase)
			assert.Equal(t, "waiting for pruning confirmation of /ConfigMap/old", res.Message)
			assert.Equal(t, time.Second, res.RequeueAfter, "no hot loop with a zero wave delay")
		}
	})
	t.Run("hook deletion", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, hookObj(pod("hook"), "PreSync"), pod("app"))
		h.kube.put(annotate(hookObj(pod("hook"), "PreSync"), AnnotationKeyOperation, "old-op/0/PreSync"))
		h.kube.mu.Lock()
		h.kube.objs[ResourceKey{Kind: "Pod", Namespace: testNS, Name: "hook"}].SetFinalizers([]string{"example.com/slow"})
		h.kube.mu.Unlock()
		for range 3 {
			res := h.step()
			require.Equal(t, v1alpha1.OperationRunning, res.Phase)
			assert.Equal(t, "waiting for deletion of hook /Pod/hook", res.Message)
			assert.Equal(t, time.Second, res.RequeueAfter)
		}
		assert.Len(t, h.kube.callsWith("delete"), 1, "a terminating hook is not deleted again")
	})
	t.Run("successful non-final wave uses the wave delay", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, cm("a"), wave(cm("b"), 1))
		res := h.step()
		assert.Equal(t, v1alpha1.OperationRunning, res.Phase)
		assert.Zero(t, res.RequeueAfter, "progress was made; no wave delay configured")
	})
}

func TestStepReplaceForceIsIdempotentWithinAnAttempt(t *testing.T) {
	t.Parallel()
	target := annotate(cm("job"), v1alpha1.AnnotationKeySyncOptions, "Replace=true,Force=true")
	h := newHarness(t, target)
	h.kube.put(cm("job"))
	staleLive := h.kube.live(h.in.Targets)

	for range 2 { // the first result is lost: the step runs again
		in := h.in
		in.Live = staleLive // the cache may still hold the object before the replacement
		res, err := h.engine.Step(context.Background(), &in)
		require.NoError(t, err)
		assert.Equal(t, v1alpha1.OperationSucceeded, res.Phase, res.Message)
		assert.Equal(t, "configmap/job replaced", findResult(res.Results, "job", "").Message)
	}
	assert.Equal(t, []string{"force-replace dest-ns/job"}, h.kube.callsWith("force-replace"))
	live, err := h.kube.Get(context.Background(), gvkConfigMap, testNS, "job")
	require.NoError(t, err)
	assert.Equal(t, "op-uid-1/0/Sync", live.GetAnnotations()[AnnotationKeyOperation])

	h.in.Attempt = 1
	h.run(5)
	assert.Len(t, h.kube.callsWith("force-replace"), 2, "a retry attempt replaces again")

	t.Run("create", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, annotate(cm("new"), v1alpha1.AnnotationKeySyncOptions, "Replace=true"))
		for range 2 { // the first result is lost and the cache has not seen the object yet
			in := h.in
			in.Live = map[ResourceKey]*unstructured.Unstructured{}
			res, err := h.engine.Step(context.Background(), &in)
			require.NoError(t, err)
			assert.Equal(t, v1alpha1.OperationSucceeded, res.Phase, res.Message)
			assert.Equal(t, "configmap/new created", findResult(res.Results, "new", "").Message)
		}
		assert.Equal(t, []string{"create dest-ns/new"}, h.kube.callsWith("create"))

		// Someone else's object with that name is not taken over.
		h2 := newHarness(t, annotate(cm("taken"), v1alpha1.AnnotationKeySyncOptions, "Replace=true"))
		h2.kube.putUntracked(cm("taken"))
		in := h2.in
		in.Live = map[ResourceKey]*unstructured.Unstructured{}
		res, err := h2.engine.Step(context.Background(), &in)
		require.NoError(t, err)
		assert.Equal(t, v1alpha1.OperationFailed, res.Phase)
		assert.Contains(t, findResult(res.Results, "taken", "").Message, "already exists")
	})
}
