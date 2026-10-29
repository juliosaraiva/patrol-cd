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
| any other change | `make ci-unit` (`-race`, envtest, the strict skip policy) on the packages of the changed files, a data file counting for the nearest package above it; every package when `go.mod`, `go.sum` or a file outside every package changed (`examples/`, `deploy/`, a script, the Makefiles: a test anywhere may read it, `test/examples` reads `examples/`), except the UI's sources and the agents' instructions, which no Go test reads; off Linux, when Go files changed, `go vet` of every package for Linux too, since files built only for Linux compile in no other step there |
| `test/integration/` | `make ci-integration` |
| `deploy/` or `test/deploy/` | `make ci-deploy` |
| `hack/kind/`, `test/e2e/`, `deploy/crds/` or `api/v1alpha1/` | `make ci-scripts` (its `validate.sh` checks the e2e fixtures against the CRDs) |
| `ui/` or `api/openapi/` | `make ci-ui` (the UI's API types are generated from the OpenAPI document) |
| `Dockerfile`, `hack/images/`, `go.mod` or `ui/package.json` | `make image-verify-pins VERIFY_PINS_FLAGS=--offline` (the image's pins; `ci-image` runs the rest of the image checks on CI) |
| Go, JavaScript/TypeScript or workflow files | CodeQL for those languages (below) |

A documentation-only change (the rule of `hack/ci/classify-changes.sh`: `docs/`, `*.md` at the root,
`.gitignore`) runs the first row only. The script stops at the first failure.

The linters read no configuration from outside the tree (`golangci-lint run --no-config`,
`shellcheck --norc` with `SHELLCHECK_OPTS` cleared), so a file in HOME or a parent directory cannot
make a local run lenient where CI's is not; Go runs with `GOWORK=off`, so a `go.work` in a parent
directory cannot replace the tree's modules; inherited make flags and `MAKE` are dropped (`-k`, or
`MAKE=true`, which would empty every recipe that runs `$(MAKE)`); and the gate's scripts ignore replace refs and grafts
(`GIT_NO_REPLACE_OBJECTS`, `GIT_GRAFT_FILE`), which could make a commit read with another commit's
tree or parents. A run records nothing when HEAD moved during it (a checkout, reset or stash, even
one undone before the end, which HEAD's reflog shows), or when the environment sets what can weaken
a check: a make variable the makefiles (and those they include) define with `?=`, even to the
empty string, which make takes as defined (an empty `CI_RACE` drops `-race`, an empty `ACTIONLINT`
lints nothing), `CI_GO_TEST_FLAGS`, `GOFLAGS` (also from Go's env file), or `BASH_ENV` or an
exported bash function, which every bash the checks start reads, where a function can stand in for
a tool. Some distributions export one in every login shell (Fedora's `which`, Lmod's `module`):
un-export it (`export -nf which`) before the run.

When every check passed on a **clean** tree, it writes a record for HEAD's tree
(`git rev-parse HEAD^{tree}`) to `$(git rev-parse --git-common-dir)/patrol-sdlc/checks/<tree>`. A
dirty tree is checked but not recorded; the run lists what makes it dirty: a change, an untracked
file, a file marked assume-unchanged or skip-worktree, or an ignored file the checks read, which
`git status` does not show (a UI build that `make ui-build` copied into `internal/ui/dist`, which Go
embeds: `make ui-clean` removes it; a `.env` file under `ui/`, which Vite and vitest read). `--no-codeql` (`make sdlc-check SDLC_CHECK_FLAGS=--no-codeql`,
for quick iterations) records nothing either. Records are keyed by tree, so amending a commit or
adding one needs a new run, and rebasing onto a new `main` changes the tree too.

The test suites need what CI installs: envtest assets (`make envtest`), kustomize, helm, git-lfs,
gpg and sshd, and `make ci-static` needs jq (its SDLC tests feed the Claude Code hook with it);
`hack/ci/preflight.sh static unit` lists what is missing. A missing tool turns into a forbidden skip and fails the run, as on CI
(`hack/ci/allowed-skips.txt`); install it (for example `brew install git-lfs`).

## CodeQL

CodeQL runs locally before every push and on GitHub. Its license allows that for this repository:
the CodeQL CLI may analyze an open-source codebase (one released under an OSI-approved license,
here Apache-2.0), and automated analysis in CI is allowed when that codebase is hosted on GitHub.com.
The repository is public since 2026-10-23 for this reason; on a private repository the same use
would need a GitHub Code Security license.

- **Bundle.** `hack/sdlc/codeql.sh install` (`make codeql-install`) downloads CodeQL 2.27.3 into
  `bin/codeql-2.27.3/` and checks its SHA-256 against the pin in the script and the publisher's
  checksum file (a release asset can be replaced under its tag); `hack/sdlc/codeql.sh fetch DIR`
  downloads and checks the tarball only, as CI does. macOS and Linux x86-64 only (CodeQL ships no Linux arm64 bundle); about 2 GB
  unpacked; needs `zstd`.
- **Analysis.** `hack/sdlc/codeql.sh analyze [go] [javascript-typescript] [actions]` (`make codeql`,
  `CODEQL_LANGUAGES=go make codeql`) builds a database per language (Go by tracing `go build ./...`;
  JavaScript/TypeScript across the repository and GitHub Actions workflows without a build, with the
  exclusions of `.github/codeql/codeql-config.yml`) and runs the `security-extended` suite. SARIF
  files and a summary go to `dist/codeql/`.
- **Gate.** `hack/sdlc/sarifgate` fails on every result of level `error` or `warning` that is not
  suppressed (a suppression with no status or the status `accepted`; a rejected one, or one under
  review, suppresses nothing) and not accepted in `hack/sdlc/codeql-baseline.txt`; notes are listed
  but do not fail. A baseline line names the rule, the file, CodeQL's line-independent fingerprint
  and the reason. Prefer fixing: accept an alert only when it is a false positive or a deliberate
  design, and dismiss it on GitHub with the same reason.
- **On GitHub.** `.github/workflows/codeql.yml` fetches the same bundle with `codeql.sh fetch`
  (checked against the same SHA-256) and runs it with the same suite and configuration on
  pull requests (not documentation-only ones), on pushes to `main` and every Monday, and uploads the
  results to code scanning (Security tab), where new alerts show on the pull request.

## Agent review: the `review-change` skill

`/review-change` (or asking Claude Code to review the branch) runs the saved workflow
`.claude/workflows/sdlc-review.js` on the branch's diff:

1. `hack/sdlc/review-input.sh` names the tree, the base, the changed files and writes the diff, as
   committed: no attribute (`-diff`, `binary`) or textconv driver replaces a file's content in it.
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
replace them. They run as the change has them: a change to the workflow, the agents' instructions
(the verifier's severity rules among them), the skill or the scripts that prepare and record the
review is reviewed by its own version, not the base's (the Limits).

## The push gate

`hack/sdlc/gate.sh COMMIT` decides whether a commit may be pushed. It needs, for the commit's tree:

- a passing check record, always;
- a passing review record, unless the change since the merge base is documentation only (the rule
  above, except that `docs/parity/`, and `CLAUDE.md`, `CLAUDE.local.md`, `AGENTS.md` and anything
  under a `.claude/` directory at any depth, in any case, are not documentation here: a status file
  that marks a feature complete needs its evidence reviewed, and the agents' instructions their
  review; Claude Code loads a nested `CLAUDE.md` or skill for the files under it, and on a
  case-insensitive file system, as macOS's default, `claude.md` is that file).

A change to the gate's own files (`.githooks/`, `hack/sdlc/`, `.claude/settings.json`) is judged by
the gate of its base too (the merge base with `origin/main`, run from the base's files), so a gate
the change breaks cannot pass the change; so is any push made while the checked-out branch has
changed those files, since the gate that runs is the work tree's. Both hooks do this, each on its
own, and both refuse a push while one of those files is marked assume-unchanged or skip-worktree,
which would hide an edit of it from that comparison. A push the base's gate
refuses (because the change also changed what the records hold, say) is made by the owner. This
covers the gate's decision, not the checks behind a record: a change that weakens
`hack/sdlc/check.sh` writes a passing record with its own check.sh, which the base's gate accepts;
the agent review and CI stand behind such a change.

Two hooks run it:

- **git's pre-push hook** (`.githooks/pre-push`, enabled with `make sdlc-git-hooks`, which sets
  `core.hooksPath` for the clone and, with `extensions.worktreeConfig`, for the work tree it runs in,
  whose own value then wins). It is the authority: git gives it the commits actually being pushed, so a
  commit made in the same command line (`git commit … && git push`), a push from a script, an
  alias or another tool are all checked when git runs in the work tree (a push given the git
  directory from outside it is not: the Limits). A person can bypass it in an emergency with
  `git push --no-verify`.
- **the Claude Code hook** (`hack/sdlc/claude-pre-push.sh`, a `PreToolUse` hook in
  `.claude/settings.json` for the tools that run commands: Bash, Monitor, the terminal, and the browser
  pane's `preview_start`, whose command is the `.claude/launch.json` configuration it names). While
  git's hook is not enabled it refuses every command but `make sdlc-git-hooks`, so git's hook runs
  whatever Claude Code pushes; that command runs even when the hook's binary cannot be built (a
  fresh clone offline, Go missing from the hook's PATH). Then every command goes to
  `hack/sdlc/pushcheck` (built into `bin/` on first use; while it cannot be built, a command that
  mentions git or what would skip its hook is refused, and the others run). Claude Code blocks a command only when a hook exits with status 2, so any other failure
  is turned into 2: the wrapper's, when the binary dies, and the settings' hook command's, which runs
  the wrapper with `bash` (an executable bit does not matter) and lets commands run only where the
  checkout has no wrapper (a commit from before the gate). Its job is to keep git's hook from being
  skipped by mistake or by a shortcut, and to
  refuse early, with a reason, a push the gate would refuse. It is not a boundary against someone who
  sets out to defeat it (see Limits). It works in three layers.

  1. **The text net.** A command line that names what could keep git's hook from running is refused,
     whatever the shell would do with it, in any command and with no exemptions: `--no-verify` and
     every abbreviation git accepts for it, `core.hooksPath` in any case, the `.githooks` directory, a
     `GIT_*` variable outside a short safe list (the author and committer variables,
     `GIT_TERMINAL_PROMPT`, `GIT_TRACE*`, `GIT_LFS_SKIP_SMUDGE` and a few more; `GIT_EDITOR`,
     `GIT_PAGER` and `GIT_SEQUENCE_EDITOR` with a literal value that is safe shell code), and git's
     push plumbing (`send-pack`, `receive-pack`, `upload-pack`, `http-push`, and the remote helpers
     `remote-http(s)`, `remote-ftp(s)`, `remote-ext`, `remote-fd` and `git-remote-*`, which run
     send-pack themselves). Hook input it cannot read is refused too. The net reads the command
     line as written, and each command's words and heredoc bodies as bash expands them, so
     `$'\x2d-no-verify'` counts too. The hook cannot tell what a program, shell code, a shell alias or
     function (oh-my-zsh's `gp='git push'`) or an option it does not model does with such a token, so
     it does not try: text that only mentions one (a commit message, a pull request body) is written
     with the Edit or Write tools or into a file (`git commit -F FILE`,
     `gh pr create --body-file FILE`), and a search for one uses Claude Code's Grep tool where it has
     one, or a quoted pattern that does not spell the token out (`grep -rn 'hooks[P]ath'`).
  2. **The walker** parses the command as bash with `mvdan.cc/sh` to find each `git push` and the
     directory it runs in, and gives a clear reason for what it refuses.
  3. **Where a push may run.** From Claude Code a push runs only in this clone or one of its
     worktrees (the same `git rev-parse --git-common-dir`): another clone, a bare repository, a `.git`
     directory or an unrelated repository may not run git's hook, so a push there is refused, dry runs
     and deletions included. So is code the walker could not read, which may push: a command word it
     does not know (below), which may be an alias that pushes (oh-my-zsh's `gp`); a shell reading a
     script from standard input it did not read (`(cd X && sh) <<EOF`, `… | env sh`, `sh < <(…)`), or
     from the input of `xargs` or `parallel` (`xargs sh -c`, `xargs -I{} sh -c {}`, `parallel` with no
     command); a shell given a script it cannot read (`sh -c "$X"`); `source` of standard input or of
     a word it cannot read; an `eval` of a dynamic string; a command word that is not literal. Such
     code runs only in a work tree of this clone where git's hook runs the gate as committed
     (`core.hooksPath` is `.githooks`, the hook and `hack/sdlc/gate.sh` are there, and the gate's own
     files have no uncommitted or hidden change),
     which judges what it pushes. It is placed where it runs: code in a function body at each call
     of the function, in the call's directory moved by any `cd` in the body before it (from an
     unknown directory once `CDPATH` or `HOME` is set, which may take that `cd` elsewhere, and for
     a function the shell calls on its own, `chpwd` or `TRAPDEBUG`); code in a
     substitution in the directory of its statement. A
     command that does not parse as bash is refused outright. In
     this clone a push is refused while `core.hooksPath` is not
     `.githooks`, while `.githooks/pre-push` is missing or not executable, while the gate's own files
     (`.githooks/`, `hack/sdlc/`, `.claude/settings.json`) have uncommitted changes, and from a branch
     without `hack/sdlc/gate.sh`.

  The walker's contract:

  - **What it models.** Words are expanded as bash does when the result does not depend on the
    environment, the filesystem or another command: quotes, escapes, `$'…'` and brace expansion, so
    `git p""ush` and `git push {origin,main}` are read as bash runs them. A word with a variable, a
    substitution, an unquoted glob or an unescaped `$` that bash leaves as text is *not literal*. The
    Bash tool may run zsh, so zsh's readings win where they differ: `{git push …}` without spaces is a
    brace group, `$=x`, `$^x` and `$~x` are not literal, `chdir` is `cd`, and a lone directory name is
    a `cd` (AUTO_CD) unless it is a builtin or function; zsh's `-` precommand is read as an unknown word, since an alias of that name
    (oh-my-zsh's `cd -`) runs first. `command -p` and `--`, and `builtin --`, run the command that
    follows. Text in quotes, heredoc bodies and comments are not
    commands. `cd`, `pushd` and `git -C` move the directory only where they certainly run: inside a
    pipeline, a background job, a subshell, a substitution or a command run by `env`, a wrapper,
    `find -exec` or `kubectl exec` they do not reach the commands after them; a `cd` that may or may
    not run (after `||`, in an `if`, `case`, loop or function, or in an `if` or `while` condition,
    whose branches can run whether it worked or not) leaves the directory unknown; and so does a
    `cd` that may fail without stopping the command (`cd DIR; git push`), unless `&&` or `|| exit`
    guards it (the commands of such an exit fallback, `|| { …; exit 1; }`, run from an unknown
    directory); sudo's `-D`, `-R` and `-i` run the command from an unknown directory; a relative `cd`
    after `CDPATH` (or zsh's `cdpath`) is set, in any form and also within a word (`${CDPATH:=…}`,
    taken in the order the shell expands it), goes to an unknown directory. A `cd` operand
    with `..` is resolved both ways, lexically as bash's default cd does and as the kernel does
    (after a symbolic link, the parent of its target, where `cd -P` and a shell made physical go):
    when the two differ, the directory is unknown. `git -C` is a chdir, so its path stays as written
    for `gatePush`'s stat and git to resolve. zsh's `emulate -c`
    and `repeat` run their code in this shell, and a glob qualifier that runs code (`*(e:…:)`,
    `*(+fn)`) runs it as zsh globs, before the command, wherever zsh globs (arguments, assignments
    and arrays, a `for` list, a redirection): the directory is unknown from that command on, and a
    push in its code is refused. A program on PATH runs as a child, whose cd cannot move the shell;
    any other command word (not a builtin the walker knows, not a function the command defines) may
    be a function or alias of the user's shell setup, which the Bash tool loads and which can cd
    (zoxide's `z`, oh-my-zsh's `...` and `take`) or push (oh-my-zsh's `gp`), so the directory after
    it is unknown, it may run only in this clone (above), and a git subcommand in its arguments is
    checked as git's own would be. `enable`, `disable`, `functions` and `autoload` leave each name
    they touch unknown too (`functions -c f ls`, `autoload ls`). A function
    the command defines counts only where its definition certainly runs in this shell (a statement
    of its own, not in a subshell, branch, chain, pipeline, the background or `eval`), and a call of
    it may cd wherever its body, or a function it calls, may; it runs instead of the builtin or
    program of its name (`echo`, `true`, `nohup` …), except after `command`, `builtin`, `exec` or
    `env` and in a wrapper, which run programs, and a push in a call's arguments is refused (the
    function may hand them to git or a shell). A definition that may not run, and whose body may
    cd, leaves its name unknown; a function whose body may cd may also run in a shell the command
    starts (a subshell, bash's `export -f`). A definition chained with `&&`, `||` or a pipe
    (`f() { …; } && cd /x`) is one element of the chain; the rest runs in this shell. It follows
    the command a wrapper runs (`timeout`, `nice`, `sudo`, `nohup`, `env`, `xargs`, `find -exec`
    and `-ok`, `kubectl`/`docker exec -- …` (where a push is refused, below), `command`, `exec`, …)
    into that command, and a shell
    given a script among the arguments of a program it does not model (`arch -arm64 sh -c '…'`,
    `script -q /dev/null bash -c '…'`, from an unknown directory after a word it does not know), and parses `bash -c '…'`,
    a heredoc or here-string given to `bash`/`sh`/`zsh`/`dash`/`ksh`/`mksh` or to `source
    /dev/stdin` (also one `xargs` runs, and a function definition's heredoc, read at each call of the
    function), a literal script piped to a shell (whatever the shell's options, also behind a wrapper:
    `env sh`, `nice sh`; `echo` text with a backslash is not read as written, since zsh's echo
    expands `\n`), `eval`, `watch` (its operands as `sh -c` runs them,
    or its argv with `-x`), and the commands git runs for a subcommand (below). A `git push` found
    this way is gated like a direct one. A function body or `trap` action runs later, from wherever
    the shell is then: a push in it runs from an unknown directory, and a `cd` in it leaves the
    directory unknown after each call of the function. A function runs the functions it calls as
    they are defined when it is called (bash and zsh look a function up then), so its `cd` is
    followed through them at each call, and one that calls a function defined, or defined again,
    after it runs code the walker did not read: like a word it does not know, from the call or, if
    it may cd, from an unknown directory. A function that runs code given at run time (`"$@"`, `$1`,
    `eval`, also through `noglob`, `builtin` or `repeat N`) runs its arguments as a command in this
    shell: a `cd` among them (`r cd DIR`) moves the directory, wherever the call is (in a body, at a
    pipeline's end, given to another such function), and from any of them when the body shifts them,
    slices `"${@:N}"` or runs later positionals (`"$2"`); a function that passes its arguments to
    such a function is one too, whichever is defined first, and so is one that may be defined (in a
    chain or a branch); a function named there, as among
    the arguments of a word the walker does not know, runs there too. A call of a function the
    command defines whose name is no program on PATH or builtin is also a word the walker does not
    know, so it runs only in this clone: an alias of that name in the shell's setup would run
    instead (zsh expands aliases before it looks functions up; oh-my-zsh's `gp` is `git push`). A
    trap action, like a function the shell calls on its own, is read again after each definition,
    since it runs the functions as they are defined when it runs. bash 5.3's `${ …; }` and
    `${| …; }` run in this shell, which the walker does not follow, so they are refused. Each
    function, and the command, may hold code the walker did not read in at most 256 places (each
    body holds those of the functions it calls); past that the command is refused, so a long chain
    of functions cannot run the hook past its timeout. A `trap` action, or a function the shell
    calls on its own (zsh's `chpwd`, `precmd`, `preexec`, `periodic`, `TRAPDEBUG` and the other
    `TRAP…` functions, a command-not-found handler), that may cd, or a `trap` action the hook cannot
    read, can move the directory between any two commands (a DEBUG trap runs before each, an ERR trap
    after a failing one), so a push after it is refused and anything else after it runs from an
    unknown directory, even after a `cd`. `trap - SIG`, `trap SIG` and `trap N …` reset signals and run
    nothing. At most 64 trap actions are followed (each is read again after each definition); past
    that the command is refused. zsh runs an
    anonymous function at once, in this shell, and the walker follows it there (`() {cd /x}`,
    `() cmd`); the spaced form `() { …; }` does not parse as bash, so a command with it is refused.
  - **What it refuses wherever the command runs**, because it can point git elsewhere, run a push the
    gate cannot see, or move what the gate measures from:
    - a push with an argument that is not literal (it could be `--no-verify`);
    - a push run in a container (`kubectl exec … -- git push`, `docker exec ctr git push`): it
      pushes the container's clone, whose commits the gate does not judge and where git's hook does
      not run. A shell run there (`docker exec ctr sh -c '…'`, `kubectl exec -i pod -- sh <<EOF`, a
      script piped to it, also behind a wrapper: `… | sudo docker exec -i ctr sh`) is refused when
      the command's text names a push, with or without `--`, also as the command of `xargs`, `git
      bisect run` or `submodule foreach`; a script piped to `ssh host sh` is refused as a push run
      on another host, as is a push in any `ssh` command (a quoted command, `sh -c`, a heredoc);
    - a `GIT_*` variable outside a short safe list set in the shell (`GIT_DIR`, `GIT_WORK_TREE`,
      `GIT_CONFIG_*` …): inline, with `env` (also after `--`, and with a value that is not
      literal), `export`/`readonly`/`declare` (also quoted, `"NAME=VALUE"`, and through a
      nameref), `read`, `printf -v`, `let`, arithmetic or `${NAME:=…}`, but not merely printed.
      `GIT_EDITOR`, `GIT_PAGER` and `GIT_SEQUENCE_EDITOR` pass with a literal value that is safe
      shell code, since git runs it through `sh`, and so do `EDITOR`, `VISUAL` and `PAGER`, which git
      falls back to; set from a value the hook does not read (`read`, `printf -v`, a `for` loop),
      they are refused;
    - `git -c`, `git --config-env` and `git config` writes of keys outside a short safe list
      (`core.hooksPath`, `alias.*`, `include.*`, `diff.external`, `diff.<driver>.command` …; `git
      -c` may set `core.editor`, `core.pager` and `sequence.editor` as their `GIT_*` variables may
      be set), read as git reads them (without a subcommand, an option after the
      key is a value: `git config KEY VALUE --get` writes); `--git-dir`, `--work-tree`, `--bare`, `--shallow-file` and other
      global options before `push`; `git send-pack`, `git http-push` and git's remote helpers
      (`git remote-*`, `git-remote-*`, also handed to a function or an unknown word), which push
      without the hook;
    - a command git runs for a subcommand that holds a push, or that the hook cannot read:
      `rebase -x`/`--exec` in every spelling git accepts (`-x'…'`, `-ix …`, `--exe=…`), `difftool
      -x`, `filter-branch`'s filters and `--setup`, `send-email`'s command options, `bisect run`,
      `submodule foreach` (one word as a script, several as the command), `for-each-repo`, the upload-pack of `fetch`, `pull`, `clone` and
      `ls-remote`, push's receive-pack and `--exec`, `archive --exec`, `grep -O`;
    - a write to the ref the gate measures a change from: `update-ref` or `symbolic-ref` under
      `refs/remotes/` (`symbolic-ref` with one name and no `-d` reads it, and passes), a `fetch` or `pull` refspec into `remotes/`, any of them reading refs or
      refspecs from standard input (`--stdin` in every abbreviation git accepts),
      `git remote add`, `remove`, `rename`, `set-url` or `set-branches`, and `git fast-import` and
      `git replace`;
    - each git command of the items above also when git is handed to a function the command defines
      or to an unknown word (`f git config alias.x …` with `f() { "$@"; }`);
    - a git subcommand that is not a git command: an alias, whose definition the command could
      change on the way (a config file it writes, `HOME`, `XDG_CONFIG_HOME`), also among the
      arguments of a function the command defines or of an unknown word (`retry git ship`). A
      `git-*` program run by its own name, and git run in a container by `kubectl`/`docker exec`, are
      not alias lookups here;
    - a shell alias or zsh function the command defines (`alias`, zsh's
      `functions[…]=`/`aliases[…]=`, also through `typeset`) whose code runs git or changes the
      directory, or whose name is one the walker reads as a builtin or program it models (`cd`,
      `exit`, `git`, `eval` …); one whose name or code it cannot read (`alias "$n=…"`,
      `functions[$n]=…`, or zsh's code parameters set by `set -A`, `read -A`, `printf -v`, a loop or
      arithmetic); a function zsh calls on each cd or before each command (`chpwd_functions`, and the
      other hook arrays) whose code may cd; `enable`, `disable`, `functions`, `autoload` aimed at such a
      name, a hook function or a word it cannot read (`disable cd`, bash's `enable -n cd`, zsh's
      `functions -c f cd`), and `add-zsh-hook`; and `hash` of a name to another program for git;
    - a file sourced, or an `eval` of a dynamic string (`eval "$(ssh-agent -s)"`), before a push; a
      push from an unknown directory;
    - a push it cannot resolve: inside `$(…)`, behind `eval` with a dynamic string, through a
      non-literal command word, git option or shell script, piped through `xargs` or `parallel`, in
      a script piped to a shell by a producer it cannot read (`printf`, a chain through `tee`), or
      in what `source` reads from a word it cannot read (`. <(…)`). Its text check for these reads
      git's global options and their values (`git -C DIR push`, `git -c K=V push`) and git by its
      path, and reads a backslash both as bash removes it and as printf and zsh's echo expand it
      (`\n` a line break).
  - **What it refuses for a push in this clone:** an unknown `git push` option, and the forms the
    gate cannot check ref by ref: `--all`, `--branches`, `--mirror`, `--tags`, `:`, a glob.

  Then it runs the gate on each pushed commit as it is before the command runs, which gives an early,
  clear refusal; git's hook catches a commit the same command creates, and a ref that does not exist
  yet (a branch or tag created earlier in the command) is left to git's hook too. Deletions, dry runs
  in this clone, `git push -h` and reads such as `git config --get user.name` pass. Commands that do
  none of the above run untouched, whatever they say about git push in a branch name, a path, a
  pattern or a variable.

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
  one, and CI does not read them. CI runs its own checks. They are plain files that anything running
  as the user can write (a command, the Write tool), so the gate is only as honest as what writes
  them; `check.sh` and `record-review.sh` are the only writers the SDLC uses.
- The Claude Code hook catches mistakes and shortcuts, not deliberate evasion. It sees the command
  line, not what the command builds at run time or the files and programs it runs: a token or a git
  command assembled from variables or computed by a program; the content of script files and
  Makefiles; code given to an interpreter (`python -c`, `perl -e`, `awk`); commands on a remote host
  (`ssh host …`); and the user's shell setup, which the Bash tool loads (aliases and functions such
  as oh-my-zsh's `gp='git push'` or zoxide's `z`, options such as CDABLE_VARS). Its text net refuses
  a hook token anywhere in the command line, so a push through any of them still runs git's hook
  unless the token is assembled at run time; and a command word that is no program on PATH makes the
  directory unknown and may run only in this clone (a function the command defines of such a name
  may run only there too, as an alias of the setup would replace it, but the directory after it
  stays known: an alias that cds, as oh-my-zsh's `-` or a `dev='cd ~/Developer'`, given the name of a
  function the command defines, moves a later push unseen), so a function that cds to
  another clone does not place a push in this one, and an alias that pushes does not push from
  another one. An alias
  or function that shadows a program on PATH or a builtin (`make`, `ls`, `echo`) is not seen, nor
  is an alias of the setup that skips git's hook on its own (one that runs `git push --no-verify`). git's
  pre-push hook (required before any command runs) checks the pushes they make in this clone's work
  trees; a script or program that disables git's hook on purpose gets past both.
- The agent review runs the workflow, agents and skill of the change under review, not those of its
  base: unlike the gate, which a change to the gate's own files also faces in its base's version, a
  change that loosens the reviewers or the verifier (what counts as minor, say) is judged by the
  loosened rules. A change that weakens both `hack/sdlc/check.sh` and the review has no local check
  behind it; CI's own checks and the owner's review of the pull request stand behind it.
- Scope of the walker (the owner's decision, after reviews kept finding new constructed evasions): it
  is held to the commands a developer or agent writes for their own sake. A misreading that needs a
  command built to get past it (a script piped into a wrapped shell, chains of runner functions, a
  function redefined between calls, runners of positional or sliced arguments, an alias that shadows
  a function) is a documented limit: the review rates it minor, and git's hook still checks every
  push from this clone. An ordinary command that pushes from another clone or skips git's hook
  unseen, an ordinary command wrongly refused, or a fault in the gate itself (`gate.sh`, git's hook,
  the hook's gates, the text net) still blocks a push (`.claude/agents/review-verifier.md`).
- `core.hooksPath` is the relative `.githooks`, which git resolves against the directory it runs
  hooks from: the top of the work tree it found or, when it is given the git directory (`--git-dir`,
  `GIT_DIR`) without a work tree, its current directory; inside `.git` it has no work tree. So git's
  hook does not run for a push given this clone's git directory from outside the work tree
  (`git --git-dir=…/.git push` from another directory, `cd .git && git push`). The Claude Code hook
  refuses those forms on a command line, not in a script or another tool. An absolute path would
  need `extensions.worktreeConfig` for linked worktrees and stop working, silently for git, when the
  clone moves; #231 tracks a fix.
- The Claude Code hook checks the gate, and `core.hooksPath`, as they are before the command runs. A
  command line that commits and also turns git's hook off on the way without naming it (a program
  or an editor that rewrites `.git/config` or the hook) pushes the new commit unchecked.
- golangci-lint, shellcheck and actionlint are pinned to the tree's configuration; the UI's ESLint and
  TypeScript find the tree's own files first. A check added later that reads a configuration file
  from a parent directory or HOME needs the same pinning, or a local run can judge differently from
  CI.
- The gate measures a change from `refs/remotes/origin/main` as this clone has it. A command that
  moves that ref other than through the forms above (writing the ref file) changes what counts as
  the change, and a change that then looks like documentation only skips the review.
- The text net also refuses harmless commands: a commit message or pull request body that names a
  token, a search for one, `go test -run` with one in a test name. Write those with the Edit or Write
  tools or into a file, and search as the text net's refusal says.
- A hook that runs past its timeout (120 s; a first build of `hack/sdlc/pushcheck` that downloads
  modules can) may let the command run, as Claude Code treats a timeout as a hook error rather than
  a refusal; git's own hook still gates a push.
- `preview_start` is checked only with `jq` installed; without it the call is refused, and the whole
  hook input stands in for a Bash command (so `make sdlc-git-hooks` must then run outside Claude
  Code).
- The hooks keep a cooperative process honest; they are **not a security boundary**. That boundary
  belongs on GitHub, in branch protection or a ruleset on `main` that requires CI, CodeQL and a
  review (#25, #220). None applies yet: until the owner turns it on, a push to `main` that gets past
  the local hooks lands there directly, and the owner's merge decision on a pull request is the only
  other check. GitHub's API writes commits and moves branches without any push (`gh api` on a
  repository's `contents`, `git/refs` or `merges`, a pull request's update-branch), so neither hook
  sees it; only branch protection would.
- A check run takes most of an hour, and records for the tree it started on. A file edited during
  the run and restored by hand before its end is not seen (HEAD moving is).
- The checks run the tools on PATH, `bin/` first (which the clean-tree rule does not inspect), and
  check no tool's version: off Linux, `hack/ci/install-tools.sh` does not install the pins of
  `hack/ci/tools.lock`, so a local golangci-lint or shellcheck of another version can pass what CI's
  fails, or fail what it passes. CI runs the pinned tools.
- The hooks come from the checked-out tree: while a commit from before the gate is checked out, they
  are not there to run.
- The check record is keyed by the tree. Two of its checks also read history (commit subjects,
  gitleaks over the branch's commits): a commit reworded or re-parented onto a checked tree passes
  the gate without them; CI runs both again on the pull request.
- The checks run the tests of the changed packages, not of the packages that import them; CI's full
  tier runs everything on every push to `main`.
- CodeQL `security-extended` finds known vulnerability patterns; it does not prove their absence.
- CodeQL's local Go database is built for the host: on macOS, files built only for Linux
  (`internal/sandbox/rlimit_linux.go`) are not in it, and the darwin variants are. A new alert in
  Linux-only code shows first in CI's CodeQL upload, which gates nothing until branch protection does.
- A function or program a sourced file defines (`source hack/kind/lib.sh && patrol::cluster_exists
  x`) runs from a directory the hook cannot tell, since the file may cd: in this clone it is refused
  unless a literal `cd` into the clone comes after the `source`, which the refusal says.
