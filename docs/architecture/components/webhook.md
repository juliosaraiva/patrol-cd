# Component: `internal/webhook`

Status: implemented. `patrol-server` mounts the handler at `/api/webhook` and
`/api/webhook/{provider...}` (`internal/server/server_api.go` builds it,
`internal/server/server_routes.go` adds the routes, unauthenticated and hidden from
OpenAPI); the ApplicationSet controller can serve the same handler on its optional
`--webhook-addr` listener. Parity areas: `docs/parity/areas/03-repositories-clusters-webhooks.md`
(REPO-114..REPO-131) and `06-applicationset.md` (ASET-104..ASET-107). Threat model: T9
(webhook spoofing), T10 (replay and flood).

Webhook ingestion turns SCM and registry deliveries into Kubernetes-native refresh
requests. It re-implements the behavior of Argo CD v3.5.5 `util/webhook` (API server
webhook) and `applicationset/webhook` (ApplicationSet controller webhook) behind one
endpoint. It holds no authoritative state: the delivery cache, rate limiters and queues are
in memory, per replica and bounded. Every effect is idempotent (an annotation set to a
fixed value), and polling remains the recovery path, so a lost or rejected delivery only
delays a refresh.

## Responsibilities

- Serve `POST /api/webhook` (provider detected from headers) and `POST /api/webhook/{provider}`.
- Read the raw body with the `webhook.maxPayloadSizeMB` cap (clamped to the 25 MiB that
  every `pkg/scm` verifier accepts) before any parsing, within a global in-flight body
  budget.
- Authenticate deliveries with the `pkg/scm` `WebhookVerifier` of the provider, using the
  secrets of `patrol-secret` and of `SCMProvider` objects (`spec.webhook.secretRef`).
- Normalize deliveries to `scm.Event`, de-duplicate verified redeliveries, rate-limit per
  source, queue accepted events.
