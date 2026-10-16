package operation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/controller/core"
	"github.com/juliosaraiva/patrol-cd/internal/controller/retention"
)

// attemptFailed handles a Failed or Error attempt (SYNC-026/027, APP-047): while the retry
// strategy allows it the Operation stays Running with finishedAt set and the retry time in
// its message; otherwise it completes with " (retried n times)." appended.
func (s *step) attemptFailed(ctx context.Context, phase v1alpha1.OperationPhase, message, reason string) (reconcile.Result, error) {
	r := EffectiveRetry(s.op, s.app)
	if RetryAllowed(r, s.op.Status.RetryCount) {
		now := s.now()
		next := s.op.Status.RetryCount + 1
		retryAt, err := NextRetryAt(r, now.Time, next)
		if err != nil {
			return s.complete(ctx, v1alpha1.OperationError, fmt.Sprintf("%s (failed to retry: %v)", message, err), v1alpha1.OperationReasonInfrastructureError)
		}
		msg := fmt.Sprintf("%s. Retrying attempt #%d at %s.", message, next, retryAt.Format(time.Kitchen))
		if err := s.patch(ctx, func(st *v1alpha1.OperationStatus) {
			st.Phase = v1alpha1.OperationRunning
			st.FinishedAt = &now
			st.RetryCount = next
			st.Message = msg
			s.setCondition(st, v1alpha1.OperationConditionRetrying, metav1.ConditionTrue, v1alpha1.OperationReasonRetryScheduled, msg)
		}); err != nil {
			return reconcile.Result{}, err
		}
		s.log.Info("operation attempt failed; retry scheduled", "phase", phase, "attempt", next, "retryAt", retryAt.Format(time.RFC3339))
		if err := s.updateApplication(ctx, nil); err != nil {
			return reconcile.Result{}, err
		}
		return reconcile.Result{RequeueAfter: max(retryAt.Sub(s.r.cfg.Clock.Now()), requeueSoonAfter)}, nil
	}
	if n := s.op.Status.RetryCount; n > 0 {
		message = fmt.Sprintf("%s (retried %d times).", message, n)
		if reason == v1alpha1.OperationReasonSyncFailed || reason == v1alpha1.OperationReasonInfrastructureError {
			reason = v1alpha1.OperationReasonRetryLimitReached
		}
	}
	return s.complete(ctx, phase, message, reason)
}

// complete moves the Operation to a terminal phase and runs the completion side effects
// (SYNC-077, SYNC-089): history entry (successful, non-dry-run, non-partial syncs),
// Application.status.operationState, OperationCompleted Event, metrics, the post-operation
// refresh and retention.
func (s *step) complete(ctx context.Context, phase v1alpha1.OperationPhase, message, reason string) (reconcile.Result, error) {
	now := s.now()
	if err := s.patch(ctx, func(st *v1alpha1.OperationStatus) {
		st.Phase = phase
		st.Message = message
		st.FinishedAt = &now
		meta.RemoveStatusCondition(&st.Conditions, v1alpha1.OperationConditionRetrying)
		s.setCondition(st, v1alpha1.OperationConditionCompleted, metav1.ConditionTrue, reason, message)
	}); err != nil {
		return reconcile.Result{}, err
	}
	s.log.Info("operation completed", "phase", phase, "message", message, "retries", s.op.Status.RetryCount)
	if s.app == nil {
		return reconcile.Result{}, nil
	}
	if err := s.updateApplication(ctx, s.recordHistory); err != nil {
		return reconcile.Result{}, err
	}
	s.emitCompleted()
	if s.r.cfg.Metrics != nil {
		var d time.Duration
		if s.op.Status.StartedAt != nil {
			d = now.Sub(s.op.Status.StartedAt.Time)
		}
		server := s.app.Spec.Destination.Server
		if p := s.op.Status.Plan; p != nil && p.Destination.Server != "" {
			server = p.Destination.Server
		}
		s.r.cfg.Metrics.ObserveSync(s.app.Namespace, s.app.Name, s.app.Spec.GetProject(), server, string(phase), s.op.IsDryRun(), d)
	}
	if !s.op.IsDryRun() && s.op.Status.StartedAt != nil {
		level := core.CompareForceResolve
		if s.isAutomated() {
			level = core.CompareLatest
		}
		s.r.cfg.Refresher.RequestRefresh(client.ObjectKeyFromObject(s.app), level, 0)
	}
	if _, err := retention.CollectIfNeeded(ctx, s.r.c, s.r.cfg.APIReader, s.app, s.op, s.log); err != nil {
		s.log.Warn("operation retention failed", "error", err)
	}
	return reconcile.Result{}, nil
}

