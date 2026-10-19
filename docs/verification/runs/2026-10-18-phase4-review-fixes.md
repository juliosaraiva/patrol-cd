# Phase 4 review findings: reproduction and fixes (2026-10-18)

Environment: macOS arm64, Go 1.27.2, Helm v4.3.1, kustomize v5.8.1 (`bin/kustomize`), envtest
kube-apiserver/etcd 1.36.0 (`bin/envtest/k8s/1.36.0-darwin-arm64`), golangci-lint 2.15.0. No
Docker, kind or kubelet was used (Docker Desktop disk full; image build NOT EXECUTED).

## Findings

Each finding was first reproduced by a new regression test run against the unfixed tree
(`go test -count=1 -run '<tests>' ./test/deploy/` with `KUBEBUILDER_ASSETS`,
`PATROL_REQUIRE_ENVTEST=1`, `PATROL_REQUIRE_DEPLOY_TOOLS=1`), then fixed.

| # | Finding | Before the fix | Fix | Test |
|---|---------|----------------|-----|------|
| 1 | Helm/kubectl owned `ssh_known_hosts`, which the certificate API rewrites | `helm upgrade` → `UPGRADE FAILED: … conflict with "patrol-server" using v1: .data.ssh_known_hosts`; static re-apply with `--force-conflicts` → "reverted the known host added through the API" | chart and manifests render `patrol-ssh-known-hosts-cm` without data unless `configs.sshKnownHosts` is set; `patrolcd bootstrap` seeds `DefaultSSHKnownHosts` once (new step, `--skip-ssh-known-hosts`, Role rule get/update on that ConfigMap); TLS/GnuPG ownership documented | `test/deploy`: `TestTrustConfigMapsHelmUpgrade`, `TestTrustConfigMapsStaticReapply`; `internal/server`: `TestBootstrapSeedsKnownHosts`, `TestBootstrapKnownHostsRBAC`, `TestDefaultSSHKnownHostsFingerprints`; wiring negative control `known-hosts-never-seeded` |
| 2 | Guides said a namespace admin can apply the namespace-scoped install | as namespace `admin` (rules of `system:aggregate-to-admin/-edit/-view`): 6 Roles refused, `attempting to grant RBAC permissions not currently held`, 12 objects refused | installation.md and manifests.md corrected (cluster-admin, `cluster-admin` bound in the namespace, or `admin` + `escalate`/`bind` on Roles) | `TestNamespaceInstallPermissions` (all three personas) |
| 3 | `kubectl scale` documented for dynamic distribution; Helm owns `spec.replicas` | upgrade after `kubectl scale` → conflict `.spec.replicas`; changing `controller.replicas` also changed the pod template (env `PATROL_CONTROLLER_REPLICAS`) | docs: shards via `controller.replicas` only; with dynamic distribution the env is no longer rendered, so a replica change does not restart the shards | `TestControllerReplicasHelm` |
| 4 | No NetworkPolicy egress to the OTLP collector | 0 egress rules to 4317 (or the configured port) for server, controller, repo-server | `patrol.np.tracing` rule from the effective `otlp.address` (port from the address, 4317 default, or `tracing.networkPolicy.port`; peers `tracing.networkPolicy.to`) | `TestTracingEgress` |
| 5 | Self-service notifications: get/list/watch on every ConfigMap and Secret of Application namespaces | SAR: `list secrets` in an app namespace allowed=true; `get secrets/db-credentials` allowed=true | rules restricted by `resourceNames: [patrol-notifications-cm]` / `[patrol-notifications-secret]` | `TestRBACSufficiency/notifications-selfservice` (no 403 in the audit log with cluster-wide field-selected informers; SARs as documented) |

## Validation after the fixes

| Command (repository root) | Exit | Result |
|---------------------------|------|--------|
| `KUBEBUILDER_ASSETS=$PWD/bin/envtest/k8s/1.36.0-darwin-arm64 PATROL_REQUIRE_ENVTEST=1 PATROL_REQUIRE_DEPLOY_TOOLS=1 go test -race -count=1 -timeout 40m -v ./test/deploy/` | 0 | `ok … 277.152s`; 21 top-level tests and 152 subtests PASS, 0 FAIL, 0 SKIP, no data race |
| `KUBEBUILDER_ASSETS=… go test -race -count=1 ./internal/server/... ./cmd/patrolcd/...` | 0 | `ok internal/server 35.922s`, `ok cmd/patrolcd 18.251s` |
| `go test -count=1 ./hack/genmanifests/ ./hack/helmdocs/` | 0 | ok, ok |
| `make helm-lint` | 0 | defaults and 10 profiles `--strict`; 23/23 invalid value files refused as expected |
| `make helm-template` | 0 | 10 profiles rendered twice, identical |
| `make verify-manifests` | 0 | `deploy/manifests is up to date (37 generated files)` |
| `make verify-generated` | 0 | `generated files are up to date`; `git status` unchanged |
| `go vet ./internal/server/... ./cmd/patrolcd/... ./test/deploy/... ./hack/...` | 0 | |
| `golangci-lint run ./internal/server/... ./cmd/patrolcd/... ./test/deploy/... ./hack/genmanifests/...` | 1 | 0 issues in the files changed here; 2 `errcheck` issues in `internal/server/core_support.go:262,266` (unmodified file, last commit 584248d, outside this change) |

Observation recorded by `TestTrustConfigMapsHelmUpgrade`: `helm upgrade -f ci/default.yaml`
(an empty `{}` file) counts as "no values" and reuses the previous release's values; the
switch back to API-managed known hosts therefore uses `--reset-values` (documented in
installation.md, "Trust material").

## Addendum 2026-10-18 (correction, finding F8 of the second review round)

The last row of "Validation after the fixes" calls `internal/server/core_support.go`
"unmodified". That described the file at the time of that run only. Afterwards, outside the
five fixes above, documented `//nolint:errcheck` directives were added at 6 sites in 5 files,
and nothing else in those files changed (`git diff --numstat`: 6 lines changed, each only
gaining the comment):

| File | Line(s) | Site |
|------|---------|------|
| `internal/server/core_support.go` | 262, 266 | `<-errc` after the informers stopped (timeout / caller cancellation wins) |
| `internal/apiserver/rest/sse.go` | 174 | `case <-done:` after the stream ended |
| `internal/apiserver/services/applications/terminal.go` | 423 | `case <-done:` while the session closes |
| `internal/cmp/server.go` | 348 | `<-errCh` (Serve returns `http.ErrServerClosed` after Shutdown) |
| `pkg/scm/pagination_test.go` | 89 | `for range scm.Pages(...)` that breaks after the first page |

Reason: golangci-lint 2.15.0 (installed 2026-10-17) reports unchecked error-channel receives
and ignored range-over-func errors that the earlier 2.x release did not report. Re-run on the
final tree (second review round, 2026-10-18): `golangci-lint run ./...` exit 0, `0 issues.`
Details: [`2026-10-18-phase4-review-round2.md`](2026-10-18-phase4-review-round2.md).
