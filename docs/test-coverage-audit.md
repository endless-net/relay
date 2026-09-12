# Relay functional test coverage audit

Status: complete for the implemented Relay product, verified on 2026-09-13 (Europe/Moscow). Scope: only this repository and the Relay product. This
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

## Verified functional inventory

| Capability | Existing executable evidence | Audit additions / remaining work |
| --- | --- | --- |
| TLS public protocol, hello/frame parsing and bounds | `TestProductProtocol`, `TestRelayRequiresExplicitVersionAndForwardsOverTLS13` | Added exact payload/heartbeat limits, malformed credential/signature/identity matrix, stream interception and strict-frame fuzz target |
| Credentials and trust rotation | `TestProductTrustRotation`, upstream contract tests | Added invalid trust matrix, inclusive/exclusive key validity, unknown key, typed-nil protobuf rejection |
| Authorization, directed ACL and network isolation | `TestProductAuthorizationControl`, `TestProductCrossNetworkRouting` | Added fresh/stale exact TTL and slow-error boundaries, ownership and bounded-cache replacement regressions |
| Control client and lease state | `TestRegressionHungHeartbeatMustFence`, `TestProductFencing` | Added nil/invalid replies, epoch/route bounds, atomic rejected-state update and trust ownership |
| Session lifecycle, local delivery, fencing | `TestSessionRenewalFailureClosesSession`, `TestProductFencing`, `TestProductResources` | Added authenticated-session fencing/readiness, replacement and late-cleanup regressions; verified by the full regular E2E evidence below |
| Mesh forwarding, boot/epoch fencing, reconnect | `TestMultiRelayInfrastructure`, `TestProductMeshFencing` | Added bounded queue, payload ownership, invalid-route and unchanged/removed peer snapshot tests; verified by the full regular E2E evidence below |
| Limits, queues, bandwidth and metrics | `TestProductResources`, admission tests | Added exact bandwidth/replenishment, closed/full queue, concurrent counter and bounded metric-label checks; verified by the full regular E2E evidence below |
| Persistent registry, sessions, endpoints | PostgreSQL integration tests; `TestProductSnapshotPersistence` | Added shared Memory/PostgreSQL session matrix and snapshot aliasing/ordering/rollback cases; verified in the real PostgreSQL CI job |
| SPIFFE identity, trust domain, renewal and outage | `TestProductSPIFFE`, `TestProductSPIREOutage`, custom-domain matrix | Added own-identity revalidation after source rotation, missing SVID, error propagation and recovery |
| Startup, health/readiness, shutdown and smoke tool | Product trust-bootstrap, snapshot, lifecycle groups | Added live metrics/readiness transitions, storage outage/recovery, cancellation/timeout, boot IDs, environment parsing and trusted/untrusted TLS smoke checks |
| Experimental extension validation | `TestContracts`, `TestRejectInvalidAuthorityAndBounds`, `FuzzDiscoveryValidation` | Added address/count/nonce/proof/timing/region/identity boundaries; 100% statement coverage of validators locally, not a runtime activation claim |
| Testserver and external-client fixture | `TestUpstreamTestServer`; full E2E | Added strict configuration, unauthenticated harness rejection, fixture configuration, scoped leases/replacement/release and explicit unsupported-mesh checks; external client repositories excluded |

The full product scenario inventory and CI execution model are maintained in
[product-e2e.md](product-e2e.md). Every inventory row above is backed by its named
regressions and the relevant product groups, not merely by an aggregate green job.

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

The four regressions were reproduced before their fixes; additional supporting
contract tests were added afterward. The full regular product verification below
covers these fixes, not an earlier implementation snapshot.

## Regular verification evidence

