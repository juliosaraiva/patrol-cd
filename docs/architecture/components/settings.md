# Component: `internal/settings`

Status: implemented (library; components wire it in their own tasks). Decision records:
[ADR-0003](../adr/0003-resource-model.md) (settings stay in ConfigMaps/Secrets with Argo CD
key names, one shared parser), [ADR-0009](../adr/0009-authn-authz.md) (server key, accounts,
OIDC/Dex, RBAC ConfigMap), [ADR-0004](../adr/0004-no-redis.md) (no Redis keys).

`internal/settings` is the single settings model of every Patrol CD component. It turns the
settings ConfigMaps and Secrets of the control-plane namespace into an immutable, typed
snapshot, reports every problem as an `Issue`, keeps the snapshot current from informers,
and exposes `patrol-cmd-params-cm` as typed component parameters. Behavior was researched in
Argo CD v3.5.5 (`util/settings`, `util/env`, `util/cert`, `util/db/gpgkeys.go`,
`server/extension`, `docs/operator-manual/argocd-*.yaml`, `manifests/base`) and
re-implemented; no upstream code is copied. Two upstream data items are reused for parity:
the shipped default `resource.exclusions` list and the default source hydrator templates.

## Sources

| Patrol object | Argo CD object | Read | Label `app.kubernetes.io/part-of=patrol-cd` required |
|---------------|----------------|------|---------------------------------------------------|
| `patrol-cm` | `argocd-cm` | general settings | yes (OPS-012) |
| `patrol-rbac-cm` | `argocd-rbac-cm` | RBAC input for `internal/rbac` | no (read by name, TEN-078) |
| `patrol-cmd-params-cm` | `argocd-cmd-params-cm` | component parameters | no |
| `patrol-tls-certs-cm` | `argocd-tls-certs-cm` | hostname → PEM certificates | yes |
| `patrol-ssh-known-hosts-cm` | `argocd-ssh-known-hosts-cm` | `ssh_known_hosts` | yes |
| `patrol-gpg-keys-cm` | `argocd-gpg-keys-cm` | key ID → armored public key | yes |
| `patrol-notifications-cm` | `argocd-notifications-cm` | validated with `internal/notifications` | no |
| `patrol-secret` | `argocd-secret` | server key, TLS pair, accounts, webhook secrets, `$<key>` targets | no |
| `patrol-notifications-secret` | `argocd-notifications-secret` | **key names only** (values are never kept) | no |
| any Secret | labelled Secrets | `$<secret>:<key>` targets | yes (TEN-131) |

All names are configurable (`Names`); defaults are the `Default*Name` constants.

### Renames

Only names that literally contain the product name are renamed:

- Object names: `argocd-cm` → `patrol-cm` and likewise for every object above; label value
  `part-of=argocd` → `part-of=patrol-cd`.
- Environment variables of `patrol-cmd-params-cm` keys: prefix `ARGOCD_` → `PATROL_`
  (`PatrolEnv`). Names without the prefix are kept (`GRPC_ENABLE_TXT_SERVICE_CONFIG`,
  `REDIS_SERVER`, `REDISDB`, `REDIS_COMPRESSION`, `DEX_CONTINUE_ON_CONNECTOR_FAILURE`).
- Default addresses: `argocd-repo-server:8081` → `patrol-repo-server:8081`,
  `argocd-commit-server:8086` → `patrol-commit-server:8086`, `argocd-dex-server:5556` →
  `patrol-dex-server:5556`.
- No ConfigMap or Secret **key** contains "argocd", so no key is renamed (asserted by
  `TestConfigMapKeyRegister`). Values that embed Argo CD API names are mapped where Patrol has
  its own: the shipped `ignoreResourceUpdates` rule for Applications targets
  `cd.patrol.example_Application` and Patrol's annotation keys (`DefaultIgnoreResourceUpdates`).

## Public API

