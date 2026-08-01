#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: $0 VERSION [OUTPUT_DIR]" >&2
  echo "example: $0 v1.1.3 dist" >&2
}

version=${1:-}
output_dir=${2:-dist}

if [[ ! "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  usage
  exit 2
fi

repository_root=$(git rev-parse --show-toplevel)
cd "$repository_root"

mkdir -p "$output_dir"
output_dir=$(cd "$output_dir" && pwd)
work_dir=$(mktemp -d "${TMPDIR:-/tmp}/endlessnet-relay-release.XXXXXX")
trap 'rm -rf -- "$work_dir"' EXIT

commit=$(git rev-parse HEAD)
go_version=$(go version)
archives=()

for arch in amd64 arm64; do
  package_name="endlessnet-relay_${version}_linux_${arch}"
  package_dir="$work_dir/$package_name"
  mkdir -p "$package_dir/examples"

  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" \
    go build -trimpath -ldflags "-s -w" \
    -o "$package_dir/endlessnet-relay" ./cmd/endlessnet-relay
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" \
    go build -trimpath -ldflags "-s -w" \
    -o "$package_dir/endlessnet-relay-coordinator" ./cmd/endlessnet-relay-coordinator
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" \
    go build -trimpath -ldflags "-s -w" \
    -o "$package_dir/endlessnet-relay-smoke" ./cmd/endlessnet-relay-smoke

  cp \
    deploy/endlessnet-relay.service \
    deploy/endlessnet-relay-coordinator.service \
    deploy/endlessnet-relay-cert-renew.service \
    deploy/endlessnet-relay-cert-renew.timer \
    deploy/renew-relay-certificate.sh \
    deploy/reload-relay-certificate.sh \
    deploy/prepare-relay-certificate-renewal.sh \
    deploy/restore-relay-certificate-renewal.sh \
    "$package_dir/"
  cp \
    deploy/coordinator.env.example \
    deploy/relay.env.example \
    deploy/relay-instance.env.example \
    deploy/endpoints.example.json \
    "$package_dir/examples/"
  cp docs/systemd-deployment.md "$package_dir/SYSTEMD-DEPLOYMENT.ru.md"
  cp LICENSE NOTICE THIRD_PARTY_NOTICES "$package_dir/"

  {
    printf 'version=%s\n' "$version"
    printf 'commit=%s\n' "$commit"
    printf 'target=linux/%s\n' "$arch"
    printf 'builder=%s\n' "$go_version"
  } >"$package_dir/BUILD-INFO"

  archive_name="${package_name}.tar.gz"
  tar -C "$work_dir" -czf "$output_dir/$archive_name" "$package_name"
  archives+=("$archive_name")
done

(
  cd "$output_dir"
  sha256sum -- "${archives[@]}" >checksums.txt
)

printf 'release artifacts written to %s\n' "$output_dir"
