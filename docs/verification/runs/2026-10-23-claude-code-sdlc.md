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
| `go test -race -count=1 ./hack/sdlc/...` | passed: `pushcheck` and `sarifgate`, 731 tests (counted with `-json`) |
| `bash hack/sdlc/test-sdlc.sh` (the gate end to end in scratch repositories with a bare remote, and check.sh with stub make targets) | passed: 213 passed, 0 failed; also with `GOFLAGS=-mod=readonly` (as CI sets it), `PATROL_SDLC_BASE=HEAD~1` and `CI_GO_TEST_FLAGS=-short` set |
| The suite in a scratch copy of the tree with the review diff's `--text --no-textconv` removed | its two new checks fail (`-diff` and a textconv driver hide main.go's content) |
| `GOOS=linux CGO_ENABLED=0 GOWORK=off go vet ./...` on macOS (the step check.sh now adds off Linux) | passed, in 3 minutes from a cold cache |
| A graft making HEAD a root and origin/main its child, in a scratch repository (`graft-check.sh` in the scratchpad), and pushcheck without its graft setting (`graftmut/mutate.py`) | without `hack/sdlc/lib.sh` the changed files are `[]`, with it `[main.go]`; the mutant fails `TestGatePushRunsTheBaseGate` |
| `/bin/bash hack/ci/lint-shell.sh` (bash 3.2) in a scratch repository with no POSIX sh script | reports SC2086 in its bash script; the version before this change stopped with `sh_files[@]: unbound variable` |
| Mutation sweep of `hack/sdlc/pushcheck` (a scratchpad script: each refusal, unknown-directory, environment and option-value site of `parse.go`, each `return true` of a predicate, each refusal or early allowing return of `main.go`, and its loop over the pushed sources, disabled or narrowed in turn through `go test -overlay`, the repository untouched) | 154 sites; 151 caught by a failing test, 2 do not compile (the base gate's refusal, which a test asserts, and the extraction's error path); the survivor is an error path of the environment in `baseGate` (git failing to name its directory right after it worked), and two, on the last runs, were code no test could reach: the value a `syntax.Walk` callback returned as it left a node, which Walk ignores, and then the whole after-the-walk probe of a call the statement walk had not reached, which the twentieth round's statement-time walk of words made dead; both were removed. On the twenty-second round's tree, checkSubcommands' refusal of git's commands that push without the hook survived: the walker now refuses them wherever it finds them, before they reach the list; it stays as a second line, with a unit test. The first sweep, on `0b31e0f`, covered refusal sites only and left 20 in `parse.go` and 5 in `main.go` uncaught; the eleventh and twelfth reviews found more kinds of site, which the sweep now covers. Each uncaught site got a test, and four that could not change the outcome were removed |
| Targeted mutants of the sixteenth round's structural changes (`structural16.py` in the scratchpad: the split of a chained definition, what a child walker inherits, the `program` flag of `env`, `command` and wrappers, a defined function's precedence and its return, zsh code parameters in `typeset`, `set -o NAME -A`, the `TRAP…` and may-cd conditions of hook functions, the text check of a function call's arguments) | 14 mutants (rerun after the seventeenth round, with a child walker's known functions split out), all caught; two more showed code no test could reach (a recursive split that `syntax.Walk` already does, a path check the exact-name lookup makes moot), which was removed |
| Targeted mutants of the seventeenth round's changes (`structural17.py` in the scratchpad: each place an unknown word is recorded or merged from a child walker, the `program` exemption, the git subcommands taken from a function's or unknown word's arguments and their global-option skip, the names `functions`/`autoload` touch, each word position of the statement-level glob-qualifier check and its use in changesDir, the unreadable `trap` action, the gate-file paths and the unknown-directory branch of main.go) | 18 mutants, all caught |
| Targeted mutants of the eighteenth round's changes (`structural18.py` in the scratchpad: the escape-sequence reading of the text check, echo text with a backslash, each new record of code the walker did not read (a stdin shell, xargs' or parallel's input, source, dynamic eval, a non-literal command word) and its merge from xargs' walker, the replace options, source's parse of a heredoc and its text check, the stdin refspec and ref forms for fetch and pull and their abbreviations, main.go's message) | 18 mutants, all caught |
| Targeted mutants of the nineteenth round's changes (`structural19.py` in the scratchpad: the routing of a function body's unread code to the function and its record at a call, the statement context that places a substitution's probe, a child walker's copy of those functions, AUTO_CD's skip of known commands, the hook checks that unread code needs) | 11 mutants, all caught |
| Targeted mutants of the twentieth round's changes (`structural20.py` in the scratchpad: the statement-time walk of a statement's words, its substitutions and the statements it leaves to stmt, the function body's placeholder directory and callRun's two placements, a body's pushes, hook functions, a call's and a child walker's use of the body's runs, AUTO_CD's builtin-only skip, the clean-gate-files condition for unread code, sarifgate's level `none` and numeric severity) | 16 mutants, all caught (two survived a first run and got tests: a probe repeated for nested statements, and a relative `cd` in a body) |
| Targeted mutants of the twenty-first round's changes (`structural21.py` in the scratchpad: the walk of an exit fallback and its unknown directory, callRun under CDPATH and HOME, the dedup of recorded code, a maybe-defined function's record at the call and what a child walker knows, AUTO_CD in a body and beside a maybe-defined function, changesDir at named and anonymous function bodies, the remote helpers in the walker, the text net and checkSubcommands, the refusal of unreadable input) | 17 mutants, all caught (three survived a first run and got tests) |
| Targeted mutants of the twenty-second round's changes (`structural22.py` in the scratchpad: `git config`'s stop at the first positional and its subcommands, git walked among a function's or unknown word's arguments and its data guard, the silent gate's message; and, through the shell suite, each trigger check.sh gained or widened: ci-ui for the OpenAPI document, ci-scripts for the CRDs and their types, every package for a file outside every package and the files that do not count, the image's offline pin checks) | 25 mutants (10 in Go, 15 in check.sh), all caught (two survived a first run: a read flag before two positionals got a test, and check.sh's own go.mod rule, which the rule for a file outside every package now covers, was removed) |
| Targeted mutants of the twenty-third round's changes (`structural23.py` in the scratchpad: unknownWord's guard for a maybe-defined function, find's `-ok` and `-okdir`, the verdicts unknownWord keeps (used, kept only when no recursion was cut short further up, the cut recorded and passed up, keyed by directory, cleared at a definition and by `autoload`), sarifgate's fingerprint line, and through the shell suite the reasons a tree is not clean) | 14 mutants, all caught (four survived a first run and got stronger tests: the deep chain now ends without a cd, the recursion cases go through a substitution's walker, and a third function passes the cut up) |
| Targeted mutants of the twenty-fourth round's changes (`structural24.py` in the scratchpad: the reachability that decides whether a call may cd (a callee's body, each body once, the calls a body collects, in a pipeline too, the verdicts by directory and cleared at a definition), the generations and references that make a call stale (per name and in all, through a callee, for a dynamic word, eval and source, the words of a body and those its walk meets, a certain definition replacing what was noted), the stale call's record and its directory, autoload, what a definition rereads (a definition that may not run, a hook that may cd or is stale), functions among the arguments of one that runs code given at run time or of an unknown word, a shell given a script among an unmodeled program's arguments, and through the shell suite the agents' files in any case) | 35 mutants (32 in Go, 3 in lib.sh), all caught (five survived a first run and got tests: calls at the end of a pipeline, a stale callee's callee, eval "$@" given a function, a substitution's call in a body, a replaced definition; two did not compile and were rewritten) |
| A replace ref giving `origin/main` HEAD's tree, in a scratch repository (`replace-check.sh` in the scratchpad) | without `hack/sdlc/lib.sh` the changed files are `[]`; with it, `[main.go]` |
| `PATH="$PWD/bin:$PATH" go test -race -count=1 ./internal/git/...` (git-lfs 3.7.1 in `bin/`) | passed on `c409428` (and `./internal/reposerver/...` on `6f9f985`); TestLFS passed again after its change to a git wrapper |
| TestLFS against a client without its LFS filter settings (`go test -overlay`, `lfsmut.sh` in the scratchpad) | fails through the wrapper that skips the system gitconfig; passes when git reads a system config that sets the filter, as the test did before on GitHub's runner |

