# Component: `internal/server` and `cmd/patrolcd` (process wiring)

Status: implemented. The multi-call binary runs every control-plane component; each
subcommand has a library entry point that the command and the integration tests share.
Decision records: [ADR-0004](../adr/0004-no-redis.md), [ADR-0005](../adr/0005-public-api.md),
[ADR-0006](../adr/0006-internal-apis.md), [ADR-0007](../adr/0007-rendering-isolation.md),
[ADR-0008](../adr/0008-sharding-and-ha.md), [ADR-0009](../adr/0009-authn-authz.md). Command
reference: [`docs/reference/cli-patrolcd.md`](../../reference/cli-patrolcd.md). Parity
overlay: `docs/parity/status/wiring.yaml`.

| Path | Contents |
|------|----------|
| `cmd/patrolcd/main.go` | `server.DispatchHelpers()` first, then `server.Main` (command tree) |
| `internal/server/helpers.go` | Hidden helper modes, in dispatch order |
| `internal/server/commands.go` | Cobra tree, binary-name dispatch, flag → configuration mapping per component |
| `internal/server/params.go` | `ParamFlags`: patrol-cmd-params-cm parameters as flags, precedence resolution |
| `internal/server/runtime.go`, `logging.go` | Common flags, bootstrap (logger, kubeconfig, namespace, `patrol-cmd-params-cm`, client tuning) |
| `internal/server/server*.go` | `Config`, `Validate`, `Run`: the patrol-server dependency graph; extra routes (`/extensions.js`, `/extensions/…` assets, `/download/…`, `/api/webhook`) |
| `internal/server/controller.go`, `treestate.go` | `ControllerConfig`, `RunController`; the tree API state provider |
| `internal/server/appset.go`, `notifications.go`, `components.go` | `RunAppSetController`, `RunNotificationsController`, `RunRepoServer`, `RunCommitServer`, `RunCMPServer` |
| `internal/server/observe.go`, `group.go`, `tlsutil.go`, `token.go`, `clients.go`, `kube.go` | Metrics/probe/profiler listener, tracing, task group with bounded drain, TLS, internal token, internal clients |
| `internal/server/core.go`, `core_support.go` | `NewCoreHandler`: the patrol-server API assembled inside the CLI for `patrol --core` (no listener, caller's credentials); see [cli-core-mode.md](cli-core-mode.md) |
| `internal/ui` | `//go:embed all:dist` of the UI build copied by `make ui-build`, and the "UI not built" page |
| `hack/build.mk` | `ui-build`, `ui-clean`, `build-bin`, `build-all` (included by the root `Makefile`) |

## Binary

`patrolcd <component> [flags]` with the components `server`, `controller`, `repo-server`,
`appset-controller`, `notifications-controller`, `cmp-server`, `commit-server`, plus
`k8s-auth aws|gcp|azure` (ExecCredential plugin, `internal/clusters/k8sauth`) and `version
[--short] [-o json|wide]`. The image may link the binary under component names (OPS-145):
`patrol-server`, `patrol-controller`/`patrol-application-controller`, `patrol-repo-server`,
`patrol-applicationset-controller`/`patrol-appset-controller`,
`patrol-notifications`/`patrol-notifications-controller`, `patrol-cmp-server`,
`patrol-commit-server`, `patrol-k8s-auth`; `PATROL_BINARY_NAME` selects the same way. Any
other name runs the command tree as is.

Hidden helper modes are recognized before any argument parsing (`server.DispatchHelpers`,
must stay the first statement of `main` and of `TestMain` in packages whose tests re-execute
the test binary). Order matters: the sandbox helper execs its target in place, and the
target may be this binary again in another mode.

| Order | Mode | Trigger | Library |
|-------|------|---------|---------|
| 1 | rlimit helper | `os.Args[1] == "__patrol-sandbox-exec"` | `sandbox.MaybeHelperMain` |
| 2 | git askpass | `PATROL_GIT_ASKPASS_FILE` set and exactly one argument (the prompt) | `git.MaybeAskpassMain` |
| 3 | Jsonnet evaluator | `os.Args[1] == "jsonnet-eval"` | `jsonnet.MaybeJsonnetMain` |

`TestEveryHelperMainIsDispatched` parses every non-test Go file of `internal/` and `pkg/`
and fails when a `MaybeXxxMain` function is not in `server.Helpers()`;
`cmd/patrolcd:TestHelperModesAreDispatched` runs the built binary in each mode.

## Configuration precedence

For every `patrol-cmd-params-cm` key of a component (`settings.ParamsForComponent`):

```
command-line flag  >  environment variable (PATROL_*)  >  patrol-cmd-params-cm  >  compiled default
```

- `ParamFlags` registers one flag per key that has a flag name in the register (type-aware:
  booleans accept `--flag`, lists are comma-separated, maps `k=v,...`, quantities `10M`,
  durations `90s`/`2d`). Keys without a flag name are read from the environment and the
  ConfigMap only. Flags the command defines itself win a name collision.
- Resolution reuses `settings.ResolveParams`: a set flag is presented as the key's
  environment variable (or ConfigMap entry for keys without one), so every source goes
  through the same parser. An invalid flag value is a startup error; an invalid environment
  or ConfigMap value falls back to the default with upstream's warning, except quantities
  and enumerations, which are fatal (upstream). An empty environment variable selects the
  default (upstream projection semantics).
- Components with Kubernetes access (server, controller, appset-controller,
  notifications-controller) read `patrol-cmd-params-cm` from the control-plane namespace at
  startup (15 s bound): NotFound → not used; Forbidden → warning, not used; other errors
  are fatal. Parameters are resolved twice: first from flags and environment (so the
  startup logger and the kubeconfig are configured), then with the ConfigMap. The
  repo-server, commit-server and cmp-server have no Kubernetes access (no service account
  token); they read the ConfigMap only through the environment variables the install
  manifests project it into (upstream behavior).
- Libraries that read `PATROL_*` variables themselves (Kubernetes client tuning
  `PATROL_K8S_*`, sharding `PATROL_CONTROLLER_*`) receive a lookup that answers with the
  resolved, explicitly configured parameter values first, then the process environment.
- Changing `patrol-cmd-params-cm` needs a restart, as upstream; `patrol-cm` and the other
  settings objects are followed live by `settings.Manager`.

## Common flags

| Flag | Components | Meaning |
|------|------------|---------|
| `--kubeconfig`, `--context`, `-n/--namespace` | server, controller, appset, notifications | Control-plane cluster; without a kubeconfig the in-cluster configuration. Namespace: flag, else `POD_NAMESPACE`/service account namespace, else the kubeconfig context namespace (client-go semantics) |
| `--loglevel`, `--logformat` | all | From the component's `<prefix>.log.level` / `<prefix>.log.format` parameters (cmp-server: its own options). Level: logrus names in any case (`warning`→`warn`, `trace`→`debug`). Format: `json` or `text`; when configured nowhere, JSON in a pod (`KUBERNETES_SERVICE_HOST` set) and text elsewhere. `log.format.timestamp` sets a Go time layout. `applicationsetcontroller.debug` forces `debug` |
| `--metrics-addr` | all but k8s-auth | Metrics, probes and profiler listener; `-` disables. Default per component (table below); for server, repo-server and commit-server it is `--metrics-address`:`--metrics-port` unless given |
| `--healthz-addr` | server, controller, appset, notifications, commit-server | Optional separate listener for `/healthz`, `/readyz`, `/livez` (appset default `:8081`, upstream `--probe-addr`) |
| `--enable-pprof` | all with a metrics listener | Serve `/debug/pprof/*` unconditionally. Otherwise each request reads the file named by `PATROL_ENABLE_PROFILER_FILE_PATH` (default `/home/patrol/params/profiler.enabled`, the projection of `<component>.profile.enabled`) and serves only when it contains `true`; else 401 `Profiler endpoint is not enabled in 'patrol-cmd-params-cm' ConfigMap` (OPS-056) |
| `--tls-cert`, `--tls-key` | server (public listener), controller (tree API), repo-server, commit-server | Serving pair, re-read when the files change (mounted Secret rotation without restart) |
| `--tls-client-ca` | controller (tree API), repo-server, commit-server | Requires and verifies client certificates (mTLS); a missing file disables mTLS (upstream). Repo-server: overrides `reposerver.client.ca.path` |
| `--internal-token-file` | all but cmp-server and k8s-auth | Shared bearer token of internal APIs (ADR-0006). Default `$PATROL_INTERNAL_TOKEN_FILE` or `/app/config/internal-token/token` (Secret `patrol-internal-token`, key `token`). When the default file does not exist and the component has Kubernetes access, the Secret is read directly (development) |
| `--shutdown-timeout` | all | Bound of the graceful drain after SIGTERM/SIGINT (default 30 s) |

## Environment variables

Every `patrol-cmd-params-cm` key has the environment variable named in its flag's help
(`settings.ParamsForComponent`; upstream `ARGOCD_*` renamed `PATROL_*`). Variables read
besides those:

| Variable | Read by | Meaning |
|----------|---------|---------|
| `PATROL_BINARY_NAME` | all | Selects the subcommand like the executable name |
| `PATROL_INTERNAL_TOKEN_FILE` | all but cmp-server | Default of `--internal-token-file` |
| `PATROL_ENABLE_PROFILER_FILE_PATH` | all with a metrics listener | Profiler toggle file (default `/home/patrol/params/profiler.enabled`) |
| `KUBECONFIG`, `POD_NAMESPACE` | Kubernetes-facing components | client-go configuration and namespace defaulting |
| `KUBERNETES_SERVICE_HOST`, `KUBERNETES_SERVICE_PORT` | all | In a pod: in-cluster configuration and JSON as default log format |
| `PATROL_K8S_CLIENT_QPS`, `_BURST`, `_MAX_IDLE_CONNECTIONS`, `PATROL_K8S_TCP_TIMEOUT`, `_TCP_KEEPALIVE`, `_TLS_HANDSHAKE_TIMEOUT`, `_TCP_IDLE_TIMEOUT`, `PATROL_K8S_SERVER_SIDE_TIMEOUT`, `PATROL_K8SCLIENT_RETRY_MAX`, `_RETRY_BASE_BACKOFF` | server, controller | Destination cluster clients (`clusters.TuningFromLookup`; the parameter values win) |
| `PATROL_FAKE_IN_CLUSTER` | server, controller | Development: serve the in-cluster destination from the local kubeconfig |
| `PATROL_CONTROLLER_REPLICAS`, `PATROL_CONTROLLER_SHARD`, `PATROL_ENABLE_DYNAMIC_CLUSTER_DISTRIBUTION`, `PATROL_CONTROLLER_HEARTBEAT_TIME`, `PATROL_APPLICATION_CONTROLLER_NAME` | controller | Sharding (`sharding.ConfigFromEnv`) |
| `PATROL_GPG_ENABLED` | server, controller, repo-server | `false` disables GnuPG source verification criteria |
| `PATROL_SESSION_FAILURE_MAX_FAIL_COUNT`, `PATROL_SESSION_FAILURE_WINDOW_SECONDS`, `PATROL_SESSION_MAX_CACHE_SIZE`, `PATROL_MAX_CONCURRENT_LOGIN_REQUESTS_COUNT` | server | Login throttling (`auth.LoginConfigFromEnv`) |
| `PATROL_APPLICATIONSET_CONTROLLER_NAMESPACES` | server | ApplicationSet namespaces served by the API (the appset controller reads `applicationsetcontroller.namespaces`) |
| `PATROL_EXEC_TIMEOUT`, `PATROL_EXEC_FATAL_TIMEOUT`, `PATROL_GIT_ATTEMPTS_COUNT`, `PATROL_GIT_RETRY_DURATION`, `PATROL_GIT_RETRY_MAX_DURATION`, `PATROL_PAUSE_GEN_AFTER_FAILED_ATTEMPTS`, `PATROL_PAUSE_GEN_MINUTES`, `PATROL_PAUSE_GEN_REQUESTS` | repo-server | Tool timeouts, git retries, manifest generation error caching (invalid values ignored with a warning) |
| `PATROL_CMP_*`, `PATROL_PLUGINSOCKFILEPATH`, `PATROL_EXEC_TIMEOUT` (and the `ARGOCD_*` names) | cmp-server | `cmp.OptionsFromEnv` |
| `PATROL_GIT_ASKPASS_FILE`, `PATROL_GIT_ASKPASS_ORIGIN` | helper mode | Set by `internal/git` for the askpass child; never set by operators |

## Ports

| Component | Listener | Default | Flags |
|-----------|----------|---------|-------|
| server | API, UI, webhooks (HTTPS unless `--insecure`; plain HTTP answered with 307) | `0.0.0.0:8080` | `--address`, `--port` |
| server | `/metrics`, probes, profiler | `0.0.0.0:8083` | `--metrics-address`, `--metrics-port`, `--metrics-addr` |
| controller | `/metrics`, probes, profiler | `:8082` | `--metrics-port`, `--metrics-addr` |
| controller | tree API (`/internal/v1/*`, `/healthz`) | `:8090` | `--tree-api-addr` (TLS with `--tls-cert`) |
| repo-server | API (TLS unless `--disable-tls`) | `0.0.0.0:8081` | `--address`, `--port` |
| repo-server | `/healthz[?full=true]`, `/readyz`, `/metrics` | `0.0.0.0:8084` | `--metrics-address`, `--metrics-port`, `--metrics-addr` |
| appset-controller | `/metrics`, probes, profiler | `:8080` | `--metrics-addr` |
| appset-controller | probes | `:8081` | `--healthz-addr` |
| appset-controller | SCM webhooks | disabled | `--webhook-addr` (upstream `:7000`) |
| notifications-controller | `/metrics`, probes, profiler | `:9001` | `--metrics-addr` |
| commit-server | API (TLS with `--tls-cert`) | `0.0.0.0:8086` | `--address`, `--port` |
| commit-server | `/metrics`, probes, profiler | `0.0.0.0:8087` | `--metrics-address`, `--metrics-port`, `--metrics-addr` |
| cmp-server | unix socket `<socket-dir>/<plugin>.sock` | `/home/patrol/cmp-server/plugins` | `--socket-dir`; metrics disabled (`--metrics-addr`) |

Probe semantics: `/healthz` and `/readyz` evaluate readiness (503 `shutting down` once a
drain started, 503 `not ready` while a check fails); `/livez` only reports that the process
serves HTTP. patrol-server also answers `/healthz` on the API port (also at the root under
`--rootpath`, apiserver).

| Component | Readiness |
|-----------|-----------|
| server | settings loaded, informer caches synced, repository lister and cluster registry synced |
| controller | manager caches synced, `LeaseManager.Healthz` (replica count read, Lease API healthy, preferred shard acquired), settings loaded |
| appset-controller | repository lister synced (the manager runs leader-elected) |
| notifications-controller | repository lister synced when the repo-server client is configured |
| repo-server | the repo-server's own probe listener (`?full=true` dials the API listener) |
| commit-server, cmp-server | always ready while serving |

## Dependency graphs

### patrol-server (`server.Run`)

```
server.Run(Config) ── Validate
 ├─ clients: clientset, dynamic, controller-runtime client (uncached, with watch)
 ├─ informer cache: Applications (application namespaces), ApplicationSets and SCMProviders
 │    (ApplicationSet namespaces), AppProjects / Clusters / Repositories (control plane);
 │    every other read (Secrets, ConfigMaps, Events) goes to the API server (splitReader)
 ├─ settings.EnsureServerSecretKey ─► settings.Manager (patrol-cm, patrol-rbac-cm, patrol-secret, …)
 ├─ rbac.Enforcer ◄─ patrol-rbac-cm (apiserver.applySettings) and AppProject roles (informer)
 ├─ auth: revocation.SecretStore, TokenService(signing keys from patrol-secret,
 │    AccountLookup, ProjectTokenRegistry, SSO external verifier), Authenticator(PATROL_SESSION_*),
 │    core.SSO (OIDC; /api/dex proxy to --dex-server when dex.config is set)
 ├─ repo-server apiclient.HTTPClient (token, TLS/mTLS, trace propagation)
 ├─ repository.Lister + Resolver; clusters.Registry (cluster.inClusterEnabled followed live)
 ├─ treeapi.Client: ShardRouter(StatusShardResolver(registry), LeaseHolders) or --controller-tree-api-url
 ├─ services: applications (+ resource routes: tree client, destination configs),
 │    applicationsets (BoundedGenerator over generators.Config), projects, clusters
 │    (Connector, StatusWriter), repositories, notifications, badge, extensions
 ├─ routes: /extensions.js, /extensions/<path> assets, /download/<file>, /api/webhook[/<provider>]
 ├─ webhook.Handler (Matrix evaluation through appset.Evaluator)
 ├─ apiserver.Server (route table, middleware, UI from internal/ui plus --staticassets, TLS)
 └─ observability listener; startup tasks after cache sync: default project, token normalization
```

Startup tasks: `EnsureServerSecretKey` creates `patrol-secret` `server.secretkey` when
missing (OPS-030); once the caches synced, AppProject `default` is created with the
permissive upstream spec when it does not exist (never overwritten, TEN-002) and project
token metadata is normalized (`project.NormalizeJWTTokens`, 3 conflict retries, TEN-034).
Failures of the two cache-time tasks are logged and the server keeps serving (upstream exits
on a normalization failure).

UI: `internal/ui` embeds `internal/ui/dist` (the copy of `ui/dist` made by `make
ui-build`); without a build only `.keep` exists and a built-in page explains how to build
the UI. `--staticassets` (default `/shared/app`) adds the files of a directory (OPS-111),
as upstream does: a name is looked up in the embedded UI first, then in the directory, which
is opened with `os.OpenRoot`. Symbolic links that stay inside the directory work (ConfigMap
volumes link every key through `..data`); a link leading out of it (to `/etc/passwd`, a mounted
token) fails and its target is never served (today the UI handler answers 404, or the index
page for HTML requests; upstream answers 500, see integration finding W-14). A missing
directory logs upstream's warning `Static assets directory "<dir>" does not exist, using only
embedded assets`; any other open error stops the server.

Root path and base href (OPS-105): with `--rootpath` the UI base (`<base href>`, cookie path)
is the root path, as upstream forces it; a `--basehref` that differs (the default `/`
included) logs upstream's warning `--basehref and --rootpath had conflict: basehref: <b>
rootpath: <r>`. Without a root path `--basehref` applies.

TLS of the public listener (OPS-097, unless `--insecure`): `server.tls.minversion` /
`.maxversion`; `server.tls.ciphers` applies to TLS 1.2. The listener offers HTTP/2, and
net/http refuses to serve HTTP/2 with an explicit TLS 1.2 list that has neither
`TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256` nor `TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256`
(RFC 9113 section 9.2.2). The upstream default `TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384` is such
a list, so both suites are appended to any list without one (logged; info for the default,
warning for a configured list). This also lets TLS 1.2 clients use the generated ECDSA
certificate. With minimum version 1.3 the list is ignored, as upstream (warning `TLSv1.3
cipher suites are not configurable, ignoring value of --tlsciphers` when it is not the
default).

Dex (OPS-102): `--dex-server` with `--dex-server-plaintext` uses `http://`; by default
HTTPS without certificate verification (upstream); `--dex-server-strict-tls` verifies Dex's
chain against `--dex-server-ca-cert-path` (default `/app/config/dex/tls/ca.crt`) and pins the
leaf: Dex must present exactly the certificate in `--dex-server-cert-path` (default
`/app/config/dex/tls/tls.crt`), else `dex server certificate does not match`. Both files are
read at start; a missing or invalid file stops the server with a message naming it. The
transport serves the `/api/dex` proxy and server-side OIDC discovery.

Extension routes:

- `GET /extensions.js` (public, no credentials): every regular, non-symlink file named
  `extension*.js` below `--extensions-dir` (default `/tmp/extensions`), in lexical walk
  order, each wrapped in `// source: <relative path>` + `try { … } catch(e) {
  console.error('Extension <name> failed to load:', e); }`; missing directory → empty 200;
  `Content-Type: text/javascript; charset=utf-8`, `Cache-Control: no-cache`; 64 MiB cap.
- `GET /extensions/<path>` without the `Patrol-Application-Name` header serves the file
  `<extensions-dir>/<path>` when it exists (`os.Root`: no `..`, no symbolic link leaving the
  directory, no dot segments; content type from the extension; same authentication as the
  proxy route); every other request (and every request with the header) is a proxy
  extension call (API-046..049).
- `GET /download/<file>` (public): `patrol-<os>-<arch>[.exe]` from `--download-dir`
  (default `/shared/download`), `patrol-linux` = `patrol-linux-<server arch>`; on Linux
  without such a file, `patrol-linux` and `patrol-linux-<arch>` serve the `patrol` binary
  found in `PATH` (upstream, API-014); `Content-Disposition: attachment`.
- `/api/webhook` and `/api/webhook/<provider>` (any method, no Patrol credentials,
  rate-limited and verified by `internal/webhook`; the client IP honors
  `--trusted-proxies`).

### Application controller (`server.RunController`)

```
server.RunController(ControllerConfig) ── Validate
 ├─ controller-runtime manager (cache: Applications/Operations in application namespaces,
 │    AppProjects/Clusters/Repositories in the control plane; no manager-wide leader election)
 ├─ settings.Manager, clusters.Registry, repository.Lister/Resolver, repo-server client
 ├─ cache.Manager ─► compare.LiveStateCache ─► compare.KubeServerSideDiff ─► compare.Manager
 ├─ sharding: Assigner (registry events, Application informer), LeaseManager
 │    (Leases patrol-controller-shard-<n>, StaticReplicas or WorkloadReplicas), Ownership
 │    (core.Deps.OwnsCluster), AssignedShardWriter (Cluster.status.assignedShard)
 ├─ app.Reconciler; operation.Reconciler (+ requeue channel); operation.KubeEngines
 │    (sync engine: wave delay, parallelism from parameters)
 ├─ hydrator.Setup when hydrator.enabled (commit-server client: --commit-server-timeout,
 │    TLS for https commit.server; AppFilter = allowed namespace and owned shard)
 ├─ treeapi.Handler over treeState on --tree-api-addr
 └─ observability listener
```

Sharding wiring (ADR-0008): `sharding.ConfigFromEnv` reads `PATROL_CONTROLLER_REPLICAS`,
`PATROL_CONTROLLER_SHARD`, `PATROL_ENABLE_DYNAMIC_CLUSTER_DISTRIBUTION` (also
`--dynamic-cluster-distribution-enabled`), `PATROL_CONTROLLER_HEARTBEAT_TIME`,
`PATROL_APPLICATION_CONTROLLER_NAME`; the algorithm comes from
`controller.sharding.algorithm` (`--sharding-method`). Every replica runs the Lease manager,
also with one replica (shard 0's Lease gives a standby takeover). Reconcilers skip
Applications and Operations of clusters the replica does not own; on every acquisition,
loss or assignment change the wiring waits for the cache, requests a refresh of every
Application (`RefreshAll`) and pushes every non-terminal Operation into a channel source of
the Operation controller, so work skipped while a shard was not owned is resumed (Running
operations continue on the new owner). Caches and sync engines of clusters no longer owned
are dropped. The Operation controller is registered by the wiring with the same watches as
`(*operation.Reconciler).SetupWithManager` plus that channel.

Tree API state (`treeState`, `treeapi.StateProvider`): the tree and masked managed resources
of the Application reconciler's last comparison (`app.Reconciler.State`); 503 before the
first comparison; 421 with the owning shard when this replica does not own the Application;
server-side diff with the Application's normalization (ignoreDifferences, system rules, known
types), tracking keys, mutation-webhook option and structured merge, through the controller's
dry runner and type parser; live resources read with the controller's destination
configuration and the cluster's discovery data.

Controller parameters mapped: `core.ParamsFrom` (application namespaces, processors,
self-heal, sync timeout), `controller.resource.health.persist`, `controller.diff.server.side`,
`controller.repo.error.grace.period.seconds`, `controller.ignore.normalizer.jq.timeout`,
`controller.sync.wave.delay.seconds`, `controller.kubectl.parallelism.limit` (per-step
parallelism of the sync engine), `controller.cluster.cache.batch.events.processing` /
`.events.processing.interval`, `controller.hydration.processors`, `hydrator.enabled`,
`commit.server`, `controller.k8s.*` (control-plane client QPS/burst; destination clients
through `PATROL_K8S_*`).

Commit-server client (source hydrator): `commit.server` is `host:port` or `http://…` (plain
HTTP, upstream) or `https://…` (TLS: `--commit-server-ca-cert-path` verifies the commit-server
certificate, without it the certificate is not verified and the internal token authenticates,
like the repo-server client; `--commit-server-client-cert-path` /
`--commit-server-client-cert-key-path` present a client certificate to a commit-server started
with `--tls-client-ca`). Every request is bounded by `--commit-server-timeout` (default 10 min,
clone + write + push): hydration runs without a deadline, so the bound is what frees a
hydration worker when the commit-server accepts a request and never answers. Without effect: `controller.metrics.cache.expiration`,
`controller.app.state.cache.expiration`, `controller.default.cache.expiration` (no Redis
caches, ADR-0004).

### Other components

| Command | Entry point | Wiring |
|---------|-------------|--------|
| `repo-server` | `RunRepoServer` → `reposerver.Run` | `reposerver.*` parameters; `PATROL_EXEC_TIMEOUT`, `PATROL_EXEC_FATAL_TIMEOUT`, `PATROL_GIT_ATTEMPTS_COUNT`, `PATROL_GIT_RETRY_DURATION`, `PATROL_GIT_RETRY_MAX_DURATION`, `PATROL_PAUSE_GEN_AFTER_FAILED_ATTEMPTS`, `PATROL_PAUSE_GEN_MINUTES`, `PATROL_PAUSE_GEN_REQUESTS`, `PATROL_GPG_ENABLED`; trust material `--ssh-known-hosts-path`, `--tls-certs-dir`, `--gpg-keys-dir` (ConfigMap mounts, `-` disables), `--plugin-socket-dir`, `--root-dir`, `--credentials-dir`, `--gnupg-home-parent`, `--helm-plugins-dir`, `--revision-cache-expiration`, `--max-working-copies`. TLS: `--tls-cert`/`--tls-key`, else the mounted `patrol-repo-server-tls` pair in `/app/config/reposerver/tls` when present, else self-signed; versions and ciphers from `reposerver.tls.*`; mTLS from `reposerver.client.ca.path` / `--tls-client-ca` |
| `appset-controller` | `RunAppSetController` → `appset.Run` | `applicationsetcontroller.*` parameters (policy override defaults to true when no policy is configured), leader election (`applicationsetcontroller.enable.leader.election`, controller-runtime Lease), Git generator through the repo-server with credentials from `repository.Resolver`, `--scm-proxy-url`, `--scm-no-proxy`, `--concurrent-application-updates`, `--metrics-applicationset-labels`, `--cache-sync-period`, optional `--webhook-addr` |
| `notifications-controller` | `RunNotificationsController` → `notifications/controller.Run` | `application.namespaces`, `notificationscontroller.*`, `--enable-leader-election` (controller-runtime Lease `patrol-notifications-controller`), `--app-label-selector`, `--config-map-name`, `--secret-name`, egress guard flags, repo.* helpers through `--repo-server` (5 s timeout, upstream) and `repository.Resolver` |
| `commit-server` | `RunCommitServer` → `commitserver.Run` | API listener with optional TLS/mTLS, `--work-dir`, `--credentials-dir`, `--git-path`, `--git-timeout`, `--max-concurrent-requests`, known hosts and per-host CA bundles (`--ssh-known-hosts-path`, `--tls-certs-dir`, read per request) |
| `cmp-server` | `RunCMPServer` → `cmp.Run` | `cmp.Options` flags and `PATROL_CMP_*` / `ARGOCD_*` environment |
| `k8s-auth` | `k8sauth.NewCommand` | ExecCredential on stdout only |

## Shutdown

SIGTERM or SIGINT cancels the component context; a second signal terminates the process
immediately. Every long-running part is a task of `runGroup`: the first failing task (or a
panic, converted to an error) cancels the others; readiness turns 503 as soon as the drain
starts; HTTP servers shut down with `--shutdown-timeout`; from the cancellation (signal or
failure) the group waits at most `--shutdown-timeout` + 5 s, even when no task returns, and
then reports the tasks still running. Shard Leases are
released (OnLost callbacks first), leader election Leases are released on cancel, the CMP
socket is removed. Exit status: 0 after a clean shutdown, 1 on error.

## Observability

- Metrics: one registry per process with the Go and process collectors plus every library's
  collectors (`patrol_server_*`, `patrol_controller_*`, `patrol_reposerver_*`,
  `patrol_appset_*`, `patrol_notifications_*`, `patrol_commitserver_*`, settings, repository,
  webhook, sharding, tree API metrics), served on the metrics listener only.
- Tracing (OPS-085): with `otlp.address` (server, controller, repo-server) a global OTLP/gRPC
  tracer provider is installed (`otlp.insecure` default true; `otlp.headers` sent with every
  export and never logged; `otlp.attrs` `key:value` resource attributes, malformed ones skipped
  with upstream's warning; `service.name` `patrol-server` / `patrol-controller` /
  `patrol-repo-server`; AlwaysSample; W3C trace context). Client spans with context
  propagation wrap calls to the repo-server, the controller tree API and the commit-server;
  the controller's tree API listener continues incoming traces. Exporter setup failures are
  fatal (`failed to initialize tracing`).
- Logs: `log/slog` (JSON or text), installed as `slog.Default` and as controller-runtime's
  logger; component name attribute; parameter sources logged at debug level without values.

## Security notes

- The internal token is read from a file (or the Secret during development) and never logged;
  missing token → startup error. Clients of internal services never use environment proxies.
- Without `--controller-tree-api-ca-cert-path` / `repo-server-ca-cert-path` / strict TLS,
  internal TLS is not verified (upstream default for self-signed internal certificates); the
  bearer token still authenticates the peer. Verification is opt-in per component.
- Static extension assets and downloads are served through `os.Root` (no traversal or symlink
  escape) and only regular files; `extensions.js` skips symbolic links.
- `--allow-cluster-exec` is off: cluster connection checks of patrol-server never run exec
  plugins or use the server's cloud identity unless enabled.
- The profiler is off by default and toggled per request by a file the operator controls.
- `otlp.headers` and other parameter values are never logged.

## Differences from upstream and gaps

| Upstream | Patrol | Reason |
|----------|--------|--------|
| Default log format JSON everywhere | JSON in a pod, text on a terminal when not configured | readability for local runs; explicit configuration wins |
| `/extensions.js` `application/javascript` | `text/javascript; charset=utf-8` | RFC 9239 media type; both execute under `nosniff` |
| Static files under `/extensions/` not served | served when present (without the proxy header) | UI extension assets next to their bundle |
| `argocd-server` persists a generated self-signed certificate in `argocd-secret` | generated in memory per process (apiserver); `--tls-cert`/`--tls-key` (mounted `patrol-server-tls`) win over `patrol-secret` | no secret writes for a disposable certificate |
| ApplicationSet controller webhook on `:7000` by default | disabled unless `--webhook-addr`; patrol-server's `/api/webhook` refreshes ApplicationSets | one public webhook endpoint |
| Notifications controller without leader election | `--enable-leader-election` (default off) | HA option |
| Startup token normalization failure exits | logged, server keeps serving | availability |

Gaps (not implemented here): `--gloglevel` and routing of klog output into slog (client-go
leader election and some client-go messages are printed by klog in its own format;
`k8s.io/klog/v2` is only an indirect dependency in `go.mod`); kubectl client flags other than
`--kubeconfig`, `--context`, `-n` (OPS-145); server-side tracing spans of patrol-server and
the repo-server (their HTTP servers are built inside `internal/apiserver` and
`internal/reposerver`); the profiler on the repo-server probe listener; `--healthz-addr` for
the repo-server and cmp-server; gzip (`server.enable.gzip`, OPS-110) and
`server.api.content.types` (JSON bodies are always required, apiserver); upstream controller
flags without parameters (`--app-resync*`, `--wq-*`, `--metrics-application-*`,
`--metrics-cluster-labels`, `--enable-k8s-event`); the Dex wrapper (`rundex`, `gendexcfg`,
OPS-152); the hydrator's app controller is not requeued on shard acquisition (it relies on
Application events). The application controller assembly duplicates
`internal/controller.Run` (which has no hooks for sharding, the tree API and the hydrator);
see `docs/verification/integration-findings.md`.

