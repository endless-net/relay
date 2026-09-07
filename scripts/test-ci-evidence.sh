#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
fixture=$(mktemp -d "$script_dir/../.ci-evidence.XXXXXX")
fixture=$(cd -- "$fixture" && pwd)
[[ "$fixture" == "$(dirname -- "$script_dir")"/.ci-evidence.* ]]
trap 'rm -rf -- "$fixture"' EXIT
export CI_EVIDENCE_FIXTURE=$fixture
export GITHUB_REPOSITORY=example/relay
export GH_BIN="$fixture/gh"
commit=0123456789abcdef0123456789abcdef01234567

cat > "$GH_BIN" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  *actions/workflows/ci.yml/runs*)
    if [[ ${CI_EVIDENCE_PENDING_ONCE:-false} == true && ! -f "$CI_EVIDENCE_FIXTURE/polled" ]]; then
      touch "$CI_EVIDENCE_FIXTURE/polled"
      jq '.workflow_runs[0].status = "in_progress"' "$CI_EVIDENCE_FIXTURE/runs.json"
    else
      cat "$CI_EVIDENCE_FIXTURE/runs.json"
    fi ;;
  *'actions/runs/42/jobs?filter=latest&per_page=100'*) cat "$CI_EVIDENCE_FIXTURE/jobs.json" ;;
  *) echo 'Unexpected GitHub API request' >&2; exit 1 ;;
esac
MOCK
chmod +x "$GH_BIN"

reset_fixture() {
  jq -n --arg sha "$commit" '{workflow_runs: [{id: 42, run_number: 3,
    head_sha: $sha, head_branch: "main", event: "push", path: ".github/workflows/ci.yml",
    status: "completed", conclusion: "success"}]}' > "$fixture/runs.json"
  jq -n '[{jobs: (["verify", "product-e2e / images", "product-e2e / storage"] +
    (["protocol-auth", "spiffe-mesh", "custom-domain", "trust-bootstrap", "snapshot",
      "fencing-recovery", "resources-lifecycle"] | map("product-e2e / scenarios (" + . + ", tests, domain)")) |
    map({name: ., status: "completed", conclusion: "success"}))}]' > "$fixture/jobs.json"
}

reject() {
  if bash "$script_dir/verify-ci-evidence.sh" "$commit" 0 > "$fixture/result" 2> "$fixture/error"; then
    echo "Unexpected acceptance: $1" >&2
    exit 1
  fi
}

reset_fixture
test "$(bash "$script_dir/verify-ci-evidence.sh" "$commit")" = 42

for mutation in \
  '.workflow_runs = []' \
  '.workflow_runs[0].head_sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"' \
  '.workflow_runs[0].head_branch = "feature/change"' \
  '.workflow_runs[0].event = "pull_request"' \
  '.workflow_runs[0].event = "workflow_dispatch"' \
  '.workflow_runs[0].path = ".github/workflows/other.yml"' \
  '.workflow_runs[0].status = "in_progress"' \
  '.workflow_runs[0].conclusion = "failure"' \
  '.workflow_runs += [(.workflow_runs[0] | .id = 43 | .run_number = 4 | .conclusion = "failure")]'; do
  reset_fixture
  jq "$mutation" "$fixture/runs.json" > "$fixture/changed.json"
  mv "$fixture/changed.json" "$fixture/runs.json"
  reject "$mutation"
done

# Every required job must be present exactly once and successful, including
# groups returned on later API pages. A green aggregate is not sufficient.
for index in $(seq 0 9); do
  for mutation in "del(.[0].jobs[$index])" \
    ".[0].jobs[$index].conclusion = \"failure\"" \
    ".[0].jobs[$index].conclusion = \"skipped\"" \
    ".[0].jobs += [.[0].jobs[$index]]"; do
    reset_fixture
    jq "$mutation" "$fixture/jobs.json" > "$fixture/changed.json"
    mv "$fixture/changed.json" "$fixture/jobs.json"
    reject "$mutation"
  done
done

reset_fixture
jq '[{jobs: .[0].jobs[0:5]}, {jobs: .[0].jobs[5:]}]' "$fixture/jobs.json" > "$fixture/changed.json"
mv "$fixture/changed.json" "$fixture/jobs.json"
test "$(bash "$script_dir/verify-ci-evidence.sh" "$commit")" = 42

reset_fixture
test "$(CI_EVIDENCE_PENDING_ONCE=true bash "$script_dir/verify-ci-evidence.sh" "$commit" 1)" = 42

if bash "$script_dir/verify-ci-evidence.sh" invalid 0 > /dev/null 2>&1; then
  echo 'Accepted an invalid SHA' >&2
  exit 1
fi
echo 'CI evidence acceptance and rejection tests passed'
