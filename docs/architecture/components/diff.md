# Component: `internal/diff`

Status: implemented (phase 1, library only). Owner task: T1.12. Parity area:
`docs/parity/areas/04-diff-health-resources.md` (DIFF-001 … DIFF-037).

Patrol's desired-vs-live comparison engine. It re-implements the observable behavior of the
Argo CD v3.5.5 diff stack (gitops-engine `pkg/diff`, `util/argo/diff`, `util/argo/normalizers`,
`util/argo/managedfields`) without depending on gitops-engine, Argo CD or `k8s.io/kubernetes`.
It uses client-go types and scheme, `k8s.io/apimachinery` strategic/JSON merge patches,
`sigs.k8s.io/structured-merge-diff/v6`, `github.com/itchyny/gojq`, `github.com/gobwas/glob`
and `github.com/evanphx/json-patch/v5` (JSON merge patch only). It does not import
`api/v1alpha1`.

## Responsibilities

- Decide whether a live object matches its desired manifest and produce the two documents
  shown in diff views (`NormalizedLive`, `PredictedLive`).
- Three strategies with the baseline's selection rules: legacy three-way diff
  (last-applied-configuration) with two-way fallback, structured-merge diff (local SSA
  simulation), server-side diff (SSA dry run through a caller-supplied `DryRunner`, with
  mutating-webhook removal unless `IncludeMutationWebhook=true`).
- Built-in normalizations: typed re-encoding of built-in kinds (quantities, int vs float,
  omitempty, generated `creationTimestamp: null`), API-server defaults, Secret `stringData`
  folding, Role/ClusterRole empty rules, aggregated ClusterRole rules (`ignoreAggregatedRoles`),
  Endpoints ordering, StatefulSet `volumeClaimTemplates`, namespace/empty-annotation stripping.
- User/system normalizations: ignore rules (JSON pointers, jq path expressions,
  `managedFieldsManagers`, glob group/kind matching), `knownTypeFields`,
  `ignoreResourceStatusField`, `ignoreResourceUpdates` hashing.
- Secret masking for diff output (`++++++++` masks preserving equality per key).
- Compare-options / sync-options annotation parsing helpers.

Out of scope: pairing targets with live objects and sync-status computation (controller),
tracking-metadata normalization (`internal/tracking`, run by the caller before `Diff`),
settings parsing (`internal/settings`), the diff cache (controller), building the
`GvkParser` from cluster discovery and implementing `DryRunner` (controller / `internal/cache`).

## Public API