- Push events: annotate matching Applications with `cd.patrol.example/refresh=normal` (plus
  `cd.patrol.example/hydrate=normal` when the hydrator's dry source matched) and matching
  ApplicationSets with `cd.patrol.example/application-set-refresh=true`.
- Pull request events: annotate ApplicationSets with matching Pull Request generators.
- GHCR package events: annotate Applications whose OCI source matches the published tag.
- Metrics and audit logs.

Out of scope: parsing `patrol-cm`/`patrol-secret` (`internal/settings`), signature
algorithms and payload parsing (`pkg/scm/<provider>`), generator evaluation
(`internal/appset`, consumed through `GeneratorEvaluator`), consuming the annotations
(controller, ApplicationSet controller), mounting and TLS (`internal/server`).

## Public API

| Symbol | Purpose |
|--------|---------|
| `NewHandler(Config) (*Handler, error)` | Validates the configuration and builds the handler. Starts no goroutines. |
| `(*Handler).ServeHTTP` | `http.Handler` for `/api/webhook` and `/api/webhook/{provider}`. |
| `(*Handler).Register(*http.ServeMux)` | Mounts the handler at `Config.PathPrefix` and below it. |
| `(*Handler).Start(ctx) error` | Runs the event and refresh workers until `ctx` ends, then answers 503, drains the queues for at most `ShutdownTimeout` and returns. Satisfies controller-runtime `manager.Runnable`. Can be called once. |
| `Config` | See below. |
| `SettingsSource`, `CurrentSettings(m)` | Reads `settings.Webhook` for every request (secrets, size limit, jitter). `CurrentSettings` adapts `*settings.Manager`; before its first snapshot `SecretsLoaded` is false. |
| `GeneratorEvaluator` | `GenerateParams(ctx, appSet, gen)` and `InterpolateGenerator(appSet, gen, params)`, implemented by `internal/appset`, for Matrix generator matching (ASET-107). |
| `BitbucketCloudOptions`, `BitbucketCredentials` | Diffstat callback configuration (REPO-119). Credentials redact themselves in `String` and `LogValue`. |
| `RateLimit`, `RemoteAddrSource` | Per-source token bucket configuration and the default source key (client IP). |
| `WebURLRegex(webURL)`, `APIURLRegex(apiURL)` | Repository URL equivalence (REPO-124) and the upstream host-only API URL regex. |
| `Provider*` constants | Path names and metric labels: `github`, `gitlab`, `gogs`, `gitea`, `bitbucket`, `bitbucketserver`, `azuredevops`, `ghcr`. |
| `Default*` constants | Defaults listed below. |

`Config` fields:

| Field | Default | Meaning |
|-------|---------|---------|
| `Namespace` | required | Control-plane namespace. Its Applications and ApplicationSets are always considered; its SCMProviders verify deliveries. |
| `ApplicationNamespaces`, `ApplicationSetNamespaces` | none | Extra namespaces (glob, or `/regex/`) whose Applications / ApplicationSets webhooks refresh (`application.namespaces`, `applicationsetcontroller.namespaces`). |
| `SCMProviderNamespaces` | `[Namespace]` | Namespaces whose SCMProvider webhook secrets verify deliveries. Each listed namespace can authenticate deliveries for every repository of its provider type. |
| `Client` | required | Writes the annotations (JSON merge patches). |
| `Reader` | `Client` | Lists Applications, ApplicationSets, SCMProviders; reads Secrets. Use a cache-backed reader: the SCMProvider list is read for every request. |
| `APIReader` | `Client` | Reads an Application right before its refresh patch; the patch carries the `resourceVersion` read. Use a live reader (`mgr.GetAPIReader()`): with a lagging cache the conditional patch conflicts until the cache catches up and may fail after the retries (it never overwrites a newer value). |
| `Settings` | required | `SettingsSource`. |
| `RequireSignatures` | false | Rejects every delivery that cannot be verified, even when no webhook secret is configured anywhere (Patrol extension). |
| `AllowUnsignedPerProvider` | false | Restores upstream's per-provider unsigned policy (see Secrets). By default, once any webhook secret is configured, unsigned deliveries are rejected for every provider. |
| `Workers` | 50 | Event workers (`server.webhook.parallelism.limit`). |
| `RefreshWorkers` | 20 | Patch workers (`server.webhook.refresh.workers`). |
| `QueueSize` | 50000 | Queued events (upstream capacity). |
| `MaxQueuedBytes` | 256 MiB | Estimated memory of queued events (body size + 1 KiB each). |
| `MaxInFlightBodyBytes` | 128 MiB (minimum 25 MiB) | Bytes of request bodies being read and verified concurrently, across all sources. A known `Content-Length` is charged before reading, an unknown one as bytes arrive; released when the request ends. |
| `RateLimit` | 50 req/s, burst 200, 10000 sources | Per-source token bucket, checked before the body is read. |
| `SourceKey` | `RemoteAddrSource` | Behind a reverse proxy, key on a header set by the trusted proxy. |
| `DeliveryCacheSize`, `DeliveryTTL` | 10000, 1h | Redelivery cache bounds. |
| `GeneratorEvaluator` | nil | See ASET-107 below. |
| `BitbucketCloud` | callback enabled, `https://api.bitbucket.org/2.0` allowed, 10s | REPO-119. |
| `PathPrefix` | `/api/webhook` | Mount path. |
| `APITimeout` | 10s | Bound of each Kubernetes API call. |
| `ShutdownTimeout` | 30s | Queue drain bound. |
| `Registerer` | nil | Prometheus registerer for the metrics. |
| `Logger` | `slog.Default()` | Audit and diagnostic logs. |

### Wiring (API server)

```go
h, err := webhook.NewHandler(webhook.Config{
    Namespace:             ns,
    ApplicationNamespaces: params.ApplicationNamespaces,
    Client:                mgr.GetClient(),
    Reader:                mgr.GetCache(),          // or the manager client (cache-backed reads)
    APIReader:             mgr.GetAPIReader(),      // live reads before conditional refresh patches
    Settings:              webhook.CurrentSettings(settingsManager),
    Workers:               params.WebhookParallelismLimit,
    RefreshWorkers:        params.WebhookRefreshWorkers,
    GeneratorEvaluator:    appsetEvaluator,          // internal/appset adapter, optional
    BitbucketCloud:        webhook.BitbucketCloudOptions{Credentials: repoCredsByURL},
    Registerer:            metricsRegistry,
    Logger:                logger,
})
mux.Handle("/api/webhook", h); mux.Handle("/api/webhook/", h) // or h.Register(mux)
mgr.Add(h)                                                     // runs Start
```

The endpoint is unauthenticated (no session, no CSRF check), as upstream: authentication is
the provider signature. The RBAC needed is `list/watch` on Applications, ApplicationSets
and SCMProviders, `patch` on Applications and ApplicationSets, and `get` on the Secrets
SCMProviders reference.

## Behavior

### Request flow and responses (REPO-114, REPO-115, ASET-104)

1. Per-source rate limit → `429 Too many webhook requests` (`Retry-After: 1`).
2. Provider resolution. `/api/webhook`: the first provider whose header predicate matches,
   in this order: Azure DevOps (`X-Vss-Activityid`), Gitea (`X-Gitea-Event`), Gogs
   (`X-Gogs-Event`, before GitHub because Gogs also sends GitHub headers), GitHub
   (`X-GitHub-Event` other than `package`, without Gogs/Gitea headers), GitLab
   (`X-Gitlab-Event`), Bitbucket Cloud (`X-Hook-UUID`), Bitbucket Server (`X-Event-Key`
   without `X-Hook-UUID`; delivery ID `X-Request-Id`), GHCR (`X-GitHub-Event: package`). None → `400 Unknown
   webhook event`. `/api/webhook/{provider}` (also `bitbucketcloud`, `bitbucket-cloud`,
   `bitbucket-server`, `azure-devops`): unknown name → `404 Unknown webhook provider`;
   headers of another provider → `400 Unknown webhook event`.
3. Non-POST → `405 Webhook processing failed` with `Allow: POST`.
4. Body. The limit is `webhook.maxPayloadSizeMB` (default 50 MB) clamped to 25 MiB
   (`MaxEffectivePayloadSize` = `scm.MaxWebhookPayloadBytes`, the cap of every `pkg/scm`
   verifier; the clamp is logged once). `Content-Length` above the limit is rejected
   without reading; otherwise at most limit+1 bytes are read. Over the limit → `400 Webhook
   processing failed: payload must be valid JSON under N MB` (N = effective limit/1 MiB,
   integer division as upstream, so 25 by default). Before reading, the request is charged
   to the in-flight body budget (`MaxInFlightBodyBytes`): exhausted → `503 Webhook server is
   busy` (`Retry-After: 1`), also when a body without `Content-Length` exhausts it while
   being read.
5. Secrets (below). patrol-secret not loaded yet and no SCMProvider secret → `503 Webhook
   secrets are not loaded yet` (`Retry-After: 10`). Listing SCMProviders fails → `503`.
   Every configured secret unreadable → rejected (400; GHCR 401). No secret for the
   provider while another provider has one (default policy), or `RequireSignatures` →
   `400 Webhook processing failed` (GHCR `401 Unauthorized`; result `unsigned_rejected`).
6. Verification and parsing by the provider's `WebhookVerifier` over the exact raw bytes;
   with several candidate secrets, a signature mismatch tries the next one, any other error
   is final. Missing or mismatched signature → `400 Webhook processing failed` (security
   log); GHCR → `401 Unauthorized`. Malformed JSON → `400 ... valid JSON under N MB`.
   Unsupported event → `400 Webhook processing failed`. Invalid delivery ID (over 128
   bytes or non-printable) → 400.
7. De-duplication of verified deliveries by provider + delivery ID → `200` (no effect).
8. Irrelevant events (ping, push without repository URL, pull request actions that do not
   refresh, skipped package events) → `200`.
9. Enqueue → `200`; queue or byte budget full, or shutting down → `503 Queue is full,
   discarding webhook payload` (the delivery is forgotten so a redelivery is processed).

Workers recover from panics (logged, delivery forgotten) and keep serving.

### Secrets (REPO-116, T9)

| Provider | patrol-secret key | SCMProvider types whose `webhook.secretRef` also verifies |
|----------|-------------------|------------------------------------------------------------|
| GitHub, GHCR | `webhook.github.secret` | `github` |
| GitLab | `webhook.gitlab.secret` | `gitlab` |
| Gogs | `webhook.gogs.secret` | — |
| Gitea | `webhook.gogs.secret` (upstream verifies Gitea deliveries as Gogs) | `gitea` |
| Bitbucket Cloud | `webhook.bitbucket.uuid` | `bitbucketCloud` |
| Bitbucket Server | `webhook.bitbucketserver.secret` | `bitbucketServer` |
| Azure DevOps | `webhook.azuredevops.username` + `webhook.azuredevops.password` | `azureDevOps` (the key holds `username:password`) |

- Candidates are the patrol-secret value first, then SCMProvider secrets ordered by
  namespace and name, at most 16. Any candidate verifies the delivery, so an old and a new
  secret can coexist during rotation (GitHub and every other provider).
- Settings are read per request: rotating patrol-secret (including the Azure DevOps
  credentials) applies without a restart.
- Unsigned deliveries. The provider is chosen from request headers and the repository
  from the payload, and neither is authenticated without a secret. A per-provider secret
  therefore does not protect a repository by itself: a forged delivery shaped like
  another provider's (for example `X-GitHub-Event: push` whose `html_url` names a GitLab
  repository) is verified with that other provider's secrets. Patrol's policy:
  - No webhook secret configured anywhere (no patrol-secret key, no SCMProvider
    `webhook.secretRef` in `SCMProviderNamespaces`): unsigned deliveries are accepted, as
    upstream (only refreshes and generator evaluation can result, which polling would do
    anyway).
  - Any webhook secret configured, for any provider (default): unsigned deliveries are
    rejected for every provider (`unsigned_rejected`), so configuring one secret closes
    the unsigned path for all routes. Configure a secret for every provider that sends
    webhooks.
  - `Config.AllowUnsignedPerProvider`: upstream's per-provider behavior (accept unsigned
    deliveries for providers without a secret even when others have one). This re-opens
    the cross-provider bypass above.
  - `Config.RequireSignatures`: reject every unsigned delivery, even without any secret.
  Unverified deliveries are never recorded in the delivery cache (their IDs are
  attacker-controlled).
