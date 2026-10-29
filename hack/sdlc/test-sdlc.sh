#!/usr/bin/env bash
# Regression tests of the SDLC push gate (docs/guides/claude-code-sdlc.md), end to end in scratch
# repositories with a bare remote: the Claude Code hook (hack/sdlc/claude-pre-push.sh with
# hack/sdlc/pushcheck), git's pre-push hook (.githooks/pre-push), the gate's decisions on check and
# review records (hack/sdlc/gate.sh), the safeguards of hack/sdlc/record-review.sh and
# hack/sdlc/review-input.sh, and the agreement of the documentation rule with
# hack/ci/classify-changes.sh. Needs git, go and jq; runs in a few seconds once bin/sdlc-pushcheck is
# built. Run by `make ci-sdlc`.
#
#   hack/sdlc/test-sdlc.sh
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
src=$(cd "$here/../.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
# git's defaults for what the gate turns off itself (check.sh, which runs this suite, exports them),
# so its replace-ref and graft tests can fail; and no function the caller exports (Fedora's which,
# Lmod's module), which sdlc_weakened counts and every bash this suite starts would import.
unset GIT_NO_REPLACE_OBJECTS GIT_GRAFT_FILE
# shellcheck disable=SC2163 # un-exports the function named by $f
while read -r _ _ f; do export -nf "$f"; done < <(declare -Fx)
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.invalid GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.invalid

passed=0
failed=0
check() { # check NAME WANT GOT
	if [[ $2 == "$3" ]]; then
		passed=$((passed + 1))
	else
		failed=$((failed + 1))
		echo "FAIL $1: want '$2', got '$3'"
	fi
}
contains() { # contains NAME FILE TEXT: FILE contains TEXT
	if grep -qF -- "$3" "$2"; then
		check "$1" yes yes
	else
		check "$1" "'$3' in $2" "$(head -c 400 "$2")"
	fi
}

# The documentation rule must be classify-changes.sh's docs_re.
ci_re=$(sed -n "s/^docs_re='\(.*\)'\$/\1/p" "$src/hack/ci/classify-changes.sh")
# shellcheck disable=SC2016 # the pattern matches the literal text of lib.sh
sdlc_re=$(sed -n 's/.*\[\[ -z \$f || \$f =~ \(.*\) \]\] || return 1/\1/p' "$src/hack/sdlc/lib.sh")
check "docs rule agrees with classify-changes.sh" "$ci_re" "$sdlc_re"
# ... but for docs/parity/, whose status files claim features complete: they need a review.
docs_only() { # docs_only FILE...: 0 when the gate counts the files as documentation only
	printf '%s\n' "$@" | (source "$src/hack/sdlc/lib.sh" && sdlc_docs_only) && echo 0 || echo 1
}
check "a guide is documentation" 0 "$(docs_only docs/guides/x.md README.md)"
check "a parity status file is not documentation" 1 "$(docs_only docs/parity/status/x.yaml)"
check "the parity matrix is not documentation" 1 "$(docs_only docs/guides/x.md docs/parity/matrix.md)"
check "CLAUDE.md is not documentation" 1 "$(docs_only CLAUDE.md)"
check "AGENTS.md is not documentation" 1 "$(docs_only README.md AGENTS.md)"
for f in docs/CLAUDE.md docs/guides/CLAUDE.md CLAUDE.local.md docs/AGENTS.md docs/.claude/skills/x/SKILL.md \
	docs/guides/.claude/agents/x.md claude.local.md docs/guides/claude.md docs/guides/Agents.md \
	docs/.Claude/skills/x/SKILL.md docs/Parity/matrix.md; do
	check "$f is not documentation" 1 "$(docs_only docs/guides/x.md "$f")"
done
check "code is not documentation" 1 "$(docs_only main.go)"

# CI's CodeQL bundle must be the one hack/sdlc/codeql.sh pins and checks: the workflow fetches it
# with codeql.sh and hands that file to the action.
# shellcheck disable=SC2016 # the pattern matches the literal text of the workflow
check "CodeQL bundle of CI is fetched by codeql.sh" 1 \
	"$(grep -cF 'run: hack/sdlc/codeql.sh fetch "$RUNNER_TEMP/codeql"' "$src/.github/workflows/codeql.yml")"
check "CodeQL bundle of CI is codeql.sh's Linux asset" \
	"$(sed -n "s/^[[:space:]]*asset='\(codeql-bundle-linux64[^']*\)'/\1/p" "$src/hack/sdlc/codeql.sh")" \
	"$(sed -n 's|^[[:space:]]*tools: \${{ runner.temp }}/codeql/\(.*\)$|\1|p' "$src/.github/workflows/codeql.yml")"

# The hook command in .claude/settings.json blocks when the wrapper cannot run or dies (Claude Code
# blocks only on status 2), and lets commands run in a checkout without it (from before the gate).
hookcmd=$(jq -r '.hooks.PreToolUse[0].hooks[0].command' "$src/.claude/settings.json")
settings_hook() { # settings_hook PROJECT_DIR: exit status of the settings' hook command
	local rc=0
	echo '{}' | CLAUDE_PROJECT_DIR=$1 sh -c "$hookcmd" >/dev/null 2>&1 || rc=$?
	echo "$rc"
}
mkdir -p "$work/p/hack/sdlc"
check "settings hook: no wrapper in the checkout" 0 "$(settings_hook "$work/p")"
printf 'exit 0\n' >"$work/p/hack/sdlc/claude-pre-push.sh"
check "settings hook: the wrapper passes, executable or not" 0 "$(settings_hook "$work/p")"
printf 'kill -9 $$\n' >"$work/p/hack/sdlc/claude-pre-push.sh"
check "settings hook: the wrapper is killed" 2 "$(settings_hook "$work/p")"
printf 'exit 1\n' >"$work/p/hack/sdlc/claude-pre-push.sh"
check "settings hook: the wrapper fails" 2 "$(settings_hook "$work/p")"
# The format hook after an edit runs the formatter where the checkout has it, and does nothing (no
# hook error on every edit) where it does not.
fmtcmd=$(jq -r '.hooks.PostToolUse[0].hooks[0].command' "$src/.claude/settings.json")
format_hook() { # format_hook PROJECT_DIR: exit status of the settings' format hook command
	local rc=0
	echo '{}' | CLAUDE_PROJECT_DIR=$1 sh -c "$fmtcmd" >/dev/null 2>&1 || rc=$?
	echo "$rc"
}
check "format hook: no formatter in the checkout" 0 "$(format_hook "$work/p")"
printf 'echo formatted >"%s/formatted"\n' "$work" >"$work/p/hack/sdlc/claude-format.sh"
check "format hook: runs the formatter, executable or not" 0 "$(format_hook "$work/p")"
check "format hook: the formatter ran" formatted "$(cat "$work/formatted" 2>/dev/null)"

