# Operations — parity inventory (Argo CD v3.5.5)

Scope: everything an operator touches to install, configure, scale, observe, secure, back up and troubleshoot an
Argo CD v3.5.5 control plane. In scope: the installation bundles (`install.yaml`, `namespace-install.yaml`,
`core-install.yaml`, `ha/*`, `*-with-hydrator`, `cluster-rbac`, `crds`) and the components, Services, RBAC,
NetworkPolicies and probes they ship; every key of `argocd-cmd-params-cm` (grouped per component, every key listed
with its env var and default); an ownership register of every `argocd-cm` key; the `argocd-secret` keys and their
bootstrap; high availability and scaling (replicas, processors, kubectl/repo-server parallelism, caching, controller
sharding algorithms, manual shard assignment, dynamic cluster distribution, Redis and Redis HA, ApplicationSet leader
election, workqueue rate limits, Kubernetes client QPS/retry); reconciliation scheduling (soft/hard reconciliation,
jitter, ignore-resource-updates); every Prometheus metric with type and labels for the application controller, API
server, repo-server, ApplicationSet controller, commit-server and notifications controller; OpenTelemetry tracing;
logging; Kubernetes events/audit; disaster recovery (`argocd admin export/import`); TLS/mTLS for every component;
ingress, gRPC/HTTP multiplexing, root path/base href; `managed-by-url`; core (headless) mode and `argocd admin
dashboard`; the operator-facing `argocd admin` troubleshooting tools; the server-command flag references; tested
Kubernetes versions; the security-considerations document; and the 3.0–3.5 upgrade notes that define baseline defaults.

Out of scope (owned by other area files, only cross-referenced here): the semantics of individual argocd-cm keys that
belong to rendering (`02-sources-rendering.md`), repositories/clusters/webhooks (`03-repositories-clusters-webhooks.md`),
diff/health/resource customizations (`04-diff-health-resources.md`), synchronization (`05-synchronization.md`),
ApplicationSets (`06-applicationset.md`), projects/RBAC/authentication/SSO (`07-projects-rbac-auth.md`), plus the
notifications engine, source hydrator, UI/extensions and the general (non-admin) CLI, which are covered by their own
area inventories. Where an ops key/flag only plumbs a value into one of those features, this file lists the key and
points at the owner.

Permalink base: `https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/`.

General parsing rule used throughout (applies to every env-backed flag): flags default to the value of their env var;
an explicitly passed flag wins over the env var; an env var that fails to parse or is outside the allowed range is
ignored with a warning and the compiled default is used; booleans accept only `true`/`false` (case-insensitive);
durations accept Go syntax and, as a fallback, day suffixes (`util/env/env.go`).

---

## A. Installation bundles, components and RBAC

### OPS-001 Cluster-scoped standard install bundle (`manifests/install.yaml`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/hack/update-manifests.sh#L108 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/cluster-install/kustomization.yaml#L4 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/namespace-install/kustomization.yaml#L4 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/kustomization.yaml#L5 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/installation.md#L18 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/installation.md#L89 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/README.md#L5
- **Maturity**: GA
- **Semantics**: `install.yaml` = `kustomize build manifests/cluster-install` = namespace-install base + `cluster-rbac`
  + `crds`. It ships: 3 CRDs (`applications`, `applicationsets`, `appprojects`.argoproj.io); ConfigMaps `argocd-cm`
  (with default exclusions/ignoreResourceUpdates, see OPS-155), `argocd-cmd-params-cm` (empty), `argocd-gpg-keys-cm`,
  `argocd-notifications-cm`, `argocd-rbac-cm`, `argocd-ssh-known-hosts-cm`, `argocd-tls-certs-cm`; Secrets
  `argocd-secret` (empty, populated at runtime), `argocd-notifications-secret`; StatefulSet
  `argocd-application-controller` (replicas 1); Deployments `argocd-applicationset-controller`, `argocd-dex-server`,
  `argocd-notifications-controller`, `argocd-redis`, `argocd-repo-server`, `argocd-server`; Services
  `argocd-applicationset-controller`, `argocd-dex-server`, `argocd-metrics`, `argocd-notifications-controller-metrics`,
  `argocd-redis`, `argocd-repo-server`, `argocd-server`, `argocd-server-metrics`; 7 ServiceAccounts, 6 Roles/RoleBindings,
  3 ClusterRoles/ClusterRoleBindings, 7 NetworkPolicies. Images: `quay.io/argoproj/argocd:v3.5.5`,
  `public.ecr.aws/docker/library/redis:8.2.3-alpine`, `ghcr.io/dexidp/dex:v2.45.1`. All objects are namespace-less in
  the bundle (applied into the target namespace, conventionally `argocd`); ClusterRoleBindings reference ServiceAccounts
  in namespace `argocd`, so a different namespace requires patching `/subjects/0/namespace` of each ClusterRoleBinding.
- **Edge cases**: Installing into a namespace other than `argocd` without patching the ClusterRoleBindings leaves the
  controller/server without cluster permissions (permission errors at runtime). Applying with client-side `kubectl apply`
  fails on the ApplicationSet CRD (see OPS-006). The kustomization pins `newTag: v3.5.5`.
- **Configuration surface**: manifest files; kustomize `namespace:` + ClusterRoleBinding subject patch.
- **Exposure**: Install artifact only (no API/CLI/UI).
- **Tests required**:
  - Positive: `kubectl apply --server-side --force-conflicts -n argocd -f install.yaml` on an empty cluster yields all listed objects and all pods Ready; an Application targeting `https://kubernetes.default.svc` syncs.
  - Positive: inventory test asserting the exact kind/name set listed above (golden list).
  - Negative: install into `argocd2` without the ClusterRoleBinding patch → controller logs forbidden errors on cluster-wide list; with the patch it works.
  - Recovery: delete any Deployment pod → recreated and functional; re-apply of the bundle is idempotent.

### OPS-002 Namespace-scoped install bundle (`manifests/namespace-install.yaml`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/hack/update-manifests.sh#L111 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/namespace-install/kustomization.yaml#L4 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/application-controller-roles/argocd-application-controller-role.yaml#L9 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/installation.md#L27 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/installation.md#L39
- **Maturity**: GA
- **Semantics**: Same components as OPS-001 but **without CRDs and without any ClusterRole/ClusterRoleBinding**. CRDs must
  be installed separately (`kubectl apply --server-side --force-conflicts -k manifests/crds`). The controller Role only
  grants: `secrets,configmaps` get/list/watch; `applications,applicationsets,appprojects` full CRUD; `events`
  create/list; `deployments` get/list/watch — so the in-cluster destination can only manage Argo CD's own CRs in its own
  namespace; real deployments target external clusters registered with credentials (or the local cluster registered via
  `argocd cluster add <ctx> --in-cluster --namespace <ns>` with explicit credentials).
- **Edge cases**: Applying without CRDs → controller fails to start informers. An Application deploying namespaced
  workloads into another namespace of the local cluster through `https://kubernetes.default.svc` fails with RBAC
  forbidden (expected). Operators may add Roles bound to `argocd-application-controller`.
- **Configuration surface**: manifest file; extra RoleBindings for the controller ServiceAccount.
- **Exposure**: Install artifact.
- **Tests required**:
  - Positive: with CRDs pre-installed, all pods become Ready; an Application targeting a registered external cluster syncs.
  - Negative: assert no ClusterRole/ClusterRoleBinding/CRD in the bundle (golden diff vs OPS-001).
  - Negative: Application deploying a Deployment into another namespace of the local cluster → sync error containing "forbidden".
  - Authorization: controller SA cannot list cluster-scoped resources (SubjectAccessReview check).

### OPS-003 Core (headless) install bundle (`manifests/core-install.yaml`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/core-install/kustomization.yaml#L4 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/hack/update-manifests.sh#L120 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/core.md#L10 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/core.md#L46 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/installation.md#L57
- **Maturity**: GA
- **Semantics**: Built from `crds` + `cluster-rbac/application-controller` + `base/config` + application-controller +
  applicationset-controller + repo-server + redis. No `argocd-server`, no Dex, no notifications controller, no
  `argocd-notifications-cm/secret`, no server/appset ClusterRoles (only the controller ClusterRole). Users interact via
  Kubernetes (Application/ApplicationSet CRs) or the CLI in core mode (OPS-122) / `argocd admin dashboard` (OPS-123).
  Not available: Argo CD RBAC model, Argo CD API, notifications, OIDC authentication; UI/CLI partially available.
  Redis is included because the controller relies on it for caching (controller can technically run without it but it
  is not recommended).
- **Edge cases**: `argocd-cmd-params-cm`, `argocd-rbac-cm` etc. are still shipped (base/config). The appset controller
  has no ClusterRole in core-install, so ApplicationSets in other namespaces are not supported out of the box.
- **Configuration surface**: manifest file.
- **Exposure**: Install artifact; CLI `--core`.
- **Tests required**:
  - Positive: golden inventory (ClusterRole only `argocd-application-controller`; no `argocd-server` Deployment).
  - Positive: an Application CR applied with kubectl is reconciled and synced.
  - Negative: no Service exposing an API (port 8080/443) exists.
  - Positive: `argocd app list --core` works against this install (OPS-122).

### OPS-004 High-availability install bundles (`manifests/ha/install.yaml`, `manifests/ha/namespace-install.yaml`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/ha/base/kustomization.yaml#L5 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/ha/base/overlays/argocd-repo-server-deployment.yaml#L6 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/ha/base/overlays/argocd-server-deployment.yaml#L6 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/ha/base/overlays/argocd-server-deployment.yaml#L26 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/ha/base/overlays/argocd-cmd-params-cm.yaml#L6 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/ha/cluster-install/kustomization.yaml#L4 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/high_availability.md#L6 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/installation.md#L47
- **Maturity**: GA
- **Semantics**: Same component set as the non-HA bundles, except: `argocd-repo-server` replicas 2 and `argocd-server`
  replicas 2, each with **required** pod anti-affinity on `kubernetes.io/hostname` and preferred anti-affinity on
  `topology.kubernetes.io/zone`; `argocd-server` sets `ARGOCD_API_SERVER_REPLICAS=2`; `redis.server` set to
  `argocd-redis-ha-haproxy:6379` in `argocd-cmd-params-cm`; the single `argocd-redis` Deployment is replaced by the
  Redis HA set (OPS-052: StatefulSet `argocd-redis-ha-server` ×3, Deployment `argocd-redis-ha-haproxy` ×3, Services
  `argocd-redis-ha`, `argocd-redis-ha-announce-{0,1,2}`, `argocd-redis-ha-haproxy`, ConfigMaps
  `argocd-redis-ha-configmap`, `argocd-redis-ha-health-configmap`, Roles/SAs `argocd-redis-ha`,
  `argocd-redis-ha-haproxy`). The application-controller StatefulSet stays at 1 replica (sharding is opt-in, OPS-040).
  `ha/namespace-install.yaml` omits CRDs and ClusterRoles exactly as OPS-002.
- **Edge cases**: Requires at least 3 schedulable nodes (required anti-affinity); IPv6-only clusters are not supported.
  ApplicationSet controller and Dex remain single-replica.
- **Configuration surface**: manifest files.
- **Exposure**: Install artifact.
- **Tests required**:
  - Positive: on a 3-node cluster all HA pods Ready; killing one Redis HA server pod does not break reconciliation.
  - Negative: on a 1-node cluster the second repo-server/server replica stays Pending (anti-affinity), first replica serves.
  - Positive: golden inventory and replica counts (server 2, repo-server 2, redis-ha-server 3, haproxy 3, controller 1).

### OPS-005 Source-hydrator install variants (`*-with-hydrator`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/cluster-install-with-hydrator/kustomization.yaml#L1 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/namespace-install-with-hydrator/kustomization.yaml#L1 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/core-install-with-hydrator/kustomization.yaml#L1 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/ha/install-with-hydrator/kustomization.yaml#L1 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/ha/cluster-install-with-hydrator/kustomization.yaml#L1 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/hack/update-manifests.sh#L123 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/commit-server/argocd-commit-server-deployment.yaml#L27
- **Maturity**: Beta (Source Hydrator)
- **Semantics**: `install-with-hydrator.yaml`, `namespace-install-with-hydrator.yaml`, `core-install-with-hydrator.yaml`
  = corresponding bundle + `base/commit-server` (Deployment/Service/SA/NetworkPolicy `argocd-commit-server`, ports
  8086 gRPC / 8087 metrics) + a JSON patch setting `argocd-cmd-params-cm` `data: {"hydrator.enabled": "true"}`.
  `ha/install-with-hydrator.yaml` and `ha/namespace-install-with-hydrator.yaml` add only the commit-server; they do
  **not** set `hydrator.enabled`.
- **Edge cases**: The non-HA hydrator patch uses `op: add` on `/data`, replacing the whole (empty) data map. In HA hydrator
  bundles the controller/server run with the hydrator disabled until the operator sets `hydrator.enabled: "true"`.
- **Configuration surface**: `argocd-cmd-params-cm` `hydrator.enabled`, `commit.server`, `commitserver.*` (OPS-024).
- **Exposure**: Install artifact.
- **Tests required**:
  - Positive: `install-with-hydrator.yaml` contains commit-server objects and `hydrator.enabled: "true"`.
  - Positive: `ha/install-with-hydrator.yaml` contains commit-server and does not contain a `hydrator.enabled` data value (parity of the quirk).
  - Positive: controller connects to `argocd-commit-server:8086` when hydrator enabled.

### OPS-006 CRD bundle and server-side-apply installation requirement
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/crds/kustomization.yaml#L4 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/3.2-3.3.md#L5 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/3.2-3.3.md#L13 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/core.md#L61
- **Maturity**: GA
- **Semantics**: `manifests/crds` contains `application-crd.yaml`, `appproject-crd.yaml`, `applicationset-crd.yaml`.
  Since 3.3 the ApplicationSet CRD exceeds the 262144-byte `last-applied-configuration` annotation limit, so installs
  and upgrades must use server-side apply with conflict resolution (`kubectl apply --server-side --force-conflicts`).
  When Argo CD manages itself, the managing Application needs `syncOptions: [ServerSideApply=true]` (conflict
  resolution is then added automatically). Helm upgrades are unaffected.
- **Edge cases**: Client-side apply error: `The CustomResourceDefinition "applicationsets.argoproj.io" is invalid:
  metadata.annotations: Too long: may not be more than 262144 bytes`. `--force-conflicts` overwrites operator edits to
  fields the manifests define (affinity, env, probes) but preserves fields they do not define (resources, tolerations).
  Users who set `ClientSideApplyMigration=false` as a 3.3.0/3.3.1 workaround should remove it.
