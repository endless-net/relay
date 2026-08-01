#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: $0 (--version v1.X.Y | --revision COMMIT_SHA) [--workflow-ref refs/heads/main]" >&2
}

version=
revision=
workflow_ref=
while [[ $# -gt 0 ]]; do
  case "$1" in
    --version) version=${2:-}; shift 2 ;;
    --revision) revision=${2:-}; shift 2 ;;
    --workflow-ref) workflow_ref=${2:-}; shift 2 ;;
    *) usage; exit 2 ;;
  esac
done

if [[ -n "$version" && -n "$revision" ]] || [[ -z "$version" && -z "$revision" ]]; then
  usage
  exit 2
fi
if [[ -n "$version" && ! "$version" =~ ^v1\.[0-9]+\.[0-9]+$ ]]; then
  echo "version must be an exact v1-prefixed semantic version" >&2
  exit 2
fi
if [[ -n "$revision" && ! "$revision" =~ ^[0-9a-f]{40}$ ]]; then
  echo "revision must be a full lowercase commit SHA" >&2
  exit 2
fi
if [[ -n "$workflow_ref" && "$workflow_ref" != refs/heads/main ]]; then
  echo "manual production deployment must run from refs/heads/main" >&2
  exit 1
fi

repository=${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}
gh_bin=${GH_BIN:-gh}

if [[ -n "$version" ]]; then
  git fetch --no-tags origin '+refs/heads/main:refs/remotes/origin/main'
  git fetch --force origin "refs/tags/${version}:refs/tags/${version}"
  release_commit=$(git rev-parse "${version}^{commit}")
  source_label=$version
else
  git show-ref --verify --quiet refs/remotes/origin/main || {
    echo "origin/main is missing from the full checkout" >&2
    exit 1
  }
  release_commit=$(git rev-parse "${revision}^{commit}")
  [[ "$release_commit" == "$revision" ]] || {
    echo "revision does not resolve to the requested commit" >&2
    exit 1
  }
  source_label=$revision
fi
if ! git merge-base --is-ancestor "$release_commit" refs/remotes/origin/main; then
  echo "$source_label does not reference a commit in origin/main" >&2
  exit 1
fi

merged_pr_count=$(
  "$gh_bin" api \
    -H 'Accept: application/vnd.github+json' \
    -H 'X-GitHub-Api-Version: 2022-11-28' \
    "repos/${repository}/commits/${release_commit}/pulls" \
    --jq '[.[] | select(.merged_at != null and .base.ref == "main")] | length'
)
[[ "$merged_pr_count" =~ ^[0-9]+$ ]] || {
  echo "GitHub returned an invalid merged PR count" >&2
  exit 1
}
if (( merged_pr_count == 0 )); then
  root_commit=$(git rev-list --max-parents=0 refs/remotes/origin/main)
  commit_count=$(git rev-list --count refs/remotes/origin/main)
  if [[ "$version" == v1.1.2 && "$commit_count" == 1 && "$release_commit" == "$root_commit" ]]; then
    echo "verified $version as the single-commit public baseline"
    exit 0
  fi
  echo "$source_label commit $release_commit was not delivered through a merged PR into main" >&2
  exit 1
fi

echo "verified $source_label commit $release_commit from a merged PR into main"
