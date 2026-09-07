# Possible future directions for Relay

Status: options for discussion, not an approved roadmap.

None of these items is a promise or a selected design. Before implementation,
each needs an owner, a measurable goal, an explicit decision and a compatible
rollout plan. Do not add complexity to the dataplane without an observed problem.

## 1. Strengthen contracts and operations

### F-02. Define SLOs and control/mesh observability

**Problem.** Relay has basic counters but lacks dedicated metrics for Relay
Coordinator, mesh peer state, authorization latency, remaining lease time and
trust bundle age.

**Possible scope.** RPC latency and error codes, PostgreSQL latency, cache hits
and stale use, active/fenced instances, session renewal failures, mesh readiness,
reconnects, queue depth and credential key age. Add dashboards and alerts without
node IDs or payloads. End-to-end correlation IDs require cardinality and privacy
analysis first.

**Entry criterion.** Define SLIs for connection success, p95/p99 delivery latency,
ACL/control failure rates, mesh recovery time and drop rates.

### F-03. Refine readiness, draining and post-deployment checks

Relay `/readyz` checks the listener, instance lease and usable trust; shutdown is
bounded to 5 seconds. Remaining work concerns observability and the operator's
operational acceptance.

**Problem.** Relay `/healthz` does not represent control leases or mesh state.
Relay Coordinator `/readyz` only checks endpoint snapshot reads. Checking systemd
state alone does not validate the full mTLS control flow.

**Options.** Preserve separate liveness and readiness probes. Expose remaining
instance lease time; assess PostgreSQL, upstream authorization and trust bundle
availability with appropriate criticality; validate bounded draining and run
production smoke checks after each rollout stage.

**Constraint.** Readiness must not turn a brief failure of an optional dependency
into a cascading outage.

### F-04. Make migrations and housekeeping safe across replicas

**Problem.** Each process applies migrations at startup without a ledger or an
explicit distributed lock. Expired instances and sessions remain in the database.

**Options.** A monotonic migration ledger, advisory lock, separate migration job
and periodic batch cleanup with age/size metrics. Preserve the project constraints:
no `DEFAULT`, explicit `NOT NULL` or PostgreSQL foreign keys. Cleanup must retain
the last epoch of each network/node pair. Released and expired sessions carry
fencing history; new acquisitions must continue that counter.

### F-05. Formalize key and certificate lifecycles

**Problem.** The code supports overlapping signing keys, but safe rotation depends
on the order in which trust bundles, credentials and workload certificates are issued.

**Possible scope.** Rotation runbooks with overlap windows, expiry alerts,
automated URI SAN checks, staged CA bundle rollout, emergency revocation and
clock-skew tests. Private keys remain outside release artifacts.

## 2. Production resilience

### F-06. Run multiple Relay Coordinator replicas

Relay Coordinator runs separately from Relay, and lease state lives in PostgreSQL.
Further replication requires safe concurrent migrations, idempotent startup
operations, gRPC/HTTPS load balancing and chaos tests.

Decide in advance:

- Whether PostgreSQL is the sole fencing authority.
- How Relay switches between Coordinator addresses.
- The acceptable outage budget for per-frame authorization.
- How to prevent concurrent publication of conflicting endpoint snapshots.

Adding a second replica does not remove the dependency on a single PostgreSQL service.

### F-07. Validate active-active topology in operator deployments

Product E2E checks three Relay instances. Operator acceptance should cover client
endpoint selection, session migration, host failure and return with a new
`boot_id`, followed by regional expansion.

Active-active operations still need SLOs, a capacity model and automated failure
exercises that verify fencing. Each public Relay DNS name needs its own certificate.

## 3. Scale the control path

### F-08. Reduce per-frame authorization cost without weakening revocation

**Signal.** p99 control latency affects delivery latency, or `AuthorizePeer` QPS
limits the cluster.

**Options to compare.** A bounded local route cache, signed policy snapshots,
push invalidation by `network_revision`, batch RPCs or a capability token for a
node pair. Every option needs a bounded lifetime, fail-closed behavior after
expiry and an ACL revocation test.

Simply extending the stale TTL directly increases the post-revocation window.

### F-09. Reconsider full mesh only after measuring a limit

**Signal.** O(N²) streams, reconnect storms or peer-update fan-out become a
measurable problem as Relay counts grow.

