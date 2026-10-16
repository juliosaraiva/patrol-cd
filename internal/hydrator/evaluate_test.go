package hydrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
)

// APP-114, APP-115, APP-130, APP-133: the hydration need decision, in upstream order.
func TestEvaluate(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 16, 10, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *metav1.Time { t := metav1.NewTime(now.Add(-d)); return &t }
	op := func(app *v1alpha1.Application, phase v1alpha1.HydrateOperationPhase, finished *metav1.Time) {
		app.Status.SourceHydrator.CurrentOperation = &v1alpha1.HydrateOperation{
			Phase: phase, StartedAt: at(time.Hour), FinishedAt: finished, SourceHydrator: app.Spec.SourceHydrator.DeepCopy(),
		}
	}
	cases := []struct {
		name     string
		setup    func(app *v1alpha1.Application, repo *fakeRepoServer)
		needed   bool
		hard     bool
		reason   string
		resolved string
		err      bool
		resolves int
	}{
		{name: "no operation", setup: func(*v1alpha1.Application, *fakeRepoServer) {}, needed: true, reason: reasonNoOperation},
		{name: "hydrating", setup: func(a *v1alpha1.Application, _ *fakeRepoServer) {
			op(a, v1alpha1.HydrateOperationPhaseHydrating, nil)
			a.Annotations = map[string]string{v1alpha1.AnnotationKeyHydrate: "hard"}
		}, reason: reasonInProgress},
		{name: "hard request", setup: func(a *v1alpha1.Application, _ *fakeRepoServer) {
			op(a, v1alpha1.HydrateOperationPhaseHydrated, at(time.Minute))
			a.Status.SourceHydrator.LastComparedDryRevision = shaA
			a.Annotations = map[string]string{v1alpha1.AnnotationKeyHydrate: "hard"}
		}, needed: true, hard: true, reason: reasonHardRequested},
		{name: "spec differs", setup: func(a *v1alpha1.Application, _ *fakeRepoServer) {
			op(a, v1alpha1.HydrateOperationPhaseHydrated, at(time.Minute))
			a.Status.SourceHydrator.CurrentOperation.SourceHydrator.SyncSource.Path = "old"
		}, needed: true, reason: reasonSpecDiffers},
		{name: "failed, explicit request", setup: func(a *v1alpha1.Application, _ *fakeRepoServer) {
			op(a, v1alpha1.HydrateOperationPhaseFailed, at(time.Second))
			a.Annotations = map[string]string{v1alpha1.AnnotationKeyHydrate: "normal"}
		}, needed: true, reason: reasonRetryFailed},
		{name: "failed, any value is a normal request", setup: func(a *v1alpha1.Application, _ *fakeRepoServer) {
			op(a, v1alpha1.HydrateOperationPhaseFailed, at(time.Second))
			a.Annotations = map[string]string{v1alpha1.AnnotationKeyHydrate: "please"}
		}, needed: true, reason: reasonRetryFailed},
		{name: "failed long ago", setup: func(a *v1alpha1.Application, _ *fakeRepoServer) {
			op(a, v1alpha1.HydrateOperationPhaseFailed, at(3*time.Minute))
		}, needed: true, reason: reasonFailedLongAgo},
		{name: "failed recently without baseline", setup: func(a *v1alpha1.Application, _ *fakeRepoServer) {
			op(a, v1alpha1.HydrateOperationPhaseFailed, at(time.Minute))
		}, reason: reasonFailedWait},
		{name: "failed without finish time counts as recent", setup: func(a *v1alpha1.Application, _ *fakeRepoServer) {
			op(a, v1alpha1.HydrateOperationPhaseFailed, nil)
		}, reason: reasonFailedWait},
		{name: "failed recently, unchanged revision", setup: func(a *v1alpha1.Application, _ *fakeRepoServer) {
			op(a, v1alpha1.HydrateOperationPhaseFailed, at(time.Minute))
			a.Status.SourceHydrator.LastComparedDryRevision = shaA
		}, reason: reasonNotNeeded, resolved: shaA, resolves: 1},
		{name: "failed recently, new revision retries now", setup: func(a *v1alpha1.Application, r *fakeRepoServer) {
			op(a, v1alpha1.HydrateOperationPhaseFailed, at(time.Minute))
			a.Status.SourceHydrator.LastComparedDryRevision = shaA
			r.rev = shaB
		}, needed: true, reason: reasonNewRevision, resolved: shaB, resolves: 1},
		{name: "no compared revision", setup: func(a *v1alpha1.Application, _ *fakeRepoServer) {
			op(a, v1alpha1.HydrateOperationPhaseHydrated, at(time.Minute))
		}, needed: true, reason: reasonNoComparedRev},
		{name: "unchanged revision", setup: func(a *v1alpha1.Application, _ *fakeRepoServer) {
			op(a, v1alpha1.HydrateOperationPhaseHydrated, at(time.Minute))
			a.Status.SourceHydrator.LastComparedDryRevision = shaA
		}, reason: reasonNotNeeded, resolved: shaA, resolves: 1},
		{name: "new revision", setup: func(a *v1alpha1.Application, r *fakeRepoServer) {
			op(a, v1alpha1.HydrateOperationPhaseHydrated, at(time.Minute))
			a.Status.SourceHydrator.LastComparedDryRevision = shaA
			r.rev = shaB
		}, needed: true, reason: reasonNewRevision, resolved: shaB, resolves: 1},
		{name: "pinned SHA equal to the compared revision", setup: func(a *v1alpha1.Application, _ *fakeRepoServer) {
			a.Spec.SourceHydrator.DrySource.TargetRevision = shaA
			op(a, v1alpha1.HydrateOperationPhaseHydrated, at(time.Minute))
			a.Status.SourceHydrator.LastComparedDryRevision = shaA
		}, reason: reasonNotNeeded, resolved: shaA},
		{name: "resolution error", setup: func(a *v1alpha1.Application, r *fakeRepoServer) {
			op(a, v1alpha1.HydrateOperationPhaseHydrated, at(time.Minute))
			a.Status.SourceHydrator.LastComparedDryRevision = shaA
			r.resolveErr = errors.New("ls-remote failed")
		}, reason: reasonUnknown, err: true, resolves: 1},
		{name: "manifest-generate-paths without changes", setup: func(a *v1alpha1.Application, r *fakeRepoServer) {
			op(a, v1alpha1.HydrateOperationPhaseHydrated, at(time.Minute))
			a.Status.SourceHydrator.LastComparedDryRevision = shaA
			a.Annotations = map[string]string{v1alpha1.AnnotationKeyManifestGeneratePaths: ".;/shared"}
			r.pathsResp = &apiclient.UpdateRevisionForPathsResponse{Changes: false, Revision: shaB}
		}, reason: reasonNotNeeded, resolved: shaB},
		{name: "manifest-generate-paths with changes", setup: func(a *v1alpha1.Application, r *fakeRepoServer) {
			op(a, v1alpha1.HydrateOperationPhaseHydrated, at(time.Minute))
			a.Status.SourceHydrator.LastComparedDryRevision = shaA
			a.Annotations = map[string]string{v1alpha1.AnnotationKeyManifestGeneratePaths: "."}
			r.pathsResp = &apiclient.UpdateRevisionForPathsResponse{Changes: true, Revision: shaB}
		}, needed: true, reason: reasonNewRevision, resolved: shaB},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := hydratorApp("a", "out/a")
			e := newTestEnv(t, defaultProject("default"))
			e.now = now
			tc.setup(app, e.repo)
			d := e.h.evaluate(context.Background(), app)
			assert.Equal(t, tc.needed, d.needed)
			assert.Equal(t, tc.hard, d.hard)
			assert.Equal(t, tc.reason, d.reason)
			assert.Equal(t, tc.resolved, d.resolved)
			assert.Equal(t, tc.err, d.err != nil, "error: %v", d.err)
			_, resolves := e.repo.counts()
			assert.Equal(t, tc.resolves, resolves)
		})
	}
}

