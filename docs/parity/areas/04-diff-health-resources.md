# Comparison, Health & Resource Selection — parity inventory (Argo CD v3.5.5)

Scope: everything that decides *whether* a live cluster object matches its desired manifest, *how healthy* it is, and *which* cluster objects Argo CD sees and attributes to an Application. In scope: the diff engine (legacy 3-way diff with `last-applied-configuration`, two-way fallback, structured-merge diff, server-side diff and mutation-webhook removal), all diff normalizations (built-in kinds, `knownTypeFields`, app-level and system-level `ignoreDifferences` with `jsonPointers` / `jqPathExpressions` / `managedFieldsManagers`, `resource.compareoptions`), `ignoreResourceUpdates` refresh suppression, the `argocd.argoproj.io/compare-options` annotation (`IgnoreExtraneous`, `ServerSideDiff`, `IncludeMutationWebhook`), secret masking, resource/app sync-status computation, every built-in Go health check, Lua health customizations (argocd-cm overrides, wildcards, sandbox) and the full embedded `resource_customizations` inventory, aggregate app health, persisted vs tree health, resource exclusions/inclusions/`respectRBAC`, API discovery, managed-object selection, orphaned resources, the resource tree (ownerReferences and synthesized relations), node info enrichment (pods, services, ingresses, Gateway API, Istio, links, hosts), the ManagedResources / ServerSideDiff / ResourceTree APIs, `argocd app diff`, `argocd app resources`, `argocd admin settings resource-overrides` and the UI diff/orphan views.
Out of scope (covered by other areas): sync execution and sync options (`ServerSideApply`, `RespectIgnoreDifferences`, `Prune`, `Delete`), hooks/waves execution, resource actions execution (only inventoried here), resource tracking methods (only the diff-time normalization is here), repo-server manifest generation, cluster credential management, sharding, notifications.

Notation: all upstream links are permalinks to commit `a5038b0e2bbac1715754380ae0084df8e3d5b48c`. "Patrol" is the re-implementation under test.

## Diff engine

### DIFF-001 Diff pipeline and result model
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/diff/diff.go#L300, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/diff/diff.go#L414, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/diff.go#L76, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/diff.go#L881, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/reconcile.go#L71, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L918, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/diff-strategies.md#L1
- **Maturity**: GA
- **Semantics**: Comparison pairs target and live objects by resource key (group, kind, namespace, name). Pairing (`sync.Reconcile`) drops ignored hook objects, splits hooks out of the target list, de-duplicates live objects sharing a UID across API groups (keeping the one present in Git, otherwise one arbitrary survivor), and for each target looks up the live object under the namespaced key, the cluster-scoped key, or both when scope is unknown; remaining live objects are appended with a nil target (extraneous). For each pair `StateDiffs` runs: (1) `preDiffNormalize` on deep copies — tracking-metadata normalization (DIFF-026) and, when both sides exist and an ignore rule with `managedFieldsManagers` matches, managed-fields normalization (DIFF-021); (2) builds a composite normalizer = ignore-differences normalizer followed by known-types normalizer; (3) `diff.Diff` per pair, choosing server-side diff (DIFF-009), structured-merge diff (DIFF-008) or the legacy 3-way/2-way diff (DIFF-002/003). Each pair yields `DiffResult{Modified, NormalizedLive, PredictedLive}` where `Modified = !bytes.Equal(NormalizedLive, PredictedLive)` (JSON bytes). Inputs are never mutated.
- **Edge cases**: `DiffArray` errors `left and right arrays have mismatched lengths` if target/live counts differ; diff-config validation errors (`diffConfig validation error: ResourceIgnoreDifferences can not be nil` / `ResourceOverride can not be nil` / `AppName must be set when retrieving from cache` / `StateCache must be set when retrieving from cache` / `serverSideDryRunner must be set when using server side diff`) are returned to API/CLI callers, while the controller discards the Build error and then fails with `failed to perform pre-diff normalization: preDiffNormalize error: diffConfig can not be nil`; any StateDiffs error produces a `ComparisonError` condition `Failed to compare desired state to live state: ...` and forces sync status `Unknown` for every resource; a normalizer error is only logged (`Failed to normalize <gvk>/<ns>/<name>`), it does not fail the diff.
- **Configuration surface**: none directly; aggregates all options in DIFF-002..DIFF-026.
- **Exposure**: CRD (`status.sync`, `status.resources[]`) / API (ManagedResources, ServerSideDiff) / CLI (`argocd app diff`, `argocd app get`) / UI (resource DIFF tab, app diff panel).
- **Tests required**: positive: identical target/live -> Modified=false; changed spec field -> Modified=true with both normalized JSON documents returned; extraneous live object paired with nil target; target with no live -> paired with nil live; duplicate-UID live objects in two groups collapse to the Git-declared one; negative: mismatched array lengths error; invalid diff config error surfaces as ComparisonError and sync Unknown; recovery: next reconcile after fixing config clears the condition.

### DIFF-002 Legacy three-way diff using last-applied-configuration
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/diff.go#L122, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/diff.go#L731, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/diff.go#L812, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/diff.go#L611, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/diff.go#L862, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/diff-strategies.md#L10
- **Maturity**: GA
- **Semantics**: Default strategy when neither server-side diff nor structured-merge diff applies. If the live object carries `kubectl.kubernetes.io/last-applied-configuration`, it is parsed as `orig`, fully normalized with the same options, and a three-way diff (orig, config, live) is computed. For types registered in the client-go scheme: a strategic three-way merge patch (`CreateThreeWayMergePatch`, overwrite=true) is applied to live; scheme defaulting is then applied to both predicted-live and live (by computing a defaulting patch on the typed object and applying it to the untyped JSON so unknown fields survive) and both are re-marshaled through `map[string]any` to sort keys. For unregistered types (CRDs): fields present in live but absent from `orig` are removed from live (`RemoveMapFields`) and a JSON three-way merge patch is applied with `jsonpatch.MergePatch`.
- **Edge cases**: unparsable last-applied annotation -> logged (`Failed to get last applied configuration`) and two-way diff is used; 3-way failure -> logged (`three-way diff calculation failed: ... Falling back to two-way diff`) then two-way; patch construction errors carry messages `failed to construct thre way merge patch` (sic) / `failed to construct strategic merge patch`; fields removed from Git but still in last-applied are predicted as deleted (OutOfSync); fields added by controllers but never in last-applied are ignored.
- **Configuration surface**: annotation `kubectl.kubernetes.io/last-applied-configuration` on live objects (written by client-side apply sync).
- **Exposure**: CRD (resource `status`) / API (ManagedResources `predictedLiveState`, `normalizedLiveState`) / CLI (`argocd app diff`) / UI (DIFF tab).
- **Tests required**: port gitops-engine `TestThreeWayDiff*`, `TestThreeWayDiffExample1..4`, `TestThreeWayDiffExample2WithDifference`, `TestThreeWayDiffExplicitNamespace`; positive: controller-added field (e.g. Deployment `spec.template.spec.dnsPolicy` default) not reported; negative: field removed from Git but present in last-applied reported as diff; CRD with extra live status fields not reported; recovery: corrupt annotation falls back to two-way without error.

### DIFF-003 Two-way diff fallback
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/diff.go#L554, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/diff.go#L133
- **Maturity**: GA
- **Semantics**: When no usable last-applied annotation exists (or 3-way failed), `TwoWayDiff` runs `ThreeWayDiff(config, config.DeepCopy(), live)` — i.e. the desired object is used as its own "last applied", so only fields declared in Git are compared; live-only fields (defaults, controller-populated) are not reported.
- **Edge cases**: list fields managed by strategic merge keys merge by key, so extra live list items under a merge key are kept, while atomic lists replace; for CRDs the JSON merge path removes live map fields not in config before diffing.
- **Configuration surface**: none.
- **Exposure**: same as DIFF-002.
- **Tests required**: positive: resource created with server-side apply (no last-applied) and unchanged -> Synced; negative: changing a Git-declared field -> OutOfSync; live-only label added by a controller -> still Synced.

### DIFF-004 Create and delete diff (missing live or missing target)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/diff.go#L564, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/diff.go#L140
- **Maturity**: GA
- **Semantics**: If live is nil: `Modified=true`, `NormalizedLive="null"`, `PredictedLive=<config JSON>`. If config is nil: `Modified=false`, `NormalizedLive=<live JSON>`, `PredictedLive="null"` (extraneous resources are flagged OutOfSync by the sync-status logic, not by the diff). Server-side diff and structured-merge diff use this same shortcut (no dry-run apply is executed on create).
- **Edge cases**: both nil -> error `both live and config are null objects`; both non-nil passed to the helper -> `unnexpected state: expected live or config to be null: not create or delete operation` (sic).
- **Configuration surface**: none.
- **Exposure**: API (ManagedResources items with `liveState: "null"` / `targetState: "null"`), UI (diff shows full add/remove), CLI.
- **Tests required**: positive: missing resource -> Modified=true and full predicted manifest; extraneous resource -> Modified=false with NormalizedLive populated; SSD create path issues zero dry-run calls (assert via fake applier); negative: both-nil error.

### DIFF-005 Remarshal, scheme defaulting and creationTimestamp normalization
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/diff.go#L1230, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/diff.go#L905, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/diff.go#L586, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/diff.go#L698
- **Maturity**: GA
- **Semantics**: Before any strategy, both config and live are remarshaled: JSON round-trip strips Go type information (int vs float64); if the GVK is registered in the scheme, the object is decoded into the typed struct with `DisallowUnknownFields`, converted back to unstructured (dropping omitempty zero values and normalizing quantities/int-or-string), and fields that exist only because of the typed marshaller (e.g. `creationTimestamp: null`) are removed (`RemoveMapFields(original, typed)`). `Normalize` then always deletes `metadata.creationTimestamp` and dispatches the built-in kind normalizers (Secret, Role/ClusterRole, Endpoints).
- **Edge cases**: unknown fields or invalid spec in Git -> typed decode fails, original object is used unchanged (logged at V(1) `Could not unmarshal to object of type ...`); unregistered GVK (all CRDs) -> only the JSON round-trip; marshal failure panics (not reachable with valid unstructured input); resource quantities like `cpu: 0.5` vs `500m` compare equal for registered kinds.
- **Configuration surface**: none.
- **Exposure**: indirect (affects Modified and normalized documents everywhere).
- **Tests required**: port `TestRemarshal`, `TestRemarshalStatefulSetCreationTimestamp`, `TestRemarshalResources`, `TestNullCreationTimestamp`, `TestDiffResourceWithInvalidField`; positive: `cpu: "1"` vs `cpu: 1000m` equal; negative: CRD numeric `1` vs `1.0` not falsely different after round-trip.

### DIFF-006 StatefulSet volumeClaimTemplates workaround
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/diff.go#L796, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/diff.go#L824
- **Maturity**: GA
- **Semantics**: In the strategic 3-way path for `apps/StatefulSet` or `extensions/StatefulSet`, fields in live `spec.volumeClaimTemplates` items that are absent from the corresponding `orig` items are removed from live before computing the patch (`RemoveListFields`), suppressing API-server defaults on embedded PVC templates (e.g. `status`, `volumeMode`).
- **Edge cases**: applies only when both orig and live have `spec.volumeClaimTemplates`; list length differences are not reconciled by the workaround; not applied for server-side diff.
- **Configuration surface**: none.
- **Exposure**: indirect.
- **Tests required**: positive: StatefulSet whose live PVC template gained `volumeMode: Filesystem` and `status.phase: Pending` stays Synced; negative: changed storage request in Git is reported.

### DIFF-007 Namespace and empty-annotation stripping in three-way diff
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/diff.go#L770
- **Maturity**: GA
- **Semantics**: Before the 3-way patch is computed, `metadata.namespace` is removed from both `orig` and `config`, and `metadata.annotations` is removed when it is null or an empty map. Live keeps its namespace, so the patch never tries to change it.
- **Edge cases**: a namespace mismatch between Git and live is therefore never a diff in this strategy (pairing by key already guarantees the same namespace); cluster-scoped objects with an erroneous namespace in Git are normalized earlier (DIFF-029).
- **Configuration surface**: none.
- **Exposure**: indirect.
- **Tests required**: port `TestRemoveNamespaceAnnotation`, `TestIgnoreNamespaceForClusterScopedResources`; positive: `annotations: {}` in Git vs no annotations live -> Synced.

### DIFF-008 Structured-merge diff (implicit with ServerSideApply)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/diff.go#L113, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/diff.go#L421, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/diff.go#L474, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L909, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/diff-strategies.md#L18
- **Maturity**: Deprecated (diff-strategies.md marks it "Feature Discontinued ... in favour of Server-Side Diff"; still active in code)
- **Semantics**: Used (when server-side diff is off) if the Application has sync option `ServerSideApply=true` in `spec.syncPolicy.syncOptions`, or if the desired object carries `argocd.argoproj.io/sync-options` containing `ServerSideApply=true`. Config and live are converted to typed values using the cluster OpenAPI GVK parser (falling back to the built-in scheme), and the structured-merge-diff `Updater.Apply` is executed with manager `argocd-controller` (operation Apply, force=true) against the live managed fields, i.e. a local simulation of server-side apply. If the merge produces no change, live is returned for both sides; otherwise predicted and live are normalized (last-applied annotation removed, scheme defaults patched in for registered kinds).
- **Edge cases**: GVK not resolvable -> error `unable to resolve parseableType for GroupVersionKind: <gvk>` which fails the comparison (ComparisonError); live managedFields decode failure -> `error decoding managed fields`; CRDs with schema defaults can show false diffs (documented reason for discontinuation); HPA v2 desired vs v1 live conversion handled by the version converter (`TestStructuredMergeDiff_HPAv2ToV1Conversion`).
- **Configuration surface**: Application `spec.syncPolicy.syncOptions: [ServerSideApply=true]`; resource annotation `argocd.argoproj.io/sync-options: ServerSideApply=true`; fixed manager name `argocd-controller` (`common.ArgoCDSSAManager`).
- **Exposure**: CRD / API / CLI / UI (same as DIFF-001).
- **Tests required**: port `TestStructuredMergeDiff`, `TestStructuredMergeDiff_HPAv2ToV1Conversion`, `TestStructuredMergeDiff_HPAv2ToV1Conversion_Modified`; positive: SSA-synced Deployment unchanged -> Synced; negative: field owned by another manager changed in Git -> diff; precedence: server-side diff enabled overrides SMD.

### DIFF-009 Server-side diff (dry-run apply)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/diff.go#L81, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/diff.go#L165, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/diff_options.go#L70, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L864, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sync.go#L59, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-application-controller/commands/argocd_application_controller.go#L305, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/diff-strategies.md#L31
- **Maturity**: GA (diff-strategies.md: "Stable (Since v3.1.0)"; not listed in feature-maturity.md)
- **Semantics**: Enablement precedence: controller-wide flag `--server-side-diff-enabled` (env `ARGOCD_APPLICATION_CONTROLLER_SERVER_SIDE_DIFF`, cmd-params `controller.diff.server.side`, default false) OR Application annotation `argocd.argoproj.io/compare-options` containing `ServerSideDiff=true`; then, if the annotation contains `ServerSideDiff=false`, it is disabled for that app regardless. When on: config/live get only the light pre-normalization (creationTimestamp + built-in kinds; ignoreDifferences/knownTypes skipped); for each pair with both sides present a server-side apply in dry-run mode (`DryRunServer`, force=false, validate=false, SSA=true, manager `argocd-controller`) returns predicted live; mutation-webhook changes are removed (DIFF-010) unless included; predicted live is remarshaled and fully normalized (ignoreDifferences + knownTypes) and `metadata.managedFields`, `metadata.resourceVersion` and the last-applied annotation are removed from predicted and live before byte comparison. SSD takes precedence over structured-merge diff. Create/delete pairs use DIFF-004 without calling the API.
- **Edge cases**: dry-run errors (e.g. admission webhook rejection, validation errors) fail the whole comparison: `error calculating server side diff: serverSideDiff error: error running server side apply in dryrun mode for resource <Kind>/<name>: ...` -> ComparisonError, sync Unknown; failure to build the dry-run applier adds an `UnknownError` condition and leaves the dry runner unset, so config validation fails and the comparison errors with `... preDiffNormalize error: diffConfig can not be nil` (sync Unknown); GVK parser load failure adds `UnknownError` condition; annotation matching is exact and case-sensitive (`ServerSideDiff=True` is ignored); app-level ignoreDifferences fields must still be valid in the dry-run request because they are only removed after the dry run.
- **Configuration surface**: flag `--server-side-diff-enabled`; env `ARGOCD_APPLICATION_CONTROLLER_SERVER_SIDE_DIFF`; `argocd-cmd-params-cm` `controller.diff.server.side`; Application annotation `argocd.argoproj.io/compare-options: ServerSideDiff=true|false`.
- **Exposure**: CRD (annotation) / API (ServerSideDiff RPC, DIFF-039) / CLI (`argocd app diff --server-side-diff`) / UI (diff panels render cached SSD results).
- **Tests required**: port `TestServerSideDiff`, `TestExcludeManagerOwnedAncestors`, `TestServerSideDiffPreservesManagerOwnedFieldUnderWebhookMutation`; positive: global flag on -> dry-run invoked; annotation true with flag off -> invoked; annotation false with flag on -> not invoked (legacy used); validating webhook rejecting the object -> ComparisonError; negative: `ServerSideDiff=True` (wrong case) ignored; authorization: dry-run uses the controller's cluster credentials (and impersonation if configured in the sync area); recovery: after webhook fix the next refresh clears the condition.

