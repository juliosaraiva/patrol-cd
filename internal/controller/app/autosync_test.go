package app

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/ptr"
	"pgregory.net/rapid"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/controller/core"
)

var now = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

func testApp(mutate ...func(*v1alpha1.Application)) *v1alpha1.Application {
	a := &v1alpha1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: "guestbook", Namespace: "patrol", UID: "uid-1"},
		Spec: v1alpha1.ApplicationSpec{
			Source:      &v1alpha1.ApplicationSource{RepoURL: "https://git.example.com/r.git", Path: "guestbook", TargetRevision: "main"},
			Destination: v1alpha1.ApplicationDestination{Server: "https://k8s", Namespace: "default"},
			SyncPolicy:  &v1alpha1.SyncPolicy{Automated: &v1alpha1.SyncPolicyAutomated{}},
		},
	}
	for _, fn := range mutate {
		fn(a)
	}
	return a
}

func lastSync(phase v1alpha1.OperationPhase, rev string, src *v1alpha1.ApplicationSource) func(*v1alpha1.Application) {
	return func(a *v1alpha1.Application) {
		a.Status.OperationState = &v1alpha1.ApplicationOperationState{
			OperationName: "op", Phase: phase, Message: "boom",
			SyncResult: &v1alpha1.SyncResultSummary{Revision: rev, Source: src},
		}
	}
}

func outOfSyncResources(names ...string) []v1alpha1.ResourceStatus {
	var out []v1alpha1.ResourceStatus
	for _, n := range names {
		out = append(out, v1alpha1.ResourceStatus{Kind: "ConfigMap", Name: n, Status: v1alpha1.SyncStatusCodeOutOfSync})
	}
	return out
}

