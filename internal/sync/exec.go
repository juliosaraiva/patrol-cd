package sync

import (
	"fmt"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/util/retry"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// operationPhaseOf maps a result code to the task phase recorded with it.
func operationPhaseOf(code v1alpha1.ResultCode) v1alpha1.OperationPhase {
	switch code {
	case v1alpha1.ResultCodeSynced:
		return v1alpha1.OperationRunning
	case v1alpha1.ResultCodeSyncFailed:
		return v1alpha1.OperationFailed
	case v1alpha1.ResultCodePruned, v1alpha1.ResultCodePruneSkipped:
		return v1alpha1.OperationSucceeded
	}
	return ""
}

// runTasks executes tasks (SYNC-067): remove finalizers of leftover hooks, prune (after the
// confirmation gate), delete hooks with BeforeHookCreation, then apply in consecutive
// groups of the same kind (groups sequentially, tasks of a group concurrently). The state
// is the worst of successful < pending < failed. Transient errors are remembered with
// noteTransient and leave the task pending.
func (r *run) runTasks(tasks syncTasks, dryRun bool) runState {
	dryRun = dryRun || r.dryRun
	state := stateSuccessful
	pruneTasks, createTasks := tasks.split(func(t *task) bool { return t.isPrune() })

	// Leftover hooks from earlier syncs keep no finalizer, so the sync is idempotent.
	if !dryRun {
		existing := tasks.filter(func(t *task) bool { return t.hook && t.pending() && t.live != nil })
		state = r.parallel(existing, state, func(t *task) runState {
			if err := r.removeHookFinalizer(t); err != nil {
				if IsTransient(err) {
					r.noteTransient(err)
					return statePending
				}
				r.set(t, t.status, v1alpha1.OperationError, fmt.Sprintf("failed to remove hook finalizer: %v", err))
				return stateFailed
			}
			return stateSuccessful
		})
	}
	if state != stateSuccessful {
		return state
	}

	if !r.pruneConfirmed {
		for _, t := range pruneTasks {
			if r.opts.RequiresPruneConfirmation(t.live) {
				r.log.Info("prune requires confirmation", "resource", t.key().String())
				return statePending
			}
		}
	}
	state = r.parallel(pruneTasks, state, func(t *task) runState {
		code, msg := r.pruneObject(t, dryRun)
		switch code {
		case resultTransient:
			return statePending
		case v1alpha1.ResultCodeSyncFailed:
			r.set(t, code, operationPhaseOf(code), msg)
			return stateFailed
		}
		if !dryRun || r.dryRun {
			r.set(t, code, operationPhaseOf(code), msg)
		}
		return stateSuccessful
	})
	if state != stateSuccessful {
		return state
	}

	if deletes := createTasks.filter(func(t *task) bool { return t.deleteBeforeCreation() }); len(deletes) > 0 {
		state = r.parallel(deletes, state, func(t *task) runState {
			if dryRun {
				return stateSuccessful
			}
			if ts := t.live.GetDeletionTimestamp(); ts != nil && !ts.IsZero() {
				return statePending // already being deleted: wait without deleting again
			}
			err := r.deleteResource(t)
			switch {
			case err == nil:
				return statePending // wait until the old hook is gone
			case apierrors.IsNotFound(err):
				t.live = nil // already gone: create it now
				return stateSuccessful
			case IsTransient(err):
				r.noteTransient(err)
				return statePending
			default:
				r.set(t, t.status, v1alpha1.OperationError, err.Error()) // "failed to delete resource: ..."
				return stateFailed
			}
		})
	}
	if state != stateSuccessful {
		return state
	}

	var group syncTasks
	for _, t := range createTasks {
		if len(group) > 0 && group[0].target.GetKind() != t.kind() {
			state = r.processCreateTasks(state, group, dryRun)
			group = syncTasks{t}
			continue
		}
		group = append(group, t)
	}
	if len(group) > 0 {
		state = r.processCreateTasks(state, group, dryRun)
	}
	return state
}

func (r *run) processCreateTasks(state runState, tasks syncTasks, dryRun bool) runState {
	return r.parallel(tasks, state, func(t *task) runState {
		if dryRun && t.skipDryRun {
			if r.dryRun {
				// A dry-run operation can never apply this kind; record what would happen
				// instead of waiting for it forever.
				op := "created"
				if t.live != nil {
					op = "configured"
				}
				r.set(t, v1alpha1.ResultCodeSynced, v1alpha1.OperationSucceeded, opMessage(t.target, op, true))
			}
			return stateSuccessful
		}
		code, msg := r.applyObject(t, dryRun, r.opts.ValidateFor(t.target))
		if code == resultTransient {
			return statePending
		}
		out := stateSuccessful
		if code == v1alpha1.ResultCodeSyncFailed {
			r.log.Info("apply failed", "resource", t.key().String(), "dryRun", dryRun, "message", msg)
			out = stateFailed
		}
		if !dryRun || r.dryRun || code == v1alpha1.ResultCodeSyncFailed {
			phase := operationPhaseOf(code)
			if r.dryRun && phase == v1alpha1.OperationRunning {
				// Nothing is created in a dry run: validation success means success.
				phase = v1alpha1.OperationSucceeded
			}
			r.set(t, code, phase, msg)
		}
		return out
	})
}

// applyObject applies one target (SYNC-041..SYNC-044, SYNC-074).
func (r *run) applyObject(t *task, dryRun, validate bool) (v1alpha1.ResultCode, string) {
	kube := r.e.kube
	if dryRun && (t.nsPending || t.deleteBeforeCreation()) {
		// The server-side dry run must not validate against state this sync replaces: the
		// namespace is created by this sync and does not exist yet (NotFound), or the old hook
		// is deleted before the new one is created (a patch of the old object could fail on
		// immutable fields). Upstream's client dry run sends nothing either.
		return v1alpha1.ResultCodeSynced, opMessage(t.target, "created", true)
	}
	replace := r.opts.ReplaceFor(t.target, t.live)
	force := ForceFor(r.in.strategyForce(), t.target, t.live)
	ssa := r.opts.ServerSideApplyFor(t.target)

	if ssa && !dryRun && r.opts.ClientSideApplyMigration && mayNeedClientSideApplyMigration(t.live, r.csaManager) {
		if err := kube.MigrateClientSideApply(r.ctx, t.live, r.csaManager); err != nil {
			if IsTransient(err) {
				r.noteTransient(err)
				return resultTransient, ""
			}
			return v1alpha1.ResultCodeSyncFailed, fmt.Sprintf("Failed to perform client-side apply migration for %s: %v", keyOf(t.live), err)
		}
	}

	var (
		msg string
		err error
	)
	switch {
	case replace && t.live != nil && (isCRD(t.target) || isNamespaceKind(t.target)):
		// Replacing a CRD would delete all its instances and replacing a namespace its
		// content: update them in place instead.
		u := t.target.DeepCopy()
		u.SetResourceVersion(t.live.GetResourceVersion())
		err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
			_, uerr := kube.Update(r.ctx, u, UpdateOptions{DryRun: dryRun})
			if !apierrors.IsConflict(uerr) {
				return uerr
			}
			fresh, gerr := kube.Get(r.ctx, u.GroupVersionKind(), u.GetNamespace(), u.GetName())
			if gerr != nil {
				return gerr
			}
			u.SetResourceVersion(fresh.GetResourceVersion())
			return uerr
		})
		if err == nil {
			msg = fmt.Sprintf("%s/%s updated", t.target.GetKind(), t.target.GetName())
		} else {
			msg = fmt.Sprintf("error when updating: %v", err)
		}
	case replace && t.live != nil && force:
		// Delete and re-create is not idempotent: once per operation attempt
		// (sync-state-machine.md, "Idempotency and restart recovery").
		var done bool
		if !dryRun {
			done, err = r.doneByThisAttempt(t)
		}
		if done {
			msg = opMessage(t.target, "replaced", false)
		} else if err == nil {
			msg, err = kube.Replace(r.ctx, r.withOperationMarker(t), ReplaceOptions{DryRun: dryRun, Force: true})
		}
	case replace && t.live != nil:
		msg, err = kube.Replace(r.ctx, t.target, ReplaceOptions{DryRun: dryRun})
	case replace:
		msg, err = kube.Create(r.ctx, r.withOperationMarker(t), CreateOptions{DryRun: dryRun, Validate: validate})
		if apierrors.IsAlreadyExists(err) {
			// Created by this attempt in a step whose result was lost, before the live state
			// saw it.
			if done, gerr := r.doneByThisAttempt(t); done {
				msg, err = opMessage(t.target, "created", dryRun), nil
			} else if IsTransient(gerr) {
				err = gerr
			}
		}
	case ssa:
		msg, err = kube.ServerSideApply(r.ctx, t.target, ApplyOptions{DryRun: dryRun, Force: force, Validate: validate})
	default:
		msg, err = kube.ClientSideApply(r.ctx, t.target, ApplyOptions{DryRun: dryRun, Force: force, Validate: validate})
	}
	if err != nil {
		if IsTransient(err) {
			r.noteTransient(err)
			return resultTransient, ""
		}
		if !strings.HasPrefix(msg, "error when updating: ") {
			msg = err.Error()
		}
		return v1alpha1.ResultCodeSyncFailed, msg
	}
	if isCRD(t.target) && !dryRun {
		if err := kube.WaitForCRDEstablished(r.ctx, t.target.GetName(), r.e.cfg.CRDEstablishTimeout); err != nil {
			r.log.Warn("CRD not established in time", "crd", t.target.GetName(), "error", err.Error())
		}
	}
	return v1alpha1.ResultCodeSynced, msg
}

