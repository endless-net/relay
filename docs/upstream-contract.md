# Supported upstream contract

Relay's authorization and trust contract is the protobuf schema
[`api/relay/v1/upstream.proto`](../api/relay/v1/upstream.proto). Buf builds, lints
and generates it in this repository. Nothing is published to Buf Schema Registry.
An operator supplies an implementation of `endlessnet.relay.v1.RelayUpstreamService`
and a credential issuer. Integrators own conformance and deployment acceptance.

## Transport and identity

The only upstream transport is gRPC over HTTP/2 with TLS 1.3 and mutual X.509-SVID
authentication. `--coordinator-url` remains an HTTPS origin such as
`https://upstream.example:9447`; it selects the gRPC authority, not a REST prefix.
Userinfo, query, fragment and path prefixes are rejected before storage or listeners
start. There is no HTTP/JSON adapter, protocol negotiation or fallback.

The client is the exact configured Relay Coordinator identity; the server is the
exact configured upstream identity. Another identity in the same trust domain is
rejected. Defaults are `spiffe://endlessnet.ru/service/relay-coordinator` and
`spiffe://endlessnet.ru/service/coordinator`. Both binaries accept `--trust-domain`,
`--coordinator-identity` and `--upstream-identity` (environment:
`ENDLESSNET_RELAY_TRUST_DOMAIN`, `ENDLESSNET_RELAY_COORDINATOR_IDENTITY`,
`ENDLESSNET_RELAY_UPSTREAM_IDENTITY`). Empty service settings derive identities
from the domain. Contradictory domains, overlapping service/relay identities and
identical service identities fail at startup.

The production client bounds each RPC to 10 seconds or the caller's earlier
deadline, and request/response messages to 1 MiB. Cancellation propagates to the
upstream. It recursively rejects unknown fields in successful responses.
Servers must enforce exact caller identity and reject unknown fields recursively,
using the published `RejectUnknownUnaryServerInterceptor` or equivalent validation.

## RPCs

| Method | Request | Successful response |
| --- | --- | --- |
| `AuthorizeCredential` | `AuthorizeCredentialRequest { Credential credential }` | Empty `AuthorizeCredentialResponse` with gRPC OK |
| `AuthorizePeerPair` | `AuthorizePeerPairRequest { Credential credential, string peer_id, string peer_network_id }` | Empty `AuthorizePeerPairResponse` with gRPC OK |
| `GetTrustBundle` | Empty `GetTrustBundleRequest` | `GetTrustBundleResponse { SigningTrustBundle relay_trust_bundle }` |

The full method prefix is `/endlessnet.relay.v1.RelayUpstreamService/`.
Unsupported service versions or methods fail with `Unimplemented`.

### Authorization

Only an OK RPC with the expected valid protobuf response authorizes.
`PermissionDenied` and `Unauthenticated` are explicit denials and cannot use a
previous stale allow. Other non-OK statuses, invalid responses and transport errors
are upstream failures subject only to the bounded stale window described below.
Servers return `InvalidArgument` for malformed requests, including missing
credentials, noncanonical identifiers and unknown fields. Do not encode denial
inside a successful response.

The upstream verifies signature, issuer/key validity, expiry, active source node
and network membership. Peer authorization additionally requires an active target
in `peer_network_id` and an explicitly allowed directed source/target pair.
The destination network is required even for same-network peers. A cross-network
pair requires an explicit upstream permission for those exact identities; ordinary
network membership cannot authorize a node in another network.
Authorization of A to B does not authorize B to A.

The imported `Credential` message contains `algorithm`, `key_id`, `network_id`,
`node_id`, `expires_at_unix_nano` and `signature`. Expiry is represented as Unix nanoseconds
in protobuf; conversion must preserve the instant used by the credential codec.
The algorithm remains `ed25519-relay-credential-v3`. Public client credentials and
their canonical signing bytes are unchanged. Use the published
[conversion helpers](../api/relay/v1/contract.go) and
[credential codec](../protocol/v1/protocol.go); no private signing key belongs in Relay.

### Trust

The trust response requires a `relay_trust_bundle`. It uses the existing
`SigningTrustBundle`: version 1, `active_key_id` and typed `SigningTrustKey` entries.
Each key contains `key_id`, algorithm `ed25519`, `public_key` and optional validity
windows represented by `not_before_unix_nano` / `not_after_unix_nano` (zero means
absent). Key IDs must match public keys. Duplicate keys, unsupported versions,
missing active keys, invalid validity windows and unknown fields are rejected.

Publish old and new keys together before issuing credentials with the new key.
Retirement removes the old key. Bootstrap without usable trust fails.

## Cache and delivery semantics

Positive decisions are fresh for 5 seconds. Only upstream failures permit a
previous positive result up to 30 seconds from its acquisition. Explicit denial
is cached for 1 second. Credential expiry and stale deadlines are checked again
after upstream completion. Trust uses the same fresh/stale windows. Delivery is
best effort, without a durable queue or delivery ACK.

## Integration cutover and scope

An integrator must implement this service before deploying Relay Coordinator
against it. The former JSON authorization and trust routes are not used or
supported. A JSON-only upstream cannot bootstrap the product. No existing
protocol, credential, schema or artifact version number is increased by this
transport cutover.

The public client `relay-v1` JSON-lines protocol and the separate read-only
endpoint snapshot HTTPS interface are unchanged. The harness-only control API is
not part of the product upstream contract.

## Conformance ownership

Independent [credential](../internal/authz/testdata/authorize-credential.textproto)
and [peer-pair](../internal/authz/testdata/authorize-peer-pair.textproto) textproto
fixtures specify requests without using production client serialization code.
Their signatures are deliberately unusable. [Wire conformance tests](../internal/authz/conformance_test.go)
exercise generated gRPC clients, statuses, deadlines, unknown fields and trust
validation. The independent [E2E upstream](../e2e/cmd/mock-coordinator/main.go)
implements the service behind real SPIFFE mTLS. E2E verifies exact identities,
typed calls, invalid requests, inactive targets and absence of the removed JSON routes.
Generated runtime credentials and PKI are never published.

Green product CI verifies Relay against this contract. Every production upstream
must run its own conformance tests; another repository's implementation is not
certified by Relay's test server.
