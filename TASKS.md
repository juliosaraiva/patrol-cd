# Patrol CD — task tracker

Legend: `[ ]` todo · `[~]` in progress · `[x]` done (evidence linked) · `[!]` blocked

Resume rule: read this file, then `docs/parity/matrix.md`, then `docs/verification/log.md`.

## Current work

- W1 parity research (`docs/parity/areas/*.md`): 9/10 areas done (1,289 features); ops area + completeness critic re-running (run wf_af0f5fae-226)
- W2 CRD types: implemented + reviewed; review-fix re-running (run wf_3b816f38-c83)
- W3 Phase 1a libraries (run wf_b4bb6ee9-d6b): done impl — sandbox, rbac, auth, lua+health, cache, scm-core, notifications; re-running — diff, git, helm/oci, scm-providers impl and all reviews/fixes
- W4 UI foundation (run wf_20ba85e9-0a8): scaffold, design system, shell, API layer
- Session limit interrupted W1–W3 on 2026-10-12; resumed 2026-10-13 via resumeFromRunId. Checkpoint commit cee7e81.

## Blockers

- None yet. Known future dependency: live GitHub verification needs a GitHub App or token
  supplied by the user (see "Blocked on me" in the final report). Contract tests use fakes.

## Phase 0 — Foundation
- [x] T0.1 Inspect repo/tooling; git init; CLAUDE.md; ADRs 0001–0013; architecture overview
- [x] T0.2 Pin baseline (`docs/parity/baseline.md`)
- [x] T0.3 Go module + pinned deps (`hack/deps`), tools module (`tools/go.mod`: controller-gen v0.20.0, setup-envtest, kustomize v5.8.1), envtest 1.36.0 binaries
- [~] T0.4 Parity inventory per area (9/10 done) → `docs/parity/areas/*.md`
- [x] T0.5 Parity matrix generator `hack/parity` (areas + `docs/parity/status/*.yaml` overlays → `matrix.md`)
- [~] T0.6 CRD types + CEL + envtest admission tests
- [ ] T0.7 Makefile, CI workflow, lint config, `verify-generated`

## Phase 1 — Core libraries (parallel, disjoint packages)
- [ ] T1.1 `internal/settings`: settings ConfigMaps/Secrets parser, watcher, validation
- [ ] T1.2 `internal/rbac`: policy grammar, glob/regex, deny precedence, builtin roles, scopes
- [ ] T1.3 `internal/auth`: local accounts, bcrypt, JWT sessions, API tokens, revocation, lockout, OIDC/PKCE, Dex config
- [ ] T1.4 `internal/project`: source/destination/resource/namespace/window/signature checks
- [ ] T1.5 `internal/sandbox`: subprocess limits; path/symlink/archive safety
- [ ] T1.6 `internal/git`: CLI wrapper (fetch, checkout, ls-remote, submodules, LFS, SSH known hosts, TLS, proxy, askpass, GPG verify, shallow)
- [ ] T1.7 `internal/helm` + `internal/oci`: chart repos, OCI charts/artifacts, creds, index caching, dependency build
- [ ] T1.8 `internal/render`: directory, Helm, Kustomize (versions), Jsonnet (child process), CMP client, detection, param overrides files, build env, multi-source refs
- [ ] T1.9 `internal/reposerver` + `internal/cmp` + `cmd` wiring: HTTP service, cache, revision resolution, limits
- [ ] T1.10 `internal/clusters`: rest.Config from Cluster CRD (+ exec, AWS, GCP, Azure), in-cluster, connection state
- [ ] T1.11 `internal/cache`: per-cluster live graph, discovery, watches, exclusions/inclusions, tracking index, owner graph, health/info at ingest
- [ ] T1.12 `internal/diff`: normalization, ignore rules (JSON pointer, jq, managers), legacy 3-way, SSA/server-side diff, known types, secret masking
- [ ] T1.13 `internal/health` + `internal/lua`: built-in checks, Lua sandbox, customizations (Apache-2.0 data), actions
- [ ] T1.14 `internal/tracking`: label / annotation / annotation+label / installation ID
- [ ] T1.15 `internal/sync`: planner (phases, waves, hooks, ordering, options), executor (CSA/SSA/replace/force/prune/propagation), dry-run, namespace creation
- [ ] T1.16 `pkg/scm`: contract, registry, fake, conformance; GitHub (App auth, repos, PRs, revisions, webhooks, statuses, checks); GitLab, Gitea, Bitbucket Cloud/Server, Azure DevOps, AWS CodeCommit adapters
- [ ] T1.17 `internal/notifications`: expr triggers, templates+functions, all baseline services, subscriptions, dedup, self-service

