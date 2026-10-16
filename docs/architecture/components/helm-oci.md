# Component: `internal/helm` and `internal/oci`

Status: implemented (phase 1). Related decisions: [ADR-0007](../adr/0007-rendering-isolation.md)
(rendering isolation), [ADR-0004](../adr/0004-no-redis.md) (in-memory, discardable caches).

## Responsibilities

`internal/oci` (OCI registries, oras-go v2):

1. Parse repository URLs (`oci://host[:port]/path` or `host[:port]/path`; one `oci://`
   prefix stripped exactly like upstream `strings.TrimPrefix`).
2. Resolve a revision (tag, digest, or semver constraint over tags) to a manifest digest.
3. List tags (`_` converted back to `+`), with pagination and a tag cap.
4. Fetch size-bounded, digest-verified manifests and map annotations to revision metadata.
5. Pull an artifact's single content layer and extract it safely (upstream layer rules,
   media-type allow-list, size limits, Helm chart flattening).
6. Fetch the chart `.tgz` of a Helm OCI chart for `internal/helm`.
7. Provide the TLS/proxy transport shared with the Helm HTTP client.

`internal/helm` (Helm 4 CLI through `internal/sandbox.Run`, plus native HTTP/OCI):

1. Resolve chart versions (exact pins, semver constraints) against an HTTP repository
   index (TTL + ETag cache, size cap) or OCI registry tags.
2. Download charts (index URL or OCI chart layer) into a credential-scoped archive cache
   and extract them into a fresh directory per call.
3. Run `helm template` with the baseline argument order and value precedence.
4. Run `helm dependency build` with repository/registry credentials in per-call 0600 files.
5. Application-details helpers: `helm show chart`, `helm show values`, raw `values.yaml`,
   flattened parameters.
6. Value-file path confinement and the `helm.valuesFileSchemes` allow-list.

Neither package imports `api/v1alpha1` (see "Adapting CRD types").

## Public API

### `internal/oci`

| Symbol | Purpose |
|--------|---------|
| `NewClient(Options) (*Client, error)` | `Options{Username, Password, AccessToken, CAData, CertData, KeyData, InsecureSkipVerify, Proxy, NoProxy, PlainHTTP, UserAgent, MaxManifestBytes, MaxTags, CacheDir, HTTPClient}`. Credentials are bound (`auth.StaticCredential`) to the registry host of each call's repository. `AccessToken` is used directly for Bearer challenges. |
| `(*Client).ResolveReference(ctx, repoURL, ref) (digest, error)` | Literal resolve first; only if that fails and `ref` is a constraint (not an exact version): list tags, `MaxVersion`, resolve that tag (`+`→`_`). |
| `(*Client).ListTags(ctx, repoURL) ([]string, error)` | All pages, `_`→`+`, capped by `MaxTags` (default 100 000). |
| `(*Client).FetchManifest(ctx, repoURL, ref) (ocispec.Manifest, ocispec.Descriptor, error)` | OCI or Docker v2 manifest only; ≤ `MaxManifestBytes` (default 4 MiB); digest-verified. |
| `MetadataFromManifest(m) Metadata` | `createdAt, authors, docsUrl, sourceUrl, version, description` from the standard annotations (title/revision/url not returned). |
| `(*Client).PullArtifact(ctx, repoURL, digest, dest, Limits) (Artifact, error)` | `Limits{MaxLayers (10), MaxLayerBytes (1 GiB), AllowedMediaTypes (default list), Archive sandbox.ArchiveLimits}`. `dest` must be absent or an empty directory. |
| `(*Client).FetchHelmChart(ctx, repoURL, version, w, maxBytes) (digest, error)` | Streams the single Helm chart content layer (current or legacy media type), verified. |
| `(*Client).Ping`, `(*Client).CleanCache(repoURL)` | Connectivity check (`/v2/`); drop cached layers of a repository (hard refresh). |
| `MaxVersion(rev, tags)`, `IsVersion`, `IsConstraint` | Baseline semver resolution (shared with Helm). |
| `ParseRepository`, `TrimScheme`, `IsOCIURL`, `TagFromVersion`, `VersionFromTag`, `IsDigest` | Reference helpers. |
| `NewTransport(TransportOptions)` | TLS (CA replaces system roots, client cert only when cert+key), proxy (`Proxy`/`NoProxy` or environment), keep-alives off. |
| `MediaTypeHelmConfig`, `MediaTypeHelmChartContent`, `MediaTypeImageLayer[Gzip]`, `DefaultAllowedMediaTypes` | Media types. |
| Errors | `ErrInvalidReference, ErrNotFound, ErrUnauthorized, ErrNoMatchingVersion, ErrInvalidConstraint, ErrManifestTooLarge, ErrUnsupportedManifest, ErrTooManyLayers, ErrMediaTypeNotAllowed, ErrContentLayerCount, ErrLayerTooLarge, ErrInvalidHelmChart, ErrDestinationExists, ErrTooManyTags, ErrInvalidTLSConfig`. Sentinels are attached without changing upstream message text. |
| `ocitest.New(t, Options)` | Test-only in-process registry (basic or bearer auth, TLS, pagination, request counters, push helpers). |

