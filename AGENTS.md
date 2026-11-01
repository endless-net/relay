# Правила работы: relay

Репозиторий публичный и использует автономный локальный профиль по STD-030,
STD-035 и STD-036. Этот профиль заменяет прежний private include: чтение
Service Kit или приватного workspace, их credentials, код, workflows и образы
не являются обязательной зависимостью. Приватный профиль не копируется сюда.

- Перед изменениями прочитать существующие документы затронутого контракта.
  Зафиксировать revision и dirty state прочитанных источников; недоступность
  необходимого источника назвать явно.
- Выполнять только запрошенную работу в этом репозитории. Сохранять чужие локальные
  изменения; соседний checkout или упоминание consumer не разрешает его менять.
  Commit/push, PR, выпуск, deployment и реальные инфраструктурные операции требуют
  соответствующего запроса. Skills без явного разрешения не изменять.
- Spec Kit устанавливается и запускается только в workspace. В этом репозитории
  не создавать его конфигурацию, skills, команды или конституцию. При работе через
  workspace использовать его согласованные spec/plan/tasks; автономная работа
  опирается на этот AGENTS.md. Предложение и успешная сборка не означают принятие
  продукта или разрешение перейти к runtime.
- Схема, генерация и SDK принадлежат producer (STD-006/007). Consumer не создаёт
  ручную копию доменной модели. API/wire/runtime versions и migration status
  фиксировать отдельно; credentials не помещать в Git, логи или generated files.
- Для public CI использовать GitHub-hosted runners и публичные локальные configs
  (STD-015/029/033/035). Общие Bash/scripts/baseline checks выполняются в Ubuntu
  26.04; platform-specific проверки отдельно указывают OS и точный scope.
- При отказе необходимого инструмента или проверке с skip фиксировать not-verified,
  не pass. Для изменения кода выполнять применимые обязательные проверки всего
  затронутого модуля, а не только изменённых строк. Для документации проверить
  локальные ссылки и `git diff --check`.

## Сохранённые правила локального профиля

Неизвестные policy/timeout/limit values не выдумываются. Наблюдаемые результаты,
запрещённые исходы и применимые проверки связываются с постановкой задачи.

- STD-016/018/020/023/024: authority определяется настоящим caller/resource/action,
  dependency failure не расширяет доступ; bounded deadline/cancellation/retry,
  immutable replay inputs, expiry и current-state checks у соответствующего owner.
  Shape/serialization fixtures не доказывают runtime authentication или durable replay.
- STD-034: локальная работа ограничена запрошенным owner scope; consumer references
  не разрешают менять чужой checkout. Host lifecycle/provision/deployment/
  activation/rollback относятся к внешнему платформенному контуру вне активного
  состава. Эти функции не переходят сервисам или Service Kit; место и owner
  требуют отдельного решения COMPOSITION-Q-01. Изолированный test fixture
  не получает production authority.
- Public consumer может получать private producer API по закреплённой версии с
  read-only credentials при сборке. Это не разрешает Service Kit dependency;
  credentials не попадают в Git/logs/artifacts и недоверенные external PR, а
  отсутствие необходимого доступа завершает fetch ошибкой без fallback.
- Применимость и exact revision источников и результатов проверок фиксируются;
  proposed решения не становятся accepted из-за документа или успешной сборки.

Локальные особенности указывают причину, область и заменяемое положение.
Наличие документов не расширяет разрешённые записи или публикации.

## Проверки владельца

Собственные Go-модули — корневой `go.mod` и независимый `api/go.mod`; каждый
использует свою `.golangci-lint.yaml`, без загрузки private конфигурации.
Из корня каждого модуля в Ubuntu 26.04 выполнять базовый набор:

```bash
goimports -l .
go vet ./...
golangci-lint run --config .golangci-lint.yaml ./... --timeout 1m
go test -short ./...
govulncheck ./...
```

Непустой вывод `goimports -l` означает ошибку форматирования; исправления
просматриваются отдельно. Команды `cmd/relay` и `cmd/relay-coordinator` реализуют dataplane и registry;
готовность подтверждается проверками runtime009. Сборка Linux amd64 выполняется отдельно
командой `python3 -B scripts/ci.py build`.

Версии инструментов и producer-owned generation/fixtures описаны в
[контрактном README](api/README.md). При изменении схемы использовать закреплённые
Buf build/lint и воспроизводимую генерацию; generated outputs вручную не менять.
Buf breaking и проверки прежних producer/consumer пар не являются обязательным
v0 gate; исторические comparisons не требуют compatibility layer.

Внешний JSON dataplane отдельно проверяется командой из корня репозитория:

```bash
python3 api/checks/verify_public_wire.py
```

Зависимости JSON-проверки устанавливать вне репозитория в изолированную среду.
Контрактные shape/byte/semantic fixtures не подтверждают production signatures,
upstream authorization или TTL/revocation budgets. По карте владельцев Relay
отвечает за opaque payload и registry/leases/fencing; наличие SDK не подтверждает их исполнение.
Coordinator остаётся authority Network/Node pair,
Signing — purpose trust. Provision/host/deploy/activation находятся во внешнем
платформенном контуре вне активного состава; эти функции не переходят Relay или
Service Kit. Место и owner требуют отдельного решения COMPOSITION-Q-01.

## MVP v0 и цикл 002

Новая v0-линейка НЕ поддерживает backward compatibility до отдельного объявления
стабильного major новой линейки, включая patch. Изменения — hard cutover, без
shims, dual API и fallback ради прошлых версий. Сохраняемые данные обновляются
прямыми миграциями; скрытый reset запрещён. Runtime и api/v0.* версионируются
независимо. Consumer закрепляет точную версию producer API; `@latest`, first-party
local replace и обход pin через GOWORK запрещены.

Профиль .ci/profile.json явно перечисляет все собственные модули и outputs.
Базовый CI: format/lint, go vet, go test -short и govulncheck для Go, применимые
Dart/JSON unit/static checks. Integration/E2E и тесты прежних версий в обязательный
MVP gate не входят. Runtime009 реализует обе команды: relay и relay-coordinator;
результаты проверок не подменяют отдельную post-MVP сетевую приёмку.

Автономные команды: python3 -B scripts/ci.py check и build; unit tooling — scripts/test_ci.py.
Kit не загружается, собственные публичные lint configs остаются у владельца.
Release допускается только по push обычного v0-тега после exact commit checks и
сборки. API-тег выбирает только source/SDK, runtime-тег — runtime outputs.
Spec Kit и общая конституция находятся только в workspace.
