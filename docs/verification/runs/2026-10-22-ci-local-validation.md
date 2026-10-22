# CI workflow: local validation (2026-10-21 / 22)

What was run to check `.github/workflows/ci.yml` and its helpers (`docs/guides/ci.md`) before their first
run on GitHub, what it found, and what it cannot show.

**Nothing here ran on GitHub Actions**: this record covers what was checked before the workflow's first run, and
no claim about billable minutes, runner behaviour, the `ci` gate, the cache keys or the amd64 image rests on it.
The runs on GitHub, which found four more problems (CI-F8, CI-F9, CI-F10, CI-F14), are in
[`2026-10-22-github-ci-runs.md`](2026-10-22-github-ci-runs.md) and `log.md` V-0035.

## Environment

| | |
|---|---|
| Developer machine | macOS 27.0.1, arm64; Go 1.27.3 (`go.mod`: 1.27.2); golangci-lint 2.15.0; shellcheck 0.11.0; actionlint 1.7.12; gitleaks; Docker Desktop 29.9.2 (VM: 8 CPUs, 16 GB) |
| Linux | `ubuntu:24.04` arm64 container on that VM, `--cpus=2 --memory=7g --init`, git 2.43.0, Go 1.27.2, Helm 4 / kustomize 5.8.1 / git-lfs from `hack/images/fetch-tools.sh`, golangci-lint 2.15.0 (SHA-256 verified) |
| Load | The Docker VM also runs unrelated containers (an observability stack, Jenkins, an emulated amd64 Java service, other kind clusters). During the runs its load average was 100 to 250, CPU pressure (`/proc/pressure/cpu`) about 96 %, free memory a few hundred MB. **Timings are therefore not runner timings** and are quoted only as "it finished"; envtest control planes took over 90 s to start in the container. |

## What was run

| Check | Result |
|---|---|
| `make ci-gofmt ci-gomod ci-lint-go ci-openapi ci-parity ci-rules ci-shell ci-actionlint ci-generated` (macOS) | exit 0 |
| `make ci-gitleaks` (history of HEAD, then a copy of the files git does not ignore) | no leaks |
| `make ci-unit CI_RACE=0` (macOS), run 1 | 99 packages, 85 ok; 3 failed tests (a flaky controller test, two tests of the new envtest helper that were wrong); `TestLFS` skipped, which the skip policy rejects (git-lfs is not installed on this machine). All four were fixed or explained (below). 31 min while other work ran |
| `make ci-unit CI_RACE=0` (macOS), run 2 | 99 packages, 86 ok; 8,630 tests; one flaky test (`internal/lua`, fixed) and the same `TestLFS` skip. 16 min |
| `make ci-unit CI_RACE=0` (macOS), run 3, on the final tree | 99 packages, 87 ok, 0 failed; 8,638 tests: 8,512 passed, 0 failed, 126 skipped (125 allowed + `TestLFS`); the report exits 1 only because of the `TestLFS` skip and the must-run rule for it. 14 min |
| `make ci-static` (macOS), on the final tree | exit 0, 2 min 10 s (includes gofmt, `go mod tidy -diff`, vet, golangci-lint, generated files, OpenAPI, parity, rules, shell, actionlint, gitleaks) |
| `make ci-scripts` (macOS), on the final tree | exit 0; `test-scripts: 79 passed, 0 failed` |
| Linux container, 70 packages that need no envtest (`go test -p 1`, no `-race`) | exit 0: 5,543 tests, 0 failed; 197 skips, of which 125 are the allowed conformance skips and 72 are envtest tests skipped because `KUBEBUILDER_ASSETS` was deliberately unset; **no skip caused by a missing tool** (git, git-lfs, gpg, sshd, helm, kustomize all ran). 18 min |
| Offline suite of the kind scripts, `test/e2e/fixtures/test-scripts.sh` (macOS, bash 3.2) | 79 passed, 0 failed (before the review fixes: 76); the fake-clock section runs the real wait loops of `verify-install.sh`, and fails for each of three mutations of their deadlines. Bash 5.2 has not run it; `make ci-scripts` will on the runner |
| Clean-room kind run with the CI settings (`PATROL_WAIT_SCALE=3`), run 1: `kind-down`, `make image`, `kind-up`, `kind-fixtures`, `kind-smoke`, `kind-install`, `kind-verify VERIFY_FLAGS=--lifecycle` | every step before verify passed (image 286 s, up 192 s, fixtures 117 s, smoke 37 s, install 138 s); **verify failed at its first check**: the server and the ApplicationSet controller had restarted once (KV-14) |
| The same, run 3, on the final tree (after the review of the wait: probe bound to its context, bootstrap time counted, a stop is a clean stop) | all steps rc 0 (image 40 s, up 106 s, fixtures 39 s, smoke 13 s, install 93 s, verify 245 s; the VM was calmer); `verify-install: 58 passed, 0 failed`; 0 restarts (the race was not hit this time, so the log lines of the wait are not in this run) |
| The same, run 2, after the fix (`waitForCRDs`) | all steps rc 0 (image 72 s, up 129 s, fixtures 108 s, smoke 65 s, install 217 s, verify 247 s); `verify-install: 58 passed, 0 failed`; all four components logged a 2.0 to 4.4 s wait for the ApplicationSet CRD; 0 restarts |
| `hack/ci/classify-changes.sh` in scratch git repositories (push, pull request merge) | docs-only, LICENSE-only, `tools.lock`, workflow, kind-script, `hack/ci/gate.sh` and Dependabot changes select the tiers `docs/guides/ci.md` describes |
| `make ci-unit` with `GITHUB_ACTIONS=true` on one package | `::stop-commands::<token>` printed before the report and `::<token>::` after it; exit status unchanged |

## What it found

Product and test defects, all in `docs/verification/phase4-findings.md` (KV-14 and CI-F1 to CI-F7): `git.Client.Checkout("")`
on git before 2.48, envtest control planes leaked by failed starts, a CRD start-up race in the server and the
ApplicationSet controller, three flaky tests. The CI tooling itself was reviewed by independent agents (below) and
the confirmed findings were fixed.

## Independent reviews (agent workflows)

Read-only reviewers by area, then one skeptic per finding that tried to refute it (default: refuted unless reproduced).
Counts are findings the skeptics confirmed after reading the code and, where possible, reproducing them.

| Round | Scope | Confirmed / refuted |
|---|---|---|
| 1 (earlier session) | the whole CI work | 48 findings checked, most fixed |
| 2 | the diagnostics script, the test report tool, the kind scripts, the envtest helper and the CI glue after round 1's fixes | 16 / 3 |
| 3 | round 2's fixes and the whole diff | 7 / 0 |
| 4 | the start-up CRD wait and the last CI tweaks | 3 / 1 (a probe that ignored its context, a stop during the wait that exited 1, no test of the call site: all fixed and mutation-checked) |

The reviewers and skeptics are language models; their agreement is evidence that a defect exists, not that none is left.

## Not done

- No run on GitHub Actions when this was written (superseded: see `2026-10-22-github-ci-runs.md`).
- No Linux `-race` run, and no Linux run of the packages that need envtest, `test/integration` or `test/deploy` (the container could not start
  control planes reliably on the loaded VM; envtest-backed packages were run on macOS).
- `TestLFS` did not run on the developer machine (git-lfs missing); it passed in the Linux container.
- No run on x86-64.
