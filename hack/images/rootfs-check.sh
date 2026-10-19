#!/usr/bin/env bash
# Structural check of the patrol-cd runtime image without a container runtime.
#
# Assembles the final stage's filesystem in a scratch directory from the same inputs the
# Dockerfile uses (hack/images/rootfs-setup.sh on a minimal Alpine-like /etc, the pinned tools
# from hack/images/fetch-tools.sh, cross-compiled linux binaries, hack/images/link-aliases.sh) and
# compares it with hack/images/rootfs-expected.txt. It also cross-checks the layout against the
# code: the alias list against binaryAliases in internal/server/commands.go, the mount points
# against the default paths compiled into internal/, the binaries' ELF architecture and Go build
# settings, the bundled tool versions against hack/images/checksums.txt, and the connect-proxy
# build command shipped in its BUILD note against the Dockerfile's connect stage.
#
# Not simulated (reported as such): Alpine packages (apk add), the connect-proxy compilation
# (a stub stands in for the binary) and ownership changes (recorded by rootfs-setup.sh --no-chown;
# files the Dockerfile COPYs are owned by 0:0, BuildKit's default).
#
# Usage: hack/images/rootfs-check.sh [--arch amd64|arm64] [--bin-dir DIR] [--tools-dir DIR]
#                                    [--cache DIR] [--work DIR] [--keep]
#   --arch       target architecture (default: amd64)
#   --bin-dir    use DIR/patrolcd and DIR/patrol (linux/<arch>) instead of building them
#   --tools-dir  use a fetch-tools.sh --out directory instead of downloading
#   --cache      download cache for fetch-tools.sh (default: .cache/image-tools)
#   --work       scratch directory (default: a new temporary directory)
#   --keep       keep the scratch directory
#
# "check && pass || fail" is intended below: pass only prints, so fail runs exactly when the
# check fails.
# shellcheck disable=SC2015
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo=$(cd "$here/../.." && pwd)
arch=amd64
bin_dir=
tools_dir=
cache=$repo/.cache/image-tools
work=
keep=0

usage() { sed -n '2,23s/^# \{0,1\}//p' "${BASH_SOURCE[0]}"; }
while [[ $# -gt 0 ]]; do
	case $1 in
	--arch) arch=${2:?}; shift 2 ;;
	--bin-dir) bin_dir=${2:?}; shift 2 ;;
	--tools-dir) tools_dir=${2:?}; shift 2 ;;
	--cache) cache=${2:?}; shift 2 ;;
	--work) work=${2:?}; shift 2 ;;
	--keep) keep=1; shift ;;
	-h | --help) usage; exit 0 ;;
	*) echo "rootfs-check: unknown argument $1 (see --help)" >&2; exit 2 ;;
	esac
done
case $arch in
amd64) elf_arch='x86-64' ;;
arm64) elf_arch='ARM aarch64' ;;
*) echo "rootfs-check: --arch must be amd64 or arm64" >&2; exit 2 ;;
esac

if [[ -z $work ]]; then
	work=$(mktemp -d "${TMPDIR:-/tmp}/patrol-rootfs.XXXXXX")
else
	mkdir -p "$work"
fi
if [[ $keep -eq 0 ]]; then
	trap 'rm -rf "$work"' EXIT
fi
root=$work/rootfs
rm -rf "$root"
mkdir -p "$root"

failures=0
pass() { printf 'PASS  %s\n' "$*"; }
fail() { printf 'FAIL  %s\n' "$*"; failures=$((failures + 1)); }
note() { printf 'NOTE  %s\n' "$*"; }

if stat --version >/dev/null 2>&1; then
	mode_of() { stat -c %a "$1"; }
else
	# BSD stat: %p is the full st_mode in octal (e.g. 41777); keep the permission digits.
	mode_of() {
		local m
		m=$(stat -f %p "$1")
		printf '%s' "${m: -4}"
	}
