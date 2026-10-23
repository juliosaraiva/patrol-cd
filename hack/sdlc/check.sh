#!/usr/bin/env bash
# Deterministic checks of the change on the current branch, run before every push
# (docs/guides/claude-code-sdlc.md). What runs depends on the files changed since the merge base
# with origin/main:
#
#   always                 make ci-static (gofmt, go mod, build, vet, golangci-lint, generated files,
#                          OpenAPI, parity, test rules, tier tests, SDLC script tests, shell,
#                          actionlint, gitleaks) and make release-lint-commits (commit subjects)
#   Go files, Go data      make ci-unit on the changed packages, a data file counting for the
#                          nearest package above it (-race, envtest, skip policy); go.mod or
#                          go.sum: every package
#   test/integration/      make ci-integration
#   deploy/, test/deploy/  make ci-deploy
#   hack/kind/, test/e2e/  make ci-scripts
#   ui/                    make ci-ui
#   Go, JS/TS, workflows   CodeQL: hack/sdlc/codeql.sh analyze for the languages that changed
#
# Documentation-only changes run the first row only. When every check passed on a clean tree, the
# result is recorded for HEAD's tree in $(git rev-parse --git-common-dir)/patrol-sdlc/checks/; a push
# needs that record (hack/sdlc/gate.sh). A dirty tree is checked but never recorded.
#
# Usage: hack/sdlc/check.sh [--no-codeql]
#   --no-codeql  skip CodeQL (faster iteration; nothing is recorded)
set -euo pipefail

# The whole script is one group, read before it runs: a run takes most of an hour, and bash reads
# a script as it goes, so an edit to this file during a run would otherwise run a mix of the two.
{
# Drop any inherited make flags: `-i`/`-k` would make failing recipes (gofmt, vet, the test suite)
# be ignored and let a broken run record a pass. MAKEFILES would read a makefile before ours, whose
# `:=` beats our `?=` (a tool replaced by a no-op).
unset MAKEFLAGS MFLAGS MAKELEVEL GNUMAKEFLAGS MAKEFILES
# A go.work in a parent directory, or GOWORK, would build and test against other modules than the
# tree's go.mod and go.sum, as CI (which has none) never does.
export GOWORK=off

cd "$(git rev-parse --show-toplevel)"
# shellcheck source=hack/sdlc/lib.sh
source hack/sdlc/lib.sh
# The tools `make tools` and hack/ci/install-tools.sh put in bin/ (kustomize, git-lfs, ...) come first,
# as on CI, where the setup action puts its tool directory on PATH.
export PATH="$PWD/bin:$PATH"

codeql=1
for arg in "$@"; do
	case $arg in
	--no-codeql) codeql=0 ;;
	*)
		echo "usage: hack/sdlc/check.sh [--no-codeql]" >&2
		exit 2
		;;
	esac
done

head=$(git rev-parse HEAD)
tree=$(git rev-parse 'HEAD^{tree}')
base=$(sdlc_base HEAD)
changed=()
while IFS= read -r f; do changed+=("$f"); done < <(sdlc_changed "$base" HEAD)
record=1
if ! sdlc_clean; then
	record=0
	echo "sdlc-check: the working tree has uncommitted changes; checking it, but recording nothing (commit first)"
fi
[[ $codeql -eq 1 ]] || record=0
if [[ -n ${PATROL_SDLC_BASE:-} ]]; then
	record=0 # the gate measures from origin/main; a check against another base proves nothing for it
	echo "sdlc-check: PATROL_SDLC_BASE is set; checking against it, but recording nothing"
