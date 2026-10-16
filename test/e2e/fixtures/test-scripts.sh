#!/usr/bin/env bash
# Offline regression test for the kind tooling. Starts two envtest kube-apiservers (no Docker)
# whose kubeconfig contexts are named kind-patrol-control and kind-patrol-target, points
# PATROL_KIND_CACHE at them, and exercises:
#   - the hack/kind/lib.sh guards (cluster names, context names, non-local servers);
#   - hack/kind/register-target.sh (CRD install, ServiceAccount + TokenRequest in the target,
#     Secret + Cluster in the control plane; the stored token authenticates against the target
#     with TLS verified by the stored caData);
#   - test/e2e/fixtures/patrol/register.sh (credential copies, Repository/Application objects);
#   - hack/kind/env.sh.
# The envtest servers stand in for the kind clusters, so this is labelled "simulated" in
# docs/verification; the real environment is checked by test/e2e/fixtures/smoke.sh.
#
# Usage: test/e2e/fixtures/test-scripts.sh   (KUBEBUILDER_ASSETS defaults like validate.sh)
set -euo pipefail
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
SIM=$(mktemp -d)
mkdir -p "$SIM/cache"
os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in x86_64) arch=amd64 ;; aarch64) arch=arm64 ;; esac
export KUBEBUILDER_ASSETS=${KUBEBUILDER_ASSETS:-$ROOT/bin/envtest/k8s/1.36.0-$os-$arch}
# shellcheck source=test/e2e/fixtures/lib/envtest.sh
source "$ROOT/test/e2e/fixtures/lib/envtest.sh"
cleanup() {
	envtest_stop
	rm -rf "$SIM"
}
trap cleanup EXIT

envtest_start "$SIM/control" kind-patrol-control
cp "$ENVTEST_KUBECONFIG" "$SIM/cache/patrol-control.kubeconfig"
CONTROL_SERVER=$ENVTEST_SERVER
envtest_start "$SIM/target" kind-patrol-target
cp "$ENVTEST_KUBECONFIG" "$SIM/cache/patrol-target.kubeconfig"
TARGET_SERVER=$ENVTEST_SERVER
cp "$SIM/cache/patrol-target.kubeconfig" "$SIM/cache/patrol-target-internal.kubeconfig"
kubectl config --kubeconfig "$SIM/cache/patrol-target-internal.kubeconfig" set-cluster kind-patrol-target \
	--server https://patrol-target-control-plane:6443 >/dev/null
echo "control=$CONTROL_SERVER target=$TARGET_SERVER"
export PATROL_KIND_CACHE=$SIM/cache

pass=0
fail=0
expect() { # expect ok|fail DESCRIPTION -- CMD...
	local want=$1 desc=$2
	shift 3
	local got=ok
	"$@" >"$SIM/out" 2>&1 || got=fail
	if [[ $got == "$want" ]]; then
		pass=$((pass + 1))
		printf 'PASS  %-62s (%s) %s\n' "$desc" "$got" "$(tail -n 1 "$SIM/out")"
	else
		fail=$((fail + 1))
		printf 'FAIL  %-62s want %s got %s\n' "$desc" "$want" "$got"
		sed 's/^/      /' "$SIM/out"
	fi
}
lib() { bash -c "source '$ROOT/hack/kind/lib.sh'; $*"; }

