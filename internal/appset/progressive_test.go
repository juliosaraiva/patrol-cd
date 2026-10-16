package appset

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

var discard = slog.New(slog.DiscardHandler)

func step(maxUpdate *intstr.IntOrString, exprs ...v1alpha1.ApplicationMatchExpression) v1alpha1.ApplicationSetRolloutStep {
	return v1alpha1.ApplicationSetRolloutStep{MatchExpressions: exprs, MaxUpdate: maxUpdate}
}

func envIn(values ...string) v1alpha1.ApplicationMatchExpression {
	return v1alpha1.ApplicationMatchExpression{Key: "env", Operator: "In", Values: values}
}

func labeled(name string, labels map[string]string) v1alpha1.Application {
	return v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

func TestBuildSteps(t *testing.T) {
	t.Parallel()
	as := &v1alpha1.ApplicationSet{}
	as.Spec.Strategy = rollingSync(
		step(nil, envIn("dev")),
		step(nil, envIn("staging")), // no Application: does not block
		step(nil, v1alpha1.ApplicationMatchExpression{Key: "env", Operator: "NotIn", Values: []string{"dev", "staging"}}),
		step(nil, v1alpha1.ApplicationMatchExpression{Key: "env", Operator: "Exists"}),
	)
	apps := []v1alpha1.Application{
		labeled("d1", map[string]string{"env": "dev"}),
		labeled("p1", map[string]string{"env": "prod"}),
		labeled("nolabel", nil),
	}
	steps, stepOf := buildSteps(discard, as, apps)
	assert.Equal(t, [][]string{{"d1"}, {}, {"p1", "nolabel"}, {"nolabel"}}, steps,
		"NotIn selects an Application without the label; an invalid operator never matches a present label")
	assert.Equal(t, map[string]int{"d1": 0, "p1": 2, "nolabel": 2}, stepOf)
	assert.Equal(t, 1, stepNumber("d1", stepOf))
	assert.Equal(t, -1, stepNumber("other", stepOf))

	// Not RollingSync: no steps.
	steps, stepOf = buildSteps(discard, &v1alpha1.ApplicationSet{}, apps)
	assert.Empty(t, steps)
	assert.Empty(t, stepOf)
}

func TestAppsToSync(t *testing.T) {
	t.Parallel()
	as := &v1alpha1.ApplicationSet{}
	as.Status.ApplicationStatus = []v1alpha1.ApplicationSetApplicationStatus{
		{Application: "d1", Status: v1alpha1.ProgressiveSyncHealthy},
		{Application: "d2", Status: v1alpha1.ProgressiveSyncProgressing},
		{Application: "p1", Status: v1alpha1.ProgressiveSyncWaiting},
	}
	current := []v1alpha1.Application{labeled("d1", nil), labeled("d2", nil), labeled("p1", nil)}
	steps := [][]string{{"d1", "d2"}, {}, {"p1"}}
	assert.Equal(t, map[string]bool{"d1": true, "d2": true}, appsToSync(as, steps, current))
	as.Status.ApplicationStatus[1].Status = v1alpha1.ProgressiveSyncHealthy
	assert.Equal(t, map[string]bool{"d1": true, "d2": true, "p1": true}, appsToSync(as, steps, current), "an empty step does not block")
	assert.Equal(t, map[string]bool{"d1": true, "d2": true}, appsToSync(as, steps, current[:1]), "a missing Application blocks the next step")
}

func TestMaxUpdateValue(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      *intstr.IntOrString
		total   int
		want    int
		limited bool
	}{
		{"unset", nil, 10, 0, false},
		{"int", ptr.To(intstr.FromInt32(2)), 10, 2, true},
		{"ten percent of ten", ptr.To(intstr.FromString("10%")), 10, 1, true},
		{"percent rounds down but at least one", ptr.To(intstr.FromString("10%")), 3, 1, true},
		{"fifty percent", ptr.To(intstr.FromString("50%")), 5, 2, true},
		{"zero percent", ptr.To(intstr.FromString("0%")), 10, 0, true},
		{"zero", ptr.To(intstr.FromInt32(0)), 10, 0, true},
		{"invalid behaves as one", ptr.To(intstr.FromString("abc")), 10, 1, true},
	}
	for _, tt := range tests {
		got, limited := maxUpdateValue(discard, tt.in, tt.total)
		assert.Equal(t, tt.limited, limited, tt.name)
		assert.Equal(t, tt.want, got, tt.name)
	}
}

