# Component: `internal/render`, `internal/render/jsonnet`, `internal/render/cmpwire`

Status: implemented (library only; not yet wired into the repo-server, which owns checkout,
locking, caching and the `GenerateManifest` / `GetAppDetails` / `ListApps` / `ListPlugins`
RPCs). Parity area: `docs/parity/areas/02-sources-rendering.md` (SRC-001..SRC-096 rendering
features; see `docs/parity/status/render.yaml`). Related decisions:
[ADR-0006](../adr/0006-internal-apis.md) (CMP over a unix socket),
[ADR-0007](../adr/0007-rendering-isolation.md) (tool isolation, Jsonnet child process,
plugins never receive credentials).

`internal/render` turns **one** application source that is already on local disk into
Kubernetes objects. It re-implements upstream Argo CD v3.5.5
`reposerver/repository/repository.go` (`GenerateManifests`, `findManifests`, `helmTemplate`,
`getResolvedValueFiles`, `mergeSourceParameters`, `newEnv`, `getHelmRepos`,
`runConfigManagementPluginSidecars`), `util/app/discovery`, `util/kustomize`, `util/cmp` (client
side) and the Jsonnet VM setup. It uses `internal/helm` (Helm 4 CLI, chart pulls, value-file
confinement), `internal/sandbox` (process execution, tar, symlink checks), `internal/tracking`
(optional tracking metadata), `internal/project` (dependency repository permission message) and
`internal/oci` (URL helpers).

## Responsibilities

1. Resolve the application path inside the checkout (or pull a Helm chart source).
2. Merge the in-repository parameter override files into a copy of the source.
3. Determine the tool: explicit block, else CMP discovery, else Chart.yaml / kustomization,
   else Directory, honoring the enabled source types.
4. Render: Directory (YAML/JSON + Jsonnet in a sandboxed child), Helm, Kustomize (edits in a
   scratch copy), or a Config Management Plugin sidecar.
5. Post-process: flatten `List` kinds, drop null lists, validate every object, optionally write
   tracking metadata, enforce count and size limits.
6. Helpers for the repo-server: `Discover` (ListApps), `ListPlugins`, `PluginParameters`
   (GetAppDetails for plugins), `ExplicitType`, `IsSourceTypeEnabled`, `SplitYAML`.

Out of scope (callers): git/OCI checkout, revision resolution, out-of-bounds symlink scan of the
whole checkout, repository locks, manifest/error caches, multi-source orchestration (one call per
source; ref-only short circuit), `$ref` checkouts (the caller passes their local roots), spec
validation messages, error path sanitization (`<path to cached source>`), settings parsing.

## Public API (`internal/render`)

