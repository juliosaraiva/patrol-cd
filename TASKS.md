# Patrol CD — task tracker

Legend: `[ ]` todo · `[~]` in progress · `[x]` done (evidence linked) · `[!]` blocked

Resume rule: read this file, then `docs/parity/matrix.md`, then `docs/verification/log.md`.

## Current work

- **2026-10-22: automated releases with a changelog**, on the user's request (branch
  `claude/automated-release-management-801d68`). `hack/release` (next version from conventional commits,
  `CHANGELOG.md` section, chart version bump, release notes, release assets; unit-tested), `hack/release.mk`
  (`make release-*`), `.github/workflows/release-pr.yml` (keeps the `release/next` pull request current after
  every push to `main`) and `release.yml` (after CI passes on `main` with a release pending: CLI binaries,
  install manifests, chart, multi-arch image to `ghcr.io/<owner>`, chart to `oci://…/charts`, tag and GitHub
  release); commit-subject lint on pull requests in the `static` job; guide `docs/guides/releasing.md`.
  Checked locally (V-0038, V-0039). **Not yet run on GitHub**; needs the repository setting "Allow GitHub Actions to
  create and approve pull requests" and, for CI on the release pull request, the secret `RELEASE_TOKEN`.
- **2026-10-21/22: CI with the kind job** (T0.7), on the user's request; pull request
  [#4](https://github.com/juliosaraiva/patrol-cd/pull/4) (`feat/ci`). `.github/workflows/ci.yml` (jobs:
  static, unit, ui, image, integration, deploy, kind, and the `ci` gate), `hack/ci.mk` (`make ci-*`),
  `hack/ci/` (pinned tool installer, preflight, `go test -json` skip report, tier selection and its tests,
  kind diagnostics), the kind scripts' `PATROL_WAIT_SCALE`; guide `docs/guides/ci.md`. Checked locally first
  (V-0034), then on GitHub: four runs of the full tier with the kind job, the first and third failed and were
  fixed, the second and fourth **passed all 9 jobs** (V-0035, tested head `44b420e`;
  `docs/verification/runs/2026-10-22-github-ci-runs.md`). The work also fixed what it found in the product
  and its tests: git before 2.48 (`origin/HEAD`), leaked envtest control planes (`internal/envtestx`), the
  start-up race on the ApplicationSet CRD (`waitForCRDs`, KV-14), a project token refused as revoked before
  the cache saw it, and flaky or racy tests (`phase4-findings.md` CI-F1 to CI-F14). **Not yet observed on
  GitHub**: the kind-only tier, a pull request with a label, `CI_HEAVY_ON_PR`, manual runs, the code and full tiers
  on caches restored from `main` (docs-only runs restored them), arm64; no failure path of the kind job on a runner.
  The first push to `main` passed the full tier and the docs-only and code-only probe pull requests ran only their
  tiers (V-0036); a documentation-only push to `main` ran only the docs-only tier (V-0037). Cache usage went over
  GitHub's 10 GB limit after the first full runs (12.4 GB) and was 9.4 GB once the probes' caches were deleted.
- **2026-10-18: Phase 4 closeout** (Dockerfile, static manifests, Helm chart, first-run
  bootstrap). Two review rounds fixed; the final full `./test/deploy/` run passed (157 subtests, 281 s,
  `docs/verification/log.md` V-0027); committed and pushed (`3ea2bd3`). **2026-10-19: the Docker
  disk was freed (V-0028) and the image was built and checked (linux/arm64); on the user's request
  the kind validation (T4.3) ran (V-0029..V-0031), was reviewed twice, and its closing run after
  the review fixes is V-0032.** State and open items:
  section "PHASE 4 CLOSEOUT STATE" below. The W1–W6 lines that follow are historical.
