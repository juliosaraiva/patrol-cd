# shellcheck shell=bash
# Starts and stops a throwaway etcd + kube-apiserver from the envtest binaries
# (KUBEBUILDER_ASSETS), with no Docker and no network. Sourced by test/e2e/fixtures/validate.sh.
#
#   envtest_start DIR [CONTEXT]   start; writes DIR/kubeconfig (context CONTEXT, default envtest)
#                                 and sets ENVTEST_KUBECONFIG, ENVTEST_CONTEXT, ENVTEST_SERVER
#   envtest_stop                  stop both processes (also registered by the caller's EXIT trap)
#
# The API server mirrors controller-runtime envtest defaults (RBAC, ServiceAccount admission
# disabled, privileged allowed) and authenticates a static admin token in system:masters.
#
# ENVTEST_* variables are read by the sourcing script.
# shellcheck disable=SC2034

ENVTEST_PIDS=""

envtest_free_port() {
	local p
	for _ in $(seq 1 50); do
		p=$((20000 + RANDOM % 30000))
		if ! (echo >"/dev/tcp/127.0.0.1/$p") 2>/dev/null; then
			printf '%s\n' "$p"
			return 0
		fi
	done
	return 1
}

envtest_start() {
	local dir=$1 ctx=${2:-envtest} assets=${KUBEBUILDER_ASSETS:?KUBEBUILDER_ASSETS is not set}
	local etcd_port peer_port api_port token
	[[ -x $assets/etcd && -x $assets/kube-apiserver ]] || {
		echo "envtest: etcd/kube-apiserver not found in $assets" >&2
		return 1
	}
	mkdir -p "$dir/etcd" "$dir/certs"
	etcd_port=$(envtest_free_port)
	peer_port=$(envtest_free_port)
	api_port=$(envtest_free_port)

	"$assets/etcd" --data-dir "$dir/etcd" \
		--listen-client-urls "http://127.0.0.1:$etcd_port" --advertise-client-urls "http://127.0.0.1:$etcd_port" \
		--listen-peer-urls "http://127.0.0.1:$peer_port" --initial-advertise-peer-urls "http://127.0.0.1:$peer_port" \
		--initial-cluster "default=http://127.0.0.1:$peer_port" --name default \
		--unsafe-no-fsync --log-level error >"$dir/etcd.log" 2>&1 &
	ENVTEST_PIDS="$ENVTEST_PIDS $!"

	openssl genrsa -out "$dir/certs/sa.key" 2048 2>/dev/null
	openssl rsa -in "$dir/certs/sa.key" -pubout -out "$dir/certs/sa.pub" 2>/dev/null
	token=$(openssl rand -hex 32)
	printf '%s,envtest-admin,envtest-admin,system:masters\n' "$token" >"$dir/certs/tokens.csv"
	chmod 600 "$dir/certs/tokens.csv"

	"$assets/kube-apiserver" \
		--etcd-servers "http://127.0.0.1:$etcd_port" \
		--cert-dir "$dir/certs" \
		--secure-port "$api_port" --bind-address 127.0.0.1 --advertise-address 127.0.0.1 \
		--service-cluster-ip-range 10.0.0.0/24 \
		--service-account-issuer "https://127.0.0.1:$api_port/" \
		--service-account-key-file "$dir/certs/sa.pub" \
		--service-account-signing-key-file "$dir/certs/sa.key" \
		--token-auth-file "$dir/certs/tokens.csv" \
		--authorization-mode RBAC \
		--disable-admission-plugins ServiceAccount \
		--allow-privileged=true >"$dir/kube-apiserver.log" 2>&1 &
	ENVTEST_PIDS="$ENVTEST_PIDS $!"

	ENVTEST_SERVER="https://127.0.0.1:$api_port"
	ENVTEST_CONTEXT=$ctx
	ENVTEST_KUBECONFIG="$dir/kubeconfig"
	# The API server writes a self-signed serving certificate (with its CA) to
	# cert-dir/apiserver.crt when it starts; the kubeconfig trusts exactly that file.
	for _ in $(seq 1 120); do
		[[ -s $dir/certs/apiserver.crt ]] && break
		sleep 0.5
	done
	[[ -s $dir/certs/apiserver.crt ]] || {
		echo "envtest: kube-apiserver did not write its serving certificate; last log lines:" >&2
		tail -n 20 "$dir/kube-apiserver.log" >&2
		return 1
	}
	kubectl config --kubeconfig "$ENVTEST_KUBECONFIG" set-cluster "$ctx" --server "$ENVTEST_SERVER" \
		--certificate-authority "$dir/certs/apiserver.crt" --embed-certs=true >/dev/null
	kubectl config --kubeconfig "$ENVTEST_KUBECONFIG" set-credentials "$ctx" --token "$token" >/dev/null
	kubectl config --kubeconfig "$ENVTEST_KUBECONFIG" set-context "$ctx" --cluster "$ctx" --user "$ctx" >/dev/null
	kubectl config --kubeconfig "$ENVTEST_KUBECONFIG" use-context "$ctx" >/dev/null
	chmod 600 "$ENVTEST_KUBECONFIG"

	for _ in $(seq 1 120); do
		if kubectl --kubeconfig "$ENVTEST_KUBECONFIG" --context "$ctx" get --raw /readyz >/dev/null 2>&1; then
			return 0
		fi
		sleep 0.5
	done
	echo "envtest: kube-apiserver did not become ready; last log lines:" >&2
	tail -n 20 "$dir/kube-apiserver.log" >&2
	kubectl --kubeconfig "$ENVTEST_KUBECONFIG" --context "$ctx" get --raw /readyz >&2 || true
	return 1
}

envtest_stop() {
	# PIDs were recorded etcd first; stop in reverse order (API server before etcd: an API server
	# whose etcd vanished keeps retrying instead of exiting). SIGKILL after 10 seconds.
	local pid order="" _
	for pid in $ENVTEST_PIDS; do order="$pid $order"; done
	# The group redirect also silences bash's "Terminated" job notices.
	{
		for pid in $order; do
			kill "$pid" || continue
			for _ in $(seq 1 20); do
				kill -0 "$pid" || break
				sleep 0.5
			done
			kill -9 "$pid" || true
			wait "$pid" || true
		done
	} 2>/dev/null
	ENVTEST_PIDS=""
}
