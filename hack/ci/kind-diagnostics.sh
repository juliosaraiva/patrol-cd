#!/usr/bin/env bash
# Collects allow-listed diagnostics of the disposable patrol-* kind environment into OUTDIR, for
# upload after a failed CI job, then scrubs the bundle of known secret values.
#
# Usage: hack/ci/kind-diagnostics.sh OUTDIR
#        hack/ci/kind-diagnostics.sh --scrub-only DIR
#
# OUTDIR must be new or empty. OUTDIR/INDEX.txt is written first, saying "scrub: PENDING", and
# rewritten after the scrub; its "scrub:" line is the only line that can say "scrub: ok". Upload
# more than INDEX.txt only when that line starts with "scrub: ok".
#
# Collected (anything missing, failed or skipped is noted in OUTDIR/INDEX.txt):
#   host/   date, uptime, free, df, docker version/system df/ps -a/stats, the "kind" network,
#           the patrol-registry log, dmesg tail (sudo -n only), netfilter modules, ip addr/route
#   kind/<cluster>/                 `kind export logs` of every existing patrol-* cluster
#   nodes/<node>/                   nft ruleset and netfilter modules seen inside each kind node
#   k8s/<cluster>/                  through the patrol::kubectl guards (hack/kind/lib.sh): nodes
#           (wide and described), pods, events, workloads/services/endpointslices/pvcs (wide),
#           NetworkPolicies, ConfigMap and Secret NAMES only, Leases, helm list/history, the
#           Patrol custom resources (status columns and conditions; Repository, RepositoryCredential
#           and Cluster objects, which hold Secret references, not credentials), pod logs of the
#           patrol and patrol-e2e namespaces and of kindnet (with --previous after a restart);
#           skipped (and noted) when the cluster's Secret names cannot be listed first, since the
#           scrub cannot vouch for the bundle without its Secrets (the scrub still reads them)
# Never collected: Secret contents (no `get secret -o yaml/json`), `helm get values/manifest/all`,
# the kubeconfigs under .cache/kind, other scripts' temporary directories, the environment.
#
# Time: collection stops once DIAG_BUDGET_SECONDS (default 600) have passed; the remaining items
# are noted as skipped and the scrub still runs. Every command that can block (kind, docker,
# kubectl, helm, sudo dmesg, grep) runs under timeout(1) or gtimeout when one is installed: 60 s
# per call (`kind export logs` 300 s, the bundle grep 300 s), never past the budget during
# collection. Without timeout(1) they run unbounded and only the checks between items apply.
#
# Interrupted (INT, TERM, HUP) or stopped by an error: the running command is stopped, everything
# in OUTDIR but INDEX.txt is removed and INDEX.txt says "scrub: INCOMPLETE (interrupted ...)". A
# KILL leaves "scrub: PENDING" next to unscrubbed files. Exit status: 0 when the run completed
# (whatever the scrub status; with --scrub-only only when the scrub is ok, 3 when it is
# INCOMPLETE), 2 on a usage error, 128+N after signal N.
#
# Scrub: the values of secret-looking keys of every Secret in the patrol-* clusters (passwords,
# tokens, private keys, kubeconfig credentials; e.g. the initial admin password, the e2e fixture
# credentials, the patrol-target cluster bearerToken), in decoded and stored (base64) form, plus
# the client keys of the .cache/kind kubeconfigs (counted apart), are written to a mode-600
# temporary file (never printed). Every file of the bundle that contains one of them
# (`grep -rlFf`) is replaced by a one-line note, and so is every *.gz file (grep cannot look
# inside); symbolic links are removed. The scrub status is
#   ok                          the Secrets of every patrol-* cluster were read;
#   ok (nothing to scrub: ...)  no patrol-* cluster and no kubeconfig exist (kind-up failed before
#                               creating one), so the bundle holds host state only;
#   INCOMPLETE: ...             the Secrets of a cluster could not be read (no kubeconfig, API down
#                               or too slow), no value could be read at all, or grep failed.
#                               Files holding a value that was known are replaced all the same.
# --scrub-only applies just this step to an existing directory (e.g. job logs before upload): it
# writes no INDEX.txt, removes nothing when interrupted and has no "nothing to scrub" case, so it
# needs the clusters to still exist. Upload DIR only when the exit status is 0.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
lib=$here/../kind/lib.sh
# shellcheck source=hack/kind/lib.sh
source "$lib"