- A configured SCMProvider secret that cannot be read (missing Secret or key) still counts
  as configured: deliveries are rejected rather than accepted unsigned (fail closed).
- `settings.Webhook.SecretsLoaded == false` (patrol-secret never loaded): treated as
  configured but unknown; only SCMProvider secrets can verify, otherwise 503.
- GitLab tokens, the Bitbucket Cloud UUID and Azure DevOps basic auth authenticate the
  sender, not the body (no body signature exists for those providers).

### Applications (REPO-117..REPO-127, REPO-129, REPO-130)

For a push, the repository URLs are: the web URL (GitHub, GitLab, Gogs, Gitea, Bitbucket
Cloud), both clone links (Bitbucket Server, each matched separately, `.git` kept) or the
remote URL (Azure DevOps). For each URL, `WebURLRegex` builds
`(?i)^((https?|ssh)://)?(<user>@)?((alt)?ssh\.)?<host>(:\d+)?[:/]<path>(\.git)?$` (host and
path regex-escaped), so HTTPS/SSH/scp forms, ports, `ssh.`/`altssh.` hosts and `.git` are
equivalent (GitHub Enterprise included). Applications are listed in the control-plane
namespace, or in every namespace when `ApplicationNamespaces` is set and filtered by it.

Candidate sources of an Application: `spec.source`, every entry of `spec.sources` (ref-only
sources included), or for `spec.sourceHydrator` the sync source plus the dry source. The
first source that matches decides:

