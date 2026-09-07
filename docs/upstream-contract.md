# Supported upstream contract

Relay is an independent product. A self-hosting operator supplies an upstream
implementing this contract and a credential issuer. No EndlessNet source or
production database is involved. Compatibility of the actual EndlessNet
Coordinator is an external integration task (architecture D-032, audit R1).

## Transport and identity

HTTPS TLS 1.3 with mutual X.509-SVID authentication. The client is the exact
configured Relay Coordinator identity; the server is the exact configured
upstream identity. A different identity in the same trust domain is rejected.
Defaults are `spiffe://endlessnet.ru/service/relay-coordinator` and
`spiffe://endlessnet.ru/service/coordinator`. Both binaries accept
`--trust-domain`, `--coordinator-identity`, `--upstream-identity` (environment:
`ENDLESSNET_RELAY_TRUST_DOMAIN`, `ENDLESSNET_RELAY_COORDINATOR_IDENTITY`,
`ENDLESSNET_RELAY_UPSTREAM_IDENTITY`). Empty service settings derive identities
from the configured domain. Contradictory domains, overlapping service/relay
identities and identical service identities fail at startup.

## Authorization

`POST /internal/coordinator/relay-control/v1/authorize`, JSON Content-Type:

- Credential: `{"action":"credential","credential":{...}}`.
- Directed pair: `{"action":"peer","credential":{...},"peer_id":"destination"}`.

Credential fields are exactly `algorithm`, `key_id`, `network_id`, `node_id`,
`expires_at` (RFC3339 timestamp), `signature`. See the published
[credential codec](../protocol/v1/protocol.go) for Ed25519 canonical signing bytes,
base64 encoding and key identifiers; the format remains unchanged.

Only **204 No Content** authorizes. **401/403** denies (never stale-allow a fresh
denial); every other status, malformed response or transport failure is an
upstream error. There is no fallback to a differently shaped API or 200 allow.
Upstream verifies signature, issuer/key validity, expiry, active source node and
network membership. Peer authorization additionally requires an active target
in that same network and an explicitly allowed directed source/target pair.
Unknown fields, actions and malformed requests must be rejected.

## Trust

`GET /internal/coordinator/relay-control/v1/trust-bundle` returns successful JSON
`{"relay_trust_bundle":{"version":1,"active_key_id":"...","keys":[...]}}`.
Each key contains `key_id`, `algorithm` (`ed25519`), `public_key`, optional
`not_before` and `not_after`. The key identifier must match its public key;
duplicates, unsupported versions, missing active key, invalid validity windows,
unknown fields and trailing JSON are rejected. See the same codec for validation.
During rotation, publish old and new keys before issuing credentials with the
new key; retirement removes the old key. Bootstrap without usable trust fails.

## Cache and delivery semantics

Positive decisions are fresh for 5 seconds. Only upstream errors permit a
previous positive result up to 30 seconds from its acquisition; explicit denial
is cached for 1 second. Credential expiry and stale deadlines are checked again
after upstream completion. Trust has the same fresh/stale windows. Delivery is
best effort: no durable queue or delivery ACK is added by this contract.

## Conformance ownership

[HTTP conformance tests](../internal/authz/authz_test.go) and the independently
controlled [E2E upstream](../e2e/cmd/mock-coordinator/main.go) verify the product
boundary. Fixtures and identities are generated per run and never published.
A production upstream must run its own conformance tests; green Relay CI does
not establish compatibility of another repository's implementation.