usage() { awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "${BASH_SOURCE[0]}"; }

scrub_only=0
out=
while [[ $# -gt 0 ]]; do
	case $1 in
	--scrub-only) scrub_only=1 ;;
	-h | --help)
		usage
		exit 0
		;;
	-*)
		echo "kind-diagnostics: unknown option $1 (see --help)" >&2
		exit 2
		;;
	*)
		[[ -z $out ]] || {
			echo "kind-diagnostics: one directory only (see --help)" >&2
			exit 2
		}
		out=$1
		;;
	esac
	shift
done
[[ -n $out ]] || {
	echo "kind-diagnostics: OUTDIR is required (see --help)" >&2
	exit 2
}
budget=${DIAG_BUDGET_SECONDS:-600}
if [[ ! $budget =~ ^[1-9][0-9]{0,5}$ ]]; then
	echo "kind-diagnostics: DIAG_BUDGET_SECONDS must be a whole number of seconds (got '$budget')" >&2
	exit 2
fi
if [[ $scrub_only -eq 1 ]]; then
	if [[ ! -d $out ]]; then
		echo "kind-diagnostics: $out is not a directory" >&2
		exit 2
	fi
elif [[ -e $out || -L $out ]] && { [[ ! -d $out ]] || [[ -n $(ls -A "$out") ]]; }; then
	# An interrupted run removes everything in OUTDIR but INDEX.txt, so it must all be ours.
	echo "kind-diagnostics: $out must be a new or empty directory" >&2
	exit 2
fi
mkdir -p "$out"
out=$(cd "$out" && pwd)

work=$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/patrol-diag.XXXXXX")
chmod 700 "$work"
index=$work/index
: >"$index"
: >"$work/child"
started=$(date -u +%Y-%m-%dT%H:%M:%SZ)
deadline=$((SECONDS + budget))
krt=--request-timeout=30s
call_limit=60    # one kind/docker/kubectl/helm call
export_limit=300 # `kind export logs` of one cluster
scrub_limit=60   # reading the Secrets of one cluster
grep_limit=300   # the scrub's grep over the bundle
lim=$call_limit  # limit of the command being run (see set_lim)
bgpid=           # background subshell being waited for (see run_bg)
stage=collection
finished=0
why=
budget_noted=0
clusters=
list_failed=
clusters_note="not listed yet"
cluster_re='^patrol-[a-z0-9]([a-z0-9-]*[a-z0-9])?$'
name_re='^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$'
have() { command -v "$1" >/dev/null 2>&1; }
timeout_bin=
if have timeout; then
	timeout_bin=timeout
elif have gtimeout; then
	timeout_bin=gtimeout
fi

# note STATUS TEXT: one index line. STATUS is one of the fixed words below and is always followed
# by a space, and newlines in TEXT are flattened, so no index line can read "scrub: ...".
note() { printf '%-5s %s\n' "$1" "${2//$'\n'/ }" >>"$index"; }
skip() { note skip "$1: $2"; }

# write_index SCRUB: (re)writes OUTDIR/INDEX.txt through a rename, so it is never half written.
write_index() {
	{
		echo "Patrol CD kind diagnostics (hack/ci/kind-diagnostics.sh)"
		echo "started $started, written $(date -u +%Y-%m-%dT%H:%M:%SZ) after ${SECONDS}s (collection budget ${budget}s), host $(uname -srm)"
		echo "clusters: $clusters_note"
		echo "scrub: $1"
		echo
		echo "status path (ok = collected; FAIL = command failed, output kept; skip = not collected; WARN, scrub = notes)"
		cat "$index"
	} >"$out/.INDEX.txt.new"
	mv -f "$out/.INDEX.txt.new" "$out/INDEX.txt"
}

# stop_children: stops the command being waited for (the background subshell of run_bg and the
# process of with_timeout, which passes the signal on to its process group) and gives them 10 s.
stop_children() {
	local pids p i=0 alive
	pids="$bgpid $(cat "$work/child" 2>/dev/null || true)"
	for p in $pids; do kill -TERM "$p" 2>/dev/null || true; done
	while [[ $i -lt 20 ]]; do
		alive=
		for p in $pids; do
			if kill -0 "$p" 2>/dev/null; then alive=1; fi
		done
		[[ -n $alive ]] || return 0
		sleep 0.5
		i=$((i + 1))
	done
	for p in $pids; do kill -KILL "$p" 2>/dev/null || true; done
}

