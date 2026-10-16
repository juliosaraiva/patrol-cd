# Component: `internal/compare`

Status: implemented (library; the Application reconciler, the tree API and the API server do
not call it yet). Decision records: ADR-0004 (no Redis: the controller owns the live graph and
serves trees and diffs), ADR-0001 (independent implementation).

`internal/compare` is the application state comparison engine. It re-implements the observable
behavior of Argo CD v3.5.5 `controller/state.go` (`CompareAppState`, `GetRepoObjs`,
`NormalizeTargetObjects`), `controller/health.go`, `controller/cache` (resource info,
attribution) and the tree assembly of `controller/appcontroller.go` (`getResourceTree`,
`getAppHosts`, `hideSecretData`), plus the comparison helpers of `util/argo`. No upstream code
is copied.

## Responsibilities

- Desired state: one repo-server `GenerateManifest` call per source with resolved repository
  credentials, Helm/OCI dependency repositories and credential templates, kustomize and Helm
  settings, enabled source types, tracking method/instance label/installation ID, destination
  Kubernetes version and API versions, `$ref` sources (multi-source), the project's source
  integrity policy and the manifest-generate-paths annotation. Ref-only sources contribute
  only their revision. Hydrator Applications compare against the hydrated sync source. Local
  manifests replace rendering (refused when the project enforces source integrity).
- Revision change evaluation for automated sync (`RevisionsMayHaveChanges`), with
  `UpdateRevisionForPaths` when manifest-generate-paths is set.
- Target normalization: namespace stripping/defaulting from cluster discovery, tracking
  re-applied to changed objects, de-duplication (last wins, `RepeatedResourceWarning`),
  resource exclusions (`ExcludedResourceWarning`), managed namespace synthesis
  (`managedNamespaceMetadata`).
- Live state from `internal/cache` (`GetManagedLiveObjs` with tracking attribution), project
  filtering (`IsLiveResourcePermitted`), shared resource detection, pairing (hooks split,
  duplicate UIDs across API groups removed, unknown scopes checked both ways).
- Diff through `internal/diff` with the Application's `ignoreDifferences`, system overrides,
  `knownTypeFields`, `ignoreAggregatedRoles`, tracking normalization, structured merge for
  `ServerSideApply=true`, and server-side diff (controller default overridden by the
  compare-options annotation) through a `DryRunner`.
- Resource and application sync status, requiresPruning, deletion confirmation, sync waves,
  application health (`internal/health` + `internal/lua`), persisted vs tree health, source
  integrity conditions, source types, bounded `status.resources`.
- Resource info population for the live-state cache (`PopulateResourceInfo`): pod status,
  containers, restarts, requests, images; Service/Ingress/Gateway API/Istio networking info;
  link annotations; revisions; custom labels; node info; child Application links; health;
  ignoreResourceUpdates hashes; attribution of root resources.
- Resource tree (`ApplicationTree`), orphaned resources, hosts and `status.summary`.
- Secret masking of managed resources for caching/serving.

Out of scope: writing status, conditions or events (controller), refresh queues and comparison
levels, the diff cache, automated sync decisions, the tree/diff HTTP API, sharding, cluster
cache invalidation on credential changes.

## Public API

