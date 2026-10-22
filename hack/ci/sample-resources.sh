#!/usr/bin/env bash
# Appends a line with CPU load, memory, disk and Docker container usage every INTERVAL seconds until
# it is killed. The kind job runs it in the background so that the resource needs of the three-node
# environment on a 2 vCPU runner are measured, not guessed (the numbers go into the run record).
#
# Usage: hack/ci/sample-resources.sh OUTFILE [INTERVAL_SECONDS]
set -u

out=${1:?usage: sample-resources.sh OUTFILE [INTERVAL_SECONDS]}
interval=${2:-30}
printf 'time_utc\tload1\tmem_used_mb\tmem_avail_mb\tswap_used_mb\troot_used_gb\troot_avail_gb\tcontainers\tdocker_cpu_pct\n' >"$out"
while true; do
	load=$(cut -d' ' -f1 /proc/loadavg 2>/dev/null || echo n/a)
	mem=$(free -m 2>/dev/null | awk '/^Mem:/ {print $3 "\t" $7}')
	swap=$(free -m 2>/dev/null | awk '/^Swap:/ {print $3}')
	disk=$(df -BG / 2>/dev/null | awk 'NR == 2 {gsub("G", ""); print $3 "\t" $4}')
	containers=$(docker ps -q 2>/dev/null | wc -l | tr -d ' ')
	dcpu=$(docker stats --no-stream --format '{{.CPUPerc}}' 2>/dev/null | tr -d '%' | awk '{s += $1} END {printf "%.1f", s}')
	printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$(date -u +%FT%TZ)" "$load" "${mem:-n/a	n/a}" "${swap:-n/a}" "${disk:-n/a	n/a}" "${containers:-0}" "${dcpu:-0}" >>"$out"
	sleep "$interval"
done