fi
pad4() {
	local m=$1
	while [[ ${#m} -lt 4 ]]; do m=0$m; done
	printf '%s' "$m"
}

# --- inputs ------------------------------------------------------------------------------------
if [[ -z $bin_dir ]]; then
	bin_dir=$work/bin
	note "building linux/$arch binaries (CGO_ENABLED=0, -trimpath; without the web UI build)"
	for cmd in patrolcd patrol; do
		(cd "$repo" && CGO_ENABLED=0 GOOS=linux GOARCH=$arch GOFLAGS=-mod=readonly \
			go build -trimpath -buildvcs=false -ldflags '-s -w' -o "$bin_dir/$cmd" "./cmd/$cmd")
	done
fi
for cmd in patrolcd patrol; do
	[[ -f $bin_dir/$cmd ]] || { echo "rootfs-check: $bin_dir/$cmd not found" >&2; exit 2; }
done
if [[ -z $tools_dir ]]; then
	tools_dir=$work/tools
	"$here/fetch-tools.sh" --platform "linux/$arch" --out "$tools_dir" --cache "$cache"
fi

# --- simulated base image -------------------------------------------------------------------------
# The parts of alpine:3.24 that rootfs-setup.sh reads or changes: account files (with Alpine's
# "ping" group holding gid 999), /tmp, the package manager and tini locations, a package-default
# ssh_config, and a setuid program that must lose its bit.
mkdir -p "$root/etc/ssh" "$root/sbin" "$root/usr/bin" "$root/usr/local/bin" "$root/tmp"
cat >"$root/etc/passwd" <<'EOF'
root:x:0:0:root:/root:/bin/sh
bin:x:1:1:bin:/bin:/sbin/nologin
daemon:x:2:2:daemon:/sbin:/sbin/nologin
nobody:x:65534:65534:nobody:/:/sbin/nologin
EOF
cat >"$root/etc/group" <<'EOF'
root:x:0:root
bin:x:1:root,bin,daemon
shadow:x:42:
ping:x:999:
nogroup:x:65533:
nobody:x:65534:
EOF
cat >"$root/etc/shadow" <<'EOF'
root:*::0:::::
bin:!::0:::::
daemon:!::0:::::
nobody:!::0:::::
EOF
printf '# package default\n' >"$root/etc/ssh/ssh_config"
printf '#!/bin/sh\n# stub of /sbin/apk\n' >"$root/sbin/apk"
printf '#!/bin/sh\n# stub of /sbin/tini (Alpine package tini)\n' >"$root/sbin/tini"
printf '#!/bin/sh\n# setuid stub\n' >"$root/usr/bin/suid-stub"
chmod 0755 "$root/sbin/apk" "$root/sbin/tini" "$root/tmp"
chmod 4755 "$root/usr/bin/suid-stub"

# --- Dockerfile runtime stage: RUN rootfs-setup.sh (twice: it must be idempotent) ----------------
chown_log=$work/chown.log
: >"$chown_log"
snapshot() {
	(cd "$root" && find . -print | LC_ALL=C sort | while IFS= read -r p; do
		printf '%s %s %s\n' "$p" "$(mode_of "$p")" "$(if [[ -L $p ]]; then readlink "$p"; fi)"
	done
	cat etc/passwd etc/group etc/shadow etc/gitconfig etc/ssh/ssh_config) 2>/dev/null
}
if sh "$here/rootfs-setup.sh" --root "$root" --config-dir "$here" --no-chown "$chown_log" 2>"$work/setup1.log"; then
	pass "rootfs-setup.sh completed on the simulated base"
else
	fail "rootfs-setup.sh failed: $(cat "$work/setup1.log")"
fi
first=$(snapshot)
if sh "$here/rootfs-setup.sh" --root "$root" --config-dir "$here" --no-chown "$work/chown2.log" 2>"$work/setup2.log" &&
	[[ "$(snapshot)" == "$first" ]]; then
	pass "rootfs-setup.sh is idempotent (second run changed nothing)"
else
	fail "second rootfs-setup.sh run failed or changed the filesystem"
fi

# --- Dockerfile build stage + COPY instructions ----------------------------------------------------
out=$work/buildout/bin
mkdir -p "$out"
cp "$bin_dir/patrolcd" "$bin_dir/patrol" "$out/"
chmod 0755 "$out/patrolcd" "$out/patrol"
sh "$here/link-aliases.sh" "$out" "$here/aliases.txt" 2>/dev/null
for t in helm kustomize git-lfs; do
	install -m 0755 "$tools_dir/bin/$t" "$root/usr/local/bin/$t"
done
printf '#!/bin/sh\n# stub: compiled in the Dockerfile connect stage\n' >"$root/usr/local/bin/connect-proxy"
chmod 0755 "$root/usr/local/bin/connect-proxy"
mkdir -p "$root/usr/share/licenses/patrol-cd" "$root/usr/share/patrol-cd"
cp -R "$tools_dir/licenses/." "$root/usr/share/licenses/"
install -m 0644 "$here/licenses/GPL-2.0.txt" "$root/usr/share/licenses/connect-proxy/COPYING"
install -m 0644 "$here/licenses/connect-proxy-BUILD.txt" "$root/usr/share/licenses/connect-proxy/BUILD"
cp "$tools_dir/share/tools.lock" "$root/usr/share/patrol-cd/tools.lock"
install -m 0644 "$repo/LICENSE" "$repo/NOTICE" "$root/usr/share/licenses/patrol-cd/"
cp -RP "$out/." "$root/usr/local/bin/"
note "connect-proxy is a stub here (the binary is compiled in the connect stage); Alpine packages are not installed"

# --- expected layout -----------------------------------------------------------------------------
owner_of() { # last --no-chown record wins; COPY'd files are 0:0
	local o
	o=$(awk -v p="$1" '$2 == p { o = $1 } END { print o }' "$chown_log")
	printf '%s' "${o:-0:0}"
}
checked=0
while read -r type mode owner path target; do
	case $type in '' | '#'*) continue ;; esac
	checked=$((checked + 1))
	f=$root$path
	case $type in
	absent)
		if [[ -e $f || -L $f ]]; then fail "$path should not exist"; else pass "$path absent"; fi
		continue
		;;
	l)
		if [[ ! -L $f ]]; then fail "$path: not a symbolic link"; continue; fi
		[[ $(readlink "$f") == "$target" ]] || { fail "$path -> $(readlink "$f"), expected $target"; continue; }
		;;
	d) [[ -d $f && ! -L $f ]] || { fail "$path: not a directory"; continue; } ;;
	f | x) [[ -f $f && ! -L $f ]] || { fail "$path: not a regular file"; continue; } ;;
	*) fail "rootfs-expected.txt: unknown type $type"; continue ;;
	esac
	if [[ $type != l ]]; then
		got=$(pad4 "$(mode_of "$f")")
		[[ $got == "$mode" ]] || { fail "$path: mode $got, expected $mode"; continue; }
	fi
	got_owner=$(owner_of "$path")
	[[ $got_owner == "$owner" ]] || { fail "$path: owner $got_owner, expected $owner"; continue; }
	pass "$type $mode $owner $path${target:+ -> $target}"
