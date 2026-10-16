package integration

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/controller/core"
)

// TestSkipReconcile: an Application annotated skip-reconcile=true gets no status; a value
// that does not parse as true does not skip; removing the annotation resumes (APP-088).
func TestSkipReconcile(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.startController()
	e.repo.commit(e.repoURL, "skip", "main", configMap("skip-cm", nil, nil))
	e.repo.commit(e.repoURL, "noskip", "main", configMap("noskip-cm", nil, nil))
	skip := e.newApp("skip", func(a *v1alpha1.Application) {
		a.Annotations = map[string]string{v1alpha1.AnnotationKeySkipReconcile: "true"}
	})
	noskip := e.newApp("noskip", func(a *v1alpha1.Application) {
		a.Annotations = map[string]string{v1alpha1.AnnotationKeySkipReconcile: "yes"}
	})
	e.create(skip)
	e.create(noskip)
	e.waitApp(noskip, "reconciled despite an unparsable value", outOfSync)
	time.Sleep(2 * time.Second)
	assert.Empty(t, e.getApp(skip).Status.Sync.Status, "no status while skip-reconcile is true")

	patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:null}}}`, v1alpha1.AnnotationKeySkipReconcile)
	require.NoError(t, e.c.Patch(e.ctx, skip.DeepCopy(), client.RawPatch(types.MergePatchType, []byte(patch))))
	e.waitApp(skip, "reconciled after the annotation was removed", outOfSync)
}

// TestSpecErrors: a missing project gives InvalidSpecError with sync and health Unknown and
// no comparison; creating the project resolves it (APP-002, APP-033).
func TestSpecErrors(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.startController()
	e.repo.commit(e.repoURL, "spec", "main", configMap("spec-cm", nil, nil))
	a := e.newApp("spec", func(a *v1alpha1.Application) { a.Spec.Project = "later" })
	e.create(a)
	got := e.waitApp(a, "InvalidSpecError", func(a *v1alpha1.Application) bool {
		return hasCondition(a, v1alpha1.ApplicationConditionInvalidSpecError) != nil
	})
	c := hasCondition(got, v1alpha1.ApplicationConditionInvalidSpecError)
	assert.Equal(t, "Application referencing project later which does not exist", c.Message)
	assert.Equal(t, v1alpha1.ReasonProjectNotFound, c.Reason)
	assert.Equal(t, metav1.ConditionTrue, c.Status)
	assert.Equal(t, v1alpha1.SyncStatusCodeUnknown, got.Status.Sync.Status)
	assert.Equal(t, v1alpha1.HealthStatusUnknown, got.Status.Health.Status)
	if r := hasCondition(got, v1alpha1.ApplicationConditionReconciled); assert.NotNil(t, r) {
		assert.Equal(t, metav1.ConditionFalse, r.Status)
	}

	e.create(permissiveProject(e.ns, "later"))
	got = e.waitApp(a, "resolved after the project was created", func(a *v1alpha1.Application) bool {
		return hasCondition(a, v1alpha1.ApplicationConditionInvalidSpecError) == nil && outOfSync(a)
	})
	if r := hasCondition(got, v1alpha1.ApplicationConditionReconciled); assert.NotNil(t, r) {
		assert.Equal(t, metav1.ConditionTrue, r.Status)
	}
}

// TestApplicationsInAnyNamespace: with application.namespaces, Applications in matching
// namespaces are reconciled when their project permits the namespace (sourceNamespaces),
// get UnknownError when it does not, and are ignored in other namespaces (APP-067/068).
func TestApplicationsInAnyNamespace(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	teamA, teamB, other := "team-"+e.sfx+"-a", "team-"+e.sfx+"-b", "other-"+e.sfx
	for _, ns := range []string{teamA, teamB, other} {
		e.createNamespace(ns)
	}
	e.startController(withParams(func(p *core.Params) { p.ApplicationNamespaces = []string{"team-" + e.sfx + "-*"} }))
	proj := permissiveProject(e.ns, "teams")
	proj.Spec.SourceNamespaces = []string{teamA}
	e.create(proj)
	rev := e.repo.commit(e.repoURL, "anyns", "main", configMap("anyns-cm", nil, nil))
	inNS := func(ns string) func(*v1alpha1.Application) {
		return func(a *v1alpha1.Application) {
			a.Namespace = ns
			a.Spec.Project = "teams"
			a.Spec.Source.Path = "anyns"
		}
	}
	allowed := e.newApp("ns-a", inNS(teamA))
	denied := e.newApp("ns-b", inNS(teamB))
	ignored := e.newApp("ns-other", inNS(other))
	for _, a := range []*v1alpha1.Application{allowed, denied, ignored} {
		e.create(a)
	}
	e.waitApp(allowed, "compared", outOfSync)
	got := e.waitApp(denied, "UnknownError", func(a *v1alpha1.Application) bool {
		return hasCondition(a, v1alpha1.ApplicationConditionUnknownError) != nil
	})
	assert.Equal(t, fmt.Sprintf("application '%s' in namespace '%s' is not permitted to use project 'teams'", denied.Name, teamB),
		hasCondition(got, v1alpha1.ApplicationConditionUnknownError).Message)
	assert.Equal(t, v1alpha1.SyncStatusCodeUnknown, got.Status.Sync.Status)

	op := e.sync(allowed)
	e.waitOp(op, "synced", phaseIs(v1alpha1.OperationSucceeded))
	synced := e.waitApp(allowed, "synced with history", func(a *v1alpha1.Application) bool {
		return synced(rev)(a) && len(a.Status.History) == 1
	})
	require.Len(t, synced.Status.History, 1)
	cm := mustLive(t, e, "anyns-cm")
	assert.Contains(t, cm.GetAnnotations()[v1alpha1.AnnotationKeyTrackingID], teamA+"_"+allowed.Name)

	time.Sleep(time.Second)
	assert.Empty(t, e.getApp(ignored).Status.Sync.Status, "Applications in other namespaces are ignored")
}

// TestRefreshAnnotationAndRecentRefresh: the refresh annotation forces a comparison and is
// removed; a managed resource change refreshes without bumping reconciledAt (APP-042/078/085).
func TestRefreshAnnotationAndRecentRefresh(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.startController()
	rev := e.repo.commit(e.repoURL, "rf", "main", configMap("rf-cm", map[string]string{"k": "v"}, nil))
	a := e.newApp("rf")
	e.create(a)
	e.waitApp(a, "compared", outOfSync)
	e.waitOp(e.sync(a), "synced", phaseIs(v1alpha1.OperationSucceeded))
	e.waitApp(a, "synced", synced(rev))
	// Let the post-operation refresh land, then take the reference reconciledAt.
	time.Sleep(1500 * time.Millisecond)
	before := e.getApp(a)

	e.mutateLive(gvrConfigMap, "rf-cm", func(u *unstructured.Unstructured) { u.Object["data"] = map[string]any{"k": "changed"} })
	drifted := e.waitApp(a, "OutOfSync from a live change", outOfSync)
	assert.True(t, drifted.Status.ReconciledAt.Equal(before.Status.ReconciledAt), "recent refreshes keep reconciledAt")

	e.refresh(a, v1alpha1.RefreshTypeHard)
	got := e.waitApp(a, "refreshed", func(a *v1alpha1.Application) bool {
		_, has := a.Annotations[v1alpha1.AnnotationKeyRefresh]
		return !has && a.Status.ReconciledAt.After(before.Status.ReconciledAt.Time)
	})
	assert.Equal(t, v1alpha1.SyncStatusCodeOutOfSync, got.Status.Sync.Status)
}

// TestRepositoryErrors: a manifest generation error is reported as ComparisonError with an
// Unknown sync status (no grace period configured), a sync attempted meanwhile ends in Error
// with the formatted condition (SYNC-076), and recovery clears the condition (APP-034).
func TestRepositoryErrors(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.startController()
	rev := e.repo.commit(e.repoURL, "repoerr", "main", configMap("repoerr-cm", nil, nil))
	a := e.newApp("repoerr")
	e.create(a)
	e.waitApp(a, "compared", outOfSync)

	e.repo.setError(e.repoURL, "repoerr", fmt.Errorf("rpc error: repository not reachable"))
	e.refresh(a, v1alpha1.RefreshTypeNormal)
	got := e.waitApp(a, "ComparisonError", func(a *v1alpha1.Application) bool {
		return hasCondition(a, v1alpha1.ApplicationConditionComparisonError) != nil
	})
	assert.Equal(t, v1alpha1.SyncStatusCodeUnknown, got.Status.Sync.Status)
	assert.Contains(t, hasCondition(got, v1alpha1.ApplicationConditionComparisonError).Message, "repository not reachable")
	if r := hasCondition(got, v1alpha1.ApplicationConditionReconciled); assert.NotNil(t, r) {
		assert.Equal(t, metav1.ConditionFalse, r.Status)
	}
	op := e.waitOp(e.sync(a), "Error", phaseIs(v1alpha1.OperationError))
	assert.Contains(t, op.Status.Message, "ComparisonError: Failed to load target state")

	e.repo.setError(e.repoURL, "repoerr", nil)
	e.refresh(a, v1alpha1.RefreshTypeNormal)
	e.waitApp(a, "recovered", func(a *v1alpha1.Application) bool {
		return outOfSync(a) && a.Status.Sync.Revision == rev && hasCondition(a, v1alpha1.ApplicationConditionComparisonError) == nil
	})
}

// TestShardFilter: Applications and Operations of a destination another shard owns are not
// processed (core.Deps.OwnsCluster, ADR-0008).
func TestShardFilter(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.startController(withOwnsCluster(func(c *v1alpha1.Cluster) bool { return false }))
	e.repo.commit(e.repoURL, "shard", "main", configMap("shard-cm", nil, nil))
	a := e.newApp("shard")
	e.create(a)
	op := e.sync(a)
	time.Sleep(2 * time.Second)
	assert.Empty(t, e.getApp(a).Status.Sync.Status, "not reconciled by a replica that does not own the cluster")
	assert.Empty(t, e.getOp(op).Status.Phase, "operations of other shards are not executed")
}

// TestShardFilterTerminalOperation: a replica that does not own the destination leaves a
// completed Operation and its Application alone (no phase label, no summary or history
// repair), so it never races the owning replica's completion writes (integration finding
// I-3).
func TestShardFilterTerminalOperation(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.startController(withOwnsCluster(func(c *v1alpha1.Cluster) bool { return false }))
	a := e.newApp("shard-done")
	e.create(a)
	op := e.sync(a)
	now := metav1.Now()
	op.Status = v1alpha1.OperationStatus{Phase: v1alpha1.OperationSucceeded, Message: "done elsewhere", StartedAt: &now, FinishedAt: &now,
		SyncResult: &v1alpha1.SyncOperationResult{Revision: "abc"}}
	require.NoError(t, e.c.Status().Update(e.ctx, op))
	// The non-owner must leave both objects alone for the whole window.
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(pollEvery) {
		got := e.getOp(op)
		require.Empty(t, got.Labels[v1alpha1.LabelKeyOperationPhase], "the phase label is the owner's to write")
		app := e.getApp(a)
		require.Nil(t, app.Status.OperationState, "the Application summary is the owner's to repair")
		require.Empty(t, app.Status.History, "the history is the owner's to record")
	}
}
