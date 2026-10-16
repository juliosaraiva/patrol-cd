package integration

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/controller/app"
)

func automated(prune, selfHeal bool) func(*v1alpha1.Application) {
	return func(a *v1alpha1.Application) {
		a.Spec.SyncPolicy = &v1alpha1.SyncPolicy{Automated: &v1alpha1.SyncPolicyAutomated{Prune: ptr.To(prune), SelfHeal: ptr.To(selfHeal)}}
	}
}

// TestAutomatedSyncOnNewRevisionAndPrune: automated sync creates a deterministic automated
// Operation for the compared revision; a new commit (refresh requested as a webhook would)
// is synced by a second automated Operation, which prunes the removed resource.
func TestAutomatedSyncOnNewRevisionAndPrune(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctl := e.startController()
	rev1 := e.repo.commit(e.repoURL, "auto", "main",
		configMap("auto-a", map[string]string{"k": "1"}, nil), configMap("auto-b", map[string]string{"k": "1"}, nil))
	a := e.newApp("auto", automated(true, false))
	e.create(a)

	got := e.waitApp(a, "auto-synced to rev1", func(a *v1alpha1.Application) bool {
		return synced(rev1)(a) && a.Status.OperationState != nil && a.Status.OperationState.Phase == v1alpha1.OperationSucceeded && len(a.Status.History) == 1
	})
	ops := e.operations(a)
	require.Len(t, ops, 1)
	first := ops[0]
	assert.True(t, first.Spec.InitiatedBy.Automated)
	// The first decision saw no summarized operation.
	before := got.DeepCopy()
	before.Status.OperationState = nil
	assert.Equal(t, app.AutomatedOperationName(before, []string{rev1}, 0), first.Name)
	assert.True(t, strings.HasPrefix(first.Name, a.Name+"-auto-"))
	assert.Equal(t, rev1, first.Spec.Sync.Revision)
	assert.True(t, first.Spec.Sync.Prune)
	require.NotNil(t, first.Spec.Retry)
	assert.Equal(t, int64(5), first.Spec.Retry.Limit, "automated syncs retry 5 times by default")
	assert.True(t, ctl.events.has("OperationStarted: Initiated automated sync to '"+rev1+"'"))

	rev2 := e.repo.commit(e.repoURL, "auto", "main", configMap("auto-a", map[string]string{"k": "2"}, nil))
	e.refresh(a, v1alpha1.RefreshTypeNormal)
	got = e.waitApp(a, "auto-synced to rev2 with auto-b pruned", func(a *v1alpha1.Application) bool {
		return synced(rev2)(a) && len(a.Status.History) == 2 && len(a.Status.Resources) == 1
	})
	assert.Empty(t, got.Annotations[v1alpha1.AnnotationKeyRefresh], "refresh annotation removed")
	e.waitLive(gvrConfigMap, "auto-b", "pruned", gone)
	cm, ok := e.live(gvrConfigMap, "auto-a")
	require.True(t, ok)
	v, _, _ := unstructured.NestedString(cm.Object, "data", "k")
	assert.Equal(t, "2", v)
	assert.Len(t, e.operations(a), 2)
	assert.Nil(t, hasCondition(got, v1alpha1.ApplicationConditionSyncError))
}

// TestAutomatedSyncWithoutPruneSkipsPruneOnly: without automated.prune a change that only
// requires pruning creates no Operation and the app stays OutOfSync.
func TestAutomatedSyncWithoutPruneSkipsPruneOnly(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.startController()
	rev1 := e.repo.commit(e.repoURL, "noprune", "main",
		configMap("noprune-a", nil, nil), configMap("noprune-b", nil, nil))
	a := e.newApp("noprune", automated(false, false))
	e.create(a)
	e.waitApp(a, "synced", synced(rev1))
	e.repo.commit(e.repoURL, "noprune", "main", configMap("noprune-a", nil, nil))
	e.refresh(a, v1alpha1.RefreshTypeNormal)
	got := e.waitApp(a, "OutOfSync with a resource requiring pruning", func(a *v1alpha1.Application) bool {
		if !outOfSync(a) {
			return false
		}
		for _, r := range a.Status.Resources {
			if r.Name == "noprune-b" && r.RequiresPruning {
				return true
			}
		}
		return false
	})
	time.Sleep(time.Second)
	assert.Len(t, e.operations(a), 1, "no operation for a prune-only change")
	assert.True(t, outOfSync(e.getApp(got)))
	_, ok := e.live(gvrConfigMap, "noprune-b")
	assert.True(t, ok)
}