| Symbol | Purpose |
|--------|---------|
| `New(Config) (*Renderer, error)` | `Config{Helm *helm.Client, KustomizePath, HelmPath, GitPath, ToolPATH, WorkDir, Exec sandbox.Limits, Jsonnet jsonnet.Runner, PluginSocketDir, PluginTarExclusions, PluginUseManifestGeneratePaths, CMPRetryAttempts, CMPRetryBackoff, CAData func(host) []byte, Limits, Registerer, Logger}`. Defaults: `kustomize`/`helm`/`git` from `ToolPATH` (else the `PATH` of `sandbox.BaseEnv`), `WorkDir` = `os.TempDir()` (must be absolute), socket dir `/home/patrol/cmp-server/plugins`, 3 CMP config attempts with linear 1 s backoff. Invalid tar exclusion globs fail with `error verifying exclusion pattern "<p>": ...`. |
| `(*Renderer).Generate(ctx, Input) (*Result, error)` | Renders one source. Safe for concurrent use. |
| `Input` | `RootDir` (absolute checkout root, never modified), `AppPath` (relative; default `Source.Path`), `Source`, `Revision` (resolved; for chart sources the version to pull, resolved from `targetRevision` when empty), `Repo`, `AppName` (instance name), `ProjectName`, `Namespace` (destination), `KubeVersion`, `APIVersions`, `Repos`, `HelmRepoCreds`, `ProjectSourceRepos`, `HasMultipleSources`, `RefSources map["$ref"]dir`, `KustomizeOptions`, `HelmOptions`, `EnabledSourceTypes`, `ManifestGeneratePaths`, `GitCreds` (Kustomize remote bases only), `Tracking *Tracking{Tracker, Method, InstallationID}`, `OnSourceArchived func()`, `Limits`. |
| `Result{Manifests, SourceType, Commands}` | Objects in tool order; effective type (`Helm`, `Kustomize`, `Directory`, `Plugin`); executed helm/kustomize command lines with checkout roots replaced by `.`. |
| `Limits` | `MaxCombinedDirectoryManifestsSize` (10 000 000, upstream `10M`), `MaxManifests` (100 000), `MaxOutputBytes` (256 MiB: tool stdout, CMP response, serialized result, per Directory file), `MaxScratchCopyBytes` (1 GiB), `MaxScratchCopyFiles` (500 000), `MaxOverrideFileBytes` (1 MiB), `MaxCMPTarBytes` (1 GiB). Zero = default, negative = unlimited (upstream's `0` for the directory limit maps to `-1`). `Input.Limits` overrides `Config.Limits` field by field. |
| `(*Renderer).Discover(ctx, root, appPath, enabled)` | ListApps: `{".": "Plugin"}` when a plugin claims the path, else every directory below `appPath` with a `*Chart.yaml` (Helm) or kustomization file (Kustomize; wins in the same directory). |
| `(*Renderer).ListPlugins()` | Socket file names without `.sock` (`<name>[-<version>]`), lexical. Missing dir: `failed to get plugins from dir <dir>, error=...`. |
| `(*Renderer).PluginParameters(ctx, Input)` | Parameter announcements of the selected plugin (reduced env: name, repo URL, path, target revision + plugin env/params; whole repo streamed). |
| `ExplicitType`, `IsSourceTypeEnabled`, `KustomizationNames`, `IsKustomization`, `OverrideFiles(appName)`, `SourceFile`, `LegacySourceFile`, `SplitYAML` | Shared helpers. |
| `ErrorCode(err)` | Repo-server code (`apiclient.CodeInvalidArgument` for upstream FailedPrecondition cases, `CodeNotFound` for an empty value-file glob, `CodePermissionDenied` for non-permitted dependency repos), `""` otherwise. |
| Errors | `ErrInvalidManifest`, `ErrTooManyManifests`, `ErrOutputTooLarge`, `ErrExceededMaxCombinedManifestFileSize` (upstream text), `ErrScratchCopyLimit`, `ErrHelmNotConfigured`, `*GlobNoMatchError`; `ErrMessageAppPathDoesNotExist` (hydrator messaging). |
| Constants | `SkipFileRenderingMarker` (`+argocd:skip-file-rendering`), `PatrolSkipFileRenderingMarker` (`+patrol:skip-file-rendering`), `PatchesCommandComment`, `Default*`. |

### `internal/render/jsonnet`

| Symbol | Purpose |
|--------|---------|
| `Evaluate(Request) (Response, error)` | In-process evaluation (the child runs it). Fresh VM per file; ext vars / TLAs (string or code); import lookup = importing file's dir, then `JPaths` last to first; every import (and the evaluated file) located with `Lstat` only, then its symlink-resolved path must be a regular file inside `RepoRoot`, then read with `MaxImportBytes` (64 MiB). Output capped by `MaxOutputBytes`; stack by `MaxStack` (500). Evaluation stops at the first failing file (`Response.Error{Index, Message}`). |
| `Runner{Path, Limits, NoRlimits}.Run(ctx, Request)` | Re-executes `Path` (default `os.Executable()`) as `<bin> jsonnet-eval` through `sandbox.Run` (JSON request on stdin, response on stdout). Defaults: 90 s timeout, 60 CPU seconds, 4 GiB address space (rlimits need `sandbox.MaybeHelperMain` in the same binary), stdout cap = output cap + 1 MiB. Refuses to run when `MaybeJsonnetMain` was not called. Errors wrap `ErrChildFailed` (and `sandbox.ErrTimeout` / `ErrOutputLimit`). |
| `MaybeJsonnetMain()` | Must be called at the top of `main` of every binary used as the evaluator (`cmd/patrolcd`, test binaries), right after `sandbox.MaybeHelperMain()`. |

### `internal/render/cmpwire` (CMP protocol, shared with the cmp-server)

HTTP/1.1 over `<socket dir>/<metadata.name>[-<spec.version>].sock`:
`GET /plugin/v1/config` → `ConfigResponse{isDiscoveryConfigured, provideGitCreds}`;
`POST /plugin/v1/match|generate|parameters` with body `application/vnd.patrol.cmp.stream.v1` =
4-byte big-endian length + JSON `Metadata{appName, appRelPath, checksum (hex SHA-256), size, env[]}`
(≤ 4 MiB) + exactly `size` bytes of tar.gz → `MatchResponse`, `ManifestResponse{manifests[]}`,
`ParametersResponse{parameterAnnouncements[]}`. Errors: non-2xx with
`application/problem+json` body `Error{code, message}`. The client deadline travels in
`Patrol-Cmp-Deadline` (RFC 3339 nano). Helpers: `EncodeMetadata`/`WriteMetadata`,
`ReadMetadata` (validates: hex checksum, non-negative size, `appRelPath` local
(`illegal appPath: out of workDir bound`), env names), `PayloadReader` (exact size and checksum,
`file checksum validation error`), `EnvEntries`, `IsLocalRelPath`. The sidecar
(`internal/cmp`, not yet written) must import this package.

## Behavior

### Application path and tool detection (SRC-001..004)

- `AppPath`/`Source.Path` is resolved with `sandbox.SecureJoin` inside `RootDir`; errors are
  upstream's: `<p>: app path is absolute`, `<p>: app path outside root` (also for a symlink
  leaving the root), `<p>: app path does not exist`, `<p>: app path is not a directory`.
