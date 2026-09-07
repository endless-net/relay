# Возможное будущее EndlessNet Relay

Статус: варианты для обсуждения, не утверждённый roadmap.

Ни один пункт ниже не следует считать обещанием или выбранным дизайном. Перед
реализацией он должен получить владельца, измеримую цель, отдельное решение и
план совместимого rollout. Базовый принцип — не усложнять data plane без
наблюдаемой проблемы.

## 1. Ближайшее укрепление контрактов и эксплуатации

### F-02. Ввести SLO и наблюдаемость control/mesh plane

**Проблема.** Relay имеет базовые counters, но нет метрик Relay Coordinator,
состояния mesh peers, latency авторизации, запаса lease и возраста trust bundle.

**Возможный объём.** Метрики RPC latency/error codes, PostgreSQL latency,
cache hit/stale use, active/fenced instances, session renew failures, mesh
ready/reconnect/queue depth, credential key age. Добавить dashboards и alerts,
не включая node IDs или payload. Сквозной correlation ID допустим только после
анализа кардинальности и приватности.

**Критерий запуска.** Сначала определить SLI: успешность connect, p95/p99
delivery latency, доля ACL/control failures, время восстановления mesh и доля
drops.

### F-03. Уточнить readiness, drain и post-deploy проверки

Уточнение 7 сентября: Relay уже проверяет listener, instance lease и пригодный
trust в `/readyz`; shutdown ограничен 5 секундами. Оставшаяся часть пункта —
наблюдаемость и операционная приёмка, принадлежащая инфраструктуре оператора.

**Проблема.** Текущий Relay `/healthz` не отражает control lease или mesh, а
Relay Coordinator `/readyz` проверяет только чтение endpoint snapshot. При
deploy Coordinator проверяется systemd state, но не полный mTLS control flow.

**Варианты.** Разделить liveness и readiness; считать Relay ready только после
регистрации и trust bundle; экспонировать запас instance lease; проверять
PostgreSQL, upstream authorization и trust bundle с разной критичностью;
добавить bounded drain period и production smoke после каждого этапа rollout.

**Ограничение.** Readiness не должна создавать каскадный отказ из-за краткой
недоступности необязательной зависимости.

### F-04. Сделать миграции и housekeeping безопасными для нескольких replicas

**Проблема.** Миграции применяются каждым процессом при старте без журнала и
явной распределённой блокировки. Истёкшие instances и sessions остаются в БД.

**Варианты.** Монотонный migration ledger, advisory lock, отдельный migration
job, периодическая batch-очистка с метриками возраста/объёма. Сохраняются
ограничения проекта: без `DEFAULT`, явного `NOT NULL` и PostgreSQL foreign keys.

### F-05. Формализовать жизненный цикл ключей и сертификатов

**Проблема.** Код поддерживает перекрытие signing keys, но безопасный результат
зависит от операционной последовательности выдачи trust bundle, credential и
сертификатов workloads.

**Возможный объём.** Runbook ротации с overlap window, expiry alerts,
автоматическая проверка URI SAN, staged rollout CA bundle, emergency revocation
и тесты clock skew. Private keys по-прежнему не входят в release artifacts.

## 2. Отказоустойчивость production

### F-06. Несколько Relay Coordinator replicas

Код отделяет Coordinator от процесса Relay, а lease state уже находится в
PostgreSQL. Следующий шаг возможен только после безопасных concurrent migrations,
идемпотентных startup operations, балансировки gRPC/HTTPS и chaos-тестов.

Нужно заранее решить:

- является ли PostgreSQL единственным fencing authority;
- как Relay переключается между адресами Coordinator;
- какой outage budget допустим для per-frame authorization;
- как исключить одновременную публикацию различающихся endpoint snapshots.

Само добавление второй replica не устраняет зависимость от одного PostgreSQL.

### F-07. Развернуть active-active Relay topology в production

E2E проверяет три Relay, а production workflow валидирует основной Relay и
резервный Relay на `spb`. Следующие этапы: проверить клиентский выбор endpoint,
session migration, отказ хоста, возврат хоста с новым `boot_id`, затем
региональное расширение.

До active-active эксплуатации всё ещё нужны SLO, capacity model и
автоматизированный game day с проверкой fencing. Сертификаты остаются отдельными
для каждого публичного Relay DNS-имени.

## 3. Масштабирование control path

### F-08. Снизить стоимость авторизации каждого кадра без ослабления revocation

**Сигнал.** p99 control latency влияет на delivery latency или QPS
`AuthorizePeer` ограничивает кластер.

**Варианты для сравнения.** Локальный bounded cache маршрутов на Relay,
подписанные policy snapshots, push-инвалидация по `network_revision`, batch RPC
или capability token на пару узлов. Любой вариант должен иметь ограниченный
срок, fail-closed поведение после него и тест на отзыв ACL.