func TestAlreadyAttemptedSync(t *testing.T) {
	t.Parallel()
	src := v1alpha1.ApplicationSource{RepoURL: "https://git.example.com/r.git", Path: "guestbook", TargetRevision: "main"}
	other := src
	other.Path = "other"
	multi := func(a *v1alpha1.Application) {
		a.Spec.Source = nil
		a.Spec.Sources = v1alpha1.ApplicationSources{src, other}
	}
	for _, tc := range []struct {
		name       string
		app        *v1alpha1.Application
		desired    []string
		hasChanges bool
		want       bool
	}{
		{"no operation state", testApp(), []string{"sha1"}, true, false},
		{"same revision and source", testApp(lastSync(v1alpha1.OperationSucceeded, "sha1", &src)), []string{"sha1"}, true, true},
		{"different revision", testApp(lastSync(v1alpha1.OperationSucceeded, "sha1", &src)), []string{"sha2"}, true, false},
		{"different revision without changes", testApp(lastSync(v1alpha1.OperationSucceeded, "sha1", &src)), []string{"sha2"}, false, true},
		{"different source", testApp(lastSync(v1alpha1.OperationSucceeded, "sha1", &other)), []string{"sha1"}, true, false},
		{"completed without result", testApp(func(a *v1alpha1.Application) {
			a.Status.OperationState = &v1alpha1.ApplicationOperationState{Phase: v1alpha1.OperationError}
		}), []string{"sha1"}, true, true},
		{"running without result", testApp(func(a *v1alpha1.Application) {
			a.Status.OperationState = &v1alpha1.ApplicationOperationState{Phase: v1alpha1.OperationRunning}
		}), []string{"sha1"}, true, false},
		{"multi-source same", testApp(multi, func(a *v1alpha1.Application) {
			a.Status.OperationState = &v1alpha1.ApplicationOperationState{Phase: v1alpha1.OperationSucceeded,
				SyncResult: &v1alpha1.SyncResultSummary{Revisions: []string{"a", "b"}, Sources: v1alpha1.ApplicationSources{src, other}}}
		}), []string{"a", "b"}, true, true},
		{"multi-source one revision differs", testApp(multi, func(a *v1alpha1.Application) {
			a.Status.OperationState = &v1alpha1.ApplicationOperationState{Phase: v1alpha1.OperationSucceeded,
				SyncResult: &v1alpha1.SyncResultSummary{Revisions: []string{"a", "b"}, Sources: v1alpha1.ApplicationSources{src, other}}}
		}), []string{"a", "c"}, true, false},
		{"multi-source sources differ", testApp(multi, func(a *v1alpha1.Application) {
			a.Status.OperationState = &v1alpha1.ApplicationOperationState{Phase: v1alpha1.OperationSucceeded,
				SyncResult: &v1alpha1.SyncResultSummary{Revisions: []string{"a", "b"}, Sources: v1alpha1.ApplicationSources{src}}}
		}), []string{"a", "b"}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, _, _ := alreadyAttemptedSync(tc.app, tc.desired, tc.hasChanges)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestSelfHealRemainingBackoff mirrors the upstream table: Duration 2s, Factor 3, Cap 2m.
func TestSelfHealRemainingBackoff(t *testing.T) {
	t.Parallel()
	p := core.Params{SelfHealBackoff: &wait.Backoff{Duration: 2 * time.Second, Factor: 3, Cap: 2 * time.Minute}}
	finished := func(ago time.Duration) *metav1.Time { t := metav1.NewTime(now.Add(-ago)); return &t }
	for i, tc := range []struct {
		attempts   int
		finishedAt *metav1.Time
		want       time.Duration
	}{
		{0, finished(0), 0},
		{1, finished(0), 2 * time.Second},
		{2, finished(0), 6 * time.Second},
		{3, nil, 18 * time.Second},
		{4, nil, 54 * time.Second},
		{5, nil, 120 * time.Second},
		{6, nil, 120 * time.Second},
		{6, finished(0), 120 * time.Second},
		{40, finished(time.Minute), 60 * time.Second},
	} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			t.Parallel()
			a := testApp(func(a *v1alpha1.Application) {
				a.Status.OperationState = &v1alpha1.ApplicationOperationState{FinishedAt: tc.finishedAt}
			})
			assert.Equal(t, tc.want, selfHealRemainingBackoff(a, tc.attempts, p, now))
		})
	}
	// Without a backoff, the fixed timeout minus the time since the last operation.
	a := testApp(func(a *v1alpha1.Application) {
		a.Status.OperationState = &v1alpha1.ApplicationOperationState{FinishedAt: finished(3 * time.Second)}
	})
	assert.Equal(t, 2*time.Second, selfHealRemainingBackoff(a, 3, core.Params{SelfHealTimeout: 5 * time.Second}, now))
	assert.Equal(t, time.Duration(0), selfHealRemainingBackoff(testApp(), 3, p, now), "no operation state")
}

