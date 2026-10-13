# Component: `internal/notifications` (notifications engine)

Status: implemented (phase 1, library only). Decision record: ADR-0012. Parity area:
`docs/parity/areas/09-notifications.md` (NOTIF-001..109).

`internal/notifications` is Patrol's own notifications engine with the Argo CD v3.5.5
configuration format. It parses the notifications ConfigMap/Secret pair, evaluates triggers
with `expr`, renders templates with `text/template` + sprig, resolves subscriptions, records
delivery state in an annotation and delivers through 21 built-in services. It operates on
applications as `map[string]any` (unstructured JSON) and does **not** import `api/v1alpha1`.
The controller process (informers, work queue, PATCH of annotations, Events, metrics
endpoint), the REST API handlers and the CLI commands are separate components that call this
library.

## Package layout

| Package | Contents |
|---------|----------|
| `internal/notifications` | `ParseConfig`, `Config`, `Engine` (`Process`, `RunTrigger`, `Render`, `NotifyTemplate`, `Destinations`, list functions), trigger/template compilation, helper namespaces, subscriptions, `DeliveryState`, `Metrics` + Prometheus implementation, annotation keys. |
| `internal/notifications/services` | Rendered `Notification` model (one optional section per service), `Service` interface, `New(type, options, deps)`, the 21 services plus `NewConsole`, shared HTTP client (timeouts, bounded retries, URL validation, redaction), `NewGuardedDialer` (SSRF guard), git URL helpers. |
| `internal/notifications/catalog` | Embedded default triggers/templates derived from the Argo CD catalog (Apache-2.0, see its README; must be listed in `NOTICE`). |

## Responsibilities

- Parse `service.<type>[.<name>]`, `template.<name>`, `trigger.<name>`, `defaultTriggers`,
  `defaultTriggers.<service>`, `subscriptions`, `context`, the deprecated `config.yaml`
  (ConfigMap) and `notifiers.yaml` (Secret). `$key` references are substituted from the
  Secret inside the **parsed** YAML tree (string values and map keys), so a secret value can
  never change the document structure. Missing keys stay literal and produce a warning.
- Evaluate trigger conditions (`when`) and `oncePer` with expr v1.17.8 against the variables
  `app`, `appProject`, `context` (with `notificationType`), `secrets` (unless self-service),
  and helper namespaces `time`, `strings`, `sync`, `repo`.
- Render templates (every string field of every section is a template) with sprig minus
  `env`, `expandenv` and `getHostByName`; extra variables `serviceType` (service **type**)
  and `recipient`. Multiple templates of one condition are merged in order (non-empty
  `message` overrides; a section present in a later template replaces the earlier one;
  `webhook` entries merge per webhook name). Defaults: `github.repoURLPath`
  `{{.app.spec.source.repoURL}}`, `github.revisionPath`
  `{{.app.status.operationState.syncResult.revision}}`, `newrelic.revision/changelog/user`,
  `alertmanager.generatorURL` and label `alertname` = template name.
- Resolve destinations: default subscriptions (label selector), application annotations,
  AppProject annotations (including the deprecated project recipients annotation).
- Deduplicate with the delivery state and deliver; report one `DeliveryResult` per
  (condition, destination): `Sent`, `SkippedDuplicate`, `Failed` (with redacted error) or
  `DryRun`.

## Public API

```go
cfg, err := notifications.ParseConfig(cm.Data, secret.Data, notifications.WithDefaultCatalog())
err = cfg.Validate()                         // all cross-reference/compile problems, joined
eng, err := notifications.NewEngine(cfg,
    notifications.WithProjectGetter(projects),   // GetAppProject(ctx, name) (map[string]any, error)
    notifications.WithRepoService(repo),         // GetCommitMetadata / GetAppDetails
    notifications.WithMetrics(promMetrics),      // NewPrometheusMetrics(registry)
    notifications.WithNetwork(services.Network{DialContext: services.NewGuardedDialer(...), RootCAs: certsForHost}),
    notifications.WithLogger(logger))
if ok, reason := notifications.SyncStatusRefreshed(app); !ok { /* skip, NOTIF-010 */ }
state := notifications.DeliveryStateFromAnnotations(annotations, eng.AnnotationPrefix())
newState, results, err := eng.Process(ctx, app, state)
annotations, err = notifications.ApplyDeliveryState(annotations, eng.AnnotationPrefix(), newState)
```

