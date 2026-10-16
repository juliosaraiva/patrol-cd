#!/usr/bin/env bash
# Sends a signed GitHub webhook delivery (a generated "push" event by default), for manual
# testing of patrol-server's webhook endpoint. The signature is
#   X-Hub-Signature-256: sha256=<hex HMAC-SHA256(secret, raw body)>
# computed over the exact bytes curl sends (--data-binary @file).
#
# Usage: test/e2e/fixtures/webhook/send-github-push.sh [options]
#   --url URL           endpoint (default https://localhost:8443/api/webhook)
#   --secret-file FILE  file holding the webhook secret; default: $PATROL_WEBHOOK_SECRET.
#                       Without a secret the delivery is sent unsigned.
#   --repo URL          repository URL; http(s) URLs fill html_url/clone_url, ssh:// and
#                       user@host:path URLs fill ssh_url
#                       (default http://git-server.patrol-e2e.svc.cluster.local/git/guestbook.git)
#   --ref REF           pushed ref (default refs/heads/main)
#   --default-branch B  repository.default_branch (default main)
#   --before SHA        (default 40 zeros)
#   --after SHA         (default 40 "1"s)
#   --changed PATH      file listed as modified in the pushed commit; repeatable
#   --payload FILE      send FILE verbatim instead of a generated payload
#   --event NAME        X-GitHub-Event (default push; e.g. ping or pull_request with --payload)
#   --delivery ID       X-GitHub-Delivery (default: random UUID)
#   --insecure          skip TLS verification (self-signed local endpoints)
#   --dry-run           print the headers and body instead of sending
#   --self-test         check the HMAC implementation against GitHub's documented example
#
# Requires bash, curl and openssl. Note: openssl receives the secret as a command-line argument
# (`openssl dgst -hmac`), so it is briefly visible to other local users in the process list; use
# this tool with test secrets only.
set -euo pipefail

url=https://localhost:8443/api/webhook
secret_file=""
repo=http://git-server.patrol-e2e.svc.cluster.local/git/guestbook.git
ref=refs/heads/main
default_branch=main
before=0000000000000000000000000000000000000000
after=1111111111111111111111111111111111111111
changed=()
payload_file=""
event=push
delivery=""
insecure=0
dry_run=0
self_test=0

die() {
	printf 'send-github-push: %s\n' "$*" >&2
	exit 1
}
usage() { awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "${BASH_SOURCE[0]}"; }
need() { [[ $# -ge 2 && -n $2 ]] || die "$1 needs a value"; }

while [[ $# -gt 0 ]]; do
	case "$1" in
	--url) need "$@"; url=$2; shift 2 ;;
	--secret-file) need "$@"; secret_file=$2; shift 2 ;;
	--repo) need "$@"; repo=$2; shift 2 ;;
	--ref) need "$@"; ref=$2; shift 2 ;;
	--default-branch) need "$@"; default_branch=$2; shift 2 ;;
	--before) need "$@"; before=$2; shift 2 ;;
	--after) need "$@"; after=$2; shift 2 ;;
	--changed) need "$@"; changed+=("$2"); shift 2 ;;
	--payload) need "$@"; payload_file=$2; shift 2 ;;
	--event) need "$@"; event=$2; shift 2 ;;
	--delivery) need "$@"; delivery=$2; shift 2 ;;
	--insecure) insecure=1; shift ;;
	--dry-run) dry_run=1; shift ;;
	--self-test) self_test=1; shift ;;
	-h | --help) usage; exit 0 ;;
	*) die "unknown argument: $1 (see --help)" ;;
	esac
done

command -v openssl >/dev/null || die "openssl not found on PATH"

# hmac_sha256_hex SECRET FILE
hmac_sha256_hex() {
	openssl dgst -sha256 -hmac "$1" <"$2" | awk '{ print $NF }'
}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

if [[ $self_test -eq 1 ]]; then
	# Example from GitHub's "Validating webhook deliveries" documentation.
	printf 'Hello, World!' >"$work/body"
	got=$(hmac_sha256_hex "It's a Secret to Everybody" "$work/body")
	want=757107ea0eb2509fc211221cce984b8a37570b6d7586c22c46f4379c8b043e17
	[[ $got == "$want" ]] || die "self-test failed: got $got, want $want"
	echo "self-test passed: sha256=$got"
	exit 0
