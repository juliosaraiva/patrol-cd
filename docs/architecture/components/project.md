# Component: `internal/project`

Status: implemented (library only; not yet wired into the API server or the controller).
Decision records: ADR-0003 (resource model), ADR-0013 (validation strategy). Upstream baseline:
Argo CD v3.5.5 (`pkg/apis/application/v1alpha1` AppProject and SyncWindows methods,
`util/argo` validation, `util/settings/impersonation.go`, `util/sourceintegrity`).

AppProject policy evaluation and Application spec validation as pure functions over the
`api/v1alpha1` types plus small lookup interfaces. The API server (admission of user intent)
and the controller (refresh conditions, every sync task) must both use this package so the
tenant boundary is enforced identically on both sides (overview.md, trust boundary 6).

## Responsibilities

- Source permission (`spec.sourceRepos`, URL normalization, globs, `!` negation) and source
  namespace permission (`spec.sourceNamespaces`, globs and `/regex/`).
- Destination resolution (server or name to a registered Cluster, in-cluster default,
  ambiguous/missing errors) and destination permission (server/name/namespace globs with
  negation, `permitOnlyProjectScopedClusters`).
- Resource kind permission (cluster/namespace allow and deny lists), live-object permission
  and the per-task sync permission check.
- Sync windows: matching, activity, `CanSync` with manual/overrun semantics, the application
  window state for API/CLI/UI, and the next state change (Patrol addition).
- Project validation (`ValidateProject`) with the upstream messages and codes; role, policy and
  group name rules; window add/update/delete helpers.
- Role helpers and project-token metadata (add, remove, lookup, normalization between
  `spec.roles[].jwtTokens` and `status.jwtTokensByRole`).
- Destination service account selection for impersonation and the impersonation settings.
- Effective project: scoped repositories and clusters, global projects (`globalProjects`).
- Orphaned-resource settings (enablement, warning, exclusions).
- Source integrity: effective policy from deprecated `signatureKeys`, git policy selection.
- Application validation (`ValidateApplication`): project existence, namespace permission,
  upstream `ValidatePermissions`, plus static multi-source reference checks and sync option
  checks.

Out of scope: reading settings ConfigMaps (`internal/settings`; this package parses only the
`globalProjects` value and the two impersonation keys it is given), RBAC evaluation
(`internal/rbac`), token signing/verification (`internal/auth`), GPG verification
(repo-server), the cluster registry (`internal/clusters`), and wiring into the API server and
controller.

## Public API

