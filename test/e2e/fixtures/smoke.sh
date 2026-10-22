#!/usr/bin/env bash
# Checks the deployed kind environment and e2e fixtures from inside the clusters. Every check
# except registry-image runs in the toolbox pod (patrol-control, namespace patrol-e2e):
#
#   cross-cluster   kubectl against https://patrol-target-control-plane:6443 (DNS from a pod,
#                   TLS verified with the cluster CA, authenticated) using patrol-target-kubeconfig
#   git-http-auth   clone/ls-remote/push over /git with basic auth; anonymous access is refused
#   git-http-public anonymous clone over /public; anonymous push is refused
#   git-ssh         clone over SSH with the client key and strict host key checking
#   helm-repo       helm repo add + helm pull (anonymous and /private with basic auth; 401 without)
#   oci             helm pull of the OCI chart and oras pull of the manifest bundle (patrol-registry:5000)
#   registry-image  (--with-registry-image) pushes patrol-cd/e2e-toolbox:dev to localhost:5001 and
#                   runs it as localhost:5001/patrol-cd/e2e-toolbox:dev in both clusters
#
# Usage: test/e2e/fixtures/smoke.sh [--with-registry-image]
# Exit status is non-zero when any check fails. Credentials stay inside the pod.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=hack/kind/lib.sh
source "$here/../../../hack/kind/lib.sh"

with_registry_image=0
case "${1-}" in
--with-registry-image) with_registry_image=1 ;;
-h | --help)
	awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "${BASH_SOURCE[0]}"
	exit 0
	;;
"") ;;
*) patrol::die "unexpected argument: $1 (see --help)" ;;
esac

patrol::require_tools kubectl kind docker
cluster=$PATROL_CONTROL_CLUSTER
ns=patrol-e2e
kn() { patrol::kubectl "$cluster" -n "$ns" "$@"; }
kn wait --for=condition=Ready pod/toolbox --timeout="$(patrol::secs 60)s" >/dev/null

passed=0
failed=0
failed_names=""
# check NAME: runs the shell script read from stdin in the toolbox pod.
check() {
	local name=$1 script out
	script=$(cat)
	if out=$(printf '%s\n' "$script" | kn exec -i toolbox -- sh -s 2>&1); then
		passed=$((passed + 1))
		printf 'PASS  %-16s %s\n' "$name" "$(printf '%s' "$out" | tail -n 1)"
	else
		failed=$((failed + 1))
		failed_names="$failed_names $name"
		printf 'FAIL  %s\n%s\n' "$name" "$(printf '%s' "$out" | sed 's/^/      /')"
	fi
}

check cross-cluster <<'EOF'
set -eu
kc=/etc/patrol-e2e/patrol-target/kubeconfig
test -r "$kc" || { echo "missing $kc (Secret patrol-target-kubeconfig)"; exit 1; }
getent hosts patrol-target-control-plane
nodes=$(kubectl --kubeconfig "$kc" get nodes -o name)
echo "$nodes"
test "$nodes" = node/patrol-target-control-plane
kubectl --kubeconfig "$kc" get --raw /version | grep gitVersion >/dev/null
echo "patrol-target API reachable from a patrol-control pod: $nodes"
EOF

check git-http-auth <<'EOF'
set -eu
export GIT_TERMINAL_PROMPT=0 GIT_CONFIG_GLOBAL=/dev/null
base=http://git-server.patrol-e2e.svc.cluster.local/git
cred='!f() { echo "username=$GIT_HTTP_USERNAME"; echo "password=$GIT_HTTP_PASSWORD"; }; f'
g() { git -c credential.helper= -c "credential.helper=$cred" "$@"; }
cd "$(mktemp -d)"
if git ls-remote "$base/guestbook.git" >/dev/null 2>&1; then echo "anonymous access to /git was allowed"; exit 1; fi
g ls-remote "$base/guestbook.git"
g ls-remote "$base/guestbook.git" | grep 'refs/heads/feature/e2e' >/dev/null
g ls-remote "$base/guestbook.git" | grep 'refs/tags/v1.0.0^{}' >/dev/null
g clone -q --branch v1.0.0 "$base/guestbook.git" gb
test -f gb/guestbook-ui-deployment.yaml
grep -q 'replicas: 1' gb/guestbook-ui-deployment.yaml
g -C gb checkout -q -b "smoke-$$"
echo smoke > gb/smoke.txt
g -C gb add smoke.txt
g -C gb -c user.name=smoke -c user.email=smoke@patrol.example commit -q -m smoke
g -C gb push -q origin "smoke-$$"
g -C gb push -q origin --delete "smoke-$$"
g clone -q "$base/apps.git" apps
test -d apps/kustomize-guestbook/overlays/staging
echo "clone, ls-remote and push with basic auth OK; anonymous refused"
EOF