[PR #28](https://github.com/endless-net/relay/pull/28) was merged as
`221611cd689469167b717e33e18ee8e33e084e2a` after
[CI 34719935205](https://github.com/endless-net/relay/actions/runs/34719935205)
passed all required checks. Its PR head is
`867294c82ada49f47534334da69c411f727df4cc`; the product artifacts record GitHub's
tested merge checkout `eb34b5d31ac30a2b95637d28cf0cb18d6c0484e8`.
All three commits have the identical Git tree
`02133be22413aef8bc730f356ad71544aef9a344`.

The downloaded JSON event artifacts prove the following results. Counts are
terminal/leaf test cases, including repeated scenarios under the custom trust
domain; they are not counts of distinct feature implementations.

| Product group | Passed leaf cases | Package duration, seconds |
| --- | ---: | ---: |
| protocol/auth | 18 | 93.971 |
| SPIFFE/mesh | 9 | 55.035 |
| custom-domain | 15 | 80.721 |
| trust-bootstrap | 3 | 49.613 |
| snapshot | 2 | 23.296 |
| fencing/recovery | 3 | 58.397 |
| resources/lifecycle | 8 | 44.446 |
| Total | 58 | — |

There are no failed or skipped test cases. Each group additionally reports two
package-level `skip` events for the testless `e2e/cmd/fault-proxy` and
`e2e/cmd/mock-coordinator` packages; these are not omitted scenarios.
The real PostgreSQL job passed both `TestPostgresReleasedEpochCannotBeReused`
(including the shared store matrix) and
`TestPostgresPersistenceExpiryOverflowAndSnapshots`.

Runtime vet/race/build, isolated API verification, protobuf generation/lint,
dependency scanning, CodeQL and signoff also passed in this run. Locally, all
three command packages passed ten repeated race-enabled runs.

## Regression source index

- [Authorization cache boundaries and ownership](../internal/authz/cache_contract_test.go).
- [Control response validation and state atomicity](../internal/relaycontrol/response_contract_test.go).
- [Fencing/readiness](../internal/relay/lifecycle_contract_test.go),
  [queue/session/bandwidth boundaries](../internal/relay/resource_boundaries_test.go)
  and [concurrent metrics](../internal/relay/metrics_contract_test.go).
- [Mesh queue and peer-snapshot invariants](../internal/mesh/queue_contract_test.go).
- [Shared storage contract](../internal/store/contract_matrix_test.go) and
  [real PostgreSQL invocation](../internal/store/postgres_integration_test.go).
- [SPIFFE identity-source rotation](../internal/tlsconfig/source_contract_test.go).
- [Relay health and bootstrap](../cmd/endlessnet-relay/health_test.go),
  [Relay Coordinator readiness](../cmd/endlessnet-relay-coordinator/health_test.go)
  and [TLS smoke checks](../cmd/endlessnet-relay-smoke/main_test.go).
- [Public credential boundaries](../relayapi/v1/credential_boundaries_test.go),
  [trust/frame/snapshot boundaries](../relayapi/v1/trust_boundaries_test.go),
  [typed gRPC contract](../relayapi/v1/relayapigrpc/contract_test.go) and
  [experimental extension boundaries](../relayapi/v1/extensions/boundaries_test.go).
- [Stateful upstream peer](../internal/testserver/server_test.go),
  [harness configuration](../internal/testserver/config_test.go) and
  [external-client fixture](../relaytest/relaytest_test.go).
- [CI scenario selection guard](../deploy/e2e_inventory_test.go).

## Extended recovery and final acceptance

[Extended CI 34719932178](https://github.com/endless-net/relay/actions/runs/34719932178)
ran the regular product matrix and the separately selected `TestProductExtended`
on source `867294c82ada49f47534334da69c411f727df4cc`.
Its JSON artifact records **1801.960 seconds, 317 completed recovery cycles,
zero failures and zero skipped test cases**. Each cycle verifies bidirectional
connectivity, then recreates a Relay; every tenth cycle also stops and starts
Relay Coordinator. This is finite 30-minute recovery evidence, not an unlimited
availability or performance guarantee.

The merge commit was independently verified by
[main CI 34721117770](https://github.com/endless-net/relay/actions/runs/34721117770).
The PR, manual and main runs all test the same executable source tree. This final
evidence update changes documentation only and does not claim a new runtime test.

| Acceptance requirement | Evidence / boundary |
| --- | --- |
| Implemented product functionality inventoried | Functional inventory and linked source index above; every regular E2E declaration is selected by CI |
| Missing boundary and regression tests implemented | New source-linked tests, runtime/API race and vet checks, plus ten repeated command-package runs |
| Discovered defects fixed | Four reproduced regressions, passing regression tests and full E2E on the fixed code |
| Real complete Relay service integration | Three Relay instances, actual Relay Coordinator, PostgreSQL and mesh; 58 regular leaf cases |
| Only external Coordinator replaced | Stateful upstream testserver, verified over HTTP/2/mTLS and the published contract |
| SPIFFE not mocked in full integration | Real isolated SPIRE bootstrap, renewal, identity rejection and outage scenarios in two trust domains |
| Long-running recovery verified | 317 cycles in 1801.960 seconds on the same source as the merged implementation |
| Experimental API types kept separate from runtime | Shape/boundary validation only, including 100% extension-validator statement coverage and bounded fuzz runs |
| Repository and version scope preserved | Only Relay files changed; no contract/protocol/module version increase, production access, deployment or other repository changes |

No deployment is required to establish this test result. Integrator upstream
conformance and production acceptance remain explicitly outside this Relay-only
goal. Testing establishes the documented finite scenario coverage, not proof that
all possible faults or future features have been covered.
