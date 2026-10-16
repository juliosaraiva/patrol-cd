# Synchronization — parity inventory (Argo CD v3.5.5)

Scope: everything that happens between "an operation is requested on an Application" and "the cluster matches the
desired state (or the operation fails)". In scope: the `Application.operation` / `status.operationState` model; the
manual Sync, Rollback and TerminateOperation APIs, CLI and UI; every `SyncOperation` field; automated sync (prune,
selfHeal, allowEmpty, enabled), same-revision loop prevention and self-heal backoff; retry; every sync option
(app-level `spec.syncPolicy.syncOptions` and resource-level `argocd.argoproj.io/sync-options`); phases, waves and
hooks (Argo CD and Helm annotations, delete policies, PreDelete/PostDelete); task ordering; pruning order; dry-run;
missing-CRD handling; sync windows (definition, matching, evaluation, overrun, enforcement points); revision
history and rollback; sync timeout; service account impersonation for sync and for API-server live operations; resource actions (Lua discovery and
execution, parameters, create-operations, built-in action catalogue). Out of scope (covered by other areas): manifest
generation and source types, diffing/normalization and compare-options (only referenced where they change sync
behaviour), health assessment internals (only the health-to-sync-phase mapping is here), resource tracking methods,
the full Application deletion/finalizer flow (only hook and wave interactions are listed here), AppProject CRUD other
than sync windows and destination service accounts, ApplicationSet progressive syncs, the source hydrator, and
notifications.

Permalink base: `https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/`.
`gitops-engine` lives in-tree at `gitops-engine/`.

---

## A. Operation model and manual sync

### SYNC-001 Application operation object (declarative sync trigger)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1348 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1340 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L939 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1121 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/crds/application-crd.yaml#L57 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/sync-kubectl.md#L1
- **Maturity**: GA
- **Semantics**: An Application carries an optional top-level `operation` (sibling of `spec`/`status`). The only
  operation kind is `sync` (`operation.sync: SyncOperation`). Other fields: `initiatedBy{username, automated}`,
  `info[]{name,value}` (free-form, persisted), `retry` (RetryStrategy). The controller's operation worker
  (`processAppOperationQueueItem`) re-reads the Application from the API server (not the informer) whenever
  `operation != nil` and processes it. Writing `operation` via `kubectl apply/patch` is a supported way to start a
  sync (no Argo CD API RBAC applies; only Kubernetes RBAC on the Application object). The API helper
  `SetAppOperation` sets `operation`, clears `status.operationState`, retries on update conflicts, and refuses with
  gRPC `FailedPrecondition` "another operation is already in progress" if `operation` is already non-nil.
  When an operation completes, the controller patches `/operation` to `null` (see SYNC-081).
- **Edge cases**: `operation.sync` omitted → `SetAppOperation` still performs the update and then returns
  `InvalidArgument` "Operation unspecified"; the controller then fails the operation with phase `Error`, message
  "Invalid operation request: no operation specified". Patching `operation` while another operation is running is
  not prevented at the Kubernetes level (only the Argo CD API guards it). Stale informer data is avoided by the
  fresh GET; if the Application was deleted meanwhile it is evicted from the cache.
- **Configuration surface**: CRD `Application.operation.{sync, initiatedBy.username, initiatedBy.automated, info[], retry}`.
- **Exposure**: CRD (direct write); API (indirectly via Sync/Rollback RPCs); CLI/UI indirectly.
- **Tests required**:
  - Positive: `kubectl patch --type merge` with `operation.sync.syncStrategy.hook: {}` starts a sync; `status.operationState.operation` mirrors it; `operation` becomes null on completion.
  - Negative: API sync while `operation` set → `FailedPrecondition` "another operation is already in progress".
  - Negative: `operation: {}` (no sync) → operationState phase `Error`, message "Invalid operation request: no operation specified".
  - Recovery: operation written while controller is down is processed after restart.

### SYNC-002 Manual sync API (ApplicationService.Sync)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2084 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.proto#L126 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.proto#L447
- **Maturity**: GA
- **Semantics**: `POST /api/v1/applications/{name}/sync` (body `ApplicationSyncRequest`: name, revision, dryRun,
  prune, strategy, resources[], manifests[], infos[], retryStrategy, syncOptions{items[]}, appNamespace, project,
  sourcePositions[], revisions[]). Processing order (each step can short-circuit):
  1. Load app with RBAC `applications, get` (404/403 obfuscation rules of `getAppEnforceRBAC`).
  2. Evaluate project sync windows as a *manual* sync: invalid window → `PermissionDenied` "cannot sync: invalid sync window: <err>"; blocked → `PermissionDenied` "cannot sync: blocked by sync window".
  3. RBAC `applications, sync, <project>/<app>`.
  4. If `manifests` given: RBAC `applications, override`; if automated sync enabled and not dryRun → `FailedPrecondition` "cannot use local sync when Automatic Sync Policy is enabled unless for dry run".
  5. App being deleted → `FailedPrecondition` "application is deleting".
  6. Revision resolution (SYNC-003/004/005).
  7. Retry = request `retryStrategy` else `spec.syncPolicy.retry`; syncOptions = request `syncOptions.items` (if the message is present) else `spec.syncPolicy.syncOptions` (SYNC-013).
  8. `Replace=true` in the effective options while the server disallows replace → `FailedPrecondition` (SYNC-049).
  9. `manifests` while the project enforces source integrity → `FailedPrecondition` "Cannot use local manifests when source integrity is enforced".
  10. Build Operation: `sync.source` = spec source (single-source only), `sync.sources` = `spec.sources`, `revision`/`revisions` resolved, `prune`, `dryRun`, `syncStrategy`, `resources`, `manifests`, `syncOptions`; `initiatedBy.username` = session user; `info` = request infos; set via `SetAppOperation`.
  11. Audit event reason `OperationStarted`: "<user> initiated [partial ]sync to <rev>" (multi-source: comma-joined display revisions) or "initiated [partial ]sync locally".
- **Edge cases**: The sync-window check happens before the `sync` RBAC check (a get-only user can learn the window state). The API does not validate that `resources` exist in the app. Nil `strategy` is treated as hook strategy by the controller.
- **Configuration surface**: none beyond those referenced.
- **Exposure**: API `ApplicationService.Sync` / `POST /api/v1/applications/{name}/sync`; CLI `argocd app sync`; UI "Sync" button / sync panel.
- **Tests required**:
  - Positive: sync with defaults creates `operation.sync` with resolved SHA revision and `initiatedBy.username`.
  - Authorization: user with `get` but not `sync` → PermissionDenied; `manifests` without `override` → PermissionDenied.
  - Negative: app with deletionTimestamp → FailedPrecondition "application is deleting".
  - Negative: active deny window without manualSync → PermissionDenied "cannot sync: blocked by sync window".
  - Negative: operation already in progress → FailedPrecondition.

### SYNC-003 Sync revision (single source)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2242 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2442 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2464 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sync.go#L150
- **Maturity**: GA
- **Semantics**: `revision` empty → spec `targetRevision` is used as the ambiguous revision. Local manifests →
  no resolution (revision stays empty). Non-Helm source and the ambiguous revision is already a commit SHA → used
  verbatim. Otherwise the repo-server `ResolveRevision` is called with `NoRevisionCache=true`; the returned concrete
  revision is stored in `operation.sync.revision`, the ambiguous form is used for the audit message. The controller
  resolves again during the first `CompareAppState` and overwrites `syncResult.revision` with the concrete SHA so
  that resumed/retried operations keep syncing the same commit.
- **Edge cases**: resolution errors → `FailedPrecondition` with the repo-server error text. Helm sources always go to
  the repo server (versions/constraints). Revision equal to spec target (or `HEAD` when target empty) is not treated
  as an override (SYNC-005).
- **Configuration surface**: `ApplicationSyncRequest.revision`; `operation.sync.revision`.
- **Exposure**: API; CLI `--revision`; UI sync panel "Revision" field (defaults to `?revision=` query, then targetRevision, then `HEAD`).
- **Tests required**:
  - Positive: branch name resolves to SHA in `operation.sync.revision`; SHA input is not re-resolved.
  - Negative: unknown branch → FailedPrecondition.
  - Recovery: controller restart mid-sync keeps syncing the originally resolved SHA even if the branch moved.

