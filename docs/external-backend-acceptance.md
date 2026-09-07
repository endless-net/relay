# External backend acceptance driver

`internal/relay/backend_integration_test.go` provides a test-process driver for
an external compatible upstream. Build it from an exact source commit:

```sh
go test -c -o relay-backend.test ./internal/relay
RELAY_EXTERNAL_BACKEND_TEST=1 ./relay-backend.test -test.v \
  -test.run='^TestExternalBackendDataplane$' -test.timeout=5m
```

Set `RELAY_BACKEND_DATABASE_URL` to an isolated loopback PostgreSQL database.
The driver creates an owned schema, applies the production store migrations,
and removes that schema on normal shutdown. It must never target production.

The parent writes one JSON object to stdin with `UpstreamAddress` (a loopback
host and port) and `UpstreamCAPEM` (JSON-encoded bytes containing public PEM CA
certificates). The upstream must implement the published gRPC service, including
trust discovery. The parent must retain stdin until its traffic assertions finish.

After startup, stdout contains `ENDLESSNET_RELAY_READY ` followed by a JSON object
with `Address` and `CAPEM`. Other stdout lines are test logs. The parent configures
its clients with this public endpoint/trust and credentials from its own issuer,
then closes stdin after completing acceptance. The marker proves readiness only;
the parent owns traffic, denial and state-transition assertions and must check the
driver's final exit status.

The driver uses the production PostgreSQL store, Relay Coordinator,
`authz.GRPCAuthorizer` and default authorization cache, control client, mesh
manager and TLS dataplane. Control connections authenticate an ephemeral Relay
SPIFFE URI certificate. All certificates are isolated fixtures; upstream uses
server-authenticated TLS. This is a single-instance driver, not Workload API
rotation, exact upstream caller identity or multi-instance mesh acceptance.
Ordinary tests skip it unless explicitly enabled; a successful ordinary test run
does not prove external backend integration.
