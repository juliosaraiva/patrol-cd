# Development lifecycle with Claude Code

How a change to Patrol CD goes from a GitHub issue to a merged pull request when Claude Code (or a
person) makes it: deterministic checks, including CodeQL, and an agent review run before every push,
and a hook refuses a push of anything that did not pass them. CI (`docs/guides/ci.md`) runs the same
checks again on GitHub.

```
issue ─► branch ─► change + tests ─► commit ─► make sdlc-check ─► /review-change ─► git push ─► pull request ─► CI + CodeQL ─► merge
                                               (deterministic)    (agents)          (gate)
```

| Step | What | Where |
|------|------|-------|
| Issue | Work starts from a GitHub issue; labels, tracking issues and scope rules are in `TASKS.md` | GitHub issues |
| Branch | `<type>/<topic>` from an up-to-date `main` | |
| Commit | conventional subject `<type>(<scope>): <description>` | `make release-lint-commits` |
| Checks | `make sdlc-check`: CI static checks, tests of what changed, CodeQL; records a pass for HEAD's tree | `hack/sdlc/check.sh` |
| Review | the `review-change` skill: reviewer agents by path, a verifier per blocking finding; records the outcome | `.claude/skills/review-change`, `.claude/workflows/sdlc-review.js`, `.claude/agents/` |
| Push | the hook lets `git push` through only with both records for the pushed tree | `hack/sdlc/claude-pre-push.sh`, `hack/sdlc/gate.sh` |
| Pull request | template sections, `Closes #N`, assignee | `.github/pull_request_template.md` |
| CI | tiers of `ci.yml`, CodeQL code scanning | `.github/workflows/ci.yml`, `.github/workflows/codeql.yml` |
| Merge | when the owner asks, after CI passed on the head commit | |

The `sdlc` skill (`.claude/skills/sdlc/SKILL.md`) gives Claude Code these steps; `CLAUDE.md` points to
this guide.

## Deterministic checks: `make sdlc-check`

`hack/sdlc/check.sh` looks at the files changed since the merge base with `origin/main` and runs:

| When | Check |
|------|-------|
| always | `make ci-static` (gofmt, `go mod tidy`, build, vet, golangci-lint, generated files, OpenAPI, parity matrix, test report rules, tier and SDLC gate tests, shell scripts, workflow lint, gitleaks) and `make release-lint-commits` |
| Go files changed | `make ci-unit` on the changed packages (`-race`, envtest, the strict skip policy); all packages when `go.mod` or `go.sum` changed |
| `test/integration/` | `make ci-integration` |
| `deploy/` or `test/deploy/` | `make ci-deploy` |
| `hack/kind/` or `test/e2e/` | `make ci-scripts` |
| `ui/` | `make ci-ui` |
| Go, JavaScript/TypeScript or workflow files | CodeQL for those languages (below) |

A documentation-only change (the rule of `hack/ci/classify-changes.sh`: `docs/`, `*.md` at the root,
`.gitignore`) runs the first row only. The script stops at the first failure.

When every check passed on a **clean** tree, it writes a record for HEAD's tree
(`git rev-parse HEAD^{tree}`) to `$(git rev-parse --git-common-dir)/patrol-sdlc/checks/<tree>`. A
dirty tree is checked but not recorded, and `--no-codeql` (`make sdlc-check SDLC_CHECK_FLAGS=--no-codeql`,
for quick iterations) records nothing either. Records are keyed by tree, so amending a commit or
adding one needs a new run, and rebasing onto a new `main` changes the tree too.

The test suites need what CI installs: envtest assets (`make envtest`), kustomize, helm, git-lfs,
gpg and sshd. A missing tool turns into a forbidden skip and fails the run, as on CI
(`hack/ci/allowed-skips.txt`); install it (for example `brew install git-lfs`).

## CodeQL

CodeQL runs locally before every push and on GitHub. Its license allows that for this repository:
the CodeQL CLI may analyze an open-source codebase (one released under an OSI-approved license,
here Apache-2.0), and automated analysis in CI is allowed when that codebase is hosted on GitHub.com.
The repository is public since 2026-10-23 for this reason; on a private repository the same use
would need a GitHub Code Security license.

- **Bundle.** `hack/sdlc/codeql.sh install` (`make codeql-install`) downloads CodeQL 2.27.3 into
  `bin/codeql-2.27.3/` and checks its SHA-256 against the pin in the script and the publisher's
  checksum file. macOS and Linux x86-64 only (CodeQL ships no Linux arm64 bundle); about 2 GB
  unpacked; needs `zstd`.
- **Analysis.** `hack/sdlc/codeql.sh analyze [go] [javascript-typescript] [actions]` (`make codeql`,
  `CODEQL_LANGUAGES=go make codeql`) builds a database per language (Go by tracing `go build ./...`;
  JavaScript/TypeScript across the repository and GitHub Actions workflows without a build, with the
  exclusions of `.github/codeql/codeql-config.yml`) and runs the `security-extended` suite. SARIF
  files and a summary go to `dist/codeql/`.
