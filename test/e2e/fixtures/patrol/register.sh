#!/usr/bin/env bash
# Registers the e2e fixture repositories (and optionally the sample Applications) with the
# Patrol CD instance in patrol-control.
#
# Usage: test/e2e/fixtures/patrol/register.sh --namespace NS [--with-applications]
#
#   1. Secrets in NS, copied from the fixture Secrets in patrol-e2e (values are never printed) and
#      labeled cd.patrol.example/secret-type=repo-creds (RepositoryCredential) or repository:
#        e2e-git-http-creds    username, password   <- git-server-http-auth
#        e2e-git-ssh-creds     sshPrivateKey        <- git-server-ssh-client
#        e2e-chart-repo-creds  username, password   <- chart-repo-auth
#   2. the lines of git-server/known_hosts (the git server's TEST-ONLY SSH host key) appended to
#      the ssh_known_hosts key of ConfigMap patrol-ssh-known-hosts-cm when missing, so the
#      repo-server's strict host key checking accepts the fixture git server over SSH;
#   3. repositories.yaml (Repository / RepositoryCredential objects) into NS;
#   4. with --with-applications, applications.yaml (one Application per fixture source type,
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

# put_secret NAME TYPE KEY...: creates or updates Secret NAME in NS from $tmp/KEY files, labeled
# cd.patrol.example/secret-type=TYPE as Patrol CD requires of Secrets that its kinds reference
# (repo-creds: RepositoryCredential, repository: Repository, cluster: Cluster).
put_secret() {
	local name=$1 type=$2 key
	shift 2
	local from=()
	for key in "$@"; do from+=("--from-file=$key=$tmp/$key"); done
	k -n "$ns" create secret generic "$name" "${from[@]}" --dry-run=client -o yaml |
		k label --local -f - app.kubernetes.io/part-of=patrol-e2e-fixtures "cd.patrol.example/secret-type=$type" -o yaml |
		apply -f - >/dev/null
	rm -f "$tmp"/*
	patrol::log "wrote Secret $ns/$name"
}

k create namespace "$ns" --dry-run=client -o yaml | k apply --server-side --field-manager=patrol-e2e -f - >/dev/null

copy_key git-server-http-auth username
copy_key git-server-http-auth password
put_secret e2e-git-http-creds repo-creds username password
copy_key git-server-ssh-client sshPrivateKey
put_secret e2e-git-ssh-creds repo-creds sshPrivateKey
copy_key chart-repo-auth username
copy_key chart-repo-auth password
put_secret e2e-chart-repo-creds repository username password

# The repo-server checks SSH host keys strictly. The ssh_known_hosts key holds one entry per line;
# the fixture's lines are appended when missing, keeping every other entry (idempotent). The key is
# owned by the bootstrap or the API, so the apply forces the conflict.
known_hosts_cm=patrol-ssh-known-hosts-cm
# A missing ConfigMap or key reads as empty; any other failure (API error, timeout) stops the
# script rather than being mistaken for "no known hosts" and force-applied over the real list.
current=$(k -n "$ns" get configmap "$known_hosts_cm" --ignore-not-found \
	-o go-template='{{with .data}}{{with index . "ssh_known_hosts"}}{{.}}{{end}}{{end}}')
combined=$current
added=0
while IFS= read -r line; do
	[[ -z $line || $line == \#* ]] && continue
	if ! grep -qxF -- "$line" <<<"$current"; then
		[[ -z $combined || $combined == *$'\n' ]] || combined+=$'\n'
		combined+="$line"$'\n'
		added=1
	fi
done <"$here/../git-server/known_hosts"
if [[ $added -eq 1 ]]; then
	printf '%s' "$combined" >"$tmp/ssh_known_hosts"
	k -n "$ns" create configmap "$known_hosts_cm" --from-file=ssh_known_hosts="$tmp/ssh_known_hosts" --dry-run=client -o yaml |
		k label --local -f - app.kubernetes.io/part-of=patrol-cd -o yaml |
		apply --force-conflicts -f - >/dev/null
	rm -f "$tmp/ssh_known_hosts"
	patrol::log "added the fixture git server's host key to ConfigMap $ns/$known_hosts_cm"
fi

apply -f "$here/repositories.yaml"
if [[ $with_apps -eq 1 ]]; then
	apply -f "$here/applications.yaml"
fi
patrol::log "registered e2e fixtures in namespace $ns"
