# Claude Code SDLC: pre-push checks, agent review and the push gate (2026-10-23)

Issue [#229](https://github.com/juliosaraiva/patrol-cd/issues/229), branch `feat/claude-code-sdlc`.
The change adds `make sdlc-check` (CI's static checks, the tests of what changed, CodeQL gated
against a triaged baseline), the `review-change` skill (reviewer agents and an adversarial verifier
per blocking finding), and the push gate: git's pre-push hook (`.githooks/pre-push`, the authority)
and a Claude Code `PreToolUse` hook (`hack/sdlc/claude-pre-push.sh`, `hack/sdlc/pushcheck`). The
guide is [docs/guides/claude-code-sdlc.md](../../guides/claude-code-sdlc.md).

## Environment

| Item | Value |
|------|-------|
| Host | macOS arm64 (Darwin 27.0.0) |
| Go | go1.27.3 darwin/arm64 |
| git | 2.56.0 (Homebrew); git-lfs 3.7.1 in `bin/` only |
| Shell of the Bash tool | zsh 5.9.2 with the user's oh-my-zsh setup (aliases such as `gp='git push'`, AUTO_CD on) |
| Linters | golangci-lint 2.15.0, shellcheck 0.11.0 |
| CodeQL | bundle 2.27.3 (`hack/sdlc/codeql.sh`, SHA-256 pinned) |

The machine was shared with up to 16 review and red-team agents during most runs.

## Checks of the code at `c409428` (the last code commit before this record's update)

| Command | Result |
|---------|--------|
| `go vet ./hack/sdlc/... ./internal/git/...`; `golangci-lint run ./hack/sdlc/... ./internal/git/...` | passed, 0 issues |
| `shellcheck hack/sdlc/*.sh .githooks/pre-push` | passed |
| `go test -race -count=1 ./hack/sdlc/...` | passed: `pushcheck` and `sarifgate`, 411 tests (counted with `-json`) |
| `bash hack/sdlc/test-sdlc.sh` (the gate end to end in scratch repositories with a bare remote) | passed: 106 passed, 0 failed |
| `PATH="$PWD/bin:$PATH" go test -race -count=1 ./internal/git/...` | passed (and `./internal/reposerver/...` on `6f9f985`, which has the same `internal/git`) |

## Agent review

Eight runs of `.claude/workflows/sdlc-review.js` on successive trees, each verified finding fixed
before the next run:

| Tree | Blocking (verified) | Minor | Refuted |
|------|---------------------|-------|---------|
| `09baca3` | 15 | 12 | 0 |
| `eaae387` | 9 | 10 | 0 |
| `ead001d` | 10 | 10 | 0 |
| `b0bb381` | 10 | 10 | 0 |
| `4922ea6` | 15 | 13 | 0 |
| `b9bbfb2` | 11 | 7 | 0 |
| `83ea66c` | 7 | 6 | 0 |
| `fbdc7df` | 8 | 7 | 0 |

Most findings were ways around the Claude Code hook (shell and git constructs it read differently
from bash, zsh or git) and tests that could not fail. The seventh run's were six untested branches
and one bypass (a zsh function defined through `functions[…]=`). The eighth run's were six more
spellings the walker misread (a quoted or escaped `export`, `=env`, a piped script with shell
options, `rebase -x'…'`, `watch`, git's upload-pack and receive-pack commands) and two gaps in the
gate scripts (a local branch named `origin/main`, ignored source files). Rather than model each new
spelling, the hook now has a net under the walker: a command it does not model, whose own words
name `--no-verify`, `core.hooksPath` or an unsafe `GIT_*` variable, is refused. The review of the
tree that contains this record runs after it is written; its result is in the pull request.

## Red team

Four runs of a red-team workflow (lenses: bash and zsh language, git's surface, the gate around the
parser, false refusals over a corpus of ordinary commands). Each finding was reproduced in a
sandbox with a throwaway bare remote (`probe.sh` through the hook, `realrun.sh` for real in bash and
zsh) and checked by a verifier against the guide's contract.

| Run | Bypasses | False refusals confirmed | Refuted |
|-----|----------|--------------------------|---------|
| 1 | 0 | 43 | 4 |
| 2 | 0 | 21 | 4 |
| 3 | 0 | 3 | 3 |
| 4 | 0 | 5 | 4 |

All confirmed false refusals were fixed and have tests.

## `make sdlc-check` runs

Six full runs on earlier trees (logs in the session scratchpad); none recorded a pass. One ran to
the end and failed in `go test`; five were stopped when their tree was replaced, three of them after
packages had already failed. The failures: `internal/git` (`TestLFS`, below) in the third, fourth
and sixth runs, and tests that fail under load: `internal/server`
`TestWaitForCRDsTimesOutNamingTheMissingKinds` (#228), `internal/apiserver/services/applications`
(`TestApplicationLogs`, #223), `internal/appset/generators` (`TestPluginGeneratorFailures/timeout`,
#224), and once `internal/cmp` (the test was not identified: the report was overwritten by a later
run; `go test -race -count=5 ./internal/cmp/...` alone passed). `ci-static` and the commit-subject
check passed in every run.

A seventh run, on `a7346cd` (tree `fbdc7df`, with the #227 fix), ran every step while 12 review
agents loaded the machine: `ci-static` passed (314 s), the commit-subject check passed, the Go tests
of all 102 packages passed (1790 s), and CodeQL of Go, the web UI and the workflows passed (638 s).
It then stopped with `check.sh: line 172: syntax error`: the file had been edited while it ran, and
bash reads a script as it goes. Nothing was recorded. check.sh is now one group that bash reads whole
before running it. The run on the tree that contains this record is in the pull request.

## Findings outside the SDLC

- **#227, fixed in this branch:** with LFS enabled, `internal/git` never configured the LFS filter
  (it runs git with `HOME=/dev/null`, `GIT_CONFIG_GLOBAL=/dev/null` and its own settings, and the
  image's `/etc/gitconfig` has none), so `git lfs pull` skipped the checkout and LFS files stayed
  pointer files. `TestLFS` passed on GitHub's runner only because the runner's git-lfs package
  configures the filter system-wide; here it failed (`expected "binary-content\n", actual "version
  https://git-lfs.github.com/spec/v1 …"`). Reproduced by hand with the client's environment
  (`git lfs pull`: "Skipping object checkout, Git LFS is not installed"); after the fix `TestLFS`
  passes and `TestLFSFilterConfig` checks the configuration whatever the host's gitconfig holds.
- **#228:** `TestWaitForCRDsTimesOutNamingTheMissingKinds` needs a discovery call within 100 ms;
  under load none finished. `go test -race -count=10 -run
  'TestWaitForCRDsTimesOutNamingTheMissingKinds$' ./internal/server/` alone passed 10 of 10.
- **No protection on `main`:** `gh api repos/juliosaraiva/patrol-cd/rules/branches/main` returns
  `[]`; the only ruleset targets no branch. The guide says so (#25, #220).

## Incident

During the fifth review run, a reviewer agent's reproduction script ran `git push --no-verify origin
main` against the real repository three times. GitHub refused each push as a non-fast-forward;
`main` stayed at `774c10a`. The review workflow's common prompt now forbids any `git push`,
`git send-pack` or other command that could push the repository, and asks for a throwaway bare
remote instead.

## Limits

- Local macOS runs only; CI on the pull request is the Linux result.
- The final `make sdlc-check` (with CodeQL) and the agent review of the tree that contains this
  record run after it is written: a record cannot hold results about the tree it is part of. Both
  are reported in the pull request.
- The Claude Code hook is not a security boundary (guide, Limits): it does not read the user's shell
  aliases, functions or options, the content of scripts, or code given to an interpreter.