# The Go checks build against the tree's go.mod, not a go.work in a parent directory or GOWORK.
check "check.sh turns go.work off" 1 "$(grep -c '^export GOWORK=off' "$src/hack/sdlc/check.sh")"
check "codeql.sh turns go.work off" 1 "$(grep -c '^export GOWORK=off' "$src/hack/sdlc/codeql.sh")"
check "the wrapper builds pushcheck with go.work off" 1 "$(grep -c 'GOWORK=off go build' "$src/hack/sdlc/claude-pre-push.sh")"

# The shell lint reads no shellcheck configuration from outside the tree: an rc file in HOME that
# disables every check, and SHELLCHECK_OPTS, leave a finding a finding. (The scratch repository has
# no POSIX sh script, which bash 3.2's set -u must also handle.)
if command -v shellcheck >/dev/null 2>&1; then
	lint=$work/lint
	git init -q "$lint"
	mkdir -p "$lint/hack/ci" "$work/lint-home"
	cp "$src/hack/ci/lint-shell.sh" "$lint/hack/ci/"
	# shellcheck disable=SC2016 # the script text holds an unquoted $1 on purpose
	printf '#!/usr/bin/env bash\necho $1\n' >"$lint/bad.sh"
	git -C "$lint" add -A
	echo 'disable=all' >"$work/lint-home/.shellcheckrc"
	rc=0
	(cd "$lint" && HOME=$work/lint-home SHELLCHECK_OPTS='--exclude=SC2086' hack/ci/lint-shell.sh) >"$work/lint.out" 2>&1 || rc=$?
	check "shell lint ignores a HOME rc file and SHELLCHECK_OPTS" 1 "$rc"
	contains "shell lint reports the finding" "$work/lint.out" "SC2086"
else
	echo "test-sdlc: shellcheck not installed; skipping the shell lint configuration check"
fi

# Build the hook's binary (the wrapper builds it on first use).
(cd "$src" && GOWORK=off go build -o bin/sdlc-pushcheck ./hack/sdlc/pushcheck)

# A clone with the gate scripts and the git hook on main, a bare remote, and a change branch.
remote=$work/remote.git
git init -q --bare -b main "$remote"
repo=$work/repo
git init -q -b main "$repo"
mkdir -p "$repo/hack/sdlc" "$repo/hack/ci" "$repo/docs" "$repo/.githooks"
cp "$src"/hack/sdlc/{lib.sh,gate.sh,record-review.sh,review-input.sh,claude-pre-push.sh} "$repo/hack/sdlc/"
cp "$src/hack/ci/classify-changes.sh" "$repo/hack/ci/"
cp "$src/.githooks/pre-push" "$repo/.githooks/"
cd "$repo"
git add -A
git commit -q -m base
git remote add origin "$remote"
git push -q origin main
# The hook script runs this binary (it would build it from hack/sdlc/pushcheck, which this clone lacks).
mkdir bin
cp "$src/bin/sdlc-pushcheck" bin/
echo bin/ >>.git/info/exclude
git checkout -q -b change
echo code >main.go
git add main.go
git commit -q -m "feat: code"
state=$(git rev-parse --path-format=absolute --git-common-dir)/patrol-sdlc
tree=$(git rev-parse 'HEAD^{tree}')

hook() { # hook COMMAND [CWD]: prints the exit status of the hook binary for this repository
	local rc=0
	jq -n --arg c "$1" --arg d "${2:-$repo}" '{tool_name: "Bash", tool_input: {command: $c}, cwd: $d}' |
		"$src/bin/sdlc-pushcheck" "$repo" >/dev/null 2>"$work/hook.err" || rc=$?
	echo "$rc"
}
wrapper() { # wrapper COMMAND: exit status of the Claude Code hook script itself
	local rc=0
	jq -n --arg c "$1" --arg d "$repo" '{tool_name: "Bash", tool_input: {command: $c}, cwd: $d}' |
		"$repo/hack/sdlc/claude-pre-push.sh" >/dev/null 2>"$work/hook.err" || rc=$?
	echo "$rc"
}
preview() { # preview NAME: exit status of the hook script for the browser pane's preview_start
	local rc=0
	jq -n --arg n "$1" --arg d "$repo" '{tool_name: "mcp__Claude_Browser__preview_start", tool_input: {name: $n}, cwd: $d}' |
		"$repo/hack/sdlc/claude-pre-push.sh" >/dev/null 2>"$work/hook.err" || rc=$?
	echo "$rc"
}
gate() {
	local rc=0
	hack/sdlc/gate.sh >/dev/null 2>"$work/gate.err" || rc=$?
	echo "$rc"
}
gitpush() { # gitpush ARGS...: a real push through git's pre-push hook; prints its exit status
	local rc=0
	git push -q "$@" >/dev/null 2>"$work/push.err" || rc=$?
	echo "$rc"
}
remote_has() { # remote_has BRANCH: the commit the remote has for BRANCH, or "none"
	git --git-dir="$remote" rev-parse --verify --quiet "refs/heads/$1" || echo none
}

# Commands that are not pushes, or not gated.
check "not a push" 0 "$(hook 'git status')"
check "push in a word" 0 "$(hook 'echo pushing')"
check "push in quoted text and a heredoc" 0 "$(hook "$(printf 'git commit -m "docs: git push"\ncat <<EOF\ngit push\nEOF')")"
check "deletion" 0 "$(hook 'git push origin --delete old')"
check "dry run" 0 "$(hook 'git push --dry-run origin change')"
mkdir "$work/other"
git init -q "$work/other"
# From Claude Code a push runs only in this clone and its worktrees: another repository may not run
# git's hook.
check "push from another repository" 2 "$(hook "git -C $work/other push")"
contains "reason: not a work tree of this clone" "$work/hook.err" "which is not a work tree of $repo"