1. Revision: `targetRevision` empty or `HEAD` → the push touched the default branch;
   `refs/heads/x` / `refs/tags/x` → `x` equals the pushed revision; otherwise the full
   value equals it, or is a semver constraint (Masterminds, `v` prefix accepted) satisfied
   by the pushed revision. Branches and tags are not distinguished.
2. URL: the source `repoURL` matches the regex.
3. Paths: `cd.patrol.example/manifest-generate-paths` entries (`;`-separated, trimmed,
   empties skipped; `/x` repository-relative, others joined to the source path and
   cleaned; ignored for the hydrator sync source, which uses its own path). A changed file
   equal to a path, under it, or matching it as a `path.Match` glob refreshes. No
   annotation, no changed-file list (Bitbucket Server, Azure DevOps, Bitbucket Cloud
   without callback) or a truncated list (over 10000 files) refreshes.

A match enqueues `refresh=normal`, plus `hydrate=normal` when the matching source is the
dry source. Per repository URL, refreshes beyond `webhook.refresh.jitter.threshold` are
delayed by a uniform random duration below `webhook.refresh.jitter` (0 disables jitter).

GHCR `package` events (`action: published`, `package_type: container`, non-empty tag)
match sources whose `repoURL` normalizes (strip `oci://`, trailing `/`, lower-case) to
`ghcr.io/<owner>/<package>` and whose `targetRevision` equals the tag or is a semver
constraint it satisfies; no jitter.

