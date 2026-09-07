# EndlessNet Relay

EndlessNet Relay provides the TLS relay dataplane, Relay Coordinator, and
active-active relay mesh used when peers cannot establish a direct path.

Relay is a standalone public product. A compatible upstream owns networks,
nodes, directed peer authorization and signing trust; Relay accesses it only
through the [published upstream contract](docs/upstream-contract.md). EndlessNet
integrates and operates this product, but its sources, database and production
configuration are not required to build or test Relay.

## Documentation

- [Service architecture](docs/architecture.md)
- [Autonomous product E2E and coverage](docs/product-e2e.md)
- [Architecture decisions](docs/decisions.md)
- [Possible future directions](docs/future.md)

## Binaries

- `endlessnet-relay`: public TLS dataplane and relay-to-relay mesh.
- `endlessnet-relay-coordinator`: session leases, fencing, endpoint registry,
  and cached authorization.
- `endlessnet-relay-smoke`: protocol and deployment smoke checks.

Production listeners require TLS 1.3. Internal identities and rotating
X.509-SVIDs come from a local SPIRE Workload API. The Relay Coordinator uses a
peer-authenticated PostgreSQL database.

## Build and test

Go consumers use the canonical module path:

```sh
go get github.com/endless-net/relay@v1.1.4
```

```sh
go test -race ./...
go build ./cmd/...
```

The hermetic multi-relay suite requires Docker with Compose:

```sh
go test -tags=e2e -count=1 -timeout=15m ./e2e/...
```

It starts PostgreSQL, an independently controlled contract upstream, SPIRE Server/Agents, the Relay Coordinator,
and three Relay containers. Test credentials and PKI are ephemeral.

## Self-hosting

The complete Relay and Relay Coordinator implementations are published under
Apache-2.0. Operators can build the binaries or images and run an independent
Relay deployment.

Running the software does not grant access to the managed EndlessNet network.
Managed clients accept only short-lived relay credentials signed by a trusted
control-plane Ed25519 key. Expired credentials, unknown issuers, invalid
signatures, and contract-version mismatches are rejected locally.

Self-hosted operators must provide:

- a Relay Coordinator and PostgreSQL database;
- TLS certificates for public Relay listeners;
- SPIRE identities for internal mTLS;
- network connectivity between mesh endpoints;
- endpoint configuration, a compatible upstream and credential issuer.

## Immutable releases and integration

Relay publishes immutable release artifacts after product CI/E2E. It does not
initiate Infrastructure rollout and is not part of the EndlessNet server release
set. Operators select a pinned artifact and deploy it using their own automation.
EndlessNet Infrastructure owns its deployment and integration acceptance.

Release archives contain binaries, service units, renewal helpers, license
notices, and checksums. They never contain production environment files,
inventory, private keys, or endpoint topology.

For a dependency matrix and a manual systemd installation procedure for both
the Relay Coordinator and Relay nodes, see
[systemd deployment](docs/systemd-deployment.md). Local multi-architecture
archives can be built with:

```sh
bash scripts/build-release.sh v1.1.3 dist
```
