# Patrol CD — task tracker

Legend: `[ ]` todo · `[~]` in progress · `[x]` done (evidence linked) · `[!]` blocked

Resume rule: read this file, then `docs/parity/matrix.md`, then `docs/verification/log.md`.

## Current work

- W1 parity research (`docs/parity/areas/*.md`): 9/10 areas done (1,289 features); ops area + completeness critic re-running (run wf_af0f5fae-226)
- W2 CRD types: implemented + reviewed; review-fix re-running (run wf_3b816f38-c83)
- W3 Phase 1a libraries (run wf_b4bb6ee9-d6b): done impl — sandbox, rbac, auth, lua+health, cache, scm-core, notifications; re-running — diff, git, helm/oci, scm-providers impl and all reviews/fixes
- W4 UI foundation (run wf_20ba85e9-0a8): scaffold, design system, shell, API layer
- W5 Phase 1b (run wf_0e7d0d9a-708): settings, project, tracking, clusters, sync engine
- W6 Phase 2a (run wf_e0428cb6-bde): repository creds, render, cmp, repo-server, compare, controllers, sharding/tree API, appset, apiserver core
- W1 research DONE: 1,457 features; aliases in docs/parity/aliases.yaml
- Session limit interrupted W1–W3 on 2026-10-12; resumed 2026-10-13 via resumeFromRunId. Checkpoint commit cee7e81.

## GATE (user instruction, 2026-10-16)

**When Phase 3 (API, CLI, UI: T3.1–T3.4) is complete and verified, BEFORE starting any further Phase 4 work (Dockerfile, Helm chart, manifests, e2e, packaging): create a PRIVATE GitHub repository for this project under the authenticated account (`gh`, user juliosaraiva) and push the code.**
- Explicitly authorized by the user in chat; covers creating the private repo and pushing. Nothing else external (no releases, no image publishing, no making it public).
- Before pushing: run gitleaks (installed in ~/go/bin) over the tree and history, confirm `.env*`/kubeconfigs/keys are not tracked, and review test-only keys are clearly marked.
- Needs real commits: local commit signing (1Password SSH agent) must work; do not bypass the user's signing config.
- Already in flight before this instruction (do not start more Phase 4 items): envs-and-examples workflow wf_8ca37009-3bd (kind env, fixtures, examples).

## Resume state (2026-10-16)

Phase 1a (sandbox, git, helm, oci, rbac, auth, lua, health, cache, diff, scm + 8 providers, notifications engine) and Phase 1b (settings, project, tracking, clusters, sync) are implemented, reviewed and fixed; Phase 1a re-verified by orchestrator (V-0004). UI foundation done (V-0005).
Phase 2a, 2b and UI pages all died on the usage limit on 2026-10-13 with zero completed agents; partial files were left on disk. Relaunched 2026-10-16 with a resume note so agents finish partial work:
- Phase 2a (repository, render, cmp, reposerver, compare, controller, sharding/treeapi, appset, apiserver-core): run wf_da6e16d6-c8f
- Phase 2b (notifications controller, webhook, hydrator + commit-server): run wf_7ff05528-e96
- UI pages (apps-core, apps-resources, create/appsets/extensions, settings): run wf_f74ab748-98d
`go build ./...` is red until these land.

Update 2026-10-16 (afternoon):
- Phase 2b DONE and verified (V-0006). UI pages DONE and verified (V-0008). UI extension-host mounts: agent ad6536e1e1ec08113 running.
- Phase 2a: repository, render, cmp, reposerver, appset, apiserver-core implemented+reviewed+fixed; reposerver fix and compare impl still running; controller and sharding/treeapi not started (wait on compare).
- Phase 3a API handlers (repositories, clusters-projects, appsets-misc, applications core routes): run wf_b0aa8ab6-934. Application resource routes (tree, resource, logs, terminal, actions) wait on controller treeapi.
- envs-and-examples (kind clusters, fixtures, examples): run wf_8ca37009-3bd.
- Next once Phase 2a lands: application resource routes, CLI (3 agents), cmd/patrolcd wiring, Dockerfile + Helm chart + manifests, e2e suite, security tests, benchmarks, docs.

## Blockers

- Local commits fail since 2026-10-13 ~01:00: `commit.gpgsign=true` with 1Password SSH signing returns "1Password: agent returned an error". Not overriding the user's signing config; changes accumulate in the working tree (staged). Needs: unlock/approve 1Password, then commit.

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
