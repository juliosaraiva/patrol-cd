# Phase 4 second review round: findings, fixes and verification (2026-10-18)

Environment: macOS 27.0.1 arm64, Go 1.27.2, Helm v4.3.1+gc0d08d0, kustomize v5.8.1
(`bin/kustomize`), yq v4, envtest kube-apiserver/etcd 1.36.0
(`bin/envtest/k8s/1.36.0-darwin-arm64`), golangci-lint 2.15.0. No Docker, kind, kubelet,
scheduler, controller-manager or CNI was used (Docker Desktop disk full; image build NOT
EXECUTED). Scratch files (mutation overlays, logs) lived in the session scratchpad and are not
part of the repository.

## Review

- Independent, read-only review workflow `wf_21493a28-fb7` (17 agents) over five dimensions:
  image, chart, manifests, bootstrap, honesty of the records. A skeptic pass verified each
  reported finding: **12 confirmed** (verified severity important: IMG-1, CH-4, MAN-1, F1;
  minor: CH-1, CH-2, CH-3, CH-5, BS-1, F2, F3, F4), **0 refuted**, **25 minor findings not
  independently verified** (IMG-2..4, CH-6..12, MAN-2..4, BS-2..5, F5..F12).
- Fix workflow: four agents with disjoint file ownership (image; chart and manifests; bootstrap;
  test harness and verification records). The minor findings were reproduced before they were
  fixed. The agents did not commit; the orchestrator commits after verification.

## Status per finding

"Before → after" names the check that failed before the fix and passes after it; details are in
the agents' reports and, for the bootstrap, in
[`2026-10-18-bootstrap-dv6.md`](2026-10-18-bootstrap-dv6.md).

