# Component: `internal/tracking`

Status: implemented (library only; not yet wired into the controller, repo-server or
hydrator). Parity areas: `docs/parity/areas/01-application.md` (APP-069..APP-077, APP-038),
`04-diff-health-resources.md` (DIFF-026, DIFF-076, DIFF-087), `05-synchronization.md`
(SYNC-080, SYNC-082).

Resource tracking marks every rendered resource with the Application that manages it and
reads that mark back from live objects. It re-implements Argo CD v3.5.5
`util/argo/resource_tracking.go` and the self-reference and shared-resource checks of
`controller/state.go` using the Patrol keys from `api/v1alpha1/keys.go`. It is a pure library:
no I/O, no logging, no global state. It imports `api/v1alpha1` (keys, `TrackingMethod`,
`Application`) and `k8s.io/apimachinery`.

## Responsibilities

- Write tracking metadata onto desired objects for the three methods (`annotation`,
  `label`, `annotation+label`), including the installation-id annotation.
- Read the managing Application's instance name from live objects, honoring the
  installation ID.
- Build and parse tracking ids (`<instance>:<group>/<kind>:<namespace>/<name>`).
- Compute instance names (apps in any namespace) and their display form.
- Decide self-reference: a tracking id copied onto another object does not make that object
  part of the Application.
- Managed-root test (`IsManagedBy`: tracked by the app, no ownerReferences, self-referenced),
  the tracking-side precondition for pruning, and shared-resource detection with the upstream
  SharedResourceWarning wording.
- Tracking-method migration normalization for diffing (label → annotation).
- Remove tracking metadata (hydrated manifests).

Out of scope: reading settings (`patrol-cm`; owned by `internal/settings`), target namespace
normalization and duplicate detection (controller), live-set assembly (cache iteration,
owners synthesized by the cache such as Endpoints -> Service, adoption of untracked objects
matching a target; `internal/cache` + controller), AppProject permission checks, emitting
conditions, failing syncs with `FailOnSharedResource`, and UI presentation.

## Public API

| Symbol | Purpose |
|--------|---------|
| `Method` (= `v1alpha1.TrackingMethod`), `MethodAnnotation`, `MethodLabel`, `MethodAnnotationAndLabel` | Tracking methods. |
| `EffectiveMethod(m)`, `UsesAnnotation(m)`, `ValidateMethod(m)` | `""` and unknown values behave as `annotation`; `ValidateMethod` is for settings loaders. |
| `Config{InstanceLabelKey, ControlPlaneNamespace}`, `Config.Validate()` | Installation-wide settings. Empty label key → `app.kubernetes.io/instance`. `Validate` checks the label key (qualified name) and the namespace (DNS label). |
| `New(Config) *ResourceTracker` | Immutable, concurrency-safe tracker. Build a new one when settings change. |
| `Tracker` interface | `SetAppInstance(obj, instanceName, defaultNamespace, method, installationID) error`, `RemoveAppInstance(obj, method) error`, `GetAppName(obj, method, installationID) string`, `GetAppInstance(obj, method, installationID) *AppInstanceValue`, `IsSelfReferencedObject(live, target, method, installationID) bool`, `Normalize(target, live, method) error`. Implemented by `*ResourceTracker`. |
| `(*ResourceTracker).InstanceName(App)`, `IsManagedBy(obj, App, method, installationID)`, `DetectSharedResource(live, App, method, installationID) (otherInstance, shared)` | Application-level helpers. |
| `(*ResourceTracker).DiffTrackingKeys(method) (labelKey, annotationKey)` | Keys for `diff.Options.Tracking`; empty under label tracking. |
| `(*ResourceTracker).TrackingLabelKeys()`, `TrackingAnnotationKeys()` | Keys to retain in compact live-state caches (`cache.Settings.TrackingLabelKeys/TrackingAnnotationKeys`). |
| `InstanceLabelKey()`, `ControlPlaneNamespace()` | Effective configuration. |
| `AppInstanceValue{ApplicationName, Namespace, Group, Kind, Name}` | Tracking id content. `String()`, `References(obj)` (self-reference comparison). |
| `BuildTrackingID(v)`, `ParseTrackingID(s)`, `NewAppInstanceValue(obj, instanceName, defaultNamespace)` | Tracking id codec. |
| `App{Name, Namespace}`, `AppOf(*v1alpha1.Application)`, `App.QualifiedName()` | Application identity. |
| `InstanceName(name, namespace, controlPlaneNamespace)`, `ParseInstanceName(instance, defaultNamespace)`, `InstanceNameFromQualified(qualified, defaultNamespace)`, `DisplayName(instance)` | Instance names (`ns_name`) and their `ns/name` display form. |
| `SharedResourceMessage(live, app, otherInstance)` | `"<Kind>/<name> is part of applications <ns>/<app> and <other ns/name>"`. |
| `TruncateLabel(val)`, `LabelMaxLength` | Label truncation of `annotation+label`. |
| `IsCRD(obj)` | CRDs never receive tracking metadata from the repo-server and are skipped by `Normalize`. |
| Errors | `ErrWrongResourceTrackingFormat` (upstream message), `ErrLabelTruncation` (upstream message), `ErrNilObject`, `ErrInvalidInstanceName`. |

