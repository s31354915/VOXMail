#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -ne 1 ] || [[ ! "$1" =~ ^[0-9a-f]{40}$ ]]; then
	echo "usage: $0 SOURCE_SHA (40 lowercase hexadecimal characters)" >&2
	exit 2
fi
: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"

source_sha=$1
max_attempts=${VOXMAIL_CI_MAX_ATTEMPTS:-240}
poll_seconds=${VOXMAIL_CI_POLL_SECONDS:-15}
if [[ ! "$max_attempts" =~ ^[1-9][0-9]*$ ]] || [[ ! "$poll_seconds" =~ ^[0-9]+$ ]]; then
	echo "invalid CI polling bounds" >&2
	exit 2
fi

# Query one named producer workflow and exact commit, not similarly named
# check runs from another workflow or an older source revision.
endpoint="repos/${GITHUB_REPOSITORY}/actions/workflows/ci.yml/runs"
last_state="missing"
for ((attempt = 1; attempt <= max_attempts; attempt++)); do
	if ! response=$(gh api --method GET "$endpoint" -f "head_sha=$source_sha" -f per_page=100 2>&1); then
		echo "CI status API error for workflow ci.yml at ${source_sha}:" >&2
		printf '%s\n' "$response" >&2
		exit 1
	fi
	if ! matching_runs=$(jq -ce --arg sha "$source_sha" '
		if (.workflow_runs | type) != "array" then error("workflow_runs is not an array") else
			[.workflow_runs[] | select(.head_sha == $sha)]
		end
	' <<<"$response" 2>&1); then
		echo "CI status API response error for workflow ci.yml at ${source_sha}:" >&2
		printf '%s\n' "$matching_runs" >&2
		exit 1
	fi

	if [ "$matching_runs" = "[]" ]; then
		last_state="missing"
	else
		if ! run=$(jq -ce 'max_by([.run_number, .run_attempt])' <<<"$matching_runs" 2>&1); then
			echo "CI status API response error selecting the latest exact-source run:" >&2
			printf '%s\n' "$run" >&2
			exit 1
		fi
		run_id=$(jq -r '.id' <<<"$run")
		run_number=$(jq -r '.run_number' <<<"$run")
		run_attempt=$(jq -r '.run_attempt' <<<"$run")
		status=$(jq -r '.status' <<<"$run")
		conclusion=$(jq -r '.conclusion // ""' <<<"$run")
		last_state="run=${run_id} attempt=${run_attempt} status=${status} conclusion=${conclusion:-pending}"
		case "$status" in
			completed)
				if [ "$conclusion" = "success" ]; then
					echo "CI workflow run ${run_number} attempt ${run_attempt} succeeded for ${source_sha}"
					exit 0
				fi
				echo "CI workflow run ${run_number} attempt ${run_attempt} failed for ${source_sha}: ${conclusion:-missing conclusion}" >&2
				exit 1
				;;
			queued|in_progress|waiting|requested|pending)
				;;
			*)
				echo "CI status API returned unsupported run status ${status} for ${source_sha}" >&2
				exit 1
				;;
		esac
	fi

	if [ "$attempt" -eq "$max_attempts" ]; then
		echo "timed out waiting for the exact-source CI workflow after ${max_attempts} attempts: ${last_state}" >&2
		exit 1
	fi
	if [ "$last_state" = "missing" ]; then
		echo "CI workflow run for ${source_sha} is missing (attempt ${attempt}/${max_attempts})"
	else
		echo "CI workflow is still running for ${source_sha} (${last_state}; attempt ${attempt}/${max_attempts})"
	fi
	sleep "$poll_seconds"
done
