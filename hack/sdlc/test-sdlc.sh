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

# Build the hook's binary (the wrapper builds it on first use).
(cd "$src" && go build -o bin/sdlc-pushcheck ./hack/sdlc/pushcheck)

# A clone with the gate scripts and the git hook on main, a bare remote, and a change branch.
remote=$work/remote.git
git init -q --bare -b main "$remote"
repo=$work/repo
git init -q -b main "$repo"
mkdir -p "$repo/hack/sdlc" "$repo/hack/ci" "$repo/docs" "$repo/.githooks"
cp "$src"/hack/sdlc/{lib.sh,gate.sh,record-review.sh,review-input.sh} "$repo/hack/sdlc/"
cp "$src/hack/ci/classify-changes.sh" "$repo/hack/ci/"
cp "$src/.githooks/pre-push" "$repo/.githooks/"
cd "$repo"
git add -A
git commit -q -m base
git remote add origin "$remote"
git push -q origin main
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
check "push from another repository" 0 "$(hook "git -C $work/other push")"

# Without core.hooksPath the hook refuses every push from this repository.
check "git hook not enabled" 2 "$(hook 'git push -u origin change')"
contains "reason: make sdlc-git-hooks" "$work/hook.err" "make sdlc-git-hooks"
git config core.hooksPath .githooks

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
check "unknown source" 2 "$(hook 'git push origin no-such-branch')"

# No records: the hook and git's hook both refuse.
check "bare push" 2 "$(hook 'git push')"
contains "reason names the tree" "$work/hook.err" "no passing deterministic checks for tree $tree"
check "git -C DIR push" 2 "$(hook "git -C $repo push origin HEAD" "$work")"
check "cd DIR && git push" 2 "$(hook "cd $repo && git push origin change" "$work")"
check "git hook refuses the push" 1 "$(gitpush origin change)"
check "remote did not get it" none "$(remote_has change)"

# Checks recorded, review missing, then failing, then passing.
mkdir -p "$state/checks" "$state/reviews"
printf 'tree=%s\nresult=pass\n' "$tree" >"$state/checks/$tree"
check "review missing" 1 "$(gate)"
contains "reason says review missing" "$work/gate.err" "agent review for tree $tree: missing"
echo "# review" >"$work/report.md"
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

# Commit and push in one command: the Claude Code hook sees the recorded commit, but git's hook sees
# the new one and refuses it.
echo more >>main.go
check "hook runs before the commit exists" 0 "$(hook 'git commit -qam "fix: more" && git push origin change')"
git commit -qam "fix: more"
check "git hook refuses the new commit" 1 "$(gitpush origin change)"
check "remote kept the checked commit" "$(git rev-parse HEAD~1)" "$(remote_has change)"
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
if [[ -s $(jq -r .diffFile <<<"$input") ]]; then check "review input diff written" yes yes; else check "review input diff written" yes no; fi

echo "test-sdlc: $passed passed, $failed failed"
[[ $failed -eq 0 ]]
