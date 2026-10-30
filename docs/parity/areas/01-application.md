# Application — parity inventory (Argo CD v3.5.5)

Scope: this area covers the `Application` custom resource (`argoproj.io/v1alpha1`) as a data model and as a lifecycle object. In scope: the complete field inventory of `spec`, `status` and the top-level `operation` field (`ApplicationSpec`, `ApplicationSource*`, `ApplicationDestination`, `SyncPolicy`, `Info`, `ApplicationStatus`, every `ApplicationCondition` type, `OperationState`, `RevisionHistory`, `SourceHydrator`, `ResourceStatus`, `HealthStatus`); create/update/patch/delete through the CRD, the API server, the CLI and the UI; finalizers (cascade foreground/background, non-cascade, pre-delete and post-delete hooks with their finalizers); Applications in any namespace; resource tracking methods and ownership-conflict detection; refresh and reconciliation semantics (refresh annotation, periodic, jitter, hard reconciliation, skip-reconcile); Application health aggregation and app-of-apps; per-application notice, info, external links and managed-by-url; multiple sources (`ref`, `$ref` value files, `name`); the Source Hydrator (controller hydrator, commit server, `hydrator.metadata`, git notes); and every key of `docs/user-guide/annotations-and-labels.md`. Out of scope (owned by other areas, referenced here only at the field/key level): per-sync-option behaviour, sync waves and sync-phase hooks, auto-sync/self-heal algorithms beyond the CRD fields, diffing and `ignoreDifferences`/compare-option semantics, Helm/Kustomize/Jsonnet/CMP rendering semantics, AppProject rules, ApplicationSet, RBAC policy grammar, notifications, Git webhooks, repository/credential management, cluster sharding, resource actions, pod logs/exec, and the `sync`/`rollback`/`diff`/`resources` API calls.

All permalinks use base `https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/`.

