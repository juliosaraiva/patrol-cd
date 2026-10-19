#!/usr/bin/env bash
# Verifies a Patrol CD installation in the patrol-control kind cluster against the live
# clusters (make kind-verify). Run it after hack/kind/up.sh, test/e2e/fixtures/deploy.sh and
# hack/kind/install.sh.
#
# Usage: hack/kind/verify-install.sh [--namespace NS] [--lifecycle]
#
# Checks (each prints PASS or FAIL; the exit status is 1 when any failed):
#   pods         every Patrol pod is Running and ready, with no restarts
#   security     per component: uid 999, an empty capability bounding set (drop ALL), no_new_privs,
#                the root file system mounted read-only (read from /proc/self/mounts)
#   bootstrap    the bootstrap init container converged; the initial admin Secret exists
#   api          /healthz, anonymous requests refused, admin login with the initial password,
#                the default AppProject exists (the password only ever travels through pipes)
#   netpol       the repo-server reaches the fixtures on the ports the values allow and nothing
#                else: not the Kubernetes API, not the controller
#   metrics      the four metrics endpoints answer from another namespace
#   sync         every fixture Application is synced through the API (an Operation per
#                Application) and ends Synced and Healthy, with its workload running on
#                patrol-target
#   drift        a Deployment scaled directly on patrol-target is reported OutOfSync and a
#                resync restores it
#
# The sync, drift and lifecycle sections change state (they sync the fixture Applications, scale a
# Deployment on patrol-target, and with --lifecycle reinstall the release). Each of them starts only
# when every check before it passed; the run stops there with exit status 1 otherwise. The pods
# section runs first: when Patrol CD is not installed and healthy nothing else is touched.
#
# The API address is https://127.0.0.1:8443 (the NodePort of hack/kind/values.yaml); PATROL_API may
# set another port on 127.0.0.1, localhost or [::1], never another host (the initial admin password
# is sent there).
#
# --lifecycle adds the disruptive checks: helm rollback to revision 1 and upgrade again; the
# admin password changed through the API; helm uninstall and re-install. The changed and the old
# initial password must be refused afterwards (a new initial password is generated), while the
# CRDs, the Applications and the workloads on patrol-target survive. The fixtures are then
# registered again (the re-install drops the fixture git server's host key) and the run waits until
# e2e-guestbook-ssh has really been compared again. A failed rollback, upgrade, uninstall or
# install stops the run with the commands that put the installation back.
set -uo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=hack/kind/lib.sh
source "$here/lib.sh"

usage() { awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "${BASH_SOURCE[0]}"; }

ns=patrol
lifecycle=0
while [[ $# -gt 0 ]]; do
	case "$1" in
	--namespace)
		[[ $# -ge 2 ]] || patrol::die "--namespace needs a value"
		ns=$2
		shift 2
		;;
	--lifecycle)
		lifecycle=1
		shift
		;;
	-h | --help)
		usage
		exit 0
		;;
	*) patrol::die "unexpected argument: $1 (see --help)" ;;
	esac
done
[[ $ns =~ ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$ ]] || patrol::die "invalid namespace name: $ns"

