package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/compare"
	"github.com/juliosaraiva/patrol-cd/internal/controller/core"
	"github.com/juliosaraiva/patrol-cd/internal/project"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

// specConditionTypes are the condition types spec validation is authoritative for.
var specConditionTypes = []string{v1alpha1.ApplicationConditionInvalidSpecError, v1alpha1.ApplicationConditionUnknownError}

// msgStatusTooLarge is the UnknownError message when the status exceeds the object size limit
// (APP-087).
const msgStatusTooLarge = "Application status exceeds the Kubernetes resource size limit and could not be persisted. " +
	"The displayed status may be stale. Reduce the number of managed resources, set ApplyOutOfSyncOnly=true, " +
	"lower spec.revisionHistoryLimit, or split the Application."

// appRun is the state of one reconcile of one Application.
type appRun struct {
	r       *Reconciler
	app     *v1alpha1.Application
	st      *settings.Settings
	cluster *v1alpha1.Cluster
	destErr error
	log     *slog.Logger
}

func (a *appRun) now() time.Time { return a.r.cfg.Clock.Now() }

// refreshApp decides whether to compare and, if so, compares, auto-syncs and persists.
func (a *appRun) refreshApp(ctx context.Context) (reconcile.Result, error) {
	key := client.ObjectKeyFromObject(a.app)
	requested, tree, wait := a.r.refresh.takeAll(key)
	rec := a.st.Reconciliation()
	d := needRefresh(a.app, rec, requested, a.now())
	if d.level == core.CompareNone {
		out := a.requeue(a.app.Status.ReconciledAt, rec, wait)
		if tree {
			res, err := a.refreshTree(ctx)
			if err != nil {
				return res, err
			}
		}
		return out, nil
	}
	// The cache may not show this controller's latest status write yet; decide again on the
	// object read from the API server, so a stale status neither repeats a comparison nor
	// feeds the automated sync decision.
	fresh := &v1alpha1.Application{}
	if err := a.r.cfg.APIReader.Get(ctx, key, fresh); err != nil {
		a.r.refresh.restore(key, requested)
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if fresh.DeletionTimestamp != nil || core.SkipReconcile(fresh.Annotations) {
		a.r.refresh.restore(key, requested)
		return requeueSoon(), nil
	}
	a.app = fresh
	if d = needRefresh(a.app, rec, requested, a.now()); d.level == core.CompareNone {
		return a.requeue(a.app.Status.ReconciledAt, rec, wait), nil
	}
	start := a.now()
	a.log.Info("refreshing application status", "reason", d.reason, "level", d.level.String(), "hard", d.hard)
	res, err := a.compareAndPersist(ctx, d, rec)
	if a.r.cfg.Metrics != nil {
		server := a.app.Spec.Destination.Server
		if a.cluster != nil {
			server = a.cluster.Spec.Server
		}
		a.r.cfg.Metrics.ReconcileDuration.WithLabelValues(a.app.Namespace, server).Observe(a.r.cfg.Clock.Since(start).Seconds())
	}
	if err != nil && !errors.Is(err, core.ErrGone) {
		a.r.refresh.restore(key, max(requested, d.level))
		return reconcile.Result{}, err
	}
	if wait > 0 && (res.RequeueAfter == 0 || wait < res.RequeueAfter) {
		res.RequeueAfter = wait
	}
	return res, err
}

// requeue returns the periodic refresh delay, or wait when it is earlier.
func (a *appRun) requeue(reconciledAt *metav1.Time, rec settings.Reconciliation, wait time.Duration) reconcile.Result {
	d := nextExpiry(reconciledAt, rec, a.now(), a.r.cfg.Jitter)
	if wait > 0 && (d == 0 || wait < d) {
		d = wait
	}
	return reconcile.Result{RequeueAfter: d}
}

// compareAndPersist is one full refresh (upstream processAppRefreshQueueItem).
func (a *appRun) compareAndPersist(ctx context.Context, d refreshDecision, rec settings.Reconciliation) (reconcile.Result, error) {
	app := a.app
	proj, perr := a.r.cfg.ResolveProject(ctx, app, a.st)
	var issues []project.Issue
	if perr != nil {
		issues = []project.Issue{project.ProjectErrorIssue(perr, app)}
	} else {
		issues = project.ValidateApplication(ctx, app, proj, a.r.cfg.ClusterLookup(a.st), project.ValidateOptions{ControlPlaneNamespace: a.r.cfg.Namespace})
	}
	specConds := make([]compare.Condition, 0, len(issues))
	var msgs []string
	for _, i := range issues {
		specConds = append(specConds, compare.Condition{Type: i.Type, Reason: i.Reason, Message: i.Message})
		msgs = append(msgs, i.Type+": "+i.Message)
	}
	if len(issues) > 0 {
		// Spec errors: no comparison or sync; sync and health are Unknown (APP-033/036), and
		// no orphaned resources are counted.
		a.log.Info("application spec is invalid", "issues", strings.Join(msgs, "; "))
		a.r.cfg.Metrics.ForgetApplication(app.Namespace, app.Name)
		err := a.persist(ctx, d, func(s *v1alpha1.ApplicationStatus, gen int64, now metav1.Time) {
			s.Sync.Status = v1alpha1.SyncStatusCodeUnknown
			setHealth(s, v1alpha1.HealthStatusUnknown, now)
			s.Conditions = compare.MergeConditions(s.Conditions, specConds, specConditionTypes, gen, now)
			s.ObservedGeneration = gen
			setAggregates(s, gen, now, false, strings.Join(msgs, "; "))
		})
		res := reconcile.Result{}
		if rec.Timeout > 0 {
			res.RequeueAfter = rec.Timeout + a.r.cfg.Jitter(rec.Jitter)
		}
		return res, err
	}

	lastOp := a.lastOperation(ctx)
	sources := app.Spec.GetSources()
	in := compare.Input{App: app, Project: proj, NoCache: d.hard, NoRevisionCache: d.level == core.CompareForceResolve}
	if d.level == core.CompareRecent {
		if revs := app.Status.GetRevisions(); len(revs) == len(sources) {
			in.Revisions = slices.Clone(revs)
		}
	}
	if lastOp != nil {
		in.LastSyncResult = lastOp.Status.SyncResult
		if lastOp.Spec.Sync != nil && len(lastOp.Spec.Sync.Manifests) > 0 {
			in.LocalManifests = lastOp.Spec.Sync.Manifests
		}
	}
	res, err := a.r.cfg.Comparer.Compare(ctx, in)
	switch {
	case errors.Is(err, compare.ErrRepoErrorDeferred):
		a.log.Info("ignoring temporary failed attempt to compare app state against repo", "error", err)
		a.r.refresh.restore(client.ObjectKeyFromObject(app), d.level)
		return reconcile.Result{RequeueAfter: repoErrorRetry}, nil
	case err != nil && ctx.Err() != nil:
		return reconcile.Result{}, err
	case err != nil:
		msg := fmt.Sprintf("Failed to compare desired state to live state: %v", err)
		a.log.Warn("comparison failed", "error", err)
		perr := a.persist(ctx, d, func(s *v1alpha1.ApplicationStatus, gen int64, now metav1.Time) {
			s.Sync.Status = v1alpha1.SyncStatusCodeUnknown
			setHealth(s, v1alpha1.HealthStatusUnknown, now)
			s.Conditions = compare.MergeConditions(s.Conditions, nil, specConditionTypes, gen, now)
			s.Conditions = compare.MergeConditions(s.Conditions, []compare.Condition{{Type: v1alpha1.ApplicationConditionComparisonError, Reason: v1alpha1.ReasonComparisonFailed, Message: msg}},
				[]string{v1alpha1.ApplicationConditionComparisonError}, gen, now)
			s.ObservedGeneration = gen
			setAggregates(s, gen, now, false, msg)
		})
		return reconcile.Result{RequeueAfter: repoErrorRetry}, perr
	}

	a.storeComparison(ctx, res)
	if res.Tree != nil {
		a.r.cfg.Metrics.SetOrphanedResources(app.Namespace, app.Name, app.Spec.GetProject(), project.OrphanedResourcesEnabled(proj), res.OrphanedResources)
	}
	if err := a.updateDeleteHookFinalizers(ctx, res); err != nil {
		return reconcile.Result{}, err
	}

	// Automated sync, unless sync windows block it (then SyncError is left untouched).
	var syncErr *compare.Condition
	evaluated := false
	var autoRequeue time.Duration
	if allowed, werr := project.CanSync(proj, app, false, nil, a.now()); werr == nil && allowed {
		evaluated = true
		dec := a.autoSync(ctx, res, lastOp)
		syncErr, autoRequeue = dec.condition, dec.requeueAfter
	} else if app.Spec.IsAutomatedSyncEnabled() {
		a.log.Info("sync prevented by sync window")
	}

	var reconciledAt *metav1.Time
	err = a.persist(ctx, d, func(s *v1alpha1.ApplicationStatus, gen int64, now metav1.Time) {
		s.Sync = res.SyncStatus
		setHealth(s, res.Health, now)
		s.Resources = res.Resources
		s.ResourceHealthSource = res.ResourceHealthSource
		s.SourceType = res.SourceType
		s.SourceTypes = res.SourceTypes
		if res.Tree != nil {
			s.Summary = res.Summary
		}
		if s.ReconciledAt == nil || d.level >= core.CompareLatest {
			t := metav1.NewTime(res.ReconciledAt.Time)
			if t.IsZero() {
				t = now
			}
			s.ReconciledAt = &t
		}
		reconciledAt = s.ReconciledAt
		s.ControllerNamespace = a.r.cfg.Namespace
		s.ObservedGeneration = gen
		s.Conditions = compare.MergeConditions(s.Conditions, nil, specConditionTypes, gen, now)
		s.Conditions = compare.MergeConditions(s.Conditions, res.Conditions, res.EvaluatedConditionTypes, gen, now)
		if evaluated {
			var found []compare.Condition
			if syncErr != nil {
				found = append(found, *syncErr)
			}
			s.Conditions = compare.MergeConditions(s.Conditions, found, []string{v1alpha1.ApplicationConditionSyncError}, gen, now)
		}
		msg := ""
		if res.FailedToLoad {
			msg = strings.Join(res.ConditionMessages(v1alpha1.ApplicationConditionComparisonError), "; ")
		}
		setAggregates(s, gen, now, !res.FailedToLoad, msg)
	})
	if err != nil {
		return reconcile.Result{}, err
	}
	if autoRequeue > 0 {
		// Self-heal backoff: compare again (level latest) once it elapsed (upstream
		// requestAppRefresh with the remaining time).
		a.r.refresh.RequestRefresh(client.ObjectKeyFromObject(app), core.CompareLatest, autoRequeue)
	}
	return a.requeue(reconciledAt, rec, 0), nil
}

// persist writes the status computed by mutate (status subresource, optimistic concurrency,
// only when changed), records sync/health transition Events, and removes a processed refresh
// annotation. A status too large for the API server is replaced by the previous status plus
// an UnknownError condition.
func (a *appRun) persist(ctx context.Context, d refreshDecision, mutate func(s *v1alpha1.ApplicationStatus, gen int64, now metav1.Time)) error {
	before := a.app.Status.DeepCopy()
	now := metav1.NewTime(a.now())
	// The status describes the generation that was compared. When a conflict makes the write
	// land on a newer generation, observedGeneration keeps the compared one, so the newer spec
	// is compared by the next reconcile instead of being marked as observed.
	gen := a.app.Generation
	_, err := core.PatchStatus(ctx, a.r.c, a.r.cfg.APIReader, a.app, func(app *v1alpha1.Application) error {
		mutate(&app.Status, gen, now)
		return nil
	})
	if err != nil && isTooLarge(err) {
		a.log.Error("application status is too large to persist", "error", err)
		_, err = core.PatchStatus(ctx, a.r.c, a.r.cfg.APIReader, a.app, func(app *v1alpha1.Application) error {
			app.Status.Conditions = compare.MergeConditions(app.Status.Conditions,
				[]compare.Condition{{Type: v1alpha1.ApplicationConditionUnknownError, Reason: v1alpha1.ReasonUnknownError, Message: msgStatusTooLarge}},
				nil, app.Generation, now)
			return nil
		})
	}
	if err != nil {
		return err
	}
	a.recordTransitions(before, &a.app.Status)
	if d.annotation {
		if _, err := core.PatchObject(ctx, a.r.c, a.r.cfg.APIReader, a.app, func(app *v1alpha1.Application) error {
			delete(app.Annotations, v1alpha1.AnnotationKeyRefresh)
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func isTooLarge(err error) bool {
	if apierrors.IsRequestEntityTooLargeError(err) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "request is too large") || strings.Contains(msg, "Request entity too large") || strings.Contains(msg, "etcdserver: request is too large")
}

// recordTransitions emits "Updated sync status" and "Updated health status" Events
// (APP-087/141).
func (a *appRun) recordTransitions(before, after *v1alpha1.ApplicationStatus) {
	if before.Sync.Status != after.Sync.Status && after.Sync.Status != "" {
		a.r.cfg.Recorder.Eventf(a.app, nil, corev1.EventTypeNormal, core.EventReasonResourceUpdated, core.ActionReconcile,
			"Updated sync status: %s -> %s", orDash(string(before.Sync.Status)), after.Sync.Status)
	}
	if before.Health.Status != after.Health.Status && after.Health.Status != "" {
		a.r.cfg.Recorder.Eventf(a.app, nil, corev1.EventTypeNormal, core.EventReasonResourceUpdated, core.ActionReconcile,
			"Updated health status: %s -> %s", orDash(string(before.Health.Status)), after.Health.Status)
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// setHealth sets the health status code, updating lastTransitionTime on change.
func setHealth(s *v1alpha1.ApplicationStatus, h v1alpha1.HealthStatusCode, now metav1.Time) {
	if s.Health.Status != h || s.Health.LastTransitionTime == nil {
		t := now
		s.Health.LastTransitionTime = &t
	}
	s.Health.Status = h
}

// setAggregates maintains the Reconciled, Synced and Healthy aggregate conditions.
func setAggregates(s *v1alpha1.ApplicationStatus, gen int64, now metav1.Time, reconciled bool, failure string) {
	set := func(t string, ok bool, reason, msg string) {
		st := metav1.ConditionFalse
		if ok {
			st = metav1.ConditionTrue
		}
		meta.SetStatusCondition(&s.Conditions, metav1.Condition{Type: t, Status: st, Reason: reason, Message: msg, ObservedGeneration: gen, LastTransitionTime: now})
	}
	if reconciled {
		set(v1alpha1.ApplicationConditionReconciled, true, v1alpha1.ReasonReconcileSucceeded, "the latest generation was compared with the live state")
	} else {
		if failure == "" {
			failure = "the comparison failed"
		}
		failure = truncateMessage(failure, 4096)
		set(v1alpha1.ApplicationConditionReconciled, false, v1alpha1.ReasonReconcileFailed, failure)
	}
	switch s.Sync.Status {
	case v1alpha1.SyncStatusCodeSynced:
		set(v1alpha1.ApplicationConditionSynced, true, v1alpha1.ReasonSynced, "live state matches the desired state")
	case v1alpha1.SyncStatusCodeOutOfSync:
		set(v1alpha1.ApplicationConditionSynced, false, v1alpha1.ReasonOutOfSync, "live state differs from the desired state")
	default:
		set(v1alpha1.ApplicationConditionSynced, false, v1alpha1.ReasonSyncStatusUnknown, "the sync status is unknown")
	}
	reason := map[v1alpha1.HealthStatusCode]string{
		v1alpha1.HealthStatusHealthy:     v1alpha1.ReasonHealthy,
		v1alpha1.HealthStatusProgressing: v1alpha1.ReasonProgressing,
		v1alpha1.HealthStatusDegraded:    v1alpha1.ReasonDegraded,
		v1alpha1.HealthStatusSuspended:   v1alpha1.ReasonSuspended,
		v1alpha1.HealthStatusMissing:     v1alpha1.ReasonMissing,
	}[s.Health.Status]
	if reason == "" {
		reason = v1alpha1.ReasonHealthUnknown
	}
	set(v1alpha1.ApplicationConditionHealthy, s.Health.Status == v1alpha1.HealthStatusHealthy, reason, "application health is "+orDash(string(s.Health.Status)))
}

// lastOperation returns the Operation summarized in status.operationState, if it still
// exists.
func (a *appRun) lastOperation(ctx context.Context) *v1alpha1.Operation {
	os := a.app.Status.OperationState
	if os == nil || os.OperationName == "" {
		return nil
	}
	op := &v1alpha1.Operation{}
	key := client.ObjectKey{Namespace: a.app.Namespace, Name: os.OperationName}
	if err := a.r.c.Get(ctx, key, op); err != nil {
		// The Operation cache may not have the Operation the summary names yet.
		if !apierrors.IsNotFound(err) || a.r.cfg.APIReader.Get(ctx, key, op) != nil {
			return nil
		}
	}
	if op.Spec.Application != a.app.Name {
		return nil
	}
	return op
}

// updateDeleteHookFinalizers adds the pre/post-delete finalizers (and their /cleanup stage)
// when the rendered manifests contain PreDelete/PostDelete hooks and removes them when they
// no longer do (APP-064/065). It runs before the status write, so an Application never shows
// a successful comparison without the finalizers its hooks need.
func (a *appRun) updateDeleteHookFinalizers(ctx context.Context, res *compare.Result) error {
	if res.FailedToLoad || a.app.DeletionTimestamp != nil {
		return nil
	}
	_, err := core.PatchObject(ctx, a.r.c, a.r.cfg.APIReader, a.app, func(app *v1alpha1.Application) error {
		app.Finalizers = setFinalizers(app.Finalizers, res.HasPreDeleteHooks, v1alpha1.FinalizerPreDelete, v1alpha1.FinalizerPreDeleteCleanup)
		app.Finalizers = setFinalizers(app.Finalizers, res.HasPostDeleteHooks, v1alpha1.FinalizerPostDelete, v1alpha1.FinalizerPostDeleteCleanup)
		return nil
	})
	return err
}

func setFinalizers(list []string, present bool, names ...string) []string {
	for _, n := range names {
		has := slices.Contains(list, n)
		switch {
		case present && !has:
			list = append(list, n)
		case !present && has:
			list = slices.DeleteFunc(list, func(s string) bool { return s == n })
		}
	}
	return list
}

// requeueSoon re-runs a reconcile shortly (the cache lags behind a change seen through the API).
func requeueSoon() reconcile.Result { return reconcile.Result{RequeueAfter: 100 * time.Millisecond} }

// truncateMessage cuts s to at most n bytes without splitting a UTF-8 sequence.
func truncateMessage(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