// APP-133: manifest-generate-paths entries are resolved against the dry source path and the
// previous compared revision is the synced revision.
func TestEvaluateManifestGeneratePathsRequest(t *testing.T) {
	t.Parallel()
	app := hydratorApp("a", "out/a")
	app.Annotations = map[string]string{v1alpha1.AnnotationKeyManifestGeneratePaths: ".;../common;/shared/base"}
	app.Status.SourceHydrator.LastComparedDryRevision = shaA
	app.Status.SourceHydrator.CurrentOperation = &v1alpha1.HydrateOperation{Phase: v1alpha1.HydrateOperationPhaseHydrated, SourceHydrator: app.Spec.SourceHydrator.DeepCopy()}
	e := newTestEnv(t, defaultProject("default"))
	e.repo.pathsResp = &apiclient.UpdateRevisionForPathsResponse{Changes: false, Revision: shaB}
	d := e.h.evaluate(context.Background(), app)
	require.NoError(t, d.err)
	require.Len(t, e.repo.pathsReqs, 1)
	r := e.repo.pathsReqs[0]
	assert.Equal(t, []string{"dry/a", "dry/common", "shared/base"}, r.Paths)
	assert.Equal(t, shaA, r.SyncedRevision)
	assert.Equal(t, "main", r.Revision)
	assert.Equal(t, "a", r.AppName)
	assert.Equal(t, "dry/a", r.Source.Path)
}

