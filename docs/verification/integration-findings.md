# Integration findings

Requests and observations raised while integrating components, addressed to the owners of
the paths named. Each entry: date, source workstream, target path, request, why.

## 2026-10-16 — server wiring (`internal/server`, `cmd/patrolcd`)

| # | Target | Request | Why |
|---|--------|---------|-----|
| W-1 | `internal/controller` (`run.go`, `operation/reconciler.go`) | Offer hooks so the binary does not re-assemble the controller: e.g. `RunConfig.OwnsCluster`, a `RunConfig.Setup func(ctx, *Components) error` called before `mgr.Start` (manager, app reconciler, registry, live cache, server-side diff), and `(*operation.Reconciler).SetupWithManager(mgr, extra ...source.Source)`. | `internal/server.RunController` duplicates `controller.Run` (sharding, tree API and hydrator need the manager and the reconciler) and re-registers the Operation controller with the same watches plus a requeue channel. Any change to `controller.Run` or `SetupWithManager` must be mirrored there until the hooks exist. |
| W-2 | `internal/controller/app` | Expose the comparison state needed by the tree API (`State` exists) and consider a `RequeueAll` that also reaches controllers sharing the Application (hydrator). | After a shard acquisition the hydrator's app controller is not requeued (`hydrator.Setup` returns no requeue hook); it relies on later Application events. |
| W-3 | `internal/hydrator` | A method to requeue every accepted Application (shard acquisition). | See W-2. |
| W-4 | `internal/apiserver` (`server.go`) | `Config.Middleware func(http.Handler) http.Handler` (outermost wrapper) or equivalent. | Server-side OpenTelemetry spans for patrol-server (OPS-085) and gzip (OPS-110) cannot be added from the wiring because `Serve` uses the internal handler. |
| W-5 | `internal/apiserver` | `/healthz?full=true` (upstream lists clusters, OPS-011). | Liveness probe parity. |
| W-6 | `internal/reposerver` (`run.go`) | A handler wrapper hook (tracing) and pprof on the probe listener (OPS-056), or a `Serve` variant that takes the probe handler, so `--healthz-addr`/`--enable-pprof` work for the repo-server too. | The probe/metrics listener and the API server are built inside `serve`. |
| W-7 | `go.mod` | Promote `k8s.io/klog/v2` to a direct dependency (it is already in the graph) so `patrolcd` can route klog through slog (`klog.SetSlogLogger`) and offer `--gloglevel` (OPS-088). | client-go leader election and other client-go messages are printed in klog's own text format next to the JSON logs. The wiring task may not modify `go.mod`. |
| W-8 | `internal/apiserver/services/extensions` | Decide whether static UI extension assets should get a dedicated route. | The wiring serves `GET /extensions/<path>` from the extensions directory when the file exists and the request carries no `Patrol-Application-Name` header (proxy calls always do), falling back to the proxy handler; assets need an authenticated session like the proxy route. |
| W-9 | deploy/manifests owner | Mount `patrol-internal-token` (key `token`) at `/app/config/internal-token/token`; `patrol-repo-server-tls` at `/app/config/reposerver/tls`; mTLS client pairs at `/app/config/reposerver/mtls/client.{crt,key}`; trust ConfigMaps at `/app/config/ssh/ssh_known_hosts`, `/app/config/tls`, `/app/config/gpg/source` (repo-server and commit-server); `<component>.profile.enabled` at `/home/patrol/params/profiler.enabled`; project `patrol-cmd-params-cm` into environment variables for repo-server and commit-server; headless Service `patrol-controller` port 8090; RBAC: server create/update `patrol-secret`, create AppProjects, update `appprojects/status`, get Leases; controller Leases get/list/watch/create/update, Cluster status update; appset/notifications Leases. | See `docs/architecture/components/wiring.md` (Deployment requirements). |
| W-10 | `internal/cli` (`--core` mode) | `internal/server.Run(ctx, server.Config)` is the in-process entry point; if the CLI needs only an `http.Handler`, ask for an exported builder (`serverAssembly` is unexported today). | Core mode needs the assembled server without listeners. |
| W-11 | `docs/parity/matrix.md` | Regenerate with `go run ./hack/parity` after merging `docs/parity/status/wiring.yaml`. | The wiring task does not write the shared matrix. |
| W-12 | `internal/apiserver` (`tls.go`, `buildTLSConfig`) | Make `TLSOptions` safe for every caller: ignore `Ciphers` when the minimum version is 1.3 (upstream), and either add `TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256` / `TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256` or drop `h2` from `NextProtos` when a TLS 1.2 list has neither. Consider an RSA-2048 self-signed certificate (upstream) or document that the generated one is ECDSA. | With `NextProtos` `h2` and an explicit list lacking those suites (the upstream default `TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384`), `http.Server.Serve` returns `http2: TLSConfig.CipherSuites is missing an HTTP/2-required AES_128_GCM_SHA256 cipher`. The wiring now normalizes the list before passing it (`internal/server.apiCipherSuites`), so `patrolcd server` starts; other users of `apiserver.Config` still hit it. With an RSA-only list the ECDSA self-signed certificate cannot serve TLS 1.2 clients. |
| W-13 | `internal/apiserver/core` (`sso.go`, `DexTransport`) | Accept the pinned Dex leaf (DER) in strict mode, e.g. `DexTransport(strict bool, roots *x509.CertPool, pinned []byte)`, with upstream's `dex server certificate does not match`. | Upstream strict mode verifies the chain and requires the leaf to equal `/app/config/dex/tls/tls.crt` (OPS-102). `DexTransport` cannot pin, so the wiring builds its own strict transport (`internal/server.dexStrictTransport`); one implementation should own this. |
| W-14 | `internal/apiserver` (`static.go`, `UIHandler.serveFile`; `OverlayFS`) | Answer like upstream's `http.FileServer`: 404 only for `fs.ErrNotExist`, 403 for `fs.ErrPermission`, 500 for other open errors, instead of treating every error as "not a file" (404, or the index page for HTML requests). `OverlayFS` should not hide a non-`ErrNotExist` error of `Upper`. | OPS-111's negative test expects 500 for a symlink escaping `--staticassets`. The wiring now confines the directory with `os.OpenRoot` and returns the escape error (not `ErrNotExist`), so no content is disclosed, but the status is 404. |
| W-15 | `internal/reposerver` (`run.go`, `selfSignedCertificate`) and deploy owner | Observation, inferred from code and not run: the repo-server's generated certificate is ECDSA P-256 while `reposerver.tls.ciphers` defaults to the RSA-only upstream list, so a TLS 1.2-only client cannot connect to a repo-server without a mounted pair (TLS 1.3 clients, including every Patrol client, are unaffected). Upstream generates RSA. | Same cause as W-12 on another listener (no HTTP/2 there, so no startup failure). |