fi
# A recorded pass must come from a canonical run: if the environment can weaken a check
# (sdlc_weakened), check but record nothing.
weakened=()
while IFS= read -r v; do weakened+=("$v"); done < <(sdlc_weakened Makefile hack/*.mk)
if [[ ${#weakened[@]} -gt 0 ]]; then
	record=0
	echo "sdlc-check: ${weakened[*]} set in the environment; checking, but recording nothing (the gate needs a canonical run)"
fi

echo "sdlc-check: HEAD $head (tree $tree), base $base, ${#changed[@]} changed files"

ran=()
step() {
	local name=$1
	shift
	echo
	echo "==> sdlc-check: $name"
	local start=$SECONDS
	if "$@"; then
		ran+=("$name")
		echo "<== sdlc-check: $name passed ($((SECONDS - start))s)"
	else
		echo "<== sdlc-check: $name FAILED ($((SECONDS - start))s)" >&2
		exit 1
	fi
}

any() { # any REGEX: succeeds when a changed file matches
	local f
	for f in ${changed[@]+"${changed[@]}"}; do
		[[ $f =~ $1 ]] && return 0
	done
	return 1
}

step ci-static make --no-print-directory ci-static
step commit-subjects make --no-print-directory release-lint-commits RELEASE_RANGE=refs/remotes/origin/main..HEAD

if printf '%s\n' ${changed[@]+"${changed[@]}"} | sdlc_docs_only; then
	echo
	echo "sdlc-check: documentation only; no tests, no CodeQL"
else
	# Go packages of the changed files: for each file, the nearest directory at or above it that holds
	# Go sources (testdata and embedded data such as internal/lua/customizations/*/*/health.lua belong
	# to the package above them); without the two suites that have their own targets, and only
	# packages go can load (hack/deps holds a single `//go:build tools` file).
	pkgs=()
	if any '^(go\.mod|go\.sum)$'; then
		# A dependency change touches every package; test them all except the two suites that have
		# their own targets (ci-integration, ci-deploy), which ci-unit's default also excludes.
		while IFS= read -r p; do
			[[ -n $p ]] && pkgs+=("$p")
		done < <(go list ./... | grep -v -e '/test/integration' -e '/test/deploy')
	else
		dirs=()
		seen=$'\n'
		for f in ${changed[@]+"${changed[@]}"}; do
			d=${f%%/testdata/*}
			[[ $d == "$f" ]] && d=$(dirname "$f")
			while [[ $d != . ]] && ! compgen -G "$d/*.go" >/dev/null; do
				d=$(dirname "$d")
			done
			[[ $d == . || $d == test/integration* || $d == test/deploy* ]] && continue
			[[ $seen == *$'\n'"$d"$'\n'* ]] && continue
			seen+="$d"$'\n'
			dirs+=("./$d")
		done
		if [[ ${#dirs[@]} -gt 0 ]]; then
			while IFS= read -r p; do
				[[ -n $p ]] && pkgs+=("$p")
			done < <(go list -e -f '{{if not .Error}}{{.ImportPath}}{{end}}' "${dirs[@]}")
		fi
	fi
	if [[ ${#pkgs[@]} -gt 0 ]]; then
		step "go tests (${#pkgs[@]} packages)" make --no-print-directory ci-unit CI_UNIT_PKGS="${pkgs[*]}"
	fi
	if any '^test/integration/'; then
		step integration make --no-print-directory ci-integration
	fi
	if any '^(deploy/|test/deploy/)'; then
		step deploy make --no-print-directory ci-deploy
	fi
	if any '^(hack/kind/|test/e2e/)'; then
		step kind-scripts make --no-print-directory ci-scripts
	fi
	if any '^ui/'; then
		step ui make --no-print-directory ci-ui
	fi
	if [[ $codeql -eq 1 ]]; then
		langs=()
		any '\.go$|^go\.(mod|sum)$' && langs+=(go)
		any '\.(ts|tsx|js|jsx|mjs|cjs)$|(^|/)package\.json$' && langs+=(javascript-typescript)
		any '^\.github/(workflows|actions)/' && langs+=(actions)
		# The gate's own inputs: every language, so a new pin, query configuration or baseline is tried.
		if any '^(hack/sdlc/(codeql\.sh|codeql-baseline\.txt|sarifgate/)|\.github/codeql/)'; then
			langs=(go javascript-typescript actions)
		fi
		if [[ ${#langs[@]} -gt 0 ]]; then
			step "codeql (${langs[*]})" hack/sdlc/codeql.sh analyze "${langs[@]}"
		else
			echo
			echo "sdlc-check: no Go, UI or workflow change; no CodeQL"
		fi
	else
		echo
		echo "sdlc-check: CodeQL skipped (--no-codeql)"
	fi
fi

echo
if [[ $record -eq 1 ]]; then
	# Nothing may have changed while the checks ran.
	if [[ $(git rev-parse 'HEAD^{tree}') != "$tree" ]] || ! sdlc_clean; then
		echo "sdlc-check: the tree changed while the checks ran; nothing recorded" >&2
		exit 1
	fi
	state=$(sdlc_state_dir)
	{
		echo "tree=$tree"
		echo "head=$head"
		echo "base=$base"
		echo "result=pass"
		printf 'checks=%s\n' "$(
			IFS=,
			echo "${ran[*]}"
		)"
		echo "time=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
	} >"$state/checks/$tree"
	echo "sdlc-check: all checks passed; recorded for tree $tree"
else
	echo "sdlc-check: all checks passed; not recorded (dirty tree or --no-codeql)"
fi
exit
}
