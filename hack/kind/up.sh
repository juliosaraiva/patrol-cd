#!/usr/bin/env bash
# Creates the disposable Patrol CD kind environment. Idempotent: existing pieces are reused,
# stopped node/registry containers are started, configuration is re-applied.
#
#   patrol-control   1 control-plane + 1 worker; 127.0.0.1:8443 -> NodePort 30443
#   patrol-target    1 control-plane node (workload cluster)
#   patrol-registry  registry container on 127.0.0.1:5001, attached to the "kind" network
#
# Outputs (never merged into ~/.kube/config):
#   .cache/kind/patrol-control.kubeconfig          host access, context kind-patrol-control
#   .cache/kind/patrol-target.kubeconfig           host access, context kind-patrol-target
#   .cache/kind/patrol-target-internal.kubeconfig  server https://patrol-target-control-plane:6443
#                                                  (for clients running inside patrol-control)
#   .cache/kind/patrol-target-control-plane.ip     its IP on the kind network (informational)
#
# Missing node and registry images are pulled first, with retries (patrol::docker_pull_retry).
#
# Environment: PATROL_KIND_NODE_IMAGE, PATROL_REGISTRY_IMAGE, PATROL_WAIT_SCALE (see lib.sh).
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=hack/kind/lib.sh
source "$here/lib.sh"

usage() { awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "${BASH_SOURCE[0]}"; }
case "${1-}" in
-h | --help)
	usage
	exit 0
	;;
"") ;;
*) patrol::die "unexpected argument: $1 (see --help)" ;;
esac

patrol::require_tools docker kind kubectl
docker info >/dev/null 2>&1 || patrol::die "the docker daemon is not reachable"
mkdir -p "$PATROL_KIND_CACHE"
chmod 700 "$PATROL_KIND_CACHE"

# pull_missing IMAGE: pulls IMAGE with retries unless it is already in the local image store, so
# that a transient registry error is retried and reported with the image name instead of failing
# `docker run` or `kind create cluster` (which would otherwise pull it themselves).
pull_missing() {
	docker image inspect "$1" >/dev/null 2>&1 && return 0
	patrol::log "pulling $1"
	patrol::docker_pull_retry "$1"
}

ensure_registry() {
	local state
	state=$(patrol::registry_state)
	if [[ -z $state ]]; then
		pull_missing "$PATROL_REGISTRY_IMAGE"
		patrol::log "creating registry container $PATROL_REGISTRY_NAME ($PATROL_REGISTRY_IMAGE) on 127.0.0.1:$PATROL_REGISTRY_HOST_PORT"
		docker run -d --restart=always --name "$PATROL_REGISTRY_NAME" --label "$PATROL_DOCKER_LABEL" \
			-p "127.0.0.1:${PATROL_REGISTRY_HOST_PORT}:5000" "$PATROL_REGISTRY_IMAGE" >/dev/null
		return
	fi
	patrol::registry_is_ours ||
		patrol::die "a container named $PATROL_REGISTRY_NAME exists but was not created by hack/kind (label $PATROL_DOCKER_LABEL missing); remove or rename it"
	if [[ $state != true ]]; then
		patrol::log "starting registry container $PATROL_REGISTRY_NAME"
		docker start "$PATROL_REGISTRY_NAME" >/dev/null
	else
		patrol::log "registry container $PATROL_REGISTRY_NAME is running"
	fi
}

connect_registry() {
	local nets
	nets=$(docker inspect -f '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}' "$PATROL_REGISTRY_NAME")
	case " $nets " in
	*" $PATROL_KIND_NETWORK "*) ;;
	*)
		patrol::log "connecting $PATROL_REGISTRY_NAME to docker network $PATROL_KIND_NETWORK"
		docker network connect "$PATROL_KIND_NETWORK" "$PATROL_REGISTRY_NAME"
		;;
	esac
}

write_kubeconfig() {
	local cluster=$1 kc tmp
	kc=$(patrol::kubeconfig "$cluster")
	tmp=$(mktemp "$PATROL_KIND_CACHE/.kubeconfig.XXXXXX")
	kind get kubeconfig --name "$cluster" >"$tmp"
	chmod 600 "$tmp"
	mv "$tmp" "$kc"
	patrol::guard_kubeconfig "$kc" "kind-$cluster"
}

start_stopped_nodes() {
	local cluster=$1 node
	for node in $(kind get nodes --name "$cluster"); do
		if [[ $(docker inspect -f '{{.State.Running}}' "$node") != true ]]; then
			patrol::log "starting stopped node container $node"
			docker start "$node" >/dev/null
		fi
	done
}

ensure_cluster() {
	local cluster=$1 kc
	patrol::guard_cluster "$cluster"
	kc=$(patrol::kubeconfig "$cluster")
	if patrol::cluster_exists "$cluster"; then
		patrol::log "cluster $cluster already exists"
		start_stopped_nodes "$cluster"
	else
		pull_missing "$PATROL_KIND_NODE_IMAGE"
		patrol::log "creating cluster $cluster with $PATROL_KIND_NODE_IMAGE"
		kind create cluster --name "$cluster" --config "$PATROL_KIND_DIR/$cluster.yaml" \
			--image "$PATROL_KIND_NODE_IMAGE" --kubeconfig "$kc" --wait "$(patrol::secs 180)s"
	fi
	write_kubeconfig "$cluster"
}

