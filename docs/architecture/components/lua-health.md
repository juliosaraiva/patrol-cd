# Component: `internal/health` and `internal/lua`

Status: implemented (phase 1). Decision records: [ADR-0001](../adr/0001-independent-implementation.md)
(reuse of upstream Lua customizations as data).

## Responsibilities

1. `internal/health` — assess the health of one Kubernetes object and aggregate resource
   health into application health, with Argo CD v3.5.5 semantics (gitops-engine
   `pkg/health`, `controller/health.go`):
   - built-in Go checks for Deployment, StatefulSet, DaemonSet, ReplicaSet, Pod, Service,
     PersistentVolumeClaim, Ingress (`extensions` and `networking.k8s.io`), Job,
     HorizontalPodAutoscaler (`autoscaling/v1` annotation conditions and `autoscaling/v2`),
     APIService (`v1`, `v1beta1`) and Argo Workflows `Workflow`;
   - the status ordering used to pick the worst health;
   - the application aggregation rule (hooks excluded, Missing handling, ignore-healthcheck,
     self reference, child Patrol Applications, all-missing rule) and the per-resource health
     to persist;
   - opt-in health of Patrol `Application` objects from `status.health`.
2. `internal/lua` — a sandboxed gopher-lua runtime that runs
   - health scripts configured in `patrol-cm` and the built-in ones copied from upstream
     `resource_customizations/` (238 `health.lua`, embedded), and
   - resource actions: discovery (`discovery.lua`) and execution (`action.lua`, 93 built-in),
     returning objects to patch or create;
   - parsing of the `resource.customizations*` settings keys (both formats).

Neither package imports `api/v1alpha1`. Constants that must match it are duplicated and
documented: `health.ApplicationGroup` (`cd.patrol.example`), `health.HookFinalizer`
(`cd.patrol.example/hook-finalizer`, = `v1alpha1.FinalizerHook`) and
`health.AnnotationIgnoreHealthCheck` (`cd.patrol.example/ignore-healthcheck`,
= `v1alpha1.AnnotationKeyIgnoreHealthCheck`).

## Public API

### `internal/health`

