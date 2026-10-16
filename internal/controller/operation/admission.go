package operation

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/controller/core"
	"github.com/juliosaraiva/patrol-cd/internal/project"
)

// admit evaluates the admission rules of a Pending Operation (sync-state-machine.md):
// cancellation, Application deletion, the per-Application operation slot, rollback
// restrictions and sync windows. An admitted Operation becomes Running with startedAt set.
func (s *step) admit(ctx context.Context) (reconcile.Result, error) {
	op, app := s.op, s.app
	if op.Spec.Cancel {
		return s.complete(ctx, v1alpha1.OperationCancelled, msgTerminated, v1alpha1.OperationReasonCancelled)
	}
	if op.Spec.Sync == nil && op.Spec.Rollback == nil {
		return s.reject(ctx, v1alpha1.OperationReasonInvalidSpec, "Invalid operation request: no operation specified")
	}
	if app.DeletionTimestamp != nil {
		return s.reject(ctx, ReasonApplicationDeleting, msgApplicationDeleting)
	}

	// The operation slot: an admitted (Running/Terminating) Operation always holds it; among
	// Pending ones the oldest wins. Read from the API server so a lagging cache cannot admit
	// two Operations.
	ops, err := core.ListOperationsLive(ctx, s.r.cfg.APIReader, op.Namespace, op.Spec.Application)
	if err != nil {
		return reconcile.Result{}, err
	}
	for i := range ops {
		other := &ops[i]
		if other.UID == op.UID || !core.IsNonTerminal(other) {
			continue
		}
		if core.IsActive(other) || core.Older(other, op) {
			s.log.Info("rejecting operation: another operation is in progress", "other", other.Name)
			return s.reject(ctx, v1alpha1.OperationReasonOperationInProgress, msgOperationInProgress)
		}
	}

	if op.Spec.Rollback != nil {
		if app.Spec.IsAutomatedSyncEnabled() {
			return s.reject(ctx, ReasonRollbackNotAllowed, msgRollbackAutoSync)
		}
		entry, ok := core.FindHistory(app.Status.History, op.Spec.Rollback.ID)
		if !ok {
			return s.reject(ctx, v1alpha1.OperationReasonRollbackTargetNotFound,
				fmt.Sprintf("application %s does not have deployment with id %d", app.QualifiedName(), op.Spec.Rollback.ID))
		}
		if entry.Source == nil && len(entry.Sources) == 0 {
			return s.reject(ctx, ReasonRollbackNotAllowed, msgRollbackNoSource)
		}
	}

	// Sync windows. A project that cannot be loaded is reported by the first step.
	st, err := s.r.cfg.CurrentSettings()
	if err != nil {
		return reconcile.Result{}, err
	}
	if proj, perr := s.r.cfg.ResolveProject(ctx, app, st); perr == nil {
		now := s.r.cfg.Clock.Now()
		allowed, werr := project.CanSync(proj, app, !s.isAutomated(), nil, now)
		if werr != nil || !allowed {
			return s.blockedByWindow(ctx, proj, werr)
		}
	}

	now := s.now()
	if err := s.patch(ctx, func(st *v1alpha1.OperationStatus) {
		st.Phase = v1alpha1.OperationRunning
		st.StartedAt = &now
		st.Message = ""
		s.setCondition(st, v1alpha1.OperationConditionAdmitted, metav1.ConditionTrue, v1alpha1.OperationReasonAdmitted, "operation admitted")
	}); err != nil {
		return reconcile.Result{}, err
	}
	s.log.Info("operation admitted", "initiator", op.Spec.InitiatedBy.Username, "automated", s.isAutomated(),
		"dryRun", op.IsDryRun(), "rollback", op.Spec.Rollback != nil)
	if err := s.updateApplication(ctx, nil); err != nil {
		return reconcile.Result{}, err
	}
	return requeueSoon(), nil
}

// blockedByWindow handles a Pending Operation that sync windows do not allow: automated
// Operations are deleted (upstream skips automated syncs during closed windows); manual ones
// stay Pending with Admitted=False/SyncWindowClosed until a window transition.
func (s *step) blockedByWindow(ctx context.Context, proj *v1alpha1.AppProject, werr error) (reconcile.Result, error) {
	msg := msgSyncWindowBlocked
	if werr != nil {
		msg += ": " + werr.Error()
	}
	if s.isAutomated() {
		s.log.Info("deleting automated operation blocked by a sync window")
		uid := s.op.UID
		if err := s.r.c.Delete(ctx, s.op, client.Preconditions{UID: &uid}); client.IgnoreNotFound(err) != nil {
			return reconcile.Result{}, fmt.Errorf("delete automated operation blocked by sync window: %w", err)
		}
		return reconcile.Result{}, nil
	}
	if err := s.patch(ctx, func(st *v1alpha1.OperationStatus) {
		st.Phase = v1alpha1.OperationPending
		st.Message = msg
		s.setCondition(st, v1alpha1.OperationConditionAdmitted, metav1.ConditionFalse, v1alpha1.OperationReasonSyncWindowClosed, msg)
	}); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{RequeueAfter: s.windowRecheck(proj)}, nil
}

// windowRecheck returns when sync windows may change next, bounded by windowRecheck.
func (s *step) windowRecheck(proj *v1alpha1.AppProject) time.Duration {
	now := s.r.cfg.Clock.Now()
	next, ok, err := project.NextSyncWindowTransition(project.MatchSyncWindows(proj.Spec.SyncWindows, s.app), now)
	if err != nil || !ok {
		return windowRecheck
	}
	if d := next.Sub(now) + time.Second; d > 0 && d < windowRecheck {
		return d
	}
	return windowRecheck
}

// reject ends a Pending Operation as Rejected with Admitted=False.
func (s *step) reject(ctx context.Context, reason, message string) (reconcile.Result, error) {
	now := s.now()
	if err := s.patch(ctx, func(st *v1alpha1.OperationStatus) {
		st.Phase = v1alpha1.OperationRejected
		st.Message = message
		st.FinishedAt = &now
		s.setCondition(st, v1alpha1.OperationConditionAdmitted, metav1.ConditionFalse, reason, message)
		s.setCondition(st, v1alpha1.OperationConditionCompleted, metav1.ConditionTrue, reason, message)
	}); err != nil {
		return reconcile.Result{}, err
	}
	s.log.Info("operation rejected", "reason", reason, "message", message)
	if s.app != nil {
		s.r.cfg.Recorder.Eventf(s.op, s.app, corev1.EventTypeWarning, core.EventReasonOperationRejected, core.ActionSync, "%s", message)
	}
	return reconcile.Result{}, nil
}