### SYNC-004 Multi-source revisions (revisions + sourcePositions / source names)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2207 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/app.go#L1773 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/app.go#L1920 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/commands/argocd_app_sync.md
- **Maturity**: GA
- **Semantics**: For multi-source apps `revisions[i]` applies to 1-based `sourcePositions[i]`; unspecified sources use
  their spec `targetRevision`. Every source is resolved independently (SYNC-003 rules per source) into
  `operation.sync.revisions[]` (index-aligned with `spec.sources`); `operation.sync.revision` stays empty.
  CLI accepts `--source-names` (mapped to positions via the app's source `name`) or `--source-positions`, not both.
- **Edge cases**: position `<=0` or `> len(sources)` → error "source position is out of range" (server).
  CLI errors: "Cannot use --revisions and --source-positions options when 0 or more than 1 application names are passed",
  "Only one of source-positions and source-names can be specified.", "While using --revisions and --source-positions, length of values for both flags should be same.",
  "source-position cannot be less than or equal to 0, Counting starts at 1", "Unknown source name '<n>'",
  and "argocd cli does not work on multi-source app with --revision flag. Use --revisions and --source-positions instead."
  Server does not validate `len(revisions)==len(sourcePositions)` (mismatch is undefined behaviour; CLI enforces it).
- **Configuration surface**: `ApplicationSyncRequest.{revisions, sourcePositions}`; `operation.sync.revisions`.
- **Exposure**: API; CLI `--revisions`, `--source-positions`, `--source-names`; UI (multi-source revision fields — not verified, see Unconfirmed).
- **Tests required**:
  - Positive: sync 2-source app with `--revisions v2 --source-positions 2` changes only source 2's revision.
  - Negative: position 3 on a 2-source app → error; `--revision` on multi-source app → CLI fatal.

### SYNC-005 Revision override restrictions
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2218 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2243 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L1354 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/rbac.md#L213 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cm.yaml#L465
- **Maturity**: GA
- **Semantics**: A requested revision that differs from `targetRevision` (empty target compared as `HEAD`) is an
  "override". If `argocd-cm` `application.sync.requireOverridePrivilegeForRevisionSync` is `true`, RBAC
  `applications, override` is additionally required. If automated sync is enabled and the request is not a dry run →
  `FailedPrecondition` "Cannot sync to <rev>: auto-sync currently set to <target>" (single source) or
  "Cannot sync source <repoURL> to <rev>: auto-sync currently set to <target>" (multi-source).
- **Edge cases**: setting key absent/empty → `false`; non-boolean value → error "error parsing application.sync.requireOverridePrivilegeForRevisionSync value: ..., expected true or false" wrapped as "error getting setting 'RequireOverridePrivilegeForRevisionSync' from configmap". Dry-run overrides are allowed with auto-sync.
- **Configuration surface**: `argocd-cm` `application.sync.requireOverridePrivilegeForRevisionSync` (default false).
- **Exposure**: API/CLI/UI (any sync with a revision).
- **Tests required**:
  - Authorization: with setting true, sync to other branch without `override` → PermissionDenied; with `override` → allowed.
  - Negative: auto-sync app + different revision (not dry run) → FailedPrecondition message above.
  - Positive: auto-sync app + different revision + dryRun → allowed.

### SYNC-006 Prune flag on a sync operation
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1420 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_context.go#L1454 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1822
- **Maturity**: GA
- **Semantics**: Live resources tracked by the app with no target manifest become prune tasks. With `prune=false`
  each yields result `PruneSkipped` "ignored (requires pruning)" (operation can still succeed; app stays OutOfSync).
  With `prune=true` the object is deleted (unless Prune=false/confirm, SYNC-031/032) → `Pruned` "pruned"; objects
  already carrying a deletionTimestamp are not deleted again but still reported "pruned". Deletion uses the prune
  propagation policy (SYNC-040).
- **Edge cases**: the CLI exits non-zero with "<n> resources require pruning" after a successful full-app sync that
  left the app non-Synced and has `PruneSkipped` results not caused by "no prune". Pruning failures → `SyncFailed`
  with the API error text and the wave fails.
- **Configuration surface**: `operation.sync.prune`; `ApplicationSyncRequest.prune`.
- **Exposure**: API; CLI `--prune`; UI sync panel "Prune" checkbox (with typed "prune" confirmation dialogs when any/all selected resources require pruning).
- **Tests required**:
  - Positive: extra live resource deleted with `prune=true`, result `Pruned`.
  - Negative: `prune=false` → `PruneSkipped` "ignored (requires pruning)"; CLI exits 1 with "1 resources require pruning".
  - Edge: resource already terminating is not re-deleted.

### SYNC-007 Dry-run sync
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1422 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_context.go#L1584 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_context.go#L1719 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sync.go#L408 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1729
- **Maturity**: GA
- **Semantics**: Every apply/replace/create runs with kubectl *client* dry-run; SSA is disabled in dry-run;
  prune tasks report `Pruned` "pruned (dry run)" without deleting; hook and resource results that would be
  `Running` are reported `Succeeded`, so waves/phases advance immediately. Namespace auto-creation is also dry-run.
  Revision history is not written and no post-operation refresh is requested. Dry-run is allowed with local manifests
  and with revision overrides even when auto-sync is enabled.
- **Edge cases**: with CreateNamespace and a missing namespace, namespaced resources may fail dry-run because the
  namespace is not really created. CLI `--dry-run` does not apply the "operation not successful / requires pruning"
  exit checks.
- **Configuration surface**: `operation.sync.dryRun`; request `dryRun`.
- **Exposure**: API; CLI `--dry-run`; UI sync panel "Dry Run" flag.
- **Tests required**:
  - Positive: dry-run against changed manifests → Succeeded, cluster unchanged, history length unchanged.
  - Positive: dry-run with extra resource + prune → "pruned (dry run)", resource still exists.
  - Negative: invalid manifest → operation Failed.

### SYNC-008 Sync strategy: hook (default) vs apply
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1644 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1446 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sync.go#L311 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/sync-kubectl.md#L63
- **Maturity**: GA
- **Semantics**: `syncStrategy.hook{force}` (default when nil or `hook` set) runs hooks and waves. `syncStrategy.apply{force}`
  sets skipHooks=true: hook manifests are not created at all; non-hook resources still follow phases/waves. Skip-hooks
  is also forced for any selective sync (`resources` non-empty). Rollback always uses `apply` (SYNC-093).
- **Edge cases**: if both `apply` and `hook` are set, `apply` wins for `Force()` and `IsApplyStrategy()`. CLI `--strategy`
  other than `apply|hook|""` → fatal "Unknown sync strategy: '<s>'".
- **Configuration surface**: `operation.sync.syncStrategy.{apply,hook}`.
- **Exposure**: API; CLI `--strategy apply|hook`; UI sync panel "Apply Only" flag.
- **Tests required**:
  - Positive: app with PreSync job, strategy apply → job not created; strategy hook → created.
  - Positive: e2e parity with `TestSyncWithSkipHook`.

### SYNC-009 Force via sync strategy
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1652 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_context.go#L1401 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/utils/kube/resource_ops.go#L485 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/sync-kubectl.md#L83
- **Maturity**: GA
- **Semantics**: `force=true` applies to every resource of the operation: client-side apply runs like
  `kubectl apply --force` (delete and re-create when the patch fails); replace runs `kubectl replace --force`
  (delete then create). Equivalent per-resource behaviour is available via `Force=true` (SYNC-043).
- **Edge cases**: destructive; UI asks for confirmation with FORCE_WARNING. Force has no effect on creates.
- **Configuration surface**: `operation.sync.syncStrategy.{apply|hook}.force`.
- **Exposure**: API; CLI `--force`; UI sync panel "Force" flag (+ confirm dialog).
- **Tests required**:
  - Positive: immutable-field change (e.g. Service clusterIP/Job template) fails without force and succeeds with force (resource re-created, new UID).

### SYNC-010 Selective (partial) sync — `resources`
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1368 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1397 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sync.go#L313 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L977 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/app.go#L1528 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/app.go#L2209 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/selective_sync.md#L1
- **Maturity**: GA
- **Semantics**: `operation.sync.resources[]{group, kind, name, namespace}` restricts the sync to matching managed
  resources: match = name, kind and group equal and (namespace empty or equal) (`HasIdentity`, no wildcards server-side).
  Partial sync: hooks are skipped, revision history is not recorded, the completion event says "Partial sync operation".
  The CLI `--resource GROUP:KIND:NAME | GROUP:KIND:NAMESPACE/NAME | !...` supports `*` wildcards and `!` exclusions,
  evaluated client-side against `status.resources` (includes first, then excludes win) and expanded into concrete
  resources; `--label k=v` selects target manifests by label. The UI lists non-hook resources with all/out-of-sync/none
  selectors and sends `resources=null` when all are selected.
- **Edge cases**: `Exclude` is `json:"-"` — exclusions never reach the server. CLI errors: "resource should have
  GROUP:KIND:NAME, but instead got: <r>", "resource with namespace should have GROUP:KIND:NAMESPACE/NAME, but instead got: <r>",
  "No matching app resources found for resource filter: <...>", "No matching resources found for labels: <...>".
  Self-heal syncs are partial syncs (SYNC-022). Resources whose tracking id points to another object are filtered out
  (SYNC-098).
- **Configuration surface**: `operation.sync.resources[]`.
- **Exposure**: API `resources`; CLI `--resource`, `--label`; UI sync panel resource checkboxes ("WARNING: partial synchronization is not recorded in history").
- **Tests required**:
  - Positive: sync one Deployment of an app with two out-of-sync resources; only it changes; history unchanged; hooks not run.
  - Positive: namespace-qualified selection when names collide across namespaces.
  - Negative: CLI filter matching nothing → fatal message.

### SYNC-011 Source / sources override in SyncOperation
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1428 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sync.go#L79 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sync.go#L134
- **Maturity**: GA
- **Semantics**: If `operation.sync.source` or `operation.sync.sources` is set, the operation renders and syncs that
  source definition instead of `spec`; `syncResult.source/sources` record it. Otherwise the spec sources are used.
  Non-empty `sources` makes the operation multi-source. The API Sync always fills `source` from the spec for
  single-source apps; Rollback fills it from history.
- **Edge cases**: when written directly via kubectl the source override bypasses API-level checks; project
  source/destination permission checks still happen in comparison/sync (other area). Auto-sync loop detection compares
  `syncResult.source(s)` with the spec, so an override sync makes the next comparison treat the spec as "new".
- **Configuration surface**: `operation.sync.source`, `operation.sync.sources`.
- **Exposure**: CRD; API (indirect: Rollback).
- **Tests required**:
  - Positive: operation with `source.path` different from spec deploys the override path and records it in `syncResult.source`.

### SYNC-012 Local manifests sync (`manifests`, `argocd app sync --local`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1431 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2104 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L716 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1986 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/app.go#L1940 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/app_diff.go#L182 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/application_sources.md#L13
- **Maturity**: GA
- **Semantics**: The CLI renders the local directory client-side (`--local`, `--local-repo-root` default `/`) with the
  app's source settings and cluster version info, then sends YAML strings in `manifests`. The controller uses them
  as the target state instead of the repository (no repo-server call; revision empty). Until the next operation,
  periodic refreshes keep comparing against `status.operationState.operation.sync.manifests`.
  Requires RBAC `override`; refused with auto-sync unless dry run; refused when project source integrity is enforced
  (both API and controller: condition "Cannot use local manifests when source integrity is enforced").
- **Edge cases**: multi-source apps → CLI fatal "argocd cli does not work on multi-source app with --local flag";
  unparsable manifests → ComparisonError "Failed to load local manifests: ...". Audit event "initiated sync locally".
  Hook generated names use an empty revision segment.
- **Configuration surface**: `operation.sync.manifests[]`.
- **Exposure**: API `manifests`; CLI `--local`, `--local-repo-root`.
- **Tests required**:
  - Positive: `TestLocalManifestSync`/`TestLocalSync` parity: local change deployed; app compares to local manifests afterwards.
  - Negative: `TestNoLocalSyncWithAutosyncEnabled`; Positive `TestLocalSyncDryRunWithAutosyncEnabled`.
  - Authorization: no `override` → denied. Negative: source integrity enforced → rejected.

### SYNC-013 Per-operation sync options
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1434 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2121 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/app.go#L1955 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-sync-panel/application-sync-panel.tsx#L61
- **Maturity**: GA
- **Semantics**: The controller reads sync options only from `operation.sync.syncOptions` (not from the spec) — the
  spec list is copied into the operation by the API (when the request omits `syncOptions`) and by auto-sync. A request
  that includes `syncOptions` **replaces** the app-level list entirely. The CLI sends `syncOptions` only when
  `--replace`, `--server-side` or `--apply-out-of-sync-only` is given, and then sends only those options (dropping
  app-level options such as `CreateNamespace=true` for that operation). The UI pre-fills the panel with the app-level
  options and sends the edited full list.
- **Edge cases**: options are exact, case-sensitive strings (no trimming at app/operation level). Unknown strings are ignored.
- **Configuration surface**: `operation.sync.syncOptions[]`, request `syncOptions.items[]`.
- **Exposure**: API; CLI (3 flags); UI sync panel "Sync Options".
- **Tests required**:
  - Positive: app with `CreateNamespace=true`; CLI sync with `--server-side` to a missing namespace fails (option dropped) — parity of replacement semantics.
  - Positive: UI-style request with full list keeps app options.

### SYNC-014 Per-operation retry strategy
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2121 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/app.go#L2000 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-sync-panel/application-sync-panel.tsx#L274
- **Maturity**: GA
- **Semantics**: `operation.retry` = request `retryStrategy` if present, else `spec.syncPolicy.retry`, else zero value
  (no retries). CLI builds it only when `--retry-limit != 0` (with `--retry-backoff-duration` default 5s,
  `--retry-backoff-max-duration` default 3m0s, `--retry-backoff-factor` default 2, `--retry-refresh`). UI retry
  section initialised from spec retry.
- **Edge cases**: Rollback never sets `operation.retry`. Durations are serialized as Go duration strings ("5s", "3m0s").
- **Configuration surface**: `operation.retry`.
- **Exposure**: API; CLI `--retry-*`; UI retry options.
- **Tests required**:
  - Positive: failing sync with `--retry-limit 2` shows `retryCount` increments and final message "(retried 2 times)."

### SYNC-015 autoHealAttemptsCount (self-heal attempt counter)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1441 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2500 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/crds/application-crd.yaml#L121
- **Maturity**: GA
- **Semantics**: `operation.sync.autoHealAttemptsCount` (Go `SelfHealAttemptsCount`) is set only by auto-sync
  self-heal: copied from the previous operation and incremented by one for each self-heal operation. It drives the
  self-heal backoff (SYNC-023). A new-revision auto-sync or any manual sync starts at 0, which resets the backoff.
- **Edge cases**: if `status.operationState` was cleared (e.g. by a manual SetAppOperation), the counter restarts.
- **Configuration surface**: `operation.sync.autoHealAttemptsCount` (JSON name differs from Go name).
- **Exposure**: CRD (status/operation).
- **Tests required**:
  - Positive: consecutive drift corrections produce counts 1,2,3 in `status.operationState.operation.sync.autoHealAttemptsCount`.

### SYNC-016 Operation info and initiator
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1353 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/app.go#L216 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L1194
- **Maturity**: GA
- **Semantics**: `info[]` (name/value) is stored with the operation and visible in `operationState.operation.info`.
  `initiatedBy.username` is the API session user; `initiatedBy.automated=true` for controller-initiated operations.
  `initiatedBy` is copied into revision history entries. CLI `--info key=value` (repeatable, parsed as labels).
- **Edge cases**: CLI rejects malformed `--info` values via label parsing errors; info order is not preserved (map iteration).
- **Configuration surface**: `operation.info`, `operation.initiatedBy`.
- **Exposure**: API `infos`; CLI `--info`; UI shows initiator in history ("Initiated by").
- **Tests required**:
  - Positive: `--info ticket=ABC-1` visible in operationState; automated sync shows `automated: true` in history.

---

## B. Automated sync

### SYNC-017 Automated sync enablement (`automated`, `automated.enabled`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1531 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1608 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2855 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/auto_sync.md#L9
- **Maturity**: GA
- **Semantics**: Auto-sync is enabled iff `spec.syncPolicy.automated` is non-nil and `enabled` is nil or true.
  `enabled: false` disables it while preserving `prune/selfHeal/allowEmpty`. When an informer update flips auto-sync
  (or self-heal) from off to on, the controller requests an immediate refresh.
- **Edge cases**: `automated: {}` = enabled with prune/selfHeal/allowEmpty false. Built-in `toggle-auto-sync` action
  (SYNC-115) flips `enabled`.
- **Configuration surface**: `spec.syncPolicy.automated.{enabled, prune, selfHeal, allowEmpty}`.
- **Exposure**: CRD; CLI `app create/set --sync-policy automated|auto|automatic|manual|none` (SYNC-100); UI app summary "SYNC POLICY" toggle and "Toggle Auto-Sync" action.
- **Tests required**:
  - Positive: `automated: {}` app auto-syncs a new commit; `enabled: false` does not.
  - Positive: enabling auto-sync triggers a sync without waiting for the reconciliation interval.

### SYNC-018 Auto-sync trigger conditions
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2414 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2042 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/auto_sync.md#L134
- **Maturity**: GA
- **Semantics**: Evaluated after every app reconciliation, only if project sync windows allow an *automated* sync
  now (`CanSync(isManual=false, nil)`; otherwise log "Sync prevented by sync window"). Skip when: auto-sync disabled;
  `operation` already set; app has deletionTimestamp; app sync status is not `OutOfSync` (Synced/Unknown never
  auto-sync). Then prune-only, loop-prevention, self-heal and allowEmpty checks apply (SYNC-019..022). On success
  the operation is set via `SetAppOperation` and an event "Initiated automated sync to '<revs>'" is emitted.
- **Edge cases**: `ErrAnotherOperationInProgress` from a stale informer is silently ignored. Other SetAppOperation
  errors become a `SyncError` condition with the error text. Errors in sync window parsing are treated as "cannot sync".
- **Configuration surface**: reconciliation interval (`timeout.reconciliation`, other area).
- **Exposure**: CRD/controller only.
- **Tests required**:
  - Positive: OutOfSync app auto-syncs; Synced app never creates an operation.
  - Negative: active deny window → no operation created; app being deleted → none.

### SYNC-019 Automated prune (`automated.prune`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2444 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2465 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/auto_sync.md#L38
- **Maturity**: GA
- **Semantics**: Auto-sync operations carry `prune = automated.prune` (default false). If prune is disabled and
  every non-Synced resource is only `requiresPruning`, the auto-sync is skipped ("need to prune extra resources only
  but automated prune is disabled") — no operation, no condition.
- **Edge cases**: resource-level `Prune=false/confirm` still apply during auto-sync with prune.
- **Configuration surface**: `spec.syncPolicy.automated.prune`.
- **Exposure**: CRD; CLI `--auto-prune`; UI "PRUNE RESOURCES" checkbox.
- **Tests required**:
  - Positive: removing a manifest with auto-prune deletes the resource.
  - Negative: without auto-prune, removal leaves app OutOfSync and no operation is created.

### SYNC-020 allowEmpty protection
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2523 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/auto_sync.md#L58
- **Maturity**: GA
- **Semantics**: With `automated.prune=true` and `allowEmpty=false`, if every resource in `status.resources`
  requires pruning (the auto-sync would delete everything), auto-sync is refused with a `SyncError` condition
  "Skipping sync attempt to [<revs>]: auto-sync will wipe out all resources". `allowEmpty=true` allows it.
- **Edge cases**: an app with zero resources at all also satisfies "all need prune" (vacuous) — no operation and the
  condition is set. Manual syncs are never blocked by this.
- **Configuration surface**: `spec.syncPolicy.automated.allowEmpty`.
- **Exposure**: CRD; CLI `--allow-empty`.
- **Tests required**:
  - Negative: delete all manifests with auto-prune → SyncError condition, resources remain (`TestAutoSyncNotAllowEmpty`).
  - Positive: same with allowEmpty → resources pruned (`TestAutoSyncAllowEmpty`).

### SYNC-021 Auto-sync loop prevention for the same revision
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2485 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2568 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L416 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/auto_sync.md#L138
- **Maturity**: GA
- **Semantics**: "Already attempted" = previous `operationState.syncResult` targeted the same revision(s) (single:
  `syncResult.revision == desired`; multi: `syncResult.revisions` deep-equal) AND the same source(s) as the current
  spec (deep-equal). If the comparison found that the new revision cannot contain changes for this app
  (`revisionsMayHaveChanges=false`, e.g. manifest-generate-paths annotation and no changed paths, or same SHA), only
  sources are compared. If already attempted: previous phase not successful → no retry; `SyncError` condition
  "Failed last sync attempt to [<revs>]: <previous message>". Previous successful and selfHeal off → skip. Previous
  successful and selfHeal on → self-heal path (SYNC-022).
- **Edge cases**: `operationState` nil → not attempted. `syncResult` nil while the previous phase completed →
  treated as attempted (prevents loops after errors). Any spec source change (path, helm params, etc.) counts as new.
- **Configuration surface**: `argocd.argoproj.io/manifest-generate-paths` annotation (other area) affects detection.
- **Exposure**: controller only.
- **Tests required**:
  - Negative: failed auto-sync to SHA X is not retried for X (beyond retry strategy) and shows SyncError; new commit Y is attempted.
  - Positive: changing a helm parameter at the same SHA triggers a new auto-sync.
  - Parity with `TestAlreadyAttemptSync` table.

### SYNC-022 Self-heal (`automated.selfHeal`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2493 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2510 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/auto_sync.md#L77
- **Maturity**: GA
- **Semantics**: When the last successful sync already targeted the desired revision/sources but the app is
  OutOfSync again (live drift), self-heal creates a new automated operation once the backoff (SYNC-023) allows it.
  The operation's `resources` lists every resource whose status is not `Synced` (kind, group, name — no namespace),
  making it a partial sync: hooks do not run and no history entry is recorded.
  `autoHealAttemptsCount` is incremented.
- **Edge cases**: while backing off, a refresh is re-queued after the remaining duration and the log says "Skipping
  auto-sync: already attempted sync to ... (retrying in ...)". Multi-source apps: a change in any source triggers a
  normal (non-self-heal) auto-sync even when selfHeal is off.
- **Configuration surface**: `spec.syncPolicy.automated.selfHeal`.
- **Exposure**: CRD; CLI `--self-heal`; UI "SELF HEAL" checkbox.
- **Tests required**:
  - Positive: manual `kubectl scale` drift is reverted with selfHeal (`TestAutoSyncSelfHealEnabled`), not reverted without (`TestAutoSyncSelfHealDisabled`).
  - Positive: self-heal operation has `resources` set and does not append to history.

### SYNC-023 Self-heal timing (backoff, timeout)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2603 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-application-controller/commands/argocd_application_controller.go#L184 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-application-controller/commands/argocd_application_controller.go#L274 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cmd-params-cm.yaml#L71 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller_test.go#L4081
- **Maturity**: GA (cooldown flag Deprecated)
- **Semantics**: If `--self-heal-backoff-timeout-seconds` is non-zero (default 2), exponential backoff is used:
  delay(n) = the n-th step of {Duration=2s, Factor=3, Cap=300s} where n = current `autoHealAttemptsCount`
  (n=0 → 0, 1 → 2s, 2 → 6s, 3 → 18s, 4 → 54s, … capped). Remaining wait = delay − time since previous
  `operationState.finishedAt` (or full delay if unfinished). If backoff timeout is 0, a fixed
  `--self-heal-timeout-seconds` (default 0) minus time since finishedAt is used. Self-heal proceeds when remaining ≤ 0.
- **Edge cases**: `--self-heal-backoff-cooldown-seconds` (default 330) is parsed but marked deprecated "has no effect".
  Factor/cap 0 values are passed through to `wait.Backoff` as-is.
- **Configuration surface**: `argocd-cmd-params-cm` `controller.self.heal.timeout.seconds`, `controller.self.heal.backoff.timeout.seconds`, `controller.self.heal.backoff.factor`, `controller.self.heal.backoff.cap.seconds`, `controller.self.heal.backoff.cooldown.seconds`; env `ARGOCD_APPLICATION_CONTROLLER_SELF_HEAL_{TIMEOUT_SECONDS,BACKOFF_TIMEOUT_SECONDS,BACKOFF_FACTOR,BACKOFF_CAP_SECONDS,BACKOFF_COOLDOWN_SECONDS}`; flags `--self-heal-*`.
- **Exposure**: controller flags/config only.
- **Tests required**:
  - Positive: table parity with `TestSelfHealRemainingBackoff` (0→0, 1→2s, 2→6s, 3→18s, 4→54s, 5/6→cap).
  - Positive: backoff disabled (0) uses fixed timeout.

### SYNC-024 Auto-sync operation content and default retry
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2458 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2475
- **Maturity**: GA
- **Semantics**: Auto-sync builds `operation.sync{source: spec source (nil for multi-source), revision:
  status.sync.revision, revisions: status.sync.revisions, sources: spec.sources, prune: automated.prune,
  syncOptions: spec.syncPolicy.syncOptions}`, `initiatedBy.automated=true` and `retry = spec.syncPolicy.retry` or,
  when unset, `{limit: 5}` (default backoff 5s ×2, max 3m). After completion an automated operation triggers a
  `CompareWithLatest` refresh (no forced revision resolution).
- **Edge cases**: the revision is the one resolved by the last comparison, not re-resolved at operation creation.
- **Configuration surface**: `spec.syncPolicy.retry`.
- **Exposure**: controller only.
- **Tests required**:
  - Positive: failing auto-sync without spec retry retries 5 times ("(retried 5 times).").
  - Positive: operation carries spec syncOptions.

### SYNC-025 Auto-sync errors as SyncError condition
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2044 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1906
- **Maturity**: GA
- **Semantics**: Each reconciliation that evaluates auto-sync replaces `SyncError` conditions: set when autoSync
  returns a condition (failed previous attempt, allowEmpty wipe-out, SetAppOperation error); cleared otherwise.
  Not touched when the sync window blocks evaluation.
- **Edge cases**: other condition types are untouched.
- **Configuration surface**: `status.conditions[type=SyncError]`.
- **Exposure**: CRD status; UI conditions panel; CLI `app get`.
- **Tests required**:
  - Positive: condition appears after failed auto-sync and disappears after a successful sync of a new revision.

---

## C. Retry

### SYNC-026 Retry limit
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1544 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1703 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/auto_sync.md#L97
- **Maturity**: GA
- **Semantics**: When an operation ends `Failed` or `Error` and it was not terminating, it is retried while
  `retryCount < limit` or `limit < 0` (unlimited). `limit: 0` → no retries. The operation is put back to `Running`
  with `finishedAt=now`, `retryCount++` and message "<msg>. Retrying attempt #<n> at <h:mmPM>." When retries are
  exhausted the final message gets " (retried <n> times)." appended.
- **Edge cases**: terminated operations (user or timeout) are never retried. Both `Failed` and `Error` phases retry.
- **Configuration surface**: `spec.syncPolicy.retry.limit`, `operation.retry.limit`.
- **Exposure**: CRD; CLI `--sync-retry-limit` (app set) / `--retry-limit` (sync); UI retry options.
- **Tests required**:
  - Positive: `TestProcessRequestedAppOperation_FailedHasRetries`, `..._RunningPreviouslyFailed`, `..._FailedNoRetries` parity.
  - Positive: `limit: -1` keeps retrying.
  - Negative: `TestProcessRequestedAppOperation_HasRetriesTerminated` — terminated op not retried.

### SYNC-027 Retry backoff (duration, factor, maxDuration)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1567 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1598 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/application_defaults.go#L6
- **Maturity**: GA
- **Semantics**: next retry at = lastAttempt + min(maxDuration, duration × factor^retryCount). Defaults: duration
  5s, factor 2, maxDuration 3m. `duration`/`maxDuration` accept a bare integer (seconds) or a Go duration string.
  maxDuration ≤ 0 disables the cap. While the time has not come, the controller re-queues a refresh for the remaining
  time ("Skipping retrying in-progress operation. Attempting again at: <RFC3339>").
- **Edge cases**: unparsable duration → operation `Error` "<msg> (failed to retry: unable to parse <v> as a duration)"
  (or `Error` with that message when evaluated on resume). `factor` is an integer.
- **Configuration surface**: `retry.backoff.{duration, factor, maxDuration}`.
- **Exposure**: CRD; CLI `--sync-retry-backoff-*` / `--retry-backoff-*`; UI retry options.
- **Tests required**:
  - Positive: `TestProcessRequestedAppOperation_RunningPreviouslyFailedBackoff` parity; numeric "10" == 10s.
  - Negative: `duration: "abc"` → Error with "failed to retry".

### SYNC-028 Retry refresh (`retry.refresh`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1649 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/auto_sync.md#L117
- **Maturity**: GA
- **Semantics**: On each retry attempt with `refresh: true`, `operation.sync.revision` and `revisions` are cleared so the
  latest revision of the source is resolved; message "Retrying operation with latest revisions. Attempt #<n>".
  Without it the originally resolved revision is retried ("Retrying operation. Attempt #<n>"). In both cases
  `syncResult` and `finishedAt` are reset before the attempt.
- **Edge cases**: a fix pushed to the branch is picked up only with refresh=true.
- **Configuration surface**: `retry.refresh`.
- **Exposure**: CRD; CLI `--sync-retry-refresh` / `--retry-refresh`; UI retry options.
- **Tests required**:
  - Positive: `TestAutoSyncRetryAndRefreshEnabled` / `TestSyncWithRetryAndRefreshEnabled` parity.

---

## D. Sync options

### SYNC-029 Sync option parsing and precedence (app-level vs resource annotation)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/common/types.go#L10 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/resource/annotations.go#L16 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1496 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/sync-options.md#L1
- **Maturity**: GA
- **Semantics**: Resource annotation `argocd.argoproj.io/sync-options` is a comma-separated list; items are
  whitespace-trimmed, empty items dropped, duplicates removed; boolean options are matched as exact strings
  (e.g. `Prune=false`), key=value options by prefix (`Prune=`). App/operation-level options are a list matched by exact
  string. Which levels each option honours:
  `Prune=false|confirm` and `Delete=false|confirm`: resource value overrides app value.
  `Replace=true`, `Force=true`: resource (target **or live**) OR app-level (Force only via strategy).
  `ServerSideApply=true`: app OR resource; resource `ServerSideApply=false` overrides app.
  `Validate=false`, `SkipDryRunOnMissingResource=true`: app OR resource (target).
  `PruneLast=true`: app OR resource (live object).
  App/operation-only: `CreateNamespace`, `ApplyOutOfSyncOnly`, `PrunePropagationPolicy`, `RespectIgnoreDifferences`,
  `FailOnSharedResource`, `ClientSideApplyMigration=false`.
- **Edge cases**: `Replace=false` and `ApplyOutOfSyncOnly=false` constants exist but have no effect anywhere.
  Unknown options and wrong case are silently ignored. App-level strings are not trimmed.
- **Configuration surface**: annotation `argocd.argoproj.io/sync-options`; `spec.syncPolicy.syncOptions[]`; `operation.sync.syncOptions[]`.
- **Exposure**: CRD/annotations; CLI `--sync-option` (app create/set, `!` prefix removes); UI sync options form.
- **Tests required**:
  - Positive: annotation " Prune=false , Validate=false" honours both.
  - Positive: resource `ServerSideApply=false` overrides app `ServerSideApply=true`.
  - Negative: annotation `Replace=false` on a resource with app `Replace=true` still replaces (parity).

### SYNC-030 Prune=false
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_context.go#L333 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_context.go#L1457 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/sync-options.md#L7
- **Maturity**: GA
- **Semantics**: A prune task whose live object (or, absent an annotation, the app/operation default `Prune=`) says
  `false` is not deleted: result `PruneSkipped` "ignored (no prune)". Such results are excluded from the CLI's
  "requires pruning" count. The app remains OutOfSync (use compare-option `IgnoreExtraneous` to hide).
- **Edge cases**: resource `Prune=true` overrides app `Prune=false`. Only evaluated when the sync has prune enabled
  (otherwise "ignored (requires pruning)").
- **Configuration surface**: annotation `Prune=false`; app `Prune=false`.
- **Exposure**: CRD/annotations; UI sync options "Prune" select (true/false/confirm).
- **Tests required**:
  - Positive: `TestDoNotPruneResourceLevelPruneFalse`, `TestDoNotPruneAppLevelPruneFalse`, e2e `TestSyncOptionPruneFalse*` parity.

### SYNC-031 Prune=confirm (and pruning confirmation)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_context.go#L322 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_context.go#L1618 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sync.go#L332 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L4173 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-details/application-details.tsx#L1387 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/sync-options.md#L37
- **Maturity**: GA
- **Semantics**: If any prune task in the wave being run requires confirmation (resource or default `Prune=confirm`)
  and the app is not confirmed, the wave returns *pending* before deleting or applying anything; operation stays
  `Running` with message "waiting for pruning confirmation of <group>/<kind>/<name>[ and N more resources]".
  Confirmation = Application annotation `argocd.argoproj.io/deletion-approved` holding an RFC3339 timestamp `>=`
  `operationState.startedAt`. After confirmation the prune proceeds. Resource status `requiresDeletionConfirmation`
  is true for such resources.
- **Edge cases**: an approval timestamp older than the operation start does not count; non-RFC3339 values are
  ignored. Confirmation is requested even when the sync has `prune=false` (the task is then `PruneSkipped` after
  confirmation). UI shows "Confirm Pruning" only while phase is Running and some resource requires confirmation.
- **Configuration surface**: annotation `Prune=confirm`; app `Prune=confirm`; Application annotation `argocd.argoproj.io/deletion-approved`.
- **Exposure**: CRD; CLI `argocd app confirm-deletion` (SYNC-104); UI "Confirm Pruning" button.
- **Tests required**:
  - Positive: `TestPruneConfirmResourceLevel` / `TestPruneConfirmAppLevel` parity (message exact).
  - Negative: approval timestamp before startedAt → still waiting.

### SYNC-032 Delete=false
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1333 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/sync-options.md#L111
- **Maturity**: GA
- **Semantics**: During cascading Application deletion, a live resource with `Delete=false` (resource annotation, or
  app-level `Delete=` default when no annotation) is left in the cluster. Other exclusions in the same check: CRDs,
  the Application itself (self-managed), and `helm.sh/resource-policy: keep`.
- **Edge cases**: does not affect pruning during sync (use Prune=false). Resource `Delete=true` overrides app `Delete=false`.
- **Configuration surface**: annotation `Delete=false`; app `Delete=false`.
- **Exposure**: CRD/annotations; UI sync options "Delete" select.
- **Tests required**:
  - Positive: cascade-delete app; resource with Delete=false survives (`Test_syncDeleteOption` parity).

### SYNC-033 Delete=confirm
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L1171 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1428 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/sync-options.md#L136
- **Maturity**: GA
- **Semantics**: Cascading deletion waits (no deletes at all) while any to-be-deleted resource has
  `requiresDeletionConfirmation` and the app's `deletion-approved` timestamp is not `>=` its deletionTimestamp.
  `requiresDeletionConfirmation` = `Delete=confirm` or `Prune=confirm` on target or live object (or app default).
- **Edge cases**: UI "Confirm Deletion" sets the annotation to `new Date().toISOString()`.
- **Configuration surface**: annotation/app `Delete=confirm`; annotation `argocd.argoproj.io/deletion-approved`.
- **Exposure**: CRD; CLI `confirm-deletion`; UI "Confirm Deletion".
- **Tests required**:
  - Positive: e2e `TestDeletionConfirmation`, `TestDeletionConfirmationAppLevel` parity.

### SYNC-034 Validate=false
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sync.go#L320 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_context.go#L1713 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/utils/kube/resource_ops.go#L469 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/sync-options.md#L70
- **Maturity**: GA
- **Semantics**: Apply/create run with schema validation `Ignore` instead of `Strict` (`kubectl apply --validate=false`)
  for the annotated resource, or for all resources when set at app/operation level. Applies to both dry-run and wet run.
- **Edge cases**: replace (`kubectl replace`) path does not pass a validation flag.
- **Configuration surface**: annotation/app `Validate=false`.
- **Exposure**: CRD; UI "Skip Schema Validation" checkbox.
- **Tests required**:
  - Positive/negative: e2e `TestSyncOptionsValidateFalse` / `TestSyncOptionsValidateTrue` parity.

### SYNC-035 SkipDryRunOnMissingResource=true
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_context.go#L1067 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sync.go#L334 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/sync-options.md#L82
- **Maturity**: GA
- **Semantics**: If API discovery reports the resource's GVK as NotFound, the task is marked skip-dry-run instead of
  failing when: the option is set app-wide, OR the target is annotated, OR a CRD for that group/kind is part of the
  same sync (automatic). The real apply still happens (and fails if the type still does not exist).
- **Edge cases**: dry-run still runs when the CRD already exists. Non-NotFound discovery errors are not skipped.
- **Configuration surface**: annotation/app `SkipDryRunOnMissingResource=true`.
- **Exposure**: CRD/annotations.
- **Tests required**:
  - Positive: CR whose CRD is created by an operator later: annotated → sync proceeds; not annotated → "one or more synchronization tasks are not valid: ...".
  - Positive: CRD + CR in same app sync without any option.

### SYNC-036 CreateNamespace=true
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sync.go#L337 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sync_namespace.go#L14 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_context.go#L1161 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_context.go#L514 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/sync-options.md#L434
- **Maturity**: GA
- **Semantics**: If `spec.destination.namespace` is non-empty and no target manifest is a Namespace with that name,
  a synthetic `v1/Namespace` task (phase PreSync) is added when the namespace does not exist (or always, when
  managedNamespaceMetadata is set — SYNC-037). A missing-namespace creation task is executed *before* the dry-run
  (so dry-run of namespaced objects can succeed); failure → operation `Failed` "the namespace failed to apply, reason:
  <msg>" (SyncFail hooks are not started for this failure).
  Result row: kind Namespace, status Synced, message "namespace/<ns> created", hookPhase Running, syncPhase PreSync.
  The created namespace carries no tracking metadata, so it is not pruned or deleted with the app.
- **Edge cases**: namespace GET errors other than NotFound → SyncFailed/Error result "namespace auto creation failed: ...".
  A Namespace manifest for the destination namespace in Git always wins over the synthetic one, and a missing one is
  also created before dry-run even without this option. Hooks are not considered when checking for an explicit
  Namespace manifest.
- **Configuration surface**: app/operation `CreateNamespace=true`; CLI `--sync-option CreateNamespace=true`.
- **Exposure**: CRD; UI "Auto-Create Namespace" checkbox.
- **Tests required**:
  - Positive: e2e `TestSyncWithCreateNamespace`, `TestNamespaceAutoCreation` parity.
  - Negative: `TestSyncWithCreateNamespaceAndDryRunError` — namespace still created even when dry-run fails.

### SYNC-037 managedNamespaceMetadata
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1512 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sync_namespace.go#L30 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L828 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L3699 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sync.go#L369 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/sync-options.md#L454
- **Maturity**: GA
- **Semantics**: With `CreateNamespace=true` and `spec.syncPolicy.managedNamespaceMetadata{labels, annotations}`, the
  synthetic namespace is applied on every sync with exactly those labels and annotations plus annotation
  `argocd.argoproj.io/sync-options: ServerSideApply=true` (so existing foreign labels survive via SSA field ownership).
  The value is stored in `operationState.syncResult.managedNamespaceMetadata`; if the spec differs from that stored
  value the app is OutOfSync and a refresh is triggered. If tracking metadata is added to the namespace, the
  comparison synthesises a matching target so the namespace is neither OutOfSync nor pruned.
- **Edge cases**: without CreateNamespace nothing happens. A Git-defined Namespace manifest overrides the metadata.
  Adopting a namespace previously managed client-side may drop existing labels (documented).
- **Configuration surface**: `spec.syncPolicy.managedNamespaceMetadata.{labels,annotations}`.
- **Exposure**: CRD only.
- **Tests required**:
  - Positive: `TestPersistManagedNamespaceMetadataState`, `TestUpdatedManagedNamespaceMetadata`, `TestCompareAppStateManagedNamespaceMetadataWithLiveNsDoesNotGetPruned` parity.
  - Positive: changing a label in managedNamespaceMetadata makes app OutOfSync; sync updates the namespace.

### SYNC-038 PruneLast=true
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_context.go#L1127 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sync.go#L322 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/sync-options.md#L211
- **Maturity**: GA
- **Semantics**: Affected prune tasks get wave = (max wave of Sync-phase tasks, including prune tasks, floored at 0) + 1,
  i.e. they run as an implicit final wave of the Sync phase after all other waves are applied and healthy. App-level
  applies to all prune tasks; the annotation is read from the live object.
- **Edge cases**: overrides the reverse-wave ordering of SYNC-063 for those tasks. PostSync hooks still run after them.
- **Configuration surface**: app/operation `PruneLast=true`; annotation `PruneLast=true` (live object).
- **Exposure**: CRD; UI "Prune Last" checkbox.
- **Tests required**:
  - Positive: `TestPruneLast` parity (wave numbers).

### SYNC-039 PrunePropagationPolicy
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sync.go#L218 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_context.go#L1474 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/sync-options.md#L196
- **Maturity**: GA
- **Semantics**: `PrunePropagationPolicy=foreground|background|orphan` selects the DeleteOptions propagation policy;
  default foreground. The same delete options are used for prune deletions **and** hook deletions during sync
  (BeforeHookCreation, HookSucceeded/HookFailed, terminate).
- **Edge cases**: any other value → foreground. Exact-string match (first matching of background, foreground, orphan in that order).
- **Configuration surface**: app/operation `PrunePropagationPolicy=<policy>`.
- **Exposure**: CRD; UI "Prune Propagation Policy" select.
- **Tests required**:
  - Positive: orphan policy prunes a Deployment while its ReplicaSet remains.
  - Positive: `TestSyncContext_GetDeleteOptions_*` parity.

### SYNC-040 ApplyOutOfSyncOnly=true
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_context.go#L761 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sync.go#L323 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/sync-options.md#L164
- **Maturity**: GA
- **Semantics**: Non-hook tasks whose diff result says "not modified" and that have both target and live objects are
  dropped from the dry-run and from execution. Hooks, new resources and prune tasks always run. Unlike selective
  sync, hooks run and history is recorded.
- **Edge cases**: diff results are keyed on the normalized live (or predicted live) object; for cluster-scoped
  resources the live namespace is used for matching. Resources without a diff entry are applied.
- **Configuration surface**: app/operation `ApplyOutOfSyncOnly=true`; CLI sync `--apply-out-of-sync-only`.
- **Exposure**: CRD; CLI; UI "Apply Out of Sync Only".
- **Tests required**:
  - Positive: `TestSync_ApplyOutOfSyncOnly`, `TestSync_ApplyOutOfSyncOnly_ClusterResources`, e2e `TestSyncWithApplyOutOfSyncOnly` parity (only changed resources in syncResult).

### SYNC-041 Replace=true
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_context.go#L1400 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_context.go#L1418 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/sync-options.md#L233
- **Maturity**: GA
- **Semantics**: Instead of apply: existing object → `kubectl replace` (with `--force` when force); missing object →
  `kubectl create` (with validation). Exceptions: CRDs and Namespaces are *updated* (PUT with live resourceVersion,
  message "<Kind>/<name> updated") to avoid deleting all CR instances / namespace contents. Replace takes precedence
  over server-side apply. Triggered by app/operation option or the annotation on target or live object.
- **Edge cases**: update errors produce message "error when updating: <err>". The server can refuse operation-level
  replace (SYNC-047). UI requires typing "replace" to confirm.
- **Configuration surface**: app/operation `Replace=true`; annotation `Replace=true`; CLI sync `--replace`.
- **Exposure**: CRD; CLI; UI "Replace" checkbox (only when server allows).
- **Tests required**:
  - Positive: `TestSync_Replace`, e2e `TestSyncOptionReplace`, `TestSyncOptionReplaceFromCLI` parity.
  - Edge: CRD with Replace=true is updated, CR instances survive.

### SYNC-042 Force=true (resource annotation)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_context.go#L1401 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/sync-options.md#L263
- **Maturity**: GA
- **Semantics**: Resource-level equivalent of strategy force (SYNC-009), read from target or live annotations. Commonly
  combined with `Replace=true` to delete/re-create (e.g. Jobs). With apply, it maps to `kubectl apply --force`.
- **Edge cases**: `Force=true` in app-level syncOptions is ignored (not read).
- **Configuration surface**: annotation `Force=true`.
- **Exposure**: annotations only.
- **Tests required**:
  - Positive: `TestSync_Force`, e2e `TestSyncWithForceReplace` parity.
  - Negative: app-level `Force=true` has no effect.

### SYNC-043 ServerSideApply=true / ServerSideApply=false
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_context.go#L1281 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/utils/kube/resource_ops.go#L486 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L128 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L910 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/sync-options.md#L279
- **Maturity**: GA
- **Semantics**: Apply uses server-side apply with field manager `argocd-controller` and force-conflicts. Enabled by
  app/operation option or resource annotation; resource `ServerSideApply=false` disables it. Never used during dry-run
  (client dry-run validation). RBAC objects bypass `kubectl auth reconcile` under SSA. App-level SSA also switches the
  diff to structured-merge diff (comparison area). Partial manifests (patches) are possible together with `Validate=false`.
- **Edge cases**: Replace=true takes precedence. Message on annotation-size errors is augmented with a hint to use SSA
  (SYNC-075).
- **Configuration surface**: app/operation `ServerSideApply=true`; annotation `ServerSideApply=true|false`; CLI sync `--server-side`.
- **Exposure**: CRD; CLI; UI "Server-Side Apply".
- **Tests required**:
  - Positive: `TestSync_ServerSideApply`, `TestSyncContext_ServerSideApplyWithDryRun`, e2e `TestMultiSync*ServerSide*` parity; managedFields show `argocd-controller` with operation Apply.

### SYNC-044 ClientSideApplyMigration (CSA → SSA field ownership migration)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_context.go#L1302 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_context.go#L1328 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sync.go#L228 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/sync-options.md#L360
- **Maturity**: GA
- **Semantics**: Enabled by default. Before a non-dry-run server-side apply, if the live object has a managedFields entry
  for manager M (default `kubectl-client-side-apply`, overridable by Application annotation
  `argocd.argoproj.io/client-side-apply-migration-manager`) with operation `Update`, the controller fetches the fresh
  object and JSON-patches managedFields (k8s `csaupgrade`) to move M's fields to `argocd-controller`, retrying on
  conflict. Disabled with `ClientSideApplyMigration=false`.
- **Edge cases**: managers with operation `Apply` are not migrated. Failure → `SyncFailed` "Failed to perform
  client-side apply migration for <key>: <err>".
- **Configuration surface**: app/operation `ClientSideApplyMigration=false`; Application annotation `argocd.argoproj.io/client-side-apply-migration-manager`.
- **Exposure**: CRD only.
- **Tests required**:
  - Positive: `TestNeedsClientSideApplyMigration`, `TestPerformCSAUpgradeMigration_*`, `TestClientSideApplyMigration` parity.
  - Negative: option false → no managedFields patch.

### SYNC-045 RespectIgnoreDifferences=true
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sync.go#L239 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sync.go#L421 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sync.go#L548 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/sync-options.md#L413
- **Maturity**: GA
- **Semantics**: Before syncing, each target that has a live counterpart is patched so that fields ignored by
  `spec.ignoreDifferences` (and system-level ignore rules) carry the live values: normalize live and target, compute
  the normalizer's patch on live (strategic merge for built-in types, JSON merge otherwise) excluding `status`, apply it
  to the normalized target, then restore non-ignored fields clobbered by `patchStrategy: replace` parents. New
  resources (no live) are applied as-is.
- **Edge cases**: normalization errors → operation `Error` "Failed to normalize target resources: <err>". Live
  `status` is never copied into the target.
- **Configuration surface**: app/operation `RespectIgnoreDifferences=true`; `spec.ignoreDifferences`.
- **Exposure**: CRD; UI "Respect Ignore Differences".
- **Tests required**:
  - Positive: ignoring `/spec/replicas` keeps HPA-set replicas after sync (`TestNormalizeTargetResources*` parity incl. PDB selector case).

### SYNC-046 FailOnSharedResource=true
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sync.go#L156 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sync.go#L593 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/sync-options.md#L400
- **Maturity**: GA
- **Semantics**: If the app currently has a `SharedResourceWarning` condition (SYNC-087) and the operation has this
  option, the operation fails immediately (phase `Failed`) with "Shared resource found: <first condition message>"
  before any apply.
- **Edge cases**: uses conditions from the last reconciliation, not a fresh check. Without the option, shared
  resources are applied (and re-labelled) normally.
- **Configuration surface**: app/operation `FailOnSharedResource=true`.
- **Exposure**: CRD only.
- **Tests required**:
  - Negative: two apps managing the same ConfigMap; second app with the option → Failed with message prefix "Shared resource found: ".

### SYNC-047 API-level replace gate (`server.sync.replace.allowed`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2134 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-server/commands/argocd_server.go#L343 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cmd-params-cm.yaml#L167
- **Maturity**: GA
- **Semantics**: When the API server runs with replace disallowed, a Sync whose effective options (request or
  app-level) contain `Replace=true` is refused: `FailedPrecondition` "sync with replace was disabled on the API
  Server level via the server configuration". The UI hides the Replace checkbox (settings `syncWithReplaceAllowed`).
- **Edge cases**: resource-level `Replace=true` annotations and automated syncs are not affected (controller-side).
- **Configuration surface**: `argocd-cmd-params-cm` `server.sync.replace.allowed` (default true); env `ARGOCD_SYNC_WITH_REPLACE_ALLOWED`; flag `--sync-with-replace-allowed`.
- **Exposure**: API; UI.
- **Tests required**:
  - Negative: server flag false + CLI `--replace` → FailedPrecondition.
  - Positive: annotation-level replace still works with the flag false.

---

## E. Phases, waves and hooks

### SYNC-048 Sync phases PreSync / Sync / PostSync
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/common/types.go#L67 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_phase.go#L10 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_tasks.go#L15 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/sync-waves.md#L5
- **Maturity**: GA
- **Semantics**: Non-hook resources always belong to phase `Sync`. Hooks (annotation `argocd.argoproj.io/hook`)
  belong to every listed phase (one task per phase). Phase order: PreSync(-1) < Sync(0) < PostSync(1) < SyncFail(2).
  PreSync hooks complete before Sync-phase applies; PostSync runs after all Sync-phase tasks (including non-hook
  resources) are applied and Healthy. A failing task stops later phases and triggers SyncFail (SYNC-049).
- **Edge cases**: a hook listing `PreSync,PostSync` is created twice with distinct names when it uses generateName;
  with a fixed name the second phase reuses the same object (BeforeHookCreation deletes/recreates it).
- **Configuration surface**: annotation `argocd.argoproj.io/hook: PreSync|Sync|PostSync[,...]`.
- **Exposure**: annotations; results show `syncPhase` and `hookType`.
- **Tests required**:
  - Positive: e2e `TestPreSyncHookSuccessful`, `TestSyncHookSuccessful`, `TestPostSyncHookSuccessful` parity.
  - Negative: `TestPreSyncHookFailure`, `TestSyncHookFailure`, `TestPostSyncHookFailure` parity (later phases not run).

### SYNC-049 SyncFail hooks
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_context.go#L630 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_context.go#L906 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/sync-waves.md#L35
- **Maturity**: GA
- **Semantics**: SyncFail hooks are excluded from normal execution and only run when the operation fails during
  execution (an apply/prune failure in a step, or a completed-unsuccessful task). They do not run for task-validation
  failures ("not valid"), dry-run failures, or namespace auto-creation failures. While they run the operation stays `Running`; when all have
  completed the operation is `Failed` with "<reason>[, reason: <task messages>]" and, if any SyncFail hook failed, a
  second line "one or more SyncFail hooks failed, reason: <messages>". With no SyncFail hooks the operation fails
  immediately. If running hooks exist when an apply fails, SyncFail hooks are started and the operation waits.
- **Edge cases**: SyncFail hooks never run on success or on termination. Duplicate messages are de-duplicated.
- **Configuration surface**: annotation `argocd.argoproj.io/hook: SyncFail`.
- **Exposure**: annotations.
- **Tests required**:
  - Positive: `TestSync_FailedSyncWithSyncFailHook_*`, e2e `TestSyncFailHookPodFailure`, `TestSyncFailHookPodFailureSyncFailFailure` parity.
  - Negative: `TestSync_SuccessfulSyncWithSyncFailHook` — not run on success.

### SYNC-050 Skip hook
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/hook/hook.go#L34 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L981 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/health.go#L27 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/sync-waves.md#L11
- **Maturity**: GA
- **Semantics**: `argocd.argoproj.io/hook: Skip` (as the only valid type) means the manifest is never applied, is not a
  hook, has no sync status and does not affect app sync/health. If combined with other types (e.g. `Skip,PreSync`),
  Skip is ignored and the object is a normal hook for the other phases.
- **Edge cases**: an existing live object with Skip is not pruned (target exists).
- **Configuration surface**: annotation value `Skip`.
- **Exposure**: annotations.
- **Tests required**:
  - Positive: e2e `TestHookSkip` parity.

### SYNC-051 PreDelete hooks
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/hook.go#L32 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/hook.go#L97 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1406 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2077 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L611 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/sync-waves.md#L57
- **Maturity**: GA
- **Semantics**: Manifests annotated `argocd.argoproj.io/hook: PreDelete` (or `helm.sh/hook: pre-delete`) are removed
  from normal syncs (unless also annotated with a sync-phase hook type) and make the controller add finalizers
  `pre-delete-finalizer.argocd.argoproj.io` and `pre-delete-finalizer.argocd.argoproj.io/cleanup`. On Application
  deletion, before any resource is deleted, the hooks are rendered from the spec target revisions, namespaced hooks
  without namespace get the destination namespace, each is validated against project permissions, tracking metadata
  is added, and they are created. The controller waits until all are Healthy (no health check = Healthy), then removes
  the pre-delete finalizer and continues cascading deletion. Degraded hooks are deleted (to allow a retry with fixed
  manifests) and the error "PreDelete hook(s) failed: <ns/name,...>" becomes a `DeletionError` condition. After
  resource deletion, the cleanup finalizer phase deletes the hooks per delete policy (none = always delete;
  HookSucceeded when aggregate Healthy; HookFailed when aggregate Degraded) and waits for them to disappear.
- **Edge cases**: hook forbidden by the project → "PreDelete hook <key> is not permitted: <err>". Existing object with
  the same key is not re-created. AlreadyExists on create is ignored. Finalizers are only updated when the app is
  not already being deleted. Not run during pruning.
- **Configuration surface**: annotations `argocd.argoproj.io/hook: PreDelete`, `helm.sh/hook: pre-delete`; finalizers `pre-delete-finalizer.argocd.argoproj.io[/cleanup]`.
- **Exposure**: annotations; Application finalizers.
- **Tests required**:
  - Positive: e2e `TestPreDeleteHook`; Recovery: `TestPreDeleteHookFailureAndRetry`; Authorization: `TestPreDeleteHookProjectRestriction` parity.

### SYNC-052 PostDelete hooks
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/hook.go#L37 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1489 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1523 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/sync-waves.md#L80
- **Maturity**: GA
- **Semantics**: Same mechanics as PreDelete but annotation `PostDelete` / `helm.sh/hook: post-delete`, finalizers
  `post-delete-finalizer.argocd.argoproj.io[/cleanup]`, executed after all app resources are deleted, then cleaned up
  per delete policy.
- **Edge cases**: destination cluster unknown → all hook finalizers are removed and deletion completes without hooks.
  Failure message "PostDelete hook(s) failed: ...". Impersonation applies to hook creation (SYNC-091).
- **Configuration surface**: as above.
- **Exposure**: annotations; finalizers.
- **Tests required**:
  - Positive: e2e `TestPostDeleteHook` parity; Authorization: `TestFinalizeAppDeletion_DeleteHookProjectRestriction`.

### SYNC-053 Hook annotation parsing and unsupported values
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/hook/hook.go#L26 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/hook/hook.go#L43 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/ignore/ignore.go#L10 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/reconcile.go#L13
- **Maturity**: GA
- **Semantics**: Hook value is CSV (trimmed, deduped). Valid Argo CD types: PreSync, Sync, PostSync, SyncFail, Skip.
  If any valid Argo CD type is present, Helm hook annotations on that object are ignored for typing. An object that
  is a hook but has no valid type (e.g. `argocd.argoproj.io/hook: Foo`, `helm.sh/hook: test`) is dropped entirely —
  neither applied, pruned nor tracked. Hooks are never pruned as extraneous resources.
- **Edge cases**: `helm.sh/hook: crd-install` is not a hook (normal resource). PreDelete/PostDelete-only objects are
  filtered before the engine (SYNC-051/052).
- **Configuration surface**: annotations `argocd.argoproj.io/hook`, `helm.sh/hook`.
- **Exposure**: annotations.
- **Tests required**:
  - Negative: unknown hook value → object never created.
  - Positive: `helm.sh/hook: crd-install` CRD applied as normal resource.

### SYNC-054 Hook delete policy HookSucceeded
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_task.go#L142 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_context.go#L621 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/sync-waves.md#L41
- **Maturity**: GA
- **Semantics**: Hooks with `argocd.argoproj.io/hook-delete-policy: HookSucceeded` (or Helm `hook-succeeded`) are
  deleted only when the *whole operation* finishes successfully (all phases), not when the individual hook succeeds.
- **Edge cases**: if the operation fails, these hooks are kept. Policies are CSV; Argo CD and Helm policies are unioned.
- **Configuration surface**: annotation `argocd.argoproj.io/hook-delete-policy`.
- **Exposure**: annotations.
- **Tests required**:
  - Positive: `TestSync_HooksDeletedAfterSyncSucceeded`, e2e `TestHookDeletePolicyHookSucceededHookExit0` parity.
  - Negative: `TestHookDeletePolicyHookSucceededHookExit1` (kept on failure), `TestSync_HooksNotDeletedIfPhaseNotCompleted`.

### SYNC-055 Hook delete policy HookFailed
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_task.go#L146 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_context.go#L625 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_context.go#L829
- **Maturity**: GA
- **Semantics**: Completed hooks with `HookFailed` (or Helm `hook-failed`) are deleted when the operation fails (any
  completed-unsuccessful task, or apply failure once SyncFail handling completed) — including hooks that themselves
  succeeded. On termination, running hooks and completed HookFailed hooks are deleted.
- **Edge cases**: kept when the operation succeeds.
- **Configuration surface**: annotation value `HookFailed`.
- **Exposure**: annotations.
- **Tests required**:
  - Positive: `TestSync_HooksDeletedAfterSyncFailed`, e2e `TestHookDeletePolicyHookFailedHookExit1` parity.
  - Negative: e2e `TestHookDeletePolicyHookFailedHookExit0` (kept on success).

### SYNC-056 Hook delete policy BeforeHookCreation (default)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/hook/delete_policy.go#L11 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_task.go#L134 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_context.go#L1650
- **Maturity**: GA
- **Semantics**: Default when no valid policy is given. When a pending hook task finds an existing live object with
  the same name, it is deleted first; the wave then returns *pending* (operation Running, "waiting for deletion of
  hook <g>/<k>/<n>") and the hook is created in a later reconcile once the old object is gone.
- **Edge cases**: NotFound on delete (race) → treated as already deleted and creation proceeds. Delete errors →
  Error "failed to delete resource: <err>". With generateName hooks the name is unique per operation, so nothing is
  deleted.
- **Configuration surface**: annotation value `BeforeHookCreation` / Helm `before-hook-creation`.
- **Exposure**: annotations.
- **Tests required**:
  - Positive: `TestBeforeHookCreation`, e2e `TestHookBeforeHookCreation`; Negative: `TestHookBeforeHookCreationFailure`; Edge: `TestSync_HookWithReplaceAndBeforeHookCreation_AlreadyDeleted`.

### SYNC-057 Hook naming with generateName
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_context.go#L983 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_context_test.go#L1351
- **Maturity**: GA
- **Semantics**: A hook without `metadata.name` gets name =
  `generateName + lower("<rev>-<phase>-<operation startedAt unix seconds>")` where `<rev>` is the first 7 characters of
  the sync revision when the revision has ≥ 8 characters, otherwise the whole revision. Names are deterministic for
  an operation, so a resumed operation (controller restart) finds the same objects. Named hooks keep their name and
  are applied (not created), i.e. created only once unless BeforeHookCreation recreates them. Non-hook resources are
  never named.
- **Edge cases**: local-manifest syncs have an empty revision (name contains "-<phase>-"). Names may exceed k8s
  limits if generateName is long (API error surfaces as SyncFailed).
- **Configuration surface**: `metadata.generateName` on hook manifests.
- **Exposure**: manifests.
- **Tests required**:
  - Positive: `TestUnnamedHooksGetUniqueNames` parity ("foobarb-presync-", "foobar-postsync-"); e2e `TestAutomaticallyNamingUnnamedHook`, `TestNamingNonHookResource`.

### SYNC-058 Hook finalizer `argocd.argoproj.io/hook-finalizer`
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/hook/hook.go#L13 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_context.go#L998 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_context.go#L849
- **Maturity**: GA
- **Semantics**: Every hook created by a sync gets the finalizer so that external deletion (TTL controllers, users)
  cannot remove it before Argo CD records its result. The finalizer is removed once the hook task completes, when
  re-running a pending hook that still exists from a previous sync, and on termination. Removal uses update with
  conflict retries (re-GET on conflict); NotFound is a no-op.
- **Edge cases**: failure → Error "Failed to remove hook finalizer: <err>" (or "failed to remove hook finalizer" in runTasks, which fails the wave).
- **Configuration surface**: finalizer name.
- **Exposure**: Kubernetes objects.
- **Tests required**:
  - Positive: e2e `TestHookFinalizerPreSync/Sync/PostSync`, `TestSync_ExistingHooksWithFinalizer` parity.

### SYNC-059 Hook execution status (health → operation phase)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_context.go#L349 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_context.go#L540 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_context.go#L573
- **Maturity**: GA
- **Semantics**: A hook's `hookPhase` is derived from its live health (with Lua overrides): Unknown/Degraded → Failed,
  Progressing/Suspended → Running, Healthy → Succeeded; no health check → Succeeded with message "<name> created".
  The operation waits while any hook is Running ("waiting for completion of hook <g>/<k>/<n>[ and N more hooks]").
  Non-hook resources in multi-step syncs: Healthy → Succeeded, Degraded → Failed, other statuses keep waiting
  ("waiting for healthy state of ..."); no health check → Succeeded.
- **Edge cases**: a running task whose live object disappeared → SyncFailed/Error "Resource <g>/<k>/<n> is missing, it might have been deleted";
  health evaluation error → Error "failed to get resource health: <err>". Suspended non-hook resources block multi-step syncs.
- **Configuration surface**: resource health customizations (other area).
- **Exposure**: `syncResult.resources[].hookPhase`.
- **Tests required**:
  - Positive: Job hook transitions Running → Succeeded; Pod hook failure → Failed (`TestPostSyncHookPodFailure`).
  - Edge: `TestSync_MultistepResourceDeletionMidstep` parity; `TestInvalidlHookWaitsForOtherHooksToComplete`.

### SYNC-060 Helm hook type mapping
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/hook/helm/type.go#L12 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/hook/helm/hook.go#L5 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/helm.md#L501
- **Maturity**: GA
- **Semantics**: `helm.sh/hook` values: `pre-install`, `pre-upgrade` → PreSync; `post-install`, `post-upgrade` → PostSync;
  `pre-delete` → PreDelete; `post-delete` → PostDelete (Argo CD controller); `crd-install` → not a hook. Any other
  Helm hook value (`pre-rollback`, `post-rollback`, `test`, `test-success`, `test-failure`, ...) makes the object an
  untyped hook that is dropped. Ignored entirely when the object also has a valid `argocd.argoproj.io/hook`.
- **Edge cases**: `pre-install,pre-upgrade` on one object → single PreSync task (deduped by phase).
- **Configuration surface**: annotation `helm.sh/hook`.
- **Exposure**: annotations.
- **Tests required**:
  - Positive: `TestTypes` / `TestType_HookType` parity; chart with pre-upgrade job runs it as PreSync.
  - Negative: `helm.sh/hook: test` pod is never created.

### SYNC-061 Helm hook-weight and hook-delete-policy mapping
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/hook/helm/weight.go#L10 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/hook/helm/delete_policy.go#L10 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/syncwaves/waves.go#L12
- **Maturity**: GA
- **Semantics**: Wave = integer `argocd.argoproj.io/sync-wave` if present and parseable; otherwise integer
  `helm.sh/hook-weight` (applies to any object, hook or not); otherwise 0. `helm.sh/hook-delete-policy` values
  `before-hook-creation`, `hook-succeeded`, `hook-failed` map to the Argo CD policies and are unioned with Argo CD
  policies.
- **Edge cases**: unparseable sync-wave falls back to hook-weight; unparseable hook-weight → 0.
  `helm.sh/hook-delete-timeout` unsupported.
- **Configuration surface**: annotations `helm.sh/hook-weight`, `helm.sh/hook-delete-policy`.
- **Exposure**: annotations.
- **Tests required**:
  - Positive: hook-weight "-1" orders a hook before weight "0"; `sync-wave: "abc"` + hook-weight "3" → wave 3.

### SYNC-062 Sync waves (`argocd.argoproj.io/sync-wave`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/syncwaves/waves.go#L12 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_context.go#L659 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L953 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/sync-waves.md#L100
- **Maturity**: GA
- **Semantics**: Integer (may be negative), default 0. Within a phase, the lowest pending wave runs first; each
  reconcile step applies exactly one (phase, wave) group. The wave of each resource is exposed in
  `status.resources[].syncWave` (from target, or from the hook manifest for live hooks).
- **Edge cases**: `TestCompareAppStateSyncHookSyncWave` parity for hook waves.
- **Configuration surface**: annotation `argocd.argoproj.io/sync-wave` (string integer).
- **Exposure**: annotations; status `syncWave`.
- **Tests required**:
  - Positive: resources in waves -1, 0, 1 applied in that order with health gating; `TestSyncSuccessfully_Multistep` parity.

### SYNC-063 Wave progression and health gating
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_tasks.go#L274 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_context.go#L573 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_context.go#L703
- **Maturity**: GA
- **Semantics**: A sync is multi-step if tasks span more than one (phase, wave). Then the controller waits for every
  task of the current step to be Succeeded (Healthy) before the next step; a Degraded resource fails the operation
  ("one or more synchronization tasks completed unsuccessfully"). Single-step syncs without hooks succeed right after
  apply ("successfully synced (all tasks run)") even if resources later degrade. When no pending tasks remain:
  "successfully synced (no more tasks)".
- **Edge cases**: pruned resources still terminating block the next step ("waiting for deletion of ..."). A step whose
  wave hook (delay) errors ends in Error "SyncWaveHook failed: <err>" after preemptively terminating running hooks.
- **Configuration surface**: none.
- **Exposure**: operation messages.
- **Tests required**:
  - Positive: e2e `TestDegradedDeploymentIsSucceededAndSynced`, `TestOneProgressingDeploymentIsSucceededAndSynced`, `TestFixingDegradedApp` parity.
  - Positive: `TestWaitForCleanUpBeforeNextWave`, `TestSync_SyncWaveHookError`.

### SYNC-064 Wave delay (`ARGOCD_SYNC_WAVE_DELAY`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sync.go#L44 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sync.go#L610 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/application-controller/argocd-application-controller-statefulset.yaml#L187 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/sync-waves.md#L111
- **Maturity**: GA
- **Semantics**: After a (phase, wave) step is applied without failure and it is not the final step, the controller
  sleeps `ARGOCD_SYNC_WAVE_DELAY` seconds (default 2) synchronously, giving other controllers time to react before
  health is assessed.
- **Edge cases**: non-integer value → default 2; 0 or negative → no delay. Not applied after the final step or after a failed step.
- **Configuration surface**: env `ARGOCD_SYNC_WAVE_DELAY` (wired from `argocd-cmd-params-cm` `controller.sync.wave.delay.seconds`).
- **Exposure**: controller config.
- **Tests required**:
  - Positive: `TestSync_SyncWaveHook` parity (hook invoked per wave with final flag); measured delay ≈ configured value between waves.

### SYNC-065 Task ordering (phase, wave, kind, name)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_tasks.go#L26 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_tasks.go#L85 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/sync-waves.md#L121
- **Maturity**: GA
- **Semantics**: Sort key: phase order, then wave, then kind order, then name. Kind order (first → last): Namespace,
  NetworkPolicy, ResourceQuota, LimitRange, PodSecurityPolicy, PodDisruptionBudget, ServiceAccount, Secret, SecretList,
  ConfigMap, StorageClass, PersistentVolume, PersistentVolumeClaim, CustomResourceDefinition, ClusterRole,
  ClusterRoleList, ClusterRoleBinding, ClusterRoleBindingList, Role, RoleList, RoleBinding, RoleBindingList, Service,
  DaemonSet, Pod, ReplicationController, ReplicaSet, Deployment, HorizontalPodAutoscaler, StatefulSet, Job, CronJob,
  IngressClass, Ingress, APIService; all other kinds (custom resources) after these. Result `order` reflects first
  recording order.
- **Edge cases**: kind matching ignores group (e.g. any `Deployment` kind uses the Deployment slot).
- **Configuration surface**: none.
- **Exposure**: `syncResult.resources` order.
- **Tests required**:
  - Positive: `TestSyncCreateInSortedOrder` and sync_tasks_test ordering parity.

### SYNC-066 Namespace-first and CRD-first dependency adjustment
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_tasks.go#L112 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_tasks.go#L142
- **Maturity**: GA
- **Semantics**: After sorting, a Namespace task (core group) is moved immediately before the first task whose object
  lives in that namespace, and a CRD task is moved before the first CR of its `spec.group/spec.names.kind`. The moved
  dependency inherits the phase and wave of that first dependent (so a Namespace annotated wave 5 used by a wave-0
  Deployment is applied in wave 0).
- **Edge cases**: only target objects participate (prune tasks ignored).
- **Configuration surface**: none.
- **Exposure**: ordering only.
- **Tests required**:
  - Positive: Namespace in wave 5 + Deployment in wave 0 in that namespace → Namespace applied in wave 0 first.
  - Positive: CRD in wave 2 and CR in wave 0 → CRD applied in wave 0 before the CR.

### SYNC-067 Execution within a step (prune → hook deletion → apply by kind group)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_context.go#L1584 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_context.go#L1703
- **Maturity**: GA
- **Semantics**: For one step: (1) remove leftover hook finalizers of pending existing hooks; (2) prune tasks
  concurrently (after the confirmation gate); (3) BeforeHookCreation deletions (step returns pending if anything was
  deleted); (4) apply/create tasks in consecutive groups of the same kind — groups run sequentially, tasks within a
  group concurrently. Step state is the worst of successful < pending < failed.
- **Edge cases**: a failure in any group still lets later groups run in the same step (state merges), but the step is failed.
- **Configuration surface**: none.
- **Exposure**: internal behaviour.
- **Tests required**:
  - Positive: prune of old ConfigMap happens before apply of new ConfigMap in the same wave.

### SYNC-068 Pruning order (reverse waves, deletion wait)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_context.go#L1097 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_context.go#L599 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/sync-waves.md#L37
- **Maturity**: GA
- **Semantics**: The distinct waves of prune tasks are mirrored (symmetric swap over the sorted set of prune waves), so
  higher-wave resources are pruned first; non-prune tasks keep their waves. A wave containing pruned objects that are
  still terminating blocks progression. A prune failure fails the operation; lower prune waves are not processed.
- **Edge cases**: PruneLast overrides the mirrored wave. Prune waves interleave with apply waves by number.
- **Configuration surface**: none.
- **Exposure**: ordering.
- **Tests required**:
  - Positive: `TestWaveReorderingOfPruneTasks` table and e2e `TestSyncPruneOrderWithSyncWaves` parity.
  - Negative: `TestSyncPruneFailure`.

### SYNC-069 Sync waves for Applications (app-of-apps)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/health/health.go#L104 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/health.go#L81 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/health.md#L29
- **Maturity**: GA
- **Semantics**: Child `argoproj.io/Application` manifests honour `sync-wave` like any resource, but there is no
  built-in health check for Applications, so a child app is considered done immediately after apply and later waves
  do not wait for it. Operators must add the documented `resource.customizations.health.argoproj.io_Application` Lua
  check to gate waves on child health. Missing/Unknown health of a child Application never degrades the parent.
- **Edge cases**: the parent app's own Application object (self-managed) is excluded from its health.
- **Configuration surface**: `argocd-cm` `resource.customizations.health.argoproj.io_Application`.
- **Exposure**: annotations + argocd-cm.
- **Tests required**:
  - Positive: without the health customization, wave-1 child app is created without waiting for wave-0 child health; with it, the sync waits.

### SYNC-070 Wave ordering during cascading app deletion
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sort_delete.go#L24 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1438
- **Maturity**: GA
- **Semantics**: Cascading deletion deletes only the objects of the highest remaining sync wave per reconcile, and waits
  while any object is still terminating, so waves are deleted in reverse order.
- **Edge cases**: objects excluded by Delete=false / keep policy / CRD rule do not block.
- **Configuration surface**: sync-wave annotations.
- **Exposure**: Application deletion (other area for the full flow).
- **Tests required**:
  - Positive: app with waves 0 and 1 deletes wave-1 objects first.

---

## F. Engine mechanics and operation lifecycle

### SYNC-071 Pre-sync dry-run validation
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_context.go#L503 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_context.go#L1389
- **Maturity**: GA
- **Semantics**: On the first step of an operation (no results recorded yet), every task (all phases/waves, minus the
  namespace-creation task, minus unmodified resources under ApplyOutOfSyncOnly, minus skip-dry-run tasks) is applied
  with kubectl client dry-run. Any failure → operation `Failed` "one or more objects failed to apply (dry run)" with the
  failing resource results recorded; nothing is changed in the cluster (except the auto-created namespace).
- **Edge cases**: the dry-run uses client strategy even with SSA. Successful dry-run results are not recorded.
- **Configuration surface**: SkipDryRunOnMissingResource, Validate.
- **Exposure**: operation results.
- **Tests required**:
  - Negative: invalid field in a wave-2 resource fails the whole operation before wave 0 is applied.

### SYNC-072 Missing kinds, CRD handling and CRD readiness
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_context.go#L1039 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_context.go#L1261 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L43
- **Maturity**: GA
- **Semantics**: Task validation looks up each GVK via discovery (retrying Unauthorized, caching per GVK). NotFound
  without a skip rule → task result `SyncFailed` with the error and operation `Failed` "one or more synchronization tasks
  are not valid: <sorted distinct messages joined by '; '>". After applying a CRD (not dry-run) the controller polls
  up to 3s for the `Established` condition (errors only logged). Result messages containing "the server could not find
  the requested resource" are rewritten to "The Kubernetes API could not find <g>/<k> for requested resource <ns>/<n>.
  Make sure the \"<k>\" CRD is installed on the destination cluster." or the version-mismatch variant.
- **Edge cases**: messages containing the 262144-byte annotation limit get an appended hint to use SSA or Replace.
- **Configuration surface**: none.
- **Exposure**: operation message and results.
- **Tests required**:
  - Negative: CR of unknown kind → messages above (`TestSync_getSyncTasks_FailureMessage`, `Test_getSyncTasks_ErrorCaching`, `TestServerResourcesRetry` parity).
  - Positive: `TestSyncCustomResources` parity.

### SYNC-073 Project permission validation during sync
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sync.go#L647 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_context.go#L1090
- **Maturity**: GA
- **Semantics**: Every task (including hooks and prune tasks) is checked against the AppProject: group/kind/name
  allow/deny lists (cluster- vs namespace-scoped per discovery) and, for namespaced resources, the destination
  (cluster + namespace). Violations make the task `SyncFailed` and the operation `Failed` before anything is applied.
  Messages: "resource <group>:<kind> is not permitted in project <p>", "namespace <ns> is not permitted in project '<p>'",
  "failed to get API resource info for <g>/<k>: unable to verify permissions".
- **Edge cases**: validation uses the API resource scope, not the manifest namespace.
- **Configuration surface**: AppProject whitelists/blacklists/destinations (other area).
- **Exposure**: operation results.
- **Tests required**:
  - Authorization: `TestSyncNotPermittedNamespace`, `TestValidateSyncPermissions`, e2e `TestPermissionDeniedWithNegatedNamespace` parity.

### SYNC-074 Apply mechanics (client-side apply, RBAC reconcile, target namespace defaulting)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/utils/kube/resource_ops.go#L390 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/utils/kube/resource_ops.go#L261 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_context.go#L1012
- **Maturity**: GA
- **Semantics**: Default apply = in-process `kubectl apply` (client-side, overwrite, last-applied annotation). For
  `rbac.authorization.k8s.io/v1` objects without SSA, `kubectl auth reconcile` runs first (handles immutable roleRef;
  for namespaced RBAC, the namespace must exist when not dry-run: "error getting namespace <ns>"). Target objects with
  empty namespace get the destination namespace before apply (also cluster-scoped ones, harmless). kubectl errors are
  reported as the task message.
- **Edge cases**: auth reconcile errors → "error running kubectl auth reconcile: <err>". ClusterRole/ClusterRoleBinding skip the namespace check.
- **Configuration surface**: none.
- **Exposure**: behaviour.
- **Tests required**:
  - Positive: RoleBinding with changed roleRef syncs without force.
  - Positive: `TestObjectsGetANamespace` parity.

### SYNC-075 Per-resource sync results
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1773 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/common/types.go#L106 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_context.go#L1735 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sync.go#L365
- **Maturity**: GA
- **Semantics**: `status.operationState.syncResult.resources[]` has one row per (resource key, sync phase):
  group, version, kind, namespace, name, `status` (Synced | SyncFailed | Pruned | PruneSkipped; empty while pending),
  `message` (kubectl output such as "deployment.apps/x configured", latest non-empty message kept), `hookType`,
  `hookPhase` (Running | Succeeded | Failed | Error), `syncPhase`, `images` (best-effort from workload
  specs). Status→phase defaults: Synced→Running, SyncFailed→Failed, Pruned/PruneSkipped→Succeeded. Rows are ordered by
  first insertion. The list is rebuilt from engine state each step and re-seeded from status on resume.
- **Edge cases**: messages are augmented (SYNC-072). The syncResult also stores `revision(s)`, `source(s)` and
  `managedNamespaceMetadata`.
- **Configuration surface**: status fields.
- **Exposure**: CRD status; CLI `app sync/get` resource table; UI "Sync Status" panel.
- **Tests required**:
  - Positive: e2e `ResourceResultMatches` style assertions for created/pruned/hook rows.

### SYNC-076 Operation phases and messages
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/common/types.go#L76 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_context.go#L425 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sync.go#L102
- **Maturity**: GA
- **Semantics**: Phases: Running, Terminating, Failed, Error, Succeeded (Failed/Error/Succeeded are terminal). Running
  messages: "waiting for completion of hook ...", "waiting for healthy state of ...", "waiting for deletion of [hook] ...",
  "waiting for pruning confirmation of ...", each with " and N more resources|hooks", plus "one or more tasks are
  running". Controller-level Error messages include: "Failed to generate sync ID", "Invalid operation request: no
  operation specified", formatted ComparisonError/InvalidSpecError conditions ("<type>: <msg>;..."), "Failed to get
  destination cluster: ...", "Failed to load resource overrides: ...", "failed to initialize sync context: ...",
  "failed to record sync to history: ...", "Failed to load application project: ...", recovered panics.
- **Edge cases**: comparison errors (e.g. repo unreachable) abort the operation with Error before any apply.
- **Configuration surface**: none.
- **Exposure**: `status.operationState.{phase,message}`; CLI/UI.
- **Tests required**:
  - Negative: `TestSyncComparisonError` parity (Error with condition text).
  - Positive: `Test_setRunningPhase` message table parity.

### SYNC-077 Operation state persistence, resumption and completion side effects
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1596 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1752 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sync.go#L202 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1451
- **Maturity**: GA
- **Semantics**: New operation → `operationState{operation, phase: Running, startedAt: now}`. Each step replaces the
  whole `status.operationState` via JSON patch (retrying until success; NotFound evicts the app). On terminal phase:
  `finishedAt` is set, `/operation` is set to null, a k8s event `OperationCompleted` ("[Partial ]Sync operation to <rev>
  succeeded" / "... failed: <msg>") is emitted, sync count and duration metrics are recorded, and (non-dry-run) a
  refresh is requested (manual: force revision resolution; automated: latest). An in-progress operation
  (`operationState` non-terminal) is resumed after controller restart using the recorded results. Operation
  processing re-queues the app while Running; the operation queue is fed after every refresh.
- **Edge cases**: panics during processing → phase Error with the panic message. A Running result never overwrites a
  concurrent Terminating set by the API (fresh GET check). Unchanged state skips the patch.
- **Configuration surface**: `status.operationState.{operation, phase, message, syncResult, startedAt, finishedAt, retryCount}`.
- **Exposure**: CRD status; UI operation panel; CLI `app get`.
- **Tests required**:
  - Positive: `TestProcessRequestedAppOperation_Successful`, `TestProcessRequestedAppAutomatedOperation_Successful`, `TestSetOperationStateOnDeletedApp`, `TestSetOperationStateLogRetries` parity.
  - Recovery: kill controller during a multi-wave sync; on restart it continues at the next wave without re-running applied hooks.

### SYNC-078 Terminate operation
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2508 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.proto#L483 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_context.go#L725 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/sync_context.go#L791 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/app.go#L2958 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-operation-state/application-operation-state.tsx#L101
- **Maturity**: GA
- **Semantics**: `DELETE /api/v1/applications/{name}/operation` (RBAC `applications, sync`) sets
  `status.operationState.phase = Terminating` with up to 10 conflict retries (100ms apart), waits until the informer
  has observed the new resourceVersion, and logs
  "terminated running operation". The controller, seeing Terminating, calls Terminate: removes finalizers from
  completed hooks; for each running hook re-reads it, removes the finalizer, deletes it if still running / status
  unknown / HookFailed policy, and records its real terminal status or "Terminated"; phase `Failed` "Operation
  terminated" or `Error` "Operation termination had errors". Applied non-hook resources are not rolled back; SyncFail
  hooks do not run; no retry.
- **Edge cases**: no operation in progress → `InvalidArgument` "Unable to terminate operation. No operation is in progress";
  persistent conflicts → `Internal` "Failed to terminate app. Too many conflicts".
- **Configuration surface**: none.
- **Exposure**: API `TerminateOperation`; CLI `argocd app terminate-op APPNAME [-N ns]` (prints "Application '<n>' operation terminating"); UI "Terminate" button in operation panel.
- **Tests required**:
  - Positive: `TestTerminate*` (engine), `TestSyncAndTerminate`, `TestTerminateOperationWithConflicts`, e2e `TestTerminateWithRunningHooks` parity.
  - Authorization: user without `sync` cannot terminate.

### SYNC-079 Sync timeout (`controller.sync.timeout.seconds`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1626 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1670 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-application-controller/commands/argocd_application_controller.go#L280 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller_test.go#L3372
- **Maturity**: GA
- **Semantics**: If non-zero, a Running operation whose `startedAt` is older than the timeout is set to Terminating
  ("operation is terminating due to timeout") and terminated; final message "Operation terminated, triggered by
  controller sync timeout[ (retried N times).]". A check is scheduled `timeout` after each new operation starts.
  Timeout counts from the original start, including retries. 0 = disabled (default).
- **Edge cases**: an already Terminating operation past the timeout ends with plain "Operation terminated".
- **Configuration surface**: `argocd-cmd-params-cm` `controller.sync.timeout.seconds`; env `ARGOCD_APPLICATION_CONTROLLER_SYNC_TIMEOUT`; flag `--sync-timeout`.
- **Exposure**: controller config.
- **Tests required**:
  - Positive: `TestProcessRequestedAppOperation_SyncTimeout` table parity (4 cases).

### SYNC-080 Self-referenced object filter
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sync.go#L313 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L1288
- **Maturity**: GA
- **Semantics**: With annotation-based tracking, live objects whose tracking id (app:group/kind:namespace/name) does not
  match the object itself (e.g. copied by another controller) are excluded from sync tasks (never pruned/applied) and
  from sync status. With label tracking every object is assumed self-referenced.
- **Edge cases**: cluster-scoped objects match regardless of the namespace in the tracking id.
- **Configuration surface**: tracking method (other area).
- **Exposure**: behaviour.
- **Tests required**:
  - Negative: a Secret copied with the tracking annotation of another Secret is not pruned by the app.

### SYNC-081 Resource status fields used by sync
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2231 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L943
- **Maturity**: GA
- **Semantics**: `status.resources[]` carries `hook` (true for hooks incl. PreDelete/PostDelete annotated live objects),
  `requiresPruning` (live without target and self-referenced), `syncWave`, `requiresDeletionConfirmation`
  (SYNC-033). Hooks, ignored/skip resources and non-self-referenced objects have no sync status and do not affect
  the app sync status.
- **Edge cases**: these drive auto-sync prune decisions, UI prune warnings and CLI pruning checks.
- **Configuration surface**: status.
- **Exposure**: CRD; UI resource tree; CLI.
- **Tests required**:
  - Positive: extra resource shows `requiresPruning: true`; Prune=confirm resource shows `requiresDeletionConfirmation: true`.

### SYNC-082 Shared resource detection (SharedResourceWarning)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L804 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1910
- **Maturity**: GA
- **Semantics**: During comparison, any live object in the app's target set whose tracking points to a different app
  instance yields condition `SharedResourceWarning` "<Kind>/<name> is part of applications <this> and <other>". A
  normal sync then takes ownership (re-labels) the object; FailOnSharedResource (SYNC-046) blocks it instead.
- **Edge cases**: instance names `ns_name` are displayed as `ns/name`.
- **Configuration surface**: none.
- **Exposure**: status conditions; UI.
- **Tests required**:
  - Positive: two apps owning the same object → warning on the second app.

---

## G. Sync windows

### SYNC-083 Sync window definition and validation
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2925 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L3433 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L3463 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/app_project_types.go#L268 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/crds/appproject-crd.yaml#L336
- **Maturity**: GA
- **Semantics**: `AppProject.spec.syncWindows[]`: `kind` (allow|deny), `schedule` (5-field cron: minute hour dom month
  dow), `duration` (Go duration), `applications[]`, `namespaces[]`, `clusters[]`, `manualSync`, `timeZone` (IANA,
  default UTC), `andOperator` (Go `UseAndOperator`), `description` (≤255 chars), `syncOverrun`. Project validation
  rejects: bad kind "kind '<k>' mismatch: can only be allow or deny"; "cannot parse schedule '<s>': ..."; "cannot parse
  duration '<d>': ..."; "description must not exceed 255 characters"; no selectors → OutOfRange "window '<k>':'<s>':'<d>'
  requires one of application, cluster or namespace"; duplicates (same kind, schedule, duration, selectors, timeZone,
  andOperator; manualSync/description/syncOverrun ignored) → AlreadyExists "sync window with kind ... already exists".
  Invalid timeZone is replaced by UTC with a warning.
- **Edge cases**: windows active-ness is computed by finding the next cron fire after (now + tzOffset − duration) and
  checking it is before (now + tzOffset); the tz offset is taken at *current* time (DST shifts apply uniformly).
- **Configuration surface**: AppProject CRD `spec.syncWindows`.
- **Exposure**: CRD; API (Project Create/Update); CLI `argocd proj windows add|update|delete|list|enable-manual-sync|disable-manual-sync|enable-sync-overrun|disable-sync-overrun`; UI project "Sync Windows" editor.
- **Tests required**:
  - Positive/negative: `TestSyncWindow_Validate`, `TestAppProject_ValidateSyncWindowList`, `TestAppProjectSpec_AddWindow`, `TestSyncWindow_Active`, `TestSyncWindows_Active` parity (incl. time zones).

### SYNC-084 Sync window matching (applications, clusters, namespaces, AND operator)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L3101 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/app_project_types.go#L446 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/sync_windows.md#L9
- **Maturity**: GA
- **Semantics**: Glob patterns (`*` matches all) against: app `metadata.name` (not namespace-qualified); cluster =
  `spec.destination.name` OR `spec.destination.server`; namespace = `spec.destination.namespace`. Default (OR): a
  window applies if any configured selector matches. With `andOperator: true` every configured (non-empty) selector
  list must match. A window with no selectors never matches.
- **Edge cases**: apps addressed by destination name match on name, by server on URL — not both resolved.
- **Configuration surface**: `syncWindows[].{applications,clusters,namespaces,andOperator}`.
- **Exposure**: as SYNC-083.
- **Tests required**:
  - Positive: `TestSyncWindows_Matches`, `TestSyncWindows_Matches_AND_Operator` parity.

### SYNC-085 Sync window evaluation (allow/deny precedence, manualSync)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L3181 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L3246 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/sync_windows.md#L29
- **Maturity**: GA
- **Semantics**: Over the windows matching the app: none → allowed. Any active deny → denied, unless the sync is
  manual and *all* active deny windows have `manualSync: true`. Else any active allow → allowed. Else if allow windows
  exist (all inactive) → denied, unless manual and *all* inactive allow windows have manualSync. Else allowed. Parse
  errors → "invalid sync windows: ..." treated as denied.
- **Edge cases**: deny overrides allow. Overrun rules (SYNC-086) only apply to in-progress operations.
- **Configuration surface**: `syncWindows[].{kind, manualSync}`.
- **Exposure**: as SYNC-083; app `GET /api/v1/applications/{name}/syncwindows` returns `activeWindows`, `assignedWindows`, `canSync` (evaluated as manual).
- **Tests required**:
  - Positive: `TestSyncWindows_CanSync`, `TestSyncWindows_hasDeny`, `TestSyncWindows_hasAllow`, `TestServer_GetApplicationSyncWindowsState` parity.

### SYNC-086 Sync overrun
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2946 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L3296 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L3332 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/sync_windows.md#L42
- **Maturity**: GA
- **Semantics**: For an in-progress operation (start time known): if an active deny blocks it, it may continue when
  all *active* deny windows have `syncOverrun` and the sync would have been allowed at the operation's `startedAt`.
  If allow windows exist but none is active, it may continue when all *inactive* allow windows have `syncOverrun`
  and the sync was allowed at `startedAt`.
- **Edge cases**: new syncs are never allowed by overrun. The "allowed at start" check reuses manualSync semantics.
- **Configuration surface**: `syncWindows[].syncOverrun`; CLI `--sync-overrun`, `enable-sync-overrun`, `disable-sync-overrun`; UI "Enable sync overrun".
- **Exposure**: CRD/CLI/UI.
- **Tests required**:
  - Positive: `TestSyncWindows_SyncOverrun` table and e2e `TestSyncWindowSyncOverrun` parity (all 9 documented transitions).

### SYNC-087 Sync window enforcement points
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2092 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2042 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sync.go#L123 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sync.go#L624
- **Maturity**: GA
- **Semantics**: (1) API Sync: manual evaluation, no start time → PermissionDenied when blocked. (2) Auto-sync
  initiation: automated evaluation, no start time → no operation. (3) Every controller step of any operation:
  isManual = not `initiatedBy.automated`, start time = `operationState.startedAt` (enables overrun); if blocked, the
  step does nothing; a Running operation's message becomes "Sync operation blocked by sync window[: <err>]" and the
  operation stays Running (paused) until allowed (or sync timeout terminates it).
- **Edge cases**: operations created via kubectl bypass (1) but not (3). Rollback API does not check windows itself (only (3)).
- **Configuration surface**: none.
- **Exposure**: API, controller.
- **Tests required**:
  - Positive: `TestSyncWindowDeniesSync` parity (message).
  - Positive: operation started before a deny window without overrun pauses, resumes when window closes.

### SYNC-088 Sync windows CLI / API / UI
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/projectwindows.go#L22 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2880 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/project/project.go#L526 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/settings/components/project-sync-windows-edit-panel/project-sync-windows-edit-panel.tsx#L57 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/sync_windows.md#L108
- **Maturity**: GA
- **Semantics**: CLI `argocd proj windows` subcommands: `add PROJECT --kind --schedule --duration [--applications]
  [--namespaces] [--clusters] [--manual-sync] [--time-zone=UTC] [--use-and-operator] [--description] [--sync-overrun]`;
  `update PROJECT ID [--schedule --duration --applications --namespaces --clusters --time-zone --description]` (lists
  replace whole lists; time zone defaults to UTC); `delete PROJECT ID`; `enable-manual-sync|disable-manual-sync PROJECT ID`;
  `enable-sync-overrun|disable-sync-overrun PROJECT ID`; `list PROJECT [-o json|yaml|wide]` (columns ID, STATUS,
  KIND, SCHEDULE, DURATION, APPLICATIONS, NAMESPACES, CLUSTERS, MANUALSYNC, SYNCOVERRUN, TIMEZONE, USEANDOPERATOR).
  All mutate via Project Update. APIs: `GET /api/v1/applications/{name}/syncwindows` (RBAC app get) and
  `GET /api/v1/projects/{name}/syncwindows` (RBAC projects get; returns active windows). UI: project Sync Windows
  editor (kind, schedule, time zone, duration, manualSync, syncOverrun, AND operator, description, selectors) and an
  app status-panel sync-window indicator (red denied / orange manual allowed / green allowed).
- **Edge cases**: `add` errors "cannot create window: require kind, schedule, duration and one or more of applications,
  namespaces and clusters"; `update` with nothing → "cannot update: require one or more of schedule, duration,
  application, namespace, cluster or description"; unknown ID → "window with id '<id>' not found".
- **Configuration surface**: AppProject.
- **Exposure**: API/CLI/UI.
- **Tests required**:
  - Positive: `TestSyncWindow_Update`, `TestAppProjectSpec_DeleteWindow`; CLI add/list round-trip.

---

## H. History and rollback

### SYNC-089 Revision history recording
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sync.go#L408 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L1194 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1833
- **Maturity**: GA
- **Semantics**: After a successful, non-dry-run, non-partial operation, an entry is appended to `status.history`:
  `id` (last id + 1, starting at 0), `deployedAt` (now, UTC), `deployStartedAt` (operation startedAt), `initiatedBy`,
  and for single-source `revision` + `source`, for multi-source `revisions` + `sources` (the compared source(s)), then
  the list is truncated to the limit and merge-patched. Failure → operation Error "failed to record sync to history: <err>".
- **Edge cases**: self-heal and selective syncs never add entries; ApplyOutOfSyncOnly syncs do. Rollbacks add entries.
- **Configuration surface**: `status.history[]`.
- **Exposure**: CRD; CLI `argocd app history`; UI "History and rollback".
- **Tests required**:
  - Positive: `TestPersistRevisionHistory`, `TestPersistRevisionHistoryRollback` parity.

### SYNC-090 revisionHistoryLimit
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L96 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1385 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/application_defaults.go#L28
- **Maturity**: GA
- **Semantics**: Keeps the newest N entries (default 10). `0` (or negative) keeps none.
- **Edge cases**: ids keep increasing after truncation.
- **Configuration surface**: `spec.revisionHistoryLimit`; CLI `--revision-history-limit` (app create/set, default 10).
- **Exposure**: CRD; CLI.
- **Tests required**:
  - Positive: e2e `TestRevisionHistoryLimit` parity.

### SYNC-091 Rollback
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2265 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.proto#L161 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/app.go#L2690 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-details/application-details.tsx#L276 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/auto_sync.md#L146
- **Maturity**: GA
- **Semantics**: `POST /api/v1/applications/{name}/rollback` {id, dryRun, prune, appNamespace, project}; RBAC
  `applications, sync`. Refused if deleting ("application is deleting"), if auto-sync is enabled (FailedPrecondition
  "rollback cannot be initiated when auto-sync is enabled"), if the id is unknown (InvalidArgument "application <ns/name>
  does not have deployment with id <id>"), or if the entry has neither source nor sources (FailedPrecondition "cannot
  rollback to revision deployed with Argo CD v0.11 or lower. sync to revision instead."). Creates a sync operation with
  the entry's revision(s) and source(s), strategy `apply` (hooks skipped), spec syncOptions, request prune/dryRun; no
  retry. Event "initiated rollback to <id>". Multi-source entries roll back all sources. CLI `argocd app rollback APPNAME
  [ID] [--prune] [--timeout] [-o] [-N]`: without ID uses the second-to-last history entry ("application '<n>' should
  have at least two successful deployments"), waits for completion. UI history panel "Rollback"/"Redeploy"; if
  auto-sync is on, the UI offers to disable it (sets `automated.enabled=false`) first.
- **Edge cases**: after rollback the app is OutOfSync with the spec (expected). Sync windows are enforced by the
  controller only. CLI has no `--dry-run` flag although the API supports it.
- **Configuration surface**: none.
- **Exposure**: API `Rollback`; CLI; UI.
- **Tests required**:
  - Positive: e2e `TestAppRollbackSuccessful`; multi-source rollback restores both revisions.
  - Negative: auto-sync enabled → FailedPrecondition; unknown id → InvalidArgument.
  - Authorization: user without `sync` denied.

### SYNC-092 History CLI
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/app.go#L2654 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/commands/argocd_app_history.md
- **Maturity**: GA
- **Semantics**: `argocd app history APPNAME [-N ns] [-o wide|id]` prints the history table (wide) or just ids.
- **Edge cases**: read via Get (RBAC get).
- **Configuration surface**: none.
- **Exposure**: CLI.
- **Tests required**:
  - Positive: ids printed match `status.history[].id`.

---

## I. Impersonation

### SYNC-093 Service account impersonation for sync (destinationServiceAccounts)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sync.go#L260 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/impersonation.go#L19 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L3565 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2923 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1401 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/app-sync-using-impersonation.md#L56
- **Maturity**: Beta
- **Semantics**: With `argocd-cm` `application.sync.impersonation.enabled: "true"`, sync (apply, prune, hooks) runs with
  Kubernetes impersonation of a service account derived from the project's `spec.destinationServiceAccounts[]`
  `{server, namespace, defaultServiceAccount}`: the first entry whose `server` glob matches the destination cluster
  URL and `namespace` glob matches `spec.destination.namespace` wins. `defaultServiceAccount` `ns:name` →
  `system:serviceaccount:ns:name`; plain `name` → namespace = destination namespace, or the Application's namespace if
  empty. The same impersonation applies to app deletion (resources and delete hooks) and to API-server live-resource
  operations (resource actions, patch/delete resource).
- **Edge cases**: invalid glob → error "invalid glob pattern for destination server|namespace"; empty name or chars
  `!*[]{}\/` → "default service account contains invalid chars '<v>'"; sync failures surface as operation Error
  "failed to derive service account to impersonate: ...". Project validation rejects `!` in server/namespace.
  CLI `argocd proj add-destination-service-account` / `remove-destination-service-account` (projects area).
- **Configuration surface**: `argocd-cm` `application.sync.impersonation.enabled` (default false); AppProject `spec.destinationServiceAccounts`.
- **Exposure**: CRD; CLI (proj commands); UI (project editor); controller.
- **Tests required**:
  - Positive: `TestSyncWithImpersonate`, `TestDeriveServiceAccountMatchingNamespaces/Servers`, e2e `TestSyncWithImpersonateWithSyncServiceAccount` parity.
  - Negative: e2e `TestSyncWithMissingServiceAccount`, `TestSyncWithValidSAButDisallowedDestination`; Positive `TestSyncWithFeatureDisabled`.
  - Authorization: SA lacking RBAC → apply fails with Forbidden in result message.

### SYNC-094 Impersonation enforcement (`application.sync.impersonation.enforced`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sync.go#L273 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L2751 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/app-sync-using-impersonation.md#L80
- **Maturity**: Beta
- **Semantics**: When impersonation is enabled and no destination service account matches: enforced (default true;
  any value other than "false") → sync Error "no matching service account found for destination server <s> and
  namespace <ns>" (API server live-resource calls return the same error); not enforced → fall back to the controller's
  own identity with an info log.
- **Edge cases**: during cascading deletion the controller does not check enforcement — no match means no
  impersonation (controller identity).
- **Configuration surface**: `argocd-cm` `application.sync.impersonation.enforced`.
- **Exposure**: config.
- **Tests required**:
  - Negative: e2e `TestSyncWithNoDestinationServiceAccountsInProject`; Positive: `TestSyncWithNoDestinationServiceAccountsAndEnforcementDisabled`.

---

## J. CLI and UI surfaces for sync

### SYNC-095 `argocd app sync` command
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/app.go#L1699 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/app.go#L2059 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/commands/argocd_app_sync.md
- **Maturity**: GA
- **Semantics**: `argocd app sync [APPNAME... | -l selector | --project p]` with flags `--app-namespace/-N`,
  `--apply-out-of-sync-only`, `--assumeYes`, `--async`, `--dry-run`, `--force`, `--info`, `--label`, `--local`,
  `--local-repo-root`, `--output/-o json|yaml|wide|tree|tree=detailed`, `--preview-changes`, `--project`, `--prune`,
  `--replace`, `--resource`, `--retry-backoff-duration`, `--retry-backoff-factor`, `--retry-backoff-max-duration`,
  `--retry-limit`, `--retry-refresh`, `--revision`, `--revisions`, `--selector/-l`, `--server-side`,
  `--server-side-diff-concurrency`, `--server-side-diff-max-batch-kb`, `--source-names`, `--source-positions`,
  `--strategy apply|hook`, `--timeout` (0 = no timeout), `--ignore-normalizer-jq-execution-timeout`. Selector/project
  resolve to apps (fatal "No matching apps found for filter: ..." if none). `--preview-changes` prints the diff and asks
  for confirmation (`--assumeYes` skips); no diff → "====== No Differences found ======" and no sync. Unless `--async`,
  waits for the operation and exits non-zero on "Operation has completed with phase: <p>" or "<n> resources require pruning".
- **Edge cases**: "Cannot use selector option when application name(s) passed as argument(s)". No args/selector/project → help + exit 1.
- **Configuration surface**: none.
- **Exposure**: CLI.
- **Tests required**:
  - Positive: sync by selector syncs all matching apps sequentially.
  - Negative: failed operation → exit code 1.
  - Positive: `--async` returns immediately after the API call.

### SYNC-096 Sync policy CLI flags (`app create` / `app set`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/util/app.go#L139 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/util/app.go#L222 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/util/app.go#L290
- **Maturity**: GA
- **Semantics**: `--sync-policy manual|none` removes `automated`; `automated|automatic|auto` sets `automated: {}`
  (resetting prune/selfHeal/allowEmpty unless also passed). `--auto-prune`, `--self-heal`, `--allow-empty` set the
  corresponding pointers; if `automated` was nil they create `automated{enabled:false}` (auto-sync stays disabled).
  `--sync-option X` adds, `--sync-option !X` removes (repeatable). `--sync-retry-limit N` (>0 sets retry with
  `--sync-retry-backoff-duration` 5s, `--sync-retry-backoff-max-duration` 3m0s, `--sync-retry-backoff-factor` 2,
  `--sync-retry-refresh`; 0 removes retry; negative → fatal "Invalid sync-retry-limit [N]"). `--sync-retry-refresh`
  alone sets `retry.refresh`. Empty sync policies are normalised to nil. Invalid policy → fatal "Invalid sync-policy: <v>".
- **Edge cases**: `--sync-retry-limit -1` (unlimited) cannot be set through the CLI (rejected), only via manifest.
- **Configuration surface**: `spec.syncPolicy.*`.
- **Exposure**: CLI.
- **Tests required**:
  - Positive: `app set --self-heal` on manual app yields `automated: {enabled: false, selfHeal: true}`.
  - Negative: `--sync-retry-limit -1` fatal.

### SYNC-097 Manual sync UI panel (single app and bulk)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-sync-panel/application-sync-panel.tsx#L23 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-sync-options/application-sync-options.tsx#L118 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/shared/services/applications-service.ts#L279
- **Maturity**: GA
- **Semantics**: Panel fields: Revision; flags Prune / Dry Run / Apply Only / Force; sync options (Skip Schema
  Validation→`Validate=false`, Auto-Create Namespace, Apply Out of Sync Only, Respect Ignore Differences, Server-Side
  Apply, Prune Last, Prune Propagation Policy select, Prune select true/false/confirm, Delete select, Replace when
  allowed); retry options; resource selection (non-hook resources, at least one required: "Select at least one
  resource"). Confirmation prompts: typed "prune" when pruning some/all resources, typed "replace" for Replace, confirm
  dialog for Force. Bulk sync panel (applications list) applies the same flags/options to many apps.
- **Edge cases**: all resources selected → `resources: null` (full sync, recorded in history).
- **Configuration surface**: none.
- **Exposure**: UI.
- **Tests required**:
  - Positive: UI sync with options sends `syncOptions.items` including app-level options.

### SYNC-098 `argocd app wait`
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/app.go#L1587 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/app.go#L2242 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/commands/argocd_app_wait.md
- **Maturity**: GA
- **Semantics**: Waits for apps (names or `-l`) until conditions hold: `--sync`, `--health`, `--operation` (no
  operation in progress), `--suspended`, `--degraded`, `--delete`, `--hydrated`; none given = sync+health+operation.
  `--resource` filters as in sync; `--timeout` seconds (0 = forever); `--output`.
- **Edge cases**: health flags are OR-ed (healthy OR suspended OR degraded when several are set).
- **Configuration surface**: none.
- **Exposure**: CLI.
- **Tests required**:
  - Positive: `app wait --operation` returns after a running sync completes.

### SYNC-099 `argocd app confirm-deletion`
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/app.go#L3245 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/common/types.go#L19
- **Maturity**: GA
- **Semantics**: Sets Application annotation `argocd.argoproj.io/deletion-approved` to now (RFC3339) via Update with
  validate=false; confirms both pending prunes (Prune=confirm) and pending deletions (Delete=confirm). Prints
  "Application '<name>' updated successfully".
- **Edge cases**: requires `applications, update` RBAC. Approval only counts for operations/deletions started at or before the timestamp.
- **Configuration surface**: annotation.
- **Exposure**: CLI; UI "Confirm Pruning" / "Confirm Deletion".
- **Tests required**:
  - Positive: pending Prune=confirm sync completes after the command.

---

## K. Resource actions

### SYNC-100 Resource action discovery (ListResourceActions)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2561 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/lua/lua.go#L331 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/lua/lua.go#L414 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2682 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.proto#L503
- **Maturity**: GA
- **Semantics**: `GET /api/v1/applications/{name}/resource/actions?namespace&resourceName&version&group&kind`
  (RBAC `applications, get`). The live object (or the Application itself when kind=Application, group=argoproj.io and
  resourceName == app name) is loaded; discovery scripts: custom `discovery.lua` for the exact `group/Kind` key (or
  `Kind` for core) and, if `mergeBuiltinActions` or no custom actions, the built-in script. Each script returns a table
  `name -> {disabled, iconClass, displayName, params[{name}]}` (or empty table); first script wins per action name
  (custom precedence). Response `actions[]{name, params[{name}], disabled, iconClass, displayName}`; enabled by default.
- **Edge cases**: resource must be part of the app tree with a UID ("<kind> <group> <name> not found as part of
  application <app>"). Script errors → "error executing Lua discovery script: ...". Unknown keys in params (e.g.
  `default`) are dropped from the API model.
- **Configuration surface**: `argocd-cm` actions customizations (SYNC-104).
- **Exposure**: API; CLI `app actions list`; UI resource "Actions" menu.
- **Tests required**:
  - Positive: Deployment lists restart/pause/resume/scale with correct disabled flags.
  - Positive: custom discovery with mergeBuiltinActions true/false.

### SYNC-101 Run resource action (RunResourceActionV2 / deprecated RunResourceAction)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2662 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2778 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2640 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/lua/lua.go#L207 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/lua/lua.go#L449 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.proto#L511
- **Maturity**: GA (V1 RPC Deprecated)
- **Semantics**: `POST /api/v1/applications/{name}/resource/actions/v2` body {namespace, resourceName, version, group,
  kind, action, resourceActionParameters[{name,value}], appNamespace, project}. RBAC `applications,
  action/<group>/<kind>/<action>`. Action script = custom definition with that name, else built-in
  `resource_customizations/<group>/<Kind>/actions/<name>/action.lua`. The script returns either the modified object
  (old style = patch) or an array of `{operation: "create"|"patch", resource}` (SYNC-103). For patch: JSON merge patch
  (live → returned, after fixing Lua empty-table→array conversions); `{}` → no-op; if the patch touches `status`, first
  try patching the `status` subresource (NotFound → treat as no subresource and patch the whole thing), then patch the
  rest. Audit events "ran action <a>[ on resource <g>/<k>/<n>]". Deprecated `POST .../resource/actions` (body `action`)
  maps to V2 without parameters and logs a warning.
- **Edge cases**: every returned resource must be permitted by the project ("application is not permitted to manage
  <g>/<k>/<n> in <ns>"). Unsupported operation → "unsupported operation: <op>". Non-table return → "expect table output
  from Lua script, not <type>". Unknown action name → "error getting Lua resource action: built-in script does not exist".
- **Configuration surface**: actions customizations.
- **Exposure**: API; CLI `app actions run`; UI Actions menu.
- **Tests required**:
  - Positive: e2e `TestOldStyleResourceAction`, `TestCRDStatusSubresourceAction` parity.
  - Authorization: user without `action/apps/Deployment/restart` denied.

### SYNC-102 Action parameters
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/lua/lua.go#L106 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2697 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/utils.tsx#L632 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/resource_actions.md#L225
- **Maturity**: GA
- **Semantics**: Discovery declares `params = {{name = "..."}}`; run requests pass `resourceActionParameters`
  name/value strings, exposed to the script as global table `actionParams[name] = value`. UI prompts "Enter input
  parameters for action: <name>" for actions with params (otherwise "Perform <name> action?").
- **Edge cases**: the CLI `actions run` cannot pass parameters. Scripts validate values themselves (e.g. scale:
  "invalid number: <v>").
- **Configuration surface**: discovery `params`.
- **Exposure**: API; UI.
- **Tests required**:
  - Positive: scale Deployment with replicas=3 via API.
  - Negative: replicas="abc" → error "invalid number: abc".

### SYNC-103 Create-resource actions (`operation: create`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/lua/impacted_resource.go#L17 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2722 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/resource_actions.md#L34
- **Maturity**: GA (not listed in feature-maturity.md; prose docs still call it "An alpha feature, introduced in 2.8" — see Doc/implementation differences)
- **Semantics**: An action may return a list of impacted resources; `create` entries are first dry-run created
  (server-side `DryRun: All`) after project-permission checks for all entries, then created for real, and `patch`
  entries patched (patch only meaningful for the source resource). Creation is not transactional.
- **Edge cases**: any permission or dry-run failure aborts before real changes. Created resources are not tracked
  unless the script copies tracking metadata; auto-prune deletes them unless `Prune=false`.
- **Configuration surface**: action script output.
- **Exposure**: API; UI; CLI run.
- **Tests required**:
  - Positive: e2e `TestNewStyleResourceActionPermitted`, `TestNewStyleResourceActionMixedOk` parity.
  - Authorization: creating a kind not permitted by the project fails before any creation.

### SYNC-104 Custom actions configuration (argocd-cm)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L1140 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2662 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/resource_actions.md#L44
- **Maturity**: GA
- **Semantics**: `resource.customizations.actions.<group_Kind>` holds YAML `{discovery.lua, definitions[{name, action.lua}], mergeBuiltinActions}`. Custom actions replace built-ins
  for that kind unless `mergeBuiltinActions: true` (custom first). Discovery entries may set `disabled`, `iconClass`,
  `displayName`, `params`.
- **Edge cases**: no wildcard keys for actions (exact group/kind). Running a built-in action name that the custom
  discovery does not list still works (fallback lookup). Invalid YAML → error at discovery/run.
- **Configuration surface**: `argocd-cm` keys above.
- **Exposure**: config.
- **Tests required**:
  - Positive: custom CronJob suspend/resume actions from docs example.

### SYNC-105 Lua sandbox for actions
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/lua/lua.go#L75
- **Maturity**: GA
- **Semantics**: gopher-lua state with only package, base, table and a "safe" os library (preloaded so `require("os")`
  works); global `obj` (the live object) and `actionParams`; 1-second execution timeout; runtime error stack traces
  are stripped from messages returned to users.
- **Edge cases**: infinite loops abort after 1s.
- **Configuration surface**: none.
- **Exposure**: internal.
- **Tests required**:
  - Negative: script calling `io.open` fails; busy loop times out.

### SYNC-106 Actions CLI (`argocd app actions list|run`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/app_actions.go#L46 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/app_actions.go#L146
- **Maturity**: GA
- **Semantics**: `list APPNAME [--group --kind --namespace --resource-name] [-o yaml|json]` lists actions of matching
  managed resources (table GROUP KIND NAME ACTION DISABLED). `run APPNAME ACTION --kind K [--group --namespace
  --resource-name --all]` runs on managed resources (from ManagedResources, not child resources); `--all` required when
  several match; mixed groups → "Ambiguous resource group. Use flag --group to specify resource group explicitly.";
  falls back to the deprecated RPC when the server returns Unimplemented.
- **Edge cases**: `--kind` required. No parameter flag.
- **Configuration surface**: none.
- **Exposure**: CLI.
- **Tests required**:
  - Positive: `app actions run myapp restart --kind Deployment`.

### SYNC-107 Built-in actions: Deployment / StatefulSet / DaemonSet
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/apps/Deployment/actions/discovery.lua#L1 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/apps/Deployment/actions/restart/action.lua#L1 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/apps/StatefulSet/actions/scale/action.lua#L1 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/scale_application_resources.md#L1
- **Maturity**: GA
- **Semantics**: Deployment: `restart` (sets pod template annotation `kubectl.kubernetes.io/restartedAt` = UTC now),
  `pause`/`resume` (`spec.paused`; mutually disabled), `scale` (param `replicas` → `spec.replicas`). StatefulSet:
  `restart`, `scale`. DaemonSet: `restart`.
- **Edge cases**: scale with HPA or auto-sync self-heal gets reverted (documented).
- **Configuration surface**: none.
- **Exposure**: API/UI/CLI.
- **Tests required**:
  - Positive: action_test.yaml parity for each.

### SYNC-108 Built-in actions: Job and CronJob
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/batch/Job/actions/discovery.lua#L1 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/batch/Job/actions/terminate/action.lua#L1 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/batch/CronJob/actions/create-job/action.lua#L1
- **Maturity**: GA
- **Semantics**: Job (only while not completed): `suspend`/`resume` (`spec.suspend`), `terminate` (adds status condition
  FailureTarget "ManuallyTerminated" — requires status subresource patch). CronJob: `create-job` (create operation:
  Job named `<cronjob>-<yyMMddHHmm>` UTC from jobTemplate, annotation `cronjob.kubernetes.io/instantiate: manual`,
  controller ownerReference), `suspend`/`resume`.
- **Edge cases**: two create-job runs in the same minute collide (AlreadyExists).
- **Configuration surface**: none.
- **Exposure**: API/UI/CLI.
- **Tests required**:
  - Positive: create-job creates a Job owned by the CronJob.

### SYNC-109 Built-in actions: Argo Rollouts
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/argoproj.io/Rollout/actions/discovery.lua#L1
- **Maturity**: GA
- **Semantics**: `restart` ("Restart Pods": `spec.restartAt`=now), `resume` (clear `status.pauseConditions`, `spec.paused=false`),
  `abort` (`status.abort=true`), `retry` (clear `status.abort`), `pause` (`spec.paused=true`), `promote-full`
  (v0.10+: `status.promoteFull=true`; older: clear abort, jump to last step), `skip-current-step` (canary: clear pause
  conditions, `currentStepIndex+1`). Disabled flags depend on fully-promoted/abort/paused/strategy state.
- **Edge cases**: status-field actions require the status subresource patch path.
- **Configuration surface**: none.
- **Exposure**: API/UI/CLI.
- **Tests required**:
  - Positive: action_test.yaml parity for Rollout.

### SYNC-110 Built-in actions: other Argo kinds
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/argoproj.io/Application/actions/toggle-auto-sync/action.lua#L1 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/argoproj.io/AnalysisRun/actions/discovery.lua#L1 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/argoproj.io/CronWorkflow/actions/discovery.lua#L1 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-details/application-details.tsx#L1344
- **Maturity**: GA
- **Semantics**: Application `toggle-auto-sync` (display "Disable Auto-Sync"/"Enable Auto-Sync"; flips
  `automated.enabled`, creating `automated{enabled:true}` when absent) — used by the UI "Toggle Auto-Sync" button against
  the app itself; AnalysisRun `terminate` (`spec.terminate=true`, disabled when terminal); CronWorkflow and
  WorkflowTemplate `create-workflow` (create operation); promoter.argoproj.io PullRequest `merge` (`spec.state=merged`,
  enabled when open).
- **Edge cases**: toggling on the app itself requires `action/argoproj.io/Application/toggle-auto-sync` RBAC.
- **Configuration surface**: none.
- **Exposure**: API/UI/CLI.
- **Tests required**:
  - Positive: toggle-auto-sync twice returns to original state.

### SYNC-111 Built-in actions: third-party CRDs
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/resource_actions_builtin.md ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/keda.sh/ScaledObject/actions/discovery.lua#L1 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/source.toolkit.fluxcd.io/GitRepository/actions/discovery.lua#L1
- **Maturity**: GA
- **Semantics**: Shipped action scripts for: Flux (`source.toolkit.fluxcd.io` Bucket/GitRepository/HelmChart/HelmRepository/OCIRepository,
  `helm.toolkit.fluxcd.io/HelmRelease`, `kustomize.toolkit.fluxcd.io/Kustomization`, `image.toolkit.fluxcd.io`
  ImageRepository/ImageUpdateAutomation, `notification.toolkit.fluxcd.io` Alert/Provider/Receiver: reconcile (annotation
  `reconcile.fluxcd.io/requestedAt`), suspend, resume); KEDA ScaledObject (pause, paused-replicas, resume) and
  ScaledJob (pause, resume); external-secrets ExternalSecret `refresh` and PushSecret `push`; CloudNativePG Cluster
  (promote, reload, restart, resume, suspend); Strimzi StrimziPodSet `rolling-update`; MariaDB (resume, suspend);
  Percona PerconaServerMongoDB `unpause`; Numaflow InterStepBufferService/MonoVertex/Pipeline (force-promote, pause,
  unpause-fast, unpause-gradual, Pipeline delete-marking); Numaplane MonoVertexRollout/PipelineRollout (pause,
  unpause-*, allow/disallow-data-loss). Full list: 93 action directories under `resource_customizations/**/actions/*`.
- **Edge cases**: parity requires byte-equivalent outputs per each `action_test.yaml`.
- **Configuration surface**: none.
- **Exposure**: API/UI/CLI.
- **Tests required**:
  - Positive: run every upstream `action_test.yaml` discovery/action case against Patrol's Lua runtime.

### SYNC-112 Resource action RBAC and project checks
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2673 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2821 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/rbac.md#L186
- **Maturity**: GA
- **Semantics**: Running requires `applications, action/<group>/<kind>/<action>` on `<project>/<app>` (core group empty:
  `action//Pod/x`); wildcards supported by the RBAC engine (`action/*`, `action/apps/Deployment/*`). Listing needs `get`.
  Each affected resource must pass `IsResourcePermitted` for the app's project and destination.
- **Edge cases**: impersonation (SYNC-093) applies to the cluster calls.
- **Configuration surface**: RBAC policy CSV.
- **Exposure**: API.
- **Tests required**:
  - Authorization: policy `action/apps/Deployment/*` allows restart but not `action//Pod/...`.

## L. Impersonation of API-server live operations

Since 3.5, when impersonation is enabled, the API server also impersonates the destination service account for the
calls it makes to an Application's destination cluster. This section lists each affected operation separately (the
shared rules are in SYNC-113), plus the API-server paths that are explicitly not impersonated (SYNC-123, SYNC-124).
For the controller-side sync and deletion behaviour, see SYNC-093 and SYNC-094.

### SYNC-113 API-server impersonated cluster config resolution (shared rules)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1401 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1411 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1420 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1431 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1436 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1440 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/impersonation.go#L19 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L2742 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L2751 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L4081 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/cluster-rbac/server/argocd-server-clusterrole.yaml#L9 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application_test.go#L5180 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/3.4-3.5.md#L108 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/app-sync-using-impersonation.md#L33
- **Maturity**: Beta (Service Account Impersonation, `docs/operator-manual/feature-maturity.md#L26`)
- **Semantics**: Each API-server RPC that reaches the destination cluster for a user request (SYNC-114 to SYNC-122)
  builds its Kubernetes client from one shared resolver, `getApplicationClusterConfig(app, project)`:
  1. Resolve the destination cluster from `spec.destination` (server or name). Errors are returned as "error validating destination: ...".
  2. Build the cluster REST config with Argo CD's client defaults (`Cluster.RESTConfig()`, which is `RawRestConfig()` plus `SetK8SConfigDefaults`).
  3. Read `application.sync.impersonation.enabled` from `argocd-cm` on every call. Only the exact string `"true"` enables
     impersonation. When it is disabled, the cluster credentials are returned unchanged.
  4. When it is enabled, derive the service account with the same algorithm as sync (SYNC-093): the first
     `spec.destinationServiceAccounts[]` entry of the Application's project whose `server` glob matches the cluster URL and
     whose `namespace` glob matches `spec.destination.namespace`.
  5. On a match, set only `Impersonate.UserName = system:serviceaccount:<ns>:<name>`. No impersonated groups, UID or
     extra fields are set.
  6. With no match, read `application.sync.impersonation.enforced`, which defaults to true when the key is absent and is
     true for any value other than `"false"`. If enforced, the error is "no matching service account found for destination
     server <server> and namespace <namespace>". If not enforced, Argo CD writes the info log "no matching service account
     found for impersonation for app <appns>/<app> (project: <proj>, server: <server>, namespace: <ns>), falling back to
     controller service account" and returns the credentials without impersonation.
  Callers wrap resolver errors as "error getting application cluster config: <cause>". In the API server, the identity
  used when nothing is impersonated is the cluster secret's credentials. For an in-cluster destination without
  credentials, it is the `argocd-server` pod's own service account. It is not the controller's service account, despite
  the log text. Because the settings and project are read on each call, changes apply to the next request without a
  restart. The following are never impersonated: the resource tree and managed-resource lookups that each operation runs
  first (these read the controller's cache), application-level events (SYNC-115), the web terminal (SYNC-123) and the
  API-server server-side diff (SYNC-124).
- **Edge cases**:
  - Derivation errors propagate as "error deriving service account to impersonate: invalid glob pattern for destination
    server|namespace: ..." or "... default service account contains invalid chars '<v>'".
  - A ConfigMap read failure gives "error getting impersonation setting: ..." or "error getting impersonation enforcement
    setting: ...".
  - Kubernetes only accepts impersonation from an identity that has `impersonate` on `serviceaccounts`. The shipped
    `argocd-server` ClusterRole grants get/patch/delete on `*/*`, list events, get pods and pods/log, get/list/watch
    applications(sets), and create jobs and workflows, but not `impersonate`. So for an in-cluster destination that uses
    the pod identity, Kubernetes rejects impersonated API-server calls unless the operator adds that permission. External
    clusters use the cluster secret's credentials.
  - Within a request, the impersonation check runs after Argo CD RBAC and, for resource-scoped operations, after the
    resource-tree membership check. A request denied by RBAC, or one that names a resource outside the tree, never reaches
    the resolver.
- **Configuration surface**: `argocd-cm` `application.sync.impersonation.enabled` (default false) and
  `application.sync.impersonation.enforced` (default true); AppProject `spec.destinationServiceAccounts[]`; cluster
  secrets (the credentials that do the impersonating).
- **Exposure**: CRD (AppProject); API (internal mechanism behind the RPCs in SYNC-114 to SYNC-122); no direct CLI or UI.
- **Tests required**:
  - Positive: `TestGetApplicationClusterConfig/ImpersonationDisabled` → `Impersonate.UserName` empty.
  - Positive: `.../ImpersonationEnabledWithMatch` → `system:serviceaccount:<destns>:test-sa`; `.../ImpersonationEnabledWithMatchEnforcementDisabled` → still impersonated.
  - Negative: `.../ImpersonationEnabledWithNoMatch` → error contains "no matching service account found".
  - Positive: `.../ImpersonationEnabledWithNoMatchEnforcementDisabled` → no impersonation, info log emitted.
  - Positive: outgoing requests carry exactly one `Impersonate-User` header and no `Impersonate-Group` / `Impersonate-Uid` / `Impersonate-Extra-*`.
  - Recovery: set `enabled` to `"false"` in `argocd-cm` and confirm the next call is not impersonated without restarting
    the server; add a matching `destinationServiceAccounts` entry and confirm the next call succeeds.
  - Authorization: an impersonated SA that lacks the required verb gets the Kubernetes Forbidden text inside the RPC error.

### SYNC-114 Impersonated cluster discovery for GetManifests / GetManifestsWithFiles
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L491 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L513 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L518 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L523 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L652 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L682 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L687 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/utils/kube/ctl.go#L168 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/utils/kube/ctl.go#L355 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.proto#L405 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.proto#L410 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/app.go#L1885 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/app.go#L2918 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/app_diff.go#L401 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/app_diff.go#L420
- **Maturity**: Beta
- **Semantics**: `GET /api/v1/applications/{name}/manifests` (GetManifests) and the client-streaming
  `POST /api/v1/applications/manifestsWithFiles` (GetManifestsWithFiles) both require RBAC `applications, get`. Before
  calling the repo-server, each resolves the impersonated config (SYNC-113) and uses it to fetch the destination's server
  version (`GET /version`) and its API resources through discovery (only resources that support both list and watch).
  These become the `KubeVersion` and `ApiVersions` of the manifest request, which feed Helm `.Capabilities` and Kustomize.
  Under impersonation, the generated manifests therefore reflect what the impersonated SA can discover. Discovery runs on
  every call and is not cached. No resource verbs are needed, only read access to the non-resource discovery URLs
  (`/version`, `/api`, `/apis/...`).
- **Edge cases**:
  - If impersonation is enforced and no SA matches, the whole call fails with "error getting application cluster config:
    no matching service account found ...", even though generating manifests writes nothing to the cluster.
  - A discovery failure gives "error getting server version: failed to get server version: ..." or "error getting API
    resources: ...".
  - GetManifests checks for a namespace that is not enabled (NamespaceNotPermittedError) before resolving the cluster config.
  - GetManifestsWithFiles resolves impersonation with the project returned by the RBAC lookup, then reloads the project
    for repository lookup.
  - Patrol must not add impersonated groups, which would change the discovery permissions the API server sees.
- **Configuration surface**: as SYNC-113.
- **Exposure**: API (both RPCs); CLI `argocd app manifests APPNAME --revision <r>` (and `--revisions/--source-positions`),
  `argocd app diff --revision ...` and `argocd app diff --local ... --server-side-generate` (GetManifestsWithFiles), and
  `argocd app sync --label ...` (fetches the target manifests to select resources). The UI service defines
  `getManifest` but no component calls it (see Unconfirmed).
- **Tests required**:
  - Positive: impersonation enabled with a matching SA → manifests are returned, and `/version` and discovery requests carry `Impersonate-User`.
  - Negative: enforced with no match → the error above, and the repo-server is never called.
  - Positive: not enforced with no match → succeeds with the server's credentials.
  - Negative: the SA is denied discovery (system:discovery binding removed) → "error getting server version".
  - Positive: `argocd app sync --label` under enforced impersonation with no match fails before the sync is submitted.

### SYNC-115 Impersonated resource event listing (ListResourceEvents)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L899 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L913 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L927 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L939 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L955 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.proto#L61 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.proto#L362 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/3.4-3.5.md#L121
- **Maturity**: Beta
- **Semantics**: `GET /api/v1/applications/{name}/events?resourceNamespace&resourceName&resourceUID&appNamespace&project`
  requires RBAC `applications, get`. It has two modes:
  - Application events (`resourceName` and `resourceUID` both empty): Argo CD lists Events in the Application's namespace
    on the control-plane cluster, with field selector involvedObject name/uid/namespace equal to the Application, using
    the API server's own clientset. This mode is never impersonated, and the resolver is not called, so it works even
    when impersonation is enforced and no SA matches.
  - Resource events: the target must be in the app tree (`nodes` or `orphanedNodes`) with an exact match on UID, name and
    namespace. Argo CD then resolves the impersonated config and lists `events` in `resourceNamespace` with field selector
    `involvedObject.name/uid/namespace`.
  Required verb: `list` on core `events` in the resource's namespace.
- **Edge cases**:
  - A resource not in the tree returns InvalidArgument "<resourceName> not found as part of application <name>", with no
    cluster call.
  - For a cluster-scoped resource, `resourceNamespace` is empty, so the list is cluster-wide and the SA needs a
    cluster-wide `list events`.
  - Impersonation errors return "error getting application cluster config: ...". Forbidden returns "error listing
    resource events: ...".
  - Orphaned resources are accepted.
- **Configuration surface**: as SYNC-113.
- **Exposure**: API; UI application "Events" tab (application mode) and resource details "Events" tab (resource mode); no CLI.
- **Tests required**:
  - Positive: resource events listed with an SA that has `list events` in the namespace; the request carries `Impersonate-User`.
  - Negative: an SA without `list events` → "error listing resource events".
  - Positive: application events under enforced impersonation with no match still succeed, without impersonation.
  - Positive: a cluster-scoped resource (empty namespace) triggers an all-namespaces list.
  - Negative: a resource not in the tree → InvalidArgument and no Kubernetes call.

### SYNC-116 Impersonated live manifest read (GetResource)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1485 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1508 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1512 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1520 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1530 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/utils/kube/ctl.go#L181 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.proto#L490 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/resource-details/resource-details.tsx#L277 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/3.4-3.5.md#L118
- **Maturity**: Beta
- **Semantics**: `GET /api/v1/applications/{name}/resource?namespace&resourceName&version&group&kind&appNamespace&project`
  requires RBAC `applications, get`. The node must be in the cached app tree with a non-empty UID. Argo CD then resolves
  the impersonated config, uses discovery to map the GVK to a resource that supports `get`, and runs `GET` on the object
  as the impersonated SA. A request `version` overrides the version from the tree. For core `Secret`, `data` values are masked before the response is returned. Required verb: `get`.
- **Edge cases**:
  - A node missing from the tree, or one with an empty UID, returns InvalidArgument "<kind> <group> <name> not found as
    part of application <app>".
  - Resolver errors return "error getting application cluster config: ...". Forbidden or discovery failures return
    "error getting resource: ...".
  - The UI resource-details panel catches GetResource errors and shows no live state instead of an error. A Forbidden
    response for the impersonated SA therefore shows an empty live manifest.
- **Configuration surface**: as SYNC-113.
- **Exposure**: API; CLI `argocd app get-resource APPNAME --kind K [--resource-name N --group G --project P --filter-fields F --show-managed-fields -o yaml|json]`; UI resource details "Live Manifest" and the diff view.
- **Tests required**:
  - Positive: an SA with `get` → the manifest is returned; a Secret is masked.
  - Negative: an SA without `get` → error containing "forbidden"; under enforced impersonation with no match → resolver error.
  - Positive: with impersonation disabled, the request is sent with the server's credentials and no `Impersonate-User`.
  - Authorization: a user without `applications, get` is denied before any cluster call.

### SYNC-117 Impersonated resource patch (PatchResource)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1558 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1574 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1578 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1593 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1497 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/utils/kube/ctl.go#L229 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/grpc/grpc.go#L27 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.proto#L495 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/3.4-3.5.md#L119
- **Maturity**: Beta
- **Semantics**: `POST /api/v1/applications/{name}/resource` (body = the patch string; query namespace, resourceName,
  version, group, kind, patchType, appNamespace, project) requires RBAC `applications, update`, or the fine-grained
  `update/<group>/<kind>/<ns>/<name>` (RBAC area). After the tree check, Argo CD resolves the impersonated config, runs
  discovery, and sends one `PATCH` with the requested patch type. It does not GET the object first, so code only needs
  `patch` (the upgrade doc lists get and patch). Secret results are masked. Argo CD emits the Kubernetes Event (reason
  ResourceUpdated) "patched resource <group>/<kind> '<name>'".
- **Edge cases**:
  - A failed patch of a core Secret returns only "failed to patch Secret <ns>/<name>", so no secret data leaks. Other
    failures return "error patching resource: <k8s msg>". A nil result returns "failed to patch resource: manifest was nil".
  - Upstream defect (found by reading the code, not by running it): when
    `server.rbac.disableApplicationFineGrainedRBACInheritance` is `"false"` and the user lacks app-level `update` but has
    the fine-grained permission, the retry RBAC lookup throws away the project. The resolver then gets a nil project.
    With impersonation enabled, this is a nil-pointer panic, which the gRPC recovery handler turns into `codes.Internal`
    with the panic text. With the default setting (inheritance disabled when the key is empty), the fine-grained action is
    checked first and the project is kept. Patrol should impersonate using the Application's project in both paths (see
    Doc/implementation differences).
- **Configuration surface**: as SYNC-113, plus `argocd-cm` `server.rbac.disableApplicationFineGrainedRBACInheritance`.
- **Exposure**: API; CLI `argocd app patch-resource APPNAME --patch P --patch-type T --kind K --resource-name N [--group --namespace --all --project]`; UI resource details "Live Manifest" Edit/Save.
- **Tests required**:
  - Positive: an SA with only `patch`, and no `get`, can patch.
  - Negative: an SA without `patch` → "error patching resource: ... forbidden".
  - Negative: a Forbidden patch on a Secret → exactly "failed to patch Secret ns/name".
  - Authorization: a user without `update` (and without fine-grained permission) is denied before any cluster call.
  - Positive (Patrol target): fine-grained `update/...` with inheritance enabled and impersonation enabled impersonates the project SA. Upstream returns Internal.

### SYNC-118 Impersonated resource deletion (DeleteResource)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1601 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1619 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1623 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1630 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1634 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/utils/kube/ctl.go#L253 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.proto#L528 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/app_resources.go#L382 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/3.4-3.5.md#L120
- **Maturity**: Beta
- **Semantics**: `DELETE /api/v1/applications/{name}/resource?...&force&orphan` requires RBAC `applications, delete`, or
  the fine-grained `delete/<group>/<kind>/<ns>/<name>`. After the tree check, Argo CD resolves the impersonated config,
  runs discovery, and sends one `DELETE` as the impersonated SA. Propagation is `orphan` → Orphan; otherwise
  `force` → Background with `gracePeriodSeconds: 0`; otherwise Foreground. Orphan takes precedence over force. Argo CD
  emits the Event (reason ResourceDeleted) "deleted resource <group>/<kind> '<name>'". Required verb: `delete`.
- **Edge cases**:
  - Forbidden returns "error deleting resource: ...". Resolver errors return "error getting application cluster config: ...".
  - The fine-grained inheritance nil-project defect described in SYNC-117 applies to `delete` too.
  - The UI pod "Delete" shortcut always sends `orphan=false`, with `force` taken from its dialog checkbox.
- **Configuration surface**: as SYNC-117.
- **Exposure**: API; CLI `argocd app delete-resource APPNAME --kind K --resource-name N [--group --namespace --force --orphan --all --orphaned --project]`; UI resource "Delete" dialog (foreground/background/orphan choices) and the pod Delete action.
- **Tests required**:
  - Positive: an SA with `delete` deletes, and the propagation policy matches the flags in each of the three cases.
  - Negative: an SA without `delete` → "error deleting resource: ... forbidden".
  - Negative: enforced impersonation with no match → the resolver error, and the object is not deleted.
  - Authorization: a user without `delete` is denied.

### SYNC-119 Impersonated pod log streaming (PodLogs)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1855 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1894 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1903 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1915 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1925 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1931 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1952 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L949 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.proto#L533 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/3.4-3.5.md#L122
- **Maturity**: Beta
- **Semantics**: `GET /api/v1/applications/{name}/pods/{podName}/logs` and `GET /api/v1/applications/{name}/logs`
  (server stream) require RBAC `applications, get` and `logs, get`. The order is: parse `untilTime`, run the RBAC
  checks, load the tree, resolve the impersonated config, then select pods from the tree by
  kind/group/resourceName/namespace. Because the config is resolved before pod selection, an enforced no-match fails even
  when no pod would be selected. With zero pods the stream ends with no error. With more pods than
  `server.maxPodLogsToRender` (default 10), the RPC returns InvalidArgument "max pods to view logs are reached. Please
  provide more granular query". For each pod, Argo CD calls `GET pods/{name}/log` as the impersonated SA, with container,
  follow, `timestamps=true`, sinceSeconds, sinceTime, tailLines and previous. The code calls only the `pods/log`
  subresource and needs `get` on it.
- **Edge cases**:
  - If a pod's log stream cannot be opened (for example Forbidden for the impersonated SA), the RPC does not fail. The
    Kubernetes error text is sent as a log line for that pod, and the other pods keep streaming.
  - Resolver errors end the stream with "error getting application cluster config: ...".
  - An invalid `untilTime` returns "invalid untilTime parameter value: ..." before RBAC runs.
- **Configuration surface**: as SYNC-113, plus `argocd-cm` `server.maxPodLogsToRender`.
- **Exposure**: API; CLI `argocd app logs APPNAME [--kind --group --name --namespace -c/--container -f/--follow --tail --since-seconds --until-time --filter -m/--match-case -p/--previous]`; UI resource details "Logs" tab.
- **Tests required**:
  - Positive: an SA with `get pods/log` streams logs, and the request carries `Impersonate-User`.
  - Negative: an SA without `pods/log` → the stream holds a line with the Forbidden text, and the RPC status is OK.
  - Negative: enforced with no match → the stream errors before any log request, even for a query that selects no pods.
  - Authorization: a user without `logs, get` is denied before any cluster call.

### SYNC-120 Impersonated resource action discovery (ListResourceActions)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2561 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2583 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2599 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2609 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application_test.go#L5330 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.proto#L503
- **Maturity**: Beta (impersonation behaviour; action discovery itself is SYNC-100, GA)
- **Semantics**: `GET /api/v1/applications/{name}/resource/actions` requires RBAC `applications, get`.
  - For a managed resource: tree check, then the impersonated config, then discovery plus `GET` of the live object as
    the impersonated SA. The Lua discovery scripts see the object as that SA reads it. Required verb: `get` on the target.
  - For the Application itself (kind `Application`, group `argoproj.io`, `resourceName` equal to the app name): the
    object comes from the informer with no cluster GET, but the impersonated config is still resolved. An enforced
    no-match therefore fails the listing even though no cluster call is made.
- **Edge cases**:
  - Resolver errors return "error getting application cluster config: ...". Forbidden returns "error getting resource: ...".
  - The upgrade doc's permission table does not list ListResourceActions.
- **Configuration surface**: as SYNC-113.
- **Exposure**: API; CLI `argocd app actions list APPNAME`; UI resource "Actions" menu (populated on open).
- **Tests required**:
  - Positive: `TestGetUnstructuredLiveResourceOrAppWithImpersonation` (Application self path → impersonated config `system:serviceaccount:<destns>:test-sa`).
  - Positive: for a managed Deployment, the GET carries `Impersonate-User`.
  - Negative: an SA without `get` → "error getting resource"; enforced with no match on the self path → resolver error.

### SYNC-121 Impersonated resource action execution (RunResourceAction / RunResourceActionV2)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2662 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2673 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2729 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2735 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2756 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2783 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2799 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2801 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2840 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/utils/kube/ctl.go#L205 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/3.4-3.5.md#L123
- **Maturity**: Beta (impersonation behaviour; action execution itself is SYNC-101, GA)
- **Semantics**: `POST /api/v1/applications/{name}/resource/actions/v2` (and the deprecated `.../resource/actions`)
  requires RBAC `applications, action/<group>/<kind>/<action>`. All cluster calls use the single impersonated config
  resolved for the Application, in this order:
  1. `GET` the live object, except on the Application self path (SYNC-122).
  2. Run the Lua action.
  3. For each impacted resource, run the Argo CD project permission check (not Kubernetes RBAC). For each `create`
     operation, send a server-side dry-run `create` (`dryRun: [All]`). All dry runs finish before any real write.
  4. Write in the order the script returned: a `patch` computes a JSON merge patch, and `{}` means no Kubernetes call.
     If the patch changes `status`, Argo CD first sends `PATCH` to the `status` subresource; a NotFound means there is
     no status subresource, and Argo CD falls back to one full patch. It then sends `PATCH` for the remaining fields. A
     `create` sends `POST`.
  Required verbs: `get` on the target; `create` on every kind a create-type action makes (for example `jobs` for CronJob
  `create-job`); `patch` on patched kinds, plus `patch` on `<resource>/status` when the action changes status.
- **Edge cases**:
  - The sequence is not transactional. If a later step fails, earlier patches and creates stay applied.
  - A Forbidden dry-run create is returned unwrapped, and nothing is written.
  - A non-NotFound error from the status-subresource patch returns "error patching resource: ..." without attempting
    the main patch.
  - A create failure returns "error creating resource: ...".
  - Custom actions may need more verbs, depending on what they return.
- **Configuration surface**: as SYNC-113; actions customizations (SYNC-104).
- **Exposure**: API; CLI `argocd app actions run APPNAME ACTION --kind K [--resource-name --group --namespace --all]` (the CLI sends no parameters); UI resource "Actions" menu.
- **Tests required**:
  - Positive: Deployment `restart` with an SA that has `get` and `patch`.
  - Positive: CronJob `create-job` with an SA that has `get cronjobs` and `create jobs`.
  - Negative: CronJob `create-job` with an SA lacking `create jobs` → the dry-run fails and no Job exists afterwards.
  - Negative: a status-changing action on a CRD with a status subresource where the SA lacks `patch` on `<res>/status` → error, main patch not applied.
  - Authorization: a user without `action/...` is denied before any cluster call.

### SYNC-122 Application self-action (toggle-auto-sync) under impersonation
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2584 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2599 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2603 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/argoproj.io/Application/actions/discovery.lua#L1 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-summary/application-summary.tsx#L563 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-details/application-details.tsx#L1370
- **Maturity**: Beta
- **Semantics**: Users who fail the `canI applications update` check toggle auto-sync in the UI by running the built-in
  action `toggle-auto-sync` on the Application itself (kind `Application`, group `argoproj.io`, name equal to
  resourceName). The application details menu always uses the action; the summary panel uses it only when the user
  lacks `update`. RBAC is `applications, action/argoproj.io/Application/toggle-auto-sync`. The server reads the
  Application from the informer, resolves the cluster config for the Application's destination cluster (impersonated
  per SYNC-113), and sends the merge patch for the Application object through that config. Under impersonation, the
  destination SA therefore needs `patch` on `applications.argoproj.io` in the Application's namespace, on the cluster
  that the destination points to. The audit Event is "ran action toggle-auto-sync", with no resource suffix.
- **Edge cases**:
  - Enforced with no match → the toggle fails ("error getting application cluster config: ...").
  - For a remote destination, the patch goes to the remote cluster rather than the control plane. The runtime result of
    that was not verified (see Unconfirmed).
- **Configuration surface**: as SYNC-113.
- **Exposure**: API; UI application details "Toggle Auto-Sync" menu item and the summary "Enable/Disable Auto-Sync" button; no CLI.
- **Tests required**:
  - Positive: an in-cluster destination with an SA that can patch Applications in the app namespace → `spec.syncPolicy.automated.enabled` flips.
  - Negative: an SA without patch on Applications → error; enforced with no match → error.
  - Authorization: a user without the action permission is denied.

### SYNC-123 Web terminal is not impersonated
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/terminal.go#L62 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/terminal.go#L86 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/terminal.go#L155 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/terminal.go#L182 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/terminal.go#L206 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/terminal.go#L306 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L1227 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/3.4-3.5.md#L110
- **Maturity**: Beta
- **Semantics**: The `/terminal` WebSocket handler (HTTP, not gRPC) is enabled only with `argocd-cm` `exec.enabled: "true"`
  (otherwise 404) and requires RBAC `applications, get` and `exec, create`. It builds its client from
  `Cluster.RawRestConfig()` and never calls the impersonation resolver. The pod `GET` and the `pods/exec` stream always run
  with Argo CD's own credentials for the cluster, whatever the values of `application.sync.impersonation.enabled` and
  `.enforced` and whether an SA matches. The permissions of the impersonated SA have no effect on the terminal.
- **Edge cases**: With impersonation enforced and no matching `destinationServiceAccounts`, the terminal still works while
  logs, events and resource reads fail. This is a privilege difference that operators may not expect, because the
  3.4→3.5 note says impersonation now covers "all API server operations".
- **Configuration surface**: `argocd-cm` `exec.enabled`, `exec.shells`; RBAC `exec, create`.
- **Exposure**: API (HTTP `/terminal?pod&container&appName&appNamespace&projectName&namespace`); UI pod "Terminal" tab; no CLI.
- **Tests required**:
  - Positive (parity): impersonation enabled and enforced with no matching SA → the terminal session starts, and the exec
    request has no `Impersonate-User` header.
  - Negative: `exec.enabled` unset → 404; a user without `exec, create` → forbidden.

### SYNC-124 API-server ServerSideDiff is not impersonated
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2992 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L3018 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L3029 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.proto#L460 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/app_diff.go#L291
- **Maturity**: Beta
- **Semantics**: `GET /api/v1/applications/{appName}/server-side-diff` requires RBAC `applications, get`. It builds its
  client from `Cluster.RawRestConfig()` with no impersonation. The OpenAPI schema load and the server-side-apply dry runs
  run with Argo CD's own cluster credentials, whatever the impersonation settings. The controller's comparison
  (including `ServerSideDiff=true` compare option) is not impersonated either; only sync and deletion are impersonated
  on the controller side (SYNC-093).
- **Edge cases**: With impersonation enforced and no matching SA, ServerSideDiff still succeeds. Dry-run results can
  differ from what the impersonated SA would get at sync time; for example, an admission policy keyed on the user.
- **Configuration surface**: none specific.
- **Exposure**: API; CLI `argocd app diff --server-side-diff` (or the app's compare-options annotation `ServerSideDiff=true`).
- **Tests required**:
  - Positive (parity): enforced impersonation with no match → ServerSideDiff returns results, and the dry-run requests carry no `Impersonate-User`.

## Unconfirmed

- Multi-source rollback restriction mentioned in the task scope: not found. Checked
  `server/application/application.go` (Rollback), `cmd/argocd/commands/app.go` (NewApplicationRollbackCommand,
  findRevisionHistory), `ui/src/app/applications/components/application-deployment-history/application-deployment-history.tsx`,
  `docs/user-guide/multiple_sources.md`; the server rolls back `Sources`/`Revisions` from history.
- UI controls for multi-source revisions in the sync panel: the panel shows a single "Revision" field bound to the default
  source (`getAppDefaultSource`); whether a per-source revision editor exists elsewhere was not verified
  (`ui/src/app/applications/components/application-sync-panel/application-sync-panel.tsx`, `revision-form-field/`).
- Exact UI bulk-sync panel option set (`ui/src/app/applications/components/applications-sync-panel/applications-sync-panel.tsx`)
  was only skimmed (uses the same ApplicationSyncOptions and ApplyOnly/DryRun/Force flags).
- Behaviour of an operation whose `retry.refresh` clears the revision for multi-source apps beyond clearing
  `revision`/`revisions` (resolution then follows CompareAppState defaults) — not traced end-to-end in
  `controller/state.go` GetRepoObjs.
- Whether the legacy single-key `resource.customizations` YAML accepts an `actions` entry per group/kind (`ResourceOverride.Actions` has no json tag; checked `util/settings/settings.go#L1051`, `pkg/apis/application/v1alpha1/types.go#L2595`); only the split key `resource.customizations.actions.<group_Kind>` was confirmed.
- Whether `server.sync.replace.allowed` is also enforced for Rollback (spec option `Replace=true`): Rollback handler does not
  check it (`server/application/application.go#L2265-L2325`); not tested.

- UI use of GetManifests (SYNC-114): `getManifest` is defined in `ui/src/app/shared/services/applications-service.ts#L176`,
  but no caller was found under `ui/src/app` (searched for `getManifest(`), so UI exposure is recorded as none.
- Runtime result of the Application self-action (SYNC-122) for a remote destination. The code sends the Application
  merge patch through the destination cluster's config (`server/application/application.go#L2599`, `#L2603`, `#L2778`);
  the actual outcome (NotFound, missing CRD, or patching an unrelated object) was not executed.
- The nil-project panic on the fine-grained RBAC inheritance retry path under impersonation (SYNC-117/SYNC-118) comes
  from reading the code (`server/application/application.go#L1494-L1497`, `#L1512`, `util/settings/impersonation.go#L28`)
  and was not reproduced.
- Which groups Kubernetes attaches to an impersonated `system:serviceaccount:*` user when no `Impersonate-Group` is sent
  (this decides whether default discovery/`system:authenticated` bindings apply to SYNC-114). This is Kubernetes
  apiserver behaviour, not verified in this checkout.

## Doc/implementation differences

- `docs/user-guide/auto_sync.md#L141` says self-heal retries "after self-heal timeout (5 seconds by default)" controlled by
  `--self-heal-timeout-seconds`. Code: `--self-heal-timeout-seconds` defaults to 0 and is only used when exponential backoff
  is disabled; by default backoff (2s × 3^n, cap 300s) applies (`argocd_application_controller.go#L274-L278`,
  `appcontroller.go#L2603`).
- Auto-sync operations default to `retry.limit: 5` when `spec.syncPolicy.retry` is unset (`appcontroller.go#L2475`); not
  documented in `auto_sync.md`.
- `docs/user-guide/sync-waves.md#L49-L50` describes `HookFailed` as "deleted after the hook failed"; code deletes completed
  hooks with HookFailed when the *operation* fails, even if the hook itself succeeded (`sync_context.go#L625`, `#L637`).
- `docs/operator-manual/rbac.md#L220` says override sync is impossible with auto-sync; code allows it for dry runs
  (`application.go#L2108`, `#L2227`, `#L2252`).
- `docs/user-guide/helm.md#L531` says "If you define any Argo CD hooks, all Helm hooks will be ignored"; code ignores Helm hook
  types only on the same object that has a valid Argo CD hook type (`hook/hook.go#L51-L56`); Helm delete policies are
  always merged.
- `docs/operator-manual/resource_actions.md#L227` says parameters are defined in a `parameters` key; the API/Lua contract uses
  `params` (`types.go#L2686`, built-in `discovery.lua` files). Built-in scripts also emit `default` for params, which the
  API model (`ResourceActionParam` has only `name`) drops.
- Resource actions docs describe parameters generally; the CLI `argocd app actions run` cannot pass parameters
  (`app_actions.go#L207` TODO).
- `docs/user-guide/sync-options.md` lists `Replace=false` nowhere but the engine defines `Replace=false` and
  `ApplyOutOfSyncOnly=false` constants that have no effect (`common/types.go#L30`, `#L40`).
- `docs/user-guide/sync-kubectl.md#L137` example uses `operations:` (should be `operation:`) and `#L115` mentions a "groups"
  field (the field is `group`).
- `docs/user-guide/app-sync-using-impersonation.md#L82` states a missing service-account match fails the operation when
  enforced; during Application deletion the controller does not apply enforcement and falls back to the controller identity
  (`appcontroller.go#L2923-L2938`).
- `docs/user-guide/sync-options.md#L51` implies only pruned `Prune=confirm` resources block; code blocks the whole wave
  (including applies in that wave) and asks for confirmation even when the sync was not started with prune
  (`sync_context.go#L1618-L1625`).
- The CLI `argocd app sync` replaces (not merges) app-level sync options when `--replace/--server-side/--apply-out-of-sync-only`
  are used (`app.go#L1955-L1974`, `application.go#L2130-L2132`); undocumented.
- `docs/operator-manual/resource_actions.md#L36` labels list-returning (create/patch) actions "An alpha feature, introduced in
  2.8", but `docs/operator-manual/feature-maturity.md` does not list it; this inventory records it as GA per the maturity rule
  (SYNC-103).

- `docs/operator-manual/upgrading/3.4-3.5.md#L110` says impersonation now applies to "all API server operations". Code
  does not impersonate the web terminal (`server/application/terminal.go#L62`, `#L182`, raw config), the API-server
  ServerSideDiff (`server/application/application.go#L3018`), or application-level ListResourceEvents, which uses the
  server's own clientset (`application.go#L914`). `docs/operator-manual/app-sync-using-impersonation.md#L33` likewise
  says impersonation covers "UI operations triggered by the user", but the UI terminal is not covered (SYNC-123, SYNC-124).
- The permission table in `docs/operator-manual/upgrading/3.4-3.5.md#L116-L123` differs from the code in four ways:
  - Patch resource lists `get, patch`, but the code only sends PATCH (`application.go#L1574`).
  - Pod logs lists `get` on `pods` and `pods/log`, but the code only calls the `pods/log` subresource (`application.go#L1931`).
  - It omits the impersonated discovery in GetManifests/GetManifestsWithFiles (`application.go#L513`, `#L682`), the
    impersonated GET in ListResourceActions (`application.go#L2609`), and the `status`-subresource PATCH used by actions
    (`application.go#L2799`).
  - `create` is needed only for create-type actions, as a dry run and then a create (`application.go#L2735`, `#L2840`).
- The API-server fallback log (`server/application/application.go#L1436`) says "falling back to controller service
  account". In the API server, the identity used is the cluster secret's credentials, or the `argocd-server` pod's SA for
  an in-cluster destination without credentials (`pkg/apis/application/v1alpha1/types.go#L3983-L3986`).
- Neither impersonation doc says that the Argo CD credential for a cluster must hold `impersonate` on `serviceaccounts`.
  The shipped `argocd-server` ClusterRole (`manifests/cluster-rbac/server/argocd-server-clusterrole.yaml#L9-L51`) does not
  grant it (the controller ClusterRole grants `*`). So once impersonation is enabled, impersonated API-server calls to an
  in-cluster destination that uses the pod identity are rejected by Kubernetes unless the operator adds the permission.
- Upstream defect: in `getAppLiveResource`, the fine-grained RBAC retry (`application.go#L1497`) throws away the project,
  and `getApplicationClusterConfig` receives `nil`. With impersonation enabled and
  `server.rbac.disableApplicationFineGrainedRBACInheritance: "false"`, PatchResource and DeleteResource end in a recovered
  panic (`codes.Internal`). Patrol should derive the SA from the Application's
  project instead (SYNC-117).
