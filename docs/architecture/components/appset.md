# Component: `internal/appset`

Status: implemented (library and controller; not yet wired into `cmd/patrolcd`, the API server
or the webhook handler). Decision records: ADR-0003 (resource model), ADR-0011 (Operation
resource), ADR-0013 (validation strategy). Upstream baseline: Argo CD v3.5.5
(`applicationset/**`, `cmd/argocd-applicationset-controller`). Parity area:
`docs/parity/areas/06-applicationset.md`.

The ApplicationSet controller generates Applications from the generators and template of each
ApplicationSet, validates them against their AppProject, creates/updates/deletes them per the
effective policy, maintains the ApplicationSet status, and drives progressive syncs. It is an
independent implementation; no upstream code is imported or copied.

## Packages

| Package | Responsibility |
|---------|----------------|
| `internal/appset/template` | Templating engine: fasttemplate and Go `text/template` + Sprig (without `env`, `expandenv`, `getHostByName`) plus `normalize`, `slugify`, `toYaml`, `fromYaml`, `fromYamlArray`; `goTemplateOptions`; rendering of every string field and map key of the Application (JSON walk, raw JSON fields included); `templatePatch` (strategic merge, project immutable); matrix interpolation of the second child; generator-template merge (mergo semantics); evaluation limits for untrusted templates (see Security). |
| `internal/appset/generators` | List, Clusters, Git (directories/files), SCM Provider, Pull Request, Cluster Decision Resource, Plugin, Matrix, Merge; post-selector; `values`; requeue intervals; SCM credentials (`tokenRef`, `appSecretName` (control-plane ApplicationSets only), `providerRef`), allow-list, root CA, proxy, strict mode; parameter-set bound (`Config.MaxParameterSets`, default `DefaultMaxParameterSets` = 20 000). |
| `internal/appset` | Reconciler, validation, create/update/delete, preserved fields, ignore rules, status (conditions, health, resources, applicationStatus), progressive syncs (RollingSync), Reverse deletion, events, metrics, watches, `Run`; API helpers (`APIGenerator`, `ValidateForAPI`, `CheckInvalidGenerators`); webhook evaluator. |

## Public API

| Symbol | Purpose |
|--------|---------|
| `Config` | Controller configuration: `Namespace` (control plane), `ApplicationSetNamespaces` (names, globs, `/regexp/`), `Policy`, `EnablePolicyOverride` (`DefaultEnablePolicyOverride(configuredPolicy)` gives the upstream default), `EnableProgressiveSyncs`, `GlobalPreservedAnnotations/Labels`, `MaxResourcesStatusCount` (0 = unlimited; callers pass `DefaultMaxResourcesStatusCount`), `ConcurrentApplicationUpdates` (1..200), `ConcurrentReconciliations` (default 10), `DryRun`, `DisableInCluster`, `GlobalProjects`, `Generators` (`generators.Config`), `MetricsApplicationSetLabels`, `Registry`, `Logger`, `Now`. |
| `ParsePolicy`, `EffectivePolicy` | Controller policy parsing (upstream error text) and the effective policy (ApplicationSet `applicationsSync` only when set and override is enabled). |
| `NewNamespaceMatcher` → `Allowed`, `MultiNamespace`, `LiteralNamespaces` | ApplicationSet namespace selection (control namespace always allowed). |
| `NewReconciler(cfg, cachedClient, apiReader, recorder)`, `(*Reconciler).Reconcile`, `(*Reconciler).SetupWithManager(ctx, mgr)` | The controller. The cached client must carry `OwnerIndex` (`SetupWithManager` registers it). `recorder` is an `events.EventRecorder` (nil: no events). |
| `Run(ctx, RunConfig)` | Builds a manager (caches restricted to the needed types/namespaces, leader election `DefaultLeaderElectionID`), generator clients (Secrets/ConfigMaps read live, the rest cached; dynamic client), and runs until `ctx` is done. |
| `GenerateApplications(ctx, log, appSet, generators.Set)` | Rendering shared by the controller and the API: Applications in generator order, namespace forced to the ApplicationSet's, first error + reason (`ApplicationGenerationFromParamsError`/`RenderTemplateParamsError`). |
| `NewAPIGenerator(generators.Config).Generate(ctx, appSet)` | `POST /api/v1/applicationsets/generate`: no validation, no writes, tokenRef strict mode, no SCM proxy; errors carry captured generator logs (`unable to generate Applications of ApplicationSet: <err>\n<logs>`). |
| `ValidateForAPI`, `Projects`, `CheckInvalidGenerators`, `ErrTemplatedProject` | API admission checks (unrecognized generators recovered from `kubectl.kubernetes.io/last-applied-configuration`; templated project rejected in `spec.template` and in every generator-level template that applies). `Projects` returns every project the generated Applications can use (top-level template plus top-level generator templates, Matrix/Merge included), each of which the API server must authorize. |
| `NewEvaluator(generators.Config)` | Implements `webhook.GeneratorEvaluator` (Matrix webhook matching). |
| `ShouldRequeueForApplication`, `ShouldRequeueForApplicationSet` | Event filters (exported for reuse/tests). |
| `CalculateHealth`, `IsRollingSyncStrategy`, `RollingSyncEnabled`, `IsDeletionOrderReversed`, `NormalizeApplicationSpec` | Helpers. |
| `OwnerIndex`, `OwnerIndexFunc` | Field index of Applications by controlling ApplicationSet name. |
| Metrics | `patrol_appset_info{namespace,name,resource_update_status}`, `patrol_appset_owned_applications{namespace,name}`, `patrol_appset_reconcile{namespace,name}` (histogram, seconds), `patrol_appset_labels{namespace,name,label_<key>...}` (only when configured). Info/owned/labels are computed from the cache at scrape time, so deleted ApplicationSets leave no series. |

