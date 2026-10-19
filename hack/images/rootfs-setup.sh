#!/bin/sh
# Prepares the patrol-cd runtime filesystem in the Dockerfile "runtime" stage, after the Alpine
# packages are installed: the non-root user patrol (uid/gid 999, home /home/patrol), the mount
# points the components expect (read-only root filesystem operation), system git and SSH client
# configuration, removal of setuid/setgid bits and of the package manager binary.
#
# Usage: rootfs-setup.sh [--root DIR] [--config-dir DIR] [--no-chown LOGFILE]
#                        [--keep-package-manager]
#
#   --root                  filesystem root to prepare (default /)
#   --config-dir            directory holding gitconfig and ssh_config (default: this script's)
#   --no-chown LOGFILE      do not change ownership; append "uid:gid path" lines to LOGFILE
#                           instead (hack/images/rootfs-check.sh runs unprivileged)
#   --keep-package-manager  keep /sbin/apk (for derived images that install more packages)
#
# Idempotent: running it twice leaves the same result.
set -eu

root=/
config_dir=$(cd "$(dirname "$0")" && pwd)
chown_log=
keep_apk=0
uid=999
gid=999
user=patrol
home=/home/patrol

die() {
	printf 'rootfs-setup: %s\n' "$*" >&2
	exit 1
}
log() { printf 'rootfs-setup: %s\n' "$*" >&2; }

while [ $# -gt 0 ]; do
	case $1 in
	--root) root=${2:?--root needs a value}; shift 2 ;;
	--config-dir) config_dir=${2:?--config-dir needs a value}; shift 2 ;;
	--no-chown) chown_log=${2:?--no-chown needs a log file}; shift 2 ;;
	--keep-package-manager) keep_apk=1; shift ;;
	-h | --help)
		sed -n '2,19s/^# \{0,1\}//p' "$0"
		exit 0
		;;
	*) die "unknown argument: $1 (see --help)" ;;
	esac
done

root=$(cd "$root" && pwd)
r=${root%/}
etc=$r/etc
for f in passwd group; do
	[ -f "$etc/$f" ] || die "$etc/$f not found"
done
for f in gitconfig ssh_config; do
	[ -f "$config_dir/$f" ] || die "$config_dir/$f not found"
done

# own UID:GID PATH (non-recursive).
own() {
	if [ -n "$chown_log" ]; then
		printf '%s %s\n' "$1" "$2" >>"$chown_log"
	else
		chown -h "$1" "$r$2"
	fi
}

# replace_file FILE: replace FILE's content with FILE.new, keeping its inode, owner and mode.
replace_file() {
	cat "$1.new" >"$1"
	rm -f "$1.new"
}

# --- group ------------------------------------------------------------------------------------
# Alpine's alpine-baselayout assigns gid 999 to the group "ping" (unprivileged ICMP sockets),
# which nothing in this image uses; it is removed when it has no members so patrol can take 999.
existing=$(awk -F: -v n="$user" '$1 == n { print $0 }' "$etc/group")
if [ -n "$existing" ]; then
	[ "$existing" = "$user:x:$gid:" ] || die "unexpected group entry: $existing"
else
	holder=$(awk -F: -v g="$gid" '$3 == g { print $1 ":" $4 }' "$etc/group")
	case $holder in
	'') ;;
	ping:)
		awk -F: -v g="$gid" '!($1 == "ping" && $3 == g)' "$etc/group" >"$etc/group.new"
		replace_file "$etc/group"
		log "removed group ping (gid $gid, no members)"
		;;
	*) die "gid $gid is already used: ${holder%%:*}" ;;
	esac
	printf '%s:x:%s:\n' "$user" "$gid" >>"$etc/group"
fi

# --- passwd and shadow ------------------------------------------------------------------------
entry="$user:x:$uid:$gid:Patrol CD:$home:/sbin/nologin"
existing=$(awk -F: -v n="$user" '$1 == n { print $0 }' "$etc/passwd")
if [ -n "$existing" ]; then
	[ "$existing" = "$entry" ] || die "unexpected passwd entry: $existing"