| ID | Area | Status | Before → after |
|----|------|--------|----------------|
| IMG-1, F1 | image | fixed | the image now ships the GPL v2 text and a `BUILD` note for connect-proxy (`/usr/share/licenses/connect-proxy/{COPYING,BUILD,connect.c}`); NOTICE claims only what ships. `rootfs-check.sh` asserts both files and that `BUILD` matches the Dockerfile's gcc line; 4 mutations fail, unmodified passes. `make image-rootfs-check` re-run here: exit 0, 52 layout entries and 100 PASS lines per arch |
| IMG-2 | image | fixed | OCI `licenses` label is a valid SPDX expression (`Apache-2.0 AND MIT AND GPL-2.0-or-later AND LicenseRef-Bundled-Dependencies`); parser check exit 0, negative control exit 1 |
| IMG-3 | image | fixed (reproduced) | `make -n image` with an untracked file in the build context: `GIT_TREE_STATE=clean` → `dirty`, version gains `-dirty` |
| IMG-4 | image | fixed (reproduced) | a dangling link printed FAIL and then a contradictory PASS → FAIL only |
| F10 | image docs | fixed | REPO-023 note says compile + `nm` symbol check only; no link was run |
| CH-1 | chart | fixed | `patrol.suffixedName` keeps the metrics Service names within 63 characters; `TestServiceNamesAreDNSLabels` FAIL → PASS; envtest dry run rejected the old 64-character name |
| CH-2, CH-9 | chart | fixed | NetworkPolicy `except` entries only under CIDRs that strictly contain them; new `networkPolicy.excludeMetadataEndpoints` (default 169.254.169.254/32, fd00:ec2::254/128) for repo-server and commit-server egress; `TestNetworkPolicyExcept` FAIL → PASS; fuzz cross-check against Python `ipaddress`: 0 mismatches |
| CH-3 | chart | fixed | hardened controller writes named per API group (no `*`); docs state what remains possible. Harness: `TestRBACSufficiency/hardened` has 10 more SubjectAccessReviews (run here, below) |
| CH-4 | chart | fixed | `serviceAccount.create=false` without a name fails the render for every component (the `default` fallback is gone); `TestServiceAccountNameRequired` FAIL → PASS |
| CH-5 | docs | fixed | installation.md security table states the notifications controller's `list`/`watch` on every ConfigMap and Secret of the namespace (sibling text in `docs/architecture/overview.md`: open, R2-1) |
| CH-6 | chart | fixed (reproduced) | self-service notification RBAC follows the binary's flag > env > ConfigMap precedence and case-insensitive `true`; `TestNotificationsSelfServiceRBAC` FAIL → PASS |
| CH-7 | chart | fixed | PDB `maxUnavailable: 0` rendered as 0 (was 1); `TestPDBMaxUnavailable` |
| CH-8 | chart | fixed | HPA without metrics or with `minReplicas > maxReplicas` refused (two new `ci/invalid` files) |
| CH-10 | docs | docs only | uninstall deletes the trust ConfigMaps; export/re-add recipe documented; keeping them is a decision (R2-7) |
| CH-11, CH-12 | docs | fixed | Dex runs as 1001; `controller.podManagementPolicy` is install-time only (StatefulSet update Forbidden in envtest) |
| MAN-1 | docs | fixed | namespace-scoped installs need a `Cluster` object with `namespaces: [patrol]` before local Applications; the harness's namespaced run already creates it (`test/deploy/rbac_test.go`) |
| MAN-2, F3 | records | fixed (this agent) | hash tables re-derived on the final tree (below) |
| MAN-3 | docs | fixed | documented overlays set `sortOptions: {order: fifo}`; order equal to install.yaml (61 objects) |
| MAN-4 | generator | fixed | core variants layered over `install.yaml`; the harness's variant table now matches (below) |
| BS-1 | bootstrap | fixed | DV-6 implemented (stale initial admin Secret replaced), the RBAC rationale corrected; the clock-free UID variant was not adopted (R2-6) |
| BS-2 | chart NOTES | fixed | NOTES branch on admin disabled / hash set / `createAdmin=false` / `bootstrap.enabled=false` (`helm install --dry-run=client` before/after) |
| BS-3 | bootstrap | fixed (reproduced) | the command's client has no client-side rate limiter; `TestBootstrapClientNotThrottled` FAIL → PASS |
| BS-4, F6, BS-5, F11 | bootstrap docs | fixed | stale "manifests are future work" statements removed; RBAC contract described precisely; unrecorded measurements replaced by a recorded run |
| F2 | chart | fixed | `ci/all-features.yaml` renders an Ingress with an HTTPS backend; `TestIngressBackendPort` FAIL → PASS |
| F4 | records | fixed (this agent) | `log.md` entries V-0018 … V-0027 |
| F5, F9 | docs | fixed | token mount optional only on the bootstrap pod; `createAdmin: false` only skips the password |
| F7 | harness | fixed (this agent) | no-op upgrade compares every rendered object (below) |
| F8 | records | fixed (this agent) | dated addendum in [`2026-10-18-phase4-review-fixes.md`](2026-10-18-phase4-review-fixes.md) |
| F12 | records | fixed (this agent) | M-1 marked resolved in `phase4-findings.md` |

Not fixed, with the reason: CH-1 for ConfigMap/Secret names (limit 253, truncation could make
plugin ConfigMap names collide); CH-10 `resource-policy: keep` (decision, R2-7); IMG-1 side
claims (SBOM content and Alpine license fields need the image, R2-8); BS-1 clock-free variant
(R2-6); the `patrolcd bootstrap` help text (R2-2); overview.md (R2-1). Open follow-ups:
[`../phase4-findings.md`](../phase4-findings.md), section "From the second review round".

## Harness and record changes by this agent

