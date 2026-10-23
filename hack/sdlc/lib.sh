# Helpers shared by the hack/sdlc scripts (sourced, not run). See docs/guides/claude-code-sdlc.md.
# shellcheck shell=bash

# sdlc_state_dir prints the directory that holds this clone's check and review records. It lives in
# the git directory, so records are never committed, and in the common one, so every worktree of the
# clone shares them (records are keyed by tree hash, which names the content exactly).
sdlc_state_dir() {
	local d
	d=$(git rev-parse --path-format=absolute --git-common-dir)/patrol-sdlc
	mkdir -p "$d/checks" "$d/reviews"
	printf '%s\n' "$d"
}

# sdlc_base COMMIT prints the commit a change is measured from: the merge base of COMMIT and the
# upstream main branch (PATROL_SDLC_BASE overrides origin/main).
sdlc_base() {
	git merge-base "$1" "${PATROL_SDLC_BASE:-origin/main}"
}

# sdlc_changed BASE COMMIT prints the files that differ between BASE and COMMIT, one per line.
sdlc_changed() {
	git -c core.quotePath=false diff --name-only --no-renames "$1" "$2"
}

# sdlc_docs_only reads file names on stdin and succeeds when every one is documentation: the rule
# of docs_re in hack/ci/classify-changes.sh (hack/sdlc/test-sdlc.sh checks that they agree). No
# file at all also counts (there is nothing to review).
sdlc_docs_only() {
	local f
	while IFS= read -r f; do
		[[ -z $f || $f =~ ^(docs/|[^/]+\.md$|\.gitignore$) ]] || return 1
	done
	return 0
}

# sdlc_clean succeeds when the working tree has no tracked changes and no untracked files that git
# does not ignore: only then is HEAD's tree what the checks saw.
sdlc_clean() {
	[[ -z $(git status --porcelain --untracked-files=normal) ]]
}
