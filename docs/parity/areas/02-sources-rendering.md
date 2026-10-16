# Sources & manifest rendering — parity inventory (Argo CD v3.5.5)

Scope: everything the **repo-server** (and its config-management-plugin sidecars) does to turn an Application source into a list of Kubernetes manifests, plus the repo-server RPCs that expose source introspection. In scope: tool detection; Directory (plain YAML/JSON, recurse/include/exclude, skip marker, size limit); Jsonnet (extVars, TLAs, libs, import confinement); Helm (every `ApplicationSourceHelm` field, value-file resolution incl. globs/remote/`$ref`, dependency build, HTTP and OCI chart repositories, chart version resolution, index/chart caches); Kustomize (every `ApplicationSourceKustomize` field, versions, build options, remote-base and helm-plugin guards); Config Management Plugins v2 (sidecar `plugin.yaml`, discovery, parameters, env, streaming protocol, timeouts); build environment; `.argocd-source*.yaml` override files; multi-source `$ref`; OCI artifact sources; revision resolution (git refs, semver constraints, `tagPrefix`, Helm/OCI tags); repo-server caching and error caching; repo locking, parallelism, exec timeouts, size limits; out-of-bounds symlink protection; git submodules/checkout; local-manifest streaming (`GenerateManifestWithFiles`); and the RPCs `GenerateManifest`, `GetAppDetails`, `ListRefs`, `ListOCITags`, `ListApps`, `ListPlugins`, `GetRevisionMetadata`, `GetOCIMetadata`, `GetRevisionChartDetails`, `GetHelmCharts`, `ResolveRevision`, `GetGitFiles`, `GetGitDirectories`, `UpdateRevisionForPaths`, and the `argocd.argoproj.io/manifest-generate-paths` annotation semantics.
Out of scope (covered by other areas): repository/credential CRUD and `TestRepository` (repositories area), git transport/auth/retry/GPG and source-integrity verification internals (source integrity area), resource tracking label/annotation details (tracking area; only the injection point is listed here), Helm hook → Argo hook mapping and sync behaviour (sync area), diffing, ApplicationSet generators (they *consume* `GetGitFiles`/`GetGitDirectories`), the source hydrator, webhooks (they consume the refresh-path matching listed here), Application spec validation conditions other than source/type checks.

