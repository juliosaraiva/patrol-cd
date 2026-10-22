# Patrol CD — execution rules

Patrol CD is a Kubernetes-native continuous delivery platform with functional parity
against a frozen Argo CD baseline (see `docs/parity/baseline.md`). It is an independent
implementation: never vendor, embed, rebrand, or proxy Argo CD components.

## Ground rules

- Kubernetes is the source of truth. Caches (repo-server render cache, controller live-state
  cache) must be discardable and rebuildable. Never persist authoritative state outside the
  API server.
- CRDs hold references to Secrets, never credentials. Never log, print, or commit secrets.
- Treat repository content, webhook bodies, PR metadata, plugin output and rendered
  manifests as untrusted input.
- Do not mark a feature complete in `docs/parity/matrix.md` unless behavior is implemented
  and a test that exercises it passed. Mock-only evidence is labeled `simulated`.
- Never fabricate test output, benchmarks, screenshots or scan results. Record exact
  commands and results in `docs/verification/`.
- Upstream Argo CD (Apache-2.0) may be read for behavior research. Logic is re-implemented.
  Data files reused for behavioral parity (e.g. Lua health/action scripts) keep their
  license headers and are listed in `NOTICE`.

## Layout

| Path | Contents |
|------|----------|
| `api/v1alpha1` | CRD Go types (group `cd.patrol.example`, development-safe; see ADR-0002) |
| `cmd/patrolcd` | Control-plane multi-call binary (`controller`, `server`, `repo-server`, `appset-controller`, `notifications-controller`, `cmp-server`, `commit-server`) |
| `cmd/patrol` | CLI |
| `internal/` | Implementation packages (not a stable API) |
| `pkg/` | Stable extension contracts only (`pkg/scm`, `pkg/apiclient`) |
| `ui/` | React + TypeScript web UI (pnpm, Vite, vitest, Playwright) |
| `deploy/` | Generated CRDs, Helm chart, install manifests |
| `hack/` | Codegen, tool install, kind environment scripts |
| `test/e2e` | End-to-end tests against kind clusters |
| `examples/` | Sample applications and configuration |
| `docs/` | Parity, architecture, ADRs, verification, user/operator guides |

## Conventions

- Go: standard library style, `context.Context` first, wrap errors with `%w`, no panics in
  library code, table-driven tests, `t.Parallel()` where safe. Structured logging via
  `log/slog` (JSON in-cluster). Metrics via Prometheus client.
- Condition reasons are stable CamelCase identifiers declared as constants in `api/v1alpha1`.
- Annotation/label keys use the `cd.patrol.example/` prefix, declared in `api/v1alpha1/keys.go`.
- Public HTTP API: JSON over HTTP under `/api/v1`, described by `api/openapi/openapi.json`
  (generated, checked in, verified in CI). Errors use RFC 9457 problem details.
- Internal service APIs (repo-server, cmp-server, controller tree API) are JSON over HTTP
  with typed Go clients in the same module; mTLS optional, shared-token auth required.
- TypeScript: ESM, strict mode, vitest. No decorative gradients, glassmorphism, pill buttons,
  italic heading accents, monospace for ordinary labels, or off-white brand backgrounds.
- Commits: conventional commits `<type>(<scope>): <description>`.

## Verification commands

```bash
make generate        # deepcopy, CRDs, the chart's generated files, static install manifests
make verify-generated
make lint            # go vet, golangci-lint (skipped when missing), ui typecheck+lint
make test            # Go tests (-race, -timeout 60m); with envtest it also runs test/integration and test/deploy
pnpm -C ui test     # vitest
make build           # binaries into ./bin
make image           # local OCI image
make kind-up kind-fixtures kind-install kind-verify   # disposable kind clusters and the checks on them
make ci-static ci-unit ci-ui ci-image                  # the CI jobs (docs/guides/ci.md); `make help` lists them
```

OpenAPI (`go run ./internal/apiserver/cmd/genopenapi -check`), the parity matrix
(`go run ./hack/parity -check`) and the UI API types (`pnpm -C ui gen:api:check`) are checked outside
`make generate`; `make ci-static` and `make ci-ui` run them.

Go: run `go vet ./...` after changes. UI: `pnpm -C ui typecheck` before marking done.

## Interruption policy

Work autonomously. Stop and ask only for: unavailable credentials/infrastructure with no
meaningful local substitute; unresolvable material ambiguity; destructive or externally
consequential actions (force-push, publishing, real clusters, GitHub App registration,
touching anything outside this repo and disposable kind clusters); security/licensing
blockers; unexplained reproducible failures blocking progress; hard environment limits.

Disposable environments authorized: kind clusters named `patrol-*`, local Docker images
tagged `patrol-cd/*:dev`, temp dirs under the session scratchpad.

## Durable state

- `TASKS.md` — checklist, current work, blockers. Update when work starts/finishes.
- `docs/parity/matrix.md` — feature status with evidence links.
- `docs/verification/log.md` — executed checks (command, env, exit status, artifact).
- Upstream research clone (not committed): session scratchpad `upstream/argo-cd` at
  tag v3.5.5. Re-clone with
  `git clone --depth 1 --branch v3.5.5 https://github.com/argoproj/argo-cd.git`.