| Symbol | Purpose |
|--------|---------|
| `Parse(Input, ...ParseOption) *Settings`, `WithPrevious(prev)` | Build a snapshot; never fails. `Input{Namespace, Names, ConfigMaps, Secrets, IgnorePartOfLabel}`. |
| `(*Settings).Validate() []Issue`, `Issue{Source, Object, Key, Severity, Message}`, `HasErrors` | Every problem, sorted. `SeverityError` = value not used; `SeverityWarning` = used but suspicious. |
| `(*Settings).SourceState(src) SourceState` | `SourceStateLoaded`, `SourceStateRetained` (object missing/unlabelled, last loaded data in force) or `SourceStateAbsent` (never loaded, defaults). |
| `ValidateGroups(s, groups...)`, `ValidationGroups()` | Engine of `patrol admin settings validate [--group]` with upstream summaries (`SSO is not configured`, `N accounts`, `N resource overrides`, ...). |
| `AcceptedKeys() []KeySpec`, `LookupKey(src, key)` | Every accepted key/pattern per source with description and deprecation. |
| Getters | `URL`, `AdditionalURLs`, `StatusBadge`, `AnonymousUserEnabled`, `UserSessionDuration`, `PasswordPattern`, `Accounts`, `UserLoginsDisabled`, `UI`, `Help`, `GoogleAnalytics`, `DexConfig`, `DexConnectors`, `DexConfigResolved`, `IsDexConfigured`, `OIDCConfig`, `IsSSOConfigured`, `OIDCTLSInsecureSkipVerify`, `OIDCProviderConfig` (→ `internal/auth/oidc.Config`, external IdP or bundled Dex), `ResourceFilter` (implements `cache.ResourceFilter`), `IsExcludedResource`, `ResourceOverrides` (`lua.ResourceOverride` map), `SystemIgnoreRules`, `ResourceUpdateIgnoreRules`, `KnownTypeFields` (→ `internal/diff`), `CompareOptions`, `RespectRBAC`, `CacheRespectRBAC`, `IgnoreResourceUpdatesEnabled`, `SensitiveMaskAnnotations`, `CustomLabels`, `Include/ExcludeEventLabelKeys`, `SourceTypeEnabled`, `EnabledSourceTypes`, `Kustomize`, `KustomizeBinaryPath`, `HelmValuesFileSchemes`, `InstanceLabelKey`, `TrackingMethod`, `AllowedNodeLabels`, `InstallationID`, `Reconciliation`, `Webhook`, `InClusterEnabled`, `MaxPodLogsToRender`, `Exec`, `ApplicationFineGrainedRBACInheritanceDisabled`, `DeepLinks`, `Extensions`, `ExtensionConfigs`, `Impersonation`, `RequireOverridePrivilegeForRevisionSync`, `Hydrator`, `GlobalProjects`, `ServerSecretKey`, `SigningKeys` (→ `auth.KeySet`), `ServerCertificate`, `ResolveSecretRef`, `RBAC` (→ `rbac.SetPolicySources`/`ApplyConfigMap`), `TLSCertificates`, `TLSCertificatesForServer`, `SSHKnownHosts`, `SSHKnownHostsData`, `GPGKeys`, `Notifications`, `CmdParams`, `Params`, `Fingerprint`, `ResourceVersions`. All return copies. `Webhook.SecretsLoaded` is false while `patrol-secret` was never loaded. |
| `Sensitive` | Secret value; `String`/`Format`/`GoString`/`LogValue`/`MarshalText` print `[redacted]`; `Reveal` returns it. |
| `NewManager(client, ManagerOptions)`, `(*Manager).Start(ctx)`, `Current()`, `Subscribe(fn) (unsubscribe)` | Watcher. Options: `Namespace`, `Names`, `Debounce` (200 ms), `ResyncPeriod` (10 m), `SyncTimeout` (2 m), `IgnorePartOfLabel`, `DisableReferencedSecrets`, `Recorder` (`EventRecorder`, satisfied by client-go `record.EventRecorder`), `Metrics`, `Logger`. |
| `Metrics{Reloaded func(ReloadResult)}`, `NewPrometheusMetrics(reg)` | `patrol_settings_reloads_total{result}`, `patrol_settings_reload_duration_seconds`, `patrol_settings_issues{severity}`, `patrol_settings_last_published_timestamp_seconds`. |
| `EnsureServerSecretKey(ctx, client, namespace, ...BootstrapOption) (generated bool, err)` | Creates/updates `patrol-secret` with 32 random bytes in `server.secretkey` when missing, with optimistic concurrency. |
| `ParamsForComponent(Component) []ParamSpec`, `ParamSpecs`, `ParamKeys`, `ResolveParams(c, cmData, lookupEnv) (*Params, []Issue)`, `PatrolEnv` | `patrol-cmd-params-cm` as typed parameters. `Params.String/Bool/Int/Float/Duration/Quantity/Strings/StringMap/FlagDefault/Lookup/Values`. `ParamSpec.CaseInsensitive` (enumerations matched in any case, e.g. `*.log.format`); type `ParamLogLevel` for `*.log.level`. |
| `CoreExcludedResources()`, `DefaultResourceExclusions()`, `DefaultResourceExclusionsYAML`, `DefaultIgnoreResourceUpdates()` | Built-in exclusions and the values the install manifests ship. |
| `ParseTLSCertificates`, `ParseSSHKnownHosts`, `SSHFingerprintSHA256`, `IsValidHostname`, `GPGKeyID`, `ParseGPGPublicKey`, `ValidateExternalURL`, `ValidateAzureGraphAPIEndpoint`, `UnmarshalDexConfig`, `IsSecretRef` | Reusable validators (certificate API, CLI). |

