# Projects, RBAC & Identity (tenancy) — parity inventory (Argo CD v3.5.5)

Scope: the multi-tenancy and identity layer of Argo CD at commit `a5038b0e2bbac1715754380ae0084df8e3d5b48c`.
In scope: the `AppProject` CRD and every spec/status field (`sourceRepos` incl. `!` negation, `sourceNamespaces`,
`destinations` by server/name/namespace incl. negation, `destinationServiceAccounts`, cluster/namespace resource
allow/deny lists, `orphanedResources`, `roles` with policies/groups/JWT tokens, `syncWindows`, `signatureKeys`,
`sourceIntegrity`, `permitOnlyProjectScopedClusters`, `description`, `status.jwtTokensByRole`); the `default` project;
global projects (`globalProjects` in `argocd-cm`); project-scoped repositories and clusters; project role tokens;
enforcement points in the API server and application controller (including privilege-escalation protections on
project update); the `ProjectService` API, `argocd proj` / `argocd admin proj` CLI and the project settings UI; the
RBAC engine (Casbin model, `policy.csv` grammar, resources/actions/objects, deny precedence, `policy.default`, `scopes`,
`policy.matchMode`, built-in roles, `policy.<name>.csv` overlays, fine-grained `update/*`/`delete/*` and
`server.rbac.disableApplicationFineGrainedRBACInheritance`, `argocd admin settings rbac can|validate`,
`AccountService.CanI`); local accounts (`accounts.<name>`, `admin.*`, password hashing/pattern/change, initial admin
secret, API keys, login rate limiting, session duration and renewal, anonymous access, token revocation); SSO (bundled
Dex config generation and proxy, `oidc.config` options, web and CLI login flows incl. PKCE, token verification and
refresh, group claim mapping, UserInfo/Azure group resolution, logout); session/user-info endpoints; Kubernetes-event
audit logging.
Out of scope (covered by other areas): sync engine internals (only the sync-window gate and per-resource permission
checks are described here), repository/cluster credential storage formats (only the `project` scoping is described),
GPG key management and the repo-server signature verification implementation (only the project-level policy is
described), ApplicationSet RBAC details beyond the shared `applicationsets` resource semantics, notifications,
proxy-extension routing (only the `extensions, invoke` RBAC is listed), the web terminal internals (only
`exec, create` RBAC is listed).

Permalink base used below: `https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/`.

---

## A. AppProject resource model

### TEN-001 AppProject custom resource
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/app_project_types.go#L36-L57, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2834-L2869, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/crds/appproject-crd.yaml#L1-L430, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/project.yaml#L1-L1, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/projects.md#L1-L8
- **Maturity**: GA
- **Semantics**: Namespaced CRD `appprojects.argoproj.io` (group `argoproj.io`, version `v1alpha1`, kind `AppProject`, plural `appprojects`, short names `appproj`, `appprojs`). `metadata` and `spec` are required. No status subresource (`+genclient:noStatus`), so `status` is written together with the object. Spec fields: `sourceRepos`, `destinations`, `description`, `roles`, `clusterResourceWhitelist`, `namespaceResourceBlacklist`, `orphanedResources`, `syncWindows`, `namespaceResourceWhitelist`, `signatureKeys` (deprecated), `clusterResourceBlacklist`, `sourceNamespaces`, `permitOnlyProjectScopedClusters`, `destinationServiceAccounts`, `sourceIntegrity`. Projects are only read from the Argo CD control-plane namespace (all listers/clients use the server/controller namespace). Schema-required sub-fields: `group`+`kind` on resource list items, `server`+`defaultServiceAccount` on destination service accounts, `name` on roles, `iat` on JWT tokens, `keyID` on signature keys, `git` / `policies` / `repos`+`gpg` / `url` / `mode`+`keys` inside `sourceIntegrity`.
- **Edge cases**: AppProjects created in a namespace other than the control-plane namespace are never consulted. The CRD performs only structural validation; semantic validation (TEN-009, TEN-017..TEN-021, TEN-029) happens only in the API server, so objects applied with `kubectl` can contain values the API would reject and the controller will still use them.
- **Configuration surface**: CRD `appprojects.argoproj.io`; all `spec.*`, `status.jwtTokensByRole`.
- **Exposure**: CRD; API (`ProjectService`, TEN-044..TEN-051); CLI (`argocd proj`, TEN-053..TEN-062); UI (Settings → Projects, TEN-064).
- **Tests required**:
  - Round-trip every spec field and `status.jwtTokensByRole` through the CRD schema without loss.
  - `kubectl get appproj` / `appprojs` resolve.
  - Objects missing required nested fields (e.g. clusterResourceWhitelist item without `kind`) are rejected by the Kubernetes API.
  - AppProject in a non-control-plane namespace is ignored by API and controller.

### TEN-002 Default project bootstrap and protection
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L291-L318, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/application_defaults.go#L24-L25, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L3827-L3833, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L1016-L1020, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/project/project.go#L465-L468, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/projects.md#L10-L44
- **Maturity**: GA
- **Semantics**: On every API-server start, if AppProject `default` does not exist in the control-plane namespace it is created with `sourceRepos: ['*']`, `destinations: [{server: '*', namespace: '*'}]`, `clusterResourceWhitelist: [{group: '*', kind: '*'}]` (AlreadyExists is tolerated; an existing `default` is never overwritten). An Application with empty `spec.project` is treated as project `default` everywhere (`GetProject()`), and the controller/API normalizes the stored spec to `project: default`. `ProjectService.Delete` refuses `default`.
- **Edge cases**: Deleting `default` via the API returns InvalidArgument `name 'default' is reserved and cannot be deleted`; deleting it with `kubectl` succeeds and it is re-created at next API-server restart. A restrictive `default` (e.g. empty sources/destinations) makes all apps using it fail permission checks (TEN-040).
- **Configuration surface**: AppProject named `default`.
- **Exposure**: CRD; API (`DELETE /api/v1/projects/default` rejected); UI (default project listed).
- **Tests required**:
  - Fresh install creates `default` with exactly the permissive spec.
  - Restart with a modified `default` does not overwrite it.
  - API delete of `default` fails with the exact message; kubectl delete followed by restart re-creates it.
  - Application created without `spec.project` is persisted/normalized to `default`.

### TEN-003 spec.description
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2839-L2841, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/crds/appproject-crd.yaml#L93-L96, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/util/project.go#L38-L38
- **Maturity**: GA
- **Semantics**: Free-text description, max 255 characters enforced by the CRD schema (`maxLength: 255`). Informational only.
- **Edge cases**: 256-character description rejected by the Kubernetes API (and therefore by `ProjectService.Create/Update`, which surface the Kubernetes error).
- **Configuration surface**: `spec.description`; CLI `--description`.
- **Exposure**: CRD; API; CLI (`proj create|set --description`); UI (Summary → GENERAL → DESCRIPTION).
- **Tests required**:
  - 255 chars accepted, 256 rejected.
  - CLI `proj set --description` updates only the description.

### TEN-004 spec.sourceRepos allow patterns
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/app_project_types.go#L445-L480, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L642-L692, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L1060-L1078, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/projects.md#L54-L88
- **Maturity**: GA
- **Semantics**: An application source is permitted iff at least one non-negated entry matches and no negated entry rejects it (TEN-005). Both the pattern and the app `repoURL` are passed through `git.NormalizeGitURL` before matching; matching uses gobwas glob with `/` as separator (so `*` does not cross `/`, `**` does), except the literal pattern `*` which matches everything. Checked for: the single `source`, every entry of `sources` (multi-source), and for `sourceHydrator` both `drySource` and `syncSource` (sync source only checked when its repoURL differs). Also used to filter which repositories/repo-credentials are offered to a project (`GetPermittedRepos`, `GetPermittedReposCredentials`). Empty `sourceRepos` permits nothing. Project-scoped repositories are appended as additional allowed URLs (TEN-038); global-project sourceRepos are appended (TEN-037).
- **Edge cases**: Failure condition message: `application repo <url> is not permitted in project '<proj>'` (type `InvalidSpecError`); for hydrator sync source: `sync source repo <url> is not permitted in project '<proj>'`. Duplicate entries rejected by API: `source repository '<x>' already added`. A glob that fails to compile never matches (logged warning). URL normalization makes `https://github.com/Org/Repo.git` and `https://github.com/org/repo` comparable.
- **Configuration surface**: `spec.sourceRepos[]`; CLI `--src`, `proj add-source/remove-source`.
- **Exposure**: CRD; API (project Create/Update; app Create/Update validation); CLI; UI (SOURCE REPOSITORIES).
- **Tests required**:
  - `*` permits any repo; exact URL permits only that repo (with/without `.git`, case-insensitive host).
  - `https://github.com/org/*` permits `https://github.com/org/a` but not `https://github.com/org/a/b`; `https://github.com/org/**` permits both.
  - Multi-source app with one disallowed source yields one condition per disallowed source.
  - Hydrator app: disallowed dry source vs disallowed sync source produce distinct messages.
  - Empty list rejects all; duplicate entry rejected by API.

### TEN-005 spec.sourceRepos negation ('!') patterns
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/app_project_types.go#L458-L480, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/app_project_types.go#L223-L233, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/app_project_types.go#L529-L532, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/projects.md#L63-L88
- **Maturity**: GA
- **Semantics**: Entries prefixed with `!` are deny patterns: the URL after `!` is normalized, then the source is rejected as soon as it matches a deny pattern (evaluation returns false immediately, regardless of order). Deny patterns alone never permit anything: at least one allow pattern must match. `!*` is rejected by the API.
- **Edge cases**: API error `source repository has an invalid format, '!*'` (InvalidArgument). A project with only negated entries permits nothing. Deny wins over a more specific allow listed earlier or later.
- **Configuration surface**: `spec.sourceRepos[]` entries beginning with `!`.
- **Exposure**: CRD; API; CLI (`proj add-source PROJECT '!<url>'`); UI.
- **Tests required**:
  - `['!https://gitlab.com/group/**', '*']` rejects `https://gitlab.com/group/x/y` and permits others.
  - Order independence (deny listed before/after allow).
  - `!*` rejected with exact message.
  - Only-deny project permits nothing.

### TEN-006 spec.destinations server/namespace patterns
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/app_project_types.go#L482-L527, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L694-L718, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/projects.md#L90-L125
- **Maturity**: GA
- **Semantics**: The application destination is first resolved to a registered Cluster (by server URL or by name; resolution errors become `InvalidSpecError` conditions). Permission is evaluated against the resolved cluster's `server` and `name` plus the destination namespace. For each destination entry: server matches if the cluster server is non-empty and glob-matches `item.server`; name matches likewise; namespace glob-matches `item.namespace` (no separators; `*` matches anything, including empty). An entry matches if (server or name matched) and namespace matched. Permitted iff any entry matches and no deny entry rejects (TEN-008). Nil cluster → not permitted. Also used for every namespaced resource the app syncs or tracks (TEN-041).
- **Edge cases**: Failure message: `application destination server '<server-or-name>' and namespace '<ns>' do not match any of the allowed destinations in project '<proj>'` (uses cluster name when the app destination is by name). Destination with both `name` and `server` → `application destination can't have both name and server defined: <name> <server>`. Unknown name → `there are no clusters with this name: <name>`; duplicated names → `there are N clusters with the same name: [...]`. Empty app namespace matches only `*` or empty patterns.
- **Configuration surface**: `spec.destinations[].server|name|namespace`; CLI `--dest server,namespace`, `proj add-destination`.
- **Exposure**: CRD; API; CLI; UI (DESTINATIONS).
- **Tests required**:
  - `{server: '*', namespace: 'team-*'}` permits `team-a`, rejects `prod`.
  - Destination given by name resolves to cluster and matches a server-based project entry.
  - Unknown cluster name and ambiguous names produce the exact conditions.
  - Resource-level: a manifest targeting a non-permitted namespace in a permitted cluster is refused at sync (TEN-041).

### TEN-007 spec.destinations name-based matching
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/app_project_types.go#L507-L527, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/project.go#L277-L328, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/test/e2e/project_management_test.go#L186-L210
- **Maturity**: GA
- **Semantics**: A destination entry may specify `name` instead of `server`; it then matches by the resolved cluster's name (glob). Matching is independent of how the Application addressed the cluster (an app using `server:` still matches a project entry using `name:` if the cluster's name matches, and vice versa).
- **Edge cases**: The project-update privilege check only considers `server` values (TEN-046), so name-only entries contribute an empty string to that check. Renaming a cluster can silently change project permissions.
- **Configuration surface**: `spec.destinations[].name`; CLI `proj add-destination PROJECT NAME NAMESPACE --name`.
- **Exposure**: CRD; API; CLI; UI.
- **Tests required**:
  - Project entry `{name: in-cluster, namespace: '*'}` permits app with `server: https://kubernetes.default.svc`.
  - Name glob `prod-*` matches clusters named `prod-eu`.
  - Cluster rename removes permission on next reconcile.

### TEN-008 spec.destinations negation semantics
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/app_project_types.go#L507-L527, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/app_project_types.go#L445-L455, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/projects.md#L97-L125
- **Maturity**: GA
- **Semantics**: `!pattern` in `server`, `name` or `namespace` matches when the value does NOT match `pattern`. Evaluation per entry (first rejection wins, returns false immediately): (a) entry fully matches → mark matched; (b) reject if (name not matched and `item.name` is a deny pattern) OR (server not matched and `item.server` is a deny pattern AND namespace matched); (c) reject if namespace not matched and `item.namespace` is a deny pattern and server matched. Permitted iff some entry matched and no entry rejected.
- **Edge cases**: Operator precedence in (b) is `A || (B && C)`: a negated `name` rejects regardless of namespace. `{namespace: '!kube-system', server: '*'}` rejects `kube-system` on all clusters; `{namespace: '*', server: '!https://team1-*'}` rejects team1 clusters. Negation must be combined with an allow entry to permit anything.
- **Configuration surface**: `spec.destinations[]` with `!`-prefixed values.
- **Exposure**: CRD; API; CLI (`proj add-destination PROJECT '!<server>' '!<ns>'`); UI.
- **Tests required**:
  - Doc example (deny kube-system, deny team1 servers, allow all) evaluated against a matrix of server/namespace pairs.
  - Negated name entry rejects even when another entry would allow.
  - Negation-only project permits nothing.

### TEN-009 Project spec validation (API server)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/app_project_types.go#L165-L323, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/project/project.go#L72-L82
- **Maturity**: GA
- **Semantics**: `ValidateProject` + `rbac.ValidatePolicy(ProjectPoliciesString())` run on Create, Update, CreateToken and DeleteToken. Rules (first failure returned): destinations reject `!*` in name/server/namespace and duplicate keys (`server/namespace`, or `name/namespace` when server empty); `sourceNamespaces` unique; `sourceRepos` reject `!*` and duplicates; roles (TEN-017..TEN-019); sync windows (TEN-021); destination service accounts (TEN-029). Casbin syntax errors → `policy syntax error: ...` (InvalidArgument).
- **Edge cases**: Messages: `name has an invalid format, '!*'`, `server has an invalid format, '!*'`, `namespace has an invalid format, '!*'`, `destination '<key>' already added`, `source namespace '<ns>' already added`, `source repository has an invalid format, '!*'`, `source repository '<x>' already added`. Because token create/delete re-validate the stored project, a project made invalid via kubectl blocks token operations (`error validating project: ...`).
- **Configuration surface**: n/a (API behaviour).
- **Exposure**: API (Create/Update/CreateToken/DeleteToken); CLI and UI surface errors.
- **Tests required**:
  - Each rule above rejected with the exact message and gRPC code.
  - Valid complex project accepted.
  - kubectl-applied invalid project → token create fails with validation error.

### TEN-010 spec.sourceNamespaces (applications in any namespace)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/app_project_types.go#L618-L631, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/glob/list.go#L15-L33, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L792-L803, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L430-L432, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/app-any-namespace.md#L34-L48, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/app-any-namespace.md#L117-L125
- **Maturity**: GA
- **Semantics**: An Application may use a project iff its namespace is empty, equals the control-plane namespace, or matches an entry of `sourceNamespaces`. Entries wrapped in `/.../` are Go regular expressions; others are globs. The namespace must additionally be enabled globally via `--application-namespaces` on server and controller (other area). Not merged from global projects.
- **Edge cases**: Controller error/condition `application '<app>' in namespace '<ns>' is not permitted to use project '<proj>'`. Apps in the control-plane namespace may reference any project regardless of `sourceNamespaces`. CLI `proj remove-source-namespace` refuses to remove `*` (prints `Source namespace '*' does not exist in project or cannot be removed`). Duplicate entries rejected by API.
- **Configuration surface**: `spec.sourceNamespaces[]`; CLI `--source-namespaces`, `proj add-source-namespace|remove-source-namespace`.
- **Exposure**: CRD; API; CLI; UI (SOURCE NAMESPACES).
- **Tests required**:
  - Glob `team-*` and regex `/^((?!not-allowed).)*$/` behave as documented.
  - App in control-plane namespace may use a project with empty `sourceNamespaces`.
  - App in non-listed namespace gets the not-permitted condition and is not reconciled.

### TEN-011 spec.clusterResourceWhitelist
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/app_project_types.go#L398-L418, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2912-L2919, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L3856-L3876, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/projects.md#L127-L162
- **Maturity**: GA
- **Semantics**: Cluster-scoped resources are denied unless the whitelist is non-empty and contains a matching item. Each item has `group`, `kind` (both `filepath.Match` patterns, required) and optional `name` (`filepath.Match`; empty = all names). A cluster-scoped resource is permitted iff whitelisted AND not blacklisted (TEN-012).
- **Edge cases**: Empty/nil whitelist → no cluster-scoped resource permitted. Core group is `''`. Malformed patterns never match. Name matching is applied to the object name (e.g. Namespace `team1-*`).
- **Configuration surface**: `spec.clusterResourceWhitelist[].group|kind|name`; CLI `--allow-cluster-resource [group/]kind[/name]`, `proj allow-cluster-resource PROJECT GROUP KIND [NAME]`.
- **Exposure**: CRD; API; CLI; UI (cluster resource allow list).
- **Tests required**:
  - Default project (`*/*`) permits ClusterRole; empty list denies it.
  - `{group: '', kind: Namespace, name: team1-*}` permits `team1-a`, denies `kube-system`.
  - Kind glob `Cluster*` matches ClusterRole and ClusterRoleBinding.

### TEN-012 spec.clusterResourceBlacklist
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/app_project_types.go#L412-L418, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2858-L2859, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/projects.md#L152-L162
- **Maturity**: GA
- **Semantics**: Same item shape as the whitelist; a match denies the resource even if whitelisted.
- **Edge cases**: Blacklist without whitelist is meaningless for cluster-scoped kinds (still denied). `{kind: Namespace, name: kube-*}` denies only those names.
- **Configuration surface**: `spec.clusterResourceBlacklist[]`; CLI `--deny-cluster-resource`, `proj deny-cluster-resource`.
- **Exposure**: CRD; API; CLI; UI (cluster resource deny list).
- **Tests required**:
  - Whitelist `*/*` + blacklist `rbac.authorization.k8s.io/ClusterRole` denies ClusterRole only.
  - Name-scoped deny leaves other names permitted.

### TEN-013 spec.namespaceResourceWhitelist
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/app_project_types.go#L403-L410, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L3843-L3854
- **Maturity**: GA
- **Semantics**: For namespaced resources: if the whitelist is nil, all group/kinds are whitelisted; otherwise the resource must match an item (`group`, `kind` via `filepath.Match`; no `name` field). Then the blacklist is applied (TEN-014). Namespaced resources must also satisfy the destination check (TEN-006).
- **Edge cases**: A non-nil empty list (e.g. stored literally as `[]`) whitelists nothing, while an omitted field whitelists everything; round-trips that drop empty lists (`omitempty`) change semantics. No per-name filtering for namespaced kinds.
- **Configuration surface**: `spec.namespaceResourceWhitelist[]`; CLI `--allow-namespaced-resource [group/]kind`, `proj allow-namespace-resource ... -l allow`.
- **Exposure**: CRD; API; CLI; UI (namespace resource allow list).
- **Tests required**:
  - Omitted whitelist permits Deployment; whitelist `[apps/Deployment]` denies ConfigMap.
  - Stored `[]` vs omitted produce different outcomes.

### TEN-014 spec.namespaceResourceBlacklist
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/app_project_types.go#L403-L410, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/projects.md#L28-L44
- **Maturity**: GA
- **Semantics**: A namespaced resource matching any blacklist item is denied. `{group: '*', kind: '*'}` denies all namespaced kinds.
- **Edge cases**: Denied namespaced kinds are also excluded from orphaned-resource detection (TEN-016) and from tree/live-state (TEN-041).
- **Configuration surface**: `spec.namespaceResourceBlacklist[]`; CLI `--deny-namespaced-resource`, `proj deny-namespace-resource`.
- **Exposure**: CRD; API; CLI; UI (namespace resource deny list).
- **Tests required**:
  - Blacklist `/ResourceQuota` denies ResourceQuota sync with message `resource :ResourceQuota is not permitted in project <p>`.
  - Wildcard deny blocks all namespaced kinds.

### TEN-015 spec.orphanedResources monitoring and warning
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2805-L2823, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L584-L693, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/util/project.go#L144-L154, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/orphaned-resources.md#L1-L37
- **Maturity**: GA
- **Semantics**: Monitoring is enabled when `spec.orphanedResources` is non-nil. The controller lists top-level resources in each app's destination namespace, removes those managed by the app, keeps namespaced ones that are permitted by the project lists and not excluded (TEN-016), walks their hierarchy skipping resources owned by another existing Application and those not permitted by the project, and exposes them as orphaned nodes in the resource tree; the `argocd_app_orphaned_resources_count` metric is set. If `warn: true` and at least one orphan exists, condition `OrphanedResourceWarning` with message `Application has N orphaned resources` is set; otherwise that condition is cleared. `warn` defaults to false when omitted.
- **Edge cases**: `orphanedResources: {}` enables detection without warnings. Project without the field: no orphan detection, no warning. CLI: `--orphaned-resources` enables, `--orphaned-resources-warn` sets `warn` only when the flag is explicitly given.
- **Configuration surface**: `spec.orphanedResources.warn`; CLI `--orphaned-resources`, `--orphaned-resources-warn`.
- **Exposure**: CRD; API; CLI; UI (RESOURCE MONITORING section; app tree "Show Orphaned").
- **Tests required**:
  - Unmanaged ConfigMap in destination namespace appears as orphan; warning only when `warn: true`.
  - Resource owned by another Application is not reported.
  - Removing the field clears orphans and condition.

### TEN-016 spec.orphanedResources.ignore and built-in exclusions
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L543-L565, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2813-L2818, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/orphaned-resources.md#L39-L68
- **Maturity**: GA
- **Semantics**: Never orphaned: Service `kubernetes` in namespace `default`; any ServiceAccount named `default`; any ConfigMap named `kube-root-ca.crt`; namespaced kinds denied by the project. `ignore[]` items `{group, kind, name}`: empty `kind` matches all kinds, otherwise glob; `group` glob (empty matches only the core group); empty `name` matches all, otherwise glob.
- **Edge cases**: Omitting `group` in an ignore item only matches core-group resources. Ignore matches only top-level resources (children of an ignored top-level resource are not traversed).
- **Configuration surface**: `spec.orphanedResources.ignore[].group|kind|name`; CLI `proj add-orphaned-ignore PROJECT GROUP KIND [--name]`, `proj remove-orphaned-ignore`.
- **Exposure**: CRD; API; CLI; UI (orphaned ignore list editor).
- **Tests required**:
  - Each built-in exclusion is never reported.
  - `{kind: Secret, name: '*.example.com'}` suppresses matching secrets only.
  - Group omitted vs `*` difference for an apps/Deployment orphan.

### TEN-017 spec.roles definition and validation
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L3498-L3510, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2770-L2777, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/app_project_types.go#L235-L264, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/projects.md#L172-L215
- **Maturity**: GA
- **Semantics**: Role fields: `name` (required), `description`, `policies[]`, `jwtTokens[]` (legacy token storage, TEN-034), `groups[]`. Names must match `^[a-zA-Z0-9]([-_a-zA-Z0-9]*[a-zA-Z0-9])?$` and be unique per project. Duplicate policies or groups within a role are rejected.
- **Edge cases**: Messages: `role '<r>' already exists` (AlreadyExists), `invalid role name '<r>'. Must consist of alphanumeric characters, '-' or '_', and must start and end with an alphanumeric character`, `policy '<p>' already exists for role '<r>'`, `group '<g>' already exists for role '<r>'` (AlreadyExists). Roles are not merged from global projects.
- **Configuration surface**: `spec.roles[]`.
- **Exposure**: CRD; API; CLI (`proj role create|delete|list|get`); UI (Roles tab).
- **Tests required**:
  - Valid/invalid role names (`a`, `a-b_c`, `-a`, `a-`, `a.b`).
  - Duplicate role/policy/group rejected with codes.

