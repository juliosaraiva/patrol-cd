# Notifications — parity inventory (Argo CD v3.5.5)

Scope: everything Argo CD does to notify humans and external systems about Application state changes. **In scope**: the `argocd-notifications` controller binary (`cmd/argocd-notification`, `notification_controller/`), its flags/env/`argocd-cmd-params-cm` keys, watch scope (controller namespace, `--application-namespaces`, label selector), the sync-status freshness gate, AppProject lookup, the Argo CD-specific glue in `util/notification/` (factory settings, template/trigger variables, `context`, `secrets`, `appProject`, legacy `config.yaml`/`notifiers.yaml`/`recipients.argocd-notifications.argoproj.io` support, expression helper namespaces `time`, `strings`, `repo`), the `argocd-notifications-cm` / `argocd-notifications-secret` configuration model (`service.<type>[.<name>]`, `template.<name>`, `trigger.<name>`, `defaultTriggers[.<service>]`, `subscriptions`, `context`, `$secret-key` references), triggers (`when`, multiple conditions, `oncePer`, `send`, `description`), templates (`message` + per-service sections), subscriptions (Application / AppProject annotations, YAML `subscriptions` annotation, default subscriptions), delivery state (`notified.notifications.argoproj.io`), self-service (per-namespace) configuration, metrics, every delivery service documented at this commit (alertmanager, awssqs, email, gcppubsub, github, googlechat, grafana, mattermost, nats, newrelic, opsgenie, pagerduty, pagerdutyv2, pushover, rocketchat, slack, teams, teams-workflows, telegram, webex, webhook), the default catalog (`notifications_catalog/install.yaml`), the `NotificationService` gRPC/REST API, the `argocd admin notifications` CLI and the UI subscription editor.
**Out of scope** (other areas): Argo CD Kubernetes event emission / audit logging, UI toast "notifications" (`ctx.notifications.show`), Application sync/health semantics themselves (only consumed here), repo-server internals behind `GetRevisionMetadata`/`GetAppDetails` (sources area), repo-server TLS/mTLS server side (repositories/security area), generic `argocd-cm` `resource.customizations.ignoreResourceUpdates` machinery (diff/resources area; only the notified-annotation entry is listed here), ApplicationSet annotation preservation machinery (ApplicationSet area; only the notified-annotation entry is listed here).

