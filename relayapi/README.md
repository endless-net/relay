# Relay API module

Module: `github.com/endless-net/relay/relayapi`. Initial release: `v0.0.0`,
repository tag: `relayapi/v0.0.0`. The module path has no `/v0` suffix.
Go module versions are independent from protocol versions and runtime releases.
Published tags must never be moved or republished with different content.

## Layout

- `v1`: current public TLS protocol, credential codec, trust and endpoint types.
- `v1/relayapigrpc`: generated gRPC control, mesh and upstream bindings and
  strict validation/conversion helpers. No runtime handlers or storage.
- `v1/extensions`: proposed contracts for future capabilities, with shape and
  bound validation; these are NOT active relay-v1 wire messages.
- `../proto/relay/v1`: canonical protobuf sources, generated using the root Buf
  configuration. Go consumers need only this module; schema consumers pin the
  same repository tag. Sources are not duplicated into the module.

```go
require github.com/endless-net/relay/relayapi v0.0.0
```

Import public types from `github.com/endless-net/relay/relayapi/v1` and RPC
bindings from `github.com/endless-net/relay/relayapi/v1/relayapigrpc`.
Old `protocol/v1` and `api/relay/v1` Go packages are removed, without wrappers.
Consumers must migrate imports and run their conformance tests.

## Independent validation and release

Run `go test -race ./...`, `go vet ./...` and `go mod verify` in this directory.
Contract CI also copies this directory alone into an isolated consumer environment
with `GOWORK=off`; no runtime, database or server image is required.
The separate API workflow validates `relayapi/v0.0.0` tag pushes and verifies
download through a fresh Go consumer with no local replacement. A Git tag makes
the Go module available independently of any server release. Only publish a tag
after its source is merged and API checks pass; version increases need approval.

The root runtime uses a local replace for coordinated development, like the
Coordinator repository. This does not prove that the published module is usable;
the tag consumer check provides that separate evidence. Runtime builds embed the
checkout API, so their commit and module directory identify the actual source.

## Future capabilities

The `extensions` package is an experimental design surface in the v0 SDK.
It does not modify the current wire version, credentials or trust formats.
The runtime must reject these objects on `relay-v1-tls`. No compatibility
negotiation, implicit feature enablement or fallback is introduced. A future
activation must define and approve its transport version, implement producers and
consumers together, and prove the effects in network tests.

| Capability | Contract and activation obligations |
| --- | --- |
| Drain/restart/rebalancing | `LifecycleNotice`: deadline, bounded retry budget, jitter and preferred endpoint IDs. Client intersects hints with its authenticated map; no arbitrary redirect or lease extension. |
| Presence | `PresenceNotice`: directional flow, subscription sequence, expiry. Authorize before revealing existence; reject old sequence or expired notice and clear subscription on context change. |
| Regions | `RegionPolicy`: home, allowed and fallback regions, endpoint IDs and cross-region mesh policy. Intersect with accepted snapshot; never bypass residency on failure. Topology and placement are operator concerns. |
| Restricted networks | `BootstrapEndpoint`: literal IPv4/IPv6 addresses, separate TLS name and explicit TLS/WSS transport. Validate TLS identity even without DNS. TCP 443 requires deployment and acceptance, not a new protocol constant. |
| Discovery | `DiscoveryEnvelope`: bounded encrypted payload, operation, exact directed flow, nonce and expiry. Peers must authenticate all bindings, enforce replay protection and candidate policy. Relay cannot authorize a new route from discovery. |
| Peer relay | `PeerRelayOffer` and `PeerRelayReservation`: expiring UDP candidate and authority-bound allocation. Define proof codec, issuer trust, revocation, quota, return traffic and datagram framing before activation. An offer alone never grants access. |
| Fairness | `TrafficBudget`: byte rate/burst, sessions and discovery rate. Enforce authenticated scope at the service; Billing entitlement remains upstream. No missing numeric field means unlimited. |
| Offline restart | `ResumePolicy`: effective expiry cannot exceed credential expiry. Credential persistence is disabled in current runtime; enabling it requires approved storage, trust, revocation and outage rules. |
| Diagnostics | `Diagnostic`: bounded path/stage/outcome and RTT, including captive portal. No payload or credential; peer-level reports require authorization and must not become high-cardinality metric labels. |

Validate methods check shape and bounds, not signatures, upstream permission,
replay history or current routing. Production implementations must enforce those
checks separately. Constants are design bounds, not an approved availability SLO.
Peer relay and WebSocket remain optional future transports, not promises of
support from the current server. A peer relay never decrypts WireGuard traffic.