### Refresh writes (idempotent)

- A de-duplicating delaying work queue (comparable request: kind, namespace, name,
  hydrate) collapses repeated requests for the same object.
- Applications: the object is read through `APIReader` and patched with a JSON merge
  patch of `metadata.annotations` that also carries `metadata.resourceVersion` (optimistic
  lock; other annotations are untouched). The values are `normal`, or `hard` when a hard
  refresh (hydration) is pending in the object read. If the object changed in between (a
  user requested a hard refresh, the controller consumed the annotation), the API server
  answers 409 and the request is re-read and re-decided. So a pending `hard` is never
  downgraded, and a stale view showing `hard` cannot suppress a refresh: the patch is sent
  even when the values read are already set, and the `resourceVersion` check proves the
  read was current.
- ApplicationSets: an unconditional merge patch of
  `cd.patrol.example/application-set-refresh=true` (a single value, nothing to preserve).
- Conflicts, 429 and server timeouts are retried up to 5 attempts 100 ms apart (upstream:
  conflicts only, with an unconditional patch). A deleted object is logged and counted as
  `not_found`; a conflict persisting after 5 attempts (for example a permanently stale
  `APIReader`) is counted as `error` and left to polling.

### ApplicationSets (ASET-104..ASET-107)

ApplicationSets are listed in the control-plane namespace or, with
`ApplicationSetNamespaces`, in every namespace and filtered by it. An ApplicationSet is
refreshed when any top-level generator is affected:

- Git generator (push events): `repoURL` matches the regex and the revision is empty/HEAD
  with the default branch touched, or the short (`refs/heads/` stripped) or full revision
  equals the pushed revision (no semver).
- Pull Request generator (pull request events with action opened, closed, synchronize,
  labeled, reopened or unlabeled; GitLab open/close/reopen/update/merge and Azure DevOps
  created/updated/merged normalize onto them):
  - GitHub: owner and repo equal (case-insensitive) and the generator API (explicit, from
    the referenced SCMProvider, or `https://api.github.com/`) is on the host of the
    payload repository API URL.
  - GitLab: `project` equals the target project ID (or `owner/repo`, Patrol extension) and
    the API (default `https://gitlab.com/`) host equals the project web URL host.
  - Azure DevOps: project and repository names equal (case-sensitive).
  - Gitea, Bitbucket Server, Bitbucket Cloud (Patrol extensions): owner/project and repo
    (case-insensitive) and, when the generator has an API URL, its host.
- Plugin generator at top level: any push or pull request event.
- Matrix (exactly two children): child 0 matching (Git, Pull Request, nested Matrix/Merge);
  otherwise, with a `GeneratorEvaluator`, child 0's parameter sets are generated and child
  1 interpolated with each and checked (Git, Pull Request, Plugin, nested Matrix/Merge),
  then child 1 as written. At most 10000 interpolations per ApplicationSet; beyond that it
  is refreshed. Evaluation or interpolation errors → not refreshed (as upstream).
  Without an evaluator, templated (`{{`) fields of child 1 match any value.
- Merge: any child matching (Git, Pull Request, nested Matrix/Merge); Plugin children do
  not refresh.
- Malformed nested Matrix/Merge JSON → not refreshed (the ApplicationSet controller reports
  it).

### Bitbucket Cloud callback (REPO-119)

For verified `repo:push` deliveries only, the API base is derived from
`repository.links.self.href` minus `/repositories/<full_name>`. Because that URL is not
covered by the UUID check, it must be one of `BitbucketCloudOptions.AllowedAPIURLs`
(default `https://api.bitbucket.org/2.0`); otherwise no call is made. Credentials come from
`Credentials(ctx, webURL)`: user `x-token-auth` + password → bearer; other user + password
→ basic auth; otherwise the bearer token (empty: anonymous). The diffstat
(`<before>..<after>`, renames, every page, old and new paths) replaces the changed-file
list, and the main branch lookup sets `TouchedDefaultBranch`. Any failure keeps the
conservative defaults (every path changed, default branch touched). 10s timeout.