// TestSelfHeal: drift of a managed resource flips the app OutOfSync through a live-state
// event; with selfHeal a partial automated Operation reverts it without a history entry;
// without selfHeal nothing happens.
func TestSelfHeal(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.startController()
	rev := e.repo.commit(e.repoURL, "heal", "main", configMap("heal-cm", map[string]string{"k": "desired"}, nil))
	e.repo.commit(e.repoURL, "noheal", "main", configMap("noheal-cm", map[string]string{"k": "desired"}, nil))
	heal := e.newApp("heal", automated(false, true))
	noheal := e.newApp("noheal", automated(false, false))
	e.create(heal)
	e.create(noheal)
	e.waitApp(heal, "synced", func(a *v1alpha1.Application) bool { return synced(rev)(a) && len(a.Status.History) == 1 })
	e.waitApp(noheal, "synced", func(a *v1alpha1.Application) bool { return synced("")(a) && len(a.Status.History) == 1 })

	for _, name := range []string{"heal-cm", "noheal-cm"} {
		e.mutateLive(gvrConfigMap, name, func(u *unstructured.Unstructured) {
			require.NoError(t, unstructured.SetNestedField(u.Object, "drifted", "data", "k"))
		})
	}
	e.waitApp(noheal, "OutOfSync after drift", outOfSync)
	e.waitLive(gvrConfigMap, "heal-cm", "self-healed", func(u *unstructured.Unstructured, ok bool) bool {
		v, _, _ := unstructured.NestedString(u.Object, "data", "k")
		return ok && v == "desired"
	})
	got := e.waitApp(heal, "Synced again", synced(rev))
	assert.Len(t, got.Status.History, 1, "self-heal is a partial sync and records no history")
	ops := e.operations(heal)
	require.Len(t, ops, 2)
	selfHeal := ops[0]
	if ops[1].Spec.Sync.SelfHealAttemptsCount > 0 {
		selfHeal = ops[1]
	}
	assert.Equal(t, int64(1), selfHeal.Spec.Sync.SelfHealAttemptsCount)
	require.Len(t, selfHeal.Spec.Sync.Resources, 1)
	assert.Equal(t, "heal-cm", selfHeal.Spec.Sync.Resources[0].Name)

	// A second drift is healed after the self-heal backoff (2s for the second attempt).
	e.waitOps(heal, "first self-heal finished", func(ops []v1alpha1.Operation) bool {
		return len(ops) == 2 && ops[0].Status.Phase.IsTerminal() && ops[1].Status.Phase.IsTerminal()
	})
	e.mutateLive(gvrConfigMap, "heal-cm", func(u *unstructured.Unstructured) {
		require.NoError(t, unstructured.SetNestedField(u.Object, "drifted-again", "data", "k"))
	})
	e.waitLive(gvrConfigMap, "heal-cm", "healed again", func(u *unstructured.Unstructured, ok bool) bool {
		v, _, _ := unstructured.NestedString(u.Object, "data", "k")
		return ok && v == "desired"
	})
	ops = e.waitOps(heal, "a second self-heal operation", func(ops []v1alpha1.Operation) bool { return len(ops) == 3 })
	var attempts []int64
	for _, op := range ops {
		attempts = append(attempts, op.Spec.Sync.SelfHealAttemptsCount)
	}
	assert.ElementsMatch(t, []int64{0, 1, 2}, attempts)

	time.Sleep(time.Second)
	assert.Len(t, e.operations(noheal), 1, "no self-heal without selfHeal")
	v, _, _ := unstructured.NestedString(mustLive(t, e, "noheal-cm").Object, "data", "k")
	assert.Equal(t, "drifted", v)
}