fi

# json_string VALUE: VALUE as a JSON string literal. Control characters are rejected.
json_string() {
	local s=$1
	if [[ $s == *[[:cntrl:]]* ]]; then
		die "control characters are not supported in payload values"
	fi
	s=${s//\\/\\\\}
	s=${s//\"/\\\"}
	printf '"%s"' "$s"
}

secret=""
if [[ -n $secret_file ]]; then
	[[ -r $secret_file ]] || die "cannot read $secret_file"
	secret=$(cat "$secret_file")
elif [[ -n ${PATROL_WEBHOOK_SECRET:-} ]]; then
	secret=$PATROL_WEBHOOK_SECRET
fi

# Reject control characters up front (json_string runs in command substitutions).
for v in "$ref" "$default_branch" "$before" "$after" "$repo" "$event" ${changed[@]+"${changed[@]}"}; do
	[[ $v != *[[:cntrl:]]* ]] || die "control characters are not supported in payload values"
done

body="$work/body.json"
if [[ -n $payload_file ]]; then
	[[ -r $payload_file ]] || die "cannot read $payload_file"
	cp "$payload_file" "$body"
else
	html_url=""
	clone_url=""
	ssh_url=""
	case "$repo" in
	http://* | https://*)
		clone_url=$repo
		html_url=${repo%.git}
		;;
	*) ssh_url=$repo ;;
	esac
	path=${repo%.git}
	name=${path##*/}
	owner_path=${path%/*}
	owner=${owner_path##*[/:]}
	[[ -n $name && -n $owner ]] || die "cannot derive owner/name from --repo $repo"
	modified="["
	sep=""
	if [[ ${#changed[@]} -gt 0 ]]; then
		for p in "${changed[@]}"; do
			modified+="$sep$(json_string "$p")"
			sep=","
		done
	fi
	modified+="]"
	{
		printf '{"ref":%s,"before":%s,"after":%s,"created":false,"deleted":false,"forced":false,' \
			"$(json_string "$ref")" "$(json_string "$before")" "$(json_string "$after")"
		printf '"commits":[{"id":%s,"message":"e2e push","added":[],"removed":[],"modified":%s}],' \
			"$(json_string "$after")" "$modified"
		printf '"head_commit":{"id":%s,"message":"e2e push","added":[],"removed":[],"modified":%s},' \
			"$(json_string "$after")" "$modified"
		printf '"repository":{"id":1,"name":%s,"full_name":%s,"owner":{"login":%s,"name":%s},"private":false,' \
			"$(json_string "$name")" "$(json_string "$owner/$name")" "$(json_string "$owner")" "$(json_string "$owner")"
		printf '"html_url":%s,"clone_url":%s,"ssh_url":%s,"default_branch":%s},' \
			"$(json_string "$html_url")" "$(json_string "$clone_url")" "$(json_string "$ssh_url")" "$(json_string "$default_branch")"
		printf '"sender":{"login":"patrol-e2e","id":1,"type":"User"}}'
	} >"$body"
fi

if [[ -z $delivery ]]; then
	hex=$(openssl rand -hex 16)
	delivery="${hex:0:8}-${hex:8:4}-${hex:12:4}-${hex:16:4}-${hex:20:12}"
fi
headers=(
	-H "Content-Type: application/json"
	-H "User-Agent: GitHub-Hookshot/patrol-e2e"
	-H "X-GitHub-Event: $event"
	-H "X-GitHub-Delivery: $delivery"
)
if [[ -n $secret ]]; then
	sig=$(hmac_sha256_hex "$secret" "$body")
	headers+=(-H "X-Hub-Signature-256: sha256=$sig")
fi

if [[ $dry_run -eq 1 ]]; then
	echo "POST $url"
	for ((i = 1; i < ${#headers[@]}; i += 2)); do echo "${headers[$i]}"; done
	echo
	cat "$body"
	echo
	exit 0
fi

command -v curl >/dev/null || die "curl not found on PATH"
curl_opts=(-sS -X POST --data-binary "@$body" -w '\nHTTP %{http_code}\n')
if [[ $insecure -eq 1 ]]; then
	curl_opts+=(-k)
fi
curl "${curl_opts[@]}" "${headers[@]}" "$url"