| Symbol | Purpose |
|--------|---------|
| `New(Deps) (*Manager, error)` | `Deps{RepoServer, Repositories, Settings, Clusters, LiveState, ServerSideDiff?, ApplicationExists?, ControlPlaneNamespace, ServerSideDiffEnabled, PersistResourceHealth, RepoErrorGracePeriod, MaxResources, GPGEnabled, JQTimeout, Registerer, Logger, Now}`. |
| `(*Manager).Compare(ctx, Input) (*Result, error)` | One comparison. `Input{App, Project (effective), Revisions, Sources, NoCache, NoRevisionCache, LocalManifests, HasMultipleSources, RollbackOrSyncOverride, LastSyncResult, SkipTree}`. |
| `(*Manager).ResourceTree(ctx, TreeInput) (*TreeResult, error)` | Tree-only refresh from the managed resources of the last comparison. |
| `Result` | `SyncStatus` (status, comparedTo, revision(s), shortRevision), `Health`, `ResourceHealthSource`, `Resources` (sorted, bounded) + `ResourcesTruncated`, `Managed []ManagedResource`, `Conditions []Condition`, `EvaluatedConditionTypes`, `SourceType(s)`, `Revisions` (resolved), `RevisionsMayHaveChanges`, `HasPre/PostDeleteHooks`, `ReconciledAt`, `Timings`, `Summary`, `Tree`, `TreeError`, `OrphanedResources`, `Cluster`, `Targets`, `Live`, `DiffResults`, `Normalizer`, `ServerSideDiff`, `ManifestResponses`, `FailedToLoad`; `MaskSecrets(ctx, hideAnnotations)`. |
| `ManagedResource` | `Key, Version, Target, Live, Diff, Hook, SelfReferenced, RequiresPruning, RequiresDeletionConfirmation, SyncWave, Status, Health, ResourceVersion`. |
| `MergeConditions(existing, found, evaluated, generation, now)` | Folds the (possibly repeated) conditions into the one-per-type `metav1.Condition` list. |
| `ApplicationTree`, `ResourceNode`, `ResourceRef`, `InfoItem`, `NetworkingInfo`, `LoadBalancerIngress`, `HostInfo`, `HostResourceInfo` | Tree API types (upstream JSON field names). |
| `LiveStateProvider`, `LiveStateCache`, `NewLiveStateCache(LiveStateCacheOptions{Manager, Config, Settings, ControlPlaneNamespace, Base, Logger})`, `CacheManager` (`*cache.Manager` implements it), `ClusterKey`, `CacheSettings(...)`, `(*LiveStateCache).Forget(cluster)` | Per-cluster caches with this package's info populator; reconfigured in place when cache-relevant settings or the cluster's namespaces change (invariant 15). |
| `PopulateResourceInfo(InfoConfig)`, `ResourceInfo`, `InfoOf`, `AppOf`, `SkipResourceUpdate`, `PodInfo`, `NodeInfo` | Cache integration for the controller's update handler (attribution, refresh suppression). |
| `ServerSideDiffProvider`, `KubeServerSideDiff`, `NewKubeServerSideDiff(config, live, ttl)` | SSA dry runs (`dryRun=All`, force, raw config, no impersonation) and OpenAPI v2 type parsers per cluster (cached, TTL). |
| `RepositoryResolver`, `ClusterResolver`, `SettingsSource`/`StaticSettings` | Dependencies (`*repository.Resolver`, `*clusters.Registry`, `*settings.Manager` implement them). |
| `NewNormalizer(settings, app, jqTimeout, log)`, `AppIgnoreRules` | The Application's diff normalizer (also exposed as `Result.Normalizer`). |
| Errors | `ErrRepoErrorDeferred` (first repo error within the grace period: keep the old status), `ErrInvalidInput`. |
| Constants | Phase names `Phase*`, info item names `Info*`, `ConditionResourcesTruncated`/`ReasonResourcesTruncated`, `DefaultMaxResources` (5000), `DefaultSchemaTTL`. |

### Wiring by the controller (expected use)

1. Build one `cache.Manager` (register `OnResourceUpdated` through `OnCacheCreated`; use
   `InfoOf`, `AppOf`, `SkipResourceUpdate` for refresh triggering), a `LiveStateCache` over it
   (`Config` = `Registry.RestConfig` bound to the controller options; `Base` = tuning knobs,
   `ClusterResyncPeriod: 12h`, `RetainManagedFields: true` when managedFieldsManagers rules or
   server-side diff are used) and a `KubeServerSideDiff` (`Config` = `Registry.RawRestConfig`).
   Call `cache.Manager.Invalidate` + `LiveStateCache.Forget` when a cluster's credentials or
   server change or `refreshRequestedAt` is newer than the last sync.
2. Resolve the effective project (`project.ResolveAppProject`) and call `Compare` with
   `NoCache`/`NoRevisionCache` from the refresh type. On `ErrRepoErrorDeferred` keep the
   previous status. Pass `LastSyncResult` from the Operation named by
   `status.operationState` (the Application status summary has no managed namespace metadata).
3. Persist `SyncStatus`, `Health` (+ lastTransitionTime), `Resources`, `ResourceHealthSource`,
   `SourceType(s)`, `ReconciledAt`, `Summary` (only when `Tree != nil`), and conditions with
   `MergeConditions(existing, specConditions+Result.Conditions, Result.EvaluatedConditionTypes
   + [InvalidSpecError, UnknownError], generation, now)`.
4. Serve `Result.Tree` and `Result.MaskSecrets(...)` from the tree API; use
   `ResourceTree` for tree-only refreshes. Pass `Targets`, `Live`, `DiffResults` and
   `Normalizer` to `sync.Input`.

## Behavior and invariants

1. **Initial status** is `Unknown` with comparedTo (destination, ignoreDifferences, source or
   sources) and the requested revisions (blank entries take `targetRevision`). Missing settings
   return that status without error (upstream).
