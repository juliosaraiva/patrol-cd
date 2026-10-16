package sync

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/health"
)

// Step executes the next step of a sync operation (at most one (phase, wave) group) and
// returns the new state, mirroring upstream syncContext.Sync:
//
//  1. Plan; fail on shared resources (FailOnSharedResource) or invalid tasks.
//  2. First step only: create the destination namespace if needed, then dry-run every task.
//  3. Assess running tasks (hook health → hook phase, resource health in multi-step syncs)
//     and wait while any runs or a pruned object is still being deleted.
//  4. On a completed-unsuccessful task run SyncFail hooks, then fail.
//  5. Apply the lowest pending (phase, wave): remove stale hook finalizers, prune (after
//     the confirmation gate), delete hooks with BeforeHookCreation, apply by kind group.
//
// The returned error is nil except for invalid input, context cancellation and transient
// API errors (wrapping ErrTransient, with a Running result holding the partial results).
func (e *Engine) Step(ctx context.Context, in *Input) (*StepResult, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}
	r := e.newRun(ctx, in)
	r.log.Debug("sync step", "revision", in.Revision, "dryRun", in.DryRun, "skipHooks", in.skipHooks(), "started", r.started())
	res, err := r.step()
	if cerr := ctx.Err(); cerr != nil {
		return nil, fmt.Errorf("sync step: %w", cerr)
	}
	return res, err
}

func (r *run) finish(phase v1alpha1.OperationPhase, message string) (*StepResult, error) {
	r.setPhase(phase, message)
	return r.result(0), nil
}

func (r *run) wait(message string, requeue time.Duration) (*StepResult, error) {
	r.setPhase(v1alpha1.OperationRunning, message)
	return r.result(requeue), nil
}

