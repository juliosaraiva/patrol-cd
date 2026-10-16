# Component: `internal/hydrator`

Status: implemented (library and controllers; not yet wired into `patrolcd controller`).
Decision records: ADR-0006 (internal APIs: repo-server and commit-server clients), ADR-0001
(independent implementation), ADR-0013 (validation). Parity area:
`docs/parity/areas/01-application.md` (APP-113..APP-137); status overlay:
`docs/parity/status/hydrator.yaml`. Companion component: [commit-server](commitserver.md).

`internal/hydrator` is the Source Hydrator controller (Beta in the Argo CD v3.5.5 baseline,
upstream `controller/hydrator/hydrator.go` and `controller/hydrator_dependencies.go`). For
Applications with `spec.sourceHydrator` it renders the dry source through the repo-server,
commits the result to the sync branch (or the `hydrateTo` staging branch) through the
commit-server, records `status.sourceHydrator`, and requests a normal refresh so the
application controller compares and syncs the hydrated commit. No upstream code is used.

## Responsibilities

- Decide when an Application needs hydration (APP-114, APP-115, APP-130, APP-131, APP-133).
- Group Applications that share a dry source and a destination branch, and hydrate each
  group in one commit (APP-116), after validating every member (APP-117).
- Render dry sources without caches and strip tracking metadata (APP-118), enforce source
  integrity on the dry revision (APP-134).
- Build the commit request: write credentials (APP-125), commit message template
  (APP-126), README template (APP-127), author (APP-128), dry commit metadata (APP-129).
- Persist `status.sourceHydrator` with optimistic concurrency (APP-137) and request the
  post-hydration refresh (APP-132).

Out of scope: comparing/syncing the sync source (application controller), the API refresh
endpoint that sets `cd.patrol.example/hydrate` and the `hydratorEnabled` settings field
(API server), dry-source webhooks (webhook), git writes (commit-server), credential
resolution (internal/repository), the CLI and UI.

## Public API

