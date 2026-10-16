# shellcheck shell=bash
# Shared helpers for the disposable Patrol CD kind environment. Sourced by hack/kind/*.sh and
# test/e2e/fixtures/*.sh; not executable on its own.
#
# Safety rules enforced here:
#   - kind clusters are only ever addressed by name and the name must match patrol-*;
#   - kubectl is only ever run with an explicit --kubeconfig (a file under .cache/kind) and an
#     explicit --context, and the context must match kind-patrol-* and point at a local kind API
#     endpoint (127.0.0.1/localhost or a patrol-*-control-plane container on the kind network);
#   - kind is always given --kubeconfig, so ~/.kube/config is never read or modified.
#
# Compatible with bash 3.2 (macOS /bin/bash).
#
# The variables below are used by the scripts that source this file.
# shellcheck disable=SC2034

PATROL_ROOT=${PATROL_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}
PATROL_KIND_DIR="$PATROL_ROOT/hack/kind"
PATROL_KIND_CACHE=${PATROL_KIND_CACHE:-$PATROL_ROOT/.cache/kind}

PATROL_CONTROL_CLUSTER=patrol-control
PATROL_TARGET_CLUSTER=patrol-target
PATROL_KIND_CLUSTERS="$PATROL_CONTROL_CLUSTER $PATROL_TARGET_CLUSTER"

# Kubernetes 1.36 node image (same minor as envtest and kubectl in this repo); digest from the
# kind v0.33.0 release notes. Override with PATROL_KIND_NODE_IMAGE.
PATROL_KIND_NODE_IMAGE=${PATROL_KIND_NODE_IMAGE:-kindest/node:v1.36.4@sha256:099e049362a1526b2db71494e1947aae99bd16290d7c895f2b7ea312e3cbfaed}

# kind's shared docker network. Every kind node and the registry are attached to it, which is
# what lets patrol-control reach https://patrol-target-control-plane:6443.
PATROL_KIND_NETWORK=kind

PATROL_REGISTRY_NAME=patrol-registry
PATROL_REGISTRY_IMAGE=${PATROL_REGISTRY_IMAGE:-registry:3.0.0}
PATROL_REGISTRY_HOST_PORT=5001
# Address of the registry as seen from the host (and, through containerd hosts.toml, from kind
# nodes pulling images) and from pods/containers on the kind network.
PATROL_REGISTRY_HOST="localhost:${PATROL_REGISTRY_HOST_PORT}"
PATROL_REGISTRY_INTERNAL="${PATROL_REGISTRY_NAME}:5000"
# Docker label marking containers created by these scripts; down.sh removes only labelled ones.
PATROL_DOCKER_LABEL_KEY=cd.patrol.example.kind-role
PATROL_DOCKER_LABEL="${PATROL_DOCKER_LABEL_KEY}=registry"

# Internal API endpoint of patrol-target for clients running inside patrol-control.
PATROL_TARGET_INTERNAL_SERVER="https://${PATROL_TARGET_CLUSTER}-control-plane:6443"

patrol::log() { printf '[patrol-kind] %s\n' "$*" >&2; }
patrol::die() {
	printf '[patrol-kind] error: %s\n' "$*" >&2
	exit 1
}

patrol::require_tools() {
	local t
	for t in "$@"; do
		command -v "$t" >/dev/null 2>&1 || patrol::die "required tool not found on PATH: $t"
	done
}

# patrol::guard_cluster NAME: refuses any kind cluster name other than patrol-<name>.
patrol::guard_cluster() {
	local name=${1-}
	if [[ ! $name =~ ^patrol-[a-z0-9]([a-z0-9-]*[a-z0-9])?$ ]]; then
		patrol::die "refusing to operate on kind cluster '${name}': only clusters named patrol-* are allowed"
	fi
}