| Symbol | Purpose |
|--------|---------|
| `IsSourcePermitted(proj, repoURL)`, `NormalizeGitURL`, `SameRepoURL` | `sourceRepos` check (TEN-004/005). |
| `IsAppNamespacePermitted(proj, appNS, controlNS)`, `CheckAppNamespace` → `*NotPermittedError` | `sourceNamespaces` (TEN-010, APP-068). `NotPermittedError.Error()` is the controller wording, `APIMessage()` the API's internal one, `AppNotAllowedMessage(p)` the vague client-facing one. |
| `ClusterLookup` (`GetCluster`, `GetClusterServersByName`, `GetProjectClusters`), `ProjectClusterLister` | Lookup interfaces. `ClusterIndex` (`NewClusterIndex(clusters, inClusterEnabled)`) implements the upstream registry rules over a snapshot; `KubeLookup` implements them over a controller-runtime `client.Reader`. |
| `ResolveDestinationServer`, `ResolveDestination`, `DestinationOf`, `Destination{Server, Name, Namespace}` | Destination resolution with upstream messages (APP-003/004). |
| `IsDestinationPermitted(ctx, proj, Destination, lister)`, `IsAppDestinationPermitted` | Destination permission (TEN-006/007/008/028). |
| `IsGroupKindPermitted`, `IsGroupKindNamePermitted`, `IsResourcePermitted`, `IsLiveResourcePermitted`, `ValidateSyncPermissions(ctx, proj, obj, ResourceScope, cluster, lister)`, `MissingNamespaceMessage`, `LiveResourceCheckErrorMessage` | Kind lists and per-object checks (TEN-011..014, TEN-041, SYNC-073). |
| `MatchSyncWindows`, `SyncWindowActive`, `ActiveSyncWindows`, `InactiveAllowSyncWindows`, `CanSyncWithWindows(ws, manual, opStart, now)`, `ActiveWindows(proj, app, now)`, `CanSync(proj, app, manual, opStart, now)` | Sync windows (TEN-021..025, SYNC-083..087). Every function takes the evaluation time. |
| `GetApplicationSyncWindowsState`, `SyncWindowStatus`, `GetSyncWindowTimes`, `NextSyncWindowTransition` | API/CLI/UI window state; next open/close instant for requeueing Pending operations and for the UI (Patrol addition). |
| `ValidateProject(proj) error` (`*ValidationError{Code, Message}`, `errors.Is(err, ErrInvalidProject)`), `ValidateRoleName`, `ValidateRolePolicy`, `ValidateGroupName`, `ValidateSyncWindow`, `SyncWindowIdentityHash`, `NormalizeSyncWindowTimeZones`, `AddSyncWindow`, `UpdateSyncWindow`, `DeleteSyncWindow` | Project validation (TEN-009, TEN-017..019, TEN-021). |
| `GetRoleByName`, `RoleSubject`, `ProjectPoliciesString`, `RBACRoles`, `NormalizePolicy`, `NormalizePolicies`, `AddGroupToRole`, `RemoveGroupFromRole` | Roles (TEN-017..020). `RBACRoles` adapts roles for `rbac.Enforcer.SetProjectPolicies`. |
| `ValidateJWTTokenID`, `AddJWTToken`, `RemoveJWTToken`, `GetJWTToken`, `GetJWTTokenFromSpec`, `NormalizeJWTTokens` | Token metadata (TEN-032..035). |
| `DeriveServiceAccount`, `SelectServiceAccount(proj, app, server, ImpersonationSettings)`, `ImpersonationSettingsFromConfigMap`, `*NoServiceAccountError` | Impersonation (TEN-029/030, SYNC-093/094). |
| `ParseGlobalProjectSettings`, `GlobalProjects`, `MergeVirtualProject`, `VirtualProject`, `EffectiveProject(ctx, proj, Lookups)`, `ResolveAppProject(ctx, app, controlNS, Lookups)`, `IsNotFound` | Effective project (TEN-037..039, APP-002). `Lookups{Projects, Repositories, Clusters, GlobalProjects}`. |
| `OrphanedResourcesEnabled`, `OrphanedResourcesWarn`, `IsKnownOrphanedResourceExclusion`, `IsOrphanCandidate`, `OrphanedResourceWarningMessage` | Orphaned resources (TEN-015/016, APP-041). |
| `EffectiveSourceIntegrity` (+ `SignatureKeysMigration`), `RequiredSignatureKeys`, `MatchGitIntegrityPolicies`, `GitVerificationFor`, `HasSourceIntegrityCriteria`, `UnknownGPGModeError` | Source integrity policy (TEN-026/027, REPO-068/072/073). |
| `ValidateApplication(ctx, app, proj, lookup, ValidateOptions) []Issue`, `ValidatePermissions`, `ProjectErrorIssue`, `ValidateRefSources`, `ValidateSourceNames`, `ValidateSyncOptions`, `Issue.Condition` | Application validation (TEN-040, APP-002..005/014/015/017/026/033/036). |

### Caller integration

- **Controller refresh:** `proj, gerr, err := ResolveAppProject(ctx, app, controlNS, lookups)`;
  on error report `ProjectErrorIssue(err, app)`; else `ValidateApplication(ctx, app, proj,
  lookups.Clusters, opts)`. Set each issue with `Issue.Condition` (status True) and skip
  comparison/sync while an `InvalidSpecError` or `UnknownError` is present. Log `gerr`.
- **Sync:** for every task call `ValidateSyncPermissions` (scope from discovery); before each
  step call `CanSync(proj, app, !initiatedBy.automated, &startedAt, now)`; select the
  impersonated identity with `SelectServiceAccount`.
- **API server:** on project create/update: `NormalizePolicies`, `NormalizeJWTTokens`,
  `ValidateProject`, then `NormalizeSyncWindowTimeZones`. On application create/update:
  `ValidateSourceNames` + `ValidateApplication`, joined into
  `application spec for <app> is invalid: <Type>: <message>; ...`. Manual sync:
  `CanSync(..., manual=true, nil, now)`, refuse with `SyncBlockedAPIMessage`.