// progressiveFixture is an ApplicationSet with two RollingSync steps (dev, prod) reconciled
// by a controller with progressive syncs enabled and a controllable clock.
type progressiveFixture struct {
	t     *testing.T
	c     client.WithWatch
	r     *Reconciler
	clock *fixedClock
	as    *v1alpha1.ApplicationSet
}

func newProgressiveFixture(t *testing.T, maxUpdate *intstr.IntOrString, elems ...map[string]any) *progressiveFixture {
	t.Helper()
	as := listAppSet(t, controlNS, "rollout", elems...)
	as.UID = "appset-uid"
	as.Spec.Template.Spec.SyncPolicy = &v1alpha1.SyncPolicy{
		Automated:   &v1alpha1.SyncPolicyAutomated{Prune: ptr.To(true)},
		SyncOptions: v1alpha1.SyncOptions{"CreateNamespace=true"},
	}
	as.Spec.Strategy = rollingSync(step(maxUpdate, envIn("dev")), step(maxUpdate, envIn("prod")))
	c := newFakeClient(t, as, appProject("default"))
	clock := &fixedClock{now: time.Now().UTC().Truncate(time.Second)}
	r, _ := newTestReconciler(t, c, func(cfg *Config) {
		cfg.EnableProgressiveSyncs = true
		cfg.Now = clock.Now
	})
	return &progressiveFixture{t: t, c: c, r: r, clock: clock, as: as}
}

func (f *progressiveFixture) reconcile() {
	f.t.Helper()
	f.clock.Advance(5 * time.Second)
	reconcileOnce(f.t, f.r, f.as)
}

func (f *progressiveFixture) statuses() map[string]v1alpha1.ApplicationSetApplicationStatus {
	f.t.Helper()
	out := map[string]v1alpha1.ApplicationSetApplicationStatus{}
	for _, s := range getAppSet(f.t, f.c, controlNS, "rollout").Status.ApplicationStatus {
		out[s.Application] = s
	}
	return out
}

func (f *progressiveFixture) statusOf(name string) v1alpha1.ProgressiveSyncStatusCode {
	f.t.Helper()
	return f.statuses()[name].Status
}

func (f *progressiveFixture) operations() map[string]v1alpha1.Operation {
	f.t.Helper()
	var list v1alpha1.OperationList
	require.NoError(f.t, f.c.List(context.Background(), &list, client.InNamespace(controlNS)))
	out := map[string]v1alpha1.Operation{}
	for _, op := range list.Items {
		out[op.Spec.Application] = op
	}
	return out
}

// simulate sets an Application's status as a fast application controller would: it reports in
// the same second as the Application's last progressive sync transition, so timestamps alone
// cannot order its report after the transition (the API server stores seconds). With an
// operation phase, the operation state names the RollingSync Operation when one exists (a manual
// sync otherwise).
func (f *progressiveFixture) simulate(name string, sync v1alpha1.SyncStatusCode, health v1alpha1.HealthStatusCode, opPhase v1alpha1.OperationPhase) {
	f.t.Helper()
	app := &v1alpha1.Application{}
	require.NoError(f.t, f.c.Get(context.Background(), client.ObjectKey{Namespace: controlNS, Name: name}, app))
	at := metav1.NewTime(f.clock.Now().Truncate(time.Second))
	if st, ok := f.statuses()[name]; ok && st.LastTransitionTime != nil {
		at = *st.LastTransitionTime
	}
	app.Status.Sync.Status = sync
	app.Status.Health.Status = health
	app.Status.ReconciledAt = &at
	if opPhase != "" {
		opName := "manual-sync"
		if op, ok := f.operations()[name]; ok {
			opName = op.Name
		}
		app.Status.OperationState = &v1alpha1.ApplicationOperationState{OperationName: opName, Phase: opPhase, StartedAt: &at}
	}
	require.NoError(f.t, f.c.Status().Update(context.Background(), app))
	f.clock.Advance(2 * time.Second)
}

