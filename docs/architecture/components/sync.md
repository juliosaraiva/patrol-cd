# Component: `internal/sync`

Status: implemented (library only; the Operation reconciler that drives it is not built yet).
Decision records: ADR-0011 (Operation resource and sync state machine), ADR-0001
(independent implementation). Normative behavior: `docs/architecture/sync-state-machine.md`.
Parity area: `docs/parity/areas/05-synchronization.md`; status overlay:
`docs/parity/status/sync.yaml`.

`internal/sync` plans and executes sync operations. It re-implements the observable
behavior of the Argo CD v3.5.5 sync engine (`gitops-engine/pkg/sync`, the controller's
`controller/sync.go` and `controller/sync_namespace.go`, and the `kubectl` operations the
engine runs in-process) for an external, re-entrant reconciler that persists progress in
`Operation.status`. No upstream code is imported or copied. It depends on `api/v1alpha1`,
`internal/cache` (for `ResourceKey`), `internal/health`, `internal/diff` and client-go;
it does not depend on `internal/git`, `internal/helm` or `internal/oci`.

## Responsibilities

- Turn rendered manifests (including hooks) and the managed live objects into an ordered
  task list: apply tasks, prune tasks, hook tasks per phase, the synthetic namespace task.
- Validate tasks against API discovery (missing kinds, `SkipDryRunOnMissingResource`) and
  the project (`PermissionFunc`) before anything changes.
- Execute one (phase, wave) group per call, wait for hooks/health/deletions, run SyncFail
  hooks on failure, apply hook delete policies, terminate on request.
- Provide the Kubernetes write path (`KubeClient`): client-side apply with the
  last-applied annotation, server-side apply, CSA→SSA field-ownership migration, replace,
  create, update, delete, server-side dry runs, CRD readiness.
- Helpers for other components: option parsing and precedence (`ParseOptions`,
  `Options.*For`), hook parsing (`HookTypes`, `SyncPhases`, `DeletePolicies`, `Wave`),
  deletion-hook partitioning (`PartitionTargets`, `IsPreDeleteHook`, `IsPostDeleteHook`),
  `Delete=false|confirm` and `deletion-approved` evaluation (`Options.DeleteDisabled`,
  `Options.RequiresDeletionConfirmation`, `DeletionApproved`), `RespectIgnoreDifferences`,
  `Digest` (status.plan.renderedInputDigest), `WaveDelayFromEnv`.

Out of scope (other components): the Operation reconciler loop (admission, revision
resolution, rendering, retries/backoff, timeouts, persistence, events, history), sync
windows, automated sync, tracking metadata and self-reference decisions
(`internal/tracking`), project policy evaluation (`internal/project`), the comparison that
produces `DiffResults` and the `SharedResourceWarning` condition, PreDelete/PostDelete hook
execution during Application deletion, API/CLI/UI surfaces.

## Public API