### TEN-018 Project role policy grammar and validation
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2702-L2768, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/rbac/rbac.go#L112-L119, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/app_project_types.go#L360-L382, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/projects.md#L195-L205
- **Maturity**: GA
- **Semantics**: Each policy must be `p, <sub>, <res>, <act>, <obj>, <eft>` (exactly 6 comma-separated tokens, first `p`). `sub` must equal `proj:<project>:<role>`. `res` ∈ {applications, applicationsets, logs, exec, clusters, repositories}. `act` ∈ {get, create, update, delete, sync, override, *} or matches regex `action/.*`, `update/.*`, `delete/.*` (unanchored). `obj` must match `^<project>(/[*\w-.]+)?/[*\w-.]+$` (project-relative, optional namespace segment). `eft` ∈ {allow, deny}. Before validation, Create/Update normalize each policy to `token, token, ...` (single space after commas).
- **Edge cases**: Messages: `invalid policy rule '<p>': must be of the form: 'p, sub, res, act, obj, eft'`; `... policy subject must be: 'proj:<p>:<r>', not '<x>'`; `... project resource must be: 'applications', 'applicationsets', 'repositories', 'exec', 'logs' or 'clusters', not '<x>'`; `... invalid action '<x>'`; `... object must be of form '<p>/*', '<p>[/<NAMESPACE>]/<APPNAME>' or '<p>/<APPNAME>', not '<x>'`; `... effect must be: 'allow' or 'deny'`. Objects containing `:` or extra `/` (e.g. a repo URL) are rejected, so project-role policies on repositories/clusters must use wildcard-style objects. Cross-project objects (`other/*`) are rejected. `projects` and `accounts` resources cannot be granted from a project role (except the implicit `projects get`, TEN-020).
- **Configuration surface**: `spec.roles[].policies[]`; CLI `proj role add-policy|remove-policy -a -p -o -r`.
- **Exposure**: CRD; API; CLI; UI (role policy editor).
- **Tests required**:
  - Each validation branch with exact message.
  - `p, proj:p:r, applications, action/apps/Deployment/restart, p/*, allow` accepted.
  - Object `p/ns/app` accepted; `p/https://x` rejected; `q/*` rejected.
  - Policies with irregular spacing are stored normalized.

### TEN-019 Project role groups (SSO bindings)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2779-L2803, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/app_project_types.go#L325-L358, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/rbac.md#L362-L378
- **Maturity**: GA
- **Semantics**: Each entry in `groups` becomes `g, <group>, proj:<project>:<role>` in the runtime policy (TEN-020); any user whose scope values (TEN-074) include the group gets the role's policies for requests on that project's objects. Group names may be quoted with `"` (required if they contain `,`).
- **Edge cases**: Validation: `group '<g>' must be quoted` (contains comma, unquoted), `group '<g>' is empty`, `group '<g>' contains invalid characters` (`"`, `\n`, `\r`, `\t` inside), `group '<g>' contains a leading space`, `group '<g>' contains a trailing space`. Values from any configured scope (e.g. `email`) work as "groups". Members of a role's groups may create/delete that role's tokens without `projects, update` (TEN-032).
- **Configuration surface**: `spec.roles[].groups[]`; CLI `proj role add-group|remove-group PROJECT ROLE GROUP-CLAIM`.
- **Exposure**: CRD; API; CLI; UI (role groups editor).
- **Tests required**:
  - User with group claim gets role policies on project apps, not on other projects.
  - Each group validation error.
  - Quoted group with comma accepted and matched.

### TEN-020 Project runtime policy generation
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/app_project_types.go#L384-L396, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/rbacpolicy/rbacpolicy.go#L119-L146
- **Maturity**: GA
- **Semantics**: For each role, the runtime policy contains `p, proj:<p>:<r>, projects, get, <p>, allow` (implicit read of the project itself), the role's policies verbatim, and one `g` line per group. This policy is only loaded when the RBAC request object identifies the project: for resources applications/applicationsets/repositories/clusters/logs/exec the first `/`-segment of an object with ≥2 segments; for `projects` the whole object.
- **Edge cases**: A project role can never affect another project's objects because its policy is only loaded for its own project. Objects without `/` (e.g. a global cluster URL that splits to `https:`) load no project policy.
- **Configuration surface**: derived from `spec.roles`.
- **Exposure**: API (all RBAC checks); visible via `argocd proj role get` (prints `ProjectPoliciesString`).
- **Tests required**:
  - Role token can `GET /api/v1/projects/<p>` without an explicit policy.
  - Role policy granting `applications, get, p/*` does not grant `q/x` even with crafted object.
  - `proj role get` output contains the implicit line.

### TEN-021 spec.syncWindows definition and validation
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2921-L2950, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L3432-L3485, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/app_project_types.go#L266-L288, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L3046-L3097, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/sync_windows.md#L1-L8
- **Maturity**: GA
- **Semantics**: Window fields: `kind` (`allow`|`deny`), `schedule` (5-field cron: minute hour dom month dow), `duration` (Go duration), `applications[]`, `namespaces[]`, `clusters[]`, `manualSync`, `timeZone`, `andOperator`, `description` (≤255 chars), `syncOverrun`. API validation: kind, schedule and duration must parse; at least one of applications/namespaces/clusters; no two windows with the same identity hash (kind, schedule, duration, applications, namespaces, clusters, timeZone, andOperator — `manualSync`, `description`, `syncOverrun` excluded). Windows from global projects are appended (TEN-037).
- **Edge cases**: Messages: `kind '<k>' mismatch: can only be allow or deny`; `cannot parse schedule '<s>': ...`; `cannot parse duration '<d>': ...`; `description must not exceed 255 characters`; `window '<k>':'<s>':'<d>' requires one of application, cluster or namespace` (OutOfRange); `sync window with kind ... already exists (hash=..., duplicate detected)` (AlreadyExists). Two windows differing only in `syncOverrun` count as duplicates. Invalid windows stored via kubectl make `CanSync` error: manual sync API returns PermissionDenied `cannot sync: invalid sync window: ...`, auto-sync is skipped.
- **Configuration surface**: `spec.syncWindows[]`.
- **Exposure**: CRD; API; CLI (`proj windows`); UI (Sync Windows tab).
- **Tests required**:
  - Each validation error and code.
  - Duplicate detection ignores manualSync/description/syncOverrun.
  - kubectl-applied invalid schedule blocks manual sync with the exact error and suppresses auto-sync.

### TEN-022 Sync window application selection (OR / AND)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L3099-L3173, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/sync_windows.md#L9-L27
- **Maturity**: GA
- **Semantics**: A window applies to an Application when: default (OR) — app name glob-matches any `applications` entry, OR destination name/server glob-matches any `clusters` entry, OR destination namespace glob-matches any `namespaces` entry. With `andOperator: true` every non-empty selector list must match. Negation is not supported in window selectors.
- **Edge cases**: Cluster matching uses the Application's `spec.destination.name`/`server` as written (not the resolved cluster). With AND, empty selector lists are ignored (not treated as non-matching).
- **Configuration surface**: `syncWindows[].applications|namespaces|clusters|andOperator`.
- **Exposure**: CRD; API; CLI (`--applications`, `--namespaces`, `--clusters`, `--use-and-operator`); UI.
- **Tests required**:
  - OR vs AND truth table across the three selectors.
  - App addressed by name vs server matched by `clusters` entries.

### TEN-023 Sync window evaluation (allow/deny/manualSync)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L3175-L3290, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2092-L2098, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sync.go#L123-L132, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sync.go#L624-L641, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2042-L2044, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/sync_windows.md#L29-L41
- **Maturity**: GA
- **Semantics**: With no matching windows syncs are allowed. If any matching active `deny` window → denied, unless the sync is manual and ALL active deny windows have `manualSync: true`. Else if any matching active `allow` window → allowed. Else if matching `allow` windows exist but none active → denied unless manual and ALL inactive allow windows have `manualSync: true`. Else allowed. A window is active when `cron.Next(now + tzOffset − duration)` is before `now + tzOffset`. Enforcement points: API Sync (manual) returns PermissionDenied `cannot sync: blocked by sync window`; controller auto-sync skips when not allowed; controller operation execution re-checks (`isManual` from `initiatedBy.automated`) and, if blocked while Running, sets message `Sync operation blocked by sync window`.
- **Edge cases**: Deny overrides allow when both active. A single deny window without manualSync blocks manual syncs even if other deny windows allow manual.
- **Configuration surface**: `kind`, `manualSync`, `schedule`, `duration`.
- **Exposure**: CRD; API; CLI (`proj windows enable-manual-sync|disable-manual-sync`); UI (window status colours).
- **Tests required**:
  - Matrix: no windows / active allow / inactive allow / active deny / both, × manual vs automated.
  - Mixed manualSync across multiple deny windows.
  - Running operation becomes blocked mid-flight with the exact message.

### TEN-024 Sync window syncOverrun
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L3197-L3233, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L3292-L3366, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/sync_windows.md#L42-L80, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/test/e2e/project_management_test.go#L581-L634
- **Maturity**: GA
- **Semantics**: Applies only when an operation start time is known (controller operation processing). Active deny case: if ALL active deny windows have `syncOverrun` and the sync would have been allowed at the operation start time, it continues. Inactive-allow case: if ALL inactive allow windows have `syncOverrun` and the sync was allowed at start, it continues.
- **Edge cases**: The documented transition table (9 scenarios) must hold, e.g. "Allow without overrun → Deny with overrun" is allowed; "Multiple allows, one without overrun → all end" is blocked. Manual API sync requests never use overrun (no start time).
- **Configuration surface**: `syncWindows[].syncOverrun`; CLI `--sync-overrun`, `proj windows enable-sync-overrun|disable-sync-overrun`.
- **Exposure**: CRD; API; CLI; UI ("Enable sync overrun").
- **Tests required**:
  - All 9 documented transition scenarios.
  - Overrun ignored for new syncs started inside a deny window.

### TEN-025 Sync window timeZone
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L3036-L3044, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L3433-L3441, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L3395-L3430
- **Maturity**: GA
- **Semantics**: `timeZone` is an IANA name; the schedule is interpreted in that zone by offsetting "now" by the zone's current UTC offset. Empty or unknown zones fall back to UTC (validation rewrites the field to `UTC`; warning logged). `proj windows update` without `--time-zone` resets the zone to `UTC`.
- **Edge cases**: Offset is taken at evaluation time (DST transitions shift windows). Invalid zone does not fail validation.
- **Configuration surface**: `syncWindows[].timeZone`; CLI `--time-zone` (default `UTC`).
- **Exposure**: CRD; API; CLI; UI (Time Zone select).
- **Tests required**:
  - `0 22 * * *` in `Europe/Amsterdam` is active at 20:xx/21:xx UTC depending on DST.
  - Invalid zone stored as `UTC` after API validation.

### TEN-026 spec.signatureKeys (deprecated)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2825-L2831, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2871-L2910, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/util/project.go#L127-L138, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/sourceintegrity/gpg.go#L64-L89
- **Maturity**: Deprecated
- **Semantics**: `signatureKeys[].keyID` lists GnuPG key IDs. Effective behaviour is computed by `EffectiveSourceIntegrity`: if no signature keys → `sourceIntegrity` as-is; if keys and no `sourceIntegrity` → a single git policy `{repos: [{url: '*'}], gpg: {mode: head, keys: <keys>}}`; if `sourceIntegrity.git` is also set → signature keys ignored (error logged); if `sourceIntegrity` set without `git` → merged copy with the migrated git policy. CLI accepts 16-hex short IDs or 40-hex fingerprints (fingerprint truncated to last 16 chars).
- **Edge cases**: CLI error `'<k>' is not a valid GnuPG key ID`; `proj add-signature-key` duplicate → `Specified signature key is already defined in project`; remove missing → `Specified signature key is not configured for project`.
- **Configuration surface**: `spec.signatureKeys[].keyID`; CLI `--signature-keys`, `proj add-signature-key|remove-signature-key`.
- **Exposure**: CRD; API; CLI; UI (GPG SIGNATURE KEYS).
- **Tests required**:
  - The four migration branches of `EffectiveSourceIntegrity`.
  - CLI key ID normalization and validation.

### TEN-027 spec.sourceIntegrity (git GPG policies)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/source_integrity.go#L8-L54, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/sourceintegrity/source_integrity.go#L20-L164, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/sourceintegrity/source_integrity.go#L239-L245
- **Maturity**: GA
- **Semantics**: `sourceIntegrity.git.policies[]` each with `repos[].url` (glob; `!` prefix excludes) and `gpg {mode, keys[]}`. A policy applies to a repo when at least one non-negated URL matches and no negated URL matches. Exactly one applicable policy is required: 0 → no check; >1 → the check always fails with `multiple (N) git source integrity policies found for repo URL: <url>`. Modes: `none` (no verification), `head` (verify target commit or annotated tag), `strict` (verify full ancestry). Keys must be in the repo-server keyring. Applies only to Git sources (not Helm/OCI). Globally disabled when `ARGOCD_GPG_ENABLED` is `false`/`no`.
- **Edge cases**: Unknown mode → error `unknown GPG mode "<m>" configured for GIT source integrity`. Problem reporting is limited to 10 most recent problematic signatures. Verification itself is performed by the repo-server (out of scope); failures surface as comparison/sync errors.
- **Configuration surface**: `spec.sourceIntegrity.git.policies[].repos[].url`, `.gpg.mode`, `.gpg.keys[]`; env `ARGOCD_GPG_ENABLED`.
- **Exposure**: CRD; API; CLI (`proj source-integrity git policies`, TEN-062); UI: none found.
- **Tests required**:
  - Policy selection with negated URLs; zero/one/multiple matches.
  - `none` vs `head` vs `strict` with signed/unsigned history.
  - GPG disabled env bypasses checks.

### TEN-028 spec.permitOnlyProjectScopedClusters
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/app_project_types.go#L482-L505, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L2862-L2863, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/projects.md#L396-L418
- **Maturity**: GA
- **Semantics**: When true, a destination that matches the project's destinations is additionally required to be one of the project-scoped clusters (cluster secrets with `project: <name>`), compared by cluster name OR server. Applies to every destination check (app validation, sync, live-state filtering).
- **Edge cases**: Errors listing project clusters surface as `could not retrieve project clusters: ...`. Not settable via CLI flags or UI (YAML/API only).
- **Configuration surface**: `spec.permitOnlyProjectScopedClusters`.
- **Exposure**: CRD; API; CLI: none (only via `proj create -f`/`proj edit`); UI: none found.
- **Tests required**:
  - Project with `*` destinations + flag: app to non-scoped cluster gets destination condition; app to scoped cluster passes.
  - Match by name when servers differ.