func TestDecideAutoSync(t *testing.T) {
	t.Parallel()
	src := v1alpha1.ApplicationSource{RepoURL: "https://git.example.com/r.git", Path: "guestbook", TargetRevision: "main"}
	outOfSync := v1alpha1.SyncStatus{Status: v1alpha1.SyncStatusCodeOutOfSync, Revision: "sha2"}
	base := autoSyncInput{sync: outOfSync, resources: outOfSyncResources("a"), revisionsMayHaveChanges: true, now: now, params: core.DefaultParams()}
	run := func(mutate func(*autoSyncInput)) autoSyncDecision {
		in := base
		in.app = testApp()
		mutate(&in)
		return decideAutoSync(in)
	}

	t.Run("creates an automated operation", func(t *testing.T) {
		t.Parallel()
		d := run(func(in *autoSyncInput) {
			in.app.Spec.SyncPolicy.Automated.Prune = ptr.To(true)
			in.app.Spec.SyncPolicy.SyncOptions = v1alpha1.SyncOptions{"CreateNamespace=true"}
		})
		require.NotNil(t, d.operation)
		op := d.operation
		assert.True(t, op.Spec.InitiatedBy.Automated)
		assert.Equal(t, "guestbook", op.Spec.Application)
		assert.Equal(t, "sha2", op.Spec.Sync.Revision)
		assert.True(t, op.Spec.Sync.Prune)
		assert.Equal(t, v1alpha1.SyncOptions{"CreateNamespace=true"}, op.Spec.Sync.SyncOptions)
		assert.Equal(t, int64(5), op.Spec.Retry.Limit)
		assert.Equal(t, "guestbook", op.Labels[v1alpha1.LabelKeyOperationApplication])
		require.Len(t, op.OwnerReferences, 1)
		assert.True(t, *op.OwnerReferences[0].Controller)
		assert.Nil(t, d.condition)
	})
	t.Run("uses the application retry strategy", func(t *testing.T) {
		t.Parallel()
		d := run(func(in *autoSyncInput) { in.app.Spec.SyncPolicy.Retry = &v1alpha1.RetryStrategy{Limit: -1} })
		require.NotNil(t, d.operation)
		assert.Equal(t, int64(-1), d.operation.Spec.Retry.Limit)
	})
	for _, tc := range []struct {
		name   string
		mutate func(*autoSyncInput)
	}{
		{"disabled", func(in *autoSyncInput) { in.app.Spec.SyncPolicy.Automated.Enabled = ptr.To(false) }},
		{"no automated policy", func(in *autoSyncInput) { in.app.Spec.SyncPolicy = nil }},
		{"operation in progress", func(in *autoSyncInput) { in.operationInProgress = true }},
		{"deleting", func(in *autoSyncInput) { ts := metav1.NewTime(now); in.app.DeletionTimestamp = &ts }},
		{"synced", func(in *autoSyncInput) { in.sync.Status = v1alpha1.SyncStatusCodeSynced }},
		{"unknown", func(in *autoSyncInput) { in.sync.Status = v1alpha1.SyncStatusCodeUnknown }},
		{"prune only without auto-prune", func(in *autoSyncInput) {
			in.resources = []v1alpha1.ResourceStatus{{Name: "x", Status: v1alpha1.SyncStatusCodeOutOfSync, RequiresPruning: true}, {Name: "y", Status: v1alpha1.SyncStatusCodeSynced}}
		}},
		{"already synced without self-heal", func(in *autoSyncInput) { lastSync(v1alpha1.OperationSucceeded, "sha2", &src)(in.app) }},
	} {
		t.Run("skips: "+tc.name, func(t *testing.T) {
			t.Parallel()
			d := run(tc.mutate)
			assert.Nil(t, d.operation)
			assert.Nil(t, d.condition)
		})
	}
	t.Run("failed previous attempt", func(t *testing.T) {
		t.Parallel()
		d := run(func(in *autoSyncInput) { lastSync(v1alpha1.OperationFailed, "sha2", &src)(in.app) })
		assert.Nil(t, d.operation)
		require.NotNil(t, d.condition)
		assert.Equal(t, v1alpha1.ApplicationConditionSyncError, d.condition.Type)
		assert.Equal(t, "Failed last sync attempt to [sha2]: boom", d.condition.Message)
	})
	t.Run("would wipe out every resource", func(t *testing.T) {
		t.Parallel()
		d := run(func(in *autoSyncInput) {
			in.app.Spec.SyncPolicy.Automated.Prune = ptr.To(true)
			in.resources = []v1alpha1.ResourceStatus{{Name: "x", Status: v1alpha1.SyncStatusCodeOutOfSync, RequiresPruning: true}}
		})
		assert.Nil(t, d.operation)
		require.NotNil(t, d.condition)
		assert.Equal(t, "Skipping sync attempt to [sha2]: auto-sync will wipe out all resources", d.condition.Message)
	})
	t.Run("allowEmpty permits wiping out", func(t *testing.T) {
		t.Parallel()
		d := run(func(in *autoSyncInput) {
			in.app.Spec.SyncPolicy.Automated.Prune = ptr.To(true)
			in.app.Spec.SyncPolicy.Automated.AllowEmpty = ptr.To(true)
			in.resources = []v1alpha1.ResourceStatus{{Name: "x", Status: v1alpha1.SyncStatusCodeOutOfSync, RequiresPruning: true}}
		})
		assert.NotNil(t, d.operation)
	})
	t.Run("self-heal", func(t *testing.T) {
		t.Parallel()
		d := run(func(in *autoSyncInput) {
			in.app.Spec.SyncPolicy.Automated.SelfHeal = ptr.To(true)
			lastSync(v1alpha1.OperationSucceeded, "sha2", &src)(in.app)
			in.selfHealAttempts = 0
			in.resources = append(outOfSyncResources("drifted"), v1alpha1.ResourceStatus{Kind: "ConfigMap", Name: "fine", Status: v1alpha1.SyncStatusCodeSynced})
		})
		require.NotNil(t, d.operation)
		assert.Equal(t, int64(1), d.operation.Spec.Sync.SelfHealAttemptsCount)
		assert.Equal(t, []v1alpha1.SyncOperationResource{{Kind: "ConfigMap", Name: "drifted"}}, d.operation.Spec.Sync.Resources)
	})
	t.Run("self-heal backoff", func(t *testing.T) {
		t.Parallel()
		d := run(func(in *autoSyncInput) {
			in.app.Spec.SyncPolicy.Automated.SelfHeal = ptr.To(true)
			lastSync(v1alpha1.OperationSucceeded, "sha2", &src)(in.app)
			fin := metav1.NewTime(now.Add(-time.Second))
			in.app.Status.OperationState.FinishedAt = &fin
			in.selfHealAttempts = 2
		})
		assert.Nil(t, d.operation)
		assert.Equal(t, 5*time.Second, d.requeueAfter)
	})
}