2. **Failures load as much as possible**: manifest, live-state, normalization, project-check and
   diff errors become `ComparisonError` conditions and set every resource and the app to
   `Unknown` (`FailedToLoad`). Messages follow upstream (`Failed to load target state: failed to
   generate manifest for source <i> of <n>: ...`, `Failed to load live state: ...`, `Failed to
   compare desired state to live state: ...`). Manifest errors use reason
   `ManifestGenerationFailed`, others `ComparisonFailed`.
3. **Repo error grace period**: the first manifest error of an Application, and later ones
   within `RepoErrorGracePeriod` of it, return `ErrRepoErrorDeferred` unless
   `NoRevisionCache` or `RollbackOrSyncOverride`; success clears the record (bounded to 10,000
   entries). Keyed by namespace/name/UID.
4. **Target normalization** (DIFF-029): cluster-scoped kinds lose their namespace; namespaced
   and unknown kinds get the destination namespace; tracking is re-applied to modified
   objects; `generateName`-only objects are keyed `<generateName><index>`; the last of
   repeated keys wins with `Resource <key> appeared <n> times among application resources.`;
   output keeps first-appearance order.
5. **Live set**: cached roots tracked by the instance name plus a GET-backed lookup of every
   target; objects the project does not permit are dropped; `SharedResourceWarning` for live
   objects tracked by another instance; a live managed namespace without a manifest gets a
   synthesized target (labels/annotations + `ServerSideApply=true`) so it is not pruned.
6. **Pairing**: PreDelete/PostDelete-only hooks removed; sync hooks and hooks without a valid
   type are not compared; live objects sharing a UID across API groups keep the one in Git (or
   the smallest key); unpaired live objects follow the targets sorted by key.
7. **Status rules** (DIFF-030): hooks, ignored hooks, `Skip` targets and live objects with a
   copied tracking id get no status; otherwise modified/missing/extraneous is `OutOfSync`
   (extraneous objects with `IgnoreExtraneous` do not affect the app), else `Synced`; kinds the
   project denies are `Unknown`; failures make everything `Unknown`; changed managed namespace
   metadata makes the app `OutOfSync`. `requiresPruning` = extraneous and self-referenced.
   A namespaced object without namespace adds `InvalidSpecError`.
8. **Health** (DIFF-062..064): skip-hooks, live hooks and ignored hooks excluded;
   ignore-healthcheck and self-referencing Applications neither recorded nor aggregated;
   Missing recorded but not aggregated; all-missing → `Missing`; child Applications that are
   Missing/Unknown ignored; the first assessment error is a `ComparisonError` (`error setting
   app health: ...`). Per-resource health is written to `status.resources` only with
   `PersistResourceHealth` (`resourceHealthSource` inline vs `appTree`).
9. **Bounded resources**: sorted by `group/kind/namespace/name`; above `MaxResources`
   resources needing attention (not Synced, requiring pruning, not Healthy) are kept first and
   `ResourcesTruncatedWarning` reports the cut. `Managed` is never truncated.
10. **Tree** (DIFF-080): missing resources are `Missing` nodes without UID; live ones are
    traversed with descendants, pruning what the project denies; parent refs carry the child's
    namespace when the parent is found there; nodes, orphans and hosts are sorted. Orphans
    (project `orphanedResources` set): namespaced top-level resources of the destination
    namespace, not managed, permitted, not excepted (built-in and `ignore` rules), skipping
    those attributed to an existing Application; `OrphanedResourceWarning` (`Application has
    <n> orphaned resources`) when `warn` is true.
11. **Hosts** (DIFF-098): nodes running app pods with cpu/memory/storage capacity, app and
    neighbor requests (terminal neighbor pods excluded) in milli-units and only
    `application.allowedNodeLabels`.
12. **Summary** (DIFF-100): unique sorted URLs of managed nodes plus the Application's own link
    annotations (unvalidated), unique sorted images, `isAppOfApps` for child Patrol
    Applications.
13. **Determinism**: conditions, resources, tree nodes and orphan traversal are ordered; no map
    iteration order reaches results.
