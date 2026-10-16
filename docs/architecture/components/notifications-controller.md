# Component: `internal/notifications/controller` (notifications controller)

Status: implemented (library + `Run` entry point; the `patrolcd notifications-controller`
command, its flags and the install manifests are not built yet). Decision records: ADR-0012
(notifications engine), ADR-0011 (Operation resource), ADR-0003 (resource model). Parity
area: `docs/parity/areas/09-notifications.md`; status overlay:
`docs/parity/status/notifications-controller.yaml`. Engine: `internal/notifications`
(see `notifications.md`), which this package uses unchanged.

The controller re-implements the observable behavior of Argo CD v3.5.5
`notification_controller/` + `util/notification/` (and the notifications-engine controller
loop they drive) on controller-runtime. No upstream code is imported or copied.

## Responsibilities

- Watch Applications of the control-plane namespace and, when `ApplicationNamespaces` is set
  (`application.namespaces`), of matching namespaces (globs, `/regexp/`); apply the
  Application label selector; watch AppProjects of the control-plane namespace (project-level
  subscriptions) and the notifications ConfigMap/Secret (by name).
- Convert each Application to the Argo CD JSON shape the engine and the upstream catalog
  expect (`TemplateApplication`), completing `status.operationState` from the Operation it
  names.
- Build one engine per configuration (control plane, and with self-service each application
  namespace that has its own ConfigMap), cached by object version (hot reload).
- Evaluate (freshness gate, subscriptions, triggers), deliver, and record the delivery state
  in `notified.notifications.cd.patrol.example` with a resourceVersion-guarded merge patch.
- Report failed deliveries as logs, `patrol_notifications_deliveries_total{succeeded="false"}`
  and Warning Events on the Application; retry them with backoff.
- Offer dry-run entry points for the CLI (`patrol admin notifications trigger run` /
  `template notify`) and for "what would be sent now" evaluations.

## Public API

```go
err := controller.Run(ctx, controller.Config{
    RestConfig: restCfg, Namespace: "patrol",
    ApplicationNamespaces: []string{"team-*"},   // application.namespaces
    AppLabelSelector: "",                        // --app-label-selector
    ConfigMapName: "", SecretName: "",           // default patrol-notifications-cm / -secret
    SelfServiceEnabled: false,                   // notificationscontroller.selfservice.enabled
    Processors: 1,                               // notificationscontroller.processors.count
    ResyncPeriod: 0,                             // default 60s
    Registry: promRegistry, Logger: slogger,
    RepoClient: repoClient, RepoResolver: resolver, // repo.* helpers (optional)
    Network: services.Network{RootCAs: certsForHost}, // per-host CAs, timeouts
    EgressAllowedPrefixes: nil, AllowPrivateEgress: false, // control-plane configuration
    TenantEgressAllowedPrefixes: nil,            // namespace configurations (default: none)
    LeaderElection: true,                        // LeaderElectionNamespace/ID optional
})

// CLI support
dr, err := controller.NewDryRun(cmData, secretData, controller.DryRunOptions{
    Projects: controller.NewProjectGetter(reader, ns),
    Operations: controller.NewOperationGetter(reader, 0),
    Repo: controller.NewRepoService(repoClient, resolver), Output: os.Stdout})
results, err := dr.TriggerRun(ctx, "on-sync-failed", app)        // trigger run
deliveries, err := dr.TemplateNotify(ctx, "app-sync-failed", app, recipients) // template notify (default console:stdout)
eval, err := dr.Evaluate(ctx, app)                                // dry evaluation, sends nothing
cm, secret, err := controller.LoadConfig(ctx, reader, ns, "", "")  // in-cluster config
app, err := controller.GetApplication(ctx, reader, ns, name)       // or ParseApplication(fileBytes)
dest, err := controller.ParseRecipient("slack:my-channel")
view, err := controller.TemplateApplication(ctx, appMap, ops)     // Argo CD view of an Application
```

`Run` returns `nil` after `ctx` is cancelled (deliveries made so far are recorded first) and
an error for invalid configuration, cache-sync failure (e.g. missing RBAC), a lost leader
lease or a controller failure. It serves no HTTP endpoint: the command serves `Registry`
(and its probes). Metrics are registered on `Registry` only; a nil registry keeps them
unregistered.

## Application view (`TemplateApplication`)

Triggers and templates see `.app` with Argo CD field paths. The stored Application JSON is
deep-copied and adjusted:

