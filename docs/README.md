# Relay documentation

This catalog describes the product through its code, tests, data model and
repository workflows. Architecture and runtime validation are recorded separately.

- [Product architecture](architecture.md): purpose, components, flows, contracts,
  security, resilience and operations.
- [Architecture decisions](decisions.md): accepted decisions and their consequences.
- [Possible future directions](future.md): options, priorities and the conditions
  that would justify additional complexity.
- [Upstream contract](upstream-contract.md): authorization, trust and identity
  requirements for compatible integrations.
- [Protobuf contracts](protobuf-contracts.md): published control and mesh contracts.
- [Product E2E](product-e2e.md): autonomous validation, coverage and recorded evidence.
- [Systemd deployment](systemd-deployment.md): operator-managed installation and rollback.

The documentation distinguishes three kinds of statements:

- **Implemented**: behavior follows directly from the current code or is tested.
- **Decided**: a design constraint that requires an explicit architectural decision
  to change.
- **Possible**: an option for discussion, not an approved roadmap.

Upstream domain logic and client endpoint selection are outside this repository.
They are described here only where they interact with Relay's published contracts.