| Symbol | Purpose |
|--------|---------|
| `type Status string`; `StatusHealthy`, `StatusSuspended`, `StatusProgressing`, `StatusMissing`, `StatusDegraded`, `StatusUnknown`; `(Status).Valid()` | Health codes. |
| `type HealthStatus{Status, Message}` | Assessment result (`json:"status,omitempty"`, `json:"message,omitempty"`). |
| `IsWorse(current, candidate Status) bool` | Order Healthy < Suspended < Progressing < Missing < Degraded < Unknown; unrecognized codes rank as Healthy. |
| `type Overrides interface{ GetResourceHealth(obj) (*HealthStatus, error) }` | Custom health that wins over built-ins; `(nil, nil)` = no opinion. `*lua.Runtime` implements it (a nil `*lua.Runtime` is valid). |
| `Assess(obj, overrides) (*HealthStatus, error)` | Pending deletion → overrides → built-in check → `nil` (kind without health). |
| `HasBuiltinCheck(schema.GroupKind) bool` | Whether a Go check exists. |
| `ErrNilObject`, `ErrUnsupportedVersion`, `ErrMalformedObject`, `ErrOverride` | Sentinels (match with `errors.Is`; message texts equal upstream's). |
| `type ResourceHealth{Group, Kind, Live, Hook, IgnoreHealthCheck, SelfReference, Health}` | One managed resource as input to aggregation. |
| `Aggregate([]ResourceHealth) Status`, `AggregateDetailed(...) AggregateResult{Status, Resources}` | Application health; `Resources[i]` is the health to persist for input `i` (nil = none). |
| `IgnoresHealthCheck(live) bool`, `AnnotationIgnoreHealthCheck` | Exact `"true"` check of the ignore annotation. |
| `ApplicationHealth(obj) *HealthStatus`, `WithApplicationHealth(next Overrides) Overrides`, `IsApplication(obj)`, `ApplicationGroup`, `ApplicationKind` | Opt-in Patrol Application health (see below). |
| `HookFinalizer` | Finalizer that keeps a deleting hook object assessable. |

### `internal/lua`

| Symbol | Purpose |
|--------|---------|
| `New(overrides map[string]ResourceOverride, opts...) *Runtime` | Immutable, concurrency-safe runtime. Options: `WithTimeout(d)` (default `DefaultTimeout` = 1 s), `WithLogger(*slog.Logger)`. Rebuild on settings change. |
| `(*Runtime).GetResourceHealth(obj)` / `GetResourceHealthContext(ctx, obj)` | `health.Overrides`; `(nil, nil)` when no script applies. |
| `(*Runtime).HealthScript(obj) (*HealthScript{Script, UseOpenLibs, Source, Key}, error)` | Script selection only (for tooling such as `patrol admin settings resource-overrides health`). `Source` ∈ `configured`, `configured-wildcard`, `builtin`, `builtin-wildcard`. |
| `(*Runtime).ExecuteHealth(ctx, obj, script, useOpenLibs)` | Run an arbitrary health script with the result contract below. |
| `(*Runtime).ListActions(ctx, obj) ([]Action, error)` | Discovery; sorted by name; empty list when the kind has no discovery. `Action{Name, Params []ActionParam{Name}, Disabled, IconClass, DisplayName}`. |
| `(*Runtime).DiscoveryScripts(obj)`, `ExecuteDiscovery(ctx, obj, scripts)` | Lower-level discovery. |
| `(*Runtime).ExecuteAction(ctx, obj, name, []ActionParameter{Name, Value})` | Runs the named action → `[]ImpactedResource{Object, Operation}` with `Operation` ∈ `OperationPatch` (`"patch"`), `OperationCreate` (`"create"`). |
| `(*Runtime).ActionScript(obj, name)`, `ExecuteActionScript(ctx, obj, script, params)` | Lower-level actions. |
| `type ResourceOverride{HealthLua, UseOpenLibs, Actions, IgnoreDifferences, IgnoreResourceUpdates, KnownTypeFields}` | One customization; `Actions`, `IgnoreDifferences`, `IgnoreResourceUpdates` are raw YAML (`GetActions()`, `GetIgnoreDifferences()`, `GetIgnoreResourceUpdates()` parse them into `ResourceActions{ActionDiscoveryLua, Definitions []ActionDefinition{Name, ActionLua}, MergeBuiltinActions}` and `IgnoreDiff{JSONPointers, JQPathExpressions, ManagedFieldsManagers}`). |
| `ParseResourceOverrides(cmData map[string]string)` | Parses `resource.customizations` (legacy YAML block, wildcards allowed) and `resource.customizations.<type>.<group_kind>` split keys (`health`, `useOpenLibs`, `actions`, `ignoreDifferences`, `ignoreResourceUpdates`, `knownTypeFields`; `<group_kind>` = `group_Kind`, `Kind` or `all` → `*/*`). Split keys are layered over the legacy block. |
| `MarshalLegacy(map)`, `OverrideKey(schema.GroupKind)`, `CustomizationsKey` | Helpers. |
| `ApplicationHealthLua` | The documented Patrol Application health customization (Lua). |
| `ScriptError{Message, Timeout}`; `ErrTimeout`, `ErrActionNotFound`, `ErrNoDiscovery`, `ErrInvalidOutput`, `ErrInvalidCustomization`, `ErrNestedTable`, `ErrSparseArray`, `ErrInvalidKeys`, `ErrValueTooDeep` | Errors. `ScriptError.Error()` is the Lua message without stack trace (upstream text); it unwraps to the underlying gopher-lua error (`*lua.ApiError` for runtime errors) and to the context error. |

### Adapting CRD and settings types

- Settings: `lua.New(lua.ParseResourceOverrides(patrolCM.Data))`. The diff package reads
  `IgnoreDifferences`/`IgnoreResourceUpdates`/`KnownTypeFields` from the same map and adds
  the `resource.compareoptions`-derived `/status` pointers itself (not done here).
- Application status: for each compared resource build `health.ResourceHealth`:
  `Live` = live object exists; `Hook` = target is a Skip-only hook, or live object is a hook
  or an ignored hook (sync package); `IgnoreHealthCheck` = `health.IgnoresHealthCheck(live)`;
  `SelfReference` = live object is the Application itself (UID, name, namespace, group, kind);
  `Health` = `health.Assess(live, overrides)` where `overrides` is the `*lua.Runtime`
  (wrapped with `health.WithApplicationHealth` only if application health is enabled).
  Copy `AggregateResult.Resources[i]` into `status.resources[i].health` when resource health
  is persisted inline; report the first `Assess` error as a comparison condition.

## Behavior

### Resource health (`Assess`)

1. `deletionTimestamp` set and no `HookFinalizer` → `Progressing` "Pending deletion".
2. Overrides: error → `Unknown` with the error text (and the error, wrapping `ErrOverride`);
   non-nil result → returned as is (an empty status returned by Lua shadows the Go check).
3. Built-in Go check by group/kind; checks are version-specific exactly as upstream (for
   example `apps/v1beta2` Deployment → `Unknown` "unsupported Deployment GVK: ...").
4. Otherwise no health (`nil`).

Every check reproduces upstream's branch order and message texts (asserted in tests).

### Application aggregation

Start Healthy; skip hooks entirely; a missing resource is recorded as `Missing` but never
worsens the app; ignore-healthcheck and self-referencing resources count as live but are
neither recorded nor aggregated; resources without health are skipped; a live child Patrol
Application that is Missing or Unknown does not affect its parent; the app takes the worst
remaining status; if the app has non-hook resources and none is live, it is `Missing`.

### Patrol Application health (opt-in)

No built-in check exists for Applications (upstream removed it in v1.8 to avoid app-of-apps
deadlocks). Two equivalent ways to enable it, both yielding `Progressing` until
`status.health` is set, then `status.health.{status,message}` (invalid codes → `Unknown`
"Lua returned an invalid health status"):

- settings key `resource.customizations.health.cd.patrol.example_Application` with
  `lua.ApplicationHealthLua` (the upstream-documented script adapted to Patrol's group);
- in Go, `health.WithApplicationHealth(runtime)` (configured scripts still win).

### Lua script lookup

Health (`HealthScript`): (1) configured key equal to `group/Kind` (`Kind` for core) with a
non-empty script; (2) configured keys as gobwas globs (no separators, `*` crosses `/`),
first match **in sorted key order** with a non-empty script; (3) built-in
`<group>/<Kind>/health.lua`; (4) built-in wildcard directories (`_` = `*`, doublestar
matching, `*` does not cross `/`) in sorted order: `_.cnrm.cloud.google.com/_`,
`_.crossplane.io/_`, `_.upbound.io/_`, `grafana-org-operator.kubitus-project.gitlab.io/_`,
`operator.victoriametrics.com/_`.

Actions: discovery = configured `discovery.lua` for the exact key (no wildcards), followed by
the built-in one unless the configuration has actions and `mergeBuiltinActions` is false;
first script wins per action name. Execution = configured definition with that name, else the
built-in `actions/<name>/action.lua`.

Built-in aliases: `cd.patrol.example/Application` and `cd.patrol.example/ApplicationSet` use
the built-ins of `argoproj.io/Application` (action `toggle-auto-sync`; no health) and
`argoproj.io/ApplicationSet` (health from `ErrorOccurred`/`ResourcesUpToDate`/
`RolloutProgressing` conditions) when they have none of their own; Patrol's CRDs share those
field layouts.

### Script contract

- Globals: `obj` (the object; numbers are Lua numbers; map keys are inserted in sorted order so
  `pairs()` iterates deterministically) and `actionParams` (string values; empty for health
  and discovery).
- Health result: table → decoded as `{status, message}` with case-insensitive keys; a table
  that does not decode (empty table, wrong field types) → empty status; status outside the six
  codes → `Unknown` "Lua returned an invalid health status"; `nil`/no value → empty status;
  other types → error "expect table output from Lua script, not <type>". The last returned
  value is used.
- Action result: object → patch of the source object; array of
  `{operation = "create"|"patch", resource = object}`; "unsupported operation: ..." otherwise.
  Empty tables that were objects in the source object are restored as objects in patch
  results (Lua cannot distinguish `{}` from `[]`).
- Lua → JSON follows the gopher-json encoder upstream uses: empty table → `[]`; tables are
  arrays (keys exactly 1..n) or objects (string keys only); functions/userdata fail.

## Sandbox and limits

| Profile | Used for | Libraries |
|---------|----------|-----------|
| safe | all built-in scripts, all discovery and action scripts, configured health scripts without `useOpenLibs` | base without `dofile`, `loadfile`, `load`, `loadstring`, `require`, `module`, `collectgarbage`, `print` (no-op); `table`; `string` (`rep` capped at 4 MiB, `format` width/precision ≤ 2 digits as in Lua 5.1, no `dump`); `math`; `os` with only `time`, `date`, `clock`, `difftime` (also via `require("os")`; `require` resolves only `os`, `string`, `table`, `math` and never reads files) |
| open | configured health scripts with `useOpenLibs: true` | the complete gopher-lua standard library (`package`, `io`, full `os`, `debug`, `coroutine`, `channel`, …), as upstream; `print` is a no-op |

Every evaluation uses a fresh Lua state (no state leaks between objects or tenants) with:
an execution deadline (`context.WithTimeout`, checked before every VM instruction, default
1 s, caller cancellation honored); call stack limit 256 frames; value stack 1 024 slots
growing to at most 65 536 ("registry overflow" beyond); Go↔Lua conversion depth limit 1 000.
Compiled scripts are cached by SHA-256 of their source (LRU, 512 entries).

## Invariants

- A `Runtime` is immutable after `New`; all methods are safe for concurrent use.
- Built-in scripts never get the open profile.
- Script errors never panic the caller; Go panics inside the VM are recovered as Lua errors.
- Error messages returned to users carry no Lua stack traces.
- Action patch results always target the source object (same group, kind, namespace, name).
- `Aggregate` is independent of resource order (property-tested), and hooks never change it.

## Failure modes

| Failure | Result |
|---------|--------|
| Script syntax/runtime error | `*ScriptError` with the Lua message → `Assess` returns `Unknown` + error |
| Infinite loop / slow script | `*ScriptError` matching `ErrTimeout` and `context.DeadlineExceeded` after the deadline |
| Caller context canceled | `*ScriptError` matching `context.Canceled` |
| Deep recursion / huge `unpack` | "lua callstack overflow" / "registry overflow" error (fast, no crash) |
| Wrong return type / malformed action output | error matching `ErrInvalidOutput` (upstream message text) |
| Cyclic or sparse result table | `ErrNestedTable` / `ErrSparseArray` / `ErrInvalidKeys` |
| Invalid `resource.customizations*` setting | `ParseResourceOverrides` error wrapping `ErrInvalidCustomization` |
| Invalid glob key | logged once at `New`, key only matches nothing (upstream) |
| Unknown action / traversal in action name or kind | `ErrActionNotFound` |
| Typed decode failure in a Go check | `Unknown` with "failed to convert unstructured <Kind> to typed: ..." (`ErrMalformedObject`) |

## Security notes

- Objects are untrusted input; scripts are trusted configuration (patrol-cm is
  admin-controlled) or embedded data. Even so, the safe profile removes every file, process,
  environment and code-loading primitive; `useOpenLibs` is an explicit admin opt-in for health
  scripts only, mirroring upstream, and grants file and process access inside the controller —
  document it as privileged in operator docs.
- Action and kind names are validated as single path elements before built-in lookup, so a
  user permitted to run `action/apps/Deployment/*` cannot reach another kind's scripts via
  `../`.
- `print` output is discarded so scripts cannot write unbounded, unstructured data to
  controller logs. Nothing in this package logs object content or settings values.
- Memory: there is no allocation hook in gopher-lua. Bounds are the deadline, the stack
  limits, the `string.rep`/`string.format` caps and the conversion depth limit. A malicious
  admin script can still allocate heavily within its deadline (for example by repeated string
  concatenation) — see known gaps.

## Parity feature areas served

`docs/parity/areas/04-diff-health-resources.md`: DIFF-043 … DIFF-063 (status codes, evaluation
order, every built-in Go check, Lua health via settings and wildcards, sandbox/useOpenLibs,
result contract, built-in Lua lookup, aggregation, ignore-healthcheck), DIFF-024 and the
settings parsing part of DIFF-022/023 (customization keys), and the persisted-health input of
DIFF-064. `docs/parity/areas/05-synchronization.md`: SYNC-100 … SYNC-111 runtime parts
(discovery, execution, parameters, create actions, custom action configuration, action sandbox,
all built-in actions). `docs/parity/areas/01-application.md`: APP-028, APP-029, APP-092
(Application health customization, toggle-auto-sync for Patrol Applications), APP-093.

## Verification

- Every case of every upstream `health_test.yaml` (238 files, 1 127 cases) and
  `action_test.yaml` (35 files, 81 discovery + 104 action cases) runs against this runtime
  (`TestBuiltinHealthScripts`, `TestBuiltinActionScripts`), plus the 4 scenarios of
  `argoproj.io/Application/actions/actions_test.yaml` that upstream's runner skips. Discovery
  additionally asserts the exact action count (upstream asserts containment only).
- All 47 upstream gitops-engine health fixtures are asserted (`internal/health/testdata`).
- Property tests (rapid): `IsWorse` is a strict weak order; aggregation is order independent,
  hook-invariant and never better than a contributing resource; Go↔Lua conversion round-trip;
  split-key and legacy settings round-trip; format checker never panics.

## Known gaps and deliberate differences

- **Safe profile is a superset of upstream's default**: upstream exposes only base, table,
  package and `require("os")` (time/date) to configured health scripts without `useOpenLibs`
  and to all actions; Patrol also exposes `string`, `math` and a global time-only `os`.
  Scripts valid upstream behave the same; scripts that fail upstream only because `string` is
  missing (DIFF-059's negative case) succeed in Patrol.
- **Built-in scripts run with the safe profile**; upstream gives them all libraries. All
  built-in tests pass with the safe profile.
- **Upstream file/loader functions are removed** (`dofile`, `loadfile`, `load`, `loadstring`,
  file-based `require`) in the safe profile; upstream leaves them available.
- **Deterministic choices** where upstream iterates Go maps: wildcard override selection
  (sorted keys), `pairs()` order over object maps, and discovery result order (sorted).
- **Stricter action output validation**: entries without `resource` or `operation`, and patch
  results targeting another object, are errors (upstream accepts them silently). Shared
  (non-cyclic) subtables in results are accepted (upstream's encoder rejects them).
- **Timeout error text**: "lua script exceeded execution timeout of 1s" instead of the raw
  "<string>:N: context deadline exceeded".
- **Uninterruptible Go builtins**: the deadline is checked between VM instructions; a single
  long-running Go library call (pathological Lua pattern on a large string in an admin
  script) runs to completion before the deadline takes effect.
- **No per-state memory limit** (gopher-lua offers none besides a process-exiting `SetMx`).
- **Embedded data text**: verbatim scripts still write "By Argo CD at: ..." (Flux reconcile
  actions) and "Job was terminated explicitly through Argo CD" (Job terminate).
- **Not here**: merge-patch computation, status-subresource split, project permission checks,
  dry-run creation and RBAC for actions (API server); hook classification (sync package);
  persisted vs tree health switching, health transition timestamps and events (controller);
  `resource.compareoptions` status pointers (diff/settings).
- `NOTICE` lists `internal/lua/customizations/**`; the copied gitops-engine fixtures in
  `internal/health/testdata` (attributed in its README) should be added to `NOTICE` by its
  owner.