func (f *progressiveFixture) rolloutCondition() *metav1.Condition {
	f.t.Helper()
	return findCondition(getAppSet(f.t, f.c, controlNS, "rollout"), v1alpha1.ApplicationSetConditionRolloutProgressing)
}

func TestProgressiveSyncStepProgression(t *testing.T) {
	t.Parallel()
	f := newProgressiveFixture(t, nil, elem("dev1", "dev"), elem("dev2", "dev"), elem("prod1", "prod"))

	f.reconcile() // creates the Applications
	apps := listApps(t, f.c, controlNS)
	require.Len(t, apps, 3)
	for _, a := range apps {
		require.NotNil(t, a.Spec.SyncPolicy.Automated)
		assert.False(t, *a.Spec.SyncPolicy.Automated.Enabled, "automated sync is disabled on RollingSync Applications")
	}

	f.reconcile() // statuses: dev Pending, prod Waiting; dev Operations created
	st := f.statuses()
	assert.Equal(t, v1alpha1.ProgressiveSyncPending, st["dev1"].Status)
	assert.Equal(t, msgMovedToPending, st["dev1"].Message)
	assert.Equal(t, "1", st["dev1"].Step)
	assert.Equal(t, v1alpha1.ProgressiveSyncWaiting, st["prod1"].Status)
	assert.Equal(t, msgDefaultWaiting, st["prod1"].Message)
	assert.Equal(t, "2", st["prod1"].Step)
	cond := f.rolloutCondition()
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, "ApplicationSet is performing rollout of step 1", cond.Message)
	assert.Equal(t, v1alpha1.HealthStatusProgressing, getAppSet(t, f.c, controlNS, "rollout").Status.Health.Status)

	ops := f.operations()
	require.Len(t, ops, 2)
	op := ops["dev1"]
	assert.Equal(t, v1alpha1.OperationInitiator{Username: SyncInitiator, Automated: true}, op.Spec.InitiatedBy)
	assert.Equal(t, []v1alpha1.Info{{Name: "Reason", Value: SyncReason}}, op.Spec.Info)
	require.NotNil(t, op.Spec.Sync)
	assert.True(t, op.Spec.Sync.Prune, "prune follows the original automated policy")
	assert.Equal(t, v1alpha1.SyncOptions{"CreateNamespace=true"}, op.Spec.Sync.SyncOptions)
	assert.Equal(t, &v1alpha1.RetryStrategy{Limit: 5}, op.Spec.Retry)
	owner := metav1.GetControllerOf(&op)
	require.NotNil(t, owner)
	assert.Equal(t, "Application", owner.Kind)
	assert.Equal(t, "dev1", owner.Name)
	assert.Equal(t, "dev1", op.Labels[v1alpha1.LabelKeyOperationApplication])
	assert.Equal(t, string(v1alpha1.ProgressiveSyncPending), listApps(t, f.c, controlNS)["dev1"].Annotations[v1alpha1.AnnotationKeyApplicationSetProgressiveStatus])

	f.reconcile() // idempotent: no duplicate Operations
	assert.Len(t, f.operations(), 2)

	// The application controller ran the syncs.
	f.simulate("dev1", v1alpha1.SyncStatusCodeSynced, v1alpha1.HealthStatusProgressing, v1alpha1.OperationSucceeded)
	f.simulate("dev2", v1alpha1.SyncStatusCodeOutOfSync, v1alpha1.HealthStatusProgressing, v1alpha1.OperationRunning)
	f.reconcile()
	st = f.statuses()
	assert.Equal(t, v1alpha1.ProgressiveSyncProgressing, st["dev1"].Status)
	assert.Equal(t, msgSyncSucceeded, st["dev1"].Message)
	assert.Equal(t, v1alpha1.ProgressiveSyncProgressing, st["dev2"].Status)
	assert.Equal(t, msgBecameProgressing, st["dev2"].Message)
	assert.Equal(t, v1alpha1.ProgressiveSyncWaiting, st["prod1"].Status)

	f.simulate("dev1", v1alpha1.SyncStatusCodeSynced, v1alpha1.HealthStatusHealthy, "")
	f.reconcile()
	assert.Equal(t, v1alpha1.ProgressiveSyncHealthy, f.statusOf("dev1"))
	assert.Equal(t, msgBecameHealthy, f.statuses()["dev1"].Message)
	assert.Equal(t, v1alpha1.ProgressiveSyncWaiting, f.statusOf("prod1"), "prod waits for every dev Application")
	assert.NotContains(t, f.operations(), "prod1")

	f.simulate("dev2", v1alpha1.SyncStatusCodeSynced, v1alpha1.HealthStatusHealthy, "")
	f.reconcile() // dev2 Healthy; step 1 complete is seen by the next pass
	f.reconcile()
	assert.Equal(t, v1alpha1.ProgressiveSyncPending, f.statusOf("prod1"))
	assert.Contains(t, f.operations(), "prod1")
	assert.Equal(t, "ApplicationSet is performing rollout of step 2", f.rolloutCondition().Message)

	f.simulate("prod1", v1alpha1.SyncStatusCodeSynced, v1alpha1.HealthStatusHealthy, v1alpha1.OperationSucceeded)
	f.reconcile()
	f.reconcile()
	assert.Equal(t, v1alpha1.ProgressiveSyncHealthy, f.statusOf("prod1"))
	cond = f.rolloutCondition()
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, v1alpha1.ApplicationSetReasonApplicationSetRolloutComplete, cond.Reason)
	assert.Equal(t, v1alpha1.HealthStatusHealthy, getAppSet(t, f.c, controlNS, "rollout").Status.Health.Status)

	// A spec change of a Healthy Application sends it back to Waiting.
	as := getAppSet(t, f.c, controlNS, "rollout")
	as.Spec.Template.Spec.Source.TargetRevision = "v2"
	require.NoError(t, f.c.Update(context.Background(), as))
	f.simulate("dev1", v1alpha1.SyncStatusCodeOutOfSync, v1alpha1.HealthStatusHealthy, "")
	opsBefore := countOperations(t, f.c)
	f.reconcile()
	st = f.statuses()
	// Waiting (spec differs), then Pending in the same pass: step 1 may always sync.
	assert.Equal(t, v1alpha1.ProgressiveSyncPending, st["dev1"].Status)
	// dev2 still reports Synced (its controller has not refreshed yet), so, as upstream, it goes
	// from Waiting straight back to Healthy and is not synced.
	assert.Equal(t, v1alpha1.ProgressiveSyncHealthy, st["dev2"].Status)
	assert.Equal(t, opsBefore+1, countOperations(t, f.c), "a new Operation for the new Pending transition of dev1")

	// A revision change sends a synced Application through Waiting to Healthy.
	app := listApps(t, f.c, controlNS)["prod1"]
	app.Status.Sync.Revision = "abc123"
	require.NoError(t, f.c.Status().Update(context.Background(), &app))
	f.reconcile()
	assert.Equal(t, []string{"abc123"}, f.statuses()["prod1"].TargetRevisions)
}

