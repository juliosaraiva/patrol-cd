#!/usr/bin/env bash
# Registers the e2e fixture repositories (and optionally the sample Applications) with the
# Patrol CD instance in patrol-control.
#
# Usage: test/e2e/fixtures/patrol/register.sh --namespace NS [--with-applications]
#
#   1. Secrets in NS, copied from the fixture Secrets in patrol-e2e (values are never printed):
#        e2e-git-http-creds    username, password   <- git-server-http-auth
#        e2e-git-ssh-creds     sshPrivateKey        <- git-server-ssh-client
#        e2e-chart-repo-creds  username, password   <- chart-repo-auth
#   2. repositories.yaml (Repository / RepositoryCredential objects) into NS;
#   3. with --with-applications, applications.yaml (one Application per fixture source type,
#      destination name patrol-target; register it first with hack/kind/register-target.sh).
#
# NS is the namespace Patrol CD runs in. Requires the Patrol CRDs in patrol-control and
# test/e2e/fixtures/deploy.sh to have run.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=hack/kind/lib.sh
source "$here/../../../../hack/kind/lib.sh"

ns=""
with_apps=0
while [[ $# -gt 0 ]]; do
	case "$1" in
	--namespace)
		[[ $# -ge 2 ]] || patrol::die "--namespace needs a value"
		ns=$2
		shift 2
		;;
	--with-applications)
		with_apps=1
		shift
		;;
	-h | --help)
		awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "${BASH_SOURCE[0]}"
		exit 0
		;;
	*) patrol::die "unexpected argument: $1 (see --help)" ;;
	esac
done
[[ -n $ns ]] || patrol::die "--namespace is required (the namespace Patrol CD runs in)"
[[ $ns =~ ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$ ]] || patrol::die "invalid namespace name: $ns"

patrol::require_tools kubectl
cluster=$PATROL_CONTROL_CLUSTER
k() { patrol::kubectl "$cluster" "$@"; }
apply() { k -n "$ns" apply --server-side --field-manager=patrol-e2e "$@"; }

for crd in repositories.cd.patrol.example repositorycredentials.cd.patrol.example applications.cd.patrol.example; do
	k get crd "$crd" >/dev/null 2>&1 || patrol::die "CRD $crd is not installed in $cluster"
done

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
chmod 700 "$tmp"

# copy_key SRC_SECRET KEY: writes the decoded value of a patrol-e2e Secret key to $tmp/KEY.
copy_key() {
	k -n patrol-e2e get secret "$1" -o go-template="{{index .data \"$2\" | base64decode}}" >"$tmp/$2" ||
		patrol::die "cannot read key $2 of Secret patrol-e2e/$1 (run test/e2e/fixtures/deploy.sh)"
	[[ -s $tmp/$2 ]] || patrol::die "key $2 of Secret patrol-e2e/$1 is empty"
}

# put_secret NAME KEY...: creates or updates Secret NAME in NS from $tmp/KEY files.
put_secret() {
	local name=$1 key
	shift
	local from=()
	for key in "$@"; do from+=("--from-file=$key=$tmp/$key"); done
	k -n "$ns" create secret generic "$name" "${from[@]}" --dry-run=client -o yaml |
		k label --local -f - app.kubernetes.io/part-of=patrol-e2e-fixtures -o yaml |
		apply -f - >/dev/null
	rm -f "$tmp"/*
	patrol::log "wrote Secret $ns/$name"
}

k create namespace "$ns" --dry-run=client -o yaml | k apply --server-side --field-manager=patrol-e2e -f - >/dev/null

copy_key git-server-http-auth username
copy_key git-server-http-auth password
put_secret e2e-git-http-creds username password
copy_key git-server-ssh-client sshPrivateKey
put_secret e2e-git-ssh-creds sshPrivateKey
copy_key chart-repo-auth username
copy_key chart-repo-auth password
put_secret e2e-chart-repo-creds username password

apply -f "$here/repositories.yaml"
if [[ $with_apps -eq 1 ]]; then
	apply -f "$here/applications.yaml"
fi
patrol::log "registered e2e fixtures in namespace $ns"