Other entry points: `RunTrigger` (`admin notifications trigger run`), `NotifyTemplate` +
`services.NewConsole(os.Stdout)` via `WithServices` (`template notify`, default recipient
`console:stdout`), `Render` (dry render), `WithDryRun(true)` (render, never send),
`ListServices/ListTriggers/ListTemplates` (API, sorted), `Destinations`, `Warnings`,
`FailedResults`, `AnnotationDestinations`, `LegacyRecipientsDestinations`,
`Config.GlobalDestinations`, `StateKey`.

Annotation keys (prefix overridable with `WithAnnotationPrefix`; `api/v1alpha1/keys.go`
should mirror the values):

| Key | Meaning |
|-----|---------|
| `notifications.cd.patrol.example/subscribe.<trigger>.<service>` | `;`-separated recipients (empty value = one empty recipient). |
| `notifications.cd.patrol.example/subscribe.<service>` | Recipients for `defaultTriggers.<service>` or `defaultTriggers`. |
| `notifications.cd.patrol.example/subscriptions` | YAML list `{trigger: [..], destinations: [{service, recipients}]}`; empty `trigger` = default triggers. |
| `[<trigger>.]recipients.notifications.cd.patrol.example` (AppProject only) | Deprecated comma list of `service[:recipient]`. |
| `notified.notifications.cd.patrol.example` | Delivery state JSON `{"[<oncePer>:][<ns>:]<trigger>:[<i>].<b64url(sha1(when))>:<service>:<recipient>": <unix>}`. |

### Adapting CRD types

Callers convert typed objects with `runtime.DefaultUnstructuredConverter.ToUnstructured`
(or JSON round trip) and pass the map. The engine never mutates the map it is given
(templates operate on a deep copy because sprig `set/unset/merge` mutate dictionaries).
Field paths in triggers/templates are the JSON field names of the Application CRD, so
Patrol's `Application` must keep the Argo CD JSON shape for template portability.

## Processing algorithm and invariants

1. Destinations = default subscriptions matching labels ∪ app annotations ∪ project
   annotations (∪ legacy project recipients), deduplicated, sorted (triggers by name,
   destinations by service then recipient). No destinations → state returned unchanged.
2. For each trigger: unknown trigger → `Failed` (`ErrTriggerNotFound`) per destination.
   Each condition is evaluated; an evaluation error makes it not triggered (baseline);
   `IncTriggerEvaluation` per condition.
3. Not triggered → key removed for every destination unless the condition has `oncePer`.
4. Triggered → if the key exists: `SkippedDuplicate`; else record, render, send. Failure →
   key removed again (retried at the next evaluation), `Failed` result,
   `IncDelivery(..., false)`. Success → `Sent`, `IncDelivery(..., true)`.
5. State truncated to the 100 newest entries (ties by key).

Invariants: `Process` is deterministic for a given input and clock; it never mutates `app`,
the project object or the input state; a destination not attempted because `ctx` ended keeps
its previous state; secrets never appear in returned errors; the `Engine` is immutable and
safe for concurrent use (rebuild it when the ConfigMap/Secret change).

## Services