check git-http-public <<'EOF'
set -eu
export GIT_TERMINAL_PROMPT=0 GIT_CONFIG_GLOBAL=/dev/null
base=http://git-server.patrol-e2e.svc.cluster.local/public
cd "$(mktemp -d)"
git clone -q "$base/helm-guestbook.git" hg
test -f hg/Chart.yaml
git -C hg checkout -q -b "smoke-$$"
if git -C hg push -q origin "smoke-$$" 2>/dev/null; then echo "anonymous push was allowed"; exit 1; fi
echo "anonymous clone OK; anonymous push refused"
EOF

check git-ssh <<'EOF'
set -eu
export GIT_CONFIG_GLOBAL=/dev/null
d=/etc/patrol-e2e/git-ssh
test -r "$d/sshPrivateKey" || { echo "missing $d (Secret git-server-ssh-client)"; exit 1; }
install -m 0600 "$d/sshPrivateKey" /tmp/id_git
export GIT_SSH_COMMAND="ssh -i /tmp/id_git -o IdentitiesOnly=yes -o UserKnownHostsFile=$d/known_hosts -o StrictHostKeyChecking=yes -o BatchMode=yes"
cd "$(mktemp -d)"
git clone -q ssh://git@git-server.patrol-e2e.svc.cluster.local/git/kustomize-guestbook.git ks
test -f ks/overlays/staging/kustomization.yaml
git -C ks log --format='%H %s' -1
echo "clone over SSH with strict host key checking OK"
EOF

check helm-repo <<'EOF'
set -eu
export HOME=$(mktemp -d)
# Helm 3.19 (the toolbox's Alpine package) does not create the --destination directory.
mkdir -p "$HOME/charts"
base=http://chart-repo.patrol-e2e.svc.cluster.local
helm repo add e2e "$base" >/dev/null
helm pull e2e/helm-guestbook --version 0.1.0 -d "$HOME/charts"
code=$(curl -s -o /dev/null -w '%{http_code}' "$base/private/index.yaml")
test "$code" = 401 || { echo "expected 401 from /private without credentials, got $code"; exit 1; }
printf '%s' "$CHART_REPO_PASSWORD" | helm repo add e2e-private "$base/private" --username "$CHART_REPO_USERNAME" --password-stdin >/dev/null
helm pull e2e-private/helm-guestbook --version 0.2.0 -d "$HOME/charts"
ls "$HOME/charts"
echo "helm repo add/pull OK (anonymous and basic auth)"
EOF

check oci <<'EOF'
set -eu
export HOME=$(mktemp -d)
cd "$HOME"
mkdir -p charts
helm pull oci://patrol-registry:5000/patrol-e2e/charts/helm-guestbook --version 0.1.0 --plain-http -d charts
test -f charts/helm-guestbook-0.1.0.tgz
oras pull --plain-http patrol-registry:5000/patrol-e2e/manifests/guestbook:v1.0.0 -o bundle >/dev/null
tar -tzf bundle/bundle.tar.gz | grep guestbook-ui-deployment.yaml >/dev/null
echo "OCI chart and manifest bundle pulled from patrol-registry:5000"
EOF

if [[ $with_registry_image -eq 1 ]]; then
	ref=$("$PATROL_KIND_DIR/load-image.sh" --registry patrol-cd/e2e-toolbox:dev)
	for c in $PATROL_KIND_CLUSTERS; do
		patrol::kubectl "$c" -n default delete pod registry-pull-check --ignore-not-found --wait=true >/dev/null
		patrol::kubectl "$c" -n default run registry-pull-check --image="$ref" --image-pull-policy=Always \
			--restart=Never --command -- true >/dev/null
		if patrol::kubectl "$c" -n default wait --for=jsonpath='{.status.phase}'=Succeeded \
			pod/registry-pull-check --timeout="$(patrol::secs 120)s" >/dev/null 2>&1; then
			passed=$((passed + 1))
			printf 'PASS  %-16s %s pulled and ran %s\n' registry-image "$c" "$ref"
		else
			failed=$((failed + 1))
			failed_names="$failed_names registry-image($c)"
			printf 'FAIL  registry-image (%s)\n' "$c"
			patrol::kubectl "$c" -n default describe pod registry-pull-check | tail -n 15 | sed 's/^/      /'
		fi
		patrol::kubectl "$c" -n default delete pod registry-pull-check --ignore-not-found >/dev/null
	done
fi

echo "smoke: $passed passed, $failed failed${failed_names:+ (${failed_names# })}"
[[ $failed -eq 0 ]]
