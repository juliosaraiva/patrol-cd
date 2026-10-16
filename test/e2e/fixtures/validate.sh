#!/usr/bin/env bash
# Offline validation of the kind/e2e manifests against a real kube-apiserver (envtest binaries,
# Kubernetes 1.36) with the Patrol CRDs from deploy/crds installed. No Docker, no network.
#
#   fixture manifests      git-server, seed Job, chart-repo, toolbox, namespace,
#                          hack/kind/manifests/target-manager.yaml
#   Patrol objects         test/e2e/fixtures/patrol/*.yaml, hack/kind/manifests/cluster-patrol-target.yaml
#                          (CRD structural schemas and CEL rules)
#   rendered seed apps     guestbook (plain), kustomize-guestbook overlays (kustomize build),
#                          helm-guestbook (helm lint --strict; helm template with every values
#                          file), each also with its seed/updates overlay applied
#
# Every object is created with `kubectl create --dry-run=server` (strict field validation);
# namespaces are created for real. Prints one line per object and fails on the first error.
#
# Usage: test/e2e/fixtures/validate.sh
#   KUBEBUILDER_ASSETS  default: bin/envtest/k8s/1.36.0-<os>-<arch> (make envtest)
#   KUSTOMIZE           default: bin/kustomize, else kustomize on PATH
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
root=$(cd "$here/../../.." && pwd)
# shellcheck source=test/e2e/fixtures/lib/envtest.sh
source "$here/lib/envtest.sh"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in x86_64) arch=amd64 ;; aarch64) arch=arm64 ;; esac
export KUBEBUILDER_ASSETS=${KUBEBUILDER_ASSETS:-$root/bin/envtest/k8s/1.36.0-$os-$arch}
kustomize=${KUSTOMIZE:-$root/bin/kustomize}
[[ -x $kustomize ]] || kustomize=$(command -v kustomize) || {
	echo "kustomize not found (make tools)" >&2
	exit 1
}
for t in kubectl helm openssl; do
	command -v "$t" >/dev/null || {
		echo "$t not found on PATH" >&2
		exit 1
	}
done

work=$(mktemp -d)
cleanup() {
	envtest_stop
	rm -rf "$work"
}
trap cleanup EXIT