### Command wiring of parameters

```go
params, issues := settings.ResolveParams(settings.ComponentController, cmdParamsCM, os.LookupEnv)
// log issues; exit when settings.HasErrors(issues) (invalid quantities and enumerations are
// fatal upstream)
for _, spec := range settings.ParamsForComponent(settings.ComponentController) {
    if spec.Flag != "" { /* register flag spec.Flag with default params.FlagDefault(spec.Key) */ }
}
```

Precedence: command-line flag (caller) > environment variable (`spec.Env`, as projected by the
install manifests) > `patrol-cmd-params-cm` read directly > compiled default. An empty value
selects the default unless `spec.AllowEmpty`. `*.log.format` accepts `json`/`text` in any
letter case and `*.log.level` every logrus level name in any case (upstream `strings.ToLower`
and `logrus.ParseLevel`); the level value is the slog name to use (`warning`→`warn`;
`trace`→`debug`, `fatal`/`panic`→`error` with a warning, as slog has no such levels), so
`slog.Level.UnmarshalText(params.String(key))` always succeeds. The table covers the 185 upstream keys (reference
document plus manifest-only keys, OPS-028) for 7 components; every manifest `configMapKeyRef`
wiring is reproduced (`TestParamWiringMatchesManifests`). Redis and gRPC TXT keys are accepted
and reported as having no effect.

## Behavior

- **Per-key isolation.** Each `patrol-cm` key, the RBAC `scopes`, `server.secretkey` and the
  TLS pair are applied on their own. A value that fails to parse is reported (error) and the
  value the previous snapshot accepted for that key is used (`...; keeping the last valid
  value`); without one, the default applies (`...; using the default`). Other keys are
  unaffected (rapid property `TestPerKeyIsolationProperty`). Removing a key returns it to its
  default. Fail-closed exceptions: RBAC policy documents are passed through unchanged so an
  invalid policy denies everything (TEN-078); malformed account settings disable the account
  (`auth.AccountsFromMaps`); invalid TLS certificate, known-host and GPG entries are dropped;
  invalid parameters use the compiled default.