## UI → API requests

Raised 2026-10-16 while switching the web UI (`ui/`) from its provisional route/schema tables to
the published `api/openapi/openapi.json` (workstream: UI ↔ API contract reconciliation). Target
for all: `internal/apiserver/openapi` and the route/request types in `internal/apiserver`,
`internal/apiserver/services/*`, `pkg/apiclient`. The UI works with the document as it is; where
it compensates, the compensation is named and points back here.

| # | Operations / schemas | Request | Why (UI impact today) |
|---|----------------------|---------|-----------------------|
| UI-1 | CRD roots in responses: `Application`, `AppProject`, `ApplicationSet`, `Operation`, `Repository`, `RepositoryCredential`, `Cluster`, `SCMProvider` (and their `*List.items`, `ApplicationOperationResponse`, `ProjectDetailed`, `ApplicationSetGenerateResponse.applications`, `WatchEvent_*.object`) | Mark `metadata` required where these schemas describe stored objects returned by the server (e.g. response-side wrappers with `required: ["metadata"]`, as `RepositoryInfo`/`RegisteredCluster` already do). | The CRD `openAPIV3Schema` leaves `metadata` optional because it also describes request bodies, so every `app.metadata.name` is `possibly undefined` in generated types. The UI narrows read types with `Stored<T>` in `ui/src/api/types.ts` (request bodies keep the generated `*Input` shapes). |
| UI-2 | `getApplicationLogs`, `getPodLogs` | Declare the streaming response: `200` `text/event-stream` with each `data` field a `LogEntry`, the final entry `last: true`, `: heartbeat` comments, and the `event: error` problem document — next to the existing `application/octet-stream` (download=true). | Only the download variant is documented, so generated types cannot type the stream and the UI's contract test validates mock log events against `components.schemas.LogEntry` by name. |
| UI-3 | `invokeExtensionPost`, `invokeExtensionPut`, `invokeExtensionPatch` (and `getApplicationManifestsWithFiles`) | Declare their request bodies: an opaque pass-through body (`content: {"*/*": {}}`) for the extension proxy, and `application/gzip` (binary) for manifests-with-files. | The document declares no `requestBody`, so a generated client has no way to send the body these routes forward or consume; the UI contract test exempts the proxy bodies explicitly. |
| UI-4 | `WatchEvent_Application.type`, `WatchEvent_ApplicationTree.type`, `ApplicationSetWatchEvent.type` | Constrain `type` to `enum: ["ADDED", "MODIFIED", "DELETED"]` (`apiclient.EventAdded/EventModified/EventDeleted`). | Generated types say `string`; the UI keeps its own `WatchEventType` union in `ui/src/api/types.ts`. |
| UI-5 | `TerminalMessage` (`openTerminal` frames) | Constrain `operation` to `enum: ["stdin", "stdout", "resize"]` and make `data` optional (it is ignored for `resize`). | `data` is required by the schema, so the UI sends `data: ""` with resize frames to stay valid. |
| UI-6 | Hidden SCM webhook routes `/api/webhook[/{provider}]` | Publish the accepted receiver names (`github`, `gitlab`, `gitea`, `bitbucket`, `bitbucketserver`, `azuredevops`, `gogs`, `ghcr`, aliases) — in the document, in `Settings`, or as a mapping from `SCMProvider.spec.type`. | The SCM provider details page shows the webhook URL to register; the UI hard-codes the type → receiver mapping in `ui/src/pages/settings/scmproviders/model.ts` (`webhookProviderName`). Before this change it wrongly used the SCMProvider object name. |

