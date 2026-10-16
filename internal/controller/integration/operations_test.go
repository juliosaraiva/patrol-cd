package integration

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/controller/core"
	"github.com/juliosaraiva/patrol-cd/internal/controller/operation"
)

func cancelOp(e *env, op *v1alpha1.Operation) {
	e.t.Helper()
	require.NoError(e.t, e.c.Patch(e.ctx, op.DeepCopy(), client.RawPatch(types.MergePatchType, []byte(`{"spec":{"cancel":true}}`))))
}

// TestTerminateAndOperationSlot: while an Operation waits for a PreSync hook, a second
// Operation is Rejected (OperationInProgress); cancelling the first terminates it
// (Cancelled, running hook deleted) without a history entry.
func TestTerminateAndOperationSlot(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.startController()
	e.repo.commit(e.repoURL, "term", "main", hookPod("term-pre-", v1alpha1.HookTypePreSync), configMap("term-cm", nil, nil))
	a := e.newApp("term")
	e.create(a)
	e.waitApp(a, "compared", outOfSync)

	op := e.sync(a)
	running := e.waitOp(op, "waiting for the hook", func(op *v1alpha1.Operation) bool {
		return op.Status.Phase == v1alpha1.OperationRunning && strings.Contains(op.Status.Message, "waiting for completion of hook")
	})
	require.Len(t, e.podsWithPrefix("term-pre-"), 1)
	assert.NotNil(t, running.Status.Progress)

	second := e.sync(a)
	rejected := e.waitOp(second, "rejected", phaseIs(v1alpha1.OperationRejected))
	assert.Equal(t, "another operation is already in progress", rejected.Status.Message)
	assert.True(t, metaCondition(rejected.Status.Conditions, v1alpha1.OperationConditionAdmitted, metav1.ConditionFalse))

	cancelOp(e, op)
	cancelled := e.waitOp(op, "Cancelled", phaseIs(v1alpha1.OperationCancelled))
	assert.Equal(t, "Operation terminated", cancelled.Status.Message)
	assert.Eventually(t, func() bool { return len(e.podsWithPrefix("term-pre-")) == 0 }, waitTimeout, pollEvery, "running hook deleted")
	got := e.waitApp(a, "summary shows the cancellation", func(a *v1alpha1.Application) bool {
		return a.Status.OperationState != nil && a.Status.OperationState.OperationName == op.Name && a.Status.OperationState.Phase == v1alpha1.OperationCancelled
	})
	assert.Empty(t, got.Status.History)
}

// TestSyncTimeoutTerminates: controller.sync.timeout.seconds terminates a Running operation
// with the timeout cause in its message (SYNC-079).
func TestSyncTimeoutTerminates(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.startController(withParams(func(p *core.Params) { p.SyncTimeout = 3 * time.Second }))
	e.repo.commit(e.repoURL, "slow", "main", hookPod("slow-pre-", v1alpha1.HookTypePreSync))
	a := e.newApp("slow")
	e.create(a)
	e.waitApp(a, "compared", func(a *v1alpha1.Application) bool { return a.Status.ReconciledAt != nil })
	op := e.sync(a)
	done := e.waitOp(op, "terminated by the timeout", func(op *v1alpha1.Operation) bool { return op.Status.Phase.IsTerminal() })
	assert.Equal(t, v1alpha1.OperationCancelled, done.Status.Phase)
	assert.Equal(t, "Operation terminated, triggered by controller sync timeout", done.Status.Message)
}

// TestRollback: two syncs record history ids 0 and 1; a rollback to id 0 restores the first
// revision with the apply strategy and records a third entry; rollbacks are rejected for an
// unknown id and while automated sync is enabled.
func TestRollback(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.startController()
	rev1 := e.repo.commit(e.repoURL, "rb", "main", configMap("rb-cm", map[string]string{"v": "1"}, nil))
	a := e.newApp("rb")
	e.create(a)
	e.waitApp(a, "compared", outOfSync)
	e.waitOp(e.sync(a), "first sync", phaseIs(v1alpha1.OperationSucceeded))
	rev2 := e.repo.commit(e.repoURL, "rb", "main", configMap("rb-cm", map[string]string{"v": "2"}, nil))
	e.refresh(a, v1alpha1.RefreshTypeNormal)
	e.waitApp(a, "rev2 compared", func(a *v1alpha1.Application) bool { return outOfSync(a) && a.Status.Sync.Revision == rev2 })
	e.waitOp(e.sync(a), "second sync", phaseIs(v1alpha1.OperationSucceeded))
	got := e.waitApp(a, "two history entries", func(a *v1alpha1.Application) bool { return len(a.Status.History) == 2 })
	assert.Equal(t, []int64{0, 1}, []int64{got.Status.History[0].ID, got.Status.History[1].ID})
	assert.Equal(t, rev1, got.Status.History[0].Revision)

	unknown := e.sync(a, func(op *v1alpha1.Operation) {
		op.Spec.Sync = nil
		op.Spec.Rollback = &v1alpha1.RollbackOperation{ID: 42}
	})
	rejected := e.waitOp(unknown, "rejected", phaseIs(v1alpha1.OperationRejected))
	assert.Equal(t, fmt.Sprintf("application %s/%s does not have deployment with id 42", a.Namespace, a.Name), rejected.Status.Message)

	rb := e.sync(a, func(op *v1alpha1.Operation) {
		op.Spec.Sync = nil
		op.Spec.Rollback = &v1alpha1.RollbackOperation{ID: 0}
	})
	done := e.waitOp(rb, "rollback succeeded", phaseIs(v1alpha1.OperationSucceeded))
	assert.Equal(t, rev1, done.Status.SyncResult.Revision)
	e.waitLive(gvrConfigMap, "rb-cm", "back to v1", func(u *unstructured.Unstructured, ok bool) bool {
		v, _, _ := unstructured.NestedString(u.Object, "data", "v")
		return ok && v == "1"
	})
	got = e.waitApp(a, "three history entries, OutOfSync with the spec", func(a *v1alpha1.Application) bool {
		return len(a.Status.History) == 3 && outOfSync(a)
	})
	assert.Equal(t, int64(2), got.Status.History[2].ID)
	assert.Equal(t, rev1, got.Status.History[2].Revision)

	enableAuto := `{"spec":{"syncPolicy":{"automated":{"enabled":false}}}}`
	require.NoError(t, e.c.Patch(e.ctx, got.DeepCopy(), client.RawPatch(types.MergePatchType, []byte(enableAuto))))
	// Automated sync configured but disabled still allows rollbacks; enabled does not.
	require.NoError(t, e.c.Patch(e.ctx, got.DeepCopy(), client.RawPatch(types.MergePatchType, []byte(`{"spec":{"syncPolicy":{"automated":{"enabled":true}}}}`))))
	e.waitApp(a, "auto-synced back to rev2", synced(rev2))
	blocked := e.sync(a, func(op *v1alpha1.Operation) {
		op.Spec.Sync = nil
		op.Spec.Rollback = &v1alpha1.RollbackOperation{ID: 0}
	})
	rejected = e.waitOp(blocked, "rejected", phaseIs(v1alpha1.OperationRejected))
	assert.Equal(t, "rollback cannot be initiated when auto-sync is enabled", rejected.Status.Message)
	assert.True(t, metaCondition(rejected.Status.Conditions, v1alpha1.OperationConditionAdmitted, metav1.ConditionFalse))
	assert.Equal(t, operation.ReasonRollbackNotAllowed, rejected.Status.Conditions[0].Reason)
}

