#!/usr/bin/env bash
# Prints the host environment of a CI job and checks the tools its profile needs: present, at
# the pinned version and, for helm, first on PATH. One line per fact or check, so the job log
# can be grepped:
#
#   env  KEY            VALUE              host fact ("n/a" where it is Linux-only and the host
#                                          is not Linux)
#   tool NAME           VERSION  PATH      a tool the profile uses
#   ok   MESSAGE                           requirement met
#   WARN MESSAGE                           worth a look, not fatal
#   FAIL MESSAGE                           requirement not met (one line per problem)
#
# Usage: hack/ci/preflight.sh [--fix] [--warn-disk] PROFILE...
#
#   PROFILE      static | unit | integration | deploy | ui | image | kind | release (several are merged)
#   --fix        kind: raise fs.inotify.max_user_watches/max_user_instances to kind's
#                recommendation with `sudo -n sysctl -w` when they are lower (Linux, passwordless
#                sudo only); without it low limits are a FAIL
#   --warn-disk  kind: less than 20 GiB free on / is a WARN instead of a FAIL
#
# Expected versions are read from the repository: go.mod (Go), tools/go.mod (kustomize),
# Makefile (envtest), Dockerfile NODE_IMAGE (Node), ui/package.json packageManager (pnpm) and
# hack/ci/tools.lock (helm, kind, kubectl, golangci-lint, gitleaks, shellcheck, actionlint).
# Runs on Linux (CI) and macOS (developers). Exit status: 0 when nothing failed (warnings
# allowed), 1 when at least one FAIL line was printed, 2 on a usage error.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo=$(cd "$here/../.." && pwd)
lock=$here/tools.lock

usage() { awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "${BASH_SOURCE[0]}"; }
usage_error() {
	printf 'preflight: %s (see --help)\n' "$*" >&2
	exit 2
}

fix=0
warn_disk=0
profiles=()
while [[ $# -gt 0 ]]; do
	case $1 in
	--fix) fix=1 ;;
	--warn-disk) warn_disk=1 ;;
	-h | --help)
		usage
		exit 0
		;;
	static | unit | integration | deploy | ui | image | kind | release) profiles+=("$1") ;;
	*) usage_error "unknown profile or option: $1" ;;
	esac
	shift
done
[[ ${#profiles[@]} -gt 0 ]] || usage_error "name a profile: static, unit, integration, deploy, ui, image or kind"

# What the selected profiles need.
need_go=0 need_gotest=0 need_static=0 need_ui=0 need_docker=0 need_kind=0 need_helm=0 need_kustomize=0
for p in "${profiles[@]}"; do
	case $p in
	unit | integration | deploy) need_go=1 need_gotest=1 need_helm=1 need_kustomize=1 ;;
	static) need_go=1 need_static=1 need_helm=1 need_kustomize=1 ;;
	release) need_go=1 need_helm=1 need_kustomize=1 ;;
	ui) need_ui=1 ;;
	image) need_docker=1 need_go=1 ;;
	kind) need_docker=1 need_kind=1 need_helm=1 ;;
	esac
done

fails=0
warns=0
ok() { printf 'ok   %s\n' "$*"; }
warn() {
	printf 'WARN %s\n' "$*"
	warns=$((warns + 1))
}
fail() {
	printf 'FAIL %s\n' "$*"
	fails=$((fails + 1))
}
fact() { printf 'env  %-14s %s\n' "$1" "$2"; }
tool() { printf 'tool %-14s %-12s %s\n' "$1" "$2" "$3"; }
have() { command -v "$1" >/dev/null 2>&1; }
# no_v VERSION: VERSION without a leading "v" before a digit (v1.2.3 -> 1.2.3).
no_v() { case $1 in v[0-9]*) printf '%s\n' "${1#v}" ;; *) printf '%s\n' "$1" ;; esac; }
# first LINE of a command's combined output, "" when it fails to start.
first() { "$@" </dev/null 2>&1 | head -n 1 || true; }
# lock_version NAME: the version tools.lock pins (same on every platform).
lock_version() { awk -v n="$1" '$1 == n { print $2; exit }' "$lock"; }
# version_ge A B: dotted version A >= B (numeric fields, missing fields count as 0).
version_ge() {
	awk -v a="$1" -v b="$2" 'BEGIN {
		na = split(a, x, "."); nb = split(b, y, "."); n = na > nb ? na : nb
		for (i = 1; i <= n; i++) { if (x[i] + 0 > y[i] + 0) exit 0; if (x[i] + 0 < y[i] + 0) exit 1 }
		exit 0 }'
}

