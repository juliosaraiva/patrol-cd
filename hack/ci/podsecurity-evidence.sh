#!/usr/bin/env bash
# Prints what TestPodSecurityRestricted exercised, from the `go test -json` stream of the deploy
# suite: the accounting line the test logs (profiles, pods and containers admitted by a namespace that
# enforces the "restricted" Pod Security level) and the subtests that passed. The test fails on its
# own when a profile is skipped or nothing is admitted; this is the record of what it covered, for the
# job summary.
#
#   hack/ci/podsecurity-evidence.sh [dist/ci/deploy.json]
set -euo pipefail

events=${1:-dist/ci/deploy.json}
summary=${GITHUB_STEP_SUMMARY:-/dev/null}

if [[ ! -s $events ]]; then
	echo "podsecurity-evidence: $events not found or empty" >&2
	exit 1
fi
command -v jq >/dev/null 2>&1 || {
	echo "podsecurity-evidence: jq not found" >&2
	exit 1
}

test_result=$(jq -rs '[.[] | select(.Test == "TestPodSecurityRestricted" and (.Action == "pass" or .Action == "fail" or .Action == "skip")) | .Action] | last // "none"' "$events")
line=$(jq -rs '[.[] | select(.Test == "TestPodSecurityRestricted" and .Action == "output") | .Output | select(contains("Pod Security restricted:"))] | first // ""' "$events" | sed 's/^[[:space:]]*//; s/[[:space:]]*$//')
passed=$(jq -rs '[.[] | select(.Action == "pass" and ((.Test // "") | startswith("TestPodSecurityRestricted/"))) | .Test | sub("^TestPodSecurityRestricted/"; "")] | sort | join(" ")' "$events")
failed=$(jq -rs '[.[] | select(.Action == "fail" and ((.Test // "") | startswith("TestPodSecurityRestricted/"))) | .Test | sub("^TestPodSecurityRestricted/"; "")] | sort | join(" ")' "$events")

{
	echo "### Pod Security (restricted) coverage"
	echo
	echo "- test result: \`$test_result\`"
	echo "- accounting line: \`${line:-none}\`"
	echo "- subtests passed: ${passed:-none}"
	[[ -z $failed ]] || echo "- subtests failed: $failed"
} | tee -a "$summary"

[[ $test_result == pass && -n $line && -z $failed ]]