- Override files are merged first (below), then: exactly one of `kustomize`/`helm`/`directory`/
  `plugin` fixes the type (`multiple application sources defined: Kustomize,Helm,...` in that
  order, wrapped as `error getting app source type: ...`); a disabled explicit Helm/Kustomize
  renders as Directory. Plugin is never disabled.
- Implicit: every socket in the plugin directory is probed in lexical order (config first;
  plugins without discovery are skipped; the repository is archived once and streamed to each
  discovery-enabled plugin); the first match wins. Any failure (no directory, dial errors) falls
  back to the built-ins: a file in the app directory whose name ends with `Chart.yaml` → Helm, a
  `kustomization.yaml|yml|Kustomization` → Kustomize (wins over Helm), else Directory. Disabled
  tools are not detected.

### Parameter override files (SRC-092, SRC-093)

Merged in this order, later files winning, with RFC 7386 merge-patch semantics on the JSON form
of the source:

1. `.argocd-source.yaml` (upstream generic file, honored for migration and Argo CD Image Updater),
2. `.patrol-source.yaml`,
3. `.argocd-source-<instance>.yaml`,
4. `.patrol-source-<instance>.yaml`,

where `<instance>` is `Input.AppName` (`<ns>_<name>` for Applications outside the control-plane
namespace). So the Patrol name wins over the upstream name at the same specificity, and
application-specific files win over generic ones. `path`, `repoURL`, `chart` and
`targetRevision` are restored afterwards. Files must be regular files resolving inside the
repository (`<file>: override file resolves outside the repository`), at most
`MaxOverrideFileBytes`; directories with those names are ignored; an empty file is an empty
patch; errors read `error while parsing source parameters: <file>: ...`. The caller's
`Input.Source` is never modified.

### Build environment (SRC-091)

`ARGOCD_APP_NAME`, `_NAMESPACE` (destination), `_PROJECT_NAME`, `_REVISION`, `_REVISION_SHORT`
(7), `_REVISION_SHORT_8`, `_SOURCE_REPO_URL`, `_SOURCE_PATH`, `_SOURCE_TARGET_REVISION`, each
also as `PATROL_APP_*` with the same value (ARGOCD set first). Entries with a name and an empty
value are kept (upstream `Env.Environ`). Substitution is `os.Expand`: `$VAR`/`${VAR}`, unknown →
empty, `$$` → `$`. Substituted: Helm parameters, value-file and file-parameter paths (after the
`$ref` prefix for ref paths), Jsonnet ext vars and TLAs, Kustomize images and labels, annotations
when `commonAnnotationsEnvsubst`, plugin env values (which also see `KUBE_VERSION` and
`KUBE_API_VERSIONS`). Exported as process environment to `kustomize` (build and `edit add
component`) and sent to plugins; never to helm (as upstream).

### Directory and Jsonnet (SRC-005..018)