// TestSelfHealAfterManualSync: a manual sync between two drifts resets the self-heal attempt
// counter (upstream reads it from the last operation); the next heal is attempt 1 again and
// must not be mistaken for the earlier attempt-1 heal of the same revision.
func TestSelfHealAfterManualSync(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.startController()
	rev := e.repo.commit(e.repoURL, "rheal", "main", configMap("rheal-cm", map[string]string{"k": "desired"}, nil))
	a := e.newApp("rheal", automated(false, true))
	e.create(a)
	e.waitApp(a, "auto-synced", func(a *v1alpha1.Application) bool { return synced(rev)(a) && len(a.Status.History) == 1 })
	drift := func(v string) {
		e.mutateLive(gvrConfigMap, "rheal-cm", func(u *unstructured.Unstructured) {
			require.NoError(t, unstructured.SetNestedField(u.Object, v, "data", "k"))
		})
	}
	healed := func(u *unstructured.Unstructured, ok bool) bool {
		v, _, _ := unstructured.NestedString(u.Object, "data", "k")
		return ok && v == "desired"
	}
	allTerminal := func(n int) func([]v1alpha1.Operation) bool {
		return func(ops []v1alpha1.Operation) bool {
			if len(ops) != n {
				return false
			}
			for i := range ops {
				if !ops[i].Status.Phase.IsTerminal() {
					return false
				}
			}
			return true
		}
	}

	drift("drifted")
	e.waitLive(gvrConfigMap, "rheal-cm", "first self-heal", healed)
	ops := e.waitOps(a, "auto sync and first heal finished", allTerminal(2))
	var firstHeal string
	for _, op := range ops {
		if op.Spec.Sync.SelfHealAttemptsCount == 1 {
			firstHeal = op.Name
		}
	}
	require.NotEmpty(t, firstHeal)

	manual := e.sync(a)
	e.waitOp(manual, "manual sync succeeded", phaseIs(v1alpha1.OperationSucceeded))
	e.waitApp(a, "manual sync summarized", func(a *v1alpha1.Application) bool {
		return synced(rev)(a) && a.Status.OperationState != nil && a.Status.OperationState.OperationName == manual.Name &&
			a.Status.OperationState.Phase == v1alpha1.OperationSucceeded
	})

	drift("drifted-again")
	e.waitLive(gvrConfigMap, "rheal-cm", "healed after the manual sync", healed)
	ops = e.waitOps(a, "a new self-heal operation", allTerminal(4))
	var heals []string
	for _, op := range ops {
		if op.Spec.Sync.SelfHealAttemptsCount == 1 {
			heals = append(heals, op.Name)
		}
	}
	assert.Len(t, heals, 2, "both heals are attempt 1 (the manual sync reset the counter)")
	assert.Contains(t, heals, firstHeal)
	got := e.waitApp(a, "Synced", synced(rev))
	assert.Nil(t, hasCondition(got, v1alpha1.ApplicationConditionSyncError))
}

// TestAutoSyncBackAfterManualSync: after a manual sync to a revision of another branch, the
// automated sync returns the Application to its target revision with a new Operation, even
// though an earlier automated Operation synced that same revision (SYNC-019/021).
func TestAutoSyncBackAfterManualSync(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.startController()
	rev1 := e.repo.commit(e.repoURL, "back", "main", configMap("back-cm", map[string]string{"k": "main"}, nil))
	a := e.newApp("back", automated(true, false))
	e.create(a)
	e.waitApp(a, "auto-synced to rev1", func(a *v1alpha1.Application) bool {
		return synced(rev1)(a) && a.Status.OperationState != nil && a.Status.OperationState.Phase == v1alpha1.OperationSucceeded
	})
	first := e.operations(a)
	require.Len(t, first, 1)

	other := e.repo.commit(e.repoURL, "back", "feature", configMap("back-cm", map[string]string{"k": "feature"}, nil))
	manual := e.sync(a, func(op *v1alpha1.Operation) { op.Spec.Sync.Revision = other })
	done := e.waitOp(manual, "manual sync to the feature revision", phaseIs(v1alpha1.OperationSucceeded))
	assert.Equal(t, other, done.Status.SyncResult.Revision)

	got := e.waitApp(a, "automated sync back to rev1", func(a *v1alpha1.Application) bool {
		os := a.Status.OperationState
		return synced(rev1)(a) && os != nil && os.InitiatedBy.Automated && os.OperationName != first[0].Name &&
			os.Phase == v1alpha1.OperationSucceeded
	})
	assert.Nil(t, hasCondition(got, v1alpha1.ApplicationConditionSyncError))
	v, _, _ := unstructured.NestedString(mustLive(t, e, "back-cm").Object, "data", "k")
	assert.Equal(t, "main", v)
	assert.Len(t, e.operations(a), 3)
}