# on_exit: after an interrupt or an error, removes the unscrubbed bundle and says so in INDEX.txt.
# It may run while a command's output is redirected into the bundle, so it reports on the script's
# own stdout and stderr, kept as fds 3 and 4 (closed for the background commands).
exec 3>&1 4>&2
on_exit() {
	local rc=$? left
	set +e
	exec 1>&3 2>&4
	trap '' INT TERM HUP
	if [[ $finished -ne 1 ]]; then
		[[ -n $why ]] || why="stopped by an error (exit $rc)"
		stop_children
		if [[ $scrub_only -eq 0 ]]; then
			find "$out" -mindepth 1 -maxdepth 1 ! -name INDEX.txt -exec rm -rf {} + 2>/dev/null
			left=$(find "$out" -mindepth 1 -maxdepth 1 ! -name INDEX.txt 2>/dev/null | wc -l | tr -d ' ')
			if [[ $left -eq 0 ]]; then
				write_index "INCOMPLETE ($why during the $stage): every collected file was removed"
			else
				write_index "INCOMPLETE ($why during the $stage): $left collected file(s) could not be removed"
			fi
			echo "kind-diagnostics: $why during the $stage; kept only $out/INDEX.txt" >&2
		else
			echo "kind-diagnostics: scrub INCOMPLETE ($why); $out was not fully scrubbed" >&2
		fi
	fi
	rm -rf "$work"
	exit "$rc"
}
on_signal() {
	trap '' INT TERM HUP
	why="interrupted by SIG$1"
	exit "$2"
}
trap on_exit EXIT
trap 'on_signal INT 130' INT
trap 'on_signal TERM 143' TERM
trap 'on_signal HUP 129' HUP

if [[ $scrub_only -eq 0 ]]; then
	write_index "PENDING (collection in progress; nothing here has been checked yet)"
fi

# --- bounded commands ------------------------------------------------------------------------------
# with_timeout SECS CMD...: runs the binary CMD (timeout(1) cannot run a shell function: wrap one
# in "$BASH" -c, see lib_call) under timeout(1) or gtimeout when installed (TERM to its process
# group after SECS seconds, KILL 5 s later), unbounded otherwise. CMD runs in the background with
# its PID in $work/child, so that a signal reaches the trap at once and the trap can stop it.
with_timeout() {
	local secs=$1 pid rc=0
	shift
	if [[ -n $timeout_bin ]]; then
		"$timeout_bin" -k 5 "$secs" "$@" </dev/null 3>&- 4>&- &
	else
		"$@" </dev/null 3>&- 4>&- &
	fi
	pid=$!
	echo "$pid" >"$work/child"
	wait "$pid" || rc=$?
	: >"$work/child"
	return "$rc"
}

# set_lim SECS: the limit of the next command; during collection never past the budget.
set_lim() {
	lim=$1
	if [[ $stage == collection ]]; then
		local left=$((deadline - SECONDS))
		[[ $left -ge $lim ]] || lim=$left
		[[ $lim -ge 1 ]] || lim=1
	fi
}

# in_budget WHAT: true while the collection budget lasts; afterwards notes WHAT as skipped.
in_budget() {
	if [[ $SECONDS -lt $deadline ]]; then return 0; fi
	if [[ $budget_noted -eq 0 ]]; then
		budget_noted=1
		note WARN "time budget of ${budget}s used up after ${SECONDS}s: the remaining items were skipped"
		echo "kind-diagnostics: time budget of ${budget}s used up; skipping the remaining items" >&2
	fi
	skip "$1" "time budget used up"
	return 1
}

# run_bg CMD...: runs CMD, a binary (under with_timeout $lim) or a function of this script (which
# bounds its own commands with $lim), and waits for it. A function runs in a background subshell,
# so that a failing lib.sh guard (patrol::die) cannot end the script.
run_bg() {
	local rc=0
	if declare -F "$1" >/dev/null; then
		("$@") </dev/null 3>&- 4>&- &
		bgpid=$!
		wait "$bgpid" || rc=$?
		bgpid=
	else
		with_timeout "$lim" "$@" || rc=$?
	fi
	return "$rc"
}

