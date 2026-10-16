#!/usr/bin/env bash
# Build step of the e2e chart repository: packages the helm-guestbook seed chart twice and
# indexes the result, then builds patrol-cd/e2e-chart-repo:dev from it.
#
#   helm-guestbook-0.1.0.tgz  test/e2e/fixtures/seed/repos/helm-guestbook       (appVersion 1.0.0)
#   helm-guestbook-0.2.0.tgz  same + seed/updates/helm-guestbook overlay         (appVersion 1.1.0)
#   index.yaml                helm repo index (relative chart URLs)
#
# Usage: test/e2e/fixtures/chart-repo/build.sh [--package-only]
# Output directory: $PATROL_E2E_CHARTS_DIR (default .cache/kind/build/charts), recreated each run.
# The same packages are pushed as OCI artifacts by test/e2e/fixtures/oci/push.sh.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
root=$(cd "$here/../../../.." && pwd)

package_only=0
case "${1-}" in
--package-only) package_only=1 ;;
-h | --help)
	awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "${BASH_SOURCE[0]}"
	exit 0
	;;
"") ;;
*)
	echo "unexpected argument: $1" >&2
	exit 2
	;;
esac

command -v helm >/dev/null || {
	echo "helm not found on PATH" >&2
	exit 1
}
out=${PATROL_E2E_CHARTS_DIR:-$root/.cache/kind/build/charts}
src="$here/../seed/repos/helm-guestbook"
updates="$here/../seed/updates/helm-guestbook"

case "$out" in
"" | / | "$HOME" | "$root")
	echo "refusing to use output directory '$out'" >&2
	exit 1
	;;
esac
rm -rf "$out"
mkdir -p "$out"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

helm lint --strict "$src" >/dev/null
helm package "$src" --version 0.1.0 --app-version 1.0.0 --destination "$out" >/dev/null

mkdir -p "$work/helm-guestbook"
cp -R "$src/." "$work/helm-guestbook/"
cp -R "$updates/." "$work/helm-guestbook/"
helm lint --strict "$work/helm-guestbook" >/dev/null
helm package "$work/helm-guestbook" --version 0.2.0 --app-version 1.1.0 --destination "$out" >/dev/null

helm repo index "$out"
echo "packaged charts in $out:" >&2
(cd "$out" && ls -1) >&2

if [[ $package_only -eq 0 ]]; then
	command -v docker >/dev/null || {
		echo "docker not found on PATH" >&2
		exit 1
	}
	docker build -t patrol-cd/e2e-chart-repo:dev --build-context charts="$out" "$here"
fi