### Adapting settings and CRD types (callers)

- `patrol-cm` `application.resourceTrackingMethod` → `method` (validate with
  `ValidateMethod`; pass the value or `""`, both mean annotation). `application.instanceLabelKey`
  → `Config.InstanceLabelKey`. `installationID` → `installationID` argument. The control-plane
  namespace → `Config.ControlPlaneNamespace`.
- **Instance name, not app name**: `SetAppInstance` takes the instance name
  (`t.InstanceName(tracking.AppOf(app))`) and the Application's destination namespace, like
  upstream (`app.InstanceName(ns)`, `app.Spec.Destination.Namespace`). The repo-server receives
  the instance name in the render request and never needs the control-plane namespace.
- **Controller, target normalization**: set namespaces first (cluster-scoped cleared,
  namespaced defaulted to the destination namespace), then call `SetAppInstance`; on error
  raise `ComparisonError` `Failed to normalize target state: <err>` (DIFF-029 wording).
- **Repo-server**: call `SetAppInstance` for every rendered object except CRDs (`IsCRD`) when
  the request carries an app name; wrap errors as
  `failed to set app instance tracking info on manifest: <err>` (SRC-121).
- **Live-state cache** (`cache.Settings.PopulateResourceInfo`): `appName =
  t.GetAppName(un, method, installationID)` for root objects; retain
  `TrackingLabelKeys()`/`TrackingAnnotationKeys()`.
- **Comparison**: for each paired object call
  `IsSelfReferencedObject(live, target, ...)`; non-self-referenced live objects get no sync
  status, never `requiresPruning`, and are excluded from sync tasks (APP-075, SYNC-080). For
  each live object call `DetectSharedResource` and add `SharedResourceWarning` with
  `SharedResourceMessage` (APP-038). `FailOnSharedResource` fails the operation with
  `Shared resource found: <first SharedResourceWarning message>` (APP-077, sync engine).
- **Diff**: either set `diff.Options.Tracking = diff.TrackingOptions{LabelKey, AnnotationKey}`
  from `DiffTrackingKeys(method)`, or call `Normalize(target, liveCopy, method)` before
  `diff.Diff`. The two are equivalent (differential property test); do not rely on running
  both, although `Normalize` is idempotent.
- **Prune/delete**: a live object without a desired counterpart may be pruned only when
  `IsManagedBy(obj, app, method, installationID)` holds (it already excludes objects with
  ownerReferences on the object), the cache has synthesized no owner for it, and the
  AppProject permits the resource and its destination (threat model T11).
- **Hydrator**: `RemoveAppInstance(obj, method)` on dry manifests before writing them.

## Behavior and invariants

1. **Tracking id format**: `<instance>:<group>/<kind>:<namespace>/<name>`; namespace is the
   object namespace, or `defaultNamespace` for objects without one (cluster-scoped resources
   carry the destination namespace). The installation ID is a separate annotation
   (`cd.patrol.example/installation-id`), not a suffix of the id, as upstream.
2. **Codec**: parsing splits on the first two `:` only (names such as
   `system:aggregate-to-admin` survive) and requires exactly one `/` in `<group>/<kind>` and in
   `<namespace>/<name>`. For every accepted string `BuildTrackingID(ParseTrackingID(s)) == s`;
   `ParseTrackingID(BuildTrackingID(v)) == v` for every valid identity (property tests).
3. **Methods**:
   - `annotation` (default, also `""` and unknown values): writes tracking-id; writes
     installation-id when configured, removes it otherwise; ignores the instance label when
     reading.
   - `label`: writes the instance label with the untruncated instance name; reads only the
     label; the installation ID is neither written nor checked; every object is
     self-referenced.
   - `annotation+label`: as `annotation`, plus the label truncated by `TruncateLabel` (63
     bytes, trailing `[^a-zA-Z0-9]` stripped; error when nothing remains). Ownership is read
     from the annotation only.
