#!/usr/bin/env bash
set -euo pipefail
root=$(git rev-parse --show-toplevel)
cd -- "$root"
export GOWORK=off GOFLAGS=-mod=readonly
scratch=$(mktemp -d)
trap 'rm -rf -- "$scratch"' EXIT

# Build the same source archive used by an API release, without creating a tag.
go run ./scripts/ci build --tag api/v0.0.0 --out "$scratch/release"
mkdir -- "$scratch/contract" "$scratch/built"
git archive HEAD -- api | tar -xf - -C "$scratch/contract"
tar -xzf "$scratch/release/relay-api-api-v0.0.0-source-any.tar.gz" -C "$scratch/built"
diff -qr "$scratch/contract/api" "$scratch/built"

sdk_files() {
  find . -type f \( -name '*.pb.go' -o -name '*.connect.go' \) -printf '%P\n' | LC_ALL=C sort
}

cd -- "$scratch/built"
sdk_files > "$scratch/expected-files"
test -s "$scratch/expected-files"
cut -c 67- generated.sha256 | LC_ALL=C sort > "$scratch/manifest-files"
diff -u "$scratch/expected-files" "$scratch/manifest-files"
sha256sum --strict --quiet --check generated.sha256
while IFS= read -r file; do rm -- "$file"; done < "$scratch/expected-files"
buf generate
goimports -w .
sdk_files > "$scratch/actual-files"
diff -u "$scratch/expected-files" "$scratch/actual-files"
sha256sum --strict --quiet --check generated.sha256
go build ./...
printf 'API build matches contract at %s\n' "$(git -C "$root" rev-parse HEAD)"