Upstream permalink base: `https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/`. The delivery engine is the vendored module `github.com/argoproj/notifications-engine v0.5.1-0.20260503100631-0cff13b8a717` ([go.mod#L18](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/go.mod#L18)). **The engine source was not present in the local module cache (`~/go/pkg/mod/github.com/` has no `argoproj` entry) and no `vendor/` directory exists**, so engine-internal behaviour (state-key format, retry-on-failure, template engine choice, per-service payload shapes) is taken from Argo CD's own docs and is flagged in "Unconfirmed" where the docs are silent. The set of compiled-in services is corroborated by indirect dependencies in `go.mod` (Slack [#L272](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/go.mod#L272), gomail [#L301](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/go.mod#L301), Opsgenie [#L259](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/go.mod#L259), PagerDuty [#L159](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/go.mod#L159), Telegram [#L158](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/go.mod#L158), Rocket.Chat [#L161](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/go.mod#L161), SQS [#L169](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/go.mod#L169), Pushover [#L223](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/go.mod#L223), NATS [#L252](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/go.mod#L252), Pub/Sub [#L141](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/go.mod#L141), go-github v88 [#L326](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/go.mod#L326)). Template functions use Sprig v3.3.0 ([go.mod#L14](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/go.mod#L14)); trigger expressions use `expr-lang/expr` v1.17.8 ([go.mod#L40](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/go.mod#L40)).

Maturity: no notifications feature appears in `docs/operator-manual/feature-maturity.md` ([#L18](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/feature-maturity.md#L18)), so everything defaults to GA, except items the code/docs explicitly mark deprecated (legacy `config.yaml`/`notifiers.yaml`/recipients annotation, `--argocd-repo-server-strict-tls`, the Office 365 Connectors `teams` service).

---

## A. Controller process and runtime

### NOTIF-001 Notifications controller process lifecycle
- **Upstream**: [cmd/argocd-notification/commands/argocd_notification.go#L57](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-notification/commands/argocd_notification.go#L57), [argocd_notification.go#L64](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-notification/commands/argocd_notification.go#L64), [argocd_notification.go#L143](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-notification/commands/argocd_notification.go#L143), [argocd_notification.go#L158](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-notification/commands/argocd_notification.go#L158), [argocd_notification.go#L164](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-notification/commands/argocd_notification.go#L164), [notification_controller/controller/controller.go#L190](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notification_controller/controller/controller.go#L190), [controller.go#L205](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notification_controller/controller/controller.go#L205), [common/common.go#L32](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L32), [cmd/main.go#L63](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/main.go#L63), [manifests/base/notification/argocd-notifications-controller-deployment.yaml#L9](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/notification/argocd-notifications-controller-deployment.yaml#L9)
- **Maturity**: GA
- **Semantics**: Multi-call binary entrypoint `argocd-notifications` (`common.CommandNotifications`); manifest runs `/usr/local/bin/argocd-notifications`, workingDir `/app`, Deployment strategy `Recreate`. Startup order: resolve namespace from kubeconfig/in-cluster (`-n/--namespace` kubectl flag), log startup info `"ArgoCD Notifications Controller"` with `namespace`; build REST config; apply Argo CD client defaults (NOTIF-009); build dynamic + typed clients; parse log level/format (NOTIF-005); build repo-server client (NOTIF-007); construct the Argo CD service (settings manager + repo-server connection); start the metrics HTTP server (NOTIF-056); construct the controller and `Init` it: run four informers (Applications, AppProjects in the controller namespace, the notifications Secret, the notifications ConfigMap) and wait for all caches to sync; then `Run(processors)` workers. SIGINT/SIGTERM cancels the root context and the command returns `nil`. Any panic in the command is recovered and logged as fatal with the stack trace. There is **no leader election** in this command, so the controller is a single-replica component.
- **Edge cases**: cache sync failure/context timeout → `failed to initialize controller: timed out waiting for caches to sync` (unit test asserts the inner message); REST config errors → `failed to create REST client config: ...`; dynamic/typed client errors → `failed to create dynamic client: ...` / `failed to create Kubernetes client: ...`; repo-server dial failure → `failed to initialize Argo CD service: ...`; running two replicas → both evaluate and deliver independently (no coordination; inferred from absence of leader election).
- **Configuration surface**: kubectl flags added by `cli.AddKubectlFlagsToCmd` (`--kubeconfig`, `--context`, `--namespace`, `--server`, `--token`, ...); Deployment `argocd-notifications-controller`, ServiceAccount `argocd-notifications-controller`.
- **Exposure**: Component only (no CRD/API/CLI/UI).
- **Tests required**:
  - positive: controller starts, syncs informers for apps/projects/cm/secret and begins processing.
  - negative: unreachable API server → process exits with `failed to initialize controller: timed out waiting for caches to sync` (or client error) and non-zero code.
  - recovery: SIGTERM → clean exit (code 0) without delivering partial notifications.
  - negative: starting with an invalid `--logformat` exits before informers start (NOTIF-005).

### NOTIF-002 Application watch scope (controller namespace + `--application-namespaces`)
- **Upstream**: [controller.go#L80](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notification_controller/controller/controller.go#L80), [controller.go#L111](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notification_controller/controller/controller.go#L111), [controller.go#L139](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notification_controller/controller/controller.go#L139), [controller.go#L156](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notification_controller/controller/controller.go#L156), [util/glob/list.go#L20](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/glob/list.go#L20), [argocd_notification.go#L190](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-notification/commands/argocd_notification.go#L190), [controller_test.go#L254](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notification_controller/controller/controller_test.go#L254), [docs/operator-manual/app-any-namespace.md#L74](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/app-any-namespace.md#L74), [examples/k8s-rbac/argocd-server-applications/argocd-notifications-controller-rbac-clusterrole.yaml#L9](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/examples/k8s-rbac/argocd-server-applications/argocd-notifications-controller-rbac-clusterrole.yaml#L9)
- **Maturity**: GA
- **Semantics**: With an empty `--application-namespaces` list the Application client is namespaced to the controller namespace. With a non-empty list the client is cluster-wide; the informer's **List** keeps only items whose namespace equals the controller namespace or matches any list entry (`glob.MatchStringInList(..., REGEXP)`: entries wrapped in `/.../` are regular expressions, otherwise glob patterns such as `team-*`); the **Watch** is not filtered, so out-of-scope apps can enter the cache but are skipped at processing time with reason `app is not in one of the application-namespaces, nor the notification controller namespace`. Informer resync period 60 s (`resyncPeriod`). Apps in additional namespaces get notifications with the same config as controller-namespace apps (and additionally their own namespace config in self-service mode, NOTIF-054).
- **Edge cases**: list `["namespace1","namespace2"]` and app in `namespace3` → skipped (unit test); app in controller namespace is always in scope even if not listed; cluster-wide list/watch requires the example ClusterRole (`applications` get/list/watch/update/patch) — with only the default namespaced Role, listing fails and Init times out; regex entry `/^team-.*$/` matches `team-a`; comma-separated env value.
- **Configuration surface**: flag `--application-namespaces` (string slice); env `ARGOCD_APPLICATION_NAMESPACES` (comma separated); `argocd-cmd-params-cm` key `application.namespaces` (wired via env in the Deployment).
- **Exposure**: Component flag / ConfigMap only.
- **Tests required**:
  - positive: app in controller namespace notified with empty list.
  - positive: app in `team-a` notified with `--application-namespaces=team-*`; with `/^team-a$/` regex.
  - negative: app in unlisted namespace never notified (even though it appears in watch events).
  - authorization: without cluster-scoped RBAC the controller fails to start when namespaces are configured.

### NOTIF-003 Application label selector
- **Upstream**: [argocd_notification.go#L180](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-notification/commands/argocd_notification.go#L180), [controller.go#L162](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notification_controller/controller/controller.go#L162), [controller.go#L177](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notification_controller/controller/controller.go#L177)
- **Maturity**: GA
- **Semantics**: `--app-label-selector` (default empty = all) is passed as `LabelSelector` to both List and Watch of the Application informer; apps not matching the selector are invisible to the controller. The AppProject informer is not filtered.
- **Edge cases**: invalid selector syntax → API server rejects list → cache never syncs → startup failure; removing the matching label from an app makes the informer delete it (no further notifications, no "deleted" trigger evaluation for it).
- **Configuration surface**: flag `--app-label-selector` (no env var, no cmd-params key).
- **Exposure**: Component flag only.
- **Tests required**:
  - positive: `--app-label-selector=notify=true` → only labelled apps notified.
  - negative: unlabelled app never triggers.
  - negative: malformed selector → controller fails to initialise.

### NOTIF-004 Processor concurrency
- **Upstream**: [argocd_notification.go#L179](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-notification/commands/argocd_notification.go#L179), [argocd_notification.go#L173](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-notification/commands/argocd_notification.go#L173), [argocd_notification_test.go#L10](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-notification/commands/argocd_notification_test.go#L10), [argocd-notifications-controller-deployment.yaml#L62](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/notification/argocd-notifications-controller-deployment.yaml#L62), [docs/operator-manual/notifications/index.md#L73](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/index.md#L73), [docs/operator-manual/argocd-cmd-params-cm.yaml#L424](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cmd-params-cm.yaml#L424)
- **Maturity**: GA
- **Semantics**: Number of concurrent work-queue processors passed to the engine's `Run(processors, stop)`. Default 1. Env default parsed with bounds `[1, MaxInt32]`; out-of-range or non-numeric env value falls back to 1 (test: env `0` → 1, env `4` → 4). Explicit flag wins over env.
- **Edge cases**: `ARGOCD_NOTIFICATION_CONTROLLER_PROCESSORS_COUNT=0` → 1; `=abc` → 1; flag `--processors-count=0` is accepted by cobra as-is (no validation in Argo CD code; engine behaviour with 0 workers unconfirmed).
- **Configuration surface**: flag `--processors-count`; env `ARGOCD_NOTIFICATION_CONTROLLER_PROCESSORS_COUNT`; `argocd-cmd-params-cm` key `notificationscontroller.processors.count`.
- **Exposure**: Component flag / ConfigMap.
- **Tests required**:
  - positive: env `4` → flag default 4; cmd-params key propagates via env.
  - negative: env `0`/garbage → 1.
  - positive: N apps processed concurrently with processors>1 without duplicate deliveries per app.

### NOTIF-005 Logging configuration
- **Upstream**: [argocd_notification.go#L101](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-notification/commands/argocd_notification.go#L101), [argocd_notification.go#L181](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-notification/commands/argocd_notification.go#L181), [argocd-notifications-controller-deployment.yaml#L50](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/notification/argocd-notifications-controller-deployment.yaml#L50), [argocd-cmd-params-cm.yaml#L419](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cmd-params-cm.yaml#L419)
- **Maturity**: GA
- **Semantics**: `--loglevel` one of `debug|info|warn|error` (logrus parse; default `info`); `--logformat` `json` (default) or `text` (case-insensitive); with `text` and `FORCE_LOG_COLORS=1`, colours are forced. Debug level logs freshness-gate decisions (NOTIF-010). The Deployment also maps `log.format.timestamp` → `ARGOCD_LOG_FORMAT_TIMESTAMP` (not read in this command's code).
- **Edge cases**: unknown level → `failed to parse log level: ...`; unknown format → `unknown log format '<value>'`; both cause startup failure.
- **Configuration surface**: flags `--loglevel`, `--logformat`; env `ARGOCD_NOTIFICATIONS_CONTROLLER_LOGLEVEL`, `ARGOCD_NOTIFICATIONS_CONTROLLER_LOGFORMAT`, `FORCE_LOG_COLORS`; cmd-params keys `notificationscontroller.log.level`, `notificationscontroller.log.format`.
- **Exposure**: Component flag / ConfigMap.
- **Tests required**:
  - positive: JSON logs by default; text when configured.
  - negative: `--logformat=xml` → exact error `unknown log format 'xml'`.
  - negative: `--loglevel=verbose` → startup error.

### NOTIF-006 Notification ConfigMap/Secret names and informers
- **Upstream**: [argocd_notification.go#L188](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-notification/commands/argocd_notification.go#L188), [argocd_notification.go#L189](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-notification/commands/argocd_notification.go#L189), [util/notification/k8s/informers.go#L12](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/k8s/informers.go#L12), [informers.go#L16](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/k8s/informers.go#L16), [controller.go#L88](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notification_controller/controller/controller.go#L88), [controller.go#L102](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notification_controller/controller/controller.go#L102), [manifests/base/notification/argocd-notifications-cm.yaml#L1](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/notification/argocd-notifications-cm.yaml#L1), [argocd-notifications-secret.yaml#L1](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/notification/argocd-notifications-secret.yaml#L1)
- **Maturity**: GA
- **Semantics**: The controller reads one ConfigMap (default `argocd-notifications-cm`) and one Secret (default `argocd-notifications-secret`) through filtered informers (`fieldSelector metadata.name=<name>`, resync 3 min). Without self-service they watch only the controller namespace; with self-service, all namespaces (NOTIF-054). Config is re-parsed by the engine factory when these objects change, so edits take effect without restart (hot reload). Default install ships both objects empty (no triggers/templates/services) — nothing is sent until configured.
- **Edge cases**: missing ConfigMap/Secret → engine treats config as empty / not found (API server variant returns empty lists, NOTIF-101); custom names via flags are honoured by the controller only — the API server and `argocd admin notifications` always use the default names (NOTIF-101, NOTIF-104).
- **Configuration surface**: flags `--config-map-name`, `--secret-name` (no env vars).
- **Exposure**: ConfigMap/Secret.
- **Tests required**:
  - positive: custom `--config-map-name=my-cm` reads triggers from `my-cm`.
  - positive: editing the ConfigMap changes behaviour without restart.
  - negative: objects with other names in the namespace are ignored.
  - recovery: ConfigMap deleted then recreated → notifications resume.

### NOTIF-007 Repo-server connection (address, TLS, mTLS)
- **Upstream**: [argocd_notification.go#L125](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-notification/commands/argocd_notification.go#L125), [argocd_notification.go#L142](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-notification/commands/argocd_notification.go#L142), [argocd_notification.go#L184](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-notification/commands/argocd_notification.go#L184), [argocd_notification.go#L192](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-notification/commands/argocd_notification.go#L192), [util/tls/tls.go#L513](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/tls/tls.go#L513), [argocd-notifications-controller-deployment.yaml#L86](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/notification/argocd-notifications-controller-deployment.yaml#L86), [docs/operator-manual/mtls.md#L104](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/mtls.md#L104), [docs/operator-manual/argocd-cmd-params-cm.yaml#L428](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cmd-params-cm.yaml#L428)
- **Maturity**: GA
- **Semantics**: Repo-server is only needed for `repo.GetCommitMetadata` / `repo.GetAppDetails` (NOTIF-044/045). Address default `argocd-repo-server:8081`; client created with 5 s timeout parameter. TLS modes: `--argocd-repo-server-plaintext` disables TLS; `--repo-server-ca-cert-path` enables strict validation against that CA; mTLS client cert/key default to `/app/config/reposerver/mtls/client.{crt,key}` and are skipped if absent. Legacy strict mode (NOTIF-008) loads `<ARGOCD_APP_CONF_PATH|/app/config>/reposerver/tls/{tls.crt,ca.crt}`. Without CA and without plaintext → non-validating TLS. Deployment mounts `argocd-repo-server-tls` at `/app/config/reposerver/tls` and `argocd-repo-server-mtls` at `/app/config/reposerver/mtls` (both optional).
- **Edge cases**: only one of client cert/key set → `--repo-server-client-cert-key-path is required when --repo-server-client-cert-path is specified` (and vice-versa) → `failed to get repo-server client TLS configuration: ...`; unreadable CA → `error loading repo-server CA: ...`; repo-server down at runtime → only templates/triggers that call `repo.*` fail (NOTIF-044).
- **Configuration surface**: flags `--argocd-repo-server`, `--argocd-repo-server-plaintext`, `--repo-server-ca-cert-path`, `--repo-server-client-cert-path`, `--repo-server-client-cert-key-path`; env `ARGOCD_NOTIFICATION_CONTROLLER_REPO_SERVER_PLAINTEXT`, `ARGOCD_NOTIFICATION_CONTROLLER_REPO_SERVER_CA_CERT_PATH`, `ARGOCD_NOTIFICATION_CONTROLLER_REPO_SERVER_CLIENT_CERT_PATH`, `ARGOCD_NOTIFICATION_CONTROLLER_REPO_SERVER_CLIENT_CERT_KEY_PATH`, `ARGOCD_APP_CONF_PATH`; cmd-params keys `notificationscontroller.repo.server.plaintext`, `notificationscontroller.repo.server.ca.cert.path`, `notificationscontroller.repo.server.client.cert.path`, `notificationscontroller.repo.server.client.cert.key.path`.
- **Exposure**: Component flag / ConfigMap.
- **Tests required**:
  - positive: plaintext, non-validating TLS, CA-validated TLS and mTLS each allow `GetCommitMetadata`.
  - negative: wrong CA → template calling `repo.GetCommitMetadata` fails delivery; templates not calling repo still deliver.
  - negative: client cert without key → startup error text above.

### NOTIF-008 Deprecated `--argocd-repo-server-strict-tls`
- **Upstream**: [argocd_notification.go#L186](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-notification/commands/argocd_notification.go#L186), [argocd_notification.go#L130](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-notification/commands/argocd_notification.go#L130), [docs/operator-manual/upgrading/3.4-3.5.md#L229](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/3.4-3.5.md#L229)
- **Maturity**: Deprecated
- **Semantics**: Boolean, default false, marked deprecated (`use --argocd-repo-server-ca-cert-path instead` is the cobra deprecation message). OR-combined with CA-derived strict mode. When strict, TLS enabled and no CA pool loaded from `--repo-server-ca-cert-path`, the pool is loaded from `/app/config/reposerver/tls/tls.crt` + `ca.crt` (overridable root via `ARGOCD_APP_CONF_PATH`).
- **Edge cases**: files missing → `failed to load repo-server certificate pool: ...` at startup; using the flag prints the cobra deprecation warning; combined with `--argocd-repo-server-plaintext` → plaintext wins (DisableTLS set, strict pool not loaded).
- **Configuration surface**: flag `--argocd-repo-server-strict-tls` (no env).
- **Exposure**: Component flag.
- **Tests required**:
  - positive: flag + mounted secret → validated TLS.
  - negative: flag + no files → startup error.
  - positive: deprecation warning emitted.

### NOTIF-009 Kubernetes client defaults and user agent
- **Upstream**: [argocd_notification.go#L78](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-notification/commands/argocd_notification.go#L78), [argocd_notification.go#L84](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-notification/commands/argocd_notification.go#L84), [pkg/apis/application/v1alpha1/cluster_constants.go#L45](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/cluster_constants.go#L45)
- **Maturity**: GA
- **Semantics**: User agent `argocd-notifications-controller/<version> (<platform>)`. Argo CD shared client defaults are applied so the per-app PATCH persisting the notified annotation is not throttled: QPS 50 / burst 2×QPS (100) by default, tunable via `ARGOCD_K8S_CLIENT_QPS` / `ARGOCD_K8S_CLIENT_BURST` (plus the other shared transport settings).
- **Edge cases**: invalid defaults application → `failed to apply K8s client defaults: ...`.
- **Configuration surface**: env `ARGOCD_K8S_CLIENT_QPS`, `ARGOCD_K8S_CLIENT_BURST`, `ARGOCD_K8S_CLIENT_MAX_IDLE_CONNECTIONS`.
- **Exposure**: Component env only.
- **Tests required**:
  - positive: requests carry the documented user agent.
  - positive: 200 apps triggering simultaneously are all annotated without client-side throttling stalls at default settings.

### NOTIF-010 Sync-status freshness gate (skip stale apps)
- **Upstream**: [controller.go#L111](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notification_controller/controller/controller.go#L111), [controller.go#L238](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notification_controller/controller/controller.go#L238), [controller_test.go#L20](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notification_controller/controller/controller_test.go#L20)
- **Maturity**: GA
- **Semantics**: Before evaluating triggers for an Application the controller decides whether its sync status reflects the latest completed operation. Process (do not skip) when: no `status.operationState`; no `status.operationState.phase`; phase not in {`Failed`,`Error`,`Succeeded`} (e.g. `Running`); or `finishedAt` is not after both `status.reconciledAt` and `status.observedAt`. Skip with reason `sync status out of date` when: completed phase without `finishedAt`; `finishedAt` not RFC3339-parseable (warn log `Failed to parse FinishedAt '<v>'`); or `finishedAt` is strictly after both `reconciledAt` and `observedAt` (missing/unparseable timestamps count as zero time). The skip only defers evaluation — the next app update (after the app controller refreshes) is processed.
- **Edge cases**: finishedAt 01:05, reconciledAt 01:02, observedAt 01:04 → skip; finishedAt 01:02, reconciledAt 01:04, observedAt 01:06 → process; finishedAt equal to reconciledAt → process (not strictly after); `Running` phase always processed (so `on-sync-running` is not delayed).
- **Configuration surface**: none.
- **Exposure**: internal.
- **Tests required**:
  - positive: each of the four upstream table cases.
  - positive: `on-sync-succeeded` is not sent until `reconciledAt >= finishedAt`, then sent exactly once.
  - negative: garbage `finishedAt` → never processed until fixed.

### NOTIF-011 Custom CA certificates for outbound service HTTPS
- **Upstream**: [controller.go#L191](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notification_controller/controller/controller.go#L191), [util/cert/cert.go#L318](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/cert/cert.go#L318), [util/cert/cert.go#L90](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/cert/cert.go#L90), [argocd-notifications-controller-deployment.yaml#L21](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/notification/argocd-notifications-controller-deployment.yaml#L21), [docs/operator-manual/notifications/services/webhook.md#L81](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/webhook.md#L81)
- **Maturity**: GA
- **Semantics**: At `Init` the engine's HTTP utility is given a cert resolver backed by `argocd-tls-certs-cm` (mounted at `/app/config/tls`, path overridable via `ARGOCD_TLS_DATA_PATH`). For an outbound HTTPS host, PEM certificates stored under the key `<hostname>` (port stripped) are trusted in addition to system roots. Applies to engine services that build HTTP clients through the engine's `httputil` (documented for webhook; which other services use it is unconfirmed).
- **Edge cases**: host not present → system roots only (`x509: certificate signed by unknown authority` if self-signed); key containing invalid PEM → resolver error (`no certificates found in existing file` / parse error) → delivery fails; path traversal in hostname rejected (`could not get certificate for host ...`).
- **Configuration surface**: ConfigMap `argocd-tls-certs-cm` key `<hostname>`; env `ARGOCD_TLS_DATA_PATH`.
- **Exposure**: ConfigMap (also manageable via `argocd cert` in the repositories area).
- **Tests required**:
  - positive: webhook to self-signed HTTPS endpoint succeeds after adding its cert to `argocd-tls-certs-cm`.
  - negative: without the entry delivery fails with an x509 error and `succeeded=false` metric.

### NOTIF-012 Install manifests: RBAC, Service, NetworkPolicy, probes
- **Upstream**: [manifests/base/notification/argocd-notifications-controller-role.yaml#L9](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/notification/argocd-notifications-controller-role.yaml#L9), [argocd-notifications-controller-metrics-service.yaml#L10](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/notification/argocd-notifications-controller-metrics-service.yaml#L10), [argocd-notifications-controller-network-policy.yaml#L13](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/notification/argocd-notifications-controller-network-policy.yaml#L13), [argocd-notifications-controller-deployment.yaml#L111](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/notification/argocd-notifications-controller-deployment.yaml#L111), [examples/.../argocd-notifications-controller-rbac-clusterrole.yaml#L9](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/examples/k8s-rbac/argocd-server-applications/argocd-notifications-controller-rbac-clusterrole.yaml#L9), [docs/operator-manual/metrics.md#L393](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/metrics.md#L393)
- **Maturity**: GA
- **Semantics**: Namespaced Role: `applications`,`appprojects` get/list/watch/update/patch; `configmaps`,`secrets` list/watch; `get` only on `argocd-notifications-cm` and `argocd-notifications-secret`. Optional ClusterRole for apps-in-any-namespace / self-service: `applications` get/list/watch/update/patch and `secrets`,`configmaps` get/list/watch cluster-wide. Service `argocd-notifications-controller-metrics` port 9001 (`metrics`); NetworkPolicy allows ingress TCP 9001 from any namespace only; liveness probe TCP 9001; container hardened (non-root, read-only root FS, drop ALL, seccomp RuntimeDefault).
- **Edge cases**: custom `--config-map-name/--secret-name` require editing the Role's `resourceNames`; NetworkPolicy has no egress restriction (outbound delivery unrestricted).
- **Configuration surface**: manifests.
- **Exposure**: install manifests.
- **Tests required**:
  - authorization: controller works with exactly the shipped Role (patching apps' annotations succeeds).
  - authorization: removing `patch` on applications causes deliveries to repeat (state cannot be persisted) — see NOTIF-052.
  - positive: ServiceMonitor selecting `app.kubernetes.io/name: argocd-notifications-controller-metrics` scrapes metrics.

### NOTIF-013 Local timezone for time functions (`TZ`)
- **Upstream**: [docs/operator-manual/notifications/functions.md#L4](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/functions.md#L4), [util/notification/expression/time/time.go#L7](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/expression/time/time.go#L7)
- **Maturity**: GA
- **Semantics**: `time.Now()` / `.Local()` use the process local zone, set via the `TZ` env var on the controller container (e.g. `Asia/Tokyo`). Example: `{{ (call .time.Parse .app.status.operationState.startedAt).Local.Format "2006-01-02T15:04:05Z07:00" }}`.
- **Edge cases**: unset `TZ` in a distroless/UTC image → UTC; invalid `TZ` → Go falls back to UTC.
- **Configuration surface**: env `TZ` on `argocd-notifications-controller`.
- **Exposure**: Deployment env.
- **Tests required**:
  - positive: with `TZ=Asia/Tokyo`, `.Local.Format` renders `+09:00` offsets.
  - positive: without `TZ`, renders `Z`/UTC.

---

## B. Configuration model (`argocd-notifications-cm` / `argocd-notifications-secret`)

### NOTIF-014 ConfigMap key grammar, parsing and hot reload
- **Upstream**: [docs/operator-manual/notifications/argocd-notifications-cm.yaml#L1](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/argocd-notifications-cm.yaml#L1), [docs/operator-manual/notifications/services/overview.md#L1](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/overview.md#L1), [docs/operator-manual/notifications/troubleshooting-errors.md#L1](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/troubleshooting-errors.md#L1), [util/notification/settings/settings.go#L20](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/settings/settings.go#L20), [settings.go#L52](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/settings/settings.go#L52)
- **Maturity**: GA
- **Semantics**: Recognised `data` keys: `service.<type>` and `service.<type>.<custom-name>` (YAML service options, NOTIF-015), `template.<name>` (YAML template, NOTIF-030), `trigger.<name>` (YAML list of conditions, NOTIF-022), `defaultTriggers` (YAML list, NOTIF-028), `defaultTriggers.<service>` (YAML list, NOTIF-029), `subscriptions` (YAML list, NOTIF-051), `context` (YAML string map, NOTIF-017), plus the deprecated `config.yaml` (NOTIF-019). Secret `data` holds values referenced as `$key` (NOTIF-016) and the deprecated `notifiers.yaml` (NOTIF-020). The engine factory parses the pair into an in-memory config and Argo CD's `InitGetVars` hook (context + legacy merge + variable provider) runs on every (re)parse; changes to either object are picked up by the informers without restart.
- **Edge cases**: YAML syntax errors surface as "Failed to parse new settings" / `error converting YAML to JSON` (classic example: unquoted `icon: :rocket:`; must be `":rocket:"`); invalid `context` YAML or a non-string context value (e.g. `port: 8080`, unmarshalled into `map[string]string`) makes `InitGetVars` fail and therefore the whole config unusable; how the engine behaves for unknown keys and whether it keeps the last good config after a bad edit are unconfirmed (see Unconfirmed).
- **Configuration surface**: ConfigMap `argocd-notifications-cm` (or `--config-map-name`), Secret `argocd-notifications-secret` (or `--secret-name`).
- **Exposure**: ConfigMap/Secret; read-only views via API (NOTIF-101..103) and CLI (NOTIF-105, NOTIF-107).
- **Tests required**:
  - positive: config containing every key type parses; API lists match keys.
  - negative: `icon: :rocket:` unquoted → parse error logged, no deliveries from that config.
  - negative: `context: "port: 8080"` → config rejected.
  - recovery: fixing the YAML restores deliveries without restart.

### NOTIF-015 Service registration (`service.<type>[.<custom-name>]`) and custom names
- **Upstream**: [docs/operator-manual/notifications/services/overview.md#L1](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/overview.md#L1), [overview.md#L19](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/overview.md#L19), [argocd-notifications-cm.yaml#L43](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/argocd-notifications-cm.yaml#L43), [test/e2e/notification_test.go#L13](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/test/e2e/notification_test.go#L13), [server/notification/notification_test.go#L77](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/notification/notification_test.go#L77)
- **Maturity**: GA
- **Semantics**: `service.<type>` registers a service whose **name** equals `<type>`; `service.<type>.<custom-name>` registers a service named `<custom-name>` of that type, allowing several instances of one type (e.g. `service.slack.workspace1`, `service.slack.workspace2`, or a Slack-type notifier named `mattermost`). Subscriptions, default subscriptions and the API refer to the **name** (`service.webhook.test` is listed as `test`). Webhook services are always named (`service.webhook.<webhook-name>`) and templates address them by that name (NOTIF-069).
- **Edge cases**: unknown `<type>` → `service type 'xxxx' is not supported` (NOTIF-091); subscription to a name with no service → `notification service 'xxxx' is not supported`; two keys yielding the same name (e.g. `service.slack` and `service.webhook.slack`) — precedence unconfirmed.
- **Configuration surface**: ConfigMap keys `service.*`.
- **Exposure**: ConfigMap; API `ListServices` returns names; UI service autocomplete.
- **Tests required**:
  - positive: `service.webhook.test` → ListServices `[{name:"test"}]` (e2e parity).
  - positive: two Slack workspaces receive separate deliveries via `subscribe.<trigger>.workspace1` / `.workspace2`.
  - negative: `service.foo` → unsupported type error, other services still usable (unconfirmed isolation — verify against upstream).

### NOTIF-016 Secret references (`$key`) in service configuration
- **Upstream**: [docs/operator-manual/notifications/services/overview.md#L13](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/overview.md#L13), [argocd-notifications-cm.yaml#L44](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/argocd-notifications-cm.yaml#L44), [troubleshooting-errors.md#L85](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/troubleshooting-errors.md#L85), [docs/operator-manual/notifications/index.md#L86](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/index.md#L86)
- **Maturity**: GA
- **Semantics**: Any service option value may contain `$<secret-key>`, replaced by the value of that key in the notifications Secret (same namespace as the ConfigMap that references it; in self-service mode an app-namespace ConfigMap resolves against the app-namespace Secret). Works inside nested structures (maps such as `apiKeys`, `serviceKeys`, `recipientUrls`, header values like `token $github-token`). Keys may appear as map keys too (`$opsgenie-team-id: $opsgenie-team-api-key` in the sample).
- **Edge cases**: missing key → warning `config referenced xxx, but key does not exist in secret` and the literal is kept (documented troubleshooting); docs show a cross-secret form `$<secret-name>:<key>` with a labelled Secret — not confirmable in Argo CD code (see Unconfirmed); secret value whitespace handling unconfirmed.
- **Configuration surface**: Secret `argocd-notifications-secret` keys.
- **Exposure**: ConfigMap/Secret.
- **Tests required**:
  - positive: `token: $slack-token` resolved; header value `token $github-token` resolved inside a string.
  - negative: unknown key → warning text emitted, delivery fails at the remote (literal `$x` sent).
  - recovery: adding the key to the Secret later makes delivery succeed without restart.

### NOTIF-017 Shared template context (`context` key) and injected `notificationType`
- **Upstream**: [settings.go#L52](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/settings/settings.go#L52), [settings.go#L74](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/settings/settings.go#L74), [util/notification/settings/legacy.go#L229](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/settings/legacy.go#L229), [settings_test.go#L87](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/settings/settings_test.go#L87), [docs/operator-manual/notifications/templates.md#L29](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/templates.md#L29)
- **Maturity**: GA
- **Semantics**: ConfigMap key `context` holds a YAML map of string→string exposed as `.context` in templates and `context` in trigger expressions (e.g. `argocdUrl` used by every catalog template). For every destination Argo CD injects `context.notificationType = <destination service name>` and then copies the user map over it, so a user-defined `notificationType` key wins. Legacy `config.yaml` `context:` is merged on top (NOTIF-019).
- **Edge cases**: absent `context` key → `.context` contains only `notificationType`; `{{.context.argocdUrl}}` with no such key renders `<no value>`; non-string values make the config invalid (NOTIF-014); for a custom-named service `notificationType` is the custom name (e.g. `workspace1`), not the type.
- **Configuration surface**: ConfigMap key `context`.
- **Exposure**: ConfigMap.
- **Tests required**:
  - positive: `context: "argocdUrl: https://cd"` renders in templates and is usable in `when`.
  - positive: `.context.notificationType` equals destination service name; user-defined `notificationType` overrides it.
  - negative: missing key renders `<no value>` (match upstream text).

### NOTIF-018 Secrets in templates (`.secrets`)
- **Upstream**: [settings.go#L94](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/settings/settings.go#L94), [settings.go#L65](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/settings/settings.go#L65), [settings_test.go#L97](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/settings/settings_test.go#L97), [templates.md#L75](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/templates.md#L75), [index.md#L128](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/index.md#L128)
- **Maturity**: GA
- **Semantics**: When self-service is **disabled**, the variable map contains `secrets` = the notifications Secret's `data` (map of key → raw bytes), usable as `{{ .secrets.sampleWebhookToken }}` in templates (and `secrets` in trigger expressions). When self-service is **enabled**, `secrets` is omitted for every evaluation (controller-namespace and app-namespace configs alike).
- **Edge cases**: self-service on → `.secrets.x` renders `<no value>` / expression error; values are `[]byte` in Go — how they print in templates is unconfirmed (see Unconfirmed); the API server and CLI always use the non-self-service provider (secrets available).
- **Configuration surface**: Secret data; flag `--self-service-notification-enabled` disables it.
- **Exposure**: Secret/ConfigMap.
- **Tests required**:
  - positive: webhook body containing `{{ .secrets.token }}` renders the secret value (byte-for-byte parity with upstream output).
  - negative: with self-service enabled the same template does not expose the secret.
  - authorization: app-namespace templates in self-service mode can never read the controller-namespace Secret.

### NOTIF-019 Legacy `config.yaml` in the ConfigMap
- **Upstream**: [legacy.go#L20](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/settings/legacy.go#L20), [legacy.go#L55](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/settings/legacy.go#L55), [legacy.go#L76](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/settings/legacy.go#L76), [legacy.go#L172](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/settings/legacy.go#L172), [legacy_test.go#L16](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/settings/legacy_test.go#L16), [legacy_test.go#L47](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/settings/legacy_test.go#L47)
- **Maturity**: Deprecated
- **Semantics**: If `data["config.yaml"]` is non-empty, warn `Key 'config.yaml' in ConfigMap is deprecated, please migrate to new settings` and merge a legacy document `{triggers, templates, context, subscriptions}` on top of the new-format config:
  - `context`: JSON-merge-patched over the `context` key (legacy keys win).
  - `subscriptions`: JSON merge patch on a list → the legacy list **replaces** the new-format `subscriptions` entirely (test: new `[my-trigger1]` becomes legacy `[my-trigger2 selector test=true]`).
  - `templates[]` (`name`, `subject`→`email.subject`, `body`→`message`, plus inline notification fields): the legacy entry **replaces** any same-named template (the computed merge is discarded), e.g. `{message: foo}` + legacy `body: bar` → `{message: bar}`.
  - `triggers[]` (`name`, `condition`, `template`, `description`, `enabled`): if the trigger exists only its **first** condition is modified (non-empty legacy fields overwrite `when`, `send=[template]`, `description`); otherwise a one-condition trigger is created; `enabled: true` appends the name to `defaultTriggers`.
- **Edge cases**: malformed YAML → `error in ApplyLegacyConfig ...` / `error in unmarshaling legacy config: ...` and the config is unusable; `enabled: true` on a trigger already in `defaultTriggers` appends a duplicate; legacy trigger with no `condition` and no existing trigger yields an empty `when` (evaluation behaviour unconfirmed); legacy template overriding a richer new-format template drops its service sections.
- **Configuration surface**: ConfigMap key `config.yaml`.
- **Exposure**: ConfigMap.
- **Tests required**:
  - positive: port upstream `TestMergeLegacyConfig` and `TestMergeLegacyConfig_DefaultTriggers` expectations exactly.
  - positive: deprecation warning logged once per parse.
  - negative: invalid YAML → whole config rejected.

### NOTIF-020 Legacy `notifiers.yaml` in the Secret
- **Upstream**: [legacy.go#L42](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/settings/legacy.go#L42), [legacy.go#L131](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/settings/legacy.go#L131), [legacy.go#L162](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/settings/legacy.go#L162), [legacy_test.go#L82](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/settings/legacy_test.go#L82)
- **Maturity**: Deprecated
- **Semantics**: If Secret `data["notifiers.yaml"]` is non-empty, warn `Key 'notifiers.yaml' in Secret is deprecated, please migrate to new settings` and register services from `{email, slack, opsgenie, grafana: <options>, webhook: [{name, <webhook options>}]}`: `email`, `slack`, `opsgenie`, `grafana` register services with those fixed names; each webhook item registers a service named by its `name`. Legacy registration runs after the new-format parse, so it **overrides** same-named services from `service.*` keys.
- **Edge cases**: invalid YAML → `error unmarshaling legacy services config: ...` and config unusable; values are literal (no `$key` substitution — the file already lives in the Secret; engine behaviour for `$` here unconfirmed); webhook item without `name` registers under the empty name.
- **Configuration surface**: Secret key `notifiers.yaml`.
- **Exposure**: Secret.
- **Tests required**:
  - positive: `slack: {token: my-token}` registers service `slack` (upstream test asserts `cfg.Services["slack"]` non-nil).
  - positive: legacy `webhook: [{name: hook1, url: ...}]` usable as `subscribe.<t>.hook1`.
  - negative: malformed YAML rejects config.

### NOTIF-021 Legacy recipients annotation (`[<trigger>.]recipients.argocd-notifications.argoproj.io`)
- **Upstream**: [legacy.go#L187](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/settings/legacy.go#L187), [legacy.go#L191](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/settings/legacy.go#L191), [controller.go#L151](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notification_controller/controller/controller.go#L151), [legacy_test.go#L114](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/settings/legacy_test.go#L114)
- **Maturity**: Deprecated
- **Semantics**: Any annotation whose key **ends with** `recipients.argocd-notifications.argoproj.io` is a legacy subscription. The part before the suffix (trailing dots trimmed) is the trigger name; empty → all `defaultTriggers`. Value: comma-separated list (empty items dropped, items trimmed) of `service[:recipient]`; only the text between the first and second `:` is used as recipient. If `defaultTriggers.<service>` exists for the item's service, that list **replaces** the trigger list even when the key names a trigger explicitly. Argo CD applies this parser to the **AppProject** annotations of the app's project (merged into the app's destinations). Application-level legacy annotations are not handled by Argo CD code (engine handling unconfirmed).
- **Edge cases**: `my-trigger.recipients...: slack:my-channel` → `{my-trigger: [slack/my-channel]}`; bare key with `defaultTriggers=[my-trigger]` → `{my-trigger: [...]}`; with `defaultTriggers.slack=[trigger-a,trigger-b]` → both triggers; `email:a@b.com:extra` → recipient `a@b.com`; `webhookname` (no colon) → empty recipient; no default triggers and no trigger prefix → nothing.
- **Configuration surface**: AppProject annotations.
- **Exposure**: CRD (AppProject metadata).
- **Tests required**:
  - positive: port the three upstream `TestGetDestinations*` cases.
  - positive: project with legacy annotation notifies all its apps.
  - negative: recipient with extra colons truncated as upstream.

---

## C. Triggers

### NOTIF-022 Trigger definition (`trigger.<name>`: `when` + `send`)
- **Upstream**: [docs/operator-manual/notifications/triggers.md#L1](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/triggers.md#L1), [triggers.md#L6](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/triggers.md#L6), [argocd-notifications-cm.yaml#L6](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/argocd-notifications-cm.yaml#L6), [notifications_catalog/install.yaml#L476](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notifications_catalog/install.yaml#L476)
- **Maturity**: GA
- **Semantics**: `trigger.<name>` is a YAML list of conditions; each condition has `when` (boolean expression, NOTIF-024), `send` (list of template names rendered and delivered when the condition is true; each template typically provides service-specific parts), optional `oncePer` (NOTIF-025) and `description` (NOTIF-027). Triggers are evaluated per Application (after the freshness gate, NOTIF-010) for every trigger that has at least one destination (subscriptions, NOTIF-047..051); each evaluation increments `argocd_notifications_trigger_eval_total{name,triggered}`.
- **Edge cases**: `send` referencing an undefined template → delivery error (exact message unconfirmed); trigger referenced by a subscription but not defined → error/skip (unconfirmed); docs also show a single mapping instead of a list for `trigger.on-deployed` (acceptance unconfirmed).
- **Configuration surface**: ConfigMap key `trigger.<name>`.
- **Exposure**: ConfigMap; API `ListTriggers`; CLI `admin notifications trigger get|run`; UI trigger autocomplete.
- **Tests required**:
  - positive: `when: app.status.sync.status == 'Unknown'` + `send: [t1, t2]` delivers both templates' content.
  - negative: false condition → no delivery; eval metric `triggered="false"`.
  - negative: template missing → delivery failure counted `succeeded="false"`.

### NOTIF-023 Condition bundles (multiple conditions per trigger)
- **Upstream**: [triggers.md#L24](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/triggers.md#L24)
- **Maturity**: GA
- **Semantics**: A trigger may contain several conditions, each with its own `send` list, so one subscription covers several stages (e.g. `Succeeded` → `[github-commit-status]`, `Running` → `[github-commit-status]`, `Error|Failed` → `[app-sync-failed, github-commit-status]`). Conditions are evaluated independently; every true condition sends its templates; dedup state is kept per condition (engine; key format unconfirmed).
- **Edge cases**: two conditions true at once → both send; editing one condition's `when` must not re-send other conditions (depends on per-condition key — unconfirmed).
- **Configuration surface**: ConfigMap `trigger.<name>` list entries.
- **Exposure**: ConfigMap.
- **Tests required**:
  - positive: phase Running→Succeeded sends condition-2 then condition-1 templates, once each.
  - positive: phase Failed sends both `app-sync-failed` and `github-commit-status`.

### NOTIF-024 Trigger expression language (expr) and optional chaining
- **Upstream**: [triggers.md#L1](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/triggers.md#L1), [triggers.md#L47](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/triggers.md#L47), [triggers.md#L130](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/triggers.md#L130), [util/notification/expression/expr.go#L27](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/expression/expr.go#L27), [settings.go#L94](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/settings/settings.go#L94), [util/notification/expression/time/time_test.go#L48](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/expression/time/time_test.go#L48)
- **Maturity**: GA
- **Semantics**: `when` and `oncePer` are `expr-lang/expr` (v1.17.8) expressions evaluated against the variable map: `app` (Application as unstructured map), `appProject` (map, `{}` if unresolved), `context`, `secrets` (non-self-service only), and helper namespaces `time`, `strings`, `repo` (functions called directly: `time.Now().Sub(time.Parse(app.status?.operationState.startedAt)).Minutes() >= 5`). Supports `?.` optional chaining (`app.status?.operationState.phase` ≡ nil-guarded access), `in [...]`, `and/or/!`, `any(...)`, map indexing `app.metadata.annotations["k"]`.
- **Edge cases**: `app.status.operationState.phase` when `operationState` is absent → evaluation error (documented reason for `?.`); a panic inside a helper (e.g. `time.Parse` of a bad string, `repo.GetCommitMetadata` failure) turns into an evaluation error; non-boolean result handling unconfirmed; syntax errors → config/trigger error (timing — parse vs evaluation — unconfirmed).
- **Configuration surface**: `when`/`oncePer` strings.
- **Exposure**: ConfigMap; CLI `trigger run` evaluates it.
- **Tests required**:
  - positive: `time.Now().Truncate(time.Hour).Format(time.RFC3339)` with a fixed clock yields `2022-09-26T11:00:00Z` (upstream test).
  - positive: `?.` expression on an app without `operationState` evaluates false without error.
  - negative: non-`?.` access on missing section → evaluation error, no delivery, error logged.

### NOTIF-025 `oncePer` (send once per field value)
- **Upstream**: [triggers.md#L58](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/triggers.md#L58), [triggers.md#L80](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/triggers.md#L80), [triggers.md#L84](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/triggers.md#L84), [notifications_catalog/install.yaml#L490](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notifications_catalog/install.yaml#L490)
- **Maturity**: GA
- **Semantics**: Optional expression on a condition; the notification for that condition/destination is sent only once per distinct value of the expression (e.g. `app.status.sync.revision` — once per Git revision; `app.status?.operationState.syncResult.revision` — recommended for mono-repos so unrelated commits do not re-notify; `app.metadata.annotations["example.com/version"]`; `app.metadata.name` — effectively once ever). Unlike plain conditions, a value already notified is not re-armed when the condition becomes false (prevents flapping health from re-sending). State lives in the notified annotation (NOTIF-052).
- **Edge cases**: value evaluates to nil/empty (e.g. no operationState) → behaviour unconfirmed (likely treated as "no oncePer" key); mono-repo with `sync.revision` → a notification per commit to the repo even if the app did not change; state history size limits unconfirmed (a very old value could be re-sent after eviction).
- **Configuration surface**: `oncePer` field.
- **Exposure**: ConfigMap.
- **Tests required**:
  - positive: health flaps Healthy→Progressing→Healthy on the same revision → exactly one `on-deployed`.
  - positive: new revision → new notification.
  - recovery: controller restart does not resend for an already-notified value (state persisted).

### NOTIF-026 Default de-duplication and re-arming (no `oncePer`)
- **Upstream**: [triggers.md#L58](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/triggers.md#L58), [applicationset/controllers/applicationset_controller.go#L70](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/applicationset_controller.go#L70)
- **Maturity**: GA
- **Semantics**: A condition sends once when it becomes true and is not re-sent while it stays true across re-evaluations (60 s resync, every app update); when it evaluates false the state for that condition/destination is cleared, so the next false→true transition sends again ("flapping" re-sends, which `oncePer` exists to avoid). Each (trigger, condition, destination) pair is tracked independently, so adding a new recipient sends to that recipient on the next evaluation if the condition is currently true.
- **Edge cases**: `when: "true"` without oncePer → sent once, never again unless state is lost; adding a second destination to an already-true trigger notifies only the new destination (unconfirmed — derives from per-destination state).
- **Configuration surface**: none.
- **Exposure**: internal (state visible in annotation).
- **Tests required**:
  - positive: condition stays true over 5 resyncs → one delivery.
  - positive: true→false→true → two deliveries.
  - positive: new recipient added while true → delivered to new recipient only.

### NOTIF-027 Condition `description`
- **Upstream**: [notifications_catalog/install.yaml#L477](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notifications_catalog/install.yaml#L477), [docs/operator-manual/notifications/catalog.md#L8](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/catalog.md#L8), [legacy.go#L121](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/settings/legacy.go#L121)
- **Maturity**: GA
- **Semantics**: Free-text description per condition; informational only (shown by `trigger get` and used in generated catalog docs); legacy `description` maps onto the first condition.
- **Edge cases**: none functional.
- **Configuration surface**: `description` field.
- **Exposure**: ConfigMap; CLI `trigger get` output.
- **Tests required**:
  - positive: `trigger get -o yaml` echoes description; `wide` output includes it (column layout unconfirmed).

### NOTIF-028 `defaultTriggers`
- **Upstream**: [triggers.md#L100](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/triggers.md#L100), [argocd-notifications-cm.yaml#L39](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/argocd-notifications-cm.yaml#L39), [controller.go#L150](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notification_controller/controller/controller.go#L150), [legacy.go#L104](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/settings/legacy.go#L104)
- **Maturity**: GA
- **Semantics**: YAML list of trigger names used whenever a subscription does not name a trigger: `notifications.argoproj.io/subscribe.<service>` annotations (NOTIF-048) on apps and projects, legacy recipients annotations without trigger prefix (NOTIF-021), and legacy `enabled: true` triggers append to it.
- **Edge cases**: empty list + `subscribe.<service>` → no destinations; names that are not defined triggers → errors/no-op at evaluation (unconfirmed); whether default subscriptions without `triggers` use it is unconfirmed.
- **Configuration surface**: ConfigMap key `defaultTriggers`.
- **Exposure**: ConfigMap.
- **Tests required**:
  - positive: `defaultTriggers: [on-sync-status-unknown]` + `subscribe.slack: chan` notifies on Unknown status.
  - negative: no defaultTriggers → `subscribe.slack` produces nothing.

### NOTIF-029 Service-specific default triggers (`defaultTriggers.<service>`)
- **Upstream**: [triggers.md#L114](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/triggers.md#L114), [controller.go#L150](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notification_controller/controller/controller.go#L150), [legacy.go#L216](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/settings/legacy.go#L216), [legacy_test.go#L142](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/settings/legacy_test.go#L142)
- **Maturity**: GA
- **Semantics**: `defaultTriggers.<service-name>` overrides `defaultTriggers` for trigger-less subscriptions to that service (doc example: `subscribe.slack` uses `defaultTriggers` = `[on-sync-status-unknown]`, `subscribe.mattermost` uses `defaultTriggers.mattermost` = `[on-sync-running, on-sync-succeeded]`). In the legacy recipients parser it replaces the trigger list even when the annotation names a trigger.
- **Edge cases**: key uses the service **name** (custom name for `service.<type>.<name>`); empty list for a service → no triggers for that service.
- **Configuration surface**: ConfigMap keys `defaultTriggers.<service>`.
- **Exposure**: ConfigMap.
- **Tests required**:
  - positive: doc example yields slack on Unknown only and mattermost on running+succeeded only.
  - positive: legacy `TestGetDestinations_ServiceDefaultTriggers` parity.

---

## D. Templates and variables

### NOTIF-030 Template definition (`template.<name>`, `message`)
- **Upstream**: [docs/operator-manual/notifications/templates.md#L1](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/templates.md#L1), [argocd-notifications-cm.yaml#L19](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/argocd-notifications-cm.yaml#L19), [notifications_catalog/install.yaml#L3](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notifications_catalog/install.yaml#L3)
- **Maturity**: GA
- **Semantics**: `template.<name>` is a YAML object; `message` is a Go-template string used as the generic body for any service; additional top-level keys are service-specific sections (`email`, `slack`, `mattermost`, `teams`, `teams-workflows`, `github`, `webhook`, `googlechat`, `opsgenie`, `pagerduty`, `pagerdutyv2`, `newrelic`, `rocketchat`, `alertmanager`, `gcppubsub`, ...; NOTIF-034). Every string field in the template (including nested service fields such as Slack `attachments`, webhook `path`/`body`, GitHub `repoURLPath`) is itself a Go template rendered with the same variables. Templates are reusable by many triggers.
- **Edge cases**: template parse error (e.g. unclosed `{{`) → error at config parse or render (timing unconfirmed); referencing a missing field renders `<no value>`; a template with neither `message` nor a section for the destination's service sends an empty message (service-dependent; unconfirmed).
- **Configuration surface**: ConfigMap key `template.<name>`.
- **Exposure**: ConfigMap; API `ListTemplates`; CLI `admin notifications template get|notify`.
- **Tests required**:
  - positive: `message: "Application {{.app.metadata.name}} sync is {{.app.status.sync.status}}."` renders correctly for slack, email and webhook destinations.
  - negative: malformed template → no delivery, error logged.
  - positive: nested service field templating (e.g. `slack.attachments`) is rendered.

### NOTIF-031 Template variables (`app`, `appProject`, `context`, `secrets`, `serviceType`, `recipient`, helpers)
- **Upstream**: [templates.md#L19](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/templates.md#L19), [settings.go#L71](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/settings/settings.go#L71), [settings.go#L94](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/settings/settings.go#L94), [expr.go#L27](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/expression/expr.go#L27), [expr_test.go#L9](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/expression/expr_test.go#L9)
- **Maturity**: GA
- **Semantics**: Argo CD's provider returns: `app` (the Application object as delivered by the informer, unstructured), `appProject` (NOTIF-033), `context` (NOTIF-017), `secrets` (NOTIF-018, non-self-service only), and helper namespaces `time`, `strings` (shared, registered at init) and `repo` (bound per-app to the Argo CD service). The engine adds `serviceType` (service type name such as `slack`/`email`, for conditional rendering like `{{if eq .serviceType "slack"}}`) and `recipient` (destination recipient). In templates helpers are invoked with `call` (`{{ call .repo.RepoURLToHTTPS .app.spec.source.repoURL }}`).
- **Edge cases**: helper-namespace keys overwrite same-named user variables (Spawn copies helpers after vars); `.serviceType` for a custom-named service — type vs name unconfirmed (docs say type); `.recipient` is empty for recipient-less subscriptions (github/webhook).
- **Configuration surface**: none (provided).
- **Exposure**: templates/triggers.
- **Tests required**:
  - positive: `{{.serviceType}}` and `{{.recipient}}` render per destination.
  - positive: `time`, `strings`, `repo` namespaces are present (upstream `TestExpr`).
  - positive: catalog `{{if eq .serviceType "slack"}}:white_check_mark:{{end}}` prefix only on Slack.

### NOTIF-032 `app` object semantics (multi-source, status fields)
- **Upstream**: [settings.go#L72](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/settings/settings.go#L72), [troubleshooting-errors.md#L43](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/troubleshooting-errors.md#L43), [troubleshooting-errors.md#L64](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/troubleshooting-errors.md#L64), [docs/operator-manual/notifications/examples.md#L20](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/examples.md#L20)
- **Maturity**: GA
- **Semantics**: `app` is the raw Application JSON (spec, status, metadata, operation) exactly as stored; templates must therefore address multi-source apps through `.app.spec.sources` / `.app.status.operationState.syncResult.revisions` (e.g. `{{ (index .app.spec.sources 0).repoURL }}`, `{{index .app.status.operationState.syncResult.revisions 0}}`). Sprig `toJson` can serialise sub-objects such as `.app.status.operationState.syncResult` (resources with `images`, `message`, `hookPhase`, ...).
- **Edge cases**: single-source defaults (`{{.app.spec.source.repoURL}}`) on a multi-source app render `<no value>`, causing GitHub error `GitHub.repoURL (<no value>) does not have a /` or `POST .../statuses/: 404 Not Found`.
- **Configuration surface**: none.
- **Exposure**: templates/triggers.
- **Tests required**:
  - positive: `{{toJson .app.status.operationState.syncResult}}` webhook body matches upstream byte-for-byte for a fixture app.
  - negative: multi-source app with default GitHub template → documented error.

### NOTIF-033 `appProject` variable
- **Upstream**: [settings.go#L77](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/settings/settings.go#L77), [settings.go#L116](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/settings/settings.go#L116), [util/notification/argocd/service.go#L138](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/argocd/service.go#L138), [controller.go#L99](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notification_controller/controller/controller.go#L99), [settings_test.go#L120](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/settings/settings_test.go#L120), [service_test.go#L34](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/argocd/service_test.go#L34), [templates.md#L48](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/templates.md#L48)
- **Maturity**: GA
- **Semantics**: For every evaluation, `appProject` is the full AppProject object (unstructured map: metadata, spec incl. `description`, `sourceRepos`, `roles[].policies`, destinations) named by `app.spec.project` (empty/absent → `default`), **always looked up in the controller namespace** (also for apps in other namespaces). Lookup order: AppProject informer cache in the controller (deep copy) → live GET fallback (CLI, or cache miss), with a 5 s timeout. If not found or any error → `appProject` is `{}` and a warning `Failed to get AppProject for notification template: <err>` is logged with app/namespace/project fields. The key is always present.
- **Edge cases**: project missing → `{{.appProject.metadata.name}}` renders `<no value>`, `appProject.spec` in `when` → nil (use `?.`); app with `spec` not a map → `{}`; cache served objects must not be mutated (deep-copied).
- **Configuration surface**: none.
- **Exposure**: templates/triggers.
- **Tests required**:
  - positive: template prints project description and iterates `.appProject.spec.roles`.
  - positive: app in namespace `team-a` with project `p` in controller namespace resolves `p`.
  - negative: nonexistent project → `{}` and warning; delivery still happens.
  - positive: cached lookup performs no API GET (upstream test asserts zero dynamic-client actions).

### NOTIF-034 Service-specific template sections (general contract)
- **Upstream**: [templates.md#L108](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/templates.md#L108), [argocd-notifications-cm.yaml#L24](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/argocd-notifications-cm.yaml#L24), [services/overview.md#L39](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/overview.md#L39)
- **Maturity**: GA
- **Semantics**: A single template can carry sections for several services; at delivery time only `message` plus the section matching the destination's service type is used (e.g. `email.subject` for email, `slack.attachments` for Slack, `webhook.<name>` for a named webhook). When several templates are listed in `send`, they are combined into one notification per destination (engine merges templates; merge precedence unconfirmed).
- **Edge cases**: template lacking the section for the destination type → only `message` is used; for webhook a template without a `webhook.<name>` entry for that webhook sends nothing/empty request (unconfirmed).
- **Configuration surface**: template sections.
- **Exposure**: ConfigMap.
- **Tests required**:
  - positive: one template with `email` + `slack` sections → email gets subject, Slack gets attachments, both get message.
  - positive: `send: [a, b]` where `a` has `slack` and `b` has `email` sections → both parts applied to the respective destinations.

### NOTIF-035 Sprig template function library
- **Upstream**: [templates.md#L119](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/templates.md#L119), [docs/operator-manual/upgrading/2.6-2.7.md#L45](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/2.6-2.7.md#L45), [go.mod#L14](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/go.mod#L14), [examples.md#L20](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/examples.md#L20)
- **Maturity**: GA
- **Semantics**: Templates have the Sprig v3 function set (e.g. `toJson`, `upper`, `default`, `semverCompare` with Masterminds/semver v3 `^` semantics) in addition to Go template built-ins (`if`, `range`, `index`, `eq`, `ne`, `call`).
- **Edge cases**: `^` semver constraint semantics changed with Sprig v3 (2.7 upgrade note); functions that panic produce render errors.
- **Configuration surface**: none.
- **Exposure**: templates.
- **Tests required**:
  - positive: `{{ .app.metadata.name | upper }}`, `{{ toJson .app.status.sync }}`, `{{ semverCompare "^1.2.0" "1.3.0" }}` match upstream output.

---

## E. Expression / template helper functions

### NOTIF-036 `time.Now`
- **Upstream**: [util/notification/expression/time/time.go#L7](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/expression/time/time.go#L7), [time.go#L13](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/expression/time/time.go#L13), [time_test.go#L48](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/expression/time/time_test.go#L48), [functions.md#L35](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/functions.md#L35)
- **Maturity**: GA
- **Semantics**: Returns current Go `time.Time` (local zone, NOTIF-013); all Go `Time` methods are callable (`Sub`, `Add`, `Before`, `Truncate`, `Format`, `Unix`, `Local`, ...). In templates: `{{ (call .time.Now).Unix }}`.
- **Edge cases**: comparisons with `time.Parse` results across zones are absolute-instant comparisons.
- **Configuration surface**: none.
- **Exposure**: triggers/templates.
- **Tests required**:
  - positive: upstream fixed-clock test.
  - positive: `time.Now().Sub(time.Parse(x)).Minutes() >= 5` true for x 10 minutes ago.

### NOTIF-037 `time.Parse`
- **Upstream**: [time.go#L41](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/expression/time/time.go#L41), [functions.md#L41](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/functions.md#L41)
- **Maturity**: GA
- **Semantics**: `time.Parse(s)` parses **RFC3339 only** and returns `time.Time`.
- **Edge cases**: any non-RFC3339 input (including empty string or nil-derived `""`) panics → evaluation/render error (e.g. catalog `on-deployed` on an app whose `health.lastTransitionTime` is missing); no layout argument is accepted.
- **Configuration surface**: none.
- **Exposure**: triggers/templates.
- **Tests required**:
  - positive: `time.Parse("2021-01-01T01:02:00Z")` works.
  - negative: `time.Parse("2021-01-01")` → evaluation error, trigger not fired.

### NOTIF-038 `time` duration and layout constants
- **Upstream**: [time.go#L14](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/expression/time/time.go#L14), [time_test.go#L12](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/expression/time/time_test.go#L12), [functions.md#L46](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/functions.md#L46)
- **Maturity**: GA
- **Semantics**: Durations `time.Nanosecond`, `Microsecond`, `Millisecond`, `Second`, `Minute`, `Hour` (Go `time.Duration`, usable as `1 * time.Minute`); layouts `time.Layout`, `ANSIC`, `UnixDate`, `RubyDate`, `RFC822`, `RFC822Z`, `RFC850`, `RFC1123`, `RFC1123Z`, `RFC3339`, `RFC3339Nano`, `Kitchen`, `Stamp`, `StampMilli`, `StampMicro`, `StampNano` with Go's exact strings.
- **Edge cases**: no `time.Day`; no `DateTime`/`DateOnly`/`TimeOnly` constants.
- **Configuration surface**: none.
- **Exposure**: triggers/templates.
- **Tests required**:
  - positive: all 22 names exist (upstream `TestNewTimeExprs`).
  - negative: `time.DateOnly` undefined → error.

### NOTIF-039 `strings.ReplaceAll`
- **Upstream**: [util/notification/expression/strings/strings.go#L9](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/expression/strings/strings.go#L9), [strings_test.go#L23](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/expression/strings/strings_test.go#L23), [functions.md#L87](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/functions.md#L87)
- **Maturity**: GA
- **Semantics**: `strings.ReplaceAll(s, old, new)` = Go `strings.ReplaceAll` (all non-overlapping occurrences).
- **Edge cases**: empty `old` inserts `new` between every rune (Go semantics).
- **Configuration surface**: none.
- **Exposure**: triggers/templates.
- **Tests required**:
  - positive: `test_replace`,`_`,`=` → `test=replace`.

### NOTIF-040 `strings.ToUpper` / `strings.ToLower`
- **Upstream**: [strings.go#L10](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/expression/strings/strings.go#L10), [strings_test.go#L34](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/expression/strings/strings_test.go#L34), [functions.md#L92](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/functions.md#L92)
- **Maturity**: GA
- **Semantics**: Unicode-aware case conversion (Go `strings.ToUpper/ToLower`). Only these three `strings` functions exist (no `Contains`, `HasPrefix`, ... in this namespace; expr built-ins like `contains`/`startsWith` remain available in triggers).
- **Edge cases**: `strings.Contains` → undefined function error.
- **Configuration surface**: none.
- **Exposure**: triggers/templates.
- **Tests required**:
  - positive: `test`→`TEST`, `TEST`→`test`.
  - negative: `strings.Contains(...)` fails like upstream.

### NOTIF-041 `repo.RepoURLToHTTPS`
- **Upstream**: [util/notification/expression/repo/repo.go#L86](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/expression/repo/repo.go#L86), [repo.go#L98](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/expression/repo/repo.go#L98), [functions.md#L110](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/functions.md#L110)
- **Maturity**: GA
- **Semantics**: Parses any git URL form (scp-like `git@host:org/repo.git`, `ssh://`, `https://`, `git://`) with `chainguard-dev/git-urls`, forces scheme `https`, drops userinfo, keeps host/port/path (including a `.git` suffix).
- **Edge cases**: `git@github.com:argoproj/argo-cd.git` → `https://github.com/argoproj/argo-cd.git`; `https://user:pw@host/x` → `https://host/x`; `ssh://git@host:2222/x` keeps `:2222`; unparseable input panics → render/eval error.
- **Configuration surface**: none.
- **Exposure**: triggers/templates.
- **Tests required**:
  - positive: the conversions above.
  - negative: garbage URL → render error, delivery counted failed.

### NOTIF-042 `repo.FullNameByRepoURL`
- **Upstream**: [repo.go#L72](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/expression/repo/repo.go#L72), [functions.md#L115](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/functions.md#L115), [services/webhook.md#L139](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/webhook.md#L139)
- **Maturity**: GA
- **Semantics**: Parses the URL, strips a trailing `.git`, splits the path on `/` dropping empties, returns the **first two** segments joined by `/` (`owner/repo`); if fewer than two segments, returns the stripped path as-is. Host-agnostic (works for any provider).
- **Edge cases**: GitLab subgroup `https://gitlab.com/group/sub/repo.git` → `group/sub` (truncated); `https://host/repo.git` → `/repo` (leading slash kept); unparseable → panic → error.
- **Configuration surface**: none.
- **Exposure**: triggers/templates.
- **Tests required**:
  - positive: `https://github.com/argoproj/argo-cd.git` and `git@github.com:argoproj/argo-cd.git` → `argoproj/argo-cd`.
  - edge: subgroup and single-segment cases above.

### NOTIF-043 `repo.QueryEscape`
- **Upstream**: [repo.go#L100](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/expression/repo/repo.go#L100), [functions.md#L120](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/functions.md#L120)
- **Maturity**: GA
- **Semantics**: Go `url.QueryEscape` (space → `+`, `/` → `%2F`), e.g. GitLab project paths in API URLs.
- **Edge cases**: not path-escaping (space becomes `+`).
- **Configuration surface**: none.
- **Exposure**: triggers/templates.
- **Tests required**:
  - positive: `argoproj/argo cd` → `argoproj%2Fargo+cd`.

### NOTIF-044 `repo.GetCommitMetadata(sha)`
- **Upstream**: [repo.go#L49](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/expression/repo/repo.go#L49), [repo.go#L101](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/expression/repo/repo.go#L101), [service.go#L65](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/argocd/service.go#L65), [util/notification/expression/shared/appdetail.go#L11](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/expression/shared/appdetail.go#L11), [functions.md#L130](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/functions.md#L130)
- **Maturity**: GA
- **Semantics**: Returns `{Message, Author, Date (time.Time), Tags []string}` for the given revision of the app's `spec.source.repoURL`, using the repository credentials resolved from Argo CD's repo DB for (repoURL, `spec.project`) and the repo-server `GetRevisionMetadata` RPC. Typical use: `{{(call .repo.GetCommitMetadata .app.status.sync.revision).Author}}`.
- **Edge cases**: app without `spec.source.repoURL` (multi-source) → panic `failed to get application source repo URL` → error; missing `spec.project` → panic `failed to get application project`; repo-server unreachable / unknown revision → error; called per render (no caching in Argo CD code) so expensive in large fan-outs.
- **Configuration surface**: repo-server connection (NOTIF-007); repository credentials in Argo CD.
- **Exposure**: triggers/templates.
- **Tests required**:
  - positive: author/message/date/tags returned for a known commit of a private repo (credentials used).
  - negative: multi-source app → render error text contains `failed to get application source repo URL`.
  - negative: repo-server down → delivery fails, other templates unaffected.

### NOTIF-045 `repo.GetAppDetails()`
- **Upstream**: [repo.go#L37](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/expression/repo/repo.go#L37), [repo.go#L109](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/expression/repo/repo.go#L109), [service.go#L86](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/argocd/service.go#L86), [appdetail.go#L22](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/expression/shared/appdetail.go#L22), [appdetail.go#L38](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/expression/shared/appdetail.go#L38), [appdetail_test.go#L12](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/expression/shared/appdetail_test.go#L12), [functions.md#L140](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/functions.md#L140)
- **Maturity**: GA
- **Semantics**: Calls repo-server `GetAppDetails` for **source index 0** (works for single- and multi-source apps) with repo credentials, Helm repositories, Kustomize and Helm settings from Argo CD. Returns `{Type, Helm, Kustomize, Directory}` where `Helm` (if Helm) is `{HelmAppSpec{Name, ValueFiles, Parameters, Values, FileParameters}, HelmParameterOverrides}` with methods `GetParameterValueByName(name)` (checks `spec.source.helm.parameters` overrides first, then chart parameters; `""` if absent) and `GetFileParameterPathByName(name)`.
- **Edge cases**: Helm app whose `spec.source.helm` is nil → Argo CD code dereferences `appSource.Helm.Parameters` → runtime panic → render error (code-reading finding); repo lookup/settings errors → error; Kustomize/Directory apps have `Helm` nil.
- **Configuration surface**: repo-server connection; `argocd-cm` Helm/Kustomize settings.
- **Exposure**: triggers/templates.
- **Tests required**:
  - positive: `{{ (call .repo.GetAppDetails).Helm.GetParameterValueByName "image.tag" }}` returns override value when overridden (upstream test: `value-override`).
  - positive: Type `Kustomize` for a kustomize app.
  - negative: Helm app with no `helm:` block — match upstream failure.

### NOTIF-046 `sync.GetInfoItem` (documented only)
- **Upstream**: [functions.md#L101](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/functions.md#L101), [expr.go#L17](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/expression/expr.go#L17), [expr.go#L27](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/expression/expr.go#L27)
- **Maturity**: GA (documented) — **not registered in code at this commit**; treat as Unknown.
- **Semantics**: Docs: `sync.GetInfoItem(app, name) string` returns a sync-operation `info` item value. Code registers only `time`, `strings` (global) and `repo` (per app); no `sync` namespace exists, so expressions/templates using it fail.
- **Edge cases**: `sync.GetInfoItem(app, "x")` in a trigger → undefined-variable error; in a template `{{ call .sync.GetInfoItem ... }}` → render error.
- **Configuration surface**: none.
- **Exposure**: triggers/templates (doc only).
- **Tests required**:
  - negative: parity — using `sync.GetInfoItem` produces an evaluation/render error (unless Patrol deliberately ships it as a documented superset; record decision).

---

## F. Subscriptions

### NOTIF-047 Application subscription annotation `notifications.argoproj.io/subscribe.<trigger>.<service>`
- **Upstream**: [docs/operator-manual/notifications/subscriptions.md#L1](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/subscriptions.md#L1), [docs/user-guide/subscriptions.md#L1](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/subscriptions.md#L1), [index.md#L41](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/index.md#L41), [services/github.md#L55](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/github.md#L55), [services/webhook.md#L71](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/webhook.md#L71)
- **Maturity**: GA
- **Semantics**: Annotation key `notifications.argoproj.io/subscribe.<trigger>.<service>` on an Application subscribes it; value is a `;`-separated recipient list (e.g. `my-channel1;my-channel2`), each recipient becoming its own destination. Recipient meaning is service-specific (Slack channel, email address, Opsgenie team, PagerDuty service ID, Telegram chat, Google Chat webhook name, Teams channel name, ...). Services that need no recipient (GitHub, webhook, AWS SQS default queue, GCP Pub/Sub default topic) use an empty value `""`. `<service>` is the service **name** (custom name for `service.<type>.<name>`).
- **Edge cases**: whitespace around recipients (trimming unconfirmed — UI placeholder shows `my-channel1; my-channel2`); empty segments; trigger names containing dots cannot be expressed (key split on `.`); annotation for an undefined trigger or service → error at delivery (NOTIF-055); removing the annotation stops future deliveries (state entries remain in the notified annotation).
- **Configuration surface**: Application annotation.
- **Exposure**: CRD (Application metadata); API (Application update/patch); UI (NOTIF-109); `kubectl patch` per docs.
- **Tests required**:
  - positive: two Slack channels each receive one message on `on-sync-succeeded`.
  - positive: webhook subscription with empty value delivers.
  - negative: subscription to undefined service → `notification service '<name>' is not supported`, delivery metric `succeeded="false"`.
  - authorization: users who can update the Application (Argo CD RBAC `applications, update`) can add subscriptions via API/UI; read-only users cannot.

### NOTIF-048 Trigger-less subscription `notifications.argoproj.io/subscribe.<service>`
- **Upstream**: [triggers.md#L119](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/triggers.md#L119), [controller.go#L150](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notification_controller/controller/controller.go#L150)
- **Maturity**: GA
- **Semantics**: Annotation with only the service segment subscribes the recipients to `defaultTriggers.<service>` if defined, otherwise to `defaultTriggers` (NOTIF-028/029).
- **Edge cases**: no default triggers configured → no destinations; UI regex requires both trigger and service segments, so these annotations are shown only in the raw ANNOTATIONS editor.
- **Configuration surface**: Application/AppProject annotation.
- **Exposure**: CRD.
- **Tests required**:
  - positive: doc example (`subscribe.slack` + `subscribe.mattermost`) yields the documented trigger split.
  - negative: no defaultTriggers → nothing sent.

### NOTIF-049 YAML subscriptions annotation `notifications.argoproj.io/subscriptions`
- **Upstream**: [docs/operator-manual/notifications/services/slack.md#L73](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/slack.md#L73)
- **Maturity**: GA
- **Semantics**: A single annotation holding a YAML list of `{trigger: [names...], destinations: [{service: <name>, recipients: [..]}]}` entries, letting one annotation define several triggers × several services × several recipients.
- **Edge cases**: invalid YAML → subscriptions from this annotation ignored with error log (unconfirmed); empty `trigger` list semantics (default triggers?) unconfirmed; interplay/duplication with `subscribe.*` keys for the same trigger/service/recipient (dedup unconfirmed).
- **Configuration surface**: Application annotation.
- **Exposure**: CRD.
- **Tests required**:
  - positive: doc example produces deliveries to 2 Slack channels + 3 emails for the first trigger group and 2 Slack channels for the second.
  - negative: malformed YAML does not break other subscription annotations.

### NOTIF-050 AppProject-level subscriptions
- **Upstream**: [subscriptions.md#L18](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/subscriptions.md#L18), [controller.go#L143](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notification_controller/controller/controller.go#L143), [controller.go#L209](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notification_controller/controller/controller.go#L209), [controller.go#L87](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notification_controller/controller/controller.go#L87), [controller_test.go#L105](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notification_controller/controller/controller_test.go#L105), [controller_test.go#L131](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notification_controller/controller/controller_test.go#L131), [controller_test.go#L155](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notification_controller/controller/controller_test.go#L155)
- **Maturity**: GA
- **Semantics**: For every app, the controller's `alterDestinations` hook looks up its project (`spec.project`, empty → `default`) in the AppProject informer keyed `<controller-namespace>/<project>` (never the app namespace — fixes apps-in-any-namespace, argoproj/argo-cd#28137), deep-copies it, and merges into the app's destinations: (1) the project's `notifications.argoproj.io/subscribe.*` annotations (whether the engine parser also reads the YAML `notifications.argoproj.io/subscriptions` form on projects is unconfirmed), resolved with `defaultTriggers`/`defaultTriggers.<service>`; (2) the project's legacy recipients annotations (NOTIF-021). Thus project subscriptions apply to all of the project's apps, in addition to their own.
- **Edge cases**: project not in cache → no project destinations (silently); project without annotations → treated as empty; `spec.project` not a string → nil; same destination via app and project → duplicate suppression unconfirmed (state key identical, so likely one delivery).
- **Configuration surface**: AppProject annotations.
- **Exposure**: CRD (AppProject metadata); API (project update); no dedicated UI editor.
- **Tests required**:
  - positive: project annotation notifies every app of the project, including one in a non-controller namespace.
  - positive: app with no `spec.project` inherits `default` project subscriptions.
  - negative: app of another project not notified.
  - authorization: only users allowed to update the AppProject can set project-wide subscriptions.

### NOTIF-051 Default (global) subscriptions in the ConfigMap
- **Upstream**: [subscriptions.md#L28](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/subscriptions.md#L28), [subscriptions.md#L56](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/subscriptions.md#L56), [argocd-notifications-cm.yaml#L82](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/argocd-notifications-cm.yaml#L82), [legacy.go#L80](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/settings/legacy.go#L80)
- **Maturity**: GA
- **Semantics**: ConfigMap key `subscriptions` holds a YAML list of `{recipients: ["<service>:<recipient>" | "<service>"], triggers: [..], selector: "<label selector>"}`; each entry applies to every Application (in scope of this config) whose labels match `selector` (absent → all apps). For named webhooks the recipient is just `<webhook-name>`.
- **Edge cases**: invalid selector → config parse error (unconfirmed); legacy `config.yaml` `subscriptions` replaces this list (NOTIF-019); entry without `triggers` → default triggers? (unconfirmed); recipient containing `:` (email) is split on the first colon only (unconfirmed for new format).
- **Configuration surface**: ConfigMap key `subscriptions`.
- **Exposure**: ConfigMap.
- **Tests required**:
  - positive: unlabelled subscription notifies all apps; `selector: test=true` only labelled ones.
  - positive: `- <webhook-name>` recipient form delivers to that webhook.
  - negative: app not matching selector receives nothing.

---

## G. Delivery semantics and multi-tenancy

### NOTIF-052 Delivery state annotation `notified.notifications.argoproj.io`
- **Upstream**: [argocd_notification.go#L79](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-notification/commands/argocd_notification.go#L79), [applicationset/controllers/applicationset_controller.go#L70](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/applicationset_controller.go#L70), [applicationset_controller.go#L84](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/applicationset_controller.go#L84), [manifests/base/config/argocd-cm.yaml#L18](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/config/argocd-cm.yaml#L18), [argocd-notifications-controller-role.yaml#L10](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/notification/argocd-notifications-controller-role.yaml#L10)
- **Maturity**: GA
- **Semantics**: Which (trigger, condition, oncePer value, destination) combinations were already notified is persisted on each Application as the annotation `notified.notifications.argoproj.io` via a PATCH by the notifications controller (hence `update/patch` on `applications` in its Role and the raised client QPS). Integrations protect it: the ApplicationSet controller preserves this annotation when regenerating Applications (`defaultPreservedAnnotations`), and the default `argocd-cm` `resource.customizations.ignoreResourceUpdates.argoproj.io_Application` (and `_Rollout`) ignores changes to it so annotation writes do not cause refreshes of parent apps. Restarting the controller therefore does not resend.
- **Edge cases**: annotation value format (JSON map of key → unix timestamp) and history truncation are engine-internal (unconfirmed); deleting the annotation re-sends every currently-true condition; PATCH forbidden → state not persisted → repeated sends each evaluation (inferred); apps managed by an ApplicationSet keep state across regeneration.
- **Configuration surface**: Application annotation (written by controller).
- **Exposure**: CRD (Application metadata).
- **Tests required**:
  - positive: after a delivery the annotation exists and controller restart causes no resend.
  - recovery: deleting the annotation causes exactly one resend for currently-true conditions.
  - positive: ApplicationSet regeneration keeps the annotation; parent app-of-apps is not refreshed by annotation writes.
  - authorization: without `patch` RBAC, behaviour matches upstream (repeated sends + errors).

### NOTIF-053 Delivery failures and redelivery
- **Upstream**: [troubleshooting-errors.md#L37](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/troubleshooting-errors.md#L37), [monitoring.md#L11](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/monitoring.md#L11), [services/webhook.md#L22](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/webhook.md#L22), [services/github.md#L118](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/github.md#L118), [controller.go#L38](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notification_controller/controller/controller.go#L38)
- **Maturity**: GA
- **Semantics**: A failed send is logged under "Failed to notify recipient ..." and counted `argocd_notifications_deliveries_total{succeeded="false"}`. Only the webhook service has built-in HTTP retries (NOTIF-070). GitHub commit-status 422 after 1000 statuses per SHA/context is ignored and treated as completed. Whether a failed delivery is retried on the next evaluation (60 s resync / next app change) because it is not recorded as notified is engine-internal (unconfirmed; expected yes).
- **Edge cases**: permanently failing destination generates a failure per evaluation cycle (if retried); one failing destination does not block other destinations of the same trigger (expected; unconfirmed).
- **Configuration surface**: none (webhook retry options in NOTIF-070).
- **Exposure**: logs/metrics.
- **Tests required**:
  - negative: Slack 5xx → `succeeded="false"` metric and error log.
  - recovery: remote recovers → notification delivered on a subsequent evaluation (verify upstream parity).
  - positive: second destination unaffected by first destination's failure.

### NOTIF-054 Self-service (namespace-based) notification configuration
- **Upstream**: [index.md#L49](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/index.md#L49), [index.md#L86](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/index.md#L86), [index.md#L124](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/index.md#L124), [controller.go#L88](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notification_controller/controller/controller.go#L88), [controller.go#L124](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notification_controller/controller/controller.go#L124), [settings.go#L24](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/settings/settings.go#L24), [argocd_notification.go#L191](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-notification/commands/argocd_notification.go#L191), [argocd-notifications-controller-deployment.yaml#L80](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/notification/argocd-notifications-controller-deployment.yaml#L80), [argocd-cmd-params-cm.yaml#L426](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cmd-params-cm.yaml#L426), [controller_test.go#L187](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notification_controller/controller/controller_test.go#L187)
- **Maturity**: GA
- **Semantics**: With `--self-service-notification-enabled` the ConfigMap/Secret informers watch the configured names in **all** namespaces and the engine controller is built "with namespace support": each Application is processed with the controller-namespace config **and**, if present, the `argocd-notifications-cm`/`-secret` in the Application's own namespace (requires apps-in-any-namespace via `--application-namespaces`). Each config resolves its own services, templates, triggers, `$secret` references and subscriptions; when the same service and trigger exist at both levels, both notifications are sent according to their own configuration. The `secrets` template variable is disabled for all configs.
- **Edge cases**: app-namespace config cannot reference controller-namespace services/templates (separate configs; unconfirmed whether cross-reference is possible); needs cluster-wide get/list/watch on configmaps/secrets (example ClusterRole); flag off → app-namespace ConfigMaps are ignored; per-config dedup state isolation in the notified annotation unconfirmed.
- **Configuration surface**: flag `--self-service-notification-enabled`; env `ARGOCD_NOTIFICATION_CONTROLLER_SELF_SERVICE_NOTIFICATION_ENABLED`; cmd-params `notificationscontroller.selfservice.enabled` (plus `application.namespaces`).
- **Exposure**: ConfigMap/Secret in app namespaces; component flag.
- **Tests required**:
  - positive: doc example — controller config only has Slack; app namespace defines `service.pagerdutyv2` + secret; app subscribed to `on-sync-failed.pagerdutyv2` gets a PagerDuty event.
  - positive: same trigger/service at both levels → two deliveries.
  - negative: flag disabled → app-namespace config ignored.
  - authorization: tenant config in `team-a` is never applied to apps in `team-b`; `.secrets` unavailable everywhere.

### NOTIF-055 Unknown services / templates / triggers at delivery time
- **Upstream**: [troubleshooting-errors.md#L33](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/troubleshooting-errors.md#L33), [troubleshooting-errors.md#L39](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/troubleshooting-errors.md#L39)
- **Maturity**: GA
- **Semantics**: Subscriptions naming a service that is not configured (or whose config failed to parse) fail with `notification service 'xxxx' is not supported`; a `service.<type>` key with an unknown type fails parsing with `service type 'xxxx' is not supported`. Errors are logged per destination and counted as failed deliveries.
- **Edge cases**: typos in service names in annotations; behaviour for triggers/templates referenced but undefined is unconfirmed (expected similar "not found" errors).
- **Configuration surface**: none.
- **Exposure**: logs/metrics.
- **Tests required**:
  - negative: exact error strings above for each case.
  - positive: other valid destinations on the same app still deliver.

---

## H. Metrics

### NOTIF-056 Metrics endpoint
- **Upstream**: [argocd_notification.go#L37](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-notification/commands/argocd_notification.go#L37), [argocd_notification.go#L149](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-notification/commands/argocd_notification.go#L149), [argocd_notification.go#L183](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-notification/commands/argocd_notification.go#L183), [docs/operator-manual/notifications/monitoring.md#L3](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/monitoring.md#L3), [test/e2e/notification_test.go#L43](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/test/e2e/notification_test.go#L43)
- **Maturity**: GA
- **Semantics**: HTTP server on `0.0.0.0:<metrics-port>` (default 9001) serving `/metrics` from the engine registry (namespace prefix `argocd`) plus the Prometheus default gatherer (Go/process metrics). Also used as the e2e health check and the TCP liveness target. The metrics server starts before informer sync.
- **Edge cases**: port already in use → `log.Fatal` (process exits); there is no separate `/healthz` path.
- **Configuration surface**: flag `--metrics-port`.
- **Exposure**: HTTP `/metrics`.
- **Tests required**:
  - positive: `GET :9001/metrics` returns 200 with `go_goroutines` and notifications metrics.
  - negative: occupied port → process exits.

### NOTIF-057 Metric `argocd_notifications_deliveries_total`
- **Upstream**: [monitoring.md#L11](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/monitoring.md#L11), [docs/operator-manual/notifications/grafana-dashboard.json#L149](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/grafana-dashboard.json#L149)
- **Maturity**: GA
- **Semantics**: Counter of delivery attempts with labels `trigger` (trigger name), `service` (service name), `succeeded` (`true`/`false`).
- **Edge cases**: skipped (already notified) destinations are not counted; label cardinality grows with custom service names.
- **Configuration surface**: none.
- **Exposure**: Prometheus.
- **Tests required**:
  - positive: one success → `{trigger="on-sync-succeeded",service="slack",succeeded="true"} 1`.
  - negative: failure increments `succeeded="false"`.

### NOTIF-058 Metric `argocd_notifications_trigger_eval_total`
- **Upstream**: [monitoring.md#L20](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/monitoring.md#L20), [grafana-dashboard.json#L63](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/grafana-dashboard.json#L63)
- **Maturity**: GA
- **Semantics**: Counter of trigger evaluations with labels `name` (trigger) and `triggered` (`true`/`false`). Evaluations happen on every processed app event and every 60 s resync for triggers with destinations.
- **Edge cases**: apps skipped by the freshness gate or namespace filter are not evaluated (no increment).
- **Configuration surface**: none.
- **Exposure**: Prometheus; shipped Grafana dashboard JSON.
- **Tests required**:
  - positive: subscribed app with false condition increments `triggered="false"` per evaluation.
  - positive: dashboard queries from `grafana-dashboard.json` return data against Patrol metrics.

---

## I. Delivery services

Service type identifiers used in `service.<type>` keys and as template section names are those of the per-service docs below. Every option value may use `$secret-key` (NOTIF-016). Unless stated, the recipient is taken from the subscription value.

### NOTIF-059 Email service (`service.email`)
- **Upstream**: [docs/operator-manual/notifications/services/email.md#L5](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/email.md#L5), [email.md#L46](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/email.md#L46), [email.md#L60](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/email.md#L60), [index.md#L17](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/index.md#L17), [legacy.go#L132](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/settings/legacy.go#L132), [go.mod#L301](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/go.mod#L301)
- **Maturity**: GA
- **Semantics**: SMTP delivery. Options: `host`, `port`, `username`, `password` (both optional — unauthenticated relay supported), `from`, `html` (bool: send body as HTML), `insecure_skip_verify` (bool). Recipient = destination email address (one email per recipient). Template section `email.subject` (Go template) sets the subject; body = `message`. Gmail requires an app password; port 465 (implicit TLS) and 587 (STARTTLS) both shown in docs.
- **Edge cases**: missing `email.subject` → empty subject (unconfirmed); auth failure → delivery error; legacy `subject` template field maps to `email.subject` (NOTIF-019).
- **Configuration surface**: `service.email[.<name>]` keys above; template `email.subject`.
- **Exposure**: ConfigMap; subscriptions `...<trigger>.email: a@b.com;c@d.com`.
- **Tests required**:
  - positive: SMTP sink receives subject/body for each of two recipients.
  - positive: `html: true` sends `text/html`.
  - negative: wrong password → `succeeded="false"`.
  - positive: no-auth relay works without username/password.

### NOTIF-060 Slack service configuration (`service.slack`)
- **Upstream**: [docs/operator-manual/notifications/services/slack.md#L5](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/slack.md#L5), [slack.md#L25](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/slack.md#L25), [argocd-notifications-cm.yaml#L46](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/argocd-notifications-cm.yaml#L46), [go.mod#L272](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/go.mod#L272)
- **Maturity**: GA
- **Semantics**: Slack Web API (`chat.postMessage` family) using bot OAuth `token` (required; scopes `chat:write`, plus `chat:write.customize` for username/icon). Options: `apiURL` (alternate Slack-compatible API base, e.g. Mattermost `/api`), `channels` (list), `icon` (emoji or URL), `insecureSkipVerify`, `signingSecret`, `username`, `disableUnfurl` (bool, disables link unfurling), HTTP tuning (NOTIF-090). Recipient = channel name or ID; the bot must be a member of private channels.
- **Edge cases**: bot not invited → Slack error (`not_in_channel`/`channel_not_found`) → failed delivery; YAML-unquoted emoji `icon: :rocket:` breaks parsing (NOTIF-014); semantics of `channels` and `signingSecret` are not described in docs (unconfirmed).
- **Configuration surface**: `service.slack[.<name>]` keys above.
- **Exposure**: ConfigMap.
- **Tests required**:
  - positive: message posted to channel via mock Slack API with bearer token.
  - positive: `disableUnfurl: true` sets unfurl flags off in the request.
  - positive: `apiURL` override routes requests to the alternate base URL.
  - negative: invalid token → failure metric.

### NOTIF-061 Slack template fields: `attachments`, `blocks`, `username`, `icon`
- **Upstream**: [slack.md#L93](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/slack.md#L93), [slack.md#L125](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/slack.md#L125), [notifications_catalog/install.yaml#L20](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notifications_catalog/install.yaml#L20)
- **Maturity**: GA
- **Semantics**: Template `slack.attachments` and `slack.blocks` are Go-templated **strings containing JSON** arrays (Slack legacy attachments / Block Kit) sent alongside `message` as text. `slack.username` and `slack.icon` override the service-level values for that template (template wins).
- **Edge cases**: rendered JSON invalid (e.g. trailing comma from a `range`) → delivery error; catalog templates build JSON with conditionals (`Repository` vs `Repositories`) that must render identically.
- **Configuration surface**: template `slack.{attachments,blocks,username,icon}`.
- **Exposure**: ConfigMap.
- **Tests required**:
  - positive: catalog `app-sync-succeeded` attachments JSON for single- and multi-source apps equals upstream rendering.
  - positive: template `username: testbot` overrides service `username`.
  - negative: malformed attachments JSON → failed delivery.

### NOTIF-062 Slack threading: `groupingKey` and `notifyBroadcast`
- **Upstream**: [slack.md#L154](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/slack.md#L154), [install.yaml#L53](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notifications_catalog/install.yaml#L53)
- **Maturity**: GA
- **Semantics**: `slack.groupingKey` (templated string, e.g. `{{.app.status.sync.revision}}`) aggregates messages into a Slack thread: the first message with a given key starts the thread, later messages with the same key (across templates, independently per channel) are posted as replies. `notifyBroadcast: true` additionally broadcasts the threaded reply to the channel. Catalog templates set `groupingKey: ""` and `notifyBroadcast: false`.
- **Edge cases**: thread timestamp storage is in-memory in the engine (restart loses thread association — unconfirmed); empty key → no threading.
- **Configuration surface**: template `slack.groupingKey`, `slack.notifyBroadcast`.
- **Exposure**: ConfigMap.
- **Tests required**:
  - positive: running→succeeded messages with same revision key land in one thread.
  - positive: `notifyBroadcast: true` sets reply broadcast.
  - positive: different channels keep separate threads for the same key.

### NOTIF-063 Slack `deliveryPolicy` (`Post`, `PostAndUpdate`, `Update`)
- **Upstream**: [slack.md#L208](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/slack.md#L208), [install.yaml#L52](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notifications_catalog/install.yaml#L52)
- **Maturity**: GA
- **Semantics**: `slack.deliveryPolicy`: `Post` (default) always posts a new message; `PostAndUpdate` posts the first message for a `groupingKey` and updates it afterwards; `Update` only updates an existing message for the `groupingKey`. `PostAndUpdate`/`Update` require `groupingKey`.
- **Edge cases**: `Update` with no prior message → nothing sent / error (unconfirmed); policy without groupingKey → error (unconfirmed exact text); invalid value handling unconfirmed.
- **Configuration surface**: template `slack.deliveryPolicy`.
- **Exposure**: ConfigMap.
- **Tests required**:
  - positive: `PostAndUpdate` + key → one message edited in place on second notification.
  - positive: `Post` → two messages.
  - negative: `Update` without groupingKey → failure matching upstream.

### NOTIF-064 Mattermost service (`service.mattermost`)
- **Upstream**: [docs/operator-manual/notifications/services/mattermost.md#L3](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/mattermost.md#L3), [mattermost.md#L45](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/mattermost.md#L45), [mattermost.md#L55](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/mattermost.md#L55)
- **Maturity**: GA
- **Semantics**: Options `apiURL`, `token` (bot token), `insecureSkipVerify`, HTTP tuning. Recipient = channel **ID**. Template `mattermost.attachments` = JSON string of Slack-compatible attachments.
- **Edge cases**: channel name instead of ID → API error; bot not in team/channel → failure.
- **Configuration surface**: `service.mattermost[.<name>]`; template `mattermost.attachments`.
- **Exposure**: ConfigMap.
- **Tests required**:
  - positive: post with attachments to mock Mattermost API using bearer token.
  - negative: wrong channel ID → failed delivery.

### NOTIF-065 Opsgenie service configuration (`service.opsgenie`)
- **Upstream**: [docs/operator-manual/notifications/services/opsgenie.md#L1](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/opsgenie.md#L1), [opsgenie.md#L34](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/opsgenie.md#L34), [opsgenie.md#L83](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/opsgenie.md#L83), [argocd-notifications-cm.yaml#L67](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/argocd-notifications-cm.yaml#L67), [go.mod#L259](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/go.mod#L259)
- **Maturity**: GA
- **Semantics**: Creates Opsgenie alerts. Options `apiUrl` (`api.opsgenie.com` or `api.eu.opsgenie.com`), `apiKeys` (map team → API-integration key). Recipient = team name, which selects the API key from `apiKeys`. Alert message = rendered `message`.
- **Edge cases**: recipient not in `apiKeys` → error (unconfirmed text); EU account with US URL → auth failure.
- **Configuration surface**: `service.opsgenie[.<name>]` `apiUrl`, `apiKeys`.
- **Exposure**: ConfigMap.
- **Tests required**:
  - positive: alert created with correct key per team.
  - negative: unknown team → failed delivery.

### NOTIF-066 Opsgenie template fields
- **Upstream**: [opsgenie.md#L21](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/opsgenie.md#L21), [opsgenie.md#L44](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/opsgenie.md#L44)
- **Maturity**: GA
- **Semantics**: `opsgenie.description` (required), `priority` (P1–P5, default P3), `alias` (dedup key), `note`, `actions` ([]string), `tags` ([]string), `visibleTo` (list of `{Type: team|user, Id|Name|Username}`), `details` (map string→string), `entity`, `user`. All templated.
- **Edge cases**: invalid priority value → API rejection/mapping (unconfirmed); `visibleTo` item without `type` invalid.
- **Configuration surface**: template `opsgenie.*`.
- **Exposure**: ConfigMap.
- **Tests required**:
  - positive: doc example renders all fields into the create-alert request.
  - positive: missing priority → P3.

### NOTIF-067 Grafana annotations service (`service.grafana`)
- **Upstream**: [docs/operator-manual/notifications/services/grafana.md#L1](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/grafana.md#L1), [grafana.md#L24](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/grafana.md#L24), [grafana.md#L44](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/grafana.md#L44), [grafana.md#L59](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/grafana.md#L59)
- **Maturity**: GA
- **Semantics**: Creates a Grafana annotation whose text is the rendered `message` (no Grafana-specific template section). Options `apiUrl`/`apiURL` (docs use both spellings; example `https://grafana.example.com/api`), `apiKey` (service-account key, Editor role), `insecureSkipVerify`, HTTP tuning. Recipient = `|`-separated list of annotation tags (`tag1|tag2`).
- **Edge cases**: empty recipient → annotation without tags (unconfirmed); wrong key → 401 failure.
- **Configuration surface**: `service.grafana[.<name>]`.
- **Exposure**: ConfigMap.
- **Tests required**:
  - positive: annotation POST with text and tags `[tag1,tag2]`.
  - negative: invalid apiKey → failure.

### NOTIF-068 Webhook service configuration (`service.webhook.<name>`)
- **Upstream**: [docs/operator-manual/notifications/services/webhook.md#L1](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/webhook.md#L1), [webhook.md#L32](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/webhook.md#L32), [webhook.md#L81](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/webhook.md#L81), [legacy.go#L152](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/settings/legacy.go#L152)
- **Maturity**: GA
- **Semantics**: Generic HTTP request. Options: `url` (base URL), `headers` (list of `{name, value}`; values may embed `$secret` refs such as `token $github-token`), `basicAuth` `{username, password}`, `insecureSkipVerify`, retry options (NOTIF-070), HTTP tuning. The service is always named (`service.webhook.<webhook-name>`), subscribed as `subscribe.<trigger>.<webhook-name>: ""`, and in default subscriptions as recipient `<webhook-name>`. Custom CAs via `argocd-tls-certs-cm` (NOTIF-011).
- **Edge cases**: untrusted cert → `x509: certificate signed by unknown authority`; webhook without matching template section sends nothing/empty (unconfirmed); success criteria by HTTP status unconfirmed beyond the documented 5xx retry.
- **Configuration surface**: `service.webhook.<name>` keys above.
- **Exposure**: ConfigMap.
- **Tests required**:
  - positive: headers and basic auth present on received request.
  - positive: `insecureSkipVerify: true` reaches a self-signed endpoint.
  - negative: self-signed without trust → x509 failure.

### NOTIF-069 Webhook template (`webhook.<name>.method|path|body`)
- **Upstream**: [webhook.md#L51](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/webhook.md#L51), [webhook.md#L104](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/webhook.md#L104), [webhook.md#L169](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/webhook.md#L169), [templates.md#L92](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/templates.md#L92)
- **Maturity**: GA
- **Semantics**: Template section `webhook` is a map keyed by **webhook service name**; each entry has `method` (`GET`, `POST`, `PUT`, `PATCH`; default `GET`), `path` (templated, appended to the service `url`), `body` (templated string sent verbatim, e.g. JSON, form-encoded `key1=value1&key2=value2`). Content type comes from service headers. One template can target several webhooks.
- **Edge cases**: GitHub commit-status example path `/repos/{{call .repo.FullNameByRepoURL ...}}/statuses/{{...revision}}`; empty path → request to base `url`; body with secrets via `.secrets` (non-self-service).
- **Configuration surface**: template `webhook.<name>.{method,path,body}`.
- **Exposure**: ConfigMap.
- **Tests required**:
  - positive: POST with rendered JSON body to `url+path`.
  - positive: default method GET when omitted.
  - positive: form-data example delivered with `application/x-www-form-urlencoded` header from service.

### NOTIF-070 Webhook retries (`retryMax`, `retryWaitMin`, `retryWaitMax`)
- **Upstream**: [webhook.md#L14](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/webhook.md#L14), [webhook.md#L22](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/webhook.md#L22)
- **Maturity**: GA
- **Semantics**: The webhook client retries on network errors and HTTP 5xx: `retryMax` (default 3), wait between `retryWaitMin` (default 1s) and `retryWaitMax` (default 5s). When all retries fail the send returns an error (failed delivery).
- **Edge cases**: 4xx not retried; duration parsing format (`1s`) per Go durations (unconfirmed).
- **Configuration surface**: `service.webhook.<name>.{retryMax,retryWaitMin,retryWaitMax}`.
- **Exposure**: ConfigMap.
- **Tests required**:
  - positive: server returns 503 twice then 200 → delivered, 3 requests observed.
  - negative: 400 → one request, failure.
  - negative: 5xx always → `retryMax+1` attempts then failure.

### NOTIF-071 Telegram service (`service.telegram`)
- **Upstream**: [docs/operator-manual/notifications/services/telegram.md#L1](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/telegram.md#L1), [telegram.md#L17](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/telegram.md#L17), [telegram.md#L37](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/telegram.md#L37), [go.mod#L158](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/go.mod#L158)
- **Maturity**: GA
- **Semantics**: Bot API delivery with option `token`. Recipient = public channel username or private chat ID (negative number such as `-1000000000000`); optional forum thread with `<chatID>|<threadID>` (e.g. `-1000000000000|2`). Body = `message`.
- **Edge cases**: bot not admin of channel → failure; non-numeric thread part → error (unconfirmed); username with/without `@` handling unconfirmed.
- **Configuration surface**: `service.telegram[.<name>]` `token`.
- **Exposure**: ConfigMap.
- **Tests required**:
  - positive: message to chat ID; recipient `chat|thread` is posted into that forum thread.
  - positive: username recipient.
  - negative: invalid token → failure.

### NOTIF-072 Microsoft Teams (Office 365 Connectors) service (`service.teams`)
- **Upstream**: [docs/operator-manual/notifications/services/teams.md#L3](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/teams.md#L3), [teams.md#L22](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/teams.md#L22), [teams.md#L67](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/teams.md#L67), [teams.md#L91](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/teams.md#L91), [services/overview.md#L50](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/overview.md#L50), [install.yaml#L55](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notifications_catalog/install.yaml#L55)
- **Maturity**: Deprecated (Microsoft retired Office 365 Connectors on 2026-03-31; docs label the service legacy/deprecated)
- **Semantics**: Option `recipientUrls` (map channel name → incoming-webhook URL, usually `$secret`). Recipient = channel name key. Sends a MessageCard. Template `teams` fields: `title`, `text`, `summary`, `themeColor` (hex), `sections` (JSON string), `facts` (JSON string, alternative to sections), `potentialAction` (JSON string of OpenUri actions); `text` falls back to `message`. Success requires the response body to be exactly `"1"` for connector URLs. Catalog templates populate `teams.title/facts/potentialAction/themeColor`.
- **Edge cases**: recipient not in `recipientUrls` → error; connector URLs now rejected by Microsoft → failures; standard channels only.
- **Configuration surface**: `service.teams[.<name>]` `recipientUrls`; template `teams.*`.
- **Exposure**: ConfigMap.
- **Tests required**:
  - positive: MessageCard JSON with title/facts/themeColor/potentialAction posted to mapped URL; response `1` → success.
  - negative: response body other than `1` → failure.
  - negative: unknown channel name → failure.

### NOTIF-073 Microsoft Teams Workflows service (`service.teams-workflows`)
- **Upstream**: [docs/operator-manual/notifications/services/teams-workflows.md#L5](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/teams-workflows.md#L5), [teams-workflows.md#L13](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/teams-workflows.md#L13), [teams-workflows.md#L73](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/teams-workflows.md#L73), [teams-workflows.md#L130](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/teams-workflows.md#L130), [teams-workflows.md#L234](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/teams-workflows.md#L234)
- **Maturity**: GA
- **Semantics**: Option `recipientUrls` (channel → Power Automate webhook URL on `api.powerautomate.com`, `api.powerplatform.com`, `flow.microsoft.com`, or containing `/powerautomate/`). Always sends an Adaptive Card wrapped in the `{type: message, attachments: [...]}` envelope. Template `teams-workflows` fields converted: `title` → large bold TextBlock (colour = `themeColor`, semantic `Good|Warning|Attention|Accent` or hex), `text` → wrapped TextBlock (falls back to `message`), `facts` → FactSet (`name`→`title`), `sections[].facts` → FactSets, `potentialAction` OpenUri → `Action.OpenUrl` (other action types dropped), `summary` preserved but unused. `adaptiveCard` (raw AdaptiveCard JSON, v1.4) takes precedence over all other fields and is only wrapped; `template` is a legacy raw JSON field. Supports standard, shared and private channels.
- **Edge cases**: invalid `adaptiveCard` JSON → failure; non-OpenUri actions silently dropped; existing `teams` templates do not apply unless renamed to `teams-workflows`.
- **Configuration surface**: `service.teams-workflows[.<name>]` `recipientUrls`; template `teams-workflows.{title,text,summary,themeColor,facts,sections,potentialAction,adaptiveCard,template}`.
- **Exposure**: ConfigMap.
- **Tests required**:
  - positive: field-conversion table produces the documented Adaptive Card body/actions.
  - positive: `adaptiveCard` provided → other fields ignored, card wrapped in envelope.
  - negative: unknown channel → failure.

### NOTIF-074 Google Chat service (`service.googlechat`)
- **Upstream**: [docs/operator-manual/notifications/services/googlechat.md#L3](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/googlechat.md#L3), [googlechat.md#L48](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/googlechat.md#L48), [googlechat.md#L86](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/googlechat.md#L86)
- **Maturity**: GA
- **Semantics**: Option `webhooks` (map space name → incoming webhook URL). Recipient = webhook name. Simple text = `message`; card messages via template `googlechat.cardsV2` (YAML or JSON string, all Card v2 fields) or deprecated `googlechat.cards` (legacy cards); `googlechat.threadKey` (templated) posts into a thread keyed by that value.
- **Edge cases**: recipient not in `webhooks` → error; invalid card structure → API 400 failure.
- **Configuration surface**: `service.googlechat[.<name>]` `webhooks`; template `googlechat.{cardsV2,cards,threadKey}`.
- **Exposure**: ConfigMap.
- **Tests required**:
  - positive: text message to mapped webhook.
  - positive: cardsV2 YAML rendered/converted to JSON body.
  - positive: threadKey sent as thread key parameter for both text and card messages.

### NOTIF-075 GitHub service configuration (`service.github`, GitHub App auth)
- **Upstream**: [docs/operator-manual/notifications/services/github.md#L3](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/github.md#L3), [github.md#L18](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/github.md#L18), [github.md#L107](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/github.md#L107), [troubleshooting-errors.md#L43](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/troubleshooting-errors.md#L43), [go.mod#L326](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/go.mod#L326)
- **Maturity**: GA
- **Semantics**: Authenticates as a GitHub App: `appID`, `installationID`, `privateKey` (PEM, via `$secret`), optional `enterpriseBaseURL` (must include `/api/v3`), HTTP tuning. Subscription value is empty (`subscribe.<trigger>.github: ""`). Template section `github` with `repoURLPath` (default `{{.app.spec.source.repoURL}}`) and `revisionPath` (default `{{.app.status.operationState.syncResult.revision}}`) locating the repo and commit, plus sub-objects `status` (NOTIF-076), `deployment` (NOTIF-077), `pullRequestComment` (NOTIF-078), `checkRun` (NOTIF-079); any combination may be present.
- **Edge cases**: multi-source app with default paths → `GitHub.repoURL (<no value>) does not have a /` or `POST .../statuses/: 404 Not Found` (override `repoURLPath: "{{ (index .app.spec.sources 0).repoURL }}"`, `revisionPath: "{{index .app.status.operationState.syncResult.revisions 0}}"`); GHE URL without `/api/v3` → failures; invalid key → auth failure.
- **Configuration surface**: `service.github[.<name>]` options; template `github.{repoURLPath,revisionPath}`.
- **Exposure**: ConfigMap.
- **Tests required**:
  - positive: installation token obtained and used against mock GitHub (github.com and GHE base).
  - negative: multi-source app with defaults → documented error.
  - positive: path overrides fix multi-source.

### NOTIF-076 GitHub commit status (`github.status`)
- **Upstream**: [github.md#L71](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/github.md#L71), [github.md#L109](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/github.md#L109), [github.md#L118](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/github.md#L118)
- **Maturity**: GA
- **Semantics**: Creates a commit status on `revisionPath` with `state` (`error|failure|pending|success`), `label` (status context, e.g. `continuous-delivery/{{.app.metadata.name}}`), `targetURL`; description = `message`, truncated if ≥140 characters. HTTP 422 after the 1000-statuses-per-SHA/context limit is ignored and the notification treated as completed.
- **Edge cases**: message ≥140 chars truncated; 422 limit swallowed.
- **Configuration surface**: template `github.status.{state,label,targetURL}`.
- **Exposure**: ConfigMap.
- **Tests required**:
  - positive: `POST /repos/{owner}/{repo}/statuses/{sha}` with state/context/target_url/description.
  - positive: 150-char message truncated as upstream.
  - positive: 422 → counted success / no retry loop.

### NOTIF-077 GitHub deployment (`github.deployment`)
- **Upstream**: [github.md#L78](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/github.md#L78), [github.md#L111](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/github.md#L111), [github.md#L116](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/github.md#L116)
- **Maturity**: GA
- **Semantics**: Creates a GitHub deployment and deployment status: `state`, `environment`, `environmentURL`, `logURL`, `requiredContexts` (list; `[]` disables status checks), `autoMerge` (default **true**; set false to deploy older refs), `transientEnvironment` (bool), `reference` (ref to deploy; default = revision).
- **Edge cases**: `autoMerge: true` with an outdated ref → GitHub 409 merge conflict failure; existing deployment reuse semantics unconfirmed.
- **Configuration surface**: template `github.deployment.*`.
- **Exposure**: ConfigMap.
- **Tests required**:
  - positive: create deployment + status with all fields.
  - positive: `reference` overrides ref; default ref = revision.
  - negative: required contexts failing → GitHub error surfaces as failed delivery.

### NOTIF-078 GitHub pull-request comment (`github.pullRequestComment`)
- **Upstream**: [github.md#L87](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/github.md#L87), [github.md#L114](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/github.md#L114)
- **Maturity**: GA
- **Semantics**: Posts `content` (templated, truncated if ≥65536 chars) as a comment on pull requests associated with the revision; `commentTag` identifies the comment — if a comment carrying the tag exists it is updated (upsert), else a new one is created.
- **Edge cases**: no PR associated with the commit → nothing posted (unconfirmed); tag embedding format unconfirmed.
- **Configuration surface**: template `github.pullRequestComment.{content,commentTag}`.
- **Exposure**: ConfigMap.
- **Tests required**:
  - positive: first notification creates comment; second with same tag edits it.
  - positive: content >65536 chars truncated.

### NOTIF-079 GitHub check run (`github.checkRun`)
- **Upstream**: [github.md#L92](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/github.md#L92)
- **Maturity**: GA
- **Semantics**: Creates a check run on the revision: `name`, `details_url`, `status` (`queued|in_progress|completed`), `conclusion` (e.g. `success`), `started_at`, `completed_at` (RFC3339), `output.{title,summary,text}`.
- **Edge cases**: `conclusion` required when `status: completed` (GitHub API rule); invalid timestamps → API 422.
- **Configuration surface**: template `github.checkRun.*`.
- **Exposure**: ConfigMap.
- **Tests required**:
  - positive: check run POST carries all fields.
  - negative: completed without conclusion → failed delivery as upstream.

### NOTIF-080 PagerDuty (REST incidents) service (`service.pagerduty`)
- **Upstream**: [docs/operator-manual/notifications/services/pagerduty.md#L3](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/pagerduty.md#L3), [pagerduty.md#L25](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/pagerduty.md#L25), [pagerduty.md#L36](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/pagerduty.md#L36), [pagerduty.md#L57](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/pagerduty.md#L57), [go.mod#L159](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/go.mod#L159)
- **Maturity**: GA
- **Semantics**: Creates PagerDuty incidents via REST API. Options: API token (example key `token`; parameter list says `pagerdutyToken`), `from` (email of a valid PagerDuty user). Recipient = PagerDuty service ID. Template `pagerduty.{title, urgency, body, priorityID}` (priority only on Standard/Enterprise plans).
- **Edge cases**: option name ambiguity (`token` vs `pagerdutyToken`) — see Doc differences; invalid `from` → 400.
- **Configuration surface**: `service.pagerduty[.<name>]`; template `pagerduty.*`.
- **Exposure**: ConfigMap.
- **Tests required**:
  - positive: incident created on mocked API for the recipient service ID with title/urgency/body/priority.
  - negative: invalid token → failure.

### NOTIF-081 PagerDuty Events API v2 service (`service.pagerdutyv2`)
- **Upstream**: [docs/operator-manual/notifications/services/pagerduty_v2.md#L3](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/pagerduty_v2.md#L3), [pagerduty_v2.md#L38](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/pagerduty_v2.md#L38), [pagerduty_v2.md#L56](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/pagerduty_v2.md#L56), [index.md#L91](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/index.md#L91)
- **Maturity**: GA
- **Semantics**: Triggers Events API v2 events. Option `serviceKeys` (map service name → integration key). Recipient = service name key. Template `pagerdutyv2` (all strings): `summary` (required), `severity` (required; `critical|warning|error|info`), `source` (required), `component`, `group`, `class`, `url` ("View in ArgoCD" link), `dedupKey` (events with same key correlate into one incident). `timestamp` and `custom_details` are not supported.
- **Edge cases**: recipient missing from `serviceKeys` → error; invalid severity → API 400.
- **Configuration surface**: `service.pagerdutyv2[.<name>]` `serviceKeys`; template `pagerdutyv2.*`.
- **Exposure**: ConfigMap.
- **Tests required**:
  - positive: enqueue request with routing key from `serviceKeys[recipient]` and payload fields.
  - positive: `dedupKey` passed through.
  - negative: unknown recipient → failure.

### NOTIF-082 New Relic deployment markers (`service.newrelic`)
- **Upstream**: [docs/operator-manual/notifications/services/newrelic.md#L3](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/newrelic.md#L3), [newrelic.md#L37](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/newrelic.md#L37), [newrelic.md#L48](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/newrelic.md#L48)
- **Maturity**: GA
- **Semantics**: Records a deployment for a New Relic APM application. Options `apiURL` (e.g. `https://api.newrelic.com`), `apiKey`, HTTP tuning. Recipient = New Relic application ID. Template `newrelic.{revision, description, changelog, user}` with defaults: revision `{{.app.status.operationState.syncResult.revision}}`, description = `message`, changelog = commit message and user = commit author via `repo.GetCommitMetadata` of `.app.status.sync.revision`.
- **Edge cases**: default changelog/user call the repo-server — failures there fail the delivery unless overridden (inferred from NOTIF-044).
- **Configuration surface**: `service.newrelic[.<name>]`; template `newrelic.*`.
- **Exposure**: ConfigMap.
- **Tests required**:
  - positive: deployment recorded on a mocked New Relic API for the recipient app ID with defaulted revision/description/changelog/user (endpoint path and payload shape per upstream engine; unconfirmed).
  - positive: explicit `description` overrides message.

### NOTIF-083 Rocket.Chat service (`service.rocketchat`)
- **Upstream**: [docs/operator-manual/notifications/services/rocketchat.md#L3](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/rocketchat.md#L3), [rocketchat.md#L53](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/rocketchat.md#L53), [rocketchat.md#L65](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/rocketchat.md#L65), [go.mod#L161](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/go.mod#L161)
- **Maturity**: GA
- **Semantics**: Logs in as a bot user and posts messages. Options `email`, `password`, `alias`, `icon`, `avatar`, `serverUrl`. Recipient must be prefixed `#channel` or `@user`; otherwise interpreted as a room ID. Template `rocketchat.attachments` (Slack-compatible JSON string).
- **Edge cases**: bot not in channel → failure; YAML `#my_channel` as annotation value must be quoted in YAML (unquoted `#` starts a comment — docs example is unquoted).
- **Configuration surface**: `service.rocketchat[.<name>]`; template `rocketchat.attachments`.
- **Exposure**: ConfigMap.
- **Tests required**:
  - positive: `#chan`, `@user`, raw room ID recipients each routed correctly.
  - negative: bad credentials → failure.

### NOTIF-084 Pushover service (`service.pushover`)
- **Upstream**: [docs/operator-manual/notifications/services/pushover.md#L1](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/pushover.md#L1), [pushover.md#L25](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/pushover.md#L25), [go.mod#L223](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/go.mod#L223)
- **Maturity**: GA
- **Semantics**: Option `token` (application API key). Recipient = Pushover user key. Body = `message` (no documented template section).
- **Edge cases**: invalid user key → API error.
- **Configuration surface**: `service.pushover[.<name>]` `token`.
- **Exposure**: ConfigMap.
- **Tests required**:
  - positive: message pushed with token + user key.
  - negative: invalid user key → failure.

### NOTIF-085 Prometheus Alertmanager service (`service.alertmanager`)
- **Upstream**: [docs/operator-manual/notifications/services/alertmanager.md#L3](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/alertmanager.md#L3), [alertmanager.md#L19](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/alertmanager.md#L19), [alertmanager.md#L130](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/alertmanager.md#L130), [alertmanager.md#L168](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/alertmanager.md#L168)
- **Maturity**: GA
- **Semantics**: Pushes alerts to one or more Alertmanager instances. Options: `targets` (list `host:port`, all receive the alert — HA), `scheme` (default `http`), `apiPath` (default `/api/v2/alerts`), `insecureSkipVerify`, `basicAuth` `{username,password}` or `bearerToken` (basicAuth wins if both), `timeout` (seconds, default 3), HTTP tuning. Template `alertmanager.labels` (≥1 required; label `alertname` defaults to the template name), `annotations` (map), `generatorURL` (default `{{.app.spec.source.repoURL}}`); all templated.
- **Edge cases**: no labels → error (unconfirmed text); some targets down → partial success semantics unconfirmed; `send_resolved` must be disabled on receivers to avoid resolved notifications (doc advice).
- **Configuration surface**: `service.alertmanager[.<name>]`; template `alertmanager.*`.
- **Exposure**: ConfigMap.
- **Tests required**:
  - positive: alert POSTed to every target with labels incl. defaulted `alertname`.
  - positive: basicAuth preferred over bearerToken.
  - negative: timeout exceeded → failure.

### NOTIF-086 Webex Teams service (`service.webex`)
- **Upstream**: [docs/operator-manual/notifications/services/webex.md#L3](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/webex.md#L3), [webex.md#L33](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/webex.md#L33)
- **Maturity**: GA
- **Semantics**: Option `token` (bot access token). Recipient = personal email or room ID. Body = `message`.
- **Edge cases**: email vs room ID detection rule unconfirmed.
- **Configuration surface**: `service.webex[.<name>]` `token`.
- **Exposure**: ConfigMap.
- **Tests required**:
  - positive: message to an email recipient and to a room ID recipient (request field mapping per upstream engine; unconfirmed).
  - negative: bad token → failure.

### NOTIF-087 AWS SQS service (`service.awssqs`)
- **Upstream**: [docs/operator-manual/notifications/services/awssqs.md#L3](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/awssqs.md#L3), [awssqs.md#L64](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/awssqs.md#L64), [awssqs.md#L108](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/awssqs.md#L108), [go.mod#L169](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/go.mod#L169)
- **Maturity**: GA
- **Semantics**: Sends `message` to an SQS queue. Options: `queue` (name; overridden by a non-empty subscription recipient), `region` (or `AWS_DEFAULT_REGION`), `key`/`secret` (or `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`, IRSA/OIDC), `account` (queue owner account ID), `endpointUrl` (e.g. localstack). ConfigMap values take precedence over env. FIFO queues need `messageGroupId` in the template (doc shows it as a top-level template key).
- **Edge cases**: FIFO without messageGroupId → AWS error; empty recipient → configured `queue`; queue lookup in other account needs `account`.
- **Configuration surface**: `service.awssqs[.<name>]`; env `AWS_*`; template `messageGroupId`.
- **Exposure**: ConfigMap.
- **Tests required**:
  - positive: message delivered to localstack queue; recipient overrides queue.
  - positive: env-only credentials work.
  - negative: FIFO queue without group ID → failure.

### NOTIF-088 GCP Pub/Sub service (`service.gcppubsub`)
- **Upstream**: [docs/operator-manual/notifications/services/gcppubsub.md#L3](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/gcppubsub.md#L3), [gcppubsub.md#L51](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/gcppubsub.md#L51), [gcppubsub.md#L90](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/gcppubsub.md#L90), [go.mod#L141](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/go.mod#L141)
- **Maturity**: GA
- **Semantics**: Publishes `message` to a Pub/Sub topic. Options `project`, `topic` (overridden by non-empty recipient), `keyFile` (service-account JSON path; absent → Application Default Credentials / GKE Workload Identity). Template `gcppubsub.attributes` (map string→templated string) become message attributes.
- **Edge cases**: missing topic → publish error; keyFile path must be mounted into the controller.
- **Configuration surface**: `service.gcppubsub[.<name>]`; template `gcppubsub.attributes`.
- **Exposure**: ConfigMap.
- **Tests required**:
  - positive: publish to emulator with attributes; recipient topic override.
  - negative: nonexistent topic → failure.

### NOTIF-089 NATS service (`service.nats`)
- **Upstream**: [docs/operator-manual/notifications/services/nats.md#L3](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/nats.md#L3), [nats.md#L25](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/nats.md#L25), [go.mod#L252](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/go.mod#L252)
- **Maturity**: GA
- **Semantics**: Publishes `message` to a NATS subject. Options (example keys lowercase): `url` (e.g. `nats://nats:4222`), `headers` (map), `user`/`password`, `nkey`. Recipient = subject.
- **Edge cases**: option key case (docs list `Url`, `Headers`, `User`, `Password`, `Nkey`) — see Doc differences; empty recipient → publish error (unconfirmed).
- **Configuration surface**: `service.nats[.<name>]`.
- **Exposure**: ConfigMap.
- **Tests required**:
  - positive: subscriber on `mytopic` receives message with headers.
  - positive: user/password and nkey auth.
  - negative: server unreachable → failure.

### NOTIF-090 HTTP connection-pool tuning options (shared)
- **Upstream**: [alertmanager.md#L14](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/alertmanager.md#L14), [github.md#L11](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/github.md#L11), [grafana.md#L12](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/grafana.md#L12), [mattermost.md#L8](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/mattermost.md#L8), [newrelic.md#L7](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/newrelic.md#L7), [slack.md#L19](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/slack.md#L19), [webhook.md#L17](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/webhook.md#L17)
- **Maturity**: GA
- **Semantics**: Services alertmanager, github, grafana, mattermost, newrelic, slack and webhook accept `maxIdleConns`, `maxIdleConnsPerHost`, `maxConnsPerHost` (ints) and `idleConnTimeout` (duration string, e.g. `90s`) configuring the HTTP transport.
- **Edge cases**: unparseable `idleConnTimeout` → config/service creation error (unconfirmed); defaults when unset = engine defaults (unconfirmed).
- **Configuration surface**: per-service options above.
- **Exposure**: ConfigMap.
- **Tests required**:
  - positive: options accepted for each listed service and applied to the transport (observable via connection reuse in tests).
  - negative: `idleConnTimeout: banana` rejected like upstream.

### NOTIF-091 Supported service type identifiers
- **Upstream**: [services/overview.md#L39](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/services/overview.md#L39), [mkdocs.yml#L82](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/mkdocs.yml#L82), [troubleshooting-errors.md#L33](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/troubleshooting-errors.md#L33)
- **Maturity**: GA
- **Semantics**: Documented `service.<type>` identifiers at this commit: `alertmanager`, `awssqs`, `email`, `gcppubsub`, `github`, `googlechat`, `grafana`, `mattermost`, `nats`, `newrelic`, `opsgenie`, `pagerduty`, `pagerdutyv2`, `pushover`, `rocketchat`, `slack`, `teams`, `teams-workflows`, `telegram`, `webex`, `webhook` (21). Any other type fails with `service type 'xxxx' is not supported`. No GitLab, Discord, Jira, etc. services are documented.
- **Edge cases**: case sensitivity of type identifiers (unconfirmed — assume exact lower-case); `console` destination used by the CLI default recipient (NOTIF-106) is not a configurable `service.` type in docs.
- **Configuration surface**: ConfigMap `service.*` keys.
- **Exposure**: ConfigMap.
- **Tests required**:
  - positive: each of the 21 identifiers parses with minimal valid options.
  - negative: `service.gitlab` → `service type 'gitlab' is not supported`.

---

## J. Default catalog (`notifications_catalog/install.yaml`)

All catalog templates reference `{{.context.argocdUrl}}`, so `context.argocdUrl` must be set for working links. Patrol must reproduce the template text byte-for-byte (including quirks) to reach rendering parity.

### NOTIF-092 Catalog distribution and installation
- **Upstream**: [notifications_catalog/install.yaml#L1](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notifications_catalog/install.yaml#L1), [hack/gen-catalog/main.go#L44](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/hack/gen-catalog/main.go#L44), [docs/operator-manual/notifications/catalog.md#L1](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/catalog.md#L1), [index.md#L11](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/index.md#L11), [manifests/base/notification/kustomization.yaml#L4](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/notification/kustomization.yaml#L4)
- **Maturity**: GA
- **Semantics**: The catalog is a ConfigMap manifest named `argocd-notifications-cm` with 8 `template.*` and 8 `trigger.*` keys, generated from `notifications_catalog/templates/*.yaml` and `notifications_catalog/triggers/*.yaml` (generator also writes `catalog.md`). It is **not** part of the default install (the shipped `argocd-notifications-cm` has no data); users apply it (`kubectl apply -n argocd [--server-side --force-conflicts] -f .../notifications_catalog/install.yaml`). No services, subscriptions or `context` are included.
- **Edge cases**: applying over an existing ConfigMap merges/overwrites same-named keys (apply semantics); triggers have no default subscriptions, so nothing fires until annotated.
- **Configuration surface**: ConfigMap keys listed in NOTIF-093..100.
- **Exposure**: manifest file.
- **Tests required**:
  - positive: Patrol ships an equivalent catalog whose rendered output equals upstream for fixture apps (single-source, multi-source, with conditions).
  - positive: API lists exactly the 8 triggers and 8 templates after applying it.

### NOTIF-093 Catalog `on-created` → `app-created`
- **Upstream**: [install.yaml#L476](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notifications_catalog/install.yaml#L476), [install.yaml#L3](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notifications_catalog/install.yaml#L3)
- **Maturity**: GA
- **Semantics**: Condition `when: "true"`, `oncePer: app.metadata.name`, description `Application is created.`. Template: `email.subject` and `message` = `Application {{.app.metadata.name}} has been created.`; `teams.title` same.
- **Edge cases**: subscribing an existing app sends "created" once (condition is always true); deleting and recreating an app with the same name sends again (state lives on the object).
- **Configuration surface**: catalog keys `trigger.on-created`, `template.app-created`.
- **Exposure**: ConfigMap.
- **Tests required**:
  - positive: new subscribed app → exactly one message.
  - positive: recreate same-named app → one more message.

### NOTIF-094 Catalog `on-deleted` → `app-deleted`
- **Upstream**: [install.yaml#L482](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notifications_catalog/install.yaml#L482), [install.yaml#L9](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notifications_catalog/install.yaml#L9)
- **Maturity**: GA
- **Semantics**: `when: app.metadata.deletionTimestamp != nil`, `oncePer: app.metadata.name`. Template: subject/message/teams.title `Application {{.app.metadata.name}} has been deleted.`.
- **Edge cases**: only fires while the Application still exists with a deletion timestamp (i.e. it has a finalizer such as `resources-finalizer.argocd.argoproj.io`); an app deleted without finalizers disappears before evaluation → no message (inferred from condition); patching the notified annotation on a terminating object must still succeed.
- **Configuration surface**: catalog keys.
- **Exposure**: ConfigMap.
- **Tests required**:
  - positive: app with resources finalizer deleted (cascade) → one message.
  - negative: app without finalizer → no message (match upstream).

### NOTIF-095 Catalog `on-deployed` → `app-deployed`
- **Upstream**: [install.yaml#L488](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notifications_catalog/install.yaml#L488), [install.yaml#L15](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notifications_catalog/install.yaml#L15), [notifications_catalog/triggers/on-deployed.yaml#L1](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notifications_catalog/triggers/on-deployed.yaml#L1)
- **Maturity**: GA
- **Semantics**: `when: app.status.operationState != nil and app.status.operationState.phase in ['Succeeded'] and app.status.health.status == 'Healthy' and (!time.Parse(app.status.health.lastTransitionTime).Add(1 * time.Minute).Before(time.Parse(app.status.operationState.finishedAt)) or time.Parse(app.status.health.lastTransitionTime).Before(time.Parse(app.status.operationState.startedAt)))`; `oncePer: app.status.operationState?.syncResult?.revision`; description `Application is synced and healthy. Triggered once per commit.`. Template: subject `New version of an application {{.app.metadata.name}} is up and running.`; message with Slack-only `:white_check_mark:`; Slack attachment colour `#18be52` with fields Sync Status, Repository/Repositories (`:arrow_heading_up:` prefixed, newline-joined for multi-source), Revision, one field per `status.conditions` entry; `deliveryPolicy: Post`, `groupingKey: ""`, `notifyBroadcast: false`; Teams facts (Sync Status, Repository/ies with `⬆️ `, Revision, conditions), potentialAction "Operation Application" and "Open Repository", `themeColor '#000080'`, title.
- **Edge cases**: missing `status.health.lastTransitionTime` or `startedAt`/`finishedAt` → `time.Parse` panic → evaluation error; the "Open Repository" Teams action URI is the repo URL prefixed with `⬆️ ` (not a valid URL — upstream quirk); health that became Healthy >1 min before the sync finished and not before it started → not "deployed" (no-op sync).
- **Configuration surface**: catalog keys.
- **Exposure**: ConfigMap.
- **Tests required**:
  - positive: sync that changes resources and becomes Healthy → one message per revision.
  - negative: no-op sync on long-healthy app → no message.
  - positive: rendered Slack/Teams JSON equals upstream for single- and multi-source fixtures.

### NOTIF-096 Catalog `on-health-degraded` → `app-health-degraded`
- **Upstream**: [install.yaml#L496](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notifications_catalog/install.yaml#L496), [install.yaml#L96](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notifications_catalog/install.yaml#L96)
- **Maturity**: GA
- **Semantics**: `when: app.status.health.status == 'Degraded'`, `oncePer: app.status.operationState?.syncResult?.revision`, description `Application has degraded`. Template: subject `Application {{.app.metadata.name}} has degraded.`; message with Slack-only `:exclamation:` and details link; Slack colour `#f4c030`, fields Health Status, Repository/ies, conditions; Teams facts Health Status, Repository/ies, conditions, actions "Open Application"/"Open Repository", `themeColor '#FF0000'`.
- **Edge cases**: app never synced (no operationState) → oncePer value nil (behaviour unconfirmed); repeated degradations on the same revision → one message only.
- **Configuration surface**: catalog keys.
- **Exposure**: ConfigMap.
- **Tests required**:
  - positive: Healthy→Degraded → one message; Degraded→Healthy→Degraded same revision → no second message.
  - positive: new revision then Degraded → new message.

### NOTIF-097 Catalog `on-sync-failed` → `app-sync-failed`
- **Upstream**: [install.yaml#L502](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notifications_catalog/install.yaml#L502), [install.yaml#L169](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notifications_catalog/install.yaml#L169)
- **Maturity**: GA
- **Semantics**: `when: app.status.operationState != nil and app.status.operationState.phase in ['Error', 'Failed']`, `oncePer` sync-result revision, description `Application syncing has failed`. Template: subject `Failed to sync application {{.app.metadata.name}}.`; message includes `finishedAt` and `operationState.message` plus `?operation=true` link; Slack colour `#E96D76` (Sync Status, Repository/ies, conditions); Teams facts Sync Status, Failed at, Repository/ies, conditions; actions "Open Operation"/"Open Repository"; `themeColor '#FF0000'`.
- **Edge cases**: retried failing syncs on the same revision → one message (oncePer); failure before any `syncResult` (e.g. manifest generation error) → oncePer nil (unconfirmed).
- **Configuration surface**: catalog keys.
- **Exposure**: ConfigMap.
- **Tests required**:
  - positive: failed sync → one message with error text.
  - positive: freshness gate delays send until reconciledAt ≥ finishedAt (NOTIF-010).

### NOTIF-098 Catalog `on-sync-running` → `app-sync-running`
- **Upstream**: [install.yaml#L509](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notifications_catalog/install.yaml#L509), [install.yaml#L246](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notifications_catalog/install.yaml#L246)
- **Maturity**: GA
- **Semantics**: `when: app.status.operationState != nil and app.status.operationState.phase in ['Running']`, `oncePer` sync-result revision, description `Application is being synced`. Template: subject `Start syncing application {{.app.metadata.name}}.`; message with `startedAt` and operation link; Slack colour `#0DADEA`; Teams facts Sync Status, Started at, Repository/ies, conditions, actions "Open Operation"/"Open Repository"; no themeColor.
- **Edge cases**: at `Running` the `syncResult.revision` may be absent/previous (oncePer value of the previous sync → may suppress the message for a new sync of the same revision).
- **Configuration surface**: catalog keys.
- **Exposure**: ConfigMap.
- **Tests required**:
  - positive: manual sync → running message before the succeeded message.
  - edge: resync of same revision follows upstream suppression behaviour.

### NOTIF-099 Catalog `on-sync-status-unknown` → `app-sync-status-unknown`
- **Upstream**: [install.yaml#L515](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notifications_catalog/install.yaml#L515), [install.yaml#L322](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notifications_catalog/install.yaml#L322)
- **Maturity**: GA
- **Semantics**: `when: app.status.sync.status == 'Unknown'`, `oncePer` sync-result revision, description `Application status is 'Unknown'`. Template: subject `Application {{.app.metadata.name}} sync status is 'Unknown'`; message (Slack `:exclamation:` prefix) and, for non-Slack services, a bullet list of `status.conditions[].message`; Slack colour `#E96D76`; Teams facts, actions "Open Application"/"Open Repository"; no themeColor.
- **Edge cases**: comparison errors on never-synced apps (no operationState → nil oncePer, unconfirmed).
- **Configuration surface**: catalog keys.
- **Exposure**: ConfigMap.
- **Tests required**:
  - positive: repo unreachable → Unknown → message listing conditions for email, not for Slack.

### NOTIF-100 Catalog `on-sync-succeeded` → `app-sync-succeeded`
- **Upstream**: [install.yaml#L521](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notifications_catalog/install.yaml#L521), [install.yaml#L399](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/notifications_catalog/install.yaml#L399)
- **Maturity**: GA
- **Semantics**: `when: app.status.operationState != nil and app.status.operationState.phase in ['Succeeded']`, `oncePer` sync-result revision, description `Application syncing has succeeded`. Template: subject `Application {{.app.metadata.name}} has been successfully synced.`; message with Slack `:white_check_mark:`, `finishedAt` and operation link; Slack colour `#18be52`; Teams facts Sync Status, Synced at, Repository/ies, conditions; actions "Operation Details"/"Open Repository"; `themeColor '#000080'`; title.
- **Edge cases**: auto-sync re-syncing the same revision → no second message; freshness gate applies.
- **Configuration surface**: catalog keys.
- **Exposure**: ConfigMap.
- **Tests required**:
  - positive: `kubectl patch app ... subscribe.on-sync-succeeded.slack` + sync → one Slack message (index.md getting-started flow).
  - positive: email destination receives subject above.

---

## K. API, CLI and UI

### NOTIF-101 `NotificationService.ListServices` (`GET /api/v1/notifications/services`)
- **Upstream**: [server/notification/notification.go#L39](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/notification/notification.go#L39), [server/notification/notification.proto#L49](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/notification/notification.proto#L49), [server/server.go#L378](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L378), [server/server.go#L571](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L571), [server/server.go#L1089](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L1089), [server/server.go#L1242](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L1242), [assets/swagger.json#L2889](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/assets/swagger.json#L2889), [server/notification/notification_test.go#L77](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/notification/notification_test.go#L77), [test/e2e/notification_test.go#L13](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/test/e2e/notification_test.go#L13)
- **Maturity**: GA
- **Semantics**: Returns `{items: [{name}]}` — the configured service **names** from the API server's own engine factory, which always reads `argocd-notifications-cm`/`argocd-notifications-secret` in the Argo CD namespace (fixed names, self-service off, informers started with the server). `ServicesListRequest` is empty. If the ConfigMap is not found → empty list (no error); other config errors → returned as gRPC error. Order is unspecified (map iteration).
- **Edge cases**: controller using custom `--config-map-name` → API still reads the default name; app-namespace (self-service) services are never listed; legacy `notifiers.yaml` services appear (they are merged during config init — inferred); names only, no types or options exposed.
- **Configuration surface**: none.
- **Exposure**: API gRPC `notification.NotificationService/ListServices`, REST `GET /api/v1/notifications/services`; UI service autocomplete.
- **Tests required**:
  - positive: `service.webhook.test` → `[{name:"test"}]`.
  - negative: missing ConfigMap → `{}`/empty items, HTTP 200.
  - authorization: unauthenticated request → 401 unless anonymous access is enabled; any authenticated user (no RBAC check) → 200.

### NOTIF-102 `NotificationService.ListTriggers` (`GET /api/v1/notifications/triggers`)
- **Upstream**: [notification.go#L24](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/notification/notification.go#L24), [notification.proto#L45](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/notification/notification.proto#L45), [assets/swagger.json#L2935](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/assets/swagger.json#L2935), [notification_test.go#L86](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/notification/notification_test.go#L86), [test/e2e/notification_test.go#L33](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/test/e2e/notification_test.go#L33), [server.go#L984](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L984)
- **Maturity**: GA
- **Semantics**: Returns `{items: [{name}]}` of trigger names from the default ConfigMap (same source rules as NOTIF-101). NotFound → empty list.
- **Edge cases**: upstream defect — on a non-NotFound error the handler does not return the error and dereferences a nil API, the panic is converted by the gRPC recovery interceptor into an Internal error; Patrol should return a proper error (same status class: Internal/Unknown).
- **Configuration surface**: none.
- **Exposure**: API gRPC `ListTriggers`, REST `GET /api/v1/notifications/triggers`; UI trigger autocomplete.
- **Tests required**:
  - positive: catalog applied → 8 names; e2e: `trigger.on-created` → `[{name:"on-created"}]`.
  - negative: missing ConfigMap → empty list.
  - negative: corrupt config → 5xx error, server keeps running.
  - authorization: as NOTIF-101.

### NOTIF-103 `NotificationService.ListTemplates` (`GET /api/v1/notifications/templates`)
- **Upstream**: [notification.go#L55](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/notification/notification.go#L55), [notification.proto#L54](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/notification/notification.proto#L54), [assets/swagger.json#L2912](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/assets/swagger.json#L2912), [notification_test.go#L95](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/notification/notification_test.go#L95), [test/e2e/notification_test.go#L23](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/test/e2e/notification_test.go#L23)
- **Maturity**: GA
- **Semantics**: Returns `{items: [{name}]}` of template names from the default ConfigMap; NotFound → empty; other errors returned. Not used by the UI.
- **Edge cases**: legacy `config.yaml` templates included (inferred).
- **Configuration surface**: none.
- **Exposure**: API gRPC `ListTemplates`, REST `GET /api/v1/notifications/templates`.
- **Tests required**:
  - positive: `template.app-created` → `[{name:"app-created"}]`.
  - negative: missing ConfigMap → empty.
  - authorization: as NOTIF-101.

### NOTIF-104 `argocd admin notifications` command group and global flags
- **Upstream**: [cmd/argocd/commands/admin/notifications.go#L27](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/notifications.go#L27), [notifications.go#L38](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/notifications.go#L38), [notifications.go#L78](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/notifications.go#L78), [cmd/argocd/commands/admin/admin.go#L81](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/admin.go#L81), [util/notification/settings/settings.go#L34](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/notification/settings/settings.go#L34), [docs/user-guide/commands/argocd_admin_notifications.md#L13](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/commands/argocd_admin_notifications.md#L13), [docs/operator-manual/notifications/troubleshooting.md#L1](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/troubleshooting.md#L1)
- **Maturity**: GA
- **Semantics**: `argocd admin notifications` (talks directly to Kubernetes; no Argo CD API) wraps the engine "tools" command for Application resources. Subcommands: `template get`, `template notify`, `trigger get`, `trigger run` (NOTIF-105..108). Global flags: `--config-map <file>` (`-` = read e.g. `kustomize build` output from stdin; default: load in-cluster `argocd-notifications-cm`), `--secret <file>` (`:empty` = empty Secret; default: load in-cluster `argocd-notifications-secret`), all kubectl client flags (`--kubeconfig`, `--context`, `-n`, `--server`, `--token`, `--as`, ...), and repo-server flags `--argocd-repo-server` (default `argocd-repo-server:8081`), `--argocd-repo-server-plaintext`, deprecated `--argocd-repo-server-strict-tls`, `--repo-server-ca-cert-path`, `--repo-server-client-cert-path`, `--repo-server-client-cert-key-path` (no env prefix). Self-service is always off (secrets available). The Argo CD service (repo DB + repo-server client) is initialised lazily before commands run; `appProject` is looked up live.
- **Edge cases**: k8s config parse failure → `Failed to parse k8s config: ...` (fatal); repo TLS errors → `Failed to get repo-server client TLS configuration` / `Failed to load tls certs`; repo-server unreachable only breaks `repo.*` functions; service not initialised → `argocdService is not initialized`; custom controller ConfigMap names must be passed as files.
- **Configuration surface**: flags above.
- **Exposure**: CLI.
- **Tests required**:
  - positive: `--config-map ./cm.yaml --secret :empty trigger get` works offline from a file.
  - positive: `kustomize build | argocd admin notifications template notify ... --config-map -`.
  - negative: invalid kubeconfig → fatal error text.
  - authorization: requires Kubernetes RBAC to read the ConfigMap/Secret/Applications in the namespace.

### NOTIF-105 `argocd admin notifications template get [NAME]`
- **Upstream**: [docs/user-guide/commands/argocd_admin_notifications_template_get.md#L1](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/commands/argocd_admin_notifications_template_get.md#L1), [troubleshooting-commands.md#L1](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/troubleshooting-commands.md#L1)
- **Maturity**: GA
- **Semantics**: Prints configured templates (all, or the one named); `-o/--output json|yaml|wide|name` (default `wide`). `-o yaml` prints the template definition.
- **Edge cases**: unknown NAME → error (text unconfirmed); unknown output format → error.
- **Configuration surface**: `-o`.
- **Exposure**: CLI.
- **Tests required**:
  - positive: `template get app-sync-succeeded -o=yaml` prints the catalog definition.
  - positive: `-o name` lists names only.

### NOTIF-106 `argocd admin notifications template notify NAME RESOURCE_NAME`
- **Upstream**: [docs/user-guide/commands/argocd_admin_notifications_template_notify.md#L1](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/commands/argocd_admin_notifications_template_notify.md#L1), [troubleshooting.md#L22](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/troubleshooting.md#L22)
- **Maturity**: GA
- **Semantics**: Renders template NAME for Application RESOURCE_NAME and sends it to each `--recipient <service>:<recipient>` (repeatable `stringArray`; default `console:stdout`, which prints the rendered notification to the console). Uses the same variable provider as the controller (incl. `secrets`, `appProject`, `repo.*`). Bypasses triggers and delivery state (does not write the notified annotation — inferred).
- **Edge cases**: unknown template/app/service → error; `--recipient slack:chan` actually posts to Slack.
- **Configuration surface**: `--recipient`.
- **Exposure**: CLI.
- **Tests required**:
  - positive: default recipient prints rendered message/sections to stdout.
  - positive: `--recipient webhook-name:` delivers to a test server.
  - negative: nonexistent app → error, non-zero exit.

### NOTIF-107 `argocd admin notifications trigger get [NAME]`
- **Upstream**: [docs/user-guide/commands/argocd_admin_notifications_trigger_get.md#L1](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/commands/argocd_admin_notifications_trigger_get.md#L1), [troubleshooting.md#L15](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/troubleshooting.md#L15)
- **Maturity**: GA
- **Semantics**: Prints configured triggers (all or NAME) with `-o json|yaml|wide|name` (default `wide`).
- **Edge cases**: as NOTIF-105.
- **Configuration surface**: `-o`.
- **Exposure**: CLI.
- **Tests required**:
  - positive: `trigger get --config-map ./cm.yaml --secret :empty` lists triggers from file.
  - positive: `trigger get on-sync-failed -o=yaml`.

### NOTIF-108 `argocd admin notifications trigger run NAME RESOURCE_NAME`
- **Upstream**: [docs/user-guide/commands/argocd_admin_notifications_trigger_run.md#L1](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/commands/argocd_admin_notifications_trigger_run.md#L1)
- **Maturity**: GA
- **Semantics**: Evaluates trigger NAME's conditions against an Application (RESOURCE_NAME may be an in-cluster app name or a manifest file path such as `./sample-app.yaml`) and prints the result per condition; does not send.
- **Edge cases**: expression errors printed as errors; file vs name resolution rule unconfirmed.
- **Configuration surface**: none beyond globals.
- **Exposure**: CLI.
- **Tests required**:
  - positive: `trigger run on-sync-status-unknown ./sample-app.yaml` prints `true` for an Unknown fixture and `false` otherwise.
  - negative: undefined trigger → error.

### NOTIF-109 UI: notification subscriptions editor (Application summary)
- **Upstream**: [ui/src/app/applications/components/application-summary/edit-notification-subscriptions.tsx#L11](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-summary/edit-notification-subscriptions.tsx#L11), [edit-notification-subscriptions.tsx#L59](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-summary/edit-notification-subscriptions.tsx#L59), [edit-notification-subscriptions.tsx#L136](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-summary/edit-notification-subscriptions.tsx#L136), [application-summary.tsx#L79](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-summary/application-summary.tsx#L79), [application-summary.tsx#L123](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-summary/application-summary.tsx#L123), [edit-annotations.tsx#L11](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-summary/edit-annotations.tsx#L11), [ui/src/app/shared/services/notification-service.ts#L4](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/shared/services/notification-service.ts#L4), [edit-notification-subscriptsions.test.ts#L1](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-summary/edit-notification-subscriptsions.test.ts#L1)
- **Maturity**: GA
- **Semantics**: In the Application details SUMMARY panel edit mode, a "NOTIFICATION SUBSCRIPTIONS" section (hidden in view mode; subscriptions are visible in ANNOTATIONS) shows one row per annotation matching `^notifications\.argoproj\.io/subscribe\.[a-zA-Z-]{1,100}\.[a-zA-Z-]{1,100}$`: fixed prefix, trigger autocomplete (from `GET /api/v1/notifications/triggers`), service autocomplete (from `GET /api/v1/notifications/services`), free-text recipients (placeholder `my-channel1; my-channel2`), remove button; `+` adds an empty row only if the last row is non-empty. Matching annotations are removed from the ANNOTATIONS editor while editing and re-merged on save (subscription keys first, other annotations override). Choosing a trigger/service pair that already exists in another row replaces that row. Reset on mode switch.
- **Edge cases**: trigger/service names with digits, underscores or dots (e.g. `workspace1`, `slack_webhook`, `on-sync-succeeded.v2`) do not match the regex → not shown in the editor (remain in ANNOTATIONS); saving an empty row produces key `notifications.argoproj.io/subscribe..` which Kubernetes rejects (inferred); free-text values accepted even if not in autocomplete lists; AppProject UI has no subscription editor.
- **Configuration surface**: Application annotations.
- **Exposure**: UI (Application → Summary → Edit).
- **Tests required**:
  - positive: add row `on-sync-succeeded`/`slack`/`chan` → saved annotation key/value exact.
  - positive: existing matching annotations pre-populate rows; non-matching (`subscribe.on-x.workspace1`) stay in ANNOTATIONS.
  - negative: API returns 401/empty lists → editor still allows free text.
  - authorization: users without `applications, update` cannot save (API 403 surfaced).

---

## Unconfirmed

The notifications-engine module (`github.com/argoproj/notifications-engine v0.5.1-0.20260503100631-0cff13b8a717`) could not be inspected: checked `~/go/pkg/mod/github.com/` (no `argoproj` directory), `~/go/pkg/mod/cache/download/github.com/argoproj` (absent), and `<checkout>/vendor` (absent). Everything below depends on engine internals and was not verifiable from Argo CD code; docs consulted are under `docs/operator-manual/notifications/`.

- **Delivery-state key format and history size** in `notified.notifications.argoproj.io` (NOTIF-025, NOTIF-026, NOTIF-052): which tuple (trigger, condition index/hash, oncePer value, service, recipient, config namespace) forms the key, value encoding (timestamps), truncation limit. Checked: `applicationset/controllers/applicationset_controller.go`, `manifests/base/config/argocd-cm.yaml`, `cmd/argocd-notification/commands/argocd_notification.go` (only the key name and PATCH persistence are confirmed).
- **Retry/redelivery after a failed send** (NOTIF-053): whether failed deliveries are left un-recorded and retried on the next evaluation. Checked: `notification_controller/controller/controller.go`, `docs/operator-manual/notifications/troubleshooting-errors.md`, `monitoring.md`.
- **Template engine**: `templates.md` says `html/template`; whether the engine uses `text/template` or `html/template` (HTML escaping of `&`, `<`, quotes in rendered JSON bodies) is unverified (NOTIF-030). Checked: `docs/operator-manual/notifications/templates.md#L2`, `docs/operator-manual/upgrading/2.6-2.7.md#L45`.
- **Rendering of `.secrets.<key>`**: Argo CD passes `secret.Data` (`map[string][]byte`); whether templates print the string or a byte-slice representation is unverified (NOTIF-018). Checked: `util/notification/settings/settings.go#L98`, `settings_test.go#L97`.
- **Cross-secret references `$<secret-name>:<key>`** shown in `troubleshooting-errors.md#L85` — not implemented in Argo CD code; engine support unverified (NOTIF-016).
- **Missing secret key behaviour** beyond the documented warning text (literal kept vs empty) (NOTIF-016).
- **Trigger YAML as a single mapping** (not a list) as shown in `triggers.md#L74` and `examples.md#L28` — acceptance unverified (NOTIF-022).
- **Config error isolation**: whether an unknown `service.<type>` or a bad template/trigger invalidates the whole config or only that entry; whether the last good config is kept; whether unknown ConfigMap keys are ignored (NOTIF-014, NOTIF-015, NOTIF-091).
- **Error texts** for undefined trigger/template references, Slack `deliveryPolicy` misuse, Opsgenie unknown team, Alertmanager without labels, PagerDuty v2 unknown service key (NOTIF-022, NOTIF-063, NOTIF-065, NOTIF-081, NOTIF-085).
- **Non-boolean `when` results** and timing of expression compilation (parse-time vs evaluation-time errors) (NOTIF-024).
- **`oncePer` evaluating to nil/empty** (e.g. catalog triggers on never-synced apps) (NOTIF-025, NOTIF-096..099).
- **Multi-template merge precedence** when `send` lists several templates defining the same field (NOTIF-034).
- **`.serviceType` for custom-named services** (type vs name) (NOTIF-031).
- **YAML `notifications.argoproj.io/subscriptions` annotation details**: empty `trigger` list semantics, invalid-YAML handling, duplication with `subscribe.*` keys (NOTIF-049); recipient whitespace trimming for `subscribe.*` values (NOTIF-047).
- **Default subscriptions**: recipient parsing for values containing `:`, entries without `triggers`, invalid selector handling (NOTIF-051).
- **Duplicate destinations** from app + project subscriptions — whether deduplicated (NOTIF-050).
- **Application-level legacy `recipients.argocd-notifications.argoproj.io` annotations**: Argo CD code applies the legacy parser only to AppProject annotations (`controller.go#L151`); engine handling for Applications unverified (NOTIF-021).
- **Self-service internals**: state isolation between controller-level and namespace-level configs, and whether an app-namespace config may reference controller-level services/templates (NOTIF-054).
- **Per-service wire formats**: exact API endpoints/payloads for New Relic, Webex (email vs room detection), Telegram (username `@` handling, thread parameter), Opsgenie priority mapping, GitHub PR-comment tag embedding and PR discovery, GitHub deployment reuse, Google Chat thread parameters, Teams Workflows envelope, Slack `channels`/`signingSecret` semantics and thread-timestamp persistence across restarts (NOTIF-060..089).
- **Webhook**: HTTP status codes treated as success, whether templates can add headers, behaviour when a template has no entry for the subscribed webhook (NOTIF-068, NOTIF-069); duration format of `retryWaitMin/Max` (NOTIF-070).
- **AWS SQS `messageGroupId` placement** (top-level template key as documented vs an `awssqs:` section) (NOTIF-087).
- **NATS option key casing** (`url` vs `Url`) (NOTIF-089).
- **PagerDuty v1 token option name** (`token` vs `pagerdutyToken`) (NOTIF-080).
- **`slack.message` under the `slack:` section** used in `examples.md#L110` — whether such a field exists (NOTIF-061).
- **HTTP tuning defaults** and `idleConnTimeout` parse errors (NOTIF-090); which services use the `argocd-tls-certs-cm` cert resolver besides webhook (NOTIF-011).
- **CLI internals**: `RESOURCE_NAME` file-vs-cluster resolution, `console` pseudo-service implementation, exact `wide` table columns, error texts for unknown names (NOTIF-104..108).
- **`--processors-count=0`** passed explicitly (no validation in Argo CD code) (NOTIF-004).
- **Engine-provided `sync` helper namespace**: Argo CD registers none (`util/notification/expression/expr.go#L17`); an engine-side `sync` namespace is considered unlikely but could not be ruled out without engine source (NOTIF-046).
- **GitLab / other providers**: no `gitlab` service is documented at this commit; absence in the engine registry not verifiable (NOTIF-091).

## Doc/implementation differences

- **`sync.GetInfoItem` is documented but not registered.** `docs/operator-manual/notifications/functions.md#L101` documents `sync.GetInfoItem(app, name)`, but `util/notification/expression/expr.go#L17-L36` registers only `time`, `strings` and `repo`. Baseline = function unavailable (NOTIF-046).
- **Notifications controller repo-server CA flag name.** `docs/operator-manual/tls.md#L40`, `#L205` and `#L220` (and `mtls.md#L77`) name it `--argocd-repo-server-ca-cert-path`; the code registers `--repo-server-ca-cert-path` (`cmd/argocd-notification/commands/argocd_notification.go#L192` → `util/tls/tls.go#L522`). The code's own deprecation message for `--argocd-repo-server-strict-tls` also points to the non-existent `--argocd-repo-server-ca-cert-path` (`argocd_notification.go#L187`, `cmd/argocd/commands/admin/notifications.go#L81`) (NOTIF-007, NOTIF-008).
- **Self-service flag spelling.** `index.md#L128` says `--self-service-notification-enable`; the flag is `--self-service-notification-enabled` (`argocd_notification.go#L191`) (NOTIF-054).
- **`repo.FullNameByRepoURL` scope.** `functions.md#L117` says it supports only GitHub, GitLab and Bitbucket; the code is host-agnostic and returns the first two path segments, truncating GitLab subgroup paths (`util/notification/expression/repo/repo.go#L72-L84`) (NOTIF-042).
- **`GetParameterValueByName` precedence.** `functions.md#L153` says it reads the `Parameters` field; the code first checks `spec.source.helm.parameters` overrides (`util/notification/expression/shared/appdetail.go#L38-L52`) (NOTIF-045).
- **`functions.md#L126` example** uses `.app.status.RepoURL`, which is not an Application field (should be `.app.spec.source.repoURL`) (NOTIF-043).
- **CLI default object names.** `troubleshooting.md#L8-L10` says the CLI loads `argocd-notification-cm` / `argocd-notification-secret` (and calls the Secret a ConfigMap); the code uses `argocd-notifications-cm` / `argocd-notifications-secret` (`cmd/argocd/commands/admin/notifications.go#L42`) (NOTIF-104).
- **Custom names not honoured outside the controller.** Docs present `--config-map-name/--secret-name` as controller flags only; the API server (`server/server.go#L378-L381`) and the CLI (`admin/notifications.go#L42`) are hard-wired to the default names and self-service off — no doc mentions this (NOTIF-006, NOTIF-101).
- **Service list in `services/overview.md#L39-L55`** omits GCP Pub/Sub, NATS, New Relic, PagerDuty, PagerDuty V2 and Webex, which have pages in `mkdocs.yml#L82-L104` (NOTIF-091).
- **PagerDuty option name.** `services/pagerduty.md#L7` lists `pagerdutyToken`, its example uses `token:` (`#L32`) (NOTIF-080).
- **Grafana docs.** Parameter list says `apiURL` (`grafana.md#L9`) while the example uses `apiUrl` (`#L31`); the template example nests `templates:` under `data` (`#L54`), which is not a valid key layout (NOTIF-067).
- **NATS docs.** Parameter list capitalises `Url`, `Headers`, `User`, `Password`, `Nkey` (`nats.md#L7-L11`) while the example uses lowercase `url`, `headers` (`#L33`) (NOTIF-089).
- **Engine-generic examples.** `awssqs.md`, `gcppubsub.md`, `nats.md` use `.obj` and Deployment resources, and `pagerduty.md`/`pagerduty_v2.md` use `.rollout`/`Rollout`; in Argo CD the variable is `.app` and only Applications are watched. The SQS/PubSub/NATS trigger examples write `- oncePer:` as a separate list item, which creates a second condition without `when` (NOTIF-087..089, NOTIF-080/081).
- **Opsgenie docs** say to put `apiUrl`/team/`apiKey` in `argocd-notifications-secret` (`opsgenie.md#L18`) but the example configures them in the ConfigMap (`#L40-L43`) (NOTIF-065).
- **Rocket.Chat docs** refer to an "`argocd-configmap`" (`rocketchat.md#L40`); the key belongs in `argocd-notifications-cm`; the annotation example `#my_channel` is unquoted YAML (a comment) (NOTIF-083).
- **Catalog install command** differs between `index.md#L14` (`--server-side --force-conflicts`) and `catalog.md#L5` (plain apply) (NOTIF-092).
- **`ListTriggers` error handling.** Not a doc mismatch but a code defect against the proto contract: on a non-NotFound config error `server/notification/notification.go#L26-L30` falls through and dereferences a nil API (recovered as an Internal error), unlike `ListServices`/`ListTemplates` which return the error (NOTIF-102).
- **`GetAppDetails` on Helm apps without `spec.source.helm`.** `util/notification/argocd/service.go#L127` dereferences `appSource.Helm.Parameters` without a nil check; docs (`functions.md#L140`) imply it works for any Helm app (NOTIF-045).