**Options.** Regional gateways, sparse topology with route discovery, hierarchical
mesh or a separate transport backbone. Compare hop counts, failure impact,
cross-region traffic cost and fencing complexity.

For a small number of Relay instances, the current one-hop full mesh remains
preferable for its simplicity.

### F-10. Define feedback and flow control for remote delivery

**Problem.** Fire-and-forget mesh does not report remote frame-delivery outcomes
to the source Relay.

**Options.** Message IDs and bounded acknowledgments, explicit `fenced`, `no_peer`
and `slow_consumer` codes, credit-based mesh flow control, or explicitly retaining
fire-and-forget semantics.

Choose the product guarantee before changing the protocol. A Relay acknowledgment
only confirms receipt into memory and must not be presented as end-to-end delivery.
Add a durable broker only for a separate, demonstrated product requirement.

### F-11. Distribute endpoint snapshots dynamically

**Signal.** Restart-based endpoint changes create unacceptable operational delays.

**Options.** An upstream watch API, a signed snapshot in object storage or a
separate admin RPC. Require monotonic versions, content hashes, atomic replacement,
a rollback policy and rejection of conflicting snapshots with the same version.

## 4. Data protocol evolution

### F-12. Consider relay-v2 only when measurements justify it

JSON-lines is convenient for diagnosis, but JSON and base64 increase frame size.
A future version 2 could consider length-prefixed binary framing, protobuf or
QUIC if CPU, bandwidth or head-of-line blocking becomes a measured constraint.
This is a design option, not authorization to change a protocol version.

Minimum requirements for a proposed v2:

- An explicit transition plan for v1 and v2 during rollout.
- Explicit version negotiation without downgrade.
- Equivalent or stricter limits and unknown-field policy.
- Parser fuzzing and cross-version conformance tests.
- No payload logging.
- A v1 retirement plan based on observed client usage rather than a calendar date.

### F-13. Improve fairness and abuse protection

As public load grows, measure the need for token buckets instead of fixed
one-second windows, per-network quotas, dynamic per-source limits and edge DDoS
protection. High-cardinality identifiers must not exhaust memory before authentication.

## 5. Engineering confidence

Useful extensions to the test suite include:

- Load tests for admission, the authorization cache, per-frame RPCs and mesh queues.
- Fuzz tests for the public decoder, credentials, trust bundles and mesh validation.
- Chaos cases for PostgreSQL failover, prolonged Coordinator outages, clock skew,
  packet loss, asymmetric partitions, certificate expiry and key rotation.
- An upgrade/downgrade matrix for adjacent Relay and Relay Coordinator releases.
- Verification of arm64 artifacts and actual systemd rollback.
- Longer soak tests with reconnects and goroutine/memory growth checks.

## 6. Signals for choosing the next step

| Observed signal | Consider first |
| --- | --- |
| An unexplained drop or fencing incident | F-02, then F-03 |
| A rollout failure or incompatible client | Contract conformance and the upgrade matrix |
| Coordinator outages exceed the SLO | F-04 and F-06 |
| Another production Relay or region is needed | F-02, F-03 and F-07 |
| p99 delivery latency correlates with control RPCs | F-08 |
| Mesh streams or reconnects reach a limit | F-09 |
| Users need the reason for remote drops | F-10 |
| Endpoints change more often than releases | F-11 |
| JSON/base64 measurably limits CPU or bandwidth | F-12 |

## 7. Invariants to preserve

Unless a separate ADR demonstrates otherwise, preserve:

- The upstream as the authority for ACLs, revocation and signing trust.
- TLS 1.3 on production listeners and mTLS identities on control/mesh.
- Strictly versioned contracts.
- Fencing of stale processes and sessions.
- Bounded memory, queues and wait times.
- No payloads in the database, metrics or ordinary logs.
- Immutable releases, verifiable provenance and rollback.
- No secrets or production credentials in the repository or artifacts.

## 8. Approving a future change

Before implementation:

1. Record the baseline and target SLO.
2. Describe the security and privacy impact.
3. Check wire, gRPC, schema and deployment compatibility.
4. Choose a canary and rollback strategy.
5. Add unit, race, integration and E2E tests, plus chaos/load tests where needed.
6. Define new metrics and alerts.
7. Record the ADR before changing production topology or contracts.