14. Inputs (`Application`, `AppProject`) are never modified; results own their objects.
15. **Live-state cache settings** (`LiveStateCache`): each cluster key remembers the cache it
    verified and the signature of the settings it runs with (resource filter, cluster
    namespaces and clusterResources, tracking label/method/installation ID, Lua overrides,
    customLabels, respectRBAC, ignoreResourceUpdates). A call whose settings snapshot differs
    takes the key's transition lock (waiting is bounded by its context), re-reads the
    settings, applies them to the verified cache in place (`ClusterCache.Invalidate` with
    `WithSettings`, as upstream's settings watch does) and records them, then syncs. The change
    is therefore applied before any sync and never depends on a sync with the old settings;
    a call that fails afterwards (sync error, cancelled context) does not lose it, and no
    caller with the new snapshot receives the cache before it was applied. Until a first
    `Get` succeeds the key is unverified: the settings passed to the cache being built are
    remembered, and a change discards that cache (`Manager.Invalidate`) so the next one is
    built with the new settings. A cache the Manager replaced behind the key's back (an
    invalidation without `Forget`) gets the settings applied in place before it is returned.
    `Forget` resets the key; a transition in flight when it runs does not record its outcome.

## Failure modes

| Condition | Result |
|-----------|--------|
| Invalid input, unresolvable destination | error, no Result |
| `ctx` ended | error wrapping `ctx.Err()`, no Result (never a half-computed status) |
| Repo error within grace period | `ErrRepoErrorDeferred` |
| Repo-server / repository / cluster cache unavailable | `ComparisonError`, sync `Unknown`, resources `Unknown` |
| Invalid jq ignore rule | `ComparisonError` `Failed to compare desired state to live state: failed to create diff normalizer: ...` |
| Unsupported knownTypeFields | warning log, comparison continues |
| Type parser / dry runner unavailable | `UnknownError` condition; server-side diff then fails per object |
| Health assessment error | `ComparisonError` `error setting app health: ...`; Unknown health aggregated |
| Source integrity check problems | `ComparisonError` per response, one `<check>: <problem>` line each |
| Tree failure | `TreeError` set and logged; comparison still valid; `Summary` left zero |
| More than 100,000 manifests | manifest error |
| Cache sync fails on the call that observes a settings change | error (comparison `Unknown`); the new settings are already applied in place, the next call syncs with them |
| Context ends while another call applies a settings change | error wrapping `ctx.Err()`; nothing recorded, the next call applies the change |

## Security notes

- Repository credentials stay in `apiclient.Repository` (logged only through its redacting
  `LogValue`); nothing in this package logs objects, manifests or secret values. Condition
  messages carry repo-server and API errors (credential-free by contract).
- Live content is untrusted: tracking metadata decides attribution but the project's
  destination and kind restrictions filter the live set and the tree; copied tracking ids are
  never pruned. Owner-chain attribution is cycle-safe.
- Server-side dry runs use `dryRun=All` and never fall back to real applies; they run with the
  cluster's raw credentials (no impersonation), as upstream.
- `Result.Managed` holds raw Secret data from the cluster and Git; serve only
  `MaskSecrets(...)` output. Link annotation URLs are validated (http/https only) before they
  reach the tree.
- Bounded: manifests per Application (100,000), `status.resources` (`MaxResources`), merged
  condition messages (20 entries, 32 KiB), repo error records (10,000).

## Parity feature areas served

`docs/parity/status/compare.yaml` lists the IDs: comparison (DIFF-028, 029, 030, 070, 076, 077),
health (DIFF-062, 063, 064, 066), orphans (DIFF-078, 079), tree and info (DIFF-080, 087,
089..100), refresh suppression inputs (DIFF-033), secret masking (DIFF-031), server-side diff
wiring (DIFF-009), and Application status features (APP-012, 014, 015, 016, 022, 027..031,
034, 038..041, 043, 075, 077, 086, 090, 091, 093, 100..102).

## Known gaps and deliberate differences

- **No diff cache** (DIFF-011): every comparison recomputes diffs (ADR-0004: no Redis).
- **Normalization errors fail the comparison**: upstream keeps comparing with no targets after a
  tracking failure (everything looks extraneous); Patrol reports `Unknown`.
- **Unknown cluster scope without a cache**: upstream treats every kind as cluster-scoped when
  the cluster cache is unavailable; Patrol treats it as unknown (namespaced), which only matters
  for targets of a comparison that is `Unknown` anyway.
- **Revisions on failure**: upstream clears `status.sync.revisions` of multi-source
  Applications when manifest generation fails; Patrol keeps the requested revisions.
- **Repo error grace bookkeeping** is keyed by namespace/name/UID (upstream: name only), and a
  grace period of 0 disables deferral (upstream defers the first error even then).
- **Legacy `verifyResult`** interpretation (repo-servers predating source integrity) is not
  implemented: the Patrol repo-server always returns `sourceIntegrityResult`.
- **Pod requests** are computed here (kubelet/scheduler semantics: max of spec and resized
  status requests, sidecars, init containers, pod-level requests for cpu/memory, overhead)
  because `k8s.io/component-helpers` is not a module dependency.