func (r *run) step() (*StepResult, error) {
	poll := r.e.cfg.PollInterval
	tasks, out, err := r.buildTasks()
	if err != nil {
		return r.abortTransient(err)
	}
	if out != nil {
		return r.finish(out.phase, out.message)
	}
	if r.sharedMessage != "" {
		return r.finish(v1alpha1.OperationFailed, "Shared resource found: "+r.sharedMessage)
	}
	if !r.valid {
		msg := "one or more synchronization tasks are not valid"
		if msgs := r.results.failedMessages(); len(msgs) > 0 {
			msg = fmt.Sprintf("%s: %s", msg, strings.Join(msgs, "; "))
		}
		return r.finish(v1alpha1.OperationFailed, msg)
	}

	if !r.started() {
		dryRunTasks := tasks
		// The namespace must exist before the dry run of the resources in it.
		if ns := r.namespaceCreationTask(tasks); ns != nil {
			r.log.Info("creating namespace", "namespace", ns.target.GetName())
			state := r.runTasks(syncTasks{ns}, false)
			if err := r.transient(); err != nil {
				return r.abortFirstStep(err)
			}
			if state == stateFailed {
				r.executeSyncFailPhase(nil, syncTasks{ns}, "the namespace failed to apply")
				return r.result(0), nil
			}
			if !r.dryRun {
				r.createdNamespaces[ns.target.GetName()] = true
			}
			dryRunTasks = tasks.filter(func(t *task) bool { return t != ns })
		}
		if r.opts.ApplyOutOfSyncOnly {
			dryRunTasks = dryRunTasks.filter(func(t *task) bool { return !t.outOfSyncOnlySkip })
		}
		r.markPendingNamespaces(tasks)
		state := r.runTasks(dryRunTasks, true)
		if err := r.transient(); err != nil {
			return r.abortFirstStep(err)
		}
		if state == stateFailed {
			return r.finish(v1alpha1.OperationFailed, "one or more objects failed to apply (dry run)")
		}
	}

	// Give other controllers the configured time to react to the last applied wave before
	// its health is assessed (upstream sleeps in the wave hook; the engine requeues).
	if remaining := r.waveDelayRemaining(tasks); remaining > 0 {
		waiting := tasks.filter(func(t *task) bool { return t.running() })
		if len(waiting) > 0 {
			return r.wait(runningMessage(waiting, false, r.pruneConfirmed, r.opts), remaining)
		}
	}

	// Update the state of running tasks from the live objects (prune tasks never run).
	for _, t := range tasks.filter(func(t *task) bool { return t.running() && t.live != nil }) {
		if t.hook {
			phase, msg, err := r.hookPhase(t)
			if err != nil {
				r.set(t, t.status, v1alpha1.OperationError, fmt.Sprintf("failed to get resource health: %v", err))
			} else {
				r.set(t, t.status, phase, msg)
			}
			continue
		}
		hs, err := r.e.health(t.live)
		if err != nil {
			continue
		}
		switch {
		case hs == nil:
			r.set(t, t.status, v1alpha1.OperationSucceeded, t.message)
		case hs.Status == health.StatusHealthy:
			r.set(t, t.status, v1alpha1.OperationSucceeded, hs.Message)
		case hs.Status == health.StatusDegraded:
			r.set(t, t.status, v1alpha1.OperationFailed, hs.Message)
		}
	}

	// Wait while hooks run, or any task runs in a multi-step sync.
	multiStep := tasks.multiStep()
	running := tasks.filter(func(t *task) bool { return (multiStep || t.hook) && t.running() })
	if len(running) > 0 {
		for _, t := range running {
			if t.live != nil {
				continue
			}
			_, err := r.e.kube.Get(r.ctx, t.gvk(), t.namespace(), t.name())
			switch {
			case err == nil:
				continue // created, not yet in the live state
			case IsTransient(err):
				continue // keep waiting
			case !apierrors.IsNotFound(err):
				r.set(t, t.status, v1alpha1.OperationError, fmt.Sprintf("Failed to get live resource %v", err))
				continue
			}
			r.set(t, v1alpha1.ResultCodeSyncFailed, v1alpha1.OperationError,
				fmt.Sprintf("Resource %s/%s/%s is missing, it might have been deleted", t.group(), t.kind(), t.name()))
		}
		return r.wait(runningMessage(running, false, r.pruneConfirmed, r.opts), poll)
	}

	// Wait for pruned objects that are still terminating.
	terminating := tasks.filter(func(t *task) bool {
		return t.pruned() && t.live != nil && t.live.GetDeletionTimestamp() != nil
	})
	if len(terminating) > 0 {
		return r.wait(runningMessage(terminating, true, r.pruneConfirmed, r.opts), poll)
	}

	// Persist a hook's completion before its finalizer is removed or a delete policy
	// deletes it, so a restart never finds a "running" hook gone.
	if !r.dryRun && tasks.any(func(t *task) bool { return t.hook && t.completed() && !t.prevCompleted && t.live != nil }) {
		return r.wait("one or more tasks are running", 0)
	}

	for _, t := range tasks.filter(func(t *task) bool { return t.hook && t.completed() }) {
		if err := r.removeHookFinalizer(t); err != nil {
			if IsTransient(err) {
				return r.abortTransient(err)
			}
			r.set(t, t.status, v1alpha1.OperationError, fmt.Sprintf("Failed to remove hook finalizer: %v", err))
		}
	}
	deleteOnSuccess := tasks.filter(func(t *task) bool { return t.hook && t.live != nil && !t.running() && t.deleteOnPhaseSuccessful() })
	deleteOnFailure := tasks.filter(func(t *task) bool { return t.hook && t.live != nil && !t.running() && t.deleteOnPhaseFailed() })

	syncFailTasks, tasks := tasks.split(func(t *task) bool { return t.phase == v1alpha1.SyncPhaseSyncFail })
	syncFailed := tasks.filter(func(t *task) bool { return t.status == v1alpha1.ResultCodeSyncFailed })

	if tasks.any(func(t *task) bool { return t.completed() && !t.successful() }) {
		r.deleteHooks(deleteOnFailure)
		r.executeSyncFailPhase(syncFailTasks, syncFailed, "one or more synchronization tasks completed unsuccessfully")
		if err := r.transient(); err != nil {
			return r.abortTransient(err)
		}
		return r.result(poll), nil
	}

	pending := tasks.filter(func(t *task) bool { return t.pending() })
	if r.opts.ApplyOutOfSyncOnly {
		pending = pending.filter(func(t *task) bool { return !t.outOfSyncOnlySkip })
	}
	if len(pending) == 0 {
		r.deleteHooks(deleteOnSuccess)
		return r.finish(v1alpha1.OperationSucceeded, "successfully synced (no more tasks)")
	}

	phase, wave := pending.phase(), pending.wave()
	final := phase == pending.lastPhase() && wave == pending.lastWave()
	waveTasks, remaining := pending.split(func(t *task) bool { return t.phase == phase && t.wave() == wave })
	r.setPhase(v1alpha1.OperationRunning, "one or more tasks are running")
	r.markPendingNamespaces(tasks)
	r.log.Debug("running wave", "phase", phase, "wave", wave, "tasks", len(waveTasks), "final", final)
	state := r.runTasks(waveTasks, false)
	if err := r.transient(); err != nil {
		return r.abortTransient(err)
	}
	if state != stateFailed {
		// Like upstream's wave hook, also for a pending wave.
		r.applied = &waveID{phase: phase, wave: wave}
	}
	// A pending wave (prune confirmation, deletion before hook creation) is polled; only an
	// applied non-final wave requeues after the wave delay, which may be zero: that step made
	// progress, so an immediate requeue cannot loop.
	requeue := poll
	if state == stateSuccessful && !final {
		requeue = r.e.cfg.WaveDelay
	}

	switch state {
	case stateFailed:
		failed := waveTasks.filter(func(t *task) bool { return t.status == v1alpha1.ResultCodeSyncFailed })
		runningHooks := waveTasks.filter(func(t *task) bool { return t.running() })
		if len(runningHooks) > 0 {
			if len(syncFailTasks) > 0 {
				if done := r.executeSyncFailPhase(syncFailTasks, failed, "one or more objects failed to apply"); !done {
					runningHooks = append(runningHooks, syncFailTasks...)
				}
			}
			r.setPhase(v1alpha1.OperationRunning, runningMessage(runningHooks, false, r.pruneConfirmed, r.opts))
		} else if done := r.executeSyncFailPhase(syncFailTasks, failed, "one or more objects failed to apply"); done {
			r.deleteHooks(deleteOnFailure)
		}
	case stateSuccessful:
		if len(remaining) == 0 && !waveTasks.any(func(t *task) bool { return t.hook }) {
			// Last wave without hooks: an apply equates to success even if the resources
			// later degrade.
			r.deleteHooks(deleteOnSuccess)
			r.setPhase(v1alpha1.OperationSucceeded, "successfully synced (all tasks run)")
		} else {
			r.setPhase(v1alpha1.OperationRunning, runningMessage(waveTasks, false, r.pruneConfirmed, r.opts))
		}
	default:
		r.setPhase(v1alpha1.OperationRunning, runningMessage(waveTasks.filter(func(t *task) bool {
			return t.deleteBeforeCreation() || (t.isPrune() && t.pending())
		}), true, r.pruneConfirmed, r.opts))
	}
	if err := r.transient(); err != nil {
		return r.abortTransient(err)
	}
	return r.result(requeue), nil
}

