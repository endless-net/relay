# Cross-network peer routing work

Status: draft implementation; end-to-end cross-network routing is not supported yet.

The protobuf source now carries `peer_network_id` in control and upstream peer
authorization requests, and `destination_network_id` in mesh frames. The existing
mesh `network_id` identifies the source. Control authorization now requires the
destination network, forwards it to upstream, keys cached decisions by the full
pair and resolves the destination session in that network. Tests distinguish
same-named nodes in two networks and reject network/epoch substitutions.
Client frames now require `peer_network_id`; local delivery and mesh forwarding
resolve that destination network. Delivered frames preserve `from_network_id`
alongside the source node. Local TLS tests use identical node IDs in two networks,
check an explicit denial and verify remote destination epoch fencing. The mTLS
mesh test carries distinct source and destination networks across actual gRPC.
Consumer integration and full product E2E remain unverified; this draft is not
ready to activate.

Before this work can merge or release:

- Carry explicit source and destination network/node identities through client
  frames, local delivery, control calls and mesh delivery. Reject missing and
  noncanonical destination networks; do not infer them from source credentials.
- Include the full directed identity pair in authorization cache keys. Check
  source ownership/epoch and resolve the destination session in its own network.
- Require the upstream to authorize the exact cross-network pair. A normal
  same-network permission must not authorize another network or node.
- Preserve destination epoch and relay boot fencing across mesh delivery, and
  preserve source identity in delivered frames.
- Test local and cross-relay allow/deny, missing fields, network substitution,
  source and destination session replacement, credential expiry and explicit
  upstream revocation. Upstream service implementations and client consumers
  must adopt the producer contract before activation.

Relay transports encrypted packets; it cannot distinguish inner application
connection initiation from replies. Operators implementing limited peer sharing
must enforce inner traffic permissions at the authenticated endpoints while
allowing the transport handshake required by the authorized pair.

Protocol and module version identifiers have not been increased. Any release
version change needs separate explicit authorization. Until the runtime and
consumer work is verified, this draft must not be published as a usable module.