## Reconcile flow

1. Ignore ApplicationSets outside allowed namespaces. Get; NotFound drops the metric series.
2. Terminating: if the ApplicationSet has `cd.patrol.example/applicationset-finalizer` (or the
   Argo-style `resources-finalizer.cd.patrol.example`): when the effective policy forbids
   deletes, owner references to this ApplicationSet are removed from its Applications; else,
   with `preserveResourcesOnDeletion`, the resources finalizers of its Applications are removed;
   with `deletionOrder: Reverse` (RollingSync with steps) Applications assigned to steps are
   deleted one at a time from the last step (requeue 10 s; after 2 minutes terminating, an
   uncached read decides: gone → continue, still present → `application has not been deleted in
   over 2 minutes`). Then the finalizers are removed.
3. Add the finalizer when ordered deletion (progressive syncs enabled + Reverse) or
   `preserveResourcesOnDeletion` needs to act before garbage collection.
4. Log unrecognized generators. Generate; on error set `ErrorOccurred` (reason of the first
   error), `ParametersGenerated=False`, `ResourcesUpToDate=False`, requeue 3 minutes, no writes.
5. Validate every generated Application (see Security); lookup failures → validation error
   condition and requeue 3 minutes.
6. Progressive syncs (when enabled): clear `status.applicationStatus` unless RollingSync;
   otherwise update per-Application status, promote within `maxUpdate`, set
   `RolloutProgressing`. Disabled: clear any `status.applicationStatus`.
7. Validation errors → `ErrorOccurred`/`ApplicationValidationError` with the last message (by
   qualified name) and ` (and N more)`.
8. RollingSync: automated sync disabled (`automated.enabled=false`) on every desired
   Application, `cd.patrol.example/application-set-progressive-status` annotation set, sync
   requests collected for Pending Applications of eligible steps.
9. Create/update (policy allows update) or create only (Applications not already owned), sorted
   by name, bounded parallelism, deterministic first error (`UpdateApplicationError` /
   `CreateApplicationError`). Events `created`/`updated`.
10. RollingSync syncs (policy allows update): one `Operation` per Pending transition (see below).
11. Delete owned Applications absent from the full generated list (policy allows delete),
    releasing finalizers first when the destination is invalid or resources are preserved
    (`DeleteApplicationError`). Events `Updated`, `Deleted`.
12. `status.resources`/`resourcesCount` from owned Applications overlaid with this pass's writes
    (the cache may lag), truncated to `MaxResourcesStatusCount`.
13. Remove the refresh annotation (optimistic-lock patch; failure → `RefreshApplicationError`).
14. No validation error → `ResourcesUpToDate=True` (`ApplicationSetUpToDate`), `ErrorOccurred=False`.
    Requeue after the minimum generator interval (3 minutes when validation failed and no
    generator polls; earlier when a rejected Operation must be retried).

