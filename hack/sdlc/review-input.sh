#!/usr/bin/env bash
# Prepares the input of the agent review (the review-change skill, .claude/workflows/sdlc-review.js):
# writes the diff of the branch since its merge base with origin/main to the review directory and
# prints a JSON object for the workflow's args: repo, base, head, tree, docsOnly, diffFile, files.
# Refuses a dirty tree, because the review is recorded for HEAD's tree.
#
# Usage: hack/sdlc/review-input.sh
set -euo pipefail

# The review is recorded for the whole tree, so it covers the whole change since origin/main, as
# hack/sdlc/gate.sh measures it: PATROL_SDLC_BASE (local iteration of check.sh) may not narrow it.
unset PATROL_SDLC_BASE
cd "$(git rev-parse --show-toplevel)"
# shellcheck source=hack/sdlc/lib.sh
source hack/sdlc/lib.sh

if ! sdlc_clean; then
	echo "review-input: commit or stash the working tree first; the review is recorded for HEAD's tree" >&2
	exit 1
fi
head=$(git rev-parse HEAD)
tree=$(git rev-parse 'HEAD^{tree}')
base=$(sdlc_base HEAD)
state=$(sdlc_state_dir)
diff_file=$state/reviews/$tree.diff
git diff --no-color --no-ext-diff "$base" HEAD >"$diff_file"
docs_only=false
sdlc_changed "$base" HEAD | sdlc_docs_only && docs_only=true

json_string() { # escape for a JSON string literal
	local s=${1//\\/\\\\}
	s=${s//\"/\\\"}
	s=${s//$'\t'/\\t}
	printf '"%s"' "$s"
}
printf '{"repo":%s,"base":"%s","head":"%s","tree":"%s","docsOnly":%s,"diffFile":%s,"files":[' \
	"$(json_string "$PWD")" "$base" "$head" "$tree" "$docs_only" "$(json_string "$diff_file")"
sep=
while IFS= read -r f; do
	printf '%s%s' "$sep" "$(json_string "$f")"
	sep=,
done < <(sdlc_changed "$base" HEAD)
printf ']}\n'