- **Unavailable objects.** When `patrol-cm`, `patrol-rbac-cm` or `patrol-secret` is deleted,
  or `patrol-cm` loses the part-of label, the data last loaded from it (carried by
  `WithPrevious`) is parsed in its place (`SourceStateRetained`) and an error names the
  object: `ConfigMap not found; keeping the settings last loaded from it (resourceVersion N)
  until it is restored[; local accounts are disabled meanwhile]` (or `ConfigMap is ignored
  because it is not labelled ...; keeping ...`). This mirrors upstream, whose `GetSettings`
  fails (subscribers keep the last settings) and whose RBAC informer has no delete handler;
  falling back to defaults would re-enable a disabled admin, drop resource exclusions and RBAC
  deny rules, and clear webhook secrets. While `patrol-cm` or `patrol-secret` is not loaded
  every local account is disabled (upstream `GetAccounts` fails, so logins and account tokens
  fail). Without earlier data (`SourceStateAbsent`, e.g. at startup) the keys use their
  defaults, local accounts are disabled for a missing `patrol-cm`/`patrol-secret`, and
  `Webhook.SecretsLoaded` is false. To return to defaults, apply an empty labelled object.
  Retention is per object name and namespace; other ConfigMaps are optional and fail closed
  when missing (no certificates, known hosts or GPG keys means no trust).
- **Upstream interpretations kept.** `== "true"` booleans (`statusbadge.enabled`,
  `users.anonymous.enabled`, `ui.bannerpermanent`, `exec.enabled`,
  `oidc.tls.insecure.skip.verify`, `application.sync.impersonation.enabled`, `*.enable`),
  `!= "false"` booleans (`ga.anonymizeusers`, `cluster.inClusterEnabled`,
  `application.sync.impersonation.enforced`); non-canonical values are warnings.
  `strconv.ParseBool` keys (`resource.ignoreResourceUpdatesEnabled`,
  `server.rbac.disableApplicationFineGrainedRBACInheritance`,
  `application.sync.requireOverridePrivilegeForRevisionSync`) are errors when invalid.
  `help.chatText` is `Chat now!` by default and empty without `help.chatUrl`; `exec.shells`
  and `resource.customLabels` are split without trimming; label-key lists drop spaces.
  `resource.respectRBAC` accepts `""`, `normal`, `strict`. Kustomize versions come from
  `kustomize.path.<v>` and the deprecated `kustomize.version.<v>`; a duplicate is an error and
  the `path` form wins. `globalProjects` stops at the first invalid selector (upstream break).
- **Secret references.** `$<key>` reads `patrol-secret`; `$<secret>:<key>` reads a Secret
  labelled `part-of=patrol-cd`. Values are trimmed. Unresolved references stay literal and are
  warnings. They are resolved in `oidc.config`, `extension.config[.<name>]`, the webhook keys
  of `patrol-secret`, and on demand in `dex.config` (`DexConfigResolved`).
- **Validation sources.** OIDC (`issuer`/`userInfoBaseURL` http(s), `rootCA` PEM, Azure Graph
  host allow-list, durations, unknown fields), Dex (YAML map, connectors list, `url`
  required), deep links (YAML; URL templates parsed with sprig minus `env`, `expandenv`,
  `getHostByName`; `if` compiled with expr), extensions (upstream `validateConfigs` messages),
  hydrator templates, RBAC (`rbac.Validate` with the built-in policy), notifications
  (`notifications.ParseConfig` + `Config.Validate` with placeholder secret values), every
  `patrol-cmd-params-cm` value, unknown and deprecated keys of every source (with
  suggestions for common mistakes).