else
	holder=$(awk -F: -v u="$uid" '$3 == u { print $1 }' "$etc/passwd")
	[ -z "$holder" ] || die "uid $uid is already used: $holder"
	printf '%s\n' "$entry" >>"$etc/passwd"
fi
if [ -f "$etc/shadow" ] && ! awk -F: -v n="$user" '$1 == n { found = 1 } END { exit !found }' "$etc/shadow"; then
	# Locked password: the account is only ever used as the container's process identity.
	printf '%s:!::0:::::\n' "$user" >>"$etc/shadow"
fi

# --- directories ------------------------------------------------------------------------------
# PATH MODE OWNER. Patrol-owned leaves are mount points: in a pod they are covered by volumes
# (docs/guides/images.md); the image only guarantees that they exist with sane ownership. Parents
# stay root-owned so the process cannot add entries next to them.
dirs="
/home 0755 root
$home 0755 $user
$home/params 0755 $user
$home/cmp-server 0755 $user
$home/cmp-server/plugins 0755 $user
$home/cmp-server/config 0755 $user
/app 0755 root
/app/config 0755 root
/app/config/ssh 0755 $user
/app/config/tls 0755 $user
/app/config/gpg 0755 root
/app/config/gpg/source 0755 $user
/app/config/gpg/keys 0700 $user
/app/config/reposerver 0755 root
/app/config/reposerver/tls 0700 $user
/app/config/reposerver/mtls 0700 $user
/app/config/internal-token 0700 $user
/app/config/dex 0755 root
/app/config/dex/tls 0700 $user
/shared 0755 root
/shared/app 0755 root
"
printf '%s\n' "$dirs" | while read -r path mode owner; do
	[ -n "$path" ] || continue
	mkdir -p "$r$path"
	chmod "$mode" "$r$path"
	case $owner in
	root) own 0:0 "$path" ;;
	"$user") own "$uid:$gid" "$path" ;;
	*) die "unknown owner $owner for $path" ;;
	esac
done
# /tmp must stay world-writable with the sticky bit (an emptyDir covers it in a pod).
mkdir -p "$r/tmp"
chmod 1777 "$r/tmp"

# --- configuration files ----------------------------------------------------------------------
install_file() { # SRC DEST MODE
	mkdir -p "$(dirname "$r$2")"
	cp "$1" "$r$2"
	chmod "$3" "$r$2"
	own 0:0 "$2"
}
install_file "$config_dir/gitconfig" /etc/gitconfig 0644
install_file "$config_dir/ssh_config" /etc/ssh/ssh_config 0644

# tini: Alpine installs /sbin/tini; /usr/bin/tini is the path many manifests and docs use.
if [ -e "$r/sbin/tini" ] && [ ! -e "$r/usr/bin/tini" ] && [ ! -L "$r/usr/bin/tini" ]; then
	mkdir -p "$r/usr/bin"
	ln -s /sbin/tini "$r/usr/bin/tini"
	own 0:0 /usr/bin/tini
fi

# --- privilege reduction ----------------------------------------------------------------------
# No setuid/setgid programs (the pods also set allowPrivilegeEscalation: false).
find "$r/" -xdev -type f \( -perm -4000 -o -perm -2000 \) -print >"$r/tmp/.patrol-suid" 2>/dev/null || true
while IFS= read -r f; do
	[ -n "$f" ] || continue
	chmod ug-s "$f"
	log "removed setuid/setgid bit: ${f#"$r"}"
done <"$r/tmp/.patrol-suid"
rm -f "$r/tmp/.patrol-suid"

# Package manager: the binary is removed (the installed-package database under /lib/apk/db
# stays for vulnerability scanners); caches are dropped.
if [ "$keep_apk" -eq 0 ]; then
	rm -f "$r/sbin/apk"
	rm -rf "$r/etc/apk/cache" "$r/var/cache/apk"
	log "package manager binary removed (/sbin/apk)"
fi

log "prepared $root for $user ($uid:$gid, home $home)"
