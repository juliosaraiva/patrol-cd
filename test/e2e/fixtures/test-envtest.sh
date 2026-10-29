#!/usr/bin/env bash
# Offline test of envtest_start (test/e2e/fixtures/lib/envtest.sh) with fake etcd, kube-apiserver,
# kubectl and openssl first on PATH: no envtest binaries, no network. It covers a start whose first
# attempt finds a port taken (it starts again on other ports), one where every attempt does (it gives
# up with status 2), an API server failure that is no port conflict (status 1, no retry), a setup step
# that fails (status 1, no API server started), and a second start that retries while a first one
# runs (the first keeps running).
#
# Usage: test/e2e/fixtures/test-envtest.sh   (run by `make ci-scripts`)
set -euo pipefail
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
work=$(mktemp -d)
fake=$work/bin
mkdir -p "$fake"

# etcd stays up. The API server writes its certificate, then either fails, logging $FAKE_MESSAGE
# (a taken port by default), for the first $FAKE_FAILS attempts under $FAKE_FAIL_DIR, or stays up and
# records its PID, which kubectl's readiness check looks for. The attempts are counted in DIR.attempts.
cat >"$fake/etcd" <<'EOF'
#!/usr/bin/env bash
exec sleep 300
EOF
cat >"$fake/kube-apiserver" <<'EOF'
#!/usr/bin/env bash
certdir=
while [[ $# -gt 0 ]]; do
	[[ $1 == --cert-dir ]] && certdir=$2
	shift
done
printf 'cert\n' >"$certdir/apiserver.crt"
dir=$(dirname "$certdir")
n=$(( $(cat "$dir.attempts" 2>/dev/null || echo 0) + 1 ))
echo "$n" >"$dir.attempts"
if [[ $dir == "${FAKE_FAIL_DIR:-}" && $n -le ${FAKE_FAILS:-0} ]]; then
	echo "E run.go:72] \"command failed\" err=\"${FAKE_MESSAGE:-failed to listen on 127.0.0.1:1: bind: address already in use}\"" >&2
	exit 1
fi
echo $$ >"$dir/apiserver.pid"
exec sleep 300
EOF
# kubectl: `config …` writes the kubeconfig; `get --raw /readyz` succeeds while the API server of the
# kubeconfig's directory runs.
cat >"$fake/kubectl" <<'EOF'
#!/usr/bin/env bash
kubeconfig=
for a in "$@"; do
	[[ ${prev:-} == --kubeconfig ]] && kubeconfig=$a
	prev=$a
done
case " $* " in
*" config "*) touch "$kubeconfig" ;;
*" /readyz "*)
	pid=$(cat "$(dirname "$kubeconfig")/apiserver.pid" 2>/dev/null) || exit 1
	kill -0 "$pid" 2>/dev/null
	;;
*) exit 1 ;;
esac
EOF
chmod +x "$fake/etcd" "$fake/kube-apiserver" "$fake/kubectl"
# An openssl whose `rand` (the token) fails; the steps after it would not fail on their own.
mkdir -p "$work/badssl"
# shellcheck disable=SC2016 # the fake's own "$1" and "$@"
printf '#!/bin/sh\n[ "$1" = rand ] && exit 1\nexec %s "$@"\n' "$(command -v openssl)" >"$work/badssl/openssl"
chmod +x "$work/badssl/openssl"
export KUBEBUILDER_ASSETS=$fake PATH="$fake:$PATH"

# shellcheck source=test/e2e/fixtures/lib/envtest.sh
source "$ROOT/test/e2e/fixtures/lib/envtest.sh"
trap 'envtest_stop; rm -rf "$work"' EXIT

passed=0
failed=0
check() { # check NAME WANT GOT
	if [[ $2 == "$3" ]]; then
		passed=$((passed + 1))
	else
		failed=$((failed + 1))
		echo "FAIL $1: want '$2', got '$3'"
	fi
}
start() { # start DIR: envtest_start in this shell (its processes stay tracked); status in $rc
	rc=0
	envtest_start "$1" "$(basename "$1")" 2>"$1.err" || rc=$?
}
alive() { # alive PID...: "yes" when every PID runs
	local p
	for p in "$@"; do
		kill -0 "$p" 2>/dev/null || {
			echo no
			return
		}
	done
	echo yes
}

# A port taken on the first attempt: it starts again, on other ports.
export FAKE_FAIL_DIR=$work/once FAKE_FAILS=1
start "$work/once"
check "a taken port: started" 0 "$rc"
check "a taken port: two attempts" 2 "$(cat "$work/once.attempts")"
check "a taken port: said so" 1 "$(grep -c 'was taken; starting again' "$work/once.err")"

# Every attempt finds a port taken: it gives up after three, with status 2.
export FAKE_FAIL_DIR=$work/always FAKE_FAILS=9
start "$work/always"
check "always taken: status" 2 "$rc"
check "always taken: three attempts" 3 "$(cat "$work/always.attempts")"

# Any other API server failure: status 1, no retry.
export FAKE_FAIL_DIR=$work/other FAKE_FAILS=9 FAKE_MESSAGE="invalid flag"
start "$work/other"
check "another failure: status" 1 "$rc"
check "another failure: one attempt" 1 "$(cat "$work/other.attempts")"
unset FAKE_MESSAGE

# A setup step that fails stops the start (envtest_start_once runs without errexit): no API server.
PATH="$work/badssl:$PATH" start "$work/setup"
check "a failing setup step: status" 1 "$rc"
check "a failing setup step: no API server started" absent "$([[ -e $work/setup.attempts ]] && echo present || echo absent)"

# Two starts in one shell, as test-scripts.sh does: the second's retry leaves the first running.
export FAKE_FAIL_DIR=$work/target FAKE_FAILS=1
before=$ENVTEST_PIDS
start "$work/control"
check "two starts: the first started" 0 "$rc"
read -r -a first <<<"${ENVTEST_PIDS#"$before"}"
check "two starts: the first's etcd and API server" 2 "${#first[@]}"
start "$work/target"
check "two starts: the second started" 0 "$rc"
check "two starts: the second retried" 2 "$(cat "$work/target.attempts")"
check "two starts: the first still runs" yes "$(alive "${first[@]}")"

echo "test-envtest: $passed passed, $failed failed"
[[ $failed -eq 0 ]]
