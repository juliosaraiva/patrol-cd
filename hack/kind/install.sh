#!/usr/bin/env bash
# Installs Patrol CD from the Helm chart into the patrol-control kind cluster, then registers
# patrol-target as a destination and the e2e fixtures as repositories and Applications.
#
# Usage: hack/kind/install.sh [--namespace NS] [--skip-load] [--skip-register]
#
#   1. loads the local image patrol-cd/patrol-cd:dev into patrol-control (build it with
#      `make image`; --skip-load keeps the image already on the nodes);
#   2. helm upgrade --install release "patrol" of deploy/helm/patrol-cd into NS (default
#      "patrol") with hack/kind/values.yaml, and waits for every workload to be ready; when the
#      release already existed and an image was loaded, its workloads are restarted (the image
#      tag does not change, so helm alone would keep the old pods);
#   3. hack/kind/register-target.sh and test/e2e/fixtures/patrol/register.sh
#      --with-applications (--skip-register stops after the install).
#
# Requires hack/kind/up.sh and test/e2e/fixtures/deploy.sh to have run. Idempotent. The UI and
# API are at https://127.0.0.1:8443 (self-signed certificate); the initial admin password is in
# Secret patrol-initial-admin-secret (it is never printed here).
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=hack/kind/lib.sh
source "$here/lib.sh"

usage() { awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "${BASH_SOURCE[0]}"; }

ns=patrol
load=1
register=1
while [[ $# -gt 0 ]]; do
	case "$1" in
	--namespace)
		[[ $# -ge 2 ]] || patrol::die "--namespace needs a value"
		ns=$2
		shift 2
		;;
	--skip-load)
		load=0
		shift
		;;
	--skip-register)
		register=0
		shift
		;;
	-h | --help)
		usage
		exit 0
		;;
	*) patrol::die "unexpected argument: $1 (see --help)" ;;
	esac
done
[[ $ns =~ ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$ ]] || patrol::die "invalid namespace name: $ns"

patrol::require_tools docker kind kubectl helm
cluster=$PATROL_CONTROL_CLUSTER
patrol::cluster_exists "$cluster" || patrol::die "kind cluster $cluster does not exist (run hack/kind/up.sh)"
kc=$(patrol::kubeconfig "$cluster")
patrol::guard_kubeconfig "$kc" "kind-$cluster"

image=patrol-cd/patrol-cd:dev
if [[ $load -eq 1 ]]; then
	docker image inspect "$image" >/dev/null 2>&1 || patrol::die "image $image not found (run make image)"
	"$here/load-image.sh" --cluster "$cluster" "$image"
fi

# The image tag is always `dev`, so a rebuilt image changes nothing in the pod templates and helm
# would leave the old pods running: restart them when the release already existed.
existed=0
if patrol::helm "$cluster" -n "$ns" status patrol >/dev/null 2>&1; then existed=1; fi

patrol::log "helm upgrade --install patrol into $cluster/$ns (values: hack/kind/values.yaml)"
patrol::helm "$cluster" upgrade --install patrol "$PATROL_ROOT/deploy/helm/patrol-cd" \
	--namespace "$ns" --create-namespace \
	--values "$here/values.yaml" --wait --timeout "$(patrol::secs 360)s" >/dev/null

if [[ $load -eq 1 && $existed -eq 1 ]]; then
	patrol::log "restarting the Patrol CD workloads to pick up the loaded image"
	workloads=$(patrol::kubectl "$cluster" -n "$ns" get deployments,statefulsets \
		-l app.kubernetes.io/instance=patrol -o name)
	[[ -n $workloads ]] || patrol::die "no Patrol CD workloads found in $cluster/$ns"
	while IFS= read -r w; do
		patrol::kubectl "$cluster" -n "$ns" rollout restart "$w" >/dev/null
	done <<<"$workloads"
	while IFS= read -r w; do
		patrol::kubectl "$cluster" -n "$ns" rollout status "$w" --timeout="$(patrol::secs 300)s" >/dev/null
	done <<<"$workloads"
fi

if [[ $register -eq 1 ]]; then
	"$here/register-target.sh" --namespace "$ns"
	"$PATROL_ROOT/test/e2e/fixtures/patrol/register.sh" --namespace "$ns" --with-applications
	patrol::log "waiting for the repo-server to see the fixture git server's host key (the kubelet refreshes mounted ConfigMaps with a delay)"
	patrol::wait_fixture_host_key "$ns" "$(patrol::secs 240)" ||
		patrol::die "the repo-server does not see the fixture host key in /app/config/ssh/ssh_known_hosts after $(patrol::secs 240) s"
fi

cat <<EOF

Patrol CD is installed in $cluster/$ns.
  UI / API   https://127.0.0.1:8443   (self-signed certificate; user: admin)
  password   kubectl --kubeconfig $kc -n $ns get secret patrol-initial-admin-secret -o jsonpath='{.data.password}' | base64 -d
  Applications are synced manually: POST /api/v1/applications/NAME/sync, or the UI.
EOF