- **Manager.** One informer per configured object name (field selector
  `metadata.name=<name>`, so RBAC can be limited with `resourceNames`) plus one
  label-selected Secret informer for references (`DisableReferencedSecrets` removes it).
  Handlers drop resync updates (same resourceVersion) and filter by namespace/name/label
  defensively. Events within the debounce window coalesce into one reload; a reload
  publishes only when the input fingerprint (object data, retained data and source states)
  changed, so losing or restoring an object is published once. Subscribers run sequentially in the
  Manager goroutine, in snapshot order, with panic recovery; they are not called for the
  initial snapshot (`Current()` has it when `Start` returns). Each new issue is logged once
  and recorded as an Event on the object it concerns (`InvalidSettings` for errors,
  `SettingsWarning` for warnings); an issue is not re-emitted while it persists.
- **Bootstrap.** `EnsureServerSecretKey` creates the Secret (labelled) when absent or adds the
  key with an `Update` carrying the read `resourceVersion`; on `Conflict`/`AlreadyExists` it
  re-reads and keeps the other replica's key (bounded to 5 attempts). An existing key, even a
  short one, is never replaced.

## Invariants

- A `Settings` is immutable; every getter returns copies; all methods are safe for
  concurrent use. `Manager.Current` is lock-free (`atomic.Pointer`).
- Equal input yields an equal `Fingerprint` and identical issues.
- Issue messages and Events never contain Secret values: references are not quoted (a
  "reference" may be a secret starting with `$`), Secret-sourced parse errors are replaced by
  a generic message, TLS errors describe structure only (`TestSecretValuesNeverLeak`).

## Failure modes

| Condition | Result |
|-----------|--------|
| `patrol-cm`, `patrol-rbac-cm` or `patrol-secret` deleted, or `patrol-cm` unlabelled, after loading | error; last loaded data stays in force; local accounts disabled while `patrol-cm`/`patrol-secret` is unavailable |
| `patrol-cm` missing or unlabelled with no earlier data | error; defaults; local accounts disabled |
| `patrol-secret` missing with no earlier data | errors; no server key (`SigningKeys` fails), local accounts disabled, `Webhook.SecretsLoaded=false` |
| TLS/SSH/GPG ConfigMap without the part-of label | ignored + error naming the label |
| `server.secretkey` missing or < 32 bytes | error; previous key kept when there was one; `SigningKeys` fails otherwise |
| `*.log.level`/`*.log.format` not a logrus level / format | error (upstream exits); default used |
| Invalid value | see per-key isolation |
| RBAC policy invalid | error `the RBAC policy does not load; every request is denied until it is fixed` |
| Initial informer sync fails or times out | `Start` returns an error and stops its informers |
| Subscriber panics | recovered and logged; later subscribers still run |
| Bootstrap conflicts 5 times | error `still conflicting after 5 attempts` |

## Security notes

- Secret material is held as `Sensitive`; nothing logs or formats values. The notifications
  Secret's values are never read into the snapshot.
- Only labelled Secrets are reference targets, so a `patrol-cm` editor cannot read arbitrary
  Secrets of the namespace through `$<secret>:<key>`; the notifications Secret is excluded
  even when labelled.