All services implement `Send(ctx, Notification, Destination) error`, are wrapped by a
redacting decorator, and (HTTP ones) share: per-attempt timeout (30s), response cap (1 MiB),
up to 3 retries with jittered exponential backoff (1s–5s) on network errors, 408, 429
(`Retry-After` honored, capped at 30s) and 5xx except 501/505, http/https-only URLs plus the
`ValidateURL` hook, redirect re-validation (max 5), per-host CA hook, `insecureSkipVerify`
and pool tuning (`maxIdleConns`, `maxIdleConnsPerHost`, `maxConnsPerHost`,
`idleConnTimeout`). The engine bounds one delivery including retries (`WithSendTimeout`,
default 2m).

| Type | Wire behavior |
|------|---------------|
| `slack` | `POST {apiURL}chat.postMessage` (JSON, `Bearer`), `chat.update` for `PostAndUpdate`/`Update`; attachments/blocks JSON; username/icon (emoji vs URL); `disableUnfurl`; `groupingKey` threads (in-memory LRU of 10k keys), `notifyBroadcast`. |
| `mattermost` | `POST {apiURL}/api/v4/posts` `{channel_id, message, props.attachments}`. |
| `rocketchat` | `POST /api/v1/login` then `/api/v1/chat.postMessage` (`#chan`/`@user` → `channel`, else `roomId`). |
| `teams` | MessageCard to `recipientUrls[recipient]`; success requires body `1`; Workflows URLs auto-detected and sent as Adaptive Card. |
| `teams-workflows` | Adaptive Card envelope; `adaptiveCard` wins; facts/sections → FactSet; OpenUri → `Action.OpenUrl`; hex theme colors map to semantic colors. |
| `googlechat` | `webhooks[recipient]`; `text`, `cards`, `cardsV2` (YAML/JSON, wrapped with `cardId`), `threadKey` + `messageReplyOption`. |
| `telegram` | `POST {apiURL}/bot<token>/sendMessage`, Markdown; `-id`, `-id|thread`, numeric id or `@channel`. |
| `webex` | `POST /v1/messages` markdown; e-mail → `toPersonEmail`, else `roomId`. |
| `pushover` | `POST /1/messages.json` `{token, user, message}`. |
| `grafana` | `POST {apiUrl}/annotations` `{time(ms), isRegion:false, tags: recipient split on |, text}`. |
| `opsgenie` | `POST {apiUrl}/v2/alerts`, `GenieKey <apiKeys[recipient]>`, message ≤130 runes, responder `{type: team, id: recipient}`, all template fields. |
| `pagerduty` | `POST /incidents` (`Token token=`, `From`), service id = recipient. |
| `pagerdutyv2` | `POST /v2/enqueue` `event_action: trigger`, `routing_key = serviceKeys[recipient]`, required summary/severity/source. |
| `newrelic` | `POST {apiURL}/v2/applications/<id>/deployments.json` (`X-Api-Key`). |
| `alertmanager` | `POST {scheme}://{target}{apiPath}` JSON array to every target concurrently; success if any target accepts; basic auth wins over bearer; per-target timeout (default 3s). |
| `webhook` | `webhook.<name>.method/path/body` (default `GET` base URL), static headers, basic auth, `retryMax`/`retryWaitMin`/`retryWaitMax`. |
| `github` | GitHub App (ghinstallation) + REST: commit status (description ≤140, 422 status-limit ignored), deployment (reuses an existing one for ref+environment, `autoMerge` default true) + status, PR comment upsert by tag marker `<!-- patrol-notifications TAG -->` (also finds `argocd-notifications` markers), check run. |
| `awssqs` | aws-sdk-go-v2 SQS: `GetQueueUrl` (owner `account`) unless the queue is a URL, `SendMessage` with string attributes, group/dedup ids; static keys or default chain (IRSA). |
| `gcppubsub` | REST `POST /v1/projects/{p}/topics/{t}:publish`, OAuth2 from `keyFile`/`keyJSON` (service_account only) or ADC. |
| `nats` | Minimal NATS client: `INFO` → optional TLS → `CONNECT` (user/pass, token in URL, nkey seed signature) → `PUB`/`HPUB` → `PING`/`PONG`. |
| `email` | `net/smtp`: implicit TLS on 465 (or `implicitTLS`), STARTTLS when offered, PLAIN/LOGIN/CRAM-MD5 (never cleartext to non-local hosts), quoted-printable body, RFC 2047 subject, CR/LF stripped from headers; recipient may be a comma list. |