- **Gate.** `hack/sdlc/sarifgate` fails on every result of level `error` or `warning` that is not
  suppressed in the source and not accepted in `hack/sdlc/codeql-baseline.txt`; notes are listed
  but do not fail. A baseline line names the rule, the file, CodeQL's line-independent fingerprint
  and the reason. Prefer fixing: accept an alert only when it is a false positive or a deliberate
  design, and dismiss it on GitHub with the same reason.
- **On GitHub.** `.github/workflows/codeql.yml` runs the same bundle, suite and configuration on
  pull requests (not documentation-only ones), on pushes to `main` and every Monday, and uploads the
  results to code scanning (Security tab), where new alerts show on the pull request.

## Agent review: the `review-change` skill

`/review-change` (or asking Claude Code to review the branch) runs the saved workflow
`.claude/workflows/sdlc-review.js` on the branch's diff:

1. `hack/sdlc/review-input.sh` names the tree, the base, the changed files and writes the diff.
2. The workflow picks reviewers by path. Each reviewer is an agent whose instructions are in
   `.claude/agents/`; they read the diff and the code around it, may run targeted tests, and never
   edit the tree.

   | Agent | Looks at | For |
   |-------|----------|-----|
   | `go-reviewer` | every Go file | errors, context, concurrency, leaks, client misuse, portability |
   | `controller-reviewer` | `api/`, controllers, cache, sync, ApplicationSet, hydrator, server wiring | reconcile correctness, stale cache reads, ownership, status and finalizers, the ground rules |
   | `security-reviewer` | API server, auth, RBAC, repo-server, render, sandbox, git/helm/OCI, webhooks, `pkg/`, `cmd/`, chart, image, workflows | authorization, untrusted input, secrets, SSRF, workflow supply chain |
   | `test-reviewer` | tests, testdata, `test/`, `hack/ci/` | flakiness patterns this repository hit before (CI-F1…CI-F20), tests that cannot fail, missing coverage |
   | `api-reviewer` | API server, `pkg/apiclient`, OpenAPI, CLI, UI API layer, parity records | contract and compatibility, parity claims without evidence |
   | `ui-reviewer` | `ui/` | behaviour, API use, accessibility, the UI design rules |
   | `delivery-reviewer` | chart, manifests, image, kind scripts, Makefiles, workflows, release, SDLC files | install and upgrade, pinning, permissions, bash 3.2 and 5 portability |

3. Every `blocker` or `major` finding goes to `review-verifier`, which tries to refute it against
   the code and, where it can, reproduces it. Only confirmed findings count; minor findings are
   reported but do not block.
4. The skill writes a report next to the record and calls `hack/sdlc/record-review.sh` with the
   number of confirmed blocking findings. The record passes when that number is 0 and every
   reviewer finished.

The reviewers and the verifier are language models: their agreement is evidence that a defect
exists, not proof that none is left. They complement the deterministic checks and CI; they do not
replace them.

## The push gate

`hack/sdlc/gate.sh COMMIT` decides whether a commit may be pushed. It needs, for the commit's tree:

- a passing check record, always;
- a passing review record, unless the change since the merge base is documentation only.

Two hooks run it:

- **git's pre-push hook** (`.githooks/pre-push`, enabled with `make sdlc-git-hooks`, which sets
  `core.hooksPath`). It is the authority: git gives it the commits actually being pushed, so a
  commit made in the same command line (`git commit … && git push`), a push from a script, an
  alias or another tool are all checked. A person can bypass it in an emergency with
  `git push --no-verify`.
