#!/usr/bin/env bash
# Claude Code PreToolUse hook for the tools that run commands (.claude/settings.json): blocks a
# `git push` whose commits hack/sdlc/gate.sh does not accept, and whatever would keep git's own
# pre-push hook from checking a push (docs/guides/claude-code-sdlc.md).
#
# git's pre-push hook (.githooks/pre-push) is the gate's authority: it also sees what a script, an
# alias or another tool pushes. So while this checkout has it, every command but
# `make sdlc-git-hooks` is refused until it is enabled. Then every command goes to
# hack/sdlc/pushcheck, which parses it as bash (so a "git push" in quoted text or a heredoc is not a
# push) and runs the gate. The binary is built into bin/ on first use and rebuilt when its sources or
# go.sum change; while a change does not build, the last binary that did is used, and without one,
# commands that mention git are refused and the others run, so the build can be fixed. Exit 2 blocks
# the command and shows stderr to Claude Code.
set -uo pipefail

input=$(cat)
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
command=$(jq -r '.tool_input.command // empty' <<<"$input" 2>/dev/null) || command=""

if [[ -f $root/.githooks/pre-push && $command != "make sdlc-git-hooks" ]] && git -C "$root" rev-parse --git-dir >/dev/null 2>&1 &&
	[[ $(git -C "$root" config --get core.hooksPath) != .githooks ]]; then
	echo "sdlc pre-push hook: git's pre-push hook is not enabled in $root (core.hooksPath is not .githooks): run \`make sdlc-git-hooks\` once, then run the command again" >&2
	exit 2
fi

bin=$root/bin/sdlc-pushcheck
if [[ ! -x $bin || -n $(find "$root/hack/sdlc/pushcheck" "$root/go.sum" -newer "$bin" -print 2>/dev/null | head -n 1) ]]; then
	if ! (cd "$root" && go build -o "$bin.tmp" ./hack/sdlc/pushcheck && mv -f "$bin.tmp" "$bin") >/dev/null 2>&1 && [[ ! -x $bin ]]; then
		plain=${command//[\"\'\\]/}
		if [[ $plain == *git* || $plain == *GIT_* ]]; then
			echo "sdlc pre-push hook: cannot build hack/sdlc/pushcheck, so this command, which mentions git, cannot be checked; fix the build first" >&2
			exit 2
		fi
		exit 0
	fi
fi
"$bin" "$root" <<<"$input"
