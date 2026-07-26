#!/usr/bin/env bash
set -euo pipefail

umask 0077

: "${RUNNER_TEMP:?RUNNER_TEMP is required}"
: "${GITHUB_PATH:?GITHUB_PATH is required}"

case "$RUNNER_TEMP" in
  /*) ;;
  *) echo "RUNNER_TEMP must be absolute" >&2; exit 2 ;;
esac

readonly ansible_version="2.19.3"
readonly runtime="$RUNNER_TEMP/endlessnet-relay-ansible-$ansible_version"
case "$runtime" in
  "$RUNNER_TEMP"/endlessnet-relay-ansible-*) ;;
  *) echo "Ansible runtime escaped RUNNER_TEMP" >&2; exit 2 ;;
esac

system_version=""
if command -v ansible-playbook >/dev/null 2>&1; then
  system_version="$(ansible-playbook --version | awk 'NR == 1 { gsub(/]/, "", $3); print $3 }')"
fi
case "$system_version" in
  2.1[6-9].*|2.20.*)
    ansible-playbook --version
    exit 0
    ;;
esac

if [ -e "$runtime" ] || [ -L "$runtime" ]; then
  echo "Ansible runtime path already exists" >&2
  exit 2
fi

cleanup_on_failure() {
  local status="$?"
  trap - EXIT
  if [ "$status" -ne 0 ] && [ -d "$runtime" ] && [ ! -L "$runtime" ]; then
    rm -rf -- "$runtime"
  fi
  exit "$status"
}
trap cleanup_on_failure EXIT

python3 -m venv "$runtime"
"$runtime/bin/python" -m pip install \
  --disable-pip-version-check \
  --no-cache-dir \
  "ansible-core==$ansible_version"

installed_version="$("$runtime/bin/ansible-playbook" --version | awk 'NR == 1 { gsub(/]/, "", $3); print $3 }')"
[ "$installed_version" = "$ansible_version" ] || {
  echo "Unexpected Ansible Core version: $installed_version" >&2
  exit 1
}

printf '%s\n' "$runtime/bin" >>"$GITHUB_PATH"
trap - EXIT