- **One condition per type**: Argo CD lists repeated conditions; `MergeConditions` joins their
  messages with `; `.
- **Truncation condition**: `ResourcesTruncatedWarning` is a Patrol addition not yet declared
  in `api/v1alpha1` (requested).
- **`Application` status summary has no managed namespace metadata**: callers pass the
  Operation's `syncResult` as `LastSyncResult`.
- **Settings changes reach a cluster cache lazily**: upstream's settings watch invalidates
  every cluster cache as soon as the settings change; `LiveStateCache` applies the change on
  the next `ClusterCache` call for that cluster (every comparison makes one). The first call
  for a key (and the first after `Forget`) calls `Manager.Invalidate` so that a cache of
  unknown settings is not adopted. Per-key bookkeeping (a few words) is kept for every
  cluster key ever used.

## Verification

Run on 2026-10-16 (darwin/arm64, Go 1.27.2, envtest kube-apiserver 1.36.0):

| Command | Result |
|---------|--------|
| `go build ./...` | exit 0 |
| `go vet ./internal/compare/` | exit 0 |
| `golangci-lint run ./internal/compare/...` | `0 issues.` |
| `go test -race -count=1 ./internal/compare/` (no envtest) | `ok` (envtest tests skipped) |
| `KUBEBUILDER_ASSETS=bin/envtest/k8s/1.36.0-darwin-arm64 go test -race -count=1 -v ./internal/compare/` | `ok ... 7.1s`: 54 tests and 33 subtests passed, none skipped |
| same with `-count=3` | `ok` (flakiness check) |
| `go test -race -count=20 -run 'TestLiveStateCache\|TestSameCache' ./internal/compare/` | `ok` |
| `go run ./hack/parity` | exit 0 |

Live-state cache settings (review fix): the new `livestate_test.go` tests were also run, through
`go test -overlay`, against the previous `ClusterCache` (signature recorded before the cache
was reconfigured). The fake-Manager tests failed (change lost after a failed call, concurrent
callers given the cache before the change, change blocked by a failing old-settings sync,
unverified cache adopted after a change, rebuilt cache adopted, Forget race), and
`TestEnvSettingsChangeAppliedAfterFailedCall/cache_sync_error` failed on every run (the
`cancelled_context` case fails only when `cache.Manager.Get` notices the cancelled context,
which its `select` decides at random). All pass with the fix.

Unit tests: info population (pods: kubectl reason table incl. init containers, sidecars,
signals, terminating, NodeLost, scheduling gates; requests incl. sidecars, init, resize,
overhead; Service, Ingress incl. the TLS hosts quirk and ignore-default-links, Gateway, routes,
Istio, link validation, revisions, custom labels, managed-by-url), attribution and update
hashing, target normalization, pairing, condition merging (rapid property test of the message
bound), resource bounding, health aggregation, deletion confirmation, managed namespace,
refresh paths, short revisions, grace period, summary.

envtest tests (real kube-apiserver, real `internal/cache`, `internal/diff`, `internal/tracking`,
`internal/health`, `internal/project`, `internal/settings`; fake repo-server returning canned
manifests with tracking stamped like the real one): Missing → Synced → OutOfSync → extraneous
with requiresPruning → IgnoreExtraneous; IgnoreExtraneous does not hide modifications;
multi-source last-wins with `RepeatedResourceWarning`, ref-only source revision and ref targets
sent to the repo-server, revision overrides; ignoreDifferences (JSON pointer, jq, name scoping,
invalid jq); `SharedResourceWarning` across two Applications; copied tracking ids; orphaned
resources with warn/ignore/other Applications/monitoring off; exclusions; namespace defaulting
and stripping, missing destination namespace; hooks excluded from health, regular failed pod
degrades, ignore-healthcheck; repo error → `ComparisonError` + `Unknown` and recovery; grace
period; local manifests and the source integrity refusal; project-denied kinds `Unknown`;
managed namespace metadata (synthesized target, change since last sync); tree with
Deployment → ReplicaSet → Pod, hosts with allowed node labels, summary, tree-only rebuild;
server-side diff with real dry runs (Synced, drift) and the missing-provider condition; Secret
masking; truncation; hydrator sync source and the HydrateTo waiting message; cache invalidation
on settings change, also when the first call after the change fails (sync error, cancelled
context). Unit tests with a fake `CacheManager` cover the settings transitions: failed and
cancelled calls, a cache that never syncs with the old settings, concurrent callers,
unverified caches, caches rebuilt behind the key's back, and `Forget` during a transition.
