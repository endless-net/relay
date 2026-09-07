# Accepted architecture decisions

Status: accepted decisions. Original documentation snapshot: July 19, 2026;
updated September 7, 2026. Runtime evidence is published separately.

Original discussions are not available for every decision. The rationale below
is derived from code, tests, repository history and operational constraints.
Observed behavior is distinguished from inferred motivation.

## ADR-001. Own the Relay repository and release lifecycle

Status: accepted.

**Context.** Relay has its own security profile, public listeners, multi-host
deployment requirements and release cadence.

**Decision.** The dataplane, Relay Coordinator, mesh, schemas, self-hosting
documentation and release artifacts belong to this repository. Relay is a
standalone public product integrated through published network contracts.

**Consequences.** Operators can validate and roll back releases and deployments
independently. Contracts with a compatible upstream are versioned network
contracts and require coordinated integration changes.

## ADR-002. Separate the dataplane, relay control plane and upstream

Status: accepted.

**Context.** A public Relay needs a fast, minimal data path. ACLs, revocation and
domain topology must not be duplicated in every edge process.

**Decision.** Relay forwards payloads without owning the domain model. Relay
Coordinator manages short leases and routing. A compatible upstream is the
authority for networks, nodes, ACLs, revocation and signing trust.

**Consequences.** Responsibilities are explicit and payloads stay outside the
control plane. The cost is two network calls in the uncached control path and a
dependency on authorization availability for frame delivery.

## ADR-003. Use active-active Relay instances and a one-hop full mesh

Status: accepted for the current scale.

**Context.** Nodes in one network can connect to different regions and must
communicate without a central dataplane bottleneck.

**Decision.** Each live Relay receives the peer registry from Relay Coordinator
and maintains a gRPC stream to each peer. Remote frames traverse one mesh hop.

**Consequences.** Routing is simple, latency is predictable and one Relay failure
does not stop the others. Connection counts grow quadratically; region and
priority currently do not reduce the mesh topology.

## ADR-004. Enforce one session owner with leases, boot IDs and epochs

Status: accepted.

**Context.** After a partition, restart or node migration, an old Relay can still
believe that a session is active.

**Decision.** A Relay instance is identified by `(relay_id, boot_id)` and holds a
short lease. A session is identified by `(network_id, node_id)` and receives an
increasing `epoch` on every acquisition. Renewal, release and mesh delivery
check the exact owner. Release retains an inactive row with the last epoch;
renewal cannot revive expired or released sessions. Epoch overflow fails instead
of reusing a value.

**Consequences.** Stale state is fenced without consensus between Relay instances.
The system depends on PostgreSQL availability and correct TTLs. After migration,
clients can observe a short error window while routing updates and the old
session closes.

## ADR-005. Protect production channels with TLS 1.3 and mTLS workload identities

Status: accepted.

**Context.** Relay is public; control and mesh commands can change routing and
forward user traffic.

**Decision.** The public listener uses TLS 1.3. Control and mesh use SPIFFE mTLS
through the local SPIRE Workload API and exact URI SAN identities. A Relay SVID
must contain exactly one identity, `spiffe://<trust-domain>/relay/{relay_id}`.
The operator configures the trust domain and service identities. Production
plaintext listeners are rejected by the code; shared tokens are not an
authentication mechanism.

**Consequences.** A peer cannot impersonate a `relay_id` without its matching
SVID. Operations depend on the local SPIRE Agent, Workload API selectors and
trust bundle availability. SVID rotation is dynamic.

## ADR-006. Authenticate nodes with short-lived Ed25519 credentials

Status: accepted.

**Context.** Public Relay listeners must verify node identity without access to
the upstream's signing secret and support key changes without stopping the cluster.

**Decision.** An Ed25519-signed credential binds the network, node, key ID and
expiry. Relay receives a versioned trust bundle with multiple keys and trust windows.

**Consequences.** Signature verification is local and fast; the private signing
key is never distributed to Relay. Revocation before credential expiry still
requires control-plane checks and session renewal.

## ADR-007. Check ACLs for every frame and fail closed with bounded stale caching

Status: accepted.

**Context.** ACLs can change during a long-lived session, so a connection-time
check is insufficient. An upstream request for every frame adds load and turns
a brief upstream failure into immediate dataplane unavailability.

**Decision.** Relay calls `AuthorizePeer` for every frame. Relay Coordinator checks
the current source lease, ACL and destination lease. Positive decisions are fresh
for 5 seconds and can be used for up to 30 seconds only on upstream errors.
Negative decisions are fresh for 1 second and cannot provide stale authorization.

**Consequences.** ACL changes propagate within a bounded window; the system fails
closed after the stale window. The cost is a synchronous control RPC per frame
and an explicit window of up to 30 seconds during which recently authorized
traffic can continue during an upstream outage.