os=$(uname -s)
linux=0
[[ $os == Linux ]] && linux=1

# --- environment -----------------------------------------------------------------------------------
echo "preflight: profile(s) ${profiles[*]} on $(uname -n 2>/dev/null || echo ?)"
fact uname "$(uname -srm)"
[[ -z ${ImageOS:-}${ImageVersion:-} ]] || fact runner_image "${ImageOS:-?} ${ImageVersion:-?}"
if have nproc; then
	fact nproc "$(nproc)"
else
	fact nproc "$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo n/a)"
fi
if [[ $linux -eq 1 ]] && have free; then
	fact mem_mb "$(free -m | awk '/^Mem:/ { printf "total=%s used=%s available=%s", $2, $3, $7 }')"
	fact swap_mb "$(free -m | awk '/^Swap:/ { printf "total=%s used=%s", $2, $3 }')"
else
	fact mem_mb "n/a"
fi
for m in / /mnt /tmp; do
	if [[ -d $m ]]; then
		fact "disk $m" "$(df -Ph "$m" | awk 'NR == 2 { printf "size=%s used=%s avail=%s use=%s fs=%s", $2, $3, $4, $5, $1 }')"
	fi
done
fact umask "$(umask)"
fact ulimit_n "$(ulimit -n)"
if [[ $linux -eq 1 && -r /proc/sys/fs/inotify/max_user_watches ]]; then
	fact inotify "max_user_watches=$(cat /proc/sys/fs/inotify/max_user_watches) max_user_instances=$(cat /proc/sys/fs/inotify/max_user_instances)"
else
	fact inotify "n/a"
fi
fact user "$(id -un 2>/dev/null || echo ?) uid=$(id -u)"
fact path "$PATH"
if have docker; then
	dv=$(docker version --format 'client={{.Client.Version}} server={{.Server.Version}} api={{.Server.APIVersion}}' 2>/dev/null) ||
		dv="client=$(docker version --format '{{.Client.Version}}' 2>/dev/null || echo ?) server=unreachable"
	fact docker "$dv"
	di=$(docker info --format 'storage={{.Driver}} cgroup={{.CgroupDriver}}/v{{.CgroupVersion}} cpus={{.NCPU}} mem_bytes={{.MemTotal}} os={{.OperatingSystem}} arch={{.Architecture}} root={{.DockerRootDir}}' 2>/dev/null) || di="n/a"
	fact docker_info "$di"
	if docker info --format '{{json .DriverStatus}}' 2>/dev/null | grep 'io.containerd.snapshotter' >/dev/null; then
		fact docker_store "containerd image store"
	elif [[ $di != n/a ]]; then
		fact docker_store "classic (graph driver)"
	fi
	fact buildx "$(first docker buildx version)"
else
	fact docker "n/a (not installed)"
fi

# --- tool checks -----------------------------------------------------------------------------------
# check_exact NAME WANT GOT PATH: tool line plus ok/FAIL for an exact version.
check_exact() {
	tool "$1" "${3:-?}" "$4"
	if [[ $3 == "$2" ]]; then
		ok "$1 $2"
	else
		fail "$1: found ${3:-no version} at $4, need $2"
	fi
}

