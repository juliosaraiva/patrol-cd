#!/usr/bin/env bash
# Validates every pin of the patrol-cd image.
#
#   offline (always):
#     - hack/images/checksums.txt: columns, names, platforms, lowercase SHA-256, https URLs that
#       contain the pinned version (or a commit for pinned-commit lines), install specs, both
#       architectures present for every binary tool
#     - Dockerfile: syntax frontend and base images pinned as name:tag@sha256:<digest>; the Go
#       image matches go.mod's go version, pnpm matches ui/package.json packageManager, all
#       Alpine-based images use the runtime's Alpine branch
#     - hack/images/apk-packages.txt: well-formed package names
#   online (unless --offline):
#     - downloads every pinned file again (fetch-tools.sh, both platforms, no cache unless
#       --cache) and verifies the SHA-256; archives are listed and extracted as in the image build
#     - re-checks each SHA-256 against the publisher's checksum file (column 7)
#     - base images: the pinned digest still exists in the registry (anonymous pull token), its
#       index covers linux/amd64 and linux/arm64, and whether the tag still points to it
#     - Alpine packages: every name exists for x86_64 and aarch64 in the runtime's branch
#
# Usage: hack/images/verify-pins.sh [--offline] [--skip-images] [--skip-apk] [--cache DIR]
# Needs: bash (3.2+), awk, curl (online), jq (base image checks), tar, sha256sum or shasum.
#
# "check && pass || fail" is intended below: pass only prints, so fail runs exactly when the
# check fails.
# shellcheck disable=SC2015
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo=$(cd "$here/../.." && pwd)
checksums=$here/checksums.txt
dockerfile=$repo/Dockerfile
offline=0
skip_images=0
skip_apk=0
cache=

while [[ $# -gt 0 ]]; do
	case $1 in
	--offline) offline=1; shift ;;
	--skip-images) skip_images=1; shift ;;
	--skip-apk) skip_apk=1; shift ;;
	--cache) cache=${2:?}; shift 2 ;;
	-h | --help) sed -n '2,21s/^# \{0,1\}//p' "${BASH_SOURCE[0]}"; exit 0 ;;
	*) echo "verify-pins: unknown argument $1 (see --help)" >&2; exit 2 ;;
	esac
done

failures=0
warnings=0
pass() { printf 'PASS  %s\n' "$*"; }
fail() { printf 'FAIL  %s\n' "$*"; failures=$((failures + 1)); }
warn() { printf 'WARN  %s\n' "$*"; warnings=$((warnings + 1)); }

work=$(mktemp -d "${TMPDIR:-/tmp}/patrol-pins.XXXXXX")
trap 'rm -rf "$work"' EXIT

fetch() { # URL FILE
	curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --tlsv1.2 \
		--retry 3 --retry-delay 2 --connect-timeout 30 --max-time 600 -o "$2" "$1"
}