// TestProgressiveSyncSameSecondStart: the RollingSync Operation is recognized by name even when
// it starts (and the Application is reconciled) in the same second as the Pending transition.
func TestProgressiveSyncSameSecondStart(t *testing.T) {
	t.Parallel()
	setup := func(t *testing.T) (*progressiveFixture, v1alpha1.Application, metav1.Time, string) {
		f := newProgressiveFixture(t, nil, elem("dev1", "dev"))
		f.reconcile()
		f.reconcile()
		st := f.statuses()["dev1"]
		require.Equal(t, v1alpha1.ProgressiveSyncPending, st.Status)
		require.NotNil(t, st.LastTransitionTime)
		op, ok := f.operations()["dev1"]
		require.True(t, ok)
		return f, listApps(t, f.c, controlNS)["dev1"], *st.LastTransitionTime, op.Name
	}
	report := func(f *progressiveFixture, app v1alpha1.Application, opName string, phase v1alpha1.OperationPhase, startedAt, reconciledAt metav1.Time) {
		app.Status.Sync.Status = v1alpha1.SyncStatusCodeSynced
		app.Status.Health.Status = v1alpha1.HealthStatusHealthy
		app.Status.ReconciledAt = &reconciledAt
		app.Status.OperationState = &v1alpha1.ApplicationOperationState{OperationName: opName, Phase: phase, StartedAt: &startedAt, FinishedAt: &startedAt}
		require.NoError(f.t, f.c.Status().Update(context.Background(), &app))
	}

	t.Run("own operation in the same second", func(t *testing.T) {
		t.Parallel()
		f, app, pendingAt, opName := setup(t)
		// Started 400ms after the transition: stored as the same second.
		started := metav1.NewTime(pendingAt.Add(400 * time.Millisecond).Truncate(time.Second))
		report(f, app, opName, v1alpha1.OperationSucceeded, started, started)
		f.reconcile()
		assert.Equal(t, v1alpha1.ProgressiveSyncProgressing, f.statusOf("dev1"))
		assert.Equal(t, msgSyncSucceeded, f.statuses()["dev1"].Message)
		f.reconcile()
		assert.Equal(t, v1alpha1.ProgressiveSyncHealthy, f.statusOf("dev1"))
		assert.Equal(t, metav1.ConditionFalse, f.rolloutCondition().Status, "the rollout completes")
	})

	t.Run("rejected own operation stays Pending", func(t *testing.T) {
		t.Parallel()
		f, app, pendingAt, opName := setup(t)
		report(f, app, opName, v1alpha1.OperationRejected, pendingAt, metav1.NewTime(pendingAt.Add(time.Minute)))
		f.reconcile()
		assert.Equal(t, v1alpha1.ProgressiveSyncPending, f.statusOf("dev1"))
	})

	t.Run("another operation in the same second is not attributed", func(t *testing.T) {
		t.Parallel()
		f, app, pendingAt, _ := setup(t)
		// It may have started before the transition within that second.
		report(f, app, "autosync-1", v1alpha1.OperationSucceeded, pendingAt, metav1.NewTime(pendingAt.Add(time.Minute)))
		f.reconcile()
		assert.Equal(t, v1alpha1.ProgressiveSyncPending, f.statusOf("dev1"))
		// One that started strictly later is (upstream semantics: e.g. a manual sync).
		app = listApps(t, f.c, controlNS)["dev1"]
		report(f, app, "manual-1", v1alpha1.OperationSucceeded, metav1.NewTime(pendingAt.Add(time.Second)), metav1.NewTime(pendingAt.Add(time.Minute)))
		f.reconcile()
		assert.Equal(t, v1alpha1.ProgressiveSyncProgressing, f.statusOf("dev1"))
	})
}