- **Configuration surface**: kubectl apply mode; Application sync option `ServerSideApply=true`.
- **Exposure**: Install artifact.
- **Tests required**:
  - Positive: server-side apply of each bundle succeeds on a fresh cluster and on top of the previous minor's install.
  - Negative: client-side apply of the ApplicationSet CRD fails with the "Too long" annotation error (document Patrol's equivalent if its CRD is smaller).
  - Positive: self-managed upgrade via an Application with `ServerSideApply=true` succeeds.

### OPS-007 Cluster-scope RBAC bundle (`manifests/cluster-rbac`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/cluster-rbac/kustomization.yaml#L4 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/cluster-rbac/application-controller/argocd-application-controller-clusterrole.yaml#L9 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/cluster-rbac/server/argocd-server-clusterrole.yaml#L9 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/cluster-rbac/applicationset-controller/argocd-applicationset-controller-clusterrole.yaml#L9 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/security.md#L143
- **Maturity**: GA
- **Semantics**: Three ClusterRoles + ClusterRoleBindings to the namespace ServiceAccounts:
  - `argocd-application-controller`: `*/*/*` on all API groups/resources plus `*` on all nonResourceURLs (cluster-admin equivalent).
  - `argocd-server`: `delete,get,patch` on `*/*` (UI live-object delete/view, `argocd app patch`); `list` on core `events`;
    `get` on `pods`, `pods/log`; `get,list,watch` on `applications`,`applicationsets`; `create` on `batch/jobs` and
    `argoproj.io/workflows` (UI-triggered actions).
  - `argocd-applicationset-controller`: CRUD+watch on `applications`,`applicationsets`,`applicationsets/finalizers`;
    `get,list,watch` on `appprojects`; `get,patch,update` on `applicationsets/status`; `create,get,list,patch,watch` on
    `events`; `get,list,watch` on `secrets`,`configmaps`; `create` on `coordination.k8s.io/leases` and
    `get,update,create` on lease `58ac56fa.applicationsets.argoproj.io` (leader election).
  Operators may narrow write privileges by editing the ClusterRoles (read across the cluster is required).
- **Edge cases**: The server ClusterRole does not include `create`/`update` on arbitrary resources, so resource actions
  that create objects other than Jobs/Workflows on the local cluster fail with forbidden unless the role is extended.
- **Configuration surface**: ClusterRole rules.
- **Exposure**: Install artifact.
- **Tests required**:
  - Positive: golden rule sets for all three ClusterRoles.
  - Authorization: SubjectAccessReview — server SA can `delete` an arbitrary Deployment but cannot `create` a ConfigMap cluster-wide; appset SA cannot `delete` secrets.
  - Positive: narrowing controller ClusterRole to read-only + namespaced write still allows reconciliation of apps in the allowed namespaces.

### OPS-008 Namespaced component Roles, RoleBindings and ServiceAccounts
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/application-controller-roles/argocd-application-controller-role.yaml#L9 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/server/argocd-server-role.yaml#L9 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/applicationset-controller/argocd-applicationset-controller-role.yaml#L9 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/dex/argocd-dex-server-role.yaml#L9 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/notification/argocd-notifications-controller-role.yaml#L9 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/redis/argocd-redis-role.yaml#L9
- **Maturity**: GA
- **Semantics**: Namespace Roles (bound to same-named SAs):
  - controller: `secrets,configmaps` get/list/watch; `applications,applicationsets,appprojects` create/get/list/watch/update/patch/delete; `events` create/list; `apps/deployments` get/list/watch (dynamic distribution).
  - server: `secrets,configmaps` full CRUD+watch; `applications,appprojects,applicationsets` full CRUD+watch; `events` create/list.
  - applicationset-controller: same rules as its ClusterRole (incl. leases).
  - dex-server: `secrets,configmaps` get/list/watch.
  - notifications-controller: `applications,appprojects` get/list/watch/update/patch; `configmaps,secrets` list/watch; `get` only on `configmaps/argocd-notifications-cm` and `secrets/argocd-notifications-secret`.
  - redis: `get` on `secrets/argocd-redis`; `create` on `secrets` (used by the `redis-initial-password` init container).
  - repo-server and commit-server: ServiceAccounts without Roles (no Kubernetes API privileges).
- **Edge cases**: repo-server has no K8s privileges by design (security property; OPS-163). Notifications controller can
  only read its own named ConfigMap/Secret.
- **Configuration surface**: Role rules.
- **Exposure**: Install artifact.
- **Tests required**:
  - Positive: golden rule sets.
  - Authorization: repo-server SA token cannot `get` any Secret; notifications SA cannot `get` `argocd-secret`.
  - Positive: redis init container can create `argocd-redis` on first start.

### OPS-009 Component Services and port map
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L74 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/server/argocd-server-service.yaml#L1 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/repo-server/argocd-repo-server-service.yaml#L1 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/application-controller/argocd-metrics.yaml#L1 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/dex/argocd-dex-server-service.yaml#L1 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-notification/commands/argocd_notification.go#L36 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-applicationset-controller/commands/applicationset_controller.go#L294 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/ingress.md#L3
- **Maturity**: GA
- **Semantics**: Default listeners and Services:
  | Component | Container ports | Service(s) |
  |---|---|---|
  | argocd-server | 8080 (gRPC+HTTPS+HTTP mux), 8083 metrics | `argocd-server` 80→8080 (`http`), 443→8080 (`https`); `argocd-server-metrics` 8083 |
  | argocd-repo-server | 8081 gRPC, 8084 metrics/health | `argocd-repo-server` 8081 (`server`), 8084 (`metrics`) |
  | argocd-application-controller | 8082 metrics/health | `argocd-metrics` 8082 |
  | argocd-applicationset-controller | 7000 webhook, 8080 metrics, 8081 probes | `argocd-applicationset-controller` 7000 (`webhook`), 8080 (`metrics`) |
  | argocd-notifications-controller | 9001 metrics | `argocd-notifications-controller-metrics` 9001 |
  | argocd-dex-server | 5556 http(s), 5557 grpc, 5558 metrics | `argocd-dex-server` 5556/5557/5558 |
  | argocd-redis | 6379 | `argocd-redis` 6379 (`tcp-redis`) |
  | argocd-commit-server | 8086 gRPC, 8087 metrics/health | `argocd-commit-server` 8086/8087 |
  | argocd-redis-ha-haproxy (HA) | 6379, 9101 exporter | `argocd-redis-ha-haproxy` 6379 (`tcp-haproxy`), 9101 (`http-exporter-port`) |
  The dynamic-distribution controller Deployment variant adds Service `argocd-application-controller` 8082 and 8084.
- **Edge cases**: The controller metrics listener always binds `0.0.0.0:<metrics-port>` (no address flag). The API
  server's HTTP port 80 maps to the same 8080 container port; non-TLS HTTP/1.1 requests are redirected to HTTPS when TLS
  is enabled (OPS-098).
- **Configuration surface**: `--port`, `--metrics-port`, `--address`, `--metrics-address` flags per component (OPS-145..153).
- **Exposure**: Install artifact; network.
- **Tests required**:
  - Positive: each Service port reaches the intended listener (gRPC health / `/metrics` / `/healthz`).
  - Positive: `curl http://argocd-server/` returns 307 to `https://...` when TLS enabled; with `--insecure` returns the UI.
  - Negative: controller has no listener other than 8082.

### OPS-010 NetworkPolicies
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/repo-server/argocd-repo-server-network-policy.yaml#L1 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/redis/argocd-redis-network-policy.yaml#L1 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/dex/argocd-dex-server-network-policy.yaml#L1 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/server/argocd-server-network-policy.yaml#L1 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/commit-server/argocd-commit-server-network-policy.yaml#L1 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/ha/base/redis-ha/argocd-redis-ha-proxy-network-policy.yaml#L1 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/ha/base/redis-ha/argocd-redis-ha-server-network-policy.yaml#L1
- **Maturity**: GA
- **Semantics**: Ingress-only policies (pod selectors by `app.kubernetes.io/name`):
  - application-controller: 8082 from any namespace.
  - applicationset-controller: 7000 and 8080 from any namespace.
  - commit-server: 8086 only from `argocd-application-controller` pods; 8087 from any namespace.
  - dex-server: 5556, 5557 only from `argocd-server`; 5558 from any namespace.
  - notifications-controller: 9001 from any namespace.
  - redis: 6379 only from `argocd-server`, `argocd-repo-server`, `argocd-application-controller`.
  - repo-server: 8081 only from server, application-controller, notifications-controller, applicationset-controller; 8084 from any namespace.
  - server: all ingress allowed (`- {}`).
  - HA haproxy: 6379, 26379 from server/repo-server/controller; 9101 from any namespace. HA redis server: ingress 6379/26379 from haproxy and redis-ha pods; egress to redis-ha pods 6379/26379 and DNS 53 TCP/UDP.
- **Edge cases**: Pods with non-default `app.kubernetes.io/name` labels (renamed installs) are blocked. CMP sidecars run
  inside the repo-server pod so are unaffected. Egress is unrestricted except for redis-ha.
- **Configuration surface**: NetworkPolicy objects.
- **Exposure**: Install artifact.
- **Tests required**:
  - Positive: golden policy set.
  - Negative (with a NetworkPolicy-enforcing CNI): a pod labelled differently cannot reach repo-server 8081 or redis 6379; can reach 8084 metrics.
  - Positive: all components still function with policies enforced.

### OPS-011 Component health endpoints and probes
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/healthz/healthz.go#L13 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L436 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-repo-server/commands/argocd_repo_server.go#L186 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-commit-server/commands/argocd_commit_server.go#L31 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L272 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/server/argocd-server-deployment.yaml#L411 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/repo-server/argocd-repo-server-deployment.yaml#L287 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/application-controller/argocd-application-controller-statefulset.yaml#L362
- **Maturity**: GA
- **Semantics**: `GET /healthz` returns `200 ok\n` or `503` (error logged with `component=healthcheck`).
  - API server (port 8080, also under root path): 503 "API Server is terminating and unable to serve requests" while
    shutting down; 503 "API Server is not available: it either hasn't started or is restarting" before `available`;
    `?full=true` additionally lists clusters and fails only if the error contains the "not an object" message.
    Probes: liveness `/healthz?full=true` (period 30s, timeout 5s), readiness `/healthz` (period 30s).
  - Repo-server (metrics port 8084): `/healthz` always ok; `?full=true` dials its own gRPC endpoint (with an ephemeral
    client cert under mTLS) and requires gRPC health `SERVING`. Liveness `?full=true` initialDelay 30s period 30s
    failureThreshold 3; readiness `/healthz` initialDelay 5s period 10s.
  - Controller (8082): readiness `/healthz` (initialDelay 5s, period 10s); with dynamic distribution the readiness check
    also performs the shard heartbeat (OPS-045) and fails when the Deployment replicas ≤ 0 or the shard ConfigMap
    update fails.
  - Commit-server (8087): same `?full=true` self-dial semantics as repo-server.
  - ApplicationSet controller: controller-runtime probes on `:8081` (`--probe-addr`).
- **Edge cases**: The API server health handler is also mounted inside the root-path mux so `/healthz` works with and
  without `--rootpath`. Repo-server full health check uses a 60s connection timeout.
- **Configuration surface**: probe definitions; `?full=true` query.
- **Exposure**: HTTP endpoints.
- **Tests required**:
  - Positive: each endpoint returns `ok`.
  - Negative: stop repo-server gRPC (simulate) → `?full=true` returns 503 while plain `/healthz` returns 200.
  - Recovery: API server returns 503 during a settings-triggered restart (OPS-095/098) and 200 afterwards.

### OPS-012 Settings object discovery (namespace and `app.kubernetes.io/part-of=argocd` label)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L536 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L784 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L1449 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L830 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L51
- **Maturity**: GA
- **Semantics**: All components read settings from the namespace they run in (`--namespace`/kubeconfig namespace).
  The settings manager's ConfigMap informer is filtered by label `app.kubernetes.io/part-of=argocd` (resync 3m), so
  `argocd-cm`, `argocd-rbac-cm`, `argocd-ssh-known-hosts-cm`, `argocd-tls-certs-cm`, `argocd-gpg-keys-cm` must carry this
  label to be visible. Secrets are watched with selector `argocd.argoproj.io/secret-type!=cluster` and `getSecrets()`
  additionally filters by the part-of label (these provide `$<secret>:<key>` references). Well-known names: `argocd-cm`,
  `argocd-secret`, `argocd-notifications-cm`, `argocd-notifications-secret`, `argocd-rbac-cm`,
  `argocd-ssh-known-hosts-cm`, `argocd-tls-certs-cm`, `argocd-gpg-keys-cm`, `argocd-app-controller-shard-cm`,
  `argocd-cmd-params-cm`. Changes propagate to subscribers without restart except where noted (OPS-017, OPS-095).
- **Edge cases**: An `argocd-cm` without the label is treated as missing (NotFound). `argocd-cmd-params-cm` is consumed
  only through env vars at pod start, so its label is irrelevant.
- **Configuration surface**: label `app.kubernetes.io/part-of: argocd`.
- **Exposure**: Kubernetes objects.
- **Tests required**:
  - Negative: remove the label from `argocd-cm` → settings reads fail/defaults apply; re-add → settings picked up without restart.
  - Positive: a Secret with the label is resolvable via `$name:key` in `dex.config`; without the label it is not.

### OPS-013 Redis password bootstrap (`argocd-redis` Secret, `redis-initial-password` init container)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/redis/argocd-redis-deployment.yaml#L18 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/redis_initial_password.go#L38 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L494 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/application-controller/argocd-application-controller-statefulset.yaml#L24 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/ha/base/redis-ha/chart/values.yaml#L2
- **Maturity**: GA
- **Semantics**: The Redis Deployment runs init container `argocd admin redis-initial-password` which creates Secret
  `argocd-redis` with key `auth` = 16 random chars from `[0-9A-Za-z-]` if absent (AlreadyExists tolerated), then
  verifies the key exists. Redis runs with `--save "" --appendonly no --requirepass $(REDIS_PASSWORD)`. Controller, server
  and repo-server read `REDIS_PASSWORD` from `argocd-redis/auth` (non-optional secretKeyRef). Redis HA chart uses
  `existingSecret: argocd-redis`, `authKey: auth`.
- **Edge cases**: If the Secret exists without key `auth`, the init container exits fatally ("key auth doesn't exist in
  secret argocd-redis"). Components fail to start until the Secret exists (secretKeyRef not optional).
- **Configuration surface**: Secret `argocd-redis` key `auth`; env `REDIS_PASSWORD`.
- **Exposure**: CLI `argocd admin redis-initial-password` (OPS-141); install artifact.
- **Tests required**:
  - Positive: fresh install creates `argocd-redis` with a 16-char password; Redis rejects unauthenticated clients.
  - Positive: idempotent — second run leaves the existing password unchanged.
  - Negative: Secret present without `auth` → init container fails with the message above.

### OPS-014 Custom tooling in the repo-server (volume-mounted binaries / BYOI)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/custom_tools.md#L1 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/custom_tools.md#L15 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/custom_tools.md#L47 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cm.yaml#L263
- **Maturity**: GA
- **Semantics**: The repo-server is the only component that executes config-management tools; operators can replace
  or add binaries either by an init container copying binaries into an `emptyDir` mounted over the binary path (e.g.
  `subPath: helm` → `/usr/local/bin/helm`) or by building a derived image (switch to root, install, switch back to
  `$ARGOCD_USER_ID`). Additional Kustomize versions are referenced via `kustomize.path.<version>` /
  `kustomize.version.<version>` in argocd-cm (owner: 02).
- **Edge cases**: Pod runs with `readOnlyRootFilesystem: true`, so tools must be mounted, not downloaded at runtime.
- **Configuration surface**: repo-server pod spec (volumes/initContainers/image); argocd-cm kustomize keys.
- **Exposure**: Deployment customisation only.
- **Tests required**:
  - Positive: override `helm` via init-container volume mount; `helm version` used by manifest generation reports the overridden binary.
  - Positive: derived image with an extra tool used by a CMP/kustomize generator works.

### OPS-015 Air-gapped operation prerequisites
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/kustomization.yaml#L5 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L1416 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cm.yaml#L34 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cm.yaml#L43
- **Maturity**: GA
- **Semantics**: Facts relevant to disconnected installs (no dedicated upstream doc exists): the bundles reference four
  images (`quay.io/argoproj/argocd:v3.5.5`, `public.ecr.aws/docker/library/redis:8.2.3-alpine`,
  `ghcr.io/dexidp/dex:v2.45.1`, HA: `public.ecr.aws/docker/library/haproxy:3.0.8-alpine`) which can be re-pointed with
  kustomize `images:`; the CLI binary is served by the API server itself at `/download/argocd-linux` and
  `/download/argocd-linux-<arch>` (OPS-112); additional platform download links are configured with
  `help.download.<os>-<arch>` (owner: UI area); Google Analytics is only contacted when `ga.trackingid` is set; Helm
  repository index, OCI registries and Git hosts are only contacted as configured.
- **Edge cases**: `imagePullPolicy: Always` on several containers requires a reachable mirror.
- **Configuration surface**: kustomize image overrides; `help.download.*`; `ga.trackingid`.
- **Exposure**: Install artifact; HTTP `/download`.
- **Tests required**:
  - Positive: install with all images re-tagged to a local registry and no egress; UI, API and an Application from an in-cluster Git server work.
  - Positive: `/download/argocd-linux` serves the binary without internet access.

### OPS-016 Tested Kubernetes versions
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/tested-kubernetes-versions.md#L3 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/installation.md#L122
- **Maturity**: GA
- **Semantics**: Argo CD 3.5 is tested against Kubernetes v1.36, v1.35, v1.34, v1.33 (3.4: v1.35–v1.32; 3.3: v1.35–v1.32).
- **Edge cases**: None documented for older versions (untested, not blocked by code).
- **Configuration surface**: None.
- **Exposure**: Documentation.
- **Tests required**:
  - Positive: Patrol e2e matrix runs on Kubernetes 1.33, 1.34, 1.35 and 1.36.

---

## B. Configuration surfaces

### OPS-017 `argocd-cmd-params-cm` mechanism
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/server-commands/additional-configuration-method.md#L1 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cmd-params-cm.yaml#L1 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/config/argocd-cmd-params-cm.yaml#L1 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/application-controller/argocd-application-controller-statefulset.yaml#L29 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/env/env.go#L18 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/env/env.go#L123
- **Maturity**: GA
- **Semantics**: Component flags can be configured through `argocd-cmd-params-cm` keys. Each key is mapped in the
  shipped manifests to an env var via `configMapKeyRef{name: argocd-cmd-params-cm, key: <key>, optional: true}`; the
  binary's flag default reads that env var. Precedence: explicit CLI flag > env var (from ConfigMap or set directly) >
  compiled default. Prefix convention: `controller.` (application-controller), `server.`, `reposerver.`,
  `commitserver.`, `dexserver.`, `applicationsetcontroller.`, `notificationscontroller.`; un-prefixed keys are shared
  (OPS-018). Values are read only at process start → pods must be restarted after a change. Exception: the
  `*.profile.enabled` keys are projected as a file `/home/argocd/params/profiler.enabled` and read on every request
  (OPS-056). The shipped ConfigMap is empty (all defaults).
- **Edge cases**: Missing ConfigMap/key → env var unset → compiled default. Unparseable or out-of-range values log a
  warning ("Could not parse ...", "... less than minimum ... allowed") and fall back to the default instead of failing.
  Unknown keys are silently ignored. A key not wired in the manifests (e.g. documented-only keys) has no effect.
- **Configuration surface**: ConfigMap `argocd-cmd-params-cm`; component env vars.
- **Exposure**: ConfigMap.
- **Tests required**:
  - Positive: set `controller.status.processors: "7"`, restart → controller starts 7 refresh workers (observable via debug log or metrics).
  - Positive: CLI flag overrides the ConfigMap value.
  - Negative: `controller.status.processors: "abc"` → warning logged, default 20 used, pod healthy.
  - Negative: change a key without restart → no effect until restart.

### OPS-018 Shared (un-prefixed) cmd-params keys
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cmd-params-cm.yaml#L9 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cmd-params-cm.yaml#L41 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/cache/cache.go#L211 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L39
- **Maturity**: GA (`hydrator.enabled`: Beta; `application.namespaces`: Beta per doc comment)
- **Semantics**: Every key, env var(s) and default:
  | Key | Env var(s) / consumers | Default |
  |---|---|---|
  | `repo.server` | `ARGOCD_APPLICATION_CONTROLLER_REPO_SERVER`, `ARGOCD_SERVER_REPO_SERVER`, `ARGOCD_APPLICATIONSET_CONTROLLER_REPO_SERVER` | `argocd-repo-server:8081` |
  | `commit.server` | `ARGOCD_APPLICATION_CONTROLLER_COMMIT_SERVER` | `argocd-commit-server:8086` |
  | `redis.server` | `REDIS_SERVER` (controller, server, repo-server) | `argocd-redis:6379` (HA: `argocd-redis-ha-haproxy:6379`) |
  | `redis.compression` | `REDIS_COMPRESSION` | `gzip` (`gzip`\|`none`) |
  | `redis.db` | `REDISDB` | `0` |
  | `hydrator.enabled` | `ARGOCD_HYDRATOR_ENABLED` (controller, server) | `false` |
  | `otlp.address` / `otlp.insecure` / `otlp.headers` / `otlp.attrs` | `ARGOCD_{APPLICATION_CONTROLLER,SERVER,REPO_SERVER}_OTLP_{ADDRESS,INSECURE,HEADERS,ATTRS}` | `""` / `true` / `{}` / `[]` (OPS-085) |
  | `application.namespaces` | `ARGOCD_APPLICATION_NAMESPACES` (controller, server, notifications) | empty (only control-plane namespace); comma list of globs |
  | `log.format.timestamp` | `ARGOCD_LOG_FORMAT_TIMESTAMP` (controller, repo-server, appset, dex, notifications, commit-server) | `""` = RFC3339 |
  The notifications controller has no `--argocd-repo-server` env mapping (uses its compiled default, OPS-150).
- **Edge cases**: `redis.compression` invalid value → component fails at cache creation (`CompressionTypeFromString`
  error). `application.namespaces` must be identical across controller, server and notifications for consistent
  behaviour (owner of semantics: 07 / apps-in-any-namespace). The API server's `argocd-server` deployment does not map
  `log.format.timestamp`.
- **Configuration surface**: keys above.
- **Exposure**: ConfigMap.
- **Tests required**:
  - Positive: `redis.server` pointing to an alternate Redis is used by all three components (connections observed).
  - Positive: `redis.compression: none` stores uncompressed values (inspect Redis).
  - Negative: `redis.compression: zstd` → component exits with an error.
  - Positive: `log.format.timestamp: "2006-01-02"` changes JSON `time` format in repo-server logs.

### OPS-019 Application-controller cmd-params: processing and reconciliation keys
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cmd-params-cm.yaml#L59 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-application-controller/commands/argocd_application_controller.go#L259 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sync.go#L611
- **Maturity**: GA (`controller.sharding.algorithm` non-legacy values: Alpha)
- **Semantics**: Every key (env var → flag, default):
  | Key | Env var | Flag | Default | Owner |
  |---|---|---|---|---|
  | `controller.status.processors` | `ARGOCD_APPLICATION_CONTROLLER_STATUS_PROCESSORS` | `--status-processors` | 20 (min 0) | OPS-038 |
  | `controller.operation.processors` | `..._OPERATION_PROCESSORS` | `--operation-processors` | 10 (min 0) | OPS-038 |
  | `controller.hydration.processors` | `..._HYDRATION_PROCESSORS` | `--hydration-processors` | 5 (min 1; <1 forced to 1) | OPS-038 |
  | `controller.log.format` / `controller.log.level` | `..._LOGFORMAT` / `..._LOGLEVEL` | `--logformat` / `--loglevel` | `json` / `info` | OPS-086 |
  | `controller.metrics.cache.expiration` | `..._METRICS_CACHE_EXPIRATION` | `--metrics-cache-expiration` | `0` (disabled) | OPS-075 |
  | `controller.self.heal.timeout.seconds` | `..._SELF_HEAL_TIMEOUT_SECONDS` | `--self-heal-timeout-seconds` | 0 | 05 |
  | `controller.self.heal.backoff.timeout.seconds` | `..._SELF_HEAL_BACKOFF_TIMEOUT_SECONDS` | `--self-heal-backoff-timeout-seconds` | 2 (0 disables backoff) | 05 |
  | `controller.self.heal.backoff.factor` | `..._SELF_HEAL_BACKOFF_FACTOR` | `--self-heal-backoff-factor` | 3 | 05 |
  | `controller.self.heal.backoff.cap.seconds` | `..._SELF_HEAL_BACKOFF_CAP_SECONDS` | `--self-heal-backoff-cap-seconds` | 300 | 05 |
  | `controller.self.heal.backoff.cooldown.seconds` (manifest only) | `..._SELF_HEAL_BACKOFF_COOLDOWN_SECONDS` | `--self-heal-backoff-cooldown-seconds` (deprecated, no effect) | 330 | 05 |
  | `controller.sync.timeout.seconds` | `..._SYNC_TIMEOUT` | `--sync-timeout` | 0 (no timeout) | 05 |
  | `controller.sync.wave.delay.seconds` | `ARGOCD_SYNC_WAVE_DELAY` | — | 2 (non-final waves) | 05 |
  | `controller.resource.health.persist` | `..._PERSIST_RESOURCE_HEALTH` | `--persist-resource-health` | false | 04 |
  | `controller.sharding.algorithm` | `ARGOCD_CONTROLLER_SHARDING_ALGORITHM` | `--sharding-method` | `legacy` | OPS-041..043 |
  | `controller.kubectl.parallelism.limit` | `..._KUBECTL_PARALLELISM_LIMIT` | `--kubectl-parallelism-limit` | 20 (<1 = unlimited) | OPS-039 |
  | `controller.repo.error.grace.period.seconds` | `ARGOCD_REPO_ERROR_GRACE_PERIOD_SECONDS` (duration) | `--repo-error-grace-period-seconds` | 180 | 04 |
  | `controller.diff.server.side` | `..._SERVER_SIDE_DIFF` | `--server-side-diff-enabled` | false | 04 |
  | `controller.ignore.normalizer.jq.timeout` (manifest only) | `ARGOCD_IGNORE_NORMALIZER_JQ_TIMEOUT` | `--ignore-normalizer-jq-execution-timeout-seconds` | 0 → normalizer default | 04 |
  | `controller.profile.enabled` | file `profiler.enabled` | — | false | OPS-056 |
  | `controller.cluster.cache.batch.events.processing` | `ARGOCD_CLUSTER_CACHE_BATCH_EVENTS_PROCESSING` | — | true | OPS-049 |
  | `controller.cluster.cache.events.processing.interval` | `ARGOCD_CLUSTER_CACHE_EVENTS_PROCESSING_INTERVAL` | — | `100ms` | OPS-049 |
  (`...` = `ARGOCD_APPLICATION_CONTROLLER`.)
- **Edge cases**: `controller.status.processors: 0` starts no refresh workers (apps never reconcile) — allowed by the
  parser. `ARGOCD_SYNC_WAVE_DELAY` non-integer is ignored (default 2s). `controller.self.heal.backoff.timeout.seconds: 0`
  disables exponential backoff and falls back to the fixed `self.heal.timeout.seconds`.
- **Configuration surface**: keys above.
- **Exposure**: ConfigMap; controller flags.
- **Tests required**:
  - Positive: each key changes the corresponding flag value (unit test of env → flag binding, table-driven over all keys).
  - Negative: deprecated cooldown key set → no behavioural change, flag accepted.
  - Negative: `controller.sync.wave.delay.seconds: "x"` → 2s delay retained.

### OPS-020 Application-controller cmd-params: repo-server connection and caches
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cmd-params-cm.yaml#L47 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cmd-params-cm.yaml#L82 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-application-controller/commands/argocd_application_controller.go#L150 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/cache/appstate/cache.go#L34
- **Maturity**: GA
- **Semantics**:
  | Key | Env var | Flag | Default |
  |---|---|---|---|
  | `controller.repo.server.timeout.seconds` | `ARGOCD_APPLICATION_CONTROLLER_REPO_SERVER_TIMEOUT_SECONDS` | `--repo-server-timeout-seconds` | 60 |
  | `controller.repo.server.plaintext` | `..._REPO_SERVER_PLAINTEXT` | `--repo-server-plaintext` | false |
  | `controller.repo.server.strict.tls` | `..._REPO_SERVER_STRICT_TLS` | `--repo-server-strict-tls` (deprecated, hidden) | false |
  | `controller.repo.server.ca.cert.path` | `..._REPO_SERVER_CA_CERT_PATH` | `--repo-server-ca-cert-path` | `""` |
  | `controller.repo.server.client.cert.path` | `..._REPO_SERVER_CLIENT_CERT_PATH` | `--repo-server-client-cert-path` | `/app/config/reposerver/mtls/client.crt` |
  | `controller.repo.server.client.cert.key.path` | `..._REPO_SERVER_CLIENT_CERT_KEY_PATH` | `--repo-server-client-cert-key-path` | `/app/config/reposerver/mtls/client.key` |
  | `controller.app.state.cache.expiration` | `ARGOCD_APP_STATE_CACHE_EXPIRATION` | `--app-state-cache-expiration` | `1h` (max 10h) |
  | `controller.default.cache.expiration` | `ARGOCD_DEFAULT_CACHE_EXPIRATION` | `--default-cache-expiration` | `24h` |
  Repo-server RPCs exceeding the timeout fail with `context deadline exceeded` (reconcile error); TLS semantics in OPS-101/103.
- **Edge cases**: `controller.app.state.cache.expiration` above 10h is rejected → default 1h. Strict TLS without CA path
  loads `/app/config/controller/tls/{tls.crt,ca.crt}` (mounted from `argocd-repo-server-tls`) and exits fatally if
  unreadable.
- **Configuration surface**: keys above.
- **Exposure**: ConfigMap; controller flags.
- **Tests required**:
  - Positive: lower timeout (1s) with a slow plugin → app condition shows a deadline-exceeded comparison error.
  - Negative: `controller.app.state.cache.expiration: 11h` → warning, 1h used.
  - Positive: TLS combos covered in OPS-101/103.

### OPS-021 Kubernetes client tuning and gRPC TXT lookup keys (controller, server, appset, repo-server, commit-server)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cmd-params-cm.yaml#L94 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cmd-params-cm.yaml#L113 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cmd-params-cm.yaml#L216 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cmd-params-cm.yaml#L387 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/application-controller/argocd-application-controller-statefulset.yaml#L109 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/3.3-3.4.md#L47
- **Maturity**: GA
- **Semantics**: For prefix `P` ∈ {`controller`, `server`, `applicationsetcontroller`}:
  `P.k8s.client.qps` → `ARGOCD_K8S_CLIENT_QPS` (50); `P.k8s.client.burst` → `ARGOCD_K8S_CLIENT_BURST` (2×QPS = 100);
  `P.k8s.client.max.idle.connections` → `ARGOCD_K8S_CLIENT_MAX_IDLE_CONNECTIONS` (500);
  `P.k8s.tcp.timeout` → `ARGOCD_K8S_TCP_TIMEOUT` (30s); `P.k8s.tcp.keepalive` → `ARGOCD_K8S_TCP_KEEPALIVE` (30s);
  `P.k8s.tls.handshake.timeout` → `ARGOCD_K8S_TLS_HANDSHAKE_TIMEOUT` (10s); `P.k8s.tcp.idle.timeout` →
  `ARGOCD_K8S_TCP_IDLE_TIMEOUT` (5m). For `controller` and `server` only: `P.k8sclient.retry.max` →
  `ARGOCD_K8SCLIENT_RETRY_MAX` (0), `P.k8sclient.retry.base.backoff` → `ARGOCD_K8SCLIENT_RETRY_BASE_BACKOFF` (100 ms).
  For `controller`, `server`, `reposerver`, `commitserver`, `applicationsetcontroller`:
  `P.grpc.enable.txt.service.config` → `GRPC_ENABLE_TXT_SERVICE_CONFIG` (`false`; gRPC DNS TXT `_grpc_config.<host>`
  lookups disabled since 3.4). Semantics of the values: OPS-047/048.
- **Edge cases**: The same env var names are used by every component (the prefix only selects which pod gets it).
  Burst default is computed from the effective QPS (QPS 30 → burst 60) unless burst is set.
- **Configuration surface**: keys above.
- **Exposure**: ConfigMap.
- **Tests required**:
  - Positive: `controller.k8s.client.qps: "5"` → observable client-side throttling (`argocd_kubectl_rate_limiter_duration_seconds` grows).
  - Positive: `controller.grpc.enable.txt.service.config: "true"` sets `GRPC_ENABLE_TXT_SERVICE_CONFIG=true` in the pod env.

### OPS-022 API server cmd-params keys
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cmd-params-cm.yaml#L139 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-server/commands/argocd_server.go#L307 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/server/argocd-server-deployment.yaml#L37
- **Maturity**: GA (`server.enable.proxy.extension`: Beta)
- **Semantics**: Every key (env var, flag, default, owner):
  `server.listen.address` (`ARGOCD_SERVER_LISTEN_ADDRESS`, `--address`, `0.0.0.0`);
  `server.metrics.listen.address` (`ARGOCD_SERVER_METRICS_LISTEN_ADDRESS`, see quirk below, `0.0.0.0`);
  `server.insecure` (`ARGOCD_SERVER_INSECURE`, `--insecure`, false, OPS-098);
  `server.basehref` (`ARGOCD_SERVER_BASEHREF`, `--basehref`, `/`, OPS-106); `server.rootpath` (`ARGOCD_SERVER_ROOTPATH`,
  `--rootpath`, `""`, OPS-105); `server.staticassets` (`ARGOCD_SERVER_STATIC_ASSETS`, `--staticassets`, `/shared/app`,
  OPS-111); `server.k8sclient.retry.*` (OPS-021); `server.api.content.types` (`ARGOCD_API_CONTENT_TYPES`,
  `--api-content-types`, `application/json`, empty allowed, OPS-108); `server.webhook.parallelism.limit`
  (`ARGOCD_SERVER_WEBHOOK_PARALLELISM_LIMIT`, 50, range 1–1000, owner 03); `server.glob.cache.size`
  (`ARGOCD_SERVER_GLOB_CACHE_SIZE`, 10000, min 1, owner 07); `server.webhook.refresh.workers`
  (`ARGOCD_SERVER_WEBHOOK_REFRESH_WORKERS`, 20, range 1–1000, owner 03); `server.sync.replace.allowed`
  (`ARGOCD_SYNC_WITH_REPLACE_ALLOWED`, `--sync-with-replace-allowed`, true, owner 05); `server.log.format` /
  `server.log.level` (`ARGOCD_SERVER_LOGFORMAT` / `ARGOCD_SERVER_LOG_LEVEL`, json/info); `server.repo.server.timeout.seconds`
  (60), `server.repo.server.plaintext`, `server.repo.server.strict.tls` (deprecated), `server.repo.server.ca.cert.path`,
  `server.repo.server.client.cert.path`, `server.repo.server.client.cert.key.path` (`ARGOCD_SERVER_REPO_SERVER_*`,
  OPS-101/103); `server.dex.server` (`ARGOCD_SERVER_DEX_SERVER`, `argocd-dex-server:5556`), `server.dex.server.plaintext`,
  `server.dex.server.strict.tls` (OPS-102); `server.disable.auth` (`ARGOCD_SERVER_DISABLE_AUTH`, false, owner 07);
  `server.enable.gzip` (`ARGOCD_SERVER_ENABLE_GZIP`, true, OPS-110); `server.x.frame.options`
  (`ARGOCD_SERVER_X_FRAME_OPTIONS`, `sameorigin`, OPS-109); `server.tls.minversion` / `server.tls.maxversion` /
  `server.tls.ciphers` (`ARGOCD_TLS_MIN_VERSION` / `ARGOCD_TLS_MAX_VERSION` / `ARGOCD_TLS_CIPHERS`, 1.2 / 1.3 /
  `TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384`, OPS-097); `server.connection.status.cache.expiration`
  (`ARGOCD_SERVER_CONNECTION_STATUS_CACHE_EXPIRATION`, 1h); `server.oidc.cache.expiration`
  (`ARGOCD_SERVER_OIDC_CACHE_EXPIRATION`, 3m); `server.app.state.cache.expiration` (`ARGOCD_APP_STATE_CACHE_EXPIRATION`,
  1h); `server.default.cache.expiration` (`ARGOCD_DEFAULT_CACHE_EXPIRATION`, 24h); `server.enable.proxy.extension`
  (`ARGOCD_SERVER_ENABLE_PROXY_EXTENSION`, false, owner: extensions); `server.profile.enabled` (file, OPS-056);
  `server.k8s.*` and `server.grpc.enable.txt.service.config` (OPS-021). Manifest-only keys: OPS-028. The server pod
  also receives `applicationsetcontroller.enable.new.git.file.globbing`, `.scm.root.ca.path`, `.allowed.scm.providers`,
  `.enable.scm.providers`, `.enable.github.api.metrics` (used by the API server's ApplicationSet generate/preview
  endpoints, owner 06) and `hydrator.enabled`, `application.namespaces`, `otlp.*`, `redis.*`, `repo.server`.
- **Edge cases**: `server.metrics.listen.address` is broken in code: the env var value is used as the **flag name**
  (`StringVar(&metricsHost, env.StringFromEnv("ARGOCD_SERVER_METRICS_LISTEN_ADDRESS","metrics-address"), "0.0.0.0", …)`),
  and the metrics listener is created on `ListenHost` anyway, so this key never changes the bind address (Doc/impl
  differences). When both `--rootpath` and a different `--basehref` are set, a warning is logged and basehref is forced
  to rootpath.
- **Configuration surface**: keys above.
- **Exposure**: ConfigMap; server flags.
- **Tests required**:
  - Positive: table-driven env→flag binding test for every key.
  - Negative (parity of quirk): `server.metrics.listen.address: 127.0.0.1` → metrics still bound on the main listen host (document Patrol decision if fixed).
  - Positive: `server.api.content.types: ""` allows `text/plain` POST (with security warning log).

### OPS-023 Repo-server cmd-params keys
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cmd-params-cm.yaml#L242 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-repo-server/commands/argocd_repo_server.go#L255 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/repo-server/argocd-repo-server-deployment.yaml#L38 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/security.md#L252
- **Maturity**: GA
- **Semantics**: Every key (env var, default, owner):
  `reposerver.listen.address` (`ARGOCD_REPO_SERVER_LISTEN_ADDRESS`, `0.0.0.0`); `reposerver.metrics.listen.address`
  (`ARGOCD_REPO_SERVER_LISTEN_METRICS_ADDRESS` in the manifest, but the binary reads
  `ARGOCD_REPO_SERVER_METRICS_LISTEN_ADDRESS` — see Doc/implementation differences; `0.0.0.0`);
  `reposerver.log.format` / `.log.level` (`ARGOCD_REPO_SERVER_LOGFORMAT` / `_LOGLEVEL`, json/info);
  `reposerver.parallelism.limit` (`ARGOCD_REPO_SERVER_PARALLELISM_LIMIT`, compiled default 0 = unlimited; the doc
  example value `1` is not the default; OPS-035); `reposerver.disable.tls` (`ARGOCD_REPO_SERVER_DISABLE_TLS`, false);
  `reposerver.tls.minversion` / `.maxversion` / `.ciphers` (`ARGOCD_TLS_MIN_VERSION` etc., OPS-097);
  `reposerver.client.ca.path` (`ARGOCD_REPO_SERVER_CLIENT_CA_PATH`, `/app/config/reposerver/mtls/client-ca.crt`, OPS-103);
  `reposerver.repo.cache.expiration` (`ARGOCD_REPO_CACHE_EXPIRATION`, 24h, OPS-036); `reposerver.default.cache.expiration`
  (`ARGOCD_DEFAULT_CACHE_EXPIRATION`, 24h); `reposerver.max.combined.directory.manifests.size`
  (`ARGOCD_REPO_SERVER_MAX_COMBINED_DIRECTORY_MANIFESTS_SIZE`, `10M`, owner 02); `reposerver.plugin.tar.exclusions`
  (`ARGOCD_REPO_SERVER_PLUGIN_TAR_EXCLUSIONS`, `;`-separated globs, owner 02); `reposerver.plugin.use.manifest.generate.paths`
  (false, owner 02); `reposerver.allow.oob.symlinks` (false, owner 02); `reposerver.streamed.manifest.max.tar.size`
  (`100M`), `reposerver.streamed.manifest.max.extracted.size` (`1G`), `reposerver.oci.manifest.max.extracted.size` (`1G`),
  `reposerver.disable.oci.manifest.max.extracted.size` (false), `reposerver.oci.layer.media.types` (3 defaults) (owner 02);
  `reposerver.enable.git.submodule` (`ARGOCD_GIT_MODULES_ENABLED`, true, owner 02/03); `reposerver.git.lsremote.parallelism.limit`
  (`ARGOCD_GIT_LS_REMOTE_PARALLELISM_LIMIT`, 0 = unlimited, OPS-037); `reposerver.git.request.timeout`
  (`ARGOCD_GIT_REQUEST_TIMEOUT`, 15s, OPS-037); `reposerver.enable.builtin.git.config` (true, owner 03);
  `reposerver.include.hidden.directories` (false, owner 02); `reposerver.grpc.enable.txt.service.config` (OPS-021);
  `reposerver.profile.enabled` (OPS-056). Manifest-only keys: OPS-028. The repo-server also receives
  `timeout.reconciliation` from argocd-cm as `ARGOCD_RECONCILIATION_TIMEOUT` (revision cache TTL, OPS-036).
- **Edge cases**: Quantity strings are parsed with Kubernetes `resource.ParseQuantity`; invalid → process exits.
  `--client-ca-path` together with `--disable-tls` → startup error "--client-ca-path cannot be used when --disable-tls is
  enabled" (only when the path is non-empty; the default path is non-empty, so `reposerver.disable.tls: "true"`
  requires `reposerver.client.ca.path: ""`).
- **Configuration surface**: keys above.
- **Exposure**: ConfigMap; repo-server flags.
- **Tests required**:
  - Positive: env→flag binding table test for every key.
  - Negative: `reposerver.max.combined.directory.manifests.size: "abc"` → startup failure.
  - Negative: `reposerver.disable.tls: "true"` with default client CA path → startup error above; with `reposerver.client.ca.path: ""` → starts plaintext.

### OPS-024 Commit-server cmd-params keys
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cmd-params-cm.yaml#L307 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-commit-server/commands/argocd_commit_server.go#L110 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/commit-server/argocd-commit-server-deployment.yaml#L27
- **Maturity**: Beta (Source Hydrator)
- **Semantics**: `commitserver.listen.address` (`ARGOCD_COMMIT_SERVER_LISTEN_ADDRESS`, `0.0.0.0`, port 8086);
  `commitserver.log.format` / `.log.level` (`ARGOCD_COMMIT_SERVER_LOGFORMAT` / `_LOGLEVEL`, json/info);
  `commitserver.metrics.listen.address` (`ARGOCD_COMMIT_SERVER_METRICS_LISTEN_ADDRESS`, `0.0.0.0`, port 8087);
  `commitserver.grpc.enable.txt.service.config` (OPS-021). Also receives `log.format.timestamp`.
- **Edge cases**: No TLS/redis/otlp options exist for the commit-server (gRPC is plaintext, OPS-104).
- **Configuration surface**: keys above.
- **Exposure**: ConfigMap; commit-server flags.
- **Tests required**:
  - Positive: listen/metrics address keys take effect after restart.

### OPS-025 Dex server cmd-params keys
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cmd-params-cm.yaml#L321 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-dex/commands/argocd_dex.go#L144 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/dex/argocd-dex-server-deployment.yaml#L44
- **Maturity**: GA
- **Semantics**: `dexserver.log.format` / `dexserver.log.level` (`ARGOCD_DEX_SERVER_LOGFORMAT` / `_LOGLEVEL`, json/info);
  `dexserver.disable.tls` (`ARGOCD_DEX_SERVER_DISABLE_TLS`, false, OPS-100); `dexserver.connector.failure.continue`
  (`DEX_CONTINUE_ON_CONNECTOR_FAILURE`, doc default `"false"`, consumed by the Dex binary itself, owner 07).
- **Edge cases**: The log keys configure the `argocd-dex` wrapper; Dex's own log format is set via the generated config.
- **Configuration surface**: keys above.
- **Exposure**: ConfigMap.
- **Tests required**:
  - Positive: `dexserver.disable.tls: "true"` → Dex served over HTTP; server must use `server.dex.server.plaintext: "true"` (OPS-102).

### OPS-026 ApplicationSet controller cmd-params keys
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cmd-params-cm.yaml#L330 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-applicationset-controller/commands/applicationset_controller.go#L294 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/interface.go#L40 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/applicationset-controller/argocd-applicationset-controller-deployment.yaml#L30
- **Maturity**: GA (`applicationsetcontroller.enable.progressive.syncs`: Beta)
- **Semantics**: Every key → env var (`ARGOCD_APPLICATIONSET_CONTROLLER_` prefix abbreviated `A_`) → flag, default; semantics
  owned by `06-applicationset.md`: `enable.leader.election` (`A_ENABLE_LEADER_ELECTION`, `--enable-leader-election`,
  false, OPS-055); `policy` (`A_POLICY`, `--policy`, `""`; valid `sync`,`create-only`,`create-update`,`create-delete`);
  `enable.policy.override` (`A_ENABLE_POLICY_OVERRIDE`, default = `policy == ""`); `concurrent.reconciliations.max`
  (`A_CONCURRENT_RECONCILIATIONS`, 10, min 1); `enable.new.git.file.globbing` (false); `debug` (`A_DEBUG`, false, overrides
  loglevel); `log.format` / `log.level` (`A_LOGFORMAT` / `A_LOGLEVEL`); `dryrun` (`A_DRY_RUN`, false);
  `enable.git.submodule` (`ARGOCD_GIT_MODULES_ENABLED`, true); `enable.progressive.syncs` (false);
  `namespaces` (`A_NAMESPACES`, `--applicationset-namespaces`); `scm.root.ca.path`; `allowed.scm.providers`;
  `enable.scm.providers` (true); `webhook.parallelism.limit` (50, 1–1000); `requeue.after`
  (`A_REQUEUE_AFTER`, env only, default 3m, range 1s–8760h); `enable.tokenref.strict.mode` (`A_TOKENREF_STRICT_MODE`);
  `global.preserved.annotations` / `global.preserved.labels`; `enable.github.api.metrics` (false, OPS-081);
  `status.max.resources.count` (`A_MAX_RESOURCES_STATUS_COUNT`, 5000); `profile.enabled` (OPS-056);
  `k8s.*`, `grpc.enable.txt.service.config` (OPS-021); `repo.server.ca.cert.path`, `repo.server.client.cert.path`,
  `repo.server.client.cert.key.path` (OPS-101/103). Manifest-only: `repo.server.plaintext`, `repo.server.strict.tls`,
  `repo.server.timeout.seconds` (60) (OPS-028). Flags without keys: `--metrics-addr` (`:8080`), `--probe-addr`
  (`:8081`), `--webhook-addr` (`:7000`), `--scm-proxy-url`/`--scm-no-proxy` (`A_SCM_PROXY_URL`/`A_SCM_NO_PROXY`),
  `--cache-sync-period` (`A_CACHE_SYNC_PERIOD`, 10h, max 24h), `--concurrent-application-updates`
  (`A_CONCURRENT_APPLICATION_UPDATES`, 1, range 1–200), `--metrics-applicationset-labels`.
- **Edge cases**: Invalid `policy` → logs "Policy value can be: sync, create-only, create-update, create-delete, default
  value: sync" and exits 1. More than one watched namespace with SCM providers enabled and no allow-list → logs "When
  enabling applicationset in any namespace using applicationset-namespaces, you must either set
  --enable-scm-providers=false or specify --allowed-scm-providers" and exits 1.
- **Configuration surface**: keys above.
- **Exposure**: ConfigMap; appset flags.
- **Tests required**:
  - Positive: env→flag binding table test.
  - Negative: both exit conditions above.

### OPS-027 Notifications controller cmd-params keys
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cmd-params-cm.yaml#L419 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-notification/commands/argocd_notification.go#L179 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/notification/argocd-notifications-controller-deployment.yaml#L50
- **Maturity**: GA
- **Semantics**: `notificationscontroller.log.level` / `.log.format` (`ARGOCD_NOTIFICATIONS_CONTROLLER_LOGLEVEL` /
  `_LOGFORMAT`, info/json); `notificationscontroller.processors.count` (`ARGOCD_NOTIFICATION_CONTROLLER_PROCESSORS_COUNT`,
  1, min 1); `notificationscontroller.selfservice.enabled` (`ARGOCD_NOTIFICATION_CONTROLLER_SELF_SERVICE_NOTIFICATION_ENABLED`,
  false, owner: notifications); `notificationscontroller.repo.server.plaintext`
  (`ARGOCD_NOTIFICATION_CONTROLLER_REPO_SERVER_PLAINTEXT`); `notificationscontroller.repo.server.ca.cert.path`,
  `.client.cert.path`, `.client.cert.key.path` (`ARGOCD_NOTIFICATION_CONTROLLER_REPO_SERVER_*`, OPS-101/103). Also
  receives `log.format.timestamp` and `application.namespaces`.
- **Edge cases**: `log.format` other than json/text → `unknown log format '<v>'` and exit. The text format only applies a
  formatter when `FORCE_LOG_COLORS=1`; `log.format.timestamp` is not applied by this controller (it uses its own
  formatter setup).
- **Configuration surface**: keys above.
- **Exposure**: ConfigMap; notifications flags.
- **Tests required**:
  - Positive: `processors.count: "3"` → 3 workers.
  - Negative: `log.format: "xml"` → controller exits with the message above.

### OPS-028 Manifest-wired cmd-params keys missing from the reference document
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/application-controller/argocd-application-controller-statefulset.yaml#L43 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/server/argocd-server-deployment.yaml#L145 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/server/argocd-server-deployment.yaml#L259 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/repo-server/argocd-repo-server-deployment.yaml#L200 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/applicationset-controller/argocd-applicationset-controller-deployment.yaml#L172
- **Maturity**: GA
- **Semantics**: Keys consumed by the shipped manifests but absent from `argocd-cmd-params-cm.yaml` (all must be
  supported for parity): `controller.self.heal.backoff.cooldown.seconds` (deprecated no-op);
  `controller.ignore.normalizer.jq.timeout` (`ARGOCD_IGNORE_NORMALIZER_JQ_TIMEOUT`);
  `server.content.security.policy` (`ARGOCD_SERVER_CONTENT_SECURITY_POLICY`, default `frame-ancestors 'self';`, OPS-109);
  `server.http.cookie.maxnumber` (`ARGOCD_MAX_COOKIE_NUMBER`, default 20 cookie chunks for the auth token, owner 07);
  `reposerver.helm.manifest.max.extracted.size` (`ARGOCD_REPO_SERVER_HELM_MANIFEST_MAX_EXTRACTED_SIZE`, `1G`),
  `reposerver.disable.helm.manifest.max.extracted.size` (false), `reposerver.revision.cache.lock.timeout`
  (`ARGOCD_REVISION_CACHE_LOCK_TIMEOUT`, 10s, OPS-036), `reposerver.grpc.max.size` (`ARGOCD_GRPC_MAX_SIZE_MB`, OPS-054),
  `reposerver.helm.user.agent` (`ARGOCD_HELM_USER_AGENT`, owner 02); `applicationsetcontroller.repo.server.plaintext`,
  `applicationsetcontroller.repo.server.strict.tls`, `applicationsetcontroller.repo.server.timeout.seconds` (60). From
  `argocd-cm`: `timeout.reconciliation`, `timeout.reconciliation.jitter`, `timeout.hard.reconciliation` (OPS-058..060).
- **Edge cases**: These keys are optional refs — absent keys keep compiled defaults.
- **Configuration surface**: keys above.
- **Exposure**: ConfigMap.
- **Tests required**:
  - Positive: each key changes behaviour/flag value after restart (binding test).

### OPS-029 `argocd-cm` key register and area ownership
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cm.yaml#L9 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L423 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/accounts.go#L20 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L1695
- **Maturity**: GA (individual keys per owner)
- **Semantics**: Every key in the reference `argocd-cm.yaml` plus code-only keys, with owner:
  | Key(s) | Owner |
  |---|---|
  | `url`, `additionalUrls` | 07 (SSO/URLs; change restarts API server, OPS-095) |
  | `statusbadge.enabled`, `statusbadge.url` | UI/API (badge) |
  | `users.anonymous.enabled`, `users.session.duration`, `passwordPattern`, `admin.enabled`, `accounts.<name>`, `accounts.<name>.enabled` | 07 |
  | `ga.trackingid`, `ga.anonymizeusers`, `help.chatUrl`, `help.chatText`, `help.download.{linux-amd64,linux-arm64,linux-ppc64le,linux-s390x,darwin-amd64,darwin-arm64,windows-amd64}` | UI |
  | `dex.config`, `oidc.config`, `oidc.tls.insecure.skip.verify` | 07 |
  | `resource.customizations.{ignoreDifferences,health,actions,knownTypeFields}.<group_kind>`, `resource.customizations.ignoreDifferences.all`, `resource.customizations` (legacy single key), `resource.compareoptions`, `resource.sensitive.mask.annotations` | 04 / 05 (actions) |
  | `resource.ignoreResourceUpdatesEnabled`, `resource.customizations.ignoreResourceUpdates.{all,<group_kind>}` | **this file** (OPS-062..064) |
  | `resource.exclusions`, `resource.inclusions`, `resource.respectRBAC`, `resource.customLabels` | 04 |
  | `resource.includeEventLabelKeys`, `resource.excludeEventLabelKeys` | **this file** (OPS-092) |
  | `kustomize.enable`, `jsonnet.enable`, `helm.enable`, `kustomize.buildOptions`, `kustomize.path.<v>`, `kustomize.buildOptions.<v>`, `kustomize.version.<v>` (deprecated), `helm.valuesFileSchemes` | 02 |
  | `application.instanceLabelKey`, `application.resourceTrackingMethod`, `installationID` | 04 (tracking) |
  | `application.allowedNodeLabels`, `server.maxPodLogsToRender`, `exec.enabled`, `exec.shells` | UI / API (pod view, logs, terminal) |
  | `ui.cssurl`, `ui.bannercontent`, `ui.bannerurl`, `ui.bannerpermanent`, `ui.bannerposition` | UI |
  | `timeout.reconciliation`, `timeout.reconciliation.jitter`, `timeout.hard.reconciliation` (code/manifest only) | **this file** (OPS-058..060) |
  | `webhook.refresh.jitter`, `webhook.refresh.jitter.threshold`, `webhook.maxPayloadSizeMB` | 03 |
  | `cluster.inClusterEnabled` | 03 |
  | `project.links`, `application.links`, `resource.links` | UI (deep links) |
  | `extension.config`, `extension.config.<name>` (split form, code) | extensions |
  | `application.sync.impersonation.enabled`, `application.sync.impersonation.enforced`, `application.sync.requireOverridePrivilegeForRevisionSync` | 05 / 07 |
  | `commit.author.name`, `commit.author.email`, `sourceHydrator.commitMessageTemplate`, `sourceHydrator.readmeMessageTemplate` | source hydrator |
  | `globalProjects` (code only), `server.rbac.disableApplicationFineGrainedRBACInheritance` (code only) | 07 |
  All keys are read live through the settings manager (OPS-012) except `timeout.*` (env at pod start).
- **Edge cases**: The reference yaml omits `timeout.hard.reconciliation`, `globalProjects`,
  `server.rbac.disableApplicationFineGrainedRBACInheritance` and split `extension.config.<name>` keys.
- **Configuration surface**: ConfigMap `argocd-cm`.
- **Exposure**: ConfigMap; API `GET /api/v1/settings` exposes a subset (owner: API area).
- **Tests required**:
  - Positive: register completeness test — every key above is parsed by Patrol's settings package or explicitly marked "owned elsewhere"; no reference key is silently unknown.

### OPS-030 `argocd-secret` keys and runtime bootstrap
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-secret.yaml#L11 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L1722 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L2398 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/accounts.go#L27 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/config/argocd-secret.yaml#L1
- **Maturity**: GA
- **Semantics**: Keys: `tls.crt`/`tls.key` (API server cert; deprecated location, OPS-096); `admin.password` (bcrypt) and
  `admin.passwordMtime`; `admin.tokens` (code); `server.secretkey` (JWT signing key); webhook secrets
  `webhook.github.secret`, `webhook.gitlab.secret`, `webhook.bitbucket.uuid`, `webhook.bitbucketserver.secret`,
  `webhook.gogs.secret`, `webhook.azuredevops.username`, `webhook.azuredevops.password` (owner 03);
  `accounts.<name>.password`, `accounts.<name>.passwordMtime`, `accounts.<name>.tokens` (owner 07). Every data key of
  `argocd-secret` is referenceable as `$<key>` and every key of other part-of-labelled secrets as `$<secret>:<key>`.
  Bootstrap on API-server start (`InitializeSettings`): if admin is enabled and has no hash → generate a 16-char random
  password, store it in `argocd-initial-admin-secret` (OPS-031), store the bcrypt hash + mtime; if `server.secretkey`
  is missing → generate a 32-byte signature; if no certificate and not `--insecure` → generate a self-signed cert for
  `localhost`, `argocd-server`, `argocd-server.<ns>`, `argocd-server.<ns>.svc`, `argocd-server.<ns>.svc.cluster.local`
  (org "Argo CD") and persist it in `argocd-secret` (only when not externally managed).
- **Edge cases**: Missing `server.secretkey` reported as incomplete settings until bootstrapped. Concurrent replicas:
  an update conflict is treated as "initialized by another replica" (warning) and settings are re-read. With admin
  disabled, no password is generated. A change to any of the five main webhook secrets restarts the API server
  (OPS-095).
- **Configuration surface**: Secret `argocd-secret`.
- **Exposure**: Kubernetes Secret; API (indirect).
- **Tests required**:
  - Positive: fresh install → `argocd-secret` gains `server.secretkey`, `admin.password`, `admin.passwordMtime`, `tls.crt`, `tls.key`.
  - Positive: with `--insecure` no `tls.crt` is generated.
  - Positive: two server replicas starting simultaneously both succeed (conflict tolerated) and share one signature.
  - Negative: delete `server.secretkey` → regenerated on restart; existing sessions invalid.

### OPS-031 Initial admin password Secret and `argocd admin initial-password`
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L2400 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/initial_password.go#L19 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/commands/argocd_admin_initial-password.md#L1
- **Maturity**: GA
- **Semantics**: The generated initial password is written to Secret `argocd-initial-admin-secret` key `password`
  (create-or-update). `argocd admin initial-password` (kubeconfig access) prints the password followed by
  "This password must be only used for first time login. We strongly recommend you update the password using
  `argocd account update-password`."
- **Edge cases**: Secret missing → command fails with the Kubernetes NotFound error. Secret without `password` key →
  prints nothing, exits 0. The Secret is not deleted automatically after a password change.
- **Configuration surface**: Secret `argocd-initial-admin-secret`.
- **Exposure**: CLI `argocd admin initial-password`.
- **Tests required**:
  - Positive: after install, command prints a 16-char password that authenticates as `admin`.
  - Negative: Secret deleted → non-zero exit with NotFound.

### OPS-032 Environment-only tuning variables register
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L257 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/high_availability.md#L36 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/cache.go#L46 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L140
- **Maturity**: GA (dynamic distribution vars: Alpha)
- **Semantics**: Operationally relevant env vars with no cmd-params key (owner feature in parentheses):
  `ARGOCD_CONTROLLER_REPLICAS`, `ARGOCD_CONTROLLER_SHARD`, `ARGOCD_APPLICATION_CONTROLLER_NAME` (OPS-040/045);
  `ARGOCD_ENABLE_DYNAMIC_CLUSTER_DISTRIBUTION`, `ARGOCD_CONTROLLER_HEARTBEAT_TIME` (OPS-045);
  `ARGOCD_API_SERVER_REPLICAS`, `ARGOCD_MAX_CONCURRENT_LOGIN_REQUESTS_COUNT` (OPS-034);
  `ARGOCD_K8S_RETRY_COUNT`, `ARGOCD_K8S_RETRY_DURATION_MILLISECONDS` (OPS-048); `ARGOCD_K8S_SERVER_SIDE_TIMEOUT` (OPS-047);
  `WORKQUEUE_BUCKET_SIZE`, `WORKQUEUE_BUCKET_QPS`, `WORKQUEUE_FAILURE_COOLDOWN_NS`, `WORKQUEUE_BASE_DELAY_NS`,
  `WORKQUEUE_MAX_DELAY_NS`, `WORKQUEUE_BACKOFF_FACTOR` (OPS-046); `ARGOCD_CLUSTER_CACHE_*` (OPS-049);
  `ARGO_CD_UPDATE_CLUSTER_INFO_TIMEOUT` (OPS-050); `ARGOCD_APPLICATION_TREE_SHARD_SIZE` (OPS-053);
  `ARGOCD_GRPC_MAX_SIZE_MB`, `ARGOCD_GRPC_KEEP_ALIVE_MIN` (OPS-054); `ARGOCD_ENABLE_GRPC_TIME_HISTOGRAM` (OPS-079);
  `ARGOCD_EXEC_TIMEOUT`, `ARGOCD_EXEC_FATAL_TIMEOUT`, `ARGOCD_GIT_ATTEMPTS_COUNT`, `ARGOCD_GIT_RETRY_*`,
  `ARGOCD_PAUSE_GEN_AFTER_FAILED_ATTEMPTS`, `ARGOCD_PAUSE_GEN_MINUTES`, `ARGOCD_PAUSE_GEN_REQUESTS` (OPS-037);
  `ARGOCD_WEBHOOK_MANIFEST_CACHE_WARM_DISABLED` (owner 03); `ARGOCD_HELM_INDEX_CACHE_DURATION`,
  `ARGOCD_GITHUB_APP_CREDS_EXPIRATION_DURATION`, `ARGOCD_AZURE_SERVICE_PRINCIPAL_CREDS_EXPIRATION_DURATION` (owner 03);
  `ARGOCD_WATCH_API_BUFFER_SIZE` (1000, API watch buffer); `REDIS_USERNAME`, `REDIS_SENTINEL_USERNAME`,
  `REDIS_SENTINEL_PASSWORD`, `REDIS_RETRY_COUNT`, `REDIS_CREDS_DIR_PATH` (OPS-051); `ARGOCD_ENABLE_PROFILER_FILE_PATH`
  (OPS-056); `ARGOCD_LOG_FORMAT_ENABLE_FULL_TIMESTAMP`, `FORCE_LOG_COLORS` (OPS-087); `ARGOCD_RBAC_DEBUG`,
  `ARGOCD_SSO_DEBUG` (OPS-089); `ARGOCD_APP_CONF_PATH` (base path of mounted TLS material, default `/app/config`);
  `GOMEMLIMIT` (OPS-057); `KUBECACHEDIR` (`/tmp/kubecache`, set on the controller).
- **Edge cases**: See each owner feature.
- **Configuration surface**: container env.
- **Exposure**: Pod env.
- **Tests required**:
  - Positive: register test — Patrol documents the equivalent (or explicit non-support) of each variable.

---

## C. High availability and scaling

### OPS-033 HA topology requirements and per-component replica model
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/high_availability.md#L1 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/high_availability.md#L267 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/ha/base/overlays/argocd-repo-server-deployment.yaml#L9 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/application-controller/argocd-application-controller-statefulset.yaml#L400
- **Maturity**: GA
- **Semantics**: Argo CD is stateless apart from Kubernetes objects; Redis is a disposable cache that can be rebuilt
  without service disruption. Replica model: `argocd-server` and `argocd-repo-server` are freely horizontally scalable
  (stateless); the application controller scales only via sharding (OPS-040); the ApplicationSet controller is
  active/standby via leader election (OPS-055); Dex uses an in-memory store, so multiple instances may hold inconsistent
  data (run one); Redis HA is pre-configured for exactly three servers/sentinels. Non-HA pods carry preferred
  anti-affinity (weight 100 same-component on `kubernetes.io/hostname`, weight 5 any `part-of: argocd` pod).
- **Edge cases**: HA bundles need ≥3 nodes; IPv6-only clusters unsupported. Losing Redis only costs cache warm-up
  (manifests regenerated, tree recomputed).
- **Configuration surface**: replicas; affinity.
- **Exposure**: Install artifact.
- **Tests required**:
  - Recovery: delete Redis pod/data → reconciliation continues after cache rebuild; no Application state lost.
  - Recovery: kill one of two server replicas during UI/API use → requests succeed against the other.
  - Negative: two Dex replicas → document/enforce single replica (Patrol may differ; record).

### OPS-034 API server horizontal scaling (`ARGOCD_API_SERVER_REPLICAS`, login concurrency)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L140 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L172 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/high_availability.md#L230 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/high_availability.md#L255
- **Maturity**: GA
- **Semantics**: `argocd-server` is stateless; recommended ≥3 replicas for zero-downtime upgrades. Env
  `ARGOCD_MAX_CONCURRENT_LOGIN_REQUESTS_COUNT` (default 50, min 0; 0 = no limit) is divided by
  `ARGOCD_API_SERVER_REPLICAS` (default 1) using integer division to get the per-replica cap on concurrent login
  requests (brute-force protection; owner of login semantics: 07). `ARGOCD_GRPC_MAX_SIZE_MB` sizes API responses (OPS-054).
- **Edge cases**: Replicas > limit (e.g. 50/64) yields 0 per replica, i.e. unlimited. `ARGOCD_API_SERVER_REPLICAS=0` skips
  the division.
- **Configuration surface**: env `ARGOCD_API_SERVER_REPLICAS`, `ARGOCD_MAX_CONCURRENT_LOGIN_REQUESTS_COUNT`.
- **Exposure**: Pod env.
- **Tests required**:
  - Positive: with limit 2 and replicas 1, a third concurrent login is rejected (owner 07 asserts message); with replicas 2 the per-replica limit is 1.
  - Recovery: rolling restart of 3 replicas keeps the API available (continuous probe).

### OPS-035 Repo-server scaling and manifest-generation parallelism
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-repo-server/commands/argocd_repo_server.go#L257 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L136 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L684 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/high_availability.md#L15 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/high_availability.md#L272
- **Maturity**: GA
- **Semantics**: `--parallelismlimit` (`reposerver.parallelism.limit`, default 0 = unlimited) bounds concurrent manifest
  generations with a weighted semaphore (wait time observed in `argocd_repo_parallelism_wait_duration_seconds`,
  OPS-078) to avoid OOM/thread exhaustion from forked tools. Repositories are cloned under `/tmp` (or `$TMPDIR`; mount a
  volume for many/large repos). Generation that mutates the working copy is serialized per repository clone
  (`argocd_repo_pending_request_total`); generation without side effects runs concurrently: Helm (parallel by default
  since 3.0), plain directories, CMP sidecars, and CMP plugins whose app directory contains `.argocd-allow-concurrency`;
  Kustomize apps with parameter overrides in the same repo remain serialized. Fully qualified refs
  (`refs/heads/<b>`, `refs/tags/<t>`) avoid scanning all refs when resolving `targetRevision`. The
  `argocd.argoproj.io/manifest-generate-paths` annotation narrows cache invalidation (owner 02/03).
- **Edge cases**: Semaphore acquire honours request cancellation (wait still observed). Shallow clones disable the
  non-webhook manifest-generate-paths optimisation.
- **Configuration surface**: `reposerver.parallelism.limit`; `TMPDIR`; repo-server replicas.
- **Exposure**: ConfigMap; flags.
- **Tests required**:
  - Positive: limit 1 + two concurrent generations → second waits (wait histogram non-zero), both succeed.
  - Positive: two Helm apps in the same repo dir generate in parallel; two Kustomize apps with parameter overrides do not.
  - Negative: client cancels while waiting → request aborted, semaphore not leaked.

### OPS-036 Repo-server caching and expirations
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/cache/cache.go#L64 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/cache/cache.go#L233 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/high_availability.md#L41 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cm.yaml#L331
- **Maturity**: GA
- **Semantics**: Repo-server cache (Redis, key suffix `|<CacheVersion 1.8.3>`): `--repo-cache-expiration`
  (`ARGOCD_REPO_CACHE_EXPIRATION`, 24h) for app lists, app details, generated manifests, revision metadata;
  `--revision-cache-expiration` (env `ARGOCD_RECONCILIATION_TIMEOUT` = argocd-cm `timeout.reconciliation`, default 3m)
  for resolved symbolic revisions (`HEAD`, branches, tags); `--revision-cache-lock-timeout`
  (`ARGOCD_REVISION_CACHE_LOCK_TIMEOUT`, 10s; 0 disables) TTL of the lock that de-duplicates concurrent resolution of the
  same revision; `--default-cache-expiration` (`ARGOCD_DEFAULT_CACHE_EXPIRATION`, 24h). Manifests are cached by commit
  SHA, so a Helm chart/remote base changing without a new commit is only picked up after cache expiry or a hard
  refresh. The API server registers the same repo-cache flags (unprefixed) plus `--repo-server-*` Redis flags.
- **Edge cases**: `timeout.reconciliation: 0s` makes the revision cache TTL 0 → per argocd-cm doc the default cache
  expiration applies; the repo-server must be restarted after changing `timeout.reconciliation`.
- **Configuration surface**: keys `reposerver.repo.cache.expiration`, `reposerver.default.cache.expiration`,
  `reposerver.revision.cache.lock.timeout`; argocd-cm `timeout.reconciliation`.
- **Exposure**: ConfigMap; flags.
- **Tests required**:
  - Positive: second manifest request for the same SHA served from cache (no tool exec).
  - Positive: branch moved → after revision cache expiry the new SHA is resolved; before expiry the old SHA (unless hard refresh).
  - Positive: `repo-cache-expiration: 1m` → chart republished at same version is regenerated after 1 minute.

### OPS-037 Repo-server exec, Git and failure-pause tuning
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/exec/exec.go#L44 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/client.go#L219 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/client.go#L305 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/metrics/githandlers.go#L15 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-repo-server/commands/argocd_repo_server.go#L47 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L1034 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/high_availability.md#L48
- **Maturity**: GA
- **Semantics**: `ARGOCD_EXEC_TIMEOUT` (Go duration, default 90s) — tool commands receive SIGTERM after this;
  `ARGOCD_EXEC_FATAL_TIMEOUT` (default 10s) — SIGKILL after the additional grace. `ARGOCD_GIT_ATTEMPTS_COUNT` — retries
  of remote Git operations (ls-remote/fetch) with `ARGOCD_GIT_RETRY_DURATION` (250ms), `ARGOCD_GIT_RETRY_MAX_DURATION`
  (5s), `ARGOCD_GIT_RETRY_FACTOR` (2). `ARGOCD_GIT_REQUEST_TIMEOUT` (`reposerver.git.request.timeout`, 15s) — HTTP client
  timeout of Git requests. `ARGOCD_GIT_LS_REMOTE_PARALLELISM_LIMIT` (`reposerver.git.lsremote.parallelism.limit`, 0 =
  unlimited) — semaphore around `ls-remote`. Manifest-generation failure pausing: `ARGOCD_PAUSE_GEN_AFTER_FAILED_ATTEMPTS`
  (3; 0 disables), `ARGOCD_PAUSE_GEN_MINUTES` (60), `ARGOCD_PAUSE_GEN_REQUESTS` (0) — after N consecutive cached failures
  further generation is short-circuited (cached error returned) for the configured minutes or number of requests.
- **Edge cases**: Unparseable `ARGOCD_EXEC_TIMEOUT` silently falls back to 90s. Unparseable `ARGOCD_GIT_ATTEMPTS_COUNT`
  panics ("Invalid value in ARGOCD_GIT_ATTEMPTS_COUNT env variable"). A command ignoring SIGTERM is killed after the
  fatal timeout.
- **Configuration surface**: env vars above; cmd-params keys noted.
- **Exposure**: Pod env.
- **Tests required**:
  - Positive: plugin sleeping beyond `ARGOCD_EXEC_TIMEOUT=2s` → generation fails with a timeout error; process tree gone after fatal timeout.
  - Positive: ls-remote limit 1 with 5 concurrent resolutions → serialized (duration histogram).
  - Positive: 3 consecutive generation failures → 4th request returns cached error without executing the tool; after pause window generation retried.
  - Negative: `ARGOCD_GIT_ATTEMPTS_COUNT=abc` → startup panic (or documented Patrol divergence).

### OPS-038 Controller status, operation and hydration processors
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-application-controller/commands/argocd_application_controller.go#L266 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L955 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L947 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/high_availability.md#L82
- **Maturity**: GA (hydration processors: Beta)
- **Semantics**: Each controller replica runs `--status-processors` (20) goroutines draining the app refresh queue
  (comparisons, ms-scale), `--operation-processors` (10) draining the operation (sync) queue, one goroutine each for the
  comparison-type queue and project queue, and, when the hydrator is enabled, one app-hydrate enqueuer plus
  `--hydration-processors` (5) workers on the hydration queue keyed by {source repo, target revision, destination
  branch} (same key never processed concurrently). Upstream guidance: ~50/25 for 1000 apps.
- **Edge cases**: Hydration processors <1 are forced to 1 with warning "hydration-processors was set to %d; hydration
  requires at least one worker, using 1 instead". Status/operation processors may be 0 (no workers).
- **Configuration surface**: keys `controller.status.processors`, `controller.operation.processors`,
  `controller.hydration.processors`.
- **Exposure**: ConfigMap; flags.
- **Tests required**:
  - Positive: with operation processors 1, two simultaneous syncs run sequentially; with 2, concurrently.
  - Negative: hydration processors 0 → warning and 1 worker.
  - Negative: status processors 0 → no reconciliation (parity) — or documented divergence.

### OPS-039 Controller kubectl parallelism limit
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L227 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L362 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-application-controller/commands/argocd_application_controller.go#L281 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cmd-params-cm.yaml#L92
- **Maturity**: GA
- **Semantics**: `--kubectl-parallelism-limit` (20; <1 = unlimited) creates a weighted semaphore acquired around every
  kubectl-library operation run by the controller (apply/create/replace/auth reconcile, etc.); each call increments
  `argocd_kubectl_exec_total{hostname,command}` and, while waiting/holding with a limit, `argocd_kubectl_exec_pending`
  (OPS-069).
- **Edge cases**: The pending gauge is only maintained when a limit is configured.
- **Configuration surface**: `controller.kubectl.parallelism.limit`.
- **Exposure**: ConfigMap; flags.
- **Tests required**:
  - Positive: limit 1 with a multi-resource sync → apply operations serialized; pending gauge rises above 0 during the sync.
  - Positive: limit 0 → gauge never incremented.

### OPS-040 Controller sharding: replica count and shard identity
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sharding/sharding.go#L460 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sharding/sharding.go#L262 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sharding/sharding.go#L66 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/high_availability.md#L112 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/application-controller/argocd-application-controller-statefulset.yaml#L35
- **Maturity**: GA
- **Semantics**: Clusters (not apps) are the unit of sharding. Replica count = `ARGOCD_CONTROLLER_REPLICAS` (default 0;
  manifests set "1") unless dynamic distribution is enabled (OPS-045). With replicas ≤ 1 the controller logs "Processing
  all cluster shards" and handles shard 0 (all clusters). With replicas > 1: shard = `ARGOCD_CONTROLLER_SHARD` if ≥ 0,
  otherwise inferred from the hostname suffix after the last `-` (StatefulSet ordinal, e.g. `...-controller-2` → 2).
  Each replica only processes Applications whose destination cluster maps to its shard (cluster filter), and only
  caches those clusters. The in-cluster destination (no cluster Secret) always maps to shard 0. To scale: set StatefulSet
  replicas N and the env `ARGOCD_CONTROLLER_REPLICAS=N` (changing it restarts all controller pods).
- **Edge cases**: Hostname without numeric suffix → warning "hostname should end with shard number separated by '-'
  but got: <h>" and shard 0. Computed shard **greater than** replicas → warning and shard 0 (shard == replicas is not
  caught). Mismatch between StatefulSet replicas and the env var leaves clusters unprocessed or duplicated.
