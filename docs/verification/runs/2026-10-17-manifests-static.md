# Static install manifests: verification (2026-10-18)

Generator `hack/genmanifests`, output `deploy/manifests` (37 generated files plus the hand-written
`profiles/*.yaml` and `README.md`), targets `manifests-install` / `verify-manifests` in
`hack/deploy.mk` (wired into `make generate` and `make verify-generated`), guide
`docs/guides/manifests.md`, parity overlay `docs/parity/status/manifests.yaml`, cross-task requests
`docs/verification/phase4-findings.md` (M-1..M-7). File name kept from the task assignment; the
runs below were executed on 2026-10-18.

Environment: macOS 27.0 arm64; Go 1.27.2; Helm v4.3.1 (host binary, only renders; isolated
`HELM_*_HOME`, no plugins, no kubeconfig); kustomize v5.8.1 (`bin/kustomize`, pinned in
`tools/go.mod`); kubectl v1.36.1; yq v4.54.1; jq; golangci-lint; gitleaks (`~/go/bin`). A
disposable etcd + kube-apiserver v1.36.0 started from the envtest binaries
(`bin/envtest/k8s/1.36.0-darwin-arm64`) in the session scratchpad, RBAC authorization, Pod
Security admission on (default plugins), a locally generated admin token (never printed); no
scheduler, kubelet, controller-manager or CNI. Upstream reference: the shared clone in the
scratchpad had only its pack files left, so `manifests/` of Argo CD v3.5.5
(`a5038b0e2bbac1715754380ae0084df8e3d5b48c`) was re-cloned sparsely into a separate scratch
directory and read only.

> **Values re-derived on the final tree, 2026-10-18.** The chart and the generator changed after
> these runs (first review fixes:
> [`2026-10-18-phase4-review-fixes.md`](2026-10-18-phase4-review-fixes.md); second review
> round: [`2026-10-18-phase4-review-round2.md`](2026-10-18-phase4-review-round2.md)), so every
> generated file except `crds.yaml` changed. The table "Generated single files" (sha256, object
> counts) and the sizes below were recomputed on the final tree after `make verify-manifests`
> (exit 0, `deploy/manifests is up to date (37 generated files)`), with `shasum -a 256
> deploy/manifests/<file>` and `yq -N 'select(.kind != null) | .kind' <file> | sort | uniq -c`.
> The previous hashes are kept under "Superseded hashes". Checks #1–#24, including the
> workload hashes printed by #22, ran on the earlier generation and were **not** repeated
> here. On the final tree, the equivalent checks are `TestStaticManifestsMatchChart` (every
> variant equal to the chart render with its profiles, kustomize bases identical) and
> `TestStaticManifestsApply` (install.yaml applied, re-applied with 0 changes, deleted; strict
> dry run of 563 objects across the 11 files), both passed in a targeted run on 2026-10-18
> (round-2 record); the full deploy suite result is in `docs/verification/log.md`.

## Design decisions recorded here

- Source of truth: the chart. Every variant is `helm template patrol deploy/helm/patrol-cd
  --namespace patrol --kube-version 1.36.0 -f deploy/manifests/profiles/<...>`. Profiles:
  `install` (`{}`), `namespace-install` (`crds.install/admissionPolicy: false`, `rbac.scope:
  namespaced`), `core-install` (`server`, `dex`, `notificationsController` disabled, as upstream
  core-install), `ha` (server and repo-server 2 replicas with required node anti-affinity as
  upstream; controller 2 shards, ApplicationSet/notifications controllers and commit-server 2;
  preferred node anti-affinity and soft zone spread), `hydrator` (`hydrator.enabled: true`). No
  cert-manager or Prometheus Operator dependency in any static variant.
- Helm metadata: `helm.sh/chart` and `app.kubernetes.io/managed-by: Helm` removed and **not
  replaced** (not `kustomize`): the files are applied by kubectl, kustomize, a GitOps tool or
  Patrol itself, so no fixed value would be true; upstream's static manifests carry no
  `managed-by`; a Helm adoption needs relabelling either way. `helm.sh/*` annotations (CRD
  `resource-policy: keep`) removed. Hooks and tests, Helm keys outside metadata (selectors),
  unrendered `RELEASE-NAME` and Secrets with `data`/`stringData` are refused. The chart's
  `checksum/cmd-params` pod annotation is kept (deterministic; rolls pods when a release changes
  the shipped ConfigMap).