echo "starting envtest kube-apiserver from $KUBEBUILDER_ASSETS"
envtest_start "$work/envtest"
k() { kubectl --kubeconfig "$ENVTEST_KUBECONFIG" --context "$ENVTEST_CONTEXT" "$@"; }
echo "server version: $(k version -o json | sed -n 's/.*"gitVersion": *"\([^"]*\)".*/\1/p' | tail -n 1)"

objects=0
# dry_run SOURCE [NAMESPACE] < manifests: server-side dry-run create of every object on stdin.
dry_run() {
	local src=$1 ns=${2-} out
	local args=(create --dry-run=server --validate=strict -o name -f -)
	[[ -z $ns ]] || args+=(-n "$ns")
	if ! out=$(k "${args[@]}" 2>&1); then
		printf 'FAIL  %s\n%s\n' "$src" "$out" >&2
		exit 1
	fi
	while IFS= read -r line; do
		[[ -n $line ]] || continue
		objects=$((objects + 1))
		printf 'ok    %-58s %s\n' "$src" "$line"
	done <<<"$out"
}
file() { dry_run "${1#"$root"/}" "${2-}" <"$1"; }
# rendered LABEL NAMESPACE COMMAND...: dry-run the output of a renderer (not in a pipeline, so the
# object count survives).
rendered() {
	local label=$1 ns=$2
	shift 2
	"$@" >"$work/rendered.yaml"
	dry_run "$label" "$ns" <"$work/rendered.yaml"
}

echo "installing Patrol CRDs (server-side apply of deploy/crds)"
k apply --server-side -f "$root/deploy/crds" >/dev/null
# `kubectl wait` fails at once while a new CRD has no status conditions yet, so retry.
for _ in $(seq 1 30); do
	k wait --for=condition=Established --timeout=2s crd --all >/dev/null 2>&1 && break
	sleep 1
done
k wait --for=condition=Established --timeout=2s crd --all >/dev/null

# Namespaces are created for real (the fixture's own Namespace manifest included).
printf 'ok    %-58s %s (created)\n' test/e2e/fixtures/namespace.yaml "$(k create -f "$here/namespace.yaml" -o name)"
objects=$((objects + 1))
k create namespace patrol-validate -o name >/dev/null
k create namespace e2e-render -o name >/dev/null

echo "fixture manifests"
for f in "$here"/git-server/manifests/*.yaml "$here/git-server/seed-job.yaml" \
	"$here"/chart-repo/manifests/*.yaml "$here/toolbox/pod.yaml" \
	"$root/hack/kind/manifests/target-manager.yaml"; do
	file "$f"
done

echo "Patrol objects (namespace patrol-validate)"
for f in "$here/patrol/repositories.yaml" "$here/patrol/applications.yaml" \
	"$root/hack/kind/manifests/cluster-patrol-target.yaml"; do
	file "$f" patrol-validate
done

# stage REPO: copy of a seed repository with its updates overlay applied (second revision).
stage() {
	local dst="$work/stage/$1"
	rm -rf "$dst"
	mkdir -p "$dst"
	cp -R "$here/seed/repos/$1/." "$dst/"
	[[ ! -d $here/seed/updates/$1 ]] || cp -R "$here/seed/updates/$1/." "$dst/"
	printf '%s\n' "$dst"
}

echo "seed: guestbook (plain directory)"
for f in "$here"/seed/repos/guestbook/*.yaml; do file "$f" e2e-render; done
for f in "$(stage guestbook)"/*.yaml; do
	# shellcheck disable=SC2094 # $f is only read; its name is a label
	dry_run "seed/updates/guestbook/$(basename "$f")" e2e-render <"$f"
done

echo "seed: kustomize-guestbook (kustomize build)"
for overlay in staging production; do
	rendered "kustomize build overlays/$overlay" e2e-render \
		"$kustomize" build "$here/seed/repos/kustomize-guestbook/overlays/$overlay"
done
rendered "kustomize build overlays/staging (updates)" e2e-render \
	"$kustomize" build "$(stage kustomize-guestbook)/overlays/staging"

echo "seed: helm-guestbook (helm lint + helm template)"
chart="$here/seed/repos/helm-guestbook"
helm lint --strict "$chart" >/dev/null
updated_chart=$(stage helm-guestbook)
mkdir -p "$work/stage/chart" && rm -rf "$work/stage/chart/helm-guestbook" && mv "$updated_chart" "$work/stage/chart/helm-guestbook"
updated_chart="$work/stage/chart/helm-guestbook"
helm lint --strict "$updated_chart" >/dev/null
rendered "helm template helm-guestbook" e2e-render helm template e2e "$chart"
rendered "helm template helm-guestbook (updates)" e2e-render helm template e2e "$updated_chart"
values_updated=$(stage helm-values)
for v in "$here/seed/repos/helm-values/values/dev.yaml" "$here/seed/repos/helm-values/values/prod.yaml" \
	"$values_updated/values/dev.yaml"; do
	label=${v#"$here"/}
	label=${label#"$work"/stage/}
	rendered "helm template -f $label" e2e-render helm template e2e "$chart" -f "$v"
done
charts_dir=${PATROL_E2E_CHARTS_DIR:-$root/.cache/kind/build/charts}
for tgz in "$charts_dir"/helm-guestbook-*.tgz; do
	[[ -f $tgz ]] || continue
	rendered "helm template $(basename "$tgz")" e2e-render helm template e2e "$tgz"
done

echo "validated $objects objects against kube-apiserver $ENVTEST_SERVER with deploy/crds installed"