4. **Installation ID**: under annotation methods an object is attributed only when its
   installation-id annotation equals the configured value exactly (both empty matches).
   Setting an ID makes previously synced resources unowned until re-synced; a resource with
   an ID is invisible to an installation without one.
5. **Self-reference**: with a desired object, live is self-referenced iff the identity built
   from the desired object references it (handles API group migrations); without one, iff the
   live tracking id references live; missing/unreadable ids count as self-referenced (as
   upstream). Cluster-scoped objects ignore the namespace in the id. The app name is not
   compared.
6. **`IsManagedBy`** = tracked by the app's instance name (installation ID honored) **and**
   no ownerReferences **and** self-referenced by its own tracking id: upstream's managed live
   set (`gitops-engine/pkg/cache/cluster.go` `GetManagedLiveObjs`: tracked, no ownerRefs)
   combined with the `requiresPruning` self-reference test (`controller/state.go`).
   Untracked, foreign, other-installation, copied and owned objects are never managed; under
   label tracking this is what keeps ReplicaSets and Pods that inherit the pod-template
   instance label out of pruning. An `ownerReferences` value that is not a list (or
   unreadable metadata) counts as owned (fail closed; `GetOwnerReferences` would return nil).
7. **Shared resources**: any non-empty tracked name different from the app's instance name
   (with label tracking: any foreign label value, e.g. a Helm release name). Self-reference is
   not considered, as upstream.
8. **Normalize**: no-op under label tracking, for nil objects, live without the instance
   label, or CRDs. Otherwise backfills the desired tracking id onto live only when live has
   none, and removes the live instance label when the desired object has none. Never replaces
   an existing live id (a moved resource stays a diff), never modifies the target, only touches
   those two keys, and is idempotent (property tests).
9. **Atomic writes**: `SetAppInstance` validates everything (nil object, empty name, `:` in
   the name under annotation methods, label truncation, malformed metadata) before modifying
   the object; an error leaves the object unchanged.
10. Readers never panic on arbitrary metadata shapes (property test) and never modify input.

## Failure modes

| Situation | Result |
|-----------|--------|
| Tracking id malformed / missing | `GetAppName` = `""`, `GetAppInstance` = nil: not owned (APP-070); `ParseTrackingID` returns `ErrWrongResourceTrackingFormat` |
| Labels/annotations not a string map | readers: not owned; writers: `failed to get annotations from target object <gvk> <ns>/<name>: ...` / `failed to set app instance label: failed to get labels ...`; `Normalize`: `failed to get app instance label: ...` |
| Label cannot be truncated (`annotation+label`) | `failed to set app instance label: unable to truncate label to not end with a special character` (upstream), object unchanged |
| Instance name over 63 bytes with `label` | written as is; the API server rejects the apply (`must be no more than 63 bytes`), as upstream |
| Empty instance name, or `:` in it with annotation methods | `ErrInvalidInstanceName` (Patrol guard; upstream would write an id that never matches) |
| Nil object to a writer | `ErrNilObject` |
| Invalid label key | not detected by `New`; `Config.Validate` reports it; the API server rejects writes |
| `ownerReferences` not a list, or metadata unreadable | `IsManagedBy` = false (fail closed) |

## Security notes

- Tracking metadata on live objects is attacker-influenced: anyone with write access to a
  resource can set its tracking id, instance label and installation-id annotation, and the
  installation ID is not secret (it is on every managed object). `IsManagedBy` reads only the
  object's own metadata, so its guarantee is limited to: a tracking id copied from another
  object (different group, kind, namespace or name), an installation ID other than the
  configured one, an id naming another Application, and objects with ownerReferences are
  rejected. A self-consistent forged id (pointing at the object itself, naming the victim
  Application and carrying the installation ID) is accepted, as upstream
  (`TestIsManagedByAcceptsSelfConsistentForgedID`). Under label tracking any object without
  ownerReferences that carries the instance label is accepted. Tracking is therefore not an
  authorization boundary: the controls against adopting or pruning foreign resources are
  AppProject destination and resource-kind restrictions, which callers must enforce before
  deleting, and RBAC limiting who can write metadata on resources in the destinations
  (threat model T11).
- Label tracking cannot distinguish Patrol's label from third-party tools writing the same
  key; operators should prefer annotation tracking or the dedicated key
  `cd.patrol.example/instance`.
- Values are never logged by this package; error messages contain only kinds, namespaces and
  names.

## Parity feature areas served

