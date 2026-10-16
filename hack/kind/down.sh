#!/usr/bin/env bash
# Deletes every kind cluster named patrol-* and the patrol-registry container (with its
# anonymous storage volume), plus the kubeconfig files hack/kind wrote under .cache/kind.
# Nothing else is touched: other kind clusters, the shared "kind" docker network and
# ~/.kube/config are left alone. A container named patrol-registry that does not carry the
# hack/kind label is reported and kept.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=hack/kind/lib.sh
source "$here/lib.sh"

case "${1-}" in
-h | --help)
	awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "${BASH_SOURCE[0]}"
	exit 0
	;;
"") ;;
*) patrol::die "unexpected argument: $1 (see --help)" ;;
esac

patrol::require_tools docker kind
mkdir -p "$PATROL_KIND_CACHE"

clusters=$(patrol::kind_clusters)
if [[ -z $clusters ]]; then
	patrol::log "no kind clusters named patrol-*"
else
	while IFS= read -r cluster; do
		patrol::guard_cluster "$cluster"
		kc=$(patrol::kubeconfig "$cluster")
		# kind removes the cluster's entry from the kubeconfig it is given; give it ours (refreshed,
		# so the file exists) rather than letting it default to $KUBECONFIG or ~/.kube/config.
		kind get kubeconfig --name "$cluster" >"$kc" 2>/dev/null || : >"$kc"
		chmod 600 "$kc"
		patrol::log "deleting kind cluster $cluster"
		kind delete cluster --name "$cluster" --kubeconfig "$kc"
		rm -f "$kc" "$(patrol::internal_kubeconfig "$cluster")" "$PATROL_KIND_CACHE/${cluster}-control-plane.ip"
	done <<<"$clusters"
fi

state=$(patrol::registry_state)
if [[ -z $state ]]; then
	patrol::log "no $PATROL_REGISTRY_NAME container"
elif patrol::registry_is_ours; then
	patrol::log "removing registry container $PATROL_REGISTRY_NAME"
	docker rm -f -v "$PATROL_REGISTRY_NAME" >/dev/null
else
	patrol::log "keeping container $PATROL_REGISTRY_NAME: it was not created by hack/kind (label $PATROL_DOCKER_LABEL missing)"
fi

# Verify.
left=$(patrol::kind_clusters)
[[ -z $left ]] || patrol::die "kind clusters still present: $left"
if [[ -n $(patrol::registry_state) ]] && patrol::registry_is_ours; then
	patrol::die "registry container $PATROL_REGISTRY_NAME still present"
fi
patrol::log "done: no patrol-* kind clusters and no hack/kind registry container remain"
