# Component: `internal/reposerver`, `internal/reposerver/apiclient`

Status: implemented (service, HTTP server, typed HTTP client; not yet wired into
`cmd/patrolcd` nor called by the controller/API server through HTTP). Decision records:
[ADR-0006](../adr/0006-internal-apis.md) (JSON over HTTP, shared token, optional mTLS),
[ADR-0007](../adr/0007-rendering-isolation.md) (tool isolation), [ADR-0004](../adr/0004-no-redis.md)
(in-memory, disposable caches). Parity: `docs/parity/areas/02-sources-rendering.md`
(SRC-090, SRC-094..SRC-134), `03-repositories-clusters-webhooks.md` (REPO-044, REPO-057/058,
REPO-064, REPO-068..071, REPO-076); see `docs/parity/status/reposerver.yaml`.

The repo-server obtains application sources (git working copies, Helm charts, OCI
artifacts, uploaded directories), renders them with `internal/render`, and answers the
repository queries of the controller, the API server and the ApplicationSet controller.
It re-implements upstream Argo CD v3.5.5 `reposerver/repository/repository.go`
(`runRepoOperation`, `GenerateManifest`, `GenerateManifestWithFiles`, `GetAppDetails`,
`GetRevisionMetadata`, `UpdateRevisionForPaths`, ...), `reposerver/cache` (manifest and
secondary caches, manifest-generation error caching), `util/sourceintegrity` (GPG `head`
mode) and `util/app/path` (refresh paths). It never reads Kubernetes objects: callers
forward resolved credentials in every request (`apiclient.Repository`).

## Public API

### `internal/reposerver`