| Symbol | Purpose |
|--------|---------|
| `Setup(ctx, mgr, Deps) (*Hydrator, error)` | Registers the Application field index by hydration key and the two controllers. Returns `(nil, nil)` when `Deps.Enabled` (`hydrator.enabled`) is false. |
| `New(Deps) (*Hydrator, error)` | Unregistered hydrator (tests, custom wiring). |
| `(*Hydrator).ReconcileApp(ctx, reconcile.Request)` | app-hydration reconciler. |
| `(*Hydrator).ReconcileKey(ctx, Key)` | manifest-hydration reconciler (typed controller-runtime controller keyed by `Key`). |
| `(*Hydrator).Enqueue(Key)` | Request processing of a key (never deduplicated as covered). |
| `Key`, `KeyOf(app)` | Grouping key: normalized dry repoURL, dry targetRevision, normalized destination repoURL, destination branch (`hydrateTo.targetBranch`, else `syncSource.targetBranch`). |
| `Deps` | `Enabled`, `ControlPlaneNamespace`, `Client`, `APIReader`, `RepoServer` (`apiclient.Client`), `CommitServer` (`commitserver.Client`), `Repositories` (read), `WriteCredentials` (write), `HelmRepositories` (optional), `Projects` (default `KubeProjectResolver`), `Settings`, `ClusterInfo` (optional), `Processors` (default 5, `controller.hydration.processors`), `AppProcessors` (default 1), `PollInterval`, `StaleTimeout`, `DisableGPG` (zero value: GPG source integrity criteria enforced, as upstream's default), `AppFilter` (sharding/namespaces), `Registry`, `Logger`, `Now`. |
| `RepositoryResolver`, `WriteCredentialResolver`, `HelmRepositoryLister`, `ProjectResolver`, `ClusterInfo`, `SettingsSource` | Dependency interfaces. |
| `Repositories{Resolver}` | Adapter over `*repository.Resolver` (`ResolveForURL` with purpose read/write, `ReposForProject`, `HelmCredentialTemplates`; interface `RepositoryAccessResolver`). |
| `KubeProjectResolver`, `StaticSettings` | Default project resolution (internal/project, global projects, source namespaces) and a fixed settings snapshot. |

Metrics (registered with `Deps.Registry`): `patrol_hydrator_hydrations_total{result=hydrated|skipped|failed}`,
`patrol_hydrator_hydration_duration_seconds{result}`, `patrol_hydrator_group_applications`,
`patrol_hydrator_app_evaluations_total{outcome=hydrate|skip|error}`.

## Behavior and invariants

### app-hydration controller (one worker by default, as upstream)

Triggered by Application creation (and the initial listing), generation changes, a present
hydrate annotation, a changed `manifest-generate-paths` annotation and changes of
`status.sourceHydrator`; other status writes do not trigger it. Every reconcile returns
`RequeueAfter`: the poll interval (`PollInterval`, default `timeout.reconciliation`; 0
disables polling), earlier when a failed operation leaves its cooldown or a Hydrating
operation becomes stale.

Decision order (upstream `appNeedsHydration`): no `sourceHydrator` → skip; no
`currentOperation` → hydrate; phase Hydrating → skip (a pending hydrate request is consumed
without effect); `hydrate: hard` → hydrate (forced); `spec.sourceHydrator` differs from
`currentOperation.sourceHydrator` → hydrate; phase Failed: explicit request → retry now,
finished more than 2 minutes ago → retry, `finishedAt` missing counts as within the
cooldown, no `lastComparedDryRevision` → wait; then: no `lastComparedDryRevision` →
hydrate; otherwise resolve the dry `targetRevision` (repo-server `ResolveRevision`,
`ForceRefresh` when a hydrate was requested) and hydrate when it differs. With
`manifest-generate-paths` and a full-depth repository, `UpdateRevisionForPaths` decides
from the paths (entries relative to `drySource.path`, `/`-prefixed ones to the root).
Resolution errors skip (logged, counted). Any annotation value other than `hard` is a
normal request.

When no hydration follows, the resolved revision is written to
`lastComparedDryRevision` (an unchanged revision, or a commit outside the annotated paths).
The hydrate annotation is then removed with a JSON patch whose `test` operation requires
the evaluated value, so a newer request is never lost. Hydrating operations older than
`StaleTimeout` (default `timeout.reconciliation`, 120 s when 0) re-enqueue their key
(APP-131).

### manifest-hydration controller (`Processors` workers, one key never concurrent)

1. List the group from the cache index (sorted by qualified name; the first app supplies
   the sync branch and the dry revision, as upstream's "first app"); apply `AppFilter`.
2. Mark every member not already Hydrating with `currentOperation{phase: Hydrating,
   startedAt, sourceHydrator: <spec copy>}`; already Hydrating members keep `startedAt` and
   get the snapshot of their current spec (what is rendered now), so the recorded
   configuration always matches the hydrated output.
3. Validate every member (messages as upstream): `failed to get project "<p>": <err>`,
   `application repo <url> is not permitted in project '<p>'`, root path
   `app is configured to hydrate to the repository root (branch "<b>", path "<p>") which is
   not allowed`, Patrol addition `app is configured to hydrate outside the repository ...`
   for `..` paths, `destination repo <url> is not permitted in project '<p>'`, duplicate
   destination `app <other> hydrator uses the same destination: repo=<url>, path=<p>` on
   both (paths are not normalized, as upstream).
4. Render the first app at the dry `targetRevision`; its resolved SHA (must be a commit
   SHA) is used for all others, rendered concurrently (at most 8 at once). Requests carry
   `NoCache` and `NoRevisionCache`, the app's instance name, destination namespace, Helm
   and Kustomize settings, tracking settings, enabled source types, project name and source
   repos, effective source integrity, and the destination Kubernetes version when
   `ClusterInfo` is set. Tracking metadata is removed from each object
   (`tracking.RemoveAppInstance`). Source integrity fails closed (APP-134 messages): a
   result reporting problems always fails, whatever `DisableGPG` and the project criteria;
   with a criterion (GPG policy for the dry repository, GPG not disabled) a missing result
   fails too.
5. Skip the commit when every member's `lastSuccessfulOperation` already has this dry SHA,
   its configuration equals the snapshot being hydrated and all share one hydrated SHA,
   unless the key was forced by a hard request.
6. Read the dry commit metadata (read credentials, project-scoped only when the group has
   one project), resolve write credentials for the destination repository with the same
   project rule (none found → anonymous push with a warning), render the commit message
   template, and call the commit-server with the sync branch, target branch, author, README
   template, paths and `DryRepoURL`.
7. Failure: every member gets `phase: Failed`, `finishedAt`, `message: Failed to hydrate:
   <err>` (members without an error of their own get `cannot hydrate because application
   <first failing app> has an error` / `... and <n> more have errors`), and, once resolved,
   `drySHA` and `lastComparedDryRevision`. Group-wide errors (metadata, credentials,
   template, commit) are reported on every member.
8. Success: `currentOperation{phase: Hydrated, startedAt, finishedAt, drySHA, hydratedSHA,
   sourceHydrator}`, `lastSuccessfulOperation`, `lastComparedDryRevision = drySHA`, then
   `cd.patrol.example/refresh: normal` unless a refresh request is already pending.

Status writes are JSON merge patches of the status subresource with the observed
`resourceVersion`; on conflict the Application is re-read through `APIReader` and the
mutation re-applied (at most 5 attempts). A deleted Application leaves the group.

Redundant processing is suppressed in memory: a request is covered (not processed again)
when it was made from an Application version that the latest completed processing of the
key read or wrote, with the spec that processing hydrated (validated and rendered) for the
Application, for a reason that depends only on the Application, and for the dry revision
that processing rendered. The spec condition matters because the status written at the end
of a processing returns the newest version: a spec changed while the group was being
hydrated is in that version, but was not hydrated, so its `spec.sourceHydrator differs`
request is processed. Hard requests, cooldown/stale re-enqueues and `Enqueue` are never
covered. The state is a cache: after a restart every request is processed.

## Failure modes

| Failure | Effect |
|---------|--------|
| Settings not loaded | Reconcilers return an error (controller backoff). |
| Listing the group fails | Error returned, key retried with backoff; statuses untouched. |
| Repo-server, credentials, template or commit-server error | Group marked Failed; retried after the 2-minute cooldown, on an explicit request, or on a new dry commit. |
| Status write conflict | Re-read and re-applied; after 5 attempts the key is retried with backoff. |
| Controller crash while Hydrating | Key re-enqueued once the operation is older than `StaleTimeout`. |
| Revision resolution error during evaluation | Evaluation skipped, logged, counted as `error`; next poll retries. |

## Security notes

- Credentials (read and write) are resolved per request, forwarded in memory to the
  repo-server/commit-server, never stored in status, logs or metrics. `Key` and request
  logging use sanitized URLs; `apiclient.Repository` redacts through `LogValue`.
- Failure messages written to the status (readable by anyone who can read the Application)
  have `scheme://user:password@` userinfo redacted and are bounded to 4096 runes.
- Repository content and repo-server output are untrusted: manifests must decode as
  Kubernetes objects; paths are validated here and again by the commit-server.
- Project policy is enforced before any render: dry and destination repositories must be
  permitted, the Application namespace must be admitted by the project.

## Parity IDs covered

APP-113 (partial: controller side), APP-114, APP-115, APP-116, APP-117, APP-118, APP-123
(hydrator side), APP-124 (hydrator side), APP-125, APP-126, APP-127, APP-128, APP-129
(hydrator side), APP-130, APP-131, APP-132 (hydrator side), APP-133, APP-134, APP-136,
APP-137. Commit-server features: see [commitserver.md](commitserver.md).

## Known gaps and deviations

- **Kubernetes version** (APP-118): upstream renders the dry source without the destination
  cluster version. The task asked for it, so `Deps.ClusterInfo` (optional) supplies it;
  leaving it nil reproduces upstream exactly. Output then depends on the destination
  cluster for charts using `.Capabilities`.
- **Already-hydrated check** (APP-116 edge case): upstream inspects only the first app's
  `lastSuccessfulOperation.drySHA`, so an Application added to a group or a changed
  `syncSource.path` is not written until the next dry commit. Patrol skips only when every
  member already hydrated this dry SHA with its current configuration; otherwise the
  commit-server is called (it is idempotent: unchanged output creates no commit).
- **Hard hydrate** (APP-115): upstream's hard request still hits the same skip; Patrol
  always calls the commit-server for a hard request (no commit when nothing changed).
- **`lastComparedDryRevision`** (APP-114): upstream writes the resolved revision on every
  evaluation; Patrol writes it when no hydration follows and lets the hydration record the
  dry SHA it used, so a crash between evaluation and hydration cannot lose the new commit.
- **`hydrator.metadata` repoURL**: the request carries `DryRepoURL` (the dry repository);
  upstream records the destination repository there, which breaks the README's
  `git checkout <drySha>` when the repositories differ. Identical when they are the same.
- **Outside-repository paths**: `../x` passes the CRD pattern; Patrol rejects it per app
  during validation (upstream fails later in the commit server for the whole group).
- Not implemented here: API refresh setting the hydrate annotation, `hydratorEnabled` in the
  settings API, comparison error suffix "waiting for an external process ..." (APP-123),
  syncing from the hydrated branch (APP-132), CLI flags (APP-138), UI (APP-139).

## Requests to other owners

- `cmd/patrolcd controller`: call `hydrator.Setup` when `hydrator.enabled`; build the
  commit-server client with `commitserver.NewClient(commit.server, internal token)`; pass
  `controller.hydration.processors`, the sharding `AppFilter`, `Repositories{Resolver}` for
  read/write/Helm access, and optionally a `ClusterInfo`.
- `cmd/patrolcd commit-server`: call `commitserver.Run` (see commitserver.md).
- `internal/controller`: write Application status with patches that do not touch
  `status.sourceHydrator` (or with full updates that preserve it), and compare/sync the
  sync source (`spec.GetSources()`).
- `internal/repository`: `*repository.Resolver` must keep satisfying
  `hydrator.RepositoryAccessResolver` (`ResolveForURL`, `ReposForProject`,
  `HelmCredentialTemplates`). The tests do not import the package (it was being edited
  concurrently and did not always build); compatibility was checked with
  `go build -overlay` adding `var _ RepositoryAccessResolver = (*repository.Resolver)(nil)`.
- `hack/parity`: alias merging iterates a map and appends impl/tests in random order, so
  `go run ./hack/parity -check` can report a stale matrix right after regeneration.
- repo-server: honor `ManifestRequest.NoCache`/`NoRevisionCache` and
  `UpdateRevisionForPaths` with an unresolved `Revision` (return the resolved SHA).

## Verification

Commands and results (2026-10-16, darwin/arm64, Go 1.27.2, git CLI from PATH):

```bash
go build ./internal/hydrator/ ./internal/commitserver/            # ok
go vet ./internal/hydrator/ ./internal/commitserver/              # ok
golangci-lint run ./internal/hydrator/... ./internal/commitserver/... # 0 issues
go test -race -count=1 ./internal/hydrator/                       # ok (envtest tests skipped)
KUBEBUILDER_ASSETS=$PWD/bin/envtest/k8s/1.36.0-darwin-arm64 \
  go test -race -count=1 -v ./internal/hydrator/                  # ok, 30 top-level tests PASS
```

The envtest suite (`TestEnvHydrationLifecycle`, `TestEnvHydrationFailureFailsGroup`,
`TestEnvHydrateToStagingBranch`) runs the hydrator in a controller-runtime manager against
kube-apiserver 1.36 with the generated CRDs, a fake repo-server, and the real commit-server
over HTTP writing to a local bare repository with the git CLI. It passed 6 consecutive runs
with `-race` after the request-coverage fix. Set `HYDRATOR_TEST_LOG=1` to see the hydrator
log in the test output. `go build ./...` failed at the time in `internal/reposerver`
(another workstream's files), not in these packages.

Review follow-up (2026-10-16): (1) request coverage also compares the spec digest of the
evaluated version with the spec the processing hydrated, and `markHydrating` refreshes the
snapshot of members already Hydrating; (2) source integrity fails closed by default
(`Deps.GPGEnabled` replaced by `Deps.DisableGPG`, failing results always rejected). The
regression tests `TestReconcileKeySpecChangeDuringHydrationIsNotLost`, the new
`TestReconcileKeyCoverage` cases and the `failed` / `not performed` / `failed without
criteria` cases of `TestReconcileKeySourceIntegrity` fail on the previous logic (checked with
`go test -overlay`) and pass now:

```bash
go build ./...                                                    # ok
go vet ./internal/hydrator/ ./internal/commitserver/              # ok
golangci-lint run ./internal/hydrator/... ./internal/commitserver/... # 0 issues
go test -race -count=1 ./internal/hydrator/ ./internal/commitserver/  # ok (envtest skipped)
KUBEBUILDER_ASSETS=$PWD/bin/envtest/k8s/1.36.0-darwin-arm64 PATROL_REQUIRE_ENVTEST=1 \
  go test -race -count=3 -v ./internal/hydrator/                  # ok, TestEnv* PASS x3
```
