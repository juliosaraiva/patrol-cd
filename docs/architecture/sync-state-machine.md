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
| `Rejected` | yes | Not admitted (another operation in progress, sync window denies, rollback not allowed) |

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

Admission rules (evaluated when a `Pending` operation is first seen):

1. Another non-terminal Operation for the same Application exists and is older
   (`creationTimestamp`, then name) → `Rejected` / `OperationInProgress`.
2. The Application is being deleted → `Rejected` / `ApplicationDeleting`.
3. Sync windows: a deny window is active, or no allow window is active while allow windows
   exist, and the operation is automated, or manual sync is not permitted by the window
   (`manualSync: false`) → stays `Pending` with condition `SyncWindowBlocked` (automated
   operations are deleted instead of waiting, matching upstream behavior of skipping
   auto-sync during closed windows).
4. Rollback requested while automated sync is enabled, or for a multi-source application
   without history sources → `Rejected` / `RollbackNotAllowed`.

## Execution of `Running`

Each reconcile performs at most one step and persists the result before returning:

1. **Resolve** (first step only): resolve every source revision to an immutable revision
   (Git SHA, chart version, OCI digest) and write `status.plan.resolvedRevisions`,
   `status.syncResult.revision(s)` and `status.startedAt`. Subsequent steps and restarts use
   the persisted revisions, never re-resolving a branch.
2. **Plan**: render manifests at the resolved revisions (repo-server cache makes this cheap
   after a restart), load live state, compute the task list:
   - apply tasks for target objects (skipped when `ApplyOutOfSyncOnly` and already synced),
   - prune tasks for managed live objects absent from target (`Prune=false` → `PruneSkipped`,
     `Prune=confirm` → blocked until the confirmation annotation is present; `PruneLast`
     moves to a final wave),
   - hook tasks for `PreSync`, `Sync`, `PostSync`, `SyncFail` (only on failure) phases,
   - ordering: phase → wave → kind order (namespaces, CRDs, …) → name.
   Write `status.plan.renderedInputDigest` (sha256 over the sorted normalized manifests) and
   `status.progress.totalTasks`. Project permission checks run per task; any denial fails
   the operation before mutation.
3. **Wave step**: pick the lowest (phase, wave) with incomplete tasks:
   - server-side dry-run of every task in the wave (skipped with
     `SkipDryRunOnMissingResource` for missing CRDs created earlier in this sync),
   - apply/create/prune with the configured strategy (client-side apply, server-side apply,
     replace, force),
   - record per-task results into `status.syncResult.resources` (bounded: one entry per task),
   - requeue until every task in the wave is complete: hooks finished (Job/Pod/Argo-style
     workflow health), applied resources healthy (or no health check), pruned resources gone.
   - wave delay (`PATROL_SYNC_WAVE_DELAY`, default 2s) between waves.
4. **Finish**: run hook deletion policies (`HookSucceeded`, `HookFailed`), set terminal
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
- Transient API errors (timeouts, 429, 5xx, 409 conflicts) inside a step are retried by the
  controller workqueue with rate-limited backoff without counting as an attempt.

## Automated sync guard

The Application reconciler creates an automated Operation only if:
- `syncPolicy.automated` is enabled, the app is `OutOfSync`, no Operation is non-terminal,
- the resolved revision set differs from the last automated attempt
  (`status.operationState` + history), unless `selfHeal` is enabled and the self-heal backoff
  for the current attempt count has elapsed,
- pruning would not delete all resources when `allowEmpty=false`,
- the sync window permits automated sync.
