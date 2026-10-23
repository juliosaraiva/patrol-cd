# Backlog sweep of 2026-10-23: from TASKS.md to GitHub issues

On 2026-10-23 the task tracker moved from `TASKS.md` to [GitHub issues](https://github.com/juliosaraiva/patrol-cd/issues), on the user's request to check every task, bug fix, production-readiness and observability gap and to track the work in GitHub issues instead of `TASKS.md`. This records how the issues were found and checked, what was left out and why. The tracker as it was is archived in [`tasks-until-2026-10-23.md`](tasks-until-2026-10-23.md).

## Method

A read-only agent workflow (`wf_05f8b7e7-c04`) on `main` at `73465cd`, 27 agents:

1. **Explore**: eight explorers, one per dimension, each required to cite evidence it checked itself (file:line, tracker ID, CI run, command output): recorded findings (`docs/verification/*-findings.md`, threat model, run records), the `TASKS.md` backlog and component docs, parity areas 01-05 and 06-10 (`docs/parity`), defects (CI failures on `main`, recorded flaky tests, a sweep of the non-test Go code and the UI), production readiness, observability (compared with upstream Argo CD v3.5.5), and test gaps. 311 candidates.
2. **Consolidate**: one agent grouped candidates that one pull request would close; 119 were merged into others, leaving 192.
3. **Verify**: 16 adversarial verifiers, 12 candidates each, tried to refute every candidate against the code and records (verdicts: confirmed, corrected, already done, not actionable, unsupported). A completeness critic added 8 candidates, verified the same way.
4. **Result**: 194 issues filed (confirmed or corrected), 6 rejected, 146 tracker entries found already done.

A second workflow (`wf_5a7eb950-9cc`, 8 agents) rewrote the 53 merged candidates into single issue texts without adding facts. The agents are language models: a confirmed verdict means the verifier saw the problem in the code or the records, not that the list is complete.

## Issues

| Tracking issue | Sub-issues |
|---|---|
| [#205](https://github.com/juliosaraiva/patrol-cd/issues/205) bugs and flaky tests | 16 |
| [#206](https://github.com/juliosaraiva/patrol-cd/issues/206) security | 15 |
| [#207](https://github.com/juliosaraiva/patrol-cd/issues/207) observability | 21 |
| [#208](https://github.com/juliosaraiva/patrol-cd/issues/208) production readiness | 25 |
| [#209](https://github.com/juliosaraiva/patrol-cd/issues/209) CI and release automation | 9 |
| [#210](https://github.com/juliosaraiva/patrol-cd/issues/210) test and verification gaps (Phase 5) | 33 |
| [#211](https://github.com/juliosaraiva/patrol-cd/issues/211) documentation (Phase 6) | 16 |
| [#212](https://github.com/juliosaraiva/patrol-cd/issues/212) parity gaps against Argo CD v3.5.5 | 33 |
| [#213](https://github.com/juliosaraiva/patrol-cd/issues/213) records and cleanup | 26 |

By priority: p0 3, p1 8, p2 92, p3 91. By kind: parity 45, testing 36, chore 26, docs 23, production-readiness 20, security 15, bug 12, ci 9, observability 4, flaky-test 4.

`p0` at filing time (`main` was red):

- [#91](https://github.com/juliosaraiva/patrol-cd/issues/91) Fix flaky TestClientServerRoundTripProperty: a path value of exactly "/" never matches a route (CI-F20)
- [#93](https://github.com/juliosaraiva/patrol-cd/issues/93) Fix flaky TestDynamicDistributionEnvtest: wait for the released shard Lease instead of reading it once (CI-F19)
- [#95](https://github.com/juliosaraiva/patrol-cd/issues/95) Make the Release PR workflow pass on main: allow Actions to open PRs and create the release label

## Rejected candidates

- **Close out the unexplained TestStaticManifestsApply CRD deletion timeout** (not-actionable): This is a watch item that is already documented and has already been made diagnosable. No code work is open. Records: docs/verification/log.md:61 V-0026 (timeout once under full-suite load, then passed 3/3 in isolation) and :62 V-0027 (full run passed, 'still unexplained, only made diagnosable'); TASKS.md:224; runs/2026-10-18-phase4-review-round2.md:109-110. The diagnostic change is in place: test/deploy/manifests_test.go:283-291 sets the CRD wait to 5 minutes (with a comment that explains it), and :328-360 waitDeleted/deletionFacts report deletionTimestamp, finalizers, conditions and remaining instances. No recurrence: V-0027 passed 21/157, V-0033 (2026-10-21) passed 21/157, and the CI Deploy suite jobs succeeded in runs 39876114735 and 39872474264. Nothing can be fixed until the failure recurs. If it does, the failure output will carry the facts needed to open a concrete issue.
- **Fix the two UI tests that fail under vitest shuffle** (not-actionable): Real, but it duplicates bugs-ui-order-dependent-tests: same two tests, same seeds, same files. It should be filed once, under that key. Re-verified in this pass. In ui/ on 73465cd, `pnpm exec vitest run --no-cache src/shell/shell.test.tsx --sequence.shuffle --sequence.seed=1792182808729` -> 1 failed | 45 passed (expected '/applications' to be '/applicationsets'). `... src/pages/applications/ApplicationDetailsPage.test.tsx --sequence.shuffle --sequence.seed=1792182754351` -> 1 failed | 15 passed (no 'Managed resources' table). The sources (log.md V-0014, runs/2026-10-16-ui-flake.md:162-175) are merged into the canonical item.
- **Fix the two order-dependent UI tests and run vitest with shuffle** (not-actionable): Real, but it duplicates bugs-ui-order-dependent-tests: the same two order-dependent tests and seeds from TASKS.md:195. File it once, under that key. Both seeded runs still fail on 73465cd (shell.test.tsx: 1 failed | 45 passed; ApplicationDetailsPage.test.tsx: 1 failed | 15 passed). Its acceptance item, running ci-ui or a nightly with sequence.shuffle, is folded into the canonical item.
- **Fix the two order-dependent UI tests exposed by vitest --sequence.shuffle** (not-actionable): Real, but it duplicates bugs-ui-order-dependent-tests: identical tests, seeds and failure messages (TASKS.md:195). File it once, under that key. Both seeded commands were re-run in this pass and still fail (1 failed | 15 passed; 1 failed | 45 passed). Its acceptance items, five shuffled runs and a shuffled CI or scheduled run, fit the canonical issue.
- **Generate the e2e git-server SSH host key at deploy time instead of committing it** (not-actionable): The committed key is a documented, deliberate design decision: test/e2e/fixtures/git-server/manifests/ssh-host-key.secret.yaml header says it is committed on purpose so known_hosts stays stable and strict host-key checking can be tested, and test/e2e/fixtures/README.md:117-118 records the same. test/e2e/fixtures/git-server/known_hosts is consumed by test-scripts.sh:417-435 (byte-exact comparisons), patrol/repositories.yaml and register.sh. .gitleaks.toml path-allowlists it with a reason. The stated harm (scanners flag it) is not fixed by the acceptance: the key stays in git history after removal. TASKS.md:198 only says 'consider' it later. It protects nothing, so there is no security defect.
- **Write an end-to-end GitHub integration guide** (already-done): examples/github/README.md (92 lines, '# GitHub integration walk-through') already covers the end-to-end flow the item says is missing: '## 1. Create the GitHub App' (permissions table, webhook URL and secret, private key), '## 2. Webhook registration checklist', '## 3. Deploy' (SCMProvider with App and check runs, RepositoryCredential, AppProject, pull request ApplicationSet with providerRef, notifications), and '## 4. Lifecycle of a preview' (commit status, PR comment, check run, cleanup on close). docs/guides/examples.md:133-140 links it, and test/examples validates its manifests against a fake GitHub (examples.md:162). The only remaining work is verification against live GitHub, which needs credentials and belongs in a separate item.

## Tracker entries found already done

Entries of `TASKS.md`, the findings files and the parity records that the explorers checked and found done; they got no issue. Prefix: the explorer that checked it.

- [findings] M-1: docs/guides/installation.md 'Without Helm' links deploy/manifests and manifests.md; `grep -n -i planned docs/guides/installation.md` finds nothing
- [findings] M-4 (informational): make ci-generated runs verify-generated, verify-manifests and helm-lint with Helm 4 from hack/ci/tools.lock; no action
- [findings] M-5 (informational): documented in docs/guides/manifests.md:355-360 (kustomize adds metadata.namespace to the ValidatingAdmissionPolicy and its binding; ignored by the API server)
- [findings] M-6: release staging sets image.registry and retags the manifests (hack/release/files.go:43 setImageRegistry, hack/release/main.go:347-368 stage; V-0038: install.yaml retagged to ghcr.io/.../patrol-cd:0.1.0). The in-tree dev manifests stay unqualified on purpose
- [findings] M-7 (informational): layout note for the harness; test/deploy TestStaticManifestsMatchChart uses it
- [findings] DV-1: deploy/helm/patrol-cd/templates/_config.tpl:149 fails the render; ci/invalid/appset-namespaces-without-scm-allowlist.yaml exists
- [findings] DV-6: internal/server/bootstrap_secrets.go:416 initialSecretStale plus deleteStaleInitialSecret; tests TestBootstrapStaleInitialSecret etc.; TestHelmLifecycle step 8 (V-0025, V-0027); confirmed on kind (V-0030)
- [findings] DV-9 (informational): harness counts streaming-list watch as list; no action
- [findings] R2-1: docs/architecture/overview.md:51 says list/watch on every ConfigMap and Secret of the control-plane namespace
- [findings] R2-2: `grep 'never changes existing'` in internal/server/bootstrap_cmd.go and docs/reference/cli-patrolcd.md finds nothing; the bootstrap golden changed together
- [findings] KV-1: internal/reposerver/config.go:207 private _patrol-repo root; root_test.go exists
- [findings] KV-2: test/e2e/fixtures/patrol/register.sh:70-78 labels Secrets with cd.patrol.example/secret-type
- [findings] KV-3: docs/guides/installation.md:337 'Ports are the destination pod's ports' bullet
- [findings] KV-4, KV-5, KV-6, KV-8, KV-9: fixed in hack/kind and the fixtures (V-0029, V-0032 closing run 58/58)
- [findings] KV-7: internal/cli/core/kubecontexts.go:93-101 UserMessage
- [findings] KV-11: matches upstream, no code change needed; only the entry needs closing. Upstream v3.5.5 controller/health.go:25 starts app health at Healthy, and state.go:1034-1051 sets sync Unknown on failedToLoadObjs while health still comes from setApplicationHealth. Patrol internal/health/aggregate.go:56-66 applies the same rule
- [findings] KV-12: hack/kind/lib.sh:170-175 patrol::wait_fixture_host_key; verify-install prints the refusal detail
- [findings] KV-14: internal/server/kube.go:60 waitForCRDs; V-0034 second clean-room run 0 restarts
- [findings] CI-F1: internal/git/repo.go:157 `git remote set-head origin --auto`
- [findings] CI-F2: internal/envtestx/envtestx.go:26 Start; TestStartFailureStopsWhatStarted in hack/ci/must-run.txt
- [findings] CI-F3: internal/controller/integration env.updateApp used at extra_test.go:38,46 and deletion_test.go:183
- [findings] CI-F4 (environment): CI creates /run/sshd; preflight warns
- [findings] CI-F5 (environment): container without --init; no product change needed
- [findings] CI-F6 (developer machine only, git-lfs missing): TestLFS passes on the Linux runner; not a product issue
- [findings] CI-F7..CI-F14: fixed per phase4-findings.md and passing in CI runs 39792963794 and 39814479674 (e.g. internal/apiserver/identity.go:48 ProjectTokenRegistryWithFallback for CI-F14; hack/ci/classify-changes.sh:33 includes .github/actions/ for CI-F12)
- [findings] CI-F15: test/integration/stack/stack.go:269 freeAddr with handedOut (commit efbc535). The integration failure of PR #5 run 39872459611 (TestSyncWavesOrdering, server and notifications-controller both on 127.0.0.1:33199) is this defect: its head ee3c2e5 predates the fix (`git show ee3c2e5:test/integration/stack/stack.go | grep -c handedOut` -> 0; 73465cd -> 6)
- [findings] CI-F16: test/integration/stack/stack.go:524-529,721 stopComponents before the git server closes
- [findings] CI-F17: hack/release/select.go:192-195 selectRelease (release commit plus CI check); tests in select_test.go
- [findings] CI-F18: internal/server/bootstrap_secrets_test.go:666-670 serviceAccountClient uses newBootstrapClient
- [findings] W-8: decided and documented in docs/architecture/components/wiring.md:231 and :364 (static files under /extensions/ served without the proxy header)
- [findings] W-9: deploy/manifests/install.yaml has the /app/config/internal-token (5), reposerver/tls, ssh, tls, gpg/source and /home/patrol/params mounts plus the patrol-controller Service on port 8090; installed and verified on kind (V-0032)
- [findings] W-10: internal/server/core.go:152 NewCoreHandler exported
- [findings] W-11: matrix regenerated; `make ci-static` runs ci-parity (hack/ci.mk:58)
- [findings] UI-1..UI-6: resolved in api/openapi/openapi.json (StoredApplication present; x-patrol-webhook-receivers present)
- [findings] I-1: internal/server/controller_refresh_test.go:17 TestClusterCacheRefreshRequested
- [findings] I-2: internal/apiserver/core/account.go:315 waitAccount; tests account_test.go:257,280
- [findings] O-4 (informational): correctness held under injected conflicts; no action
- [findings] runs/2026-10-22-ci-local-validation.md 'Not done' (Linux -race, envtest on Linux, x86-64): done on GitHub ubuntu-24.04 x64 (V-0035 run 39792963794, unit -race 99 packages)
- [findings] runs/2026-10-19-image-build.md 'linux/amd64 not built': the CI 'Image (linux/amd64)' job builds it (V-0035, V-0036)
- [findings] V-UICAE-03 parity -check non-determinism: fixed (V-0007)
- [findings] V-0013 UI 'Errors 1 error' flake: fixed (V-0014, ui/src/test/guards.ts)
- [backlog] TASKS.md T0.4 parity inventory: 10 area files in docs/parity/areas with 1,457 feature headings (sum of grep -c '^### ' = 141+138+131+221+124+132+155+135+109+171), matrix generated; TASKS.md:47 'W1 research DONE'
- [backlog] TASKS.md T0.6 CRD types + CEL + admission tests: api/v1alpha1/*_types.go (36 XValidation rules), api/v1alpha1/admission_test.go envtest; remaining CRD validation defects filed as backlog-crd-validation-defects
- [backlog] TASKS.md T0.7 Makefile/CI/verify-generated: .github/workflows/ci.yml 9 jobs passed on GitHub (V-0035, V-0036); only the golangci-lint config remains (backlog-golangci-config)
- [backlog] TASKS.md T1.1-T1.17 Phase 1 libraries: internal/{settings,rbac,auth,project,sandbox,git,helm,oci,render,reposerver,cmp,clusters,cache,diff,health,lua,tracking,sync,notifications} and pkg/scm (github, gitlab, gitea, gogs, bitbucketcloud, bitbucketserver, azuredevops, awscodecommit + conformance + fake) exist with tests; verified in V-0004, V-0006, V-0010, V-0016 (83/83 packages); parity remainders (e.g. TEN-114 Dex config, REPO-071 GPG strict) are parity features, missing overlays filed as backlog-parity-backfill-*
- [backlog] TASKS.md T2.1-T2.7 controllers and services: internal/controller/{app,operation,sharding,treeapi}, internal/appset, internal/notifications/controller, internal/hydrator, internal/commitserver, internal/webhook with tests; internal/controller/integration 35 envtest tests; V-0006, V-0010
- [backlog] TASKS.md T3.1-T3.4 API, CLI, UI: TASKS.md:200-202 'PHASE 3 COMPLETE', V-0016 full-tree gate (83/83 Go packages, UI 912 tests, lint 0 issues)
- [backlog] TASKS.md T5.2 envtest integration tests: internal/controller/integration (35 tests) and test/integration (lifecycle, recovery, stack; 16/16 passed V-0015)
- [backlog] TASKS.md T5.6 failure injection (as listed): test/integration/recovery_test.go TestControllerRestartMidSync:55, TestCacheLossMidOperation:117, TestDestinationUnreachable:168, TestRepoServerDown:249, TestAPIConflictInjection:302 (envtest; on-cluster faults are in backlog-e2e-harness-lifecycle)
- [backlog] TASKS.md Requirements carried: /extensions.js served: internal/server/server_routes.go:33 PathExtensionsJS, internal/server/routes_test.go:70,81; /extensions/* assets served (integration-findings W-8)
- [backlog] TASKS.md Cleanup backlog: regenerate api/openapi/openapi.json: regenerated (V-0012) and checked in CI by make ci-openapi (hack/ci.mk:98 genopenapi -check)
- [backlog] TASKS.md Pre-push secret scan: gitleaks runs in CI (ci.yml:94 make ci-gitleaks)
- [backlog] TASKS.md Blockers: Docker Desktop disk full resolved 2026-10-19 (V-0028); commit signing resolved
- [backlog] integration-findings W-9 deployment mounts: chart mounts patrol-internal-token and profiler params (deploy/helm/patrol-cd/templates/_helpers.tpl:219,409-430)
- [backlog] integration-findings W-10 core mode: internal/cli/core uses server.NewCoreHandler (V-CLICOREMODE-01/02)
- [backlog] integration-findings W-11 matrix regeneration: make ci-parity runs hack/parity -check (hack/ci.mk:103)
- [backlog] integration-findings UI-1..UI-6 server side resolved in openapi.json (V-UIAPI-01); remaining UI cleanups filed as backlog-ui-drop-api-compensations
- [backlog] integration-findings I-1, I-2: fixed with regression tests (TestClusterCacheRefreshRequested, TestAccountTokenVisibleWhenReturned)
- [backlog] api-repositories.md gap 'scmproviders RBAC constant pending': internal/rbac/constants.go:40 ResourceSCMProviders, builtin-policy.csv:21,54-56
- [backlog] git.md gap 'MaybeAskpassMain must be wired': internal/server/helpers.go:28 git-askpass helper
- [backlog] webhook.md gap 12 'Matrix evaluation needs GeneratorEvaluator': wired at internal/server/server_api.go:252 and internal/server/appset.go:177
- [backlog] notifications.md gap 'keys.go must declare annotation keys': api/v1alpha1/keys.go:172-182
- [backlog] notifications-controller.md gap 'CLI patrol admin notifications not built': internal/cli/admin/notifications.go:39 with notifications_test.go
- [backlog] phase4-findings M-1: resolved (installation.md links deploy/manifests)
- [backlog] phase4-findings M-6 (image registry for static manifests/chart): handled at release time by hack/release/files.go:43 setImageRegistry and :74 retagManifest
- [backlog] phase4-findings DV-1 and DV-6: fixed with tests (chart _config.tpl render failure; bootstrap stale initial Secret, TestBootstrapStaleInitialSecret, TestHelmLifecycle step 8)
- [backlog] phase4-findings R2-1, R2-2: done (overview.md row; bootstrap help text and golden)
- [backlog] phase4-findings KV-1, KV-2, KV-4..KV-10, KV-12, KV-14: fixed or documented per phase4-findings.md:63-76
- [backlog] phase4-findings CI-F1..CI-F5, CI-F7..CI-F18: fixed per phase4-findings.md:88-105 (CI-F6 is the developer machine's missing git-lfs, not an issue per lead)
- [backlog] TASKS.md T4.1-T4.4: ticked [x] with evidence V-0028..V-0033; open remainders filed (backlog-supply-chain-reports, backlog-kind-verify-other-profiles, backlog-examples-on-installation)
- [parity-a] TASKS.md T1.6 internal/git: implemented with real-git tests (internal/git/http_test.go, ssh_test.go, client_test.go); targeted run `go test -run 'TestHTTPBasicAuthViaAskpass|TestSubmodules|TestWithRetry|TestHTTPBearerToken' ./internal/git/` -> ok
- [parity-a] TASKS.md T1.7 internal/helm + internal/oci: implemented with tests (internal/helm/index_test.go, pull_test.go; internal/oci/pull_test.go TestFetchHelmChart)
- [parity-a] TASKS.md T1.8 internal/render and T1.9 internal/reposerver + internal/cmp: implemented; reposerver/cmp overlays mark the features implemented (docs/parity/status/reposerver.yaml, cmp.yaml) with tests such as internal/reposerver/helmoci_test.go TestOCISource
- [parity-a] TASKS.md T1.11 internal/cache: implemented with envtest coverage (internal/cache/envtest_test.go TestEnvtestCRDLifecycle, TestEnvtestNamespaceRestricted); targeted run ok
- [parity-a] TASKS.md T1.12 internal/diff: implemented (legacy, structured-merge, server-side, ignore rules, masking); targeted run `go test ./internal/diff/ -run ...` -> ok
- [parity-a] TASKS.md T1.13 internal/health + internal/lua: implemented; 111 customization groups embedded; targeted run ok (internal/health 0.685s, internal/lua 1.499s)
- [parity-a] TASKS.md T1.14 internal/tracking and T1.15 internal/sync: implemented and tested (docs/parity/status/tracking.yaml, sync.yaml; internal/sync/envtest_test.go)
- [parity-a] TASKS.md T2.1/T2.2 Application and Operation reconcilers: implemented (internal/controller/app, internal/controller/operation; internal/controller/integration tests TestManualSyncLifecycle, TestRestartMidOperation, TestPreDeleteHook)
- [parity-a] TASKS.md T2.6 source hydrator + commit-server: implemented and wired (internal/server/commands.go:336,445,630; internal/hydrator/envtest_test.go TestEnvHydrationLifecycle)
- [parity-a] TASKS.md T2.7 webhook ingestion: implemented (internal/webhook; docs/parity/status/webhook.yaml REPO-114 implemented, envtest TestEnvtestWebhookRefresh)
- [parity-a] TASKS.md 'Carried-over requests: treeapi handler serverSideDiff should also enforce target/live identity match': done at internal/controller/treeapi/handler.go:358-364 (name/namespace/group/kind mismatch -> invalid)
- [parity-a] docs/parity/status/hydrator.yaml APP-113 'Missing: controller/cmd wiring of the flag and commit.server address, settings API hydratorEnabled, UI gating': done (internal/server/commands.go:336,445; internal/server/controller.go:117; internal/apiserver/core/settings.go:70; ui/src/pages/create/ApplicationCreateForm.tsx:395)
- [parity-a] docs/parity/status/render.yaml SRC-077/SRC-085/SRC-071 'cmp-server (internal/cmp) not written': done (internal/cmp/server.go, docs/parity/status/cmp.yaml implemented, internal/cmp/server_test.go TestGenerateEndToEnd)
- [parity-a] docs/parity/status/render.yaml SRC-039 'removing the marker and vendored charts on revision change ... not yet written': done by checkout `git clean -ffdx` (internal/git/repo.go:265)
- [parity-a] docs/parity/status/clusters.yaml REPO-106 'patrol cluster add command is not implemented': done (cli-core verified-simulated, internal/cli/cluster_test.go TestClusterLifecycle)
- [parity-a] docs/parity/status/clusters.yaml REPO-096 'invalidate-cache API and cache resync are elsewhere': done (internal/server/controller.go:505-520, internal/server/controller_refresh_test.go:43)
- [parity-a] docs/parity/status/sync.yaml SYNC-006 'CLI <n> resources require pruning exit check is not implemented': done (internal/cli/app/sync.go:397)
- [parity-a] docs/parity/status/api-applications.yaml SYNC-004 'CLI flags pending': done (internal/cli/app/sync.go:126-127 --revisions/--source-positions)
- [parity-b] TASKS.md T1.2 internal/rbac (unchecked box): implemented with grammar, glob/regex, deny precedence, built-in roles and scopes, plus property and complexity tests; re-verified in V-0004; targeted `go test ./internal/rbac/` run passed this session
- [parity-b] TASKS.md T1.3 internal/auth (unchecked): local accounts, bcrypt, JWT sessions, API tokens, revocation store, lockout, OIDC/PKCE, Dex helpers in internal/auth and internal/auth/oidc; V-0004; `go test ./internal/auth/oidc/` passed this session (Azure options and IdP refresh filed separately)
- [parity-b] TASKS.md T1.17 internal/notifications (unchecked): expr triggers, templates and helpers, 21 services (services/registry.go:9-58), subscriptions, dedup, self-service; V-0004
- [parity-b] TASKS.md T2.4 ApplicationSet controller (unchecked): internal/appset, wired by internal/server/appset.go (generators, repo-server, credentials, webhook evaluator, params)
- [parity-b] TASKS.md T2.5 Notifications controller (unchecked): internal/notifications/controller plus the patrolcd command at internal/server/commands.go:588-616; V-0006
- [parity-b] TASKS.md 'Requirements carried to later tasks: Server must serve /extensions.js': done, internal/server/server_routes.go:33 PathExtensionsJS route with per-file wrapping
- [parity-b] TASKS.md 'RUNNING: rbac regex-mode project-policy DoS fix': done, internal/rbac/complexity_test.go TestAdversarialProjectPolicyCannotStallEnforce, TestProjectPolicyRejectsCommasInFields
- [parity-b] TASKS.md Phase 3b 'CLI admin (incl. Argo CD import/migration conversion)': done, internal/cli/admin/argocdimport.go and argoconvert.go with tests
- [parity-b] docs/parity/status/notifications-controller.yaml NOTIF-104/106/108 'CLI not built': done, cli-admin.yaml marks them verified (internal/cli/admin/notifications.go:39-484); only the status note is stale (covered by the area-09 chore)
- [bugs] CI-F15 / TestSyncWavesOrdering failure in run 39872459611 (Dependabot PR head ee3c2e5): the log shows the server and the notifications-controller both serving metrics on 127.0.0.1:33199, then `connection refused` on login. This is the same port collision as CI-F15. The fix efbc535 (freeAddr never hands out an address twice, test/integration/stack/stack.go:257-289) is not in ee3c2e5 but is on main, and the integration suite passed on main in run 39876114735
- [bugs] CI-F18 / TestBootstrapRBACContract failure in run 39869163105 (head b668190, `rate: Wait(n=1) would exceed context deadline`): fixed by 840c020 (the test uses newBootstrapClient), which is an ancestor of 73465cd
- [bugs] CI-F16 / TestOnboardingThroughAPI failure in run 39838778892 (head efbc535): fixed on main (NewGitServer stops components first); the integration suite passed in runs 39872667954 and 39876114735
- [bugs] CI-F1..CI-F14 (phase4-findings.md): all marked fixed with tests, and none recurred in the runs I read (39872474264, 39872459611, 39869163105, 39876114735)
- [prod] phase4-findings.md M-1: installation.md 'Without Helm' (installation.md:570-590) links deploy/manifests and manifests.md; the 'planned' wording is gone
- [prod] phase4-findings.md DV-1: deploy/helm/patrol-cd/templates/_config.tpl:149 fails the render when applicationSetNamespaces is set without an SCM allow-list
- [prod] phase4-findings.md DV-6: internal/server/bootstrap_secrets.go:416 initialSecretStale and deleteStaleInitialSecret regenerate a stale initial admin password
- [prod] phase4-findings.md KV-3: docs/guides/installation.md:337 documents that NetworkPolicy ports are the destination pod's ports
- [prod] phase4-findings.md KV-14: internal/server/kube.go:60 waitForCRDs makes the components wait for the Patrol CRDs to be served
- [prod] phase4-findings.md CI-F17: release.yml plan job runs `go run ./hack/release select -github-output`, which chooses the CHANGELOG commit and requires its CI run to have passed
- [prod] TASKS.md 'Requirements carried' /extensions.js: served by patrol-server (internal/server/server_routes.go:32-33, --extensions-dir in commands.go:284)
- [prod] TASKS.md T4.1 'Not done: linux/amd64': the CI job 'Image (linux/amd64)' (ci.yml:309-310, hack/ci.mk:200-207) builds, layout-checks and smoke-tests amd64 on every code push and passed in run 39876114735 (multi-arch is still open)
- [prod] matrix OPS-109 (not-started): security headers are implemented, internal/apiserver/rest/middleware.go:237-249 with params server.x.frame.options and server.content.security.policy (params_table.go:160-161)
- [prod] matrix OPS-098 (not-started): HTTP-to-HTTPS redirect on the TLS port is implemented (internal/apiserver/tls.go:176-178 splitListener, :283-288 httpsRedirect)
- [prod] matrix OPS-122 (not-started): CLI core mode is implemented (docs/reference/cli-core-mode.md:13-15, `patrol --core`)
- [prod] matrix OPS-038/OPS-039 (not-started): controller.status.processors, controller.operation.processors and controller.kubectl.parallelism.limit exist (internal/settings/params_table.go:121-122,136)
- [prod] matrix OPS-144 code part: `patrol admin export` and `import` are implemented with envtest tests (internal/cli/admin/export.go:28, backup_test.go:121-600); only the procedure and the DR drill are missing (filed)
- [prod] phase4-findings.md M-4: CI installs Helm 4 and `make ci-generated` runs verify-generated, verify-manifests and helm-lint (hack/ci.mk ci-static)
- [obs] matrix OPS-070 (listed not-started): patrol_app_reconcile_duration_seconds in internal/controller/core/metrics.go:47-51, asserted by internal/controller/integration/extra_test.go:247 TestSyncMetrics
- [obs] matrix OPS-071 (listed not-started): patrol_app_sync_total / patrol_app_sync_duration_seconds_total in internal/controller/core/metrics.go:39-46,93-102, asserted by extra_test.go:218-240 TestSyncMetrics
- [obs] matrix OPS-072 (listed not-started): patrol_app_k8s_request_total via WrapConfig in internal/controller/core/metrics.go:57-60,104-131, asserted in extra_test.go:247 and test/integration/recovery_test.go (labels narrower than upstream: no per-app labels)
- [obs] matrix OPS-080 (listed not-started): patrol_appset_info/owned_applications/reconcile/labels and --metrics-applicationset-labels in internal/appset/metrics.go and internal/server/commands.go:532, tested by internal/appset/metrics_test.go:15 TestMetrics
- [obs] matrix OPS-084 (listed not-started): commit-server metrics with repo/request_type/response_type labels in internal/commitserver/metrics.go:37-58 (tested in internal/commitserver/run_test.go TestRun); notifications metrics NOTIF-057/058 implemented and tested. Only argocd_commitserver_userinfo_request_duration_seconds has no equivalent
- [obs] matrix OPS-093 (listed not-started): ApplicationSet events created/updated/Deleted/Updated-finalizer in internal/appset/apply.go:179,383,433, covered by internal/appset/reconciler_test.go
- [obs] matrix OPS-056 profiler: implemented for server, controller, appset, notifications, commit-server (internal/server/observe.go:124-142, TestObservability); only the repo-server part is open (filed as obs-reposerver-pprof-healthz)
- [obs] CLI debug output: --loglevel debug logs every API request (method, host, redacted path, status, requestId, duration) via internal/cli/cmdutil/debuglog.go:16-60; no item needed
- [obs] API access logging with request IDs and audit logging exist: internal/apiserver/rest/middleware.go:122-208 (RequestIDs, AccessLog), internal/apiserver/rest/audit.go:225,274 (audit log line + Event + patrol_server_audit_events_total)
- [testing] CI-F15 / TestSyncWavesOrdering failure in run 39872459611 (Dependabot PR head ee3c2e5, 'connection refused' with two components on 127.0.0.1:33199): the freeAddr fix efbc535 is on main (git merge-base: in 73465cd, not in ee3c2e5); no new item
- [testing] CI-F18 TestBootstrapRBACContract rate-limiter failure (run 39869163105): fixed by 840c020, which is on main; later runs pass
- [testing] TASKS.md T5.2 envtest integration tests (controllers): exist and run in CI: internal/controller/integration (34 tests incl. autosync, deletion, operations, restart), envtest suites in 25 packages incl. appset, hydrator, notifications/controller, sharding, webhook; -coverpkg from internal/controller/integration gives 76.2% of operation/app/core
- [testing] TASKS.md T5.1 unit + property tests: largely done: every non-main, non-test-support package has tests except dead internal/controller (filed); rapid property tests in ~50 test files; sandbox fuzz targets; residual gaps filed (fuzz targets for parsers, coverage reporting)
- [testing] TASKS.md T5.6 at envtest level: test/integration/recovery_test.go TestControllerRestartMidSync, TestCacheLossMidOperation, TestDestinationUnreachable, TestRepoServerDown, TestAPIConflictInjection pass in the CI integration job (runs 39872667954, 39876114735); only the kind-level part is filed
- [testing] Allowed CI skips (hack/ci/allowed-skips.txt): all 125 skips in the unit job of run 39872667954 are pkg/scm conformance checks for capabilities a provider does not declare or upstream does not have (archived flag, PR labels, body-signing on token-auth webhooks), each with a reason in the provider test and coverage in a provider-specific test; no capability is implemented without being declared (only github implements commit statuses/check runs). No item
- [testing] TASKS.md cleanup 'Regenerate api/openapi/openapi.json after all API services land': `make ci-openapi` (genopenapi -check) runs in the static job and passes
- [testing] TASKS.md requirement 'Server must serve /extensions.js': internal/server/routes_test.go:58 TestExtensionsBundle and :87 TestExtensionStaticAssets
- [critic] TASKS.md 'Requirements carried to later tasks' /extensions.js: served (internal/server/server_routes.go:33 PathExtensionsJS; --extensions-dir at internal/server/commands.go:284)
- [critic] TASKS.md 'Regenerate api/openapi/openapi.json': done; CI Static checks (genopenapi -check) passed on main run 39876114735
- [critic] TASKS.md T0.4 'Parity inventory (9/10 done)': stale, docs/parity/matrix.md lists all 10 areas (10-operations: 171 features, total 1457)
- [critic] TASKS.md T1.1-T3.4 unchecked boxes: done per the 'PHASE 3 COMPLETE (2026-10-17)' section and V-0016
- [critic] integration-findings.md UI-1..UI-6: resolved in the generated OpenAPI ('Resolution of UI-1 ... UI-6'); only optional UI cleanups remain (candidate findings-ui-contract-compensations-cleanup)
- [critic] integration-findings.md W-8 (static UI extension assets): implemented, server serves /extensions/<path> from --extensions-dir (internal/server/commands.go:284)
- [critic] phase4-findings.md M-1, DV-1, DV-5, DV-6, R2-1, R2-2, KV-1..KV-9, KV-12, KV-14, CI-F1..CI-F3, CI-F7..CI-F18: marked fixed/resolved with regression tests in the table
- [critic] CI run 39872459611 (PR #5, TestSyncWavesOrdering 'connection refused'): this is CI-F15 (freeAddr port reuse), not a new flake. The head ee3c2e5 does not contain fix efbc535 (`git merge-base --is-ancestor efbc535 ee3c2e5` is false), and the log shows the notifications-controller and the server both on 127.0.0.1:33199
- [critic] CI run 39869163105 (TestBootstrapRBACContract rate limiter): CI-F18, fixed on the PR #8 branch before merge
- [critic] Parity SYNC-006 'CLI <n> resources require pruning not implemented': implemented at internal/cli/app/sync.go:397 (status stale; covered by area 05 re-audit)
- [critic] Parity APP-065/SYNC-052 'PostDelete hook execution is the controller's': implemented internal/controller/app/deletion.go:153-166, tests TestPreDeleteHook/TestPostDeleteHook (internal/controller/integration/deletion_test.go)
- [critic] Parity ASET-128 'serving appset metrics not wired': wired, internal/server/appset.go:128 serves the registry (Gatherer: reg)
- [critic] docs/architecture/components/hydrator.md:190 / APP-113 'hydratorEnabled in the settings API not implemented': implemented internal/apiserver/core/settings.go:70 and ui/src/pages/settings/repositories/RepositoriesPage.tsx:307 (stale doc; covered by backlog-stale-component-docs)
- [critic] Sync impersonation (TEN-030/SYNC-093) and globalProjects: wired (internal/controller/operation/execute.go:214, internal/settings/parse_cm.go:542)

## Coverage notes of the explorers

### findings

Read in full: docs/verification/phase4-findings.md (M-1..M-7, DV-1..DV-9, R2-1..R2-8, KV-1..KV-14, CI-F1..CI-F18), docs/verification/integration-findings.md (W-1..W-15, UI-1..UI-6, I-1, I-2, O-1..O-4), docs/architecture/threat-model.md (T1-T23), docs/verification/log.md (V-0001..V-0039), and the open, limits and not-done parts of runs/*.md: image-build NOT done, kind-validation Not covered and KV table, ci-local-validation Not done, github-ci-runs Not shown, chart-static, manifests-static and deploy-validation NOT EXECUTED, review-round2 'Not fixed', ui-flake follow-up, bootstrap-dv6 NOT EXECUTED, kind-environment K-table.

Every open entry was checked against the code at 73465cd with grep and sed. For the threat model, each row's mitigation was spot-checked in code (T1, T2, T4-T7 and T9-T23 are implemented); test mapping is the gap.

The upstream clone at the given scratchpad path is empty (0 bytes, no .git), so upstream behavior was read read-only with `gh api repos/argoproj/argo-cd/contents/...?ref=v3.5.5`: server/server.go, cmd/argocd/commands/headless/headless.go, controller/state.go, controller/health.go, cmd/argocd-repo-server/commands/argocd_repo_server.go.

CI and runs were read with gh run list/view and gh api GET only. No tests were run, no files changed.

Not covered: the per-feature parity matrix (other explorers); a runtime reproduction of W-15 (inferred from code, as the finding says); the HA behavior of account tokens across patrol-server replicas (I-2 waits only on the local snapshot; not verified, not filed).

Outside scope, one line each:
- Release PR run 39876114689 failed on a missing repo setting and the missing 'release' label (filed above because it is evidence for V-0038).
- CI run 39876114735 at 73465cd had only Unit tests still running when checked; all other jobs succeeded.
- The Phase 5 scenario WIP exists only in the local ref refs/patrol/snapshots/20261016T214116Z, never pushed (V-0017); it could be lost.
- test/examples shows 33 API server warnings for the unqualified finalizer resources-finalizer.cd.patrol.example (V-0033 calls it 'cleanup backlog'); upstream has the same warning by design (api/v1alpha1/keys.go:13-20).
- CH-1 for ConfigMap/Secret name length was left unfixed by decision (review-round2).
- The `server` help default for content-security-policy ('frame-ancestors 'self';') differs from the effective apiserver default ('default-src 'self'; frame-ancestors 'self''), because the param default is treated as unset; worth a look by the apiserver explorer.

### backlog

Read in full: TASKS.md (all 232 lines: current work, STOP rule, gate, resume state, blockers, T0.1-T6.4, completed log, requirements carried, cleanup backlog, remaining Phase 3 checklist, Phase 4 closeout and open items), docs/verification/phase4-findings.md, integration-findings.md, log.md (V-0001..V-0039, truncated lines), the 'Gaps/Known gaps/Not implemented/Differences and gaps' sections of all 42 docs/architecture/components/*.md, overview.md headings, all 13 ADRs, threat-model.md, sync-state-machine.md restart section, README.md, and the limitation sections of docs/guides/{installation,manifests,images,ci,releasing}.md. Checked every T-item and open line against code (git grep, file reads), docs/parity/status/*.yaml and matrix.md; computed that all 435 not-started features have no overlay entry. Read-only commands only; ran two targeted vitest files with --no-cache and recorded shuffle seeds (both still fail); gh run view/list, gh api GET for cache usage and workflow permissions, gh label list. git status stayed clean. Not covered: docs/guides/local-development.md and examples.md read only by grep; I did not verify every component-doc 'Differences from upstream' entry (deliberate differences), and pure parity features named in gap sections (Dex config generation TEN-114/OPS-152, /auth/logout TEN-108, GPG strict REPO-071, REPO-128 cache warming, API-003 fields projection, TEN-125/126 OIDC refresh/Azure, reposerver.git.lsremote.parallelism.limit and git.request.timeout accepted but ignored, project read lock, kubelogin devicecode/interactive) are left to the parity explorers. Outside my scope but important: (1) CI on main is red from TestDynamicDistributionEnvtest (internal/controller/sharding/envtest_test.go:254) and CI run 39876114735 was still in progress; (2) security residuals stated in docs: AppProject destination escalation checks for ?/[...] globs and deny exceptions (api-clusters-projects.md:217-226), no Lua table memory limit (lua-health.md), whole-Secret rewrite per token revocation and no collapse metric (auth.md); (3) pkg/apiclient/links.go comments could not be recovered after an overwrite (api-appsets-misc.md:252-256); (4) notifications GitHub service does not use pkg/scm (decision with the pkg/scm owner, notifications.md).

### parity-a

Read: docs/parity/matrix.md (all APP/SRC/REPO/DIFF/SYNC rows), every not-started and in-progress definition I needed from docs/parity/areas/01-05, all docs/parity/status/*.yaml (flattened with yq into per-ID contributor lists), docs/parity/aliases.yaml, hack/parity/main.go, TASKS.md, and the design docs compare.md, sync.md, sync-state-machine.md and helm-oci.md. I checked a sample of the code behind every work package: internal/{compare,controller/app,controller/operation,clusters,cache,diff,health,lua,git,helm,oci,reposerver,apiserver/services/{applications,repositories,clusters},server,cli/app,cli/admin}, plus ui/src/pages and api/v1alpha1.

Checks I ran:
- A script that resolves each of the 1490 test references in scoped overlays to an existing file and test function. No misses.
- A search for implemented entries with no tests. None found.
- Targeted tests, all ok: internal/health, internal/lua, internal/diff, internal/git, internal/cache.

Limits:
- The upstream clone at scratchpad/upstream/argo-cd is empty (empty directories, 0 files). Upstream behavior comes from the area docs, which cite upstream line links. The claim in the live-event issue that upstream collects apps of both old and new objects rests on DIFF-087's text.
- I did not run envtest-based controller or API tests, and did not inspect every one of the roughly 230 stale-status IDs individually. The audit issues list them, with sampled evidence for each class.
- Area 04 per-ID notes for DIFF-089..100 (already implemented) were not re-checked.

Outside my scope, worth a look:
- internal/controller/run.go (292 lines, `controller.Run`) is not imported anywhere. The controller is wired in internal/server/controller.go. The dead copy lacks the invalidate-cache handling and is still cited as impl for APP-067 in docs/parity/status/controller.yaml:147.
- TEN-010 (canonical of APP-068): sourceNamespaces regex uses RE2, so the documented lookahead example `/^((?!not-allowed).)*$/` never matches (docs/parity/status/project.yaml note). This is a parity gap for the area 07 explorer.
- internal/sync/keys.go:32 declares `cd.patrol.example/operation` locally. CLAUDE.md requires keys in api/v1alpha1/keys.go, and sync.md lists this as an open request.
- No orphaned-resources or cluster-info metrics exist. The observability explorer may want both.

### parity-b

Read: docs/parity/matrix.md (summary and rows for areas 06-10; extracted all 363 not-started/in-progress rows), docs/parity/status/README.md, every docs/parity/status/*.yaml entry for those IDs (parsed into a table), feature definitions in docs/parity/areas/06-10 for each work package, TASKS.md, phase4-findings.md DV-2, docs/guides/installation.md (Dex, HA, upgrades), releasing.md, the auth/apiserver/notifications/project/ui component docs, and hack/parity/main.go.

Code checked, by sampling each package: internal/server (appset, notifications, commands, runtime, components, server_api, server_routes, core), internal/appset, internal/auth (+oidc), internal/rbac, internal/apiserver (core/sso, settings, static, openapi_doc, rest), internal/notifications (+services, catalog, controller), internal/clusters/status.go, internal/controller (sharding/status.go, core/params.go, app), internal/compare, internal/settings (params_table, keys, oidc), internal/cli/admin, ui/src (pages/auth, help, applications, shell), the chart (dex, notifications-controller, values) and api/openapi/openapi.json.

Ran (read-only):
- targeted go tests for internal/rbac, internal/auth, internal/auth/oidc, internal/notifications and internal/notifications/services; all printed ok
- a script that checks cited paths and test functions in all status files; it found 3 broken ASET citations

Not covered:
- I did not open every one of the 1457 feature definitions. Per-ID claims in the chores rest on code plus test existence, not on a line-by-line comparison with upstream.
- The scratchpad upstream clone is partial (gitops-engine, reposerver, test, ui, util; not a git checkout), so upstream behaviour comes from the area docs.
- The notifications-engine source is not available locally.

Left to the observability explorer:
- area 10: OPS-065..084, OPS-085..094 (incl. OPS-091 --enable-k8s-event, which also closes TEN-052), OPS-168..171, OPS-075
- other areas: ASET-128, ASET-129, NOTIF-056, API-008 (tracing spans)

Outside my scope but important:
1. settings.yaml has 140 'Settings part only' entries (122 in-progress), which also hold back APP-, SRC-, REPO-, DIFF- and SYNC- features in areas 01-05.
2. APP-001 (Application CRD) has no status entry, the same pattern as ASET-001/TEN-001.
3. REPO-096: the cluster cache rebuild on cd.patrol.example/refresh is 'the controller's' per its note and was not checked.
4. TASKS.md keeps the deferred finalizer rename and the Phase 5 scenario WIP in snapshot refs/patrol/snapshots/20261016T214116Z (appset, auth, notifications, tenancy, extensions scenarios); those belong to the e2e backlog.
5. DV-3, DV-4 and DV-5 (chart documentation findings) are still listed as open in phase4-findings.md.

### bugs

CI: I read the failed jobs of runs 39872474264 (unit: sharding), 39876114689 (Release PR), 39872459611 (integration, CI-F15 signature), 39869163105 (unit, CI-F18), 39838778892 (integration, CI-F16), and the cancelled runs' job states. I waited for run 39876114735 (73465cd) to finish: it failed only in Unit tests (TestClientServerRoundTripProperty), and every other job passed, including integration, deploy and kind.

Lead's sharding diagnosis: confirmed by reading lease.go and envtest_test.go and by an overlay probe. I used `go test -overlay` with test files in the scratchpad, so no repository file changed.

Flaky tests: I read phase4-findings.md (CI-F1..F18, KV, DV, R2), integration-findings.md, log.md and the run records. I reproduced both UI shuffle-seed failures and the rapid seed. My local rapid reproduction wrote a failfile under internal/apiserver/rest/testdata/; I deleted it, and `git status --porcelain` is empty.

Code sweep of the non-test Go under internal/, cmd/ and pkg/ (744 files, about 185k lines):
- panic(): only re-raises of http.ErrAbortHandler plus test harness helpers.
- Discarded errors: reviewed every `_ =` and `_, _ =` site. All are benign: writes to the response, drains, Flush, best-effort cleanup, and appset setCondition after an already-logged error.
- 501 / not implemented: only upstream's 'hydrator is disabled'.
- Goroutines: reviewed all 57 launches; each has a stop path (context, Close, WaitGroup).
- context.Background(): only in predicates, shutdown, metrics collectors and log calls.
- io.ReadAll: every read of untrusted input is bounded. The OIDC userinfo read is unbounded but comes from the configured IdP.
- Secrets in logs: none found; Repository, commit request and helm command logging is redacted.
- Request bodies: all use MaxBytesReader or an in-flight budget.
- I also ran an AST scan for defer-in-loop, split indexing and single-value type assertions. That scan found the duck-type panic.
- I ran golangci-lint with bodyclose, nilerr, contextcheck and errorlint. Every finding I checked was a false positive or intentional.

UI: read the floating promises, mutateAsync, JSON.parse sites and query error states. No defects found besides the order-dependent tests.

Not run: whole suites, multi-node clusters (the HA rollout stall rests on reading the rendered objects), and upstream applicationset/controller code (not in the local clone). Near the end, the permission classifier refused a few read-only commands (git log of roundtrip_test.go, a sed of GOROOT); I used Read instead.

Outside my scope but worth a look:
- KV-11: health Healthy with sync Unknown on ComparisonError; probably the same as upstream, not confirmed.
- KV-13: leader election lost on API timeouts.
- DV-2: the core install has no default AppProject.
- W-12: apiserver.buildTLSConfig does not normalize HTTP/2 ciphers itself; only the internal/server wiring does.
- W-15: repo-server ECDSA self-signed certificate with an RSA-only TLS 1.2 cipher list.
- GPG 'strict' source-integrity mode fails closed as unsupported (internal/reposerver/integrity.go:20-22).
- Finalizer names are not domain-qualified (deferred rename).
- R2-4 and R2-8: musl COPYRIGHT and SBOM not run.
- Residual integration port risk: freeAddr does not coordinate with envtest's own port picks; not observed since CI-F15.
- Undetermined CRD-deletion timeout in a 2026-10-18 deploy closeout run; mitigated with longer waits and diagnostics, not recurred.

### prod

Read for the prod dimension: TASKS.md (all of it); docs/verification/phase4-findings.md (M, DV, R2, KV and CI-F entries); integration-findings.md (W, O and I entries); the Helm chart (values.yaml global, image, bootstrap, internalTLS, server, controller, repoServer, dex, networkPolicy; values.schema.json; templates _helpers.tpl, _config.tpl, rbac/_rules.tpl, dex, repo-server, controller statefulset, prometheusrule); deploy/manifests README and profiles; Chart.yaml; Dockerfile base pins; hack/image.mk, hack/ci.mk, hack/release.mk, hack/release/files.go; .github/workflows/release.yml, release-pr.yml and the ci.yml image job; dependabot.yml; docs/guides installation, manifests, images and releasing; ADR-0002, 0006 and 0008; the bootstrap and wiring component docs; the parity matrix 10-operations rows and their area semantics; internal/server (token.go, tlsutil.go, observe.go, commands.go, server.go readiness, group.go); internal/apiserver (server.go shutdown and limits, tls.go, rest csrf, audit and ratelimit); internal/clusters/status.go and the clusters API store; internal/cli/admin export and import.\n\nCommands run (all read-only): gh run list, gh run view 39876114689 --log-failed (Release PR failure cause: Actions may not create pull requests and the release label is missing), gh run view 39876114735 (CI on 73465cd was still running unit and kind), gh api GET for vulnerability-alerts (404), automated-security-fixes (disabled) and actions/permissions/workflow (can_approve false), gh label list, gh secret list, helm template of ci/ha.yaml into the scratchpad, yq on deploy/manifests/install.yaml. No tests were run, no image was built, nothing was changed.\n\nNot covered: the upstream clone in the scratchpad is partial (gitops-engine, reposerver, test, ui and util only; no manifests, .github or docs), so upstream comparisons rely on docs/parity/areas/10-operations.md. I did not audit every template line, Helm 3 behaviour, measured resource usage, or the commit-server and hydrator runtime in depth. Metrics, logging, tracing and events were left to the observability explorer.\n\nSeen outside my scope:\n- CI on main is red on TestDynamicDistributionEnvtest (CI explorer, CI-F19).\n- matrix APP-074 notes that the controller's prune decision does not yet require IsManagedBy plus AppProject checks (possible controller safety gap).\n- KV-11: a never-compared Application reports Healthy.\n- KV-13: the ApplicationSet and notifications controllers exit on leader election loss under API latency; the cause is unknown and upstream behaves similarly.\n- integration-findings O-3 (refresh lost while the destination is down) and W-5 (/healthz?full=true parity).\n- A test-only SSH host private key is committed in test/e2e/fixtures/git-server.\n- R2-6 (clock-free staleness check) is optional and was not filed.\n- No golangci-lint config.

### obs

**What I checked.** I built an inventory of metric registrations across internal/ (77 `patrol_*` families), the /metrics, probe and pprof listeners (internal/server/observe.go, internal/reposerver/run.go, internal/apiserver/server.go), slog setup (internal/server/logging.go), the CLI debug transport, OTLP setup and spans, and Kubernetes Event emitters in the controller, server, appset, notifications and settings. On the chart side I read the ServiceMonitor, PrometheusRule, metrics Services and ports, NetworkPolicy metricsFrom, probes and profiler mounts, the installation guide's observability section, wiring.md, integration-findings W-4..W-7, area 10 OPS-011/056/065-094/168-171 with their matrix rows and status yaml, the kind verify script, and the 2026-10-19 kind validation logs. I also checked controller-runtime v0.24.1 in the module cache to confirm which registry its workqueue, reconcile and leader-election metrics use.

**Per-component inventory:**
- **server:** HTTP/login/SSE/audit/denial/rate-limit/panic/extension metrics plus settings, webhook and repository metrics; probes and pprof on 8083; access and audit logs plus Events; OTLP client spans only.
- **controller:** app/compare/hydrator/sharding/tree API/settings metrics; pprof; Events; client spans and tree API server spans.
- **repo-server:** repo-server, render and CMP-client metrics; /healthz?full=true, /readyz and /metrics; no pprof; tracer set up but no server spans.
- **appset-controller:** appset, webhook and repository metrics; pprof; Events.
- **notifications-controller:** delivery and trigger-evaluation metrics; pprof; delivery-failure Events.
- **commit-server:** commit-server metrics; pprof.
- **cmp-server:** CMP metrics, listener off by default.
- **Missing everywhere:** the controller-runtime registry, client-go metrics and a build-info metric.

**Limitations:**
- The upstream clone at the given scratchpad path is incomplete: no .git, and only gitops-engine/reposerver/test/ui/util subsets. I fetched upstream v3.5.5 docs/operator-manual/metrics.md, controller/server/reposerver/applicationset metrics sources and three cmd files read-only with `gh api` GET into /private/tmp/claude-501/-Users-juliosaraiva-Developer-patrol-cd/f48f8e54-38e6-41b4-b750-ec30e49851a4/scratchpad/obs-upstream.
- I ran no Go tests. All findings rest on code reading.
- I did not run promtool on the PrometheusRule. The shard-alert defect is inferred from lease.go plus the scale-down scenario in envtest_test.go.

**Outside my scope:**
- networkPolicy.metricsFrom defaults to all namespaces (values.yaml:1280-1281), so /debug/pprof is reachable from any namespace when the profiler toggle is on (security).
- go.mod pins otel API v1.41.0 with otel SDK/exporter v1.40.0 (version skew, chore).
- In the kind run the appset and notifications controllers exited on 'leader election lost' (2026-10-19-kind-validation-logs.txt:416-420); worth an HA/resilience look.
- W-1: the controller assembly is still duplicated between internal/server/controller.go and internal/controller/run.go (controller).

### testing

Read: TASKS.md (all), hack/ci/allowed-skips.txt, hack/ci/must-run.txt, hack/ci.mk (ci-* targets), .github/workflows/ci.yml and release.yml (job layout), test/ tree (deploy, e2e/fixtures incl. README and fake-github placeholder, examples, integration and stack/), hack/kind/verify-install.sh header and checks, docs/verification/{phase4-findings.md CI-F*/R2-*, integration-findings.md, runs/2026-10-19-kind-validation.md, runs/2026-10-22-github-ci-runs.md, runs/2026-10-16-ui-flake.md, log.md V-0008/9}, docs/parity/status/*.yaml (headers, statuses classified per file with a script), docs/parity/matrix.md (verified-simulated, blocked, implemented notes mentioning fakes, area 04/07/09 rows), docs/architecture/threat-model.md, ADR-0001, ADR-0004, NOTICE, ui/playwright.config.ts and ui/e2e specs, Dockerfile UI stage.

Commands (all read-only for the repo; tree clean at the end): `go list` test-file counts and a source/test line ratio per package; gh run list/view and --log-failed for runs 39872474264, 39872459611, 39869163105, 39876114735 (now failed: TestClientServerRoundTripProperty), 39876114689, 39872667954; local reproduction of the rapid seed with -rapid.nofailfile; a ServeMux probe program in the scratchpad; `go vet` and three `go test -overlay` batches running the 28 snapshot scenarios against envtest (27 pass, TestCLI fails); vitest with the two recorded shuffle seeds (both still fail); `go test -cover`/-coverpkg on controller and CLI packages; `GOOS=windows go vet` of the CLI; git ls-remote for refs/patrol/*.

Not covered: I did not download the integration-logs artifact of run 39872459611 (download needs permission; diagnosis rests on the visible log plus the CI-F15 fix ancestry). I did not run the sharding test repeatedly to measure its failure rate. Upstream clone in the scratchpad is partial (gitops-engine, reposerver, test/e2e testdata, ui, util), so no upstream e2e comparison was possible. Parity re-audit items give per-file counts, not per-ID verdicts.

Outside my scope, seen in passing: (1) Release PR run 39876114689 failed with 'GitHub Actions is not permitted to create or approve pull requests' (repository setting). (2) release.yml rebuilds and pushes the image instead of promoting the CI-tested artifact. (3) integration-findings O-1: the controller never writes Cluster.status (connection state, version, cache info). (4) internal/cli/config/perm_windows.go makes the token-bearing CLI config permission check a no-op on Windows. (5) The unit CI job takes about 23.5 minutes. (6) 17 status overlays carry stale 'not wired' headers (filed as three re-audit items here; the parity explorer may overlap). (7) A path parameter equal to '/' cannot be routed by the API (Go ServeMux), harmless for Kubernetes names.

### critic

Read in full: TASKS.md; docs/verification/phase4-findings.md (every M/DV/R2/KV/CI-F row); docs/verification/integration-findings.md (W-1..W-15, UI-1..UI-6, I-1, I-2, O-1..O-4); docs/verification/log.md (V-0013..V-0039); the 'Not covered' and 'Known limitations' sections of the kind validation run, the image-static run, ci.md, releasing.md, manifests.md, installation.md and README.

From docs/parity/matrix.md I went through every not-started, blocked and different-by-design row, plus 66 in-progress rows whose notes say "not implemented / missing / not wired". I spot-checked about 10 of them against the code; most are stale statuses that the re-audit candidates already cover. I also grepped the component docs for "not implemented" statements.

GitHub: I pulled the failing-test lines from every failed run (39876114735, 39876114689, 39872474264, 39872459611, 39869163105) with `gh run view --log-failed`, and read PR #5's merge state, the branch protection and ruleset APIs, and the label list.

Code and chart: I scanned for exported functions in internal/ and pkg/ that have no non-test caller (105 hits) and followed up the risky ones: trusted proxies, impersonation, global projects, password policy, redirect validation, symlink checks, cache invalidation, notification state, refresh paths. I also checked the chart for monitoring, PDBs, resources, NetworkPolicy metadata exclusions, CRD keep, trust ConfigMaps and repo-server emptyDir limits, plus http.Server timeouts, cookie attributes, webhook limits, the token cache and working-copy keys.

Tests run (the working tree stayed clean):
- `go test -count=1 -shuffle=on -p 6 ./internal/... ./pkg/... ./hack/... ./cmd/...`: only internal/lua failed.
- Targeted Lua watchdog reproductions with GOMAXPROCS=1 and 2.
- UI vitest with `--sequence.shuffle --sequence.seed=424242`: 912/912 passed.

Not covered:
- envtest-dependent packages, test/integration and test/deploy under shuffle (KUBEBUILDER_ASSETS was unset, so those tests skipped).
- staticcheck, because the installed binary is too old for Go 1.27.
- A direct upstream comparison: the scratchpad upstream/argo-cd clone is partial (ui, test, util, gitops-engine, reposerver directories only, and no usable .git), so I relied on the parity area docs for upstream behaviour.
- GHCR storage and transfer limits for private packages on the Free plan; not verified, so not reported.
- I did not re-run the CI jobs or the kind environment.