# Without core.hooksPath the hook script refuses every command but the one that enables git's hook,
# and pushcheck every push from this repository, dry runs and deletions included.
check "git hook not enabled: any command" 2 "$(wrapper 'ls')"
contains "reason: make sdlc-git-hooks" "$work/hook.err" "make sdlc-git-hooks"
check "git hook not enabled: make sdlc-git-hooks runs" 0 "$(wrapper 'make sdlc-git-hooks')"
check "git hook not enabled: only make sdlc-git-hooks itself" 2 "$(wrapper 'make sdlc-git-hooks; git push origin change')"
# The target sets the work tree's own value too: with extensions.worktreeConfig, a value in
# config.worktree wins over the clone's, and the hooks would stay off whatever it set there.
git init -q "$work/wtc"
git -C "$work/wtc" config extensions.worktreeConfig true
git -C "$work/wtc" config --worktree core.hooksPath /elsewhere
make -s -C "$work/wtc" -f "$src/hack/sdlc.mk" sdlc-git-hooks
check "make sdlc-git-hooks overrides a work tree's own value" .githooks "$(git -C "$work/wtc" config --get core.hooksPath)"
# Without the extension, in a clone with a linked worktree, where git refuses --worktree.
git init -q "$work/wtl"
git -C "$work/wtl" commit -q --allow-empty -m base
git -C "$work/wtl" worktree add -q "$work/wtl-linked"
check "make sdlc-git-hooks with a linked worktree and no extension" 0 "$(make -s -C "$work/wtl" -f "$src/hack/sdlc.mk" sdlc-git-hooks >/dev/null 2>&1 && echo 0 || echo 1)"
check "  and git's hook is enabled there" .githooks "$(git -C "$work/wtl-linked" config --get core.hooksPath)"
check "git hook not enabled: push" 2 "$(hook 'git push -u origin change')"
contains "reason: make sdlc-git-hooks" "$work/hook.err" "make sdlc-git-hooks"
check "git hook not enabled: negated dry run" 2 "$(hook 'git push -n --no-dry-run origin change')"
check "git hook not enabled: negated delete" 2 "$(hook 'git push -d --no-delete origin change')"
check "git hook not enabled: heredoc to bash" 2 "$(hook "$(printf "bash <<'EOF'\ngit push origin change\nEOF")")"
# Other clones of this project, with or without a work tree, are other repositories too.
git clone -q "$remote" "$work/clone2"
check "second clone" 2 "$(hook 'git push origin HEAD:other' "$work/clone2")"
contains "reason: a second clone" "$work/hook.err" "which is not a work tree of $repo"
git clone -q --bare "$remote" "$work/bare.git"
check "bare clone" 2 "$(hook 'git push origin main:other' "$work/bare.git")"
contains "reason: a bare clone" "$work/hook.err" "which is not a work tree of $repo"
git clone -q --no-checkout "$remote" "$work/nocheckout"
check "clone without a checkout" 2 "$(hook 'git push origin main:other' "$work/nocheckout")"
contains "reason: a clone without a checkout" "$work/hook.err" "which is not a work tree of $repo"
# Without a binary and with a build that fails (this clone has no pushcheck sources; a fresh clone
# offline or without Go on the hook's PATH), the command that enables git's hook still runs.
mv bin/sdlc-pushcheck "$work/pushcheck.saved"
check "no binary, git hook not enabled: make sdlc-git-hooks runs" 0 "$(wrapper 'make sdlc-git-hooks')"
check "no binary, git hook not enabled: any other command" 2 "$(wrapper 'ls')"
git config core.hooksPath .githooks
check "no binary, a failed build: a command without git runs" 0 "$(wrapper 'ls')"
check "no binary, a failed build: a command with git is refused" 2 "$(wrapper 'git status')"
contains "reason: the build failed" "$work/hook.err" "cannot build hack/sdlc/pushcheck"
mv "$work/pushcheck.saved" bin/sdlc-pushcheck
check "git hook enabled: any command" 0 "$(wrapper 'ls')"
# A pushcheck binary that dies refuses the command instead of letting it run unchecked.
cp bin/sdlc-pushcheck "$work/pushcheck.good"
printf '#!/bin/sh\nkill -9 $$\n' >bin/sdlc-pushcheck
check "a killed pushcheck refuses" 2 "$(wrapper 'ls')"
contains "reason: a killed pushcheck" "$work/hook.err" "stopped with status 137"
cp "$work/pushcheck.good" bin/sdlc-pushcheck
check "git hook enabled: the wrapper runs pushcheck on every command" 2 "$(wrapper "git p''ush --no-verify origin change")"
contains "reason: the text net" "$work/hook.err" "in a command line could keep git's pre-push hook from running"
# preview_start runs the command of a .claude/launch.json configuration: the wrapper checks it.
mkdir -p .claude
cat >.claude/launch.json <<'EOF'
{"version": "0.0.1", "configurations": [
  {"name": "push", "runtimeExecutable": "git", "runtimeArgs": ["-c", "core.hooksPath=/dev/null", "push", "origin", "HEAD:x"], "port": 1},
  {"name": "dev", "runtimeExecutable": "pnpm", "runtimeArgs": ["dev"], "port": 5173},
  {"name": "attach", "url": "http://localhost:5173"}
]}
EOF
check "preview_start of a push with git's hook off" 2 "$(preview push)"
contains "reason: core.hooksPath" "$work/hook.err" "core.hooksPath"
check "preview_start of a dev server" 0 "$(preview dev)"
check "preview_start attaching to a server" 0 "$(preview attach)"
check "preview_start of an unknown configuration" 2 "$(preview nosuch)"
rm -r .claude