| Symbol | Purpose |
|--------|---------|
| `NewEngine(kube Kube, health HealthFunc, cfg Config) (*Engine, error)` | Engine for one destination cluster. `health` nil → `internal/health.Assess` without overrides. Safe for concurrent use; holds no per-operation state. |
| `Config{Manager, WaveDelay, PollInterval, CRDEstablishTimeout, Parallelism, Clock, Logger}` / `DefaultConfig()` | Field manager (`patrol-controller`), wave delay (`PATROL_SYNC_WAVE_DELAY`, default 2s), requeue interval while waiting (5s), CRD wait (3s), concurrent API calls per step (10). |
| `(*Engine).Plan(ctx, *Input) (*Plan, error)` | Pure plan: no mutation (read-only discovery, the destination namespace GET, callbacks on copies). |
| `(*Engine).Step(ctx, *Input) (*StepResult, error)` | Executes at most one (phase, wave) group. |
| `(*Engine).Terminate(ctx, *Input) (*StepResult, error)` | Cancels: cleans up running hooks, returns `Cancelled` "Operation terminated" or `Error` "Operation termination had errors". |
| `Input` | `OperationUID`, `Attempt` (status.retryCount), `App{Name, Namespace, Annotations, Destination}`, `Targets`, `Live map[ResourceKey]*Unstructured`, `DiffResults map[ResourceKey]bool`, `SyncOptions`, `Prune`, `DryRun`, `Strategy`, `Resources`, `ManagedNamespaceMetadata`, `TrackManagedNamespace`, `StartedAt`, `Previous []ResourceResult`, `Progress`, callbacks `Permission`, `IsNamespaced`, `SetTracking`, `SharedWith`, `IsSelfReferenced`, `Normalizer`. |
| `StepResult{Phase, Message, Results, Progress, RequeueAfter, ManagedNamespaceMetadata}` | What the reconciler persists. While `Phase == Running` it calls `Step` again after `RequeueAfter` (0 = immediately; returned only after a step that made progress: a wave applied with no wave delay, or a hook completion to persist). Waiting steps (health, hooks, deletions, prune confirmation) return `PollInterval`. |
| `Plan{Tasks, Valid, Errors, SharedResource, Warnings, PendingConfirmation, SkipHooks, MultiStep, Digest, TotalTasks}`, `(*Plan).Preview()` | Planned tasks (`PlannedTask{Key, Version, Phase, Wave, Hook, HookType, DeletePolicies, Action, Message, SkipDryRun, RequiresConfirmation, Status, HookPhase, ResultMessage, Target, Live}`), destructive-change warnings, prune confirmations, `sync --dry-run` / `--preview-changes` output. |
| `Kube` | Interface: `Resource`, `Get`, `ClientSideApply`, `ServerSideApply`, `MigrateClientSideApply`, `Replace`, `Create`, `Update`, `Delete`, `WaitForCRDEstablished`, with `ApplyOptions`, `ReplaceOptions`, `CreateOptions`, `UpdateOptions`, `DeleteOptions` (all with server-side `DryRun`). |
| `NewKubeClient(*rest.Config, KubeClientOptions)`, `NewKubeClientFor(dynamic, discovery, opts)` | `Kube` implementation. |
| `HealthFunc`, `PermissionFunc(obj, namespaced bool) error` | Injected health (built-in + Lua overrides) and project check. |
| `ResourceKey` | Alias of `internal/cache.ResourceKey`, so the cache's managed live objects are passed unchanged. |
| `ErrTransient`, `ErrInvalidInput`, `IsTransient(err)` | Error classification. |
| Constants | `AnnotationKeyOperation` (`cd.patrol.example/operation`), Helm annotation keys, `DefaultManager`, `DefaultClientSideApplyMigrationManager`, `EnvSyncWaveDelay`, `DefaultWaveDelay`, `DefaultPollInterval`, `DefaultCRDEstablishTimeout`. |

### Wiring by the Operation reconciler (expected use)

1. Build `Input` from the Operation (UID, `status.retryCount` as `Attempt`, `spec.sync`,
   `status.startedAt`, `status.syncResult.resources`, `status.progress`), the Application (name, namespace,
   annotations, destination, `syncPolicy.managedNamespaceMetadata`), the rendered
   manifests at the persisted revisions, `internal/cache.GetManagedLiveObjs`, the
   comparison's diff results, and callbacks from `internal/tracking` and `internal/project`.
   `SyncOptions` must be the operation's options (the API/auto-sync copy the Application's).
   The cache strips `metadata.managedFields` unless `Settings.RetainManagedFields` is set;
   the engine then reads each object once more before a server-side apply to decide on the
   client-side apply migration (SYNC-044). Setting `RetainManagedFields` (also needed by
   server-side diff) avoids that extra GET.
2. If `spec.cancel` (or the sync timeout) → `Terminate`; else `Step`.
3. Persist `Phase/Message/Results/Progress/ManagedNamespaceMetadata` with optimistic
   concurrency; on `ErrTransient` requeue with rate-limited backoff (optionally persisting
   the partial result) without counting a retry; requeue after `RequeueAfter` while Running.
   A transient error during the first step (namespace creation, dry run) returns no rows, so
   persisting it never marks the dry run as done.
