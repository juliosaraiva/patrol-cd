#!/usr/bin/env bash
# Runtime smoke test of a built patrol-cd image (make image-smoke). Starts the repo-server, the
# one component that needs no Kubernetes API, the way the Helm chart runs it: non-root, read-only
# root filesystem, every capability dropped, no privilege escalation, the chart's writable
# emptyDirs as tmpfs, and a throwaway random internal token written inside the container (never
# printed). It then checks /readyz and /healthz?full=true, that tini is PID 1, that SIGTERM stops
# the process with exit status 0, the version stamp of both binaries, the versions of the bundled
# tools against hack/images/checksums.txt, and that every component alias of
# hack/images/aliases.txt dispatches to its subcommand. The container is removed on every exit
# path.
#
# Usage: hack/images/smoke.sh [IMAGE]   (default patrol-cd/patrol-cd:dev; DOCKER overrides docker,
# SMOKE_PORT the loopback port, default 18084)
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
image="${1:-patrol-cd/patrol-cd:dev}"
docker="${DOCKER:-docker}"
name="patrol-smoke-$$"
port="${SMOKE_PORT:-18084}"
fail=0

cleanup() { "$docker" rm -f "$name" >/dev/null 2>&1 || true; }
trap cleanup EXIT

# expect DESCRIPTION ACTUAL WANT: PASS when equal.
expect() {
	if [[ "$2" == "$3" ]]; then
		printf 'PASS %s\n' "$1"
	else
		printf 'FAIL %s: got %q, want %q\n' "$1" "$2" "$3"
		fail=1
	fi
}
# expect_match DESCRIPTION ACTUAL REGEX: PASS when ACTUAL matches the extended regex.
expect_match() {
	if [[ "$2" =~ $3 ]]; then
		printf 'PASS %s: %s\n' "$1" "$2"
	else
		printf 'FAIL %s: got %q, want a match for %s\n' "$1" "$2" "$3"
		fail=1
	fi
}

# first_line CMD...: the first output line, reading all of it (a closed pipe makes Go binaries
# report "broken pipe" and pipefail then aborts the script).
first_line() {
	local out
	out="$("$@")"
	printf '%s' "${out%%$'\n'*}"
}

# The restricted runtime the chart sets (securityContext of every container).
restricted=(--read-only --cap-drop ALL --security-opt no-new-privileges --user 999:999)

# 1. The image runs as the documented user and its binaries carry the build stamp.
expect "runs as patrol (999:999)" "$("$docker" run --rm --network none "${restricted[@]}" "$image" id)" \
	"uid=999(patrol) gid=999(patrol) groups=999(patrol)"
expect_match "patrolcd version stamp" \
	"$(first_line "$docker" run --rm --network none "${restricted[@]}" "$image" patrolcd version)" '^patrolcd: [0-9a-zA-Z.+-]+$'
expect_match "patrol version stamp" \
	"$(first_line "$docker" run --rm --network none "${restricted[@]}" "$image" patrol version --client)" '^patrol: [0-9a-zA-Z.+-]+$'

# The pinned versions are the single source of truth (hack/images/checksums.txt, column 2).
pinned() { awk -v t="$1" '$1 == t { print $2; exit }' "$here/checksums.txt"; }
run() { "$docker" run --rm --network none "${restricted[@]}" --tmpfs /tmp:uid=999,gid=999,mode=1777 -e HOME=/tmp "$image" "$@"; }
expect_match "helm version" "$(run helm version --short)" "^v$(pinned helm)\\+"
expect_match "kustomize version" "$(run kustomize version)" "^v$(pinned kustomize)\$"
expect_match "git-lfs version" "$(run git lfs version)" "^git-lfs/$(pinned git-lfs) "
expect_match "git version" "$(run git --version)" '^git version 2\.'
expect_match "connect-proxy runs (static binary)" "$(run connect-proxy 2>&1 | sed -n '/^Version /p')" '^Version [0-9.]+$'

# Every component alias dispatches to its own subcommand, not to the root help.
root_help="$(first_line run patrolcd --help)"
while read -r alias; do
	[[ -z "$alias" || "$alias" == \#* ]] && continue
	first="$(first_line run "$alias" --help)"
	if [[ -n "$first" && "$first" != "$root_help" ]]; then
		printf 'PASS alias %s dispatches: %s\n' "$alias" "$first"
	else
		printf 'FAIL alias %s: first help line %q\n' "$alias" "$first"
		fail=1
	fi
done <"$here/aliases.txt"

# 2. The repo-server starts under the chart's restricted settings (it needs a port to probe).
"$docker" run -d --name "$name" "${restricted[@]}" \
	--tmpfs /tmp:uid=999,gid=999,mode=1777,size=256m \
	--tmpfs /var/lib/patrol/repo:uid=999,gid=999,mode=0755,size=512m \
	--tmpfs /var/run/patrol/credentials:uid=999,gid=999,mode=0700,size=16m \
	--tmpfs /var/run/patrol/gnupg:uid=999,gid=999,mode=0700,size=64m \
	--tmpfs /app/config/internal-token:uid=999,gid=999,mode=0700,size=1m \
	-p "127.0.0.1:${port}:8084" "$image" \
	sh -c 'head -c 24 /dev/urandom | base64 > /app/config/internal-token/token; exec "$@"' sh \
	/usr/local/bin/patrolcd repo-server --shutdown-timeout=30s --root-dir=/var/lib/patrol/repo \
	--credentials-dir=/var/run/patrol/credentials --gnupg-home-parent=/var/run/patrol/gnupg >/dev/null

code=000
for _ in $(seq 1 30); do
	code=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:${port}/readyz" || true)
	[[ "$code" == 200 ]] && break
	sleep 1
done
expect "/readyz status" "$code" 200
expect "/healthz?full=true status" \
	"$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:${port}/healthz?full=true" || true)" 200
expect "PID 1" "$("$docker" exec "$name" cat /proc/1/comm)" tini
expect "state (running, restarts)" "$("$docker" inspect -f '{{.State.Running}} {{.RestartCount}}' "$name")" "true 0"

# 3. SIGTERM drains and exits 0.
"$docker" stop -t 40 "$name" >/dev/null
expect "exit status after SIGTERM" "$("$docker" inspect -f '{{.State.ExitCode}}' "$name")" 0
expect_match "log after SIGTERM" "$("$docker" logs "$name" 2>&1 | grep -o 'msg=stopped' | head -1)" '^msg=stopped$'

if ((fail != 0)); then
	echo "image-smoke: FAILED for $image" >&2
	exit 1
fi
echo "image-smoke: all checks passed for $image"