func TestProgressiveSyncMaxUpdate(t *testing.T) {
	t.Parallel()
	f := newProgressiveFixture(t, ptr.To(intstr.FromInt32(1)), elem("dev1", "dev"), elem("dev2", "dev"), elem("dev3", "dev"))
	f.reconcile()
	f.reconcile()
	pending := 0
	for _, s := range f.statuses() {
		if s.Status == v1alpha1.ProgressiveSyncPending {
			pending++
		}
	}
	assert.Equal(t, 1, pending, "maxUpdate 1: one Application at a time")
	assert.Len(t, f.operations(), 1)
}

func TestProgressiveSyncMaxUpdateZeroBlocks(t *testing.T) {
	t.Parallel()
	f := newProgressiveFixture(t, ptr.To(intstr.FromString("0%")), elem("dev1", "dev"))
	f.reconcile()
	f.reconcile()
	assert.Equal(t, v1alpha1.ProgressiveSyncWaiting, f.statusOf("dev1"))
	assert.Empty(t, f.operations())
	// A manual sync makes it Synced and Healthy: Waiting goes straight to Healthy.
	f.simulate("dev1", v1alpha1.SyncStatusCodeSynced, v1alpha1.HealthStatusHealthy, v1alpha1.OperationSucceeded)
	f.reconcile()
	assert.Equal(t, v1alpha1.ProgressiveSyncHealthy, f.statusOf("dev1"))
	assert.Equal(t, msgSyncedHealthy, f.statuses()["dev1"].Message)
}

