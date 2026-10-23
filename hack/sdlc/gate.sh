#!/usr/bin/env bash
# The push gate of the SDLC (docs/guides/claude-code-sdlc.md): succeeds when COMMIT's tree has a
# passing check record (hack/sdlc/check.sh) and, unless the change is documentation only, a review
# record without blocking findings (hack/sdlc/record-review.sh, written by the review-change
# skill). It reads records only and runs in a second, so the Claude Code hook
# (hack/sdlc/claude-pre-push.sh) and the git pre-push hook (.githooks/pre-push) can call it on every
# push.
#
# Usage: hack/sdlc/gate.sh [COMMIT]   (default HEAD)
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
# shellcheck source=hack/sdlc/lib.sh
source hack/sdlc/lib.sh

commit=${1:-HEAD}
tree=$(git rev-parse "$commit^{tree}")
state=$(sdlc_state_dir)
base=$(sdlc_base "$commit")
problems=()

if ! grep -qx 'result=pass' "$state/checks/$tree" 2>/dev/null; then
	problems+=("no passing deterministic checks for tree $tree: run \`make sdlc-check\` on a clean tree at this commit")
fi
if sdlc_changed "$base" "$commit" | sdlc_docs_only; then
	review="not needed (documentation only)"
else
	review="missing"
	if [[ -f $state/reviews/$tree ]]; then
		if grep -qx 'result=pass' "$state/reviews/$tree"; then
			review="passed"
		else
			review="blocking findings ($(sed -n 's/^blocking=//p' "$state/reviews/$tree"))"
		fi
	fi
	if [[ $review != passed ]]; then
		problems+=("agent review for tree $tree: $review: run the review-change skill (/review-change), fix every blocking finding, commit, and check again")
	fi
fi

if [[ ${#problems[@]} -gt 0 ]]; then
	echo "sdlc-gate: push of $(git rev-parse --short "$commit") refused:" >&2
	for p in "${problems[@]}"; do
		echo "  - $p" >&2
	done
	echo "  (docs/guides/claude-code-sdlc.md)" >&2
	exit 1
fi
echo "sdlc-gate: $(git rev-parse --short "$commit") may be pushed (checks passed; review $review)"