### TEN-029 spec.destinationServiceAccounts (sync impersonation)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L3564-L3572, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/app_project_types.go#L290-L320, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/impersonation.go#L17-L50, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/feature-maturity.md#L43-L43
- **Maturity**: Beta
- **Semantics**: Entries `{server, namespace, defaultServiceAccount}`. First entry (in order, including entries appended from global projects) whose `server` glob-matches the destination cluster server and whose `namespace` glob-matches the app destination namespace wins. `defaultServiceAccount` `ns:name` → `system:serviceaccount:ns:name`; plain `name` → namespace is the app destination namespace (or the Application's namespace if destination namespace empty). Used for sync (controller) and live-resource operations (API) only when impersonation is enabled (TEN-030).
- **Edge cases**: API validation: server/namespace must not contain `!` (`server has an invalid format, '<x>'` / `namespace has an invalid format, '<x>'`), must compile as globs, `defaultServiceAccount` non-blank and free of `!*[]{}\/` (`defaultServiceAccount has an invalid format, '<x>'`), `server/namespace` unique (`destinationServiceAccount '<key>' already added`). Runtime: invalid globs → `invalid glob pattern for destination server|namespace: ...`; bad SA → `default service account contains invalid chars '<x>'`. Matching uses server only (no name field).
- **Configuration surface**: `spec.destinationServiceAccounts[]`; CLI `--dest-service-accounts server,namespace,sa`, `proj add-destination-service-account ... [--service-account-namespace]`.
- **Exposure**: CRD; API; CLI; UI (DESTINATION SERVICE ACCOUNTS).
- **Tests required**:
  - First-match ordering, glob server/namespace.
  - `ns:sa` vs `sa` qualification incl. empty destination namespace.
  - Each validation error.

### TEN-030 Impersonation enablement and enforcement settings
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L566-L568, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L2741-L2760, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sync.go#L266-L293, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/feature-maturity.md#L67-L67
- **Maturity**: Beta
- **Semantics**: `application.sync.impersonation.enabled: "true"` in `argocd-cm` enables impersonation. `application.sync.impersonation.enforced` (default true; only the literal `"false"` disables) controls behaviour when no destination service account matches: enforced → sync fails with `no matching service account found for destination server <s> and namespace <ns>`; not enforced → falls back to the controller's own service account (info log).
- **Edge cases**: Any value other than `"false"` for `enforced` (including garbage) enforces. Enabled flag requires exact `"true"`.
- **Configuration surface**: `argocd-cm`: `application.sync.impersonation.enabled`, `application.sync.impersonation.enforced`.
- **Exposure**: ConfigMap; settings API exposes `impersonationEnabled`; UI shows destination service accounts.
- **Tests required**:
  - Enabled + enforced + no match → sync error message.
  - Enabled + not enforced → sync as controller SA.
  - Disabled → destinationServiceAccounts ignored.

### TEN-031 AppProject status and project-level conditions
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/app_project_types.go#L53-L57, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2259-L2280, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2831-L2842
- **Maturity**: GA
- **Semantics**: `AppProject.status` only contains `jwtTokensByRole` (TEN-034). There are no AppProject conditions; project problems are reported on referencing Applications as conditions: missing project → `InvalidSpecError` `Application referencing project <p> which does not exist`; other project lookup errors (incl. namespace not permitted) → `UnknownError` with the error text; permission failures → `InvalidSpecError` (TEN-040).
- **Edge cases**: Conditions are recomputed on every refresh and cleared when fixed.
- **Configuration surface**: `status.jwtTokensByRole`.
- **Exposure**: CRD; API (Get returns status); UI (Application conditions).
- **Tests required**:
  - App referencing a nonexistent project gets the exact condition; creating the project clears it.
  - AppProject has no `status.conditions`.

## B. Project role tokens

### TEN-032 Project role token creation
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/project/project.go#L84-L164, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/app_project_types.go#L143-L163, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/session/sessionmanager.go#L198-L216, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/project/project.proto#L28-L40, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/project/project.proto#L80-L86, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/projects.md#L217-L259
- **Maturity**: GA
- **Semantics**: `POST /api/v1/projects/{project}/roles/{role}/token` with `{expiresIn (seconds), id, description}`. Authorization: `projects, update, <project>` OR the caller's groups (TEN-074 scopes) intersect the role's `groups`. Token is an HS256 JWT signed with `server.secretkey`, claims `iss: argocd`, `sub: proj:<project>:<role>`, `iat`, `nbf`, `jti` (given id or random UUID), `exp` only if `expiresIn > 0`. Token metadata `{iat, exp, id}` is appended to `status.jwtTokensByRole[role]` (and mirrored to `spec.roles[].jwtTokens`, TEN-034); retried on update conflict; per-project lock. Audit event `ResourceCreated` "`<user> created token`". `description` is accepted but not stored.
- **Edge cases**: Unknown role → NotFound `project '<p>' does not have role '<r>'`. Reused id → InvalidArgument `Token id '<id>' has been used. ` (checked against spec tokens). The token value is returned only once.
- **Configuration surface**: n/a.
- **Exposure**: API (`ProjectService.CreateToken`); CLI `argocd proj role create-token PROJECT ROLE [-e/--expires-in] [-i/--id] [-t/--token-only]` (aliases `token-create`); UI (Roles → role → JWT Tokens → Create, with Token ID / Expires In).
- **Tests required**:
  - Create with and without expiry; claims exactly as specified.
  - Authorization: projects-update allowed; group member without projects-update allowed; neither → PermissionDenied.
  - Duplicate id rejected; unknown role NotFound.
  - Concurrent creations do not lose tokens.

### TEN-033 Project role token deletion (revocation)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/project/project.go#L203-L249, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/app_project_types.go#L118-L141, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/project/project.proto#L20-L26, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/project/project.proto#L88-L91
- **Maturity**: GA
- **Semantics**: `DELETE /api/v1/projects/{project}/roles/{role}/token/{iat}?id=<id>`. Authorization same as creation. Token located by `id` first, then by `iat`, in both spec and status, and removed; the project is updated; audit event `ResourceDeleted` "`<user> deleted token`". Removing the metadata revokes the token (TEN-035).
- **Edge cases**: Unknown role or unknown token → success with empty response (idempotent, no update). CLI only passes ISSUED-AT (no id), so for two tokens issued in the same second the first match by iat is removed.
- **Configuration surface**: n/a.
- **Exposure**: API; CLI `argocd proj role delete-token PROJECT ROLE ISSUED-AT` (aliases `token-delete`, `remove-token`; confirmation prompt); UI (delete token, "Delete All Expired Tokens").
- **Tests required**:
  - Delete by iat and by id; token stops authenticating immediately.
  - Unknown token/role returns success.
  - Authorization matrix as for creation.

### TEN-034 Project token storage and normalization
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/app_project_types.go#L534-L616, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L3512-L3517, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/project/project.go#L550-L580, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L1008-L1008
- **Maturity**: GA
- **Semantics**: Token metadata `{iat (required), exp, id}` lives in `status.jwtTokensByRole[<role>].items` and is mirrored to `spec.roles[].jwtTokens` (legacy). Normalization: tokens without `id` get `id = strconv(iat)`; spec and status lists are merged by id, sorted by iat descending, and written to both; status entries for removed roles are dropped. Run on server start for all projects (`NormalizeProjs`, 3 conflict retries, fatal on failure), on Get/GetDetailedProject responses, on Update requests and around token creation.
- **Edge cases**: Deleting a role removes its token entries (and so revokes its tokens) on next normalization. Legacy tokens created before ids existed are matched by iat.
- **Configuration surface**: `status.jwtTokensByRole`, `spec.roles[].jwtTokens`.
- **Exposure**: CRD; API (Get shows normalized tokens); CLI `proj role list-tokens` (aliases `list-token`, `token-list`; `-u/--unixtime`), `proj role get`.
- **Tests required**:
  - Spec-only legacy tokens migrate to status on startup.
  - Token without id gets iat-based id.
  - Removing a role drops its status entry.

### TEN-035 Project token authentication
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/session/sessionmanager.go#L257-L298, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/app_project_types.go#L96-L116, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/rbacpolicy/rbacpolicy.go#L45-L56
- **Maturity**: GA
- **Semantics**: Tokens with `iss: argocd` are parsed with HMAC (`server.secretkey`); standard `exp`/`nbf`/`iat` validation applies. If the subject has the form `proj:<p>:<r>` (exactly three `:`-separated parts), the project is looked up and the token must exist in `status.jwtTokensByRole[r]`, matched by `jti` or, failing that, by `iat`. No Redis revocation check and no renewal apply to project tokens.
- **Edge cases**: Deleted project → lookup error (Unauthenticated `invalid session: ...`). Missing token → `JWT token for role '<r>' issued at '<iat>' does not exist in project '<p>'`. Because of the iat fallback, a deleted token remains valid if another token for the same role has the same `iat` second. Non-HMAC algorithm → `unexpected signing method: <alg>`.
- **Configuration surface**: `argocd-secret` `server.secretkey`.
- **Exposure**: API (all gRPC/REST via bearer/cookie/`token` metadata); CLI `--auth-token` / `ARGOCD_AUTH_TOKEN`.
- **Tests required**:
  - Valid token works; expired token rejected; tampered signature rejected.
  - Deleted token rejected; deleted project rejected.
  - Same-second iat collision behaviour.

### TEN-036 Project token authorization
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/rbacpolicy/rbacpolicy.go#L58-L117, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/rbacpolicy/rbacpolicy.go#L148-L162, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/account/account.go#L59-L66
- **Maturity**: GA
- **Semantics**: For requests whose object resolves to a project (TEN-020), a project-token subject is evaluated against builtin + `argocd-rbac-cm` policy + that project's runtime policy (default role also applies). If the token's project differs from the request's project → denied. For requests without a project (e.g. `clusters, get, <url>`), the subject `proj:p:r` is evaluated against the global policy only (so global `p`/`g` lines naming `proj:p:r` and `policy.default` apply). Project tokens carry no groups. Project tokens cannot change passwords: InvalidArgument `password can only be changed for local users, not user "proj:p:r"`.
- **Edge cases**: Changes to role policies take effect immediately for existing tokens. Explicit denies in global policy override project-role allows.
- **Configuration surface**: `spec.roles[].policies`; `argocd-rbac-cm`.
- **Exposure**: API.
- **Tests required**:
  - Token for project A denied on project B app even with wildcard policy.
  - Policy change applies to existing token without reissue.
  - `policy.default: role:readonly` grants a project token global read.
  - Global deny overrides role allow.

## C. Multi-tenancy composition and enforcement

### TEN-037 Global projects (globalProjects in argocd-cm)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L178-L181, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L2590-L2606, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L1132-L1205, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/projects.md#L287-L318, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/test/e2e/project_management_test.go#L513-L579
- **Maturity**: GA
- **Semantics**: `argocd-cm` key `globalProjects` is a YAML list of `{projectName, labelSelector}` (full Kubernetes LabelSelector: `matchLabels` and `matchExpressions` In/NotIn/Exists/DoesNotExist). For a project P, every entry whose selector matches P (and whose `projectName` ≠ P) contributes that global project; the effective ("virtual") project is P with these lists appended from each global project in configuration order: `clusterResourceWhitelist`, `clusterResourceBlacklist`, `namespaceResourceWhitelist`, `namespaceResourceBlacklist`, `syncWindows`, `sourceRepos`, `destinations`, `destinationServiceAccounts`. Not inherited: roles, sourceNamespaces, orphanedResources, signatureKeys/sourceIntegrity, permitOnlyProjectScopedClusters, description. Used by controller and API wherever an app's project is resolved.
- **Edge cases**: Invalid YAML → error `error unmarshalling global project settings: ...` (callers log and use no global projects). An invalid selector, list error, or a missing global project aborts processing of the remaining entries (`break`), silently dropping later global projects. Appending to an empty `namespaceResourceWhitelist` turns "all allowed" into "only the inherited list".
- **Configuration surface**: `argocd-cm`: `globalProjects`; labels on AppProjects.
- **Exposure**: ConfigMap; API `GET /api/v1/projects/{name}/globalprojects`, `GET /api/v1/projects/{name}/detailed` (`globalProjects`); UI (INHERITED FROM GLOBAL PROJECTS).
- **Tests required**:
  - Labelled project inherits each listed field; non-inherited fields unaffected.
  - matchLabels and each matchExpressions operator.
  - Missing global project entry stops later entries (parity behaviour).
  - Global project is not its own global project.

### TEN-038 Project-scoped repositories
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L762-L790, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/repository_secrets.go#L564-L599, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L255-L276, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/projects.md#L320-L372
- **Maturity**: GA
- **Semantics**: A repository secret with `project: <name>` is project-scoped. When resolving an app's project, the URLs of its scoped repositories are appended to `sourceRepos` (so they are permitted without being listed). When looking up credentials for a repo URL in the context of a project, a secret with exactly matching `project` wins; otherwise an unscoped secret is used; secrets scoped to other projects are never used. RBAC objects for scoped repos are `<project>/<url>` (TEN-082).
- **Edge cases**: Multiple matching secrets log `Found multiple credentials for repoURL: <url>`. ApplicationSets with templated `project` can only use unscoped repositories (doc).
- **Configuration surface**: repository Secret `data.project`; `argocd repo add --project`.
- **Exposure**: Secret; API (`GetDetailedProject.repositories`, repository APIs with project); CLI (`repo add --project`); UI (SCOPED REPOSITORIES).
- **Tests required**:
  - Scoped repo usable only by apps of that project.
  - Credential precedence: scoped > unscoped; other-project scoped ignored.
  - Scoped URL permitted even when absent from `sourceRepos`.

### TEN-039 Project-scoped clusters
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L776-L789, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/db/cluster.go#L283-L293, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/cluster_informer.go#L150-L158, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/cluster/cluster.go#L45-L50, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/projects.md#L372-L395
- **Maturity**: GA
- **Semantics**: A cluster secret with `project: <name>` is project-scoped. When resolving an app's project, each scoped cluster adds destinations: `{server: <cluster.server>, namespace: '*'}` if the cluster has no `namespaces`, otherwise one entry per listed namespace. Combined with TEN-028 to restrict a project to its own clusters. RBAC object for scoped clusters is `<project>/<server>`.
- **Edge cases**: Malformed cluster cache entries are skipped. Scoped clusters are listed (sanitized) by `GetDetailedProject`.
- **Configuration surface**: cluster Secret `data.project`, `data.namespaces`; `argocd cluster add --project`.
- **Exposure**: Secret; API; CLI; UI (SCOPED CLUSTERS).
- **Tests required**:
  - Scoped cluster without namespaces permits any namespace; with namespaces only those.
  - Scoped cluster permitted even if not in `destinations`.

### TEN-040 Application ↔ project permission enforcement (conditions)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L641-L719, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2259-L2280, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1365-L1389
- **Maturity**: GA
- **Semantics**: `ValidatePermissions` checks sources (TEN-004/005) then destination cluster resolution and permission (TEN-006..008, TEN-028) against the effective project (with global/scoped additions). Controller: on every refresh sets `InvalidSpecError`/`UnknownError` conditions and skips reconciliation/sync when any exist. API: app create/update returns InvalidArgument `application spec for <app> is invalid: <conditions>`.
- **Edge cases**: Destination resolution failure returns early (only that condition). Validation errors from the project lookup itself become `UnknownError`.
- **Configuration surface**: project spec.
- **Exposure**: API (app Create/Update); controller; UI (app conditions).
- **Tests required**:
  - Disallowed repo → condition and no sync; fix project → condition cleared.
  - API create with disallowed destination returns exact InvalidArgument message.

### TEN-041 Resource-level enforcement in controller
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sync.go#L643-L671, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L783-L801, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/state.go#L1000-L1004, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1344-L1361, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L626-L641, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/app_project_types.go#L420-L433
- **Maturity**: GA
- **Semantics**: For each target object at sync: kind must be permitted by resource lists (cluster vs namespaced by API discovery) and, if namespaced, its namespace must be a permitted destination; otherwise the sync task fails with `resource <group>:<kind> is not permitted in project <p>` or `namespace <ns> is not permitted in project '<p>'`; unknown API resource → `failed to get API resource info for <g>/<k>: unable to verify permissions`. Live objects not permitted are dropped from comparison (and deletion during app finalization); target resources not permitted get sync status `Unknown`; tree traversal omits non-permitted children.
- **Edge cases**: Errors while checking live-object permission add `ComparisonError` `Failed to check if live resource "<k>" is permitted in project "<p>": ...`. Cascade delete never deletes non-permitted live objects.
- **Configuration surface**: project resource lists and destinations.
- **Exposure**: controller (sync results, tree, status).
- **Tests required**:
  - Sync of a ClusterRole in a project without cluster whitelist fails with exact message.
  - Manifest in non-permitted namespace fails with exact message.
  - Not-permitted live object is not pruned/deleted.

### TEN-042 AppProject deletion finalizer
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/app_project_types.go#L435-L443, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1189-L1261
- **Maturity**: GA
- **Semantics**: If an AppProject has finalizer `resources-finalizer.argocd.argoproj.io` and is being deleted, the controller removes the finalizer only when no Application references it (`spec.project` matches, namespace allowed, and project namespace permitted); cached references are re-confirmed against the API server; if none exist live the finalizer is removed. Otherwise deletion stays pending (log `Cannot remove project '<p>' finalizer as it is still referenced by application <app>`). Errors retry after a fixed delay.
- **Edge cases**: Fail-closed when the API server cannot be reached. Without the finalizer, kubectl deletion is immediate regardless of references.
- **Configuration surface**: `metadata.finalizers: [resources-finalizer.argocd.argoproj.io]`.
- **Exposure**: CRD; controller.
- **Tests required**:
  - Project with finalizer and referencing app stays Terminating until app deleted.
  - Stale cache reference does not block forever.

### TEN-043 Changing an Application's project
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1347-L1363, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/projects.md#L164-L170
- **Maturity**: GA
- **Semantics**: When an update changes `spec.project`, the caller needs `applications, update` on the old RBAC name (checked by caller and again here) and `applications, create` on the new RBAC name; the new project must exist and permit the app namespace; then permission validation runs against the new project.
- **Edge cases**: Missing create on new project → PermissionDenied with `permission denied: applications, create, <newproj>/<app>, sub: ...`.
- **Configuration surface**: `Application.spec.project`.
- **Exposure**: API (app Update/UpdateSpec/Patch); CLI `argocd app set --project`; UI.
- **Tests required**:
  - Move allowed only with update(old)+create(new).
  - Move to project not permitting the app namespace fails.

## D. Project API

### TEN-044 ProjectService.Create (with upsert)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/project/project.go#L251-L286, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/project/project.proto#L14-L18, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/project/project.proto#L93-L99, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L1207-L1226
- **Maturity**: GA
- **Semantics**: `POST /api/v1/projects` `{project, upsert}`. Requires `projects, create, <name>`. Normalizes role policies, validates (TEN-009), creates in the control-plane namespace. If it already exists: without upsert, returns the existing object when specs are deep-equal, else InvalidArgument `existing project spec is different; use upsert flag to force update; difference in keys "<paths>"`; with upsert, requires `projects, update` and replaces the spec. Emits `ResourceCreated` audit event "`<user> created project`".
- **Edge cases**: Missing payload → InvalidArgument `missing payload 'project' in request`. Upsert path does not run the privilege-escalation checks of Update (TEN-046).
- **Configuration surface**: n/a.
- **Exposure**: API; CLI `argocd proj create [--upsert] [-f file|-]`; UI (New Project).
- **Tests required**:
  - Create without permission denied.
  - Idempotent create of identical spec returns existing.
  - Differing spec without upsert returns the diff-keys message; with upsert requires update permission.

### TEN-045 ProjectService.Get / List / GetDetailedProject / GetGlobalProjects
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/project/project.go#L288-L357, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L739-L760, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/project/project.proto#L101-L119
- **Maturity**: GA
- **Semantics**: `GET /api/v1/projects` lists projects filtered by `projects, get, <name>` (no error for hidden ones). `GET /api/v1/projects/{name}` requires `projects, get`; returns project with normalized tokens. `GET /api/v1/projects/{name}/detailed` returns `{project (virtual, with global additions), globalProjects, repositories (scoped, normalized+sanitized), clusters (scoped, sanitized)}`. `GET /api/v1/projects/{name}/globalprojects` returns matching global projects.
- **Edge cases**: Get on non-existent project after RBAC pass returns the Kubernetes NotFound. Detailed uses informer cache (may lag).
- **Configuration surface**: n/a.
- **Exposure**: API; CLI `argocd proj list [-o json|yaml|wide|name]`, `argocd proj get PROJECT [-o json|yaml|wide]` (uses detailed API); UI.
- **Tests required**:
  - List hides projects without get permission.
  - Detailed response contains sanitized scoped repos/clusters (no secrets).

### TEN-046 ProjectService.Update — privilege-escalation protections
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/project/project.go#L359-L403, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/project/util.go#L3-L20, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L3487-L3496, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/project/project_test.go#L142-L198
- **Maturity**: GA
- **Semantics**: `PUT /api/v1/projects/{project.metadata.name}` requires `projects, update`. Then, under a per-project lock: for every destination server value present in exactly one of (new, old) lists → `clusters, update, <server>`; for every sourceRepo present in exactly one of (new, old) → `repositories, update, <repo>`; if any of the four resource allow/deny lists changed → `clusters, update` on every destination server of the new spec. Policies normalized, tokens normalized, spec validated before these checks.
- **Edge cases**: Both additions and removals require the permission (symmetric difference). Name-only destinations contribute `""` to the server list (checked against object `""`). Changing role policies/groups/windows requires only `projects, update`.
- **Configuration surface**: n/a.
- **Exposure**: API; CLI (all mutating `argocd proj` commands go through Update); UI.
- **Tests required**:
  - Adding a destination without `clusters, update` → PermissionDenied.
  - Adding a source without `repositories, update` → PermissionDenied.
  - Changing clusterResourceWhitelist without cluster update on all destinations → denied.
  - Removing a destination also requires the cluster permission.

### TEN-047 ProjectService.Update — application impact validation
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/project/project.go#L405-L462, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/project/project_test.go#L200-L320
- **Maturity**: GA
- **Semantics**: Lists Applications of the project in the control-plane namespace; counts apps whose source was permitted by the old spec but not the new, and apps whose destination was permitted before but not after (apps whose destination cluster is missing count as invalid only for `ErrDestinationMissing`; other cluster lookup errors skip the app). If any, rejects with InvalidArgument `as a result of project update N applications source became invalid and M applications destination became invalid` (parts omitted when zero). Otherwise updates and emits `ResourceUpdated` "`<user> updated project`".
- **Edge cases**: Only apps in the control-plane namespace are counted (apps in other namespaces are not protected). Validation uses the raw project spec (no global/scoped additions) for both old and new.
- **Configuration surface**: n/a.
- **Exposure**: API; CLI; UI.
- **Tests required**:
  - Removing a source used by an app is rejected; succeeds if another pattern still permits it.
  - Removing a destination used by an app is rejected; succeeds if another permits it.
  - App in another namespace does not block (parity).

### TEN-048 ProjectService.Delete
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/project/project.go#L464-L494, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/project/project.proto#L129-L132
- **Maturity**: GA
- **Semantics**: `DELETE /api/v1/projects/{name}`. Rejects `default`; requires `projects, delete`; under lock, fails if any Application in the control-plane namespace references the project: InvalidArgument `project is referenced by N applications`; else deletes and emits `ResourceDeleted` "`<user> deleted project`".
- **Edge cases**: Apps in other namespaces are not counted by this check (the controller finalizer, TEN-042, still protects if present).
- **Configuration surface**: n/a.
- **Exposure**: API; CLI `argocd proj delete PROJECT...` (multiple names, confirmation prompt); UI (Delete).
- **Tests required**:
  - Delete referenced project rejected with count; unreferenced deleted.
  - Permission denied without `projects, delete`.

### TEN-049 ProjectService.ListEvents
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/project/project.go#L496-L514, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/project/project.proto#L134-L137
- **Maturity**: GA
- **Semantics**: `GET /api/v1/projects/{name}/events` requires `projects, get`; returns Kubernetes Events in the control-plane namespace whose involvedObject name/uid/namespace match the project.
- **Edge cases**: Non-existent project → NotFound after RBAC check.
- **Configuration surface**: n/a.
- **Exposure**: API; UI (Events tab).
- **Tests required**:
  - Events produced by project create/update appear.
  - Denied without get permission.

### TEN-050 ProjectService.ListLinks (project deep links)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/project/project.go#L166-L201, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/project/project.proto#L144-L147, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L557-L557
- **Maturity**: GA
- **Semantics**: `GET /api/v1/projects/{name}/links` requires `projects, get`; evaluates `project.links` deep-link templates from `argocd-cm` against the project object with `status` stripped; evaluation errors are logged and the failing links omitted.
- **Edge cases**: RBAC failure returns a generic `unauthorized access to project <name>` error.
- **Configuration surface**: `argocd-cm` `project.links`.
- **Exposure**: API; UI (Summary → LINKS).
- **Tests required**:
  - Link template referencing `project.metadata.name` renders.
  - Template referencing status data renders empty.

### TEN-051 ProjectService.GetSyncWindowsState
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/project/project.go#L526-L548, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/project/project.proto#L139-L142
- **Maturity**: GA
- **Semantics**: `GET /api/v1/projects/{name}/syncwindows` requires `projects, get`; returns the currently active windows of the project (raw spec, not including global projects; no app matching), or an empty list.
- **Edge cases**: Unparsable window → error returned.
- **Configuration surface**: n/a.
- **Exposure**: API; UI (sync window status).
- **Tests required**:
  - Active vs inactive windows at controlled time.
  - Denied without project get.

### TEN-052 Audit events via Kubernetes Events
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/audit_logger.go#L20-L230, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/project/project.go#L516-L524, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-server/commands/argocd_server.go#L341-L341
- **Maturity**: GA
- **Semantics**: Audit logger writes a log line and a `corev1.Event` (type Normal, reason one of `StatusRefreshed`, `ResourceCreated`, `ResourceUpdated`, `ResourceDeleted`, `ResourceActionRan`, `OperationStarted`, `OperationCompleted`; source component e.g. `argocd-server`; involvedObject = Application/AppProject/ApplicationSet or resource; name `<obj>.<hex nanos>`; message `<user> <action>`, user = email for SSO, subject for local, `Unknown user` if none). Application events carry annotations `dest-server`, `dest-namespace`, `user`; AppProject events carry no annotations. Resource events are created in the control-plane namespace with annotation `resource-namespace`. `--enable-k8s-event` / `ARGOCD_ENABLE_K8S_EVENT` (default `all`; `none` disables; otherwise list of reasons) filters which reasons are emitted (server and controller).
- **Edge cases**: Event create failures are logged only. `all`/`none` short-circuit list processing.
- **Configuration surface**: flag `--enable-k8s-event`; env `ARGOCD_ENABLE_K8S_EVENT`.
- **Exposure**: Kubernetes Events; API (project/app event listing).
- **Tests required**:
  - Project create/update/delete and token create/delete produce events with exact reason/message.
  - `--enable-k8s-event=none` suppresses; `ResourceCreated` only allows create events.

## E. Project CLI and UI

### TEN-053 CLI: argocd proj create / set / get / list / edit / delete
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/project.go#L106-L185, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/project.go#L833-L1168, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/util/project.go#L37-L247, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/commands/argocd_proj_create.md#L1-L1
- **Maturity**: GA
- **Semantics**: `proj create PROJECT` flags: `--description`, `-d/--dest server,namespace` (repeatable), `--dest-service-accounts server,namespace,sa`, `-s/--src`, `--signature-keys`, `--source-namespaces`, `--allow-cluster-resource [group/]kind[/name]`, `--deny-cluster-resource`, `--allow-namespaced-resource [group/]kind`, `--deny-namespaced-resource`, `--orphaned-resources`, `--orphaned-resources-warn`, `-f/--file` (path, URL or `-` for stdin), `--upsert`. `proj set PROJECT` takes the same spec flags and replaces only the fields whose flags are given (prints help and exits 1 if none). `proj get` (`-o json|yaml|wide`), `proj list` (`-o json|yaml|wide|name`), `proj edit` (opens editor on YAML, then Update), `proj delete PROJECT...` (confirmation per project).
- **Edge cases**: `-f` file whose `metadata.name` differs from the argument → `project name '<a>' does not match project spec metadata.name '<b>'`. Malformed `--dest` → `Expected destination of the form: server,namespace. Received: <x>`; malformed `--dest-service-accounts` → `Expected destination service account of the form: server,namespace, defaultServiceAccount. Received: <x>`.
- **Configuration surface**: CLI flags above.
- **Exposure**: CLI.
- **Tests required**:
  - Each flag maps to the right spec field; `set` leaves untouched fields unchanged.
  - Parsing errors with exact messages.
  - `create -f -` reads stdin.

### TEN-054 CLI: proj add/remove-source and add/remove-source-namespace
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/project.go#L477-L598, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/project.go#L789-L831
- **Maturity**: GA
- **Semantics**: `proj add-source PROJECT URL` appends (prints `Source repository '<x>' already allowed in project` and does nothing if present); `remove-source` removes (prints `Source repository '<x>' does not exist in project` if absent). `add-source-namespace PROJECT NAMESPACE` / `remove-source-namespace` analogous for `sourceNamespaces`.
- **Edge cases**: `remove-source-namespace` cannot remove `*`. Negated URLs are passed literally (`'!<url>'`).
- **Configuration surface**: CLI.
- **Exposure**: CLI.
- **Tests required**:
  - Idempotent add/remove messages.
  - `*` source namespace removal refused.

### TEN-055 CLI: proj add/remove-destination
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/project.go#L277-L372
- **Maturity**: GA
- **Semantics**: `add-destination PROJECT SERVER/NAME NAMESPACE [--name]` appends `{server|name, namespace}`; duplicate → fatal `Specified destination is already defined in project`. `remove-destination PROJECT SERVER NAMESPACE` removes; missing → fatal `Specified destination does not exist in project`.
- **Edge cases**: `remove-destination` matches by server only (no `--name`).
- **Configuration surface**: CLI.
- **Exposure**: CLI.
- **Tests required**:
  - Add by server and by `--name`; duplicate errors.
  - Remove missing destination error.

### TEN-056 CLI: proj allow/deny cluster and namespace resource commands
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/project.go#L600-L786, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/commands/argocd_proj_allow-cluster-resource.md#L1-L1
- **Maturity**: GA
- **Semantics**: `allow-cluster-resource PROJECT GROUP KIND [NAME]` and `deny-cluster-resource PROJECT GROUP KIND [NAME]` (default `-l/--list allow`): allow adds to the chosen list when it is the allow list and removes from it when it is the deny list; deny does the inverse. `allow-namespace-resource PROJECT GROUP KIND` and `deny-namespace-resource` (default `-l deny`, exactly 3 args, no NAME). `--list` accepts `allow`/`white`, anything else means deny list. Prints e.g. `Group '<g>' and kind '<k>' is added to allowed namespaced resources`; no update when nothing changed.
- **Edge cases**: `deny-cluster-resource` accepts an optional NAME though its usage string omits it. Cluster items match on group+kind+name exactly when removing.
- **Configuration surface**: CLI.
- **Exposure**: CLI.
- **Tests required**:
  - All four commands × both lists produce the documented add/remove behaviour and messages.
  - NAME handling for cluster resources.

### TEN-057 CLI: proj add/remove-orphaned-ignore
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/project.go#L374-L475
- **Maturity**: GA
- **Semantics**: `add-orphaned-ignore PROJECT GROUP KIND [--name pattern]` appends to `orphanedResources.ignore` (creating `orphanedResources` if nil); duplicate → `Specified resource is already defined in the orphaned ignore list of project`. `remove-orphaned-ignore` removes the matching item.
- **Edge cases**: Remove when `orphanedResources` is nil → `Specified resource does not exist in the orphaned ignore list of project`; item not found → `Specified resource does not exist in the orphaned ignore of project`.
- **Configuration surface**: CLI.
- **Exposure**: CLI.
- **Tests required**:
  - Add enables monitoring implicitly; duplicates rejected.
  - Both remove error messages.

### TEN-058 CLI: proj add/remove-signature-key
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/project.go#L187-L275
- **Maturity**: Deprecated
- **Semantics**: `add-signature-key PROJECT KEY-ID` validates the key ID (16-hex or 40-hex) and appends to `signatureKeys`; `remove-signature-key` removes it.
- **Edge cases**: Messages in TEN-026.
- **Configuration surface**: CLI.
- **Exposure**: CLI.
- **Tests required**:
  - Fingerprint input stored as short key ID.
  - Duplicate/missing errors.

### TEN-059 CLI: proj add/remove-destination-service-account
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/project.go#L1170-L1284
- **Maturity**: Beta
- **Semantics**: `add-destination-service-account PROJECT SERVER NAMESPACE SERVICE_ACCOUNT [--service-account-namespace NS]` appends `{server, namespace, defaultServiceAccount: SA or NS:SA}`; `remove-destination-service-account PROJECT SERVER NAMESPACE SERVICE_ACCOUNT` removes.
- **Edge cases**: `*` in service account or SA namespace → fatal `ServiceAccount for DestinationServiceAccount must not contain wildcards` / `service-account-namespace for DestinationServiceAccount must not contain wildcards`; duplicate → `Specified destination service account is already defined in project`; missing → `Specified destination service account does not exist in project`.
- **Configuration surface**: CLI.
- **Exposure**: CLI.
- **Tests required**:
  - SA namespace qualification.
  - Wildcard/duplicate/missing errors.

### TEN-060 CLI: argocd proj role command group
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/project_role.go#L27-L703, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/project.go#L93-L98
- **Maturity**: GA
- **Semantics**: Subcommands: `list PROJECT [-o json|yaml|wide|name]`, `get PROJECT ROLE` (name, description, policies incl. implicit line, groups, tokens), `create PROJECT ROLE [--description]` (prints `Role '<r>' created` / `Role '<r>' already exists`), `delete PROJECT ROLE` (confirm), `add-policy PROJECT ROLE -a ACTION [-p allow|deny] -o OBJECT [-r RESOURCE]` (builds `p, proj:<p>:<r>, <res>, <act>, <p>/<obj>, <perm>`; resource default `applications`, must be project-scoped else help+exit 1), `remove-policy` (same flags, confirm), `add-group PROJECT ROLE GROUP-CLAIM`, `remove-group` (confirm), `create-token`, `delete-token`, `list-tokens` (TEN-032..TEN-034).
- **Edge cases**: All mutations use `ProjectService.Update` (projects update permission). `add-group` existing → `Group '<g>' already present in role '<r>'`.
- **Configuration surface**: CLI.
- **Exposure**: CLI.
- **Tests required**:
  - Each subcommand's output and side effect.
  - add-policy with non-project-scoped resource exits with help.

### TEN-061 CLI: argocd proj windows command group
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/projectwindows.go#L24-L456
- **Maturity**: GA
- **Semantics**: `add PROJECT -k kind --schedule --duration [--applications] [--namespaces] [--clusters] [--manual-sync] [--time-zone UTC] [--use-and-operator] [--description] [--sync-overrun]`; `delete PROJECT ID` (confirm); `update PROJECT ID [--schedule --duration --applications --namespaces --clusters --time-zone --description]`; `list PROJECT [-o json|yaml|wide]` (columns ID, STATUS, KIND, SCHEDULE, DURATION, APPLICATIONS, NAMESPACES, CLUSTERS, MANUALSYNC, SYNCOVERRUN, TIMEZONE, USEANDOPERATOR); `enable-manual-sync|disable-manual-sync|enable-sync-overrun|disable-sync-overrun PROJECT ID`. ID is the zero-based index in `spec.syncWindows`.
- **Edge cases**: `add` missing kind/schedule/duration → `cannot create window: require kind, schedule, duration and one or more of applications, namespaces and clusters`; `update` with nothing → `cannot update: require one or more of schedule, duration, application, namespace, cluster or description`; bad ID → `window with id '<n>' not found`.
- **Configuration surface**: CLI.
- **Exposure**: CLI.
- **Tests required**:
  - Full add/update/delete/list cycle.
  - Toggle commands flip only the targeted field.
  - Error messages.

### TEN-062 CLI: argocd proj source-integrity git policies
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/project_source_integrity.go#L57-L470
- **Maturity**: GA
- **Semantics**: `list PROJECT`; `add PROJECT --repo-url (repeatable) --gpg-mode strict|head|none --gpg-key (repeatable)`; `update PROJECT POLICY_ID [--repo-url|--add-repo-url|--delete-repo-url] [--gpg-key|--add-gpg-key|--delete-gpg-key] [--gpg-mode] [-y]`; `delete PROJECT POLICY_ID... [-y]`. POLICY_ID is the zero-based policy index.
- **Edge cases**: `invalid POLICY_ID '<x>'`, `the POLICY_ID N is out of range (0-M)`, `gpg-mode must be set`, `gpg-mode must be one of: strict, head, none`, `invalid GPG key ID '<k>': ...`, `option --gpg-key, cannot be combined with --add-gpg-key or --delete-gpg-key`, `option --repo-url, cannot be combined with --add-repo-url or --delete-repo-url`.
- **Configuration surface**: CLI.
- **Exposure**: CLI.
- **Tests required**:
  - Add/list/update/delete round trip.
  - Each validation error.

### TEN-063 CLI: argocd admin proj
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/project.go#L28-L247, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/project_allowlist.go#L36-L166
- **Maturity**: GA
- **Semantics**: `generate-spec PROJECT` (same spec flags as `proj create`, `-f/--file`, `-i/--inline`, `-o json|yaml`) prints an AppProject manifest offline. `generate-allow-list CLUSTERROLE_PATH PROJ_NAME [-o file]` builds an AppProject whose `namespaceResourceWhitelist` contains every group/kind the ClusterRole may `create` (resolved via cluster discovery). `update-role-policy PROJECT_GLOB set|remove ACTION [--role pattern] [--resource] [--scope] [--permission] [--dry-run=true]` edits role policies directly via the Kubernetes API (bypassing Argo CD validation), printing diffs.
- **Edge cases**: `set` without scope → `flag --group cannot be empty if permission should be set in role` (message names a non-existent flag); without permission → `flag --permission cannot be empty if permission should be set in role`; unknown modification → `modification <m> is not supported`. Dry-run is the default.
- **Configuration surface**: kubeconfig flags.
- **Exposure**: CLI (requires Kubernetes access, not the Argo CD API).
- **Tests required**:
  - generate-spec output equals what `proj create` would send.
  - generate-allow-list from a sample ClusterRole.
  - update-role-policy dry-run makes no change; non-dry-run updates matching roles only.

### TEN-064 UI: project management
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/settings/components/project-details/project-details.tsx#L337-L918, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/settings/components/project-details/resource-lists-panel.tsx#L34-L150, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/settings/components/project-sync-windows-edit-panel/project-sync-windows-edit-panel.tsx#L57-L82, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/settings/components/project-role-jwt-tokens/project-role-jwt-tokens.tsx#L36-L80, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/settings/components/projects-list/projects-list.tsx#L24-L24
- **Maturity**: GA
- **Semantics**: Settings → Projects: list + "New Project"; project page tabs Summary / Roles / Sync Windows / Events; Summary sections GENERAL (name, description, labels, links, applications), SOURCE REPOSITORIES, SCOPED REPOSITORIES, SOURCE NAMESPACES, DESTINATIONS, SCOPED CLUSTERS, DESTINATION SERVICE ACCOUNTS, INHERITED FROM GLOBAL PROJECTS, GPG SIGNATURE KEYS, RESOURCE MONITORING (enable, warn, ignore list), cluster/namespace allow/deny lists; Roles: name, description, policies editor, groups editor, JWT tokens (create with Token ID/Expires In, delete, "Delete All Expired Tokens"); Sync Windows: kind, schedule, time zone, duration, manual sync, sync overrun, AND operator, description, applications/namespaces/clusters; Delete project.
- **Edge cases**: No UI controls found for `permitOnlyProjectScopedClusters` or `sourceIntegrity`. Errors shown as notifications ("Unable to update project", etc.).
- **Configuration surface**: n/a.
- **Exposure**: UI.
- **Tests required**:
  - E2E UI flows for each section using the API contracts above.
  - Expired token bulk delete.

## F. RBAC engine

### TEN-065 Casbin model and enforcement pipeline
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/assets/model.conf#L1-L14, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/rbac/rbac.go#L340-L426, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L350-L359, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/rbac.md#L43-L70
- **Maturity**: GA
- **Semantics**: Request `(sub, res, act, obj)`; policy `(sub, res, act, obj, eft)`; role definition `g = _, _` (user/group → role, transitive); effect `some(allow) && !some(deny)`; matcher `g(r.sub, p.sub) && match(r.res, p.res) && match(r.act, p.act) && match(r.obj, p.obj)` where match is glob or regex (TEN-075). Pipeline per request: (1) if `policy.default` set, enforce with the default role as subject — allow returns true; (2) if subject is JWT claims, run the claims enforcer (TEN-074/TEN-036) — allow returns true; (3) enforce with the raw subject (`""` for claims/anonymous). Policy sources are concatenated: built-in, user (`argocd-rbac-cm`), runtime (project).
- **Edge cases**: Casbin errors are treated as deny. Subject strings are matched exactly through `g` (no patterns in role membership).
- **Configuration surface**: `argocd-rbac-cm`.
- **Exposure**: API (every RPC), HTTP handlers.
- **Tests required**:
  - Transitive role inheritance (`g, u, r1`, `g, r1, r2`).
  - Default-role allow short-circuits; default-role deny does not block user policies.
  - Unknown subject denied.

### TEN-066 policy.csv grammar and parsing
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/rbac/rbac.go#L575-L634, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/rbac/rbac_test.go#L418-L437, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/rbac/rbac_test.go#L525-L571, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-rbac-cm.yaml#L10-L20
- **Maturity**: GA
- **Semantics**: Policy text is split on `\n`; each line trimmed; empty lines and lines starting with `#` ignored; each line parsed as CSV (double-quote quoting, leading space trimmed). `p` lines must have exactly 6 tokens; `g` lines exactly 3; the first token must be a section key present in the model (`p` or `g`). Quoted values may contain commas (e.g. `"role,admin"`).
- **Edge cases**: Any invalid line fails the whole policy load: `invalid RBAC policy: <line>` or `error parsing policy line "<line>": ...` (consequences at startup and on live reload: TEN-078). Inline trailing comments are not supported.
- **Configuration surface**: `argocd-rbac-cm` `policy.csv`, `policy.*.csv`.
- **Exposure**: ConfigMap; CLI validate (TEN-088).
- **Tests required**:
  - Upstream good/bad policy fixtures (`TestValidatePolicy`, `TestLoadPolicyLine`).
  - Quoted subject with comma enforced correctly.

### TEN-067 RBAC resources
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/rbac/rbac.go#L58-L119, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/rbac.md#L72-L86
- **Maturity**: GA
- **Semantics**: Resources: `clusters`, `projects`, `applications`, `applicationsets`, `repositories`, `write-repositories`, `certificates`, `accounts`, `gpgkeys`, `logs`, `exec`, `extensions`. Project-scoped (object prefixed by project): `applications`, `applicationsets`, `logs`, `exec`, `clusters` (for scoped clusters), `repositories` (for scoped repos).
- **Edge cases**: Resource names are matched with glob/regex like other tokens (`*` grants all resources).
- **Configuration surface**: policy `res` token.
- **Exposure**: API; CLI `account can-i` validates against this list.
- **Tests required**:
  - Each resource enforced at its API (see TEN-068 table).
  - `*` resource wildcard.

### TEN-068 RBAC actions and per-resource enforcement
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/rbac/rbac.go#L73-L109, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2100-L2111, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2215-L2251, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2673-L2674, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/rbac.md#L186-L251
- **Maturity**: GA
- **Semantics**: Actions: `get`, `create`, `update`, `delete`, `sync`, `override`, `action`, `invoke`, plus path forms `action/<group>/<kind>/<action-name>` (resource actions; core group empty, e.g. `action//Pod/maintenance-off`), `update/<group>/<kind>/<ns>/<name>` and `delete/<group>/<kind>/<ns>/<name>` (TEN-070). `sync` is required for sync/rollback/terminate-operation; `override` additionally for syncing local manifests, and for syncing to a revision different from the app's target revision when `application.sync.requireOverridePrivilegeForRevisionSync: "true"`. `applicationsets` use get/create/update/delete with object `<project>/<appset>` where project is the template project. `extensions` use `invoke` with object = extension name (plus `applications, get` on the originating app). `logs, get` and `exec, create` use app RBAC names. Certificates: get/create/delete; gpgkeys: get/create/delete; accounts: get/update; clusters/repositories/projects: get/create/update/delete.
- **Edge cases**: Local sync with auto-sync enabled → FailedPrecondition `cannot use local sync when Automatic Sync Policy is enabled unless for dry run`. Glob `*` in action does not treat `/` as separator (`action/*` matches all actions).
- **Configuration surface**: policy `act` token; `argocd-cm` `application.sync.requireOverridePrivilegeForRevisionSync`.
- **Exposure**: API.
- **Tests required**:
  - Sync with/without `sync`; local-manifest sync needs `override`.
  - Revision override needs `override` only when the setting is true.
  - `action/apps/Deployment/restart` granted narrowly.
  - Extensions invoke requires both policies.

### TEN-069 Object syntax for project-scoped resources (apps in any namespace)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/security/rbac.go#L7-L13, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L4153-L4155, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L176-L253, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/rbac.md#L88-L116, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/app-any-namespace.md#L137-L150
- **Maturity**: GA
- **Semantics**: Application/ApplicationSet RBAC object is `<project>/<name>` when the object lives in the control-plane namespace (or namespace empty), else `<project>/<namespace>/<name>`. Project defaults to `default`. Policies like `*/app-ns/*` restrict by namespace; `proj/*` (glob without separators) matches both 2- and 3-segment names.
- **Edge cases**: Control-plane apps can never be matched by a `proj/argocd/*` style object (always 2 segments). When a request specifies a project, a first RBAC check is done on the given project and a second on the actual app's project; mismatches return NotFound to avoid leaking existence (TEN-081).
- **Configuration surface**: policy `obj` token.
- **Exposure**: API.
- **Tests required**:
  - Policy `*/team-ns/*` grants only apps in `team-ns`.
  - Control-plane app matches `p/app` not `p/argocd/app`.

### TEN-070 Fine-grained update/* and delete/* sub-resource actions
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1485-L1518, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/assets/builtin-policy.csv#L20-L27, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/rbac.md#L122-L165
- **Maturity**: GA
- **Semantics**: Operations on an application's managed resources (patch/delete resource, run resource actions) are authorized with action `update/<group>/<kind>/<namespace>/<name>` or `delete/<group>/<kind>/<namespace>/<name>` on the app RBAC object. With the v3 default, plain `update`/`delete` on the application does not cover resources. The resource must exist in the app's resource tree, else InvalidArgument `<Kind> <group> <name> not found as part of application <app>`.
- **Edge cases**: Glob does not treat `/` as separator: `delete/*/Pod/*` can also match other shapes; docs recommend always specifying four segments. Deny on `update/*` with allow on `update` permits app edits but not resource edits.
- **Configuration surface**: policy `act` token.
- **Exposure**: API (`PatchResource`, `DeleteResource`, `RunResourceAction`, resource get on live objects).
- **Tests required**:
  - `delete/*/Pod/*/*` allows Pod delete but not Deployment delete.
  - App-level `delete` does not allow resource delete (default).
  - Resource outside the tree rejected with exact message.

### TEN-071 server.rbac.disableApplicationFineGrainedRBACInheritance
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L542-L543, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L936-L947, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1485-L1498, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/rbac.md#L166-L184
- **Maturity**: GA
- **Semantics**: `argocd-cm` key, default (empty) = `true`. When true: resource operations check only the fine-grained action. When `false` (pre-v3 behaviour): first check plain `update`/`delete` on the app; only if that is PermissionDenied, check the fine-grained action.
- **Edge cases**: With `false`, an explicit allow on app-level `delete` cannot be narrowed by a fine-grained deny. Non-boolean value → parse error returned to the caller.
- **Configuration surface**: `argocd-cm` `server.rbac.disableApplicationFineGrainedRBACInheritance`.
- **Exposure**: ConfigMap; API.
- **Tests required**:
  - Default vs `false` behaviour for the documented policy examples.
  - Invalid value surfaces an error.

### TEN-072 Deny precedence
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/assets/model.conf#L10-L11, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/rbac.md#L283-L306
- **Maturity**: GA
- **Semantics**: Within one enforcement call, any matching `deny` wins over any number of matching `allow`s, independent of policy order. Denies apply per subject evaluation: a deny for the user subject does not prevent an allow via the default role (evaluated first) or via a group evaluation.
- **Edge cases**: Group evaluation is per group: user subject denied but group allowed → allowed. Project-role allow cannot override a global deny for the same subject because they are evaluated in the same enforcer.
- **Configuration surface**: `eft` token.
- **Exposure**: API.
- **Tests required**:
  - Allow+deny same subject → deny regardless of order.
  - User-level deny vs group-level allow → allowed (parity).
  - Default-role allow cannot be blocked by a deny on the user.

### TEN-073 policy.default (default role)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/rbac/rbac.go#L314-L320, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/rbac/rbac.go#L399-L406, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/rbac.md#L22-L31, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-rbac-cm.yaml#L31-L34
- **Maturity**: GA
- **Semantics**: `policy.default: <role>` is evaluated first for every request (authenticated, anonymous, project token); if it allows, access is granted. Empty/omitted → no default.
- **Edge cases**: Default-role permissions cannot be removed for any user by deny rules. A default-role deny only fails the default check and evaluation continues with the user's own policies.
- **Configuration surface**: `argocd-rbac-cm` `policy.default`.
- **Exposure**: ConfigMap.
- **Tests required**:
  - `role:readonly` default grants read to arbitrary authenticated users.
  - Default change is picked up live.

### TEN-074 scopes and group-based evaluation
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/rbacpolicy/rbacpolicy.go#L33-L43, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/rbacpolicy/rbacpolicy.go#L82-L116, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/jwt/jwt.go#L50-L75, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L892-L907, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/rbac.md#L328-L378
- **Maturity**: GA
- **Semantics**: `scopes` in `argocd-rbac-cm` is a YAML list of claim names (default `[groups]`). After the subject check fails, values of these claims (string, []string or []any of strings) are collected as groups; each group that appears as the first element of at least one `g` line (global or project runtime) is evaluated as subject; first allow wins. Groups with only `p` lines and no `g` line are never evaluated.
- **Edge cases**: Invalid `scopes` YAML → error `error unmarshalling scopes: ...` (fatal at startup; on live update the user policy is not reloaded while default role/match mode already changed). Any claim (e.g. `email`) can act as a group. A group named like a local user (e.g. `admin`) inherits that user's bindings (builtin `g, admin, role:admin`).
- **Configuration surface**: `argocd-rbac-cm` `scopes`.
- **Exposure**: ConfigMap; `GetUserInfo.groups` uses the same scopes.
- **Tests required**:
  - Default scopes; custom `[groups, email]`; string-valued claim.
  - Group with only `p` lines not honoured; adding `g` line makes it work.
  - SSO user with group `admin` gets admin (parity security test).

### TEN-075 policy.matchMode (glob / regex)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/rbac/rbac.go#L35-L42, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/rbac/rbac.go#L285-L312, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/rbac/rbac.go#L180-L183, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/rbac/rbac_test.go#L468-L509, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/rbac.md#L299-L323
- **Maturity**: GA
- **Semantics**: `glob` (default, also used for any unrecognized value): gobwas glob with no separators (`*` matches `/`). `regex`: Casbin regex match (Go `regexp`). Applies to res, act and obj tokens (not to role membership).
- **Edge cases**: Changing mode flushes the enforcer cache. See Unconfirmed for anchoring and built-in `*` patterns in regex mode.
- **Configuration surface**: `argocd-rbac-cm` `policy.matchMode`.
- **Exposure**: ConfigMap; CLI `admin settings rbac can` honours it.
- **Tests required**:
  - Upstream `TestGlobMatchMode`/`TestRegexMatchMode` cases.
  - Unknown value behaves as glob.

### TEN-076 Built-in roles (role:readonly, role:admin)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/assets/builtin-policy.csv#L1-L53, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L352-L356, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/rbac.md#L13-L20
- **Maturity**: GA
- **Semantics**: Built-in policy always loaded: `role:readonly` = get on applications/applicationsets/logs (`*/*`), certificates, clusters, repositories, write-repositories, projects, accounts, gpgkeys (`*`). `role:admin` = create/update/`update/*`/delete/`delete/*`/sync/override/`action/*` on applications, get/create/update/delete applicationsets, create/update/delete certificates, clusters, repositories, write-repositories, projects; update accounts; create/delete gpgkeys; create exec; inherits `role:readonly`; user `admin` is bound to `role:admin`. `extensions` is not granted by either role.
- **Edge cases**: User policies cannot remove built-in grants except via deny on the same subject. `exec, create` is in admin but exec also requires `exec.enabled` (other area).
- **Configuration surface**: none (embedded asset).
- **Exposure**: RBAC.
- **Tests required**:
  - Matrix of every built-in grant for both roles.
  - `admin` lacks `extensions, invoke` unless granted.

### TEN-077 policy.<name>.csv overlays
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/rbac/rbac.go#L508-L537, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/rbac/rbac_test.go#L47-L108, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/rbac.md#L416-L441
- **Maturity**: GA
- **Semantics**: User policy = `policy.csv` followed by every other key matching `policy.*.csv`, in lexical key order, each joined with a newline.
- **Edge cases**: Overlays are parsed with the same grammar; an invalid overlay invalidates the whole user policy.
- **Configuration surface**: `argocd-rbac-cm` `policy.<any>.csv`.
- **Exposure**: ConfigMap; CLI `admin settings rbac` uses the same composition.
- **Tests required**:
  - Upstream `TestPolicyCSV` ordering cases.
  - Overlay only (no `policy.csv`) works.

### TEN-078 argocd-rbac-cm live reload
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/rbac/rbac.go#L444-L548, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/rbac/rbac.go#L148-L156, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L261-L262
- **Maturity**: GA
- **Semantics**: The server reads `argocd-rbac-cm` at startup (missing ConfigMap tolerated) and then watches it with an informer (field selector by name, resync 10 minutes). Each change sets the default role, match mode, scopes, then the composed user policy; every change flushes the enforcer cache. `ARGOCD_RBAC_DEBUG=1` enables Casbin request logging.
- **Edge cases**: Startup errors (invalid policy or `scopes`) are fatal (server exits). On a live update, `SetUserPolicy` stores the new text in the adapter before loading it, so an invalid `policy.csv` is not rolled back: subsequent enforcer construction fails and RBAC checks fail closed (gRPC requests return Internal `error creating casbin enforcer: ...` via panic recovery) until the ConfigMap is fixed. If `scopes` is invalid on a live update, default role and match mode are already applied but the user policy is not updated. Updates with unchanged resourceVersion are ignored.
- **Configuration surface**: ConfigMap `argocd-rbac-cm`; env `ARGOCD_RBAC_DEBUG`.
- **Exposure**: ConfigMap.
- **Tests required**:
  - Policy change effective without restart.
  - Invalid policy at startup prevents server start.
  - Invalid policy pushed at runtime makes protected calls fail (no access granted) and recovers when fixed.
  - Missing ConfigMap at startup does not fail.

### TEN-079 Runtime project policy merge and enforcer caching
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/rbac/rbac.go#L158-L205, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/rbac/rbac.go#L378-L397, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/rbac/rbac_test.go#L405-L416
- **Maturity**: GA
- **Semantics**: Enforcers are cached per project name with the project policy string (1h TTL); a cached enforcer is reused only if the policy string is unchanged. Project policy is appended after built-in and user policy, so global denies override project allows. If the project policy fails to load, the enforcer falls back to built-in+user policy only (error `Failed to load project '<p>' policy`).
- **Edge cases**: A syntactically broken project policy (e.g. via kubectl) silently removes that project's role grants.
- **Configuration surface**: n/a.
- **Exposure**: API.
- **Tests required**:
  - Bad runtime policy: admin still allowed, readonly still denied writes (upstream `TestInvalidRuntimePolicy`).
  - Role policy edit reflected on next request.

### TEN-080 Policy validation and referential-integrity warning
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/rbac/rbac.go#L263-L283, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/rbac/rbac.go#L550-L562
- **Maturity**: GA
- **Semantics**: `ValidatePolicy` builds an enforcer from the text; failure → `policy syntax error: <policy>`. Then roles that have no implicit permissions are reported as a warning `user defined roles not found in policies: <roles>` (not an error).
- **Edge cases**: Validation passes with dangling `g` targets.
- **Configuration surface**: n/a.
- **Exposure**: API (project validation), CLI validate.
- **Tests required**:
  - Dangling role produces warning but valid result.
  - Syntax error message.

### TEN-081 Permission-denied errors and information hiding
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/rbac/rbac.go#L348-L376, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L186-L246, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/rbac/rbac_test.go#L439-L466
- **Maturity**: GA
- **Semantics**: `EnforceErr` returns gRPC PermissionDenied `permission denied: <res>, <act>, <obj>[, sub: <user-id>][, iat: <RFC3339>]`. Application access: if the request names a project and the first check fails, the app is still fetched (timing equalization) and PermissionDenied returned; if the app does not exist and project was given → NotFound; if the app exists in a different project than requested → NotFound; when no project given, missing or forbidden apps both return PermissionDenied.
- **Edge cases**: List endpoints silently filter instead of erroring.
- **Configuration surface**: n/a.
- **Exposure**: API.
- **Tests required**:
  - Exact error string with sub/iat for JWT callers.
  - Existence-hiding matrix (project given/not given × exists/not × allowed/not).

### TEN-082 Project-scoped repository and cluster RBAC objects
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/repository/repository.go#L87-L92, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/cluster/cluster.go#L45-L50, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/rbacpolicy/rbacpolicy.go#L132-L138, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/projects.md#L328-L345
- **Maturity**: GA
- **Semantics**: Repository/cluster RBAC object is `<project>/<url-or-server>` when the repo/cluster is project-scoped, else the bare URL/server. Project-scoped objects load that project's runtime policy, so project roles can grant `repositories|clusters, create|update|delete|get, <project>/*`.
- **Edge cases**: Project-role policy objects cannot contain URLs (TEN-018), so fine-grained per-URL grants must be in `argocd-rbac-cm`. Creating a scoped repo for project P requires `repositories, create, P/<url>`.
- **Configuration surface**: policy `obj`.
- **Exposure**: API (repository, repocreds, cluster services).
- **Tests required**:
  - Project role with `repositories, create, p/*` can add a scoped repo to p but not to q or unscoped.
  - Global policy `p/https://github.example.com/*` grants narrowly.

### TEN-083 write-repositories resource
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/rbac/rbac.go#L65-L65, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/repository/repository.go#L169-L169, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/repository/repository.go#L504-L504, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/assets/builtin-policy.csv#L14-L14
- **Maturity**: GA
- **Semantics**: Separate resource for write-credential repositories (used by the source hydrator/commit server): get/create/update/delete with the same object syntax as `repositories`. Built-in readonly has get; admin has create/update/delete.
- **Edge cases**: Not project-scoped in `ProjectScoped` (project roles cannot grant it) and not accepted by `admin settings rbac can --strict`.
- **Configuration surface**: policy `res = write-repositories`.
- **Exposure**: API (write repository endpoints).
- **Tests required**:
  - Readonly can list write repos; cannot create.
  - `repositories` grant does not imply `write-repositories`.

### TEN-084 logs, exec and extensions enforcement
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1894-L1894, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/terminal.go#L155-L155, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/extension/extension.go#L692-L692, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/rbac.md#L253-L281
- **Maturity**: GA
- **Semantics**: Pod logs require `logs, get, <app-rbac-name>` (always enforced in v3). Web terminal requires `exec, create, <app-rbac-name>` (checked on session start and on each websocket message). Proxy extensions require `extensions, invoke, <extension-name>` in addition to read access to the app.
- **Edge cases**: Extensions `invoke` is not in built-in roles. Exec permission is project-scoped (grantable from project roles).
- **Configuration surface**: policy.
- **Exposure**: API, websocket, HTTP extension proxy.
- **Tests required**:
  - Logs denied without `logs, get` even with `applications, get`.
  - Exec denied without `exec, create`.
  - Extension call denied without invoke.

### TEN-085 Disable authentication (--disable-auth)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-server/commands/argocd_server.go#L316-L316, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L353-L353, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L1541-L1543, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/session/sessionmanager.go#L545-L550
- **Maturity**: GA
- **Semantics**: `argocd-server --disable-auth` (env `ARGOCD_SERVER_DISABLE_AUTH`, cmd-params key `server.disable.auth`) skips authentication on gRPC/HTTP and disables RBAC enforcement (every check allowed). Settings API then returns banner/overrides as if logged in.
- **Edge cases**: Account endpoints that depend on identity behave as anonymous.
- **Configuration surface**: flag/env above.
- **Exposure**: server flag.
- **Tests required**:
  - Unauthenticated request succeeds for a write API when disabled.

### TEN-086 RBAC glob cache size
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/glob/glob.go#L12-L42, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-server/commands/argocd_server.go#L339-L339, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/server/argocd-server-deployment.yaml#L337-L341, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/high_availability.md#L262-L262
- **Maturity**: GA
- **Semantics**: Compiled glob patterns are cached in a bounded LRU (default 10000 entries); `--glob-cache-size` / `ARGOCD_SERVER_GLOB_CACHE_SIZE` / `argocd-cmd-params-cm` `server.glob.cache.size` (min 1).
- **Edge cases**: Performance-only; semantics unchanged.
- **Configuration surface**: flag/env/cmd-params key above.
- **Exposure**: server flag.
- **Tests required**:
  - Flag accepted; values <1 rejected/clamped per env parser.

### TEN-087 CLI: argocd admin settings rbac can
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/settings_rbac.go#L29-L231, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/settings_rbac.go#L303-L442, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/rbac.md#L443-L460
- **Maturity**: GA
- **Semantics**: `argocd admin settings rbac can ROLE/SUBJECT ACTION RESOURCE [SUB-RESOURCE]` with exactly one of `--policy-file` (CSV or ConfigMap YAML) or `--namespace` (reads live `argocd-rbac-cm`); `--default-role` (else `policy.default` from source), `--use-builtin-policy` (default true), `--strict` (default true; validates resource and action against a per-resource table; path actions allowed only for applications update/delete/action), `-q/--quiet`. Resource shorthands (`app`, `apps`, `proj`, `repo`, `cert`, `cluster`, `log`, `key`, `extension`, ...) resolve to canonical names. For project-scoped resources an empty or `*` sub-resource becomes `*/*`. Prints `Yes` (exit 0) or `No` (exit 1). Honours `policy.matchMode` from the source.
- **Edge cases**: Neither/both of `--policy-file` and `--namespace` → `please provide exactly one of --policy-file or --namespace`. Strict errors: `'<r>' is not a valid resource name`, `'<a>' is not a valid action for <r>`. Strict table allows accounts `create|update` only and omits `write-repositories`. Evaluates subjects only (no claims/groups/project runtime policy).
- **Configuration surface**: CLI flags + kubeconfig flags.
- **Exposure**: CLI.
- **Tests required**:
  - Yes/No exit codes with file and ConfigMap inputs.
  - Shorthand resolution; strict-mode rejections.
  - Default role from file honoured unless overridden.

### TEN-088 CLI: argocd admin settings rbac validate
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/settings_rbac.go#L233-L301
- **Maturity**: GA
- **Semantics**: `argocd admin settings rbac validate --policy-file FILE | --namespace NS` composes the user policy (CSV or ConfigMap, incl. overlays) and validates it: prints `Policy is valid.` (exit 0) or `Policy is invalid: <err>` (exit 1).
- **Edge cases**: Extra args → `too many arguments`; empty policy → fatal `Policy is empty or could not be loaded.`; missing/both flags → `please provide exactly one of --policy-file or --namespace`.
- **Configuration surface**: CLI.
- **Exposure**: CLI.
- **Tests required**:
  - Valid/invalid files; ConfigMap with overlays; empty policy.

### TEN-089 AccountService.CanI and argocd account can-i
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/account/account.go#L123-L152, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/account/account.proto#L78-L81, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/account.go#L210-L247
- **Maturity**: GA
- **Semantics**: `GET /api/v1/account/can-i/{resource}/{action}/{subresource=**}` evaluates the caller's claims (full pipeline incl. groups and project policies) and returns `{value: "yes"|"no"}`. Action must be one of the bare actions list and resource one of the resources list. For project-scoped resources with a 3-part subresource `p/ns/name`, the namespace is dropped when it equals the control-plane namespace.
- **Edge cases**: Unknown action/resource → InvalidArgument `[<list>] does not contain <x>`; path actions such as `update/*` or `action/...` are rejected by this validation. CLI argument order is `ACTION RESOURCE SUBRESOURCE` (exactly 3).
- **Configuration surface**: n/a.
- **Exposure**: API; CLI `argocd account can-i`; UI (used to gate UI controls).
- **Tests required**:
  - yes/no per policy; groups honoured.
  - Normalization of default namespace.
  - Invalid action error.

## G. Local accounts and sessions

### TEN-090 Local account definitions
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/accounts.go#L20-L59, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/accounts.go#L177-L313, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/index.md#L6-L63
- **Maturity**: GA
- **Semantics**: `argocd-cm` `accounts.<name>: <capabilities>` (comma-separated `login`, `apiKey`) defines an account; `accounts.<name>.enabled: "false"` disables (default enabled). `argocd-secret` stores `accounts.<name>.password` (bcrypt), `accounts.<name>.passwordMtime` (RFC3339), `accounts.<name>.tokens` (JSON `[{id, iat, exp}]`). Writes delete keys equal to defaults (`enabled: true`, empty password, `[]` tokens).
- **Edge cases**: Unknown capability values are ignored with a warning. Account names containing `.` cannot be expressed (keys with >3 parts ignored). Invalid `enabled` value fails account parsing for all accounts. Invalid tokens JSON for non-admin logs an error and yields empty tokens. Deleting a user requires removing the ConfigMap key (password secret key recommended).
- **Configuration surface**: `argocd-cm` `accounts.*`; `argocd-secret` `accounts.*`.
- **Exposure**: ConfigMap/Secret; API (AccountService); CLI (`argocd account`); UI (Settings → Accounts).
- **Tests required**:
  - Capabilities parsing incl. spaces and unknown values.
  - Disabled account cannot log in or use tokens.
  - Round-trip of password/mtime/tokens keys.

### TEN-091 Built-in admin account
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/accounts.go#L27-L34, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/accounts.go#L197-L224, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/assets/builtin-policy.csv#L53-L53, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/index.md#L65-L80
- **Maturity**: GA
- **Semantics**: Account `admin` always exists with capability `login`; `admin.enabled` in `argocd-cm` (default true), `admin.password`, `admin.passwordMtime`, `admin.tokens` in `argocd-secret`. Bound to `role:admin` by the built-in policy.
- **Edge cases**: Invalid `admin.enabled` value is ignored (warning) and admin stays enabled. Disabled admin cannot log in and its tokens are rejected.
- **Configuration surface**: `argocd-cm` `admin.enabled`; `argocd-secret` `admin.password|passwordMtime|tokens`.
- **Exposure**: ConfigMap/Secret; API; CLI; UI.
- **Tests required**:
  - `admin.enabled: "false"` blocks login.
  - Admin has no apiKey capability by default (token creation fails).

### TEN-092 Initial admin password
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L2397-L2438, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L529-L533, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/initial_password.go#L16-L43
- **Maturity**: GA
- **Semantics**: At API-server start, if admin is enabled and has no password hash, a 16-character random password from `[0-9A-Za-z-]` is generated, stored in Secret `argocd-initial-admin-secret` key `password` (created or updated), and its bcrypt hash plus mtime saved; if admin has a hash but no mtime, mtime is initialized. `argocd admin initial-password` prints the secret value and a recommendation to change it.
- **Edge cases**: Disabled admin → no password generated. Existing hash never regenerated (deleting the initial secret does not reset the password).
- **Configuration surface**: Secret `argocd-initial-admin-secret`.
- **Exposure**: Secret; CLI.
- **Tests required**:
  - Fresh install generates password and secret; login works with it.
  - Restart does not rotate.

### TEN-093 Server signature key and JWT signing
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L425-L425, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L1723-L1730, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L2447-L2455, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/session/sessionmanager.go#L232-L239
- **Maturity**: GA
- **Semantics**: `argocd-secret` `server.secretkey` (32 random bytes generated if missing) signs all Argo CD-issued JWTs with HS256 (`iss: argocd`). It also derives the encryption key for SSO state/cache encryption and the Dex client secret (TEN-114).
- **Edge cases**: Rotating the key invalidates all local/project/API tokens.
- **Configuration surface**: `argocd-secret` `server.secretkey`.
- **Exposure**: Secret.
- **Tests required**:
  - Missing key generated on start.
  - Key rotation invalidates existing tokens.

### TEN-094 Password hashing (bcrypt) and argocd account bcrypt
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/password/password.go#L29-L99, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/bcrypt.go#L13-L41
- **Maturity**: GA
- **Semantics**: Passwords are hashed with bcrypt at cost ≥ bcrypt.DefaultCost (10); verification uses bcrypt compare. Blank passwords are never valid (`blank passwords are not allowed` on hash; verify returns false). `argocd account bcrypt [--password]` (prompt or stdin) prints a bcrypt hash for manual secret editing.
- **Edge cases**: Externally provided hashes with any bcrypt cost verify. Non-bcrypt hashes never verify.
- **Configuration surface**: secret keys `admin.password`, `accounts.<n>.password`.
- **Exposure**: CLI.
- **Tests required**:
  - Hash produced by `account bcrypt` works when pasted into the secret.
  - Blank password rejected.

### TEN-095 Password pattern (passwordPattern)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L924-L934, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L144-L145, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/account/account.go#L84-L98, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/settings/settings.go#L136-L138
- **Maturity**: GA
- **Semantics**: `argocd-cm` `passwordPattern` (Go regex, default `^.{8,32}$`) must match new passwords on UpdatePassword. Exposed via the settings API only to logged-in users.
- **Edge cases**: Mismatch → `new password does not match the following expression: <pattern>`; invalid regex → `failed to compile password regex: ...`.
- **Configuration surface**: `argocd-cm` `passwordPattern`.
- **Exposure**: ConfigMap; API; UI (password form).
- **Tests required**:
  - Default pattern boundaries (7/8/32/33 chars).
  - Custom pattern enforced; invalid pattern error.

### TEN-096 AccountService.UpdatePassword
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/account/account.go#L41-L121, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L136-L137, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/account.go#L77-L155
- **Maturity**: GA
- **Semantics**: `PUT /api/v1/account/password` `{newPassword, currentPassword, name}`. Target = `name` or caller. If target ≠ caller, or caller is SSO, requires `accounts, update, <name>`. Local callers must supply their own current password (verified via the login path incl. rate limiting); SSO callers' tokens must be ≤5 minutes old. New password must match the pattern; stored bcrypt hash and `passwordMtime=now` (invalidates older tokens, TEN-102). CLI prompts for current password (local sessions) and new password with confirmation, then re-logs in to refresh the local context when changing its own password.
- **Edge cases**: Wrong current password → InvalidArgument `current password does not match`; project token → InvalidArgument `password can only be changed for local users, not user "<sub>"`; old SSO token → `SSO token is too old. Please use 'argocd relogin' to get a new token`; missing RBAC → `permission denied: ...`.
- **Configuration surface**: n/a.
- **Exposure**: API; CLI `argocd account update-password [--account] [--current-password] [--new-password]`; UI (User Info → Update Password).
- **Tests required**:
  - Self change; admin changes another user's password with RBAC; without RBAC denied.
  - SSO user with fresh vs stale token.
  - Old tokens rejected after change.

### TEN-097 Local login (SessionService.Create)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/session/session.go#L43-L92, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/session/sessionmanager.go#L464-L526, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/session/session.proto#L46-L52
- **Maturity**: GA
- **Semantics**: `POST /api/v1/session` `{username, password}` (no auth required). Verifies (serialized per username): blank password → Unauthenticated `Blank passwords are not allowed`; username >32 bytes → InvalidArgument `Username is too long (32 bytes max)`; lockout check (TEN-098); unknown account or bad password → Unauthenticated `Invalid username or password` (with a dummy hash for unknown users); disabled → `Account <u> is disabled`; no login capability → `Account <u> does not have login capability`. Response delays are padded to a random 500–1000 ms. On success returns a JWT `sub: <user>:login`, `jti` UUID, `exp = now + users.session.duration`; HTTP gateway also sets the `argocd.token` cookie. Login success/failure counter metric and log lines are emitted.
- **Edge cases**: Request with `token` field → Unauthenticated `token-based session creation no longer supported. please upgrade argocd cli to v0.7+`; empty username/password → `no credentials supplied`.
- **Configuration surface**: n/a.
- **Exposure**: API; CLI `argocd login`; UI login form.
- **Tests required**:
  - Each error path and gRPC code.
  - Token claims and cookie set.
  - Timing padding present (≥500 ms).

### TEN-098 Failed login rate limiting and lockout
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/session/sessionmanager.go#L82-L139, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/session/sessionmanager.go#L347-L462, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/index.md#L111-L128
- **Maturity**: GA
- **Semantics**: Per-username failure records `{failCount, lastFailed}` kept in memory per server replica. Once `failCount ≥ ARGOCD_SESSION_FAILURE_MAX_FAIL_COUNT` (default 5, min 1) and the last failure is within `ARGOCD_SESSION_FAILURE_WINDOW_SECONDS` (default 300; 0 = no window, permanent until success/restart), login is refused with `Invalid username or password` without checking the password (and without incrementing). Records older than the window are expired on the next update; a successful login removes the record. Cache size bounded by `ARGOCD_SESSION_MAX_CACHE_SIZE` (default 10000): on overflow one random entry other than `admin` and the current user is evicted.
- **Edge cases**: Lockout ends `window` seconds after the last failed attempt. Unknown usernames are tracked too. State is not shared across replicas.
- **Configuration surface**: env `ARGOCD_SESSION_FAILURE_MAX_FAIL_COUNT`, `ARGOCD_SESSION_FAILURE_WINDOW_SECONDS`, `ARGOCD_SESSION_MAX_CACHE_SIZE`.
- **Exposure**: API (login).
- **Tests required**:
  - 5 failures then correct password still rejected until window passes.
  - Window 0 behaviour.
  - Cache overflow never evicts admin.

### TEN-099 Concurrent login request limiting
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/session/ratelimiter.go#L11-L23, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L141-L180, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L1034-L1038
- **Maturity**: GA
- **Semantics**: A semaphore limits concurrent `SessionService.Create` calls to `ARGOCD_MAX_CONCURRENT_LOGIN_REQUESTS_COUNT` (default 50; 0 disables) divided by `ARGOCD_API_SERVER_REPLICAS` (default 1). Excess requests fail immediately with `Invalid username or password`.
- **Edge cases**: Integer division can yield 0 (= unlimited) when replicas exceed the count.
- **Configuration surface**: env `ARGOCD_MAX_CONCURRENT_LOGIN_REQUESTS_COUNT`, `ARGOCD_API_SERVER_REPLICAS`.
- **Exposure**: API.
- **Tests required**:
  - Burst above limit gets immediate failures.
  - Limit 0 disables.

### TEN-100 Session duration and local token auto-renewal
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L1662-L1669, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/session/sessionmanager.go#L329-L344, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L1549-L1556, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L1121-L1141
- **Maturity**: GA
- **Semantics**: `argocd-cm` `users.session.duration` (Argo CD duration syntax, default 24h) sets the `exp` of login tokens and the max OIDC session lifetime (TEN-125). When a login-capability token has <5 minutes remaining, the server issues a new token with the same total duration and returns it in gRPC header `renew-token`; the HTTP gateway turns it into a `Set-Cookie`.
- **Edge cases**: Unparsable duration → warning, 24h used. API-key tokens are never renewed.
- **Configuration surface**: `argocd-cm` `users.session.duration`.
- **Exposure**: ConfigMap; API headers/cookies.
- **Tests required**:
  - Custom duration reflected in exp.
  - Renewal header/cookie returned within the last 5 minutes only.

### TEN-101 Account API tokens (CreateToken / DeleteToken)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/account/account.go#L174-L282, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/account/account.proto#L101-L112, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/account.go#L376-L453
- **Maturity**: GA
- **Semantics**: `POST /api/v1/account/{name}/token` `{expiresIn, id}` and `DELETE /api/v1/account/{name}/token/{id}`. Caller may manage own tokens (local session for the same account) or needs `accounts, update, <name>`. Account must have `apiKey`. Token: `sub: <name>:apiKey`, `jti` = id or UUID, `exp` if expiresIn>0; metadata `{id, iat, exp}` appended to the account's tokens. Delete removes the metadata (revoking the token).
- **Edge cases**: Duplicate id → `account already has token with id '<id>'`; no capability → `account '<n>' does not have apiKey capability`; delete unknown → NotFound `token with id '<id>' does not exist`; errors are wrapped (`failed to update account with new token: ...`, `permission denied to create token for account <n>: ...`).
- **Configuration surface**: secret `accounts.<n>.tokens`.
- **Exposure**: API; CLI `argocd account generate-token [-a] [-e 0s] [--id]`, `argocd account delete-token [-a] ID` (confirm); UI (Account details → Generate New / Delete).
- **Tests required**:
  - Self-service vs RBAC paths.
  - Deleted token rejected.
  - Expiry honoured.

### TEN-102 Local token validation rules
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/session/sessionmanager.go#L241-L345
- **Maturity**: GA
- **Semantics**: For `iss: argocd` non-project tokens: subject `<name>[:login|:apiKey]` (missing suffix = apiKey); account must exist, be enabled (`account <n> is disabled`) and have the capability (`account <n> does not have '<cap>' capability`); `jti` required (`token does not have a unique identifier (jti claim) and cannot be validated`); jti not revoked (`token is revoked, please re-login`); apiKey tokens must be present in the account's token list (`account <n> does not have token with id <id>`); token `iat` must not precede `passwordMtime` (`account password has changed since token issued`). Claims are returned with `sub` rewritten to the bare account name.
- **Edge cases**: Login tokens are not required to be in any list; only Redis revocation/password change invalidate them before expiry.
- **Configuration surface**: n/a.
- **Exposure**: API authentication.
- **Tests required**:
  - Each rejection branch.
  - Legacy subject without suffix treated as apiKey.

### TEN-103 Account list and get
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/account/account.go#L154-L215, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/account/account.proto#L91-L99
- **Maturity**: GA
- **Semantics**: `GET /api/v1/account` returns accounts (name, enabled, capabilities, tokens sorted by iat desc) the caller may `accounts, get` (own account always visible to local sessions), sorted by name. `GET /api/v1/account/{name}` same rule, error if not permitted.
- **Edge cases**: SSO user with same name as a local account does not get implicit self-access (issuer must be argocd).
- **Configuration surface**: n/a.
- **Exposure**: API; CLI `argocd account list [-o]`, `argocd account get [-a] [-o]`; UI (Settings → Accounts).
- **Tests required**:
  - Readonly role sees all; plain user sees only self.

### TEN-104 Anonymous access (users.anonymous.enabled)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L501-L501, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L1563-L1573, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/rbac.md#L33-L41
- **Maturity**: GA
- **Semantics**: When `users.anonymous.enabled: "true"`, requests with no/invalid token proceed with claims `""`; RBAC then grants only `policy.default` (and any policy for the empty subject). The auth error is kept in context so `GetUserInfo.loggedIn` is false.
- **Edge cases**: Invalid tokens are also downgraded to anonymous instead of failing. Version API reveals details to anonymous users only when enabled.
- **Configuration surface**: `argocd-cm` `users.anonymous.enabled`.
- **Exposure**: ConfigMap; API.
- **Tests required**:
  - Anonymous read with `policy.default: role:readonly`; write denied.
  - Disabled → Unauthenticated `no session information`.

### TEN-105 Token transport (metadata, bearer header, cookie chunking)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L1642-L1673, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/http/http.go#L22-L46, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/http/http.go#L83-L127, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/http/http.go#L242-L252, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L129-L130
- **Maturity**: GA
- **Semantics**: Token lookup order: gRPC metadata `token`, then `Authorization: Bearer <jwt>` (must have 3 segments), then cookie `argocd.token` (possibly split into `argocd.token`, `argocd.token-1`, ... and re-joined). Cookies are set with `path=<basehref>`, `SameSite=lax`, `httpOnly`, `Secure` unless `--insecure`; values are chunked to ≤4093-byte cookies, max `ARGOCD_MAX_COOKIE_NUMBER` (default 20) chunks.
- **Edge cases**: Too large token → `the authentication token is N characters long and requires M cookies but the max number of cookies is K. Contact your Argo CD administrator to increase the max number of cookies`. Missing token → Unauthenticated `no session information`.
- **Configuration surface**: env `ARGOCD_MAX_COOKIE_NUMBER`.
- **Exposure**: API, HTTP.
- **Tests required**:
  - Precedence of metadata > bearer > cookie.
  - Large SSO token split and rejoined.

### TEN-106 SessionService.GetUserInfo
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/session/session.go#L99-L116, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/session/sessionmanager.go#L687-L740, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/session/session.proto#L26-L45
- **Maturity**: GA
- **Semantics**: `GET /api/v1/session/userinfo` (auth errors ignored) returns `{loggedIn, username, iss, groups}`: loggedIn iff a user identifier exists and no auth error; username = subject for `iss: argocd`, else `email` claim or user identifier; groups = values of the configured RBAC scopes.
- **Edge cases**: Anonymous → loggedIn false, empty username.
- **Configuration surface**: `scopes`.
- **Exposure**: API; CLI `argocd account get-user-info [-o yaml|json]`; UI (User Info page).
- **Tests required**:
  - Local, SSO (with/without email), anonymous responses.

### TEN-107 SessionService.Delete
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/session/session.go#L94-L97, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/session/session.proto#L54-L59
- **Maturity**: GA
- **Semantics**: `DELETE /api/v1/session` returns an empty token; over HTTP the gateway overwrites the auth cookie with an empty value. No server-side revocation (see TEN-108).
- **Edge cases**: Token remains valid if replayed.
- **Configuration surface**: n/a.
- **Exposure**: API.
- **Tests required**:
  - Cookie cleared; replayed token still accepted (parity).

### TEN-108 Logout endpoint and token revocation (/auth/logout)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/logout/logout.go#L20-L140, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L1182-L1182, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L380-L384, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/index.md#L444-L477
- **Maturity**: GA
- **Semantics**: `/auth/logout` reads the `argocd.token` cookie(s), clears all `argocd.token*` cookies (path = basehref), verifies the token; if valid and it has `exp` in the future, revokes its `jti` (or `at_hash` if no jti) in Redis with TTL = remaining lifetime (10 s timeout). Redirects (303) to the OIDC `logoutURL` (with `{{token}}` and `{{logoutRedirectURL}}` substituted) for SSO tokens when configured, else to the Argo CD base URL (+ root path).
- **Edge cases**: No cookie → 400 `Failed to retrieve ArgoCD auth token`. Invalid token → just redirect. Tokens without `exp` are not revoked. Entra ID tokens with `uti` but no `jti` are revoked under `at_hash`, while verification checks `uti` (revocation ineffective for them).
- **Configuration surface**: `oidc.config.logoutURL`.
- **Exposure**: HTTP; UI logout; CLI `argocd logout` (TEN-110).
- **Tests required**:
  - Local token revoked and rejected afterwards.
  - Redirect URL construction with/without logoutURL.
  - Missing cookie 400.

### TEN-109 Revoked token store
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/session/state.go#L15-L155, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/session/sessionmanager.go#L634-L666
- **Maturity**: GA
- **Semantics**: Revocations are written to Redis key `revoked-token|<id>` with TTL and published on channel `new-revoked-token`; every server keeps an in-memory set updated by the subscription and fully resynced by SCAN every 15 s (retry every minute on failure). Checked for local login/API tokens (jti) and IdP tokens (jti, else `uti`, else id-token access token hash).
- **Edge cases**: Redis outage → revocations from other replicas may be missed until resync; local revocation is immediate on the revoking replica.
- **Configuration surface**: Redis connection.
- **Exposure**: internal.
- **Tests required**:
  - Revocation on replica A rejected on replica B.
  - Expired revocation entries disappear.

### TEN-110 CLI: argocd login / logout / relogin
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/login.go#L39-L193, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/logout.go#L24-L138, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/relogin.go#L29-L107
- **Maturity**: GA
- **Semantics**: `argocd login SERVER [--username --password | --sso [--sso-port 8085] [--callback URL] [--sso-launch-browser=true]] [--name CTX] [--skip-test-tls]` (also `--core`, `--port-forward`): probes TLS (asks to proceed on plaintext/insecure), logs in, prints `'<user>' logged in successfully` and stores server/user/context in the local config. `argocd logout CONTEXT` (confirm) POSTs to `/auth/logout` with the token cookie (warns on failure), removes the token locally, prints `Logged out from '<ctx>'`. `argocd relogin [--password] [--sso-port] [--callback] [--sso-launch-browser]` re-authenticates the current context (password flow for `iss: argocd`, else SSO).
- **Edge cases**: Logout with no config → `Nothing to logout from`; unknown context → `Context <c> does not exist`; relogin without config → `No context found. Login using `argocd login``.
- **Configuration surface**: CLI config file.
- **Exposure**: CLI.
- **Tests required**:
  - Password and SSO login write the expected context.
  - Logout revokes server-side and removes local token even if server unreachable.

### TEN-111 CLI: argocd account command group
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/account.go#L38-L540, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/commands/argocd_account.md#L1-L1
- **Maturity**: GA
- **Semantics**: Subcommands: `bcrypt`, `can-i`, `delete-token`, `generate-token`, `get`, `get-user-info`, `list`, `session-token [-o json]`, `update-password`. `session-token` refreshes SSO tokens via the stored refresh token, validates the token locally and prints it (json: type local|sso, issuer, username, token, has_refresh_token, issued_at, expires_at).
- **Edge cases**: session-token errors: `Refresh token is invalid or expired. Please run 'argocd relogin' to re-authenticate`, `No configuration found...`, `No context found...`, `No authentication token found...`, `Invalid token format. Please run 'argocd relogin'`, `Token is invalid or expired. Please run 'argocd relogin'`.
- **Configuration surface**: CLI.
- **Exposure**: CLI.
- **Tests required**:
  - Each subcommand against the API contracts (TEN-089, TEN-094, TEN-096, TEN-101, TEN-103, TEN-106).
  - session-token json shape.

### TEN-112 UI: login, user info and accounts
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/login/components/login.tsx#L22-L90, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/user-info/components/user-info-overview/user-info-overview.tsx#L93-L110, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/settings/components/accounts-list/accounts-list.tsx#L11-L11, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/settings/components/account-details/account-details.tsx#L21-L137, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/settings/settings.go#L67-L77
- **Maturity**: GA
- **Semantics**: Login page: username/password form (hidden when no enabled account has `login`, via `userLoginsDisabled`), "SSO Login" button linking to `auth/login?return_url=...` when SSO configured, SSO error banner when `has_sso_error=true`. User Info page: username, issuer, groups, Update Password (local users). Settings → Accounts: list, account details with token generation (Token ID, Expires In) and deletion.
- **Edge cases**: Dex 500 errors redirect to `/login?has_sso_error=true` (TEN-115).
- **Configuration surface**: n/a.
- **Exposure**: UI.
- **Tests required**:
  - UI flows mapped to API calls; userLoginsDisabled hides form.

## H. Single sign-on

### TEN-113 SSO enablement and server restart on configuration change
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L1915-L1936, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L1366-L1376, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L823-L843
- **Maturity**: GA
- **Semantics**: SSO is configured if Dex is configured (`url` non-empty AND `dex.config` parses to a non-empty map) or `oidc.config` parses. When configured, the server registers `/api/dex/` (proxy), `/auth/login`, `/auth/callback`. The server performs a graceful restart when the generated Dex config, OIDC config, `url` or `additionalUrls` change (also webhook secrets).
- **Edge cases**: `dex.config` without `url` → SSO disabled. Invalid YAML → warning, treated as unconfigured. If both are set, `oidc.config` takes precedence for issuer/client (Dex still generated).
- **Configuration surface**: `argocd-cm` `url`, `additionalUrls`, `dex.config`, `oidc.config`.
- **Exposure**: ConfigMap; HTTP routes.
- **Tests required**:
  - Routes absent without SSO.
  - Config change triggers restart.

### TEN-114 Dex configuration generation
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/dex/config.go#L16-L164, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L2337-L2353, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/index.md#L143-L221
- **Maturity**: GA
- **Semantics**: From `dex.config`: sets `issuer = <url>/api/dex`, `storage: memory`, `web` (http or https on 0.0.0.0:5556 with /tmp TLS), `grpc 0.0.0.0:5557`, `telemetry 0.0.0.0:5558`, logger level/format from env if absent, `oauth2.skipApprovalScreen: true` if absent; prepends static clients `argo-cd` (secret = first 40 chars of base64url(sha256(server.secretkey)), redirect URIs `<url>/auth/callback` + additional URLs), `argo-cd-cli` (public, `http://localhost`, `http://localhost:8085/auth/callback`), `argo-cd-pkce` (public, `http://localhost:4000/auth/callback`); injects `redirectURI: <url>/api/dex/callback` into connectors of types oidc, saml, microsoft, linkedin, gitlab, github, bitbucket-cloud, openshift, gitea, google, oauth; resolves `$secret` references and escapes `$` in resolved values.
- **Edge cases**: `connectors` missing/not a list or connector without map `config` (for those types) → `malformed Dex configuration found`. User-defined `staticClients` are appended after the built-ins.
- **Configuration surface**: `argocd-cm` `dex.config`; env `ARGOCD_LOG_LEVEL`/`ARGOCD_LOG_FORMAT`.
- **Exposure**: `argocd-dex rundex` / `gendexcfg` (Dex deployment).
- **Tests required**:
  - Golden-file generation from sample configs.
  - Malformed configs error.

### TEN-115 Dex reverse proxy and TLS
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/dex/dex.go#L22-L133, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-server/commands/argocd_server.go#L315-L335
- **Maturity**: GA
- **Semantics**: `/api/dex/*` is reverse-proxied to the Dex server (`--dex-server`, default `argocd-dex-server:5556`); `--dex-server-plaintext` disables TLS; `--dex-server-strict-tls` verifies Dex's certificate against the configured CA and pins it, else TLS verification is skipped. Dex 500 responses are converted to a 303 redirect to `<basehref>/login?has_sso_error=true` (error body logged). Server-side calls to the issuer are rewritten to the internal Dex address.
- **Edge cases**: Certificate mismatch → `dex server certificate does not match`.
- **Configuration surface**: flags `--dex-server`, `--dex-server-plaintext`, `--dex-server-strict-tls` (env `ARGOCD_SERVER_DEX_SERVER*`).
- **Exposure**: HTTP.
- **Tests required**:
  - Proxy forwards discovery/auth endpoints.
  - 500 → redirect conversion.

### TEN-116 OIDC core configuration
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L197-L246, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L1949-L2044, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L2074-L2082, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L2129-L2137, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L2170-L2178, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L2261-L2286, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/index.md#L315-L369
- **Maturity**: GA
- **Semantics**: `oidc.config` YAML keys: `name`, `issuer`, `clientID`, `clientSecret`, `cliClientID`, `requestedScopes`, `requestedIDTokenClaims`, `logoutURL`, `rootCA` (PEM), `enablePKCEAuthentication`, `domainHint`, `enableUserInfoGroups`, `userInfoBaseURL`, `userInfoPath`, `userInfoCacheExpiration`, `refreshTokenThreshold`, `azure.{useWorkloadIdentity, enableUserGroupOverageClaim, graphApiEndpoint, userGroupOverageClaimCacheExpiration}`, `allowedAudiences`, `skipAudienceCheckWhenTokenHasNoAudience`. String values starting with `$` are resolved from secrets (TEN-131). `issuer` and `userInfoBaseURL` must be http(s) URLs (warning only). TLS to the IdP uses `rootCA` if parseable (else system roots with warning); `oidc.tls.insecure.skip.verify: "true"` disables verification (also for Dex). Redirect URI is `<url>/auth/callback` (or matching additional URL).
- **Edge cases**: Unparsable `oidc.config` → treated as not configured (warning). When Dex is used, client ID is `argo-cd` and secret derived from the server key.
- **Configuration surface**: `argocd-cm` `oidc.config`, `oidc.tls.insecure.skip.verify`, `url`, `additionalUrls`.
- **Exposure**: ConfigMap.
- **Tests required**:
  - Each key parsed; secret reference resolved.
  - rootCA and insecure-skip-verify behaviour against a self-signed IdP.

### TEN-117 OIDC requestedScopes
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/oidc/oidc.go#L196-L201, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/oidc/oidc.go#L1114-L1123, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L182-L183
- **Maturity**: GA
- **Semantics**: Scopes requested by the server login flow: `requestedScopes` or default `[openid, profile, email, groups]`; with Dex: default scopes plus `federated:id`. CLI adds `offline_access` when the provider advertises it or advertises no scopes.
- **Edge cases**: Custom list replaces (not extends) defaults.
- **Configuration surface**: `oidc.config.requestedScopes`.
- **Exposure**: ConfigMap; auth URL.
- **Tests required**:
  - Default vs custom scope in authorization URL; Dex adds `federated:id`.

### TEN-118 OIDC requestedIDTokenClaims
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/oidc/oidc.go#L901-L923, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/index.md#L371-L401
- **Maturity**: GA
- **Semantics**: Map of claim → `{essential, value, values}` sent as the OIDC `claims` request parameter `{"id_token": {...}}` on web and CLI authorization requests.
- **Edge cases**: Marshal failure logs error and omits the parameter.
- **Configuration surface**: `oidc.config.requestedIDTokenClaims`.
- **Exposure**: ConfigMap; settings API (`idTokenClaims`).
- **Tests required**:
  - Authorization URL contains exact JSON.

### TEN-119 OIDC audience validation (allowedAudiences, skipAudienceCheckWhenTokenHasNoAudience)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/oidc/provider.go#L92-L161, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L2139-L2168, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/index.md#L330-L341
- **Maturity**: GA
- **Semantics**: Tokens with an `aud` claim must verify for at least one allowed audience: `allowedAudiences` if non-empty, else `[clientID, cliClientID?]`; Dex: `[argo-cd, argo-cd-cli]`. Tokens without `aud` are verified with client-ID check skipped only if `skipAudienceCheckWhenTokenHasNoAudience: true` (default false). Expired tokens short-circuit.
- **Edge cases**: aud present but no allowed audiences → `token has an audience claim, but no allowed audiences are configured`. Multiple failures → `token verification failed for all audiences: ...`.
- **Configuration surface**: `oidc.config.allowedAudiences`, `oidc.config.skipAudienceCheckWhenTokenHasNoAudience`.
- **Exposure**: ConfigMap.
- **Tests required**:
  - Token for cliClientID accepted by default; foreign audience rejected.
  - aud-less token rejected by default, accepted with skip flag.

### TEN-120 OIDC enablePKCEAuthentication (server web flow)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L2180-L2185, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/oidc/oidc.go#L456-L459, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/oidc/oidc.go#L575-L577
- **Maturity**: GA
- **Semantics**: When true, the server's web login generates a PKCE verifier (stored encrypted in the state cookie) and sends an S256 challenge; the verifier is supplied at code exchange. Default false. Exposed to clients via settings API.
- **Edge cases**: Not applicable to implicit flow. Dex mode never enables it (Dex client is confidential).
- **Configuration surface**: `oidc.config.enablePKCEAuthentication`.
- **Exposure**: ConfigMap; settings API.
- **Tests required**:
  - Authorization URL has `code_challenge_method=S256` only when enabled; exchange includes verifier.

### TEN-121 OIDC cliClientID
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apiclient/apiclient.go#L321-L358, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/index.md#L349-L353
- **Maturity**: GA
- **Semantics**: The CLI uses `cliClientID` (if set) instead of `clientID` for its local-callback flow; it is also an implicitly allowed audience. With Dex the CLI uses `argo-cd-cli`.
- **Edge cases**: If unset, CLI uses `clientID` (needs `http://localhost:8085/auth/callback` registered).
- **Configuration surface**: `oidc.config.cliClientID`.
- **Exposure**: ConfigMap; settings API (`cliClientID`); CLI.
- **Tests required**:
  - CLI authorization URL uses cliClientID; resulting token accepted by server.

### TEN-122 UserInfo group retrieval
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/oidc/oidc.go#L925-L1103, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L2062-L2110, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/index.md#L403-L415
- **Maturity**: GA
- **Semantics**: When `enableUserInfoGroups: true` and `userInfoPath` set, for every IdP-issued token the server calls `<userInfoBaseURL or issuer><userInfoPath>` with the user's cached access token (stored encrypted at login), validates JWT responses (`application/jwt`) or parses JSON, requires matching `sub`, and replaces the `groups` claim. Responses are cached encrypted per sub for min(userInfoCacheExpiration, token expiry) (token expiry if setting is 0/longer). Cache entries are invalidated at each new login.
- **Edge cases**: No cached access token or 401 from userinfo → session invalid (`error while querying userinfo endpoint: ...` → Unauthenticated). Sub mismatch → error with OIDC spec reference. HTTP middleware returns 401 `Invalid session`.
- **Configuration surface**: `oidc.config.enableUserInfoGroups`, `userInfoPath`, `userInfoBaseURL`, `userInfoCacheExpiration`.
- **Exposure**: ConfigMap.
- **Tests required**:
  - Groups from userinfo used for RBAC.
  - Cache expiry rules; sub mismatch rejected; 401 invalidates session.

### TEN-123 OIDC logoutURL
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/logout/logout.go#L39-L47, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/logout/logout.go#L133-L139, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/index.md#L417-L442
- **Maturity**: GA
- **Semantics**: Template with placeholders `{{token}}` (raw ID token) and `{{logoutRedirectURL}}` (Argo CD URL for the request, trailing slash removed). Used only for non-local tokens on `/auth/logout`.
- **Edge cases**: Placeholders replaced literally (no URL encoding).
- **Configuration surface**: `oidc.config.logoutURL`.
- **Exposure**: ConfigMap; HTTP.
- **Tests required**:
  - Redirect contains substituted values.

### TEN-124 OIDC domainHint
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/oidc/oidc.go#L203-L209, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/oidc/oidc.go#L453-L455, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/login.go#L319-L333
- **Maturity**: GA
- **Semantics**: Trimmed `domainHint` is added as `domain_hint` to the server's web authorization URL.
- **Edge cases**: The CLI SSO flow never sends it (its check reads a zero-value struct).
- **Configuration surface**: `oidc.config.domainHint`.
- **Exposure**: ConfigMap.
- **Tests required**:
  - Web auth URL contains `domain_hint`; CLI URL does not (parity).

### TEN-125 OIDC refreshTokenThreshold and server-side refresh
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/oidc/oidc.go#L120-L194, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/oidc/oidc.go#L718-L818, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L1595-L1637, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/index.md#L252-L255
- **Maturity**: GA
- **Semantics**: At web login the full OAuth2 token (incl. refresh token) is cached encrypted under `oidc_token_<sub>_<sid>` with TTL bounded by `users.session.duration` from session start. On each request with an external `oidc.config` (or Dex), if the ID token expires within `refreshTokenThreshold` (default 0 = only when expired), the server refreshes via the cached token and returns the new ID token as `renew-token`/cookie; expired ID tokens are recovered the same way. Sessions older than `users.session.duration` are not refreshed (forcing re-login).
- **Edge cases**: Doc says Dex embedded login is not refreshed (see Doc differences). Unparsable threshold → warning, 0.
- **Configuration surface**: `oidc.config.refreshTokenThreshold`; `users.session.duration`.
- **Exposure**: API.
- **Tests required**:
  - Token near expiry is renewed; past session duration not renewed.

### TEN-126 Azure-specific OIDC options
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L2187-L2259, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/oidc/oidc_azure.go#L36-L222, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/oidc/oidc.go#L486-L573
- **Maturity**: GA
- **Semantics**: `azure.useWorkloadIdentity`: code exchange uses `client_assertion` (JWT bearer) read from `AZURE_FEDERATED_TOKEN_FILE` (cached 10 min) instead of a client secret. `azure.enableUserGroupOverageClaim`: when the ID token has `_claim_names`/`_claim_sources` (200+ groups) and UserInfo groups are not enabled, groups are fetched via Graph `POST /me/getMemberGroups` (`securityEnabledOnly: true`) with the cached access token (requires `User.Read` in `scp`), cached for min(setting, token expiry). `graphApiEndpoint` must be https and one of graph.microsoft.com/.us/.de/microsoftgraph.chinacloudapi.cn (default `https://graph.microsoft.com/v1.0`). Revocation id falls back to `uti`.
- **Edge cases**: Missing env/file → `AZURE_FEDERATED_TOKEN_FILE env variable not found, ...` / `... specified file does not exist`; missing scope → `access token missing User.Read scope; ...`; invalid endpoint disables overage resolution (warning).
- **Configuration surface**: `oidc.config.azure.*`; env `AZURE_FEDERATED_TOKEN_FILE`.
- **Exposure**: ConfigMap.
- **Tests required**:
  - Overage token resolves groups via mocked Graph.
  - Invalid endpoint rejected.
  - Workload identity assertion sent.

### TEN-127 Web SSO login and callback flow
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/oidc/oidc.go#L306-L484, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/oidc/oidc.go#L524-L656, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/oidc/oidc.go#L838-L899, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L131-L134
- **Maturity**: GA
- **Semantics**: `GET /auth/login?return_url=...`: validates return_url (scheme+host must equal `url` or an additional URL and path within it; CRLF rejected), creates a 24-char random state stored with return URL and PKCE verifier in an encrypted `argocd.oauthstate` cookie (5 min, HttpOnly, SameSite=Lax, Secure if https), redirects (303) to the IdP using authorization-code flow if the provider supports response type `code`, else implicit (`response_type=id_token` + nonce). `GET /auth/callback`: handles IdP `error`, verifies state cookie, exchanges code, verifies ID token, clears cached userinfo/overage groups, caches access token and OIDC token, sets the `argocd.token` cookie(s) to the ID token and redirects to return URL. Redirect URI is chosen per request host among `url`/`additionalUrls`. `ARGOCD_SSO_DEBUG=1` renders token details instead of redirecting.
- **Edge cases**: Invalid return URL → 400 `Invalid redirect URL: the protocol and host (including port) must match and the path must be within allowed URLs if provided`; state mismatch → 400 `invalid state in 'argocd.token' cookie`; missing id_token → 500 `no id_token in token response`; verification failure → 500 generic token verification error.
- **Configuration surface**: `url`, `additionalUrls`; env `ARGOCD_SSO_DEBUG`.
- **Exposure**: HTTP; UI.
- **Tests required**:
  - Full code flow with a mock IdP incl. PKCE variant.
  - Open-redirect attempts rejected.
  - Implicit flow when provider lacks `code`.

### TEN-128 IdP token verification in the API
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/session/sessionmanager.go#L583-L681, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/oidc/provider.go#L163-L205
- **Maturity**: GA
- **Semantics**: Tokens whose `iss` ≠ `argocd` are verified against the configured issuer's JWKS (lazy discovery, audience rules TEN-119); on `failed to verify signature` the provider is re-discovered once (Dex key rotation). Expired tokens return claims `{iss: sso}` with `ErrTokenVerification` so the UI can redirect to login (and server attempts refresh, TEN-125). Revocation is checked by jti/uti/access-token-hash. Without SSO configured → `SSO is not configured`.
- **Edge cases**: Verification failure → Unauthenticated `invalid session: ...`.
- **Configuration surface**: SSO settings.
- **Exposure**: API.
- **Tests required**:
  - Valid IdP token accepted; wrong issuer/aud/signature rejected.
  - Key rotation recovery.
  - Revoked IdP token rejected.

### TEN-129 User identifier and group claim mapping
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/jwt/jwt.go#L142-L166, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/session/sessionmanager.go#L691-L707, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/rbac.md#L47-L54
- **Maturity**: GA
- **Semantics**: RBAC subject = `federated_claims.user_id` (Dex) if present and non-empty, else `sub`. Display username = subject for local tokens, else `email` claim if present, else the identifier. Groups = values of configured `scopes` claims (TEN-074).
- **Edge cases**: Non-map `federated_claims` ignored. A user whose identifier equals a local account name or role subject receives those bindings.
- **Configuration surface**: `scopes`.
- **Exposure**: API.
- **Tests required**:
  - Dex token with federated_claims maps to user_id.
  - Email displayed but not used as subject.

### TEN-130 CLI SSO login with PKCE and token refresh
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/login.go#L205-L361, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apiclient/apiclient.go#L394-L436
- **Maturity**: GA
- **Semantics**: CLI starts a local HTTP server on `localhost:<sso-port>` (default 8085; `--callback` overrides base), redirect `<base>/auth/callback`, always uses PKCE S256 (43-char verifier) for code flow, `access_type=offline`, requested claims, opens browser unless disabled; supports implicit flow via a JS fragment bounce (max 2 callback requests). Stores ID token and refresh token. Before API calls, if the stored token is expired and a refresh token exists, the CLI redeems it and persists the new tokens.
- **Edge cases**: Errors: `Unknown state nonce`, `no code in request: ...`, `no id_token in token response`, `Unable to complete login flow: too many redirects`, `<server> is not configured with SSO`.
- **Configuration surface**: CLI flags; `cliClientID`.
- **Exposure**: CLI.
- **Tests required**:
  - Code+PKCE flow against mock IdP.
  - Automatic refresh with expired token.

### TEN-131 Secret references in SSO configuration
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L1745-L1754, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L2488-L2541, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/index.md#L498-L596
- **Maturity**: GA
- **Semantics**: Any string value in `oidc.config` / `dex.config` starting with `$` is replaced: `$<key>` → `argocd-secret` key; `$<secret-name>:<key>` → key of another Secret labelled `app.kubernetes.io/part-of: argocd` in the control-plane namespace. Resolved values are whitespace-trimmed.
- **Edge cases**: Unknown reference → value left as-is (warning `secret key does not exist in secret`). In Dex connector configs, `$` inside resolved values is escaped to survive Dex env expansion.
- **Configuration surface**: `argocd-secret`, labelled Secrets.
- **Exposure**: ConfigMap/Secret.
- **Tests required**:
  - Both reference syntaxes; unlabelled secret not resolved.
  - Secret value containing `$` passes intact to Dex.

### TEN-132 Public settings exposure of auth configuration
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/settings/settings.go#L41-L160, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/settings/settings.go#L193-L200
- **Maturity**: GA
- **Semantics**: `GET /api/v1/settings` (works unauthenticated) exposes `url`, `additionalUrls`, `userLoginsDisabled` (true iff no enabled account has `login`), `dexConfig.connectors` (parsed), `oidcConfig {name, issuer, clientID, cliClientID, scopes, enablePKCEAuthentication, idTokenClaims}` — never secrets. `passwordPattern` only for logged-in users.
- **Edge cases**: Dex config parse failure → field omitted.
- **Configuration surface**: n/a.
- **Exposure**: API; UI; CLI (SSO bootstrap).
- **Tests required**:
  - Unauthenticated response contains no clientSecret.
  - userLoginsDisabled toggles with admin.enabled and accounts.

### TEN-133 HTTP authentication middleware for non-gRPC endpoints
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/session/sessionmanager.go#L528-L581, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L1226-L1226, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L1305-L1310
- **Maturity**: GA
- **Semantics**: Web terminal and proxy-extension HTTP endpoints authenticate from the `argocd.token` cookie only, verify the token, apply UserInfo/Azure group resolution when SSO is configured, and put claims in the request context for RBAC.
- **Edge cases**: No cookie → 400 `Auth cookie not found`; invalid token → 401 `Invalid token`; group resolution failure → 401 `Invalid session`. Bypassed with `--disable-auth`.
- **Configuration surface**: n/a.
- **Exposure**: HTTP.
- **Tests required**:
  - Status codes for each failure; valid cookie passes claims.

## I. IdP integration recipes and CI workload identity

The features in this section cover the documented identity-provider recipes (`docs/operator-manual/user-management/*.md`)
and the CI workload-identity login flow. Each recipe combines mechanisms already specified above (TEN-114..TEN-133);
the sections below pin down the exact configuration shape each recipe uses, the Argo CD-side behaviour it relies on,
and the per-IdP group-claim mapping that Patrol must reproduce. Behaviour that is implemented inside the bundled Dex
binary (`ghcr.io/dexidp/dex:v2.45.1`, not a Go dependency of Argo CD) is described as the recipe documents it and
listed under Unconfirmed where it could not be verified from the checkout.

### TEN-134 CI workload-identity login via Dex token exchange (end-to-end)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/dex/config.go#L94-L109, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L1366-L1376, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/dex/dex.go#L54-L99, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L2074-L2082, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L2142-L2157, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/session/sessionmanager.go#L585-L654, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L1579-L1637, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/jwt/jwt.go#L142-L166, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apiclient/apiclient.go#L229-L233, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apiclient/apiclient.go#L308-L313, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apiclient/apiclient.go#L394-L436, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/root.go#L81, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/dex/argocd-dex-server-deployment.yaml#L38-L42, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/github-actions.md#L8-L30, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/github-actions.md#L76-L118, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/gitlab-ci.md#L44-L65
- **Maturity**: GA
- **Semantics**: Argo CD contains no token-exchange code of its own; the flow is assembled from the bundled Dex and existing Argo CD pieces:
  1. The Dex config Argo CD generates always contains the public static client `argo-cd-cli` (no secret, redirect URIs `http://localhost`, `http://localhost:8085/auth/callback`), prepended before any user `staticClients` (TEN-114).
  2. The CI job obtains an OIDC ID token from its platform, then sends `POST <url>/api/dex/token` (`application/x-www-form-urlencoded`) with HTTP Basic `argo-cd-cli:` (empty password) and form fields `grant_type=urn:ietf:params:oauth:grant-type:token-exchange`, `connector_id=<dex connector id>`, `scope=openid email profile federated:id`, `requested_token_type=urn:ietf:params:oauth:token-type:access_token`, `subject_token=<CI token>`, `subject_token_type=urn:ietf:params:oauth:token-type:id_token`.
  3. The API server forwards everything under `/api/dex/` to Dex unchanged (path, method, body, headers; `Host` rewritten to the Dex address). This route exists whenever SSO is configured (TEN-113).
  4. Dex validates the subject token against the named connector's issuer and returns JSON whose `access_token` is a Dex-signed JWT (`iss = <url>/api/dex`, `aud = argo-cd-cli`).
  5. The caller passes that token as `ARGOCD_AUTH_TOKEN` (or `--auth-token`, whitespace-trimmed). The CLI sends it in the gRPC `token` metadata; REST clients use `Authorization: Bearer` (TEN-105).
  6. Because `iss` is not `argocd`, the API server verifies the token against the issuer's JWKS (`IssuerURL()` = `<url>/api/dex` in Dex mode) with allowed audiences fixed to `[argo-cd, argo-cd-cli]` (TEN-119, TEN-128).
  7. The RBAC subject is `federated_claims.user_id` (Dex sets it to the upstream CI token's `sub` when `federated:id` is requested), falling back to `sub` (TEN-129). Groups exist only if the connector supplies them and the `groups` scope was requested. The display username is the `email` claim if present, else the subject.

  No refresh token is involved, so CI jobs exchange a new token for every run.
- **Edge cases**:
  - If `oidc.config` is also set, `IssuerURL()` returns the external OIDC issuer, so Dex-issued tokens fail verification. The result is Unauthenticated `invalid session: failed to verify the token`.
  - Exchanging through a user-defined Dex static client (e.g. `id: ci`) gives `aud=ci`. That audience is rejected because, in Dex mode, the allowed audiences are hard-coded to `argo-cd`/`argo-cd-cli`. `allowedAudiences` only applies to `oidc.config`.
  - Omitting `federated:id` from `scope` produces no `federated_claims`, so the subject becomes Dex's opaque `sub` (an encoding of the user ID and connector ID). Policies written against the CI `sub` then silently fail to match.
  - A Dex HTTP 500 on `/api/dex/token` is rewritten by the proxy into `303 See Other` with an empty body and `Location: <basehref>/login?has_sso_error=true` (TEN-115). `curl -f` treats 303 as success, so scripts must check the JSON body. Dex 4xx errors (unknown connector, invalid subject token) pass through unchanged.
  - An expired exchanged token yields Unauthenticated `invalid session: failed to verify the token`. Server-side refresh only runs when `oidc.config` is set and a refresh token was cached at web login, so exchanged tokens are never refreshed.
  - Revocation lookup uses `jti`, then `uti`, then the token's `at_hash`. Dex tokens carry no `jti` (see Unconfirmed for `at_hash` on exchanged access tokens).
  - CLI precedence: the env/flag token replaces the config-file token. However, if a local config exists and its current context holds a refresh token while the stored token is expired, `NewClient` redeems that refresh token and overwrites the env-supplied token. CI must run without a stale config file.
  - If no SSO is configured, `/api/dex/` is not registered and the exchange cannot reach Dex.
- **Configuration surface**: `argocd-cm` `url`, `dex.config.connectors[]` (`type: oidc`, `id`, `name`, `config.issuer`, `config.scopes`, `config.userNameKey`, `config.insecureSkipEmailVerified`); `argocd-rbac-cm` `policy.csv`, `policy.default`, `scopes`; CLI env `ARGOCD_AUTH_TOKEN`, `ARGOCD_SERVER`, `ARGOCD_OPTS` (e.g. `--grpc-web`); flag `--auth-token`.
- **Exposure**: HTTP `POST /api/dex/token` (reverse-proxied to Dex); API (every RPC with the bearer token, e.g. `GET /api/v1/session/userinfo`, `GET /api/v1/applications`); CLI (`argocd account get-user-info`, `argocd app list`, `argocd proj list` with `ARGOCD_AUTH_TOKEN`).
- **Tests required**:
  - Positive (E2E): a mock CI OIDC issuer, a Dex `oidc` connector for it, the exchange at `/api/dex/token` with `argo-cd-cli:`, then `argocd account get-user-info` reports the CI `sub` as username and `argocd app list` returns only the apps the policy grants that subject.
  - Positive: the token is accepted over gRPC, gRPC-web (`--grpc-web`) and REST (`Authorization: Bearer`).
  - Negative: a wrong `connector_id`, a subject token from another issuer, or an expired subject token returns a Dex 4xx through the proxy unchanged.
  - Negative: a token minted for a custom static client is rejected by the API (audience).
  - Negative: with `oidc.config` and `dex.config` both set, the Dex-exchanged token is rejected.
  - Negative: an exchange without `federated:id` yields an opaque subject, and the CI-sub policy is not applied (deny / `policy.default` only).
  - Authorization: policy lines keyed on the exact CI subject grant only the listed actions; another repository's or branch's subject is denied.
  - Recovery: a Dex 500 becomes a 303 to `/login?has_sso_error=true`; an expired exchanged token becomes Unauthenticated, and a fresh exchange works without a server restart.
  - Regression: with a stale CLI config containing a refresh token, the env token is overridden (parity).

### TEN-135 GitHub Actions OIDC login recipe
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/github-actions.md#L11-L30, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/github-actions.md#L34-L119, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/github-actions.md#L122-L140, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/jwt/jwt.go#L142-L166, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/assets/model.conf#L13-L14
- **Maturity**: GA
- **Semantics**: Dex connector `type: oidc`, `id: github-actions`, `config.issuer: https://token.actions.githubusercontent.com/` (GitHub Enterprise Server: `https://<host>/_services/token`), `scopes: [openid]`, `userNameKey: sub`, `insecureSkipEmailVerified: true`.
  - The workflow needs `permissions: id-token: write`. It requests the token from `ACTIONS_ID_TOKEN_REQUEST_URL` with `Authorization: bearer $ACTIONS_RUNTIME_TOKEN` and reads `.value`.
  - It then runs the TEN-134 exchange with `connector_id=github-actions` and uses `.access_token` as `ARGOCD_AUTH_TOKEN` with `ARGOCD_SERVER` and `ARGOCD_OPTS=--grpc-web`.
  - The RBAC subject is the GitHub token `sub`, e.g. `repo:my-org/my-repo:pull_request`. Example policy: `p, repo:my-org/my-repo:pull_request, applications, get, my-project/*, allow`.
  - The recipe relies on the v3 subject mapping (`federated_claims.user_id`); the guide notes it is unsupported on v2.
- **Edge cases**:
  - Subjects are matched exactly (Casbin `g(r.sub, p.sub)`, TEN-065). Each trigger produces a different `sub` (pull_request, branch ref, environment), so each needs its own policy line or a `g` mapping to a role.
  - GitHub tokens carry no `email` or `groups`, so the display username falls back to the subject and only user-level policies apply.
- **Configuration surface**: `argocd-cm` `dex.config` (connector above); `argocd-rbac-cm` `policy.csv`; GitHub workflow `permissions.id-token: write`.
- **Exposure**: HTTP (`/api/dex/token`); API; CLI.
- **Tests required**:
  - Positive: a token with `sub=repo:o/r:pull_request` (signed by a mock `token.actions.githubusercontent.com` issuer) can `get` apps in `my-project`.
  - Negative: a token with `sub=repo:o/r:ref:refs/heads/main` is denied by the same policy.
  - Negative: a GHES issuer token is rejected by a connector configured for github.com.
  - Authorization: the subject cannot `sync` unless the policy grants it.

### TEN-136 GitLab CI OIDC login recipe
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/gitlab-ci.md#L11-L29, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/gitlab-ci.md#L33-L66, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/gitlab-ci.md#L69-L83, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/assets/model.conf#L13-L14, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/rbac/rbac.go#L400-L426
- **Maturity**: GA
- **Semantics**: Dex connector `type: oidc`, `config.issuer: https://gitlab.com` (or the self-managed GitLab URL), `scopes: [openid]`, `userNameKey: sub`, `insecureSkipEmailVerified: true`.
  - The job declares `id_tokens: GITLAB_OIDC_TOKEN: aud: <Argo CD URL>` and runs the TEN-134 exchange with `connector_id` equal to the connector `id`, using `.access_token` as `ARGOCD_AUTH_TOKEN`.
  - The RBAC subject is the GitLab `sub` claim, `project_path:<group>/<project>:ref_type:<branch|tag>:ref:<ref>`. Example: `p, project_path:my-repo/my-project:ref_type:branch:ref:main, applications, sync, production/*, allow`.
- **Edge cases**:
  - The guide's connector `id` (`github-ci`) differs from its `connector_id` (`gitlab-ci`). Copied verbatim, the exchange fails in Dex (see Doc differences).
  - The guide's wildcard subject `project_path:my-repo/my-project:*` only matches a subject literally equal to that string: subjects are not glob-matched, and no Casbin role-matching function is registered.
- **Configuration surface**: `argocd-cm` `dex.config`; `argocd-rbac-cm` `policy.csv`; GitLab job `id_tokens.<VAR>.aud`.
- **Exposure**: HTTP (`/api/dex/token`); API; CLI.
- **Tests required**:
  - Positive: the `...:ref_type:branch:ref:main` subject may `sync` `production/*`.
  - Negative: the `...:ref:feature-x` subject is denied `sync`.
  - Negative (parity): a policy subject containing `*` does not match concrete GitLab subjects.
  - Negative: a mismatched `connector_id` produces a Dex error passed through the proxy.

### TEN-137 Dex connector environment-variable references and `$` escaping
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/dex/config.go#L137-L151, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L2526-L2540, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L2543-L2588, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/dex/dex_test.go#L584-L703, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-dex/commands/argocd_dex.go#L104-L119, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/microsoft.md#L280-L300
- **Maturity**: GA
- **Semantics**: After `$`-secret resolution of the whole Dex config (TEN-131), each connector's `config` map is walked recursively, including lists and nested maps. In every string value, `$` is replaced by `$$` so that Dex's environment expansion yields the literal character. This applies both to resolved secret values and to literal strings that contain `$` (e.g. `test$test` → `test$$test`, `pass${WORD}end` → `pass$${WORD}end`, `a$b$c$d` → `a$$b$$c$$d`). The one exception is a value that starts with `$` and whose remainder is not a key in the resolved secret map, i.e. an unresolved reference such as `$AUTHENTIK_CLIENT_ID` or `$MICROSOFT_APPLICATION_ID`. Such a value is left unescaped, so Dex can expand it from the environment of the `dex` container. Dex runs as a child process of `argocd-dex rundex` and inherits that environment. Top-level keys (`issuer`, `staticClients`, `logger`) and connector keys outside `config` are not escaped.
- **Edge cases**:
  - Unresolved references log the warning `secret key does not exist in secret`.
  - A bare `$` is escaped to `$$`.
  - A resolved secret whose value itself starts with `$` (e.g. secret value `$abc`) is re-examined after resolution. Because `abc` is not a secret key, it is treated as an unresolved env reference and left unescaped, which exposes it to Dex env expansion (code-derived; parity).
  - Strings with a `$` that is not at the start are never treated as references.
- **Configuration surface**: `argocd-cm` `dex.config` connectors; `argocd-secret` / labelled Secrets; env vars on the `argocd-dex-server` `dex` container.
- **Exposure**: Dex config generation (`argocd-dex rundex`, `argocd-dex gendexcfg`).
- **Tests required**:
  - Literal and secret-resolved `$` are escaped in connector config; `issuer` is not.
  - `$UNSET_KEY` is passed through verbatim, and the Dex process resolves it from its env.
  - Golden output for nested lists/maps inside `config`.
  - Parity case: a secret value beginning with `$` is not escaped.

### TEN-138 Dex connector redirectURI override and connector validation
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/dex/config.go#L111-L136, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/dex/config.go#L158-L164, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L2288-L2295, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L2337-L2339, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L802-L830, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-dex/commands/argocd_dex.go#L104-L127, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/index.md#L215-L219
- **Maturity**: GA
- **Semantics**: For connectors of type `oidc`, `saml`, `microsoft`, `linkedin`, `gitlab`, `github`, `bitbucket-cloud`, `openshift`, `gitea`, `google` and `oauth`, `config.redirectURI` is unconditionally set to `<url>/api/dex/callback`. The URL is built with `path.Join`, so a path prefix in `url` is kept. Any user-supplied value is overwritten, including the external or localhost values shown in several IdP guides. Other connector types (e.g. `ldap`, `authproxy`, `keystone`, `atlassian-crowd`) are left untouched. `additionalUrls` do not produce extra connector callbacks, because Dex has a single issuer.
- **Edge cases**:
  - `connectors` missing or not a list, a connector entry that is not a map, or a redirect-type connector whose `config` is not a map → `malformed Dex configuration found`.
  - A connector without a string `type` triggers an unchecked type assertion (panic).
  - Both failures are fatal: `argocd-server` calls `CheckError` on Dex config generation at start-up and on each settings change, and `argocd-dex rundex` does the same. A bad `dex.config` applied at runtime therefore makes both processes exit (crash-loop) until it is fixed.
  - Config generation is skipped entirely (no Dex) when `url` is empty.
- **Configuration surface**: `argocd-cm` `url`, `dex.config.connectors[].type`, `connectors[].config.redirectURI` (ignored for listed types).
- **Exposure**: Dex config generation; `argocd-server` and `argocd-dex-server` lifecycle.
- **Tests required**:
  - A user `redirectURI` on `saml`/`oidc`/`google`/`microsoft` is replaced with `<url>/api/dex/callback`; on `ldap` the config has no injected key.
  - A `url` with a sub-path keeps the prefix.
  - A malformed connector list, or a connector without `type`, causes the documented error/exit (parity) and recovers after the ConfigMap is fixed and pods restart.

### TEN-139 Dex SAML connector integration (shared by SAML recipes)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/dex/config.go#L111-L136, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/oidc/oidc.go#L196-L201, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/okta.md#L13-L64, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/microsoft.md#L214-L278, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/google.md#L73-L139, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/identity-center.md#L12-L71, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/dex/argocd-dex-server-deployment.yaml#L88-L110
- **Maturity**: GA
- **Semantics**: SAML IdPs are supported only through a Dex `type: saml` connector. The recipes use these `config` keys:
  - `ssoURL`: IdP SSO endpoint.
  - `caData` (base64 of the PEM certificate including the BEGIN/END lines) or `ca` (path to a file mounted into the `dex` container).
  - `entityIssuer`: SP entity ID, normally `<url>/api/dex/callback`.
  - `ssoIssuer`: optional IdP entity ID.
  - `usernameAttr`, `emailAttr`, `groupsAttr`: SAML attribute names.

  The IdP must use `<url>/api/dex/callback` as ACS URL, and Argo CD forces that value into `redirectURI` (TEN-138). Login is SP-initiated via `/auth/login`. The Argo CD web flow requests Dex scopes `openid profile email groups federated:id` (TEN-117), so the SAML groups attribute becomes the `groups` claim of the Dex ID token, and RBAC uses `scopes` (default `[groups]`, TEN-074). Recipes commonly set `scopes: '[groups, email]'` (or `[email,groups]`) so that `g` lines can bind either group values or the user's email.
- **Edge cases**:
  - IdP-initiated login (tile/badge opening the ACS directly) is not supported by Dex. The Okta guide disables app visibility for this reason.
  - Without `groupsAttr`, users have no groups and only user/email bindings and `policy.default` apply.
  - Group values are passed through verbatim: names, IDs or DNs. Values containing commas must be quoted in `policy.csv` (TEN-066).
- **Configuration surface**: `argocd-cm` `url`, `dex.config.connectors[type=saml].config.*`; `argocd-rbac-cm` `scopes`, `policy.csv`; optional volume mount on `argocd-dex-server` for `ca`.
- **Exposure**: UI (SSO button → `/auth/login` → IdP → `/api/dex/callback` → `/auth/callback`); API.
- **Tests required**:
  - Mock SAML IdP: the groups attribute appears in `GET /api/v1/session/userinfo` `groups` and grants `role:admin` via `g, <group>, role:admin`.
  - `scopes: '[groups, email]'` lets an email-keyed `g` line apply.
  - A user without groups gets `policy.default` only.
  - The `ca` file variant works when mounted into the `dex` container.

### TEN-140 Auth0 OIDC recipe (namespaced groups claim)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/auth0.md#L8-L23, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/auth0.md#L28-L68, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L228-L249, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L1949-L1974
- **Maturity**: GA
- **Semantics**: Direct OIDC (no Dex) with `oidc.config` keys `name: Auth0`, `issuer: https://<tenant>.<eu|us>.auth0.com/`, `clientID`, `clientSecret`. `requestedScopes` lists `openid, profile, email` plus the custom claim name (e.g. `http://your.domain/groups`), which is sent as an extra scope value. The IdP application registers callback `<url>/auth/callback` and login URL `<url>/login`. Auth0 emits group membership under a namespaced (URL) claim, so `argocd-rbac-cm` sets `scopes: '[http://your.domain/groups, email]'`. The RBAC claim names are arbitrary strings, URLs included. Example policy: `p, someProjectGroup, applications, *, someProject/*, allow`, `g, argocd-global-admins, role:admin`, `policy.default: role:readonly`.
- **Edge cases**:
  - The guide's `domain_hint` key is not an `oidc.config` field: unknown keys are silently ignored, and only `domainHint` is honoured (TEN-124).
  - If `scopes` still contains only `groups`, users get no groups and fall back to `policy.default`.
- **Configuration surface**: `argocd-cm` `url`, `oidc.config`; `argocd-secret` (via `$` reference, TEN-131); `argocd-rbac-cm` `scopes`, `policy.csv`, `policy.default`.
- **Exposure**: UI; API; CLI (`argocd login --sso`, requires a localhost callback on the IdP client).
- **Tests required**:
  - A token with claim `"http://your.domain/groups": ["argocd-global-admins"]` plus `scopes` set to that URL grants admin.
  - Without the `scopes` change → readonly only.
  - `domain_hint` in config does not add `domain_hint` to the authorization URL; `domainHint` does.

### TEN-141 Okta SAML via Dex recipe
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/okta.md#L13-L64, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/okta.md#L68-L104, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/okta.md#L106-L121, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/dex/config.go#L111-L136
- **Maturity**: GA
- **Semantics**: This is the TEN-139 SAML connector with `id: okta` and `usernameAttr: email`, `emailAttr: email`, `groupsAttr: group`. Okta's Group Attribute Statement regex (e.g. `argocd-*`) controls which Okta groups are sent. RBAC: `g, argocd-admins, role:admin` with `scopes: '[email,groups]'`. Optional Dex `logger: {level: debug, format: json}` is preserved (TEN-114). For the "private deployment" variant, the guide exposes only the callback publicly and sets the connector `redirectURI` to the external host.
- **Edge cases**:
  - Argo CD overwrites `redirectURI` with `<url>/api/dex/callback` (TEN-138). The private-deployment variant can only work when `url` itself is the externally reachable callback host (see Doc differences).
  - The guide's example lists both `caData` and `ca` while stating only one may be set (Dex behaviour when both are present: Unconfirmed).
- **Configuration surface**: `argocd-cm` `url`, `dex.config`; `argocd-rbac-cm` `scopes`, `policy.csv`.
- **Exposure**: UI; API.
- **Tests required**:
  - SAML assertion with `group=argocd-admins` → admin; a user whose groups are not sent → readonly / denied.
  - Configured `redirectURI` ignored in the generated Dex config.

### TEN-142 Okta OIDC recipe (custom authorization server, CLI client)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/okta.md#L123-L177, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/okta.md#L179-L224, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/oidc/oidc.go#L901-L923, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L2142-L2157, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apiclient/apiclient.go#L321-L358
- **Maturity**: GA
- **Semantics**: Direct OIDC with `issuer` set to an Okta custom authorization server (`https://<org>.okta.com/oauth2/<id>`), `clientID`, `clientSecret` (web app with redirect `<url>/auth/callback`), and `cliClientID` (a separate SPA app with redirect `http://localhost:8085/auth/callback`). It requests `requestedScopes: ["openid","profile","email","groups"]` and `requestedIDTokenClaims: {"groups": {"essential": true}}`, which is sent as the OIDC `claims` parameter (TEN-118). The authorization server defines a `groups` scope and a `groups` ID-token claim filtered by regex. Tokens minted for `cliClientID` are accepted because `cliClientID` is an implicit allowed audience (TEN-119/TEN-121). The CLI flow uses PKCE without a secret (TEN-130).
- **Edge cases**:
  - Without API Access Management (the custom authorization server), Okta's org server does not return `groups`. RBAC then sees no groups (guide warning).
  - A missing `cliClientID` makes the CLI use the web `clientID`, which needs the localhost redirect and public-client settings.
- **Configuration surface**: `argocd-cm` `oidc.config` (`issuer`, `clientID`, `clientSecret`, `cliClientID`, `requestedScopes`, `requestedIDTokenClaims`); `argocd-rbac-cm`.
- **Exposure**: UI; API; CLI (`argocd login --sso`).
- **Tests required**:
  - The authorization URL contains `claims={"id_token":{"groups":{"essential":true}}}` and the `groups` scope.
  - A CLI token with `aud=<cliClientID>` is accepted; one with a foreign `aud` is rejected.
  - The `groups` claim maps to roles.

### TEN-143 OneLogin OIDC recipe (roles as groups, client_secret_basic)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/onelogin.md#L35-L72, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/onelogin.md#L78-L85, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/onelogin.md#L104-L168, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/oidc/oidc.go#L290-L303, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/oidc/provider.go#L207-L214
- **Maturity**: GA
- **Semantics**: Direct OIDC with `issuer: https://<subdomain>.onelogin.com/oidc/2`, `clientID`, `clientSecret` (a plain value; older base64 encoding is no longer required) and `requestedScopes` equal to the default `[openid, profile, email, groups]`. The OneLogin app's Groups parameter is set to "User Roles" with "Semicolon Delimited Input", so the token's `groups` claim lists role names (e.g. `["Manager","ProductTeam","TestEngineering"]`). RBAC: `g, TestEngineering, role:org-admin` with a custom role and `policy.default: role:readonly`. The OneLogin SSO tab sets Token Endpoint auth to "Basic". Argo CD builds its `oauth2.Config` without an `AuthStyle`, so `golang.org/x/oauth2` (v0.36.0) first sends HTTP Basic credentials (client ID and secret form-URL-encoded) and retries with form-body credentials on failure, caching the style that worked.
- **Edge cases**: A wrong secret → IdP `invalid_client`, and `/auth/callback` returns HTTP 500 `failed to get token: ...` (wrapping the IdP error), on both auth styles. Role names with commas must be quoted.
- **Configuration surface**: `argocd-cm` `oidc.config`; `argocd-rbac-cm` `policy.csv`, `policy.default`.
- **Exposure**: UI; API.
- **Tests required**:
  - Mock IdP accepting only `client_secret_basic` → login succeeds.
  - Mock IdP accepting only `client_secret_post` → login succeeds after fallback.
  - The `TestEngineering` role grants the custom role's permissions; others get readonly.

### TEN-144 Keycloak OIDC recipe with client authentication
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/keycloak.md#L9-L81, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/keycloak.md#L157-L206, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/keycloak.md#L225-L229, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/oidc/oidc.go#L718-L748
- **Maturity**: GA
- **Semantics**: This recipe uses a confidential Keycloak client with redirect `<url>/auth/callback`. The secret is stored in `argocd-secret` key `oidc.keycloak.clientSecret` and referenced as `clientSecret: $oidc.keycloak.clientSecret` (TEN-131). `oidc.config` has `issuer: https://<keycloak>/realms/<realm>` (Keycloak < 17 uses `/auth/realms/<realm>`), `clientID`, `refreshTokenThreshold: 2m` and `requestedScopes: ["openid","profile","email","groups","offline_access"]`. `offline_access` makes Keycloak return a refresh token, so server-side refresh works (TEN-125). Groups come from a `groups` client scope with a Group Membership mapper (claim `groups`, full path disabled). RBAC: `g, ArgoCDAdmins, role:admin`.
- **Edge cases**:
  - If `refreshTokenThreshold` is ≥ the token lifetime (Keycloak default 5 min), every request triggers a refresh.
  - With "full group path" enabled, group values are `/ArgoCDAdmins`, so the `g` line must match that exactly.
  - The guide advises restarting `argocd-server` on 401/login loops.
  - The CLI cannot use this confidential client, because the CLI never sends a secret (TEN-130).
- **Configuration surface**: `argocd-cm` `oidc.config`; `argocd-secret` `oidc.keycloak.clientSecret`; `argocd-rbac-cm`.
- **Exposure**: UI; API.
- **Tests required**:
  - Login with secret reference resolved.
  - Token refreshed when within 2m of expiry.
  - `groups=["ArgoCDAdmins"]` → admin; `"/ArgoCDAdmins"` does not match `ArgoCDAdmins`.

### TEN-145 Keycloak OIDC recipe with PKCE (public client, CLI login)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/keycloak.md#L83-L155, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/keycloak.md#L208-L233, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/oidc/oidc.go#L456-L459, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/oidc/oidc.go#L575-L577, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/login.go#L205-L361
- **Maturity**: GA
- **Semantics**: This recipe uses a public Keycloak client (no client authentication) with PKCE method S256 and redirect URIs `http://localhost:8085/auth/callback` (CLI; depends on `--sso-port`) and `<url>/auth/callback`. `oidc.config` has `issuer`, `clientID`, `enablePKCEAuthentication: true` (no `clientSecret`), `refreshTokenThreshold: 2m` and the same scopes as TEN-144. The server web flow then sends `code_challenge`/`code_challenge_method=S256` and supplies the verifier at code exchange (TEN-120). The CLI (`argocd login <host> --sso --grpc-web`) always uses PKCE with the same `clientID` (TEN-130).
- **Edge cases**:
  - Switching an existing install from client auth to PKCE can surface `invalid_request: Missing parameter: code_challenge_method` from stale browser state (the guide suggests clearing cookies).
  - The guide's extra redirect `<url>/pkce/verify` does not correspond to any route at this commit (see Doc differences).
- **Configuration surface**: `argocd-cm` `oidc.config.enablePKCEAuthentication`; CLI `--sso`, `--sso-port`.
- **Exposure**: UI; API; CLI.
- **Tests required**:
  - Web login against a PKCE-required public client succeeds only with `enablePKCEAuthentication: true`.
  - CLI login succeeds and its token is accepted (aud = clientID).
  - With PKCE disabled → IdP error surfaced as a callback failure.

### TEN-146 OpenUnison OIDC recipe (public client, DN groups)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/openunison.md#L9-L32, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/openunison.md#L38-L71, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/rbac/rbac.go#L603-L633
- **Maturity**: GA
- **Semantics**: An OpenUnison `Trust` with `clientId: argocd`, `publicEndpoint: true` (no client secret, because the CLI cannot use one) and redirect URIs `<url>/auth/callback` plus `http://localhost:8085/auth/callback`. `oidc.config` has `name: OpenUnison`, `issuer: https://<ou-host>/auth/idp/k8sIdp`, `clientID: argocd` and `requestedScopes: ["openid","profile","email","groups"]`, with no `clientSecret`. Both web and CLI logins use the same client. Groups arrive in the `groups` claim. For LDAP/AD sources they are full Distinguished Names, which contain commas and must be double-quoted: `g, "CN=k8s_login_cluster_admins,CN=Users,DC=...", role:admin`. An OpenUnison portal "badge" opens Argo CD, which then completes the SP-initiated login silently against the existing OpenUnison session.
- **Edge cases**:
  - The server web flow exchanges the code without a secret: Basic auth with an empty password first, then form fallback (TEN-143 mechanism).
  - An unquoted DN is split by the CSV parser into extra fields, and the `g` line is rejected (`invalid RBAC policy`).
- **Configuration surface**: `argocd-cm` `oidc.config`; `argocd-rbac-cm` `policy.csv`.
- **Exposure**: UI; API; CLI.
- **Tests required**:
  - A DN group (quoted) grants admin.
  - The unquoted DN line fails validation.
  - Web and CLI logins both work with a public client and no secret.

### TEN-147 Zitadel OIDC recipe (project roles as groups, logout, SSO-only)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/zitadel.md#L25-L93, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/zitadel.md#L95-L141, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/zitadel.md#L144-L210, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/logout/logout.go#L133-L139, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/login/components/login.tsx#L71-L92
- **Maturity**: GA
- **Semantics**: Zitadel web app (CODE flow) with redirect `<url>/auth/callback` and post-logout URI `<url>`. Project roles `argocd_administrators`/`argocd_users` are copied into a `groups` claim by a Zitadel Action (`groupsClaim`) on the "Pre Userinfo creation" and "Pre access token creation" triggers.
  - `argocd-cm`: `admin.enabled: "false"` (with no other `login`-capable local accounts, `userLoginsDisabled` is true and the UI hides the form) and `oidc.config` with `name: Zitadel`, `issuer`, `clientID` of the form `<number>@<project>`, `clientSecret`, `requestedScopes` = defaults, and a static `logoutURL: https://<zitadel>/oidc/v1/end_session` (no placeholders, TEN-123).
  - `argocd-rbac-cm`: `scopes: '[groups]'`, `g, argocd_administrators, role:admin`, `g, argocd_users, role:readonly`, `policy.default: ''` (no default role).
  - The UI shows only the SSO button, labelled `Log in via Zitadel` (TEN-155).
- **Edge cases**:
  - Users with no Zitadel role get no groups and, with an empty default, are denied everything.
  - The client ID contains `@`, which is form-URL-encoded (`%40`) inside the HTTP Basic credentials at the token endpoint.
  - If the Zitadel action is not marked "Allowed To Fail", users without roles cannot log in at the IdP.
- **Configuration surface**: `argocd-cm` `admin.enabled`, `url`, `oidc.config` (`logoutURL`); `argocd-rbac-cm` `scopes`, `policy.csv`, `policy.default`.
- **Exposure**: UI (login page, User Info groups, logout redirect); API.
- **Tests required**:
  - The `argocd_users` group gives readonly; no group → all requests denied.
  - Logout redirects to the static end-session URL.
  - With `admin.enabled: "false"` the login page has no username/password form.
  - A client ID with `@` authenticates.

### TEN-148 Microsoft Entra ID OIDC recipe (app registration)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/microsoft.md#L10-L125, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/microsoft.md#L127-L212, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/microsoft.md#L302-L360, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/oidc/oidc.go#L486-L522, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/session/sessionmanager.go#L656-L666
- **Maturity**: GA
- **Semantics**:
  - **App registration**: web redirect `<url>/auth/callback` plus a "Mobile and desktop" redirect `http://localhost:8085/auth/callback` for the CLI. Permission `User.Read` and a groups claim (token configuration).
  - **`oidc.config`**: `name: Azure`, `issuer: https://login.microsoftonline.com/<tenant>/v2.0`, `clientID`, and either `clientSecret: $oidc.azure.clientSecret` or `azure.useWorkloadIdentity: true`. Workload identity relies on pod label `azure.workload.identity/use: "true"` and service-account annotation `azure.workload.identity/client-id` on `argocd-server`, plus a federated credential; the token is read from `AZURE_FEDERATED_TOKEN_FILE` (TEN-126). Also `requestedIDTokenClaims: {groups: {essential: true, value: "ApplicationGroup"}}` and `requestedScopes: [openid, profile, email]`.
  - **Groups**: the `groups` claim carries group object IDs (GUIDs), bound with `g, "<object-id>", role:org-admin`. `scopes: '[groups, email]'` additionally allows email bindings. Users in 200+ groups need `azure.enableUserGroupOverageClaim` (TEN-126).
  - **Optional settings**: `domainHint` adds `domain_hint` to the web authorization URL (TEN-124).
  - **CLI**: `argocd login <host> --grpc-web-root-path / --sso` runs the PKCE code flow with `access_type=offline`, the `claims` parameter and `offline_access` (TEN-130).
- **Edge cases**:
  - Entra tokens carry `uti` instead of `jti`. Revocation verification uses `uti`, but logout does not record it (existing Doc difference).
  - Graph overage failures return 401 (TEN-126).
  - Copying the guide's inline `// ...` comments into YAML makes `useWorkloadIdentity` a string, so the whole `oidc.config` is discarded (see Doc differences).
- **Configuration surface**: `argocd-cm` `oidc.config` (`azure.*`, `requestedIDTokenClaims`, `domainHint`); `argocd-secret` `oidc.azure.clientSecret`; `argocd-server` pod label / SA annotation; env `AZURE_FEDERATED_TOKEN_FILE`; `argocd-rbac-cm`.
- **Exposure**: UI; API; CLI.
- **Tests required**:
  - GUID group binding grants the custom role.
  - Secret vs workload-identity code exchange: the latter sends a `client_assertion`.
  - CLI login token accepted (aud = clientID).
  - The guide's YAML with inline `//` comments disables SSO (parity).

### TEN-149 Microsoft Entra ID SAML via Dex recipe
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/microsoft.md#L214-L278, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/dex/config.go#L111-L136
- **Maturity**: GA
- **Semantics**: This is a TEN-139 SAML connector (`id: saml`). The Entra enterprise app sets Identifier (Entity ID) and Reply URL to `<url>/api/dex/callback` and Sign-on URL to `<url>/auth/login`, adds an `email` claim from `user.mail`, and adds a group claim named `Group` (all groups, source Group ID, not emitted as role claims). The connector config has `entityIssuer: <url>/api/dex/callback`, `ssoURL: https://login.microsoftonline.com/<tenant>/saml2`, `caData` (base64 signing cert), `usernameAttr: email`, `emailAttr: email` and `groupsAttr: Group`. RBAC binds group object IDs: `g, "<object-id>", role:org-admin`, with `policy.default: role:readonly`.
- **Edge cases**:
  - The attribute name is case-sensitive (`Group` vs `groups`). A mismatch yields no groups (Dex behaviour; see Unconfirmed).
  - The `redirectURI` in the example is overwritten (TEN-138).
- **Configuration surface**: `argocd-cm` `dex.config`; `argocd-rbac-cm`.
- **Exposure**: UI; API.
- **Tests required**:
  - SAML assertion with a `Group` attribute value equal to the GUID → custom role.
  - Without the attribute → readonly.

### TEN-150 Microsoft Entra ID via Dex `microsoft` connector recipe
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/microsoft.md#L280-L300, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/dex/config.go#L158-L164, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L2578-L2588
- **Maturity**: GA
- **Semantics**: Dex connector `type: microsoft` with `config.clientID: $MICROSOFT_APPLICATION_ID`, `config.clientSecret: $MICROSOFT_CLIENT_SECRET`, `config.tenant: <tenant id>` and `config.groups: [DevOps]` (Dex group filter). Argo CD first tries to resolve the `$` values from `argocd-secret`. If they are not secret keys, they pass unescaped and Dex expands them from the `dex` container environment (TEN-137). `redirectURI` is forced to `<url>/api/dex/callback` (TEN-138). Groups returned by Dex become the `groups` claim used by RBAC.
- **Edge cases**:
  - The example's `redirectURI: http://localhost:8080/api/dex/callback` is ignored.
  - If neither a secret key nor an env var exists, Dex receives an empty or literal value and the IdP rejects the client (Dex behaviour; Unconfirmed).
- **Configuration surface**: `argocd-cm` `dex.config`; `argocd-secret` keys or `dex` container env.
- **Exposure**: UI; API.
- **Tests required**:
  - `$KEY` present in `argocd-secret` is resolved and escaped.
  - `$ENV` absent from the secret is left for Dex env expansion.
  - The group filter restricts the groups seen in userinfo (Dex conformance).

### TEN-151 Google OIDC via Dex recipe (no groups)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/google.md#L3-L22, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/google.md#L24-L71, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/jwt/jwt.go#L142-L166, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/session/sessionmanager.go#L691-L707
- **Maturity**: GA
- **Semantics**: Dex connector `type: oidc`, `id: google`, `name: Google`, `config.issuer: https://accounts.google.com`, `clientID`, `clientSecret`. The Google OAuth client registers JS origin `<url>` and redirect `<url>/api/dex/callback`. Google does not expose `groups` via OIDC, so RBAC relies on `policy.default` (guide: `role:readonly`) and per-user bindings. The RBAC subject is `federated_claims.user_id`, the upstream Google `sub` (an opaque numeric ID), not the email. To bind by email, add `email` to `argocd-rbac-cm` `scopes` and use `g, user@example.com, role:admin`. The UI and CLI display the email.
- **Edge cases**:
  - A `p`/`g` line keyed on the email does nothing unless `email` is listed in `scopes`.
  - "Authorized domains" on the consent screen limits who can log in, not Argo CD RBAC.
- **Configuration surface**: `argocd-cm` `dex.config`; `argocd-rbac-cm` `policy.default`, `scopes`, `policy.csv`.
- **Exposure**: UI; API; CLI (`argocd login --sso` via `argo-cd-cli`).
- **Tests required**:
  - The subject equals the Google `sub`; the email shown in userinfo.
  - An email binding is effective only with `scopes` including `email`.
  - Readonly default applies.

### TEN-152 Google SAML via Dex recipe
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/google.md#L73-L144
- **Maturity**: GA
- **Semantics**: This is a TEN-139 SAML connector (`id: saml`). The Google custom SAML app sets ACS URL and Entity ID to `<url>/api/dex/callback` and maps Primary email to the attributes `name` and `email`. The connector config has `ssoURL: https://accounts.google.com/o/saml2/idp?idpid=<id>`, `entityIssuer: <url>/api/dex/callback`, `caData` (base64 PEM), `usernameAttr: name`, `emailAttr: email` and optional `ssoIssuer`. No group information is available, so RBAC uses `policy.default` and email bindings (via `scopes`).
- **Edge cases**: The certificate must be PEM before base64 encoding. Dex documents SAML as deprecated/unsafe (guide warning).
- **Configuration surface**: `argocd-cm` `dex.config`; `argocd-rbac-cm`.
- **Exposure**: UI; API.
- **Tests required**:
  - Mock Google SAML IdP login → `policy.default` permissions.
  - Email binding with `scopes: '[groups, email]'`.

### TEN-153 Google Groups via Dex `google` connector recipe
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/google.md#L146-L268, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/dex/argocd-dex-server-deployment.yaml#L19-L110, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/dex/config.go#L158-L164, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-dex/commands/argocd_dex.go#L82-L127
- **Maturity**: GA
- **Semantics**: Dex connector `type: google`, `id: google`, with `config.clientID`, `config.clientSecret`, `config.adminEmail` (the admin identity to impersonate), optional `fetchTransitiveGroupMembership: true`, and either `serviceAccountFilePath` (option 1) or no file (option 2, GKE Workload Identity on the `argocd-dex-server` service account).
  - **Option 1 credentials**: Secret `argocd-google-groups-json` (key `googleAuth.json`) is mounted read-only at `/tmp/oidc`, so `serviceAccountFilePath: /tmp/oidc/googleAuth.json`. The mount goes on the `dex` container (not the `copyutil` init container), alongside the existing `static-files` (`/shared`) and `dexconfig` (`/tmp`) mounts (and the stock `argocd-dex-server-tls` mount at `/tls`).
  - **Google-side setup**: a service account with domain-wide delegation and scope `https://www.googleapis.com/auth/admin.directory.group.readonly`, plus the Admin SDK enabled.
  - **RBAC**: the `groups` claim contains Google group email addresses (e.g. `g, sysadmins@yourcompany.com, role:admin`).
  - **Restart**: the guide restarts `argocd-dex-server` after changes. Note that `rundex` already restarts the Dex child process when the generated config changes, but not when a mounted file changes.
- **Edge cases**:
  - The `redirectURI` the guide asks to edit is overwritten anyway (`google` is a redirect-injected type).
  - Granting only the broader `admin.directory.group` scope prevents group retrieval (guide).
  - Replacing the `volumes`/`volumeMounts` lists verbatim with the guide's snippet drops the stock `argocd-dex-server-tls` mount. Dex then serves the self-signed certificate that `rundex` generates. This matters for `--dex-server-strict-tls`.
- **Configuration surface**: `argocd-cm` `dex.config`; Secret with the service-account JSON; `argocd-dex-server` Deployment `volumes`/`volumeMounts`; Dex SA Workload Identity annotation.
- **Exposure**: UI (User Info shows groups); API.
- **Tests required**:
  - Mock Google directory: the group email in the `groups` claim grants admin.
  - Transitive membership flag honoured (Dex conformance).
  - A missing credentials file → Dex connector failure surfaced as an SSO error (TEN-115 redirect).

### TEN-154 AWS IAM Identity Center SAML via Dex recipe
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/user-management/identity-center.md#L8-L92
- **Maturity**: GA
- **Semantics**: This is a TEN-139 SAML connector (`id: aws`, `name: AWS IAM Identity Center`). Config: `ssoURL: https://portal.sso.<region>.amazonaws.com/saml/assertion/<id>`, `caData` (base64 Identity Center certificate) or `ca` (a mounted file), `entityIssuer: <url>/api/dex/callback`, `usernameAttr: email`, `emailAttr: email`, `groupsAttr: groups`. Users/groups are assigned to the Identity Center application, and the group attribute mapping (described as an unofficial workaround) emits group IDs. RBAC: `g, <Identity Center Group ID>, role:admin` with `scopes: '[groups, email]'`.
- **Edge cases**:
  - Group values are Identity Center group IDs, not display names.
  - `redirectURI` in the example points at an external host and is overwritten (TEN-138).
- **Configuration surface**: `argocd-cm` `dex.config`; `argocd-rbac-cm` `scopes`, `policy.csv`.
- **Exposure**: UI; API.
- **Tests required**:
  - An assertion with `groups=<group-id>` → admin.
  - Email binding effective through `scopes`.

### TEN-155 UI SSO login button label
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/login/components/login.tsx#L71-L92, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/login/components/login.tsx#L94-L101, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/login/components/login.tsx#L139-L141, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/settings/settings.go#L41-L160
- **Maturity**: GA
- **Semantics**: The login page treats SSO as configured when the public settings have `dexConfig.connectors` non-empty or an `oidcConfig` object. The SSO button links to `auth/login?return_url=<encoded>`. Its label is:
  - `Log in via <oidcConfig.name>` when `oidcConfig` is present;
  - else `Log in via <connector name>` when exactly one Dex connector exists;
  - else `SSO Login`.

  An "or" separator and the username/password form appear only when `userLoginsDisabled` is false. When logins are disabled and no SSO is configured, the page shows `Login is disabled. Please contact your system administrator.`
- **Edge cases**:
  - `oidc.config` without `name` yields the label `Log in via ` with an empty name.
  - With two or more Dex connectors the generic label is shown, and Dex presents its own connector chooser.
- **Configuration surface**: `argocd-cm` `oidc.config.name`, `dex.config.connectors[].name`, `admin.enabled`, `accounts.*`.
- **Exposure**: UI (login page); API (`GET /api/v1/settings`).
- **Tests required**:
  - Label for each of the three cases.
  - SSO-only install hides the form.
  - Disabled logins with no SSO show the disabled message.

## Unconfirmed

- Regex match mode details (TEN-075): whether Casbin's regex matcher in v2.135.0 anchors patterns and how built-in policy objects such as `*/*` and `*` (invalid Go regexes) behave in `policy.matchMode: regex` (error-as-deny vs panic recovery). The Casbin module source is not vendored in the checkout; checked `util/rbac/rbac.go#L180-L183`, `util/rbac/rbac_test.go#L496-L509`, `go.mod#L30`.
- Maximum role-inheritance depth for `g` chains (Casbin default role manager) — not set in Argo CD code; checked `util/rbac/rbac.go`, `assets/model.conf`.
- Whether `accounts.admin: <capabilities>` in `argocd-cm` adds capabilities (e.g. `apiKey`) to the built-in admin account: code path `util/settings/accounts.go#L226-L283` appends them, but no test or doc confirms the intended support (checked `util/settings/accounts_test.go`, `docs/operator-manual/user-management/index.md`).
- Exact list of secrets passed as `secrets` to `updateSettingsFromSecret` (documented as Secrets labelled `app.kubernetes.io/part-of: argocd`); the label filter itself was not traced to its call site (checked `util/settings/settings.go#L1723-L1754`, `#L1449-L1462`).
- UI coverage for `sourceIntegrity` and `permitOnlyProjectScopedClusters`: no references found in `ui/src/app/settings/components/` or `ui/src/app/shared/models.ts`; they may only be editable via API/CLI YAML.
- Server-side handling of `users.session.duration` for the bundled Dex flow vs the documented "no refresh for Dex embedded login" note (TEN-125): server code calls `CheckAndRefreshToken` whenever Dex is configured; whether Dex returns refresh tokens to the `argo-cd` static client by default was not verified (checked `server/server.go#L1621-L1637`, `util/dex/config.go`).
- Behaviour of the UI when `GetUserInfo.loggedIn` is false but anonymous access is enabled (rendering of read-only views) — not traced in `ui/src/app/app.tsx`.
- Runtime effect of an invalid `policy.csv` update (TEN-078, fail-closed with Internal errors) is derived from code reading only (`util/rbac/rbac.go#L158-L205`, `#L436-L442`, `#L590-L599`, `server/server.go#L971-L984`, `util/grpc/grpc.go#L27-L32`); no upstream test exercises it, and the exact behaviour of non-gRPC HTTP handlers (terminal, extensions) on the resulting panic was not traced.
- Dex-internal behaviour of the token-exchange flow (TEN-134..TEN-136): whether Dex v2.45.1 enables the `urn:ietf:params:oauth:grant-type:token-exchange` grant by default; whether it accepts the grant from the public `argo-cd-cli` client with an empty secret; that it requires a connector supporting token identity (the `oidc` connector); how it validates the subject token's `aud` when the connector has no `clientID` (as in the recipes); and whether the issued access token carries `aud=argo-cd-cli`, `federated_claims` and `at_hash` (which decides whether `/auth/logout` can revoke it). Dex is not a Go dependency (no `dexidp` module in `go.mod`); only the image tag is pinned (`manifests/base/dex/argocd-dex-server-deployment.yaml#L40`). No upstream test exercises token exchange: a repository-wide search for `token-exchange` finds only `docs/operator-manual/user-management/github-actions.md` and `gitlab-ci.md`.
- How Dex expands unresolved `$VAR` values in connector config (TEN-137, TEN-150), including the result when the variable is unset in the `dex` container. Argo CD only guarantees that such values are passed unescaped (`util/dex/dex_test.go#L672-L683`); the expansion itself happens inside the Dex binary.
- Connector attribute semantics that are delegated to Dex (TEN-139, TEN-141, TEN-149..TEN-154): case-sensitivity of SAML `groupsAttr`; mapping of SAML NameID to `federated_claims.user_id`; behaviour when both `caData` and `ca` are set; the `microsoft` connector `groups` filter and group-name format; and the `google` connector's `fetchTransitiveGroupMembership` and Workload Identity support. Checked `util/dex/config.go`, `util/dex/dex_test.go` and the IdP guides; no Argo CD code interprets these keys.
- Whether the SSO button text is shown upper-cased (the guides show `LOGIN VIA AZURE` / `LOG IN WITH ZITADEL`): `ui/src/app/login/components/login.tsx#L87-L90` renders `Log in via <name>`, and the `argo-button` styles come from `argo-ui`, which is not present in the checkout (`ui/node_modules` absent; `ui/src/app/login/components/login.scss` upper-cases only `__more-row` links).

## Doc/implementation differences

- `docs/operator-manual/user-management/index.md#L124-L125` says `ARGOCD_SESSION_MAX_CACHE_SIZE` defaults to 1000; code default is 10000 (`util/session/sessionmanager.go#L85-L86`).
- `docs/operator-manual/user-management/index.md#L119-L122` says with `ARGOCD_SESSION_FAILURE_WINDOW_SECONDS=0` logins are rejected after 10 consecutive failures; code rejects after `ARGOCD_SESSION_FAILURE_MAX_FAIL_COUNT` (default 5) with no window (`util/session/sessionmanager.go#L444-L462`).
- `docs/operator-manual/user-management/index.md#L408-L413` documents `userInfoURL`; the implemented key is `userInfoBaseURL` (`util/settings/settings.go#L235`); `userInfoURL` is ignored and the issuer URL is used as base.
- `docs/operator-manual/rbac.md#L292-L295` states that an action "allowed or denied by the default policies" is effective without further evaluation; code only short-circuits on allow — a deny for the default role does not stop evaluation of the user's own policies (`util/rbac/rbac.go#L399-L406`).
- `docs/operator-manual/rbac.md#L74-L86` resource/action table omits the `write-repositories` resource (present in `util/rbac/rbac.go#L65` and `assets/builtin-policy.csv#L14`), lists `certificates` without `update` while the built-in admin policy grants `certificates, update` (`assets/builtin-policy.csv#L33`), and lists `accounts` actions get/update while `argocd admin settings rbac can --strict` only accepts accounts create/update (`cmd/argocd/commands/admin/settings_rbac.go#L93-L96`) — so `rbac can <sub> get accounts` fails in strict mode.
- `docs/operator-manual/rbac.md#L88-L98` lists only applications, applicationsets, logs, exec as project-scoped; code also treats `clusters` and `repositories` as project-scoped (`util/rbac/rbac.go#L112-L119`), and project-role validation's own error message lists all six.
- `docs/user-guide/projects.md#L90` says destination cluster `name` is not used for matching; code matches by name as well (`pkg/apis/application/v1alpha1/app_project_types.go#L511`), and `proj add-destination --name` exists.
- `docs/user-guide/projects.md#L93-L101` shows `argocd proj add-destination <PROJECT> <CLUSTER>,<NAMESPACE>`; the CLI takes `PROJECT SERVER/NAME NAMESPACE` as separate arguments (`cmd/argocd/commands/project.go#L288`).
- `docs/user-guide/projects.md#L130-L134` shows `allow-namespace-resource`/`deny-namespace-resource` with an optional `[<NAME>]`; namespaced commands accept exactly 3 arguments, while the cluster commands accept NAME (`cmd/argocd/commands/project.go#L665-L674`).
- `docs/user-guide/projects.md#L291` says global projects match `matchExpressions`; code uses a full LabelSelector, so `matchLabels` also works (`util/argo/argo.go#L1147`).
- `docs/user-guide/projects.md#L226` says project JWT tokens "aren't stored in Argo CD"; token metadata (iat/exp/id, not the token itself) is stored in `status.jwtTokensByRole` and `spec.roles[].jwtTokens` (`server/project/project.go#L147-L156`).
- `docs/operator-manual/user-management/index.md#L252-L255` says Argo CD's OIDC refresh does not apply to Dex's embedded login; server code invokes `CheckAndRefreshToken` when Dex is configured too (`server/server.go#L1622-L1636`) — effective behaviour depends on Dex issuing refresh tokens (see Unconfirmed).
- `docs/operator-manual/user-management/index.md#L450-L454` says logout revokes the token; revocation keys on `jti` or `at_hash` (`server/logout/logout.go#L113-L117`) whereas verification also uses `uti` (`util/session/sessionmanager.go#L661-L666`), so Entra ID tokens carrying `uti` without `jti` are not effectively revoked.
- `cmd/argocd/commands/admin/project.go#L100` error text refers to flag `--group`, but the flag is `--scope`.
- `cmd/argocd/commands/login.go#L320-L333`: the CLI's `domain_hint` handling reads an uninitialised struct, so `oidc.config.domainHint` is never sent by `argocd login --sso` although the server web flow sends it.
- `docs/operator-manual/user-management/gitlab-ci.md#L19` defines the Dex connector as `id: github-ci`, but the exchange example at `#L50` sends `connector_id=gitlab-ci`. Followed verbatim, the exchange fails in Dex.
- `docs/operator-manual/user-management/gitlab-ci.md#L78` uses the policy subject `project_path:my-repo/my-project:*` as if it were a wildcard. Subjects are matched exactly via `g(r.sub, p.sub)` (`assets/model.conf#L14`), and no Casbin role-matching function is registered (`util/rbac/rbac.go#L180-L200`), so the line only matches that literal subject.
- `docs/operator-manual/user-management/auth0.md#L42` sets `domain_hint` in `oidc.config`; the implemented key is `domainHint` (`util/settings/settings.go#L243`). Unknown keys are ignored silently, so no domain hint is sent.
- Several guides set connector `redirectURI` to a specific value: `okta.md#L99` (private deployment, external host), `identity-center.md#L67`, `microsoft.md#L260`, `microsoft.md#L296` (`http://localhost:8080/...`), and `google.md#L134`, `#L221-L231`. Argo CD always overwrites `redirectURI` with `<url>/api/dex/callback` for these connector types (`util/dex/config.go#L111-L136`). As a result, the Okta private-deployment variant cannot point the SAML ACS at a host other than `url`.
- `docs/operator-manual/user-management/keycloak.md#L113` lists redirect URI `https://{hostname}/pkce/verify` as needed for the UI. No `pkce/verify` route exists in `ui/src`, `server` or `util/oidc` at this commit; server-side PKCE completes at `/auth/callback` (`util/oidc/oidc.go#L456-L459`, `#L575-L577`).
- `docs/operator-manual/user-management/microsoft.md#L77-L79` puts `// ...` comments inline in the `oidc.config` YAML. `useWorkloadIdentity: true // ...` then parses as a string, unmarshalling into the boolean fails, and the whole `oidc.config` is discarded with warning `invalid oidc config` (`util/settings/settings.go#L1949-L1974`). Likewise, `clientSecret` would include the comment text.
- `docs/operator-manual/user-management/google.md#L190-L215` shows `volumeMounts`/`volumes` lists for `argocd-dex-server` that omit the stock `argocd-dex-server-tls` volume (`manifests/base/dex/argocd-dex-server-deployment.yaml#L93-L110`). Applied as replacements, they drop the configured Dex TLS certificate, and `rundex` falls back to a generated self-signed one (`cmd/argocd-dex/commands/argocd_dex.go#L82-L96`).
