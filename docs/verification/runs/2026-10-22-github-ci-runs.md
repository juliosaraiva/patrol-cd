# CI workflow: the first runs on GitHub (2026-10-22)

The four runs of `.github/workflows/ci.yml` on pull request
[#4](https://github.com/juliosaraiva/patrol-cd/pull/4) (`feat/ci` into `main`), what each found, and what
the passing run does and does not show. Runner: GitHub-hosted `ubuntu-24.04` (x64), the private repository on
the Free plan. The local validation that came before is in
[`2026-10-22-ci-local-validation.md`](2026-10-22-ci-local-validation.md).

This pull request changes `.github/workflows/ci.yml` and the Makefiles, so every run was in the **full tier with
the kind job** (unit tests under `-race`, integration, deploy, image, kind); other tiers have not run yet.

## The runs

| Run | Head | Result | What failed, and the fix |
|-----|------|--------|--------------------------|
| [39778169607](https://github.com/juliosaraiva/patrol-cd/actions/runs/39778169607) | `3cfee11` | failed | **Deploy suite**: `TestPodSecurityRestricted` shared one deadline across its parallel subtests, which started minutes after their parent on 2 CPUs (CI-F8). **Unit tests**: `TestRunnerLimits` died under `-race` on the sandbox's address-space limit (CI-F9); `TestEnvtestConcurrencyStress` asserted an event that a resync can swallow (CI-F10) |
| [39784348046](https://github.com/juliosaraiva/patrol-cd/actions/runs/39784348046) | `00a8fc5` | **passed**, all 9 jobs | fixes for the first run, plus the full tier for changes to the shared setup action (CI-F12) |
| [39788936657](https://github.com/juliosaraiva/patrol-cd/actions/runs/39788936657) | `353cff6` | failed | **Integration suite**: `TestStackStarts` at "first use of a new project role token": `invalid session: token revoked (HTTP 401)`. A product race: the server decides whether a project token is known from the informer cache, which can lag the write that recorded it (CI-F14) |
| [39792963794](https://github.com/juliosaraiva/patrol-cd/actions/runs/39792963794) | `44b420e` | **passed**, all 9 jobs | fix for the third run, plus the fixture race of the generators' tests and the CRD wait's error reporting (CI-F11, CI-F13) |

Run 4, the last one on code, is the evidence for the tested head `44b420e2a560608e5719ee3adda47c8b6081b7d8`.
The commits after it change documentation only; each push runs the full tier again, and the checks of the final
head are on the pull request (a record cannot name the run of the commit that contains it).

## Run 4 (head `44b420e`)

| Job | Result | Time | What it showed |
|-----|--------|------|----------------|
| Choose tiers | success | 0.1 min | full tier and kind (the pull request changes `ci.yml` and the Makefiles) |
| Static checks | success | 3.2 min | gitleaks, gofmt, `go mod tidy -diff`, build, vet, golangci-lint, generated files, OpenAPI, parity, test report rules, `test-classify: 36 passed, 0 failed`, shell, actionlint |
| Unit tests | success | 23.8 min | `-race`: 99 packages (87 ok, 12 without test files), 8,650 tests, 8,525 passed, 0 failed, 125 skipped, **all 125 allowed and none forbidden** (git, git-lfs, gpg, sshd, helm and kustomize ran); `test-scripts: 79 passed, 0 failed` |
| Web UI | success | 4.2 min | install, API types check, typecheck, lint, vitest, build |
| Image (linux/amd64) | success | 6.3 min | pin checks, BuildKit checks, build, layout, smoke and rootfs checks |
| Integration suite | success | 11.1 min | 23 tests counting subtests under `-race`, 0 skipped |
| Deploy suite | success | 7.8 min | 178 tests counting subtests, 0 skipped. `Pod Security restricted: 10 profiles, 52 pods (66 containers incl. init) admitted`; subtests passed: airgap, all-features, core, default, ha, hardened, hydrator, ingress-nginx, minimal, namespaced, negative-control |
| Kind (install, verify, lifecycle) | success | 9.6 min | `kind-up`, `kind-fixtures`, `kind-smoke`, `kind-install`, `kind-verify --lifecycle` at `PATROL_WAIT_SCALE=3`: `verify-install: 58 passed, 0 failed` |
| CI (gate) | success | 0.1 min | `changes=success static=success unit=success ui=success image=success integration=success deploy=success kind=success` |

The sum of the job times is about 66 minutes (billing rounds each job up; about 72). The first passing run (run 2)
took about 77: caches were cold for the first runs, and the image job went from 13.1 to 5.9 to 6.3 minutes.

The gate job's first attempt did its work (its steps all succeeded at 14:31:23Z) but GitHub left its check run
"in progress" for more than twenty minutes; only that job was re-run (`gh run rerun --job`, attempt 2) and passed in
7 s. The run itself was already `completed success`.

## Not shown

- **Other tiers.** Docs only, code only (no `-race`, no integration, deploy or kind) and kind only have not run on
  GitHub; the tier selection is covered by `hack/ci/test-classify.sh` (36 cases), not by runs. They cannot be
  observed on this pull request: a pull request run compares the merge commit with `main`, so every push to a
  pull request that changes `ci.yml` selects the full tier, whatever the last commit touches.
- **Pushes to `main` and manual runs**, the `ci:kind` and `ci:full` labels (not created yet), `CI_HEAVY_ON_PR`, and
  `workflow_dispatch` (only possible once `ci.yml` is on the default branch).
- **Cache behaviour on `main`.** Caches saved by a pull request are not shared with `main`; what a push to `main`
  restores is unobserved.
- **Failure paths on a runner**: the kind diagnostics bundle and its secret scrub were exercised only locally, and
  no run failed in the kind job.
- **Flakiness.** Three of four runs failed for a different reason each (all fixed); two clean runs do not show that
  none is left.
- **arm64**, and anything that needs branch protection (the `ci` gate is advisory on the Free plan).
- **Minutes.** The figures above are for the full tier on this pull request; a month of mixed pull requests is
  unmeasured.
