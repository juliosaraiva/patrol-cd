#!/usr/bin/env bash
# Makes local docker images available to the patrol-* kind clusters.
#
# Usage: hack/kind/load-image.sh [--cluster patrol-control|patrol-target|all] [--registry] [IMAGE...]
#
#   (default)   kind load docker-image into the selected clusters (default: all = both).
#               Pods use the image name unchanged, with imagePullPolicy IfNotPresent or Never.
#   --registry  push to the patrol-registry instead, as localhost:5001/<IMAGE>. Pods in any
#               patrol-* cluster then use image localhost:5001/<IMAGE> (containerd on each node
#               maps it to the registry container). The temporary localhost:5001/... tag is
#               removed from the local docker image store after the push.
#
# Without IMAGE arguments, every local image matching patrol-cd/*:dev is used.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=hack/kind/lib.sh
source "$here/lib.sh"

usage() { awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "${BASH_SOURCE[0]}"; }

target=all
registry=0
images=()
while [[ $# -gt 0 ]]; do
	case "$1" in
	--cluster)
		[[ $# -ge 2 ]] || patrol::die "--cluster needs a value"
		target=$2
		shift 2
		;;
	--registry)
		registry=1
		shift
		;;
	-h | --help)
		usage
		exit 0
		;;
	-*) patrol::die "unknown flag: $1 (see --help)" ;;
	*)
		images+=("$1")
		shift
		;;
	esac
done

patrol::require_tools docker kind

case "$target" in
all) clusters=$PATROL_KIND_CLUSTERS ;;
patrol-*)
	patrol::guard_cluster "$target"
	clusters=$target
	;;
*) patrol::die "--cluster must be patrol-control, patrol-target or all (got '$target')" ;;
esac

if [[ ${#images[@]} -eq 0 ]]; then
	while IFS= read -r img; do
		[[ -n $img ]] && images+=("$img")
	done < <(docker image ls --filter 'reference=patrol-cd/*:dev' --format '{{.Repository}}:{{.Tag}}' | sort -u)
	[[ ${#images[@]} -gt 0 ]] || patrol::die "no local patrol-cd/*:dev images found; pass IMAGE arguments"
fi
for img in "${images[@]}"; do
	docker image inspect "$img" >/dev/null 2>&1 || patrol::die "image not found locally: $img"
done

if [[ $registry -eq 1 ]]; then
	[[ $(patrol::registry_state) == true ]] || patrol::die "registry $PATROL_REGISTRY_NAME is not running (run hack/kind/up.sh)"
	for img in "${images[@]}"; do
		ref="${PATROL_REGISTRY_HOST}/${img}"
		docker tag "$img" "$ref"
		patrol::log "pushing $ref"
		if ! docker push --quiet "$ref" >/dev/null; then
			docker rmi "$ref" >/dev/null 2>&1 || true
			patrol::die "push failed: $ref"
		fi
		docker rmi "$ref" >/dev/null
		printf '%s\n' "$ref"
	done
	exit 0
fi

arch=$(docker version -f '{{.Server.Arch}}')
for cluster in $clusters; do
	patrol::cluster_exists "$cluster" || patrol::die "kind cluster $cluster does not exist (run hack/kind/up.sh)"
	for img in "${images[@]}"; do
		patrol::log "loading $img into $cluster"
		if ! kind load docker-image "$img" --name "$cluster" >/dev/null; then
			# Docker's containerd image store can hand kind a multi-platform index whose other
			# platforms are not present locally; retry with a single-platform archive.
			patrol::log "kind load docker-image failed; retrying with a linux/$arch image archive"
			tmp=$(mktemp -d)
			docker save --platform "linux/$arch" -o "$tmp/image.tar" "$img"
			kind load image-archive "$tmp/image.tar" --name "$cluster" >/dev/null
			rm -rf "$tmp"
		fi
	done
done
