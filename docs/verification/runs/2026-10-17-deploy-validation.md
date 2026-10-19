# Deployment validation (2026-10-18)

Harness `test/deploy` (package `deploy_test`, documented in `test/deploy/doc.go`) for the Helm
chart `deploy/helm/patrol-cd` (0.1.0), the static manifests `deploy/manifests` and
`deploy/crds`. Findings: [`../phase4-findings.md`](../phase4-findings.md) (DV-1 … DV-9). Parity
overlay: `docs/parity/status/deploy-validation.yaml`. (File name kept from the task assignment;
the runs below were executed on 2026-10-18.)

> **Historical record.** The counts and results below are those of run 1 on the tree of that
> run. The harness, the chart and the bootstrap changed afterwards (review fixes:
> [`2026-10-18-phase4-review-fixes.md`](2026-10-18-phase4-review-fixes.md), second round:
> [`2026-10-18-phase4-review-round2.md`](2026-10-18-phase4-review-round2.md)); see the
> addendum at the end for what step 2 and step 8 of the Helm lifecycle now assert. The final
> suite result is recorded in `docs/verification/log.md`.

Environment: macOS 27.0.1 arm64, Go 1.27.2, Helm v4.3.1 (host; the image pins 4.2.1), kustomize
v5.8.1 (`bin/kustomize`), git 2.56.0, envtest kube-apiserver/etcd v1.36.0
(`bin/envtest/k8s/1.36.0-darwin-arm64`), golangci-lint 2.15.0. No Docker, kind, kubelet,
scheduler, controller-manager or CNI was used.

## Results

