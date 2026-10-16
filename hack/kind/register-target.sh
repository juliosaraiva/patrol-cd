#!/usr/bin/env bash
# Registers patrol-target as a destination cluster of the Patrol CD instance in patrol-control.
#
# Usage: hack/kind/register-target.sh --namespace NS [--install-crds] [--token-duration 8760h]
#
#   1. patrol-target: ServiceAccount kube-system/patrol-manager bound to cluster-admin
#      (hack/kind/manifests/target-manager.yaml) and a bearer token for it (TokenRequest API).
#   2. patrol-control, namespace NS (created if missing): Secret cluster-patrol-target with keys
#      bearerToken and caData (patrol-target's cluster CA, PEM), then the Cluster object of
#      hack/kind/manifests/cluster-patrol-target.yaml (server
#      https://patrol-target-control-plane:6443).
#
# NS is the namespace Patrol CD runs in. The Patrol CRDs must be installed in patrol-control;
# --install-crds applies deploy/crds (server-side) when they are not installed yet. Secrets are applied
# server-side, so no last-applied annotation copies the token; nothing secret is printed.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=hack/kind/lib.sh
source "$here/lib.sh"

usage() { awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "${BASH_SOURCE[0]}"; }

ns=""
install_crds=0
duration=8760h
while [[ $# -gt 0 ]]; do
	case "$1" in
	--namespace)
		[[ $# -ge 2 ]] || patrol::die "--namespace needs a value"
		ns=$2
		shift 2
		;;
	--install-crds)
		install_crds=1
		shift
		;;
	--token-duration)
		[[ $# -ge 2 ]] || patrol::die "--token-duration needs a value"
		duration=$2
		shift 2
		;;
	-h | --help)
		usage
		exit 0
		;;
	*) patrol::die "unexpected argument: $1 (see --help)" ;;
	esac
done
[[ -n $ns ]] || patrol::die "--namespace is required (the namespace Patrol CD runs in)"
[[ $ns =~ ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$ ]] || patrol::die "invalid namespace name: $ns"

patrol::require_tools kubectl base64
control=$PATROL_CONTROL_CLUSTER
target=$PATROL_TARGET_CLUSTER
internal=$(patrol::internal_kubeconfig "$target")
patrol::guard_kubeconfig "$internal" "kind-$target"

apply() { patrol::kubectl "$control" apply --server-side --field-manager=patrol-kind "$@"; }

if patrol::kubectl "$control" get crd clusters.cd.patrol.example >/dev/null 2>&1; then
	# Already installed (by a previous run or by the Patrol install): leave it to its owner.
	[[ $install_crds -eq 0 ]] || patrol::log "Patrol CRDs already installed in $control; not reapplying"
elif [[ $install_crds -eq 1 ]]; then
	patrol::log "installing Patrol CRDs into $control"
	apply -f "$PATROL_ROOT/deploy/crds" >/dev/null
	patrol::wait_established "$control" clusters.cd.patrol.example repositories.cd.patrol.example \
		repositorycredentials.cd.patrol.example applications.cd.patrol.example
else
	patrol::die "CRD clusters.cd.patrol.example is not installed in $control (pass --install-crds)"
fi

patrol::log "creating ServiceAccount kube-system/patrol-manager in $target"
patrol::kubectl "$target" apply --server-side --field-manager=patrol-kind \
	-f "$PATROL_KIND_DIR/manifests/target-manager.yaml" >/dev/null

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
chmod 700 "$tmp"
patrol::kubectl "$target" -n kube-system create token patrol-manager --duration "$duration" |
	tr -d '\n' >"$tmp/bearerToken"
[[ -s $tmp/bearerToken ]] || patrol::die "TokenRequest for kube-system/patrol-manager returned no token"
kubectl config view --kubeconfig "$internal" --context "kind-$target" --raw --minify \
	-o jsonpath='{.clusters[0].cluster.certificate-authority-data}' | base64 -d >"$tmp/caData"
grep -q 'BEGIN CERTIFICATE' "$tmp/caData" || patrol::die "no CA certificate in $internal"

patrol::log "writing Secret $ns/cluster-patrol-target and Cluster $ns/patrol-target in $control"
patrol::kubectl "$control" create namespace "$ns" --dry-run=client -o yaml | apply -f - >/dev/null
patrol::kubectl "$control" -n "$ns" create secret generic cluster-patrol-target \
	--from-file=bearerToken="$tmp/bearerToken" --from-file=caData="$tmp/caData" \
	--dry-run=client -o yaml |
	patrol::kubectl "$control" label --local -f - app.kubernetes.io/part-of=patrol-cd-dev -o yaml |
	apply -f - >/dev/null
apply -n "$ns" -f "$PATROL_KIND_DIR/manifests/cluster-patrol-target.yaml" >/dev/null

patrol::log "registered: Cluster $ns/patrol-target -> $PATROL_TARGET_INTERNAL_SERVER (token valid for $duration)"
