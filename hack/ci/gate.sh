#!/usr/bin/env bash
# The aggregator step of .github/workflows/ci.yml: one status that stands for the whole workflow.
# Reads the results of the needed jobs as NAME=RESULT arguments (RESULT is success, failure,
# cancelled or skipped, as in `needs.<job>.result`) and fails when any job failed or was cancelled.
# A skipped job is fine only when the tier that owns it was switched off on purpose; the caller
# lists those jobs with --optional.
#
# Usage: hack/ci/gate.sh [--optional NAME,NAME] NAME=RESULT...
set -euo pipefail

optional=,
if [[ ${1:-} == --optional ]]; then
	optional=,${2:?--optional needs a comma separated list},
	shift 2
fi

bad=0
for arg in "$@"; do
	name=${arg%%=*}
	result=${arg#*=}
	case $result in
	success) printf 'ok       %s\n' "$name" ;;
	skipped)
		if [[ $optional == *",$name,"* ]]; then
			printf 'skipped  %s (switched off for this event)\n' "$name"
		else
			printf 'SKIPPED  %s: a job that must run was skipped (an upstream job failed?)\n' "$name"
			bad=1
		fi
		;;
	*)
		printf 'FAILED   %s: %s\n' "$name" "$result"
		bad=1
		;;
	esac
done
exit $bad
