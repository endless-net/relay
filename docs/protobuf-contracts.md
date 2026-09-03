# Protobuf API contracts

Relay's versioned gRPC contract is the source schema under
`api/relay/v1/relay.proto`. Buf owns compilation, linting, breaking-change
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
buf build api -o contracts/proto-baseline/relay.binpb
```

Until that release evidence exists, the compatibility workflow reports the
baseline check as skipped. It is advisory and does not block merge.