// APP-115: an explicit request bypasses the revision cache; the annotation is consumed and
// the key enqueued.
func TestReconcileAppConsumesHydrateRequest(t *testing.T) {
	t.Parallel()
	app := hydratorApp("a", "out/a")
	app.Annotations = map[string]string{v1alpha1.AnnotationKeyHydrate: "normal"}
	app.Status.SourceHydrator.LastComparedDryRevision = shaA
	app.Status.SourceHydrator.CurrentOperation = &v1alpha1.HydrateOperation{Phase: v1alpha1.HydrateOperationPhaseHydrated, SourceHydrator: app.Spec.SourceHydrator.DeepCopy()}
	e := newTestEnv(t, defaultProject("default"), app)
	e.repo.rev = shaB

	res, err := e.h.ReconcileApp(testCtx(t), reconcile.Request{NamespacedName: types.NamespacedName{Namespace: controlNS, Name: "a"}})
	require.NoError(t, err)
	assert.Equal(t, 2*time.Minute, res.RequeueAfter, "settings default timeout.reconciliation")
	require.Len(t, e.repo.resolveReqs, 1)
	assert.True(t, e.repo.resolveReqs[0].ForceRefresh)

	got := e.get(t, "a")
	assert.NotContains(t, got.Annotations, v1alpha1.AnnotationKeyHydrate)
	assert.Equal(t, shaA, got.Status.SourceHydrator.LastComparedDryRevision, "recorded by the hydration, not the evaluation")
	assert.Equal(t, []Key{keyOf(t, app)}, e.h.queue.pendingKeys())
	_, forced := e.h.forced.Load(keyOf(t, app))
	assert.False(t, forced)
}

// APP-114: an unchanged revision is not hydrated but lastComparedDryRevision is refreshed
// (here: a commit outside manifest-generate-paths); a hard request marks the key forced and
// a request while Hydrating is consumed without effect.
func TestReconcileAppRecordsComparedRevision(t *testing.T) {
	t.Parallel()
	req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: controlNS, Name: "a"}}

	t.Run("not needed", func(t *testing.T) {
		t.Parallel()
		app := hydratorApp("a", "out/a")
		app.Annotations = map[string]string{v1alpha1.AnnotationKeyManifestGeneratePaths: "."}
		app.Status.SourceHydrator.LastComparedDryRevision = shaA
		app.Status.SourceHydrator.CurrentOperation = &v1alpha1.HydrateOperation{Phase: v1alpha1.HydrateOperationPhaseHydrated, SourceHydrator: app.Spec.SourceHydrator.DeepCopy()}
		e := newTestEnv(t, defaultProject("default"), app)
		e.repo.pathsResp = &apiclient.UpdateRevisionForPathsResponse{Revision: shaB}
		_, err := e.h.ReconcileApp(testCtx(t), req)
		require.NoError(t, err)
		assert.Equal(t, shaB, e.get(t, "a").Status.SourceHydrator.LastComparedDryRevision)
		assert.Empty(t, e.h.queue.pendingKeys())
	})
	t.Run("hard", func(t *testing.T) {
		t.Parallel()
		app := hydratorApp("a", "out/a")
		app.Annotations = map[string]string{v1alpha1.AnnotationKeyHydrate: "hard"}
		app.Status.SourceHydrator.LastComparedDryRevision = shaA
		app.Status.SourceHydrator.CurrentOperation = &v1alpha1.HydrateOperation{Phase: v1alpha1.HydrateOperationPhaseHydrated, SourceHydrator: app.Spec.SourceHydrator.DeepCopy()}
		e := newTestEnv(t, defaultProject("default"), app)
		_, err := e.h.ReconcileApp(testCtx(t), req)
		require.NoError(t, err)
		_, forced := e.h.forced.Load(keyOf(t, app))
		assert.True(t, forced)
		assert.NotContains(t, e.get(t, "a").Annotations, v1alpha1.AnnotationKeyHydrate)
	})
	t.Run("while hydrating", func(t *testing.T) {
		t.Parallel()
		app := hydratorApp("a", "out/a")
		app.Annotations = map[string]string{v1alpha1.AnnotationKeyHydrate: "hard"}
		started := metav1.NewTime(time.Date(2026, 10, 16, 9, 59, 0, 0, time.UTC))
		app.Status.SourceHydrator.CurrentOperation = &v1alpha1.HydrateOperation{Phase: v1alpha1.HydrateOperationPhaseHydrating, StartedAt: &started, SourceHydrator: app.Spec.SourceHydrator.DeepCopy()}
		e := newTestEnv(t, defaultProject("default"), app)
		res, err := e.h.ReconcileApp(testCtx(t), req)
		require.NoError(t, err)
		assert.Empty(t, e.h.queue.pendingKeys())
		assert.NotContains(t, e.get(t, "a").Annotations, v1alpha1.AnnotationKeyHydrate)
		assert.Equal(t, 61*time.Second, res.RequeueAfter, "re-evaluated when the operation becomes stale")
	})
}