### `internal/helm`

| Symbol | Purpose |
|--------|---------|
| `Client{HelmPath, WorkDir, UserAgent, Proxy, NoProxy, PluginsDir, IndexTTL, MaxIndexBytes, MaxChartBytes, MaxValuesBytes, Archive, Exec, IndexCacheEntries}` | `WorkDir` (required) is the cache root: `charts/` (archives), `work/` (per-call helm homes, extracted charts). Defaults: TTL 3 m, index 100 MiB, chart 512 MiB, values 16 MiB, extraction per `sandbox` defaults (1 GiB). Safe for concurrent use; do not copy after first use. |
| `Creds{Username, Password, CAData, CertData, KeyData, InsecureSkipVerify, PassCredentials, PlainHTTP}` | One repository's credentials/transport. `PlainHTTP` = upstream `insecureOCIForceHttp`. |
| `(*Client).ResolveChartVersion(ctx, repoURL, chart, constraint, creds, enableOCI) (string, error)` | Exact version → returned verbatim (no network). Otherwise index entries (HTTP) or tags of `<repoURL>/<chart>` (OCI when `enableOCI` or `IsHelmOCIRepo`). Errors `invalid revision: ...`, `chart '<c>' not found in index`, `unable to get tags: ...`. |
| `(*Client).GetIndex(ctx, repoURL, creds, noCache)`, `ListCharts`, `GetTags`, `InvalidateIndex` | Index/tag access (GetHelmCharts, hard refresh). |
| `(*Client).PullChart(ctx, repoURL, chart, version, creds, enableOCI) (chartDir, error)` | Returns a fresh directory (the chart's top-level dir) owned by the caller (`os.RemoveAll` when done). `CleanChartCache` drops the cached archive. |
| `(*Client).Template` / `TemplateWithCommand(ctx, chartDir, TemplateOpts)` | `TemplateOpts{ReleaseName, Namespace, KubeVersion, APIVersions, ValueFiles, Values, SetParams, SetStringParams, SetFileParams []KV, SkipCRDs, IncludeCRDs, SkipSchemaValidation, SkipTests, PassCredentials, Version}`. Returns manifests and the display command line. |
| `(*Client).DependencyBuild(ctx, chartDir, []RepoCreds)`, `DependencyBuildWithOptions(..., DependencyOpts{InsecureSkipVerify})`, `EnsureDependencies` | `RepoCreds{Name, URL, EnableOCI, Creds}`. `EnsureDependencies` = per-directory lock + `.patrol-helm-dep-up` marker. |
| `IsMissingDependencyErr`, `DependencyRepos(chartDir)`, `ChartDependencies(chartDir)` | Dependency-build trigger and Chart.yaml dependency parsing (`@name`, `alias:name`, `https://`, `oci://`). |
| `(*Client).ShowChart` → `ChartMetadata` (`.Details()` → `ChartDetails`), `ShowValues`, `ReadChartValues`, `GetParameters` | App details. |
| `ResolveValueFilePathOrURL(appPath, repoRoot, file, schemes)`, `ResolveFileOrDirectoryPath`, `ValuesFileSchemes(raw, present)`, `IsURLSchemeAllowed`, `DefaultValuesFileSchemes` | Value-file safety. |
| `CheckVersion`, `NormalizeKubeVersion`, `IsHelmOCIRepo`, `RedactCommand`, `ExtraValuesPlaceholder`, `DependencyMarkerFile` | Helpers/constants. |
| Errors | `ErrUnsupportedVersion, ErrChartNotFound, ErrVersionNotFound, ErrIndexFetch, ErrIndexTooLarge, ErrDownload, ErrChartTooLarge, ErrInvalidChart, ErrPathOutsideRoot, ErrURLSchemeNotAllowed, ErrInvalidValueFile, ErrInvalidRepoName, ErrInvalidArgument, ErrValuesTooLarge`, plus `ErrNoMatchingVersion`/`ErrInvalidConstraint` (aliases of the OCI ones). Subprocess failures unwrap to `*sandbox.ExitError`, `sandbox.ErrTimeout`, `sandbox.ErrOutputLimit`. |

## Behavior details

- **Template arguments** (upstream order): `template . --name-template <r> [--namespace]
  [--kube-version] --set… --set-string… --set-file… --values <files…> [--values <inline>]
  --api-versions… [--include-crds] [--skip-schema-validation] [--skip-tests]`, cwd =
  chart dir. `--include-crds` unless `SkipCRDs` (and always with `IncludeCRDs`).
  Duplicate parameter names keep the last value at the first position (upstream used maps,
  so last wins). Unescaped commas become `\,` unless the value is `{...}`.
  `KubeVersion` is normalized with `k8s.io/apimachinery` generic parsing (`v1.30.11+IKS` →
  `1.30.11`). `Version` accepts `""`, `v3`, `v4` only.
- **Command line** (`TemplateWithCommand`): `helm …` with whitespace arguments quoted and
  the inline values file shown as `<temp file with values from source.helm.values/valuesObject>`.
  Repository checkout paths are replaced by the caller (upstream `redactPaths`).
- **Errors**: `failed to execute helm template command: failed running helm: \`<cmd>\` failed
  exit status N: <stderr>`; runs of `--api-versions X ` become `<api versions removed> `.
  The per-call helm home path is replaced by `<helm-home>` in stderr.
- **Chart.lock**: removed after `helm template` when it did not exist before.
- **Index cache**: in-memory LRU (128 entries) keyed by URL + credential fingerprint;
  served for `IndexTTL`; then revalidated with `If-None-Match`/`If-Modified-Since`
  (304 reuses the parsed index); `noCache` bypasses the read but refreshes. Concurrent
  fetches are collapsed (singleflight, detached from a single caller's cancellation).
  OCI tag lists share the cache (`tags|…` keys).
- **Chart download**: HTTP charts use the index entry's first URL (relative URLs resolved
  like Helm's `ResolveReferenceURL`, base query kept); basic auth is sent only to the
  repository's scheme+host unless `PassCredentials`. The index entry is chosen as
  `helm pull --version` does (Helm `IndexFile.Get`): an equal version string wins (cached
  index first; without one the index is refetched once); otherwise the highest entry of
  the fresh index that satisfies the requested version read as a semver constraint is
  used, so the pins `ResolveChartVersion` returns verbatim (`v1.2.3`, `1.2`) find `1.2.3`
  (logged at info as "using closest available version"). OCI charts: tag = version with
  `+`→`_` (no constraint fallback, as in helm), single chart content layer,
  digest-verified. Archives are cached under
  `WorkDir/charts/<sha256(url, chart, requested version, oci, credential fingerprint)>.tgz`,
  downloaded once per key (singleflight + key lock), opened under the key lock, and
  extracted with `sandbox.ExtractTarGzContext` into `WorkDir/work/extract-*`. The chart
  directory (last segment of the chart name) is then checked with
  `sandbox.CheckSymlinksContext` as a root of its own (upstream runs
  `checkOutOfBoundsSymlinks` on the chart path) and only then renamed to
  `…/extract-*.chart` and returned.
- **Per-call helm home**: `HOME`, `XDG_{CACHE,CONFIG,DATA}_HOME`, `HELM_{CACHE,CONFIG,DATA}_HOME`,
  `HELM_REGISTRY_CONFIG`, `HELM_REPOSITORY_CONFIG`, `HELM_REPOSITORY_CACHE`,
  `HELM_CONTENT_CACHE`, `HELM_PLUGINS` (empty dir unless `PluginsDir`), `KUBECONFIG`
  (nonexistent file), all under `WorkDir/work/home-*`, removed after the call. The rest of
  the environment is `sandbox.BaseEnv` plus proxy variables (`Client.Proxy` or the
  process's `HTTP(S)_PROXY`/`NO_PROXY`).
- **Dependency build**: HTTP repositories go to `repositories.yaml` (name, url, username,
  password, caFile/certFile/keyFile written as 0600 files, `insecure_skip_tls_verify`,
  `pass_credentials_all`); names default to the URL with `/`→`-`, unsafe characters become
  `-`, duplicate names get `-2`, `-3`…; OCI repositories with both username and password
  go to the registry config (oras `FileStore`, 0600). Flags: `--insecure-skip-tls-verify`
  (option), `--plain-http` if any OCI repository has `PlainHTTP`.
- **Value files**: URL with a scheme → allowed only if the scheme is in the list
  (case-insensitive), returned verbatim; `/x` is repo-root relative, other paths
  app-relative; `..` applied lexically, then `sandbox.SecureJoin` resolves every component
  inside the root. Result is symlink-free and strictly inside the root.
- **Parameters**: `helm show values .` (skipped if `values.yaml` escapes the root) merged
  with value files in order (missing/unreadable local files skipped; `http(s)` fetched
  without credentials, capped; other schemes skipped); flattened `a.b`, `l[0].c`, scalars
  `%v` (`null` → `<nil>`).
- **OCI pull**: ≤ `MaxLayers` layers; Helm artifacts (config
  `application/vnd.cncf.helm.config.v1+json`) count only chart content layers, others count
  layers ending in `tar+gzip`/`tar.gzip`/`tar`; counted layers must be allowed; exactly
  one; ≤ `MaxLayerBytes`; downloaded completely and digest-verified (to `CacheDir` or a temp
  file) before extraction into a staging directory next to `dest`, renamed on success. For
  Helm artifacts the single chart directory becomes `dest`; it is first checked with
  `sandbox.CheckSymlinksContext` as a root of its own. Registry operations are bounded by
  `DefaultTimeout`; extraction honors the caller's context.

## Invariants

- Credentials never appear in helm argv, the helm environment, logs or returned errors;
  they exist only in memory and in 0600 files inside a per-call directory removed after the
  call (tested with a fake helm that captures argv/env/config files).
- Two concurrent helm calls never share a helm home (tested).
- An index, tag list or chart archive cached for one credential set is never served to a
  caller with different credentials (cache keys include a credential fingerprint; tested).
- The OCI layer cache is used only after the manifest referencing the layer was fetched
  (authorized) again from the same repository (tested with an anonymous client).
- Nothing extracted from a chart or OCI layer is written outside its staging directory;
  absolute names, `..`, escaping symlinks, writes through symlinked parents, devices and
  over-limit archives are rejected and the staging directory is removed (tested).
- Every symlink in a returned tree resolves inside that tree: the directory `PullChart`
  returns and the `dest` of `PullArtifact`. Extraction checks links against the staging
  root, which is wider than the returned tree when only the chart directory is kept, so
  that directory is re-checked as its own root before it is moved; links such as
  `up -> ..`, `a/b -> ../..` or `x -> ../sibling/file` (inside the archive, outside the
  chart) fail the pull, so a chart cannot reach `WorkDir/work` (other calls' helm homes and
  charts) or `dest`'s siblings (tested; links inside the chart, including `d/self -> ..`,
  are kept). Callers do not need their own out-of-bounds check for these trees, but one
  is harmless.
- No byte of an OCI layer is extracted before its digest has been verified (tested with
  a registry serving tampered content).
- `ResolveValueFilePathOrURL` never returns a local path outside `repoRoot` or equal to it
  (table tests + rapid property over trees with inside/outside/looping symlinks, checked
  against `filepath.EvalSymlinks`).
- `MaxVersion` returns a tag satisfying the constraint with no greater satisfying tag, and
  the selected version does not depend on tag order (rapid property).
- Index and chart downloads are bounded (`MaxIndexBytes`, `MaxChartBytes`), as are
  manifests, tag lists, value documents, Chart.yaml (4 MiB) and helm output
  (`sandbox.Limits`).

## Failure modes

| Situation | Result |
|-----------|--------|
| Index non-200 | `error loading repo index: failed to get index: <status>` (`ErrIndexFetch`) |
| Index over cap / bad YAML | `ErrIndexTooLarge` / `error decoding index: …` |
| Chart not in index | `chart '<c>' not found in index` (`ErrChartNotFound`) |
| Version not in index (after refetch, no exact or constraint match) | `error fetching chart: chart "<c>" version "<v>" not found in <repo> repository` (`ErrVersionNotFound`) |
| Chart symlink leaves the chart directory | `error untarring chart: chart contains out-of-bounds symlinks. file: <path>` (`sandbox.ErrPathEscape`, `*sandbox.OutOfBoundsSymlinkError`) |
| OCI Helm chart symlink leaves the chart directory | `cannot extract contents of oci image with revision <d>: oci image contains out-of-bounds symlinks. file: <path>` (`sandbox.ErrPathEscape`, `*sandbox.OutOfBoundsSymlinkError`) |
| Constraint without match / invalid | `invalid revision: version matching constraint not found in N tags` / `invalid revision: failed to determine semver constraint: …` |
| OCI tag listing fails | `unable to get tags: failed to get tags: …` (+ `ErrUnauthorized`/`ErrNotFound`) |
| Chart archive over cap | `ErrChartTooLarge` |
| Archive unsafe / over extraction limit / corrupt | `error untarring chart: …` wrapping `sandbox.ErrUnsafeArchiveEntry`, `ErrAbsolutePath`, `ErrPathEscape`, `ErrArchiveLimit`, `ErrInvalidArchive` |
| Archive without the chart directory | `ErrInvalidChart` |
| helm exits non-zero | `failed running helm: \`…\` failed exit status N: <stderr>` (unwraps to `*sandbox.ExitError`) |
| helm timeout / output cap | `sandbox.ErrTimeout` / `sandbox.ErrOutputLimit` |
| Missing dependencies in template | detect with `IsMissingDependencyErr`, then `EnsureDependencies` |
| Dependency build fails | `failed to build helm dependencies: …` (`EnsureDependencies` prefixes `error building helm chart dependencies: `; marker not written) |
| Value file escapes root | `file '<orig>' resolved to outside repository root` (`ErrPathOutsideRoot`) |
| Remote scheme not allowed | `the URL scheme '<s>' is not allowed` (`ErrURLSchemeNotAllowed`) |
| Symlink loop / resolves to root | `internal error: failed to resolve path. Check logs for more details` (`ErrInvalidValueFile`) |
| `helm.version` other than ""/v3/v4 | `helm version '<v>' is not supported` (`ErrUnsupportedVersion`) |
| Bad kube version | `could not parse kubernetes version <v>: …` |
| OCI layer rules | `expected no more than 10 oci layers, got N`, `oci layer media type <t> is not in the list of allowed media types`, `expected only a single oci content layer, got N`, `expected 1 directory, found N` |
| OCI extraction failure | `cannot extract contents of oci image with revision <d>: could not decompress layer: …` |
| OCI digest mismatch | `could not save oci digest <d>: oci: verify layer …: mismatched digest` |
| Unknown OCI tag | `cannot get digest for revision <r>: …` (`ErrNotFound`) |

## Security notes

- Repository content, index files, registry responses, chart archives and helm output are
  untrusted: every read is size-bounded, archives go through `internal/sandbox` extraction,
  manifests and layers are digest-verified, and `Chart.yaml` is opened via `SecureJoin`.
- Helm plugins never load unless an operator sets `Client.PluginsDir`; the host's
  `~/.config/helm`, `~/.docker/config.json` and kubeconfig are never read (isolated `HOME`).
- Cross-host credential leaks: chart downloads send basic auth only to the repository
  origin unless `PassCredentials`; Go's HTTP client drops `Authorization` on cross-domain
  redirects; oras drops it on cross-origin redirects and refuses bearer realms on
  private/loopback IPs of another host.
- URLs in logs and errors are redacted (`url.Redacted`). Logged fields: repository URL,
  chart, version, durations, byte counts.
- Remote value files are fetched (by helm for template, by `GetParameters` for app
  details) only for schemes in `helm.valuesFileSchemes`; this is an SSRF surface the
  operator controls through that setting and repo-server egress policy (ADR-0007).
- `TLS MinVersion` 1.2. `InsecureSkipVerify` only per repository opt-in.

## Parity feature areas served (`docs/parity/areas/02-sources-rendering.md`)

| Feature | How | Notes |
|---------|-----|-------|
| SRC-019 template invocation/env | `TemplateWithCommand`, per-call home, Chart.lock cleanup | output splitting is the caller's |
| SRC-020/021 release name, namespace | `ReleaseName`, `Namespace` (flag omitted when empty) | name derivation from app is the caller's |
| SRC-022 value files (local) | `ResolveValueFilePathOrURL` | absolute symlinks inside root rejected (stricter) |
| SRC-023 globs, SRC-024 ignoreMissing, SRC-095 `$ref` | building blocks only (resolver; caller expands globs, stats files, maps `$ref` roots) | |
| SRC-025 remote value files / schemes | `ValuesFileSchemes`, `IsURLSchemeAllowed`, resolver | |
| SRC-026/027 values / valuesObject | `TemplateOpts.Values` (0600 temp file, last `--values`, placeholder in command) | JSON→YAML conversion is the caller's |
| SRC-028/029 parameters, fileParameters | `SetParams`/`SetStringParams`/`SetFileParams`, comma escaping, dedup | env substitution is the caller's |
| SRC-030 precedence | argument order | tested |
| SRC-031 passCredentials | `Creds.PassCredentials` (pull, dependency repos) | |
| SRC-032/033/034 skipCrds, skipSchemaValidation, skipTests | flags | tested with real helm |
| SRC-035 version | `CheckVersion` | |
| SRC-036/037 kubeVersion, apiVersions | `NormalizeKubeVersion`, `--api-versions` | cluster-vs-source choice is the caller's |
| SRC-039 dependency build on demand | `IsMissingDependencyErr`, `EnsureDependencies` (lock + marker) | marker removal on checkout clean is the caller's |
| SRC-040 dependency repos & creds | `DependencyRepos`, `RepoCreds`, repositories.yaml / registry config | credential matching (URL → name → template prefix → OCI host) is the caller's |
| SRC-041 not-permitted repos | caller inspects the error text (`is not a valid chart repository or cannot be reached`, `could not download`) | |
| SRC-042 HTTP chart repo | `PullChart` (native download) | |
| SRC-043 OCI chart (Helm repo) | `PullChart`/`ResolveChartVersion` with `enableOCI` or schemeless URL, `Creds.PlainHTTP` | native oras, no `helm registry login` |
| SRC-044 version resolution | `ResolveChartVersion`, `oci.MaxVersion` | |
| SRC-045 index fetch/cache/limits | `GetIndex` (TTL, ETag, credential-scoped, 100 MiB default) | |
| SRC-046 chart cache & extraction limit | archive cache, `CleanChartCache`, `Client.Archive` | |
| SRC-047 errors and commands | `TemplateWithCommand`, error rewriting, `RedactCommand` | |
| SRC-097 OCI source | `PullArtifact` | tool detection on the tree is the caller's |
| SRC-098 OCI revision resolution | `ResolveReference` | tag cache is the caller's |
| SRC-099 layer rules | `PullArtifact` + `Limits` | |
| SRC-100 OCI extraction limit | `Limits.Archive` (negative = disabled) | |
| SRC-101 OCI Helm charts | Helm flattening in `PullArtifact` | |
| SRC-102 OCI metadata | `FetchManifest` + `MetadataFromManifest` | caching is the caller's |
| SRC-103 OCI tags | `ListTags` | RBAC/caching are the caller's |
| SRC-122 GetAppDetails Helm | `ReadChartValues`, `GetParameters` | value-file listing is the caller's |
| SRC-129 chart details | `ShowChart` → `Details()` | |
| SRC-130 GetHelmCharts | `ListCharts` | |
| SRC-131 ResolveRevision (Helm/OCI) | `ResolveChartVersion`, `ResolveReference` | |

## Adapting CRD types (callers)

| CRD / setting | Mapping |
|---------------|---------|
| Repository `username`/`password`, `tlsClientCertData`/`Key`, `insecure`, CA from TLS-certs ConfigMap | `helm.Creds` / `oci.Options` (`CAData` = PEM for the host) |
| Repository `enableOCI`, `type: oci`, `insecureOCIForceHttp` | `enableOCI` argument / `RepoCreds.EnableOCI`; `Creds.PlainHTTP` / `Options.PlainHTTP` |
| Repository `proxy`/`noProxy` | `Client.Proxy`/`NoProxy`, `Options.Proxy`/`NoProxy` |
| Azure workload identity | caller obtains the token and passes it as `Password` (ACR) or `AccessToken` |
| `spec.source.helm.*` | `TemplateOpts`; `passCredentials` → `Creds.PassCredentials` for pulls and dependency repos |
| `spec.source.targetRevision` | `constraint` / `ref` |
| `reposerver.helm.manifest.max.extracted.size`, `disable.*` | `Client.Archive.MaxTotalBytes` (and `MaxFileBytes`), `-1` |
| `reposerver.oci.manifest.max.extracted.size`, `disable.*` | `oci.Limits.Archive` |
| `reposerver.oci.layer.media.types` | `oci.Limits.AllowedMediaTypes` |
| `--helm-registry-max-index-size` | `Client.MaxIndexBytes` |
| `--revision-cache-expiration` | `Client.IndexTTL` |
| `reposerver.helm.user.agent` | `Client.UserAgent` / `Options.UserAgent` |
| `helm.valuesFileSchemes` (argocd-cm compatible key) | `ValuesFileSchemes(raw, present)` |
| `ARGOCD_EXEC_TIMEOUT` | `Client.Exec.Timeout` |

## Verification

Tests: `internal/oci/*_test.go`, `internal/helm/*_test.go`; hermetic (httptest chart
repositories with/without TLS and basic auth, `ocitest` registry with basic/bearer/TLS,
temp dirs, the local helm 4 binary; tests needing helm skip when it is absent and honor
`PATROL_TEST_HELM`). Covered: semver tables and properties; tag pagination/conversion/cap;
basic, bearer-exchange, access-token and TLS auth; manifest bounds and index rejection;
metadata mapping; every layer rule; tampered layer; malicious archives (traversal,
absolute, symlink escape, symlinks that stay in the archive but leave the chart directory
(`up -> ..`, `a/b -> ../..`, `x -> ../other/secret`) for HTTP and OCI charts, symlinked
parent, device, size limits, corrupt gzip); internal chart symlinks kept; HTTP pulls of
`v1.2.3`/`1.2`/`1` pins via the constraint fallback (table + rapid property for the
selection: satisfies, maximal, order-independent); layer and
chart caches incl. credential scoping and hard refresh; index TTL/ETag/noCache/
concurrency/size/auth/TLS; chart pulls over HTTP (relative URLs, pass-credentials origin
rule, concurrency) and OCI; template defaults, flags, precedence, comma escaping,
set-file, kube/api versions, schema validation, CRDs, tests, version check, error rewriting,
command line, isolation of concurrent homes, Chart.lock cleanup, timeout, rlimit helper
path, remote values; dependency build from HTTPS (URL, `@name`, `alias:`) and OCI
registries, missing credentials, marker semantics, credential files (argv/env free,
0600, removed); value-file resolution table and confinement property; parameters,
chart details, raw values. Exact commands and results are reported by the implementing
task.

## Known gaps and deliberate deviations

- Chart downloads are native (index URL / OCI layer), not `helm pull`: Helm getter plugins
  (`s3://`, `gs://` chart repositories) are not supported for chart sources; chart
  provenance (`--verify`) is not supported (upstream does not use it either).
- `helm registry login` is replaced by a pre-written registry config; OCI dependency
  registries get no per-registry CA/client certificate (upstream passed them to `login`
  only, so the build did not use them either).
- HTTP index `digest` fields are not verified (Helm does not verify them on pull).
- The chart archive cache is keyed by the requested version string, as upstream's is: a
  partial pin such as `1.2` keeps serving the 1.2.x chart it first downloaded until
  `CleanChartCache` (hard refresh), even after a newer 1.2.x is published. A request
  string that is not a semver constraint only matches an index entry with the same
  version string.
- Index over the size cap is an error (upstream silently truncated and failed decoding).
- `IsHelmOCIRepo` keeps the upstream quirk: `host:port/path` URLs whose host is an IP do
  not parse and are not auto-detected as OCI; set `enableOCI`.
- `ResolveReference` resolves the `_` form of a semver tag chosen by a constraint
  (upstream resolved the `+` form, which fails for build-metadata tags).
- `MaxVersion` ignores tags that fail to parse as semver for any reason (upstream aborted
  on non-`ErrInvalidSemVer` parse errors) and has no `tagPrefix` (git-only feature).
- Absolute symlinks are rejected in value-file resolution even when they point inside the
  repository (inherited from `sandbox.SecureJoin`).
- Parameters with duplicate names are passed once (last value, first position) and in
  declaration order; upstream passed them in random map order.
- The default user agent is `patrol-cd` (upstream `argocd-repo-server/<version> (<platform>)`);
  callers set `UserAgent`.
- Caches are in-memory/per-process (ADR-0004); OCI tag lists and metadata are not cached
  by `internal/oci` itself (repo-server concern).
- The dependency marker is `.patrol-helm-dep-up` (upstream `.argocd-helm-dep-up`).