| # | Command (repository root) | Exit | Result |
|---|---------------------------|------|--------|
| 1 | `KUBEBUILDER_ASSETS="$(make --no-print-directory -s envtest)" PATROL_REQUIRE_ENVTEST=1 go test -race -count=1 -run '^TestHelmLifecycle$' -v ./test/deploy/` with the **unmodified** test on the final tree | 1 | `re-install: patrol-initial-admin-secret/password changed` (the old step 8 asserted the pre-fix reuse), 45.10 s |
| 2 | the same with the new test and `GOFLAGS=-overlay=<scratch>` replacing `internal/server/bootstrap_secrets.go` by a copy whose `initialSecretStale` returns false (pre-fix behavior; the overlay also reaches the `go build ./cmd/patrolcd` the harness runs) | 1 | 3 failures, as intended: `…/password was not replaced (the stale initial admin password was reused, DV-6)`, `… still has the first install's UID … (not re-created)`, `stale initial password: login error <nil>, want invalid username or password`; 52.47 s |
| 3 | negative control of the widened no-op check: scratch overlay of the test whose step-2 upgrade adds `--set-json networkPolicy.excludeMetadataEndpoints=[]` (changes only the repo-server NetworkPolicy) | 1 | previous four-kind snapshot: `found 0 modified objects`; new check: `no-op upgrade modified NetworkPolicy/lifecycle/patrol-repo-server (resourceVersion 342 → resourceVersion 409)`, `59/60 rendered objects unchanged`; 55.30 s |
| 4 | #1 with the new test on the final tree | 0 | PASS 51.96 s: `no-op upgrade: 60/60 rendered objects unchanged`; `after uninstall + re-install: internal token kept; new initial admin password (new Secret UID) logs in; stale initial and changed passwords rejected; server.secretkey regenerated=true` |
| 5 | scratch test added by overlay: a CRD with an extra finalizer and one instance with a finalizer, deleted, then `waitDeleted` with a 3 s timeout | 1 (intended) | the failure reads: `timed out after 3s waiting for CustomResourceDefinition//widgets.zz.example deleted (last read error: none): deletionTimestamp … (7s ago), finalizers [zz.example/hold customresourcecleanup.apiextensions.k8s.io], resourceVersion 211, condition NamesAccepted=True …, condition Established=True …, condition Terminating=True (reason InstanceDeletionInProgress: CustomResource deletion is in progress; …), 1 instances of … left` |
| 6 | `KUBEBUILDER_ASSETS=… PATROL_REQUIRE_ENVTEST=1 PATROL_REQUIRE_DEPLOY_TOOLS=1 go test -race -count=1 -run '^(TestStaticManifestsApply\|TestStaticManifestsMatchChart)$' -v ./test/deploy/` | 0 | `install.yaml: 61 objects applied, re-applied with 0 changes`; `60 objects gone, 1 terminating`; `563 objects validated by strict dry run across 11 files`; 10 variants and `crds.yaml` equal to the chart (core variants with `[install.yaml core-install.yaml]`), kustomize bases identical; 28.61 s |
| 7 | `KUBEBUILDER_ASSETS=… PATROL_REQUIRE_ENVTEST=1 PATROL_REQUIRE_DEPLOY_TOOLS=1 go test -race -count=1 -run '^TestRBACSufficiency$/^hardened$' -v ./test/deploy/` | 0 | 14 SubjectAccessReviews as documented (new: no `clusterrolebindings`, `serviceaccounts/token`, `pods/exec`, `pods/ephemeralcontainers`, `nodes/proxy` create, node and namespace updates, `validatingwebhookconfigurations`; still allowed: `create secrets` in `kube-system`, `get nodes/proxy`); 435 audited requests by 4 service accounts, no denied request; 31.50 s |
| 8 | `go vet ./test/deploy/`; `gofmt -l test/deploy`; `golangci-lint run ./test/deploy/...`; `golangci-lint run ./...` | 0, –, 0, 0 | vet clean; gofmt no output; `0 issues.` for the package and for the whole tree |
| 9 | `make verify-manifests`; `shasum -a 256` and `yq` kind counts of the 11 single files | 0 | `deploy/manifests is up to date (37 generated files)`; table in [`2026-10-17-manifests-static.md`](2026-10-17-manifests-static.md) |
| 10 | `make helm-template` twice + `cmp` of `SHA256SUMS`; each profile rendered directly with the record's command | 0, 0 | identical; 10/10 direct hashes equal `SHA256SUMS`; table in [`2026-10-17-chart-static.md`](2026-10-17-chart-static.md) |
| 11 | `make helm-package` twice; `helm lint --strict dist/patrol-cd-0.1.0.tgz --kube-version 1.36.0`; packaged default render hash | 0, 0, 0 | `2fea06a5…` both times, 181,769 bytes; lints; default render `f78082dd…` equals the directory chart's |
| 12 | `make helm-lint` | 0 | defaults and 10 profiles `--strict`; 28/28 `ci/invalid` files refused with the expected message |
| 13 | `make image-rootfs-check` | 0 | `rootfs-check: OK (linux/amd64)`, `OK (linux/arm64)`, 52 layout entries per arch, 200 PASS lines, 0 FAIL, 23 s |
| 14 | the reviewer's reproduction loops for F3 (manifest and render hashes vs the records) | 0 | after the update: 11/11 manifest hashes and 10/10 render hashes MATCH (the reviewer's exact `grep` patterns); against the records as they were before: 10 DIFF + 1 MATCH (`crds.yaml`) and 10/10 DIFF |
| 15 | `go run ./hack/parity`; `go run ./hack/parity -check` | 0, 0 | matrix regenerated with the round-2 status notes; check passes |