Не следует просто увеличивать stale TTL: это напрямую расширяет окно после
revocation.

### F-09. Пересмотреть full mesh только при подтверждённом пределе

**Сигнал.** Число Relay делает O(N²) streams, reconnect storms или fan-out
peer updates измеримой проблемой.

**Варианты.** Региональные gateways, sparse topology с route discovery,
иерархический mesh или отдельный transport backbone. Нужно сравнить hops,
blast radius, стоимость cross-region traffic и сложность fencing.

Для небольшого числа Relay текущий one-hop full mesh остаётся предпочтительнее
из-за простоты.

### F-10. Определить обратную связь и flow control для удалённой доставки

**Проблема.** Fire-and-forget mesh не сообщает исходному Relay результат
удалённой доставки кадра.

**Варианты.** Message ID и bounded acknowledgment, явные коды `fenced`,
`no_peer`, `slow_consumer`, credit-based mesh flow control или документированное
сохранение fire-and-forget semantics.

Перед изменением нужно выбрать продуктовую гарантию. Ack от Relay означает
только приём в память и не должен выдаваться за end-to-end delivery. Durable
broker следует добавлять лишь при отдельной бизнес-потребности.

### F-11. Динамически распространять endpoint snapshot

**Сигнал.** Изменение endpoint через restart создаёт неприемлемую операционную
задержку.

**Варианты.** Watch API главного Coordinator, подписанный snapshot в object
storage или отдельный admin RPC. Обязательны монотонная версия, hash содержимого,
атомарная замена, rollback policy и защита от двух разных snapshots одной
версии.

## 4. Эволюция data protocol

### F-12. Рассматривать relay-v2 только по результатам измерений

JSON-lines удобен для диагностики, но base64 и JSON увеличивают размер кадра.
Версия 2 может рассмотреть length-prefixed binary framing, protobuf или QUIC,
если CPU, bandwidth или head-of-line blocking станут измеримым пределом.

Минимальные требования к v2:

- одновременная поддержка v1 и v2 во время rollout;
- явное согласование версии без downgrade;
- те же или более строгие limits и unknown-field policy;
- fuzzing parser и cross-version conformance suite;
- отсутствие логирования payload;
- план отключения v1 по наблюдаемой доле клиентов, а не по календарной дате.

### F-13. Улучшить fairness и защиту от злоупотреблений

При росте публичной нагрузки можно измерить необходимость token-bucket вместо
секундного window, квот на сеть, динамических per-source limits и интеграции с
edge DDoS protection. Решение не должно позволять высокой кардинальности
идентификаторов исчерпать память до аутентификации.

## 5. Инженерная уверенность

Полезные расширения тестового контура:

- нагрузочные тесты admission, auth cache, per-frame RPC и mesh queues;
- fuzz tests публичного decoder, credential/trust bundle и mesh message
  validation;
- chaos cases: PostgreSQL failover, длительный Coordinator outage, clock skew,
  packet loss, asymmetric partition, certificate expiry и key rotation;
- upgrade/downgrade matrix для соседних версий Relay и Coordinator;
- проверка arm64 artifacts и реального systemd rollback;
- длительный soak test с reconnect и контролем goroutine/memory growth.

## 6. Сигналы для выбора следующего шага

| Наблюдаемый сигнал | Сначала рассмотреть |
| --- | --- |
| Нельзя объяснить drop или fencing incident | F-02, затем F-03 |
| Ошибка rollout или несовместимый клиент | F-01 и upgrade matrix |
| Coordinator outage превышает SLO | F-04 и F-06 |
| Нужен ещё один production Relay/регион | F-02, F-03 и F-07 |
| p99 delivery связан с control RPC | F-08 |
| Mesh streams/reconnect создают предел | F-09 |
| Пользователям нужна причина удалённого drop | F-10 |
| Endpoint меняются чаще release cycle | F-11 |
| JSON/base64 подтверждённо ограничивает CPU или сеть | F-12 |

## 7. Инварианты, которые не следует размывать

Если отдельный ADR не докажет обратное, развитие должно сохранять:

- главный Coordinator как источник истины для ACL, revocation и signing trust;
- TLS 1.3 на production listener и mTLS identities на control/mesh;
- строгую версионированность контрактов;
- fencing старых processes и sessions;
- ограниченные память, очереди и время ожидания;
- отсутствие payload в БД, метриках и обычных логах;
- immutable releases, проверяемое происхождение и откат;
- отсутствие secrets и production credentials в репозитории и artifacts.

## 8. Процесс принятия будущего изменения

Перед началом реализации:

1. зафиксировать baseline и целевой SLO;
2. описать security и privacy impact;
3. проверить совместимость wire, gRPC, схемы и deployment;
4. выбрать canary/rollback стратегию;
5. добавить unit, race, integration, E2E и при необходимости chaos/load tests;
6. определить новые метрики и alerts;
7. оформить ADR и только затем менять production topology или contract.
