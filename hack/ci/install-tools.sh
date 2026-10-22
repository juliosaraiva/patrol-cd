#!/usr/bin/env bash
# Installs the command-line tools CI jobs need from hack/ci/tools.lock, for the host platform
# (Linux amd64 or arm64), and checks that every installed binary reports exactly the pinned
# version. Downloads go through hack/images/fetch-tools.sh, which verifies the SHA-256 before
# anything is extracted.
#
# Usage: hack/ci/install-tools.sh [--dest DIR] [--cache DIR] TOOL... | all
#        hack/ci/install-tools.sh --check [--cache DIR] [TOOL...]
#
#   TOOL     a name from column 1 of hack/ci/tools.lock, or "all" (all of them: helm, kind,
#            kubectl, golangci-lint, gitleaks, shellcheck, actionlint)
#   --dest   install directory (default ${RUNNER_TEMP:-${TMPDIR:-/tmp}}/patrol-ci-bin); the tools
#            land in DIR/bin. When GITHUB_PATH is set, DIR/bin is appended to it, so later steps
#            of the job find these tools first on PATH; otherwise the export line is printed.
#   --cache  content-addressed download cache handed to fetch-tools.sh (DIR/<sha256>)
#   --check  validate tools.lock instead of installing (any host, also macOS): format, the URL
#            contains the version, every line (both platforms) downloaded again into a temporary
#            directory with its SHA-256 verified and its archive member extracted, and the
#            SHA-256 compared with the publisher's checksum file of column 7 (github-asset-digest
#            lines: with the digest GitHub reports for the release asset, a WARN when the GitHub
#            API cannot be read)
#
# Output: one line per installed tool, "NAME VERSION SHA256" (the verified download), on stdout;
# progress on stderr. --check prints PASS/WARN/FAIL lines and a summary.
# Exit status: 0 success, 1 a download, checksum or version check failed, 2 usage error or an
# unsupported host (installing needs Linux amd64/arm64: this is for CI runners and containers).
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo=$(cd "$here/../.." && pwd)
lock=$here/tools.lock
fetch_tools=$repo/hack/images/fetch-tools.sh

usage() { awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "${BASH_SOURCE[0]}"; }
die() {
	printf 'install-tools: %s\n' "$*" >&2
	exit 1
}
usage_error() {
	printf 'install-tools: %s (see --help)\n' "$*" >&2
	exit 2
}
log() { printf 'install-tools: %s\n' "$*" >&2; }

dest=${RUNNER_TEMP:-${TMPDIR:-/tmp}}
dest=${dest%/}/patrol-ci-bin
cache=
check=0
tools=()
while [[ $# -gt 0 ]]; do
	case $1 in
	--dest)
		[[ $# -ge 2 && -n $2 ]] || usage_error "--dest needs a value"
		dest=$2
		shift 2
		;;
	--cache)
		[[ $# -ge 2 && -n $2 ]] || usage_error "--cache needs a value"
		cache=$2
		shift 2
		;;
	--check)
		check=1
		shift
		;;
	-h | --help)
		usage
		exit 0
		;;
	-*) usage_error "unknown option: $1" ;;
	*)
		tools+=("$1")
		shift
		;;
	esac
done

[[ -f $lock ]] || die "lock file not found: $lock"
[[ -x $fetch_tools ]] || die "not executable: $fetch_tools"

# Names in tools.lock, in file order, without duplicates.
known=$(awk '$1 !~ /^#/ && NF > 0 && !seen[$1]++ { print $1 }' "$lock")