// recordHistory appends the history entry of a successful, non-dry-run, non-partial sync
// once (keyed by the Operation name, so a retried completion does not duplicate it).
func (s *step) recordHistory(app *v1alpha1.Application) {
	op := s.op
	if op.Status.Phase != v1alpha1.OperationSucceeded || op.IsDryRun() || isPartial(op) || op.Status.SyncResult == nil {
		return
	}
	if core.HistoryRecorded(app.Status.History, op.Name) {
		return
	}
	if !summarizes(app.Status.OperationState, op) {
		return
	}
	sr := op.Status.SyncResult
	entry := v1alpha1.RevisionHistory{
		Revision:        sr.Revision,
		Revisions:       sr.Revisions,
		Source:          sr.Source,
		Sources:         sr.Sources,
		DeployStartedAt: op.Status.StartedAt,
		InitiatedBy:     op.Spec.InitiatedBy,
		OperationName:   op.Name,
	}
	if op.Status.FinishedAt != nil {
		entry.DeployedAt = *op.Status.FinishedAt
	} else {
		entry.DeployedAt = s.now()
	}
	app.Status.History = core.AppendHistory(app.Status.History, entry, core.RevisionHistoryLimit(app))
}

func isPartial(op *v1alpha1.Operation) bool {
	return op.Spec.Sync != nil && len(op.Spec.Sync.Resources) > 0
}

// updateApplication writes the operation summary to Application.status.operationState when
// this Operation is the one it should describe, then applies extra.
func (s *step) updateApplication(ctx context.Context, extra func(*v1alpha1.Application)) error {
	if s.app == nil {
		return nil
	}
	_, err := core.PatchStatus(ctx, s.r.c, s.r.cfg.APIReader, s.app, func(a *v1alpha1.Application) error {
		if shouldSummarize(a.Status.OperationState, s.op) {
			a.Status.OperationState = Summary(s.op)
		}
		if extra != nil {
			extra(a)
		}
		return nil
	})
	if errors.Is(err, core.ErrGone) {
		return nil
	}
	return err
}

// shouldSummarize reports whether Application.status.operationState should describe op: the
// summary is empty or already names op, op holds the operation slot, or op started after the
// summarized Operation. Operations that were never admitted are not summarized.
func shouldSummarize(cur *v1alpha1.ApplicationOperationState, op *v1alpha1.Operation) bool {
	if op.Status.StartedAt == nil {
		return false
	}
	if cur == nil || cur.OperationName == op.Name || core.IsActive(op) {
		return true
	}
	return cur.StartedAt == nil || cur.StartedAt.Before(op.Status.StartedAt)
}

func summarizes(cur *v1alpha1.ApplicationOperationState, op *v1alpha1.Operation) bool {
	return cur != nil && cur.OperationName == op.Name
}

// Summary returns the Application.status.operationState of op.
func Summary(op *v1alpha1.Operation) *v1alpha1.ApplicationOperationState {
	out := &v1alpha1.ApplicationOperationState{
		OperationName: op.Name,
		Phase:         op.Status.Phase,
		Message:       truncate(op.Status.Message, 4096),
		StartedAt:     op.Status.StartedAt.DeepCopy(),
		FinishedAt:    op.Status.FinishedAt.DeepCopy(),
		RetryCount:    op.Status.RetryCount,
		InitiatedBy:   op.Spec.InitiatedBy,
	}
	if sr := op.Status.SyncResult; sr != nil && (sr.Revision != "" || len(sr.Revisions) > 0 || sr.Source != nil || len(sr.Sources) > 0) {
		out.SyncResult = &v1alpha1.SyncResultSummary{
			Revision:  sr.Revision,
			Revisions: append([]string(nil), sr.Revisions...),
			Source:    sr.Source.DeepCopy(),
			Sources:   sr.Sources.DeepCopy(),
		}
	}
	return out
}