- **Configuration surface**: env `ARGOCD_CONTROLLER_REPLICAS`, `ARGOCD_CONTROLLER_SHARD`; StatefulSet replicas.
- **Exposure**: Pod env; `argocd admin cluster shards/stats` (OPS-133/134).
- **Tests required**:
  - Positive: 2 replicas, 4 clusters → every cluster's apps reconciled by exactly one replica (log/metrics show `argocd_cluster_info` per pod).
  - Positive: `ARGOCD_CONTROLLER_SHARD=1` on pod 0 overrides hostname inference.
  - Negative: hostname `controller-abc` → shard 0 with warning.
  - Recovery: scale 2→3 with env updated → clusters redistributed, no app left unreconciled.

### OPS-041 Sharding algorithm: `legacy` (default)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sharding/sharding.go#L106 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sharding/sharding.go#L85 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L147 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/high_availability.md#L140
- **Maturity**: GA
- **Semantics**: `shard = fnv32a(cluster.ID) % replicas` (cluster ID = cluster Secret UID). Manual `shard` < replicas
  wins. nil cluster (in-cluster without Secret) → 0; empty ID → 0; replicas 0 → -1 (no shard). Distribution is stable but
  not uniform. Selected via `--sharding-method legacy` / `controller.sharding.algorithm` /
  `ARGOCD_CONTROLLER_SHARDING_ALGORITHM`. Unknown algorithm names log "distribution type <x> is not supported,
  defaulting to legacy".
- **Edge cases**: Removing/adding clusters never moves other clusters (hash of own UID only).
- **Configuration surface**: `controller.sharding.algorithm: legacy`.
- **Exposure**: ConfigMap; flags.
- **Tests required**:
  - Positive: golden test — given UIDs and replicas, shard equals fnv32a mod N (exact parity values).
  - Negative: algorithm `foo` → warning and legacy behaviour.

### OPS-042 Sharding algorithm: `round-robin`
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sharding/sharding.go#L141 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sharding/sharding.go#L288 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/high_availability.md#L151 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/feature-maturity.md#L23
- **Maturity**: Alpha
- **Semantics**: Clusters sorted by ID; `shard = index % replicas`. Equal distribution ±1. Manual shard < replicas wins;
  nil cluster → 0; cluster not in list → -1 (warning "Cluster with id=<id> not found in cluster map."); replicas < 1 → -1
  (warning "The number of replicas (%d) is lower than 1").
- **Edge cases**: Removing the cluster at rank 0 reshuffles all clusters. Typically paired with dynamic distribution.
- **Configuration surface**: `controller.sharding.algorithm: round-robin`.
- **Exposure**: ConfigMap; flags.
- **Tests required**:
  - Positive: 5 clusters / 2 replicas → shards 3/2 by sorted ID order (golden).
  - Positive: removing the first-ranked cluster changes assignments of others (documented reshuffle).

### OPS-043 Sharding algorithm: `consistent-hashing` (bounded loads)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sharding/sharding.go#L171 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sharding/sharding.go#L203 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sharding/sharding.go#L241 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/high_availability.md#L144
- **Maturity**: Alpha
- **Semantics**: Builds a consistent-hash ring with members "0".."N-1"; iterates clusters sorted by ID, assigning each
  with `GetLeast(cluster.ID)` and then increasing the chosen shard's load by the number of Applications whose
  `spec.destination.server` equals the cluster server (bounded loads). Manual shard < replicas wins; nil cluster → 0;
  cluster not in current list → -1. Aims at near-equal distribution (±~10%) with minimal reshuffling on cluster/shard changes.
- **Edge cases**: Ring lookup error → cluster falls back to shard 0 (logged). Apps addressing clusters by
  `destination.name` count under an empty server key (no load attributed).
- **Configuration surface**: `controller.sharding.algorithm: consistent-hashing`.
- **Exposure**: ConfigMap; flags.
- **Tests required**:
  - Positive: golden assignment for a fixed cluster/app set (requires identical ring implementation — `controller/sharding/consistent`).
  - Positive: adding one cluster moves at most a small number of existing clusters.

### OPS-044 Manual cluster shard assignment (`shard` field of the cluster Secret)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sharding/sharding.go#L69 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/high_availability.md#L138 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/high_availability.md#L162
- **Maturity**: GA
- **Semantics**: A cluster Secret (`argocd.argoproj.io/secret-type: cluster`) may carry `shard: "<n>"` (also settable via
  cluster create/update API/CLI, owner 03). If `n < replicas` the cluster is pinned to shard n for every algorithm;
  otherwise the warning "Specified cluster shard (n) for cluster: <name> is greater than the number of available shard.
  Assigning automatically." is logged and the algorithm decides.
- **Edge cases**: Pinning survives algorithm changes; pinning to a shard with no running pod leaves the cluster unprocessed.
- **Configuration surface**: cluster Secret `shard`.
- **Exposure**: CRD-like Secret; API/CLI cluster shard field (owner 03).
- **Tests required**:
  - Positive: cluster pinned to shard 1 with 2 replicas → only pod 1 reconciles its apps.
  - Negative: shard 5 with 2 replicas → warning, automatic assignment.

### OPS-045 Dynamic cluster distribution
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sharding/sharding.go#L306 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sharding/sharding.go#L357 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sharding/sharding.go#L40 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L272 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/ha/base/controller-deployment/kustomization.yaml#L4 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/dynamic-cluster-distribution.md#L24
- **Maturity**: Alpha
- **Semantics**: Enabled by `--dynamic-cluster-distribution-enabled` / `ARGOCD_ENABLE_DYNAMIC_CLUSTER_DISTRIBUTION=true`
  with the controller run as a **Deployment** (overlay `manifests/ha/base/controller-deployment` scales the StatefulSet to
  0 and adds Deployment `argocd-application-controller`; the env var itself is not set by the overlay). Replica count is
  read from the Deployment named `$ARGOCD_APPLICATION_CONTROLLER_NAME` (default `argocd-application-controller`).
  Shard ownership is stored in ConfigMap `argocd-app-controller-shard-cm`, key `shardControllerMapping`, a JSON list of
  `{ShardNumber, ControllerName (pod hostname), HeartbeatTime}`. Each readiness probe (every 10s) runs the heartbeat:
  pad the list to `replicas`; if the list is longer than `replicas` reset it; refresh own entry (by `ARGOCD_CONTROLLER_SHARD`
  or hostname); otherwise claim the first entry with empty controller or heartbeat older than `3 × HeartbeatDuration`.
  `ARGOCD_CONTROLLER_HEARTBEAT_TIME` sets HeartbeatDuration (default 10s, allowed 10–60s). On shard change the controller
  updates its filter and re-queues all applications. Startup retries ConfigMap conflicts 3 times.
- **Edge cases**: Deployment missing → fatal "(dynamic cluster distribution) failed to get app controller deployment";
  nil replicas → "... failed to get app controller deployment replica count". Readiness fails with "application controller
  deployment replicas is not set or is less than 0" when replicas ≤ 0, and with "error while updating the heartbeat for to
  the Shard Mapping ConfigMap" on CM errors. Scale-down resets mappings and every pod re-claims. When the CM is absent it
  is created with this pod on its env shard or shard 0.
- **Configuration surface**: env `ARGOCD_ENABLE_DYNAMIC_CLUSTER_DISTRIBUTION`, `ARGOCD_CONTROLLER_HEARTBEAT_TIME`,
  `ARGOCD_APPLICATION_CONTROLLER_NAME`; ConfigMap `argocd-app-controller-shard-cm`; controller Role
  `apps/deployments` get/list/watch.
- **Exposure**: Pod env; ConfigMap.
- **Tests required**:
  - Positive: 2→3 replicas → CM gains a 3rd entry, clusters redistributed, no restart of existing pods.
  - Recovery: kill a pod; its shard is re-claimed by a new pod after heartbeat timeout (30s default).
  - Negative: delete the Deployment while enabled → readiness/start errors as above.
  - Negative: `ARGOCD_CONTROLLER_HEARTBEAT_TIME=5` → out of range, default 10 used.

### OPS-046 Application controller workqueue rate limiting
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/ratelimiter/ratelimiter.go#L21 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/ratelimiter/ratelimiter.go#L39 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-application-controller/commands/argocd_application_controller.go#L296 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/high_availability.md#L558
- **Maturity**: GA
- **Semantics**: Controller queues use `max(perItem, bucket)`. Global token bucket: `--wq-bucket-size`
  (`WORKQUEUE_BUCKET_SIZE`, 500, min 1), `--wq-bucket-qps` (`WORKQUEUE_BUCKET_QPS`, MaxFloat64 = disabled, min 1).
  Per-item exponential with auto-reset: `--wq-cooldown-ns` (`WORKQUEUE_FAILURE_COOLDOWN_NS`, 0 = per-item limiter off,
  max 24h), `--wq-basedelay-ns` (`WORKQUEUE_BASE_DELAY_NS`, 1ms), `--wq-maxdelay-ns` (`WORKQUEUE_MAX_DELAY_NS`, 1s, min
  1ms), `--wq-backoff-factor` (`WORKQUEUE_BACKOFF_FACTOR`, 1.5). When cooldown ≠ 0: backoff = base if time since last
  requeue ≥ cooldown, else `min(max, base × factor^numRequeue)`; when cooldown = 0: backoff = base.
- **Edge cases**: Doc defaults differ from code (base 1μs/max 3s in doc vs 1ms/1s in code) — code is baseline.
- **Configuration surface**: flags/env above.
- **Exposure**: Pod env; flags.
- **Tests required**:
  - Positive: bucket QPS 1 / size 1 with 10 apps → refreshes spread ≈1/s.
  - Positive: per-item limiter enabled → a hot-looping app's requeue delay grows exponentially to max and resets after cooldown.

### OPS-047 Kubernetes client QPS, burst, transport timeouts and server-side timeout
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/cluster_constants.go#L41 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L3902 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/3.2-3.3.md#L82
- **Maturity**: GA
- **Semantics**: `SetK8SConfigDefaults` is applied to the local client of every component and to every managed-cluster
  REST config: QPS = `ARGOCD_K8S_CLIENT_QPS` (50, float), Burst = `ARGOCD_K8S_CLIENT_BURST` (2×QPS); custom transport with
  dial timeout `ARGOCD_K8S_TCP_TIMEOUT` (30s), keep-alive `ARGOCD_K8S_TCP_KEEPALIVE` (30s), TLS handshake
  `ARGOCD_K8S_TLS_HANDSHAKE_TIMEOUT` (10s), `MaxIdleConns = MaxIdleConnsPerHost = MaxConnsPerHost =
  ARGOCD_K8S_CLIENT_MAX_IDLE_CONNECTIONS` (500), idle timeout `ARGOCD_K8S_TCP_IDLE_TIMEOUT` (5m), proxy from environment
  unless the cluster config sets one; request `Timeout` = `ARGOCD_K8S_SERVER_SIDE_TIMEOUT` (0 = none; separated from the
  TCP timeout since 3.3).
- **Edge cases**: `MaxConnsPerHost` equals the idle setting, so lowering it caps concurrency per API server. Exec/auth
  providers are folded into the custom transport.
- **Configuration surface**: env vars (cmd-params keys OPS-021).
- **Exposure**: Pod env.
- **Tests required**:
  - Positive: QPS 1 → measurable throttling of controller list/watch at startup.
  - Positive: `ARGOCD_K8S_SERVER_SIDE_TIMEOUT=1s` → long list calls carry `timeout=1s`.
  - Negative: unreachable managed cluster → connection error within ~30s dial timeout.

### OPS-048 Kubernetes client HTTP retries
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/http/http.go#L27 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/http/http.go#L174 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/types.go#L3942 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-server/commands/argocd_server.go#L44 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/kube/failureretrywrapper.go#L19 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/high_availability.md#L622
- **Maturity**: GA
- **Semantics**: (1) `ARGOCD_K8SCLIENT_RETRY_MAX` (0 = off) and `ARGOCD_K8SCLIENT_RETRY_BASE_BACKOFF` (100 ms) wrap every
  K8s transport with a retrier: retries on transport errors, HTTP 429, status 0, and 5xx except 501; backoff starts at
  base and doubles, capped at 10s; request body is buffered and replayed; aborts on context cancellation. (2) API server
  only: `ARGOCD_K8S_RETRY_COUNT` (0, max 10) and `ARGOCD_K8S_RETRY_DURATION_MILLISECONDS` (100, max 1000) add a fixed-
  interval retry wrapper to the Argo CD CRD clientset, retrying on internal/timeout/server-timeout/429 errors, probable
  EOF, connection reset, and HTTP 503/504.
- **Edge cases**: 4xx (other than 429) and 501 are never retried. Retry max below 1 is treated as disabled.
- **Configuration surface**: env vars; cmd-params `controller.k8sclient.retry.*`, `server.k8sclient.retry.*`.
- **Exposure**: Pod env.
- **Tests required**:
  - Positive: API proxy returning 503 twice then 200 → request succeeds with retry max ≥2.
  - Negative: 404 → no retry; 501 → no retry.
  - Positive: backoff doubles (100ms, 200ms, 400ms) capped at 10s (timing test with fake clock).

### OPS-049 Controller cluster-cache tuning
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/cache.go#L46 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/cache.go#L84 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/cache.go#L120 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/high_availability.md#L190
- **Maturity**: GA
- **Semantics**: Env vars and defaults: `ARGOCD_CLUSTER_CACHE_RESYNC_DURATION` (12h full relist),
  `ARGOCD_CLUSTER_CACHE_WATCH_RESYNC_DURATION` (10m max watch lifetime before relist/restart),
  `ARGOCD_CLUSTER_SYNC_RETRY_TIMEOUT_DURATION` (10s retry after cluster sync error), `ARGOCD_CLUSTER_CACHE_LIST_PAGE_SIZE`
  (500), `ARGOCD_CLUSTER_CACHE_LIST_PAGE_BUFFER_SIZE` (1, min 1; prefetch pages to beat etcd compaction "continue parameter
  is too old to display a consistent list result"), `ARGOCD_CLUSTER_CACHE_LIST_SEMAPHORE` (50 concurrent list
  operations across clusters), `ARGOCD_CLUSTER_CACHE_ATTEMPT_LIMIT` (1 = no retry), `ARGOCD_CLUSTER_CACHE_RETRY_USE_BACKOFF`
  (false), `ARGOCD_CLUSTER_CACHE_BATCH_EVENTS_PROCESSING` (true), `ARGOCD_CLUSTER_CACHE_EVENTS_PROCESSING_INTERVAL`
  (100ms; only with batching). The controller watches only preferred API versions; conversion to the Git version falls
  back to a live API query (counted in `argocd_app_k8s_request_total`).
- **Edge cases**: Batching off → events processed one by one (higher lock contention).
- **Configuration surface**: env vars; cmd-params `controller.cluster.cache.*`.
- **Exposure**: Pod env.
- **Tests required**:
  - Positive: batch interval 1s → `argocd_resource_events_processed_in_batch` > 1 under churn.
  - Positive: page size 10 with 100 ConfigMaps → paged list (observed via API audit or request counter).

### OPS-050 Cluster info refresh interval
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/clusterinfoupdater.go#L27 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/high_availability.md#L118
- **Maturity**: GA
- **Semantics**: The controller periodically writes cluster info (server version, API/resource counts, apps count,
  connection state) for its clusters to the cache; interval = `ARGO_CD_UPDATE_CLUSTER_INFO_TIMEOUT` (Go duration, default
  10s, bounded 10s–1m).
- **Edge cases**: Values outside [10s, 1m] are ignored (default). The doc describes the unit as seconds; the code parses
  a duration string (see Doc/implementation differences).
- **Configuration surface**: env var.
- **Exposure**: Pod env; cluster info visible via API/CLI/UI (owner 03).
- **Tests required**:
  - Positive: `30s` → cluster info updates at ~30s cadence.
  - Negative: `5s` → warning, 10s used.

### OPS-051 Redis cache connectivity (address, DB, TLS, Sentinel, compression, credentials, retries)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/cache/cache.go#L24 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/cache/cache.go#L139 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/cache/cache.go#L211 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/cache/cache.go#L119 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/security.md#L36
- **Maturity**: GA
- **Semantics**: Flags on controller, server, repo-server (and `--repo-server-` prefixed copies on the server):
  `--redis` (`REDIS_SERVER`, default `argocd-redis:6379`), `--redisdb` (`REDISDB`, 0), `--sentinel` (repeatable; when set a
  failover client is used), `--sentinelmaster` (`master`), `--redis-use-tls`, `--redis-client-certificate`,
  `--redis-client-key`, `--redis-insecure-skip-tls-verify`, `--redis-ca-certificate` (else system roots),
  `--redis-compress` (`REDIS_COMPRESSION`, `gzip`|`none`), `--default-cache-expiration`. Credentials: if
  `REDIS_CREDS_DIR_PATH` is set, read files `auth`, `auth_username`, `sentinel_username`, `sentinel_auth` from that
  directory (missing file = empty; unreadable = error); otherwise env `REDIS_PASSWORD`, `REDIS_USERNAME`,
  `REDIS_SENTINEL_USERNAME`, `REDIS_SENTINEL_PASSWORD`, with `<PREFIX>_`-prefixed overrides for prefixed caches (e.g.
  `REPO_SERVER_REDIS_PASSWORD`). `REDIS_RETRY_COUNT` (3) client retries. A hook rebuilds the client on failures. Redis
  traffic is plaintext by default.
- **Edge cases**: Prefixed flags (`--repo-server-redis…`) only take effect when explicitly passed (`Changed`); their env
  defaults are therefore ignored and the unprefixed value is used. `REDIS_CREDS_DIR_PATH` pointing to a missing directory
  → error "failed to access Redis credentials: mount path ... does not exist or is inaccessible". Redis unreachable →
  components keep running with cache misses/errors (counted with `failed="true"`).
- **Configuration surface**: flags/env above; cmd-params `redis.*`.
- **Exposure**: Flags; env.
- **Tests required**:
  - Positive: TLS Redis with custom CA; mTLS client cert.
  - Positive: Sentinel mode with 3 sentinels; master failover → clients reconnect.
  - Positive: creds from mounted directory override env.
  - Negative: wrong password → requests fail, `argocd_redis_request_total{failed="true"}` increments, reconciliation continues (slower).

### OPS-052 Redis HA (redis-ha chart: StatefulSet + Sentinel + HAProxy)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/ha/base/redis-ha/chart/values.yaml#L1 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/ha/base/redis-ha/chart/requirements.yaml#L1 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/ha/base/overlays/argocd-cmd-params-cm.yaml#L6 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/high_availability.md#L270
- **Maturity**: GA
- **Semantics**: Generated from the `redis-ha` chart 4.35.10 with: auth from `argocd-redis`/`auth`; no persistence;
  master group `argocd`; `save ""`; bind 0.0.0.0; HAProxy enabled (image tag 3.0.8-alpine, IPv6 off, client/server timeouts
  6m, check interval 3s, metrics on 9101, `maxconn 4096`); sentinel bind 0.0.0.0 with a postStart hook that sleeps 30s
  then runs `sentinel reset argocd`. Clients connect to `argocd-redis-ha-haproxy:6379`, which routes to the current
  master. Three redis-ha-server pods (StatefulSet) and three HAProxy pods.
- **Edge cases**: Designed for exactly three Redis/sentinel instances. Master failover is transparent behind HAProxy.
- **Configuration surface**: chart values (regenerated with `generate.sh`), `redis.server`.
- **Exposure**: Install artifact.
- **Tests required**:
  - Recovery: delete the master pod → new master elected; Argo CD cache ops resume within seconds; no app errors persist.
  - Positive: HAProxy exporter on 9101 serves metrics.

### OPS-053 Application resource-tree sharding in Redis
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/cache/appstate/cache.go#L18 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/high_availability.md#L215
- **Maturity**: GA
- **Semantics**: `ARGOCD_APPLICATION_TREE_SHARD_SIZE` (default 0 = single key; max 1000; recommended ~100) splits each
  application's resource tree into multiple Redis keys of at most N nodes, reducing controller↔Redis traffic for large apps.
  Must be set consistently on the controller (writer) and API server (reader).
- **Edge cases**: Values >1000 ignored.
- **Configuration surface**: env var.
- **Exposure**: Pod env.
- **Tests required**:
  - Positive: shard size 10 with a 35-resource app → tree stored in 4 keys; UI/API tree identical to unsharded.

### OPS-054 gRPC message size and keepalive
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apiclient/apiclient.go#L68 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/apiclient/clientset.go#L29 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L924 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L453 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/high_availability.md#L258
- **Maturity**: GA
- **Semantics**: `ARGOCD_GRPC_MAX_SIZE_MB` sets max send/recv message size: default 200 MB for the API server/CLI
  client and 100 MB for repo-server client/server (`reposerver.grpc.max.size` maps to it on the repo-server). API gRPC
  server: connection timeout 300s; keepalive enforcement minimum `ARGOCD_GRPC_KEEP_ALIVE_MIN` (default 10s, Go
  duration), clients ping at 2× that.
- **Edge cases**: Exceeding the limit yields gRPC `ResourceExhausted`. Unparseable keepalive → warning, 10s.
- **Configuration surface**: env vars; `reposerver.grpc.max.size`.
- **Exposure**: Pod env.
- **Tests required**:
  - Negative: app list > limit with `ARGOCD_GRPC_MAX_SIZE_MB=1` → ResourceExhausted; raising the limit fixes it.

### OPS-055 ApplicationSet controller HA (leader election)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-applicationset-controller/commands/applicationset_controller.go#L172 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-applicationset-controller/commands/applicationset_controller.go#L297 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/cluster-rbac/applicationset-controller/argocd-applicationset-controller-clusterrole.yaml#L59
- **Maturity**: GA
- **Semantics**: `--enable-leader-election` (`applicationsetcontroller.enable.leader.election`, default false) enables
  controller-runtime leader election on Lease `58ac56fa.applicationsets.argoproj.io` (namespace of the controller), so
  multiple replicas run active/standby. Shipped manifests run 1 replica.
- **Edge cases**: Without leader election, multiple replicas would reconcile concurrently (conflicting writes).
- **Configuration surface**: key/flag above; replicas.
- **Exposure**: ConfigMap.
- **Tests required**:
  - Positive: 2 replicas with election → only the lease holder reconciles; kill it → the other takes over within lease duration.

### OPS-056 CPU/memory profiling endpoints
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/profile/profile.go#L11 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/profile/profile.go#L24 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/application-controller/argocd-application-controller-statefulset.yaml#L437 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-applicationset-controller/commands/applicationset_controller.go#L182 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/high_availability.md#L664
- **Maturity**: GA
- **Semantics**: `/debug/pprof/`, `/debug/pprof/cmdline`, `/debug/pprof/profile`, `/debug/pprof/symbol`,
  `/debug/pprof/trace` are registered on the metrics listener of the controller (8082), API server (8083), repo-server
  (8084) and ApplicationSet controller (metrics server extra handler). Each request reads file
  `ARGOCD_ENABLE_PROFILER_FILE_PATH` (default `/home/argocd/params/profiler.enabled`, projected from cmd-params key
  `<component>.profile.enabled`); only exact content `true` serves the profile, otherwise HTTP 401 "Profiler endpoint is not
  enabled in 'argocd-cmd-params-cm' ConfigMap". Toggling takes effect without restart (ConfigMap volume refresh).