# Forms the hook refuses whatever the records.
check "--tags" 2 "$(hook 'git push --tags origin')"
check "matching refspec" 2 "$(hook 'git push origin :')"
check "--no-verify" 2 "$(hook 'git push --no-verify origin change')"
check "push behind a wrapper" 2 "$(hook 'timeout 60 git push origin change')"
# shellcheck disable=SC2016 # the command texts hold unexpanded variables on purpose
check "push in a substitution" 2 "$(hook 'out=$(git push origin change)')"
# shellcheck disable=SC2016
check "unknown directory" 2 "$(hook 'cd "$TMPDIR" && git push')"
check "directory that does not exist" 2 "$(hook "cd $work/nowhere && git push")"
# A ref that does not resolve yet (a branch or tag created earlier in the same command) is left to
# git's own pre-push hook, which sees what it ends up pointing at.
check "unresolved source defers to git's hook" 0 "$(hook 'git push origin no-such-branch')"
check "create a branch then push it" 0 "$(hook 'git switch -c fix/new && git push -u origin fix/new')"
check "abbreviated --no-verify" 2 "$(hook 'git push --no-veri origin change')"
check "alias defined with -c" 2 "$(hook "git -c alias.ship='push --no-verify' ship origin change")"
check "configuration included with -c" 2 "$(hook 'git -c include.path=/x.cfg push origin change')"
git config alias.ship 'push --no-verify'
check "configured alias for a push" 2 "$(hook 'git ship origin change')"
contains "reason: alias" "$work/hook.err" "git ship: not a git command but an alias"
git config --unset alias.ship
# An alias the command itself writes into git's configuration on the way.
check "alias written by the command" 2 "$(hook "printf '[alias]\\n\\tship = push --no-verify\\n' >>.git/config && git ship origin change")"
# shellcheck disable=SC2016 # the command text holds an unexpanded variable on purpose
check "alias from another HOME" 2 "$(hook 'HOME=$PWD/h git ship origin change')"
# A shell alias or function (the Bash tool loads the user's, such as gp='git push') handed --no-verify.
check "--no-verify to a shell alias" 2 "$(hook 'gp --no-verify origin change')"
# A zsh function or alias the command defines for itself.
check "zsh function defined through its parameter" 2 "$(hook "functions[gp]='git push --no-verify'; gp origin change")"
check "alias defined and run through eval" 2 "$(hook "alias gp='git push --no-verify'; eval 'gp origin change'")"

# Forms that keep git's hook from running (the relative core.hooksPath has no hook outside the work
# tree) are refused wherever they run.
check "push from inside .git" 2 "$(hook 'git push origin change' "$repo/.git")"
check "git -C .git push" 2 "$(hook 'git -C .git push origin change')"
check "--git-dir from elsewhere" 2 "$(hook "git --git-dir=$repo/.git push origin change" "$work")"
check "exported GIT_DIR" 2 "$(hook "export GIT_DIR=$repo/.git; cd $work && git push origin change")"
# --no-verify is refused wherever the hook thinks the push runs: a cd that fails leaves the shell here.
check "--no-verify after a cd that may fail" 2 "$(hook "cd $work/other; git push --no-verify origin change")"
check "--no-verify from another repository" 2 "$(hook "git -C $work/other push --no-verify")"
check "git hook: push from inside .git skips it (why the above are refused)" 0 "$(git -C .git push -q origin change:from-git-dir 2>/dev/null && echo 0)"
git push -q origin :from-git-dir

# No records: the hook and git's hook both refuse.
check "bare push" 2 "$(hook 'git push')"
contains "reason names the tree" "$work/hook.err" "no passing deterministic checks for tree $tree"
check "git -C DIR push" 2 "$(hook "git -C $repo push origin HEAD" "$work")"
check "cd DIR && git push" 2 "$(hook "cd $repo && git push origin change" "$work")"
check "cd DIR; git push: the cd may fail" 2 "$(hook "cd $repo; git push origin change" "$work")"
contains "reason: a cd that may fail" "$work/hook.err" "a cd that may fail without stopping the command"
check "git hook refuses the push" 1 "$(gitpush origin change)"
check "remote did not get it" none "$(remote_has change)"
check "help is not a push" 0 "$(hook 'git push -h')"

# A worktree of this clone on a commit from before the gate: git's hook is not there to run.
old=$(git commit-tree "$(git hash-object -t tree /dev/null)" -m "chore: before the gate")
git worktree add -q -b old "$work/old" "$old"
check "push from a branch without the gate" 2 "$(hook 'git push origin old' "$work/old")"
contains "reason: check out a branch based on main" "$work/hook.err" "check out a branch based on main"

# A worktree of the clone shares its records and core.hooksPath.
git worktree add -q -b wt "$work/wt"
check "push from a worktree" 2 "$(hook 'git push origin wt' "$work/wt")"
contains "reason names the worktree's tree" "$work/hook.err" "no passing deterministic checks for tree $tree"
check "--no-verify from a worktree" 2 "$(hook 'git push --no-verify origin wt' "$work/wt")"
rc=0
git -C "$work/wt" push -q origin wt >/dev/null 2>&1 || rc=$?
check "git hook refuses the worktree's push" 1 "$rc"
check "remote did not get the worktree's branch" none "$(remote_has wt)"

# Checks recorded, review missing, then failing, then passing.
mkdir -p "$state/checks" "$state/reviews"
printf 'tree=%s\nresult=pass\n' "$tree" >"$state/checks/$tree"
check "review missing" 1 "$(gate)"
contains "reason says review missing" "$work/gate.err" "agent review for tree $tree: missing"
# The gate measures from origin/main whatever PATROL_SDLC_BASE says, so a self-referencing base
# cannot make the diff empty and skip the review.
check "gate ignores PATROL_SDLC_BASE" 1 "$(PATROL_SDLC_BASE=$(git rev-parse HEAD) gate)"
# Nor can a local branch or tag named origin/main stand in for the remote-tracking ref.
git branch origin/main HEAD 2>/dev/null
git tag origin/main HEAD 2>/dev/null
check "gate ignores a local branch and tag named origin/main" 1 "$(gate)"
contains "reason says review missing, not documentation only" "$work/gate.err" "agent review for tree $tree: missing"
git branch -q -D origin/main
git tag -d origin/main >/dev/null
check "hook refuses moving origin/main" 2 "$(hook 'git update-ref refs/remotes/origin/main HEAD')"
echo "# review" >"$work/report.md"
# record-review refuses a tree with no prepared review input (the diff review-input.sh writes).
rc=0
hack/sdlc/record-review.sh --tree "$tree" --blocking 0 --report "$work/report.md" >/dev/null 2>&1 || rc=$?
check "record without a review input is refused" 1 "$rc"
touch "$state/reviews/$tree.diff" # review-input.sh writes this in the real flow
rc=0
hack/sdlc/record-review.sh --tree "$tree" --blocking 2 --report "$work/report.md" >/dev/null || rc=$?
check "record a failing review" 0 "$rc"
check "blocking findings stop the push" 1 "$(gate)"
contains "reason counts the findings" "$work/gate.err" "blocking findings (2)"
hack/sdlc/record-review.sh --tree "$tree" --blocking 0 --report "$work/report.md" >/dev/null
check "passing review" 0 "$(gate)"
check "hook lets the push through" 0 "$(hook 'git push -u origin change')"
check "hook: cd DIR && git push" 0 "$(hook "cd $repo && git push" "$work")"
check "git hook lets the push through" 0 "$(gitpush -u origin change)"
check "remote got the commit" "$(git rev-parse HEAD)" "$(remote_has change)"

