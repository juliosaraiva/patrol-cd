package integration

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// TestRestartMidOperation kills the controller twice during a multi-wave sync: while a
// PreSync hook runs and while wave 0 waits for its Deployment to become healthy. Each new
// controller resumes the Running Operation from its persisted status: the hook is adopted
// (not created twice), applied resources are not re-created, and the sync completes (SYNC-077,
// sync-state-machine.md "Idempotency and restart recovery").
func TestRestartMidOperation(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctl := e.startController()
	rev := e.repo.commit(e.repoURL, "rs", "main",
		hookPod("rs-pre-", v1alpha1.HookTypePreSync),
		withWave(deployment("rs-web"), 0),
		withWave(configMap("rs-after", nil, nil), 1),
	)
	a := e.newApp("rs")
	e.create(a)
	e.waitApp(a, "compared", outOfSync)
	op := e.sync(a)
	e.waitOp(op, "waiting for the PreSync hook", func(op *v1alpha1.Operation) bool {
		return op.Status.Phase == v1alpha1.OperationRunning && strings.Contains(op.Status.Message, "waiting for completion of hook")
	})
	hooks := e.podsWithPrefix("rs-pre-")
	require.Len(t, hooks, 1)
	hookName := hooks[0].GetName()

	// First crash: during the PreSync phase.
	ctl.stop()
	assert.Equal(t, v1alpha1.OperationRunning, e.getOp(op).Status.Phase, "the operation survives the crash")
	ctl = e.startController()
	time.Sleep(2 * time.Second)
	hooks = e.podsWithPrefix("rs-pre-")
	require.Len(t, hooks, 1, "the hook is adopted, not created again")
	assert.Equal(t, hookName, hooks[0].GetName())
	e.setPodPhase(hookName, corev1.PodSucceeded)

	web := e.waitLive(gvrDeployment, "rs-web", "wave 0 applied", exists)
	e.waitOp(op, "waiting for the Deployment", func(op *v1alpha1.Operation) bool {
		return op.Status.Phase == v1alpha1.OperationRunning && strings.Contains(op.Status.Message, "waiting for healthy state of")
	})
	_, ok := e.live(gvrConfigMap, "rs-after")
	assert.False(t, ok, "wave 1 waits for wave 0")

	// Second crash: mid-wave, between apply and health.
	ctl.stop()
	e.startController()
	time.Sleep(time.Second)
	e.markDeploymentAvailable("rs-web")
	done := e.waitOp(op, "Succeeded", phaseIs(v1alpha1.OperationSucceeded))
	assert.Equal(t, int64(0), done.Status.RetryCount)
	e.waitLive(gvrConfigMap, "rs-after", "wave 1 applied", exists)
	webAfter, ok := e.live(gvrDeployment, "rs-web")
	require.True(t, ok)
	assert.Equal(t, web.GetUID(), webAfter.GetUID(), "the Deployment was not re-created")
	assert.Len(t, e.podsWithPrefix("rs-pre-"), 1, "exactly one hook Pod ran")
	var hookRows int
	for _, r := range done.Status.SyncResult.Resources {
		if r.HookType == v1alpha1.HookTypePreSync {
			hookRows++
			assert.Equal(t, v1alpha1.OperationSucceeded, r.HookPhase)
		}
	}
	assert.Equal(t, 1, hookRows)
	got := e.waitApp(a, "synced with one history entry", func(a *v1alpha1.Application) bool {
		return synced(rev)(a) && len(a.Status.History) == 1
	})
	assert.Equal(t, op.Name, got.Status.History[0].OperationName)

}

func (e *env) getOp(op *v1alpha1.Operation) *v1alpha1.Operation {
	e.t.Helper()
	out := &v1alpha1.Operation{}
	require.NoError(e.t, e.c.Get(e.ctx, keyOfOp(op), out))
	return out
}