# Points containerd on every node at the registry container for both image names:
# localhost:5001/... (same reference as on the host) and patrol-registry:5000/... (plain HTTP).
configure_node_registry() {
	local cluster=$1 node
	for node in $(kind get nodes --name "$cluster"); do
		docker exec "$node" mkdir -p "/etc/containerd/certs.d/${PATROL_REGISTRY_HOST}" \
			"/etc/containerd/certs.d/${PATROL_REGISTRY_INTERNAL}"
		printf '[host."http://%s"]\n  capabilities = ["pull", "resolve"]\n' "$PATROL_REGISTRY_INTERNAL" |
			docker exec -i "$node" cp /dev/stdin "/etc/containerd/certs.d/${PATROL_REGISTRY_HOST}/hosts.toml"
		printf 'server = "http://%s"\n\n[host."http://%s"]\n  capabilities = ["pull", "resolve"]\n' \
			"$PATROL_REGISTRY_INTERNAL" "$PATROL_REGISTRY_INTERNAL" |
			docker exec -i "$node" cp /dev/stdin "/etc/containerd/certs.d/${PATROL_REGISTRY_INTERNAL}/hosts.toml"
	done
}

# KEP-1755 local registry discovery ConfigMap.
document_registry() {
	local cluster=$1
	patrol::kubectl "$cluster" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: local-registry-hosting
  namespace: kube-public
data:
  localRegistryHosting.v1: |
    host: "${PATROL_REGISTRY_HOST}"
    hostFromClusterNetwork: "${PATROL_REGISTRY_INTERNAL}"
    hostFromContainerRuntime: "${PATROL_REGISTRY_HOST}"
    help: "docs/guides/local-development.md"
EOF
}

wait_ready() {
	local cluster=$1
	patrol::log "waiting for $cluster nodes and CoreDNS"
	patrol::kubectl "$cluster" wait --for=condition=Ready nodes --all --timeout="$(patrol::secs 180)s" >/dev/null
	patrol::kubectl "$cluster" -n kube-system rollout status deployment/coredns --timeout="$(patrol::secs 180)s" >/dev/null
}

write_internal_kubeconfig() {
	local cluster=$1 kc tmp ip
	kc=$(patrol::internal_kubeconfig "$cluster")
	tmp=$(mktemp "$PATROL_KIND_CACHE/.kubeconfig.XXXXXX")
	kind get kubeconfig --name "$cluster" --internal >"$tmp"
	chmod 600 "$tmp"
	mv "$tmp" "$kc"
	patrol::guard_kubeconfig "$kc" "kind-$cluster"
	ip=$(docker inspect -f "{{with index .NetworkSettings.Networks \"${PATROL_KIND_NETWORK}\"}}{{.IPAddress}}{{end}}" "${cluster}-control-plane")
	printf '%s\n' "$ip" >"$PATROL_KIND_CACHE/${cluster}-control-plane.ip"
}

# Node-level check: the patrol-control control-plane container resolves the patrol-target API
# server by container name and verifies its certificate with the cluster CA. (Pod-level
# reachability is checked by test/e2e/fixtures/smoke.sh.)
check_cross_cluster() {
	local from="${PATROL_CONTROL_CLUSTER}-control-plane" ca
	ca=$(kubectl config view --kubeconfig "$(patrol::internal_kubeconfig "$PATROL_TARGET_CLUSTER")" \
		--context "kind-$PATROL_TARGET_CLUSTER" --raw --minify \
		-o jsonpath='{.clusters[0].cluster.certificate-authority-data}')
	printf '%s' "$ca" | base64 -d | docker exec -i "$from" cp /dev/stdin /tmp/patrol-target-ca.crt
	if docker exec "$from" curl -fsS --max-time 10 --cacert /tmp/patrol-target-ca.crt \
		"${PATROL_TARGET_INTERNAL_SERVER}/version" >/dev/null; then
		patrol::log "cross-cluster check passed: $from -> ${PATROL_TARGET_INTERNAL_SERVER} (TLS verified)"
	else
		patrol::die "cross-cluster check failed: $from cannot reach ${PATROL_TARGET_INTERNAL_SERVER}"
	fi
}

ensure_registry
for cluster in $PATROL_KIND_CLUSTERS; do
	ensure_cluster "$cluster"
done
connect_registry
for cluster in $PATROL_KIND_CLUSTERS; do
	configure_node_registry "$cluster"
	wait_ready "$cluster"
	document_registry "$cluster"
done
write_internal_kubeconfig "$PATROL_TARGET_CLUSTER"
check_cross_cluster

patrol::log "ready"
cat >&2 <<EOF

  patrol-control  kubeconfig $(patrol::kubeconfig "$PATROL_CONTROL_CLUSTER")  (context kind-$PATROL_CONTROL_CLUSTER)
  patrol-target   kubeconfig $(patrol::kubeconfig "$PATROL_TARGET_CLUSTER")  (context kind-$PATROL_TARGET_CLUSTER)
                  from patrol-control: ${PATROL_TARGET_INTERNAL_SERVER} ($(patrol::internal_kubeconfig "$PATROL_TARGET_CLUSTER"))
  registry        host ${PATROL_REGISTRY_HOST}, kind network ${PATROL_REGISTRY_INTERNAL}

  eval "\$(hack/kind/env.sh)"   # exports PATROL_CONTROL_KUBECONFIG, PATROL_TARGET_KUBECONFIG, ...
EOF