# Several refs in one push: each pushed commit is gated, whatever its place among them.
git switch -q -c unrec
echo unrecorded >>main.go
git commit -qam "fix: unrecorded"
git switch -q change
check "hook gates every pushed ref" 2 "$(hook 'git push origin change unrec')"
contains "reason names the unrecorded commit" "$work/hook.err" "push of $(git rev-parse --short unrec) refused"
check "hook gates every pushed ref, in any order" 2 "$(hook 'git push origin unrec change')"
check "git hook gates every pushed ref" 1 "$(gitpush origin change unrec)"
check "remote did not get the unrecorded ref" none "$(remote_has unrec)"

# Commit and push in one command: the Claude Code hook sees the recorded commit, but git's hook sees
# the new one and refuses it.
echo more >>main.go
check "hook runs before the commit exists" 0 "$(hook 'git commit -qam "fix: more" && git push origin change')"
git commit -qam "fix: more"
check "git hook refuses the new commit" 1 "$(gitpush origin change)"
check "remote kept the checked commit" "$(git rev-parse HEAD~1)" "$(remote_has change)"

# git's hook fails closed when gate.sh is missing or not executable, rather than letting the push by.
chmod -x hack/sdlc/gate.sh
check "git hook fails closed without an executable gate.sh" 1 "$(gitpush origin change)"
contains "reason: restore gate.sh" "$work/push.err" "hack/sdlc/gate.sh is missing or not executable"
chmod +x hack/sdlc/gate.sh
rc=0
hack/sdlc/record-review.sh --tree "$tree" --blocking 0 --report "$work/report.md" >/dev/null 2>&1 || rc=$?
check "review of an old tree is refused" 1 "$rc"
newtree=$(git rev-parse 'HEAD^{tree}')
echo dirty >>main.go
rc=0
hack/sdlc/record-review.sh --tree "$newtree" --blocking 0 --report "$work/report.md" >/dev/null 2>&1 || rc=$?
check "review on a dirty tree is refused" 1 "$rc"
git checkout -q main.go

# Documentation only: checks needed, review not.
git checkout -q -b docs main
echo text >docs/guide.md
git add docs/guide.md
git commit -q -m "docs: guide"
dtree=$(git rev-parse 'HEAD^{tree}')
check "docs without checks" 1 "$(gate)"
printf 'tree=%s\nresult=pass\n' "$dtree" >"$state/checks/$dtree"
check "docs with checks, no review" 0 "$(gate)"

# A change that breaks the gate is judged by its base's gate too, which asks for the records of the
# change's tree: both hooks refuse it, though the changed gate passes everything.
git checkout -q -b weak main
printf '#!/usr/bin/env bash\nexit 0\n' >hack/sdlc/gate.sh
git commit -qam "fix(sdlc): a gate that passes everything"
check "a broken gate: pushcheck asks the base's gate" 2 "$(hook 'git push origin weak')"
contains "reason: the base's gate" "$work/hook.err" "so the gate of its base"
check "a broken gate: git's hook asks the base's gate" 1 "$(gitpush origin weak)"
contains "reason: git's hook names the base's gate" "$work/push.err" "judged it too"
check "remote did not get the broken gate" none "$(remote_has weak)"
# A replace ref that gives the broken commit its base's files hides nothing from git's hook.
git replace weak "$(git commit-tree "$(git rev-parse 'main^{tree}')" -p main -m fake)"
check "a broken gate behind a replace ref: git's hook" 1 "$(gitpush origin weak)"
contains "reason: git's hook still names the base's gate" "$work/push.err" "judged it too"
git replace -d weak >/dev/null
# From a work tree whose branch broke the gate, a push of another branch, which changes no gate file
# itself, is judged by the base's gate too: the gate that runs is the work tree's.
git checkout -q -B plainb main
echo plain >plain.txt
git add plain.txt
git commit -qm "chore: plain"
git checkout -q weak
check "a broken gate checked out: pushcheck asks the base's gate for another branch" 2 "$(hook 'git push origin plainb')"
contains "reason: pushcheck names the checked-out gate" "$work/hook.err" "or the gate checked out to judge it"
check "a broken gate checked out: git's hook asks the base's gate for another branch" 1 "$(gitpush origin plainb)"
contains "reason: git's hook names the checked-out gate" "$work/push.err" "or the gate checked out to judge it"
check "remote did not get the other branch" none "$(remote_has plainb)"
# The same when the gate's change is not committed: git's hook runs the work tree's gate.
git checkout -q main
printf '#!/usr/bin/env bash\nexit 0\n' >hack/sdlc/gate.sh
check "an uncommitted broken gate: git's hook asks the base's gate" 1 "$(gitpush origin plainb)"
contains "reason: git's hook names the base's gate for an uncommitted change" "$work/push.err" "judged it too"
git checkout -q -- hack/sdlc/gate.sh
# An edit of the gate that git status is told not to show would run unseen: git's hook refuses it.
for mark in assume-unchanged skip-worktree; do
	git update-index --"$mark" hack/sdlc/gate.sh
	printf '#!/usr/bin/env bash\nexit 0\n' >hack/sdlc/gate.sh
	check "a gate edit hidden by $mark: git's hook refuses" 1 "$(gitpush origin plainb)"
	contains "reason: a hidden gate edit ($mark)" "$work/push.err" "is marked assume-unchanged or skip-worktree"
	git update-index --no-"$mark" hack/sdlc/gate.sh
	git checkout -q -- hack/sdlc/gate.sh
done
# And when the checked-out branch changes only the settings, or only git's hook.
for path in .claude/settings.json .githooks/pre-push; do
	git checkout -q -B "only-$(basename "$path")" main
	mkdir -p "$(dirname "$path")"
	echo '# edited' >>"$path"
	git add "$path"
	git commit -qm "chore: edit $path"
	check "a checked-out edit of $path: pushcheck asks the base's gate for another branch" 2 "$(hook 'git push origin plainb')"
	contains "reason: pushcheck names the checked-out $path" "$work/hook.err" "or the gate checked out to judge it"
	check "a checked-out edit of $path: git's hook asks the base's gate for another branch" 1 "$(gitpush origin plainb)"
	contains "reason: git's hook names the checked-out $path" "$work/push.err" "judged it too"