`TypeConsole` (`services.NewConsole`) is a CLI-only pseudo service, not configurable.

## Failure modes

| Situation | Behavior |
|-----------|----------|
| Invalid YAML in a key | `ParseConfig` fails (`ErrInvalidConfig`, strict, baseline) or skips with a warning (`Lenient()`). |
| Unknown service type / invalid service options | Engine still builds; `Warnings()`; deliveries to that service fail (`services.ErrUnsupportedServiceType`/`ErrInvalidOptions`). |
| Trigger/template compile error | `Warnings()`; `Validate()` reports; evaluation/render of that item fails. |
| Undefined trigger / template / service at delivery | `Failed` with `ErrTriggerNotFound` ("trigger with name 'x' is not defined"), `ErrTemplateNotFound` ("template 'x' is not supported"), `ErrServiceNotConfigured` ("notification service 'x' is not supported"). |
| Remote 4xx | `*services.StatusError` (redacted body), not retried, `Failed`, retried at the next evaluation. |
| Remote 5xx/429/network | Bounded retries, then as above. |
| AppProject lookup error | Warning log, `appProject = {}`, no project subscriptions (baseline). |
| Repo helper failure | Template/condition error → `Failed` / not triggered. |
| Oversized template output (>1 MiB per field) | `ErrTemplateOutputTooLarge`. |
| Corrupt/oversized state annotation (>256 KiB) | Treated as empty (re-delivers currently true conditions once). |
| `ctx` cancelled | Partial results/state returned with `ctx.Err()`; unattempted destinations untouched. |

## Security notes

- Secrets: substituted values, all Secret values and each service's credential fields are
  redacted from every error (`******`); URLs in errors lose user info and query values;
  redacted errors keep `errors.Is/As` classification but do not expose the raw cause via
  `Unwrap`. The engine never logs option payloads or rendered notifications.
- SSRF: endpoints can come from user-editable places (annotations select services and
  recipients; templates render webhook paths). Only http/https (plus smtp/nats for those
  services) are allowed; `Network.ValidateURL` is called for every endpoint and redirect;
  `NewGuardedDialer` blocks loopback, RFC1918, link-local (cloud metadata), CGNAT, ULA,
  multicast and reserved ranges at connect time (DNS-rebinding safe), with allow/block
  prefixes. The controller should install it unless the operator opts out (in-cluster
  Alertmanager targets need an allow prefix). With an HTTP proxy configured the guard
  applies to the proxy address.
- Templates cannot read the environment or resolve DNS (removed sprig functions); they run
  on deep copies; output is size-bounded. `.secrets` is exposed only for controller-level
  configuration (baseline); self-service engines hide it.
- GCP credentials are restricted to `service_account` keys (external-account files could
  point token exchange at arbitrary URLs); key files are read with a 64 KiB cap.
- SMTP: credentials are never sent over cleartext to non-local servers; `requireTLS`
  refuses unencrypted delivery; header injection is neutralized.

## Parity feature areas served

Implemented in this library (tests in `internal/notifications/**/*_test.go`):
NOTIF-010 (`SyncStatusRefreshed`), NOTIF-011 (CA hook), NOTIF-013 (`time.Now` uses the
process TZ), NOTIF-014..020, NOTIF-021 (project annotations, Patrol key), NOTIF-022..045,
NOTIF-046 (see deviations), NOTIF-047..055, NOTIF-057/058 (metric hooks, `patrol_` names),
NOTIF-059..091, NOTIF-092..100 (embedded catalog), and the engine side of NOTIF-101..108
(list functions, `RunTrigger`, `NotifyTemplate`, console service).