Walk order is lexical; only the app directory unless `recurse` (symlinked directories are never
followed). Candidates match `^.*\.(yaml|yml|json|jsonnet)$`; symlinked files must resolve inside
the root (`illegal filepath in symlink at "<rel>"`; dangling or non-regular targets are skipped
with a warning); `exclude` then `include` (gobwas-glob semantics without separators, so `*`
crosses `/`; `{a,b}` alternation; invalid pattern → matches nothing). Patterns with alternations
are evaluated through an equivalent RE2 expression (linear time; property-tested against gobwas);
files containing `+argocd:skip-file-rendering` or `+patrol:skip-file-rendering` are skipped and
not counted. The summed size of YAML/JSON candidates is checked against
`MaxCombinedDirectoryManifestsSize` before parsing. Content is read as UTF-8 with BOM and UTF-16
(BOM) conversion. YAML: multi-document; on a decode error the file is an error if it already
produced an object or contains `apiVersion:`, `kind:` and `metadata:`, otherwise ignored. JSON:
exactly one value (`Found multiple objects in "<f>". Only single objects are allowed in JSON
files.`). Jsonnet files (unless `Directory` is disabled) are evaluated in one sandboxed child call
per render with `JPaths = [appPath] + libs` (libs resolved from the repository root, must stay
inside it: `file '<lib>' resolved to outside repository root`); output must be an object or an
array of objects (`Failed to evaluate jsonnet "<f>": ...`, `Failed to unmarshal generated json
"<f>": ...`).

### Helm (SRC-019..047, SRC-095/096)

`helm template . --name-template <release> [--namespace] [--kube-version] --set… --set-string…
--set-file… --values <files…> [--values <inline>] --api-versions… [--include-crds]
[--skip-schema-validation] [--skip-tests]` through `internal/helm` (isolated helm home per call,
Chart.lock cleanup, `<api versions removed>` rewriting). Release name: `releaseName`, else the
name part of the instance name. Namespace: `helm.namespace`, else the destination. Kube version
and API versions: source values replace the cluster's. Value files: env-substituted, `$ref/…`
resolved in the referenced root (multi-source only; single-source `$x` is an env reference),
relative to the chart dir or `/` = repository root, confined (`file '<f>' resolved to outside
repository root`), remote URLs only with an allowed scheme (`HelmOptions.ValuesFileSchemes`; nil
allows none), doublestar globs expanded in place with explicit entries keeping their position and
duplicates removed (`values file glob "<p>" matched no files`, `CodeNotFound`), missing files and
empty globs skipped with `ignoreMissingValueFiles`. `valuesObject` replaces `values`; neither is
substituted. Dependencies are built only after a missing-dependency template error, through
`helm.EnsureDependencies` (lock + marker; writes `charts/` into the checkout like upstream) with
repository credentials matched by exact URL, name, credential template prefix, then OCI prefix;
an unreachable repository not permitted by the project yields `helm repos <r…> are not permitted
in project '<p>'` (`CodePermissionDenied`). Chart sources are pulled (`helm.PullChart`, version
resolved when `Input.Revision` is empty, which then becomes `ARGOCD_APP_REVISION`) into a fresh
directory removed after the call.

Repository proxy (SRC-136): `Input.Repo.Proxy`/`NoProxy` apply to every Helm operation of the
call: `helm template` and `dependency build` (the source repository's proxy, also for
dependency repositories, as upstream) and, for chart sources, the version lookup and pull (the
chart repository, which is `Input.Repo` there). `internal/helm` only has a client-wide
`Proxy`/`NoProxy`, so a call with a repository proxy uses a `helm.Client` with the settings of
`Config.Helm`, that proxy and its own work directory under `<WorkDir>/helm-proxy/<hash>` (chart
and index caches per proxy). Up to 16 such clients are kept; past that, each call gets one in a
temporary directory removed afterwards. Without a repository proxy `Config.Helm` is used, which
passes the repo-server's own proxy variables through.

### Kustomize (SRC-048..068)

