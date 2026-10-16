#!/usr/bin/env bash
# Prints shell exports describing the kind environment. Usage:
#
#   eval "$(hack/kind/env.sh)"
#   kubectl --kubeconfig "$PATROL_CONTROL_KUBECONFIG" --context "$PATROL_CONTROL_CONTEXT" get nodes
#
# KUBECONFIG is deliberately not exported: pass --kubeconfig/--context explicitly.
# Missing kubeconfig files (clusters not created yet) are reported on stderr only.
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

control_kc=$(patrol::kubeconfig "$PATROL_CONTROL_CLUSTER")
target_kc=$(patrol::kubeconfig "$PATROL_TARGET_CLUSTER")
target_internal_kc=$(patrol::internal_kubeconfig "$PATROL_TARGET_CLUSTER")

emit() { printf 'export %s=%q\n' "$1" "$2"; }
emit PATROL_CONTROL_KUBECONFIG "$control_kc"
emit PATROL_CONTROL_CONTEXT "kind-$PATROL_CONTROL_CLUSTER"
emit PATROL_TARGET_KUBECONFIG "$target_kc"
emit PATROL_TARGET_CONTEXT "kind-$PATROL_TARGET_CLUSTER"
emit PATROL_TARGET_INTERNAL_KUBECONFIG "$target_internal_kc"
emit PATROL_TARGET_INTERNAL_SERVER "$PATROL_TARGET_INTERNAL_SERVER"
emit PATROL_REGISTRY "$PATROL_REGISTRY_HOST"
emit PATROL_REGISTRY_INTERNAL "$PATROL_REGISTRY_INTERNAL"
emit PATROL_UI_URL "https://localhost:8443"

for f in "$control_kc" "$target_kc" "$target_internal_kc"; do
	[[ -f $f ]] || patrol::log "note: $f does not exist yet (run hack/kind/up.sh)"
done