- Namespace: objects keep `namespace: patrol` (a wrong `-n` fails); cluster-wide variants start
  with a generated `Namespace` (`app.kubernetes.io/{instance,part-of,version}`, Pod Security
  `enforce: restricted` pinned to v1.30, `audit`/`warn: restricted` latest). Namespace-scoped
  variants and `crds.yaml` have no Namespace.
- Order: Namespace, CRDs, ValidatingAdmissionPolicy, binding, NetworkPolicies, PDBs,
  ServiceAccounts, Secrets, ConfigMaps, ClusterRoles, ClusterRoleBindings, Roles, RoleBindings,
  Services, Deployments, StatefulSet, HPAs; then by namespace and name. The bases use kustomize
  `sortOptions: fifo` so `kustomize build` keeps it.
- Layout: shared bases `namespace/` and `crds/`, one base per variant holding `resources.yaml`
  (formatted by kustomize), single files = header comment + `kustomize build` of the base.
  `deploy/manifests/kustomization.yaml` and `ha/kustomization.yaml` point at the cluster-wide
  bases.

## Results

| # | Command (repository root unless noted) | Exit | Result |
|---|-----------------------------------------|------|--------|
| 1 | `make manifests-install` (twice), `shasum -a 256` of the 37 generated files after each | 0, 0 | second run `37 generated files, 0 written`; identical hashes for all 37 files |
| 2 | `make verify-manifests` | 0 | `deploy/manifests is up to date (37 generated files)` |
| 3 | `make verify-generated` (controller-gen, helm-docs, genmanifests, diff of the generated set incl. `deploy/manifests`) | 0 | `generated files are up to date` |
| 4 | negative: `sed -i 's/replicas: 1$/replicas: 3/' deploy/manifests/install.yaml; touch deploy/manifests/stray.yaml; make verify-manifests; make verify-generated` | 2, 2 | `stale: install.yaml (line 34800: have "  replicas: 3", want "  replicas: 1")` and `unexpected (not generated, not hand-written): stray.yaml`; verify-generated failed on the unexpected file; files restored with `make manifests-install` (`cmp` with a backup: identical) |
| 5 | `go vet ./hack/genmanifests` | 0 | |
| 6 | `golangci-lint run ./hack/genmanifests/...` | 0 | `0 issues.` |
| 7 | `go test -race -count=1 -v ./hack/genmanifests` | 0 | 9 tests pass (`ok … 22.870s`): sanitizing and refusals, YAML parsing (duplicate keys/objects), scope rules, apply order, the independent verifier (changed field, unquoted octal, missing/extra object, kept Helm label, namespace position, kind order), header wrapping, variant table, and `TestManifestsAreCurrent` (two full generations byte-identical and equal to the committed files) |
| 8 | `kustomize build <base>` vs `sed '/^#/d' <file>` for the 10 variant bases, `crds`, `.` and `ha` (in `deploy/manifests`) | 0 | all 13 equal |
| 9 | `kubectl apply --dry-run=client -f <file>` for the 11 single files (local API server for discovery) | 0 | every object accepted: crds 10, install 61, install-with-hydrator 66, namespace-install 50, namespace-install-with-hydrator 55, core-install 42, core-install-with-hydrator 47, ha/install 61, ha/install-with-hydrator 66, ha/namespace-install 50, ha/namespace-install-with-hydrator 55 |
| 10 | `kubectl apply --dry-run=server -f crds.yaml` (client-side apply) | 1 | `The CustomResourceDefinition "applicationsets.cd.patrol.example" is invalid: metadata.annotations: Too long: may not be more than 262144 bytes` (why every command uses `--server-side`) |
| 11 | `kubectl apply --server-side --force-conflicts -f crds.yaml`; `kubectl wait --for=condition=Established crd -l app.kubernetes.io/part-of=patrol-cd` | 0 | 8 CRDs, the ValidatingAdmissionPolicy and its binding applied; 8 CRDs Established |
| 12 | `kubectl apply --server-side -f namespace/namespace.yaml`, then `kubectl apply --server-side --force-conflicts --dry-run=server --validate=strict -n patrol -f <file>` for the 10 variants | 0 | every object accepted with strict field validation, **0 warnings**, with the namespace labelled Pod Security `enforce`/`warn`/`audit: restricted` |
| 13 | control: a Deployment without security context, dry run in the same namespace | 0 | `Warning: would violate PodSecurity "restricted:latest": allowPrivilegeEscalation != false …` (so the 0 warnings of #12 are meaningful) |
| 14 | `kubectl apply --server-side --force-conflicts -f install.yaml` (real), snapshot of the resourceVersions of the 61 objects, re-apply, snapshot | 0, 0 | re-apply changed nothing (identical resourceVersions) |
| 15 | apply `ha/install.yaml`, then `install.yaml`; apply `install-with-hydrator.yaml`, then `install.yaml` | 0 | patrol-server `replicas=2`, zone spread and required node anti-affinity appear, then are removed again (`replicas=1`, none); `hydrator.enabled`/`commit.server` appear, then are removed from `patrol-cmd-params-cm`; the commit-server Deployment stays (`kubectl apply` does not prune) |
| 16 | `kubectl -n patrol delete deployment,service,serviceaccount,networkpolicy,poddisruptionbudget -l app.kubernetes.io/instance=patrol,app.kubernetes.io/component=commit-server` (guide) | 0 | the 5 commit-server objects deleted |
| 17 | `kubectl apply --server-side --dry-run=server -n other -f namespace-install.yaml` | 1 | `the namespace from the provided object "patrol" does not match the namespace "other"` |
| 18 | `kubectl delete -f cluster-install/resources.yaml --ignore-not-found` (guide, keeps CRDs and namespace) | 0 | 50 objects deleted; 0 Patrol objects left in `patrol`; 8 CRDs and the namespace kept |
| 19 | overlay from the guide (`images`, `labels` with `includeSelectors: false`, strategic merge patches of `patrol-cm` and `patrol-cmd-params-cm`) built with `bin/kustomize`, server dry run | 0 | 6 image references rewritten, none left; `patrol-cmd-params-cm` keys `repo.server,reposerver.parallelism.limit` (merged, not replaced); `patrol-cm` gains `url` (10 keys); selectors unchanged; 61 objects accepted |
| 20 | overlay `namespace: patrol-prod`, server dry run, real apply of its ValidatingAdmissionPolicy | 0 | Namespace renamed, ClusterRoleBinding and RoleBinding subjects `patrol-prod`, 0 objects left in `patrol`; kustomize adds `namespace: patrol-prod` to the ValidatingAdmissionPolicy and binding; 61 objects accepted; the stored policy has no namespace |
| 21 | `kubectl kustomize <overlay>`; `kubectl apply --server-side --dry-run=server -k namespace` | 0, 0 | overlay: no warning; base applied directly with `-k`: `Warning: Sorting order is set both in 'kustomization.yaml' ('sortOptions') and in a CLI flag ('--reorder'). Using the kustomization file …` (documented, harmless) |
| 22 | independent equivalence script (below): chart render vs static file, both parsed by `kubectl create --dry-run=client -o json`, Helm metadata removed with jq | 0 | all 10 variants EQUAL for the workloads (Deployments/StatefulSet) and for every object (static Namespace excluded); `install.yaml` also EQUAL to the chart's own `ci/default.yaml` render |
| 23 | `gitleaks dir --config .gitleaks.toml <path>` for `deploy/manifests`, `hack/genmanifests`, `hack/deploy.mk`, `Makefile`, the guide, the parity overlay, `docs/verification/phase4-findings.md` and this file | 0 | `no leaks found` for each |
| 24 | `go run ./hack/parity -root <scratch copy>` then `-check` | 0, 0 | `docs/parity/status/manifests.yaml` parses and references known IDs (OPS-001..006); the repository's `docs/parity/matrix.md` was not regenerated (owned elsewhere) |

Checks 9–21 ran twice on fresh API servers (exploration, then the recorded run). The workload
hashes of #22 differ between profiles (e.g. install `95298f604c111e64`, HA `2cc7138e2bdf5fdf`),
so the comparison is sensitive to the values; the generator's own verifier is additionally
tested with deliberate differences (#7).

## Generated single files

Values re-derived on the final tree, 2026-10-18 (see the note at the top).

| File | sha256 | Objects |
|------|--------|---------|
| `install.yaml` | `b06f77a36d21b3fb6b43969050994a6fd632353dd57a915df509eedd4bb1f528` | 61: Namespace, 8 CRD, VAP, VAPB, 5 NetworkPolicy, 5 PDB, 5 ServiceAccount, 2 Secret, 7 ConfigMap, 2 ClusterRole, 2 ClusterRoleBinding, 5 Role, 5 RoleBinding, 7 Service, 4 Deployment, 1 StatefulSet |
| `install-with-hydrator.yaml` | `aa14c9ecda7cf81b611d289f4eeb4a045a3aef1e017a64889dccba22f679da52` | 66: install + commit-server Deployment, Service, ServiceAccount, NetworkPolicy, PDB |
| `namespace-install.yaml` | `dc392bd03a5a44cd1a64dc067486ec38e600619b43b9d3e8334a622c2747cc4e` | 50: no Namespace, CRD, VAP or Cluster RBAC; 7 Roles/RoleBindings |
| `namespace-install-with-hydrator.yaml` | `12bc5db4b2e07103a5543b091fe488c69f778a5cab9a0840d3a5a34a6da70507` | 55 |
| `core-install.yaml` | `969e5ad3dc58dff20c12a068d4a5bbfc1fecd3e379d4f283d8e4c93aa6fce3a8` | 42: controller, repo-server, ApplicationSet controller; 1 ClusterRole; 6 ConfigMaps, 1 Secret |
| `core-install-with-hydrator.yaml` | `838559d05c5793b1d55bd3a3b8c46e72f683ba3b05a288261a56b19c2f5e70ca` | 47 |
| `ha/install.yaml` | `09c3530ca08f336e8a92a18ddcca2e497760109f6546103d97dc2a9888c5a7d5` | 61 (replicas 2; `PATROL_CONTROLLER_REPLICAS=2`) |
| `ha/install-with-hydrator.yaml` | `a65b24ef9a50f605dd992f40d470bb67e66711a3b672f740f9bfaec5575aa8ff` | 66 |
| `ha/namespace-install.yaml` | `5d3e295c2d0ef22a6aa3ab80dcfb113a750a752b8a81ab8e8ff6e8406a261a54` | 50 |
| `ha/namespace-install-with-hydrator.yaml` | `f9cad0c411919043fa72564a191f7f9a145c29b29dce87429b404e526d6d39b7` | 55 |
| `crds.yaml` | `fa114fd946d9ec40a21a1d434f5501bb0b1fe71b487e69dfd28d98aea97823cb` | 10: 8 CRD, VAP, VAPB (unchanged) |

Sizes (final tree): 1.95–1.98 MB per file with CRDs (the CRDs are 1.9 MB of YAML, 1.4 MB of it
indentation; the ApplicationSet CRD alone 1.4 MB), 66–77 kB for namespace-scoped files;
`deploy/manifests` is 16.6 MB in total (8 copies of the CRDs: 6 cluster-wide files,
`crds.yaml`, `crds/crds.yaml`).

Superseded hashes (earlier generation, before the review fixes): install `37530add…`,
install-with-hydrator `8630ca5a…`, namespace-install `c735d0bf…`,
namespace-install-with-hydrator `4c65e5f3…`, core-install `87ca886f…`,
core-install-with-hydrator `103d643e…`, ha/install `4879804e…`, ha/install-with-hydrator
`580348b9…`, ha/namespace-install `a9b9def0…`, ha/namespace-install-with-hydrator `770374b5…`
(first 8 hex digits; the object counts were the same).

## NOT EXECUTED / BLOCKED

- Installation on a cluster with nodes (kind): BLOCKED, Docker Desktop's disk is full and no image
  is built (`TASKS.md`); no pod, probe, init container, PodDisruptionBudget, HPA or NetworkPolicy
  enforcement was exercised. The deployment validation task (`test/deploy`) covers the API-level
  checks and RBAC sufficiency with the real binaries.
- `docker build`: not part of this task (no image is built here).
- Full uninstall (`kubectl delete -f install.yaml`): NOT EXECUTED; namespace and CRD deletion
  need the namespace controller and garbage collector, which the bare API server does not run.
- HA scheduling (required anti-affinity, zone spread) on several nodes: NOT EXECUTED.
- Self-management (Patrol managing its own installation with `ServerSideApply=true`): NOT
  EXECUTED.

## Appendix: independent equivalence script (#22)

Run with `KUBECONFIG` pointing at the local API server: `equiv.sh <repository> <scratch dir>`.

```bash
#!/usr/bin/env bash
set -euo pipefail
repo=$1; out=$2
cd "$repo"
norm='def clean: walk(if type=="object" and (.metadata|type)=="object" then
    .metadata |= (
      (if .labels then .labels |= del(.["helm.sh/chart"], .["app.kubernetes.io/managed-by"]) else . end)
      | (if .annotations then .annotations |= with_entries(select((.key|startswith("helm.sh/")|not) and (.key|startswith("meta.helm.sh/")|not))) else . end)
      | (if .labels == {} then del(.labels) else . end)
      | (if .annotations == {} then del(.annotations) else . end))
  else . end);
(if .kind == "List" then .items[] else . end) | clean'
objs() { kubectl create --dry-run=client -o json -f - | jq -c "$norm"; }
status=0
while read -r file profiles; do
  args=(); for p in ${profiles//,/ }; do args+=(-f "deploy/manifests/profiles/$p"); done
  helm template patrol deploy/helm/patrol-cd -n patrol --kube-version 1.36.0 "${args[@]}" | objs | jq -cs 'sort_by(.kind, .metadata.name)[]' > "$out/chart.json"
  objs < "deploy/manifests/$file" | jq -cs 'map(select(.kind != "Namespace")) | sort_by(.kind, .metadata.name)[]' > "$out/static.json"
  wl='select(.kind=="Deployment" or .kind=="StatefulSet")'
  w1=$(jq -c "$wl" "$out/chart.json" | shasum -a 256 | cut -c1-16); w2=$(jq -c "$wl" "$out/static.json" | shasum -a 256 | cut -c1-16)
  nw=$(jq -c "$wl" "$out/static.json" | wc -l | tr -d ' ')
  n1=$(wc -l < "$out/chart.json" | tr -d ' '); n2=$(wc -l < "$out/static.json" | tr -d ' ')
  if [ "$w1" = "$w2" ] && cmp -s "$out/chart.json" "$out/static.json"; then res=EQUAL; else res=DIFFERENT; status=1; diff <(jq -S . "$out/chart.json") <(jq -S . "$out/static.json") | head -20 || true; fi
  printf '%-42s workloads=%s (%s) objects chart=%s static=%s(+Namespace if cluster-wide) %s\n' "$file" "$nw" "$w2" "$n1" "$n2" "$res"
done <<'LIST'
install.yaml install.yaml
install-with-hydrator.yaml install.yaml,hydrator.yaml
namespace-install.yaml namespace-install.yaml
namespace-install-with-hydrator.yaml namespace-install.yaml,hydrator.yaml
core-install.yaml core-install.yaml
core-install-with-hydrator.yaml core-install.yaml,hydrator.yaml
ha/install.yaml install.yaml,ha.yaml
ha/install-with-hydrator.yaml install.yaml,ha.yaml,hydrator.yaml
ha/namespace-install.yaml namespace-install.yaml,ha.yaml
ha/namespace-install-with-hydrator.yaml namespace-install.yaml,ha.yaml,hydrator.yaml
LIST
helm template patrol deploy/helm/patrol-cd -n patrol --kube-version 1.36.0 -f deploy/helm/patrol-cd/ci/default.yaml | objs | jq -cs 'sort_by(.kind, .metadata.name)[]' > "$out/ci-default.json"
objs < deploy/manifests/install.yaml | jq -cs 'map(select(.kind != "Namespace")) | sort_by(.kind, .metadata.name)[]' > "$out/static.json"
if cmp -s "$out/ci-default.json" "$out/static.json"; then echo "install.yaml == chart ci/default.yaml render (Helm metadata removed): EQUAL"; else echo "install.yaml vs ci/default.yaml: DIFFERENT"; status=1; fi
exit $status
```

Output:

```
install.yaml                               workloads=5 (95298f604c111e64) objects chart=60 static=60(+Namespace if cluster-wide) EQUAL
install-with-hydrator.yaml                 workloads=6 (f53b26107ce8c4b1) objects chart=65 static=65(+Namespace if cluster-wide) EQUAL
namespace-install.yaml                     workloads=5 (95298f604c111e64) objects chart=50 static=50(+Namespace if cluster-wide) EQUAL
namespace-install-with-hydrator.yaml       workloads=6 (f53b26107ce8c4b1) objects chart=55 static=55(+Namespace if cluster-wide) EQUAL
core-install.yaml                          workloads=3 (b9de220fc2d240c0) objects chart=41 static=41(+Namespace if cluster-wide) EQUAL
core-install-with-hydrator.yaml            workloads=4 (0527a0cb6238acef) objects chart=46 static=46(+Namespace if cluster-wide) EQUAL
ha/install.yaml                            workloads=5 (2cc7138e2bdf5fdf) objects chart=60 static=60(+Namespace if cluster-wide) EQUAL
ha/install-with-hydrator.yaml              workloads=6 (319f55e21d221da1) objects chart=65 static=65(+Namespace if cluster-wide) EQUAL
ha/namespace-install.yaml                  workloads=5 (2cc7138e2bdf5fdf) objects chart=50 static=50(+Namespace if cluster-wide) EQUAL
ha/namespace-install-with-hydrator.yaml    workloads=6 (319f55e21d221da1) objects chart=55 static=55(+Namespace if cluster-wide) EQUAL
install.yaml == chart ci/default.yaml render (Helm metadata removed): EQUAL
```

(Workload hashes repeat between install and namespace-install: the variants differ only in RBAC
and cluster-scoped objects.)
