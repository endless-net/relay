# Relay v1 public wire candidate

Источник — `63b65114dc2db99db1788ce3e4781f634690cfb8`:
[`relayapi/v1/protocol.go`](https://github.com/endless-net/relay/blob/63b65114dc2db99db1788ce3e4781f634690cfb8/relayapi/v1/protocol.go),
[`protocol_test.go`](https://github.com/endless-net/relay/blob/63b65114dc2db99db1788ce3e4781f634690cfb8/relayapi/v1/protocol_test.go),
[`internal/relay/relay.go`](https://github.com/endless-net/relay/blob/63b65114dc2db99db1788ce3e4781f634690cfb8/internal/relay/relay.go).
Это неопубликованный migration candidate, без runtime, experimental extensions,
private-key signing helper или нового Coordinator callback authority.

JSON Schema [`public-wire.schema.json`](schemas/public-wire.schema.json) имеет
стабильный `$id` `urn:endlessnet:relay:public-wire:v1`. Coordinator может ссылаться
на `$defs/Endpoint`, `$defs/Credential`, `$defs/SigningTrustBundle` через закрытый
локальный registry. Модели не копируются потребителем.

| Shape | Семантика и ограничения |
| --- | --- |
| Endpoint / EndpointSnapshot | `relay-v1-tls`, canonical nonempty ID/address, priority ≥0, optional trimmed region; positive snapshot version и уникальные endpoint IDs. Пустой/null endpoints не означает готовый relay |
| Credential | Algorithm `ed25519-relay-credential-v3`, key ID, explicit Network/Node, expiry, raw URL base64 signature; ключа в credential нет |
| SigningTrustKey / SigningTrustBundle | Bundle version 1, Ed25519 public bytes, derived SHA-256 key ID, unique keys и существующий active key; optional half-open validity interval |
| ClientHello | Type `client_hello`, version 1, credential; heartbeat отсутствует/0 либо 100..30000 ms. Нулевое значение не назначает production policy |
| ClientFrame | Type `client_frame`, version 1, explicit `peer_network_id` и `peer_id`, opaque payload |
| ServerFrame | Type `server_frame`, version 1, explicit `from_network_id` и `from_node_id`, opaque payload |
| Ready / Error / Heartbeat | Отдельные versioned message kinds; ready=true сообщает готовность сессии, error не grant, heartbeat time — RFC3339Nano |

TLS stream несёт UTF-8 JSON objects, по одному на строку; writer завершает запись
байтом LF (`0a`). Исторический decoder отвергает unknown fields и несколько JSON
values в одной строке; frame bound — 147456 bytes. JSON payload кодируется обычным
padded base64; decoded payload содержит 1..65536 байт. Верхняя граница проверяется
также для текстов одинаковой base64 длины: 65536 принимается, 65537/65538 отвергаются.
Payload не интерпретируется и не раскрывается control authority.

Schema проверяет форму и bounds. Offline checker дополнительно проверяет hash
публичного key ID, active/duplicate keys, порядок validity window, expiry на
явном fixture clock и duplicate endpoint IDs. Public material fixtures синтетические;
signature bytes не являются действительной подписью или credential. Подпись,
authenticated trust transition, TLS peer, pair authorization, cache/revocation,
lease и прекращение потоков остаются отдельными runtime gates. Historical
`DecodeStrict` сам по себе не обещает отбрасывать duplicate JSON keys; strict
fixture loader не является доказательством production decoder behavior.

Проверка из корня workspace в Ubuntu 26.04 с закреплёнными зависимостями
`jsonschema==4.25.1`, `referencing==0.37.0`:

```bash
python3 services/relay/api/checks/verify_public_wire.py
```

[`fixtures/cases.json`](fixtures/cases.json) связывает shape cases с `$defs`;
[`wire-vectors.json`](wire-vectors.json) содержит exact bytes с delimiter,
opaque payload bytes и semantic cases. Проверка не открывает сеть, не запускает
сервер и не использует private keys. При первом выпуске нет published baseline;
неподтверждённые совместимые consumer revisions этим набором не назначаются.
