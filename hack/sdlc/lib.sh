# Helpers shared by the hack/sdlc scripts (sourced, not run). See docs/guides/claude-code-sdlc.md.
# shellcheck shell=bash

# A replace ref (git replace) or a graft (.git/info/grafts) can make a commit read with another
# commit's tree or parents, and so change what counts as the change: the gate's git commands read
# history as it is. (The graft file is a path that cannot exist: git reads no grafts, and prints no
# deprecation hint as it would for an empty file.)
export GIT_NO_REPLACE_OBJECTS=1 GIT_GRAFT_FILE=/dev/null/no-grafts

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
# upstream main branch, named in full so that a local branch or tag called origin/main cannot stand
# in for it (PATROL_SDLC_BASE overrides it; the gate and the review input unset that).
sdlc_base() {
	git merge-base "$1" "${PATROL_SDLC_BASE:-refs/remotes/origin/main}"
}

# sdlc_changed BASE COMMIT prints the files that differ between BASE and COMMIT, one per line.
sdlc_changed() {
	git -c core.quotePath=false diff --name-only --no-renames "$1" "$2"
}

# sdlc_docs_only reads file names on stdin and succeeds when every one is documentation: the rule
# of docs_re in hack/ci/classify-changes.sh (hack/sdlc/test-sdlc.sh checks that they agree), except
# docs/parity/, whose status files claim features complete (such a claim needs its evidence reviewed),
# and CLAUDE.md, CLAUDE.local.md and AGENTS.md at any depth, the instructions agents follow (this
# lifecycle among them; Claude Code loads a nested CLAUDE.md for the files under it). No file at all
# also counts (nothing to review).
sdlc_docs_only() {
	local f
	while IFS= read -r f; do
		case ${f##*/} in CLAUDE.md | CLAUDE.local.md | AGENTS.md) return 1 ;; esac
		[[ $f != docs/parity/* ]] || return 1
		[[ -z $f || $f =~ ^(docs/|[^/]+\.md$|\.gitignore$) ]] || return 1
	done
	return 0
}

# sdlc_weakened MAKEFILE... prints what in the environment could weaken a check, one per line: a
# make variable the makefiles define with `?=` (replace a tool with a no-op, drop -race, change the
# commit range), CI_GO_TEST_FLAGS, and GOFLAGS from the environment or Go's env file (`go env -w`,
# GOENV), which can skip tests. The list is derived, so a new `?=` variable is covered.
sdlc_weakened() {
	local v
	while IFS= read -r v; do
		[[ -n ${!v:-} ]] && printf '%s\n' "$v"
	done < <(sed -nE 's/^[[:space:]]*([A-Z][A-Z0-9_]*)[[:space:]]*\?=.*/\1/p' "$@" | sort -u
		printf '%s\n' CI_GO_TEST_FLAGS)
	[[ -z $(go env GOFLAGS 2>/dev/null) ]] || printf '%s\n' "GOFLAGS (go env)"
}

# sdlc_ignored prints the ignored files and directories the checks could read although HEAD's tree
# lacks them: a .gitignore pattern such as kubeconfig* or coverage.* can hide a source file, and an
# ignored dot file can configure a linter (.shellcheckrc, .golangci.yml, .github/actionlint.yaml).
# The build's outputs and caches (bin/, dist/, ui/dist/, node_modules/, test reports, *.test, *.out
# ...) and the state directories of the tools that work on the tree (.cache/, .agents/, .codex/,
# .claude/worktrees/, Claude Code's .claude/settings.local.json) do not count.
sdlc_ignored() {
	git ls-files --others --ignored --exclude-standard --directory --no-empty-directory |
		grep -Ev '^(\.(cache|agents|codex)/|\.claude/(worktrees/|settings\.local\.json$)|bin/|dist/|ui/dist/|test/e2e/artifacts/)|(^|/)(node_modules|coverage|test-results|playwright-report)/|(^|/)(\.DS_Store|\.env(\..*)?)$|\.(test|out|swp|tsbuildinfo)$' || true
}

# sdlc_clean succeeds when the working tree has no tracked changes, no untracked files that git
# does not ignore, no ignored files the checks could read (sdlc_ignored), and no tracked file whose
# changes git status does not show (marked assume-unchanged or skip-worktree: a lower-case or S tag
# in git ls-files -v): only then is HEAD's tree what the checks saw.
sdlc_clean() {
	# grep reads all of git's output (no -q), so pipefail cannot see git die of SIGPIPE.
	[[ -z $(git status --porcelain --untracked-files=normal) && -z $(sdlc_ignored) ]] &&
		! git ls-files -v | grep -E '^([a-z]|S) ' >/dev/null
}