- **Edge cases**: Commit-server, notifications and Dex have no profiler.
- **Configuration surface**: `controller.profile.enabled`, `server.profile.enabled`, `reposerver.profile.enabled`,
  `applicationsetcontroller.profile.enabled`.
- **Exposure**: HTTP on metrics ports.
- **Tests required**:
  - Negative: default → 401 with the message.
  - Positive: set key `true` → after volume refresh `go tool pprof http://<pod>:8082/debug/pprof/heap` works, no restart.

### OPS-057 Go memory limit tuning (`GOMEMLIMIT`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/high_availability.md#L680
- **Maturity**: GA
- **Semantics**: Documented operator guidance: set `GOMEMLIMIT` to 80–90% of the container memory limit (e.g. 1800MiB for
  2Gi) on Argo CD containers to trigger GC before OOMKill (controller cold start, expensive API requests); too close to
  the working set causes GC thrashing.
- **Edge cases**: Pure Go-runtime behaviour; no Argo CD code involved.
- **Configuration surface**: env `GOMEMLIMIT`.
- **Exposure**: Pod env.
- **Tests required**:
  - Positive: Patrol components honour `GOMEMLIMIT` (Go runtime) — smoke test under memory pressure, or record divergence if Patrol uses a different runtime.

---

## D. Reconciliation scheduling and reconcile optimization

### OPS-058 Periodic soft reconciliation (`timeout.reconciliation`, `--app-resync`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-application-controller/commands/argocd_application_controller.go#L45 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-application-controller/commands/argocd_application_controller.go#L143 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-application-controller/commands/argocd_application_controller.go#L259 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2194 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2250 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2683 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/cache/cache.go#L70 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/application-controller/argocd-application-controller-statefulset.yaml#L37 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/repo-server/argocd-repo-server-deployment.yaml#L38 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cm.yaml#L331
- **Maturity**: GA
- **Semantics**: argocd-cm `timeout.reconciliation` (Go duration, e.g. `120s`) is projected into env
  `ARGOCD_RECONCILIATION_TIMEOUT` of the application controller and the repo-server at pod start. Controller flag
  `--app-resync` (integer **seconds**; default 120 = env value converted to seconds). Behaviour:
  (1) the Application informer is created with a resync period equal to the soft timeout (or the hard timeout when that
  is non-zero and smaller), so every app is periodically re-enqueued; (2) on each refresh the controller computes
  `softExpired = timeout > 0 && status.reconciledAt + timeout < now` (an app with nil `reconciledAt` is always
  soft-expired so it gets a first comparison); soft expiry triggers a **normal** refresh (resolves the latest revision of
  the source and recomputes the diff) and logs `Refreshing app status (comparison expired, requesting refresh.
  reconciledAt: <t>, expiry: <d>), level (<n>)`. Value `0` disables periodic reconciliation (internally the informer
  resync becomes 100 years); apps are then refreshed only by watch events, webhooks, explicit refresh or spec changes.
  On the repo-server the same env sets `--revision-cache-expiration` (TTL of resolved symbolic revisions; repo-server
  compiled default **3m** when the key is absent, whereas the controller compiled default is **120s**).
- **Edge cases**: A change of the key has no effect until the controller **and** repo-server are restarted (env read
  at start). Unparseable values fall back to the compiled defaults (warning). With `timeout.reconciliation: 0s` the
  repo-server revision cache TTL becomes 0 (per doc the default cache expiration then applies) so branch heads are
  re-resolved on every request. User-requested refreshes (`argocd.argoproj.io/refresh` annotation) and spec/destination
  changes take precedence over expiry when choosing the refresh reason.
- **Configuration surface**: argocd-cm `timeout.reconciliation`; env `ARGOCD_RECONCILIATION_TIMEOUT`; flags
  `--app-resync` (controller), `--revision-cache-expiration` (repo-server).
- **Exposure**: ConfigMap; controller/repo-server flags. Effect visible via `status.reconciledAt` (CRD), app list/UI.
- **Tests required**:
  - Positive: `timeout.reconciliation: 30s` + restart → `status.reconciledAt` advances roughly every 30–90s (with default 60s jitter) without any Git/cluster change.
  - Positive: new commit on a tracked branch (no webhook) is detected within timeout + jitter.
  - Negative: `timeout.reconciliation: 0s` → no periodic refresh (reconciledAt frozen) while a watched-resource change still refreshes the app.
  - Negative: invalid value `abc` → default 120s used, controller starts.
  - Recovery: changing the key without restarting pods has no effect (parity), takes effect after restart.

### OPS-059 Reconciliation jitter (`timeout.reconciliation.jitter`, `--app-resync-jitter`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-application-controller/commands/argocd_application_controller.go#L47 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-application-controller/commands/argocd_application_controller.go#L261 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2791 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/application-controller/argocd-application-controller-statefulset.yaml#L49 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cm.yaml#L343
- **Maturity**: GA
- **Semantics**: argocd-cm `timeout.reconciliation.jitter` → env `ARGOCD_RECONCILIATION_JITTER` → `--app-resync-jitter`
  (seconds, default 60). When the informer delivers a periodic resync (old and new Application have the **same
  resourceVersion**) the refresh is enqueued after a uniformly random delay in `[0, jitter)`, spreading the load on the
  refresh queue and repo-server; with a non-zero delay the app is also added to the operation queue. Real updates (new
  resourceVersion) are enqueued immediately. `0` disables jitter.
- **Edge cases**: Jitter only applies to informer resync events, not to watch-driven or user-requested refreshes. The
  effective period is therefore `timeout` to `timeout + jitter` (doc wording). Restart required after change.
- **Configuration surface**: argocd-cm `timeout.reconciliation.jitter`; env `ARGOCD_RECONCILIATION_JITTER`; flag `--app-resync-jitter`.
- **Exposure**: ConfigMap; controller flag.
- **Tests required**:
  - Positive: 200 apps, timeout 60s, jitter 60s → refresh start times distributed over the 60s window (no single spike).
  - Positive: jitter 0 → all resync refreshes enqueued immediately.
  - Negative: a spec edit is processed immediately even with jitter configured.

### OPS-060 Periodic hard reconciliation (`timeout.hard.reconciliation`, `--app-hard-resync`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-application-controller/commands/argocd_application_controller.go#L49 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-application-controller/commands/argocd_application_controller.go#L260 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2224 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2018 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/application-controller/argocd-application-controller-statefulset.yaml#L43
- **Maturity**: GA
- **Semantics**: argocd-cm `timeout.hard.reconciliation` → env `ARGOCD_HARD_RECONCILIATION_TIMEOUT` → `--app-hard-resync`
  (seconds, default 0 = disabled). When `reconciledAt + hardTimeout < now` (or `reconciledAt` is nil and the hard timeout
  is enabled) the refresh type is **hard**: the comparison is performed with `noCache=true`, so the repo-server bypasses
  its manifest cache and regenerates manifests; log reason `comparison expired, requesting hard refresh`. Hard expiry
  wins over soft expiry. If hardTimeout is non-zero and smaller than the soft timeout, the informer resync period uses
  the hard timeout.
- **Edge cases**: The key is consumed by the shipped manifests but is not listed in the reference `argocd-cm.yaml`
  (Doc/implementation differences). Very low values cause continuous cache-bypassing manifest generation (repo-server
  load). Restart required.
- **Configuration surface**: argocd-cm `timeout.hard.reconciliation`; env `ARGOCD_HARD_RECONCILIATION_TIMEOUT`; flag `--app-hard-resync`.
- **Exposure**: ConfigMap; controller flag.
- **Tests required**:
  - Positive: hard timeout 120s with a Helm chart re-published under the same version → regenerated manifests detected within ~120s (soft refresh alone would serve the cached manifests).
  - Positive: hard timeout 0 → no hard refresh ever occurs automatically (repo-server cache hit counter grows).
  - Positive: hard timeout < soft timeout → informer resync follows the hard timeout.

### OPS-061 Refresh triggers and refresh-cause logging
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L436 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L378 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L499 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1053 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2202 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/cache.go#L380 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/reconcile.md#L3 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/reconcile.md#L71
- **Maturity**: GA
- **Semantics**: Besides periodic expiry (OPS-058/060), an app refresh is requested by: user refresh annotation
  (`argocd.argoproj.io/refresh: normal|hard`, highest precedence, forces revision re-resolution), `spec.source(s)` differing
  from `status.sync.comparedTo`, destination / managedNamespaceMetadata / ignoreDifferences changes, automated-sync
  enablement, and **watch events of live resources** in the cluster cache. For a changed live object the controller maps
  it to owning apps; a change of a managed (tracked) resource requests level `CompareWithRecent` and logs at **info**
  `Requesting app refresh caused by object update` with fields `comparison-level`, `namespace` (`(cluster-scoped)` for
  cluster-scoped), `name`, `api-version`, `kind`, `server`, `cluster-name` plus the standard app fields; orphan/child
  resource changes request `ComparisonWithNothing` (tree-only) and log the same message at **debug**. Every refresh
  logs `Refreshing app status (<reason>), level (<n>)`. Updates to core `Endpoints` never requeue apps; an Application
  whose watched object is itself (app-of-apps self reference) is not force-refreshed; apps of another shard or outside
  allowed namespaces are skipped.
- **Edge cases**: Pending refresh requests are coalesced (max comparison level wins). Missing project → "Unable to
  determine project for app" error and no refresh.
- **Configuration surface**: log level (`controller.log.level`); annotation `argocd.argoproj.io/refresh` (owner 01).
- **Exposure**: Controller logs; CRD annotation.
- **Tests required**:
  - Positive: edit a managed Deployment's spec → info log with kind/api-version and a CompareWithRecent refresh.
  - Positive: create a Pod owned by a managed ReplicaSet → debug-only log, tree refresh.
  - Negative: Endpoints update → no app refresh.
  - Positive: refresh annotation `hard` → noCache comparison and annotation removed after processing.

### OPS-062 Ignore resource updates: global switch and skip semantics (`resource.ignoreResourceUpdatesEnabled`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L485 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L1031 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/cache.go#L390 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/cache.go#L571 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/cache.go#L606 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/reconcile.md#L12 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/reconcile.md#L95
- **Maturity**: GA
- **Semantics**: Default **enabled** (empty/missing key = `true`; any other value parsed with `strconv.ParseBool`).
  When enabled, the cluster cache computes a manifest hash for every tracked resource (resources with an app tracking
  id, any `argoproj.io/Application`, or untracked resources opted in via OPS-064) after applying the ignore-resource-
  updates normalization (OPS-063). On a watch update, if both old and new hashes are non-empty and equal **and** the
  health status is unchanged, the event is dropped: no app refresh is requested and the controller logs (debug)
  `Ignoring change of object because none of the watched resource fields have changed` with server/namespace/name/
  api-version/kind. Disabled (`'false'`) → no hashing, every update requests a refresh.
- **Edge cases**: Health transitions always trigger a refresh even if all changed fields are ignored. Hash generation
  errors are logged (`Failed to generate manifest hash`) and the update is not skipped. Invalid boolean → settings read
  error. The setting is read live (settings informer), no restart needed.
- **Configuration surface**: argocd-cm `resource.ignoreResourceUpdatesEnabled`.
- **Exposure**: ConfigMap; controller debug logs.
- **Tests required**:
  - Positive: default config, status-only churn on a tracked Deployment (no health change) → no refresh, debug log present.
  - Positive: same churn causing health Healthy→Progressing → refresh occurs.
  - Negative: `resource.ignoreResourceUpdatesEnabled: "false"` → every status update triggers a refresh.

### OPS-063 Ignore resource updates rules (`resource.customizations.ignoreResourceUpdates.*`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L996 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L1185 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L1221 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/reconcile.md#L26 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/reconcile.md#L42 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/reconcile.md#L51 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/reconcile.md#L67
- **Maturity**: GA
- **Semantics**: Per group/kind rules in argocd-cm `resource.customizations.ignoreResourceUpdates.<group>_<Kind>`
  (core group: `<Kind>`) or `resource.customizations.ignoreResourceUpdates.all`, each a YAML object with
  `jsonPointers` (RFC 6902) and/or `jqPathExpressions`; also accepted inside the legacy `resource.customizations` block
  under key `ignoreResourceUpdates`. Effective rule set = ignoreResourceUpdates rules **plus**, when
  `resource.compareoptions.ignoreDifferencesOnResourceUpdates` is true (default true since 3.0), the system-level
  `ignoreDifferences` jsonPointers/jqPathExpressions/managedFieldsManagers of the same key; plus, always,
  `/metadata/resourceVersion`, `/metadata/generation`, `/metadata/managedFields` for `*/*`. The controller logs
  `Using diffing customizations to ignore resource updates` when the ignoreDifferences merge is active. Rules are applied
  only to tracked resources (or opted-in, OPS-064). The JQ timeout is `--ignore-normalizer-jq-execution-timeout-seconds`.
- **Edge cases**: Application-level `spec.ignoreDifferences` are **not** used for resource-update skipping (system-level
  only). Malformed YAML in a rule → resource overrides fail to load (settings error surfaced in controller logs). Ignoring
  `/status` does not suppress refreshes caused by health changes (OPS-062).
- **Configuration surface**: argocd-cm `resource.customizations.ignoreResourceUpdates.<gk>`, `.all`,
  `resource.compareoptions` (`ignoreDifferencesOnResourceUpdates`); shipped defaults OPS-155.
- **Exposure**: ConfigMap; `argocd admin settings resource-overrides ignore-resource-updates` (OPS-128).
- **Tests required**:
  - Positive: rule `external-secrets.io_ExternalSecret` ignoring `/status/refreshTime` → refreshTime churn produces no refresh.
  - Positive: `.all` rule with `/status` applies to every tracked kind.
  - Positive: system ignoreDifferences on `/spec/replicas` for Deployments → HPA-driven replica changes do not refresh the app; with `ignoreDifferencesOnResourceUpdates: false` they do.
  - Positive: resourceVersion/generation/managedFields-only changes never refresh.

### OPS-064 Opt-in for untracked resources (`argocd.argoproj.io/ignore-resource-updates`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/cache.go#L81 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/cache.go#L402 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/reconcile.md#L127
- **Maturity**: GA
- **Semantics**: Untracked resources (no app tracking id, e.g. ReplicaSets/Pods/Jobs created by controllers) are not
  hashed and every change refreshes the owning app's tree. Setting annotation
  `argocd.argoproj.io/ignore-resource-updates: "true"` on such a resource (typically via pod/job templates) makes the
  controller hash it with the ignore-resource-updates rules of its kind (e.g. `batch_Job`, `Pod` with `/status`), so
  irrelevant updates are skipped as in OPS-062.
- **Edge cases**: Value parsed with `strconv.ParseBool`; unparseable → treated as false. The annotation has no effect
  when the global switch is disabled. It is ignored on tracked resources (they are always hashed).
- **Configuration surface**: annotation on live resources.
- **Exposure**: Kubernetes resource annotation.
- **Tests required**:
  - Positive: CronJob whose job/pod templates carry the annotation and `batch_Job`/`Pod` rules ignoring `/status` → status churn of Jobs/Pods does not refresh the app.
  - Negative: annotation value `yes` → treated as false, updates refresh the app.

---

## E. Metrics

