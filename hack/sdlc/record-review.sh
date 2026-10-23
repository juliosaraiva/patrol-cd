#!/usr/bin/env bash
# Records the outcome of the agent review (the review-change skill) for HEAD's tree, which
# hack/sdlc/gate.sh requires before a push. The review must have been run on this exact tree:
# --tree is the tree the review input named, and HEAD must still have it.
#
# Usage: hack/sdlc/record-review.sh --tree TREE --blocking N --report FILE
#   N     number of verified findings of severity blocker or major (0 lets the push through)
#   FILE  the review report (Markdown); copied next to the record
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
# shellcheck source=hack/sdlc/lib.sh
source hack/sdlc/lib.sh

usage() {
	echo "usage: hack/sdlc/record-review.sh --tree TREE --blocking N --report FILE" >&2
	exit 2
}
tree='' blocking='' report=''
while [[ $# -gt 0 ]]; do
	case $1 in
	--tree) tree=${2:-} && shift 2 ;;
	--blocking) blocking=${2:-} && shift 2 ;;
	--report) report=${2:-} && shift 2 ;;
	*) usage ;;
	esac
done
[[ -n $tree && $blocking =~ ^[0-9]+$ && -f $report ]] || usage

current=$(git rev-parse 'HEAD^{tree}')
if [[ $tree != "$current" ]] || ! sdlc_clean; then
	echo "record-review: the review was for tree $tree, but HEAD's tree is $current (or the working tree is dirty); review again" >&2
	exit 1
fi
state=$(sdlc_state_dir)
if [[ $report != "$state/reviews/$tree.md" ]]; then
	cp "$report" "$state/reviews/$tree.md"
fi
result=pass
[[ $blocking -eq 0 ]] || result=fail
{
	echo "tree=$tree"
	echo "head=$(git rev-parse HEAD)"
	echo "result=$result"
	echo "blocking=$blocking"
	echo "report=$state/reviews/$tree.md"
	echo "time=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
} >"$state/reviews/$tree"
echo "record-review: tree $tree: $result ($blocking blocking findings); report $state/reviews/$tree.md"