| Symbol | Purpose |
|--------|---------|
| `Diff(ctx, config, live, Options) (*Result, error)` | Compare one pair. Either side may be nil (create/delete); both nil → `ErrNilObjects`. Inputs are never mutated. |
| `DiffArray(ctx, configs, lives, Options) (*ResultList, error)` | Index-aligned pairs; `ErrLengthMismatch` on different lengths. |
| `Normalize(ctx, un, Options)` | Built-in normalizations plus `Options.Normalizer`, in place. Normalizer errors are logged, not returned (baseline). |
| `TwoWayDiff`, `ThreeWayDiff`, `StructuredMergeDiff`, `LastAppliedConfig`, `NormalizeSecret`, `NormalizeManagedFields` | Building blocks used by `Diff`, exported for tests/tools. |
| `Options` | `Normalizer`, `IgnoreAggregatedRoles`, `ServerSide`, `DryRunner`, `Manager` (default `patrol-controller`), `GVKParser`, `StructuredMerge`, `IgnoreMutationWebhook`, `SyncOptionsAnnotation`, `Log *slog.Logger`. `DefaultOptions()` returns the baseline defaults (manager, `IgnoreMutationWebhook: true`). |
| `Result{Modified, NormalizedLive, PredictedLive}` / `ResultList{Diffs, Modified}` | JSON documents; `"null"` for an absent side. |
| `DryRunner` | `ServerSideApplyDryRun(ctx, obj, manager) (*unstructured.Unstructured, error)`; must use dryRun=All and force-conflicts=true like the sync (baseline sets `ForceConflicts` for SSA). |
| `Normalizer`, `NormalizerFunc`, `Chain(...)`, `TrustedManagersProvider` | Normalizer contract (concurrency-safe, context-aware). A normalizer that also implements `TrustedManagers(gk, name, ns)` drives the managedFieldsManagers pre-step. |
| `IgnoreRule{Group, Kind, Name, Namespace, JSONPointers, JQPathExpressions, ManagedFieldsManagers}` | Neutral ignore rule (app `spec.ignoreDifferences` entry or system override). |
| `NewIgnoreNormalizer(rules, IgnoreNormalizerOptions{JQTimeout, Log})` | Compiles rules. Invalid jq → `ErrInvalidIgnoreRule` (fatal, like the baseline's normalizer construction). |
| `KnownTypeField{Field, Type}`, `NewKnownTypesNormalizer(map[GroupKind][]KnownTypeField)`, `SupportedKnownTypes()` | knownTypeFields. Unsupported types are skipped and reported with `ErrUnsupportedKnownType` (the normalizer is still returned). |
| `NewNormalizer(NormalizerConfig{Rules, KnownTypeFields, Ignore})` | Standard chain: ignore rules then known types. |
| `HideSecretData(target, live, hideAnnotations)`, `MaskSecretResult(res, hideAnnotations)` | Secret masking of objects / of a diff result pair (needed for server-side diff results). |
| `AnnotationOptions`, `HasAnnotationOption`, `ParseCompareAnnotation` → `CompareAnnotation{IgnoreExtraneous, ServerSideDiff *bool, IncludeMutationWebhook}`, `ServerSideDiffEnabled(default)` | compare-options / sync-options parsing (trim, drop empty, de-dup, exact match; `ServerSideDiff=false` wins). |
| `IgnoreStatusMode`, `ParseIgnoreStatusMode`, `StatusIgnoreRules`, `CompareOptions`, `DefaultCompareOptions`, `SystemIgnoreRules` | `resource.compareoptions` semantics. |
| `ResourceUpdateIgnoreRules(updateRules, diffRules, CompareOptions)`, `ResourceUpdateHash(ctx, un, n)` | ignoreResourceUpdates: effective rules (always + `/metadata/resourceVersion`, `/metadata/generation`, `/metadata/managedFields`) and the content hash used to drop irrelevant watch events. |
| Constants | `AnnotationLastAppliedConfig`, `DefaultManager`, `DefaultSyncOptionsAnnotation`, `DefaultCompareOptionsAnnotation`, `DefaultJQTimeout`. |
| Errors | `ErrNilObjects`, `ErrLengthMismatch`, `ErrNoDryRunner`, `ErrNoGVKParser`, `ErrNoSchema`, `ErrNoManagedFields`, `ErrManagerNotFound`, `ErrDryRun`, `ErrInvalidIgnoreRule`, `ErrUnsupportedKnownType`. |

### Adapting CRD and settings types (callers)

- **Application `spec.ignoreDifferences[]`** → one `IgnoreRule` per entry, fields copied 1:1
  (`group` omitted means the core group `""`).
- **System overrides** (`resource.customizations.ignoreDifferences.<group_kind>` and the legacy
  block): one `IgnoreRule` per key with `Group`/`Kind` from the key (`all` / `*/*` → `"*"`,
  `"*"`; a bare `Kind` key → group `""`). Then append `SystemIgnoreRules(overrides, opts)` to
  add the `/status` rule from `ignoreResourceStatusField`.
- **Order**: app rules first, then system rules (the baseline applies app entries, then
  overrides). Build once per reconcile with `NewNormalizer` and pass it in `Options.Normalizer`.
- **knownTypeFields**: `map[schema.GroupKind][]KnownTypeField` from
  `resource.customizations.knownTypeFields.<group_kind>`; log `ErrUnsupportedKnownType` as a
  warning.
- **Strategy selection**: `Options.ServerSide = ParseCompareAnnotation(app.Annotations,
  <compare-options key>).ServerSideDiffEnabled(controllerFlag)`;
  `Options.IgnoreMutationWebhook = !annotation.IncludeMutationWebhook`;
  `Options.StructuredMerge` = app sync option `ServerSideApply=true`;
  `Options.SyncOptionsAnnotation` = `api/v1alpha1` sync-options key (the default constant
  mirrors it). `Options.GVKParser` = parser built from the destination cluster's OpenAPI
  (`managedfields.NewGVKParser`). `Options.Manager` = the sync engine's SSA field manager.
- **Before `Diff`**: run tracking normalization (`internal/tracking`) on copies, as the
  baseline's `preDiffNormalize` does. Managed-fields normalization is done inside `Diff`.
- **Secrets**: for Secret results call `HideSecretData` on target/live before recomputing
  (legacy diff) or `MaskSecretResult` on a server-side diff result, with
  `resource.sensitive.mask.annotations` as `hideAnnotations`.
- **IgnoreExtraneous**: `ParseCompareAnnotation(live.Annotations, key).IgnoreExtraneous`.

## Behavior and invariants

1. Inputs are never mutated; `Diff` works on re-encoded copies.
2. `Modified == !bytes.Equal(NormalizedLive, PredictedLive)` on canonical JSON (sorted keys).
3. Pipeline (`Diff`): managedFieldsManagers pre-normalization → typed re-encoding of both sides
   → built-in normalization (+ user normalizer, deferred for server-side diff) → strategy.
4. Create (live nil): `Modified=true`, `NormalizedLive="null"`. Extraneous (config nil):
   `Modified=false`, `PredictedLive="null"`. Server-side diff never calls the API server for
   these.
5. Legacy: last-applied annotation (fully normalized) is the original state; namespace and
   empty annotations stripped from original and config; built-in kinds use a strategic merge
   patch and then API defaults on both sides; other kinds trim live to the original's shape and
   use a JSON merge patch. Unparsable annotation or failed three-way → two-way diff.
6. Structured merge: config applied onto live with the manager's identity and force; fields
   previously applied by the manager and dropped from config are removed unless co-owned;
   no-op apply returns live for both sides. Built-in kinds use the client-go static schema
   when the parser knows the kind (baseline behavior); unknown kinds with a parser →
   `ErrNoSchema`; without a parser the deduced schema is used (atomic lists).
7. Server-side diff: the dry run receives the complete config (ignore rules applied only
   afterwards); with `IgnoreMutationWebhook`, fields not owned by the manager are reverted to
   live values except ancestors of owned leaves (preserve-unknown-fields) and composite list
   keys, while manager-owned removals stay removed; `managedFields`, `resourceVersion` and the
   last-applied annotation are stripped from both sides.
8. `Normalize` is idempotent (property-tested) and every normalizer is safe for concurrent use.
9. Secret masks: per key, equal values ↔ equal masks across target, live and both last-applied
   annotations; masks are `+` runs of length 8+4k; a malformed or explicitly hidden
   last-applied annotation becomes `++++++++` (property-tested: no plaintext or base64 value
   survives).

## Failure modes

| Situation | Result |
|-----------|--------|
| Both objects nil / slices of different lengths | `ErrNilObjects` / `ErrLengthMismatch` |
| Context cancelled | error wrapping `ctx.Err()` |
| Invalid jq expression | `NewIgnoreNormalizer` error (`ErrInvalidIgnoreRule`); callers raise a ComparisonError |
| jq runtime error, timeout, no/multiple/non-object result; pointer to bad index | patch skipped, debug log with group/kind/namespace/name |
| JSON pointer to a missing path | silently skipped |
| Known-type value not decodable (e.g. invalid quantity) | normalizer error, logged at error level, diff continues |
| Server-side diff without `DryRunner` / dry run fails | `ErrNoDryRunner` / `ErrDryRun` wrapping the API error |
| Dry-run result without managedFields / without the manager / no parser / unknown GVK | `ErrNoManagedFields` / `ErrManagerNotFound` / `ErrNoGVKParser` / `ErrNoSchema` |
| Invalid live managedFields (structured merge, trusted managers) | error |
| Objects that do not match their schema (trusted managers) | compared unchanged |

## Security notes

- Secret values are never logged: conversion and jq error texts for Secrets are replaced by the
  error type (`sanitizeError`), and `LastAppliedConfig` does not wrap JSON parse errors (they can
  quote the annotation). Other error texts are truncated to 512 bytes.
- jq expressions are tenant input. They run with a per-expression timeout (default 1s,
  `IgnoreNormalizerOptions.JQTimeout`), cannot read the process environment (`$ENV`/`env` are
  empty) and panics inside the evaluator are contained.
- Server-side diff results contain raw Secret data from the API server: callers must apply
  `MaskSecretResult` before caching or serving them. The last-applied annotation (which can
  embed Secret values) is stripped from both sides of server-side diff results.
- The dry run is performed with the controller's destination-cluster credentials and never
  persists (`dryRun=All`); a `DryRunner` must not fall back to a real apply.

## Parity feature areas served

DIFF-001 (pipeline/result model), DIFF-002 (three-way), DIFF-003 (two-way), DIFF-004
(create/delete), DIFF-005 (remarshal, defaults, creationTimestamp), DIFF-006 (StatefulSet
templates), DIFF-007 (namespace/annotation stripping), DIFF-008 (structured merge incl.
cross-version managers), DIFF-009/DIFF-010 (server-side diff, mutation webhook removal),
DIFF-012/013/014/015 (Secret, Role, aggregated roles, Endpoints), DIFF-016 (knownTypeFields),
DIFF-017..DIFF-023 (ignore rule matching, pointers, jq, timeout, managedFieldsManagers, system
rules — given caller adaptation), DIFF-025 (ignoreResourceStatusField), DIFF-027/028 parsing
helpers, DIFF-031/032 (masking), DIFF-033/035/037 building blocks (rules + hash). Pairing,
caching (DIFF-011), sync status (DIFF-030), APIs, CLI and UI belong to other components.

## Verification

- Unit tests (table-driven) for every strategy, normalization and helper; fixtures are written
  for Patrol and modeled on the upstream test scenarios.
- Property tests (`pgregory.net/rapid`): JSON-pointer removal is differential-tested against
  `evanphx/json-patch` (v4 API, the semantics the baseline uses); `Normalize` idempotency;
  `Diff(x, x)` never modified and never mutates input; Endpoints order insensitivity; Secret
  masking never reveals values and preserves equality per key.
- `testdata/openapi-v2.json`: OpenAPI v2 subset fetched from the envtest kube-apiserver 1.36
  (`go run ./internal/diff/internal/gen/openapifixture`), used to build the `GvkParser`.
- `TestEnvtestStrategiesAgree` (runs when `KUBEBUILDER_ASSETS` is set): applies Deployment,
  Service, StatefulSet and MutatingWebhookConfiguration to a real kube-apiserver with SSA and
  client-side apply, then checks legacy, structured-merge and server-side diffs (real dry runs)
  report no drift for unchanged objects and drift for changed ones.
- `knowntypes_corev1.go` and `testdata/known_types.txt` are generated
  (`go generate ./internal/diff`); the list equals the baseline's `diffing_known_types.txt`.

## Known gaps and deviations

- **API defaults**: the baseline links the Kubernetes defaulting functions
  (`k8s.io/kubernetes`), which Patrol does not depend on. `defaults.go` re-implements the
  defaults for Pod/PodTemplate/ReplicationController/Service/Secret/Endpoints/PVC/Namespace,
  apps/v1 Deployment/StatefulSet/DaemonSet/ReplicaSet, batch/v1 Job/CronJob, networking/v1
  NetworkPolicy, autoscaling v1/v2 HPA, admissionregistration v1/v1beta1 webhooks,
  storage/v1 StorageClass and scheduling/v1 PriorityClass (including nested PodSpec,
  containers, probes, volumes, resource rounding). Other kinds/versions (e.g. apps/v1beta*,
  PersistentVolume, LimitRange, legacy volume plugins' defaults) get no defaults, which can show
  as drift only when a strategic patch replaces a structure that contained server defaults.
- **Version conversion** (structured merge): managers recorded at another API version are
  re-typed with that version's schema instead of converted through internal types; if the
  schema rejects the object the manager is ignored (as the API server does for removed
  versions).
- **Structured-merge StatefulSet templates**: like the baseline, `volumeClaimTemplates` is an
  atomic list and the server adds `apiVersion`/`kind` to templates, so this strategy reports
  drift for StatefulSets with templates (verified against a real API server); server-side diff
  does not. The baseline marks the strategy as discontinued.
- **Endpoints order**: subsets and ports are sorted by content digest rather than the upstream
  spew/MD5 order; equality semantics are identical, the displayed order may differ.
- **jq memory**: evaluation is time-bounded but not memory-bounded (gojq has no allocation
  limit). A hostile expression can allocate large values within the timeout. Mitigation
  candidates: evaluate in a sandboxed child process (ADR-0007 style) or an AST allow-list.
- **System managedFieldsManagers for core kinds**: the baseline looks system overrides up by
  the literal key `group/kind`, so overrides for core kinds never contribute trusted managers
  (parity quirk). Patrol applies whatever rules the caller passes; to reproduce the quirk the
  caller must drop `ManagedFieldsManagers` from system rules for core-group kinds.
- **jq number handling**: integers are passed to jq as Go `int` (the baseline goes through
  `float64`), so integers above 2^53 keep their exact value.
