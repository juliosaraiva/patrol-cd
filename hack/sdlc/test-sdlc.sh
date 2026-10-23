#!/usr/bin/env bash
# Regression tests of the SDLC push gate (docs/guides/claude-code-sdlc.md): the Claude Code hook's
# reading of push commands (hack/sdlc/claude-pre-push.sh), the gate's decisions on check and review
# records (hack/sdlc/gate.sh), the safeguards of hack/sdlc/record-review.sh, and the agreement of
# the documentation rule with hack/ci/classify-changes.sh. Builds scratch repositories; needs git, go
# and jq; runs in a few seconds once bin/sdlc-pushcheck is built. Run by `make ci-sdlc`.
#
#   hack/sdlc/test-sdlc.sh
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
src=$(cd "$here/../.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.invalid GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.invalid
export PATROL_SDLC_BASE=main

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

# The documentation rule must be classify-changes.sh's docs_re.
ci_re=$(sed -n "s/^docs_re='\(.*\)'\$/\1/p" "$src/hack/ci/classify-changes.sh")
# shellcheck disable=SC2016 # the pattern matches the literal text of lib.sh
sdlc_re=$(sed -n 's/.*\[\[ -z \$f || \$f =~ \(.*\) \]\] || return 1/\1/p' "$src/hack/sdlc/lib.sh")
check "docs rule agrees with classify-changes.sh" "$ci_re" "$sdlc_re"

# A repository with the SDLC scripts on main and a change branch.
repo=$work/repo
git init -q -b main "$repo"
mkdir -p "$repo/hack/sdlc" "$repo/hack/ci" "$repo/docs"
cp "$src"/hack/sdlc/{lib.sh,gate.sh,record-review.sh,review-input.sh} "$repo/hack/sdlc/"
cp "$src/hack/ci/classify-changes.sh" "$repo/hack/ci/"
cd "$repo"
git add -A
git commit -q -m base
git checkout -q -b change
echo code >main.go
git add main.go
git commit -q -m "feat: code"
state=$(git rev-parse --path-format=absolute --git-common-dir)/patrol-sdlc
tree=$(git rev-parse 'HEAD^{tree}')

hook() { # hook COMMAND [CWD]: prints the hook's exit status
	local rc=0
	jq -n --arg c "$1" --arg d "${2:-$repo}" '{tool_name: "Bash", tool_input: {command: $c}, cwd: $d}' |
		"$src/hack/sdlc/claude-pre-push.sh" >/dev/null 2>"$work/hook.err" || rc=$?
	echo "$rc"
}
gate() {
	local rc=0
	hack/sdlc/gate.sh >/dev/null 2>"$work/gate.err" || rc=$?
	echo "$rc"
}

# Commands that are not pushes, or not gated.
check "not a push" 0 "$(hook 'git status')"
check "push in a word" 0 "$(hook 'echo pushing')"
check "push in quoted text and a heredoc" 0 "$(hook "$(printf 'git commit -m \"docs: git push\"\ncat <<EOF\ngit push\nEOF')")"
check "deletion" 0 "$(hook 'git push origin --delete old')"
check "deletion refspec" 0 "$(hook 'git push origin :old')"
check "dry run" 0 "$(hook 'git push --dry-run origin change')"
check "push from a repository without the gate" 0 "$(hook 'git push' "$work")"
check "--tags is refused" 2 "$(hook 'git push --tags origin')"

# No records: every form of push of the change is blocked.
check "bare push" 2 "$(hook 'git push')"
grep -q "no passing deterministic checks for tree $tree" "$work/hook.err"
check "reason names the tree" 0 $?
check "push -u origin branch" 2 "$(hook 'git push -u origin change 2>&1 | tail -3')"
check "git -C DIR push" 2 "$(hook "git -C $repo push origin HEAD" "$work")"
check "cd DIR && git push" 2 "$(hook "cd $repo && git push origin change" "$work")"
check "push of a refspec" 2 "$(hook 'git push origin HEAD:refs/heads/x')"
check "unknown source" 2 "$(hook 'git push origin no-such-branch')"
# shellcheck disable=SC2016 # the command text holds an unexpanded variable on purpose
check "unknown directory" 2 "$(hook 'cd "$TMPDIR" && git push')"

# Checks recorded, review missing, then failing, then passing.
mkdir -p "$state/checks" "$state/reviews"
printf 'tree=%s\nresult=pass\n' "$tree" >"$state/checks/$tree"
check "review missing" 1 "$(gate)"
grep -q "agent review for tree $tree: missing" "$work/gate.err"
check "reason says review missing" 0 $?
echo "# review" >"$work/report.md"
rc=0
hack/sdlc/record-review.sh --tree "$tree" --blocking 2 --report "$work/report.md" >/dev/null || rc=$?
check "record a failing review" 0 "$rc"
check "blocking findings stop the push" 1 "$(gate)"
grep -q "blocking findings (2)" "$work/gate.err"
check "reason counts the findings" 0 $?
hack/sdlc/record-review.sh --tree "$tree" --blocking 0 --report "$work/report.md" >/dev/null
check "passing review" 0 "$(gate)"
check "hook lets the push through" 0 "$(hook 'git push -u origin change')"
check "hook: cd DIR && git push" 0 "$(hook "cd $repo && git push" "$work")"

# A new commit has a new tree: the records do not carry over.
echo more >>main.go
git commit -q -am "fix: more"
check "new tree needs new records" 2 "$(hook 'git push')"
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

# The review input names this tree and the changed files.
git checkout -q change
input=$(hack/sdlc/review-input.sh)
check "review input tree" "$newtree" "$(jq -r .tree <<<"$input")"
check "review input files" "main.go" "$(jq -r '.files | join(",")' <<<"$input")"
check "review input docsOnly" false "$(jq -r .docsOnly <<<"$input")"
check "review input diff written" 0 "$(test -s "$(jq -r .diffFile <<<"$input")" && echo 0 || echo 1)"

echo "test-sdlc: $passed passed, $failed failed"
[[ $failed -eq 0 ]]