# collect_t SECS REL CMD...: runs CMD (see run_bg) for at most SECS, with stdout and stderr in
# OUTDIR/REL, and records the result in the index. Never fails.
collect_t() {
	local rel=$2 rc=0
	set_lim "$1"
	shift 2
	in_budget "$rel" || return 0
	mkdir -p "$(dirname "$out/$rel")"
	run_bg "$@" >"$out/$rel" 2>&1 || rc=$?
	if [[ $rc -eq 0 ]]; then
		note ok "$rel"
	elif [[ $rc -eq 124 && -n $timeout_bin ]]; then
		note FAIL "$rel (timed out after ${lim}s)"
	else
		note FAIL "$rel (exit $rc)"
	fi
	return 0
}
collect() { collect_t "$call_limit" "$@"; }

# capture FILE SECS CMD...: runs CMD (see run_bg) for at most SECS, stdout to FILE (in the work
# directory), stderr dropped; returns its status.
capture() {
	local file=$1
	set_lim "$2"
	shift 2
	run_bg "$@" >"$file" 2>/dev/null
}

# k CLUSTER ARGS...: patrol::kubectl (same lib.sh guards: patrol-* name, its .cache/kind kubeconfig,
# a local kind API server) with the kubectl binary under with_timeout. Run through run_bg only.
k() {
	local c=$1 kc
	shift
	kc=$(patrol::kubeconfig "$c") || return 1
	patrol::guard_kubeconfig "$kc" "kind-$c"
	with_timeout "$lim" kubectl --kubeconfig "$kc" --context "kind-$c" "$krt" "$@"
}
# lib_call FUNC ARGS...: a lib.sh function in a new bash, so that with_timeout can bound it. That
# bash does not inherit pipefail, which lib.sh relies on its callers to set (patrol::kind_clusters
# would otherwise return 0 when kind fails).
lib_call() {
	# shellcheck disable=SC2016
	with_timeout "$lim" env PATROL_ROOT="$PATROL_ROOT" PATROL_KIND_CACHE="$PATROL_KIND_CACHE" \
		"$BASH" -c 'set -o pipefail; source "$1"; shift; "$@"' kind-diagnostics "$lib" "$@"
}
# h CLUSTER ARGS...: patrol::helm (helm has no request timeout of its own).
h() { lib_call patrol::helm "$@"; }

# usable CLUSTER: the cluster's kubeconfig exists and passes the lib.sh guards (no API call).
usable() {
	local kc
	kc=$(patrol::kubeconfig "$1")
	[[ -f $kc ]] && (patrol::guard_kubeconfig "$kc" "kind-$1") >/dev/null 2>&1
}