// TestHistoryLimitAndRetention: revisionHistoryLimit bounds status.history (ids keep
// increasing) and the terminal Operations kept for the Application.
func TestHistoryLimitAndRetention(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.startController()
	a := e.newApp("hist", func(a *v1alpha1.Application) { a.Spec.RevisionHistoryLimit = ptr.To[int64](2) })
	var revs []string
	for i := range 3 {
		revs = append(revs, e.repo.commit(e.repoURL, "hist", "main", configMap("hist-cm", map[string]string{"i": fmt.Sprint(i)}, nil)))
		if i == 0 {
			e.create(a)
		} else {
			e.refresh(a, v1alpha1.RefreshTypeNormal)
		}
		rev := revs[i]
		e.waitApp(a, "new revision compared", func(a *v1alpha1.Application) bool { return a.Status.Sync.Revision == rev && outOfSync(a) })
		e.waitOp(e.sync(a), "sync", phaseIs(v1alpha1.OperationSucceeded))
	}
	got := e.waitApp(a, "history bounded to 2", func(a *v1alpha1.Application) bool {
		return len(a.Status.History) == 2 && a.Status.History[1].ID == 2
	})
	assert.Equal(t, int64(1), got.Status.History[0].ID)
	assert.Equal(t, revs[1], got.Status.History[0].Revision)
	assert.Equal(t, revs[2], got.Status.History[1].Revision)
	e.waitOps(a, "two terminal operations retained", func(ops []v1alpha1.Operation) bool { return len(ops) == 2 })
}

// TestSyncWindowBlocksManualOperation: a deny window keeps a manual Operation Pending
// (Admitted=False/SyncWindowClosed) and prevents automated syncs; removing it admits the
// Operation.
func TestSyncWindowBlocksManualOperation(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.startController()
	proj := permissiveProject(e.ns, "windowed")
	proj.Spec.SyncWindows = v1alpha1.SyncWindows{{Kind: "deny", Schedule: "* * * * *", Duration: "1h", Applications: []string{"*"}}}
	e.create(proj)
	rev := e.repo.commit(e.repoURL, "win", "main", configMap("win-cm", nil, nil))
	e.repo.commit(e.repoURL, "winauto", "main", configMap("winauto-cm", nil, nil))
	a := e.newApp("win", func(a *v1alpha1.Application) { a.Spec.Project = "windowed" })
	auto := e.newApp("winauto", func(a *v1alpha1.Application) { a.Spec.Project = "windowed" }, automated(false, false))
	e.create(a)
	e.create(auto)
	e.waitApp(a, "compared", outOfSync)
	e.waitApp(auto, "compared", outOfSync)

	op := e.sync(a)
	pending := e.waitOp(op, "blocked by the window", func(op *v1alpha1.Operation) bool {
		return op.Status.Phase == v1alpha1.OperationPending && metaCondition(op.Status.Conditions, v1alpha1.OperationConditionAdmitted, metav1.ConditionFalse)
	})
	assert.Equal(t, "Sync operation blocked by sync window", pending.Status.Message)
	assert.Equal(t, v1alpha1.OperationReasonSyncWindowClosed, pending.Status.Conditions[0].Reason)
	assert.Empty(t, e.operations(auto), "automated sync is not attempted in a deny window")

	cur := &v1alpha1.AppProject{}
	require.NoError(t, e.c.Get(e.ctx, client.ObjectKeyFromObject(proj), cur))
	cur.Spec.SyncWindows = nil
	require.NoError(t, e.c.Update(e.ctx, cur))
	e.waitOp(op, "admitted after the window was removed", phaseIs(v1alpha1.OperationSucceeded))
	e.waitApp(a, "synced", synced(rev))
	e.waitApp(auto, "auto-synced once allowed", synced(""))
}