## Behavior and invariants

- **Pure and deterministic.** No function reads the clock, the environment or global
  settings; the evaluation time and settings are parameters. Inputs are not modified except
  by the documented mutators (`Normalize*`, `Add*`, `Remove*`, `Update*`, `Delete*`).
- **Deny wins** for sources and destinations: a source matching a `!` entry, or a destination
  rejected by a negated server/name/namespace entry, is refused whatever the other entries
  and their order (property-tested). Adding a non-negated entry never revokes.
- **Upstream negation semantics, including the surprising part:** a `!` entry that does not
  match counts as a match, so `["!https://github.com/org/secret"]` permits every other
  repository, and `[org/*, !org/secret]` permits repositories outside `org` too. Upstream
  code and unit tests behave this way (destinations explicitly test it); the upstream docs and
  the parity inventory (TEN-005, TEN-008 "negation-only permits nothing") say otherwise. Kept
  for parity; pinned by `TestIsNegatedSourcePermitted`, `TestIsNegatedDestinationPermitted`.
- **Sync window time zones:** the schedule is evaluated on the wall clock of the window's zone
  using the UTC offset in effect at the evaluation instant, so a DST change shifts all later
  evaluations, including the overrun "allowed at operation start" check (upstream uses
  wall-clock now for both). A window is active strictly between a fire and fire + duration.
  Property-tested against a wall-clock oracle around 2026 US/EU DST changes, fixed-offset
  zones, and caller `Location` independence.
- Manual syncs are never more restricted than automated ones, and an operation start time
  never restricts (property-tested).
- **Scoped resources are identities, not patterns.** A project-scoped Repository or Cluster
  widens the effective project by exactly itself and never revokes: a scoped repository is
  added only when its normalized URL is a non-empty glob literal that is not negated, and a
  scoped cluster's server and namespaces are added as escaped literals. The project's own deny
  entries still apply to them (property-tested, envtest-tested).
- **Project tokens are matched by jti when they have one.** The issued-at lookup is used only
  when no id is given (legacy tokens without a jti, CLI `delete-token`).
- Caches (compiled globs, regexps, schedules, time zones) are bounded LRUs, including failures,
  so invalid patterns are logged once and project editors cannot grow memory without bound.

## Failure modes

| Condition | Behavior |
|-----------|----------|
| Malformed glob in sourceRepos/destinations/kind lists/window selectors | Never matches (logged once). A malformed **negated** pattern matches everything, as upstream. |
| Malformed regex in sourceNamespaces | Never matches (fail closed, logged once). |
| Unparsable schedule or duration in any window | `CanSync` returns false with `invalid sync windows: cannot parse ...`; the controller must not sync, the API reports the error. |
| `TZ=` prefix without a schedule | Error (robfig/cron would panic). Any parser panic is converted to an error. |
| Schedule that never fires (`0 0 30 2 *`) | Treated as always active, as upstream (zero time is before now). |
| Unknown time zone | Evaluated as UTC (logged once); `NormalizeSyncWindowTimeZones` rewrites it to `UTC`. |
| Duplicate cluster servers among Cluster objects | Resolved to the lexicographically first object (upstream cannot express this). |
| Global project missing or invalid selector | Later entries ignored (upstream `break`); returned as a non-fatal error. |
| Project-scoped cluster lookup error | `IsDestinationPermitted` returns `could not retrieve project clusters: ...`; `ValidateApplication` reports only an `UnknownError`, as upstream. |
| Nil project/application/lookup | No panic: treated as "not found"/"not permitted". |
| Scoped repository URL that is a glob, a negation, or unparsable | Ignored (logged once, URL redacted); the repository must be listed in `sourceRepos` explicitly. |
| Scoped cluster with an empty server | Ignored. |

## Security notes

- Every input is untrusted (project editors, application authors). Pattern compilation is
  bounded and cached; robfig/cron is guarded against its panic; RE2 regexps run in linear
  time.
- Repository URLs with embedded passwords are redacted in condition messages and logs
  (`https://user:xxxxx@host/...`); upstream prints them verbatim.