words() { echo $#; }

# --- collection ------------------------------------------------------------------------------------
collect_host() {
	collect host/date.txt date -u
	collect host/uptime.txt uptime
	if have free; then collect host/free.txt free -m; else skip host/free.txt "free not available"; fi
	collect host/df.txt df -h
	if have dmesg && have sudo && capture "$work/probe" 10 sudo -n true; then
		collect host/dmesg-tail.txt dmesg_tail
	else
		skip host/dmesg-tail.txt "dmesg needs passwordless sudo"
	fi
	if have lsmod; then collect host/lsmod-netfilter.txt lsmod_netfilter; else skip host/lsmod-netfilter.txt "lsmod not available"; fi
	if have ip; then
		collect host/ip-addr.txt ip -br addr
		collect host/ip-route.txt ip route
	else
		skip host/ip "ip not available"
	fi
	if ! have docker; then
		skip host/docker "docker not installed"
		return 0
	fi
	collect host/docker-version.txt docker version
	collect host/docker-system-df.txt docker system df
	collect host/docker-ps.txt docker ps -a
	collect host/docker-stats.txt docker stats --no-stream
	collect host/docker-network-kind.json docker network inspect "$PATROL_KIND_NETWORK"
	in_budget host/patrol-registry.log || return 0
	if capture "$work/probe" "$call_limit" docker inspect -f '{{.State.Running}}' "$PATROL_REGISTRY_NAME" &&
		[[ -s $work/probe ]]; then
		collect host/patrol-registry.log docker logs --timestamps "$PATROL_REGISTRY_NAME"
	else
		skip host/patrol-registry.log "no $PATROL_REGISTRY_NAME container"
	fi
}
dmesg_tail() { with_timeout "$lim" sudo -n dmesg -T | tail -n 300; }
netfilter_re='^(nf_|nft_|nfnetlink|xt_|x_tables|ip_tables|ip6_tables|iptable_|ip6table_|br_netfilter|bridge|veth|overlay)'
lsmod_netfilter() { with_timeout "$lim" lsmod | awk -v re="$netfilter_re" 'NR == 1 || $1 ~ re'; }
node_modules() { with_timeout "$lim" docker exec "$1" cat /proc/modules | awk -v re="$netfilter_re" '$1 ~ re { print $1 }' | sort; }

collect_kind() {
	local c node
	if [[ -z $clusters ]]; then
		skip kind "no patrol-* kind clusters (or kind/docker missing)"
		return 0
	fi
	for c in $clusters; do
		patrol::guard_cluster "$c"
		collect_t "$export_limit" "kind/$c.export.txt" kind export logs "$out/kind/$c" --name "$c"
		in_budget "nodes/$c-*" || continue
		capture "$work/nodes" "$call_limit" kind get nodes --name "$c" || true
		while read -r node; do
			[[ $node =~ ^patrol-[a-z0-9-]+$ ]] || continue
			collect "nodes/$node/nft-ruleset.txt" docker exec "$node" nft list ruleset
			collect "nodes/$node/netfilter-modules.txt" node_modules "$node"
		done <"$work/nodes"
	done
}

# pod_logs CLUSTER NS [SELECTOR]: current logs of every pod (all containers), and --previous for
# each container that restarted.
pod_logs() {
	local c=$1 ns=$2 sel=${3:-} pod cname restarts
	local args=(-n "$ns" get pods -o 'jsonpath={range .items[*]}{.metadata.name}{"\n"}{end}')
	[[ -z $sel ]] || args+=(-l "$sel")
	in_budget "k8s/$c/logs/$ns" || return 0
	capture "$work/pods" "$call_limit" k "$c" "${args[@]}" || true
	while read -r pod; do
		[[ $pod =~ $name_re ]] || continue
		collect "k8s/$c/logs/$ns/$pod.log" k "$c" -n "$ns" logs "$pod" --all-containers --prefix --timestamps
		in_budget "k8s/$c/logs/$ns/$pod.*.previous.log" || continue
		capture "$work/restarts" "$call_limit" k "$c" -n "$ns" get pod "$pod" -o 'jsonpath={range .status.initContainerStatuses[*]}{.name}{" "}{.restartCount}{"\n"}{end}{range .status.containerStatuses[*]}{.name}{" "}{.restartCount}{"\n"}{end}' || true
		while read -r cname restarts; do
			[[ $cname =~ $name_re && $restarts =~ ^[0-9]+$ ]] || continue
			[[ $restarts -gt 0 ]] || continue
			collect "k8s/$c/logs/$ns/$pod.$cname.previous.log" k "$c" -n "$ns" logs "$pod" -c "$cname" --previous --timestamps
		done <"$work/restarts"
	done <"$work/pods"
}

conditions_jsonpath='{range .items[*]}{.kind} {.metadata.namespace}/{.metadata.name}{"\n"}{range .status.conditions[*]}{"  "}{.type}={.status} reason={.reason} since={.lastTransitionTime} {.message}{"\n"}{end}{end}'
app_columns='NAMESPACE:.metadata.namespace,NAME:.metadata.name,SYNC:.status.sync.status,HEALTH:.status.health.status,REVISION:.status.sync.revision,OPERATION:.status.operationState.phase,MESSAGE:.status.operationState.message'

collect_cluster() {
	local c=$1 d="k8s/$1" ns rel kind
	collect "$d/nodes.txt" k "$c" get nodes -o wide
	collect "$d/nodes-describe.txt" k "$c" describe nodes
	collect "$d/pods.txt" k "$c" get pods -A -o wide
	collect "$d/events.txt" k "$c" get events -A --sort-by=.lastTimestamp
	collect "$d/workloads.txt" k "$c" get deploy,sts,ds,svc,endpointslices,pvc -A -o wide
	collect "$d/networkpolicies.yaml" k "$c" get networkpolicies -A -o yaml
	collect "$d/configmap-names.txt" k "$c" get configmaps -A
	collect "$d/secret-names.txt" k "$c" get secrets -A
	collect "$d/leases.txt" k "$c" get leases -A -o wide
	collect "$d/helm-list.txt" h "$c" list -A
	if [[ -f $out/$d/helm-list.txt ]]; then
		# The header line and an error message fail the name checks.
		while read -r rel ns _; do
			[[ $rel =~ $name_re && $ns =~ $name_re ]] || continue
			collect "$d/helm-history-$ns-$rel.txt" h "$c" history "$rel" -n "$ns"
		done <"$out/$d/helm-list.txt"
	fi
	if in_budget "$d/patrol"; then
		if capture "$work/probe" "$call_limit" k "$c" get crd applications.cd.patrol.example; then
			collect "$d/patrol/applications.txt" k "$c" get applications.cd.patrol.example -A -o "custom-columns=$app_columns"
			for kind in applications applicationsets operations clusters repositories; do
				collect "$d/patrol/$kind-conditions.txt" k "$c" get "$kind.cd.patrol.example" -A -o "jsonpath=$conditions_jsonpath"
			done
			collect "$d/patrol/operations.txt" k "$c" get operations.cd.patrol.example -A -o wide
			collect "$d/patrol/appprojects.txt" k "$c" get appprojects.cd.patrol.example -A
			collect "$d/patrol/applicationsets.txt" k "$c" get applicationsets.cd.patrol.example -A
			collect "$d/patrol/references.yaml" k "$c" get repositories.cd.patrol.example,repositorycredentials.cd.patrol.example,clusters.cd.patrol.example -A -o yaml
		else
			skip "$d/patrol" "Patrol CRDs not installed in $c (or the API did not answer)"
		fi
	fi
	for ns in patrol patrol-e2e; do
		in_budget "$d/*-$ns" || continue
		if capture "$work/probe" "$call_limit" k "$c" get namespace "$ns"; then
			collect "$d/describe-pods-$ns.txt" k "$c" -n "$ns" describe pods
			collect "$d/leases-$ns.yaml" k "$c" -n "$ns" get leases -o yaml
			pod_logs "$c" "$ns"
		fi
	done
	pod_logs "$c" kube-system app=kindnet
}

# --- scrub -----------------------------------------------------------------------------------------
# Key names whose values are treated as secret (case-insensitive), minus public ones (CA and
# public certificates and keys, usernames, known_hosts, authorized_keys, password timestamps),
# which would only cause needless deletions.
secret_key_re='(password|passwd|token|secret|private|credential|apikey|api-key|key-data|kubeconfig|dockerconfig|(^|[._-])key$|^config$)'
public_key_re='(mtime|username|\.pub$|known_hosts|authorized_keys|\.crt$|cadata)'

# add_value FILE: appends the secret lines of a decoded value to the patterns file: each line
# (trimmed) of at least 8 characters that is not PEM armor; for a kubeconfig, only the
# credential fields (client-key-data, token, password) and the decoded client key.
add_value() {
	local f=$1
	if grep -q -e 'client-key-data:' -e '^ *token:' -e 'clusters:' "$f" 2>/dev/null; then
		awk '$1 == "client-key-data:" || $1 == "token:" || $1 == "password:" { if (length($2) >= 8) print $2 }' "$f" >>"$patterns"
		awk '$1 == "client-key-data:" { print $2 }' "$f" | while IFS= read -r v; do
			printf '%s' "$v" | base64 -d 2>/dev/null >"$work/key.pem" || continue
			add_lines "$work/key.pem"
		done
	else
		add_lines "$f"
	fi
}
add_lines() {
	LC_ALL=C awk '{ sub(/\r$/, ""); gsub(/^[ \t]+|[ \t]+$/, "") }
		length($0) >= 8 && $0 !~ /^-----/ { print }' "$1" >>"$patterns"
}
# kubeconfig_credentials FILE: how many credential fields add_value takes from a kubeconfig.
kubeconfig_credentials() {
	awk '($1 == "client-key-data:" || $1 == "token:" || $1 == "password:") && length($2) >= 8 { n++ } END { print n + 0 }' "$1"
}

# gather_patterns: fills the patterns file; sets unread (clusters whose Secrets could not be read)
# and the counters of the summary. A partial read still contributes the values it returned.
gather_patterns() {
	local c ns name key value kc
	for c in $clusters; do
		if ! usable "$c"; then
			note WARN "scrub: cannot read the Secrets of $c (no usable kubeconfig)"
			unread="$unread $c"
			continue
		fi
		# Values go to a file in the private work directory, never to stdout. The $ in the Go
		# template is meant literally.
		: >"$work/data"
		chmod 600 "$work/data"
		# shellcheck disable=SC2016
		if capture "$work/data" "$scrub_limit" k "$c" get secrets -A -o 'go-template={{range .items}}{{if ne .type "helm.sh/release.v1"}}{{$ns := .metadata.namespace}}{{$n := .metadata.name}}{{range $k, $v := .data}}{{$ns}} {{$n}} {{$k}} {{$v}}{{"\n"}}{{end}}{{end}}{{end}}'; then
			read_clusters=$((read_clusters + 1))
		else
			note WARN "scrub: cannot read the Secrets of $c (the API refused, failed or did not answer within ${scrub_limit}s)"
			unread="$unread $c"
		fi
		while read -r ns name key value; do
			[[ -n $value ]] || continue
			printf '%s\n' "$key" | LC_ALL=C grep -iE "$secret_key_re" >/dev/null || continue
			if printf '%s\n' "$key" | LC_ALL=C grep -iE "$public_key_re" >/dev/null; then continue; fi
			secret_values=$((secret_values + 1))
			[[ ${#value} -ge 8 ]] && printf '%s\n' "$value" >>"$patterns"
			printf '%s' "$value" | base64 -d 2>/dev/null >"$work/value" || continue
			[[ $(wc -c <"$work/value") -ge 8 ]] || short_values=$((short_values + 1))
			add_value "$work/value"
		done <"$work/data"
		rm -f "$work/data" "$work/value" "$work/key.pem"
	done
	unread=${unread# }
	# Client keys of the host-side kubeconfigs (read here, never copied into the bundle).
	for kc in "$PATROL_KIND_CACHE"/patrol-*.kubeconfig; do
		[[ -f $kc ]] || continue
		kc_files=$((kc_files + 1))
		kc_values=$((kc_values + $(kubeconfig_credentials "$kc")))
		add_value "$kc"
	done
	rm -f "$work/key.pem"
}

# replace FILE WHY: replaces a file of the bundle by a one-line note.
replace() {
	printf 'removed by hack/ci/kind-diagnostics.sh: this file %s\n' "$2" >"$1"
	echo "kind-diagnostics: scrubbed ${1#"$out"/} ($2)"
	note scrub "${1#"$out"/}: $2, replaced by a note"
	removed=$((removed + 1))
}

scrub() {
	local rc=0 f problems=
	stage=scrub
	patterns=$work/patterns
	: >"$patterns"
	chmod 600 "$patterns"
	unread=
	read_clusters=0
	secret_values=0
	short_values=0
	kc_files=0
	kc_values=0
	removed=0
	gather_patterns
	LC_ALL=C sort -u -o "$patterns" "$patterns"
	chmod 600 "$patterns"
	pattern_count=$(wc -l <"$patterns" | tr -d ' ')
	# grep -r does not follow symbolic links and cannot look inside compressed files.
	while IFS= read -r -d '' f; do
		rm -f "$f"
		note scrub "${f#"$out"/}: symbolic link, removed"
	done < <(find "$out" -type l -print0)
	while IFS= read -r -d '' f; do
		replace "$f" "was compressed, which the secret scrub cannot check"
	done < <(find "$out" -type f -name '*.gz' -print0)
	if [[ $pattern_count -gt 0 ]]; then
		# GNU grep matches many fixed strings at once; BSD grep (macOS) takes minutes on a large
		# bundle, so a Homebrew ggrep is preferred when there is one.
		local grep=grep
		have ggrep && grep=ggrep
		with_timeout "$grep_limit" env LC_ALL=C "$grep" -rlF --null -f "$patterns" -- "$out" \
			>"$work/matches" 2>"$work/grep.err" || rc=$?
		while IFS= read -r -d '' f; do
			[[ -f $f ]] || continue
			replace "$f" "contained the value of a Kubernetes Secret"
		done <"$work/matches"
	fi
	rm -f "$patterns"
	[[ -z $unread ]] || problems="the Secrets of $unread could not be read, so files holding only those values may remain"
	if [[ $scrub_only -eq 1 && -z $clusters ]]; then
		problems="no patrol-* kind cluster to read the Secret values from"
		[[ -z $list_failed ]] || problems="the patrol-* kind clusters could not be listed (kind get clusters failed or timed out)"
	fi
	if [[ $rc -gt 1 ]]; then
		problems="${problems:+$problems; }grep failed (exit $rc): $(head -n 1 "$work/grep.err")"
	fi
	if [[ -n $problems ]]; then
		scrub_status="INCOMPLETE: $problems"
	elif [[ $pattern_count -gt 0 ]]; then
		scrub_status=ok
	elif [[ $scrub_only -eq 0 && -z $clusters && $kc_files -eq 0 ]]; then
		# Nothing in the bundle came from a cluster (kind deletes a cluster whose creation failed).
		scrub_status="ok (nothing to scrub: no patrol-* kind cluster and no kubeconfig in $PATROL_KIND_CACHE, so only host state was collected)"
	else
		scrub_status="INCOMPLETE: no secret values could be read (clusters gone or API unreachable); nothing was scrubbed"
	fi
}

# --- main ------------------------------------------------------------------------------------------
if have kind && have docker; then
	if ! capture "$work/clusters" "$call_limit" lib_call patrol::kind_clusters; then
		list_failed=1
		note WARN "kind get clusters failed or timed out: no cluster was looked at"
		: >"$work/clusters"
	fi
	while IFS= read -r c; do
		if [[ $c =~ $cluster_re ]]; then clusters="$clusters $c"; fi
	done <"$work/clusters"
	clusters=${clusters# }
fi
clusters_note=${clusters:-none}
[[ -z $list_failed ]] || clusters_note="unknown (kind get clusters failed or timed out)"

if [[ $scrub_only -eq 0 ]]; then
	if [[ -n $timeout_bin ]]; then bound="each command bounded by $timeout_bin"; else bound="no timeout(1): commands unbounded"; fi
	echo "kind-diagnostics: collecting into $out (budget ${budget}s, $bound)"
	collect_host
	collect_kind
	for c in $clusters; do
		in_budget "k8s/$c" || continue
		# A cheap check (names only, no values) that the API answers before spending the budget on
		# it: the scrub reads the Secrets again at the end, and only that read can vouch.
		if ! usable "$c"; then
			skip "k8s/$c" "kubeconfig $(patrol::kubeconfig "$c") missing or not a local kind context"
		elif ! capture "$work/probe" "$call_limit" k "$c" get secrets -A -o name; then
			skip "k8s/$c" "the API did not list its Secret names (refused, failed or no answer within ${lim}s), and without its Secrets the scrub cannot vouch for its items"
		else
			collect_cluster "$c"
		fi
	done
fi

scrub
# shellcheck disable=SC2086 # counts the words of $clusters
summary="$secret_values Secret value(s) from $read_clusters of $(words $clusters) cluster(s) and $kc_values kubeconfig credential(s) from $kc_files file(s) checked ($short_values Secret value(s) shorter than 8 bytes, not matched), $removed file(s) replaced"
echo "kind-diagnostics: scrub $scrub_status; $summary"

if [[ $scrub_only -eq 1 ]]; then
	finished=1
	[[ $scrub_status == ok ]] || exit 3
	exit 0
fi

write_index "$scrub_status; $summary"
finished=1
collected=$(grep -c '^ok ' "$index" || true)
failed=$(grep -c '^FAIL ' "$index" || true)
skipped=$(grep -c '^skip ' "$index" || true)
size=$(du -sh "$out" | awk '{ print $1 }')
echo "kind-diagnostics: $collected item(s) collected, $failed failed, $skipped skipped in ${SECONDS}s; $size in $out (see INDEX.txt)"
