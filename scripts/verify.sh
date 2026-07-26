#!/usr/bin/env sh
set -eu

test -z "$(gofmt -l .)"
go vet ./...
go test -race ./...
go build ./cmd/endlessnet-relay ./cmd/endlessnet-relay-coordinator ./cmd/endlessnet-relay-smoke