## ADR-008. Version wire contracts and parse messages strictly

Status: accepted and implemented.

**Context.** Silently accepting unknown fields or versions between independently
released components can change security semantics.

**Decision.** The public protocol requires `protocol_version = 1`, uses fixed
message types and rejects unknown JSON fields and trailing values. The gRPC
contract is in package `endlessnet.relay.v1`; mesh messages require an envelope
version and a `oneof`. Unknown protobuf fields are rejected recursively at every
RPC boundary.

**Consequences.** Incompatible changes fail explicitly instead of silently
degrading behavior. Extending v1 requires care: even an additional field is
incompatible with strict decoders.

## ADR-009. Forward opaque frames with a hard size limit and best-effort delivery

Status: accepted.

**Context.** Relay must not interpret application protocols or retain unbounded data.

**Decision.** Payloads are opaque bytes up to 64 KiB. Sessions and queues are kept
in memory. There is no durable storage, retry or end-to-end acknowledgment.

**Consequences.** The data path stays simple, payloads stay out of the database
and logs, and failures do not replay stale data. Applications choose their own
retransmission, deduplication and acknowledgment semantics when needed.

## ADR-010. Bound resources and isolate slow or aggressive clients

Status: accepted.

**Context.** The public listener is exposed to connection exhaustion, slow
authentication and receiver backpressure.

**Decision.** Enforce global and per-source connection limits, a concurrent
authentication limit, authentication timeout, maximum frame size, bounded
per-session and per-peer queues, and an optional bandwidth limit. Disconnect
slow consumers.

**Consequences.** Memory and goroutine counts have predictable bounds. One client
cannot block another client's writer. Under overload, explicit rejection takes
priority over uncontrolled resource growth; load tests inform limit settings.

## ADR-011. Keep coordination state in a dedicated PostgreSQL database

Status: accepted.

**Context.** Fencing needs atomic epoch increments and shared state across Relay
Coordinator instances. Upstream domain tables are outside the Relay release lifecycle.

**Decision.** Relay Coordinator uses a dedicated PostgreSQL database. Session
acquisition uses a transaction with an instance row lock and an atomic conflict
update to increment the epoch. Migrations are embedded in the binary and applied
at startup. The schema avoids `DEFAULT`, explicit `NOT NULL` and PostgreSQL
foreign keys.

**Consequences.** State survives Coordinator restarts and can support high
availability. PostgreSQL is on the critical control-plane path; startup migration
coordination and expired-row cleanup remain minimal.

## ADR-012. Publish endpoints as a monotonically versioned snapshot

Status: accepted.

**Context.** Clients need a consistent set of public Relay endpoints that can be
updated independently of the live mesh registry.

**Decision.** Relay Coordinator loads a strict JSON snapshot, rejects version
rollback and atomically replaces the endpoint set in PostgreSQL.

**Consequences.** Publication represents a complete, reproducible state. Updates
currently require a Coordinator restart. Different contents under the same
version are rejected, so topology changes require a higher snapshot version.

## ADR-013. Publish immutable artifacts and support verifiable operator rollback

Status: accepted.

**Context.** Relay deployments span hosts. Partial rollouts and unverifiable
builds increase the risk of network degradation.

**Decision.** Releases are built from semver tags with merged-PR provenance checks.
Checksums, multi-architecture artifacts, containers, SBOMs and provenance are
published after product CI/E2E. Operators own deployment and rollback. The
systemd guide describes release directories, atomic symlinks and sequential
rollout with restoration of the previous version on failure.

**Consequences.** Release workflows publish artifacts and do not initiate
production rollout. Product E2E creates ephemeral SPIRE infrastructure without
production access. Deployment and rollback procedures belong to the operator.

## ADR-014. Keep production hosts simple and minimally privileged

Status: accepted.

**Context.** Relay does not require an orchestrator for its current topology,
but operates at a public boundary.

**Decision.** The supplied systemd units run components as a dedicated user with
filesystem sandboxing and no new privileges. Configuration templates are
versioned with the release; operators own actual runtime configuration.
Workload identities come from the SPIRE Workload API, and the public private
key is supplied through a systemd credential.

**Consequences.** Operations are inspectable and artifacts contain no credentials.
Scaling, certificate rotation and multi-host coordination remain responsibilities
of the operator's deployment automation.

## Changing these decisions

An incompatible implementation change requires a separate ADR or an update to
this log before implementation starts. At minimum, describe:

1. The measurable problem with the current decision.
2. Security and compatibility requirements.
3. Alternatives considered.
4. Data migration and staged rollout.
5. Rollback procedures.
6. Tests, metrics and completion criteria.