Binary: `kustomize.path.<version>` for `kustomize.version` (literal match; `error getting
kustomize binary path: kustomize version <v> is not registered`), else `Config.KustomizePath`
(shown as `kustomize`). When any override is set (namePrefix, nameSuffix, images, replicas,
commonLabels, commonAnnotations, namespace, patches, components) or the build options contain
`--enable-helm`, the repository (minus `.git`) is copied into a private scratch directory
(bounded, symlinks re-checked) and every edit and the build run there; the shared checkout is
never modified. Edits run in upstream order with upstream argv (`edit set nameprefix -- p`, …,
`edit add label [--force] [--without-selector] [--include-templates] k:v…` sorted by key, one
comma-joined argument for kustomize < 3.8.5, `edit add annotation [--force]`, `edit set namespace
-- ns`, patches appended to the kustomization's `patches` list (re-marshalled; comment line in
`Commands`), `edit add component` (≥ 3.7.0; missing/escaping components dropped with
`ignoreMissingComponents`)). Build: `kustomize build <app> <buildOptions…>` with cwd = root, plus
`--helm-kube-version`/`--helm-api-versions` when `--enable-helm` and kustomize ≥ 5.3.0. The build
environment is `sandbox.BaseEnv` + per-call `HOME`/`XDG_*` + build env + proxy variables +
`GIT_SSL_CAINFO` (HTTPS repository with a configured CA, via `Config.CAData`) + git credential
env (`Input.GitCreds`). `PATH` is prefixed with two wrappers: `git` (inserts `--end-of-options`
into `git fetch --depth=1 <repo> <ref>`, refuses other fetch shapes; GHSA-9v9p-x54c-58gc) and
`helm` (pins `HELM_DATA_HOME`/`HELM_CONFIG_HOME`/`HELM_PLUGINS` to an empty per-call directory;
GHSA-fw5c-w8rc-j7fx). Errors name the shared checkout, never the scratch copy. Proxy variables
(upstream `os.Environ()` + `proxy.UpsertEnv`): with a repository proxy, `HTTP_PROXY`,
`HTTPS_PROXY`, `NO_PROXY` and their lower-case forms are set from `Repo.Proxy`/`Repo.NoProxy`
(`no_proxy` always, possibly empty, so a pod-level `NO_PROXY` never applies); without one, the
repo-server's own `HTTP(S)_PROXY`/`NO_PROXY` variables (either case) are passed through. No other
repo-server variable reaches kustomize.

### Config Management Plugins (client side, SRC-071..080, 084..087, 090)

