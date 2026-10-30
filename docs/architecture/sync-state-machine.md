# Sync operation state machine

Applies to `Operation` objects (ADR-0011). The controller shard owning the Application's
destination cluster is the only writer of `Operation.status`.

## States

| Phase | Terminal | Meaning |
|-------|----------|---------|
| `Pending` | no | Accepted, waiting for the Application's operation slot or a sync window |
| `Running` | no | Plan resolved; executing waves |
| `Terminating` | no | `spec.cancel=true` observed; stopping |
| `Succeeded` | yes | All tasks applied, hooks completed, health gates passed |
| `Failed` | yes | A task, hook or health gate failed; retry budget exhausted |
| `Error` | yes | Infrastructure or validation error that prevented execution (render failure, project denial, invalid spec); retry budget exhausted |
| `Cancelled` | yes | Termination completed |
| `Rejected` | yes | Not admitted (invalid spec, Application being deleted, another operation in progress, rollback not allowed or its target not found) |

## Transitions

```
            ┌──────────── admission fails ───────────────▶ Rejected
            │
 (created) ─┴─▶ Pending ── slot acquired & window open ──▶ Running ──┬── all waves done ─────▶ Succeeded
                  │                                                   ├── task failed ─┬─ retries left ─▶ Running (attempt+1, after backoff)
                  │                                                   │                └─ exhausted ────▶ Failed
                  │                                                   ├── unrecoverable error ─┬─ retries left ─▶ Running
                  │                                                   │                        └─ exhausted ────▶ Error
                  └── cancel ─▶ Cancelled                             └── cancel ─▶ Terminating ── hooks cleaned ─▶ Cancelled
```

Admission rules (evaluated in this order while an operation is `Pending`;
`internal/controller/operation/admission.go`):

1. `spec.cancel` is set → `Cancelled`. Neither `spec.sync` nor `spec.rollback` is set →
   `Rejected` / `InvalidSpec`.
2. The Application is being deleted → `Rejected` / `ApplicationDeleting`.
3. Another Operation for the same Application is admitted (`Running` or `Terminating`), or
   is `Pending` and older (`creationTimestamp`, then name) → `Rejected` /
   `OperationInProgress`.
4. Rollback requested while automated sync is enabled, or to a history entry without
   sources → `Rejected` / `RollbackNotAllowed`; to a history ID that does not exist →
   `Rejected` / `RollbackTargetNotFound`.
5. Sync windows do not allow the sync now (a deny window is active, or allow windows exist
   and none is active; a manual sync passes when the blocking windows set `manualSync`) →
   a manual operation stays `Pending` with condition `Admitted=False`, reason
   `SyncWindowClosed`, and is checked again at the next window transition (at least once a
   minute); an automated operation is deleted instead of waiting, matching upstream
   behavior of skipping auto-sync during closed windows.

Rejection sets `Admitted=False` with the reason above. An admitted operation becomes
`Running` with `status.startedAt` set and condition `Admitted=True`.

## Execution of `Running`

Each reconcile performs at most one step and persists the result before returning:

1. **Resolve** (first step only): compare the Application at the requested revisions;
   repo-server resolves every source revision to an immutable revision (Git SHA, chart
   version, OCI digest) while it renders. One status write records `status.plan`
   (`resolvedRevisions`, `renderedInputDigest`, `destination`, planning `warnings`),
   `status.syncResult.revision(s)` and `source(s)`, `status.progress.totalTasks` and
   condition `Planned=True` / `RevisionsResolved`. (`status.startedAt` was written at
   admission.) Subsequent steps and restarts use the persisted revisions, never
   re-resolving a branch.
2. **Plan** (every step): render manifests at the resolved revisions (repo-server cache
   makes this cheap after a restart), load live state, compute the task list:
   - apply tasks for target objects (skipped when `ApplyOutOfSyncOnly` and already synced),
   - prune tasks for managed live objects absent from target (`Prune=false` → `PruneSkipped`,
     `Prune=confirm` → blocked until the confirmation annotation is present; `PruneLast`
     moves to a final wave),
   - hook tasks for `PreSync`, `Sync`, `PostSync`, `SyncFail` (only on failure) phases,
   - ordering: phase → wave → kind order (namespaces, CRDs, …) → name.
   `status.plan.renderedInputDigest` (sha256 over the sorted normalized manifests) is
   compared with the new rendering; a change adds a plan warning. Project permission checks
   run per task; any denial fails the operation before mutation.
