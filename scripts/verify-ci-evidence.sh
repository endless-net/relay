#!/usr/bin/env bash
set -euo pipefail

commit=${1:?exact commit SHA is required}
wait_seconds=${2:-0}
[[ "$commit" =~ ^[0-9a-f]{40}$ ]] || { echo 'A full lowercase commit SHA is required' >&2; exit 2; }
[[ "$wait_seconds" =~ ^[0-9]+$ ]] || { echo 'Wait duration must be a nonnegative integer' >&2; exit 2; }
repository=${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}
gh_bin=${GH_BIN:-gh}
deadline=$((SECONDS + wait_seconds))

# Only main push runs qualify. PR merge commits, manual runs and tag releases
# cannot provide evidence, even if GitHub reports them as successful.
while :; do
  runs=$("$gh_bin" api --method GET "repos/$repository/actions/workflows/ci.yml/runs" \
    -f head_sha="$commit" -f branch=main -f event=push -f per_page=100)
  run=$(jq -c --arg sha "$commit" '[.workflow_runs[] |
    select(.head_sha == $sha and .head_branch == "main" and .event == "push" and
      .path == ".github/workflows/ci.yml")] | sort_by(.run_number) | last // empty' <<< "$runs")
  if [[ -n "$run" ]]; then
    state=$(jq -r '.status' <<< "$run")
    if [[ "$state" == completed ]]; then
      if [[ $(jq -r '.conclusion' <<< "$run") != success ]]; then
        echo 'Latest main CI for the release commit did not succeed' >&2
        exit 1
      fi
      break
    fi
  fi
  if (( SECONDS >= deadline )); then
    echo 'No completed successful main CI for the exact release commit' >&2
    exit 1
  fi
  echo 'Waiting for main CI for the exact release commit' >&2
  remaining=$((deadline - SECONDS))
  sleep "$((remaining < 10 ? remaining : 10))"
done

run_id=$(jq -er '.id | select(type == "number" and . > 0) | tostring' <<< "$run")
jobs=$("$gh_bin" api --paginate --slurp "repos/$repository/actions/runs/$run_id/jobs?filter=latest&per_page=100")
for required in verify 'product-e2e / images' 'product-e2e / storage'; do
  jq -e --arg name "$required" '[.[].jobs[] | select(.name == $name)] |
    length == 1 and all(.[]; .status == "completed" and .conclusion == "success")' <<< "$jobs" >/dev/null || {
    echo "Missing successful CI job: $required" >&2
    exit 1
  }
done

# GitHub truncates long matrix job names. The group prefix remains stable.
for group in protocol-auth spiffe-mesh custom-domain trust-bootstrap snapshot fencing-recovery resources-lifecycle; do
  prefix="product-e2e / scenarios ($group,"
  jq -e --arg prefix "$prefix" '[.[].jobs[] | select(.name | startswith($prefix))] |
    length == 1 and all(.[]; .status == "completed" and .conclusion == "success")' <<< "$jobs" >/dev/null || {
    echo "Missing successful product E2E group: $group" >&2
    exit 1
  }
done

echo "Verified main CI and all product E2E groups for $commit in run $run_id" >&2
printf '%s\n' "$run_id"
