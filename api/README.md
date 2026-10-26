# Relay API: registry и mesh

Producer — Relay, RB-relay. Требования находятся в
[единой спецификации workspace](../../../specs/001-endlessnet-project/spec.md).
Самостоятельный module — `github.com/endless-net/relay/api`, generated Go/gRPC
package — `github.com/endless-net/relay/api/relay/v1`. Runtime/handlers/storage
и host provisioning/deploy/activation не входят в API-модуль.

## Поверхность и ownership

[proto/relay/v1/relay.proto](proto/relay/v1/relay.proto) содержит RelayControl
RegisterInstance/HeartbeatInstance/AcquireSession/RenewSession/ReleaseSession/
AuthorizePeer и RelayMesh.Connect bidirectional stream. Source/destination
network identities и epoch/boot fencing сохраняют разные поля. Relay передаёт
opaque payload и не принимает организационную политику.

Источник: [63b65114dc2db99db1788ce3e4781f634690cfb8](https://github.com/endless-net/relay/blob/63b65114dc2db99db1788ce3e4781f634690cfb8/proto/relay/v1/relay.proto).
Исторический callback [upstream.proto](https://github.com/endless-net/relay/blob/63b65114dc2db99db1788ce3e4781f634690cfb8/proto/relay/v1/upstream.proto)
описывал RelayUpstreamService.AuthorizeCredential/AuthorizePeerPair/GetTrustBundle.
Он не скопирован и не генерируется как canonical Relay SDK: Network/Node/pair
authority принадлежит Coordinator, purpose trust — Signing. EDGE-Q05 сохраняет
необходимость wire/ownership migration; наличие ссылки не утверждает новую
совместимость и не создаёт второго policy producer.

Внешний `relay-v1-tls` dataplane с JSON framing и opaque WireGuard payload —
отдельный wire contract. Он не заменён на Protobuf RPC. Experimental extensions
не становятся частью текущего v1. Историческая recursive rejection unknown
protobuf fields — runtime behavior; tests preservation unknown bytes в SDK
не утверждают его acceptance этим runtime.

## Генерация и версии

Exact source/tool/runtime pins: [toolchain.json](toolchain.json). Buf1.50.0,
protoc-gen-go1.36.10 и protoc-gen-go-grpc1.5.1 — локальные инструменты; remote
plugins и schema registry не используются. Go минимум1.26.0 из реальных
runtime dependency constraints.

```bash
buf format --diff --exit-code
buf build
buf lint
buf generate
goimports -w .
go build ./...
go vet ./...
go test -short ./...
```

Goimports — exact v0.48.0. Повторяется полный generation/format pipeline,
сравниваются bytes/SHA256 generated Go files. Узкие исторические lint exceptions
объясняют сохранённые package path/RelayControl/RelayMesh/Connect naming;
STANDARD остальные и breaking FILE действуют.

Новая API-линия не опубликована; будущий `api/v<semver>` tag независим от wire
`endlessnet.relay.v1` и runtime. Published supported `api/` baseline первого
выпуска отсутствует. [migration.json](migration.json) фиксирует 2 намеренно
несовместимых historical findings: Go import переход `relayapi/` → `api/` и
quarantine upstream.proto. Сравнение нормализованных оставшихся wire shapes
проходит; это не скрывает удаления callback и не является release baseline.
Consumer/runtime imports в этом этапе не меняются.

Контрактные tests проверяют inventory без foreign authority, направленные
network identities, epoch/opaque bytes roundtrip и malformed wire. Они не
реализуют signature/upstream permission/cache/lease semantics: эти runtime gates
и numeric budgets должны быть согласованы отдельно до реализации.

Dependency security gate: `golang.org/x/net` закреплён на `v0.60.0` после
findings GO-2026-6617/6612/6611/6603 в `v0.58.0`. Транзитивные
`x/sys v0.48.0` и `x/text v0.42.0` и minimum Go1.26 согласованы в go.mod.
Повторные результаты обязательного vulnerability scan фиксируются workspace.

Public dataplane JSON shapes, framing, limits и offline vectors находятся в
[PUBLIC-WIRE.md](PUBLIC-WIRE.md), отдельно от registry/mesh Protobuf. Схема
`urn:endlessnet:relay:public-wire:v1` остаётся producer-owned. Эти fixtures
не подтверждают подпись, доверенную ротацию, pair authorization или production.