3. **Dry run** (in the sync engine's first step of an attempt, before its first wave;
   `internal/sync/step.go`): before any task is applied, every task of every wave is
   dry-run once on the server (skipped with `SkipDryRunOnMissingResource` for kinds the
   cluster does not serve yet, such as CRDs created earlier in this sync;
   `ApplyOutOfSyncOnly` leaves out synced resources). A namespace the sync creates
   (`CreateNamespace=true`) is applied first so the resources in it can be dry-run. A
   failed dry run fails the attempt before anything else is applied (`spec.retry` may start
   another). Later waves are not dry-run again.
4. **Wave step**: pick the lowest (phase, wave) with incomplete tasks:
   - apply/create/prune with the configured strategy (client-side apply, server-side apply,
     replace, force),
   - record per-task results into `status.syncResult.resources` (bounded: one entry per task),
   - requeue until every task in the wave is complete: hooks finished (Job/Pod/Argo-style
     workflow health), applied resources healthy (or no health check), pruned resources gone.
   - wave delay (`PATROL_SYNC_WAVE_DELAY`, default 2s) between waves.
5. **Finish**: run hook deletion policies (`HookSucceeded`, `HookFailed`), set terminal
   phase, `finishedAt`, append an Application history entry (only for successful,
   non-dry-run syncs), update `Application.status.operationState`, emit Event, notify.

## Idempotency and restart recovery

- A task's identity is `(phase, wave, group, kind, namespace, name)`. Hooks using
  `generateName` get the deterministic name `<generateName><first 8 hex of sha256(operationUID/phase/wave/index)>`,
  so re-running a hook step after a crash finds the existing hook object instead of creating
  a second one.
- Applying the same manifest twice is a no-op for apply strategies; replace/force are guarded
  by comparing the live object's tracking annotation and `cd.patrol.example/operation`
  annotation with the current operation UID.
- `status.progress` (phase, wave) and per-resource results are written after each wave; on
  restart the executor re-plans and treats a task as complete when the persisted result says
  so **and** the live state confirms it (resource present with expected tracking; hook
  completed). Divergence (e.g. a resource deleted meanwhile) re-executes the task.
- Lease loss mid-step: the step's writes are guarded by optimistic concurrency
  (`resourceVersion`) on the Operation; a stale writer's status update fails with 409 and the
  step is abandoned.

## Retry semantics

- `spec.retry.limit` (−1 unlimited, 0 none) counts attempts after a `Failed`/`Error` outcome.
  Backoff: `duration * factor^attempt`, capped at `maxDuration` (defaults 5s, 2, 3m).
- `retry.refresh=true` re-resolves revisions before the next attempt (new plan identity is
  recorded); otherwise the persisted revisions are reused.
- Transient Kubernetes API errors inside a step (server and client timeouts, 429, 503,
  connection refused or reset, unexpected EOF; `IsTransient` in `internal/sync/kube.go`)
  end the step without recording a failure and requeue it after a fixed poll interval (5s,
  `DefaultPollInterval` in `internal/sync/keys.go`; `syncStep` in
  `internal/controller/operation/execute.go`). There is no rate-limited backoff, and the
  attempt is not counted.
- Other API errors are not transient: a 500 (a failing admission webhook, say) fails the
  task, as upstream does. A 409 conflict on a resource is retried inside the step; a 409 on
  the Operation's own status write abandons the step, which is recomputed from the newer
  object after the same poll interval.

## Automated sync guard

The Application reconciler creates an automated Operation only if
(`internal/controller/app/autosync.go`, `decideAutoSync`):
- `syncPolicy.automated` is enabled, the app is `OutOfSync`, no Operation is non-terminal,
- the sync has not already been attempted: `alreadyAttemptedSync` compares the resolved
  revision set and the source spec with the last operation recorded in
  `status.operationState` (`syncResult.revision(s)` and `source(s)`; the history is not
  read). An attempt that succeeded is repeated only when `selfHeal` is enabled and the
  self-heal backoff for the current attempt count has elapsed,
- pruning would not delete all resources when `allowEmpty=false`,
- the sync window permits automated sync.

When the last attempt at the same revisions failed (its phase is not `Succeeded`), the
reconciler creates no new automated Operation for that revision, even with `selfHeal`, and
sets condition `SyncError` with reason `AutoSyncFailed` ("Failed last sync attempt to
…"). A new revision, a change of the source spec, or a successful manual sync clears the
way.