| Symbol | Purpose |
|--------|---------|
| `New(Config) (*Service, error)` | Prepares the root directory (stale entries of a previous process removed, root `0300`), the renderer, Helm client, caches and metrics. When `RootDir` is the root of a volume owned by another user (a Kubernetes emptyDir is owned by root and world-writable: mode 0777 with the setgid bit for the fsGroup, no sticky bit), the process cannot change its mode and works in a private subdirectory `_patrol-repo` of it instead (`prepareRoot`; found by the kind validation, KV-1). The fallback applies only when the root dir is owned by root or by the process and the error is a permission error: a root dir owned by another user is refused, and so is a `_patrol-repo` entry that is not a real directory owned by the process (a symlink, a file or someone else's directory is never adopted). Anyone who can write the mount root can still rename the private directory, so a work volume must be mounted only into the repo-server container (the chart does). |
| `(*Service)` methods | Implement `apiclient.Client` in process (every `GenerateManifest`/`GetAppDetails`/... below). Errors carry a code (`ErrorCode`) and have working-copy paths replaced by `<path to cached source>`. |
| `(*Service).Handler() (http.Handler, error)` | The HTTP API with the configured token, size caps, timeout, metrics and readiness. |
| `NewHandler(apiclient.Client, HandlerOptions)` | The HTTP API around any `apiclient.Client` (tests, fakes). |
| `Run(ctx, Config) error` / `Serve(ctx, Config, net.Listener) error` | Process wiring for `patrolcd repo-server`: TLS (files, `TLSConfig`, or a self-signed certificate like upstream; `DisableTLS`), mTLS with `ClientCAFile`, the plain-HTTP probe and metrics listener on `MetricsAddress` (see "Wire protocol"), graceful shutdown (30 s). |
| `(*Service).Close()` | Removes the private GnuPG home. |
| `RefreshPaths(annotation, sourcePath)`, `AppFilesHaveChanged(refreshPaths, changedFiles)` | `manifest-generate-paths` semantics (SRC-135 / APP-090), shared with callers. |
| `ErrorCode(err) string` | `apiclient.Code*` classification of service errors. |
| `Config` | See "Configuration". |

### `internal/reposerver/apiclient`

| Symbol | Purpose |
|--------|---------|
| `types.go` | The contract (owned jointly, extended additively): request/response types, `Client`, error codes, `Error`. This task added `GitFilesRequest.SourceIntegrity`, `GitDirectoriesRequest.SourceIntegrity`, `UpdateRevisionForPathsRequest.SyncedRefSources` and `.NoRevisionCache`. |
| `NewHTTPClient(baseURL, token, tlsConfig, opts...)` | Typed client (`*HTTPClient` implements `Client`). `baseURL` is `host:port` (https when `tlsConfig` is set) or a URL. Environment proxies are never used. Options: `WithRetry(attempts, base, max)` (default 3, 100 ms → 2 s, jittered; only connection failures and `503 unavailable`), `WithRequestTimeout` (default 10 min when the context has no deadline), `WithMaxRequestBytes` / `WithMaxResponseBytes` (100 MiB), `WithHTTPClient`. |
| `PathPrefix`, `Method*`, `Part*`, `PathHealthz`, `PathReadyz`, `Problem`, `StatusForCode`, `CodeForStatus`, `IsCode` | Wire protocol. |

## Wire protocol

`POST /repo/v1/<Method>` with `Authorization: Bearer <internal token>` (SHA-256 then
constant-time comparison), `Content-Type: application/json`, one JSON value (trailing data
rejected), size-capped (`MaxRequestBytes`). `ListPlugins` takes `{}`. Responses are JSON;
errors are RFC 9457 `application/problem+json` `{type, title, status, detail, code}` which
the client maps to `*apiclient.Error{Code, Message}`. Status mapping: `invalid_argument`
400, `unauthenticated` 401, `permission_denied` 403, `not_found` 404, `resource_exhausted`
429 (413 for oversized bodies), `unavailable` 503, `deadline_exceeded` 504, `internal` 500.
Unknown methods are 404, other HTTP methods 405, wrong content types 415.

`GenerateManifestWithFiles` is `multipart/form-data` with, in order, `request` (JSON
`ManifestRequest`), `files` (gzipped tar, streamed by the client, never buffered) and
`checksum` (hex SHA-256 of the files part, computed while streaming). The handler spools
the files part into a private temporary file within `StreamedManifestMaxTarSize`
(`file exceeded max size of N bytes`), verifies the checksum (`file checksum validation
error: calc X sent Y`) and only then calls the service. The client does not retry it.

`GET /healthz` (liveness) and `GET /readyz` (root directory usable, not shutting down)
need no token. Every request runs under `RequestTimeout` (context and body read deadline).

Probes (OPS-011): with mTLS the API listener rejects connections without a client
certificate, `/healthz` included, so kubelet probes use the plain-HTTP listener on
`MetricsAddress` (upstream metrics port 8084, `DefaultMetricsAddress`), which never asks
for client certificates: `GET /healthz` (`ok`), `GET /healthz?full=true` (also checks that
the API listener answers `GET /healthz` over loopback; `503` when it does not — upstream's
liveness probe), `GET /readyz` and, with a `Gatherer`, `GET /metrics`. When client
certificates are verified, `Serve` generates an ephemeral self-signed health-check client
certificate (client-auth only, key kept in memory), adds it to a copy of the client CA pool
and presents it in the self check (upstream `GenerateHealthCheckClientCert`); the server
certificate is not verified on that loopback connection, as upstream.

## Behavior

### Revision resolution

- git: `internal/git` `ResolveRevision` through a shared `git.RefCache`
  (`RevisionCacheExpiration`, default 3 min, coalesced listings, keyed by location and
  credential HMAC); `noRevisionCache`/`noCache` refresh it. Full SHAs never contact the
  remote; truncated SHAs are expanded in the working copy under an exclusive repository
  lock. `tagPrefix` and semver constraints as upstream (SRC-104..107).
- Helm: `helm.ResolveChartVersion` (exact versions verbatim; constraints over the index or
  OCI tags); `noRevisionCache` invalidates the cached index first. `ResolveRevision` and
  chart details always resolve uncached (upstream).
- OCI: `oci.ResolveReference` (tag, digest, constraint) → digest.

### Working copies and locking

- One persistent working copy per (`git.RepoKey(url)`, credential fingerprint) under the
  root at a random name (`r-<hex>`): different credentials never share a checkout, so a
  commit fetched with one credential cannot be read with another (the fingerprint is an
  HMAC with a per-process random key; credentials never reach disk except in the per-command
  0600 files of `internal/git`). Directories are `0000` while idle and `0700` while held
  (upstream `directoryPermissionInitializer`).
- `git.RepoLock`: holders of the same revision share a working copy (all renders allow
  concurrency because Kustomize edits run in scratch copies in `internal/render`); another
  revision waits. Each working copy owns its lock, so lock state is dropped with the copy:
  evictions and credential rotations (every rotation is a new fingerprint, hence a new
  working copy and a full clone) never accumulate lock state. A request needing several working copies (main source + `$ref` sources)
  locks them in path order, so two requests can never deadlock; a copy needed twice at the
  same revision is locked once.
- Checkout (upstream strategy): `Init`; when the previous holder used the same revision
  non-exclusively and `HEAD` already is the commit, nothing runs (built Helm dependencies are
  kept); otherwise fetch only when the commit is missing (the commit itself for shallow
  repositories, the default refspec otherwise), `checkout --force` (+ submodules unless
  `DisableSubmodules`), and on failure fetch the revision explicitly and check out
  `FETCH_HEAD`. Errors keep upstream wording (`Failed to fetch default: ...`).
- Idle working copies are evicted least recently used beyond `MaxWorkingCopies` (default
  512) and when their short-lived credentials (`CredentialsExpireAt`) expired; held copies
  are never evicted.
- Out-of-bounds symlinks (SRC-117): every checkout (main and `$ref` repositories, `.git`
  skipped), every pulled OCI artifact and every upload is scanned with
  `sandbox.CheckSymlinksContext` unless `AllowOutOfBoundsSymlinks`; results are memoized for
  12 h per (working copy, revision) and bypassed by `noCache`. Messages: `repository contains
  out-of-bounds symlinks. file: <rel>`, `oci image contains ...`, `streamed files contains
  ...`. Helm charts are checked by `internal/helm`.

### GenerateManifest

1. Ref-only sources of multi-source applications (`ref` set, no path, git) only resolve their
   revision (no checkout).
2. Resolve the main revision, then the `$ref` sources used by Helm value files and file
   parameters (SRC-095/096, upstream error texts, keys sorted in the "not one of the
   available sources" message): `sources.ref ... cannot contain any special characters`,
   `source referenced "$x", but no source has a 'ref' field defined`, `... which is not one
   of the available sources (...)`, `... Helm charts are not yet not supported for 'ref'
   sources`, `cannot reference multiple revisions for the same repository (...)`, `cannot
   reference a different revision of the same repository (...)`, `failed to get git client
   for repo <url>` (cause logged).
3. Cache lookup (unless `noCache`); then the parallelism limit (`ParallelismLimit`, wait
   time observed); then content: git checkout(s) + symlink scan + second cache lookup
   (double-checked, upstream); OCI pull into a fresh `o-` directory; Helm chart sources are
   pulled by the renderer (`noCache` cleans the chart/OCI cache first).
4. Source integrity (git sources and, stricter than upstream, git `$ref` sources whose
   problems are prefixed with the ref name) is computed before rendering and returned in
   `sourceIntegrityResult` (plus the deprecated `verifyResult` line); rendering still
   happens, as upstream (the controller turns problems into conditions and refuses sync).
5. Multi-source git sources without path and chart render nothing. The application path
   is validated (`<p>: app path does not exist|is absolute|outside root|is not a directory`,
   `invalid_argument`), then `render.Generate` runs with the build environment, `$ref` roots,
   tracking metadata (only when `instanceLabelKey` and `appName` are set; CRDs excluded),
   Kustomize remote-base git credentials (git sources only: Helm and OCI repository
   credentials never reach git hosts named by artifact content), and `OnSourceArchived`
   releasing the locks early for plugins. Manifests are serialized within `MaxRequestBytes`.

### Manifest cache (SRC-110..112)

In-memory LRU bounded by entries and bytes (`ManifestCacheEntries` 4096,
`ManifestCacheBytes` 256 MiB), TTL `RepoCacheExpiration` (24 h). Key = SHA-256 over:
repository location, repository type, source (git/OCI target revision dropped — the resolved
revision supersedes it), application name and namespace, destination namespace, kube
version, sorted API versions, Kustomize/Helm options, enabled source types, tracking method,
instance label key, installation ID, multi-source flag, ref sources (location, target
revision, chart), project name, manifest-generate-paths, source integrity policy plus the
mounted GnuPG key set and the requested revision (verification depends on them), plugin
socket names (plugin versions), a server stamp (tool binary paths, sizes and mtimes, plugin
tar exclusions, directory size limit, OCI media types) — then the resolved revision and the
resolved `$ref` commits. Credentials are never part of the key; every key includes the
application identity, so tenants cannot share entries. Hits return private copies with
`revision` rewritten. Manifest-generation error caching (SRC-111): after
`PauseGenerationAfterFailedAttempts` (3; negative disables) consecutive failures, requests get
`Manifest generation error (cached): <last error>` until `PauseGenerationMinutes` (60) passed
since the first failure or `PauseGenerationRequests` (0 = off) cached errors were served;
`noCache` bypasses it; a success resets it. As upstream, only failures after the content is
obtained count (`$ref` checkouts and their symlink scans, source integrity, application
path, rendering): failures of the main checkout (fetch, authentication, timeouts) and its
symlink scan, OCI pulls (and the OCI application path) and Helm chart downloads are returned
without being recorded, so a recovered remote or a rotated credential renders on the next
request. Chart download failures are recognized by `internal/render`'s `error fetching
chart: ` prefix until it exposes a typed error. Cancellations and deadlines are never
recorded.

### UpdateRevisionForPaths (SRC-134)

No paths → `changes=true`. Untyped repositories whose source is a chart/OCI and have no ref
sources → `changes=false`. Git: both revisions resolved; equal → no changes; otherwise the
new revision is checked out (no submodules), the synced commit fetched when missing, and the
changed files (`ChangedFiles`, cached per pair) matched against the refresh paths
(`AppFilesHaveChanged`: equal, below, or `filepath.Match` glob). `$ref` sources whose target
revision changed (from `SyncedRefSources` to `RefSources`) are compared the same way; an
unknown ref returns `changes=true` and the upstream error. When nothing relevant changed but
revisions advanced, every cached rendering of the source identity (repository, application,
source without target revision, multi-source flag) at the synced revision (and old ref
commits) is copied to the new revision, so the next `GenerateManifest` of any cluster
configuration is a hit; entries carrying a source integrity verdict are not copied (they are
re-verified). A cache miss still answers `changes=false`.

### Other methods

| Method | Notes |
|--------|-------|
| `ResolveRevision` | Per kind as above; `ambiguousRevision` = `"<input> (<resolved>)"`; `ForceRefresh` bypasses the ref cache. |
| `ListRefs` | Branches and tags (sorted short names) through the ref cache. |
| `ListOCITags` | Registry tags, cached for `RevisionCacheExpiration` per location and credentials. |
| `ListApps` | Revision resolved, shared checkout, `render.Discover` over the whole tree; cached per commit. |
| `ListPlugins` | Socket names of `PluginSocketDir` (`failed to get plugins from dir ...`). |
| `GetAppDetails` | Type: explicit block (Directory when disabled) else plugin discovery / Chart.yaml / kustomization / Directory. Helm: value files (names containing `values`, `.yaml`/`.yml`), raw in-bounds `values.yaml`, flattened parameters of `helm show values` merged with the selected value files (no env substitution; `$ref` files from git ref sources, locked together with the main checkout; globs; `ignoreMissingValueFiles`), file parameters verbatim. Kustomize: full build (edits, selected version, empty namespace; remote-base git credentials for git sources only) → sorted container/initContainer images at any depth. Plugin: parameter announcements. Chart sources are pulled, OCI artifacts extracted. Cached per resolved revision. |
| `GetRevisionMetadata` | Requires a full or ≥7-hex SHA (`revision <r> must be resolved`); author, date, tags, message, `Argocd-reference-commit-*` references; source integrity and the legacy signature line on request; cache bypassed when verification is requested or recorded. |
| `GetRevisionChartDetails` | Version resolved uncached, chart pulled, `helm show chart` → description, home, maintainers (`Name <email>`). |
| `GetOCIMetadata` | Manifest annotations (created, authors, docs, source, version, description). |
| `GetHelmCharts` | Index listing (refreshed); refused for OCI repositories. |
| `GetGitFiles` | Legacy mode: `git ls-files --full-name -z -- <pattern>` (git pathspec, tracked files) run through `internal/sandbox` with an exact environment; new globbing: `doublestar` over the real root, matches resolving outside the repository ignored, directories skipped. Files are read through `os.Root` (symlinks leaving the repository are skipped, never followed), bounded by `MaxGitFilesBytes`. A failed source integrity check fails the request (`permission_denied`). |
| `GetGitDirectories` | Directory walk; dot-directories only with `IncludeHiddenDirectories`; `.git` never. |
| `TestRepository` | git `ls-remote`; Helm index (or OCI ping with `enableOCI`, which requires a host-and-path URL); OCI ping. `error testing repository connectivity: ...`, `unsupported repository type "<t>"`. |
| `GenerateManifestWithFiles` | Upload extracted with `sandbox.ExtractTarGzContext` (absolute names, `..`, escaping/absolute symlinks, links outside, devices rejected; `StreamedManifestMaxExtractedSize`) into a fresh `s-` directory, symlink scan, rendered with revision `streamed` (chart field ignored: the upload is the chart), never cached, always removed. |

### Source integrity (REPO-064, REPO-068..071, REPO-076)

Policies are matched with `project.GitVerificationFor` (several matches fail closed,
mode `none` or no gpg block skips, `DisableGPG` skips). Mode `head`: when the requested
revision is a remote tag (`refs/tags/x` or a short name listed as a tag) its tag object is
verified (fetched when the commit was already present; shallow working copies fall back to
the commit), else the commit; `git verify-commit|verify-tag --raw` against a private GnuPG
home built from `GPGKeysDir` (files named by 16-hex key ID; rebuilt when the directory
content changes, keys imported with `gpg --batch --no-autostart --import` and trusted
ultimately; readers hold the home during verification). A signature passes when good and made
by an allowed key (short IDs or fingerprints; the signing key or its primary key). Problems use
upstream wording; check name `GIT/GPG`. Mode `strict` fails closed with a problem (history
walk with seal commits not implemented).

### Configuration (settings → `Config`)

| Setting (`patrol-cmd-params-cm`, upstream env) | Field |
|------------------------------------------------|-------|
| `reposerver.listen.address` (+ port) | `ListenAddress` |
| `reposerver.metrics.listen.address` (+ port, upstream 8084) | `MetricsAddress` (probe and metrics listener; empty disables it, `DefaultMetricsAddress` = `:8084`) + `Gatherer` |
| `reposerver.parallelism.limit` | `ParallelismLimit` |
| `reposerver.disable.tls`, `reposerver.client.ca.path`, TLS cert files | `DisableTLS`, `ClientCAFile`, `TLSCertFile`/`TLSKeyFile` or `TLSConfig` (min/max versions and ciphers: caller builds `TLSConfig`) |
| `reposerver.repo.cache.expiration` | `RepoCacheExpiration` |
| `--revision-cache-expiration` | `RevisionCacheExpiration` |
| `reposerver.max.combined.directory.manifests.size` | `MaxCombinedDirectoryManifestsSize` (`0` upstream = unlimited → negative) |
| `reposerver.plugin.tar.exclusions`, `reposerver.plugin.use.manifest.generate.paths` | `PluginTarExclusions`, `PluginUseManifestGeneratePaths` |
| `reposerver.allow.oob.symlinks` | `AllowOutOfBoundsSymlinks` |
| `reposerver.streamed.manifest.max.tar.size` / `.max.extracted.size` | `StreamedManifestMaxTarSize` / `StreamedManifestMaxExtractedSize` |
| `reposerver.helm.manifest.max.extracted.size` (+ `disable.`) | `HelmManifestMaxExtractedSize`, `DisableHelmManifestMaxExtractedSize` |
| `reposerver.oci.manifest.max.extracted.size` (+ `disable.`), `reposerver.oci.layer.media.types` | `OCIManifestMaxExtractedSize`, `DisableOCIManifestMaxExtractedSize`, `OCILayerMediaTypes` |
| `reposerver.enable.git.submodule` | `DisableSubmodules` (inverted) |
| `reposerver.enable.builtin.git.config` | `DisableBuiltinGitConfig` (inverted) |
| `reposerver.include.hidden.directories` | `IncludeHiddenDirectories` |
| `reposerver.grpc.max.size` (MiB) | `MaxRequestBytes` |
| `reposerver.helm.user.agent` | `HelmUserAgent` |
| `ARGOCD_EXEC_TIMEOUT` / `ARGOCD_EXEC_FATAL_TIMEOUT` | `ExecTimeout` / `ExecKillGrace` |
| `ARGOCD_GIT_ATTEMPTS_COUNT`, `..._RETRY_DURATION`, `..._RETRY_MAX_DURATION` | `GitRetryAttempts`, `GitRetryBase`, `GitRetryMax` |
| `ARGOCD_PAUSE_GEN_AFTER_FAILED_ATTEMPTS`, `..._MINUTES`, `..._REQUESTS` | `PauseGeneration*` |
| `ARGOCD_GPG_ENABLED` | `DisableGPG` (inverted) |
| mounted `ssh_known_hosts`, TLS certs, GnuPG keys | `SSHKnownHostsPath`, `TLSCertsDir`, `GPGKeysDir` (empty = defaults `/app/config/ssh/ssh_known_hosts`, `/app/config/tls`, `/app/config/gpg/source`, applied by `New`; `TrustMaterialDisabled` (`-`) turns one off; missing files = none) |
| `patrol-internal-token` Secret | `Token` / `TokenFile` |

### Metrics

With `Config.Registerer`: `patrol_reposerver_requests_total{method,code}`,
`patrol_reposerver_request_duration_seconds{method}`,
`patrol_reposerver_render_duration_seconds{source_type,outcome}`,
`patrol_reposerver_git_requests_total{operation=ls-remote|fetch|checkout,outcome}` and
`patrol_reposerver_git_request_duration_seconds{operation}` (ls-remote counts remote ref
listings including those served by the ref cache), `patrol_reposerver_cache_requests_total
{cache,result}` (manifest, appdetails, listapps, revisionmetadata, chartdetails, ocimetadata,
ocitags, gitfiles, gitdirs, gitfileschanges), `patrol_reposerver_parallelism_wait_seconds`,
`patrol_reposerver_working_copies`, `patrol_reposerver_manifest_cache_entries|bytes`, plus the
`internal/render` metrics. Labels never carry repository URLs.

## Security notes

- Credentials: only in memory and per-command 0600 files (git/helm/oci packages); never in
  argv, logs, cache keys (HMAC fingerprints only), error messages (git/helm/oci redaction plus
  `git.SanitizeURL` in repo-server messages). Requests are never logged whole (nested
  `Repository` values would bypass `LogValue` redaction); logs carry method, code and the
  sanitized error.
- Tenancy: working copies and secondary caches (app details, listings, metadata, files,
  directories, changed files, OCI tags, chart details) are keyed by credential fingerprint;
  the manifest cache is keyed by application identity.
- Untrusted content: symlink scans, confined value files (`internal/helm`), `os.Root` file
  reads for ApplicationSet files, safe archive extraction, bounded responses and uploads,
  hardened `git ls-files` environment (no global/system config, hooks and fsmonitor off).
- The internal token is compared in constant time over SHA-256 digests; mTLS requires and
  verifies client certificates when `ClientCAFile` is set. The ephemeral health-check
  certificate trusted in addition never leaves the process, and API methods still need the
  token. The probe listener serves only health, readiness and metrics.
- Kustomize remote bases receive git credentials only from git sources (never registry or
  Helm repository credentials), in `GenerateManifest` and `GetAppDetails` alike.
- Error paths: `<root>/<entry>` (all spellings of the root, longest first) become
  `<path to cached source>` (SRC-118); errors keep their codes and sentinels.

## Differences from upstream

- HTTP/JSON instead of gRPC (ADR-0006); uploads are one multipart request with a trailing
  checksum part instead of a client stream.
- No Redis: caches are per replica; no distributed revision-cache lock (SRC-107).
- Working copies are keyed by credentials as well as location; clones of a previous process
  are discarded at startup instead of being re-associated (SRC-118 restart reuse).
- `TestRepository` returns `verifiedRepository: true` on success (upstream always false).
- `$ref` git sources are verified against source integrity policies too (upstream verifies
  only the main source); integrity verdicts are never copied by `UpdateRevisionForPaths`; the
  manifest cache key includes the GnuPG key set and the requested revision when a policy is
  set.
- Annotated tags are detected through the remote tag listing; a lightweight tag's problem
  names the tag instead of the commit.
- `GetGitFiles` never follows symlinks out of the repository in legacy mode and skips
  directories in new globbing mode; `GetGitDirectories` never lists `.git`.
- Keys accepted by `head` mode include the primary key of a signing subkey.
- `GetHelmCharts` refuses OCI repositories explicitly; chart listing order is by name.
- `GetAppDetails` does not offer Helm/OCI repository credentials to Kustomize remote bases
  (upstream passes the source repository's credentials for every source kind).
- The `?full=true` self check is an HTTP `GET /healthz` on the API listener instead of a
  gRPC health call.

## Requests to other components

- `cmd/patrolcd`: call `git.MaybeAskpassMain()`, `sandbox.MaybeHelperMain()`,
  `jsonnet.MaybeJsonnetMain()` first in `main`; `repo-server` subcommand → `reposerver.Run`
  with `Config` built from `internal/settings` params (table above).
- Controller / API server: construct `apiclient.NewHTTPClient`; pass `ManifestRequest.AppName`
  as the instance name; fill `UpdateRevisionForPathsRequest.SyncedRefSources` and the same
  `Source`/`AppName`/`HasMultipleSources` as the matching `ManifestRequest`; compute refresh
  paths with `reposerver.RefreshPaths`; skip `UpdateRevisionForPaths` for shallow repositories.
- ApplicationSet controller: pass `SourceIntegrity` in `GitFilesRequest`/`GitDirectoriesRequest`.
- `internal/git`: (1) `LsSignatures`-style history walk with seal commits for GPG `strict`
  (REPO-071); (2) `LsFiles` (pathspec listing) so the repo-server need not run `git ls-files`
  itself; (3) event hooks (ls-remote/fetch, cache hit vs remote call) for precise metrics;
  (4) optional: `RepoLock.Forget(path)` — no longer needed here (each working copy owns
  its lock), but useful to other users of one shared `RepoLock`.
- `internal/render`: a typed error (or sentinel) for chart pull failures in `pullChart`, so the
  repo-server need not match the `error fetching chart: ` prefix to keep them out of the
  generation error cache; optionally an `Input` field accepting an already-pulled chart.
- `cmd/patrolcd` / install manifests: set `MetricsAddress` (default `:8084`) and point the
  liveness probe at `/healthz?full=true` and readiness at `/healthz` on that port (OPS-011).

## Known gaps

- GPG `strict` mode is not implemented (fails closed).
- `reposerver.git.lsremote.parallelism.limit` and `reposerver.git.request.timeout` are not
  applied (the git CLI is bounded by `ExecTimeout`); `revision.cache.lock.timeout` does not
  apply (no distributed lock).
- Submodule, LFS, proxy and SSH paths are delegated to `internal/git` and not re-tested here.
- Plugin-based rendering is exercised with a minimal in-test cmpwire sidecar only.

## Verification

`go test -race ./internal/reposerver/...` — hermetic: local bare repositories (`file://`,
`AllowLocalRepositories`), real `git`, `helm` (`/opt/homebrew/bin/helm` or `PATROL_TEST_HELM`),
`kustomize` (`bin/kustomize`), `gpg` (keys generated in short temporary homes), an
`httptest` Helm repository, the in-process OCI registry (`internal/oci/ocitest`), a cmpwire
sidecar on a unix socket, and every API call made through the real HTTP client against
`httptest` servers (plus a TLS/mTLS `Serve` test). Tests needing a missing tool are skipped.
Exact commands and results are recorded by the implementing task.