### APP-001 Application CRD resource definition
- **Upstream**: [types.go#L59](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L59), [types.go#L63](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L63), [types.go#L68](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L68), [application-crd.yaml#L9](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/crds/application-crd.yaml#L9), [application-crd.yaml#L19](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/crds/application-crd.yaml#L19), [application-crd.yaml#L2339](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/crds/application-crd.yaml#L2339), [application-crd.yaml#L7105](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/crds/application-crd.yaml#L7105), [application-crd.yaml#L7111](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/crds/application-crd.yaml#L7111), [register.go#L8](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/register.go#L8)
- **Maturity**: GA
- **Semantics**: CRD `applications.argoproj.io`, group `argoproj.io`, served+storage version `v1alpha1`, kind `Application`, list kind `ApplicationList`, plural `applications`, singular `application`, short names `app` and `apps`, scope `Namespaced`. Top-level object fields: `metadata`, `spec`, `status`, and `operation` (the operation request lives at the root, not under spec/status). Additional printer columns: `Sync Status` (`.status.sync.status`), `Health Status` (`.status.health.status`), `Revision` (`.status.sync.revision`, priority 10), `Project` (`.spec.project`, priority 10). Root schema requires `metadata` and `spec`; `spec` requires `destination` and `project`. The CRD declares `subresources: {}` (no `/status` subresource, `+genclient:noStatus`), so the controller and API server write `status` and `operation` through ordinary update/patch of the whole object.
- **Edge cases**: Omitting the `spec.project` key is rejected by the API server schema, but `project: ""` is accepted and later defaulted to `default` (APP-002). Because there is no status subresource, any principal with `update`/`patch` on the object can overwrite `status` (relied upon by the skip-reconcile use case, APP-088). Unknown fields are pruned except under `valuesObject` (preserve-unknown-fields).
- **Configuration surface**: CRD manifest `manifests/crds/application-crd.yaml`.
- **Exposure**: CRD: yes (kubectl `get app|apps`, columns above). API: objects returned by `ApplicationService`. CLI: `argocd app *`. UI: Applications list/details.
- **Tests required**:
  - Positive: `kubectl get apps` lists objects with the four printer columns (two only in `-o wide`); short names `app`/`apps` resolve.
  - Negative: creating an Application without `spec.destination` or without the `spec.project` key fails schema validation.
  - Positive: `status` written via a normal PATCH of the object persists (no status subresource).
  - Positive: `operation` round-trips as a root-level field.

### APP-002 spec.project and the default project
- **Upstream**: [types.go#L82](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L82), [types.go#L3828](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L3828), [application_defaults.go#L25](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/application_defaults.go#L25), [argo.go#L1016](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L1016), [appcontroller.go#L2283](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2283), [appcontroller.go#L2831](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2831), [appcontroller.go#L417](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L417), [application.go#L1135](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1135), [application.yaml#L17](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/application.yaml#L17)
- **Maturity**: GA
- **Semantics**: Name of the AppProject (looked up in the control-plane namespace). Empty string means `default` everywhere (`GetProject()`). `NormalizeApplicationSpec` writes `default` into an empty `spec.project`; the API server normalizes on create/update and the controller patches the normalized spec back during reconciliation (`normalizeApplication`, merge patch, log `Normalized app spec`). Project is resolved through a per-name cache; global projects are merged in (virtual project). The project must permit the Application's namespace (APP-068).
- **Edge cases**: Project not found -> controller condition `InvalidSpecError` with message `Application referencing project <name> which does not exist`; other lookup errors -> `UnknownError`; namespace not permitted -> `UnknownError` with `application '<name>' in namespace '<ns>' is not permitted to use project '<proj>'`. On any of these the controller sets sync and health to `Unknown` (APP-036). API create/update returns `InvalidArgument: app is not allowed in project "<p>", or the project does not exist` for both "not found" and "not permitted" to avoid project enumeration.
- **Configuration surface**: `spec.project`; AppProject objects.
- **Exposure**: CRD `spec.project`; API create/update/patch; CLI `argocd app create|set --project`; UI create panel `PROJECT` field and summary panel edit.
- **Tests required**:
  - Positive: app with `project: ""` is persisted/normalized to `default`.
  - Negative: app referencing a missing project gets `InvalidSpecError` with the exact message, sync/health `Unknown`.
  - Negative (authorization): API create in a nonexistent project returns the vague InvalidArgument message.
  - Recovery: creating the missing project clears the condition on next reconciliation.

### APP-003 spec.destination.server
- **Upstream**: [types.go#L1195](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1195), [argo.go#L1089](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L1089), [argo.go#L1117](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L1117), [argo.go#L694](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L694), [argo.go#L37](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L37), [application_defaults.go#L31](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/application_defaults.go#L31), [application.yaml#L213](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/application.yaml#L213)
- **Maturity**: GA
- **Semantics**: API server URL of the target cluster. Exactly one of `server` or `name` must be set. Trailing `/` characters are trimmed before lookup. The URL must match a registered cluster (in-cluster is `https://kubernetes.default.svc`). After resolution the project must permit the destination (server + namespace) via `IsDestinationPermitted`. A change in `spec.destination` (compared with `status.sync.comparedTo.destination`) triggers a refresh with reason `spec.destination differs`.
- **Edge cases**: Both set -> `application destination can't have both name and server defined: <name> <server>`; neither set -> `Destination server missing from app spec`; unknown server -> `error getting cluster by server "<url>": ...`; not permitted -> `InvalidSpecError` `application destination server '<server>' and namespace '<ns>' do not match any of the allowed destinations in project '<proj>'`. API create/update with unresolvable destination -> `InvalidArgument: application destination spec for <app> is invalid: <err>`.
- **Configuration surface**: `spec.destination.server`; cluster secrets.
- **Exposure**: CRD; API; CLI `--dest-server`; UI create panel `Cluster URL`.
- **Tests required**:
  - Positive: server with trailing slash resolves to the registered cluster.
  - Negative: name+server both set -> exact error; unknown server -> condition `InvalidSpecError`.
  - Authorization: destination outside project destinations -> `InvalidSpecError` message above; API create rejected.
  - Positive: changing destination triggers immediate refresh.

### APP-004 spec.destination.name
- **Upstream**: [types.go#L1201](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1201), [argo.go#L1096](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L1096), [argo.go#L709](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L709), [application_defaults.go#L34](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/application_defaults.go#L34)
- **Maturity**: GA
- **Semantics**: Symbolic cluster name, resolved to exactly one server URL via the cluster registry (`in-cluster` is the default in-cluster name). The resolved cluster is then used for everything else; permission error messages print the cluster name instead of the URL when `name` was used.
- **Edge cases**: No match -> `there are no clusters with this name: <name>`; multiple clusters with the same name -> `there are <n> clusters with the same name: [<url1> <url2>]`; lookup failure -> `error getting cluster by name "<name>": ...`.
- **Configuration surface**: `spec.destination.name`.
- **Exposure**: CRD; API; CLI `--dest-name`; UI create panel (cluster by name).
- **Tests required**:
  - Positive: `name: in-cluster` resolves.
  - Negative: ambiguous name -> exact message; unknown name -> exact message as `InvalidSpecError`.

### APP-005 spec.destination.namespace
- **Upstream**: [types.go#L1198](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1198), [state.go#L525](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L525), [state.go#L1006](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L1006), [types.go#L373](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L373), [hook.go#L150](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/hook.go#L150), [application.yaml#L219](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/application.yaml#L219)
- **Maturity**: GA
- **Semantics**: Default namespace for rendered namespaced resources that have no `metadata.namespace`; cluster-scoped resources have any namespace stripped. When the controller changes a resource namespace it re-applies the tracking metadata. It is the default Helm/Kustomize namespace unless `helm.namespace`/`kustomize.namespace` is set, the default namespace for PreDelete/PostDelete hooks, the namespace targeted by `CreateNamespace=true` and `managedNamespaceMetadata`, and is part of the project destination check.
- **Edge cases**: A namespaced resource that still ends up with an empty namespace (empty destination namespace and none in manifest) produces `InvalidSpecError` `Namespace for <name> <gvk> is missing.`. Scope (namespaced vs cluster) is taken from cluster discovery; unknown kinds are treated as namespaced.
- **Configuration surface**: `spec.destination.namespace`.
- **Exposure**: CRD; API; CLI `--dest-namespace`; UI create panel `Namespace`.
- **Tests required**:
  - Positive: Deployment without namespace is applied/compared in destination namespace.
  - Positive: ClusterRole with a namespace in Git is treated as cluster-scoped (namespace stripped).
  - Negative: empty destination namespace + namespaced manifest without namespace -> exact `InvalidSpecError`.

### APP-006 spec.source (single source: repoURL, path, chart, targetRevision)
- **Upstream**: [types.go#L196](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L196), [types.go#L246](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L246), [types.go#L337](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L337), [types.go#L347](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L347), [argo.go#L588](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L588), [argo.go#L680](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L680), [application-crd.yaml#L1455](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/crds/application-crd.yaml#L1455), [application.yaml#L20](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/application.yaml#L20)
- **Maturity**: GA
- **Semantics**: `repoURL` (CRD-required within `source`), `path` (directory in Git/OCI), `targetRevision` (branch/tag/commit; empty means `HEAD` for Git; semver version/constraint for Helm), `chart` (Helm repo chart name; presence makes the source Helm). `oci://` prefix marks OCI sources. Precedence: if `spec.sourceHydrator` is set, `source`/`sources` are ignored and the sync source is used; else if `spec.sources` is non-empty it wins and `spec.source` is ignored; else `spec.source`. Validation (`ValidatePermissions`): single source requires `repoURL` and (`path` or `chart`); a Helm `chart` requires `targetRevision`; repo must be in the project's `sourceRepos`. A change to the source (vs `status.sync.comparedTo.source`) triggers refresh `spec.source differs` with forced revision resolution.
- **Edge cases**: Messages (as `InvalidSpecError`): `spec.source.repoURL and either spec.source.path or spec.source.chart are required`; `spec.source.targetRevision is required if the manifest source is a helm chart`; `application repo <url> is not permitted in project '<proj>'`. The `ref` field is ignored for a single `source`.
- **Configuration surface**: `spec.source.{repoURL,path,targetRevision,chart}`.
- **Exposure**: CRD; API; CLI `--repo --path --revision --helm-chart`; UI create panel SOURCE section and summary panel.
- **Tests required**:
  - Positive: Git source with path; Helm chart with version; OCI source.
  - Negative: missing path and chart -> exact message; chart without targetRevision -> exact message.
  - Authorization: repo outside `sourceRepos` -> `InvalidSpecError`.
  - Positive: both `source` and `sources` set -> `sources` used.

### APP-007 spec.source.tagPrefix
- **Upstream**: [types.go#L219](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L219), [client.go#L267](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/client.go#L267), [repository.go#L364](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L364), [argocd_app_create.md#L113](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/commands/argocd_app_create.md#L113)
- **Maturity**: GA
- **Semantics**: Filters Git tags to those starting with the prefix before evaluating `targetRevision` as a semver constraint; the prefix is stripped before comparison and re-added to the resolved tag (e.g. prefix `component-b/` + `1.0.*` resolves to `component-b/1.0.1`).
- **Edge cases**: Only meaningful for Git sources with a semver-constraint `targetRevision`. Detailed resolution rules live in the repository area.
- **Configuration surface**: `spec.source.tagPrefix` / `spec.sources[].tagPrefix`.
- **Exposure**: CRD; API; CLI `--tag-prefix`; UI: status panel shows the branch/revision as `tagPrefix + targetRevision` ([application-status-panel.tsx#L84](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-status-panel/application-status-panel.tsx#L84)); no create-panel input found (see Unconfirmed).
- **Tests required**:
  - Positive: tags `a/1.0.0`, `a/1.0.1`, `b/2.0.0` with prefix `a/` and constraint `1.0.*` resolve to `a/1.0.1`.
  - Negative: no matching tag -> comparison error.

### APP-008 spec.source.helm options (field inventory)
- **Upstream**: [types.go#L567](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L567), [types.go#L605](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L605), [types.go#L615](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L615), [types.go#L625](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L625), [types.go#L682](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L682), [application.yaml#L26](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/application.yaml#L26)
- **Maturity**: GA
- **Semantics**: Fields: `valueFiles[]`, `parameters[]` (`name`, `value`, `forceString`), `releaseName` (default: Application name), `values` (string block, patchStrategy replace), `valuesObject` (raw object, preserve-unknown-fields; takes precedence over `values`), `fileParameters[]` (`name`, `path`), `version`, `passCredentials`, `ignoreMissingValueFiles`, `skipCrds`, `skipTests`, `skipSchemaValidation`, `namespace` (default destination namespace), `kubeVersion`, `apiVersions[]` (`[group/]version/kind`; default from target cluster). CLI `--helm-set`/`--helm-set-string` (`name=value`) escape unescaped commas in the value (`a,b` -> `a\,b`); `--helm-set-string` sets `forceString: true`; adding a parameter with an existing name overwrites it. `IsZero` treats an empty helm block as absent (normalized away, APP-013).
- **Edge cases**: CLI parameter without `=` -> `expected helm parameter of the form param=value but received: <text>`; file parameter -> `expected helm file parameter of the form param=path but received: <text>`. Rendering semantics belong to the source-tooling area.
- **Configuration surface**: `spec.source.helm.*`, `spec.sources[].helm.*`, `spec.sourceHydrator.drySource.helm.*`.
- **Exposure**: CRD; API; CLI `--values --values-literal-file --release-name --helm-version --helm-pass-credentials --helm-set --helm-set-string --helm-set-file --helm-skip-crds --helm-skip-schema-validation --helm-skip-tests --helm-namespace --helm-kube-version --helm-api-versions --ignore-missing-value-files`; `argocd app unset --values --values-literal --parameter --pass-credentials --ignore-missing-value-files`; UI parameters tab.
- **Tests required**:
  - Positive: each field round-trips through CRD/API; `valuesObject` preserves arbitrary nested keys.
  - Positive: `valuesObject` and `values` both set -> `valuesObject` wins.
  - Negative: malformed `--helm-set` -> exact CLI error.

### APP-009 spec.source.kustomize options (field inventory)
- **Upstream**: [types.go#L720](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L720), [types.go#L761](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L761), [types.go#L804](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L804), [types.go#L852](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L852), [application.yaml#L109](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/application.yaml#L109)
- **Maturity**: GA
- **Semantics**: Fields: `namePrefix`, `nameSuffix`, `images[]` (`[old=]name[:tag|@digest]`; merged by image name), `commonLabels`, `commonAnnotations`, `commonAnnotationsEnvsubst`, `forceCommonLabels`, `forceCommonAnnotations`, `labelWithoutSelector`, `labelIncludeTemplates`, `version`, `namespace`, `replicas[]` (`name`, `count` int-or-string; must parse as integer), `patches[]` (`path`, `patch`, `target{group,version,kind,name,namespace,annotationSelector,labelSelector}`, `options` map[string]bool), `components[]`, `ignoreMissingComponents`, `kubeVersion`, `apiVersions[]`. Empty blocks are normalized away.
- **Edge cases**: Replica count not an integer -> `expected integer value for count. Received: <v>`; CLI replica without `=` -> `expected parameter of the form: name=count. Received: <text>`. Rendering semantics in source-tooling area.
- **Configuration surface**: `spec.source.kustomize.*` (also in `sources[]` and `drySource`).
- **Exposure**: CRD; API; CLI `--nameprefix --namesuffix --kustomize-*`, `--ignore-missing-components`; `argocd app unset --kustomize-image --kustomize-replica --kustomize-version --kustomize-namespace --nameprefix --namesuffix --ignore-missing-components`; UI parameters tab.
- **Tests required**:
  - Positive: each field round-trips; `--kustomize-image` replaces existing image with same name.
  - Negative: non-integer replica count rejected by CLI.

### APP-010 spec.source.directory and jsonnet options (field inventory)
- **Upstream**: [types.go#L931](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L931), [types.go#L916](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L916), [types.go#L900](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L900), [argo.go#L1045](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L1045), [application.yaml#L160](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/application.yaml#L160)
- **Maturity**: GA
- **Semantics**: `directory.recurse`, `directory.include` (glob), `directory.exclude` (glob, takes precedence over include), `directory.jsonnet.{extVars[], tlas[], libs[]}` where vars are `{name, value, code}`. Normalization: a directory block with only `include`/`exclude` keeps only those; an entirely empty directory block becomes nil.
- **Edge cases**: `IsZero` ignores include/exclude (so `recurse:false` + include only is "zero" but include/exclude are preserved by normalization).
- **Configuration surface**: `spec.source.directory.*`.
- **Exposure**: CRD; API; CLI `--directory-recurse --directory-include --directory-exclude --jsonnet-tla-str --jsonnet-tla-code --jsonnet-ext-var-str --jsonnet-ext-var-code --jsonnet-libs`; UI parameters tab.
- **Tests required**:
  - Positive: include/exclude-only directory block survives normalization.
  - Positive: empty `directory: {}` is removed on normalization.

### APP-011 spec.source.plugin options (field inventory)
- **Upstream**: [types.go#L1136](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1136), [types.go#L1015](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1015), [types.go#L1047](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1047), [types.go#L1100](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1100), [types.go#L1183](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1183), [application.yaml#L186](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/application.yaml#L186)
- **Maturity**: GA
- **Semantics**: `plugin.name` (optional; sidecar plugin discovery when empty), `plugin.env[]` (`{name,value}`), `plugin.parameters[]` each with `name` plus one of `string`, `array`, `map`. JSON marshalling emits `{}`/`[]` for present-but-empty map/array. Parameters are exposed to the plugin as `ARGOCD_APP_PARAMETERS=<json>` and per-parameter env vars `PARAM_<NAME>` (string), `PARAM_<NAME>_<KEY>` (map), `PARAM_<NAME>_<i>` (array), with names uppercased and non `[A-Z0-9_]` characters replaced by `_`.
- **Edge cases**: Removing an unknown env var via CLI -> `unable to find env variable with key "<k>" for plugin "<name>"`; env entry without `=` -> `expected env entry of the form: param=value but received: <text>`; `$$` escapes `$` in env substitution.
- **Configuration surface**: `spec.source.plugin.*`.
- **Exposure**: CRD; API; CLI `--config-management-plugin --plugin-env`, `argocd app unset --plugin-env`; UI parameters tab.
- **Tests required**:
  - Positive: present-but-empty `array: []`/`map: {}` round-trip and are distinguishable from absent.
  - Positive: env var naming for parameter `my-param` -> `PARAM_MY_PARAM`.

### APP-012 Explicit source type exclusivity and status.sourceType / sourceTypes
- **Upstream**: [types.go#L416](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L416), [types.go#L3799](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L3799), [argo.go#L467](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L467), [types.go#L1233](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1233), [state.go#L1097](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L1097)
- **Maturity**: GA
- **Semantics**: Source type enum: `Helm`, `Kustomize`, `Directory`, `Plugin`. At most one of `helm`, `kustomize`, `directory`, `plugin` may be set explicitly; otherwise type is auto-detected by the repo server. After comparison the controller stores the detected type in `status.sourceType` (single source) or `status.sourceTypes[]` (multi-source, one per source, in order).
- **Edge cases**: More than one explicit block -> `multiple application sources defined: Kustomize,Helm` (order Kustomize, Helm, Directory, Plugin); surfaced as an API error during repo validation.
- **Configuration surface**: source blocks; `status.sourceType`, `status.sourceTypes`.
- **Exposure**: CRD status; API; CLI `app get` (Source type); UI summary.
- **Tests required**:
  - Negative: helm+kustomize blocks -> exact error on API create with validation.
  - Positive: multi-source app fills `sourceTypes` aligned with `sources`.

### APP-013 Application spec normalization
- **Upstream**: [argo.go#L1016](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L1016), [argo.go#L1035](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L1035), [types.go#L1539](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1539), [appcontroller.go#L2283](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2283), [application.go#L1397](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1397)
- **Maturity**: GA
- **Semantics**: Applied by the API server on create/update/patch and by the controller on every full reconciliation (controller persists the diff with a two-way merge patch): empty `project` -> `default`; a `syncPolicy` with no automated/syncOptions/retry/managedNamespaceMetadata -> nil; per source (or each of `sources`): zero-valued `kustomize` and `helm` blocks -> nil; `directory` keeps only include/exclude if set, else nil.
- **Edge cases**: Normalization of `spec.sourceHydrator.drySource` source-type blocks is not performed (only `source`/`sources`). Normalization patch failures are logged and do not block reconciliation.
- **Configuration surface**: none.
- **Exposure**: CRD (observable rewrite of spec); API.
- **Tests required**:
  - Positive: `syncPolicy: {}` disappears after reconcile; `helm: {}` disappears.
  - Positive: API create with `project: ""` returns `default`.

### APP-014 spec.sources (multiple sources)
- **Upstream**: [types.go#L98](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L98), [types.go#L294](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L294), [appcontroller.go#L1994](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1994), [state.go#L310](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L310), [state.go#L561](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L561), [argo.go#L665](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L665), [multiple_sources.md#L9](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/multiple_sources.md#L9), [multiple_sources.md#L42](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/multiple_sources.md#L42)
- **Maturity**: GA
- **Semantics**: Non-empty `spec.sources` (and no `sourceHydrator`) makes the app multi-source; `spec.source` is ignored. Manifests are generated per source in order and concatenated; duplicates are not filtered before dedup. If several sources produce the same group/kind/namespace/name, the last one wins and `RepeatedResourceWarning` is raised. Each source validated: `repoURL` plus one of `path`, `chart` or `ref` required; repo permitted by project. Status uses plural fields: `status.sync.revisions`, `status.sync.comparedTo.sources`, `status.sourceTypes`, history entries with `sources`/`revisions`, `operationState.syncResult.sources/revisions`. Any change to any source triggers refresh `at least one of the spec.sources differs`.
- **Edge cases**: Validation message per source: `spec.source.repoURL and either source.path, source.chart, or source.ref are required for source <source>`. Errors during generation are reported as `failed to generate manifest for source <i> of <n>: ...`. Source names must be unique (APP-017).
- **Configuration surface**: `spec.sources[]`.
- **Exposure**: CRD; API; CLI `argocd app create -f`, `add-source`, `remove-source`, `--source-position`, `--source-name`; UI summary shows each source.
- **Tests required**:
  - Positive: two sources combine resources; same resource in both -> last wins + `RepeatedResourceWarning`.
  - Negative: source missing path/chart/ref -> exact message.
  - Positive: status uses `revisions[]` aligned with sources.

### APP-015 spec.sources[].ref and $ref value-file references
- **Upstream**: [types.go#L215](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L215), [argo.go#L543](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L543), [repository.go#L566](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L566), [repository.go#L846](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L846), [multiple_sources.md#L56](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/multiple_sources.md#L56)
- **Maturity**: GA
- **Semantics**: A source with `ref: <key>` can be referenced by other sources' Helm `valueFiles` and `fileParameters[].path` as `$<key>/<path>`; `$<key>` resolves to the root of the referenced repository at that source's `targetRevision` (path field is ignored for resolution). Ref keys must match `^[a-zA-Z0-9_-]+$` and be unique. Only applied for multi-source apps. Ref sources' own revision changes are evaluated as part of the referencing source.
- **Edge cases**: Invalid key -> `sources.ref <key> cannot contain any special characters except '_' and '-'`; duplicate -> `` invalid sources: multiple sources had the same `ref` key ``; unknown `$x` with no refs -> `source referenced "$x", but no source has a 'ref' field defined`; unknown with refs -> `source referenced "$x", which is not one of the available sources ($a, $b)`; ref source with `chart` -> `source has a 'chart' field defined, but Helm charts are not yet not supported for 'ref' sources`; same repo referenced at two revisions -> `cannot reference multiple revisions for the same repository (...)`; ref to the app's own repo at a different commit -> `cannot reference a different revision of the same repository (...)`. `$values` only recognized at the start of the value file path.
- **Configuration surface**: `spec.sources[].ref`; `helm.valueFiles`, `helm.fileParameters[].path`.
- **Exposure**: CRD; API; CLI `--ref`, `argocd app unset --ref`; UI source panel.
- **Tests required**:
  - Positive: chart + `$values/charts/x/values.yaml` from a Git ref source renders.
  - Negative: each error string above.
  - Positive: commit to the ref repo triggers re-render of the referencing source.

### APP-016 Ref-only sources (no manifest generation)
- **Upstream**: [repository.go#L636](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L636), [state.go#L441](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L441), [multiple_sources.md#L81](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/multiple_sources.md#L81)
- **Maturity**: GA
- **Semantics**: In a multi-source app, a source with `ref` set, no `path`, not Helm (`chart` empty) and not OCI generates no manifests; the repo server only resolves its revision (which is still reported in `status.sync.revisions`). If `path` is set the source also generates manifests from that path.
- **Edge cases**: The same source in single-source mode (`spec.source`) is not treated as ref-only.
- **Configuration surface**: `spec.sources[].ref` without `path`.
- **Exposure**: CRD; API.
- **Tests required**:
  - Positive: ref-only source contributes zero resources but a resolved SHA in `status.sync.revisions[i]`.
  - Positive: ref source with `path` contributes resources.

### APP-017 spec.sources[].name (source names)
- **Upstream**: [types.go#L217](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L217), [application.go#L1326](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1326), [state.go#L392](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L392), [application.yaml#L211](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/application.yaml#L211)
- **Maturity**: GA
- **Semantics**: Optional display/addressing name for a source in multi-source apps; used in UI and as an alternative to 1-based source position in CLI commands; used to attribute source-integrity problems (`source <name>` instead of `source <i> of <n>`).
- **Edge cases**: API create/update rejects duplicates: `application <app> has duplicate source name: <name>` (empty names are not checked). Uniqueness is not enforced by the CRD schema or the controller (declarative duplicates are accepted).
- **Configuration surface**: `spec.sources[].name`.
- **Exposure**: CRD; API; CLI `--source-name`; UI.
- **Tests required**:
  - Negative: API create with two sources named `a` -> exact error.
  - Positive: `argocd app get --source-name a` selects the right source.

### APP-018 Source addressing in CLI (add-source, remove-source, source position)
- **Upstream**: [types.go#L301](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L301), [app.go#L179](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/util/app.go#L179), [app.go#L879](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/app.go#L879), [app.go#L3093](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/app.go#L3093), [app.go#L3157](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/app.go#L3157), [argocd_app_add-source.md#L18](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/commands/argocd_app_add-source.md#L18), [argocd_app_remove-source.md#L21](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/commands/argocd_app_remove-source.md#L21)
- **Maturity**: GA
- **Semantics**: Source positions are 1-based in the CLI (`--source-position`, default -1 = not set); `--source-name` is translated to a position. `GetSourcePtrByPosition(p)` maps to index p-1; for source-hydrator apps index -1 addresses the dry source and 0 the sync source. `argocd app set`/`unset`/`get` on a multi-source app require a position > 0 (or a name). `add-source` appends a new source built from the source flags to an app that already has `spec.sources` and saves it with `UpdateSpec` (honouring `--validate`). `remove-source` removes the source at a position or name.
- **Edge cases**: CLI fatal messages: `Only one of source-position and source-name can be specified.`; `Unknown source name '<n>'`; `Source position should be specified and must be greater than 0 for applications with multiple sources`; `Source position should be less than the number of sources in the application`; add-source on a single-source app: `Cannot add source: application <app> does not have spec.sources defined`; remove-source: `Value of source-position must be greater than 0`, `Application does not have multiple sources configured`, `Cannot remove the only source remaining in the app`, `Application does not have source at <n>`.
- **Configuration surface**: CLI flags only.
- **Exposure**: CLI `argocd app add-source`, `argocd app remove-source`, `argocd app set|unset|get --source-position|--source-name`.
- **Tests required**:
  - Positive: add-source on a 2-source app yields 3 sources, new one last.
  - Negative: add-source on a single-source app fails with the exact message.
  - Positive: remove-source by name; negative: removing the last remaining source fails.
  - Negative: `--source-name` together with `--source-position` fails.

### APP-019 spec.syncPolicy.automated (field level)
- **Upstream**: [types.go#L1518](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1518), [types.go#L1531](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1531), [types.go#L1608](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1608), [appcontroller.go#L2414](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2414), [appcontroller.go#L2855](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2855), [app.go#L222](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/util/app.go#L222), [app.go#L290](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/util/app.go#L290), [application.yaml#L229](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/application.yaml#L229)
- **Maturity**: GA
- **Semantics**: `automated` present enables auto-sync unless `enabled: false` (`enabled` nil == true). `prune` (default false), `selfHeal` (default false), `allowEmpty` (default false). Toggling auto-sync from disabled to enabled, or selfHeal from false to true, forces an immediate refresh. Auto-sync operations default to `retry.limit: 5` when `syncPolicy.retry` is nil. Detailed auto-sync/self-heal algorithm is in the sync area.
- **Edge cases**: CLI `--auto-prune/--self-heal/--allow-empty` on an app without `automated` creates `automated: {enabled: false, ...}` (configured but disabled). CLI `--sync-policy none|manual` removes `automated`; `automated|automatic|auto` sets `automated: {}`; other values -> fatal `Invalid sync-policy: <v>`. Auto-sync is skipped while an operation is in progress, while deleting, when not OutOfSync, when only pruning is needed and prune is off, and refuses `Skipping sync attempt to <revs>: auto-sync will wipe out all resources` (SyncError) when prune would delete everything and `allowEmpty` is false.
- **Configuration surface**: `spec.syncPolicy.automated.{enabled,prune,selfHeal,allowEmpty}`.
- **Exposure**: CRD; API; CLI `--sync-policy --auto-prune --self-heal --allow-empty`; UI create panel `SYNC POLICY` and summary panel toggles; resource action `toggle-auto-sync` (APP-092).
- **Tests required**:
  - Positive: `automated: {}` auto-syncs; `enabled: false` does not.
  - Positive: enabling auto-sync triggers immediate refresh.
  - Negative: prune+!allowEmpty with all resources requiring prune -> SyncError message.

### APP-020 spec.syncPolicy.syncOptions (field level)
- **Upstream**: [types.go#L1473](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1473), [types.go#L1502](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1502), [types.go#L1522](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1522), [app.go#L239](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/util/app.go#L239), [application.yaml#L234](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/application.yaml#L234)
- **Maturity**: GA
- **Semantics**: List of `Key=Value` strings applied to every sync of the app (also copied into auto-sync operations). Lookups are exact-string (`HasOption`) or by `Key=` prefix (`GetOptionValue`). App-level options that this area depends on: `Delete=false|confirm`, `Prune=false|confirm` (deletion behaviour, APP-061/APP-062), `ServerSideApply=true` (enables structured-merge diff), `FailOnSharedResource=true` (APP-077), `CreateNamespace=true` (APP-022). All other options' behaviour is owned by the sync area.
- **Edge cases**: CLI `--sync-option X` adds (no duplicates), `--sync-option !X` removes exactly `X`; removing the last item collapses `syncPolicy` to nil when otherwise empty.
- **Configuration surface**: `spec.syncPolicy.syncOptions[]`.
- **Exposure**: CRD; API; CLI `--sync-option`; UI create panel `SYNC OPTIONS` checkboxes and summary panel.
- **Tests required**:
  - Positive: `--sync-option Prune=false` then `--sync-option '!Prune=false'` leaves no option.
  - Positive: duplicates are not added.

### APP-021 spec.syncPolicy.retry
- **Upstream**: [types.go#L1544](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1544), [types.go#L1553](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1553), [types.go#L1567](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1567), [types.go#L1598](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1598), [application_defaults.go#L6](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/application_defaults.go#L6), [app.go#L255](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/util/app.go#L255), [application.yaml#L252](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/application.yaml#L252)
- **Maturity**: GA
- **Semantics**: `limit` (0 = no retries; negative = unlimited), `backoff.duration` (default `5s`; bare integer = seconds, or Go duration), `backoff.factor` (default 2), `backoff.maxDuration` (default `3m`; capped wait), `refresh` (on retry, drop the pinned revision(s) and use latest). Next retry time = `lastAttempt + min(maxDuration, duration * factor^retryCount)` (maxDuration <= 0 disables the cap). Copied into auto-sync operations; manual syncs carry their own `retryStrategy`.
- **Edge cases**: Unparseable duration -> operation `Error` with message `unable to parse <s> as a duration` (or `<msg> (failed to retry: ...)`). CLI `--sync-retry-limit 0` removes retry; negative -> fatal `Invalid sync-retry-limit [<n>]`; `--sync-retry-refresh` creates a retry block with limit 0 if none.
- **Configuration surface**: `spec.syncPolicy.retry.{limit,backoff.{duration,factor,maxDuration},refresh}`.
- **Exposure**: CRD; API; CLI `--sync-retry-limit --sync-retry-backoff-duration --sync-retry-backoff-factor --sync-retry-backoff-max-duration --sync-retry-refresh`; UI create panel `RETRY` options.
- **Tests required**:
  - Positive: backoff sequence 5s,10s,20s,... capped at 3m.
  - Positive: `duration: "30"` interpreted as 30s.
  - Negative: `duration: "abc"` -> operation Error with exact message.

### APP-022 spec.syncPolicy.managedNamespaceMetadata
- **Upstream**: [types.go#L1512](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1512), [types.go#L3699](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L3699), [state.go#L604](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L604), [state.go#L828](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L828), [state.go#L1036](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L1036), [application.yaml#L243](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/application.yaml#L243)
- **Maturity**: GA
- **Semantics**: `labels` and `annotations` applied to the destination namespace when `CreateNamespace=true`. The last-applied value is recorded in `status.operationState.syncResult.managedNamespaceMetadata`; when spec differs from it the app is reported `OutOfSync` and a refresh is triggered (`spec.syncPolicy.managedNamespaceMetadata differs`). A managed namespace that exists live but not in Git is compared against a synthesized Namespace so it is neither pruned nor OutOfSync; a Namespace defined in Git takes precedence.
- **Edge cases**: No-op unless `CreateNamespace=true` (per docs). Namespace application semantics are in the sync area.
- **Configuration surface**: `spec.syncPolicy.managedNamespaceMetadata.{labels,annotations}`.
- **Exposure**: CRD; API; UI (YAML edit); no dedicated CLI flag found.
- **Tests required**:
  - Positive: changing labels makes app OutOfSync until synced.
  - Positive: managed namespace not in Git is not pruned.

### APP-023 spec.ignoreDifferences (field level)
- **Upstream**: [types.go#L88](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L88), [types.go#L107](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L107), [types.go#L124](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L124), [appcontroller.go#L2231](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2231), [application.yaml#L260](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/application.yaml#L260)
- **Maturity**: GA
- **Semantics**: List of `{group, kind (required), name, namespace, jsonPointers[], jqPathExpressions[], managedFieldsManagers[]}`. Copied into `status.sync.comparedTo.ignoreDifferences`; a change (nil and empty treated equal) triggers refresh `spec.ignoreDifferences differs`. Diff semantics are owned by the diff area.
- **Edge cases**: `name`/`namespace` are exact matches (not globs) per docs; `group`/`kind` may be `*`.
- **Configuration surface**: `spec.ignoreDifferences[]`.
- **Exposure**: CRD; API; UI (YAML edit); no dedicated CLI flag found.
- **Tests required**:
  - Positive: editing ignoreDifferences triggers refresh and updates `comparedTo`.

### APP-024 spec.info (extra information)
- **Upstream**: [types.go#L89](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L89), [types.go#L1468](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1468), [application-crd.yaml#L1050](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/crds/application-crd.yaml#L1050), [application-summary.tsx#L636](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-summary/application-summary.tsx#L636), [extra_info.md#L6](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/extra_info.md#L6)
- **Maturity**: GA
- **Semantics**: Ordered list of `{name, value}` (both required by schema) shown on the app details summary `INFO` section; purely informational (no controller semantics). The UI summary panel allows adding, removing and reordering entries.
- **Edge cases**: Missing `name` or `value` is rejected by the CRD schema. No CLI flag sets `spec.info` (`argocd app sync --info` sets `operation.info`, a different field).
- **Configuration surface**: `spec.info[]`.
- **Exposure**: CRD; API; UI summary `INFO` (editable); CLI: none.
- **Tests required**:
  - Positive: info entries displayed in order; reorder in UI persists.
  - Negative: entry without value rejected by schema.

### APP-025 spec.revisionHistoryLimit and status.history
- **Upstream**: [types.go#L91](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L91), [types.go#L3836](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L3836), [application_defaults.go#L28](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/application_defaults.go#L28), [types.go#L1385](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1385), [types.go#L1833](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1833), [state.go#L1194](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L1194), [sync.go#L408](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sync.go#L408), [app.go#L2598](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/app.go#L2598)
- **Maturity**: GA
- **Semantics**: Default limit 10; 0 stores no history. History (`status.history`, oldest first) gets one entry per successful, non-dry-run, non-partial (no `resources` filter) sync: `id` (previous last id + 1, starting at 0), `revision`/`source` (single) or `revisions`/`sources` (multi), `deployedAt` (completion, UTC), `deployStartedAt`, `initiatedBy`. After appending, the list is truncated to the newest N entries. Persisted via merge patch of `status.history`.
- **Edge cases**: Failure to persist history turns the operation into `Error` with `failed to record sync to history: <err>`. Negative limits behave as 0 (`Trunc` clamps). IDs are not reused after truncation.
- **Configuration surface**: `spec.revisionHistoryLimit`.
- **Exposure**: CRD; API; CLI `--revision-history-limit`, `argocd app history` (columns `SOURCE`, `ID`, `DATE`, `REVISION`; `-o id`); UI summary `REVISION HISTORY LIMIT` (editable), `History and rollback` panel.
- **Tests required**:
  - Positive: 12 successful syncs with limit 10 keep ids 2..11.
  - Positive: limit 0 -> empty history.
  - Negative: partial sync (`--resource`) and dry-run add no history.

### APP-026 spec.sourceHydrator schema and validation
- **Upstream**: [types.go#L101](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L101), [types.go#L428](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L428), [types.go#L470](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L470), [types.go#L503](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L503), [types.go#L522](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L522), [application-crd.yaml#L1851](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/crds/application-crd.yaml#L1851), [argo.go#L618](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L618), [argo.go#L646](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L646), [feature-maturity.md#L37](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/feature-maturity.md#L37), [source-hydrator.md#L96](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/source-hydrator.md#L96)
- **Maturity**: Beta
- **Semantics**: `drySource{repoURL, targetRevision, path (all CRD-required), helm, kustomize, directory, plugin}`; `syncSource{targetBranch, path, repoURL}` (`targetBranch` required; `path` required with minLength 1 and pattern `^.{2,}|[^./]$`; `repoURL` optional, defaults to `drySource.repoURL`); `hydrateTo{targetBranch}` (optional; inherits repo and path from syncSource). When set, `GetSource()`/`GetSources()` return the sync source `{repoURL, path, targetRevision: targetBranch}`; `source`/`sources` are ignored and the app is not multi-source. Controller/API validation: `spec.sourceHydrator.drySource.repoURL is required`, `spec.sourceHydrator.syncSource.targetBranch is required`, `when spec.sourceHydrator.hydrateTo is set, spec.sourceHydrator.hydrateTo.targetBranch is required` (all `InvalidSpecError`); dry repo must be permitted by the project (`application repo <url> is not permitted in project '<p>'`), and a distinct sync repo too (`sync source repo <url> is not permitted in project '<p>'`).
- **Edge cases**: The CRD path pattern rejects `.`, `/` and single-character `.`/`/` paths but allows two-character paths such as `./`; root paths are additionally rejected at hydration time (APP-117). Repo validation on create uses the dry source (the sync branch may not exist yet).
- **Configuration surface**: `spec.sourceHydrator.*`.
- **Exposure**: CRD; API; CLI (APP-138); UI (APP-139).
- **Tests required**:
  - Negative: each validation message above.
  - Negative: `syncSource.path: "."` rejected by CRD schema.
  - Positive: omitted `syncSource.repoURL` resolves to dry repo in `GetSource()`.

### APP-027 status.sync and sync status codes
- **Upstream**: [types.go#L1880](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1880), [types.go#L1930](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1930), [types.go#L1942](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1942), [state.go#L637](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L637), [state.go#L927](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L927), [state.go#L980](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L980), [state.go#L1034](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L1034), [appcontroller.go#L1964](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1964), [application-crd.yaml#L6209](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/crds/application-crd.yaml#L6209)
- **Maturity**: GA
- **Semantics**: `status.sync = {status (required), comparedTo{source, sources, destination (required), ignoreDifferences}, revision, revisions[]}`. Codes: `Synced`, `OutOfSync`, `Unknown`. Starts `Synced`; a managed resource that is modified, missing live, or extra live makes the app `OutOfSync`, except: hooks, ignored resources (hook annotation without type), targets annotated `Skip`, live objects whose tracking id does not reference themselves, and extraneous live resources annotated `argocd.argoproj.io/compare-options: IgnoreExtraneous` (resource itself is still marked OutOfSync). A managed Namespace synthesized from `managedNamespaceMetadata` is not OutOfSync when only absent. If target or live state could not be loaded the app (and every resource) is `Unknown`; a changed `managedNamespaceMetadata` forces `OutOfSync`. When `refreshAppConditions` finds spec/project errors, sync and health are set to `Unknown` and the cached tree/managed resources are emptied. `revision`/`revisions` hold resolved SHAs (manifest revisions) after comparison; `comparedTo` records exactly what was compared (single: `source`; multi: `sources`).
- **Edge cases**: Resources whose group/kind/name is not permitted by the project get resource-level `Unknown`. Transient repo errors within the grace period skip the status update entirely (APP-086). Comparison results persist only if the status patch changes something.
- **Configuration surface**: none (computed).
- **Exposure**: CRD status (printer columns); API Get/List/Watch; CLI `argocd app get` (`Sync Status`), `argocd app list` (`STATUS`); UI status panel `SYNC STATUS`, list tiles.
- **Tests required**:
  - Positive: modify a live Deployment -> app `OutOfSync`; revert -> `Synced`.
  - Positive: extra resource with IgnoreExtraneous does not flip app to OutOfSync.
  - Negative: repo unreachable beyond grace period -> `Unknown` + `ComparisonError`.
  - Positive: multi-source app records `revisions[]` and `comparedTo.sources`.

### APP-028 status.health and health status codes
- **Upstream**: [types.go#L1954](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1954), [types.go#L1966](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1966), [health.go#L18](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/health/health.go#L18), [health.go#L45](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/health/health.go#L45), [appcontroller.go#L2334](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2334), [application-crd.yaml#L2374](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/crds/application-crd.yaml#L2374)
- **Maturity**: GA
- **Semantics**: `status.health = {status, message (deprecated, unused), lastTransitionTime}`. Codes: `Healthy`, `Suspended`, `Progressing`, `Missing`, `Degraded`, `Unknown`; severity order (best -> worst) Healthy < Suspended < Progressing < Missing < Degraded < Unknown. `lastTransitionTime` is set to now only when the health code changes (single place, during status persistence), otherwise preserved; an event `Updated health status: <old> -> <new>` is emitted. Resource-level `HealthStatus{status, message, lastTransitionTime (deprecated)}`.
- **Edge cases**: Spec/project errors force `Unknown`. A live object with `deletionTimestamp` and no hook finalizer reports `Progressing` / `Pending deletion`.
- **Configuration surface**: none (computed); resource health customizations live in another area.
- **Exposure**: CRD status (printer column); API; CLI `app get` (`Health Status`), `app list` (`HEALTH`); UI status panel `APP HEALTH`.
- **Tests required**:
  - Positive: health transition updates `lastTransitionTime` and emits an event; unchanged health keeps it.
  - Positive: ordering — Degraded child beats Progressing child.

### APP-029 Application health aggregation rules
- **Upstream**: [health.go#L20](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/health.go#L20), [health.go#L41](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/health.go#L41), [health.go#L52](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/health.go#L52), [health.go#L76](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/health.go#L76), [health.go#L81](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/health.go#L81), [health.go#L91](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/health.go#L91), [health.go#L100](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/health.go#L100), [state.go#L1051](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L1051)
- **Maturity**: GA
- **Semantics**: Start at `Healthy`; for each managed resource: skip targets marked hook `Skip`, live hooks and ignored resources, live resources annotated `argocd.argoproj.io/ignore-healthcheck: "true"`, and the Application itself (self-managed app, matched by UID/name/namespace/kind). Resources with no health check (nil) are skipped. A missing live resource gets resource health `Missing` but does not lower app health. A child `argoproj.io/Application` whose health is `Missing` or `Unknown` does not affect the parent. Otherwise the worst code wins. Finally, if the app has at least one (non-hook) resource but none exists live, app health becomes `Missing`.
- **Edge cases**: Health-evaluation errors (e.g. Lua errors) add `ComparisonError` `error setting app health: failed to get resource health for "<kind>" with name "<n>" in namespace "<ns>": ...`; with >1 error the message is `see application-controller logs for <n> other errors; most recent error was: ...`. An app with zero resources is `Healthy`.
- **Configuration surface**: `argocd.argoproj.io/ignore-healthcheck` annotation; resource health customizations (other area).
- **Exposure**: CRD status; API; CLI; UI.
- **Tests required**:
  - Positive: app with only missing resources -> `Missing`; app with no resources -> `Healthy`.
  - Positive: Degraded child annotated ignore-healthcheck -> app Healthy.
  - Positive: child Application reporting Unknown does not degrade the parent.
  - Negative: broken Lua health script -> ComparisonError with exact prefix.

### APP-030 status.resources (ResourceStatus)
- **Upstream**: [types.go#L2231](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2231), [state.go#L943](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L943), [appcontroller.go#L2067](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2067), [application-crd.yaml#L5174](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/crds/application-crd.yaml#L5174)
- **Maturity**: GA
- **Semantics**: One entry per managed (target or live) resource: `group, version, kind, namespace, name, status (Synced|OutOfSync|Unknown|"" for hooks/ignored/non-self-referenced), health (APP-031), hook (bool), requiresPruning (no target, live exists, tracking self-referenced), syncWave (target's wave, or the hook's wave for live hooks), requiresDeletionConfirmation (APP-062)`. List sorted by `group/kind/namespace/name`.
- **Edge cases**: Generated-name targets get a synthetic key (`<generateName><index>`) for dedup. Status-size overflow falls back to an error condition (APP-087).
- **Configuration surface**: none.
- **Exposure**: CRD status; API (`ManagedResources`/`ResourceTree` are other areas); CLI `argocd app get` resource table; UI tree/list views.
- **Tests required**:
  - Positive: resources ordered deterministically; extra live resource has `requiresPruning: true`.
  - Positive: hook resources have `hook: true` and no status.

### APP-031 Resource health persistence location (inline vs appTree)
- **Upstream**: [types.go#L1205](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1205), [health.go#L68](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/health.go#L68), [health.go#L95](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/health.go#L95), [application.go#L2905](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2905), [argocd_application_controller.go#L293](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-application-controller/commands/argocd_application_controller.go#L293), [argocd-cmd-params-cm.yaml#L84](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cmd-params-cm.yaml#L84)
- **Maturity**: GA
- **Semantics**: Controller flag `--persist-resource-health` (env `ARGOCD_APPLICATION_CONTROLLER_PERSIST_RESOURCE_HEALTH`, cmd-params key `controller.resource.health.persist`, default `false`). When false, `status.resources[].health` is nil and `status.resourceHealthSource: appTree`; the API server re-populates resource health in Get and Watch responses (not in List) from the cached resource tree (Sync and Rollback fill the Application they read but return the one `SetAppOperation` writes, which has none), using `Missing` / `Resource has not been created` for nodes never created. When true, health is stored inline and `resourceHealthSource` is empty (inline).
- **Edge cases**: Clients reading the CR directly (kubectl) see no resource health in appTree mode. Cache miss -> no health filled.
- **Configuration surface**: flag/env/cmd-params key above; `status.resourceHealthSource`.
- **Exposure**: CRD status; API responses; UI.
- **Tests required**:
  - Positive: default mode -> CR has no resource health but API Get/Watch return it; List does not.
  - Positive: persist=true -> CR contains health, `resourceHealthSource` empty.

### APP-032 status.conditions mechanics
- **Upstream**: [types.go#L1892](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1892), [types.go#L1920](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1920), [types.go#L3712](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L3712), [types.go#L3769](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L3769), [appcontroller.go#L1571](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1571), [appcontroller.go#L2712](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2712)
- **Maturity**: GA
- **Semantics**: Condition `{type, message (required), lastTransitionTime}`. Exactly nine types exist (APP-033..APP-041). `IsError()` = type ends with `Error`. Each producer evaluates a fixed set of types and `SetConditions` replaces only those: conditions of non-evaluated types are kept; an evaluated type that is no longer produced is removed; an unchanged type+message keeps its original `lastTransitionTime`. Result sorted by `type/message/time`. Owners: refreshAppConditions -> {InvalidSpecError, UnknownError}; CompareAppState -> {ComparisonError, SharedResourceWarning, RepeatedResourceWarning, ExcludedResourceWarning}; resource tree -> {OrphanedResourceWarning}; auto-sync -> {SyncError}; deletion -> {DeletionError}. The informer namespace indexer may also add `InvalidSpecError`/`UnknownError` directly (project error or invalid destination) via a merge patch of `status.conditions`, skipping identical existing conditions.
- **Edge cases**: The sync operation refuses to run while `ComparisonError` or `InvalidSpecError` exists (operation `Error`, message = formatted conditions).
- **Configuration surface**: none.
- **Exposure**: CRD status; API; CLI `argocd app list` `CONDITIONS` column, `argocd app get` conditions table; UI status panel condition counts + Conditions panel.
- **Tests required**:
  - Positive: warning persists across unrelated evaluations; re-evaluation without the warning removes it.
  - Positive: unchanged message keeps `lastTransitionTime`.
  - Negative: sync while `InvalidSpecError` present -> operation Error.

### APP-033 Condition InvalidSpecError
- **Upstream**: [types.go#L1901](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1901), [argo.go#L642](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L642), [appcontroller.go#L2833](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2833), [appcontroller.go#L2723](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2723), [state.go#L1007](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L1007)
- **Maturity**: GA
- **Semantics**: Application source/destination/project spec is invalid. Produced by `ValidatePermissions` (source/sources/hydrator required-field messages, repo not permitted, destination resolution errors, destination not permitted), by project-not-found (`Application referencing project <p> which does not exist`), by invalid destination at index time, and by `Namespace for <name> <gvk> is missing.` during comparison. API create/update converts these into `InvalidArgument: application spec for <app> is invalid: <formatted conditions>`.
- **Edge cases**: When present the app shows sync/health `Unknown` (if from refreshAppConditions) and sync is blocked.
- **Configuration surface**: none.
- **Exposure**: CRD status; API error strings; CLI; UI conditions.
- **Tests required**:
  - Negative: each producing message is reproducible and clears after the fix.

### APP-034 Condition ComparisonError
- **Upstream**: [types.go#L1903](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1903), [state.go#L698](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L698), [state.go#L720](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L720), [state.go#L777](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L777), [state.go#L922](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L922), [state.go#L1058](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L1058)
- **Maturity**: GA
- **Semantics**: Controller failed to compare state. Messages: `Failed to load target state: <err>`; `Cannot use local manifests when source integrity is enforced`; `Failed to load local manifests: <err>`; `Failed to normalize target state: <err>`; `Failed to load live state: <err>` (e.g. namespaced-mode clusters: `cluster level <Kind> "<name>" can not be managed when in namespaced mode`, `namespace "<ns>" for <Kind> "<name>" is not managed`); `Failed to check if live resource "<key>" is permitted in project "<p>": <err>`; `Failed to compare desired state to live state: <err>`; `error setting app health: <err>`; source-integrity verification failures; managed-namespace synthesis errors. Any load failure marks the app and all resources `Unknown`.
- **Edge cases**: Repo errors are ignored for the grace period before the condition appears (APP-086); manual/hard refreshes bypass the grace period.
- **Configuration surface**: none.
- **Exposure**: CRD status; API; CLI; UI.
- **Tests required**:
  - Negative: invalid path in repo -> `Failed to load target state:` prefix and sync `Unknown`.
  - Recovery: fixing the repo clears the condition on next refresh.

### APP-035 Condition SyncError
- **Upstream**: [types.go#L1905](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1905), [appcontroller.go#L2042](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2042), [appcontroller.go#L2490](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2490), [appcontroller.go#L2531](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2531), [appcontroller.go#L2552](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2552)
- **Maturity**: GA
- **Semantics**: Controller failed to start an automatic sync. Messages: `Failed last sync attempt to <revs>: <operation message>` (same revisions/sources already failed), `Skipping sync attempt to <revs>: auto-sync will wipe out all resources`, or the error from setting the operation. Evaluated only when the sync window allows syncing; cleared on the next auto-sync evaluation without error. When a sync window blocks, SyncError is left untouched (log `Sync prevented by sync window`).
- **Edge cases**: `another operation is already in progress` from a stale informer is swallowed (no condition).
- **Configuration surface**: `spec.syncPolicy.automated`.
- **Exposure**: CRD status; API; CLI; UI.
- **Tests required**:
  - Negative: failing auto-sync to revision X produces the exact `Failed last sync attempt` message and no retry for X.
  - Recovery: new commit clears SyncError and auto-syncs.

### APP-036 Condition UnknownError
- **Upstream**: [types.go#L1907](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1907), [appcontroller.go#L2259](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2259), [appcontroller.go#L2839](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2839), [appcontroller.go#L2370](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2370), [state.go#L891](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L891)
- **Maturity**: GA
- **Semantics**: Unknown controller error: project lookup errors other than NotFound (including `application '<a>' in namespace '<ns>' is not permitted to use project '<p>'`), `ValidatePermissions` internal errors, GVK parser / server-side-diff dry-run applier errors, and the status-size fallback message (APP-087). Together with InvalidSpecError it forces sync/health `Unknown` when produced by refreshAppConditions.
- **Edge cases**: UnknownError from compare (GVK parser) does not set Unknown sync by itself.
- **Configuration surface**: none.
- **Exposure**: CRD status; API; CLI; UI.
- **Tests required**:
  - Negative: app in a namespace not allowed by its project -> UnknownError exact message, sync/health Unknown.

### APP-037 Condition DeletionError
- **Upstream**: [types.go#L1899](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1899), [appcontroller.go#L1141](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1141), [appcontroller.go#L1154](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1154), [sync-waves.md#L69](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/sync-waves.md#L69)
- **Maturity**: GA
- **Semantics**: Set when `finalizeApplicationDeletion` returns an error (message = the error, e.g. `error executing pre-delete hooks: PreDelete hook(s) failed: ns/name`); also emits a Warning event `Unable to delete application resources: <err>`. Cleared when a later deletion pass succeeds (in-memory `SetConditions`).
- **Edge cases**: The clear is performed only in memory and is not explicitly patched; it disappears when another status persist happens (see Unconfirmed).
- **Configuration surface**: none.
- **Exposure**: CRD status; UI conditions; CLI `app get`.
- **Tests required**:
  - Negative: failing PreDelete hook -> DeletionError with hook names.
  - Recovery: fixing the hook allows deletion to finish.

### APP-038 Condition SharedResourceWarning
- **Upstream**: [types.go#L1909](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1909), [state.go#L804](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L804)
- **Maturity**: GA
- **Semantics**: For each live object selected for this app whose tracking value names a different app instance, add `<Kind>/<name> is part of applications <this ns/name> and <other ns/name>` (other instance name converted from `ns_name` to `ns/name`). See APP-077 for sync-time enforcement.
- **Edge cases**: One condition per shared object (multiple entries possible). With label tracking, any foreign instance label value triggers it.
- **Configuration surface**: tracking settings (APP-070..APP-074).
- **Exposure**: CRD status; UI; CLI.
- **Tests required**:
  - Positive: two apps rendering the same ConfigMap -> warning on the app that does not own the tracking id.

### APP-039 Condition RepeatedResourceWarning
- **Upstream**: [types.go#L1911](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1911), [state.go#L561](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L561)
- **Maturity**: GA
- **Semantics**: Target manifests contain the same group/kind/namespace/name more than once (within one source or across sources): `Resource <key> appeared <n> times among application resources.`; only the last occurrence is kept.
- **Edge cases**: Keys are computed after namespace defaulting, so a namespaced resource with and without explicit destination namespace collide.
- **Configuration surface**: none.
- **Exposure**: CRD status; UI; CLI.
- **Tests required**:
  - Positive: duplicate ConfigMap in two sources -> warning, last definition applied.

### APP-040 Condition ExcludedResourceWarning
- **Upstream**: [types.go#L1913](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1913), [state.go#L753](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L753)
- **Maturity**: GA
- **Semantics**: A target resource matches `resource.exclusions` / not `resource.inclusions` for the destination cluster: it is dropped from targets and `Resource <group>/<kind> <name> is excluded in the settings` is added.
- **Edge cases**: Exclusion is evaluated per destination server.
- **Configuration surface**: `argocd-cm` `resource.exclusions`/`resource.inclusions` (settings area).
- **Exposure**: CRD status; UI; CLI.
- **Tests required**:
  - Positive: excluded kind in Git -> warning and resource ignored.

### APP-041 Condition OrphanedResourceWarning
- **Upstream**: [types.go#L1915](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1915), [appcontroller.go#L544](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L544), [appcontroller.go#L584](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L584), [appcontroller.go#L681](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L681), [types.go#L2806](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2806)
- **Maturity**: GA
- **Semantics**: Only when the AppProject has `orphanedResources` set and `orphanedResources.warn: true`: top-level resources in the destination namespace that are not managed by this app, not owned by another known app, permitted by the project, and not excluded produce `Application has <n> orphaned resources`. Built-in exclusions: `default/Service kubernetes`, ServiceAccount `default`, ConfigMap `kube-root-ca.crt`; plus project `orphanedResources.ignore[]` (glob on group/kind/name).
- **Edge cases**: `orphanedResources: {}` lists orphans in the tree but does not warn (warn nil == false).
- **Configuration surface**: AppProject `spec.orphanedResources.{warn,ignore}`.
- **Exposure**: CRD status; UI (orphaned resources toggle); CLI.
- **Tests required**:
  - Positive: warn=true with one stray ConfigMap -> `Application has 1 orphaned resources`.
  - Negative: warn unset -> no condition.

### APP-042 status.reconciledAt and status.observedAt
- **Upstream**: [types.go#L1225](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1225), [types.go#L1228](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1228), [appcontroller.go#L2062](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2062), [types.go#L3660](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L3660)
- **Maturity**: GA (`observedAt`: Deprecated)
- **Semantics**: `reconciledAt` is set to now on a full reconciliation when it is nil or the comparison level is "latest" (periodic, spec-change, user refresh, post-sync), not on resource-event-driven ("recent") comparisons nor on tree-only refreshes. It drives periodic expiry (APP-081/APP-083). `observedAt` is deprecated and never written.
- **Edge cases**: `reconciledAt` nil means "never reconciled" -> always soft-expired.
- **Configuration surface**: none.
- **Exposure**: CRD status; API; UI (last refresh shown).
- **Tests required**:
  - Positive: managed-resource change does not bump `reconciledAt`; explicit refresh does.

### APP-043 status.summary (externalURLs, images, isAppOfApps)
- **Upstream**: [types.go#L2107](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2107), [types.go#L2140](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2140), [appcontroller.go#L1946](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1946), [appcontroller.go#L2039](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2039), [application-summary.tsx#L278](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-summary/application-summary.tsx#L278)
- **Maturity**: GA
- **Semantics**: Computed from the resource tree: `externalURLs` = union of `networkingInfo.externalURLs` of all tree nodes (ingress/gateway-derived and validated `link.argocd.argoproj.io/*` annotations on resources) plus the values of `link.argocd.argoproj.io/*` annotations on the Application itself; `images` = union of container images of all nodes; `isAppOfApps` = any node is `argoproj.io/Application`. URLs and images de-duplicated and sorted. Updated on full reconciles and on tree-only refreshes.
- **Edge cases**: Application-level link annotation values are copied verbatim (including `title|url` and without URL-scheme validation); the UI filters out non-http(s) entries before rendering.
- **Configuration surface**: link annotations (APP-100), ignore-default-links (APP-101).
- **Exposure**: CRD status; API; UI summary `URLS`, `IMAGES`, list tile external-link icon.
- **Tests required**:
  - Positive: Ingress host appears in `externalURLs`; app annotation `link.argocd.argoproj.io/x: https://a` appears.
  - Positive: app containing child Applications has `isAppOfApps: true`.

### APP-044 status.controllerNamespace
- **Upstream**: [types.go#L1240](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1240), [appcontroller.go#L2073](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2073)
- **Maturity**: GA
- **Semantics**: Set to the application controller's namespace on every full reconciliation.
- **Edge cases**: Not set on tree-only refreshes or when spec errors short-circuit reconciliation.
- **Configuration surface**: controller namespace.
- **Exposure**: CRD status; API.
- **Tests required**:
  - Positive: after first reconcile `status.controllerNamespace` equals the control-plane namespace.

### APP-045 operation field lifecycle
- **Upstream**: [types.go#L73](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L73), [types.go#L1340](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1340), [types.go#L1348](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1348), [types.go#L1416](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1416), [argo.go#L939](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L939), [argo.go#L40](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L40), [application.go#L382](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L382), [appcontroller.go#L1752](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1752), [application-crd.yaml#L57](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/crds/application-crd.yaml#L57)
- **Maturity**: GA
- **Semantics**: `operation = {sync{revision, revisions[], prune, dryRun, syncStrategy{apply{force}|hook{force}}, resources[]{group,kind,name,namespace}, source, sources, manifests[], syncOptions[], autoHealAttemptsCount}, initiatedBy{username, automated}, info[]{name,value}, retry{limit, backoff, refresh}}`. Setting an operation (Sync/Rollback API, auto-sync) uses `SetAppOperation`: fails with `FailedPrecondition: another operation is already in progress` if `operation` is non-nil; otherwise sets `operation`, clears `status.operationState`, and updates the object, retrying on conflicts; an operation without `sync` returns `InvalidArgument: Operation unspecified` (upstream performs this check after the update call, so the sync-less operation has already been written). The controller re-reads the app from the API (not the informer) before processing. When the operation reaches a completed phase the controller JSON-patches `status.operationState` and sets `/operation` to null in the same patch. The API server's Create strips any `operation` supplied by the client (logged as a security event); Update/Patch only copy spec/labels/annotations/finalizers, so they cannot set `operation`.
- **Edge cases**: Declaratively applied `operation` (kubectl) is processed by the controller. `autoHealAttemptsCount` is the JSON name of `SelfHealAttemptsCount`.
- **Configuration surface**: none.
- **Exposure**: CRD root field; API `Sync`/`Rollback`/`TerminateOperation` (sync area); CLI `argocd app get --show-operation`; UI operation panel.
- **Tests required**:
  - Negative: second Sync while one runs -> `another operation is already in progress`.
  - Negative (security): API Create with `operation` set persists app without operation.
  - Positive: completed operation leaves `operation` null and `operationState` populated.

### APP-046 status.operationState
- **Upstream**: [types.go#L1451](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1451), [types.go#L1757](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1757), [types.go#L1773](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1773), [types.go#L77](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/common/types.go#L77), [sync.go#L71](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sync.go#L71), [appcontroller.go#L1752](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1752), [application-crd.yaml#L3244](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/crds/application-crd.yaml#L3244)
- **Maturity**: GA
- **Semantics**: `{operation (copy of requested op), phase (Running|Terminating|Failed|Error|Succeeded), message, syncResult{resources[], revision, revisions[], source, sources, managedNamespaceMetadata}, startedAt, finishedAt, retryCount}`. `ResourceResult = {group, version, kind, namespace, name, status (Synced|SyncFailed|Pruned|PruneSkipped), message, hookType (PreSync|Sync|PostSync|Skip|SyncFail), hookPhase, syncPhase (PreSync|Sync|PostSync|SyncFail), images[]}`. New operations start `Running` with `startedAt=now`. On any completed phase `finishedAt=now`; the whole `operationState` is replaced via JSON patch. A completed operation emits event `OperationCompleted`: `Sync operation to <rev> succeeded` / `Partial sync operation ... failed: <msg>`; non-dry-run completions trigger a refresh (force-resolve for manual syncs, latest for automated).
- **Edge cases**: A phase of "" panics (`no phase was set`); a panic inside the operation sets `Error` with the panic text. Project load failure -> `Error` `Failed to load application project: <err>`. NotFound during patch evicts the app from the informer.
- **Configuration surface**: none.
- **Exposure**: CRD status; API; CLI `argocd app get --show-operation`, `app wait`; UI `Sync Status` panel.
- **Tests required**:
  - Positive: successful sync -> `Succeeded`, `finishedAt` set, history appended.
  - Negative: failed sync -> `Failed` with per-resource `SyncFailed` result.

### APP-047 Operation retry handling
- **Upstream**: [appcontroller.go#L1632](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1632), [appcontroller.go#L1649](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1649), [appcontroller.go#L1703](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1703), [appcontroller.go#L2475](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2475)
- **Maturity**: GA
- **Semantics**: When a non-terminating operation ends `Failed`/`Error` and `retryCount < retry.limit` (or limit < 0), the controller keeps phase `Running`, sets `finishedAt=now`, increments `retryCount` and sets message `<msg>. Retrying attempt #<n> at <h:mmPM>.`. A Running state with `finishedAt` set means "waiting for retry": before `NextRetryAt` the app is re-queued for later; afterwards `finishedAt`/`syncResult` are cleared, message `Retrying operation[ with latest revisions]. Attempt #<n>`, and when `retry.refresh` is true the pinned revision(s) are dropped. When retries are exhausted the message gets ` (retried <n> times).`. Auto-sync operations default to limit 5.
- **Edge cases**: Retry time calculation error -> phase `Error` with the parse error. Terminated operations are never retried.
- **Configuration surface**: `spec.syncPolicy.retry`, sync request `retryStrategy`.
- **Exposure**: CRD status; API; CLI `--sync-retry-*`; UI retry options.
- **Tests required**:
  - Positive: limit 2 failing sync -> 2 retries then Failed with `(retried 2 times).`.
  - Positive: `refresh: true` retry picks up a new commit.

### APP-048 Operation termination and controller sync timeout
- **Upstream**: [application.go#L2508](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2508), [application.proto#L483](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.proto#L483), [appcontroller.go#L1624](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1624), [appcontroller.go#L1690](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1690), [argocd_application_controller.go#L280](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-application-controller/commands/argocd_application_controller.go#L280), [argocd_app_terminate-op.md#L11](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/commands/argocd_app_terminate-op.md#L11)
- **Maturity**: GA
- **Semantics**: `TerminateOperation` (`DELETE /api/v1/applications/{name}/operation`, RBAC action `sync`) sets `status.operationState.phase = Terminating` with up to 10 conflict retries; the controller resumes the operation in Terminating mode to clean up. If the controller finds Terminating after a Running sync step, it keeps Terminating (`operation is terminating`). Controller `--sync-timeout` (env `ARGOCD_APPLICATION_CONTROLLER_SYNC_TIMEOUT`, default 0 = disabled) marks operations older than the timeout `Terminating` with `operation is terminating due to timeout`, and the final message gets `, triggered by controller sync timeout`.
- **Edge cases**: No operation -> `InvalidArgument: Unable to terminate operation. No operation is in progress`; conflicts exhausted -> `Internal: Failed to terminate app. Too many conflicts`.
- **Configuration surface**: `--sync-timeout`.
- **Exposure**: API; CLI `argocd app terminate-op`; UI operation panel `Terminate`.
- **Tests required**:
  - Positive: terminate a long PreSync hook -> phase Failed/terminated and hook cleaned.
  - Negative: terminate with no operation -> exact error.
  - Positive: sync-timeout 5s on a hanging sync -> terminated with timeout message.

### APP-049 Create Application via API
- **Upstream**: [application.go#L348](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L348), [application.go#L361](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L361), [application.go#L378](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L378), [application.go#L397](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L397), [application.go#L994](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L994), [application.proto#L372](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.proto#L372), [application.proto#L101](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.proto#L101), [app.go#L114](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/app.go#L114), [argocd_app_create.md#L36](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/commands/argocd_app_create.md#L36)
- **Maturity**: GA
- **Semantics**: `ApplicationService.Create` (`POST /api/v1/applications`, body = Application; request fields `upsert`, `validate`). Steps: RBAC `applications, create, <rbacName>`; read-lock the project; `validate` defaults to true; resolve the project (vague error, APP-002); `validateAndNormalizeApp` (APP-054); reject namespaces not enabled (`namespace '<ns>' is not permitted`); drop any client-supplied `operation`; create; emit event `ResourceCreated` ("created application"); wait up to 2s for the informer to observe the new resourceVersion. If the object already exists: equal `spec`, labels, annotations and finalizers -> return existing (idempotent); otherwise without `upsert` -> `InvalidArgument: existing application spec is different, use upsert flag to force update`; with `upsert` -> requires RBAC `update` and updates with label/annotation merge (finalizers replaced).
- **Edge cases**: Nil application -> `error creating application: application is nil in request`. Empty name -> `resource name may not be empty`. Declarative `kubectl apply` bypasses all API-side validation (only schema applies). CLI `argocd app create` accepts `ns/name`, `-f` (file/URL, may contain several apps), `--upsert`, `--validate`, `--set-finalizer` (appends `resources-finalizer.argocd.argoproj.io`), `-N`; it pre-fetches the app and prints `application '<name>' created|updated|unchanged`.
- **Configuration surface**: request fields `upsert`, `validate`.
- **Exposure**: CRD (kubectl create/apply); API `POST /api/v1/applications`; CLI `argocd app create`; UI `+ NEW APP` create panel (form and YAML editor, `SET DELETION FINALIZER` checkbox).
- **Tests required**:
  - Positive: create, then identical create returns existing without error.
  - Negative: differing spec without upsert -> exact InvalidArgument; with upsert but no `update` permission -> PermissionDenied.
  - Negative (authorization): no `create` permission -> PermissionDenied.
  - Security: `operation` in request is not persisted.
  - Negative: namespace not enabled -> `namespace '<ns>' is not permitted`.

### APP-050 Update Application via API
- **Upstream**: [application.go#L1051](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1051), [application.go#L964](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L964), [application.go#L1018](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1018), [application.proto#L418](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.proto#L418), [application.proto#L107](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.proto#L107)
- **Maturity**: GA
- **Semantics**: `PUT /api/v1/applications/{application.metadata.name}` with body Application and optional `validate` (default true) and `project` (expected current project). RBAC `update` on the new object's RBAC name, then the stored app is fetched with `update` enforcement and project matching (APP-053). After validation/normalization, the server copies only `spec`, `labels`, `annotations` (replaced, not merged) and `finalizers` onto the stored object and updates it; `status` and `operation` from the request are ignored. Conflicts are retried up to 10 times re-reading the object (`Internal: Failed to update application. Too many conflicts`). Emits `ResourceUpdated` ("updated application spec") and waits for informer sync.
- **Edge cases**: Nil application -> `error updating application: application is nil in request`. Removing an annotation such as `notice.*` requires sending the full annotation map.
- **Configuration surface**: request `validate`, `project`.
- **Exposure**: CRD; API; CLI `argocd app confirm-deletion` (uses Update with `validate=false`); UI summary "Edit"/"Save" on metadata.
- **Tests required**:
  - Positive: update replaces labels (removed labels disappear).
  - Positive: status in request is ignored.
  - Negative (authorization): update permission on old but not on new project -> denied (APP-055).

### APP-051 Update Application spec via API (UpdateSpec)
- **Upstream**: [application.go#L1068](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1068), [application.proto#L426](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.proto#L426), [application.proto#L144](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.proto#L144), [app.go#L2985](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/app.go#L2985)
- **Maturity**: GA
- **Semantics**: `PUT /api/v1/applications/{name}/spec` with body `spec`, plus `validate`, `appNamespace`, `project`. Requires `update`; replaces the spec only (labels/annotations/finalizers unchanged), same validation and conflict handling as Update; returns the stored spec.
- **Edge cases**: Nil spec -> `error updating application spec: spec is nil in request`.
- **Configuration surface**: request fields.
- **Exposure**: API; CLI `argocd app set`, `unset`, `edit` (opens spec YAML in `$EDITOR`), `add-source`, `remove-source`; UI parameter/summary edits.
- **Tests required**:
  - Positive: `argocd app set --revision v2` changes only `spec.source.targetRevision`.
  - Negative: invalid spec with `validate=true` -> InvalidArgument `application spec for <app> is invalid: ...`.

### APP-052 Patch Application via API
- **Upstream**: [application.go#L1090](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1090), [application.proto#L434](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.proto#L434), [application.proto#L153](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.proto#L153), [app.go#L3046](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/app.go#L3046), [argocd_app_patch.md#L21](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/commands/argocd_app_patch.md#L21)
- **Maturity**: GA
- **Semantics**: `PATCH /api/v1/applications/{name}` body `{patch, patchType, appNamespace, project}`. Requires `get` (with project semantics) then `update`. `patchType` `json` (default when empty) applies an RFC 6902 patch to the full Application JSON; `merge` applies an RFC 7386 merge patch. The patched object goes through Update logic with `validate=true` (only spec/labels/annotations/finalizers take effect).
- **Edge cases**: Other types -> `InvalidArgument: Patch type '<t>' is not supported`; bad JSON patch -> `error decoding json patch: ...`; inapplicable op -> `error applying patch: ...`. Patching `/status` or `/operation` has no effect.
- **Configuration surface**: request fields.
- **Exposure**: API; CLI `argocd app patch APP --patch <body> --type json|merge` (default `json`).
- **Tests required**:
  - Positive: JSON patch replacing `/spec/source/targetRevision`; merge patch adding an annotation.
  - Negative: `--type strategic` -> exact error.

### APP-053 Get, List and Watch Applications via API
- **Upstream**: [application.go#L176](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L176), [application.go#L288](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L288), [application.go#L780](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L780), [application.go#L1251](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1251), [application.proto#L21](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.proto#L21), [application.proto#L357](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.proto#L357), [argocd_app_get.md#L48](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/commands/argocd_app_get.md#L48), [argocd_app_list.md#L25](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/commands/argocd_app_list.md#L25)
- **Maturity**: GA
- **Semantics**: Get `GET /api/v1/applications/{name}` (query `appNamespace`, `projects`/legacy `project`, `refresh`, `resourceVersion`) reads through the Kubernetes client (not the informer) for read-after-watch consistency. Existence-hiding rules: if a project is given and RBAC denies -> `PermissionDenied` (after a dummy GET to equalize timing); given project + not found -> `NotFound`; no project -> `PermissionDenied` for both "denied" and "not found"; app exists but in another project than requested -> `NotFound`; namespace not enabled -> `PermissionDenied`. More than one project -> `InvalidArgument: multiple projects specified - the get endpoint accepts either zero or one project`. List `GET /api/v1/applications` filters by `selector`, `name`, `projects`, `repo`, `appNamespace`; drops apps in non-enabled namespaces or without RBAC `get`; sorted by name; list resourceVersion = informer's last sync RV. Watch `GET /api/v1/stream/applications` streams `ApplicationWatchEvent{type, application}`; sends `ADDED` for current apps when no `resourceVersion` is given or a single `name` is watched; filters by RBAC, namespace, selector, projects and min resourceVersion.
- **Edge cases**: Selector parse errors -> `error parsing the selector: ...`. Client-side CLI filters: `argocd app list -c/--cluster`, `-P/--path`.
- **Configuration surface**: watch buffer env `ARGOCD_WATCH_API_BUFFER_SIZE` (default 1000, [common.go#L282](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L282)).
- **Exposure**: API; CLI `argocd app get` (`-o json|yaml|wide|tree`, `--show-operation`, `--show-params`, `--timeout`), `argocd app list` (`-o wide|name|json|yaml`, columns NAME, CLUSTER, NAMESPACE, PROJECT, STATUS, HEALTH, SYNCPOLICY, CONDITIONS [+REPO, PATH, TARGET]); UI list/details (watch-driven).
- **Tests required**:
  - Authorization: user without access gets `PermissionDenied` for both existing and nonexistent apps (no project given).
  - Authorization: user with access, wrong project in query -> `NotFound`.
  - Positive: Watch with name emits initial ADDED event.

### APP-054 Server-side validation on create/update (validate flag)
- **Upstream**: [application.go#L1321](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1321), [argo.go#L299](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L299), [argo.go#L429](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L429), [argo.go#L806](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L806), [argo.go#L87](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L87)
- **Maturity**: GA
- **Semantics**: Always: non-empty name; unique source names; project-change authorization (APP-055); destination resolvable (`InvalidArgument: application destination spec for <app> is invalid: <err>`); `ValidatePermissions` (APP-006/APP-014/APP-026/APP-003); `managed-by-url` validation (APP-102); normalization (APP-013). Only when `validate=true`: for each source the repository is tested (`repository not accessible: ...`), explicit source type uniqueness, cluster reachability (`Unable to get cluster: ...`), and a manifest-generation dry run per source with `NoRevisionCache` (`Unable to generate manifests in <path>: <err>`); for hydrator apps the dry source is validated (without cluster version/API versions). Errors are formatted `<Type>: <message>` joined by `;` inside `application spec for <app> is invalid: ...`.
- **Edge cases**: A directory glob that matches no files is not an API error (deferred to ComparisonError). Validation failing on an inaccessible first repo aborts checking remaining sources.
- **Configuration surface**: request `validate`; CLI `--validate` (default true).
- **Exposure**: API create/update/updateSpec (patch always validates); CLI.
- **Tests required**:
  - Negative: nonexistent path with validate -> `Unable to generate manifests in <path>`; with `--validate=false` -> accepted.
  - Negative: destination not permitted is rejected even with `--validate=false`.

### APP-055 Changing an Application's project
- **Upstream**: [application.go#L1347](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1347), [application.go#L964](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L964)
- **Maturity**: GA
- **Semantics**: When an update/upsert changes `spec.project`, the caller needs `create` on the app's RBAC name in the new project and `update` on the RBAC name in the old project; the new project must exist and permit the app's namespace (vague error otherwise). Validation then uses the new project.
- **Edge cases**: Combined with `project` query param mismatch rules from APP-053.
- **Configuration surface**: `spec.project`.
- **Exposure**: API; CLI `argocd app set --project`; UI summary edit.
- **Tests required**:
  - Authorization: user with update in old + create in new succeeds; missing either is denied.

### APP-056 Delete Application via API (cascade and propagationPolicy)
- **Upstream**: [application.go#L1162](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1162), [application.go#L1177](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1177), [application.go#L2947](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2947), [application.proto#L442](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.proto#L442), [application.proto#L113](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.proto#L113), [app.go#L1262](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/app.go#L1262), [utils.tsx#L91](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/utils.tsx#L91), [applications-service.ts#L215](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/shared/services/applications-service.ts#L215), [argocd_app_delete.md#L28](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/commands/argocd_app_delete.md#L28), [app_deletion.md#L3](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/app_deletion.md#L3)
- **Maturity**: GA
- **Semantics**: `DELETE /api/v1/applications/{name}` with query `cascade`, `propagationPolicy`, `appNamespace`, `project`. Requires `get` (project semantics) and `delete`. If `cascade` is nil or true: map `propagationPolicy` case-insensitively (`""` -> `resources-finalizer.argocd.argoproj.io`, `foreground` -> `.../foreground`, `background` -> `.../background`) and, if that exact finalizer is absent and the app is not already being deleted, add it via merge patch of `metadata.finalizers`. If `cascade=false`: remove every propagation finalizer. Then delete the object and emit `ResourceDeleted` ("deleted application"). Pre/post-delete finalizers are not touched by the API. CLI `argocd app delete APP...` (`--cascade` default true, `-p/--propagation-policy` default `foreground`, both sent only when explicitly set; `-y`; `-l` selector; `--wait` for the DELETED watch event; `-N`); interactive prompt only for cascade deletes on a TTY. UI dialog: type the app name; choose `Foreground` (default, sends `foreground`), `Background`, or `Non-cascading` (sends `cascade=false`).
- **Edge cases**: `cascade=false` + non-empty policy -> `InvalidArgument: cannot set propagation policy when cascading is disabled`; unknown policy (e.g. `orphan`) -> `InvalidArgument: invalid propagation policy: orphan`. Adding a policy finalizer does not remove a different existing one (e.g. background requested when the default finalizer is present leaves both; the controller uses the first propagation finalizer in list order). Delete on an app already terminating is still issued (idempotent retries).
- **Configuration surface**: request `cascade`, `propagationPolicy`.
- **Exposure**: CRD (kubectl delete honours finalizers already set); API; CLI `argocd app delete`; UI delete dialogs (list, details, resource tree).
- **Tests required**:
  - Positive: API delete without params adds `resources-finalizer.argocd.argoproj.io` and resources are removed.
  - Positive: `cascade=false` removes propagation finalizers; resources remain.
  - Negative: both error strings above.
  - Authorization: `get` without `delete` -> PermissionDenied.

### APP-057 Finalizer resources-finalizer.argocd.argoproj.io (cascade, foreground)
- **Upstream**: [application_defaults.go#L10](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/application_defaults.go#L10), [types.go#L3575](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L3575), [types.go#L3673](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L3673), [appcontroller.go#L1428](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1428), [appcontroller.go#L1456](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1456), [app_deletion.md#L41](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/app_deletion.md#L41)
- **Maturity**: GA
- **Semantics**: Presence of this finalizer (or either variant) makes deletion cascading: the controller deletes the app's managed resources (APP-061) using Kubernetes `PropagationPolicy: Foreground`, waits until they are gone, then removes the propagation finalizer(s) so the Application can be removed. Can be set declaratively, by `argocd app create --set-finalizer`, by the UI create checkbox, or by API delete.
- **Edge cases**: Deletion is processed by the operation workers only when no `operation` is pending (an in-flight operation finishes first). Resources not permitted by the project are not deleted.
- **Configuration surface**: `metadata.finalizers`.
- **Exposure**: CRD; API; CLI; UI.
- **Tests required**:
  - Positive: kubectl delete of an app with the finalizer removes resources then the app.
  - Positive: app without finalizer deleted via kubectl leaves resources (non-cascade).

### APP-058 Finalizer resources-finalizer.argocd.argoproj.io/foreground
- **Upstream**: [application_defaults.go#L19](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/application_defaults.go#L19), [types.go#L3687](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L3687), [appcontroller.go#L1456](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1456)
- **Maturity**: GA
- **Semantics**: Same as APP-057 (foreground propagation); added by API/UI delete with `propagationPolicy=foreground`.
- **Edge cases**: Propagation policy is chosen from the first propagation finalizer in `metadata.finalizers`; only the background variant switches to Background.
- **Configuration surface**: `metadata.finalizers`.
- **Exposure**: CRD; API; CLI `-p foreground`; UI Foreground option.
- **Tests required**:
  - Positive: Deployment deleted with foreground propagation (Deployment remains with deletionTimestamp until Pods/ReplicaSets are gone).

### APP-059 Finalizer resources-finalizer.argocd.argoproj.io/background
- **Upstream**: [application_defaults.go#L22](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/application_defaults.go#L22), [appcontroller.go#L1457](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1457), [app_deletion.md#L57](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/app_deletion.md#L57)
- **Maturity**: GA
- **Semantics**: Cascade deletion with `PropagationPolicy: Background` for each managed resource. The controller still waits until the managed (top-level) resources are no longer present before removing the finalizer; dependents are garbage collected by Kubernetes afterwards.
- **Edge cases**: See doc difference: docs describe the Application as removed "immediately".
- **Configuration surface**: `metadata.finalizers`.
- **Exposure**: CRD; API; CLI `-p background`; UI Background option.
- **Tests required**:
  - Positive: managed objects receive background delete; app disappears after top-level objects are gone.

### APP-060 Non-cascading deletion
- **Upstream**: [application.go#L1195](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1195), [types.go#L3665](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L3665), [appcontroller.go#L1540](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1540), [app_deletion.md#L25](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/app_deletion.md#L25)
- **Maturity**: GA
- **Semantics**: Without propagation finalizers the Application object is deleted and managed resources are left in the cluster (their tracking metadata is left as-is). PreDelete/PostDelete hooks still run if their finalizers are present. When no Argo CD finalizer remains, the controller clears the app's cached managed resources and resource tree and requeues its project for refresh.
- **Edge cases**: Orphaned resources keep `argocd.argoproj.io/tracking-id`, so a new app with the same name re-adopts them.
- **Configuration surface**: absence of propagation finalizers.
- **Exposure**: CRD; API `cascade=false`; CLI `--cascade=false`; UI Non-cascading.
- **Tests required**:
  - Positive: non-cascade delete leaves Deployment running.
  - Positive: non-cascade delete of an app with PreDelete hooks still runs them.

### APP-061 Cascade deletion resource selection and ordering
- **Upstream**: [appcontroller.go#L1333](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1333), [appcontroller.go#L1344](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1344), [appcontroller.go#L1438](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1438), [appcontroller.go#L1454](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1454), [sort_delete.go#L24](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sort_delete.go#L24), [appcontroller.go#L1363](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1363)
- **Maturity**: GA
- **Semantics**: Candidates = live objects tracked as managed by the app and permitted by the AppProject. Excluded from deletion: CRDs, the Application itself (self-managed app), resources whose effective `Delete` sync option is `false` (resource annotation `argocd.argoproj.io/sync-options: Delete=false` overrides app-level `syncPolicy.syncOptions`), and resources annotated `helm.sh/resource-policy: keep`. Each pass: if any candidate already has a `deletionTimestamp`, wait; otherwise sort candidates by sync wave descending and delete only the objects in the highest wave (in parallel); re-list; remaining deletable objects -> wait for next pass (log `<n> objects remaining for deletion`); none -> remove propagation finalizers. The controller re-reads the Application from the API at the start (NotFound -> evict from cache). Impersonation (if enabled) is applied to the delete calls.
- **Edge cases**: Errors from delete calls surface as `DeletionError`. Objects requiring deletion confirmation block the whole cascade (APP-062). Project lookup failure returns an error (DeletionError).
- **Configuration surface**: `Delete=false` sync option; `helm.sh/resource-policy: keep`; sync waves.
- **Exposure**: CRD/controller behaviour.
- **Tests required**:
  - Positive: resources in waves 0 and 5 -> wave 5 deleted first, wave 0 only after wave 5 is gone.
  - Positive: `Delete=false` and `helm.sh/resource-policy: keep` resources survive; CRDs survive.
  - Positive: app managing its own Application object does not delete itself.

### APP-062 Deletion confirmation (Delete=confirm / Prune=confirm, deletion-approved)
- **Upstream**: [state.go#L1171](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L1171), [types.go#L4173](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L4173), [types.go#L19](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/common/types.go#L19), [appcontroller.go#L1447](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1447), [app.go#L3245](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/app.go#L3245), [application-details.tsx#L1383](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-details/application-details.tsx#L1383)
- **Maturity**: GA
- **Semantics**: A resource requires confirmation when its effective `Delete` or `Prune` sync option (resource annotation overrides app-level syncOptions) equals `confirm`; this is reported as `status.resources[].requiresDeletionConfirmation`. During cascade deletion, if any to-be-deleted resource requires confirmation and the Application annotation `argocd.argoproj.io/deletion-approved` (RFC3339 timestamp) is missing, unparsable, or earlier than the app's `deletionTimestamp`, deletion waits. `argocd app confirm-deletion APP` sets the annotation to now via Update (`validate=false`). The UI shows `Confirm Deletion` (app being deleted with confirmation-required resources) and `Confirm Pruning` (running operation with such resources).
- **Edge cases**: An approval timestamp older than the deletion start does not count. Prune-confirmation semantics during sync belong to the sync area.
- **Configuration surface**: sync options `Delete=confirm`, `Prune=confirm`; annotation `argocd.argoproj.io/deletion-approved`.
- **Exposure**: CRD annotation; API (Update); CLI `argocd app confirm-deletion`; UI action menu.
- **Tests required**:
  - Positive: cascade delete blocks until confirm-deletion, then completes.
  - Negative: stale approval timestamp does not unblock.

### APP-063 Deletion with an unresolvable destination
- **Upstream**: [appcontroller.go#L1381](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1381)
- **Maturity**: GA
- **Semantics**: If the destination cluster cannot be resolved during finalization (cluster removed, ambiguous name), the controller removes all propagation finalizers and all pre/post-delete finalizers (every stage) in one patch, logs `Resource entries removed from undefined cluster`, and the Application is deleted without touching any resources or running hooks.
- **Edge cases**: Project lookup happens before this step; a missing project blocks deletion with DeletionError instead.
- **Configuration surface**: none.
- **Exposure**: controller behaviour.
- **Tests required**:
  - Recovery: delete the cluster secret, then delete the app with finalizer -> app removed promptly.

### APP-064 PreDelete hooks and pre-delete-finalizer.argocd.argoproj.io
- **Upstream**: [application_defaults.go#L13](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/application_defaults.go#L13), [hook.go#L28](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/hook.go#L28), [hook.go#L97](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/hook.go#L97), [hook.go#L242](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/hook.go#L242), [state.go#L611](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L611), [appcontroller.go#L2077](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2077), [appcontroller.go#L1406](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1406), [sync-waves.md#L59](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/sync-waves.md#L59)
- **Maturity**: GA
- **Semantics**: A rendered resource is a PreDelete hook when `argocd.argoproj.io/hook` contains `PreDelete` or `helm.sh/hook` contains `pre-delete` (comma lists, trimmed). Such resources are excluded from normal sync unless they also declare a sync-phase hook (PreSync/Sync/PostSync/SyncFail). On each full reconcile (while not deleting) the controller adds both `pre-delete-finalizer.argocd.argoproj.io` and `pre-delete-finalizer.argocd.argoproj.io/cleanup` when the app renders PreDelete hooks and removes them when it no longer does (patched before status). On deletion, before any resource deletion: hooks are re-rendered from Git at the spec target revisions; namespaced hooks get the destination namespace if none; each must pass project destination/resource restrictions; missing hooks are created with tracking metadata (AlreadyExists tolerated) and the pass ends; then hook health is evaluated (no health check = Healthy): any `Degraded` -> the failed hooks are deleted (to allow retry) and the error `PreDelete hook(s) failed: <ns/name,...>` becomes `DeletionError`; any `Progressing` -> wait; all Healthy -> remove the stage-less pre-delete finalizer and continue with cascade deletion.
- **Edge cases**: Hook not permitted by project -> `PreDelete hook <key> is not permitted: <err>` (fail closed). Scope of hooks determined from discovery (cluster-scoped hooks never get a namespace). Hooks are recreated after being deleted for failure, so a permanently failing hook blocks deletion until Git is fixed or the finalizer is removed manually.
- **Configuration surface**: hook annotations; finalizers above.
- **Exposure**: CRD (finalizers visible); controller behaviour; UI DeletionError condition.
- **Tests required**:
  - Positive: app with PreDelete Job gets both finalizers; deletion runs the Job before deleting resources.
  - Negative: failing Job -> DeletionError `PreDelete hook(s) failed: ...`, resources untouched.
  - Authorization: PreDelete hook targeting a forbidden namespace -> not created, DeletionError.
  - Positive: removing the hook from Git removes the finalizers.

### APP-065 PostDelete hooks and post-delete-finalizer.argocd.argoproj.io
- **Upstream**: [application_defaults.go#L16](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/application_defaults.go#L16), [hook.go#L37](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/hook.go#L37), [appcontroller.go#L1489](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1489), [appcontroller.go#L2092](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2092), [sync-waves.md#L80](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/sync-waves.md#L80)
- **Maturity**: GA
- **Semantics**: Same detection and finalizer management as PreDelete (`PostDelete` / `helm.sh/hook: post-delete`; finalizers `post-delete-finalizer.argocd.argoproj.io` and `.../cleanup`). Executed after the cascade step has removed the propagation finalizers (or immediately for non-cascade deletes), with the same create/health/fail/retry logic and error `PostDelete hook(s) failed: ...`. When healthy, the stage-less post-delete finalizer is removed.
- **Edge cases**: PostDelete hooks are created in a cluster where the app's resources are already gone; hooks are tracked by the app and therefore are not themselves deleted by cascade (they are created later).
- **Configuration surface**: hook annotations; finalizers.
- **Exposure**: CRD; controller behaviour.
- **Tests required**:
  - Positive: PostDelete Job runs after resources are deleted; app removed after Job succeeds.
  - Negative: failing PostDelete Job -> DeletionError, Application CR remains.

### APP-066 Delete-hook cleanup stage (/cleanup finalizers)
- **Upstream**: [hook.go#L261](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/hook.go#L261), [appcontroller.go#L1506](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1506), [appcontroller.go#L1523](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1523), [types.go#L3614](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L3614)
- **Maturity**: GA
- **Semantics**: After PostDelete execution, the controller processes `pre-delete-finalizer.argocd.argoproj.io/cleanup` then `post-delete-finalizer.argocd.argoproj.io/cleanup`: it collects the live hooks of that type, computes their aggregated (worst) health, and deletes each hook that has no `hook-delete-policy`, or whose policies include `HookSucceeded` with aggregate `Healthy` or `HookFailed` with aggregate `Degraded`; it waits until those hooks are gone, then removes the cleanup finalizer. Order of finalizer processing per pass: pre-delete -> cascade -> post-delete -> pre-delete/cleanup -> post-delete/cleanup.
- **Edge cases**: Hooks with `BeforeHookCreation` only are not deleted by cleanup. Finalizer names are composed as `<base>/<stage>`. Derived from code reading: PreDelete hook objects are created with the app's tracking metadata and have no owner references, so during a cascading delete they are among the managed live objects and are deleted in the cascade step (unless excluded, e.g. `Delete=false`); the PreDelete cleanup stage then usually finds nothing.
- **Configuration surface**: `argocd.argoproj.io/hook-delete-policy` on hooks.
- **Exposure**: controller behaviour; CRD finalizers.
- **Tests required**:
  - Positive: hook without delete policy is deleted at cleanup; app removed afterwards.
  - Positive: hook with only `HookFailed` policy that succeeded is left in the cluster.

### APP-067 Applications in any namespace (application.namespaces)
- **Upstream**: [appcontroller.go#L2639](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2639), [appcontroller.go#L2676](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2676), [appcontroller.go#L2896](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2896), [application_namespaces.go#L9](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/security/application_namespaces.go#L9), [argocd_application_controller.go#L292](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-application-controller/commands/argocd_application_controller.go#L292), [argocd_server.go#L336](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-server/commands/argocd_server.go#L336), [application.go#L2967](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2967), [argocd-cmd-params-cm.yaml#L39](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cmd-params-cm.yaml#L39), [app-any-namespace.md#L30](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/app-any-namespace.md#L30)
- **Maturity**: GA
- **Semantics**: Applications are always allowed in the control-plane namespace. Additional namespaces are enabled with `--application-namespaces` on both the controller and the API server (env `ARGOCD_APPLICATION_NAMESPACES`, cmd-params key `application.namespaces`, comma-separated). Each entry is a shell glob (e.g. `team-*`, `*`) or a regex wrapped in `/.../`. With at least one entry, the controller lists/watches Applications cluster-wide and filters out non-enabled namespaces (list, informer events, hydrator app list). The API server ignores apps in non-enabled namespaces in List/Watch and rejects other operations (`namespace '<ns>' is not permitted`, surfaced as PermissionDenied on reads). The namespace must also be allowed by the app's project (APP-068).
- **Edge cases**: Requires a cluster-scoped installation and extra Kubernetes RBAC for `argocd-server` (docs). Workloads must be restarted after changing cmd-params. An app in a non-enabled namespace is silently ignored by the controller (no status).
- **Configuration surface**: flags/env/cmd-params above.
- **Exposure**: CRD (apps in other namespaces); API `appNamespace` parameters; CLI `ns/name` and `-N/--app-namespace`; UI `ns/name` in Application Name, namespace column/filter.
- **Tests required**:
  - Positive: app in `team-a` reconciled with `application.namespaces: team-*` and project `sourceNamespaces: [team-a]`.
  - Negative: app in non-enabled namespace is ignored by controller and invisible via API.
  - Positive: regex entry `/^team-(a|b)$/` matches only those namespaces.

### APP-068 AppProject sourceNamespaces enforcement for Applications
- **Upstream**: [app_project_types.go#L625](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/app_project_types.go#L625), [argo.go#L794](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L794), [appcontroller.go#L430](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L430), [types.go#L19](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/types.go#L19), [app-any-namespace.md#L85](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/app-any-namespace.md#L85)
- **Maturity**: GA
- **Semantics**: An Application whose namespace is empty or equals the control-plane namespace may use any project. Otherwise its namespace must match (glob or `/regex/`) an entry of the project's `spec.sourceNamespaces`. Controller violation: `UnknownError` `application '<a>' in namespace '<ns>' is not permitted to use project '<p>'`, sync/health `Unknown`, no comparison/sync, no resource-change refresh. API: project resolution fails with `application '<a>' in namespace '<ns>' is not allowed to use project <p>` internally, returned to clients as the vague `app is not allowed in project "<p>", or the project does not exist`.
- **Edge cases**: Enforcement is independent of Argo CD RBAC (API refuses even admins). Granting the control-plane namespace in `sourceNamespaces` is discouraged (privilege escalation).
- **Configuration surface**: AppProject `spec.sourceNamespaces[]`.
- **Exposure**: CRD; API; CLI; UI.
- **Tests required**:
  - Negative: app in `ns-b` using project allowing only `ns-a` -> UnknownError exact message.
  - Positive: app in control-plane namespace can use a project with empty `sourceNamespaces`.

### APP-069 Qualified names, instance names and RBAC names
- **Upstream**: [types.go#L4132](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L4132), [types.go#L4144](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L4144), [types.go#L4153](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L4153), [rbac.go#L8](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/security/rbac.go#L8), [argo.go#L1230](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L1230), [appcontroller.go#L2883](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2883), [app-any-namespace.md#L131](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/app-any-namespace.md#L131)
- **Maturity**: GA
- **Semantics**: Qualified name `<namespace>/<name>` (or `<name>` when namespace empty) used by CLI/UI and controller keys; CLI parses `a/b` as namespace `a`, name `b`, unqualified names use `-N` or the server default. Instance name (tracking value, caches, metrics) is `<name>` when the app is in the control-plane namespace (or namespace empty) else `<namespace>_<name>`. RBAC object is `<project>/<name>` for control-plane-namespace apps, else `<project>/<namespace>/<name>`.
- **Edge cases**: Control-plane apps can never be matched by RBAC patterns `proj/argocd/*` (always 2-segment). `ns_name` instance names are parsed back by splitting on the first `_` (`toAppKey`), so an app literally named `a_b` in the control-plane namespace is ambiguous with app `b` in namespace `a` for event-driven refresh keys (derived from code).
- **Configuration surface**: none.
- **Exposure**: CLI/UI naming; RBAC; tracking ids.
- **Tests required**:
  - Positive: `argocd app get team/app1` targets namespace `team`.
  - Positive: tracking annotation of an app in `team` starts with `team_app1:`.
  - Authorization: policy `p, r, applications, get, proj/team/*, allow` grants access only to apps in `team`.

### APP-070 Resource tracking method: annotation (default)
- **Upstream**: [types.go#L115](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L115), [settings.go#L904](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L904), [resource_tracking.go#L146](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/resource_tracking.go#L146), [resource_tracking.go#L233](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/resource_tracking.go#L233), [resource_tracking.go#L238](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/resource_tracking.go#L238), [common.go#L215](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L215), [cluster.go#L1533](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/cache/cluster.go#L1533), [resource_tracking.md#L7](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/resource_tracking.md#L7)
- **Maturity**: GA
- **Semantics**: `argocd-cm` `application.resourceTrackingMethod` empty or `annotation`. Every rendered resource gets `argocd.argoproj.io/tracking-id: <instanceName>:<group>/<kind>:<namespace>/<name>` (namespace = resource namespace, or the destination namespace for cluster-scoped resources). Ownership is read from that annotation; parse failures (`wrong resource tracking format, should be <application-name>:<group>/<kind>:<namespace>/<name>`) mean "not owned". The live objects considered managed are those whose tracking resolves to this app and that have no ownerReferences, plus any live object matching a target key even without tracking (adoption). The `installation-id` annotation is added or removed according to APP-074.
- **Edge cases**: Label-only metadata does not grant ownership under this method. Changing the method requires a sync to rewrite metadata.
- **Configuration surface**: `argocd-cm` `application.resourceTrackingMethod`.
- **Exposure**: Resource metadata on managed objects; UI shows resources.
- **Tests required**:
  - Positive: synced resource carries exact tracking-id format; cluster-scoped resource uses destination namespace in the id.
  - Negative: malformed tracking-id -> resource treated as unmanaged.

### APP-071 Resource tracking method: label
- **Upstream**: [resource_tracking.go#L69](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/resource_tracking.go#L69), [resource_tracking.go#L171](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/resource_tracking.go#L171), [state.go#L1295](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L1295), [resource_tracking.md#L41](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/resource_tracking.md#L41)
- **Maturity**: GA
- **Semantics**: `application.resourceTrackingMethod: label`. The instance label (APP-073) is set to the app instance name and ownership is read from it. The value is not truncated (Kubernetes rejects values > 63 chars). Self-reference checks are skipped (every labelled object is considered self-referenced).
- **Edge cases**: Long `<ns>_<name>` instance names fail at apply time. Third-party tools that set `app.kubernetes.io/instance` cause false ownership.
- **Configuration surface**: `application.resourceTrackingMethod`, `application.instanceLabelKey`.
- **Exposure**: resource labels.
- **Tests required**:
  - Positive: label tracking adopts an existing object labelled with the app name.
  - Negative: 70-char instance name -> sync error from the API server.

### APP-072 Resource tracking method: annotation+label
- **Upstream**: [resource_tracking.go#L182](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/resource_tracking.go#L182), [resource_tracking.go#L131](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/resource_tracking.go#L131), [resource_tracking.md#L8](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/resource_tracking.md#L8)
- **Maturity**: GA
- **Semantics**: Writes both the tracking annotation (authoritative for ownership) and the instance label (informational). The label value is truncated to 63 characters and trailing non-alphanumeric characters are stripped.
- **Edge cases**: If stripping leaves nothing usable -> `failed to set app instance label: unable to truncate label to not end with a special character`.
- **Configuration surface**: `application.resourceTrackingMethod: annotation+label`.
- **Exposure**: resource metadata.
- **Tests required**:
  - Positive: 80-char instance name -> label truncated to 63 chars ending alphanumeric; ownership still works via annotation.

### APP-073 application.instanceLabelKey
- **Upstream**: [settings.go#L471](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L471), [settings.go#L892](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L892), [common.go#L190](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L190), [common.go#L196](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L196), [kube.go#L22](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/kube/kube.go#L22), [resource_tracking.md#L65](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/resource_tracking.md#L65)
- **Maturity**: GA
- **Semantics**: `argocd-cm` key `application.instanceLabelKey`; default `app.kubernetes.io/instance`; docs recommend `argocd.argoproj.io/instance`. Used by `label` and `annotation+label` methods. If set to the legacy key `applications.argoproj.io/app-name`, the label is also written into `spec.template.metadata.labels` of Deployments/ReplicaSets/StatefulSets/DaemonSets (`apps`/`extensions`).
- **Edge cases**: Tracking removal for hydrated manifests under `label` removes `app.kubernetes.io/instance` specifically, not a custom key (derived from code, see APP-118).
- **Configuration surface**: `argocd-cm` `application.instanceLabelKey`.
- **Exposure**: resource labels.
- **Tests required**:
  - Positive: custom key `argocd.argoproj.io/instance` used for label tracking.

### APP-074 installationID
- **Upstream**: [settings.go#L477](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L477), [settings.go#L916](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L916), [common.go#L216](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L216), [resource_tracking.go#L53](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/resource_tracking.go#L53), [resource_tracking.go#L149](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/resource_tracking.go#L149), [resource_tracking.md#L27](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/resource_tracking.md#L27)
- **Maturity**: GA
- **Semantics**: `argocd-cm` key `installationID`. With annotation-based tracking, each managed resource gets `argocd.argoproj.io/installation-id: <id>` (removed when not configured). A resource is considered owned only if its `installation-id` annotation equals the configured value exactly (both empty also matches), allowing several Argo CD instances to manage one cluster with identically named apps.
- **Edge cases**: Setting an installationID makes previously synced resources (without the annotation) appear unowned until re-synced. A resource carrying an installation-id while the instance has none is not owned.
- **Configuration surface**: `argocd-cm` `installationID`.
- **Exposure**: resource annotations.
- **Tests required**:
  - Positive: two instances with different IDs and same app name do not see each other's resources.
  - Negative: resource with mismatching installation-id is not pruned/considered managed.

### APP-075 Non-self-referencing tracking annotations (copied resources)
- **Upstream**: [state.go#L1288](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L1288), [state.go#L1332](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L1332), [state.go#L981](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L981), [resource_tracking.md#L35](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/resource_tracking.md#L35)
- **Maturity**: GA
- **Semantics**: With annotation-based methods, a live object is self-referenced only if the tracking id's group, kind, name and namespace match the object (namespace equal, or object cluster-scoped). The desired manifest is used to build the expected id when available (handles API-group migrations); otherwise the live annotation is compared to the live object. Non-self-referenced objects get no sync status, do not affect app sync status, are never `requiresPruning`, and appear as top-level nodes in the UI.
- **Edge cases**: Under `label` tracking this check always passes.
- **Configuration surface**: tracking method.
- **Exposure**: status; UI.
- **Tests required**:
  - Positive: a Secret copied to another namespace with the original tracking-id does not make the app OutOfSync nor get pruned.

### APP-076 Tracking-method migration normalization
- **Upstream**: [resource_tracking.go#L262](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/resource_tracking.go#L262), [resource_tracking.md#L98](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/resource_tracking.md#L98)
- **Maturity**: GA
- **Semantics**: During diffing with `annotation`/`annotation+label`, for non-CRD live objects that carry the instance label: if the live object has no tracking annotation, the desired annotation is backfilled into the normalized live copy (so switching from label tracking does not mark everything OutOfSync); if the desired object has no instance label, the label is removed from the normalized live copy. A live tracking annotation that differs from the desired one is a real diff.
- **Edge cases**: Normalization only affects comparison; metadata is rewritten on the next sync.
- **Configuration surface**: tracking method.
- **Exposure**: diff/sync status.
- **Tests required**:
  - Positive: switch from label to annotation -> apps stay Synced; after sync, resources carry tracking-id.

### APP-077 Ownership conflict detection (SharedResourceWarning and FailOnSharedResource)
- **Upstream**: [state.go#L804](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L804), [sync.go#L155](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sync.go#L155), [sync.go#L593](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sync.go#L593)
- **Maturity**: GA
- **Semantics**: Comparison raises `SharedResourceWarning` for every live object selected for the app whose tracking value names a different app. If a sync runs with sync option `FailOnSharedResource=true` (app-level or per-sync), the operation fails immediately with `Shared resource found: <first SharedResourceWarning message>`.
- **Edge cases**: The check uses the conditions present on the app when the sync starts (after a fresh comparison in the sync).
- **Configuration surface**: sync option `FailOnSharedResource=true`.
- **Exposure**: CRD status/operation; UI; CLI.
- **Tests required**:
  - Positive: shared ConfigMap + FailOnSharedResource -> operation Failed with exact prefix.
  - Positive: without the option, sync proceeds and takes ownership.

### APP-078 Refresh annotation argocd.argoproj.io/refresh (normal)
- **Upstream**: [application_annotations.go#L6](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/application_annotations.go#L6), [types.go#L3580](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L3580), [appcontroller.go#L2202](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2202), [appcontroller.go#L2319](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2319), [state.go#L1126](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L1126), [annotations-and-labels.md#L13](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/annotations-and-labels.md#L13)
- **Maturity**: GA
- **Semantics**: Presence of the annotation (any value other than `hard`) requests a normal refresh: full comparison at level "force resolve" (revisions re-resolved without the revision cache; repo errors are not hidden by the grace period), diff cache bypassed. The controller removes the annotation when it persists the reconciliation status (also on the spec-error path), via the same merge patch as status.
- **Edge cases**: If the controller exits early on a transient repo error the annotation stays until a later reconciliation. Takes precedence over all other refresh reasons.
- **Configuration surface**: annotation `argocd.argoproj.io/refresh`.
- **Exposure**: CRD annotation (kubectl annotate); API/CLI/UI via APP-080.
- **Tests required**:
  - Positive: `kubectl annotate app x argocd.argoproj.io/refresh=normal` -> new `reconciledAt`, annotation removed.

### APP-079 Refresh annotation argocd.argoproj.io/refresh=hard
- **Upstream**: [types.go#L3591](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L3591), [appcontroller.go#L2018](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2018), [application.go#L832](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L832), [application_annotations.go#L5](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/application_annotations.go#L5)
- **Maturity**: GA
- **Semantics**: Value `hard` additionally sets `noCache` for manifest generation (repo server regenerates manifests instead of using the manifest cache). Through the API, a hard refresh also forces `GetAppDetails` with `NoCache` for the primary source (errors only logged) and requests a hard hydration (APP-115).
- **Edge cases**: No code path was found that invalidates the controller's live cluster cache on hard refresh (see Unconfirmed / doc differences).
- **Configuration surface**: annotation value `hard`.
- **Exposure**: CRD annotation; API `refresh=hard`; CLI `--hard-refresh`; UI `Hard Refresh`.
- **Tests required**:
  - Positive: after changing a Helm chart dependency served under the same revision, hard refresh picks up the change while normal refresh does not.

### APP-080 Refresh via API, CLI and UI
- **Upstream**: [application.go#L800](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L800), [argo.go#L240](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L240), [app.go#L228](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/app.go#L228), [application-details.tsx#L1422](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-details/application-details.tsx#L1422), [applications-service.ts#L62](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/shared/services/applications-service.ts#L62)
- **Maturity**: GA
- **Semantics**: `GET /api/v1/applications/{name}?refresh=normal|hard` (RBAC `get`): subscribes to app events, merge-patches annotations `argocd.argoproj.io/refresh=<type>` and `argocd.argoproj.io/hydrate=<normal|hard>` (up to 5 attempts on conflict, 100 ms apart), then blocks until it sees an event for the app with a higher resourceVersion and without the refresh annotation, and returns that object. Any `refresh` value other than `hard` is treated as normal. CLI: `--refresh` / `--hard-refresh` on `argocd app get` (and other app commands that read the app). UI: details action `Refresh` (normal) with sub-action `Hard Refresh`; list page bulk refresh panel.
- **Edge cases**: Client context deadline -> `application refresh deadline exceeded`. The `hydrate` annotation is set even for apps without a source hydrator and is then never removed for them (only the hydrator removes it) — derived from code reading.
- **Configuration surface**: none.
- **Exposure**: API; CLI; UI.
- **Tests required**:
  - Positive: Get with refresh returns an object whose `reconciledAt` is newer.
  - Negative: client timeout returns the deadline error.
  - Authorization: refresh requires only `get`.

### APP-081 Periodic reconciliation (timeout.reconciliation)
- **Upstream**: [argocd_application_controller.go#L259](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-application-controller/commands/argocd_application_controller.go#L259), [argocd_application_controller.go#L45](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-application-controller/commands/argocd_application_controller.go#L45), [argocd-application-controller-statefulset.yaml#L37](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/application-controller/argocd-application-controller-statefulset.yaml#L37), [appcontroller.go#L2250](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2250), [appcontroller.go#L2683](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2683), [argocd-cm.yaml#L341](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cm.yaml#L341)
- **Maturity**: GA
- **Semantics**: `argocd-cm` `timeout.reconciliation` -> env `ARGOCD_RECONCILIATION_TIMEOUT` -> `--app-resync` (seconds, default 120). The app informer resyncs at this period (or at the hard timeout if that is non-zero and smaller), generating update events; an app whose `reconciledAt` is nil or older than the timeout is refreshed at level "latest" (revision cache allowed) with reason `comparison expired, requesting refresh`. 0 disables time-based expiry (an app never reconciled is still reconciled once). The same value is the hydrator's stale-operation timeout and failed-hydration cooldown reference.
- **Edge cases**: Requires controller restart after change. Interacts with jitter (APP-082).
- **Configuration surface**: `timeout.reconciliation`, env, flag.
- **Exposure**: controller configuration.
- **Tests required**:
  - Positive: with 30s timeout, new commit is detected within ~30s + jitter without webhook.
  - Positive: 0s timeout -> no periodic refresh after the first.

### APP-082 Reconciliation jitter
- **Upstream**: [argocd_application_controller.go#L261](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-application-controller/commands/argocd_application_controller.go#L261), [appcontroller.go#L2791](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2791), [argocd-application-controller-statefulset.yaml#L49](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/application-controller/argocd-application-controller-statefulset.yaml#L49), [argocd-cm.yaml#L348](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cm.yaml#L348)
- **Maturity**: GA
- **Semantics**: `timeout.reconciliation.jitter` -> `ARGOCD_RECONCILIATION_JITTER` -> `--app-resync-jitter` (seconds, default 60). For informer resync events only (old and new resourceVersion equal), the refresh is delayed by a uniformly random duration between 0 (inclusive) and the jitter value (exclusive) and the operation queue is also re-queued; real object changes are processed immediately. 0 disables.
- **Edge cases**: Effective period becomes timeout..timeout+jitter.
- **Configuration surface**: key/env/flag above.
- **Exposure**: controller configuration.
- **Tests required**:
  - Positive: with jitter, resync-triggered refreshes spread over the window; spec edits are not delayed.

### APP-083 Hard reconciliation timeout
- **Upstream**: [argocd_application_controller.go#L260](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-application-controller/commands/argocd_application_controller.go#L260), [appcontroller.go#L2223](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2223), [argocd-application-controller-statefulset.yaml#L43](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/application-controller/argocd-application-controller-statefulset.yaml#L43)
- **Maturity**: GA
- **Semantics**: `argocd-cm` `timeout.hard.reconciliation` -> `ARGOCD_HARD_RECONCILIATION_TIMEOUT` -> `--app-hard-resync` (seconds, default 0 = disabled). When enabled and `reconciledAt` is older than it (or nil), the periodic refresh is a hard refresh (manifest cache bypass), reason `comparison expired, requesting hard refresh`.
- **Edge cases**: Hard expiry wins over soft expiry when both apply.
- **Configuration surface**: key/env/flag above.
- **Exposure**: controller configuration.
- **Tests required**:
  - Positive: hard timeout 60s triggers manifest regeneration every ~60s.

### APP-084 Refresh triggers and comparison levels
- **Upstream**: [appcontroller.go#L84](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L84), [appcontroller.go#L2194](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2194), [appcontroller.go#L1053](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1053), [appcontroller.go#L1939](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1939), [appcontroller.go#L1729](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1729)
- **Maturity**: GA
- **Semantics**: Comparison levels: 0 nothing (tree/summary only from cached managed resources; falls back to full when cache or destination unavailable), 1 recent (compare at `status.sync.revision(s)`, no Git resolution), 2 latest (resolve spec targetRevision using revision cache), 3 force-resolve (bypass revision cache). Decision order: refresh annotation -> force-resolve; `spec.source(s)` differ from `comparedTo` -> force-resolve; soft/hard expiry -> latest; destination differs; managedNamespaceMetadata differs; ignoreDifferences differs; controller-requested level (max of queued requests). Post-operation refresh: force-resolve for manual, latest for automated syncs; enabling auto-sync/self-heal -> latest.
- **Edge cases**: If no reason applies the queue item is dropped (no work).
- **Configuration surface**: none.
- **Exposure**: controller behaviour (logs `Refreshing app status (<reason>), level (<n>)`).
- **Tests required**:
  - Positive: editing `spec.source.targetRevision` triggers immediate force-resolve refresh.
  - Positive: editing labels only does not trigger a comparison.

### APP-085 Resource-change-driven refresh
- **Upstream**: [appcontroller.go#L436](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L436), [appcontroller.go#L378](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L378), [cache.go#L403](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/cache.go#L403)
- **Maturity**: GA
- **Semantics**: When a watched cluster object changes, each owning app is refreshed: managed resource -> level "recent"; non-managed descendants -> level "nothing" (tree only). Namespaced unmanaged objects also refresh apps that monitor orphaned resources in that namespace. Skipped when the changed object is the Application itself, when the app cannot be processed (namespace, shard, skip-reconcile), or when its project does not allow the app. Updates filtered by `ignoreResourceUpdates` / `argocd.argoproj.io/ignore-resource-updates` do not trigger refreshes unless health changes (APP-110).
- **Edge cases**: Recent-level comparisons do not update `reconciledAt`.
- **Configuration surface**: resource update ignore rules (other area).
- **Exposure**: controller behaviour.
- **Tests required**:
  - Positive: scaling a managed Deployment manually flips app OutOfSync within seconds.
  - Positive: a self-managing app does not loop.

### APP-086 Repository error grace period
- **Upstream**: [state.go#L696](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L696), [appcontroller.go#L2022](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2022), [argocd_application_controller.go#L262](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-application-controller/commands/argocd_application_controller.go#L262), [argocd_application_controller.go#L51](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-application-controller/commands/argocd_application_controller.go#L51)
- **Maturity**: GA
- **Semantics**: `--repo-error-grace-period-seconds` (env `ARGOCD_REPO_ERROR_GRACE_PERIOD_SECONDS`, default 180 = resync 120 + jitter 60). When manifest generation fails and the comparison is not force-resolve, the first failure and any failures within the grace period since the first are ignored (reconciliation exits without persisting status, log `Ignoring temporary failed attempt to compare app state against repo`); afterwards `ComparisonError` and `Unknown` are recorded. Success clears the tracking.
- **Edge cases**: The first-seen timestamp is keyed by Application name only (not namespace), so same-named apps in different namespaces share it (derived from code).
- **Configuration surface**: flag/env above.
- **Exposure**: controller configuration; status.
- **Tests required**:
  - Positive: brief repo outage (< grace) leaves status unchanged.
  - Positive: manual refresh during an outage shows ComparisonError immediately.

### APP-087 Status persistence and size-limit fallback
- **Upstream**: [appcontroller.go#L2328](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2328), [appcontroller.go#L2364](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2364)
- **Maturity**: GA
- **Semantics**: Status (and annotation removals) are written as a JSON merge patch computed against the informer copy; no-op when unchanged (`No status changes. Skipping patch`). Emits events `Updated sync status: <a> -> <b>` and `Updated health status: <a> -> <b>`. If the API server rejects the patch as too large, the controller retries with the original status plus a single `UnknownError`: `Application status exceeds the Kubernetes resource size limit and could not be persisted. The displayed status may be stale. Reduce the number of managed resources, set ApplyOutOfSyncOnly=true, lower spec.revisionHistoryLimit, or split the Application.` NotFound evicts the app from the informer cache.
- **Edge cases**: Fallback patch failure is only logged.
- **Configuration surface**: none.
- **Exposure**: CRD status; events.
- **Tests required**:
  - Negative: app with thousands of resources exceeding etcd limit -> UnknownError with exact message.

### APP-088 Skip-reconcile annotation on an Application
- **Upstream**: [common.go#L247](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L247), [appcontroller.go#L2643](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2643), [appcontroller.go#L2655](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2655), [feature-maturity.md#L36](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/feature-maturity.md#L36), [skip_reconcile.md#L10](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/skip_reconcile.md#L10)
- **Maturity**: Alpha
- **Semantics**: `argocd.argoproj.io/skip-reconcile` parsed with Go `strconv.ParseBool` (`1, t, T, TRUE, true, True` = skip). When true the controller ignores the app's add/update/delete events and resource-change refreshes: no comparison, no status writes (a newly created app has no `status`), no auto-sync, no hydration. Setting it to false or removing it resumes processing. Unparsable values are logged at debug and do not skip.
- **Edge cases**: Derived from code reading: because deletion finalization is driven from the same event queues, an app being deleted while skip-reconcile is true keeps its finalizers until the annotation is removed. Third parties may write `status` (no status subresource).
- **Configuration surface**: annotation on Application.
- **Exposure**: CRD annotation.
- **Tests required**:
  - Positive: app created with annotation has no status after several resync periods.
  - Positive: removing annotation leads to reconciliation.
  - Negative: value `yes` does not skip.

### APP-089 Skip-reconcile annotation on a cluster Secret
- **Upstream**: [cache.go#L59](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sharding/cache.go#L59), [appcontroller.go#L2669](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2669), [annotations-and-labels.md#L14](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/annotations-and-labels.md#L14)
- **Maturity**: Alpha
- **Semantics**: If the destination cluster's annotations contain `argocd.argoproj.io/skip-reconcile` parsing to true, `IsManagedCluster` returns false for every shard, so no Application targeting that cluster is processed.
- **Edge cases**: Apps whose destination cannot be resolved are treated as managed (processed, then reported with conditions).
- **Configuration surface**: annotation on cluster Secret.
- **Exposure**: cluster secret metadata.
- **Tests required**:
  - Positive: annotating the cluster secret freezes all its apps' status.

### APP-090 manifest-generate-paths annotation
- **Upstream**: [application_annotations.go#L9](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/application_annotations.go#L9), [path.go#L115](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/app/path/path.go#L115), [state.go#L457](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L457), [state.go#L373](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L373), [annotations-and-labels.md#L11](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/annotations-and-labels.md#L11)
- **Maturity**: GA
- **Semantics**: `argocd.argoproj.io/manifest-generate-paths` on the Application: `;`-separated paths, each trimmed; empty entries skipped; entries starting with `/` are repo-root-relative (leading `/` stripped); others are joined to the source `path` and cleaned (`.` = the source path). When the app has a previously synced/compared revision, the repo is not shallow (`depth == 0`) and the annotation is non-empty, the controller asks the repo server whether files under those paths changed between the synced revision and the new revision (`UpdateRevisionForPaths`); if not, cached manifests are reused for the new revision and the source is reported as unchanged (affects auto-sync "already attempted" logic). Also passed to manifest generation and used by webhooks (other area).
- **Edge cases**: Shallow clones disable the optimization. For hydrator apps the sync source always uses its own `syncSource.path` (APP-133).
- **Configuration surface**: annotation.
- **Exposure**: CRD annotation.
- **Tests required**:
  - Positive: commit outside annotated paths does not regenerate manifests and does not trigger auto-sync; commit inside does.
  - Positive: `.; /shared` covers source path and repo-root `shared`.

### APP-091 App-of-apps (Applications managing Applications)
- **Upstream**: [types.go#L2155](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2155), [health.go#L80](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/health.go#L80), [appcontroller.go#L378](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L378), [info.go#L92](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/info.go#L92), [utils.tsx#L486](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/utils.tsx#L486), [cluster-bootstrapping.md#L194](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/cluster-bootstrapping.md#L194), [app_deletion.md#L75](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/app_deletion.md#L75)
- **Maturity**: GA
- **Semantics**: A parent app rendering `argoproj.io/Application` objects: `status.summary.isAppOfApps = true`; child Application health counts only if a health check exists (none built in, APP-092) and is neither Missing nor Unknown; an Application that lists itself among its resources is excluded from its own health, from cascade deletion and from resource-change refresh loops. Cascading delete of the parent deletes child Application objects, which then cascade according to their own finalizers. Child Application tree nodes carry `managed-by-url` info for cross-instance links. The UI treats an Application with a non-empty `app.kubernetes.io/part-of` label as a "child application" in delete dialogs and allows deleting it from the parent's resource tree through the Application delete API.
- **Edge cases**: Without the health customization, sync waves do not wait for child apps to become healthy.
- **Configuration surface**: child app finalizers; health customization.
- **Exposure**: CRD; UI tree (click-through to child app), delete dialog.
- **Tests required**:
  - Positive: parent with two children has `isAppOfApps: true`; parent health unaffected by Unknown child.
  - Positive: cascade-deleting the parent with finalizer-bearing children removes the children's resources.

### APP-092 Child Application health customization and toggle-auto-sync action
- **Upstream**: [health.md#L29](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/health.md#L29), [health.go#L121](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/health/health.go#L121), [discovery.lua#L1](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/argoproj.io/Application/actions/discovery.lua#L1), [action.lua#L1](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/resource_customizations/argoproj.io/Application/actions/toggle-auto-sync/action.lua#L1)
- **Maturity**: GA
- **Semantics**: No built-in health check exists for `argoproj.io/Application` (removed in 1.8); operators may add `resource.customizations.health.argoproj.io_Application` in `argocd-cm` (documented Lua: `Progressing` until `status.health.status` exists, then mirror it). A built-in resource action `toggle-auto-sync` is discoverable on Application resources: display name `Disable Auto-Sync` when automated sync is enabled (`automated` present with `enabled` true or unset) and `Enable Auto-Sync` otherwise; running it sets `automated.enabled=false`, or creates/sets `automated.enabled=true`.
- **Edge cases**: Action modifies the child Application object in the cluster (parent may revert it on next sync unless ignored).
- **Configuration surface**: `argocd-cm` `resource.customizations.health.argoproj.io_Application`; built-in action.
- **Exposure**: UI resource actions menu on child Application nodes; API/CLI resource actions (other area).
- **Tests required**:
  - Positive: with the documented Lua, parent waits for child health in sync waves.
  - Positive: toggle-auto-sync flips `spec.syncPolicy.automated.enabled` both ways.

### APP-093 ignore-healthcheck annotation
- **Upstream**: [common.go#L224](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L224), [health.go#L40](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/health.go#L40), [health.md#L292](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/health.md#L292)
- **Maturity**: GA
- **Semantics**: `argocd.argoproj.io/ignore-healthcheck: "true"` on a live managed (immediate child) resource excludes its health from app health aggregation; the resource still counts as "has resources/has live resources" for the Missing rule.
- **Edge cases**: Only the exact string `true` is recognised. Not listed in `annotations-and-labels.md`.
- **Configuration surface**: annotation on managed resources.
- **Exposure**: resource metadata.
- **Tests required**:
  - Positive: Degraded Deployment with the annotation -> app Healthy.
  - Negative: value `True` is not honoured.

### APP-094 Application notice: content
- **Upstream**: [notice.ts#L3](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-notice/notice.ts#L3), [notice.ts#L95](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-notice/notice.ts#L95), [notice.ts#L108](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-notice/notice.ts#L108), [notice.ts#L119](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-notice/notice.ts#L119), [notice.ts#L140](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-notice/notice.ts#L140), [notice-banner.tsx#L26](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-notice/notice-banner.tsx#L26), [application-details.tsx#L1005](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-details/application-details.tsx#L1005), [application-tile.tsx#L82](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/applications-list/application-tile.tsx#L82), [application-notice.md#L18](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/application-notice.md#L18)
- **Maturity**: GA
- **Semantics**: `notice.argocd.argoproj.io/content` on an Application enables a per-app notice (UI-only; no server/controller logic). Missing or whitespace-only content -> no notice. Unicode bidi override characters (U+202A-U+202E, U+2066-U+2069) are stripped; content longer than 500 characters is cut to 499 characters plus `…`; list/tile tooltips preview at most 200 characters. Banner is rendered on the details page between the status panel and the resource views; an icon appears next to the app name in tile and list views. Dismissals are stored per user in view preferences under a key `<ns>/<name>:<hash(content)>` (changing content re-shows the banner); at most 200 dismissals are kept (oldest dropped).
- **Edge cases**: Independent of the global `ui.bannercontent` banner.
- **Configuration surface**: annotation `notice.argocd.argoproj.io/content`.
- **Exposure**: UI only (details banner, list/tile icon).
- **Tests required**:
  - Positive: 600-char content renders 499 chars + ellipsis.
  - Positive: dismissing hides banner; editing content re-shows it.
  - Negative: whitespace-only content shows nothing; bidi characters removed.

### APP-095 Application notice: severity
- **Upstream**: [notice.ts#L27](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-notice/notice.ts#L27), [notice.ts#L33](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-notice/notice.ts#L33), [notice.ts#L55](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-notice/notice.ts#L55), [application-notice.md#L31](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/application-notice.md#L31)
- **Maturity**: GA
- **Semantics**: `notice.argocd.argoproj.io/severity`: `info` (default), `warning`, `critical`; trimmed and lower-cased; unknown values fall back to `info`. Drives banner styling and default icon (`fa-info-circle`, `fa-exclamation-triangle`, `fa-exclamation-circle`).
- **Edge cases**: `WARNING` is accepted (case-insensitive).
- **Configuration surface**: annotation.
- **Exposure**: UI only.
- **Tests required**:
  - Positive: each severity renders the matching icon; `bogus` renders info.

### APP-096 Application notice: url
- **Upstream**: [notice.ts#L82](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-notice/notice.ts#L82), [utils.ts#L44](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/shared/utils.ts#L44), [application-notice.md#L26](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/application-notice.md#L26)
- **Maturity**: GA
- **Semantics**: `notice.argocd.argoproj.io/url`: if a valid `http:`/`https:` URL after trimming, the banner text and icon link to its canonical form (`URL.href`); otherwise no link.
- **Edge cases**: `javascript:`/`data:` URLs are dropped.
- **Configuration surface**: annotation.
- **Exposure**: UI only.
- **Tests required**:
  - Negative: `javascript:alert(1)` renders no link; positive: https URL links.

### APP-097 Application notice: permanent
- **Upstream**: [notice.ts#L71](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-notice/notice.ts#L71), [notice-banner.tsx#L48](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-notice/notice-banner.tsx#L48), [application-notice.md#L27](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/application-notice.md#L27)
- **Maturity**: GA
- **Semantics**: `notice.argocd.argoproj.io/permanent`: `true` (trimmed, case-insensitive) removes the dismiss button and ignores previous dismissals; default false.
- **Edge cases**: List/tile icon is always shown regardless of dismissal.
- **Configuration surface**: annotation.
- **Exposure**: UI only.
- **Tests required**:
  - Positive: permanent banner has no dismiss control and reappears even if previously dismissed.

### APP-098 Application notice: icon
- **Upstream**: [notice.ts#L43](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-notice/notice.ts#L43), [notice.ts#L100](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-notice/notice.ts#L100), [application-notice.md#L39](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/application-notice.md#L39)
- **Maturity**: GA
- **Semantics**: `notice.argocd.argoproj.io/icon` overrides the icon only if (trimmed) it is one of `fa-info-circle`, `fa-exclamation-triangle`, `fa-exclamation-circle`, `fa-bell`, `fa-wrench`, `fa-clock`, `fa-bullhorn`, `fa-life-ring`, `fa-shield-alt`; otherwise the severity default is used.
- **Edge cases**: Arbitrary class injection is prevented by the allowlist.
- **Configuration surface**: annotation.
- **Exposure**: UI only.
- **Tests required**:
  - Positive: `fa-bell` used; `fa-bell big-red` falls back to severity icon.

### APP-099 Application notice: scope
- **Upstream**: [notice.ts#L63](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-notice/notice.ts#L63), [notice.ts#L162](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-notice/notice.ts#L162), [application-notice.md#L29](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/application-notice.md#L29)
- **Maturity**: GA
- **Semantics**: `notice.argocd.argoproj.io/scope`: `banner` (details banner only), `icon` (list/tile icon only), `both` (default); trimmed, case-insensitive; unknown -> `both`.
- **Edge cases**: none beyond fallback.
- **Configuration surface**: annotation.
- **Exposure**: UI only.
- **Tests required**:
  - Positive: `icon` hides the banner; `banner` hides the icon.

### APP-100 link.argocd.argoproj.io/* external link annotations
- **Upstream**: [common.go#L233](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L233), [info.go#L44](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/info.go#L44), [settings.go#L1708](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L1708), [types.go#L2160](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2160), [application-summary.tsx#L278](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-summary/application-summary.tsx#L278), [external-url.md#L6](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/external-url.md#L6)
- **Maturity**: GA
- **Semantics**: Any annotation key with prefix `link.argocd.argoproj.io/` (any suffix, multiple allowed) whose value is `<url>` or `<title>|<url>`. On cluster resources, the URL part must parse with scheme `http`/`https` (otherwise ignored); the full value is added to the node's `networkingInfo.externalURLs` (external-link icon on the node) and to `status.summary.externalURLs`. On the Application object itself, values are added to `status.summary.externalURLs` verbatim. The UI splits on `|` to render title and href and drops non-http(s) URLs.
- **Edge cases**: Link annotations are kept even when `ignore-default-links` is true.
- **Configuration surface**: annotations on Applications and on managed resources.
- **Exposure**: CRD status summary; UI node link icons and summary `URLS`.
- **Tests required**:
  - Positive: `Dashboard|https://grafana/x` renders link text `Dashboard`.
  - Negative: `javascript:` value on a resource is not in `externalURLs`.

### APP-101 ignore-default-links annotation
- **Upstream**: [common.go#L242](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L242), [info.go#L250](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/info.go#L250), [info.go#L459](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/info.go#L459), [external-url.md#L28](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/external-url.md#L28)
- **Maturity**: GA
- **Semantics**: `argocd.argoproj.io/ignore-default-links` (parsed with `ParseBool`) on an Ingress (`extensions`/`networking.k8s.io`) or Gateway API Gateway suppresses URLs auto-generated from its spec/addresses; `link.argocd.argoproj.io/*` URLs remain.
- **Edge cases**: Unparsable values keep defaults (links shown).
- **Configuration surface**: annotation on Ingress/Gateway.
- **Exposure**: status summary; UI.
- **Tests required**:
  - Positive: Ingress with `ignore-default-links: "true"` contributes no host URL.

### APP-102 managed-by-url annotation
- **Upstream**: [application_annotations.go#L14](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/application_annotations.go#L14), [argo.go#L407](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L407), [application.go#L1391](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1391), [info.go#L786](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/info.go#L786), [utils.tsx#L2039](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/utils.tsx#L2039), [utils.ts#L53](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/shared/utils.ts#L53), [managed-by-url.md#L84](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/managed-by-url.md#L84)
- **Maturity**: GA
- **Semantics**: `argocd.argoproj.io/managed-by-url` on an Application names the Argo CD instance that manages it. API create/update/patch validate it: must parse and use `http`/`https` (`InvalidArgument: application spec for <app> is invalid: InvalidSpecError: invalid managed-by URL: URL must include http or https protocol` or `... failed to parse URL: ...`). The controller copies it into the Application node's info (`managed-by-url`) in the parent's resource tree. UI links to an Application (from node info first, then the app's annotation) become `<managed-by-url>/applications/<ns>/<name>` opened as external; an invalid value falls back to the local link, logs a browser console warning and shows `managed-by-url: invalid url provided`.
- **Edge cases**: Declarative (kubectl) creation is not validated by the controller. Empty value is ignored.
- **Configuration surface**: annotation.
- **Exposure**: CRD annotation; API validation; UI tree/list links.
- **Tests required**:
  - Negative: API create with `argocd.example.com` (no scheme) -> exact InvalidArgument.
  - Positive: child app node in parent tree links to the other instance.

### APP-103 pref.argocd.argoproj.io/default-view annotation
- **Upstream**: [models.ts#L166](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/shared/models.ts#L166), [application-details.tsx#L633](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-details/application-details.tsx#L633), [ui-customization.md#L7](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/ui-customization.md#L7)
- **Maturity**: GA
- **Semantics**: Default details view for the app: `tree` (default), `pods`, `network`, `list`. A `view` URL query parameter takes precedence.
- **Edge cases**: Value is not validated by the UI code (cast as-is).
- **Configuration surface**: annotation.
- **Exposure**: UI only.
- **Tests required**:
  - Positive: `default-view: list` opens the list view; `?view=tree` overrides.

### APP-104 pref.argocd.argoproj.io/default-pod-sort annotation
- **Upstream**: [models.ts#L167](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/shared/models.ts#L167), [application-details.tsx#L646](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-details/application-details.tsx#L646), [ui-customization.md#L9](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/ui-customization.md#L9)
- **Maturity**: GA
- **Semantics**: Default pod-view grouping: `node`, `parentResource`, `topLevelResource`. `podSortMode` URL query parameter takes precedence.
- **Edge cases**: Not validated.
- **Configuration surface**: annotation.
- **Exposure**: UI only.
- **Tests required**:
  - Positive: `default-pod-sort: topLevelResource` groups pods by top-level resource.

### APP-105 argocd.argoproj.io/compare-options annotation key
- **Upstream**: [common.go#L218](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L218), [state.go#L864](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L864), [state.go#L885](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L885), [state.go#L994](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L994), [annotations-and-labels.md#L8](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/annotations-and-labels.md#L8)
- **Maturity**: GA
- **Semantics**: Comma-separated options. On the Application: `ServerSideDiff=true` enables server-side diff for this app, `ServerSideDiff=false` disables it even when enabled controller-wide, `IncludeMutationWebhook=true` includes mutation-webhook changes in server-side diff. On a managed resource: `IgnoreExtraneous` keeps an extraneous (prune-candidate) resource from making the app OutOfSync. Full semantics owned by the diff area.
- **Edge cases**: Option matching is exact per comma-separated item.
- **Configuration surface**: annotation on Application or resources.
- **Exposure**: metadata.
- **Tests required**:
  - Positive: app-level `ServerSideDiff=false` overrides controller-wide enablement.

### APP-106 argocd.argoproj.io/sync-options annotation key
- **Upstream**: [types.go#L12](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/common/types.go#L12), [appcontroller.go#L1334](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1334), [state.go#L1175](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L1175), [annotations-and-labels.md#L15](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/annotations-and-labels.md#L15)
- **Maturity**: GA
- **Semantics**: Per-resource sync options (comma-separated `Key=Value`). In this area: resource-level `Delete=false` / `Delete=confirm` and `Prune=confirm` override app-level `syncPolicy.syncOptions` for cascade deletion and deletion confirmation (APP-061, APP-062). All other options are owned by the sync area.
- **Edge cases**: Resource-level value wins over app-level only when the key is present on the resource.
- **Configuration surface**: annotation on managed resources.
- **Exposure**: metadata.
- **Tests required**:
  - Positive: with app-level `Delete=false`, cascade delete keeps resources without the key but deletes a resource annotated `Delete=true` (resource-level value wins when present).

### APP-107 argocd.argoproj.io/sync-wave annotation key
- **Upstream**: [types.go#L14](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/common/types.go#L14), [state.go#L953](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L953), [sort_delete.go#L20](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sort_delete.go#L20), [annotations-and-labels.md#L16](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/annotations-and-labels.md#L16)
- **Maturity**: GA
- **Semantics**: Integer wave (default 0). In this area it populates `status.resources[].syncWave` and orders cascade deletion (highest wave first). Sync ordering semantics belong to the sync area.
- **Edge cases**: Unparsable values handled by gitops-engine (sync area).
- **Configuration surface**: annotation.
- **Exposure**: metadata; status.
- **Tests required**:
  - Positive: `syncWave` reported in status; deletion order reverse of waves.

### APP-108 argocd.argoproj.io/hook and hook-delete-policy annotation keys
- **Upstream**: [types.go#L16](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/common/types.go#L16), [types.go#L116](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/common/types.go#L116), [types.go#L135](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/common/types.go#L135), [hook.go#L33](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/hook.go#L33), [hook.go#L13](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/hook/hook.go#L13), [annotations-and-labels.md#L9](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/annotations-and-labels.md#L9)
- **Maturity**: GA
- **Semantics**: `argocd.argoproj.io/hook` (comma list): `PreSync`, `Sync`, `PostSync`, `SyncFail`, `Skip` (sync area) and `PreDelete`, `PostDelete` (this area, APP-064/APP-065; Helm `pre-delete`/`post-delete` are equivalent). `argocd.argoproj.io/hook-delete-policy`: `HookSucceeded`, `HookFailed`, `BeforeHookCreation` (cleanup in APP-066). Hook resources are flagged `hook: true` in status and excluded from sync status and health aggregation. Internal finalizer `argocd.argoproj.io/hook-finalizer` is placed on sync hooks by the engine (not documented in the annotations page).
- **Edge cases**: A resource with the hook annotation but no recognised type is "ignored" (no status, no health).
- **Configuration surface**: annotations on rendered resources.
- **Exposure**: metadata; status.
- **Tests required**:
  - Positive: `PreDelete` hook not applied during normal sync.

### APP-109 argocd.argoproj.io/application-set-refresh annotation key
- **Upstream**: [common.go#L444](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L444), [annotations-and-labels.md#L7](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/annotations-and-labels.md#L7)
- **Maturity**: GA
- **Semantics**: Set to `"true"` on an ApplicationSet when a webhook requests its refresh; removed by the ApplicationSet controller at the end of reconciliation. Listed here for reference-table completeness; behaviour owned by the ApplicationSet area.
- **Edge cases**: Not applicable to Applications.
- **Configuration surface**: annotation on ApplicationSet.
- **Exposure**: CRD (ApplicationSet).
- **Tests required**:
  - Positive (ApplicationSet area): annotation removed after reconcile.

### APP-110 argocd.argoproj.io/ignore-resource-updates annotation key
- **Upstream**: [cache.go#L81](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/cache.go#L81), [cache.go#L402](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/cache.go#L402), [reconcile.md#L127](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/reconcile.md#L127), [annotations-and-labels.md#L19](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/annotations-and-labels.md#L19)
- **Maturity**: GA
- **Semantics**: On an untracked object (no owning app and not itself an Application), `ParseBool(value)==true` makes it eligible for `ignoreResourceUpdates` filtering (its manifest is hashed so updates that only touch ignored fields and keep health unchanged do not trigger app refreshes). Tracked resources and Applications are always eligible.
- **Edge cases**: Unparsable values -> not eligible. Requires `resource.ignoreResourceUpdatesEnabled` (default true) and matching rules (other area).
- **Configuration surface**: annotation; `argocd-cm` ignoreResourceUpdates rules.
- **Exposure**: metadata.
- **Tests required**:
  - Positive: Pod from a CronJob template with the annotation and `/status` ignore rule does not trigger app refresh on status-only updates.

### APP-111 argocd.argoproj.io/client-side-apply-migration-manager annotation
- **Upstream**: [common.go#L221](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L221), [sync.go#L228](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sync.go#L228), [types.go#L55](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/sync/common/types.go#L55), [sync-options.md#L382](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/sync-options.md#L382)
- **Maturity**: GA
- **Semantics**: Application-level annotation naming the field manager used when migrating client-side-applied fields to server-side apply; default `kubectl-client-side-apply`. Migration behaviour itself is owned by the sync area.
- **Edge cases**: Not listed in `annotations-and-labels.md`.
- **Configuration surface**: annotation on Application.
- **Exposure**: metadata.
- **Tests required**:
  - Positive: custom manager value is used in SSA migration (sync area test).

### APP-112 Label keys reference (instance labels, secret-type and others)
- **Upstream**: [common.go#L188](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L188), [common.go#L194](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L194), [common.go#L198](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L198), [common.go#L207](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L207), [annotations-and-labels.md#L30](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/annotations-and-labels.md#L30)
- **Maturity**: GA
- **Semantics**: Documented labels: `argocd.argoproj.io/instance` (recommended custom tracking label, active only when configured as `application.instanceLabelKey`, APP-073) and `argocd.argoproj.io/secret-type` on Secrets (`cluster`, `repository`, `repo-creds`, `scm-creds` documented; code also defines `repository-write` and `repo-write-creds`, used by the source hydrator, APP-125). Code-defined but undocumented on that page: `app.kubernetes.io/instance` (default tracking label), legacy `applications.argoproj.io/app-name`, cluster-secret labels `argocd.argoproj.io/kubernetes-version` and `argocd.argoproj.io/auto-label-cluster-info` (cluster area). The UI also interprets `app.kubernetes.io/part-of` on Applications (APP-091).
- **Edge cases**: See doc differences for the undocumented values.
- **Configuration surface**: labels.
- **Exposure**: metadata.
- **Tests required**:
  - Positive: secrets labelled `repository-write` are used only for hydrator pushes (APP-125).

### APP-113 Source Hydrator enablement and components
- **Upstream**: [argocd_application_controller.go#L309](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-application-controller/commands/argocd_application_controller.go#L309), [argocd_application_controller.go#L265](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-application-controller/commands/argocd_application_controller.go#L265), [appcontroller.go#L224](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L224), [argocd_server.go#L342](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-server/commands/argocd_server.go#L342), [settings.go#L124](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/settings/settings.go#L124), [argocd-cmd-params-cm.yaml#L12](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cmd-params-cm.yaml#L12), [argocd-cmd-params-cm.yaml#L23](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cmd-params-cm.yaml#L23), [feature-maturity.md#L68](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/feature-maturity.md#L68), [source-hydrator.md#L16](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/source-hydrator.md#L16)
- **Maturity**: Beta
- **Semantics**: Disabled by default. `argocd-cmd-params-cm` `hydrator.enabled: "true"` -> env `ARGOCD_HYDRATOR_ENABLED` -> `--hydrator-enabled` on both the application controller (creates the hydrator, the per-app hydrate queue and the hydration queue) and the API server (exposes `hydratorEnabled` in `/api/v1/settings`, enabling hydrator UI and write-credential management). Requires the commit server (`argocd-commit-server`, shipped in `*-install-with-hydrator.yaml` manifests); controller address `--commit-server` / `ARGOCD_APPLICATION_CONTROLLER_COMMIT_SERVER` / cmd-params `commit.server`, default `argocd-commit-server:8086`. Components must be restarted after changing the flag.
- **Edge cases**: Derived from code reading: with the feature disabled, Applications that have `spec.sourceHydrator` are still compared and synced from the sync source (branch/path) but are never hydrated and `status.sourceHydrator` stays empty; nothing rejects such specs.
- **Configuration surface**: cmd-params `hydrator.enabled`, `commit.server`; env/flags above.
- **Exposure**: configuration; settings API field `hydratorEnabled`; UI gating.
- **Tests required**:
  - Positive: with flag on, hydrator app gets `status.sourceHydrator.currentOperation`.
  - Positive: with flag off, same app syncs from existing hydrated branch without hydrating.

### APP-114 Hydration need detection
- **Upstream**: [hydrator.go#L124](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/hydrator/hydrator.go#L124), [hydrator.go#L635](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/hydrator/hydrator.go#L635), [hydrator.go#L613](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/hydrator/hydrator.go#L613), [state.go#L424](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L424), [appcontroller.go#L2802](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2802), [source-hydrator.md#L161](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/source-hydrator.md#L161)
- **Maturity**: Beta
- **Semantics**: Every informer update of an app (including periodic resyncs and annotation patches) enqueues it on the hydrate queue. Decision order: no `sourceHydrator` -> skip; no `currentOperation` -> hydrate; phase `Hydrating` -> skip; hard hydrate requested -> hydrate; `spec.sourceHydrator` differs from `currentOperation.sourceHydrator` -> hydrate; phase `Failed`: explicit hydrate request -> retry now, failed more than 2 minutes ago (or `finishedAt` nil counts as within cooldown) -> retry, no `lastComparedDryRevision` -> wait; otherwise compare revisions: empty `lastComparedDryRevision` -> hydrate; else resolve the dry `targetRevision` (bypassing the revision cache when a hydrate was requested) and hydrate if it differs (or, with `manifest-generate-paths`, if the annotated paths changed). The resolved dry SHA is stored in `status.sourceHydrator.lastComparedDryRevision` on every evaluation. Revision-evaluation errors -> skip (logged).
- **Edge cases**: Adding/removing Applications does not trigger hydration by itself (one hydration per dry commit). AddFunc does not enqueue hydration; the first update/resync does.
- **Configuration surface**: none beyond APP-115/APP-133.
- **Exposure**: controller behaviour; status.
- **Tests required**:
  - Positive: new dry commit -> hydration; unchanged commit -> no hydration but `lastComparedDryRevision` refreshed.
  - Positive: editing `spec.sourceHydrator` forces re-hydration.

### APP-115 Hydrate annotation argocd.argoproj.io/hydrate (normal/hard)
- **Upstream**: [application_annotations.go#L7](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/application_annotations.go#L7), [types.go#L548](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L548), [types.go#L3598](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L3598), [hydrator_dependencies.go#L102](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/hydrator_dependencies.go#L102), [application.go#L818](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L818)
- **Maturity**: Beta
- **Semantics**: `argocd.argoproj.io/hydrate: hard` forces hydration; any other value is `normal` (bypass revision cache and retry a failed hydration immediately). Set by API refresh (`refresh=hard` -> `hard`, otherwise `normal`) and by dry-source webhooks (webhook area). Removed by the controller each time it persists hydration status for the app (also when no hydration is needed).
- **Edge cases**: A request that arrives while the app is `Hydrating` is consumed without effect. The annotation is only removed for apps with `spec.sourceHydrator` while the hydrator is enabled (see APP-080). Not listed in `annotations-and-labels.md`.
- **Configuration surface**: annotation.
- **Exposure**: CRD annotation; API refresh; UI Refresh/Hard Refresh.
- **Tests required**:
  - Positive: hard refresh re-hydrates even with unchanged dry SHA (new commit only if output changed).
  - Positive: annotation removed after evaluation.

### APP-116 Hydration grouping and atomicity
- **Upstream**: [hydrator.go#L161](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/hydrator/hydrator.go#L161), [types.go#L5](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/hydrator/types/types.go#L5), [hydrator.go#L184](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/hydrator/hydrator.go#L184), [hydrator.go#L301](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/hydrator/hydrator.go#L301), [hydrator.go#L414](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/hydrator/hydrator.go#L414), [hydrator.go#L698](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/hydrator/hydrator.go#L698)
- **Maturity**: Beta
- **Semantics**: Hydration work is keyed by `{normalized dry repoURL, dry targetRevision, normalized destination repoURL, destination branch (hydrateTo.targetBranch or syncSource.targetBranch)}`; the rate-limited queue never processes one key concurrently. For a key, all processable apps sharing it are loaded, every app not already Hydrating gets `currentOperation{phase: Hydrating, startedAt: now, sourceHydrator: <spec copy>}`, all apps are validated, then manifests for the first app are generated at the dry `targetRevision` and its resolved SHA is used for all other apps (generated in parallel); a single commit containing every app's path is produced. If any app fails, no commit is made and every app in the group is marked `Failed`: the failing apps with their own error and the others with `cannot hydrate because application <ns/name> has an error` / `... and <n> more have errors` (first failing app by sorted qualified name), messages prefixed `Failed to hydrate: `.
- **Edge cases**: If the last successful hydration of the first app already used the same dry SHA, the commit step is skipped and the previous hydrated SHA is reused (only the first app is inspected).
- **Configuration surface**: none.
- **Exposure**: status per app.
- **Tests required**:
  - Positive: two apps sharing dry repo/branch produce one commit touching both paths.
  - Negative: one app with a bad path -> both apps Failed with specific and generic messages.

### APP-117 Hydration validation rules
- **Upstream**: [hydrator.go#L354](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/hydrator/hydrator.go#L354), [hydrator.go#L385](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/hydrator/hydrator.go#L385), [hydrator.go#L397](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/hydrator/hydrator.go#L397), [hydrator.go#L712](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/hydrator/hydrator.go#L712), [source-hydrator.md#L149](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/source-hydrator.md#L149)
- **Maturity**: Beta
- **Semantics**: Per app: project must be resolvable and permit the app (`failed to get project "<p>": <err>`); dry repo permitted (`application repo <url> is not permitted in project '<p>'`); destination path (`syncSource.path`) must not be a root path after `filepath.Clean` (`""`, `.`, `/`): `app is configured to hydrate to the repository root (branch "<b>", path "<p>") which is not allowed`; destination repo permitted (`destination repo <url> is not permitted in project '<p>'`); no two apps in the group may share destination repo+path (`app <other> hydrator uses the same destination: repo=<url>, path=<p>` set on both).
- **Edge cases**: Duplicate detection does not normalize paths (`a/` vs `a` are different).
- **Configuration surface**: AppProject `sourceRepos`.
- **Exposure**: status messages.
- **Tests required**:
  - Negative: each message above.

### APP-118 Dry-manifest rendering for hydration
- **Upstream**: [hydrator.go#L551](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/hydrator/hydrator.go#L551), [hydrator_dependencies.go#L53](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/hydrator_dependencies.go#L53), [resource_tracking.go#L192](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/resource_tracking.go#L192), [source-hydrator.md#L184](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/source-hydrator.md#L184), [source-hydrator.md#L650](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/source-hydrator.md#L650)
- **Maturity**: Beta
- **Semantics**: The dry source (`repoURL`, `path`, `targetRevision`, `helm`/`kustomize`/`directory`/`plugin`) is rendered by the repo server with manifest and revision caches bypassed and without sending the destination cluster's version/API versions. Tracking metadata is stripped from every object according to the tracking method (annotation methods remove `tracking-id` and `installation-id`; label methods remove `app.kubernetes.io/instance`) so hydrated output is deterministic. The repo server's `commands` for the render are recorded per path.
- **Edge cases**: Errors: `failed to get manifests: failed to get repo objects for app "<ns/name>": ...`.
- **Configuration surface**: dry source options.
- **Exposure**: hydrated Git output.
- **Tests required**:
  - Positive: hydrated `manifest.yaml` contains no `argocd.argoproj.io/tracking-id`.
  - Positive: Helm dry source with `valueFiles` and `parameters` renders as in a normal app.

### APP-119 Hydrated output layout written by the commit server
- **Upstream**: [hydratorhelper.go#L38](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/commitserver/commit/hydratorhelper.go#L38), [hydratorhelper.go#L26](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/commitserver/commit/hydratorhelper.go#L26), [hydratorhelper.go#L109](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/commitserver/commit/hydratorhelper.go#L109), [hydratorhelper.go#L179](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/commitserver/commit/hydratorhelper.go#L179), [commit.go#L21](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/commitserver/commit/commit.go#L21), [hydrator.go#L15](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/hydrator/hydrator.go#L15), [source-hydrator.md#L153](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/source-hydrator.md#L153)
- **Maturity**: Beta
- **Semantics**: In the checked-out destination branch the commit server writes: at the repository root `hydrator.metadata` (JSON, 2-space indent, no HTML escaping: `repoURL`, `drySha`, `author`, `date` (RFC3339), `subject`, `body` (dry commit body minus recognised trailers), `references`) and `.gitattributes` (`**/README.md linguist-generated=true` and `**/hydrator.metadata linguist-generated=true`); per app path (created with mode 0755) `manifest.yaml` (truncated and rewritten; YAML stream of all objects in render order, 2-space indent). If `manifest.yaml` changed versus git (untracked counts as changed), it also writes per-path `hydrator.metadata` (`repoURL`, `drySha`, `commands`) and `README.md` from the README template (APP-127). Existing other files in the path are never deleted. Changes are committed with `git add .` and pushed to the target branch.
- **Edge cases**: Path `.` is mapped to the repo root by the commit server (the controller rejects root paths first). When an app is deleted or its path changes, old output stays in Git.
- **Configuration surface**: none.
- **Exposure**: Git repository contents.
- **Tests required**:
  - Positive: first hydration creates `manifest.yaml`, `hydrator.metadata`, `README.md` per path plus root `hydrator.metadata` and `.gitattributes`.
  - Positive: stale extra file in path is preserved.

### APP-120 Branch handling (sync branch bootstrap, target branch creation)
- **Upstream**: [commit.go#L106](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/commitserver/commit/commit.go#L106), [commit.go#L137](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/commitserver/commit/commit.go#L137), [client.go#L1544](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/client.go#L1544), [client.go#L1574](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/client.go#L1574), [hydrator.go#L423](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/hydrator/hydrator.go#L423)
- **Maturity**: Beta
- **Semantics**: Request requires repo, repo URL, target branch and sync branch (`repo is required`, `repo URL is required`, `target branch is required`, `sync branch is required`). The repo is cloned (full fetch) into a temp dir. The sync branch (`syncSource.targetBranch` of the first app) is checked out; if it does not exist it is created as an orphan branch with an empty commit `Initial commit for <branch>` and pushed. The target branch (hydrateTo or sync branch) is checked out; if missing it is created from the sync branch (pushed with the hydrated commit).
- **Edge cases**: Apps in one group with different sync branches use the first app's sync branch as base (documented FIXME).
- **Configuration surface**: `syncSource.targetBranch`, `hydrateTo.targetBranch`.
- **Exposure**: Git.
- **Tests required**:
  - Positive: nonexistent sync branch is bootstrapped as an orphan with the initial commit.
  - Positive: nonexistent hydrateTo branch is created from the sync branch.

### APP-121 Git-note-based hydration state (refs/notes/hydrator.metadata)
- **Upstream**: [commit.go#L21](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/commitserver/commit/commit.go#L21), [commit.go#L56](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/commitserver/commit/commit.go#L56), [commit.go#L161](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/commitserver/commit/commit.go#L161), [hydratorhelper.go#L222](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/commitserver/commit/hydratorhelper.go#L222), [client.go#L1638](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/client.go#L1638), [client.go#L1661](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/client.go#L1661), [client.go#L1732](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/client.go#L1732), [source-hydrator.md#L543](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/source-hydrator.md#L543)
- **Maturity**: Beta
- **Semantics**: Hydration state is a git note in namespace `hydrator.metadata` (ref `refs/notes/hydrator.metadata`) attached to the hydrated commit, content JSON `{"drySha":"<sha>"}`. Before writing, the commit server fetches the notes ref and reads the note on the target branch HEAD; if its `drySha` equals the request's dry SHA, it returns HEAD without writing. After a new commit (or a no-op, APP-122) it adds/overwrites the note and pushes the notes ref without force, retrying with exponential backoff (50 ms initial, 1 s max interval, 5 s max elapsed) on concurrent-update errors (`fetch first`, `reference already exists`, `incorrect old value`, `failed to update ref`, `cannot lock ref`).
- **Edge cases**: A missing note is not an error; a malformed note -> `json unmarshal failed ...`. Two Argo CD instances writing the same branch are unsupported (note short-circuits the other instance).
- **Configuration surface**: none.
- **Exposure**: Git notes.
- **Tests required**:
  - Positive: re-hydrating the same dry SHA produces no new commit.
  - Recovery: concurrent note pushes from two shards both succeed after retry.

### APP-122 No-op hydration (unchanged manifests)
- **Upstream**: [commit.go#L172](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/commitserver/commit/commit.go#L172), [hydratorhelper.go#L76](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/commitserver/commit/hydratorhelper.go#L76), [client.go#L1741](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/client.go#L1741)
- **Maturity**: Beta
- **Semantics**: If no path's `manifest.yaml` differs from the committed version, no commit is created; the note for the new dry SHA is attached to the current HEAD and that HEAD SHA is returned as `hydratedSha`.
- **Edge cases**: Changes only in root `hydrator.metadata` (e.g. a new dry commit message) are not committed when manifests are unchanged.
- **Configuration surface**: none.
- **Exposure**: Git; `status.sourceHydrator.currentOperation.hydratedSHA` equals previous.
- **Tests required**:
  - Positive: dry commit touching only a README in the dry repo -> no hydrated commit, note added, app status `Hydrated` with unchanged hydratedSHA.

### APP-123 hydrateTo staging branch
- **Upstream**: [types.go#L522](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L522), [types.go#L262](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L262), [state.go#L378](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L378), [source-hydrator.md#L290](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/source-hydrator.md#L290)
- **Maturity**: Beta
- **Semantics**: With `hydrateTo.targetBranch` set, hydrated commits go to that branch (same repo and path as the sync source) while the app keeps syncing from `syncSource.targetBranch`; promotion between branches is external (PR/CI). If the sync branch lacks the path, comparison fails with `failed to generate manifest for source 1 of 1: ... - waiting for an external process to update <syncBranch> from <hydrateToBranch>`.
- **Edge cases**: `hydrateTo` with empty `targetBranch` is an InvalidSpecError (APP-026).
- **Configuration surface**: `spec.sourceHydrator.hydrateTo.targetBranch`.
- **Exposure**: CRD; CLI `--hydrate-to-branch`; UI create panel.
- **Tests required**:
  - Positive: commits land on staging branch; app stays at previous state until merged.
  - Negative: before first promotion, ComparisonError contains the "waiting for an external process" suffix.

### APP-124 Separate destination repository (syncSource.repoURL)
- **Upstream**: [types.go#L515](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L515), [types.go#L439](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L439), [argo.go#L658](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L658), [source-hydrator.md#L119](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/source-hydrator.md#L119)
- **Maturity**: Beta
- **Semantics**: `syncSource.repoURL` (optional, default `drySource.repoURL`) makes hydration push to, and the app sync from, another repository; `hydrateTo` inherits it. Both repos must be in the project's `sourceRepos`; write credentials are looked up for the destination repo.
- **Edge cases**: Different URL spellings of the same repo are normalized for grouping keys.
- **Configuration surface**: `spec.sourceHydrator.syncSource.repoURL`.
- **Exposure**: CRD; UI create panel field; CLI: no flag (see doc differences).
- **Tests required**:
  - Positive: dry in repo A, hydrated in repo B; app syncs from B.
  - Negative: repo B not in sourceRepos -> InvalidSpecError / hydration error.

### APP-125 Hydrator write credentials
- **Upstream**: [hydrator.go#L469](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/hydrator/hydrator.go#L469), [hydrator.go#L485](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/hydrator/hydrator.go#L485), [repository.go#L99](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/repository.go#L99), [repository_secrets.go#L579](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/repository_secrets.go#L579), [common.go#L207](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L207), [source-hydrator.md#L53](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/source-hydrator.md#L53), [source-hydrator.md#L522](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/source-hydrator.md#L522)
- **Maturity**: Beta
- **Semantics**: Pushes use write credentials from Secrets labelled `argocd.argoproj.io/secret-type: repository-write` (exact repo) or `repo-write-creds` (URL-prefix templates), separate from pull credentials. Project scoping: if all apps in the group belong to one project, project-scoped write secrets may be used; otherwise only global ones. If the lookup returns no repository, the push proceeds without credentials (warning). Dry-source metadata is read with normal (pull) credentials.
- **Edge cases**: Lookup errors -> `failed to get hydrator credentials: ...`. Multiple matching secrets -> non-deterministic choice with warning `Found multiple credentials for repoURL: <url>`.
- **Configuration surface**: write Secrets (repositories area).
- **Exposure**: Secrets; UI repo settings (write credentials when hydrator enabled).
- **Tests required**:
  - Positive: push succeeds with only a `repository-write` secret.
  - Negative: apps from two projects with only project-scoped write secrets -> push fails.

### APP-126 Hydrated commit message template
- **Upstream**: [settings.go#L49](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L49), [settings.go#L519](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L519), [settings.go#L1090](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L1090), [template.go#L23](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/hydrator/template.go#L23), [hydrator.go#L685](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/hydrator/hydrator.go#L685), [source-hydrator.md#L433](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/source-hydrator.md#L433)
- **Maturity**: Beta
- **Semantics**: `argocd-cm` `sourceHydrator.commitMessageTemplate` (Go text/template + Sprig generic functions minus `env`, `expandenv`, `getHostByName`). Data: `{metadata: {repoURL, drySha, author, date, subject, body, references[]{commit{author, date, subject, body, sha, repoUrl}}}}` (JSON field names). Default template: `{{.metadata.drySha | trunc 7}}: {{ .metadata.subject }}`, optional blank line + body, then `Co-authored-by:` lines for each reference author and for the dry commit author.
- **Edge cases**: Template parse/execute errors fail the hydration (`failed to get hydrator commit templated message: ...`). An empty rendered message is passed to git as-is.
- **Configuration surface**: `argocd-cm` `sourceHydrator.commitMessageTemplate`.
- **Exposure**: Git commit messages.
- **Tests required**:
  - Positive: default message format for a dry commit with trailers.
  - Negative: invalid template -> app `Failed` with the error.

### APP-127 Hydration README template
- **Upstream**: [settings.go#L64](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L64), [settings.go#L854](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L854), [hydratorhelper.go#L128](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/commitserver/commit/hydratorhelper.go#L128), [hydratorhelper.go#L88](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/commitserver/commit/hydratorhelper.go#L88), [source-hydrator.md#L462](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/source-hydrator.md#L462)
- **Maturity**: Beta
- **Semantics**: `argocd-cm` `sourceHydrator.readmeMessageTemplate` (Go template with the same restricted Sprig set) renders each changed path's `README.md`. Data is the Go struct (fields `.RepoURL`, `.DrySHA`, `.Commands`, `.References`, ...). Default: `# Manifest Hydration` with a shell block `git clone <repo>`, `git checkout <drySHA>`, the recorded commands, and an optional `## References` list.
- **Edge cases**: Derived from code reading: the per-path metadata passed to the README contains only `RepoURL`, `DrySHA` and `Commands`, so `.References` is always empty and the references section never renders. Template errors fail the commit (`failed to write readme: ...`).
- **Configuration surface**: `argocd-cm` `sourceHydrator.readmeMessageTemplate`.
- **Exposure**: Git README files.
- **Tests required**:
  - Positive: default README contains clone/checkout lines and hydration commands.

### APP-128 Hydrated commit author
- **Upstream**: [settings.go#L523](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L523), [settings.go#L1102](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L1102), [commit.go#L253](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/commitserver/commit/commit.go#L253), [source-hydrator.md#L501](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/source-hydrator.md#L501)
- **Maturity**: Beta
- **Semantics**: `argocd-cm` `commit.author.name` (default `Argo CD`) and `commit.author.email` (default `argo-cd@example.com`), each defaulted independently; used for the hydrated commit, the orphan initial commit and notes.
- **Edge cases**: Hydrated commits are not signed.
- **Configuration surface**: `argocd-cm` keys above.
- **Exposure**: Git.
- **Tests required**:
  - Positive: only name configured -> email default.

### APP-129 Commit tracing trailers and hydrator.metadata references
- **Upstream**: [client.go#L1074](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/client.go#L1074), [client.go#L1096](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/client.go#L1096), [client.go#L1064](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/client.go#L1064), [repository.go#L2804](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L2804), [types.go#L1681](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1681), [hydrator.go#L31](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/hydrator/hydrator.go#L31), [source-hydrator.md#L332](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/source-hydrator.md#L332)
- **Maturity**: Beta
- **Semantics**: Dry commit body lines `Argocd-reference-commit-<key>: <value>` are parsed into a single reference: `repourl` (must parse as URL), `author` (RFC 5322 address; stored as the formatted address string), `date` (RFC3339), `subject`, `body` (JSON string literal, decoded), `sha` (lowercase hex 5-40 chars). Valid trailers are removed from the body; invalid or unknown ones stay in the body and are logged. Last occurrence wins. Result exposed as `references[0].commit{author, date, subject, body, sha, repoUrl}` in root `hydrator.metadata`, the commit-message template data, and `RevisionMetadata.references` returned by the revision metadata API. Subject = first paragraph of the message (split on the first blank line).
- **Edge cases**: All trailers optional; omitted keys are omitted in JSON.
- **Configuration surface**: dry commit trailers.
- **Exposure**: Git metadata; API `RevisionMetadata`; UI revision tooltips.
- **Tests required**:
  - Positive: all six trailers parsed; body trailer decoded from JSON.
  - Negative: `sha` uppercase -> trailer kept in body, not in references.

### APP-130 Hydration failure handling and retry cooldown
- **Upstream**: [hydrator.go#L322](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/hydrator/hydrator.go#L322), [hydrator.go#L650](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/hydrator/hydrator.go#L650), [hydrator.go#L243](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/hydrator/hydrator.go#L243), [source-hydrator.md#L554](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/source-hydrator.md#L554)
- **Maturity**: Beta
- **Semantics**: On failure `currentOperation` gets `phase: Failed`, `finishedAt: now`, `message: Failed to hydrate: <err>`; when the dry SHA was resolved it is recorded in `currentOperation.drySHA` and `lastComparedDryRevision`. Unattended retry happens after a fixed 2-minute cooldown; an explicit refresh/dry-source webhook retries immediately; a new dry commit retries immediately when `lastComparedDryRevision` is known; with no baseline the controller waits for the cooldown.
- **Edge cases**: The cooldown is hard-coded (2 minutes) in code, not derived from `timeout.reconciliation` (see doc differences).
- **Configuration surface**: none.
- **Exposure**: status; UI hydrate operation panel.
- **Tests required**:
  - Recovery: fix the dry repo, push a new commit -> immediate successful hydration.
  - Positive: without changes, retry occurs only after 2 minutes.

### APP-131 Stale Hydrating recovery
- **Upstream**: [hydrator.go#L148](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/hydrator/hydrator.go#L148), [hydrator.go#L301](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/hydrator/hydrator.go#L301), [appcontroller.go#L224](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L224)
- **Maturity**: Beta
- **Semantics**: If an app has been `Hydrating` for longer than the app resync period (`statusRefreshTimeout`, i.e. `timeout.reconciliation`), its hydration key is re-enqueued; apps already `Hydrating` keep their original `startedAt` when re-marked.
- **Edge cases**: Covers controller crashes mid-hydration.
- **Configuration surface**: `timeout.reconciliation`.
- **Exposure**: status.
- **Tests required**:
  - Recovery: kill controller during hydration -> after restart and timeout, hydration completes.

### APP-132 Post-hydration refresh and syncing from the hydrated branch
- **Upstream**: [hydrator.go#L260](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/hydrator/hydrator.go#L260), [hydrator_dependencies.go#L89](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/hydrator_dependencies.go#L89), [types.go#L246](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L246), [application.go#L540](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L540)
- **Maturity**: Beta
- **Semantics**: On success each app gets `currentOperation{phase: Hydrated, finishedAt, drySHA, hydratedSHA, sourceHydrator}`, `lastSuccessfulOperation{drySHA, hydratedSHA, sourceHydrator}`, `lastComparedDryRevision = drySHA`, then a normal refresh is requested by patching the refresh annotation (works across controller shards; no hydrate annotation). The app compares and syncs the sync source (`syncSource.repoURL|drySource.repoURL`, `syncSource.path`, `targetBranch`), so `status.sync.revision` is the hydrated commit. The API's GetManifests renders the dry source for hydrator apps.
- **Edge cases**: Sync/auto-sync behaviour is identical to a plain Git app on the hydrated branch.
- **Configuration surface**: none.
- **Exposure**: status; UI.
- **Tests required**:
  - Positive: after hydration the app refreshes and auto-syncs the new hydrated commit.

### APP-133 manifest-generate-paths with the source hydrator
- **Upstream**: [path.go#L118](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/app/path/path.go#L118), [state.go#L424](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L424), [source-hydrator.md#L575](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/source-hydrator.md#L575)
- **Maturity**: Beta
- **Semantics**: For the dry source, annotation paths are resolved relative to `drySource.path` and compared against `lastComparedDryRevision`; a dry commit not touching them does not trigger hydration. The sync source always uses `[syncSource.path]` regardless of the annotation.
- **Edge cases**: Requires non-shallow dry repo.
- **Configuration surface**: annotation.
- **Exposure**: controller behaviour.
- **Tests required**:
  - Positive: commit outside annotated dry paths -> no hydration.

### APP-134 Source integrity enforcement on the dry revision
- **Upstream**: [hydrator.go#L562](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/hydrator/hydrator.go#L562), [source-hydrator.md#L592](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/source-hydrator.md#L592)
- **Maturity**: Beta
- **Semantics**: If the project's effective source-integrity policy applies to the dry source, hydration is rejected unless the repo server performed and passed verification: `source integrity verification required but not performed for app "<ns/name>" dry revision "<rev>"` or `source integrity verification failed for app "<ns/name>" dry revision "<rev>": <err>`. Hydrated commits are not signed.
- **Edge cases**: Verifying the hydrated branch with signature policies will fail (unsigned commits).
- **Configuration surface**: AppProject `spec.sourceIntegrity` (project area).
- **Exposure**: status.
- **Tests required**:
  - Negative: unsigned dry commit with GPG policy -> Failed with exact message.

### APP-135 Commit server component
- **Upstream**: [server.go#L27](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/commitserver/server.go#L27), [commit.proto#L7](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/commitserver/commit/commit.proto#L7), [commit.proto#L57](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/commitserver/commit/commit.proto#L57), [commit.go#L63](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/commitserver/commit/commit.go#L63), [argocd_commit_server.go#L110](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-commit-server/commands/argocd_commit_server.go#L110), [common.go#L80](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L80), [metrics.go#L53](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/commitserver/metrics/metrics.go#L53)
- **Maturity**: Beta
- **Semantics**: Separate gRPC service `CommitService.CommitHydratedManifests(CommitHydratedManifestsRequest{repo, syncBranch, targetBranch, drySha, commitMessage, paths[]{path, manifests[]{manifestJSON}, commands[]}, dryCommitMetadata, authorName, authorEmail, readmeMessage}) -> {hydratedSha}`; also registers version and gRPC health services. Flags: `--address` (env `ARGOCD_COMMIT_SERVER_LISTEN_ADDRESS`, default `0.0.0.0`), `--port` (8086), `--metrics-address` (`ARGOCD_COMMIT_SERVER_METRICS_LISTEN_ADDRESS`), `--metrics-port` (8087), `--loglevel`, `--logformat`. Metrics: `argocd_commitserver_commit_pending_request_total`, `argocd_commitserver_git_request_total`, `argocd_commitserver_git_request_duration_seconds`, `argocd_commitserver_commit_request_duration_seconds`, `argocd_commitserver_userinfo_request_duration_seconds`, `argocd_commitserver_commit_request_total`. Each request works in a fresh temp dir under `/tmp/_commit-service`, removed afterwards.
- **Edge cases**: Errors are returned with git output logged.
- **Configuration surface**: flags/env above.
- **Exposure**: internal gRPC; metrics endpoint.
- **Tests required**:
  - Positive: health check serving; metrics increment on success and failure.

### APP-136 Hydration worker concurrency (hydration-processors)
- **Upstream**: [argocd_application_controller.go#L268](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-application-controller/commands/argocd_application_controller.go#L268), [appcontroller.go#L947](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L947), [appcontroller.go#L1022](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1022), [argocd-cmd-params-cm.yaml#L63](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cmd-params-cm.yaml#L63)
- **Maturity**: Beta
- **Semantics**: `--hydration-processors` (env `ARGOCD_APPLICATION_CONTROLLER_HYDRATION_PROCESSORS`, cmd-params `controller.hydration.processors`, default 5) parallel workers for distinct hydration keys; values < 1 are coerced to 1 with a warning. The per-app hydrate queue uses a single worker.
- **Edge cases**: Same key never processed concurrently.
- **Configuration surface**: flag/env/key above.
- **Exposure**: configuration.
- **Tests required**:
  - Positive: two keys hydrate concurrently with processors=2.

### APP-137 status.sourceHydrator
- **Upstream**: [types.go#L1247](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1247), [types.go#L1268](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1268), [types.go#L1286](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1286), [types.go#L1296](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L1296), [application-crd.yaml#L5244](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/crds/application-crd.yaml#L5244)
- **Maturity**: Beta
- **Semantics**: `status.sourceHydrator = {lastSuccessfulOperation{drySHA, hydratedSHA, sourceHydrator}, currentOperation{startedAt, finishedAt, phase (Hydrating|Failed|Hydrated, CRD enum), message, drySHA, hydratedSHA, sourceHydrator}, lastComparedDryRevision}`. Persisted by the controller with the same merge-patch mechanism as other status (and consuming the hydrate annotation).
- **Edge cases**: `currentOperation.hydratedSHA` absent on failure.
- **Configuration surface**: none.
- **Exposure**: CRD status; API; UI.
- **Tests required**:
  - Positive: phase transitions Hydrating -> Hydrated with SHAs populated.

### APP-138 Source hydrator CLI flags
- **Upstream**: [app.go#L112](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/util/app.go#L112), [app.go#L811](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/util/app.go#L811), [app.go#L179](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/util/app.go#L179), [argocd_app_create.md#L51](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/commands/argocd_app_create.md#L51)
- **Maturity**: Beta
- **Semantics**: `--dry-source-repo`, `--dry-source-revision`, `--dry-source-path`, `--sync-source-branch`, `--sync-source-path`, `--hydrate-to-branch` on `argocd app create|set|add-source` (shared app flags). If any of them is passed, the command edits `spec.sourceHydrator` (creating it when a non-empty value is given) and ignores normal source flags; `--hydrate-to-branch ""` removes `hydrateTo`.
- **Edge cases**: No flag exists for `syncSource.repoURL`.
- **Configuration surface**: CLI flags.
- **Exposure**: CLI.
- **Tests required**:
  - Positive: create a hydrator app purely from flags.
  - Positive: `argocd app set --hydrate-to-branch ""` clears hydrateTo.

### APP-139 Source hydrator UI
- **Upstream**: [application-create-panel.tsx#L401](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-create-panel/application-create-panel.tsx#L401), [hydrator-source-panel.tsx#L75](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-create-panel/hydrator-source-panel.tsx#L75), [application-status-panel.tsx#L246](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-status-panel/application-status-panel.tsx#L246), [application-hydrate-operation-state.tsx#L16](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-hydrate-operation-state/application-hydrate-operation-state.tsx#L16)
- **Maturity**: Beta
- **Semantics**: When `hydratorEnabled`, the create panel shows a source-hydrator checkbox that moves the current `source` into `drySource` (and back), with inputs for dry repo/revision/path, sync repo/branch/path and hydrateTo branch. The status panel shows a `SOURCE HYDRATOR` item (phase icon, phase, finished/started time, message, dry SHA link) that opens a hydrate operation panel (PHASE, MESSAGE, STARTED AT, DURATION, FINISHED AT when not Hydrating, DRY REVISION, ...).
- **Edge cases**: Hidden when the server reports `hydratorEnabled: false`.
- **Configuration surface**: none.
- **Exposure**: UI.
- **Tests required**:
  - Positive: create a hydrator app through the UI; status panel shows Hydrated.

### APP-140 argocd app CLI lifecycle command group
- **Upstream**: [app.go#L114](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/app.go#L114), [app.go#L1262](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/app.go#L1262), [app.go#L3046](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/app.go#L3046), [argocd_app.md#L1](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/commands/argocd_app.md#L1)
- **Maturity**: GA
- **Semantics**: Lifecycle subcommands in this area: `create` (APP-049), `get` (APP-053/APP-080), `list` (APP-053), `set` and `unset` (APP-051, source/sync-policy/destination/project/revision-history/hydrator flags), `edit` (spec in `$EDITOR`, UpdateSpec), `patch` (APP-052), `delete` (APP-056), `history` (APP-025), `confirm-deletion` (APP-062), `add-source` / `remove-source` (APP-018), `terminate-op` (APP-048). All accept `ns/name` and `-N/--app-namespace`. Other subcommands (`sync`, `wait`, `rollback`, `diff`, `manifests`, `resources`, `actions`, `logs`, `get-resource`, `patch-resource`, `delete-resource`) belong to other areas.
- **Edge cases**: The documented `--env` flag on create/set/add-source is declared but never read (no effect).
- **Configuration surface**: CLI.
- **Exposure**: CLI.
- **Tests required**:
  - Positive: each listed subcommand round-trip against Patrol's API with qualified names.

### APP-141 Application lifecycle Kubernetes events
- **Upstream**: [audit_logger.go#L40](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/audit_logger.go#L40), [application.go#L393](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L393), [application.go#L1222](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1222), [appcontroller.go#L1798](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1798), [appcontroller.go#L2557](https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2557)
- **Maturity**: GA
- **Semantics**: Events recorded on the Application with reasons: `ResourceCreated` (API create), `ResourceUpdated` (API update/terminate, sync/health status transitions), `ResourceDeleted` (API delete), `OperationStarted` (`Initiated automated sync to '<revs>'`), `OperationCompleted` (sync result), `StatusRefreshed` (warning `Unable to delete application resources: <err>`), `ResourceActionRan` (actions area). Event labels can be enriched from app labels (settings area).
- **Edge cases**: Messages listed in APP-028, APP-046, APP-087.
- **Configuration surface**: none here.
- **Exposure**: Kubernetes events; UI app events tab.
- **Tests required**:
  - Positive: create/update/delete via API emit the three reasons with expected messages.

## Unconfirmed

- APP-007: no UI input for `tagPrefix` was found in the create panel; checked `ui/src/app/applications/components/application-create-panel/application-create-panel.tsx` and `ui/src/app/applications/components/utils.tsx` (only display usages at `utils.tsx#L962` and `application-status-panel.tsx#L84`).
- APP-037: whether the in-memory clearing of `DeletionError` (`appcontroller.go#L1154`) is ever persisted; no explicit patch follows it in `controller/appcontroller.go`; it may persist only when a later status patch happens or the app disappears.
- APP-079: `docs/user-guide/annotations-and-labels.md#L13` and `application_annotations.go#L5` say a hard refresh invalidates the "target cluster state cache"; no code path invalidating the live cluster cache on hard refresh was found (checked `controller/appcontroller.go`, `controller/state.go`, `server/application/application.go`, `util/argo/argo.go` for `RefreshTypeHard`). Only manifest cache bypass (`noCache`) and app-details cache bypass were confirmed.
- APP-088: the conclusion that an Application being deleted with `skip-reconcile: true` keeps its finalizers is derived from event filtering in `canProcessApp` (`controller/appcontroller.go#L2643`) and was not verified by a test.
- APP-113: behaviour of hydrator apps when the hydrator is disabled is derived from `GetSource()` (`types.go#L246`) and controller wiring (`appcontroller.go#L224`); no upstream test asserting it was located.
- APP-127: the claim that `.References` is always empty in README rendering is derived from `commitserver/commit/hydratorhelper.go#L88-L99`; not confirmed by an upstream test (checked `commitserver/commit/hydratorhelper_test.go` names only).
- APP-066: whether PreDelete hook objects are removed by the cascade step rather than the cleanup stage is derived from `gitops-engine/pkg/cache/cluster.go#L1533` (managed = tracked and no ownerRefs) and `controller/hook.go#L170`; not confirmed by e2e test.
- Exact UI labels of the hydrate operation panel beyond PHASE/MESSAGE/STARTED AT/DURATION/FINISHED AT/DRY REVISION (checked `application-hydrate-operation-state.tsx` lines 16-45 only).
- Event label enrichment from Application labels (`util/argo/argo.go` `GetAppEventLabels`, settings key) was not read in detail.

## Doc/implementation differences

- `docs/user-guide/source-hydrator.md#L545` says hydration state is tracked with a git note "in a dedicated `source-hydrator` namespace"; the code uses notes namespace `hydrator.metadata` (`commitserver/commit/commit.go#L21`, ref `refs/notes/hydrator.metadata`). The comment in `util/git/client.go#L1644` also mentions `refs/notes/source-hydrator`. Baseline: `hydrator.metadata`.
- `docs/user-guide/source-hydrator.md#L157` says "The repository root is never written to"; the commit server always writes root-level `hydrator.metadata` and `.gitattributes` (`commitserver/commit/hydratorhelper.go#L45`, `#L51`). Only manifests are kept out of the root.
- `docs/user-guide/source-hydrator.md#L560` says the failed-hydration cooldown "matches `ARGOCD_RECONCILIATION_TIMEOUT`"; the code hard-codes 2 minutes (`controller/hydrator/hydrator.go#L652`), independent of `timeout.reconciliation`.
- `docs/user-guide/source-hydrator.md#L396-L421` shows `references[].commit.author` as an object `{name, email}` and key `repoURL`; the code stores `author` as a formatted RFC 5322 string (`reposerver/repository/repository.go#L2812`) and the reference JSON key is `repoUrl` (`pkg/apis/application/v1alpha1/types.go#L1703`).
- `docs/user-guide/source-hydrator.md#L462-L499` README template example renders `.References`; per-path README data never includes references (`commitserver/commit/hydratorhelper.go#L88`), so that section is always empty (derived).
- `docs/user-guide/source-hydrator.md#L3-L7` labels the feature "Beta (Since v3.5.0)" while `docs/operator-manual/feature-maturity.md#L27` says introduced in v2.14.0; and `source-hydrator.md#L594` marks hydrator signature verification "Alpha" though feature-maturity lists only the hydrator as Beta (classified Beta here).
- `docs/user-guide/sync-waves.md#L67` says PreDelete hooks are "cleaned up and the Application resources are deleted" after success, and `#L77` suggests manually deleting a failing hook "to allow deletion to proceed"; the code deletes Degraded hooks itself and recreates missing hooks from Git on the next pass (`controller/hook.go#L240-L249`, `#L168-L188`), so deleting a failing hook does not bypass it; the stage-less pre-delete finalizer is removed before cascade deletion and hook cleanup happens in a later `/cleanup` stage (`controller/appcontroller.go#L1421`, `#L1506`).
- `docs/user-guide/app_deletion.md#L114-L118` says with Background policy "The Application is removed immediately"; the controller still waits until the app's managed top-level resources are gone before removing the finalizer (`controller/appcontroller.go#L1470-L1486`).
- `docs/operator-manual/app-any-namespace.md#L193-L197` says the Application "named `foo` in the namespace `bar`" is addressed as `foo/bar`; the CLI parses `foo/bar` as namespace `foo`, name `bar` (`util/argo/argo.go#L1230`), consistent with the UI section at `#L218`.
- `docs/operator-manual/app-any-namespace.md#L24` refers to "the default `label` setting" for tracking; the code default is `annotation` (`util/settings/settings.go#L909-L911`).
- `docs/user-guide/resource_tracking.md#L61` lists "Labels are truncated to 63 characters" as a limitation of label tracking; under `label` tracking the value is not truncated (`util/argo/resource_tracking.go#L171-L179`); truncation applies only to `annotation+label`.
- `docs/user-guide/annotations-and-labels.md` omits keys defined and used in code: `argocd.argoproj.io/hydrate`, `argocd.argoproj.io/installation-id`, `argocd.argoproj.io/deletion-approved`, `argocd.argoproj.io/ignore-healthcheck`, `argocd.argoproj.io/client-side-apply-migration-manager`, `argocd.argoproj.io/hook-finalizer`, finalizers `pre-delete-finalizer.argocd.argoproj.io[/cleanup]` and `post-delete-finalizer.argocd.argoproj.io[/cleanup]`, the default tracking label `app.kubernetes.io/instance`, and secret-type values `repository-write` / `repo-write-creds` (`common/common.go#L188-L249`, `pkg/apis/application/v1alpha1/application_annotations.go#L8`, `application_defaults.go#L13-L16`).
- `docs/operator-manual/managed-by-url.md#L105` says invalid URLs "will prevent the Application from being created or updated"; this is enforced only by the API server (`server/application/application.go#L1391`); declaratively applied Applications are not validated.
- `docs/user-guide/commands/argocd_app_create.md#L53` documents `--env` ("Application environment to monitor"); the flag is declared (`cmd/util/app.go#L109`) but its value is never used.