### Metrics

| Metric | Labels |
|--------|--------|
| `patrol_webhook_requests_total` | `provider` (closed set + `unknown`), `result` (`accepted`, `ignored`, `duplicate`, `unknown_provider`, `method_not_allowed`, `rate_limited`, `too_large`, `busy`, `invalid_payload`, `unsupported_event`, `unauthorized`, `unsigned_rejected`, `secrets_unavailable`, `queue_full`, `error`) |
| `patrol_webhook_refreshes_total` | `kind` (`application`, `applicationset`), `result` (`requested`, `not_found`, `error`) |
| `patrol_webhook_events_in_flight` | — |
| `patrol_webhook_event_duration_seconds` | — (histogram) |

Repository URLs are not used as labels (untrusted, unbounded).

### Audit log

One `webhook: event processed` record per processed event with `provider`, `event`,
`action`, `deliveryID`, `verified`, `repo` (redacted URL), `revision`, `applications`,
`applicationSets` and `duration`; failures log `webhook: event processing failed` with the
error. Each refresh logs kind, namespace and name. Rejections log the provider and reason
with `security=high` for signature, size and unsigned-policy failures. Bodies, headers and
secret values are never logged. Payload-derived values (`action`, `repo`, `revision`, GHCR
repository and tag) are clipped to 256 bytes and error messages (which may quote header or
payload values) to 1 KiB, with a `...[truncated N bytes]` marker, so an unauthenticated
sender cannot inflate log volume.

## Invariants

- The body is never parsed before the size cap and the signature check.
- A configured (or unknown) secret is never bypassed: missing, unreadable or unloaded
  secrets reject the delivery, and (default policy) any configured secret makes
  signatures mandatory for every provider.
- Effects are annotation writes with fixed values; repeating a delivery is harmless. A
  refresh patch never overwrites an annotation value it did not read (resourceVersion).
- Memory is bounded: in-flight request bodies (`MaxInFlightBodyBytes`, raw bytes; JSON
  parsing transiently allocates a small multiple of the body being parsed), rate-limiter
  LRU, delivery cache, event queue (count and bytes), de-duplicating refresh queue,
  interpolation budget, secret candidate cap (16 HMACs of at most 25 MiB per delivery, and
  at most `MaxInFlightBodyBytes / 25 MiB` such deliveries concurrently), URL length cap
  (2048 bytes before building regular expressions), log values clipped.

## Failure modes

| Failure | Behavior |
|---------|----------|
| API server unavailable while listing SCMProviders | 503; the SCM may redeliver. |
| Listing Applications/ApplicationSets fails | Event processing error logged; delivery forgotten so a redelivery is processed. |
| Patch fails after retries | `refreshes_total{result="error"}`, warning log; polling recovers. |
| Queue full / shutdown | 503; delivery forgotten. |
| In-flight body budget exhausted | 503 `Webhook server is busy`, `Retry-After: 1`; nothing read or remembered. |
| Refresh patch conflicts 5 times | `refreshes_total{result="error"}`; polling recovers. |
| Shutdown timeout | Remaining refreshes abandoned (logged); polling recovers. |
| Bitbucket callback fails | Conservative defaults; matching apps refreshed. |

## Security notes (T9, T10)

- SEC-WH-1/2: HMAC (GitHub, GHCR, Gogs, Gitea, Bitbucket Server) over the exact raw bytes,
  constant-time; token/UUID/basic-auth comparisons are constant-time (`pkg/scm`).
- SEC-WH-3: deliveries without a valid signature are rejected whenever a secret is
  configured for the provider, including SCMProvider secrets and unloaded patrol-secret.
  Because the provider route is chosen by attacker-controlled headers and the repository
  URL comes from the payload, a secret for one provider only protects its repositories if
  unsigned deliveries are refused on every route: by default, any configured webhook
  secret makes signatures mandatory for all providers. `AllowUnsignedPerProvider`
  (upstream behavior) re-opens the bypass: with only `webhook.gitlab.secret` set, an
  unsigned `X-GitHub-Event: push` naming a GitLab repository refreshes its Applications;
  a Gitea SCMProvider secret is bypassed through the Gogs route the same way. The effect
  is limited to refreshes and generator evaluation.