| # | Command (repository root) | Exit | Result |
|---|---------------------------|------|--------|
| 1 | `KUBEBUILDER_ASSETS=$PWD/bin/envtest/k8s/1.36.0-darwin-arm64 PATROL_REQUIRE_ENVTEST=1 PATROL_REQUIRE_DEPLOY_TOOLS=1 go test -race -count=1 -timeout 40m -v ./test/deploy/` | 0 | `ok github.com/juliosaraiva/patrol-cd/test/deploy 246.792s`; wall clock 255 s; 16 top-level tests and 142 subtests PASS, 0 SKIP (the `PATROL_REQUIRE_*` variables turn any skip into a failure). The patrolcd binary run by the RBAC tests was built with `-race` too; no data race reported in any component log. |
| 2 | `go vet ./test/deploy/` | 0 | |
| 3 | `golangci-lint run ./test/deploy/...` | 0 | `0 issues.` |
| 4 | `make helm-lint` (after the DV-1 chart fix) | 0 | defaults and 10 profiles lint `--strict`; 23/23 negative value files refused with the expected message (new: `ci/invalid/appset-namespaces-without-scm-allowlist.yaml`) |
| 5 | `go run ./hack/helmdocs -check`; `go test -count=1 ./hack/helmdocs/` | 0, 0 | chart generated files current; `ok … 1.878s` |
| 6 | `HELM=helm go run ./hack/genmanifests -check` | 0 | `deploy/manifests is up to date (37 generated files)` |
| 7 | `go run ./hack/parity -root <scratch copy of docs/>` then `-check` | 0, 0 | the overlay parses and references known IDs (the repository's matrix.md was not regenerated) |

Earlier iterations of run 1 (without `-race`, single tests) were used while building the
harness; their failures are the findings and harness fixes described below, not results.

## What each test checks (counts from run 1)

| Test | Checks | Counts |
|------|--------|--------|
| `TestChartLint` | `helm lint --strict --kube-version 1.36.0 -f ci/<profile>.yaml`, no `[WARNING]`/`[ERROR]` lines | 10 profiles |
| `TestChartRenderDeterministic` | two `helm template` processes per profile produce identical bytes; every object has apiVersion, kind, name; no duplicate kind/namespace/name | 10 profiles, 633 objects |
| `TestValuesSchema` | every profile accepted; rejected: key typos at top level, component, nested and `global` level, wrong types (string for integer/boolean, list for map, map for list, negative replicas), invalid enums (`internalTLS.mode`, `podAntiAffinity`, `rbac.scope`, `image.pullPolicy`, Service type), and every `ci/invalid/*.yaml` with its `# expect:` message | 10 accepted, 37 rejected (23 from ci/invalid) |
| `TestAPIServerDryRun` | server-side apply of each rendered document's raw YAML with `dryRun=All`, `fieldValidation=Strict`, `force`, admission included (Patrol CRDs and the Operation ValidatingAdmissionPolicy installed); any error **or warning** fails; namespaced objects must carry a namespace, cluster-scoped ones none | 612 objects validated; 21 third-party objects (cert-manager Certificate/Issuer, ServiceMonitor, PrometheusRule, HTTPRoute in `ha`/`all-features`) not validated (no CRD in envtest) |
| `TestAPIServerDryRunNegativeControls` | the checks above are live: a values typo inside `global.containerSecurityContext` is rejected (`field not declared in schema`), a duplicated YAML key is rejected, a warning (unqualified finalizer on a custom resource) is reported, an unknown kind is reported | 4 controls |
| `TestRenderedCRDsMatchDeployCRDs` | the chart's CRD templates and admission policy equal `deploy/crds` (spec and labels) and carry `helm.sh/resource-policy: keep` | 10 objects |
| `TestPodSecurityRestricted` | a Pod built from every pod template is created (dry run, strict) in a namespace enforcing, auditing and warning `restricted:latest`; no warning allowed. Negative controls: a privileged pod and a baseline-only pod are rejected by PodSecurity (proves the plugin enforces in envtest, which disables only the ServiceAccount plugin) | 52 pods, 66 containers (init containers included), 2 controls |
| `TestWiring` | every patrolcd container (argv[0] base name `patrolcd`) parsed with `server.NewRootCommand()` → `Find` → `ParseFlags` (unknown flags, bad values and positional arguments fail); `PATROL_*` variables checked against `settings.ParamsForComponent` and the wiring.md table (unknown names fail, `PATROL_FAKE_IN_CLUSTER`/askpass variables must never be set, `configMapKeyRef` key must be the parameter's key and optional); listeners computed from the parsed flags (wiring.md port table) ⇔ containerPorts; readiness/liveness/startup probe ports and paths per listener (liveness must not use readiness semantics where `/livez` exists); every Service selects exactly one pod template and targets declared ports; the StatefulSet's governing Service is headless with port 8090; files the binaries read (internal token, trust ConfigMaps with their keys, profiler toggle with the component's `<prefix>.profile.enabled` key, mTLS client pairs when the repo-server requires client certificates, every absolute path flag) are provided by a volume with the right item, writable directories are emptyDirs, `/tmp` is a writable emptyDir; keys missing from non-optional volumes fail; the bootstrap pod's token volume is optional and its main container does not pass `--internal-token-file`; CMP sidecars share the socket volume with the repo-server and have `plugin.yaml`; Kubernetes-facing pods mount a token and set `POD_NAMESPACE`, the others do not mount one | 62 patrolcd containers in 10 profiles (4 others not parsed: Dex, `copyutil`) |
| `TestWiringNegativeControls` | 11 broken variants of the default profile are each reported (unknown flag, bad flag value, positional argument, unknown/miswired/development variable, undeclared port, liveness on `/readyz`, token not mounted, `--internal-token-file` next to the bootstrap, unmounted path flag); the unmodified profile has no problem | 11 controls |
| `TestDocumentedEnvIsRead` | every documented variable of the expectations appears in non-test Go code under internal/, cmd/, pkg/ | 49 variables |
| `TestParamsResolve` | each container's parameters resolved with `settings.ResolveParams` from the rendered patrol-cmd-params-cm, literal environment and flags (any issue fails); the ApplicationSet controller's startup precondition (DV-1) | 50 containers in 10 profiles |
| `TestRBACSufficiency` | see "RBAC sufficiency" below | 8 runs |
| `TestRBACMissingRuleDetected` | inverse: default profile with the notifications controller's two Lease rules and patrol-server's AppProject/Cluster/Repository/RepositoryCredential rule removed from the rendered Roles: both components are reported (audit 403s matched to the service account and resource, and "forbidden" lines in both containers' logs) | 9 denied requests detected (1 notifications, 8 server) |
| `TestHelmLifecycle` | helm CLI against a fresh envtest (admin kubeconfig, isolated helm home, `--no-hooks`, no `--wait`): see "Helm lifecycle" | 1 run |
| `TestStaticManifestsMatchChart` | every single-file manifest equals the chart render with its profiles (Helm metadata removed) plus only the Pod-Security-labelled Namespace in cluster-wide variants; `kustomize build` of each base yields the same objects in the same order; `crds.yaml` equals deploy/crds | 10 variants + crds.yaml |
| `TestStaticManifestsApply` | `install.yaml` server-side applied object by object (strict, warnings fail) to a fresh envtest, re-applied (0 objects changed: generation for objects with one, resourceVersion otherwise), deleted in reverse order (60 objects gone; the Namespace stays Terminating: no namespace controller); strict dry run of all 11 files against the shared envtest | 61 objects applied; 563 objects dry-run across 11 files |

## RBAC sufficiency (`TestRBACSufficiency`)

Per run: render the profile for release `patrol` in namespace `rbac-<run>`; install every
rendered object except the CRDs and admission policy (already installed from deploy/crds,
identical per `TestRenderedCRDsMatchDeployCRDs`) with server-side apply and strict validation;
request a token for each service account that a pod mounts (TokenRequest API); run every
Deployment/StatefulSet pod (one replica) through the pod emulator; drive the scenario; stop the
pods with SIGTERM (exit status 0 required within `terminationGracePeriodSeconds`; slowest stop
1.02 s); then read the kube-apiserver audit log (policy: Metadata for all requests, Request
level only for Operation creations) and fail on any 403 of the run's service accounts and on any
"forbidden" in a component log (no allow-list entry was needed).

Scenario (skipped parts in brackets): bootstrap init container creates `patrol-internal-token`,
`patrol-secret` keys and (admin enabled) `patrol-initial-admin-secret`; [API profiles] login as
admin with that password, repository with credentials and an Application (automated sync) through
the API, [else] the same objects with the admin client (core: AppProject `default` created first,
DV-2); the controller renders through the repo-server from a local git smart-HTTP server and
syncs a ConfigMap into the destination namespace with its own identity; Application Synced and
Healthy, an automated Operation Succeeded; [API] resource tree (controller tree API), managed
resources, [local-cluster RBAC] live resource view and Application events (patrol-server's own
identity); ApplicationSet with a list generator (2 elements) creates two Applications, which the
controller compares; [API] manual sync of one of them (Operation created by patrol-server on the
user's behalf, `impersonate`); Leases held: controller shard 0, and the leader election Leases of
the ApplicationSet and notifications controllers when they run with `--enable-leader-election`;
SubjectAccessReviews documenting the profile's scope.

| Run | Values | Objects installed | Pods | Audited requests (service accounts) | Denied | "forbidden" in logs | SubjectAccessReviews |
|-----|--------|-------------------|------|-------------------------------------|--------|---------------------|----------------------|
| default | ci/default.yaml | 50 | 5 | 489 (4) | 0 | 0 | 6 as documented: controller `*/*/*` allowed (cluster-admin by design); server may get but not create Secrets in kube-system; ApplicationSet and notifications controllers and the repo-server have no local-cluster access |
| core | ci/core.yaml | 40 | 4 | 367 (3) | 0 | 0 | controller `*/*/*` allowed |
| namespaced | ci/namespaced.yaml + local cluster registered with namespaces [release, team-a] | 54 | 5 | 504 (4) | 0 | 0 | 5: controller writes in the release namespace and team-a only, no cluster-scoped list; server no live views outside |
| minimal | ci/minimal.yaml | 33 | 4 | 448 (3) | 0 | 0 | 1 (no notifications RBAC) |
| hydrator | ci/hydrator.yaml | 55 | 6 (commit-server included) | 484 (4) | 0 | 0 | — |
| ingress-nginx | ci/ingress-nginx.yaml (`--insecure` server) | 51 | 5 | 486 (4) | 0 | 0 | — |
| apps-any-namespace | ci/default.yaml + `applicationNamespaces: [rbac-apps-*]`, `applicationSetNamespaces: [rbac-apps-team]`, SCM allow-list; Applications and the ApplicationSet in `rbac-apps-team` (project `sourceNamespaces` patched) | 60 | 5 | 495 (4) | 0 | 0 | 3: pattern ⇒ cluster-wide Application RBAC; ApplicationSet controller only in the listed namespace |
| hardened | ci/hardened.yaml + internal TLS Secrets from a test CA (`existingSecret`, verify, mTLS) | 48 | 5 | 413 (4) | 0 | 0 | 4: controller reads everything, writes core resources but no RBAC; server no local access |

The repo-server and commit-server made no API request (no token mounted). In every run the
readiness, liveness and startup probes of every container answered 200 at their mapped ports.

### Pod emulation (deviations from a real kubelet)

`test/deploy/podrunner_test.go` runs `go build ./cmd/patrolcd` once (with `-race` under `-race`)
and starts each rendered patrolcd container as a process with:

1. **Arguments**: the rendered `command`+`args` (`$(VAR)` expanded), argv[0] replaced by the
   built binary. Appended (pflag: the last value wins): listeners moved to loopback ports
   (`--address=127.0.0.1 --port=…`, `--metrics-addr=127.0.0.1:…`, `--healthz-addr`,
   `--tree-api-addr`, `--webhook-addr`, from the same listener table as `TestWiring`);
   patrol-server's `--controller-tree-api-url=<scheme>://127.0.0.1:<controller tree port>`
   instead of shard routing through StatefulSet pod DNS (so patrol-server's `get leases` is not
   exercised).
2. **Volumes**: each Secret/ConfigMap/projected/emptyDir volume materialized in a per-pod
   directory from the live objects (retried while a required one is missing, like the kubelet;
   optional ones may be absent or empty). Container paths in arguments and environment values
   are rewritten to those directories; implicit default paths are passed explicitly only when a
   volume covers them (`--ssh-known-hosts-path`, `--tls-certs-dir`, `--gpg-keys-dir`,
   `--plugin-socket-dir`, the repo-server TLS pair) or through the documented variables
   (`PATROL_INTERNAL_TOKEN_FILE`, `PATROL_ENABLE_PROFILER_FILE_PATH`), so an empty optional token
   volume still selects patrol-server's API fallback, as in a pod.
3. **Environment**: as rendered (literal values, `fieldRef`, `resourceFieldRef`, ConfigMap and
   Secret references read from the API server); Service addresses rewritten to the loopback port
   of the selected pod; `patrol-cmd-params-cm` values or compiled defaults that name a mounted
   path or a Service presented as the parameter's environment variable; `PATH`, `HOME`, `TMPDIR`
   (the `/tmp` volume) and `LANG=C` from the harness.
4. **Identity**: `KUBECONFIG` with a TokenRequest token of the pod's service account (context
   namespace = release namespace) and `PATROL_FAKE_IN_CLUSTER=true`, so the in-cluster
   destination uses the same service account identity (one process per container, so the
   controller and patrol-server each use their own).
5. **Scale**: one replica per workload; no scheduler, so affinity, topology spread, PDBs and HPAs
   are not exercised; hostnames are the host's (single-replica sharding does not need them).

### Unexercised rules (informational)

Verb × resource cells of the rendered rules that no allowed request matched, intersected over
the runs where the rule exists (full per-run lists: run 1 log, `rbac_test.go:403`). Not proof of
excess: the scenario covers startup, sync, ApplicationSet generation and reads, not deletion,
resource actions, SCM providers or notification delivery. Candidates are discussed in DV-8.

| Service account | Rule (abridged) | Never used |
|-----------------|-----------------|------------|
| applicationset-controller | operations get/list/watch/create/delete/impersonate | all (progressive syncs only; off by default) |
| applicationset-controller | applicationsets get/list/watch/create/update/patch/delete; status get/update/patch; finalizers update | create, update, patch, delete, get; status get/update; finalizers update (watch/list and status patch used) |
| applicationset-controller | applications CRUD | get, update, patch, delete (create, list, watch used) |
| applicationset-controller | secrets, configmaps get/list/watch (twice, control plane and ApplicationSet namespaces) | get secrets, list/watch configmaps |
| controller | operations; operations/status; applications update/patch; applications/status update; applications/finalizers | delete/get/update operations, update operations/status, update/patch applications, update applications/status, update finalizers |
| controller | core events create/patch (twice) | both (events.k8s.io is used) |
| controller | clusters get/patch, clusters/status update/patch; leases watch | all of these |
| notifications-controller | applications get/patch; operations; events (both groups); its Secret get | all (no notification configured) |
| server | Role: secrets delete; configmaps create/update; leases get; */status update/patch; clusters, repositorycredentials writes; applicationsets and scmproviders writes; operations get/watch/patch | as listed |
| server | ClusterRole (local cluster): patch/delete `*`, get pods and pods/log, create jobs | resource actions, logs and terminal are not part of the scenario |

## Helm lifecycle (`TestHelmLifecycle`)

Fresh envtest, release `patrol`, namespace `lifecycle`, `-f ci/default.yaml`:

0. Install with a typo in `global.containerSecurityContext`: refused by Helm 4's server-side apply
   (`field not declared in schema`) after the CRDs were applied; release `failed`; uninstalled.
1. Install: 60/60 rendered objects present. `patrolcd bootstrap` (the init container's command,
   run with the admin kubeconfig because no pod starts) generates the runtime Secrets.
2. Upgrade with identical values: no Deployment, StatefulSet, Service or ConfigMap changed
   (resourceVersions equal); generated keys unchanged.
3. Upgrade with `image.tag=0.1.1`, `server.replicas=2`, `global.commonLabels.team=platform`,
   controller memory limit, `notificationsController.enabled=false`: selectors and Service
   clusterIPs unchanged, labels and image applied, the notifications controller's objects
   removed, the controller StatefulSet has no volumeClaimTemplates; generated keys unchanged.
4. Rollback to revision 1 (revision 4): images, labels, replicas and the notifications
   controller restored; generated keys unchanged.
5. A second cluster-wide release in another namespace is refused (`invalid ownership metadata`
   on the kept CRDs, DV-4).
6. A namespaced release (`ci/namespaced.yaml`) next to it: refused while `team-a` does not exist
   (DV-3), then 54/54 objects; uninstalled cleanly.
7. Admin password changed in `patrol-secret`; `helm uninstall`: all namespaced and cluster-scoped
   release objects gone, CRDs and admission policy kept ("kept due to the resource policy");
   `patrol-internal-token` and `patrol-initial-admin-secret` remain, `patrol-secret` is removed.
8. Re-install: 60/60 objects; bootstrap reuses the kept token and initial password; the changed
   admin password is no longer valid and the stale initial password is (DV-6).

## Chart fix made by this task (DV-1)

`deploy/helm/patrol-cd/templates/_config.tpl:140-151`, `deploy/helm/patrol-cd/ci/all-features.yaml:34-35`,
`deploy/helm/patrol-cd/ci/invalid/appset-namespaces-without-scm-allowlist.yaml`. Before:
`TestParamsResolve/all-features` failed and the real binary exited at startup in the
`apps-any-namespace` run without an allow-list. After: the render is refused with a message
naming the two remedies; accepted combinations checked by hand with `helm template`
(`enable.scm.providers=false`, an allow-list in values or in `appsetController.extraArgs`, the
ApplicationSet controller disabled, `applicationSetNamespaces` holding only the release
namespace); runs 1, 4, 5 and 6 above pass.

## NOT EXECUTED / BLOCKED

- **Image build and pods on nodes**: BLOCKED (Docker Desktop disk full, `TASKS.md`). The RBAC
  runs execute the patrolcd binary built on the host, not the image; tools inside the image
  (helm 4.2.1, kustomize, git-lfs, tini) were not used.
- **kubelet behavior**: volume mounts, the optional token volume's late population, probes by
  the kubelet, SIGTERM via the container runtime, `readOnlyRootFilesystem`/non-root enforcement,
  resource limits and `GOMEMLIMIT` effects: emulated or not exercised (see deviations).
- **NetworkPolicy enforcement**: NOT EXECUTED (no CNI); policies are only validated by the API
  server.
- **Scheduling**: affinity, topology spread, PDB eviction, HPA scaling, multi-replica controller
  sharding (`ha` profile): NOT EXECUTED.
- **cert-manager issuance** (`ha`, `all-features`), **Dex**, **CMP plugin sidecars** (`airgap`,
  `all-features`): not run in the RBAC harness (non-patrolcd containers or external
  controllers); their objects pass the wiring and dry-run checks except the 21 third-party
  objects, which were NOT validated (no CRDs in envtest; no upstream CRD with a published
  checksum was downloaded).
- **Ingress, HTTPRoute, ServiceMonitor scraping, PrometheusRule evaluation**: NOT EXECUTED.
- **Kubernetes versions other than 1.36.0**: NOT EXECUTED.
- **Notification delivery, hydration commits, resource actions, web terminal, SSO**: not part of
  the RBAC scenario (rules they need are listed as unexercised).

## Addendum 2026-10-18 (second review round)

Changes to the harness after run 1 (details and commands:
[`2026-10-18-phase4-review-round2.md`](2026-10-18-phase4-review-round2.md)):

- **Helm lifecycle step 2** now compares every rendered object (60 in the default profile)
  across the no-op upgrade: resourceVersion, or generation plus labels and annotations for the
  CRDs and the ValidatingAdmissionPolicy (the API server writes their status). Step 2 above
  covered only Deployments, StatefulSets, Services and ConfigMaps (finding F7).
- **Helm lifecycle step 8** now asserts the fixed DV-6 behavior: the internal token is kept;
  the stale `patrol-initial-admin-secret` is replaced (new password digest, new UID); the new
  password logs in through `internal/auth` against the live `patrol-cm` and `patrol-secret`;
  the first initial password and the password changed before the uninstall are rejected with
  `invalid username or password`. Step 8 above describes the behavior before the fix.
- **TestStaticManifestsApply**: the CRD deletion wait is 5 minutes (other objects 2 minutes)
  and a timeout reports the object's deletionTimestamp, finalizers, resourceVersion and, for a
  CRD, its conditions and remaining instances.
- **TestStaticManifestsMatchChart**: the core variants are rendered with
  `[install.yaml, core-install.yaml(, hydrator.yaml)]`, as the generator now does (MAN-4).
- **TestRBACSufficiency/hardened**: 10 more SubjectAccessReviews for the narrowed controller
  rules (no RBAC, admission, node, namespace or subresource writes; Secret writes in
  `kube-system` and `get nodes/proxy` remain, as documented).
- Counts that changed with the chart: 634 rendered objects in the 10 profiles (all-features
  gained an Ingress), 28 `ci/invalid` files.