done
git checkout -q weak
# Each of the gate's own paths sends a change to the base's gate, in both hooks, which list the same.
for path in .githooks/pre-push .claude/settings.json; do
	git checkout -q -B edit main
	mkdir -p "$(dirname "$path")"
	echo '# edited' >>"$path"
	git add "$path"
	git commit -qm "chore: edit $path"
	check "an edit of $path: pushcheck asks the base's gate" 2 "$(hook 'git push origin edit')"
	contains "reason: pushcheck names the base's gate for $path" "$work/hook.err" "so the gate of its base"
	check "an edit of $path: git's hook asks the base's gate" 1 "$(gitpush origin edit)"
	contains "reason: git's hook names the base's gate for $path" "$work/push.err" "judged it too"
done
gate_paths=".githooks hack/sdlc .claude/settings.json"
# git's hook compares the pushed commit, HEAD and the work tree with the base, each on the same paths.
hook_lists=$(sed -n -e 's/.*git diff --quiet .* -- \(.*\) \&\&$/\1/p' -e 's/.*git diff --quiet .* -- \(.*\); then$/\1/p' "$src/.githooks/pre-push")
check "git's hook compares three path lists" 3 "$(wc -l <<<"$hook_lists" | tr -d ' ')"
check "git's hook sends the gate's paths to the base's gate" "$gate_paths" "$(sort -u <<<"$hook_lists")"
check "pushcheck sends the same paths" "$gate_paths" \
	"$(sed -n 's/^var gateFiles = \[\]string{\(.*\)}/\1/p' "$src/hack/sdlc/pushcheck/main.go" | tr -d '",')"
# A change to the gate that has its records goes through: the base's gate (this repository's real
# gate.sh, which sources the base's lib.sh) accepts it, in both hooks.
git checkout -q -B gatefix main
echo '# a comment' >>hack/sdlc/gate.sh
git commit -qam "fix(sdlc): comment the gate"
gtree=$(git rev-parse 'HEAD^{tree}')
printf 'tree=%s\nresult=pass\n' "$gtree" >"$state/checks/$gtree"
printf 'tree=%s\nresult=pass\nblocking=0\n' "$gtree" >"$state/reviews/$gtree"
check "a recorded gate change: pushcheck lets it through" 0 "$(hook 'git push origin gatefix')"
check "a recorded gate change: git's hook lets it through" 0 "$(gitpush origin gatefix)"
check "remote got the recorded gate change" "$(git rev-parse HEAD)" "$(remote_has gatefix)"

# The review input names this tree and the changed files since origin/main, whatever
# PATROL_SDLC_BASE says: a narrower review must not be recorded for the whole tree.
git checkout -q change
input=$(PATROL_SDLC_BASE=HEAD hack/sdlc/review-input.sh)
check "review input tree" "$newtree" "$(jq -r .tree <<<"$input")"
check "review input files" "main.go" "$(jq -r '.files | join(",")' <<<"$input")"
check "review input docsOnly" false "$(jq -r .docsOnly <<<"$input")"
if [[ -s $(jq -r .diffFile <<<"$input") ]]; then check "review input diff written" yes yes; else check "review input diff written" yes no; fi
# Attributes (here the clone's own) or a textconv driver cannot hide the content from the reviewers.
printf '* -diff\n' >.git/info/attributes
input=$(hack/sdlc/review-input.sh)
check "review diff shows content despite -diff" 1 "$(grep -c '^+++ b/main.go' "$(jq -r .diffFile <<<"$input")")"
printf '* diff=hide\n' >.git/info/attributes
git config diff.hide.textconv 'echo hidden'
input=$(hack/sdlc/review-input.sh)
check "review diff shows content despite textconv" 0 "$(grep -c hidden "$(jq -r .diffFile <<<"$input")")"
git config --unset diff.hide.textconv
rm .git/info/attributes

# An ignored file the checks could read (a .gitignore pattern such as kubeconfig* hiding a source
# file) leaves the tree unclean, so nothing is recorded for a tree that lacks it; build outputs don't.
echo 'kubeconfig*' >>.git/info/exclude
touch kubeconfig.go
# shellcheck source=hack/sdlc/lib.sh
check "an ignored source file is not clean" 1 "$(source hack/sdlc/lib.sh && sdlc_clean && echo 0 || echo 1)"
# shellcheck source=hack/sdlc/lib.sh
check "the reason names it" "  ignored by git, but read by the checks: kubeconfig.go" "$(source hack/sdlc/lib.sh && sdlc_unclean 2>&1)"
rm kubeconfig.go
touch bin/tool.out
# shellcheck source=hack/sdlc/lib.sh
check "ignored build outputs are clean" 0 "$(source hack/sdlc/lib.sh && sdlc_clean && echo 0 || echo 1)"
# An ignored .env file is no check's input, but under ui/ Vite and vitest read it.
printf '.env\n.env.*\n' >>.git/info/exclude
touch .env hack/.env.local
# shellcheck source=hack/sdlc/lib.sh
check "an ignored .env outside ui/ is clean" 0 "$(source hack/sdlc/lib.sh && sdlc_clean && echo 0 || echo 1)"
mkdir ui
touch ui/keep ui/.env.test.local
git add ui/keep # ui/ holds a tracked file, as in the project, so git names the ignored one in it
# shellcheck source=hack/sdlc/lib.sh
check "an ignored .env under ui/ is read by the checks" "ui/.env.test.local" "$(source hack/sdlc/lib.sh && sdlc_ignored)"
git rm -q --cached ui/keep
rm -r .env hack/.env.local ui
# A tracked file git status is told not to look at hides its changes, so it leaves the tree unclean.
git update-index --skip-worktree main.go
# shellcheck source=hack/sdlc/lib.sh
check "a skip-worktree file is not clean" 1 "$(source hack/sdlc/lib.sh && sdlc_clean && echo 0 || echo 1)"
# shellcheck source=hack/sdlc/lib.sh
check "the reason names it" "  hidden from git status (assume-unchanged or skip-worktree): main.go" "$(source hack/sdlc/lib.sh && sdlc_unclean 2>&1)"
git update-index --no-skip-worktree main.go
git update-index --assume-unchanged main.go
# shellcheck source=hack/sdlc/lib.sh
check "an assume-unchanged file is not clean" 1 "$(source hack/sdlc/lib.sh && sdlc_clean && echo 0 || echo 1)"
git update-index --no-assume-unchanged main.go
# An ignored dot file can configure a linter (.shellcheckrc, .golangci.yml): not clean. Tool state
# directories (.cache/) are.
printf '.shellcheckrc\n.cache/\n' >>.git/info/exclude
echo 'disable=all' >.shellcheckrc
# shellcheck source=hack/sdlc/lib.sh
check "an ignored linter configuration is not clean" 1 "$(source hack/sdlc/lib.sh && sdlc_clean && echo 0 || echo 1)"
rm .shellcheckrc
mkdir -p .cache && touch .cache/state
# shellcheck source=hack/sdlc/lib.sh
check "an ignored tool state directory is clean" 0 "$(source hack/sdlc/lib.sh && sdlc_clean && echo 0 || echo 1)"
rm -r .cache
# A replace ref that gives origin/main HEAD's tree cannot make the change look empty.
mainsha=$(git rev-parse refs/remotes/origin/main)
git replace "$mainsha" "$(git commit-tree "$(git rev-parse 'HEAD^{tree}')" -m fake)"
# shellcheck source=hack/sdlc/lib.sh
check "replace refs do not change what counts as the change" main.go \
	"$(source hack/sdlc/lib.sh && sdlc_changed "$(sdlc_base HEAD)" HEAD | paste -sd, -)"