- Because of the negation semantics above, operators must not rely on `!` entries to narrow
  a list of specific allows. Use explicit allow entries only, or `*` plus denies.
- Creating a project-scoped Repository or Cluster cannot widen the project beyond that
  resource: upstream appends their URL/server as patterns, so a scoped repository `*` or `!x`,
  or a scoped cluster `https://*`, opened the project to every repository or cluster. The
  CRDs admit such values (see API defects 4 and 5).
- A revoked project token stays revoked even when another token of the role has the same
  issued-at second (upstream accepts it through the iat fallback), and deleting by an
  unknown id never removes another token.
- The vague API message (`AppNotAllowedMessage`) must be used for both "project missing" and
  "namespace not permitted" to avoid project enumeration.
- Nothing is logged except invalid patterns, regexps, time zones and ignored scoped
  repository URLs (redacted; no secrets, no request data).

## Parity feature areas served

Recorded in `docs/parity/status/project.yaml`:

- `implemented`: TEN-004..009, TEN-011..014, TEN-016..018, TEN-021..025, TEN-028..031,
  TEN-034, TEN-037, TEN-040, APP-004, SYNC-083..086, REPO-068, REPO-072.
- `in-progress` (this package's part is done; the rest belongs to the API server, controller,
  repo-server, auth or registry): TEN-010, TEN-015, TEN-019, TEN-020, TEN-026, TEN-027,
  TEN-032, TEN-033, TEN-035, TEN-038, TEN-039, TEN-041, TEN-051, APP-002, APP-003, APP-005,
  APP-014, APP-015, APP-017, APP-026, APP-033, APP-036, APP-041, APP-068, SYNC-073,
  SYNC-087, SYNC-088, SYNC-093, SYNC-094, REPO-004, REPO-073, REPO-078, REPO-081.

## Deliberate differences and upstream discrepancies

1. **Regular expressions are RE2** (upstream regexp2). Lookarounds such as the documented
   `/^((?!not-allowed).)*$/` do not compile and never match (fail closed). No regexp2
   dependency is available.
2. **Time-zone offset** is taken at the caller's evaluation time instead of wall-clock now
   (identical in production, deterministic in tests).
3. **`ValidateProject` does not rewrite time zones**; `NormalizeSyncWindowTimeZones` does,
   after validation, reproducing upstream's write path (upstream mutates during validation;
   duplicate detection hashes the raw zone in both cases).
4. **Sync window duplicate hash** is FNV-64a over a JSON identity (upstream gob + xxhash). The
   number in the message differs; detection is identical (manualSync, description and
   syncOverrun excluded, nil and empty lists equal).
5. **Window parse errors** have code InvalidArgument (upstream returns a plain error, gRPC
   Unknown).
6. **Policy syntax check** uses `internal/rbac`, so the text after `policy syntax error:`
   differs from Casbin's.
7. **Sync options are validated** (syntax, known keys, known values) as `InvalidSpecError`
   (ADR-0013). Keys and values are case-sensitive, because the sync engine (`internal/sync`)
   matches options verbatim like upstream `HasOption`; a wrong-case spelling such as
   `ServerSideApply=True` is reported with a "did you mean" hint instead of being silently
   ignored. Upstream ignores unknown options; migrated Applications with typos will now report
   a condition. Disable with `ValidateOptions.SkipSyncOptions`.
8. **Multi-source reference checks** (APP-015) are reported as `InvalidSpecError` by
   `ValidateApplication`; upstream reports them from manifest generation (ComparisonError).
   Available refs are listed sorted (upstream: map order).
9. **Token tie order:** tokens with the same `iat` are ordered by id (upstream: random).
   Removal keeps the order (upstream swaps with the last element).
10. **Global projects** are selected by matching the project's own labels (upstream lists
    projects by selector and looks for the project; same result for a consistent cache).
11. **`HasSourceIntegrityCriteria`** follows upstream code: a matching policy with mode `none`
    (or without `gpg`) is not a criterion, contrary to the REPO-069/073 inventory text. With
    GPG disabled no policy is a criterion (upstream skips only the first lookup per process,
    REPO-076; not reproduced).