# check_lock_tool NAME: a tools.lock tool must be on PATH and report the pinned version.
check_lock_tool() {
	local name=$1 want path got
	want=$(lock_version "$name")
	if ! path=$(command -v "$name"); then
		fail "$name: not on PATH (need $want: hack/ci/install-tools.sh $name)"
		return 0
	fi
	case $name in
	kind) got=$(first "$path" version | awk '{ print $2 }') ;;
	kubectl) got=$("$path" version --client -o json </dev/null 2>/dev/null | sed -n 's/.*"gitVersion": *"\([^"]*\)".*/\1/p' | head -n 1) ;;
	golangci-lint) got=$(first "$path" --version | sed -n 's/.* version \([^ ]*\) .*/\1/p') ;;
	gitleaks) got=$(first "$path" version) ;;
	shellcheck) got=$("$path" --version </dev/null 2>/dev/null | sed -n 's/^version: //p') ;;
	actionlint) got=$(first "$path" -version) ;;
	esac
	check_exact "$name" "$want" "$(no_v "$got")" "$path"
}

# check_helm_binary LABEL PATH: PATH must be Helm v<lock version>.
check_helm_binary() {
	local label=$1 path=$2 want got
	want=$(lock_version helm)
	got=$("$path" version --template '{{.Version}}' </dev/null 2>/dev/null) || got=
	tool "$label" "${got:-?}" "$path"
	case $got in
	"v$want") ok "$label is Helm $got" ;;
	v4.*) fail "$label: $path is Helm $got, need v$want" ;;
	v3.* | v2.*) fail "$label: $path is Helm $got (Helm 4 is required: hack/genmanifests refuses others), need v$want" ;;
	*) fail "$label: $path does not report a Helm version, need v$want" ;;
	esac
}

if [[ $need_go -eq 1 ]]; then
	want=$(awk '$1 == "toolchain" { sub(/^go/, "", $2); print $2; exit }' "$repo/go.mod")
	[[ -n $want ]] || want=$(awk '$1 == "go" { print $2; exit }' "$repo/go.mod")
	if path=$(command -v go); then
		# The installed toolchain, from outside the module so go does not switch toolchains.
		got=$(cd / && GOTOOLCHAIN=local go env GOVERSION 2>/dev/null) || got=
		check_exact go "$want" "${got#go}" "$path"
		gotc=$(cd / && go env GOTOOLCHAIN 2>/dev/null) || gotc=
		[[ $gotc == local ]] || warn "go: GOTOOLCHAIN is '${gotc:-?}'; CI sets local so go never downloads another toolchain"
	else
		fail "go: not on PATH (need $want, from go.mod)"
	fi
fi