Status writes are JSON merge patches of the status subresource (the controller is the only
writer); conditions keep `lastTransitionTime` while type/status/reason/message are unchanged,
carry `observedGeneration`, and `status.health` is recomputed with every condition write.

## Progressive syncs (RollingSync)

- Steps: `matchExpressions` ANDed over generated Application labels; present key → `In`/`NotIn`
  (other operators never match); absent key → `In` fails, others skipped; empty list selects all.
  First matching step wins; Applications in no step get step `-1` and are never synced by the
  controller.
- Status lifecycle per upstream (`Waiting` → `Pending` → `Progressing` → `Healthy`, revision or
  spec change → `Waiting`, `Waiting` + `Synced` → `Healthy`/`Progressing`, `InvalidSpecError`/
  `UnknownError` moves `Pending` → `Progressing`), messages identical to upstream.
- `Pending` → `Progressing` recognizes the RollingSync Operation by identity:
  `status.operationState.operationName` equal to the deterministic name below (any phase but
  `Rejected`), without timestamp or `reconciledAt` conditions. Timestamps are stored with second
  precision, so upstream's `startedAt` after the `Pending` transition never holds for an
  Operation started in the same second (the rollout stalled). Other operations keep the upstream
  rule (`startedAt` strictly after the transition, `reconciledAt` after it).
- `maxUpdate`: int or percent of the step's entries (rounded down, at least 1 unless `0%`;
  invalid → 1); `0` blocks the step (manual syncs only).
- Syncs are `Operation` objects (ADR-0011), not `Application.operation`: name
  `<app>-rollingsync-<sha256(appset UID, app, Pending time)[:10]>` (idempotent across reconciles),
  owned by the Application, label `cd.patrol.example/application`, `initiatedBy
  {username: applicationset-controller, automated: true}`, info `Reason: ApplicationSet
  RollingSync triggered a sync of this Application resource`, `sync.prune` = the template's
  automated prune (only when the Application has a sync policy), `sync.syncOptions` = the
  Application's, `retry` = the Application's or `{limit: 5}`. An Operation `Rejected` because
  another Operation was in progress is deleted and recreated on a later pass.

## Security

- The Git generator scopes repository credentials and source integrity with the project of the
  generator entry's effective template (the generator-level template's `spec.project` when set,
  for a Matrix/Merge child its parent's; else `spec.template.spec.project`), i.e. the project its
  Applications land in. That project must exist and, for ApplicationSets outside the
  control-plane namespace, list the ApplicationSet namespace in `spec.sourceNamespaces`;
  otherwise generation fails before any repository is read. A templated project uses global
  credentials only and skips verification (upstream).
- `appSecretName` (SCM Provider and Pull Request generators) names a GitHub App Secret of the
  control-plane namespace: only ApplicationSets in the control-plane namespace may use it;
  tenants use `providerRef` (SCMProvider and Secrets in their own namespace).
- Every generated Application is validated before any write: unique name; the project exists
  in the control-plane namespace and, for ApplicationSets outside it, lists the ApplicationSet
  namespace in `spec.sourceNamespaces` (so a templated `project` cannot escalate into a project
  that does not trust the tenant namespace); the destination resolves to a registered cluster
  (or in-cluster unless disabled) and is permitted by the project's destinations (effective
  project: scoped clusters/repositories, global projects).
- Generated Applications always live in the ApplicationSet namespace; `templatePatch` cannot
  change `spec.project`.
- With other namespaces enabled, SCM providers require an allow-list (startup error otherwise,
  upstream message). Secrets for `tokenRef`/`caRef` are read from the ApplicationSet namespace;
  GitHub App Secrets, plugin and cluster decision ConfigMaps, Clusters and AppProjects from the
  control-plane namespace. `Run` reads Secrets and ConfigMaps live instead of caching every
  Secret of the cluster.
- RollingSync Operations are automated and name an initiator, so the controller's service
  account needs the `impersonate` verb on `operations.cd.patrol.example`
  (`v1alpha1.OperationInitiatorVerb`).
