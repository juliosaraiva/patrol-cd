#!/usr/bin/env bash
# Claude Code PostToolUse hook for Edit, MultiEdit and Write (.claude/settings.json): formats a Go
# file of this repository with gofmt right after Claude wrote it, so `make ci-gofmt` never fails on
# an edit. Other files are left alone (the web UI has no formatter; eslint runs in make ci-ui).
set -uo pipefail

file=$(jq -r '.tool_input.file_path // empty' 2>/dev/null) || exit 0
[[ $file == *.go && -f $file ]] || exit 0
top=$(git -C "$(dirname "$file")" rev-parse --show-toplevel 2>/dev/null) || exit 0
[[ -x $top/hack/sdlc/gate.sh ]] || exit 0
gofmt -w "$file" >&2 || { # Claude Code shows a hook's stderr, with gofmt's file:line error
	echo "gofmt could not format $file (syntax error?)" >&2
	exit 2
}
