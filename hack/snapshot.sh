#!/usr/bin/env bash
# Records the current working tree (tracked + untracked, respecting .gitignore) as a
# recoverable snapshot under refs/patrol/snapshots/<timestamp> without creating a commit on
# any branch and without requiring commit signing. Restore with:
#   git checkout refs/patrol/snapshots/<ts> -- .        (or git stash apply <sha>)
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
git add -A
sha=$(git stash create "patrol snapshot ${1:-}")
[ -n "$sha" ] || { echo "nothing to snapshot"; exit 0; }
ts=$(date -u +%Y%m%dT%H%M%SZ)
git update-ref "refs/patrol/snapshots/$ts" "$sha"
echo "snapshot refs/patrol/snapshots/$ts -> $sha"
