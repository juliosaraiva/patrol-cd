# Component: ApplicationSet, notification, badge and proxy extension APIs

Status: implemented (handlers, Go client, tests); not yet wired into `cmd/patrolcd`.
Decision records: [ADR-0005](../adr/0005-public-api.md), [ADR-0009](../adr/0009-authn-authz.md).
Normative contract: [`api-contract.md`](../api-contract.md) ("ApplicationSets" and "Notifications,
webhooks, badges, extensions"). Framework: [`apiserver.md`](apiserver.md). Parity status:
[`docs/parity/status/api-appsets-misc.yaml`](../../parity/status/api-appsets-misc.yaml).

| Package | Contents |
|---------|----------|
| `internal/apiserver/services/applicationsets` | ApplicationSet list/get/create (upsert, dry run)/delete, generate, resource tree, events, SSE watch |
| `internal/apiserver/services/notifications` | Names of configured notification services, triggers and templates |
| `internal/apiserver/services/badge` | Public SVG status badge |
| `internal/apiserver/services/extensions` | Proxy extensions (`/extensions/<name>/...`) |
| `internal/apiserver/services/internal/miscapitest` | Test harness: real route chain, token service, enforcer and settings over HTTP, envtest |
| `pkg/apiclient` (`applicationsets.go`, `notifications.go`, `badge.go`, `extensions.go`) | Request/response types shared with the server and the Go client methods |

Each package exports `New(Deps) *Service` (`Service` implements `apiserver.Service`; the
concrete type is returned because `internal/apiserver` imports these packages for the OpenAPI
document) and a `Deps` struct of narrow interfaces. Routes whose collaborators are nil answer
503, so `New(Deps{})` registers every route for `DocumentedServices()`.

Deep links: the shared evaluator `internal/apiserver/services/links` (`NewContext`, `Evaluate`,
`ClusterData`) already existed when this work started and is used as is by the applications and
projects services; `pkg/apiclient/links.go` holds `LinkInfo`/`LinksResponse` (see "Gaps").

## Routes and authorization

RBAC objects are `<project>/<name>` in the control-plane namespace and
`<project>/<namespace>/<name>` elsewhere (`rbac.AppObject`).

| Method | Path | Operation | Authorization |
|--------|------|-----------|---------------|
| GET | `/api/v1/applicationsets` | `listApplicationSets` | `applicationsets, get` per item on each of its projects (`rest.Can`); `projects`, `selector`, `appsetNamespace`, `limit`/`continue` |
| GET | `/api/v1/stream/applicationsets` | `watchApplicationSets` | `applicationsets, get` per event when sent, on each project of the object; `name`, `projects`, `selector`, `appsetNamespace`/`appSetNamespace`, `resourceVersion`, `Last-Event-ID` |
| POST | `/api/v1/applicationsets` | `createApplicationSet` | `applicationsets, create` on every project the generated Applications can use (`appset.Projects`); upsert of a different spec also `update` on every project of the new spec and of the existing object; `?upsert=&dryRun=` |
| POST | `/api/v1/applicationsets/generate` | `generateApplicationSet` | as create (no update) |
| GET | `/api/v1/applicationsets/{name}` | `getApplicationSet` | `applicationsets, get` on every project of the object; `?appsetNamespace=` |
| DELETE | `/api/v1/applicationsets/{name}` | `deleteApplicationSet` | `applicationsets, delete` on every project of the object; `?appsetNamespace=&cascade=&propagationPolicy=foreground\|background` |
| GET | `/api/v1/applicationsets/{name}/resource-tree` | `getApplicationSetResourceTree` | `applicationsets, get` on every project of the object |
| GET | `/api/v1/applicationsets/{name}/events` | `listApplicationSetEvents` | `applicationsets, get` on every project of the object |
| GET | `/api/v1/notifications/services` | `listNotificationServices` | authenticated (no RBAC, as upstream) |
| GET | `/api/v1/notifications/triggers` | `listNotificationTriggers` | authenticated |
| GET | `/api/v1/notifications/templates` | `listNotificationTemplates` | authenticated |
| GET | `/api/badge` | `getBadge` | public while `statusbadge.enabled`; 404 otherwise |
| GET, POST, PUT, PATCH, DELETE | `/extensions/{extension}/{path...}` | `invokeExtension<Method>` | `extensions, invoke, <extension>` and `applications, get` on the Application of `Patrol-Application-Name`/`Patrol-Project-Name` |

The projects of an ApplicationSet are those of `appset.Projects`: the literal
`spec.template.spec.project` ("default" when empty) and the `spec.project` of every
generator-level template that applies. Every action is authorized on the RBAC object of each
of them (`<project>/<name>`), for new and for existing objects, so a tenant cannot create,
read, rewrite or delete an ApplicationSet that generates Applications in a project it has no
rights in (threat model T3). The API refuses to create a templated project; on an existing
object (created with kubectl) a templated project fails closed: it is authorized as the
wildcard project, `*/<name>`, which only grants covering every project match. Audit entries
record the object of the template project.

## Behavior and invariants

### ApplicationSets

- Namespaces: the control-plane namespace plus `Config.Namespaces` (names, globs, `/regexp/`,
  the same matcher as `sourceNamespaces`). The wiring fills it from
  `applicationsetcontroller.namespaces`. Other namespaces are refused with 403
  `namespace '<ns>' is not permitted` (get, create, generate, tree, events, delete) or filtered
  (list, watch).
- No existence oracle: get, tree, events and delete read the object first and answer the same
  403 `permission denied` for a missing and a forbidden ApplicationSet (the denial is audited
  with the full RBAC triple).
- Create, dry run and generate: `appset.ValidateForAPI` (unrecognized generators, templated
  project in `spec.template` or in a generator-level template) → 400
  `error validating ApplicationSets: ...`; the namespace must be enabled; `create` is authorized
  on every project before anything about the projects is revealed; each project must exist
  (400 `ApplicationSet references project <p> which does not exist`) and, outside the
  control-plane namespace, list the namespace in `sourceNamespaces` (403).
- Create: server-managed metadata and status of the body are dropped. An existing object with
  the same spec, labels, annotations and finalizers is returned unchanged; a different one
  needs `upsert` (400 `existing ApplicationSet spec is different, use upsert flag to force
  update`). Upsert merges labels and annotations, replaces finalizers but keeps the
  controller's `cd.patrol.example/applicationset-finalizer`. A `metadata.resourceVersion` in the
  body makes it conditional (409 `conflict`); without one, conflicts are retried up to 10 times
  on fresh reads (409 `Failed to update ApplicationSets. Too many conflicts` after that).
- Dry run returns the input with `status.resources` (sorted by name) and
  `status.resourcesCount` of the would-be Applications; nothing is written.
- Generate and dry run: `Deps.Generator` is `applicationsets.NewGenerator` (the
  `internal/appset` generators and templates, tokenRef strict mode, no SCM proxy, as
  `appset.NewAPIGenerator`), which enforces the bounds while it generates: the deadline
  `Config.GenerateTimeout` (default 60 s → 504) is checked before every generator call and
  every rendered Application; the parameter sets of all generator entries together
  (`MaxGenerateParameterSets`, 20 000, counted before post-selectors; also the per-generator
  bound of Matrix products, enforced while they are built), the Applications
  (`MaxGeneratedApplications`, 5000) and their JSON bytes (`MaxGenerateResponseBytes`, 32 MiB)
  are capped as they are produced (400 naming the limit). The output is checked again before
  it is returned. Generator failures are 400 with `unable to generate Applications of
  ApplicationSet: <err>\n<captured logs>` (logs bounded to 16 KiB); generation stops at the
  first failure, which is the error `appset.GenerateApplications` reports. Generated
  Applications live in the ApplicationSet namespace. Measured on the 12 × Matrix(141 × 141)
  fan-out (238 572 Applications, 4.7 GB allocated, 5.8 s unbounded): 5 001 rendered, 125 MB,
  0.14 s at the default limits; 96 ms, 81 MB under a 100 ms deadline
  (`generator_internal_test.go`).
- Delete: `cascade=false` → orphan propagation; `propagationPolicy` foreground or background
  (default); a UID precondition prevents deleting a recreated namesake.
- Resource tree: one node per `status.resources` entry (group/version/kind defaulted to
  `cd.patrol.example/v1alpha1/Application`), namespace of the ApplicationSet, health, sync
  status as the info item `Sync Status`, parentRef to the ApplicationSet (with UID), sorted by
  `group/kind/namespace/name`.
- Events: `core/v1` Events in the ApplicationSet namespace whose involved object has its name,
  namespace and UID (field selectors on the uncached reader, re-checked in memory), at most
  `MaxEvents` (1000).
- Watch: Server-Sent Events `{type, object}` through `rest.ServeSSE` (heartbeats, credential
  re-verification, bounded buffer). Without `resourceVersion` the server lists, sends the
  current state when `name` is given (sorted by qualified name), then watches from the list's
  resourceVersion; a numeric `resourceVersion` or `Last-Event-ID` starts the watch there
  (list-then-watch clients miss nothing) and also drops older objects (upstream filter). The
  API server watch is re-established when it closes; an expired version ends the stream with
  an `error` event 410. Filters: name, namespace, projects, selector, enabled namespace;
  `applicationsets, get` is evaluated with the current policy for every event.
- Audit: every mutation (create, upsert, delete, generate) is audited with the RBAC triple and,
  for writes, the ApplicationSet as target; `Deps.Recorder` records the upstream Events
  `ResourceCreated` (`<user> created ApplicationSet`), `ResourceUpdated` (`<user> updated
  ApplicationSets spec`) and `ResourceDeleted` (`<user> deleted ApplicationSets`) on the
  ApplicationSet.

### Notifications

- Source: `patrol-notifications-cm` (and the key names of `patrol-notifications-secret`) of the
  settings snapshot; parsed with `notifications.ParseConfig` and the built-in catalog unless
  `Config.DisableDefaultCatalog` (same default as the controller). Parsed once per settings
  fingerprint.
- Missing ConfigMap → empty lists (200). A configuration that does not parse → 500 with a
  fixed detail; parse errors (which may quote values) are not returned or logged, only counted.
- Only names are returned (and, for triggers, the distinct condition descriptions). Secret
  values never reach the parser (placeholders replace them).

### Badge

- `statusbadge.enabled` false → 404 before any lookup. Enabled: `name` (DNS subdomain),
  `appNamespace` or `namespace` (DNS label; not enabled → "Not Found"), repeatable `project`
  (≤ 50, DNS label case-insensitively; aggregates every Application of those projects in the
  namespace, paged by `PageSize` 500 up to `MaxApplications` 10 000, beyond which the badge is
  Unknown; a controller-runtime cache reader, which cannot paginate and truncates at `Limit`,
  is detected by its `continue-not-supported` token and read in one call bounded by
  `MaxApplications`+1, without deep copies),
  `revision`, `keepFullRevision`, `showAppName`, `width` (1..4096, else ignored). All
  parameters are validated (400) before any read.
- Upstream colors and sizes (131/192/400 px, title row 40 px high); the SVG is drawn by
  Patrol (no copied asset, no logo); every text is XML-escaped and stripped of characters XML
  forbids. Headers `Content-Type: image/svg+xml`, `Cache-Control: private, no-store`,
  `Access-Control-Allow-Origin: *`. Not audited; presented tokens are ignored.

### Proxy extensions

- `Config.Enabled` (server.enable.proxy.extension) false → 404 `proxy extensions are not
  enabled`.
- Order: extension name (`^[A-Za-z0-9_-]+$`, CR/LF stripped) and headers are validated (400
  `Invalid headers: ...`), WebSocket upgrades are refused unless `EnableWebSockets` (400), then
  authorization, then the extension lookup (404 `Extension not found`), then backend selection
  (400 `invalid extension`). Authorization failures are all 403 `permission_denied`
  `Unauthorized extension request`: no `applications, get` on
  `<header project>/[<ns>/]<name>`, no `extensions, invoke, <name>`, Application namespace not
  enabled (`Config.ApplicationNamespaces`), Application missing, project mismatch, project
  missing, namespace not in the project's `sourceNamespaces`, destination unresolvable or not
  permitted (`project.IsAppDestinationPermitted`). Callers therefore learn nothing about
  Applications or unregistered extensions they may not use. Each case is audited with a reason.
- Registry: built from `settings.Extensions()` (validated `extension.config` /
  `extension.config.<name>`, `$` references resolved); rebuilt when the configuration,
  including resolved header values, changes (idle connections of the old transports closed).
  A service URL must be `http`/`https` with a host and no credentials, query or fragment;
  otherwise that extension is not registered (logged) and the others keep working. As upstream,
  only the scheme and host of the URL are used; the forwarded path is the request path after
  `/extensions/<name>` (escaping preserved), the query is forwarded unchanged.
- Backend selection: the single service of a one-service extension; otherwise the service
  registered for the Application destination's name or server, then for the resolved cluster's
  name and/or server.
- Forwarded request headers: allow-list (`Accept*`, `Cache-Control`, `Pragma`, `Content-Type`,
  `Content-Encoding`, `Content-Language`, conditional and range headers, `User-Agent`,
  `Traceparent`, `Tracestate`, `Patrol-Application-Name`, `Patrol-Project-Name`, plus
  `Config.AllowedRequestHeaders`); never `Authorization`, `Cookie`, `Proxy-Authorization`,
  `Forwarded`/`X-Forwarded-*`, hop-by-hop headers or client-supplied identity headers. The server
  sets `X-Request-Id`, `Patrol-Namespace`, `Patrol-Target-Cluster-Name`/`-URL`, `Patrol-User-Id`,
  `Patrol-Username`, `Patrol-User-Groups` (RBAC groups), then the configured headers (Secret
  values revealed only into the outgoing request; never logged).
- Responses: hop-by-hop headers dropped (`httputil.ReverseProxy`), `Set-Cookie` removed;
  `Content-Length` over `MaxResponseBytes` (64 MiB) → 502; a streamed body over the cap aborts the
  connection (never a silently truncated "complete" response). Redirects are returned to the
  caller, never followed. Transport: no environment proxy, dial timeout and keep-alive, idle
  timeout and pool size from the backend config, TLS handshake 10 s, `ResponseHeaderTimeout`
  (30 s) and `RequestTimeout` (2 min, not for WebSocket sessions) → 504; unreachable → 502. Error
  responses never contain backend addresses.
- Request bodies capped at `MaxRequestBytes` (16 MiB) → 413. Cookie-authenticated mutations
  need `X-Requested-With: patrol` (route chain).
- Audit: every call (`AuditAlways`) with `extensions, invoke, <name>`; the Application is in
  the message, not the target (no Kubernetes Event per proxied call). Metrics
  `patrol_server_extension_requests_total{extension,status}` and
  `patrol_server_extension_request_duration_seconds{extension}` on `Deps.Registry` (only
  registered extension names become labels).

## Differences from upstream

| Area | Upstream | Patrol | Reason |
|------|----------|--------|--------|
| Missing ApplicationSet (get, tree, events, delete) | NotFound | 403 `permission denied`, same as forbidden | no existence oracle (threat model T1) |
| ApplicationSet namespaces | `application.namespaces` of argocd-server; delete unchecked | `Config.Namespaces` (applicationsetcontroller.namespaces); delete checked; create also needs the project's `sourceNamespaces` | tenant boundary (T3) |
| ApplicationSet RBAC (create, generate, get, list, watch, tree, events, upsert, delete) | `spec.template.spec.project` only | every project of `appset.Projects` (generator templates too); templated project on an existing object → `*/<name>` | escalation and cross-tenant reads, rewrites and deletes through generator templates (T3) |
| Generate / dry-run bounds | none | deadline, total parameter sets, Applications and bytes enforced while generating | DoS (any user with `create` on one project) |
| Upsert | always retried; finalizers replaced | conditional with `metadata.resourceVersion`; controller finalizer kept | optimistic concurrency (ADR-0005) |
| Delete | default propagation | `cascade`, `propagationPolicy`, UID precondition | contract parity with Applications |
| Watch `resourceVersion` | minimum version filter | also the watch start (gapless list-then-watch); expired → 410 event | correctness |
| List | no pagination | `limit`/`continue` | api-contract.md |
| Informer wait after writes | `waitSync` up to 2 s | none (no server-side cache) | |
| Notification lists | random order, names only; ListTriggers panics on config errors | sorted; trigger descriptions; 500 with fixed detail | |
| Badge disabled | "Unknown" badge (200) | 404 | contract/task requirement; reveals nothing either way |
| Badge drawing | regex edits of an embedded asset with logo | own SVG, escaped text, no logo | no copied assets |
| Extension authorization failures | 401 `Unauthorized extension request` | 403 with the same detail | 401 makes the UI log the user out |
| Extension headers | `Argocd-*` | `Patrol-*` | mechanical rename (crds.md) |
| Forwarded headers | all but `Authorization`/`Cookie` | allow-list | T8/T22 |
| Backend responses | passed through | `Set-Cookie` removed, size cap, timeouts | session fixation, DoS |
| WebSocket | proxied | refused unless `EnableWebSockets` | task requirement |
| Proxy disabled | falls through to static files | 404 problem | |

## Requests to other owners (wiring)

- `cmd/patrolcd` / `internal/apiserver` server assembly: construct the services with
  `applicationsets.New(Deps{Reader: cached client, APIReader: uncached client, Writer, Watcher:
  client.WithWatch, Generator: applicationsets.NewGenerator(generators.Config{Client,
  Namespace, Repos, Dynamic, SCM: {AllowedSCMProviders, enable flag, root CA}, Plugin}),
  Recorder: the server's EventRecorder})` (`appset.NewAPIGenerator` no longer satisfies
  `applicationsets.Generator`: it does not enforce the limits), `notifications.New(Deps{Settings})`,
  `badge.New(Deps{Settings, Reader (cached or not), Config{Namespace, ApplicationNamespaces}})`,
  `extensions.New(Deps{Settings, Reader, Registry, Config{Enabled:
  server.enable.proxy.extension, Namespace, ApplicationNamespaces}})`, and pass them in
  `Deps.Services`. Patrol API server RBAC needs get/list/watch/create/update/delete on
  applicationsets, get/list on events, applications, appprojects and clusters.
- `ui/`: `invokeExtension` comments mention 401; Patrol answers 403 `permission_denied` (the
  client already maps it to an `ApiError`). The badge URL helper uses `appNamespace`, which is
  accepted (as is upstream `namespace`).
- `api/openapi/openapi.json` must be regenerated (`go run ./internal/apiserver/cmd/genopenapi`)
  after all services land; `TestOpenAPIUpToDate` fails until then.

- `internal/appset` owner: `appset.GenerateApplications` (controller path) has no total bound
  across generator entries and checks the context only after a failed Transform; the API does
  not use it for generation any more (it reimplements the same loop with bounds, checked for
  equivalence in `TestBoundedGeneratorMatchesAppset`). Accepting a budget there would let both
  share one loop.

## Gaps

- Not wired into `cmd/patrolcd`; nothing here is "verified" end to end with the real server
  binary or the UI.
- Only the List generator is exercised through the API tests (the other generators are covered
  by `internal/appset`); the SCM allow-list for API generation depends on the wiring above.
- No garbage collector in envtest: cascading deletion of generated Applications is verified by
  the propagation finalizer the API server sets, not by Applications disappearing.
- The expired-watch (410) path is not exercised (envtest's compaction window is not
  controllable); the mapping is in `watch.go`.
- Extension `Location` headers are passed through unchanged (they may name the backend host).
- `pkg/apiclient/links.go` was rewritten by this work after it had been created concurrently by
  another service owner (an unintended overwrite); the content keeps the field names and JSON
  tags its users (`internal/apiserver/services/links`, `applications.go`) rely on, but the
  original file's exact text (comments) could not be recovered and should be checked by its
  owner.

## Verification

Run on 2026-10-16 (macOS arm64, Go 1.27.2, envtest kube-apiserver 1.36.0); full output in
[`docs/verification/runs/2026-10-16-api-appsets-misc.txt`](../../verification/runs/2026-10-16-api-appsets-misc.txt).

```bash
go vet ./pkg/apiclient/ ./internal/apiserver/services/{applicationsets,notifications,badge,extensions}/ ./internal/apiserver/services/internal/...
golangci-lint run ./internal/apiserver/services/{applicationsets,notifications,badge,extensions,internal}/...
KUBEBUILDER_ASSETS=$PWD/bin/envtest/k8s/1.36.0-darwin-arm64 go test -race -count=1 \
  ./internal/apiserver/services/applicationsets/ ./internal/apiserver/services/notifications/ \
  ./internal/apiserver/services/badge/ ./internal/apiserver/services/extensions/ ./pkg/apiclient/
go run ./internal/apiserver/cmd/genopenapi -out <scratch>/openapi.json   # generation succeeds with the new routes
go test -count=1 ./internal/apiserver/
go run ./hack/parity
```

Results (rerun after the review fixes, same commands with `-v`): `go build ./...`, vet and
golangci-lint clean (0 issues); the four service packages and `pkg/apiclient` pass with envtest
(73 top-level tests, 78 subtests, none skipped; V-APIAM-01 in `docs/verification/log.md`);
the OpenAPI document generates; in `internal/apiserver` only `TestOpenAPIUpToDate` fails
(`api/openapi/openapi.json is stale`), as expected until the document is regenerated after all
services land; `hack/parity` reports no problem.

Tests: route-level authorization matrices (allowed role, other project, read-only, no role,
project role token, SSO group, tenant namespace user, anonymous), existence-oracle checks
(missing vs forbidden bodies identical), tenant isolation (namespaced RBAC objects, disabled
namespaces, sourceNamespaces), validation (templated and generator-level projects,
unrecognized generators, missing projects), upsert semantics (idempotent create, conflict
without upsert, merge, conditional 409, project change permissions, controller finalizer kept),
dry run and generate (namespace forcing, timeout, output bounds, generator errors; bounds and
deadline enforced inside the real generators on a 238 572-Application fan-out, equivalence with
`appset.NewAPIGenerator`), every-project authorization of reads, upserts and deletes of
existing ApplicationSets with generator-level or templated projects, delete
propagation, resource tree and UID-filtered events, SSE watch (snapshot, modify, delete,
per-event RBAC, revocation mid-stream, gapless list-then-watch), audit entries and Events;
notifications (catalog, descriptions, no secrets or service configuration in any response,
missing ConfigMap, invalid configuration and recovery, authorization); badge (disabled 404,
colors, revision/width/title options, project aggregation (paged through the uncached client
and through a controller-runtime cache, with the cap), namespaces, validation before
lookup, XML escaping and well-formedness, headers, public access); proxy extensions (header
contract, allow-list and identity headers, Secret-backed headers not logged, Set-Cookie
removal, backend selection by destination, uniform 403s, header validation, unknown and
disabled extensions, configuration and Secret reload, request/response caps, streaming abort,
timeouts, unreachable backends, redirects not followed, CSRF for cookie sessions, WebSocket
refused by default and proxied when enabled, metrics); Go client request shapes.
