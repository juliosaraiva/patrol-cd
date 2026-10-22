#!/usr/bin/env bash
# Lints every tracked shell script: shellcheck -x on all of them, bash -n on the bash ones and
# dash -n (when dash is installed) on the ones that promise POSIX sh. A file counts as a shell
# script when it ends in .sh or its first line is a shell shebang. Run by `make ci-shell`.
#
# Usage: hack/ci/lint-shell.sh [--list]
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
list_only=0
[[ ${1:-} == --list ]] && list_only=1

bash_files=()
sh_files=()
while IFS= read -r -d '' f; do
	[[ -f $f && ! -L $f ]] || continue
	first=$(head -n 1 "$f" 2>/dev/null || true)
	case $first in
	'#!/bin/sh' | '#!/usr/bin/env sh') sh_files+=("$f") ;;
	'#!'*bash*) bash_files+=("$f") ;;
	*)
		case $f in
		*.sh) bash_files+=("$f") ;;
		esac
		;;
	esac
done < <(git ls-files -z --cached --others --exclude-standard)

if [[ $list_only -eq 1 ]]; then
	printf '%s\n' "${bash_files[@]}" "${sh_files[@]}" | sort
	exit 0
fi

all=("${bash_files[@]}" "${sh_files[@]}")
[[ ${#all[@]} -gt 0 ]] || {
	echo "lint-shell: no shell scripts found" >&2
	exit 1
}
command -v shellcheck >/dev/null 2>&1 || {
	echo "lint-shell: shellcheck is not installed (hack/ci/install-tools.sh shellcheck)" >&2
	exit 1
}

echo "lint-shell: shellcheck $(shellcheck --version | sed -n 's/^version: //p') on ${#all[@]} scripts (${#sh_files[@]} POSIX sh)"
shellcheck -x "${all[@]}"

status=0
for f in "${bash_files[@]}"; do
	bash -n "$f" || status=1
done
if command -v dash >/dev/null 2>&1; then
	for f in "${sh_files[@]}"; do
		dash -n "$f" || status=1
	done
else
	echo "lint-shell: dash not installed; skipping dash -n on ${#sh_files[@]} POSIX scripts"
fi
exit $status