Selection: with `plugin.name`, socket `<name>.sock` only (must stay in the socket dir); used
without probing when it has no discovery, else only if `match` says supported (or discovery is
disabled). Without a name: auto-discovery as above. Messages are upstream's (`could not find
cmp-server plugin with name "<n>" supporting the given repository[: <cause>]`, `could not find
plugin supporting the given repository[: <last probe error>]`, `failed to list all plugins in
dir: ...`, `error dialing to cmp-server for plugin <f>: ...`), wrapped as `CMP processing failed
for application "<app>": ...`. The config call is retried on dial errors. Generate env: build env
+ `KUBE_VERSION` (normalized cluster version) + `KUBE_API_VERSIONS` (comma-joined) +
`ARGOCD_ENV_<name>` (substituted) + `ARGOCD_APP_PARAMETERS` (JSON) + `PARAM_<NAME>[_<KEY>|_<i>]`
(upper-cased, non `[A-Z0-9_]` → `_`). Implicit detection probes send only the build env. The
source tree (repository root, or with `PluginUseManifestGeneratePaths` the deepest common
ancestor of the app path and the `manifest-generate-paths` entries) is archived with
`sandbox.TarCountContext` (deterministic, `.git` and `PluginTarExclusions` excluded, out-of-bounds
symlinks rejected, `no files to send(<root>)` when empty) into a private temp file with its
SHA-256, bounded by `MaxCMPTarBytes`; `OnSourceArchived` fires once the archive exists (upstream
`tarDoneCh`). Symlinks are confined to the archive root, so with `PluginUseManifestGeneratePaths`
a repository symlink that leaves the common root (for example `apps/a/shared -> ../shared` with
the annotation `.`) fails the call with `error compressing repo files: out of bounds symlink
found: <link>`. Upstream archives such a link verbatim, and its cmp-server's untar then rejects
it (`error checking symlink ... path escapes from parent`), so the generation fails either way;
Patrol fails before contacting the sidecar. Responses are bounded by `MaxOutputBytes`; each returned document string is split
with `SplitYAML` (`failed to convert CMP manifests to unstructured objects: ...`).

### Output post-processing and limits (SRC-011, SRC-121)

Lists are flattened (`resource list item has unexpected type`), `items: null` lists dropped,
every object must have `apiVersion` and `kind` and map metadata (`ErrInvalidManifest`), count ≤
`MaxManifests`, summed JSON size ≤ `MaxOutputBytes` (`ErrOutputTooLarge`). `MaxManifests` is also
enforced while output is decoded: one budget per call counts every document of tool output
(Helm, Kustomize, each CMP document string) and of every Directory file and Jsonnet output (List
items individually, null Lists as zero; Jsonnet arrays are decoded element by element), and
decoding stops with `ErrTooManyManifests` as soon as it is exceeded, so a stream of tiny
documents never materializes more than `MaxManifests` objects (`SplitYAML` itself is
unlimited). With
`Input.Tracking`, tracking metadata is written on every object except CRDs (upstream does this in
the repo-server, with the destination namespace as default namespace).

### Metrics and logging

With `Config.Registerer`: `patrol_render_duration_seconds{source_type, outcome}` (histogram),
`patrol_render_jsonnet_evaluations_total{outcome}`,
`patrol_render_cmp_requests_total{operation=config|match|generate|parameters, outcome}`.
Logs (`component=render`) are structured (debug level per render: app, redacted repository
URL, path, chart, source type, duration, manifest count or error). Credentials and plugin output
are never logged; tool errors are logged as returned (they may quote chart template lines, as
upstream's do).

## Differences from upstream

- Override files: the `.patrol-source*.yaml` names are added (precedence above).
- `PATROL_APP_*` duplicates of the build environment.
- Built-in detection inspects only the app directory (upstream walks the subtree but only uses
  the root entry); unreadable subdirectories therefore do not fail detection.
- Kustomize overrides run in a scratch copy instead of mutating the checkout (no need for
  exclusive repository locks for kustomize edits); the version used for argument formatting is
  detected per binary, not once per process.
- Dependency repository credentials: a `oci://` dependency matched by a non-OCI credential
  template stays OCI (upstream copies the template's `enableOCI`, losing it).
- Unreachable dependency repositories: Patrol writes `repositories.yaml` instead of running
  `helm repo add`, so the trigger for the PermissionDenied rewrite also includes Helm's
  `no cached repository for ...` message.
- A JSON file that is an object without `kind` is skipped when it does not look like a manifest
  (upstream emits an empty object after its OCI-manifest probe).
- Directory candidate files are read with a bound (`MaxOutputBytes`; Jsonnet: 64 MiB) for the
  skip-marker check; upstream reads them whole.
- `provideGitCreds` (SRC-084) is different by design: plugins never receive repository
  credentials (ADR-0007); a warning is logged.
- CMP transport is the `cmpwire` HTTP protocol instead of gRPC streams (ADR-0006).
- Plugin output is not logged at debug level (it may contain Secrets).
- `MaxManifests` (no upstream equivalent) bounds the object count while decoding.
- The kustomize build environment carries only the minimal base environment, the build env and
  the proxy variables, not the whole repo-server environment.
- An out-of-root symlink under `PluginUseManifestGeneratePaths` fails before the archive is sent
  (upstream fails in the sidecar's extraction).

## Requests to other components

- `cmd/patrolcd` (and every binary using `jsonnet.Runner` with the default path) must call
  `sandbox.MaybeHelperMain()` then `jsonnet.MaybeJsonnetMain()` first in `main`.
- `internal/cmp` (cmp-server) must implement `internal/render/cmpwire` (socket naming,
  endpoints, framing, `PayloadReader` verification, problem errors, honoring `Patrol-Cmp-Deadline`).
- The repo-server must sanitize checkout paths in returned errors (`<path to cached source>`,
  SRC-118): tool errors (helm/kustomize stderr) can name files under `RootDir`.
- `internal/helm`: per-call `Proxy`/`NoProxy` (for example in `TemplateOpts`,
  `DependencyOpts` and the pull/resolve options) would replace the per-proxy client pool and let
  proxied repositories share the chart and index caches.
- The repo-server must apply `Repository.Proxy` to its own Helm calls outside rendering
  (`ResolveRevision`, chart listing, `show chart`), which use its `helm.Client` directly.
- The repo-server owns `$ref` checkouts (pass their roots in `RefSources`), the dependency
  marker cleanup on revision change, and the OOB symlink scan of the checkout (SRC-117).

## Tests

`go test -race ./internal/render/...` (real `helm` from `PATROL_TEST_HELM`,
`/opt/homebrew/bin/helm` or `PATH`; `bin/kustomize` or `PATROL_TEST_KUSTOMIZE`; `git`; tests that
need a missing binary are skipped). Fixtures live in `internal/render/testdata/{directory,
jsonnet,helm,kustomize}`; chart repositories are served by `httptest` and an in-process OCI
registry (`internal/oci/ocitest`); plugins by an in-test cmpwire sidecar on a unix socket. No
network access is needed.