- Untrusted input bounds: rendered strings ≤ 1 MiB and ≤ 4 MiB per evaluation (one generated
  Application, one generator interpolation), streamed into a bounded buffer by both engines;
  Go templates run with evaluation limits: ≤ 1 000 000 steps per evaluation (template
  invocations and range iterations charge their node count, function calls their arguments,
  key generation and password hashing in bulk), every function that builds, formats or
  serializes a value checks an estimate (shared references, escaping, indentation, printf
  widths) against 8 MiB before allocating, `until`/`untilStep`/`seq` ≤ 10 000 elements with
  bounds ≤ 2^53, split functions ≤ 10 000 pieces, value nesting ≤ 200, `set`/`merge*` refuse to
  create cyclic values (printing one overflows the stack); ≤ 20 000 parameter sets per generator
  (a Matrix product is checked before it is built); plugin responses ≤ 16 MiB; cluster decision
  resources read in pages of 250, more than 1000 matching fail the generation (a partial list
  would delete Applications); last-applied annotation parsed only up to 1 MiB, condition messages
  ≤ 32 KiB, generator logs captured by the API generator ≤ 16 KiB. Secrets are never logged.

## Deliberate differences and upstream discrepancies

| Area | Upstream | Patrol |
|------|----------|--------|
| RollingSync trigger | sets `Application.operation` | creates an `Operation` (ADR-0011); rejected Operations are retried |
| Generated Application validation | project exists, destination resolves | also project destinations and, outside the control plane, `sourceNamespaces` (stricter; tenant boundary) |
| Git generator credential/integrity scope | `spec.template.spec.project`, existence only | project of the entry's effective template, which must permit the ApplicationSet namespace (tenant boundary) |
| `appSecretName` | any ApplicationSet may name a control-plane Secret | control-plane ApplicationSets only |
| RollingSync `Pending` → `Progressing` | `startedAt` after the transition (stalls within the same second) | the RollingSync Operation by name; upstream rule for other operations |
| Template evaluation | output unbounded; sprig functions unbounded | evaluation limits (Security); hostile templates fail with `template exceeds an evaluation limit` |
| Parameter sets | unbounded (Matrix N×M) | ≤ `MaxParameterSets` per generator |
| Cluster decision resources | single unbounded list | paged; more than 1000 fail the generation |
| API project checks | `spec.template.spec.project` only | also generator-level templates (`Projects`) |
| Duplicate generated names | code drops every Application with the name (docs say the first wins) | first occurrence applied, later ones reported (parity inventory semantics) |
| ApplicationSet finalizer | `resources-finalizer.argocd.argoproj.io`, added only for Reverse deletion | `cd.patrol.example/applicationset-finalizer` (the migrated `resources-finalizer.cd.patrol.example` is honored); also added for `preserveResourcesOnDeletion`, whose resources finalizers are stripped from Applications on ApplicationSet deletion and before pruning an Application |
| Releasing Applications (non-delete policy) | removes every owner reference | removes only references to this ApplicationSet |
| Cache staleness | cache-syncing client writes into the informer | writes overlaid on the cached list for status; `AlreadyExists` on create falls back to an uncached read and update; Reverse deletion trusts an uncached NotFound |
| `status.applicationStatus` writes | only on status/step/message changes | also on `targetRevisions` changes (otherwise re-detected forever) |
| Status writes, refresh removal | full updates with conflict retries | merge patches (status) and an optimistic-lock patch (annotation) |
| `status.resources[].group/version/kind` | from cached TypeMeta (may be empty) | always `cd.patrol.example`/`v1alpha1`/`Application` |
| Namespace patterns | invalid regexp logged, never matches | invalid pattern is a startup error; RE2 has no look-around (the upstream doc example `/^((?!not-allowed).)*$/` cannot compile in either) |
| Unrecognized generator names | first key of a random map iteration | smallest key |
| `requeueAfterSeconds` < 0 | immediate requeue loop | no time-based requeue |
| Metrics | `argocd_appset_*` | `patrol_appset_*`, same labels |
| Events | `record` API, reasons `created`/`updated`/`Deleted`/`Updated` | `events.k8s.io` API, same reasons, actions `Create`/`Update`/`Delete` |
| Plugin token Secret | `argocd-secret` | `patrol-secret` (`$<secret>:<key>` supported) |
| Cluster generator selectors | Secret labels incl. `secret-type=cluster` | Cluster labels plus a synthetic `cd.patrol.example/secret-type=cluster`, so migrated selectors keep matching |
| `slugify` | unidecode transliteration | Latin-1/Latin Extended-A transliteration, other non-ASCII → `-` |