func mustLive(t *testing.T, e *env, name string) *unstructured.Unstructured {
	t.Helper()
	obj, ok := e.live(gvrConfigMap, name)
	require.True(t, ok)
	return obj
}

// TestFailedSyncRetryBackoff: a sync that fails is retried with the backoff of its retry
// strategy and ends Failed with "(retried n times)."; an automated sync of the same revision
// is not attempted again and reports SyncError; a fixed commit is synced.
func TestFailedSyncRetryBackoff(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.startController()
	bad := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Service", "metadata": map[string]any{"name": "bad-svc"},
		"spec": map[string]any{"type": "NotAType", "ports": []any{map[string]any{"port": int64(80)}}},
	}}
	rev := e.repo.commit(e.repoURL, "bad", "main", bad)
	a := e.newApp("bad", automated(false, false), func(a *v1alpha1.Application) {
		a.Spec.SyncPolicy.Retry = &v1alpha1.RetryStrategy{Limit: 2, Backoff: &v1alpha1.Backoff{Duration: "1s", Factor: ptr.To[int64](1), MaxDuration: "2s"}}
	})
	e.create(a)
	ops := e.waitOps(a, "one operation", func(ops []v1alpha1.Operation) bool { return len(ops) == 1 })
	op := &ops[0]
	retrying := e.waitOp(op, "waiting for a retry", func(op *v1alpha1.Operation) bool {
		return op.Status.Phase == v1alpha1.OperationRunning && op.Status.FinishedAt != nil && op.Status.RetryCount >= 1
	})
	assert.Contains(t, retrying.Status.Message, "Retrying attempt #")
	assert.True(t, metaCondition(retrying.Status.Conditions, v1alpha1.OperationConditionRetrying, metav1.ConditionTrue))
	failed := e.waitOp(op, "Failed after retries", phaseIs(v1alpha1.OperationFailed))
	assert.Equal(t, int64(2), failed.Status.RetryCount)
	assert.True(t, strings.HasSuffix(failed.Status.Message, "(retried 2 times)."), failed.Status.Message)
	assert.Contains(t, failed.Status.Message, "one or more objects failed to apply")

	got := e.waitApp(a, "SyncError for the failed revision", func(a *v1alpha1.Application) bool {
		c := hasCondition(a, v1alpha1.ApplicationConditionSyncError)
		return c != nil && strings.HasPrefix(c.Message, "Failed last sync attempt to ["+rev+"]")
	})
	assert.Empty(t, got.Status.History)
	time.Sleep(time.Second)
	assert.Len(t, e.operations(a), 1, "the failed revision is not synced again")

	fixed := e.repo.commit(e.repoURL, "bad", "main", configMap("bad-fixed", nil, nil))
	e.refresh(a, v1alpha1.RefreshTypeNormal)
	got = e.waitApp(a, "fixed revision synced", func(a *v1alpha1.Application) bool {
		return a.Status.OperationState != nil && a.Status.OperationState.Phase == v1alpha1.OperationSucceeded &&
			a.Status.OperationState.SyncResult != nil && a.Status.OperationState.SyncResult.Revision == fixed
	})
	e.waitApp(got, "SyncError cleared", func(a *v1alpha1.Application) bool {
		return hasCondition(a, v1alpha1.ApplicationConditionSyncError) == nil
	})
}
