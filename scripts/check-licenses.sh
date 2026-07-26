#!/usr/bin/env bash
set -euo pipefail

module_dir="${1:-.}"
report_file="$(mktemp)"
trap 'rm -f "$report_file"' EXIT

(
  cd "$module_dir"
  go run github.com/google/go-licenses/v2@v2.0.1 report ./...
) >"$report_file"

failed=false
while IFS=, read -r package license_url license; do
  case "$license" in
    Apache-2.0|MIT|BSD-2-Clause|BSD-3-Clause)
      ;;
    *)
      printf 'disallowed or unknown license: package=%s license=%s url=%s\n' \
        "$package" "$license" "$license_url" >&2
      failed=true
      ;;
  esac
done <"$report_file"

if "$failed"; then
  exit 1
fi

sort -t, -k1,1 "$report_file"