### OPS-065 Metrics endpoints, registries and scraping
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L321 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/metrics/metrics.go#L187 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/metrics/metrics.go#L74 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/metrics/metrics.go#L41 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-repo-server/commands/argocd_repo_server.go#L188 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-applicationset-controller/commands/applicationset_controller.go#L166 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-notification/commands/argocd_notification.go#L149 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/commitserver/metrics/metrics.go#L48 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/metrics.md#L1 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/metrics.md#L296
- **Maturity**: GA
- **Semantics**: Plain-HTTP, unauthenticated `GET /metrics` (Prometheus text format) per component:
  controller `0.0.0.0:8082` (Service `argocd-metrics`; gathers the app registry plus the controller-runtime registry,
  i.e. workqueue, Go and process metrics; also serves `/healthz` and pprof); API server `<listen host>:8083`
  (`argocd-server-metrics`; own registry + Prometheus default gatherer incl. Go/process); repo-server
  `<metrics-address>:8084` (`argocd-repo-server` port `metrics`; own registry with process and Go collectors; also
  `/healthz` and pprof); ApplicationSet controller `:8080` (controller-runtime metrics server, `--metrics-addr`;
  controller-runtime + workqueue + Go/process metrics); notifications controller `0.0.0.0:9001`
  (`argocd-notifications-controller-metrics`; notifications registry + default gatherer); commit-server
  `<metrics-address>:8087` (own registry with process/Go collectors); Dex `5558` (Dex's own metrics). Documented
  Prometheus-Operator ServiceMonitors select `app.kubernetes.io/name` = `argocd-metrics`, `argocd-server-metrics`,
  `argocd-repo-server`, `argocd-applicationset-controller`, `argocd-dex-server`, `argocd-redis-ha-haproxy`
  (port `http-exporter-port`), `argocd-notifications-controller-metrics`, `argocd-commit-server`, endpoint port
  `metrics`. With multiple controller replicas, scrapers must use pod/endpoint discovery (each replica exports only its
  shard). An example Grafana dashboard is published (`examples/dashboard.json`).
- **Edge cases**: No TLS or auth on any metrics endpoint (network policy only, OPS-010). Metric families are absent until
  first observation (CounterVec/HistogramVec).
- **Configuration surface**: per-component metrics port/address flags (OPS-145..153).
- **Exposure**: HTTP endpoints; Services.
- **Tests required**:
  - Positive: each endpoint returns 200 with `go_goroutines` (where Go collectors are registered) and the component metric families listed in OPS-066..084.
  - Positive: ServiceMonitor selectors from the doc match the shipped Service labels/ports.
  - Positive: two controller replicas expose disjoint `argocd_app_info` series (shard filter).

### OPS-066 `argocd_app_info` gauge
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/metrics/metrics.go#L65 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/metrics/metrics.go#L395 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/metrics/metrics.go#L424 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/metrics.md#L11
- **Maturity**: GA
- **Semantics**: Collected at scrape time from the Application lister (only apps this replica can process). One series
  per app, value 1, labels: `namespace`, `name`, `project`, `autosync_enabled` (`true|false`), `repo` (normalized Git URL
  of the primary source), `dest_server` (resolved destination cluster URL, empty if resolution fails — warning logged),
  `dest_namespace`, `sync_status` (`Synced|OutOfSync|Unknown`; empty → `Unknown`), `health_status` (empty → `Unknown`),
  `operation` (`delete` when deletionTimestamp set, else `sync` when an operation is in progress, else empty).
- **Edge cases**: Apps addressed by `destination.name` are resolved to the server URL. Replaces the removed
  `argocd_app_sync_status`/`argocd_app_health_status`/`argocd_app_created_time` (OPS-156).
- **Configuration surface**: none.
- **Exposure**: `/metrics` (controller).
- **Tests required**:
  - Positive: golden label set for a synced healthy auto-sync app and for an app being deleted (`operation="delete"`).
  - Positive: series disappears when the app is deleted (scrape-time collection).

### OPS-067 `argocd_app_labels` gauge (`--metrics-application-labels`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/metrics/metrics.go#L168 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/metrics/metrics.go#L11 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-application-controller/commands/argocd_application_controller.go#L285 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/metrics.md#L78
- **Maturity**: GA
- **Semantics**: Disabled unless `--metrics-application-labels` (repeatable / comma list) is set. Then one series per app
  with labels `namespace`, `name`, `project` plus `label_<normalized key>` for each configured label key (characters
  invalid in Prometheus label names replaced by `_`, e.g. `team-name` → `label_team_name`); missing labels yield empty
  values; value 1.
- **Edge cases**: The flag has **no env var and no cmd-params mapping** in code/manifests even though metrics.md shows
  `controller.metrics.application.labels` in argocd-cmd-params-cm (Doc/implementation differences). Cardinality grows with
  distinct label values.
- **Configuration surface**: controller flag `--metrics-application-labels`.
- **Exposure**: `/metrics` (controller).
- **Tests required**:
  - Positive: flag `team-name,business-unit` → `argocd_app_labels{label_business_unit=...,label_team_name=...}`.
  - Negative: setting only the documented cmd-params key does not enable the metric (parity of the gap, or documented Patrol fix).

### OPS-068 `argocd_app_condition` gauge (`--metrics-application-conditions`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/metrics/metrics.go#L178 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/metrics/metrics.go#L462 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-application-controller/commands/argocd_application_controller.go#L286 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/metrics.md#L116
- **Maturity**: GA
- **Semantics**: Disabled unless `--metrics-application-conditions` lists condition types (e.g.
  `OrphanedResourceWarning`, `ExcludedResourceWarning`). Per app and per listed condition type present in
  `status.conditions`, a series `argocd_app_condition{namespace,name,project,condition}` with value = number of
  conditions of that type. Types not present produce no series (not 0).
- **Edge cases**: No env/cmd-params mapping. Flag help text names the metric `argocd_application_conditions`
  (cosmetic difference).
- **Configuration surface**: controller flag `--metrics-application-conditions`.
- **Exposure**: `/metrics` (controller).
- **Tests required**:
  - Positive: app with 2 `SyncError`-type conditions and the flag set to `SyncError` → value 2.
  - Negative: condition type not in the flag list → no series.

### OPS-069 Controller kubectl execution metrics (`argocd_kubectl_exec_total`, `argocd_kubectl_exec_pending`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/metrics/metrics.go#L96 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/metrics/metrics.go#L101 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L362 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/metrics.md#L29
- **Maturity**: GA
- **Semantics**: `argocd_kubectl_exec_total` (counter) and `argocd_kubectl_exec_pending` (gauge), labels `hostname`
  (controller pod hostname), `command` (`apply`, `auth`, `create`, `replace`). The counter increments on every
  kubectl-library operation; the pending gauge is incremented while waiting for/holding the parallelism semaphore and
  decremented after (only when `--kubectl-parallelism-limit` ≥ 1, OPS-039).
- **Edge cases**: Both reset by the metrics cache expiration cron (OPS-075).
- **Configuration surface**: none (limit in OPS-039).
- **Exposure**: `/metrics` (controller).
- **Tests required**:
  - Positive: sync of 3 resources with apply → `argocd_kubectl_exec_total{command="apply"}` ≥ 3.
  - Positive: limit 1 under concurrent syncs → pending gauge observed > 0 and returns to 0.

### OPS-070 `argocd_app_reconcile` histogram
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/metrics/metrics.go#L106 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/metrics/metrics.go#L318 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/metrics.md#L16
- **Maturity**: GA
- **Semantics**: Histogram of app reconciliation (refresh) duration in seconds, labels `namespace` (app namespace),
  `dest_server`; buckets `0.25, 0.5, 1, 2, 4, 8, 16`.
- **Edge cases**: Per-app identity is intentionally not a label. Reset by OPS-075.
- **Configuration surface**: none.
- **Exposure**: `/metrics` (controller).
- **Tests required**:
  - Positive: after N refreshes `argocd_app_reconcile_count{namespace,dest_server}` increases by N; bucket boundaries match exactly.

### OPS-071 Sync metrics (`argocd_app_sync_total`, `argocd_app_sync_duration_seconds_total`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/metrics/metrics.go#L72 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/metrics/metrics.go#L80 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/metrics/metrics.go#L249 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1818 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/metrics.md#L17
- **Maturity**: GA
- **Semantics**: `argocd_app_sync_total` (counter) labels `namespace`, `name`, `project`, `dest_server`, `phase`
  (`Succeeded|Failed|Error|...` completed phases only), `dry_run` (`true|false`); incremented once per completed
  operation. `argocd_app_sync_duration_seconds_total` (counter) labels `namespace`, `name`, `project`, `dest_server`; adds
  `finishedAt - startedAt` in whole seconds when `finishedAt` is set.
- **Edge cases**: Running/Terminating phases are never counted. Unresolvable destination → `dest_server=""` with
  warning "Unable to get destination cluster, setting dest_server label to empty string in sync metric".
- **Configuration surface**: none.
- **Exposure**: `/metrics` (controller).
- **Tests required**:
  - Positive: successful sync → `argocd_app_sync_total{phase="Succeeded",dry_run="false"}` +1 and duration counter grows by the operation duration.
  - Positive: dry-run sync → `dry_run="true"`.
  - Negative: failed sync → `phase="Failed"` or `"Error"` accordingly.

### OPS-072 `argocd_app_k8s_request_total` counter
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/metrics/metrics.go#L88 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/metrics/metrics.go#L287 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/metrics/transportwrapper.go#L1 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/metrics.md#L13
- **Maturity**: GA
- **Semantics**: Counter of Kubernetes API requests made on behalf of an application (sync/live-state lookups), labels
  `namespace`, `name`, `project` (empty when not app-scoped), `server`, `response_code`, `verb`
  (`Get|Watch|List|Create|Delete|Patch|Update`), `resource_kind`, `resource_namespace`, `dry_run`.
- **Edge cases**: Requests outside an app context are counted with empty app labels. Reset by OPS-075.
- **Configuration surface**: none.
- **Exposure**: `/metrics` (controller).
- **Tests required**:
  - Positive: syncing a Deployment yields `verb="Patch"` (or `Create`) series with `resource_kind="Deployment"`.
  - Positive: dry-run sync records `dry_run="true"`.

### OPS-073 `argocd_app_orphaned_resources_count` gauge
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/metrics/metrics.go#L138 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/metrics/metrics.go#L277 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/metrics.md#L15
- **Maturity**: GA
- **Semantics**: Gauge per app (`namespace`, `name`, `project`) set to the number of orphaned resources found in the
  destination namespace when the project enables orphaned-resource monitoring (owner of orphan semantics: 07/04).
- **Edge cases**: Not emitted for apps whose project does not monitor orphans; reset by OPS-075.
- **Configuration surface**: AppProject `spec.orphanedResources` (owner 07).
- **Exposure**: `/metrics` (controller).
- **Tests required**:
  - Positive: project with orphan monitoring and 2 unmanaged ConfigMaps in the namespace → gauge 2.

### OPS-074 Cluster cache and event-processing metrics (incl. `argocd_cluster_labels`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/metrics/clustercollector.go#L17 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/metrics/clustercollector.go#L26 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/metrics/clustercollector.go#L81 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/metrics/clustercollector.go#L169 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/metrics/metrics.go#L116 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/metrics/metrics.go#L146 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-application-controller/commands/argocd_application_controller.go#L287 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/metrics.md#L19 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/metrics.md#L192
- **Maturity**: GA
- **Semantics**: Cluster collector refreshes cluster info every 30s (10s timeout) and emits at scrape:
  `argocd_cluster_info{server,k8s_version,name}` = 1; `argocd_cluster_api_resource_objects{server}` = cached object count;
  `argocd_cluster_api_resources{server}` = monitored API resource count; `argocd_cluster_cache_age_seconds{server}` =
  seconds since last full cache sync (`-1` if never synced); `argocd_cluster_connection_status{server,k8s_version}` =
  1 if no sync error else 0; optional `argocd_cluster_labels{server,name,label_<key>...}` = 1 when
  `--metrics-cluster-labels` is set (only for clusters that have labels). Event metrics:
  `argocd_cluster_events_total{server,group,kind}` (counter, every watch event); `argocd_resource_events_processing{server}`
  (histogram, buckets 0.25..16 s, batch processing time); `argocd_resource_events_processed_in_batch{server}` (gauge,
  events in last batch; OPS-049 batching).
- **Edge cases**: Only clusters cached by this shard are reported. `k8s_version` uses the `vMajor.Minor.Patch` format
  since 3.4 (OPS-160).
- **Configuration surface**: controller flag `--metrics-cluster-labels` (no env).
- **Exposure**: `/metrics` (controller).
- **Tests required**:
  - Positive: unreachable cluster → `argocd_cluster_connection_status=0`; reachable → 1.
  - Positive: `--metrics-cluster-labels team` → `argocd_cluster_labels{label_team=...}` only for labelled clusters.
  - Positive: creating 10 ConfigMaps increments `argocd_cluster_events_total{kind="ConfigMap"}` by ≥10.

### OPS-075 Metrics cache expiration (`controller.metrics.cache.expiration`, `--metrics-cache-expiration`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-application-controller/commands/argocd_application_controller.go#L273 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/metrics/metrics.go#L328 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L328 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/metrics.md#L69
- **Maturity**: GA
- **Semantics**: Duration (default `0` = disabled; env `ARGOCD_APPLICATION_CONTROLLER_METRICS_CACHE_EXPIRATION`). When
  set, a cron `@every <duration>` resets (and logs `Reset Prometheus metrics based on existing expiration '<d>'`)
  `argocd_app_sync_total`, `argocd_app_sync_duration_seconds_total`, `argocd_kubectl_exec_total`,
  `argocd_kubectl_exec_pending`, `argocd_app_orphaned_resources_count`, `argocd_app_k8s_request_total`,
  `argocd_cluster_events_total`, `argocd_redis_request_total`, `argocd_app_reconcile`, `argocd_redis_request_duration`,
  `argocd_resource_events_processing`, `argocd_resource_events_processed_in_batch` and all client-go `argocd_kubectl_*`
  metrics, removing series of deleted apps/projects (cardinality control). Scrape-time collectors (app_info,
  cluster metrics) are unaffected.
- **Edge cases**: Counters drop to zero at each reset (Prometheus `increase()` handles resets). Setting the expiration
  twice errors "expiration is already set".
- **Configuration surface**: `controller.metrics.cache.expiration`.
- **Exposure**: ConfigMap; controller flag.
- **Tests required**:
  - Positive: expiration 1m → series for a deleted app disappear from `argocd_app_sync_total` after ≤1m.
  - Negative: default 0 → series persist for the process lifetime.

### OPS-076 Redis request metrics
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/metrics/metrics.go#L121 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/metrics/metrics.go#L30 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/metrics/metrics.go#L103 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/metrics.md#L25
- **Maturity**: GA
- **Semantics**: Per component (names/labels differ, parity must keep them exactly):
  controller `argocd_redis_request_total{hostname,initiator="argocd-application-controller",command,failed}` and
  histogram `argocd_redis_request_duration{hostname,initiator}` (buckets 0.01, 0.05, 0.1, 0.25, 0.5, 1);
  API server `argocd_redis_request_total{initiator,command,failed}` and `argocd_redis_request_duration{initiator}`
  (buckets 0.1, 0.25, 0.5, 1, 2); repo-server `argocd_redis_request_total{initiator,command,failed}` and
  `argocd_redis_request_duration_seconds{initiator}` (buckets 0.1, 0.25, 0.5, 1, 2). `failed` is `true|false`;
  manifest-rename cache misses are counted as `failed="false"`.
- **Edge cases**: Cache misses are not failures. Redis outages increment `failed="true"` while components continue.
- **Configuration surface**: none.
- **Exposure**: `/metrics` (controller, server, repo-server).
- **Tests required**:
  - Positive: golden metric names/label sets per component.
  - Negative: stop Redis → `failed="true"` increments in all three components.

### OPS-077 Repo-server Git and OCI metrics
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/metrics/metrics.go#L48 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/metrics/metrics.go#L57 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/metrics/metrics.go#L122 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/metrics/githandlers.go#L15 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/metrics/ocihandlers.go#L12 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/metrics.md#L259
- **Maturity**: GA
- **Semantics**: Git: `argocd_git_request_total{repo,request_type}` (counter; `request_type` ∈ `fetch`, `ls-remote`),
  `argocd_git_request_duration_seconds{repo,request_type}` (histogram, buckets 0.1, 0.25, 0.5, 1, 2, 4, 10, 20),
  `argocd_git_fetch_fail_total{repo,revision}`, `argocd_git_lsremote_fail_total{repo,revision}` (code-only, undocumented).
  OCI: `argocd_oci_request_total{repo,request_type}` and `argocd_oci_request_duration_seconds{repo,request_type}`
  (`request_type` ∈ `extract`, `resolve-revision`, `digest-metadata`, `get-tags`, `test-repo`; same buckets),
  `argocd_oci_extract_fail_total{repo,revision}`, `argocd_oci_resolve_revision_fail_total{repo,revision}`,
  `argocd_oci_digest_metadata_fail_total{repo,revision}`, `argocd_oci_get_tags_fail_total{repo}`,
  `argocd_oci_test_repo_fail_total{repo}`.
- **Edge cases**: `repo` is the repository URL (high cardinality with many repos).
- **Configuration surface**: none.
- **Exposure**: `/metrics` (repo-server).
- **Tests required**:
  - Positive: manifest generation for a new Git revision increments `fetch` and `ls-remote` counters.
  - Negative: unreachable repo → `argocd_git_lsremote_fail_total` / `argocd_git_fetch_fail_total` increment.
  - Positive: OCI app generation increments `argocd_oci_request_total{request_type="extract"}`.

### OPS-078 Repo-server lock and parallelism metrics
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/metrics/metrics.go#L85 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/metrics/metrics.go#L94 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L684 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/metrics.md#L272
- **Maturity**: GA
- **Semantics**: `argocd_repo_pending_request_total{repo}` (gauge) = requests waiting for the per-repository lock
  (serialized generation, OPS-035); `argocd_repo_parallelism_wait_duration_seconds` (histogram, no labels, buckets 0.1,
  0.25, 0.5, 1, 2, 4, 10, 20, 60, 120) = time waiting on the `--parallelismlimit` semaphore, observed on every acquire
  attempt including failed/cancelled ones.
- **Edge cases**: With parallelism limit 0 the histogram records near-zero waits.
- **Configuration surface**: `reposerver.parallelism.limit`.
- **Exposure**: `/metrics` (repo-server).
- **Tests required**:
  - Positive: limit 1 + concurrent requests → wait histogram has observations > 0.1s.
  - Positive: two concurrent Kustomize generations on one repo → pending gauge 1 during contention.

### OPS-079 gRPC server metrics and `ARGOCD_ENABLE_GRPC_TIME_HISTOGRAM`
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L181 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L916 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/server.go#L87 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L300 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/metrics.md#L220
- **Maturity**: GA
- **Semantics**: The API server and repo-server (and CMP server) always register go-grpc-middleware server metrics
  (`grpc_server_started_total`, `grpc_server_handled_total`, `grpc_server_msg_received_total`,
  `grpc_server_msg_sent_total`, labels `grpc_type`, `grpc_service`, `grpc_method` and `grpc_code` for handled). Env
  `ARGOCD_ENABLE_GRPC_TIME_HISTOGRAM=true` additionally enables `grpc_server_handling_seconds` (histogram).
- **Edge cases**: API server parses the env with `ParseBoolFromEnv`; repo-server requires the exact string `true`.
  The docs state gRPC metrics only appear with the env set, but the counters are always exported (Doc/implementation
  differences).
- **Configuration surface**: env `ARGOCD_ENABLE_GRPC_TIME_HISTOGRAM`.
- **Exposure**: `/metrics` (server 8083, repo-server 8084).
- **Tests required**:
  - Positive: default → `grpc_server_handled_total` present, `grpc_server_handling_seconds_bucket` absent.
  - Positive: env true → histogram present.

### OPS-080 ApplicationSet controller metrics
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/metrics/metrics.go#L18 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/metrics/metrics.go#L45 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/metrics/metrics.go#L74 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/metrics/metrics.go#L113 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/metrics.md#L139
- **Maturity**: GA
- **Semantics**: Registered in the controller-runtime registry (`:8080/metrics`): `argocd_appset_info{namespace,name,
  resource_update_status}` gauge = 1 (`resource_update_status` = reason of the `ResourcesUpToDate` condition, else
  `Unknown`); `argocd_appset_owned_applications{namespace,name}` gauge = `len(status.resources)`;
  `argocd_appset_reconcile{namespace,name}` histogram (default Prometheus buckets) of reconcile duration;
  optional `argocd_appset_labels{namespace,name,label_<key>...}` when `--metrics-applicationset-labels` is set. Also
  controller-runtime/workqueue metrics and the client-go `argocd_kubectl_*` metrics (OPS-083).
- **Edge cases**: Only ApplicationSets passing the namespace filter are reported. `owned_applications` is capped by
  `applicationsetcontroller.status.max.resources.count` (status truncation, owner 06). Flag help says
  `argocd_applicationset_labels` (cosmetic).
- **Configuration surface**: appset flag `--metrics-applicationset-labels` (no env).
- **Exposure**: `/metrics` (appset controller).
- **Tests required**:
  - Positive: appset generating 3 apps → `argocd_appset_owned_applications=3`, `argocd_appset_info{resource_update_status="ApplicationSetUpToDate"}`.
  - Positive: labels flag adds `label_*` labels.

### OPS-081 ApplicationSet GitHub API metrics (`applicationsetcontroller.enable.github.api.metrics`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/github_metrics.go#L52 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/github_metrics.go#L73 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/metrics.md#L166
- **Maturity**: GA (disabled by default)
- **Semantics**: When enabled (`--enable-github-api-metrics` / env `ARGOCD_APPLICATIONSET_CONTROLLER_ENABLE_GITHUB_API_METRICS`;
  API server: `--appset-enable-github-api-metrics`), the GitHub HTTP client used by SCM-provider/PR generators is
  instrumented: `argocd_github_api_requests_total{method,endpoint,status,appset_namespace,appset_name}`,
  `argocd_github_api_request_duration_seconds{method,endpoint,appset_namespace,appset_name}` (default buckets),
  gauges `argocd_github_api_rate_limit_remaining`, `_limit`, `_reset_seconds`, `_used` with labels
  `{endpoint,appset_namespace,appset_name,resource}` from GitHub rate-limit headers.
- **Edge cases**: Doc lists the labels as "name and namespace"; actual label names are `appset_namespace`/`appset_name`.
- **Configuration surface**: cmd-params `applicationsetcontroller.enable.github.api.metrics`.
- **Exposure**: `/metrics` (appset controller; API server for preview/generate calls).
- **Tests required**:
  - Positive: SCM generator against a mocked GitHub API returning rate-limit headers → counters and gauges populated with exact label names.
  - Negative: disabled → no `argocd_github_api_*` families.

### OPS-082 API server metrics (`argocd_info`, `argocd_login_request_total`, proxy-extension metrics)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/metrics/metrics.go#L45 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/metrics/metrics.go#L60 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/metrics/metrics.go#L66 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/session/session.go#L49 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/metrics.md#L216
- **Maturity**: GA (proxy-extension metrics: Beta)
- **Semantics**: `argocd_info{version}` gauge = 1 (code-only, not in the metrics table); `argocd_login_request_total{status}`
  counter with `status` ∈ `success|failure` for `SessionService.Create` (local user login);
  `argocd_proxy_extension_request_total{extension,status}` counter and
  `argocd_proxy_extension_request_duration_seconds{extension}` histogram (buckets 0.1, 0.25, 0.5, 1, 2, 5, 10) for
  requests proxied to proxy extensions (owner of extension semantics: 08). Plus Redis (OPS-076), gRPC (OPS-079) and
  client-go (OPS-083) metrics.
- **Edge cases**: SSO logins do not go through `SessionService.Create` and are not counted.
- **Configuration surface**: none.
- **Exposure**: `/metrics` (`argocd-server-metrics:8083`).
- **Tests required**:
  - Positive: one bad and one good password login → `failure` and `success` each +1.
  - Positive: `argocd_info{version="v3.5.5"}`-equivalent version label equals the running version.

### OPS-083 Client-go Kubernetes client metrics (`argocd_kubectl_*`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/metrics/kubectl/kubectl_metrics.go#L48 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/metrics/kubectl/kubectl_metrics.go#L98 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/metrics/kubectl/kubectl_metrics.go#L130 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/metrics.md#L31
- **Maturity**: GA
- **Semantics**: Registered with client-go's metrics hooks in controller, API server and ApplicationSet controller:
  `argocd_kubectl_client_cert_rotation_age_seconds` (gauge, no labels); `argocd_kubectl_request_duration_seconds{host,verb}`,
  `argocd_kubectl_rate_limiter_duration_seconds{host,verb}`, `argocd_kubectl_dns_resolution_duration_seconds{host}`
  (histograms, buckets 0.005, 0.1, 0.5, 2, 8, 30); `argocd_kubectl_request_size_bytes{host,method}`,
  `argocd_kubectl_response_size_bytes{host,method}` (histograms, buckets 64, 512, 4096, 65536, 1048576, 16777216);
  `argocd_kubectl_requests_total{host,method,code}`; `argocd_kubectl_exec_plugin_call_total{code,call_status}`
  (`call_status` ∈ `no_error|plugin_execution_error|plugin_not_found_error|client_internal_error`);
  `argocd_kubectl_request_retries_total{host,method,code}`; `argocd_kubectl_transport_cache_entries` (gauge);
  `argocd_kubectl_transport_create_calls_total{result}` (`hit|miss|unreachable`).
- **Edge cases**: Reset by OPS-075 on the controller. `code` is the HTTP status (client-go), unlike Argo CD's own
  `response_code`.
- **Configuration surface**: none.
- **Exposure**: `/metrics` (controller, server, appset controller).
- **Tests required**:
  - Positive: golden metric names, types, label names and bucket boundaries.
  - Positive: low QPS (OPS-047) raises `argocd_kubectl_rate_limiter_duration_seconds` observations.

### OPS-084 Commit-server and notifications-controller metrics
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/commitserver/metrics/metrics.go#L28 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/commitserver/metrics/metrics.go#L53 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/commitserver/metrics/metrics.go#L101 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-notification/commands/argocd_notification.go#L149 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/metrics.md#L282 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/notifications/monitoring.md#L11
- **Maturity**: Beta (commit-server, Source Hydrator); GA (notifications)
- **Semantics**: Commit-server: `argocd_commitserver_commit_pending_request_total{repo}` (gauge),
  `argocd_commitserver_git_request_total{repo,request_type}` (counter; `fetch|ls-remote|push`),
  `argocd_commitserver_git_request_duration_seconds{repo,request_type}` (histogram, buckets 0.1..20),
  `argocd_commitserver_commit_request_duration_seconds{repo,response_type}` (histogram; `success|failure`),
  `argocd_commitserver_userinfo_request_duration_seconds{repo,credential_type}` (histogram),
  `argocd_commitserver_commit_request_total{repo,response_type}` (counter), plus Go/process collectors.
  Notifications controller (registry prefix `argocd`): `argocd_notifications_deliveries_total{trigger,service,succeeded}`
  and `argocd_notifications_trigger_eval_total{name,triggered}` — semantics owned by `09-notifications.md`
  (NOTIF-057/058).
- **Edge cases**: Notifications metrics port changeable only by `--metrics-port` (no env/cmd-params key).
- **Configuration surface**: commit-server `--metrics-address/--metrics-port`; notifications `--metrics-port`.
- **Exposure**: `/metrics` (commit-server 8087, notifications 9001).
- **Tests required**:
  - Positive: one successful hydration commit → `argocd_commitserver_commit_request_total{response_type="success"}` +1 and a `push` git request recorded.
  - Negative: push rejected → `response_type="failure"`.

---

## F. Tracing, logging, events and runtime diagnostics

### OPS-085 OpenTelemetry tracing (OTLP gRPC exporter)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/trace/trace.go#L20 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-application-controller/commands/argocd_application_controller.go#L231 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-application-controller/commands/argocd_application_controller.go#L288 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-server/commands/argocd_server.go#L281 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-server/commands/argocd_server.go#L324 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-repo-server/commands/argocd_repo_server.go#L171 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-repo-server/commands/argocd_repo_server.go#L262 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L1204 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/application-controller/argocd-application-controller-statefulset.yaml#L269 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cmd-params-cm.yaml#L25 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/3.3-3.4.md#L144
- **Maturity**: GA
- **Semantics**: Supported by the application controller, API server and repo-server (and the CMP sidecar server, owner
  02). Flags (env, cmd-params key): `--otlp-address` (`ARGOCD_<COMP>_OTLP_ADDRESS`, `otlp.address`, default empty =
  tracing off), `--otlp-insecure` (`..._OTLP_INSECURE`, `otlp.insecure`, default **true** → plaintext gRPC; false →
  TLS with system roots), `--otlp-headers` (`..._OTLP_HEADERS`, `otlp.headers`, `k1=v1,k2=v2` sent with every export),
  `--otlp-attrs` (`..._OTLP_ATTRS`, `otlp.attrs`, `key:value,...` added as resource attributes) where `<COMP>` is
  `APPLICATION_CONTROLLER`, `SERVER`, `REPO_SERVER`. Exporter: OTLP/gRPC trace exporter with batch span processor,
  `AlwaysSample` sampler, W3C TraceContext propagator; resource `service.name` = `argocd-controller` (controller),
  `argocd-server`, `argocd-repo-server`. gRPC client/server stats handlers (otelgrpc) instrument inter-component calls;
  the API server extracts trace context from incoming HTTP headers before invoking gRPC-gateway handlers; proxy
  extensions/dex proxy are wrapped with otelhttp. The API server re-initializes the tracer on every in-process restart
  (OPS-095). Since 3.4 the bundled OTel SDK uses semantic conventions v1.40 (metric/attribute names changed).
- **Edge cases**: Exporter creation failure is fatal (`failed to initialize tracing: ...`). An attr without exactly one
  `:` is skipped with warning `OTLP attr '<a>' split with ':' length not 2`. The dynamic-distribution controller
  Deployment variant does not map `otlp.attrs` (manifest gap). ApplicationSet, notifications, commit-server and Dex have
  no OTLP options.
- **Configuration surface**: `otlp.address`, `otlp.insecure`, `otlp.headers`, `otlp.attrs`; flags above.
- **Exposure**: ConfigMap; flags; OTLP collector.
- **Tests required**:
  - Positive: with an OTLP collector, a UI-triggered sync produces linked spans across argocd-server → controller → repo-server with the three service names.
  - Positive: headers configured are received by the collector; attrs appear as resource attributes.
  - Negative: malformed attr skipped with warning; unreachable collector does not break request processing.
  - Positive: `otlp.insecure: "false"` uses TLS to the collector.

### OPS-086 Log format and level per component
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/cli/cli.go#L186 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/cli/cli.go#L200 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/log/logrus.go#L35 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-application-controller/commands/argocd_application_controller.go#L269 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-server/commands/argocd_server.go#L311 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-applicationset-controller/commands/applicationset_controller.go#L113 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-notification/commands/argocd_notification.go#L101
- **Maturity**: GA
- **Semantics**: Every server component accepts `--logformat` (`json` default | `text`) and `--loglevel` (`debug|info|warn|
  error`, default `info`; any logrus level name accepted). Env/cmd-params mapping: controller
  `ARGOCD_APPLICATION_CONTROLLER_LOGFORMAT/_LOGLEVEL` (`controller.log.format/.level`), server
  `ARGOCD_SERVER_LOGFORMAT`/`ARGOCD_SERVER_LOG_LEVEL` (`server.log.*`), repo-server `ARGOCD_REPO_SERVER_LOGFORMAT/_LOGLEVEL`,
  appset `ARGOCD_APPLICATIONSET_CONTROLLER_LOGFORMAT/_LOGLEVEL` plus `--debug` (`..._DEBUG`) which forces `debug`,
  notifications `ARGOCD_NOTIFICATIONS_CONTROLLER_LOGFORMAT/_LOGLEVEL`, dex wrapper `ARGOCD_DEX_SERVER_LOGFORMAT/_LOGLEVEL`,
  commit-server `ARGOCD_COMMIT_SERVER_LOGFORMAT/_LOGLEVEL`. The chosen format/level is exported to the process env
  (`ARGOCD_LOG_FORMAT`, `ARGOCD_LOG_LEVEL`) so child loggers created later (e.g. gitops-engine/logr bridges) inherit it.
- **Edge cases**: Unknown format → fatal `Unknown log format '<f>'` (notifications: returns `unknown log format '<f>'`);
  empty format = text in `SetLogFormat`. Invalid level → fatal parse error. `argocd admin` itself has `--logformat`
  (json) / `--loglevel` (info) without env.
- **Configuration surface**: flags/env/cmd-params keys above.
- **Exposure**: Pod logs.
- **Tests required**:
  - Positive: each component emits JSON with `level`, `msg`, `time` by default; `text` produces logfmt-style lines.
  - Negative: `controller.log.format: xml` → controller exits with the error.
  - Positive: appset `--debug=true` overrides `loglevel=error`.

### OPS-087 Log timestamp format, full timestamps and forced colors
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/log/logrus.go#L65 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/log/logrus.go#L69 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/log/logrus.go#L73 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L315 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-notification/commands/argocd_notification.go#L109 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cmd-params-cm.yaml#L41
- **Maturity**: GA
- **Semantics**: `ARGOCD_LOG_FORMAT_TIMESTAMP` (cmd-params `log.format.timestamp`) sets the Go time layout for the
  `time` field (JSON and text; empty = RFC3339). `ARGOCD_LOG_FORMAT_ENABLE_FULL_TIMESTAMP=1` prints full timestamps in
  text format. `FORCE_LOG_COLORS=1` forces ANSI colors in text format.
- **Edge cases**: The notifications controller builds its own formatter (plain JSONFormatter, or TextFormatter only when
  `FORCE_LOG_COLORS=1`), so the timestamp/full-timestamp settings do not apply there. Values other than `1` for the two
  boolean env vars are treated as off.
- **Configuration surface**: env vars above; cmd-params `log.format.timestamp`.
- **Exposure**: Pod logs.
- **Tests required**:
  - Positive: `log.format.timestamp: "2006-01-02T15:04:05.000Z07:00"` → millisecond timestamps in repo-server JSON logs.
  - Positive: text format + `ARGOCD_LOG_FORMAT_ENABLE_FULL_TIMESTAMP=1` → full timestamp instead of elapsed seconds.

### OPS-088 Kubernetes client (klog) verbosity (`--gloglevel`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/cli/cli.go#L208 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-application-controller/commands/argocd_application_controller.go#L271 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-server/commands/argocd_server.go#L313
- **Maturity**: GA
- **Semantics**: Controller and API server accept `--gloglevel <int>` (default 0, no env/cmd-params key) which
  initializes klog with `logtostderr=true`, `stderrthreshold=INFO` and `-v=<n>`, exposing client-go request tracing
  (e.g. `-v=6` prints every API call, `-v=9` bodies).
- **Edge cases**: Other components do not expose the flag.
- **Configuration surface**: flag `--gloglevel`.
- **Exposure**: Pod logs.
- **Tests required**:
  - Positive: `--gloglevel 6` on the controller logs Kubernetes request URLs at startup list/watch.

### OPS-089 RBAC and SSO debug logging (`ARGOCD_RBAC_DEBUG`, `ARGOCD_SSO_DEBUG`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L260 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L356 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/oidc/oidc.go#L271 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/oidc/oidc.go#L650 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/session/sessionmanager.go#L177
- **Maturity**: GA
- **Semantics**: API-server env flags (exact value `1`): `ARGOCD_RBAC_DEBUG=1` enables Casbin enforcement logging (each
  RBAC decision with request tuple and result); `ARGOCD_SSO_DEBUG=1` enables verbose OAuth2/OIDC debugging (HTTP
  client dump of IdP exchanges and token/claims processing in the session manager). Owner of RBAC/SSO semantics: 07.
- **Edge cases**: Any other value (e.g. `true`) is ignored. SSO debug output can contain tokens — operator must treat
  logs as sensitive.
- **Configuration surface**: env on `argocd-server`.
- **Exposure**: Pod logs.
- **Tests required**:
  - Positive: `ARGOCD_RBAC_DEBUG=1` → an allowed and a denied request both logged with the evaluated policy tuple.
  - Negative: `ARGOCD_RBAC_DEBUG=true` → no RBAC debug output.

### OPS-090 Structured log fields, security tagging and API payload logging
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/app/log/log.go#L9 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L472 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L937 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L1218 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/security.md#L211 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/security.md#L230 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/security.md#L239
- **Maturity**: GA
- **Semantics**: Application-scoped log lines carry `application` (name), `app-namespace`, `project`; ApplicationSet
  lines carry `applicationSet`, `appSet-namespace`, `project`. Security-relevant lines carry `security` = 1 (low) …
  5 (emergency) and, where applicable, `CWE` (used e.g. for out-of-bounds symlinks, webhook verification failures,
  disabled CSRF protection: `Content-Type enforcement is disabled, which may make your API vulnerable to CSRF attacks`
  with `security=3`). The API server logs gRPC request payloads except for sensitive methods (cluster create/update,
  session create, password update, GPG key create, repository/repocreds create/update/validate incl. write variants,
  `PatchResource`, `GetManifestsWithFiles`) — owner of interceptor details: 08 (API-008). Client IPs are not logged.
- **Edge cases**: Tagging is incomplete by design (doc warning). Payload logging must never include secrets.
- **Configuration surface**: none (log level controls volume).
- **Exposure**: Pod logs.
- **Tests required**:
  - Positive: a refresh log line includes `application`, `app-namespace`, `project`.
  - Positive: `server.api.content.types: ""` start-up warning includes `security=3`.
  - Negative: creating a repository with a password does not log the password (payload suppressed).

### OPS-091 Kubernetes events (audit trail) and `--enable-k8s-event`
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/audit_logger.go#L39 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/audit_logger.go#L49 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/audit_logger.go#L115 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/audit_logger.go#L180 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/audit_logger.go#L207 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1151 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1816 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2332 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2558 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L2540 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/project/project.go#L516 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/applicationset/applicationset.go#L599 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-application-controller/commands/argocd_application_controller.go#L308 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/security.md#L176
- **Maturity**: GA
- **Semantics**: Reasons: `StatusRefreshed`, `ResourceCreated`, `ResourceUpdated`, `ResourceDeleted`, `ResourceActionRan`,
  `OperationStarted`, `OperationCompleted`. Each event is created (never aggregated: name `<obj>.<unixnano hex>`,
  count 1) with `source.component` = `argocd-application-controller` or `argocd-server`, `involvedObject` = the
  Application/AppProject/ApplicationSet (or, for resource actions, the live resource node with its original namespace),
  annotations carrying log fields (`dest-server`, `dest-namespace`, `user`, `resource-namespace`), and is also logged at
  info with `type`/`reason`. Namespace: the object's namespace for Application/AppProject/ApplicationSet; the Argo CD
  namespace for other resources. Emitters — controller: `Updated sync status: <old> -> <new>`, `Updated health status:
  <old> -> <new>` (Normal/ResourceUpdated), `Initiated automated sync to '<revs>'` (OperationStarted),
  `[Partial ]Sync operation [to <rev>] succeeded|failed: <msg>` (Normal|Warning/OperationCompleted),
  `Unable to delete application resources: <err>` (Warning/StatusRefreshed). API server (message prefixed with the
  username or `Unknown user`): app `created application`, `updated application spec`, `deleted application`,
  `patched resource <g>/<k> '<n>'`, `deleted resource ...`, `initiated sync to <rev>`/`initiated rollback to <id>`,
  `terminated running operation`, `ran action <a>[ on resource <g>/<k>/<n>]` (+ a resource event); project
  `created project`, `updated project`, `deleted project`, `created token`, `deleted token`; ApplicationSet
  `created ApplicationSet`, `updated ApplicationSets spec`, `deleted ApplicationSets`. Toggle: `--enable-k8s-event`
  (controller and server; env `ARGOCD_ENABLE_K8S_EVENT`, comma list; default `all`; `none` disables all; otherwise only
  the listed reasons are emitted).
- **Edge cases**: Event creation failure logs `Unable to create audit event: <err>` and is otherwise ignored. AppProject
  events omit the `user` annotation (fields not passed). `ARGOCD_ENABLE_K8S_EVENT` is not wired to any cmd-params key.
  Order in the list matters only for `all`/`none` (first match returns).
- **Configuration surface**: flag/env `--enable-k8s-event` / `ARGOCD_ENABLE_K8S_EVENT`.
- **Exposure**: Kubernetes Events; API `ListResourceEvents` / `GET /api/v1/applications/{name}/events`,
  `/api/v1/projects/{name}/events` (owner 08); UI events tab.
- **Tests required**:
  - Positive: create + sync an app via API as `admin` → events `ResourceCreated` ("admin created application"), `OperationStarted`, `OperationCompleted`, sync/health `ResourceUpdated` from the controller, with correct `source.component`.
  - Positive: `--enable-k8s-event=OperationCompleted` → only that reason is emitted.
  - Positive: `none` → no events, logs still written.
  - Authorization: events for apps in another namespace (apps-in-any-namespace) are created in the app's namespace.

### OPS-092 Event label propagation (`resource.includeEventLabelKeys`, `resource.excludeEventLabelKeys`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L1316 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L2624 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L2640 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cm.yaml#L231
- **Maturity**: GA
- **Semantics**: For Application events (controller and server), labels of the Application merged with labels of its
  AppProject (Application wins on conflict) are filtered: a label is copied onto the Event's `metadata.labels` only if its
  key matches a glob in `resource.includeEventLabelKeys` (comma list, spaces stripped) and does not match a glob in
  `resource.excludeEventLabelKeys`. Default (keys absent) = no labels copied.
- **Edge cases**: Project lookup failure is logged as warning and only app labels are considered. Not applied to
  AppProject/ApplicationSet/resource events.
- **Configuration surface**: argocd-cm `resource.includeEventLabelKeys`, `resource.excludeEventLabelKeys`.
- **Exposure**: ConfigMap; Kubernetes Events.
- **Tests required**:
  - Positive: include `team,env*`, app label `team=a`, project label `env=prod` → event has both labels.
  - Positive: exclude `env*` removes `env` even if included.
  - Positive: conflicting key on app and project → app value used.

### OPS-093 ApplicationSet controller Kubernetes events
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/applicationset_controller.go#L753 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/applicationset_controller.go#L871 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/applicationset_controller.go#L965
- **Maturity**: GA
- **Semantics**: Through the controller-runtime recorder, the ApplicationSet controller emits Normal events on the
  ApplicationSet: reason = the controller-runtime create-or-update result (`created` or `updated`; `unchanged`
  emits no event to avoid etcd churn) with message `<action> Application "<name>"`, reason `Deleted` with `Deleted Application "<name>"`, and reason `Updated`
  with `Updated Application "<name>" finalizer before deletion, because application has an invalid destination`.
  Not affected by `--enable-k8s-event`. Requires `events` create/patch RBAC (OPS-007).
- **Edge cases**: Standard client-go event aggregation applies (unlike OPS-091).
- **Configuration surface**: none.
- **Exposure**: Kubernetes Events on ApplicationSet objects.
- **Tests required**:
  - Positive: appset generating a new app → Normal event with message `... Application "<name>"`.
  - Positive: generator element removed (policy sync) → `Deleted` event.

### OPS-094 Runtime diagnostics: periodic stats, stack and heap dumps
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-application-controller/commands/argocd_application_controller.go#L227 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-server/commands/argocd_server.go#L271 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-repo-server/commands/argocd_repo_server.go#L230 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-applicationset-controller/commands/applicationset_controller.go#L284
- **Maturity**: GA
- **Semantics**: Controller, API server, repo-server (and CMP server) call `stats.RegisterStackDumper()`,
  `stats.StartStatsTicker(10m)` and `stats.RegisterHeapDumper("memprofile")` from `github.com/argoproj/pkg/v2/stats`;
  the ApplicationSet controller only starts the 10-minute stats ticker. Behaviour of the external package (see
  Unconfirmed): periodic log of memory/goroutine statistics every 10 minutes; a signal-triggered goroutine stack dump and
  a signal-triggered heap profile written to file `memprofile`. Complemented by the pprof endpoints (OPS-056).
- **Edge cases**: The heap dump writes into the working directory; with read-only root filesystems the dump may fail.
- **Configuration surface**: none.
- **Exposure**: Pod logs; signals.
- **Tests required**:
  - Positive: after 10 minutes each listed component has logged one stats line.
  - Positive (if Patrol implements signals): sending the dump signal produces a goroutine dump in logs.

---

## G. TLS and mTLS

### OPS-095 API server settings watcher and in-process restart
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L803 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L785 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L436 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-server/commands/argocd_server.go#L276
- **Maturity**: GA
- **Semantics**: The API server subscribes to settings changes. It performs a graceful **in-process restart** (stop
  listeners, re-create servers, re-read settings, re-init tracing; the pod is not restarted) when any of these change:
  generated Dex config, OIDC config, `url`, `additionalUrls`, `webhook.github.secret`, `webhook.gitlab.secret`,
  `webhook.bitbucket.uuid`, `webhook.bitbucketserver.secret`, `webhook.gogs.secret`; logs e.g. `url modified.
  restarting`, then `shutting down settings watch`. Changes of `extension.config` only rebuild the proxy-extension
  registry (no restart). TLS certificate changes are hot-reloaded without restart (OPS-096). During the restart
  `/healthz` returns 503 "API Server is not available: it either hasn't started or is restarting".
- **Edge cases**: Azure DevOps webhook credentials are not in the restart list (read per request). In-flight requests
  are drained with a 20s shutdown timeout.
- **Configuration surface**: argocd-cm / argocd-secret keys above.
- **Exposure**: API availability.
- **Tests required**:
  - Positive: change `url` in argocd-cm → log `url modified. restarting`, new value effective (SSO redirect) without pod restart.
  - Positive: change `extension.config` → no restart, registry updated.
  - Recovery: continuous `/healthz` probe during restart shows a brief 503 then 200.

### OPS-096 API server TLS certificate sources and hot reload
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L535 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L1735 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L1771 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L1798 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L2457 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L649 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/tls.md#L42 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/tls.md#L62
- **Maturity**: GA
- **Semantics**: Priority: (1) Secret `argocd-server-tls` keys `tls.crt`/`tls.key` (type `kubernetes.io/tls` optional;
  safe for cert-manager/SealedSecrets); (2) `argocd-secret` `tls.crt`/`tls.key` (deprecated); (3) a self-signed
  certificate generated at bootstrap (org "Argo CD", RSA 2048, valid 1 year, SANs `localhost`, `argocd-server`,
  `argocd-server.<ns>`, `argocd-server.<ns>.svc`, `argocd-server.<ns>.svc.cluster.local`) persisted into `argocd-secret`
  (only when not insecure). The TLS listener resolves the certificate per handshake (`GetCertificate`) from current
  settings, so renewed certificates are served without restart (log `tls certificate modified. reloading certificate`).
  Parsed certificates are cached per Secret name + resourceVersion. A certificate loaded from `argocd-server-tls` is
  never written back to `argocd-secret`.
- **Edge cases**: A Secret with only one of the two keys is ignored (falls through to the next source). An unparsable
  key pair → settings error (incomplete settings) and the previous certificate stays in use. If neither secret has a
  cert and the server runs `--insecure`, no cert is generated.
- **Configuration surface**: Secrets `argocd-server-tls`, `argocd-secret`.
- **Exposure**: HTTPS/gRPC endpoint.
- **Tests required**:
  - Positive: create `argocd-server-tls` → served cert switches without restart (openssl s_client).
  - Positive: both secrets populated → `argocd-server-tls` wins.
  - Positive: fresh install → self-signed cert with the 5 SANs persisted in `argocd-secret`.
  - Negative: invalid PEM in `argocd-server-tls` → previous cert keeps being served, error logged.

### OPS-097 TLS protocol versions and cipher suites (`--tlsminversion`, `--tlsmaxversion`, `--tlsciphers`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/tls/tls.go#L31 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/tls/tls.go#L91 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/tls/tls.go#L102 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/tls/tls.go#L138 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/tls/tls.go#L190 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/tls.md#L50 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/tls.md#L105
- **Maturity**: GA
- **Semantics**: Available on `argocd-server` and `argocd-repo-server` (shared env names `ARGOCD_TLS_MIN_VERSION`,
  `ARGOCD_TLS_MAX_VERSION`, `ARGOCD_TLS_CIPHERS`; cmd-params `server.tls.*`, `reposerver.tls.*`). Versions `1.0|1.1|1.2|1.3`
  (defaults min `1.2`, max `1.3`); empty string = Go default. Ciphers: colon-separated Go cipher-suite names (default
  `TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384`); the special value `list` prints `Supported TLS ciphers:` with the versions of
  each suite and exits 0. When min version is 1.3, cipher configuration is ignored (warning `TLSv1.3 cipher suites are not
  configurable, ignoring value of --tlsciphers` if non-default).
- **Edge cases**: Invalid version → startup error `error retrieving TLS version by min version "<v>": <v> is not valid TLS
  version`; min > max → `minimum TLS version <a> must not be higher than maximum TLS version <b>`; unknown cipher →
  `invalid cipher suite specified: <name>`. Ciphers do not restrict TLS 1.3 suites when max is 1.3 (Go behaviour).
- **Configuration surface**: flags/env/cmd-params above.
- **Exposure**: TLS endpoints.
- **Tests required**:
  - Positive: min 1.3 → TLS 1.2 client handshake rejected.
  - Negative: `--tlsminversion 1.3 --tlsmaxversion 1.2` → startup failure with the exact message.
  - Positive: `--tlsciphers list` prints suites and exits 0.
  - Negative: unknown cipher name → startup failure.

### OPS-098 `--insecure` mode, HTTP→HTTPS redirect and single-port protocol multiplexing
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-server/commands/argocd_server.go#L307 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L909 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L610 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L627 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L1379 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/ingress.md#L3
- **Maturity**: GA
- **Semantics**: The API server serves gRPC, gRPC-Web, REST/JSON and the UI on one port (8080) using connection
  multiplexing. TLS on (default, a certificate exists, not insecure): plain HTTP/1.x requests on the port are answered by
  a redirect server with **307 Temporary Redirect** to `https://<Host><path>[?query]` (root path prefixed if missing);
  everything else is TLS-terminated (ALPN `http/1.1`, `h2`) and then split into HTTPS (HTTP/1.1) and gRPC (HTTP/2 with
  `content-type: application/grpc`). `--insecure` (`server.insecure`, `ARGOCD_SERVER_INSECURE`) disables TLS: HTTP/1.1 and
  h2c gRPC are served in clear text and no self-signed certificate is generated. Insecure mode is the documented mode
  behind TLS-terminating proxies/ingresses.
- **Edge cases**: HTTPS clients are expected to use HTTP/1.1 (h2 is used only by gRPC). In insecure mode, the CLI must
  use `--plaintext` (or `--grpc-web` through a proxy). The internal grpc-gateway loopback connection uses TLS with
  `InsecureSkipVerify` when TLS is on.
- **Configuration surface**: `server.insecure`; Service ports 80/443 → 8080.
- **Exposure**: Network endpoint.
- **Tests required**:
  - Positive: TLS mode, `curl -i http://server:8080/applications?x=1` → 307 with `Location: https://server:8080/applications?x=1`.
  - Positive: gRPC (CLI) and REST (curl) both work on the same port in TLS and in insecure mode.
  - Positive: `--insecure` → no `tls.crt` generated in `argocd-secret` (OPS-030) and HTTP served directly.

### OPS-099 Repo-server server certificate (`argocd-repo-server-tls`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/server.go#L46 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/server.go#L55 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/tls/tls.go#L433 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/repo-server/argocd-repo-server-deployment.yaml#L369 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/tls.md#L117
- **Maturity**: GA
- **Semantics**: Secret `argocd-repo-server-tls` (optional; keys `tls.crt`, `tls.key`, `ca.crt`) is mounted at
  `/app/config/reposerver/tls` (base path overridable with `ARGOCD_APP_CONF_PATH`). If both files exist the repo-server
  loads them (`Loading TLS configuration from cert=... and key=...`), otherwise it generates an in-memory self-signed
  certificate for hosts `localhost`, `reposerver` (`Generating self-signed TLS certificate for this session`). TLS
  options from OPS-097 apply. The certificate is read only at start: changes require a repo-server restart. The same
  Secret is mounted into clients for the legacy strict-TLS path (server `/app/config/server/tls`, controller
  `/app/config/controller/tls`, appset/notifications `/app/config/reposerver/tls`).
- **Edge cases**: Unreadable/mismatched key pair → startup failure `unable to initialize TLS configuration with cert=...`.
  The self-signed certificate cannot be verified by clients, hence clients default to non-validating TLS (OPS-101).
  Documented SANs required: `DNS:argocd-repo-server`, `DNS:argocd-repo-server.<ns>.svc`.
- **Configuration surface**: Secret `argocd-repo-server-tls`; `--disable-tls` / `reposerver.disable.tls`.
- **Exposure**: gRPC endpoint 8081.
- **Tests required**:
  - Positive: provide a CA-signed cert → served by repo-server after restart; strict clients connect.
  - Negative: update the Secret without restart → old cert still served (parity).
  - Positive: `reposerver.disable.tls: "true"` (with `reposerver.client.ca.path: ""`) → plaintext gRPC.

### OPS-100 Dex server TLS (`argocd-dex-server-tls`, `--disable-tls`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-dex/commands/argocd_dex.go#L82 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-dex/commands/argocd_dex.go#L146 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/dex/argocd-dex-server-deployment.yaml#L94 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/tls.md#L145
- **Maturity**: GA
- **Semantics**: `argocd-dex rundex` loads `/tls/tls.crt` + `/tls/tls.key` (Secret `argocd-dex-server-tls`, optional mount)
  or generates a self-signed certificate for `localhost`, `dexserver`, writes the PEMs to `/tmp/tls.crt` and `/tmp/tls.key`
  and generates a Dex config whose HTTPS listener uses them. `--disable-tls` (`dexserver.disable.tls`,
  `ARGOCD_DEX_SERVER_DISABLE_TLS`) makes Dex serve plain HTTP. Restart required for certificate changes.
- **Edge cases**: Failure to write the files → fatal `could not write TLS certificate: ...`. Documented SANs:
  `DNS:argocd-dex-server`, `DNS:argocd-dex-server.<ns>.svc`; self-signed `ca.crt` must be included for strict clients.
- **Configuration surface**: Secret `argocd-dex-server-tls`; `dexserver.disable.tls`.
- **Exposure**: Dex HTTPS 5556.
- **Tests required**:
  - Positive: default → Dex serves HTTPS with a self-signed cert; SSO login via the API server proxy succeeds.
  - Positive: `dexserver.disable.tls: "true"` + `server.dex.server.plaintext: "true"` → login works over HTTP.
  - Negative: Dex plaintext but server not plaintext → SSO login fails (TLS handshake error).

### OPS-101 Component → repo-server TLS verification (`--repo-server-plaintext`, `--repo-server-ca-cert-path`, deprecated `--repo-server-strict-tls`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/tls/tls.go#L513 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/apiclient/clientset.go#L164 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-server/commands/argocd_server.go#L161 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-application-controller/commands/argocd_application_controller.go#L150 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-applicationset-controller/commands/applicationset_controller.go#L222 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-notification/commands/argocd_notification.go#L130 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/tls.md#L32 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/tls.md#L184 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/3.4-3.5.md#L229
- **Maturity**: GA (`--repo-server-strict-tls`: Deprecated)
- **Semantics**: Clients: argocd-server, application controller, ApplicationSet controller, notifications controller.
  Modes: (a) plaintext — `--repo-server-plaintext` (notifications: `--argocd-repo-server-plaintext`) uses insecure gRPC
  credentials (repo-server must run `--disable-tls`); (b) default — TLS **without** verification (`InsecureSkipVerify`);
  (c) verified — `--repo-server-ca-cert-path <file>` (env `ARGOCD_<PREFIX>_REPO_SERVER_CA_CERT_PATH`, prefixes
  `SERVER`, `APPLICATION_CONTROLLER`, `APPLICATIONSET_CONTROLLER`, `NOTIFICATION_CONTROLLER`; cmd-params
  `<comp>.repo.server.ca.cert.path`) loads a CA pool and verifies the server certificate; (d) legacy strict —
  deprecated `--repo-server-strict-tls` (hidden; notifications `--argocd-repo-server-strict-tls`, no env) enables
  verification and, if no CA path was given, loads the mounted `argocd-repo-server-tls` `tls.crt`+`ca.crt` from the
  component-specific mount (OPS-099); both flags combine with OR semantics; an explicit CA path takes precedence over
  the legacy mount. RPC timeout `--repo-server-timeout-seconds` (60; notifications fixed 5s).
- **Edge cases**: CA file unreadable → startup error `error loading repo-server CA: ...`; legacy path unreadable → fatal.
  Deprecated flag prints a cobra deprecation warning `use --repo-server-ca-cert-path instead`. Notifications controller
  flag is `--repo-server-ca-cert-path` (shared helper), not `--argocd-repo-server-ca-cert-path` as documented (Doc/
  implementation differences).
- **Configuration surface**: flags/env/cmd-params above (OPS-020/022/026/027).
- **Exposure**: Internal gRPC.
- **Tests required**:
  - Positive: CA-signed repo-server cert + `--repo-server-ca-cert-path` on all 4 clients → all RPCs succeed.
  - Negative: CA path pointing to a different CA → `x509: certificate signed by unknown authority` errors in app conditions.
  - Positive: default (no CA) works against a self-signed repo-server.
  - Positive: plaintext on clients + `--disable-tls` on repo-server works; mismatched (plaintext client, TLS server) fails.
  - Positive: deprecated `--repo-server-strict-tls` still works and logs deprecation.

### OPS-102 API server → Dex TLS verification (`--dex-server-plaintext`, `--dex-server-strict-tls`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-server/commands/argocd_server.go#L192 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-server/commands/argocd_server.go#L334 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/dex/dex.go#L29 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/dex/dex.go#L125 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/tls.md#L232
- **Maturity**: GA
- **Semantics**: `--dex-server` (`ARGOCD_SERVER_DEX_SERVER`, `server.dex.server`, default `argocd-dex-server:5556`);
  `--dex-server-plaintext` (`server.dex.server.plaintext`) → `http://` scheme, no TLS; default → HTTPS without
  verification; `--dex-server-strict-tls` (`server.dex.server.strict.tls`) → verifies against `ca.crt` **and pins** the
  leaf: the presented certificate must byte-equal `/app/config/dex/tls/tls.crt` (mounted from `argocd-dex-server-tls`),
  else `dex server certificate does not match`. Used by the `/api/dex` reverse proxy and by the OIDC client.
- **Edge cases**: Strict mode with missing mounted files → fatal at start. Plaintext flag also changes the generated
  Dex config issuer expectations (dex config generated with TLS disabled).
- **Configuration surface**: keys/flags above.
- **Exposure**: Internal HTTPS.
- **Tests required**:
  - Positive: strict TLS with matching mounted cert → SSO works.
  - Negative: strict TLS while Dex serves a different (even CA-valid) cert → login fails with "dex server certificate does not match".

### OPS-103 Repo-server mutual TLS (`argocd-repo-server-mtls`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/tls/tls.go#L490 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/tls/tls.go#L554 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/server.go#L68 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/apiclient/clientset.go#L62 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/apiclient/clientset.go#L174 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-repo-server/commands/argocd_repo_server.go#L144 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-repo-server/commands/argocd_repo_server.go#L186 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/repo-server/argocd-repo-server-deployment.yaml#L378 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/mtls.md#L11 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/3.4-3.5.md#L186
- **Maturity**: GA (new in 3.5, opt-in)
- **Semantics**: Secret `argocd-repo-server-mtls` (optional) keys: `client-ca.crt` (projected only into the repo-server at
  `/app/config/reposerver/mtls/client-ca.crt`), `client.crt`, `client.key`, optional `server-ca.crt` (projected into
  server, controller StatefulSet, appset and notifications at `/app/config/reposerver/mtls/`). Server side:
  `--client-ca-path` (`ARGOCD_REPO_SERVER_CLIENT_CA_PATH`, `reposerver.client.ca.path`, default
  `/app/config/reposerver/mtls/client-ca.crt`): if the file exists, `RequireAndVerifyClientCert` with that CA bundle
  (log `mTLS enabled for repo-server: requiring client certificates from CA=<path>`); if absent, mTLS is silently
  skipped. The repo-server generates an ephemeral self-signed ECDSA P-256 client certificate (10 years,
  `Argo CD Health Check`) added to its client-CA pool for its own `/healthz?full=true` self-dial (log `Generated
  ephemeral health-check client certificate (CN=...)`). Client side: `--repo-server-client-cert-path` /
  `--repo-server-client-cert-key-path` (defaults the mounted `client.crt`/`client.key`; env/cmd-params per component);
  if the cert file exists at start it is eagerly loaded and then served via `GetClientCertificate`, re-read when the
  files' mtime changes (`Client certificate reloaded from <path> (mtime changed)`), and an empty certificate is
  presented if the file later disappears. Authentication only: any certificate signed by the client CA is accepted (no
  per-component identity).
- **Edge cases**: `--client-ca-path` with `--disable-tls` → startup error `--client-ca-path cannot be used when
  --disable-tls is enabled` (default path is non-empty). Only one of client cert/key flags set →
  `--repo-server-client-cert-key-path is required when --repo-server-client-cert-path is specified` (and vice versa).
  Cert file present but invalid → `failed to load initial client certificate: ...`. The dynamic-distribution controller
  Deployment variant does not mount `argocd-repo-server-mtls` (Doc/implementation differences). The `server-ca.crt` key
  is mounted but not used by default (`--repo-server-ca-cert-path` must point to it explicitly).
- **Configuration surface**: Secret `argocd-repo-server-mtls`; `reposerver.client.ca.path`;
  `<comp>.repo.server.client.cert.path`, `<comp>.repo.server.client.cert.key.path`.
- **Exposure**: Internal gRPC.
- **Tests required**:
  - Positive: create the Secret, restart → all clients connect with the shared client cert; repo-server logs mTLS enabled.
  - Negative: a client without a cert (e.g. grpcurl) → TLS handshake failure (bad certificate).
  - Positive: repo-server liveness `?full=true` stays healthy with mTLS on.
  - Recovery: rotate `client.crt`/`client.key` in the Secret → clients pick up the new cert without restart (mtime change).
  - Negative: `reposerver.disable.tls: "true"` with the default client CA path → startup error.

### OPS-104 Plaintext internal channels (commit-server gRPC, Redis default, metrics)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/commitserver/apiclient/clientset.go#L39 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-commit-server/commands/argocd_commit_server.go#L110 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/cache/cache.go#L235 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/security.md#L36
- **Maturity**: GA (commit-server: Beta)
- **Semantics**: The controller → commit-server gRPC connection always uses insecure (plaintext) credentials and the
  commit-server has no TLS options; access is restricted by NetworkPolicy to the controller (OPS-010). Redis traffic is
  plaintext unless `--redis-use-tls` and related flags are set (OPS-051). All `/metrics`, `/healthz` and pprof endpoints
  are plain HTTP. The security doc states that service-to-service traffic between server, repo-server and controller is
  TLS (true for repo-server gRPC, OPS-099/101).
- **Edge cases**: Service meshes can wrap these channels (documented approach for disabling internal TLS, OPS-099/101).
- **Configuration surface**: none for commit-server; Redis TLS flags.
- **Exposure**: Internal network.
- **Tests required**:
  - Positive: packet capture shows plaintext gRPC between controller and commit-server (parity) — or Patrol documents an added TLS option as extension.
  - Positive: Redis TLS flags enable TLS (OPS-051).

---

## H. Exposure: root path, HTTP surface flags and ingress patterns

### OPS-105 Server root path (`--rootpath`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-server/commands/argocd_server.go#L215 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-server/commands/argocd_server.go#L310 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L1143 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L1379 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L1253 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/ingress.md#L848 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/ingress.md#L894
- **Maturity**: GA
- **Semantics**: `--rootpath` (`ARGOCD_SERVER_ROOTPATH`, `server.rootpath`, default empty) serves the whole API server
  (REST, gRPC-Web, UI, `/auth`, `/api/dex`, `/swagger-ui`, `/download`, `/extensions`, webhooks) under `/<root>/` by
  stripping the prefix; `/healthz` is additionally served at the bare path for probes. When set, `--basehref` is forced
  to the root path (OPS-106). The HTTP→HTTPS redirect keeps/prepends the prefix. Native gRPC (HTTP/2) clients are not
  path-routed; CLI users behind a path-based proxy use `argocd login <host> --grpc-web-root-path /<root>` (OPS-107).
- **Edge cases**: Requests outside the prefix (other than `/healthz`) return 404. Leading/trailing slashes are trimmed
  (`/argo-cd/` ≡ `argo-cd`). When `--rootpath` is set, a warning `--basehref and --rootpath had conflict: ...` is logged
  whenever basehref differs from rootpath, including the default `/`.
- **Configuration surface**: `server.rootpath`.
- **Exposure**: HTTP routing; UI.
- **Tests required**:
  - Positive: `--rootpath /argo-cd` → UI at `/argo-cd/`, REST at `/argo-cd/api/v1/applications`, `/healthz` at both `/healthz` and `/argo-cd/healthz`.
  - Negative: `/api/v1/applications` without prefix → 404.
  - Positive: TLS mode redirect from `http://host/applications` → `https://host/argo-cd/applications`.
  - Positive: `argocd login host --grpc-web-root-path /argo-cd` works through a path-based proxy.

### OPS-106 UI base href (`--basehref`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-server/commands/argocd_server.go#L309 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L1433 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L1182 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/dex/dex.go#L59 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/ingress.md#L900
- **Maturity**: GA
- **Semantics**: `--basehref` (`ARGOCD_SERVER_BASEHREF`, `server.basehref`, default `/`) rewrites `<base href>` in the
  served `index.html` to `/<basehref>/` (computed once) and is used for logout redirects and the Dex reverse-proxy target
  path; the API itself keeps serving at `/`, so the fronting proxy must rewrite `/<basehref>/(.*)` → `/$1`
  (documented nginx example, with `proxy_buffering off` for streaming endpoints).
- **Edge cases**: `/` or empty leaves index.html unchanged. Ignored (overridden) when `--rootpath` is set.
- **Configuration surface**: `server.basehref`.
- **Exposure**: UI.
- **Tests required**:
  - Positive: `--basehref /argo-cd` → index.html contains `<base href="/argo-cd/">`, API still under `/api/...`.
  - Positive: behind an nginx rewrite the UI loads all assets and SSO logout returns to `/argo-cd/`.

### OPS-107 CLI connectivity through proxies (gRPC-Web fallback, root path, extra headers)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/root.go#L82 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/root.go#L86 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/ingress.md#L840 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/ingress.md#L1040
- **Maturity**: GA
- **Semantics**: Operator-facing contract for ingress designs: the CLI speaks native gRPC over HTTP/2 by default;
  `--grpc-web` switches to gRPC-Web over HTTP/1.1 (needed for L7 proxies without HTTP/2 upstream support, e.g. GKE
  ingress); `--grpc-web-root-path <path>` enables gRPC-Web under a sub-path; `--header/-H k:v` (repeatable or comma-
  separated) adds headers to every request so the CLI can pass through authenticating reverse proxies. Owner of the
  CLI transport semantics: 08 (API-053).
- **Edge cases**: The server accepts gRPC-Web on the main port (content type `application/grpc-web+proto`).
- **Configuration surface**: CLI global flags (persisted in the CLI config by `argocd login`).
- **Exposure**: CLI.
- **Tests required**:
  - Positive: behind an HTTP/1.1-only proxy, `argocd app list --grpc-web` succeeds while native gRPC fails.
  - Positive: proxy requiring header `x-token: foo` → `argocd login ... --header 'x-token:foo'` succeeds.

### OPS-108 API content-type enforcement (`--api-content-types`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-server/commands/argocd_server.go#L317 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L1215 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L1288 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cmd-params-cm.yaml#L157
- **Maturity**: GA
- **Semantics**: `--api-content-types` (`ARGOCD_API_CONTENT_TYPES`, `server.api.content.types`; semicolon-separated,
  default `application/json`, empty allowed) restricts non-GET requests under `/api/` to the listed `Content-Type`
  values (case-insensitive exact match) → otherwise `415 Invalid content type`. Empty list disables enforcement and logs
  a `security=3` CSRF warning. Owner of REST semantics: 08 (API-003).
- **Edge cases**: `application/json; charset=utf-8` does not match `application/json` (exact header comparison).
  Webhook (`/api/webhook`) and badge endpoints are registered outside this handler.
- **Configuration surface**: `server.api.content.types`.
- **Exposure**: REST API.
- **Tests required**:
  - Negative: POST `/api/v1/applications` with `text/plain` → 415.
  - Positive: GET with any content type passes; empty config allows `text/plain` POST and logs the warning.

### OPS-109 Security response headers (`--x-frame-options`, `--content-security-policy`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-server/commands/argocd_server.go#L329 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L1464 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L1484
- **Maturity**: GA
- **Semantics**: For UI/static responses: `X-Frame-Options` = `--x-frame-options` (`ARGOCD_SERVER_X_FRAME_OPTIONS`,
  `server.x.frame.options`, default `sameorigin`; empty disables the header); `Content-Security-Policy` =
  `--content-security-policy` (`ARGOCD_SERVER_CONTENT_SECURITY_POLICY`, manifest key `server.content.security.policy`,
  default `frame-ancestors 'self';`; empty disables); `X-XSS-Protection: 1` always. index.html is served with no-cache
  headers; hashed `main.<20 hex>.js` bundles get `Cache-Control: public, max-age=31536000, immutable`. Owner: 08 (API-013).
- **Edge cases**: Headers are not added to `/api/` JSON responses.
- **Configuration surface**: keys above.
- **Exposure**: HTTP headers.
- **Tests required**:
  - Positive: GET `/` → `X-Frame-Options: sameorigin`, `Content-Security-Policy: frame-ancestors 'self';`.
  - Positive: `server.x.frame.options: ""` → header absent.

### OPS-110 Response compression (`--enable-gzip`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-server/commands/argocd_server.go#L318 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L1160 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L1201
- **Maturity**: GA
- **Semantics**: `--enable-gzip` (`ARGOCD_SERVER_ENABLE_GZIP`, `server.enable.gzip`, default **true**) wraps the REST
  gateway, `/extensions.js` and static assets in a compression handler (gzip/deflate by `Accept-Encoding`), except
  requests with `Accept: text/event-stream` (SSE streams are never compressed). The core-mode local server disables gzip.
- **Edge cases**: gRPC and gRPC-Web responses are not affected.
- **Configuration surface**: `server.enable.gzip`.
- **Exposure**: HTTP.
- **Tests required**:
  - Positive: `Accept-Encoding: gzip` on `/api/v1/applications` → `Content-Encoding: gzip`.
  - Negative: SSE watch with `Accept: text/event-stream` → uncompressed.
  - Positive: `server.enable.gzip: "false"` → never compressed.

### OPS-111 Additional static assets directory (`--staticassets`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-server/commands/argocd_server.go#L308 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L364 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/3.0-3.1.md#L3
- **Maturity**: GA
- **Semantics**: The UI is embedded in the binary; `--staticassets` (`ARGOCD_SERVER_STATIC_ASSETS`, `server.staticassets`,
  default `/shared/app`) is overlaid via a composable FS rooted with `os.OpenRoot`, so operator files (e.g. custom CSS
  referenced by `ui.cssurl`, logos) are served alongside embedded assets. Root-confined access means symlinks pointing
  outside the directory fail (3.1 behaviour change: HTTP 500).
- **Edge cases**: Non-existent directory → warning `Static assets directory "<d>" does not exist, using only embedded
  assets`; other open errors are fatal. The 3.1 upgrade note names the default `/app/shared` while code uses
  `/shared/app` (Doc/implementation differences).
- **Configuration surface**: `server.staticassets`; volume mounted into argocd-server.
- **Exposure**: HTTP static files.
- **Tests required**:
  - Positive: mount `custom.css` into `/shared/app` → served at `/custom.css`.
  - Negative: symlink to `/etc/passwd` inside the directory → 500, file content not disclosed.

### OPS-112 CLI binary download endpoint (`/download/argocd-linux[-<arch>]`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L1266 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L1416
- **Maturity**: GA
- **Semantics**: If an `argocd` binary is on the server's PATH, `GET /download/argocd-linux` and
  `/download/argocd-linux-<GOARCH>` serve it (unauthenticated `http.ServeFile`); other platforms are offered via
  argocd-cm `help.download.*` links (owner 08, API-014/API-109).
- **Edge cases**: Binary missing → routes not registered (404) and warning `argocd not in PATH`. Mixed-arch server
  replicas each serve their own arch on the arch-agnostic route.
- **Configuration surface**: none.
- **Exposure**: HTTP; UI help page.
- **Tests required**:
  - Positive: download returns an executable matching the server version/arch.

### OPS-113 Ingress pattern: TLS passthrough to the API server
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/ingress.md#L231 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/ingress.md#L267 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/ingress.md#L1100
- **Maturity**: GA
- **Semantics**: A single hostname for UI, REST and gRPC by passing TLS through to `argocd-server` (port `https`/443),
  which terminates TLS with `argocd-server-tls` and multiplexes protocols (OPS-098). Documented recipes: ingress-nginx
  Ingress with `nginx.ingress.kubernetes.io/ssl-passthrough: "true"` + `force-ssl-redirect: "true"` (controller needs
  `--enable-ssl-passthrough`; ingress-nginx is marked retired/archived); cert-manager variant adds
  `backend-protocol: HTTPS`; Gateway API `TLSRoute` (experimental channel) attached to a `Passthrough` TLS listener.
- **Edge cases**: cert-manager cannot issue certificates for passthrough Gateway listeners (doc note); TLS must stay
  enabled on the server (not `--insecure`).
- **Configuration surface**: Ingress/Gateway objects; `argocd-server-tls`.
- **Exposure**: Network.
- **Tests required**:
  - Positive: passthrough ingress → browser UI and `argocd login <host>` (native gRPC) both work on one hostname.

### OPS-114 Ingress pattern: TLS termination at the proxy with a separate gRPC host
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/ingress.md#L305 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/ingress.md#L375 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/ingress.md#L93
- **Maturity**: GA
- **Semantics**: When the ingress terminates TLS and supports one backend protocol per Ingress, two Ingresses are used:
  HTTP host (e.g. `argocd.example.com`, backend port `http`, `backend-protocol: HTTP`) and gRPC host
  (`grpc.argocd.example.com`, backend port `https`, `backend-protocol: GRPC`), each with its own TLS secret; the API
  server runs `--insecure` (h2c gRPC upstream). Same pattern documented for F5 NGINX (requires HTTP/2 listener) and
  Contour (separate internal/external Ingress classes, gRPC host annotated through the Service
  `projectcontour.io/upstream-protocol.h2c: "https,443"`).
- **Edge cases**: CLI must log in to the gRPC host (or use `--grpc-web` against the HTTP host).
- **Configuration surface**: Ingress objects; `server.insecure`.
- **Exposure**: Network.
- **Tests required**:
  - Positive: with `--insecure`, UI through the HTTP host and `argocd login grpc.<host>` through the gRPC host both work.

### OPS-115 Ingress pattern: Traefik IngressRoute (single host, header-based gRPC routing)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/ingress.md#L440 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/ingress.md#L448
- **Maturity**: GA
- **Semantics**: Traefik v3 terminates TLS; one `IngressRoute` with two rules on the same host: default rule →
  `argocd-server:80`; rule matching `Header(Content-Type, application/grpc)` (higher priority) → `argocd-server:80` with
  `scheme: h2c`. API server runs `--insecure`.
- **Edge cases**: Requires h2c support from the server in insecure mode (OPS-098).
- **Configuration surface**: IngressRoute CRD.
- **Exposure**: Network.
- **Tests required**:
  - Positive: single hostname serves UI and native gRPC CLI.

### OPS-116 Ingress pattern: AWS ALB with a dedicated gRPC target group
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/ingress.md#L476 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/ingress.md#L486
- **Maturity**: GA
- **Semantics**: Extra Service (e.g. `argogrpc`, NodePort, port 443 → 8080) annotated
  `alb.ingress.kubernetes.io/backend-protocol-version: GRPC`; the Ingress (`backend-protocol: HTTPS`) routes requests with
  header `Content-Type: application/grpc` to it via `alb.ingress.kubernetes.io/conditions.argogrpc` (gRPC path listed
  before the UI path) and uses gRPC health check `/grpc.health.v1.Health/Check` with success code `0`. Classic ELB/NLB can
  be used as L4 balancers.
- **Edge cases**: Requires the API server to implement the standard gRPC health service (owner 08, API-011) and to
  downgrade non-gRPC calls to HTTP/1.
- **Configuration surface**: Service + Ingress annotations.
- **Exposure**: Network.
- **Tests required**:
  - Positive: gRPC health check `grpc.health.v1.Health/Check` returns SERVING on port 8080 (TLS on).

### OPS-117 Ingress pattern: GKE (NEG, BackendConfig, FrontendConfig)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/ingress.md#L672 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/ingress.md#L717 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/ingress.md#L740
- **Maturity**: GA
- **Semantics**: API server `--insecure` (avoids redirect loops); Service with `cloud.google.com/neg` and
  `cloud.google.com/backend-config` annotations; BackendConfig HTTP health check `GET /healthz` on port 8080
  (interval 30s, timeout 5s); FrontendConfig `redirectToHttps`; Ingress with TLS secret (path `/*` with
  `ImplementationSpecific` for GKE < 1.21.3-gke.1600, else `/` Prefix). The GCLB path is HTTP/1.1 → CLI uses `--grpc-web`.
- **Edge cases**: `/healthz` must answer 200 over plain HTTP in insecure mode.
- **Configuration surface**: GKE CRDs and annotations.
- **Exposure**: Network.
- **Tests required**:
  - Positive: `/healthz` returns 200 over HTTP with `--insecure`; CLI with `--grpc-web` works through an HTTP/1.1-only LB.

### OPS-118 Ingress pattern: Istio with a sub-path
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/ingress.md#L546
- **Maturity**: GA
- **Semantics**: Kustomize patch runs the API server with `--insecure --basehref /argocd --rootpath /argocd` and env
  `ARGOCD_MAX_CONCURRENT_LOGIN_REQUESTS_COUNT=0`; an Istio Gateway/VirtualService routes `/argocd` to `argocd-server:80`.
- **Edge cases**: Login concurrency limit disabled in the example (OPS-034).
- **Configuration surface**: server flags; Istio objects.
- **Exposure**: Network.
- **Tests required**:
  - Positive: UI and REST reachable under `/argocd` through the mesh ingress.

### OPS-119 Ingress pattern: Ambassador / Emissary Mappings
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/ingress.md#L11 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/ingress.md#L20 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/ingress.md#L53
- **Maturity**: GA
- **Semantics**: Host-based: two Mappings — UI (`host: argocd.example.com` → `https://argocd-server:443`) and CLI
  (`host: argocd.example.com:443`, `regex_headers Content-Type ^application/grpc.*$`, `grpc: true` → `argocd-server:80`)
  because the CLI sends the port in the Host header; API server `--insecure` (not required with gRPC-Web). Path-based:
  `--rootpath /argo-cd` and one Mapping with `prefix`/`rewrite` `/argo-cd`; CLI uses `--grpc-web-root-path /argo-cd`.
- **Edge cases**: With `strip_matching_host_port` the port in the CLI mapping must be omitted.
- **Configuration surface**: Mapping CRDs; server flags.
- **Exposure**: Network.
- **Tests required**:
  - Positive: CLI Host header includes `:443` when connecting to `host:443` (Patrol CLI parity for host header).

### OPS-120 Ingress pattern: Contour HTTPProxy (same host, h2c)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/ingress.md#L185 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/ingress.md#L192
- **Maturity**: GA
- **Semantics**: With `--insecure`, a Contour `HTTPProxy` can serve REST and gRPC on one hostname by routing the gRPC
  route to the service with `protocol: h2c`.
- **Edge cases**: Same h2c dependency as OPS-115.
- **Configuration surface**: HTTPProxy CRD.
- **Exposure**: Network.
- **Tests required**:
  - Positive: single hostname via HTTPProxy works for UI and CLI.

### OPS-121 Ingress pattern: Gateway API (HTTPRoute, GRPCRoute, BackendTLSPolicy)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/ingress.md#L949 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/ingress.md#L985 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/ingress.md#L1013 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/ingress.md#L1038 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/ingress.md#L1071
- **Maturity**: GA
- **Semantics**: Gateway terminates TLS; `HTTPRoute` → `argocd-server:80` for the UI/REST host; `GRPCRoute` →
  `argocd-server:443` on a separate gRPC host with the Service port `appProtocol: kubernetes.io/h2c` (Helm value
  `server.service.servicePortHttpsAppProtocol`); optional `BackendTLSPolicy` to keep TLS to the server (validating
  against a CA ConfigMap and hostname `argocd-server.argocd.svc.cluster.local`) instead of `--insecure`. Same-host
  HTTPRoute+GRPCRoute is implementation-dependent and needs header matches. TLS passthrough variant: OPS-113.
- **Edge cases**: The upstream install manifests do not set `appProtocol` on the Service (Helm-only value).
- **Configuration surface**: Gateway API objects; Service `appProtocol`.
- **Exposure**: Network.
- **Tests required**:
  - Positive: GRPCRoute with h2c backend + `--insecure` → native gRPC CLI works; HTTPRoute serves UI.
  - Positive: BackendTLSPolicy with the server certificate's CA keeps end-to-end TLS.

---

## I. Core mode, local dashboard, multi-instance and self-management

### OPS-122 Core (headless) mode CLI (`argocd login --core`, `--core`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/headless/headless.go#L180 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/headless/headless.go#L264 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/headless/headless.go#L56 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/headless/headless.go#L125 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/core.md#L64 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/core.md#L10
- **Maturity**: GA
- **Semantics**: With `--core` (or a CLI context saved by `argocd login --core`), each CLI invocation starts an
  in-process API server bound to `localhost:<random port>` with `DisableAuth=true`, `Insecure=true`, gzip off, proxy
  extensions off, using the user's kubeconfig (namespace = kubeconfig context namespace). Redis: an embedded in-memory
  Redis (miniredis) for server state plus a lazily port-forwarded connection to the cluster's Redis
  (`app.kubernetes.io/name=argocd-redis-ha-haproxy` or `argocd-redis`, port 6379, password from the `argocd-redis`
  Secret) for app-state cache; repo-server access is port-forwarded on demand to pod port 8081. The CLI then talks
  plaintext gRPC to the local server; the server shuts down when the command ends. Authorization is purely Kubernetes
  RBAC of the caller; Argo CD RBAC, API, notifications and OIDC are unavailable. Owner of CLI flag semantics: 08 (API-055).
- **Edge cases**: The kubeconfig context must point at the Argo CD namespace (`kubectl config set-context --current
  --namespace=argocd`), else listing returns nothing/forbidden. 5 connectivity attempts at 1s interval → `all retries
  failed: ...`. Missing Redis password → warning, continues. Logging forced to error level.
- **Configuration surface**: CLI `--core`; kubeconfig; CLI flags `--redis-haproxy-name`, `--redis-name`,
  `--repo-server-name`, `--redis-compress` (owner 08).
- **Exposure**: CLI.
- **Tests required**:
  - Positive: on a core install, `argocd login --core` then `argocd app list`/`app sync` work with only kubeconfig credentials.
  - Authorization: a kube user without `get applications` gets a forbidden error.
  - Positive: commands needing manifests (`app diff`) reach the repo-server via port-forward.

### OPS-123 Local web UI (`argocd admin dashboard`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/dashboard.go#L42 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/dashboard.go#L62 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L86 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/core.md#L92
- **Maturity**: GA
- **Semantics**: Starts the core-mode local API server (OPS-122) on `--address` (default `localhost`) / `--port` (default
  8080) and prints `Argo CD UI is available at http://<address>:<port>`; serves the full UI without login (auth
  disabled) until SIGINT/SIGTERM, then shuts down (`clean shutdown`). Accepts kubectl flags (`-n argocd`, `--context`)
  and cache flags (e.g. `--redis-compress`).
- **Edge cases**: Binding to a non-loopback address exposes an unauthenticated UI with the caller's Kubernetes
  privileges. Port in use → `could not start dashboard: ...`.
- **Configuration surface**: CLI flags.
- **Exposure**: CLI; local UI.
- **Tests required**:
  - Positive: `argocd admin dashboard -n argocd --port 8085` → UI lists apps without login.
  - Negative: occupied port → error exit.

### OPS-124 Managed-by URL annotation (`argocd.argoproj.io/managed-by-url`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/application_annotations.go#L15 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/cache/info.go#L787 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L407 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1391 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/deeplinks/deeplinks.go#L65 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/shared/utils.ts#L44 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/managed-by-url.md#L5
- **Maturity**: GA
- **Semantics**: Annotation on an Application naming the URL of the Argo CD instance that manages it (multi-instance
  app-of-apps / hub-and-spoke). The controller copies it into the resource-tree node info (`managed-by-url`) for child
  Application nodes; the UI links child apps to `<managed-by-url>/applications/<ns>/<name>` (external link) instead of the
  local instance when the value is a valid `http:`/`https:` URL, otherwise falls back to the local URL (console warning,
  invalid-URL badge "managed-by-url: invalid url provided"). Deep links get `managedByURL` in their template context when
  valid. API create/update validation rejects non-http(s) values with InvalidArgument `application spec for <app> is
  invalid: ... invalid managed-by URL: URL must include http or https protocol` (owner of UI rendering: 08, API-115).
- **Edge cases**: Validation applies only through the Argo CD API; Applications created by kubectl/GitOps sync (the
  documented app-of-apps flow) are not validated, only rendered defensively by the UI. Empty value = no annotation.
- **Configuration surface**: Application annotation.
- **Exposure**: CRD; API validation; UI.
- **Tests required**:
  - Positive: child app with `https://secondary.example.com` → tree node info contains `managed-by-url`, UI link targets the secondary instance.
  - Negative: API create with `javascript:alert(1)` → InvalidArgument with the message above.
  - Negative: same invalid value applied via kubectl → accepted by Kubernetes, UI falls back to local link.

### OPS-125 Self-managed Argo CD (managing Argo CD with an Application)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/declarative-setup.md#L1471 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/3.2-3.3.md#L13
- **Maturity**: GA
- **Semantics**: Argo CD can be deployed by an Application in its own namespace whose source is a Kustomize overlay
  over the upstream `manifests/cluster-install` (plus repo/cluster Secrets, ingress, config patches). Since 3.3 the
  Application must use `syncOptions: [ServerSideApply=true]` (conflict resolution added automatically) because of the
  ApplicationSet CRD size (OPS-006).
- **Edge cases**: Syncing the Argo CD Application restarts the controller performing the sync (operation resumes after
  restart). Without SSA, sync fails with the 262144-byte annotation error.
- **Configuration surface**: Application spec.
- **Exposure**: CRD.
- **Tests required**:
  - Positive: self-managed Patrol upgrade via an Application with SSA completes and the app ends Synced/Healthy.
  - Recovery: controller restart mid-sync → operation resumes/completes.

---

## J. Operator troubleshooting tools (`argocd admin`)

### OPS-126 `argocd admin settings validate` (and settings source flags)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/settings.go#L153 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/settings.go#L67 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/settings.go#L199 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/settings.go#L268 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/commands/argocd_admin_settings_validate.md#L1 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/troubleshooting.md#L12
- **Maturity**: GA
- **Semantics**: `argocd admin settings` (persistent flags `--argocd-cm-path`, `--argocd-secret-path`,
  `--load-cluster-settings`, plus kubectl flags) builds an offline settings manager over a fake clientset: argocd-cm from
  the file, else from the cluster when `--load-cluster-settings`; argocd-secret from the file, else from the cluster when
  `--load-cluster-settings`, else a stub (`admin.password`/`server.secretkey` = `test`). `validate [--group <g>]...`
  runs validators for groups `accounts` (`<n> accounts`), `general` (Dex/OIDC config parse, `url` missing hint,
  instance label key, help, Google Analytics; summary `Dex is configured`/`OIDC is configured`/`SSO is not configured`),
  `kustomize` (build options / `(<n> versions)`), `resource-overrides` (`<n> resource overrides`); default all groups
  sorted. Output per group `✅ <group>` + summary or `❌ <group>` + error, followed by captured debug logs. Sibling
  groups: `settings rbac` (owner 07), `resource-overrides` (OPS-127..130).
- **Edge cases**: Neither `--argocd-cm-path` nor `--load-cluster-settings` → error `either --argocd-cm-path must be
  provided or --load-cluster-settings must be set to true`. An unknown `--group` (e.g. `plugins`, used in the command's
  own example) has no validator (nil function → crash) (Doc/implementation differences). Exit code is 0 even when a group
  fails.
- **Configuration surface**: CLI flags; local YAML files.
- **Exposure**: CLI.
- **Tests required**:
  - Positive: valid argocd-cm file → all four groups ✅.
  - Negative: malformed `dex.config` → `❌ general` with `invalid dex.config: ...`.
  - Negative: no source flags → the error above.

### OPS-127 `argocd admin settings resource-overrides ignore-differences`
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/settings.go#L344 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/settings.go#L390 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/troubleshooting.md#L20
- **Maturity**: GA
- **Semantics**: `ignore-differences RESOURCE_YAML_PATH` loads the resource, looks up the override for `<group>/<Kind>`
  (core: `<Kind>`) and applies its `ignoreDifferences` jsonPointers/jqPathExpressions with the ignore normalizer; prints
  `Following fields are ignored:` and a diff of original vs normalized, or `No fields are ignored by ignoreDifferences
  settings: ...`, or `Ignore differences are not configured for '<group>/<kind>'`.
- **Edge cases**: `managedFieldsManagers` rules are not evaluated (needs live objects). `.all` overrides are not merged by
  this lookup (key lookup only). Missing argument → help + exit 1.
- **Configuration surface**: argocd-cm `resource.customizations.ignoreDifferences.*`.
- **Exposure**: CLI.
- **Tests required**:
  - Positive: Deployment + rule `/spec/replicas` → diff shows replicas removed.
  - Negative: kind without rules → "not configured" message.

### OPS-128 `argocd admin settings resource-overrides ignore-resource-updates`
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/settings.go#L365 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/settings.go#L440
- **Maturity**: GA
- **Semantics**: Same as OPS-127 but uses the effective ignore-resource-updates overrides (OPS-063: ignoreResourceUpdates
  merged with ignoreDifferences when enabled, plus the three metadata defaults). Messages: `No overrides configured for
  '<group>/<kind>'`, `Ignore resource updates are not configured for ...`, `No fields are ignored by ignoreResourceUpdates
  settings: ...`, or the diff. Flag `--ignore-normalizer-jq-execution-timeout` (default normalizer timeout).
- **Edge cases**: Because `*/*` metadata defaults always exist, kinds without explicit rules still resolve only through
  their own key (per-kind lookup).
- **Configuration surface**: argocd-cm ignoreResourceUpdates keys.
- **Exposure**: CLI.
- **Tests required**:
  - Positive: ExternalSecret with `/status/refreshTime` rule → diff shows the field removed.

### OPS-129 `argocd admin settings resource-overrides health`
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/settings.go#L488 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/troubleshooting.md#L32
- **Maturity**: GA
- **Semantics**: `health RESOURCE_YAML_PATH` evaluates resource health with the configured Lua overrides plus built-in
  health checks (owner of health semantics: 04) and prints `STATUS: <status>` and `MESSAGE: <message>`, or `Health script
  is not configured for '<group>/<kind>'` when no health is determinable.
- **Edge cases**: Lua errors are fatal (non-zero exit).
- **Configuration surface**: argocd-cm `resource.customizations.health.*`.
- **Exposure**: CLI.
- **Tests required**:
  - Positive: custom health script returning Degraded → `STATUS: Degraded`.
  - Positive: built-in kind (Deployment) without overrides still reports status.

### OPS-130 `argocd admin settings resource-overrides list-actions` / `run-action`
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/settings.go#L521 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/settings.go#L565 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/troubleshooting.md#L42
- **Maturity**: GA
- **Semantics**: `list-actions RESOURCE_YAML_PATH` runs the action discovery Lua and prints a sorted `NAME DISABLED`
  table. `run-action RESOURCE_YAML_PATH ACTION` (alias `action`; `--param k=v` repeatable) executes the action Lua offline
  and prints `Following fields have been changed:` + diff for patch operations, `Following resource was created:` + YAML
  for create operations. Owner of action semantics: 04/05.
- **Edge cases**: Only works when an `actions` override exists for the kind (`Actions are not configured for ...`) even
  though built-in actions exist for some kinds. Bad `--param` → fatal `Invalid parameter format: <p>`. Unknown action
  name → error from the Lua VM. The command examples use `resource-overrides action list` which does not exist
  (Doc/implementation differences).
- **Configuration surface**: argocd-cm `resource.customizations.actions.*`.
- **Exposure**: CLI.
- **Tests required**:
  - Positive: Deployment with a configured `restart` action → listed; run prints the annotation diff.
  - Negative: `--param bad` → fatal message.

### OPS-131 `argocd admin app generate-spec`
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/app.go#L77 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/app.go#L131 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/commands/argocd_admin_app_generate-spec.md#L1
- **Maturity**: GA
- **Semantics**: Builds an Application manifest offline from the same flags as `argocd app create` (owner 08) and prints it
  (`-o yaml|json`, default yaml); `-f/--file` uses a manifest file/URL as base, `-i/--inline` writes the result back to that
  file, `--set-finalizer` adds the resources finalizer, `-l/--label`, `--annotations`. No API call.
- **Edge cases**: `--name` is deprecated (ignored when a file is given). Multiple apps in the file → all printed.
- **Configuration surface**: CLI flags.
- **Exposure**: CLI.
- **Tests required**:
  - Positive: generated YAML equals the spec `argocd app create` would submit for the same flags (golden).
  - Positive: `--inline` rewrites the file.

### OPS-132 `argocd admin app get-reconcile-results` / `diff-reconcile-results`
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/app.go#L250 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/app.go#L315 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/app.go#L170
- **Maturity**: GA
- **Semantics**: `get-reconcile-results PATH` writes `{applications: [{name, health, sync, conditions}]}` for all apps
  in the namespace (label selector `--l`, output `--o yaml|json`). Without `--refresh` it copies current status from the
  CRs; with `--refresh` it recomputes reconciliation locally (in-process live state cache using the kubeconfig, repo-server
  at `--repo-server` or auto port-forward to the repo-server pod on 8081 with non-validating TLS), optionally
  `--server-side-diff`, `--ignore-normalizer-jq-execution-timeout`. `diff-reconcile-results PATH1 PATH2` prints, per app
  name (sorted), a diff of the two result entries (apps missing on one side shown as added/removed). Used to compare
  behaviour across versions/settings before upgrading.
- **Edge cases**: Flags are literally `--l` and `--o` (double-dash single letters). Wrong arg count → help + exit 1.
- **Configuration surface**: CLI flags.
- **Exposure**: CLI.
- **Tests required**:
  - Positive: two snapshots differing in one app's sync status → diff shows only that app.
  - Positive: `--refresh` produces results without the controller running.

### OPS-133 `argocd admin cluster stats`
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/cluster.go#L457 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/cluster.go#L81 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/cluster.go#L167
- **Maturity**: GA
- **Semantics**: Lists every cluster with `SERVER SHARD CONNECTION NAMESPACES COUNT APPS COUNT RESOURCES COUNT`. Shard is
  computed client-side with the selected `--sharding-method` (default legacy) and `--replicas` (default = number of pods
  labelled `app.kubernetes.io/name=<app controller name>`); `--shard N` filters. Cluster info (connection, counts) is
  read from Redis, by default through an automatic port-forward to the HAProxy/Redis pod (`--port-forward-redis`, default
  true; else cache flags `--redis` etc. are used). Clusters processed in batches of 10 concurrently.
- **Edge cases**: With replicas 0 all clusters show shard 0. Redis password read from `argocd-redis` (warning if absent).
  The computed shard ignores dynamic-distribution ConfigMap state. Positional args in the documented example are ignored.
- **Configuration surface**: CLI flags.
- **Exposure**: CLI.
- **Tests required**:
  - Positive: 2 controller pods, 4 clusters → shard column equals the legacy algorithm output (OPS-041).
  - Positive: `--shard 1` filters rows.

### OPS-134 `argocd admin cluster shards`
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/cluster.go#L178 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/cluster.go#L233
- **Maturity**: GA
- **Semantics**: Same inputs as OPS-133; prints `SHARD RESOURCES COUNT` with each shard's cached resource count and its
  percentage of the per-shard average (`<n> (<p>%)`), to evaluate balance.
- **Edge cases**: No controller pods and no `--replicas` → prints nothing (exit 0). No clusters → nothing.
- **Configuration surface**: CLI flags.
- **Exposure**: CLI.
- **Tests required**:
  - Positive: balanced 2 shards → both near 100%.

### OPS-135 `argocd admin cluster kubeconfig`
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/cluster.go#L520 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/troubleshooting.md#L55
- **Maturity**: GA
- **Semantics**: `kubeconfig CLUSTER_URL OUTPUT_PATH` reads the cluster Secret (by server URL) and writes a kubeconfig
  reproducing Argo CD's REST config (bearer token, TLS, exec/AWS auth) with the current namespace, to troubleshoot
  connectivity (`kubectl get pods -v 9` with it, typically from inside the controller pod).
- **Edge cases**: Requires exactly two args (help + exit 1 otherwise); examples in the command help with one/zero args or
  `--delete` are not implemented (Doc/implementation differences). Unknown server → NotFound error.
- **Configuration surface**: CLI.
- **Exposure**: CLI.
- **Tests required**:
  - Positive: generated kubeconfig authenticates against the managed cluster.
  - Negative: unknown URL → error exit.

### OPS-136 `argocd admin cluster generate-spec`
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/cluster.go#L566 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/cluster.go#L693
- **Maturity**: GA
- **Semantics**: Generates the declarative cluster Secret for a kubeconfig context offline (fake clientset in namespace
  `argocd`): flags of `argocd cluster add` (owner 03) plus `--bearer-token` (default placeholder `bearer-token`),
  `--generate-bearer-token` (creates the `argocd-manager` SA/RBAC and fetches a token from the target cluster),
  `--service-account` (`argocd-manager`), `--system-namespace` (`kube-system`), `--label`, `--annotation`, `-o yaml|json`.
- **Edge cases**: Unknown context → fatal `Context <c> does not exist in kubeconfig`; `--in-cluster` with
  `--cluster-endpoint` → fatal `Can only use one of --in-cluster or --cluster-endpoint`.
- **Configuration surface**: CLI.
- **Exposure**: CLI.
- **Tests required**:
  - Positive: output Secret has `argocd.argoproj.io/secret-type: cluster` and the context's server.

### OPS-137 `argocd admin cluster namespaces` (+ `enable-namespaced-mode`, `disable-namespaced-mode`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/cluster.go#L252 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/cluster.go#L309 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/cluster.go#L347 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/cluster.go#L402
- **Maturity**: GA
- **Semantics**: `namespaces` prints `CLUSTER NAMESPACES` — namespaces actually used by apps per cluster (from
  `status.resources`, else destination namespace; shortest first, first 4 + `(total N)`). `enable-namespaced-mode PATTERN`
  sets `namespaces` (and `--cluster-resources`) on clusters whose URL glob-matches and whose namespace count is ≤
  `--max-namespace-count` (default 0 → effectively never unless set); `disable-namespaced-mode PATTERN` clears
  `namespaces`. Both default to `--dry-run=true` (prints `... done (dry run)`).
- **Edge cases**: Clusters with zero used namespaces are skipped by enable. Apps with unresolvable destinations abort with
  `error validating application destination`.
- **Configuration surface**: CLI.
- **Exposure**: CLI; modifies cluster Secrets.
- **Tests required**:
  - Positive: enable with `--max-namespace-count 5 --dry-run=false` → cluster Secret gains the namespaces list.
  - Positive: default dry run makes no changes.

### OPS-138 `argocd admin repo generate-spec`
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/repo.go#L92 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/repo.go#L197
- **Maturity**: GA
- **Semantics**: Generates the repository Secret offline (fake clientset, namespace `argocd`) from the same flags as
  `argocd repo add` (owner 03); prints with `-o yaml|json`. Prompts for the password when `--username` is given without
  `--password`.
- **Edge cases**: Helm repo without `--name` → `must specify --name for repos of type 'helm'`; SSH key with non-SSH URL,
  TLS client cert/key mismatch, bearer-token combination errors as in `repo add`.
- **Configuration surface**: CLI.
- **Exposure**: CLI.
- **Tests required**:
  - Positive: output Secret has `argocd.argoproj.io/secret-type: repository` and the URL-derived name.
  - Negative: helm repo without name → error.

### OPS-139 `argocd admin proj generate-spec` / `update-role-policy`
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/project.go#L42 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/project.go#L146
- **Maturity**: GA
- **Semantics**: `generate-spec PROJECT` builds an AppProject offline from `argocd proj create` flags (`-o`, `-f`, `-i`).
  `update-role-policy PROJECT_GLOB MODIFICATION ACTION` bulk-edits project role policies: `set` (requires `--resource`,
  `--scope`, `--permission`) adds or replaces the policy for ACTION in roles matching `--role` (default `*`); `remove`
  deletes it; `--dry-run` defaults to **true**. Owner of project/RBAC semantics: 07.
- **Edge cases**: Only policies with exactly 6 comma-separated parts are matched.
- **Configuration surface**: CLI.
- **Exposure**: CLI; modifies AppProjects.
- **Tests required**:
  - Positive: `update-role-policy '*' set 'action/*' --role '*deployer*' --resource applications --scope '*' --permission allow --dry-run=false` adds the policy to matching roles only.

### OPS-140 `argocd admin proj generate-allow-list`
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/project_allowlist.go#L42 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/project_allowlist.go#L104
- **Maturity**: GA
- **Semantics**: `generate-allow-list CLUSTERROLE_PATH PROJ_NAME` reads a ClusterRole YAML and the cluster's preferred API
  resources (discovery via kubeconfig) and emits an AppProject named PROJ_NAME whose `namespaceResourceWhitelist`
  contains a group/kind for every rule resource the role can `create` (case-insensitive verb match), resolving resource
  names to kinds; output to stdout or `-o/--out <file>`.
- **Edge cases**: Rules without apiGroups are skipped; only the **first** apiGroup of each rule is considered; wildcard
  resources (`*`) do not match any discovered name. Discovery errors abort.
- **Configuration surface**: CLI.
- **Exposure**: CLI.
- **Tests required**:
  - Positive: ClusterRole allowing create on `deployments` (apps) and `configmaps` ("") → whitelist `apps/Deployment`, `/ConfigMap`.
  - Negative: rule with only `get` → not included.

### OPS-141 `argocd admin redis-initial-password`
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/redis_initial_password.go#L38 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/commands/argocd_admin_redis-initial-password.md#L1
- **Maturity**: GA
- **Semantics**: Idempotently ensures Secret `argocd-redis` key `auth` exists (16 random chars from `[0-9A-Za-z-]`),
  printing `Checking for initial Redis password in secret <ns>/argocd-redis at key auth.`, `Argo CD Redis secret state
  confirmed: secret name argocd-redis.` and `Password secret is configured properly.`; used as the Redis init container
  (OPS-013).
- **Edge cases**: Existing Secret without `auth` → fatal `key auth doesn't exist in secret argocd-redis.`. Never
  rotates an existing password.
- **Configuration surface**: kubectl flags.
- **Exposure**: CLI.
- **Tests required**:
  - Positive: run twice → same password.
  - Negative: Secret without key → non-zero exit.

---

## K. Disaster recovery

### OPS-142 `argocd admin export`
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/backup.go#L33 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/backup.go#L455 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/admin.go#L105 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/admin.go#L173 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/disaster_recovery.md#L15
- **Maturity**: GA
- **Semantics**: Writes a multi-document YAML (separator `---`) to stdout (`-o -`, default) or `-o <file>` containing, in
  order: ConfigMaps `argocd-cm`, `argocd-rbac-cm`, `argocd-ssh-known-hosts-cm`, `argocd-tls-certs-cm`; Secrets that are
  `argocd-secret`, carry label `argocd.argoproj.io/secret-type` (repositories, repo-creds, clusters, …) or annotation
  `managed-by: argocd.argoproj.io`; all AppProjects; Applications; ApplicationSets. Objects are stripped to
  `apiVersion`, `kind`, `metadata.{name, labels, annotations, finalizers}` and their remaining body (spec/data/status);
  `metadata.namespace` is kept only for objects outside the Argo CD namespace. Applications/ApplicationSets from
  additional namespaces are included when the namespace matches `--application-namespaces` /
  `--applicationset-namespaces` globs, or by default the `application.namespaces` /
  `applicationsetcontroller.namespaces` values from `argocd-cmd-params-cm` (then all namespaces are listed and filtered).
- **Edge cases**: Missing `argocd-cmd-params-cm` → fatal when namespace flags are not given. Missing required ConfigMap
  (e.g. `argocd-rbac-cm`) → fatal NotFound. ApplicationSet list Forbidden → warning, continue. Running in the wrong
  namespace does not fail (doc note). Not exported: `argocd-cmd-params-cm`, `argocd-gpg-keys-cm`, notifications
  ConfigMap/Secret, `argocd-server-tls`, `argocd-redis`, `argocd-initial-admin-secret` (unless labelled/annotated as above).
- **Configuration surface**: CLI flags (kubectl flags incl. `-n`).
- **Exposure**: CLI.
- **Tests required**:
  - Positive: golden export of a seeded instance (ordering, stripped metadata, namespace handling).
  - Positive: apps in an extra namespace exported only when the namespace is enabled.
  - Negative: ConfigMap `argocd-tls-certs-cm` missing → non-zero exit.

### OPS-143 `argocd admin import` (incl. `--prune`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/backup.go#L158 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/backup.go#L367 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/backup.go#L429 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/backup.go#L484 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/admin.go#L124 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/disaster_recovery.md#L21
- **Maturity**: GA
- **Semantics**: `import SOURCE` (`-` = stdin) with client QPS 100 / burst 50. Prints `import process started <ns>`. Builds
  the live set (the 4 well-known ConfigMaps, Argo CD Secrets per OPS-142, Applications/ApplicationSets in enabled
  namespaces, all AppProjects). For each backup object (namespace defaulted to the Argo CD namespace): skip when it matches
  `--skip-resources-with-label key=value` (`Skipping ...`); skip Applications/ApplicationSets whose namespace is not
  enabled; create if absent (`<g>/<k> <name> in namespace <ns> created`); if present and spec/data, labels, annotations
  (ignoring `last-applied-configuration`) and finalizers are equal → unchanged (printed only with `--verbose`); otherwise
  update live: copy annotations, labels, finalizers and `data` (Secret/ConfigMap) or `spec` (+`status` for Applications
  if present in the backup) keeping other live fields (`updated`). `--ignore-tracking` keeps live tracking annotation/label
  values. `--stop-operation` clears `operation` on Applications (and forces an update when an operation exists).
  `--override-on-conflict` retries updates on conflict with the latest resourceVersion. Afterwards every live object not
  in the backup prints `needs pruning`, or with `--prune` is deleted (Applications first have finalizers removed),
  asking `Are you sure you want to prune <g>/<k> <name> ? [y/n]` when prompts are enabled
  (`--prompts-enabled`, default from local CLI config, false); live objects matching the skip label are never pruned.
  `--dry-run` prints the same lines suffixed ` (dry run)` without writing. Finishes with `Import process completed
  successfully in namespace <ns> at <RFC3339>, duration: <d>`. Forbidden/NotFound API errors are warnings.
- **Edge cases**: With `--prune`, a missing well-known ConfigMap in the backup (live one present) hits
  `Unexpected kind 'ConfigMap' in prune list` (fatal, possibly after partial pruning — map iteration order).
  `--application-namespaces`/`--applicationset-namespaces` default to the cmd-params values; if unset, only objects
  without namespace/with the control-plane namespace are imported. Conflicts without `--override-on-conflict` print
  `Failed to update ...` and then abort.
- **Configuration surface**: CLI flags above.
- **Exposure**: CLI.
- **Tests required**:
  - Positive: export → wipe → import restores all objects (round-trip golden).
  - Positive: `--dry-run` makes no changes and prints `(dry run)` lines.
  - Positive: `--prune` deletes an extra project/app (app finalizer removed first); without it prints `needs pruning`.
  - Positive: `--skip-resources-with-label` excludes both import and prune for labelled objects.
  - Negative: prune with a backup missing `argocd-rbac-cm` → fatal unexpected kind (parity) or documented fix.
  - Positive: `--stop-operation` clears in-flight operations.

### OPS-144 Backup/restore procedure and version pinning
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/disaster_recovery.md#L1 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/overview.md#L14
- **Maturity**: GA
- **Semantics**: Documented procedure: determine the server version (`argocd version`), run export/import with the CLI
  image of the **same** version (`quay.io/argoproj/argocd:$VERSION argocd admin export > backup.yaml`, `... import - <
  backup.yaml`, mounting `~/.kube`), passing `-n <namespace>` when not `argocd`. Major upgrades should be preceded by a
  backup. Redis content and the initial admin secret are not part of the backup (regenerated).
- **Edge cases**: Export silently succeeds against the wrong namespace (empty-ish backup).
- **Configuration surface**: none.
- **Exposure**: CLI/doc.
- **Tests required**:
  - Positive: DR drill — new cluster, install same version, import backup → apps reconcile to Synced without re-entering credentials.

---

## L. Server-commands reference (component binaries and flags)

### OPS-145 Shared component CLI conventions (multi-call binary, Kubernetes client flags)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/main.go#L36 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L25 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/Dockerfile#L151 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/cli/cli.go#L68 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/server-commands/additional-configuration-method.md#L1
- **Maturity**: GA
- **Semantics**: One `argocd` binary dispatches on `basename(argv[0])` or env `ARGOCD_BINARY_NAME`: `argocd-server`,
  `argocd-application-controller`, `argocd-repo-server`, `argocd-cmp-server`, `argocd-commit-server`, `argocd-dex`,
  `argocd-notifications`, `argocd-git-ask-pass`, `argocd-applicationset-controller`, `argocd-k8s-auth`; anything else
  (e.g. `argocd-linux-amd64`) runs the CLI. The image symlinks all names except `argocd-git-ask-pass` under
  `/usr/local/bin` and runs under `tini`. Every Kubernetes-facing component accepts the standard client flags
  `--kubeconfig`, `--as`, `--as-group`, `--as-uid`, `--certificate-authority`, `--client-certificate`, `--client-key`,
  `--cluster`, `--context`, `--disable-compression`, `--insecure-skip-tls-verify`, `-n/--namespace`, `--password`,
  `--proxy-url`, `--request-timeout`, `--server`, `--tls-server-name`, `--token`, `--user`, `--username`; the namespace
  defaults to the in-cluster ServiceAccount namespace (kubeconfig context namespace out of cluster) and determines where
  all settings are read (OPS-012). Every env-backed flag can be fed from `argocd-cmd-params-cm` (OPS-017). Owner of the CLI
  dispatch/plugin behaviour: 08 (API-057).
- **Edge cases**: Out-of-cluster without kubeconfig → client config error at start.
- **Configuration surface**: binary name; env `ARGOCD_BINARY_NAME`; kubectl flags.
- **Exposure**: Container command.
- **Tests required**:
  - Positive: invoking the binary via each symlink name starts the corresponding component (`--help` header).
  - Positive: `-n other` makes a component read `argocd-cm` from `other`.

### OPS-146 `argocd-application-controller` flag reference
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-application-controller/commands/argocd_application_controller.go#L258 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/server-commands/argocd-application-controller.md#L1
- **Maturity**: GA (sharding/dynamic distribution: Alpha; hydrator: Beta)
- **Semantics**: Complete flag set (defaults; owner feature): `--app-resync` 120, `--app-hard-resync` 0,
  `--app-resync-jitter` 60 (OPS-058..060); `--app-state-cache-expiration` 1h, `--default-cache-expiration` 24h, `--redis`,
  `--redisdb`, `--sentinel`, `--sentinelmaster`, `--redis-use-tls`, `--redis-ca-certificate`, `--redis-client-certificate`,
  `--redis-client-key`, `--redis-insecure-skip-tls-verify`, `--redis-compress` gzip (OPS-020/051);
  `--application-namespaces` (07); `--commit-server` `argocd-commit-server:8086`, `--hydrator-enabled`,
  `--hydration-processors` 5; `--dynamic-cluster-distribution-enabled` false (OPS-045); `--enable-k8s-event` all (OPS-091);
  `--gloglevel` 0, `--logformat` json, `--loglevel` info (OPS-086/088); `--ignore-normalizer-jq-execution-timeout-seconds`
  0; `--kubectl-parallelism-limit` 20 (OPS-039); `--metrics-application-conditions`, `--metrics-application-labels`,
  `--metrics-cluster-labels`, `--metrics-cache-expiration` 0, `--metrics-port` 8082 (OPS-065..075);
  `--operation-processors` 10, `--status-processors` 20 (OPS-038); `--otlp-address/-attrs/-headers/-insecure`
  (OPS-085); `--persist-resource-health` false (04); `--repo-error-grace-period-seconds` 180;
  `--repo-server` `argocd-repo-server:8081`, `--repo-server-timeout-seconds` 60, `--repo-server-plaintext`,
  `--repo-server-ca-cert-path`, `--repo-server-client-cert-path`, `--repo-server-client-cert-key-path` (OPS-101/103),
  hidden deprecated `--repo-server-strict-tls`; `--self-heal-timeout-seconds` 0, `--self-heal-backoff-timeout-seconds` 2,
  `--self-heal-backoff-factor` 3, `--self-heal-backoff-cap-seconds` 300, hidden deprecated
  `--self-heal-backoff-cooldown-seconds` 330 (05); `--server-side-diff-enabled` false (04); `--sharding-method` legacy
  (OPS-041..043); `--sync-timeout` 0 (05); `--wq-backoff-factor` 1.5, `--wq-basedelay-ns` 1ms, `--wq-bucket-qps` MaxFloat64,
  `--wq-bucket-size` 500, `--wq-cooldown-ns` 0, `--wq-maxdelay-ns` 1s (OPS-046); plus OPS-145 client flags. No
  `--metrics-address` (binds 0.0.0.0).
- **Edge cases**: Deprecated flags print a deprecation notice when used and are hidden from help.
- **Configuration surface**: flags/env.
- **Exposure**: Container args.
- **Tests required**:
  - Positive: `--help` flag-set parity test (names, types, defaults) against this list.

### OPS-147 `argocd-server` flag reference (incl. `argocd-server version`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-server/commands/argocd_server.go#L306 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-server/commands/argocd_server.go#L319 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/server-commands/argocd-server.md#L1 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/server-commands/argocd-server_version.md#L1
- **Maturity**: GA (proxy extension: Beta; hydrator: Beta)
- **Semantics**: Flags: `--address` 0.0.0.0, `--port` 8080, `--metrics-address`(see OPS-022 quirk), `--metrics-port` 8083;
  `--insecure`, `--tlsminversion` 1.2, `--tlsmaxversion` 1.3, `--tlsciphers` (OPS-097/098); `--rootpath`, `--basehref` /,
  `--staticassets` /shared/app, `--x-frame-options` sameorigin, `--content-security-policy`, `--api-content-types`
  application/json, `--enable-gzip` true (OPS-105..111); `--disable-auth` (07); `--dex-server`, `--dex-server-plaintext`,
  `--dex-server-strict-tls` (OPS-102); `--repo-server`, `--repo-server-timeout-seconds` 60, `--repo-server-plaintext`,
  `--repo-server-ca-cert-path`, `--repo-server-client-cert-path`, `--repo-server-client-cert-key-path`, hidden
  `--repo-server-strict-tls` (OPS-101/103); caches `--app-state-cache-expiration`, `--connection-status-cache-expiration`
  1h, `--oidc-cache-expiration` 3m, `--login-attempts-expiration` 24h (deprecated/unused), `--default-cache-expiration`,
  `--repo-cache-expiration`, `--revision-cache-expiration`, `--revision-cache-lock-timeout`, Redis flags and the
  `--repo-server-`-prefixed Redis flag copies (OPS-051); `--application-namespaces`, `--enable-k8s-event`,
  `--enable-proxy-extension`, `--glob-cache-size` 10000, `--hydrator-enabled`, `--sync-with-replace-allowed` true,
  `--webhook-parallelism-limit` 50, `--webhook-refresh-workers` 20; ApplicationSet API helpers `--appset-scm-root-ca-path`,
  `--appset-enable-scm-providers` true, `--appset-allowed-scm-providers`, `--appset-enable-new-git-file-globbing`,
  `--appset-enable-github-api-metrics`; `--gloglevel`, `--logformat`, `--loglevel`; `--otlp-*`; OPS-145 client flags.
  Subcommand `argocd-server version [--short]` prints build version info.
- **Edge cases**: `--login-attempts-expiration` is accepted but unused.
- **Configuration surface**: flags/env.
- **Exposure**: Container args.
- **Tests required**:
  - Positive: help flag-set parity test; `argocd-server version --short` prints the version string only.

### OPS-148 `argocd-repo-server` flag reference
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-repo-server/commands/argocd_repo_server.go#L255 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/server-commands/argocd-repo-server.md#L15
- **Maturity**: GA
- **Semantics**: `--address` 0.0.0.0, `--port` 8081, `--metrics-address` 0.0.0.0, `--metrics-port` 8084; `--logformat`,
  `--loglevel`; `--parallelismlimit` 0 (OPS-035); `--disable-tls`, `--client-ca-path`, `--tlsminversion`,
  `--tlsmaxversion`, `--tlsciphers` (OPS-097/099/103); `--otlp-*`; size limits `--max-combined-directory-manifests-size` 10M,
  `--streamed-manifest-max-tar-size` 100M, `--streamed-manifest-max-extracted-size` 1G,
  `--helm-manifest-max-extracted-size` 1G, `--disable-helm-manifest-max-extracted-size`, `--helm-registry-max-index-size`
  1G (env `ARGOCD_REPO_SERVER_HELM_MANIFEST_MAX_INDEX_SIZE`, no cmd-params key), `--oci-manifest-max-extracted-size` 1G,
  `--disable-oci-manifest-max-extracted-size`, `--oci-layer-media-types` (3 defaults); `--plugin-tar-exclude`,
  `--plugin-use-manifest-generate-paths`, `--allow-oob-symlinks`, `--include-hidden-directories`,
  `--enable-builtin-git-config` true (owner 02/03); caches `--repo-cache-expiration` 24h, `--revision-cache-expiration` 3m,
  `--revision-cache-lock-timeout` 10s, `--default-cache-expiration` 24h and Redis flags (OPS-036/051). No kubectl client
  flags (repo-server has no Kubernetes access).
- **Edge cases**: Quantity parse errors are fatal at start.
- **Configuration surface**: flags/env.
- **Exposure**: Container args.
- **Tests required**:
  - Positive: help flag-set parity test.

### OPS-149 `argocd-applicationset-controller` flag reference
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-applicationset-controller/commands/applicationset_controller.go#L294 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/server-commands/argocd-applicationset-controller.md#L1
- **Maturity**: GA (progressive syncs: Beta)
- **Semantics**: `--metrics-addr` :8080, `--probe-addr` :8081, `--webhook-addr` :7000, `--enable-leader-election`
  (OPS-055), `--applicationset-namespaces`, `--argocd-repo-server` (`ARGOCD_APPLICATIONSET_CONTROLLER_REPO_SERVER`),
  `--repo-server-plaintext`, `--repo-server-timeout-seconds` 60, `--repo-server-ca-cert-path`,
  `--repo-server-client-cert-path`, `--repo-server-client-cert-key-path`, hidden `--repo-server-strict-tls`, `--policy`,
  `--enable-policy-override`, `--debug`, `--logformat`, `--loglevel`, `--allowed-scm-providers`, `--enable-scm-providers`,
  `--dry-run`, `--token-ref-strict-mode`, `--enable-progressive-syncs`, `--enable-new-git-file-globbing`,
  `--concurrent-reconciliations` 10, `--concurrent-application-updates` 1, `--scm-root-ca-path`, `--scm-proxy-url`,
  `--scm-no-proxy`, `--preserved-annotations`, `--preserved-labels`, `--webhook-parallelism-limit` 50,
  `--metrics-applicationset-labels`, `--enable-github-api-metrics`, `--max-resources-status-count` 5000,
  `--cache-sync-period` 10h; plus OPS-145 client flags. Semantics owned by 06 (keys in OPS-026).
- **Edge cases**: Note the repo-server flag name differs from other components (`--argocd-repo-server`).
- **Configuration surface**: flags/env.
- **Exposure**: Container args.
- **Tests required**:
  - Positive: help flag-set parity test.

### OPS-150 `argocd-notifications` flag reference
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-notification/commands/argocd_notification.go#L37 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-notification/commands/argocd_notification.go#L178
- **Maturity**: GA
- **Semantics**: `--processors-count` 1 (`ARGOCD_NOTIFICATION_CONTROLLER_PROCESSORS_COUNT`), `--app-label-selector` (no env),
  `--loglevel`, `--logformat`, `--metrics-port` 9001 (no env), `--argocd-repo-server` `argocd-repo-server:8081` (no env),
  `--argocd-repo-server-plaintext`, deprecated `--argocd-repo-server-strict-tls` (no env), `--repo-server-ca-cert-path`,
  `--repo-server-client-cert-path`, `--repo-server-client-cert-key-path` (env prefix `ARGOCD_NOTIFICATION_CONTROLLER_`),
  `--config-map-name` argocd-notifications-cm, `--secret-name` argocd-notifications-secret, `--application-namespaces`,
  `--self-service-notification-enabled`; plus OPS-145 client flags. Repo-server RPC timeout fixed at 5s. Semantics owned by 09.
- **Edge cases**: No generated server-commands doc page exists for this binary (doc gap). Metrics bind to all interfaces.
- **Configuration surface**: flags/env.
- **Exposure**: Container args.
- **Tests required**:
  - Positive: help flag-set parity test; `--config-map-name custom-cm` reads triggers from that ConfigMap.

### OPS-151 `argocd-commit-server` flag reference
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-commit-server/commands/argocd_commit_server.go#L110 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L80
- **Maturity**: Beta (Source Hydrator)
- **Semantics**: `--logformat`, `--loglevel`, `--address` 0.0.0.0 (`ARGOCD_COMMIT_SERVER_LISTEN_ADDRESS`), `--port` 8086,
  `--metrics-address` 0.0.0.0 (`ARGOCD_COMMIT_SERVER_METRICS_LISTEN_ADDRESS`), `--metrics-port` 8087. No TLS, Redis,
  OTLP or kubectl flags. Hydration semantics owned by the source-hydrator area.
- **Edge cases**: No generated server-commands doc page exists for this binary.
- **Configuration surface**: flags/env (OPS-024).
- **Exposure**: Container args.
- **Tests required**:
  - Positive: help flag-set parity test; custom `--port` reachable by the controller via `commit.server`.

### OPS-152 `argocd-dex` wrapper (`rundex`, `gendexcfg`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-dex/commands/argocd_dex.go#L30 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-dex/commands/argocd_dex.go#L50 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-dex/commands/argocd_dex.go#L157 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/server-commands/argocd-dex_rundex.md#L1
- **Maturity**: GA
- **Semantics**: `argocd-dex rundex` (flags `--logformat`, `--loglevel`, `--disable-tls`, client flags): runs inside the
  Dex image (`ghcr.io/dexidp/dex`): init container `copyutil` copies the Argo CD binary to `/shared/argocd-dex` and the
  Dex container runs `/shared/argocd-dex rundex`, which requires the `dex` binary on PATH, prepares TLS (OPS-100),
  generates Dex config from argocd-cm/argocd-secret (owner 07), writes `/tmp/dex.yaml` and runs `dex serve /tmp/dex.yaml`;
  on settings changes that alter the generated config it SIGTERMs Dex and restarts it (`dex config modified. restarting
  dex`); if no Dex config is generated it logs `dex is not configured` and idles. `argocd-dex gendexcfg [-o file]` prints
  the generated config with `staticClients[].secret` masked as `******` (or writes it unmasked to `-o`).
- **Edge cases**: `dex` not found → fatal at start. Generated config is logged at debug level through a redactor.
- **Configuration surface**: flags; argocd-cm `dex.config`.
- **Exposure**: Container args.
- **Tests required**:
  - Positive: editing `dex.config` restarts the Dex child process without pod restart.
  - Positive: `gendexcfg` masks client secrets on stdout.

### OPS-153 Auxiliary binaries register (`argocd-cmp-server`, `argocd-git-ask-pass`, `argocd-k8s-auth`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/main.go#L56 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/main.go#L65 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/main.go#L70
- **Maturity**: GA
- **Semantics**: Operationally relevant entry points owned by other areas: `argocd-cmp-server` (config-management plugin
  sidecar server, supports OTLP and gRPC metrics; owner 02), `argocd-git-ask-pass` (Git credential helper talking to the
  repo-server's local ask-pass socket; owner 03), `argocd-k8s-auth` (exec credential plugin for GCP/AWS/Azure cluster auth;
  owner 03). Patrol must ship equivalents under the same names for drop-in images/manifests.
- **Edge cases**: These run in CLI mode (errors silenced, printed by the dispatcher).
- **Configuration surface**: binary names.
- **Exposure**: Container/exec plugin.
- **Tests required**:
  - Positive: each binary name resolves to its function in the Patrol image.

---

## M. Upgrades and baseline defaults

### OPS-154 Upgrade procedure and versioning policy
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/overview.md#L7 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/overview.md#L20 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/installation.md#L118 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/developer-guide/release-process-and-cadence.md#L47
- **Maturity**: GA
- **Semantics**: Semver-like policy: patch releases have no breaking changes; minor releases may contain minor breaking
  changes with workarounds (read every intermediate upgrade note); major releases may break behaviour (take a backup,
  OPS-144). Upgrade = re-apply the whole manifest set of the target version with
  `kubectl apply -n argocd --server-side --force-conflicts -f .../<version>/manifests/install.yaml` (or `ha/install.yaml`),
  not only image changes. Support policy (release process document): only the three most recent minor versions are
  eligible for patch releases; CVE fixes go to all supported versions.
- **Edge cases**: Client-side apply fails since 3.3 (OPS-006).
- **Configuration surface**: none.
- **Exposure**: Install artifact.
- **Tests required**:
  - Positive: upgrade from previous Patrol minor by re-applying manifests keeps all Applications Synced.

### OPS-155 Shipped `argocd-cm` defaults (default resource exclusions and ignoreResourceUpdates, since 3.0)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/config/argocd-cm.yaml#L9 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/config/argocd-cm.yaml#L57 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/2.14-3.0.md#L67 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/2.14-3.0.md#L330
- **Maturity**: GA
- **Semantics**: The installed `argocd-cm` (labels `app.kubernetes.io/name: argocd-cm`, `part-of: argocd`) is not empty:
  `resource.customizations.ignoreResourceUpdates.all` = jsonPointers `/status`;
  `...argoproj.io_Application` = jq `.metadata.annotations."notified.notifications.argoproj.io"`,
  `."argocd.argoproj.io/refresh"`, `."argocd.argoproj.io/hydrate"`, `.operation`;
  `...argoproj.io_Rollout` = `notified.notifications.argoproj.io` annotation;
  `...autoscaling_HorizontalPodAutoscaler` = annotations `autoscaling.alpha.kubernetes.io/{behavior,conditions,metrics,current-metrics}`;
  `...ConfigMap` = annotations `cluster-autoscaler.kubernetes.io/last-updated`, `control-plane.alpha.kubernetes.io/leader`;
  `...apps_ReplicaSet` = annotations `deployment.kubernetes.io/desired-replicas`, `deployment.kubernetes.io/max-replicas`,
  `rollout.argoproj.io/desired-replicas`; `...discovery.k8s.io_EndpointSlice` = `/metadata`, `/endpoints`, `/ports`;
  `...Endpoints` = `/metadata`, `/subsets`. `resource.exclusions`: core/discovery `Endpoints`, `EndpointSlice`;
  `coordination.k8s.io Lease`; authentication/authorization `SelfSubjectReview`, `TokenReview`,
  `LocalSubjectAccessReview`, `SelfSubjectAccessReview`, `SelfSubjectRulesReview`, `SubjectAccessReview`;
  `certificates.k8s.io CertificateSigningRequest`; `cert-manager.io CertificateRequest`; `cilium.io CiliumIdentity`,
  `CiliumEndpoint`, `CiliumEndpointSlice`; `kyverno.io`/`reports.kyverno.io`/`wgpolicyk8s.io` `PolicyReport`,
  `ClusterPolicyReport`, `EphemeralReport`, `ClusterEphemeralReport`, `AdmissionReport`, `ClusterAdmissionReport`,
  `BackgroundScanReport`, `ClusterBackgroundScanReport`, `UpdateRequest`. Semantics of exclusions: owner 04.
- **Edge cases**: Operators overriding `argocd-cm` without these keys revert to 2.x behaviour (more churn/watches).
- **Configuration surface**: argocd-cm.
- **Exposure**: Install artifact.
- **Tests required**:
  - Positive: golden comparison of the shipped argocd-cm data keys/values.
  - Positive: with defaults, Endpoints/Lease objects are not watched/cached; Pod status churn of tracked resources does not refresh apps.

### OPS-156 3.0 baseline behaviour changes (register)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/2.14-3.0.md#L19 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/2.14-3.0.md#L84 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/2.14-3.0.md#L241 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/2.14-3.0.md#L338 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/2.14-3.0.md#L432
- **Maturity**: GA
- **Semantics**: Defaults Patrol must reproduce (owner in parentheses): fine-grained `update`/`delete` RBAC no longer
  inherited by sub-resources; `server.rbac.disableApplicationFineGrainedRBACInheritance: false` restores v2 (07); `logs`
  RBAC always enforced, `server.rbac.log.enforce.enable` removed (07); default `resource.exclusions` (OPS-155, 04);
  legacy metrics `argocd_app_sync_status`, `argocd_app_health_status`, `argocd_app_created_time` and
  `ARGOCD_LEGACY_CONTROLLER_METRICS` removed — use `argocd_app_info` labels (OPS-066); Dex RBAC subject =
  `federated_claims.user_id` via `federated:id` scope instead of `sub` (07); repositories in argocd-cm (`repositories`,
  `repository.credentials`, `helm.repositories`) no longer supported (03); ApplicationSet `applyNestedSelectors` ignored,
  nested selectors always applied (06); Helm 3.17.1 null-value semantics (02); default tracking method `annotation`
  (`application.resourceTrackingMethod: label` opts out) (04); `cluster.inClusterEnabled: "false"` makes in-cluster apps
  Unknown (03); default ignoreResourceUpdates incl. `/status` (OPS-155); resource health stored outside the Application CR
  (`status.resourceHealthSource: appTree`; `controller.resource.health.persist: true` restores) (04); empty plugin env
  vars passed (`ARGOCD_ENV_X=""`) (02); system `ignoreDifferences` also used for ignoring resource updates (OPS-063);
  `ignoreResourceStatusField` default `all` (04); no default ignore of CRD `spec.preserveUnknownFields` (04); project API
  responses sanitized (07).
- **Edge cases**: Label→annotation tracking switch can orphan resources deleted in the first sync after upgrade (doc warning).
- **Configuration surface**: see owners.
- **Exposure**: Various.
- **Tests required**:
  - Positive: fresh install defaults match each listed 3.0 behaviour (one assertion per bullet, delegated to owner areas).
  - Negative: legacy metric names absent from `/metrics`.

### OPS-157 3.1 behaviour changes (register)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/3.0-3.1.md#L3 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/3.0-3.1.md#L9 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/3.0-3.1.md#L31
- **Maturity**: GA (v1 actions API: Deprecated)
- **Semantics**: `--staticassets` symlink confinement (OPS-111); `/api/v1/applications/{name}/resource/actions` deprecated
  in favour of `.../resource/actions/v2` with JSON body and `resourceActionParameters`; server logs `RunResourceAction was
  called. RunResourceAction is deprecated ...`; CLI falls back with `RunResourceActionV2 is not supported by the server,
  falling back to RunResourceAction.` (08); OIDC authorization-code flow with PKCE handled by the server (IdP must allow
  `/auth/callback`) (07); Helm 3.18.4, Kustomize 5.7.0 at that time (superseded, OPS-162); project API sanitization.
- **Edge cases**: none beyond owners.
- **Configuration surface**: see owners.
- **Exposure**: API/CLI/UI.
- **Tests required**:
  - Positive: calling the v1 actions endpoint works and logs the deprecation warning.

### OPS-158 3.2 behaviour changes (register)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/3.1-3.2.md#L7 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/3.1-3.2.md#L41 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/3.1-3.2.md#L64 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/3.1-3.2.md#L94
- **Maturity**: GA (hydrator items: Beta)
- **Semantics**: Source-hydrator sync paths must be non-root (hydrator area); Kustomize version honoured from
  `.argocd-source.yaml` (02); repo-server gRPC `kustomizeOptions.binaryPath` deprecated in favour of
  `kustomizeOptions.versions` (warning `kustomizeOptions.binaryPath is deprecated, use KustomizeOptions.versions instead`)
  (02); built-in CronJob health check (may turn apps Degraded; suspended CronJobs stay Healthy) (04); ApplicationSet
  `status.resources` capped at 5000 (`applicationsetcontroller.status.max.resources.count`) (06).
- **Edge cases**: Large monorepo lock contention note (fixed in a later 3.2 patch; not a baseline behaviour).
- **Configuration surface**: see owners.
- **Exposure**: Various.
- **Tests required**:
  - Positive: appset generating 6000 apps → status lists 5000 resources.

### OPS-159 3.3 behaviour changes (register)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/3.2-3.3.md#L5 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/3.2-3.3.md#L37 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/3.2-3.3.md#L77 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/3.2-3.3.md#L82 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/3.2-3.3.md#L89
- **Maturity**: GA (hydrator items: Beta; cooldown flag: Deprecated)
- **Semantics**: Server-side apply required for install/upgrade (OPS-006, OPS-125); source hydrator records state in git
  notes and only commits when manifests change, and no longer cleans the app path (hydrator area); anonymous Settings API
  omits `resourceOverrides` (08); new `ARGOCD_K8S_SERVER_SIDE_TIMEOUT` separated from `ARGOCD_K8S_TCP_TIMEOUT` (OPS-047);
  `--self-heal-backoff-cooldown-seconds` deprecated no-op (05); Helm 3.19.2 at that time and Helm 2 unsupported (no
  `helm version --client`) (02); Kustomize 5.8.0 (02).
- **Edge cases**: Users of 3.3.0/3.3.1 with `ClientSideApplyMigration=false` should remove it.
- **Configuration surface**: see owners.
- **Exposure**: Various.
- **Tests required**:
  - Positive: anonymous `GET /api/v1/settings` has no `resourceOverrides`.

### OPS-160 3.4 behaviour changes (register)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/3.3-3.4.md#L5 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/3.3-3.4.md#L29 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/3.3-3.4.md#L47 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/3.3-3.4.md#L118 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/3.3-3.4.md#L127
- **Maturity**: GA
- **Semantics**: Cluster version stored/returned as `vMajor.Minor.Patch` (affects `argocd.argoproj.io/kubernetes-version`
  auto-labels and cluster generators; CMP `$KUBE_VERSION` unchanged `Major.Minor.Patch`) (03/06); Application health
  `Missing` only when all resources are missing (04); gRPC DNS TXT service-config lookups disabled by default
  (`GRPC_ENABLE_TXT_SERVICE_CONFIG=false`, re-enable per component, OPS-021); Dex 2.45 with
  `ContinueOnConnectorFailure` enabled by default (`dexserver.connector.failure.continue: "false"` disables) (07);
  go-oidc verifies signature before claims (expired token signed by a rotated-out key → login page instead of SSO
  redirect) (07); OpenTelemetry SDK 1.42 / semconv 1.40 (renamed OTel metrics/attributes, OPS-085).
- **Edge cases**: Dashboards relying on old OTel names must be updated.
- **Configuration surface**: see owners.
- **Exposure**: Various.
- **Tests required**:
  - Positive: `argocd cluster get` shows `v1.x.y` server version format.
  - Positive: component pods have `GRPC_ENABLE_TXT_SERVICE_CONFIG=false` unless overridden.

### OPS-161 3.5 behaviour changes (register)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/3.4-3.5.md#L5 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/3.4-3.5.md#L46 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/3.4-3.5.md#L60 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/3.4-3.5.md#L108 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/3.4-3.5.md#L131 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/3.4-3.5.md#L141 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/3.4-3.5.md#L180
- **Maturity**: GA (GnuPG configuration: Deprecated; `--repo-server-strict-tls`: Deprecated)
- **Semantics**: Helm 4 (4.2.x): plain-HTTP OCI registries require `insecureOCIForceHttp` on the repo (and on dependency
  repos); `--insecure-skip-server-verification` together with `--insecure-oci-force-http` silently drops `--plain-http`;
  `spec.source.helm.version` ignored (02/03); UI extensions must externalize `react/jsx-runtime` (React 19) (08);
  `ListResourceEvents`/`ListEvents` gRPC methods return Argo CD `EventList` type (REST JSON unchanged; OpenAPI schema
  `eventsEventList`) (08); impersonation applies to all API server operations (get/patch/delete resource, events, logs,
  actions) (07); SSH repos without credentials use `argocd-ssh-known-hosts-cm` for host-key verification (03); GnuPG
  verification replaced by Source Integrity (`sourceIntegrity` in AppProject; legacy config warns) and
  `verifyResult` deprecated in favour of `sourceIntegrityResult` (02/07); release `sbom.tar.gz` contains
  `bom-ui-pnpm.spdx.json` (OPS-167); repo-server mTLS (OPS-103); `--repo-server-strict-tls` deprecated (OPS-101).
- **Edge cases**: gRPC clients of the event APIs must be regenerated.
- **Configuration surface**: see owners.
- **Exposure**: Various.
- **Tests required**:
  - Positive: REST `GET /api/v1/applications/{name}/events` JSON shape unchanged.
  - Positive: deprecated strict-TLS flag still works with a warning.

### OPS-162 Bundled tool and image versions
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/hack/tool-versions.sh#L14 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/Dockerfile#L1 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/Dockerfile#L41 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/kustomization.yaml#L7 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/dex/argocd-dex-server-deployment.yaml#L40 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/ha/base/redis-ha/chart/values.yaml#L17
- **Maturity**: GA
- **Semantics**: v3.5.5 image `quay.io/argoproj/argocd:v3.5.5` built on Ubuntu 26.04, user `argocd` uid/gid 999,
  entrypoint `tini --`, packages incl. `git`, `gpg`, `openssh-client`, `connect-proxy`; bundled tools Helm 4.2.1,
  Kustomize 5.8.1, git-lfs 3.7.1 (oras 1.2.0 for tooling); third-party images Dex `ghcr.io/dexidp/dex:v2.45.1`, Redis
  `8.2.3-alpine`, HAProxy `3.0.8-alpine` (HA). Tool versions define rendering behaviour (owner 02).
- **Edge cases**: Operators overriding tools via OPS-014 change behaviour.
- **Configuration surface**: images.
- **Exposure**: Install artifact.
- **Tests required**:
  - Positive: `helm version`/`kustomize version` inside the Patrol repo-server report compatible versions (or documented deviations).

---

## N. Security considerations

### OPS-163 Component privilege separation and repository trust model
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/security.md#L43 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/security.md#L63 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/security.md#L81 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/security.md#L143 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/security.md#L204 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/repo-server/argocd-repo-server-deployment.yaml#L1
- **Maturity**: GA
- **Semantics**: Security properties Patrol must preserve: the repo-server (which executes Helm/Kustomize/plugins on
  untrusted repo content) has **no Kubernetes privileges** (SA without Role, OPS-008), stores no credentials at rest
  (credentials are passed per request), runs non-root with a read-only root filesystem and is reachable only from
  Argo CD components (NetworkPolicy, OPS-010); built-in tools can be disabled (02); remote bases/chart dependencies can
  pull from repositories outside the allow-list (documented risk); cluster write privileges can be narrowed by editing
  `argocd-server`/`argocd-application-controller` ClusterRoles while read access stays cluster-wide (OPS-007); webhook
  payloads are untrusted and only trigger the same refresh that happens periodically (owner 03).
- **Edge cases**: CMP sidecars with secrets (e.g. decryption keys) weaken the isolation (doc warning).
- **Configuration surface**: manifests.
- **Exposure**: Install artifact.
- **Tests required**:
  - Authorization: repo-server token cannot read Secrets or list pods.
  - Negative: Helm chart attempting to read files outside the repo (`../../`) fails / is confined (owner 02 asserts details).
  - Positive: repo-server container has `readOnlyRootFilesystem: true` and runs as non-root.

### OPS-164 Sensitive data handling (redaction in API and logs)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/security.md#L90 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L937
- **Maturity**: GA
- **Semantics**: Cluster credentials, Git credentials, OAuth2 client secrets and Kubernetes Secret values are never
  returned by the API and are redacted in API payload logs (sensitive methods excluded, OPS-090); Secret manifests in
  diffs/trees are masked (owner 04/08). Admin password stored as bcrypt in `argocd-secret` (OPS-030).
- **Edge cases**: SSO debug logging (OPS-089) can reveal tokens.
- **Configuration surface**: argocd-cm `resource.sensitive.mask.annotations` (owner 04).
- **Exposure**: API, logs.
- **Tests required**:
  - Negative: `GET /api/v1/repositories` and `/clusters` never contain passwords/tokens/private keys.
  - Negative: managed Secret data shown masked (`++++++++`, distinct values get distinct `+` runs) in diff/live manifest APIs.

### OPS-165 Directory-app memory protection guidance
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/security.md#L252
- **Maturity**: GA
- **Semantics**: Operators should bound repo-server memory for raw YAML/JSON (directory) apps with
  `reposerver.max.combined.directory.manifests.size` (default `10M`, per app), sizing it as memory limit / (300 ×
  number of directory apps) because in-memory size can be ~300× the on-disk size for crafted manifests. Enforcement
  semantics owned by 02.
- **Edge cases**: Limit is per application; concurrent generations multiply memory use.
- **Configuration surface**: cmd-params key.
- **Exposure**: ConfigMap.
- **Tests required**:
  - Negative: directory app whose files exceed the limit → manifest generation error naming the limit.

### OPS-166 Managed-cluster credential rotation and revocation procedure
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/security.md#L102
- **Maturity**: GA
- **Semantics**: Cluster Secrets hold the `argocd-manager` ServiceAccount bearer token (or exec/IAM config). Rotation:
  delete the token Secret on the managed cluster and re-run `argocd cluster add`; revocation: delete the `argocd-manager`
  SA, ClusterRole `argocd-manager-role` and binding, then `argocd cluster rm`. EKS IAM auth needs no rotation. Owner of
  cluster registration: 03.
- **Edge cases**: Non-expiring SA token Secrets are created for ≥1.24 clusters by `cluster add`.
- **Configuration surface**: managed cluster RBAC.
- **Exposure**: CLI.
- **Tests required**:
  - Positive: after token rotation and re-add, connection status returns to Successful.

### OPS-167 Signed release assets and provenance verification
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/signed-release-assets.md#L9 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/signed-release-assets.md#L27 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/signed-release-assets.md#L66 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/signed-release-assets.md#L109 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/signed-release-assets.md#L139 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/upgrading/3.4-3.5.md#L180
- **Maturity**: GA
- **Semantics**: Container images are keyless-signed with cosign (verifiable with `cosign verify` against the GitHub OIDC
  issuer/workflow identity) and carry SLSA provenance (verify with `slsa-verifier verify-image`); CLI binaries have a
  single provenance attestation `argocd-cli.intoto.jsonl`; the SBOM `sbom.tar.gz` (Go modules SPDX, image SPDX, UI pnpm
  SPDX JSON since 3.5) has `argocd-sbom.intoto.jsonl` provenance plus `.pem`/`.sig`; admission policy controllers can
  enforce these.
- **Edge cases**: Release-engineering feature (no runtime code).
- **Configuration surface**: release pipeline.
- **Exposure**: Release artifacts.
- **Tests required**:
  - Positive: Patrol release images verify with cosign keyless and slsa-verifier; SBOM attestation verifies.

## O. Supplementary observability (workqueue metrics, debug trace logging)

### OPS-168 Application-controller workqueue metrics (`workqueue_*` families on `:8082/metrics`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/metrics/metrics.go#L190 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/metrics/metrics.go#L193 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L200 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L201 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L202 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L203 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L204 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L205 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L998 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1005 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1018 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1022 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1052 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1067 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1885 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2764 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2803 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L2816 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L255 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/hydrator_dependencies.go#L112 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/ratelimiter/ratelimiter.go#L39 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/go.mod#L121 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/go.mod#L130 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/metrics/metrics_test.go#L667 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/metrics/metrics_test.go#L675 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/examples/dashboard.json#L1344 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/proposals/004-scalability-benchmarking.md#L79
- **Maturity**: GA
- **Semantics**: The controller `/metrics` handler gathers two registries: the app registry (OPS-066..079) and the
  controller-runtime registry (`ctrlmetrics.Registry`, commented upstream as "contains workqueue metrics, process and
  golang metrics"). Controller-runtime v0.24.1 (`go.mod`) registers seven workqueue families in that registry and
  installs itself as client-go's (v0.36.1) global workqueue metrics provider at process init, so every **named**
  client-go queue the controller builds is instrumented. Families (label `name` and label `controller` both carry the
  queue name):
  - `workqueue_depth` gauge `{name, controller, priority}` — keys waiting to be handed to a worker; `priority` is always
    the empty string for these non-priority queues (upstream test expects `priority=""`).
  - `workqueue_adds_total` counter `{name, controller}` — adds that were not already pending (de-duplicated).
  - `workqueue_queue_duration_seconds` histogram — time from add to `Get` by a worker; buckets
    `ExponentialBuckets(1e-8, 10, 12)` (10 ns … 1000 s); native-histogram factor 1.1, max 100 buckets, reset 1 h.
  - `workqueue_work_duration_seconds` histogram — time from `Get` to `Done`; same buckets.
  - `workqueue_unfinished_work_seconds` gauge — sum of ages of in-flight items; `workqueue_longest_running_processor_seconds`
    gauge — age of the oldest in-flight item; both refreshed every 500 ms.
  - `workqueue_retries_total` counter — incremented on every `AddAfter`/`AddRateLimited` call (including zero delay).

  Named queues (all built with the custom app-controller rate limiter, OPS-046):
  `app_reconciliation_queue` (app refresh/compare; drained by `--status-processors` workers, OPS-038),
  `app_operation_processing_queue` (operations; `--operation-processors` workers; every processed refresh re-enqueues
  the app here with `AddRateLimited`), `project_reconciliation_queue` (one worker), `app_hydration_queue` and
  `manifest_hydration_queue` (source hydrator; workers started only when the hydrator is enabled). Metric children are
  created when each queue is constructed, so all five queues export zero-valued series from startup even with the
  hydrator disabled. Most enqueues (informer add/update handlers, `requestAppRefresh` without an explicit delay,
  post-refresh operation enqueue, hydration dependency enqueue) use `AddRateLimited`, so `workqueue_retries_total`
  counts rate-limited enqueues, not failures; plain `Add` calls (Application/AppProject delete events) are not counted.
  With sharding (OPS-040+), each replica exports the queues of its own process only. The published Grafana example
  graphs `sum(workqueue_depth{name=~"app_.*"}) by (name)` ("Workqueue Depth").
- **Edge cases**: The comparison-type refresh queue (`appComparisonTypeRefreshQueue`) is unnamed and exports no
  metrics. Rate-limiter delays happen before an item enters the queue, so they do not show in
  `workqueue_queue_duration_seconds` (they do increment `workqueue_retries_total`). Re-adding a key while it is being
  processed increments `adds_total`/`depth` once; re-adding a key that is still pending is a no-op for the metrics. A stuck
  processor shows as a steadily growing `workqueue_longest_running_processor_seconds`. The provider is first-wins
  (`SetProvider` uses `sync.Once`) and is set by controller-runtime package init in the multi-call `argocd` binary. The
  API server, repo-server, notifications controller and commit-server endpoints do not gather the controller-runtime
  registry and export no `workqueue_*` families (the API server webhook refresh queue is unnamed anyway).
- **Configuration surface**: none dedicated; values are driven by `--status-processors`, `--operation-processors`,
  `--hydration-processors` (OPS-038), the workqueue rate-limit flags/env (OPS-046) and hydrator enablement.
- **Exposure**: Controller metrics endpoint `:8082/metrics` (Service `argocd-metrics`); Grafana example dashboard.
- **Tests required**:
  - Positive: a fresh controller exports `workqueue_depth`, `workqueue_adds_total`, `workqueue_queue_duration_seconds`, `workqueue_work_duration_seconds`, `workqueue_unfinished_work_seconds`, `workqueue_longest_running_processor_seconds`, `workqueue_retries_total` for each of the five queue names with `name` = `controller` = queue name and `priority=""` on depth.
  - Positive: creating an Application increments `workqueue_adds_total{name="app_reconciliation_queue"}` and `workqueue_retries_total{name="app_reconciliation_queue"}`; after it is processed, `app_operation_processing_queue` adds also increase.
  - Positive: hydrator disabled → `app_hydration_queue`/`manifest_hydration_queue` series exist with value 0.
  - Negative: no `workqueue_*` series for an unnamed queue; deleting an Application (plain `Add`) does not increment `retries_total`.
  - Recovery: a reconcile blocked on a slow cluster raises `workqueue_longest_running_processor_seconds{name="app_reconciliation_queue"}`, which falls back to 0 once the item completes.
  - Positive: with two shards each replica reports only its own queue activity.

### OPS-169 ApplicationSet controller workqueue and controller-runtime reconcile metrics (`applicationset` queue)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/applicationset_controller.go#L621 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/applicationset_controller.go#L623 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-applicationset-controller/commands/applicationset_controller.go#L164 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-applicationset-controller/commands/applicationset_controller.go#L166 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-applicationset-controller/commands/applicationset_controller.go#L172 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-applicationset-controller/commands/applicationset_controller.go#L294 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-applicationset-controller/commands/applicationset_controller.go#L317 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/go.mod#L130
- **Maturity**: GA
- **Semantics**: The ApplicationSet controller runs a controller-runtime manager whose metrics server serves the
  controller-runtime registry on `--metrics-addr` (default `:8080`). Its single controller is built with
  `For(&ApplicationSet{})` and no explicit name, so the controller and its queue are named `applicationset` (lowercased
  kind). Argo CD sets no controller options other than `MaxConcurrentReconciles`, so controller-runtime v0.24.1's
  default priority queue is used: the same seven `workqueue_*` families as OPS-168 with `name="applicationset"`,
  `controller="applicationset"`; on `workqueue_depth` the `priority` label carries the item priority as a decimal string
  (`0` for normal events, `-100` for initial-list and unchanged-resourceVersion resync events from the default
  `For`/`Owns` handlers; more than 25 distinct priorities collapse to `exceeded_cardinality_limit`). Default per-item
  rate limiter for this queue: exponential 5 ms → 1000 s. Controller-runtime reconcile metrics for the controller
  (all `{controller="applicationset"}`), pre-initialised when the controller starts: `controller_runtime_reconcile_total{result=error|
  requeue|requeue_after|success}`, `controller_runtime_reconcile_errors_total`,
  `controller_runtime_terminal_reconcile_errors_total`, `controller_runtime_reconcile_panics_total`,
  `controller_runtime_reconcile_timeouts_total`, `controller_runtime_max_concurrent_reconciles` (=
  `--concurrent-reconciliations`, env `ARGOCD_APPLICATIONSET_CONTROLLER_CONCURRENT_RECONCILIATIONS`, default 10),
  `controller_runtime_active_workers`, plus the histogram `controller_runtime_reconcile_time_seconds`. With leader
  election enabled, `leader_election_master_status{name="58ac56fa.applicationsets.argoproj.io"}` reports 1 on the
  leader. These sit alongside the `argocd_appset_*` families of OPS-080.
- **Edge cases**: Application (owned) and cluster Secret watch events feed the same `applicationset` queue, so its
  metrics combine all three event sources. Events filtered out by the namespace event filter never reach the queue.
  The queue and the reconcile-metric children are created only when the controller starts, so with leader election on,
  standby replicas export no `applicationset` workqueue or `controller_runtime_*` series at all.
- **Configuration surface**: `--metrics-addr`; `--concurrent-reconciliations` /
  `applicationsetcontroller.concurrent.reconciliations.max`; `--enable-leader-election` (OPS-055).
- **Exposure**: ApplicationSet controller `:8080/metrics`.
- **Tests required**:
  - Positive: `/metrics` exposes `workqueue_depth{name="applicationset",controller="applicationset",priority=...}` and the other workqueue families.
  - Positive: `controller_runtime_max_concurrent_reconciles{controller="applicationset"}` equals the configured concurrency (default 10); `controller_runtime_reconcile_total` has the four `result` series from start.
  - Positive: a reconcile error increments `controller_runtime_reconcile_errors_total` and `controller_runtime_reconcile_total{result="error"}`.
  - Positive: on startup with existing ApplicationSets, depth briefly reports items under `priority="-100"`.
  - Positive: with leader election on, only the leader reports `leader_election_master_status` = 1, and the standby exports no `workqueue_*{name="applicationset"}` series.

### OPS-170 Kubectl debug trace logging (`ARGOCD_TRACING_ENABLED=1`)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/kube/kubectl.go#L17 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/kube/kubectl.go#L18 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/kube/kubectl.go#L22 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/kube/kubectl.go#L27 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/kube/kubectl.go#L31 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/utils/tracing/api.go#L9 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/utils/tracing/nop.go#L8 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/utils/tracing/logging.go#L18 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/utils/tracing/logging.go#L40 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/utils/kube/ctl.go#L169 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/utils/kube/ctl.go#L182 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/utils/kube/ctl.go#L206 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/utils/kube/ctl.go#L230 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/utils/kube/ctl.go#L254 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/utils/kube/ctl.go#L302 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/utils/kube/ctl.go#L333 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/utils/kube/ctl.go#L344 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/utils/kube/ctl.go#L356 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/utils/kube/resource_ops.go#L263 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/utils/kube/resource_ops.go#L320 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/utils/kube/resource_ops.go#L337 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/utils/kube/resource_ops.go#L361 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/utils/kube/resource_ops.go#L391 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/cache/cluster.go#L207 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-application-controller/commands/argocd_application_controller.go#L181 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sync.go#L64 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/sync.go#L346 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/hook.go#L182 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/controller/appcontroller.go#L1464 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L1030 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L1574 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/application/application.go#L3029 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/app.go#L414 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/log/logrus.go#L27 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/gitops-engine/pkg/utils/tracing/logging_test.go#L17 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/developer-guide/debugging-locally.md#L46
- **Maturity**: GA (undocumented debug switch; no maturity entry)
- **Semantics**: At process start (`util/kube` package `init`), if env `ARGOCD_TRACING_ENABLED` equals exactly `1`,
  the package tracer becomes gitops-engine's `LoggingTracer`; otherwise it stays a `NopTracer`. Every `KubectlCmd`
  created through `util/kube.NewKubectl()` or `util/kube.ManageServerSideDiffDryRuns()` carries that tracer, and the
  resource-operation objects it creates (`ManageResources`, `ManageServerSideDiffDryRuns`) inherit it. Each traced call
  opens a span and, when the call returns, logs one entry with message `Trace` and fields `operation_name`, `time_ms`
  (elapsed milliseconds as a float) plus the span baggage. Span names and baggage: `GetAPIResources`,
  `GetServerVersion` (no baggage); `GetResource`, `CreateResource`, `PatchResource`, `DeleteResource` (`kind`,
  `name`); `ConvertToVersion` (`from`, `to` group/version); resource operations `ApplyResource`, `CreateResource`,
  `ReplaceResource`, `UpdateResource`, `AuthReconcile` (`kind`, `name`). Where it applies: the application controller
  (sync apply/create/replace/update and RBAC auth-reconcile through gitops-engine sync, hook create/delete, resource
  deletion during app deletion, server-side-diff dry-run applies), the API server (cluster/app server-version and
  API-resource discovery, resource get/patch/delete actions, server-side diff) and `argocd admin app
  get-reconcile-results`. It is separate from OTLP tracing (OPS-085): nothing is exported, only log lines.
- **Edge cases**: Any value other than `1` (`true`, `yes`, empty) leaves tracing off; the switch is read once and needs a
  restart. No `argocd-cmd-params-cm` key or manifest maps it, and the env-only register (OPS-032) does not list it. The
  tracer's logger is built at package-init time from `ARGOCD_LOG_FORMAT`/`ARGOCD_LOG_LEVEL` as present in the process
  environment *before* `--logformat`/`--loglevel` are applied. With the stock manifests (which set neither variable)
  trace lines are therefore JSON at info level even when the component runs with `text` format or `warn` level. The same
  package logger is the `KubectlCmd` logger. The gitops-engine cluster cache builds its own `KubectlCmd` with a
  `NopTracer`, so cluster-cache discovery/list/watch is never traced. Baggage field order is not stable (map).
- **Configuration surface**: env `ARGOCD_TRACING_ENABLED` (`1` = on) on argocd-application-controller, argocd-server, or
  the `argocd admin` process; output format/level follow env `ARGOCD_LOG_FORMAT` / `ARGOCD_LOG_LEVEL`.
- **Exposure**: Component logs only (no CRD/API/CLI flag/UI).
- **Tests required**:
  - Positive: controller with `ARGOCD_TRACING_ENABLED=1` syncing a Deployment logs `Trace` entries with `operation_name="ApplyResource"`, `kind="Deployment"`, `name=<name>` and a numeric `time_ms`.
  - Positive: API server with the env set logs `PatchResource`/`DeleteResource` traces for UI resource actions, and `GetServerVersion` traces for cluster info.
  - Negative: env unset or `ARGOCD_TRACING_ENABLED=true` → no `Trace` lines from kubectl operations.
  - Edge: component started with `--logformat text` and no `ARGOCD_LOG_FORMAT` env still emits the kubectl trace lines in JSON.
  - Negative: cluster-cache list/watch activity produces no trace lines even with tracing on.

### OPS-171 External command timing trace logs (`exec <binary>` spans)
- **Upstream**:
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/exec/exec.go#L65 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/exec/exec.go#L67 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/exec/exec.go#L68 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/exec/exec.go#L69 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/exec/exec.go#L74 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/exec/exec.go#L286 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/client.go#L1840 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/cmd.go#L51 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/cmd.go#L62 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/cmd.go#L184 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/client.go#L170 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/helm/helm.go#L140 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/kustomize/kustomize.go#L28 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/sourceintegrity/gpg.go#L235 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmpserver/plugin/plugin.go#L90 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/cli/cli.go#L186 ,
  https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/log/logrus.go#L27
- **Maturity**: GA
- **Semantics**: Always on (not gated by `ARGOCD_TRACING_ENABLED`). Every external command run through
  `util/exec.Run`, `RunWithRedactor` or `RunWithExecRunOpts` opens a `LoggingTracer` span named `exec <argv[0]>`
  (e.g. `exec git`, `exec helm`, `exec kustomize`, `exec gpg`) with baggage `dir` (working directory) and `args`
  (`fmt` rendering of the full argv, e.g. `[git fetch origin --tags --force]`, passed through the redactor when the
  caller supplies one). When the command returns (success, non-zero exit or timeout kill) one info-level `Trace` log
  entry is written with `operation_name`, `time_ms`, `dir`, `args`. Users: the git client (all git invocations, no
  redactor; repo-server and commit-server), Helm (`RunWithRedactor`; the redactor rewrites `--username <v>` and
  `--password <v>` to `******`), Kustomize and GnuPG (`util/sourceintegrity` key generation/import in the repo-server). The logger is created per call
  from `ARGOCD_LOG_FORMAT`/`ARGOCD_LOG_LEVEL`, which `SetLogFormat`/`SetLogLevel` export from each component's
  `--logformat`/`--loglevel`, so these lines follow the component's configured format and level.
- **Edge cases**: At `--loglevel warn` or higher the lines are suppressed. Git args are logged unredacted (credentials are
  supplied via askpass/env, not argv). `util/exec.RunCommand`/`RunCommandExt` called directly do not trace, and CMP
  sidecar plugin commands use their own runner (no `Trace` line). Timeout/termination behaviour (`ARGOCD_EXEC_TIMEOUT`,
  `ARGOCD_EXEC_FATAL_TIMEOUT`) belongs to OPS-037; the trace still records the elapsed time of a killed command.
- **Configuration surface**: none (log level/format only).
- **Exposure**: Component logs (repo-server, commit-server; any process using these helpers).
- **Tests required**:
  - Positive: a repo-server manifest generation for a Git+Helm app logs `Trace` entries with `operation_name="exec git"` and `operation_name="exec helm"`, each with `dir`, `args` and numeric `time_ms`.
  - Positive: a Helm repository with username/password (`helm repo add --username ... --password ...`) logs `args` containing `--username ******` and `--password ******`; registry login uses `--password-stdin`, so no secret appears in its args.
  - Negative: repo-server at `--loglevel warn` emits no `Trace` lines.
  - Positive: `--logformat text` renders the trace lines in text format.
  - Recovery: a git command killed by `ARGOCD_EXEC_TIMEOUT` still produces its `Trace` line.

---

## Unconfirmed

- **OPS-094 runtime diagnostics**: `stats.RegisterStackDumper`, `stats.StartStatsTicker`, `stats.RegisterHeapDumper` come
  from the external module `github.com/argoproj/pkg/v2/stats`, which is not vendored in the checkout. The exact trigger
  signals (expected SIGUSR1 for stack dump, SIGUSR2 for heap profile) and the stats log line format could not be read.
  Paths checked: `util/stats/stats.go` (only `TimingStats`), `cmd/argocd-*/commands/*.go`, `go.mod`.
- **OPS-079 gRPC metric names**: only `grpc_server_handled_total` and `grpc_server_msg_sent_total` are named in the docs;
  `grpc_server_started_total`, `grpc_server_msg_received_total` and `grpc_server_handling_seconds` are the standard names
  of `go-grpc-middleware/providers/prometheus`, whose source is not in the checkout. Paths checked: `server/server.go`,
  `reposerver/server.go`, `cmpserver/server.go`, `docs/operator-manual/metrics.md`.
- **OPS-084 notifications metrics**: defined in the external `notifications-engine` module (`controller.NewMetricsRegistry`);
  names/labels taken from `docs/operator-manual/notifications/monitoring.md` and the notifications Grafana dashboard.
  Paths checked: `cmd/argocd-notification/commands/argocd_notification.go`, `notification_controller/controller/controller.go`.
- **OPS-117 GKE ingress**: the statement that the CLI needs `--grpc-web` behind GCLB is an inference (the doc configures only
  an HTTP backend and does not mention gRPC). Path checked: `docs/operator-manual/ingress.md` (GKE section).
- **OPS-119 Ambassador**: the claim that the CLI sends the port in the Host header is a doc statement; CLI transport code
  (`pkg/apiclient`) was not audited for it (owner 08).
- **OPS-116 AWS ALB**: "argo currently downgrades non-grpc calls to HTTP1" is a doc statement about cmux matching; not
  separately tested. Path checked: `server/server.go` (cmux setup).
- **OPS-052 Redis HA**: failover timings and HAProxy health-check behaviour are defined by the generated redis-ha chart
  templates/values and were not exercised. Paths checked: `manifests/ha/base/redis-ha/chart/values.yaml`,
  `manifests/ha/base/redis-ha/chart/requirements.yaml`.
- **OPS-157/158/159/161 upgrade registers**: bundled Helm/Kustomize versions mentioned in older notes (3.18.4, 5.7.0,
  3.19.2, 5.8.0) are historical; the 3.5.5 baseline versions come from `hack/tool-versions.sh` (OPS-162). Behaviour
  details of each register item are owned and verified by the owner areas named in each row.
- **OPS-057 `GOMEMLIMIT`**: pure Go-runtime guidance with no Argo CD code involved (doc-only).
- **OPS-168/169 dependency-defined details**: workqueue family names, label sets, histogram buckets, the 500 ms
  unfinished-work refresh, retry counting on `AddAfter`, the priority-queue default, the `applicationset` controller name
  and the `controller_runtime_*` families come from client-go v0.36.1 and controller-runtime v0.24.1 (versions pinned in
  `go.mod`), read from a local Go module cache rather than the argo-cd checkout. The argo-cd test
  `controller/metrics/metrics_test.go` (`TestWorkqueueMetrics`) confirms only the presence of six families and the
  `name`/`controller`/`priority` labels for a test queue. Paths checked: `controller/metrics/metrics.go`,
  `controller/appcontroller.go`, `applicationset/controllers/applicationset_controller.go`,
  `cmd/argocd-applicationset-controller/commands/applicationset_controller.go`, `go.mod`.
- **`rest_client_requests_total` on controller endpoints**: controller-runtime registers
  `rest_client_requests_total{code,method,host}` in its registry and hooks client-go's request-result metric at
  package init; `util/metrics/kubectl/kubectl_metrics.go` (L185) later overwrites that hook directly. So this family
  should only carry series for requests made before `RegisterWithClientGo()` runs. Not verified at runtime.
- **OPS-170/171 log-line shape**: the logr→logrus bridge is `github.com/bombsimon/logrusr/v4` v4.1.0 (`go.mod` L28),
  whose source is not in the checkout or the local module cache. Two points are inferred from the gitops-engine test
  (`logger.Info(0, "Trace")` with `operation_name`/`time_ms` values) and logr conventions, not read from the adapter:
  that V(0) `Info` maps to logrus `info`, and that the custom formatter in `util/log/logrus.go` renders every value via
  `fmt` `%v` (so JSON output would carry `time_ms` as a string). Paths checked: `util/log/logrus.go`,
  `gitops-engine/pkg/utils/tracing/logging_test.go`.

## Doc/implementation differences

- **`server.metrics.listen.address` (OPS-022)**: the server binds metrics on `--address`'s host; the env value is used as
  the *flag name* in `StringVar(&metricsHost, env.StringFromEnv("ARGOCD_SERVER_METRICS_LISTEN_ADDRESS","metrics-address"), ...)`
  and `Listen()` uses `ListenHost` for the metrics listener, so the documented key never changes the metrics bind address.
- **`reposerver.metrics.listen.address` (OPS-023)**: manifests map it to `ARGOCD_REPO_SERVER_LISTEN_METRICS_ADDRESS` but
  the binary reads `ARGOCD_REPO_SERVER_METRICS_LISTEN_ADDRESS`; the key has no effect.
- **`reposerver.parallelism.limit` (OPS-023/035)**: reference yaml shows `"1"`; compiled default is `0` (unlimited).
- **Workqueue defaults (OPS-046)**: high_availability.md documents base delay 1000 ns (1μs) and max delay 3 s; code
  defaults are 1 ms and 1 s.
- **`ARGO_CD_UPDATE_CLUSTER_INFO_TIMEOUT` (OPS-050)**: documented as seconds; code parses a Go duration bounded 10s–1m.
- **`dexserver.connector.failure.continue` (OPS-025/160)**: reference yaml shows `"false"` with "to fail the entire auth
  flow ... set this to false"; per the 3.4 note Dex's own default is enabled when the key is unset.
- **argocd-cm reference (OPS-029/060)**: `timeout.hard.reconciliation` (consumed by manifests), `globalProjects`,
  `server.rbac.disableApplicationFineGrainedRBACInheritance` and split `extension.config.<name>` keys are not in
  `argocd-cm.yaml`.
- **argocd-cmd-params-cm reference (OPS-028)**: keys wired in manifests but absent from the reference
  (`controller.self.heal.backoff.cooldown.seconds`, `controller.ignore.normalizer.jq.timeout`,
  `server.content.security.policy`, `server.http.cookie.maxnumber`, `reposerver.helm.manifest.max.extracted.size`,
  `reposerver.disable.helm.manifest.max.extracted.size`, `reposerver.revision.cache.lock.timeout`,
  `reposerver.grpc.max.size`, `reposerver.helm.user.agent`, `applicationsetcontroller.repo.server.plaintext/.strict.tls/
  .timeout.seconds`); `--helm-registry-max-index-size` has an env var but no key (OPS-148).
- **`controller.metrics.application.labels` (OPS-067)**: metrics.md shows this cmd-params key, but
  `--metrics-application-labels` has no env binding and no manifest mapping; the key does nothing.
- **gRPC metrics toggle (OPS-079)**: metrics.md says gRPC metrics only show up with `ARGOCD_ENABLE_GRPC_TIME_HISTOGRAM=true`
  (and "not exposed by default" on the repo-server); code always registers the gRPC counters, the env only adds the
  handling-time histogram.
- **Undocumented metrics (OPS-077/082)**: `argocd_git_lsremote_fail_total` (repo-server) and `argocd_info{version}` (API
  server) exist in code but not in metrics.md.
- **Metric names in flag help (OPS-067/068/080)**: help texts mention `argocd_application_labels`,
  `argocd_application_conditions`, `argocd_applicationset_labels`; real names are `argocd_app_labels`,
  `argocd_app_condition`, `argocd_appset_labels`.
- **GitHub API metric labels (OPS-081)**: doc says labels "name and namespace of an applicationset"; real label names are
  `appset_name`, `appset_namespace` (plus `method`, `endpoint`, `status`, `resource`).
- **TLS cipher default (OPS-097)**: tls.md lists `TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384:TLS_RSA_WITH_AES_256_GCM_SHA384`;
  code default (and generated server-commands docs) is `TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384` only.
- **Notifications CA flag name (OPS-101)**: tls.md/mtls.md name `--argocd-repo-server-ca-cert-path` for the notifications
  controller; the shared helper registers `--repo-server-ca-cert-path` (env `ARGOCD_NOTIFICATION_CONTROLLER_REPO_SERVER_CA_CERT_PATH`).
- **Legacy strict-TLS mount path (OPS-101)**: the 3.5 note says the deprecated flag loads `/app/config/server/tls/`; that is
  true only for argocd-server — the controller uses `/app/config/controller/tls/`, appset/notifications
  `/app/config/reposerver/tls/`. tls.md describes it as auto-discovery "from the Secret" while code reads mounted files.
- **mTLS auto-mount (OPS-103)**: mtls.md says the Secret is mounted "in every relevant pod"; the dynamic-distribution
  controller Deployment variant (`manifests/base/application-controller-deployment`) mounts neither
  `argocd-repo-server-mtls` nor maps `otlp.attrs`. `server-ca.crt` is mounted but unused unless
  `--repo-server-ca-cert-path` points at it.
- **Dex strict TLS (OPS-102)**: tls.md describes CA validation; code additionally pins the exact leaf certificate
  (`dex server certificate does not match`).
- **Static assets default (OPS-111)**: the 3.1 upgrade note names `/app/shared`; code default is `/shared/app`.
- **managed-by-url validation (OPS-124)**: the doc says invalid URLs prevent creation/update; validation exists only in
  the API server create/update path — Applications applied by kubectl or synced by a parent app are accepted and only
  rendered defensively by the UI.
- **Security doc TLS claim (OPS-104)**: "All network communication is performed over TLS including service-to-service";
  the controller→commit-server gRPC, metrics/health endpoints and default Redis traffic are plaintext.
- **Security doc refresh interval (OPS-058)**: webhook section says regular refresh occurs "at three minute intervals";
  controller default is 120 s plus up to 60 s jitter (repo-server revision cache default is 3 m).
- **`argocd admin` examples**: `argocd admin initial-password reset` (no `reset` subcommand; OPS-031);
  `settings validate --group plugins` (no such group, nil validator; OPS-126); `resource-overrides action list` (command is
  `list-actions`; OPS-130); `cluster kubeconfig` with 0/1 args or `--delete` (requires exactly 2 args, no `--delete`;
  OPS-135); `cluster stats target-cluster` (positional ignored; OPS-133); `app get-reconcile-results APPNAME` (takes an
  output PATH; OPS-132).
- **Missing generated references (OPS-150/151)**: `docs/operator-manual/server-commands/` has no pages for
  `argocd-notifications`, `argocd-commit-server`, `argocd-cmp-server`, `argocd-k8s-auth`, `argocd-git-ask-pass`.
- **Workqueue / controller-runtime metrics (OPS-168/169)**: `docs/operator-manual/metrics.md` documents no
  `workqueue_*` or `controller_runtime_*` families and none of the queue names (`app_reconciliation_queue`,
  `app_operation_processing_queue`, `project_reconciliation_queue`, `app_hydration_queue`, `manifest_hydration_queue`,
  `applicationset`). The shipped example dashboard (`examples/dashboard.json`, "Workqueue Depth") nevertheless queries
  `workqueue_depth{name=~"app_.*"}`.
- **`ARGOCD_TRACING_ENABLED` (OPS-170)**: appears only as a line in the developer-guide example env file
  (`docs/developer-guide/debugging-locally.md` L46). It appears nowhere in the operator manual or any other doc page, and its trace output ignores the component's `--logformat`/`--loglevel` unless
  `ARGOCD_LOG_FORMAT`/`ARGOCD_LOG_LEVEL` are set in the environment.
- **Exec trace logs (OPS-171)**: the always-on `Trace` log line for every git/helm/kustomize/gpg invocation is not
  documented anywhere.
