#!/usr/bin/env bash
# Deploys the e2e fixtures into namespace patrol-e2e of the patrol-control kind cluster:
#
#   1. builds the fixture images (build-images.sh) and loads them into patrol-control;
#   2. creates the credentials Secrets if missing (random passwords, a fresh SSH client key);
#      their values are never printed and never written outside the cluster:
#        git-server-http-auth           username, password      (git smart HTTP basic auth)
#        git-server-ssh-authorized-keys authorized_keys         (git server side)
#        git-server-ssh-client          sshPrivateKey, known_hosts (client side, for tests)
#        chart-repo-auth                username, password      (chart repo /private)
#        patrol-target-kubeconfig       kubeconfig              (server https://patrol-target-control-plane:6443)
#   3. deploys the git server, (re)runs the git-seed Job, deploys the chart repository and the
#      toolbox pod;
#   4. pushes the OCI artifacts into patrol-registry (oci/push.sh).
#
# Usage: test/e2e/fixtures/deploy.sh [--skip-build] [--skip-oci]
# Requires hack/kind/up.sh to have run. Idempotent; re-running resets the seeded repositories.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=hack/kind/lib.sh
source "$here/../../../hack/kind/lib.sh"

skip_build=0
skip_oci=0
while [[ $# -gt 0 ]]; do
	case "$1" in
	--skip-build) skip_build=1 ;;
	--skip-oci) skip_oci=1 ;;
	-h | --help)
		awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "${BASH_SOURCE[0]}"
		exit 0
		;;
	*) patrol::die "unexpected argument: $1 (see --help)" ;;
	esac
	shift
done

patrol::require_tools docker kind kubectl openssl ssh-keygen
cluster=$PATROL_CONTROL_CLUSTER
ns=patrol-e2e
k() { patrol::kubectl "$cluster" "$@"; }
kn() { patrol::kubectl "$cluster" -n "$ns" "$@"; }

patrol::cluster_exists "$cluster" || patrol::die "kind cluster $cluster does not exist (run hack/kind/up.sh)"

if [[ $skip_build -eq 0 ]]; then
	"$here/build-images.sh"
fi
"$PATROL_KIND_DIR/load-image.sh" --cluster "$cluster" \
	patrol-cd/e2e-git-server:dev patrol-cd/e2e-chart-repo:dev patrol-cd/e2e-toolbox:dev

k apply -f "$here/namespace.yaml" >/dev/null

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
chmod 700 "$tmp"

has_secret() { kn get secret "$1" >/dev/null 2>&1; }

# create_secret NAME KEY=FILE...: creates a generic Secret from files (server-side apply, so no
# last-applied annotation duplicates the data).
create_secret() {
	local name=$1 arg
	shift
	local from=()
	for arg in "$@"; do from+=("--from-file=$arg"); done
	kn create secret generic "$name" "${from[@]}" --dry-run=client -o yaml |
		patrol::kubectl "$cluster" label --local -f - app.kubernetes.io/part-of=patrol-e2e-fixtures -o yaml |
		kn apply --server-side --field-manager=patrol-e2e -f - >/dev/null
	patrol::log "created Secret $ns/$name"
}

random_password() { openssl rand -hex 24 | tr -d '\n' >"$1"; }

if ! has_secret git-server-http-auth; then
	printf 'patrol-e2e' >"$tmp/git-username"
	random_password "$tmp/git-password"
	create_secret git-server-http-auth username="$tmp/git-username" password="$tmp/git-password"
fi
if ! has_secret chart-repo-auth; then
	printf 'patrol-e2e' >"$tmp/chart-username"
	random_password "$tmp/chart-password"
	create_secret chart-repo-auth username="$tmp/chart-username" password="$tmp/chart-password"
fi
if ! has_secret git-server-ssh-client || ! has_secret git-server-ssh-authorized-keys; then
	kn delete secret git-server-ssh-client git-server-ssh-authorized-keys --ignore-not-found >/dev/null
	ssh-keygen -q -t ed25519 -N '' -C patrol-e2e-client -f "$tmp/id_ed25519"
	create_secret git-server-ssh-client sshPrivateKey="$tmp/id_ed25519" known_hosts="$here/git-server/known_hosts"
	create_secret git-server-ssh-authorized-keys authorized_keys="$tmp/id_ed25519.pub"
	# sshd reads authorized_keys only at start: restart the server if it is already running.
	if kn get deployment git-server >/dev/null 2>&1; then
		kn rollout restart deployment/git-server >/dev/null
	fi
fi
internal=$(patrol::internal_kubeconfig "$PATROL_TARGET_CLUSTER")
if [[ -f $internal ]]; then
	patrol::guard_kubeconfig "$internal" "kind-$PATROL_TARGET_CLUSTER"
	cp "$internal" "$tmp/kubeconfig"
	create_secret patrol-target-kubeconfig kubeconfig="$tmp/kubeconfig"
else
	patrol::log "note: $internal not found; Secret patrol-target-kubeconfig not created"
fi
rm -rf "${tmp:?}"/*

patrol::log "deploying git server"
k apply -f "$here/git-server/manifests/" >/dev/null
kn rollout status deployment/git-server --timeout=180s

patrol::log "seeding repositories"
kn delete job git-seed --ignore-not-found --wait=true >/dev/null
k apply -f "$here/git-server/seed-job.yaml" >/dev/null
if ! kn wait --for=condition=Complete job/git-seed --timeout=300s >/dev/null; then
	kn logs job/git-seed --all-containers >&2 || true
	patrol::die "git-seed Job did not complete"
fi
kn logs job/git-seed >&2

patrol::log "deploying chart repository and toolbox"
k apply -f "$here/chart-repo/manifests/" >/dev/null
kn rollout status deployment/chart-repo --timeout=120s
# Pods are immutable: replace the toolbox so it picks up new Secrets and images.
kn delete pod toolbox --ignore-not-found --wait=true >/dev/null
k apply -f "$here/toolbox/pod.yaml" >/dev/null
kn wait --for=condition=Ready pod/toolbox --timeout=120s >/dev/null

if [[ $skip_oci -eq 0 ]]; then
	"$here/oci/push.sh"
fi

cat >&2 <<EOF

e2e fixtures ready in $cluster, namespace $ns:
  git (basic auth)   http://git-server.$ns.svc.cluster.local/git/<repo>.git
  git (anonymous)    http://git-server.$ns.svc.cluster.local/public/<repo>.git
  git (ssh)          ssh://git@git-server.$ns.svc.cluster.local/git/<repo>.git
  helm repo          http://chart-repo.$ns.svc.cluster.local  (and /private with basic auth)
  OCI (pods)         oci://${PATROL_REGISTRY_INTERNAL}/patrol-e2e/charts/helm-guestbook
                     oci://${PATROL_REGISTRY_INTERNAL}/patrol-e2e/manifests/guestbook
  smoke test         test/e2e/fixtures/smoke.sh
EOF