// pruneObject deletes a live object that is no longer desired (SYNC-006, SYNC-030,
// SYNC-039). Objects already being deleted are not deleted again.
func (r *run) pruneObject(t *task, dryRun bool) (v1alpha1.ResultCode, string) {
	if !r.in.Prune {
		return v1alpha1.ResultCodePruneSkipped, "ignored (requires pruning)"
	}
	if r.opts.PruneDisabled(t.live) {
		return v1alpha1.ResultCodePruneSkipped, "ignored (no prune)"
	}
	if dryRun {
		return v1alpha1.ResultCodePruned, "pruned (dry run)"
	}
	if ts := t.live.GetDeletionTimestamp(); ts == nil || ts.IsZero() {
		err := r.e.kube.Delete(r.ctx, t.live.GroupVersionKind(), t.live.GetNamespace(), t.live.GetName(),
			DeleteOptions{PropagationPolicy: r.opts.PrunePropagationPolicy})
		switch {
		case err == nil, apierrors.IsNotFound(err):
		case IsTransient(err):
			r.noteTransient(err)
			return resultTransient, ""
		default:
			return v1alpha1.ResultCodeSyncFailed, err.Error()
		}
	}
	return v1alpha1.ResultCodePruned, "pruned"
}

// deleteResource deletes a hook with the operation's propagation policy.
func (r *run) deleteResource(t *task) error {
	err := r.e.kube.Delete(r.ctx, t.gvk(), t.namespace(), t.name(), DeleteOptions{PropagationPolicy: r.opts.PrunePropagationPolicy})
	if err != nil {
		return fmt.Errorf("failed to delete resource: %w", err)
	}
	return nil
}