## Failure modes

| Condition | Behavior |
|-----------|----------|
| Generator/render error | `ErrorOccurred` + reason, requeue 3 m, no create/update/delete, refresh annotation kept |
| Project/cluster lookup failure | `ErrorOccurred`/`ApplicationValidationError`, requeue 3 m |
| Invalid Applications | valid ones applied; existing invalid ones not deleted; requeue 3 m if nothing polls |
| Invalid `ignoreApplicationDifferences` jq | `failed to build ignore diff config: ...`, `UpdateApplicationError`, error returned (backoff) |
| Application owned by another ApplicationSet | `AlreadyOwnedError`, `UpdateApplicationError`, other Applications still applied |
| Write errors | per-Application, first by name reported; context cancellation aborts |
| Operation create error | `SyncApplicationError`, error returned |
| Reverse deletion stuck > 2 m | error (exponential backoff) |
| Panic in a reconcile | recovered, logged with stack, returned as error |

## Parity feature areas served

Recorded in `docs/parity/status/appset.yaml` (ASET-002..099, 101..103, 108, 114, 128, 130, 132).
API, CLI, UI and webhook features (ASET-100, 104..107, 109..127) belong to other components;
this package provides `APIGenerator`, `ValidateForAPI` and `Evaluator` for them.

## Known gaps

- Not wired: `cmd/patrolcd appset-controller` (flags/env/cmd-params → `RunConfig`, metrics and
  probe servers, webhook server), API server endpoints, webhook evaluator registration.
- GitHub API metrics (ASET-129): the generators accept an `scm.RequestObserver` factory
  (`SCMConfig.Observer`); no Prometheus implementation is provided here.
- The Git generator's repo-server connection (TLS/mTLS/timeout, ASET-131) and new file
  globbing (ASET-022) are the caller's/repo-server's responsibility; the generator passes the
  flags through.
- Progressive sync detection relies on the application controller populating
  `status.operationState.startedAt`, `status.reconciledAt`, sync/health status and honoring
  `automated.enabled=false`; that controller is not built yet, so progression is verified with
  simulated Application status only.
- No garbage collector in envtest: cascade deletion of Applications through owner references
  is not exercised.

## Requests to other owners

- `cmd/patrolcd`: add the `appset-controller` subcommand mapping the
  `applicationsetcontroller.*` parameters (`internal/settings`) to `RunConfig`; compute
  `EnablePolicyOverride` with `DefaultEnablePolicyOverride` when the parameter is unset; register
  `Config.Registry` on the metrics server.
- Deploy/RBAC: applicationsets (+status, finalizers), applications, operations (create, get,
  delete, `impersonate`), clusters/appprojects/repositories/scmproviders (read), secrets and
  configmaps (get) in ApplicationSet namespaces and the control plane, events (create), leases.
- `internal/apiserver`: `ValidateForAPI` + `NewAPIGenerator(...).Generate` for
  `POST /applicationsets/generate` and create dry-run; authorize the ApplicationSet action on
  every project returned by `Projects` (not only `spec.template.spec.project`).
- `internal/webhook`: `appset.NewEvaluator` as `Config.GeneratorEvaluator`.

## Verification

```bash
go build ./...
go vet ./internal/appset/...
golangci-lint run ./internal/appset/...
KUBEBUILDER_ASSETS=$PWD/bin/envtest/k8s/1.36.0-darwin-arm64 go test -race ./internal/appset/...
go run ./hack/parity
```

Unit tests use the controller-runtime fake client (owner index, status subresource, UIDs on
create) and the `pkg/scm/fake` provider; generator tests use an in-memory repo service, the
dynamic fake client and `httptest` plugin servers. The envtest tests (`TestEnvtest*`) run the
real `Run` entry point against one kube-apiserver with the generated CRDs and simulate the
application controller by writing Application status; they skip without `KUBEBUILDER_ASSETS`
(fail with `PATROL_REQUIRE_ENVTEST=1`).
