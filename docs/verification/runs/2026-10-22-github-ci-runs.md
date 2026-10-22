# CI workflow: the first runs on GitHub (2026-10-22)

The four runs of `.github/workflows/ci.yml` on pull request
[#4](https://github.com/juliosaraiva/patrol-cd/pull/4) (`feat/ci` into `main`), what each found, and what
the passing run does and does not show. Runner: GitHub-hosted `ubuntu-24.04` (x64), the private repository on
the Free plan. The local validation that came before is in
[`2026-10-22-ci-local-validation.md`](2026-10-22-ci-local-validation.md).

This pull request changes `.github/workflows/ci.yml` and the Makefiles, so every run was in the **full tier with
the kind job** (unit tests under `-race`, integration, deploy, image, kind). The first push to `main` after the merge
and the two probe pull requests for the other tiers are in the last two sections.

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

## After the merge: the first push to `main`

[39814479674](https://github.com/juliosaraiva/patrol-cd/actions/runs/39814479674), event `push`, `main` at the
merge commit `532a3b7`, started 16:29Z (a pull request's caches are not visible to `main`, so this run started with
no cache of its own):

| Job | Result | Time |
|-----|--------|------|
| Choose tiers | success | 0.1 min |
| Static checks | success | 9.3 min |
| Unit tests (`-race`) | success | 37.3 min |
| Web UI | success | 4.0 min |
| Image (linux/amd64) | success | 12.9 min |
| Integration suite | success | 18.1 min |
| Deploy suite | success | 15.8 min |
| Kind (install, verify, lifecycle) | success | 9.7 min (started 16:42Z, when the image job finished) |
| CI (gate) | success | 0.1 min |

Wall time 37.7 min; the sum of the job times is 107.3 minutes, against about 66 on pull request #4 with warm
caches. The push trigger selects the full tier and the kind job, as designed, and nothing was cancelled.
The unit job spent 34.6 minutes in its `Go tests` step (16:31 to 17:05Z; 23.8 minutes in total on pull request #4
with warm caches) because the `-race` build cache was empty; saving the caches afterwards took 27 seconds.

At the same moment the Dependabot pull request
[#5](https://github.com/juliosaraiva/patrol-cd/pull/5) (`github-actions` group bump; it changes
`.github/workflows/ci.yml`, so it selects the full tier) ran as well:
[39814720894](https://github.com/juliosaraiva/patrol-cd/actions/runs/39814720894) passed all 9 jobs, 104.7 job
minutes, for the same reason (its caches were not yet on `main`). Every Dependabot bump of a workflow action is
therefore a full-tier run.

### Caches after the run

`main` now holds the caches the jobs saved: Go build and module caches for the static (851 MiB), unit (988 MiB),
integration (610 MiB) and deploy (603 MiB) jobs, the image job's `actions/setup-go` cache (775 MiB), the pinned tools
(37 MiB), `envtest` (47 MiB), the pnpm store (44 MiB) and the image hand-over (89 MiB). By GitHub's cache scoping a
pull request can restore these. **The repository's cache usage was 12.4 GB in 33 caches at 17:07Z against the 10 GB limit** (the
pull-request refs #4 to #7 hold copies of most of these, saved before `main` had any), so GitHub evicts the least
recently used caches and a later run may find a cold cache again. The first warm pull-request run after the merge
has not been observed (see below).

## The other tiers, on two throwaway pull requests

Two draft pull requests against `main`, opened to be closed without merging; each run started 16:31Z, while the run
above was still using the runners, so the times include cold PR-ref caches and are not tier benchmarks.

| Pull request | Change | Run | Jobs that ran (minutes) | Jobs skipped |
|--------------|--------|-----|--------------------------|--------------|
| [#6](https://github.com/juliosaraiva/patrol-cd/pull/6) `probe/docs-only` | one new file, `docs/probes/ci-docs-only.md` | [39814799212](https://github.com/juliosaraiva/patrol-cd/actions/runs/39814799212) | Choose tiers 0.1, Static 10.7, CI 0.1 (about 11) | unit, UI, image, integration, deploy, kind |
| [#7](https://github.com/juliosaraiva/patrol-cd/pull/7) `probe/code-only` | one comment line appended to `internal/envtestx/envtestx.go` (no workflow, Makefile or module file) | [39814822430](https://github.com/juliosaraiva/patrol-cd/actions/runs/39814822430) | Choose tiers 0.1, Static 10.3, Unit without `-race` 17.4, Web UI 4.0, Image 12.2, CI 0.1 (about 44) | integration, deploy, kind |

Both runs passed, and the `CI` gate counted the skipped jobs as success. The docs-only tier runs `static` only; the
code tier runs `static`, `unit` without `-race`, `ui` and `image`, and not the integration, deploy and kind jobs.
That is what `docs/guides/ci.md` says and what `hack/ci/test-classify.sh` asserts; this is the first time a runner
confirmed it. The `ci:kind` and `ci:full` labels were created (`gh label create`); applying one to a pull request
has not been tried.

## Not shown

- **The kind-only tier** (a pull request that touches only the kind scripts, or the `ci:kind` label) has not run on
  GitHub, nor has a pull request with a label.
- **`CI_HEAVY_ON_PR`** and **`workflow_dispatch`** (the latter is possible now that `ci.yml` is on `main`) have not been used.
- **Warm-cache runs of the heavier tiers.** The probes ran before `main` had caches, so the times above for the
  code-only and docs-only tiers are cold-cache times. The only run since is the docs-only run of the pull request
  that records this ([#9](https://github.com/juliosaraiva/patrol-cd/pull/9), run
  [39820388783](https://github.com/juliosaraiva/patrol-cd/actions/runs/39820388783)): `Static checks` hit the
  `go-static-...` cache saved by `main` (851 MB, "Cache restored successfully") and took 3.2 minutes instead of
  10.7; the jobs that ran were the same three as in #6. How long the code and full tiers take on restored caches is
  unobserved, and the cache usage is above the repository limit (see above).
- **Failure paths on a runner**: the kind diagnostics bundle and its secret scrub were exercised only locally, and
  no run failed in the kind job.
- **Flakiness.** Three of the first four runs failed for a different reason each (all fixed); the fifth run of the
  pull request, the push to `main` and the Dependabot run passed, which does not show that none is left.
- **arm64**, and anything that needs branch protection (the `ci` gate is advisory on the Free plan).
- **Minutes billed.** The figures are job durations read from the API. The run-timing endpoint reported 0 billable
  milliseconds for the push run, and the billing endpoint needs a token scope this session does not have, so how
  many of the 2,000 monthly minutes were used is unmeasured. Counting job durations only, the hour of the merge
  (`main`, #5, #6, #7) used about 267 job-minutes.