func TestAutomatedOperationName(t *testing.T) {
	t.Parallel()
	a := testApp()
	n1 := AutomatedOperationName(a, []string{"sha1"}, 0)
	assert.Equal(t, n1, AutomatedOperationName(a, []string{"sha1"}, 0), "deterministic")
	assert.True(t, strings.HasPrefix(n1, "guestbook-auto-"))
	assert.NotEqual(t, n1, AutomatedOperationName(a, []string{"sha2"}, 0))
	assert.NotEqual(t, n1, AutomatedOperationName(a, []string{"sha1"}, 1))
	b := testApp(func(a *v1alpha1.Application) { a.Spec.Source.Path = "other" })
	assert.NotEqual(t, n1, AutomatedOperationName(b, []string{"sha1"}, 0), "source changes produce a new attempt")

	// The summarized operation is part of the decision: once another Operation ran (a manual
	// sync, an earlier heal), the same revisions and attempt get a new name.
	started := metav1.NewTime(now)
	after := func(name string, at metav1.Time) *v1alpha1.Application {
		return testApp(func(a *v1alpha1.Application) {
			a.Status.OperationState = &v1alpha1.ApplicationOperationState{OperationName: name, Phase: v1alpha1.OperationSucceeded, StartedAt: &at}
		})
	}
	m1 := AutomatedOperationName(after("manual-1", started), []string{"sha1"}, 1)
	assert.Equal(t, m1, AutomatedOperationName(after("manual-1", started), []string{"sha1"}, 1), "same state, same name")
	assert.NotEqual(t, m1, AutomatedOperationName(after("heal-1", started), []string{"sha1"}, 1))
	assert.NotEqual(t, m1, AutomatedOperationName(after("manual-1", metav1.NewTime(now.Add(time.Minute))), []string{"sha1"}, 1), "a re-created Operation of the same name")
	assert.NotEqual(t, n1, AutomatedOperationName(after("manual-1", started), []string{"sha1"}, 0))

	rapid.Check(t, func(t *rapid.T) {
		name := rapid.StringMatching(`[a-z]([a-z0-9-]{0,250}[a-z0-9])?`).Draw(t, "name")
		app := testApp(func(a *v1alpha1.Application) { a.Name = name })
		got := AutomatedOperationName(app, []string{rapid.String().Draw(t, "rev")}, rapid.Int64Range(0, 100).Draw(t, "attempt"))
		if errs := validation.IsDNS1123Subdomain(got); len(errs) > 0 {
			t.Fatalf("invalid operation name %q: %v", got, errs)
		}
	})
}