## Phase 2 — Controllers and services
- [ ] T2.1 Application reconciler (refresh, compare, status, conditions, auto-sync, self-heal, finalizers/cascade deletion, post-delete hooks, skip-reconcile, apps in any namespace)
- [ ] T2.2 Operation reconciler (state machine, retries, cancel, history, retention, rollback)
- [ ] T2.3 Sharding (leases, algorithms, dynamic distribution), metrics, controller internal tree/diff API
- [ ] T2.4 ApplicationSet controller (all generators, templating, policies, progressive sync, appsets any namespace)
- [ ] T2.5 Notifications controller
- [ ] T2.6 Source hydrator + commit-server
- [ ] T2.7 Webhook ingestion (all baseline providers) → refresh annotations

## Phase 3 — API, CLI, UI
- [ ] T3.1 API server: route table, OpenAPI generation, problem details, auth middleware, RBAC, audit, SSE watches, logs, terminal, badge, extensions proxy, deep links, settings, version
- [ ] T3.2 All resource services (applications, appsets, projects, repositories, repocreds, clusters, accounts, certificates, gpgkeys, sessions, notifications, settings)
- [ ] T3.3 `pkg/apiclient` + CLI (`patrol`) full command tree incl. `admin` tools, core mode, completion
- [ ] T3.4 Web UI (inventory, details, tree, diff, logs, terminal, history, sync panel, onboarding, projects, accounts, settings, appsets, notifications, extensions)

## Phase 4 — Packaging & environments
- [ ] T4.1 Dockerfile (pinned helm/kustomize/git-lfs), local image build
- [ ] T4.2 Generated manifests: install, namespace-install, core-install, ha; Helm chart with documented values; NetworkPolicies; security contexts
- [ ] T4.3 kind environment (control + target clusters, in-cluster git server, chart repo, OCI registry)
- [ ] T4.4 Examples (each source type, multi-source, multi-cluster, GitHub previews, notifications, CMP plugin)

## Phase 5 — Verification
- [ ] T5.1 Unit + property tests across packages
- [ ] T5.2 envtest integration tests (controllers)
- [ ] T5.3 e2e suite on kind (deploy, drift, self-heal, update, prune, recovery, multi-source, multi-cluster, appset, isolation, auth, notifications, extensions)
- [ ] T5.4 Browser tests (Playwright)
- [ ] T5.5 Security tests (authz negative, malicious repo/plugin, SSRF, webhook spoofing/replay)
- [ ] T5.6 Failure injection: restart mid-sync, cache loss, cluster unavailable, repo unavailable
- [ ] T5.7 HA, backup/restore, upgrade
- [ ] T5.8 Benchmarks + methodology
- [ ] T5.9 Side-by-side fixtures vs Argo CD v3.5.5 where practical
- [ ] T5.10 SBOM, vulnerability and license reports

## Phase 6 — Documentation & review
- [ ] T6.1 Product overview, install/quickstart, CRD/API/CLI reference, GitHub guide, SCM extension guide, plugin + UI extension guides, multi-tenancy/security guide, runbooks, upgrade/DR, Argo migration guide, known differences
- [ ] T6.2 Threat model with mitigations mapped to tests
- [ ] T6.3 Independent final review against parity matrix; fix blocking findings
- [ ] T6.4 Final report

## Completed log

- 2026-10-12 T0.1–T0.3 (see `docs/verification/log.md` entries V-0001..V-0003)
