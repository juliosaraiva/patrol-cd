#!/usr/bin/env bash
# Claude Code PreToolUse hook for the Bash tool (.claude/settings.json): blocks a `git push` whose
# commits hack/sdlc/gate.sh does not accept (docs/guides/claude-code-sdlc.md).
#
# git's own pre-push hook (.githooks/pre-push) is the gate's authority: it also sees what a script, an
# alias or another tool pushes. So while this checkout has it, every command is refused until it is
# enabled (`make sdlc-git-hooks`). Then a command without the word "push" returns at once; the others
# go to hack/sdlc/pushcheck, which parses them as bash (so a "git push" in quoted text or a heredoc
# is not a push) and runs the gate. The binary is built into bin/ on first use and rebuilt when its
# sources or go.sum change. Exit 2 blocks the command and shows stderr to Claude Code.
set -uo pipefail

input=$(cat)
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)

if [[ -f $root/.githooks/pre-push && $input != *sdlc-git-hooks* ]] && git -C "$root" rev-parse --git-dir >/dev/null 2>&1 &&
	[[ $(git -C "$root" config --get core.hooksPath) != .githooks ]]; then
	echo "sdlc pre-push hook: git's pre-push hook is not enabled in $root (core.hooksPath is not .githooks): run \`make sdlc-git-hooks\` once, then run the command again" >&2
	exit 2
fi
[[ $input == *push* ]] || exit 0

bin=$root/bin/sdlc-pushcheck
if [[ ! -x $bin || -n $(find "$root/hack/sdlc/pushcheck" "$root/go.sum" -newer "$bin" -print 2>/dev/null | head -n 1) ]]; then
	if ! (cd "$root" && go build -o "$bin" ./hack/sdlc/pushcheck) >&2; then
		echo "sdlc pre-push hook: cannot build hack/sdlc/pushcheck; fix the build, or push by hand" >&2
		exit 2
	fi
fi
"$bin" "$root" <<<"$input"