- **the Claude Code hook** (`hack/sdlc/claude-pre-push.sh`, a `PreToolUse` hook for the Bash tool
  and the terminal tool in `.claude/settings.json`). While git's hook is not enabled it refuses every
  command but `make sdlc-git-hooks`, so git's hook runs whatever Claude Code pushes. Then every
  command goes to `hack/sdlc/pushcheck` (built into `bin/` on first use), which parses it as bash
  with `mvdan.cc/sh`. Its contract:

  - **What it models.** Words are expanded as bash does when the result does not depend on the
    environment, the filesystem or another command: quotes, escapes, `$'…'` and brace expansion, so
    `git p""ush` and `git push {--no-verify,origin} main` are read as bash runs them. A word with a
    variable, a substitution or an unquoted glob is *not literal*. Text in quotes, heredoc bodies and
    comments are not commands. `cd`, `pushd` and `git -C` move the directory only where they
    certainly run: inside a pipeline, a background job, a subshell or a function they do not reach
    the commands after them, and a `cd` that may or may not run (after `&&` or `||`, in an `if`,
    `case`, loop or function) leaves the directory unknown. It follows the command a wrapper runs
    (`timeout`, `nice`, `sudo`, `nohup`, `env`, `xargs`, `find -exec`, `kubectl`/`docker exec -- …`,
    `command`, `exec`, …) into that command, and parses `bash -c '…'`, a heredoc or here-string given
    to `bash`/`sh`/`zsh`/`dash`/`ksh`/`mksh`, `eval` and `git rebase -x`/`bisect run`/`submodule
    foreach`. A `git push` found this way is gated like a direct one; a push that cannot be gated per
    step (`rebase -x`, `submodule foreach`) is refused.
  - **What it refuses wherever the command runs**, because it can keep git's hook from running: a
    `GIT_*` variable outside a short safe list (`GIT_DIR`, `GIT_WORK_TREE`, `GIT_CONFIG_*` …, set
    inline, with `env`, `export`, `declare`, `read` or `printf -v` — but not merely printed); `git
    -c` and `git config` writes of keys outside a short safe list (`core.hooksPath`, `alias.*`,
    `include.*`, `core.pager` …); `--git-dir`, `--work-tree`, `--bare` and other global options
    before `push`; a configured alias that pushes; a file sourced before a push; and a push it cannot
    resolve — inside `$(…)`, behind `eval` with a dynamic string, through a non-literal command word,
    git option or shell script, or in a command that does not parse as bash (the Bash tool may run
    zsh) and names a push or a hook override.
  - **What it refuses for a push from a clone of this project** (any clone, and its worktrees): a
    push while that clone's `core.hooksPath` is not `.githooks`, from inside its `.git` directory, from
    an unknown directory, or from a branch without the gate; `--no-verify`; an abbreviated or unknown
    `git push` option (git accepts `--no-veri` for `--no-verify`); a non-literal argument; and the
    forms the gate cannot check ref by ref: `--all`, `--branches`, `--mirror`, `--tags`, `:`.

  Then it runs that clone's gate on each pushed commit as it is before the command runs, which gives
  an early, clear refusal; git's hook catches a commit the same command creates, and a ref that does
  not exist yet (a branch or tag created earlier in the command) is left to git's hook too. Deletions,
  dry runs, `git push -h` and reads such as `git config --get` pass. Commands that do nothing of the
  above run untouched, whatever they say about git push in a branch name, a path, a pattern, a commit
  message or a variable.

`make sdlc-gate` shows the decision for HEAD. A push the Claude Code hook refuses is made by the
owner, by hand.

A second hook (`hack/sdlc/claude-format.sh`, `PostToolUse` for Edit, MultiEdit and Write) runs
`gofmt -w` on each Go file Claude Code writes.

## Files

| Path | Role |
|------|------|
| `hack/sdlc/check.sh`, `hack/sdlc.mk` | deterministic checks, make targets |
| `hack/sdlc/codeql.sh`, `hack/sdlc/sarifgate/`, `hack/sdlc/codeql-baseline.txt`, `.github/codeql/codeql-config.yml` | CodeQL install, analysis, gate, accepted alerts, shared configuration |
| `hack/sdlc/review-input.sh`, `hack/sdlc/record-review.sh` | input and record of the agent review |
| `hack/sdlc/gate.sh`, `hack/sdlc/claude-pre-push.sh`, `hack/sdlc/pushcheck/`, `.githooks/pre-push` | the push gate, the Claude Code hook and its command parser, the git hook |
| `hack/sdlc/claude-format.sh` | gofmt after edits |
| `hack/sdlc/lib.sh`, `hack/sdlc/test-sdlc.sh` | shared helpers; offline tests of the gate (`make ci-sdlc`, part of `make ci-static`) |
| `.claude/settings.json` | the hooks and the permissions for the SDLC commands |
| `.claude/agents/*.md` | the reviewer and verifier agents |
| `.claude/workflows/sdlc-review.js`, `.claude/skills/review-change/`, `.claude/skills/sdlc/` | the review workflow and the skills |
| `.github/workflows/codeql.yml`, `.github/pull_request_template.md` | CodeQL on GitHub, the pull request template |

## Limits

- The records live in the clone's git directory: they show what ran on this machine, not on another
  one, and CI does not read them. CI runs its own checks.
- The Claude Code hook sees the command line, not what it builds at run time or the files and
  programs it runs: a git command whose name or subcommand is assembled from variables or `$'…'`
  escapes; the content of script files, Makefiles and shell functions defined elsewhere; code given
  to an interpreter (`python -c`, `perl -e`, `awk`, a custom command that is not a known wrapper); and
  commands on a remote host (`ssh host …`). git's pre-push hook (required before any command runs)
  checks the pushes they make; a script or program that disables git's hook on purpose gets past both.
  The hooks keep a cooperative process honest and catch mistakes and shortcuts; they are not a
  security boundary against someone who sets out to defeat them. The boundary is on GitHub: CI and the
  branch rules of `main`.
- The hooks come from the checked-out tree: while a commit from before the gate is checked out, they
  are not there to run.
- The check record is keyed by the tree. Two of its checks also read history (commit subjects,
  gitleaks over the branch's commits): a commit reworded or re-parented onto a checked tree passes
  the gate without them; CI runs both again on the pull request.
- The checks run the tests of the changed packages, not of the packages that import them; CI's full
  tier runs everything on every push to `main`.
- CodeQL `security-extended` finds known vulnerability patterns; it does not prove their absence.