git replace -d "$mainsha" >/dev/null
# Nor can a graft that makes HEAD a root commit and origin/main's child.
printf '%s\n%s %s\n' "$(git rev-parse HEAD)" "$mainsha" "$(git rev-parse HEAD)" >.git/info/grafts
# shellcheck source=hack/sdlc/lib.sh
check "grafts do not change what counts as the change" main.go \
	"$(source hack/sdlc/lib.sh && sdlc_changed "$(sdlc_base HEAD 2>/dev/null)" HEAD 2>/dev/null | paste -sd, -)"
rm .git/info/grafts

# check.sh records nothing when the environment can weaken a check (sdlc_weakened).
printf 'GOLANGCI_LINT ?= golangci-lint\n' >"$work/w.mk"
printf 'GOFLAGS=-skip=TestX\n' >"$work/goenv"
# shellcheck source=hack/sdlc/lib.sh
check "weakened: nothing set" "" "$(unset CI_GO_TEST_FLAGS GOLANGCI_LINT BASH_ENV && source "$src/hack/sdlc/lib.sh" && GOENV=off GOFLAGS='' sdlc_weakened "$work/w.mk")"
# shellcheck source=hack/sdlc/lib.sh
check "weakened: a ?= variable" GOLANGCI_LINT "$(unset CI_GO_TEST_FLAGS && source "$src/hack/sdlc/lib.sh" && GOENV=off GOFLAGS='' GOLANGCI_LINT=true sdlc_weakened "$work/w.mk")"
# Set to the empty string, a ?= variable is defined for make, so its default does not apply.
# shellcheck source=hack/sdlc/lib.sh
check "weakened: a ?= variable set empty" GOLANGCI_LINT "$(unset CI_GO_TEST_FLAGS && source "$src/hack/sdlc/lib.sh" && GOENV=off GOFLAGS='' GOLANGCI_LINT='' sdlc_weakened "$work/w.mk")"
# shellcheck source=hack/sdlc/lib.sh
check "weakened: CI_GO_TEST_FLAGS set empty" CI_GO_TEST_FLAGS "$(unset GOLANGCI_LINT && source "$src/hack/sdlc/lib.sh" && GOENV=off GOFLAGS='' CI_GO_TEST_FLAGS='' sdlc_weakened "$work/w.mk")"
# A ?= variable of an included makefile (the root Makefile includes hack/kind/kind.mk), at any depth.
printf 'include %s/w2.mk\n' "$work" >>"$work/w.mk"
printf -- '-include %s/w3.mk %s/missing.mk\nNESTED_A ?= a\n' "$work" "$work" >"$work/w2.mk"
printf 'include %s/w.mk\nNESTED_B ?= b\n' "$work" >"$work/w3.mk" # a cycle, read once
# shellcheck source=hack/sdlc/lib.sh
check "weakened: a ?= variable of an included makefile" "NESTED_A,NESTED_B" "$(unset CI_GO_TEST_FLAGS GOLANGCI_LINT && source "$src/hack/sdlc/lib.sh" && GOENV=off GOFLAGS='' NESTED_A=x NESTED_B='' sdlc_weakened "$work/w.mk" | paste -sd, -)"
# shellcheck source=hack/sdlc/lib.sh
check "weakened: the project's makefiles include hack/kind/kind.mk's" KIND_IMAGES "$(cd "$src" && unset CI_GO_TEST_FLAGS && source hack/sdlc/lib.sh && GOENV=off GOFLAGS='' KIND_IMAGES=x sdlc_weakened Makefile hack/*.mk | grep -x KIND_IMAGES)"
# shellcheck source=hack/sdlc/lib.sh
check "weakened: GOFLAGS in Go's env file" "GOFLAGS (go env)" "$(unset CI_GO_TEST_FLAGS GOLANGCI_LINT && source "$src/hack/sdlc/lib.sh" && GOENV=$work/goenv GOFLAGS='' sdlc_weakened "$work/w.mk")"
# BASH_ENV, and an exported function, reach every bash the checks start, where a function can stand
# in for a tool.
# shellcheck source=hack/sdlc/lib.sh
check "weakened: BASH_ENV" BASH_ENV "$(unset CI_GO_TEST_FLAGS GOLANGCI_LINT && source "$src/hack/sdlc/lib.sh" && GOENV=off GOFLAGS='' BASH_ENV=/dev/null sdlc_weakened "$work/w.mk")"
# shellcheck source=hack/sdlc/lib.sh disable=SC2329 # gofmt is called by the checks, not here
check "weakened: an exported function" "exported function gofmt" "$(unset CI_GO_TEST_FLAGS GOLANGCI_LINT BASH_ENV && source "$src/hack/sdlc/lib.sh" && gofmt() { :; } && export -f gofmt && GOENV=off GOFLAGS='' sdlc_weakened "$work/w.mk")"