4. On a retry attempt, reset `syncResult.resources` and `progress` (like upstream clears
   `syncResult`), so the new attempt dry-runs again.

## Behavior and invariants

Planning (mirrors upstream `getSyncTasks` plus controller preparation):

1. Targets: PreDelete/PostDelete-only hooks dropped; hooks without a valid type ignored;
   `SetTracking` applied to copies; resources paired with live objects by key (scope from
   `IsNamespaced`, discovery or a CRD manifest of the same sync; unknown scope checks both
   keys); live objects sharing a UID across API groups de-duplicated. Namespaced targets
   without namespace get the destination namespace; cluster-scoped ones get none (upstream
   sets the destination namespace on them too and reports it in results — see deviations).
2. `RespectIgnoreDifferences=true`: targets receive live values of ignored fields
   (`RespectIgnoreDifferences`), never `status`.
3. Selective sync (`Resources`): group/kind/name match (`*` wildcard as documented on
   `SyncOperationResource`), empty namespace matches all; hooks are skipped for selective
   syncs and the apply strategy; `IsSelfReferenced` filters live objects.
4. Hooks: one task per phase; `generateName`-only hooks get
   `generateName + sha256(uid/phase/wave/index)[:8]`; every hook carries
   `cd.patrol.example/hook-finalizer` and `cd.patrol.example/operation:
   <uid>/<attempt>/<phase>`.
5. `CreateNamespace=true`: a PreSync `Namespace` task unless a non-hook manifest defines
   the namespace; added when the namespace is missing, or always with managed metadata
   (labels/annotations + `sync-options: ServerSideApply=true`).
6. Validation: discovery per GVK (Unauthorized retried), NotFound + skip rule →
   skip-dry-run, other errors → task `SyncFailed`; `PermissionFunc` on every task including
   prunes and skip-dry-run tasks.
7. Ordering: prune waves mirrored; `PruneLast` → last Sync wave + 1; sort by phase, wave,
   kind order (35-kind list, unknown kinds last), name, then kind, group, namespace,
   apply-before-prune (total order, property-tested); Namespace/CRD moved before their first
   dependent, inheriting its phase and wave.
8. Persisted results are re-applied by `(resource key, sync phase)`; a hook of this
   operation attempt found live without a persisted result is adopted (Synced/Running).
   Hooks of an earlier attempt are not adopted, so a retry re-runs them (BeforeHookCreation),
   as upstream retries do.

Execution (`Step`, mirrors upstream `Sync`):

1. `FailOnSharedResource` + `SharedWith` → `Failed` "Shared resource found: …"; invalid
   tasks → `Failed` "one or more synchronization tasks are not valid[: msgs]".
2. First step (no results yet): destination namespace created (wet) before the dry run
   ("the namespace failed to apply, reason: …" on failure); every task server-side
   dry-run → `Failed` "one or more objects failed to apply (dry run)". Not sent to the
   server, because they would be validated against state this sync replaces: skip-dry-run
   tasks, unchanged ApplyOutOfSyncOnly tasks, tasks in a namespace created by this sync,
   hooks that BeforeHookCreation deletes and re-creates (reported "created (dry run)"), and
   custom resources whose CRD this sync creates or changes (a CRD counts as unchanged only
   when `DiffResults` says so; `PlannedTask.SkipDryRun`). A transient error during the
   first step discards its rows, so the next step repeats the dry run.
3. Wave delay: if the last applied non-final wave started less than `WaveDelay` ago,
   return Running with the remaining time without assessing health.