Upstream permalink base: `https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/`. Bundled tool versions at this commit: Helm `4.2.1`, Kustomize `5.8.1` ([hack/tool-versions.sh#L14](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/hack/tool-versions.sh#L14)). Maturity: none of the features in this area appear in `docs/operator-manual/feature-maturity.md` ([#L18](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/feature-maturity.md#L18)), so all default to GA unless marked Deprecated by docs/code.

---

## A. Tool detection and source typing

### SRC-001 Explicit source type selection
- **Upstream**: [pkg/apis/application/v1alpha1/types.go#L3799](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L3799), [reposerver/repository/repository.go#L1922](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L1922), [util/argo/argo.go#L467](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L467), [docs/user-guide/tool_detection.md#L5](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/tool_detection.md#L5)
- **Maturity**: GA
- **Semantics**: The presence (non-nil) of exactly one of `spec.source.kustomize`, `.helm`, `.directory`, `.plugin` fixes the source type to Kustomize / Helm / Directory / Plugin respectively, regardless of repository contents. An empty block (`plugin: {}`, `directory: {}`) counts as "present". Type is evaluated *after* `.argocd-source*.yaml` overrides are merged (SRC-092, SRC-093), so an override file can introduce or conflict with an explicit type. If the explicit type is a built-in tool disabled via `argocd-cm` (SRC-003) the source is rendered as `Directory` instead (Plugin can never be disabled). Response field `ManifestResponse.sourceType` carries the effective type.
- **Edge cases**: more than one block set → error `multiple application sources defined: Kustomize,Helm` (order: Kustomize, Helm, Directory, Plugin), surfaced as `error getting app source type: ...` from manifest generation and as `error verifying source type: ...` during spec validation; explicit `helm` on a path with no `Chart.yaml` → `helm template` failure; explicit `directory` on a Helm chart dir → templates parsed as YAML (see SRC-006).
- **Configuration surface**: CRD `spec.source{,s[]}.{helm,kustomize,directory,plugin}`.
- **Exposure**: CRD; API (Application create/update; `ManifestResponse.sourceType`, `RepoAppDetailsResponse.type`); CLI (`argocd app create|set` tool flags imply a type, e.g. `--directory-recurse`, `--helm-set`, `--nameprefix`, `--config-management-plugin`); UI (create panel tool dropdown Directory/Helm/Kustomize/Plugin — [application-create-panel.tsx#L558](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-create-panel/application-create-panel.tsx#L558)).
- **Tests required**:
  - positive: each block alone forces its tool even when repo contents suggest another (e.g. `directory: {}` on a dir containing `Chart.yaml`).
  - negative: two blocks → exact error text `multiple application sources defined: Kustomize,Helm`.
  - positive: explicit Helm with `helm.enable: "false"` → rendered as Directory, `sourceType=Directory`.
  - positive: explicit Plugin is never downgraded even if all built-ins disabled.

### SRC-002 Implicit tool detection
- **Upstream**: [util/app/discovery/discovery.go#L37](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/app/discovery/discovery.go#L37), [util/app/discovery/discovery.go#L73](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/app/discovery/discovery.go#L73), [util/kustomize/kustomize.go#L77](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/kustomize/kustomize.go#L77), [docs/user-guide/tool_detection.md#L27](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/tool_detection.md#L27)
- **Maturity**: GA
- **Semantics**: When no explicit type: (1) CMP discovery first — every `*.sock` in the plugin socket dir is probed in directory-listing (lexical) order; the first plugin that has discovery configured and matches wins → `Plugin` (SRC-071..075). (2) Otherwise the app path is walked; a file whose basename *ends with* `Chart.yaml` marks its directory Helm; a file named exactly `kustomization.yaml`, `kustomization.yml` or `Kustomization` marks it Kustomize (Kustomize overwrites Helm for the same directory because it is visited later). Only the entry for the app path itself (`"."`) decides the type. (3) Otherwise `Directory`. Built-in types disabled in `argocd-cm` are not detected. Detection env passed to CMP discovery contains only the standard `ARGOCD_APP_*` build env (no `ARGOCD_ENV_*`, no params).
- **Edge cases**: `MyChart.yaml` (suffix match) also triggers Helm; both `Chart.yaml` and `kustomization.yaml` in app root → Kustomize; detection walks the *whole* subtree even though only the root counts (walk errors, e.g. unreadable dirs, fail detection with `error getting app source type: ...`); missing/unreadable plugin socket directory is silently ignored for implicit detection; a plugin that errors during probing is treated as non-matching.
- **Configuration surface**: plugin socket dir `ARGOCD_PLUGINSOCKFILEPATH` (default `/home/argocd/cmp-server/plugins`); `argocd-cm` `kustomize.enable`/`helm.enable`.
- **Exposure**: implicit in GenerateManifest/GetAppDetails/ListApps; UI shows detected type (app details `Type`).
- **Tests required**:
  - positive: dir with `Chart.yaml` → Helm; with `kustomization.yml` → Kustomize; with `Kustomization` → Kustomize; plain YAML → Directory.
  - positive: both Chart.yaml and kustomization.yaml → Kustomize.
  - positive: CMP with matching discovery beats Chart.yaml.
  - negative: `Chart.yaml` only in a subdirectory does not make root Helm.
  - negative: `KUSTOMIZATION.yaml` (wrong case) not detected.

### SRC-003 Disabling built-in config management tools
- **Upstream**: [util/settings/settings.go#L591](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L591), [util/settings/settings.go#L977](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L977), [util/app/discovery/discovery.go#L26](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/app/discovery/discovery.go#L26), [reposerver/repository/repository.go#L1983](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L1983), [docs/user-guide/tool_detection.md#L34](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/tool_detection.md#L34), [docs/operator-manual/argocd-cm.yaml#L254](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cm.yaml#L254)
- **Maturity**: GA
- **Semantics**: `argocd-cm` keys `kustomize.enable`, `helm.enable`, `jsonnet.enable` (default `true`; only a non-empty value other than `"true"` disables). The controller sends `enabledSourceTypes` map `{Kustomize,Helm,Directory(=jsonnet.enable),Plugin:true}` in each request. Disabled Helm/Kustomize: not auto-detected, and an explicit type falls back to Directory. `jsonnet.enable=false` maps to the `Directory` key and causes `*.jsonnet` files to be skipped during directory rendering (YAML/JSON still processed). Missing map / missing key = enabled.
- **Edge cases**: value `"false"`, `"False"`, `"no"` all disable (anything ≠ `"true"`); empty string keeps default enabled; Plugin cannot be disabled; repo-server API calls that do not send the map (e.g. API server `ListApps`/`GetAppDetails`) treat everything as enabled.
- **Configuration surface**: `argocd-cm`: `kustomize.enable`, `helm.enable`, `jsonnet.enable`.
- **Exposure**: ConfigMap only.
- **Tests required**:
  - positive: `helm.enable: "false"` + chart dir → rendered as Directory (chart YAML templates not executed).
  - positive: `jsonnet.enable: "false"` → `.jsonnet` ignored, `.yaml` still rendered.
  - negative: `helm.enable: ""` keeps Helm enabled.

### SRC-004 Application path resolution
- **Upstream**: [util/app/path/path.go#L17](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/app/path/path.go#L17), [reposerver/repository/repository.go#L541](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L541), [util/argo/argo.go#L588](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L588)
- **Maturity**: GA
- **Semantics**: `spec.source.path` is joined to the checkout root (git, extracted OCI image, or streamed dir). Empty path = root. For Helm chart-repo sources the app path is the extracted chart directory and `path` is ignored.
- **Edge cases**: absolute path → `<path>: app path is absolute`; escaping root → `<path>: app path outside root`; missing → `<path>: app path does not exist` (constant `ErrMessageAppPathDoesNotExist`, used by hydrator messaging); a file → `<path>: app path is not a directory`. Spec validation: single-source requires `repoURL` and (`path` or `chart`) → `spec.source.repoURL and either spec.source.path or spec.source.chart are required`; multi-source requires `path`, `chart` or `ref`; chart sources require `targetRevision` (`spec.source.targetRevision is required if the manifest source is a helm chart`).
- **Configuration surface**: CRD `spec.source.path`.
- **Exposure**: CRD; CLI `--path`; UI source panel PATH ([application-parameters.tsx#L683](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-parameters/application-parameters.tsx#L683)).
- **Tests required**:
  - negative: `/abs`, `../x`, nonexistent, file path each produce exact error.
  - positive: `path: .` and empty path render repo root.
  - negative: chart source without targetRevision rejected at validation.

---

## B. Directory sources

### SRC-005 Plain YAML/JSON manifest loading
- **Upstream**: [reposerver/repository/repository.go#L1966](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L1966), [reposerver/repository/repository.go#L1969](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L1969), [reposerver/repository/repository.go#L2019](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L2019), [reposerver/repository/repository.go#L2084](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L2084), [docs/user-guide/directory.md#L3](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/directory.md#L3)
- **Maturity**: GA
- **Semantics**: Candidate files match regex `^.*\.(yaml|yml|json|jsonnet)$` (case-sensitive). Files are read as UTF-8 with BOM/UTF-16 auto-conversion. `.yaml/.yml`: multi-document YAML-or-JSON stream decoded document by document into unstructured objects (empty docs skipped). `.json`: exactly one JSON object per file. Files are processed in `filepath.Walk` (lexical) order; the output preserves that order.
- **Edge cases**: `.json` with more than one top-level value → `Found multiple objects in "<file>". Only single objects are allowed in JSON files.` (gRPC FailedPrecondition); invalid JSON → `Failed to unmarshal "<file>": ...` (unless it parses as an OCI image manifest — see Unconfirmed); `.YAML` uppercase ignored; unreadable file → `Failed to open "<path>"`; UTF-16 encoded YAML must render (upstream test `TestGenerateFromUTF16`).
- **Configuration surface**: none (implicit for Directory type).
- **Exposure**: CRD (`spec.source.directory` optional); API/CLI/UI indirectly.
- **Tests required**:
  - positive: mixed `.yaml`, `.yml`, `.json` files; multi-doc YAML with `---` and empty docs.
  - positive: UTF-16 LE/BE files decode.
  - negative: JSON file with two objects → exact error.
  - negative: `.txt` / `.YAML` ignored.

### SRC-006 Non-manifest and malformed YAML handling
- **Upstream**: [reposerver/repository/repository.go#L2055](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L2055), [docs/user-guide/directory.md#L46](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/directory.md#L46)
- **Maturity**: GA
- **Semantics**: If YAML decoding fails: (a) if at least one object was already decoded from the file → error `Failed to unmarshal "<file>": failed to unmarshal manifest: ...`; (b) else, if the raw file contains all of `apiVersion:`, `kind:` and `metadata:` → same error; (c) otherwise the file is silently ignored (treated as unrelated YAML such as a values file).
- **Edge cases**: a file whose first doc is valid and second malformed → error; a Helm `values.yaml` with no `kind:` → ignored; a broken manifest that lacks `metadata:` → silently ignored (upstream behavior, must be preserved).
- **Configuration surface**: none.
- **Exposure**: implicit.
- **Tests required**:
  - positive: unrelated YAML (`foo: bar`) ignored without error.
  - negative: malformed YAML containing apiVersion/kind/metadata → FailedPrecondition error naming the file.
  - negative: valid doc followed by malformed doc → error.

### SRC-007 Directory `recurse`
- **Upstream**: [pkg/apis/application/v1alpha1/types.go#L933](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L933), [reposerver/repository/repository.go#L2196](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L2196), [docs/user-guide/directory.md#L25](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/directory.md#L25)
- **Maturity**: GA
- **Semantics**: Default `false`: only files directly in the app path. `true`: walk all subdirectories (symlinked directories are not followed by the walker). Hidden directories are included when recursing.
- **Edge cases**: recursion into nested chart/kustomize dirs parses their YAML as plain manifests (templates usually ignored or error per SRC-006); `.git` is not special-cased but contains no matching files normally.
- **Configuration surface**: `spec.source.directory.recurse`.
- **Exposure**: CRD; CLI `argocd app create|set --directory-recurse` ([cmd/util/app.go#L147](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/util/app.go#L147)); UI Parameters tab "DIRECTORY RECURSE" ([application-parameters.tsx#L1052](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-parameters/application-parameters.tsx#L1052)).
- **Tests required**:
  - positive: recurse=false returns 2 root files, recurse=true returns 4 including nested (mirrors upstream `Test_findManifests`).
  - positive: symlinked subdirectory is not traversed.

### SRC-008 Directory `include` glob
- **Upstream**: [pkg/apis/application/v1alpha1/types.go#L939](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L939), [reposerver/repository/repository.go#L2163](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L2163), [util/glob/glob.go#L90](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/glob/glob.go#L90), [docs/user-guide/directory.md#L51](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/directory.md#L51)
- **Maturity**: GA
- **Semantics**: Single gobwas-glob pattern (supports `{a,b}` alternation, `*`, `?`, `[...]`) compiled **without separators** (so `*` also matches `/`) and matched against the file path relative to the app path. When set, only matching files are loaded. Evaluated after the extension filter and after `exclude`.
- **Edge cases**: invalid pattern → warning logged and match returns false → nothing included (no error); `*.yaml` matches `sub/x.yaml` when recursing; patterns apply to `.jsonnet` files too.
- **Configuration surface**: `spec.source.directory.include`.
- **Exposure**: CRD; CLI `--directory-include` ([cmd/util/app.go#L169](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/util/app.go#L169)); UI "INCLUDE" ([application-parameters.tsx#L1088](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-parameters/application-parameters.tsx#L1088)).
- **Tests required**:
  - positive: `{*.yml,*.yaml}` includes both; `some-directory/*` with recurse.
  - negative: malformed pattern `[` includes nothing and does not error.

### SRC-009 Directory `exclude` glob
- **Upstream**: [pkg/apis/application/v1alpha1/types.go#L937](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L937), [reposerver/repository/repository.go#L2159](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L2159), [docs/user-guide/directory.md#L89](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/directory.md#L89)
- **Maturity**: GA
- **Semantics**: Same glob semantics as include; matching files are skipped. Exclude is evaluated before include, so a file matching both is excluded. Directories are never pruned by exclude (only files are tested).
- **Edge cases**: invalid exclude pattern → nothing excluded; `{config.json,env-usw2/*}` combined with include `*.yaml`.
- **Configuration surface**: `spec.source.directory.exclude`.
- **Exposure**: CRD; CLI `--directory-exclude` ([cmd/util/app.go#L168](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/util/app.go#L168)); UI "EXCLUDE" ([application-parameters.tsx#L1096](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-parameters/application-parameters.tsx#L1096)).
- **Tests required**:
  - positive: include+exclude combination from docs example yields only `deployment.yaml` and `env-use2/configmap.yaml`.
  - positive: exclude matching nothing leaves output unchanged (upstream `TestFindManifests_Exclude_NothingMatches`).

### SRC-010 Skip-file-rendering marker
- **Upstream**: [reposerver/repository/repository.go#L82](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L82), [reposerver/repository/repository.go#L2173](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L2173), [docs/user-guide/directory.md#L134](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/directory.md#L134)
- **Maturity**: GA
- **Semantics**: Any candidate file (yaml/yml/json/jsonnet) whose raw bytes contain the literal substring `+argocd:skip-file-rendering` anywhere is skipped entirely (not parsed, not counted toward the size limit).
- **Edge cases**: marker inside a string value or in a non-comment location still skips; applies only to Directory type (not Helm/Kustomize/CMP).
- **Configuration surface**: file content marker.
- **Exposure**: repo content only.
- **Tests required**:
  - positive: file with `# +argocd:skip-file-rendering` and a valid ConfigMap produces no object.
  - positive: marker in a `.jsonnet` file skips evaluation.

### SRC-011 List kinds and null lists
- **Upstream**: [reposerver/repository/repository.go#L1780](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L1780), [reposerver/repository/repository.go#L1952](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L1952)
- **Maturity**: GA
- **Semantics**: For every tool, after rendering, objects that are Kubernetes Lists (`kind: *List` with `items`) are flattened into their items; a "null list" (`items: null`, no `spec`/`status`) yields nothing; `nil` objects are dropped. Each resulting object is JSON-serialized into `ManifestResponse.manifests`.
- **Edge cases**: list item that is not an object → `resource list item has unexpected type`; `ConfigMapList` with `items: null` must not error (upstream `TestGenerateNullList`).
- **Configuration surface**: none.
- **Exposure**: implicit in all manifest RPCs.
- **Tests required**:
  - positive: `v1/List` with 2 items yields 2 manifests.
  - positive: null list yields 0 manifests without error.

### SRC-012 Symlinked manifest files in directory apps
- **Upstream**: [reposerver/repository/repository.go#L2127](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L2127), [util/io/files/util.go#L103](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/io/files/util.go#L103)
- **Maturity**: GA
- **Semantics**: A candidate file that is a symlink is resolved (`EvalSymlinks`); it must resolve inside the repo root; the target's size and regularity are used. Applies independently of the global OOB-symlink scan (SRC-117).
- **Edge cases**: dangling symlink → skipped with warning `destination of symlink "<rel>" is missing`; symlink resolving outside root → error `invalid manifest file "<path>": illegal filepath in symlink at "<rel>"`; symlink to non-regular file (dir, device, fifo) → skipped with warning `ignoring symlink at ... to non-regular file ...`; circular symlink → error (upstream test "circular link should throw an error").
- **Configuration surface**: none.
- **Exposure**: implicit.
- **Tests required**:
  - positive: in-repo symlink to a manifest renders it.
  - negative: out-of-root symlink → illegal filepath error.
  - positive: dangling symlink skipped without error.

### SRC-013 Max combined directory manifests size
- **Upstream**: [reposerver/repository/repository.go#L85](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L85), [reposerver/repository/repository.go#L2213](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L2213), [cmd/argocd-repo-server/commands/argocd_repo_server.go#L266](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-repo-server/commands/argocd_repo_server.go#L266), [docs/operator-manual/argocd-cmd-params-cm.yaml#L271](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cmd-params-cm.yaml#L271)
- **Maturity**: GA
- **Semantics**: Before parsing, the sum of sizes of candidate (non-skipped, included, non-`.jsonnet`) files is computed; if it exceeds the limit the whole generation fails. Default `10M` (Kubernetes quantity). `0` disables the limit (used by CLI local generation).
- **Edge cases**: error text `failed to get potentially valid manifests: exceeded max combined manifest file size`; jsonnet files never count; skipped-marker and excluded files never count; symlink targets counted by real size.
- **Configuration surface**: flag `--max-combined-directory-manifests-size`, env `ARGOCD_REPO_SERVER_MAX_COMBINED_DIRECTORY_MANIFESTS_SIZE`, `argocd-cmd-params-cm` `reposerver.max.combined.directory.manifests.size`.
- **Exposure**: server config only.
- **Tests required**:
  - negative: two 6M files with 10M limit → exact error.
  - positive: limit 0 → unlimited; excluded large file does not count.

---

## C. Jsonnet (inside Directory sources)

### SRC-014 Jsonnet file evaluation
- **Upstream**: [reposerver/repository/repository.go#L1982](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L1982), [docs/user-guide/jsonnet.md#L3](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/jsonnet.md#L3)
- **Maturity**: GA
- **Semantics**: Every `*.jsonnet` candidate file in a Directory source is evaluated with go-jsonnet (a fresh VM per file). Output must be a JSON array of objects or a single object; arrays are appended in order. Skipped entirely when `jsonnet.enable` is false. `.libsonnet` files are not evaluated directly (extension filter) but can be imported.
- **Edge cases**: evaluation error → `Failed to evaluate jsonnet "<file>": <err>` (FailedPrecondition); output neither array nor object (e.g. a string) → `Failed to unmarshal generated json "<file>": ...`; jsonnet output may contain List kinds (flattened by SRC-011).
- **Configuration surface**: `spec.source.directory.jsonnet` (optional).
- **Exposure**: CRD; CLI jsonnet flags; UI Directory parameters.
- **Tests required**:
  - positive: array output and object output (upstream `TestGenerateJsonnetManifestInDir`, `...InRootDir`).
  - negative: syntax error → exact prefix.
  - negative: scalar output → unmarshal error.

### SRC-015 Jsonnet external variables (`extVars`)
- **Upstream**: [pkg/apis/application/v1alpha1/types.go#L918](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L918), [reposerver/repository/repository.go#L2238](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L2238), [docs/user-guide/jsonnet.md#L5](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/jsonnet.md#L5)
- **Maturity**: GA
- **Semantics**: List of `{name, value, code}`. Values are build-env substituted (SRC-091) first. `code: false` → `std.extVar` string; `code: true` → value parsed as jsonnet code (`ExtCode`).
- **Edge cases**: invalid code value → jsonnet evaluation error for any file referencing it; referencing an undefined extVar → evaluation error; `$$` escapes `$`.
- **Configuration surface**: `spec.source.directory.jsonnet.extVars[].{name,value,code}`.
- **Exposure**: CRD; CLI `--jsonnet-ext-var-str`, `--jsonnet-ext-var-code` (format `name=value`; value-less entries keep name only) ([cmd/util/app.go#L151](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/util/app.go#L151)); UI "EXTERNAL VARIABLES" ([application-parameters.tsx#L1072](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-parameters/application-parameters.tsx#L1072)).
- **Tests required**:
  - positive: `app=${ARGOCD_APP_NAME}` string extVar renders app name.
  - positive: code extVar `{a:1}` usable as object.
  - negative: missing extVar referenced → error.

### SRC-016 Jsonnet top-level arguments (`tlas`)
- **Upstream**: [pkg/apis/application/v1alpha1/types.go#L920](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L920), [reposerver/repository/repository.go#L2235](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L2235)
- **Maturity**: GA
- **Semantics**: Same shape and env substitution as extVars; passed as TLA string (`TLAVar`) or code (`TLACode`). Applied to every `.jsonnet` file in the source.
- **Edge cases**: a file that is not a function ignores TLAs (go-jsonnet semantics); a file that requires a TLA not provided errors.
- **Configuration surface**: `spec.source.directory.jsonnet.tlas[]`.
- **Exposure**: CRD; CLI `--jsonnet-tla-str`, `--jsonnet-tla-code` ([cmd/util/app.go#L149](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/util/app.go#L149)); UI "TOP-LEVEL ARGUMENTS" ([application-parameters.tsx#L1057](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-parameters/application-parameters.tsx#L1057)).
- **Tests required**:
  - positive: function-style jsonnet receives `ns=${ARGOCD_APP_NAMESPACE}`.
  - positive: code TLA (`replicas=3`) typed as number.

### SRC-017 Jsonnet library paths (`libs`)
- **Upstream**: [pkg/apis/application/v1alpha1/types.go#L922](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L922), [reposerver/repository/repository.go#L2256](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L2256), [util/io/path/resolved.go#L76](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/io/path/resolved.go#L76)
- **Maturity**: GA
- **Semantics**: JPaths = `[appPath] + libs`, where each lib is resolved **relative to the repository root** (leading `/` also = repo root), symlinks resolved, and must stay inside the repo (may equal the root). Import lookup order (mirrors go-jsonnet): importing file's directory, then JPaths in reverse order (last lib first, app path last).
- **Edge cases**: lib outside repo → error `file '<lib>' resolved to outside repository root` (upstream `TestGenerateJsonnetLibOutside`); nonexistent lib dir is accepted (only boundary checked).
- **Configuration surface**: `spec.source.directory.jsonnet.libs[]`.
- **Exposure**: CRD; CLI `--jsonnet-libs` ([cmd/util/app.go#L153](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/util/app.go#L153)).
- **Tests required**:
  - positive: `libs: [vendor]` resolves `import 'k.libsonnet'` from `<repo>/vendor`.
  - negative: `libs: [../../etc]` rejected.

### SRC-018 Jsonnet import confinement
- **Upstream**: [reposerver/repository/repository.go#L2286](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L2286), [reposerver/repository/repository.go#L2333](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L2333)
- **Maturity**: GA
- **Semantics**: Every `import`/`importstr`/`importbin` is located with stat-only lookups first, then validated: resolved (symlink-evaluated) path must be inside the repo root and must be a regular file; only then is it read. Prevents reading `/proc/self/environ`, `/dev/zero`, traversal outside repo.
- **Edge cases**: absolute import outside repo → `jsonnet import "<path>" resolves outside repository root`; device file → rejected before read (`... is not a regular file` or outside-root); missing import → `couldn't open import "<p>": no match locally or in the Jsonnet library paths`.
- **Configuration surface**: none.
- **Exposure**: implicit.
- **Tests required**:
  - negative: `import '/etc/passwd'`, `import '../../../x'`, `importstr '/dev/zero'` all fail quickly without reading (upstream `TestGenerateJsonnetImport*OutsideRepoRoot`).
  - positive: import via in-repo symlink to in-repo file succeeds.

---

## D. Helm

### SRC-019 Helm template invocation and process environment
- **Upstream**: [reposerver/repository/repository.go#L1260](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L1260), [util/helm/cmd.go#L429](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/cmd.go#L429), [util/helm/cmd.go#L70](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/cmd.go#L70), [util/helm/cmd.go#L487](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/cmd.go#L487), [docs/user-guide/helm.md#L3](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/helm.md#L3)
- **Maturity**: GA
- **Semantics**: Helm sources (git path containing a chart, chart-repo chart, OCI chart) are rendered with `helm template . --name-template <release> [--namespace <ns>] [--kube-version <v>] [--set ...] [--set-string ...] [--set-file ...] [--values ...]* [--values <inline-values-tmp>] [--api-versions ...]* [--include-crds] [--skip-schema-validation] [--skip-tests]`, run with cwd = chart dir. Unless running locally (CLI), each invocation uses an isolated temp helm home: env `XDG_CACHE_HOME`, `XDG_CONFIG_HOME`, `XDG_DATA_HOME`, `HELM_CONFIG_HOME` under a per-invocation temp dir (deleted afterwards), plus repo proxy/noProxy env. Build env vars are **not** exported to the helm process (only substituted into fields, SRC-038). If `Chart.lock` did not exist before `helm template`, any lock file it creates is deleted afterwards (workaround for helm/helm#6870). Output is split into objects (multi-doc YAML).
- **Edge cases**: helm binary failure → `failed to execute helm template command: failed running helm: \`helm template ...\` failed exit status 1: <stderr>`; empty chart output → zero manifests (valid); `Chart.lock` stat error → `failed to clean up chart lock file: ...`.
- **Configuration surface**: CRD `spec.source.helm.*`; env `ARGOCD_EXEC_TIMEOUT` (SRC-115).
- **Exposure**: CRD; API (GenerateManifest / GetManifests REST `GET /api/v1/applications/{name}/manifests`); CLI `argocd app manifests`; UI app details / diff.
- **Tests required**:
  - positive: default command line exactly `helm template . --name-template <app> --namespace <ns> --include-crds` (upstream `Test_GenerateManifests_Commands`).
  - positive: two concurrent renders do not share helm cache/config dirs.
  - positive: Chart.lock created by template is removed; pre-existing Chart.lock retained.

### SRC-020 Helm `releaseName`
- **Upstream**: [pkg/apis/application/v1alpha1/types.go#L573](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L573), [reposerver/repository/repository.go#L1265](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L1265), [reposerver/repository/repository.go#L1289](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L1289), [docs/user-guide/helm.md#L479](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/helm.md#L479)
- **Maturity**: GA
- **Semantics**: `--name-template` = `helm.releaseName` if non-empty; otherwise the Application **name part** of the instance name (the `<ns>_` prefix used for apps outside the control-plane namespace is stripped); if that is empty, the raw request app name.
- **Edge cases**: app `team-a/foo` in non-default namespace → release `foo` (not `team-a_foo`); release names violating Helm rules (>53 chars, underscores) fail in helm.
- **Configuration surface**: `spec.source.helm.releaseName`.
- **Exposure**: CRD; CLI `--release-name` ([cmd/util/app.go#L126](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/util/app.go#L126)); UI (via YAML/parameters).
- **Tests required**:
  - positive: default = app name; override honored (`different-release-name`).
  - positive: namespaced app instance name stripped to name.

### SRC-021 Helm `namespace`
- **Upstream**: [pkg/apis/application/v1alpha1/types.go#L591](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L591), [pkg/apis/application/v1alpha1/types.go#L373](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L373), [reposerver/repository/repository.go#L1292](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L1292)
- **Maturity**: GA
- **Semantics**: `--namespace` = `helm.namespace` if set, else the Application destination namespace; omitted entirely if both empty. (`GetNamespaceOrDefault` would also consider `kustomize.namespace`, but a Helm source cannot have a kustomize block.)
- **Edge cases**: cluster-scoped-only apps with empty destination namespace → no `--namespace` flag (helm default `default` applies to `.Release.Namespace`).
- **Configuration surface**: `spec.source.helm.namespace`; `spec.destination.namespace`.
- **Exposure**: CRD; CLI `--helm-namespace` ([cmd/util/app.go#L135](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/util/app.go#L135)).
- **Tests required**:
  - positive: override `different-namespace` appears as `--namespace different-namespace`.
  - positive: empty both → flag absent.

### SRC-022 Helm `valueFiles` (local resolution)
- **Upstream**: [pkg/apis/application/v1alpha1/types.go#L569](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L569), [reposerver/repository/repository.go#L1436](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L1436), [util/io/path/resolved.go#L113](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/io/path/resolved.go#L113), [util/io/path/resolved.go#L135](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/io/path/resolved.go#L135), [docs/user-guide/helm.md#L52](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/helm.md#L52)
- **Maturity**: GA
- **Semantics**: Each entry is env-substituted, then: relative paths resolve against the chart directory; paths starting with `/` resolve against the repository root (for chart-repo sources the "repo root" is the extracted chart). Symlinks are resolved recursively (max depth 10) and the final path must be strictly inside the repo root (resolving to the root itself is rejected). Each resolved file becomes `--values <abs path>` in declared order; duplicates (same absolute path) are passed once (first position wins, except explicit-vs-glob rule SRC-023).
- **Edge cases**: traversal `../../x` outside root → `error resolving helm value files: error resolving value file path: file '<orig>' resolved to outside repository root`; symlink chain >10 → `internal error: failed to resolve path. Check logs for more details`; missing file without ignore flag → passed through to helm which fails (`open ...: no such file or directory`); values file from another git repo requires multi-source `$ref` (SRC-095). Error messages must not leak absolute temp paths (SRC-118).
- **Configuration surface**: `spec.source.helm.valueFiles[]`.
- **Exposure**: CRD; CLI `--values` (repeatable), `argocd app unset --values` ([cmd/argocd/commands/app.go#L1045](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/app.go#L1045)); UI "VALUES FILES" ([application-parameters.tsx#L792](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-parameters/application-parameters.tsx#L792)).
- **Tests required**:
  - positive: relative and `/`-absolute entries resolve correctly; order preserved.
  - negative: traversal and out-of-root symlink rejected (upstream `TestGenerateHelmWithValuesDirectoryTraversal*`, `TestHelmManifestFromChartRepoWithValueFileOutsideRepo`).
  - positive: in-repo symlink allowed (`TestHelmManifestFromChartRepoWithValueFileLinks`).

### SRC-023 Helm `valueFiles` glob patterns
- **Upstream**: [reposerver/repository/repository.go#L1446](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L1446), [reposerver/repository/repository.go#L1511](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L1511), [reposerver/repository/repository.go#L1619](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L1619), [reposerver/repository/repository.go#L1645](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L1645), [reposerver/repository/repository.go#L702](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L702), [docs/user-guide/helm.md#L93](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/helm.md#L93)
- **Maturity**: GA
- **Semantics**: An entry whose *resolved* local path contains `*`, `?` or `[` is expanded with doublestar (`**` recursive). Env substitution happens before expansion; `$ref/...` globs are expanded inside the referenced repo. Matches are inserted at the pattern's position in doublestar lexical order (each directory's own files before descending). Files also listed explicitly (non-glob) are skipped by the glob so the explicit entry controls position. A file matched by multiple globs appears once at its first position. Each match's symlink-resolved path must stay within the effective root (ref repo root for `$ref` globs). Remote URLs are never glob-expanded.
- **Edge cases**: no match → `values file glob "<pattern>" matched no files` returned as gRPC `NotFound`; spec validation ignores this specific error (app is saved; controller later shows ComparisonError); with `ignoreMissingValueFiles: true` no-match is silently skipped; malformed pattern → `error expanding glob pattern "<p>": ...`; match escaping root via symlink → `glob pattern "<p>": glob match "<m>" resolved to outside repository root`.
- **Configuration surface**: `spec.source.helm.valueFiles[]` with glob metacharacters.
- **Exposure**: CRD; CLI `--values 'envs/*.yaml'`.
- **Tests required**:
  - positive: `envs/*.yaml` lexical order; `envs/**/*.yaml` per-directory-first ordering.
  - positive: explicit entry after glob moves file to end; duplicate across two globs dedup.
  - negative: zero matches → NotFound with exact message; with ignore flag → success.
  - negative: symlink escape in glob match rejected (`Test_getResolvedValueFiles_glob_symlink_escape`).

### SRC-024 Helm `ignoreMissingValueFiles`
- **Upstream**: [pkg/apis/application/v1alpha1/types.go#L584](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L584), [reposerver/repository/repository.go#L1535](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L1535), [docs/user-guide/helm.md#L77](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/helm.md#L77)
- **Maturity**: GA
- **Semantics**: When `true`, local value files (and `$ref` files) that do not exist after resolution are omitted from the `--values` list, and globs with no match are skipped. Remote URLs are never existence-checked. Default `false`.
- **Edge cases**: path-boundary violations still error even with the flag; also honored by GetAppDetails parameter extraction.
- **Configuration surface**: `spec.source.helm.ignoreMissingValueFiles`.
- **Exposure**: CRD; CLI `--ignore-missing-value-files` / `argocd app unset --ignore-missing-value-files` ([cmd/util/app.go#L124](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/util/app.go#L124)).
- **Tests required**:
  - positive: missing `values-optional-override.yaml` skipped (upstream `TestHelmWithMissingValueFiles`).
  - negative: without flag, missing file → helm error.

### SRC-025 Remote value files and `helm.valuesFileSchemes`
- **Upstream**: [util/io/path/resolved.go#L53](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/io/path/resolved.go#L53), [util/io/path/resolved.go#L116](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/io/path/resolved.go#L116), [util/settings/settings.go#L1245](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L1245), [reposerver/apiclient/repository.go#L3](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/apiclient/repository.go#L3), [docs/operator-manual/argocd-cm.yaml#L271](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cm.yaml#L271), [docs/user-guide/helm.md#L600](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/helm.md#L600)
- **Maturity**: GA
- **Semantics**: A value file (or file parameter) that parses as a URL with a non-empty scheme is treated as remote. It is allowed only if its scheme (case-insensitive) is in the allowed list; then it is passed verbatim to `helm --values <url>` (helm fetches it). Allowed list = `argocd-cm` `helm.valuesFileSchemes` (comma separated, trimmed); if the key is absent the default is `https, http`; if present but empty → no remote files allowed. Callers that send no HelmOptions allow none.
- **Edge cases**: disallowed scheme → `the URL scheme '<scheme>' is not allowed`; remote URL with glob chars passed literally; remote files are not existence-checked by `ignoreMissingValueFiles`; custom schemes (e.g. `s3://`) require a Helm getter plugin in the repo-server image.
- **Configuration surface**: `argocd-cm` `helm.valuesFileSchemes`.
- **Exposure**: CRD (valueFiles); ConfigMap.
- **Tests required**:
  - positive: `https://.../values.yaml` accepted by default (upstream `TestGenerateHelmWithURL`).
  - negative: `ftp://...` rejected; key present but empty → `https` rejected.
  - positive: `helm.valuesFileSchemes: "s3"` allows `s3://` (case-insensitive `S3://`).

### SRC-026 Helm `values` (inline string)
- **Upstream**: [pkg/apis/application/v1alpha1/types.go#L576](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L576), [pkg/apis/application/v1alpha1/values.go#L91](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/values.go#L91), [reposerver/repository/repository.go#L1303](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L1303), [docs/user-guide/helm.md#L351](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/helm.md#L351)
- **Maturity**: GA
- **Semantics**: YAML string written to a randomly named temp file in the OS temp dir (mode 0644) and passed as the **last** `--values` (after all valueFiles). Ignored when `valuesObject` is set. Not env-substituted. Temp file deleted after rendering unless the source has `ref` set (upstream quirk).
- **Edge cases**: invalid YAML → helm error; in returned `commands` the temp path is replaced by `<temp file with values from source.helm.values/valuesObject>`.
- **Configuration surface**: `spec.source.helm.values`.
- **Exposure**: CRD; CLI `--values-literal-file <file|http(s) URL>` (stored as `valuesObject` via `SetValuesString`; must be a YAML map or string, else `invalid type`) and `argocd app unset --values-literal`; UI "VALUES" editor ([application-parameters.tsx#L807](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-parameters/application-parameters.tsx#L807)).
- **Tests required**:
  - positive: inline values override valueFiles key.
  - positive: command output contains placeholder, not temp path.
  - negative: `--values-literal-file` with a YAML list → CLI error `invalid type`.

### SRC-027 Helm `valuesObject`
- **Upstream**: [pkg/apis/application/v1alpha1/types.go#L589](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L589), [pkg/apis/application/v1alpha1/values.go#L17](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/values.go#L17), [pkg/apis/application/v1alpha1/values.go#L103](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/values.go#L103), [docs/user-guide/helm.md#L328](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/helm.md#L328)
- **Maturity**: GA
- **Semantics**: Free-form object (`x-kubernetes-preserve-unknown-fields`). When non-nil it is converted JSON→YAML and used instead of `values` (values silently ignored). Same temp-file mechanism as SRC-026. Considered empty if the rendered YAML is empty.
- **Edge cases**: both set → valuesObject wins; `valuesObject: {}` renders `{}` (non-empty file) — still passed.
- **Configuration surface**: `spec.source.helm.valuesObject`.
- **Exposure**: CRD; CLI (`--values-literal-file` writes here); UI VALUES editor dumps valuesObject as YAML.
- **Tests required**:
  - positive: valuesObject beats values for the same key (precedence test).
  - positive: nested maps/lists preserved through JSON↔YAML conversion.

### SRC-028 Helm `parameters` (`--set` / `--set-string`)
- **Upstream**: [pkg/apis/application/v1alpha1/types.go#L605](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L605), [reposerver/repository/repository.go#L1322](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L1322), [util/helm/cmd.go#L399](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/cmd.go#L399), [docs/user-guide/helm.md#L372](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/helm.md#L372)
- **Maturity**: GA
- **Semantics**: Each `{name, value, forceString}` → `--set name=value`, or `--set-string` when `forceString: true`. Values are env-substituted. Unescaped commas in values are escaped (`a,b` → `a\,b`) unless the whole value is wrapped in `{...}` (helm list syntax); already-escaped `\,` kept. Parameters are collected into maps keyed by name: duplicate names → last wins; argument order among `--set` flags is not stable.
- **Edge cases**: `name=` with empty value allowed; CLI `-p`/`--helm-set` parse `name=value` (error `expected helm parameter of the form param=value but received: <text>`), and `-p` is rejected for non-Helm apps (`Parameters can only be set against Helm applications`).
- **Configuration surface**: `spec.source.helm.parameters[].{name,value,forceString}`.
- **Exposure**: CRD; CLI `-p/--parameter`, `--helm-set`, `--helm-set-string` ([cmd/util/app.go#L129](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/util/app.go#L129)), `argocd app unset -p`; UI "PARAMETERS" (overrides seeded from app details) ([application-parameters.tsx#L835](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-parameters/application-parameters.tsx#L835)).
- **Tests required**:
  - positive: forceString `true` keeps `"false"` a string; plain `--set` makes bool.
  - positive: `a,b` escaped; `{a,b}` list preserved.
  - positive: duplicate names → last value.
  - negative: CLI `-p` on Kustomize app fails.

### SRC-029 Helm `fileParameters` (`--set-file`)
- **Upstream**: [pkg/apis/application/v1alpha1/types.go#L615](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L615), [reposerver/repository/repository.go#L1329](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L1329), [docs/user-guide/helm.md#L460](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/helm.md#L460)
- **Maturity**: GA
- **Semantics**: Each `{name, path}` → `--set-file name=<resolved>`. Path resolution identical to value files (relative to chart, `/` = repo root, boundary-checked, allowed URL schemes, `$ref/...` to a referenced source — SRC-095). Env substitution applied (after ref resolution for `$ref` paths). Commas escaped as for parameters.
- **Edge cases**: out-of-root path → `error resolving helm value file path: ...`; bad `$ref` → `error resolving set-file path: ...`; nonexistent file → helm error (no ignore flag applies).
- **Configuration surface**: `spec.source.helm.fileParameters[].{name,path}`.
- **Exposure**: CRD; CLI `--helm-set-file key=path` ([cmd/util/app.go#L131](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/util/app.go#L131)); UI file-parameter rows in Parameters tab.
- **Tests required**:
  - positive: relative and absolute file param (upstream `TestGenerateHelmWithFileParameter`, `TestGenerateHelmWithAbsoluteFileParameter`).
  - positive: multi-source `$ref` file parameter (`TestGenerateMultiSourceHelmWithFileParameter`).
  - negative: traversal rejected.

### SRC-030 Helm value precedence (argument ordering)
- **Upstream**: [util/helm/cmd.go#L436](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/cmd.go#L436), [docs/user-guide/helm.md#L398](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/helm.md#L398)
- **Maturity**: GA
- **Semantics**: Effective precedence (lowest→highest): chart `values.yaml` < `valueFiles` (in list order, later wins) < `values` *or* `valuesObject` (single extra `--values`, valuesObject replacing values) < `parameters`/`fileParameters` (`--set*` always beat `--values` in helm).
- **Edge cases**: duplicate keys inside one YAML doc → last wins (YAML parser); duplicate parameter names → last wins.
- **Configuration surface**: combination of SRC-022..029.
- **Exposure**: CRD.
- **Tests required**:
  - positive: same key in valueFiles, values, valuesObject, parameters → parameter value rendered; remove parameter → valuesObject; etc. (upstream `TestGenerateHelmWithValues`).

### SRC-031 Helm `passCredentials`
- **Upstream**: [pkg/apis/application/v1alpha1/types.go#L582](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L582), [reposerver/repository/repository.go#L441](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L441), [util/helm/cmd.go#L298](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/cmd.go#L298), [util/helm/cmd.go#L221](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/cmd.go#L221), [docs/user-guide/helm.md#L710](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/helm.md#L710)
- **Maturity**: GA
- **Semantics**: When `true`, `--pass-credentials` is added to `helm pull` for chart-repo sources and to every `helm repo add` performed for dependency builds, allowing helm to send repo credentials to chart URLs on other domains. Default `false`. Not applied to OCI pulls.
- **Edge cases**: has no effect on git-path charts without dependencies.
- **Configuration surface**: `spec.source.helm.passCredentials`.
- **Exposure**: CRD; CLI `--helm-pass-credentials`, `argocd app unset --pass-credentials` ([cmd/argocd/commands/app.go#L1056](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/app.go#L1056)).
- **Tests required**:
  - positive: chart pull command contains `--pass-credentials` iff flag true.
  - positive: dependency `repo add` contains flag.

### SRC-032 Helm `skipCrds`
- **Upstream**: [pkg/apis/application/v1alpha1/types.go#L586](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L586), [util/helm/cmd.go#L462](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/cmd.go#L462), [docs/user-guide/helm.md#L731](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/helm.md#L731)
- **Maturity**: GA
- **Semantics**: By default Argo CD passes `--include-crds` so chart `crds/` content is rendered; `skipCrds: true` omits the flag.
- **Edge cases**: CRDs emitted by templates (not `crds/`) are always rendered.
- **Configuration surface**: `spec.source.helm.skipCrds`.
- **Exposure**: CRD; CLI `--helm-skip-crds` ([cmd/util/app.go#L132](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/util/app.go#L132)).
- **Tests required**:
  - positive: chart with `crds/` folder renders CRD by default, not with skipCrds.

### SRC-033 Helm `skipSchemaValidation`
- **Upstream**: [pkg/apis/application/v1alpha1/types.go#L601](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L601), [util/helm/cmd.go#L465](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/cmd.go#L465), [docs/user-guide/helm.md#L751](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/helm.md#L751)
- **Maturity**: GA
- **Semantics**: Adds `--skip-schema-validation` so `values.schema.json` violations do not fail rendering.
- **Edge cases**: chart with invalid values per schema: fails without flag, succeeds with it (upstream `Test_SkipSchemaValidation`).
- **Configuration surface**: `spec.source.helm.skipSchemaValidation`.
- **Exposure**: CRD; CLI `--helm-skip-schema-validation`.
- **Tests required**:
  - negative/positive pair on a schema-violating chart.

### SRC-034 Helm `skipTests`
- **Upstream**: [pkg/apis/application/v1alpha1/types.go#L599](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L599), [util/helm/cmd.go#L468](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/cmd.go#L468), [docs/user-guide/helm.md#L771](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/helm.md#L771)
- **Maturity**: GA
- **Semantics**: Adds `--skip-tests`, removing `helm.sh/hook: test*` manifests from output.
- **Edge cases**: without flag, test hooks are rendered (then ignored by sync as unsupported hooks — sync area).
- **Configuration surface**: `spec.source.helm.skipTests`.
- **Exposure**: CRD; CLI `--helm-skip-tests`.
- **Tests required**:
  - positive: chart with test pod omits it only when flag set.

### SRC-035 Helm `version` field
- **Upstream**: [pkg/apis/application/v1alpha1/types.go#L580](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L580), [util/helm/cmd.go#L36](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/cmd.go#L36), [docs/user-guide/helm.md#L692](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/helm.md#L692)
- **Maturity**: Deprecated (kept for backward compatibility only)
- **Semantics**: Accepted values `""`, `v3`, `v4` — all use the single bundled Helm (v4) binary. Any other value fails.
- **Edge cases**: `version: v2` or `3` → `failed to create new helm command: helm version 'v2' is not supported` wrapped as `error initializing helm app object: ...`.
- **Configuration surface**: `spec.source.helm.version`.
- **Exposure**: CRD; CLI `--helm-version`.
- **Tests required**:
  - positive: `v3` and `v4` render identically.
  - negative: `v2` rejected with exact message.

### SRC-036 Helm `kubeVersion` (Capabilities.KubeVersion)
- **Upstream**: [pkg/apis/application/v1alpha1/types.go#L594](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L594), [pkg/apis/application/v1alpha1/types.go#L388](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L388), [reposerver/repository/repository.go#L1248](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L1248), [controller/state.go#L279](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L279)
- **Maturity**: GA
- **Semantics**: `--kube-version` = `helm.kubeVersion` if set, else the destination cluster's server version supplied by the controller (only when it sends runtime state). The value is normalized with Kubernetes generic version parsing (leading `v` and suffixes like `+IKS`, `-eks-...` dropped → `MAJOR.MINOR.PATCH`). Omitted if empty.
- **Edge cases**: unparsable value → `could not parse kubernetes version <v>: ...`; `1.30.11+IKS` → `--kube-version 1.30.11` (upstream `TestGenerateHelmKubeVersion`); cache key includes cluster version/API versions so changing cluster version invalidates manifests (SRC-110).
- **Configuration surface**: `spec.source.helm.kubeVersion`.
- **Exposure**: CRD; CLI `--helm-kube-version` ([cmd/util/app.go#L136](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/util/app.go#L136)).
- **Tests required**:
  - positive: override wins over cluster version; normalization of `v1.16.0`, `1.2.3+something`.
  - negative: `abc` → parse error.

### SRC-037 Helm `apiVersions` (Capabilities.APIVersions)
- **Upstream**: [pkg/apis/application/v1alpha1/types.go#L597](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L597), [pkg/apis/application/v1alpha1/types.go#L403](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L403), [util/helm/cmd.go#L459](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/cmd.go#L459)
- **Maturity**: GA
- **Semantics**: One `--api-versions <v>` per entry. Source list (`helm.apiVersions`) replaces (not merges) the cluster-discovered list when non-empty. Cluster list format is `group/version` and `group/version/Kind` strings produced by the controller.
- **Edge cases**: helm error output containing the (huge) `--api-versions` args is rewritten replacing them with `<api versions removed> ` (SRC-047).
- **Configuration surface**: `spec.source.helm.apiVersions[]`.
- **Exposure**: CRD; CLI `--helm-api-versions` (repeatable).
- **Tests required**:
  - positive: override list `v3`,`v4` passed exactly; cluster list ignored.
  - positive: chart using `.Capabilities.APIVersions.Has` renders conditionally.

### SRC-038 Helm build-environment substitution
- **Upstream**: [reposerver/repository/repository.go#L1354](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L1354), [reposerver/repository/repository.go#L1458](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L1458), [docs/user-guide/helm.md#L578](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/helm.md#L578)
- **Maturity**: GA
- **Semantics**: `$VAR` / `${VAR}` references to build env (SRC-091) are substituted in: parameter values (`--set`, `--set-string`), valueFiles entries (before resolution; for `$ref` entries only the part after the ref), fileParameters paths. Not substituted: `values`, `valuesObject`, `releaseName`, `namespace`. Unknown variables expand to empty string; `$$` → `$`.
- **Edge cases**: a single-source app with a value file `$values/x.yaml` (no ref sources) is env-substituted → `$values` → empty → `/x.yaml` (repo-root-relative) rather than erroring.
- **Configuration surface**: field values.
- **Exposure**: CRD.
- **Tests required**:
  - positive: `--set-string app=${ARGOCD_APP_NAME}` renders `test-value-test-app` (upstream `TestGenerateHelmWithEnvVars`).
  - positive: valueFiles `envs/$ARGOCD_APP_NAME/*.yaml`.
  - negative: values block containing `$ARGOCD_APP_NAME` stays literal.

### SRC-039 Helm dependency build on demand
- **Upstream**: [reposerver/repository/repository.go#L1378](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L1378), [reposerver/repository/repository.go#L1219](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L1219), [util/helm/helm.go#L67](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/helm.go#L67), [util/helm/helm.go#L80](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/helm.go#L80), [util/helm/cmd.go#L351](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/cmd.go#L351)
- **Maturity**: GA
- **Semantics**: `helm template` is tried first. Only if it fails with a missing-dependency error (`found in requirements.yaml, but missing in charts` or `found in Chart.yaml, but missing in charts/ directory`) does the repo-server: lock on the chart path, skip if marker file `.argocd-helm-dep-up` exists, otherwise register dependency repos (SRC-040: `helm repo add` for HTTP repos, `helm registry login`/logout for OCI repos with username+password), run `helm dependency build [--insecure-skip-tls-verify if repo insecure] [--plain-http if any dependency repo has insecureOCIForceHttp]`, write the marker, and re-run `helm template`. The marker and vendored `charts/` are removed when the checkout is cleaned (revision change, SRC-116).
- **Edge cases**: other template errors are returned immediately (no dep build); dep build failure → `error building helm chart dependencies: failed to build helm dependencies: ...` (or PermissionDenied, SRC-041); concurrent renders of the same chart path build once; registry login failure → `failed to login to registry <repo>: ...`.
- **Configuration surface**: Chart.yaml/requirements.yaml dependencies; repository credentials.
- **Exposure**: implicit.
- **Tests required**:
  - positive: chart with `dependencies` and no `charts/` renders after automatic build; second render reuses marker (no network).
  - positive: revision change cleans marker → rebuild.
  - negative: unreachable dependency repo error propagated.

### SRC-040 Helm dependency repository & credential resolution
- **Upstream**: [reposerver/repository/repository.go#L1110](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L1110), [reposerver/repository/repository.go#L1176](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L1176), [reposerver/repository/repository.go#L1603](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L1603), [util/helm/cmd.go#L166](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/cmd.go#L166), [controller/state.go#L337](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L337)
- **Maturity**: GA
- **Semantics**: Dependencies are read from the chart's `Chart.yaml` `dependencies[].repository`. Recognized forms: `@<name>` and `alias:<name>` (look up a configured Helm repository by name), `https://...` and `oci://...` URLs (repo name = URL with `/` → `-`; `oci://` stripped and OCI enabled). Other schemes (`http://`, `file://`) are not registered. Matching order for credentials: exact URL among project-permitted Helm repositories (URL compared without `oci://`) → by name → first repository-credential template whose URL is a prefix (non-OCI creds: prefix of URL sans `oci://`; OCI-type creds: prefix of `oci://`+URL, which also enables OCI) → for OCI deps, first permitted repo that is OCI-enabled (or type `oci`) whose URL is prefix-related in either direction. Credential fields copied: username, password, SSH key, TLS client cert/key, Azure workload identity, insecureOCIForceHttp. For OCI-type sources the controller also adds permitted OCI repos/creds to the candidate list.
- **Edge cases**: dependency without matching creds still registered anonymously; named repo (`@stable`) not configured → registered with empty URL (helm fails); OCI repo with only one of username/password → no registry login.
- **Configuration surface**: Repository/RepoCreds secrets (`type: helm|oci`, `enableOCI`, `insecureOCIForceHttp`), project `sourceRepos`.
- **Exposure**: implicit.
- **Tests required**:
  - positive: `@name`, `alias:name`, https URL, oci URL each resolved (upstream `TestGetHelmRepo_NamedRepos*`, `TestGetHelmRepos_OCI*`).
  - positive: creds template prefix match applies password; InsecureOCIForceHttp propagated (`TestGetHelmRepos_InsecureOCIForceHttpPropagated*`).
  - negative: `http://` dependency repo not added (helm error surfaces).

### SRC-041 Helm dependency repositories not permitted by project
- **Upstream**: [reposerver/repository/repository.go#L1384](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L1384), [reposerver/repository/repository.go#L1241](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L1241)
- **Maturity**: GA
- **Semantics**: If dependency build fails with an error containing `is not a valid chart repository or cannot be reached` or `could not download`, every dependency repo not matched by the project's `sourceRepos` globs is collected and the error is replaced by gRPC PermissionDenied `helm repos <r1>, <r2> are not permitted in project '<project>'`.
- **Edge cases**: other dependency errors pass through unchanged; permitted-but-unreachable repos keep the original error.
- **Configuration surface**: AppProject `spec.sourceRepos`.
- **Exposure**: implicit (error surfaces as ComparisonError).
- **Tests required**:
  - negative: project sourceRepos `[git repo only]` + unreachable dependency → PermissionDenied message with project name.
  - positive: sourceRepos `*` → original helm error.

### SRC-042 Helm chart from an HTTP(S) chart repository
- **Upstream**: [reposerver/repository/repository.go#L434](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L434), [util/helm/client.go#L183](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/client.go#L183), [util/helm/cmd.go#L257](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/cmd.go#L257), [pkg/apis/application/v1alpha1/types.go#L214](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L214), [docs/user-guide/helm.md#L9](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/helm.md#L9)
- **Maturity**: GA
- **Semantics**: A source with non-empty `chart` is a Helm chart source (`IsHelm`). The chart is downloaded with `helm pull --destination <tmp> --version <resolved> [--username U --password P] [--insecure-skip-tls-verify] --repo <repoURL> <chart> [--ca-file] [--cert-file] [--key-file] [--pass-credentials]` into a per-(url,chart,version) cache file, extracted to a temp dir, and the app path becomes `<tmp>/<last path segment of chart>`. `path` is ignored. Rendering then proceeds as SRC-019 with repo root = chart dir. Revision for caching/status = resolved chart version. The extracted chart is OOB-symlink-checked.
- **Edge cases**: pull producing ≠1 file → `expected 1 file, found N`; pull failure → `error fetching chart: failed to fetch chart: ...`; chart name with `/` uses last segment; `..`/`.` names kept verbatim (normalization guard).
- **Configuration surface**: CRD `spec.source.{repoURL,chart,targetRevision}`; Helm repository secrets.
- **Exposure**: CRD; CLI `--repo --helm-chart --revision`; UI chart picker (GetHelmCharts, [application-parameters.tsx#L636](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-parameters/application-parameters.tsx#L636)).
- **Tests required**:
  - positive: chart from index-based repo renders (upstream `TestHelmManifestFromChartRepo`).
  - positive: valueFiles relative to extracted chart (`TestHelmManifestFromChartRepoWithValueFile`).
  - negative: unknown chart → index error (SRC-044).

### SRC-043 Helm chart from an OCI registry (Helm-type repo)
- **Upstream**: [reposerver/repository/repository.go#L2958](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L2958), [util/helm/client.go#L447](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/client.go#L447), [util/helm/cmd.go#L309](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/cmd.go#L309), [util/helm/cmd.go#L93](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/cmd.go#L93), [docs/user-guide/helm.md#L28](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/helm.md#L28), [docs/user-guide/oci.md#L62](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/oci.md#L62)
- **Maturity**: GA
- **Semantics**: Chart sources are treated as OCI when the repository has `enableOCI: true` or the `repoURL` has no scheme/host (e.g. `registry-1.docker.io/bitnamicharts`). Flow: optional `helm registry login <host> [--plain-http] --username U --password-stdin [--ca-file] [--cert-file] [--key-file] [--insecure]` (only if both username and password set), `helm pull oci://<repoURL>/<chart> --version <v> --destination <tmp> [...tls] [--plain-http]`, logout afterwards. Plain HTTP when the repository has `insecureOCIForceHttp`.
- **Edge cases**: `oci://` prefix in a Helm-type repoURL is *not* supported (docs); registry host derived from URL (`//` prepended if missing); login failure → `error logging into OCI registry: ...`.
- **Configuration surface**: Repository `type: helm`, `enableOCI`, `insecureOCIForceHttp`, username/password, TLS client cert.
- **Exposure**: CRD; CLI `argocd repo add --type helm --enable-oci` (repositories area).
- **Tests required**:
  - positive: schemeless repoURL treated as OCI without enableOCI.
  - positive: credentials passed via stdin (never on argv); command log redacts `--username/--password` values.
  - positive: insecureOCIForceHttp adds `--plain-http` to login and pull.

### SRC-044 Helm chart version resolution (exact and semver constraints)
- **Upstream**: [reposerver/repository/repository.go#L2966](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L2966), [util/versions/tags.go#L49](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/versions/tags.go#L49), [util/helm/index.go#L27](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/index.go#L27), [util/helm/client.go#L467](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/client.go#L467), [docs/user-guide/tracking_strategies.md#L12](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/tracking_strategies.md#L12)
- **Maturity**: GA
- **Semantics**: If `targetRevision` parses as an exact semver (`1.2.0`, `v1.2`, `1.2`) it is used verbatim without contacting the repo. Otherwise it is treated as a Masterminds semver constraint (`1.2.*`, `>=1.0.0 <2.0.0`, `*`, `^1.2`, `~1.2`, `*-0` to include prereleases) and the highest matching version among available versions is chosen: for HTTP repos the `index.yaml` entries of the chart; for OCI repos the registry tags of `<repo>/<chart>` (tags with `_` converted back to `+`). Non-semver tags are ignored. If the string is neither a version nor a valid constraint, it must equal one of the versions literally.
- **Edge cases**: chart missing from index → `chart '<chart>' not found in index`; no version satisfying constraint → `invalid revision: version matching constraint not found in N tags`; invalid constraint not in tags → `invalid revision: failed to determine semver constraint: ...`; tag listing failure (OCI) → `unable to get tags: ...`; exact version that does not exist fails later at pull time; prereleases only match constraints with `-0`.
- **Configuration surface**: `spec.source.targetRevision`.
- **Exposure**: CRD; API `ResolveRevision`; UI revision field.
- **Tests required**:
  - positive: `1.*` picks highest 1.x; `*-0` includes `2.0.0-rc.1`; exact pin not resolved against index.
  - negative: constraint with no match → exact message.
  - positive: OCI tag `1.0.0_build.1` matched as `1.0.0+build.1`.

### SRC-045 Helm repository index fetch, caching and limits
- **Upstream**: [util/helm/client.go#L280](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/client.go#L280), [util/helm/client.go#L352](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/client.go#L352), [util/helm/client.go#L135](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/client.go#L135), [reposerver/cache/cache.go#L178](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/cache/cache.go#L178), [cmd/argocd-repo-server/commands/argocd_repo_server.go#L272](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-repo-server/commands/argocd_repo_server.go#L272), [common/common.go#L278](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L278)
- **Maturity**: GA
- **Semantics**: `GET <repoURL>/index.yaml` with basic auth (if username or password), repo TLS settings (CA file, client cert/key, insecure), proxy/noProxy, keep-alives disabled, and `User-Agent` = `ARGOCD_HELM_USER_AGENT` or default `argocd-repo-server/<version> (<platform>)`. Response body read through a limit reader of `--helm-registry-max-index-size` (default `1G`). Raw index cached in Redis under `helm-index|<repoURL>` for the revision-cache TTL (`--revision-cache-expiration`, default 3m); OCI tag lists cached under `helm-index|<host/path/chart>`. Per-URL in-process lock serializes fetches. `noRevisionCache`/hard refresh bypasses the cache read.
- **Edge cases**: non-200 → `failed to get index: <status>`; index larger than limit is truncated, producing a YAML decode error (`error decoding index: ...`); empty cached value treated as miss; cache read errors only logged.
- **Configuration surface**: env `ARGOCD_HELM_USER_AGENT` / cmd-params `reposerver.helm.user.agent`; flag `--helm-registry-max-index-size` (env `ARGOCD_REPO_SERVER_HELM_MANIFEST_MAX_INDEX_SIZE`); `--revision-cache-expiration` (env `ARGOCD_RECONCILIATION_TIMEOUT`).
- **Exposure**: internal; affects GetHelmCharts/ResolveRevision.
- **Tests required**:
  - positive: second resolution within TTL issues no HTTP request; hard refresh issues one.
  - positive: custom user agent header sent.
  - negative: 401 index → error contains status.

### SRC-046 Helm chart tarball cache and extraction size limit
- **Upstream**: [util/helm/client.go#L438](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/client.go#L438), [util/helm/client.go#L166](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/client.go#L166), [util/helm/client.go#L155](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/client.go#L155), [cmd/argocd-repo-server/commands/argocd_repo_server.go#L271](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-repo-server/commands/argocd_repo_server.go#L271), [cmd/argocd-repo-server/commands/argocd_repo_server.go#L275](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-repo-server/commands/argocd_repo_server.go#L275)
- **Maturity**: GA
- **Semantics**: Downloaded `.tgz` is kept on local disk at a randomized path keyed by `{url, chart, version}` and reused across requests (per-path lock). Each render extracts into a fresh temp dir (deleted after use). Extraction is size-limited by `--helm-manifest-max-extracted-size` (default `1G`) using the Go untar; if `--disable-helm-manifest-max-extracted-size` is set, `tar -zxvf` is used without limit. `noCache` (hard refresh) deletes the cached tarball before pulling.
- **Edge cases**: extracted content beyond limit → `error untarring chart: ...`; tarball containing out-of-bounds symlink → `chart contains out-of-bounds symlinks. file: <file>` (SRC-117).
- **Configuration surface**: flags/env `ARGOCD_REPO_SERVER_HELM_MANIFEST_MAX_EXTRACTED_SIZE`, `ARGOCD_REPO_SERVER_DISABLE_HELM_MANIFEST_MAX_EXTRACTED_SIZE`.
- **Exposure**: server config.
- **Tests required**:
  - positive: second render does not re-download; hard refresh re-downloads.
  - negative: zip-bomb chart exceeding limit fails.

### SRC-047 Helm error and command reporting
- **Upstream**: [util/helm/cmd.go#L472](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/cmd.go#L472), [util/helm/cmd.go#L62](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/cmd.go#L62), [reposerver/repository/repository.go#L1422](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L1422)
- **Maturity**: GA
- **Semantics**: The exact `helm template` command line is returned in `ManifestResponse.commands` with randomized repo checkout paths replaced by `.` and the inline-values temp file replaced by `<temp file with values from source.helm.values/valuesObject>`. Errors include the helm stderr; if the message contains `--api-versions`, all `--api-versions X ` sequences are replaced by `<api versions removed> `. Credentials in helm command logs are redacted (`--username ******`, `--password ******`).
- **Edge cases**: commands field populated only on success.
- **Configuration surface**: none.
- **Exposure**: API `ManifestResponse.commands` (used by hydrator/UI).
- **Tests required**:
  - positive: commands string matches upstream expectation format exactly.
  - negative: failing chart with many API versions → message contains `<api versions removed>`.

---

## E. Kustomize

### SRC-048 Kustomize build basics
- **Upstream**: [reposerver/repository/repository.go#L1739](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L1739), [util/kustomize/kustomize.go#L124](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/kustomize/kustomize.go#L124), [util/kustomize/kustomize.go#L383](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/kustomize/kustomize.go#L383), [docs/user-guide/kustomize.md#L23](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/kustomize.md#L23), [docs/user-guide/kustomize.md#L167](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/kustomize.md#L167)
- **Maturity**: GA
- **Semantics**: Optional `kustomize edit` mutations (SRC-049..063, run with cwd = app path, in fixed order: nameprefix, namesuffix, images, replicas, labels, annotations, namespace, patches, components) are applied to the checked-out kustomization, then `kustomize build <appPath>` runs with cwd = repo root. Build env = repo-server env + build env vars (SRC-091, exported as real env vars) + git credential env of the app's repository (so private remote bases over the same creds work) + `GIT_SSL_CAINFO=<bundle>` when the repo URL is HTTPS and a custom CA is configured for its host + proxy env; PATH is prefixed with the git and helm wrapper dirs (SRC-067/068). Output YAML is split into objects. Returned `commands` have the repo root replaced by `.`.
- **Edge cases**: build failure → `` `kustomize build <path>` failed exit status 1: <stderr> ``; remote bases with different credentials than the app repo fail (by design); edit commands (except `edit add component`) inherit only the repo-server env.
- **Configuration surface**: CRD `spec.source.kustomize`; `argocd-cm` kustomize keys (SRC-064/065).
- **Exposure**: CRD; CLI kustomize flags; UI Kustomize parameters.
- **Tests required**:
  - positive: overlay renders; command list equals upstream ordering (`Test_GenerateManifests_Commands` kustomize case).
  - positive: `ARGOCD_APP_NAME` visible to a kustomize exec plugin/env-based generator.
  - negative: invalid kustomization → error includes kustomize stderr.

### SRC-049 Kustomize `namePrefix`
- **Upstream**: [pkg/apis/application/v1alpha1/types.go#L722](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L722), [util/kustomize/kustomize.go#L165](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/kustomize/kustomize.go#L165)
- **Maturity**: GA
- **Semantics**: `kustomize edit set nameprefix -- <prefix>` (overrides any prefix in kustomization). Not env-substituted. Makes processing exclusive (SRC-116).
- **Edge cases**: prefix starting with `-` is safe due to `--`.
- **Configuration surface**: `spec.source.kustomize.namePrefix`.
- **Exposure**: CRD; CLI `--nameprefix`, `argocd app unset --nameprefix`; UI "NAME PREFIX" ([application-parameters.tsx#L734](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-parameters/application-parameters.tsx#L734)).
- **Tests required**:
  - positive: all resource names prefixed; command `kustomize edit set nameprefix -- test-prefix`.

### SRC-050 Kustomize `nameSuffix`
- **Upstream**: [pkg/apis/application/v1alpha1/types.go#L724](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L724), [util/kustomize/kustomize.go#L174](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/kustomize/kustomize.go#L174)
- **Maturity**: GA
- **Semantics**: `kustomize edit set namesuffix -- <suffix>`; exclusive processing.
- **Edge cases**: as nameprefix.
- **Configuration surface**: `spec.source.kustomize.nameSuffix`.
- **Exposure**: CRD; CLI `--namesuffix`, `argocd app unset --namesuffix`; UI "NAME SUFFIX" ([application-parameters.tsx#L742](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-parameters/application-parameters.tsx#L742)).
- **Tests required**:
  - positive: names suffixed.

### SRC-051 Kustomize `images`
- **Upstream**: [pkg/apis/application/v1alpha1/types.go#L687](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L687), [util/kustomize/kustomize.go#L183](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/kustomize/kustomize.go#L183)
- **Maturity**: GA
- **Semantics**: List of kustomize image specs (`[old=]name[:tag|@digest]`), each env-substituted, passed in one `kustomize edit set image <spec...>` call. CLI/UI merge semantics: an image whose name (text before the first of `=`, `:`, `@` present in the new spec) matches an existing entry replaces it, else appended.
- **Edge cases**: digest form `alpine@sha256:...`; `${ARGOCD_APP_REVISION}` substitution supported; exclusive processing.
- **Configuration surface**: `spec.source.kustomize.images[]`.
- **Exposure**: CRD; CLI `--kustomize-image` (repeatable), `argocd app unset --kustomize-image <name>`; UI "IMAGES" with overrides seeded from app details ([application-parameters.tsx#L770](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-parameters/application-parameters.tsx#L770)).
- **Tests required**:
  - positive: `image=override` replaces image in Deployment.
  - positive: env-substituted tag.
  - positive: CLI merge replaces same-name image.

### SRC-052 Kustomize `replicas`
- **Upstream**: [pkg/apis/application/v1alpha1/types.go#L761](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L761), [util/kustomize/kustomize.go#L201](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/kustomize/kustomize.go#L201)
- **Maturity**: GA
- **Semantics**: List of `{name, count}` where `count` is int-or-string; rendered as `kustomize edit set replicas name=count ...`. Note: replicas alone do **not** mark the source as non-concurrent (upstream `AllowsConcurrentProcessing` ignores replicas).
- **Edge cases**: non-integer string count → `expected integer value for count. Received: <s>`; CLI form `name=count` (error `expected parameter of the form: name=count. Received: ...`).
- **Configuration surface**: `spec.source.kustomize.replicas[].{name,count}`.
- **Exposure**: CRD; CLI `--kustomize-replica`, `argocd app unset --kustomize-replica`.
- **Tests required**:
  - positive: `guestbook-ui=1337` and `count: "3"` both accepted.
  - negative: `count: "abc"` → exact error.

### SRC-053 Kustomize `commonLabels`
- **Upstream**: [pkg/apis/application/v1alpha1/types.go#L728](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L728), [util/kustomize/kustomize.go#L222](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/kustomize/kustomize.go#L222), [util/kustomize/kustomize.go#L105](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/kustomize/kustomize.go#L105)
- **Maturity**: GA
- **Semantics**: `kustomize edit add label [--force] [--without-selector] [--include-templates] k:v ...`. Label values are **always** env-substituted. For kustomize < v3.8.5 all pairs are joined into one comma-separated argument; otherwise one argument per pair (map order not stable). Exclusive processing.
- **Edge cases**: existing key in kustomization without force → kustomize error; values containing `:` handled by kustomize parsing.
- **Configuration surface**: `spec.source.kustomize.commonLabels`.
- **Exposure**: CRD; CLI `--kustomize-common-label k=v` (repeatable; parsed with label parser).
- **Tests required**:
  - positive: label applied to metadata and (by default) selectors.
  - positive: `${ARGOCD_APP_NAME}` substituted in label value.
  - negative: conflicting existing label without force fails.

### SRC-054 Kustomize `forceCommonLabels`
- **Upstream**: [pkg/apis/application/v1alpha1/types.go#L734](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L734), [util/kustomize/kustomize.go#L225](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/kustomize/kustomize.go#L225), [docs/user-guide/kustomize.md#L40](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/kustomize.md#L40)
- **Maturity**: GA
- **Semantics**: Adds `--force` to `edit add label`, allowing replacement of existing labels. Only effective when `commonLabels` non-empty.
- **Edge cases**: flag set with empty commonLabels → no command.
- **Configuration surface**: `spec.source.kustomize.forceCommonLabels`.
- **Exposure**: CRD; CLI `--kustomize-force-common-label`.
- **Tests required**:
  - positive: existing label overridden with force.

### SRC-055 Kustomize `labelWithoutSelector`
- **Upstream**: [pkg/apis/application/v1alpha1/types.go#L750](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L750), [util/kustomize/kustomize.go#L228](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/kustomize/kustomize.go#L228), [docs/user-guide/kustomize.md#L32](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/kustomize.md#L32)
- **Maturity**: GA
- **Semantics**: Adds `--without-selector`: labels are applied to metadata but not selectors (and, per kustomize, not templates unless SRC-056). Only effective with non-empty commonLabels.
- **Edge cases**: requires a kustomize version supporting the flag (bundled 5.8.1 does).
- **Configuration surface**: `spec.source.kustomize.labelWithoutSelector`.
- **Exposure**: CRD; CLI `--kustomize-label-without-selector`.
- **Tests required**:
  - positive: Deployment selector unchanged, metadata labeled.

### SRC-056 Kustomize `labelIncludeTemplates`
- **Upstream**: [pkg/apis/application/v1alpha1/types.go#L758](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L758), [util/kustomize/kustomize.go#L231](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/kustomize/kustomize.go#L231), [docs/user-guide/kustomize.md#L33](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/kustomize.md#L33)
- **Maturity**: GA
- **Semantics**: Adds `--include-templates` (labels applied to pod templates). Typically combined with `labelWithoutSelector`.
- **Edge cases**: only with non-empty commonLabels.
- **Configuration surface**: `spec.source.kustomize.labelIncludeTemplates`.
- **Exposure**: CRD; CLI `--kustomize-label-include-templates`.
- **Tests required**:
  - positive: `--force --without-selector --include-templates test:label` command form; template labels set, selector not.

### SRC-057 Kustomize `commonAnnotations`
- **Upstream**: [pkg/apis/application/v1alpha1/types.go#L732](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L732), [util/kustomize/kustomize.go#L247](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/kustomize/kustomize.go#L247)
- **Maturity**: GA
- **Semantics**: `kustomize edit add annotation [--force] k:v ...`; values **not** env-substituted unless SRC-059. Exclusive processing.
- **Edge cases**: existing key without force → kustomize error.
- **Configuration surface**: `spec.source.kustomize.commonAnnotations`.
- **Exposure**: CRD; CLI `--kustomize-common-annotation k=v`.
- **Tests required**:
  - positive: annotation applied; literal `${ARGOCD_APP_NAME}` kept when envsubst off.

### SRC-058 Kustomize `forceCommonAnnotations`
- **Upstream**: [pkg/apis/application/v1alpha1/types.go#L736](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L736), [util/kustomize/kustomize.go#L250](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/kustomize/kustomize.go#L250), [docs/user-guide/kustomize.md#L41](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/kustomize.md#L41)
- **Maturity**: GA
- **Semantics**: Adds `--force` to `edit add annotation`.
- **Edge cases**: only with non-empty commonAnnotations.
- **Configuration surface**: `spec.source.kustomize.forceCommonAnnotations`.
- **Exposure**: CRD; CLI `--kustomize-force-common-annotation`.
- **Tests required**:
  - positive: existing annotation replaced with force; fails without.

### SRC-059 Kustomize `commonAnnotationsEnvsubst`
- **Upstream**: [pkg/apis/application/v1alpha1/types.go#L740](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L740), [util/kustomize/kustomize.go#L254](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/kustomize/kustomize.go#L254), [docs/user-guide/kustomize.md#L239](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/kustomize.md#L239)
- **Maturity**: GA
- **Semantics**: When `true`, annotation values are env-substituted with the build env before `edit add annotation`.
- **Edge cases**: unknown variable → empty string; `$$` escape.
- **Configuration surface**: `spec.source.kustomize.commonAnnotationsEnvsubst`.
- **Exposure**: CRD only (no CLI flag).
- **Tests required**:
  - positive: `app-source: ${ARGOCD_APP_NAME}` → `app-source: <app>` (`annotation-test-app` in upstream test).

### SRC-060 Kustomize `namespace`
- **Upstream**: [pkg/apis/application/v1alpha1/types.go#L738](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L738), [util/kustomize/kustomize.go#L272](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/kustomize/kustomize.go#L272), [docs/user-guide/kustomize.md#L289](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/kustomize.md#L289)
- **Maturity**: GA
- **Semantics**: `kustomize edit set namespace -- <ns>`; kustomize sets namespace fields (including subjects etc.). Takes precedence over destination namespace defaulting done later by the controller. Exclusive processing.
- **Edge cases**: also consulted by `GetNamespaceOrDefault` (irrelevant for Kustomize rendering).
- **Configuration surface**: `spec.source.kustomize.namespace`.
- **Exposure**: CRD; CLI `--kustomize-namespace`, `argocd app unset --kustomize-namespace`; UI "NAMESPACE" ([application-parameters.tsx#L750](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-parameters/application-parameters.tsx#L750)).
- **Tests required**:
  - positive: ClusterRoleBinding subject namespace set.

### SRC-061 Kustomize `patches`
- **Upstream**: [pkg/apis/application/v1alpha1/types.go#L804](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L804), [util/kustomize/kustomize.go#L282](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/kustomize/kustomize.go#L282), [docs/user-guide/kustomize.md#L48](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/kustomize.md#L48)
- **Maturity**: GA
- **Semantics**: Each patch `{path, patch, target{group,version,kind,name,namespace,labelSelector,annotationSelector}, options}` is appended to the `patches:` list of the first existing kustomization file among `kustomization.yaml`, `kustomization.yml`, `Kustomization` in the app path (list created if absent). The file is rewritten (re-marshalled YAML, original mode preserved; comments/ordering lost). A comment line `# kustomization.yaml updated with patches. There is no \`kustomize edit\` command for adding patches. ...` is appended to `commands`. Not env-substituted. Exclusive processing.
- **Edge cases**: no kustomization file → `kustomization file not found in the path`; kustomization not a map → `expected kustomization.yaml to be type map[string]any, but got ...`; existing `patches` not a list → `expected 'patches' field in kustomization.yaml to be []any, but got <T>`; invalid patch content → kustomize build error.
- **Configuration surface**: `spec.source.kustomize.patches[]`.
- **Exposure**: CRD only (no CLI flag); ApplicationSet templates.
- **Tests required**:
  - positive: JSON6902 inline patch changes containerPort; merged with existing file patches.
  - negative: missing kustomization file → exact error.

### SRC-062 Kustomize `components`
- **Upstream**: [pkg/apis/application/v1alpha1/types.go#L746](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L746), [util/kustomize/kustomize.go#L337](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/kustomize/kustomize.go#L337), [docs/user-guide/kustomize.md#L134](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/kustomize.md#L134)
- **Maturity**: GA
- **Semantics**: Paths (relative to app path) added via `kustomize edit add component <c...>` executed with the full build env. Requires kustomize ≥ v3.7.0 (version detected via `kustomize version --short`; unparsable version assumed v99.99.99). Exclusive processing.
- **Edge cases**: older kustomize → `kustomize components require kustomize v3.7.0 and above`; nonexistent component dir (without SRC-063) → kustomize error.
- **Configuration surface**: `spec.source.kustomize.components[]`.
- **Exposure**: CRD only (no CLI flag for components themselves).
- **Tests required**:
  - positive: `../component` added and its patches applied.
  - negative: missing component dir fails.

### SRC-063 Kustomize `ignoreMissingComponents`
- **Upstream**: [pkg/apis/application/v1alpha1/types.go#L748](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L748), [util/kustomize/kustomize.go#L346](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/kustomize/kustomize.go#L346)
- **Maturity**: GA
- **Semantics**: When `true`, each component path is checked for existence inside the repo root (using a root-confined filesystem handle); missing ones (or ones escaping the repo) are dropped; if none remain no `edit add component` is run.
- **Edge cases**: component path escaping repo root is silently dropped rather than erroring; repo root open failure → `failed to open the repo folder: ...`.
- **Configuration surface**: `spec.source.kustomize.ignoreMissingComponents`.
- **Exposure**: CRD; CLI `--ignore-missing-components`, `argocd app unset --ignore-missing-components`.
- **Tests required**:
  - positive: one existing + one missing component → only existing added.
  - positive: all missing → no command, build succeeds.

### SRC-064 Kustomize version selection
- **Upstream**: [pkg/apis/application/v1alpha1/types.go#L730](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L730), [util/settings/settings.go#L290](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L290), [util/settings/settings.go#L1264](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L1264), [util/settings/settings.go#L1311](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L1311), [docs/user-guide/kustomize.md#L195](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/kustomize.md#L195), [docs/operator-manual/argocd-cm.yaml#L263](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cm.yaml#L263)
- **Maturity**: GA (`kustomize.version.<v>` key form and `KustomizeOptions.binaryPath` are Deprecated)
- **Semantics**: Binary = deprecated `kustomizeOptions.binaryPath` if set (warning logged); else if `spec.source.kustomize.version` is set, the path registered for that exact version name via `argocd-cm` `kustomize.path.<version>` (or deprecated `kustomize.version.<version>`); else `kustomize` on PATH. Applies to manifest generation and GetAppDetails.
- **Edge cases**: unregistered version → `error getting kustomize binary path: kustomize version <v> is not registered`; same version registered under both key forms → `failed to add kustomize version from "<key>": found duplicate kustomize version: <v>` (settings load fails); version string compared literally (`v3.5.4` ≠ `3.5.4`).
- **Configuration surface**: `spec.source.kustomize.version`; `argocd-cm` `kustomize.path.<v>`, `kustomize.version.<v>`.
- **Exposure**: CRD; CLI `--kustomize-version`, `argocd app unset --kustomize-version`; UI "VERSION" dropdown from settings `kustomizeVersions` ([application-parameters.tsx#L715](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-parameters/application-parameters.tsx#L715)).
- **Tests required**:
  - positive: registered version uses that binary (upstream `Test_GenerateManifest_KustomizeWithVersionOverride`, `TestGetAppDetailsKustomize_CustomVersion`).
  - negative: unregistered version → exact error.
  - negative: duplicate registration → settings error.

### SRC-065 Kustomize build options
- **Upstream**: [util/kustomize/kustomize.go#L384](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/kustomize/kustomize.go#L384), [util/kustomize/kustomize.go#L425](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/kustomize/kustomize.go#L425), [util/settings/settings.go#L1274](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L1274), [docs/user-guide/kustomize.md#L175](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/kustomize.md#L175)
- **Maturity**: GA
- **Semantics**: `argocd-cm` `kustomize.buildOptions` string is split on whitespace (no shell quoting) and appended: `kustomize build <path> <opts...>`. Applies to every Kustomize app regardless of selected version. Keys `kustomize.buildOptions.<version>` are parsed into the version registry but are **not** applied by the repo-server at this commit (see Doc/implementation differences).
- **Edge cases**: options with quoted spaces break; `--enable-helm` triggers SRC-066; invalid option → kustomize error.
- **Configuration surface**: `argocd-cm` `kustomize.buildOptions`, `kustomize.buildOptions.<version>`.
- **Exposure**: ConfigMap; API settings (`GET /api/v1/settings` exposes `kustomizeOptions.buildOptions`).
- **Tests required**:
  - positive: `--load-restrictor LoadRestrictionsNone` allows files outside kustomization dir.
  - positive (parity): per-version build options have no effect.

### SRC-066 Kustomize `kubeVersion` / `apiVersions` (Helm inflation)
- **Upstream**: [pkg/apis/application/v1alpha1/types.go#L753](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L753), [util/kustomize/kustomize.go#L428](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/kustomize/kustomize.go#L428), [reposerver/repository/repository.go#L1745](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L1745), [docs/user-guide/kustomize.md#L271](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/kustomize.md#L271)
- **Maturity**: GA
- **Semantics**: Only when build options contain the substring `--enable-helm` **and** kustomize ≥ v5.3.0 **and** it is a manifest-generation call (not GetAppDetails): append `--helm-kube-version <v>` (normalized like SRC-036) and one `--helm-api-versions <v>` per API version. Values: `kustomize.kubeVersion`/`apiVersions` if set, else cluster values (note `helm.kubeVersion` would take precedence via shared helper, but cannot co-exist on a Kustomize source).
- **Edge cases**: without `--enable-helm` the fields are ignored silently; unparsable kubeVersion → `could not parse kubernetes version ...`.
- **Configuration surface**: `spec.source.kustomize.kubeVersion`, `.apiVersions[]`; `argocd-cm` `kustomize.buildOptions: --enable-helm`.
- **Exposure**: CRD; CLI `--kustomize-kube-version`, `--kustomize-api-versions`.
- **Tests required**:
  - positive: command `kustomize build . --enable-helm --helm-kube-version 5.6.7 --helm-api-versions v1 --helm-api-versions v2`.
  - negative: no `--enable-helm` → flags absent.

### SRC-067 Kustomize remote-base git argument-injection guard
- **Upstream**: [util/kustomize/gitwrapper.go#L44](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/kustomize/gitwrapper.go#L44), [util/kustomize/gitwrapper.go#L107](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/kustomize/gitwrapper.go#L107)
- **Maturity**: GA
- **Semantics**: A `git` shell wrapper is installed (once per process) in a temp dir prepended to the `kustomize build` PATH. Non-`fetch` subcommands pass through; `git fetch --depth=1 <repo> <ref>` (exactly 4 args) is rewritten to insert `--end-of-options` before the positionals; any other `fetch` shape is refused (exit 1, stderr `git wrapper: unrecognized 'git fetch' invocation, refusing (see GHSA-9v9p-x54c-58gc)`). Remote bases keep working.
- **Edge cases**: git absent from PATH → wrapper not installed (warning); git path containing `'` or newline → build error `failed to install git wrapper for kustomize build: ...`.
- **Configuration surface**: none.
- **Exposure**: implicit.
- **Tests required**:
  - negative: remote base with `?ref=--upload-pack=touch /tmp/x` does not execute the command.
  - positive: normal `?ref=v1.0.0` remote base still builds.

### SRC-068 Kustomize Helm-plugin isolation guard (`--enable-helm`)
- **Upstream**: [util/kustomize/helmwrapper.go#L42](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/kustomize/helmwrapper.go#L42), [util/kustomize/helmwrapper.go#L110](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/kustomize/helmwrapper.go#L110)
- **Maturity**: GA
- **Semantics**: A `helm` wrapper is prepended to the kustomize PATH; it forces `HELM_DATA_HOME`, `HELM_CONFIG_HOME` to a fixed, empty Argo-owned directory and `HELM_PLUGINS=<dir>/plugins` before exec'ing real helm, so kustomizations (e.g. `helmGlobals.configHome`) cannot make Helm load attacker-supplied plugins/downloaders. Chart inflation from http/https/oci repos still works.
- **Edge cases**: helm absent → not installed; unusual helm path chars → error `failed to install helm wrapper for kustomize build: ...`.
- **Configuration surface**: none.
- **Exposure**: implicit.
- **Tests required**:
  - negative: kustomization with `helmGlobals.configHome` pointing to repo dir containing a downloader plugin → plugin not executed.
  - positive: `helmCharts` from https repo inflates.

---

## F. Config Management Plugins v2 (sidecar CMP)

### SRC-069 Plugin configuration file (`ConfigManagementPlugin` in `plugin.yaml`)
- **Upstream**: [cmpserver/plugin/config.go#L19](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmpserver/plugin/config.go#L19), [cmpserver/plugin/config.go#L68](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmpserver/plugin/config.go#L68), [cmpserver/plugin/config.go#L85](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmpserver/plugin/config.go#L85), [cmd/argocd-cmp-server/commands/argocd_cmp_server.go#L50](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-cmp-server/commands/argocd_cmp_server.go#L50), [common/common.go#L110](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L110), [docs/operator-manual/config-management-plugins.md#L26](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/config-management-plugins.md#L26)
- **Maturity**: GA
- **Semantics**: The `argocd-cmp-server` sidecar reads `<config-dir>/plugin.yaml` (default dir `/home/argocd/cmp-server/config`, flag `--config-dir-path`) at startup. Schema: `apiVersion` (not validated), `kind: ConfigManagementPlugin`, `metadata.name` (required), `spec.version` (optional), `spec.init{command[],args[]}`, `spec.generate{command[],args[]}` (required), `spec.discover{fileName, find{glob, command[], args[]}}`, `spec.parameters{static[], dynamic{command[],args[]}}`, `spec.preserveFileMode` (bool), `spec.provideGitCreds` (bool). Commands execute as `command[0]` with args `command[1:]+args`. It is *not* a Kubernetes CRD (the v1 `argocd-cm` `configManagementPlugins` mechanism and `lockRepo` were removed; `lockRepo` exists only in the deprecated, unused v1 type).
- **Edge cases**: startup fails (fatal) with `invalid plugin configuration file. metadata.name should be non-empty`, `invalid plugin configuration file. kind should be ConfigManagementPlugin, found <k>`, `invalid plugin configuration file. spec.generate command should be non-empty`; no discovery → info log telling users to set `spec.source.plugin.name` to `<name>[-<version>]`; config changes require sidecar restart.
- **Configuration surface**: sidecar file `plugin.yaml`; flags `--config-dir-path`, `--loglevel`, `--logformat` (env `ARGOCD_CMP_SERVER_LOGLEVEL`/`_LOGFORMAT`), OTLP flags.
- **Exposure**: sidecar container config only.
- **Tests required**:
  - negative: each of the three validation errors aborts startup.
  - positive: minimal config (name + generate) starts and serves.
  - positive: `command: [sh, -c]` + `args: ['echo ...']` executes `sh -c 'echo ...'`.

### SRC-070 cmp-server runtime, socket naming and registration
- **Upstream**: [cmpserver/plugin/config.go#L99](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmpserver/plugin/config.go#L99), [cmpserver/server.go#L84](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmpserver/server.go#L84), [cmpserver/server.go#L108](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmpserver/server.go#L108), [cmpserver/plugin/plugin.proto#L68](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmpserver/plugin/plugin.proto#L68), [common/common.go#L411](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L411), [docs/operator-manual/config-management-plugins.md#L176](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/config-management-plugins.md#L176)
- **Maturity**: GA
- **Semantics**: The sidecar listens on a Unix socket `<sockdir>/<metadata.name>-<spec.version>.sock` (or `<name>.sock` if no version), where `<sockdir>` = `ARGOCD_PLUGINSOCKFILEPATH` or `/home/argocd/cmp-server/plugins` (shared volume with repo-server). Stale socket is removed before listen and on SIGINT/SIGTERM. gRPC services: `plugin.ConfigManagementPluginService` (`GenerateManifest`, `MatchRepository`, `GetParametersAnnouncement` — client-streaming; `CheckPluginConfiguration` — unary), Version service, gRPC health, reflection. Max send/recv message size `ARGOCD_GRPC_MAX_SIZE_MB` (default 100 MiB). The repo-server discovers plugins purely by the presence of socket files.
- **Edge cases**: two sidecars with same name/version clobber each other's socket; repo-server dial uses insecure transport over the socket with retry (3 attempts, linear 1s backoff) for unary calls only.
- **Configuration surface**: env `ARGOCD_PLUGINSOCKFILEPATH` (both containers), `ARGOCD_GRPC_MAX_SIZE_MB`, `ARGOCD_ENABLE_GRPC_TIME_HISTOGRAM`.
- **Exposure**: internal gRPC over Unix socket.
- **Tests required**:
  - positive: socket named `my-plugin-v1.0.sock` when version set.
  - positive: socket removed on SIGTERM.
  - positive: CheckPluginConfiguration returns `isDiscoveryConfigured` and `provideGitCreds`.

### SRC-071 Plugin selection by name (`spec.source.plugin.name`)
- **Upstream**: [util/app/discovery/discovery.go#L91](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/app/discovery/discovery.go#L91), [util/app/discovery/discovery.go#L159](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/app/discovery/discovery.go#L159), [reposerver/repository/repository.go#L1758](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L1758), [docs/operator-manual/config-management-plugins.md#L287](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/config-management-plugins.md#L287)
- **Maturity**: GA
- **Semantics**: With `plugin.name` set, only socket `<name>.sock` is dialed (name must be `<metadata.name>-<spec.version>` or `<metadata.name>`). If that plugin has no discovery configured it is used unconditionally. If it has discovery, `MatchRepository` must return supported; otherwise not used. Empty name → auto-discovery (SRC-075). The name must not escape the socket dir.
- **Edge cases**: unknown name / dial failure → `could not find cmp-server plugin with name "<n>" supporting the given repository: error dialing to cmp-server for plugin <n>.sock: ...`; discovery mismatch → `could not find cmp-server plugin with name "<n>" supporting the given repository`; `../x` name → `invalid socket file path, "<f>" is outside plugin socket dir "<dir>"`; whole failure wrapped as `CMP processing failed for application "<app>": ...`.
- **Configuration surface**: `spec.source.plugin.name`.
- **Exposure**: CRD; CLI `--config-management-plugin` (sets `plugin: {name}` replacing the whole plugin block); UI plugin NAME dropdown from `GET /api/v1/settings/plugins` ([application-parameters.tsx#L876](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-parameters/application-parameters.tsx#L876)).
- **Tests required**:
  - positive: named plugin without discovery used for any repo.
  - negative: named plugin whose discovery doesn't match → exact error.
  - negative: nonexistent name → dial error message.

### SRC-072 Discovery: `discover.fileName`
- **Upstream**: [cmpserver/plugin/plugin.go#L335](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmpserver/plugin/plugin.go#L335), [docs/operator-manual/config-management-plugins.md#L128](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/config-management-plugins.md#L128)
- **Maturity**: GA
- **Semantics**: Go `filepath.Glob` of `<appDir>/<fileName>` inside the received tarball (no `**`); supported if ≥1 match. Highest priority: if set, `find.glob` and `find.command` are ignored.
- **Edge cases**: malformed pattern → MatchRepository error `match repository error: error finding filename match for pattern "<p>": ...` (treated as non-match by auto-discovery; error reported for named plugins); `./subdir/s*.yaml` style allowed.
- **Configuration surface**: `plugin.yaml` `spec.discover.fileName`.
- **Exposure**: sidecar config.
- **Tests required**:
  - positive: `*.cue` matches app with `main.cue`.
  - negative: `**/x.yaml` does not match nested (no doublestar).

### SRC-073 Discovery: `discover.find.glob`
- **Upstream**: [cmpserver/plugin/plugin.go#L347](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmpserver/plugin/plugin.go#L347)
- **Maturity**: GA
- **Semantics**: `zglob` (supports `**`) of `<appDir>/<glob>`; supported if ≥1 match. Used only when `fileName` is empty.
- **Edge cases**: glob error → error as above with `error finding glob match for pattern`.
- **Configuration surface**: `spec.discover.find.glob`.
- **Exposure**: sidecar config.
- **Tests required**:
  - positive: `**/Chart.yaml` matches nested chart.

### SRC-074 Discovery: `discover.find.command`
- **Upstream**: [cmpserver/plugin/plugin.go#L362](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmpserver/plugin/plugin.go#L362), [docs/operator-manual/config-management-plugins.md#L137](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/config-management-plugins.md#L137)
- **Maturity**: GA
- **Semantics**: Used only when neither fileName nor glob set. Runs `command+args` with cwd = the **application directory** inside the CMP workdir and env = sidecar env + request env entries; supported iff exit code 0 **and** stdout (trailing newline trimmed) non-empty.
- **Edge cases**: non-zero exit → MatchRepository error (`error running find command: ...`), treated as non-match in auto-discovery; empty output → not supported.
- **Configuration surface**: `spec.discover.find.command[]`, `spec.discover.find.args[]`.
- **Exposure**: sidecar config.
- **Tests required**:
  - positive: `find . -name env.yaml` with match → supported.
  - negative: command exits 0 with no output → not supported.
  - negative: command exits 1 → not supported (auto) / error (named).

### SRC-075 Auto-discovery order and plugins without discovery
- **Upstream**: [util/app/discovery/discovery.go#L112](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/app/discovery/discovery.go#L112), [util/app/discovery/discovery.go#L189](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/app/discovery/discovery.go#L189), [docs/operator-manual/config-management-plugins.md#L454](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/config-management-plugins.md#L454)
- **Maturity**: GA
- **Semantics**: With no plugin name (either `plugin: {}` explicit type or implicit detection), all socket files in the socket dir are tried in lexical order: `CheckPluginConfiguration` first (cheap); plugins without discovery are skipped; then `MatchRepository` streams the repo; the first supported plugin wins. Each probe of a discovery-configured plugin uploads the full repo tarball.
- **Edge cases**: explicit `plugin: {}` with no matching plugin → `could not find plugin supporting the given repository[: <last probe error>]`; unreadable socket dir → `failed to list all plugins in dir: ...`; implicit detection swallows these errors and falls back to Helm/Kustomize/Directory.
- **Configuration surface**: `spec.source.plugin: {}`; sidecar discovery rules.
- **Exposure**: CRD.
- **Tests required**:
  - positive: two matching plugins → lexically first socket chosen.
  - positive: plugin without discovery never auto-selected.
  - negative: `plugin: {}` with no match → exact error.

### SRC-076 CMP `init` command
- **Upstream**: [cmpserver/plugin/plugin.go#L253](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmpserver/plugin/plugin.go#L253), [docs/operator-manual/config-management-plugins.md#L40](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/config-management-plugins.md#L40)
- **Maturity**: GA
- **Semantics**: If `spec.init.command` is non-empty it runs immediately before `generate`, in the app directory with the same env; stdout ignored; non-zero exit fails generation.
- **Edge cases**: failure message `` `<cmd args>` failed <cause>: <stderr> `` propagated up as `error generating manifests in cmp: rpc error: ...`.
- **Configuration surface**: `spec.init`.
- **Exposure**: sidecar config.
- **Tests required**:
  - positive: init creating a file consumed by generate.
  - negative: init exit 1 → error containing stderr.

### SRC-077 CMP `generate` command and output handling
- **Upstream**: [cmpserver/plugin/plugin.go#L243](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmpserver/plugin/plugin.go#L243), [cmpserver/plugin/plugin.go#L75](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmpserver/plugin/plugin.go#L75), [reposerver/repository/repository.go#L2392](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L2392), [docs/operator-manual/config-management-plugins.md#L47](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/config-management-plugins.md#L47)
- **Maturity**: GA
- **Semantics**: Runs `generate` in the app directory; stdout must be a YAML/JSON object stream; it is split into documents by the sidecar, returned as strings, then re-split into objects by the repo-server. Stderr is logged (info) even on success; empty stdout logs a warning and yields zero manifests (valid — may lead to pruning).
- **Edge cases**: non-zero exit → `` `<args>` failed exit status N: <stderr> ``; unparsable output → repo-server error `failed to convert CMP manifests to unstructured objects: ...` (first 1000 chars logged at debug); pipes without `pipefail` can hide failures (documented caveat).
- **Configuration surface**: `spec.generate`.
- **Exposure**: sidecar config; result via GenerateManifest.
- **Tests required**:
  - positive: JSON ConfigMap echo rendered with `$ARGOCD_APP_NAME` expanded by the shell.
  - negative: invalid YAML output → exact error prefix.
  - positive: empty output → 0 manifests, no error.

### SRC-078 CMP build environment (`ARGOCD_APP_*`, `KUBE_VERSION`, `KUBE_API_VERSIONS`)
- **Upstream**: [reposerver/repository/repository.go#L2351](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L2351), [cmpserver/plugin/plugin.go#L252](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmpserver/plugin/plugin.go#L252), [docs/operator-manual/config-management-plugins.md#L217](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/config-management-plugins.md#L217)
- **Maturity**: GA
- **Semantics**: For generate (and discovery when `plugin` is explicit), env = all standard build vars (SRC-091) + `KUBE_VERSION=<normalized cluster version>` + `KUBE_API_VERSIONS=<comma-joined cluster API versions>` + plugin env (SRC-079) + parameter env (SRC-080) + optional git creds (SRC-084). These are sent as `EnvEntry` list in stream metadata; the sidecar runs commands with its own process env followed by these entries (later duplicates win per exec semantics). For implicit detection probes only `ARGOCD_APP_*` are sent. For GetParametersAnnouncement only `ARGOCD_APP_NAME`, `ARGOCD_APP_SOURCE_REPO_URL`, `ARGOCD_APP_SOURCE_PATH`, `ARGOCD_APP_SOURCE_TARGET_REVISION` (+ plugin env/params).
- **Edge cases**: `KUBE_VERSION` uses only cluster version (source kubeVersion fields ignored); unparsable cluster version → `could not parse kubernetes version ...`; env entries without `=` dropped.
- **Configuration surface**: none.
- **Exposure**: implicit.
- **Tests required**:
  - positive: generate script echoes all vars; values match request (revision, short revisions, project, namespace).
  - positive: KUBE_API_VERSIONS comma list.

### SRC-079 CMP application env (`plugin.env` → `ARGOCD_ENV_*`)
- **Upstream**: [reposerver/repository/repository.go#L2376](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L2376), [pkg/apis/application/v1alpha1/types.go#L1136](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1136), [docs/operator-manual/config-management-plugins.md#L223](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/config-management-plugins.md#L223)
- **Maturity**: GA
- **Semantics**: Each `spec.source.plugin.env[] {name,value}` becomes `ARGOCD_ENV_<name>=<value>` after substituting references to the build env (including `KUBE_VERSION`/`KUBE_API_VERSIONS`) into the value; `$$` escapes. The prefix prevents overriding sensitive variables. Name is not case-converted or sanitized.
- **Edge cases**: references to sidecar system variables are *not* resolved (expand to empty, see Doc differences); CLI `--plugin-env NAME=VALUE` (error `expected env entry of the form: param=value but received: ...`), duplicates merged by name; `argocd app unset --plugin-env NAME` removes (error `unable to find env variable with key "<k>" for plugin "<p>"`).
- **Configuration surface**: `spec.source.plugin.env[]`.
- **Exposure**: CRD; CLI `--plugin-env`, `argocd app unset --plugin-env`; UI plugin "ENV" ([application-parameters.tsx#L892](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-parameters/application-parameters.tsx#L892)).
- **Tests required**:
  - positive: `FOO=bar` visible as `ARGOCD_ENV_FOO`; `REV=test-$ARGOCD_APP_REVISION` interpolated.
  - negative: plugin env named `PATH` cannot override PATH (becomes `ARGOCD_ENV_PATH`).

### SRC-080 CMP parameters (`plugin.parameters` → env)
- **Upstream**: [pkg/apis/application/v1alpha1/types.go#L1015](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1015), [pkg/apis/application/v1alpha1/types.go#L1100](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1100), [docs/operator-manual/config-management-plugins.md#L240](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/config-management-plugins.md#L240)
- **Maturity**: GA
- **Semantics**: Parameters `[{name, string | array | map}]` produce `ARGOCD_APP_PARAMETERS=<JSON array>` (custom marshalling: `map` set-but-nil → `{}`, `array` set-but-nil → `[]`, `string` only if set) plus per-parameter vars: `PARAM_<ESC(name)>=<string>`, `PARAM_<ESC(name)>_<i>=<item>` for arrays, `PARAM_<ESC(name)>_<ESC(key)>=<value>` for maps, where ESC = uppercase and every char outside `[A-Z0-9_]` → `_`. Announcement defaults are **not** sent. Parameter values are not env-substituted.
- **Edge cases**: a parameter may carry several of string/array/map simultaneously (all emitted); name collisions after escaping overwrite (`a-b` and `a_b`); map key ordering nondeterministic.
- **Configuration surface**: `spec.source.plugin.parameters[]`.
- **Exposure**: CRD; UI Parameters tab renders announced params and writes `plugin.parameters` ([application-parameters.tsx#L956](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-parameters/application-parameters.tsx#L956)); no CLI flag.
- **Tests required**:
  - positive: JSON exactly `[{"name":"values-files","array":["values-dev.yaml"]},{"name":"helm-parameters","map":{"image.tag":"v1.2.3"}}]`.
  - positive: `PARAM_HELM_PARAMETERS_IMAGE_TAG=v1.2.3`, `PARAM_VALUES_FILES_0=values-dev.yaml`.
  - positive: empty array param → `"array":[]`.

### SRC-081 CMP static parameter announcements
- **Upstream**: [cmpserver/plugin/config.go#L58](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmpserver/plugin/config.go#L58), [reposerver/repository/repository.proto#L206](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.proto#L206), [docs/operator-manual/config-management-plugins.md#L71](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/config-management-plugins.md#L71)
- **Maturity**: GA
- **Semantics**: `spec.parameters.static[]` entries `{name, title, tooltip, required, itemType, collectionType, string, array, map}` are returned verbatim by `GetParametersAnnouncement` for every app. They only drive the UI form; not validated against app values.
- **Edge cases**: `collectionType` must be set for array/map (not inferred) — UI behavior; server does not validate.
- **Configuration surface**: `plugin.yaml` `spec.parameters.static`.
- **Exposure**: API via `GetAppDetails` (`plugin.parametersAnnouncement`); UI Parameters tab.
- **Tests required**:
  - positive: announcements round-trip all fields.

### SRC-082 CMP dynamic parameter announcements
- **Upstream**: [cmpserver/plugin/plugin.go#L413](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmpserver/plugin/plugin.go#L413), [docs/operator-manual/config-management-plugins.md#L106](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/config-management-plugins.md#L106)
- **Maturity**: GA
- **Semantics**: If `spec.parameters.dynamic.command` is set it runs in the app directory; stdout must be a JSON array of announcements; result = dynamic list followed by static list (static "wins" in UI by being later).
- **Edge cases**: command failure → `get parameters announcement error: error executing dynamic parameter output command: ...`; non-JSON → `error unmarshaling dynamic parameter output into ParametersAnnouncementResponse: ...`.
- **Configuration surface**: `spec.parameters.dynamic`.
- **Exposure**: API GetAppDetails; UI.
- **Tests required**:
  - positive: dynamic + static concatenation order.
  - negative: invalid JSON output → exact error.

### SRC-083 CMP `preserveFileMode`
- **Upstream**: [cmpserver/plugin/config.go#L31](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmpserver/plugin/config.go#L31), [util/cmp/stream.go#L52](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/cmp/stream.go#L52), [docs/operator-manual/config-management-plugins.md#L495](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/config-management-plugins.md#L495)
- **Maturity**: GA
- **Semantics**: Default `false`: files extracted in the sidecar get reset (non-executable) modes. `true`: original tar modes preserved (e.g. executable scripts in repo).
- **Edge cases**: applies to all three streaming RPCs.
- **Configuration surface**: `spec.preserveFileMode`.
- **Exposure**: sidecar config.
- **Tests required**:
  - positive: executable script in repo runnable only when true.

### SRC-084 CMP `provideGitCreds`
- **Upstream**: [cmpserver/plugin/config.go#L32](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmpserver/plugin/config.go#L32), [reposerver/repository/repository.go#L2418](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L2418), [docs/operator-manual/config-management-plugins.md#L519](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/config-management-plugins.md#L519)
- **Maturity**: GA
- **Semantics**: Repo-server calls `CheckPluginConfiguration`; if `provideGitCreds` is true and the app repository has git creds, the creds' environment (git ASKPASS-based: askpass helper + nonce; repo-server serves credentials over the shared askpass socket, path overridable by `ARGOCD_ASK_PASS_SOCK`) is appended to the generate env, valid for the duration of the call.
- **Edge cases**: creds env failure → `failed to retrieve git creds environment variables: ...`; not provided to discovery or parameter announcements.
- **Configuration surface**: `spec.provideGitCreds`; env `ARGOCD_ASK_PASS_SOCK` (both containers).
- **Exposure**: sidecar config.
- **Tests required**:
  - positive: plugin can `git ls-remote` a private repo with app creds when enabled.
  - negative: disabled → no GIT_ASKPASS vars in env.

### SRC-085 Repository tarball streaming protocol (repo-server → CMP)
- **Upstream**: [util/cmp/stream.go#L89](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/cmp/stream.go#L89), [util/cmp/stream.go#L114](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/cmp/stream.go#L114), [util/cmp/stream.go#L38](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/cmp/stream.go#L38), [util/cmp/stream.go#L175](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/cmp/stream.go#L175), [cmpserver/plugin/plugin.proto#L11](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmpserver/plugin/plugin.proto#L11), [common/common.go#L422](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L422)
- **Maturity**: GA
- **Semantics**: The root (repo root, or common root per SRC-087) is tar+gzipped to a temp file (excluding globs, SRC-086) computing SHA-256. First message: `metadata{appName=<basename of app path>, appRelPath=<app path relative to root>, checksum, size, env[]}`; then `file{chunk}` messages of `ARGOCD_CMP_CHUNK_SIZE` bytes (default 1024; invalid → default). Receiver writes chunks to a temp file, verifies checksum, untars into a fresh per-request dir (no size limit), and validates `appRelPath` stays within the workdir. As soon as the tarball is built, the repo-server releases its repository lock (tarDone signal) so other operations can proceed while the plugin runs.
- **Edge cases**: nothing to send → `no files to send(<root>)`; checksum mismatch → `file checksum validation error`; first message not metadata → `error getting stream metadata: metadata is nil`; out-of-bounds appRelPath → `illegal appPath: out of workDir bound`; stream ctx cancellation reported with ctx error.
- **Configuration surface**: env `ARGOCD_CMP_CHUNK_SIZE` (repo-server).
- **Exposure**: internal gRPC.
- **Tests required**:
  - positive: large repo streamed in chunks reconstructs identically.
  - negative: tampered chunk → checksum error.
  - positive: repo lock released before generate completes (concurrent op not blocked).

### SRC-086 Plugin tarball exclusions
- **Upstream**: [util/io/files/tar.go#L245](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/io/files/tar.go#L245), [cmd/argocd-repo-server/commands/argocd_repo_server.go#L267](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-repo-server/commands/argocd_repo_server.go#L267), [docs/operator-manual/config-management-plugins.md#L359](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/config-management-plugins.md#L359)
- **Maturity**: GA
- **Semantics**: Each glob is matched with Go `filepath.Match` against the path relative to the tar root; matching directories are skipped entirely, matching files omitted. Applies to generate, discovery and announcement streams.
- **Edge cases**: invalid pattern → `error verifying exclusion pattern "<p>": ...` (tar creation fails); `.git/*` excludes contents (dir entry itself unless `.git` also listed); `*` does not cross `/`.
- **Configuration surface**: flag `--plugin-tar-exclude` (repeatable), env `ARGOCD_REPO_SERVER_PLUGIN_TAR_EXCLUSIONS` (`;`-separated), `argocd-cmd-params-cm` `reposerver.plugin.tar.exclusions`.
- **Exposure**: server config.
- **Tests required**:
  - positive: `.git/*` reduces tarball; plugin does not see `.git` contents.
  - negative: malformed glob breaks CMP generation with exact error.

### SRC-087 CMP common-root streaming via `manifest-generate-paths`
- **Upstream**: [reposerver/repository/utils.go#L16](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/utils.go#L16), [reposerver/repository/utils.go#L56](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/utils.go#L56), [reposerver/repository/repository.go#L2407](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L2407), [docs/operator-manual/config-management-plugins.md#L374](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/config-management-plugins.md#L374)
- **Maturity**: GA
- **Semantics**: When enabled, the generate stream sends only the longest common directory prefix of the app path and all annotation paths (`;`-separated; relative to app path, `/`-absolute = repo root; each secure-joined inside the repo). The app path is the deepest possible root. No annotation → full repo (backward compatible). Discovery/announcement streams always send the whole repo.
- **Edge cases**: annotation `.;../shared` with app `apps/a` → root `apps`; disjoint paths → common ancestor (possibly repo root); traversal entries are clamped by secure join; invalid entries skipped with error log.
- **Configuration surface**: flag `--plugin-use-manifest-generate-paths`, env `ARGOCD_REPO_SERVER_PLUGIN_USE_MANIFEST_GENERATE_PATHS`, `reposerver.plugin.use.manifest.generate.paths`; Application annotation `argocd.argoproj.io/manifest-generate-paths`.
- **Exposure**: server config + annotation.
- **Tests required**:
  - positive: plugin only receives `apps/` subtree; relative app path inside tarball correct.
  - positive: flag off → whole repo.

### SRC-088 CMP timeouts and process termination
- **Upstream**: [cmpserver/plugin/plugin.go#L35](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmpserver/plugin/plugin.go#L35), [cmpserver/plugin/plugin.go#L107](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmpserver/plugin/plugin.go#L107), [util/buffered_context/buffered_context.go#L13](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/buffered_context/buffered_context.go#L13), [cmd/argocd-application-controller/commands/argocd_application_controller.go#L264](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-application-controller/commands/argocd_application_controller.go#L264), [docs/operator-manual/config-management-plugins.md#L318](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/config-management-plugins.md#L318)
- **Maturity**: GA
- **Semantics**: The CMP request inherits the caller's deadline (controller/API server repo-server RPC timeout, default 60s via `--repo-server-timeout-seconds` / `controller.repo.server.timeout.seconds` / `server.repo.server.timeout.seconds`). The sidecar shortens it by 100 ms; commands run in their own process group; on context expiry the group gets SIGTERM, then SIGKILL after 5 s. No per-command `ARGOCD_EXEC_TIMEOUT` is applied by cmp-server code.
- **Edge cases**: requests without deadline run unbounded; partial output discarded on kill (error `... failed signal: terminated`).
- **Configuration surface**: `argocd-cmd-params-cm` `controller.repo.server.timeout.seconds`, `server.repo.server.timeout.seconds`.
- **Exposure**: server config.
- **Tests required**:
  - negative: `sleep 120` generate with 60s RPC deadline fails at ~60s and child processes are killed.
  - positive: child spawned by generate killed via process group.

### SRC-089 cmp-server work directory and isolation
- **Upstream**: [cmpserver/plugin/plugin.go#L53](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmpserver/plugin/plugin.go#L53), [cmpserver/plugin/plugin.go#L179](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmpserver/plugin/plugin.go#L179), [common/common.go#L436](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L436), [docs/operator-manual/config-management-plugins.md#L198](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/config-management-plugins.md#L198)
- **Maturity**: GA
- **Semantics**: Workdir = `$ARGOCD_CMP_WORKDIR/_cmp_server` or `$TMPDIR/_cmp_server`; wiped and recreated (0700) at startup. Each RPC gets a fresh UUID subdirectory, deleted after the call (failure to delete logged as high-severity). Docs require a dedicated `/tmp` volume per sidecar (not shared with repo-server) and UID 999.
- **Edge cases**: concurrent requests never share directories; plugin mutations of files never affect repo-server checkout.
- **Configuration surface**: env `ARGOCD_CMP_WORKDIR`.
- **Exposure**: sidecar config.
- **Tests required**:
  - positive: two concurrent generates see isolated trees.
  - positive: per-request dir removed after completion/failure.

### SRC-090 Listing installed plugins (`ListPlugins`)
- **Upstream**: [reposerver/repository/repository.go#L285](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L285), [server/settings/settings.go#L163](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/settings/settings.go#L163)
- **Maturity**: GA
- **Semantics**: Returns one `PluginInfo{name}` per socket file in the socket dir (name = filename without `.sock`, i.e. `<name>[-<version>]`). Exposed by API server `GET /api/v1/settings/plugins` (used by UI plugin dropdown).
- **Edge cases**: missing socket dir → `failed to get plugins from dir <dir>, error=...` (API returns `failed to list sidecar plugins from reposerver: ...`); non-socket files ignored.
- **Configuration surface**: `ARGOCD_PLUGINSOCKFILEPATH`.
- **Exposure**: API (repo-server RPC `ListPlugins`; REST `GET /api/v1/settings/plugins`); UI plugin selector.
- **Tests required**:
  - positive: two sidecars → two names, versions included.
  - negative: no socket dir → error.

---

## G. Build environment and in-repo overrides

### SRC-091 Build environment variables and substitution rules
- **Upstream**: [reposerver/repository/repository.go#L1828](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L1828), [reposerver/repository/repository.go#L1851](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L1851), [pkg/apis/application/v1alpha1/types.go#L181](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L181), [pkg/apis/application/v1alpha1/types.go#L4132](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L4132), [docs/user-guide/build-environment.md#L3](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/build-environment.md#L3)
- **Maturity**: GA
- **Semantics**: For manifest generation: `ARGOCD_APP_NAME` (instance name: `<name>`, or `<namespace>_<name>` when the Application lives outside the control-plane namespace), `ARGOCD_APP_NAMESPACE` (destination namespace), `ARGOCD_APP_PROJECT_NAME`, `ARGOCD_APP_REVISION` (resolved commit SHA / chart version / OCI digest; `streamed` checksum for local streams), `ARGOCD_APP_REVISION_SHORT` (first 7 chars), `ARGOCD_APP_REVISION_SHORT_8` (first 8), `ARGOCD_APP_SOURCE_REPO_URL`, `ARGOCD_APP_SOURCE_PATH`, `ARGOCD_APP_SOURCE_TARGET_REVISION` (unresolved spec value). Empty-valued entries are omitted from exported env. Substitution uses `os.Expand` semantics: `$VAR`/`${VAR}`; unknown → empty string; `$$` → `$`. GetAppDetails uses a reduced set (name, revision, repo URL, path, target revision). Where applied: Helm (SRC-038), Jsonnet extVars/TLAs, Kustomize images/commonLabels/(annotations when enabled) and exported env, CMP (SRC-078/079).
- **Edge cases**: revisions shorter than 7/8 chars are not padded; OCI digest `sha256:abc...` short form is `sha256:` (7 chars) — preserved as upstream; `ARGOCD_APP_NAMESPACE` is the destination namespace, not the Application's namespace.
- **Configuration surface**: none.
- **Exposure**: implicit.
- **Tests required**:
  - positive: each variable value for git, helm-chart and OCI sources (upstream `Test_newEnv`).
  - positive: `$$FOO` renders literal `$FOO`; `${UNKNOWN}` → empty.
  - positive: app in non-control-plane namespace → `ns_name`.

### SRC-092 Parameter override file `.argocd-source.yaml`
- **Upstream**: [reposerver/repository/repository.go#L79](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L79), [reposerver/repository/repository.go#L1867](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L1867), [docs/user-guide/parameters.md#L66](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/parameters.md#L66)
- **Maturity**: GA
- **Semantics**: If `<appPath>/.argocd-source.yaml` exists (regular file), its YAML is converted to JSON and applied as an RFC 7386 JSON merge patch onto the JSON form of the ApplicationSource before type detection and rendering. Afterwards `chart`, `path`, `repoURL`, `targetRevision` are restored from the original spec (cannot be overridden); all other source fields (`helm`, `kustomize`, `directory`, `plugin`, `ref`, `name`, `tagPrefix`) can be. Overrides are not persisted in the Application and not part of the manifest cache key (the commit SHA already changes). Used by Argo CD Image Updater write-back.
- **Edge cases**: directory with that name ignored; invalid YAML → `error while parsing source parameters: <file>: ...` wrapped in `error getting app source type`; `null` in patch deletes a field (merge-patch semantics, e.g. removing `helm.parameters`); patch adding `helm:` to an explicit kustomize source → `multiple application sources defined`; applies to GetAppDetails too (upstream `TestGetAppDetailsWithAppParameterFile`).
- **Configuration surface**: repository file.
- **Exposure**: repo content.
- **Tests required**:
  - positive: `kustomize.images` from file applied.
  - positive: attempt to override `path`/`targetRevision` ignored.
  - negative: malformed YAML → error naming the file.

### SRC-093 Application-specific override file `.argocd-source-<appname>.yaml`
- **Upstream**: [reposerver/repository/repository.go#L80](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L80), [reposerver/repository/repository.go#L1870](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L1870), [docs/user-guide/parameters.md#L89](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/parameters.md#L89)
- **Maturity**: GA
- **Semantics**: After the generic file, `<appPath>/.argocd-source-<appName>.yaml` is merged on top (wins on conflicts), where `<appName>` is the request app name = instance name (`<ns>_<name>` for apps outside the control-plane namespace). Same field-restore rules.
- **Edge cases**: app in non-default namespace must use `.argocd-source-<ns>_<name>.yaml`; local CLI generation uses the same naming.
- **Configuration surface**: repository file.
- **Exposure**: repo content.
- **Tests required**:
  - positive: both files present → app-specific value wins (upstream `TestGenerateManifestsWithAppParameterFile`).
  - positive: namespaced app file name.

---

## H. Multiple sources

### SRC-094 Multi-source manifest generation and ref-only sources
- **Upstream**: [controller/state.go#L310](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L310), [reposerver/repository/repository.go#L637](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L637), [reposerver/repository/repository.go#L657](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L657), [docs/user-guide/multiple_sources.md#L9](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/multiple_sources.md#L9)
- **Maturity**: GA
- **Semantics**: When `spec.sources` is non-empty, `spec.source` is ignored; the controller issues one GenerateManifest per source (with `hasMultipleSources=true` and the `refSources` map) and concatenates results in source order. A source with `ref` set, no `path`, and not chart/OCI is short-circuited: only its revision is resolved (no checkout). A source with neither `path` nor `chart` yields no manifests (after checkout). Duplicate resources across sources: last wins (with RepeatedResourceWarning — controller area).
- **Edge cases**: ref source *with* path renders manifests too; for multi-source, `revision` used as commit SHA without `git rev-parse` (it is already resolved by ls-remote).
- **Configuration surface**: `spec.sources[]` incl. `ref`, `name`.
- **Exposure**: CRD; CLI `--source-position`/`--source-name` on `app set/unset`, `--ref`, `--source-name`; UI multi-source panels.
- **Tests required**:
  - positive: two git sources concatenated in order.
  - positive: ref-only source returns revision only, no git checkout (upstream `TestGenerateManifest_RefOnlyShortCircuit`).
  - positive: last source wins on duplicate resource.

### SRC-095 Helm value files and file parameters from referenced sources (`$ref`)
- **Upstream**: [reposerver/repository/repository.go#L840](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L840), [reposerver/repository/repository.go#L1551](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L1551), [util/argo/argo.go#L543](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L543), [docs/user-guide/multiple_sources.md#L46](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/multiple_sources.md#L46)
- **Maturity**: GA
- **Semantics**: A source with `ref: <key>` defines variable `$<key>` (must match `^[a-zA-Z0-9_-]+$`, unique). In another source's `helm.valueFiles[]` or `helm.fileParameters[].path`, an entry starting with `$<key>/` resolves relative to the **root** of the referenced repository (even if that source has a `path`), checked out at the ref source's target revision (or the rollback revision) using that repo's own depth; boundary checks apply to the referenced repo root; env substitution after ref expansion; globs expanded inside it. Every referenced repo is checked out (with OOB symlink check) and locked for the duration of rendering; its resolved commit SHA is part of the manifest cache key, so pushes to the values repo invalidate cached manifests.
- **Edge cases**: see SRC-096 for errors; `$ref` usable only at the beginning of the path; ref target that is a Helm chart is rejected; GetAppDetails also checks out ref repos (git-type only) to compute parameters.
- **Configuration surface**: `spec.sources[].ref`; `helm.valueFiles`, `helm.fileParameters`.
- **Exposure**: CRD; CLI `--ref`, `argocd app unset --ref`; UI REF field ([application-parameters.tsx#L692](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-parameters/application-parameters.tsx#L692)).
- **Tests required**:
  - positive: chart repo source + git ref values file (upstream `TestHelmChartReferencingExternalValues`).
  - positive: push to values repo changes cached manifests (cache key) without chart change.
  - negative: `$values/../../etc/passwd` rejected; out-of-bounds symlink in ref repo rejected.
  - positive: ref repo depth honored (`..._RefSourceDepthIsHonored`).

### SRC-096 Ref-source validation and consistency errors
- **Upstream**: [reposerver/repository/repository.go#L566](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L566), [reposerver/repository/repository.go#L855](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L855), [util/argo/argo.go#L553](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L553), [docs/user-guide/multiple_sources.md#L84](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/multiple_sources.md#L84)
- **Maturity**: GA
- **Semantics**: Validation performed before/while rendering multi-source apps, with exact error texts: invalid key → `sources.ref <ref> cannot contain any special characters except '_' and '-'`; duplicate → ``invalid sources: multiple sources had the same `ref` key``; `$x/...` with no ref sources → `source referenced "$x", but no source has a 'ref' field defined`; unknown key → `source referenced "$x", which is not one of the available sources ($a, $b)`; ref source with chart → `source has a 'chart' field defined, but Helm charts are not yet not supported for 'ref' sources`; two refs to the same repo with different targetRevision → `cannot reference multiple revisions for the same repository ($b references "x" while $a references "y")`; ref to the app's own repo resolving to a different commit → `cannot reference a different revision of the same repository (...)`; ref repo resolution failure → `failed to get git client for repo <url>`.
- **Edge cases**: these checks only run when `hasMultipleSources`; single-source apps treat `$x/...` as an env-substituted path.
- **Configuration surface**: `spec.sources[]`.
- **Exposure**: CRD; errors surface as InvalidSpec/Comparison conditions.
- **Tests required**:
  - negative: each error message reproduced (upstream `TestHelmChartReferencingExternalValues_InvalidRefs`).
  - positive: same repo referenced at the same revision works.

---

## I. OCI artifact sources

### SRC-097 OCI artifact as application source (`oci://`)
- **Upstream**: [pkg/apis/application/v1alpha1/types.go#L337](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L337), [reposerver/repository/repository.go#L395](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L395), [util/oci/client.go#L130](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/oci/client.go#L130), [docs/user-guide/oci.md#L3](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/oci.md#L3)
- **Maturity**: GA
- **Semantics**: A source whose `repoURL` starts with `oci://` is an OCI source (takes precedence over `chart`). The artifact at the resolved digest is downloaded (oras) into a local tar cache keyed by digest, the single content layer extracted to a temp dir, OOB-symlink-checked, and `path` selects a subdirectory (use `.` for root). Normal tool detection/explicit type then applies to the extracted tree (Directory/Kustomize/Helm/Plugin). Credentials from repository/creds of `type: oci` (username/password via static registry credential; TLS cert; `insecureOCIForceHttp` → plain HTTP). For OCI sources the controller adds permitted OCI repos/creds to the Helm dependency candidates.
- **Edge cases**: `noCache` (hard refresh) deletes the cached image before extraction; OCI image with out-of-bounds symlink → `oci image contains out-of-bounds symlinks. file: <f>`; `revision` reported = digest.
- **Configuration surface**: `spec.source.repoURL: oci://...`, `path`, `targetRevision`; OCI repository secrets.
- **Exposure**: CRD; CLI `argocd app create --repo oci://...`; UI revision picker uses `ListOCITags`.
- **Tests required**:
  - positive: plain-manifests image (`path: .`) renders as Directory.
  - positive: image containing kustomization renders with Kustomize.
  - positive: OCI source never invokes git client (upstream `TestGenerateManifest_OCISourceSkipsGitClient`).

### SRC-098 OCI revision resolution (tag, digest, semver constraint)
- **Upstream**: [util/oci/client.go#L394](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/oci/client.go#L394), [util/oci/client.go#L431](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/oci/client.go#L431), [reposerver/repository/repository.go#L2944](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L2944)
- **Maturity**: GA
- **Semantics**: `targetRevision` is first resolved directly against the registry (tag or digest → manifest digest). If that fails and the revision is a semver constraint (not an exact version), all tags are listed (cached under `oci-tags|<repo>` for the revision-cache TTL unless no-cache; `_` converted to `+`), the max satisfying version chosen, and its digest resolved. Result is always a `sha256:` digest.
- **Edge cases**: non-constraint unresolvable tag → `failed to resolve revision "<r>": cannot get digest for revision <r>: ...`; no matching version → `no version for constraints: ...`; tag-list failure → `error fetching tags: ...`.
- **Configuration surface**: `spec.source.targetRevision`.
- **Exposure**: API ResolveRevision; status revision.
- **Tests required**:
  - positive: tag `1.16.1` → digest; `1.*` → highest 1.x digest.
  - negative: unknown tag → exact error.

### SRC-099 OCI layer media-type allowlist and layer constraints
- **Upstream**: [util/oci/client.go#L284](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/oci/client.go#L284), [util/oci/client.go#L526](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/oci/client.go#L526), [cmd/argocd-repo-server/commands/argocd_repo_server.go#L278](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-repo-server/commands/argocd_repo_server.go#L278), [docs/user-guide/oci.md#L92](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/oci.md#L92)
- **Maturity**: GA
- **Semantics**: Manifest may have at most 10 layers. Content layers = media types ending in `tar+gzip`, `tar.gzip` or `tar`; for Helm-chart artifacts (config media type `application/vnd.cncf.helm.config.v1+json`) only `application/vnd.cncf.helm.chart.content.v1.tar+gzip` layers count. Exactly one content layer is required, and its media type must be in the allowlist (default `application/vnd.oci.image.layer.v1.tar`, `application/vnd.oci.image.layer.v1.tar+gzip`, `application/vnd.cncf.helm.chart.content.v1.tar+gzip`). Non-content layers (attestations etc.) are ignored.
- **Edge cases**: >10 layers → `expected no more than 10 oci layers, got N`; disallowed type → `oci layer media type <t> is not in the list of allowed media types`; 0 or ≥2 content layers → `expected only a single oci content layer, got N`.
- **Configuration surface**: flag `--oci-layer-media-types`, env `ARGOCD_REPO_SERVER_OCI_LAYER_MEDIA_TYPES` (comma list), `reposerver.oci.layer.media.types`.
- **Exposure**: server config.
- **Tests required**:
  - negative: two tar.gz layers → error; custom media type rejected until allowlisted.
  - positive: image with extra attestation layer accepted.

### SRC-100 OCI extraction size limit
- **Upstream**: [util/oci/client.go#L626](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/oci/client.go#L626), [cmd/argocd-repo-server/commands/argocd_repo_server.go#L273](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-repo-server/commands/argocd_repo_server.go#L273)
- **Maturity**: GA
- **Semantics**: The content layer is decompressed (gzip or plain tar) with a byte limit of `--oci-manifest-max-extracted-size` (default `1G`); `--disable-oci-manifest-max-extracted-size` removes the limit. Extraction paths are secure-joined into the destination.
- **Edge cases**: over limit → `cannot extract contents of oci image with revision <d>: could not decompress layer: ...`.
- **Configuration surface**: env `ARGOCD_REPO_SERVER_OCI_MANIFEST_MAX_EXTRACTED_SIZE`, `ARGOCD_REPO_SERVER_DISABLE_OCI_MANIFEST_MAX_EXTRACTED_SIZE`; cmd-params `reposerver.oci.manifest.max.extracted.size`, `reposerver.disable.oci.manifest.max.extracted.size`.
- **Exposure**: server config.
- **Tests required**:
  - negative: oversized layer rejected; disabled flag accepts it.

### SRC-101 OCI Helm chart artifacts via `oci://` sources
- **Upstream**: [util/oci/client.go#L54](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/oci/client.go#L54), [util/oci/client.go#L649](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/oci/client.go#L649), [docs/user-guide/oci.md#L26](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/oci.md#L26)
- **Maturity**: GA
- **Semantics**: When the artifact is a Helm chart (helm config media type), its single top-level chart directory is flattened to the extraction root, so `path: .` points at `Chart.yaml` and the source renders as Helm (all `helm.*` fields apply). Chart dependencies may be resolved against OCI repos added by the controller.
- **Edge cases**: helm layer containing not exactly one directory → `expected 1 directory, found N`; `path` other than `.` will usually not find the chart.
- **Configuration surface**: `spec.source.repoURL: oci://<registry>/<chart>`, `path: .`, `helm.*`.
- **Exposure**: CRD.
- **Tests required**:
  - positive: `oci://registry-1.docker.io/bitnamicharts/nginx` @ `15.9.0` with `valuesObject` renders.

### SRC-102 OCI metadata (`GetOCIMetadata`)
- **Upstream**: [reposerver/repository/repository.go#L2835](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L2835), [server/application/application.proto#L400](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.proto#L400), [docs/user-guide/oci.md#L132](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/oci.md#L132)
- **Maturity**: GA
- **Semantics**: For a digest, fetch the image manifest and map annotations: `org.opencontainers.image.created`→`createdAt`, `.authors`→`authors`, `.documentation`→`docsUrl`, `.source`→`sourceUrl`, `.version`→`version`, `.description`→`description`. Cached under `ocimetadata|<repo>|<revision>` for repo-cache TTL.
- **Edge cases**: fetch failure → `failed to extract digest metadata for revision "<r>": ...` (metric incremented); `title`, `revision`, `url` annotations are not returned.
- **Configuration surface**: none.
- **Exposure**: API `GET /api/v1/applications/{name}/revisions/{revision}/ocimetadata`; UI revision tooltip/summary.
- **Tests required**:
  - positive: annotations mapped exactly (upstream `TestGetOCIMetadata`); second call served from cache.

### SRC-103 OCI tag listing (`ListOCITags`)
- **Upstream**: [reposerver/repository/repository.go#L206](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L206), [server/repository/repository.go#L241](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/repository/repository.go#L241), [server/repository/repository.proto#L157](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/repository/repository.proto#L157)
- **Maturity**: GA
- **Semantics**: Returns `Refs{tags}` of an OCI repository; reads the `oci-tags|<repo>` tag cache when present (revision-cache TTL) before listing the registry. RBAC: `repositories, get` on the repo.
- **Edge cases**: client creation failure → `error creating oci client: ...`.
- **Configuration surface**: none.
- **Exposure**: API `GET /api/v1/repositories/{repo}/oci-tags`; UI revision autocomplete for OCI sources.
- **Tests required**:
  - positive: tags listed with `_`→`+` conversion.
  - authorization: user without `repositories get` denied.

---

## J. Git revision resolution and checkout

### SRC-104 Git revision resolution (HEAD, branch, tag, SHA)
- **Upstream**: [util/git/client.go#L912](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/client.go#L912), [util/git/client.go#L942](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/client.go#L942), [util/git/git.go#L24](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/git.go#L24), [util/git/workaround.go#L89](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/workaround.go#L89), [reposerver/repository/repository.go#L2931](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L2931), [docs/user-guide/tracking_strategies.md#L31](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/tracking_strategies.md#L31)
- **Maturity**: GA
- **Semantics**: Resolution via remote ref listing (ls-remote equivalent, in-memory): a full 40-hex SHA is returned as-is without contacting the remote; empty revision = `HEAD`; semver handling (SRC-105/106) may rewrite the revision to a tag name; then refs are matched by short name (`main`, `v1.0`) or full name (`refs/heads/main`, `refs/tags/v1.0`, any `refs/...` e.g. `refs/pull/1/head`); symbolic refs (HEAD) are followed; annotated tags resolve to the peeled commit SHA; finally a ≥7-hex string is assumed to be a (truncated) commit SHA. Transient errors are retried per git client retry settings.
- **Edge cases**: unresolvable → `unable to resolve '<rev>' to a commit SHA`; branch and tag with same short name → first match in remote listing order wins (ambiguity documented; use fully-qualified refs); 64-hex (SHA-256 repo) strings are treated as truncated SHAs.
- **Configuration surface**: `spec.source.targetRevision`.
- **Exposure**: CRD; API ResolveRevision; CLI `--revision`; UI TARGET REVISION ([application-parameters.tsx#L678](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-parameters/application-parameters.tsx#L678)).
- **Tests required**:
  - positive: HEAD, branch, tag, annotated tag (peeled), full SHA, 7-char SHA, `refs/pull/N/head` (non-standard refs, upstream `TestCheckoutRevisionCanGetNonstandardRefs`).
  - negative: `doesnotexist` → exact error.

### SRC-105 Git semver constraints on tags
- **Upstream**: [util/git/client.go#L958](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/client.go#L958), [util/versions/tags.go#L49](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/versions/tags.go#L49), [docs/user-guide/tracking_strategies.md#L62](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/tracking_strategies.md#L62)
- **Maturity**: GA
- **Semantics**: If `targetRevision` is a valid semver constraint (and not an exact version), the highest tag (never branches) satisfying it is selected (Masterminds semantics; prereleases only with `-0`); the selected tag is then resolved to a commit. Exact versions (`v1.0.0`) are used as literal ref names. If the constraint matches no tag, the error is swallowed and the original string is resolved literally (normally failing with `unable to resolve ...`).
- **Edge cases**: tags that are not semver ignored; `1.2.*` vs tags `v1.2.3` (leading `v` accepted by semver parser).
- **Configuration surface**: `spec.source.targetRevision`.
- **Exposure**: CRD.
- **Tests required**:
  - positive: `>=1.0.0 <2.0.0` picks `v1.9.0` over `v2.0.0`.
  - positive: `*-0` includes prerelease tags.
  - negative: constraint with no match → `unable to resolve` error.

### SRC-106 `tagPrefix` for prefixed semver tags
- **Upstream**: [pkg/apis/application/v1alpha1/types.go#L223](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L223), [util/versions/tags.go#L16](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/versions/tags.go#L16), [reposerver/repository/repository.go#L364](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L364), [docs/user-guide/tracking_strategies.md#L68](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/tracking_strategies.md#L68)
- **Maturity**: GA
- **Semantics**: Git sources only. Tags are filtered to those starting with `tagPrefix`, the prefix is stripped before semver evaluation and re-added to the result (`app1/` + `v1.0.*` → `app1/v1.0.1`). An exact version with prefix resolves to `<prefix><version>`. Used by GenerateManifest and ResolveRevision.
- **Edge cases**: tagPrefix with a non-semver revision (e.g. `main`) → internal error `tagPrefix "<p>" requires targetRevision to be a semver constraint, but "<r>" is not a valid constraint`, which is swallowed so `main` resolves as a branch; ApplicationSet templates may template the prefix.
- **Configuration surface**: `spec.source.tagPrefix`.
- **Exposure**: CRD; CLI `--tag-prefix` ([cmd/util/app.go#L111](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/util/app.go#L111)).
- **Tests required**:
  - positive: table from docs (three rows) resolves exactly.
  - positive: `>=v1.0.0-0` with prefix matches `app1/v1.0.0-rc.1`.

### SRC-107 Git reference cache
- **Upstream**: [util/git/client.go#L813](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/client.go#L813), [reposerver/cache/cache.go#L220](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/cache/cache.go#L220), [reposerver/cache/cache.go#L69](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/cache/cache.go#L69)
- **Maturity**: GA
- **Semantics**: Remote ref listings are cached in Redis under `git-refs|<repoURL>` for `--revision-cache-expiration` (env `ARGOCD_RECONCILIATION_TIMEOUT`, default 3m). A distributed lock (`--revision-cache-lock-timeout`, env `ARGOCD_REVISION_CACHE_LOCK_TIMEOUT`, default 10s; 0 disables) ensures only one repo-server replica lists a repo at a time while others wait/read. Bypassed when `noRevisionCache` or `noCache` is set.
- **Edge cases**: failed listing releases the lock; cache errors propagate if lock owner missing (see upstream `TestGetRefs_*`).
- **Configuration surface**: flags `--revision-cache-expiration`, `--revision-cache-lock-timeout`.
- **Exposure**: server config.
- **Tests required**:
  - positive: two concurrent resolutions → one remote listing.
  - positive: noRevisionCache forces fresh listing.

### SRC-108 Checkout and fetch strategy
- **Upstream**: [reposerver/repository/repository.go#L3084](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L3084), [reposerver/repository/repository.go#L3042](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L3042), [util/git/client.go#L772](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/client.go#L772)
- **Maturity**: GA
- **Semantics**: Per repo URL one persistent working copy (randomized path). `git init` if needed; if the commit is not present: with `depth>0` fetch that revision at depth, else fetch default refspecs (no revision, avoids bloat); `git checkout --force <rev>` (`HEAD`/empty → `origin/HEAD`); on checkout failure fetch the revision explicitly and check out `FETCH_HEAD`; LFS checkout if repo LFS-enabled and LFS files exist; `git clean -ffdx` when a clean is requested (revision changed or previous op exclusive) or submodules enabled.
- **Edge cases**: errors are gRPC Internal: `Failed to initialize git repo: ...`, `Failed to fetch default: ...`, `Failed to checkout revision <r>: ...`, `Failed to checkout FETCH_HEAD: ...`; skip fetch when revision already present.
- **Configuration surface**: repository `depth`, `enableLfs`.
- **Exposure**: implicit.
- **Tests required**:
  - positive: present revision → no fetch (`TestCheckoutRevisionPresentSkipFetch`).
  - positive: shallow depth fetch of specific SHA (`TestFetchWithDepth`).
  - positive: untracked files from previous revision removed after revision change.

### SRC-109 Git submodules
- **Upstream**: [util/git/client.go#L763](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/client.go#L763), [util/git/client.go#L793](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/client.go#L793), [cmd/argocd-repo-server/commands/argocd_repo_server.go#L52](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-repo-server/commands/argocd_repo_server.go#L52), [docs/operator-manual/argocd-cmd-params-cm.yaml#L291](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cmd-params-cm.yaml#L291)
- **Maturity**: GA
- **Semantics**: When enabled (default `true`) and `.gitmodules` exists after checkout: `git submodule sync --recursive` then `git submodule update --init --recursive` using the repo's credentials, followed by `git clean -ffdx`. Applies to manifest generation, ListApps, app details, revision metadata. GetGitFiles/GetGitDirectories use the per-request `submoduleEnabled` flag; UpdateRevisionForPaths checkouts never update submodules.
- **Edge cases**: submodule failure → `failed to update submodules: ...`; submodules requiring different credentials fail.
- **Configuration surface**: env `ARGOCD_GIT_MODULES_ENABLED`, cmd-params `reposerver.enable.git.submodule`.
- **Exposure**: server config.
- **Tests required**:
  - positive: manifests inside submodule rendered when enabled; absent when disabled.

---

## K. Caching

### SRC-110 Manifest cache
- **Upstream**: [reposerver/cache/cache.go#L343](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/cache/cache.go#L343), [reposerver/cache/cache.go#L371](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/cache/cache.go#L371), [reposerver/cache/cache.go#L128](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/cache/cache.go#L128), [reposerver/cache/cache.go#L392](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/cache/cache.go#L392), [reposerver/repository/repository.go#L376](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L376), [docs/operator-manual/high_availability.md#L41](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/high_availability.md#L41)
- **Maturity**: GA
- **Semantics**: GenerateManifest results are cached in Redis under `mfst|<trackingKey>|<appName>|<resolvedRevision>|<destNamespace>|<FNV(appSource+refSources+refCommitSHAs) + FNV(kubeVersion|sorted apiVersions)>|<sourceIntegrityKey>[|<installationID>]`, where trackingKey = appLabelKey, prefixed with `<method>:` when tracking method ≠ label, and for non-Helm sources `repoURL`/`targetRevision` are blanked (commit SHA supersedes). Lookup happens before taking locks and, for git sources, again after checkout (double-checked). Entries carry an integrity hash; mismatch or empty entry → treated as miss and deleted. On hit, the returned `revision` is rewritten to the key revision. TTL `--repo-cache-expiration` (env `ARGOCD_REPO_CACHE_EXPIRATION`, default 24h). `.argocd-source` overrides are not part of the key.
- **Edge cases**: changing cluster version/API versions, namespace, tracking method, label key, installation ID, source integrity policy or any source field → new key; Kustomize remote bases / mutable Helm chart versions are not detected until TTL or hard refresh.
- **Configuration surface**: flags `--repo-cache-expiration`, Redis settings; cmd-params `reposerver.repo.cache.expiration`.
- **Exposure**: implicit; hard refresh via API/CLI/UI (SRC-112).
- **Tests required**:
  - positive: identical requests → second served from cache (no tool exec).
  - positive: different API versions → regeneration (`TestGenerateManifests_K8SAPIResetCache`).
  - positive: corrupted cache entry (hash mismatch) regenerates.

### SRC-111 Manifest-generation error caching (pause after failures)
- **Upstream**: [reposerver/repository/repository.go#L952](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L952), [reposerver/repository/repository.go#L1017](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L1017), [cmd/argocd-repo-server/commands/argocd_repo_server.go#L49](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-repo-server/commands/argocd_repo_server.go#L49), [common/common.go#L284](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L284)
- **Maturity**: GA
- **Semantics**: When `ARGOCD_PAUSE_GEN_AFTER_FAILED_ATTEMPTS` > 0 (default 3), each failed generation increments a consecutive-failure counter (with first-failure timestamp and last error) in the manifest cache entry. Once failures ≥ threshold, subsequent requests return the cached error `Manifest generation error (cached): <last error>` without running tools, until either `ARGOCD_PAUSE_GEN_MINUTES` (default 60) elapsed since first failure, or `ARGOCD_PAUSE_GEN_REQUESTS` (default 0 = disabled) cached responses were returned; then the entry is deleted and generation retried. A success resets counters. `noCache` (hard refresh) bypasses the error cache.
- **Edge cases**: threshold 0 disables error caching; while the failure count is below the threshold every request regenerates (the failure entry is not served); the reset-by-requests check only applies once at least one cached error has been served.
- **Configuration surface**: env `ARGOCD_PAUSE_GEN_AFTER_FAILED_ATTEMPTS`, `ARGOCD_PAUSE_GEN_MINUTES`, `ARGOCD_PAUSE_GEN_REQUESTS`.
- **Exposure**: server config.
- **Tests required**:
  - positive: after 3 failures 4th returns cached error prefix; fixing repo content at same revision still returns cached error until reset (`TestManifestGenErrorCacheFileContentsChange`).
  - positive: reset by minutes and by request count (`TestManifestGenErrorCacheByMinutesElapsed`, `...ByNumRequests`).
  - positive: noCache bypass (`TestManifestGenErrorCacheRespectsNoCache`).

### SRC-112 Cache bypass flags (`noCache`, `noRevisionCache`)
- **Upstream**: [reposerver/repository/repository.proto#L14](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.proto#L14), [reposerver/repository/repository.go#L354](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L354), [reposerver/repository/repository.go#L436](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L436)
- **Maturity**: GA
- **Semantics**: `noCache` (hard refresh): skip manifest cache read, clear cached Helm chart tarball / OCI image for the revision, bypass symlink-check memo, and imply `noRevisionCache`. `noRevisionCache`: bypass git ref cache / Helm index / OCI tag cache when resolving revisions. Spec validation always sets `noRevisionCache`.
- **Edge cases**: results are still written to cache after a no-cache generation.
- **Configuration surface**: request flags (from app refresh `hard`).
- **Exposure**: API (`argocd.argoproj.io/refresh: hard` annotation; `GET /api/v1/applications/{name}?refresh=hard`); CLI `argocd app get --hard-refresh`, `argocd app diff --hard-refresh`; UI Hard Refresh.
- **Tests required**:
  - positive: hard refresh re-runs tool even with cache hit and re-downloads chart.

### SRC-113 Other repo-server caches (app details, metadata, listings)
- **Upstream**: [reposerver/cache/cache.go#L456](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/cache/cache.go#L456), [reposerver/cache/cache.go#L158](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/cache/cache.go#L158), [reposerver/cache/cache.go#L478](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/cache/cache.go#L478)
- **Maturity**: GA
- **Semantics**: Keys (TTL = repo-cache expiration unless noted): `appdetails|<rev>|<hash(source,refs)>|<trackingMethod>`; `ldir|<repo>|<sha>` (ListApps); `revisionmetadata|<repo>|<rev>`; `chartdetails|<repo>|<chart>|<rev>`; `ocimetadata|<repo>|<rev>`; `gitfiles|<repo>|<sha>|<pattern>`; `gitdirs|<repo>|<sha>`; `gitFilesChanges|<repo>|<rev>|<syncedRev>`; `helm-index|<repo>` and `oci-tags|<repo>`, `git-refs|<repo>` (revision-cache TTL). Cache write failures are logged, never fatal.
- **Edge cases**: app details cache ignores refSource resolved SHAs (passed nil).
- **Configuration surface**: `--repo-cache-expiration`, `--revision-cache-expiration`, `--default-cache-expiration`.
- **Exposure**: implicit.
- **Tests required**:
  - positive: repeated GetAppDetails served from cache (`TestGetAppDetailsHelmUsesCache`).

---

## L. Limits, concurrency and security

### SRC-114 Manifest generation parallelism limit
- **Upstream**: [reposerver/repository/repository.go#L137](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L137), [reposerver/repository/repository.go#L385](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L385), [cmd/argocd-repo-server/commands/argocd_repo_server.go#L257](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-repo-server/commands/argocd_repo_server.go#L257), [docs/operator-manual/high_availability.md#L22](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/high_availability.md#L22)
- **Maturity**: GA
- **Semantics**: A weighted semaphore of size `--parallelismlimit` (env `ARGOCD_REPO_SERVER_PARALLELISM_LIMIT`, cmd-params `reposerver.parallelism.limit`; default 0 = unlimited) bounds concurrent GenerateManifest operations after cache miss (acquired after revision resolution and cache check, before checkout). Wait time is observed as a metric. Not applied to GetAppDetails, ListApps, streamed generation.
- **Edge cases**: context cancellation while waiting returns the ctx error.
- **Configuration surface**: as above.
- **Exposure**: server config.
- **Tests required**:
  - positive: limit 1 → second generation waits until first completes; cache hits are not blocked.

### SRC-115 Config-management tool exec timeout
- **Upstream**: [util/exec/exec.go#L44](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/exec/exec.go#L44), [util/exec/exec.go#L155](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/exec/exec.go#L155), [util/exec/exec.go#L260](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/exec/exec.go#L260), [docs/operator-manual/high_availability.md#L48](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/high_availability.md#L48)
- **Maturity**: GA
- **Semantics**: Every helm/kustomize (and helm registry/repo/pull, tar) subprocess run by the repo-server is limited by `ARGOCD_EXEC_TIMEOUT` (Go duration, default `90s`; unparsable → default). On timeout the process receives SIGKILL immediately (default timeout behaviour; `ARGOCD_EXEC_FATAL_TIMEOUT`, default 10s, only matters for callers that request SIGTERM-and-wait) and the error is `` `<cmd>` failed timeout after 1m30s ``. Effective upper bound is also the caller's RPC deadline (default 60s).
- **Edge cases**: jsonnet runs in-process (no exec timeout); CMP commands use SRC-088 instead.
- **Configuration surface**: env `ARGOCD_EXEC_TIMEOUT`, `ARGOCD_EXEC_FATAL_TIMEOUT` (repo-server).
- **Exposure**: server config.
- **Tests required**:
  - negative: helm post-render/slow template exceeding 5s with `ARGOCD_EXEC_TIMEOUT=5s` → exact timeout error.

### SRC-116 Repository locking and concurrent processing
- **Upstream**: [reposerver/repository/lock.go#L25](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/lock.go#L25), [pkg/apis/application/v1alpha1/types.go#L329](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L329), [pkg/apis/application/v1alpha1/types.go#L840](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L840), [docs/operator-manual/high_availability.md#L27](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/high_availability.md#L27)
- **Maturity**: GA
- **Semantics**: One working copy per repo; a per-path lock allows multiple concurrent operations only if they target the same revision and all allow concurrency. A source disallows concurrency if Kustomize options mutate the kustomization (`images`, `commonLabels`, `commonAnnotations`, `namePrefix`, `nameSuffix`, `namespace`, `patches`, `components`; not `replicas`). Operations on a different revision wait for in-flight ones to finish. The checkout is cleaned (`git clean -ffdx`, `checkout --force`) when the previous holder was exclusive or the revision changed. Lock is released early for CMP once the tarball is built. Helm `dependency build` is additionally serialized per chart path.
- **Edge cases**: many apps with Kustomize overrides in a monorepo serialize (documented limitation); init failure → `failed to initialize repository resources: ...`.
- **Configuration surface**: none.
- **Exposure**: implicit.
- **Tests required**:
  - positive: two kustomize apps with images on same repo/revision never run edits concurrently; mutations from one never leak into the other's output.
  - positive: plain apps on same revision run concurrently.

### SRC-117 Out-of-bounds symlink protection
- **Upstream**: [util/app/path/path.go#L50](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/app/path/path.go#L50), [reposerver/repository/repository.go#L479](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L479), [reposerver/repository/repository.go#L614](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L614), [cmd/argocd-repo-server/commands/argocd_repo_server.go#L268](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-repo-server/commands/argocd_repo_server.go#L268), [docs/operator-manual/argocd-cmd-params-cm.yaml#L276](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cmd-params-cm.yaml#L276)
- **Maturity**: GA
- **Semantics**: Unless `--allow-oob-symlinks`, after obtaining content (git checkout excluding `.git`, extracted Helm chart, extracted OCI image, referenced `$ref` repos, streamed local files) the whole tree is scanned: any absolute symlink, or relative symlink whose path components walk outside the root at any step, fails the operation. Results are memoized in-process for 12h per (root, revision, skip paths), bypassed on noCache. Even with the flag, value-file/jsonnet/directory symlink boundary checks (SRC-012/018/022) still apply.
- **Edge cases**: messages `repository contains out-of-bounds symlinks. file: <rel>`, `chart contains ...`, `oci image contains ...`, `streamed files contains ...`; dangling in-bounds symlinks are allowed; `.git` internals are skipped; upstream memoizes referenced-repo checks under the *primary* source revision.
- **Configuration surface**: flag `--allow-oob-symlinks`, env `ARGOCD_REPO_SERVER_ALLOW_OUT_OF_BOUNDS_SYMLINKS`, cmd-params `reposerver.allow.oob.symlinks`.
- **Exposure**: server config.
- **Tests required**:
  - negative: repo with `link -> /etc/passwd` and `link -> ../../x` rejected (`Test_GenerateManifests_NoOutOfBoundsAccess`, `TestChartRepoWithOutOfBoundsSymlink`).
  - positive: flag enabled → directory app proceeds, but value file via OOB link still rejected.

### SRC-118 Workspace isolation and error path sanitization
- **Upstream**: [reposerver/repository/repository.go#L170](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L170), [reposerver/repository/repository.go#L3000](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L3000), [reposerver/repository/repository.go#L345](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L345), [reposerver/repository/repository.go#L550](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L550), [util/io/path/resolved.go#L71](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/io/path/resolved.go#L71)
- **Maturity**: GA
- **Semantics**: Repos, charts and OCI images live under the root dir (`$TMPDIR/_argocd-repo`, see [reposerver/server.go#L125](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/server.go#L125)) at UUID-randomized paths. Root dir mode is `0300` (no listing); each repo dir is `chmod 0700` while in use and `0000` when idle. On startup existing clones are re-associated with their remote URL (`Init`). Errors returned to callers have `<root>/<random>` replaced with `<path to cached source>`; path-resolution internals return a generic `internal error: failed to resolve path. Check logs for more details`.
- **Edge cases**: root dir unreadable on restart → warning, clones not restored (fresh clones).
- **Configuration surface**: `TMPDIR`.
- **Exposure**: implicit.
- **Tests required**:
  - positive: error message for failing helm chart contains `<path to cached source>`, never `/tmp/_argocd-repo/<uuid>`.
  - positive: idle repo dir has mode 0000 (`TestDirectoryPermissionInitializer`).
  - recovery: restart reuses existing clone without re-cloning (`TestInit`).

---

## M. Local manifests (developer workflow)

### SRC-119 Server-side generation from uploaded files (`GenerateManifestWithFiles`)
- **Upstream**: [reposerver/repository/repository.go#L709](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L709), [util/manifeststream/stream.go#L164](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/manifeststream/stream.go#L164), [server/application/application.go#L652](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L652), [server/application/application.proto#L410](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.proto#L410), [cmd/argocd/commands/app_diff.go#L797](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/app_diff.go#L797)
- **Maturity**: GA
- **Semantics**: CLI tars+gzips a local directory (only files whose *basename* matches `--local-include` globs, default `*.yaml`, `*.yml`, `*.json`), streams `{query{name, appNamespace, project, checksum}}` then chunks to API `GetManifestsWithFiles` (RBAC `applications, get`), which builds a ManifestRequest from the stored app (first source only, cluster version/API resources, settings) and streams `{request}`, `{metadata{checksum,size}}`, chunks to repo-server `GenerateManifestWithFiles`. Repo-server receives with `--streamed-manifest-max-tar-size` (default 100M) and extracts with `--streamed-manifest-max-extracted-size` (default 1G), verifies SHA-256, OOB-symlink-checks, then renders with all tools incl. CMP (revision string `streamed`, cache not used). The API server redacts Secret data in the response.
- **Edge cases**: missing app name → `invalid request: application name is missing`; oversize tar → `file exceeded max size of N bytes`; checksum mismatch → `file checksum validation error: calc X sent Y`; no files → `no files to send`; `--server-side-diff` with `--local` requires `--server-side-generate`.
- **Configuration surface**: repo-server flags `--streamed-manifest-max-tar-size`, `--streamed-manifest-max-extracted-size` (env `ARGOCD_REPO_SERVER_STREAMED_MANIFEST_MAX_TAR_SIZE`, `..._MAX_EXTRACTED_SIZE`).
- **Exposure**: API (gRPC client-stream `ApplicationService.GetManifestsWithFiles`, REST mapping `POST /api/v1/applications/manifestsWithFiles`); CLI `argocd app diff --local <dir> --server-side-generate [--local-include ...]`.
- **Tests required**:
  - positive: local Helm/CMP app diff via server-side generate.
  - negative: tampered checksum, oversize tar, OOB symlink in uploaded dir.
  - authorization: user without `applications get` denied.

### SRC-120 Client-side local generation (`--local` without server-side generate)
- **Upstream**: [cmd/argocd/commands/app_diff.go#L153](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/app_diff.go#L153), [cmd/argocd/commands/app.go#L2952](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/app.go#L2952), [docs/user-guide/application_sources.md#L13](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/application_sources.md#L13)
- **Maturity**: Deprecated (code comments; warning `local diff without --server-side-generate is deprecated and does not work with plugins`)
- **Semantics**: The CLI itself runs the repo-server generation library against `--local` dir with `--local-repo-root` (default `/` for diff/sync, `.` for manifests), using settings from the API (label key, kustomize options, tracking method) and cluster info, with no size limit, no CMP support, helm run with the user's helm home (isLocal). Secrets are excluded from local diffs.
- **Edge cases**: requires helm/kustomize binaries on the client; `.argocd-source` overrides applied locally.
- **Configuration surface**: CLI flags `--local`, `--local-repo-root`.
- **Exposure**: CLI `argocd app manifests --local`, `argocd app diff --local`, `argocd app sync --local` (sync area).
- **Tests required**:
  - positive: `argocd app manifests --local ./guestbook` prints rendered objects.
  - negative: plugin app with client-side local → error.

---

## N. Repo-server RPCs and annotation semantics

### SRC-121 `GenerateManifest` RPC contract and post-processing
- **Upstream**: [reposerver/repository/repository.proto#L10](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.proto#L10), [reposerver/repository/repository.proto#L95](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.proto#L95), [reposerver/repository/repository.go#L632](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L632), [reposerver/repository/repository.go#L1715](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L1715), [reposerver/repository/repository.go#L1806](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L1806), [server/application/application.proto#L405](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.proto#L405)
- **Maturity**: GA
- **Semantics**: Request fields: repo, revision, noCache, appLabelKey, appName, namespace, applicationSource, repos (Helm/OCI repos for deps), kustomizeOptions, kubeVersion, apiVersions, helmRepoCreds, noRevisionCache, trackingMethod, enabledSourceTypes, helmOptions, hasMultipleSources, refSources, projectSourceRepos, projectName, annotationManifestGeneratePaths, installationID, sourceIntegrity (deprecated `verifySignature`, `plugins` ignored). Flow: resolve revision (OCI digest / Helm version / git SHA with `tagPrefix`) → resolve ref-source SHAs → cache check → semaphore → obtain content → re-check cache → verify source integrity (git) → render. Response: `manifests[]` (JSON strings), `revision` (resolved SHA/version/digest), `sourceType`, `commands[]` (helm/kustomize command lines), `sourceIntegrityResult` (and deprecated `verifyResult`). Before serialization every non-CRD object gets the app tracking label/annotation per `trackingMethod`/`appLabelKey`/`installationID` (tracking area) when both appLabelKey and appName are set. GlobNoMatch errors map to gRPC `NotFound`.
- **Edge cases**: tracking injection failure → `failed to set app instance tracking info on manifest: ...`; CRDs never receive tracking metadata here.
- **Configuration surface**: n/a (internal API between controller/API server and repo-server; gRPC port 8081, TLS/mTLS per repositories/ops area).
- **Exposure**: API internal gRPC `repository.RepoServerService/GenerateManifest`; public REST `GET /api/v1/applications/{name}/manifests` (`?revision=`, multi-source `revisions`/`sourcePositions`); CLI `argocd app manifests [--revision] [--source-positions --revisions]`; UI Manifest tab / diff.
- **Tests required**:
  - positive: response fields populated for git, helm-chart, OCI sources.
  - positive: tracking label added to Deployment but not CRD.
  - authorization (public API): `applications get` required for `/manifests`.

### SRC-122 `GetAppDetails` — Helm
- **Upstream**: [reposerver/repository/repository.go#L2472](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L2472), [reposerver/repository/repository.go#L2532](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L2532), [reposerver/repository/repository.go#L2663](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L2663), [util/helm/helm.go#L147](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/helm.go#L147), [util/helm/helm.go#L195](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/helm.go#L195)
- **Maturity**: GA
- **Semantics**: Returns `type` plus `helm{valueFiles, values, parameters, fileParameters}`: `valueFiles` = every file under the app path (recursive) whose basename contains `values` and extension `.yaml`/`.yml` (case-insensitive), relative paths; `values` = raw chart-root `values.yaml` (only if inside repo bounds); `parameters` = flattened (`a.b`, `list[0].c`) string map from `helm show values .` merged with the spec's selected valueFiles (resolved like SRC-022 but with **no** env substitution; remote http/https fetched directly; missing/permission-denied local files skipped); inline `values`/`valuesObject`/`parameters` are not merged; `fileParameters` = spec fileParameters verbatim. For multi-source, git ref repos referenced by selected value files are checked out (same-repo/multi-revision consistency errors as SRC-096).
- **Edge cases**: unparsable values → `failed to parse values: ...`; disallowed value file path → `failed to resolve value files: ...`; app details do not run dependency build.
- **Configuration surface**: request `source`, `refSources`, `helmOptions`.
- **Exposure**: API `POST /api/v1/repositories/{source.repoURL}/appdetails` (RepositoryService.GetAppDetails); UI Parameters tab/create panel/deployment history ([application-parameters.tsx#L1108](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-parameters/application-parameters.tsx#L1108)).
- **Tests required**:
  - positive: `TestGetAppDetailsHelm` parity (value files list, parameters flattening incl. arrays).
  - positive: chart without values.yaml (`TestGetAppDetailsHelm_WithNoValuesFile`).
  - positive: `$ref` value files in app details (`Test_populateHelmAppDetailsWithRef`).

### SRC-123 `GetAppDetails` — Kustomize
- **Upstream**: [reposerver/repository/repository.go#L2683](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L2683), [util/kustomize/kustomize.go#L518](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/kustomize/kustomize.go#L518)
- **Maturity**: GA
- **Semantics**: Runs a full Kustomize build (including the source's edit operations, selected version, build options, but no `--helm-kube-version` flags and with empty namespace in env) and returns `kustomize.images` = sorted list of `image` values of every `containers`/`initContainers` entry found anywhere in rendered objects (duplicates kept).
- **Edge cases**: build failure returned as error; images list may contain duplicates.
- **Configuration surface**: request `kustomizeOptions`.
- **Exposure**: API appdetails; UI IMAGES overrides.
- **Tests required**:
  - positive: images extracted (`TestGetAppDetailsKustomize`), including initContainers and CronJob nested templates.

### SRC-124 `GetAppDetails` — Plugin parameter announcements
- **Upstream**: [reposerver/repository/repository.go#L2705](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L2705), [cmpserver/plugin/plugin.go#L382](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmpserver/plugin/plugin.go#L382)
- **Maturity**: GA
- **Semantics**: Detects the plugin (by name or discovery) with reduced env (SRC-078), streams the full repo, and returns `plugin.parametersAnnouncement` from SRC-081/082. Directory type returns only `type`.
- **Edge cases**: detection failure → `failed to populate plugin app details: failed to detect CMP for app: ...`.
- **Configuration surface**: plugin.yaml parameters.
- **Exposure**: API appdetails; UI plugin parameters form ([application-parameters.tsx#L907](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-parameters/application-parameters.tsx#L907)).
- **Tests required**:
  - positive: static+dynamic announcements returned for CMP app.

### SRC-125 `GetAppDetails` API authorization and caching
- **Upstream**: [server/repository/repository.go#L333](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/repository/repository.go#L333), [server/repository/repository.proto#L167](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/repository/repository.proto#L167), [reposerver/repository/repository.go#L2515](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L2515)
- **Maturity**: GA
- **Semantics**: API requires `repositories, get` on the repo and `applications, get` on `<project>/<appName>`; for a non-existent app additionally `applications, create`; for an existing app the requested project must equal the app's and the requested source must be in the app's current spec or history (`sourceIndex`, `versionId`), else PermissionDenied; repo must be permitted in the project. Repo-server results cached under `appdetails|...` (SRC-113), honoring noCache/noRevisionCache. Not subject to the parallelism limit.
- **Edge cases**: missing `source` → InvalidArgument `missing payload in request`.
- **Configuration surface**: RBAC policies.
- **Exposure**: API/UI.
- **Tests required**:
  - authorization: user with app get but arbitrary (non-history) source → denied.
  - authorization: create-only user can query for new app name.
  - negative: repo not in project sourceRepos → denied.

### SRC-126 `ListRefs` (branches and tags)
- **Upstream**: [reposerver/repository/repository.go#L228](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L228), [util/git/client.go#L879](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/client.go#L879), [server/repository/repository.go#L262](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/repository/repository.go#L262), [server/repository/repository.proto#L153](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/repository/repository.proto#L153)
- **Maturity**: GA
- **Semantics**: Lists remote refs (git-refs cache honored) and returns short names of `refs/heads/*` as `branches` and `refs/tags/*` as `tags`, each sorted lexicographically; other refs omitted.
- **Edge cases**: client error → `error creating git client: ...`; empty repo → empty arrays.
- **Configuration surface**: none.
- **Exposure**: API `GET /api/v1/repositories/{repo}/refs` (RBAC `repositories get`); UI revision autocomplete.
- **Tests required**:
  - positive: sorted branches/tags.
  - authorization: denied without repositories get.

### SRC-127 `ListApps` (application discovery in a repo)
- **Upstream**: [reposerver/repository/repository.go#L251](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L251), [util/app/discovery/discovery.go#L37](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/app/discovery/discovery.go#L37), [server/repository/repository.go#L285](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/repository/repository.go#L285)
- **Maturity**: GA
- **Semantics**: Resolves revision, checks out (shared lock), and walks the whole repo: returns map `<dir relative to repo root> → "Helm" | "Kustomize"` (Kustomize wins for same dir); if any CMP with discovery matches at repo root, returns only `{".": "Plugin"}`. Directory-type paths are not listed. Cached by `ldir|<repo>|<sha>`. API returns `items[{path, type}]` (unordered).
- **Edge cases**: API requires `repositories get` **and** (`applications create` or `applications update`) on `<appProject>/<appName>`, and repo permitted in project; API does not forward enabled source types (disabled tools still listed).
- **Configuration surface**: none.
- **Exposure**: API `GET /api/v1/repositories/{repo}/apps?revision=&appName=&appProject=`; UI create panel path autocomplete.
- **Tests required**:
  - positive: repo with helm + kustomize dirs listed (`TestListApps`).
  - authorization: user without app create/update denied.

### SRC-128 `GetRevisionMetadata` (git commit metadata)
- **Upstream**: [reposerver/repository/repository.go#L2752](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L2752), [server/application/application.proto#L390](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.proto#L390)
- **Maturity**: GA
- **Semantics**: Input must be a full or ≥7-char hex SHA. Returns `author` (`Name <email>`), `date`, `tags` (tags pointing at the commit), `message` (body with valid `Argocd-reference-commit-*` trailers removed), `references[]` (related commits parsed from those trailers: author, date, subject, body, sha, repoURL), `sourceIntegrityResult` (and deprecated `signatureInfo`). Cached under `revisionmetadata|...`, but the cache is bypassed when the request carries source-integrity criteria or the cached entry contains integrity/signature info.
- **Edge cases**: unresolved revision → `revision <r> must be resolved`; checkout failure → `error acquiring repo lock: ...`.
- **Configuration surface**: project source-integrity policy (other area).
- **Exposure**: API `GET /api/v1/applications/{name}/revisions/{revision}/metadata` (RBAC `applications get`); UI revision tooltips/history.
- **Tests required**:
  - positive: metadata fields for a known commit (`TestGetRevisionMetadata`).
  - negative: `main` → must-be-resolved error.

### SRC-129 `GetRevisionChartDetails` (Helm chart metadata)
- **Upstream**: [reposerver/repository/repository.go#L2874](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L2874), [reposerver/repository/chart.go#L12](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/chart.go#L12), [server/application/application.proto#L395](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.proto#L395)
- **Maturity**: GA
- **Semantics**: Resolves the chart version (no revision cache), extracts the chart, runs `helm show chart .` and returns `description`, `home`, `maintainers[]` (`"Name <email>"` or `Name` when no email). Cached under `chartdetails|<repo>|<chart>|<rev>`.
- **Edge cases**: errors prefixed `helm client error:`, `error extracting chart:`, `error inspecting chart:`, `error getting chart details:`.
- **Configuration surface**: none.
- **Exposure**: API `GET /api/v1/applications/{name}/revisions/{revision}/chartdetails`; UI history/summary.
- **Tests required**:
  - positive: maintainers formatting (`TestGetRevisionChartDetails`).

### SRC-130 `GetHelmCharts` (chart listing)
- **Upstream**: [reposerver/repository/repository.go#L3130](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L3130), [server/repository/repository.go#L412](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/repository/repository.go#L412), [server/repository/repository.proto#L175](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/repository/repository.proto#L175)
- **Maturity**: GA
- **Semantics**: Fetches the repo `index.yaml` bypassing the index cache (but refreshing it) and returns `items[{name, versions[]}]` for every chart (versions in index order; map iteration order of charts is unspecified). Not supported for OCI registries (no index).
- **Edge cases**: index errors propagated.
- **Configuration surface**: none.
- **Exposure**: API `GET /api/v1/repositories/{repo}/helmcharts` (RBAC `repositories get`); UI chart dropdown.
- **Tests required**:
  - positive: charts and versions listed (`TestGetHelmCharts`).

### SRC-131 `ResolveRevision`
- **Upstream**: [reposerver/repository/repository.go#L3193](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L3193), [reposerver/repository/repository.proto#L80](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.proto#L80)
- **Maturity**: GA
- **Semantics**: Given app + sourceIndex (−1 = hydrator dry source) and an ambiguous revision, resolves per source kind: OCI → digest (no cache), Helm → version (no cache), git → SHA (with `tagPrefix`, revision cache unless `noRevisionCache`). Returns `revision` and `ambiguousRevision` formatted `"<input> (<resolved>)"`. Used by the controller (hydrator) and sync operations.
- **Edge cases**: failure returns empty fields plus error.
- **Configuration surface**: none.
- **Exposure**: internal gRPC.
- **Tests required**:
  - positive/negative per kind (`TestResolveRevision`, `TestResolveRevisionNegativeScenarios`).

### SRC-132 `GetGitFiles` (file contents by glob, for ApplicationSet)
- **Upstream**: [reposerver/repository/repository.go#L3230](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L3230), [util/git/client.go#L703](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/client.go#L703), [reposerver/repository/repository.proto#L248](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.proto#L248)
- **Maturity**: GA
- **Semantics**: Resolve revision (cache unless noRevisionCache), return cached `gitfiles|<repo>|<sha>|<pattern>` if present, else checkout (submodules per request flag), verify source integrity (fails if result is an error), list files by `path` pattern (default `.`): legacy mode `git ls-files --full-name -z -- <pattern>` (git pathspec glob, tracked files only); new globbing mode (`NewGitFileGlobbingEnabled`) doublestar glob over the working tree with symlinks evaluated and matches outside the repo ignored. Returns map path → bytes.
- **Edge cases**: nil repo → InvalidArgument `must pass a valid repo`; errors gRPC Internal: `unable to resolve git revision ...`, `unable to checkout git repo ...`, `unable to list files ...`, `unable to read files ...`.
- **Configuration surface**: ApplicationSet controller flag for new globbing (appset area).
- **Exposure**: internal gRPC (ApplicationSet git files generator).
- **Tests required**:
  - positive: both globbing modes incl. `**` (new mode) and symlink outside repo ignored.
  - negative: nil repo error (`TestErrorGetGitFiles`).

### SRC-133 `GetGitDirectories` (directory listing, for ApplicationSet)
- **Upstream**: [reposerver/repository/repository.go#L3308](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L3308), [cmd/argocd-repo-server/commands/argocd_repo_server.go#L276](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-repo-server/commands/argocd_repo_server.go#L276)
- **Maturity**: GA
- **Semantics**: Like SRC-132 but returns all directory paths (relative, excluding `.`) in the checkout; directories whose name starts with `.` (including `.git`) and their subtrees are skipped unless `--include-hidden-directories`. Cached under `gitdirs|<repo>|<sha>`.
- **Edge cases**: walk error → `error walking the file tree: ...`.
- **Configuration surface**: flag `--include-hidden-directories`, env `ARGOCD_REPO_SERVER_INCLUDE_HIDDEN_DIRECTORIES`, cmd-params `reposerver.include.hidden.directories`.
- **Exposure**: internal gRPC (ApplicationSet git directory generator).
- **Tests required**:
  - positive: hidden dirs excluded by default, included with flag (`TestGetGitDirectoriesWithHiddenDirSupported`).

### SRC-134 `UpdateRevisionForPaths` (skip regeneration when paths unchanged)
- **Upstream**: [reposerver/repository/repository.go#L3479](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L3479), [reposerver/repository/repository.go#L3396](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L3396), [reposerver/repository/repository.go#L3627](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L3627), [controller/state.go#L459](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L459)
- **Maturity**: GA
- **Semantics**: Controller calls it per source when the app has the manifest-generate-paths annotation, a non-empty synced revision and the repo `depth == 0`. Repo-server: no paths → `changes=true`; non-git main source (Helm/OCI, or untyped repo whose source is chart/OCI) without ref sources → `changes=false`, no revision. For git: resolve both revisions; equal → no changes; else checkout new revision (no submodules), fetch synced revision, `git diff --name-only` (cached `gitFilesChanges|...`), `changes` = any changed file matches a refresh path (SRC-135). For multi-source Helm with `$ref` value files/file params, each referenced ref source whose target revision differs from the synced one is compared the same way. If nothing relevant changed but revisions advanced, the cached manifests are **renamed** from the old-revision key to the new-revision key (old/new ref SHAs included) so no regeneration happens; cache-rename failures (miss/Redis) still return `changes=false`.
- **Edge cases**: nil repo → InvalidArgument; unknown `$ref` → `changes=true` plus error `source referenced "$x", but no source has a 'ref' field defined`; empty changed-file list counts as changed; resolution errors → gRPC Internal `unable to resolve git revision ...`.
- **Configuration surface**: annotation `argocd.argoproj.io/manifest-generate-paths`; repository `depth`.
- **Exposure**: internal gRPC.
- **Tests required**:
  - positive: commit touching unrelated path → `changes=false`, manifest cache moved, next GenerateManifest is a cache hit.
  - positive: commit touching annotated path → `changes=true`.
  - positive: sibling-path change with cache miss still `changes=false` (`TestUpdateRevisionForPaths_SiblingPathChangesCacheMiss`).
  - positive: Helm/OCI untyped repo does not attempt git resolution.

### SRC-135 `manifest-generate-paths` annotation refresh-path semantics
- **Upstream**: [pkg/apis/application/v1alpha1/application_annotations.go#L13](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/application_annotations.go#L13), [util/app/path/path.go#L115](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/app/path/path.go#L115), [util/app/path/path.go#L155](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/app/path/path.go#L155), [docs/operator-manual/high_availability.md#L333](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/high_availability.md#L333)
- **Maturity**: GA
- **Semantics**: Annotation value is `;`-separated, entries whitespace-trimmed, empty entries skipped. Entry starting with `/` = repo-root-relative (leading `/` stripped); otherwise relative to the source `path` (cleaned join, so `.` = app dir, `../shared` = sibling). For hydrator apps, the sync source always uses its own path (annotation ignored); the dry source uses the annotation. A changed file is relevant if it equals a refresh path, is beneath it, or matches it as a Go `filepath.Match` glob (e.g. `/shared/*-secret.yaml`). The same paths drive webhook refresh filtering (webhook area) and CMP root calculation (SRC-087). Comparison is skipped for shallow repos (`depth > 0`).
- **Edge cases**: annotation present but empty → no paths → always refresh; `**` not supported by `filepath.Match`.
- **Configuration surface**: Application annotation `argocd.argoproj.io/manifest-generate-paths`.
- **Exposure**: CRD (annotation); UI/CLI via metadata editing.
- **Tests required**:
  - positive: `.`, `/guestbook`, `.;../shared`, glob examples from docs each evaluated correctly.
  - positive: ` /path ; ` whitespace trimmed.
  - positive: shallow repo → always regenerate.

---

## O. Helm process environment and Helm plugins

### SRC-136 Helm subprocess environment inheritance and Helm home overrides
- **Upstream**: [util/helm/cmd.go#L70](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/cmd.go#L70), [util/helm/cmd.go#L73](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/cmd.go#L73), [util/helm/cmd.go#L75](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/cmd.go#L75), [util/helm/cmd.go#L83](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/cmd.go#L83), [util/helm/cmd.go#L55](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/cmd.go#L55), [util/helm/cmd.go#L511](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/cmd.go#L511), [util/proxy/proxy.go#L26](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/proxy/proxy.go#L26), [util/helm/helm.go#L47](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/helm.go#L47), [util/helm/helm.go#L136](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/helm.go#L136), [util/helm/client.go#L185](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/client.go#L185), [util/helm/client.go#L326](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/client.go#L326), [reposerver/repository/repository.go#L1361](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L1361), [reposerver/repository/repository.go#L2558](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L2558), [reposerver/repository/repository.go#L2894](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L2894), [cmd/argocd/commands/app_diff.go#L205](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/app_diff.go#L205), [docs/user-guide/helm.md#L643](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/helm.md#L643)
- **Maturity**: GA
- **Semantics**: This refines the "isolated temp helm home" statement in SRC-019.
  - **Full environment inherited**: every `helm` subprocess started through `util/helm.Cmd` begins with the repo-server process's complete environment (`cmd.Env = os.Environ()`). That covers `template`, `dependency build`, `repo add`, `registry login`/`logout`, `pull` (HTTP chart repo and OCI), `show values` (GetAppDetails) and `show chart` (GetRevisionChartDetails). Any variable on the container (image `ENV`, pod `env`/`envFrom`) therefore reaches Helm, including `HELM_PLUGINS`, `HELM_DATA_HOME`, `HELM_CACHE_HOME` and cloud credentials that a plugin may need.
  - **Per-invocation overrides (non-local only)**: four variables are appended after the inherited environment. Each points into a temp directory created per `Cmd` with `os.MkdirTemp("", "helm")` under `$TMPDIR`:
    - `XDG_CACHE_HOME=<tmp>/cache`
    - `XDG_CONFIG_HOME=<tmp>/config`
    - `XDG_DATA_HOME=<tmp>/data`
    - `HELM_CONFIG_HOME=<tmp>/config`

    They are appended last, so they win over inherited variables of the same name (Go `os/exec` uses the last value of a duplicate key). `Cmd.Close()` removes the temp directory when the render, chart extraction or login is disposed. All repo-server code paths are non-local.
  - **Not overridden**: `HELM_CACHE_HOME`, `HELM_DATA_HOME`, `HELM_PLUGINS`, `HELM_REPOSITORY_CONFIG`, `HELM_REPOSITORY_CACHE`, `HELM_REGISTRY_CONFIG` and every other `HELM_*` variable pass through unchanged. In Helm, `HELM_*_HOME` takes precedence over `XDG_*` (see Unconfirmed). So an operator-set `HELM_CACHE_HOME`/`HELM_DATA_HOME` applies to every invocation, while the Helm *config* home (`repositories.yaml`, `registry/config.json` written by `helm repo add` / `helm registry login`) is always per invocation.
  - **Proxy**: if the relevant `Repository.proxy` is non-empty, every inherited variable whose lower-cased name starts with `http_proxy`, `https_proxy` or `no_proxy` is removed. Then `http_proxy=<proxy>`, `https_proxy=<proxy>` and `no_proxy=<noProxy>` are appended. If `proxy` is empty, the pod's proxy variables are inherited unchanged. Which repository's proxy applies:
    - `template`, `dependency build` and `show values`: the Application source repository's proxy (`q.Repo`).
    - Chart pull and registry login in `ExtractChart`: the chart repository's proxy.
    - `show chart`: the proxy of the request's repository.
  - **Build environment**: build variables (`ARGOCD_APP_*`) are not exported to Helm (SRC-038).
  - **Local CLI generation** (`isLocal=true`, SRC-120): no overrides are added. Helm uses the user's own config, cache, data, plugins and registry logins.
  - **Version probe**: `helm version --short` (`helm.Version()`) does not set `cmd.Env`, so it inherits the environment with no overrides.
- **Edge cases**:
  - **Default install manifests** set `HELM_CACHE_HOME`, `HELM_CONFIG_HOME` and `HELM_DATA_HOME` to `/helm-working-dir` (SRC-138). In practice the cache and data homes are shared across all invocations for the pod's lifetime, and only the config home is per invocation. SRC-019's "concurrent renders do not share cache" holds only when `HELM_CACHE_HOME` is unset.
  - **Shared config files**: if an operator sets `HELM_REPOSITORY_CONFIG` or `HELM_REGISTRY_CONFIG`, Helm would read and write that shared file instead of the per-invocation config home. Repo credentials passed by `helm repo add --password` would then persist there. This follows from Helm semantics and is unverified here.
  - **Empty `noProxy`**: a repository proxy with empty `noProxy` yields `no_proxy=` (empty), which clears any pod-level `NO_PROXY` for that invocation.
  - **Dependency proxy**: `helm dependency build` fetches every dependency through the *source* repository's proxy, never a dependency repository's own `proxy` setting.
  - **Temp-dir failure**: if the temp dir cannot be created, the error is `failed to create temporary directory for helm: <err>`.
- **Configuration surface**: any container env var (notably `HELM_*`, `XDG_*`, `http_proxy`/`https_proxy`/`no_proxy`, `TMPDIR`); Repository `proxy`, `noProxy`.
- **Exposure**: implicit (repo-server deployment configuration only).
- **Tests required**:
  - positive: with a fake `helm` that dumps its environment, a pod variable (`FOO=bar`, `HELM_PLUGINS=/x`) is visible to `template`, `dependency build`, `repo add`, `registry login`, `pull`, `show values` and `show chart`.
  - positive: `XDG_CACHE_HOME`/`XDG_CONFIG_HOME`/`XDG_DATA_HOME`/`HELM_CONFIG_HOME` seen by helm point under a fresh per-invocation temp dir, even when the pod sets them to other values. The dir no longer exists after the request.
  - positive: pod-level `HELM_CACHE_HOME` and `HELM_DATA_HOME` reach helm unchanged.
  - positive: repository `proxy` set → pod `HTTP_PROXY`/`https_proxy`/`NO_PROXY` removed and lowercase replacements present. `proxy` unset → pod values present.
  - positive: client-side local generation → no XDG/`HELM_CONFIG_HOME` overrides.
  - negative: `ARGOCD_APP_NAME` is not present in helm's environment.
  - recovery: two concurrent renders get different config homes, so a `repositories.yaml` entry added by one is not visible to the other.

### SRC-137 Operator-installed Helm plugins (downloaders) for repo-server Helm invocations
- **Upstream**: [util/helm/cmd.go#L73](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/cmd.go#L73), [util/helm/cmd.go#L257](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/cmd.go#L257), [util/helm/cmd.go#L453](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/cmd.go#L453), [util/helm/helm.go#L106](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/helm.go#L106), [util/helm/helm.go#L163](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/helm.go#L163), [util/helm/client.go#L246](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/client.go#L246), [util/helm/client.go#L352](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/client.go#L352), [reposerver/repository/repository.go#L2967](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L2967), [reposerver/repository/repository.go#L2979](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L2979), [reposerver/repository/repository.go#L3131](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L3131), [reposerver/repository/repository.go#L3176](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L3176), [reposerver/repository/repository.go#L1189](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L1189), [reposerver/repository/repository.go#L1197](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L1197), [util/kustomize/helmwrapper.go#L23](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/kustomize/helmwrapper.go#L23), [util/kustomize/helmwrapper.go#L42](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/kustomize/helmwrapper.go#L42), [util/kustomize/kustomize_test.go#L684](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/kustomize/kustomize_test.go#L684), [docs/user-guide/helm.md#L611](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/helm.md#L611), [docs/user-guide/helm.md#L640](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/helm.md#L640), [docs/user-guide/helm.md#L647](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/helm.md#L647), [docs/user-guide/helm.md#L684](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/helm.md#L684), [docs/user-guide/helm.md#L600](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/helm.md#L600)
- **Maturity**: GA
- **Semantics**:
  - **How plugins are found**: Argo CD ships no Helm plugins and never runs `helm plugin install`. Plugin support is purely the SRC-136 environment pass-through. Helm loads plugins (`plugin.yaml`; downloader plugins declare `downloaders[].protocols` and a `command`) from `$HELM_PLUGINS`, which defaults to `$HELM_DATA_HOME/plugins`, otherwise `$XDG_DATA_HOME/helm/plugins`. Argo overrides `XDG_DATA_HOME` per invocation, so plugins are found only through an inherited `HELM_PLUGINS` or `HELM_DATA_HOME`.
  - **Documented install recipes**:
    1. A custom image runs `helm plugin install` at build time and sets `ENV HELM_PLUGINS=/home/argocd/.local/share/helm/plugins/`. The docs call this variable required.
    2. An initContainer sharing the `helm-working-dir` volume, with `HELM_CACHE_HOME`/`HELM_CONFIG_HOME`/`HELM_DATA_HOME=/helm-working-dir` on both containers, installs the plugin into `/helm-working-dir/plugins` and runs `chmod -R 777 $HELM_DATA_HOME`.
  - **Where a plugin downloader takes effect** (Helm itself resolves the URL scheme):
    - (a) `helm template --values <scheme>://...` and `--set-file key=<scheme>://...`, for remote value files whose scheme is allowed by `argocd-cm` `helm.valuesFileSchemes` (SRC-025). These URLs are passed to Helm verbatim.
    - (b) `helm pull --repo <repoURL> <chart> --version <v>` for a chart source whose `repoURL` uses a custom scheme, but only when `targetRevision` is an exact version. Argo skips its own index lookup for exact versions (SRC-044).
    - (c) `helm repo add` and `helm dependency build` for chart dependencies that Argo registers. Of these, only `@name`/`alias:name` dependencies can resolve to an Argo-configured Helm repository whose URL has a custom scheme. Dependency `repository:` URLs with any scheme other than `https`/`oci` are never `repo add`-ed by Argo (SRC-040).
  - **Where plugins do not apply** (Argo uses its own Go clients or an isolated helm):
    - Helm index download (`GET <repoURL>/index.yaml` via Go `net/http`) for semver-constraint resolution (SRC-044 and SRC-045), `GetHelmCharts` (SRC-130), and the Helm-type repository connection test (`TestRepository`). All of these fail for non-HTTP schemes.
    - The `GetAppDetails` values preview. It fetches only `http`/`https` remote value files itself and skips others as "not found".
    - Kustomize `--enable-helm` chart inflation. The SRC-068 wrapper forces `HELM_PLUGINS` to an empty Argo-owned directory, so operator-installed plugins are hidden too.
    - CMP sidecars, which are separate containers with their own environment.
  - **Trust boundary**: for native Helm sources, only the repo-server container environment controls the plugin directory. Config home and XDG dirs are Argo-owned temp dirs, and cache/data are operator-owned, so no Application field or repository content can point Helm at a plugin directory. Plugin downloader commands run with the repo-server's UID, environment and mounted secrets.
- **Edge cases**:
  - **Missing `HELM_PLUGINS`**: a custom image installs the plugin in the default user location but omits `HELM_PLUGINS`. The plugin is not found, because `XDG_DATA_HOME` is overridden and, in the default install, `HELM_DATA_HOME=/helm-working-dir`. Helm then reports an unknown-protocol error for the scheme; the exact text is Helm's.
  - **initContainer repo entries ignored**: an initContainer's `helm repo add <name> gs://...` lands in `/helm-working-dir/repositories.yaml`, which the repo-server's helm never reads because `HELM_CONFIG_HOME` is overridden per invocation (Doc/implementation differences).
  - **Custom scheme with a constraint**: a chart source with a custom-scheme `repoURL` and a constraint such as `1.*` fails in Argo's index fetch with `error loading repo index: error making HTTP request: Get "gs://.../index.yaml": unsupported protocol scheme "gs"`.
  - **Scheme not allowed**: a value file `gs://...` whose scheme is missing from `helm.valuesFileSchemes` fails with `the URL scheme 'gs' is not allowed` before Helm runs (SRC-025).
  - **Plugin lost on restart**: `/helm-working-dir` is an `emptyDir`, so a plugin installed by an initContainer is reinstalled on every pod start. Plugin failures surface as helm stderr in the ComparisonError (SRC-047).
- **Configuration surface**: repo-server container env `HELM_PLUGINS`, `HELM_DATA_HOME` (and `HELM_CACHE_HOME`); `helm-working-dir` volume; custom image or initContainer; `argocd-cm` `helm.valuesFileSchemes`; plugin-specific credentials via env or mounted volumes (e.g. `/gcp` in the docs example).
- **Exposure**: deployment configuration only. There is no CRD field, API, CLI or UI for plugins. Their effect is visible through the Helm source fields above.
- **Tests required**:
  - positive: a fake downloader plugin (`plugin.yaml` with `downloaders: [{command: ..., protocols: [probe]}]`) is placed under `$HELM_PLUGINS`. An app with `valueFiles: [probe://x/values.yaml]` and `helm.valuesFileSchemes: "https, probe"` renders with the values the plugin returned.
  - positive: the same plugin installed under `$HELM_DATA_HOME/plugins` (no `HELM_PLUGINS`) through an initContainer-style shared volume is discovered.
  - positive: a chart source with `repoURL: probe://repo`, `chart: c` and exact `targetRevision: 1.0.0` is pulled through the plugin.
  - negative: a plugin installed only in the default `~/.local/share/helm/plugins` without `HELM_PLUGINS` → unknown-scheme failure.
  - negative: a custom-scheme chart source with constraint `1.*` → index-fetch error. `GetHelmCharts` and the repository connection test also fail for that URL.
  - negative/security: kustomize `--enable-helm` with a `helmCharts` repo `probe://` never runs the plugin's downloader, whether the plugin is operator-installed or planted via `helmGlobals.configHome` (adapt upstream `TestKustomizeBuildHelmChartsIgnoresConfigHomePlugin`).
  - negative/security: a `plugins/` directory inside the chart or repository is never loaded for native Helm sources.
  - negative: a repo entry added by an initContainer `helm repo add` is not visible to `helm dependency build` (dependency `@that-name` resolves only via Argo-configured repositories).

### SRC-138 Default repo-server Helm working directory (`/helm-working-dir`)
- **Upstream**: [manifests/base/repo-server/argocd-repo-server-deployment.yaml#L278](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/repo-server/argocd-repo-server-deployment.yaml#L278), [manifests/base/repo-server/argocd-repo-server-deployment.yaml#L303](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/repo-server/argocd-repo-server-deployment.yaml#L303), [manifests/base/repo-server/argocd-repo-server-deployment.yaml#L325](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/repo-server/argocd-repo-server-deployment.yaml#L325), [manifests/base/repo-server/argocd-repo-server-deployment.yaml#L365](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/repo-server/argocd-repo-server-deployment.yaml#L365), [manifests/install.yaml#L32810](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/install.yaml#L32810), [manifests/ha/install.yaml#L33842](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/ha/install.yaml#L33842), [manifests/namespace-install.yaml#L1728](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/namespace-install.yaml#L1728), [util/helm/cmd.go#L80](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/cmd.go#L80), [docs/user-guide/helm.md#L662](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/helm.md#L662)
- **Maturity**: GA
- **Semantics**: All upstream install bundles (base, `install.yaml`, HA, namespace-install, and the `-with-hydrator` variants) configure the `argocd-repo-server` container with:
  - env `HELM_CACHE_HOME=/helm-working-dir`, `HELM_CONFIG_HOME=/helm-working-dir` and `HELM_DATA_HOME=/helm-working-dir`;
  - a volume `helm-working-dir` (`emptyDir: {}`) mounted at `/helm-working-dir`;
  - a read-only root filesystem and non-root UID 999.

  Combined with SRC-136, the effective layout per helm invocation is:
  - config: per-invocation temp dir, because Argo appends its own `HELM_CONFIG_HOME`;
  - cache: shared `/helm-working-dir`, reused across invocations and apps for the pod's lifetime;
  - data and plugins: shared `/helm-working-dir`, with plugins in `/helm-working-dir/plugins`.

  This volume is the hand-off point for the SRC-137 initContainer recipe.
- **Edge cases**:
  - **Variables removed**: if an operator removes the three env vars, Helm cache and data fall back to the per-invocation XDG temp dirs. That gives full isolation, no cache reuse, and no initContainer plugin discovery unless `HELM_PLUGINS` is set.
  - **Restarts**: the volume is an `emptyDir`, so contents are lost on pod restart.
  - **Shared cache across tenants**: Helm cache content (for example repository index files downloaded during `helm repo add` / `dependency build`) is shared between all Applications rendered by that pod.
- **Configuration surface**: repo-server Deployment env `HELM_CACHE_HOME`, `HELM_CONFIG_HOME`, `HELM_DATA_HOME`; volume `helm-working-dir`.
- **Exposure**: install manifests.
- **Tests required**:
  - positive: Patrol's install bundles set the three env vars and mount a writable `emptyDir` at the same path, with a read-only root FS.
  - positive: a downloader plugin copied into `<helm-working-dir>/plugins` by an init container is discovered by renders without `HELM_PLUGINS` (SRC-137).
  - positive: with the default manifests, the config home seen by helm is still a per-invocation temp dir, not `/helm-working-dir`.

---

## Unconfirmed

- **JSON file that fails Kubernetes decoding but decodes as an OCI image manifest** (`getObjsFromYAMLOrJSON`, [reposerver/repository/repository.go#L2032](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L2032)): the code suppresses the error and still appends the (empty/partially decoded) object. Since any JSON object decodes into the OCI `Manifest` struct, a non-Kubernetes `.json` file (e.g. `{"foo":1}`) appears to produce an empty object rather than an error or a skip. Exact downstream outcome (tracking injection on an empty object, `null` manifest) not verified; no upstream test covers it. Paths checked: repository.go L2019–L2080, repository_test.go `Test_findManifests`, `TestInvalidManifestsInDir`.
- **Directory-type app containing `kustomization.yaml` / Helm templates**: by code reading, `kustomization.yaml` (has `apiVersion` + `kind`) would be decoded as a `Kustomization` object and emitted; Helm templates with `{{ }}` usually fail YAML parsing and are ignored unless they contain `apiVersion:`/`kind:`/`metadata:` (then error). Not verified by an upstream test. Paths checked: repository.go L2055–L2078, docs/user-guide/directory.md L46.
- **Helm handling of remote `--values <url>` and `--set-file <url>`**: Argo CD passes allowed-scheme URLs to the Helm v4 binary verbatim; authentication/redirect behaviour is Helm's and was not verified. `GetAppDetails` fetches http/https value URLs itself via `config.ReadRemoteFile` (util/helm/helm.go L162–L164), whose auth behaviour was not inspected.
- **Kustomize version detection caching**: `getSemverSafe` caches the first detected version in a process-global variable and `mapToEditAddArgs` always probes the default binary ([util/kustomize/kustomize.go#L476](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/kustomize/kustomize.go#L476)); with multiple registered kustomize versions the v3.7.0/v3.8.5/v5.3.0 gates may use the wrong binary's version. Whether this is relied upon is unconfirmed.
- **UI file-parameter editing**: the Helm file-parameters section in the Parameters tab writes to `spec.source.helm.parameters` ([application-parameters.tsx#L858](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-parameters/application-parameters.tsx#L858)) — possibly a UI bug; runtime behaviour not verified.
- **`ARGOCD_HELM_INDEX_CACHE_DURATION`** is declared ([common/common.go#L306](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L306)) but no non-test reference was found (`grep` over all `.go` files); Helm index TTL is the revision-cache expiration.
- **Git transport details used by resolution/checkout** (retry counts `ARGOCD_GIT_ATTEMPTS_COUNT`, `ARGOCD_GIT_REQUEST_TIMEOUT`, ls-remote parallelism, LFS, credential helpers, builtin git config) are referenced but owned by the repositories/git area; only their effect on revision resolution is listed here.
- **Exact text of CMP kill errors** after context expiry (`signal: terminated` vs `signal: killed`) depends on whether the process exits on SIGTERM; inferred from `cmpserver/plugin/plugin.go` L107–L133, not covered by a test read.
- **`isSourcePermitted` glob semantics** for SRC-041 delegate to `AppProject.IsSourcePermitted` (projects area); not re-verified here.
- **Helm environment precedence (SRC-136/137/138)**: this checkout does not contain the Helm source (Helm 4.2.1 per [hack/tool-versions.sh#L14](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/hack/tool-versions.sh#L14); no `helm.sh/helm` module in `go.mod`). The following Helm behaviours are inferred, not read from code:
  - `HELM_CACHE_HOME`/`HELM_CONFIG_HOME`/`HELM_DATA_HOME` take precedence over `XDG_*`;
  - `HELM_PLUGINS` defaults to `$HELM_DATA_HOME/plugins`;
  - `HELM_REPOSITORY_CONFIG`/`HELM_REGISTRY_CONFIG` override config-home paths.

  The inference rests on three sources: the docs' statement that `HELM_PLUGINS` is required ([docs/user-guide/helm.md#L643](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/helm.md#L643)), the helm-wrapper comment that Helm auto-loads `$HELM_DATA_HOME/plugins/*/plugin.yaml` ([util/kustomize/helmwrapper.go#L23](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/kustomize/helmwrapper.go#L23)), and the regression test that plants a legacy-format downloader plugin ([util/kustomize/kustomize_test.go#L684](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/kustomize/kustomize_test.go#L684)). That test is negative only, so positive loading of legacy `downloaders:` plugins by Helm 4.2.1, and compatibility of third-party plugins such as helm-gcs, are not verified.
- **End-to-end `gs://` (or other plugin-scheme) Helm chart repositories**: only the shell-out paths can reach a plugin. Those are `helm pull --repo` for exact versions ([util/helm/cmd.go#L257](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/cmd.go#L257), [reposerver/repository/repository.go#L2967](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L2967)), `helm repo add` for `@name` dependencies, and remote value files. Several things are unconfirmed:
  - whether `helm pull --repo gs://...` and `helm repo add gs://...` actually use plugin getters in Helm 4.2.1;
  - whether such a repository can be registered via `argocd repo add`, because its connection test calls Argo's `net/http` index fetch ([reposerver/repository/repository.go#L3176](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L3176), [util/helm/client.go#L352](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/client.go#L352));
  - whether `helm dependency build` resolves a Chart.yaml dependency `repository: gs://...` that Argo never `repo add`s ([reposerver/repository/repository.go#L1197](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L1197)).

  Constraint resolution, `GetHelmCharts` and connection tests for such URLs fail with Go's `unsupported protocol scheme`; that failure is inferred from code, not observed.
- **Shared Helm cache contents under the default install (SRC-138)**: which files Helm 4 writes to `HELM_CACHE_HOME` during `repo add` / `dependency build` / `pull`, and whether concurrent invocations sharing `/helm-working-dir` can race, is Helm behaviour and was not verified. Upstream has no test of the inherited-environment behaviour in [util/helm/cmd_test.go](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/cmd_test.go#L52); only proxy-field assignment is tested.

## Doc/implementation differences

1. **Build env vars `KUBE_VERSION` / `KUBE_API_VERSIONS`**: [docs/user-guide/build-environment.md#L16](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/build-environment.md#L16) lists them as available to Helm, Jsonnet and Kustomize, but code only adds them to the CMP environment ([reposerver/repository/repository.go#L2351](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L2351)); `newEnv` (L1828) used for Helm/Jsonnet/Kustomize does not include them. Code is baseline.
2. **CMP `discover.find.command` working directory**: docs ([config-management-plugins.md#L68](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/config-management-plugins.md#L68)) say it "runs in the repository's root directory"; code runs it in the application directory inside the CMP workdir ([cmpserver/plugin/plugin.go#L365](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmpserver/plugin/plugin.go#L365)).
3. **CMP command timeouts**: docs ([config-management-plugins.md#L323](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/config-management-plugins.md#L323), [#L337](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/config-management-plugins.md#L337)) say each CMP command times out on the sidecar's `ARGOCD_EXEC_TIMEOUT` (90s) with `ARGOCD_EXEC_FATAL_TIMEOUT` escalation; cmp-server `runCommand` does not use `util/exec` timeouts at all — only the request context deadline (minus 100 ms) and a fixed 5 s SIGTERM→SIGKILL grace ([cmpserver/plugin/plugin.go#L107](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmpserver/plugin/plugin.go#L107)).
4. **Repo-server exec timeout signal**: [high_availability.md#L52](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/high_availability.md#L52) says a SIGTERM is sent on `ARGOCD_EXEC_TIMEOUT` followed by SIGKILL after `ARGOCD_EXEC_FATAL_TIMEOUT`; for helm/kustomize invoked via `executil.Run` the default timeout behaviour is an immediate SIGKILL without waiting ([util/exec/exec.go#L155](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/exec/exec.go#L155), [#L223](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/exec/exec.go#L223)).
5. **Per-version Kustomize build options**: docs ([kustomize.md#L177](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/kustomize.md#L177), [argocd-cm.yaml#L265](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cm.yaml#L265)) describe `kustomize.buildOptions.<version>`; settings parse it into `KustomizeVersion.BuildOptions` ([util/settings/settings.go#L1297](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L1297)) but `kustomize.Build` only uses the global `KustomizeOptions.BuildOptions` ([util/kustomize/kustomize.go#L384](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/kustomize/kustomize.go#L384)); no other consumer exists. Per-version options have no effect.
6. **OCI default media types**: [oci.md#L97](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/oci.md#L97) lists two defaults; code default also includes `application/vnd.oci.image.layer.v1.tar` ([cmd/argocd-repo-server/commands/argocd_repo_server.go#L278](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-repo-server/commands/argocd_repo_server.go#L278)); argocd-cmd-params-cm.yaml matches code.
7. **OCI metadata annotations**: [oci.md#L139](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/oci.md#L139) lists `title`, `revision`, `url` as recognized; `GetOCIMetadata` maps only `created`, `authors`, `documentation`, `source`, `version`, `description` (url marked TODO) ([reposerver/repository/repository.go#L2858](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L2858)); `documentation` is used but undocumented.
8. **Tool detection order**: [tool_detection.md#L27](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/tool_detection.md#L27) omits that CMP discovery is evaluated before Helm/Kustomize, that Kustomize wins when both `Chart.yaml` and a kustomization exist, and that any filename *ending* in `Chart.yaml` triggers Helm ([util/app/discovery/discovery.go#L41](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/app/discovery/discovery.go#L41), [#L62](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/app/discovery/discovery.go#L62)).
9. **CMP plugin env interpolation**: docs ([config-management-plugins.md#L223](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/config-management-plugins.md#L223)) say references to "system and build variables" are interpolated in `plugin.env` values; interpolation happens in the repo-server against the build env only (plus KUBE_*), so sidecar system variables expand to empty ([reposerver/repository/repository.go#L2367](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L2367)).
10. **CMP discovery with manifest-generate-paths**: [high_availability.md#L59](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/high_availability.md#L59) implies discovery copies only annotation paths; code sends the full repository for `MatchRepository` and `GetParametersAnnouncement`, and only the `GenerateManifest` stream uses the common root ([reposerver/repository/repository.go#L2400](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L2400), [#L2407](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L2407)).
11. **Directory apps with Helm/Kustomize files**: [directory.md#L46](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/directory.md#L46) says rendering "will fail"; code applies generic YAML rules (non-resource-looking YAML silently skipped) — see Unconfirmed for the exact outcome.
12. **Helm `kubeVersion` docs vs code**: CRD doc comment for `helm.kubeVersion`/`kustomize.kubeVersion` ([types.go#L592](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L592)) does not mention that values are normalized (pre-release/build metadata stripped) before being passed ([reposerver/repository/repository.go#L1248](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L1248)).
13. **Kustomize `commonLabels` env substitution**: docs mention env substitution only for `commonAnnotations` behind `commonAnnotationsEnvsubst` ([kustomize.md#L243](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/kustomize.md#L243)); code always substitutes build env into `commonLabels` values and `images` ([util/kustomize/kustomize.go#L189](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/kustomize/kustomize.go#L189), [#L236](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/kustomize/kustomize.go#L236)).
14. **`ARGOCD_HELM_INDEX_CACHE_DURATION`** constant comment ([common/common.go#L305](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L305)) claims it controls Helm index caching; nothing reads it (see Unconfirmed).
15. **Helm plugin initContainer recipe and `helm repo add`**: the docs' initContainer example ([docs/user-guide/helm.md#L684](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/helm.md#L684)) runs `helm repo add my-gcs-repo gs://...` with `HELM_CONFIG_HOME=/helm-working-dir`. The repo-server, however, overrides `HELM_CONFIG_HOME` with a per-invocation temp dir on every non-local helm call ([util/helm/cmd.go#L80](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/cmd.go#L80)). The repository entry written by the initContainer is therefore never seen by Argo's helm invocations. Only the plugin itself survives, found via the inherited `HELM_DATA_HOME`/`HELM_PLUGINS`. Code is baseline (SRC-136, SRC-137).
16. **"`gs://` protocol for Helm chart repository access"**: [docs/user-guide/helm.md#L615](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/helm.md#L615) presents plugins as enabling plugin-scheme chart repositories. Argo CD itself fetches Helm repository indexes with its own Go HTTP client ([util/helm/client.go#L352](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/client.go#L352)). That client handles semver-constraint resolution, `GetHelmCharts` and repository connection tests, and it cannot use Helm plugins. Only exact-version chart pulls, value files and named dependency repositories go through the helm binary, where plugins apply. End-to-end support is listed under Unconfirmed.