- SEC-WH-4: body size cap before parsing (effective limit `min(webhook.maxPayloadSizeMB,
  25 MiB)`); Content-Length checked first; global in-flight body budget.
- SEC-WH-5: delivery-ID de-duplication for verified deliveries (bounded LRU, TTL). It is
  not replay protection: GitHub signs no timestamp or nonce.
- SEC-WH-6: effects are idempotent annotations; hard refreshes are not downgraded
  (conditional patches on `resourceVersion`, verified against a real API server).
- SEC-WH-7: per-source rate limiting (429) before the body is read; a global in-flight
  body budget (503) bounds memory and HMAC work across sources (a single source could
  otherwise hold burst x limit bytes); queues are bounded; untrusted values are clipped in
  logs.
- Tenant namespaces are not trusted for SCMProvider secrets unless listed in
  `SCMProviderNamespaces`.
- The Bitbucket Cloud callback never sends credentials to a host taken from the payload
  unless allow-listed.

## Parity IDs covered

REPO-114..REPO-125, REPO-127, REPO-129..REPO-131, APP-090 (alias REPO-126; webhook part),
ASET-104..ASET-107. Status: `docs/parity/status/webhook.yaml`.

## Gaps and intentional differences

1. **One endpoint for Applications and ApplicationSets.** Upstream serves the ApplicationSet
   webhook from the ApplicationSet controller (`:7000/api/webhook`). Patrol's
   `patrol-server /api/webhook` refreshes both, so one SCM webhook suffices (ASET-104).
   The ApplicationSet controller serves the same handler only when `--webhook-addr` is set
   (chart `appsetController.webhook.enabled`, off by default).
2. **ApplicationSet events from every provider.** Upstream's ApplicationSet webhook handles
   GitHub, GitLab and Azure DevOps only; Patrol also refreshes Git generators on Gogs,
   Gitea, Bitbucket Cloud and Bitbucket Server pushes, GitLab tag pushes, and Pull Request
   generators on Gitea and Bitbucket pull request events. Extra refreshes are idempotent.
3. **GitHub Enterprise API URLs with a path** (`https://ghe.example.com/api/v3/`) match pull
   request events by host; upstream's host-only regex never matches them. GitLab
   generators may also name the project as `owner/repo`.
4. **SCMProvider secrets** (several per provider, rotation) and **per-request settings**
   (no restart for secret, size-limit or jitter changes, Azure DevOps credentials included)
   are Patrol extensions; upstream reads secrets at handler construction.
5. **patrol-secret not loaded** answers 503 instead of accepting unsigned deliveries.
6. **Rate limiting, delivery de-duplication, byte-bounded queue, 404 for unknown
   `/api/webhook/{provider}`** are additions (T10).
7. **Manifest cache warming (REPO-128) is not implemented.** Upstream copies cached
   manifests to the new revision key for matching sources whose files did not change
   (`ARGOCD_WEBHOOK_MANIFEST_CACHE_WARM_DISABLED`, repository
   `webhookManifestCacheWarmDisabled`). Patrol leaves such Applications to the controller,
   whose refresh path checks `manifest-generate-paths` with the repo-server
   `UpdateRevisionForPaths` call (APP-090); the webhook does not touch caches.
8. **Metrics** carry `provider`/`result` labels instead of upstream's per-repository
   `argocd_webhook_requests_total{repo}`; there is no cache-warming counter (REPO-131).
9. **Registry events** set `hydrate=normal` only when the matching source is a hydrator dry
   source; upstream also writes it on Applications without a hydrator, where it has no
   effect.
10. **Bitbucket Cloud callback** is restricted to allow-listed API hosts, reports old and
    new paths of renames, and treats zero SHAs (branch creation or deletion) as "every
    path changed".
11. **Refresh patches are conditional.** Upstream `RefreshApp` writes an unconditional
    merge patch (and can downgrade a hard refresh requested concurrently); Patrol includes
    the `resourceVersion` read and retries real conflicts. Retries also cover 429 and
    server timeouts.
