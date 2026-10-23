#!/usr/bin/env bash
# Claude Code PreToolUse hook for the Bash tool (.claude/settings.json): blocks a `git push` whose
# commits hack/sdlc/gate.sh does not accept (docs/guides/claude-code-sdlc.md). A command without the
# word "push" returns at once; the others go to hack/sdlc/pushcheck, which parses them as bash (so a
# "git push" in quoted text or a heredoc is not a push) and runs the gate. The binary is built into
# bin/ on first use and rebuilt when its sources or go.sum change. Exit 2 blocks the command and
# shows stderr to Claude Code.
set -uo pipefail

input=$(cat)
[[ $input == *push* ]] || exit 0

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
bin=$root/bin/sdlc-pushcheck
if [[ ! -x $bin || -n $(find "$root/hack/sdlc/pushcheck" "$root/go.sum" -newer "$bin" -print 2>/dev/null | head -n 1) ]]; then
	if ! (cd "$root" && go build -o "$bin" ./hack/sdlc/pushcheck) >&2; then
		echo "sdlc pre-push hook: cannot build hack/sdlc/pushcheck; fix the build, or push by hand" >&2
		exit 2
	fi
fi
"$bin" "$root" <<<"$input"
