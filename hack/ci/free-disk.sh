#!/usr/bin/env bash
# Frees disk space on a GitHub-hosted Ubuntu runner by deleting preinstalled toolchains that no
# Patrol CD job uses (.NET, Android SDK, GHC, CodeQL, Boost, Swift), then prints `df -h /` before
# and after and the space freed.
#
# Usage: hack/ci/free-disk.sh [--force] [--dry-run]
#
#   --force    run although CI is not "true" (the deletions are outside the repository, with sudo)
#   --dry-run  only list which of the paths exist
#
# Refuses to run on anything but Linux, and unless CI=true (GitHub Actions sets it) or --force is
# given. Each path is removed only if it exists, with `sudo -n rm -rf` (never a password prompt;
# no sudo when already root), all in parallel. A path that cannot be removed is reported and does
# not stop the others; the script still exits 0, since freeing space is best effort
# (hack/ci/preflight.sh checks the free space the kind job needs). Exit status 2: refused or
# usage error.
set -euo pipefail

paths=(
	/usr/share/dotnet
	/usr/local/lib/android
	/opt/ghc
	/opt/hostedtoolcache/CodeQL
	/usr/local/share/boost
	/usr/share/swift
)

force=0
dry_run=0
while [[ $# -gt 0 ]]; do
	case $1 in
	--force) force=1 ;;
	--dry-run) dry_run=1 ;;
	-h | --help)
		awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "${BASH_SOURCE[0]}"
		exit 0
		;;
	*)
		echo "free-disk: unknown argument $1 (see --help)" >&2
		exit 2
		;;
	esac
	shift
done

if [[ $(uname -s) != Linux ]]; then
	echo "free-disk: only for Linux CI runners; refusing on $(uname -s) (even with --force)" >&2
	exit 2
fi
if [[ ${CI:-} != true && $force -eq 0 ]]; then
	echo "free-disk: refusing to delete system directories outside CI (CI is not \"true\"); pass --force to run anyway" >&2
	exit 2
fi

avail_kb() { df -Pk / | awk 'NR == 2 { print $4 }'; }

existing=()
for p in "${paths[@]}"; do
	[[ -e $p ]] && existing+=("$p")
done
if [[ ${#existing[@]} -eq 0 ]]; then
	echo "free-disk: none of the paths exist; nothing to remove"
	df -h /
	exit 0
fi
if [[ $dry_run -eq 1 ]]; then
	printf 'free-disk: would remove %s\n' "${existing[@]}"
	exit 0
fi

sudo=()
if [[ $(id -u) -ne 0 ]]; then
	if ! command -v sudo >/dev/null 2>&1 || ! sudo -n true 2>/dev/null; then
		echo "free-disk: not root and no passwordless sudo; nothing removed" >&2
		df -h /
		exit 0
	fi
	sudo=(sudo -n)
fi

echo "free-disk: before"
df -h /
before=$(avail_kb)
start=$(date +%s)

pids=()
for p in "${existing[@]}"; do
	"${sudo[@]+"${sudo[@]}"}" rm -rf -- "$p" &
	pids+=($!)
done
failed=0
i=0
for p in "${existing[@]}"; do
	if wait "${pids[$i]}" && [[ ! -e $p ]]; then
		echo "free-disk: removed $p"
	else
		echo "free-disk: WARN could not remove $p (continuing)"
		failed=$((failed + 1))
	fi
	i=$((i + 1))
done

echo "free-disk: after"
df -h /
after=$(avail_kb)
awk -v b="$before" -v a="$after" -v s="$(($(date +%s) - start))" -v f="$failed" 'BEGIN {
	printf "free-disk: freed %.1f GiB on / in %d s (%.1f GiB available)%s\n", (a - b) / 1048576, s, a / 1048576,
		f ? sprintf("; %d path(s) could not be removed", f) : "" }'
