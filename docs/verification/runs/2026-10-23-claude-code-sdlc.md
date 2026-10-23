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

## Checks of the code of the last code commit before this record's update

| Command | Result |
|---------|--------|
| `go vet ./hack/sdlc/...`; `golangci-lint run --no-config ./hack/sdlc/...` | passed, 0 issues |
| `shellcheck hack/sdlc/*.sh hack/ci/lint-shell.sh`; `make ci-shell`; `actionlint` | passed |
| `go test -race -count=1 ./hack/sdlc/...` | passed: `pushcheck` and `sarifgate`, 517 tests (counted with `-json`) |
| `bash hack/sdlc/test-sdlc.sh` (the gate end to end in scratch repositories with a bare remote) | passed: 141 passed, 0 failed |
| `/bin/bash hack/ci/lint-shell.sh` (bash 3.2) in a scratch repository with no POSIX sh script | reports SC2086 in its bash script; the version before this change stopped with `sh_files[@]: unbound variable` |
| Mutation sweep of `hack/sdlc/pushcheck` (a scratchpad script: each refusal, unknown-directory, environment and option-value site of `parse.go`, each `return true` of a predicate, each refusal or early allowing return of `main.go`, and its loop over the pushed sources, disabled or narrowed in turn through `go test -overlay`, the repository untouched) | 124 sites; 120 caught by a failing test, 2 do not compile; the 2 survivors are error paths of the environment in `baseGate` (git failing to name its directory right after it worked, and the base's files failing to extract). The first sweep, on `0b31e0f`, covered refusal sites only and left 20 in `parse.go` and 5 in `main.go` uncaught; the eleventh and twelfth reviews found more kinds of site, which the sweep now covers. Each uncaught site got a test, and four that could not change the outcome were removed |
| A replace ref giving `origin/main` HEAD's tree, in a scratch repository (`replace-check.sh` in the scratchpad) | without `hack/sdlc/lib.sh` the changed files are `[]`; with it, `[main.go]` |
| `PATH="$PWD/bin:$PATH" go test -race -count=1 ./internal/git/...` | passed on `c409428` (and `./internal/reposerver/...` on `6f9f985`); `internal/git` has not changed since |

## Agent review

Twelve runs of `.claude/workflows/sdlc-review.js` on successive trees, each verified finding fixed
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
| `bdae10c` | 16 | 8 | 0 |
| `e69bdb2` | 3 | 7 | 0 |
| `f794b5b` | 5 | 10 | 0 |
| `e06316e` | 4 | 8 | 0 |

Most findings were ways around the Claude Code hook (shell and git constructs it read differently
from bash, zsh or git) and tests that could not fail. The seventh run's were six untested branches
and one bypass (a zsh function defined through `functions[…]=`). The eighth run's were six more
spellings the walker misread (a quoted or escaped `export`, `=env`, a piped script with shell
options, `rebase -x'…'`, `watch`, git's upload-pack and receive-pack commands) and two gaps in the
gate scripts (a local branch named `origin/main`, ignored source files). The ninth run, on a tree
with a net under the walker that exempted git's own subcommands and data commands, found 16 more:
ways through the exemptions, a push from another clone of the project, a nil heredoc body that
crashed the parser, and more spellings (`chdir`, `EDITOR`, `send-email`'s command options).

The counts did not converge, so the owner chose to narrow the hook instead of modelling each new
spelling (`dd02f04`). The text net now refuses, in any command and with no exemptions, a command
line that names `--no-verify` or an abbreviation of it, `core.hooksPath`, `.githooks`, a `GIT_*`
variable outside the safe list or git's push plumbing, in its text and in each command's words as
bash expands them; a push runs only from this clone or its worktrees; and the walker stays for
early, clear refusals. The guide now says the hooks catch mistakes and shortcuts, and that branch
protection (#220) is the boundary against deliberate evasion.

The tenth run, on the narrowed hook, found 3 blocking findings, all tests that could not fail:
rows for `pushd -n` and `popd` that passed whatever the walker did, and no test of seven fail-closed
refusals or of a refusal inside a command substitution. Rather than fix only those, a mutation sweep
(above) found every refusal no test caught; all now have one. Its 7 minor findings were fixed too:
a shell the command runs did not inherit an earlier `source` or dynamic `eval`; sudo's `-A`, `-D`,
`--chdir`, `-R` and `-i`; the gate-files check missed files marked assume-unchanged or skip-worktree
and read a git error as clean; the wrapper and the settings' hook command let any status but 2 run
the command (a killed binary, a wrapper that is not executable); CI downloaded the CodeQL bundle
without checking its SHA-256; the text net's refusal named a Grep tool this Claude Code build lacks;
and a test row that `filepath.Join` reduced to another.

The eleventh run found 5 blocking findings. Two were gaps: the text check behind the walker's
fail-closed refusals missed `git -C DIR push`, `git -c K=V push` and git called by its path, so a
push behind a dynamic `eval` or a non-literal shell script written that way was allowed; and a
linter configuration from outside the tree (an rc file in HOME or a parent directory, or an ignored
dot file such as `.shellcheckrc`) could make `ci-static` pass where CI fails while the tree still
counted as clean. Three were branches no test reached that the first sweep did not cover (a push's
own refusal returned by the gate in this clone, a short option's attached value that is not
literal, zsh's in-shell cd at the end of a function's pipeline). All were fixed, with the 10 minor
findings: a wrapper or xargs with a non-literal command word before `push`; a cd in an `if` or
`while` condition leaking into branches that run when it failed; editor and pager variables set
from a value the hook does not read; the Go tests reading the developer's global git
configuration; `git --help push`; the gate test not pinning the commit it checks; an unknown short
push option and a subsection key under `diff.` untested; and replace refs or `git fast-import`
making a change look like documentation only (the gate's scripts now ignore replace refs, and the
hook refuses both commands).

The twelfth run found 4 blocking findings: no test pushed more than one ref; a committed change to
the gate was judged by the changed gate alone (now the gate of the change's base judges a change to
the gate's own files too, in both hooks, each implemented on its own); `git -C link/..`, `cd -P` and
`~` after a HOME set in the command were resolved unlike git and the shell (now `..` stays as
written for git to resolve physically, and `~` is not literal once HOME is set); and a `go.work` in a
parent directory could replace the tree's modules in every Go check (now `GOWORK=off`). Of its 8
minor findings, 7 were fixed (the duplicate path finding, the shell lint test that passed for the
wrong reason under bash 3.2 and the bug behind it, sarifgate's untested report order, driver rule
levels and error paths, parity status changes skipping the review, the format hook erroring where
the checkout has no formatter); the eighth, a git-lfs version floor for custom images, is product
hardening outside this change and is filed as #230. The review of the tree that contains this
record runs after it is written; its result is in the pull request.

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
before running it. An eighth run, on `36cc2ff`, stopped in `ci-static` with `Error: parallel
golangci-lint is running`: a review agent ran golangci-lint at the same time, and golangci-lint
refuses to run twice at once. Check runs now wait until no review agents run. The run on the tree
that contains this record is in the pull request.

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
  aliases, functions or options, the content of scripts, or code given to an interpreter, and a
  token assembled at run time gets past its net. Branch protection on `main` (#220) is not on yet.
- The red-team runs were on trees before the narrowing; none has run since.
- The mutation sweep disables refusal sites only; it does not show that the walker allows every
  ordinary command (the allowed-command tests do that for the commands they list).