## Deployment requirements (for the manifests owner)

- Secrets/ConfigMaps mounted as files: `patrol-internal-token` → `/app/config/internal-token/token`
  (all components but cmp-server); `patrol-repo-server-tls` → `/app/config/reposerver/tls`;
  mTLS client pairs → `/app/config/reposerver/mtls/client.{crt,key}`; trust ConfigMaps →
  `/app/config/ssh/ssh_known_hosts`, `/app/config/tls`, `/app/config/gpg/source` (repo-server,
  commit-server); `<component>.profile.enabled` → `/home/patrol/params/profiler.enabled`.
- `patrol-cmd-params-cm` projected into environment variables for repo-server and
  commit-server (and optionally for the others, which also read it directly: `get` on the
  ConfigMap).
- RBAC: server — create/update `patrol-secret` (bootstrap), create AppProjects, update
  `appprojects/status`, get Leases (tree API routing); controller — Leases
  get/list/watch/create/update, Cluster status update, statefulsets get with dynamic
  distribution; appset and notifications controllers — Leases for leader election.
- A headless Service `patrol-controller` with port 8090 for the tree API; probe paths as in
  the readiness table.

## Verification

Run on 2026-10-16 (macOS arm64, Go 1.27.2, envtest kube-apiserver 1.36.0), from the
repository root:

```bash
go build ./...                                                                 # exit 0
go vet ./internal/server/... ./internal/ui/... ./cmd/patrolcd/...              # exit 0
golangci-lint run ./internal/server/... ./internal/ui/... ./cmd/patrolcd/...   # 0 issues
KUBEBUILDER_ASSETS=$PWD/bin/envtest/k8s/1.36.0-darwin-arm64 \
  go test -race -count=1 ./internal/server/... ./internal/ui/... ./cmd/patrolcd/...
make build-bin && ./bin/patrolcd version                                       # exit 0
make ui-build && go test -count=1 ./internal/ui/ && make ui-clean               # exit 0
```

Exact output: `docs/verification/runs/2026-10-16-wiring.txt`.

Tests: parameter precedence (flag > env > ConfigMap > default for strings, booleans, lists,
maps and environment-only keys; invalid flag values fatal; invalid environment values fall
back with warnings; fatal ConfigMap enumerations and quantities; every flagged parameter of
every component bound with its compiled default), `patrol-cmd-params-cm` reads (found,
missing, forbidden, failing) with a fake clientset and through the real bootstrap against
envtest (ConfigMap value, environment and flag overrides, client QPS and log level from the
ConfigMap), log options (local/in-cluster defaults, `--debug`), route tests through the real
route table (extension bundle wrapping, order, symlinks, missing directory; static assets
with content types and traversal/symlink/dot-file refusals falling through to the proxy;
downloads with alias and refusals; webhook paths), helper dispatch (source scan and the
built binary in each mode), `--help` goldens of every command
(`cmd/patrolcd/testdata/help`), binary-name dispatch, `version`, task group semantics
(failure, panic, bounded drain, drain bounded from a cancellation that arrives while every
task hangs), observability (readiness, livez, profiler toggled by file),
TLS (file pair reload, client CA, version and cipher validation, client options, API
listener cipher normalization), static assets through the apiserver UI handler (file, links
inside the directory, embedded-first order, links escaping it by absolute or relative path or
through a linked directory not disclosed, missing-directory warning, fatal open error), base
href with a root path (conflict warning), Dex transports (plaintext, unverified default,
strict: missing files, pinned certificate accepted, same-CA other certificate rejected with
`dex server certificate does not match`, foreign CA rejected), the commit-server client
(timeout against a stalled server; TLS and mutual TLS against the real commit-server:
accepted, no client certificate, wrong CA), internal
token loading, default project and token normalization (controller-runtime fake client), and
envtest smoke tests of every entry point: `Run` (healthz on API and metrics ports, version,
extensions.js, UI page, webhook routing, 401 on resource routes, metrics, profiler 401,
generated signing key, default project; with the compiled TLS defaults: TLS 1.3 + HTTP/2 and
TLS 1.2 handshakes, a client without an allowed suite refused; with `RootPath` and the
default base href: `<base href>` and session cookie path from the root path, conflict
warning, 404 outside the prefix, static assets served and an escaping link not disclosed),
`RunController` (readiness, tree API auth and 404,
shard 0 Lease acquired and released on shutdown), `RunAppSetController` and
`RunNotificationsController` (readiness, leader election Lease), `RunRepoServer` (TLS API,
probes incl. `?full=true`, metrics), `RunCommitServer`, `RunCMPServer` (socket created and
removed); each stops cleanly within the bound after cancellation.
