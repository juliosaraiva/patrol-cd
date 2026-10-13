# Verification log

Every executed check is recorded here: ID, date, environment, exact command, exit status,
result summary, artifact path. Status values: `passed`, `failed`, `skipped`, `blocked`,
`not executed`. Mock/fake-backed checks are labeled `simulated`.

## Environment (host)

| Item | Value |
|------|-------|
| Host | macOS 27.0.1 (arm64), 10 CPU, 32 GiB RAM |
| Go | go1.27.2 darwin/arm64 |
| Node / pnpm | v22.22.3 / 10.33.0 |
| Docker | Docker Desktop 29.8.2 (8 CPU, 11.7 GiB to VM) |
| kind / kubectl / helm | v0.33.0 / v1.36.1 / v4.3.0 (local; image pins Helm 4.2.1) |
| envtest | kube-apiserver/etcd 1.36.0 (`bin/envtest/k8s/1.36.0-darwin-arm64`) |
| controller-gen / kustomize | v0.20.0 / v5.8.1 (`tools/go.mod`) |

## Entries

| ID | Date | Command | Status | Notes |
|----|------|---------|--------|-------|
| V-0001 | 2026-10-12 | `gh release list -R argoproj/argo-cd --limit 15`; `gh api repos/argoproj/argo-cd/git/ref/tags/v3.5.5` | passed | Baseline v3.5.5 → `a5038b0e2bbac1715754380ae0084df8e3d5b48c` |
| V-0002 | 2026-10-12 | `git clone --depth 1 --branch v3.5.5 https://github.com/argoproj/argo-cd.git` | passed | `git rev-parse HEAD` = `a5038b0e…`; `VERSION` = 3.5.5 |
| V-0003 | 2026-10-12 | `go mod tidy`; `go install tool` (tools module); `setup-envtest use 1.36.0` | passed | deps pinned via `hack/deps/deps.go` |