### Resolution of UI-1 … UI-6 (2026-10-16, `internal/apiserver`, `pkg/apiclient`)

All six are resolved in the generated document (`go run ./internal/apiserver/cmd/genopenapi`;
`pnpm -C ui gen:api` regenerated `ui/src/api/schema.d.ts`). Commands and results:
`docs/architecture/components/apiserver.md` (Verification). The UI compensations named above
still compile and are now optional; dropping them is left to the UI workstream.

| # | Status | What was done | UI follow-up (optional) |
|---|--------|---------------|-------------------------|
| UI-1 | Resolved | The generator adds `Stored<Kind>` for every CRD kind (`allOf: [<Kind>]`, `required: [metadata]`; e.g. `StoredApplication`, `StoredOperation`). Every response body, stream event, type reflected for a response (`ApplicationOperationResponse`, `ProjectDetailed`, `ApplicationSetGenerateResponse`, `ProjectGlobalProjectsResponse`, `WatchEvent_Application`, `ApplicationSetWatchEvent`) and every `<Kind>List.items` references it; request bodies keep `<Kind>` (metadata optional). Go types and CRD schemas are unchanged. openapi-typescript renders `WithRequired<components["schemas"]["Application"], "metadata">`. | Replace `Stored<Schemas['X']>` with `Schemas['StoredX']` in `ui/src/api/types.ts`. The UI test validator (`ui/src/test/openapi.ts`) now evaluates `allOf`. |
| UI-2 | Resolved | `getApplicationLogs` and `getPodLogs` document `200` with both `text/event-stream` (schema `LogEntry`) and `application/octet-stream` (download): a `ResponseDoc` with the success status is now merged into the stream response instead of replacing it. Every stream's `text/event-stream` media type has `x-patrol-events`: `message` (the data schema) and `error` (`Problem`); the description names the heartbeat comments, the route description the final `last: true` entry. | `contract.test.ts` validates mock log events against the documented `text/event-stream` schema (done). |
| UI-3 | Resolved | `Route.RequestContentType` is now documented for routes whose request type has no body fields (it was dropped before): `getApplicationManifestsWithFiles` has a required `application/gzip` body (media type without a schema, i.e. binary), `invokeExtensionPost/Put/Patch/Delete` an optional `*/*` body (the proxy forwards the body of every method); GET stays without one. New `RequestDescription` / `RequestBodyOptional` route fields. | `OPAQUE_BODY` in `contract.test.ts` can check the declared body instead of exempting it. |
| UI-4 | Resolved | `WatchEvent[T].type` and `ApplicationSetWatchEvent.type` carry `enum: [ADDED, MODIFIED, DELETED]` (`jsonschema` tags, tested against `apiclient.EventAdded/Modified/Deleted`). The server emits no `BOOKMARK`/`ERROR` watch events: API server bookmarks are consumed by the ApplicationSet watch, and failures end the stream with an `event: error` problem document (`rest.ServeSSE`), so they are not in the enum. | Use the generated union instead of `WatchEventType`. |
| UI-5 | Resolved | `TerminalMessage.operation` has `enum: [stdin, stdout, resize]` (the operations `terminal.go` handles; there is no toast operation; any other value ends the session) and `data` is optional (`json:"data,omitempty"`; the server ignores it for resize and the Go client's `Resize` now omits it). | Resize frames may drop `data: ""`. |
| UI-6 | Resolved | New documentation-only service `internal/apiserver/services/webhooks` (listed in `DocumentedServices`; `Route.DocumentationOnly` routes are never mounted, so routing is unchanged; the runtime routes stay in `internal/server`): `POST /api/webhook` (`receiveWebhook`) and `POST /api/webhook/{provider}` (`receiveProviderWebhook`) with the `provider` enum (`azuredevops`, `gitea`, `gogs`, `github`, `gitlab`, `bitbucket`, `bitbucketserver`, `ghcr`, aliases `azure-devops`, `bitbucketcloud`, `bitbucket-cloud`, `bitbucket-server`; case-insensitive), plain-text error responses, `security: []`, and `x-patrol-webhook-receivers` (per receiver: kind `scm`/`registry`, required and excluded headers, authentication header, patrol-secret keys, `scmProviderTypes`). Tests check names, aliases and headers against the real `webhook.Handler`. | `webhookProviderName` can read the mapping from `x-patrol-webhook-receivers` (`kind: scm`, `scmProviderTypes`); the two new operations are listed in `unusedOperations` (done). |

## 2026-10-16 — integration harness (`test/integration`)

Source workstream: in-process integrated stack plus scenario files 1 (core lifecycle,
`lifecycle_test.go`) and 2 (recovery, `recovery_test.go`). The harness
(`test/integration/stack`) runs patrol-server (with `/api/webhook`), the repo-server, the
application controller (tree API), the ApplicationSet controller and the notifications
controller through their library entry points (`internal/server.Run`, `RunRepoServer`,
`RunController`, `RunAppSetController`, `RunNotificationsController`) against real
kube-apiservers (envtest 1.36.0): a control cluster (CRDs and the Operation admission policy;
also the destination `in-cluster` through `PATROL_FAKE_IN_CLUSTER` and a kubeconfig set for the
test process only), a shared destination `dest-1` registered with a client certificate from
envtest `AddUser` (`system:masters`), and optional dedicated destinations registered with a
ServiceAccount token from the TokenRequest API whose kube-apiserver can be stopped and started
again (fixed certificate directory and port; killed after 5 s because open watches delay its
drain up to a minute). Sources are real git repositories (`git http-backend` over HTTP, basic
auth) rendered by the repo-server with the real git/helm/kustomize binaries; tests drive the
public API (`pkg/apiclient`: local accounts, RBAC, account and project tokens, signed GitHub push
webhooks) and the CLI in-process (`internal/cli.Execute`). There is no kubelet,
controller-manager or garbage collector: Job and Deployment health is simulated by writing their
status and a fake garbage collector releases foreground deletions. Unexpected ERROR records of
any component fail the test (allow-list per Stack).

Run (tests skip unless `PATROL_INTEGRATION=1` or `KUBEBUILDER_ASSETS` is set):

```bash
PATROL_INTEGRATION=1 go test -count=1 -timeout 60m ./test/integration/...
PATROL_INTEGRATION=1 go test -race -count=1 -timeout 90m ./test/integration/...
```

### Defects found and fixed

| # | Location | Symptom | Fix | Regression tests |
|---|----------|---------|-----|------------------|
| I-1 | `internal/server/controller.go:505` (cluster subscription of `RunController`); helper `clusterCacheRefreshRequested` at `controller.go:621` | `POST /api/v1/clusters/{server}/invalidate-cache` sets the Cluster's `cd.patrol.example/refresh` annotation (upstream `refreshRequestedAt`, as documented in `api/v1alpha1/keys.go`), but the controller ignored metadata-only Cluster events: the destination's live-state cache was never dropped. `TestCacheLossMidOperation` counted 2 GET requests to the destination in the 60 s after the request (a rebuild relists every API). | A Modified event whose refresh annotation was added or changed invalidates the destination's cache (`cache.Manager.Invalidate`, `LiveStateCache.Forget`), rebuilds it at once (upstream warms it up) and requests a refresh of the Applications. The state delivered at startup (`EventAdded`) never invalidates, so a request served before a restart is not repeated (upstream compares with `lastCacheSyncTime`, which Patrol does not persist: O-1). | `internal/server/controller_refresh_test.go` `TestClusterCacheRefreshRequested` (8 cases); integration `TestCacheLossMidOperation` (failed on the previous code). |
| I-2 | `internal/apiserver/core/account.go:203` (`updatePassword`), `:268` (`createAccountToken`), `:304` (`deleteAccountToken`); helper `waitAccount` at `:315` | The token service and the login path read accounts from the settings snapshot, which follows `patrol-secret` through an informer and a 200 ms debounce. A token returned by `POST /api/v1/account/{name}/token` was rejected with `invalid session: token revoked` (401) on first use (accepted 207 ms later in the run that found it); a deleted token kept authenticating after `DELETE` returned; a login with a just-changed password failed with `Invalid username or password` while sessions issued before the change kept verifying. Scripts that run `patrol account generate-token` and use the token at once fail. Project role tokens were not affected. | The three handlers wait (at most 5 s) until the snapshot reflects their write (token listed / no longer listed, password hash changed) before returning; on timeout they return and log a warning. | `internal/apiserver/core/account_test.go` `TestAccountTokenVisibleWhenReturned`, `TestUpdatePasswordInEffectWhenReturned` (snapshot lagging 300 ms behind the secret store through the new `envOptions.secretLag`; both failed on the previous code with `invalid token: token revoked` / `Invalid username or password`); integration `TestStackStarts` (account token, project token and changed password used immediately). |

### Observations and requests (not fixed here)

| # | Target | Observation / request | Evidence |
|---|--------|-----------------------|----------|
| O-1 | `internal/server` (`RunController`) / `internal/controller` | The application controller never writes `Cluster.status` (connection state, server version, cache info and `lastCacheSyncTime`, applications count): `clusters.ComputeClusterInfo` and `StatusWriter.RecordClusterInfo` have no caller outside `internal/clusters` (upstream refreshes cluster info every 10 s). With a destination down only the Applications' `ComparisonError` reports it; a Cluster registered without the API keeps an empty status. Request: a periodic per-shard cluster info writer; it would also let I-1 compare with the last sync time like upstream. | Code search; `TestDestinationUnreachable` logs `connectionState=<nil> serverVersion="" lastCacheSyncTime=<nil>` for the stopped destination. |
| O-2 | operator docs (`internal/cache`) | After a failed sync of a destination's live-state cache, comparisons within `SyncRetryTimeout` (10 s) do not retry it, so a refresh right after the destination recovered still shows `ComparisonError`; the next comparison after the window recovers (periodic or user refresh). Same as upstream; worth a runbook line. | `TestDestinationUnreachable` refreshes until Synced. |
| O-3 | `internal/controller/app` | A refresh requested while the destination is unreachable is consumed by a comparison that fails before revision resolution; the next periodic comparison (level latest) resolves the branch through the repo-server's revision cache (3 min) and reported `Synced` at the previous commit two minutes after recovery although the branch had moved. Upstream behaves the same; consider keeping the refresh annotation (or the force-resolve level) when the comparison failed to load the target state. | First version of `TestDestinationUnreachable` (it now refreshes explicitly after recovery). |
| O-4 | `internal/controller/operation` (information) | A step abandoned on a 409 is retried after `PollInterval` (5 s). Real conflicts are followed by the newer object's watch event, so this only shows with injected faults (16 injected conflicts on Operation and Application status writes: a hooked three-wave sync took about 40 s). Correctness held: one hook Job, one history entry, one Operation, consistent summary. | `TestAPIConflictInjection`. |

Behavior checked against the baseline while writing the scenarios (no defect): history ids
start at 0 (upstream); app-level `Force=true` is not a sync-wide option (upstream: the resource
annotation `Force=true,Replace=true` or the strategy's force, both exercised); a normal refresh
is force-resolve and bypasses the repo error grace period (upstream `noRevisionCache`); a
cluster version failure counts as a repo error for the grace period (upstream `GetRepoObjs`);
the sync engine's dry run is server-side (upstream client-side) and still lets a forced replace
re-create an object with immutable fields.

### Verification (2026-10-16, macOS arm64, Go 1.27.2, envtest kube-apiserver 1.36.0, host load average 200–250 from concurrent workstreams)

| Command | Result |
|---------|--------|
| `go build ./...` | exit 0 |
| `go vet ./test/integration/... ./internal/server/ ./internal/apiserver/core/` | exit 0 |
| `golangci-lint run ./test/integration/... ./internal/server/ ./internal/apiserver/core/` | 0 issues |
| `go test -race -count=1 ./internal/apiserver/core/` | ok (incl. the two I-2 regression tests) |
| `KUBEBUILDER_ASSETS=$PWD/bin/envtest/k8s/1.36.0-darwin-arm64 go test -race -count=1 ./internal/server/ ./internal/apiserver/` | ok, ok (incl. the I-1 regression test) |
| I-1/I-2 regression tests against the previous code (fix disabled temporarily) | `TestCacheLossMidOperation`, `TestAccountTokenVisibleWhenReturned`, `TestUpdatePasswordInEffectWhenReturned` fail as described above |
| `PATROL_INTEGRATION=1 go test -count=1 -timeout 60m -v ./test/integration/` | ok, 325 s, 16/16 tests passed, none skipped |
| `PATROL_INTEGRATION=1 go test -race -count=1 -timeout 90m -v ./test/integration/` | ok, 648 s, 16/16 passed, no data race reported |

An earlier race run failed `TestAPIConflictInjection` at an immediate history assertion: the
injected conflicts (then unbounded, alternating across concurrent writers) delayed the
completion's Application write past the test's check. The test now bounds the injection budget
and waits for the history entry, then checks that it stays single; three further runs (two with
`-race`) passed. Simulated parts: workload health (Job/Deployment status written by the test),
garbage collection (foreground finalizers released by the harness); everything else runs the
real components.