// waveDelayRemaining returns how long the last applied, non-final wave still has to wait
// before its health may be assessed.
func (r *run) waveDelayRemaining(tasks syncTasks) time.Duration {
	delay := r.e.cfg.WaveDelay
	p := r.in.Progress
	if delay <= 0 || p == nil || p.StartedAt == nil {
		return 0
	}
	remaining := p.StartedAt.Add(delay).Sub(r.now)
	if remaining <= 0 {
		return 0
	}
	nonSyncFail := tasks.filter(func(t *task) bool { return t.phase != v1alpha1.SyncPhaseSyncFail })
	if len(nonSyncFail) == 0 {
		return 0
	}
	if p.SyncPhase == nonSyncFail.lastPhase() && p.Wave == int64(nonSyncFail.lastWave()) {
		return 0 // no delay after the final wave
	}
	return remaining
}

// hookPhase derives a hook's phase from the health of its live object (SYNC-059):
// Unknown/Degraded → Failed, Progressing/Suspended → Running, Healthy → Succeeded, no health
// check → Succeeded "<name> created".
func (r *run) hookPhase(t *task) (v1alpha1.OperationPhase, string, error) {
	phase := v1alpha1.OperationSucceeded
	msg := t.live.GetName() + " created"
	hs, err := r.e.health(t.live)
	if err != nil {
		return "", "", err
	}
	if hs != nil {
		switch hs.Status {
		case health.StatusUnknown, health.StatusDegraded:
			phase, msg = v1alpha1.OperationFailed, hs.Message
		case health.StatusProgressing, health.StatusSuspended:
			phase, msg = v1alpha1.OperationRunning, hs.Message
		case health.StatusHealthy:
			phase, msg = v1alpha1.OperationSucceeded, hs.Message
		}
	}
	return phase, msg, nil
}