# patrol::guard_context CONTEXT: refuses any kube context other than kind-patrol-<name>.
patrol::guard_context() {
	local ctx=${1-}
	if [[ ! $ctx =~ ^kind-patrol-[a-z0-9]([a-z0-9-]*[a-z0-9])?$ ]]; then
		patrol::die "refusing to operate on kube context '${ctx}': only contexts named kind-patrol-* are allowed"
	fi
}

# patrol::guard_kubeconfig FILE CONTEXT: CONTEXT must pass guard_context, exist in FILE and point
# at a local kind API server.
patrol::guard_kubeconfig() {
	local kc=$1 ctx=$2 server
	patrol::guard_context "$ctx"
	[[ -f $kc ]] || patrol::die "kubeconfig $kc not found (run hack/kind/up.sh)"
	server=$(kubectl config view --kubeconfig "$kc" --context "$ctx" --minify \
		-o jsonpath='{.clusters[0].cluster.server}' 2>/dev/null) ||
		patrol::die "context $ctx not found in $kc"
	case "$server" in
	https://127.0.0.1:* | https://localhost:* | https://\[::1\]:* | https://patrol-*-control-plane:6443) ;;
	*) patrol::die "refusing: context $ctx in $kc points at '${server}', which is not a local kind API server" ;;
	esac
}

# patrol::kubeconfig CLUSTER: path of the host-side kubeconfig file of a patrol-* cluster.
patrol::kubeconfig() {
	patrol::guard_cluster "$1"
	printf '%s/%s.kubeconfig\n' "$PATROL_KIND_CACHE" "$1"
}

# patrol::internal_kubeconfig CLUSTER: kubeconfig whose server is the cluster's control-plane
# container on the kind network (for clients running in another kind cluster).
patrol::internal_kubeconfig() {
	patrol::guard_cluster "$1"
	printf '%s/%s-internal.kubeconfig\n' "$PATROL_KIND_CACHE" "$1"
}

# patrol::kubectl CLUSTER ARGS...: kubectl pinned to the cluster's kubeconfig file and context.
patrol::kubectl() {
	local cluster=$1 kc
	shift
	patrol::guard_cluster "$cluster"
	kc=$(patrol::kubeconfig "$cluster")
	patrol::guard_kubeconfig "$kc" "kind-$cluster"
	kubectl --kubeconfig "$kc" --context "kind-$cluster" "$@"
}

# patrol::wait_established CLUSTER CRD...: waits up to 60s for each CRD to be Established.
# (`kubectl wait` fails at once, instead of waiting, while a new CRD has no status conditions.)
patrol::wait_established() {
	local cluster=$1 crd _
	shift
	for crd in "$@"; do
		for _ in $(seq 1 30); do
			if patrol::kubectl "$cluster" wait --for=condition=Established --timeout=2s "crd/$crd" >/dev/null 2>&1; then
				continue 2
			fi
			sleep 1
		done
		patrol::die "CRD $crd did not become Established in $cluster"
	done
}

# patrol::kind_clusters: names of existing kind clusters that match patrol-*, one per line.
patrol::kind_clusters() {
	local c
	kind get clusters 2>/dev/null | while IFS= read -r c; do
		if [[ $c =~ ^patrol-[a-z0-9]([a-z0-9-]*[a-z0-9])?$ ]]; then
			printf '%s\n' "$c"
		fi
	done
}

patrol::cluster_exists() {
	patrol::guard_cluster "$1"
	patrol::kind_clusters | grep -qx -- "$1"
}

# patrol::registry_state: "true"/"false" (running) or empty when the container does not exist.
patrol::registry_state() {
	docker inspect -f '{{.State.Running}}' "$PATROL_REGISTRY_NAME" 2>/dev/null || true
}

# patrol::registry_is_ours: the patrol-registry container carries the hack/kind label.
patrol::registry_is_ours() {
	local v
	v=$(docker inspect -f "{{index .Config.Labels \"${PATROL_DOCKER_LABEL_KEY}\"}}" "$PATROL_REGISTRY_NAME" 2>/dev/null) || return 1
	[[ $v == registry ]]
}
