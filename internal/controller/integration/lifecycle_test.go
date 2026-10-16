package integration

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// TestManualSyncLifecycle: a new Application is OutOfSync/Missing; a manual Operation syncs
// it (admission, plan, steps, completion); the Application becomes Synced/Healthy with a
// history entry, the operation summary, aggregate conditions and Events.
func TestManualSyncLifecycle(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctl := e.startController()
	rev := e.repo.commit(e.repoURL, "guestbook", "main", configMap("guestbook-config", map[string]string{"color": "blue"}, nil))
	a := e.newApp("guestbook")
	e.create(a)

	got := e.waitApp(a, "OutOfSync and Missing", func(a *v1alpha1.Application) bool {
		return outOfSync(a) && a.Status.Health.Status == v1alpha1.HealthStatusMissing
	})
	assert.Equal(t, rev, got.Status.Sync.Revision)
	assert.Equal(t, e.ns, got.Status.ControllerNamespace)
	assert.Equal(t, got.Generation, got.Status.ObservedGeneration)
	require.NotNil(t, got.Status.ReconciledAt)
	if c := hasCondition(got, v1alpha1.ApplicationConditionSynced); assert.NotNil(t, c) {
		assert.Equal(t, metav1.ConditionFalse, c.Status)
		assert.Equal(t, v1alpha1.ReasonOutOfSync, c.Reason)
	}
	if c := hasCondition(got, v1alpha1.ApplicationConditionReconciled); assert.NotNil(t, c) {
		assert.Equal(t, metav1.ConditionTrue, c.Status)
	}
	assert.Empty(t, e.operations(a), "no operation without automated sync")

	op := e.sync(a)
	done := e.waitOp(op, "Succeeded with the phase label", func(op *v1alpha1.Operation) bool {
		return op.Status.Phase == v1alpha1.OperationSucceeded && op.Labels[v1alpha1.LabelKeyOperationPhase] == string(v1alpha1.OperationSucceeded)
	})
	assert.Equal(t, a.Name, done.Labels[v1alpha1.LabelKeyOperationApplication])
	assert.Equal(t, string(v1alpha1.OperationSucceeded), done.Labels[v1alpha1.LabelKeyOperationPhase])
	require.NotNil(t, done.Status.Plan)
	assert.Equal(t, []string{rev}, done.Status.Plan.ResolvedRevisions)
	assert.NotEmpty(t, done.Status.Plan.RenderedInputDigest)
	require.NotNil(t, done.Status.SyncResult)
	assert.Equal(t, rev, done.Status.SyncResult.Revision)
	require.Len(t, done.Status.SyncResult.Resources, 1)
	assert.Equal(t, v1alpha1.ResultCodeSynced, done.Status.SyncResult.Resources[0].Status)
	assert.NotNil(t, done.Status.FinishedAt)
	assert.True(t, metaCondition(done.Status.Conditions, v1alpha1.OperationConditionAdmitted, metav1.ConditionTrue))
	assert.True(t, metaCondition(done.Status.Conditions, v1alpha1.OperationConditionPlanned, metav1.ConditionTrue))
	assert.True(t, metaCondition(done.Status.Conditions, v1alpha1.OperationConditionCompleted, metav1.ConditionTrue))
	require.Len(t, done.OwnerReferences, 1)
	assert.Equal(t, a.Name, done.OwnerReferences[0].Name)

	got = e.waitApp(a, "Synced and Healthy after the sync", func(a *v1alpha1.Application) bool {
		return synced(rev)(a) && a.Status.Health.Status == v1alpha1.HealthStatusHealthy && len(a.Status.History) == 1
	})
	h := got.Status.History[0]
	assert.Equal(t, int64(0), h.ID)
	assert.Equal(t, rev, h.Revision)
	assert.Equal(t, op.Name, h.OperationName)
	assert.Equal(t, "tester", h.InitiatedBy.Username)
	require.NotNil(t, h.Source)
	assert.Equal(t, "guestbook", h.Source.Path)
	require.NotNil(t, got.Status.OperationState)
	assert.Equal(t, op.Name, got.Status.OperationState.OperationName)
	assert.Equal(t, v1alpha1.OperationSucceeded, got.Status.OperationState.Phase)
	require.NotNil(t, got.Status.OperationState.SyncResult)
	assert.Equal(t, rev, got.Status.OperationState.SyncResult.Revision)
	if c := hasCondition(got, v1alpha1.ApplicationConditionHealthy); assert.NotNil(t, c) {
		assert.Equal(t, metav1.ConditionTrue, c.Status)
	}
	live, ok := e.live(gvrConfigMap, "guestbook-config")
	require.True(t, ok)
	color, _, _ := unstructured.NestedString(live.Object, "data", "color")
	assert.Equal(t, "blue", color)
	assert.NotEmpty(t, live.GetAnnotations()[v1alpha1.AnnotationKeyTrackingID])

	state, ok := ctl.appRec.State(keyOf(a))
	require.True(t, ok, "the last comparison is kept in memory")
	require.NotNil(t, state.Tree)
	assert.NotNil(t, state.Tree.FindNode("", "ConfigMap", e.dest, "guestbook-config"))
	require.Len(t, state.Managed, 1)
	assert.NotNil(t, state.Managed[0].Live)

	assert.Eventually(t, func() bool { return ctl.events.has("OperationCompleted: Sync operation to " + rev + " succeeded") }, waitTimeout, pollEvery)
	assert.True(t, ctl.events.has("ResourceUpdated: Updated sync status: OutOfSync -> Synced"))
}

func metaCondition(conds []metav1.Condition, t string, status metav1.ConditionStatus) bool {
	for _, c := range conds {
		if c.Type == t {
			return c.Status == status
		}
	}
	return false
}