- GPG entries holding private key material are rejected.
- `$` references inside Secret values are resolved only for the webhook keys, as upstream.
- Deleting or unlabelling a settings object never widens access: the last loaded settings,
  RBAC policy (including deny rules) and webhook secrets stay in force, and local accounts are
  disabled until `patrol-cm` and `patrol-secret` are back. Webhook handlers must reject
  payloads while `Webhook.SecretsLoaded` is false (an empty secret would otherwise mean "no
  signature check").

## Parity IDs covered

See `docs/parity/status/settings.yaml`. Summary: OPS-012, OPS-017..030 (settings and
parameter registers, server-key bootstrap), OPS-058..060, OPS-062/063, OPS-086 (log parameters), OPS-092; TEN-030,
TEN-037, TEN-071, TEN-073..075, TEN-077, TEN-078 (input and watch), TEN-090/091 (via
`internal/auth`), TEN-093, TEN-095, TEN-100, TEN-104, TEN-113, TEN-116, TEN-131;
REPO-057/058/064 (parsing and validation), REPO-093, REPO-115, REPO-116, REPO-122, REPO-129;
DIFF-014, DIFF-016, DIFF-022..025, DIFF-067..069, DIFF-071, DIFF-097, DIFF-098; SRC-003,
SRC-025, SRC-064, SRC-065; SYNC-005, SYNC-093, SYNC-094, SYNC-104; APP-070, APP-073,
APP-074, APP-126..128; API-041, API-043..045, API-047, API-085 (validate engine),
API-109..113; parameter keys of many other features (listed in the status file).

## Gaps and intentional differences

- **Not implemented here:** admin initial password and `patrol-initial-admin-secret`, the
  self-signed server certificate and the external `patrol-server-tls` Secret (OPS-030/031,
  OPS-096); Dex configuration generation and `$` escaping for Dex (TEN-114); the
  GnuPG keyring sync (REPO-064, repo-server); deep link evaluation (API server); command
  wiring of parameters and the API server restart on settings changes (OPS-095).
- **Retained objects are published:** upstream does not notify subscribers when
  `argocd-cm`/`argocd-secret` disappear; Patrol publishes one snapshot with the same retained
  values, the error issue and disabled local accounts (subscribers must apply the account
  change).
- **Last valid value instead of default** on a live update with an invalid value (upstream
  falls back to the default or fails the getter). At startup (no previous snapshot) the
  default applies as upstream.
- **Per-entry isolation** for extensions, GPG keys and certificates: upstream rejects the
  whole configuration/list on one invalid entry; Patrol drops only that entry.
- **More warnings:** unknown keys, non-canonical booleans, deprecated/removed keys (`repositories`,
  `repository.credentials`, `helm.repositories`, `configManagementPlugins`,
  `server.rbac.log.enforce.enable`) are reported; upstream ignores them silently.
- **Durations** accept Go durations and the `<n>[smhd]` form everywhere (upstream accepts only
  the latter for `users.session.duration` and `webhook.refresh.jitter`).
- **Validation in addition to upstream:** `application.instanceLabelKey` must be a qualified
  label key, `application.resourceTrackingMethod` one of the three methods, hydrator templates
  must parse, `webhook.maxPayloadSizeMB` and `server.maxPodLogsToRender` must not be negative.
- `reposerver.metrics.listen.address` uses one env name (`PATROL_REPO_SERVER_METRICS_LISTEN_ADDRESS`),
  fixing upstream's manifest/binary mismatch.
- `server.glob.cache.size` is accepted but has no effect (`internal/rbac` compiles patterns
  with the policy, TEN-086).

## Verification

```
go build ./...
go vet ./internal/settings/...
go test -race -count=1 ./internal/settings/...                         # unit + property
KUBEBUILDER_ASSETS=$PWD/bin/envtest/k8s/1.36.0-darwin-arm64 \
  go test -race -count=1 ./internal/settings/...                       # + envtest integration
```

Tests: every `patrol-cm` key family (valid fixture and a 62-case invalid table asserting key
names and upstream wording), secret references (both forms, missing Secret/key, unlabelled
Secret, no value leaks), OIDC/Dex, resource filter truth table and shipped exclusions,
RBAC composition and fail-closed policy, retention of deleted/unlabelled objects (parse and
Manager, incl. a disabled admin never re-enabled and webhook secrets kept), TLS/SSH/GPG parsing, parameter register and
manifest wiring against upstream golden files (`testdata/`), env precedence and upstream
warnings, upstream log level/format spellings, notifications validation, the Manager with a fake clientset (reload, debounce,
isolation, Events, references, deletion, panics, metrics), bootstrap conflicts with reactors,
and envtest tests (label requirement, reload, Events through the events API, concurrent
bootstrap by 8 replicas, label-selected reference resolution).