- W1 parity research (`docs/parity/areas/*.md`): 9/10 areas done (1,289 features); ops area + completeness critic re-running (run wf_af0f5fae-226)
- W2 CRD types: implemented + reviewed; review-fix re-running (run wf_3b816f38-c83)
- W3 Phase 1a libraries (run wf_b4bb6ee9-d6b): done impl — sandbox, rbac, auth, lua+health, cache, scm-core, notifications; re-running — diff, git, helm/oci, scm-providers impl and all reviews/fixes
- W4 UI foundation (run wf_20ba85e9-0a8): scaffold, design system, shell, API layer
- W5 Phase 1b (run wf_0e7d0d9a-708): settings, project, tracking, clusters, sync engine
- W6 Phase 2a (run wf_e0428cb6-bde): repository creds, render, cmp, repo-server, compare, controllers, sharding/tree API, appset, apiserver core
- W1 research DONE: 1,457 features; aliases in docs/parity/aliases.yaml
- Session limit interrupted W1–W3 on 2026-10-12; resumed 2026-10-13 via resumeFromRunId. Checkpoint commit cee7e81.

## STOP RULE (current: user instruction 2026-10-18)

**Phase 4 scope only: Dockerfile/image tooling, static manifests, Helm chart** (plus the
`patrolcd bootstrap` first-run step they require). CI pipelines, kind validation, e2e, the
security and performance phases (Phase 5/6) and publishing anything (images, charts, releases,
making the repository public) remain **out of scope until the user says otherwise**. When the
Phase 4 closeout is committed, stop and wait for the user. (The kind validation, T4.3, was done
on the user's request on 2026-10-19, and CI with the kind job on 2026-10-21; the rest of this list
is unchanged.)

Superseded rule (user instruction, 2026-10-17; satisfied at the Phase 3 boundary, kept for
history): **STOP when Phase 3 is completed.** Phase 3 = T3.1–T3.4 (API server, resource services, apiclient + CLI incl. `--core`, web UI) plus the server wiring they need (`cmd/patrolcd`, `internal/server`). At that boundary: finish the pre-push checks, make signed commits, create the PRIVATE GitHub repo and push (the earlier gate instruction, still in force), then STOP and wait for the user. Do NOT start Phase 4 (Dockerfile, Helm chart, manifests, kind validation) or Phase 5/6 work (e2e scenarios beyond the harness already in flight, security tests, benchmarks, parity audit backfill, docs workstreams).
- In flight and allowed to finish: cli-core-mode workflow (wf_0610c791-9b8); integration HARNESS agent only (wf_8ffdfd80-a30 `impl:harness`: stack + lifecycle + recovery scenarios). The workflow's later `scenarios`/`review` stages are Phase 5 → stop the workflow once `impl:harness` returns.
- Deferred past the stop (not Phase 3): parity status backfill for Phase 1a packages, UI shuffle-order test fixes, full scenario breadth (multi-source, multicluster, appset, tenancy, auth, notifications, extensions, CLI scenarios), Phase 4–6.

## GATE (user instruction, 2026-10-16)

**When Phase 3 (API, CLI, UI: T3.1–T3.4) is complete and verified, BEFORE starting any further Phase 4 work (Dockerfile, Helm chart, manifests, e2e, packaging): create a PRIVATE GitHub repository for this project under the authenticated account (`gh`, user juliosaraiva) and push the code.**
- Explicitly authorized by the user in chat; covers creating the private repo and pushing. Nothing else external (no releases, no image publishing, no making it public).
- Before pushing: run gitleaks (installed in ~/go/bin) over the tree and history, confirm `.env*`/kubeconfigs/keys are not tracked, and review test-only keys are clearly marked.
- Needs real commits: local commit signing (1Password SSH agent) works again as of 2026-10-16 (ef3bc26 is signed); do not bypass the user's signing config.
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

- (Resolved 2026-10-17) Commit signing / SSH via 1Password: the user unlocked the agent; 17 signed commits created and pushed.

