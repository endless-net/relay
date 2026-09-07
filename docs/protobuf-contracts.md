# Protobuf API contracts

Relay's versioned gRPC contract is the source schema under
`api/relay/v1/relay.proto` (control/mesh) and `api/relay/v1/upstream.proto`
(operator authorization/trust). Buf owns compilation, linting, breaking-change
checks, and Go/gRPC generation.

The Buf lint configuration keeps the existing package layout and bidirectional
mesh RPC naming as explicit, narrowly scoped exceptions. New relay schema
work must satisfy every other `STANDARD` rule.

Run locally:

```sh
buf lint
buf build
buf generate
```

The generated Go files under `api/relay/v1` are committed. A change to the
schema must regenerate them and leave no generated-file diff in CI.

After the currently released relay schema is revalidated as the first Buf
release, create `contracts/proto-baseline/relay.binpb` with:

```sh
buf build -o contracts/proto-baseline/relay.binpb
```

Until that release evidence exists, the compatibility workflow reports the
baseline check as skipped. It is advisory and does not block merge.

Schemas and generated Go/gRPC stubs are distributed through this Git repository;
there is no Buf Schema Registry publication. Pin a repository commit when consuming
the upstream contract. Required CI runs `buf lint`, `buf build` and regeneration
checks. Released-baseline comparison remains separate from these required checks.
