# Autonomous product E2E

Relay owns this suite. It requires Docker Compose on a GitHub-hosted
`ubuntu-latest` runner and builds production binaries from one source SHA.
No sources or CI from Coordinator, Signing, Client or System Tests are used.
An unavailable runner capability or a broken harness fails setup.

The Compose project includes three Relay instances, Relay Coordinator,
PostgreSQL, SPIRE Server and five agents, an independently controlled upstream,
and a transparent mesh fault proxy. Public protocol clients use TLS over the
network. Production workloads use the real SPIRE Workload API; each run issues
its own CA, keys, SVIDs and credentials. Only harness ports bind to loopback;
control and mesh remain inside the isolated project network. Upstream controls
require the harness's exact mTLS identity.

## Executable coverage

| Group | Assertions | Owning tests |
| --- | --- | --- |
| protocol/auth | TLS 1.3, plaintext/TLS 1.2 rejection, unsupported version/field, malformed and oversized input, maximum payload, local delivery and isolation | `TestProductProtocol` |
| protocol/auth | Signature, expired credential, unknown key, inactive node, different network, denied directed pair; fresh/negative/stale cache, timeout and recovery | `TestProductProtocol`, `TestProductAuthorizationControl` |
| protocol/auth | Signing-key overlap, new credential, retirement, recovery | `TestProductTrustRotation` |
| trust-bootstrap | Invalid/unavailable trust prevents startup; recovery after trust returns | `TestProductNoTrustBootstrap` |
| SPIFFE/mesh | All six directed relay pairs, bidirectional payload integrity, migration, peer failure and reconnect, short Coordinator restart | `TestMultiRelayInfrastructure` |
| SPIFFE/mesh | Real Workload API, custom-verifier identity with empty VerifiedChains, SVID renewal, temporary SPIRE outage | `TestProductSPIFFE`, `TestProductSPIREOutage` |
| SPIFFE/mesh | Wrong identity/domain, stale boot, delayed frame from old epoch, idle stream closure after boot changes | `TestProductMeshFencing` |
| custom-domain | The same SPIFFE/mesh scenarios with `operator.example` | separate matrix run |
| fencing/recovery | Concurrent acquire over gRPC, release/reacquire, delayed release, expired renewal, epoch persistence after Coordinator restart | `TestProductFencing/postgres_release_reacquire` |
| fencing/recovery | PostgreSQL outage/recovery, stalled Coordinator heartbeat, process fencing and recovery after prolonged outage | `TestProductFencing` |
| resources/lifecycle | Global/source/auth limits, stalled handshake deadline, bandwidth rejection and new-session recovery | `TestProductResources` |
| resources/lifecycle | Local/remote stalled readers, destination closure counter, bounded mesh overflow, partition/reset/reconnect, unaffected peer delivery | `TestProductResources` |
| resources/lifecycle | SIGTERM, exit without Docker forced kill, bounded shutdown and reconnect | `TestProductResources/bounded_sigterm` |
| snapshot | Exact caller, changed contents without version change, rollback, malformed snapshot, empty snapshot and persistence across restart | `TestProductSnapshotIdentity`, `TestProductSnapshotPersistence` |
| storage | Memory/PostgreSQL semantics, concurrent epochs, expired instance/old boot, overflow, persistent epoch and snapshot invariants | [storage regressions](../internal/store/fencing_test.go), [real PostgreSQL tests](../internal/store/postgres_integration_test.go) |
| extended | 30 minutes of reconnects, boot changes and Coordinator outages, waiting for both directed routes | `TestProductExtended` |

Fast regression tests remain next to the runtime code. Cache deadlines are
checked with an injected clock after upstream completion; contradictory identity
configuration is rejected at startup. Independent request fixtures live in
[authz/testdata](../internal/authz/testdata/upstream-requests.json), outside the
E2E upstream implementation.

## Running and evidence

```sh
go test -race ./...
go test -tags=e2e -count=1 -timeout=15m -run TestProductProtocol ./e2e/...
E2E_EXTENDED=1 go test -tags=e2e,extended -count=1 -timeout=40m -run TestProductExtended ./e2e/...
```

CI builds the images once and passes their archive, checksum, source SHA and
image IDs to all matrix groups. Dependencies are pinned by digest or commit.
The required `verify` check succeeds only after static/unit checks and every
product group succeed; branch protection remains enabled. The release workflow
also waits for product E2E. The soak is compiled only with the additional `extended` build tag; ordinary
E2E runs do not silently skip it. Each regular group has a 20-minute job deadline.
The manual extended job allows 45 minutes including setup for a 30-minute soak.

Every run publishes JSON test events with durations, source SHA and image IDs.
Diagnostics include container status, redacted logs and metrics; fixture PKI,
credentials and payloads are never uploaded. Artifacts expire after 7 days.
Cleanup runs regardless of test outcome. No automatic test retries conceal an
initial failure. Destructive snapshot and trust-bootstrap cases have their own
Compose projects; network faults are commanded through a bounded-buffer proxy.

Acceptance links will be recorded after all groups and extended finish green.
Product evidence does not certify the actual EndlessNet Coordinator's upstream
compatibility (R1), Infrastructure integration or production rollout. Those
remain externally owned under architecture D-032.
