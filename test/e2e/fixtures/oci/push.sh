#!/usr/bin/env bash
# Pushes the e2e OCI artifacts into the patrol-registry container:
#
#   Helm chart (helm push from the host through 127.0.0.1:5001):
#     localhost:5001/patrol-e2e/charts/helm-guestbook:0.1.0, :0.2.0
#   Plain-manifest bundle (oras, run in patrol-cd/e2e-toolbox:dev on the kind network): the
#   guestbook seed directory as one application/vnd.oci.image.layer.v1.tar+gzip layer
#     patrol-registry:5000/patrol-e2e/manifests/guestbook:v1.0.0   seed/repos/guestbook
#     patrol-registry:5000/patrol-e2e/manifests/guestbook:v1.1.0   + seed/updates/guestbook
#
# The same registry is localhost:5001 from the host and patrol-registry:5000 from pods; both
# names refer to the same repositories. Pods pull over plain HTTP, e.g.
#   helm pull oci://patrol-registry:5000/patrol-e2e/charts/helm-guestbook --version 0.1.0 --plain-http
#
# Usage: test/e2e/fixtures/oci/push.sh   (requires hack/kind/up.sh, helm, and the toolbox image)
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
fixtures=$(cd "$here/.." && pwd)
# shellcheck source=hack/kind/lib.sh
source "$fixtures/../../../hack/kind/lib.sh"

case "${1-}" in
-h | --help)
	awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "${BASH_SOURCE[0]}"
	exit 0
	;;
"") ;;
*) patrol::die "unexpected argument: $1 (see --help)" ;;
esac

patrol::require_tools docker helm curl tar
[[ $(patrol::registry_state) == true ]] || patrol::die "registry $PATROL_REGISTRY_NAME is not running (run hack/kind/up.sh)"
curl -fsS --max-time 5 "http://${PATROL_REGISTRY_HOST}/v2/" >/dev/null ||
	patrol::die "registry API not reachable at http://${PATROL_REGISTRY_HOST}/v2/"
docker image inspect patrol-cd/e2e-toolbox:dev >/dev/null 2>&1 ||
	patrol::die "image patrol-cd/e2e-toolbox:dev not found (run test/e2e/fixtures/build-images.sh toolbox)"

charts=${PATROL_E2E_CHARTS_DIR:-$PATROL_ROOT/.cache/kind/build/charts}
if ! ls "$charts"/helm-guestbook-*.tgz >/dev/null 2>&1; then
	"$fixtures/chart-repo/build.sh" --package-only
fi
for tgz in "$charts"/helm-guestbook-*.tgz; do
	patrol::log "helm push $(basename "$tgz") -> oci://${PATROL_REGISTRY_HOST}/patrol-e2e/charts"
	helm push "$tgz" "oci://${PATROL_REGISTRY_HOST}/patrol-e2e/charts" --plain-http
done

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
for rev in v1.0.0 v1.1.0; do
	stage="$work/guestbook-$rev"
	mkdir -p "$stage"
	cp -R "$fixtures/seed/repos/guestbook/." "$stage/"
	if [[ $rev == v1.1.0 ]]; then
		cp -R "$fixtures/seed/updates/guestbook/." "$stage/"
	fi
	# COPYFILE_DISABLE keeps macOS tar from adding AppleDouble (._*) entries.
	COPYFILE_DISABLE=1 tar -C "$stage" -czf "$work/guestbook-$rev.tar.gz" .
	ref="${PATROL_REGISTRY_INTERNAL}/patrol-e2e/manifests/guestbook:$rev"
	patrol::log "oras push $ref"
	docker run --rm -i --network "$PATROL_KIND_NETWORK" --entrypoint sh patrol-cd/e2e-toolbox:dev -c '
		set -eu
		cd /tmp
		cat > bundle.tar.gz
		oras push --plain-http "$1" bundle.tar.gz:application/vnd.oci.image.layer.v1.tar+gzip
	' sh "$ref" <"$work/guestbook-$rev.tar.gz"
done
patrol::log "OCI artifacts pushed"
