# EndlessNet Relay

EndlessNet Relay provides the TLS relay dataplane, Relay Coordinator, and
active-active relay mesh used when peers cannot establish a direct path.

The main EndlessNet control plane remains authoritative for networks, nodes,
ACLs, credential revocation, and signing trust. Relay processes communicate
with it only through the Relay Coordinator.

## Documentation

- [Service architecture](docs/architecture.md)
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
go test -tags=e2e -count=1 -timeout=5m ./e2e/...
```

It starts PostgreSQL, a strict main-Coordinator mock, the Relay Coordinator,
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
- a private WireGuard mesh between Relay nodes;
- endpoint configuration and a compatible credential issuer.

## Generic deployment workflow

`.github/workflows/deploy-production.yml` deploys an existing immutable release
from a standard GitHub-hosted runner. It contains no production inventory.

Configure the protected `production` environment with:

- `DEPLOY_INVENTORY_JSON`: Ansible inventory with `coordinator` and `relays`
  groups;
- `RELAY_COORDINATOR_ENV`, `RELAY_NODE_ENV`, and `RELAY_ENDPOINTS_JSON`:
  runtime configuration;
- `DEPLOY_SSH_PRIVATE_KEY` and `DEPLOY_KNOWN_HOSTS`;
- `DEPLOY_USER`: optional remote user.

The workflow validates its inputs without printing them, checks release
provenance and checksums, preflights every target, deploys the Coordinator, and
then rolls Relay nodes sequentially. Host rollback remains implemented by the
Ansible playbook. Runner lifecycle, SPIRE operations, managed inventory, and
infrastructure recovery remain outside this repository.

Release archives contain binaries, service units, renewal helpers, license
notices, and checksums. They never contain production environment files,
inventory, private keys, or endpoint topology.