// APP-131: a Hydrating operation older than the refresh timeout is re-enqueued.
func TestReconcileAppRecoversStaleHydrating(t *testing.T) {
	t.Parallel()
	app := hydratorApp("a", "out/a")
	started := metav1.NewTime(time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC))
	app.Status.SourceHydrator.CurrentOperation = &v1alpha1.HydrateOperation{Phase: v1alpha1.HydrateOperationPhaseHydrating, StartedAt: &started, SourceHydrator: app.Spec.SourceHydrator.DeepCopy()}
	e := newTestEnv(t, defaultProject("default"), app)
	_, err := e.h.ReconcileApp(testCtx(t), reconcile.Request{NamespacedName: types.NamespacedName{Namespace: controlNS, Name: "a"}})
	require.NoError(t, err)
	assert.Equal(t, []Key{keyOf(t, app)}, e.h.queue.pendingKeys())
}

func TestReconcileAppIgnoresOtherApps(t *testing.T) {
	t.Parallel()
	plain := &v1alpha1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: "plain", Namespace: controlNS},
		Spec: v1alpha1.ApplicationSpec{
			Source:      &v1alpha1.ApplicationSource{RepoURL: dryRepo, Path: "x"},
			Destination: v1alpha1.ApplicationDestination{Server: "https://kubernetes.default.svc"},
		},
	}
	e := newTestEnv(t, plain)
	for _, name := range []string{"plain", "missing"} {
		res, err := e.h.ReconcileApp(testCtx(t), reconcile.Request{NamespacedName: types.NamespacedName{Namespace: controlNS, Name: name}})
		require.NoError(t, err)
		assert.Zero(t, res)
	}
	assert.Empty(t, e.h.queue.pendingKeys())
}

// APP-130: a failed operation is re-evaluated when its cooldown ends.
func TestRequeueAfter(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	now := e.now
	app := hydratorApp("a", "out/a")
	assert.Equal(t, 2*time.Minute, e.h.requeueAfter(app))

	finished := metav1.NewTime(now.Add(-90 * time.Second))
	app.Status.SourceHydrator.CurrentOperation = &v1alpha1.HydrateOperation{Phase: v1alpha1.HydrateOperationPhaseFailed, FinishedAt: &finished}
	assert.Equal(t, 31*time.Second, e.h.requeueAfter(app))

	e.h.d.PollInterval = 10 * time.Second
	assert.Equal(t, 10*time.Second, e.h.requeueAfter(app))

	e.h.d.PollInterval = -1
	assert.Equal(t, 31*time.Second, e.h.requeueAfter(app), "cooldown requeue even without polling")
	app.Status.SourceHydrator.CurrentOperation = nil
	assert.Zero(t, e.h.requeueAfter(app))
}

func TestAppPredicate(t *testing.T) {
	t.Parallel()
	p := appPredicate(func(a *v1alpha1.Application) bool { return a.Spec.SourceHydrator != nil })
	base := hydratorApp("a", "out/a")
	base.Generation = 1
	plain := base.DeepCopy()
	plain.Spec.SourceHydrator = nil

	assert.True(t, p.Create(event.CreateEvent{Object: base}))
	assert.False(t, p.Create(event.CreateEvent{Object: plain}))
	assert.False(t, p.Delete(event.DeleteEvent{Object: base}))

	update := func(mutate func(a *v1alpha1.Application)) bool {
		n := base.DeepCopy()
		mutate(n)
		return p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: n})
	}
	assert.False(t, update(func(a *v1alpha1.Application) { a.Status.Sync.Status = v1alpha1.SyncStatusCodeSynced }), "unrelated status")
	assert.False(t, update(func(a *v1alpha1.Application) {
		a.Annotations = map[string]string{v1alpha1.AnnotationKeyRefresh: "normal"}
	}))
	assert.True(t, update(func(a *v1alpha1.Application) { a.Generation = 2 }))
	assert.True(t, update(func(a *v1alpha1.Application) {
		a.Annotations = map[string]string{v1alpha1.AnnotationKeyHydrate: "normal"}
	}))
	assert.True(t, update(func(a *v1alpha1.Application) {
		a.Annotations = map[string]string{v1alpha1.AnnotationKeyManifestGeneratePaths: "."}
	}))
	assert.True(t, update(func(a *v1alpha1.Application) { a.Status.SourceHydrator.LastComparedDryRevision = shaA }))
}