Out of scope here (other components): NOTIF-001..009, 012, 056 (controller process,
informers, flags, metrics endpoint, manifests), the HTTP handlers of NOTIF-101..103, the CLI
commands of NOTIF-104..108 and the UI editor NOTIF-109.

## Deviations from the baseline (deliberate)

- A failed delivery of an `oncePer` condition is not recorded, so it is retried; the
  baseline (as documented by the engine semantics we re-implemented) keeps the `oncePer` key
  and never retries. Non-`oncePer` conditions behave identically (retry next evaluation).
- `sync.GetInfoItem(app, name)` is implemented (documented upstream, not registered in
  Argo CD v3.5.5).
- Resilient construction: an invalid service/template/trigger only breaks deliveries that
  use it (the baseline invalidates the whole configuration); strict YAML parsing is kept.
- `context` values may be numbers/booleans (stringified); the baseline rejects them.
- A trigger may be a single condition object as shown in the upstream docs.
- Keys are applied in sorted order, so duplicate service names resolve deterministically
  (last key wins, with a warning).
- `.secrets` values are strings (the baseline exposes `[]byte`).
- `getHostByName` is removed from sprig in addition to `env`/`expandenv`.
- Context key `patrolUrl` is aliased to/from `argocdUrl`; the catalog uses `patrolUrl`.
- Telegram positive numeric recipients are chat ids (baseline would prefix `@`).
- Opsgenie messages are truncated to 130 runes instead of being rejected.
- Patrol extensions: `apiURL` for telegram/webex/pushover/pagerduty/pagerdutyv2,
  `endpoint`/`keyJSON` for gcppubsub, `messageDeduplicationId` for awssqs (top-level
  `messageGroupId` is also accepted), `implicitTLS`/`requireTLS`/`helloName` for email,
  `insecureSkipVerify` for nats, `pagerdutyToken` alias.
- Metric names use the `patrol_` prefix.

## Known gaps

- The notifications-engine module source (v0.5.1-0.20260503100631-0cff13b8a717) was not
  available offline; engine internals (state key hash, `oncePer` formatting of nil as
  `<nil>`, history size 100, merge order, Slack thread policy for `Update` without a prior
  message = no-op, Opsgenie responder by `id`, deployment reuse) are re-implemented from
  documentation and recollection and are listed as unconfirmed in the parity area.
- Delivery-state keys use the condition hash format above; state written by Argo CD under
  `notified.notifications.argoproj.io` is not migrated.
- Slack thread timestamps are in memory (lost on restart, as upstream); `channels` and
  `signingSecret` options are accepted but unused; `slack.message` (seen in one upstream
  example) is not supported.
- NATS: no JetStream, no credentials files, nkey must be a user seed (public nkeys cannot
  sign).
- GitHub uses its own REST calls with `ghinstallation`, not `pkg/scm` (which appeared during
  this work). Switching would change semantics (full-SHA requirement, check-run upsert by
  `external_id`) and should be decided with the `pkg/scm` owner.
- Self-service configuration is supported through separate engines (one per config,
  `WithSelfServiceNamespace(ns)` for app-namespace configs) sharing one state; the
  orchestration lives in the controller.
- `api/v1alpha1/keys.go` must declare the annotation keys above; `NOTICE` must list the
  catalog data files. Both are outside this component's ownership.
- Failed deliveries are surfaced as results/metrics/logs; emitting Kubernetes Events is the
  controller's job.

## Verification

Commands and results are recorded in the task report; reproduce with:

```bash
go build ./...
go vet ./internal/notifications/...
go test -race -count=1 ./internal/notifications/...
golangci-lint run ./internal/notifications/...
```

Tests are hermetic: httptest servers, an in-test SMTP server (plain, STARTTLS, implicit
TLS with a custom CA), an in-test NATS server (headers, nkey signature, `-ERR`), a fake SQS
JSON endpoint (SigV4 header checked) and a fake Google token + Pub/Sub endpoint.