# check.sh end to end, in a scratch repository (a Go module of two packages) whose make targets are
# stubs that log what they were asked to run: the step for a kind script waits for a marker when
# MARKS is set, so the test can act while the checks run. A clean run records a pass; a run during
# which HEAD moves (to another branch and back, a stash) records nothing.
chk=$work/chk
marks=$work/chk-marks
mkdir -p "$chk/hack/sdlc" "$chk/hack/kind" "$chk/a/testdata" "$chk/b" "$marks"
cp "$src"/hack/sdlc/{check.sh,lib.sh} "$chk/hack/sdlc/"
printf 'module example.com/chk\n\ngo 1.21\n' >"$chk/go.mod"
printf 'package a\n' >"$chk/a/a.go"
printf 'package b\n' >"$chk/b/b.go"
# shellcheck disable=SC2016 # make expands $(MARKS), $(LOG) and the others, not the shell
{
	printf 'ci-static release-lint-commits:\n\t@[ "$(MAKE)" != false ]\n'
	printf 'ci-unit ci-integration ci-deploy ci-ui image-verify-pins:\n\t@echo "$(strip $@ $(CI_UNIT_PKGS) $(VERIFY_PINS_FLAGS))" >>"$(LOG)"\n'
	printf 'ci-scripts:\n\t@echo $@ >>"$(LOG)"\n'
	printf '\t@[ -z "$(MARKS)" ] || { touch "$(MARKS)/started"; while [ ! -e "$(MARKS)/go" ]; do sleep 0.1; done; rm -f "$(MARKS)/started" "$(MARKS)/go"; }\n'
} >"$chk/Makefile"
touch "$chk/hack/none.mk"
git -C "$chk" init -q -b main
git -C "$chk" add -A
git -C "$chk" commit -q -m "chore: base"
git -C "$chk" update-ref refs/remotes/origin/main HEAD
git -C "$chk" switch -q -c change
checkrun() { # checkrun N ACTION: commit kind script N, run check.sh, do ACTION while a step runs; prints the exit status
	echo "echo $1" >"$chk/hack/kind/x.sh"
	git -C "$chk" add -A
	git -C "$chk" commit -q -m "chore: kind script $1"
	local rc=0 tries=0
	# Without what makes check.sh check but record nothing (CI's GOFLAGS, a PATROL_SDLC_BASE set for
	# a local iteration, Go's env file, BASH_ENV), so the test asserts the recording path wherever it runs.
	# MAKE=false too: check.sh must drop it, or the stub's ci-static fails (as $(MAKE) recipes would be emptied).
	(cd "$chk" && env -u PATROL_SDLC_BASE -u CI_GO_TEST_FLAGS -u GOFLAGS -u BASH_ENV GOENV=off MAKE=false MARKS="$marks" LOG="$work/chk.log" hack/sdlc/check.sh >"$work/chk.out" 2>&1) &
	local pid=$!
	while [[ ! -e $marks/started && $tries -lt 300 ]]; do
		sleep 0.1
		tries=$((tries + 1))
	done
	(cd "$chk" && eval "$2")
	touch "$marks/go"
	wait "$pid" || rc=$?
	echo "$rc"
}
recorded() { # recorded: the check record of HEAD's tree in the scratch repository, or "none"
	cat "$(git -C "$chk" rev-parse --path-format=absolute --git-common-dir)/patrol-sdlc/checks/$(git -C "$chk" rev-parse 'HEAD^{tree}')" 2>/dev/null | grep -x 'result=pass' || echo none
}
check "check.sh: a clean run" 0 "$(checkrun 1 true)"
check "check.sh: a clean run records a pass" result=pass "$(recorded)"
check "check.sh: HEAD to another branch and back during the run" 1 "$(checkrun 2 'git switch -q main && git switch -q -')"
contains "reason: HEAD moved" "$work/chk.out" "HEAD moved"
check "check.sh: nothing recorded after HEAD moved" none "$(recorded)"
check "check.sh: a stash during the run" 1 "$(checkrun 3 'echo y >>hack/kind/x.sh && git stash -q && git stash pop -q && git checkout -q -- hack/kind/x.sh')"
check "check.sh: nothing recorded after a stash" none "$(recorded)"

# What check.sh runs for a change: a package's data file tests that package; a file outside every
# package (examples/, a CRD, the OpenAPI document, the Dockerfile) tests every package, as a test
# anywhere may read it; the UI's sources and the agents' instructions test none.
selected() { # selected FILE...: from the base, commit a change to each FILE and run check.sh --no-codeql; prints the steps run
	git -C "$chk" switch -q -C sel main
	local f
	for f; do
		mkdir -p "$chk/$(dirname "$f")"
		echo '// x' >>"$chk/$f" # a comment to go.mod, which go list still reads
	done
	git -C "$chk" add -A
	git -C "$chk" commit -q -m "chore: change"
	: >"$work/chk.log"
	(cd "$chk" && env -u PATROL_SDLC_BASE -u CI_GO_TEST_FLAGS -u GOFLAGS -u BASH_ENV -u MARKS GOENV=off LOG="$work/chk.log" hack/sdlc/check.sh --no-codeql >"$work/sel.out" 2>&1) || echo "check.sh failed: $(tail -n 3 "$work/sel.out")"
	paste -sd ';' "$work/chk.log"
}
all="ci-unit example.com/chk/a example.com/chk/b"
check "selection: a package's data file" "ci-unit example.com/chk/a" "$(selected a/testdata/x.txt)"
check "selection: a package's data file and documentation" "ci-unit example.com/chk/a" "$(selected a/testdata/x.txt docs/x.md)"
check "selection: a file outside every package" "$all" "$(selected examples/app.yaml)"
check "selection: a file outside every package after a package's" "$all" "$(selected a/testdata/x.txt examples/app.yaml)"
check "selection: a UI source" ci-ui "$(selected ui/src/x.ts)"
check "selection: the agents' instructions" "" "$(selected sub/CLAUDE.md sub/CLAUDE.local.md sub/AGENTS.md .claude/x.md)"
check "selection: the OpenAPI document" "$all;ci-ui" "$(selected api/openapi/openapi.json)"
check "selection: a CRD" "$all;ci-deploy;ci-scripts" "$(selected deploy/crds/x.yaml)"
check "selection: the CRD types' directory" "$all;ci-scripts" "$(selected api/v1alpha1/x.txt)"
check "selection: the Dockerfile" "$all;image-verify-pins --offline" "$(selected Dockerfile)"
check "selection: the UI's package.json" "ci-ui;image-verify-pins --offline" "$(selected ui/package.json)"
check "selection: go.mod" "$all;image-verify-pins --offline" "$(selected go.mod)"
check "selection: the image's pins" "$all;image-verify-pins --offline" "$(selected hack/images/checksums.txt)"

echo "test-sdlc: $passed passed, $failed failed"
[[ $failed -eq 0 ]]