| ID | Status | Scope covered here |
|----|--------|--------------------|
| APP-069 | in-progress | instance names (`ns_name`), qualified ↔ instance conversion; CLI parsing and RBAC names elsewhere |
| APP-070 | in-progress | id format/write/read, malformed → unowned, installation-id handling; live-set selection (ownerReferences, adoption) is cache/controller |
| APP-071 | implemented | label write/read without truncation, self-reference skipped |
| APP-072 | implemented | truncation, ownership via annotation |
| APP-073 | implemented | custom key; removal of the default key only (upstream quirk); legacy key not mapped |
| APP-074 | implemented | installation-id write/remove and exact matching |
| APP-075 | in-progress | self-reference predicate; status/prune/UI consequences are controller |
| APP-076, DIFF-026 | implemented | `Normalize`, `DiffTrackingKeys` |
| APP-077, APP-038, SYNC-082 | in-progress | detection and message; conditions and `FailOnSharedResource` are controller/sync |
| SYNC-080 | in-progress | predicate for the sync resource filter |
| DIFF-076, DIFF-087 | in-progress | tracking-derived app name; managed-root predicate (tracked, no ownerReferences, self-referenced) for managed selection; cache iteration, synthesized owners and adoption are cache/controller |

## Intentional differences and parity quirks

- **Empty method**: upstream `Normalize` treats `""` as label tracking (no-op) while the
  writers treat it as annotation; upstream settings never pass `""` (they default it to
  `annotation`). Patrol treats `""` as annotation everywhere, so behavior through settings is
  identical.
- **Unknown method values**: upstream `GetAppInstance` returns nil for them, other functions
  use annotation tracking. Patrol uses annotation tracking uniformly; `ValidateMethod` lets the
  settings loader reject such values.
- **Legacy instance label `applications.argoproj.io/app-name`**: upstream also writes it into
  pod templates of Deployments/ReplicaSets/StatefulSets/DaemonSets/Jobs when configured as the
  instance label key. The key is intentionally not mapped (`api/v1alpha1/keys.go`), so Patrol
  writes only `metadata.labels`.
- **Guards**: Patrol rejects an empty instance name and `:` in it under annotation methods
  (upstream writes ids that can never match); `SetAppInstance` is atomic on error.
- Reproduced quirks: `RemoveAppInstance` removes `app.kubernetes.io/instance` even when a
  custom instance label key is configured (APP-073 edge case); `Normalize` copies an absent
  desired tracking id as an empty annotation value; `DisplayName` replaces every `_`.

## Known gaps

- Nothing calls this package yet: controller target normalization, repo-server injection,
  cache attribution, comparison conditions, sync filters and the hydrator must adopt it before
  the in-progress features can move to implemented/verified.
- Shared-resource detection compares instance names only; with label tracking a chart's own
  `app.kubernetes.io/instance` label yields a warning, as upstream.

## Verification

- Unit tests (table-driven) port the upstream `resource_tracking_test.go` scenarios
  (truncation, parse formats, Normalize migrations, moved resources, CRDs, Helm instance
  labels) and add apps-in-any-namespace qualification, installation ID isolation, copied
  resources, malformed metadata and atomic error handling.
- Property tests (`pgregory.net/rapid`): codec inverse and canonical form; `TruncateLabel`
  maximal-prefix characterization; Set/Get/IsManagedBy/Remove round trip for every method,
  object shape and Application (idempotent writes); copied objects never managed; objects with
  ownerReferences never managed (and managed again once the owners are removed);
  `Normalize` target-immutability, key confinement, stale-id preservation, idempotence and
  differential agreement with `internal/diff.NormalizeTracking`; readers never panic.
- envtest (one kube-apiserver per package via `TestMain`, skipped when `KUBEBUILDER_ASSETS` is
  unset): objects written by the tracker are stored and read back from a real API server —
  exact ids for namespaced, cluster-scoped and apps-in-any-namespace resources, a copied Secret
  is not managed, a second installation sees nothing; under label tracking a ReplicaSet owned
  by a tracked Deployment and carrying the inherited instance label is attributed to the app
  but not managed, while the Deployment is; label tracking with a 70-character
  instance name is rejected by the API server while `annotation+label` with 80 characters is
  accepted; a label-tracked ConfigMap compared with `internal/diff` after switching to
  annotation tracking is in sync once normalized, a resource moved from another app is not.

Commands: `go test -race ./internal/tracking/...` and, with
`KUBEBUILDER_ASSETS=bin/envtest/k8s/1.36.0-darwin-arm64`, the same command for the envtest
tests.