// deleteHooks applies a delete policy to completed hooks.
func (r *run) deleteHooks(tasks syncTasks) {
	for _, t := range tasks {
		if err := r.deleteResource(t); err != nil && !apierrors.IsNotFound(err) {
			if IsTransient(err) {
				r.noteTransient(err)
				continue
			}
			r.set(t, t.status, v1alpha1.OperationError, err.Error())
		}
	}
}

// removeHookFinalizer removes the hook finalizer from the live hook, re-reading it on
// update conflicts. A missing object is not an error (SYNC-058).
func (r *run) removeHookFinalizer(t *task) error {
	if t.live == nil || !slices.Contains(t.live.GetFinalizers(), v1alpha1.FinalizerHook) {
		return nil
	}
	current := t.live
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		finalizers := current.GetFinalizers()
		if !slices.Contains(finalizers, v1alpha1.FinalizerHook) {
			t.live = current
			return nil
		}
		obj := current.DeepCopy()
		obj.SetFinalizers(slices.DeleteFunc(slices.Clone(finalizers), func(f string) bool { return f == v1alpha1.FinalizerHook }))
		updated, err := r.e.kube.Update(r.ctx, obj, UpdateOptions{})
		switch {
		case err == nil:
			t.live = updated
			return nil
		case apierrors.IsNotFound(err):
			return nil
		case apierrors.IsConflict(err):
			fresh, gerr := r.e.kube.Get(r.ctx, t.gvk(), t.namespace(), t.name())
			if apierrors.IsNotFound(gerr) {
				return nil
			}
			if gerr != nil {
				return fmt.Errorf("failed to get resource: %w", gerr)
			}
			current = fresh
			return err
		default:
			return fmt.Errorf("failed to update resource: %w", err)
		}
	})
}