## Agent review

Twenty-four runs of `.claude/workflows/sdlc-review.js` on successive trees, each verified finding fixed
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
| `ffaf3de` | 5 | 10 | 0 |
| `d2b1a08` | 4 | 11 | 0 |
| `4e5b0f1` | 5 | 6 | 0 |
| `add16a6` | 6 | 2 | 0 |
| `8a551be` | 4 | 6 | 0 |
| `355dcdb` | 6 | 2 | 1 |
| `1d031ea` | 8 | 3 | 0 |
| `ecce621` | 7 | 6 | 0 |
| `03bda33` | 4 | 5 | 0 |
| `8043d72` | 2 | 4 | 0 |
| `6e62649` | 2 | 6 | 0 |
| `a1cedf4` | 2 | 3 | 0 |

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
hardening outside this change and is filed as #230.

The thirteenth run found 5 blocking findings. Two showed that telling a logical cd from a physical
one cannot be done from the command line (a `cd ..` after `cd -P`, and shell options such as
`set -P` or zsh's `chase_dots`), so the walker no longer tries: a path with `..` is kept as written
for any cd and for `git -C`, and gatePush's stat and git resolve it as the kernel does, which refuses
a push through a link and back rather than placing it in this clone. The others: a push in a command
substitution through a shell fed a heredoc, a here-string or a pipe went unseen (the substitution's
statements are now walked with their pipelines and redirections); no test of HOME reaching a shell
the command runs; and no test where the base's gate passes a change to the gate. Of the 10 minor
findings, 8 were fixed (zsh's `emulate -c`, `repeat` and code-running glob qualifiers, `--emulate`'s
value, `refs/Remotes/` on a case-insensitive file system, a refspec split at its last colon, replace
refs in the base gate's extraction and their tests, the gate-file list's tests), and 2 are recorded
as limits in the guide: a change that weakens `check.sh` writes the record the base's gate trusts,
and local tool versions are not checked. As every round found another way for the shell to move
its directory unseen, the walker's default was flipped too: a command word that is no program on
PATH (a function or alias of the user's shell, a builtin the walker does not know) now makes the
directory unknown, so a push after it is refused instead of placed in this clone.

The fourteenth run found 4 blocking findings: the gate checked out on a branch that changed it
judged pushes of other branches without the base's gate (now a change to the gate's files in the
pushed commit or in HEAD sends the push to the base's gate, in both hooks); the new rule on unknown
command words did not apply inside function bodies, loops, a pipeline's last element or a trap (it
now does, through `changesDir`); a function defined in a subshell, a branch, a chain or eval
exempted its name everywhere (now only a definition that certainly runs in this shell counts); and
`check.sh` recorded a pass after HEAD moved away and back during the run (HEAD's reflog and the
stash's are now compared). All 11 minor findings were fixed: a logical `cd ..` through a link from
another repository into this clone (both readings are now compared and an unknown directory results
when they differ), a nil-pointer panic on zsh's anonymous functions, `break`, `continue` and other
builtins, an exit-guarded chain with two cds, `read -s` and `mapfile -t`, grafts (now ignored like
replace refs), Linux-only files no step compiled on macOS (a Linux `go vet` now runs there), CLAUDE.md
and AGENTS.md counted as documentation, and the shell suite building the hook's binary with a
`go.work`.

The fifteenth run found 5 blocking findings. One was a defect the fourteenth round's fix brought in:
the comparison of reflogs in `check.sh` exited the script whenever no stash existed (`cat` of a
missing reflog under pipefail), so no check could be recorded; it now compares the last entry of
this worktree's HEAD reflog alone (a stash resets HEAD, so it is there too). The others: a function
the command defines that cds, called after a later cd, counted as harmless (now a call of a defined
function may cd wherever its body, or a function it calls, may); a definition in a pipeline counted
as defined; and neither the reflog comparison nor git's hook's new HEAD and work-tree conditions had
a test (test-sdlc.sh now runs check.sh end to end with stub make targets, switching branches and
stashing during a run, and pushes from a work tree with an uncommitted or checked-out change to each
gate path). All 6 minor findings were fixed: a function or alias the command defines under a name
the walker models (`cd`, `exit`, `git` …) is refused, a one-letter long-option abbreviation git
accepts (`--u=`) is read, `printf -vNAME` is read, AUTO_CD ignores builtins, a background definition
has a test, and other worktrees' stashes no longer count. The mutation sweep after these fixes found
one site no test needed: a definition that may not run left the directory unknown at once, though
nothing had run yet. Such a definition, when its body may cd, now makes the function's name unknown
for later calls instead, and zsh's anonymous functions, which run at once, are walked in place
(`() { …; }` does not parse as bash and is refused).

The sixteenth run found 6 blocking findings, two from the fifteenth round's change to functions.
mvdan.cc/sh reads `f() { …; } && cd /x` as a function whose body is the whole chain, so the cd after
the definition went unseen once a definition no longer cleared the directory; the parse now splits
such a chain, and the definition is one element of it. A function named after a command the walker
dispatches by name (`echo`, `true`, a wrapper, a shell) was read as that command; a function the
command defines now runs instead (but not after `command`, `builtin`, `exec`, `env` or in a
wrapper), and a function whose body may cd may also run in a shell the command starts; mutants of
that change showed a function named `bash` hiding a push in its `-c` argument, which the code before
had refused, so a push in the text of a call of a defined function is refused. zsh's code
parameters could still be set under a key or from a command the walker does not read (`set -A
functions cd :`, `functions[$n]=:`, `alias "$n=:"`); those are refused, as are `enable`, `disable`,
`functions` and `autoload` aimed at a modeled name, `add-zsh-hook`, the hook arrays, and a function
zsh calls on its own (`chpwd`, `TRAPDEBUG`) that may cd. Checking that last case showed that a
`trap` action with a cd, which left the directory unknown only until the next cd, can move it before
any later command (`trap 'cd /x' DEBUG; cd /repo && git push`); a push after it is now refused. Two
findings were untested branches (AUTO_CD's exclusion of defined functions and builtins, the
`+function` glob qualifier), and one was the new check.sh test failing under CI's `GOFLAGS` (and a
set `PATROL_SDLC_BASE`), which make check.sh check without recording; the test now runs check.sh
without them, and the suite was run with them set. Both minor findings were fixed: the review diff
ignores attributes and textconv drivers (`--text --no-textconv`, with a test that fails without
them), and TestLFS runs git through a wrapper that skips the system gitconfig, so it fails on
GitHub's runner, whose system config sets the LFS filter, when the client does not configure the
filter itself.

The seventeenth run found 4 blocking findings. The largest: the Bash tool's zsh loads the user's
aliases, and oh-my-zsh's git plugin defines `gp='git push'` (and `-='cd -'`), so `cd OTHER && gp
origin main` pushed from another clone with no push the walker could see. A command word the walker
does not know (no program on PATH, builtin or function the command defines) is now recorded with
the directory it runs in, and main.go lets it run only in this clone or its worktrees, where git's
own hook judges what it pushes; zsh's `-` is read as such a word, not as the precommand. A git
alias handed to a function or to such a word (`retry git ship origin main`) is now checked as git's
own subcommands are. zsh runs a glob qualifier's code wherever it globs, so the check moved from a
command's arguments to the whole statement (assignments and arrays, `for` lists, redirections), and
the directory is unknown from before the command, not after it. zsh's `functions -c f ls` and
`autoload ls` define a function under a program's name; every name `enable`, `disable`,
`functions` and `autoload` touch is now unknown. All 6 minor findings were fixed:
`command -p`, `command --` and `builtin --` run the command that follows; a `trap` action the hook
cannot read refuses a later push; the function-call branch that reads expanded words has tests
(`$'push'`, `pu\sh`); a refused source is gated whatever its place beside one that does not resolve
yet; pushcheck's check of uncommitted gate files includes `.claude/settings.json`, as git's hook's
does; and CLAUDE.md and AGENTS.md name git's hook as the one that refuses. The settings edit got a
Go test rather than a test-sdlc.sh case, since gatePush is where the check lives.

The eighteenth run found 6 blocking findings, five of them more ways to run code the walker did not
read from another clone: a push on a later line of a script printf or zsh's echo piped to a shell
(the text check removed backslashes, so `\ngit` read as `ngit`; escape sequences now also read as
line breaks, and echo text with a backslash is no longer read as written); a shell reading a script
from standard input the walker did not resolve (an enclosing redirection, a pipe into `env sh`, a
process substitution); `source /dev/stdin` and `. <(…)`; `xargs sh -c` and `parallel` taking the
script from their input; and a command that does not parse as bash, which skipped the unknown-word
check. Following the seventeenth round's rule, all of this is now recorded as code the walker could
not read, with the directory it runs in, and runs only in this clone; `source /dev/stdin` of a
heredoc or here-string is parsed, and `eval` of a dynamic string and a command word that is not
literal are recorded the same way. A command that does not parse is refused. The sixth: `git fetch
--stdin` and abbreviations of `update-ref --stdin` (`--stdi`, `--std`) could move origin/main, which
made a code change pass as documentation only; every abbreviation git accepts is refused for
update-ref, symbolic-ref, fetch and pull. Both minor findings were addressed: TestMain clears the
history and repository variables check.sh exports, so the replace-ref and graft cases can fail under
`make sdlc-check` too, and the guide's Limits say that the records are plain files anything running
as the user can write.

The nineteenth run found 8 blocking findings. Three were false refusals or misplacements brought in
by the last two rounds: code the walker could not read in a function body was recorded where the
function was defined, from an unknown directory, which refused the retry helper (`retry() { …
"$@" …; }; retry go test ./...`) whether or not it was called (it is now recorded where the function
is called, from the directory of the call, and a call in a shell the command starts counts too);
`popd`, `pushd`, `eval` or `trap` after an unguarded cd were read as zsh's AUTO_CD (AUTO_CD now skips
every command the walker knows); and code in a command or process substitution was placed in the
command's last directory rather than its statement's. Two were gaps in where unread code may run: a
worktree of this clone without git's hook or gate counted as safe (gateUnknown now asks what
gatePush asks: `core.hooksPath` is `.githooks` and the hook and gate.sh are there), and a work
tree's own `core.hooksPath` (with `extensions.worktreeConfig`, as two of this clone's Claude Code
worktrees have, pointing at the main checkout's hooks) kept git's hook counted off with no way to
clear it (`make sdlc-git-hooks` now sets the work tree's value too). A nested `CLAUDE.md`,
`CLAUDE.local.md` or `AGENTS.md` counted as documentation and skipped the review (any depth is now
excluded, and the delivery reviewer owns them). Two were missing tests: GNU parallel's `{}` script,
and `source /dev/fd/0` and a heredoc for `source`. All 3 minor findings were fixed: tests for the
shortest `--stdin` abbreviations and for `\u` and `\U` escapes, and `make ci-ui` runs vitest with
`CI=true` locally too, so a focused `.only` test fails as it does on GitHub instead of passing with
the rest of its file skipped (915 UI tests passed that way).

The twentieth run found 7 blocking findings, four of them in the nineteenth round's placement of
unread code: a function's code was placed at the call's directory although a `cd` in the body ran
before it; a substitution in a function body was marked only after the calls had been walked, so its
code was never recorded; a nested substitution lost its directory; and (a minor finding) a hook
function's code was never recorded at all. Instead of patching each, the walker now handles a
statement's own words when it walks the statement, in its directory: its command and process
substitutions (whose own statements are walked the same way, so nesting needs nothing more) and the
assignments in parameter expansions and arithmetic, which also fixes the fifth finding, `CDPATH` set
by `${CDPATH:=…}` taking effect only after the walk. A function body is walked from a placeholder
directory, and each call places the body's unread code relative to the call (a relative `cd` in the
body moves it, an absolute or unknown one stays); a hook function's code is recorded from an unknown
directory. The sixth: unread code was allowed in this clone while a gate file had an uncommitted or
hidden edit, which gatePush refuses for a literal push; the check moved into the helper both use.
The seventh: a `.claude/` directory under `docs/` (a directory-scoped skill or agent) counted as
documentation; anything under a `.claude/` directory at any depth now needs a review. All 6 minor
findings were addressed: the hook functions and `CDPATH` above, AUTO_CD skips only builtins and
functions (a modeled program that is not installed can still be a directory), sarifgate's tests
cover level `none` and a numeric security-severity, and `make sdlc-git-hooks` sets the work tree's
own value only with `extensions.worktreeConfig`, since git refuses `--worktree` without it in a
clone with linked worktrees (the earlier recipe exited 128 there; a test covers it).

The twenty-first run found 4 blocking findings. One came from the twentieth round's removal of the
after-the-walk probe: the fallback of a chain ending in `|| exit` (`cd X || { git push origin main;
exit 1; }`) was skipped, not walked, so a push, an unknown word, a git alias or a write to
origin/main in it went unseen; it is now walked from an unknown directory. CDPATH (or HOME) set
between a function's definition and its call could take a `cd` in the body elsewhere; a call made
after either is set now places the body's moved code in an unknown directory. git's remote helpers
(`git remote-https`, `git-remote-*`) run send-pack themselves, without the pre-push hook; they are
refused like `git send-pack`, in the walker, among a function's arguments and in the text net
(`remote-http(s)`, `remote-ftp(s)`, `remote-ext`, `remote-fd`, `git-remote-`). And `MAKE` from the
environment overrides make's own, so `MAKE=true make sdlc-check` emptied every `$(MAKE)` recipe
(ci-generated) and still recorded a pass; check.sh drops it, and the shell suite runs check.sh with
`MAKE=false` (without the fix, three of its checks fail). All 5 minor findings were fixed: a
function's code is recorded once per place (a chain of helpers calling helpers grew the list
exponentially, each entry costing several git runs); a function that may cd, called in a
substitution or defined in a branch, is recorded where it is called rather than from an unknown
directory (and a definition no longer counts as a cd for the statement holding it, `changesDir`
stopping at a named function's body); AUTO_CD inside a function body is placed relative to the call;
and hook input that does not decode is refused instead of let through. The review of the tree that
contains this record runs after it is written; its result is in the pull request.

The twenty-second run found 2 blocking findings, both in check.sh's choice of checks rather than in
the hook. A change to `api/openapi/openapi.json` alone skipped `make ci-ui`, whose `gen:api:check`
compares the UI's generated API types with it. And a change outside every Go package (an example
under `examples/`, a CRD) ran no Go test, although `test/examples`, `internal/cli/admin` and
`internal/apiserver` read those files, and no `ci-scripts`, whose `validate.sh` checks the e2e
fixtures against the CRDs. ci-ui now runs for `api/openapi/`, ci-scripts for `deploy/crds/` and
`api/v1alpha1/`, and a changed file outside every package (but documentation, the UI's sources and
the agents' instructions, which no Go test reads) tests every package, as `go.mod` already did and as
CI's unit job always does; the shell suite covers each choice with stub targets in a scratch module.
All 4 minor findings were fixed: `git config KEY VALUE --get` (or `-l`, `--name-only`) writes KEY,
since git reads no option after the first positional unless a subcommand comes first, and the walker
now reads it so; git handed to a function the command defines or to an unknown word is walked as git,
so a configuration write, a write under `refs/remotes/` or a remote helper among the arguments is
refused, not only a push or an alias; a gate that refuses without output has a test; and check.sh
runs the image's offline pin checks (`image-verify-pins --offline`: the Dockerfile's Go and pnpm
against `go.mod` and `ui/package.json`) when one of those files or `hack/images/` changes.

The twenty-third run found 2 blocking findings. One was a missing test: unknownWord's guard for a
function that may be defined, named after a program and called from another function's body, had
none (the case written for it called the name at top level, which the call already covers); two
cases now fail without it. The other: `core.hooksPath` is the relative `.githooks`, which git
resolves against the directory it runs hooks from, so git's hook does not run for a push given this
clone's git directory from outside the work tree (`--git-dir` or `GIT_DIR` from another directory, a
push from inside `.git`). The Claude Code hook refuses those forms on a command line, but a script or
another tool that pushes so is not covered, as the guide claimed. An absolute path needs
`extensions.worktreeConfig` for linked worktrees and stops working, silently for git, when the clone
moves, so the guide's claim is narrowed and its Limits list the gap; #231 tracks a fix. All 6 minor
findings were addressed: find's `-ok` and `-okdir` are walked as `-exec` and `-execdir`; unknownWord
keeps its verdict on each function (by directory, CDPATH and HOME; cleared when a definition changes
what a name runs; not kept when recursion cut the walk short at a function further up), so 40
functions each calling the next twice are followed in milliseconds rather than in time exponential in
their number, past the hook's timeout (18 took 55 s), and a child walker asks about the functions in a
fixed order; sarifgate's report test rules out a fingerprint for a result that does not fail; a dirty
tree's message lists what makes it dirty (an ignored file the checks read, such as a UI build in
`internal/ui/dist`, does not show in `git status`); the static preflight checks for jq, which
ci-static's SDLC tests need; and the Limits name GitHub's API, which writes commits without a push.

The twenty-fourth run found 2 blocking findings, both in how the walker follows the functions a
command defines. The verdicts the twenty-third round kept were dropped whenever a call back to an
outer function cut the walk short, so call graphs with such back edges still took exponential time
(18 functions, 72 s; a few more pass the hook's timeout, which lets the command run). And bash and
zsh look a function up when it is called, while the walker read a function's callees as they were
when it walked its body: a callee defined again later (`g() { true; }; f() { g; }; g() { gp; }; cd
OTHER && f`) ran code it never saw, in another clone. Whether a call may cd is now reachability over
the call graph as the functions are defined at the call, each body's own verdict kept per directory
(40 functions with back edges in 0.2 s, 200 in under 5 s). Each definition counts up a generation per
name, each function notes the command words its body runs with their generation then, and a call of
one whose body, or a callee's, runs a name defined since runs as a word the walker does not know:
from the call, or from an unknown directory if it may cd. A certain definition replaces what was
noted for the earlier one; definitions that may not run, and the functions the shell calls on its
own, are read again after each definition. A function named among the arguments of one that runs
code given at run time (`"$@"`, `eval`), or of a word the walker does not know, runs there too. All 3
minor findings were fixed: the tests write find's terminator as find receives it (a bare `;` ends the
shell command); a shell given a script among the arguments of a program the walker does not model
(`arch -arm64 sh -c '…'`) is walked; and the agents' instruction files count in any case, as a
case-insensitive file system reads them.

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
