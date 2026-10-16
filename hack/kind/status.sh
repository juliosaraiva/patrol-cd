#!/usr/bin/env bash
# Shows the state of the kind environment: patrol-* clusters and their nodes, the registry
# container, and the kubeconfig files under .cache/kind. Always exits 0 unless a tool is missing.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=hack/kind/lib.sh
source "$here/lib.sh"

patrol::require_tools docker kind kubectl

echo "kind clusters (patrol-*):"
clusters=$(patrol::kind_clusters)
if [[ -z $clusters ]]; then
	echo "  none"
fi
for cluster in $PATROL_KIND_CLUSTERS; do
	kc=$(patrol::kubeconfig "$cluster")
	if ! patrol::cluster_exists "$cluster"; then
		echo "  $cluster: not created"
		continue
	fi
	echo "  $cluster: kubeconfig $kc (context kind-$cluster)"
	if [[ -f $kc ]]; then
		patrol::kubectl "$cluster" get nodes -o wide 2>&1 | sed 's/^/    /' || true
	else
		echo "    kubeconfig missing (run hack/kind/up.sh)"
	fi
done
while IFS= read -r other; do
	case " $PATROL_KIND_CLUSTERS " in
	*" $other "*) ;;
	*) [[ -n $other ]] && echo "  $other: extra patrol-* cluster (removed by hack/kind/down.sh)" ;;
	esac
done <<<"$clusters"

echo "registry:"
state=$(patrol::registry_state)
if [[ -z $state ]]; then
	echo "  $PATROL_REGISTRY_NAME: not created"
else
	nets=$(docker inspect -f '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}' "$PATROL_REGISTRY_NAME")
	echo "  $PATROL_REGISTRY_NAME: running=$state networks=[${nets% }] host=${PATROL_REGISTRY_HOST} internal=${PATROL_REGISTRY_INTERNAL}"
fi

internal=$(patrol::internal_kubeconfig "$PATROL_TARGET_CLUSTER")
if [[ -f $internal ]]; then
	ip_file="$PATROL_KIND_CACHE/${PATROL_TARGET_CLUSTER}-control-plane.ip"
	ip=""
	[[ -f $ip_file ]] && ip=$(cat "$ip_file")
	echo "patrol-target from patrol-control: ${PATROL_TARGET_INTERNAL_SERVER} (kind network IP ${ip:-unknown}; $internal)"
fi
