# Relay functional test coverage audit

Status: implementation audited; final CI verification pending. Scope: only this repository and the Relay product. This
audit does not certify an integrator's Coordinator or production deployment.
Experimental `relayapi/v1/extensions` types are validation contracts, not active
dataplane features. No wire or module version change is part of this work.

## Acceptance rules

- Every implemented capability has a positive test, relevant rejection/boundary
  cases, and recovery/concurrency coverage where its state can change.
- Full service tests use real Relay and Relay Coordinator binaries, PostgreSQL,
  mesh and isolated SPIRE. Only the external upstream is a stateful testserver.
- Fast tests isolate boundary cases with controlled clocks and inputs. They do
  not replace full product E2E or certify Workload API behavior.
- Root `go test ./...` excludes the nested API module and integration-tagged
  PostgreSQL tests. Both are verified explicitly by CI.
- Line coverage is diagnostic, not a claim of full functional coverage.
  Generated protobuf boilerplate, subprocess startup and PostgreSQL cannot be
  assessed from the ordinary local unit-test percentage alone.
- No new test may silently skip missing product-test prerequisites in CI.
- Completion requires finishing the pending audit rows and passing their checks.

## Inventory and remaining audit

| Capability | Existing executable evidence | Audit additions / remaining work |
| --- | --- | --- |
| TLS public protocol, hello/frame parsing and bounds | `TestProductProtocol`, `TestRelayRequiresExplicitVersionAndForwardsOverTLS13` | Added exact payload/heartbeat limits, malformed credential/signature/identity matrix, stream interception and strict-frame fuzz target |
| Credentials and trust rotation | `TestProductTrustRotation`, upstream contract tests | Added invalid trust matrix, inclusive/exclusive key validity, unknown key, typed-nil protobuf rejection |
| Authorization, directed ACL and network isolation | `TestProductAuthorizationControl`, `TestProductCrossNetworkRouting` | Added fresh/stale exact TTL and slow-error boundaries, ownership and bounded-cache replacement regressions |
| Control client and lease state | `TestRegressionHungHeartbeatMustFence`, `TestProductFencing` | Added nil/invalid replies, epoch/route bounds, atomic rejected-state update and trust ownership |
| Session lifecycle, local delivery, fencing | `TestSessionRenewalFailureClosesSession`, `TestProductFencing`, `TestProductResources` | Added authenticated-session fencing/readiness, replacement and late-cleanup regressions; final E2E pending |
| Mesh forwarding, boot/epoch fencing, reconnect | `TestMultiRelayInfrastructure`, `TestProductMeshFencing` | Added bounded queue, payload ownership, invalid-route and unchanged/removed peer snapshot tests; final E2E pending |
| Limits, queues, bandwidth and metrics | `TestProductResources`, admission tests | Added exact bandwidth/replenishment, closed/full queue, concurrent counter and bounded metric-label checks; final E2E pending |
| Persistent registry, sessions, endpoints | PostgreSQL integration tests; `TestProductSnapshotPersistence` | Added shared Memory/PostgreSQL session matrix and snapshot aliasing/ordering/rollback cases; real PostgreSQL execution pending |
| SPIFFE identity, trust domain, renewal and outage | `TestProductSPIFFE`, `TestProductSPIREOutage`, custom-domain matrix | Added own-identity revalidation after source rotation, missing SVID, error propagation and recovery |
| Startup, health/readiness, shutdown and smoke tool | Product trust-bootstrap, snapshot, lifecycle groups | Added live metrics/readiness transitions, storage outage/recovery, cancellation/timeout, boot IDs, environment parsing and trusted/untrusted TLS smoke checks |
| Experimental extension validation | `TestContracts`, `TestRejectInvalidAuthorityAndBounds`, `FuzzDiscoveryValidation` | Added address/count/nonce/proof/timing/region/identity boundaries; 100% statement coverage of validators locally, not a runtime activation claim |
| Testserver and external-client fixture | `TestUpstreamTestServer`; full E2E | Added strict configuration, unauthenticated harness rejection, fixture configuration, scoped leases/replacement/release and explicit unsupported-mesh checks; external client repositories excluded |

The full product scenario inventory and CI execution model are maintained in
[product-e2e.md](product-e2e.md). This audit must remain marked in progress until
the pending verification rows are resolved, not merely until one narrow CI job is green.

`TestProductMatrixIncludesEveryRegularScenario` checks the actual Go declarations
against the CI selectors. The explicitly manual `TestProductExtended` soak is
not silently counted as part of regular E2E. Generated protobuf getters and
unreachable codec implementation branches are not a substitute for behavioral
requirements and do not have arbitrary line-coverage targets.

Local bounded fuzz evidence for this audit: `FuzzStrictPublicFrame` executed
1,173,981 inputs and `FuzzDiscoveryValidation` executed 1,067,169 inputs without
failure in 5-second requested runs (about 6 seconds including shutdown). These
are finite fuzz runs, not proof over all possible inputs.

## Reproduced regressions

- Cached trust output and upstream-owned input could mutate cache state through
  shared key slices/time pointers: `TestTrustCacheOwnsInputAndOutput`.
- Updating an existing full-cache entry unnecessarily evicted another entry:
  `TestCacheReplacementDoesNotEvictUnrelatedEntry`.
- A typed nil protobuf reply was accepted as a successful empty response:
  `TestRejectTypedNilProtobufMessages`, `TestControlRejectsNilAndInvalidSessionResponses`.
- Control-client trust validity pointers escaped through a shallow output copy:
  `TestCoordinatorStateRejectedAtomicallyAndTrustCopied`.

Each test failed before its corresponding fix. Follow-up full product verification
is required for the final source commit; earlier successful E2E is not evidence
for these fixes.