// withOperationMarker returns a copy of the target carrying this attempt's
// cd.patrol.example/operation marker, so a restarted step recognizes its own object.
func (r *run) withOperationMarker(t *task) *unstructured.Unstructured {
	u := t.target.DeepCopy()
	ann := u.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann[AnnotationKeyOperation] = operationMarker(r.in.OperationUID, r.in.Attempt, t.phase)
	u.SetAnnotations(ann)
	return u
}

// doneByThisAttempt reports whether the object of t in the cluster was already written by
// this operation attempt (it carries this attempt's marker). It reads the object instead of
// trusting the live state, which may predate a step whose result was lost.
func (r *run) doneByThisAttempt(t *task) (bool, error) {
	fresh, err := r.e.kube.Get(r.ctx, t.gvk(), t.namespace(), t.name())
	switch {
	case err == nil:
		return fresh.GetAnnotations()[AnnotationKeyOperation] == operationMarker(r.in.OperationUID, r.in.Attempt, t.phase), nil
	case apierrors.IsNotFound(err):
		return false, nil
	default:
		return false, err
	}
}

// mayNeedClientSideApplyMigration reports whether the client-side apply migration must be
// attempted before a server-side apply of live: live has fields owned by manager through
// client-side apply, or live carries no managedFields at all (the cluster cache strips them
// unless internal/cache Settings.RetainManagedFields is set), in which case
// Kube.MigrateClientSideApply decides on a fresh read.
func mayNeedClientSideApplyMigration(live *unstructured.Unstructured, manager string) bool {
	if live == nil || manager == "" {
		return false
	}
	return len(live.GetManagedFields()) == 0 || needsClientSideApplyMigration(live, manager)
}

// needsClientSideApplyMigration reports whether live has fields owned by manager through
// client-side apply (operation Update).
func needsClientSideApplyMigration(live *unstructured.Unstructured, manager string) bool {
	if live == nil || manager == "" {
		return false
	}
	for _, f := range live.GetManagedFields() {
		if f.Manager == manager && f.Operation == metav1.ManagedFieldsOperationUpdate {
			return true
		}
	}
	return false
}

// opMessage formats a kubectl-style message: "<kind>[.<group>]/<name> <operation>".
func opMessage(obj *unstructured.Unstructured, operation string, dryRun bool) string {
	gvk := obj.GroupVersionKind()
	kind := strings.ToLower(gvk.Kind)
	if gvk.Group != "" {
		kind += "." + gvk.Group
	}
	msg := kind + "/" + obj.GetName() + " " + operation
	if dryRun {
		msg += " (dry run)"
	}
	return msg
}
