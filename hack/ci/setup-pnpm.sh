#!/usr/bin/env bash
# Installs the pnpm the Dockerfile uses (ARG PNPM_VERSION / PNPM_SHA512), through corepack, which
# refuses a download whose SHA-512 differs. Node must already be on PATH (actions/setup-node).
# The version must also match ui/package.json "packageManager", so the image build and CI run the
# same pnpm.
#
# Usage: hack/ci/setup-pnpm.sh
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
version=$(sed -n 's/^ARG PNPM_VERSION=//p' "$root/Dockerfile" | head -n 1)
sha512=$(sed -n 's/^ARG PNPM_SHA512=//p' "$root/Dockerfile" | head -n 1)
[[ -n $version && $sha512 =~ ^[0-9a-f]{128}$ ]] || {
	echo "setup-pnpm: PNPM_VERSION / PNPM_SHA512 not found in the Dockerfile" >&2
	exit 1
}
declared=$(sed -n 's/.*"packageManager": *"pnpm@\([^"+]*\).*/\1/p' "$root/ui/package.json" | head -n 1)
[[ $declared == "$version" ]] || {
	echo "setup-pnpm: ui/package.json packageManager is pnpm@$declared but the Dockerfile pins $version" >&2
	exit 1
}

export COREPACK_ENABLE_DOWNLOAD_PROMPT=0 COREPACK_ENABLE_AUTO_PIN=0
command -v corepack >/dev/null 2>&1 || {
	echo "setup-pnpm: corepack not found (Node $(node --version 2>/dev/null || echo missing))" >&2
	exit 1
}
corepack enable pnpm
corepack install --global "pnpm@${version}+sha512.${sha512}"
got=$(pnpm --version)
[[ $got == "$version" ]] || {
	echo "setup-pnpm: pnpm reports $got, want $version" >&2
	exit 1
}
echo "setup-pnpm: pnpm $got (node $(node --version))"
