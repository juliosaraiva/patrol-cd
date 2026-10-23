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
# commands that mention git or what would skip its hook are refused and the others run, so the build
# can be fixed. The browser pane's preview_start runs the command a .claude/launch.json configuration
# names; that command is checked the same way (which needs jq). Exit 2 blocks the command and shows
# stderr to Claude Code.
set -uo pipefail

input=$(cat)
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
if ! command -v jq >/dev/null 2>&1 && [[ $input == *Claude_Browser__preview_start* ]]; then
	echo "sdlc pre-push hook: jq is not installed, so the command preview_start would run cannot be read and checked; install jq" >&2
	exit 2
fi
command=$(jq -r '.tool_input.command // empty' <<<"$input" 2>/dev/null) || command=""
if [[ $(jq -r '.tool_name // empty' <<<"$input" 2>/dev/null) == *Claude_Browser__preview_start ]]; then
	name=$(jq -r '.tool_input.name // empty' <<<"$input" 2>/dev/null)
	if [[ -n $name ]]; then
		# shellcheck disable=SC2016 # a jq program, not shell
		command=$(jq -er --arg n "$name" '[.configurations[]? | select(.name == $n)]
			| if length == 1 then .[0] | ([.runtimeExecutable // empty] + (.runtimeArgs // [])) | map(tostring | @sh) | join(" ")
			  else error("not one configuration") end' "$root/.claude/launch.json" 2>/dev/null) || {
			echo "sdlc pre-push hook: cannot read the configuration \"$name\" from .claude/launch.json, so the command preview_start would run cannot be checked" >&2
			exit 2
		}
		input=$(jq --arg c "$command" '.tool_input.command = $c' <<<"$input")
	fi
fi

if [[ -f $root/.githooks/pre-push && $command != "make sdlc-git-hooks" ]] && git -C "$root" rev-parse --git-dir >/dev/null 2>&1 &&
	[[ $(git -C "$root" config --get core.hooksPath) != .githooks ]]; then
	echo "sdlc pre-push hook: git's pre-push hook is not enabled in $root (core.hooksPath is not .githooks): run \`make sdlc-git-hooks\` once, then run the command again" >&2
	exit 2
fi

bin=$root/bin/sdlc-pushcheck
if [[ ! -x $bin || -n $(find "$root/hack/sdlc/pushcheck" "$root/go.sum" -newer "$bin" -print 2>/dev/null | head -n 1) ]]; then
	if ! (cd "$root" && go build -o "$bin.tmp" ./hack/sdlc/pushcheck && mv -f "$bin.tmp" "$bin") >/dev/null 2>&1 && [[ ! -x $bin ]]; then
		# Lower case, without quotes and backslashes: git, GIT_*, any abbreviation of --no-verify, hooksPath.
		plain=$(printf '%s' "${command//[\"\'\\]/}" | tr '[:upper:]' '[:lower:]')
		if [[ $plain == *git* || $plain == *no-v* || $plain == *hookspath* ]]; then
			echo "sdlc pre-push hook: cannot build hack/sdlc/pushcheck, so this command, which mentions git or what would skip git's hook, cannot be checked; fix the build first" >&2
			exit 2
		fi
		exit 0
	fi
fi
"$bin" "$root" <<<"$input"
