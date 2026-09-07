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

Acceptance recorded on 2026-09-07:

- [Regular CI: all seven groups, storage and protected verify](https://github.com/endless-net/relay/actions/runs/34119731480): PR head `9e7660fafece0ec448914be3f71ca697000f1c7a`, built/tested merge checkout `a594685d4d2b3a3800f0a8638ed98dda5ef45fbd`; 43 leaf E2E scenarios passed.
- [Extended and regular CI](https://github.com/endless-net/relay/actions/runs/34117312163): source `a7d9b733dc515bf262f8bd8f9ec9b34d72ed3a98`; extended passed in **1801.94 seconds**, completing **351 recovery cycles**. Subsequent changes only affect tests, fixtures, CI and documentation; production sources and build inputs are unchanged.

| Regular group | Leaf scenarios | Go package elapsed, seconds |
| --- | ---: | ---: |
| protocol/auth | 10 | 85.614 |
| SPIFFE/mesh | 9 | 57.130 |
| custom domain | 9 | 76.064 |
| fencing/recovery | 3 | 72.779 |
| resources/lifecycle | 8 | 55.080 |
| snapshot | 2 | 20.387 |
| trust bootstrap | 2 | 52.646 |

The following are Docker image configuration IDs, not published release tags.
The archive checksum binds the complete set of locally loaded images.

| Input | Regular CI SHA-256 | Extended CI SHA-256 |
| --- | --- | --- |
| Relay image ID | `63fad9d056fc940d874404826d023c5ebe2f0f5fbf2487ae55c258a3a99c5baf` | `ded14ed131b4f6a7283111941b9a20844532a90002f3b9cf60220c1ea6864b3f` |
| Relay Coordinator image ID | `e8f71fcb6444c005ae2db75b4e8255fbee894392589c63f2093d40debf01927c` | `5e724eb776559f2adb27a6b86e64bf3f18a467518fd3f451979b7109691a1429` |
| Upstream/proxy image ID | `ad4b2e784953db9cde51903f909215cb5efa04ab034e80c990d6f0728169ae73` | `57d9388132d5554643683109d492e043414b4d45766f4facb5177a6c73199f88` |
| Image archive | `dbb8a63faab4c40d004e76542daa87c36099adebb5c98e42c37db3f4d685501e` | `1a5cd635c062278dda469d95f34ec0e3e7d0ac14361917226e659fc6567ba8f2` |

Initial failures remain in Actions history, including
[setup/conformance](https://github.com/endless-net/relay/actions/runs/34114320117),
[directed recovery](https://github.com/endless-net/relay/actions/runs/34115225512)
and the [first soak](https://github.com/endless-net/relay/actions/runs/34116137250).
These were separate failed runs followed by corrective commits, not automatic retries.

Product evidence does not certify the actual EndlessNet Coordinator's upstream
compatibility (R1), Infrastructure integration or production rollout. Those
remain externally owned under architecture D-032.
