#!/usr/bin/env bash
# CodeQL in the local SDLC (docs/guides/claude-code-sdlc.md). The repository is public and
# Apache-2.0, which the CodeQL CLI license allows (the same analysis runs on GitHub in
# .github/workflows/codeql.yml).
#
#   fetch DIR            download the pinned CodeQL bundle for this platform into DIR, after checking
#                        its SHA-256 against the pin below and the publisher's checksum file, and
#                        print its path (CI passes it to github/codeql-action/init)
#   install              fetch the bundle and unpack it into bin/codeql-<version>/
#   analyze [LANG...]    build a database per language and run its security-extended suite, then
#                        gate the results with hack/sdlc/sarifgate (accepted alerts:
#                        hack/sdlc/codeql-baseline.txt). LANG: go, javascript-typescript, actions
#                        (default: all three). Output: dist/codeql/<lang>.sarif, dist/codeql/summary.txt
#
# Usage: hack/sdlc/codeql.sh fetch DIR | install | analyze [LANG...]
set -euo pipefail
export GOWORK=off # the tree's go.mod, not a go.work in a parent directory (CI has none)

cd "$(git rev-parse --show-toplevel)"

version=2.27.3
case "$(uname -s)/$(uname -m)" in
Darwin/*)
	asset='codeql-bundle-osx64.tar.zst'
	sha256=c773b4654a641d02967f40d994bab1850fd8bc3bddb3d20f9af17b0cca3c9a29
	;;
Linux/x86_64)
	asset='codeql-bundle-linux64.tar.zst'
	sha256=5e7607e1b721728955e31f5402d460ad227cbd2ffed0d4cc610116755796150e
	;;
*)
	echo "codeql: no CodeQL bundle for $(uname -s)/$(uname -m)" >&2
	exit 1
	;;
esac
url=https://github.com/github/codeql-action/releases/download/codeql-bundle-v$version/$asset
dir=bin/codeql-$version
codeql=$dir/codeql/codeql
out=dist/codeql

# fetch DIR downloads the pinned bundle into DIR and checks its SHA-256 against the pin and the
# publisher's checksum file (a release asset can be replaced under its tag); it prints the path.
fetch() {
	local d=$1 got
	echo "codeql: downloading CodeQL $version ($asset)" >&2
	curl -fsSL --retry 3 -o "$d/$asset" "$url"
	curl -fsSL --retry 3 -o "$d/sums" "$url.checksum.txt"
	got=$(shasum -a 256 "$d/$asset" | cut -d' ' -f1)
	if [[ $got != "$sha256" ]] || ! grep -q "^$sha256  $asset\$" "$d/sums"; then
		echo "codeql: checksum mismatch for $asset: got $got, pinned $sha256, publisher's file: $(cat "$d/sums")" >&2
		exit 1
	fi
	printf '%s\n' "$d/$asset"
}

install() {
	[[ -x $codeql ]] && return 0
	command -v zstd >/dev/null 2>&1 || {
		echo "codeql: zstd is needed to unpack the bundle (brew install zstd / apt-get install zstd)" >&2
		exit 1
	}
	tmp=$(mktemp -d) # global: the EXIT trap runs after this function returned
	trap 'rm -rf "$tmp"' EXIT
	fetch "$tmp" >/dev/null
	mkdir -p "$tmp/x"
	zstd -dc "$tmp/$asset" | tar -xf - -C "$tmp/x"
	rm -rf "$dir"
	mkdir -p "$dir"
	mv "$tmp/x/codeql" "$dir/codeql"
	"$codeql" version --format=terse
}

analyze() {
	local langs=("$@")
	[[ ${#langs[@]} -gt 0 ]] || langs=(go javascript-typescript actions)
	install
	rm -rf "$out"
	mkdir -p "$out/db"
	local sarifs=() lang pack src db
	for lang in "${langs[@]}"; do
		db=$out/db/$lang
		case $lang in
		go)
			pack=go
			# cgo off, as every build of this repository (hack/build.mk, Dockerfile); it also keeps the
			# traced build from calling the C toolchain, which the macOS bundle's x86_64 tracer cannot load.
			CGO_ENABLED=0 GOFLAGS=-mod=readonly "$codeql" database create "$db" --language=go --source-root=. \
				--codescanning-config=.github/codeql/codeql-config.yml --command='go build ./...' \
				--threads=0 --overwrite --quiet
			;;
		javascript-typescript)
			pack=javascript
			"$codeql" database create "$db" --language=javascript-typescript --source-root=. \
				--codescanning-config=.github/codeql/codeql-config.yml --build-mode=none --threads=0 --overwrite --quiet
			;;
		actions)
			pack=actions
			"$codeql" database create "$db" --language=actions --source-root=. \
				--codescanning-config=.github/codeql/codeql-config.yml --build-mode=none --threads=0 --overwrite --quiet
			;;
		*)
			echo "codeql: unknown language $lang (go, javascript-typescript, actions)" >&2
			exit 2
			;;
		esac
		src=codeql/$pack-queries:codeql-suites/$pack-security-extended.qls
		echo "codeql: analyzing $lang with $src"
		"$codeql" database analyze "$db" "$src" --format=sarif-latest --output="$out/$lang.sarif" \
			--sarif-category="$lang" --threads=0 --quiet
		sarifs+=("$out/$lang.sarif")
	done
	go run ./hack/sdlc/sarifgate -baseline hack/sdlc/codeql-baseline.txt -summary "$out/summary.txt" "${sarifs[@]}"
}

case ${1:-} in
fetch)
	[[ $# -eq 2 ]] || {
		echo "usage: hack/sdlc/codeql.sh fetch DIR" >&2
		exit 2
	}
	mkdir -p "$2"
	fetch "$2"
	;;
install) install ;;
analyze)
	shift
	analyze "$@"
	;;
*)
	echo "usage: hack/sdlc/codeql.sh fetch DIR | install | analyze [go|javascript-typescript|actions...]" >&2
	exit 2
	;;
esac
