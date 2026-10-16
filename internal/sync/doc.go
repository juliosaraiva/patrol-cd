// Package sync is Patrol CD's sync planning and execution engine.
//
// It re-implements the observable behavior of the Argo CD v3.5.5 sync engine
// (gitops-engine pkg/sync and controller/sync.go) for Operation objects that are driven by an
// external, re-entrant reconciler (docs/architecture/sync-state-machine.md):
//
//   - [Engine.Plan] computes the ordered task list of an operation (apply, prune and hook
//     tasks with phase, wave, kind order and name), validates it against API discovery and
//     project permissions, and reports destructive-change warnings, pending prune
//     confirmations and a preview for dry runs. It never mutates the cluster.
//   - [Engine.Step] executes at most one (phase, wave) group and returns the new operation
//     phase, message, per-resource results and progress. The caller persists them in
//     Operation.status and calls Step again after [StepResult.RequeueAfter] while the phase
//     is Running. Every call rebuilds its state from the inputs (targets, live objects and
//     the results persisted by previous steps), so a restarted controller simply continues.
//   - [Engine.Terminate] stops a running operation: running hooks are deleted per policy
//     and no new task starts.
//
// [KubeClient] implements the [Kube] interface on top of a dynamic client and API
// discovery: client-side apply (three-way strategic merge patch for built-in kinds, JSON
// merge patch otherwise, with the last-applied-configuration annotation), server-side
// apply, client-side to server-side field ownership migration, replace, create, update,
// delete with propagation policies and server-side dry runs.
//
// The package does not persist anything itself and holds no state between calls.
package sync