- **(Resolved 2026-10-19) Docker Desktop VM disk was full (2026-10-16).** At the user's request I pruned the build cache (18.54 GB) and dangling images (5.183 GB); 152 GB are free in the VM and other projects' containers, volumes and tagged images were not touched (V-0028). Still reclaimable if ever needed: about 17 GB of tagged unused images and 4 GB of volumes (not mine to delete). Unblocked T4.3 kind validation (done on the user's request on 2026-10-19, V-0029..V-0031), T5.3 e2e and T5.6–T5.8 (not started, out of scope until told).

- (Resolved 2026-10-16) Local commits were failing on 1Password signing; signing works again, so work can now be committed normally.

- None yet. Known future dependency: live GitHub verification needs a GitHub App or token
  supplied by the user (see "Blocked on me" in the final report). Contract tests use fakes.

## Phase 0 — Foundation
- [x] T0.1 Inspect repo/tooling; git init; CLAUDE.md; ADRs 0001–0013; architecture overview
- [x] T0.2 Pin baseline (`docs/parity/baseline.md`)
- [x] T0.3 Go module + pinned deps (`hack/deps`), tools module (`tools/go.mod`: controller-gen v0.20.0, setup-envtest, kustomize v5.8.1), envtest 1.36.0 binaries
- [~] T0.4 Parity inventory per area (9/10 done) → `docs/parity/areas/*.md`
- [x] T0.5 Parity matrix generator `hack/parity` (areas + `docs/parity/status/*.yaml` overlays → `matrix.md`)
- [~] T0.6 CRD types + CEL + envtest admission tests
- [~] T0.7 Makefile, CI workflow, lint config, `verify-generated` — Makefile and `verify-generated` done; the CI workflow passed its full tier on GitHub (V-0035, `docs/guides/ci.md`); no golangci-lint config (the defaults apply)

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
- [x] T4.1 Dockerfile (pinned helm/kustomize/git-lfs), local image build — Dockerfile, pins, `hack/images`, `make image*` targets; static checks (V-0018/V-0025) and, since 2026-10-19, the executed build for linux/arm64 (`make image`, 386 MB) with `make image-layout-check` (52/52) and `make image-smoke` (25/25) (V-0028, `docs/verification/runs/2026-10-19-image-build.md`). The arm64 image ran the five components of the chart's default profile on kind (T4.3). Not done: linux/amd64 and multi-arch builds, SBOM, scans, push.
- [x] T4.2 Generated manifests: install, namespace-install, core-install, ha; Helm chart with documented values; NetworkPolicies; security contexts — chart and `deploy/manifests` (generated from it), validated against envtest kube-apiservers: strict dry runs, Pod Security `restricted`, helm install/upgrade/rollback/uninstall/re-install, RBAC sufficiency with the real binary (V-0019…V-0027). Since 2026-10-19 the default profile also ran on kind (T4.3): live pods, probes by a kubelet and part of the NetworkPolicy enforcement (repo-server egress, metrics ingress; kindnet). Still unverified: the other profiles and the static bundles beyond one manual `cluster-install` apply, other CNIs, HA.
- [x] T4.3 kind environment (control + target clusters, in-cluster git server, chart repo, OCI registry) — `make image kind-up kind-fixtures kind-install kind-verify` from nothing in about 6 minutes; clean-room run 55/55 checks with `--lifecycle` and the script as it was before the review (V-0031, `docs/verification/runs/2026-10-19-kind-validation.md`); a later `make kind-verify` failed 3 checks until the fixture host key was restored (V-0031, KV-10). Closing clean-room run after the review fixes: 58 of 58 checks with `--lifecycle`, 42 of them in a default run (V-0032). One CNI (kindnet), arm64, one replica per component.
- [x] T4.4 Examples (each source type, multi-source, multi-cluster, GitHub previews, notifications, CMP plugin) — `examples/` (apps, platform, github, notifications, plugins; index in `docs/guides/examples.md`), added 2026-10-16 (`67e3faa`). `go test -race -count=1 -v ./test/examples/` with `PATROL_REQUIRE_ENVTEST=1 PATROL_REQUIRE_EXAMPLE_TOOLS=1`: 8 tests, 24 passes, 0 skipped on 2026-10-16 and again on 2026-10-21 (V-0033): 99 static objects and 307 server-side dry-run creates on an envtest kube-apiserver, 39 Applications rendered with the real tools, 11 ApplicationSets expanded into 23 Applications. Evidence level: validation and rendering only (envtest, a fake GitHub, a stub plugin service); the examples were not applied to a running Patrol CD installation (the kind validation ran the `test/e2e/fixtures` applications, not `examples/`).

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
- 2026-10-18 T4.2 chart + static manifests, T4.1 image tooling, first-run bootstrap; two review rounds (V-0018..V-0027); 2026-10-19 T4.1 image built and checked (V-0028)
- 2026-10-19 T4.3 kind validation (V-0029..V-0032)
- 2026-10-21 T4.4 examples ticked on the evidence of `go test ./test/examples` (V-0033); Phase 4 re-checked (build, vet, `test/deploy`)

## Requirements carried to later tasks

- **Server must serve `/extensions.js`** (UI extension bundle concatenation per upstream API-050; empty file when no extensions are installed). The UI now requests it on every page load (from `initExtensions()`), so without it every page logs a console error. Owner: server wiring task (cmd/patrolcd `server` + `internal/apiserver` static/extension serving). Also serve `/extensions/*` static assets from the extensions directory.
- `docs/parity/status/settings.yaml` lists API-111 as in-progress ("Settings part only"); update to implemented once the chat button's server-side settings are wired (UI part is done).

## Cleanup backlog (do after the in-flight workflows finish, before the GitHub push)

- Finalizer names: Kubernetes 1.36 warns for unqualified finalizer names (`resources-finalizer.cd.patrol.example`, `pre-delete-finalizer.…`, `post-delete-finalizer.…`) — 33 warnings in the examples test. Rename to domain-qualified single-slash names (`cd.patrol.example/resources-finalizer`, `…/resources-finalizer-foreground`, `…/resources-finalizer-background`, `…/pre-delete-finalizer(-cleanup)`, `…/post-delete-finalizer(-cleanup)`) in api/v1alpha1/keys.go, ADR-0002 note, docs/reference/crds.md, UI (ui/src/pages/applications/{shared/app.ts,delete/DeleteDialog.tsx}, create/model.ts, mocks/fixtures), examples/, docs/architecture/components/*.md and parity areas wording; update controller/appset code if it uses literals. Behavior is unchanged; this removes the warnings. Migration note for Argo CD users: Argo finalizer names are not honored by Patrol.
- Regenerate `api/openapi/openapi.json` after all API services land (`go run ./internal/apiserver/cmd/genopenapi`), then `-check`.

## Remaining Phase 3 / gate checklist (updated 2026-10-16 evening)

- DONE: Phase 3a API handlers (repositories, clusters-projects, appsets-misc, applications core) — reviewed+fixed (run wf_b0aa8ab6-934); rbac gained `scmproviders` (orchestrator edit).
- RUNNING: Phase 3b (run wf_a1267bc5-5bd): application resource routes (tree/logs/terminal/actions/diff), CLI core+platform, CLI app/appset, CLI admin (incl. Argo CD import/migration conversion).
- RUNNING: rbac regex-mode project-policy DoS fix (agent af2648759f6408f2b) — found by orchestrator rerun of the property test; see V-entry when it lands.
- RUNNING: Phase 2a controller review/fix.
- TODO before the GitHub gate: (1) server wiring: `internal/server` assembly + `cmd/patrolcd` multi-call (server, controller, repo-server, appset-controller, notifications-controller, cmp-server, commit-server, k8s-auth, askpass/jsonnet/sandbox helpers) + `/extensions.js` + UI embed; (2) CLI `--core` mode (in-process server transport); (3) regenerate api/openapi/openapi.json and reconcile UI generated types (UI placeholder schema names differ: RepositoryInfo/RepositoryCredentialInfo, certData plain text, SCMProviderWriteRequest has no credentials); (4) cleanup backlog above (finalizer names); (5) full-tree `go build ./... && go vet ./...`, `go test -race ./...`, UI checks; (6) gitleaks; (7) logical signed commits; (8) create PRIVATE repo and push.
- **Parity status backfill (Phase 1a packages):** sandbox, git, helm, oci, rbac, auth, lua, health, cache, diff, scm(+providers), notifications engine were built before per-package `docs/parity/status/<component>.yaml` files were required (rbac.yaml and others are missing). Run an audit workflow before the final report that maps each parity ID to code+tests for those packages and writes conservative status files (no upgrade without a test that exercises the behavior).
- UI→API requests UI-1..UI-6 resolved (openapi stored-response schemas, log SSE docs, request bodies, enums, webhook receivers doc-only service). Optional UI cleanups listed in docs/verification/integration-findings.md (use generated Stored<…>/WatchEventType types; drop `data: ''` in resize frames; read webhook receiver mapping from the spec).
- Carried-over requests: treeapi handler (internal/controller/treeapi/handler.go serverSideDiff) should also enforce target↔live identity match (API layer already does).
- Operational note: machine load average ~180 (10 cores) while many agents run suites concurrently → timing flakes (UI contract test timeout, cache test). Keep concurrency ≤ 3 active workflows; use generous timeouts in tests.
- UI backlog: two order-dependent tests exposed by `--sequence.shuffle` (ApplicationDetailsPage.test.tsx seed 1792182754351; shell.test.tsx small-screen drawer seed 1792182808729) — fix before the final review; default order is green.
- **Phase 5 scenario WIP preserved, not in the Phase 3 tree:** the stopped `impl:scenarios` agent wrote 8 unverified scenario files (appset, auth, cli, extensions, multicluster, multisource, notifications_webhook, tenancy + helpers_test.go). They were removed from the working tree and are recoverable from `refs/patrol/snapshots/20261016T214116Z` (`git checkout refs/patrol/snapshots/20261016T214116Z -- test/integration/<file>`). The extra helpers it added under `test/integration/stack/` (appset.go, charts.go, oidc.go, plugins.go, settings.go and edits to cli.go/metrics.go/stack.go/tokens.go) stay in the tree and compile.
- **Finalizer rename DEFERRED (decision 2026-10-17):** not a mechanical rename — `patrol admin migrate-group` (internal/cli/admin/groupmap.go, misc_test.go, backup_test.go, migrategroup_test.go) and the Argo CD converter (argoconvert.go:838, argocdimport_test.go) have logic/tests keyed to the `<name>.cd.patrol.example[/variant]` shape. Cosmetic only (Kubernetes warns for unqualified finalizer names; Argo CD has the same shape). Do after the stop, together with those tools.
- Pre-push secret scan (2026-10-17): `gitleaks dir` and `gitleaks git` with `.gitleaks.toml` → 0 findings. One deliberately committed TEST-ONLY private key remains (`test/e2e/fixtures/git-server/manifests/ssh-host-key.secret.yaml`, labeled, allowlisted by path); consider generating it at deploy time later.

## PHASE 3 COMPLETE (2026-10-17)

T3.1 API server, T3.2 resource services, T3.3 apiclient + CLI (incl. `--core` and `admin`), T3.4 web UI, plus `cmd/patrolcd` wiring and the integration harness: implemented, independently reviewed, fixed and verified (V-0010…V-0016). Full-tree gate (V-0016): 83/83 Go packages, UI 912 tests, lint 0 issues.
Next (NOT started, per STOP rule): the private GitHub repo + push (user instruction) happens at this boundary; after that, wait for the user. Backlog for later phases is in the sections above (Phase 4 packaging/kind, Phase 5 verification breadth incl. preserved scenario WIP in snapshot 20261016T214116Z, Phase 6 docs, parity status backfill, finalizer rename, UI shuffle-order tests, Docker disk blocker).

## GitHub gate DONE (2026-10-17) — STOPPED at the Phase 3 boundary

Private repository created and `main` pushed: https://github.com/juliosaraiva/patrol-cd (visibility PRIVATE; HEAD 5e64fa8 matched local; commits show Verified on GitHub). Pre-push: gitleaks tree+history 0 findings (V-0016 + `.gitleaks.toml`), tracked files reviewed. Per the STOP rule no Phase 4+ work has been started. Resume only when the user says so; start by reading this file, `docs/verification/log.md`, and the open decisions: Docker Desktop disk space (blocker for image build/kind; resolved 2026-10-19), parity status audit/backfill, preserved Phase 5 scenario WIP (snapshot `20261016T214116Z`), finalizer rename, UI shuffle-order tests.

## PHASE 4 STARTED (2026-10-18) — user instruction: "Start on Phase 4: Dockerfile, manifests and Helm chart"

Scope authorized now: T4.1 Dockerfile/image tooling, T4.2 static manifests + Helm chart (+ the `patrolcd bootstrap` first-run step they require, found missing: nothing generated the admin password or `patrol-internal-token`). NOT in scope until told: CI pipelines, kind validation (Docker disk still full; both resolved and done on the user's request on 2026-10-19), e2e/security/perf phases, publishing anything.
Workflow wf_71881e0f-20d: Build (boot, image, chart in parallel) → Assemble (static manifests, test/deploy harness incl. helm install/upgrade/uninstall + RBAC sufficiency against envtest) → independent reviews (chart-security, image-bootstrap) → fixes. Agents do not commit; the orchestrator commits (signed) and pushes after verification.
Known limits recorded up front: `docker build` could not be executed (Docker Desktop disk full; resolved and built 2026-10-19, see below) → image steps were verified piecewise and labeled NOT EXECUTED until then; no kubelet/CNI in envtest → NetworkPolicy enforcement and live pods unverified there (the kind validation of 2026-10-19 exercised them for the default profile, T4.3).

## PHASE 4 CLOSEOUT STATE (2026-10-18)

Done (evidence `docs/verification/log.md` V-0018…V-0027):
- Image (T4.1 `[x]`): Dockerfile, pinned tools, `hack/images`, `make image*`; built for linux/arm64 on 2026-10-19 and checked (`make image-layout-check`, `make image-smoke`, V-0028). Not done: amd64/multi-arch, SBOM, scans, push, run in a cluster.
- Helm chart and static manifests (T4.2 `[x]`), first-run bootstrap (`patrolcd bootstrap`), deployment validation harness `test/deploy` (envtest only).
- Review round 1 (5 findings) fixed (V-0023). Review round 2 (`wf_21493a28-fb7`): 12 confirmed findings fixed except the parts listed as not fixed; of the 25 minor findings, 24 fixed and CH-10 fixed in the docs only. DV-6 is fixed (stale initial admin Secret replaced). Per-finding table: `docs/verification/runs/2026-10-18-phase4-review-round2.md`.
- Hash tables of the 2026-10-17 run records re-derived on the final tree; parity matrix regenerated (no status raised; deployment entries stay `in-progress`).

Open items:
- **Final full `./test/deploy/` run after round 2: passed** (21 tests, 157 subtests, 281 s; V-0027). Earlier closeout runs: one failure from a concurrent `make verify-generated`, one CRD-deletion timeout under load (V-0026); the CRD wait now reports its state and waits 5 minutes.
- **Docker disk**: resolved 2026-10-19 (Blockers above). `make image-multiarch`, `make image-sbom` and the R2-8 SBOM/license checks are now possible but were not run.
- **kind validation** (T4.3): done 2026-10-19 on the user's request (V-0029 environment, V-0030 manual install and checks, V-0031 clean-room run 55/55 and the failed re-run after it; closing clean-room run after the review fixes V-0032, 58/58). Findings KV-1..KV-13 are in `docs/verification/phase4-findings.md`; KV-1 (repo-server crash on a volume mount root), KV-2, KV-4..KV-9 and KV-12 were fixed, KV-3 is documented, KV-10, KV-11 and KV-13 are observations (KV-11 and KV-13 open). The environment is left running (`make kind-down` removes it).
- **e2e on kind (T5.3)**: out of scope until told. `hack/kind/verify-install.sh` covers install, security contexts, part of NetworkPolicy, metrics endpoints, sync and drift (with `--lifecycle` also helm rollback, upgrade, uninstall and re-install), but not Application update, prune, self-heal, recovery, multi-cluster tenancy, ApplicationSets, auth/SSO, notifications or extensions.
- **CI pipelines** (T0.7): authorized 2026-10-21 (CI with the kind job); done and passing its full tier on GitHub (see Current work). Not in scope: publishing images or charts, required checks (no branch protection on the Free plan), Playwright, scheduled runs.
- **DV-2**: a core installation has no AppProject `default`; documented in the guides, not fixed (policy decision).
- Round-2 follow-ups R2-3…R2-8 (`docs/verification/phase4-findings.md`; R2-1 overview.md notifications row and R2-2 `patrolcd bootstrap` help text were done by the orchestrator on 2026-10-18): `hack/build.mk` tree state, musl copyright for connect-proxy, `.gitignore` kubeconfig*.go, optional clock-free staleness check, keep-or-delete decision for the trust ConfigMaps on uninstall, SBOM/apk checks that need the image.
- Other Phase 4 findings still listed in `phase4-findings.md` (M-2…M-7, DV-3…DV-5, DV-7…DV-9): check their status there before acting.
- Earlier backlog unchanged: parity status backfill for Phase 1a packages, finalizer rename, UI shuffle-order tests, Phase 5 scenario WIP (snapshot `20261016T214116Z`).
