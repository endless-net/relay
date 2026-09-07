# Autonomous product E2E

Relay owns this suite. It requires Docker Compose on a GitHub-hosted
`ubuntu-latest` runner and builds production binaries from one source SHA.
No sources or CI from an integrator's upstream, credential issuer, client or
acceptance suite are used.
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
| protocol/auth and custom domain | Typed upstream RPCs, exact caller identity, unknown fields, unsupported service version, inactive destination, deadline/recovery and removed JSON routes | `TestProductUpstreamContract` |
| protocol/auth | Signing-key overlap, new credential, retirement, recovery | `TestProductTrustRotation` |
| trust-bootstrap | Invalid/unavailable trust prevents startup; recovery after trust returns; plaintext upstream configuration rejected before startup | `TestProductNoTrustBootstrap` |
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
[authz/testdata](../internal/authz/testdata/), outside the
E2E upstream implementation.

## Running and evidence

```sh
go test -race ./...
go test -tags=e2e -count=1 -timeout=15m -run TestProductProtocol ./e2e/...
E2E_EXTENDED=1 go test -tags=e2e,extended -count=1 -timeout=40m -run TestProductExtended ./e2e/...
```

CI builds the images once and passes their archive, checksum, source SHA and
image IDs to all matrix groups. Container dependencies are pinned by digest.
The `verify` check succeeds only after static/unit checks and every
product group succeed; branch protection remains enabled. The release workflow
reuses the full CI workflow, including static/unit checks and product E2E, before
publishing artifacts. Automatic checks run on pushes to `main`; release publication
runs on new `vMAJOR.MINOR.PATCH` tags. CI and DCO can also be dispatched manually
on a branch for additional verification. The soak is compiled only with the additional `extended` build tag; ordinary
E2E runs do not silently skip it. Each regular group has a 20-minute job deadline.
The manual extended job allows 45 minutes including setup for a 30-minute soak.

Every run publishes JSON test events with durations, source SHA and image IDs.
Diagnostics include container status, redacted logs and metrics; fixture PKI,
credentials and payloads are never uploaded. Artifacts expire after 7 days.
Cleanup runs regardless of test outcome. No automatic test retries conceal an
initial failure. Destructive snapshot and trust-bootstrap cases have their own
Compose projects; network faults are commanded through a bounded-buffer proxy.

Acceptance recorded on 2026-09-07 before the upstream protobuf transport cutover
(the following source SHAs and results describe that checkpoint):

- [Final server CI and extended](https://github.com/endless-net/relay/actions/runs/34121728292): source `3c3b7ad68d3785d49bf58204ffcd8aaf1f48db7e`; all seven regular groups passed, with **44 leaf E2E scenarios**. This includes rejection of plaintext upstream configuration before storage/listeners start.
- The same run passed the extended recovery test in **1802.61 seconds**, completing **347 recovery cycles**, using the same source SHA and image archive as its regular groups.
- Earlier acceptance remains available: [initial regular CI](https://github.com/endless-net/relay/actions/runs/34119731480) (43 leaf scenarios) and [initial extended](https://github.com/endless-net/relay/actions/runs/34117312163) (1801.94 seconds, 351 recovery cycles). Those runs preceded the additional HTTPS-origin startup guard.

| Regular group | Leaf scenarios | Go package elapsed, seconds |
| --- | ---: | ---: |
| protocol/auth | 10 | 80.390 |
| SPIFFE/mesh | 9 | 81.338 |
| custom domain | 9 | 80.218 |
| fencing/recovery | 3 | 63.143 |
| resources/lifecycle | 8 | 56.980 |
| snapshot | 2 | 17.573 |
| trust bootstrap | 3 | 51.934 |

The following are Docker image configuration IDs, not published release tags.
Regular and extended jobs use the same image archive; its checksum binds the complete set of loaded images.

| Input | Final server CI SHA-256 |
| --- | --- |
| Relay image ID | `db8fd18595441f7da559447f3f2455a2d5653ddf98e5642ef8b969513c3e608d` |
| Relay Coordinator image ID | `d35c0c33a115ae04cf402b05da9f1edbac2ca41f98e5fb625626da0b3773b153` |
| Upstream/proxy image ID | `02d782245e73b89df58074ab0a260dd340144f0f96081045ea50bd99c4e7f6d8` |
| Image archive | `95d97fab2f14cac69041fb374f9f78fc706d81a450a542d6b062ead8200de159` |

Initial failures remain in Actions history, including
[setup/conformance](https://github.com/endless-net/relay/actions/runs/34114320117),
[directed recovery](https://github.com/endless-net/relay/actions/runs/34115225512)
and the [first soak](https://github.com/endless-net/relay/actions/runs/34116137250).
These were separate failed runs followed by corrective commits, not automatic retries.

Product evidence verifies Relay against its published contracts. Each integrator
separately validates its upstream compatibility, infrastructure integration and
production deployment.

## Upstream protobuf cutover evidence

[CI 34132527958](https://github.com/endless-net/relay/actions/runs/34132527958)
passed all seven groups, PostgreSQL invariants and required verification after
switching upstream AuthZ/trust to gRPC. PR source:
`e6eadc099cf0de8d9e88cbc9039f3f868c8adb32`; built/tested merge checkout:
`6bd437fbeb10a61eaff6516a026e07d3f3f3d96d`. **56 leaf E2E scenarios passed**.
This is regular-suite evidence; the earlier extended results above belong to
their recorded source SHAs.

| Group | Leaf scenarios | Go package elapsed, seconds |
| --- | ---: | ---: |
| protocol/auth | 16 | 76.186 |
| SPIFFE/mesh | 9 | 87.237 |
| custom domain | 15 | 81.889 |
| fencing/recovery | 3 | 62.851 |
| resources/lifecycle | 8 | 47.195 |
| snapshot | 2 | 17.573 |
| trust bootstrap | 3 | 50.507 |

All groups loaded the same archive and checked its source SHA and checksum.
These SHA-256 values identify CI Docker configurations and the image archive;
no registry release was published.

| Input | SHA-256 |
| --- | --- |
| Relay image ID | `49dff21308f1a9d1f87dfee63b63411518a786874f4d4184314a43206805acb9` |
| Relay Coordinator image ID | `9c51705488cdfad1c7784523845b5fb113ec01aaf344d65d4d4902689a29c84e` |
| Upstream/proxy image ID | `99d6845bbac9b84605458aedb40fa1eacd174f3ed65e319bcaa966530f0d2bf4` |
| Image archive | `905622dde8b3251644d7590ccd7b7d7d7f1481a119649c35812130c64d8cdd79` |

[Run 34133071126](https://github.com/endless-net/relay/actions/runs/34133071126)
exposed a recovery-test timing defect: a 12-second rejection deadline was shorter
than a still-valid 15-second destination lease if shutdown release did not finish.
The test now observes release/expiry in PostgreSQL before asserting network rejection,
and restores the stopped Relay on failure. The failed run remains visible; no
automatic retry masks it. Production authorization and lease semantics are unchanged.