done <"$here/rootfs-expected.txt"
note "$checked layout entries checked"

# Every link inside /usr/local/bin must resolve to an executable inside the image.
failures_before=$failures
nlinks=0
while IFS= read -r l; do
	nlinks=$((nlinks + 1))
	t=$(readlink "$l")
	case $t in /*) resolved=$root$t ;; *) resolved=$(dirname "$l")/$t ;; esac
	if [[ -x $resolved && -f $resolved ]]; then :; else fail "${l#"$root"} -> $t does not resolve to an executable"; fi
done < <(find "$root/usr/local/bin" "$root/usr/bin" -type l)
if [[ $failures -eq $failures_before ]]; then
	pass "all $nlinks symbolic links in /usr/local/bin and /usr/bin resolve inside the image"
fi

# connect-proxy is GPL-2.0-or-later: the Dockerfile ships the GPL v2 text and the BUILD note next
# to its source, and the gcc command in BUILD is the connect stage's command, verbatim.
lic=$root/usr/share/licenses/connect-proxy
grep -qxF 'COPY --chmod=0644 hack/images/licenses/GPL-2.0.txt /usr/share/licenses/connect-proxy/COPYING' "$repo/Dockerfile" &&
	grep -qxF 'COPY --chmod=0644 hack/images/licenses/connect-proxy-BUILD.txt /usr/share/licenses/connect-proxy/BUILD' "$repo/Dockerfile" &&
	pass "Dockerfile installs connect-proxy's COPYING and BUILD as simulated here" ||
	fail "Dockerfile does not COPY hack/images/licenses/{GPL-2.0.txt,connect-proxy-BUILD.txt} to /usr/share/licenses/connect-proxy/{COPYING,BUILD}"
grep -qx ' *GNU GENERAL PUBLIC LICENSE' "$lic/COPYING" && grep -qx ' *Version 2, June 1991' "$lic/COPYING" &&
	grep -q 'TERMS AND CONDITIONS FOR COPYING, DISTRIBUTION AND MODIFICATION' "$lic/COPYING" &&
	pass "connect-proxy COPYING is the GNU GPL version 2 text" || fail "connect-proxy COPYING is not the GNU GPL version 2 text"
# The connect stage's RUN lines with continuations joined; the compiler call is "gcc -...".
df_gcc=$(awk '/^FROM / { s = / AS connect$/ } s { if (sub(/\\$/, "")) { buf = buf $0 } else { print buf $0; buf = "" } }' "$repo/Dockerfile" |
	grep -oE 'gcc -[^;]+' | tr '\t' ' ' | tr -s ' ' | sed 's/ $//' || true)
build_gcc=$(grep -E '^gcc ' "$lic/BUILD" || true)
if [[ -n $df_gcc && $(wc -l <<<"$df_gcc") -eq 1 && $df_gcc == "$build_gcc" ]]; then
	pass "connect-proxy BUILD records the Dockerfile's command: $df_gcc"
else
	fail "connect-proxy BUILD command '$build_gcc' differs from the Dockerfile connect stage's '$df_gcc'"
fi

# --- accounts ------------------------------------------------------------------------------------
grep -qx 'patrol:x:999:999:Patrol CD:/home/patrol:/sbin/nologin' "$root/etc/passwd" && pass "passwd: patrol 999:999 /home/patrol /sbin/nologin" || fail "passwd entry for patrol missing or wrong"
grep -qx 'patrol:x:999:' "$root/etc/group" && pass "group: patrol 999" || fail "group entry for patrol missing or wrong"
grep -q '^ping:' "$root/etc/group" && fail "group ping (gid 999) still present" || pass "group ping (Alpine gid 999) removed"
grep -qx 'patrol:!::0:::::' "$root/etc/shadow" && pass "shadow: patrol locked" || fail "shadow entry for patrol missing or wrong"
[[ $(grep -c '^patrol:' "$root/etc/passwd") -eq 1 && $(grep -c '^patrol:' "$root/etc/group") -eq 1 ]] && pass "no duplicate account entries" || fail "duplicate patrol account entries"
grep -q '^USER 999:999$' "$repo/Dockerfile" && pass "Dockerfile USER 999:999 matches passwd" || fail "Dockerfile USER is not 999:999"
grep -q '^ENV HOME=/home/patrol' "$repo/Dockerfile" && pass "Dockerfile HOME matches passwd" || fail "Dockerfile HOME does not match /home/patrol"
grep -q '^ENTRYPOINT \["/sbin/tini", "--"\]$' "$repo/Dockerfile" && [[ -e $root/sbin/tini ]] && pass "ENTRYPOINT /sbin/tini (path of Alpine's tini package)" || fail "ENTRYPOINT does not use /sbin/tini"

# --- privileges ----------------------------------------------------------------------------------
suid=$(find "$root" -type f \( -perm -4000 -o -perm -2000 \) | sed "s#^$root##")
[[ -z $suid ]] && pass "no setuid/setgid files (stub's bit was removed)" || fail "setuid/setgid files left: $suid"

# --- code cross-checks ---------------------------------------------------------------------------
go_aliases=$(awk '/^var binaryAliases = map\[string\]string\{/ { f = 1; next } f && /^\}/ { exit }
	f && $0 !~ /^[[:space:]]*\/\// { gsub(/[",[:space:]]/, ""); split($0, a, ":"); if (a[1] != "") print a[1] }' \
	"$repo/internal/server/commands.go" | LC_ALL=C sort)
img_aliases=$(grep -v -e '^#' -e '^$' "$here/aliases.txt" | LC_ALL=C sort)
if [[ -n $go_aliases && $go_aliases == "$img_aliases" ]]; then
	pass "aliases.txt equals binaryAliases in internal/server/commands.go ($(wc -l <<<"$go_aliases" | tr -d ' ') names)"
else
	fail "aliases.txt differs from binaryAliases: $(diff <(echo "$go_aliases") <(echo "$img_aliases") | tr '\n' ' ')"
fi

# Default paths compiled into internal/ under /app/config, /home/patrol and /shared must have their
# directory in the image. /shared/download is optional by design: without it the server serves the
# patrol binary from PATH (OPS-112), and operators mount other CLI builds there.
npaths=0
while IFS= read -r p; do
	npaths=$((npaths + 1))
	case $p in
	*'*'*) continue ;; # glob patterns, not paths
	*/) dir=${p%/} ;;
	*/ssh_known_hosts | */token | */*.*) dir=$(dirname "$p") ;;
	*) dir=$p ;;
	esac
	case $dir in /shared/download) note "default path $p: optional, not in the image"; continue ;; esac
	if [[ -d $root$dir ]]; then pass "default path $p: $dir exists"; else fail "default path $p: $dir missing in the image"; fi
done < <(grep -rhoE '"/(app/config|home/patrol|shared)/[^"]*"' --include='*.go' --exclude='*_test.go' \
	"$repo/internal/" "$repo/cmd/" | tr -d '"' | LC_ALL=C sort -u)
[[ $npaths -gt 0 ]] || fail "no default paths found under internal/ and cmd/ (search failed)"

# Binaries: architecture, static linking, Go build settings, pinned versions.
if command -v file >/dev/null 2>&1; then
	for b in patrolcd patrol helm kustomize git-lfs; do
		desc=$(file -b "$root/usr/local/bin/$b")
		if [[ $desc == *ELF*"$elf_arch"* && $desc == *"statically linked"* ]]; then
			pass "$b: ELF $elf_arch, statically linked"
		else
			fail "$b: unexpected binary type: $desc"
		fi
	done
else
	note "file(1) not installed: ELF checks skipped"
fi
version_of() { awk -v n="$1" -v p="linux/$arch" '$1 == n && $3 == p { print $2 }' "$here/checksums.txt"; }
if command -v go >/dev/null 2>&1; then
	for b in patrolcd patrol; do
		info=$(go version -m "$root/usr/local/bin/$b")
		if grep -q 'CGO_ENABLED=0' <<<"$info" && grep -q -- '-trimpath=true' <<<"$info" &&
			grep -q 'GOOS=linux' <<<"$info" && grep -q "GOARCH=$arch" <<<"$info"; then
			pass "$b: CGO_ENABLED=0, -trimpath, linux/$arch ($(head -1 <<<"$info" | awk '{print $2}'))"
		else
			fail "$b: unexpected Go build settings"
		fi
	done
	v=$(version_of helm)
	go version -m "$root/usr/local/bin/helm" | grep -qE "mod[[:space:]]+helm.sh/helm/v4[[:space:]]+v$v" &&
		pass "helm reports module helm.sh/helm/v4 v$v" || fail "helm is not v$v"
	v=$(version_of kustomize)
	go version -m "$root/usr/local/bin/kustomize" | grep -q "provenance.version=v$v" &&
		pass "kustomize built with provenance.version=v$v" || fail "kustomize is not v$v"
else
	note "go not installed: Go build setting checks skipped"
fi
v=$(version_of git-lfs)
LC_ALL=C grep -aq "${v//./\\.}" "$root/usr/local/bin/git-lfs" && pass "git-lfs contains version string $v" || fail "git-lfs does not contain $v"
for t in helm kustomize git-lfs; do
	grep -q "^$t $(version_of "$t") linux/$arch " "$root/usr/share/patrol-cd/tools.lock" &&
		pass "tools.lock records $t $(version_of "$t") linux/$arch" || fail "tools.lock lacks $t"
done

# Commands the code runs must come from the image: tools above, packages below.
for pkg in git openssh-client gpg tini ca-certificates tzdata; do
	grep -qx "$pkg" "$here/apk-packages.txt" && pass "apk-packages.txt lists $pkg" || fail "apk-packages.txt lacks $pkg"
done

echo
if [[ $failures -gt 0 ]]; then
	echo "rootfs-check: $failures failure(s) (linux/$arch; scratch: $work)"
	[[ $keep -eq 1 ]] || echo "rerun with --keep to inspect the scratch directory"
	exit 1
fi
if [[ $keep -eq 1 ]]; then
	echo "rootfs-check: OK (linux/$arch; scratch kept: $work)"
else
	echo "rootfs-check: OK (linux/$arch)"
fi