func TestProgressiveSyncApplicationError(t *testing.T) {
	t.Parallel()
	f := newProgressiveFixture(t, nil, elem("dev1", "dev"))
	f.reconcile()
	f.reconcile()
	require.Equal(t, v1alpha1.ProgressiveSyncPending, f.statusOf("dev1"))
	app := listApps(t, f.c, controlNS)["dev1"]
	at := metav1.NewTime(f.clock.Now().Add(time.Second))
	app.Status.ReconciledAt = &at
	app.Status.Conditions = []metav1.Condition{{Type: v1alpha1.ApplicationConditionInvalidSpecError, Status: metav1.ConditionTrue, Reason: "InvalidSpec", Message: "bad", LastTransitionTime: at}}
	require.NoError(t, f.c.Status().Update(context.Background(), &app))
	f.clock.Advance(2 * time.Second)
	f.reconcile()
	assert.Equal(t, v1alpha1.ProgressiveSyncProgressing, f.statusOf("dev1"))
	assert.Equal(t, msgAppError, f.statuses()["dev1"].Message)
}

func TestProgressiveSyncRejectedOperationRetried(t *testing.T) {
	t.Parallel()
	f := newProgressiveFixture(t, nil, elem("dev1", "dev"))
	f.reconcile()
	f.reconcile()
	op := f.operations()["dev1"]
	op.Status.Phase = v1alpha1.OperationRejected
	require.NoError(t, f.c.Status().Update(context.Background(), &op))
	res, err := f.r.Reconcile(context.Background(), requestFor(f.as))
	require.NoError(t, err)
	assert.Equal(t, ReverseDeletionRequeue, res.RequeueAfter)
	assert.Empty(t, f.operations(), "the rejected Operation is deleted")
	f.reconcile()
	assert.Contains(t, f.operations(), "dev1", "and recreated")
}

func TestProgressiveSyncSwitchToAllAtOnce(t *testing.T) {
	t.Parallel()
	f := newProgressiveFixture(t, nil, elem("dev1", "dev"))
	f.reconcile()
	f.reconcile()
	require.NotEmpty(t, f.statuses())
	as := getAppSet(t, f.c, controlNS, "rollout")
	as.Spec.Strategy = &v1alpha1.ApplicationSetStrategy{Type: v1alpha1.ApplicationSetStrategyAllAtOnce}
	require.NoError(t, f.c.Update(context.Background(), as))
	f.reconcile()
	assert.Empty(t, f.statuses())
	assert.Nil(t, f.rolloutCondition())
	app := listApps(t, f.c, controlNS)["dev1"]
	assert.Nil(t, app.Spec.SyncPolicy.Automated.Enabled, "automated sync is no longer disabled")
	assert.NotContains(t, app.Annotations, v1alpha1.AnnotationKeyApplicationSetProgressiveStatus)
}

func TestProgressiveSyncCreateOnlyNeverTriggers(t *testing.T) {
	t.Parallel()
	f := newProgressiveFixture(t, nil, elem("dev1", "dev"))
	as := getAppSet(t, f.c, controlNS, "rollout")
	as.Spec.SyncPolicy = &v1alpha1.ApplicationSetSyncPolicy{ApplicationsSync: ptr.To(v1alpha1.ApplicationsSyncPolicyCreateOnly)}
	require.NoError(t, f.c.Update(context.Background(), as))
	f.r.cfg.EnablePolicyOverride = true
	f.reconcile()
	f.reconcile()
	assert.Equal(t, v1alpha1.ProgressiveSyncPending, f.statusOf("dev1"))
	assert.Empty(t, f.operations())
}