patrol::require_tools kubectl curl helm
control=$PATROL_CONTROL_CLUSTER
target=$PATROL_TARGET_CLUSTER
patrol::cluster_exists "$control" || patrol::die "kind cluster $control does not exist (run hack/kind/up.sh)"
patrol::cluster_exists "$target" || patrol::die "kind cluster $target does not exist (run hack/kind/up.sh)"
api=${PATROL_API:-https://127.0.0.1:8443}
[[ $api =~ ^https://(127\.0\.0\.1|localhost|\[::1\])(:[0-9]{1,5})?$ ]] ||
	patrol::die "PATROL_API must be an https address on 127.0.0.1, localhost or [::1] (got '$api')"

work=$(mktemp -d)
# lifecycle_dirty is set while the --lifecycle run has left the environment in a state that needs
# a person's attention (a changed admin password, a removed release); the trap says so on every way out.
lifecycle_dirty=""
finish() {
	rm -rf "$work"
	[[ -z $lifecycle_dirty ]] || echo "verify-install: WARNING: $lifecycle_dirty" >&2
}
trap finish EXIT
chmod 700 "$work"

pass=0
fail=0
ok() {
	pass=$((pass + 1))
	printf 'PASS  %-8s %s\n' "$1" "$2"
}
bad() {
	fail=$((fail + 1))
	printf 'FAIL  %-8s %s\n' "$1" "$2"
}
# check AREA DESCRIPTION -- CMD...: PASS when CMD succeeds.
check() {
	local area=$1 desc=$2
	shift 3
	if "$@" >"$work/out" 2>&1; then ok "$area" "$desc"; else
		bad "$area" "$desc"
		sed 's/^/        /' "$work/out" | head -8
	fi
}
# check_not AREA DESCRIPTION -- CMD...: PASS when CMD fails.
check_not() {
	local area=$1 desc=$2
	shift 3
	if "$@" >"$work/out" 2>&1; then bad "$area" "$desc (it succeeded)"; else ok "$area" "$desc"; fi
}
k() { patrol::kubectl "$control" -n "$ns" "$@"; }
kt() { patrol::kubectl "$target" "$@"; }
# eq AREA DESCRIPTION ACTUAL WANT
eq() {
	if [[ "$3" == "$4" ]]; then ok "$1" "$2"; else bad "$1" "$2: got '$3', want '$4'"; fi
}
# until_true SECONDS CMD...: polls every 3 s.
until_true() {
	local end=$((SECONDS + $1))
	shift
	while ((SECONDS < end)); do
		"$@" >/dev/null 2>&1 && return 0
		sleep 3
	done
	return 1
}
# gate WHAT: the run stops here, before a section that changes state, unless every check so far passed.
gate() {
	[[ $fail -eq 0 ]] && return 0
	echo
	echo "verify-install: earlier checks failed; not running $1 (state-changing)" >&2
	echo "verify-install: $pass passed, $fail failed"
	exit 1
}
pod_of() { k get pod -l "app.kubernetes.io/name=$1" -o name 2>/dev/null | head -n 1; }

# --- API helpers: passwords travel through files and pipes only, never argv or output.
secret_value() { k get secret "$1" -o jsonpath="{.data.$2}" | base64 -d; }
# login USER_PASSWORD_FILE [TOKEN_OUT]: prints the HTTP status.
login() {
	local pwfile=$1 out=${2:-/dev/null}
	{
		printf '{"username":"admin","password":"'
		cat "$pwfile"
		printf '"}'
	} | curl -sk -m 20 -X POST "$api/api/v1/session" -H 'Content-Type: application/json' --data-binary @- -o "$out" -w '%{http_code}'
}
# auth_header TOKEN_JSON HEADER_FILE
auth_header() { sed -n 's/.*"token":"\([^"]*\)".*/Authorization: Bearer \1/p' "$1" >"$2"; }

echo "== pods"
rows=$(k get pods -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.phase}{" "}{.status.containerStatuses[*].ready}{" "}{.status.containerStatuses[*].restartCount}{"\n"}{end}')
npods=$(grep -c . <<<"$rows")
eq pods "number of Patrol pods" "$npods" 5
badpods=$(awk '$2 != "Running" || $3 !~ /^true( true)*$/ || $4 !~ /^0( 0)*$/ {print $1}' <<<"$rows")
eq pods "all Running, ready, 0 restarts" "${badpods:-none}" none
# Nothing below may change anything unless Patrol CD is installed and healthy in this namespace.
if [[ $fail -ne 0 ]]; then
	echo
	echo "verify-install: Patrol CD is not installed and healthy in $control/$ns; stopping before any check that changes state" >&2
	echo "verify-install: $pass passed, $fail failed"
	exit 1
fi

echo "== security (effective, inside each pod)"
# The root file system is read-only when the mount options of / say so (a write test would fail for
# a non-root user on root-owned directories whatever the options); the bounding set is empty only
# when capabilities were dropped (the effective set of a non-root process is empty regardless).
# shellcheck disable=SC2016 # the script is expanded inside the pod, not here
probe='ro=writable
while read -r _ mp _ opts _; do
	if [ "$mp" = / ]; then
		case ",$opts," in *,ro,*) ro=read-only ;; *) ro=writable ;; esac
	fi
done </proc/self/mounts
printf "%s %s %s %s\n" "$(id -u)" "$(grep CapBnd /proc/self/status | cut -f2)" "$(grep NoNewPrivs /proc/self/status | cut -f2)" "$ro"'
for c in patrol-server patrol-repo-server patrol-applicationset-controller patrol-notifications-controller patrol-controller; do
	p=$(pod_of "$c")
	[[ -n $p ]] || {
		bad security "$c: no pod"
		continue
	}
	got=$(k exec "$p" -- sh -c "$probe" 2>&1 | tail -n 1)
	eq security "$c: uid, capability bounding set, no_new_privs, root fs" "$got" "999 0000000000000000 1 read-only"
done

echo "== bootstrap"
srv=$(pod_of patrol-server)
bootstrap_converged() { k logs "${srv#pod/}" -c bootstrap | grep '"msg":"converged"' >/dev/null; }
check bootstrap "init container converged" -- bootstrap_converged
check bootstrap "initial admin Secret exists" -- k get secret patrol-initial-admin-secret

echo "== api ($api)"
eq api "/healthz" "$(curl -sk -m 10 -o /dev/null -w '%{http_code}' "$api/healthz")" 200
eq api "anonymous project list is refused" "$(curl -sk -m 10 -o /dev/null -w '%{http_code}' "$api/api/v1/projects")" 401
secret_value patrol-initial-admin-secret password >"$work/pw"
eq api "admin login with the initial password" "$(login "$work/pw" "$work/session.json")" 200
auth_header "$work/session.json" "$work/auth.hdr"
default_project_listed() { curl -sk -m 20 -H @"$work/auth.hdr" "$api/api/v1/projects" | grep '"name":"default"' >/dev/null; }
check api "default AppProject exists" -- default_project_listed

echo "== netpol (repo-server egress)"
rs=$(pod_of patrol-repo-server)
nc_from_rs() { k exec "$rs" -- nc -z -w 4 "$1" "$2"; }
check netpol "git-server Service port 80 (pod port 8080) is allowed" -- nc_from_rs git-server.patrol-e2e.svc.cluster.local 80
check netpol "git-server Service port 22 (pod port 2222) is allowed" -- nc_from_rs git-server.patrol-e2e.svc.cluster.local 22
check netpol "patrol-registry:5000 is allowed" -- nc_from_rs patrol-registry 5000
check_not netpol "the Kubernetes API is refused" -- nc_from_rs kubernetes.default.svc 443
check_not netpol "the controller (port 8090) is refused" -- nc_from_rs "patrol-controller.$ns.svc.cluster.local" 8090

echo "== metrics (scraped from namespace patrol-e2e)"
for t in patrol-controller-metrics:8082 patrol-server-metrics:8083 patrol-repo-server:8084 patrol-notifications-controller-metrics:9001; do
	h=${t%%:*}
	port=${t##*:}
	code=$(patrol::kubectl "$control" -n patrol-e2e exec toolbox -- curl -s -m 8 -o /dev/null -w '%{http_code}' "http://$h.$ns.svc.cluster.local:$port/metrics" 2>&1 | tail -n 1)
	eq metrics "$t" "$code" 200
done

gate "the sync, drift and lifecycle sections"

echo "== sync (through the API)"
apps=$(k get applications.cd.patrol.example -l app.kubernetes.io/part-of=patrol-e2e-fixtures -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')
napps=$(grep -c . <<<"$apps")
eq sync "fixture Applications registered" "$napps" 7
app_state() { k get applications.cd.patrol.example "$1" -o jsonpath='{.status.sync.status}/{.status.health.status}'; }
app_result() { k get applications.cd.patrol.example "$1" -o jsonpath='{.status.sync.status}/{.status.health.status}/{.status.operationState.phase}'; }
app_compared() { [[ "$(app_state "$1")" =~ ^(Synced|OutOfSync)/ ]]; }
app_op() { k get applications.cd.patrol.example "$1" -o jsonpath='{.status.operationState.operationName}'; }
# app_done APP: a sync operation newer than the one recorded in $work/op.APP ended Synced/Healthy/Succeeded
# (an earlier success must not satisfy the check).
app_done() { [[ "$(app_op "$1")" != "$(cat "$work/op.$1")" && "$(app_result "$1")" == Synced/Healthy/Succeeded ]]; }
request_sync() { app_op "$1" >"$work/op.$1"; api_sync "$1"; }
api_sync() { curl -sk -m 30 -X POST -H @"$work/auth.hdr" -H 'Content-Type: application/json' -d '{}' "$api/api/v1/applications/$1/sync?appNamespace=$ns" -o "$work/sync-response.$1" -w '%{http_code}'; }
# Every Application must have compared once (not Unknown) before it is synced.
for a in $apps; do
	until_true 150 app_compared "$a" || bad sync "$a: no comparison result (status $(app_state "$a"))"
done
for a in $apps; do
	code=$(request_sync "$a")
	if [[ $code == 200 ]]; then
		ok sync "$a: sync request accepted"
	else
		bad sync "$a: sync request refused with HTTP $code: $(sed -n 's/.*"detail":"\([^"]*\)".*/\1/p' "$work/sync-response.$a" | head -c 300)"
	fi
done
for a in $apps; do
	# The wait and the verdict are the same condition: a new operation (not the one recorded before
	# the request) that ended Synced/Healthy/Succeeded. An earlier success never counts.
	if until_true 180 app_done "$a"; then
		ok sync "$a: new operation $(app_op "$a") ended Synced/Healthy/Succeeded"
	else
		bad sync "$a: no new operation ended Synced/Healthy/Succeeded (operation '$(app_op "$a")', before: '$(cat "$work/op.$a")', result $(app_result "$a"))"
	fi
done
if deployments=$(kt get deploy -A -o jsonpath='{range .items[*]}{.metadata.namespace}{"/"}{.metadata.name}{" "}{.status.availableReplicas}{"/"}{.spec.replicas}{"\n"}{end}'); then
	eq sync "e2e workloads present on $target" "$(awk '$1 ~ /^e2e-/ {n++} END {print n + 0}' <<<"$deployments")" 7
	unavailable=$(awk '$1 ~ /^e2e-/ {split($2, r, "/"); if (r[1] != r[2]) print $1}' <<<"$deployments")
	eq sync "e2e workloads on $target are fully available" "${unavailable:-none}" none
else
	bad sync "cannot list the Deployments on $target"
fi

gate "the drift section"

echo "== drift"
app_outofsync() { [[ "$(app_state e2e-guestbook)" == OutOfSync/* ]]; }
app_synced_healthy() { [[ "$(app_state e2e-guestbook)" == Synced/Healthy ]] && app_done e2e-guestbook; }
kt -n e2e-guestbook scale deploy guestbook-ui --replicas=5 >/dev/null
if until_true 90 app_outofsync; then ok drift "scaling a Deployment on $target is reported OutOfSync"; else bad drift "drift not detected within 90 s"; fi
request_sync e2e-guestbook >/dev/null
until_true 120 app_synced_healthy
eq drift "a new sync operation restores Synced/Healthy" "$(app_state e2e-guestbook)/$([[ "$(app_op e2e-guestbook)" != "$(cat "$work/op.e2e-guestbook")" ]] && echo new-operation || echo same-operation)" Synced/Healthy/new-operation
eq drift "and the replica count of 1" "$(kt -n e2e-guestbook get deploy guestbook-ui -o jsonpath='{.spec.replicas}')" 1

if [[ $lifecycle -eq 1 ]]; then
	gate "the lifecycle section"
	echo "== lifecycle (helm rollback, password change, uninstall, re-install)"
	helmc() { patrol::helm "$control" -n "$ns" "$@"; }
	# The commands that put the environment back, for the messages below.
	kc=$(patrol::kubeconfig "$control")
	recover="helm --kubeconfig $kc --kube-context kind-$control -n $ns uninstall patrol; make kind-install"
	# lifecycle_stop REASON: the environment cannot be trusted past this point; say so and stop.
	lifecycle_stop() {
		echo
		echo "verify-install: lifecycle stopped: $1" >&2
		echo "verify-install: $pass passed, $fail failed"
		exit 1
	}
	rel=patrol
	check lifecycle "helm rollback to revision 1" -- helmc rollback "$rel" 1 --wait --timeout 4m
	check lifecycle "helm upgrade back to the kind values" -- helmc upgrade "$rel" "$PATROL_ROOT/deploy/helm/patrol-cd" --values "$here/values.yaml" --wait --timeout 5m
	[[ $fail -eq 0 ]] || lifecycle_stop "the release could not be rolled back and upgraded; nothing else was changed. Run: make kind-install"
	# A new admin password through the API; the initial one must stop working.
	LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom | head -c 28 >"$work/pw-changed"
	{
		printf '{"currentPassword":"'
		cat "$work/pw"
		printf '","newPassword":"'
		cat "$work/pw-changed"
		printf '"}'
	} | curl -sk -m 20 -X PUT "$api/api/v1/account/password" -H @"$work/auth.hdr" -H 'Content-Type: application/json' --data-binary @- -o /dev/null -w '%{http_code}' >"$work/code"
	eq lifecycle "admin password changed through the API" "$(cat "$work/code")" 200
	[[ $fail -eq 0 ]] || lifecycle_stop "the admin password could not be changed through the API; nothing was uninstalled"
	lifecycle_dirty="the admin password was changed to a throwaway value and the release is about to be, or was, removed; if the run stopped here, run: $recover (a new initial password is generated)"
	eq lifecycle "login with the changed password" "$(login "$work/pw-changed")" 200
	eq lifecycle "login with the old initial password is refused" "$(login "$work/pw")" 401
	old_uid=$(k get secret patrol-initial-admin-secret -o jsonpath='{.metadata.uid}')
	check lifecycle "helm uninstall" -- helmc uninstall "$rel" --wait --timeout 4m
	[[ $fail -eq 0 ]] || lifecycle_stop "helm uninstall failed; the release may be partly removed and the admin password was changed. Run: $recover"
	eq lifecycle "CRDs survive the uninstall" "$(patrol::kubectl "$control" get crd -o name | grep -c cd.patrol.example)" 8
	eq lifecycle "Applications survive the uninstall" "$(k get applications.cd.patrol.example -o name | grep -c .)" 7
	no_pods() { [[ -z "$(k get pods -o name)" ]]; }
	until_true 120 no_pods || true
	check lifecycle "helm install again" -- helmc install "$rel" "$PATROL_ROOT/deploy/helm/patrol-cd" --values "$here/values.yaml" --wait --timeout 6m
	[[ $fail -eq 0 ]] || lifecycle_stop "the release is not installed. Run: make kind-install (if helm reports a ConfigMap that it does not own, delete that ConfigMap first)"
	lifecycle_dirty=""
	new_uid=$(k get secret patrol-initial-admin-secret -o jsonpath='{.metadata.uid}')
	if [[ -n $new_uid && $new_uid != "$old_uid" ]]; then
		ok lifecycle "a new initial admin Secret was generated (new UID)"
	else
		bad lifecycle "initial admin Secret was reused (uid $new_uid)"
	fi
	secret_value patrol-initial-admin-secret password >"$work/pw-new"
	healthz_ok() { [[ "$(curl -sk -m 5 -o /dev/null -w '%{http_code}' "$api/healthz")" == 200 ]]; }
	until_true 90 healthz_ok
	eq lifecycle "login with the changed password is refused" "$(login "$work/pw-changed")" 401
	eq lifecycle "login with the old initial password is refused" "$(login "$work/pw")" 401
	eq lifecycle "login with the new initial password" "$(login "$work/pw-new")" 200
	eq lifecycle "workloads on $target keep running" "$(kt get deploy -A --no-headers | grep -c e2e-)" 7
	# The chart recreated ConfigMap patrol-ssh-known-hosts-cm, which loses the fixture git server's
	# host key. Register the fixtures again so that the environment is left as it was found.
	check lifecycle "fixtures registered again (host key, repositories, Applications)" -- "$PATROL_ROOT/test/e2e/fixtures/patrol/register.sh" --namespace "$ns" --with-applications
	# The kubelet refreshes the mounted ConfigMap file with a delay; refresh the Application only
	# once the repo-server can see the key.
	patrol::wait_fixture_host_key "$ns" 180 || true
	# The Application keeps the status it had before the uninstall, and the new controller does not
	# write repository errors to it for the first 180 s: a Synced status alone proves nothing. Ask
	# for a hard refresh and wait until the controller has consumed the request (the annotation is
	# gone) and recorded a new comparison time.
	app_reconciled_at() { k get applications.cd.patrol.example e2e-guestbook-ssh -o jsonpath='{.status.reconciledAt}'; }
	refresh_pending() { k get applications.cd.patrol.example e2e-guestbook-ssh -o jsonpath='{.metadata.annotations.cd\.patrol\.example/refresh}'; }
	before=$(app_reconciled_at)
	k annotate applications.cd.patrol.example e2e-guestbook-ssh "cd.patrol.example/refresh=hard" --overwrite >/dev/null
	ssh_app_recompared() {
		[[ -z "$(refresh_pending)" && "$(app_reconciled_at)" != "$before" && "$(app_state e2e-guestbook-ssh)" == Synced/Healthy ]]
	}
	if until_true 180 ssh_app_recompared; then
		ok lifecycle "e2e-guestbook-ssh is compared again (reconciledAt $before -> $(app_reconciled_at)) and Synced/Healthy with the restored host key"
	else bad lifecycle "e2e-guestbook-ssh was not compared again after the refresh (reconciledAt '$(app_reconciled_at)' since '$before', refresh annotation '$(refresh_pending)', status $(app_state e2e-guestbook-ssh))"; fi
fi

echo
echo "verify-install: $pass passed, $fail failed"
[[ $fail -eq 0 ]]
