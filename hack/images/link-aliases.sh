#!/bin/sh
# Links every component name of hack/images/aliases.txt to the multi-call binary patrolcd in DIR
# (relative symbolic links, so they stay valid wherever DIR is copied). Used by the Dockerfile
# "build" stage on /out/bin and by hack/images/rootfs-check.sh.
#
# Usage: hack/images/link-aliases.sh DIR [ALIASES_FILE]
set -eu

die() {
	printf 'link-aliases: %s\n' "$*" >&2
	exit 1
}

[ $# -ge 1 ] && [ $# -le 2 ] || die "usage: link-aliases.sh DIR [ALIASES_FILE]"
dir=$1
aliases=${2:-$(dirname "$0")/aliases.txt}
[ -f "$dir/patrolcd" ] && [ ! -L "$dir/patrolcd" ] || die "$dir/patrolcd must be a regular file"
[ -f "$aliases" ] || die "aliases file not found: $aliases"

n=0
while read -r name rest; do
	case $name in '' | '#'*) continue ;; esac
	[ -z "$rest" ] || die "one name per line expected: $name $rest"
	case $name in
	patrol-*) ;;
	*) die "alias must start with patrol-: $name" ;;
	esac
	case $name in */* | *..*) die "invalid alias: $name" ;; esac
	rm -f "$dir/$name"
	ln -s patrolcd "$dir/$name"
	n=$((n + 1))
done <"$aliases"
[ "$n" -gt 0 ] || die "no aliases in $aliases"
printf 'link-aliases: %d names linked to %s/patrolcd\n' "$n" "$dir" >&2