Changes in `test/deploy`:

- `helmlifecycle_test.go`: step 2 compares every rendered object (resourceVersion; generation,
  labels and annotations for CRDs and the ValidatingAdmissionPolicy, whose status the API
  server writes). Step 8 asserts the DV-6 fix: token kept, initial admin Secret replaced (new
  password digest and UID), the new password logs in through `internal/auth` (`adminLogin`,
  the server's login path against the live `patrol-cm`/`patrol-secret`), the first initial
  password and the password changed before the uninstall are rejected. Positive and negative
  controls before the uninstall: the changed password logs in and the first initial password
  no longer does. `setAdminPassword` uses `auth.SecretWithPassword`, as `patrol account
  update-password` does. Passwords are compared in memory and never logged.
- `manifests_test.go`: `waitDeleted`/`deletionFacts` report deletionTimestamp, finalizers,
  resourceVersion and, for a CRD, its conditions and remaining instances; the CRD deletion wait
  is 5 minutes (other objects 2 minutes) because of the load flake below. The core variants
  use the generator's profile lists (MAN-4).
- `rbac_test.go`: `sarCheck.subresource`; the hardened SubjectAccessReviews above.

## Deploy suite history (full runs of `./test/deploy/`)

| Run | Who | Result |
|-----|-----|--------|
| first deploy validation run (2026-10-18) | deployment validation agent | passed: 16 top-level tests, 142 subtests ([record](2026-10-17-deploy-validation.md), #1) |
| after the first review fixes | review-fix agents | passed: 21 top-level tests, 152 subtests, 277 s ([record](2026-10-18-phase4-review-fixes.md)) |
| closeout run 1 | orchestrator | **failed** `TestStaticManifestsMatchChart`: the orchestrator ran `make verify-generated` at the same time, which rewrites `deploy/crds` and `deploy/manifests` in place while the test read them (not a product defect) |
| closeout run 2 (nothing else running) | orchestrator | **failed** `TestStaticManifestsApply`: `timed out after 2m0s waiting for CustomResourceDefinition//appprojects.cd.patrol.example deleted (last: still present)`; 19 other tests passed. `TestStaticManifestsApply` then passed 3/3 in isolation (21–25 s each). Root cause not determined: that failure message did not carry the CRD's state, which is what the change above adds |
| final run after the second fix round (2026-10-18, nothing else running) | orchestrator | **passed**: 21 top-level tests, 157 subtests, 0 failed, 0 skipped, 281 s (`go test -race -count=1 -v ./test/deploy/` with `PATROL_REQUIRE_ENVTEST=1`). The CRD-deletion timeout of closeout run 2 did not recur; its cause is still undetermined |

The closeout runs were executed and reported by the orchestrator; this agent did not re-run
the full suite (by rule) and their logs are not in the repository.

## NOT EXECUTED

- `docker build` / `make image`, `make image-sbom`, anything inside the image: Docker Desktop
  disk full.
- kind, kubelet, NetworkPolicy enforcement, scheduling, Kubernetes versions other than 1.36.0.
- The full `./test/deploy/` suite by this agent (the orchestrator runs it once all agents are
  done; the orchestrator's run is the last row of the table above).