echo "== guard functions"
expect ok "guard_context kind-patrol-control" -- lib patrol::guard_context kind-patrol-control
expect ok "guard_context kind-patrol-target" -- lib patrol::guard_context kind-patrol-target
expect fail "guard_context kind-eks-worker-1 (existing non-patrol context)" -- lib patrol::guard_context kind-eks-worker-1
expect fail "guard_context docker-desktop" -- lib patrol::guard_context docker-desktop
expect fail "guard_context kind-patrol- (empty suffix)" -- lib patrol::guard_context kind-patrol-
expect fail "guard_context 'kind-patrol-x;rm' (shell metachar)" -- lib patrol::guard_context "'kind-patrol-x;rm'"
expect fail "guard_context empty" -- lib patrol::guard_context "''"
expect ok "guard_cluster patrol-control" -- lib patrol::guard_cluster patrol-control
expect fail "guard_cluster manager" -- lib patrol::guard_cluster manager
expect fail "guard_cluster patrol (no suffix)" -- lib patrol::guard_cluster patrol
expect ok "patrol::kubectl patrol-control get ns (local envtest)" -- lib patrol::kubectl patrol-control get ns default -o name
expect fail "patrol::kubectl manager (non-patrol cluster)" -- lib patrol::kubectl manager get ns
# A kubeconfig with an allowed context name that points at a non-local server must be refused.
cp "$SIM/cache/patrol-control.kubeconfig" "$SIM/remote.kubeconfig"
kubectl config --kubeconfig "$SIM/remote.kubeconfig" set-cluster kind-patrol-control --server https://203.0.113.10:6443 >/dev/null
expect fail "guard_kubeconfig: kind-patrol-control -> https://203.0.113.10:6443" -- lib patrol::guard_kubeconfig "$SIM/remote.kubeconfig" kind-patrol-control
expect fail "guard_kubeconfig: context missing from file" -- lib patrol::guard_kubeconfig "$SIM/cache/patrol-control.kubeconfig" kind-patrol-target
expect ok "guard_kubeconfig: internal https://patrol-target-control-plane:6443" -- lib patrol::guard_kubeconfig "$SIM/cache/patrol-target-internal.kubeconfig" kind-patrol-target

echo "== register-target.sh on a control plane without CRDs"
expect fail "register-target.sh without CRDs and without --install-crds" -- "$ROOT/hack/kind/register-target.sh" --namespace patrol-system
expect ok "register-target.sh --install-crds (fresh)" -- "$ROOT/hack/kind/register-target.sh" --namespace patrol-system --install-crds