// emitCompleted records the OperationCompleted Event on the Application (upstream wording).
func (s *step) emitCompleted() {
	op := s.op
	revs := ""
	if sr := op.Status.SyncResult; sr != nil {
		revs = sr.Revision
		if len(sr.Revisions) > 0 {
			revs = strings.Join(sr.Revisions, ",")
		}
	}
	prefix := "Sync operation"
	if op.Spec.Rollback != nil {
		prefix = "Rollback operation"
	}
	if isPartial(op) {
		prefix = "Partial " + strings.ToLower(prefix[:1]) + prefix[1:]
	}
	if op.Status.Phase.IsSuccessful() {
		s.r.cfg.Recorder.Eventf(s.app, op, corev1.EventTypeNormal, core.EventReasonOperationCompleted, core.ActionSync, "%s to %s succeeded", prefix, revs)
		return
	}
	s.r.cfg.Recorder.Eventf(s.app, op, corev1.EventTypeWarning, core.EventReasonOperationCompleted, core.ActionSync, "%s to %s failed: %s", prefix, revs, truncate(op.Status.Message, 1024))
}

// reconcileTerminal keeps a terminal Operation consistent after a crash between its status
// write and the Application write: phase label, operation summary, history entry, retention.
func (s *step) reconcileTerminal(ctx context.Context) (reconcile.Result, error) {
	if err := s.setPhaseLabel(ctx); err != nil {
		return reconcile.Result{}, err
	}
	app := &v1alpha1.Application{}
	if err := s.r.c.Get(ctx, client.ObjectKey{Namespace: s.op.Namespace, Name: s.op.Spec.Application}, app); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	s.app = app
	stale, missingHistory := s.needsRepair(app)
	if stale || missingHistory {
		// The cache may simply lag behind the completion write: decide on a fresh read.
		fresh := &v1alpha1.Application{}
		if err := s.r.cfg.APIReader.Get(ctx, client.ObjectKeyFromObject(app), fresh); err != nil {
			return reconcile.Result{}, client.IgnoreNotFound(err)
		}
		s.app = fresh
		stale, missingHistory = s.needsRepair(fresh)
	}
	if stale || missingHistory {
		s.log.Info("repairing application operation state", "stale", stale, "missingHistory", missingHistory)
		if err := s.updateApplication(ctx, s.recordHistory); err != nil {
			return reconcile.Result{}, err
		}
	}
	// Retention lists the API server only when the cache shows more terminal Operations than
	// the limit, so reconciling every retained Operation (controller start) stays cheap.
	if _, err := retention.CollectIfNeeded(ctx, s.r.c, s.r.cfg.APIReader, s.app, s.op, s.log); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, nil
}

// needsRepair reports whether app's operation summary or history misses this terminal
// Operation's completion.
func (s *step) needsRepair(app *v1alpha1.Application) (stale, missingHistory bool) {
	cur := app.Status.OperationState
	stale = shouldSummarize(cur, s.op) && (cur == nil || cur.OperationName != s.op.Name || cur.Phase != s.op.Status.Phase ||
		!cur.FinishedAt.Equal(s.op.Status.FinishedAt) || cur.RetryCount != s.op.Status.RetryCount)
	missingHistory = s.op.Status.Phase == v1alpha1.OperationSucceeded && !s.op.IsDryRun() && !isPartial(s.op) &&
		summarizes(cur, s.op) && !core.HistoryRecorded(app.Status.History, s.op.Name)
	return stale, missingHistory
}
