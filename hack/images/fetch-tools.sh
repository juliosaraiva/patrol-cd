#!/bin/sh
# Downloads the third-party files bundled in the patrol-cd image (helm, kustomize, git-lfs,
# license texts, connect-proxy source) and verifies each against the SHA-256 pinned in
# hack/images/checksums.txt before anything is extracted or installed.
#
# Usage: hack/images/fetch-tools.sh --platform linux/<amd64|arm64> --out DIR
#                                   [--checksums FILE] [--cache DIR] [--only NAME[,NAME...]]
#
#   --platform   target platform; lines for that platform and for "any" are used
#   --out        output directory (created); files land at the DEST paths of column 6, and
#                DIR/share/tools.lock lists the pinned lines that were installed
#   --checksums  pin file (default: checksums.txt next to this script)
#   --cache      content-addressed download cache (DIR/<sha256>); reused when the sum matches
#   --only       restrict to these tool names
#
# Runs in the Dockerfile "tools" stage (busybox sh, curl) and on workstations (macOS or Linux;
# curl or wget, sha256sum or shasum, tar). Archives are listed first and refused when any entry
# is absolute or contains a ".." component; only the named members are extracted, and each must
# be a regular file.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
checksums=$here/checksums.txt
platform=
out=
cache=
only=

die() {
	printf 'fetch-tools: %s\n' "$*" >&2
	exit 1
}
log() { printf 'fetch-tools: %s\n' "$*" >&2; }

while [ $# -gt 0 ]; do
	case $1 in
	--platform) platform=${2:?--platform needs a value}; shift 2 ;;
	--out) out=${2:?--out needs a value}; shift 2 ;;
	--checksums) checksums=${2:?--checksums needs a value}; shift 2 ;;
	--cache) cache=${2:?--cache needs a value}; shift 2 ;;
	--only) only=${2:?--only needs a value}; shift 2 ;;
	-h | --help)
		sed -n '2,20s/^# \{0,1\}//p' "$0"
		exit 0
		;;
	*) die "unknown argument: $1 (see --help)" ;;
	esac
done

case $platform in
linux/amd64 | linux/arm64) ;;
'') die "--platform is required (linux/amd64 or linux/arm64)" ;;
*) die "unsupported platform: $platform (linux/amd64 or linux/arm64)" ;;
esac
[ -n "$out" ] || die "--out is required"
[ -f "$checksums" ] || die "checksum file not found: $checksums"

if command -v sha256sum >/dev/null 2>&1; then
	sha256() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
	sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
	die "need sha256sum or shasum"
fi

if command -v curl >/dev/null 2>&1; then
	download() {
		curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' \
			--tlsv1.2 --retry 3 --retry-delay 2 --connect-timeout 30 --max-time 600 -o "$2" "$1"
	}
elif command -v wget >/dev/null 2>&1; then
	download() { wget -q -T 60 -O "$2" "$1"; }
else
	die "need curl or wget"
fi

mkdir -p "$out"
out=$(cd "$out" && pwd)
[ -z "$cache" ] || { mkdir -p "$cache" && cache=$(cd "$cache" && pwd); }
work=$(mktemp -d "${TMPDIR:-/tmp}/patrol-fetch.XXXXXX")
trap 'rm -rf "$work"' EXIT
trap 'exit 130' INT TERM
mkdir -p "$out/share"
lock=$out/share/tools.lock
: >"$lock"

# safe_member NAME: archive member names must be relative and free of ".." components.
safe_member() {
	case /$1/ in
	*/../* | //*) return 1 ;;
	esac
	return 0
}

# fetch URL SHA FILE: download (or reuse the cache) and verify the sum.
fetch() {
	if [ -n "$cache" ] && [ -f "$cache/$2" ] && [ "$(sha256 "$cache/$2")" = "$2" ]; then
		cp "$cache/$2" "$3"
		return 0
	fi
	download "$1" "$3" </dev/null || die "download failed: $1"
	got=$(sha256 "$3")
	if [ "$got" != "$2" ]; then
		rm -f "$3"
		die "SHA-256 mismatch for $1: expected $2, got $got"
	fi
	if [ -n "$cache" ]; then cp "$3" "$cache/$2"; fi
}

# install_one SRC DEST MODE FILE ISARCHIVE
install_one() {
	src=$1 dest=$2 mode=$3 file=$4
	case $dest in
	/* | *..*) die "unsafe destination path: $dest" ;;
	esac
	case $mode in
	0[0-7][0-7][0-7]) ;;
	*) die "invalid mode $mode for $dest" ;;
	esac
	mkdir -p "$out/$(dirname "$dest")"
	if [ "$src" = . ]; then
		cp "$file" "$out/$dest"
	else
		safe_member "$src" || die "unsafe archive member name: $src"
		grep -qxF "$src" "$work/members" || die "member $src not found in $(basename "$file")"
		x=$work/x.$$
		rm -rf "$x" && mkdir "$x"
		tar -xzf "$file" -C "$x" "$src"
		if [ -L "$x/$src" ] || [ ! -f "$x/$src" ]; then
			die "archive member $src is not a regular file"
		fi
		cp "$x/$src" "$out/$dest"
		rm -rf "$x"
	fi
	chmod "$mode" "$out/$dest"
}

found=0
set -f
while read -r name version plat sum url specs sums rest; do
	case $name in '' | '#'*) continue ;; esac
	[ -n "$sums" ] && [ -z "$rest" ] || die "malformed line for $name (expected 7 columns)"
	[ "$plat" = "$platform" ] || [ "$plat" = any ] || continue
	if [ -n "$only" ]; then
		case ,$only, in *,"$name",*) ;; *) continue ;; esac
	fi
	case $sum in
	*[!0-9a-f]*) die "$name: sha256 must be lowercase hex" ;;
	esac
	[ ${#sum} -eq 64 ] || die "$name: sha256 must have 64 hex digits"
	case $url in https://*) ;; *) die "$name: URL must use https: $url" ;; esac

	file=$work/$sum
	fetch "$url" "$sum" "$file"
	archive=0
	case $url in *.tar.gz | *.tgz) archive=1 ;; esac
	if [ $archive -eq 1 ]; then
		tar -tzf "$file" >"$work/members" || die "cannot list archive $url"
		while IFS= read -r m; do
			safe_member "$m" || die "archive $url contains an unsafe entry: $m"
		done <"$work/members"
	fi
	oldifs=$IFS
	IFS=,
	for spec in $specs; do
		IFS=$oldifs
		src=${spec%%:*}
		rest_spec=${spec#*:}
		dest=${rest_spec%%:*}
		mode=${rest_spec#*:}
		[ -n "$src" ] && [ -n "$dest" ] && [ "$mode" != "$rest_spec" ] || die "$name: malformed install spec: $spec"
		if [ "$src" != . ] && [ $archive -eq 0 ]; then
			die "$name: archive member $src given for a non-archive URL"
		fi
		install_one "$src" "$dest" "$mode" "$file"
		log "$name $version ($plat) -> $dest"
	done
	IFS=$oldifs
	printf '%s %s %s %s %s\n' "$name" "$version" "$plat" "$sum" "$url" >>"$lock"
	found=$((found + 1))
done <"$checksums"
set +f

[ "$found" -gt 0 ] || die "no entries for $platform${only:+ (only: $only)} in $checksums"
log "installed $found pinned file(s) for $platform into $out"