// fakeAppRun returns an appRun whose client and API reader are a fake client holding objs.
func fakeAppRun(t *testing.T, objs ...client.Object) (*appRun, client.Client) {
	t.Helper()
	sch := runtime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(sch))
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(objs...).WithStatusSubresource(&v1alpha1.Operation{}).Build()
	r := &Reconciler{cfg: Config{Deps: core.Deps{Client: c, APIReader: c}}, c: c, log: slog.New(slog.DiscardHandler)}
	return &appRun{r: r, app: testApp(), log: r.log}, c
}

func automatedOp(name string) *v1alpha1.Operation {
	return &v1alpha1.Operation{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "patrol"},
		Spec:       v1alpha1.OperationSpec{Application: "guestbook", Sync: &v1alpha1.SyncOperation{Revision: "sha1"}, InitiatedBy: v1alpha1.OperationInitiator{Automated: true}},
	}
}

func TestCreateAutomatedOperation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	at := metav1.NewTime(now)
	withStatus := func(op *v1alpha1.Operation, phase v1alpha1.OperationPhase, started *metav1.Time) *v1alpha1.Operation {
		op.Status = v1alpha1.OperationStatus{Phase: phase, StartedAt: started}
		return op
	}

	t.Run("free name", func(t *testing.T) {
		t.Parallel()
		a, c := fakeAppRun(t)
		created, err := a.createAutomatedOperation(ctx, automatedOp("x-auto-1"))
		require.NoError(t, err)
		assert.True(t, created)
		require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "patrol", Name: "x-auto-1"}, &v1alpha1.Operation{}))
	})
	for _, tc := range []struct {
		name     string
		existing *v1alpha1.Operation
	}{
		{"in progress", withStatus(automatedOp("x-auto-1"), v1alpha1.OperationRunning, &at)},
		{"not processed yet", automatedOp("x-auto-1")},
		{"ran", withStatus(automatedOp("x-auto-1"), v1alpha1.OperationFailed, &at)},
	} {
		t.Run("dedupes against an operation that is "+tc.name, func(t *testing.T) {
			t.Parallel()
			a, c := fakeAppRun(t, tc.existing)
			created, err := a.createAutomatedOperation(ctx, automatedOp("x-auto-1"))
			require.NoError(t, err)
			assert.False(t, created)
			var list v1alpha1.OperationList
			require.NoError(t, c.List(ctx, &list))
			assert.Len(t, list.Items, 1)
		})
	}
	t.Run("suffixes after operations that never ran", func(t *testing.T) {
		t.Parallel()
		a, c := fakeAppRun(t, withStatus(automatedOp("x-auto-1"), v1alpha1.OperationRejected, nil))
		created, err := a.createAutomatedOperation(ctx, automatedOp("x-auto-1"))
		require.NoError(t, err)
		assert.True(t, created)
		require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "patrol", Name: "x-auto-1-1"}, &v1alpha1.Operation{}))
	})
	t.Run("generated name once every suffix is taken", func(t *testing.T) {
		t.Parallel()
		objs := []client.Object{withStatus(automatedOp("x-auto-1"), v1alpha1.OperationRejected, nil)}
		for i := 1; i < maxNameAttempts; i++ {
			objs = append(objs, withStatus(automatedOp(fmt.Sprintf("x-auto-1-%d", i)), v1alpha1.OperationCancelled, nil))
		}
		a, c := fakeAppRun(t, objs...)
		op := automatedOp("x-auto-1")
		created, err := a.createAutomatedOperation(ctx, op)
		require.NoError(t, err)
		assert.True(t, created, "the attempt is never lost")
		assert.True(t, strings.HasPrefix(op.Name, "x-auto-1-"), op.Name)
		var list v1alpha1.OperationList
		require.NoError(t, c.List(ctx, &list))
		assert.Len(t, list.Items, maxNameAttempts+1)
	})
}