### DIFF-010 IncludeMutationWebhook compare option (webhook mutation removal)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/diff.go#L217, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/diff_options.go#L33, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L885, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/diff-strategies.md#L111
- **Maturity**: GA
- **Semantics**: Default (ignoreMutationWebhook=true): from the dry-run result, every field not owned by manager `argocd-controller` (union of that manager's managedFields entries) is removed, except ancestors that have manager-owned descendants (needed for `x-kubernetes-preserve-unknown-fields` subtrees) and except fields that are components of composite associative-list keys; the remainder is merged onto live, and fields the dry-run removed relative to live (only those owned by the manager) are removed again. Net effect: webhook-injected fields revert to their live values and are not shown as drift. With `IncludeMutationWebhook=true` in the Application compare-options annotation, the raw dry-run result is compared (webhook mutations appear in diffs). Only effective when server-side diff is active.
- **Edge cases**: dry-run result without managedFields -> `predictedLive for resource <Kind>/<name> must have the managedFields`; no entry for the manager -> `no managed fields found for manager: argocd-controller`; unknown GVK in parser -> `unable to resolve parseableType for GroupVersionKind`; all wrapped as `error removing non config mutations for resource ...` and fail the comparison.
- **Configuration surface**: Application annotation `argocd.argoproj.io/compare-options: IncludeMutationWebhook=true` (combinable, e.g. `ServerSideDiff=true,IncludeMutationWebhook=true`).
- **Exposure**: CRD annotation / API (ServerSideDiff RPC honors it) / CLI / UI.
- **Tests required**: positive: sidecar-injecting mutating webhook -> Synced by default; with annotation -> OutOfSync showing the injected container; field owned by both webhook and argocd preserved (manager-owned field under webhook mutation); composite-key list (ports with containerPort+protocol) keeps key fields; negative: annotation without SSD has no effect.

### DIFF-011 Diff result cache reuse
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L1121, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/diff/diff.go#L341, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/diff/diff.go#L395, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/diff-strategies.md#L34
- **Maturity**: GA
- **Semantics**: The controller reuses the previously cached per-resource diff (the app's "managed resources" cache entry) instead of recomputing when ALL hold: not a no-cache comparison; no refresh requested on the app (normal or hard); status not soft-expired (this check is skipped when server-side diff is on); number of manifest responses equals number of sources; resolved revisions equal `status.sync.revision(s)`; `spec` (sources, destination, ignoreDifferences) equals `status.sync.comparedTo`. Within a cached run, a resource reuses its cached `Modified/NormalizedLive/PredictedLive` only if a cached entry exists for its key and the cached `resourceVersion` equals the live object's current resourceVersion (empty for missing live); otherwise it is diffed fresh.
- **Edge cases**: cache miss/error -> warning `DiffFromCache: cannot get managed resources for app ...` and full diff; for Secrets the cached normalized/predicted states are the masked ones, but only `Modified` drives sync status; changing `spec.ignoreDifferences` invalidates via comparedTo; changing system-level `resource.customizations` does NOT change comparedTo, so cached results persist until resourceVersion/revision change or refresh.
- **Configuration surface**: refresh annotation (`argocd.argoproj.io/refresh`, other area); status refresh timeout (controller flag, other area).
- **Exposure**: internal; observable via API timing and SSD call counts.
- **Tests required**: positive: unchanged app + unchanged resourceVersion -> no dry-run calls on second reconcile with SSD; resourceVersion bump -> recomputed; hard refresh -> recomputed; negative: system override change without refresh keeps old result (parity quirk); recovery: cache eviction falls back to full diff.

### DIFF-012 Secret normalization (stringData and empty values)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/diff.go#L938
- **Maturity**: GA
- **Semantics**: For core `Secret`: every `stringData` entry is moved into `data` (string values base64-encoded, non-string values copied as is so that apply would still fail on invalid input) and `stringData` is deleted; then via typed conversion every `data` value of zero length is normalized to `""` (null vs empty equivalence).
- **Edge cases**: invalid Secret (fails typed conversion, e.g. non-base64 data) -> logged `Failed to convert from unstructured into Secret`, object left as partially normalized; `stringData` key overriding an existing `data` key wins; nil `data` values (issue 943) compare equal to empty.
- **Configuration surface**: none.
- **Exposure**: indirect.
- **Tests required**: port `TestSecretStringData`, `TestInvalidSecretStringData`, `TestNullSecretData`, `TestNormalizeSecret`; positive: Git `stringData: {a: b}` vs live `data: {a: Yg==}` -> Synced.

### DIFF-013 Role/ClusterRole empty rules normalization
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/diff.go#L1038
- **Maturity**: GA
- **Semantics**: For `rbac.authorization.k8s.io` `Role` and `ClusterRole`, an empty `rules: []` is set to `null` so it equals an absent/nil rules list.
- **Edge cases**: non-array `rules` left untouched.
- **Configuration surface**: none.
- **Exposure**: indirect.
- **Tests required**: port `TestNullRoleRule`; positive: `rules: []` vs missing -> Synced.

### DIFF-014 ignoreAggregatedRoles compare option
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/diff.go#L1048, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L634, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L876, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/diffing.md#L135
- **Maturity**: GA
- **Semantics**: When `resource.compareoptions` has `ignoreAggregatedRoles: true` (default false), any Role/ClusterRole that has an `aggregationRule` map gets `rules` set to null on both sides, so rules filled in by the aggregation controller never show as drift.
- **Edge cases**: malformed (non-map) `aggregationRule` -> logged `Malformed aggregationRule in resource '<name>', won't modify.`; the CLI client-side diff (`argocd app diff`) and the ServerSideDiff API hard-code `ignoreAggregatedRoles=false`, so they can disagree with the controller's sync status (parity quirk); unparsable `resource.compareoptions` YAML makes the controller log `Could not get compare options from ConfigMap (assuming defaults)` and use defaults.
- **Configuration surface**: `argocd-cm` `resource.compareoptions: | ignoreAggregatedRoles: true`.
- **Exposure**: ConfigMap / indirect in CRD status.
- **Tests required**: port `TestDiffOptionIgnoreAggregateRoles`; positive: aggregated ClusterRole with live-populated rules -> Synced when true, OutOfSync when false; negative: non-aggregated role still diffed.

### DIFF-015 Endpoints normalization
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/diff.go#L1000
- **Maturity**: GA
- **Semantics**: For core `Endpoints`, ports with empty `protocol` get `TCP`, and subsets (addresses, ports) are sorted with the Kubernetes `SortSubsets` algorithm so ordering differences are not drift.
- **Edge cases**: conversion failure -> logged, object unchanged.
- **Configuration surface**: none.
- **Exposure**: indirect.
- **Tests required**: port `TestUnsortedEndpoints`; positive: reordered addresses -> Synced; missing protocol vs `TCP` -> Synced.

### DIFF-016 knownTypeFields normalization for CRDs
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/normalizers/knowntypes_normalizer.go#L42, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/normalizers/knowntypes_normalizer.go#L60, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/normalizers/knowntypes_normalizer.go#L84, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/normalizers/corev1_known_types.go#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L1192, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/diffing.md#L150
- **Maturity**: GA
- **Semantics**: For a group/kind with `knownTypeFields` entries (`field`: dot-separated path, `type`: one of the 241 `core/v1/<Type>` names listed in `diffing_known_types.txt` plus `core/Quantity` and `meta/v1/Duration`), the field value is JSON round-tripped through the named Kubernetes Go type so custom marshalling (quantities, durations, int-or-string) is canonical on both sides. If any path prefix resolves to an array, the remaining path is applied to every element (nested arrays supported). Default: `argoproj.io/Rollout` gets `spec.template.spec` as `core/v1/PodSpec` unless the user configured any knownTypeFields for that GK. Runs after the ignore normalizer.
- **Edge cases**: unsupported type -> warning `Failed to configure known field normalizer: type '<t>' is not supported` and entry skipped; invalid override key -> warning; value that cannot unmarshal into the type -> normalizer error, logged and normalization of that object stops (diff continues un-normalized); missing field -> no-op; user config for Rollout replaces (does not extend) the default.
- **Configuration surface**: `argocd-cm` `resource.customizations.knownTypeFields.<group>_<kind>` (YAML list of `{field, type}`); legacy `resource.customizations` YAML key `knownTypeFields`.
- **Exposure**: ConfigMap / indirect.
- **Tests required**: port `TestNormalize_MapField`, `TestNormalize_FieldInNestedSlice`, `TestNormalize_FieldInDoubleNestedSlice`, `TestNormalize_Quantity`, `TestNormalize_Duration`, `TestFieldDoesNotExist`, `TestRolloutPreConfigured`, `TestOverrideKeyWithoutGroup`, `TestKnownTypes`, `TestDiff_KnownTypeInvalidValue`; positive: Rollout `cpu: 0.1` vs `100m` -> Synced with no config; negative: unsupported type name logs and leaves diff.

### DIFF-017 Application-level ignoreDifferences resource matching
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L124, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/normalizers/diff_normalizer.go#L185, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/diff/ignore.go#L43, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/crds/application-crd.yaml#L1046, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/diffing.md#L20
- **Maturity**: GA
- **Semantics**: `spec.ignoreDifferences[]` entries `{group, kind (required), name, namespace, jsonPointers, jqPathExpressions, managedFieldsManagers}`. An entry applies to an object when `group` glob-matches the object's API group (gobwas glob; omitted group = `""` = core only), `kind` glob-matches the kind (`*` = any), `name` is empty or exactly equal, and `namespace` is empty or exactly equal. Matching is applied to both target and live (and last-applied) documents. Multiple matching entries (app and system level) are all applied; there is no precedence beyond union.
- **Edge cases**: invalid glob -> warning `failed to compile pattern ...` and the entry never matches; name/namespace are exact (no glob); group must not include the version (`apps`, not `apps/v1`); changing ignoreDifferences changes `status.sync.comparedTo` and forces recompute.
- **Configuration surface**: Application `spec.ignoreDifferences` (also recorded in `status.sync.comparedTo.ignoreDifferences`); ApplicationSet templates can set it (other area).
- **Exposure**: CRD / API (Create/Update app) / CLI (`argocd app create --file`, `argocd app edit`; no dedicated flag exists) / UI (app manifest editor).
- **Tests required**: port `TestNormalizeObjectWithMatchedGroupKind`, `TestNormalizeNoMatchedGroupKinds`, `TestNormalizeGlobMatch`; positive: `group: '*', kind: '*'` matches everything; name+namespace scoping only affects the named object; negative: `group: apps/v1` matches nothing; authorization: only users with `applications, update` can change it (API area).

### DIFF-018 ignoreDifferences jsonPointers
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/normalizers/diff_normalizer.go#L143, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/normalizers/diff_normalizer.go#L233, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/diffing.md#L73
- **Maturity**: GA
- **Semantics**: Each pointer becomes an RFC 6902 `remove` operation applied to the JSON of the object; matched patches are applied sequentially. `/` inside a key must be escaped as `~1` (and `~` as `~0`). Removal applies to both sides, so the field is invisible to the diff.
- **Edge cases**: pointer to a nonexistent path -> error silently skipped (`Unable to remove nonexistent key`, `remove operation does not apply: doc is missing path` are not logged); other apply errors logged at debug `Failed to apply normalization patch` and skipped; array index pointers (`/spec/containers/0/image`) remove by position only; malformed pointer without leading slash fails at apply time and is skipped.
- **Configuration surface**: `jsonPointers` in app-level entries, system-level `resource.customizations.ignoreDifferences.*`, `ignoreResourceUpdates`.
- **Exposure**: CRD / ConfigMap.
- **Tests required**: port `TestNormalizeMissingJsonPointer`, `TestNormalizeMatchedResourceOverrides`; positive: `/spec/replicas` ignored for HPA-scaled Deployment; escaped label key `/metadata/labels/node-role.kubernetes.io~1worker` works; negative: missing path does not error or log at info level.

### DIFF-019 ignoreDifferences jqPathExpressions
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/normalizers/diff_normalizer.go#L161, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/normalizers/diff_normalizer.go#L72, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/diffing.md#L49
- **Maturity**: GA
- **Semantics**: Each expression `E` is compiled (gojq) as `del(E)` and run against the object JSON; the single result replaces the document. Enables selecting list items by content, e.g. `.spec.template.spec.initContainers[] | select(.name == "injected-init-container")`.
- **Edge cases**: syntax error -> compile error at normalizer construction, which fails the whole StateDiffs (`failed to create diff normalizer: ...`) -> ComparisonError and sync Unknown for the app; runtime error -> `JQ patch returned error: ...` (skipped for that object, logged at debug); no output -> `JQ patch did not return any data`; multiple outputs -> `JQ patch returned multiple objects`; timeout -> `JQ patch execution timed out (<duration>)` (skipped).
- **Configuration surface**: `jqPathExpressions` in app-level entries, system-level overrides, ignoreResourceUpdates.
- **Exposure**: CRD / ConfigMap.
- **Tests required**: port `TestNormalizeJQPathExpression`, `TestNormalizeIllegalJQPathExpression`, `TestNormalizeJQPathExpressionWithError`, `TestJQPathExpressionReturnsHelpfulError`, `TestNormalizeExpectedErrorAreSilenced`, `TestNormalizeFailureLogIncludesResourceContext`; negative: illegal expression yields ComparisonError; recovery: fixing the expression clears it.

### DIFF-020 JQ execution timeout configuration
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/normalizers/diff_normalizer.go#L24, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/normalizers/diff_normalizer.go#L113, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-application-controller/commands/argocd_application_controller.go#L306, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/application-controller/argocd-application-controller-statefulset.yaml#L325, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/diffing.md#L198
- **Maturity**: GA
- **Semantics**: Per-expression timeout, default 1s (`DefaultJQExecutionTimeout`); a configured 0 means default. Controller: `--ignore-normalizer-jq-execution-timeout-seconds` (a Go duration despite the name), env `ARGOCD_IGNORE_NORMALIZER_JQ_TIMEOUT`, cmd-params key `controller.ignore.normalizer.jq.timeout`. CLI commands `argocd app diff`, `argocd app sync` (used with `--preview-changes`), `argocd admin app get-reconcile-results`, `argocd admin settings resource-overrides ignore-resource-updates` accept `--ignore-normalizer-jq-execution-timeout` (default 1s). The ServerSideDiff API and resource-overrides `ignore-differences` use the default.
- **Edge cases**: timeout error message includes the duration string; the manifest generation-hash path for ignoreResourceUpdates uses the controller value.
- **Configuration surface**: listed above.
- **Exposure**: controller flag/env/ConfigMap; CLI flags.
- **Tests required**: port `TestJqPathExpressionFailWithTimeout`; positive: expensive expression succeeds with 5s, fails with 1ms; negative: 0 falls back to 1s.

### DIFF-021 ignoreDifferences managedFieldsManagers
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/diff/diff.go#L431, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/managedfields/managed_fields.go#L23, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/managedfields/managed_fields.go#L74, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/diff/ignore.go#L43, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/diffing.md#L60
- **Maturity**: GA
- **Semantics**: Pre-diff step, only when both target and live exist and a matching ignore config (system override for `group/kind`, the `*/*` override, and app entries matched as in DIFF-017 using the target's name/namespace) lists managers. Live and target are converted to typed values (cluster OpenAPI parseable type; deduced type with atomic lists/maps when unavailable), compared, and for each live `managedFields` entry whose manager is trusted, the intersection of that entry's field set with the *modified* fields is removed from both objects. Fields owned by trusted managers that Git changes are therefore not drift.
- **Edge cases**: typed conversion failure -> debug log, objects unchanged (no error); managedFields JSON decode failure -> error `error normalizing manager <m>: ...` which fails the comparison; only modified fields are removed (added/removed fields still diff); system-level lookup uses key `group/kind` with literal `/` (for core kinds the key would be `/Kind`, see Unconfirmed); not applied for create/delete.
- **Configuration surface**: `managedFieldsManagers` in `spec.ignoreDifferences[]`, `resource.customizations.ignoreDifferences.<gk>`, `resource.customizations.ignoreDifferences.all`.
- **Exposure**: CRD / ConfigMap.
- **Tests required**: positive: `kube-controller-manager`-owned `spec.replicas` changed in Git -> Synced; entry for another manager -> OutOfSync; CRD without schema uses deduced type (whole list ignored if any item modified by manager); negative: corrupt managedFields -> ComparisonError.

### DIFF-022 System-level ignoreDifferences per group/kind
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L1140, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L1210, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/normalizers/diff_normalizer.go#L121, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/normalizers/util.go#L8, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/diffing.md#L83
- **Maturity**: GA
- **Semantics**: `argocd-cm` key `resource.customizations.ignoreDifferences.<group>_<kind>` (or `.<kind>` for core) holds YAML `{jsonPointers, jqPathExpressions, managedFieldsManagers}` applied to every Application. Key parsing: split the suffix on `_`; 2 parts -> `group/kind`, 1 part -> `kind`. Override keys are converted to `{group, kind}` (glob-capable) ignore entries and appended after app-level entries.
- **Edge cases**: more than one `_` in the suffix -> settings error `group kind should be in format ...` which makes `GetResourceOverrides` fail (comparison returns Unknown health/sync with log `Basic comparison settings cannot be loaded`); unknown customization type -> `resource customization type <t> not supported`; invalid YAML -> error; split keys override the same field set by the legacy `resource.customizations` block for that key (they are applied after it).
- **Configuration surface**: `resource.customizations.ignoreDifferences.<group_kind>` in `argocd-cm`.
- **Exposure**: ConfigMap; inspect with `argocd admin settings resource-overrides ignore-differences`.
- **Tests required**: positive: MutatingWebhookConfiguration `.webhooks[]?.clientConfig.caBundle` ignored for all apps; core kind key `resource.customizations.ignoreDifferences.Service`; negative: key with two underscores breaks settings load (assert error surfaced); recovery: fixing the key restores comparison.

### DIFF-023 System-level ignoreDifferences for all resources
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L1157, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/diff/ignore.go#L51, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/diffing.md#L106
- **Maturity**: GA
- **Semantics**: Suffix `all` maps to override key `*/*`, i.e. group `*` and kind `*`, matching every resource (including core kinds) for jsonPointers/jqPathExpressions, and merged into every managed-fields lookup.
- **Edge cases**: `*/*` also receives the automatic `/status` pointer from DIFF-025 (appended to user pointers); a health Lua under `*/*` would never match core kinds (`*/*` requires a `/`, core keys have none).
- **Configuration surface**: `resource.customizations.ignoreDifferences.all`.
- **Exposure**: ConfigMap.
- **Tests required**: positive: `/spec/replicas` ignored for Deployments and StatefulSets of all apps; managers listed under `all` honored; negative: pointers not applicable to a kind are silently skipped.

### DIFF-024 Legacy `resource.customizations` YAML block
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L1051, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2585, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2613, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/health.md#L126
- **Maturity**: GA
- **Semantics**: Single key `resource.customizations` containing a YAML map `<group/kind or kind or glob>: {health.lua, health.lua.useOpenLibs, actions, ignoreDifferences (YAML string), ignoreResourceUpdates (YAML string), knownTypeFields (list)}`. Parsed first; split keys (`resource.customizations.<type>.<group_kind>`) are layered on top. This is the only form that supports wildcard keys (`*`), since ConfigMap keys cannot contain `*`.
- **Edge cases**: invalid YAML in the block or in the nested ignoreDifferences string -> settings load error; keys beginning with `*` must be quoted; empty value is ignored.
- **Configuration surface**: `argocd-cm` `resource.customizations`.
- **Exposure**: ConfigMap.
- **Tests required**: positive: legacy block with ignoreDifferences + health.lua works; split key for the same GK overrides only the fields it sets; wildcard `"*.aws.crossplane.io/*"` health applies; negative: malformed nested YAML string errors.

### DIFF-025 ignoreResourceStatusField compare option
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L1070, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L623, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L1220, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/diffing.md#L117
- **Maturity**: GA
- **Semantics**: `resource.compareoptions.ignoreResourceStatusField`: `all` or empty (default) -> JSON pointer `/status` added to override `*/*` (status ignored for every resource); `crd` -> `/status` added only to `apiextensions.k8s.io/CustomResourceDefinition` (the CRD objects themselves, not custom resources); `none`, `off`, `false` -> nothing added; any other value -> warning `Unrecognized value for ignoreResourceStatusField - <v>, ignore status for all resources` and treated as `all`.
- **Edge cases**: unquoted YAML `off` parses as boolean false and is accepted; with IgnoreDifferencesOnResourceUpdates (default true) the `/status` ignore also suppresses status-only refreshes (DIFF-035).
- **Configuration surface**: `argocd-cm` `resource.compareoptions` YAML key `ignoreResourceStatusField`.
- **Exposure**: ConfigMap.
- **Tests required**: positive: CR with status in Git -> Synced under default; `none` -> OutOfSync; `crd` -> CRD status ignored but CR status diffed; negative: garbage value behaves as `all`.

### DIFF-026 Tracking metadata normalization during diff
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/resource_tracking.go#L262, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/diff/diff.go#L427
- **Maturity**: GA
- **Semantics**: Applied to deep copies before diffing when tracking method is `annotation` or `annotation+label` (no-op for `label`/empty) and both sides exist: if live carries the app-instance label but no `argocd.argoproj.io/tracking-id` annotation (and is not a CRD), the desired tracking annotation is copied onto live (migration from label tracking does not cause drift); if the desired object has no instance label, the label is removed from live.
- **Edge cases**: a live tracking annotation that differs from desired (resource moved between apps) is kept and surfaces as a diff; errors are ignored (`_ =`).
- **Configuration surface**: `application.resourceTrackingMethod`, `application.instanceLabelKey` (tracking area).
- **Exposure**: indirect.
- **Tests required**: positive: switch tracking from label to annotation -> apps stay Synced; negative: wrong tracking annotation on live -> OutOfSync.

### DIFF-027 compare-options annotation parsing
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L219, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/resource/annotations.go#L16, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/resource/annotations.go#L36
- **Maturity**: GA
- **Semantics**: Annotation `argocd.argoproj.io/compare-options` is a comma-separated list; items are trimmed, empty items dropped, duplicates removed, and matched by exact, case-sensitive string equality. Recognized values: on the Application object — `ServerSideDiff=true`, `ServerSideDiff=false`, `IncludeMutationWebhook=true`; on a managed resource — `IgnoreExtraneous`.
- **Edge cases**: unknown values ignored without error; `IgnoreExtraneous=true` is not recognized; options on the wrong object (e.g. `ServerSideDiff=true` on a Deployment) have no effect.
- **Configuration surface**: the annotation.
- **Exposure**: CRD / resource metadata.
- **Tests required**: positive: `" ServerSideDiff=true , IncludeMutationWebhook=true"` both recognized; negative: `IgnoreExtraneous` on Application ignored; `serverSideDiff=true` ignored.

### DIFF-028 IgnoreExtraneous compare option
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L991, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/compare-options.md#L1
- **Maturity**: GA
- **Semantics**: A resource that needs pruning (live present, no target) whose live object has `argocd.argoproj.io/compare-options: IgnoreExtraneous` is still marked `OutOfSync` at resource level (and `requiresPruning` as applicable) but does not make the Application `OutOfSync`. Health is unaffected (a Degraded extraneous resource still degrades the app).
- **Edge cases**: only applies to extraneous resources; on a resource that is present in Git and modified, the annotation has no effect; auto-sync with prune would still delete it unless `Prune=false` (sync area).
- **Configuration surface**: resource annotation (e.g. via kustomize `generatorOptions.annotations`).
- **Exposure**: CRD (`status.resources[].status`, `status.sync.status`) / UI ("needs pruning" badge) / CLI.
- **Tests required**: positive: extraneous annotated ConfigMap -> resource OutOfSync, app Synced; negative: extraneous unannotated -> app OutOfSync; modified annotated resource -> app OutOfSync.

### DIFF-029 Target manifest normalization and duplicate detection
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L525, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/utils/kube/kube.go#L74
- **Maturity**: GA
- **Semantics**: Before diffing, each target object: if its group/kind is cluster-scoped (per cluster discovery) and it has a namespace, the namespace is cleared; if namespaced (or scope unknown) and it has no namespace, the app destination namespace is set; in either case tracking metadata is re-applied. Objects are keyed by group/kind/namespace/name (objects with only `generateName` get key name `<generateName><index>`); if a key occurs more than once, condition `RepeatedResourceWarning` `Resource <key> appeared <n> times among application resources.` is added and only the last occurrence is kept.
- **Edge cases**: unknown scope (CRD not yet installed) treated as namespaced; failure to set tracking -> ComparisonError `Failed to normalize target state: ...`; namespaced resource still lacking namespace after normalization -> `InvalidSpecError` `Namespace for <name> <gvk> is missing.`
- **Configuration surface**: Application `spec.destination.namespace`.
- **Exposure**: CRD conditions / UI conditions panel / CLI `argocd app get`.
- **Tests required**: positive: duplicate Service in two files -> warning condition, one resource; cluster-scoped ClusterRole with namespace in Git -> compared without namespace; negative: namespaced object with empty destination namespace -> InvalidSpecError.

### DIFF-030 Resource and application sync status computation
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L930, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L1034, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L1288, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2231
- **Maturity**: GA
- **Semantics**: Per pair a `ResourceStatus{group, version, kind, namespace, name, status, health, hook, requiresPruning, syncWave, requiresDeletionConfirmation}` is built (version from live if present else target). Status rules in order: (a) hooks, objects with an ignored hook annotation, targets with `argocd.argoproj.io/hook: Skip`, or live objects whose tracking id does not match them (copied annotations; never for `label` tracking) -> status left empty and excluded from app sync status; (b) if not the managed-namespace placeholder and (Modified, or target missing, or live missing) -> `OutOfSync` (app OutOfSync unless DIFF-028); (c) otherwise `Synced`. Then: group/kind/name not permitted by the project -> `Unknown`; any load failure (repo, live state, diff) -> every resource `Unknown` and app `Unknown`; else if managed-namespace metadata changed -> app `OutOfSync`. `requiresPruning` = target nil, live present, self-referenced. `requiresDeletionConfirmation` from `Delete=confirm`/`Prune=confirm` options (sync area).
- **Edge cases**: live objects with copied tracking annotation pointing to another object are silently excluded; sync status initial value `Unknown` when settings cannot be loaded; hook-type live objects get their sync wave from the matching hook target.
- **Configuration surface**: none specific.
- **Exposure**: CRD `status.sync.status`, `status.resources[]` / API / CLI `argocd app get` / UI sync badges.
- **Tests required**: positive/negative matrix for (a)-(c); project denies kind -> Unknown; repo failure -> Unknown everywhere; recovery: next successful compare restores statuses.

### DIFF-031 Secret data masking in diffs and manifests
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/diff.go#L1094, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/diff.go#L1172, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L811, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1548, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L3166
- **Maturity**: GA
- **Semantics**: For core `Secret` resources, before diff results are cached/served: target, live, and both last-applied annotations are normalized (stringData -> data) and every `data` value is replaced with runs of `+` characters: per key, distinct values across the four documents get distinct replacements (`++++++++`, then +4 `+` for each further distinct value), equal values get equal replacements, so "changed vs unchanged" remains visible. The last-applied annotation is re-serialized from the masked copy. When legacy diff was used, the controller then recomputes the diff on the masked objects (so normalized/predicted states hold masked values); when server-side diff was used, it masks the SSD result pair instead (recomputing would resurrect webhook drift). The same masking is applied in GetResource, managed manifests and the ServerSideDiff API (decision based on server-parsed objects, never caller-supplied kind).
- **Edge cases**: `data: null` handled without panic (issue 5584); unparsable last-applied annotation replaced wholesale with `++++++++`; masking never applies to non-core `Secret` kinds; because distinct values receive distinct replacements across target/live/last-applied, the legacy recompute on masked objects still reports a value-only change as `Modified`; masking errors abort caching with `error hiding secret data: ...`.
- **Configuration surface**: see DIFF-032 for annotations.
- **Exposure**: API (ManagedResources, GetResource, ServerSideDiff) / CLI (`argocd app diff`, `argocd app manifests`) / UI (DIFF tab, live manifest).
- **Tests required**: port `TestHideSecretData`, `TestRedactedSecretData`; positive: changed secret value shows `++++++++` vs `++++++++++++`; unchanged shows identical masks; authorization: no API path returns raw secret data (including spoofed ServerSideDiff requests pairing a non-Secret live kind with a Secret target); recovery: invalid last-applied JSON masked entirely.

### DIFF-032 Sensitive annotation masking on Secrets
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L2656, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/diff.go#L1130, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/declarative-setup.md#L1411
- **Maturity**: GA
- **Semantics**: `argocd-cm` `resource.sensitive.mask.annotations` is a comma-separated list (all spaces stripped) of annotation keys whose values on Secrets are masked with the same `+` scheme as data. If the list includes `kubectl.kubernetes.io/last-applied-configuration`, that annotation is replaced by `++++++++` entirely.
- **Edge cases**: ConfigMap read failure -> empty list (logged); matching is exact key equality; non-Secret objects are never masked.
- **Configuration surface**: `resource.sensitive.mask.annotations`.
- **Exposure**: ConfigMap / API / CLI / UI.
- **Tests required**: port `TestHideSecretData_HideAnnotations`, `TestHideSecretData_HideAnnotations_PreserveDifference`; positive: `openshift.io/token-secret.value` masked; negative: unlisted annotation shown in clear.

### DIFF-033 ignoreResourceUpdates rules (refresh suppression)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L996, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/cache.go#L571, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/cache.go#L606, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/info.go#L793, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/reconcile.md#L26
- **Maturity**: GA
- **Semantics**: For each watched object that is tracked (has an app name from tracking) or is an `argoproj.io/Application`, the cluster cache computes an xxhash of the object JSON after applying the ignore-resource-updates normalizer (jsonPointers + jqPathExpressions from `resource.customizations.ignoreResourceUpdates.<gk>` / `.all`). On a watch update, if old and new hashes are both non-empty and equal AND the cached health status code is unchanged (both nil or same status), the event is dropped: no app refresh is requested (debug log `Ignoring change of object because none of the watched resource fields have changed`). Otherwise the owning app(s) are requested to refresh.
- **Edge cases**: `managedFieldsManagers` in ignoreResourceUpdates are ignored (only pointers/jq are used); hash failures are logged and leave the hash empty (never suppress); health message changes alone do not prevent suppression; create/delete events are never suppressed.
- **Configuration surface**: `resource.customizations.ignoreResourceUpdates.<group_kind>` and `.all`; legacy YAML key `ignoreResourceUpdates`.
- **Exposure**: ConfigMap; inspect with `argocd admin settings resource-overrides ignore-resource-updates`.
- **Tests required**: port `TestSkipResourceUpdate`, `TestShouldHashManifest`, `TestManifestHash`; positive: ExternalSecret `/status/refreshTime` change does not trigger reconcile; health change triggers reconcile even if hash equal; negative: non-ignored field change triggers refresh.

### DIFF-034 resource.ignoreResourceUpdatesEnabled switch
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L1031, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/reconcile.md#L12
- **Maturity**: GA
- **Semantics**: `argocd-cm` `resource.ignoreResourceUpdatesEnabled` defaults to `true` when empty/absent; parsed with `strconv.ParseBool`. When false, no hashes are computed and every update of an app-related object triggers a refresh.
- **Edge cases**: unparsable value -> settings error, cache settings fail to load (log `Failed to read updated settings`) and the previous settings stay in effect; change is applied live through the settings watch, invalidating the cluster caches.
- **Configuration surface**: `resource.ignoreResourceUpdatesEnabled`.
- **Exposure**: ConfigMap.
- **Tests required**: positive: `"false"` disables suppression; empty -> enabled; negative: `"maybe"` keeps prior behavior and logs.

### DIFF-035 ignoreDifferencesOnResourceUpdates
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L1009, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L1221, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/reconcile.md#L51
- **Maturity**: GA
- **Semantics**: `resource.compareoptions.ignoreDifferencesOnResourceUpdates` (default `true` via `GetDefaultDiffOptions`): system-level ignoreDifferences jsonPointers/jqPathExpressions/managers of each override (including the automatic `/status` on `*/*`) are appended to that override's ignoreResourceUpdates for hashing. App-level `spec.ignoreDifferences` are never used for update suppression.
- **Edge cases**: setting `resource.compareoptions` without this key keeps the default true (YAML unmarshal over defaults); false -> only explicit ignoreResourceUpdates rules apply.
- **Configuration surface**: `resource.compareoptions: | ignoreDifferencesOnResourceUpdates: false`.
- **Exposure**: ConfigMap.
- **Tests required**: positive: status-only change of tracked CR does not refresh under defaults; with false, it refreshes; negative: app-level ignoreDifferences do not suppress refresh.

### DIFF-036 ignore-resource-updates annotation for untracked resources
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/cache.go#L81, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/cache.go#L402, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/reconcile.md#L127
- **Maturity**: GA
- **Semantics**: Untracked objects (children such as ReplicaSets, Pods, Jobs created by CronJobs) are hashed only if they carry `argocd.argoproj.io/ignore-resource-updates` parsed as boolean true; then the same suppression as DIFF-033 applies to them.
- **Edge cases**: unparsable value -> treated as false; annotation on a tracked resource is irrelevant (tracked resources are always hashed).
- **Configuration surface**: annotation `argocd.argoproj.io/ignore-resource-updates: "true"`.
- **Exposure**: resource metadata.
- **Tests required**: positive: annotated Job `/status` changes ignored given `ignoreResourceUpdates.batch_Job`; negative: unannotated child Pod updates trigger refresh.

### DIFF-037 Default refresh suppression rules
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L1024, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/cache.go#L380, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/cache.go#L625, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/reconcile.md#L67
- **Maturity**: GA
- **Semantics**: `/metadata/resourceVersion`, `/metadata/generation` and `/metadata/managedFields` are always appended to the `*/*` ignoreResourceUpdates pointers. Independently of hashing, updates of core `Endpoints` never trigger app requeue. A change to a root (top-level tracked) node requests a `CompareWithRecent` refresh; changes to non-root descendants only request a `ComparisonWithNothing` (tree-only) refresh.
- **Edge cases**: a self-referencing Application object update never refreshes itself (prevents loops, DIFF-087).
- **Configuration surface**: none.
- **Exposure**: internal (controller logs `Requesting app refresh caused by object update` at info for managed and debug for others).
- **Tests required**: positive: Endpoints churn causes no reconciles; child Pod change causes tree-only refresh; managed Deployment spec change causes full compare.

### DIFF-038 ManagedResources API
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1831, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1824, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1448, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.proto#L455, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2264
- **Maturity**: GA
- **Semantics**: `ApplicationService.ManagedResources(ResourcesQuery)` — REST `GET /api/v1/applications/{applicationName}/managed-resources` with optional `namespace, name, version, group, kind, appNamespace, project` query params. Requires RBAC `applications, get` on the app. Returns `items[]` of `ResourceDiff{group, kind, namespace, name, targetState, liveState, normalizedLiveState, predictedLiveState, resourceVersion, modified, hook, diff(deprecated, unset)}` from the controller cache (secrets masked), excluding hooks, filtered by exact match on each non-empty query field (`version` is accepted but not used for filtering).
- **Edge cases**: cache miss -> if the app has ComparisonError/InvalidSpecError conditions those are returned as the error; otherwise a normal refresh is triggered via Get and the cache re-read; permission denied when RBAC fails; liveState/targetState are the literal string `"null"` for missing sides.
- **Configuration surface**: none.
- **Exposure**: API (gRPC + REST) / CLI (`argocd app diff`, `argocd app sync --preview-changes`, `argocd app manifests`, `argocd app patch-resource`, `argocd app delete-resource`, resource actions) / UI (DIFF tab and app diff panel).
- **Tests required**: positive: filter by kind returns only that kind; hooks never returned; authorization: user without `applications, get` gets PermissionDenied; recovery: cache miss triggers refresh and returns data.

### DIFF-039 ServerSideDiff API
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2992, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.proto#L460, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.proto#L322
- **Maturity**: GA
- **Semantics**: `ApplicationService.ServerSideDiff(ApplicationServerSideDiffQuery{appName, appNamespace, project, liveResources[] (ResourceDiff), targetManifests[] (JSON strings)})` — REST `GET /api/v1/applications/{appName}/server-side-diff`. Requires `applications, get`. Builds a diff config with app ignoreDifferences + system overrides, tracking method from settings, no cache, manager `argocd-controller`, server-side diff on, GVK parser from the destination cluster's OpenAPI, `IncludeMutationWebhook` from the app annotation; runs `StateDiffs` over the index-aligned live/target arrays; returns `items[]` (ResourceDiff with `targetState`=predicted, `liveState`=normalized live, metadata from the live entry or target) and `modified` (any). Secret results masked (DIFF-031).
- **Edge cases**: each supplied live object must match its declared name/namespace/group/kind (`InvalidArgument` `name mismatch: expected ... got ...` etc.) and must be one of the app's cached managed resources (`PermissionDenied` `<group>/<kind> <name> not found as part of application <app>`), preventing use of the endpoint against arbitrary cluster objects; empty or `"null"` liveState = create; target parse errors `error unmarshaling target manifest <i>`; dry-run errors propagate; ignoreAggregatedRoles is always false; JQ timeout is the default 1s.
- **Configuration surface**: none.
- **Exposure**: API / CLI (`argocd app diff --server-side-diff`, `argocd app sync --preview-changes` with SSD).
- **Tests required**: positive: modified Deployment returns modified=true; authorization: live resource not in app -> PermissionDenied; spoofed kind -> InvalidArgument; Secret result masked even with spoofed metadata; negative: dry-run admission rejection returns error.

### DIFF-040 `argocd app diff` CLI
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/app_diff.go#L624, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/app_diff.go#L214, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/app_diff.go#L320, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/app_diff.go#L556, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/commands/argocd_app_diff.md#L21
- **Maturity**: GA
- **Semantics**: `argocd app diff APPNAME` fetches the app (optionally `--refresh`/`--hard-refresh`), ManagedResources and settings; target manifests come from (in precedence) `--revisions`+`--source-positions`/`--source-names` (multi-source), `--revision`, `--local` (client-side generation, deprecated, secrets excluded; or `--server-side-generate` with `--local-include` globs, default `*.yaml,*.yml,*.json`, and `--local-repo-root`), else the cached target states. Targets are re-normalized (namespace/tracking/dedup). Hooks and ignored objects are skipped. Diff strategy: server-side when `--server-side-diff` is set, or by default when the app has the `ServerSideDiff=true` annotation (batched API calls, `--server-side-diff-concurrency` default -1 unlimited, 0 invalid; `--server-side-diff-max-batch-kb` default 250), else client-side using app ignoreDifferences + system overrides (ignoreAggregatedRoles false) with `--ignore-normalizer-jq-execution-timeout`. Output per changed resource: header `===== group/kind namespace/name ======` and a unified diff via `diff` or `KUBECTL_EXTERNAL_DIFF`, sorted by key. Exit: `--exit-code` (default true) exits with `--diff-exit-code` (default 1) when diffs exist.
- **Edge cases**: `--source-names` with `--source-positions` -> fatal; revisions count must match positions/names; unknown source name fatal `Unknown source name '<n>'`; source-position out of range fatal; `--server-side-diff` with `--local` requires `--server-side-generate`; explicit `--server-side-diff` on an app without annotation prints a warning; Kubernetes Secrets are excluded from local client-side diffs.
- **Configuration surface**: flags listed; env `KUBECTL_EXTERNAL_DIFF`.
- **Exposure**: CLI (also reused by `argocd app sync --preview-changes`).
- **Tests required**: positive: no diff -> exit 0, diff -> exit 1 (or custom); `--revision` compares to that revision; `--server-side-diff` batches respect max KB; negative: invalid flag combinations exit non-zero with listed messages; authorization: requires `applications, get`.

### DIFF-041 `argocd admin settings resource-overrides` troubleshooting CLI
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/settings.go#L330, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/settings.go#L390, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/settings.go#L440, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/settings.go#L488, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/commands/argocd_admin_settings_resource-overrides.md#L1
- **Maturity**: GA
- **Semantics**: Offline tooling reading `--argocd-cm-path` (or `--load-cluster-settings`; one is required: `either --argocd-cm-path must be provided or --load-cluster-settings must be set to true`). Subcommands: `ignore-differences RESOURCE_YAML_PATH` (applies jsonPointers/jqPathExpressions of all overrides, prints `Following fields are ignored:` + diff, or `No fields are ignored by ignoreDifferences settings` / `Ignore differences are not configured for '<g>/<k>'`), `ignore-resource-updates RESOURCE_YAML_PATH` (same against effective ignoreResourceUpdates incl. merged defaults; flag `--ignore-normalizer-jq-execution-timeout`), `health RESOURCE_YAML_PATH` (prints `STATUS:`/`MESSAGE:` using Lua overrides then built-in Go checks, or `Health script is not configured for '<g>/<k>'`), `list-actions`, `run-action` (actions area).
- **Edge cases**: lookup uses the exact `group/kind` (or `kind`) key — wildcard-only configs print "No overrides configured"/"not configured" even though they would apply at runtime; managedFieldsManagers are not evaluated.
- **Configuration surface**: flags `--argocd-cm-path`, `--argocd-secret-path`, `--load-cluster-settings`.
- **Exposure**: CLI.
- **Tests required**: positive: each subcommand against fixture argocd-cm; negative: missing args prints help and exits 1; no override prints the "not configured" message.

### DIFF-042 UI diff views
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-resources-diff/application-resources-diff.tsx#L17, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/resource-details/resource-details.tsx#L221, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-status-panel/application-status-panel.tsx#L293
- **Maturity**: GA
- **Semantics**: The app-level diff panel (opened from the status panel / "Diff" action) and each resource's DIFF tab render `normalizedLiveState` (a) vs `predictedLiveState` (b) as YAML unified diffs, excluding hooks and resources where both YAML renderings are equal, file path `group/kind/namespace/name`. User preferences: "Compact diff" (2 lines context vs full) and "Inline diff" (unified vs split view), persisted in view preferences.
- **Edge cases**: a resource with `Modified=true` but identical YAML renderings is hidden; when only one resource is shown, no path header.
- **Configuration surface**: browser view preferences `appDetails.compactDiff`, `appDetails.inlineDiff`.
- **Exposure**: UI.
- **Tests required**: positive: modified resource shown, synced resource hidden; toggles change rendering; negative: hook resources never shown.

## Health assessment

### DIFF-043 Health status codes and precedence
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/health/health.go#L16, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/health/health.go#L45, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/health/health.go#L55, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/health.md#L258
- **Maturity**: GA
- **Semantics**: Codes: `Healthy`, `Suspended`, `Progressing`, `Missing`, `Degraded`, `Unknown`. Order from best to worst: Healthy < Suspended < Progressing < Missing < Degraded < Unknown. `IsWorse(current, new)` is true iff new's index is greater than current's. Health is a `{status, message}` pair.
- **Edge cases**: an unrecognized or empty code has index 0 (same as Healthy), so it never worsens an aggregate; Unknown beats Degraded.
- **Configuration surface**: none.
- **Exposure**: CRD (`status.health.status`, `status.resources[].health`) / API / CLI (`argocd app get`, `argocd app wait --health|--degraded|--suspended`) / UI (health icons and filters).
- **Tests required**: table test of IsWorse for all 36 ordered pairs; empty/unknown code never worse than Healthy.

### DIFF-044 Resource health evaluation order
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/health/health.go#L70, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/health/health.go#L104, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/lua/lua.go#L44, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/health.md#L233
- **Maturity**: GA
- **Semantics**: For a live object: (1) if `metadata.deletionTimestamp` is set and the object does not carry finalizer `argocd.argoproj.io/hook-finalizer` -> `Progressing` / `Pending deletion`; (2) health override (Lua, DIFF-057..061): script error -> `Unknown` with the error text as message (and an error returned); non-nil result -> used as is; (3) built-in Go check by group/kind (DIFF-045..056), check error -> `Unknown` with error text; (4) no check -> nil (no health). Lua overrides therefore replace Go checks.
- **Edge cases**: a Lua script returning nil/empty table yields an empty status that still shadows the Go check (DIFF-060); apiVersion mismatch for a Go check (e.g. `apps/v1beta2` Deployment) -> error `unsupported Deployment GVK: ...` -> Unknown; deletion rule precedes everything including Lua.
- **Configuration surface**: Lua overrides via `argocd-cm`.
- **Exposure**: CRD / API / CLI / UI.
- **Tests required**: positive: deleting Deployment -> Progressing "Pending deletion"; hook object with hook finalizer and deletionTimestamp evaluated normally; Lua override for `apps/Deployment` wins over Go; negative: unsupported version -> Unknown with message; recovery: fixing Lua script restores status.

### DIFF-045 Built-in health: apps/Deployment
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/health/health_deployment.go#L28
- **Maturity**: GA
- **Semantics**: Only `apps/v1`. `spec.paused` -> `Suspended` "Deployment is paused". Else if `metadata.generation <= status.observedGeneration`: condition `Progressing` with reason `ProgressDeadlineExceeded` -> `Degraded` `Deployment "<name>" exceeded its progress deadline`; `spec.replicas` set and `updatedReplicas < replicas` -> `Progressing` `Waiting for rollout to finish: <u> out of <r> new replicas have been updated...`; `status.replicas > updatedReplicas` -> `Progressing` `Waiting for rollout to finish: <n> old replicas are pending termination...`; `availableReplicas < updatedReplicas` -> `Progressing` `Waiting for rollout to finish: <a> of <u> updated replicas are available...`; else `Healthy` (empty message). If generation > observedGeneration -> `Progressing` `Waiting for rollout to finish: observed deployment generation less than desired generation`.
- **Edge cases**: checks are evaluated in the listed order; nil `spec.replicas` skips the updated-vs-desired check; paused beats everything.
- **Configuration surface**: none (overridable by Lua).
- **Exposure**: CRD / API / CLI / UI.
- **Tests required**: port `TestDeploymentHealth` fixtures; one case per branch including exact messages.

### DIFF-046 Built-in health: apps/StatefulSet
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/health/health_statefulset.go#L28
- **Maturity**: GA
- **Semantics**: Only `apps/v1`. `observedGeneration == 0` or `generation > observedGeneration` -> `Progressing` "Waiting for statefulset spec update to be observed..."; `spec.replicas` set and `readyReplicas < replicas` -> `Progressing` `Waiting for <n> pods to be ready...`; RollingUpdate with a `rollingUpdate` block: if replicas and partition set and `updatedReplicas < replicas - partition` -> `Progressing` `Waiting for partitioned roll out to finish: <u> out of <r-p> new pods have been updated...`, else `Healthy` `partitioned roll out complete: <u> new pods have been updated...`; OnDelete -> `Healthy` `statefulset has <ready> ready pods`; `updateRevision != currentRevision` -> `Progressing` `waiting for statefulset rolling update to complete <u> pods at revision <rev>...`; else `Healthy` `statefulset rolling update complete <current> pods at revision <rev>...`.
- **Edge cases**: RollingUpdate without a `rollingUpdate` block falls through to the revision comparison; message wording/casing differs per branch and must match exactly.
- **Configuration surface**: none.
- **Exposure**: CRD / API / CLI / UI.
- **Tests required**: port `TestStatefulSetHealth`, `TestStatefulSetOnDeleteHealth`; partition boundary cases.

### DIFF-047 Built-in health: apps/ReplicaSet
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/health/health_replicaset.go#L29
- **Maturity**: GA
- **Semantics**: Only `apps/v1`. If `generation <= observedGeneration`: condition `ReplicaFailure` with status True -> `Degraded` (condition message); else `spec.replicas` set and `availableReplicas < replicas` -> `Progressing` `Waiting for rollout to finish: <a> out of <r> new replicas are available...`; else `Healthy`. If generation > observedGeneration -> `Progressing` `Waiting for rollout to finish: observed replica set generation less than desired generation`.
- **Edge cases**: ReplicaFailure=False ignored.
- **Configuration surface**: none.
- **Exposure**: CRD / API / CLI / UI (mostly as tree children).
- **Tests required**: one case per branch.

### DIFF-048 Built-in health: apps/DaemonSet
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/health/health_daemonset.go#L28
- **Maturity**: GA
- **Semantics**: Only `apps/v1`. `generation > observedGeneration` -> `Progressing` "Waiting for rollout to finish: observed daemon set generation less than desired generation"; update strategy OnDelete -> `Healthy` `daemon set <u> out of <d> new pods have been updated`; `updatedNumberScheduled < desiredNumberScheduled` -> `Progressing` `Waiting for daemon set "<name>" rollout to finish: <u> out of <d> new pods have been updated...`; `numberAvailable < desiredNumberScheduled` -> `Progressing` `Waiting for daemon set "<name>" rollout to finish: <a> of <d> updated pods are available...`; else `Healthy`.
- **Edge cases**: OnDelete reports Healthy even when pods are outdated.
- **Configuration surface**: none.
- **Exposure**: CRD / API / CLI / UI.
- **Tests required**: port `TestDaemonSetOnDeleteHealth`; branch coverage.

### DIFF-049 Built-in health: core Service
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/health/health_service.go#L28
- **Maturity**: GA
- **Semantics**: Only `v1`. `Healthy` unless `spec.type == LoadBalancer` with empty `status.loadBalancer.ingress`, which is `Progressing`. No message.
- **Edge cases**: ingress entries are not inspected for hostname/IP content (docs claim "at least one value"; code only checks list length).
- **Configuration surface**: none.
- **Exposure**: CRD / API / CLI / UI.
- **Tests required**: port `TestServiceHealth`; LB pending vs assigned; ClusterIP always Healthy.

### DIFF-050 Built-in health: Ingress (extensions and networking.k8s.io)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/health/health_ingress.go#L7, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/health/health.go#L117
- **Maturity**: GA
- **Semantics**: Any version of `extensions/Ingress` or `networking.k8s.io/Ingress`: non-empty `status.loadBalancer.ingress` -> `Healthy`, else `Progressing`. No message.
- **Edge cases**: ingress controllers that never publish status keep the Ingress Progressing forever (documented reason to override with Lua).
- **Configuration surface**: none.
- **Exposure**: CRD / API / CLI / UI.
- **Tests required**: port `TestIngressHealth`; both groups.

### DIFF-051 Built-in health: core PersistentVolumeClaim
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/health/health_pvc.go#L28
- **Maturity**: GA
- **Semantics**: Only `v1`. `status.phase`: `Lost` -> `Degraded`; `Pending` -> `Progressing`; `Bound` -> `Healthy`; anything else (including empty) -> `Unknown`. No message.
- **Edge cases**: WaitForFirstConsumer PVCs stay Progressing until bound.
- **Configuration surface**: none.
- **Exposure**: CRD / API / CLI / UI.
- **Tests required**: port `TestPVCHealth`; each phase.

### DIFF-052 Built-in health: core Pod
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/health/health_pod.go#L30
- **Maturity**: GA
- **Semantics**: Only `v1`. If `restartPolicy == Always`: any container (not init) in `waiting` with reason starting `Err` or ending `Error` or `BackOff` -> `Degraded`, message = those waiting messages joined by `, `. Then by phase: `Pending` -> `Progressing` (pod message); `Succeeded` -> `Healthy`; `Failed` -> `Degraded` with pod message, else first container (init then regular) failure message (terminated message, else `OOMKilled`, else `container "<name>" failed with exit code <n>`), else empty; `Running`: Always -> Ready condition true -> `Healthy`, else any container with `lastState.terminated` -> `Degraded`, else `Progressing`; OnFailure/Never -> `Progressing` (hook pods treated as finite). Any other phase -> `Unknown`.
- **Edge cases**: back-off detection deliberately skipped for OnFailure/Never pods so hook pods are not failed prematurely; restartable init containers not considered in waiting check.
- **Configuration surface**: none.
- **Exposure**: CRD / API / CLI / UI (pods usually appear as tree children).
- **Tests required**: port `TestPod` fixtures (pending, running-not-ready, crashloop, imagepullbackoff, error, running restart Always/Never/OnFailure, failed, succeeded, deletion).

### DIFF-053 Built-in health: batch/Job
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/health/health_job.go#L30
- **Maturity**: GA
- **Semantics**: Only `batch/v1`. Scans `status.conditions`: `Failed` -> failed + complete (message from it); `Complete` -> complete; `Suspended` -> complete, and suspended if its status is `True`. Result: not complete -> `Progressing`; failed -> `Degraded` (failed message); suspended -> `Suspended` (message is the failed-message variable, normally empty); else `Healthy` (last Complete/Suspended message).
- **Edge cases**: a `Suspended` condition with status `False` (resumed job) makes the job count as complete and report `Healthy` until another condition appears (parity quirk); the check uses conditions, not `spec.suspend` as docs state; condition statuses other than for Suspended are not checked (a `Complete` condition with status False still counts).
- **Configuration surface**: none.
- **Exposure**: CRD / API / CLI / UI.
- **Tests required**: port `TestJob` (running, failed, succeeded, suspended); add resumed-job (Suspended=False) quirk case.

### DIFF-054 Built-in health: autoscaling/HorizontalPodAutoscaler
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/health/health_hpa.go#L27, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/health/health_hpa.go#L86
- **Maturity**: GA
- **Semantics**: `autoscaling/v1` reads conditions from annotation `autoscaling.alpha.kubernetes.io/conditions` (JSON); `autoscaling/v2` reads `status.conditions`. Degraded if any condition matches (type, reason) in {AbleToScale: FailedGetScale, FailedUpdateScale; ScalingActive: FailedGetResourceMetric, FailedGetObjectMetric, FailedGetPodsMetric, FailedGetExternalMetric, FailedComputeMetricsReplicas, InvalidSelector} (message = condition message); else Healthy if any `AbleToScale` or `ScalingLimited` condition has status `True`; else `Progressing` "Waiting to Autoscale". v1 without annotation or empty list -> Progressing.
- **Edge cases**: invalid annotation JSON -> error `failed to convert conditions annotation to typed: ...` -> Unknown; other versions (e.g. v2beta2) -> `unsupported HPA GVK` -> Unknown; Degraded match ignores condition status.
- **Configuration surface**: none.
- **Exposure**: CRD / API / CLI / UI.
- **Tests required**: port `TestHPA` (9 fixtures incl. v1 without annotations, toofew healthy).

### DIFF-055 Built-in health: apiregistration.k8s.io/APIService
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/health/health_apiservice.go#L14
- **Maturity**: GA
- **Semantics**: `v1` and `v1beta1`: first `Available` condition: status True -> `Healthy`, otherwise `Progressing`; message `<reason>: <message>`. No Available condition -> `Progressing` "Waiting to be processed".
- **Edge cases**: unavailable aggregated API is Progressing, never Degraded.
- **Configuration surface**: none.
- **Exposure**: CRD / API / CLI / UI.
- **Tests required**: port `TestAPIService` (4 fixtures).

### DIFF-056 Built-in health: argoproj.io/Workflow
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/health/health_argo.go#L19
- **Maturity**: GA
- **Semantics**: Any version. `status.phase` empty/`Pending`/`Running` -> `Progressing`; `Succeeded` -> `Healthy`; `Failed`/`Error` -> `Degraded`; other -> `Unknown`; message = `status.message`.
- **Edge cases**: no Go check exists for `argoproj.io/Application` (returns nil; tested) — child Application health requires a Lua override (docs sample).
- **Configuration surface**: none.
- **Exposure**: CRD / API / CLI / UI.
- **Tests required**: port `TestGetArgoWorkflowHealth`, `TestApplication` (nil health).

### DIFF-057 Custom Lua health check via argocd-cm
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/lua/lua.go#L169, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/lua/lua.go#L476, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L1168, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/health.md#L90
- **Maturity**: GA
- **Semantics**: `resource.customizations.health.<group>_<kind>` (core: `.<kind>`) or `health.lua` in the legacy block defines a Lua script; the object is the global `obj` (decoded JSON tree; numbers as Lua numbers); the script returns a table `{status=..., message=...}`. Lookup key is `group/kind` (or `kind` for core); an exact key with non-empty script wins over wildcard and built-in scripts.
- **Edge cases**: script defined but empty string -> treated as absent; the same script is used by the controller (app health, resource tree) and by `admin settings resource-overrides health`; changes apply live via the settings watch (cluster cache invalidated).
- **Configuration surface**: `resource.customizations.health.<group_kind>`, `resource.customizations` YAML `health.lua`.
- **Exposure**: ConfigMap / effects in CRD, API, CLI, UI.
- **Tests required**: port `TestGetHealthScriptWithOverride`, `TestExecuteNewHealthStatusFunction`, `TestLuaHealthScript`; positive: cert-manager Certificate sample from docs; negative: override for other GK not applied.

### DIFF-058 Wildcard Lua health overrides in argocd-cm
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/lua/lua.go#L486, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/health.md#L126
- **Maturity**: GA
- **Semantics**: If no exact override, every override key is treated as a gobwas glob (no separators, `*` crosses `/`) and matched against `group/kind`; the first match with a non-empty script is used along with its `useOpenLibs`. Supported only through the `resource.customizations` YAML block (ConfigMap keys cannot contain `*`).
- **Edge cases**: multiple matching wildcards -> nondeterministic (Go map iteration); wildcard keys never match core kinds unless the pattern has no `/` (e.g. key `*` matches everything including core); `*/*` from `.all` keys only matches grouped kinds.
- **Configuration surface**: `resource.customizations` keys such as `ec2.aws.crossplane.io/*`, `"*.aws.crossplane.io/*"`.
- **Exposure**: ConfigMap.
- **Tests required**: port `TestGetHealthScriptWithKindWildcardOverride`, `TestGetHealthScriptWithGroupWildcardOverride`, `TestGetHealthScriptWithGroupAndKindWildcardOverride`, `TestExecuteWildcardHealthStatusFunction`; Patrol must document deterministic tie-breaking or reproduce "first found".

### DIFF-059 Lua sandbox, useOpenLibs and execution limits
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/lua/lua.go#L75, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/lua/oslib_safe.go#L16, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L1170, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/health.md#L158
- **Maturity**: GA
- **Semantics**: Each evaluation creates a fresh gopher-lua state with a 1-second context timeout. Always loaded: `package`, base, `table`, and a safe OS module (only `time`, `date`) preloaded so `local os = require("os")` works. Standard libraries (string, math, io, full os, etc.) are loaded only when `useOpenLibs` is true: `resource.customizations.useOpenLibs.<group_kind>: true` (parsed with ParseBool) or `health.lua.useOpenLibs` in the legacy block. All embedded (built-in) scripts run with open libs. Global `actionParams` is an empty table for health scripts.
- **Edge cases**: calling a disabled library (e.g. `os.getenv`) -> Lua API error -> Unknown; infinite loop -> context timeout error -> Unknown; Lua runtime error messages have the stack trace stripped; invalid useOpenLibs value -> settings load error.
- **Configuration surface**: `resource.customizations.useOpenLibs.<group_kind>`.
- **Exposure**: ConfigMap.
- **Tests required**: port `TestFailExternalLibCall`, `TestHandleInfiniteLoop`; positive: `string.find` works with useOpenLibs=true and fails without; `require("os").time()` works without open libs.

### DIFF-060 Lua health result contract
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/lua/lua.go#L132, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/lua/lua.go#L590
- **Maturity**: GA
- **Semantics**: Return value handling: table -> JSON-encoded and decoded into `{status, message}`; a status outside the six valid codes -> `Unknown` with message `Lua returned an invalid health status`; empty table (encodes as JSON array) -> empty HealthStatus; `nil` or no return -> empty HealthStatus; any other type -> error `expect table output from Lua script, not <type>` -> Unknown.
- **Edge cases**: empty HealthStatus shadows the Go check and contributes nothing to app health (empty code never worsens aggregate) while the tree shows no status; extra table keys are ignored.
- **Configuration surface**: none.
- **Exposure**: indirect.
- **Tests required**: port `TestFailLuaReturnNonTable`, `TestInvalidHealthStatusStatus`, `TestNoReturnHealthStatusStatus`, `TestNilHealthStatusStatus`, `TestEmptyHealthStatusStatus`.

### DIFF-061 Built-in Lua health lookup (embedded resource_customizations)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/lua/lua.go#L186, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/lua/lua.go#L518, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/lua/lua.go#L568, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/embed.go#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/health.md#L204
- **Maturity**: GA
- **Semantics**: After argocd-cm overrides, `resource_customizations/<group>/<kind>/health.lua` (core: `<kind>/health.lua`) is used if present. Otherwise wildcard directories (any group/kind path containing `_`, where `_` is replaced by `*` and matched with doublestar path semantics, so `*` does not cross `/`) are evaluated in lexicographic order of their directory path; the first match wins. Shipped inventory at this commit: 111 group directories, 247 group/kind directories, 238 `health.lua`, 36 kinds with `actions/` (see DIFF-103 onward); wildcard dirs: `_.cnrm.cloud.google.com/_`, `_.crossplane.io/_`, `_.upbound.io/_`, `grafana-org-operator.kubitus-project.gitlab.io/_`, `operator.victoriametrics.com/_`.
- **Edge cases**: invalid wildcard pattern -> error `error while fetching built-in health script: ...` -> Unknown; wildcard scripts apply only when no exact built-in exists; the glob list is computed once per process.
- **Configuration surface**: none (embedded).
- **Exposure**: indirect.
- **Tests required**: port `TestGetHealthScriptPredefined`, `TestGetHealthScriptNoPredefined`, `Test_getHealthScriptPaths`, and every `resource_customizations/**/health_test.yaml` (driven by `util/lua/health_test.go`); positive: `pkg.crossplane.io/Provider` uses its exact script, `ec2.aws.crossplane.io/Instance` uses `_.crossplane.io/_`.

### DIFF-062 Aggregate application health computation
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/health.go#L20, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L1051, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/health.md#L258
- **Maturity**: GA
- **Semantics**: Start `Healthy`. For each compared resource (immediate managed resources only; children are not inherited): skip if target is `hook: Skip`, or live is a hook or ignored hook. Mark "contains resources" and "contains live". Skip if live has `argocd.argoproj.io/ignore-healthcheck: "true"` (DIFF-063). Missing live -> health `Missing` (recorded but does not affect aggregate); live that is this Application itself (UID/name/namespace/group/kind match) is skipped; otherwise evaluate DIFF-044. nil health is skipped. Child `argoproj.io/Application` resources with `Missing` or `Unknown` do not affect the parent. Aggregate = worst per DIFF-043. Finally, if the app has non-hook resources but none exist live, health is at least `Missing`. Health errors: the first error is reported as a `ComparisonError` condition `error setting app health: failed to get resource health for "<Kind>" with name "<n>" in namespace "<ns>": ...` (Unknown health still aggregated). If project/spec conditions have errors, the controller sets app health `Unknown`.
- **Edge cases**: app with zero resources (or only hooks) is `Healthy`; all resources missing -> `Missing`; one Missing + one Healthy -> `Healthy`; degraded extraneous resource degrades the app (extraneous objects are evaluated); the "N other errors" message branch is unreachable in code (errCount never exceeds 1).
- **Configuration surface**: none.
- **Exposure**: CRD `status.health.status` / API / CLI (`argocd app get`, `argocd app wait --health`) / UI (app tile, filters).
- **Tests required**: port `TestSetApplicationHealth*` (NoResource, OnlyHooks, MissingResource, MissingResource_WithIgnoreHealthcheck, MissingResource_WithChildApp, AllMissingResources, AllMissingResources_WithHooks, HealthImproves, ResourceHealthNotPersisted) and `TestChildAppHealth`.

### DIFF-063 ignore-healthcheck annotation
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L226, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/health.go#L41, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/health.md#L292
- **Maturity**: GA
- **Semantics**: A managed resource whose *live* object has annotation `argocd.argoproj.io/ignore-healthcheck` equal to the exact string `"true"` is excluded from app health aggregation and its health is not written to `status.resources[].health`; it still counts as an existing live resource for the all-missing rule.
- **Edge cases**: annotation only in Git but not yet applied has no effect; `"True"` is not recognized; resource tree nodes still show the resource's own health.
- **Configuration surface**: resource annotation.
- **Exposure**: resource metadata / CRD.
- **Tests required**: positive: Degraded annotated Deployment -> app Healthy; negative: value `True` ignored.

### DIFF-064 Persisted resource health vs tree health
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/health.go#L68, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/health.go#L95, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2905, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1205, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-application-controller/commands/argocd_application_controller.go#L293
- **Maturity**: GA
- **Semantics**: Controller flag `--persist-resource-health` (env `ARGOCD_APPLICATION_CONTROLLER_PERSIST_RESOURCE_HEALTH`, cmd-params `controller.resource.health.persist`, default false). When true, each `status.resources[i].health` holds `{status, message}` and `status.resourceHealthSource` is empty (inline). When false, `status.resources[i].health` is nil and `status.resourceHealthSource: appTree`; the API server fills `status.resources[].health` from the cached resource tree in Get (with and without refresh), Watch, Sync and Rollback responses (https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L801, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L890, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1281, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2090, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2271), not in List (https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L288-L346): node health by key, or `Missing` "Resource has not been created" for nodes without UID/resourceVersion/createdAt.
- **Edge cases**: tree cache miss -> resources returned without health; resources with ignored health remain nil.
- **Configuration surface**: flag/env/cmd-params listed.
- **Exposure**: CRD (`status.resourceHealthSource`) / API (Get/Watch/Sync/Rollback) / CLI / UI.
- **Tests required**: positive: default mode -> CR has no per-resource health but API responses do; persist mode -> CR has health; Missing node gets the "Resource has not been created" message; recovery: switching modes converges on next reconcile.

### DIFF-065 Application health transition time and events
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2334, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1954
- **Maturity**: GA
- **Semantics**: When the persisted app health code changes, `status.health.lastTransitionTime` is set to now and a Normal event `ResourceUpdated` `Updated health status: <old> -> <new>` is emitted; otherwise the previous lastTransitionTime is preserved. `status.health.message` is deprecated and not set. Sync status changes emit `Updated sync status: <old> -> <new>`.
- **Edge cases**: first reconcile from empty status counts as a transition.
- **Configuration surface**: `--enable-k8s-event` (events area).
- **Exposure**: CRD / Kubernetes events / UI events tab.
- **Tests required**: positive: Healthy->Degraded sets new timestamp and event; unchanged health keeps timestamp.

### DIFF-066 Per-resource health in cluster cache and tree
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/cache.go#L562, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/cache.go#L307
- **Maturity**: GA
- **Semantics**: Every watched object (managed or not) gets its health computed on each cache update with the same override chain (DIFF-044); errors are dropped (health nil). The value is exposed as `ResourceNode.health` in the resource tree (managed, children and orphans) and drives update suppression (DIFF-033).
- **Edge cases**: child health never propagates to parents (by design); Lua config changes re-evaluate after cache invalidation.
- **Configuration surface**: Lua overrides.
- **Exposure**: API (ResourceTree) / UI (tree icons) / CLI (`argocd app resources --output tree=detailed`).
- **Tests required**: positive: unhealthy Pod under healthy Deployment shows Degraded node while app stays Healthy; negative: health error leaves node without health.

## Resource selection and discovery

### DIFF-067 resource.exclusions
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L881, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/filtered_resource.go#L5, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/resources_filter.go#L64, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/utils/kube/ctl.go#L88, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/declarative-setup.md#L1348
- **Maturity**: GA
- **Semantics**: `argocd-cm` `resource.exclusions` is a YAML list of `{apiGroups[], kinds[], clusters[]}`. A rule matches when any apiGroups glob matches the group (empty list = all), any kind equals the kind or is `*` (empty = all; kinds are not globbed), and any clusters glob matches the cluster API server URL (empty = all). Matching group/kinds are not discovered/watched in that cluster (API resources filtered at discovery), never appear in trees, and are not compared. Exclusions always win over inclusions.
- **Edge cases**: invalid glob -> that pattern never matches (logged), effectively ignoring the rule; invalid YAML -> settings error; resources already cached remain visible until cache invalidation/restart (docs); target objects of excluded kinds produce DIFF-070.
- **Configuration surface**: `resource.exclusions`.
- **Exposure**: ConfigMap / effects on CRD conditions, tree.
- **Tests required**: port `resources_filter_test.go` and `filtered_resource_test.go` matrices; positive: exclude `Snapshot` for `*.local` clusters only; negative: kind glob `Snap*` does not match.

### DIFF-068 resource.inclusions and filter precedence
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L872, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/resources_filter.go#L42, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/declarative-setup.md#L1385
- **Maturity**: GA
- **Semantics**: `resource.inclusions` uses the same rule shape. Decision for (group, kind, cluster): excluded (core list + exclusions) -> excluded; else matched by an inclusion -> included; else if any inclusion rule applies to this cluster (its clusters globs match, or it has no clusters) -> excluded; else included. So inclusions turn the cluster into allow-list mode only for clusters they target.
- **Edge cases**: an inclusion rule with no `clusters` makes every cluster allow-list only; truth table documented in code comments must be reproduced exactly.
- **Configuration surface**: `resource.inclusions`.
- **Exposure**: ConfigMap.
- **Tests required**: all nine rows of the truth table at resources_filter.go L42-L63.

### DIFF-069 Always-excluded core resources
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/resources_filter.go#L7
- **Maturity**: GA
- **Semantics**: Independent of configuration, these are excluded everywhere: API groups `events.k8s.io` and `metrics.k8s.io` (all kinds), core `Event`, and `coordination.k8s.io/Lease`.
- **Edge cases**: cannot be re-included via inclusions; Events shown in the UI come from the events API, not the cache.
- **Configuration surface**: none.
- **Exposure**: indirect.
- **Tests required**: positive: Lease in Git -> ExcludedResourceWarning and not compared; inclusion of `coordination.k8s.io/Lease` still excluded.

### DIFF-070 ExcludedResourceWarning for target objects
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L753, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1914
- **Maturity**: GA
- **Semantics**: Each desired object whose group/kind is excluded for the destination cluster is dropped from comparison and a condition `ExcludedResourceWarning` `Resource <group>/<kind> <name> is excluded in the settings` is added.
- **Edge cases**: the condition is recalculated each compare (cleared when the rule is removed).
- **Configuration surface**: exclusions/inclusions.
- **Exposure**: CRD conditions / UI / CLI.
- **Tests required**: positive: excluded ConfigMap in Git yields the condition and the app stays Synced; recovery: removing exclusion clears condition and the resource is compared.

### DIFF-071 resource.respectRBAC
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L654, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/cache/cluster.go#L1035, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/cache/cluster.go#L1040, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/cache/cluster.go#L1177, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/declarative-setup.md#L1419
- **Maturity**: GA
- **Semantics**: `argocd-cm` `resource.respectRBAC`: empty -> disabled (a forbidden list fails the cluster sync); `normal` -> a list call returning Forbidden/Unauthorized removes that group/kind from the watch set silently; `strict` -> on Forbidden/Unauthorized a SelfSubjectAccessReview for verb `list` (namespace `*`, or each managed namespace in namespaced mode) is created; if not allowed the API is dropped. Applied at initial sync and when starting missing watches.
- **Edge cases**: any other value -> error `invalid value for resource.respectRBAC: <v>` and the cluster cache cannot be created (cluster unavailable); strict mode where SSAR says allowed but list was forbidden -> cluster sync error `failed to load initial state of resource ...`; SSAR creation failure -> `failed to check permissions for resource ...`; the value is read when a cluster cache is created, so changes need a controller restart (not part of the settings watch); strict needs RBAC to create SelfSubjectAccessReviews.
- **Configuration surface**: `resource.respectRBAC: normal|strict`.
- **Exposure**: ConfigMap.
- **Tests required**: positive: controller without Secret list permission + normal -> cluster syncs, Secrets absent; strict -> SSAR issued; negative: invalid value -> cluster error; disabled -> sync failure on forbidden.

### DIFF-072 API discovery and watched resource set
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/utils/kube/ctl.go#L62, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/utils/kube/ctl.go#L168, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/cache/cluster.go#L1094
- **Maturity**: GA
- **Semantics**: Cluster sync: server version, full API resource list (for scope/IsNamespaced info) and OpenAPI schema/GVK parser are loaded; the watched set is the *preferred* version of each group/kind that supports both `list` and `watch` verbs and is not excluded. Each API is listed with paging and then watched. `IsNamespaced` answers from discovered APIs; unknown group/kinds return NotFound (callers treat as namespaced-or-unknown).
- **Edge cases**: partial discovery failure is tolerated if at least one resource list is returned (logged `Partial success when performing preferred resource discovery`); zero resources -> `failed to discover server resources, zero resources returned`; OpenAPI load failure fails the cluster sync.
- **Configuration surface**: exclusions/inclusions; respectRBAC.
- **Exposure**: indirect (cluster info in Cluster API, other area).
- **Tests required**: positive: kind without watch verb not cached; preferred version used for listing; negative: discovery outage -> cluster sync error and apps get ComparisonError.

### DIFF-073 Dynamic API discovery on CRD and APIService changes
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/cache/cluster.go#L859, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/cache/cluster.go#L908, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/cache/cluster.go#L958, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/cache/cluster.go#L995
- **Maturity**: GA
- **Semantics**: CRD watch events: Added -> new group/kind versions appended to API list and missing watches started; Deleted -> API removed; any CRD event reloads the OpenAPI schema. APIService events: on delete, or when the `Available` condition is `True`, discovery is re-run asynchronously with exponential backoff (500ms, factor 1.5, 9 steps, ~25s) until the APIService's group is watched; otherwise picked up at next resync.
- **Edge cases**: CRD conversion errors logged (`Failed to extract CRD resources`); aggregated APIs whose server is down at startup appear once Available.
- **Configuration surface**: none.
- **Exposure**: indirect.
- **Tests required**: positive: installing a CRD after startup makes its CRs appear in trees without restart; aggregated API becoming Available starts watches; negative: deleting the CRD removes the watch.

### DIFF-074 Live object version conversion for comparison
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/cache/cluster.go#L1589
- **Maturity**: GA
- **Semantics**: Each live object matched to a target is converted to the target's group/version before diffing (e.g. desired `autoscaling/v2` vs cached preferred version). If local conversion fails, the object is fetched from the API server in the target's GVK. Objects whose group/kind is not watched (e.g. excluded by RBAC or newly created) but are referenced by targets are fetched with a direct GET.
- **Edge cases**: NotFound on fallback GET -> treated as missing (no error); other errors -> `unexpected error getting managed object: ...` -> ComparisonError `Failed to load live state: ...`.
- **Configuration surface**: none.
- **Exposure**: indirect.
- **Tests required**: positive: HPA in Git as v2 with v1 preferred compares cleanly; negative: API error surfaces as ComparisonError.

### DIFF-075 Namespaced-mode cluster constraints
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/cache/cluster.go#L1015, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/cache/cluster.go#L1537
- **Maturity**: GA
- **Semantics**: When a cluster has `namespaces` set, namespaced APIs are listed/watched per listed namespace and cluster-scoped APIs only if `clusterResources: true`. Comparison then fails for targets outside that scope: `cluster level <Kind> "<name>" can not be managed when in namespaced mode` or `namespace "<ns>" for <Kind> "<name>" is not managed`.
- **Edge cases**: errors are returned as `Failed to load live state: failed to get managed objects...` ComparisonError and sync Unknown.
- **Configuration surface**: Cluster secret `namespaces`, `clusterResources` (cluster area).
- **Exposure**: CRD conditions.
- **Tests required**: positive: namespaced-mode cluster with namespace in list compares; negative: ClusterRole target without clusterResources -> condition text exact.

### DIFF-076 Managed live object selection and shared-resource detection
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/cache/cluster.go#L1533, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/cache.go#L729, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/cache.go#L582, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L804
- **Maturity**: GA
- **Semantics**: The app's live set = all cached objects whose tracking-derived app instance name equals this app's instance name, that have no ownerReferences (including synthesized ones) and whose full manifest is cached (full manifests are cached only for tracked root objects and all CRDs) — plus, for each target key not already included, the cached object with that key (regardless of tracking) or a direct GET when the kind is not watched. Live objects tracked to a different app produce `SharedResourceWarning` `<Kind>/<name> is part of applications <this app> and <other app>` (`_` in instance name rendered as `/`).
- **Edge cases**: tracked objects that have an ownerReference are never extraneous (not pruned); untracked objects with a target are adopted for comparison; CRDs are always available for diffing even without tracking.
- **Configuration surface**: tracking settings (other area).
- **Exposure**: CRD (`status.resources`, conditions) / UI.
- **Tests required**: positive: tracked root object not in Git -> extraneous; object tracked by app B and declared in app A -> SharedResourceWarning; negative: owned child with copied tracking label not listed as managed.

### DIFF-077 Project permission filtering during comparison
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L784, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L1001, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/app_project_types.go#L399
- **Maturity**: GA
- **Semantics**: Live objects not permitted by the AppProject (group/kind/name allow/deny lists and destination namespace/cluster) are removed from the live set (so they are not extraneous/pruned). After diffing, any resource whose group/kind/name is not permitted gets sync status `Unknown`. Project permission check errors add ComparisonError `Failed to check if live resource "<key>" is permitted in project "<p>": ...` and mark load failure.
- **Edge cases**: cluster-scoped kinds require an explicit cluster whitelist (empty whitelist denies all); namespaced kinds allowed when whitelist is nil.
- **Configuration surface**: AppProject `namespaceResourceWhitelist/Blacklist`, `clusterResourceWhitelist/Blacklist`, destinations (project area).
- **Exposure**: CRD status.
- **Tests required**: positive: denied kind in Git -> resource Unknown; denied live extraneous object not listed; negative: permission lookup error -> Unknown everywhere.

### DIFF-078 Orphaned resources monitoring
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L584, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L646, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L436, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2806, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/orphaned-resources.md#L1
- **Maturity**: GA
- **Semantics**: Enabled per AppProject by setting `spec.orphanedResources` (any non-nil value). For each app in the project: top-level (no owner) resources in the app's destination namespace that are not managed by this app, are namespaced, permitted by the project, and not excepted (DIFF-079) are traversed with their children; nodes attributed to another existing Application (by tracking) are skipped; the rest form `tree.orphanedNodes` (sorted). If `orphanedResources.warn` is true and at least one orphan exists, condition `OrphanedResourceWarning` `Application has <n> orphaned resources` is set (otherwise cleared). A per-app orphaned-resources metric is updated. Unmanaged namespaced object updates trigger tree-only refreshes of apps whose project monitors orphans in that namespace.
- **Edge cases**: `orphanedResources: {}` enables detection without warnings (warn defaults false); when disabled, no orphan computation (`warnOrphaned` irrelevant); cluster-scoped objects are never orphans; heavy namespaces impact performance (docs warning). CLI: `proj set` with either flag rebuilds `spec.orphanedResources` from scratch (`--orphaned-resources=false` without `--orphaned-resources-warn` sets it to nil, disabling monitoring; any rebuild drops an existing `ignore` list) — https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/util/project.go#L144, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/util/project.go#L206.
- **Configuration surface**: AppProject `spec.orphanedResources.warn`, `.ignore[]`.
- **Exposure**: CRD (project spec, app conditions) / API (ResourceTree `orphanedNodes`; Project Create/Update) / CLI (`argocd app resources --orphaned`; `argocd proj create|set --orphaned-resources [--orphaned-resources-warn]`) / UI ("Show Orphaned" filter in app details; project settings orphaned-resources section with warn toggle).
- **Tests required**: positive: unmanaged ConfigMap in destination namespace listed as orphan; warn=true sets condition with count; negative: resource owned by another app not listed; recovery: deleting orphan clears condition.

### DIFF-079 Orphaned resource exceptions and ignore rules
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L544, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/orphaned-resources.md#L39
- **Maturity**: GA
- **Semantics**: Never orphans: core `Service` `kubernetes` in namespace `default`; core `ServiceAccount` named `default` (any namespace; its token Secrets are children via DIFF-084); core `ConfigMap` `kube-root-ca.crt`; resources denied by the project. User rules `spec.orphanedResources.ignore[] {group, kind, name}`: a rule matches when kind is empty or glob-matches, group glob-matches (empty group matches only core), and name is empty or glob-matches.
- **Edge cases**: omitting `group` does not mean "any group" (must use `*`); invalid globs never match.
- **Configuration surface**: AppProject `spec.orphanedResources.ignore`.
- **Exposure**: CRD / API (Project Update) / CLI (`argocd proj add-orphaned-ignore PROJECT GROUP KIND [--name PATTERN]` — fatal `Specified resource is already defined in the orphaned ignore list of project` on duplicates; `argocd proj remove-orphaned-ignore PROJECT GROUP KIND [--name]` — fatal `Specified resource does not exist in the orphaned ignore list of project` when absent) / UI (project settings list).
- **Tests required**: positive: `kind: Secret, name: "*.example.com"` ignored; built-in exceptions; negative: `kind: ConfigMap` without group ignores only core ConfigMaps.

## Resource tree and relationships

### DIFF-080 Resource tree composition
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L567, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L510, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/cache.go#L274, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2025
- **Maturity**: GA
- **Semantics**: After each comparison the controller builds `ApplicationTree{nodes, orphanedNodes, hosts, shardsCount}`: managed resources missing live become nodes with group/kind/name/namespace, version from target and health `Missing` (no UID); existing ones are traversed with all descendants (DIFF-081/082), each node converted to `ResourceNode{group, version, kind, namespace, name, uid, parentRefs[], info[], networkingInfo, resourceVersion, images[], health, createdAt}`; descendants not permitted by the project are pruned from the traversal. Nodes, orphans and hosts are sorted (`group/kind/namespace/name`, host name). Parent refs carry group/kind/version/name/uid and namespace = child's namespace if the parent is found there, else `""` (cluster-scoped).
- **Edge cases**: tree and managed-resources cache are written together; on project/spec errors the tree is reset to empty.
- **Configuration surface**: none.
- **Exposure**: API (ResourceTree, WatchResourceTree) / CLI / UI (tree, list, network, pods views).
- **Tests required**: port `Test_asResourceNode_owner_refs`, `Test_asResourceNode_cross_namespace_parent`, `Test_asResourceNode_same_namespace_parent`; positive: Deployment -> ReplicaSet -> Pod chain; missing resource node has Missing health and no UID.

### DIFF-081 ownerReference hierarchy traversal
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/cache/cluster.go#L1284, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/cache/cluster.go#L1460, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/cache/resource.go#L38, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/cache/resource.go#L79
- **Maturity**: GA
- **Semantics**: Within a namespace a parent->children graph is built from ownerReferences matched by UID; synthesized owner refs without UID are back-filled by looking up `group/kind/name` in the same namespace. Depth-first traversal from the requested keys calls the action once per resource; returning false stops descent below that node. If two children share a UID (same object served under two API groups) the one with the lexicographically smaller key string is kept deterministically.
- **Edge cases**: circular ownership is detected (`circular dependency detected. <child> is child and parent of <parent>`) and the cycle is not re-entered; unparsable owner apiVersion skips that ref; keys not in the cache are ignored.
- **Configuration surface**: none.
- **Exposure**: indirect (tree).
- **Tests required**: positive: multi-level chain; duplicate-UID child chosen deterministically; negative: self-owning object does not loop.

### DIFF-082 Cross-namespace children of cluster-scoped parents
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/cache/cluster.go#L1329, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/cache/cluster.go#L543
- **Maturity**: GA
- **Semantics**: A parent-UID -> children index lets cluster-scoped resources (e.g. Crossplane Provider -> ProviderRevision -> Deployment) own namespaced children across namespaces; traversal from cluster-scoped keys follows the index, recursing through cluster-scoped intermediates, and children found this way are traversed normally within their namespace.
- **Edge cases**: a guard map prevents infinite recursion on circular cluster-scoped ownership; namespaced parents cannot own children in other namespaces (Kubernetes disallows).
- **Configuration surface**: none.
- **Exposure**: tree.
- **Tests required**: positive: cluster-scoped CR owning a namespaced Deployment shows it as child; multi-level cluster-scoped chain; negative: cycle terminates.

### DIFF-083 Synthesized relation: Endpoints -> Service
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/cache/references.go#L28
- **Maturity**: GA
- **Semantics**: A core `Endpoints` object with no ownerReferences gets a synthesized owner `v1/Service` with the same name (UID back-filled at traversal), so it appears under its Service.
- **Edge cases**: Endpoints with explicit owners keep them; because it has an owner, an Endpoints object is never a managed root nor an orphan; Endpoints updates never trigger app refresh (DIFF-037). EndpointSlices have real ownerReferences and need no synthesis.
- **Configuration surface**: none.
- **Exposure**: tree.
- **Tests required**: positive: Service node has Endpoints child.

### DIFF-084 Synthesized relation: ServiceAccount token Secret -> ServiceAccount
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/cache/references.go#L86
- **Maturity**: GA
- **Semantics**: Core Secrets of type `kubernetes.io/service-account-token` with annotations `kubernetes.io/service-account.name` and `kubernetes.io/service-account.uid` get an owner ref to `v1/ServiceAccount` with that name and UID.
- **Edge cases**: missing either annotation -> no relation; the token Secret is then not top-level (not orphan, not managed root).
- **Configuration surface**: none.
- **Exposure**: tree.
- **Tests required**: positive: SA node with token Secret child; negative: opaque Secret unaffected.

### DIFF-085 Synthesized relation: StatefulSet -> PersistentVolumeClaim
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/cache/references.go#L62, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/cache/cluster.go#L485
- **Maturity**: GA
- **Semantics**: For `apps`/`extensions` StatefulSets, any core PVC in the same namespace whose name matches regex `<template>-<statefulset>-\d+$` for one of its `volumeClaimTemplates` is given an inferred owner ref to the StatefulSet (maintained whenever either object is added/updated/removed).
- **Edge cases**: regex is not anchored at start, and template/sts names are not escaped (names with regex metacharacters behave as regex); PVCs gaining this owner stop being top-level (not orphans, not extraneous); unmarshal failure logged `Failed to extract StatefulSet ... PVC references`.
- **Configuration surface**: none.
- **Exposure**: tree.
- **Tests required**: positive: `data-web-0` under StatefulSet `web` with template `data`; negative: `data-web-x` not matched; recovery: deleting the StatefulSet removes the inferred refs.

### DIFF-086 Synthesized relation: OLM ClusterServiceVersion -> OperatorGroup
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/cache/references.go#L36
- **Maturity**: GA
- **Semantics**: `operators.coreos.com/ClusterServiceVersion` with annotation `olm.operatorGroup` gets an additional owner ref to `operators.coreos.com/v1` `OperatorGroup` of that name.
- **Edge cases**: added on top of existing ownerReferences; missing OperatorGroup -> ref unresolved (no parent node).
- **Configuration surface**: none.
- **Exposure**: tree.
- **Tests required**: positive: CSV shown under OperatorGroup.

### DIFF-087 Application attribution and refresh triggering
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/cache.go#L356, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/cache.go#L593, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L378, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/cache/cluster.go#L457
- **Maturity**: GA
- **Semantics**: A cached object is attributed to an app if it is a root (no owner refs at creation, including synthesized) with tracking pointing to the app; otherwise the app is inherited by walking same-namespace owners recursively (first owner chain that yields an app wins). On every cache update (unless suppressed by DIFF-033/037), the old and new objects' apps are collected; each app gets a refresh request at level CompareWithRecent if the object is its root node, else ComparisonWithNothing. Updates to an Application object that is the app itself never trigger its own refresh. Apps owned by another shard or outside allowed namespaces are skipped.
- **Edge cases**: circular owners -> warning `Circular dependency detected` and recursion stops; tracking on a non-root object is ignored for attribution.
- **Configuration surface**: tracking settings.
- **Exposure**: internal (controller behavior, logs).
- **Tests required**: port `Test_getAppRecursive`; positive: Pod update refreshes owning app tree-only; managed Service update triggers full compare.

### DIFF-088 ResourceTree and WatchResourceTree APIs and `argocd app resources`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1638, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1647, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.proto#L465, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/app_resources.go#L482, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/cache/appstate/cache.go#L18
- **Maturity**: GA
- **Semantics**: `ResourceTree(ResourcesQuery)` — `GET /api/v1/applications/{applicationName}/resource-tree` returns the cached ApplicationTree; `WatchResourceTree` — `GET /api/v1/stream/applications/{applicationName}/resource-tree` streams the tree on every cache change. Both require `applications, get`. Cache miss behaves as in DIFF-038 (`Cache Key is missing` hint about `--redis-compress` printed server-side). The tree is stored in Redis, optionally sharded by env `ARGOCD_APPLICATION_TREE_SHARD_SIZE` (default 0 = unsharded, max 1000; first shard carries `shardsCount`). CLI `argocd app resources APPNAME` prints `GROUP KIND NAMESPACE NAME ORPHANED`: by default top-level managed nodes (no parentRefs) plus all orphaned nodes; `--orphaned` only orphans; `--output tree` hierarchical; `--output tree=detailed` adds AGE, HEALTH, REASON; `--project` for not-found semantics; `-N` app namespace.
- **Edge cases**: `--orphaned=false` explicitly set lists only non-orphaned resources; streamed tree updates are coalesced by the cache layer.
- **Configuration surface**: env `ARGOCD_APPLICATION_TREE_SHARD_SIZE`.
- **Exposure**: API / CLI / UI (application details page subscribes to the stream).
- **Tests required**: positive: tree returned with nodes/orphans/hosts; stream emits after resource change; CLI output formats; authorization: denied without `applications, get`.

## Resource info enrichment

### DIFF-089 Pod info (status reason, containers, restarts, images, requests)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/info.go#L619
- **Maturity**: GA
- **Semantics**: For core Pods (kubectl-like): reason starts as phase (or `status.reason`), `SchedulingGated` if PodScheduled condition has that reason; init containers: first non-completed sets `Init:<reason>`, `Init:Signal:<n>`, `Init:ExitCode:<n>` or `Init:<i>/<total>` (restartable sidecars that started count as ready containers); if initialized, container statuses (last to first) set reason from waiting/terminated reason, `Signal:<n>`/`ExitCode:<n>`; `Completed` with a running container becomes `Running`; deletion with `NodeLost` -> `Unknown`, deletion of non-terminal pod -> `Terminating`. Info items: `Status Reason`, `Node`, `Containers` `<ready>/<total>` (total includes restartable init containers), `Restart Count` (if > 0, init + regular), and for non-terminal pods `cpu` and `memory` requests in milli-units. Images = unique init + regular container images. `networkingInfo.labels` = pod labels.
- **Edge cases**: conversion failure -> no info; requests computed with status resources (in-place resize aware).
- **Configuration surface**: none.
- **Exposure**: API (tree node `info`, `images`) / UI (pod tiles, pods view) / CLI (`tree=detailed` REASON).
- **Tests required**: port `TestGetPodInfo` and init/sidecar cases (kubectl parity table).

### DIFF-090 Service networking info
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/info.go#L117, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/info.go#L99
- **Maturity**: GA
- **Semantics**: Core Services: `networkingInfo.targetLabels` = `spec.selector`; for `LoadBalancer` type, `ingress` = `status.loadBalancer.ingress` entries (hostname preferred over IP per entry); external URLs from link annotations preserved.
- **Edge cases**: non-LB services have empty ingress list.
- **Configuration surface**: link annotations (DIFF-095).
- **Exposure**: API / UI network view.
- **Tests required**: port `TestGetServiceInfo`, `TestGetLinkAnnotatedServiceInfo`, `TestMaliciousLinkAnnotatedServiceInfoFiltered`.

### DIFF-091 Ingress networking info and external URLs
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/info.go#L149, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/info.go#L132
- **Maturity**: GA
- **Semantics**: `extensions` and `networking.k8s.io` Ingress: target refs = Services from `spec.backend` and every rule path backend (`serviceName` for extensions and networking v1beta1, `service.name` for v1); for each rule path, host = rule host or first LB ingress hostname/IP; scheme `https` if any `spec.tls` entry has `secretName` or a `host` equal to the rule host, else `http`; URL = `<scheme>://<host><path without trailing *>`. URLs added unless `argocd.argoproj.io/ignore-default-links: "true"`. `ingress` = LB status.
- **Edge cases**: `tls[].hosts` is a list but the code type-asserts it as a map, so host lists alone never switch to https (secretName presence does) — parity quirk; rules without host and without LB status produce no URL; duplicates de-duplicated.
- **Configuration surface**: `argocd.argoproj.io/ignore-default-links`.
- **Exposure**: API / UI (external link icons) / app summary.
- **Tests required**: port `TestGetIngressInfo`, `TestGetLinkAnnotatedIngressInfo`, `TestGetIgnoreDefaultLinksIngressInfo`, `TestGetIngressInfoWildCardPath`, `TestGetIngressInfoWithoutTls`, `TestGetIngressInfoWithHost`, `TestGetIngressInfoNoHost`, `TestExternalUrlWithSubPath`, `TestExternalUrlWithMultipleSubPaths`, `TestExternalUrlWithNoSubPath`, `TestExternalUrlWithNetworkingApi`.

### DIFF-092 Gateway API Gateway info
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/info.go#L450, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/info.go#L369
- **Maturity**: GA
- **Semantics**: `gateway.networking.k8s.io/Gateway`: `ingress` from `status.addresses` (`Hostname` -> hostname, `IPAddress` or empty type -> IP, empty values skipped); for each listener, scheme `https` for protocol HTTPS/TLS else `http`; hosts = listener hostname unless empty or `*`, else all addresses; port appended when not 80 (http) / 443 (https); URLs validated as http(s) and de-duplicated; suppressed by `ignore-default-links`.
- **Edge cases**: unsupported port type logged and treated as 0 (so `:0` appended); invalid URL skipped with warning.
- **Configuration surface**: `argocd.argoproj.io/ignore-default-links`.
- **Exposure**: API / UI.
- **Tests required**: port `TestGetGatewayInfo*` (IP, hostname, multiple listeners, non-standard port, no addresses, external link, ignore default links).

### DIFF-093 Gateway API route info (HTTPRoute, GRPCRoute, TCPRoute, TLSRoute, UDPRoute)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/info.go#L477
- **Maturity**: GA
- **Semantics**: Target refs = `spec.parentRefs` (defaults: namespace = route namespace, group `gateway.networking.k8s.io`, kind `Gateway`) plus every `spec.rules[].backendRefs[]` (defaults: namespace = route namespace, group `""`, kind `Service`); entries without name skipped; link-annotation URLs preserved. No default URLs for routes.
- **Edge cases**: cross-namespace parents/backends keep their explicit namespace.
- **Configuration surface**: none.
- **Exposure**: API / UI network view.
- **Tests required**: port `TestGetHTTPRouteInfo`, `TestGetHTTPRouteCrossNamespaceInfo`, `TestGetHTTPRouteWithExternalLinkAnnotation`, `TestGetHTTPRouteMultipleRules`, `TestGetHTTPRouteCrossNamespaceParent`, `TestGetGRPCRoute`.

### DIFF-094 Istio VirtualService and ServiceEntry info
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/info.go#L265, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/info.go#L317
- **Maturity**: GA
- **Semantics**: `networking.istio.io/VirtualService`: target Services from `spec.http[].route[].destination.host` (first DNS label = name, second = namespace, default the VS namespace). `ServiceEntry`: if `spec.workloadSelector.labels` present, `targetLabels` = those labels and a target ref of kind `Pod`.
- **Edge cases**: a ServiceEntry with `workloadSelector.labels` replaces `networkingInfo` wholesale, dropping any link-annotation URLs (parity quirk), while one without a selector keeps link URLs and gets no targets; VirtualService `tcp`/`tls` routes are not considered.
- **Configuration surface**: none.
- **Exposure**: API / UI network view.
- **Tests required**: port `TestGetIstioVirtualServiceInfo`, `TestGetIstioServiceEntryInfo`.

### DIFF-095 External link annotations and ignore-default-links
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/info.go#L44, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L241, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L245, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L1708
- **Maturity**: GA
- **Semantics**: Any resource annotation with prefix `link.argocd.argoproj.io/` adds its value (`url` or `title|url`) to `networkingInfo.externalURLs` if the URL part parses and has scheme http or https. `argocd.argoproj.io/ignore-default-links: "true"` (ParseBool) disables auto-generated URLs for Ingress and Gateway only.
- **Edge cases**: `javascript:`/`data:` URLs dropped (XSS protection); stored value keeps the `title|` prefix; Application-level link annotations go to the summary without validation (DIFF-100).
- **Configuration surface**: annotations listed.
- **Exposure**: API / UI (link icons) / app summary.
- **Tests required**: positive: `link.argocd.argoproj.io/docs: Docs|https://x` shown; negative: `javascript:alert(1)` dropped.

### DIFF-096 Revision info item
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/resource/revision.go#L9, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/info.go#L30
- **Maturity**: GA
- **Semantics**: If annotation `deployment.kubernetes.io/revision` or `rollout.argoproj.io/revision` (first found) parses as int64 > 0, or a top-level int64 `revision` field exists (ControllerRevision), an info item `Revision` = `Rev:<n>` is added.
- **Edge cases**: unparsable annotation -> 0 -> no item.
- **Configuration surface**: none.
- **Exposure**: API / UI (ReplicaSet revision badges).
- **Tests required**: positive: ReplicaSet with revision 3 -> `Rev:3`; negative: non-numeric annotation -> none.

### DIFF-097 resource.customLabels info items
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L2612, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/info.go#L34, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/declarative-setup.md#L1446
- **Maturity**: GA
- **Semantics**: `argocd-cm` `resource.customLabels` (comma-separated) — for each listed label present on a resource, an info item `{name: <label>, value: <value>}` is added.
- **Edge cases**: list is split on commas without trimming spaces (`a, b` looks for label ` b`); read only when a cluster cache is created (controller restart needed, as documented).
- **Configuration surface**: `resource.customLabels`.
- **Exposure**: API / UI.
- **Tests required**: port `TestCustomLabel`.

### DIFF-098 Node info and application hosts
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/info.go#L772, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L703, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L2762, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2013
- **Maturity**: GA
- **Semantics**: Core Nodes are cached with capacity, system info and labels; pods with node name and resource requests. For each node running app pods, `tree.hosts[]` gets `{name, systemInfo, labels (only keys in application.allowedNodeLabels), resourcesInfo[]}` where resourcesInfo for cpu, memory, storage (only if capacity > 0) has `capacity`, `requestedByApp` and `requestedByNeighbors` (non-app pods excluding Succeeded/Failed) in milli-units, sorted by resource name.
- **Edge cases**: nodes not watched (excluded or RBAC) -> no hosts; invalid label keys in `application.allowedNodeLabels` skipped with warning.
- **Configuration surface**: `argocd-cm` `application.allowedNodeLabels`.
- **Exposure**: API (tree hosts) / UI (pods view grouped by node).
- **Tests required**: port `TestGetNodeInfo`; positive: requests aggregated per node; negative: label not allowed not exposed.

### DIFF-099 Child Application info (managed-by-url)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/info.go#L786
- **Maturity**: GA
- **Semantics**: For `argoproj.io/Application` resources in a tree, annotation `argocd.argoproj.io/managed-by-url` is surfaced as info item `managed-by-url`.
- **Edge cases**: value not validated.
- **Configuration surface**: annotation.
- **Exposure**: API / UI (app-of-apps links).
- **Tests required**: positive: annotated child app node has the info item.

### DIFF-100 Application summary (external URLs, images, app-of-apps)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2140, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2039
- **Maturity**: GA
- **Semantics**: `status.summary` is recomputed from the tree: `externalURLs` = unique URLs from all managed nodes' networkingInfo plus values of the Application's own `link.argocd.argoproj.io/*` annotations, sorted; `images` = unique node images, sorted; `isAppOfApps` = true if any node is `argoproj.io/Application`. Orphaned nodes are not included.
- **Edge cases**: Application link annotations are not URL-validated here; values with `title|url` kept verbatim.
- **Configuration surface**: link annotations.
- **Exposure**: CRD `status.summary` / UI (app tiles links, images) / CLI (`argocd app get` URLs).
- **Tests required**: positive: Ingress URL appears in summary; images de-duplicated and sorted; child app sets isAppOfApps.

## Cluster cache and compatibility

### DIFF-101 Cluster cache tuning parameters
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/cache.go#L46, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/cache.go#L85, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/cache.go#L121, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/cache/cluster.go#L63, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/application-controller/argocd-application-controller-statefulset.yaml#L337
- **Maturity**: GA
- **Semantics**: Env vars (controller): `ARGOCD_CLUSTER_CACHE_RESYNC_DURATION` (full cache invalidation interval, default 12h; 0 = never), `ARGOCD_CLUSTER_CACHE_WATCH_RESYNC_DURATION` (watch restart/relist, default 10m), `ARGOCD_CLUSTER_SYNC_RETRY_TIMEOUT_DURATION` (retry after sync error, default 10s), `ARGOCD_CLUSTER_CACHE_LIST_PAGE_SIZE` (default 500), `ARGOCD_CLUSTER_CACHE_LIST_PAGE_BUFFER_SIZE` (default 1, min 1), `ARGOCD_CLUSTER_CACHE_LIST_SEMAPHORE` (concurrent list processing, default 50), `ARGOCD_CLUSTER_CACHE_ATTEMPT_LIMIT` (default 1 = no retries, min 1), `ARGOCD_CLUSTER_CACHE_RETRY_USE_BACKOFF` (default false), `ARGOCD_CLUSTER_CACHE_BATCH_EVENTS_PROCESSING` (default true; cmd-params `controller.cluster.cache.batch.events.processing`), `ARGOCD_CLUSTER_CACHE_EVENTS_PROCESSING_INTERVAL` (default 100ms; cmd-params `controller.cluster.cache.events.processing.interval`). Retryable list errors: quota conflicts/exceeded, HTTP/2 GOAWAY, transient network errors. Settings changes affecting cache settings (tracking, filters, health overrides, ignoreResourceUpdates) invalidate all cluster caches live.
- **Edge cases**: out-of-range env values fall back to defaults (env parser bounds); deprecation warnings from the API server are suppressed below debug log level.
- **Configuration surface**: env vars and cmd-params keys listed.
- **Exposure**: controller env / ConfigMap.
- **Tests required**: port `TestIsRetryableError`, `TestLoadCacheSettings`, `TestHandleModEvent_*`; positive: settings change triggers invalidation; batch processing coalesces events at the configured interval.

### DIFF-102 Kubernetes version compatibility
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/tested-kubernetes-versions.md#L3, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/cache/cluster.go#L1112
- **Maturity**: GA
- **Semantics**: Argo CD 3.5 is tested against Kubernetes v1.36, v1.35, v1.34 and v1.33. The cluster cache records the server version and API resources at each sync (exposed as cluster info and passed to manifest generation in other areas). Health checks and diff normalizations depend on stable API versions: built-in Go checks accept only the versions listed in DIFF-045..056; deprecated groups still handled include `extensions/Ingress`, `extensions/StatefulSet` (diff workaround), `autoscaling/v1` HPA (annotation conditions) and `apiregistration.k8s.io/v1beta1` APIService; `corev1.Endpoints` (deprecated since 1.33) is still normalized and related.
- **Edge cases**: objects in removed beta APIs yield `unsupported <Kind> GVK` -> Unknown health; server version fetch failure fails the cluster sync (`failed to get server version`).
- **Configuration surface**: none.
- **Exposure**: docs / cluster info.
- **Tests required**: run the Patrol e2e suite on the four tested minor versions; positive: health and diff fixtures pass on each; negative: v1beta Deployment reports Unknown with the exact message.

## Built-in resource customization inventory

Counts at this commit (directory `resource_customizations/`): 111 API-group directories, 247 group/kind directories, 238 kinds with `health.lua`, 36 kinds with `actions/` holding 93 `action.lua` scripts (excluding `testdata`); 9 kinds have actions only (`apps/DaemonSet|Deployment|StatefulSet`, `argoproj.io/Application|WorkflowTemplate`, `batch/Job`, `core.strimzi.io/StrimziPodSet`, `notification.toolkit.fluxcd.io/Alert|Provider`). One feature per API group follows. "statuses referenced" lists the health codes that appear as string literals in each script (a static hint, not a proof of reachability).

### DIFF-103 Built-in customizations: `_.cnrm.cloud.google.com`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/_.cnrm.cloud.google.com/_/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `_.cnrm.cloud.google.com` (directory name contains `_`, matched as a doublestar glob where `_` becomes `*`; used only when no exact built-in script and no argocd-cm override matches). Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `_` (wildcard: any kind): health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended; 11 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (_: 11 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-104 Built-in customizations: `_.crossplane.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/_.crossplane.io/_/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `_.crossplane.io` (directory name contains `_`, matched as a doublestar glob where `_` becomes `*`; used only when no exact built-in script and no argocd-cm override matches). Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `_` (wildcard: any kind): health.lua (statuses referenced: Healthy, Progressing, Degraded; 11 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (_: 11 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-105 Built-in customizations: `_.upbound.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/_.upbound.io/_/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `_.upbound.io` (directory name contains `_`, matched as a doublestar glob where `_` becomes `*`; used only when no exact built-in script and no argocd-cm override matches). Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `_` (wildcard: any kind): health.lua (statuses referenced: Healthy, Progressing, Degraded; 8 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (_: 8 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-106 Built-in customizations: `acid.zalan.do`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/acid.zalan.do/postgresql/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `acid.zalan.do`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `postgresql`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 4 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (postgresql: 4 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-107 Built-in customizations: `addons.cluster.x-k8s.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/addons.cluster.x-k8s.io/ClusterResourceSet/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `addons.cluster.x-k8s.io`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `ClusterResourceSet`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 3 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (ClusterResourceSet: 3 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-108 Built-in customizations: `apiextensions.k8s.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/apiextensions.k8s.io/CustomResourceDefinition/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `apiextensions.k8s.io`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `CustomResourceDefinition`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 8 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (CustomResourceDefinition: 8 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-109 Built-in customizations: `apps`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/apps/DaemonSet/actions/discovery.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/apps/Deployment/actions/discovery.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/apps/StatefulSet/actions/discovery.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `apps`. Kinds: 3; with health.lua: 0; with actions: 3.
  - `DaemonSet`: actions: restart (discovery.lua)
  - `Deployment`: actions: pause, restart, resume, scale (discovery.lua)
  - `StatefulSet`: actions: restart, scale (discovery.lua)
- **Edge cases**: built-in actions are hidden when an argocd-cm `actions` override exists for the same group/kind unless it sets `mergeBuiltinActions: true`; action semantics belong to the resource-actions area (inventoried here only).
- **Configuration surface**: none required (embedded in the binary); actions overridable/extended via `resource.customizations.actions.<group>_<kind>` (`discovery.lua`, `definitions`, `mergeBuiltinActions`).
- **Exposure**: actions: API (`ListResourceActions`, `RunResourceAction`/`RunResourceActionV2`) / CLI (`argocd app actions list|run`) / UI (resource kebab menu).
- **Tests required**: no built-in health script in this group (health comes from Go checks or is absent); actions: pass each action's `action_test.yaml` fixtures (resource-actions area) and verify discovery lists exactly the names above.

### DIFF-110 Built-in customizations: `apps.3scale.net`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/apps.3scale.net/APIManager/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `apps.3scale.net`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `APIManager`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 5 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (APIManager: 5 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-111 Built-in customizations: `apps.kruise.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/apps.kruise.io/AdvancedCronJob/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/apps.kruise.io/BroadcastJob/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/apps.kruise.io/CloneSet/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/apps.kruise.io/DaemonSet/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/apps.kruise.io/StatefulSet/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `apps.kruise.io`. Kinds: 5; with health.lua: 5; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `AdvancedCronJob`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended; 4 health_test.yaml cases)
  - `BroadcastJob`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended; 4 health_test.yaml cases)
  - `CloneSet`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended; 3 health_test.yaml cases)
  - `DaemonSet`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended; 6 health_test.yaml cases)
  - `StatefulSet`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended; 5 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (AdvancedCronJob: 4 cases; BroadcastJob: 4 cases; CloneSet: 3 cases; DaemonSet: 6 cases; StatefulSet: 5 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-112 Built-in customizations: `apps.openshift.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/apps.openshift.io/DeploymentConfig/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `apps.openshift.io`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `DeploymentConfig`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 5 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (DeploymentConfig: 5 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-113 Built-in customizations: `argoproj.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/argoproj.io/AnalysisRun/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/argoproj.io/AnalysisRun/actions/discovery.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/argoproj.io/Application/actions/discovery.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/argoproj.io/ApplicationSet/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/argoproj.io/CronWorkflow/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/argoproj.io/CronWorkflow/actions/discovery.lua#L1 (+5 more files in the same directory tree)
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `argoproj.io`. Kinds: 8; with health.lua: 6; with actions: 5. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `AnalysisRun`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Unknown; 11 health_test.yaml cases); actions: terminate (discovery.lua)
  - `Application`: actions: toggle-auto-sync (discovery.lua)
  - `ApplicationSet`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 5 health_test.yaml cases)
  - `CronWorkflow`: health.lua (statuses referenced: Healthy, Degraded; 5 health_test.yaml cases); actions: create-workflow (discovery.lua)
  - `EventBus`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 2 health_test.yaml cases)
  - `Experiment`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 6 health_test.yaml cases)
  - `Rollout`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended; 24 health_test.yaml cases); actions: abort, pause, promote-full, restart, resume, retry, skip-current-step (discovery.lua)
  - `WorkflowTemplate`: actions: create-workflow (discovery.lua)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks; built-in actions are hidden when an argocd-cm `actions` override exists for the same group/kind unless it sets `mergeBuiltinActions: true`; action semantics belong to the resource-actions area (inventoried here only).
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block); actions overridable/extended via `resource.customizations.actions.<group>_<kind>` (`discovery.lua`, `definitions`, `mergeBuiltinActions`).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons); actions: API (`ListResourceActions`, `RunResourceAction`/`RunResourceActionV2`) / CLI (`argocd app actions list|run`) / UI (resource kebab menu).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (AnalysisRun: 11 cases; ApplicationSet: 5 cases; CronWorkflow: 5 cases; EventBus: 2 cases; Experiment: 6 cases; Rollout: 24 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message; actions: pass each action's `action_test.yaml` fixtures in the resource-actions area.

### DIFF-114 Built-in customizations: `astra.netapp.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/astra.netapp.io/AppVault/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/astra.netapp.io/Application/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/astra.netapp.io/Backup/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/astra.netapp.io/ExecHook/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/astra.netapp.io/ExecHooksRun/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/astra.netapp.io/ResourceBackup/health.lua#L1 (+3 more files in the same directory tree)
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `astra.netapp.io`. Kinds: 9; with health.lua: 9; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `AppVault`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 3 health_test.yaml cases)
  - `Application`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 3 health_test.yaml cases)
  - `Backup`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 4 health_test.yaml cases)
  - `ExecHook`: health.lua (statuses referenced: Healthy, Progressing, Suspended; 3 health_test.yaml cases)
  - `ExecHooksRun`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 4 health_test.yaml cases)
  - `ResourceBackup`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 4 health_test.yaml cases)
  - `ResticVolumeBackup`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 4 health_test.yaml cases)
  - `Schedule`: health.lua (statuses referenced: Healthy; 2 health_test.yaml cases)
  - `Snapshot`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 4 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (AppVault: 3 cases; Application: 3 cases; Backup: 4 cases; ExecHook: 3 cases; ExecHooksRun: 4 cases; ResourceBackup: 4 cases; ResticVolumeBackup: 4 cases; Schedule: 2 cases; Snapshot: 4 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-115 Built-in customizations: `batch`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/batch/CronJob/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/batch/CronJob/actions/discovery.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/batch/Job/actions/discovery.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `batch`. Kinds: 2; with health.lua: 1; with actions: 2. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `CronJob`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 6 health_test.yaml cases); actions: create-job, resume, suspend (discovery.lua)
  - `Job`: actions: resume, suspend, terminate (discovery.lua)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks; built-in actions are hidden when an argocd-cm `actions` override exists for the same group/kind unless it sets `mergeBuiltinActions: true`; action semantics belong to the resource-actions area (inventoried here only).
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block); actions overridable/extended via `resource.customizations.actions.<group>_<kind>` (`discovery.lua`, `definitions`, `mergeBuiltinActions`).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons); actions: API (`ListResourceActions`, `RunResourceAction`/`RunResourceActionV2`) / CLI (`argocd app actions list|run`) / UI (resource kebab menu).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (CronJob: 6 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message; actions: pass each action's `action_test.yaml` fixtures in the resource-actions area.

### DIFF-116 Built-in customizations: `beat.k8s.elastic.co`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/beat.k8s.elastic.co/Beat/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `beat.k8s.elastic.co`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `Beat`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Unknown; 7 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (Beat: 7 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-117 Built-in customizations: `bitnami.com`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/bitnami.com/SealedSecret/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `bitnami.com`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `SealedSecret`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 3 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (SealedSecret: 3 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-118 Built-in customizations: `camel.apache.org`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/camel.apache.org/Integration/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `camel.apache.org`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `Integration`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 3 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (Integration: 3 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-119 Built-in customizations: `capabilities.3scale.net`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/capabilities.3scale.net/ActiveDoc/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/capabilities.3scale.net/Application/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/capabilities.3scale.net/ApplicationAuth/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/capabilities.3scale.net/Backend/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/capabilities.3scale.net/CustomPolicyDefinition/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/capabilities.3scale.net/DeveloperAccount/health.lua#L1 (+5 more files in the same directory tree)
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `capabilities.3scale.net`. Kinds: 11; with health.lua: 11; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `ActiveDoc`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 5 health_test.yaml cases)
  - `Application`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended; 4 health_test.yaml cases)
  - `ApplicationAuth`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 3 health_test.yaml cases)
  - `Backend`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 4 health_test.yaml cases)
  - `CustomPolicyDefinition`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 3 health_test.yaml cases)
  - `DeveloperAccount`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended; 5 health_test.yaml cases)
  - `DeveloperUser`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended; 6 health_test.yaml cases)
  - `OpenAPI`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 4 health_test.yaml cases)
  - `Product`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 5 health_test.yaml cases)
  - `ProxyConfigPromote`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 4 health_test.yaml cases)
  - `Tenant`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 3 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (ActiveDoc: 5 cases; Application: 4 cases; ApplicationAuth: 3 cases; Backend: 4 cases; CustomPolicyDefinition: 3 cases; DeveloperAccount: 5 cases; DeveloperUser: 6 cases; OpenAPI: 4 cases; Product: 5 cases; ProxyConfigPromote: 4 cases; Tenant: 3 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-120 Built-in customizations: `cassandra.rook.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/cassandra.rook.io/Cluster/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `cassandra.rook.io`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `Cluster`: health.lua (statuses referenced: Healthy, Progressing; 2 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (Cluster: 2 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-121 Built-in customizations: `cdi.kubevirt.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/cdi.kubevirt.io/DataVolume/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `cdi.kubevirt.io`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `DataVolume`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended, Unknown; 5 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (DataVolume: 5 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-122 Built-in customizations: `ceph.rook.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/ceph.rook.io/CephCluster/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/ceph.rook.io/CephObjectStore/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `ceph.rook.io`. Kinds: 2; with health.lua: 2; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `CephCluster`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 5 health_test.yaml cases)
  - `CephObjectStore`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 3 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (CephCluster: 5 cases; CephObjectStore: 3 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-123 Built-in customizations: `cert-manager.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/cert-manager.io/Certificate/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/cert-manager.io/ClusterIssuer/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/cert-manager.io/Issuer/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `cert-manager.io`. Kinds: 3; with health.lua: 3; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `Certificate`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 6 health_test.yaml cases)
  - `ClusterIssuer`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 3 health_test.yaml cases)
  - `Issuer`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 3 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (Certificate: 6 cases; ClusterIssuer: 3 cases; Issuer: 3 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-124 Built-in customizations: `certmanager.k8s.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/certmanager.k8s.io/Certificate/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/certmanager.k8s.io/Issuer/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `certmanager.k8s.io`. Kinds: 2; with health.lua: 2; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `Certificate`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 4 health_test.yaml cases)
  - `Issuer`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 3 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (Certificate: 4 cases; Issuer: 3 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-125 Built-in customizations: `clickhouse-keeper.altinity.com`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/clickhouse-keeper.altinity.com/ClickHouseKeeperInstallation/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `clickhouse-keeper.altinity.com`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `ClickHouseKeeperInstallation`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 4 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (ClickHouseKeeperInstallation: 4 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-126 Built-in customizations: `clickhouse.altinity.com`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/clickhouse.altinity.com/ClickHouseInstallation/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `clickhouse.altinity.com`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `ClickHouseInstallation`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 4 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (ClickHouseInstallation: 4 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-127 Built-in customizations: `cloudfront.aws.crossplane.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/cloudfront.aws.crossplane.io/Distribution/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `cloudfront.aws.crossplane.io`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `Distribution`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended; 7 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (Distribution: 7 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-128 Built-in customizations: `cluster.x-k8s.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/cluster.x-k8s.io/Cluster/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/cluster.x-k8s.io/Machine/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/cluster.x-k8s.io/MachineDeployment/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/cluster.x-k8s.io/MachineHealthCheck/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/cluster.x-k8s.io/MachinePool/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `cluster.x-k8s.io`. Kinds: 5; with health.lua: 5; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `Cluster`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended; 7 health_test.yaml cases)
  - `Machine`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 3 health_test.yaml cases)
  - `MachineDeployment`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended; 5 health_test.yaml cases)
  - `MachineHealthCheck`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 2 health_test.yaml cases)
  - `MachinePool`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Unknown; 4 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (Cluster: 7 cases; Machine: 3 cases; MachineDeployment: 5 cases; MachineHealthCheck: 2 cases; MachinePool: 4 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-129 Built-in customizations: `controlplane.cluster.x-k8s.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/controlplane.cluster.x-k8s.io/AWSManagedControlPlane/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `controlplane.cluster.x-k8s.io`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `AWSManagedControlPlane`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 4 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (AWSManagedControlPlane: 4 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-130 Built-in customizations: `coralogix.com`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/coralogix.com/Alert/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/coralogix.com/RecordingRuleGroupSet/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `coralogix.com`. Kinds: 2; with health.lua: 2; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `Alert`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 3 health_test.yaml cases)
  - `RecordingRuleGroupSet`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 3 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (Alert: 3 cases; RecordingRuleGroupSet: 3 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-131 Built-in customizations: `core.gardener.cloud`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/core.gardener.cloud/Shoot/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `core.gardener.cloud`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `Shoot`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Unknown; 4 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (Shoot: 4 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-132 Built-in customizations: `core.humio.com`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/core.humio.com/HumioAction/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/core.humio.com/HumioAlert/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/core.humio.com/HumioCluster/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/core.humio.com/HumioIngestToken/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/core.humio.com/HumioParser/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/core.humio.com/HumioRepository/health.lua#L1 (+1 more files in the same directory tree)
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `core.humio.com`. Kinds: 7; with health.lua: 7; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `HumioAction`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Missing, Unknown; 5 health_test.yaml cases)
  - `HumioAlert`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Missing, Unknown; 5 health_test.yaml cases)
  - `HumioCluster`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Unknown; 7 health_test.yaml cases)
  - `HumioIngestToken`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Missing, Unknown; 5 health_test.yaml cases)
  - `HumioParser`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Missing, Unknown; 5 health_test.yaml cases)
  - `HumioRepository`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Missing, Unknown; 5 health_test.yaml cases)
  - `HumioView`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Missing, Unknown; 5 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (HumioAction: 5 cases; HumioAlert: 5 cases; HumioCluster: 7 cases; HumioIngestToken: 5 cases; HumioParser: 5 cases; HumioRepository: 5 cases; HumioView: 5 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-133 Built-in customizations: `core.spinkube.dev`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/core.spinkube.dev/SpinApp/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `core.spinkube.dev`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `SpinApp`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 4 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (SpinApp: 4 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-134 Built-in customizations: `core.strimzi.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/core.strimzi.io/StrimziPodSet/actions/discovery.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `core.strimzi.io`. Kinds: 1; with health.lua: 0; with actions: 1.
  - `StrimziPodSet`: actions: rolling-update (discovery.lua)
- **Edge cases**: built-in actions are hidden when an argocd-cm `actions` override exists for the same group/kind unless it sets `mergeBuiltinActions: true`; action semantics belong to the resource-actions area (inventoried here only).
- **Configuration surface**: none required (embedded in the binary); actions overridable/extended via `resource.customizations.actions.<group>_<kind>` (`discovery.lua`, `definitions`, `mergeBuiltinActions`).
- **Exposure**: actions: API (`ListResourceActions`, `RunResourceAction`/`RunResourceActionV2`) / CLI (`argocd app actions list|run`) / UI (resource kebab menu).
- **Tests required**: no built-in health script in this group (health comes from Go checks or is absent); actions: pass each action's `action_test.yaml` fixtures (resource-actions area) and verify discovery lists exactly the names above.

### DIFF-135 Built-in customizations: `datadoghq.com`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/datadoghq.com/DatadogMetric/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `datadoghq.com`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `DatadogMetric`: health.lua (statuses referenced: Healthy, Degraded; 3 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (DatadogMetric: 3 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-136 Built-in customizations: `db.atlasgo.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/db.atlasgo.io/AtlasMigration/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/db.atlasgo.io/AtlasSchema/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `db.atlasgo.io`. Kinds: 2; with health.lua: 2; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `AtlasMigration`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 3 health_test.yaml cases)
  - `AtlasSchema`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 3 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (AtlasMigration: 3 cases; AtlasSchema: 3 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-137 Built-in customizations: `elasticsearch.k8s.elastic.co`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/elasticsearch.k8s.elastic.co/Elasticsearch/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `elasticsearch.k8s.elastic.co`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `Elasticsearch`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Unknown; 8 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (Elasticsearch: 8 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-138 Built-in customizations: `external-secrets.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/external-secrets.io/ClusterExternalSecret/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/external-secrets.io/ClusterSecretStore/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/external-secrets.io/ExternalSecret/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/external-secrets.io/ExternalSecret/actions/discovery.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/external-secrets.io/PushSecret/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/external-secrets.io/PushSecret/actions/discovery.lua#L1 (+1 more files in the same directory tree)
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `external-secrets.io`. Kinds: 5; with health.lua: 5; with actions: 2. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `ClusterExternalSecret`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 5 health_test.yaml cases)
  - `ClusterSecretStore`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 2 health_test.yaml cases)
  - `ExternalSecret`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 3 health_test.yaml cases); actions: refresh (discovery.lua)
  - `PushSecret`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 3 health_test.yaml cases); actions: push (discovery.lua)
  - `SecretStore`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 2 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks; built-in actions are hidden when an argocd-cm `actions` override exists for the same group/kind unless it sets `mergeBuiltinActions: true`; action semantics belong to the resource-actions area (inventoried here only).
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block); actions overridable/extended via `resource.customizations.actions.<group>_<kind>` (`discovery.lua`, `definitions`, `mergeBuiltinActions`).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons); actions: API (`ListResourceActions`, `RunResourceAction`/`RunResourceActionV2`) / CLI (`argocd app actions list|run`) / UI (resource kebab menu).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (ClusterExternalSecret: 5 cases; ClusterSecretStore: 2 cases; ExternalSecret: 3 cases; PushSecret: 3 cases; SecretStore: 2 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message; actions: pass each action's `action_test.yaml` fixtures in the resource-actions area.

### DIFF-139 Built-in customizations: `flagger.app`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/flagger.app/Canary/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `flagger.app`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `Canary`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Unknown; 6 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (Canary: 6 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-140 Built-in customizations: `flink.apache.org`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/flink.apache.org/FlinkDeployment/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `flink.apache.org`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `FlinkDeployment`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 9 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (FlinkDeployment: 9 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-141 Built-in customizations: `game.kruise.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/game.kruise.io/GameServer/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/game.kruise.io/GameServerSet/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `game.kruise.io`. Kinds: 2; with health.lua: 2; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `GameServer`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Unknown; 4 health_test.yaml cases)
  - `GameServerSet`: health.lua (statuses referenced: Healthy, Progressing, Suspended; 5 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (GameServer: 4 cases; GameServerSet: 5 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-142 Built-in customizations: `gateway.networking.k8s.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/gateway.networking.k8s.io/BackendTLSPolicy/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/gateway.networking.k8s.io/GRPCRoute/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/gateway.networking.k8s.io/Gateway/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/gateway.networking.k8s.io/GatewayClass/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/gateway.networking.k8s.io/HTTPRoute/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `gateway.networking.k8s.io`. Kinds: 5; with health.lua: 5; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `BackendTLSPolicy`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 5 health_test.yaml cases)
  - `GRPCRoute`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 8 health_test.yaml cases)
  - `Gateway`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 5 health_test.yaml cases)
  - `GatewayClass`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 5 health_test.yaml cases)
  - `HTTPRoute`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 5 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (BackendTLSPolicy: 5 cases; GRPCRoute: 8 cases; Gateway: 5 cases; GatewayClass: 5 cases; HTTPRoute: 5 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-143 Built-in customizations: `gateway.solo.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/gateway.solo.io/Gateway/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/gateway.solo.io/MatchableHttpGateway/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/gateway.solo.io/RouteOption/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/gateway.solo.io/RouteTable/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/gateway.solo.io/VirtualHostOption/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/gateway.solo.io/VirtualService/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `gateway.solo.io`. Kinds: 6; with health.lua: 6; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `Gateway`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended; 9 health_test.yaml cases)
  - `MatchableHttpGateway`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended; 9 health_test.yaml cases)
  - `RouteOption`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended; 9 health_test.yaml cases)
  - `RouteTable`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended; 9 health_test.yaml cases)
  - `VirtualHostOption`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended; 9 health_test.yaml cases)
  - `VirtualService`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended; 9 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (Gateway: 9 cases; MatchableHttpGateway: 9 cases; RouteOption: 9 cases; RouteTable: 9 cases; VirtualHostOption: 9 cases; VirtualService: 9 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-144 Built-in customizations: `gloo.solo.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/gloo.solo.io/Proxy/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/gloo.solo.io/Settings/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/gloo.solo.io/Upstream/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/gloo.solo.io/UpstreamGroup/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `gloo.solo.io`. Kinds: 4; with health.lua: 4; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `Proxy`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended; 9 health_test.yaml cases)
  - `Settings`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended; 9 health_test.yaml cases)
  - `Upstream`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended; 9 health_test.yaml cases)
  - `UpstreamGroup`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended; 9 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (Proxy: 9 cases; Settings: 9 cases; Upstream: 9 cases; UpstreamGroup: 9 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-145 Built-in customizations: `grafana-org-operator.kubitus-project.gitlab.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/grafana-org-operator.kubitus-project.gitlab.io/_/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `grafana-org-operator.kubitus-project.gitlab.io` (directory name contains `_`, matched as a doublestar glob where `_` becomes `*`; used only when no exact built-in script and no argocd-cm override matches). Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `_` (wildcard: any kind): health.lua (statuses referenced: Healthy, Progressing, Degraded; 3 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (_: 3 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-146 Built-in customizations: `grafana.integreatly.org`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/grafana.integreatly.org/Grafana/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/grafana.integreatly.org/GrafanaDashboard/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/grafana.integreatly.org/GrafanaDatasource/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/grafana.integreatly.org/GrafanaFolder/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `grafana.integreatly.org`. Kinds: 4; with health.lua: 4; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `Grafana`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 4 health_test.yaml cases)
  - `GrafanaDashboard`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 3 health_test.yaml cases)
  - `GrafanaDatasource`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 3 health_test.yaml cases)
  - `GrafanaFolder`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 5 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (Grafana: 4 cases; GrafanaDashboard: 3 cases; GrafanaDatasource: 3 cases; GrafanaFolder: 5 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-147 Built-in customizations: `helm.toolkit.fluxcd.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/helm.toolkit.fluxcd.io/HelmRelease/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/helm.toolkit.fluxcd.io/HelmRelease/actions/discovery.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `helm.toolkit.fluxcd.io`. Kinds: 1; with health.lua: 1; with actions: 1. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `HelmRelease`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended, Unknown; 3 health_test.yaml cases); actions: reconcile, resume, suspend (discovery.lua)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks; built-in actions are hidden when an argocd-cm `actions` override exists for the same group/kind unless it sets `mergeBuiltinActions: true`; action semantics belong to the resource-actions area (inventoried here only).
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block); actions overridable/extended via `resource.customizations.actions.<group>_<kind>` (`discovery.lua`, `definitions`, `mergeBuiltinActions`).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons); actions: API (`ListResourceActions`, `RunResourceAction`/`RunResourceActionV2`) / CLI (`argocd app actions list|run`) / UI (resource kebab menu).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (HelmRelease: 3 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message; actions: pass each action's `action_test.yaml` fixtures in the resource-actions area.

### DIFF-148 Built-in customizations: `iam.aws.crossplane.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/iam.aws.crossplane.io/Policy/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/iam.aws.crossplane.io/Role/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/iam.aws.crossplane.io/RolePolicyAttachment/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `iam.aws.crossplane.io`. Kinds: 3; with health.lua: 3; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `Policy`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended; 2 health_test.yaml cases)
  - `Role`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended; 2 health_test.yaml cases)
  - `RolePolicyAttachment`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended; 2 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (Policy: 2 cases; Role: 2 cases; RolePolicyAttachment: 2 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-149 Built-in customizations: `iammanager.keikoproj.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/iammanager.keikoproj.io/Iamrole/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `iammanager.keikoproj.io`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `Iamrole`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 4 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (Iamrole: 4 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-150 Built-in customizations: `image.toolkit.fluxcd.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/image.toolkit.fluxcd.io/ImagePolicy/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/image.toolkit.fluxcd.io/ImageRepository/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/image.toolkit.fluxcd.io/ImageRepository/actions/discovery.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/image.toolkit.fluxcd.io/ImageUpdateAutomation/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/image.toolkit.fluxcd.io/ImageUpdateAutomation/actions/discovery.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `image.toolkit.fluxcd.io`. Kinds: 3; with health.lua: 3; with actions: 2. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `ImagePolicy`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 3 health_test.yaml cases)
  - `ImageRepository`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended; 3 health_test.yaml cases); actions: reconcile, resume, suspend (discovery.lua)
  - `ImageUpdateAutomation`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended, Unknown; 3 health_test.yaml cases); actions: reconcile, resume, suspend (discovery.lua)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks; built-in actions are hidden when an argocd-cm `actions` override exists for the same group/kind unless it sets `mergeBuiltinActions: true`; action semantics belong to the resource-actions area (inventoried here only).
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block); actions overridable/extended via `resource.customizations.actions.<group>_<kind>` (`discovery.lua`, `definitions`, `mergeBuiltinActions`).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons); actions: API (`ListResourceActions`, `RunResourceAction`/`RunResourceActionV2`) / CLI (`argocd app actions list|run`) / UI (resource kebab menu).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (ImagePolicy: 3 cases; ImageRepository: 3 cases; ImageUpdateAutomation: 3 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message; actions: pass each action's `action_test.yaml` fixtures in the resource-actions area.

### DIFF-151 Built-in customizations: `install.istio.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/install.istio.io/IstioOperator/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `install.istio.io`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `IstioOperator`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Unknown; 4 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (IstioOperator: 4 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-152 Built-in customizations: `jaegertracing.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/jaegertracing.io/Jaeger/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `jaegertracing.io`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `Jaeger`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 3 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (Jaeger: 3 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-153 Built-in customizations: `k8s.keycloak.org`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/k8s.keycloak.org/Keycloak/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `k8s.keycloak.org`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `Keycloak`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 5 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (Keycloak: 5 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-154 Built-in customizations: `k8s.mariadb.com`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/k8s.mariadb.com/Backup/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/k8s.mariadb.com/Database/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/k8s.mariadb.com/Grant/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/k8s.mariadb.com/MariaDB/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/k8s.mariadb.com/MariaDB/actions/discovery.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/k8s.mariadb.com/SqlJob/health.lua#L1 (+1 more files in the same directory tree)
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `k8s.mariadb.com`. Kinds: 6; with health.lua: 6; with actions: 1. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `Backup`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 2 health_test.yaml cases)
  - `Database`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 1 health_test.yaml cases)
  - `Grant`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 1 health_test.yaml cases)
  - `MariaDB`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 6 health_test.yaml cases); actions: resume, suspend (discovery.lua)
  - `SqlJob`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 2 health_test.yaml cases)
  - `User`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 1 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks; built-in actions are hidden when an argocd-cm `actions` override exists for the same group/kind unless it sets `mergeBuiltinActions: true`; action semantics belong to the resource-actions area (inventoried here only).
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block); actions overridable/extended via `resource.customizations.actions.<group>_<kind>` (`discovery.lua`, `definitions`, `mergeBuiltinActions`).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons); actions: API (`ListResourceActions`, `RunResourceAction`/`RunResourceActionV2`) / CLI (`argocd app actions list|run`) / UI (resource kebab menu).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (Backup: 2 cases; Database: 1 cases; Grant: 1 cases; MariaDB: 6 cases; SqlJob: 2 cases; User: 1 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message; actions: pass each action's `action_test.yaml` fixtures in the resource-actions area.

### DIFF-155 Built-in customizations: `kafka.banzaicloud.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/kafka.banzaicloud.io/KafkaCluster/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `kafka.banzaicloud.io`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `KafkaCluster`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 5 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (KafkaCluster: 5 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-156 Built-in customizations: `kafka.strimzi.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/kafka.strimzi.io/Kafka/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/kafka.strimzi.io/KafkaBridge/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/kafka.strimzi.io/KafkaConnect/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/kafka.strimzi.io/KafkaConnector/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/kafka.strimzi.io/KafkaTopic/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/kafka.strimzi.io/KafkaUser/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `kafka.strimzi.io`. Kinds: 6; with health.lua: 6; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `Kafka`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 3 health_test.yaml cases)
  - `KafkaBridge`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 3 health_test.yaml cases)
  - `KafkaConnect`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 3 health_test.yaml cases)
  - `KafkaConnector`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 3 health_test.yaml cases)
  - `KafkaTopic`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 3 health_test.yaml cases)
  - `KafkaUser`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 3 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (Kafka: 3 cases; KafkaBridge: 3 cases; KafkaConnect: 3 cases; KafkaConnector: 3 cases; KafkaTopic: 3 cases; KafkaUser: 3 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-157 Built-in customizations: `karpenter.k8s.aws`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/karpenter.k8s.aws/EC2NodeClass/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `karpenter.k8s.aws`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `EC2NodeClass`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 2 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (EC2NodeClass: 2 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-158 Built-in customizations: `karpenter.sh`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/karpenter.sh/NodeClaim/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/karpenter.sh/NodePool/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `karpenter.sh`. Kinds: 2; with health.lua: 2; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `NodeClaim`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended; 8 health_test.yaml cases)
  - `NodePool`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 2 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (NodeClaim: 8 cases; NodePool: 2 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-159 Built-in customizations: `keda.sh`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/keda.sh/ScaledJob/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/keda.sh/ScaledJob/actions/discovery.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/keda.sh/ScaledObject/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/keda.sh/ScaledObject/actions/discovery.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `keda.sh`. Kinds: 2; with health.lua: 2; with actions: 2. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `ScaledJob`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended, Unknown; 5 health_test.yaml cases); actions: pause, resume (discovery.lua)
  - `ScaledObject`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended; 6 health_test.yaml cases); actions: pause, paused-replicas, resume (discovery.lua)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks; built-in actions are hidden when an argocd-cm `actions` override exists for the same group/kind unless it sets `mergeBuiltinActions: true`; action semantics belong to the resource-actions area (inventoried here only).
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block); actions overridable/extended via `resource.customizations.actions.<group>_<kind>` (`discovery.lua`, `definitions`, `mergeBuiltinActions`).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons); actions: API (`ListResourceActions`, `RunResourceAction`/`RunResourceActionV2`) / CLI (`argocd app actions list|run`) / UI (resource kebab menu).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (ScaledJob: 5 cases; ScaledObject: 6 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message; actions: pass each action's `action_test.yaml` fixtures in the resource-actions area.

### DIFF-160 Built-in customizations: `kiali.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/kiali.io/Kiali/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `kiali.io`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `Kiali`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 3 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (Kiali: 3 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-161 Built-in customizations: `kubernetes-client.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/kubernetes-client.io/ExternalSecret/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `kubernetes-client.io`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `ExternalSecret`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 3 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (ExternalSecret: 3 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-162 Built-in customizations: `kubevirt.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/kubevirt.io/VirtualMachine/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/kubevirt.io/VirtualMachineInstance/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `kubevirt.io`. Kinds: 2; with health.lua: 2; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `VirtualMachine`: health.lua (statuses referenced: Healthy, Progressing, Suspended; 10 health_test.yaml cases)
  - `VirtualMachineInstance`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended, Unknown; 5 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (VirtualMachine: 10 cases; VirtualMachineInstance: 5 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-163 Built-in customizations: `kustomize.toolkit.fluxcd.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/kustomize.toolkit.fluxcd.io/Kustomization/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/kustomize.toolkit.fluxcd.io/Kustomization/actions/discovery.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `kustomize.toolkit.fluxcd.io`. Kinds: 1; with health.lua: 1; with actions: 1. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `Kustomization`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended, Unknown; 3 health_test.yaml cases); actions: reconcile, resume, suspend (discovery.lua)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks; built-in actions are hidden when an argocd-cm `actions` override exists for the same group/kind unless it sets `mergeBuiltinActions: true`; action semantics belong to the resource-actions area (inventoried here only).
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block); actions overridable/extended via `resource.customizations.actions.<group>_<kind>` (`discovery.lua`, `definitions`, `mergeBuiltinActions`).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons); actions: API (`ListResourceActions`, `RunResourceAction`/`RunResourceActionV2`) / CLI (`argocd app actions list|run`) / UI (resource kebab menu).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (Kustomization: 3 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message; actions: pass each action's `action_test.yaml` fixtures in the resource-actions area.

### DIFF-164 Built-in customizations: `kyverno.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/kyverno.io/Policy/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `kyverno.io`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `Policy`: health.lua (statuses referenced: Healthy, Progressing; 2 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (Policy: 2 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-165 Built-in customizations: `lifecycle.keptn.sh`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/lifecycle.keptn.sh/KeptnAppVersion/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/lifecycle.keptn.sh/KeptnEvaluation/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/lifecycle.keptn.sh/KeptnTask/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/lifecycle.keptn.sh/KeptnWorkloadInstance/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/lifecycle.keptn.sh/KeptnWorkloadVersion/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `lifecycle.keptn.sh`. Kinds: 5; with health.lua: 5; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `KeptnAppVersion`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 3 health_test.yaml cases)
  - `KeptnEvaluation`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 3 health_test.yaml cases)
  - `KeptnTask`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 3 health_test.yaml cases)
  - `KeptnWorkloadInstance`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 3 health_test.yaml cases)
  - `KeptnWorkloadVersion`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 3 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (KeptnAppVersion: 3 cases; KeptnEvaluation: 3 cases; KeptnTask: 3 cases; KeptnWorkloadInstance: 3 cases; KeptnWorkloadVersion: 3 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-166 Built-in customizations: `logstash.k8s.elastic.co`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/logstash.k8s.elastic.co/Logstash/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `logstash.k8s.elastic.co`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `Logstash`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 4 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (Logstash: 4 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-167 Built-in customizations: `mariadb.mmontes.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/mariadb.mmontes.io/MariaDB/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `mariadb.mmontes.io`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `MariaDB`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 6 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (MariaDB: 6 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-168 Built-in customizations: `metrics.keptn.sh`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/metrics.keptn.sh/Analysis/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/metrics.keptn.sh/KeptnMetric/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `metrics.keptn.sh`. Kinds: 2; with health.lua: 2; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `Analysis`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 4 health_test.yaml cases)
  - `KeptnMetric`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 4 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (Analysis: 4 cases; KeptnMetric: 4 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-169 Built-in customizations: `minio.min.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/minio.min.io/Tenant/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `minio.min.io`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `Tenant`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 11 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (Tenant: 11 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-170 Built-in customizations: `monitoring.coreos.com`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/monitoring.coreos.com/Prometheus/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `monitoring.coreos.com`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `Prometheus`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 3 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (Prometheus: 3 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-171 Built-in customizations: `networking.gke.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/networking.gke.io/ManagedCertificate/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `networking.gke.io`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `ManagedCertificate`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 3 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (ManagedCertificate: 3 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-172 Built-in customizations: `nmstate.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/nmstate.io/NodeNetworkConfigurationPolicy/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `nmstate.io`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `NodeNetworkConfigurationPolicy`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended; 5 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (NodeNetworkConfigurationPolicy: 5 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-173 Built-in customizations: `notification.toolkit.fluxcd.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/notification.toolkit.fluxcd.io/Alert/actions/discovery.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/notification.toolkit.fluxcd.io/Provider/actions/discovery.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/notification.toolkit.fluxcd.io/Receiver/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/notification.toolkit.fluxcd.io/Receiver/actions/discovery.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `notification.toolkit.fluxcd.io`. Kinds: 3; with health.lua: 1; with actions: 3. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `Alert`: actions: resume, suspend (discovery.lua)
  - `Provider`: actions: resume, suspend (discovery.lua)
  - `Receiver`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended; 3 health_test.yaml cases); actions: reconcile, resume, suspend (discovery.lua)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks; built-in actions are hidden when an argocd-cm `actions` override exists for the same group/kind unless it sets `mergeBuiltinActions: true`; action semantics belong to the resource-actions area (inventoried here only).
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block); actions overridable/extended via `resource.customizations.actions.<group>_<kind>` (`discovery.lua`, `definitions`, `mergeBuiltinActions`).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons); actions: API (`ListResourceActions`, `RunResourceAction`/`RunResourceActionV2`) / CLI (`argocd app actions list|run`) / UI (resource kebab menu).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (Receiver: 3 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message; actions: pass each action's `action_test.yaml` fixtures in the resource-actions area.

### DIFF-174 Built-in customizations: `numaflow.numaproj.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/numaflow.numaproj.io/InterStepBufferService/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/numaflow.numaproj.io/InterStepBufferService/actions/discovery.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/numaflow.numaproj.io/MonoVertex/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/numaflow.numaproj.io/MonoVertex/actions/discovery.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/numaflow.numaproj.io/Pipeline/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/numaflow.numaproj.io/Pipeline/actions/discovery.lua#L1 (+1 more files in the same directory tree)
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `numaflow.numaproj.io`. Kinds: 4; with health.lua: 4; with actions: 3. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `InterStepBufferService`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 4 health_test.yaml cases); actions: force-promote (discovery.lua)
  - `MonoVertex`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 5 health_test.yaml cases); actions: force-promote, pause, unpause-fast, unpause-gradual (discovery.lua)
  - `Pipeline`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 5 health_test.yaml cases); actions: delete, force-promote, pause, unpause-fast, unpause-gradual (discovery.lua)
  - `Vertex`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 3 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks; built-in actions are hidden when an argocd-cm `actions` override exists for the same group/kind unless it sets `mergeBuiltinActions: true`; action semantics belong to the resource-actions area (inventoried here only).
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block); actions overridable/extended via `resource.customizations.actions.<group>_<kind>` (`discovery.lua`, `definitions`, `mergeBuiltinActions`).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons); actions: API (`ListResourceActions`, `RunResourceAction`/`RunResourceActionV2`) / CLI (`argocd app actions list|run`) / UI (resource kebab menu).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (InterStepBufferService: 4 cases; MonoVertex: 5 cases; Pipeline: 5 cases; Vertex: 3 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message; actions: pass each action's `action_test.yaml` fixtures in the resource-actions area.

### DIFF-175 Built-in customizations: `numaplane.numaproj.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/numaplane.numaproj.io/ISBServiceRollout/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/numaplane.numaproj.io/MonoVertexRollout/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/numaplane.numaproj.io/MonoVertexRollout/actions/discovery.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/numaplane.numaproj.io/NumaflowControllerRollout/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/numaplane.numaproj.io/PipelineRollout/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/numaplane.numaproj.io/PipelineRollout/actions/discovery.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `numaplane.numaproj.io`. Kinds: 4; with health.lua: 4; with actions: 2. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `ISBServiceRollout`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Unknown; 7 health_test.yaml cases)
  - `MonoVertexRollout`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Unknown; 7 health_test.yaml cases); actions: pause, unpause-fast, unpause-gradual (discovery.lua)
  - `NumaflowControllerRollout`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Unknown; 5 health_test.yaml cases)
  - `PipelineRollout`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Unknown; 7 health_test.yaml cases); actions: allow-data-loss, disallow-data-loss, pause, unpause-fast, unpause-gradual (discovery.lua)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks; built-in actions are hidden when an argocd-cm `actions` override exists for the same group/kind unless it sets `mergeBuiltinActions: true`; action semantics belong to the resource-actions area (inventoried here only).
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block); actions overridable/extended via `resource.customizations.actions.<group>_<kind>` (`discovery.lua`, `definitions`, `mergeBuiltinActions`).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons); actions: API (`ListResourceActions`, `RunResourceAction`/`RunResourceActionV2`) / CLI (`argocd app actions list|run`) / UI (resource kebab menu).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (ISBServiceRollout: 7 cases; MonoVertexRollout: 7 cases; NumaflowControllerRollout: 5 cases; PipelineRollout: 7 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message; actions: pass each action's `action_test.yaml` fixtures in the resource-actions area.

### DIFF-176 Built-in customizations: `objectbucket.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/objectbucket.io/ObjectBucketClaim/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `objectbucket.io`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `ObjectBucketClaim`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 3 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (ObjectBucketClaim: 3 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-177 Built-in customizations: `ocs.openshift.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/ocs.openshift.io/StorageCluster/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `ocs.openshift.io`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `StorageCluster`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Unknown; 3 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (StorageCluster: 3 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-178 Built-in customizations: `onepassword.com`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/onepassword.com/OnePasswordItem/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `onepassword.com`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `OnePasswordItem`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 3 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (OnePasswordItem: 3 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-179 Built-in customizations: `openfaas.com`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/openfaas.com/Function/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `openfaas.com`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `Function`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended; 4 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (Function: 4 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-180 Built-in customizations: `opentelemetry.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/opentelemetry.io/OpenTelemetryCollector/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `opentelemetry.io`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `OpenTelemetryCollector`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 5 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (OpenTelemetryCollector: 5 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-181 Built-in customizations: `operator.knative.dev`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/operator.knative.dev/KnativeEventing/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/operator.knative.dev/KnativeServing/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `operator.knative.dev`. Kinds: 2; with health.lua: 2; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `KnativeEventing`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Unknown; 4 health_test.yaml cases)
  - `KnativeServing`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Unknown; 4 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (KnativeEventing: 4 cases; KnativeServing: 4 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-182 Built-in customizations: `operator.openshift.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/operator.openshift.io/IngressController/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `operator.openshift.io`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `IngressController`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 4 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (IngressController: 4 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-183 Built-in customizations: `operator.victoriametrics.com`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/operator.victoriametrics.com/_/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `operator.victoriametrics.com` (directory name contains `_`, matched as a doublestar glob where `_` becomes `*`; used only when no exact built-in script and no argocd-cm override matches). Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `_` (wildcard: any kind): health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended; 5 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (_: 5 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-184 Built-in customizations: `operators.coreos.com`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/operators.coreos.com/Subscription/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `operators.coreos.com`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `Subscription`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 9 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (Subscription: 9 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-185 Built-in customizations: `pkg.crossplane.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/pkg.crossplane.io/Provider/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `pkg.crossplane.io`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `Provider`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 4 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (Provider: 4 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-186 Built-in customizations: `platform.confluent.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/platform.confluent.io/Connect/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/platform.confluent.io/Connector/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/platform.confluent.io/ControlCenter/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/platform.confluent.io/Kafka/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/platform.confluent.io/KsqlDB/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/platform.confluent.io/SchemaRegistry/health.lua#L1 (+1 more files in the same directory tree)
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `platform.confluent.io`. Kinds: 7; with health.lua: 7; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `Connect`: health.lua (statuses referenced: Healthy, Progressing; 2 health_test.yaml cases)
  - `Connector`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 3 health_test.yaml cases)
  - `ControlCenter`: health.lua (statuses referenced: Healthy, Progressing; 2 health_test.yaml cases)
  - `Kafka`: health.lua (statuses referenced: Healthy, Progressing; 2 health_test.yaml cases)
  - `KsqlDB`: health.lua (statuses referenced: Healthy, Progressing; 2 health_test.yaml cases)
  - `SchemaRegistry`: health.lua (statuses referenced: Healthy, Progressing; 2 health_test.yaml cases)
  - `Zookeeper`: health.lua (statuses referenced: Healthy, Progressing; 2 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (Connect: 2 cases; Connector: 3 cases; ControlCenter: 2 cases; Kafka: 2 cases; KsqlDB: 2 cases; SchemaRegistry: 2 cases; Zookeeper: 2 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-187 Built-in customizations: `policy`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/policy/PodDisruptionBudget/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `policy`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `PodDisruptionBudget`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 3 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (PodDisruptionBudget: 3 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-188 Built-in customizations: `policy.open-cluster-management.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/policy.open-cluster-management.io/CertificatePolicy/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/policy.open-cluster-management.io/ConfigurationPolicy/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/policy.open-cluster-management.io/OperatorPolicy/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/policy.open-cluster-management.io/Policy/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `policy.open-cluster-management.io`. Kinds: 4; with health.lua: 4; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `CertificatePolicy`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 3 health_test.yaml cases)
  - `ConfigurationPolicy`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 5 health_test.yaml cases)
  - `OperatorPolicy`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 6 health_test.yaml cases)
  - `Policy`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 8 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (CertificatePolicy: 3 cases; ConfigurationPolicy: 5 cases; OperatorPolicy: 6 cases; Policy: 8 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-189 Built-in customizations: `postgresql.cnpg.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/postgresql.cnpg.io/Cluster/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/postgresql.cnpg.io/Cluster/actions/discovery.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `postgresql.cnpg.io`. Kinds: 1; with health.lua: 1; with actions: 1. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `Cluster`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended, Unknown; 5 health_test.yaml cases); actions: promote, reload, restart, resume, suspend (discovery.lua)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks; built-in actions are hidden when an argocd-cm `actions` override exists for the same group/kind unless it sets `mergeBuiltinActions: true`; action semantics belong to the resource-actions area (inventoried here only).
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block); actions overridable/extended via `resource.customizations.actions.<group>_<kind>` (`discovery.lua`, `definitions`, `mergeBuiltinActions`).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons); actions: API (`ListResourceActions`, `RunResourceAction`/`RunResourceActionV2`) / CLI (`argocd app actions list|run`) / UI (resource kebab menu).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (Cluster: 5 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message; actions: pass each action's `action_test.yaml` fixtures in the resource-actions area.

### DIFF-190 Built-in customizations: `proclaim.dogmatiq.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/proclaim.dogmatiq.io/DNSSDServiceInstance/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `proclaim.dogmatiq.io`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `DNSSDServiceInstance`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Unknown; 7 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (DNSSDServiceInstance: 7 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-191 Built-in customizations: `projectcontour.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/projectcontour.io/ExtensionService/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/projectcontour.io/HTTPProxy/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `projectcontour.io`. Kinds: 2; with health.lua: 2; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `ExtensionService`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 4 health_test.yaml cases)
  - `HTTPProxy`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 6 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (ExtensionService: 4 cases; HTTPProxy: 6 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-192 Built-in customizations: `promoter.argoproj.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/promoter.argoproj.io/ArgoCDCommitStatus/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/promoter.argoproj.io/ChangeTransferPolicy/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/promoter.argoproj.io/ClusterScmProvider/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/promoter.argoproj.io/CommitStatus/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/promoter.argoproj.io/GitCommitStatus/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/promoter.argoproj.io/GitRepository/health.lua#L1 (+6 more files in the same directory tree)
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `promoter.argoproj.io`. Kinds: 11; with health.lua: 11; with actions: 1. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `ArgoCDCommitStatus`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Unknown; 8 health_test.yaml cases)
  - `ChangeTransferPolicy`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Unknown; 11 health_test.yaml cases)
  - `ClusterScmProvider`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Unknown; 7 health_test.yaml cases)
  - `CommitStatus`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Unknown; 9 health_test.yaml cases)
  - `GitCommitStatus`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Unknown; 12 health_test.yaml cases)
  - `GitRepository`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Unknown; 7 health_test.yaml cases)
  - `PromotionStrategy`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Unknown; 14 health_test.yaml cases)
  - `PullRequest`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Unknown; 8 health_test.yaml cases); actions: merge (discovery.lua)
  - `ScmProvider`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Unknown; 7 health_test.yaml cases)
  - `TimedCommitStatus`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Unknown; 9 health_test.yaml cases)
  - `WebRequestCommitStatus`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Unknown; 10 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks; built-in actions are hidden when an argocd-cm `actions` override exists for the same group/kind unless it sets `mergeBuiltinActions: true`; action semantics belong to the resource-actions area (inventoried here only).
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block); actions overridable/extended via `resource.customizations.actions.<group>_<kind>` (`discovery.lua`, `definitions`, `mergeBuiltinActions`).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons); actions: API (`ListResourceActions`, `RunResourceAction`/`RunResourceActionV2`) / CLI (`argocd app actions list|run`) / UI (resource kebab menu).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (ArgoCDCommitStatus: 8 cases; ChangeTransferPolicy: 11 cases; ClusterScmProvider: 7 cases; CommitStatus: 9 cases; GitCommitStatus: 12 cases; GitRepository: 7 cases; PromotionStrategy: 14 cases; PullRequest: 8 cases; ScmProvider: 7 cases; TimedCommitStatus: 9 cases; WebRequestCommitStatus: 10 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message; actions: pass each action's `action_test.yaml` fixtures in the resource-actions area.

### DIFF-193 Built-in customizations: `psmdb.percona.com`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/psmdb.percona.com/PerconaServerMongoDB/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/psmdb.percona.com/PerconaServerMongoDB/actions/discovery.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `psmdb.percona.com`. Kinds: 1; with health.lua: 1; with actions: 1. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `PerconaServerMongoDB`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended, Unknown; 6 health_test.yaml cases); actions: unpause (discovery.lua)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks; built-in actions are hidden when an argocd-cm `actions` override exists for the same group/kind unless it sets `mergeBuiltinActions: true`; action semantics belong to the resource-actions area (inventoried here only).
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block); actions overridable/extended via `resource.customizations.actions.<group>_<kind>` (`discovery.lua`, `definitions`, `mergeBuiltinActions`).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons); actions: API (`ListResourceActions`, `RunResourceAction`/`RunResourceActionV2`) / CLI (`argocd app actions list|run`) / UI (resource kebab menu).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (PerconaServerMongoDB: 6 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message; actions: pass each action's `action_test.yaml` fixtures in the resource-actions area.

### DIFF-194 Built-in customizations: `pxc.percona.com`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/pxc.percona.com/PerconaXtraDBCluster/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `pxc.percona.com`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `PerconaXtraDBCluster`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Unknown; 6 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (PerconaXtraDBCluster: 6 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-195 Built-in customizations: `rabbitmq.com`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/rabbitmq.com/Binding/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/rabbitmq.com/Exchange/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/rabbitmq.com/Permission/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/rabbitmq.com/Policy/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/rabbitmq.com/Queue/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/rabbitmq.com/RabbitmqCluster/health.lua#L1 (+3 more files in the same directory tree)
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `rabbitmq.com`. Kinds: 9; with health.lua: 9; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `Binding`: health.lua (statuses referenced: Healthy, Degraded, Unknown; 3 health_test.yaml cases)
  - `Exchange`: health.lua (statuses referenced: Healthy, Degraded, Unknown; 3 health_test.yaml cases)
  - `Permission`: health.lua (statuses referenced: Healthy, Degraded, Unknown; 3 health_test.yaml cases)
  - `Policy`: health.lua (statuses referenced: Healthy, Degraded, Unknown; 3 health_test.yaml cases)
  - `Queue`: health.lua (statuses referenced: Healthy, Degraded, Unknown; 3 health_test.yaml cases)
  - `RabbitmqCluster`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Unknown; 7 health_test.yaml cases)
  - `Shovel`: health.lua (statuses referenced: Healthy, Degraded, Unknown; 3 health_test.yaml cases)
  - `User`: health.lua (statuses referenced: Healthy, Degraded, Unknown; 3 health_test.yaml cases)
  - `Vhost`: health.lua (statuses referenced: Healthy, Degraded, Unknown; 3 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (Binding: 3 cases; Exchange: 3 cases; Permission: 3 cases; Policy: 3 cases; Queue: 3 cases; RabbitmqCluster: 7 cases; Shovel: 3 cases; User: 3 cases; Vhost: 3 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-196 Built-in customizations: `rds.aws.crossplane.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/rds.aws.crossplane.io/DBCluster/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/rds.aws.crossplane.io/DBInstance/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `rds.aws.crossplane.io`. Kinds: 2; with health.lua: 2; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `DBCluster`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended; 4 health_test.yaml cases)
  - `DBInstance`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended; 3 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (DBCluster: 4 cases; DBInstance: 3 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-197 Built-in customizations: `rollouts.kruise.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/rollouts.kruise.io/Rollout/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `rollouts.kruise.io`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `Rollout`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended; 4 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (Rollout: 4 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-198 Built-in customizations: `route.openshift.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/route.openshift.io/Route/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `route.openshift.io`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `Route`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 3 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (Route: 3 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-199 Built-in customizations: `route53.aws.crossplane.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/route53.aws.crossplane.io/ResourceRecordSet/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `route53.aws.crossplane.io`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `ResourceRecordSet`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended; 5 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (ResourceRecordSet: 5 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-200 Built-in customizations: `s3.aws.crossplane.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/s3.aws.crossplane.io/Bucket/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `s3.aws.crossplane.io`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `Bucket`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended; 2 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (Bucket: 2 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-201 Built-in customizations: `services.cloud.sap.com`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/services.cloud.sap.com/ServiceBinding/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/services.cloud.sap.com/ServiceInstance/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `services.cloud.sap.com`. Kinds: 2; with health.lua: 2; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `ServiceBinding`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 5 health_test.yaml cases)
  - `ServiceInstance`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 5 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (ServiceBinding: 5 cases; ServiceInstance: 5 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-202 Built-in customizations: `serving.knative.dev`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/serving.knative.dev/Service/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `serving.knative.dev`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `Service`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Unknown; 3 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (Service: 3 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-203 Built-in customizations: `serving.kserve.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/serving.kserve.io/InferenceService/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `serving.kserve.io`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `InferenceService`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended, Unknown; 11 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (InferenceService: 11 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-204 Built-in customizations: `snapshot.storage.k8s.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/snapshot.storage.k8s.io/VolumeSnapshot/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/snapshot.storage.k8s.io/VolumeSnapshotContent/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `snapshot.storage.k8s.io`. Kinds: 2; with health.lua: 2; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `VolumeSnapshot`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 3 health_test.yaml cases)
  - `VolumeSnapshotContent`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 3 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (VolumeSnapshot: 3 cases; VolumeSnapshotContent: 3 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-205 Built-in customizations: `solr.apache.org`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/solr.apache.org/SolrCloud/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `solr.apache.org`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `SolrCloud`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 3 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (SolrCloud: 3 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-206 Built-in customizations: `source.toolkit.fluxcd.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/source.toolkit.fluxcd.io/Bucket/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/source.toolkit.fluxcd.io/Bucket/actions/discovery.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/source.toolkit.fluxcd.io/GitRepository/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/source.toolkit.fluxcd.io/GitRepository/actions/discovery.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/source.toolkit.fluxcd.io/HelmChart/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/source.toolkit.fluxcd.io/HelmChart/actions/discovery.lua#L1 (+4 more files in the same directory tree)
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `source.toolkit.fluxcd.io`. Kinds: 5; with health.lua: 5; with actions: 5. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `Bucket`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended, Unknown; 3 health_test.yaml cases); actions: reconcile, resume, suspend (discovery.lua)
  - `GitRepository`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended, Unknown; 3 health_test.yaml cases); actions: reconcile, resume, suspend (discovery.lua)
  - `HelmChart`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended, Unknown; 3 health_test.yaml cases); actions: reconcile, resume, suspend (discovery.lua)
  - `HelmRepository`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended, Unknown; 4 health_test.yaml cases); actions: reconcile, resume, suspend (discovery.lua)
  - `OCIRepository`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended, Unknown; 3 health_test.yaml cases); actions: reconcile, resume, suspend (discovery.lua)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks; built-in actions are hidden when an argocd-cm `actions` override exists for the same group/kind unless it sets `mergeBuiltinActions: true`; action semantics belong to the resource-actions area (inventoried here only).
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block); actions overridable/extended via `resource.customizations.actions.<group>_<kind>` (`discovery.lua`, `definitions`, `mergeBuiltinActions`).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons); actions: API (`ListResourceActions`, `RunResourceAction`/`RunResourceActionV2`) / CLI (`argocd app actions list|run`) / UI (resource kebab menu).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (Bucket: 3 cases; GitRepository: 3 cases; HelmChart: 3 cases; HelmRepository: 4 cases; OCIRepository: 3 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message; actions: pass each action's `action_test.yaml` fixtures in the resource-actions area.

### DIFF-207 Built-in customizations: `sparkoperator.k8s.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/sparkoperator.k8s.io/SparkApplication/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `sparkoperator.k8s.io`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `SparkApplication`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Missing; 7 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (SparkApplication: 7 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-208 Built-in customizations: `spot.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/spot.io/SpotDeployment/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `spot.io`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `SpotDeployment`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Suspended; 3 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (SpotDeployment: 3 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-209 Built-in customizations: `stacks.crossplane.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/stacks.crossplane.io/ClusterStackInstall/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `stacks.crossplane.io`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `ClusterStackInstall`: health.lua (statuses referenced: Healthy, Progressing; 3 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (ClusterStackInstall: 3 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-210 Built-in customizations: `tower.ansible.com`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/tower.ansible.com/AnsibleJob/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `tower.ansible.com`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `AnsibleJob`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 9 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (AnsibleJob: 9 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-211 Built-in customizations: `trident.netapp.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/trident.netapp.io/TridentBackendConfig/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/trident.netapp.io/TridentOrchestrator/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `trident.netapp.io`. Kinds: 2; with health.lua: 2; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `TridentBackendConfig`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 3 health_test.yaml cases)
  - `TridentOrchestrator`: health.lua (statuses referenced: Healthy, Progressing, Degraded; 3 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (TridentBackendConfig: 3 cases; TridentOrchestrator: 3 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-212 Built-in customizations: `work.karmada.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/work.karmada.io/ClusterResourceBinding/health.lua#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/work.karmada.io/ResourceBinding/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `work.karmada.io`. Kinds: 2; with health.lua: 2; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `ClusterResourceBinding`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Unknown; 8 health_test.yaml cases)
  - `ResourceBinding`: health.lua (statuses referenced: Healthy, Progressing, Degraded, Unknown; 8 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (ClusterResourceBinding: 8 cases; ResourceBinding: 8 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

### DIFF-213 Built-in customizations: `zookeeper.pravega.io`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/zookeeper.pravega.io/ZookeeperCluster/health.lua#L1
- **Maturity**: GA
- **Semantics**: Embedded Lua customizations shipped for API group `zookeeper.pravega.io`. Kinds: 1; with health.lua: 1; with actions: 0. Built-in health scripts always run with Lua standard libraries enabled; an argocd-cm health override for the same group/kind (exact or wildcard) takes precedence.
  - `ZookeeperCluster`: health.lua (statuses referenced: Healthy, Progressing; 2 health_test.yaml cases)
- **Edge cases**: every case in each kind's `health_test.yaml` (inputs under `testdata/`) is the normative expectation for status and message; scripts must tolerate missing `status`/`conditions` blocks.
- **Configuration surface**: none required (embedded in the binary); health overridable via `resource.customizations.health.<group>_<kind>` or `resource.customizations` (YAML block).
- **Exposure**: CRD (Application `status.resources[].health`, resource tree node health) / API (ResourceTree, Get) / CLI (`argocd app get`, `argocd app resources --output tree=detailed`) / UI (resource tree health icons).
- **Tests required**: port and pass all upstream health_test.yaml fixtures (ZookeeperCluster: 2 cases); negative: an argocd-cm health override for the same group/kind replaces the built-in script; recovery: a script runtime error yields Unknown with the error text as message.

## Typed schema resolution (structured-merge parsing)

These features cover which schema Argo CD uses when it converts objects into structured-merge-diff typed values. They refine the schema statements in DIFF-008 (structured-merge diff), DIFF-010 (webhook mutation removal) and DIFF-021 (`managedFieldsManagers`). Where those sections say "cluster OpenAPI GVK parser", DIFF-214 gives the exact precedence.

### DIFF-214 Typed schema resolution order (static built-in schema before cluster GVK parser)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/utils/kube/scheme/typed.go#L17, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/utils/kube/scheme/typed.go#L28, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/utils/kube/scheme/typed.go#L51, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/utils/kube/scheme/typed.go#L58, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/diff.go#L423, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/diff/diff.go#L436, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L889, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L899, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L3024, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/faq.md#L422
- **Maturity**: GA
- **Semantics**: `ResolveParseableType(gvk, parser)` picks the typed schema in this order:
  1. If `parser` (the destination cluster's GVK parser) is nil, it returns the deduced type (DIFF-218).
  2. It looks up the GVK's OpenAPI definition name in a process-wide GVK-to-definition-name map. That map is extracted once per process (`sync.Once`) from the private `gvks` map of the *first* non-nil parser passed in. If a name is found and the compiled-in static schema (DIFF-215) has a valid type with that name, the static type is returned. The cluster's own schema for that GVK is then ignored, even when the cluster is newer or older than the compiled-in Kubernetes version.
  3. Otherwise it returns `parser.Type(gvk)` from the current call's cluster parser. This is nil when the GVK is not published in the cluster's OpenAPI document.

  In practice, built-in kinds whose definitions come from `k8s.io/api` resolve statically. CRDs, `apiextensions.k8s.io/CustomResourceDefinition`, `apiregistration.k8s.io/APIService` and aggregated APIs resolve from the cluster parser, because the static schema has none of their definition names.

  Callers of this resolution:
  - the structured-merge diff (DIFF-008), for both config and live
  - `managedFieldsManagers` pre-diff normalization (DIFF-021), on these paths:
    - controller `CompareAppState`
    - the controller's Secret re-diff in `hideSecretData`
    - the ServerSideDiff API's pre-diff normalization
    - CLI client-side diff, which has no parser (DIFF-218)

  Paths that do *not* use it:
  - server-side diff webhook-mutation removal (DIFF-220): cluster parser only
  - managed-fields version conversion inside structured-merge diff (DIFF-221): cluster parser only
  - legacy 3-way/2-way diff (DIFF-002/003): no typed parsing
- **Edge cases**:
  - **The name map is frozen from the first parser.** In a multi-cluster controller, or an API server answering ServerSideDiff for several clusters, the map comes from whichever cluster's parser reached this code first. Later CRD or APIService reloads (DIFF-073) never refresh it. A built-in GVK that the first cluster did not serve has no entry, so it resolves from the current cluster's parser instead of the static schema. Examples are an alpha/beta API or an API version enabled only on a later cluster. The same applies when the first cluster published the definition under an aggregator-suffixed name (`..._v2`).
  - A definition name that is present in the map but missing from the static schema (any CRD) falls through to the cluster parser.
  - The static type wins even when the cluster serves a different shape for the same built-in GVK. Fields added after the compiled-in version, or removed before it, then produce `field not declared in schema` (DIFF-216, DIFF-217).
  - If the cluster parser has no type for the GVK, structured-merge diff errors with `unable to resolve parseableType for GroupVersionKind: <gvk>`. `managedFieldsManagers` normalization falls back to the deduced type instead.
- **Configuration surface**: none. No flag, env var or ConfigMap key selects the schema source. The static schema version is fixed at build time (DIFF-215).
- **Exposure**: internal. It is observable through Application `status.conditions` (ComparisonError), resource sync status, ManagedResources/ServerSideDiff API results, `argocd app diff` and UI diff panels.
- **Tests required**:
  - **Positive, static schema wins:**
    - `apps/v1` Deployment where the cluster parser's Deployment schema lacks a field that the static schema declares: typed parsing succeeds.
    - Same kind where the cluster schema has an extra field that the static schema lacks, and the manifest uses it: structured-merge diff fails.
  - **Positive, CRD:** the cluster parser's schema is used. A field declared in the CRD schema parses. An undeclared field outside `x-kubernetes-preserve-unknown-fields` fails.
  - **Negative:**
    - Non-built-in GVK missing from the cluster OpenAPI under structured-merge diff: `unable to resolve parseableType for GroupVersionKind: ...`.
    - Nil parser: the deduced type is used and nothing errors.
  - **Parity quirk, multi-cluster:** cluster A (seen first) does not serve built-in GVK X and cluster B does. Diffs of X on B use B's published schema, not the static one.

### DIFF-215 Compiled-in static Kubernetes schema (version and coverage)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/utils/kube/scheme/parser.go#L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/utils/kube/scheme/parser.go#L31, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/utils/kube/scheme/parser.go#L44, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/utils/kube/scheme/parser.go#L1781, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/hack/update_static_schema.sh#L6, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/hack/update_static_schema.sh#L9, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/go.mod#L19, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/go.mod#L121, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/faq.md#L424, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/faq.md#L429, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/faq.md#L443
- **Maturity**: GA
- **Semantics**:
  - **Source.** `StaticParser()` is a structured-merge-diff `typed.Parser` built from a YAML schema embedded in the binary. The file is a verbatim copy of client-go `applyconfigurations/internal/internal.go` at the client-go version pinned in `gitops-engine/go.mod`: `k8s.io/client-go v0.36.1`, i.e. Kubernetes 1.36 API shapes. It is byte-identical, after the script's header/package/function renames, to client-go v0.36.1's file.
  - **Coverage.** 861 named types:
    - all `k8s.io/api` groups, including core, apps, batch, autoscaling, networking, policy, rbac, storage, admissionregistration, flowcontrol, resource, scheduling and extensions
    - `io.k8s.apimachinery` meta types (ObjectMeta, ManagedFieldsEntry and others)
    - `__untyped_atomic_` and `__untyped_deduced_`

    It has no `apiextensions.k8s.io` or `apiregistration.k8s.io` types and no CRD types.
  - **List/map semantics.** Each type carries Kubernetes SMD semantics: associative list keys such as `containers` keyed by `name`, atomic and granular relationships, unions, and field defaults. Validation is strict: a map field that is not declared, and has no element type, is rejected.
  - **Loading.** The parser is built lazily on first use and cached for the process. A parse failure panics (`Failed to parse schema: ...`), which cannot happen with the shipped content.
- **Edge cases**:
  - When a cluster is newer than Kubernetes 1.36 and adds fields to a built-in kind, any manifest *or live object* that carries those fields fails typed parsing on the structured-merge paths. This includes server-populated status fields.
  - Fields that exist only in older API shapes and were dropped by 1.36 fail in the same way.
  - The FAQ's only fix is to upgrade to an Argo CD build whose static schema contains the field. There is no runtime override.
- **Configuration surface**: build-time only. `gitops-engine/hack/update_static_schema.sh` reads the client-go version from `go.mod` and downloads that version's `internal.go`. No Makefile target invokes it at this commit.
- **Exposure**: internal (no CRD/API/CLI/UI surface).
- **Tests required**:
  - **Positive:**
    - Patrol ships an equivalent schema for Kubernetes 1.36.
    - Every built-in kind in `k8s.io/api` v0.36.1 resolves, for example `io.k8s.api.apps.v1.Deployment`.
    - A field present in 1.36, such as Deployment `spec.template.spec.containers[].image`, is accepted.
    - Associative-list keys match upstream, for example `containers` keyed by `name` and `ports` keyed by `containerPort`+`protocol`.
  - **Negative:**
    - A synthetic field not in 1.36 on a built-in kind is rejected with `field not declared in schema`.
    - `CustomResourceDefinition`, `APIService` and CRD kinds are not resolved statically.

### DIFF-216 `field not declared in schema` and other typed-validation failures in structured-merge diff
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/diff.go#L113, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/diff.go#L118, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/diff.go#L429, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/diff.go#L433, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/diff.go#L1257, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/diff/diff.go#L336, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L909, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L922, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/faq.md#L422, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/faq.md#L448
- **Maturity**: GA
- **Semantics**:
  - **When it runs.** Structured-merge diff applies when the app has sync option `ServerSideApply=true`, or the resource carries `argocd.argoproj.io/sync-options: ServerSideApply=true`, and server-side diff is off. Live and config are each converted with the type resolved by DIFF-214, using strict validation: `FromUnstructured` without `AllowDuplicates`.
  - **Unknown fields reach the parser.** `remarshal` decodes with `DisallowUnknownFields`, and on failure it returns the original object. Unknown fields therefore survive to the typed parser.
  - **Failure.** Any validation error aborts the comparison of the *whole application*, not just that resource:
    - The error chain is `failed to calculate diff: error calculating structured merge diff: error building typed value from config resource: <path>: field not declared in schema`, or `... from live resource: ...`.
    - The controller records a `ComparisonError` condition `Failed to compare desired state to live state: <chain>`.
    - Sync status becomes `Unknown` for every resource (DIFF-001).
  - **Error format.**
    - Paths are `.`-prefixed field segments with list keys, for example `.spec.template.spec.containers[name="app"].foo`.
    - Inside one map, iteration stops at the first undeclared key.
    - Several errors are joined as `errors:\n  <e1>\n  <e2>`.
  - **Other validation failures on the same path:**
    - scalar mismatches: `expected string, got ...`, `expected numeric (int or float), got ...`, `expected boolean, got ...`
    - duplicate associative-list keys: `duplicate entries for key [name="X"]`, for example two env vars with the same name in one container
  - **Workarounds (FAQ):**
    - remove the `ServerSideApply=true` sync option or annotation (legacy diff is used)
    - enable server-side diff (DIFF-009), which bypasses structured-merge diff
    - upgrade Argo CD
- **Edge cases**:
  - **Live-side failures:** a live object from a newer cluster with new server-populated fields, such as status, fails even if Git never mentions them.
  - **CRDs:** a Git field that is not in the CRD's structural schema, and not under `x-kubernetes-preserve-unknown-fields`, also fails, although the API server would silently prune it on apply.
  - **Create/delete pairs:** pairs where one side is nil never reach typed parsing.
  - **Server-side diff:** when it is on, structured-merge diff is not used.
  - **CLI client-side diff:** `argocd app diff` without server-side diff has no GVK parser, so it uses the deduced type (DIFF-218) and never reports this error. It can disagree with the controller's ComparisonError.
- **Configuration surface**:
  - `spec.syncPolicy.syncOptions: [ServerSideApply=true]`
  - resource annotation `argocd.argoproj.io/sync-options: ServerSideApply=true`
  - bypass: `argocd.argoproj.io/compare-options: ServerSideDiff=true`, or controller `--server-side-diff-enabled` / `controller.diff.server.side`
- **Exposure**:
  - CRD: `status.conditions[type=ComparisonError]`, `status.sync.status=Unknown`
  - API: Application Get/Watch, ManagedResources
  - CLI: `argocd app get` shows the condition
  - UI: application conditions panel and sync status
- **Tests required**:
  - **Positive:**
    - SSA app whose Deployment manifest has an undeclared `spec.foo`: ComparisonError containing `error building typed value from config resource: .spec.foo: field not declared in schema`, and all resources Unknown.
    - Live object with an undeclared status field: same error, but `from live resource`.
    - Duplicate env var names: `duplicate entries for key`.
    - Only one resource annotated with SSA while the app is not: that resource triggers the failure for the whole app.
  - **Negative:**
    - The same manifest without SSA compares normally (legacy diff).
    - With `ServerSideDiff=true`, no such error.
    - A CRD field under `x-kubernetes-preserve-unknown-fields` gives no error.
  - **Recovery:** after fixing the manifest, the next refresh clears the condition.

### DIFF-217 Typed-validation failure in `managedFieldsManagers` normalization (silent skip)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/diff/diff.go#L431, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/diff/diff.go#L436, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/managedfields/managed_fields.go#L35, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/managedfields/managed_fields.go#L98, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/managedfields/managed_fields_test.go#L153, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/faq.md#L426, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/faq.md#L450
- **Maturity**: GA
- **Semantics**:
  - **When it runs.** Pre-diff `managedFieldsManagers` normalization (DIFF-021) runs only when both sides exist and a matching ignore rule lists managers. It resolves the type through DIFF-214, so built-in kinds use the static 1.36 schema.
  - **Silent failure.** If converting live or target to a typed value fails, or the typed comparison fails, the error is only logged at debug level (`error building typed results: <err>`). Unmodified deep copies are returned. Normalization is skipped for that resource alone, no condition is raised, and the comparison continues. Causes include an undeclared field, a type mismatch or duplicate associative keys.
  - **Visible effect.** Fields that trusted managers own and Git changes are *not* ignored, so the resource can unexpectedly show `OutOfSync`. No `field not declared in schema` message appears anywhere.
  - **Error that still surfaces.** Only a managed-fields JSON decode failure still errors: `error normalizing manager <m>: ...`.
  - **Scope.** The step runs before diff-mode selection. It therefore applies under legacy diff, structured-merge diff and server-side diff, and in the ServerSideDiff API and the controller's Secret re-diff.
  - **Other normalizers unaffected.** `jsonPointers`/`jqPathExpressions` rules for the same resource are applied later by the normalizer and are unaffected.
- **Edge cases**:
  - Under SSA (structured-merge diff), the later structured-merge step fails anyway, with DIFF-216's error, for the same undeclared field.
  - Under server-side diff or legacy diff, the skip is the only symptom.
  - An empty manager list returns early before any typed conversion. That is what upstream's "does not fail if object fails validation schema" test exercises.
- **Configuration surface**: `managedFieldsManagers` in Application `spec.ignoreDifferences[]`, `resource.customizations.ignoreDifferences.<group_kind>` and `resource.customizations.ignoreDifferences.all`.
- **Exposure**:
  - CRD: `spec.ignoreDifferences`, resource `status`
  - API: ManagedResources, ServerSideDiff
  - UI: diff shows the non-ignored field
- **Tests required**:
  - **Positive:** Deployment with `managedFieldsManagers: [kube-controller-manager]` whose `spec.replicas` is changed in Git, under legacy diff: Synced.
  - **Negative:** the same, plus an undeclared field elsewhere in the manifest: no ComparisonError, but the resource is OutOfSync on `spec.replicas`. A `jsonPointers` rule on the same resource is still honored.
  - **Error case:** corrupt `managedFields[].fieldsV1` produces a ComparisonError.
  - **Recovery:** after the undeclared field is removed, the next refresh shows Synced.

### DIFF-218 Deduced parseable-type fallback (no schema available)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/utils/kube/scheme/typed.go#L18, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/managedfields/managed_fields.go#L99, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/managedfields/managed_fields_test.go#L164, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/app_diff.go#L333, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/app_diff.go#L768, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L889, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/go.mod#L131
- **Maturity**: GA
- **Semantics**:
  - **What the deduced type is.** It is structured-merge-diff's `DeducedParseableType` (`sigs.k8s.io/structured-merge-diff/v6` v6.4.0, `typed/parser.go`, type `__untyped_deduced_`):
    - every map is *separable*: per-key granular and recursive
    - every list is *atomic*, and maps nested inside lists are atomic too
    - scalars are untyped
    - any field is accepted, so it never yields `field not declared in schema`

    Note that the upstream code comment ("lists and maps are manipulated atomically") is imprecise about maps.
  - **When it is used:**
    - when the GVK parser is nil, `ResolveParseableType` returns it. This covers CLI client-side `argocd app diff`, the default when server-side diff is not requested, whose diff config never sets a parser. It also covers the controller when the cluster-cache lookup for the parser fails: an `UnknownError` condition carrying the error is added and the comparison continues with a nil parser.
    - in `managedFieldsManagers` normalization, when the resolved type is nil, for example a CRD GVK not published in the cluster OpenAPI.
  - **When it is not used.** Structured-merge diff with a non-nil parser and an unresolvable GVK errors instead (DIFF-214). Server-side diff's webhook removal never uses it.
  - **Effect on `managedFieldsManagers`.** A field inside a map owned by a trusted manager, such as `spec.replicas`, is still matched and removed. List content is compared as a whole value, so results for list items can differ from the schema-aware controller result.
  - **Effect on CLI client-side diff.** A resource annotated `argocd.argoproj.io/sync-options: ServerSideApply=true` gets structured-merge diff with the deduced type and an empty manager name, because the CLI never sets `WithManager`. Lists are then merged atomically.
- **Edge cases**:
  - The deduced type cannot detect undeclared fields, so CLI diffs can succeed where the controller raises a ComparisonError (DIFF-216). CLI output can also differ from the controller's sync status for list fields.
  - The upstream comment on `ResolveParseableType` says the deduced type is "not suitable for calculating diffs", but structured-merge diff still uses it whenever the parser is nil.
- **Configuration surface**: none. It is triggered by a missing parser or type.
- **Exposure**: CLI (`argocd app diff` client-side mode), plus the controller fallback (UnknownError condition).
- **Tests required**:
  - **Positive:**
    - `managedfields.Normalize` with a nil type does not panic and returns non-nil objects (port the upstream "does not panic when pt is nil" test).
    - CRD absent from the cluster OpenAPI with `managedFieldsManagers` owning `spec.replicas`, where Git changes replicas: ignored.
    - CLI client-side diff of an SSA-annotated Deployment containing an undeclared field: no error.
  - **Negative:** structured-merge diff with a non-nil parser lacking the GVK errors rather than falling back.
  - **Recovery:** once the cluster cache becomes available, the next comparison uses schema-aware types and the UnknownError condition clears.

### DIFF-219 Cluster OpenAPI GVK parser construction and duplicate-GVK de-duplication
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/utils/kube/ctl.go#L129, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/utils/kube/ctl.go#L147, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/utils/kube/ctl.go#L157, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/utils/kube/ctl.go#L161, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/utils/kube/uniqueprotomodels.go#L11, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/utils/kube/uniqueprotomodels.go#L91, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/utils/kube/uniqueprotomodels.go#L121, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/cache/cluster.go#L937, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/cache/cluster.go#L1123, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sync.go#L47, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L3024
- **Maturity**: GA
- **Semantics**:
  - **Construction.** `LoadOpenAPISchema` fetches the cluster's OpenAPI v2 document through discovery, converts it to proto models, de-duplicates GVKs, and calls `NewGVKParser(models, preserveUnknownFields=false)`.
  - **De-duplication.** Model names are walked in sorted (lexicographic) order. The first model that declares a GVK wins. A later model that declares any GVK already seen is dropped *entirely*, and its GVK is recorded as tainted. Tainted GVKs are logged at info level: `Duplicate GVKs detected in OpenAPI schema. This could cause inaccurate diffs.` with key `gvks`. The parser is still built; upstream prefers an imprecise parser to none.
  - **Unknown fields.** With `preserveUnknownFields=false`, unknown fields are tolerated only where the published schema has `x-kubernetes-preserve-unknown-fields`.
  - **Controller lifecycle.**
    - The parser is built during cluster-cache sync and stored per cluster.
    - It is reloaded on CRD and APIService events (DIFF-073).
    - It is replaced only when the reload returns a non-nil parser. On a reload error the previous parser stays.
    - Comparisons read it through `getGVKParser`.
  - **API server.** The ServerSideDiff RPC loads a fresh parser from the destination cluster on every request.
- **Edge cases**:
  - **Aggregated APIs** built against older Kubernetes libraries, such as metrics-server, cause the API aggregator to emit `_vN`-suffixed duplicate definitions. The lexicographically first one is kept, and it may be the stale shape.
  - **Initial-sync failure** fails the cluster sync: `failed to load open api schema while syncing cluster cache: ...`. Possible causes:
    - discovery client creation: `failed to create discovery client`
    - OpenAPI parse: `error getting openapi resources`
    - parser creation: `error getting gvk parser: failed to create GVK parser: ...` or `error getting openapi schema` / `error getting openapi data`
  - **ServerSideDiff API failure** returns `failed to get OpenAPI schema: ...` to the caller.
  - **Static schema precedence.** Built-in kinds still prefer the static schema (DIFF-214), whatever the cluster publishes.
- **Configuration surface**: none.
- **Exposure**: internal for the controller. Errors surface on the API side through ServerSideDiff and as cluster connection errors.
- **Tests required**:
  - **Positive:**
    - OpenAPI document with two definitions for one GVK: the parser is built, a warning is logged listing the GVK, and the lexicographically first definition is used.
    - Document without duplicates: no warning.
    - After a CRD is added and the schema reloaded, the new CRD GVK resolves.
  - **Negative:** OpenAPI endpoint failure during initial sync gives a cluster-sync error and app ComparisonErrors.
  - **Recovery:** a reload failure keeps the previous parser and diffs keep working.
  - **API:** ServerSideDiff against a cluster whose OpenAPI fails returns `failed to get OpenAPI schema`.

### DIFF-220 Schema source for server-side diff (cluster schema only; static schema not used)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/diff.go#L93, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/diff.go#L179, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/diff.go#L223, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/diff.go#L228, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L885, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L3051, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/faq.md#L450, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/diff-strategies.md#L111
- **Maturity**: GA
- **Semantics**:
  - **No static schema.** The server-side diff path (DIFF-009) never calls `ResolveParseableType` and never consults the static schema.
  - **Webhook-mutation removal.** With the default `IncludeMutationWebhook` unset (DIFF-010), this is the only typed step. It calls `gvkParser.Type(gvk)` on the destination cluster's parser *directly*. Built-in kinds are therefore typed with the schema the cluster itself publishes, which matches what the API server accepted in the dry run. An outdated compiled-in schema cannot cause `field not declared in schema` here.
  - **`IncludeMutationWebhook=true`.** No typed parsing occurs at all; raw dry-run output is compared.
  - **Config is never typed.** Only the dry-run result and live are typed.
  - **Pre-diff normalization still uses static.** The `managedFieldsManagers` pre-diff normalization still uses DIFF-214 resolution under server-side diff, with the silent skip of DIFF-217.
- **Edge cases**: each of these fails the app comparison as a ComparisonError.
  - The GVK is missing from the cluster parser: `unable to resolve parseableType for GroupVersionKind: <gvk>`.
  - A predicted-live or live typed conversion fails: `error converting predicted live state from unstructured to <gvk>: ...` or `error converting live state from unstructured to <gvk>: ...`.

  All of these are wrapped as `error calculating server side diff: serverSideDiff error: error removing non config mutations for resource <Kind>/<name>: ...`.
- **Configuration surface**:
  - enable: `argocd.argoproj.io/compare-options: ServerSideDiff=true`, or controller `--server-side-diff-enabled` / `controller.diff.server.side`
  - skip typed parsing: `argocd.argoproj.io/compare-options: IncludeMutationWebhook=true`
- **Exposure**:
  - CRD: annotations
  - API: ServerSideDiff RPC `GET /api/v1/applications/{appName}/server-side-diff` uses the per-request parser with the same rules
  - CLI: `argocd app diff --server-side-diff`
  - UI: indirect, through cached diffs
- **Tests required**:
  - **Positive:**
    - Server-side diff on a cluster whose Deployment schema has a field absent from the static schema, with a manifest using that field: no error, and the diff is computed.
    - The same app switched from server-side diff to SSA-only structured-merge diff: DIFF-216 error, which proves the schema-source difference.
    - With `IncludeMutationWebhook=true`, no typed conversion: a GVK absent from the cluster parser still diffs.
  - **Negative:** default server-side diff for a GVK missing from the cluster OpenAPI gives `unable to resolve parseableType` as a ComparisonError.

### DIFF-221 Managed-fields version conversion during structured-merge diff
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/diff.go#L474, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/diff.go#L498, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/internal/fieldmanager/wrapper.go#L19, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/internal/fieldmanager/borrowed_versionconverter.go#L63, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/internal/fieldmanager/borrowed_versionconverter.go#L99, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/internal/fieldmanager/borrowed_typeconverter.go#L84, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/internal/fieldmanager/borrowed_typeconverter.go#L112, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/diff_test.go#L2409, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/diff/diff_test.go#L2483
- **Maturity**: GA
- **Semantics**:
  - **When conversion happens.** Structured-merge diff runs `merge.Updater.Apply` with a version converter. Converting an object to the API version a manager recorded is skipped when that version equals the object's own version.
  - **How it converts.** Otherwise the object goes through client-go's `kubernetes/scheme.Scheme` with hub version `<group>/__internal`. The converted object is then typed with the destination cluster's parser, `parser.Type(gvk)`, *not* the static schema.
  - **Missing versions are dropped.** Two errors count as a missing version: a type not registered in the Go scheme (every CRD), or `no corresponding type for <gvk>` (absent from the cluster OpenAPI). The manager entries recorded at that version are then dropped from the managed set, so their ownership is ignored. This rule comes from structured-merge-diff v6.4.0 `merge/update.go`.
  - **Other errors fail.** Any other conversion error aborts the structured-merge diff, wrapped as `error calculating structured merge diff: error calculating diff: error while running updater.Apply: ...`.
  - **Regression test.** Upstream tests cover an HPA with config at `autoscaling/v2` and a manager entry recorded at `autoscaling/v1`:
    - identical objects: `Modified=false`
    - changed replicas: `Modified=true`
- **Edge cases**:
  - CRD manager entries at another served version are silently disregarded, which can change conflict and ownership outcomes compared with a real server-side apply.
  - Live `managedFields` that cannot be decoded fail with `error decoding managed fields: ...`.
  - The CLI client-side diff has no parser, see DIFF-218 and Unconfirmed.
- **Configuration surface**: same triggers as DIFF-008: the `ServerSideApply=true` sync option or annotation, with server-side diff off.
- **Exposure**: CRD (sync options) and the resulting sync status and diff. No dedicated API, CLI or UI surface.
- **Tests required**:
  - **Positive:** port `TestStructuredMergeDiff_HPAv2ToV1Conversion` and `TestStructuredMergeDiff_HPAv2ToV1Conversion_Modified`.
  - **CRD:** live `managedFields` recorded at `v1beta1` while config is `v1` diffs without error, and the v1beta1 entries are ignored.
  - **Negative:** malformed `managedFields` fails with `error decoding managed fields` as a ComparisonError.

## Unconfirmed

- **System-level `managedFieldsManagers` for core-group kinds.** `HasIgnoreDifference` looks up overrides with key `fmt.Sprintf("%s/%s", group, kind)`, which is `/Service` for the core group, while `convertToOverrideKey` stores `resource.customizations.ignoreDifferences.Service` under key `Service`. From reading the code, managers configured for a core kind by group/kind (not via `.all`) seem never to be applied, although jsonPointers/jqPathExpressions for the same key do work through the glob normalizer. No upstream test covers a core kind. Paths checked: `util/argo/diff/ignore.go#L46`, `util/settings/settings.go#L1210`, `util/argo/diff/ignore_test.go`, `util/argo/normalizers/util.go#L8`.
- **Safe `os` module registration without `useOpenLibs`.** `OpenSafeOs` registers `time`/`date` with `l.RegisterModule(lua.TabLibName, ...)`, so the module name is `table`, not `os`. A safe module is also preloaded for `require("os")`. Whether a global `os` table exists for user scripts with open libs disabled was not checked by running code. Built-in scripts use `local os = require("os")`. Paths checked: `util/lua/oslib_safe.go#L16`, `util/lua/lua.go#L81-L100`, `resource_customizations/postgresql.cnpg.io/Cluster/actions/restart/action.lua#L1`.
- **What `useOpenLibs=true` exposes.** In that mode `lua.NewState` opens every gopher-lua library, including the full `os` and `io`. A later `require("os")` then returns the full library rather than the safe subset. This conclusion comes from reading gopher-lua's preload semantics. It was not run. Paths checked: `util/lua/lua.go#L76-L100`.
- **Inventory status hints and test counts.** The "statuses referenced" list for each built-in script is a regex scan for quoted status literals. Scripts that copy a status from the object (for example, child health) can return codes that are not listed. Test-case counts are the number of `inputPath:` entries in each `health_test.yaml`. Paths checked: `resource_customizations/**/health.lua`, `resource_customizations/**/health_test.yaml`.
- **UI rendering.** Health and sync icon rendering, the pods view grouped by host, and the network view were only located, not inventoried in detail. Paths checked: `ui/src/app/applications/components/application-details/application-details.tsx`, `ui/src/app/applications/components/application-resource-tree/application-resource-tree.tsx`, `ui/src/app/applications/components/utils.tsx`.
- **Health aggregation error message.** The "see application-controller logs for N other errors" branch in `setApplicationHealth` looks unreachable, because `errCount` only increments while `savedErr` is nil. This was established by reading the code. No test asserts it. Path checked: `controller/health.go#L56-L102`.
- **Version-conversion hub in structured-merge diff.** `apply` passes client-go's `kubernetes/scheme.Scheme` as the object convertor (import at `gitops-engine/pkg/diff/diff.go#L24`, use at `#L477`), with hub `<group>/__internal`. That scheme registers only external versions. gitops-engine's own `scheme.Scheme` (`legacyscheme`, which has internal types, `gitops-engine/pkg/utils/kube/scheme/scheme.go#L29`) is not used here. Code was not run, so it is unknown which of two things happens for a built-in kind with managed fields at another version:
  - conversion succeeds
  - conversion returns a not-registered error, so the manager entry is dropped (DIFF-221)

  The HPA regression tests only assert the `Modified` flag. Paths checked: `gitops-engine/pkg/diff/internal/fieldmanager/borrowed_versionconverter.go#L63-L101`, `gitops-engine/pkg/diff/diff_test.go#L2409-L2530`.
- **Nil GVK parser in typed paths that bypass `ResolveParseableType`.** `GvkParser.Type` dereferences its receiver (`k8s.io/apimachinery` v0.36.1 `pkg/util/managedfields/gvkparser.go#L42-L43`). Two callers do not check for nil first: `removeWebhookMutation` (`gitops-engine/pkg/diff/diff.go#L223`) and the version converter's `typeConverter.ObjectToTyped` (`gitops-engine/pkg/diff/internal/fieldmanager/borrowed_typeconverter.go#L86`). The controller and the ServerSideDiff API normally pass a non-nil parser. The CLI client-side diff passes nil (`cmd/argocd/commands/app_diff.go#L333-L338`). A panic looks possible there, but only if all of these hold, and this was not run:
  - the resource has the `ServerSideApply=true` sync-options annotation
  - live managed fields are recorded at another API version
  - Go-scheme conversion succeeds
- **Deduced type and list-item ownership.** With the deduced type (DIFF-218), lists are atomic, so the typed comparison reports a whole list path as modified. Server-recorded managedFields entries hold item-level paths (`k:{...}`). It was not run whether `fieldpath.Set.Intersection` then removes anything, i.e. whether changes inside list items owned by a trusted manager are ignored when no schema is available. DIFF-021's test bullet "whole list ignored if any item modified by manager" is unverified on this point. Paths checked: `util/argo/managedfields/managed_fields.go#L74-L87`, `sigs.k8s.io/structured-merge-diff/v6@v6.4.0/typed/parser.go#L128-L150`.
- **Frozen GVK-to-definition-name map across clusters.** The map is built once per process from the first parser it sees (`gitops-engine/pkg/utils/kube/scheme/typed.go#L46-L56`). This can change the schema source for built-in GVKs that the first cluster did not serve (DIFF-214). That consequence comes from reading the code only. No upstream test covers more than one cluster, and `ResolveParseableType` has no direct unit test.

## Doc/implementation differences

- **JQ timeout key.** `docs/user-guide/diffing.md#L200-L208` names the JQ timeout key `ignore.normalizer.jq.timeout` in `argocd-cmd-params-cm`. The manifests wire env `ARGOCD_IGNORE_NORMALIZER_JQ_TIMEOUT` from key `controller.ignore.normalizer.jq.timeout` (`manifests/base/application-controller/argocd-application-controller-statefulset.yaml#L325-L329`). The documented key has no effect.
- **Structured-merge diff status.** `docs/user-guide/diff-strategies.md#L18-L23` calls structured-merge diff "Feature Discontinued". The code still selects it automatically whenever an app or resource uses `ServerSideApply=true` and server-side diff is off (`gitops-engine/pkg/diff/diff.go#L113`, `controller/state.go#L909`).
- **Server-side diff maturity.** `docs/operator-manual/feature-maturity.md#L77` defines link `[8]` to Server-Side Diff, but no table row uses it. `diff-strategies.md#L32` says "Stable (Since v3.1.0)". The code treats it as GA, with the default still off.
- **Job health.** `docs/operator-manual/health.md#L23-L24` says Job health is Suspended when `.spec.suspended` is true. The code uses the `Suspended` status condition (`gitops-engine/pkg/health/health_job.go#L45-L50`). A `Suspended=False` condition makes the Job count as complete and report Healthy.
- **Service and Ingress health.** `health.md#L12-L17` requires an LB ingress entry "with at least one value for hostname or IP". The code only checks that the list is non-empty (`health_service.go#L31`, `health_ingress.go#L10`).
- **Workload health.** `health.md#L8-L10` reduces Deployment, ReplicaSet, StatefulSet and DaemonSet health to observedGeneration plus updated replicas. The code also covers these cases:
  - paused Deployment reports Suspended
  - `ProgressDeadlineExceeded` reports Degraded
  - availability and old-replica checks
  - ReplicaFailure reports Degraded
  - OnDelete strategies report Healthy
  - StatefulSet partitions and revisions
- **Lua health statuses.** `health.md#L149-L156` lists four Lua statuses and says health "typically returns a Progressing status" by default. The code accepts all six codes, including `Missing` and `Unknown`. An invalid code becomes `Unknown` / "Lua returned an invalid health status". A nil or empty return gives an empty status, not Progressing (`util/lua/lua.go#L132-L165`).
- **Wildcard override order.** `health.md#L222` says the first matching wildcard in the directory structure wins. For built-ins the code sorts paths lexicographically. For argocd-cm wildcard overrides the pick is non-deterministic (map iteration, `util/lua/lua.go#L486-L495`), and the docs do not mention this.
- **Always-excluded resources.** Both `docs/operator-manual/argocd-cm.yaml#L206` and `declarative-setup.md#L1350` give incomplete lists:
  - `argocd-cm.yaml` mentions only `events.k8s.io` and `metrics.k8s.io`
  - `declarative-setup.md` adds `coordination.k8s.io/Lease`
  - neither mentions core `Event`, which the code also always excludes (`util/settings/resources_filter.go#L7-L11`)
- **Invalid globs in exclusion rules.** `declarative-setup.md#L1407` says "Invalid globs result in the whole rule being ignored". In the code only the invalid pattern fails to match; other patterns in the same rule still match (`util/settings/filtered_resource.go#L11-L36`).
- **Orphan ignore globs.** `docs/user-guide/orphaned-resources.md#L60` documents globbing only for `name`. The code also globs `kind` and `group`. An omitted `group` matches only the core group, not "any" (`controller/appcontroller.go#L554-L563`).
- **`argocd app diff` help text.** The long help says it returns "2 on general errors" and that "Kubernetes Secrets are ignored from this diff" (`cmd/argocd/commands/app_diff.go#L648`). In practice:
  - flag-validation failures exit with the generic code 20; only a wrong argument count exits 2
  - Secrets are excluded only in deprecated local client-side mode; otherwise they are diffed with masked values
- **Aggregated roles in CLI and API diffs.** `docs/user-guide/diffing.md#L135-L148` presents `ignoreAggregatedRoles` as a global compare option. The CLI client-side diff (`cmd/argocd/commands/app_diff.go#L332`) and the ServerSideDiff API (`server/application/application.go#L3043`) always use `false`, so their output can disagree with the controller's sync status.
- **Features that use the static schema (FAQ).** `docs/faq.md#L450-L453` lists three features that rely on the static schema: `managedFieldsManagers`, server-side apply without server-side diff, and server-side diff "_with_ mutation webhooks". Server-side diff never uses the static schema:
  - webhook-mutation removal types objects with the destination cluster's parser only (`gitops-engine/pkg/diff/diff.go#L223`)
  - with `IncludeMutationWebhook=true` there is no typed parsing at all

  Only structured-merge diff (`gitops-engine/pkg/diff/diff.go#L423`) and `managedFieldsManagers` normalization (`util/argo/diff/diff.go#L436`) resolve the static schema. The FAQ's workarounds still avoid the error. See DIFF-220.
- **Symptom of the `field not declared in schema` error.** `docs/faq.md#L426-L427` says the user "may get an error like `field not declared in schema`". That is true only on the structured-merge diff path, where the message appears in a ComparisonError. In `managedFieldsManagers` normalization the conversion error is only logged at debug level and the normalization is skipped (`util/argo/managedfields/managed_fields.go#L35-L40`). The visible symptom there is an unexpectedly OutOfSync resource with no error message (DIFF-217).