4. Running tasks: hook health → Running/Succeeded/Failed (no health → "<name> created");
   resource health in multi-step syncs (Healthy → Succeeded, Degraded → Failed, no health →
   Succeeded). Missing running objects are re-read; gone → `SyncFailed`/`Error` "Resource
   g/k/n is missing, it might have been deleted". Waiting messages are upstream's
   (`waiting for completion of hook …`, `healthy state of`, `deletion of [hook]`,
   `pruning confirmation of`, `and N more resources|hooks`).
5. A hook whose completion is not yet persisted is not cleaned up in the same step: the
   step returns Running ("one or more tasks are running", requeue immediately) first.
6. Completed hooks lose their finalizer; on failure HookFailed hooks are deleted and SyncFail
   hooks run (message "<reason>[, reason: a,b]" plus "\none or more SyncFail hooks failed,
   reason: …"); on success HookSucceeded hooks are deleted.
7. Next pending (phase, wave): remove leftover hook finalizers, prune-confirmation gate
   (`deletion-approved` ≥ `StartedAt`), prune ("pruned", "pruned (dry run)", "ignored
   (requires pruning)", "ignored (no prune)"), BeforeHookCreation deletion (pending until
   gone; not re-issued while the hook is terminating), then apply by consecutive kind groups (groups sequential, tasks concurrent).
   Single-step syncs without hooks succeed right after apply ("successfully synced (all
   tasks run)"); otherwise "successfully synced (no more tasks)".
8. Results: one row per (resource, phase) in first-recorded order; concurrent tasks reserve
   positions in task order, so the order is deterministic. Missing-kind and annotation-size
   messages are augmented with upstream wording.

Idempotency: every call rebuilds state from inputs; applies are idempotent; hooks have
deterministic names and are adopted after a crash; finalizers are removed and delete
policies applied only after completion was persisted; prunes skip objects already
terminating. Replace with force (delete + create) and `Replace=true` creates stamp
`cd.patrol.example/operation: <uid>/<attempt>/<phase>` on the object; before a forced
replace the engine reads the object and records "replaced" without deleting it again when
it carries this attempt's marker, and a create answered AlreadyExists counts as done when
the existing object carries it (sync-state-machine.md, "Idempotency and restart
recovery"). A retry attempt or a new operation replaces again, like upstream. Client-side
apply with force and RoleBinding re-creation are idempotent by construction (the second
patch is empty, the roleRef already matches).

`KubeClient`:

- Client-side apply = kubectl: create when missing; else three-way patch from
  `kubectl.kubernetes.io/last-applied-configuration` (strategic merge for kinds of the
  client-go scheme, JSON merge with apiVersion/kind/name preconditions otherwise), "{}" →
  "unchanged"; conflicts retried (5); with force, Invalid/Conflict → delete (background),
  wait, re-create, restore the original on failure. Field manager `patrol-controller`.
- RBAC (`rbac.authorization.k8s.io/v1`, non-SSA): namespace must exist ("error running
  kubectl auth reconcile: error getting namespace <ns>: …"); bindings whose immutable
  `roleRef` changed are deleted and re-created.
- Server-side apply with force conflicts; CSA migration with `csaupgrade` on a fresh read
  (retry on conflict; no-op when the fresh object needs none); replace = PUT with fresh
  resourceVersion (conflict retry) or, with force, delete + create; field validation
  `Strict`/`Ignore`; dry runs use `dryRun=All`.
- Discovery cached per group version, refreshed on a miss; `WaitForCRDEstablished` waits for
  `Established` and for discovery to serve the kind.

## Failure modes

| Situation | Result |
|-----------|--------|
| Invalid input (nil, no OperationUID/App.Name, nil Kube) | `ErrInvalidInput` |
| Context cancelled | error wrapping `ctx.Err()`, no result |
| Timeout / 429 / 503 / connection error during a step | partial Running result + error wrapping `ErrTransient`; the affected task stays pending (during the first step: no rows, the dry run is repeated) |
| `SetTracking` error | `Error` "failed to set tracking metadata on <key>: …" |
| Target normalization error | `Error` "Failed to normalize target resources: …" |
| Unknown kind (no skip rule), project denial | `Failed` "one or more synchronization tasks are not valid: …" |
| Destination namespace GET error | task `SyncFailed`/`Error` "namespace auto creation failed: …" → `Failed` |
| Dry-run failure | `Failed` "one or more objects failed to apply (dry run)", nothing applied |
| Apply/prune failure | SyncFail hooks, then `Failed` "one or more objects failed to apply, reason: …" |
| Hook/resource degraded in multi-step sync | `Failed` "one or more synchronization tasks completed unsuccessfully" (after SyncFail hooks) |
| CRD not established within 3s | logged; the dependent apply fails if the kind is still not served |
| Termination error | `Error` "Operation termination had errors" |

## Security notes

- Manifests and Secret data are never logged; logs carry keys, phases and API error text.
- Project permissions are checked for every task, including prune tasks and custom
  resources whose CRD is created in the same sync (upstream skips the check for the latter).
- All writes use the destination credentials of the `rest.Config` given to `KubeClient`
  (impersonation is configured by the caller); dry runs never persist (`dryRun=All`).
- Hook objects are marked with the operation UID and attempt; adoption only takes over hooks
  that carry this attempt's exact marker.
- Force/replace are destructive; `Plan.Warnings` lists them for confirmation prompts.

## Parity IDs covered

See `docs/parity/status/sync.yaml` for status and tests. Engine-level behavior of SYNC-006,
007, 008, 009, 010, 029–046 (except 047), 048–068 (PreDelete/PostDelete helpers only),
071–078, 080. Not in this package: SYNC-001..005, 011..028, 047, 069, 070, 079, 081..112.

## Known gaps and deviations

- **Hook names** follow sync-state-machine.md (`generateName` + 8 hex of
  `sha256(uid/phase/wave/index)`), not upstream's `<rev7>-<phase>-<unix start>` (SYNC-057,
  different-by-design).
- **Dry run** (SYNC-071, different-by-design) is a server-side dry run (`dryRun=All`), as
  sync-state-machine.md requires, instead of kubectl's client dry run (which computes
  patches locally and sends nothing). It validates schema, immutability and admission
  against the current cluster state, so tasks whose validation would depend on state this
  sync replaces are not sent (see Execution 2). Remaining differences: an immutable-field
  change of an object that is patched (not deleted first) fails the dry run, before
  anything is applied, where upstream fails mid-sync; admission plugins and webhooks see
  the current state and may reject an object that depends on another object created or
  changed later in the same sync; with `ServerSideApply` the dry run uses server-side apply
  (upstream switches to client-side apply). Dry-run messages use upstream's "(dry run)"
  suffix.
- **Wave delay** is a requeue (`RequeueAfter`) gated on `status.progress.startedAt`,
  not a sleep; `startedAt` is persisted with second precision, so the gate can end up to
  1s early. A pending wave (prune confirmation, BeforeHookCreation deletion) requeues
  after `PollInterval` (upstream sleeps the wave delay, then the controller requeues with
  rate limiting).
- **Hook cleanup** takes one extra immediate step: completion is persisted before
  finalizer removal and delete policies (crash safety).
- **Cluster-scoped targets** get no namespace in results; upstream reports the destination
  namespace for new cluster-scoped objects.
- **Terminate** ends `Cancelled` (Patrol state machine) instead of upstream `Failed`.
- **kubectl auth reconcile** is approximated (namespace check, roleRef re-creation); rule
  merging is left to the apply; the reconcile output is not part of the message.
- **Messages**: the kubectl temp-file prefixes are absent; errors are the API error text.
  `failed to delete resource:` is not doubled, and the hook health error is prefixed once.
- **Transient errors** (timeouts, 429, 503, connection errors) leave tasks pending and are
  returned as `ErrTransient` (normative retry semantics) instead of failing the task.
- **Running resource deleted mid-sync** fails the task like upstream; sync-state-machine.md
  ("Idempotency and restart recovery") says a completed task whose live state diverged is
  re-executed — not implemented: the engine keeps upstream semantics (re-running a
  completed Job resource is unsafe); the doc should be reconciled. The replace/force guard
  of the same section is implemented (see Idempotency).
- **Server-side apply with `Force=true`/strategy force**: the engine applies with forced
  field conflicts and ignores force; upstream's kubectl options reject the combination
  ("error validating options: --force cannot be used with --server-side").
- **Discovery**: no generic `meta.RESTMapper`; GVK→GVR mapping comes from per-group-version
  discovery (equivalent for this use).
- **`PermissionFunc`** receives the resource scope (`namespaced bool`) in addition to the
  object, as the project check needs it.
- **`GetOptionValue`**: the engine uses upstream's first-match rule for `Prune=`/`Delete=`
  values; `v1alpha1.SyncOptions.GetOptionValue` returns the last match.
- **Not implemented here**: PreDelete/PostDelete execution and SYNC-070 deletion ordering
  (Application deletion flow), sync timeout and retries (reconciler), `status.resources`
  fields, `SharedResourceWarning` computation, the API replace gate.

## Requests to other owners

- `api/v1alpha1/keys.go`: declare `cd.patrol.example/operation` (used on hooks; referenced by
  sync-state-machine.md but not declared). The engine defines `AnnotationKeyOperation`
  locally meanwhile.
- `docs/architecture/sync-state-machine.md`: reconcile "Divergence … re-executes the task"
  with SYNC-059's upstream behavior (see above), and state that the per-wave server-side
  dry run skips tasks validated against state the sync replaces (Execution 2).
- Operation reconciler: build `Live` from `internal/cache` with `RetainManagedFields=true`
  where possible (see Wiring).
- `v1alpha1.ResourceResult` has no wave field; progress and results are sufficient for
  resumption, so no change is required.

## Verification

Commands (run from the repository root on darwin/arm64, Go 1.27.2):

```bash
go build ./...
go vet ./internal/sync/
golangci-lint run ./internal/sync/...
KUBEBUILDER_ASSETS=$PWD/bin/envtest/k8s/1.36.0-darwin-arm64 go test -race -count=1 ./internal/sync/
```

Last run (review follow-up, 2026-10-13): `go build ./internal/sync/...` ok (`go build ./...`
failed in `internal/apiserver/rest` and `internal/render`, packages of other workstreams
being edited); `go vet ./internal/sync/` ok; `golangci-lint run ./internal/sync/...` 0
issues; `go test -race` with envtest ok (23 envtest tests passed, none skipped); the new
regression tests were also run with `go test -overlay` against the pre-fix code and failed
there.

- Unit tests with an in-memory `Kube` fake (`fake_test.go`): planning, ordering, options,
  hooks, every step outcome and message, terminate, transient errors, restart adoption.
- Property tests (`pgregory.net/rapid`): plans are deterministic and independent of
  manifest order; plans are sorted; the task order is total; hook names are pure.
- envtest integration (`envtest_test.go`, real kube-apiserver 1.36, no controllers, one
  namespace per test): CSA apply/three-way patch and prune, SSA and CSA migration (also
  with live state stripped of managedFields like the cluster cache), replace/force/strategy
  force and the once-per-attempt replace guard, CRD + CR in one sync and missing kinds, a
  CRD schema change with a CR using the new field, a fixed-name Job hook re-created with
  an immutable change (BeforeHookCreation), Validate=false,
  CreateNamespace + managed metadata, propagation policies (GC finalizers observed),
  pruned-object deletion wait, PruneLast/Prune=false/Delete=false, ApplyOutOfSyncOnly,
  multi-wave with requeue and wave delay, hook lifecycle with delete policies, SyncFail
  hooks, terminate, restart recovery (crash before and after persisting; no duplicate
  hooks), dry-run operations, RBAC roleRef change, selective sync, discovery. Hook
  completion is simulated with a fake `HealthFunc` (no Job controller).