if [[ $need_gotest -eq 1 ]]; then
	# -race needs cgo and a C compiler on Linux.
	if have go; then
		cgo=$(cd / && GOTOOLCHAIN=local go env CGO_ENABLED 2>/dev/null || echo ?)
		if [[ $cgo == 1 ]]; then
			ok "go: CGO_ENABLED=1 (-race)"
		else
			fail "go: CGO_ENABLED=$cgo; -race needs 1 (do not export CGO_ENABLED=0 in test jobs)"
		fi
	fi
	if path=$(command -v gcc); then
		tool gcc "$(gcc -dumpfullversion 2>/dev/null || gcc -dumpversion 2>/dev/null || echo ?)" "$path"
		ok "gcc present (cgo for -race)"
	else
		fail "gcc: not on PATH (cgo, needed by go test -race)"
	fi
	if path=$(command -v git); then
		got=$(first git --version | awk '{ print $3 }')
		tool git "${got:-?}" "$path"
		if version_ge "${got:-0}" 2.28; then ok "git $got >= 2.28 (init --initial-branch)"; else fail "git: $got is older than 2.28 (tests use git init --initial-branch)"; fi
		backend="$(git --exec-path 2>/dev/null)/git-http-backend"
		if [[ -x $backend ]]; then
			ok "git-http-backend at $backend"
		else
			fail "git-http-backend: not executable at $backend (git --exec-path)"
		fi
		got=$(git lfs version </dev/null 2>/dev/null | head -n 1) || got=
		if [[ -n $got ]]; then
			tool git-lfs "$(printf '%s\n' "$got" | sed -n 's|^git-lfs/\([^ ]*\).*|\1|p')" "$(command -v git-lfs || echo '?')"
			ok "git lfs works (internal/git TestLFS)"
		else
			fail "git-lfs: 'git lfs version' fails (internal/git TestLFS would skip)"
		fi
	else
		fail "git: not on PATH"
	fi
	for t in gpg gpgconf ssh ssh-keygen; do
		if path=$(command -v "$t"); then
			case $t in
			gpg | gpgconf) v=$(first "$t" --version | awk '{ print $NF }') ;;
			ssh) v=$(first ssh -V | awk '{ print $1 }' | sed 's/,$//') ;;
			*) v=- ;;
			esac
			tool "$t" "$v" "$path"
			ok "$t present"
		else
			fail "$t: not on PATH (internal/git and internal/reposerver tests skip without it)"
		fi
	done
	# internal/git ssh_test.go looks at these absolute paths first, then PATH.
	sshd=
	for c in /usr/sbin/sshd /opt/homebrew/sbin/sshd /usr/local/sbin/sshd; do
		[[ -x $c ]] && sshd=$c && break
	done
	[[ -n $sshd ]] || sshd=$(command -v sshd || true)
	if [[ -n $sshd ]]; then
		tool sshd "$(first "$sshd" -V | awk '{ print $1 }' | sed 's/,$//')" "$sshd"
		ok "sshd executable (internal/git TestSSHTransport)"
		# Debian's sshd -t, which that test runs first, refuses to start as root without /run/sshd
		# (a container, not a runner job); as an ordinary user it does not need it.
		if [[ $(uname -s) == Linux && $(id -u) -eq 0 && ! -d /run/sshd ]]; then
			warn "/run/sshd is missing and this runs as root: sshd -t refuses to start without it (install -d -m 0755 /run/sshd; TestSSHTransport would skip)"
		fi
	else
		fail "sshd: no executable /usr/sbin/sshd and none on PATH (install openssh-server; TestSSHTransport would skip)"
	fi
	# Envtest API server binaries.
	envtest_version=$(sed -n 's/^ENVTEST_K8S_VERSION *?= *//p' "$repo/Makefile")
	if [[ -n ${KUBEBUILDER_ASSETS:-} ]]; then
		assets=$KUBEBUILDER_ASSETS
		how="KUBEBUILDER_ASSETS"
	else
		goos=$(uname -s | tr '[:upper:]' '[:lower:]')
		case $(uname -m) in x86_64 | amd64) goarch=amd64 ;; aarch64 | arm64) goarch=arm64 ;; *) goarch=$(uname -m) ;; esac
		assets=$repo/bin/envtest/k8s/$envtest_version-$goos-$goarch
		how="KUBEBUILDER_ASSETS unset; default location"
	fi
	if [[ -x $assets/kube-apiserver && -x $assets/etcd ]]; then
		got=$(first "$assets/kube-apiserver" --version | awk '{ print $2 }')
		tool envtest "${got:-?}" "$assets"
		ok "envtest assets: kube-apiserver and etcd in $assets ($how)"
		[[ ${got#v} == "$envtest_version" ]] || warn "envtest: kube-apiserver reports ${got:-?}, Makefile ENVTEST_K8S_VERSION is $envtest_version"
	else
		fail "envtest: no executable kube-apiserver and etcd in $assets ($how; run make envtest)"
	fi
	# Tests must not reach a real cluster.
	if [[ -e $HOME/.kube/config ]]; then
		fail "kubeconfig: $HOME/.kube/config exists; Go tests read it when KUBECONFIG is unset (internal/clusters)"
	else
		ok "no $HOME/.kube/config"
	fi
	if [[ -z ${KUBECONFIG+x} ]]; then
		ok "KUBECONFIG unset"
	else
		fail "KUBECONFIG is set; unset it in Go test jobs"
	fi
fi

if [[ $need_helm -eq 1 ]]; then
	if path=$(command -v helm); then
		check_helm_binary helm "$path"
	else
		fail "helm: not on PATH (need v$(lock_version helm): hack/ci/install-tools.sh helm)"
	fi
	for var in HELM PATROL_TEST_HELM; do
		val=${!var:-}
		if [[ -n $val ]]; then
			if [[ -x $val ]]; then check_helm_binary "\$$var" "$val"; else fail "\$$var=$val is not an executable file"; fi
		fi
	done
fi

# check_kustomize LABEL PATH WANT: PATH must run and report kustomize WANT (vX.Y.Z).
check_kustomize() {
	local got
	if ! got=$("$2" version </dev/null 2>/dev/null); then
		tool "$1" "?" "$2"
		fail "$1: $2 does not run on this host (another OS or architecture? make tools), need $3"
		return 0
	fi
	check_exact "$1" "$(no_v "$3")" "$(no_v "$(printf '%s\n' "$got" | head -n 1)")" "$2"
}

if [[ $need_kustomize -eq 1 ]]; then
	want=$(awk '$1 == "sigs.k8s.io/kustomize/kustomize/v5" && $2 ~ /^v[0-9]/ { print $2; exit }' "$repo/tools/go.mod")
	kz=$repo/bin/kustomize
	if [[ -x $kz ]]; then
		check_kustomize bin/kustomize "$kz" "$want"
	else
		fail "bin/kustomize: missing (need $want: make tools)"
	fi
	for var in KUSTOMIZE PATROL_TEST_KUSTOMIZE; do
		val=${!var:-}
		if [[ -n $val ]]; then
			if [[ -x $val ]]; then check_kustomize "\$$var" "$val" "$want"; else fail "\$$var=$val is not an executable file"; fi
		fi
	done
fi

if [[ $need_static -eq 1 ]]; then
	for t in golangci-lint shellcheck gitleaks actionlint; do
		check_lock_tool "$t"
	done
	if path=$(command -v dash); then tool dash - "$path"; else tool dash missing "(lint-shell.sh skips dash -n)"; fi
fi

if [[ $need_ui -eq 1 ]]; then
	want=$(sed -n 's|^ARG NODE_IMAGE=.*/node:\([0-9][0-9.]*\)-.*|\1|p' "$repo/Dockerfile")
	if path=$(command -v node); then
		check_exact node "$want" "$(first node --version | sed 's/^v//')" "$path"
	else
		fail "node: not on PATH (need $want, the Dockerfile's NODE_IMAGE)"
	fi
	want=$(sed -n 's/.*"packageManager": *"pnpm@\([^"+]*\).*/\1/p' "$repo/ui/package.json")
	if path=$(command -v pnpm); then
		got=$(cd "$repo/ui" && COREPACK_ENABLE_DOWNLOAD_PROMPT=0 pnpm --version </dev/null 2>/dev/null | tail -n 1) || got=
		check_exact pnpm "$want" "$got" "$path"
	else
		fail "pnpm: not on PATH (need $want, ui/package.json packageManager: corepack)"
	fi
fi

if [[ $need_docker -eq 1 ]]; then
	if ! have docker; then
		fail "docker: not on PATH"
	elif ! docker version --format '{{.Server.Version}}' >/dev/null 2>&1; then
		fail "docker: the daemon is not reachable ($(first docker version --format '{{.Server.Version}}'))"
	else
		ok "docker daemon reachable"
		if docker buildx version >/dev/null 2>&1; then ok "docker buildx present"; else fail "docker buildx: not available (make image needs BuildKit)"; fi
	fi
fi

if [[ $need_kind -eq 1 ]]; then
	check_lock_tool kind
	check_lock_tool kubectl
	# inotify (kind known issue "too many open files").
	if [[ $linux -eq 1 && -r /proc/sys/fs/inotify/max_user_watches ]]; then
		for pair in max_user_watches=524288 max_user_instances=512; do
			key=${pair%=*} min=${pair#*=}
			cur=$(cat "/proc/sys/fs/inotify/$key")
			if [[ $cur -lt $min && $fix -eq 1 ]]; then
				if sudo -n true 2>/dev/null && sudo -n sysctl -q -w "fs.inotify.$key=$min" >/dev/null 2>&1; then
					echo "fixed fs.inotify.$key: $cur -> $min (sudo sysctl -w)"
				fi
				cur=$(cat "/proc/sys/fs/inotify/$key")
			fi
			if [[ $cur -ge $min ]]; then
				ok "fs.inotify.$key=$cur (>= $min)"
			else
				fail "fs.inotify.$key=$cur is below $min (kind 'too many open files'; rerun with --fix, needs passwordless sudo)"
			fi
		done
		if have lsmod; then
			mods=$(lsmod | awk 'NR > 1 && $1 ~ /^(nfnetlink_queue|nft_queue|nf_tables|br_netfilter)$/ { print $1 }' | sort | paste -s -d' ' -)
			fact netfilter_mods "${mods:-none loaded}"
		fi
	else
		fact inotify "n/a (not Linux: Docker Desktop's VM has its own limits)"
	fi
	# Host ports published by hack/kind (8443 Patrol UI/API, 5001 registry).
	existing=$(kind get clusters 2>/dev/null | grep -c '^patrol-' || true)
	for port in 8443 5001; do
		busy=unknown
		if have ss; then
			if ss -ltnH 2>/dev/null | awk -v p=":$port" '{ if (substr($4, length($4) - length(p) + 1) == p) f = 1 } END { exit !f }'; then busy=yes; else busy=no; fi
		elif have lsof; then
			if lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1; then busy=yes; else busy=no; fi
		elif [[ -r /proc/net/tcp ]]; then
			# local_address is IP:PORT in hex, state 0A is LISTEN.
			hex=$(printf ':%04X' "$port")
			tables=(/proc/net/tcp)
			[[ ! -r /proc/net/tcp6 ]] || tables+=(/proc/net/tcp6)
			if awk -v p="$hex" '$4 == "0A" && substr($2, length($2) - 4) == p { f = 1 } END { exit !f }' "${tables[@]}"; then busy=yes; else busy=no; fi
		fi
		case $busy in
		no) ok "port $port free" ;;
		unknown) warn "port $port: cannot check (no ss or lsof)" ;;
		yes)
			if [[ $existing -gt 0 ]]; then
				warn "port $port in use; patrol-* kind clusters exist, so it is probably theirs (hack/kind/up.sh reuses them)"
			else
				fail "port $port is in use; hack/kind publishes 127.0.0.1:$port"
			fi
			;;
		esac
	done
	# Free disk on / (images, three kind nodes, BuildKit cache).
	avail_kb=$(df -Pk / | awk 'NR == 2 { print $4 }')
	avail_gib=$(awk -v k="$avail_kb" 'BEGIN { printf "%.1f", k / 1048576 }')
	if [[ $avail_kb -ge $((20 * 1048576)) ]]; then
		ok "disk: ${avail_gib} GiB free on / (>= 20 GiB)"
	elif [[ $warn_disk -eq 1 ]]; then
		warn "disk: only ${avail_gib} GiB free on / (kind job wants >= 20 GiB; hack/ci/free-disk.sh)"
	else
		fail "disk: only ${avail_gib} GiB free on /, need >= 20 GiB (hack/ci/free-disk.sh, or --warn-disk)"
	fi
fi

echo "preflight: ${profiles[*]}: $fails problem(s), $warns warning(s)"
[[ $fails -eq 0 ]]