| Path | Source |
|------|--------|
| `metadata`, `spec`, `status.sync`, `status.health`, `status.history`, `status.summary`, `status.resources`, `status.reconciledAt` | Application, unchanged (same JSON names as Argo CD). |
| `status.conditions` | Active (`status: True`) conditions of Argo CD types as `{type, message, lastTransitionTime}`; Patrol's aggregate `Reconciled`/`Synced`/`Healthy` are dropped; key removed when empty. |
| `status.operationState.{phase,message,startedAt,finishedAt,retryCount}` | Summary on the Application; missing fields from the Operation. |
| `status.operationState.syncResult` | Operation `status.syncResult` (`resources`, `revision(s)`, `source(s)`, `managedNamespaceMetadata`) overlaid by the summary (summary wins). |
| `status.operationState.operation` | Requested operation from the Operation spec `{sync (no manifests), initiatedBy, info, retry[, rollback]}`. |
| `operation` (top level) | Same, only while the Operation is not terminal (Argo CD's in-flight `Application.operation`). |
| phase `Cancelled` | Shown as `Failed` (message `Operation terminated` when empty): Argo CD ends a terminated operation Failed, so `on-sync-failed` fires as upstream. `Rejected`, `Pending`, `Terminating` are passed through. |

The Operation is used only when it exists in the Application's namespace, its
`spec.application` is the Application and its `startedAt` matches the summary (a re-created
Operation of the same name is ignored). A missing Operation (retention) leaves the summary
as is; other lookup errors fail the evaluation so it is retried rather than rendered
incomplete. Terminal Operations are cached (LRU, 32 MiB of JSON, manifests and plan never
kept); Operations are read uncached and never watched.

## Processing algorithm

Per Application key (controller-runtime work queue, `Processors` concurrent keys, one key
never concurrently):

1. Out-of-scope namespace (neither the control plane nor matching `ApplicationNamespaces`):
   ignored without reading it (upstream skip reason "app is not in one of the
   application-namespaces, nor the notification controller namespace"). Deleted: forgotten.
2. Read from the cache; while this process's last state patch is not yet visible there, read
   the API server instead (so an event queued before our write cannot re-send).
3. Engines: control-plane configuration, plus the application namespace's configuration with
   self-service. A configuration that fails to parse yields no engine (logged, Warning Event
   `InvalidNotificationsConfiguration` on its ConfigMap, once per version).
4. No destinations in any engine (subscription view of labels/annotations/project only): done.
5. `TemplateApplication`; freshness gate (NOTIF-010, `SyncStatusRefreshed` on the Argo CD
   view): skipped while a completed operation's `finishedAt` is after `reconciledAt`.
6. Delivery state = annotation (+ deliveries whose state patch failed earlier in this
   process). Each engine runs `Process` in turn on the shared state (namespace engine keys are
   prefixed with the namespace, so configurations never deduplicate each other).
7. Report results (below), then persist the state delta: JSON merge patch of the annotation
   with `metadata.resourceVersion` (optimistic concurrency); on conflict re-read and re-apply
   the delta to the current annotation (concurrent entries kept), up to 8 attempts with
   backoff; persisted even during shutdown (30s bound). A failed persist keeps the delta in
   memory (5 min) for the next evaluation.
8. Requeue: after `ResyncPeriod` + up to 10% jitter; after failed deliveries 5s, doubling
   per consecutive failing evaluation up to `ResyncPeriod`.

Watches: every Application change (status included) and AppProject annotation changes
(enqueuing the project's Applications through a `spec.project` index; empty = `default`).
ConfigMap/Secret changes rebuild the engine lazily at the next evaluation.

There is no start-up suppression (as upstream): conditions already true when the controller
first evaluates an Application are delivered unless the delivery state already records them.
Dedup across restarts relies only on the annotation.

## Self-service (NOTIF-054)

- Enabled by `SelfServiceEnabled`; ConfigMap/Secret informers then cover the application
  namespaces (only the configured names, field-selected).
- An Application of namespace N is processed with the control-plane configuration and, if
  present, N's configuration; never with another namespace's.
- N's `$key` references resolve only against N's Secret; unresolved references stay literal
  (engine warning). `.secrets` is hidden from every configuration (control plane included).
- Failures about services/triggers a configuration does not define are not reported when the
  other configuration of the Application defines them (a namespace configuration never
  reports them), so subscriptions meant for one configuration do not produce Events from the
  other.
- AppProjects (and their subscriptions) always come from the control-plane namespace.
- Namespace configurations have their own egress policy: always the SSRF-guarded dialer,
  opened only by `TenantEgressAllowedPrefixes` (default none). `AllowPrivateEgress`,
  `EgressAllowedPrefixes` and a custom `Network.DialContext` apply to the control-plane
  configuration only, so a range opened for an in-cluster Alertmanager is not reachable
  from tenant ConfigMaps (threat model T8). Other `Network` settings (CAs, URL validation,
  timeouts, proxy) are shared.
- Failure Events go to the Application's namespace. A failure of the control-plane
  configuration about an Application outside the control-plane namespace is reported there
  with the trigger, service and failure class only (e.g. `the service answered HTTP 500`,
  `the endpoint is not allowed`), never the endpoint URL (incoming-webhook tokens sit in
  the path), recipient or response body (threat model T22); the full redacted error is in
  the controller log. A namespace configuration's own failures keep the full error.

## Events and metrics

| Signal | When |
|--------|------|
| Warning `NotificationDeliveryFailed` on the Application, action `Notify:<trigger>:<service>[:<config namespace>]` | Render/delivery failure, unknown trigger/template/service. The events.k8s.io recorder merges Events with the same action into a series (count + last seen), so each failing (trigger, service) has its own Event and repeated failures do not create new ones; the note is the first failure of the series. The note carries the full redacted error only when the failing configuration lives in the Application's namespace; otherwise trigger, service and failure class (see Self-service). |
| Warning `InvalidNotificationsConfiguration` on the ConfigMap | The configuration cannot be parsed (secret values redacted). |
| `patrol_notifications_deliveries_total{trigger,service,succeeded}` | One per delivery attempt (not for duplicates, undefined triggers, failures handled by the other configuration or interrupted by shutdown). |
| `patrol_notifications_trigger_eval_total{name,triggered}` | One per condition evaluation. |

Logs (slog): `notification sent` (Info), `failed to deliver notification` (Warn, with
application, trigger, service, recipient, condition, redacted error), skips at Debug. The
engine's own per-delivery failure record is demoted to Debug.

## RBAC and network

Needs: `applications` get/list/watch/patch (control-plane namespace, or cluster-wide /
the listed namespaces); `appprojects` get/list/watch (control plane); `operations` get
(application namespaces); `configmaps`/`secrets` list/watch (field-selected by name; cluster
wide with self-service and pattern namespaces); `events.k8s.io` events create/patch;
`coordination.k8s.io` leases with leader election. With literal application namespaces only
those namespaces are cached (namespaced Roles suffice).

Control-plane deliveries use the SSRF-guarded dialer unless `AllowPrivateEgress` (or a
custom `Network.DialContext`) is set; `EgressAllowedPrefixes` opens specific ranges (e.g. an
in-cluster Alertmanager). Namespace (self-service) configurations always use the guarded
dialer with `TenantEgressAllowedPrefixes` only (default none). With an HTTP proxy from the
environment the guard applies to the proxy connection (engine behavior).

## Failure modes

| Situation | Behavior |
|-----------|----------|
| Invalid `Config` (selector, names, bounds) | `Run` fails before starting. |
| `ApplicationNamespaces` entry that cannot be compiled (RE2 or glob) | Warning logged at start; the entry never matches; other entries keep working. |
| Cache sync fails (RBAC) / lease lost | `Run` returns an error. |
| ConfigMap invalid | No engine for that configuration; Event + error log; other configurations keep working. |
| Operation lookup error (not NotFound) | Evaluation fails; controller-runtime retries with backoff. |
| Delivery failure | Not recorded; Event + metric; retried (5s doubling, capped by resync) while the condition holds. |
| State patch conflict | Re-read and merge, up to 8 attempts. |
| State patch fails (e.g. no `patch` RBAC) | Delta kept in memory 5 min (no duplicate within that window in this process); after that, or after a restart, deliveries repeat (upstream: repeat every evaluation). |
| Shutdown during delivery | Undelivered destinations untouched; delivered ones recorded before exit. |

## Parity IDs covered

See `docs/parity/status/notifications-controller.yaml`: NOTIF-001 (in-progress), 002, 003,
004, 006, 009, 010, 014 (reload), 018, 025, 026, 032, 033, 047, 050, 051, 052, 053, 054,
055, 057, 058; CLI entry points of 104/106/108 (in-progress).

## Deviations (deliberate)

- Optional leader election (upstream runs a single replica without one); losing the lease
  stops `Run` with an error.
- Warning Events for delivery and configuration failures (upstream only logs).
- Failed deliveries are retried with a 5s doubling backoff instead of only at the next
  resync/app event.
- Out-of-scope namespaces: with only literal names the cache is restricted to them; with
  patterns the watch is cluster-wide and out-of-scope Applications are ignored (upstream
  filters the list but not the watch; the result is the same).
- `ApplicationNamespaces` regular expressions are Go RE2; upstream uses regexp2. An entry RE2
  cannot compile is logged and never matches, like invalid entries upstream and like the
  other Patrol components reading `application.namespaces` (`internal/webhook`,
  `internal/project`). Lookaround entries therefore differ: the upstream-documented
  exclusion example `/^((?!not-allowed).)*$/` matches every other namespace upstream but
  nothing here (use globs or an explicit list instead).
- Delivery-failure Events (upstream has none) in a namespace other than the failing
  configuration's carry only trigger, service and failure class.
- Namespace configurations get their own, closed-by-default egress policy (upstream has no
  egress guard at all).
- `.app` is the Argo CD view described above, including `Cancelled` → `Failed`.
- `repo.GetAppDetails` treats an empty project as `default` when resolving repository
  credentials.

## Known gaps

- No `cmd/patrolcd notifications-controller` command, flags/env/`patrol-cmd-params-cm`
  wiring, log format flags, metrics HTTP server, repo-server TLS flags (NOTIF-001, 005,
  007, 008, 056) or install manifests/RBAC (NOTIF-012); `Config` carries everything they
  need.
- CLI commands (`patrol admin notifications ...`, NOTIF-104..108) are not built; the
  `DryRun` API, `LoadConfig`, `GetApplication` and `ParseApplication` back them.
- Custom CA trust (`patrol-tls-certs-cm`, NOTIF-011) must be passed as
  `Config.Network.RootCAs` by the command.
- Event notes of a series keep the first failure; a different error for the same
  (trigger, service) within the recorder's 6-minute series window only increments the
  count.
- The Operation enrichment depends on the Application controller writing
  `status.operationState.operationName`; until it runs, envtest fixtures set statuses
  directly.

## Requests to other owners

- `docs/architecture/threat-model.md`: T8/T22 mitigations may cite the tenant egress policy
  and the tenant-safe failure Event notes described above.
- `api/v1alpha1`: may mirror the Event reasons `NotificationDeliveryFailed` and
  `InvalidNotificationsConfiguration` (declared here).
- `deploy/`: Role/ClusterRole rules listed under "RBAC and network".
- Application controller: keep `status.operationState.operationName`, `startedAt` and
  `finishedAt` in the summary, and `reconciledAt` updated after an operation (freshness gate).
- No engine change was needed.

## Verification

Commands (repository root, darwin/arm64, Go 1.27.2, 2026-10-16):

```bash
go build ./...
go vet ./internal/notifications/controller/...
golangci-lint run ./internal/notifications/controller/...
KUBEBUILDER_ASSETS=$PWD/bin/envtest/k8s/1.36.0-darwin-arm64 go test -race -count=1 ./internal/notifications/controller/
go test -race -count=1 ./internal/notifications/controller/   # without envtest: envtest tests skip
```

Last run (2026-10-16, after the T22/T8/namespace-expression review fixes):
`go build ./internal/notifications/...` exit 0 (`go build ./...` exit 1 only in
`internal/reposerver`, another workstream's package in progress); `go vet` exit 0;
`golangci-lint` 0 issues; `go test -race` with envtest `ok` (64 top-level tests passed,
none skipped, ~51s); without `KUBEBUILDER_ASSETS` `ok` (53 passed, the 11 envtest tests
skipped).

- Unit tests: Application view (table + `rapid` property: input never modified, no aliasing,
  another Application's Operation never exposed), `Cancelled` mapping, state deltas
  (property: concurrent entries survive), memo, namespace scope, config validation, engine
  factory (self-service isolation, hot reload, invalid configuration Event once), Operation
  getter/LRU, result reporting (metrics, Events, suppression), tenant-safe failure notes and
  failure classes, tenant egress policy (control-plane `AllowPrivateEgress`, allow-list or
  custom dialer never reach tenant engines), uncompilable namespace entries (lookahead)
  warned and never matching, repo helpers, dry-run API.
- envtest (real kube-apiserver 1.36 with the generated CRDs, httptest webhook receivers):
  transitions Running → Succeeded with Operation resources in the payload, freshness gate,
  dedup across a controller restart, health degradation; self-service isolation (namespace A
  config cannot see namespace B apps nor resolve control-plane Secret keys, `.secrets`
  hidden, disabled flag ignores namespace configs); project-level subscriptions incl. apps in
  other namespaces, empty project and prompt pickup of project annotation changes; failure →
  Warning Event + `succeeded="false"` metric, retry until success; namespace scope (regexp)
  and label selector; conflict-merging state patch; custom ConfigMap/Secret names, hot
  reload, invalid ConfigMap Event and recovery; deleting the state annotation re-sends once;
  catalog `on-sync-failed` for a cancelled Operation with `oncePer` revision; 4 processors +
  leader election (Lease held, one delivery per app); tenant isolation of failure Events
  (control-plane URL path, host and response body never in tenant-namespace Events, kept in
  the control-plane namespace) and of egress (tenant webhook to loopback refused while the
  control plane may reach it).