func TestReverseDeletion(t *testing.T) {
	t.Parallel()
	f := newProgressiveFixture(t, nil, elem("dev1", "dev"), elem("dev2", "dev"), elem("prod1", "prod"), elem("prod2", "prod"), elem("none", "qa"))
	as := getAppSet(t, f.c, controlNS, "rollout")
	as.Spec.Strategy.DeletionOrder = v1alpha1.ApplicationSetDeletionOrderReverse
	require.NoError(t, f.c.Update(context.Background(), as))
	f.reconcile()
	as = getAppSet(t, f.c, controlNS, "rollout")
	require.Contains(t, as.Finalizers, v1alpha1.FinalizerApplicationSet, "finalizer added for ordered deletion")

	require.NoError(t, f.c.Delete(context.Background(), as))
	terminating := func() []string {
		var out []string
		for name, a := range listApps(t, f.c, controlNS) {
			if a.DeletionTimestamp != nil {
				out = append(out, name)
			}
		}
		sortStringsInPlace(out)
		return out
	}
	release := func(name string) {
		app := listApps(t, f.c, controlNS)[name]
		app.Finalizers = nil
		require.NoError(t, f.c.Update(context.Background(), &app))
	}
	step := func() time.Duration {
		res, err := f.r.Reconcile(context.Background(), requestFor(f.as))
		require.NoError(t, err)
		return res.RequeueAfter
	}

	assert.Equal(t, ReverseDeletionRequeue, step())
	assert.Equal(t, []string{"prod1"}, terminating(), "the last step is deleted first, one Application at a time")
	assert.Equal(t, ReverseDeletionRequeue, step(), "waits while the Application terminates")
	assert.Equal(t, []string{"prod1"}, terminating())
	release("prod1")
	step()
	assert.Equal(t, []string{"prod2"}, terminating())
	release("prod2")
	step()
	assert.Equal(t, []string{"dev1"}, terminating(), "step 1 only after every step 2 Application is gone")
	release("dev1")
	step()
	release("dev2")
	assert.Zero(t, step())
	assertGone(t, f.c, f.as)
	_, kept := listApps(t, f.c, controlNS)["none"]
	assert.True(t, kept, "Applications outside every step are left to garbage collection")
}

func TestReverseDeletionStuckApplication(t *testing.T) {
	t.Parallel()
	f := newProgressiveFixture(t, nil, elem("dev1", "dev"))
	as := getAppSet(t, f.c, controlNS, "rollout")
	as.Spec.Strategy.DeletionOrder = v1alpha1.ApplicationSetDeletionOrderReverse
	require.NoError(t, f.c.Update(context.Background(), as))
	f.reconcile()
	require.NoError(t, f.c.Delete(context.Background(), getAppSet(t, f.c, controlNS, "rollout")))
	_, err := f.r.Reconcile(context.Background(), requestFor(f.as))
	require.NoError(t, err)
	f.clock.Advance(3 * time.Minute)
	_, err = f.r.Reconcile(context.Background(), requestFor(f.as))
	require.EqualError(t, err, "application has not been deleted in over 2 minutes")
}

func TestOperationName(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	a := operationName("uid", "app", at)
	assert.Equal(t, a, operationName("uid", "app", at))
	assert.NotEqual(t, a, operationName("uid", "app", at.Add(time.Second)))
	assert.Regexp(t, `^app-rollingsync-[0-9a-f]{10}$`, a)
	long := operationName("uid", string(make([]byte, 0))+stringOf('a', 300), at)
	assert.LessOrEqual(t, len(long), 253)
}

func stringOf(c byte, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = c
	}
	return string(b)
}

func sortStringsInPlace(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func countOperations(t *testing.T, c client.Client) int {
	t.Helper()
	var list v1alpha1.OperationList
	require.NoError(t, c.List(context.Background(), &list))
	return len(list.Items)
}