// executeSyncFailPhase fails the operation, first running the SyncFail hooks (SYNC-049).
// It returns true when the phase is final (Failed).
func (r *run) executeSyncFailPhase(syncFailTasks, failedTasks syncTasks, message string) bool {
	errMsg := messageWithReasons(failedTasks, message)
	if len(syncFailTasks) == 0 {
		r.setPhase(v1alpha1.OperationFailed, errMsg)
		return true
	}
	if syncFailTasks.all(func(t *task) bool { return t.completed() }) {
		if failedHooks := syncFailTasks.filter(func(t *task) bool { return t.status == v1alpha1.ResultCodeSyncFailed }); len(failedHooks) > 0 {
			errMsg = errMsg + "\n" + messageWithReasons(failedHooks, "one or more SyncFail hooks failed")
		}
		r.setPhase(v1alpha1.OperationFailed, errMsg)
		return true
	}
	pendingHooks := syncFailTasks.filter(func(t *task) bool { return !t.completed() && !t.running() })
	r.log.Info("running SyncFail hooks", "count", len(pendingHooks))
	r.runTasks(pendingHooks, false)
	waiting := pendingHooks
	if len(waiting) == 0 {
		waiting = syncFailTasks.filter(func(t *task) bool { return !t.completed() })
	}
	r.setPhase(v1alpha1.OperationRunning, runningMessage(waiting, false, r.pruneConfirmed, r.opts))
	return false
}

// messageWithReasons appends the distinct task messages: "<message>, reason: a,b".
func messageWithReasons(tasks syncTasks, message string) string {
	seen := map[string]bool{}
	var msgs []string
	for _, t := range tasks {
		if t.message != "" && !seen[t.message] {
			seen[t.message] = true
			msgs = append(msgs, t.message)
		}
	}
	if len(msgs) == 0 {
		return message
	}
	slices.Sort(msgs)
	return fmt.Sprintf("%s, reason: %s", message, strings.Join(msgs, ","))
}

// Terminate stops a running operation (SYNC-078): finalizers of completed hooks are
// removed, running hooks are deleted (as are completed hooks with HookFailed), and their
// last known state is recorded. Applied resources are not rolled back and SyncFail hooks do
// not run. The phase is Cancelled "Operation terminated", or Error "Operation termination
// had errors".
func (e *Engine) Terminate(ctx context.Context, in *Input) (*StepResult, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}
	r := e.newRun(ctx, in)
	r.terminating = true
	tasks, out, err := r.buildTasks()
	if err != nil {
		return r.abortTransient(err)
	}
	if out != nil {
		return r.finish(v1alpha1.OperationError, out.message)
	}
	for _, t := range tasks.filter(func(t *task) bool { return t.hook && t.completed() }) {
		if err := r.removeHookFinalizer(t); err != nil {
			r.set(t, t.status, v1alpha1.OperationError, fmt.Sprintf("Failed to remove hook finalizer: %v", err))
		}
	}
	if r.terminateHooks(tasks.filter(func(t *task) bool { return t.hook })) {
		r.setPhase(v1alpha1.OperationCancelled, "Operation terminated")
	} else {
		r.setPhase(v1alpha1.OperationError, "Operation termination had errors")
	}
	res := r.result(0)
	if cerr := ctx.Err(); cerr != nil {
		return nil, fmt.Errorf("terminate: %w", cerr)
	}
	return res, nil
}

// terminateHooks deletes running hooks (and completed HookFailed hooks), recording their
// real terminal state or "Terminated". It reports whether everything succeeded.
func (r *run) terminateHooks(hooks syncTasks) bool {
	ok := true
	for _, t := range hooks {
		if !t.running() {
			continue
		}
		if t.live == nil {
			live, err := r.e.kube.Get(r.ctx, t.gvk(), t.namespace(), t.name())
			if err != nil && !apierrors.IsNotFound(err) {
				r.set(t, t.status, v1alpha1.OperationError, fmt.Sprintf("Failed to get live resource: %v", err))
				ok = false
				continue
			}
			if err == nil {
				t.live = live
			}
		}
		if t.live == nil {
			r.set(t, t.status, v1alpha1.OperationSucceeded, "Terminated")
			continue
		}
		phase, msg, statusErr := r.hookPhase(t)
		if statusErr != nil {
			r.set(t, t.status, v1alpha1.OperationError, fmt.Sprintf("Failed to get hook health: %v", statusErr))
		}
		if err := r.removeHookFinalizer(t); err != nil {
			r.set(t, t.status, v1alpha1.OperationError, fmt.Sprintf("Failed to remove hook finalizer: %v", err))
			ok = false
			continue
		}
		if statusErr != nil || phase == v1alpha1.OperationRunning || t.deleteOnPhaseFailed() {
			if err := r.deleteResource(t); err != nil && !apierrors.IsNotFound(err) {
				r.set(t, t.status, v1alpha1.OperationFailed, fmt.Sprintf("Failed to delete: %v", err))
				ok = false
				continue
			}
		}
		switch {
		case statusErr == nil && isCompleted(phase):
			r.set(t, t.status, phase, msg)
		case t.opState != v1alpha1.OperationError:
			r.set(t, t.status, v1alpha1.OperationFailed, "Terminated")
		}
	}
	return ok
}