# --- checksums.txt format -------------------------------------------------------------------------
seen=$work/seen
: >"$seen"
lines=0
while read -r name version plat sum url specs sums rest; do
	[[ -z $name || $name == \#* ]] && continue
	lines=$((lines + 1))
	where="checksums.txt: $name $plat"
	if [[ -z $sums || -n $rest ]]; then fail "$where: expected 7 columns"; continue; fi
	[[ $name =~ ^[a-z0-9][a-z0-9-]*$ ]] || fail "$where: invalid name"
	[[ $version =~ ^[0-9][0-9A-Za-z.+-]*$ ]] || fail "$where: invalid version $version"
	[[ $plat == linux/amd64 || $plat == linux/arm64 || $plat == any ]] || fail "$where: invalid platform"
	[[ $sum =~ ^[0-9a-f]{64}$ ]] || fail "$where: sha256 must be 64 lowercase hex digits"
	[[ $url == https://* ]] || fail "$where: URL must use https"
	case $sums in
	pinned-commit) [[ $url =~ /[0-9a-f]{40}/ ]] || fail "$where: pinned-commit URL must contain a full commit id" ;;
	pinned-tag) [[ $url == *"$version"* ]] || fail "$where: URL does not contain version $version" ;;
	https://*) [[ $url == *"$version"* ]] || fail "$where: URL does not contain version $version" ;;
	*) fail "$where: column 7 must be an https checksum URL, pinned-tag or pinned-commit" ;;
	esac
	IFS=, read -r -a parts <<<"$specs"
	for spec in "${parts[@]}"; do
		[[ $spec =~ ^[^:]+:[^:/][^:]*:0[0-7]{3}$ ]] || fail "$where: malformed install spec $spec"
		dest=${spec#*:}
		dest=${dest%:*}
		[[ $dest != *..* ]] || fail "$where: install destination escapes the output directory: $dest"
	done
	if grep -qxF "$name $plat" "$seen"; then fail "$where: duplicate entry"; fi
	printf '%s %s\n' "$name" "$plat" >>"$seen"
done <"$checksums"
[[ $lines -gt 0 ]] || fail "checksums.txt has no entries"
for tool in helm kustomize git-lfs; do
	for p in linux/amd64 linux/arm64; do
		grep -qxF "$tool $p" "$seen" || fail "checksums.txt: $tool has no $p entry"
	done
done
[[ $failures -eq 0 ]] && pass "checksums.txt: $lines entries well-formed (helm, kustomize, git-lfs for linux/amd64 and linux/arm64)"

# --- Dockerfile pins -------------------------------------------------------------------------------
syntax=$(sed -n '1s/^# syntax=//p' "$dockerfile")
[[ $syntax =~ ^docker/dockerfile:[0-9]+\.[0-9]+\.[0-9]+@sha256:[0-9a-f]{64}$ ]] &&
	pass "Dockerfile syntax frontend pinned: ${syntax%@*}" || fail "Dockerfile: first line must be '# syntax=docker/dockerfile:X.Y.Z@sha256:<digest>' (got '$syntax')"
images=$work/images
sed -n 's/^ARG \([A-Z_]*_IMAGE\)=\(.*\)$/\1=\2/p' "$dockerfile" >"$images"
image() { sed -n "s/^$1=//p" "$images"; }
while IFS='=' read -r key value; do
	if [[ $value =~ ^[a-z0-9.-]+/[a-z0-9._/-]+:[A-Za-z0-9._-]+@sha256:[0-9a-f]{64}$ ]]; then
		pass "Dockerfile $key pinned: $value"
	else
		fail "Dockerfile $key is not name:tag@sha256:<digest>: $value"
	fi
done <"$images"
for k in ALPINE_IMAGE GOLANG_IMAGE NODE_IMAGE; do
	[[ -n $(image "$k") ]] || fail "Dockerfile: ARG $k missing"
done
gomod=$(awk '$1 == "go" { print $2; exit }' "$repo/go.mod")
gotag=$(image GOLANG_IMAGE)
gotag=${gotag#*:}
gotag=${gotag%%-*}
[[ $gotag == "$gomod" ]] && pass "Go image $gotag matches go.mod (go $gomod)" || fail "Go image tag $gotag differs from go.mod go $gomod"
pm=$(sed -n 's/.*"packageManager": *"pnpm@\([^"+]*\).*/\1/p' "$repo/ui/package.json")
pnpm_arg=$(sed -n 's/^ARG PNPM_VERSION=//p' "$dockerfile")
[[ -n $pm && $pm == "$pnpm_arg" ]] && pass "pnpm $pnpm_arg matches ui/package.json packageManager" || fail "Dockerfile PNPM_VERSION '$pnpm_arg' differs from ui/package.json packageManager '$pm'"
[[ $(sed -n 's/^ARG PNPM_SHA512=//p' "$dockerfile") =~ ^[0-9a-f]{128}$ ]] && pass "PNPM_SHA512 is a SHA-512 hex digest" || fail "PNPM_SHA512 must be 128 lowercase hex digits"
alpine_image=$(image ALPINE_IMAGE)
alpine_ver=$alpine_image
alpine_ver=${alpine_ver#*:}
alpine_ver=${alpine_ver%@*}
alpine_branch=${alpine_ver%.*}
for k in GOLANG_IMAGE NODE_IMAGE; do
	[[ $(image "$k") == *"-alpine$alpine_branch@"* ]] && pass "$k uses Alpine $alpine_branch like the runtime" || warn "$k does not use Alpine $alpine_branch"
done
grep -q "org.opencontainers.image.base.digest=\"${alpine_image#*@}\"" "$dockerfile" &&
	pass "OCI base.digest label matches ALPINE_IMAGE" || fail "org.opencontainers.image.base.digest label differs from ALPINE_IMAGE"

# --- apk package names -----------------------------------------------------------------------------
pkgs=()
while read -r p _; do
	[[ -z $p || $p == \#* ]] || pkgs+=("$p")
done <"$here/apk-packages.txt"
# Build-stage packages named literally in the Dockerfile (tools: curl; connect: gcc, musl-dev).
build_pkgs=()
while read -r p; do
	[[ -n $p ]] && build_pkgs+=("$p")
done < <(sed -n 's/^RUN apk add --no-cache \([a-z0-9 +._-]*\)$/\1/p' "$dockerfile" | tr ' ' '\n')
bad=0
for p in "${pkgs[@]}"; do [[ $p =~ ^[a-z0-9][a-z0-9+._-]*$ ]] || { fail "apk-packages.txt: invalid name '$p'"; bad=1; }; done
[[ $bad -eq 0 ]] && pass "apk-packages.txt: ${#pkgs[@]} package names well-formed"
[[ ${#build_pkgs[@]} -gt 0 ]] && pass "Dockerfile build-stage packages: ${build_pkgs[*]}" || fail "no build-stage apk packages found in the Dockerfile"

if [[ $offline -eq 1 ]]; then
	echo
	echo "verify-pins: offline checks done: $failures failure(s), $warnings warning(s)"
	[[ $failures -eq 0 ]] || exit 1
	exit 0
fi
command -v curl >/dev/null || { echo "verify-pins: curl is required for online checks (or use --offline)" >&2; exit 2; }

# --- downloads + SHA-256 (exactly as the image build does) -----------------------------------------
for p in linux/amd64 linux/arm64; do
	if "$here/fetch-tools.sh" --platform "$p" --out "$work/out-${p#linux/}" ${cache:+--cache "$cache"} 2>"$work/fetch-${p#linux/}.log"; then
		pass "fetch-tools.sh $p: every pinned file downloaded, SHA-256 verified, archive members extracted"
	else
		fail "fetch-tools.sh $p failed: $(tail -1 "$work/fetch-${p#linux/}.log")"
	fi
done

# --- publisher checksum files ----------------------------------------------------------------------
sumfiles=$work/sumfiles
: >"$sumfiles"
n=0
while read -r name version plat sum url specs sums rest; do
	[[ -z $name || $name == \#* || $sums != https://* ]] && continue
	f=$(awk -v u="$sums" '$1 == u { print $2 }' "$sumfiles")
	if [[ -z $f ]]; then
		n=$((n + 1))
		f=$work/sums-$n
		fetch "$sums" "$f" </dev/null || { fail "cannot download $sums"; continue; }
		printf '%s %s\n' "$sums" "$f" >>"$sumfiles"
	fi
	file=${url##*/}
	file=${file//%2F//}
	published=$(awk -v f="$file" 'NF == 2 { n = $2; sub(/^\*/, "", n); if (n == f) { print $1; exit } }' "$f")
	if [[ $published == "$sum" ]]; then
		pass "$name $plat: SHA-256 equals the publisher's ($sums)"
	else
		fail "$name $plat: pinned $sum, publisher lists '${published:-nothing}' for $file in $sums"
	fi
done <"$checksums"

# --- base images -----------------------------------------------------------------------------------
if [[ $skip_images -eq 0 ]]; then
	if ! command -v jq >/dev/null; then
		warn "jq not installed: base image checks skipped"
	else
		accept='application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json'
		refs=()
		while IFS='=' read -r _ value; do refs+=("$value"); done <"$images"
		refs+=("docker.io/$syntax")
		for ref in "${refs[@]}"; do
			name=${ref%%@*}
			digest=${ref#*@}
			tag=${name##*:}
			repo_path=${name%:*}
			repo_path=${repo_path#docker.io/}
			[[ $repo_path == */* ]] || repo_path=library/$repo_path
			if [[ ${name%%/*} != docker.io && $ref != docker.io/* ]]; then
				warn "$ref: registry not checked (only docker.io is supported here)"
				continue
			fi
			fetch "https://auth.docker.io/token?service=registry.docker.io&scope=repository:$repo_path:pull" "$work/token.json" ||
				{ fail "$ref: cannot get an anonymous pull token"; continue; }
			token=$(jq -r .token "$work/token.json")
			rm -f "$work/token.json"
			if ! curl --fail --silent --show-error --proto '=https' -H "Authorization: Bearer $token" -H "Accept: $accept" \
				-o "$work/index.json" "https://registry-1.docker.io/v2/$repo_path/manifests/$digest"; then
				fail "$ref: pinned digest not found in the registry"
				continue
			fi
			platforms=$(jq -r '[.manifests[]? | select(.platform.os == "linux") | .platform.architecture] | unique | join(" ")' "$work/index.json")
			if [[ " $platforms " == *" amd64 "* && " $platforms " == *" arm64 "* ]]; then
				pass "$repo_path@${digest:0:19}…: exists, index covers linux/amd64 and linux/arm64"
			else
				fail "$ref: index lacks linux/amd64 or linux/arm64 (has: $platforms)"
			fi
			current=$(curl --fail --silent --show-error --proto '=https' --head -H "Authorization: Bearer $token" -H "Accept: $accept" \
				"https://registry-1.docker.io/v2/$repo_path/manifests/$tag" | tr -d '\r' | awk -F': ' 'tolower($1) == "docker-content-digest" { print $2 }')
			if [[ $current == "$digest" ]]; then
				pass "$repo_path:$tag still points to the pinned digest"
			else
				warn "$repo_path:$tag now points to ${current:-?} (pinned $digest): review and bump the pin"
			fi
		done
	fi
fi

# --- Alpine packages -------------------------------------------------------------------------------
if [[ $skip_apk -eq 0 ]]; then
	for arch in x86_64 aarch64; do
		: >"$work/index-$arch"
		for r in main community; do
			mkdir -p "$work/apk-$r-$arch"
			if fetch "https://dl-cdn.alpinelinux.org/alpine/v$alpine_branch/$r/$arch/APKINDEX.tar.gz" "$work/apk-$r-$arch/APKINDEX.tar.gz"; then
				tar -xzf "$work/apk-$r-$arch/APKINDEX.tar.gz" -C "$work/apk-$r-$arch" APKINDEX 2>/dev/null
				cat "$work/apk-$r-$arch/APKINDEX" >>"$work/index-$arch"
			else
				fail "cannot download the Alpine v$alpine_branch $r index for $arch"
			fi
		done
		for p in "${pkgs[@]}" "${build_pkgs[@]}"; do
			hit=$(awk -v want="$p" 'BEGIN { RS = ""; FS = "\n" }
				{ n = ""; v = ""; prov = ""
				  for (i = 1; i <= NF; i++) {
				    if ($i ~ /^P:/) n = substr($i, 3); else if ($i ~ /^V:/) v = substr($i, 3); else if ($i ~ /^p:/) prov = " " substr($i, 3) " " }
				  if (n == want || prov ~ (" " want "[= ]")) { print n "-" v; exit } }' "$work/index-$arch")
			if [[ -n $hit ]]; then pass "alpine v$alpine_branch $arch: $p -> $hit"; else fail "alpine v$alpine_branch $arch: package $p not found"; fi
		done
	done
fi

echo
echo "verify-pins: $failures failure(s), $warnings warning(s)"
[[ $failures -eq 0 ]]