# Expand "all" and validate the names; the result is a comma-separated list for --only.
selected=
if [[ ${#tools[@]} -gt 0 ]]; then
	for t in "${tools[@]}"; do
		if [[ $t == all ]]; then
			selected=$(printf '%s\n' "$known" | paste -s -d, -)
			break
		fi
		printf '%s\n' "$known" | grep -xF -- "$t" >/dev/null || usage_error "unknown tool '$t' (known: $(printf '%s\n' "$known" | paste -s -d' ' -))"
		case ,$selected, in *,"$t",*) ;; *) selected=${selected:+$selected,}$t ;; esac
	done
fi

# lock_field NAME PLATFORM COLUMN: one column of the tools.lock line of NAME for PLATFORM.
lock_field() {
	awk -v n="$1" -v p="$2" -v c="$3" '$1 == n && $3 == p { print $c; exit }' "$lock"
}

if command -v sha256sum >/dev/null 2>&1; then
	sha256() { sha256sum "$1" | cut -d' ' -f1; }
else
	sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
fi

# --- --check ---------------------------------------------------------------------------------------
if [[ $check -eq 1 ]]; then
	command -v curl >/dev/null 2>&1 || die "--check needs curl"
	failures=0
	warnings=0
	pass() { printf 'PASS  %s\n' "$*"; }
	fail() {
		printf 'FAIL  %s\n' "$*"
		failures=$((failures + 1))
	}
	warn() {
		printf 'WARN  %s\n' "$*"
		warnings=$((warnings + 1))
	}
	get() { # URL FILE
		curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --tlsv1.2 \
			--retry 3 --retry-delay 2 --connect-timeout 30 --max-time 600 -o "$2" "$1" </dev/null
	}
	work=$(mktemp -d "${TMPDIR:-/tmp}/patrol-ci-check.XXXXXX")
	trap 'rm -rf "$work"' EXIT
	[[ -n $selected ]] || selected=$(printf '%s\n' "$known" | paste -s -d, -)

	# Format of every line (as hack/images/verify-pins.sh checks hack/images/checksums.txt).
	: >"$work/seen"
	while read -r name version plat sum url specs sums rest; do
		[[ -z $name || $name == \#* ]] && continue
		where="tools.lock: $name $plat"
		if [[ -z $sums || -n $rest ]]; then
			fail "$where: expected 7 columns"
			continue
		fi
		[[ $name =~ ^[a-z0-9][a-z0-9-]*$ ]] || fail "$where: invalid name"
		[[ $version =~ ^[0-9][0-9A-Za-z.+-]*$ ]] || fail "$where: invalid version $version"
		[[ $plat == linux/amd64 || $plat == linux/arm64 ]] || fail "$where: platform must be linux/amd64 or linux/arm64"
		[[ $sum =~ ^[0-9a-f]{64}$ ]] || fail "$where: sha256 must be 64 lowercase hex digits"
		[[ $url == https://* ]] || fail "$where: URL must use https"
		[[ $url == *"$version"* ]] || fail "$where: URL does not contain version $version"
		[[ $sums == github-asset-digest || ($sums == https://* && $sums == *"$version"*) ]] ||
			fail "$where: column 7 must be an https checksum URL containing the version, or github-asset-digest"
		[[ $specs == *":bin/$name:0755" && $specs != *,* ]] || fail "$where: install spec must be SRC:bin/$name:0755"
		[[ $specs =~ ^[^:]+:bin/[a-z0-9-]+:0[0-7]{3}$ ]] || fail "$where: malformed install spec $specs"
		if grep -qxF "$name $plat" "$work/seen"; then fail "$where: duplicate entry"; fi
		printf '%s %s\n' "$name" "$plat" >>"$work/seen"
	done <"$lock"
	for name in $known; do
		for p in linux/amd64 linux/arm64; do
			grep -qxF "$name $p" "$work/seen" || fail "tools.lock: $name has no $p entry"
		done
		v1=$(lock_field "$name" linux/amd64 2)
		v2=$(lock_field "$name" linux/arm64 2)
		[[ $v1 == "$v2" ]] || fail "tools.lock: $name has different versions per platform ($v1, $v2)"
	done
	[[ $failures -eq 0 ]] && pass "tools.lock: $(wc -l <"$work/seen" | tr -d ' ') entries well-formed ($(printf '%s\n' "$known" | paste -s -d' ' -))"

	# Downloads, SHA-256 and archive members, exactly as an install does.
	for p in linux/amd64 linux/arm64; do
		if "$fetch_tools" --platform "$p" --out "$work/out-${p#linux/}" --checksums "$lock" --only "$selected" \
			${cache:+--cache "$cache"} 2>"$work/fetch-${p#linux/}.log"; then
			pass "fetch-tools.sh $p: $selected downloaded, SHA-256 verified, members extracted"
		else
			fail "fetch-tools.sh $p failed: $(tail -n 1 "$work/fetch-${p#linux/}.log")"
		fi
	done

	# Publisher checksum files (column 7) and GitHub asset digests.
	n=0
	while read -r name version plat sum url specs sums rest; do
		[[ -z $name || $name == \#* ]] && continue
		case ,$selected, in *,"$name",*) ;; *) continue ;; esac
		file=${url##*/}
		if [[ $sums == github-asset-digest ]]; then
			if [[ ! $url =~ ^https://github\.com/([^/]+/[^/]+)/releases/download/([^/]+)/ ]]; then
				fail "$name $plat: github-asset-digest needs a GitHub release URL"
				continue
			fi
			api="https://api.github.com/repos/${BASH_REMATCH[1]}/releases/tags/${BASH_REMATCH[2]}"
			n=$((n + 1))
			if ! get "$api" "$work/release-$n.json" 2>/dev/null; then
				warn "$name $plat: cannot read $api (rate limit?); the SHA-256 was still verified by the download"
				continue
			fi
			# The release JSON lists each asset's "name" before its "digest".
			published=$(tr ',' '\n' <"$work/release-$n.json" | awk -v f="$file" '
				/"name":/ { v = $0; sub(/.*"name": *"/, "", v); sub(/".*/, "", v); cur = v }
				/"digest":/ && cur == f { v = $0; sub(/.*"digest": *"sha256:/, "", v); sub(/".*/, "", v); print v; exit }')
			if [[ $published == "$sum" ]]; then
				pass "$name $plat: SHA-256 equals GitHub's asset digest"
			else
				fail "$name $plat: pinned $sum, GitHub reports '${published:-nothing}' for $file"
			fi
			continue
		fi
		n=$((n + 1))
		if ! get "$sums" "$work/sums-$n" 2>"$work/sums-$n.err"; then
			fail "$name $plat: cannot download $sums: $(tail -n 1 "$work/sums-$n.err")"
			continue
		fi
		# Either "SHA256  FILE" lines (sha256sum format, optional "*" binary marker) or a file that
		# holds only the digest (dl.k8s.io's .sha256).
		published=$(awk -v f="$file" '
			NF == 0 { next }
			{ lines++ }
			NF == 1 { only = $1 }
			NF == 2 { n = $2; sub(/^\*/, "", n); sub(/^\.\//, "", n); if (n == f) { print $1; found = 1; exit } }
			END { if (!found && lines == 1 && only != "") print only }' "$work/sums-$n")
		if [[ $published == "$sum" ]]; then
			pass "$name $plat: SHA-256 equals the publisher's (${sums##*/})"
		else
			fail "$name $plat: pinned $sum, publisher lists '${published:-nothing}' for $file in $sums"
		fi
	done <"$lock"

	echo
	echo "install-tools --check: $failures failure(s), $warnings warning(s)"
	[[ $failures -eq 0 ]]
	exit
fi

# --- install ---------------------------------------------------------------------------------------
[[ -n $selected ]] || usage_error "name at least one tool, or all"

os=$(uname -s)
[[ $os == Linux ]] || {
	printf 'install-tools: this installs Linux binaries for CI runners and containers; the host is %s (install the tools with your package manager, or run hack/ci/install-tools.sh --check to validate the lock file)\n' "$os" >&2
	exit 2
}
case $(uname -m) in
x86_64 | amd64) platform=linux/amd64 ;;
aarch64 | arm64) platform=linux/arm64 ;;
*)
	printf 'install-tools: unsupported architecture %s (linux/amd64 and linux/arm64 only)\n' "$(uname -m)" >&2
	exit 2
	;;
esac

mkdir -p "$dest"
dest=$(cd "$dest" && pwd)
log "installing $selected for $platform into $dest/bin"
"$fetch_tools" --platform "$platform" --out "$dest" --checksums "$lock" --only "$selected" ${cache:+--cache "$cache"} ||
	die "fetch-tools.sh failed"

# reported_version NAME BINARY: the version the binary reports, without a leading "v".
reported_version() {
	local out
	case $1 in
	helm) out=$("$2" version --template '{{.Version}}') ;;
	kind) out=$("$2" version | awk '{ print $2; exit }') ;;
	kubectl) out=$("$2" version --client -o json | sed -n 's/.*"gitVersion": *"\([^"]*\)".*/\1/p' | head -n 1) ;;
	golangci-lint) out=$("$2" --version | sed -n 's/.* version \([^ ]*\) .*/\1/p' | head -n 1) ;;
	gitleaks) out=$("$2" version | head -n 1) ;;
	shellcheck) out=$("$2" --version | sed -n 's/^version: //p') ;;
	actionlint) out=$("$2" -version | head -n 1) ;;
	*) return 1 ;;
	esac
	case $out in v[0-9]*) out=${out#v} ;; esac
	printf '%s\n' "$out"
}

status=0
IFS=, read -r -a names <<<"$selected"
for name in "${names[@]}"; do
	version=$(lock_field "$name" "$platform" 2)
	sum=$(lock_field "$name" "$platform" 4)
	bin=$dest/bin/$name
	if [[ ! -x $bin ]]; then
		log "$name: $bin was not installed"
		status=1
		continue
	fi
	got=$(reported_version "$name" "$bin" </dev/null 2>/dev/null) || got=
	if [[ $got != "$version" ]]; then
		log "$name: $bin reports version '${got:-?}', tools.lock pins $version"
		status=1
		continue
	fi
	printf '%s %s %s\n' "$name" "$version" "$sum"
done
[[ $status -eq 0 ]] || die "version check failed"

if [[ -n ${GITHUB_PATH:-} ]]; then
	printf '%s\n' "$dest/bin" >>"$GITHUB_PATH"
	log "added $dest/bin to GITHUB_PATH (first on PATH from the next step on)"
else
	log "add the tools to PATH with: export PATH=\"$dest/bin:\$PATH\""
fi