12. **Project-not-found message** uses `spec.project` defaulted to `default` (upstream prints
    the raw, possibly empty, value).
13. **The source rendering** inside the multi-source required-fields message abbreviates
    nested option structs (`&ApplicationSourceHelm{...}`).
14. **NextTransition / SyncWindowTimes** are Patrol additions (no upstream equivalent). The
    merge walk over overlapping occurrences stops after 360 occurrences (End is then zero).
15. **Scoped repositories and clusters are exact** (security, TEN-038/039, REPO-004/081).
    Upstream appends their raw URL/server/namespaces to `sourceRepos`/`destinations`, where
    they act as globs and `!` negations. Here a scoped repository is added only when its
    normalized URL is a glob literal (no `*?[]{}\`, no leading `!`, parsable), and a scoped
    cluster's server and namespaces are escaped (`glob.QuoteMeta`, leading `!` escaped). Side
    effect: an IPv6 server such as `https://[fd00::1]:6443` matches itself (upstream reads
    `[...]` as a character class and never matched it). Regular URLs and servers behave as
    upstream.
16. **No issued-at fallback for tokens with a jti** (security, TEN-033/035). Upstream
    `GetJWTToken`/`RemoveJWTToken` fall back to the iat whenever the id is not found, so a
    revoked token keeps authenticating while another token of the role was issued in the
    same second, and deleting by an unknown id removes another token. Here the iat is used
    only when no id is given. Tokens issued with a jti always have their id recorded, and
    `NormalizeJWTTokens` gives legacy metadata `id = iat`, so legacy tokens (no jti claim)
    and the CLI's iat-only `delete-token` keep working.

## API defects found (api/v1alpha1, not modified)

1. `SyncWindow.Schedule` pattern and doc comment (and `docs/reference/crds.md`) admit
   descriptors (`@daily`, `@every 1h`), which the upstream parser options reject; such a
   window is accepted by admission and then makes every `CanSync` fail. Either drop the
   descriptor alternatives from the pattern or decide to support descriptors as a
   documented difference.
2. The same pattern requires exactly five fields, so a `TZ=<zone> ` / `CRON_TZ=<zone> ` prefix
   (accepted upstream by robfig/cron) is rejected at admission.
3. There is no Reason constant for "destination not permitted"; `ReasonInvalidSpec` is used
   (`ReasonDestinationNotFound` is used for resolution failures).
4. `ClusterSpec.Server` admits glob meta characters (`https://*` passes the `isURL` rule).
   Suggested: reject `*?[]{}\` in the server (an IPv6 literal host needs `[`/`]`, so allow
   them only around the host).
5. `RepositorySpec.URL` has only a length rule: `!x`, `*` or `https://github.com/*/*` are
   admitted. Suggested: reject a leading `!` and glob meta characters (`*?{}\`, and `[`/`]`
   outside an IPv6 host).

Envtest coverage of 1 and 2: `TestEnvtestCRDAdmissionVersusValidation`; of 4 and 5 (and of
the resulting non-widening): `TestEnvtestScopedResourcesAreExact`.

## Known gaps

- Not wired: no API handler or controller calls this package yet; enforcement points
  (TEN-023 API PermissionDenied, controller pause, task failure) are the callers'.
- RBAC objects for scoped repositories/clusters, project-change authorization (APP-055,
  TEN-043, TEN-046) and project update impact checks (TEN-047) belong to the API server.
- Orphaned-resource detection (listing, hierarchy, ownership) belongs to the controller.
- GPG verification, `ARGOCD_GPG_ENABLED` handling and the local-manifest refusal belong to the
  repo-server/API.
- `KubeLookup` lists Clusters and Repositories on every call; use it with a cached client.

## Verification

```bash
go build ./...
go vet ./internal/project/
go test -race ./internal/project/
KUBEBUILDER_ASSETS=$PWD/bin/envtest/k8s/1.36.0-darwin-arm64 go test -race ./internal/project/
golangci-lint run ./internal/project/...
```

The envtest tests (`TestEnvtest*`) start one kube-apiserver for the package with the generated
CRDs from `deploy/crds` and skip when `KUBEBUILDER_ASSETS` is unset (they fail instead when
`PATROL_REQUIRE_ENVTEST=1`).