echo "== CEL rejection sanity check (CRDs enforce rules in this setup)"
# CRDs were installed by register-target.sh --install-crds above.
cat >"$SIM/bad-app.yaml" <<'EOF'
apiVersion: cd.patrol.example/v1alpha1
kind: Application
metadata: {name: bad, namespace: default}
spec:
  project: default
  source: {repoURL: http://git-server.patrol-e2e.svc.cluster.local/git/guestbook.git, path: .}
  sources: [{repoURL: http://git-server.patrol-e2e.svc.cluster.local/git/guestbook.git, path: .}]
  destination: {name: patrol-target, namespace: x}
EOF
expect fail "Application with source and sources is rejected" -- \
	kubectl --kubeconfig "$SIM/cache/patrol-control.kubeconfig" --context kind-patrol-control create --dry-run=server -f "$SIM/bad-app.yaml"

echo "== register-target.sh"
expect fail "register-target.sh without --namespace" -- "$ROOT/hack/kind/register-target.sh"
expect ok "register-target.sh --namespace patrol-system --install-crds" -- "$ROOT/hack/kind/register-target.sh" --namespace patrol-system --install-crds
expect ok "register-target.sh again (idempotent)" -- "$ROOT/hack/kind/register-target.sh" --namespace patrol-system
C() { kubectl --kubeconfig "$SIM/cache/patrol-control.kubeconfig" --context kind-patrol-control "$@"; }
T() { kubectl --kubeconfig "$SIM/cache/patrol-target.kubeconfig" --context kind-patrol-target "$@"; }
expect ok "Cluster patrol-system/patrol-target server" -- \
	bash -c "test \"\$(kubectl --kubeconfig '$SIM/cache/patrol-control.kubeconfig' --context kind-patrol-control -n patrol-system get clusters.cd.patrol.example patrol-target -o jsonpath='{.spec.server} {.spec.config.credentialsRef.name}')\" = 'https://patrol-target-control-plane:6443 cluster-patrol-target' && echo match"
C -n patrol-system get secret cluster-patrol-target -o go-template='{{index .data "bearerToken" | base64decode}}' >"$SIM/token"
C -n patrol-system get secret cluster-patrol-target -o go-template='{{index .data "caData" | base64decode}}' >"$SIM/ca.pem"
expect ok "Secret keys are exactly bearerToken,caData" -- \
	bash -c "test \"\$(kubectl --kubeconfig '$SIM/cache/patrol-control.kubeconfig' --context kind-patrol-control -n patrol-system get secret cluster-patrol-target -o go-template='{{range \$k, \$v := .data}}{{\$k}},{{end}}')\" = 'bearerToken,caData,' && echo match"
expect ok "Secret has no last-applied annotation" -- \
	bash -c "! kubectl --kubeconfig '$SIM/cache/patrol-control.kubeconfig' --context kind-patrol-control -n patrol-system get secret cluster-patrol-target -o yaml | grep -q last-applied && echo none"
expect ok "caData equals patrol-target's serving CA" -- cmp "$SIM/ca.pem" "$SIM/target/certs/apiserver.crt"
# Use the stored credentials against the target API server, verifying TLS with caData.
cat >"$SIM/patrol-manager.kubeconfig" <<EOF
apiVersion: v1
kind: Config
clusters: [{name: t, cluster: {server: "$TARGET_SERVER", certificate-authority: "$SIM/ca.pem"}}]
users: [{name: m, user: {tokenFile: "$SIM/token"}}]
contexts: [{name: m, context: {cluster: t, user: m}}]
current-context: m
EOF
expect ok "token authenticates as kube-system:patrol-manager (TLS verified)" -- \
	bash -c "test \"\$(kubectl --kubeconfig '$SIM/patrol-manager.kubeconfig' auth whoami -o jsonpath='{.status.userInfo.username}')\" = system:serviceaccount:kube-system:patrol-manager && echo match"
expect ok "token is cluster-admin on target" -- kubectl --kubeconfig "$SIM/patrol-manager.kubeconfig" auth can-i '*' '*' --all-namespaces

echo "== test/e2e/fixtures/patrol/register.sh"
C create namespace patrol-e2e >/dev/null
printf 'patrol-e2e' >"$SIM/u"
openssl rand -hex 24 | tr -d '\n' >"$SIM/p"
ssh-keygen -q -t ed25519 -N '' -C sim -f "$SIM/id"
C -n patrol-e2e create secret generic git-server-http-auth --from-file=username="$SIM/u" --from-file=password="$SIM/p" >/dev/null
C -n patrol-e2e create secret generic chart-repo-auth --from-file=username="$SIM/u" --from-file=password="$SIM/p" >/dev/null
C -n patrol-e2e create secret generic git-server-ssh-client --from-file=sshPrivateKey="$SIM/id" --from-file=known_hosts="$ROOT/test/e2e/fixtures/git-server/known_hosts" >/dev/null
expect fail "register.sh with invalid namespace" -- "$ROOT/test/e2e/fixtures/patrol/register.sh" --namespace Bad_NS
expect ok "register.sh --namespace patrol-system --with-applications" -- "$ROOT/test/e2e/fixtures/patrol/register.sh" --namespace patrol-system --with-applications
expect ok "register.sh again (idempotent)" -- "$ROOT/test/e2e/fixtures/patrol/register.sh" --namespace patrol-system --with-applications
expect ok "objects present" -- C -n patrol-system get repositories.cd.patrol.example,repositorycredentials.cd.patrol.example,applications.cd.patrol.example -o name
C -n patrol-system get repositories.cd.patrol.example,repositorycredentials.cd.patrol.example,applications.cd.patrol.example,clusters.cd.patrol.example -o name | sed 's/^/      /'
C -n patrol-system get secret e2e-git-http-creds -o go-template='{{index .data "password" | base64decode}}' >"$SIM/p2"
expect ok "e2e-git-http-creds password copied byte-for-byte" -- cmp "$SIM/p" "$SIM/p2"
C -n patrol-system get secret e2e-git-ssh-creds -o go-template='{{index .data "sshPrivateKey" | base64decode}}' >"$SIM/id2"
expect ok "e2e-git-ssh-creds sshPrivateKey copied byte-for-byte" -- cmp "$SIM/id" "$SIM/id2"

echo "== env.sh"
expect ok "env.sh output evaluates" -- bash -c "eval \"\$('$ROOT/hack/kind/env.sh' 2>/dev/null)\" && test \"\$PATROL_TARGET_INTERNAL_SERVER\" = https://patrol-target-control-plane:6443 && echo \"\$PATROL_CONTROL_KUBECONFIG\""

echo "test-scripts: $pass passed, $fail failed"
[[ $fail -eq 0 ]]