12. Matrix evaluation uses the `GeneratorEvaluator` of `internal/appset`, which both
    `patrol-server` and the ApplicationSet controller's listener pass in; a handler built
    without one matches templated second children with any value (over-approximation).
13. **Payload limit clamped to 25 MiB.** Upstream applies `webhook.maxPayloadSizeMB`
    (default 50 MB) as is. Every `pkg/scm` verifier rejects bodies over 25 MiB (GitHub's
    own delivery cap), so Patrol clamps the setting: the default and any value above 25
    behave as 25 MiB, and the 400 body states the effective limit (`under 25 MB`, upstream
    `under 50 MB`). Values up to 25 behave as upstream. GHCR uses the same limit.
14. **Unsigned deliveries when some secret is configured.** Upstream accepts unsigned
    deliveries for each provider without a secret; Patrol rejects them once any webhook
    secret is configured (see SEC-WH-3). `AllowUnsignedPerProvider` restores upstream.
15. **In-flight body budget** (`503 Webhook server is busy`) is an addition (T10).

## Verification

Commands run on 2026-10-16 (darwin/arm64, Go 1.27.2) after the review fixes (payload
clamp, log clipping, in-flight body budget, conditional refresh patches, unsigned
policy):

```text
go build ./internal/webhook/...                                  # exit 0
go build ./...                                                   # exit 1: internal/repository, then
                                                                 # internal/reposerver (other workstreams,
                                                                 # mid-edit); nothing imports
                                                                 # internal/webhook yet
go vet ./internal/webhook/...                                    # exit 0
golangci-lint run ./internal/webhook/...                         # 0 issues
go test -race -count=1 ./internal/webhook/...                    # ok (envtest tests skipped)
KUBEBUILDER_ASSETS=bin/envtest/k8s/1.36.0-darwin-arm64 PATROL_REQUIRE_ENVTEST=1 \
  go test -race -count=2 ./internal/webhook/...                  # ok
KUBEBUILDER_ASSETS=bin/envtest/k8s/1.36.0-darwin-arm64 PATROL_REQUIRE_ENVTEST=1 \
  go test -race -count=1 -v ./internal/webhook/...               # ok, 37 top-level tests PASS, 0 SKIP
go run ./hack/parity                                             # exit 0
go run ./hack/parity -check                                      # exit 1: "matrix.md is stale" right
                                                                 # after regeneration; the generator
                                                                 # orders alias-merged impl/test lists
                                                                 # (APP-038, APP-077, DIFF-076 from
                                                                 # sync/tracking) non-deterministically;
                                                                 # webhook rows are stable
```

Tests: `handler_test.go` (per-provider signed fixtures: valid, explicit path, tampered,
wrong secret, missing signature, unsigned accepted/rejected, oversize with and without
Content-Length, replayed delivery ID, rate limit; HTTP status table; SCMProvider secrets,
rotation, fail-closed cases; lifecycle), `providers_test.go` (normalization and URL
equivalence per provider, event variants), `match_test.go` (upstream `Test_GetWebURLRegex`
and `TestAppRevisionHasChanged` tables, URL property test, paths, namespaces),
`process_test.go` (Application matching table: revision, paths, multi-source, hydrator;
jitter; conflict retries), `appsets_test.go` (Git, Pull Request, Plugin, Matrix, Merge and
nested generators; handler end to end), `ghcr_test.go` (registry matching, skipped events,
panic recovery), `bitbucket_test.go` (diffstat callback against a fake Bitbucket API),
`envtest_test.go` (signed deliveries against a real kube-apiserver with the generated CRDs:
annotations patched, unrelated annotations kept, namespaces filtered, hard refresh kept,
replay ignored; conditional refresh patches answered 409 by the API server when the
annotation changes between read and patch), `hardening_test.go` (effective payload limit
with signed 30 MiB and exactly-25 MiB deliveries, GHCR included; log clipping of 2 MiB
refs, 1 MiB actions and 64 KiB event names; in-flight body budget with concurrent
Content-Length and chunked requests; unsigned policy including the cross-provider forgery;
conditional refresh patches with stale readers and concurrent changes).
