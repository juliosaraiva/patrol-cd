# Patrol CD

Patrol CD is a Kubernetes-native continuous delivery platform that continuously reconciles
declaratively specified application state from version-controlled sources into Kubernetes
clusters. It is an independent implementation (its own control plane, reconciliation engine,
API, CLI and web UI) with functional parity as the goal against a frozen
[Argo CD v3.5.5](https://github.com/argoproj/argo-cd/tree/v3.5.5) baseline
(commit `a5038b0e2bbac1715754380ae0084df8e3d5b48c`). It does not embed, wrap or proxy Argo CD.

> **Status: work in progress, not released.** Phases 0–3 (research, architecture, libraries,
> controllers, API, CLI, web UI, server wiring) are implemented and tested locally. Packaging
> (images, Helm chart, install manifests), cluster-level end-to-end tests, security and
> performance verification, and most documentation are **not done yet**. Nothing here has been
> verified against a live GitHub, a real kubelet or a real multi-node cluster. The API group
> `cd.patrol.example` is a development placeholder (see ADR-0002).

## What exists

| Area | Where | State |
|------|-------|-------|
| Parity inventory of Argo CD v3.5.5 (1,457 features) | [`docs/parity`](docs/parity/baseline.md) | complete; per-feature implementation status in [`matrix.md`](docs/parity/matrix.md) |
| Architecture, ADRs, threat model | [`docs/architecture`](docs/architecture/overview.md) | written; living documents |
| CRDs (`Application`, `Operation`, `AppProject`, `ApplicationSet`, `Repository`, `RepositoryCredential`, `Cluster`, `SCMProvider`) | [`api/v1alpha1`](api/v1alpha1), [`deploy/crds`](deploy/crds) | implemented, admission-tested with envtest |
| Control plane (controller, repo-server, ApplicationSet controller, notifications, hydrator, commit-server, CMP server) | [`internal/`](internal) | implemented; unit + envtest tested |
| Public API (REST/SSE, OpenAPI 3.1) | [`internal/apiserver`](internal/apiserver), [`api/openapi/openapi.json`](api/openapi/openapi.json) | 125 documented operations (incl. the two webhook receivers) |
| SCM provider contract + GitHub (App auth, PRs, webhooks, statuses/checks) + 7 other providers | [`pkg/scm`](pkg/scm) | contract-tested against fakes only |
| CLI (`patrol`, including `--core` mode and `admin` tools) | [`internal/cli`](internal/cli), [`cmd/patrol`](cmd/patrol) | implemented |
| Multi-call control-plane binary (`patrolcd`) | [`cmd/patrolcd`](cmd/patrolcd), [`internal/server`](internal/server) | implemented |
| Web UI (React + TypeScript) | [`ui/`](ui) | implemented; tested against mocked APIs, Playwright specs not yet run |
| Integration harness (real API servers via envtest, real git/helm/kustomize) | [`test/integration`](test/integration) | lifecycle + recovery scenarios passing |
| Examples | [`examples/`](examples) | validated against the CRDs and real renderers |

Verification evidence (commands, environments, results, known gaps) is recorded in
[`docs/verification/log.md`](docs/verification/log.md). Anything that used a fake external
system is labelled simulated there.

## Build and test

Requirements: Go (see `go.mod`), Node + pnpm, git, Helm, and the pinned tools installed by
`make tools` (controller-gen, setup-envtest, kustomize).

```bash
make tools            # install pinned tools into ./bin
make generate         # deepcopy + CRDs
make verify-generated # fails if generated files are stale
make build            # Go binaries into ./bin
make test             # Go tests incl. envtest-backed ones
pnpm -C ui install && pnpm -C ui test && pnpm -C ui build

# integrated lifecycle/recovery suite (real kube-apiservers via envtest)
PATROL_INTEGRATION=1 go test ./test/integration/...
```

See [`CLAUDE.md`](CLAUDE.md) for conventions and [`TASKS.md`](TASKS.md) for the task tracker
and current limitations.

## Known limitations (high level)

- No container images, Helm chart or install manifests yet; no run on a real cluster yet.
- Pod/Deployment health is simulated in tests (envtest has no kubelet); port-forward, pod logs
  and terminal against a real kubelet are unverified.
- GitHub integration is verified against contract tests and a local fake of the GitHub API,
  not against live GitHub.
- Argo CD parity is tracked feature by feature; many rows are still `in-progress` or
  `not-started` in the matrix.

## License

Apache License 2.0 (see [`LICENSE`](LICENSE) and [`NOTICE`](NOTICE)). Some Lua health-check and
resource-action data files are derived from Argo CD v3.5.5 and keep their Apache-2.0 origin.
