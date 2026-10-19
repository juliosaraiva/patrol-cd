# Helm chart: static verification (2026-10-18)

Chart `deploy/helm/patrol-cd` 0.1.0 (appVersion 0.1.0), generator `hack/helmdocs`, targets
`hack/deploy.mk`, guide `docs/guides/installation.md`, parity overlay
`docs/parity/status/chart.yaml`. (File name kept from the task assignment; the runs below were
executed on 2026-10-18.)

Environment: macOS 27.0 arm64, Go 1.27.2, Helm v4.3.1+gc0d08d0 (the baseline pins Helm 4.2.1
inside the image; the host Helm is only used to lint and render), golangci-lint from `~/go/bin`.
No Kubernetes API server, kubelet or CNI was used by these checks.

> **Values re-derived on the final tree, 2026-10-18.** The chart changed after these runs (the
> first review fixes in [`2026-10-18-phase4-review-fixes.md`](2026-10-18-phase4-review-fixes.md)
> and the second review round in
> [`2026-10-18-phase4-review-round2.md`](2026-10-18-phase4-review-round2.md)). The render
> hashes, the packaged chart's hash and size, the default render hash (#6–#8) and the
> negative value file count (#5) below were recomputed on the final tree with the same
> commands: `make helm-template` twice with `cmp` of the two `SHA256SUMS` (exit 0, 0,
> identical), each profile also rendered directly with `helm template patrol
> deploy/helm/patrol-cd -n patrol --kube-version 1.36.0 -f ci/<profile>.yaml | shasum -a 256`
> (10/10 equal to `SHA256SUMS`), `make helm-package` twice (exit 0, 0, identical hashes),
> `make helm-lint` (exit 0). The values recorded on the earlier tree are listed under
> "Superseded values". Rows #1–#4 and #9–#11 and the external data checks are the original
> runs on the earlier tree and were not repeated for this note; the final tree's chart tests
> are listed in the round-2 record.

## Results

| # | Command (repository root) | Exit | Result |
|---|---------------------------|------|--------|
| 1 | `go run ./hack/helmdocs -check` | 0 | README.md, `templates/_generated_params.tpl`, the schema key sets and `templates/crds/*` are current; shipped `patrol-cm` defaults equal `internal/settings` |
| 2 | `go test -count=1 -race ./hack/helmdocs/` | 0 | `ok … 7.370s` (rerun after the final edit; unit tests, chart currency, determinism, and `TestRenderedProfiles`: 10 profiles rendered twice by `helm template`, identical, strict typed decoding, pod invariants, flags vs help goldens) |
| 3 | `go vet ./hack/helmdocs/` | 0 | |
| 4 | `golangci-lint run ./hack/helmdocs/` | 0 | `0 issues.` |
| 5 | `make helm-lint` | 0 | `helm lint --strict --kube-version 1.36.0` of the defaults and of `ci/{airgap,all-features,core,default,ha,hardened,hydrator,ingress-nginx,minimal,namespaced}.yaml`: `1 chart(s) linted, 0 chart(s) failed` each (only `[INFO] Chart.yaml: icon is recommended`); re-derived on the final tree: 28/28 negative value files refused with the expected error (22 at the original run) |
| 6 | `make helm-template` (twice) and `cmp` of the two `SHA256SUMS` | 0, 0, 0 | each profile rendered twice per run and compared; the two runs produced identical hashes (below) |
| 7 | `make helm-package` (twice, after the final template edit) | 0, 0 | re-derived on the final tree: `dist/patrol-cd-0.1.0.tgz` (181,769 bytes); both builds sha256 `2fea06a595e7eb6800f5f898f1ba559a06b875a91fe9d028fa007aebffc2da9f`; excludes `ci/` and `README.md.gotmpl` (`tar -tzf` lists neither) |
| 8 | `helm lint --strict dist/patrol-cd-0.1.0.tgz --kube-version 1.36.0`; `helm template patrol dist/patrol-cd-0.1.0.tgz -n patrol --kube-version 1.36.0 \| sha256sum` | 0 | packaged chart lints; re-derived on the final tree: its default render hash equals the directory chart's (`f78082dd…`, the `default.yaml` line below) |
| 9 | `helm template patrol deploy/helm/patrol-cd -n patrol --kube-version 1.29.9` / `1.30.0` / `1.33.0 -f ci/ha.yaml` | 1 / 0 / 0 | `chart requires kubeVersion: >=1.30.0-0 which is incompatible with Kubernetes v1.29.9`; 1.30 and 1.33 render |
| 10 | `go run ./hack/parity -root <copy of docs/>` then `-check` (on a scratch copy, not the repository matrix) | 0, 0 | `docs/parity/status/chart.yaml` parses and references known IDs; the repository's `docs/parity/matrix.md` was not regenerated (owned elsewhere) |
| 11 | `gitleaks dir --config .gitleaks.toml <path>` for `deploy/helm`, `hack/helmdocs`, `hack/deploy.mk`, the guide, this file and `docs/parity/status/chart.yaml` | 0 | `no leaks found` for each |

Render hashes (`dist/helm-template/SHA256SUMS`, release `patrol`, namespace `patrol`,
`--kube-version 1.36.0`; values re-derived on the final tree, 2026-10-18):

```
574c1b124629e25a0a1d65422f44052564f0b1e0f7285df1ee6305479071d504  airgap.yaml
b432e507012e73197f2f1066527015ec2bed800ef1278bc99a5f4f61658f0a2d  all-features.yaml
d9561814396f469a99b2a3d640d332d6ebd32a0896b1f3d231783a59a920b499  core.yaml
f78082dd38a56f62fb8ccd90ba941f4efe46bc13dffcd5f12a804e3d51a4fa84  default.yaml
77af9f701024248944e06d2c7a79e864f266f75d6a27f06706d0a90a4619116d  ha.yaml
35e609ad926f214def89d2c5ad03828ca2c0b2216d64bb4ccdf5c4b9f557a5aa  hardened.yaml
a89d68e3a4cd895b53c17877d32ab4f5955a08cb5adbf2864bd78b1582d07b89  hydrator.yaml
8fa9694a03d4be4a100b8c1fe2315aad18f5b4cbaa2602f93f73e3f3a1b77188  ingress-nginx.yaml
a05caf6b9bb69ce5013e31cb104d3f0ef2005f6a178d2de0e8d4a79fd9bea81c  minimal.yaml
ff69e7a6bf74e6be806be88293baf374895c8fbb24e514ac7d14d2596c5f4ed0  namespaced.yaml
```

Rendered objects per profile (`yq -N 'select(.kind != null) | .kind'` over each render): airgap
66, all-features 97, core 50, default 60, ha 80, hardened 58, hydrator 65, ingress-nginx 61,
minimal 43, namespaced 54; 634 in total (the deployment validation counted 633 before
`ci/all-features.yaml` gained an Ingress).

Superseded values (earlier tree, before the review fixes): render hashes airgap `b23085d3…`,
all-features `8aa1cdbd…`, core `e6bbaadf…`, default `a7a08709…`, ha `7eacf8b8…`, hardened
`164c102e…`, hydrator `8eb6684f…`, ingress-nginx `52ae0bd6…`, minimal `870924c6…`, namespaced
`8d5e8043…`; packaged chart 177 kB, sha256 `117ef251fa38de00eccac4de2e1aab0c2ffe13228d3586d0be12a8cb6119e6d7`.

Negative value tests (`ci/invalid/*.yaml`, each with an `# expect:` line checked by
`make helm-lint`): unknown top-level, component, `configs.cm`, `configs.rbac` and
`configs.params` keys; a parameter without effect (`redis.server`); a managed parameter
(`hydrator.enabled`); `otlp.headers` in the ConfigMap; invalid `internalTLS.mode`; malformed
image digest; grace period not above the drain bound; cert-manager mode without
`cert-manager.io/v1`; mTLS without Secret-backed certificates; a plaintext admin password;
`server.secretkey` in `configs.secret.extra`; Dex without the client secret reference; Dex
without patrol-server; namespaced RBAC with application namespaces; a CMP plugin without
configuration; ServiceMonitor without the Prometheus Operator API; zero controller replicas; a
malformed GPG key ID. Added later (28 files on the final tree): `applicationSetNamespaces`
without an SCM allow-list (DV-1), a ServiceAccount with `create: false` and no name, an HPA
without metrics, an HPA with `minReplicas` above `maxReplicas`, a malformed IPv4 and a
malformed IPv6 NetworkPolicy CIDR (second review round).

Negative controls of the checks themselves: `TestCheckFlagsRejectsUnknownFlag` and
`TestCheckPodSpecFindsViolations` (unit); a temporary profile adding `server.extraArgs:
[--bogus-flag]` and `repoServer.containerSecurityContext.readOnlyRootFilesystem: false` made
`TestRenderedProfiles/zz-negative-control.yaml` fail with `patrolcd server has no flag
--bogus-flag` and `repo-server: root filesystem is writable` (file removed afterwards).

## Data checked against external sources

| Data | How | Result |
|------|-----|--------|
| Dex image `ghcr.io/dexidp/dex:v2.45.1` digest | anonymous registry token, `GET /v2/dexidp/dex/manifests/v2.45.1` (OCI index) saved to a scratch directory, `shasum -a 256` of the body | `sha256:8499afd690c437f52301efd2b05b2455da5bd2dfc20332cd697dc9937f808462`, equal to the `docker-content-digest` header; index has linux/amd64, arm64, arm, ppc64le, s390x |
| Default `ssh_known_hosts` | GitHub: `api.github.com/meta` `ssh_keys`; Bitbucket: `bitbucket.org/site/ssh`; GitLab and Azure DevOps: `ssh-keygen -lf` fingerprints compared with docs.gitlab.com/user/gitlab_com/ and learn.microsoft.com (Azure DevOps SSH keys page) | all keys match the providers' published keys/fingerprints |
| Tested Kubernetes versions | `docs/operator-manual/tested-kubernetes-versions.md` at v3.5.5 (raw.githubusercontent.com) | 3.5: v1.36, v1.35, v1.34, v1.33 |
| Dex client secret derivation in the guide | temporary Go program calling `oidc.DexClientSecret` on a test key vs the documented `openssl dgst -sha256 -mac HMAC -macopt hexkey:…` pipeline | identical output (`E4INC6Vl7QkvYDt4AsHAOP80fnUolaHVk8EoUoin` for the test key) |

Downloads were stored in new scratch directories and only read as data.

## Not executed / blocked

- **`helm install`/`upgrade`/`uninstall` against an API server, server-side dry run, Pod Security
  admission, RBAC sufficiency**: NOT EXECUTED here; they belong to the deployment validation
  task (envtest), which runs after this one.
- **Pods on a cluster** (probes, bootstrap init container, internal token mount retry, CMP
  sidecars, Dex): BLOCKED, no image build and no kind cluster (Docker Desktop VM disk full,
  `TASKS.md`).
- **NetworkPolicy enforcement**: NOT EXECUTED (needs a CNI).
- **cert-manager issuance, ServiceMonitor scraping, PrometheusRule evaluation, HTTPRoute and
  Ingress routing**: NOT EXECUTED (rendered only, with `capabilities.extraAPIVersions` standing
  in for the CRDs).
- `docker build`: not part of this task (image task, NOT EXECUTED there).

## Static properties asserted by `TestRenderedProfiles`

For every Deployment and StatefulSet of every profile: explicit
`automountServiceAccountToken`; no host namespaces or hostPath; pod seccomp `RuntimeDefault`;
every container (init containers and CMP sidecars included) with `allowPrivilegeEscalation:
false`, capabilities exactly `drop: [ALL]`, non-root, uid ≠ 0, read-only root filesystem, CPU
and memory requests and a memory limit; readiness and liveness probes on long-running Patrol
containers; every `emptyDir` size-limited; no duplicate env names or volumes; every mount
backed by a volume; every flag passed to `patrolcd <subcommand>` present in
`cmd/patrolcd/testdata/help/<subcommand>.golden` (bootstrap included).
