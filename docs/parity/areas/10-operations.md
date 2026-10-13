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
  (with default exclusions/ignoreResourceUpdates, see OPS-137), `argocd-cmd-params-cm` (empty), `argocd-gpg-keys-cm`,
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
  Kubernetes (Application/ApplicationSet CRs) or the CLI in core mode (OPS-114) / `argocd admin dashboard` (OPS-115).
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
  - Positive: `argocd app list --core` works against this install (OPS-114).

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
- **Edge cases**: repo-server has no K8s privileges by design (security property; OPS-139). Notifications controller can
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
- **Configuration surface**: `--port`, `--metrics-port`, `--address`, `--metrics-address` flags per component (OPS-128..136).
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
  - Recovery: API server returns 503 during a settings-triggered restart (OPS-096/098) and 200 afterwards.

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
  `argocd-cmd-params-cm`. Changes propagate to subscribers without restart except where noted (OPS-017, OPS-096).
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
- **Exposure**: CLI `argocd admin redis-initial-password` (OPS-125); install artifact.
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
  The notifications controller has no `--argocd-repo-server` env mapping (uses its compiled default, OPS-131).
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
  | `url`, `additionalUrls` | 07 (SSO/URLs; change restarts API server, OPS-096) |
  | `statusbadge.enabled`, `statusbadge.url` | UI/API (badge) |
  | `users.anonymous.enabled`, `users.session.duration`, `passwordPattern`, `admin.enabled`, `accounts.<name>`, `accounts.<name>.enabled` | 07 |
  | `ga.trackingid`, `ga.anonymizeusers`, `help.chatUrl`, `help.chatText`, `help.download.{linux-amd64,linux-arm64,linux-ppc64le,linux-s390x,darwin-amd64,darwin-arm64,windows-amd64}` | UI |
  | `dex.config`, `oidc.config`, `oidc.tls.insecure.skip.verify` | 07 |
  | `resource.customizations.{ignoreDifferences,health,actions,knownTypeFields}.<group_kind>`, `resource.customizations.ignoreDifferences.all`, `resource.customizations` (legacy single key), `resource.compareoptions`, `resource.sensitive.mask.annotations` | 04 / 05 (actions) |
  | `resource.ignoreResourceUpdatesEnabled`, `resource.customizations.ignoreResourceUpdates.{all,<group_kind>}` | **this file** (OPS-062/063) |
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
  (OPS-096).
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
- **Exposure**: Pod env; `argocd admin cluster shards/stats` (OPS-121/122).
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
