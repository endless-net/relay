#!/usr/bin/env bash
set -euo pipefail
version=2.14.0
archive="golangci-lint-${version}-linux-amd64.tar.gz"
scratch=$(mktemp -d)
trap 'rm -f "$scratch/$archive" "$scratch/checksums.txt"; rmdir "$scratch" 2>/dev/null || true' EXIT
destination="${RUNNER_TEMP:?}/public-ci-bin"
mkdir -p "$destination"
curl --fail --silent --show-error --location --retry 3 "https://github.com/golangci/golangci-lint/releases/download/v${version}/$archive" -o "$scratch/$archive"
curl --fail --silent --show-error --location --retry 3 "https://github.com/golangci/golangci-lint/releases/download/v${version}/golangci-lint-${version}-checksums.txt" -o "$scratch/checksums.txt"
(cd "$scratch" && grep "  $archive$" checksums.txt | sha256sum --check --strict -)
tar -xzf "$scratch/$archive" --strip-components=1 -C "$destination" "golangci-lint-${version}-linux-amd64/golangci-lint"
printf '%s\n' "$destination" >> "${GITHUB_PATH:?}"
go install golang.org/x/tools/cmd/goimports@v0.48.0
go install golang.org/x/vuln/cmd/govulncheck@v1.7.0
