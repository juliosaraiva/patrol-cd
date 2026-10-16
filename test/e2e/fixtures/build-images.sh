#!/usr/bin/env bash
# Builds the e2e fixture images from official base images (alpine, oras):
#
#   patrol-cd/e2e-git-server:dev   git-server/   (+ seed repositories and update overlays)
#   patrol-cd/e2e-chart-repo:dev   chart-repo/   (charts packaged by chart-repo/build.sh)
#   patrol-cd/e2e-toolbox:dev      toolbox/
#
# Usage: test/e2e/fixtures/build-images.sh [git-server|chart-repo|toolbox ...]   (default: all)
#
# Repositories seeded on the git server:
#   test/e2e/fixtures/seed/repos  (or $PATROL_E2E_SEED_DIR): guestbook, helm-guestbook,
#       kustomize-guestbook, helm-values, and all of them as the monorepo apps; second revision
#       overlays from test/e2e/fixtures/seed/updates. These are the e2e contract (smoke.sh,
#       patrol/applications.yaml) and are always seeded.
#   extra repositories, $PATROL_E2E_EXTRA_REPOS ("name=dir ..."); default, when present:
#       patrol-examples         = examples/  (example Applications use paths like apps/plain-yaml/...)
#       patrol-examples-values  = examples/apps/multi-source-helm-values/values-repo
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
root=$(cd "$here/../../.." && pwd)

log() { printf '[e2e-fixtures] %s\n' "$*" >&2; }

components=("$@")
if [[ ${#components[@]} -eq 0 ]]; then
	components=(git-server chart-repo toolbox)
fi
command -v docker >/dev/null || {
	log "docker not found on PATH"
	exit 1
}

# extra_repos: "name=dir" pairs of extra repositories to seed.
extra_repos() {
	if [[ -n ${PATROL_E2E_EXTRA_REPOS+set} ]]; then
		printf '%s\n' "$PATROL_E2E_EXTRA_REPOS"
		return
	fi
	local pairs=""
	if [[ -d $root/examples && -n $(ls -A "$root/examples") ]]; then
		pairs="patrol-examples=$root/examples"
	fi
	if [[ -d $root/examples/apps/multi-source-helm-values/values-repo ]]; then
		pairs="$pairs patrol-examples-values=$root/examples/apps/multi-source-helm-values/values-repo"
	fi
	printf '%s\n' "$pairs"
}

# stage_extra_repos DIR: copies the extra repositories into DIR/<name> (a docker build context
# cannot reference directories outside itself).
stage_extra_repos() {
	local out=$1 pair name src
	rm -rf "$out"
	mkdir -p "$out"
	for pair in $(extra_repos); do
		name=${pair%%=*}
		src=${pair#*=}
		[[ $name =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ && -d $src ]] || {
			log "invalid extra repository '$pair' (want name=existing-directory)"
			exit 2
		}
		mkdir -p "$out/$name"
		cp -R "$src/." "$out/$name/"
		log "extra repository $name.git from ${src#"$root"/}"
	done
}

for component in "${components[@]}"; do
	case "$component" in
	git-server)
		seed=${PATROL_E2E_SEED_DIR:-$here/seed/repos}
		extra="$root/.cache/kind/build/seed-extra"
		stage_extra_repos "$extra"
		log "building patrol-cd/e2e-git-server:dev (seed repositories from ${seed#"$root"/})"
		docker build -t patrol-cd/e2e-git-server:dev \
			--build-context seed="$seed" --build-context seed-updates="$here/seed/updates" \
			--build-context seed-extra="$extra" "$here/git-server"
		;;
	chart-repo)
		log "building patrol-cd/e2e-chart-repo:dev"
		"$here/chart-repo/build.sh"
		;;
	toolbox)
		log "building patrol-cd/e2e-toolbox:dev"
		docker build -t patrol-cd/e2e-toolbox:dev "$here/toolbox"
		;;
	-h | --help)
		awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "${BASH_SOURCE[0]}"
		exit 0
		;;
	*)
		log "unknown component: $component"
		exit 2
		;;
	esac
done
